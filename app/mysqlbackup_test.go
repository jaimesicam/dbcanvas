package main

import (
	"strings"
	"testing"
	"time"
)

func xbTestCatalog() xbCatalog {
	return xbCatalog{Backups: []xbBackup{
		{Name: "f1", Type: "full", End: "2026-10-19T01:00:00Z", Host: "pxc01", BinlogFile: "binlog.000003", BinlogPos: "197"},
		{Name: "i1", Type: "incremental", Base: "f1", End: "2026-10-19T02:00:00Z", Host: "pxc01", BinlogFile: "binlog.000005", BinlogPos: "500"},
		{Name: "i2", Type: "incremental", Base: "i1", End: "2026-10-19T03:00:00Z", Host: "pxc01", BinlogFile: "binlog.000007", BinlogPos: "900"},
		{Name: "f2", Type: "full", End: "2026-10-19T04:00:00Z", Host: "pxc02", BinlogFile: "binlog.000002", BinlogPos: "157"},
	}}
}

func xbNames(bs []xbBackup) string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return strings.Join(out, ",")
}

// An incremental restores as its whole chain, full first; a broken chain is said, not guessed.
func TestXBChain(t *testing.T) {
	c := xbTestCatalog()
	if got, err := c.chain("i2"); err != nil || xbNames(got) != "f1,i1,i2" {
		t.Errorf("chain(i2) = %s, %v", xbNames(got), err)
	}
	if got, err := c.chain("f2"); err != nil || xbNames(got) != "f2" {
		t.Errorf("chain(f2) = %s, %v", xbNames(got), err)
	}
	c.Backups = append(c.Backups[:1], c.Backups[2:]...) // drop i1
	if _, err := c.chain("i2"); err == nil || !strings.Contains(err.Error(), "i1") {
		t.Errorf("broken chain not reported: %v", err)
	}
}

// Deleting a backup takes every incremental built on it with it, and nothing else.
func TestXBDependents(t *testing.T) {
	d := xbTestCatalog().dependents("i1")
	if !d["i1"] || !d["i2"] || d["f1"] || d["f2"] {
		t.Errorf("dependents(i1) = %v", d)
	}
}

func TestXBPlanRestore(t *testing.T) {
	c := xbTestCatalog()
	now := time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)

	p, err := xbPlanRestore(c, xbRestoreRequest{Mode: "backup", Name: "i2"}, true, now)
	if err != nil || xbNames(p.Chain) != "f1,i1,i2" || p.Replay {
		t.Errorf("backup i2: %s replay=%v %v", xbNames(p.Chain), p.Replay, err)
	}
	// latest: the newest backup, plus everything shipped after it when PITR is on.
	if p, err = xbPlanRestore(c, xbRestoreRequest{Mode: "latest"}, true, now); err != nil || xbNames(p.Chain) != "f2" || !p.Replay || !p.Stop.IsZero() {
		t.Errorf("latest: %s replay=%v stop=%v %v", xbNames(p.Chain), p.Replay, p.Stop, err)
	}
	if p, _ = xbPlanRestore(c, xbRestoreRequest{Mode: "latest"}, false, now); p.Replay {
		t.Error("latest without PITR replays")
	}
	// time: the newest backup that ends at or before the target, then replay up to it.
	p, err = xbPlanRestore(c, xbRestoreRequest{Mode: "time", Time: "2026-10-19T03:30:00Z"}, true, now)
	if err != nil || xbNames(p.Chain) != "f1,i1,i2" || !p.Replay || p.Stop.Format(time.RFC3339) != "2026-10-19T03:30:00Z" {
		t.Errorf("time 03:30: %s stop=%v %v", xbNames(p.Chain), p.Stop, err)
	}
	for _, bad := range []xbRestoreRequest{
		{Mode: "time", Time: "2026-10-19T00:30:00Z"}, // before every backup
		{Mode: "time", Time: "2026-10-19T09:00:00Z"}, // in the future
		{Mode: "time", Time: "yesterday"},
		{Mode: "backup", Name: "nope"},
		{Mode: "sideways"},
	} {
		if _, err := xbPlanRestore(c, bad, true, now); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if _, err := xbPlanRestore(c, xbRestoreRequest{Mode: "time", Time: "2026-10-19T03:30:00Z"}, false, now); err == nil {
		t.Error("time restore accepted with PITR off")
	}
}

