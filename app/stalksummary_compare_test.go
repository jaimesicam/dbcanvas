package main

import (
	"strings"
	"testing"
)

// A comparison is read as "did that change help". Getting it wrong endorses a
// change that did nothing, or condemns one that worked — so the noise band and
// the cause-and-effect rules are pinned here with the real numbers they came
// from.

func capture(facts map[string]string, findings map[string]float64) *vsModel {
	m := mdl(nil, map[string]*vsSeries{})
	m.Summary.Facts = facts
	m.Summary.Findings = findings
	m.Source.Host = "ps-01"
	return m
}

// The two real captures: the same server at 128 MiB and at 4 GiB.
func realPair() []*vsModel {
	a := capture(
		map[string]string{"bufferPoolSize": "134217728", "flushMethod": "fsync",
			"redoLogCapacity": "104857600", "syncBinlog": "1", "flushLogAtTrxCommit": "1"},
		map[string]float64{"qps": 1514, "bpMissRatioPct": 8.3, "bpFreePages": 342,
			"innodbReadMiBs": 1841.9, "deviceReadMiBs": 0, "fsyncsPerSec": 381,
			"cpuBusyPct": 38.9, "cpuIowaitPct": 3, "diskUtilPct": 17.7})
	b := capture(
		map[string]string{"bufferPoolSize": "4294967296", "flushMethod": "fsync",
			"redoLogCapacity": "104857600", "syncBinlog": "1", "flushLogAtTrxCommit": "1"},
		map[string]float64{"qps": 4583, "bpMissRatioPct": 0, "bpFreePages": 105660,
			"innodbReadMiBs": 0, "deviceReadMiBs": 0, "fsyncsPerSec": 108,
			"cpuBusyPct": 72.4, "cpuIowaitPct": 0.4, "diskUtilPct": 9.1})
	return []*vsModel{a, b}
}

func verdictByID(vs []vsVerdict, id string) *vsVerdict {
	for i := range vs {
		if vs[i].ID == id {
			return &vs[i]
		}
	}
	return nil
}

func TestBuildComparisonRealPair(t *testing.T) {
	c := buildComparison(realPair())
	if c == nil {
		t.Fatal("expected a comparison")
	}
	// Only the pool changed, so only it should be listed.
	if len(c.Settings) != 1 || c.Settings[0].Key != "bufferPoolSize" {
		t.Fatalf("settings diff = %+v, want only bufferPoolSize", c.Settings)
	}
	// Throughput tripled — well outside the noise band, and an improvement.
	var qps *vsCompareMetric
	for i := range c.Metrics {
		if c.Metrics[i].Key == "qps" {
			qps = &c.Metrics[i]
		}
	}
	if qps == nil || qps.ChangePct == nil {
		t.Fatal("no throughput row")
	}
	if *qps.ChangePct < 190 || !qps.Meaningful || qps.Improved == nil || !*qps.Improved {
		t.Errorf("throughput row wrong: %+v (%.0f%%)", qps, *qps.ChangePct)
	}
	// CPU busy rose, and must NOT be marked a regression — it is the faster
	// server. This is the tile that used to grade the wrong way.
	for _, m := range c.Metrics {
		if m.Key == "cpuBusyPct" && m.Improved != nil {
			t.Errorf("CPU busy must have no better/worse direction, got improved=%v", *m.Improved)
		}
	}
	// The pool verdict must tie the miss collapse to the throughput rise.
	v := verdictByID(c.Verdicts, "comparePool")
	if v == nil || v.Level != vsOK {
		t.Fatalf("pool verdict = %+v, want ok", v)
	}
	if !strings.Contains(v.Detail, "cause and effect") {
		t.Errorf("the pool verdict should tie the two together: %q", v.Detail)
	}
	if verdictByID(c.Verdicts, "compareThroughput") == nil {
		t.Error("no throughput verdict")
	}
}

// TestCompareNoiseBand is the guard against believing a result that is not one.
// A bit-for-bit repeat of one config measured 2.9x different on this hardware
// once the page cache warmed; anything inside the band gets called noise.
func TestCompareNoiseBand(t *testing.T) {
	a := capture(map[string]string{}, map[string]float64{"qps": 1000})
	b := capture(map[string]string{}, map[string]float64{"qps": 1050}) // +5%
	c := buildComparison([]*vsModel{a, b})
	v := verdictByID(c.Verdicts, "compareThroughput")
	if v == nil || v.Level != vsInfo {
		t.Fatalf("a 5%% move should be reported as noise, got %+v", v)
	}
	if !strings.Contains(v.Detail, "noise") {
		t.Errorf("the verdict should say so plainly: %q", v.Detail)
	}
	for _, m := range c.Metrics {
		if m.Key == "qps" && m.Meaningful {
			t.Error("a 5% move must not be flagged meaningful")
		}
	}
}

