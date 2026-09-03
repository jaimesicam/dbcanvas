package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// composebuild.go — turning a resolved spec into a design, and serving it.
//
// Three jobs, in order, and the second is the one that makes a spec worth writing:
//
//  1. RESOLVE. Each entry's OS alias and version string become concrete (compose.go).
//  2. WIRE. `monitor: true` becomes `pmmNodeId: "<the PMM node's id>"`, `ldap: true`
//     becomes `ldapAuth` plus `ldapDirNodeId`, a cluster becomes a frame plus members
//     carrying its `frameId`. These are cross-references by generated id, which is
//     exactly what a person cannot write by hand without first inventing the ids.
//  3. LAY OUT. Coordinates, because a design without them opens as a pile in the
//     top-left corner and the canvas is where anybody will look at this next.
//
// Then the existing validator runs over the result, so a composed stack is held to
// the same standard as one drawn by hand and the caller sees the same problems.

// Layout constants, taken from the built-in templates so a composed stack opens
// looking like a designed one (see templates_builtin.go).
const (
	composeColX     = 40  // first standalone column
	composeColStep  = 260 // between standalone columns
	composeRowStep  = 180 // between standalone rows
	composeCols     = 2   // standalone columns before wrapping
	composeFrameX   = 560 // cluster frames sit to the right of the standalone column
	composeFrameY   = 20
	composeFrameH   = 138
	composeFrameGap = 40
	composeMemberDX = 128 // between members inside a frame
	composeMemberIX = 14  // first member's inset from the frame's left edge
	composeMemberIY = 46  // members' inset from the frame's top edge
)

// composeBuilder accumulates a design while resolving a spec.
type composeBuilder struct {
	doc      designDoc
	resolved []composeResolved
	added    []string
	// pos holds each node's canvas coordinates by id.
	//
	// They are not on designNode: the Go side never reads a node's x/y — only the
	// browser does — so the struct does not declare them, and they survive a
	// round trip because the store keeps the design as opaque JSON. Rather than add
	// two fields to a struct 120 fields wide that nothing here needs, compose keeps
	// them beside it and injects them when the document is marshalled. See designJSON.
	pos map[string][2]float64
	// Generated ids by role, for the wiring pass.
	intranetID string
	sambaID    string
	pmmIDs     []string
	pmmByName  map[string]string
	usedLabels map[string]bool
	nextCol    int
	nextRow    int
	nextFrame  int
}

// buildCompose is the whole of steps 1–3. It returns the design, what each entry
// resolved to, and what compose added on the caller's behalf.
func buildCompose(spec composeSpec) (designDoc, map[string][2]float64, []composeResolved, []string, error) {
	b := &composeBuilder{
		pmmByName:  map[string]string{},
		usedLabels: map[string]bool{},
		pos:        map[string][2]float64{},
	}
	if len(spec.Nodes) == 0 {
		return designDoc{}, nil, nil, nil, fmt.Errorf("a spec needs at least one node")
	}

	// Reject unknown kinds before doing anything, so the error names all of them at
	// once rather than one per retry.
	var unknown []string
	for _, s := range spec.Nodes {
		kind := strings.ToLower(strings.TrimSpace(s.Kind))
		if kind == "" {
			return designDoc{}, nil, nil, nil, fmt.Errorf("every node needs a \"kind\"")
		}
		if why, no := unsupportedKinds[kind]; no {
			return designDoc{}, nil, nil, nil, fmt.Errorf(
				"compose does not build %q: %s. Start from a template "+
					"(`dbcanvas template list`) and edit the design instead", kind, why)
		}
		if _, ok := composeKindByName(kind); !ok {
			unknown = append(unknown, kind)
		}
	}
	if len(unknown) > 0 {
		names := make([]string, 0, len(composeKinds))
		for _, k := range composeKinds {
			names = append(names, k.Kind)
		}
		return designDoc{}, nil, nil, nil, fmt.Errorf("unknown kind(s) %s. Known: %s",
			strings.Join(unknown, ", "), strings.Join(names, ", "))
	}

	// Every stack needs an Intranet: it provides the DNS, CA, LDAP and package proxy
	// the other node types assume exists, which is why the designer greys the library
	// out until one is on the canvas. Adding it silently is the single biggest
	// ergonomic win here, and it is reported in `added` rather than hidden.
	hasIntranet := false
	for _, s := range spec.Nodes {
		if strings.EqualFold(s.Kind, "intranet") {
			hasIntranet = true
		}
	}
	ordered := spec.Nodes
	if !hasIntranet {
		ordered = append([]composeNodeSpec{{Kind: "intranet"}}, ordered...)
		b.added = append(b.added, "an Intranet node (DNS, CA, LDAP, package proxy) — every stack needs one")
	}

	// Pass one: create everything, so the wiring pass has ids to point at.
	for _, s := range ordered {
		if err := b.add(s); err != nil {
			return designDoc{}, nil, nil, nil, err
		}
	}
	// Pass two: the cross-references.
	if err := b.wire(ordered); err != nil {
		return designDoc{}, nil, nil, nil, err
	}
	// Pass three: prerequisites that only become known once the wiring is done.
	b.fixPrerequisites()
	return b.doc, b.pos, b.resolved, b.added, nil
}

