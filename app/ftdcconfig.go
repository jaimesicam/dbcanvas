package main

// ftdcconfig.go — turning a capture into configuration advice.
//
// Every other advisor on this page reads one chart and says what it shows. This one reads
// the capture as a whole and answers the question people actually arrive with: what should I
// change. It is deliberately separate from the charts because the useful recommendations are
// cross-cutting — the cache is too large *because* the host is swapping, and the tickets are
// exhausted *because* the disk is saturated, and neither statement can be made from one
// chart.
//
// Every rule here was derived from measured runs on one machine (29.4 GiB, 20 threads), not
// from a tuning guide. The benchmark was identical each time — the app's own rw workload,
// scale 4, 32 threads, 180 s measured, with the Stock Market Sim driving the same background
// load — and only the server configuration changed:
//
//	replica set, MongoDB defaults (cache 14.19 GiB × 3 members)   111 TPS   p95 710 ms
//	replica set, cacheSizeGB 6 / 6 / 6                            377 TPS   p95 167 ms
//	replica set, cacheSizeGB 10 / 5 / 5                           637 TPS   p95  71 ms
//	replica set, 10 / 5 / 5 + zstd collections and journal        623 TPS   p95  70 ms
//	sharded, MongoDB defaults (cache 14.19 GiB × 5 mongods)       595 TPS   p95  92 ms
//	sharded, shards 6 GiB each, config server 1 GiB               942 TPS   p95  43 ms
//
// Three of those results are the whole argument for this file. The default cache is sized
// from the machine's memory as though this mongod were the only thing on it, which on a host
// running three members means promising 43 GiB on a 29 GiB box — the baseline run swapped at
// 72,000 pages/s, took 710 ms at p95, and ended with the primary aborting on a fatal RSTL
// timeout. Sizing the cache to the real budget was worth 5.7×. And zstd, the change that
// sounds like it should have helped an I/O-bound box, was worth nothing measurable — which
// is why "keep it as it is" is a recommendation this file is willing to make.

import (
	"fmt"
	"strconv"
	"strings"
)

// fdConfig is one recommendation.
type fdConfig struct {
	// Level is the same vocabulary the chart advisors use: crit changes throughput or
	// stability, warn is worth doing, ok is "this is already right, do not change it".
	Level   string `json:"level"`
	Setting string `json:"setting"`
	Current string `json:"current,omitempty"`
	Suggest string `json:"suggest,omitempty"`
	// Why is the measured evidence from THIS capture, not a general argument.
	Why string `json:"why"`
	// Effect is what the change did when it was measured, where it was measured.
	Effect string `json:"effect,omitempty"`
}

