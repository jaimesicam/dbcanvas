package main

// ftdc_charts_test.go — the charts and the capture header added after the anatomy pass.
//
// The fixture is 374 seconds of a real Percona Server for MongoDB 8.0.28-12 replica-set
// PRIMARY under load: two chunks lifted out of a 5,218-sample capture, keeping the type-0
// metadata document with them. It is the smallest thing that exercises all five of the new
// charts, because every one of them needs something the standalone fixture next door does
// not have — a replica set, 8.0's queues.execution counters, opWorkingTime, netstat, and
// enough samples for a rate to exist at all.

import (
	"os"
	"strings"
	"testing"
)

const ftdcRSFixture = "testdata/ftdc-rs/metrics.2026-08-18T14-48-51Z-00000"

func ftdcLoadRS(t *testing.T) *fdModel {
	t.Helper()
	b, err := os.ReadFile(ftdcRSFixture)
	if err != nil {
		t.Skipf("no replica-set fixture: %v", err)
	}
	d, err := ftdcParse([][]byte{b})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return ftdcSummarise(d)
}

func fdFind(m *fdModel, id string) *fdChart {
	for i := range m.Charts {
		if m.Charts[i].ID == id {
			return &m.Charts[i]
		}
	}
	return nil
}

// Each of the five reads metrics that were in the file all along. A chart that silently
// produces nothing is the failure mode that matters here — it looks identical to a server
// that had no problem.
func TestFTDCNewChartsDrawFromARealCapture(t *testing.T) {
	m := ftdcLoadRS(t)
	for _, want := range []string{"oplogWindow", "waiting", "admission", "tcp", "processPressure"} {
		c := fdFind(m, want)
		if c == nil {
			t.Errorf("%s: no chart built from a capture that has its metrics", want)
			continue
		}
		if len(c.Series) == 0 {
			t.Errorf("%s: chart with no series", want)
		}
		if c.Advice == nil || c.Advice.Headline == "" {
			t.Errorf("%s: no advice — a chart nobody can read is a chart nobody reads", want)
		}
	}
}

// The window is a duration in minutes, not a size, and it is derived rather than stored:
// two BSON timestamps subtracted.
func TestFTDCOplogWindowIsATime(t *testing.T) {
	m := ftdcLoadRS(t)
	c := fdFind(m, "oplogWindow")
	if c == nil {
		t.Fatal("no oplogWindow chart")
	}
	if c.Unit != "minutes" {
		t.Errorf("unit %q, want minutes", c.Unit)
	}
	peak := fdMax(c.Series[0].Points)
	if peak < 5 || peak > 24*60 {
		t.Errorf("window peak %.1f minutes is not a plausible oplog window", peak)
	}
	// This capture's oplog had never wrapped, so the honest reading is "still filling"
	// rather than "fell to zero" — the warm-up ramp must not be reported as a collapse.
	if c.Advice.Level != "info" || !strings.Contains(c.Advice.Headline, "not wrapped") {
		t.Errorf("advice on a still-filling oplog = %s/%q", c.Advice.Level, c.Advice.Headline)
	}
}

// A window that grows and then shrinks IS a finding, and the shrink is measured after the
// ramp rather than from the empty start.
func TestFTDCOplogWindowReportsARealCollapse(t *testing.T) {
	// 40 samples: the window grows to 120 minutes, then collapses to 8.
	const n = 40
	base := int64(1787000000)
	early := make([]int64, n)
	late := make([]int64, n)
	ts := make([]float64, n)
	for i := 0; i < n; i++ {
		ts[i] = float64(base + int64(i))
		late[i] = base + int64(i)
		switch {
		case i < 20:
			early[i] = late[i] - int64(i*360) // window grows to 2 hours
		default:
			early[i] = late[i] - 480 // and then holds at 8 minutes
		}
	}
	d := &ftdcData{TS: ts, Series: map[string]*ftdcSeries{
		"serverStatus.oplog.earliestOptime": {Key: "serverStatus.oplog.earliestOptime", Values: early},
		"serverStatus.oplog.latestOptime":   {Key: "serverStatus.oplog.latestOptime", Values: late},
	}, Samples: n}
	c := fdChartOplogWindow(d)
	if c == nil {
		t.Fatal("no chart")
	}
	if c.Advice.Level != "crit" {
		t.Errorf("a window that fell to 8 minutes reads as %s: %s", c.Advice.Level, c.Advice.Headline)
	}
	if !strings.Contains(c.Advice.Headline, "8 minutes") {
		t.Errorf("headline does not name the narrowest window: %q", c.Advice.Headline)
	}
}

