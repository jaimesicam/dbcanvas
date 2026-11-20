package main

import (
	"strings"
	"testing"
)

// PostgreSQL 19 is offered for Percona and PGDG alike, every Percona component is packaged
// for it, and repmgr — which PGDG has not built for 19 — refuses it.

func TestPPGComponentsCover19(t *testing.T) {
	for _, c := range ppgComponents {
		if ok, why := c.Available("oraclelinux", "19", ""); !ok {
			t.Errorf("%s on PostgreSQL 19: %s", c.ID, why)
		}
	}
}

func TestLinuxClientPsql19(t *testing.T) {
	if s := lcPsqlSeries("oraclelinux"); s[len(s)-1] != "19" {
		t.Fatalf("psql series = %v, want it to end at 19", s)
	}
}

func TestRepmgrMajorIssues(t *testing.T) {
	for _, m := range []string{"", "13", "18"} {
		if !repmgrMajorSupported(m) {
			t.Errorf("repmgrMajorSupported(%q) = false, want true", m)
		}
	}
	if repmgrMajorSupported("19") {
		t.Fatal("repmgrMajorSupported(\"19\") = true, want false")
	}
	doc := designDoc{
		Frames: []designFrame{
			{ID: "f1", Type: "repmgr", Label: "repmgr-ok", PGMajor: "18"},
			{ID: "f2", Type: "repmgr", Label: "repmgr-19", PGMajor: "19"},
			{ID: "f3", Type: "patroni", Label: "patroni-19", PGMajor: "19"},
		},
		Nodes: []designNode{{ID: "a", Type: "aio", Label: "aio-01", AIOInstances: []aioInstance{
			{Kind: "repmgr", Name: "rm01", PGMajor: "19"},
			{Kind: "patroni", Name: "pt01", PGMajor: "19"},
		}}},
	}
	got := repmgrMajorIssues(doc)
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "repmgr-19") || !strings.Contains(got[1].Message, "rm01") {
		t.Fatalf("issues name the wrong objects: %+v", got)
	}
}
