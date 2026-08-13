package main

import (
	"strings"
	"testing"
)

// Advisors are read as instructions, so a wrong one is worse than none: it sends
// an operator to change a setting that was never the problem. These pin the
// judgment calls and the shapes that must not produce advice at all.

func flat(metrics map[string]float64) *vsSeries {
	s := &vsSeries{}
	for k := range metrics {
		s.Metrics = append(s.Metrics, k)
	}
	for i := 0; i < 5; i++ {
		v := map[string]float64{}
		for k, val := range metrics {
			v[k] = val
		}
		s.Points = append(s.Points, vsPoint{T: int64(i), V: v})
	}
	return s
}

// TestAdvisorsNeverInventData is the invariant that matters most: an advisor
// with no series must stay silent rather than describe a server of zeroes.
func TestAdvisorsNeverInventData(t *testing.T) {
	empty := mdl(nil, map[string]*vsSeries{})
	computeAdvisors(empty)
	if len(empty.Advisors) != 0 {
		t.Errorf("an empty capture produced %d advisors: %v", len(empty.Advisors), empty.Advisors)
	}
	// And every rule individually.
	for name, rule := range advisorRules {
		if v := rule(mdl(nil, map[string]*vsSeries{})); v != nil {
			t.Errorf("advisor %q spoke with no data: %+v", name, v)
		}
	}
}

func TestAdviseBufferPoolReads(t *testing.T) {
	for _, c := range []struct {
		name      string
		ratio     float64
		wantLevel string
	}{
		{"a heavy miss rate is critical", 8.3, vsCrit},
		{"a steady minority is a warning", 2.0, vsWarn},
		{"nearly everything cached is fine", 0.01, vsOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := mdl(nil, map[string]*vsSeries{"bufferPool": flat(map[string]float64{
				"readReqPerSec": 400000, "diskReadPerSec": 34000, "missRatioPct": c.ratio})})
			v := adviseBufferPoolReads(m)
			if v == nil || v.Level != c.wantLevel {
				t.Fatalf("got %+v, want level %q", v, c.wantLevel)
			}
			// The advice must say what the counter actually counts, since the
			// name is what misleads people about it.
			if !strings.Contains(v.Detail, "misses") {
				t.Errorf("advice should explain that this counts misses: %q", v.Detail)
			}
		})
	}
}

// TestAdviseInnodbIO pins the cross-check: when the devices did not serve what
// InnoDB claims to have read, the advice must say so and name the flush method.
func TestAdviseInnodbIO(t *testing.T) {
	m := mdl(map[string]string{"innodb_flush_method": "fsync"},
		map[string]*vsSeries{"innodbIO": flat(map[string]float64{"read": 1842 * (1 << 20), "written": 5 << 20})})
	m.Disk = &vsTabbed{Overall: flat(map[string]float64{"rKBs": 0, "wKBs": 100})}
	v := adviseInnodbIO(m)
	if v == nil || v.Level != vsWarn {
		t.Fatalf("page-cache-served reads should warn, got %+v", v)
	}
	if !strings.Contains(v.Detail, "O_DIRECT") {
		t.Errorf("advice should name the setting responsible: %q", v.Detail)
	}

	m = mdl(map[string]string{"innodb_flush_method": "O_DIRECT"},
		map[string]*vsSeries{"innodbIO": flat(map[string]float64{"read": 598 * (1 << 20)})})
	m.Disk = &vsTabbed{Overall: flat(map[string]float64{"rKBs": 596 * 1024})}
	if v := adviseInnodbIO(m); v == nil || v.Level != vsOK {
		t.Fatalf("device-backed reads should be ok, got %+v", v)
	}
}

