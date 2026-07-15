package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

// vagrant_net.go — cross-engine connectivity for hybrid (vagrant) stacks.
//
// In a hybrid stack, Docker nodes live on the stack's Docker bridge (a 172.x/16
// subnet, host gateway 172.x.0.1) while VM nodes live on a VirtualBox host-only /24
// (192.168.56.x, host gateway 192.168.56.x.1). The DBCanvas control-plane runs on
// the host, which sits on *both* networks, so it can route between them — but two
// things are missing out of the box:
//
//  1. Docker's FORWARD policy is DROP and it only ACCEPTs traffic to/from its own
//     bridges under narrow conditions, so a packet from the host-only subnet to the
//     Docker bridge (and back) is dropped. We add per-stack ACCEPT rules for the two
//     subnets in the DOCKER-USER chain, which Docker guarantees is consulted first in
//     FORWARD. Combined with net.ipv4.ip_forward=1 the host then routes both ways.
//  2. A VM's only route off its host-only adapter is its NAT default gateway, so it
//     has no path to the Docker 172.x subnet. We add a route inside each VM sending
//     the Docker subnet via the host-only gateway (the host). Docker containers need
//     no matching route: their default route already points at the bridge gateway
//     (the host), which then forwards to the host-only net.
//
// All of this is host-level and best-effort: failures are logged, never fatal, so a
// host without passwordless sudo / iptables still deploys (cross-engine traffic just
// won't flow). Rules are tagged with the stack id so teardown can remove exactly the
// ones it added, and DNS/resolv.conf is already handled by reconcileStackDNS /
// pointResolverAtIntranet — this file only opens the network path they rely on.