// The waiting chart is the GAP between the two totals, not a second drawing of latency —
// the latency chart already draws that, and two charts of the same line is how a page
// teaches people to skip both.
func TestFTDCWaitingChartsTheGap(t *testing.T) {
	const n = 10
	ts := make([]float64, n)
	lat := make([]int64, n)
	work := make([]int64, n)
	ops := make([]int64, n)
	for i := 0; i < n; i++ {
		ts[i] = float64(1787000000 + i)
		ops[i] = int64(i * 10)         // 10 ops per sample
		lat[i] = int64(i * 10 * 4000)  // 4 ms each
		work[i] = int64(i * 10 * 1000) // 1 ms of it working
	}
	series := func(k string, v []int64) *ftdcSeries { return &ftdcSeries{Key: k, Values: v} }
	d := &ftdcData{TS: ts, Samples: n, Series: map[string]*ftdcSeries{
		"serverStatus.opLatencies.reads.latency":   series("", lat),
		"serverStatus.opLatencies.reads.ops":       series("", ops),
		"serverStatus.opWorkingTime.reads.latency": series("", work),
	}}
	c := fdChartWaiting(d)
	if c == nil {
		t.Fatal("no chart")
	}
	if len(c.Series) != 1 || c.Series[0].Name != "reads" {
		t.Fatalf("want one series named reads, got %+v", c.Series)
	}
	// 4 ms total, 1 ms working → the line is the 3 ms of waiting.
	if got := fdMax(c.Series[0].Points); got < 2.9 || got > 3.1 {
		t.Errorf("waiting = %.2f ms, want 3", got)
	}
	if c.Advice.Level != "crit" {
		t.Errorf("75%% waiting reads as %s: %s", c.Advice.Level, c.Advice.Headline)
	}
}

// The capture header is the type-0 document read back. Everything in it is a fact about the
// server, and two of them come with the sentence that makes them matter.
func TestFTDCCaptureHeader(t *testing.T) {
	m := ftdcLoadRS(t)
	if len(m.Server) < 12 {
		t.Fatalf("only %d facts from a full metadata document", len(m.Server))
	}
	byLabel := map[string]fdFact{}
	for _, f := range m.Server {
		byLabel[f.Label] = f
	}
	for _, want := range []string{"Server", "Host", "OS", "Cores", "Memory", "Replica set", "dbPath", "Log"} {
		if byLabel[want].Value == "" {
			t.Errorf("%s missing from the capture header", want)
		}
	}
	if !strings.Contains(byLabel["Server"].Value, "8.0.28") {
		t.Errorf("version %q", byLabel["Server"].Value)
	}
	if byLabel["Replica set"].Value != "psmrs" {
		t.Errorf("replica set %q", byLabel["Replica set"].Value)
	}
	// The trap the header exists to surface: nothing pinned the cache, and the memory
	// mongod sized it from is the machine's, not the container's.
	if !strings.Contains(byLabel["Memory"].Note, "container") {
		t.Errorf("no note about where the cache size came from: %q", byLabel["Memory"].Note)
	}
}

// A file-descriptor ceiling below MongoDB's own recommendation is worth a sentence, and the
// standalone fixture (1024) is exactly that case.
func TestFTDCCaptureHeaderFlagsALowFDLimit(t *testing.T) {
	ents, err := os.ReadDir(ftdcTestDir)
	if err != nil {
		t.Skip("no standalone fixture")
	}
	var raw [][]byte
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "metrics.") {
			b, _ := os.ReadFile(ftdcTestDir + "/" + e.Name())
			raw = append(raw, b)
		}
	}
	d, err := ftdcParse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ftdcSummarise(d).Server {
		if f.Label == "File descriptors" {
			if f.Value != "1024" {
				t.Fatalf("fd limit read as %q", f.Value)
			}
			if !strings.Contains(f.Note, "64000") {
				t.Errorf("a 1024 fd ceiling passed without comment: %q", f.Note)
			}
			return
		}
	}
	t.Error("no file-descriptor fact in the capture header")
}

// ---------------------------------------------------------------- the second nine

