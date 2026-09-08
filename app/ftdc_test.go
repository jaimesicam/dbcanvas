package main

// ftdc_test.go — the FTDC decoder against real diagnostic.data files.
//
// Three fixtures, each the metadata document plus the last few chunks of a capture from a
// live Percona Server for MongoDB replica-set member — trimmed at BSON document boundaries,
// so each is a valid shorter file rather than a corrupt one:
//
//	metrics.rs60-mongo01   6.0.29-23
//	metrics.rs70-mongo01   7.0.39-21
//	metrics.rs-mongo03     8.0.28-12
//
// Three of them because five metric paths this page depends on MOVED between those
// releases, and every such move fails silently: a chart built on a key that is not there is
// not an error, it is an empty chart — or, in the case of the CPU core count, a chart that
// is confidently wrong.
//
// A synthetic fixture would be worse than no fixture here. FTDC's encoding has three places
// where a plausible-looking decoder is silently wrong — the zero run-length pairs, the
// column-major delta order, and BSON timestamps counting as two metrics rather than one —
// and every one of them produces output that looks like numbers. Only a real file
// disagrees with a wrong decoder, and the mechanism that makes it disagree is the metric
// count that mongod writes into each chunk: get the reference-document walk wrong by one
// and the count no longer matches, which is what readChunk refuses on.

import (
	"os"
	"strings"
	"testing"
)

func ftdcFixture(t *testing.T) *ftdcData { return ftdcFixtureNamed(t, "metrics.rs-mongo03") }

// The 6.0 and 7.0 fixtures are the metadata document plus the last two chunks of a real
// capture from a PSMDB 6.0.29-23 and 7.0.39-21 member. They exist because four metric paths
// this page depends on MOVED between those releases, and every one of them fails silently:
// a chart built on a key that is not there is not an error, it is an empty chart, and an
// empty chart on somebody else's capture looks like a server that was doing nothing.
func ftdcFixtureNamed(t *testing.T, name string) *ftdcData {
	t.Helper()
	raw, err := os.ReadFile("testdata/ftdc/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	d, err := ftdcParse([][]byte{raw})
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return d
}

// ftdcVersions is every fixture, oldest first.
var ftdcVersions = []struct{ name, file, major string }{
	{"6.0", "metrics.rs60-mongo01", "6.0."},
	{"7.0", "metrics.rs70-mongo01", "7.0."},
	{"8.0", "metrics.rs-mongo03", "8.0."},
}

func TestFTDCDecodesARealFile(t *testing.T) {
	d := ftdcFixture(t)
	if d.Chunks == 0 {
		t.Fatal("no metric chunks decoded")
	}
	// Skipped is the tell. Every chunk that fails the metric-count check lands here, so a
	// decoder that has drifted out of step with mongod's own walk shows up as skips
	// rather than as quietly wrong numbers.
	if d.Skipped != 0 {
		t.Errorf("%d chunk(s) failed to decode — the reference-document walk no longer matches mongod's", d.Skipped)
	}
	if d.Samples < 10 {
		t.Errorf("only %d samples", d.Samples)
	}
	if len(d.Series) < 1000 {
		t.Errorf("only %d metrics — an 8.0 mongod carries thousands", len(d.Series))
	}
	// Every series must be exactly as long as the timestamp column, or a chart indexing
	// them together reads one metric's value against another metric's sample.
	for k, s := range d.Series {
		if len(s.Values) != len(d.TS) {
			t.Fatalf("series %q has %d values against %d timestamps", k, len(s.Values), len(d.TS))
		}
	}
}

func TestFTDCMetadataAndClock(t *testing.T) {
	d := ftdcFixture(t)
	if d.Meta["host"] == "" {
		t.Error("no host in the metadata document")
	}
	if d.Meta["replSet"] != "rs0" {
		t.Errorf("replSet = %q, want rs0", d.Meta["replSet"])
	}
	if d.Meta["version"] == "" {
		t.Error("no server version")
	}
	// The clock has to be monotonic and plausible: samples are one second apart, so a
	// decoder that read the wrong column for `start` produces timestamps that jump.
	for i := 1; i < len(d.TS); i++ {
		if d.TS[i] < d.TS[i-1] {
			t.Fatalf("timestamps go backwards at sample %d", i)
		}
	}
	if span := d.span().Seconds(); span <= 0 || span > 3600 {
		t.Errorf("span %.0fs is not a plausible truncated capture", span)
	}
}

// The decode has to agree with the server, not merely with itself. These are values whose
// correctness is checkable from outside the file: uptime rises by about one per sample,
// and a counter never goes backwards without the server restarting.
func TestFTDCValuesAreCoherent(t *testing.T) {
	d := ftdcFixture(t)
	up := d.Series["serverStatus.uptime"]
	if up == nil {
		t.Fatal("no serverStatus.uptime")
	}
	for i := 1; i < len(up.Values); i++ {
		if up.Values[i] < up.Values[i-1] {
			t.Fatalf("uptime went backwards at sample %d (%d → %d)", i, up.Values[i-1], up.Values[i])
		}
	}
	// Uptime advances with the clock. Allowing slack for FTDC slowing its own sampling,
	// but a decoder reading the wrong column drifts wildly rather than slightly.
	gotUp := float64(up.Values[len(up.Values)-1] - up.Values[0])
	wantUp := d.TS[len(d.TS)-1] - d.TS[0]
	if diff := gotUp - wantUp; diff > 5 || diff < -5 {
		t.Errorf("uptime advanced %.0fs while the clock advanced %.0fs", gotUp, wantUp)
	}
	// A cumulative counter must not go backwards either.
	for _, k := range []string{"serverStatus.opcounters.query", "systemMetrics.cpu.user_ms"} {
		s := d.Series[k]
		if s == nil {
			continue
		}
		for i := 1; i < len(s.Values); i++ {
			if s.Values[i] < s.Values[i-1] {
				t.Errorf("%s went backwards at sample %d", k, i)
				break
			}
		}
	}
}

// has() must mean "present and ever non-zero", because a replica-set field on a standalone
// is present and always zero, and charting it says nothing except that somebody drew it.
func TestFTDCHasIgnoresAllZeroSeries(t *testing.T) {
	d := &ftdcData{
		TS: []float64{1, 2},
		Series: map[string]*ftdcSeries{
			"zeroes": {Key: "zeroes", Values: []int64{0, 0}},
			"real":   {Key: "real", Values: []int64{0, 7}},
		},
	}
	if d.has("zeroes") {
		t.Error("an all-zero series counts as present")
	}
	if !d.has("real") {
		t.Error("a series with a value does not count as present")
	}
	if d.has("absent") {
		t.Error("a missing series counts as present")
	}
}

func TestFTDCUvarint(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want uint64
		n    int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x01}, 1, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x80, 0x01}, 128, 2},
		{[]byte{0xff, 0xff, 0x03}, 65535, 3},
		{[]byte{0x80}, 0, 0}, // truncated: a short read, never a panic
		{[]byte{}, 0, 0},
	} {
		got, n := ftdcUvarint(tc.in)
		if got != tc.want || n != tc.n {
			t.Errorf("ftdcUvarint(%v) = %d,%d want %d,%d", tc.in, got, n, tc.want, tc.n)
		}
	}
}

