package main

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// sshforward.go — SSH_FORWARDING_HOST: the `ssh -L` line that brings a deployed
// node's ports to the operator's own machine.
//
// The problem it solves. When DBCanvas runs on a shared server, CONTAINER_BIND_IP
// is normally 127.0.0.1 — deployed nodes publish their ports on the server's
// loopback and nothing else on the network can reach them. That is the right
// default (an unauthenticated PMM or a root-password MySQL should not be on the
// LAN), but it leaves the operator with a PMM console they cannot open and a
// MySQL port their local client cannot dial: the browser and the client run on
// their laptop, the ports exist on the server.
//
// An SSH tunnel is the answer, and it is the same answer every time — only the
// port numbers change. So when the administrator sets SSH_FORWARDING_HOST to the
// address the server is reachable at over SSH, right-clicking a running node
// offers the exact `ssh -L` command that forwards every port that node publishes:
//
//	ssh -L 8443:127.0.0.1:8443 -L 8080:127.0.0.1:8080 user@10.0.0.7 -p 22
//
// Unset (the default) the menu item is simply absent — on a laptop install the
// ports are already local and a tunnel would be noise.

// sshForwardingHostEnv is the variable that switches the feature on. Empty (the
// default) means off.
const sshForwardingHostEnv = "SSH_FORWARDING_HOST"

// sshForwardDefaultUser is the login the command carries when the configured
// value names none. It is a placeholder the operator edits — DBCanvas has no way
// to know their account on the host it happens to run on, and guessing (root,
// the container's user) would hand them a line that fails.
const sshForwardDefaultUser = "user"

// sshForwardTarget is a parsed SSH_FORWARDING_HOST: where to ssh to, and as whom.
type sshForwardTarget struct {
	User string
	Host string
	Port int
}

// sshForwardPort is one published port of a node — the port inside the container
// and the port it answers on from the server's own shell.
type sshForwardPort struct {
	Container int `json:"container"`
	Host      int `json:"host"`
}

// sshForwardInfo is what the designer gets back for a node. Enabled is false
// whenever SSH_FORWARDING_HOST is unset or unusable, and the UI then offers
// nothing — every other field is omitted in that case.
type sshForwardInfo struct {
	Enabled bool             `json:"enabled"`
	User    string           `json:"user,omitempty"`
	Host    string           `json:"host,omitempty"`
	Port    int              `json:"port,omitempty"`
	Bind    string           `json:"bind,omitempty"`
	Ports   []sshForwardPort `json:"ports,omitempty"`
	Command string           `json:"command,omitempty"`
}

// parseSSHForwardingHost reads the configured value. The documented form is an
// address with an optional port — "10.0.0.7", "10.0.0.7:2222" — and a "user@"
// prefix is accepted too, so an installation whose operators all log in under
// the same account can drop the placeholder. IPv6 is bracketed, as everywhere
// else a host:port is written.
//
// A value that is not a plausible host is rejected rather than patched up: this
// string is rendered into a command line the operator pastes into their shell,
// so anything carrying whitespace, quotes or shell metacharacters turns the
// feature off instead of producing a command that would run something else.
func parseSSHForwardingHost(v string) (sshForwardTarget, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return sshForwardTarget{}, false
	}
	t := sshForwardTarget{User: sshForwardDefaultUser, Port: 22}
	if at := strings.LastIndex(v, "@"); at >= 0 {
		if u := strings.TrimSpace(v[:at]); u != "" {
			t.User = u
		}
		v = strings.TrimSpace(v[at+1:])
	}
	// SplitHostPort fails on a bare host ("missing port") and on an unbracketed
	// IPv6 literal ("too many colons"); both are then taken whole, which is what
	// the caller means by writing them.
	if h, p, err := net.SplitHostPort(v); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return sshForwardTarget{}, false
		}
		v, t.Port = h, n
	}
	t.Host = strings.Trim(v, "[]")
	if !sshSafeToken(t.Host) || !sshSafeToken(t.User) {
		return sshForwardTarget{}, false
	}
	// A colon survives only in an IPv6 literal. Anything else carrying one is a
	// port that SplitHostPort could not make sense of ("10.0.0.7:22:22") — reject
	// it rather than ssh to a host by that name.
	if strings.Contains(t.Host, ":") && net.ParseIP(t.Host) == nil {
		return sshForwardTarget{}, false
	}
	return t, true
}

// sshSafeToken reports whether s can be dropped into a shell command unquoted.
// Deliberately narrower than what SSH accepts: hostnames, IP literals and Unix
// logins all live inside this set.
func sshSafeToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return false
		}
	}
	return true
}

// sshForwardingTarget is parseSSHForwardingHost over the environment.
func sshForwardingTarget() (sshForwardTarget, bool) {
	return parseSSHForwardingHost(envOr(sshForwardingHostEnv, ""))
}

// sshForwardBindHost is the address a published port answers on *from a shell on
// the server* — the far end of the tunnel. That is CONTAINER_BIND_IP, except
// that a wildcard bind is not an address you can connect to, so it becomes
// loopback.
func sshForwardBindHost() string {
	ip := strings.TrimSpace(envOr("CONTAINER_BIND_IP", "127.0.0.1"))
	switch ip {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	if !sshSafeToken(ip) {
		return "127.0.0.1"
	}
	if strings.Contains(ip, ":") { // IPv6 literal — ssh -L wants it bracketed
		if net.ParseIP(ip) == nil {
			return "127.0.0.1"
		}
		return "[" + ip + "]"
	}
	return ip
}

// sshForwardCommand renders the tunnel. The local port matches the server-side
// one so every address the UI shows keeps working verbatim through the tunnel —
// the PMM link, the connection strings on a node's panel, a copied client
// command. Ports are ordered so the same node always produces the same line.
func sshForwardCommand(t sshForwardTarget, bind string, ports []sshForwardPort) string {
	if len(ports) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("ssh")
	for _, p := range ports {
		b.WriteString(" -L ")
		b.WriteString(strconv.Itoa(p.Host))
		b.WriteString(":")
		b.WriteString(bind)
		b.WriteString(":")
		b.WriteString(strconv.Itoa(p.Host))
	}
	b.WriteString(" " + t.User + "@" + t.Host)
	b.WriteString(" -p " + strconv.Itoa(t.Port))
	return b.String()
}

// handleNodeSSHForward returns the tunnel command for one running node.
//
// The published ports are read from the engine rather than from the node's
// stored config: every node type publishes something different, host ports are
// re-assigned on restart, and the engine is the one place that always knows what
// is actually bound right now.
func (a *App) handleNodeSSHForward(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	t, on := sshForwardingTarget()
	if !on {
		writeJSON(w, http.StatusOK, sshForwardInfo{})
		return
	}
	ctx := r.Context()
	pm, err := a.engCtx(ctx).ContainerPorts(ctx, dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the node's published ports: "+err.Error())
		return
	}
	ports := make([]sshForwardPort, 0, len(pm))
	for cp, hp := range pm {
		ports = append(ports, sshForwardPort{Container: cp, Host: hp})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Host < ports[j].Host })

	bind := sshForwardBindHost()
	writeJSON(w, http.StatusOK, sshForwardInfo{
		Enabled: true,
		User:    t.User,
		Host:    t.Host,
		Port:    t.Port,
		Bind:    bind,
		Ports:   ports,
		Command: sshForwardCommand(t, bind, ports),
	})
}
