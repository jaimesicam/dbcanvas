package main

import (
	"strings"
	"testing"
	"time"
)

// watchFixture is a stack with a two-member replication frame, deployed.
func watchFixture(t *testing.T) (*App, Stack) {
	t.Helper()
	app := newTestApp(t)
	u, _ := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)
	design := `{"nodes":[{"id":"a","type":"mysql","label":"db-1","frameId":"f"},{"id":"b","type":"mysql","label":"db-2","frameId":"f"}],
	  "frames":[{"id":"f","type":"mysql","label":"repl"}],"edges":[]}`
	st, err := app.store.CreateStack("w", u.ID, "2h", nil, []byte(design))
	if err != nil {
		t.Fatal(err)
	}
	app.store.SetStackStatus(st.ID, StackDeployed)
	st, _ = app.store.GetStack(st.ID)
	return app, st
}

func node(role string, lag float64, problems ...string) *liveNode {
	r := &liveRole{Role: role, Access: "ro", Problems: problems}
	if role == "primary" {
		r.Access = "rw"
	}
	if lag >= 0 {
		r.LagSec = &lag
	}
	return &liveNode{State: "running", Role: r}
}

func TestWatchAlertHysteresis(t *testing.T) {
	app, st := watchFixture(t)
	t0 := time.Now()
	pass := func(i int, b *liveNode) {
		app.watchStack(st, map[string]*liveNode{"a": node("primary", -1), "b": b}, t0.Add(time.Duration(i)*30*time.Second))
	}
	pass(0, node("replica", 0, "replication from db-1: applier stopped"))
	if al, _ := app.store.LiveAlerts(st.ID); len(al) != 0 {
		t.Fatalf("one bad sample opened %v", al)
	}
	pass(1, node("replica", 0, "replication from db-1: applier stopped"))
	al, _ := app.store.LiveAlerts(st.ID)
	if len(al) != 1 || al[0].Kind != "replication" || al[0].NodeID != "b" || al[0].Fix != "rebuild" {
		t.Fatalf("alerts = %+v", al)
	}
	pass(2, node("replica", 0))
	if al, _ := app.store.LiveAlerts(st.ID); len(al) != 1 {
		t.Fatal("one clear sample closed it")
	}
	pass(3, node("replica", 0))
	if al, _ := app.store.LiveAlerts(st.ID); len(al) != 0 {
		t.Fatalf("still open: %+v", al)
	}
	ev, _ := app.store.LiveEvents(st.ID, 0, 50)
	var kinds []string
	for _, e := range ev {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "resolved,alert" {
		t.Fatalf("events = %v", kinds)
	}
}

func TestWatchDownOpensAtOnceAndUserStopIsNot(t *testing.T) {
	app, st := watchFixture(t)
	down := &liveNode{State: "running", Role: &liveRole{Down: true, Err: "ERROR 2002"}}
	app.watchStack(st, map[string]*liveNode{"a": node("primary", -1), "b": down}, time.Now())
	al, _ := app.store.LiveAlerts(st.ID)
	if len(al) != 1 || al[0].Kind != "down" || al[0].Severity != "error" {
		t.Fatalf("alerts = %+v", al)
	}
	// A replication problem on a node that is down is the same problem: not a second alert.
	for _, a := range al {
		if a.Kind == "replication" {
			t.Fatal("down node also raised replication")
		}
	}
}

func TestWatchUnplannedFailover(t *testing.T) {
	app, st := watchFixture(t)
	t0 := time.Now()
	app.watchStack(st, map[string]*liveNode{"a": node("primary", -1), "b": node("replica", 0)}, t0)
	app.watchStack(st, map[string]*liveNode{"a": node("replica", 0), "b": node("primary", -1)}, t0.Add(30*time.Second))
	ev, _ := app.store.LiveEvents(st.ID, 0, 50)
	found := false
	for _, e := range ev {
		if e.Kind == "failover" && strings.Contains(e.Title, "db-2 is now primary (was db-1)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failover event: %+v", ev)
	}
	// The same move right after DBCanvas switched it is a switchover.
	markPlannedSwitch(st.ID)
	app.watchStack(st, map[string]*liveNode{"a": node("primary", -1), "b": node("replica", 0)}, time.Now().Add(time.Minute))
	ev, _ = app.store.LiveEvents(st.ID, 0, 1)
	if len(ev) == 0 {
		t.Fatal("no events")
	}
	ev, _ = app.store.LiveEvents(st.ID, 0, 50)
	planned := 0
	for _, e := range ev {
		if e.Kind == "switchover" {
			planned++
		}
	}
	if planned != 1 {
		t.Fatalf("switchover events = %d: %+v", planned, ev)
	}
}

func TestWatchRatesAndSamples(t *testing.T) {
	app, st := watchFixture(t)
	t0 := time.Now()
	mk := func(q int64, at int64) *liveNode {
		n := node("primary", -1)
		n.Role.Load = &liveLoad{Queries: &q, AtMs: at}
		return n
	}
	app.watchStack(st, map[string]*liveNode{"a": mk(1000, 1_000_000)}, t0)
	app.watchStack(st, map[string]*liveNode{"a": mk(4000, 1_030_000)}, t0.Add(30*time.Second))
	s, _ := app.store.LiveSamples(st.ID, 0)
	if len(s["a"]) != 2 || s["a"][1].QPS == nil || *s["a"][1].QPS != 100 {
		t.Fatalf("samples = %+v", s["a"])
	}
}

func TestAlertRulesNormalize(t *testing.T) {
	r := alertRules{LagSec: -3, DiskPct: 150, ConnPct: 5}.normalize()
	if r.LagSec != 0 || r.DiskPct != 99 || r.ConnPct != 10 {
		t.Fatalf("%+v", r)
	}
	if clampWatchSeconds(5) != minWatchSeconds || clampWatchSeconds(0) != 0 || clampWatchSeconds(9999) != maxWatchSeconds {
		t.Fatal("clamp")
	}
}

func TestWatchFailoverAcrossRestart(t *testing.T) {
	app, st := watchFixture(t)
	watchState.mu.Lock()
	delete(watchState.planned, st.ID)
	watchState.mu.Unlock()
	t0 := time.Now()
	app.watchStack(st, map[string]*liveNode{"a": node("primary", -1), "b": node("replica", 0)}, t0)
	// The app restarts: everything in memory is gone.
	watchState.mu.Lock()
	delete(watchState.stacks, st.ID)
	delete(watchState.primary, st.ID)
	delete(watchState.planned, st.ID) // another test's switch, on the same stack id
	watchState.mu.Unlock()
	app.watchStack(st, map[string]*liveNode{"a": {State: "unreachable"}, "b": node("primary", -1)}, t0.Add(30*time.Second))
	ev, _ := app.store.LiveEvents(st.ID, 0, 50)
	for _, e := range ev {
		if e.Kind == "failover" {
			return
		}
	}
	t.Fatalf("no failover across the restart: %+v", ev)
}