// fixPrerequisites turns on the settings that a relationship makes mandatory.
//
// Right now that is one thing: an OIDC issuer has to be reachable over HTTPS, so a
// Keycloak node that something signs in through needs a certificate from the Intranet
// CA. The validator already refuses the alternative — composing a design that cannot
// pass its own validation, when the fix is mechanical and implied by what was asked
// for, would just be handing the caller an error to go and fix by hand.
//
// The second is the Ubuntu desktop a Keycloak node requires. An earlier version left
// that to the caller, on the grounds that a whole desktop container is a big thing to
// add unasked — the same reason PMM is not added for "monitor". That was wrong, and
// the distinguishing fact is that this requirement is MANDATORY, not advisory:
// intranet.go refuses a Keycloak node with no VNC node outright, because its admin
// console publishes no host ports and is only reachable from a browser inside the
// stack network. A stack without monitoring is perfectly valid; a Keycloak without a
// desktop can never deploy, so composing one would only hand the caller an error to
// go and fix by hand.
func (b *composeBuilder) fixPrerequisites() {
	has := func(t string) bool {
		return slices.ContainsFunc(b.doc.Nodes, func(n designNode) bool { return n.Type == t })
	}
	if has("keycloak") && !has("vnc") {
		kind, _ := composeKindByName("vnc")
		lbl := b.label(kind, "")
		x, y := b.placeNode()
		n := designNode{
			ID: "n-" + lbl, Type: "vnc", Label: lbl,
			OS: kind.PinOS[0], OSVersion: kind.PinOS[1], Arch: platformArch(),
			CertTTLValue: 365, CertTTLUnit: "days",
		}
		b.pos[n.ID] = [2]float64{x, y}
		b.doc.Nodes = append(b.doc.Nodes, n)
		b.added = append(b.added, lbl+
			" — Keycloak's admin console publishes no host ports, so it is only "+
			"reachable from a browser inside the stack")
	}

	usesOIDC := slices.ContainsFunc(b.doc.Nodes, func(n designNode) bool { return n.EnableOIDC })
	if !usesOIDC {
		return
	}
	for i := range b.doc.Nodes {
		if b.doc.Nodes[i].Type == "keycloak" && !b.doc.Nodes[i].GenerateCert {
			b.doc.Nodes[i].GenerateCert = true
			b.added = append(b.added,
				"a certificate on "+b.doc.Nodes[i].Label+
					" — an OIDC issuer has to be reachable over HTTPS")
		}
	}
}

// label picks a unique, readable label for an entry, which is also its hostname on
// the stack network.
func (b *composeBuilder) label(kind composeKind, want string) string {
	if want != "" {
		if !b.usedLabels[want] {
			b.usedLabels[want] = true
			return want
		}
		for i := 2; ; i++ {
			try := fmt.Sprintf("%s-%d", want, i)
			if !b.usedLabels[try] {
				b.usedLabels[try] = true
				return try
			}
		}
	}
	base := kind.Kind
	if kind.Frame {
		base = kind.Kind + "-cluster"
	}
	for i := 1; ; i++ {
		try := fmt.Sprintf("%s-%02d", base, i)
		if !b.usedLabels[try] {
			b.usedLabels[try] = true
			return try
		}
	}
}

// placeNode returns the next standalone node position.
func (b *composeBuilder) placeNode() (float64, float64) {
	x := float64(composeColX + b.nextCol*composeColStep)
	y := float64(composeColX + b.nextRow*composeRowStep)
	b.nextCol++
	if b.nextCol >= composeCols {
		b.nextCol = 0
		b.nextRow++
	}
	return x, y
}