// A garbage file must be refused with a message rather than decoded into nonsense.
func TestFTDCRejectsRubbish(t *testing.T) {
	if _, err := ftdcParse([][]byte{[]byte("this is not BSON at all, not even slightly")}); err == nil {
		t.Error("garbage decoded without an error")
	}
	if _, err := ftdcParse([][]byte{{}}); err == nil {
		t.Error("an empty file decoded without an error")
	}
}

// ---------------------------------------------------------------- the summary

func TestFTDCSummaryBuildsRealCharts(t *testing.T) {
	m := ftdcSummarise(ftdcFixture(t))
	if m.Host == "" || m.ReplSet != "rs0" {
		t.Errorf("summary lost the file's identity: host=%q rs=%q", m.Host, m.ReplSet)
	}
	if len(m.Charts) < 6 {
		t.Fatalf("only %d charts built from a full mongod capture", len(m.Charts))
	}
	want := map[string]bool{
		"memberState": false, "replLag": false, "oplog": false,
		"tickets": false, "connections": false, "cache": false, "cpu": false,
		// The second pass added these, and they are the ones worth guarding: each was
		// missing from the first version of the page and each answers a question the
		// original nine could not.
		"latency": false, "oplogApply": false, "pressure": false, "memory": false,
		// The third pass came from enumerating all 5,665 metrics in this very file rather
		// than from picking familiar ones. Each of these answers something the page could
		// not before, and each is a key that has to keep existing: quorum counted from
		// per-member health, the commit point the client actually waits on, journal fsync
		// latency, the eviction split, host memory and the command breakdown.
		"quorum": false, "commitLag": false, "writeConcern": false, "syncSource": false,
		"commandMix": false, "errors": false, "contention": false,
		"cachePressure": false, "eviction": false, "journal": false, "engineIO": false,
		"historyStore": false, "hostMemory": false,
	}
	for _, c := range m.Charts {
		if _, ok := want[c.ID]; ok {
			want[c.ID] = true
		}
		if c.Why == "" {
			t.Errorf("chart %s has no explanation", c.ID)
		}
		if c.Group == "" {
			t.Errorf("chart %s has no group heading", c.ID)
		}
		if len(c.Series) == 0 {
			t.Errorf("chart %s has no series and should not have been emitted", c.ID)
		}
		// Every series must be as long as the timestamp column after downsampling, or the
		// chart draws one metric's values against another's sample times.
		for _, s := range c.Series {
			if len(s.Points) != len(m.TS) {
				t.Errorf("chart %s series %q: %d points against %d timestamps", c.ID, s.Name, len(s.Points), len(m.TS))
			}
		}
	}
	for id, found := range want {
		if !found {
			t.Errorf("chart %s was not built from a capture that has the metrics for it", id)
		}
	}
}

