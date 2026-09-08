package sim

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"stocksim/internal/store"
)

// The working-set agent exists because a big database is not, by itself, a busy
// one.
//
// The backfill agent next door will grow this app's dataset to five gigabytes.
// Left there, nothing reads those five gigabytes. Every other agent works out
// of the seed universe — twenty securities, four portfolios, a few hundred open
// orders — which is a few hundred kilobytes and sits in any buffer pool
// permanently. So a deployment with a 128 MiB buffer pool and a 5 GiB dataset
// reports a ~100% buffer pool hit rate and reads almost nothing from disk,
// which is a true measurement of a workload nobody would ever run, and reads as
// "the buffer pool size does not matter". It matters enormously; the workload
// was just never asking.
//
// This agent asks. It keeps a *working set* — by default half the dataset —
// under continuous random read, so the pages a query needs are usually not the
// pages the cache is holding. That is the whole trick: not more data, and not
// more reads, but reads spread over more data than the cache can hold. On the
// 5 GiB / 128 MiB deployment above, the working set is 2.5 GiB against a cache
// that can hold one page in twenty, and InnoDB's buffer pool reads go from
// nothing to the disk's limit.
//
// The read itself is a price-history query — "the last few hundred ticks for
// this symbol, as of some moment in the past" — because it is both the query a
// stock application would really run and, on every engine here, an index seek
// followed by a few hundred random row lookups. See each store's TicksBefore.

const (
	// workingSetRowsPerRead is how many ticks one random read asks for. Large
	// enough that the row lookups dominate the index dive, small enough that a
	// single read cannot hold a connection for long.
	workingSetRowsPerRead = 200
	// workingSetRefresh is how often the window is recomputed. The dataset is
	// growing underneath it while the backfill runs, so the working set has to
	// keep growing with it, but neither its size nor the measurement is worth
	// doing more often than this.
	workingSetRefresh = 15 * time.Second
	// workingSetIdlePoll is the pause when there is nothing to read — paused,
	// or no history written yet.
	workingSetIdlePoll = 2 * time.Second
)

// DefaultWorkingSetPct is the share of the dataset kept hot when nothing is
// configured. Half is a deliberate middle: large enough that no plausible lab
// buffer pool holds it, small enough to still be a *set* — a workload that
// touches 100% of its data uniformly has no locality at all, and real ones
// always have some.
const DefaultWorkingSetPct = 0.5

// WorkingSet is how much of the dataset to keep under continuous read, written
// either as a share of it or as an absolute size. Exactly one of the two is
// set; the zero value means the feature is off.
type WorkingSet struct {
	// Pct is a share of the dataset, 0 < Pct <= 1.
	Pct float64
	// Bytes is an absolute size, converted to a share against the dataset's
	// measured footprint at read time — asking for "2G hot" is only meaningful
	// relative to how much there is.
	Bytes int64
}

// DefaultWorkingSet is what an unconfigured deployment gets.
func DefaultWorkingSet() WorkingSet { return WorkingSet{Pct: DefaultWorkingSetPct} }

// Off reports whether no working set was asked for.
func (w WorkingSet) Off() bool { return w.Pct <= 0 && w.Bytes <= 0 }

// shareOf resolves the configuration to a fraction of a measured footprint,
// clamped to (0, 1]. An absolute size larger than the dataset means all of it.
func (w WorkingSet) shareOf(footprint int64) float64 {
	switch {
	case w.Off():
		return 0
	case w.Bytes > 0:
		if footprint <= 0 || w.Bytes >= footprint {
			return 1
		}
		return float64(w.Bytes) / float64(footprint)
	case w.Pct >= 1:
		return 1
	}
	return w.Pct
}

// String renders the configuration the way it was asked for.
func (w WorkingSet) String() string {
	switch {
	case w.Off():
		return "off"
	case w.Bytes > 0:
		return FormatBytes(w.Bytes)
	}
	return fmt.Sprintf("%.0f%% of the dataset", w.Pct*100)
}

// ParseWorkingSet reads a working-set size the way a person writes one: "50%",
// "0.5", "2.5G", or "off". A bare number of 1 or less is a share, anything
// larger is a byte count — nobody means one byte, and everybody who writes
// "0.25" means a quarter.
//
// An empty string means "not set" and returns the default rather than nothing,
// because the default is the useful behaviour and having to know a magic word
// to get it would be a trap. "off"/"none"/"0" turn it off.
//
// app/stocksim.go carries a mirror of this, so a bad value is rejected on the
// canvas rather than inside an already-deployed container.
func ParseWorkingSet(s string) (WorkingSet, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return DefaultWorkingSet(), nil
	case strings.EqualFold(s, "off"), strings.EqualFold(s, "none"):
		return WorkingSet{}, nil
	}
	if pct, ok := strings.CutSuffix(s, "%"); ok {
		n, err := strconv.ParseFloat(strings.TrimSpace(pct), 64)
		if err != nil || n < 0 || n > 100 {
			return WorkingSet{}, fmt.Errorf(
				"%q is not a working set — write it like 50%%, 0.5 or 2G", s)
		}
		if n == 0 {
			return WorkingSet{}, nil
		}
		return WorkingSet{Pct: n / 100}, nil
	}
	// A bare decimal in (0,1] is a share; everything else goes through the same
	// size parser the dataset target uses, so "2G" and "2Gi" mean there what
	// they mean there.
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		switch {
		case n == 0:
			return WorkingSet{}, nil
		case n < 0:
			return WorkingSet{}, fmt.Errorf("%q is negative", s)
		case n <= 1:
			return WorkingSet{Pct: n}, nil
		}
	}
	n, err := ParseSize(s)
	if err != nil {
		return WorkingSet{}, fmt.Errorf(
			"%q is not a working set — write it like 50%%, 0.5 or 2G", s)
	}
	if n == 0 {
		return WorkingSet{}, nil
	}
	return WorkingSet{Bytes: n}, nil
}

