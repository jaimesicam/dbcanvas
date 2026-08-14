package main

// ftdc_test.go — the FTDC decoder against a real diagnostic.data file.
//
// testdata/ftdc/metrics.rs-mongo03 is the first 119 KB of a metrics file written by a live
// Percona Server for MongoDB 8.0.28-12 replica-set member, truncated at a BSON document
// boundary so it is a valid, shorter file rather than a corrupt one.
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
	"testing"
)

func ftdcFixture(t *testing.T) *ftdcData {
	t.Helper()
	raw, err := os.ReadFile("testdata/ftdc/metrics.rs-mongo03")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	d, err := ftdcParse([][]byte{raw})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
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
	}
	for _, c := range m.Charts {
		if _, ok := want[c.ID]; ok {
			want[c.ID] = true
		}
		if c.Why == "" {
			t.Errorf("chart %s has no explanation", c.ID)
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
