package pgsql

import (
	"testing"

	"perfsim/internal/core"
)

// recovery_min_apply_delay reads back in whatever unit PostgreSQL prefers.
func TestSameDelay(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"60s", "1min", true}, {"0", "0", true}, {"30s", "30000ms", true}, {"300s", "5min", true},
		{"60s", "30s", false}, {"0", "1s", false},
	} {
		if sameDelay(c.a, c.b) != c.same {
			t.Errorf("sameDelay(%q, %q) != %v", c.a, c.b, c.same)
		}
	}
}

// A standalone server offers no lag stressor and says why; a cluster offers all.
// The load stressors act on the primary alone, so every topology has them.
func TestDialsAvailability(t *testing.T) {
	for topo, want := range map[string]bool{"standalone": false, "streaming": true} {
		e := &Engine{topology: topo}
		for _, d := range e.Dials() {
			if d.Group == groupLag && d.Available != want || d.Group != groupLag && !d.Available {
				t.Errorf("%s: %s available %v", topo, d.ID, d.Available)
			}
			if !d.Available && d.Why == "" {
				t.Errorf("%s: %s unavailable with no reason", topo, d.ID)
			}
		}
	}
}

// On Spock the pause dial disables subscriptions, and the dials that act on WAL
// replay say why they do not apply.
func TestSpockDials(t *testing.T) {
	e := &Engine{topology: "spock", cfg: Config{Manager: "spock"}}
	got := map[string]core.Dial{}
	for _, d := range e.Dials() {
		got[d.ID] = d
	}
	for _, id := range []string{"rewrite", "conflict", "delay"} {
		if got[id].Available || got[id].Why == "" {
			t.Errorf("%s: available %v why %q", id, got[id].Available, got[id].Why)
		}
	}
	if p := got["pause"]; !p.Available || p.Name != "Disable subscriptions" {
		t.Errorf("pause: %+v", p)
	}
	if !got["bigtxn"].Available {
		t.Error("bigtxn unavailable")
	}
}
