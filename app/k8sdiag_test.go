package main

import "testing"

// The collector's -resource flag takes a fixed set. The two community PostgreSQL
// operators are not Percona's, so it has no mode for them — asking for one would
// fail the capture outright, where "none" still collects every core resource,
// every pod log and whatever CRs it can see, which is all Operator Summary reads.
func TestK8sCollectResource(t *testing.T) {
	for _, tc := range []struct{ operator, want string }{
		{"pxc", "pxc"},
		{"ps", "ps"},
		{"psmdb", "psmdb"},
		// NOT "pg": that mode collects the v1 CRD group, and DBCanvas installs the
		// v2 operator. Verified against a live 3.0.0 deployment — `-resource pg`
		// produced an empty perconapgclusters.pg.percona.com.yaml while the cluster
		// was running fine, so the capture silently held no custom resources.
		{"pg", "pgv2"},
		{"cnpg", "none"}, // CloudNativePG — not a Percona operator
		{"pgo", "none"},  // Crunchy PGO — likewise
		{"", "none"},     // a cluster with no operator at all
		{"nonsense", "none"},
	} {
		if got := k8sCollectResource(tc.operator); got != tc.want {
			t.Errorf("k8sCollectResource(%q) = %q, want %q", tc.operator, got, tc.want)
		}
	}
}

// A kept dump survives the stack it came from — that is the point of keeping it.
func TestK8sDumpStoreRoundTrip(t *testing.T) {
	app := newTestApp(t)
	d := k8sDump{
		StackID: 7, FrameID: "frame-1", Cluster: "k8s-s7", Operator: "pxc",
		CapturedAt: "2026-09-03T23:09:00Z", SizeBytes: 102357, Path: "/tmp/x.tar.gz",
	}
	id, err := app.store.InsertK8sDump(d)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := app.store.K8sDump(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Cluster != d.Cluster || got.Operator != d.Operator || got.SizeBytes != d.SizeBytes {
		t.Errorf("round trip = %+v, want %+v", got, d)
	}

	list, err := app.store.K8sDumps(7, "frame-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("K8sDumps = %v, %v; want one row", list, err)
	}
	// A dump for another frame in the same stack must not leak into this one's list.
	if _, err := app.store.InsertK8sDump(k8sDump{StackID: 7, FrameID: "frame-2", CapturedAt: "2026-09-03T23:10:00Z", Path: "/tmp/y"}); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if list, _ = app.store.K8sDumps(7, "frame-1"); len(list) != 1 {
		t.Errorf("K8sDumps(frame-1) = %d rows, want 1 — the frame filter is not applied", len(list))
	}
	if all, err := app.store.AllK8sDumps(); err != nil || len(all) != 2 {
		t.Errorf("AllK8sDumps = %d rows (%v), want 2", len(all), err)
	}

	if err := app.store.DeleteK8sDump(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := app.store.K8sDump(id); err == nil {
		t.Error("want an error reading a deleted dump")
	}
}

// Newest first, so the picker's top row is the capture just taken.
func TestK8sDumpsAreNewestFirst(t *testing.T) {
	app := newTestApp(t)
	for _, at := range []string{"2026-09-01T10:00:00Z", "2026-09-03T10:00:00Z", "2026-09-02T10:00:00Z"} {
		if _, err := app.store.InsertK8sDump(k8sDump{StackID: 1, FrameID: "f", CapturedAt: at, Path: "/tmp/" + at}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := app.store.K8sDumps(1, "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].CapturedAt != "2026-09-03T10:00:00Z" || list[2].CapturedAt != "2026-09-01T10:00:00Z" {
		t.Errorf("order = %+v, want newest first", list)
	}
}
