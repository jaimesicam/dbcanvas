package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/mongotmetrics"
	"supportsim/internal/search"
	"supportsim/internal/store"

	"go.mongodb.org/mongo-driver/bson"
)

// workshop.go — the Index Workshop: the choices you make when you *define* a vector
// index, measured instead of described. Everything here runs on its own copy of the
// resolved tickets (vector_variants), never on the desk's own indexes. This file is
// the engine-neutral half — building the copy, the fixed questions, the timelines;
// each backend's WorkshopBackend supplies the indexes, queries and explanations.

type Variant struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Similarity   string `json:"similarity"`
	Quantization string `json:"quantization,omitempty"`
	Storage      string `json:"storage"`
	Lesson       string `json:"lesson"`
	int8Query    bool   // the query vector must be int8 BinData too
	ddl          string // PostgreSQL: the SQL that builds it
}

// variantDocs is how many tickets the comparison searches.
const variantDocs = 2000

// mongoVariants are the nine indexes the workshop compares on MongoDB. The first is the baseline the
// others are measured against.
// rawScale gives document id a stable random length between 0.2 and 5 (log-uniform),
// for the un-normalized variants.
func rawScale(id int64) float32 {
	r := rand.New(rand.NewSource(id*7919 + 13))
	return float32(math.Exp((r.Float64()*2 - 1) * math.Log(5)))
}

// ---------------------------------------------------------------- build

// WorkshopState is the variant collection's build state.
type WorkshopState struct {
	Phase   string    `json:"phase"` // idle | copying | indexing | ready | error
	Detail  string    `json:"detail"`
	Docs    int       `json:"docs"`
	Started time.Time `json:"started"`
	Took    float64   `json:"took"`
}

type workshop struct {
	mu    sync.Mutex
	state WorkshopState
	pg    *playground
}

// WorkshopStatus returns the build state.
func (e *Engine) WorkshopStatus() WorkshopState {
	e.ws.mu.Lock()
	defer e.ws.mu.Unlock()
	return e.ws.state
}

func (e *Engine) wsSet(phase, detail string) {
	e.ws.mu.Lock()
	e.ws.state.Phase, e.ws.state.Detail = phase, detail
	if phase == "ready" || phase == "error" {
		e.ws.state.Took = time.Since(e.ws.state.Started).Seconds()
	}
	e.ws.mu.Unlock()
}

// BuildVariants (re)creates vector_variants and its nine indexes in the background.
func (e *Engine) BuildVariants() error {
	if !e.Search.Ready() {
		return fmt.Errorf("the workshop compares real search indexes, which needs mongot — turn on Vector search for this cluster")
	}
	e.ws.mu.Lock()
	if p := e.ws.state.Phase; p == "copying" || p == "indexing" {
		e.ws.mu.Unlock()
		return fmt.Errorf("a build is already running")
	}
	e.ws.state = WorkshopState{Phase: "copying", Started: time.Now()}
	e.ws.mu.Unlock()
	go e.buildVariants(context.Background())
	return nil
}
func (e *Engine) buildVariants(ctx context.Context) {
	var src []search.Doc
	for _, d := range e.Mirror.Snapshot(store.Tickets) {
		if d.Status == "resolved" && len(d.Vec) == store.Dims && len(src) < variantDocs {
			src = append(src, d)
		}
	}
	// Approximate and exact search only part ways once there is enough to search: on a
	// few hundred vectors HNSW visits nearly all of them anyway. A young desk is topped
	// up with generated resolved tickets (negative ids, so they never collide).
	if missing := variantDocs - len(src); missing > 0 {
		e.wsSet("copying", fmt.Sprintf("embedding %d more resolved tickets so the comparison has %d to search", missing, variantDocs))
		g := corpus.NewGenerator(777)
		var ts []corpus.Ticket
		var texts []string
		for i := 0; i < missing; i++ {
			t := g.Random()
			ts = append(ts, t)
			texts = append(texts, t.EmbedText())
		}
		vecs := e.Model.EmbedBatch(texts)
		for i, t := range ts {
			src = append(src, search.Doc{ID: int64(-(i + 1)), Title: t.Subject, Team: t.TruthTeam, KB: t.TruthKB, Status: "resolved", Vec: vecs[i]})
		}
	}
	if err := e.B.Workshop().Build(ctx, src, e.wsSet); err != nil {
		e.wsSet("error", err.Error())
		return
	}
	e.ws.mu.Lock()
	e.ws.state.Docs = len(src)
	e.ws.mu.Unlock()
	e.wsSet("ready", fmt.Sprintf("%d indexes over %d documents", len(e.B.Workshop().Variants()), len(src)))
}

// detectVariants picks up a variant collection built before a restart, so the
// workshop does not ask for a rebuild it does not need.
func (e *Engine) detectVariants(ctx context.Context) {
	ready, n := e.B.Workshop().Detect(ctx)
	if !ready {
		return
	}
	e.ws.mu.Lock()
	e.ws.state = WorkshopState{Phase: "ready", Detail: fmt.Sprintf("%d indexes over %d documents (built earlier)", len(e.B.Workshop().Variants()), n), Docs: n}
	e.ws.mu.Unlock()
}

// ---------------------------------------------------------------- benchmark

// BenchRow is one variant's result.
type BenchRow struct {
	Variant
	Shell       string  `json:"shell"`
	Recall      float64 `json:"recall"`   // vs exact cosine on v_cosine
	Accuracy    float64 `json:"accuracy"` // top-5 neighbours' vote picks the right article
	P50Ms       float64 `json:"p50Ms"`
	MeanMs      float64 `json:"meanMs"`
	TopScore    float64 `json:"topScore"` // the first query's best score, to show the scale
	FieldBytes  float64 `json:"fieldBytes"`
	IndexBytes  float64 `json:"indexBytes"`
	MeanLength  float64 `json:"meanLength,omitempty"` // un-normalized only: mean |v| of the hits
	Error       string  `json:"error,omitempty"`
	Description string  `json:"description,omitempty"`
	BuildMs     float64 `json:"buildMs,omitempty"` // PostgreSQL: how long CREATE INDEX took
	Query       string  `json:"query,omitempty"`   // PostgreSQL: the benchmark's query, as run
}

