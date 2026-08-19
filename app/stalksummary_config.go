package main

// stalksummary_config.go — which server variables to change, argued from the capture.
//
// The advisors in stalksummary_advice.go each read one chart and mention settings in
// passing. This reads the capture as a whole and answers the question somebody holding a
// pt-stalk archive actually has: what do I change, and what will it buy me. It is separate
// from the per-chart advisors for the same reason the FTDC one is (see app/ftdcconfig.go):
// the useful recommendations are cross-cutting. The buffer pool is too small *and* the
// flush method is hiding it, because InnoDB's own read counter and the device's read
// counter disagree by two orders of magnitude — neither chart can say that alone.
//
// Every threshold and every measured effect here comes from benchmark runs on one machine
// (29.4 GiB, 20 threads, Percona Server 8.0.46), not from a tuning guide. See
// IMPLEMENTATION.md for the full table.

import (
	"fmt"
	"strings"
)

// vsConfig is one recommendation: what the setting is, what to make it, why from this
// capture, and what the change was worth when it was measured.
type vsConfig struct {
	Level    string `json:"level"` // crit | warn | info | ok — ok means "keep this"
	Variable string `json:"variable"`
	Current  string `json:"current,omitempty"`
	Suggest  string `json:"suggest,omitempty"`
	Why      string `json:"why"`
	Effect   string `json:"effect,omitempty"`
	// Risk is what the change costs, and is set for exactly the settings that trade a
	// guarantee for speed. A page that recommends sync_binlog=0 without saying what is
	// lost is not giving advice, it is giving somebody else's benchmark.
	Risk string `json:"risk,omitempty"`
}

// vsHostMemBytes is the machine's RAM, from pt-summary's own line ("29.4G").
func vsHostMemBytes(m *vsModel) float64 {
	s := strings.TrimSpace(m.Summary.Facts["memory"])
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'T', 't':
		mult = 1 << 40
	case 'G', 'g':
		mult = 1 << 30
	case 'M', 'm':
		mult = 1 << 20
	case 'K', 'k':
		mult = 1 << 10
	default:
		mult = 1
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return 0
	}
	return v * mult
}