// WorkingSetStatus is what the dashboard shows about all this — reported even
// when the agent is off, because "off, and here is why" is the answer to the
// question a user asks when a small buffer pool makes no difference.
type WorkingSetStatus struct {
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
	Active    bool `json:"active"`
	Threads   int  `json:"threads"`
	// Bytes is how much of the dataset is being kept hot, and DatasetBytes what
	// there is in total — the ratio between them, next to the server's cache
	// size, is the number that predicts the hit rate.
	Bytes         int64   `json:"bytes"`
	DatasetBytes  int64   `json:"datasetBytes"`
	WindowSeconds int64   `json:"windowSeconds"`
	Reads         int64   `json:"reads"`
	RowsRead      int64   `json:"rowsRead"`
	ReadsPerSec   float64 `json:"readsPerSec"`
	RowsPerSec    float64 `json:"rowsPerSec"`
	Note          string  `json:"note"`
}

func (e *Engine) setWorkingSet(s WorkingSetStatus) {
	e.mu.Lock()
	e.workingSet = s
	e.mu.Unlock()
}

// WorkingSetStatus returns the current status for BuildSnapshot.
func (e *Engine) WorkingSetStatus() WorkingSetStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.workingSet
}

// workingSetReadsPerSecond is the total random-read rate at each level. Zero
// means unthrottled, and High is zero on purpose: the point of High is to find
// out what the storage does when it is asked for more than the cache can hold,
// and a rate limit there would be measuring the rate limit. The lower two
// levels are a deliberate trickle — enough that the panel is honest and a
// baseline hit rate exists to compare against, little enough that a machine
// running six other sims is not brought to its knees by this one.
var workingSetReadsPerSecond = map[string]float64{
	LevelLow:    2,
	LevelMedium: 20,
	LevelHigh:   0,
}

// window is the slice of history the readers hit, and the securities they may
// hit it for. Recomputed by the supervisor; read by every reader.
type window struct {
	secs  []store.Security
	from  time.Time
	to    time.Time
	bytes int64 // how much data the window covers, approximately
	total int64 // the whole dataset's measured footprint
}

func (w window) ready() bool { return len(w.secs) > 0 && !w.to.IsZero() }

// pick chooses a random security and a random moment inside the window.
func (w window) pick(rnd *rand.Rand) (store.Security, time.Time) {
	s := w.secs[rnd.Intn(len(w.secs))]
	span := w.to.Sub(w.from)
	if span <= 0 {
		return s, w.to
	}
	return s, w.from.Add(time.Duration(rnd.Int63n(int64(span))))
}