// TestAdviseCheckpointAge pins the thing the old byte-count tile could not do:
// the same age means opposite things under different redo capacities.
func TestAdviseCheckpointAge(t *testing.T) {
	age := flat(map[string]float64{"age": 90 << 20})
	crit := mdl(map[string]string{"innodb_redo_log_capacity": "104857600"},
		map[string]*vsSeries{"checkpointAge": age})
	if v := adviseCheckpointAge(crit); v == nil || v.Level != vsCrit {
		t.Fatalf("90 MB of a 100 MiB log should be critical, got %+v", v)
	}
	ok := mdl(map[string]string{"innodb_redo_log_capacity": "10737418240"},
		map[string]*vsSeries{"checkpointAge": age})
	if v := adviseCheckpointAge(ok); v == nil || v.Level != vsOK {
		t.Fatalf("the same 90 MB of a 10 GiB log should be fine, got %+v", v)
	}
	// No capacity in the capture: say it cannot be judged rather than guess.
	none := mdl(nil, map[string]*vsSeries{"checkpointAge": age})
	v := adviseCheckpointAge(none)
	if v == nil || v.Level != vsInfo || !strings.Contains(v.Detail, "cannot be judged") {
		t.Fatalf("without a capacity the advice must decline to judge, got %+v", v)
	}
}

// TestAdviseSwap pins the one chart where any non-zero reading is bad news, and
// the distinction between "swapped once" and "swapping now".
func TestAdviseSwap(t *testing.T) {
	active := mdl(nil, map[string]*vsSeries{"swap": flat(map[string]float64{"used": 200, "in": 4, "out": 8})})
	if v := adviseSwap(active); v == nil || v.Level != vsCrit {
		t.Fatalf("active swapping under a database is critical, got %+v", v)
	}
	stale := mdl(nil, map[string]*vsSeries{"swap": flat(map[string]float64{"used": 200, "in": 0, "out": 0})})
	if v := adviseSwap(stale); v == nil || v.Level != vsWarn {
		t.Fatalf("swapped-out-and-idle is a warning, got %+v", v)
	}
	clean := mdl(nil, map[string]*vsSeries{"swap": flat(map[string]float64{"used": 0, "in": 0, "out": 0})})
	if v := adviseSwap(clean); v == nil || v.Level != vsOK {
		t.Fatalf("no swap is ok, got %+v", v)
	}
}

// TestAdviseCPUStealBeatsEverything: steal time is not fixable inside the
// database, so it must be reported ahead of the conclusions that are.
func TestAdviseCPUStealBeatsEverything(t *testing.T) {
	m := mdl(nil, map[string]*vsSeries{})
	m.CPU = &vsTabbed{Overall: flat(map[string]float64{
		"usr": 30, "sys": 10, "iowait": 25, "steal": 12, "idle": 23})}
	v := adviseCPU(m)
	if v == nil || v.Level != vsWarn || !strings.Contains(v.Detail, "hypervisor") {
		t.Fatalf("steal time should be called out as not-your-database, got %+v", v)
	}
}

func TestAdviseFsyncsRespectsDurability(t *testing.T) {
	// Relaxed durability on a quiet server is still worth flagging, because the
	// risk is real even when the saving is not.
	m := mdl(map[string]string{"sync_binlog": "0", "innodb_flush_log_at_trx_commit": "2"},
		map[string]*vsSeries{"fsyncs": flat(map[string]float64{"data": 10, "log": 2})})
	v := adviseFsyncs(m)
	if v == nil || v.Level != vsWarn {
		t.Fatalf("relaxed durability should warn, got %+v", v)
	}
	// A high fsync rate with safe settings is a trade-off to explain, not a fault.
	m = mdl(map[string]string{"sync_binlog": "1", "innodb_flush_log_at_trx_commit": "1"},
		map[string]*vsSeries{"fsyncs": flat(map[string]float64{"data": 400, "log": 200})})
	v = adviseFsyncs(m)
	if v == nil || v.Level != vsInfo {
		t.Fatalf("a costly but safe configuration is info, got %+v", v)
	}
	if !strings.Contains(v.Detail, "cannot lose") {
		t.Errorf("the advice must not recommend weakening durability unconditionally: %q", v.Detail)
	}
}

