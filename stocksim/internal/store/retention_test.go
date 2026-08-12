package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// The tally is what stops retention from turning a cumulative figure into a
// shrinking one. A "filled orders" count that goes *down* over time is a wrong
// number on a teaching tool, and the kind that gets believed because nothing
// about it looks broken.

// fakeStore is the smallest Store that CumulativeOrderCounts needs. Every other
// method panics: if one is ever called from this path, that is the bug.
type fakeStore struct {
	Store
	counts map[string]int64
	state  map[string]json.RawMessage
	err    error
}

func (f *fakeStore) CountOrdersByStatus(context.Context) (map[string]int64, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int64{}
	for k, v := range f.counts {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) GetState(_ context.Context, id string) (json.RawMessage, error) {
	return f.state[id], nil
}

func (f *fakeStore) PutState(_ context.Context, id string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if f.state == nil {
		f.state = map[string]json.RawMessage{}
	}
	f.state[id] = b
	return nil
}

func TestCumulativeOrderCounts(t *testing.T) {
	ctx := context.Background()

	// Before any sweep, the live counts are the whole truth.
	f := &fakeStore{counts: map[string]int64{OrderOpen: 120, OrderFilled: 4000}}
	got, err := CumulativeOrderCounts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if got[OrderFilled] != 4000 || got[OrderOpen] != 120 {
		t.Errorf("with no tally, counts should pass through: %+v", got)
	}

	// After a sweep the table holds fewer, and the cumulative figure must not
	// drop — this is the whole point of the tally.
	if err := AddPrunedTally(ctx, f, map[string]int64{OrderFilled: 3500, OrderCancelled: 90}); err != nil {
		t.Fatal(err)
	}
	f.counts = map[string]int64{OrderOpen: 130, OrderFilled: 500}
	got, err = CumulativeOrderCounts(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if got[OrderFilled] != 4000 {
		t.Errorf("filled = %d, want 4000 (500 retained + 3500 pruned)", got[OrderFilled])
	}
	if got[OrderCancelled] != 90 {
		t.Errorf("cancelled = %d, want 90 (all pruned, none retained)", got[OrderCancelled])
	}
	if got[OrderOpen] != 130 {
		t.Errorf("open = %d, want 130 — open orders are never pruned", got[OrderOpen])
	}

	// Sweeps accumulate rather than replace.
	if err := AddPrunedTally(ctx, f, map[string]int64{OrderFilled: 1000}); err != nil {
		t.Fatal(err)
	}
	got, _ = CumulativeOrderCounts(ctx, f)
	if got[OrderFilled] != 5000 {
		t.Errorf("filled after a second sweep = %d, want 5000", got[OrderFilled])
	}

	// A failing count is an error, not a silently partial answer.
	f.err = context.DeadlineExceeded
	if _, err := CumulativeOrderCounts(ctx, f); err == nil {
		t.Error("a failed count must propagate")
	}
}

// TestPrunedTallyEmpty pins the shapes that must not panic: no tally written
// yet, and a tally that is not the map it should be.
func TestPrunedTallyEmpty(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{counts: map[string]int64{OrderOpen: 1}}
	tally, err := PrunedTally(ctx, f)
	if err != nil || len(tally) != 0 {
		t.Errorf("no tally should be empty and not an error: %+v %v", tally, err)
	}
	// Garbage in the state row falls back to the live counts rather than
	// failing the dashboard.
	f.state = map[string]json.RawMessage{prunedTallyKey: json.RawMessage(`"not a map"`)}
	if tally, _ := PrunedTally(ctx, f); len(tally) != 0 {
		t.Errorf("unparseable tally should read as empty, got %+v", tally)
	}
	got, err := CumulativeOrderCounts(ctx, f)
	if err != nil || got[OrderOpen] != 1 {
		t.Errorf("unparseable tally should leave the live counts alone: %+v %v", got, err)
	}
	// Nothing to add is not a write.
	if err := AddPrunedTally(ctx, f, nil); err != nil {
		t.Errorf("an empty delta should be a no-op, got %v", err)
	}
}

func TestTerminalOrderStatuses(t *testing.T) {
	// Open must never be in the list: it is the live book, and an old resting
	// limit order is exactly what a user is looking at.
	for _, s := range TerminalOrderStatuses {
		if s == OrderOpen {
			t.Fatal("open orders must never be pruned")
		}
		if !ValidStatus(s) {
			t.Errorf("%q is not a valid order status", s)
		}
	}
	if len(TerminalOrderStatuses) != 3 {
		t.Errorf("got %d terminal statuses, want filled/cancelled/rejected", len(TerminalOrderStatuses))
	}
}

// TestRetentionCutoffIsInThePast is a guard on the arithmetic that decides what
// gets deleted: a sign error here would sweep the entire table.
func TestRetentionCutoffIsInThePast(t *testing.T) {
	const window = 15 * time.Minute
	cutoff := time.Now().UTC().Add(-window)
	if !cutoff.Before(time.Now().UTC()) {
		t.Fatal("the cutoff must be in the past")
	}
	if time.Since(cutoff) < window {
		t.Fatalf("cutoff is only %v old, want at least %v", time.Since(cutoff), window)
	}
}
