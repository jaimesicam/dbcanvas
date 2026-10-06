// Package search builds and runs the three kinds of query the desk compares:
// keyword ($search, Lucene BM25 in mongot), vector ($vectorSearch, approximate
// nearest neighbours in mongot) and hybrid ($rankFusion of the two).
//
// Every query is built as a plain aggregation pipeline first and run second, and
// the pipeline is returned with the results, rendered as mongosh text. That is the
// point of the package: the dashboard never shows a result without the exact
// pipeline that produced it, so anything on screen can be pasted into a shell and
// reproduced.
//
// When the cluster has no mongot the same questions are answered in the app — a
// brute-force cosine scan over an in-memory mirror for vectors, the classic $text
// index for keywords — and the result says so ("engine": "app"). The page keeps
// working, and the difference is the lesson: that is what you would be writing and
// operating yourself without a vector index.
package search

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"supportsim/internal/embed"
	"supportsim/internal/store"
)

// Hit is one search result, from either collection.
type Hit struct {
	ID        any       `json:"id"`
	Title     string    `json:"title"`
	Team      string    `json:"team"`
	Status    string    `json:"status,omitempty"`
	KB        string    `json:"kb,omitempty"` // article slug: the article itself, or the one a ticket was resolved with
	Plan      string    `json:"plan,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	Score     float64   `json:"score"`
	// Cosine is the cosine similarity to the query, for a vector hit — whatever the
	// engine's own score is (mongot's (1+cos)/2, pgvector's distance). The desk's
	// thresholds are written in cosine, so every engine fills this in.
	Cosine float64 `json:"cosine,omitempty"`
	// Parts is how a hybrid score was made: each input pipeline's rank or raw score,
	// its weight, and what it contributed — the server's scoreDetails, or the app's.
	Parts []Part `json:"parts,omitempty"`
}

// Part is one input pipeline's share of a fused score.
type Part struct {
	Pipeline string  `json:"pipeline"`
	Rank     int     `json:"rank,omitempty"`
	Raw      float64 `json:"raw"`
	Weight   float64 `json:"weight"`
	Value    float64 `json:"value"`
}

// HybridOpts chooses how the two rankings are fused.
//
//   - rank: $rankFusion — reciprocal rank fusion, Σ weight / (60 + rank). Only positions
//     matter, so it needs no idea of how the two engines' scores compare.
//   - score: $scoreFusion — the scores themselves, normalized ("sigmoid", "minMaxScaler"
//     or "none") and averaged with the weights. Uses how sure each engine was, which
//     helps when one is confidently right and hurts when scales disagree.
//
// VectorWeight is the vector pipeline's share, 0..1; keyword gets the rest.
type HybridOpts struct {
	Method        string  `json:"method"`        // rank | score
	VectorWeight  float64 `json:"vectorWeight"`  // 0..1
	Normalization string  `json:"normalization"` // sigmoid | minMaxScaler | none (score only)
}

// DefaultHybrid is reciprocal rank fusion with equal weights — the desk's starting point.
// (The zero value is not a default: VectorWeight 0 is a legitimate "keyword only".)
func DefaultHybrid() HybridOpts {
	return HybridOpts{Method: "rank", VectorWeight: 0.5, Normalization: "sigmoid"}
}

// Normalize fills defaults and clamps: an unknown method is rank fusion, an
// out-of-range weight is 0.5, an unknown normalization is sigmoid.
func (o HybridOpts) Normalize() HybridOpts {
	if o.Method != "score" {
		o.Method = "rank"
	}
	if o.VectorWeight < 0 || o.VectorWeight > 1 {
		o.VectorWeight = 0.5
	}
	switch o.Normalization {
	case "sigmoid", "minMaxScaler", "none":
	default:
		o.Normalization = "sigmoid"
	}
	return o
}

// Label names the fusion for the UI: "$rankFusion 0.5 : 0.5".
func (o HybridOpts) Label() string { return o.LabelFor("mongodb") }

// LabelFor names the blend as the engine spells it: MongoDB has fusion stages,
// PostgreSQL does the same arithmetic in a SQL CTE.
func (o HybridOpts) LabelFor(engine string) string {
	o = o.Normalize()
	st := "$rankFusion"
	if o.Method == "score" {
		st = "$scoreFusion (" + o.Normalization + ")"
	}
	if engine == "postgres" {
		st = "reciprocal rank fusion (SQL)"
		if o.Method == "score" {
			st = "score fusion (SQL, " + o.Normalization + ")"
		}
	}
	return fmt.Sprintf("%s · vector %.1f : keyword %.1f", st, o.VectorWeight, 1-o.VectorWeight)
}

// Result is a set of hits and how they were obtained.
type Result struct {
	Hits     []Hit   `json:"hits"`
	Pipeline string  `json:"pipeline"` // mongosh text
	Engine   string  `json:"engine"`   // mongot | app
	Ms       float64 `json:"ms"`
	Error    string  `json:"error,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// Filter is the pre-filter a vector query may carry.
type Filter struct {
	Team     string    `json:"team,omitempty"`
	Plan     string    `json:"plan,omitempty"`
	Customer string    `json:"customer,omitempty"`
	Status   []string  `json:"status,omitempty"`
	Since    time.Time `json:"since,omitempty"`
}

// IsEmpty reports whether the filter constrains nothing.
func (f Filter) IsEmpty() bool { return f.empty() }

func (f Filter) empty() bool {
	return f.Team == "" && f.Plan == "" && f.Customer == "" && len(f.Status) == 0 && f.Since.IsZero()
}

func (f Filter) bson() bson.D {
	var d bson.D
	if f.Team != "" {
		d = append(d, bson.E{Key: "team", Value: f.Team})
	}
	if f.Plan != "" {
		d = append(d, bson.E{Key: "plan", Value: f.Plan})
	}
	if f.Customer != "" {
		d = append(d, bson.E{Key: "customer", Value: f.Customer})
	}
	if len(f.Status) == 1 {
		d = append(d, bson.E{Key: "status", Value: f.Status[0]})
	} else if len(f.Status) > 1 {
		d = append(d, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: f.Status}}})
	}
	if !f.Since.IsZero() {
		d = append(d, bson.E{Key: "createdAt", Value: bson.D{{Key: "$gte", Value: f.Since}}})
	}
	return d
}