// add creates the node(s) or frame for one entry.
func (b *composeBuilder) add(s composeNodeSpec) error {
	kind, _ := composeKindByName(s.Kind)

	os_, osVer, err := resolveOS(s.OS)
	if err != nil {
		return fmt.Errorf("%s: %w", kind.Kind, err)
	}
	// A kind that only works on one OS takes it, and refuses a different one rather
	// than composing a design the validator will immediately reject.
	if kind.PinOS[0] != "" {
		if s.OS != "" && (os_ != kind.PinOS[0] || osVer != kind.PinOS[1]) {
			return fmt.Errorf("%s is only available on %s %s",
				kind.Kind, kind.PinOS[0], kind.PinOS[1])
		}
		os_, osVer = kind.PinOS[0], kind.PinOS[1]
	}
	arch := strings.TrimSpace(s.Arch)
	if arch == "" {
		arch = platformArch()
	}

	// Reject an option the kind does not have, rather than accepting it and doing
	// nothing — a flag that is silently ignored is worse than one that is refused.
	checks := []struct {
		on   bool
		can  bool
		name string
	}{
		{s.Export, kind.CanExport, "export"},
		{s.Cert, kind.CanCert, "cert"},
		{s.GTID != nil, kind.CanGTID, "gtid"},
		{s.OS != "" && kind.ImageOnly, false, "os"},
		{s.Arch != "" && kind.ImageOnly, false, "arch"},
		// Nine node types never call applyVMSize (vagrant.go), so a CPU or memory
		// limit on them reaches nothing. Refused for the same reason as the rest.
		{s.CPUs > 0 || s.MemoryGB > 0, !kind.NoSizing, "cpus/memoryGb"},
		{s.DeviceReadMBps > 0 || s.DeviceWriteMBps > 0, !kind.NoSizing, "deviceReadMbps/deviceWriteMbps"},
		// Shaping needs ports to shape, and netemPortsFor (netem.go) knows them only
		// for the database engines and the two proxies.
		{s.NetLatencyMS > 0 || s.NetJitterMS > 0 || s.NetLossPct > 0 ||
			s.NetRateMbit > 0 || s.NetAllTraffic, kind.CanShape, "network shaping"},
		{s.CertTTL != "", kind.CanCert, "certTtl"},
		{s.ReplMode != "", kind.takes("replMode"), "replMode"},
		{s.Mode != "", kind.takes("mode"), "mode"},
		{s.Setup != "", kind.takes("setup"), "setup"},
		{s.MySQLRouter, kind.takes("mysqlRouter"), "mysqlRouter"},
		{len(s.Buckets) > 0, kind.takes("buckets"), "buckets"},
		{s.TLS, kind.takes("tls"), "tls"},
		{s.AlertEmail != "", kind.takes("alertEmail"), "alertEmail"},
		{s.Dataset != "", kind.takes("dataset"), "dataset"},
	}
	for _, l := range composeLinks {
		on, _ := s.link(l.Option)
		checks = append(checks, struct {
			on   bool
			can  bool
			name string
		}{on, kind.supports(l.Option), l.Option})
	}
	for _, chk := range checks {
		if chk.on && !chk.can {
			if kind.ImageOnly && (chk.name == "os" || chk.name == "arch") {
				return fmt.Errorf("%s runs a pulled or pre-baked image, so it has no %q to choose",
					kind.Kind, chk.name)
			}
			return fmt.Errorf("%s does not support %q", kind.Kind, chk.name)
		}
	}

	// Shape first, versions second. A member count or an unsupported option can be
	// checked without touching the catalogue, and "psmrs takes 3–7 members, not 1"
	// is a far more useful thing to be told than "nothing installable on
	// oraclelinux 9" — which is what came back when this ran the other way round.
	count := s.Count
	if count <= 0 {
		count = max(kind.Members, 1)
	}
	// A fixed-topology frame derives its size from the shape it must have, so a count
	// would be a number compose could not honour.
	if kind.Topology != nil {
		if s.Setup != "" && s.Setup != "standard" && s.Setup != "minimum" {
			return fmt.Errorf("%s: setup is \"standard\" or \"minimum\", not %q", kind.Kind, s.Setup)
		}
		if s.Count > 0 {
			return fmt.Errorf("%s has a fixed topology (one mongos, a config replica set and 3 shards), "+
				"so it takes no count — use setup=standard or setup=minimum", kind.Kind)
		}
	} else if kind.Frame {
		lo, hi := kind.MinMembers, kind.MaxMembers
		if lo > 0 && count < lo || hi > 0 && count > hi {
			return fmt.Errorf("%s takes %d–%d members, not %d", kind.Kind, lo, hi, count)
		}
	} else if count > 1 && (kind.Singleton || kind.Kind == "pmm") {
		// The validator allows exactly one Intranet, Keycloak, OpenBao, VNC and
		// Watchtower per stack. PMM is not on that list, but two of them is never
		// what a terse spec meant and it makes `monitor: true` ambiguous.
		return fmt.Errorf("%s: only one per stack", kind.Kind)
	}
	if kind.Singleton {
		for _, n := range b.doc.Nodes {
			if n.Type == kind.Type {
				return fmt.Errorf("%s: only one per stack", kind.Kind)
			}
		}
	}

	// Version.
	major, minor := "", ""
	switch {
	case kind.PDPSRepo:
		if minor, err = resolvePDPSRepo(s.Version); err != nil {
			return fmt.Errorf("%s: %w", kind.Kind, err)
		}
	case kind.Catalog == "pmm":
		if minor, err = resolvePMMVersion(s.Version); err != nil {
			return fmt.Errorf("%s: %w", kind.Kind, err)
		}
	case kind.Catalog != "":
		if major, minor, err = resolveCatalogVersion(
			composeCatalog(kind.Catalog), os_, osVer, arch, s.Version); err != nil {
			return fmt.Errorf("%s: %w", kind.Kind, err)
		}
	case s.Version != "":
		return fmt.Errorf("%s has no version to pin (it is a pulled image or has no choice)", kind.Kind)
	}

	// The MySQL OIDC plugin arrived in 8.4.11-11, and provisionMySQLOIDC is "skipped
	// (not fatal)" below that — so asking for it on 8.0 gets you a node with no SSO,
	// no error at compose, none at validate and none at deploy. Compose refuses
	// options a kind does not support; a version that does not support one is the
	// same silence, and reusing the provisioner's own predicate keeps the two from
	// drifting apart.
	if s.OIDC && kind.Kind == "ps" && !psVersionAtLeast(minor, mysqlOIDCMinVersion) {
		return fmt.Errorf("ps: %q needs Percona Server %s or newer (auth_openid_connect "+
			"arrived there); %s was resolved. Try version=8.4.11 or later",
			"oidc", mysqlOIDCMinVersion, minor)
	}

	res := composeResolved{
		Kind: kind.Kind, OS: os_, OSVersion: osVer, Arch: arch,
		Major: major, Version: minor,
	}
	if kind.ImageOnly {
		res.OS, res.OSVersion, res.Arch = "", "", ""
	}

	if kind.Frame {
		res.Name = b.label(kind, s.Name)
		fr := designFrame{
			ID: "c-" + res.Name, Type: kind.Type, Label: res.Name,
			OS: os_, OSVersion: osVer, Arch: arch,
			GTID: true, UseProxy: s.Proxy,
			CertTTLValue: 365, CertTTLUnit: "days",
			X: composeFrameX, Y: float64(composeFrameY + b.nextFrame*(composeFrameH+composeFrameGap)),
			W: float64(max(400, composeMemberIX*2+composeMemberDX*count)), H: composeFrameH,
		}
		if s.GTID != nil {
			fr.GTID = *s.GTID
		}
		if s.Cert {
			fr.GenerateCert = true
		}
		if v, u, err := parseCertTTL(s.CertTTL); err != nil {
			return fmt.Errorf("%s: %w", kind.Kind, err)
		} else if u != "" {
			fr.CertTTLValue, fr.CertTTLUnit = v, u
		}
		fr.ReplMode, fr.Mode, fr.PSMDBSetup = s.ReplMode, s.Mode, s.Setup
		fr.MySQLRouter = s.MySQLRouter
		if kind.SetVersion != nil {
			kind.SetVersion(major, minor, nil, &fr)
		}
		b.nextFrame++
		b.doc.Frames = append(b.doc.Frames, fr)
		res.FrameID = fr.ID

		// Members. Usually N of the same thing, numbered; for a fixed-topology frame,
		// whatever shape the frame has to have, each member carrying the role and
		// shard index the provisioner selects on.
		members := make([]composeMember, 0, count)
		if kind.Topology != nil {
			members = kind.Topology(s.Setup)
			count = len(members)
			fr.W = float64(max(400, composeMemberIX*2+composeMemberDX*count))
			b.doc.Frames[len(b.doc.Frames)-1].W = fr.W
		} else {
			prefix := b.memberPrefix(res.Name, count)
			for i := 0; i < count; i++ {
				m := composeMember{Suffix: strconv.Itoa(i + 1)}
				if kind.Roles != nil {
					m.Role = kind.Roles(i, count)
				}
				members = append(members, composeMember{Suffix: prefix + "-" + m.Suffix, Role: m.Role})
			}
		}
		for i, m := range members {
			label := m.Suffix
			if kind.Topology != nil {
				// The frame name has to be in there: labels are hostnames, and two
				// sharded clusters in one stack would otherwise both want "mongos".
				label = b.memberPrefix(res.Name, count) + "-" + m.Suffix
			}
			n := designNode{
				ID: fmt.Sprintf("n-%s-%d", res.Name, i+1), Type: kind.Type,
				Label:   label,
				FrameID: fr.ID,
				Role:    m.Role, Shard: m.Shard,
				CPUs: s.CPUs, MemoryGB: s.MemoryGB,
				ExportEnabled: s.Export && i == 0, ExportHostPort: s.ExportPort,
			}
			b.applyShaping(&n, s)
			b.usedLabels[n.Label] = true
			b.pos[n.ID] = [2]float64{
				fr.X + composeMemberIX + float64(i*composeMemberDX),
				fr.Y + composeMemberIY,
			}
			b.doc.Nodes = append(b.doc.Nodes, n)
			res.NodeIDs = append(res.NodeIDs, n.ID)
		}
		b.resolved = append(b.resolved, res)
		return nil
	}

	// A standalone node, possibly several.
	for i := 0; i < count; i++ {
		lbl := b.label(kind, s.Name)
		x, y := b.placeNode()
		n := designNode{
			ID: "n-" + lbl, Type: kind.Type, Label: lbl,
			OS: os_, OSVersion: osVer, Arch: arch,
			UseProxy: s.Proxy, CPUs: s.CPUs, MemoryGB: s.MemoryGB,
			ExportEnabled: s.Export, ExportHostPort: s.ExportPort,
			CertTTLValue: 365, CertTTLUnit: "days",
		}
		b.pos[n.ID] = [2]float64{x, y}
		if kind.CanGTID {
			n.GTID = true
		}
		if s.GTID != nil {
			n.GTID = *s.GTID
		}
		if s.Cert {
			n.GenerateCert = true
		}
		if v, u, err := parseCertTTL(s.CertTTL); err != nil {
			return fmt.Errorf("%s: %w", kind.Kind, err)
		} else if u != "" {
			n.CertTTLValue, n.CertTTLUnit = v, u
		}
		n.Mode, n.AlertEmail, n.MCDataset, n.TLS = s.Mode, s.AlertEmail, s.Dataset, s.TLS
		n.Buckets = s.Buckets
		b.applyShaping(&n, s)
		if kind.SetVersion != nil {
			kind.SetVersion(major, minor, &n, nil)
		}
		if kind.ImageOnly {
			// A pulled or pre-baked image has no OS to choose, so carrying the
			// resolved default would be noise in the design.
			n.OS, n.OSVersion, n.Arch = "", "", ""
		}
		switch kind.Kind {
		case "intranet":
			// Always the first thing on the canvas, where the eye starts.
			b.pos[n.ID] = [2]float64{composeColX, composeColX}
			b.intranetID = n.ID
		case "sambaad":
			b.sambaID = n.ID
		case "pmm":
			b.pmmIDs = append(b.pmmIDs, n.ID)
			b.pmmByName[lbl] = n.ID
		}
		b.doc.Nodes = append(b.doc.Nodes, n)
		if i == 0 {
			res.Name = lbl
		}
		res.NodeIDs = append(res.NodeIDs, n.ID)
	}
	b.resolved = append(b.resolved, res)
	return nil
}

