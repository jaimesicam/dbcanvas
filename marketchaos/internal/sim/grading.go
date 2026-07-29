// grading.go is stage S5: baseline/validate measurement windows and
// scoring. Built around ServerStatsView/WsrepStatus/Engine's own atomic
// counters as the PRIMARY signal, not leaderboard digest deltas — see
// IMPLEMENTATION.md session 180's finding that most transaction-wrapped and
// JOIN queries never reliably accumulate Performance Schema digest
// executions in this environment, discovered live while building stage S4.
// The leaderboard stays a real, useful best-effort *display* for the
// handful of shapes confirmed to work (scanner/compliance's simpler
// reads); grading doesn't depend on it.
package sim

import (
	"context"
	"fmt"
	"time"

	"marketchaos/internal/challenge"
	"marketchaos/internal/store"
)

// gradingWindow is how long CaptureBaseline/ValidateSolution each measure —
// one window, not the written plan's "3×60s with 15s warm-up discarded
// each": a deliberate scope simplification for an interactive dashboard
// button a learner clicks and waits on, not an unattended batch job. 15s is
// long enough for the workload's own tick intervals (up to 10s for
// cleanup) to complete at least one full cycle within the window.
const gradingWindow = 15 * time.Second

// gradingSnapshot is every signal one measurement point captures — a
// point-in-time reading; CaptureBaseline/ValidateSolution take two (before
// and after gradingWindow) and diff them.
type gradingSnapshot struct {
	At       time.Time
	Server   store.ServerStats
	Wsrep    store.WsrepStatus
	Counters counterSnapshot
}

type counterSnapshot struct {
	OrdersPlaced            int64
	TradesExecuted          int64
	TxnRetries              int64
	AgentErrors             int64
	RetailOrders            int64
	InstitutionalOrders     int64
	PortfolioReads          int64
	PortfolioSummaryQueries int64
}

func (e *Engine) snapshotCounters() counterSnapshot {
	return counterSnapshot{
		OrdersPlaced:            e.counters.ordersPlaced.Load(),
		TradesExecuted:          e.counters.tradesExecuted.Load(),
		TxnRetries:              e.counters.txnRetries.Load(),
		AgentErrors:             e.counters.agentErrors.Load(),
		RetailOrders:            e.counters.retailOrders.Load(),
		InstitutionalOrders:     e.counters.institutionalOrders.Load(),
		PortfolioReads:          e.counters.portfolioReads.Load(),
		PortfolioSummaryQueries: e.counters.portfolioSummaryQueries.Load(),
	}
}

// captureGradingSnapshot reads every signal at one point in time — cheap
// enough (a handful of SHOW/SELECT statements) to not itself meaningfully
// disturb the window being measured.
func (e *Engine) captureGradingSnapshot(ctx context.Context) gradingSnapshot {
	srv, _ := e.Store.ServerStats(ctx)
	wsrep, _ := e.Store.WsrepStatus(ctx)
	return gradingSnapshot{At: time.Now(), Server: srv, Wsrep: wsrep, Counters: e.snapshotCounters()}
}

// indexCount reports how many indexes currently exist across this app's own
// tables — used by the regression check to bound how many NEW indexes a
// challenge's fix is allowed to add.
func (e *Engine) indexCount(ctx context.Context) (int, error) {
	var n int
	err := e.Store.DB.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT CONCAT(TABLE_NAME,'.',INDEX_NAME)) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE()").Scan(&n)
	return n, err
}

// GradeResult is what POST /api/challenges/validate returns — every number
// that went into the score, not just the total, so the challenge panel can
// show the learner what was actually measured.
type GradeResult struct {
	Passed             bool    `json:"passed"` // correctness gate — see CorrectnessFailure
	CorrectnessFailure string  `json:"correctnessFailure,omitempty"`
	FunctionalPass     bool    `json:"functionalPass"`
	FunctionalPoints   int     `json:"functionalPoints"`
	FunctionalNote     string  `json:"functionalNote,omitempty"`
	PerformanceMetric  string  `json:"performanceMetric"`
	PerformanceBefore  float64 `json:"performanceBefore"`
	PerformanceAfter   float64 `json:"performanceAfter"`
	PerformancePoints  int     `json:"performancePoints"`
	RegressionPoints   int     `json:"regressionPoints"`
	RegressionNote     string  `json:"regressionNote,omitempty"`
	DiagnosisPoints    int     `json:"diagnosisPoints"`
	DiagnosisNote      string  `json:"diagnosisNote,omitempty"`
	TotalScore         int     `json:"totalScore"`
	Grade              string  `json:"grade"`
}

