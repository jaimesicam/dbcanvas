package store

import (
	"context"
	"fmt"
	"time"
)

// The lab knobs: features whose only purpose is to make a database exhibit one
// specific, measurable pathology on demand.
//
// They exist for the same reason the working set does. A simulation that only
// behaves well teaches nothing about the conditions people actually have to
// diagnose, and every one of these is a condition that is hard to reproduce
// deliberately and easy to meet by accident in production:
//
//   - An idle transaction holding a read view open, so purge cannot advance and
//     the history list grows without bound.
//   - Thousands of tables, so the table cache stops holding the working set of
//     table handles and every query pays to reopen one.
//   - Queries that build large temporary tables, in memory or spilled to disk.
//   - Concurrent writers fighting over a handful of rows, so lock waits — and,
//     asked for it, real deadlocks — appear under load.
//   - A query whose predicate no index can serve, so the server reads every row.
//   - Write pressure of two distinct shapes: many tiny commits, which costs
//     fsyncs, and bulk redo, which costs checkpoint headroom.
//
// Not every engine can do every one of them, and the honest answer to "can it"
// is per engine — see Capabilities.
//
// Every one of these parks its work on tables nothing else in the application
// touches (lab_parking, lab_hotrows, lab_bulk) or reads without writing. A knob
// meant to be observed alongside the simulation must never be able to stall it.

// ErrUnsupported is returned by a lab operation an engine cannot perform. It is
// not a failure: the dashboard reports it as "not available for this engine",
// the same way the size target does for Valkey.
var ErrUnsupported = fmt.Errorf("not supported by this engine")

// LabSupport reports which knobs an engine can actually turn. Reported rather
// than assumed so the UI can say why a control is absent instead of offering one
// that silently does nothing.
type LabSupport struct {
	// IdleTransaction needs real multi-statement transactions with a snapshot
	// that holds back garbage collection.
	IdleTransaction bool `json:"idleTransaction"`
	// ExtraTables needs a per-table (or per-collection) handle the server caches.
	ExtraTables bool `json:"extraTables"`
	// TempTables needs a query planner that materialises intermediate results.
	TempTables bool `json:"tempTables"`
	// LockContention needs row-level locks a reader can take explicitly and a
	// server that detects a cycle between them.
	LockContention bool `json:"lockContention"`
	// ScanQueries needs a planner that will read every row when no index serves
	// the predicate — and a way to confirm afterwards that it did.
	ScanQueries bool `json:"scanQueries"`
	// WritePressure needs durable commits with a write-ahead log, which is what
	// makes both an fsync and a checkpoint cost something.
	WritePressure bool `json:"writePressure"`
}

// Temp-table modes. Memory keeps the intermediate result in RAM; Disk forces it
// to spill, which is the case worth measuring.
const (
	TempOff    = "off"
	TempMemory = "memory"
	TempDisk   = "disk"
)

func ValidTempMode(s string) bool {
	return s == TempOff || s == TempMemory || s == TempDisk
}

// Lock-contention modes.
//
// Light and Heavy differ in one deliberate way beyond intensity: Light locks
// the rows it needs in a consistent order, so writers queue behind each other
// and wait. Heavy locks two rows in an order that depends on the worker, which
// is the textbook recipe for a cycle — so it produces genuine deadlocks the
// server has to detect and break, on top of the waiting.
const (
	ContentionOff   = "off"
	ContentionLight = "light"
	ContentionHeavy = "heavy"
)

func ValidContentionMode(s string) bool {
	return s == ContentionOff || s == ContentionLight || s == ContentionHeavy
}

// Write-pressure modes. The two are not degrees of the same thing — they cost
// different resources and show up on different charts:
//
//	commits  many tiny transactions -> one log flush each -> fsyncs/sec
//	redo     few large transactions -> many dirty pages   -> checkpoint age
//
// Conflating them into one "write harder" knob would make an fsync-bound server
// and a checkpoint-bound one look identical, which is exactly the distinction
// worth teaching.
const (
	WritePressureOff     = "off"
	WritePressureCommits = "commits"
	WritePressureRedo    = "redo"
)

func ValidWritePressureMode(s string) bool {
	return s == WritePressureOff || s == WritePressureCommits || s == WritePressureRedo
}

// LabHotRows is how many rows the contention knob fights over. Small enough
// that a handful of writers collide constantly, more than one so Heavy has two
// distinct rows to lock in opposite orders.
const LabHotRows = 8

