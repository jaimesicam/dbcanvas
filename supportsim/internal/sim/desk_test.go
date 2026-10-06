package sim

import (
	"math"
	"testing"
	"time"

	"supportsim/internal/search"
	"supportsim/internal/store"
)

// Every engine reports cosine on its hits, whatever its own score scale; the in-app
// scan does too.
func TestMirrorScanReportsCosine(t *testing.T) {
	m := search.NewMirror()
	m.Put(store.Tickets, &search.Doc{ID: int64(1), Status: "resolved", Vec: []float32{0.6, 0.8}})
	h := m.Scan(store.Tickets, []float32{1, 0}, 1, search.Filter{})
	if len(h) != 1 || math.Abs(h[0].Cosine-0.6) > 1e-6 || math.Abs(h[0].Score-0.8) > 1e-6 {
		t.Fatalf("%+v", h)
	}
}

// The vote is weighted by similarity: two close neighbours outvote three distant ones.
func TestVoteIsSimilarityWeighted(t *testing.T) {
	ns := []Neighbor{
		{Team: "Billing", Cosine: 0.9}, {Team: "Billing", Cosine: 0.85},
		{Team: "Performance", Cosine: 0.3}, {Team: "Performance", Cosine: 0.3}, {Team: "Performance", Cosine: 0.3},
	}
	p := vote(ns, 5, func(n Neighbor) string { return n.Team })
	if p.Value != "Billing" {
		t.Fatalf("vote = %q, want Billing", p.Value)
	}
	if p.Share < 0.6 || p.Share > 0.7 || p.Cosine != 0.9 {
		t.Errorf("share %.2f cosine %.2f", p.Share, p.Cosine)
	}
	if v := vote(nil, 5, func(n Neighbor) string { return n.Team }); v.Value != "" {
		t.Error("no neighbours, no vote")
	}
}

// The in-app engine is an exact scan that honours the same pre-filters as the index,
// and reports scores on mongot's scale.
func TestMirrorScanFiltersAndScores(t *testing.T) {
	m := search.NewMirror()
	now := time.Now()
	m.Put(store.Tickets, &search.Doc{ID: int64(1), Team: "Billing", Status: "resolved", Customer: "A", CreatedAt: now, Vec: []float32{1, 0}})
	m.Put(store.Tickets, &search.Doc{ID: int64(2), Team: "Billing", Status: "open", Customer: "B", CreatedAt: now, Vec: []float32{0.8, 0.6}})
	m.Put(store.Tickets, &search.Doc{ID: int64(3), Team: "Billing", Status: "resolved", Customer: "A", CreatedAt: now.Add(-time.Hour), Vec: []float32{0, 1}})
	hits := m.Scan(store.Tickets, []float32{1, 0}, 5, search.Filter{Status: []string{"resolved"}})
	if len(hits) != 2 || hits[0].ID != int64(1) || math.Abs(hits[0].Score-1) > 1e-6 || math.Abs(hits[1].Score-0.5) > 1e-6 {
		t.Fatalf("status filter / ordering / score wrong: %+v", hits)
	}
	if hits := m.Scan(store.Tickets, []float32{1, 0}, 5, search.Filter{Customer: "A", Since: now.Add(-time.Minute)}); len(hits) != 1 || hits[0].ID != int64(1) {
		t.Fatalf("customer + since filter wrong: %+v", hits)
	}
	m.Update(int64(2), "resolved", "Performance", "kb")
	if hits := m.Scan(store.Tickets, []float32{1, 0}, 5, search.Filter{Team: "Performance"}); len(hits) != 1 || hits[0].KB != "kb" {
		t.Fatalf("update not applied: %+v", hits)
	}
	m.Remove(int64(2))
	if m.Len(store.Tickets) != 2 {
		t.Fatal("remove")
	}
}
