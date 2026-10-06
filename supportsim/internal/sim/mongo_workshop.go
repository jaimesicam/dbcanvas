package sim

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"supportsim/internal/mongotmetrics"
	"supportsim/internal/search"
	"supportsim/internal/store"
)

// mongo_workshop.go — the Index Workshop on MongoDB: nine search indexes over the
// same vectors (similarity, quantization, BSON binary vectors, normalization),
// mongot's explain, mongot's own metrics, and a search index's lifecycle.

type mongoWorkshop struct {
	St *store.Store
}

func (w *mongoWorkshop) Variants() []Variant { return mongoVariants }

var mongoVariants = []Variant{
	{Name: "v_cosine", Path: "emb", Similarity: "cosine", Storage: "array of 384 doubles",
		Lesson: "The baseline: what the desk uses. Every other row is compared with exact search on this one."},
	{Name: "v_dotproduct", Path: "emb", Similarity: "dotProduct", Storage: "array of 384 doubles",
		Lesson: "On unit-length vectors the dot product is the cosine, so the ranking is identical. Only the score scale differs. It's slightly cheaper, but only safe when every vector is normalized."},
	{Name: "v_euclidean", Path: "emb", Similarity: "euclidean", Storage: "array of 384 doubles",
		Lesson: "On unit vectors, distance and angle order neighbours the same way (|a−b|² = 2 − 2·cos). The score is 1/(1+d), so the numbers look very different."},
	{Name: "v_scalar", Path: "emb", Similarity: "cosine", Quantization: "scalar", Storage: "array; index stores int8",
		Lesson: "The HNSW graph mongot searches holds one byte per dimension instead of four, so it needs about a quarter of the memory. On disk the index is not smaller (compare its size): mongot also keeps the full-precision vectors there, to rescore the best candidates. Quantization saves RAM, not disk."},
	{Name: "v_binary", Path: "emb", Similarity: "cosine", Quantization: "binary", Storage: "array; index stores 1 bit",
		Lesson: "One bit per dimension in the graph, 32× less memory than float32, then the best candidates are rescored with the full vectors kept on disk. The biggest memory saving, and the most sensitive to numCandidates: run at 10 and at 200 and compare its recall."},
	{Name: "v_float32", Path: "emb_f32", Similarity: "cosine", Storage: "BinData vector, float32",
		Lesson: "The same numbers stored as a BSON binary vector (subtype 9) instead of an array of doubles: the same results at less than half the document size."},
	{Name: "v_int8", Path: "emb_i8", Similarity: "cosine", Storage: "BinData vector, int8", int8Query: true,
		Lesson: "Each value scaled to −127..127 and stored as one byte: a quarter of float32, in the document and on disk, since there is nothing bigger to keep. Gotcha: the query vector must be int8 too. A float query returns nothing, without an error."},
	{Name: "v_raw_cosine", Path: "emb_raw", Similarity: "cosine", Storage: "array, NOT normalized",
		Lesson: "These vectors were given random lengths. Cosine ignores length, so nothing changes."},
	{Name: "v_raw_dotproduct", Path: "emb_raw", Similarity: "dotProduct", Storage: "array, NOT normalized",
		Lesson: "The same un-normalized vectors with dotProduct: long vectors win regardless of meaning. This is why dotProduct requires normalized vectors."},
}

func (v Variant) definition() bson.D {
	f := bson.D{{Key: "type", Value: "vector"}, {Key: "path", Value: v.Path}, {Key: "numDimensions", Value: store.Dims}, {Key: "similarity", Value: v.Similarity}}
	if v.Quantization != "" {
		f = append(f, bson.E{Key: "quantization", Value: v.Quantization})
	}
	return bson.D{{Key: "fields", Value: bson.A{f, bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "team"}}}}}
}

