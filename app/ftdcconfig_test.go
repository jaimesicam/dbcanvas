package main

import (
	"strings"
	"testing"
)

// fdCfgData builds a capture with the metrics the configuration rules read. The numbers are
// the shape of the measured runs: a 29.4 GiB host, a member that either derived its cache
// or had one pinned, and a device that is either saturated or not.
func fdCfgData(cacheGB float64, availMB, swapPages, evictApp, evictAll, ioTimeMs float64) *ftdcData {
	const n = 60
	ts := make([]float64, n)
	for i := range ts {
		ts[i] = 1787000000 + float64(i)
	}
	series := map[string]*ftdcSeries{}
	put := func(key string, f func(i int) int64) {
		v := make([]int64, n)
		for i := range v {
			v[i] = f(i)
		}
		series[key] = &ftdcSeries{Values: v}
	}
	put("serverStatus.wiredTiger.cache.maximum bytes configured", func(int) int64 { return int64(cacheGB * (1 << 30)) })
	put("serverStatus.wiredTiger.cache.bytes currently in the cache", func(int) int64 { return int64(cacheGB * (1 << 30) * 0.95) })
	put("systemMetrics.memory.MemAvailable_kb", func(int) int64 { return int64(availMB * 1024) })
	put("systemMetrics.memory.SwapTotal_kb", func(int) int64 { return 16 << 20 })
	put("systemMetrics.memory.SwapFree_kb", func(int) int64 { return 8 << 20 })
	put("systemMetrics.vmstat.pswpin", func(i int) int64 { return int64(float64(i) * swapPages) })
	put("serverStatus.wiredTiger.cache.application threads page write from cache to disk count", func(i int) int64 { return int64(float64(i) * evictApp) })
	put("serverStatus.wiredTiger.cache.pages written from cache", func(i int) int64 { return int64(float64(i) * evictAll) })
	put("systemMetrics.disks.sdd.io_time_ms", func(i int) int64 { return int64(float64(i) * ioTimeMs) })
	put("systemMetrics.cpu.idle_ms", func(i int) int64 { return int64(i * 12000) }) // 60% idle of 20 cores
	return &ftdcData{TS: ts, Samples: n, Series: series, Meta: map[string]string{
		"memSizeMB": "30105", "cores": "20", "coresAvailable": "20", "thp": "never",
	}}
}

func fdCfgFor(d *ftdcData, role string) []fdConfig {
	return fdConfigAdvice(d, &fdModel{Role: role})
}

func fdCfgFind(v []fdConfig, needle string) *fdConfig {
	for i := range v {
		if strings.Contains(v[i].Setting, needle) {
			return &v[i]
		}
	}
	return nil
}

// The headline rule, and the one measured: nothing pinned the cache, so mongod took half
// the machine — and the machine was shared. This is the difference between 111 TPS and 637.
func TestConfigAdviceCatchesTheDerivedCache(t *testing.T) {
	// 14.19 GiB is exactly what a 29.4 GiB host derives, with the host out of memory.
	d := fdCfgData(14.19, 473, 71808, 400, 1000, 900)
	c := fdCfgFind(fdCfgFor(d, "replica-set member"), "cacheSizeGB")
	if c == nil {
		t.Fatal("no cache advice for a derived cache on a swapping host")
	}
	if c.Level != "crit" {
		t.Errorf("level %q, want crit", c.Level)
	}
	if !strings.Contains(c.Current, "unset") {
		t.Errorf("the current value should say it was never set: %q", c.Current)
	}
	// The suggestion has to be a HOST budget, not a per-process number: the whole failure
	// is each member sizing itself as though alone.
	if !strings.Contains(c.Suggest, "EVERY mongod") {
		t.Errorf("the suggestion must be a host-wide total: %q", c.Suggest)
	}
	if !strings.Contains(c.Why, "473") {
		t.Errorf("the evidence must be this capture's own numbers: %q", c.Why)
	}
}

// A cache that was chosen, on a host with memory to spare, is not a finding. Saying
// something about it anyway is how a diagnostic page becomes background noise.
func TestConfigAdviceLeavesAHealthyCacheAlone(t *testing.T) {
	d := fdCfgData(6, 19482, 0, 10, 1000, 400)
	// A cache that is not full and a host that is not short: nothing to do.
	d.Series["serverStatus.wiredTiger.cache.bytes currently in the cache"].Values = make([]int64, len(d.TS))
	c := fdCfgFind(fdCfgFor(d, "shard member"), "cacheSizeGB")
	if c == nil || c.Level != "ok" {
		t.Fatalf("a pinned, unpressured cache should be left alone, got %+v", c)
	}
}