// The 8.0 key move is the one that would silently empty a chart, so it is asserted
// directly: the fixture is 8.0 and the ticket chart must be built from queues.execution.
func TestFTDCTicketsUseTheEightZeroKeys(t *testing.T) {
	d := ftdcFixture(t)
	if d.Series["serverStatus.queues.execution.read.available"] == nil {
		t.Skip("fixture has no 8.0 ticket keys")
	}
	if d.Series["serverStatus.wiredTiger.concurrentTransactions.read.available"] != nil {
		t.Error("fixture unexpectedly has the pre-8.0 keys too — the fallback can no longer be told apart")
	}
	c := fdChartTickets(d)
	if c == nil || len(c.Series) == 0 {
		t.Fatal("no ticket chart on an 8.0 capture — the key move was not handled")
	}
}

func TestFTDCDownsampleKeepsShape(t *testing.T) {
	in := make([]float64, 5000)
	for i := range in {
		in[i] = float64(i)
	}
	out := fdDownsample(in, 100)
	if len(out) != 100 {
		t.Fatalf("want 100 points, got %d", len(out))
	}
	if out[0] != 0 {
		t.Errorf("first point %v, want the first sample", out[0])
	}
	// Ascending input must stay ascending: a downsample that reorders is a chart that lies.
	for i := 1; i < len(out); i++ {
		if out[i] < out[i-1] {
			t.Fatalf("downsample reordered at %d", i)
		}
	}
	if got := fdDownsample(in[:50], 100); len(got) != 50 {
		t.Errorf("a series shorter than the cap should pass through, got %d", len(got))
	}
}

// A rate must never be negative. A counter that goes backwards means the server restarted,
// and drawing that as a large negative spike would invent an event.
func TestFTDCRateIgnoresCounterResets(t *testing.T) {
	d := &ftdcData{
		TS:     []float64{100, 101, 102, 103},
		Series: map[string]*ftdcSeries{"c": {Key: "c", Values: []int64{10, 20, 5, 15}}},
	}
	r := fdRate(d, "c")
	for i, v := range r {
		if v < 0 {
			t.Errorf("negative rate %v at %d", v, i)
		}
	}
	if r[1] != 10 {
		t.Errorf("rate = %v, want 10/s", r[1])
	}
	if r[2] != 0 {
		t.Errorf("the reset should give 0, got %v", r[2])
	}
}

// fdRatio is where the second pass could most easily be wrong: opLatencies stores total
// microseconds and a total count, and neither is a latency. The average is the ratio of
// their DELTAS, and an interval with no operations must carry the previous value rather
// than dropping to zero — a zero would draw a line to the floor and read as "instant".
func TestFTDCRatioIsPerInterval(t *testing.T) {
	d := &ftdcData{
		TS: []float64{100, 101, 102, 103},
		Series: map[string]*ftdcSeries{
			// 1000us over 10 ops, then nothing, then 6000us over 2 ops.
			"lat": {Key: "lat", Values: []int64{0, 1000, 1000, 7000}},
			"ops": {Key: "ops", Values: []int64{0, 10, 10, 12}},
		},
	}
	got := fdRatio(d, "lat", "ops", 0.001) // to milliseconds
	if got[1] != 0.1 {
		t.Errorf("sample 1 = %v ms, want 0.1", got[1])
	}
	if got[2] != 0.1 {
		t.Errorf("an idle interval should carry the last value, got %v", got[2])
	}
	if got[3] != 3 {
		t.Errorf("sample 3 = %v ms, want 3 (6000us over 2 ops)", got[3])
	}
	if fdRatio(d, "lat", "missing", 1) != nil {
		t.Error("a missing denominator should give no series, not a panic")
	}
}

// Latency has to be derived from the pair, never read straight off either counter — a chart
// of cumulative microseconds is a line going up and to the right for ever.
func TestFTDCLatencyChartIsDerived(t *testing.T) {
	d := ftdcFixture(t)
	if d.Series["serverStatus.opLatencies.reads.latency"] == nil {
		t.Skip("fixture has no opLatencies")
	}
	c := fdChartLatency(d)
	if c == nil || len(c.Series) == 0 {
		t.Fatal("no latency chart from a capture that has opLatencies")
	}
	raw := d.Series["serverStatus.opLatencies.reads.latency"]
	for _, s := range c.Series {
		if len(s.Points) == 0 {
			continue
		}
		// The cumulative counter reaches tens of thousands of microseconds; a derived
		// millisecond average must be nowhere near it.
		if s.Points[len(s.Points)-1] == float64(raw.Values[len(raw.Values)-1]) {
			t.Errorf("series %q is the raw cumulative counter, not an average", s.Name)
		}
	}
}

// A block device that did nothing in the window must not get a chart. io_time_ms is
// cumulative, so a disk busy once at boot has a non-zero value for ever — filtering on
// has() rather than on activity produced four charts of flat zero on a host with one
// working disk.
func TestFTDCSkipsIdleDisks(t *testing.T) {
	d := ftdcFixture(t)
	built := fdDiskCharts(d)
	for _, b := range built {
		c := b(d)
		if c == nil {
			continue
		}
		if fdMax(c.Series[0].Points) <= 0 {
			t.Errorf("chart %s was built for a device with no I/O in the window", c.ID)
		}
	}
}