// baselineState holds the one stored baseline snapshot + starting index
// count between CaptureBaseline and ValidateSolution. Never explicitly
// cleared on challenge.Manager.Reset — harmless: ValidateSolution and a
// fresh CaptureBaseline both require an active challenge, and Reset always
// goes through StateNone first, so a stale baseline from a previous
// challenge run can never be scored against a different one.
type baselineState struct {
	snapshot   gradingSnapshot
	indexCount int
	captured   bool
}

// CaptureBaseline measures a window of the challenge's CURRENT (bad) state
// — must be called while the challenge is active and not yet fixed, so
// what gets captured is genuinely the "before" picture. Blocks for
// gradingWindow (~15s): this is a learner clicking a button and waiting on
// live measurement, not a background job.
func (e *Engine) CaptureBaseline(ctx context.Context) error {
	c, state, active := e.Challenges.Active()
	if !active {
		return fmt.Errorf("no challenge active")
	}
	if state == challenge.StateBaseline || state == challenge.StateGraded {
		return fmt.Errorf("baseline already captured for %q — reset the challenge to try again", c.ID)
	}
	before := e.captureGradingSnapshot(ctx)
	idxBefore, _ := e.indexCount(ctx)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(gradingWindow):
	}
	after := e.captureGradingSnapshot(ctx)

	e.gradingMu.Lock()
	e.baseline = baselineState{snapshot: diffSnapshot(before, after, gradingWindow), indexCount: idxBefore, captured: true}
	e.gradingMu.Unlock()
	e.Challenges.MarkBaseline()
	return nil
}

// diffSnapshot turns two point-in-time reads into a rates-over-the-window
// snapshot — server/wsrep fields become per-second deltas, counters become
// raw deltas (already naturally cumulative-since-start, a delta is what
// matters for "how much activity happened this window").
func diffSnapshot(before, after gradingSnapshot, window time.Duration) gradingSnapshot {
	secs := window.Seconds()
	if secs <= 0 {
		secs = 1
	}
	return gradingSnapshot{
		At: after.At,
		Server: store.ServerStats{
			Questions:            deltaPerSec(before.Server.Questions, after.Server.Questions, secs),
			ComSelect:            deltaPerSec(before.Server.ComSelect, after.Server.ComSelect, secs),
			ComInsert:            deltaPerSec(before.Server.ComInsert, after.Server.ComInsert, secs),
			ComUpdate:            deltaPerSec(before.Server.ComUpdate, after.Server.ComUpdate, secs),
			ComDelete:            deltaPerSec(before.Server.ComDelete, after.Server.ComDelete, secs),
			InnodbRowLockWaits:   deltaPerSec(before.Server.InnodbRowLockWaits, after.Server.InnodbRowLockWaits, secs),
			InnodbRowLockTimeMs:  after.Server.InnodbRowLockTimeMs - before.Server.InnodbRowLockTimeMs,
			InnodbDeadlocks:      after.Server.InnodbDeadlocks - before.Server.InnodbDeadlocks,
			CreatedTmpDiskTables: deltaPerSec(before.Server.CreatedTmpDiskTables, after.Server.CreatedTmpDiskTables, secs),
			ThreadsConnected:     after.Server.ThreadsConnected,
		},
		Wsrep: store.WsrepStatus{
			LocalCertFailures: after.Wsrep.LocalCertFailures - before.Wsrep.LocalCertFailures,
			LocalBFAborts:     after.Wsrep.LocalBFAborts - before.Wsrep.LocalBFAborts,
			FlowControlPaused: after.Wsrep.FlowControlPaused, // a fraction, not cumulative — as-of "after" is the right read
		},
		Counters: counterSnapshot{
			OrdersPlaced:            after.Counters.OrdersPlaced - before.Counters.OrdersPlaced,
			TradesExecuted:          after.Counters.TradesExecuted - before.Counters.TradesExecuted,
			TxnRetries:              after.Counters.TxnRetries - before.Counters.TxnRetries,
			AgentErrors:             after.Counters.AgentErrors - before.Counters.AgentErrors,
			RetailOrders:            after.Counters.RetailOrders - before.Counters.RetailOrders,
			InstitutionalOrders:     after.Counters.InstitutionalOrders - before.Counters.InstitutionalOrders,
			PortfolioReads:          after.Counters.PortfolioReads - before.Counters.PortfolioReads,
			PortfolioSummaryQueries: after.Counters.PortfolioSummaryQueries - before.Counters.PortfolioSummaryQueries,
		},
	}
}