// Replay starts at the binary log the backup ended in; one that was never shipped is an error.
func TestXBReplayLogs(t *testing.T) {
	logs := []seaweedObject{{Name: "binlog.000002"}, {Name: "binlog.000003"}, {Name: "binlog.000004"}}
	got, err := xbReplayLogs(logs, "binlog.000003")
	if err != nil || strings.Join(got, ",") != "binlog.000003,binlog.000004" {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := xbReplayLogs(logs[1:], "binlog.000002"); err == nil {
		t.Error("missing first binary log not reported")
	}
}

func TestXBParseInfo(t *testing.T) {
	out := "EPOCH=20261019T060000Z\nBYTES=77507359\nbackup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 19880569\n" +
		"binlog_pos = filename 'binlog.000006', position '197', GTID of the last change '5e9e1c00-cb83-11f1-95c8-56ab8c387bcb:1-19'\n"
	kv := xbParseInfo(out)
	if kv["to_lsn"] != "19880569" || kv["from_lsn"] != "0" {
		t.Errorf("lsns: %v", kv)
	}
	m := xbBinlogPosRe.FindStringSubmatch(kv["binlog_pos"])
	if m == nil || m[1] != "binlog.000006" || m[2] != "197" || !strings.HasSuffix(m[3], ":1-19") {
		t.Errorf("binlog_pos: %v", m)
	}
	if xbOutValue(out, "EPOCH") != "20261019T060000Z" || xbOutValue(out, "BYTES") != "77507359" {
		t.Error("KEY=value lines not read")
	}
}

func TestXBIssues(t *testing.T) {
	doc := designDoc{Nodes: []designNode{{ID: "sw", Type: "seaweedfs", Label: "seaweedfs-01"}}}
	if len(xbIssues("x", false, "", "", false, doc)) != 0 {
		t.Error("off reported")
	}
	if len(xbIssues("x", true, "", "", false, doc)) != 1 {
		t.Error("no SeaweedFS node not reported")
	}
	if len(xbIssues("x", true, "sw", "", true, doc)) != 1 {
		t.Error("keyring not reported")
	}
	if got := xbIssues("x", true, "sw", "", false, doc); len(got) != 0 {
		t.Errorf("valid design reported: %v", got)
	}
}

// The backup link turns XtraBackup (and binary-log shipping) on for the three MySQL kinds.
func TestComposeXtraBackup(t *testing.T) {
	composeCatalogFixture(t)
	doc, _, _, _ := composeBuild(t, composeSpec{Name: "x", Nodes: []composeNodeSpec{
		{Kind: "seaweedfs"},
		{Kind: "ps", Backup: true},
		{Kind: "pxc", Backup: true},
		{Kind: "ps-repl", Backup: true},
	}})
	ps := nodesOfType(doc, "ps")
	if len(ps) != 1 || !ps[0].UseXtraBackup || !ps[0].XBPITR || ps[0].UsePgBackRest || ps[0].SeaweedFSNodeID == "" {
		t.Errorf("ps: %+v", ps)
	}
	for _, f := range doc.Frames {
		if (f.Type == "pxc" || f.Type == "mysql") && (!f.UseXtraBackup || !f.XBPITR || f.SeaweedFSNodeID == "") {
			t.Errorf("%s frame: useXtraBackup=%v xbPitr=%v seaweed=%q", f.Type, f.UseXtraBackup, f.XBPITR, f.SeaweedFSNodeID)
		}
	}
}