// VectorQuery is a $vectorSearch.
type VectorQuery struct {
	Collection    string
	Vector        []float32
	K             int
	NumCandidates int
	Exact         bool
	Filter        Filter
	// ForceIndex asks a SQL engine to use its vector index even where the planner
	// would rather scan a small table, so the index's settings visibly matter.
	ForceIndex bool
}

func indexFor(coll string, vector bool) string {
	switch {
	case coll == store.KB && vector:
		return store.KBVector
	case coll == store.KB:
		return store.KBText
	case vector:
		return store.TicketsVector
	default:
		return store.TicketsText
	}
}

func projectFor(coll, scoreMeta string) bson.D {
	p := bson.D{{Key: "team", Value: 1}, {Key: "score", Value: bson.D{{Key: "$meta", Value: scoreMeta}}}}
	if coll == store.KB {
		return append(bson.D{{Key: "title", Value: 1}}, p...)
	}
	return append(bson.D{{Key: "subject", Value: 1}, {Key: "status", Value: 1}, {Key: "resolvedKB", Value: 1}, {Key: "plan", Value: 1}, {Key: "createdAt", Value: 1}}, p...)
}

func vectorStage(q VectorQuery) bson.D {
	vs := bson.D{
		{Key: "index", Value: indexFor(q.Collection, true)},
		{Key: "path", Value: "embedding"},
		{Key: "queryVector", Value: q.Vector},
	}
	if q.Exact {
		vs = append(vs, bson.E{Key: "exact", Value: true})
	} else {
		vs = append(vs, bson.E{Key: "numCandidates", Value: q.NumCandidates})
	}
	vs = append(vs, bson.E{Key: "limit", Value: q.K})
	if !q.Filter.empty() {
		vs = append(vs, bson.E{Key: "filter", Value: q.Filter.bson()})
	}
	return bson.D{{Key: "$vectorSearch", Value: vs}}
}