// The quorum chart is counted per sample from health and state rather than read from
// writableVotingMembersCount, and this is why: that counter comes from the replica-set
// CONFIG, so it reads 3 all the way through an outage in which two members are unreachable.
// This fixture contains such an outage, so a regression to the config counter shows up as a
// chart that never dips.
func TestFTDCQuorumIsCountedNotConfigured(t *testing.T) {
	d := ftdcFixture(t)
	c := fdChartQuorum(d)
	if c == nil || len(c.Series) < 2 {
		t.Fatal("no quorum chart from a replica-set capture")
	}
	avail, need := c.Series[0].Points, c.Series[1].Points
	if fdMin(avail) >= fdMax(need) {
		t.Errorf("availability never dropped below the majority (%.0f vs %.0f) — this capture "+
			"contains an outage, so the count is coming from the config rather than from health",
			fdMin(avail), fdMax(need))
	}
	if fdMax(avail) < 2 {
		t.Errorf("the set never had %.0f members available, which cannot be right", fdMax(avail))
	}
	if c.Advice == nil || c.Advice.Level != "crit" {
		t.Errorf("a window in which the majority was lost should be crit, got %+v", c.Advice)
	}
}

// Journal fsync latency is a ratio of two cumulative counters — total microseconds over a
// count of syncs — and reading either one alone gives a number that only ever goes up.
func TestFTDCJournalLatencyIsDerived(t *testing.T) {
	d := ftdcFixture(t)
	c := fdChartJournal(d)
	if c == nil {
		t.Skip("fixture has no WiredTiger log statistics")
	}
	ms := c.Series[0].Points
	if fdMax(ms) <= 0 {
		t.Fatal("journal latency is flat zero")
	}
	// The raw counter is hundreds of thousands of microseconds by the end of the window; a
	// per-sync millisecond average has to be several orders of magnitude below it.
	raw := d.Series["serverStatus.wiredTiger.log.log sync time duration (usecs)"]
	if fdMax(ms) >= float64(raw.Values[len(raw.Values)-1]) {
		t.Error("the journal chart is drawing the cumulative counter, not the average")
	}
	// A sync that takes longer than a minute is a decode error, not a slow disk.
	if fdMax(ms) > 60000 {
		t.Errorf("implausible journal latency %.0f ms", fdMax(ms))
	}
}

// The eviction advisor divides one counter by another, and the tempting way to do it —
// peak against peak — invents a ratio out of two intervals that were never the same one.
// On this near-idle fixture almost all of the little eviction there is happens on
// application threads, so a share-only test would fire crit on a server doing nothing.
func TestFTDCEvictionAdviceNeedsVolumeNotJustShare(t *testing.T) {
	d := ftdcFixture(t)
	c := fdChartEviction(d)
	if c == nil {
		t.Skip("fixture has no WiredTiger cache statistics")
	}
	if c.Advice == nil {
		t.Fatal("eviction chart with no advisor")
	}
	if c.Advice.Level == "crit" {
		t.Errorf("a capture with under a page a second of eviction must not read as crit: %q", c.Advice.Headline)
	}
}

// Commands are charted from a family of keys discovered at runtime, so the guard is that
// the discovery finds real command names and orders them busiest-first.
func TestFTDCCommandMixIsDiscoveredAndOrdered(t *testing.T) {
	d := ftdcFixture(t)
	c := fdChartCommandMix(d)
	if c == nil {
		t.Fatal("no command chart from a capture that has metrics.commands")
	}
	if len(c.Series) > 8 {
		t.Errorf("%d command series — the tail of a command distribution is always long and never interesting", len(c.Series))
	}
	for i := 1; i < len(c.Series); i++ {
		if fdMax(c.Series[i].Points) > fdMax(c.Series[i-1].Points) {
			t.Errorf("commands are not busiest-first: %s peaks above %s", c.Series[i].Name, c.Series[i-1].Name)
		}
	}
	for _, s := range c.Series {
		if strings.Contains(s.Name, ".") || s.Name == "" {
			t.Errorf("series %q is a key fragment rather than a command name", s.Name)
		}
	}
}

// fdSpanOf turns "which samples" into "how long", which is the number an advisor should
// quote. It has to use the real sample spacing, because FTDC slows down under load.
func TestFTDCSpanOfUsesRealSampleSpacing(t *testing.T) {
	d := &ftdcData{TS: []float64{0, 1, 2, 12, 13}, Series: map[string]*ftdcSeries{}}
	all := fdSpanOf(d, func(int) bool { return true })
	if all != 13 {
		t.Errorf("whole window should be 13s, got %v", all)
	}
	// Sample 3 sits ten seconds after sample 2 — counting samples would call this 1s.
	gap := fdSpanOf(d, func(i int) bool { return i == 3 })
	if gap != 10 {
		t.Errorf("a 10s gap between samples should count as 10s, got %v", gap)
	}
	if fdSpanOf(d, func(int) bool { return false }) != 0 {
		t.Error("nothing selected should be no time")
	}
}

