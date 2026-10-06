package sim

import (
	"context"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// backend.go — the one seam between the desk and the database it runs on.
//
// The desk's logic is the same on every engine: embed the ticket, find the closest
// resolved tickets, vote, merge, raise an incident. What differs is how a vector is
// stored, indexed and searched — MongoDB hands $vectorSearch to a separate mongot
// process; PostgreSQL with pgvector does it inside the executor, in the same
// transaction as the write. Each engine implements this interface, and the dashboard
// shows each one's own query language, so the comparison is between real things.

// TicketRecord is a ticket as it is written.
type TicketRecord struct {
	ID           int64
	T            corpus.Ticket
	Vec          []float32
	Created      time.Time
	Status       string
	Team         string
	ResolvedKB   string // set for history written already resolved
	ResolvedBy   string
	SuggestedKB  string
	SuggestedCos float64
	DuplicateOf  any
}

// TicketSet is a change to a ticket's state.
type TicketSet struct {
	Status     string
	Team       string
	ResolvedKB string
	Resolved   bool // stamp resolved_at now
	Incident   int
}

// SearchState is whether native vector search is usable, and the indexes behind it.
type SearchState struct {
	Ready    bool
	Indexes  []store.IndexStatus
	Err      string // why there is no native search, when there is none
	Building string // what is still being built, while it is
}

// Searcher answers the three kinds of query.
type Searcher interface {
	Vector(ctx context.Context, q search.VectorQuery) search.Result
	Keyword(ctx context.Context, coll, query string, k int, resolvedOnly bool) search.Result
	Hybrid(ctx context.Context, q search.VectorQuery, query string, resolvedOnly bool, o search.HybridOpts) search.Result
	Ready() bool
	SetReady(bool)
}

// Backend is the database the desk runs on.
type Backend interface {
	Engine() string // mongodb | postgres
	Info(ctx context.Context) store.ServerInfo
	Prepare(ctx context.Context) error
	SaveArticle(ctx context.Context, a corpus.Article, vec []float32) error
	TicketCount(ctx context.Context) (int64, error)
	NextTicketNumber(ctx context.Context) (int64, error)
	InsertTickets(ctx context.Context, recs []TicketRecord) error
	LoadTickets(ctx context.Context, put func(*search.Doc)) error
	UpdateTicket(ctx context.Context, id any, set TicketSet) error
	TicketArchetype(ctx context.Context, id any) string
	Prune(ctx context.Context, keep int64) []any
	EnsureSearch(ctx context.Context) SearchState
	Searcher() Searcher
	Freshness(ctx context.Context, vec []float32, text string) Freshness
	Workshop() WorkshopBackend
}

// WorkshopBackend is the Index Workshop's engine-specific half: which index variants
// exist and how they are built, benchmarked, explained and watched.
type WorkshopBackend interface {
	Variants() []Variant
	Detect(ctx context.Context) (ready bool, docs int)
	Build(ctx context.Context, docs []search.Doc, progress func(phase, detail string)) error
	Bench(ctx context.Context, items []evalItem, p BenchParams) (BenchResult, error)
	PlaygroundActions() []PlaygroundAction
	PlaygroundDo(ctx context.Context, id string) error
	PlaygroundProbe(ctx context.Context, vec []float32) PGEvent
	Explain(ctx context.Context, r ExplainRequest, vec []float32) ExplainResult
	Metrics(ctx context.Context) MetricsView
}

// BenchParams are the benchmark's knobs. NumCandidates is mongot's; EfSearch and
// Probes are pgvector's HNSW and IVFFlat search-time settings.
type BenchParams struct {
	K             int `json:"k"`
	NumCandidates int `json:"numCandidates"`
	EfSearch      int `json:"efSearch"`
	Probes        int `json:"probes"`
	Queries       int `json:"queries"`
}