// Shell is the definition as typed in mongosh.
func (v Variant) Shell() string {
	if v.ddl != "" {
		return v.ddl
	}
	q := ""
	if v.Quantization != "" {
		q = fmt.Sprintf(`, quantization: %q`, v.Quantization)
	}
	return fmt.Sprintf(`db.vector_variants.createSearchIndex(%q, "vectorSearch", {
  fields: [
    { type: "vector", path: %q, numDimensions: 384, similarity: %q%s },
    { type: "filter", path: "team" }
  ]
})`, v.Name, v.Path, v.Similarity, q)
}

// ---------------------------------------------------------------- BSON binary vectors

// float32Vector is a BSON binary vector (subtype 9): dtype 0x27 (float32), padding 0,
// then the values little-endian. The Go driver this app pins has no helper for it,
// and the format is small enough to write out.
func float32Vector(v []float32) primitive.Binary {
	b := make([]byte, 2+4*len(v))
	b[0] = 0x27
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[2+4*i:], math.Float32bits(x))
	}
	return primitive.Binary{Subtype: 9, Data: b}
}

// int8Vector quantizes each vector by its own largest component, so the full −127..127
// range is used — cosine does not care about the scale, only the direction.
func int8Vector(v []float32) primitive.Binary {
	m := float32(0)
	for _, x := range v {
		if a := float32(math.Abs(float64(x))); a > m {
			m = a
		}
	}
	if m == 0 {
		m = 1
	}
	b := make([]byte, 2+len(v))
	b[0] = 0x03
	for i, x := range v {
		b[2+i] = byte(int8(math.Round(float64(x / m * 127))))
	}
	return primitive.Binary{Subtype: 9, Data: b}
}

// Detect reports a variant collection built before a restart.
func (w *mongoWorkshop) Detect(ctx context.Context) (bool, int) {
	ix, err := w.St.SearchIndexes(ctx, store.Variants)
	if err != nil {
		return false, 0
	}
	want := map[string]bool{}
	for _, v := range mongoVariants {
		want[v.Name] = true
	}
	ready := 0
	for _, x := range ix {
		if q, _ := x["queryable"].(bool); q && want[fmt.Sprint(x["name"])] {
			ready++
		}
	}
	if ready < len(mongoVariants) {
		return false, 0
	}
	n, _ := w.St.C(store.Variants).EstimatedDocumentCount(ctx)
	return true, int(n)
}