func deltaPerSec(before, after int64, secs float64) int64 {
	return int64(float64(after-before) / secs)
}

// ValidateSolution measures the challenge's CURRENT state (presumably fixed
// by now) the same way CaptureBaseline did, then scores it against the
// stored baseline. Requires CaptureBaseline to have run first.
func (e *Engine) ValidateSolution(ctx context.Context) (GradeResult, error) {
	c, _, active := e.Challenges.Active()
	if !active {
		return GradeResult{}, fmt.Errorf("no challenge active")
	}
	e.gradingMu.Lock()
	baseline := e.baseline
	e.gradingMu.Unlock()
	if !baseline.captured {
		return GradeResult{}, fmt.Errorf("capture a baseline first")
	}

	before := e.captureGradingSnapshot(ctx)
	select {
	case <-ctx.Done():
		return GradeResult{}, ctx.Err()
	case <-time.After(gradingWindow):
	}
	after := e.captureGradingSnapshot(ctx)
	validate := diffSnapshot(before, after, gradingWindow)
	idxAfter, _ := e.indexCount(ctx)

	result := e.score(ctx, c, baseline.snapshot, validate, idxAfter)
	e.Challenges.MarkGraded()
	return result, nil
}

// score is the whole grading model: a hard correctness gate, then 4
// weighted buckets (30 functional / 50 performance / 10 regression / 10
// diagnosis = 100), then a letter-grade band — the same shape the written
// plan calls for, simplified from its original 20/15/10/10 performance
// split (which assumed reliable per-shape digest deltas that stage S4's
// live verification found don't hold up) down to one performance metric,
// chosen per challenge category from signals confirmed reliable.
func (e *Engine) score(ctx context.Context, c challenge.Challenge, baseline, validate gradingSnapshot, idxAfter int) GradeResult {
	var r GradeResult

	if reason := e.CheckInvariants(ctx); reason != "" {
		r.CorrectnessFailure = reason
		r.Grade = gradeFor(0)
		return r // hard gate: everything else stays zero
	}
	r.Passed = true

	if c.Mechanism == challenge.MechanismApp {
		r.FunctionalPass = e.Challenges.AppliedVariant()
		if !r.FunctionalPass {
			r.FunctionalNote = "the improved implementation hasn't been applied yet"
		}
	} else if c.FunctionalCheck != nil {
		if reason := e.Challenges.RunFunctionalCheck(ctx); reason != "" {
			r.FunctionalNote = reason
		} else {
			r.FunctionalPass = true
		}
	} else {
		r.FunctionalPass = true
	}
	if r.FunctionalPass {
		r.FunctionalPoints = 30
	}

	metric, before, after, points := performanceScore(c, baseline, validate)
	r.PerformanceMetric, r.PerformanceBefore, r.PerformanceAfter, r.PerformancePoints = metric, before, after, points

	newIndexes := idxAfter - e.baselineIndexCountLocked()
	maxNew := c.MaxNewIndexes
	if maxNew == 0 {
		maxNew = 2
	}
	if newIndexes <= maxNew {
		r.RegressionPoints = 10
	} else {
		r.RegressionNote = fmt.Sprintf("%d new index(es) added — more than this challenge's guideline of %d", newIndexes, maxNew)
	}

	rootCause, fixApproach := e.Challenges.DiagnosisAnswers()
	if rootCause != "" && rootCause == c.RootCause {
		r.DiagnosisPoints += 5
	}
	if fixApproach != "" && fixApproach == c.FixApproach {
		r.DiagnosisPoints += 5
	}
	if r.DiagnosisPoints < 10 {
		r.DiagnosisNote = "diagnosis: pick the correct root cause and fix approach for full credit"
	}

	r.TotalScore = r.FunctionalPoints + r.PerformancePoints + r.RegressionPoints + r.DiagnosisPoints
	r.Grade = gradeFor(r.TotalScore)
	return r
}

