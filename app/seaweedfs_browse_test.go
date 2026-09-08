package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCleanSeaweedPath(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"/pbm/cluster1/", "pbm/cluster1", true},
		{" backup/db ", "backup/db", true},
		{"../../etc", "", false},   // traversal out of the bucket is refused, not cleaned away
		{"pbm/../../x", "", false}, //
		{strings.Repeat("a", 2000), "", false},
	} {
		got, err := cleanSeaweedPath(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("cleanSeaweedPath(%q) = (%q, err=%v), want (%q, ok=%v)", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// The filer's directory listing, as a real SeaweedFS 4.39 node answers it (the fields we read).
func TestFilerListingParsesDirsAndFiles(t *testing.T) {
	const body = `{"Path":"/buckets/probe","Entries":[
	  {"FullPath":"/buckets/probe/dir1","Mtime":"2026-07-14T04:24:07.514165141Z","Mode":2147484153,"FileSize":0},
	  {"FullPath":"/buckets/probe/top.txt","Mtime":"2026-07-14T04:24:07.520042561Z","Mode":432,"FileSize":12}],
	  "LastFileName":"top.txt","ShouldDisplayLoadMore":true}`
	var l filerListing
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(l.Entries) != 2 || !l.ShouldDisplayLoadMore || l.LastFileName != "top.txt" {
		t.Fatalf("listing = %+v", l)
	}
	// The directory bit is the top bit of Go's FileMode — the only thing that tells a folder from a
	// zero-byte object, and what a click descends into.
	if !os.FileMode(l.Entries[0].Mode).IsDir() {
		t.Error("dir1 should be a directory")
	}
	if os.FileMode(l.Entries[1].Mode).IsDir() || l.Entries[1].FileSize != 12 {
		t.Error("top.txt should be a 12-byte file")
	}
}
