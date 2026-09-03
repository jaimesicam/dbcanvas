package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// compose_test.go — the spec language, and the one scenario it was written for.
//
// TestComposeTheAskedForStack is the headline: three PXC 8.4.5 nodes on EL8, a Percona
// Server 8.0.45 on EL9 authenticating against the Intranet's LDAP, and a PMM 3
// monitoring both. It asserts the whole resolved design, because every part of it is
// something a person would otherwise have had to know: that 8.4.5 is packaged
// 8.4.5-5.1 on Oracle Linux, that a cluster is a frame plus members carrying its id,
// that monitoring is a field holding another node's id, and that the Intranet has to
// be there at all.
//
// The catalogue is synthetic so these tests do not change meaning when `make versions`
// next runs — but it is shaped exactly like the real one, including the detail that
// makes version resolution worth having: the same upstream release carries a different
// package suffix on EL and on Ubuntu.

const composeVersionsYAML = `image_prefix: dbcanvas-systemd
images:
  - os: oraclelinux
    version: "8"
    platform: linux/amd64
    arch: amd64
    percona_server:
      "8.4":
        - 8.4.6-6.1
        - 8.4.5-5.1
      "8.0":
        - 8.0.46-37.1
        - 8.0.45-36.1
        - 8.0.44-35.1
    percona_xtradb_cluster:
      "8.4":
        - 8.4.6-6.1
        - 8.4.5-5.1
      "8.0":
        - 8.0.46-37.1
        - 8.0.45-36.1
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    arch: amd64
    percona_server:
      "8.4":
        - 8.4.6-6.1
      "8.0":
        - 8.0.46-37.1
        - 8.0.45-36.1
    percona_xtradb_cluster:
      "8.4":
        - 8.4.6-6.1
        - 8.4.5-5.1
    percona_postgresql:
      "17":
        - 17.4
      "16":
        - 16.8
    percona_server_mongodb:
      "8.0":
        - 8.0.12-4
    percona_orchestrator:
      "3":
        - 3.2.6-22
  - os: ubuntu
    version: "24.04"
    platform: linux/amd64
    arch: amd64
    percona_server:
      "8.0":
        - 8.0.45-36-1
    percona_xtradb_cluster:
      "8.4":
        - 8.4.5-5-1
pmm:
  repository: percona/pmm-server
  default_tag: "3"
  latest: "3.9.1"
  versions:
    - "3.9.1"
    - "3.9.0"
    - "3.8.1"
`