// LabBulkRows is how many wide rows the redo knob rewrites. The table is
// overwritten in place rather than appended to, so redo volume is generated
// without the table growing — the mistake §236 was written to stop repeating.
const LabBulkRows = 256

// LabBulkPayload is the size of each of those rows' payloads. 4 KiB is one
// InnoDB page's worth of user data, so a full pass dirties at least LabBulkRows
// pages and the arithmetic on the panel is something a reader can follow.
const LabBulkPayload = 4096

// MaxScanRate bounds how many index-less scans may be requested per minute. The
// cap is low on purpose: a scan of the tick history is the most expensive thing
// this application can ask for, and the knob is meant to degrade a server
// visibly, not to wedge it beyond the point where anything else can be observed.
const MaxScanRate = 120

// MaxCommitRate bounds tiny commits per second. Beyond a few thousand the
// bottleneck stops being the server and starts being this application's own
// round-trip latency, which measures the wrong thing.
const MaxCommitRate = 2000

// ClampScanRate normalises a requested scans-per-minute.
func ClampScanRate(n int) int {
	switch {
	case n <= 0:
		return 0
	case n > MaxScanRate:
		return MaxScanRate
	}
	return n
}

// ClampCommitRate normalises a requested commits-per-second.
func ClampCommitRate(n int) int {
	switch {
	case n <= 0:
		return 0
	case n > MaxCommitRate:
		return MaxCommitRate
	}
	return n
}

// lab_hotrows holds two disjoint ranges of rows, both single-row updates on a
// table nothing else in the application touches:
//
//	ids 1..LabHotRows                       the contended hot set
//	ids labCommitBase+1..+labCommitRows     the commit knob's targets
//
// They are kept apart on purpose. Commit pressure is meant to cost fsyncs and
// nothing else; if it fought for the same rows as the contention knob, an
// fsync-bound measurement would be polluted by lock waits and the two knobs
// could not be run together or told apart.
//
// Neither range is lab_parking, which the idle transaction holds an uncommitted
// lock on for up to a day — a commit storm aimed at that row would block on it
// forever rather than commit anything.
const (
	labCommitBase = 100
	labCommitRows = 8
)

// hotRowID maps a worker (or iteration) onto the contended hot set.
func hotRowID(i int) int {
	if i < 0 {
		i = -i
	}
	return 1 + i%LabHotRows
}

// commitRowID maps an iteration onto the commit knob's own rows.
func commitRowID(i int) int {
	if i < 0 {
		i = -i
	}
	return labCommitBase + 1 + i%labCommitRows
}

// contentionPlan decides which rows one worker locks, in what order, and how
// long it holds them.
//
// The lock *order* is the whole difference between the two modes:
//
//	light  every worker takes one row, from a set smaller than the worker
//	       count, so workers queue behind each other and wait.
//	heavy  every worker takes two rows, and odd-numbered workers take them in
//	       the opposite order to even-numbered ones. That is a cycle, which the
//	       server must detect and break by killing somebody — a real deadlock,
//	       not a slow wait.
//
// The hold is what makes the wait long enough to see. Without it every lock
// would be released within the same millisecond it was taken and contention
// would exist only in theory.
func contentionPlan(mode string, worker int) (ids []int, hold time.Duration, err error) {
	switch mode {
	case ContentionLight:
		// Half the hot set, so the default worker count maps two writers onto
		// every row.
		return []int{1 + worker%(LabHotRows/2)}, 25 * time.Millisecond, nil
	case ContentionHeavy:
		// Workers are paired off, and both members of a pair take the *same* two
		// rows — one ascending, one descending. Giving each worker its own pair
		// of rows would deadlock nothing, because two transactions can only
		// deadlock over rows they both want.
		pair := (worker / 2) % (LabHotRows / 2)
		a, b := 1+pair*2, 2+pair*2
		if worker%2 == 1 {
			a, b = b, a
		}
		return []int{a, b}, 40 * time.Millisecond, nil
	}
	return nil, 0, fmt.Errorf("unknown contention mode %q", mode)
}

// ContentionWorkers is how many writers a mode runs concurrently. Both counts
// exceed the number of rows they compete for, which is what guarantees the
// condition rather than merely making it likely.
func ContentionWorkers(mode string) int {
	switch mode {
	case ContentionLight:
		return 8
	case ContentionHeavy:
		return 6
	}
	return 0
}

