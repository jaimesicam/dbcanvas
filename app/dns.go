package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strings"
	"time"
)

// The Intranet node runs bind (named) as the authoritative DNS server for the
// stack's $DOMAIN. Every deployed node — including the Intranet itself — is
// published as an A record (and PTR in the reverse zone), and non-Intranet nodes
// use the Intranet as their resolver. The zone is rebuilt from scratch on every
// reconcile, so adds/removes/restarts (which can change container IPs) converge.

// --- hostnames ---

// hostLabel converts a node label into a DNS-safe label: lowercase, [a-z0-9-]
// only, other characters collapsed to single '-', trimmed of leading/trailing '-'.
func hostLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// shortID returns a short, stable token derived from a node id (4 hex of FNV-32a),
// used to disambiguate same-named nodes.
func shortID(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%04x", h.Sum32()&0xffff)
}

// stackHostnames computes a stable, unique, DNS-safe hostname for every node in
// the design. The Intranet (a per-stack singleton) is always "intranet". Other
// nodes use their sanitized label; when two share a label, each gets a stable
// suffix derived from its node id — so duplicate instances (e.g. two "pmm") stay
// distinct and keep the same name across redeploys regardless of canvas order.
func stackHostnames(doc designDoc) map[string]string {
	base := map[string]string{}
	count := map[string]int{}
	for _, n := range doc.Nodes {
		b := hostLabel(n.Label)
		if b == "" {
			b = hostLabel(n.Type)
		}
		if b == "" {
			b = "node"
		}
		if n.Type == "intranet" {
			b = "intranet"
		}
		base[n.ID] = b
		count[b]++
	}
	used := map[string]bool{}
	out := map[string]string{}
	for _, n := range doc.Nodes {
		h := base[n.ID]
		if n.Type != "intranet" && count[h] > 1 {
			h = h + "-" + shortID(n.ID)
		}
		orig := h
		for i := 2; used[h]; i++ {
			h = fmt.Sprintf("%s-%d", orig, i)
		}
		used[h] = true
		out[n.ID] = h
	}
	return out
}

func fqdnOf(host, domain string) string { return host + "." + domain }

// nodeTypeOf returns the design type of a node id within a stack.
func nodeTypeOf(st Stack, nid string) string {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return ""
	}
	for _, n := range doc.Nodes {
		if n.ID == nid {
			return n.Type
		}
	}
	return ""
}

// --- reverse zone math ---

// reverseZoneInfo derives the in-addr.arpa zone name and a PTR-owner function
// from a network CIDR, rounding the prefix down to an octet boundary (/8,/16,/24).
func reverseZoneInfo(cidr string) (zone string, owner func(ip string) string, ok bool) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", nil, false
	}
	base := ipnet.IP.To4()
	if base == nil {
		return "", nil, false
	}
	ones, _ := ipnet.Mask.Size()
	var octets int
	switch {
	case ones >= 24:
		octets = 3
	case ones >= 16:
		octets = 2
	case ones >= 8:
		octets = 1
	default:
		return "", nil, false
	}
	var parts []string
	for i := octets - 1; i >= 0; i-- {
		parts = append(parts, fmt.Sprintf("%d", base[i]))
	}
	zone = strings.Join(parts, ".") + ".in-addr.arpa"
	owner = func(ip string) string {
		p := net.ParseIP(ip).To4()
		if p == nil {
			return ""
		}
		var rev []string
		for i := 3; i >= octets; i-- {
			rev = append(rev, fmt.Sprintf("%d", p[i]))
		}
		return strings.Join(rev, ".")
	}
	return zone, owner, true
}

// staticIntranetIP picks a stable address for the Intranet within the network
// subnet (host .2, just past the .1 gateway) so its resolver IP survives
// restarts. Returns "" if the subnet can't be parsed.
func staticIntranetIP(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return ""
	}
	out := make(net.IP, 4)
	copy(out, ip)
	out[3] += 2 // network base is x.x.x.0 → x.x.x.2
	return out.String()
}

// --- zone + config rendering ---

type dnsRecord struct{ host, ip string }

const zoneTTL = 60

func zoneHeader(b *strings.Builder, domain string, serial int64) {
	fmt.Fprintf(b, "$TTL %d\n", zoneTTL)
	fmt.Fprintf(b, "@ IN SOA intranet.%s. admin.%s. ( %d 3600 600 86400 60 )\n", domain, domain, serial)
	fmt.Fprintf(b, "@ IN NS intranet.%s.\n", domain)
}

func forwardZone(domain string, serial int64, recs []dnsRecord) string {
	var b strings.Builder
	zoneHeader(&b, domain, serial)
	for _, r := range recs {
		fmt.Fprintf(&b, "%s IN A %s\n", r.host, r.ip)
	}
	return b.String()
}

func reverseZone(domain string, serial int64, recs []dnsRecord, owner func(string) string) string {
	var b strings.Builder
	zoneHeader(&b, domain, serial)
	for _, r := range recs {
		if o := owner(r.ip); o != "" {
			fmt.Fprintf(&b, "%s IN PTR %s.%s.\n", o, r.host, domain)
		}
	}
	return b.String()
}

