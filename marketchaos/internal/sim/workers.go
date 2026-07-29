package sim

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// tickLoop runs fn every interval until ctx is done — the rate-driven half
// of the hybrid concurrency model (see engine.go): market data, news,
// scanners, compliance, cleanup, and dashboard-poll all use this.
func tickLoop(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func newAgentRand() *rand.Rand { return rand.New(rand.NewSource(time.Now().UnixNano())) }

// agentOpTimeout bounds a single agent operation's total time, including any
// wait for a free connection from the pool — found live (stage S2
// verification): under Extreme traffic with a contention-heavy mix, a
// rate-driven agent using the long-lived agent context (no deadline) for its
// query stalled for tens of seconds waiting on a pool saturated by
// institutional-trader/matching-engine transactions, and looked frozen on
// the dashboard rather than erroring. opCtx makes a saturated pool fail an
// operation fast (counted as an error) instead of blocking a whole agent
// goroutine.
//
// 15s, not the original 5s: also found live, on a real PXC target — a
// genuine, correctly-working cross-node Galera certification conflict
// (exactly what pxc-conflict-heavy mix exists to produce) can legitimately
// hold a COMMIT in "wsrep: replicating and certifying write set" for several
// real seconds under heavy contention; that is not a hang. A too-short
// timeout was cutting those connections off mid-COMMIT, and each cancel+
// reopen cycle contributed to a burst of client-side connection churn that
// (combined with the pool caps below) transiently oversubscribed a PXC
// node's own max_connections faster than it could reclaim the abandoned
// sockets — a real "too many connections" incident against a live 3-node
// cluster, not a hypothetical.
const agentOpTimeout = 15 * time.Second

func opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, agentOpTimeout)
}

func detailStr(events, errs int64) string {
	return fmt.Sprintf("events=%d errs=%d", events, errs)
}

// workerPool runs a variable number of goroutines executing the same loop
// function, resized live as traffic level changes — the concurrency-
// sensitive half of the hybrid model. Unlike a ticker-driven agent's
// per-tick batch, these goroutines can have genuinely simultaneous open
// transactions, which the locking/PXC challenges need to reproduce real
// contention (a serial per-tick loop can't).
type workerPool struct {
	mu      sync.Mutex
	cancels []context.CancelFunc
	wg      sync.WaitGroup
	// loop is called once per worker slot with that slot's index (stable for
	// the worker's lifetime — used by the institutional-trader agent to pin a
	// worker to one PXC member via MemberPool.At(index) rather than
	// round-robining per order) and must run until ctx is done.
	loop func(ctx context.Context, workerIndex int)
}

func newWorkerPool(loop func(ctx context.Context, workerIndex int)) *workerPool {
	return &workerPool{loop: loop}
}

// Resize grows or shrinks the live goroutine count to n relative to parent —
// each worker gets its own child context, so shrinking cancels only the
// excess workers rather than tearing down and rebuilding the whole pool on
// every traffic-level change.
func (p *workerPool) Resize(parent context.Context, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.cancels) < n {
		idx := len(p.cancels)
		wctx, cancel := context.WithCancel(parent)
		p.cancels = append(p.cancels, cancel)
		p.wg.Add(1)
		go func(ctx context.Context, i int) {
			defer p.wg.Done()
			p.loop(ctx, i)
		}(wctx, idx)
	}
	for len(p.cancels) > n {
		last := len(p.cancels) - 1
		p.cancels[last]()
		p.cancels = p.cancels[:last]
	}
}

func (p *workerPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cancels)
}

// Stop cancels every worker and waits for them to exit.
func (p *workerPool) Stop() {
	p.mu.Lock()
	for _, c := range p.cancels {
		c()
	}
	p.cancels = nil
	p.mu.Unlock()
	p.wg.Wait()
}

// jitterSleep pauses a worker-pool loop iteration for a randomized interval
// around base — spreads concurrent workers' operations out in time rather
// than lock-stepping them, which would otherwise manufacture artificial
// contention bursts that have nothing to do with the traffic level itself.
func jitterSleep(ctx context.Context, rng *rand.Rand, base time.Duration) {
	d := base/2 + time.Duration(rng.Int63n(int64(base)))
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