// BenchResult is the whole table.
type BenchResult struct {
	Queries       int        `json:"queries"`
	K             int        `json:"k"`
	NumCandidates int        `json:"numCandidates"`
	Docs          int        `json:"docs"`
	Rows          []BenchRow `json:"rows"`
	Seconds       float64    `json:"seconds"`
	Example       string     `json:"example"` // the first query, for the score column
	EfSearch      int        `json:"efSearch,omitempty"`
	Probes        int        `json:"probes,omitempty"`
}

// Bench asks every variant the same questions.// Bench asks every variant the same questions.
func (e *Engine) Bench(ctx context.Context, p BenchParams) (BenchResult, error) {
	st := e.WorkshopStatus()
	if st.Phase != "ready" {
		return BenchResult{}, fmt.Errorf("build the variant indexes first")
	}
	if p.K <= 0 || p.K > 50 {
		p.K = 10
	}
	if p.NumCandidates < p.K {
		p.NumCandidates = p.K
	}
	if p.EfSearch < p.K {
		p.EfSearch = max(40, p.K)
	}
	if p.Probes <= 0 {
		p.Probes = 1
	}
	if p.Queries <= 0 || p.Queries > 200 {
		p.Queries = 60
	}
	res, err := e.B.Workshop().Bench(ctx, e.evalSet(p.Queries), p)
	res.Docs = st.Docs
	return res, err
}

// ---------------------------------------------------------------- explain

// ExplainRequest is the Explain panel's query.
type ExplainRequest struct {
	Query         string `json:"query"`
	Index         string `json:"index"` // a variant, or "tickets_vector"
	K             int    `json:"k"`
	NumCandidates int    `json:"numCandidates"`
	Exact         bool   `json:"exact"`
	Team          string `json:"team"`
	Probes        int    `json:"probes"` // PostgreSQL IVFFlat
}

// KV is a labelled value for the page.
type KV struct {
	K string `json:"k"`
	V string `json:"v"`
}

// Segment is one Lucene segment's share of a vector query.
type Segment struct {
	ID       string  `json:"id"`
	Mode     string  `json:"mode"` // Approximate | Exact
	Docs     float64 `json:"docs"`
	Visited  float64 `json:"visited"`
	Ms       float64 `json:"ms"`
	Filtered float64 `json:"filtered,omitempty"`
}

// ExplainResult is the summary the panel draws, plus the raw explain.
type ExplainResult struct {
	Pipeline      string    `json:"pipeline"`
	MongotVersion string    `json:"mongotVersion"`
	TotalDocs     float64   `json:"totalDocs"`
	Segments      []Segment `json:"segments"`
	Visited       float64   `json:"visited"`
	Collected     float64   `json:"collected"` // documents scored by the collector
	QueryType     string    `json:"queryType"` // e.g. InstrumentableKnnFloatVectorQuery, ExactVectorSearchQuery
	QueryMs       float64   `json:"queryMs"`
	CollectorMs   float64   `json:"collectorMs"`
	Raw           string    `json:"raw"`
	Error         string    `json:"error,omitempty"`
	Plan          string    `json:"plan,omitempty"`    // PostgreSQL: EXPLAIN ANALYZE's text
	Summary       []KV      `json:"summary,omitempty"` // PostgreSQL: the plan's essentials
}

// Explain runs explain("executionStats") on a $vectorSearch and pulls out what HNSW
// did: per segment, approximate or exact, how many vectors it visited of how many.// Explain embeds the query and asks the backend to explain its vector search.
func (e *Engine) Explain(ctx context.Context, r ExplainRequest) ExplainResult {
	return e.B.Workshop().Explain(ctx, r, e.Model.Embed(r.Query))
}

// ---------------------------------------------------------------- metrics

// IndexRow is one search index on the workshop's index table.
type IndexRow struct {
	Collection string  `json:"collection"`
	Name       string  `json:"name"`
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	Queryable  bool    `json:"queryable"`
	Version    any     `json:"version"`
	Definition string  `json:"definition"`
	SizeBytes  float64 `json:"sizeBytes"`
	Docs       float64 `json:"docs"`
	LagMs      float64 `json:"lagMs"`
	Mongot     string  `json:"mongotStatus,omitempty"`
}

// MetricsView is the Workshop's live panel: every search index in the database with
// mongot's numbers joined on, and each mongot's summary.
type MetricsView struct {
	Engine string `json:"engine"`
	// PG is PostgreSQL's panel (pgvector indexes, their use and caching, settings).
	PG        *PGMetrics              `json:"pg,omitempty"`
	Indexes   []IndexRow              `json:"indexes"`
	Mongots   []mongotmetrics.Summary `json:"mongots"`
	Endpoints []string                `json:"endpoints"`
}

// Metrics builds the Workshop's live panel.
func (e *Engine) Metrics(ctx context.Context) MetricsView {
	v := e.B.Workshop().Metrics(ctx)
	v.Engine = e.B.Engine()
	return v
}

// ---------------------------------------------------------------- helpers

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case bson.M: // a Long that came through JSON as {high, low}
		return toF(x["low"]) + toF(x["high"])*4294967296
	}
	return 0
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// PGMetrics is the PostgreSQL panel; defined with the PostgreSQL backend.
