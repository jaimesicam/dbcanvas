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
