package main

import "testing"

func TestSeaweedBuckets(t *testing.T) {
	// The list wins, blanks and duplicates are dropped, and the cap holds.
	n := designNode{Buckets: []string{"one", " two ", "", "one", "three"}}
	got := seaweedBuckets(n)
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buckets = %v, want %v", got, want)
		}
	}
	// A design saved before the list existed still has its bucket.
	if got := seaweedBuckets(designNode{Bucket: "legacy"}); len(got) != 1 || got[0] != "legacy" {
		t.Errorf("the legacy single bucket was lost: %v", got)
	}
	// …and it is not duplicated when it is also the first of the list.
	if got := seaweedBuckets(designNode{Bucket: "one", Buckets: []string{"one", "two"}}); len(got) != 2 {
		t.Errorf("the legacy bucket was counted twice: %v", got)
	}
	// Ten is the ceiling.
	var many []string
	for i := 0; i < 15; i++ {
		many = append(many, string(rune('a'+i))+"-bucket")
	}
	if got := seaweedBuckets(designNode{Buckets: many}); len(got) != maxSeaweedBuckets {
		t.Errorf("got %d buckets, want the %d cap", len(got), maxSeaweedBuckets)
	}
}

func TestPickSeaweedBucket(t *testing.T) {
	cfg := seaweedConfig{Bucket: "one", Buckets: []string{"one", "two"}}
	if got := pickSeaweedBucket(cfg, "two"); got != "two" {
		t.Errorf("a consumer's chosen bucket was ignored: %q", got)
	}
	if got := pickSeaweedBucket(cfg, ""); got != "one" {
		t.Errorf("no choice must mean the node's default: %q", got)
	}
	// A bucket the node does not have falls back to the default rather than configuring a backup
	// against a bucket that does not exist (validation blocks this design anyway).
	if got := pickSeaweedBucket(cfg, "nope"); got != "one" {
		t.Errorf("an unknown bucket must fall back to the default: %q", got)
	}
}

func TestSeaweedBucketIssues(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "s1", Type: "seaweedfs", Label: "seaweedfs-01", Buckets: []string{"pg", "mongo"}},
	}}
	if iss := seaweedBucketIssues("Patroni cluster p1", "s1", "mongo", doc); len(iss) != 0 {
		t.Errorf("a bucket the node has must validate: %v", iss)
	}
	if iss := seaweedBucketIssues("Patroni cluster p1", "s1", "", doc); len(iss) != 0 {
		t.Errorf("no choice (the default) must validate: %v", iss)
	}
	// A backup pointed at a bucket that will not exist fails at the first upload, not at deploy —
	// so it has to be caught here.
	iss := seaweedBucketIssues("Patroni cluster p1", "s1", "typo", doc)
	if len(iss) != 1 || iss[0].Level != "error" {
		t.Errorf("a bucket the node does not create must be an error: %v", iss)
	}
}