// An advisor that reports a real quantity as "0" reads as a broken chart rather than as an
// idle server, which is the difference these two formatters exist to keep.
func TestFTDCSmallNumbersDoNotReadAsZero(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0"}, {0.04, "under 0.1"}, {0.25, "0.25"}, {17.6, "17.6"}, {305, "305"},
	} {
		if got := fdAmt(tc.in); got != tc.want {
			t.Errorf("fdAmt(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := fdPctV(0.007); got != "under 1%" {
		t.Errorf("fdPctV(0.007) = %q", got)
	}
	if got := fdPct(1, 0); got != "0%" {
		t.Errorf("fdPct with no denominator = %q", got)
	}
}

// A hole in the capture has to be declared. Every chart draws a straight line across one,
// and a straight line reads as "nothing changed" when it means "nothing was recorded" —
// which, since mongod writes FTDC only while it is running, is usually the interesting part.
func TestFTDCGapsAreDeclared(t *testing.T) {
	// One-second samples with a ten-minute hole in the middle.
	d := &ftdcData{Series: map[string]*ftdcSeries{}}
	for i := 0; i < 20; i++ {
		d.TS = append(d.TS, float64(i))
	}
	for i := 0; i < 20; i++ {
		d.TS = append(d.TS, float64(620+i))
	}
	notes := fdGapNote(d)
	if len(notes) != 1 {
		t.Fatalf("a 10-minute hole in a one-second capture was not reported: %v", notes)
	}
	if !strings.Contains(notes[0], "1 gap") {
		t.Errorf("gap note does not say how many: %q", notes[0])
	}
	// A capture that samples every 30 seconds by configuration has no gaps at all, and
	// calling each ordinary interval one would be worse than saying nothing.
	slow := &ftdcData{Series: map[string]*ftdcSeries{}}
	for i := 0; i < 40; i++ {
		slow.TS = append(slow.TS, float64(i*30))
	}
	if n := fdGapNote(slow); n != nil {
		t.Errorf("a uniformly slow capture is not a gappy one: %v", n)
	}
}

// A device utilisation above 100% is not a decode error: /proc/diskstats accumulates busy
// time per queue, so multi-queue and virtio devices routinely exceed wall clock — iostat
// shows the same. It must not be printed as a percentage, because it is not one.
func TestFTDCDiskBusyAboveFullIsWorded(t *testing.T) {
	if got := fdBusy(317); got != "saturated" {
		t.Errorf("fdBusy(317) = %q — a figure over 100%% must not be printed as a percentage", got)
	}
	if got := fdBusy(100); got != "saturated" {
		t.Errorf("fdBusy(100) = %q", got)
	}
	if got := fdBusy(34); got != "34% busy" {
		t.Errorf("fdBusy(34) = %q", got)
	}
}

// ---------------------------------------------------------------- across versions

// The decoder itself has to work on all three, which is the easy half: the container format
// has not changed. Zero skipped chunks is the assertion that matters — a chunk that will not
// decode is counted rather than fatal, so a format drift would otherwise pass as "fewer
// samples than expected" instead of failing.
func TestFTDCDecodesSixSevenAndEight(t *testing.T) {
	for _, v := range ftdcVersions {
		t.Run(v.name, func(t *testing.T) {
			d := ftdcFixtureNamed(t, v.file)
			if !strings.HasPrefix(d.Meta["version"], v.major) {
				t.Errorf("fixture %s reports version %q", v.file, d.Meta["version"])
			}
			if d.Skipped != 0 {
				t.Errorf("%d chunk(s) would not decode on %s", d.Skipped, v.name)
			}
			if d.Samples < 50 || len(d.Series) < 1000 {
				t.Errorf("%s decoded only %d samples of %d metrics", v.name, d.Samples, len(d.Series))
			}
			if d.Meta["replSet"] != "rs0" {
				t.Errorf("%s lost the replica-set name: %q", v.name, d.Meta["replSet"])
			}
		})
	}
}

// The four moves, asserted from both directions.
//
// Each row says: on this version the new key is genuinely ABSENT and the old one is present.
// Asserting only that the chart builds would pass even if the fallback were removed and some
// other version's key happened to exist, so the absence is half the test.
func TestFTDCVersionKeyMoves(t *testing.T) {
	for _, tc := range []struct {
		what, older, newer string
		absentBefore       string // the first version that has `newer`
	}{
		{
			what:  "WiredTiger checkpoint statistics (WT-11171, a top-level category in 7.1)",
			older: "serverStatus.wiredTiger.transaction.transaction checkpoint max time (msecs)",
			newer: "serverStatus.wiredTiger.checkpoint.max time (msecs)",
		},
		{
			what:  "collStats sample moved under storageStats in 7.0",
			older: "local.oplog.rs.stats.maxSize",
			newer: "local.oplog.rs.stats.storageStats.maxSize",
		},
		{
			what:  "execution tickets renamed in 8.0",
			older: "serverStatus.wiredTiger.concurrentTransactions.read.available",
			newer: "serverStatus.queues.execution.read.available",
		},
		{
			what:  "step-down kills renamed in 8.0",
			older: "serverStatus.metrics.repl.stateTransition.userOperationsKilled",
			newer: "serverStatus.metrics.repl.stateTransition.totalOperationsKilled",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			sawOlder, sawNewer := false, false
			for _, v := range ftdcVersions {
				d := ftdcFixtureNamed(t, v.file)
				o, n := d.Series[tc.older] != nil, d.Series[tc.newer] != nil
				if o {
					sawOlder = true
				}
				if n {
					sawNewer = true
				}
				if !o && !n {
					t.Errorf("%s has neither name for %s — the metric may have moved again", v.name, tc.what)
				}
			}
			// If a fixture ever carried both names the fallback would be untestable, and if
			// no fixture carried the old one there would be nothing to fall back to.
			if !sawOlder || !sawNewer {
				t.Errorf("both names must appear across the fixtures (older=%v newer=%v)", sawOlder, sawNewer)
			}
		})
	}
}