// vsConfigAdvice reads the whole capture and returns what to change, worst first.
func vsConfigAdvice(m *vsModel) []vsConfig {
	var out []vsConfig
	add := func(c vsConfig) { out = append(out, c) }
	ram := vsHostMemBytes(m)
	galera := m.Source.Engine == "pxc"

	// ---- what the machine and the workload actually did ---------------------------
	var reqs, misses, missPct float64
	if s := m.Series["bufferPool"]; s != nil {
		reqs, misses = seriesMedian(s, "readReqPerSec"), seriesMedian(s, "diskReadPerSec")
		missPct = seriesMedian(s, "missRatioPct")
	}
	var innoRead, devRead float64
	if s := m.Series["innodbIO"]; s != nil {
		innoRead = seriesMedian(s, "read")
	}
	if m.Disk != nil {
		devRead = vsDiskTotal(m, "rKBs") * 1024
	}
	var dataFsync, logFsync float64
	if s := m.Series["fsyncs"]; s != nil {
		dataFsync, logFsync = seriesMedian(s, "data"), seriesMedian(s, "log")
	}
	var ckptPct float64
	if s := m.Series["checkpointAge"]; s != nil {
		if cap, ok := m.redoCapacityBytes(); ok && cap > 0 {
			ckptPct = seriesMax(s, "age") / cap * 100
		}
	}

	// ================================================== innodb_buffer_pool_size
	pool, hasPool := m.varNum("innodb_buffer_pool_size")
	switch {
	case hasPool && ram > 0 && pool <= 256<<20 && ram >= 4<<30:
		// The MySQL default is 128 MiB and has been since machines had 512 MiB of
		// memory. It is the single most consequential unset variable in the file.
		want := vsPoolTarget(ram)
		add(vsConfig{Level: vsCrit, Variable: "innodb_buffer_pool_size",
			Current: humanBytes(pool),
			Suggest: fmt.Sprintf("%s to start — but that is half of this %s machine, so divide it if other database processes share the host, and weight it towards the one serving the workload", humanBytes(want), humanBytes(ram)),
			Why: fmt.Sprintf("%s of RAM on this host and InnoDB is allowed %s of it. The pool served %s requests/s with %s misses/s (%.1f%%) — every one of those misses is a page InnoDB had to go and find because there was nowhere to keep it.",
				humanBytes(ram), humanBytes(pool), compactNum(reqs), compactNum(misses), missPct),
			Effect: "Measured on this hardware (29.4 GiB, 20 threads, Percona Server 8.0.46, a 1.4 M-row OLTP workload with a second application on the same server): 19.8 TPS at p95 4.7 s with the 128 MiB default, and 727 TPS at p95 105 ms with the source at 8 GiB and its two replicas at 2 GiB each — 12 GiB across three servers on one host, not 16 GiB each. Nothing else changed. On an asynchronous replication stack this was the largest single difference measured; on a Galera cluster whose commits were already the bottleneck, the same change was worth 9%."})
	case hasPool && ram > 0 && pool < ram/4 && missPct >= 1:
		add(vsConfig{Level: vsWarn, Variable: "innodb_buffer_pool_size",
			Current: humanBytes(pool),
			Suggest: fmt.Sprintf("%s, if nothing else on this %s machine needs the memory", humanBytes(pool*2), humanBytes(ram)),
			Why: fmt.Sprintf("%.1f%% of page requests missed the pool (%s/s). The working set is larger than the pool and the host has memory left to give it.",
				missPct, compactNum(misses)),
			Effect: "Measured on this hardware: the same workload ran at 19.8 TPS with a 128 MiB pool and 727 TPS with an 8 GiB one — 37x, from this setting alone."})
	case hasPool && missPct < 0.5 && reqs > 0:
		add(vsConfig{Level: vsOK, Variable: "innodb_buffer_pool_size",
			Current: humanBytes(pool),
			Suggest: "leave it",
			Why:     fmt.Sprintf("Only %.2f%% of %s page requests/s missed the pool. The working set fits; more memory here would buy nothing.", missPct, compactNum(reqs))})
	}

	// ================================================== innodb_flush_method
	// The signature is a disagreement between two counters, which is why no single
	// chart can raise it: InnoDB says it read hundreds of MiB/s while the devices say
	// they served almost none. The difference is the operating system's page cache,
	// holding a second copy of pages InnoDB is already caching.
	if fm := m.vars["innodb_flush_method"]; fm != "" && !strings.Contains(strings.ToUpper(fm), "O_DIRECT") &&
		innoRead >= 10<<20 && devRead < innoRead/4 {
		add(vsConfig{Level: vsWarn, Variable: "innodb_flush_method",
			Current: fm,
			Suggest: "O_DIRECT",
			Why: fmt.Sprintf("InnoDB read %s/s while the devices served %s/s. The gap is the operating system's page cache keeping a second copy of pages InnoDB already caches — memory that would do more work inside the buffer pool.",
				humanBytes(innoRead), humanBytes(devRead)),
			Risk:   "Requires a restart, and on filesystems without O_DIRECT support InnoDB falls back silently. Give the buffer pool the memory the page cache stops using, or the change is a downgrade.",
			Effect: "Measure it rather than assuming: it is the one recommendation here that did not pay on the hardware this advice was built on. With per-commit durability still in force it COST 20% (689 to 553 TPS), because the host's page cache was absorbing writes for a dataset that fitted in it; on the fully tuned server it was neutral (2,105 to 2,126 TPS, inside the noise) and improved p99 slightly. Where the dataset is much larger than memory the usual result is the opposite. Two runs settle it."})
	}

	// ================================================== innodb_redo_log_capacity
	if capBytes, ok := m.redoCapacityBytes(); ok && capBytes > 0 {
		switch {
		case ckptPct >= 60:
			add(vsConfig{Level: vsCrit, Variable: "innodb_redo_log_capacity",
				Current: humanBytes(capBytes),
				Suggest: fmt.Sprintf("%s or more", humanBytes(capBytes*8)),
				Why:     fmt.Sprintf("Checkpoint age reached %.0f%% of the redo log. Past about three quarters InnoDB stops accepting the write rate and flushes synchronously, and throughput does not degrade — it collapses.", ckptPct),
				Risk:    "Costs disk space and lengthens crash recovery. Nothing else."})
		case capBytes <= 200<<20 && dataFsync+logFsync >= 100:
			add(vsConfig{Level: vsWarn, Variable: "innodb_redo_log_capacity",
				Current: humanBytes(capBytes),
				Suggest: fmt.Sprintf("%s on a write-heavy server", humanBytes(2<<30)),
				Why:     fmt.Sprintf("Still the shipped default at %s while this server is doing %s fsyncs/s. A small redo log forces frequent checkpoints, which turns a smooth flush into a series of stalls.", humanBytes(capBytes), compactNum(dataFsync+logFsync))})
		}
	}

	// ================================================== the durability pair
	sb, hasSB := m.varNum("sync_binlog")
	ft, hasFT := m.varNum("innodb_flush_log_at_trx_commit")
	if hasSB && hasFT {
		switch {
		case sb == 1 && ft == 1 && (dataFsync+logFsync >= 200 || (galera && vsFCPaused(m) >= 10)):
			add(vsConfig{Level: vsInfo, Variable: "sync_binlog, innodb_flush_log_at_trx_commit",
				Current: fmt.Sprintf("%.0f, %.0f — full durability, an fsync per commit", sb, ft),
				Suggest: "0 and 2 if, and only if, this data can be rebuilt",
				Why:     vsDurabilityWhy(m, galera, dataFsync, logFsync),
				Risk:    "A power cut or an OS crash loses up to a second of committed transactions, and the binary log can end up behind the data — which breaks replicas built from it. A mysqld crash alone is still safe at flush_log_at_trx_commit=2. Correct for a test rig, a rebuildable cache or a reporting replica; wrong for anything holding the only copy of something."})
		case sb == 0 && ft != 1:
			add(vsConfig{Level: vsWarn, Variable: "sync_binlog, innodb_flush_log_at_trx_commit",
				Current: fmt.Sprintf("%.0f, %.0f — relaxed", sb, ft),
				Suggest: "1 and 1 on anything holding data you would miss",
				Why:     "Both durability settings are off. This is a deliberate trade and the capture cannot tell whether it was deliberate here.",
				Risk:    "Recent commits are lost on a host crash and the binary log can end up behind the data."})
		}
	}

	// ================================================== innodb_io_capacity
	if ioc, ok := m.varNum("innodb_io_capacity"); ok && ioc <= 200 && m.Disk != nil {
		if w := vsDiskTotal(m, "wKBs"); w >= 20000 { // ~20 MB/s of sustained writing
			add(vsConfig{Level: vsWarn, Variable: "innodb_io_capacity / innodb_io_capacity_max",
				Current: fmt.Sprintf("%.0f / %s", ioc, m.vars["innodb_io_capacity_max"]),
				Suggest: "2000 / 8000 on SSD or NVMe",
				Why:     fmt.Sprintf("The default assumes a single spinning disk doing 200 operations a second, while this host wrote %s/s during the capture. InnoDB paces its background flushing by this number, so leaving it at 200 makes it flush lazily and then panic.", humanBytes(w*1024))})
		}
	}

	// ================================================== buffer pool instances
	if inst, ok := m.varNum("innodb_buffer_pool_instances"); ok && inst <= 1 && pool >= 4<<30 {
		add(vsConfig{Level: vsInfo, Variable: "innodb_buffer_pool_instances",
			Current: fmt.Sprintf("%.0f", inst),
			Suggest: "8 for a pool this size",
			Why:     fmt.Sprintf("A %s pool behind a single mutex. Splitting it spreads the contention across instances; below about 1 GiB it makes no difference and MySQL ignores it.", humanBytes(pool))})
	}

	// ================================================== replica apply
	// A replica that cannot keep up does not show as a fault anywhere else: the primary
	// looks fast, the replica looks connected, and the lag grows. Measured on this
	// hardware, a single applier fell behind by 43 SECONDS PER MINUTE against a primary
	// doing 2,438 TPS — and kept falling behind after the benchmark stopped, because it
	// could not keep up with the background application either.
	if !galera {
		lag := 0.0
		if s := m.Series["replicationLag"]; s != nil {
			lag = seriesMax(s, "seconds")
		}
		workers, hasW := m.varNum("replica_parallel_workers")
		if !hasW {
			workers, hasW = m.varNum("slave_parallel_workers")
		}
		switch {
		// Ten seconds, not a minute: a replica that is steadily ten seconds behind is one
		// you cannot fail over to without losing writes, and the number only grows.
		// Measured live at 23 s behind with a single applier while the source ran at
		// 2,238 TPS — with eight it was 0.
		case hasW && workers <= 1 && lag >= 10:
			add(vsConfig{Level: vsCrit, Variable: "replica_parallel_workers",
				Current: fmt.Sprintf("%.0f — one thread applying everything the source produces", workers),
				Suggest: "8 (with binlog_transaction_dependency_tracking=WRITESET on the source)",
				Why:     fmt.Sprintf("This replica was %s behind at its worst. The source commits in parallel across many connections and a single applier replays them one at a time, so the gap grows with the write rate rather than with the size of any one transaction.", vsDur(lag)),
				Effect:  "Measured on this hardware: with one applier the replica fell behind by 43 seconds every minute and reached 23 minutes of lag; with 8 workers and WRITESET dependency tracking on the source it cleared that entire backlog in under three minutes and finished a 180 s run at 2,074 TPS with zero lag. The primary gave up 15% of its throughput to do it — which is the honest price, and it is the difference between a replica you can fail over to and one you cannot.",
				Risk:    "Parallel apply needs replica_preserve_commit_order=ON (the default since 8.0.27) to keep the replica's commit order identical to the source's. Without it, a reader on the replica can see transactions in an order the source never had."})
		case hasW && workers <= 1:
			add(vsConfig{Level: vsWarn, Variable: "replica_parallel_workers",
				Current: fmt.Sprintf("%.0f", workers),
				Suggest: "8, before you need it",
				Why:     "One applier thread. It is keeping up at this write rate, and it will stop keeping up the moment the source is tuned or the workload grows — the lag then builds silently, because nothing on either server reports it as an error."})
		case hasW && workers > 1 && lag >= 60:
			add(vsConfig{Level: vsWarn, Variable: "replica_parallel_workers",
				Current: fmt.Sprintf("%.0f", workers),
				Suggest: fmt.Sprintf("%.0f, and check binlog_transaction_dependency_tracking on the source", workers*2),
				Why:     fmt.Sprintf("Still %s behind with %.0f appliers. If the source is tracking dependencies by COMMIT_ORDER rather than WRITESET, most transactions look dependent on each other and the extra workers have nothing they are allowed to do in parallel.", vsDur(lag), workers)})
		}
	}

	// ================================================== Galera
	// A Galera cluster commits at the speed of its slowest applier, and says so in one
	// number: the share of time flow control had the writers paused. Measured on this
	// hardware, that number went 99.9% -> 18.8% -> 0.0% across the three configurations
	// below, while throughput went 31 -> 1,914 -> 2,032 TPS.
	if galera {
		fcPaused, recvQ := 0.0, 0.0
		if s := m.Series["galera"]; s != nil {
			fcPaused = seriesMedian(s, "flowControlPausedPct")
			recvQ = seriesMax(s, "recvQueue")
		}
		th, hasTh := m.varNum("wsrep_slave_threads")
		prov := m.vars["wsrep_provider_options"]
		switch {
		case hasTh && th <= 1 && fcPaused >= 10:
			add(vsConfig{Level: vsCrit, Variable: "wsrep_slave_threads",
				Current: fmt.Sprintf("%.0f — one thread applying everything the cluster produces", th),
				Suggest: "8, or the number of cores",
				Why:     fmt.Sprintf("Flow control had the writers paused %.0f%% of the time. Every node applies every write set, and with one applier a node that falls behind does not lag quietly the way an asynchronous replica does — it throttles the entire cluster down to its own speed.", fcPaused),
				Effect:  "Measured on this hardware: a 3-node PXC 8.0.46 cluster at the shipped defaults spent 99.9% of the capture paused by flow control and delivered 31 TPS. With 8 appliers, a 2 GiB gcache and gcs.fc_limit=500 — durability relaxed in the same step — it spent 0.0% paused and delivered 2,032 TPS at p99 31 ms."})
		case hasTh && th <= 1:
			add(vsConfig{Level: vsWarn, Variable: "wsrep_slave_threads",
				Current: fmt.Sprintf("%.0f", th),
				Suggest: "8, before the write rate grows into it",
				Why:     "One applier thread per node. It is keeping up now; when it stops keeping up the cost is not lag on one replica but flow control on every writer in the cluster."})
		case fcPaused >= 20:
			add(vsConfig{Level: vsWarn, Variable: "gcs.fc_limit (wsrep_provider_options)",
				Current: vsProvOpt(prov, "gcs.fc_limit"),
				Suggest: "500 on a cluster whose appliers are already parallel",
				Why:     fmt.Sprintf("Flow control paused the writers %.0f%% of the time with a peak receive queue of %.0f. A larger limit lets a node buffer more before it stops everybody else, which is the right trade when the applier is briefly rather than permanently behind.", fcPaused, recvQ),
				Risk:    "A bigger buffer means a node can be further behind before the cluster notices, so reads on a lagging node are staler and a failover has more to catch up."})
		}
		// gcache is what a rejoining node reads to catch up without a full state
		// transfer. The default is 128 MiB, which on a busy cluster is minutes.
		if g := vsProvOpt(prov, "gcache.size"); g == "128M" || g == "128MB" {
			add(vsConfig{Level: vsWarn, Variable: "gcache.size (wsrep_provider_options)",
				Current: g,
				Suggest: "2G, or enough to cover the longest restart you expect",
				Why:     "The gcache is the window a restarted node can rejoin through with an incremental transfer. Once it has wrapped, the node needs a full state transfer instead — which copies the entire dataset from a donor that is serving traffic while it does.",
				Effect:  "Measured on this hardware: raising it to 2 GiB alongside 8 appliers and gcs.fc_limit=500 took flow-control pause from 18.8% to 0.0% and p99 from 87 ms to 31 ms."})
		}
	}
	return out
}

