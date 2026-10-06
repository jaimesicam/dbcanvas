package sim

import (
	"context"
	"math"
	"sync"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// tune.go — finding the hybrid weight instead of guessing it.
//
// Equal weights are a default, not an answer: on this corpus keyword search is weak
// against the articles, so giving it half the say drags hybrid below plain vector
// search (the scoreboard shows exactly that). The sweep asks the question properly —
// for each weight from "keyword only" to "vector only", how often is the top article
// right on tickets the desk has never seen — and draws the curve.

// evalSet is a fixed set of fresh tickets with their answers, embedded once. Seeded,
// so every sweep and every benchmark is scored on the same questions.
type evalItem struct {
	t   corpus.Ticket
	vec []float32
}

func (e *Engine) evalSet(n int) []evalItem {
	e.mu.Lock()
	if len(e.eval) >= n {
		out := e.eval[:n]
		e.mu.Unlock()
		return out
	}
	e.mu.Unlock()
	g := corpus.NewGenerator(424242)
	var ts []corpus.Ticket
	var texts []string
	for i := 0; i < n; i++ {
		t := g.Random()
		ts = append(ts, t)
		texts = append(texts, t.EmbedText())
	}
	vs := e.Model.EmbedBatch(texts)
	out := make([]evalItem, n)
	for i := range ts {
		out[i] = evalItem{ts[i], vs[i]}
	}
	e.mu.Lock()
	e.eval = out
	e.mu.Unlock()
	return out
}

// TunePoint is one weight's result.
type TunePoint struct {
	VectorWeight float64 `json:"vectorWeight"`
	Accuracy     float64 `json:"accuracy"` // top-1 article right
	Ms           float64 `json:"ms"`       // mean latency of the fused query
}

// TuneResult is the curve.
type TuneResult struct {
	Collection    string      `json:"collection"`
	Method        string      `json:"method"`
	Normalization string      `json:"normalization"`
	Queries       int         `json:"queries"`
	Points        []TunePoint `json:"points"`
	Best          TunePoint   `json:"best"`
	Engine        string      `json:"engine"`
	Seconds       float64     `json:"seconds"`
}

// Tune sweeps the vector weight from 0 to 1 for one fusion method — over the KB
// articles (the answer is the top article), or over the resolved tickets (the answer
// is the vote of the top five's articles, as the desk decides).
func (e *Engine) Tune(ctx context.Context, coll, method, normalization string, n int) TuneResult {
	if coll != store.Tickets {
		coll = store.KB
	}
	if n <= 0 || n > 200 {
		n = 80
	}
	t0 := time.Now()
	items := e.evalSet(n)
	base := search.HybridOpts{Method: method, Normalization: normalization}.Normalize()
	out := TuneResult{Collection: coll, Method: base.Method, Normalization: base.Normalization, Queries: n}
	for i := 0; i <= 10; i++ {
		o := base
		o.VectorWeight = float64(i) / 10
		var mu sync.Mutex
		right, total := 0, 0.0
		engine := ""
		work := make(chan evalItem)
		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := range work {
					k := 1
					q := search.VectorQuery{Collection: coll, Vector: it.vec, NumCandidates: 100}
					if coll == store.Tickets {
						k = 5
						q.Filter.Status = []string{"resolved"}
					}
					q.K = k
					r := e.Search.Hybrid(ctx, q, it.t.EmbedText(), coll == store.Tickets, o)
					votes := map[string]int{}
					best, bv := "", 0
					for _, h := range r.Hits {
						votes[h.KB]++
						if votes[h.KB] > bv {
							best, bv = h.KB, votes[h.KB]
						}
					}
					mu.Lock()
					if best == it.t.TruthKB {
						right++
					}
					total += r.Ms
					engine = r.Engine
					mu.Unlock()
				}
			}()
		}
		for _, it := range items {
			work <- it
		}
		close(work)
		wg.Wait()
		p := TunePoint{VectorWeight: o.VectorWeight, Accuracy: float64(right) / float64(len(items)), Ms: total / float64(len(items))}
		out.Points = append(out.Points, p)
		out.Engine = engine
		// Ties go to the weight closest to an even blend: of equally good settings, the one
		// that still listens to both engines is the safer default.
		if i == 0 || p.Accuracy > out.Best.Accuracy || p.Accuracy == out.Best.Accuracy && math.Abs(p.VectorWeight-0.5) < math.Abs(out.Best.VectorWeight-0.5) {
			out.Best = p
		}
	}
	out.Seconds = time.Since(t0).Seconds()
	return out
}
