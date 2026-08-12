package sim

import (
	"math/rand"
	"testing"
	"time"

	"stocksim/internal/store"
)

func TestParseWorkingSet(t *testing.T) {
	cases := []struct {
		in   string
		want WorkingSet
	}{
		// Blank is the default, not "off" — the default is the useful
		// behaviour and needing a magic word to get it would be a trap.
		{"", WorkingSet{Pct: DefaultWorkingSetPct}},
		{"off", WorkingSet{}},
		{"None", WorkingSet{}},
		{"0", WorkingSet{}},
		{"0%", WorkingSet{}},
		{"50%", WorkingSet{Pct: 0.5}},
		{" 25 % ", WorkingSet{Pct: 0.25}},
		{"100%", WorkingSet{Pct: 1}},
		// A bare number of one or less is a share; anything larger is a size.
		{"0.5", WorkingSet{Pct: 0.5}},
		{"1", WorkingSet{Pct: 1}},
		{"2G", WorkingSet{Bytes: 2 << 30}},
		{"512Mi", WorkingSet{Bytes: 512 << 20}},
		{"2500000000", WorkingSet{Bytes: 2_500_000_000}},
	}
	for _, c := range cases {
		got, err := ParseWorkingSet(c.in)
		if err != nil {
			t.Errorf("ParseWorkingSet(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseWorkingSet(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"half", "-10%", "150%", "-1", "5X", "%"} {
		if got, err := ParseWorkingSet(bad); err == nil {
			t.Errorf("ParseWorkingSet(%q) = %+v, want an error", bad, got)
		}
	}
}

// TestWorkingSetShareOf pins the conversion the whole feature turns on: what
// share of a measured dataset ends up hot. An absolute size only means anything
// relative to how much there is, and asking for more than exists means all of
// it rather than an over-100% window.
func TestWorkingSetShareOf(t *testing.T) {
	const dataset = 4 << 30
	cases := []struct {
		name string
		ws   WorkingSet
		want float64
	}{
		{"off is nothing", WorkingSet{}, 0},
		{"a share is itself", WorkingSet{Pct: 0.5}, 0.5},
		{"a share is capped at all of it", WorkingSet{Pct: 2}, 1},
		{"a size is a share of the dataset", WorkingSet{Bytes: 1 << 30}, 0.25},
		{"a size larger than the dataset is all of it", WorkingSet{Bytes: 8 << 30}, 1},
		{"a size against an unmeasured dataset is all of it", WorkingSet{Bytes: 1 << 30}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			total := int64(dataset)
			if c.name == "a size against an unmeasured dataset is all of it" {
				total = 0
			}
			if got := c.ws.shareOf(total); got != c.want {
				t.Errorf("shareOf(%d) = %v, want %v", total, got, c.want)
			}
		})
	}
}

// TestWindowPick pins the two properties the readers depend on: every read
// lands inside the hot window, and every security is eligible. A pick that
// escaped the window would be reading data the working set does not claim to
// keep hot, which would make the panel's headline figure a lie.
func TestWindowPick(t *testing.T) {
	to := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	w := window{
		secs: []store.Security{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		from: to.Add(-48 * time.Hour),
		to:   to,
	}
	if !w.ready() {
		t.Fatal("a window with securities and an end should be ready")
	}
	seen := map[string]int{}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		s, at := w.pick(rnd)
		if at.Before(w.from) || at.After(w.to) {
			t.Fatalf("picked %s, outside the window %s–%s", at, w.from, w.to)
		}
		seen[s.ID]++
	}
	for _, s := range w.secs {
		if seen[s.ID] == 0 {
			t.Errorf("security %s was never picked in 500 tries", s.ID)
		}
	}

	// A window with no span at all — history only just started — still has to
	// hand back a usable moment rather than panicking in rand.Int63n(0).
	point := window{secs: w.secs, from: to, to: to}
	if _, at := point.pick(rnd); !at.Equal(to) {
		t.Errorf("zero-span window picked %s, want %s", at, to)
	}

	if (window{}).ready() {
		t.Error("an empty window should not be ready")
	}
}