// PSI is the one that moved the other way: 6.0.8 and 7.0 put it under systemMetrics
// (SERVER-45255) and 8.0 additionally copies it into serverStatus.extra_info, byte for byte
// the same. Reading only the 8.0 location left the chart silently empty on both older
// releases — on a page that calls it the best single answer to "was this the machine".
func TestFTDCPressureIsReadFromThePortableKey(t *testing.T) {
	for _, v := range ftdcVersions {
		t.Run(v.name, func(t *testing.T) {
			d := ftdcFixtureNamed(t, v.file)
			if d.Series["systemMetrics.pressure.cpu.some.totalMicros"] == nil {
				t.Fatal("no systemMetrics PSI in this fixture — that is the portable location")
			}
			c := fdChartPressure(d)
			if c == nil || len(c.Series) == 0 {
				t.Fatal("no pressure chart, so it is being read from the 8.0-only location")
			}
			// Every resource individually, not just "the chart exists" — losing one key is
			// a quiet hole rather than a missing chart. A resource whose counter never
			// moved in the window is correctly left off the chart, so the requirement is
			// that a resource which DID move gets a line.
			for _, res := range []string{"cpu", "io", "memory"} {
				key := "systemMetrics.pressure." + res + ".some.totalMicros"
				if d.Series[key] == nil {
					t.Errorf("%s has no %s — the portable PSI location is not what this reads", v.name, key)
					continue
				}
				if fdMax(fdRate(d, key)) == 0 {
					continue // nothing stalled on this resource; correctly not drawn
				}
				found := false
				for _, se := range c.Series {
					if strings.HasPrefix(se.Name, res+" ") {
						found = true
					}
				}
				if !found {
					t.Errorf("%s pressure moved on %s but has no line", res, v.name)
				}
			}
		})
	}
}

// The core count is the one version difference that produces a WRONG chart rather than a
// missing one. 6.0 calls it num_cpus; reading only the 7.0+ name falls back to a divisor of
// 1, and the CPU chart on a twenty-core host then reported "iowait peaked at 61% of the
// machine" — a warning generated entirely by arithmetic.
func TestFTDCCPUIsDividedByTheRealCoreCount(t *testing.T) {
	for _, v := range ftdcVersions {
		t.Run(v.name, func(t *testing.T) {
			d := ftdcFixtureNamed(t, v.file)
			c := fdChartCPU(d)
			if c == nil {
				t.Fatal("no CPU chart")
			}
			total := 0.0
			for _, s := range c.Series {
				total += fdMax(s.Points)
			}
			// Every series is a percentage of the whole machine, so their peaks cannot
			// sensibly sum past 100 by much. Dividing by 1 instead of 20 puts this in the
			// hundreds.
			if total > 150 {
				t.Errorf("CPU series sum to %.0f%% of the machine — the core count is not being read", total)
			}
			if !strings.Contains(c.Advice.Headline, "core") && !strings.Contains(c.Advice.Headline, "machine") {
				t.Errorf("unexpected CPU advice: %q", c.Advice.Headline)
			}
		})
	}
}

