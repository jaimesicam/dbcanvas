package main

import (
	"strings"
	"testing"
)

// standbyDoc is two three-member Patroni clusters, src-cluster and dst-cluster, with a link
// of the given type from the first to the second.
func standbyDoc(linkType string) designDoc {
	doc := designDoc{
		Frames: []designFrame{
			{ID: "f-src", Type: "patroni", Label: "src-cluster", PGMajor: "17"},
			{ID: "f-dst", Type: "patroni", Label: "dst-cluster", PGMajor: "17"},
		},
	}
	for _, f := range []struct{ id, stem string }{{"f-src", "src"}, {"f-dst", "dst"}} {
		for i := 1; i <= 3; i++ {
			id := f.stem + "-" + string(rune('0'+i))
			doc.Nodes = append(doc.Nodes, designNode{ID: id, Type: "patroni", FrameID: f.id, Label: id})
		}
	}
	if linkType != "" {
		doc.Edges = []designEdge{{ID: "e1", From: edgeEnd{Node: "f-src"}, To: edgeEnd{Node: "f-dst"}, Type: linkType}}
	}
	return doc
}

func TestPatroniStandbyForListsEverySourceMember(t *testing.T) {
	doc := standbyDoc("async")
	s := patroniStandbyFor(doc, "f-dst", "example.net")
	if s == nil {
		t.Fatal("dst-cluster is not a standby")
	}
	if s.SourceID != "f-src" || s.SourceLabel != "src-cluster" {
		t.Errorf("source = %s/%s", s.SourceID, s.SourceLabel)
	}
	if got := s.hostList(); got != "src-1.example.net,src-2.example.net,src-3.example.net" {
		t.Errorf("hosts = %q", got)
	}
	if patroniStandbyFor(doc, "f-src", "example.net") != nil {
		t.Error("the source is reported as a standby")
	}
	if patroniStandbyFor(standbyDoc(""), "f-dst", "example.net") != nil {
		t.Error("a cluster with no link is reported as a standby")
	}
}

func TestPatroniYAMLStandbyCluster(t *testing.T) {
	doc := standbyDoc("async")
	f := doc.Frames[1]
	s := patroniStandbyFor(doc, f.ID, "example.net")
	y := patroniYAML(f, "dst-1", "dst-1.example.net", []string{"dst-1.example.net:2379"}, pgFamilySecrets(), s)
	for _, want := range []string{
		"  dcs:\n",
		"    standby_cluster:\n      host: src-1.example.net,src-2.example.net,src-3.example.net\n      port: 5432\n      create_replica_methods:\n      - basebackup\n",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("patroni.yml lacks %q:\n%s", want, y)
		}
	}
	// It belongs to bootstrap.dcs, before the dcs postgresql section.
	if strings.Index(y, "standby_cluster:") > strings.Index(y, "    postgresql:\n") {
		t.Error("standby_cluster is not under bootstrap.dcs")
	}
	if strings.Contains(patroniYAML(f, "dst-1", "dst-1.example.net", nil, pgFamilySecrets(), nil), "standby_cluster") {
		t.Error("a cluster that is not a standby has standby_cluster")
	}
}

func TestPatroniStandbyIssues(t *testing.T) {
	errs := func(doc designDoc) []string {
		var out []string
		seen := map[string]bool{}
		for _, e := range doc.Edges {
			if src, dst, ok := patroniStandbyEdgeFrames(doc, e); ok {
				for _, is := range patroniStandbyIssues(doc, src, dst, e, seen) {
					if is.Level == "error" {
						out = append(out, is.Message)
					}
				}
			}
		}
		return out
	}

	if got := errs(standbyDoc("async")); len(got) != 0 {
		t.Errorf("a plain standby link has errors: %v", got)
	}

	cases := []struct {
		name string
		edit func(*designDoc)
		want string
	}{
		{"bidirectional", func(d *designDoc) { d.Edges[0].Type = "bidir" }, "must be one way"},
		{"major", func(d *designDoc) { d.Frames[1].PGMajor = "16" }, "same major version"},
		{"build", func(d *designDoc) { d.Frames[1].Distribution = distPGDG }, "different PostgreSQL builds"},
		{"pgbackrest", func(d *designDoc) { d.Frames[1].UsePgBackRest = true }, "cannot use pgBackRest"},
		{"pg_tde", func(d *designDoc) { d.Frames[0].EnableVault = true }, "pg_tde"},
		{"loop", func(d *designDoc) {
			d.Edges = append(d.Edges, designEdge{ID: "e2", From: edgeEnd{Node: "f-dst"}, To: edgeEnd{Node: "f-src"}, Type: "async"})
		}, "loop"},
		{"two sources", func(d *designDoc) {
			d.Frames = append(d.Frames, designFrame{ID: "f-third", Type: "patroni", Label: "third", PGMajor: "17"})
			d.Edges = append(d.Edges, designEdge{ID: "e2", From: edgeEnd{Node: "f-third"}, To: edgeEnd{Node: "f-dst"}, Type: "async"})
		}, "more than one link"},
	}
	for _, c := range cases {
		doc := standbyDoc("async")
		c.edit(&doc)
		got := strings.Join(errs(doc), " | ")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: errors %q do not mention %q", c.name, got, c.want)
		}
	}

	// A chain is a cascade, not a loop.
	doc := standbyDoc("async")
	doc.Frames = append(doc.Frames, designFrame{ID: "f-third", Type: "patroni", Label: "third", PGMajor: "17"})
	doc.Edges = append(doc.Edges, designEdge{ID: "e2", From: edgeEnd{Node: "f-dst"}, To: edgeEnd{Node: "f-third"}, Type: "async"})
	if got := errs(doc); len(got) != 0 {
		t.Errorf("a chain of standbys has errors: %v", got)
	}
}

func TestPatroniStandbyEdgeIsNotAK3DOrMemberLink(t *testing.T) {
	doc := standbyDoc("async")
	if _, ok := k3dReplEdgeFrames(doc, doc.Edges[0]); ok {
		t.Error("a Patroni standby link resolves as a Kubernetes replication link")
	}
	if len(replicationLinks(doc)) != 0 {
		t.Error("a Patroni standby link resolves as a MySQL replication channel")
	}
}

func TestComposeStandbyOf(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, res, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "patroni", Name: "east"},
		{Kind: "patroni", Name: "west", StandbyOf: "east"},
	}})
	var east, west composeResolved
	for _, r := range res {
		switch r.Name {
		case "east":
			east = r
		case "west":
			west = r
		}
	}
	found := false
	for _, e := range doc.Edges {
		if e.Type == "async" && e.From.Node == east.FrameID && e.To.Node == west.FrameID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no async link from east to west: %+v", doc.Edges)
	}
	if !hasLink(west.Links, "standbyOf→east") {
		t.Errorf("west's links = %v", west.Links)
	}
	if s := patroniStandbyFor(doc, west.FrameID, "example.net"); s == nil || s.SourceLabel != "east" {
		t.Errorf("west is not a standby of east: %+v", s)
	}

	_, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "patroni", Name: "east"},
		{Kind: "patroni", Name: "west", StandbyOf: "north"},
	}})
	if err == nil || !strings.Contains(err.Error(), "east") {
		t.Errorf("an unknown standbyOf should name the clusters there are: %v", err)
	}
	_, _, _, _, err = buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "pg", Name: "solo", StandbyOf: "east"},
		{Kind: "patroni", Name: "east"},
	}})
	if err == nil {
		t.Error("standbyOf on a standalone PostgreSQL node was accepted")
	}
}