// scanRange picks the band of tick volumes the index-less query filters on.
//
// Ticks carry a volume of 100..9099, so a band of 100 matches roughly one row in
// ninety — few enough that the result is small while the whole table still has
// to be read to produce it. That gap between rows returned and rows read is the
// thing the knob exists to show, and it is why the predicate is a narrow band
// rather than a wide one.
//
// The band moves between calls so that no single run can be dismissed as having
// hit a convenient slice of the data.
func scanRange() (lo, hi int64) {
	// 90 distinct bands across the populated range, stepped by the clock.
	band := (time.Now().UnixNano() / int64(time.Second)) % 90
	lo = 100 + band*100
	return lo, lo + 99
}

func scanDescription(lo, hi int64) string {
	return fmt.Sprintf("tick volume between %d and %d — no index on volume, so every row is read", lo, hi)
}

func contentionDescription(mode string, ids []int) string {
	switch len(ids) {
	case 1:
		return fmt.Sprintf("%s: locked hot row %d", mode, ids[0])
	case 2:
		return fmt.Sprintf("%s: locked hot rows %d then %d", mode, ids[0], ids[1])
	}
	return mode
}

// byteWord is a compact size for a note. The dashboard has its own formatter;
// this is only for the short descriptions the store itself writes.
func byteWord(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ContentionRows is how many distinct rows a mode's workers actually touch,
// derived from the plans rather than assumed to be the whole hot set — Heavy
// pairs its workers off, so it uses two rows per pair and not all of them. The
// panel says "N writers competing for M rows", and M has to be the real number
// or the sentence is wrong.
func ContentionRows(mode string) int {
	seen := map[int]bool{}
	for w := 0; w < ContentionWorkers(mode); w++ {
		ids, _, err := contentionPlan(mode, w)
		if err != nil {
			return 0
		}
		for _, id := range ids {
			seen[id] = true
		}
	}
	return len(seen)
}

// labPayload builds one wide row's contents for the redo knob. The bytes vary
// per call so that rewriting a row is a genuine change the server must log,
// rather than an update to an identical value some engines can skip.
func labPayload(seed int64) []byte {
	b := make([]byte, LabBulkPayload)
	// A cheap xorshift, not crypto: this is filler whose only requirements are
	// that it differs between calls and costs nothing to produce.
	x := uint64(seed)*2862933555777941757 + 3037000493
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x)
	}
	return b
}

// ContentionResult is what one contended write did. Waited is the part that
// matters — the time spent blocked inside the lock request itself, rather than
// the whole transaction, so it is the wait and not the work.
type ContentionResult struct {
	Waited      time.Duration
	Duration    time.Duration
	Deadlock    bool
	Timeout     bool
	Description string
}

// ScanResult is one index-less query. Scanned is read from the engine's own
// accounting of rows read, not inferred from the row count returned or from the
// plan — a query can return three rows after reading forty million, and that
// gap is the entire point of the knob.
type ScanResult struct {
	Rows        int
	RowsRead    int64
	Scanned     bool
	Duration    time.Duration
	Description string
}

// WriteResult is one batch of write pressure. Syncs is the delta of the
// engine's own log-sync counter across the batch, so "did this actually cost
// fsyncs" is measured rather than assumed — a server with
// innodb_flush_log_at_trx_commit=2 will honestly report near zero.
type WriteResult struct {
	Mode    string
	Commits int
	Bytes   int64
	Syncs   int64
	// SyncsKnown says whether Syncs and Bytes are a measurement at all. Some
	// servers do not expose the counter (pg_stat_wal is PostgreSQL 14 and up),
	// and per §239 a number that could not be read must not be shown as a
	// reading of zero.
	SyncsKnown bool
	// Capped says the batch ran out of its time budget before reaching the
	// requested count — which is the server telling you its durable commit rate,
	// and worth showing rather than hiding behind a smaller number.
	Capped      bool
	Duration    time.Duration
	Description string
}

// writeDeadline is when a batch must stop. A zero or negative budget means no
// deadline, which is what the redo mode passes.
func writeDeadline(start time.Time, budget time.Duration) time.Time {
	if budget <= 0 {
		return time.Time{}
	}
	return start.Add(budget)
}

func writeCommitsDescription(done, want int, capped bool) string {
	if capped {
		return fmt.Sprintf("%d single-row transactions, one commit each — %d asked for, "+
			"so this is the server's durable commit rate", done, want)
	}
	return fmt.Sprintf("%d single-row transactions, one commit each", done)
}

// MaxIdleTransaction is the longest an idle transaction may be held. A day is
// far past the point where any real system would have alerted on it, and the cap
// exists so a typo cannot park a transaction open until someone notices the disk
// is full.
const MaxIdleTransaction = 24 * time.Hour