// fdConfigAdvice reads the whole capture and returns what to change, worst first.
func fdConfigAdvice(d *ftdcData, m *fdModel) []fdConfig {
	var out []fdConfig
	add := func(c fdConfig) { out = append(out, c) }

	// ---- the machine, as the capture describes it ----------------------------------
	memMB := fdMetaNum(d, "memSizeMB")
	// The LAST value, not the largest: a capture that spans a restart carries both the
	// old cache size and the new one, and "what is it now" is the only reading that can be
	// acted on. Verified against a live member that had been retuned mid-capture — read as
	// a maximum it reported the size it no longer had.
	cacheSeries := fdFloats(d, "serverStatus.wiredTiger.cache.maximum bytes configured", 1)
	cacheMaxB := fdLastPositive(cacheSeries)
	cacheGB := cacheMaxB / (1 << 30)
	// What mongod picks when nothing pins it: half of (RAM − 1 GiB), floor 256 MiB.
	derived := (memMB/1024 - 1) / 2
	if derived < 0.25 {
		derived = 0.25
	}
	// Whether the cache was pinned is decided from the number itself, not from the
	// metadata. A capture that spans a restart carries only the LAST startup's options,
	// so a window from before the change would otherwise be read against the settings
	// that came after it — and an uploaded capture may carry no options at all. A cache
	// that lands within a whisker of half the machine's memory was not chosen by anyone.
	pinned := derived <= 0 || cacheGB < derived*0.97 || cacheGB > derived*1.03

	// ---- how the host actually behaved ---------------------------------------------
	availMB := fdMin(fdFloats(d, "systemMetrics.memory.MemAvailable_kb", 1/1024.0))
	swapTotal := fdMax(fdFloats(d, "systemMetrics.memory.SwapTotal_kb", 1/1024.0))
	swapFree := fdMin(fdFloats(d, "systemMetrics.memory.SwapFree_kb", 1/1024.0))
	swapUsed := 0.0
	if swapTotal > 0 {
		swapUsed = swapTotal - swapFree
	}
	swapIn := fdMax(fdRate(d, "systemMetrics.vmstat.pswpin"))
	swapOut := fdMax(fdRate(d, "systemMetrics.vmstat.pswpout"))
	memStall := fdMax(fdRate(d, "systemMetrics.pressure.memory.some.totalMicros")) / 10000

	// ---- how the storage engine behaved --------------------------------------------
	cacheUsed := fdMax(fdFloats(d, "serverStatus.wiredTiger.cache.bytes currently in the cache", 1))
	fill := 0.0
	if cacheMaxB > 0 {
		fill = cacheUsed / cacheMaxB * 100
	}
	readIn := fdMax(fdRate(d, "serverStatus.wiredTiger.cache.bytes read into cache")) / (1 << 20)
	appEvict := fdDelta(d, "serverStatus.wiredTiger.cache.application threads page write from cache to disk count")
	allEvict := fdDelta(d, "serverStatus.wiredTiger.cache.pages written from cache")
	appShare := 0.0
	if allEvict > 0 {
		appShare = appEvict / allEvict * 100
	}
	oplogInCache := fdMax(fdFloats(d, "local.oplog.rs.stats.storageStats.wiredTiger.cache.bytes currently in the cache", 1))
	oplogShare := 0.0
	if cacheMaxB > 0 {
		oplogShare = oplogInCache / cacheMaxB * 100
	}

	// ---- admission and the device ---------------------------------------------------
	ticketsOut := fdTicketsExhausted(d)
	admitMs := fdMax(fdRatio(d, "serverStatus.queues.execution.read.normalPriority.totalTimeQueuedMicros",
		"serverStatus.queues.execution.read.normalPriority.finishedProcessing", 0.001))
	journalMs := fdMax(fdRatio(d, "serverStatus.wiredTiger.log.log sync time duration (usecs)",
		"serverStatus.wiredTiger.log.log sync operations", 0.001))
	diskBusy, diskDev := fdWorstDisk(d)
	cpuIdle := fdCPUIdleShare(d)

	// A mongos has no storage engine, so every rule below it is meaningless there —
	// and a cache of "0 GiB, leave it" is worse than meaningless, it is confidence in a
	// number that does not exist. Say the one true thing about a router and stop.
	if cacheMaxB <= 0 {
		if m != nil && strings.Contains(strings.ToLower(m.Role), "mongos") {
			add(fdConfig{Level: "ok",
				Setting: "the router has no storage engine",
				Current: "no cache, no eviction, no journal",
				Suggest: "tune the shards, not this",
				Why:     "A mongos holds no data. Its own limits are connections and its task-executor pools; everything about memory, eviction and durability belongs to the members behind it.",
				Effect:  "Measured on this hardware: sizing the three shards' caches to the host budget (6 GiB each, 1 GiB for the config server) took a sharded cluster from 595 TPS to 942 TPS at p95 43 ms, without touching the router at all."})
		}
		out = append(out, fdHostConfig(d)...)
		return out
	}

	// A cache that changed mid-capture is worth a line of its own: every rate above it
	// straddles the change, and the reader is usually the person who made it.
	if lo, hi := fdMin(cacheSeries), fdMax(cacheSeries); lo > 0 && hi > lo*1.05 {
		add(fdConfig{Level: "ok",
			Setting: "the cache changed during this capture",
			Current: fmt.Sprintf("%.1f GiB → %.1f GiB", lo/(1<<30), hi/(1<<30)),
			Suggest: "read one side of the change at a time",
			Why:     "Everything else on this page is computed across the whole window, so any total or rate here spans both configurations. Zoom into one side before comparing them."})
	}

	// ================================================================= cache sizing
	switch {
	case !pinned && (swapIn+swapOut > 100 || availMB < 1024 || memStall > 50):
		// The headline finding. Nothing pinned the cache, so mongod took half the
		// machine — and the machine was not its to take.
		budget := fdCacheBudget(memMB)
		add(fdConfig{Level: "crit",
			Setting: "storage.wiredTiger.engineConfig.cacheSizeGB",
			Current: fmt.Sprintf("unset — mongod derived %.1f GiB (half of RAM − 1 GiB)", derived),
			Suggest: fmt.Sprintf("pin it. Across EVERY mongod on this %.0f GiB host, the caches should total about %.0f GiB — divide that between them, and weight it towards the member actually serving the workload rather than splitting it evenly", memMB/1024, budget),
			Why: fmt.Sprintf("The host had %s MiB of memory available at its worst, %s MiB of swap in use, and paged %s pages/s. mongod sizes its cache from the machine's total memory as though it were alone on it; every other mongod, and everything else on the box, is invisible to that calculation.",
				fdAmt(availMB), fdAmt(swapUsed), fdAmt(swapIn+swapOut)),
			Effect: "Measured on this hardware: three members each deriving 14.19 GiB on a 29.4 GiB host ran at 111 TPS with p95 710 ms and ended with the primary aborting on a fatal RSTL timeout. Pinning them to 6/6/6 gave 377 TPS; moving the same total to 10/5/5, with the weight on the primary, gave 637 TPS at p95 71 ms — 5.7× the default, with no other change."})
	case fill >= 90 && readIn >= 50 && availMB > 4096 && swapIn+swapOut < 100:
		// The opposite case: the cache is the constraint and the host has room.
		add(fdConfig{Level: "warn",
			Setting: "storage.wiredTiger.engineConfig.cacheSizeGB",
			Current: fmt.Sprintf("%.0f GiB", cacheGB),
			Suggest: fmt.Sprintf("%.0f GiB while the host still has %s MiB free", cacheGB*1.5, fdAmt(availMB)),
			Why:     fmt.Sprintf("The cache ran %s full and the engine read %s MiB/s back off the device — the working set does not fit. The host is not short of memory, so this is a ceiling you chose rather than one the machine imposed.", fdPctV(fill), fdAmt(readIn)),
			Effect:  "Measured on this hardware: raising the primary's cache from 6 GiB to 10 GiB while dropping the secondaries to 5 GiB took the same workload from 377 TPS to 637 TPS at p95 71 ms — the member serving the workload is the one that needs the memory."})
	case pinned && fill < 50 && availMB > 4096:
		add(fdConfig{Level: "ok",
			Setting: "storage.wiredTiger.engineConfig.cacheSizeGB",
			Current: fmt.Sprintf("%.0f GiB", cacheGB),
			Suggest: "leave it",
			Why:     fmt.Sprintf("The cache never went past %s full and the host kept %s MiB available. There is nothing to win here.", fdPctV(fill), fdAmt(availMB))})
	}

	// ================================================================= eviction
	// The share is only worth reporting when there was real eviction behind it: a member
	// that evicted 40 pages in the window can show 100% and mean nothing.
	if appShare >= 20 && allEvict >= 5000 {
		if diskBusy >= 80 {
			add(fdConfig{Level: "warn",
				Setting: "eviction threads (wiredTigerEngineRuntimeConfig)",
				Current: "default",
				Suggest: "leave them — the device is the limit",
				Why:     fmt.Sprintf("Application threads did %s of the eviction, which normally means eviction is short of threads. Not here: %s was %s busy. More eviction threads would queue on the same device.", fdPctV(appShare), diskDev, fdPctV(diskBusy)),
				Effect:  "Measured on this hardware: with the disk saturated, application threads did 78% of eviction and adding cache — not eviction threads — is what moved throughput."})
		} else {
			add(fdConfig{Level: "warn",
				Setting: "eviction=(threads_min,threads_max)",
				Current: "default (4, 4)",
				Suggest: "threads_min=4, threads_max=8",
				Why:     fmt.Sprintf("Application threads did %s of the eviction while %s was only %s busy — the eviction workers had headroom on the device and ran out of threads instead. Operations that evict pay for it in latency with no slow query to show for it.", fdPctV(appShare), diskDev, fdPctV(diskBusy))})
		}
	}

	// ================================================================= tickets
	if ticketsOut {
		if admitMs < 10 {
			add(fdConfig{Level: "ok",
				Setting: "storageEngineConcurrent{Read,Write}Transactions",
				Current: "dynamic (8.0 default)",
				Suggest: "leave them alone",
				Why:     fmt.Sprintf("Tickets ran out, which looks alarming and is not the problem: operations waited at most %s ms to be admitted. The queue is short because the work behind it is slow, and raising the ticket count only lets more operations wait inside the engine instead of outside it.", fdAmt(admitMs))})
		} else {
			add(fdConfig{Level: "crit",
				Setting: "the storage engine, not the ticket count",
				Current: fmt.Sprintf("operations waited up to %s ms for admission", fdAmt(admitMs)),
				Suggest: "fix the cache or the device first; do not raise the ticket count",
				Why:     "Tickets exhausted with a real wait behind them means the engine cannot retire work fast enough. More tickets would add concurrency to a component that is already the bottleneck, which reliably makes latency worse.",
				Effect:  "Measured on this hardware: admission waits fell from 121 ms to 3.5 ms when the cache was sized correctly — the ticket count was never touched."})
		}
	}

	// ================================================================= journal / device
	if journalMs >= 50 {
		add(fdConfig{Level: "warn",
			Setting: "storage.journal.commitIntervalMs (and the device under dbPath)",
			Current: fmt.Sprintf("journal syncs averaging up to %s ms", fdAmt(journalMs)),
			Suggest: "raise the commit interval only if the durability window allows; otherwise this is hardware",
			Why:     fmt.Sprintf("Every write with j:true waits for one of these. %s was %s busy at its worst, so the syncs are slow because the device is, not because they are frequent.", diskDev, fdPctV(diskBusy)),
			Effect:  "Measured on this hardware: journal syncs went from 57 ms to 1001 ms as throughput rose 5.7× — the faster the server ran, the more it was waiting on the disk. That is the ceiling this box has."})
	}

	// ================================================================= the oplog
	if oplogShare >= 25 {
		add(fdConfig{Level: "warn",
			Setting: "oplog size vs cache size",
			Current: fmt.Sprintf("the oplog held %s of the cache (%s MiB)", fdPctV(oplogShare), fdAmt(oplogInCache/(1<<20))),
			Suggest: "size the cache for the working set PLUS the oplog, or lower the write rate",
			Why:     "The oplog is a collection and lives in the same cache as the data. A quarter of the cache spent on it is a quarter the working set does not get.",
			Effect:  "Measured on this hardware: at the default cache the oplog held 42% of it; after sizing the cache the same oplog held 1.4%."})
	}

	// ================================================================= compression
	if diskBusy >= 80 && cpuIdle >= 30 {
		add(fdConfig{Level: "ok",
			Setting: "storage.wiredTiger.collectionConfig.blockCompressor",
			Current: "snappy (default)",
			Suggest: "zstd is worth ONE experiment, and may well be a wash",
			Why:     fmt.Sprintf("%s was %s busy while %s of the CPU was idle, which is the shape where trading CPU for I/O usually pays.", diskDev, fdPctV(diskBusy), fdPctV(cpuIdle)),
			Effect:  "Measured on this hardware it did not pay: zstd for collections and journal moved 637 TPS to 623 TPS — inside the noise — while p99 improved from 120 ms to 82 ms. Change it for the tail latency if that is what you are buying, not for throughput."})
	}

	out = append(out, fdHostConfig(d)...)
	return out
}

