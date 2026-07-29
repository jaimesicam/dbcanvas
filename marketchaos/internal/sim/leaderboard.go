package sim

import (
	"context"
	"sort"
	"sync"
	"time"

	"marketchaos/internal/store"
)

// LeaderboardRow is one classified shape's activity over the most recent
// sample window — deltas, not lifetime cumulative totals, since a digest's
// own COUNT_STAR/SUM_* columns never reset on their own (see
// store.DigestSnapshot's doc comment) and a lifetime total would just climb
// forever and stop reflecting "what's happening right now."
type LeaderboardRow struct {
	ShapeID      string  `json:"shapeId"`
	Label        string  `json:"label"`
	Agent        string  `json:"agent"`
	Calls        int64   `json:"calls"`        // COUNT_STAR delta this window
	AvgMs        float64 `json:"avgMs"`        // (SUM_TIMER_WAIT delta / Calls)
	MaxMs        float64 `json:"maxMs"`        // MAX_TIMER_WAIT as-of this sample (not delta — a max resets its own baseline over time)
	RowsExamined int64   `json:"rowsExamined"` // delta
	RowsSent     int64   `json:"rowsSent"`     // delta
	NoIndexUsed  int64   `json:"noIndexUsed"`  // delta
	TmpTables    int64   `json:"tmpTables"`    // delta
	TmpDiskTab   int64   `json:"tmpDiskTables"`
	SortRows     int64   `json:"sortRows"` // delta
}

// leaderboardWindow is how often the digest sampler snapshots and computes
// a fresh window — frequent enough that the dashboard feels live, coarse
// enough that a single slow outlier query doesn't dominate every sample.
const leaderboardWindow = 5 * time.Second

type digestBaseline struct {
	countStar  int64
	sumTimer   time.Duration
	rowsExam   int64
	rowsSent   int64
	tmpTables  int64
	tmpDiskTab int64
	sortRows   int64
	noIndex    int64
}

// leaderboard holds the sampler's rolling state — guarded by its own mutex
// since it's read by API requests and written by the background sampler
// goroutine.
type leaderboard struct {
	mu   sync.RWMutex
	rows []LeaderboardRow

	baseline map[string]digestBaseline // by DIGEST, not shape ID — see sample()
}

func newLeaderboard() *leaderboard {
	return &leaderboard{baseline: map[string]digestBaseline{}}
}

func (l *leaderboard) snapshot() []LeaderboardRow {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LeaderboardRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// runLeaderboardSampler polls performance_schema every leaderboardWindow,
// classifies each digest via shapes.go, aggregates same-shape digests
// together (two literal SQL statements from different agents can normalize
// to the same shape — see shapes.go's doc comment), and computes this
// window's deltas against the previous sample.
func (e *Engine) runLeaderboardSampler(ctx context.Context) {
	tickLoop(ctx, leaderboardWindow, func() {
		octx, cancel := opCtx(ctx)
		snap, err := e.Store.DigestSnapshot(octx)
		cancel()
		if err != nil {
			return
		}
		e.leaderboard.sample(snap)
	})
}

func (l *leaderboard) sample(snap []store.DigestRow) {
	l.mu.Lock()
	defer l.mu.Unlock()

	type accum struct {
		row   LeaderboardRow
		sumMs float64 // total time this window, across every digest folded into this shape — AvgMs is derived from this at the end, not accumulated incrementally
	}
	byShape := map[string]*accum{}
	seen := map[string]bool{}
	for _, d := range snap {
		seen[d.Digest] = true
		shape, ok := ClassifyDigest(d.Text)
		if !ok {
			continue // an unclassified digest (e.g. a learner's own ad-hoc query) — not this app's workload, skip
		}
		prev, hadPrev := l.baseline[d.Digest]
		l.baseline[d.Digest] = digestBaseline{
			countStar: d.CountStar, sumTimer: d.SumTimer,
			rowsExam: d.SumRowsExamined, rowsSent: d.SumRowsSent,
			tmpTables: d.SumCreatedTmpTables, tmpDiskTab: d.SumCreatedTmpDiskTab,
			sortRows: d.SumSortRows, noIndex: d.SumNoIndexUsed,
		}
		if !hadPrev {
			continue // first time seeing this digest — no delta to report yet
		}
		callsDelta := d.CountStar - prev.countStar
		if callsDelta <= 0 {
			continue // no activity this window
		}
		a, ok := byShape[shape.ID]
		if !ok {
			a = &accum{row: LeaderboardRow{ShapeID: shape.ID, Label: shape.Label, Agent: shape.Agent}}
			byShape[shape.ID] = a
		}
		a.row.Calls += callsDelta
		a.sumMs += float64(d.SumTimer-prev.sumTimer) / float64(time.Millisecond)
		if maxMs := float64(d.MaxTimer) / float64(time.Millisecond); maxMs > a.row.MaxMs {
			a.row.MaxMs = maxMs
		}
		a.row.RowsExamined += d.SumRowsExamined - prev.rowsExam
		a.row.RowsSent += d.SumRowsSent - prev.rowsSent
		a.row.NoIndexUsed += d.SumNoIndexUsed - prev.noIndex
		a.row.TmpTables += d.SumCreatedTmpTables - prev.tmpTables
		a.row.TmpDiskTab += d.SumCreatedTmpDiskTab - prev.tmpDiskTab
		a.row.SortRows += d.SumSortRows - prev.sortRows
	}

	// Forget baselines for digests that fell out of the table (performance_schema
	// itself evicts old/rare digests once it hits its configured size) — an
	// evicted-then-reappearing digest should be treated as new, not produce a
	// nonsensical negative delta against a stale baseline.
	for digest := range l.baseline {
		if !seen[digest] {
			delete(l.baseline, digest)
		}
	}

	rows := make([]LeaderboardRow, 0, len(byShape))
	for _, a := range byShape {
		if a.row.Calls > 0 {
			a.row.AvgMs = a.sumMs / float64(a.row.Calls)
		}
		rows = append(rows, a.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].AvgMs*float64(rows[i].Calls) > rows[j].AvgMs*float64(rows[j].Calls)
	})
	l.rows = rows
}