// Each of these reads metrics the fixture actually contains. A chart that silently draws
// nothing is indistinguishable from a server with nothing to report, which is the failure
// mode worth a test.
func TestFTDCSecondPassChartsDraw(t *testing.T) {
	m := ftdcLoadRS(t)
	for _, want := range []string{
		"indexBuild", "oplogCache", "dataHandles", "heap",
		"flowControl", "readPreference", "sessionStore", "diskSpace",
	} {
		c := fdFind(m, want)
		if c == nil {
			t.Errorf("%s: no chart from a capture that carries its metrics", want)
			continue
		}
		if len(c.Series) == 0 || c.Advice == nil || c.Advice.Headline == "" {
			t.Errorf("%s: %d series, advice %v", want, len(c.Series), c.Advice)
		}
	}
}

// The oplog's share of the cache has to be taken per sample, because the cache size can
// change while the server runs — this capture's own does, 14.19 GiB → 768 MiB.
func TestFTDCOplogCacheShareFollowsAResize(t *testing.T) {
	ts := make([]float64, 4)
	for i := range ts {
		ts[i] = float64(1787000000 + i)
	}
	d := &ftdcData{TS: ts, Samples: 4, Series: map[string]*ftdcSeries{
		// 200 MiB of oplog in a cache that starts at 8 GiB and is cut to 256 MiB.
		"local.oplog.rs.stats.storageStats.wiredTiger.cache.bytes currently in the cache": {
			Values: []int64{200 << 20, 200 << 20, 200 << 20, 200 << 20}},
		"serverStatus.wiredTiger.cache.maximum bytes configured": {
			Values: []int64{8 << 30, 8 << 30, 256 << 20, 256 << 20}},
		"serverStatus.wiredTiger.cache.bytes currently in the cache": {
			Values: []int64{220 << 20, 220 << 20, 220 << 20, 220 << 20}},
	}}
	c := fdChartOplogCache(d)
	if c == nil {
		t.Fatal("no chart")
	}
	// Peak against peak would read 200 MiB of 8 GiB = 2%. Per sample it is 78%.
	if c.Advice.Level != "warn" {
		t.Errorf("advice level %s: %s", c.Advice.Level, c.Advice.Headline)
	}
	if !strings.Contains(c.Advice.Headline, "78%") {
		t.Errorf("share not computed per sample: %q", c.Advice.Headline)
	}
}

// The same filesystem appears several times in a container (/, and the /etc files bind
// mounted onto it). Four copies of one line, and an outage named after /etc/hosts, is worse
// than not drawing it.
func TestFTDCDiskSpaceDeduplicatesBindMounts(t *testing.T) {
	n := 20
	ts := make([]float64, n)
	free := make([]int64, n)
	for i := 0; i < n; i++ {
		ts[i] = float64(1787000000 + i*60)
		free[i] = int64(100<<30) - int64(i)*int64(1<<30) // 100 GiB falling by 1 GiB a minute
	}
	series := map[string]*ftdcSeries{}
	for _, name := range []string{"/", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
		series["systemMetrics.mounts."+name+".free"] = &ftdcSeries{Values: append([]int64{}, free...)}
	}
	d := &ftdcData{TS: ts, Samples: n, Series: series}
	c := fdChartDiskSpace(d)
	if c == nil {
		t.Fatal("no chart")
	}
	if len(c.Series) != 1 {
		t.Errorf("%d series from four views of one filesystem: %v", len(c.Series), c.Series)
	}
	if c.Series[0].Name != "/" {
		t.Errorf("kept %q rather than the filesystem itself", c.Series[0].Name)
	}
	if c.Advice.Level != "crit" || !strings.Contains(c.Advice.Headline, "/") {
		t.Errorf("a filesystem losing 1 GiB a minute reads as %s: %s", c.Advice.Level, c.Advice.Headline)
	}
}

// ---------------------------------------------------------------- zoom

