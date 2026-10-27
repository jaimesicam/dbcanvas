package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// dnsSerial returns a strictly increasing zone serial. Plain time.Now().Unix()
// collides when two reconciles land in the same second, which makes named skip
// the reload (it keeps the older, stale zone). Tracking the last value and
// bumping guarantees each rewrite advances the serial.
var (
	dnsSerialMu   sync.Mutex
	dnsLastSerial int64
)

func dnsSerial() int64 {
	dnsSerialMu.Lock()
	defer dnsSerialMu.Unlock()
	s := time.Now().Unix()
	if s <= dnsLastSerial {
		s = dnsLastSerial + 1
	}
	dnsLastSerial = s
	return s
}

// The Intranet node runs bind (named) as the authoritative DNS server for the
// stack's $DOMAIN. Every deployed node — including the Intranet itself — is
// published as an A record (and PTR in the reverse zone), and non-Intranet nodes
// use the Intranet as their resolver. The zone is rebuilt from scratch on every
// reconcile, so adds/removes/restarts (which can change container IPs) converge.
// Records added by hand on the Intranet's DNS tab are kept by DBCanvas, not in the
// zone file, and merged in on every rebuild (dnscustom.go).

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

// revZone is one in-addr.arpa zone: the first `octets` bytes of `base` fixed, the rest the owner
// name of each PTR in it.
type revZone struct {
	name   string
	base   net.IP // 4-byte form
	octets int
}

// reverseZoneFor derives the stack's reverse zone from a network CIDR, rounding the prefix down to
// an octet boundary (/8, /16, /24).
func reverseZoneFor(cidr string) (revZone, bool) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return revZone{}, false
	}
	base := ipnet.IP.To4()
	if base == nil {
		return revZone{}, false
	}
	ones, _ := ipnet.Mask.Size()
	switch {
	case ones >= 24:
		return newRevZone(base, 3), true
	case ones >= 16:
		return newRevZone(base, 2), true
	case ones >= 8:
		return newRevZone(base, 1), true
	}
	return revZone{}, false
}

func newRevZone(base net.IP, octets int) revZone {
	var parts []string
	for i := octets - 1; i >= 0; i-- {
		parts = append(parts, fmt.Sprintf("%d", base[i]))
	}
	return revZone{name: strings.Join(parts, ".") + ".in-addr.arpa", base: base, octets: octets}
}

func (z revZone) contains(ip net.IP) bool {
	for i := 0; i < z.octets; i++ {
		if ip[i] != z.base[i] {
			return false
		}
	}
	return true
}

func (z revZone) owner(ip net.IP) string {
	var rev []string
	for i := 3; i >= z.octets; i-- {
		rev = append(rev, fmt.Sprintf("%d", ip[i]))
	}
	return strings.Join(rev, ".")
}

// --- zone + config rendering ---

type dnsRecord struct{ host, ip string }

const zoneTTL = 60

// dnsReverseFile is where the stack network's reverse zone lives; any other reverse zone — one a
// custom PTR outside the stack network called for (dnscustom.go) — is dnsExtraRevPrefix+<zone>.
const (
	dnsForwardFile    = "dbcanvas.forward"
	dnsReverseFile    = "dbcanvas.reverse"
	dnsExtraRevPrefix = "dbcanvas.rev."
)

func zoneHeader(b *strings.Builder, domain string, serial int64) {
	fmt.Fprintf(b, "$TTL %d\n", zoneTTL)
	fmt.Fprintf(b, "@ IN SOA intranet.%s. admin.%s. ( %d 3600 600 86400 60 )\n", domain, domain, serial)
	fmt.Fprintf(b, "@ IN NS intranet.%s.\n", domain)
}

// dnsZoneFile is one zone named serves: its name, its file under /var/named, and the text.
type dnsZoneFile struct{ Zone, File, Text string }

// dnsZones is everything the Intranet's bind is given: the forward zone and the reverse zones.
type dnsZones struct {
	Forward dnsZoneFile
	Reverse []dnsZoneFile
}