// fdHostConfig is the advice that is about the machine rather than the storage engine, so
// it is the same for a mongos, a config server and a shard member.
func fdHostConfig(d *ftdcData) []fdConfig {
	var out []fdConfig
	if d.Meta["thp"] == "always" {
		out = append(out, fdConfig{Level: "warn",
			Setting: "transparent huge pages",
			Current: "always",
			Suggest: "never (or madvise)",
			Why:     "MongoDB asks for THP to be off. Left on it inflates resident memory and lengthens allocation stalls, which on a box that is already short of memory is the difference between tight and swapping."})
	}
	if fd := fdMetaNum(d, "fileDescriptors"); fd > 0 && fd < 64000 {
		out = append(out, fdConfig{Level: "warn",
			Setting: "ulimit -n (file descriptors)",
			Current: strconv.Itoa(int(fd)),
			Suggest: "64000",
			Why:     "Below MongoDB's own recommendation. A connection storm hits this ceiling before it hits any limit in the server, and the failure looks like a network problem rather than a limit."})
	}
	cores, coresAvail := fdMetaNum(d, "cores"), fdMetaNum(d, "coresAvailable")
	if cores > 0 && coresAvail > 0 && coresAvail < cores {
		out = append(out, fdConfig{Level: "warn",
			Setting: "CPU allocation for this process",
			Current: fmt.Sprintf("%.0f of %.0f cores", coresAvail, cores),
			Suggest: "give it the cores or accept the ceiling",
			Why:     "The process is restricted to fewer cores than the machine has, so every per-core rate on this page is against the smaller number."})
	}
	return out
}

