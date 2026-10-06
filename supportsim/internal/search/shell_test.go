package search

import (
	"strings"
	"testing"
	"time"

	"supportsim/internal/store"
)

func TestShellRendersAVectorPipeline(t *testing.T) {
	v := make([]float32, 384)
	v[0] = 0.25
	p := VectorPipeline(VectorQuery{Collection: store.Tickets, Vector: v, K: 5, NumCandidates: 100,
		Filter: Filter{Status: []string{"resolved"}, Since: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}})
	out := Shell(store.Tickets, p)
	for _, want := range []string{"db.tickets.aggregate([", "$vectorSearch", `index: "tickets_vector"`, "queryVector: [0.2500, 0.0000, 0.0000, 0.0000, … 384 numbers]",
		"numCandidates: 100", "limit: 5", `status: "resolved"`, `createdAt: { $gte: ISODate("2026-10-06T00:00:00Z") }`, `score: { $meta: "vectorSearchScore" }`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	t.Log("\n" + out)
	t.Log("\n" + Shell(store.KB, FusionPipeline(VectorQuery{Collection: store.KB, Vector: v, K: 5, NumCandidates: 100}, "charged twice", false, HybridOpts{Method: "score", VectorWeight: 0.7})))
}

func TestFusionPipelineOptions(t *testing.T) {
	v := make([]float32, 384)
	q := VectorQuery{Collection: store.KB, Vector: v, K: 5, NumCandidates: 100}
	rank := Shell(store.KB, FusionPipeline(q, "x", false, DefaultHybrid()))
	if !strings.Contains(rank, "$rankFusion") || !strings.Contains(rank, "vector: 0.5, keyword: 0.5") || !strings.Contains(rank, "scoreDetails: true") {
		t.Errorf("default fusion:\n%s", rank)
	}
	score := Shell(store.KB, FusionPipeline(q, "x", false, HybridOpts{Method: "score", VectorWeight: 0.8, Normalization: "minMaxScaler"}))
	if !strings.Contains(score, "$scoreFusion") || !strings.Contains(score, `normalization: "minMaxScaler"`) || !strings.Contains(score, "vector: 0.8, keyword: 0.2") {
		t.Errorf("score fusion:\n%s", score)
	}
}

// The app's fallback fusion follows the same arithmetic as the server's stages.
func TestFuseInApp(t *testing.T) {
	vec := []Hit{{ID: 1, Score: 0.9}, {ID: 2, Score: 0.8}}
	kw := []Hit{{ID: 2, Score: 9}, {ID: 3, Score: 3}}
	r := fuseInApp(vec, kw, HybridOpts{Method: "rank", VectorWeight: 0.5}.Normalize(), 3)
	if r[0].ID != 2 || len(r[0].Parts) != 2 {
		t.Fatalf("rank fusion should put the document both lists have first: %+v", r)
	}
	if got, want := r[0].Score, 0.5/62+0.5/61; got-want > 1e-12 || want-got > 1e-12 {
		t.Errorf("rrf score %v want %v", got, want)
	}
	// All weight on vector: keyword cannot reorder anything.
	r = fuseInApp(vec, kw, HybridOpts{Method: "score", VectorWeight: 1, Normalization: "minMaxScaler"}, 3)
	if r[0].ID != 1 {
		t.Errorf("vector weight 1 should rank by vector alone: %+v", r)
	}
}