// buildDNSZones renders the zones from the automatic records (one per deployed node) and the
// stack's custom records. A custom A or CNAME replaces the automatic records of the same name, and
// a custom PTR (or an A published with its PTR) replaces the automatic PTRs of the same address —
// so a custom record can point a node's name somewhere else without fighting the reconcile that
// rewrites the zone after every deploy. Every PTR lands in the reverse zone that
// contains its address: the stack network's, or a /24 of its own for an address outside it.
func buildDNSZones(domain string, serial int64, subnet string, auto []dnsRecord, custom []dnsCustomRecord) dnsZones {
	named := map[string]bool{}
	customPTR := map[string]bool{}
	for _, c := range custom {
		switch c.Type {
		case "A", "CNAME":
			named[c.Name] = true
			if c.Type == "A" && c.PTR {
				customPTR[c.Value] = true
			}
		case "PTR":
			customPTR[c.Name] = true
		}
	}

	var fwd strings.Builder
	zoneHeader(&fwd, domain, serial)
	type ptr struct{ ip, target string }
	var ptrs []ptr
	for _, r := range auto {
		if !named[r.host] {
			fmt.Fprintf(&fwd, "%s IN A %s\n", r.host, r.ip)
		}
		if !customPTR[r.ip] {
			ptrs = append(ptrs, ptr{r.ip, fqdnOf(r.host, domain)})
		}
	}
	for _, c := range custom {
		switch c.Type {
		case "A":
			fmt.Fprintf(&fwd, "%s IN A %s\n", c.Name, c.Value)
			if c.PTR {
				ptrs = append(ptrs, ptr{c.Value, fqdnOf(c.Name, domain)})
			}
		case "CNAME":
			fmt.Fprintf(&fwd, "%s IN CNAME %s.\n", c.Name, c.Value)
		case "PTR":
			ptrs = append(ptrs, ptr{c.Name, c.Value})
		}
	}
	out := dnsZones{Forward: dnsZoneFile{Zone: domain, File: dnsForwardFile, Text: fwd.String()}}

	// The stack network's reverse zone is always served (empty or not), as it always has been;
	// any other is made only for the addresses that need it.
	type zoneOut struct {
		z    revZone
		file string
		b    *strings.Builder
		seen map[string]bool
	}
	var zones []*zoneOut
	newZone := func(z revZone, file string) *zoneOut {
		zo := &zoneOut{z: z, file: file, b: &strings.Builder{}, seen: map[string]bool{}}
		zoneHeader(zo.b, domain, serial)
		zones = append(zones, zo)
		return zo
	}
	if z, ok := reverseZoneFor(subnet); ok {
		newZone(z, dnsReverseFile)
	}
	for _, p := range ptrs {
		ip := net.ParseIP(p.ip).To4()
		if ip == nil {
			continue
		}
		var zo *zoneOut
		for _, cand := range zones {
			if cand.z.contains(ip) {
				zo = cand
				break
			}
		}
		if zo == nil {
			z := newRevZone(ip, 3)
			zo = newZone(z, dnsExtraRevPrefix+z.name)
		}
		line := fmt.Sprintf("%s IN PTR %s.\n", zo.z.owner(ip), p.target)
		if !zo.seen[line] {
			zo.seen[line] = true
			zo.b.WriteString(line)
		}
	}
	for _, zo := range zones {
		out.Reverse = append(out.Reverse, dnsZoneFile{Zone: zo.z.name, File: zo.file, Text: zo.b.String()})
	}
	return out
}