func composeCatalogFixture(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(composeVersionsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
	// The fixture is amd64 only; platformArch() follows DOCKER_PLATFORM.
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
}

// build is the whole pipeline, for a test that only cares about the result.
func composeBuild(t *testing.T, spec composeSpec) (designDoc, map[string][2]float64, []composeResolved, []string) {
	t.Helper()
	doc, pos, res, added, err := buildCompose(spec)
	if err != nil {
		t.Fatalf("buildCompose: %v", err)
	}
	return doc, pos, res, added
}

func hasLink(links []string, want string) bool {
	for _, l := range links {
		if l == want {
			return true
		}
	}
	return false
}

func nodesOfType(doc designDoc, typ string) []designNode {
	var out []designNode
	for _, n := range doc.Nodes {
		if n.Type == typ {
			out = append(out, n)
		}
	}
	return out
}

// ------------------------------------------------------------- the scenario

func TestComposeTheAskedForStack(t *testing.T) {
	composeCatalogFixture(t)

	doc, pos, resolved, added := composeBuild(t, composeSpec{
		Name: "repro-1234",
		Nodes: []composeNodeSpec{
			{Kind: "pxc", Count: 3, Version: "8.4.5", OS: "el8", Monitor: true},
			{Kind: "ps", Version: "8.0.45", OS: "el9", LDAP: true, Monitor: true},
			{Kind: "pmm", Version: "3"},
		},
	})

	// --- the Intranet nobody asked for, because everything needs it ---------
	intra := nodesOfType(doc, "intranet")
	if len(intra) != 1 {
		t.Fatalf("got %d intranet nodes, want exactly 1 (added automatically)", len(intra))
	}
	if len(added) != 1 || !strings.Contains(added[0], "Intranet") {
		t.Errorf("the added Intranet was not reported to the caller: %v", added)
	}

	// --- the PXC cluster ----------------------------------------------------
	if len(doc.Frames) != 1 {
		t.Fatalf("got %d frames, want 1 PXC cluster", len(doc.Frames))
	}
	fr := doc.Frames[0]
	if fr.Type != "pxc" {
		t.Errorf("frame type %q, want pxc", fr.Type)
	}
	if fr.OS != "oraclelinux" || fr.OSVersion != "8" {
		t.Errorf("cluster OS %s %s, want oraclelinux 8 (from \"el8\")", fr.OS, fr.OSVersion)
	}
	// The whole point: "8.4.5" became the series plus the EL package build.
	if fr.PXCMajor != "8.4" || fr.PXCVersion != "8.4.5-5.1" {
		t.Errorf("cluster version %s / %s, want 8.4 / 8.4.5-5.1", fr.PXCMajor, fr.PXCVersion)
	}
	members := nodesOfType(doc, "pxc")
	if len(members) != 3 {
		t.Fatalf("got %d PXC members, want 3", len(members))
	}
	for i, m := range members {
		if m.FrameID != fr.ID {
			t.Errorf("member %d is not in the frame (frameId %q, want %q)", i, m.FrameID, fr.ID)
		}
		if m.Role != "regular" {
			t.Errorf("member %d role %q, want regular", i, m.Role)
		}
		if _, ok := pos[m.ID]; !ok {
			t.Errorf("member %d has no canvas position", i)
		}
	}

	// --- the standalone Percona Server --------------------------------------
	ps := nodesOfType(doc, "ps")
	if len(ps) != 1 {
		t.Fatalf("got %d ps nodes, want 1", len(ps))
	}
	if ps[0].OS != "oraclelinux" || ps[0].OSVersion != "9" {
		t.Errorf("ps OS %s %s, want oraclelinux 9", ps[0].OS, ps[0].OSVersion)
	}
	if ps[0].PSMajor != "8.0" || ps[0].PSVersion != "8.0.45-36.1" {
		t.Errorf("ps version %s / %s, want 8.0 / 8.0.45-36.1", ps[0].PSMajor, ps[0].PSVersion)
	}
	if !ps[0].LdapAuth {
		t.Error("ps: ldap was requested but ldapAuth is not set")
	}
	if ps[0].LdapDirNodeID != intra[0].ID {
		t.Errorf("ps ldapDirNodeId %q, want the Intranet's id %q", ps[0].LdapDirNodeID, intra[0].ID)
	}

	// --- monitoring ---------------------------------------------------------
	pmm := nodesOfType(doc, "pmm")
	if len(pmm) != 1 {
		t.Fatalf("got %d pmm nodes, want 1", len(pmm))
	}
	if pmm[0].Version != "3" {
		t.Errorf("pmm version %q, want 3", pmm[0].Version)
	}
	// A cluster is monitored via its FRAME, a standalone node via the node.
	if fr.PMMNodeID != pmm[0].ID {
		t.Errorf("the cluster's pmmNodeId is %q, want %q", fr.PMMNodeID, pmm[0].ID)
	}
	if ps[0].PMMNodeID != pmm[0].ID {
		t.Errorf("ps pmmNodeId is %q, want %q", ps[0].PMMNodeID, pmm[0].ID)
	}

	// --- what the caller is told --------------------------------------------
	byKind := map[string]composeResolved{}
	for _, r := range resolved {
		byKind[r.Kind] = r
	}
	if got := byKind["pxc"]; got.Version != "8.4.5-5.1" ||
		!hasLink(got.Links, "monitor→pmm-01") {
		t.Errorf("resolved pxc reported as %+v", got)
	}
	if got := byKind["ps"]; got.Version != "8.0.45-36.1" ||
		!hasLink(got.Links, "ldap→intranet-01") || !hasLink(got.Links, "monitor→pmm-01") {
		t.Errorf("resolved ps reported as %+v", got)
	}

	// --- and it marshals to something the canvas can open -------------------
	raw, err := designJSON(doc, pos)
	if err != nil {
		t.Fatalf("designJSON: %v", err)
	}
	var back struct {
		Nodes  []map[string]any `json:"nodes"`
		Frames []map[string]any `json:"frames"`
		View   map[string]any   `json:"view"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the design does not round-trip: %v", err)
	}
	if len(back.Nodes) != 6 { // intranet + 3 pxc + ps + pmm
		t.Errorf("design has %d nodes, want 6", len(back.Nodes))
	}
	for _, n := range back.Nodes {
		if _, ok := n["x"]; !ok {
			t.Errorf("node %v has no x coordinate — it would open in a pile", n["id"])
			break
		}
	}
	if back.View == nil {
		t.Error("the design has no view — the canvas needs one")
	}
}

// TestComposeSameReleaseDifferentPackage is why version resolution exists at all.
func TestComposeSameReleaseDifferentPackage(t *testing.T) {
	composeCatalogFixture(t)
	for _, c := range []struct{ os, want string }{
		{"el8", "8.4.5-5.1"},
		{"ubuntu24.04", "8.4.5-5-1"},
	} {
		doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: "pxc", Version: "8.4.5", OS: c.os},
		}})
		if got := doc.Frames[0].PXCVersion; got != c.want {
			t.Errorf("PXC 8.4.5 on %s resolved to %q, want %q", c.os, got, c.want)
		}
	}
}

// ------------------------------------------------------------- OS aliases

func TestResolveOS(t *testing.T) {
	for _, c := range []struct{ in, family, release string }{
		{"", "oraclelinux", "9"}, // the default
		{"el8", "oraclelinux", "8"},
		{"EL8", "oraclelinux", "8"},
		{"ol9", "oraclelinux", "9"},
		{"rhel9", "oraclelinux", "9"},
		{"oraclelinux:10", "oraclelinux", "10"},
		{"ubuntu24.04", "ubuntu", "24.04"},
		{"noble", "ubuntu", "24.04"},
		{"jammy", "ubuntu", "22.04"},
		{"bookworm", "debian", "12"},
		{"debian:13", "debian", "13"},
		{"oraclelinux/9", "oraclelinux", "9"}, // the explicit escape hatch
	} {
		f, r, err := resolveOS(c.in)
		if err != nil {
			t.Errorf("resolveOS(%q): %v", c.in, err)
			continue
		}
		if f != c.family || r != c.release {
			t.Errorf("resolveOS(%q) = %s %s, want %s %s", c.in, f, r, c.family, c.release)
		}
	}
	if _, _, err := resolveOS("centos7"); err == nil {
		t.Error("an unknown OS was accepted")
	} else if !strings.Contains(err.Error(), "el8") {
		t.Errorf("the error should list what is known: %v", err)
	}
}

// ------------------------------------------------------------- versions

func TestResolveCatalogVersion(t *testing.T) {
	composeCatalogFixture(t)
	ps := composeCatalog("percona_server")

	for _, c := range []struct {
		name, want, major, minor string
	}{
		{"empty takes the newest series, latest build", "", "8.4", ""},
		{"a whole series", "8.0", "8.0", ""},
		{"an upstream release", "8.0.45", "8.0", "8.0.45-36.1"},
		{"a full package version", "8.0.45-36.1", "8.0", "8.0.45-36.1"},
		{"a release in another series", "8.4.5", "8.4", "8.4.5-5.1"},
	} {
		major, minor, err := resolveCatalogVersion(ps, "oraclelinux", "8", "amd64", c.want)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if major != c.major || minor != c.minor {
			t.Errorf("%s: %q → %s / %q, want %s / %q", c.name, c.want, major, minor, c.major, c.minor)
		}
	}
}

// TestResolveVersionPrefixBoundary is the bug a naive HasPrefix would have: asking
// for 8.0.4 must not silently hand back 8.0.45.
func TestResolveVersionPrefixBoundary(t *testing.T) {
	composeCatalogFixture(t)
	ps := composeCatalog("percona_server")
	if _, minor, err := resolveCatalogVersion(ps, "oraclelinux", "8", "amd64", "8.0.4"); err == nil {
		t.Errorf("8.0.4 matched %q — a prefix must end on a separator", minor)
	}
}

func TestResolveVersionErrorsAreUseful(t *testing.T) {
	composeCatalogFixture(t)
	ps := composeCatalog("percona_server")

	// A version that does not exist, in a series that does: name what does.
	_, _, err := resolveCatalogVersion(ps, "oraclelinux", "8", "amd64", "8.0.99")
	if err == nil {
		t.Fatal("a non-existent version was accepted")
	}
	if !strings.Contains(err.Error(), "8.0.45-36.1") {
		t.Errorf("the error should list what is available: %v", err)
	}

	// A whole series that does not exist: name the series that do.
	_, _, err = resolveCatalogVersion(ps, "oraclelinux", "8", "amd64", "5.7.44")
	if err == nil {
		t.Fatal("a non-existent series was accepted")
	}
	if !strings.Contains(err.Error(), "8.0") {
		t.Errorf("the error should list the available series: %v", err)
	}

	// An OS with no image at all.
	_, _, err = resolveCatalogVersion(ps, "debian", "13", "amd64", "")
	if err == nil {
		t.Fatal("an OS with no image was accepted")
	}
	if !strings.Contains(err.Error(), "make images") {
		t.Errorf("the error should say how to fix it: %v", err)
	}

	// An engine not published for an OS that does exist: PostgreSQL is only in the
	// el9 fixture entry.
	_, _, err = resolveCatalogVersion(composeCatalog("percona_postgresql"),
		"oraclelinux", "8", "amd64", "")
	if err == nil {
		t.Error("an engine absent from that image was accepted")
	}
}

func TestResolvePMMVersion(t *testing.T) {
	composeCatalogFixture(t)
	for _, c := range []struct{ in, want string }{
		{"", ""},           // the catalog default tag
		{"3", "3"},         // the published major tag
		{"3.9.1", "3.9.1"}, // exact
		{"3.9", "3.9.1"},   // newest in the release
	} {
		got, err := resolvePMMVersion(c.in)
		if err != nil {
			t.Errorf("resolvePMMVersion(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolvePMMVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := resolvePMMVersion("2.41.0"); err == nil {
		t.Error("a PMM version this installation cannot run was accepted")
	}
}

// ------------------------------------------------------------- refusals

func TestComposeRefusals(t *testing.T) {
	composeCatalogFixture(t)

	for _, c := range []struct {
		name  string
		spec  composeSpec
		wants string
	}{
		{"no nodes", composeSpec{Name: "x"}, "at least one node"},
		{"no kind", composeSpec{Name: "x", Nodes: []composeNodeSpec{{}}}, `needs a "kind"`},
		{"unknown kind",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "postgres"}}},
			"unknown kind"},
		// Refused by name with the reason, not silently absent.
		{"deliberately unsupported kind",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "k3d"}}},
			"operator choice"},
		// A fixed-topology frame has no count to give it.
		{"a count on a fixed topology",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "psmdb", Count: 3}}},
			"fixed topology"},
		{"an unknown setup",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "psmdb", Setup: "tiny"}}},
			`"standard" or "minimum"`},
		// Options that reach nothing on this kind are refused, not accepted.
		{"sizing on a kind that ignores it",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "keycloak", CPUs: 4}}},
			"cpus/memoryGb"},
		{"shaping on a kind with no ports to shape",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "pmm", NetLatencyMS: 50}}},
			"network shaping"},
		{"a scalar the kind does not have",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "ps", Mode: "loadbal"}}},
			`does not support "mode"`},
		{"a malformed certTtl",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "ps", Cert: true, CertTTL: "30"}}},
			"365d, 2h, 30m"},
		// A node that needs an association and has nothing to associate with is a
		// design that could never deploy, so it is refused at compose.
		{"a proxy with no backend",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "haproxy"}}},
			"nothing to work on"},
		{"an ambiguous association",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{
				{Kind: "pxc", Name: "a", OS: "el9"}, {Kind: "pxc", Name: "b", OS: "el9"},
				{Kind: "haproxy"}}},
			"say which with to="},
		{"an association naming nothing",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{
				{Kind: "pxc", OS: "el9"}, {Kind: "haproxy", To: "nope"}}},
			`called "nope"`},
		{"monitor with no PMM",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "ps", Monitor: true}}},
			`add {"kind":"pmm"}`},
		{"an option the kind does not have",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "keycloak", Monitor: true}}},
			`does not support "monitor"`},
		{"too few cluster members",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "spock", Count: 1}}},
			"members"},
		{"too many cluster members",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "pxc", Count: 99}}},
			"members"},
		{"two intranets",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet", Count: 2}}},
			"only one per stack"},
		{"a version on something with no version",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "keycloak", Version: "26"}}},
			"no version to pin"},
		{"an unavailable version",
			composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "ps", Version: "5.7.44", OS: "el8"}}},
			"5.7.44"},
	} {
		_, _, _, _, err := buildCompose(c.spec)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: error %q should mention %q", c.name, err.Error(), c.wants)
		}
	}
}

// TestComposeAmbiguousMonitoring: two PMM nodes make "monitor": true meaningless, so
// it is refused with the way to disambiguate rather than guessed.
func TestComposeAmbiguousMonitoring(t *testing.T) {
	composeCatalogFixture(t)
	_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pmm", Name: "pmm-a"},
		{Kind: "pmm", Name: "pmm-b"},
		{Kind: "ps", Monitor: true},
	}})
	if err == nil {
		t.Fatal("monitoring with two PMM nodes was accepted")
	}
	if !strings.Contains(err.Error(), "monitorWith") {
		t.Errorf("the error should say how to disambiguate: %v", err)
	}

	// And naming one works.
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pmm", Name: "pmm-a"},
		{Kind: "pmm", Name: "pmm-b"},
		{Kind: "ps", Monitor: true, MonitorWith: "pmm-b"},
	}})
	var wantID string
	for _, n := range doc.Nodes {
		if n.Label == "pmm-b" {
			wantID = n.ID
		}
	}
	if got := nodesOfType(doc, "ps")[0].PMMNodeID; got != wantID {
		t.Errorf("ps is monitored by %q, want pmm-b's id %q", got, wantID)
	}
}

// ------------------------------------------------------------- shape

func TestComposeLabelsAndIDsAreUnique(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "ps"}, {Kind: "ps"}, {Kind: "ps", Count: 2},
		{Kind: "pxc"}, {Kind: "pxc"},
	}})
	ids, labels := map[string]bool{}, map[string]bool{}
	for _, n := range doc.Nodes {
		if ids[n.ID] {
			t.Errorf("duplicate node id %q", n.ID)
		}
		if labels[n.Label] {
			t.Errorf("duplicate label %q — labels are hostnames on the stack network", n.Label)
		}
		ids[n.ID], labels[n.Label] = true, true
	}
	for _, f := range doc.Frames {
		if ids[f.ID] {
			t.Errorf("frame id %q collides with a node id", f.ID)
		}
		ids[f.ID] = true
	}
}

func TestComposeReplicationRoles(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "ps-repl", Count: 3, OS: "el8"},
	}})
	members := nodesOfType(doc, "mysql")
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3", len(members))
	}
	if members[0].Role != "primary" {
		t.Errorf("first member role %q, want primary", members[0].Role)
	}
	for _, m := range members[1:] {
		if m.Role != "secondary" {
			t.Errorf("member %s role %q, want secondary", m.Label, m.Role)
		}
	}
}

func TestComposeFrameGeometryHoldsItsMembers(t *testing.T) {
	composeCatalogFixture(t)
	doc, pos, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", Count: 5, OS: "el8"},
	}})
	fr := doc.Frames[0]
	for _, m := range nodesOfType(doc, "pxc") {
		p, ok := pos[m.ID]
		if !ok {
			t.Fatalf("%s has no position", m.Label)
		}
		if p[0] < fr.X || p[0] > fr.X+fr.W {
			t.Errorf("%s at x=%.0f falls outside its frame [%.0f, %.0f]",
				m.Label, p[0], fr.X, fr.X+fr.W)
		}
		if p[1] < fr.Y || p[1] > fr.Y+fr.H {
			t.Errorf("%s at y=%.0f falls outside its frame [%.0f, %.0f]",
				m.Label, p[1], fr.Y, fr.Y+fr.H)
		}
	}
}

// TestComposeKindTableIsConsistent guards the table itself: every row that names a
// catalogue must know how to write the version it resolves, and every cluster must
// have sane bounds. A row with a hole in it would fail at run time on one spec.
func TestComposeKindTableIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range composeKinds {
		if seen[k.Kind] {
			t.Errorf("duplicate kind %q", k.Kind)
		}
		seen[k.Kind] = true
		if k.Type == "" || k.About == "" {
			t.Errorf("%s: incomplete row", k.Kind)
		}
		if k.Catalog != "" && k.SetVersion == nil {
			t.Errorf("%s names a catalogue but cannot write the version it resolves", k.Kind)
		}
		if k.Frame && k.Topology != nil {
			if k.Members != 0 || k.MinMembers != 0 || k.MaxMembers != 0 {
				t.Errorf("%s: a fixed topology takes no member bounds", k.Kind)
			}
			if n := len(k.Topology("")); n == 0 {
				t.Errorf("%s: fixed topology is empty", k.Kind)
			}
		} else if k.Frame {
			if k.Members <= 0 || k.MinMembers <= 0 || k.MaxMembers < k.MinMembers {
				t.Errorf("%s: member bounds are %d..%d (default %d)",
					k.Kind, k.MinMembers, k.MaxMembers, k.Members)
			}
			if k.Members < k.MinMembers || k.Members > k.MaxMembers {
				t.Errorf("%s: default %d is outside %d..%d",
					k.Kind, k.Members, k.MinMembers, k.MaxMembers)
			}
		}
		if _, no := unsupportedKinds[k.Kind]; no {
			t.Errorf("%s is both supported and listed as unsupported", k.Kind)
		}
	}
}

// TestComposeEveryKindBuilds walks the whole table, so a row that resolves for nobody
// is caught here rather than by the first person to try it.
func TestComposeEveryKindBuilds(t *testing.T) {
	composeCatalogFixture(t)
	for _, k := range composeKinds {
		if k.Kind == "intranet" {
			continue // always added anyway
		}
		entry := composeNodeSpec{Kind: k.Kind}
		if k.PinOS[0] == "" && !k.ImageOnly {
			// A pinned or image-only kind refuses an OS, correctly.
			entry.OS = "el9"
		}
		nodes := []composeNodeSpec{entry}
		// A kind that needs an association gets one, since without a target it is
		// correctly refused — that refusal is covered in TestComposeRefusals.
		if len(k.EdgeTo) > 0 {
			t, _ := composeKindByName(k.EdgeTo[0])
			target := composeNodeSpec{Kind: t.Kind}
			if t.PinOS[0] == "" && !t.ImageOnly {
				target.OS = "el9"
			}
			nodes = append([]composeNodeSpec{target}, nodes...)
		}
		spec := composeSpec{Name: "x", Nodes: nodes}
		doc, _, _, _, err := buildCompose(spec)
		if err != nil {
			// A kind absent from the synthetic catalogue is expected; a kind that
			// fails for any other reason is not.
			if strings.Contains(err.Error(), "installable") || strings.Contains(err.Error(), "no ") {
				continue
			}
			t.Errorf("%s: %v", k.Kind, err)
			continue
		}
		if k.Frame && len(doc.Frames) < 1 {
			t.Errorf("%s: built no frame", k.Kind)
		}
		if len(k.EdgeTo) > 0 && len(doc.Edges) != 1 {
			t.Errorf("%s: built %d association lines, want 1", k.Kind, len(doc.Edges))
		}
		if !k.Frame {
			if len(nodesOfType(doc, k.Type)) != 1 {
				t.Errorf("%s: built no node of type %q", k.Kind, k.Type)
			}
		}
	}
}

// ------------------------------------------------------------- relationships

// TestComposeKeycloakSSO is the case that prompted the link table: a Keycloak node was
// deployable but nothing could be wired to it, so the one thing you want Keycloak for
// was the one thing compose could not do.
func TestComposeKeycloakSSO(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, resolved, _ := composeBuild(t, composeSpec{Name: "sso", Nodes: []composeNodeSpec{
		{Kind: "keycloak"},
		{Kind: "ps", Version: "8.4", OS: "el8", OIDC: true},
		{Kind: "pmm", OIDC: true},
	}})

	var kcID string
	for _, n := range doc.Nodes {
		if n.Type == "keycloak" {
			kcID = n.ID
		}
	}
	if kcID == "" {
		t.Fatal("no keycloak node was created")
	}
	ps := nodesOfType(doc, "ps")[0]
	if !ps.EnableOIDC || ps.KeycloakNodeID != kcID {
		t.Errorf("ps OIDC: enabled=%v keycloak=%q, want true / %q",
			ps.EnableOIDC, ps.KeycloakNodeID, kcID)
	}
	// PMM's own console can sign in through Keycloak too.
	pmm := nodesOfType(doc, "pmm")[0]
	if !pmm.EnableOIDC || pmm.KeycloakNodeID != kcID {
		t.Errorf("pmm OIDC: enabled=%v keycloak=%q", pmm.EnableOIDC, pmm.KeycloakNodeID)
	}
	// And the caller is told what got wired.
	for _, r := range resolved {
		if r.Kind == "ps" && !hasLink(r.Links, "oidc→keycloak-01") {
			t.Errorf("ps links reported as %v", r.Links)
		}
	}

	// Asking for it with no Keycloak in the spec names the line to add.
	_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "ps", OIDC: true},
	}})
	if err == nil {
		t.Fatal("oidc with no keycloak was accepted")
	}
	if !strings.Contains(err.Error(), `{"kind":"keycloak"}`) {
		t.Errorf("the error should name what to add: %v", err)
	}
}

func TestComposeVaultKerberosAndBackups(t *testing.T) {
	composeCatalogFixture(t)

	t.Run("vault keys in OpenBao", func(t *testing.T) {
		doc, _, _, _ := composeBuild(t, composeSpec{Name: "v", Nodes: []composeNodeSpec{
			{Kind: "openbao"},
			{Kind: "ps", Version: "8.0", OS: "el8", Vault: true},
		}})
		var baoID string
		for _, n := range doc.Nodes {
			if n.Type == "openbao" {
				baoID = n.ID
			}
		}
		ps := nodesOfType(doc, "ps")[0]
		if !ps.EnableVault || ps.OpenBaoNodeID != baoID {
			t.Errorf("ps vault: enabled=%v bao=%q, want true / %q",
				ps.EnableVault, ps.OpenBaoNodeID, baoID)
		}
	})

	t.Run("kerberos against a Samba AD DC", func(t *testing.T) {
		doc, _, _, _ := composeBuild(t, composeSpec{Name: "k", Nodes: []composeNodeSpec{
			{Kind: "sambaad"},
			{Kind: "pg", OS: "el9", Kerberos: true},
		}})
		var adID string
		for _, n := range doc.Nodes {
			if n.Type == "sambaad" {
				adID = n.ID
			}
		}
		pg := nodesOfType(doc, "pg")[0]
		if !pg.KerberosAuth || pg.LdapDirNodeID != adID {
			t.Errorf("pg kerberos: enabled=%v dir=%q, want true / %q",
				pg.KerberosAuth, pg.LdapDirNodeID, adID)
		}
		// The Intranet cannot issue tickets, so it must not be picked as the
		// provider even though it is present and is a directory.
		_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: "pg", Kerberos: true},
		}})
		if err == nil {
			t.Error("kerberos with no Samba AD DC was accepted")
		} else if !strings.Contains(err.Error(), "sambaad") {
			t.Errorf("the error should name what to add: %v", err)
		}
	})

	t.Run("backups to SeaweedFS, per engine", func(t *testing.T) {
		// The tool differs — pgBackRest, Barman, PBM — but the S3 target is one field.
		for _, c := range []struct {
			kind  string
			check func(designDoc, string) error
		}{
			{"patroni", func(d designDoc, id string) error {
				f := d.Frames[0]
				if !f.UsePgBackRest || f.SeaweedFSNodeID != id {
					return fmt.Errorf("patroni: pgBackRest=%v s3=%q", f.UsePgBackRest, f.SeaweedFSNodeID)
				}
				return nil
			}},
			{"repmgr", func(d designDoc, id string) error {
				f := d.Frames[0]
				if !f.UseBarman || f.SeaweedFSNodeID != id {
					return fmt.Errorf("repmgr: barman=%v s3=%q", f.UseBarman, f.SeaweedFSNodeID)
				}
				return nil
			}},
			{"psmrs", func(d designDoc, id string) error {
				f := d.Frames[0]
				if !f.EnablePBM || f.SeaweedFSNodeID != id {
					return fmt.Errorf("psmrs: pbm=%v s3=%q", f.EnablePBM, f.SeaweedFSNodeID)
				}
				return nil
			}},
			{"pg", func(d designDoc, id string) error {
				n := nodesOfType(d, "pg")[0]
				if !n.UsePgBackRest || n.SeaweedFSNodeID != id {
					return fmt.Errorf("pg: pgBackRest=%v s3=%q", n.UsePgBackRest, n.SeaweedFSNodeID)
				}
				return nil
			}},
		} {
			doc, _, _, _, err := buildCompose(composeSpec{Name: "b", Nodes: []composeNodeSpec{
				{Kind: "seaweedfs"},
				{Kind: c.kind, OS: "el9", Backup: true},
			}})
			if err != nil {
				// psmrs/repmgr may be absent from the synthetic catalogue.
				if strings.Contains(err.Error(), "installable") || strings.Contains(err.Error(), "no ") {
					continue
				}
				t.Errorf("%s: %v", c.kind, err)
				continue
			}
			var s3 string
			for _, n := range doc.Nodes {
				if n.Type == "seaweedfs" {
					s3 = n.ID
				}
			}
			if err := c.check(doc, s3); err != nil {
				t.Errorf("%s: %v", c.kind, err)
			}
		}
	})

	t.Run("orchestrator on a replication frame", func(t *testing.T) {
		doc, _, _, _ := composeBuild(t, composeSpec{Name: "o", Nodes: []composeNodeSpec{
			{Kind: "orchestrator", OS: "el9"},
			{Kind: "ps-repl", Count: 3, OS: "el8", Orchestrator: true},
		}})
		var orchID string
		for _, n := range doc.Nodes {
			if n.Type == "orchestrator" {
				orchID = n.ID
			}
		}
		if got := doc.Frames[0].OrchestratorNodeID; got != orchID {
			t.Errorf("frame orchestratorNodeId %q, want %q", got, orchID)
		}
		// PXC is not an Orchestrator-managed topology, so it must not accept it.
		_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: "orchestrator"}, {Kind: "pxc", Orchestrator: true},
		}})
		if err == nil {
			t.Error("pxc accepted an orchestrator, which does not manage Galera")
		}
	})
}

// TestComposeLinkTableIsConsistent: every relationship a kind claims must exist, and
// every provider named by a link must be a kind that can actually be built.
func TestComposeLinkTableIsConsistent(t *testing.T) {
	for _, l := range composeLinks {
		if l.Option == "" || l.Apply == nil || l.Missing == "" || len(l.Provides) == 0 {
			t.Errorf("incomplete link %q", l.Option)
		}
		for _, p := range l.Provides {
			if _, ok := composeKindByName(p); !ok {
				t.Errorf("link %q is provided by %q, which is not a buildable kind", l.Option, p)
			}
		}
	}
	for _, k := range composeKinds {
		for _, name := range k.Links {
			if _, ok := composeLinkByOption(name); !ok {
				t.Errorf("%s claims relationship %q, which does not exist", k.Kind, name)
			}
		}
	}
	// Every relationship is claimed by at least one kind, or it is unreachable —
	// which is exactly the state "oidc" was in before this table.
	for _, l := range composeLinks {
		used := false
		for _, k := range composeKinds {
			if k.supports(l.Option) {
				used = true
			}
		}
		if !used {
			t.Errorf("no kind supports %q, so nothing can ever ask for it", l.Option)
		}
	}
}

// TestComposePinnedOS: a kind that works on only one OS takes it, and refuses another
// rather than composing a design the validator will immediately reject. OpenBao
// installs from EPEL, which is wired up for Oracle Linux 9 alone.
func TestComposePinnedOS(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "v", Nodes: []composeNodeSpec{{Kind: "openbao"}}})
	bao := nodesOfType(doc, "openbao")[0]
	if bao.OS != "oraclelinux" || bao.OSVersion != "9" {
		t.Errorf("openbao landed on %s %s, want oraclelinux 9", bao.OS, bao.OSVersion)
	}
	_, _, _, _, err := buildCompose(composeSpec{Name: "v", Nodes: []composeNodeSpec{
		{Kind: "openbao", OS: "ubuntu24.04"},
	}})
	if err == nil {
		t.Error("openbao on Ubuntu was accepted")
	} else if !strings.Contains(err.Error(), "oraclelinux 9") {
		t.Errorf("the error should name the one OS it works on: %v", err)
	}
}

// TestComposeSingletons: the validator allows one of each of these per stack, so
// composing two is refused here rather than at deploy time.
func TestComposeSingletons(t *testing.T) {
	composeCatalogFixture(t)
	for _, kind := range []string{"keycloak", "openbao", "vnc", "watchtower"} {
		_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: kind}, {Kind: kind},
		}})
		if err == nil {
			t.Errorf("two %s nodes were accepted", kind)
		} else if !strings.Contains(err.Error(), "only one per stack") {
			t.Errorf("%s: %v", kind, err)
		}
	}
	// An explicit Intranet plus the automatic one must not double up either.
	doc, _, _, added := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "intranet"}, {Kind: "ps", OS: "el9"},
	}})
	if n := len(nodesOfType(doc, "intranet")); n != 1 {
		t.Errorf("got %d intranet nodes, want 1", n)
	}
	if len(added) != 0 {
		t.Errorf("an Intranet was added despite one being asked for: %v", added)
	}
}

// TestComposeOIDCTurnsOnTheIssuerCertificate: an OIDC issuer must be reachable over
// HTTPS, and the validator refuses the alternative — so compose sets it rather than
// handing back an error whose fix is mechanical.
func TestComposeOIDCTurnsOnTheIssuerCertificate(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, added := composeBuild(t, composeSpec{Name: "sso", Nodes: []composeNodeSpec{
		{Kind: "keycloak"},
		{Kind: "ps", OS: "el8", OIDC: true},
	}})
	kc := nodesOfType(doc, "keycloak")[0]
	if !kc.GenerateCert {
		t.Error("the Keycloak node has no certificate, so it cannot be an HTTPS issuer")
	}
	found := false
	for _, a := range added {
		if strings.Contains(a, "HTTPS") {
			found = true
		}
	}
	if !found {
		t.Errorf("turning the certificate on was not reported: %v", added)
	}

	// Without OIDC it is left alone — a certificate is not free.
	doc2, _, _, _ := composeBuild(t, composeSpec{Name: "plain", Nodes: []composeNodeSpec{
		{Kind: "keycloak"},
	}})
	if nodesOfType(doc2, "keycloak")[0].GenerateCert {
		t.Error("a certificate was turned on with nothing using Keycloak as an issuer")
	}
}

// TestComposeDrawsAssociations is the regression for the largest hole compose had:
// it wrote every relationship that is a FIELD and none that is a LINE, so a ProxySQL
// composed with a cluster right beside it had no backend, and an HAProxy could not
// pass the validator at all. The provisioners resolve these by walking the edge
// graph, so an empty Edges list means the node does nothing.
func TestComposeDrawsAssociations(t *testing.T) {
	composeCatalogFixture(t)
	doc, pos, res, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", Count: 3, OS: "el9"},
		{Kind: "haproxy", OS: "el9"},
		// Through the proxy rather than direct at the cluster — a real choice, which
		// is why compose refuses to guess when both are present.
		{Kind: "marketchaos", To: "haproxy-01"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Edges) != 2 {
		t.Fatalf("drew %d association lines, want 2", len(doc.Edges))
	}
	// The endpoint must be the FRAME, not a member: one line covers the cluster, and
	// haproxyClusterFrames looks the id up in the frame map.
	frame := doc.Frames[0].ID
	if doc.Edges[0].From.Node != frame {
		t.Errorf("the HAProxy edge starts at %q, want the frame %q", doc.Edges[0].From.Node, frame)
	}
	for _, e := range doc.Edges {
		if e.ID == "" || e.Type != "directional" {
			t.Errorf("edge %+v is missing its id or type", e)
		}
	}
	// And the plan says so, because an association the caller cannot see is one they
	// cannot correct.
	var wired int
	for _, r := range res {
		for _, l := range r.Links {
			if strings.HasPrefix(l, "to→") {
				wired++
			}
		}
	}
	if wired != 2 {
		t.Errorf("the plan reports %d associations, want 2", wired)
	}

	// And they survive serialisation. designJSON wrote a hardcoded `[]designEdge{}`
	// from back when compose drew no associations, so the plan reported every link it
	// had made and the saved design contained none of them — which is invisible to
	// any assertion on the in-memory document.
	raw, err := designJSON(doc, pos)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Edges []designEdge `json:"edges"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Edges) != len(doc.Edges) {
		t.Errorf("the saved design has %d edges, the composed one had %d",
			len(saved.Edges), len(doc.Edges))
	}

	// The real walkers agree — the point is not that an edge exists but that the
	// provisioner finds its target through it.
	if got := haproxyClusterFrames(doc, nodesOfType(doc, "haproxy")[0].ID); len(got) != 1 {
		t.Errorf("haproxyClusterFrames found %d backends, want 1", len(got))
	}
	if _, _, ok := marketChaosTarget(doc, nodesOfType(doc, "marketchaos")[0].ID); !ok {
		t.Error("marketChaosTarget found nothing to drive")
	}
}

// TestComposeShardedMongoTopology: the one frame whose members are not N of the same
// thing. The shape is fixed by validateStack, so compose has to emit exactly it.
func TestComposeShardedMongoTopology(t *testing.T) {
	composeCatalogFixture(t)
	for _, c := range []struct {
		setup             string
		nodes, cfg, perRS int
	}{
		{"", 13, 3, 3},
		{"standard", 13, 3, 3},
		{"minimum", 5, 1, 1},
	} {
		doc, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
			{Kind: "psmdb", Setup: c.setup, OS: "el9"},
		}})
		if err != nil {
			t.Errorf("setup=%q: %v", c.setup, err)
			continue
		}
		members := nodesOfType(doc, "psmdb")
		if len(members) != c.nodes {
			t.Errorf("setup=%q: %d members, want %d", c.setup, len(members), c.nodes)
		}
		roles, shards := map[string]int{}, map[int]int{}
		labels := map[string]bool{}
		for _, n := range members {
			roles[n.Role]++
			if n.Role == "shard" {
				shards[n.Shard]++
			}
			if labels[n.Label] {
				t.Errorf("setup=%q: duplicate hostname %q", c.setup, n.Label)
			}
			labels[n.Label] = true
		}
		if roles["mongos"] != 1 || roles["config"] != c.cfg || len(shards) != 3 {
			t.Errorf("setup=%q: %d mongos, %d config, %d shards; want 1, %d, 3",
				c.setup, roles["mongos"], roles["config"], len(shards), c.cfg)
		}
		for sh, n := range shards {
			if n != c.perRS {
				t.Errorf("setup=%q: shard %d has %d members, want %d", c.setup, sh, n, c.perRS)
			}
		}
	}
}