// TestCompareDurabilityBoughtNothing: giving up durability for a move inside the
// noise band is the case worth being blunt about.
func TestCompareDurabilityBoughtNothing(t *testing.T) {
	a := capture(map[string]string{"syncBinlog": "1", "flushLogAtTrxCommit": "1"},
		map[string]float64{"qps": 1000, "fsyncsPerSec": 400})
	b := capture(map[string]string{"syncBinlog": "0", "flushLogAtTrxCommit": "2"},
		map[string]float64{"qps": 1030, "fsyncsPerSec": 20})
	c := buildComparison([]*vsModel{a, b})
	v := verdictByID(c.Verdicts, "compareDurability")
	if v == nil || v.Level != vsWarn {
		t.Fatalf("durability given up for nothing should warn, got %+v", v)
	}
	if !strings.Contains(v.Detail, "put it back") {
		t.Errorf("the advice should be explicit: %q", v.Detail)
	}

	// And when it did buy throughput, the advice must still state the exposure
	// rather than simply endorsing it.
	b2 := capture(map[string]string{"syncBinlog": "0", "flushLogAtTrxCommit": "2"},
		map[string]float64{"qps": 2500, "fsyncsPerSec": 20})
	c2 := buildComparison([]*vsModel{a, b2})
	v2 := verdictByID(c2.Verdicts, "compareDurability")
	if v2 == nil || !strings.Contains(v2.Detail, "rebuildable") {
		t.Fatalf("the trade must be stated even when it paid off: %+v", v2)
	}
}

// TestCompareRegressions: a change that helps one number and hurts another is a
// trade, and saying so is the difference between advice and cheerleading.
func TestCompareRegressions(t *testing.T) {
	a := capture(map[string]string{}, map[string]float64{
		"qps": 1000, "cpuIowaitPct": 2, "diskUtilPct": 10})
	b := capture(map[string]string{}, map[string]float64{
		"qps": 2000, "cpuIowaitPct": 30, "diskUtilPct": 90})
	c := buildComparison([]*vsModel{a, b})
	v := verdictByID(c.Verdicts, "compareRegressions")
	if v == nil || v.Level != vsWarn {
		t.Fatalf("regressions should be surfaced, got %+v", v)
	}
	if !strings.Contains(v.Detail, "iowait") || !strings.Contains(v.Detail, "Disk") {
		t.Errorf("both regressions should be named: %q", v.Detail)
	}
}

// TestBuildComparisonNWay checks more than two, and that a finding missing from
// one capture does not break the row or invent a value.
func TestBuildComparisonNWay(t *testing.T) {
	models := []*vsModel{
		capture(map[string]string{"bufferPoolSize": "134217728"}, map[string]float64{"qps": 1000, "bpMissRatioPct": 8}),
		capture(map[string]string{"bufferPoolSize": "1073741824"}, map[string]float64{"qps": 2000}),
		capture(map[string]string{"bufferPoolSize": "4294967296"}, map[string]float64{"qps": 4000, "bpMissRatioPct": 0}),
	}
	c := buildComparison(models)
	if len(c.Captures) != 3 {
		t.Fatalf("got %d captures, want 3", len(c.Captures))
	}
	for _, m := range c.Metrics {
		if len(m.Values) != 3 || len(m.Have) != 3 {
			t.Fatalf("row %q has %d values for 3 captures", m.Key, len(m.Values))
		}
		if m.Key == "bpMissRatioPct" && m.Have[1] {
			t.Error("a finding absent from the middle capture must be marked absent, not zero")
		}
		// The change is always first against last, not against the middle.
		if m.Key == "qps" && (m.ChangePct == nil || *m.ChangePct < 290) {
			t.Errorf("qps change should be first-to-last (+300%%), got %+v", m.ChangePct)
		}
	}
}

func TestBuildComparisonNeedsTwo(t *testing.T) {
	if buildComparison(nil) != nil {
		t.Error("no captures should produce no comparison")
	}
	if buildComparison([]*vsModel{capture(nil, map[string]float64{"qps": 1})}) != nil {
		t.Error("one capture is not a comparison")
	}
}