// namedConf renders a minimal authoritative named.conf. It listens only on
// localhost + the Intranet's own address (never 127.0.0.11, which is Docker's
// embedded resolver it forwards external queries to), and serves the forward zone
// and every reverse zone.
func namedConf(z dnsZones, listenIP string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `plugin query "/usr/lib64/named/filter-aaaa.so" {
    filter-aaaa-on-v4 yes;
};
options {
    listen-on port 53 { 127.0.0.1; %s; };
    listen-on-v6 { none; };
    directory "/var/named";
    allow-query { any; };
    recursion yes;
    forwarders { 127.0.0.11; };
    forward first;
    dnssec-validation no;
};
`, listenIP)
	for _, f := range append([]dnsZoneFile{z.Forward}, z.Reverse...) {
		fmt.Fprintf(&b, "zone \"%s\" IN { type master; file \"/var/named/%s\"; };\n", f.Zone, f.File)
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
			// The Intranet runs on Docker; read its IP on the Intranet's engine so a
			// VM node's restart path resolves it correctly too.
			if cip, _ := a.intranetEngine().ContainerIP(ctx, dep.ContainerID, networkName(stackID)); cip != "" {
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
	a.engCtx(ctx).ExecAs(ctx, containerID, "0",
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

// dnsPlan is what the Intranet's zones are built from: where its bind runs, the automatic records
// (one per deployed node) and the stack network they are on.
type dnsPlan struct {
	intranetID, intranetIP string
	domain, subnet         string
	auto                   []dnsRecord
}

// stackDNSPlan reads the stack's current deployments into a dnsPlan: an A record for every node
// that has a container IP, including the Intranet itself. ok=false until the Intranet has a
// container IP — there is no DNS authority to write to before then.
func (a *App) stackDNSPlan(ctx context.Context, st Stack) (dnsPlan, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return dnsPlan{}, false
	}
	hosts := stackHostnames(doc)
	typeByID := map[string]string{}
	for _, n := range doc.Nodes {
		typeByID[n.ID] = n.Type
	}

	deps, _ := a.store.ListDeployments(st.ID)
	p := dnsPlan{domain: envOr("DOMAIN", "example.net")}
	for _, d := range deps {
		if typeByID[d.NodeID] == "intranet" && d.ContainerID != "" {
			p.intranetID = d.ContainerID
			break
		}
	}
	if p.intranetID == "" {
		return dnsPlan{}, false // no DNS authority yet
	}
	netName := networkName(st.ID)
	if p.intranetIP, _ = a.intranetEngine().ContainerIP(ctx, p.intranetID, netName); p.intranetIP == "" {
		return dnsPlan{}, false
	}

	for _, d := range deps {
		h := hosts[d.NodeID]
		if h == "" || d.ContainerID == "" {
			continue
		}
		// Each node's IP must be read on that node's engine — a VM node's address is
		// its host-only IP (from Vagrant), not something the Docker Intranet knows.
		ip, _ := a.nodeEngine(st, typeByID[d.NodeID]).ContainerIP(ctx, d.ContainerID, netName)
		if ip == "" {
			continue
		}
		p.auto = append(p.auto, dnsRecord{host: h, ip: ip})
		// An All-in-One node runs many instances behind one container IP. Publish
		// each instance's name as its own A record so a replication config, a PMM
		// registration or a psql -h reads as a hostname rather than "the AiO box,
		// port 15020". Same IP, different names — the port disambiguates.
		if typeByID[d.NodeID] == "aio" {
			for _, inst := range aioRuntimeInstances(d) {
				p.auto = append(p.auto, dnsRecord{host: inst.Inst, ip: ip})
			}
		}
	}
	// A replication source's per-pod LoadBalancer addresses, which a replica verifying its
	// certificate dials by name (k3dreplssl.go).
	p.auto = append(p.auto, k3dReplDNSRecords(deps, typeByID)...)
	sort.Slice(p.auto, func(i, j int) bool { return p.auto[i].host < p.auto[j].host })
	p.subnet, _ = a.intranetEngine().NetworkSubnet(ctx, netName)
	return p, true
}

// reconcileStackDNS rebuilds the Intranet's authoritative zones from the stack's
// current deployments and its custom records. Best-effort and idempotent — safe to
// call after any deploy/lifecycle change. No-op until the Intranet has a container IP.
func (a *App) reconcileStackDNS(ctx context.Context, stackID int64) {
	a.applyStackDNS(ctx, stackID)
}

// applyStackDNS is reconcileStackDNS reporting what went wrong, for the DNS tab: applied=false
// (with no error) means there is no running Intranet yet, so the records wait for its deploy.
func (a *App) applyStackDNS(ctx context.Context, stackID int64) (applied bool, err error) {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return false, err
	}
	// Cross-engine routing reconciles on the same triggers as DNS (a hybrid stack
	// needs the host FORWARD path + VM routes open before names resolved here are
	// reachable). No-op for docker-only stacks; independent of the Intranet below.
	deps, _ := a.store.ListDeployments(st.ID)
	a.reconcileStackRouting(ctx, st, deps)
	p, ok := a.stackDNSPlan(ctx, st)
	if !ok {
		return false, nil
	}
	zones := buildDNSZones(p.domain, dnsSerial(), p.subnet, p.auto, a.dnsCustomRecords(stackID))

	eng := a.intranetEngine()
	var revNames, files []string
	for _, f := range append([]dnsZoneFile{zones.Forward}, zones.Reverse...) {
		if err := eng.CopyFile(ctx, p.intranetID, "/var/named", f.File, 0o644, []byte(f.Text)); err != nil {
			return false, fmt.Errorf("write zone %s: %w", f.Zone, err)
		}
		files = append(files, f.File)
	}
	for _, f := range zones.Reverse {
		revNames = append(revNames, f.Zone)
	}
	if err := eng.CopyFile(ctx, p.intranetID, "/etc", "named.conf", 0o644, []byte(namedConf(zones, p.intranetIP))); err != nil {
		return false, fmt.Errorf("write named.conf: %w", err)
	}

	// Fix ownership and (re)load named. `rndc reconfig` loads any newly-declared
	// zones from the rewritten named.conf (and drops any no longer declared); then each
	// zone is reloaded by name (the bare `rndc reload` does not reliably re-read a
	// changed master zone file). The serial is guaranteed to advance (see dnsSerial),
	// so named honors the reload. A full restart is the fallback. Extra reverse zones
	// that are no longer needed leave their files behind, so those are removed first.
	env := []string{"FWD=" + p.domain, "REVS=" + strings.Join(revNames, " "), "FILES=" + strings.Join(files, " ")}
	reload := `set -e
for f in /var/named/` + dnsExtraRevPrefix + `*; do
  [ -e "$f" ] || continue
  case " $FILES " in *" $(basename "$f") "*) ;; *) rm -f "$f" ;; esac
done
chown root:named /etc/named.conf /var/named/dbcanvas.* 2>/dev/null || true
chmod 640 /etc/named.conf 2>/dev/null || true
chmod 644 /var/named/dbcanvas.* 2>/dev/null || true
if command -v rndc >/dev/null 2>&1; then
  rndc reconfig >/dev/null 2>&1 || true
  for z in "$FWD" $REVS; do rndc reload "$z" >/dev/null 2>&1 || true; done
else
  systemctl reload named >/dev/null 2>&1 || systemctl restart named >/dev/null 2>&1 || true
fi`
	if _, err := eng.Exec(ctx, p.intranetID, []string{"bash", "-c", reload}, env); err != nil {
		return false, fmt.Errorf("reload named: %w", err)
	}
	return true, nil
}