// Build writes vector_variants and creates the nine indexes, waiting for mongot.
func (w *mongoWorkshop) Build(ctx context.Context, src []search.Doc, progress func(phase, detail string)) error {
	coll := w.St.C(store.Variants)
	coll.Drop(ctx) // takes its search indexes with it
	var docs []any
	for _, d := range src {
		id, _ := d.ID.(int64)
		raw := make([]float32, len(d.Vec))
		s := rawScale(id)
		for i, x := range d.Vec {
			raw[i] = x * s
		}
		docs = append(docs, bson.D{
			{Key: "_id", Value: d.ID}, {Key: "subject", Value: d.Title}, {Key: "team", Value: d.Team}, {Key: "resolvedKB", Value: d.KB},
			{Key: "emb", Value: d.Vec}, {Key: "emb_f32", Value: float32Vector(d.Vec)}, {Key: "emb_i8", Value: int8Vector(d.Vec)},
			{Key: "emb_raw", Value: raw}, {Key: "rawLength", Value: s},
		})
	}
	progress("copying", fmt.Sprintf("writing %d resolved tickets with four copies of each vector", len(docs)))
	for i := 0; i < len(docs); i += 200 {
		j := min(i+200, len(docs))
		if _, err := coll.InsertMany(ctx, docs[i:j]); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	for _, v := range mongoVariants {
		if err := w.St.CreateSearchIndex(ctx, store.Variants, v.Name, "vectorSearch", v.definition()); err != nil {
			return fmt.Errorf("create %s: %w", v.Name, err)
		}
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		ix, err := w.St.SearchIndexes(ctx, store.Variants)
		ready, building := 0, []string{}
		for _, x := range ix {
			if q, _ := x["queryable"].(bool); q {
				ready++
			} else {
				building = append(building, fmt.Sprint(x["name"]))
			}
		}
		if err == nil && ready >= len(mongoVariants) {
			return nil
		}
		progress("indexing", fmt.Sprintf("mongot is building %d of %d indexes: %s", len(mongoVariants)-ready, len(mongoVariants), strings.Join(building, ", ")))
		time.Sleep(time.Second)
	}
	return fmt.Errorf("indexes were not ready within 10 minutes")
}

func (w *mongoWorkshop) Bench(ctx context.Context, items []evalItem, p BenchParams) (BenchResult, error) {
	k, numCandidates, n := p.K, p.NumCandidates, len(items)
	t0 := time.Now()
	out := BenchResult{Queries: n, K: k, NumCandidates: numCandidates, Example: items[0].t.Subject}

	run := func(v Variant, vec []float32, exact bool, kk int) ([]bson.M, time.Duration, error) {
		var qv any = vec
		if v.int8Query {
			qv = int8Vector(vec)
		}
		vs := bson.D{{Key: "index", Value: v.Name}, {Key: "path", Value: v.Path}, {Key: "queryVector", Value: qv}}
		if exact {
			vs = append(vs, bson.E{Key: "exact", Value: true})
		} else {
			vs = append(vs, bson.E{Key: "numCandidates", Value: numCandidates})
		}
		vs = append(vs, bson.E{Key: "limit", Value: kk})
		return w.St.Aggregate(ctx, store.Variants, bson.A{
			bson.D{{Key: "$vectorSearch", Value: vs}},
			bson.D{{Key: "$project", Value: bson.D{{Key: "resolvedKB", Value: 1}, {Key: "rawLength", Value: 1}, {Key: "score", Value: bson.D{{Key: "$meta", Value: "vectorSearchScore"}}}}}},
		})
	}

	// Ground truth: exact (brute-force) cosine on the baseline.
	truth := make([]map[string]bool, len(items))
	for i, it := range items {
		docs, _, err := run(mongoVariants[0], it.vec, true, k)
		if err != nil {
			return out, fmt.Errorf("exact search: %w", err)
		}
		truth[i] = map[string]bool{}
		for _, d := range docs {
			truth[i][fmt.Sprint(d["_id"])] = true
		}
	}
	sizes := w.variantFieldSizes(ctx)
	idxBytes := w.variantIndexBytes(ctx)

	for _, v := range mongoVariants {
		row := BenchRow{Variant: v, Shell: v.Shell(), FieldBytes: sizes[v.Path], IndexBytes: idxBytes[v.Name]}
		var lat []float64
		recall, right, lenSum, lenN := 0.0, 0, 0.0, 0
		for i, it := range items {
			docs, d, err := run(v, it.vec, false, k)
			if err != nil {
				row.Error = firstLineOf(err.Error())
				break
			}
			lat = append(lat, float64(d.Microseconds())/1000)
			got := 0
			votes := map[string]float64{}
			for j, doc := range docs {
				if truth[i][fmt.Sprint(doc["_id"])] {
					got++
				}
				if j < 5 {
					kb, _ := doc["resolvedKB"].(string)
					votes[kb] += 1
				}
				if j == 0 && i == 0 {
					row.TopScore = toF(doc["score"])
				}
				if strings.HasPrefix(v.Path, "emb_raw") {
					lenSum += toF(doc["rawLength"])
					lenN++
				}
			}
			if len(truth[i]) > 0 {
				recall += float64(got) / float64(len(truth[i]))
			}
			best, bv := "", -1.0
			for kb, c := range votes {
				if c > bv {
					best, bv = kb, c
				}
			}
			if best == it.t.TruthKB {
				right++
			}
		}
		if row.Error == "" && len(lat) > 0 {
			row.Recall = recall / float64(len(items))
			row.Accuracy = float64(right) / float64(len(items))
			s := append([]float64(nil), lat...)
			sort.Float64s(s)
			row.P50Ms = s[len(s)/2]
			sum := 0.0
			for _, x := range s {
				sum += x
			}
			row.MeanMs = sum / float64(len(s))
			if lenN > 0 {
				row.MeanLength = lenSum / float64(lenN)
			}
		}
		out.Rows = append(out.Rows, row)
	}
	out.Seconds = time.Since(t0).Seconds()
	return out, nil
}

// variantFieldSizes is the mean BSON size of each stored vector field.
func (w *mongoWorkshop) variantFieldSizes(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	docs, _, err := w.St.Aggregate(ctx, store.Variants, bson.A{
		bson.D{{Key: "$limit", Value: 200}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil},
			{Key: "emb", Value: bson.D{{Key: "$avg", Value: bson.D{{Key: "$bsonSize", Value: bson.D{{Key: "v", Value: "$emb"}}}}}}},
			{Key: "emb_f32", Value: bson.D{{Key: "$avg", Value: bson.D{{Key: "$bsonSize", Value: bson.D{{Key: "v", Value: "$emb_f32"}}}}}}},
			{Key: "emb_i8", Value: bson.D{{Key: "$avg", Value: bson.D{{Key: "$bsonSize", Value: bson.D{{Key: "v", Value: "$emb_i8"}}}}}}},
			{Key: "emb_raw", Value: bson.D{{Key: "$avg", Value: bson.D{{Key: "$bsonSize", Value: bson.D{{Key: "v", Value: "$emb_raw"}}}}}}},
		}}},
	})
	if err != nil || len(docs) == 0 {
		return out
	}
	for k, v := range docs[0] {
		if k != "_id" {
			out[k] = toF(v) - 8 // minus the wrapper document's own overhead
		}
	}
	return out
}

