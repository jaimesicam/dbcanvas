package valkey

import "testing"

func TestField(t *testing.T) {
	info := "# Replication\r\nrole:slave\r\nmaster_host:192.0.2.4\r\nmaster_link_status:up\r\n"
	if field(info, "role", "") != "slave" || field(info, "master_link_status", "") != "up" || field(info, "nope", "x") != "x" {
		t.Error("field")
	}
}

// A standalone server offers no lag stressor; DEBUG SLEEP needs DEBUG allowed on a replica.
func TestDialsAvailability(t *testing.T) {
	avail := func(e *Engine) map[string]bool {
		out := map[string]bool{}
		for _, d := range e.Dials() {
			out[d.ID] = d.Available
			if !d.Available && d.Why == "" {
				t.Errorf("%s unavailable with no reason", d.ID)
			}
		}
		return out
	}
	if a := avail(&Engine{topology: "standalone"}); a["bigvalues"] || a["bigdel"] || a["debugsleep"] || !a["storm"] || !a["cpu"] {
		t.Errorf("standalone: %v", a)
	}
	e := &Engine{topology: "cluster", primaryOf: map[string]string{"r1": "p1"}, debugOK: map[string]bool{"r1": false}}
	if a := avail(e); !a["bigvalues"] || !a["bigdel"] || a["debugsleep"] {
		t.Errorf("cluster without DEBUG: %v", a)
	}
	e.debugOK["r1"] = true
	if a := avail(e); !a["debugsleep"] {
		t.Errorf("cluster with DEBUG: %v", a)
	}
}

// Coordinated failover is offered, and the default, from Valkey 9.0 on.
func TestFailoverModes(t *testing.T) {
	for v, want := range map[string]bool{"9.1.2": true, "9.0.0": true, "8.1.4": false, "7.2.11": false, "": false} {
		if coordinatedFailover(v) != want {
			t.Errorf("%s: coordinated %v", v, !want)
		}
		d := failoverDial("vk", coordinatedFailover(v))
		if got := len(d.Params) == 1 && d.Params[0].Def == "coordinated"; got != want {
			t.Errorf("%s: params %+v", v, d.Params)
		}
	}
}