// The charts that must build on every supported version. A chart missing here is a silent
// hole in the page for anybody running that release, which is exactly the failure mode this
// whole file exists to catch.
func TestFTDCChartsBuildOnEveryVersion(t *testing.T) {
	// Deliberately not the full list: oplogApply, replNetwork and writeConcern depend on
	// the member's ROLE rather than on the version — a primary applies no oplog, fetches
	// nothing, and on these captures never served a majority write.
	want := []string{
		"memberState", "quorum", "replLag", "commitLag", "oplog", "syncSource",
		"latency", "ops", "commandMix", "indexEfficiency", "contention", "errors",
		"connections", "network",
		"tickets", "queues", "cache", "cachePressure", "eviction", "journal",
		"engineIO", "checkpoint", "historyStore", "memory",
		"cpu", "pressure", "hostMemory",
	}
	for _, v := range ftdcVersions {
		t.Run(v.name, func(t *testing.T) {
			built := map[string]bool{}
			for _, c := range ftdcSummarise(ftdcFixtureNamed(t, v.file)).Charts {
				built[c.ID] = true
			}
			for _, id := range want {
				if !built[id] {
					t.Errorf("chart %s does not build on %s", id, v.name)
				}
			}
			// The oplog chart survives losing its cap key — it just quietly stops drawing
			// the reference line and its advisor, which is the sort of degradation that
			// passes an existence check. Assert the cap specifically.
			if c := fdChartOplog(ftdcFixtureNamed(t, v.file)); c == nil || len(c.Series) < 2 {
				t.Errorf("oplog chart on %s has no configured-maximum line", v.name)
			}
		})
	}
}

// ---------------------------------------------------------------- sharded clusters
//
// metrics.mongos60 and metrics.mongos70 are captures from the query router of a 13-node
// sharded cluster on 6.0.29-23 and 7.0.39-21. They exist because a mongos is the one
// MongoDB process this page had never been given: it has no storage engine and no replica
// set, so two thirds of the charts are correctly empty on one, and what it has instead —
// the cluster seen from the outside — is not in any mongod's capture at all.

var ftdcRouters = []struct{ name, file string }{
	{"6.0", "metrics.mongos60"},
	{"7.0", "metrics.mongos70"},
	{"8.0", "metrics.mongos80"},
}

// The directory a mongos writes FTDC into is derived from its LOG path, not a dbPath it
// does not have: /var/log/mongo/mongos.log becomes /var/log/mongo/mongos.diagnostic.data.
// Reading only the mongod location found nothing and reported it to the user as "it exists
// only on mongod, not mongos", which is simply untrue.
func TestFTDCLooksWhereAMongosActuallyWrites(t *testing.T) {
	want := "/var/log/mongo/mongos.diagnostic.data"
	found := false
	for _, d := range ftdcDiagDirs {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("%s is not searched — a mongos capture cannot be read without it", want)
	}
	if ftdcDiagDirs[0] != "/var/lib/mongo/diagnostic.data" {
		t.Error("the mongod path should still be tried first: it is the common case")
	}
}

// A router decodes, is recognised as a router, and gets the charts that make sense for one
// — and none of the ones that do not.
func TestFTDCRouterCaptureIsRecognised(t *testing.T) {
	for _, v := range ftdcRouters {
		t.Run(v.name, func(t *testing.T) {
			d := ftdcFixtureNamed(t, v.file)
			if d.Skipped != 0 {
				t.Errorf("%d chunk(s) would not decode from a mongos capture", d.Skipped)
			}
			m := ftdcSummarise(d)
			if m.Role != "mongos router" {
				t.Errorf("role reads %q — the page will not say what kind of capture this is", m.Role)
			}
			built := map[string]bool{}
			for _, c := range m.Charts {
				built[c.ID] = true
			}
			// What a router HAS: the cluster as it sees it, and its own process.
			// catalogCache is deliberately not in this list — it counts routing-table
			// refreshes and stale-config errors, so a cluster whose chunks are not moving
			// correctly produces no chart at all, and requiring one would make this test
			// pass only on a capture taken during a rebalance.
			for _, id := range []string{"targeting", "shardPing", "routerPool", "connections", "commandMix"} {
				if !built[id] {
					t.Errorf("chart %s does not build on a %s mongos capture", id, v.name)
				}
			}
			// What a router does NOT have. A chart appearing here would mean it is being
			// built from something that is not what its title claims.
			for _, id := range []string{"cache", "tickets", "journal", "replLag", "memberState", "oplog", "quorum"} {
				if built[id] {
					t.Errorf("chart %s was built from a mongos, which has no storage engine and no replica set", id)
				}
			}
		})
	}
}

// The one thing a router capture can do that no mongod capture can. Member names are not in
// a mongod's FTDC — strings are not metrics, so its members are "member 0" and "member 1" —
// but a mongos keys its connection-pool statistics by hostname, so a router names every
// member of every shard and says how far away each was.
func TestFTDCRouterNamesEveryShardMember(t *testing.T) {
	for _, v := range ftdcRouters {
		t.Run(v.name, func(t *testing.T) {
			c := fdChartShardPing(ftdcFixtureNamed(t, v.file))
			if c == nil {
				t.Fatal("no shard-ping chart from a router capture")
			}
			// Three shards of three, plus three config servers.
			if len(c.Series) < 12 {
				t.Errorf("only %d members named, want the whole cluster", len(c.Series))
			}
			sets := map[string]bool{}
			for _, s := range c.Series {
				parts := strings.SplitN(s.Name, " / ", 2)
				if len(parts) != 2 || parts[1] == "" {
					t.Errorf("series %q does not name a set and a host", s.Name)
					continue
				}
				sets[parts[0]] = true
			}
			for _, want := range []string{"cfg", "rs0", "rs1", "rs2"} {
				if !sets[want] {
					t.Errorf("replica set %s is missing from the ping chart", want)
				}
			}
		})
	}
}

