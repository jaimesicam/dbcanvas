package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"supportsim/internal/corpus"
	"supportsim/internal/embed"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// lab.go answers the Vector Lab's questions: what does a sentence look like as a
// vector, how close are two sentences, where does a query land among the tickets,
// how much does an approximate search miss, and how long after an insert does a
// document become findable.

// EmbedView is a sentence and its vector.
type EmbedView struct {
	Text   string    `json:"text"`
	Tokens []string  `json:"tokens"`
	Vector []float32 `json:"vector"`
	Ms     float64   `json:"ms"`
	Norm   float64   `json:"norm"`
}

// Embed embeds one sentence for display.
func (e *Engine) Embed(text string) EmbedView {
	t0 := time.Now()
	v := e.Model.Embed(text)
	n := 0.0
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	return EmbedView{Text: text, Tokens: e.Model.Tokenize(text), Vector: v, Ms: msSince(t0), Norm: math.Sqrt(n)}
}

// Compare is two sentences and how similar they are.
type Compare struct {
	A      EmbedView `json:"a"`
	B      EmbedView `json:"b"`
	Cosine float64   `json:"cosine"`
	Score  float64   `json:"score"` // as $vectorSearch would report it
	// Contrib is each dimension's contribution a[i]*b[i] to the cosine — which is
	// all a cosine of two unit vectors is: a sum of 384 products.
	Contrib []float32 `json:"contrib"`
}

// Similarity compares two sentences.
func (e *Engine) Similarity(a, b string) Compare {
	va, vb := e.Embed(a), e.Embed(b)
	c := float64(embed.Cosine(va.Vector, vb.Vector))
	contrib := make([]float32, len(va.Vector))
	for i := range contrib {
		contrib[i] = va.Vector[i] * vb.Vector[i]
	}
	return Compare{A: va, B: vb, Cosine: c, Score: (1 + c) / 2, Contrib: contrib}
}

// ---------------------------------------------------------------- the map