// fdCacheBudget is how much cache a host can carry in TOTAL, across every mongod on it.
// It is deliberately a total and not a per-process number, because a capture from one
// member cannot see its siblings — and the failure this whole file exists to catch is
// precisely each member sizing itself as though it were alone.
//
// The reserve is 8 GiB rather than mongod's own 1 GiB because a mongod needs far more than
// its cache: the heap, connection stacks, sorts, and the file-system cache the engine reads
// its own compressed blocks through. Measured here, 20 GiB of cache across three members on
// a 29.4 GiB host ran without swapping; the 42.6 GiB the default asked for did not.
func fdCacheBudget(memMB float64) float64 {
	gb := memMB / 1024
	if reserve := 8.0; gb > reserve+1 {
		return gb - reserve
	}
	return gb / 2
}

// fdLastPositive is the most recent real value of a gauge — what the setting is NOW.
func fdLastPositive(v []float64) float64 {
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] > 0 {
			return v[i]
		}
	}
	return 0
}

// fdMetaNum reads a number out of the capture header, which stores everything as text.
func fdMetaNum(d *ftdcData, key string) float64 {
	v := strings.Fields(d.Meta[key])
	if len(v) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(v[0], 64)
	if err != nil {
		return 0
	}
	return f
}

// fdDelta is how much a counter moved across the whole window.
func fdDelta(d *ftdcData, key string) float64 {
	v := fdFloats(d, key, 1)
	if len(v) == 0 {
		return 0
	}
	return fdMax(v) - fdMin(v)
}