func (e *Engine) baselineIndexCountLocked() int {
	e.gradingMu.Lock()
	defer e.gradingMu.Unlock()
	return e.baseline.indexCount
}

// performanceScore picks the one signal confirmed reliable for this
// challenge's category (see this file's package doc comment) and scores
// the improvement from baseline to validate against a modest target
// improvement — full marks at targetImprovement or better, linear below
// that, matching the spirit of the written plan's percentile-improvement
// scoring without depending on the per-shape latency percentiles that
// turned out not to be reliably measurable here.
func performanceScore(c challenge.Challenge, baseline, validate gradingSnapshot) (metric string, before, after float64, points int) {
	const targetImprovement = 0.15 // 15% — modest, matches an interactive demo session's realistic window

	switch c.ID {
	case "pxc-no-retry-classification":
		// inverted: the FIX is restoring retries, so agentErrors should go
		// DOWN (conflicts get absorbed by retrying instead of surfacing).
		return scoreLowerBetter("agent errors/sec", float64(baseline.Counters.AgentErrors)/gradingWindow.Seconds(), float64(validate.Counters.AgentErrors)/gradingWindow.Seconds(), targetImprovement)
	case "portfolio-n-plus-1":
		return scoreLowerBetter("portfolio queries issued", float64(baseline.Counters.PortfolioSummaryQueries), float64(validate.Counters.PortfolioSummaryQueries), targetImprovement)
	}

	switch c.Category {
	case challenge.CategoryPXC:
		b := float64(baseline.Wsrep.LocalCertFailures + baseline.Wsrep.LocalBFAborts)
		a := float64(validate.Wsrep.LocalCertFailures + validate.Wsrep.LocalBFAborts)
		return scoreLowerBetter("wsrep cert failures + BF aborts this window", b, a, targetImprovement)
	case challenge.CategoryLocking:
		b := float64(baseline.Server.InnodbRowLockWaits) + float64(baseline.Counters.TxnRetries)
		a := float64(validate.Server.InnodbRowLockWaits) + float64(validate.Counters.TxnRetries)
		return scoreLowerBetter("lock waits + txn retries per sec", b, a, targetImprovement)
	default:
		// indexing/query-rewrite/joins/sorting/schema: overall server
		// throughput (Questions/sec) — a fix that lets the same workload
		// complete faster shows up as more total queries served in the
		// same wall-clock window.
		return scoreHigherBetter("server queries/sec", float64(baseline.Server.Questions), float64(validate.Server.Questions), targetImprovement)
	}
}

func scoreHigherBetter(metric string, before, after, target float64) (string, float64, float64, int) {
	if before <= 0 {
		if after > 0 {
			return metric, before, after, 50
		}
		return metric, before, after, 25 // no baseline activity to compare against — partial credit, not a hard fail
	}
	improvement := (after - before) / before
	return metric, before, after, pointsFor(improvement, target)
}

func scoreLowerBetter(metric string, before, after, target float64) (string, float64, float64, int) {
	if before <= 0 {
		if after <= 0 {
			return metric, before, after, 50 // nothing to reduce and nothing regressed
		}
		return metric, before, after, 10
	}
	improvement := (before - after) / before
	return metric, before, after, pointsFor(improvement, target)
}

func pointsFor(improvement, target float64) int {
	if improvement <= 0 {
		return 0
	}
	frac := improvement / target
	if frac > 1 {
		frac = 1
	}
	return int(50 * frac)
}

// gradeFor maps a 0-100 total score to the written plan's own grade bands.
func gradeFor(total int) string {
	switch {
	case total >= 90:
		return "Exchange Architect"
	case total >= 75:
		return "Senior Performance Engineer"
	case total >= 60:
		return "Database Troubleshooter"
	case total >= 40:
		return "Market Operator"
	default:
		return "Market Halted"
	}
}