// trimClusterSuffix turns "pxc-cluster-01" into "pxc" so members read "pxc-1".
func trimClusterSuffix(label string) string {
	if i := strings.Index(label, "-cluster"); i > 0 {
		return label[:i]
	}
	return label
}

// memberPrefix picks the name stem for a cluster's members.
//
// A label is the node's HOSTNAME on the stack network, so two of them colliding is a
// DNS collision, not a cosmetic problem — and the obvious stem does collide: both
// "pxc-cluster-01" and "pxc-cluster-02" trim to "pxc", so every member of the second
// cluster would be named over the first. So the short stem is used when it is free
// and the cluster's own (already unique) label when it is not: "pxc-1..3" for the
// common single-cluster case, "pxc-cluster-02-1..3" when there is more than one.
func (b *composeBuilder) memberPrefix(clusterLabel string, count int) string {
	short := trimClusterSuffix(clusterLabel)
	free := true
	for i := 1; i <= count; i++ {
		if b.usedLabels[fmt.Sprintf("%s-%d", short, i)] {
			free = false
			break
		}
	}
	if free {
		return short
	}
	return clusterLabel
}

// wire resolves the cross-references that a spec expresses as booleans.
//
// One loop over composeLinks rather than a branch per relationship: every one of them
// is "find the provider, write its id somewhere", and the only things that differ are
// which kinds can provide it and which field it lands in — both of which are data.
func (b *composeBuilder) wire(ordered []composeNodeSpec) error {
	for i, s := range ordered {
		if i >= len(b.resolved) {
			break
		}
		res := &b.resolved[i]
		for _, l := range composeLinks {
			on, with := s.link(l.Option)
			if !on {
				continue
			}
			id, err := b.provider(l, with)
			if err != nil {
				return fmt.Errorf("%s: %w", res.Name, err)
			}
			b.applyLink(l, res, id)
			res.Links = append(res.Links, l.Option+"→"+b.labelOf(id))
		}
		if err := b.associate(s, res); err != nil {
			return err
		}
	}
	return nil
}

