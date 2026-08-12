package store

import (
	"testing"
	"time"
)

// The clamps are the safety rail on knobs whose purpose is to degrade a server.
// A typo must not park a transaction open for a week or create a million tables.

func TestClampIdleTransaction(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                  0,
		-time.Hour:         0,
		30 * time.Minute:   30 * time.Minute,
		MaxIdleTransaction: MaxIdleTransaction,
		48 * time.Hour:     MaxIdleTransaction,
		10000 * time.Hour:  MaxIdleTransaction,
	} {
		if got := ClampIdleTransaction(in); got != want {
			t.Errorf("ClampIdleTransaction(%v) = %v, want %v", in, got, want)
		}
	}
	if MaxIdleTransaction != 24*time.Hour {
		t.Errorf("the documented cap is 24h, got %v", MaxIdleTransaction)
	}
}

func TestClampExtraTables(t *testing.T) {
	for in, want := range map[int]int{
		0: 0, -1: 0, 1: 1, 500: 500,
		MaxExtraTables: MaxExtraTables, MaxExtraTables + 1: MaxExtraTables, 1 << 20: MaxExtraTables,
	} {
		if got := ClampExtraTables(in); got != want {
			t.Errorf("ClampExtraTables(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestExtraTableNames pins two properties the create/drop reconciliation relies
// on: names are stable across restarts (so a redeploy reuses the tables it
// already made rather than orphaning them) and unique.
func TestExtraTableNames(t *testing.T) {
	a := ExtraTableNames(2000)
	b := ExtraTableNames(2000)
	if len(a) != 2000 {
		t.Fatalf("got %d names, want 2000", len(a))
	}
	seen := map[string]bool{}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("name %d is not stable: %q vs %q", i, a[i], b[i])
		}
		if seen[a[i]] {
			t.Fatalf("duplicate name %q", a[i])
		}
		seen[a[i]] = true
	}
	// A shorter request must be a prefix of a longer one, or shrinking the count
	// would drop the wrong tables.
	short := ExtraTableNames(10)
	for i := range short {
		if short[i] != a[i] {
			t.Fatalf("ExtraTableNames is not prefix-stable at %d: %q vs %q", i, short[i], a[i])
		}
	}
	// Every name carries the prefix the touch path checks before interpolating
	// it into SQL.
	for _, name := range short {
		if len(name) < 12 || name[:12] != "eod_summary_" {
			t.Errorf("name %q lacks the eod_summary_ prefix the SQL guard requires", name)
		}
	}
}

func TestValidTempMode(t *testing.T) {
	for _, ok := range []string{TempOff, TempMemory, TempDisk} {
		if !ValidTempMode(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Memory", "ram", "banana"} {
		if ValidTempMode(bad) {
			t.Errorf("%q should not be valid", bad)
		}
	}
}

// TestLabParkingIsOwned guards the isolation rule: an idle transaction parks on
// a table nothing else in the application writes, so a hold of hours cannot
// block the simulation it exists to be observed alongside.
func TestLabParkingIsOwned(t *testing.T) {
	for _, tables := range [][]string{mysqlTables, pgTables} {
		found := false
		for _, name := range tables {
			if name == "lab_parking" {
				found = true
			}
		}
		if !found {
			t.Error("lab_parking must be in the owned-table list so Wipe and DropSchema reach it")
		}
	}
}