// TestComputeAdvisorsKeyed checks the wiring the page depends on: every advisor
// produced is keyed by the chart it explains, and carries its own id.
func TestComputeAdvisorsKeyed(t *testing.T) {
	m := mdl(map[string]string{"innodb_redo_log_capacity": "104857600"}, map[string]*vsSeries{
		"bufferPool":    flat(map[string]float64{"readReqPerSec": 1000, "diskReadPerSec": 1, "missRatioPct": 0.1, "freePages": 10, "totalPages": 8192, "dataPages": 8000, "dirtyPages": 100}),
		"checkpointAge": flat(map[string]float64{"age": 1 << 20}),
		"qps":           flat(map[string]float64{"questions": 1500, "select": 900, "insert": 300, "update": 200, "delete": 100}),
	})
	computeAdvisors(m)
	for _, key := range []string{"bufferPoolPages", "bufferPoolReads", "checkpointAge", "qps"} {
		a, ok := m.Advisors[key]
		if !ok {
			t.Errorf("no advisor for %q", key)
			continue
		}
		if a.ID != key {
			t.Errorf("advisor %q carries id %q", key, a.ID)
		}
		if a.Headline == "" || a.Detail == "" {
			t.Errorf("advisor %q is missing text: %+v", key, a)
		}
	}
	if _, ok := m.Advisors["swap"]; ok {
		t.Error("an advisor appeared for a series that is not in this capture")
	}
}

// TestAdviseGaleraBurstyFlowControl pins the two bugs a live PXC capture
// exposed, both of which made this advisor report health during a real stall.
//
// Flow control arrives in bursts: a node paused a third of a capture is paused
// ~100% of a few seconds and 0% of the rest. Taking the median of that reads
// zero, so the advisor said "keeping up with cluster writes" for a node that was
// paused 32.5% of the time and had sent 613 flow-control messages. The mean is
// the statistic that describes what the cluster actually experienced.
func TestAdviseGaleraBurstyFlowControl(t *testing.T) {
	// 30 samples: 10 fully paused, 20 clear — a third of the window.
	s := &vsSeries{Metrics: []string{"flowControlPausedPct", "recvQueue"}}
	for i := 0; i < 30; i++ {
		p := 0.0
		if i%3 == 0 {
			p = 100
		}
		s.Points = append(s.Points, vsPoint{T: int64(i), V: map[string]float64{
			"flowControlPausedPct": p, "recvQueue": 0,
		}})
	}
	m := &vsModel{Series: map[string]*vsSeries{"galera": s}}
	v := adviseGalera(m)
	if v == nil {
		t.Fatal("no verdict")
	}
	if v.Level != vsCrit {
		t.Errorf("level = %q, want crit: a node paused a third of the capture is holding "+
			"the cluster back, and the median of a bursty signal hides exactly that", v.Level)
	}
	if got := seriesMedian(s, "flowControlPausedPct"); got != 0 {
		t.Errorf("precondition: median should be 0 for this shape, got %v", got)
	}
	if got := seriesMean(s, "flowControlPausedPct"); got < 30 || got > 37 {
		t.Errorf("mean = %v, want ~33 — the share of the capture spent paused", got)
	}
}

// A genuinely healthy node must still read ok, or the fix above would just move
// the false negative to a false positive.
func TestAdviseGaleraQuietStaysOK(t *testing.T) {
	s := &vsSeries{Metrics: []string{"flowControlPausedPct", "recvQueue"}}
	for i := 0; i < 30; i++ {
		s.Points = append(s.Points, vsPoint{T: int64(i), V: map[string]float64{
			"flowControlPausedPct": 0, "recvQueue": 1,
		}})
	}
	v := adviseGalera(&vsModel{Series: map[string]*vsSeries{"galera": s}})
	if v == nil || v.Level != vsOK {
		t.Fatalf("quiet cluster: got %+v, want ok", v)
	}
}

func TestSeriesMean(t *testing.T) {
	s := &vsSeries{Points: []vsPoint{
		{V: map[string]float64{"x": 0}}, {V: map[string]float64{"x": 0}},
		{V: map[string]float64{"x": 90}}, {V: map[string]float64{"x": 10}},
	}}
	if got := seriesMean(s, "x"); got != 25 {
		t.Errorf("seriesMean = %v, want 25", got)
	}
	if got := seriesMean(s, "absent"); got != 0 {
		t.Errorf("missing key should be 0, got %v", got)
	}
}

