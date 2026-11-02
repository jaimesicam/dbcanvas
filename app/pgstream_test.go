package main

import (
	"strings"
	"testing"
)

func pgStreamDoc(members int, f designFrame) designDoc {
	f.ID, f.Type = "f1", "pgstream"
	if f.Label == "" {
		f.Label = "pg-stream-01"
	}
	doc := designDoc{Frames: []designFrame{f}}
	for i := 0; i < members; i++ {
		doc.Nodes = append(doc.Nodes, designNode{ID: "n" + string(rune('1'+i)), Type: "pgstream", FrameID: "f1", Label: "pgstream0" + string(rune('1'+i))})
	}
	return doc
}

func TestPGStreamSlotName(t *testing.T) {
	cases := map[string]string{
		"pgstream02":            "pgstream02",
		"PG-Stream.02":          "pg_stream_02",
		"":                      "standby",
		strings.Repeat("a", 70): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := pgStreamSlotName(in); got != want {
			t.Errorf("pgStreamSlotName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pgStreamSlot(designFrame{PGNoSlots: true}, "pgstream02"); got != "" {
		t.Errorf("slots off: got %q, want none", got)
	}
}

func TestPGStreamSyncNames(t *testing.T) {
	standbys := []string{"pgstream02", "pgstream03"}
	if got := pgStreamSyncNames(designFrame{}, standbys); got != "" {
		t.Errorf("async: got %q", got)
	}
	if got, want := pgStreamSyncNames(designFrame{PGSyncStandbys: 1}, standbys), `ANY 1 ("pgstream02", "pgstream03")`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// More than there are: capped, never a list that can never be satisfied.
	if got, want := pgStreamSyncNames(designFrame{PGSyncStandbys: 5}, standbys), `ANY 2 ("pgstream02", "pgstream03")`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := pgStreamSyncNames(designFrame{PGSyncStandbys: 1}, nil); got != "" {
		t.Errorf("no standbys: got %q", got)
	}
	if got := pgStreamSyncCommit(designFrame{}); got != "on" {
		t.Errorf("default synchronous_commit = %q, want on", got)
	}
}

func TestPGStreamFrameIssues(t *testing.T) {
	errs := func(is []issue) []string {
		var out []string
		for _, i := range is {
			if i.Level == "error" {
				out = append(out, i.Message)
			}
		}
		return out
	}
	check := func(name string, members int, f designFrame, wantErr string) {
		t.Helper()
		doc := pgStreamDoc(members, f)
		got := errs(pgStreamFrameIssues(doc.Frames[0], doc, map[int][]string{}))
		if wantErr == "" {
			if len(got) > 0 {
				t.Errorf("%s: unexpected errors %v", name, got)
			}
			return
		}
		for _, m := range got {
			if strings.Contains(m, wantErr) {
				return
			}
		}
		t.Errorf("%s: want an error containing %q, got %v", name, wantErr, got)
	}
	check("three members, async", 3, designFrame{}, "")
	check("one member", 1, designFrame{}, "at least 2 nodes")
	check("ten members", 10, designFrame{}, "at most 9 nodes")
	check("sync, one of two", 3, designFrame{PGSyncStandbys: 1, PGSyncCommit: "remote_apply"}, "")
	check("sync more than standbys", 3, designFrame{PGSyncStandbys: 3}, "every commit would hang")
	check("bad commit level", 3, designFrame{PGSyncStandbys: 1, PGSyncCommit: "local"}, "unknown synchronous_commit")
	check("monitor without PMM is fine", 3, designFrame{PGQuerySource: "pgstatmonitor"}, "")

	doc := pgStreamDoc(3, designFrame{PGSyncStandbys: 2})
	var warned bool
	for _, i := range pgStreamFrameIssues(doc.Frames[0], doc, map[int][]string{}) {
		warned = warned || (i.Level == "warning" && strings.Contains(i.Message, "every standby"))
	}
	if !warned {
		t.Error("waiting on every standby should warn")
	}
}

func TestPGStreamMemberConfig(t *testing.T) {
	doc := pgStreamDoc(3, designFrame{OS: "oraclelinux", OSVersion: "9", PGMajor: "17", PGSyncStandbys: 1})
	members := pgStreamMembers(doc.Frames[0], doc)
	if len(members) != 3 || members[0].Label != "pgstream01" {
		t.Fatalf("members out of order: %+v", members)
	}
	p := pgStreamMemberConfig(doc.Frames[0], doc, members, 0, "primary", "")
	if p.SlotName != "" || p.SourceHost != "" || p.Role != "primary" {
		t.Errorf("primary config: %+v", p)
	}
	if len(p.Peers) != 2 || p.Peers[0].SlotName == "" {
		t.Errorf("primary peers: %+v", p.Peers)
	}
	s := pgStreamMemberConfig(doc.Frames[0], doc, members, 1, "standby", "pgstream01.example.net")
	if s.SlotName == "" || s.SourceHost != "pgstream01.example.net" || s.Service != "postgresql-17" || s.DataDir != "/var/lib/pgsql/17/data" {
		t.Errorf("standby config: %+v", s)
	}
}

func TestPGStreamConfEnv(t *testing.T) {
	env := strings.Join(pgStreamConfEnv(designFrame{Type: "pgstream", GenerateCert: true, PGQuerySource: "pgstatements", PMMNodeID: "pmm1"}, "/c", "/d"), "\n")
	for _, want := range []string{"REPLUSER=replicator", "TLS=1", "PRELOAD=pg_stat_statements"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	// Query analytics is a PMM option on this frame, as on Patroni.
	env = strings.Join(pgStreamConfEnv(designFrame{Type: "pgstream", PGQuerySource: "pgstatements"}, "/c", "/d"), "\n")
	if strings.Contains(env, "PRELOAD=pg_stat_statements") {
		t.Errorf("pg_stat_statements preloaded without PMM:\n%s", env)
	}
}

func TestPGStreamWiredIn(t *testing.T) {
	if !joinKinds["pgstream"] || rebuildKinds["pgstream"] != "pgstream" || switchKinds["pgstream"] != "pgstream" {
		t.Error("pgstream must join, rebuild and switch over")
	}
	if engineForType("pgstream") != "postgres" || dbEngineOf("pgstream") != "postgres" {
		t.Error("pgstream members are PostgreSQL")
	}
	doc := pgStreamDoc(2, designFrame{})
	doc.Nodes = append(doc.Nodes, designNode{ID: "h1", Type: "haproxy"}, designNode{ID: "b1", Type: "pgbouncer"})
	doc.Edges = append(doc.Edges,
		designEdge{From: edgeEnd{Node: "f1"}, To: edgeEnd{Node: "h1"}},
		designEdge{From: edgeEnd{Node: "f1"}, To: edgeEnd{Node: "b1"}})
	if _, kind, ok := haproxyBackend(doc, "h1"); !ok || kind != "pgstream" {
		t.Errorf("HAProxy backend = %q, %v", kind, ok)
	}
	if kind, _, _, ok := pgBouncerBackend(doc, "b1"); !ok || kind != "pgstream" {
		t.Errorf("PgBouncer backend = %q, %v", kind, ok)
	}
}

func TestPGStreamBackups(t *testing.T) {
	// pgBackRest without a store is an error; with one that serves plain HTTP, too.
	doc := pgStreamDoc(3, designFrame{UsePgBackRest: true})
	var got []string
	for _, i := range pgStreamFrameIssues(doc.Frames[0], doc, map[int][]string{}) {
		if i.Level == "error" {
			got = append(got, i.Message)
		}
	}
	if len(got) == 0 {
		t.Error("pgBackRest with no SeaweedFS node should be refused")
	}
	doc = pgStreamDoc(3, designFrame{UsePgBackRest: true, SeaweedFSNodeID: "sw"})
	doc.Nodes = append(doc.Nodes, designNode{ID: "sw", Type: "seaweedfs", Label: "seaweedfs-01", TLS: true})
	for _, i := range pgStreamFrameIssues(doc.Frames[0], doc, map[int][]string{}) {
		if i.Level == "error" {
			t.Errorf("a TLS store should be accepted: %s", i.Message)
		}
	}

	env := strings.Join(pgStreamConfEnv(designFrame{Type: "pgstream", Label: "pg-stream-01", UsePgBackRest: true}, "/c", "/d"), "\n")
	if !strings.Contains(env, "ARCHIVE_CMD=pgbackrest --stanza=pg-stream-01 archive-push %p") {
		t.Errorf("no archive_command:\n%s", env)
	}
	if strings.Contains(strings.Join(pgStreamConfEnv(designFrame{Type: "pgstream"}, "/c", "/d"), "\n"), "ARCHIVE_CMD") {
		t.Error("archive_command set without backups")
	}

	m := pgStreamMembers(doc.Frames[0], doc)
	c := pgStreamMemberConfig(doc.Frames[0], doc, m, 0, "primary", "")
	if c.BackupEngine != "pgbackrest" || c.BackupStanza != "pg-stream-01" {
		t.Errorf("backup fields not recorded: %+v", c)
	}
}

func TestPGRoleCheckNaming(t *testing.T) {
	if pgRoleCheckServiceScript("repmgr") != repmgrCheckServiceScript {
		t.Error("repmgr members must keep the units they already have")
	}
	s := pgRoleCheckServiceScript("pgstream")
	if strings.Contains(s, "repmgr") {
		t.Errorf("a streaming replication member's health check still names repmgr:\n%s", s)
	}
	for _, want := range []string{"/usr/local/bin/dbcanvas-pgrolechk", "pgrolechk.socket", "pgrolechk@.service", "pg_is_in_recovery()"} {
		if !strings.Contains(s, want) {
			t.Errorf("health check lacks %q", want)
		}
	}
}

func TestPGStreamCatchUpWaitsForTheShutdownCheckpoint(t *testing.T) {
	// The catch-up compares with where the old primary stopped, not with what the candidate
	// received: a candidate whose stream had stopped would otherwise be promoted short.
	if !strings.Contains(pgStreamCatchUpScript, "pg_last_wal_replay_lsn() > '$TARGET'::pg_lsn") {
		t.Error("catch-up does not wait for the old primary's shutdown checkpoint")
	}
	if strings.Contains(pgStreamCatchUpScript, "pg_last_wal_receive_lsn") {
		t.Error("catch-up still trusts the candidate's own receive position")
	}
	if !strings.Contains(pgStreamShutdownLSNScript, "shut down") || !strings.Contains(pgStreamShutdownLSNScript, "Latest checkpoint location") {
		t.Error("the shutdown LSN must come from a cleanly shut down pg_control")
	}
}