// VectorPipeline is the $vectorSearch pipeline for q.
func VectorPipeline(q VectorQuery) bson.A {
	return bson.A{vectorStage(q), bson.D{{Key: "$project", Value: projectFor(q.Collection, "vectorSearchScore")}}}
}

func textStage(coll, query string, resolvedOnly bool) bson.D {
	paths := bson.A{"subject", "body"}
	if coll == store.KB {
		paths = bson.A{"title", "summary", "steps"}
	}
	text := bson.D{{Key: "query", Value: query}, {Key: "path", Value: paths}}
	if !resolvedOnly {
		return bson.D{{Key: "$search", Value: bson.D{{Key: "index", Value: indexFor(coll, false)}, {Key: "text", Value: text}}}}
	}
	return bson.D{{Key: "$search", Value: bson.D{{Key: "index", Value: indexFor(coll, false)}, {Key: "compound", Value: bson.D{
		{Key: "must", Value: bson.A{bson.D{{Key: "text", Value: text}}}},
		{Key: "filter", Value: bson.A{bson.D{{Key: "in", Value: bson.D{{Key: "path", Value: "status"}, {Key: "value", Value: bson.A{"resolved"}}}}}}},
	}}}}}
}

// TextPipeline is the $search (BM25) pipeline.
func TextPipeline(coll, query string, k int, resolvedOnly bool) bson.A {
	return bson.A{textStage(coll, query, resolvedOnly), bson.D{{Key: "$limit", Value: k}}, bson.D{{Key: "$project", Value: projectFor(coll, "searchScore")}}}
}

// FusionPipeline fuses the two with $rankFusion or $scoreFusion, asking the server for
// scoreDetails so the page can show where each hybrid score came from.
func FusionPipeline(q VectorQuery, query string, resolvedOnly bool, o HybridOpts) bson.A {
	o = o.Normalize()
	vq := q
	vq.K = 20
	if !q.Exact && vq.NumCandidates < 200 {
		vq.NumCandidates = 200
	}
	input := bson.D{{Key: "pipelines", Value: bson.D{
		{Key: "vector", Value: bson.A{vectorStage(vq)}},
		{Key: "keyword", Value: bson.A{textStage(q.Collection, query, resolvedOnly), bson.D{{Key: "$limit", Value: 20}}}},
	}}}
	stage := "$rankFusion"
	if o.Method == "score" {
		stage = "$scoreFusion"
		input = append(input, bson.E{Key: "normalization", Value: o.Normalization})
	}
	weights := bson.D{{Key: "vector", Value: round2(o.VectorWeight)}, {Key: "keyword", Value: round2(1 - o.VectorWeight)}}
	return bson.A{
		bson.D{{Key: stage, Value: bson.D{
			{Key: "input", Value: input},
			{Key: "combination", Value: bson.D{{Key: "weights", Value: weights}}},
			{Key: "scoreDetails", Value: true},
		}}},
		bson.D{{Key: "$limit", Value: q.K}},
		bson.D{{Key: "$project", Value: append(projectFor(q.Collection, "score"), bson.E{Key: "details", Value: bson.D{{Key: "$meta", Value: "scoreDetails"}}})}},
	}
}

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

// ---------------------------------------------------------------- the searcher

// Searcher runs queries against mongot when it can and in the app when it cannot.
type Searcher struct {
	St     *store.Store
	Mirror *Mirror

	mu        sync.RWMutex
	mongot    bool           // search indexes are queryable
	fusion    map[string]int // per method: 0 unknown, 1 the server's stage works, -1 it does not
	fusionErr string
}