// associate draws the canvas association line for a kind that needs one.
//
// This is the other half of the design, and for a long time compose emitted none of
// it: every stack came out with "edges": [], so a ProxySQL had no backend to route to,
// an HAProxy could not even validate (haproxyClusterFrames requires exactly one
// associated cluster), and every application simulator came up with nothing to drive.
// No amount of field-setting substitutes, because the provisioners resolve these by
// walking the edge graph — trafficSimTarget, backendFrameForProxySQL — and never by
// reading a field.
//
// The endpoint may be a node id OR a frame id: the walkers check both, which is how
// one line reaches a whole cluster. Edges are undirected everywhere they are read, so
// the from/to order carries no meaning; only the pair does.
func (b *composeBuilder) associate(s composeNodeSpec, res *composeResolved) error {
	kind, _ := composeKindByName(s.Kind)
	if len(kind.EdgeTo) == 0 {
		if s.To != "" {
			return fmt.Errorf("%s: %q has nothing to associate with", kind.Kind, "to")
		}
		return nil
	}
	// Candidates are whole resolved entries, not nodes, so a cluster is one choice
	// rather than three — which is also what makes the ambiguity message readable.
	var cands []composeResolved
	for _, r := range b.resolved {
		if r.Name == res.Name || !slices.Contains(kind.EdgeTo, r.Kind) {
			continue
		}
		cands = append(cands, r)
	}

	pick := -1
	switch {
	case s.To != "":
		for i, c := range cands {
			if c.Name == s.To {
				pick = i
			}
		}
		if pick < 0 {
			return fmt.Errorf("%s: no %s called %q in this spec. It can be associated with: %s",
				res.Name, strings.Join(kind.EdgeTo, ", "), s.To, orNone(names(cands)))
		}
	case len(cands) == 1:
		pick = 0
	case len(cands) == 0:
		return fmt.Errorf("%s has nothing to work on — it needs an association with %s, "+
			"and this spec has none. Add one, or drop the %s node",
			res.Name, strings.Join(kind.EdgeTo, ", "), kind.Kind)
	default:
		return fmt.Errorf("%s could be associated with %s — say which with to=<name>",
			res.Name, strings.Join(names(cands), " or "))
	}

	target := cands[pick]
	// A frame is addressed by the FRAME id, so one line covers every member; a
	// standalone by its single node. Both walkers accept either.
	endpoint := func(r composeResolved) string {
		if r.FrameID != "" {
			return r.FrameID
		}
		return r.NodeIDs[0]
	}
	// Source first: a "directional" edge means data flows from → to (intranet.go),
	// and every one of these is the database feeding the thing in front of it.
	b.doc.Edges = append(b.doc.Edges, designEdge{
		ID:   fmt.Sprintf("e-%s-%s", target.Name, res.Name),
		From: edgeEnd{Node: endpoint(target), Port: "right"},
		To:   edgeEnd{Node: endpoint(*res), Port: "left"},
		Type: "directional",
	})
	res.Links = append(res.Links, "to→"+target.Name)
	return nil
}

