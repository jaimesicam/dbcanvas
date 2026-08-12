package main

import "testing"

// The verdict rules turn measurements into conclusions, so a wrong rule is worse
// than no rule — it tells an operator to leave a starved server alone, or to
// resize a healthy one. Each case below is a shape seen in a real capture.

func mdl(vars map[string]string, series map[string]*vsSeries) *vsModel {
	m := &vsModel{Series: series, Available: map[string]bool{}, vars: vars}
	m.Summary.Facts = map[string]string{}
	m.Summary.Findings = map[string]float64{}
	if m.vars == nil {
		m.vars = map[string]string{}
	}
	return m
}

func bpSeries(free, total, miss, diskReads float64) *vsSeries {
	s := &vsSeries{Metrics: []string{"freePages", "totalPages", "missRatioPct", "diskReadPerSec"}}
	for i := 0; i < 5; i++ {
		s.Points = append(s.Points, vsPoint{T: int64(i), V: map[string]float64{
			"freePages": free, "totalPages": total, "missRatioPct": miss, "diskReadPerSec": diskReads,
		}})
	}
	return s
}

func TestVerdictBufferPool(t *testing.T) {
	cases := []struct {
		name                     string
		free, total, miss, reads float64
		wantLevel                string
	}{
		// The bug this rule shipped with once, and the reason for the 10%
		// threshold: a 128 MiB pool measured 342 free pages of 8192 — 4.2%,
		// because InnoDB's page cleaner always keeps a small free list — while
		// missing 8.3% of its reads at 150k/s. An earlier 1% threshold called
		// that "never filled, not the constraint" about a server that ran 3x
		// faster the moment the pool was raised.
		{"busy pool with a small free list is still starved", 342, 8192, 8.3, 117700, vsCrit},
		{"genuinely roomy pool that misses nothing", 105660, 262144, 0, 0, vsOK},
		{"full pool that misses nothing is correctly sized", 30, 8192, 0.01, 2, vsOK},
		{"moderate sustained miss is a warning", 40, 8192, 2.0, 400, vsWarn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mdl(map[string]string{"innodb_buffer_pool_size": "134217728"},
				map[string]*vsSeries{"bufferPool": bpSeries(c.free, c.total, c.miss, c.reads)})
			v := verdictBufferPool(m)
			if v == nil {
				t.Fatal("expected a verdict")
			}
			if v.Level != c.wantLevel {
				t.Errorf("level = %q, want %q (headline %q)", v.Level, c.wantLevel, v.Headline)
			}
		})
	}
	if verdictBufferPool(mdl(nil, map[string]*vsSeries{})) != nil {
		t.Error("no buffer pool series should produce no verdict, not a guess")
	}
}

// TestVerdictPageCache pins the check that stopped a reader — this one —
// concluding that storage was saturated when the block devices were idle.
func TestVerdictPageCache(t *testing.T) {
	io := func(bytesPerSec float64) *vsSeries {
		s := &vsSeries{Metrics: []string{"read"}}
		for i := 0; i < 5; i++ {
			s.Points = append(s.Points, vsPoint{T: int64(i), V: map[string]float64{"read": bytesPerSec}})
		}
		return s
	}
	disk := func(readKBs float64) *vsTabbed {
		s := &vsSeries{Metrics: []string{"rKBs"}}
		// The first point is iostat's since-boot average and must be ignored;
		// make it wild so a regression that counts it is obvious.
		s.Points = append(s.Points, vsPoint{T: 0, V: map[string]float64{"rKBs": 9e9}})
		for i := 1; i < 5; i++ {
			s.Points = append(s.Points, vsPoint{T: int64(i), V: map[string]float64{"rKBs": readKBs}})
		}
		return &vsTabbed{Overall: s}
	}

	// Real numbers: 1,842 MiB/s reported by InnoDB, nothing at all from the
	// devices, under innodb_flush_method=fsync.
	m := mdl(map[string]string{"innodb_flush_method": "fsync"},
		map[string]*vsSeries{"innodbIO": io(1842 * (1 << 20))})
	m.Disk = disk(0)
	v := verdictPageCache(m)
	if v == nil || v.Level != vsWarn {
		t.Fatalf("page-cache-served reads should warn, got %+v", v)
	}

	// The same server under O_DIRECT: 598.8 MiB/s reported against 596.7 MiB/s
	// of real device traffic.
	m = mdl(map[string]string{"innodb_flush_method": "O_DIRECT"},
		map[string]*vsSeries{"innodbIO": io(598.8 * (1 << 20))})
	m.Disk = disk(596.7 * 1024)
	if v := verdictPageCache(m); v == nil || v.Level != vsOK {
		t.Fatalf("device-backed reads should be ok, got %+v", v)
	}

	// Nothing being read at all: no misses to attribute, so say nothing rather
	// than divide by a number that is not there.
	m = mdl(nil, map[string]*vsSeries{"innodbIO": io(0)})
	m.Disk = disk(0)
	if v := verdictPageCache(m); v != nil {
		t.Errorf("idle server should produce no page-cache verdict, got %+v", v)
	}
}