// namedConf renders a minimal authoritative named.conf. It listens only on
// localhost + the Intranet's own address (never 127.0.0.11, which is Docker's
// embedded resolver it forwards external queries to), serves the forward zone,
// and serves the reverse zone when one could be derived.
func namedConf(domain, reverseZoneName, listenIP string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `options {
    listen-on port 53 { 127.0.0.1; %s; };
    listen-on-v6 { none; };
    directory "/var/named";
    allow-query { any; };
    recursion yes;
    forwarders { 127.0.0.11; };
    forward first;
    dnssec-validation no;
};
zone "%s" IN { type master; file "/var/named/dbcanvas.forward"; };
`, listenIP, domain)
	if reverseZoneName != "" {
		fmt.Fprintf(&b, "zone \"%s\" IN { type master; file \"/var/named/dbcanvas.reverse\"; };\n", reverseZoneName)
	}
	return b.String()
}

// intranetEndpoint returns the stack's Intranet container id and its IP on the
// stack network (ok=false until one exists).
func (a *App) intranetEndpoint(ctx context.Context, stackID int64) (id, ip string, ok bool) {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return "", "", false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return "", "", false
	}
	for _, n := range doc.Nodes {
		if n.Type != "intranet" {
			continue
		}
		dep, e := a.store.GetDeployment(stackID, n.ID)
		if e == nil && dep.ContainerID != "" {
			if cip, _ := a.docker.ContainerIP(ctx, dep.ContainerID, networkName(stackID)); cip != "" {
				return dep.ContainerID, cip, true
			}
		}
	}
	return "", "", false
}

// pointResolverAtIntranet rewrites a container's /etc/resolv.conf (as root) to use
// the Intranet as its sole nameserver, so forward *and* reverse lookups resolve
// through the stack's authoritative DNS (Docker's embedded resolver answers
// reverse PTR for in-network IPs itself, bypassing the Intranet, unless we do
// this). Docker regenerates resolv.conf on each start, so it is re-applied after
// start/restart.
func (a *App) pointResolverAtIntranet(ctx context.Context, containerID, nsIP, domain string) {
	conf := fmt.Sprintf("search %s\nnameserver %s\noptions ndots:1\n", domain, nsIP)
	a.docker.ExecAs(ctx, containerID, "0",
		[]string{"bash", "-c", `printf '%s' "$CONF" > /etc/resolv.conf`}, []string{"CONF=" + conf})
}

// restoreNodeResolver re-points a non-Intranet node at the Intranet resolver
// after a (re)start, since Docker regenerates /etc/resolv.conf on each start.
func (a *App) restoreNodeResolver(ctx context.Context, st Stack, nid string, dep Deployment) {
	if dep.ContainerID == "" || nodeTypeOf(st, nid) == "intranet" {
		return
	}
	if _, ip, ok := a.intranetEndpoint(ctx, st.ID); ok {
		a.pointResolverAtIntranet(ctx, dep.ContainerID, ip, envOr("DOMAIN", "example.net"))
	}
}

// --- reconcile ---

// reconcileStackDNS rebuilds the Intranet's authoritative zones from the stack's
// current deployments: an A record (and PTR) for every node that has a container
// IP, including the Intranet itself. Best-effort and idempotent — safe to call
// after any deploy/lifecycle change. No-op until the Intranet has a container IP.
func (a *App) reconcileStackDNS(ctx context.Context, stackID int64) {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return
	}
	hosts := stackHostnames(doc)
	typeByID := map[string]string{}
	for _, n := range doc.Nodes {
		typeByID[n.ID] = n.Type
	}

	deps, _ := a.store.ListDeployments(stackID)
	intranetID := ""
	for _, d := range deps {
		if typeByID[d.NodeID] == "intranet" && d.ContainerID != "" {
			intranetID = d.ContainerID
			break
		}
	}
	if intranetID == "" {
		return // no DNS authority yet
	}
	netName := networkName(stackID)
	intranetIP, _ := a.docker.ContainerIP(ctx, intranetID, netName)
	if intranetIP == "" {
		return
	}

	var recs []dnsRecord
	for _, d := range deps {
		h := hosts[d.NodeID]
		if h == "" || d.ContainerID == "" {
			continue
		}
		ip, _ := a.docker.ContainerIP(ctx, d.ContainerID, netName)
		if ip == "" {
			continue
		}
		recs = append(recs, dnsRecord{host: h, ip: ip})
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].host < recs[j].host })

	domain := envOr("DOMAIN", "example.net")
	serial := time.Now().Unix()
	subnet, _ := a.docker.NetworkSubnet(ctx, netName)
	revZone, owner, hasRev := reverseZoneInfo(subnet)

	a.docker.CopyFile(ctx, intranetID, "/var/named", "dbcanvas.forward", 0o644, []byte(forwardZone(domain, serial, recs)))
	revName := ""
	if hasRev {
		revName = revZone
		a.docker.CopyFile(ctx, intranetID, "/var/named", "dbcanvas.reverse", 0o644, []byte(reverseZone(domain, serial, recs, owner)))
	}
	a.docker.CopyFile(ctx, intranetID, "/etc", "named.conf", 0o644, []byte(namedConf(domain, revName, intranetIP)))

	// Fix ownership and (re)load named. rndc reconfig picks up new zones from the
	// rewritten named.conf; a restart is the fallback if rndc isn't available.
	reload := `set -e
chown root:named /etc/named.conf /var/named/dbcanvas.forward 2>/dev/null || true
[ -f /var/named/dbcanvas.reverse ] && chown root:named /var/named/dbcanvas.reverse 2>/dev/null || true
chmod 640 /etc/named.conf 2>/dev/null || true
chmod 644 /var/named/dbcanvas.* 2>/dev/null || true
rndc reconfig >/dev/null 2>&1 && rndc reload >/dev/null 2>&1 || systemctl restart named >/dev/null 2>&1 || true`
	a.docker.Exec(ctx, intranetID, []string{"bash", "-c", reload}, nil)
}