// A mongos has no storage engine. Advice about its cache, its eviction or its journal is
// not merely useless, it is confidence in numbers that do not exist — the live router
// reported a "0 GiB" cache before this guard.
func TestConfigAdviceSaysNothingAboutARoutersStorage(t *testing.T) {
	d := fdCfgData(0, 19482, 0, 0, 0, 0)
	delete(d.Series, "serverStatus.wiredTiger.cache.maximum bytes configured")
	v := fdCfgFor(d, "mongos router")
	for _, c := range v {
		for _, bad := range []string{"cacheSizeGB", "eviction=", "commitIntervalMs", "oplog size"} {
			if strings.Contains(c.Setting, bad) {
				t.Errorf("a router got storage-engine advice: %q", c.Setting)
			}
		}
	}
	if fdCfgFind(v, "router has no storage engine") == nil {
		t.Error("the router should still be told where its tuning actually lives")
	}
}

// Eviction done by application threads means one of two opposite things, and the device
// decides which. Recommending more eviction threads against a saturated disk queues more
// work on the thing that is already the limit.
func TestConfigAdviceReadsEvictionAgainstTheDevice(t *testing.T) {
	busy := fdCfgFind(fdCfgFor(fdCfgData(6, 19482, 0, 780, 1000, 1000), "replica-set member"), "eviction")
	if busy == nil || !strings.Contains(busy.Suggest, "leave them") {
		t.Errorf("with the device saturated, eviction threads are not the answer: %+v", busy)
	}
	idle := fdCfgFind(fdCfgFor(fdCfgData(6, 19482, 0, 780, 1000, 200), "replica-set member"), "eviction")
	if idle == nil || !strings.Contains(idle.Suggest, "threads_max=8") {
		t.Errorf("with the device idle, more eviction threads is the answer: %+v", idle)
	}
}

// A member that evicted a handful of pages can show 100% application eviction and mean
// nothing by it. The live router's shards did exactly that between benchmark runs.
func TestConfigAdviceIgnoresEvictionWithNothingBehindIt(t *testing.T) {
	d := fdCfgData(6, 19482, 0, 1, 1, 200) // 60 pages over the window, all by app threads
	if c := fdCfgFind(fdCfgFor(d, "shard member"), "eviction"); c != nil {
		t.Errorf("eviction advice from 60 pages: %+v", c)
	}
}

// Tickets running out is the most misread signal in MongoDB tuning. With no wait behind
// them the advice is to leave them alone, in as many words.
func TestConfigAdviceRefusesToRaiseTicketCounts(t *testing.T) {
	d := fdCfgData(6, 19482, 0, 10, 1000, 400)
	n := len(d.TS)
	avail := make([]int64, n)
	for i := range avail {
		avail[i] = int64(8 - i%9) // reaches zero
	}
	d.Series["serverStatus.queues.execution.read.available"] = &ftdcSeries{Values: avail}
	c := fdCfgFind(fdCfgFor(d, "replica-set member"), "Concurrent")
	if c == nil {
		t.Fatal("tickets hit zero and the page said nothing about them")
	}
	if c.Level != "ok" || !strings.Contains(c.Suggest, "leave them alone") {
		t.Errorf("with no admission wait the answer is to leave them: %+v", c)
	}
}

// The budget is a host total, and it is deliberately not the default's arithmetic: mongod
// reserves 1 GiB for everything that is not the cache, which on a real member is wrong by
// several gigabytes before a single connection is opened.
func TestCacheBudgetLeavesTheHostRoomToBreathe(t *testing.T) {
	if got := fdCacheBudget(30105); got < 20 || got > 22 {
		t.Errorf("29.4 GiB host: budget %.1f GiB, want the ~21 GiB that ran without swapping", got)
	}
	if got := fdCacheBudget(4096); got > 2.5 {
		t.Errorf("a 4 GiB host cannot give %.1f GiB to caches", got)
	}
}

// A capture that spans a retune carries both cache sizes. Read as a maximum, the page tells
// somebody who has just fixed their configuration that they have not — which is how a
// diagnostic tool loses a reader for good.
func TestConfigAdviceReadsTheCacheAsItIsNow(t *testing.T) {
	d := fdCfgData(6, 19482, 0, 10, 1000, 400)
	v := d.Series["serverStatus.wiredTiger.cache.maximum bytes configured"].Values
	derivedBytes := 14.19 * float64(int64(1)<<30) // what a 29.4 GiB host derives
	for i := 0; i < len(v)/2; i++ {               // the first half of the window ran at that size
		v[i] = int64(derivedBytes)
	}
	got := fdCfgFor(d, "replica-set member")
	// By the end of this window the cache is pinned at 6 GiB, so the "nobody set this"
	// verdict must not fire — whatever else the page decides to say about it.
	if c := fdCfgFind(got, "cacheSizeGB"); c != nil && c.Level == "crit" {
		t.Fatalf("read the cache as it was, not as it is: %+v", c)
	}
	if fdCfgFind(got, "changed during this capture") == nil {
		t.Error("a cache that was resized mid-capture should be called out — every rate above it spans both")
	}
}
