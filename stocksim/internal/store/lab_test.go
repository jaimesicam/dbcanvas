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

// TestLabTablesAreOwned guards the isolation rule: every table a lab knob
// writes to is one nothing else in the application touches, and every one of
// them is in the owned list so Wipe and DropSchema reach it. A lab table left
// out of that list would survive a wipe and quietly outlive the deployment.
func TestLabTablesAreOwned(t *testing.T) {
	for _, tables := range [][]string{mysqlTables, pgTables} {
		for _, want := range []string{"lab_parking", "lab_hotrows", "lab_bulk"} {
			found := false
			for _, name := range tables {
				if name == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s must be in the owned-table list so Wipe and DropSchema reach it", want)
			}
		}
	}
}

func TestValidContentionMode(t *testing.T) {
	for _, ok := range []string{ContentionOff, ContentionLight, ContentionHeavy} {
		if !ValidContentionMode(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Light", "extreme", "on"} {
		if ValidContentionMode(bad) {
			t.Errorf("%q should not be valid", bad)
		}
	}
}

func TestValidWritePressureMode(t *testing.T) {
	for _, ok := range []string{WritePressureOff, WritePressureCommits, WritePressureRedo} {
		if !ValidWritePressureMode(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Commits", "wal", "storm"} {
		if ValidWritePressureMode(bad) {
			t.Errorf("%q should not be valid", bad)
		}
	}
}

func TestClampScanRate(t *testing.T) {
	for in, want := range map[int]int{
		0: 0, -5: 0, 1: 1, 60: 60,
		MaxScanRate: MaxScanRate, MaxScanRate + 1: MaxScanRate, 100000: MaxScanRate,
	} {
		if got := ClampScanRate(in); got != want {
			t.Errorf("ClampScanRate(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestClampCommitRate(t *testing.T) {
	for in, want := range map[int]int{
		0: 0, -1: 0, 500: 500,
		MaxCommitRate: MaxCommitRate, MaxCommitRate + 1: MaxCommitRate,
	} {
		if got := ClampCommitRate(in); got != want {
			t.Errorf("ClampCommitRate(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestContentionPlanLight pins the property Light depends on: every worker takes
// exactly one row, and there are fewer rows in play than the workers competing
// for them. Equal counts would give every worker its own row and no contention
// at all — a knob that silently did nothing.
func TestContentionPlanLight(t *testing.T) {
	workers := ContentionWorkers(ContentionLight)
	seen := map[int]bool{}
	for w := 0; w < workers; w++ {
		ids, hold, err := contentionPlan(ContentionLight, w)
		if err != nil {
			t.Fatalf("worker %d: %v", w, err)
		}
		if len(ids) != 1 {
			t.Fatalf("light should take one row, worker %d took %d", w, len(ids))
		}
		if hold <= 0 {
			t.Errorf("worker %d has no hold, so nothing can wait on it", w)
		}
		seen[ids[0]] = true
	}
	if len(seen) >= workers {
		t.Errorf("light spreads %d workers over %d rows — with a row each, nothing contends",
			workers, len(seen))
	}
}

// TestContentionPlanHeavyDeadlocks pins the property Heavy depends on: at least
// one pair of workers takes the same two rows in opposite orders. That cycle is
// what makes the server detect a deadlock; without it Heavy would only be a
// slower version of Light.
func TestContentionPlanHeavyDeadlocks(t *testing.T) {
	workers := ContentionWorkers(ContentionHeavy)
	plans := make([][]int, workers)
	for w := 0; w < workers; w++ {
		ids, _, err := contentionPlan(ContentionHeavy, w)
		if err != nil {
			t.Fatalf("worker %d: %v", w, err)
		}
		if len(ids) != 2 {
			t.Fatalf("heavy should take two rows, worker %d took %d", w, len(ids))
		}
		if ids[0] == ids[1] {
			t.Fatalf("worker %d locks row %d twice, which cannot deadlock", w, ids[0])
		}
		plans[w] = ids
	}
	opposed := false
	for i := range plans {
		for j := range plans {
			if plans[i][0] == plans[j][1] && plans[i][1] == plans[j][0] {
				opposed = true
			}
		}
	}
	if !opposed {
		t.Error("no two heavy workers take the same rows in opposite orders, so no cycle can form")
	}
}

func TestContentionPlanRejectsUnknownMode(t *testing.T) {
	for _, bad := range []string{ContentionOff, "", "banana"} {
		if _, _, err := contentionPlan(bad, 0); err == nil {
			t.Errorf("contentionPlan(%q) should refuse rather than invent a plan", bad)
		}
	}
	if n := ContentionWorkers(ContentionOff); n != 0 {
		t.Errorf("ContentionWorkers(off) = %d, want 0", n)
	}
}

// TestLabRowRangesAreDisjoint is the isolation rule inside lab_hotrows: the
// contended rows and the commit knob's rows must never overlap, or an
// fsync-bound measurement would be polluted by lock waits.
func TestLabRowRangesAreDisjoint(t *testing.T) {
	hot := map[int]bool{}
	for i := 0; i < LabHotRows*3; i++ {
		hot[hotRowID(i)] = true
	}
	if len(hot) != LabHotRows {
		t.Errorf("hotRowID covers %d rows, want %d", len(hot), LabHotRows)
	}
	for i := 0; i < labCommitRows*3; i++ {
		if hot[commitRowID(i)] {
			t.Fatalf("commit row %d collides with the contended hot set", commitRowID(i))
		}
	}
	// Negative inputs must still land in range: worker numbers are derived
	// arithmetic, and a negative modulo in Go would produce a row id of zero or
	// below that no seeded row has.
	if id := hotRowID(-3); id < 1 || id > LabHotRows {
		t.Errorf("hotRowID(-3) = %d, outside the seeded range", id)
	}
	if id := commitRowID(-3); id <= labCommitBase {
		t.Errorf("commitRowID(-3) = %d, outside the seeded range", id)
	}
}

// TestLabPayloadVaries guards the redo knob's premise: rewriting a row must be a
// real change. Identical bytes would let an engine skip the write, and the knob
// would generate no redo at all.
func TestLabPayloadVaries(t *testing.T) {
	a, b := labPayload(1), labPayload(2)
	if len(a) != LabBulkPayload || len(b) != LabBulkPayload {
		t.Fatalf("payload sizes %d/%d, want %d", len(a), len(b), LabBulkPayload)
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("two payloads are identical, so a rewrite would not be a change")
	}
	if bytesAllEqual(a) {
		t.Error("payload is a single repeated byte, which compresses to nothing on disk")
	}
}

func bytesAllEqual(b []byte) bool {
	for i := range b {
		if b[i] != b[0] {
			return false
		}
	}
	return true
}

// TestScanRangeIsNarrow pins the shape the scan knob depends on: a band narrow
// enough that few rows match, inside the volume range the simulation actually
// generates (100..9099). A band covering everything would return the whole table
// and stop demonstrating the read-versus-returned gap.
func TestScanRangeIsNarrow(t *testing.T) {
	lo, hi := scanRange()
	if hi <= lo {
		t.Fatalf("scanRange returned an empty band %d..%d", lo, hi)
	}
	if width := hi - lo + 1; width > 200 {
		t.Errorf("scan band is %d wide, too broad to show the read-versus-returned gap", width)
	}
	if lo < 100 || hi > 9099 {
		t.Errorf("scan band %d..%d falls outside the generated volume range 100..9099", lo, hi)
	}
}

// TestPGScanPlanStats covers the parser that replaced seq_tup_read. Rows read
// must be the rows the scan emitted *plus* the rows its filter discarded — the
// discarded ones are the work done to produce nothing, which is the entire
// point of the knob.
func TestPGScanPlanStats(t *testing.T) {
	const plan = `[{"Plan":{"Node Type":"Aggregate","Actual Rows":1,"Plans":[
		{"Node Type":"Seq Scan","Relation Name":"price_ticks",
		 "Actual Rows":10669,"Rows Removed by Filter":958531}]}}]`
	matched, read, seqScan, err := pgScanPlanStats([]byte(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !seqScan {
		t.Error("a Seq Scan of price_ticks was in the plan and was not found")
	}
	if matched != 10669 {
		t.Errorf("matched = %d, want 10669", matched)
	}
	if read != 969200 {
		t.Errorf("read = %d, want 969200 (emitted plus filtered away)", read)
	}
}

// An index scan is not the condition being asked for, and must not be reported
// as one — silently passing would make the knob look like it worked when the
// planner had found a way out.
func TestPGScanPlanStatsIndexScan(t *testing.T) {
	const plan = `[{"Plan":{"Node Type":"Aggregate","Plans":[
		{"Node Type":"Index Scan","Relation Name":"price_ticks","Actual Rows":12}]}}]`
	_, _, seqScan, err := pgScanPlanStats([]byte(plan))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if seqScan {
		t.Error("an index scan was reported as a sequential scan")
	}
}

// A plan that cannot be read is an error, never a zero — §239's rule.
func TestPGScanPlanStatsRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "[]", "not json"} {
		if _, _, _, err := pgScanPlanStats([]byte(bad)); err == nil {
			t.Errorf("pgScanPlanStats(%q) returned no error; an unreadable plan must not read as zero rows", bad)
		}
	}
}
