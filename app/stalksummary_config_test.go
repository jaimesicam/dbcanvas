package main

import (
	"strings"
	"testing"
)

// cfgModel builds a capture with the facts and series the configuration rules read.
func cfgModel(engine string, vars map[string]string, series map[string]*vsSeries) *vsModel {
	m := mdl(vars, series)
	m.Source.Engine = engine
	if m.Summary.Facts == nil {
		m.Summary.Facts = map[string]string{}
	}
	m.Summary.Facts["memory"] = "29.4G"
	return m
}

func cfgFind(v []vsConfig, needle string) *vsConfig {
	for i := range v {
		if strings.Contains(v[i].Variable, needle) {
			return &v[i]
		}
	}
	return nil
}

// The default buffer pool is 128 MiB and has been since machines had half a gigabyte of
// memory. On the hardware this advice was built from it was the difference between 19.8 TPS
// and 727.
func TestConfigCatchesTheDefaultBufferPool(t *testing.T) {
	m := cfgModel("mysql", map[string]string{"innodb_buffer_pool_size": "134217728"},
		map[string]*vsSeries{"bufferPool": flat(map[string]float64{"readReqPerSec": 157600, "diskReadPerSec": 7200, "missRatioPct": 4.9})})
	c := cfgFind(vsConfigAdvice(m), "innodb_buffer_pool_size")
	if c == nil || c.Level != vsCrit {
		t.Fatalf("128 MiB on a 29.4 GiB host must be critical, got %+v", c)
	}
	// The suggestion must not repeat mongod's mistake of sizing as though this process
	// were alone on the machine — three servers here shared one host.
	if !strings.Contains(c.Suggest, "divide it if other database processes share the host") {
		t.Errorf("the suggestion assumes the whole machine: %q", c.Suggest)
	}
	if !strings.Contains(c.Why, "4.9") {
		t.Errorf("the evidence must be this capture's own miss rate: %q", c.Why)
	}
	if !strings.Contains(c.Effect, "727 TPS") {
		t.Errorf("the measured effect is missing: %q", c.Effect)
	}
}

// A pool that fits its working set is not a finding, and saying something anyway is how a
// page full of advice becomes a page nobody reads.
func TestConfigLeavesAHealthyPoolAlone(t *testing.T) {
	m := cfgModel("mysql", map[string]string{"innodb_buffer_pool_size": "8589934592"},
		map[string]*vsSeries{"bufferPool": flat(map[string]float64{"readReqPerSec": 457500, "diskReadPerSec": 38, "missRatioPct": 0.0})})
	c := cfgFind(vsConfigAdvice(m), "innodb_buffer_pool_size")
	if c == nil || c.Level != vsOK || !strings.Contains(c.Suggest, "leave it") {
		t.Fatalf("a pool with a 0%% miss rate should be left alone, got %+v", c)
	}
}

// The durability pair is the only recommendation here that takes something away, so the
// cost has to be stated wherever it is offered.
func TestConfigAlwaysPricesTheDurabilityTrade(t *testing.T) {
	m := cfgModel("mysql", map[string]string{"sync_binlog": "1", "innodb_flush_log_at_trx_commit": "1"},
		map[string]*vsSeries{"fsyncs": flat(map[string]float64{"data": 502, "log": 105})})
	c := cfgFind(vsConfigAdvice(m), "sync_binlog")
	if c == nil {
		t.Fatal("607 fsyncs/s at full durability produced no recommendation")
	}
	if c.Risk == "" || !strings.Contains(c.Risk, "lose") {
		t.Errorf("the cost of relaxing durability must be spelled out: %+v", c)
	}
	if !strings.Contains(c.Suggest, "if, and only if") {
		t.Errorf("this must never read as an unconditional recommendation: %q", c.Suggest)
	}
}

