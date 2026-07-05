package main

import "testing"

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