// variantIndexBytes maps each variant index to its size, from mongot's metrics.
func (w *mongoWorkshop) variantIndexBytes(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	ix, err := w.St.SearchIndexes(ctx, store.Variants)
	if err != nil {
		return out
	}
	byID := map[string]string{}
	for _, x := range ix {
		byID[fmt.Sprint(x["id"])] = fmt.Sprint(x["name"])
	}
	for _, m := range w.mongotMetrics(ctx) {
		for id, x := range m.Indexes {
			if name, ok := byID[id]; ok {
				out[name] += x.SizeBytes
			}
		}
	}
	return out
}

func (w *mongoWorkshop) Explain(ctx context.Context, r ExplainRequest, vec []float32) ExplainResult {
	if r.K <= 0 || r.K > 50 {
		r.K = 5
	}
	if r.NumCandidates < r.K {
		r.NumCandidates = r.K * 10
	}
	coll, path, int8q := store.Variants, "emb", false
	if r.Index == "" || r.Index == store.TicketsVector {
		r.Index, coll, path = store.TicketsVector, store.Tickets, "embedding"
	} else {
		found := false
		for _, v := range mongoVariants {
			if v.Name == r.Index {
				path, int8q, found = v.Path, v.int8Query, true
			}
		}
		if !found {
			return ExplainResult{Error: "unknown index " + r.Index}
		}
	}
	var qv any = vec
	if int8q {
		qv = int8Vector(vec)
	}
	vs := bson.D{{Key: "index", Value: r.Index}, {Key: "path", Value: path}, {Key: "queryVector", Value: qv}}
	if r.Exact {
		vs = append(vs, bson.E{Key: "exact", Value: true})
	} else {
		vs = append(vs, bson.E{Key: "numCandidates", Value: r.NumCandidates})
	}
	vs = append(vs, bson.E{Key: "limit", Value: r.K})
	if r.Team != "" {
		vs = append(vs, bson.E{Key: "filter", Value: bson.D{{Key: "team", Value: r.Team}}})
	}
	p := bson.A{bson.D{{Key: "$vectorSearch", Value: vs}}}
	out := ExplainResult{Pipeline: strings.Replace(search.Shell(coll, p), "db."+coll+".aggregate(", "db."+coll+".explain(\"executionStats\").aggregate(", 1)}
	if int8q {
		out.Pipeline = strings.Replace(out.Pipeline, "queryVector: {", "queryVector: /* int8 BinData */ {", 1)
	}
	raw, err := w.St.Explain(ctx, coll, p)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	out.Raw = string(b)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case bson.M:
			if md, ok := x["metadata"].(bson.M); ok {
				if s, ok := md["mongotVersion"].(string); ok {
					out.MongotVersion = s
				}
				if l, ok := md["lucene"].(bson.M); ok {
					out.TotalDocs += toF(l["totalDocs"])
				}
			}
			if q, ok := x["query"].(bson.M); ok {
				if st, ok := q["stats"].(bson.M); ok {
					if c, ok := st["context"].(bson.M); ok {
						out.QueryMs += toF(c["millisElapsed"])
					}
				}
				if out.QueryType == "" {
					out.QueryType = innerQueryType(q)
				}
			}
			if c, ok := x["collectors"].(bson.M); ok {
				if a, ok := c["allCollectorStats"].(bson.M); ok {
					out.CollectorMs += toF(a["millisElapsed"])
					if ic, ok := a["invocationCounts"].(bson.M); ok {
						out.Collected += toF(ic["collect"])
					}
				}
			}
			if segs, ok := x["luceneVectorSegmentStats"].(bson.A); ok {
				for _, s := range segs {
					m, ok := s.(bson.M)
					if !ok {
						continue
					}
					sg := Segment{ID: fmt.Sprint(m["id"]), Docs: toF(m["docCount"]), Visited: toF(m["visitedDocCount"])}
					sg.Mode, _ = m["executionType"].(string)
					for _, k := range []string{"approximateStage", "exactStage"} {
						if st, ok := m[k].(bson.M); ok {
							sg.Ms += toF(st["millisElapsed"])
						}
					}
					if f, ok := m["filterMatchedDocsCount"]; ok {
						sg.Filtered = toF(f)
					}
					out.Segments = append(out.Segments, sg)
					out.Visited += sg.Visited
				}
				return
			}
			for _, v := range x {
				walk(v)
			}
		case bson.A:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(raw)
	// Exact search reports no per-segment graph statistics: it scores every document,
	// which the collector counts.
	if len(out.Segments) == 0 {
		out.Visited = out.Collected
	}
	return out
}