// NewSearcher returns a searcher over a store and a mirror.
func NewSearcher(st *store.Store, m *Mirror) *Searcher {
	return &Searcher{St: st, Mirror: m, fusion: map[string]int{}}
}

// SetReady records whether mongot-backed search is usable right now.
func (s *Searcher) SetReady(ok bool) {
	s.mu.Lock()
	s.mongot = ok
	s.mu.Unlock()
}

// Ready reports whether mongot-backed search is in use.
func (s *Searcher) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mongot
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Vector runs a $vectorSearch (or its in-app equivalent).
func (s *Searcher) Vector(ctx context.Context, q VectorQuery) Result {
	if q.K <= 0 {
		q.K = 5
	}
	if q.NumCandidates < q.K {
		q.NumCandidates = q.K * 15
	}
	p := VectorPipeline(q)
	if !s.Ready() {
		t0 := time.Now()
		hits := s.Mirror.Scan(q.Collection, q.Vector, q.K, q.Filter)
		return Result{Hits: hits, Pipeline: Shell(q.Collection, p), Engine: "app", Ms: ms(time.Since(t0)),
			Note: "No mongot: this is an exact cosine scan of every document in the app's memory — the pipeline above is what would run with vector search on."}
	}
	docs, d, err := s.St.Aggregate(ctx, q.Collection, p)
	r := Result{Pipeline: Shell(q.Collection, p), Engine: "mongot", Ms: ms(d)}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Hits = toHits(q.Collection, docs)
	for i := range r.Hits {
		r.Hits[i].Cosine = 2*r.Hits[i].Score - 1
	}
	return r
}

// Keyword runs a $search text query (or $text without mongot).
func (s *Searcher) Keyword(ctx context.Context, coll, query string, k int, resolvedOnly bool) Result {
	if k <= 0 {
		k = 5
	}
	if !s.Ready() {
		filter := bson.D{{Key: "$text", Value: bson.D{{Key: "$search", Value: query}}}}
		if resolvedOnly {
			filter = append(filter, bson.E{Key: "status", Value: "resolved"})
		}
		p := bson.A{bson.D{{Key: "$match", Value: filter}}, bson.D{{Key: "$sort", Value: bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}}},
			bson.D{{Key: "$limit", Value: k}}, bson.D{{Key: "$project", Value: projectFor(coll, "textScore")}}}
		docs, d, err := s.St.Aggregate(ctx, coll, p)
		r := Result{Pipeline: Shell(coll, p), Engine: "app", Ms: ms(d), Note: "No mongot: the classic $text index stands in for $search."}
		if err != nil {
			r.Error = err.Error()
		}
		r.Hits = toHits(coll, docs)
		return r
	}
	p := TextPipeline(coll, query, k, resolvedOnly)
	docs, d, err := s.St.Aggregate(ctx, coll, p)
	r := Result{Pipeline: Shell(coll, p), Engine: "mongot", Ms: ms(d)}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Hits = toHits(coll, docs)
	return r
}

// Hybrid fuses the two as o says. It tries the server's stage; if the server does
// not have it, the same fusion is computed here, from the same two pipelines, and the
// result says so.
func (s *Searcher) Hybrid(ctx context.Context, q VectorQuery, query string, resolvedOnly bool, o HybridOpts) Result {
	o = o.Normalize()
	if q.K <= 0 {
		q.K = 5
	}
	p := FusionPipeline(q, query, resolvedOnly, o)
	s.mu.RLock()
	fusion := s.fusion[o.Method]
	s.mu.RUnlock()
	if s.Ready() && fusion >= 0 {
		docs, d, err := s.St.Aggregate(ctx, q.Collection, p)
		if err == nil {
			s.mu.Lock()
			s.fusion[o.Method] = 1
			s.mu.Unlock()
			return Result{Hits: toHits(q.Collection, docs), Pipeline: Shell(q.Collection, p), Engine: "mongot", Ms: ms(d)}
		}
		s.mu.Lock()
		s.fusion[o.Method], s.fusionErr = -1, err.Error()
		s.mu.Unlock()
	}
	t0 := time.Now()
	vq := q
	vq.K = 20
	v := s.Vector(ctx, vq)
	kw := s.Keyword(ctx, q.Collection, query, 20, resolvedOnly)
	hits := fuseInApp(v.Hits, kw.Hits, o, q.K)
	note := "Fusion computed in the app from the vector and keyword result lists"
	s.mu.RLock()
	if s.fusionErr != "" {
		note += " — this server answered the fusion stage with: " + firstLine(s.fusionErr)
	}
	s.mu.RUnlock()
	return Result{Hits: hits, Pipeline: Shell(q.Collection, p), Engine: "app", Ms: ms(time.Since(t0)), Note: note}
}