// MaxExtraTables bounds the table-count knob. Enough to push past any default
// table_open_cache several times over, few enough that creating them is a matter
// of seconds rather than minutes.
const MaxExtraTables = 5000

// ClampIdleTransaction normalises a requested hold time.
func ClampIdleTransaction(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return 0
	case d > MaxIdleTransaction:
		return MaxIdleTransaction
	}
	return d
}

// ClampExtraTables normalises a requested table count.
func ClampExtraTables(n int) int {
	switch {
	case n <= 0:
		return 0
	case n > MaxExtraTables:
		return MaxExtraTables
	}
	return n
}

// TempQueryResult is what one temporary-table query did. Rows is what came back;
// the interesting part is whether the server had to spill to build it, which the
// caller reads from the engine's own counters rather than from a guess.
type TempQueryResult struct {
	Rows        int
	Spilled     bool
	Duration    time.Duration
	Description string
}

// extraTableName is the name of one of the synthetic tables. They are named for
// what they pretend to be — an end-of-day summary per trading day, which is a
// real and common table-per-period anti-pattern — so that a person looking at
// the schema sees something plausible rather than table_0001.
func extraTableName(i int) string {
	// A fixed epoch keeps names stable across restarts, so a redeploy reuses the
	// tables it already made instead of orphaning them.
	day := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
	return "eod_summary_" + day.Format("20060102")
}

// ExtraTableNames returns the first n synthetic table names, in order.
func ExtraTableNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, extraTableName(i))
	}
	return out
}

// LabStore is the optional half of a Store: the knobs above. Kept separate from
// Store so an engine that supports none of it is not forced to carry four
// methods that all return ErrUnsupported — though in practice every
// implementation here does implement it, most of them by declining.
type LabStore interface {
	// Capabilities reports which of the following will actually do anything.
	Capabilities() LabSupport

	// HoldIdleTransaction opens a transaction, establishes a read snapshot,
	// makes one small uncommitted change, and holds it for d — then rolls it
	// back. It blocks for the whole duration and must be given a connection of
	// its own, outside the pool the rest of the application shares.
	//
	// The read snapshot is the part that matters: while it is open the server
	// cannot purge any row version created after it, so the history list grows
	// for as long as the simulation keeps writing.
	HoldIdleTransaction(ctx context.Context, d time.Duration) error

	// EnsureExtraTables makes the synthetic table count match n, creating or
	// dropping as needed, and returns how many exist afterwards.
	EnsureExtraTables(ctx context.Context, n int) (int, error)

	// TouchExtraTables reads from the named tables, one query each, which is
	// what forces the server to hold a handle open for every one of them.
	TouchExtraTables(ctx context.Context, names []string) error

	// RunTempTableQuery runs an aggregation deliberately shaped to materialise a
	// large intermediate result, in memory or spilled to disk per mode.
	RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error)

	// EnsureLabTables prepares the fixed-size tables the three knobs below write
	// to — the hot rows and the bulk payload rows. Idempotent, and called once
	// before those agents start rather than on every operation.
	EnsureLabTables(ctx context.Context) error

	// RunContendedUpdate takes a row lock on the hot set and updates under it.
	// worker decides which rows and in what order, which is what makes Heavy
	// deadlock and Light merely queue; callers run several concurrently with
	// different worker numbers.
	//
	// A deadlock or a lock-wait timeout is a *result*, not an error — it is the
	// condition being reproduced — so both are reported in ContentionResult and
	// only unexpected failures come back as err.
	RunContendedUpdate(ctx context.Context, mode string, worker int) (ContentionResult, error)

	// RunScanQuery runs a read whose predicate no index can serve, and reports
	// whether the server really did read every row.
	RunScanQuery(ctx context.Context) (ScanResult, error)

	// RunWritePressure performs one batch of writes in the shape mode asks for:
	// n tiny committed transactions, or one transaction rewriting n wide rows.
	//
	// budget caps how long the batch may take. It matters because n is a rate
	// the caller would *like* and a durable commit costs an fsync, so on a slow
	// disk n is simply not reachable — a live run wanted 400 commits a second
	// and the server could do 96. Without the budget a batch overruns the
	// interval it was issued on, batches queue behind each other, and every load
	// level collapses onto the same saturated rate. With it, the batch stops on
	// time and reports how many commits it actually got, which is a measurement
	// of the server rather than of this loop.
	//
	// It applies to the commits mode only. A partially-written redo transaction
	// would roll back and throw away the work it had done.
	RunWritePressure(ctx context.Context, mode string, n int, budget time.Duration) (WriteResult, error)
}
