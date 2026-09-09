package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// pos returns the index of id in order, or -1.
func pos(order []string, id string) int {
	for i, v := range order {
		if v == id {
			return i
		}
	}
	return -1
}

// TestReplicaApplyOrderChain verifies the chained/bidirectional topology
// mysql01 ↔ mysql04 → mysql07 (the intranet stack) orders mysql04 before mysql07,
// so mysql07's seed reflects the upstream (mysql01) GTIDs mysql04 has acquired. A
// downstream replica seeded before its source settled would request GTIDs the source
// holds only in gtid_purged → fatal 1236.
func TestReplicaApplyOrderChain(t *testing.T) {
	fA := designFrame{ID: "fa", Type: "mysql", GTID: true}
	fB := designFrame{ID: "fb", Type: "mysql", GTID: true}
	fC := designFrame{ID: "fc", Type: "mysql", GTID: true}
	m01 := designNode{ID: "m01", Type: "mysql", FrameID: "fa"}
	m04 := designNode{ID: "m04", Type: "mysql", FrameID: "fb"}
	m07 := designNode{ID: "m07", Type: "mysql", FrameID: "fc"}
	links := []replLink{
		{src: m04, dst: m07, srcFrame: fB, dstFrame: fC}, // async mysql04 → mysql07
		{src: m01, dst: m04, srcFrame: fA, dstFrame: fB}, // bidir mysql01 ↔ mysql04
		{src: m04, dst: m01, srcFrame: fB, dstFrame: fA},
	}
	replicas := map[string]bool{"m01": true, "m04": true, "m07": true}
	order := replicaApplyOrder(links, replicas)
	if len(order) != 3 {
		t.Fatalf("order has %d nodes, want 3: %v", len(order), order)
	}
	if pos(order, "m04") > pos(order, "m07") {
		t.Fatalf("mysql04 must be configured before mysql07: %v", order)
	}
}

// TestReplicaApplyOrderPlainPrimary verifies that when the source is a plain cluster
// primary (not itself a replica), it imposes no ordering constraint and the replica is
// still emitted exactly once.
func TestReplicaApplyOrderPlainPrimary(t *testing.T) {
	fA := designFrame{ID: "fa", Type: "mysql", GTID: true}
	fB := designFrame{ID: "fb", Type: "pxc", GTID: true}
	src := designNode{ID: "src", Type: "mysql", FrameID: "fa"} // source only
	dst := designNode{ID: "dst", Type: "pxc", FrameID: "fb"}
	links := []replLink{{src: src, dst: dst, srcFrame: fA, dstFrame: fB}}
	replicas := map[string]bool{"dst": true} // src is not a replica
	order := replicaApplyOrder(links, replicas)
	if len(order) != 1 || order[0] != "dst" {
		t.Fatalf("want [dst], got %v", order)
	}
}

// The "configuring" marker is what makes a card spin instead of showing the green dot that
// means ready, so the two things asserted here are the two that can break it: the state
// machine (set, replace, clear — and the conditional clear that stops one phase switching off
// another's spinner), and the JSON key names, which the canvas reads by name. A rename on
// either side is silent: the dot simply goes green while work is still going on.
func TestConfiguringMarker(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	st, err := app.store.CreateStack("configuring", u.ID, "2h", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	if err := app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n1", State: DeployRunning}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	read := func() provProgress {
		got, err := app.store.GetDeployment(st.ID, "n1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		var p provProgress
		if len(got.Progress) > 0 {
			if err := json.Unmarshal(got.Progress, &p); err != nil {
				t.Fatalf("progress does not parse: %v", err)
			}
		}
		return p
	}

	app.setConfiguring(st.ID, "n1", "the PXC operator is building the cluster")
	if p := read(); !p.Configuring || p.ConfigPhase != "the PXC operator is building the cluster" {
		t.Fatalf("after set: %+v", p)
	}
	// The keys the canvas reads. nodeConfiguring() looks at progress.configuring and the
	// tooltip at progress.configPhase; nothing else connects the two sides.
	got, _ := app.store.GetDeployment(st.ID, "n1")
	for _, key := range []string{`"configuring":true`, `"configPhase":"the PXC operator is building the cluster"`} {
		if !strings.Contains(string(got.Progress), key) {
			t.Errorf("progress JSON is missing %s: %s", key, got.Progress)
		}
	}

	// A later phase replaces the text without the spinner ever stopping.
	app.setConfiguring(st.ID, "n1", "seeding the replica")
	if p := read(); !p.Configuring || p.ConfigPhase != "seeding the replica" {
		t.Fatalf("after a second set: %+v", p)
	}
	// The conditional clear belongs to whoever set that exact phase. The frame's own
	// readiness wait finishing must not switch off the replication phase's spinner.
	app.clearConfiguringIf(st.ID, "n1", "the PXC operator is building the cluster")
	if p := read(); !p.Configuring {
		t.Error("a stale phase must not clear a marker another phase owns")
	}
	app.clearConfiguringIf(st.ID, "n1", "seeding the replica")
	if p := read(); p.Configuring || p.ConfigPhase != "" {
		t.Errorf("the owning phase must clear it: %+v", p)
	}

	// The log survives the marker being set and cleared — they share one progress row, and an
	// earlier version of this rewrote the whole row and dropped the deployment log with it.
	app.replLogln(st.ID, "n1", "line one")
	app.setConfiguring(st.ID, "n1", "phase")
	app.replLogln(st.ID, "n1", "line two")
	app.clearConfiguring(st.ID, "n1")
	if p := read(); len(p.Log) != 2 || p.Log[0] != "line one" || p.Log[1] != "line two" {
		t.Errorf("the deployment log must survive: %+v", p.Log)
	}
	// And an unconditional clear is idempotent on a node that was never marked.
	app.clearConfiguring(st.ID, "n2")
}