// hostCmd builds a host command, prefixing `sudo -n` when the process is not already
// root (iptables/sysctl/ip need CAP_NET_ADMIN). DBCANVAS_NO_SUDO=1 forces the direct
// form for hosts that grant the capability without sudo.
func hostCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if os.Geteuid() != 0 && os.Getenv("DBCANVAS_NO_SUDO") != "1" {
		return exec.CommandContext(ctx, "sudo", append([]string{"-n", name}, args...)...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// runHost runs a host command, returning combined output and error (with the output
// folded into the error so callers can log one line).
func runHost(ctx context.Context, name string, args ...string) (string, error) {
	out, err := hostCmd(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// iptRule is one iptables rule in a specific table+chain. args is the rule body
// starting with the chain name (e.g. "DOCKER-USER -s … -j ACCEPT"); the verb
// (-C/-I/-D) and "-t <table>" are supplied by the caller.
type iptRule struct {
	table string   // "raw" | "filter" | "nat"
	args  []string // chain name followed by match/target/comment
}

// stackRules is the full set of iptables rules that connect a hybrid stack's Docker
// bridge and host-only subnets, each tagged with the stack comment so teardown can
// find them. Pure and unit-tested.
func stackRules(dockerCIDR, vmCIDR string, stackID int64) []iptRule {
	tag := []string{"-m", "comment", "--comment", stackRuleComment(stackID)}
	rule := func(table, chain string, body ...string) iptRule {
		return iptRule{table, append(append([]string{chain}, body...), tag...)}
	}
	return []iptRule{
		// (1) raw/PREROUTING: bypass Docker 29+'s direct-routing hardening. Docker
		// installs `-d <containerIP>/32 ! -i <bridge> -j DROP` at raw priority — before
		// conntrack and the FORWARD chain — so a packet from any non-bridge interface
		// (our VM's host-only NIC) to a container IP is dropped and never even reaches
		// DOCKER-USER. An ACCEPT for our subnet pair, prepended (-I) ahead of that DROP,
		// short-circuits the raw table for cross-engine traffic so it proceeds normally.
		// Subnet-scoped (not per-container) so it covers every current/future node on
		// the bridge. Without this the FORWARD/nat rules below never see the packet.
		rule("raw", "PREROUTING", "-s", vmCIDR, "-d", dockerCIDR, "-j", "ACCEPT"),
		rule("raw", "PREROUTING", "-s", dockerCIDR, "-d", vmCIDR, "-j", "ACCEPT"),
		// (2) filter/DOCKER-USER: open the FORWARD path both ways — Docker's default
		// FORWARD policy is DROP, and DOCKER-USER is consulted first so an ACCEPT wins.
		rule("filter", "DOCKER-USER", "-s", vmCIDR, "-d", dockerCIDR, "-j", "ACCEPT"),
		rule("filter", "DOCKER-USER", "-s", dockerCIDR, "-d", vmCIDR, "-j", "ACCEPT"),
		// (3) nat/POSTROUTING: exempt cross-engine traffic from Docker's
		// `-s <dockerCIDR> ! -o br… MASQUERADE`. Without this a Docker node's reply to a
		// VM (and vice-versa) is SNAT'd to the host's host-only address, so the VM sees
		// the wrong source and drops it — and even when NAT "works", peers see the host
		// IP instead of each other's, breaking DNS ACLs, replication auth and PMM
		// scraping. Inserted (-I) ahead of the MASQUERADE rules so it takes effect first.
		rule("nat", "POSTROUTING", "-s", dockerCIDR, "-d", vmCIDR, "-j", "RETURN"),
		rule("nat", "POSTROUTING", "-s", vmCIDR, "-d", dockerCIDR, "-j", "RETURN"),
	}
}

// stackRuleChains lists the (table, chain) pairs stackRules touches, for teardown to
// sweep by comment.
var stackRuleChains = []struct{ table, chain string }{
	{"raw", "PREROUTING"},
	{"filter", "DOCKER-USER"},
	{"nat", "POSTROUTING"},
}

func stackRuleComment(stackID int64) string { return fmt.Sprintf("dbcanvas-stack-%d", stackID) }

// ensureIPForward turns on IPv4 forwarding if it is off. Docker normally sets this,
// but a host with no running containers yet may not have it enabled.
func (a *App) ensureIPForward(ctx context.Context) {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(b)) == "1" {
		return
	}
	if out, err := runHost(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		log.Printf("hybrid routing: enable ip_forward: %v (%s)", err, strings.TrimSpace(out))
	}
}

// linkStackNetworks installs the host iptables rules (raw ACCEPT, FORWARD ACCEPT, nat
// RETURN — see stackRules) that let the stack's Docker bridge and host-only subnet
// reach each other, and ensures ip_forward. Each rule is prepended (-I) so it takes
// effect ahead of Docker's own drop/masquerade rules. Idempotent (each rule checked
// with -C first) and a no-op for non-hybrid stacks or a host without a vagrant backend.
func (a *App) linkStackNetworks(ctx context.Context, st Stack) {
	if st.Backend != BackendVagrant || a.vagrant == nil {
		return
	}
	dockerCIDR, _ := a.docker.NetworkSubnet(ctx, networkName(st.ID))
	vmCIDR, _ := a.vagrant.NetworkSubnet(ctx, networkName(st.ID))
	if !validCIDR(dockerCIDR) || !validCIDR(vmCIDR) {
		return // one side not provisioned yet — nothing to link
	}
	a.ensureIPForward(ctx)
	for _, r := range stackRules(dockerCIDR, vmCIDR, st.ID) {
		if _, err := runHost(ctx, "iptables", append([]string{"-t", r.table, "-C"}, r.args...)...); err == nil {
			continue // already present
		}
		if out, err := runHost(ctx, "iptables", append([]string{"-t", r.table, "-I"}, r.args...)...); err != nil {
			log.Printf("stack %d hybrid routing: add %s rule: %v (%s)", st.ID, r.table, err, strings.TrimSpace(out))
		}
	}
}

// unlinkStackNetworks removes every rule tagged for the stack from each chain
// stackRules touches. It reads the live chains rather than re-deriving subnets, so it
// still works after the networks' subnet allocations have been dropped, and cleans up
// stale duplicates.
func (a *App) unlinkStackNetworks(ctx context.Context, stackID int64) {
	if a.vagrant == nil {
		return
	}
	comment := stackRuleComment(stackID)
	for _, c := range stackRuleChains {
		out, err := runHost(ctx, "iptables", "-t", c.table, "-S", c.chain)
		if err != nil {
			continue // chain absent (no Docker) or no privilege — nothing to undo
		}
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, comment) || !strings.HasPrefix(line, "-A ") {
				continue
			}
			// Turn the "-A <chain> …" append spec printed by -S into a "-D …" delete.
			spec := append([]string{"-t", c.table, "-D"}, strings.Fields(strings.TrimPrefix(line, "-A "))...)
			if o, err := runHost(ctx, "iptables", spec...); err != nil {
				log.Printf("stack %d hybrid routing: remove %s rule: %v (%s)", stackID, c.table, err, strings.TrimSpace(o))
			}
		}
	}
}

// reconcileStackRouting keeps the cross-engine path in sync with a hybrid stack's
// current nodes. Called from reconcileStackDNS (same triggers as DNS), it (re)opens
// the host FORWARD rules and, for every running VM node, ensures a route to the
// Docker subnet via the host-only gateway. Idempotent; a no-op for docker-only
// stacks. Best-effort — a VM not yet up is simply retried on the next reconcile.
func (a *App) reconcileStackRouting(ctx context.Context, st Stack, deps []Deployment) {
	if st.Backend != BackendVagrant || a.vagrant == nil {
		return
	}
	dockerCIDR, _ := a.docker.NetworkSubnet(ctx, networkName(st.ID))
	vmCIDR, _ := a.vagrant.NetworkSubnet(ctx, networkName(st.ID))
	if !validCIDR(dockerCIDR) || !validCIDR(vmCIDR) {
		return
	}
	a.linkStackNetworks(ctx, st)

	gw := hostOnlyGateway(vmCIDR)
	if gw == "" {
		return
	}
	var doc designDoc
	typeByID := map[string]string{}
	if json.Unmarshal(st.Design, &doc) == nil {
		for _, n := range doc.Nodes {
			typeByID[n.ID] = n.Type
		}
	}
	for _, d := range deps {
		if d.ContainerID == "" || a.nodeEngine(st, typeByID[d.NodeID]) != Engine(a.vagrant) {
			continue // Docker node — routes via the bridge gateway, no VM route needed
		}
		// `ip route replace` is idempotent and survives re-runs; a VM that is down or
		// mid-boot just errors here and is picked up on the next reconcile.
		if res, err := a.vagrant.ExecAs(ctx, d.ContainerID, "root",
			[]string{"ip", "route", "replace", dockerCIDR, "via", gw}, nil); err != nil || res.Code != 0 {
			log.Printf("stack %d hybrid routing: VM %s route to %s via %s not applied yet", st.ID, d.NodeID, dockerCIDR, gw)
		}
	}
}

// validCIDR reports whether s parses as a CIDR (e.g. "172.20.0.0/16").
func validCIDR(s string) bool {
	if s == "" {
		return false
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

// hostOnlyGateway is the host's address on a VirtualBox host-only /24 — the .1 of the
// subnet, which VirtualBox assigns to the host adapter and which VMs route through to
// reach the Docker bridge. Returns "" for an unparseable CIDR.
func hostOnlyGateway(vmCIDR string) string {
	ip, _, err := net.ParseCIDR(vmCIDR)
	if err != nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.1", ip4[0], ip4[1], ip4[2])
}