// TestComposeShapesResources: the netem and blkio limits reach the node, and the
// engine's own reader agrees they are shaped.
func TestComposeShapesResources(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pxc", Count: 3, OS: "el9",
			NetLatencyMS: 40, NetJitterMS: 5, NetLossPct: 0.5, NetRateMbit: 100,
			DeviceReadMBps: 50, DeviceWriteMBps: 25},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodesOfType(doc, "pxc") {
		spec := nodeNetemSpec(n)
		if spec.LatencyMS != 40 || spec.JitterMS != 5 || spec.LossPct != 0.5 || spec.RateMbit != 100 {
			t.Errorf("%s: netem.go reads back %+v", n.Label, spec)
		}
		if len(spec.Ports) == 0 {
			t.Errorf("%s: shaping applies to no ports", n.Label)
		}
		if n.DeviceReadMBps != 50 || n.DeviceWriteMBps != 25 {
			t.Errorf("%s: blkio limits not applied", n.Label)
		}
	}
}

// TestComposeKeycloakAddsDesktop: Keycloak cannot deploy without one, so composing it
// alone has to produce a design that validates rather than an error to go and fix.
func TestComposeKeycloakAddsDesktop(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, added, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "keycloak"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodesOfType(doc, "vnc")) != 1 {
		t.Fatalf("built %d desktops, want 1", len(nodesOfType(doc, "vnc")))
	}
	if !strings.Contains(strings.Join(added, " "), "vnc") {
		t.Errorf("the desktop was added without saying so: %v", added)
	}
	// An explicit one is not doubled.
	doc, _, _, _, err = buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "keycloak"}, {Kind: "vnc"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(nodesOfType(doc, "vnc")); n != 1 {
		t.Errorf("built %d desktops for an explicit one, want 1", n)
	}
}