// fdTicketsExhausted reports whether admission ever ran dry, under either version's name.
func fdTicketsExhausted(d *ftdcData) bool {
	for _, k := range []string{
		"serverStatus.queues.execution.read.available",
		"serverStatus.queues.execution.write.available",
		"serverStatus.wiredTiger.concurrentTransactions.read.available",
		"serverStatus.wiredTiger.concurrentTransactions.write.available",
	} {
		v := fdFloats(d, k, 1)
		if len(v) > 0 && fdMin(v) == 0 && fdMax(v) > 0 {
			return true
		}
	}
	return false
}

// fdWorstDisk is the busiest device in the capture and how busy it got.
func fdWorstDisk(d *ftdcData) (float64, string) {
	worst, name := 0.0, "the device"
	for key := range d.Series {
		rest, ok := strings.CutPrefix(key, "systemMetrics.disks.")
		if !ok || !strings.HasSuffix(rest, ".io_time_ms") {
			continue
		}
		dev := strings.TrimSuffix(rest, ".io_time_ms")
		if b := fdMax(fdRate(d, key)) / 10; b > worst {
			worst, name = b, dev
		}
	}
	if worst > 100 {
		worst = 100
	}
	return worst, name
}

// fdCPUIdleShare is how much of the machine's CPU was idle at the busiest moment's opposite:
// the minimum idle share, which is what says whether there is headroom to trade.
func fdCPUIdleShare(d *ftdcData) float64 {
	cores := fdMetaNum(d, "cores")
	if cores <= 0 {
		cores = 1
	}
	idle := fdRate(d, "systemMetrics.cpu.idle_ms")
	if len(idle) == 0 {
		return 0
	}
	// ms of idle per second across all cores, as a share of the machine.
	share := fdMin(idle) / (10 * cores)
	if share > 100 {
		share = 100
	}
	return share
}