// fuseInApp is $rankFusion / $scoreFusion done by hand, for a server without them.
func fuseInApp(vec, kw []Hit, o HybridOpts, k int) []Hit {
	type acc struct {
		h     Hit
		s     float64
		parts []Part
	}
	byID := map[string]*acc{}
	add := func(name string, hs []Hit, w float64) {
		lo, hi := 0.0, 0.0
		for i, h := range hs {
			if i == 0 || h.Score < lo {
				lo = h.Score
			}
			if i == 0 || h.Score > hi {
				hi = h.Score
			}
		}
		for i, h := range hs {
			key := fmt.Sprint(h.ID)
			a := byID[key]
			if a == nil {
				a = &acc{h: h}
				byID[key] = a
			}
			var v float64
			if o.Method == "rank" {
				v = w / float64(60+i+1)
			} else {
				n := h.Score
				switch o.Normalization {
				case "sigmoid":
					n = 1 / (1 + math.Exp(-h.Score))
				case "minMaxScaler":
					n = 0
					if hi > lo {
						n = (h.Score - lo) / (hi - lo)
					}
				}
				v = w * n
			}
			a.s += v
			a.parts = append(a.parts, Part{Pipeline: name, Rank: i + 1, Raw: h.Score, Weight: w, Value: v})
		}
	}
	add("vector", vec, o.VectorWeight)
	add("keyword", kw, 1-o.VectorWeight)
	var all []acc
	for _, a := range byID {
		all = append(all, *a)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	var hits []Hit
	for i := 0; i < len(all) && i < k; i++ {
		h := all[i].h
		h.Score, h.Parts = all[i].s, all[i].parts
		hits = append(hits, h)
	}
	return hits
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func toHits(coll string, docs []bson.M) []Hit {
	out := make([]Hit, 0, len(docs))
	for _, d := range docs {
		h := Hit{ID: d["_id"]}
		h.Team, _ = d["team"].(string)
		h.Score = num(d["score"])
		h.Parts = parts(d["details"])
		if coll == store.KB {
			h.Title, _ = d["title"].(string)
			h.KB, _ = d["_id"].(string)
		} else {
			h.Title, _ = d["subject"].(string)
			h.Status, _ = d["status"].(string)
			h.KB, _ = d["resolvedKB"].(string)
			h.Plan, _ = d["plan"].(string)
			if t, ok := d["createdAt"].(interface{ Time() time.Time }); ok {
				h.CreatedAt = t.Time()
			}
		}
		out = append(out, h)
	}
	return out
}

// parts reads a fusion stage's scoreDetails into Parts.
func parts(v any) []Part {
	d, ok := v.(bson.M)
	if !ok {
		return nil
	}
	arr, _ := d["details"].(bson.A)
	var out []Part
	for _, x := range arr {
		m, ok := x.(bson.M)
		if !ok {
			continue
		}
		p := Part{Weight: num(m["weight"])}
		p.Pipeline, _ = m["inputPipelineName"].(string)
		p.Rank = int(num(m["rank"]))
		if r, ok := m["inputPipelineRawScore"]; ok {
			p.Raw = num(r)
			p.Value = num(m["value"])
		} else {
			// $rankFusion reports the raw score as "value" and the rank separately; its
			// contribution is weight / (60 + rank).
			p.Raw = num(m["value"])
			if p.Rank > 0 {
				p.Value = p.Weight / float64(60+p.Rank)
			}
		}
		out = append(out, p)
	}
	return out
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// ---------------------------------------------------------------- mirror

// Doc is a document's searchable essentials, held in memory.
type Doc struct {
	ID        any
	Title     string
	Team      string
	Status    string
	KB        string
	Plan      string
	Customer  string
	CreatedAt time.Time
	Vec       []float32
}

// Mirror keeps every embedding in memory. It is the in-app engine when there is
// no mongot, the ground truth an approximate search's recall is measured against,
// and the source of the 2-D map.
type Mirror struct {
	mu      sync.RWMutex
	kb      []*Doc
	tickets []*Doc
	byID    map[string]*Doc
}

// NewMirror returns an empty mirror.
func NewMirror() *Mirror { return &Mirror{byID: map[string]*Doc{}} }

// Put adds or replaces a document.
func (m *Mirror) Put(coll string, d *Doc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := coll + ":" + fmt.Sprint(d.ID)
	if old, ok := m.byID[key]; ok {
		*old = *d
		return
	}
	m.byID[key] = d
	if coll == store.KB {
		m.kb = append(m.kb, d)
	} else {
		m.tickets = append(m.tickets, d)
	}
}

// Update changes a ticket's mutable fields.
func (m *Mirror) Update(id any, status, team, kb string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.byID[store.Tickets+":"+fmt.Sprint(id)]; ok {
		if status != "" {
			d.Status = status
		}
		if team != "" {
			d.Team = team
		}
		if kb != "" {
			d.KB = kb
		}
	}
}

// Remove drops a ticket.
func (m *Mirror) Remove(id any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := store.Tickets + ":" + fmt.Sprint(id)
	d, ok := m.byID[key]
	if !ok {
		return
	}
	delete(m.byID, key)
	for i, t := range m.tickets {
		if t == d {
			m.tickets = append(m.tickets[:i], m.tickets[i+1:]...)
			break
		}
	}
}

// Len returns how many documents of a collection are mirrored.
func (m *Mirror) Len(coll string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if coll == store.KB {
		return len(m.kb)
	}
	return len(m.tickets)
}

// Snapshot returns copies of a collection's documents.
func (m *Mirror) Snapshot(coll string) []Doc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.tickets
	if coll == store.KB {
		src = m.kb
	}
	out := make([]Doc, len(src))
	for i, d := range src {
		out[i] = *d
	}
	return out
}

// Scan is an exact k-nearest-neighbour search: every document, every dimension.
func (m *Mirror) Scan(coll string, v []float32, k int, f Filter) []Hit {
	m.mu.RLock()
	src := m.tickets
	if coll == store.KB {
		src = m.kb
	}
	type sc struct {
		d *Doc
		s float32
	}
	all := make([]sc, 0, len(src))
	for _, d := range src {
		if f.Team != "" && d.Team != f.Team || f.Plan != "" && d.Plan != f.Plan || f.Customer != "" && d.Customer != f.Customer ||
			!f.Since.IsZero() && d.CreatedAt.Before(f.Since) {
			continue
		}
		if len(f.Status) > 0 {
			ok := false
			for _, s := range f.Status {
				ok = ok || s == d.Status
			}
			if !ok {
				continue
			}
		}
		all = append(all, sc{d, embed.Cosine(v, d.Vec)})
	}
	m.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	var out []Hit
	for i := 0; i < len(all) && i < k; i++ {
		d := all[i].d
		// mongot reports cosine similarity rescaled to [0,1] as (1 + cos) / 2; the
		// app does the same so scores mean the same thing in both engines.
		out = append(out, Hit{ID: d.ID, Title: d.Title, Team: d.Team, Status: d.Status, KB: d.KB, Plan: d.Plan, CreatedAt: d.CreatedAt,
			Score: (1 + float64(all[i].s)) / 2, Cosine: float64(all[i].s)})
	}
	return out
}