// Point is one document on the 2-D map.
type Point struct {
	ID    any     `json:"id"`
	Kind  string  `json:"kind"` // kb | ticket
	Team  string  `json:"team"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// Map is a principal-component projection of the corpus: the two directions in
// 384-dimensional space along which the documents differ most. It is a shadow of
// the real geometry — distances on it are only roughly the real ones — which the
// Lab says out loud, because a 2-D picture of a 384-D space always lies a little.
type Map struct {
	Points    []Point   `json:"points"`
	Explained []float64 `json:"explained"` // share of variance each axis captures
	mean      []float64
	pc        [2][]float64
	vecs      [][]float32
}

// Map returns the cached projection, recomputing it once a minute.
func (e *Engine) Map() *Map {
	e.mu.Lock()
	if e.mapCache != nil && time.Since(e.mapAt) < time.Minute {
		m := e.mapCache
		e.mu.Unlock()
		return m
	}
	e.mu.Unlock()

	kb := e.Mirror.Snapshot(store.KB)
	tk := e.Mirror.Snapshot(store.Tickets)
	// The most recent 500 tickets: enough to show the clusters, few enough to draw.
	if len(tk) > 500 {
		tk = tk[len(tk)-500:]
	}
	var docs []search.Doc
	var kinds []string
	for _, d := range kb {
		docs, kinds = append(docs, d), append(kinds, "kb")
	}
	for _, d := range tk {
		if d.Status == "probe" {
			continue
		}
		docs, kinds = append(docs, d), append(kinds, "ticket")
	}
	m := &Map{}
	if len(docs) < 3 {
		return m
	}
	dim := len(docs[0].Vec)
	m.mean = make([]float64, dim)
	for _, d := range docs {
		for i, x := range d.Vec {
			m.mean[i] += float64(x)
		}
		m.vecs = append(m.vecs, d.Vec)
	}
	for i := range m.mean {
		m.mean[i] /= float64(len(docs))
	}
	centered := make([][]float64, len(docs))
	total := 0.0
	for j, d := range docs {
		c := make([]float64, dim)
		for i, x := range d.Vec {
			c[i] = float64(x) - m.mean[i]
			total += c[i] * c[i]
		}
		centered[j] = c
	}
	// Power iteration on XᵀX without forming it: v ← Xᵀ(Xv), twice, deflating the
	// first component before finding the second.
	rng := rand.New(rand.NewSource(1))
	for k := 0; k < 2; k++ {
		v := make([]float64, dim)
		for i := range v {
			v[i] = rng.Float64() - 0.5
		}
		var lambda float64
		for it := 0; it < 60; it++ {
			xv := make([]float64, len(centered))
			for j, c := range centered {
				xv[j] = dot(c, v)
			}
			nv := make([]float64, dim)
			for j, c := range centered {
				for i := range nv {
					nv[i] += c[i] * xv[j]
				}
			}
			if k == 1 { // keep it orthogonal to the first component
				p := dot(nv, m.pc[0])
				for i := range nv {
					nv[i] -= p * m.pc[0][i]
				}
			}
			lambda = math.Sqrt(dot(nv, nv))
			if lambda == 0 {
				break
			}
			for i := range nv {
				nv[i] /= lambda
			}
			v = nv
		}
		m.pc[k] = v
		if total > 0 {
			m.Explained = append(m.Explained, lambda/total)
		}
	}
	for j, d := range docs {
		x, y := m.project(d.Vec)
		label := d.Title
		m.Points = append(m.Points, Point{ID: d.ID, Kind: kinds[j], Team: d.Team, Label: label, X: x, Y: y})
	}
	e.mu.Lock()
	e.mapCache, e.mapAt = m, time.Now()
	e.mu.Unlock()
	return m
}

func (m *Map) project(v []float32) (float64, float64) {
	var x, y float64
	for i, f := range v {
		c := float64(f) - m.mean[i]
		x += c * m.pc[0][i]
		y += c * m.pc[1][i]
	}
	return x, y
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// MapQuery places a sentence on the map and names its true nearest neighbours
// (by full 384-D cosine, not by distance on the picture).
type MapQuery struct {
	X         float64      `json:"x"`
	Y         float64      `json:"y"`
	Neighbors []search.Hit `json:"neighbors"`
}

// Locate projects a sentence onto the current map.
func (e *Engine) Locate(text string) MapQuery {
	m := e.Map()
	if len(m.mean) == 0 {
		return MapQuery{}
	}
	v := e.Model.Embed(text)
	x, y := m.project(v)
	hits := e.Mirror.Scan(store.Tickets, v, 6, search.Filter{Status: []string{"resolved", "open", "auto_answered", "duplicate"}})
	hits = append(e.Mirror.Scan(store.KB, v, 2, search.Filter{}), hits...)
	return MapQuery{X: x, Y: y, Neighbors: hits}
}

// ---------------------------------------------------------------- query builder

// BuilderRequest is the Lab's $vectorSearch builder.
type BuilderRequest struct {
	Query         string        `json:"query"`
	Collection    string        `json:"collection"`
	K             int           `json:"k"`
	NumCandidates int           `json:"numCandidates"`
	Exact         bool          `json:"exact"`
	Filter        search.Filter `json:"filter"`
}

// BuilderResult is the search plus how good it was.
type BuilderResult struct {
	search.Result
	EmbedMs float64       `json:"embedMs"`
	Exact   search.Result `json:"exactResult"`
	Recall  float64       `json:"recall"` // share of the exact top-k the approximate search found
}

// Build runs a $vectorSearch and, to show what approximation costs, the same query
// with exact: true — mongot's own brute-force scan — and reports recall@k.
func (e *Engine) Build(ctx context.Context, r BuilderRequest) BuilderResult {
	if r.Collection != store.KB {
		r.Collection = store.Tickets
	}
	if r.K <= 0 || r.K > 50 {
		r.K = 5
	}
	if r.NumCandidates < r.K {
		r.NumCandidates = r.K
	}
	if r.NumCandidates > 10000 {
		r.NumCandidates = 10000
	}
	t0 := time.Now()
	v := e.Model.Embed(r.Query)
	out := BuilderResult{EmbedMs: msSince(t0)}
	q := search.VectorQuery{Collection: r.Collection, Vector: v, K: r.K, NumCandidates: r.NumCandidates, Exact: r.Exact, Filter: r.Filter, ForceIndex: true}
	out.Result = e.Search.Vector(ctx, q)
	if r.Exact {
		out.Recall = 1
		return out
	}
	eq := q
	eq.Exact = true
	out.Exact = e.Search.Vector(ctx, eq)
	if len(out.Exact.Hits) > 0 {
		want := map[string]bool{}
		for _, h := range out.Exact.Hits {
			want[fmt.Sprint(h.ID)] = true
		}
		got := 0
		for _, h := range out.Hits {
			if want[fmt.Sprint(h.ID)] {
				got++
			}
		}
		out.Recall = float64(got) / float64(len(out.Exact.Hits))
	}
	return out
}

// ---------------------------------------------------------------- freshness

// Freshness is how long a new document took to become findable.
type Freshness struct {
	Engine   string  `json:"engine"`
	InsertMs float64 `json:"insertMs"`
	FoundMs  float64 `json:"foundMs"` // from insert acknowledged to first $vectorSearch hit
	Attempts int     `json:"attempts"`
	Found    bool    `json:"found"`
	Text     string  `json:"text"`
	Pipeline string  `json:"pipeline"`
	// Steps is PostgreSQL's transactional story: what another session and the writing
	// session itself see before and after COMMIT.
	Steps []FreshStep `json:"steps,omitempty"`
}

// FreshStep is one moment of the PostgreSQL freshness demonstration.
type FreshStep struct {
	When    string  `json:"when"`
	Session string  `json:"session"`
	Found   bool    `json:"found"`
	Ms      float64 `json:"ms"`
}

// Freshness inserts a probe document and measures when vector search finds it —
// an engine-specific story (see each backend's Freshness).
func (e *Engine) Freshness(ctx context.Context) Freshness {
	text := fmt.Sprintf("Freshness probe %d: a brand-new ticket nobody has seen before", time.Now().UnixNano()%100000)
	return e.B.Freshness(ctx, e.Model.Embed(text), text)
}

// ---------------------------------------------------------------- showdown

// Examples are the Showdown's curated queries, each with the article that answers
// it — so the page can mark results right or wrong instead of leaving it to taste.
var Examples = []struct {
	Query string `json:"query"`
	KB    string `json:"kb"`
}{
	// Customers' words, not the article's: vector finds these, keyword mostly not.
	{"my card was charged twice", "refund-duplicate-charge"},
	{"my developer accidentally pushed our secret token to GitHub", "rotate-api-keys"},
	{"we are paying too much, can we pick a smaller package", "change-plan"},
	{"we keep getting disconnected randomly overnight", "intermittent-disconnects"},
	{"our container can't verify the server's identity", "tls-verification"},
	{"the database server is maxed out and everything is slow", "high-cpu"},
	// Both manage.
	{"I got a new phone and can't get my login codes", "mfa-device-recovery"},
	// The words do match and keyword wins — which is what hybrid is for.
	{"reporting queries are hurting production", "read-preference"},
	{"a script wiped a collection this morning, I need it back", "point-in-time-recovery"},
	// Nobody finds it: the limit of a small general-purpose model.
	{"the cluster refuses writes after the network blip", "no-primary"},
}

// Showdown is one query answered three ways.
type Showdown struct {
	Query   EmbedView     `json:"query"`
	Keyword search.Result `json:"keyword"`
	Vector  search.Result `json:"vector"`
	Hybrid  search.Result `json:"hybrid"`
	Truth   string        `json:"truth,omitempty"` // answering article, for curated queries
}

// RunShowdown answers a query with keyword, vector and hybrid search.
func (e *Engine) RunShowdown(ctx context.Context, query, coll string, k int, f search.Filter, o *search.HybridOpts) Showdown {
	ho := e.HybridOpts()
	if o != nil {
		ho = o.Normalize()
	}
	if coll != store.Tickets {
		coll = store.KB
	}
	if k <= 0 || k > 20 {
		k = 5
	}
	ev := e.Embed(query)
	out := Showdown{Query: ev}
	for _, ex := range Examples {
		if ex.Query == query {
			out.Truth = ex.KB
		}
	}
	q := search.VectorQuery{Collection: coll, Vector: ev.Vector, K: k, NumCandidates: 150, Filter: f}
	resolved := coll == store.Tickets
	if resolved {
		q.Filter.Status = []string{"resolved"}
	}
	out.Keyword = e.Search.Keyword(ctx, coll, query, k, resolved)
	out.Vector = e.Search.Vector(ctx, q)
	out.Hybrid = e.Search.Hybrid(ctx, q, query, resolved, ho)
	return out
}

// Teams and articles for the UI's legends and lookups.
func Catalog() map[string]any {
	type art struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
		Team  string `json:"team"`
	}
	var arts []art
	for _, a := range corpus.Articles {
		arts = append(arts, art{a.Slug, a.Title, a.Team})
	}
	var outages []map[string]string
	for _, a := range corpus.Incidents() {
		outages = append(outages, map[string]string{"id": a.ID, "label": a.Subjects[0]})
	}
	return map[string]any{"teams": corpus.Teams, "articles": arts, "plans": corpus.Plans, "examples": Examples, "outages": outages,
		"thresholds": map[string]float64{"autoAnswer": autoAnswerCos, "agreement": autoAnswerAgr, "duplicate": duplicateCos, "incident": incidentCos, "incidentMin": incidentMin, "novel": novelCos}}
}
