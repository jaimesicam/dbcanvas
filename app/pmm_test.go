package main

import (
	"strings"
	"testing"
)

// TestPMMPolicyPassword: the PMM admin password meets PMM 3's policy (12+ characters, upper,
// lower, digit, symbol) without losing what it was: a generated one gets a symbol only.
func TestPMMPolicyPassword(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Ab3defGhijkLmnoPqrsT", "Ab3defGhijkLmnoPqrsT_"},
		{"admin_password", "admin_passwordA1"},
		{"Already_Ok123", "Already_Ok123"},
		{"aB1", "aB1_00000000"},
	} {
		if got := pmmPolicyPassword(c.in); got != c.want {
			t.Errorf("pmmPolicyPassword(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPMMWatchedProxySQL: the ProxySQL nodes a PMM node registers when it comes up are the
// standalone ones that name it and the members of a ProxySQL cluster that names it, each with its
// OS; ProxySQL watched by another PMM node, or none, is left out.
func TestPMMWatchedProxySQL(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f1", Type: "proxysql", PMMNodeID: "pmm-a", OS: "ubuntu"}},
		Nodes: []designNode{
			{ID: "p1", Type: "proxysql", PMMNodeID: "pmm-a", OS: "oraclelinux"},
			{ID: "p2", Type: "proxysql", PMMNodeID: "pmm-b", OS: "oraclelinux"},
			{ID: "p3", Type: "proxysql"},
			{ID: "m1", Type: "proxysql", FrameID: "f1"},
			{ID: "x", Type: "pxc", PMMNodeID: "pmm-a"},
		},
	}
	var got []string
	for _, w := range pmmWatchedProxySQL(doc, "pmm-a") {
		got = append(got, w.node.ID+"/"+w.os)
	}
	if strings.Join(got, ",") != "p1/oraclelinux,m1/ubuntu" {
		t.Fatalf("watched = %v", got)
	}
}
