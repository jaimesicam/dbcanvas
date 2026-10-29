package mongo

import "testing"

func TestRegexQuote(t *testing.T) {
	if got := regexQuote("undo:a.b"); got != `undo:a\.b` {
		t.Errorf("got %q", got)
	}
}

// A standalone server offers no lag stressor and says why; a replica set offers
// all. Of the load stressors, only the transactions need a replica set.
func TestDialsAvailability(t *testing.T) {
	for topo, want := range map[string]bool{"standalone": false, "replset": true} {
		e := &Engine{topology: topo}
		for _, d := range e.Dials() {
			needsRS := d.Group == groupLag || d.ID == "hotrow"
			if needsRS && d.Available != want || !needsRS && !d.Available || (!d.Available && d.Why == "") {
				t.Errorf("%s: %s available %v why %q", topo, d.ID, d.Available, d.Why)
			}
		}
	}
}

func TestMemberByAddr(t *testing.T) {
	e := &Engine{cfg: Config{Members: []MemberConfig{{ID: "psmrs-1", Host: "psmrs-1.example.net"}, {ID: "psmrs-2", Host: "psmrs-2.example.net"}}}}
	for addr, want := range map[string]string{"psmrs-2:27017": "psmrs-2", "psmrs-1.example.net:27017": "psmrs-1", "other:27017": ""} {
		if got := e.memberByAddr(addr); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
}