func names(rs []composeResolved) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "nothing in this spec"
	}
	return strings.Join(s, ", ")
}

// provider finds the node that satisfies a relationship.
//
// Ambiguity is refused rather than guessed: with two PMM nodes in a spec, "monitor"
// does not mean anything in particular, and picking one would be a coin toss the
// caller never sees.
func (b *composeBuilder) provider(l composeLink, want string) (string, error) {
	var candidates []designNode
	for _, n := range b.doc.Nodes {
		for _, p := range l.Provides {
			k, ok := composeKindByName(p)
			if ok && n.Type == k.Type {
				candidates = append(candidates, n)
			}
		}
	}
	if want != "" {
		for _, n := range candidates {
			if n.Label == want {
				return n.ID, nil
			}
		}
		return "", fmt.Errorf("no %s node called %q in this spec",
			strings.Join(l.Provides, " or "), want)
	}
	switch len(candidates) {
	case 1:
		return candidates[0].ID, nil
	case 0:
		return "", fmt.Errorf("%q needs a %s node — %s",
			l.Option, strings.Join(l.Provides, " or "), l.Missing)
	default:
		var names []string
		for _, n := range candidates {
			names = append(names, n.Label)
		}
		return "", fmt.Errorf("%q is ambiguous — this spec has %s. Name one with %sWith",
			l.Option, strings.Join(names, ", "), l.Option)
	}
}

// applyLink writes the reference, on the frame for a cluster and on every member
// node otherwise. A row that only handles one of the two simply ignores the nil.
func (b *composeBuilder) applyLink(l composeLink, res *composeResolved, providerID string) {
	if res.FrameID != "" {
		for i := range b.doc.Frames {
			if b.doc.Frames[i].ID == res.FrameID {
				l.Apply(providerID, nil, &b.doc.Frames[i])
			}
		}
		// A few relationships live on the members rather than the frame, so offer
		// both; Apply takes whichever it needs.
		for _, id := range res.NodeIDs {
			if n := b.node(id); n != nil {
				l.Apply(providerID, n, nil)
			}
		}
		return
	}
	for _, id := range res.NodeIDs {
		if n := b.node(id); n != nil {
			l.Apply(providerID, n, nil)
		}
	}
}

func (b *composeBuilder) node(id string) *designNode {
	for i := range b.doc.Nodes {
		if b.doc.Nodes[i].ID == id {
			return &b.doc.Nodes[i]
		}
	}
	return nil
}

func (b *composeBuilder) labelOf(id string) string {
	for i := range b.doc.Nodes {
		if b.doc.Nodes[i].ID == id {
			return b.doc.Nodes[i].Label
		}
	}
	return id
}

