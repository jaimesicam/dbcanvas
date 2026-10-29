package core

import (
	"context"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Workload is the steady application traffic whose latency the dashboard shows:
// the "what would users notice" view next to the server's own numbers. Each
// worker runs one statement, pauses briefly, and runs the next, so the rate
// stays steady and a stall shows up as latency rather than as a burst.
//
// A statement that never finishes would never be counted, which would make a
// stalled cluster look fast. So the oldest statement still in flight counts as
// a latency sample too: a cluster that has stopped committing shows its p99
// climbing second by second instead of going quiet.
type Workload struct {
	eng Engine

	mu       sync.Mutex
	threads  int
	cancel   []context.CancelFunc
	lat      []float64 // ms, completed this second
	reads    int
	writes   int
	errs     int
	inflight map[int]time.Time
	nextID   int
	lastErr  string
}

const (
	defaultThreads = 8
	maxThreads     = 64
	opPause        = 5 * time.Millisecond
	writeShare     = 0.3
)

func newWorkload(e Engine) *Workload {
	return &Workload{eng: e, inflight: map[int]time.Time{}}
}

// SetThreads changes the worker count while it runs.
func (w *Workload) SetThreads(ctx context.Context, n int) {
	if n < 0 {
		n = 0
	}
	if n > maxThreads {
		n = maxThreads
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.cancel) < n {
		c, cancel := context.WithCancel(ctx)
		w.cancel = append(w.cancel, cancel)
		go w.worker(c)
	}
	for len(w.cancel) > n {
		w.cancel[len(w.cancel)-1]()
		w.cancel = w.cancel[:len(w.cancel)-1]
	}
	w.threads = n
}

func (w *Workload) Threads() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threads
}

func (w *Workload) worker(ctx context.Context) {
	for ctx.Err() == nil {
		write := rand.Float64() < writeShare
		w.mu.Lock()
		id := w.nextID
		w.nextID++
		w.inflight[id] = time.Now()
		w.mu.Unlock()

		start := time.Now()
		opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := w.eng.Op(opCtx, write, rand.IntN(1<<30))
		cancel()
		ms := float64(time.Since(start).Microseconds()) / 1000

		w.mu.Lock()
		delete(w.inflight, id)
		if ctx.Err() == nil {
			if err != nil {
				w.errs++
				w.lastErr = err.Error()
			} else {
				w.lat = append(w.lat, ms)
				if write {
					w.writes++
				} else {
					w.reads++
				}
			}
		}
		w.mu.Unlock()
		if err != nil {
			// Do not spin on a target that refuses everything.
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		sleepCtx(ctx, opPause)
	}
}

// take returns the last second's numbers and starts the next second.
func (w *Workload) take(elapsed time.Duration) (wq, rq, p50, p99, errs float64) {
	w.mu.Lock()
	lat := w.lat
	reads, writes, e := w.reads, w.writes, w.errs
	w.lat, w.reads, w.writes, w.errs = nil, 0, 0, 0
	var oldest float64
	for _, t := range w.inflight {
		if ms := float64(time.Since(t).Microseconds()) / 1000; ms > oldest {
			oldest = ms
		}
	}
	w.mu.Unlock()

	sec := elapsed.Seconds()
	if sec <= 0 {
		sec = 1
	}
	sort.Float64s(lat)
	p50, p99 = pct(lat, .5), pct(lat, .99)
	if oldest > p99 {
		p99 = oldest
	}
	if len(lat) == 0 && oldest > 0 {
		p50 = oldest
	}
	return float64(writes) / sec, float64(reads) / sec, p50, p99, float64(e) / sec
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