// vsFCPaused is the share of the capture Galera had the writers paused, or 0 elsewhere.
func vsFCPaused(m *vsModel) float64 {
	if s := m.Series["galera"]; s != nil {
		return seriesMedian(s, "flowControlPausedPct")
	}
	return 0
}

// vsDurabilityWhy explains the fsync pair differently on a cluster, where a LOW fsync rate
// alongside heavy flow control is the symptom rather than the absence of one: the commits
// are not happening, which is exactly what is wrong.
func vsDurabilityWhy(m *vsModel, galera bool, data, log float64) string {
	if fc := vsFCPaused(m); galera && fc >= 10 {
		return fmt.Sprintf("Every commit costs an fsync for the redo log and another for the binary log, and on a Galera cluster it costs them while the other nodes wait: flow control had the writers paused %.0f%% of the time at only %s fsyncs/s. The low rate is the symptom — the commits are not getting through.", fc, compactNum(data+log))
	}
	return fmt.Sprintf("%s fsyncs/s (%s data, %s log). This is the largest single lever in this file and the only one that gives something up.", compactNum(data+log), compactNum(data), compactNum(log))
}

// vsProvOpt reads one setting out of wsrep_provider_options, which is one long
// semicolon-separated string rather than a set of variables.
func vsProvOpt(prov, key string) string {
	for _, part := range strings.Split(prov, ";") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// vsDur prints a number of seconds the way somebody reads a lag figure.
func vsDur(sec float64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%.1f h", sec/3600)
	case sec >= 90:
		return fmt.Sprintf("%.0f min", sec/60)
	default:
		return fmt.Sprintf("%.0f s", sec)
	}
}

// vsPoolTarget is where to start with a buffer pool: half the machine, which leaves room
// for the connection buffers, the operating system, and anything else sharing the host.
func vsPoolTarget(ram float64) float64 {
	switch {
	case ram <= 2<<30:
		return ram / 4
	case ram <= 8<<30:
		return ram / 2
	default:
		return ram * 0.55
	}
}

// vsDiskTotal sums one column across every device in the disk table, at its median.
func vsDiskTotal(m *vsModel, col string) float64 {
	if m.Disk == nil {
		return 0
	}
	// Every device gets a tab; "Overall" is not always present, so sum the tabs and
	// fall back to the overall series when a capture has only that.
	var total float64
	for _, s := range m.Disk.Tabs {
		total += seriesMedian(s, col)
	}
	if total == 0 && m.Disk.Overall != nil {
		total = seriesMedian(m.Disk.Overall, col)
	}
	return total
}