// designJSON marshals the document with each node's canvas coordinates folded in.
//
// The round trip through a map is what lets compose add fields designNode does not
// declare. It is not a hack for its own sake: the store treats a design as opaque
// JSON and the browser reads x/y, so the alternative was widening a shared struct
// for the benefit of one writer.
func designJSON(doc designDoc, pos map[string][2]float64) ([]byte, error) {
	nodes := make([]map[string]any, 0, len(doc.Nodes))
	for _, n := range doc.Nodes {
		raw, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		if p, ok := pos[n.ID]; ok {
			m["x"], m["y"] = p[0], p[1]
		}
		nodes = append(nodes, m)
	}
	// Edges, not a hardcoded empty list. It was literal `[]designEdge{}` back when
	// compose drew no associations, and it stayed that way after it started to —
	// so the plan reported every link it had made and the saved design had none of
	// them. json.Marshal of a nil slice is `null`, which the designer will not read,
	// so an empty document still needs the empty list.
	edges := doc.Edges
	if edges == nil {
		edges = []designEdge{}
	}
	return json.Marshal(map[string]any{
		"nodes": nodes, "frames": doc.Frames, "edges": edges,
		"view": map[string]any{"x": 0, "y": 0, "z": 1},
	})
}

// ------------------------------------------------------------- handler

func (a *App) handleComposeStack(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var spec composeSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(spec.Name) == "" && !spec.DryRun {
		writeErr(w, http.StatusBadRequest, "a stack needs a name")
		return
	}
	ttl := spec.TTL
	if ttl == "" {
		ttl = "8h"
	}
	if !validTTL(ttl) {
		writeErr(w, http.StatusBadRequest,
			"ttl must be one of 2h, 4h, 8h, 24h, 2w, infinity")
		return
	}

	doc, pos, resolved, added, err := buildCompose(spec)
	if err != nil {
		// A spec error is the caller's to fix and the message is the whole value of
		// this endpoint, so it goes back verbatim.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	design, err := designJSON(doc, pos)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to encode the design")
		return
	}

	out := composeResult{
		Design: design, Resolved: resolved, Added: added, Issues: []issue{},
	}

	// A dry run resolves and reports without creating anything, so a caller can show
	// the plan first. It cannot run the validator, which needs a stored stack — that
	// is stated rather than faked.
	if spec.DryRun {
		out.OK = true
		writeJSON(w, http.StatusOK, out)
		return
	}

	st, err := a.store.CreateStack(strings.TrimSpace(spec.Name), u.ID, ttl, expiryFor(ttl), design)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the stack")
		return
	}
	out.Stack = &st

	// Held to the same standard as a hand-drawn design: the caller gets the problems
	// the canvas would have shown them.
	issues := a.validateStack(r.Context(), st)
	if issues != nil {
		out.Issues = issues
	}
	out.OK = !hasError(issues)

	writeJSON(w, http.StatusOK, out)
}

// handleComposeKinds documents the spec language from the same table that implements
// it, for the same reason /api/meta/endpoints exists: a hand-written list of kinds
// and their options would be wrong within a release.
func (a *App) handleComposeKinds(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, composeKindsDoc())
}