// innerQueryType finds the innermost Lucene query's type — the one that names what
// actually ran (a KNN graph query, or ExactVectorSearchQuery).
func innerQueryType(q bson.M) string {
	t, _ := q["type"].(string)
	if args, ok := q["args"].(bson.M); ok {
		if inner, ok := args["query"].(bson.A); ok && len(inner) > 0 {
			if m, ok := inner[0].(bson.M); ok {
				if it := innerQueryType(m); it != "" && it != "WrappedKnnQuery" && it != "DocAndScoreQuery" {
					return it
				}
			}
		}
	}
	return t
}

func (w *mongoWorkshop) mongotEndpoints(ctx context.Context) []string {
	if v := strings.TrimSpace(os.Getenv("MONGOT_METRICS")); v != "" {
		return strings.Split(v, ",")
	}
	h := w.St.MongotHost(ctx)
	if h == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	}
	return []string{net.JoinHostPort(host, "9946")}
}

// MongotMetrics reads every endpoint.
func (w *mongoWorkshop) mongotMetrics(ctx context.Context) []mongotmetrics.Summary {
	var out []mongotmetrics.Summary
	for _, ep := range w.mongotEndpoints(ctx) {
		out = append(out, mongotmetrics.Fetch(ctx, strings.TrimSpace(ep)))
	}
	return out
}