// TestComposeRefusesSilentOIDC: below 8.4.11-11 the plugin does not exist and the
// provisioner skips it without failing, so the flag would be accepted everywhere and
// do nothing.
func TestComposeRefusesSilentOIDC(t *testing.T) {
	composeCatalogFixture(t)
	_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "keycloak"}, {Kind: "ps", Version: "8.0.45", OS: "el9", OIDC: true},
	}})
	if err == nil || !strings.Contains(err.Error(), mysqlOIDCMinVersion) {
		t.Errorf("8.0 with oidc: got %v, want a refusal naming %s", err, mysqlOIDCMinVersion)
	}
}

// TestComposeCatalogueMatchesTheBuilder: `stack kinds` is where people and the docs
// find out what a kind takes, so an option it advertises but the builder refuses —
// or one the builder accepts and it hides — is a lie in the one place nobody would
// think to check. It drifted exactly that way once: the catalogue kept offering cpus
// and memoryGb on the nine types that silently ignore them.
func TestComposeCatalogueMatchesTheBuilder(t *testing.T) {
	composeCatalogFixture(t)
	var doc struct {
		Kinds []struct {
			Kind    string   `json:"kind"`
			Members string   `json:"members"`
			Options []string `json:"options"`
			Needs   []string `json:"needs"`
		} `json:"kinds"`
	}
	// The document the handler serves, held against the table it describes. Going
	// through HTTP would only add an auth stub to the same call.
	raw, err := json.Marshal(composeKindsDoc())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Kinds) != len(composeKinds) {
		t.Fatalf("the catalogue lists %d kinds, the table has %d", len(doc.Kinds), len(composeKinds))
	}
	for i, k := range composeKinds {
		d := doc.Kinds[i]
		if d.Kind != k.Kind {
			t.Fatalf("catalogue entry %d is %q, table has %q", i, d.Kind, k.Kind)
		}
		has := func(o string) bool { return slices.Contains(d.Options, o) }
		if has("cpus") == k.NoSizing {
			t.Errorf("%s: NoSizing=%v but cpus advertised=%v", k.Kind, k.NoSizing, has("cpus"))
		}
		if has("netLatencyMs") != k.CanShape {
			t.Errorf("%s: CanShape=%v but netLatencyMs advertised=%v", k.Kind, k.CanShape, has("netLatencyMs"))
		}
		if has("to") != (len(k.EdgeTo) > 0) {
			t.Errorf("%s: EdgeTo=%v but \"to\" advertised=%v", k.Kind, k.EdgeTo, has("to"))
		}
		if has("count") != (k.Frame && k.Topology == nil) {
			t.Errorf("%s: count advertised=%v, want %v", k.Kind, has("count"), k.Frame && k.Topology == nil)
		}
		if has("version") != (k.Catalog != "" || k.PDPSRepo) {
			t.Errorf("%s: version advertised=%v but it has no catalogue", k.Kind, has("version"))
		}
		for _, sc := range k.Scalars {
			if !has(sc) {
				t.Errorf("%s accepts %q and the catalogue does not list it", k.Kind, sc)
			}
		}
		if k.Topology != nil && !strings.Contains(d.Members, "fixed") {
			t.Errorf("%s: members reads %q, want a fixed topology", k.Kind, d.Members)
		}
	}
}