// A zoom is a second read of the source narrowed to a window, so the window has to slice
// every series by the same indices as the timestamp column.
func TestFTDCWindowSlicesEverySeriesTogether(t *testing.T) {
	n := 100
	ts := make([]float64, n)
	vals := make([]int64, n)
	for i := 0; i < n; i++ {
		ts[i] = float64(1787000000 + i)
		vals[i] = int64(i)
	}
	d := &ftdcData{TS: ts, Samples: n, Series: map[string]*ftdcSeries{
		"a": {Key: "a", Values: vals}, "b": {Key: "b", Values: vals},
	}}
	w := ftdcWindow(d, 1787000010, 1787000019)
	if len(w.TS) != 10 || w.Samples != 10 {
		t.Fatalf("window has %d samples", len(w.TS))
	}
	for k, s := range w.Series {
		if len(s.Values) != 10 {
			t.Errorf("%s: %d values against %d timestamps", k, len(s.Values), len(w.TS))
		}
		if s.Values[0] != 10 || s.Values[9] != 19 {
			t.Errorf("%s: sliced to %d..%d, want 10..19", k, s.Values[0], s.Values[9])
		}
	}
	// A window outside the capture is empty rather than wrong.
	if e := ftdcWindow(d, 1788000000, 1788000100); len(e.TS) != 0 {
		t.Errorf("a window past the end returned %d samples", len(e.TS))
	}
}

// The model says when it is drawing fewer points than it read, because an advisor that
// names a peak the chart cannot show is otherwise indistinguishable from a broken chart.
func TestFTDCSaysWhenChartsAreThinned(t *testing.T) {
	m := ftdcLoadRS(t)
	thinned := false
	for _, n := range m.Notes {
		if strings.Contains(n, "Charts draw") {
			thinned = true
		}
	}
	if m.Samples > fdMaxPoints && !thinned {
		t.Errorf("%d samples drawn as %d points with no note about it", m.Samples, fdMaxPoints)
	}
	if m.Samples <= fdMaxPoints && thinned {
		t.Error("a capture that fits reported itself as thinned")
	}
}

// ---------------------------------------------------------------- the router

// A mongos capture is a different animal: no replSetGetStatus, no WiredTiger and no oplog —
// it stores nothing — and two metric sections no other kind of member has. The fixtures are
// the ones the decoder tests already use (metrics.mongos60/70/80), so the router charts are
// exercised on all three server versions rather than on one.

func TestFTDCRouterOwnCharts(t *testing.T) {
	m := ftdcSummarise(ftdcFixtureNamed(t, "metrics.mongos80"))
	if !strings.Contains(strings.ToLower(m.Role), "mongos") {
		t.Errorf("role %q — a router capture was not recognised as one", m.Role)
	}
	for _, want := range []string{"routerLatency", "routerHosts"} {
		if fdFind(m, want) == nil {
			t.Errorf("%s: missing from a router capture", want)
		}
	}
	// And nothing that needs a storage engine or a replica set.
	for _, no := range []string{"cache", "oplog", "oplogWindow", "memberState", "replLag", "eviction"} {
		if fdFind(m, no) != nil {
			t.Errorf("%s: drawn for a router, which has no such thing", no)
		}
	}
}

// The pool keys carry an FQDN, which has dots of its own — splitting the whole path on "."
// named every shard member "net".
func TestFTDCRouterHostsAreNamed(t *testing.T) {
	m := ftdcSummarise(ftdcFixtureNamed(t, "metrics.mongos80"))
	c := fdFind(m, "routerHosts")
	if c == nil {
		t.Skip("no per-host pool data in this fixture")
	}
	for _, s := range c.Series {
		if s.Name == "net" || strings.Contains(s.Name, ".") || strings.Contains(s.Name, ":") {
			t.Errorf("host series named %q — the FQDN was parsed as a path", s.Name)
		}
	}
}

// The latency histogram is the router's own measurement of shard latency, and it is the
// reason to read a router's capture at all. Every version writes it.
func TestFTDCRouterLatencyIsBucketed(t *testing.T) {
	for _, f := range []string{"metrics.mongos60", "metrics.mongos70", "metrics.mongos80"} {
		m := ftdcSummarise(ftdcFixtureNamed(t, f))
		c := fdFind(m, "routerLatency")
		if c == nil {
			t.Errorf("%s: no routerLatency chart", f)
			continue
		}
		if !c.Stack {
			t.Errorf("%s: a histogram drawn as separate lines rather than parts of a whole", f)
		}
		if len(c.Series) == 0 || fdMax(c.Series[0].Points) == 0 {
			t.Errorf("%s: no operations in any bucket", f)
		}
		if c.Series[0].Name != "under 1 ms" {
			t.Errorf("%s: buckets out of order, first is %q", f, c.Series[0].Name)
		}
	}
}