// Metrics builds the view.
func (w *mongoWorkshop) Metrics(ctx context.Context) MetricsView {
	var v MetricsView
	v.Endpoints = w.mongotEndpoints(ctx)
	v.Mongots = w.mongotMetrics(ctx)
	byID := map[string]mongotmetrics.Index{}
	for _, m := range v.Mongots {
		for id, x := range m.Indexes {
			acc := byID[id]
			acc.SizeBytes += x.SizeBytes
			acc.Docs += x.Docs
			acc.LagMs = math.Max(acc.LagMs, x.LagMs)
			if x.Status != "" {
				acc.Status = x.Status
			}
			byID[id] = acc
		}
		m.Raw = nil
	}
	for _, c := range []string{store.KB, store.Tickets, store.Variants} {
		ix, err := w.St.SearchIndexes(ctx, c)
		if err != nil {
			continue
		}
		for _, x := range ix {
			r := IndexRow{Collection: c, Name: fmt.Sprint(x["name"]), ID: fmt.Sprint(x["id"]), Type: indexType(x),
				Status: fmt.Sprint(x["status"]), Version: x["latestDefinitionVersion"]}
			r.Queryable, _ = x["queryable"].(bool)
			if d, ok := x["latestDefinition"]; ok {
				b, _ := json.Marshal(d)
				r.Definition = string(b)
			}
			if m, ok := byID[r.ID]; ok {
				r.SizeBytes, r.Docs, r.LagMs, r.Mongot = m.SizeBytes, m.Docs, m.LagMs, m.Status
			}
			v.Indexes = append(v.Indexes, r)
		}
	}
	return v
}

// indexType says vectorSearch or search. $listSearchIndexes does not always include a
// type, but a vector index's definition has "fields" and a full-text one "mappings".
func indexType(x bson.M) string {
	if t, ok := x["type"].(string); ok && t != "" {
		return t
	}
	if d, ok := x["latestDefinition"].(bson.M); ok {
		if _, ok := d["fields"]; ok {
			return "vectorSearch"
		}
		return "search"
	}
	return ""
}

const playgroundIndex = "playground"

func pgDef(similarity string, dims int, filter bool) bson.D {
	f := bson.A{bson.D{{Key: "type", Value: "vector"}, {Key: "path", Value: "emb"}, {Key: "numDimensions", Value: dims}, {Key: "similarity", Value: similarity}}}
	if filter {
		f = append(f, bson.D{{Key: "type", Value: "filter"}, {Key: "path", Value: "team"}})
	}
	return bson.D{{Key: "fields", Value: f}}
}

// mongoPlaygroundActions are the buttons, in the order the lesson goes.
var mongoPlaygroundActions = []PlaygroundAction{
	{ID: "create", Label: "1 · Create (cosine, no filter)", def: pgDef("cosine", 384, false),
		Shell: `db.vector_variants.createSearchIndex("playground", "vectorSearch",
  { fields: [ { type: "vector", path: "emb", numDimensions: 384, similarity: "cosine" } ] })`,
		Hint: "Before the index exists the plain probe returns 0 results: no error, just nothing, which is easy to mistake for \"no matches\". Then watch the status move to READY. The filter probe keeps failing, because team is not declared as a filter field."},
	{ID: "add_filter", Label: "2 · Update: add a team filter", def: pgDef("cosine", 384, true),
		Shell: `db.vector_variants.updateSearchIndex("playground",
  { fields: [ { type: "vector", path: "emb", numDimensions: 384, similarity: "cosine" },
              { type: "filter", path: "team" } ] })`,
		Hint: "The index stays queryable throughout: mongot builds the new definition beside the old one and the plain probe never fails. Watch when the filter probe starts working, and what the status says at that moment."},
	{ID: "euclidean", Label: "3 · Update: switch to euclidean", def: pgDef("euclidean", 384, true),
		Shell: `db.vector_variants.updateSearchIndex("playground",
  { fields: [ { type: "vector", path: "emb", numDimensions: 384, similarity: "euclidean" },
              { type: "filter", path: "team" } ] })`,
		Hint: "Same neighbours (the vectors are normalized), different score scale: watch the top score jump when the new definition takes over."},
	{ID: "wrong_dims", Label: "4 · Update: numDimensions 256 (wrong)", def: pgDef("cosine", 256, true),
		Shell: `db.vector_variants.updateSearchIndex("playground",
  { fields: [ { type: "vector", path: "emb", numDimensions: 256, similarity: "cosine" },
              { type: "filter", path: "team" } ] })`,
		Hint: "The vectors have 384 dimensions and the queries send 384. Watch the probes: a dimension mismatch is a hard error, and it shows up before the new definition is even ready."},
	{ID: "drop", Label: "5 · Drop", drop: true,
		Shell: `db.vector_variants.dropSearchIndex("playground")`,
		Hint:  "Watch the probes go from results to errors to 0 results. A $vectorSearch naming an index that does not exist returns nothing rather than failing, so an index someone dropped looks like a search with no matches. The data is untouched."},
}

