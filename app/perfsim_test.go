package main

import (
	"strings"
	"testing"
)

func perfSimDoc(edges ...designEdge) designDoc {
	return designDoc{
		Nodes: []designNode{
			{ID: "sim", Type: "perfsim", Label: "perfsim-01"},
			{ID: "ps1", Type: "ps", Label: "ps-01"},
			{ID: "g1", Type: "pxc", Label: "pxc-01", FrameID: "fpxc"},
			{ID: "g3", Type: "pxc", Label: "pxc-03", FrameID: "fpxc"},
			{ID: "g2", Type: "pxc", Label: "pxc-02", FrameID: "fpxc"},
			{ID: "arb", Type: "pxc", Label: "garbd-01", FrameID: "fpxc", Role: "arbitrator"},
			{ID: "pg1", Type: "pg", Label: "pg-01"},
			{ID: "sp1", Type: "spock", Label: "spock-01", FrameID: "fspock"},
			{ID: "hap", Type: "haproxy", Label: "haproxy-01"},
		},
		Frames: []designFrame{{ID: "fpxc", Type: "pxc", Label: "pxc-cluster"}, {ID: "fspock", Type: "spock", Label: "spock-cluster"}},
		Edges:  edges,
	}
}

func line(a, b string) designEdge {
	return designEdge{ID: a + b, From: edgeEnd{Node: a}, To: edgeEnd{Node: b}, Type: "directional"}
}

// A cluster target gives every data member in label order, without the arbitrator;
// a standalone server gives itself; a router gives the cluster behind it.
func TestPerfSimMembers(t *testing.T) {
	ids := func(doc designDoc) string {
		kind, target, ok := perfSimLink(doc, "sim")
		if !ok {
			return "not linked"
		}
		var out []string
		for _, n := range perfSimMembers(doc, kind, target, nil, "example.net") {
			out = append(out, n.Label)
		}
		return kind + ":" + strings.Join(out, ",")
	}
	for name, c := range map[string]struct {
		doc  designDoc
		want string
	}{
		"cluster":    {perfSimDoc(line("fpxc", "sim")), "pxc:pxc-01,pxc-02,pxc-03"},
		"standalone": {perfSimDoc(line("ps1", "sim")), "ps:ps-01"},
		"router":     {perfSimDoc(line("fpxc", "hap"), line("hap", "sim")), "haproxy:pxc-01,pxc-02,pxc-03"},
		"postgres":   {perfSimDoc(line("pg1", "sim")), "pg:pg-01"},
		"spock":      {perfSimDoc(line("fspock", "sim")), "not linked"},
	} {
		if got := ids(c.doc); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

func TestPerfSimIssues(t *testing.T) {
	sim := designNode{ID: "sim", Type: "perfsim", Label: "perfsim-01"}
	for name, c := range map[string]struct {
		doc  designDoc
		want string // a substring of the one error, or "" for none
	}{
		"unlinked":        {perfSimDoc(), "needs an association line"},
		"postgres":        {perfSimDoc(line("pg1", "sim")), ""},
		"spock":           {perfSimDoc(line("fspock", "sim")), "cannot drive yet"},
		"router no back":  {perfSimDoc(line("hap", "sim")), "does not front"},
		"cluster":         {perfSimDoc(line("fpxc", "sim")), ""},
		"router to a pxc": {perfSimDoc(line("fpxc", "hap"), line("hap", "sim")), ""},
	} {
		got := perfSimIssues(c.doc, sim)
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected %v", name, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0].Message, c.want)):
			t.Errorf("%s: %v, want one error mentioning %q", name, got, c.want)
		}
	}
}
