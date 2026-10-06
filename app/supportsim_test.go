package main

import (
	"strings"
	"testing"
)

func supportDoc(target designFrame, node *designNode) designDoc {
	doc := designDoc{Nodes: []designNode{{ID: "sim", Type: "supportsim", Label: "supportsim-01"}}}
	other := target.ID
	if node != nil {
		doc.Nodes = append(doc.Nodes, *node)
		other = node.ID
	} else {
		doc.Frames = []designFrame{target}
	}
	doc.Edges = []designEdge{{ID: "e"}}
	doc.Edges[0].From.Node, doc.Edges[0].To.Node = other, "sim"
	return doc
}

func TestSupportSimIssues(t *testing.T) {
	// A replica set with vector search: clean.
	rs := designFrame{ID: "f", Type: "psmrs", Label: "rs", VectorSearch: true}
	if iss := supportSimIssues(supportDoc(rs, nil), designNode{ID: "sim", Label: "supportsim-01"}); len(iss) != 0 {
		t.Errorf("rs with search: %v", iss)
	}
	// Without it: a warning that names the fix, not an error — the sim still runs.
	rs.VectorSearch = false
	iss := supportSimIssues(supportDoc(rs, nil), designNode{ID: "sim", Label: "supportsim-01"})
	if len(iss) != 1 || iss[0].Level != "warning" || !strings.Contains(iss[0].Message, "tick Vector search") {
		t.Errorf("rs without search: %v", iss)
	}
	// A K3D frame running the wrong operator: an error.
	k := designFrame{ID: "f", Type: "k3d", Label: "k", K3DOperator: "pxc"}
	if iss := supportSimIssues(supportDoc(k, nil), designNode{ID: "sim", Label: "supportsim-01"}); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("k3d pxc: %v", iss)
	}
	// Unlinked: an error.
	if iss := supportSimIssues(designDoc{}, designNode{ID: "sim", Label: "supportsim-01"}); len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("unlinked: %v", iss)
	}
	// A standalone node links, with the no-mongot warning.
	psm := designNode{ID: "p", Type: "psm", Label: "psm-1"}
	if iss := supportSimIssues(supportDoc(designFrame{}, &psm), designNode{ID: "sim", Label: "supportsim-01"}); len(iss) != 1 || !strings.Contains(iss[0].Message, "one member is enough") {
		t.Errorf("psm: %v", iss)
	}
}

// The metrics endpoints are the members mongot was placed on: one per replica set, one
// per shard, by label.
func TestSupportSimMongotMetrics(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "b", Type: "psmrs", FrameID: "f", Label: "rs-2"}, {ID: "a", Type: "psmrs", FrameID: "f", Label: "rs-1"},
	}}
	hosts := map[string]string{"a": "rs-1", "b": "rs-2"}
	f := designFrame{ID: "f", Type: "psmrs", VectorSearch: true}
	if got := supportSimMongotMetrics(doc, f, "psmrs", "example.net", hosts); got != "rs-1.example.net:9946" {
		t.Errorf("replica set: %q", got)
	}
	sh := designDoc{Nodes: []designNode{
		{ID: "c", Type: "psmdb", FrameID: "s", Role: "config", Label: "cfg1"},
		{ID: "m", Type: "psmdb", FrameID: "s", Role: "mongos", Label: "mongos"},
		{ID: "s1", Type: "psmdb", FrameID: "s", Role: "shard", Shard: 1, Label: "s1r1"},
		{ID: "s0", Type: "psmdb", FrameID: "s", Role: "shard", Shard: 0, Label: "s0r1"},
	}}
	hs := map[string]string{"s0": "s0r1", "s1": "s1r1"}
	if got := supportSimMongotMetrics(sh, designFrame{ID: "s", Type: "psmdb", VectorSearch: true}, "psmdb", "example.net", hs); got != "s0r1.example.net:9946,s1r1.example.net:9946" {
		t.Errorf("sharded: %q", got)
	}
	f.VectorSearch = false
	if got := supportSimMongotMetrics(doc, f, "psmrs", "example.net", hosts); got != "" {
		t.Errorf("no search, no endpoints: %q", got)
	}
}

func TestSupportSimK3DExposeWarning(t *testing.T) {
	k := designFrame{ID: "f", Type: "k3d", Label: "k3d-00", K3DOperator: "psmdb", K3DVectorSearch: true, K3DExposeReplset: "clusterip"}
	iss := supportSimIssues(supportDoc(k, nil), designNode{ID: "sim", Label: "supportsim-01"})
	if len(iss) != 1 || iss[0].Level != "warning" {
		t.Fatalf("%v", iss)
	}
	m := iss[0].Message
	for _, want := range []string{"the replica set's pods are exposed only as ClusterIP", "deploy would fail", "Set Expose · replica set to LoadBalancer"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in %q", want, m)
		}
	}
	k.K3DExposeReplset = "loadbalancer"
	if iss := supportSimIssues(supportDoc(k, nil), designNode{ID: "sim", Label: "supportsim-01"}); len(iss) != 0 {
		t.Errorf("LoadBalancer with search on is the supported shape: %v", iss)
	}
	if got := tierVerb([]string{"the mongos routers"}); got != "are" {
		t.Errorf("tierVerb(mongos routers) = %q", got)
	}
	if got := tierVerb([]string{"HAProxy"}); got != "is" {
		t.Errorf("tierVerb(HAProxy) = %q", got)
	}
}