// The lock-waits parser runs against the exact shape pt-stalk writes — captured
// from a real blocked UPDATE on a PXC node, not invented.
const lockWaitsFixture = `TS 1786597608.002923390 2026-08-13 05:06:48
*************************** 1. row ***************************
   who_blocks: thread 8214 from localhost
  idle_in_trx: 0
max_wait_time: 14
  num_waiters: 1
*************************** 1. row ***************************
    waiting_trx_id: 13213
    waiting_thread: 8215
         wait_time: 14
     waiting_query: UPDATE lab.t SET n=n+9 WHERE id=5
waiting_table_lock: ` + "`lab`.`t`" + `
   blocking_trx_id: 13212
   blocking_thread: 8214
       idle_in_trx: 0
    blocking_query: SELECT SLEEP(300)
TS 1786597609.004769416 2026-08-13 05:06:49
*************************** 1. row ***************************
   who_blocks: thread 8214 from localhost
  idle_in_trx: 90
max_wait_time: 43
  num_waiters: 3
`

func TestParseLockWaits(t *testing.T) {
	m := &vsModel{Available: map[string]bool{}}
	parseLockWaits(m, []namedFile{{base: "x-lock-waits", data: []byte(lockWaitsFixture)}})
	if !m.Available["lockWaits"] || len(m.LockWaits) != 1 {
		t.Fatalf("got %d rows, want 1 consolidated per blocking thread", len(m.LockWaits))
	}
	r := m.LockWaits[0]
	// The worst value seen wins: a wait that grows to 43s is a 43s wait.
	for k, want := range map[string]string{
		"blockingThread": "8214", "blockingTrx": "13212",
		"waitSeconds": "43", "idleInTrx": "90", "waiters": "3",
		"table": "lab.t", "blockingQuery": "SELECT SLEEP(300)",
	} {
		if r[k] != want {
			t.Errorf("%s = %q, want %q", k, r[k], want)
		}
	}
}

// A blocker idle inside its transaction is the finding that must not be soft.
func TestAdviseOldestTransactionIdleBlocker(t *testing.T) {
	m := &vsModel{Available: map[string]bool{}}
	parseLockWaits(m, []namedFile{{base: "x-lock-waits", data: []byte(lockWaitsFixture)}})
	v := adviseOldestTransaction(m)
	if v == nil || v.Level != vsCrit {
		t.Fatalf("got %+v, want crit for a session idle 90s inside a transaction", v)
	}
	for _, want := range []string{"8214", "idle in trx"} {
		if !strings.Contains(v.Headline+v.Action, want) {
			t.Errorf("verdict should name %q; got %q / %q", want, v.Headline, v.Action)
		}
	}
}

// With no lock wait at all it falls back to the transaction table, and says so
// rather than pretending it knows nobody was blocked.
func TestAdviseOldestTransactionFallback(t *testing.T) {
	m := &vsModel{
		Available: map[string]bool{},
		InnodbTrx: []map[string]string{
			{"thread": "7139", "active": "600", "rowLocks": "1", "query": "SELECT SLEEP(400)"},
		},
	}
	v := adviseOldestTransaction(m)
	if v == nil || v.Level != vsCrit {
		t.Fatalf("got %+v, want crit for a 600s transaction", v)
	}
	if !strings.Contains(v.Headline, "7139") {
		t.Errorf("should name the thread, got %q", v.Headline)
	}
	// Nothing to say when there is neither source.
	if got := adviseOldestTransaction(&vsModel{Available: map[string]bool{}}); got != nil {
		t.Errorf("no data should yield no advisor, got %+v", got)
	}
}

// "not started" sessions hold no transaction and must not reach the table — a
// live capture produced five rows, four of them this.
func TestInnodbTrxSkipsNotStarted(t *testing.T) {
	recs := parseInnodbTrxBlock("LIST OF TRANSACTIONS FOR EACH SESSION:\n" +
		"---TRANSACTION 421, not started\n0 lock struct(s)\n" +
		"---TRANSACTION 13212, ACTIVE 43 sec\nMySQL thread id 8214\n")
	started := 0
	for _, r := range recs {
		if r.status != "not started" {
			started++
		}
	}
	if started != 1 {
		t.Errorf("expected exactly one started transaction among %d records", len(recs))
	}
}
