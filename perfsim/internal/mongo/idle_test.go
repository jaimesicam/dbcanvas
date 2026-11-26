package mongo

import "testing"

// TestIdleDial: the Idle connections dial is offered, holds 20 connections unless told otherwise,
// and has no hold limit: its connections stay until the dial is turned off.
func TestIdleDial(t *testing.T) {
	e := &Engine{}
	for _, d := range e.loadDials() {
		if d.ID != "idle" {
			continue
		}
		if !d.Available || d.Kind != "toggle" || d.Group != groupConns {
			t.Fatalf("idle dial: %+v", d)
		}
		def := -1
		for _, p := range d.Params {
			if p.Key == "hold" {
				t.Fatal("the idle dial must not release its connections on a timer")
			}
			if p.Key == "conns" {
				def, _ = p.Def.(int)
			}
		}
		if def != 20 {
			t.Fatalf("idle dial default: %d connections, want 20", def)
		}
		return
	}
	t.Fatal("no idle dial")
}