// composeKindsDoc builds the spec-language catalogue. Separate from the handler so a
// test can hold it against the kind table it describes: the two drifted apart once —
// the catalogue went on advertising cpus and memoryGb for the nine node types that
// silently ignore them — and this endpoint is what the CLI and the docs are written
// from, so a lie here is a lie everywhere.
func composeKindsDoc() map[string]any {
	type kindDoc struct {
		Kind     string   `json:"kind"`
		NodeType string   `json:"nodeType"`
		Cluster  bool     `json:"cluster"`
		Members  string   `json:"members,omitempty"`
		Catalog  string   `json:"catalog,omitempty"`
		Options  []string `json:"options"`
		Needs    []string `json:"needs,omitempty"`
		About    string   `json:"about"`
	}
	kinds := make([]kindDoc, 0, len(composeKinds))
	for _, k := range composeKinds {
		d := kindDoc{Kind: k.Kind, NodeType: k.Type, Cluster: k.Frame,
			Catalog: k.Catalog, About: k.About}
		switch {
		case k.Topology != nil:
			d.Members = fmt.Sprintf("fixed: %d (standard) or %d (minimum)",
				len(k.Topology("standard")), len(k.Topology("minimum")))
		case k.Frame:
			d.Members = fmt.Sprintf("%d–%d (default %d)", k.MinMembers, k.MaxMembers, k.Members)
		}
		d.Options = []string{"name", "proxy"}
		// Only what this kind actually reads. Advertising an option that is refused
		// is the same failure as accepting one that does nothing — worse, because it
		// is this endpoint the CLI and the docs are generated from.
		if !k.NoSizing {
			d.Options = append(d.Options, "cpus", "memoryGb",
				"deviceReadMbps", "deviceWriteMbps")
		}
		if k.CanShape {
			d.Options = append(d.Options,
				"netLatencyMs", "netJitterMs", "netLossPct", "netRateMbit", "netAllTraffic")
		}
		if !k.ImageOnly {
			// A pulled or pre-baked image has no OS to pick, so offering the option
			// would be advertising a flag that does nothing.
			d.Options = append(d.Options, "os", "arch")
		}
		if k.Catalog != "" || k.PDPSRepo {
			d.Options = append(d.Options, "version")
		}
		if k.Frame && k.Topology == nil {
			d.Options = append(d.Options, "count")
		}
		for _, o := range []struct {
			on   bool
			name string
		}{
			{k.CanExport, "export"}, {k.CanExport, "exportPort"},
			{k.CanCert, "cert"}, {k.CanCert, "certTtl"}, {k.CanGTID, "gtid"},
			{len(k.EdgeTo) > 0, "to"},
		} {
			if o.on {
				d.Options = append(d.Options, o.name)
			}
		}
		d.Options = append(d.Options, k.Scalars...)
		d.Options = append(d.Options, k.Links...)
		// And say what each relationship will connect to, since "oidc" is only
		// actionable once you know it needs a keycloak node in the same spec.
		for _, name := range k.Links {
			if l, ok := composeLinkByOption(name); ok {
				d.Needs = append(d.Needs, name+" → "+strings.Join(l.Provides, "|"))
			}
		}
		// The association is a requirement, not an option, so it is stated as one.
		if len(k.EdgeTo) > 0 {
			d.Needs = append(d.Needs, "to → "+strings.Join(k.EdgeTo, "|"))
		}
		sort.Strings(d.Options)
		kinds = append(kinds, d)
	}
	unsupported := make([]map[string]string, 0, len(unsupportedKinds))
	names := make([]string, 0, len(unsupportedKinds))
	for k := range unsupportedKinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		unsupported = append(unsupported, map[string]string{"kind": k, "why": unsupportedKinds[k]})
	}
	oses := make([]string, 0, len(osAliases))
	for k := range osAliases {
		oses = append(oses, k)
	}
	sort.Strings(oses)

	return map[string]any{
		"kinds":       kinds,
		"unsupported": unsupported,
		"osAliases":   oses,
		"defaultOS":   composeDefaultOS[0] + " " + composeDefaultOS[1],
		"notes": []string{
			"An Intranet node is added automatically if the spec has none — every stack needs one.",
			"\"monitor\" requires a pmm node in the same spec; it is never added for you.",
			"\"version\" accepts a full package version, an upstream release (8.4.5), a series (8.4), or \"\" for latest.",
			"\"to\" draws the association line a proxy or a simulator needs. Leave it out and " +
				"the only legal target is used; with several, name one.",
			"Network shaping (netLatencyMs, netLossPct, …) applies to the node's own database " +
				"ports unless netAllTraffic is set. It is what reproduces a slow or lossy link.",
			"Kinds not listed here are built by starting from a template and editing the design.",
		},
	}
}

// applyShaping copies the per-node resource limits onto a node. Kept in one place
// because a member and a standalone take exactly the same set, and because the
// alternative — repeating eight assignments twice — is how one of them ends up
// missing a field.
func (b *composeBuilder) applyShaping(n *designNode, s composeNodeSpec) {
	n.NetLatencyMS, n.NetJitterMS = s.NetLatencyMS, s.NetJitterMS
	n.NetLossPct, n.NetRateMbit = s.NetLossPct, s.NetRateMbit
	n.NetAllTraffic = s.NetAllTraffic
	n.DeviceReadMBps, n.DeviceWriteMBps = s.DeviceReadMBps, s.DeviceWriteMBps
}

// parseCertTTL reads "365d", "30m" or "2h" into the value/unit pair the design and
// the certificate machinery use. Short TTLs are the point of the option — a 30-minute
// certificate is how you watch something fail to renew — so minutes are accepted
// alongside days. Returns an empty unit for an empty input, meaning "leave the
// default alone".
func parseCertTTL(ttl string) (int, string, error) {
	ttl = strings.TrimSpace(ttl)
	if ttl == "" {
		return 0, "", nil
	}
	units := map[byte]string{'d': "days", 'h': "hours", 'm': "minutes"}
	unit, ok := units[ttl[len(ttl)-1]]
	if !ok {
		return 0, "", fmt.Errorf("certTtl %q: expected a number and d, h or m — 365d, 2h, 30m", ttl)
	}
	v, err := strconv.Atoi(ttl[:len(ttl)-1])
	if err != nil || v <= 0 {
		return 0, "", fmt.Errorf("certTtl %q: expected a number and d, h or m — 365d, 2h, 30m", ttl)
	}
	return v, unit, nil
}