// TestVerdictRedoHeadroom pins the reason checkpoint age stopped being reported
// in bytes: the same 11 MB is 1% of one server's redo log and 11% of another's,
// and the old fixed 1 GB threshold could never fire on a 100 MiB log however
// full it got.
func TestVerdictRedoHeadroom(t *testing.T) {
	ckpt := func(age float64) *vsSeries {
		return &vsSeries{Metrics: []string{"age"},
			Points: []vsPoint{{T: 0, V: map[string]float64{"age": age}}}}
	}
	for _, c := range []struct {
		name      string
		age       float64
		vars      map[string]string
		wantLevel string
	}{
		{"11 MB of a 1 GiB log is nothing", 11 << 20,
			map[string]string{"innodb_redo_log_capacity": "1073741824"}, vsOK},
		{"90 MB of a 100 MiB log is critical", 90 << 20,
			map[string]string{"innodb_redo_log_capacity": "104857600"}, vsCrit},
		{"60 MB of a 100 MiB log is a warning", 60 << 20,
			map[string]string{"innodb_redo_log_capacity": "104857600"}, vsWarn},
		// Pre-8.0.30 servers express the same thing as size x count.
		{"older servers use log_file_size x files_in_group", 90 << 20,
			map[string]string{"innodb_log_file_size": "52428800", "innodb_log_files_in_group": "2"}, vsCrit},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := mdl(c.vars, map[string]*vsSeries{"checkpointAge": ckpt(c.age)})
			v := verdictRedoHeadroom(m)
			if v == nil {
				t.Fatal("expected a verdict")
			}
			if v.Level != c.wantLevel {
				t.Errorf("level = %q, want %q (%s)", v.Level, c.wantLevel, v.Headline)
			}
		})
	}
	// Without a capacity there is no ratio, and a byte count on its own is what
	// this rule exists to stop reporting.
	m := mdl(nil, map[string]*vsSeries{"checkpointAge": ckpt(11 << 20)})
	if v := verdictRedoHeadroom(m); v != nil {
		t.Errorf("no redo capacity should produce no verdict, got %+v", v)
	}
}

func TestParseVariables(t *testing.T) {
	m := mdl(nil, map[string]*vsSeries{})
	parseVariables(m, []namedFile{{data: []byte(
		"\nSHOW GLOBAL VARIABLES\n\n" +
			"innodb_buffer_pool_size\t134217728\n" +
			"innodb_flush_method\tO_DIRECT\n" +
			"innodb_redo_log_capacity\t104857600\n" +
			"sync_binlog\t1\n" +
			"innodb_flush_log_at_trx_commit\t2\n" +
			"a line with no tab at all\n")}})
	for k, want := range map[string]string{
		"bufferPoolSize": "134217728", "flushMethod": "O_DIRECT",
		"redoLogCapacity": "104857600", "syncBinlog": "1", "flushLogAtTrxCommit": "2",
	} {
		if got := m.Summary.Facts[k]; got != want {
			t.Errorf("fact %s = %q, want %q", k, got, want)
		}
	}
	if v, ok := m.varNum("innodb_buffer_pool_size"); !ok || v != 134217728 {
		t.Errorf("varNum = %v, %v", v, ok)
	}
	if _, ok := m.varNum("not_a_variable"); ok {
		t.Error("a missing variable must report absent, not 0 — they mean different things")
	}
}

func TestSeriesMedianSkipsIostatFirstSample(t *testing.T) {
	// iostat's first report is an average since boot, not an interval, so it is
	// not a measurement of anything that happened during the capture.
	s := &vsSeries{Metrics: []string{"rKBs"}, Points: []vsPoint{
		{T: 0, V: map[string]float64{"rKBs": 1e9}},
		{T: 1, V: map[string]float64{"rKBs": 10}},
		{T: 2, V: map[string]float64{"rKBs": 20}},
		{T: 3, V: map[string]float64{"rKBs": 30}},
	}}
	if got := seriesMedianSkipFirst(s, "rKBs"); got != 20 {
		t.Errorf("seriesMedianSkipFirst = %v, want 20", got)
	}
	if got := seriesMedian(s, "rKBs"); got != 25 {
		t.Errorf("seriesMedian = %v, want 25 (the since-boot point included)", got)
	}
}
