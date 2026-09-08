package main

// logsummary_mongo_slow_test.go — the slow-query pass, against the workload fixture.
//
// m07-workload is the same three-member 8.0.28-12 replica set as the other m* scenarios,
// captured while the Stock Market Sim drove it at its High level with a deliberately
// undersized WiredTiger cache (768 MiB, then 256 MiB) — 32 workers, a 6 GiB dataset, 1,500
// extra collections, unindexed scans, an index build over 11 million documents, a stalled
// secondary, a stepdown and a SIGKILL. The member logs ran at verbosity 1 and reached
// 10.5 GiB each in 86 minutes; these fixtures are a sample of that: every meaningful record
// capped at six repeats, plus 300 slow-query lines including the scans and the ones over a
// second. Nothing was rewritten.

import (
	"strings"
	"testing"
)

func TestMongoSlowScanAddsUpTheLines(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	var seen int
	for _, s := range b.Sources {
		if s.Mongo == nil {
			continue
		}
		seen++
		m := s.Mongo
		if m.Ops == 0 {
			t.Errorf("%s: no slow queries counted in a fixture full of them", s.Name)
		}
		// Every operation examined at least as much as it returned, and every counter
		// that is a sum of per-operation numbers has to be at least as large as the
		// largest single one.
		if m.Docs < m.Returned && m.Returned > 0 && m.Docs > 0 {
			t.Errorf("%s: examined %d < returned %d", s.Name, m.Docs, m.Returned)
		}
		if m.Ops >= 20 && m.Millis <= 0 {
			t.Errorf("%s: %d slow queries and no duration accumulated", s.Name, m.Ops)
		}
		if m.WaitedMs > m.Millis {
			t.Errorf("%s: waited %d ms of %d ms total", s.Name, m.WaitedMs, m.Millis)
		}
		if len(m.Namespaces) == 0 {
			t.Errorf("%s: no namespaces — the ns attribute was not read", s.Name)
		}
		if len(m.Plans) == 0 {
			t.Errorf("%s: no plans — planSummary was not read", s.Name)
		}
		if m.Debug == 0 {
			t.Errorf("%s: no debug lines counted in a verbosity-1 log", s.Name)
		}
	}
	if seen == 0 {
		t.Fatal("no source carried a mongo summary")
	}
}

// The bytes a slow operation read off the device are the one number FTDC cannot attribute
// to a collection, so losing them silently would remove the whole point of the pass.
func TestMongoSlowScanReadsStorageBytes(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	total := int64(0)
	for _, s := range b.Sources {
		if s.Mongo != nil {
			total += s.Mongo.Bytes
		}
	}
	if total == 0 {
		t.Fatal("no storage.data.bytesRead was read from any source")
	}
}

// A slow operation over a second earns a row on the timeline; the millions under it do not.
func TestMongoSlowKeepsOnlyTheWorstAsEvents(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	slow := 0
	for _, e := range b.Events {
		if e.Code == "51803" {
			slow++
			if e.Members < lsSlowEventMs {
				t.Errorf("event #%d kept a %d ms operation, threshold is %d", e.No, e.Members, lsSlowEventMs)
			}
			if e.Message == "" {
				t.Errorf("event #%d has no namespace or plan on it", e.No)
			}
		}
	}
	if slow == 0 {
		t.Fatal("not one slow operation reached the timeline")
	}
	if slow > lsSlowKeep*len(b.Sources) {
		t.Errorf("%d slow-query events — the cap is %d per source", slow, lsSlowKeep)
	}
}

// A collection scan over twelve documents is the right plan. The finding has to weigh what
// the scans COST, or it fires on every healthy member that ever ran collStats.
func TestMongoCollscanFindingWeighsCostNotCount(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	var found *lsFinding
	for _, f := range lsFindings(b) {
		if strings.HasPrefix(f.ID, "mongo-collscan") {
			f := f
			found = &f
			break
		}
	}
	if found == nil {
		t.Fatal("no collection-scan finding from a workload full of them")
	}
	if !strings.Contains(found.Detail, "documents to return") {
		t.Errorf("detail does not state the cost: %q", found.Detail)
	}
	// A source whose scans examined almost nothing must not produce one.
	cheap := &lsBundle{Sources: []lsSource{{Idx: 0, Name: "cheap.log", Node: "mongo09",
		Mongo: &lsMongoStats{Ops: 5000, Collscans: 4000, CollDocs: 4000, CollRet: 4000, Docs: 4000, Returned: 4000}}}}
	if got := lsFindingMongoCollscan(cheap); len(got) != 0 {
		t.Errorf("4000 one-document scans produced a finding: %q", got[0].Title)
	}
}

// The index build is a span, not a point: the fixture holds a real one that ran for minutes
// across all three members.
func TestMongoIndexBuildFindingMeasuresTheBuild(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	var f *lsFinding
	for _, x := range lsFindings(b) {
		if x.ID == "mongo-index-build" {
			x := x
			f = &x
			break
		}
	}
	if f == nil {
		t.Fatal("no index-build finding")
	}
	if f.Until <= f.At {
		t.Errorf("build has no duration: %v → %v", f.At, f.Until)
	}
	if !strings.Contains(f.Detail, "price_ticks") {
		t.Errorf("the collection being indexed is not named: %q", f.Detail)
	}
}

// The new catalogue entries: each of these ids was dropped as noise before, and each is a
// class of problem that has no other record.
func TestMongoNewRulesClassify(t *testing.T) {
	b := lsLoadScenario(t, "m07-workload")
	want := map[string]string{
		"3873113": "sync source", // could not find one
		"22572":   "pool",        // dropped pooled connections
		"20438":   "index build", // registering
		"20345":   "index build", // done
	}
	seen := map[string]bool{}
	for _, e := range b.Events {
		if _, ok := want[e.Code]; ok {
			seen[e.Code] = true
			if e.Label == "" || e.Meaning == "" {
				t.Errorf("id %s classified without a label or a meaning", e.Code)
			}
		}
	}
	for code, what := range want {
		if !seen[code] {
			t.Errorf("id %s (%s) is still being dropped", code, what)
		}
	}
}

// A log at verbosity 1 costs real disk on the same device the database is using, and the
// page should say so rather than leave the reader to notice a 10 GiB file.
func TestMongoLogVolumeFinding(t *testing.T) {
	b := &lsBundle{Sources: []lsSource{{
		Idx: 0, Name: "mongod.log", Node: "mongo01", Engine: pktEngineMongoDB,
		Bytes: 900 << 20, Records: 100000, FirstTS: 1787000000, LastTS: 1787000600,
		Mongo: &lsMongoStats{Ops: 10, Debug: 60000},
	}}}
	got := lsFindingMongoLogVolume(b)
	if len(got) != 1 {
		t.Fatalf("want one finding, got %d", len(got))
	}
	if got[0].Sev != lsSevWarn {
		t.Errorf("1.5 MiB/s of log reads as %q", got[0].Sev)
	}
	if !strings.Contains(got[0].Detail, "debug-level") {
		t.Errorf("detail does not say why the file is large: %q", got[0].Detail)
	}
}