// PGEvent is one change on the timeline.
func (w *mongoWorkshop) PlaygroundActions() []PlaygroundAction { return mongoPlaygroundActions }

// PlaygroundDo runs an action against the playground search index.
func (w *mongoWorkshop) PlaygroundDo(ctx context.Context, id string) error {
	var act *PlaygroundAction
	for i := range mongoPlaygroundActions {
		if mongoPlaygroundActions[i].ID == id {
			act = &mongoPlaygroundActions[i]
		}
	}
	if act == nil {
		return fmt.Errorf("unknown action %q", id)
	}
	switch {
	case act.drop:
		return w.St.DropSearchIndex(ctx, store.Variants, playgroundIndex)
	case id == "create":
		w.St.DropSearchIndex(ctx, store.Variants, playgroundIndex) // a fresh start
		time.Sleep(500 * time.Millisecond)
		return w.St.CreateSearchIndex(ctx, store.Variants, playgroundIndex, "vectorSearch", act.def)
	default:
		return w.St.UpdateSearchIndex(ctx, store.Variants, playgroundIndex, act.def)
	}
}

// PlaygroundProbe reads the index's status and runs the two probe queries.
func (w *mongoWorkshop) PlaygroundProbe(ctx context.Context, vec []float32) PGEvent {
	probe := func(filter bool) string {
		vs := bson.D{{Key: "index", Value: playgroundIndex}, {Key: "path", Value: "emb"}, {Key: "queryVector", Value: vec},
			{Key: "numCandidates", Value: 50}, {Key: "limit", Value: 3}}
		if filter {
			vs = append(vs, bson.E{Key: "filter", Value: bson.D{{Key: "team", Value: "Connectivity"}}})
		}
		docs, _, err := w.St.Aggregate(ctx, store.Variants, bson.A{bson.D{{Key: "$vectorSearch", Value: vs}},
			bson.D{{Key: "$project", Value: bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "vectorSearchScore"}}}}}}})
		switch {
		case err != nil:
			return "error: " + shorten(causeOf(err.Error()), 140)
		case len(docs) == 0:
			return "0 results"
		default:
			return fmt.Sprintf("%d results · top score %.3f", len(docs), toF(docs[0]["score"]))
		}
	}
	ev := PGEvent{Status: "DOES_NOT_EXIST"}
	if ix, err := w.St.SearchIndexes(ctx, store.Variants); err == nil {
		for _, x := range ix {
			if x["name"] != playgroundIndex {
				continue
			}
			ev.Status = fmt.Sprint(x["status"])
			ev.Queryable, _ = x["queryable"].(bool)
			ev.Version = defVersion(x["latestDefinitionVersion"])
			ev.Detail = pgDetail(x["statusDetail"])
		}
	}
	ev.Plain, ev.Filter = probe(false), probe(true)
	return ev
}

func pgDetail(v any) string {
	arr, ok := v.(bson.A)
	if !ok || len(arr) == 0 {
		return ""
	}
	m, ok := arr[0].(bson.M)
	if !ok {
		return ""
	}
	out := ""
	for _, k := range []string{"mainIndex", "stagedIndex"} {
		if x, ok := m[k].(bson.M); ok {
			ver := ""
			if dv, ok := x["definitionVersion"].(bson.M); ok {
				ver = fmt.Sprint(dv["version"])
			}
			if out != "" {
				out += " · "
			}
			name := "serving"
			if k == "stagedIndex" {
				name = "building"
			}
			out += fmt.Sprintf("%s v%s %v", name, ver, x["status"])
		}
	}
	return out
}

// causeOf keeps what the server actually said: the text after the last "caused by ::".
func defVersion(v any) string {
	if m, ok := v.(bson.M); ok {
		return fmt.Sprint(m["version"])
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