// Scatter-gather is the sharded twin of "documents examined per returned": work that did
// not have to happen, invisible in a slow-query log because no single operation is slow.
// The advisor must not read "peak 0.0 ops/s to a single shard" when the traffic was all
// against unsharded collections — it names whichever kind actually carried the work.
func TestFTDCTargetingNamesTheBusiestKind(t *testing.T) {
	for _, v := range ftdcRouters {
		t.Run(v.name, func(t *testing.T) {
			c := fdChartTargeting(ftdcFixtureNamed(t, v.file))
			if c == nil {
				t.Fatal("no targeting chart from a router capture")
			}
			if c.Advice == nil || strings.Contains(c.Advice.Headline, "0.0 ops/s") {
				t.Errorf("targeting advice reads as nothing happened: %+v", c.Advice)
			}
		})
	}
}

// The sharding charts must stay off a plain replica-set capture entirely: a member that is
// not in a sharded cluster has none of these metrics, and a chart of flat zeros is worse
// than no chart.
func TestFTDCShardingChartsAreAbsentOnAPlainReplicaSet(t *testing.T) {
	for _, v := range ftdcVersions {
		t.Run(v.name, func(t *testing.T) {
			for _, c := range ftdcSummarise(ftdcFixtureNamed(t, v.file)).Charts {
				if c.Group == "Sharding" {
					t.Errorf("chart %s appeared on a non-sharded capture", c.ID)
				}
			}
		})
	}
}

// MongoDB 8.0 nests a SHARDED deployment's whole capture by role — common.serverStatus,
// router.connPoolStats, shard.replSetGetStatus — where every earlier version has one flat
// tree, and where an 8.0 plain replica-set member still has one.
//
// The failure it caused is the worst kind this page has: the file decoded perfectly and
// reported 1,978 metrics, and produced ZERO charts, on all three kinds of process. Not one
// chart missing — every chart missing, with nothing to suggest the data was not simply
// absent. The wrapper is stripped in the decoder rather than worked around in eighty chart
// keys, so these assert both halves: that a wrapped capture is unwrapped, and that an
// unwrapped one is untouched.
func TestFTDCUnwraps80SharedRoleGroups(t *testing.T) {
	for _, f := range []string{"metrics.mongos80", "metrics.shard80"} {
		t.Run(f, func(t *testing.T) {
			d := ftdcFixtureNamed(t, f)
			for k := range d.Series {
				if _, wrapped := ftdcUnwrapRole(k); wrapped {
					t.Fatalf("metric %q still carries its 8.0 role group", k)
				}
			}
			// And the flat names every chart reads must now be there.
			for _, want := range []string{"serverStatus.connections.current", "systemMetrics.cpu.user_ms"} {
				if d.Series[want] == nil {
					t.Errorf("%s is missing after unwrapping", want)
				}
			}
			// The metadata document is grouped too — without reading through `common` an
			// 8.0 sharded capture arrives with no version and no host at all.
			if d.Meta["version"] == "" || d.Meta["host"] == "" {
				t.Errorf("capture is anonymous: version=%q host=%q", d.Meta["version"], d.Meta["host"])
			}
			if ftdcSummarise(d).Role == "" {
				t.Error("role could not be inferred")
			}
		})
	}
}

// A shard member on 8.0 is still a replica-set member, so the whole replica-set half of the
// page has to survive the unwrapping — this is what proves the strip put the tree back
// exactly rather than merely producing some charts.
func TestFTDC80ShardMemberKeepsItsReplicaSetCharts(t *testing.T) {
	built := map[string]bool{}
	for _, c := range ftdcSummarise(ftdcFixtureNamed(t, "metrics.shard80")).Charts {
		built[c.ID] = true
	}
	for _, id := range []string{
		"memberState", "replLag", "oplog", "quorum", "tickets", "cache", "journal",
		"cpu", "pressure", "commitLag",
	} {
		if !built[id] {
			t.Errorf("chart %s is missing from an 8.0 shard member", id)
		}
	}
	// And it gains the sharded ones.
	for _, id := range []string{"catalogCache", "criticalSection"} {
		if !built[id] {
			t.Errorf("sharding chart %s is missing from an 8.0 shard member", id)
		}
	}
}

// The unwrap must be a no-op on every capture that is not wrapped, which is all of them
// before 8.0 and an 8.0 plain replica-set member too.
func TestFTDCUnwrapIsANoOpOnFlatCaptures(t *testing.T) {
	for _, f := range []string{"metrics.rs60-mongo01", "metrics.rs70-mongo01", "metrics.rs-mongo03", "metrics.mongos60"} {
		t.Run(f, func(t *testing.T) {
			d := ftdcFixtureNamed(t, f)
			for _, k := range []string{"serverStatus.connections.current", "systemMetrics.cpu.user_ms"} {
				if d.Series[k] == nil {
					t.Errorf("%s went missing — the unwrap is rewriting a flat capture", k)
				}
			}
		})
	}
}