// On a Galera cluster the fsync rate is LOW while durability is the bottleneck, because
// the commits are not getting through. Keying the rule on the rate alone missed the single
// most valuable recommendation on the PXC baseline, where it was worth 57x.
func TestConfigCatchesDurabilityOnAPausedCluster(t *testing.T) {
	m := cfgModel("pxc", map[string]string{"sync_binlog": "1", "innodb_flush_log_at_trx_commit": "1", "wsrep_slave_threads": "1"},
		map[string]*vsSeries{
			"fsyncs": flat(map[string]float64{"data": 68, "log": 23}),
			"galera": flat(map[string]float64{"flowControlPausedPct": 99.9, "recvQueue": 0}),
		})
	got := vsConfigAdvice(m)
	c := cfgFind(got, "sync_binlog")
	if c == nil {
		t.Fatal("a cluster paused 99.9% of the time got no durability advice — only 91 fsyncs/s reached the disk, which is the symptom")
	}
	if !strings.Contains(c.Why, "not getting through") {
		t.Errorf("the explanation must say why the rate is low: %q", c.Why)
	}
	// And the applier, which is the other half of the same story.
	w := cfgFind(got, "wsrep_slave_threads")
	if w == nil || w.Level != vsCrit {
		t.Fatalf("one applier with flow control at 99.9%% must be critical, got %+v", w)
	}
	if !strings.Contains(w.Effect, "2,032 TPS") {
		t.Errorf("the measured effect is missing: %q", w.Effect)
	}
}

// A replica that cannot keep up is invisible everywhere else: the source looks fast, the
// replica looks connected, and the lag grows. Ten seconds is already a failover you cannot
// make.
func TestConfigCatchesASingleApplier(t *testing.T) {
	m := cfgModel("mysql", map[string]string{"replica_parallel_workers": "1"},
		map[string]*vsSeries{"replicationLag": flat(map[string]float64{"seconds": 25})})
	c := cfgFind(vsConfigAdvice(m), "replica_parallel_workers")
	if c == nil || c.Level != vsCrit {
		t.Fatalf("a replica 25 s behind with one applier must be critical, got %+v", c)
	}
	if !strings.Contains(c.Suggest, "WRITESET") {
		t.Errorf("more workers without WRITESET dependency tracking does little: %q", c.Suggest)
	}
	if !strings.Contains(c.Risk, "preserve_commit_order") {
		t.Errorf("the ordering guarantee must be named: %q", c.Risk)
	}
	// With workers already raised and no lag, there is nothing to say.
	ok := cfgModel("mysql", map[string]string{"replica_parallel_workers": "8"},
		map[string]*vsSeries{"replicationLag": flat(map[string]float64{"seconds": 0})})
	if c := cfgFind(vsConfigAdvice(ok), "replica_parallel_workers"); c != nil {
		t.Errorf("a replica keeping up should produce no advice: %+v", c)
	}
}

// The one recommendation that did not pay when it was measured. It stays in the file, and
// it says so — a page that only reports the changes that worked is a page selling something.
func TestConfigIsHonestAboutODirect(t *testing.T) {
	m := cfgModel("mysql", map[string]string{"innodb_flush_method": "fsync"},
		map[string]*vsSeries{"innodbIO": flat(map[string]float64{"read": 114 << 20, "written": 9 << 20})})
	m.Disk = &vsTabbed{Tabs: map[string]*vsSeries{"sdc": flat(map[string]float64{"rKBs": 0, "wKBs": 46556})}}
	c := cfgFind(vsConfigAdvice(m), "innodb_flush_method")
	if c == nil {
		t.Fatal("InnoDB reading 114 MiB/s while the devices serve none is the double-buffering signature")
	}
	if !strings.Contains(c.Effect, "did not pay") {
		t.Errorf("the measured result must not be dressed up: %q", c.Effect)
	}
}

// A capture with none of these series must produce nothing rather than a page of advice
// about a server of zeroes.
func TestConfigInventsNothing(t *testing.T) {
	if got := vsConfigAdvice(cfgModel("mysql", nil, map[string]*vsSeries{})); len(got) != 0 {
		t.Errorf("advice from an empty capture: %+v", got)
	}
}
