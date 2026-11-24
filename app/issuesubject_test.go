package main

import "testing"

func TestIssueSubjects(t *testing.T) {
	doc := designDoc{
		Nodes: []designNode{
			{ID: "n1", Label: "ps-repl-1"}, {ID: "n2", Label: "ps-repl-10"}, {ID: "n3", Label: "ps84"},
			{ID: "d1", Label: "twin"}, {ID: "d2", Label: "twin"},
		},
		Frames: []designFrame{{ID: "f1", Label: "ps-repl-cluster-01"}},
	}
	got := issueSubjects(doc, []issue{
		{Message: "Percona Server node ps84: binary log encryption needs 8.0.14"},
		{Message: "MySQL replication cluster ps-repl-cluster-01 needs at least one secondary"},
		{Message: "Node ps-repl-10 is set to Debian"},
		{Message: "Node ps-repl-1."},
		{Message: "Stack has no nodes to deploy"},
		{Message: "Node twin is wrong"},
		{Message: "Missing image dbcanvas/ps84-base"},
		{Message: "already attributed ps84", Subject: "keep"},
	})
	want := []string{"n3", "f1", "n2", "n1", "", "", "", "keep"}
	for i, w := range want {
		if got[i].Subject != w {
			t.Errorf("%q → %q, want %q", got[i].Message, got[i].Subject, w)
		}
	}
}
