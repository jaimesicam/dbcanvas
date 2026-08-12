package main

import (
	"testing"
	"time"
)

// The lab knobs degrade the database they point at on purpose, so the canvas
// must resolve them exactly and refuse what an engine cannot do — a control that
// silently does nothing is worse than one that is absent.

func TestStockSimLabPerEngine(t *testing.T) {
	for engine, want := range map[string]stockSimLabSupport{
		"mysql":    {IdleTxn: true, ExtraTables: true, TempTables: true},
		"postgres": {IdleTxn: true, ExtraTables: true, TempTables: true},
		// Collections are the table-handle analogue; the other two need a replica
		// set and a memory-limited planner respectively.
		"mongodb": {ExtraTables: true},
		// No snapshot, no table handles, no planner.
		"valkey": {},
	} {
		if got := stockSimLabFor(engine); got != want {
			t.Errorf("stockSimLabFor(%q) = %+v, want %+v", engine, got, want)
		}
	}
}

func TestStockSimIdleTxn(t *testing.T) {
	for _, c := range []struct {
		raw, engine string
		want        time.Duration
	}{
		{"", "mysql", 0},
		{"off", "mysql", 0},
		{"None", "mysql", 0},
		{"30m", "mysql", 30 * time.Minute},
		{"2h", "postgres", 2 * time.Hour},
		{"90s", "mysql", 90 * time.Second},
		// Capped at a day: a typo must not park a transaction open for a week.
		{"48h", "mysql", 24 * time.Hour},
		{"720h", "mysql", 24 * time.Hour},
		// Engines that cannot hold a snapshot get nothing, whatever was typed.
		{"30m", "mongodb", 0},
		{"30m", "valkey", 0},
		// Unparseable falls back to off rather than to a guess; the issue list
		// blocks the deploy.
		{"half an hour", "mysql", 0},
		{"-5m", "mysql", 0},
	} {
		if got := stockSimIdleTxn(designNode{SSIdleTxn: c.raw}, c.engine); got != c.want {
			t.Errorf("stockSimIdleTxn(%q, %q) = %v, want %v", c.raw, c.engine, got, c.want)
		}
	}
}

func TestStockSimExtraTablesAndTempMode(t *testing.T) {
	for _, c := range []struct {
		n      int
		engine string
		want   int
	}{
		{0, "mysql", 0}, {-5, "mysql", 0}, {500, "mysql", 500},
		{5000, "postgres", 5000}, {99999, "mysql", stockSimMaxExtraTables},
		{500, "mongodb", 500}, // collections count
		{500, "valkey", 0},    // nothing to cache
	} {
		if got := stockSimExtraTables(designNode{SSExtraTables: c.n}, c.engine); got != c.want {
			t.Errorf("stockSimExtraTables(%d, %q) = %d, want %d", c.n, c.engine, got, c.want)
		}
	}
	for _, c := range []struct{ mode, engine, want string }{
		{"", "mysql", "off"}, {"off", "mysql", "off"},
		{"memory", "mysql", "memory"}, {"disk", "postgres", "disk"},
		{"DISK", "mysql", "disk"}, // case-insensitive
		{"banana", "mysql", "off"},
		{"disk", "mongodb", "off"}, {"disk", "valkey", "off"},
	} {
		if got := stockSimTempTables(designNode{SSTempTables: c.mode}, c.engine); got != c.want {
			t.Errorf("stockSimTempTables(%q, %q) = %q, want %q", c.mode, c.engine, got, c.want)
		}
	}
}

func TestStockSimLabIssues(t *testing.T) {
	// A duration that does not parse is an error: the deployed value would not
	// be the one asked for.
	bad := stockSimLabIssues(designNode{Label: "sim1", SSIdleTxn: "half an hour"}, "mysql")
	if len(bad) != 1 || bad[0].Level != "error" {
		t.Fatalf("unparseable idle txn: got %+v, want one error", bad)
	}
	// A valid hold is legal and gets a plain warning about what it does, because
	// this knob exists to degrade the server it is pointed at.
	ok := stockSimLabIssues(designNode{Label: "sim1", SSIdleTxn: "30m"}, "mysql")
	if len(ok) != 1 || ok[0].Level != "info" {
		t.Fatalf("valid idle txn: got %+v, want one info", ok)
	}
	// On an engine that cannot do it, ignored rather than wrong.
	ignored := stockSimLabIssues(designNode{Label: "sim1", SSIdleTxn: "30m"}, "valkey")
	if len(ignored) != 1 || ignored[0].Level != "warning" {
		t.Fatalf("idle txn on valkey: got %+v, want one warning", ignored)
	}
	// Over the cap deploys, capped.
	capped := stockSimLabIssues(designNode{Label: "sim1", SSIdleTxn: "48h"}, "mysql")
	if len(capped) != 1 || capped[0].Level != "warning" {
		t.Fatalf("48h: got %+v, want one warning", capped)
	}
	badMode := stockSimLabIssues(designNode{Label: "sim1", SSTempTables: "banana"}, "mysql")
	if len(badMode) != 1 || badMode[0].Level != "error" {
		t.Fatalf("bad temp mode: got %+v, want one error", badMode)
	}
	for _, quiet := range []designNode{
		{Label: "s"}, {Label: "s", SSTempTables: "off"},
		{Label: "s", SSExtraTables: 500, SSTempTables: "disk"},
	} {
		if got := stockSimLabIssues(quiet, "mysql"); len(got) != 0 {
			t.Errorf("%+v on mysql: got %+v, want no issues", quiet, got)
		}
	}
}