// runWorkingSetAgent supervises the readers: it recomputes the window, keeps
// the status blob current, and lets Threads reader goroutines hammer away in
// between. Unlike the other agents this one is not a tickLoop, because its work
// is not periodic — the readers run continuously and only the bookkeeping is on
// a clock.
func (e *Engine) runWorkingSetAgent(ctx context.Context) {
	engine := e.Store.Engine()
	threads := store.ClampThreads(e.Threads)

	// The same engines that can be grown are the ones that can have a working
	// set: Valkey's tick history is a capped stream a few hundred entries deep,
	// so there is no cold data to pull in and no cache to miss.
	if !store.CanGrowToSize(engine) {
		e.setWorkingSet(WorkingSetStatus{Note: "not available for " +
			engineDisplayName(engine) + " — its tick history is a capped stream"})
		e.heartbeat(ctx, "workingset", "idle", "not available for this engine")
		return
	}
	if e.WorkingSet.Off() {
		e.setWorkingSet(WorkingSetStatus{Supported: true, Threads: threads,
			Note: "no working set configured — the dataset is written but never read back"})
		e.heartbeat(ctx, "workingset", "idle", "no working set configured")
		return
	}

	var (
		mu  sync.RWMutex
		cur window
	)
	readWindow := func() window {
		mu.RLock()
		defer mu.RUnlock()
		return cur
	}

	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			e.workingSetReader(ctx, readWindow, threads, rand.New(rand.NewSource(seed)))
		}(time.Now().UnixNano() + int64(i)*7919)
	}
	defer wg.Wait()

	var (
		lastReads, lastRows int64
		lastAt              = time.Now()
	)
	for {
		w, note, err := e.computeWindow(ctx)
		if err != nil {
			// Keep reading the window we already have: a failed measurement is
			// usually the database being briefly busy, and stopping the readers
			// over it would be the wrong reaction to the load we caused.
			e.noteErr("workingset: measure", err)
			e.heartbeat(ctx, "workingset", "error", err.Error())
			w, note = readWindow(), "cannot measure the dataset: "+err.Error()
		} else {
			mu.Lock()
			cur = w
			mu.Unlock()
		}

		reads, rows := e.counters.wsReads.Load(), e.counters.wsRows.Load()
		elapsed := time.Since(lastAt).Seconds()
		st := WorkingSetStatus{
			Supported: true, Enabled: true, Threads: threads,
			Bytes: w.bytes, DatasetBytes: w.total,
			WindowSeconds: int64(w.to.Sub(w.from).Seconds()),
			Reads:         reads, RowsRead: rows,
			Active: e.Running() && w.ready(),
			Note:   note,
		}
		if elapsed > 0 {
			st.ReadsPerSec = float64(reads-lastReads) / elapsed
			st.RowsPerSec = float64(rows-lastRows) / elapsed
		}
		lastReads, lastRows, lastAt = reads, rows, time.Now()
		e.setWorkingSet(st)
		if err == nil {
			e.heartbeat(ctx, "workingset", "ok", fmt.Sprintf("%s hot of %s, %.0f reads/s",
				FormatBytes(w.bytes), FormatBytes(w.total), st.ReadsPerSec))
		}

		if !sleepCtx(ctx, workingSetRefresh) {
			return
		}
	}
}

// workingSetReader is one reader goroutine: pick a random point in the window,
// read the history around it, repeat. Pacing is per-reader rather than through
// a shared token bucket — dividing the level's rate by the thread count gets
// the same aggregate without a channel every read has to queue on.
func (e *Engine) workingSetReader(ctx context.Context, get func() window, threads int, rnd *rand.Rand) {
	for {
		if ctx.Err() != nil {
			return
		}
		w := get()
		if !e.Running() || !w.ready() {
			if !sleepCtx(ctx, workingSetIdlePoll) {
				return
			}
			continue
		}

		sec, at := w.pick(rnd)
		ticks, err := e.Store.TicksBefore(ctx, sec.ID, at, workingSetRowsPerRead)
		if err != nil {
			e.noteErr("workingset: read history", err)
			if !sleepCtx(ctx, workingSetIdlePoll) {
				return
			}
			continue
		}
		e.counters.wsReads.Add(1)
		e.counters.wsRows.Add(int64(len(ticks)))

		if rate := workingSetReadsPerSecond[e.Level()]; rate > 0 {
			if !sleepCtx(ctx, time.Duration(float64(threads)/rate*float64(time.Second))) {
				return
			}
		}
	}
}

// computeWindow decides which slice of history is hot, and returns the sentence
// the dashboard shows about it.
//
// The window is the newest share of the tick history, sized by time. Time is a
// proxy for bytes here, and a good one: ticks are the overwhelming majority of
// this dataset and they are written at a fixed cadence — one per security per
// second, by both the live price agent and the backfill — so half the time span
// really is about half the bytes. Recent-history-is-hot is also the access
// pattern a market application actually has, which makes the resulting read
// profile something more than a synthetic scatter.
func (e *Engine) computeWindow(ctx context.Context) (window, string, error) {
	secs, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 500})
	if err != nil {
		return window{}, "", err
	}
	if len(secs) == 0 {
		return window{}, "no securities to read history for", nil
	}
	total, err := e.measureFootprint(ctx)
	if err != nil {
		return window{}, "", err
	}
	// One security's span stands for all of them: every writer here covers all
	// securities in the same pass, so their histories start and end together,
	// and asking twenty times would cost twenty index seeks to learn the same
	// thing.
	oldest, newest, err := e.Store.TickSpan(ctx, secs[0].ID)
	if err != nil {
		return window{}, "", err
	}
	if newest.IsZero() {
		return window{secs: secs, total: total}, "no price history written yet", nil
	}

	share := e.WorkingSet.shareOf(total)
	span := newest.Sub(oldest)
	w := window{
		secs:  secs,
		from:  newest.Add(-time.Duration(float64(span) * share)),
		to:    newest,
		bytes: int64(float64(total) * share),
		total: total,
	}
	note := fmt.Sprintf("%s of %s kept hot (%.0f%%), %s of history, %d threads",
		FormatBytes(w.bytes), FormatBytes(total), share*100,
		compactDuration(w.to.Sub(w.from)), store.ClampThreads(e.Threads))
	return w, note, nil
}

func compactDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f minutes", d.Minutes())
	}
	return fmt.Sprintf("%.0f seconds", d.Seconds())
}
