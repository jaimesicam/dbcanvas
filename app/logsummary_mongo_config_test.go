package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are real: the first 120 lines of three shard members' logs as dbcanvas
// deployed them (nothing pins cacheSizeGB, so each opened WiredTiger at 14527M on a
// 29.4 GiB host), and the same member's later startup after the cache was pinned to 6 GiB.
func lsConfigBundle(t *testing.T, names ...string) *lsBundle {
	t.Helper()
	var in []lsInput
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", "logsummary", "m08-config", n))
		if err != nil {
			t.Fatalf("fixture %s: %v", n, err)
		}
		in = append(in, lsInput{Name: n, Origin: "upload", Data: b})
	}
	return lsBuild(in)
}

func lsFindingByID(b *lsBundle, id string) *lsFinding {
	for i := range b.Finding {
		if b.Finding[i].ID == id {
			return &b.Finding[i]
		}
	}
	return nil
}

// Three members that each took half the machine is the finding this whole pass exists for,
// and the severity has to reflect that it is provable rather than suspected: the startup
// options say nobody set cacheSizeGB.
func TestMongoConfigCatchesDerivedCachesOnOneHost(t *testing.T) {
	b := lsConfigBundle(t, "default-sh2.log", "default-sh3.log", "default-sh4.log")
	f := lsFindingByID(b, "mongo-cache-config")
	if f == nil {
		t.Fatal("no cache-budget finding from three default-configured members")
	}
	if f.Sev != "bad" {
		t.Errorf("derived caches on one host should be bad, got %q", f.Sev)
	}
	if !strings.Contains(f.Title, "42.6 GiB") {
		t.Errorf("the total is the point of the finding, and it is 3 × 14.19 GiB: %q", f.Title)
	}
	if !strings.Contains(f.Advice, "cacheSizeGB") {
		t.Error("the advice must name the setting to change")
	}
	for _, s := range b.Sources {
		if s.MongoCfg == nil {
			t.Fatalf("%s: no configuration read from the log", s.Name)
		}
		if s.MongoCfg.Pinned {
			t.Errorf("%s: cacheSizeGB is not set in these options, so it is derived", s.Name)
		}
		if s.MongoCfg.CacheMB != 14527 {
			t.Errorf("%s: cache_size read as %v, want 14527", s.Name, s.MongoCfg.CacheMB)
		}
		if s.MongoCfg.EvictMax != 4 {
			t.Errorf("%s: eviction threads_max read as %d, want 4", s.Name, s.MongoCfg.EvictMax)
		}
	}
}

// A cache somebody chose is not a defect, and saying so at the same volume as the default
// case would make the page cry wolf. It stays informational, and still says what the total
// means.
func TestMongoConfigIsInformationalWhenTheCacheWasChosen(t *testing.T) {
	b := lsConfigBundle(t, "pinned-sh3.log")
	f := lsFindingByID(b, "mongo-cache-config")
	if f == nil {
		t.Fatal("no cache finding from a pinned member")
	}
	if f.Sev != "info" {
		t.Errorf("a deliberately sized cache is informational, got %q", f.Sev)
	}
	if s := b.Sources[0]; s.MongoCfg == nil || !s.MongoCfg.Pinned || s.MongoCfg.CacheMB != 6144 {
		t.Errorf("expected a pinned 6144M cache, got %+v", s.MongoCfg)
	}
}

// The deprecation warning for the ticket parameters is written by the FTDC thread
// enumerating server parameters, on every 8.0 member, whether or not anybody set them.
// Reading it as intent would fire this finding on every MongoDB log in existence.
func TestMongoConfigDoesNotInventPinnedTickets(t *testing.T) {
	b := lsConfigBundle(t, "default-sh2.log", "default-sh3.log", "default-sh4.log")
	if f := lsFindingByID(b, "mongo-tickets-pinned"); f != nil {
		t.Errorf("no member here pins ticket counts, but the finding fired: %s", f.Detail)
	}
	for _, s := range b.Sources {
		if len(s.MongoCfg.Tickets) != 0 {
			t.Errorf("%s: read ticket settings that are not in the options: %v", s.Name, s.MongoCfg.Tickets)
		}
	}
}

// The server's own startup objections are configuration findings by construction: it has
// already decided they are wrong, and nobody ever scrolls back to the top of the log.
func TestMongoConfigSurfacesStartupWarnings(t *testing.T) {
	b := lsConfigBundle(t, "default-sh2.log")
	f := lsFindingByID(b, "mongo-startup-warnings")
	if f == nil {
		t.Fatal("the swappiness warning in this fixture produced no finding")
	}
	if !strings.Contains(strings.ToLower(f.Detail), "swappiness") {
		t.Errorf("the detail should quote what the server said, got %q", f.Detail)
	}
}

// A log that spans a restart carries both configurations, and every rate in it straddles
// the change. Saying so is the difference between comparing two runs and averaging them.
func TestMongoConfigNoticesACacheResize(t *testing.T) {
	def, err := os.ReadFile(filepath.Join("testdata", "logsummary", "m08-config", "default-sh3.log"))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := os.ReadFile(filepath.Join("testdata", "logsummary", "m08-config", "pinned-sh3.log"))
	if err != nil {
		t.Fatal(err)
	}
	b := lsBuild([]lsInput{{Name: "sh3.log", Origin: "upload", Data: append(append([]byte{}, def...), pin...)}})
	f := lsFindingByID(b, "mongo-cache-changed")
	if f == nil {
		t.Fatal("a member restarted with a different cache size produced no finding")
	}
	if s := b.Sources[0]; s.MongoCfg.Startups != 2 || !s.MongoCfg.Changed || s.MongoCfg.CacheMB != 6144 {
		t.Errorf("expected two startups ending at 6144M, got %+v", s.MongoCfg)
	}
}
