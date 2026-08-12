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
//
// Not every engine can do every one of them, and the honest answer to "can it"
// is per engine — see Capabilities.

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
}
