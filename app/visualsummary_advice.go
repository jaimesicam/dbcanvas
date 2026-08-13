package main

import (
	"fmt"
	"strings"
)

// Per-chart advisors.
//
// The Verdicts card at the top of the report answers "what is wrong with this
// server". It cannot answer the question a reader has while looking at any
// individual chart, which is "what am I looking at, is this number good, and
// what would I change". A correct line on a graph is not self-explanatory:
// nobody arrives knowing that a full buffer pool is normal, that a checkpoint
// age of 11 MB is either fine or nearly fatal depending on a setting three
// panels away, or that "disk reads" from InnoDB may never have touched a disk.
//
// So each chart gets an advisor: what the chart measures, what *this* capture's
// numbers say, and what to change if they say something bad. They share the
// vsVerdict shape and levels with the top-level rules, and they read the same
// already-parsed series — an advisor never re-derives anything.
//
// Every one of them returns nil when its data is absent, so a capture missing a
// file simply has no advice for it rather than advice built on zeroes.

// advisorRules maps a chart key to the rule explaining it. The key is what the
// frontend looks the advisor up by, next to the chart it belongs to.
var advisorRules = map[string]func(*vsModel) *vsVerdict{
	"cpu":                adviseCPU,
	"memory":             adviseMemory,
	"swap":               adviseSwap,
	"disk":               adviseDisk,
	"bufferPoolPages":    adviseBufferPoolPages,
	"bufferPoolReads":    adviseBufferPoolReads,
	"innodbIO":           adviseInnodbIO,
	"fsyncs":             adviseFsyncs,
	"handlerReadRndNext": adviseScans,
	"historyList":        adviseHistoryList,
	"checkpointAge":      adviseCheckpointAge,
	"rowLockWaits":       adviseRowLockWaits,
	"threads":            adviseThreads,
	"qps":                adviseQPS,
	"innodbRowOps":       adviseRowOps,
	"tmpDiskTables":      adviseTmpDiskTables,
	"slowQueries":        adviseSlowQueries,
	"abortedConns":       adviseAbortedConns,
	"networkThroughput":  adviseNetworkThroughput,
	"sockQueues":         adviseSockQueues,
	"replicationLag":     adviseReplicationLag,
	"galera":             adviseGalera,
	"innodbTrx":          adviseOldestTransaction,
}

func computeAdvisors(m *vsModel) {
	m.Advisors = map[string]vsVerdict{}
	for key, rule := range advisorRules {
		if v := rule(m); v != nil {
			v.ID = key
			m.Advisors[key] = *v
		}
	}
}

// advice is the shared constructor: every advisor says what the chart measures
// before it says anything about this capture, because the explanation is half
// the point.
func advice(level, means, found, todo string) *vsVerdict {
	v := &vsVerdict{Level: level, Headline: found, Detail: means, Means: means, Action: todo}
	if todo != "" {
		v.Detail += " " + todo
	}
	return v
}

// ---- operating system ----

func adviseCPU(m *vsModel) *vsVerdict {
	if m.CPU == nil || m.CPU.Overall == nil {
		return nil
	}
	s := m.CPU.Overall
	usr, sys := seriesMedian(s, "usr"), seriesMedian(s, "sys")
	iowait, steal := seriesMedian(s, "iowait"), seriesMedian(s, "steal")
	busy := 100 - seriesMedian(s, "idle")
	means := "Where processor time went. `user` is query execution, `system` is kernel work " +
		"(I/O submission, network, context switching), `iowait` is time idle *because* it was " +
		"waiting for storage, and `steal` is time a hypervisor gave to somebody else."
	found := fmt.Sprintf("%.0f%% busy — user %.0f%%, system %.0f%%, iowait %.0f%%", busy, usr, sys, iowait)

	switch {
	case steal >= 5:
		return advice(vsWarn, means, found+fmt.Sprintf(", steal %.0f%%", steal),
			"Steal time means this guest is not getting the CPU it asked for; nothing tuned "+
				"inside the database will fix that. Take it up with the hypervisor or move the instance.")
	case iowait >= 20:
		return advice(vsWarn, means, found,
			"A fifth of the time is spent waiting on storage rather than working. Look at the "+
				"buffer pool and disk panels: the fix is almost always to ask the disks for less, "+
				"not to add CPU.")
	case busy >= 85:
		return advice(vsWarn, means, found,
			"Close to the ceiling, and with little iowait this is genuine work. Check the "+
				"statement table for queries examining far more rows than they return — that is "+
				"the usual source of CPU nobody meant to spend.")
	case sys > usr && sys >= 20:
		return advice(vsInfo, means, found,
			"More time in the kernel than in query execution, which usually means very many small "+
				"queries or connections rather than expensive ones. Connection pooling and batching "+
				"help here where query tuning will not.")
	}
	return advice(vsOK, means, found,
		"Comfortable, with little time lost to waiting. High CPU is not by itself a fault — "+
			"a busy server delivering throughput is what you want.")
}

func adviseMemory(m *vsModel) *vsVerdict {
	s := m.Series["memory"]
	if s == nil {
		return nil
	}
	used, cache := seriesMedian(s, "used"), seriesMedian(s, "cache")
	free := seriesMedian(s, "free")
	total := used + cache + seriesMedian(s, "buff") + free
	means := "How the machine's memory is spent. `cache` is the operating system's page cache, " +
		"which holds file data InnoDB has read — memory that is available to be reclaimed, and " +
		"also the reason a buffer pool miss may cost nothing."
	found := fmt.Sprintf("%.0f MB used, %.0f MB cache, %.0f MB free", used, cache, free)
	if total > 0 && free/total < 0.02 && cache/total < 0.05 {
		return advice(vsWarn, means, found,
			"Very little free and very little reclaimable cache: this machine is genuinely out "+
				"of memory, and the next allocation is a swap or an OOM kill. Reduce "+
				"innodb_buffer_pool_size or move something off this host.")
	}
	return advice(vsOK, means, found,
		"There is memory to reclaim here. Note that a large page cache is what lets InnoDB "+
			"read misses cost far less than the buffer pool miss ratio suggests — see the "+
			"InnoDB vs device I/O panel.")
}

func adviseSwap(m *vsModel) *vsVerdict {
	s := m.Series["swap"]
	if s == nil {
		return nil
	}
	used := seriesMedian(s, "used")
	in, out := seriesMedian(s, "in"), seriesMedian(s, "out")
	means := "Swap in use, and pages moving to and from it. A database keeps its hot data in " +
		"memory on purpose; swapping that memory to disk defeats the entire design."
	found := fmt.Sprintf("%.0f MB in use, %.0f in / %.0f out", used, in, out)
	switch {
	case in > 0 || out > 0:
		return advice(vsCrit, means, found,
			"Pages are actively moving through swap while the database runs. This is the worst "+
				"thing on this page: latency will be wild and unpredictable. Cut "+
				"innodb_buffer_pool_size until it stops, and consider vm.swappiness=1.")
	case used > 0:
		return advice(vsWarn, means, found,
			"Something was swapped out earlier and has not come back. Not costing anything right "+
				"now, but it means memory was overcommitted at some point.")
	}
	return advice(vsOK, means, "none in use", "Nothing swapped, which is what you want.")
}

func adviseDisk(m *vsModel) *vsVerdict {
	if m.Disk == nil || m.Disk.Overall == nil {
		return nil
	}
	s := m.Disk.Overall
	util := seriesMedianSkipFirst(s, "util")
	rAwait, wAwait := seriesMedianSkipFirst(s, "rAwait"), seriesMedianSkipFirst(s, "wAwait")
	rkb, wkb := seriesMedianSkipFirst(s, "rKBs"), seriesMedianSkipFirst(s, "wKBs")
	means := "How hard the block devices were working. `util` is the share of time the device " +
		"had a request in flight, and `await` is how long each request took end to end. On SSDs " +
		"util saturates long before the device does, so latency is the more honest signal."
	found := fmt.Sprintf("util %.0f%% · read %.0f KB/s, write %.0f KB/s · await %.1f/%.1f ms",
		util, rkb, wkb, rAwait, wAwait)
	switch {
	case rAwait >= 20 || wAwait >= 20:
		return advice(vsCrit, means, found,
			"Requests are taking tens of milliseconds, which is a device at or past its limit. "+
				"Either reduce the I/O (a bigger buffer pool is usually the cheapest way) or "+
				"move to faster storage.")
	case util >= 80:
		return advice(vsWarn, means, found,
			"Busy nearly all the time. If latency is still low the device may have queue depth "+
				"to spare, but there is no headroom left for a burst.")
	}
	return advice(vsOK, means, found, "Storage has headroom during this capture.")
}

func adviseNetworkThroughput(m *vsModel) *vsVerdict {
	s := m.Series["networkThroughput"]
	if s == nil {
		return nil
	}
	in, out := seriesMedian(s, "received"), seriesMedian(s, "sent")
	means := "Bytes MySQL received from and sent to clients. Sustained high `sent` with modest " +
		"query rates means large result sets."
	found := fmt.Sprintf("%s/s in, %s/s out", humanBytes(in), humanBytes(out))
	if out > 50<<20 {
		return advice(vsWarn, means, found,
			"Tens of megabytes a second going back to clients. Look for SELECTs without a LIMIT, "+
				"or applications fetching columns they discard — this is bandwidth and latency "+
				"nobody is using.")
	}
	return advice(vsOK, means, found, "Result sets are a reasonable size for this query rate.")
}

func adviseSockQueues(m *vsModel) *vsVerdict {
	s := m.Series["sockQueues"]
	if s == nil {
		return nil
	}
	recv, send := seriesMedian(s, "recvBacklog"), seriesMedian(s, "sendBacklog")
	means := "Sockets with data sitting in a kernel queue. A receive backlog means MySQL is not " +
		"reading requests fast enough; a send backlog means clients are not reading results fast enough."
	found := fmt.Sprintf("%.0f recv-Q, %.0f send-Q", recv, send)
	if recv+send >= 5 {
		return advice(vsWarn, means, found,
			"Sustained backlog on several sockets. If it is receive-side, the server is saturated; "+
				"if send-side, the bottleneck is the client or the network, not the database.")
	}
	return advice(vsOK, means, found, "Nothing is queueing in the kernel.")
}

// ---- InnoDB ----

func adviseBufferPoolPages(m *vsModel) *vsVerdict {
	s := m.Series["bufferPool"]
	if s == nil {
		return nil
	}
	free, total := seriesMedian(s, "freePages"), seriesMedian(s, "totalPages")
	dirty, data := seriesMedian(s, "dirtyPages"), seriesMedian(s, "dataPages")
	means := "How the buffer pool's pages are used. `free` are pages never yet allocated, " +
		"`data` hold table and index pages, `dirty` hold changes not yet written to disk. " +
		"A pool that has filled is normal and expected — it is what a cache is for."
	found := fmt.Sprintf("%.0f data, %.0f dirty, %.0f free of %.0f", data, dirty, free, total)
	dirtyPct := 0.0
	if data > 0 {
		dirtyPct = dirty / data * 100
	}
	switch {
	case total > 0 && free/total*100 >= bufferPoolRoomyPct:
		return advice(vsOK, means, found,
			fmt.Sprintf("%.0f%% of the pool has never been allocated, so it is larger than this "+
				"workload needs. There is nothing to gain by raising it.", free/total*100))
	case dirtyPct >= 70:
		return advice(vsWarn, means, found,
			"Most of what the pool holds is unwritten changes. If flushing cannot keep up, "+
				"InnoDB will start forcing it and write latency will spike — check "+
				"innodb_io_capacity and innodb_max_dirty_pages_pct.")
	}
	return advice(vsOK, means, found,
		"The pool is full, which is the normal steady state. Whether it is big *enough* is a "+
			"question the read-miss panel answers, not this one.")
}

func adviseBufferPoolReads(m *vsModel) *vsVerdict {
	s := m.Series["bufferPool"]
	if s == nil {
		return nil
	}
	req, miss := seriesMedian(s, "readReqPerSec"), seriesMedian(s, "diskReadPerSec")
	ratio := seriesMedian(s, "missRatioPct")
	means := "Logical reads against the pool, and how many of them did not find their page in " +
		"it. The second number is `Innodb_buffer_pool_reads`, which counts *misses* — despite " +
		"its reputation it does not tell you anything went to a disk."
	found := fmt.Sprintf("%s requests/s, %s misses/s (%.2f%%)", compactNum(req), compactNum(miss), ratio)
	switch {
	case ratio >= 5:
		return advice(vsCrit, means, found,
			"The working set is much larger than the pool. Raise innodb_buffer_pool_size if the "+
				"memory exists — but check the next panel first, because if the page cache is "+
				"absorbing these misses the win will be smaller than the ratio suggests.")
	case ratio >= 1:
		return advice(vsWarn, means, found,
			"A steady minority of reads misses. Worth raising innodb_buffer_pool_size if there "+
				"is spare memory; worth reducing rows examined either way.")
	}
	return advice(vsOK, means, found,
		"Essentially everything is served from memory. Buffer pool size is not limiting this "+
			"workload — and note that a ratio this low can also mean nothing is reading the "+
			"data at all.")
}

func adviseInnodbIO(m *vsModel) *vsVerdict {
	io := m.Series["innodbIO"]
	if io == nil {
		return nil
	}
	innodb := seriesMedian(io, "read") / (1 << 20)
	written := seriesMedian(io, "written") / (1 << 20)
	device := 0.0
	if m.Disk != nil && m.Disk.Overall != nil {
		device = seriesMedianSkipFirst(m.Disk.Overall, "rKBs") / 1024
	}
	means := "InnoDB's own I/O counters beside what the block devices actually served. These " +
		"two should agree; when they do not, the operating system's page cache is answering " +
		"reads that InnoDB believes it made to storage."
	found := fmt.Sprintf("InnoDB reads %.0f MiB/s, writes %.0f MiB/s · devices served %.0f MiB/s",
		innodb, written, device)
	method := m.vars["innodb_flush_method"]
	if innodb >= 1 && device < innodb*0.75 {
		todo := "Treat the buffer pool miss ratio as a sizing signal, not a cost: these misses " +
			"are being served from RAM. The same ratio on a machine with less free memory would " +
			"be far more expensive."
		if method != "" && !strings.EqualFold(method, "O_DIRECT") {
			todo += fmt.Sprintf(" innodb_flush_method=%s is what puts the page cache in the path; "+
				"under O_DIRECT these two numbers converge and the miss ratio starts costing "+
				"what it appears to.", method)
		}
		return advice(vsWarn, means, found, todo)
	}
	return advice(vsOK, means, found,
		"The counters agree, so buffer pool misses here really are disk I/O and the miss ratio "+
			"can be read at face value.")
}

func adviseFsyncs(m *vsModel) *vsVerdict {
	s := m.Series["fsyncs"]
	if s == nil {
		return nil
	}
	data, log := seriesMedian(s, "data"), seriesMedian(s, "log")
	means := "Durability calls per second. Every commit that must survive a power cut costs an " +
		"fsync, and how many depends on sync_binlog and innodb_flush_log_at_trx_commit."
	sb, hasSB := m.varNum("sync_binlog")
	ft, hasFT := m.varNum("innodb_flush_log_at_trx_commit")
	found := fmt.Sprintf("%s data fsyncs/s, %s log fsyncs/s", compactNum(data), compactNum(log))
	if hasSB && hasFT {
		found += fmt.Sprintf(" · sync_binlog=%.0f, innodb_flush_log_at_trx_commit=%.0f", sb, ft)
	}
	if data+log >= 300 {
		todo := "A high fsync rate is the price of per-commit durability. If this workload can " +
			"tolerate losing the last second of commits on a power cut — a test rig, a replica, " +
			"a rebuildable dataset — sync_binlog=0 and innodb_flush_log_at_trx_commit=2 will cut " +
			"it sharply. On anything holding data you cannot lose, leave both at 1 and buy the " +
			"throughput elsewhere. Batching commits reduces fsyncs without weakening durability at all."
		return advice(vsInfo, means, found, todo)
	}
	if hasSB && hasFT && sb == 0 && ft != 1 {
		return advice(vsWarn, means, found,
			"Both durability settings are relaxed, so a power cut or a crash of the host can "+
				"lose recent commits and leave the binlog behind the data. Correct for a lab, "+
				"wrong for anything you would miss.")
	}
	return advice(vsOK, means, found, "Durability is not costing much at this commit rate.")
}

func adviseScans(m *vsModel) *vsVerdict {
	s := m.Series["handlerReadRndNext"]
	if s == nil {
		return nil
	}
	rows := seriesMedian(s, "perSec")
	means := "Rows read by walking a table in order rather than by looking them up — " +
		"`Handler_read_rnd_next`. It counts full *table* scans only, so an expensive full " +
		"*index* scan does not appear here at all."
	found := fmt.Sprintf("%s rows/s", compactNum(rows))
	todo := "Cross-check against the statement table below, which is sorted by rows examined " +
		"and catches both kinds."
	if rows >= scanRowsPerSecWarn {
		return advice(vsWarn, means, found,
			"Tables are being walked repeatedly. Once the rows are cached this costs CPU rather "+
				"than I/O, so it hides behind a healthy buffer pool while still being the most "+
				"expensive thing the server does. "+todo)
	}
	return advice(vsOK, means, found, "Little unindexed scanning. "+todo)
}

func adviseHistoryList(m *vsModel) *vsVerdict {
	s := m.Series["historyList"]
	if s == nil {
		return nil
	}
	max := seriesMax(s, "value")
	means := "Old row versions InnoDB is keeping so that open transactions still see a " +
		"consistent snapshot. Purge removes them once no transaction needs them."
	found := fmt.Sprintf("peaked at %s undo records", compactNum(max))
	switch {
	case max >= 1e7:
		return advice(vsCrit, means, found,
			"Purge is badly behind, which usually means a very long-running transaction is "+
				"holding a snapshot open. Find it in the transaction table below and end it; "+
				"until then the undo log keeps growing and reads get slower.")
	case max >= 1e6:
		return advice(vsWarn, means, found,
			"Purge is falling behind. Look for long transactions — a session left idle inside "+
				"a transaction is the usual cause — or raise innodb_purge_threads.")
	}
	return advice(vsOK, means, found, "Purge is keeping up.")
}

func adviseCheckpointAge(m *vsModel) *vsVerdict {
	s := m.Series["checkpointAge"]
	if s == nil {
		return nil
	}
	age := seriesMax(s, "age")
	means := "How much redo has been written since the last checkpoint. What matters is not the " +
		"byte count but its share of the redo log, because that is what decides how close " +
		"InnoDB is to having to flush synchronously."
	capacity, ok := m.redoCapacityBytes()
	if !ok || capacity <= 0 {
		return advice(vsInfo, means, humanBytes(age)+" at peak",
			"The redo log size is not in this capture, so this cannot be judged. It is "+
				"innodb_redo_log_capacity on 8.0.30 and later.")
	}
	pct := age / capacity * 100
	found := fmt.Sprintf("%.1f%% of %s at peak", pct, humanBytes(capacity))
	switch {
	case pct >= 75:
		return advice(vsCrit, means, found,
			"Close to the point where InnoDB forces synchronous flushing and write throughput "+
				"collapses. Raise innodb_redo_log_capacity — it costs only disk space, and a "+
				"larger redo log lets writes be absorbed and flushed in a smoother pattern.")
	case pct >= 50:
		return advice(vsWarn, means, found,
			"Over half consumed at peak, which leaves little room for a burst. Raising "+
				"innodb_redo_log_capacity is cheap insurance.")
	}
	return advice(vsOK, means, found, "Plenty of headroom for this write rate.")
}

func adviseRowLockWaits(m *vsModel) *vsVerdict {
	s := m.Series["rowLockWaits"]
	if s == nil {
		return nil
	}
	waits := seriesMedian(s, "perSec")
	means := "How often a statement had to wait for a row lock another transaction held. " +
		"Sustained waits are the precursor to deadlocks and to unpredictable latency."
	found := fmt.Sprintf("%s waits/s", compactNum(waits))
	if waits >= 1 {
		return advice(vsWarn, means, found,
			"Transactions are contending for the same rows. Shorten them — do the reads before "+
				"opening the transaction, commit as soon as the write is done — and make sure "+
				"the statements involved use an index, since a scan takes far more locks than "+
				"it needs.")
	}
	return advice(vsOK, means, found, "Almost no contention.")
}

func adviseThreads(m *vsModel) *vsVerdict {
	s := m.Series["threads"]
	if s == nil {
		return nil
	}
	running, connected := seriesMedian(s, "running"), seriesMedian(s, "connected")
	means := "`connected` is how many clients hold a connection; `running` is how many are " +
		"actually executing at this instant. Running is the one that reflects load."
	found := fmt.Sprintf("%.0f running of %.0f connected", running, connected)
	if running >= 32 {
		return advice(vsWarn, means, found,
			"Many statements executing at once, which past a point makes everything slower "+
				"rather than faster as they contend for the same latches. A connection pool "+
				"that caps concurrency usually raises total throughput here.")
	}
	if connected > 0 && running/connected < 0.05 && connected >= 100 {
		return advice(vsInfo, means, found,
			"Many connections, almost none doing anything. Idle connections still cost memory "+
				"and a thread each — a pool would use far fewer.")
	}
	return advice(vsOK, means, found, "Concurrency is healthy.")
}

func adviseQPS(m *vsModel) *vsVerdict {
	s := m.Series["qps"]
	if s == nil {
		return nil
	}
	q := seriesMedian(s, "questions")
	sel, ins := seriesMedian(s, "select"), seriesMedian(s, "insert")
	upd, del := seriesMedian(s, "update"), seriesMedian(s, "delete")
	means := "Statements per second and their mix. This is the number to compare between two " +
		"captures — every other panel explains it, but this is what actually got delivered."
	found := fmt.Sprintf("%s queries/s — %s select, %s insert, %s update, %s delete",
		compactNum(q), compactNum(sel), compactNum(ins), compactNum(upd), compactNum(del))
	return advice(vsInfo, means, found,
		"On its own this is neither good nor bad. Pin this capture as a baseline and load "+
			"another to see whether a change actually helped.")
}

func adviseRowOps(m *vsModel) *vsVerdict {
	s := m.Series["innodbRowOps"]
	if s == nil {
		return nil
	}
	read := seriesMedian(s, "read")
	written := seriesMedian(s, "inserted") + seriesMedian(s, "updated") + seriesMedian(s, "deleted")
	means := "Rows InnoDB touched, as against statements executed. The ratio between rows read " +
		"and rows returned to clients is the single best measure of wasted work."
	found := fmt.Sprintf("%s rows read/s, %s rows written/s", compactNum(read), compactNum(written))
	if q := m.Series["qps"]; q != nil {
		if qps := seriesMedian(q, "questions"); qps > 0 && read/qps > 1000 {
			return advice(vsWarn, means, found,
				fmt.Sprintf("That is about %s rows read for every statement, which means most "+
					"statements are examining far more than they can plausibly return. The "+
					"statement table below names the worst; the fix is nearly always an index.",
					compactNum(read/qps)))
		}
	}
	return advice(vsOK, means, found, "Rows touched are in proportion to the work requested.")
}

func adviseTmpDiskTables(m *vsModel) *vsVerdict {
	s := m.Series["tmpDiskTables"]
	if s == nil {
		return nil
	}
	n := seriesMedian(s, "perSec")
	means := "Temporary tables that spilled to disk because they outgrew the in-memory limit. " +
		"Caused by sorts, GROUP BY, DISTINCT and UNION over more rows than will fit."
	found := fmt.Sprintf("%s/s", compactNum(n))
	if n >= 1 {
		return advice(vsWarn, means, found,
			"Queries are spilling to disk. Raising tmp_table_size and max_heap_table_size helps "+
				"if the spills are marginal, but the real fix is usually an index that lets the "+
				"sort or grouping be satisfied in order, or returning fewer rows.")
	}
	return advice(vsOK, means, found, "Nothing meaningful spilling to disk.")
}

func adviseSlowQueries(m *vsModel) *vsVerdict {
	s := m.Series["slowQueries"]
	if s == nil {
		return nil
	}
	n := seriesMedian(s, "perSec")
	threshold := ""
	if v, ok := m.varNum("long_query_time"); ok {
		threshold = fmt.Sprintf(" (long_query_time=%.0fs)", v)
	}
	means := "Statements that ran longer than long_query_time. The threshold is a setting, so " +
		"zero here can mean a fast server or simply a generous limit."
	found := fmt.Sprintf("%s/s%s", compactNum(n), threshold)
	if n > 0 {
		return advice(vsWarn, means, found,
			"Queries are crossing the slow threshold. The slow log will name them; the statement "+
				"table below ranks them by rows examined, which is usually the same list.")
	}
	return advice(vsOK, means, found,
		"None logged. If that seems too good, check what long_query_time is set to.")
}

func adviseAbortedConns(m *vsModel) *vsVerdict {
	s := m.Series["abortedConns"]
	if s == nil {
		return nil
	}
	clients, connects := seriesMedian(s, "clients"), seriesMedian(s, "connects")
	means := "`Aborted_connects` is connection attempts that never authenticated; " +
		"`Aborted_clients` is established connections that vanished without closing cleanly."
	found := fmt.Sprintf("%s clients/s, %s connects/s", compactNum(clients), compactNum(connects))
	switch {
	case connects >= 1:
		return advice(vsWarn, means, found,
			"Connections are failing before they authenticate — wrong credentials, a missing "+
				"grant, TLS trouble, or max_connections reached. The error log will say which.")
	case clients >= 1:
		return advice(vsInfo, means, found,
			"Clients are disappearing without closing. Usually an application not closing "+
				"connections, or a timeout (wait_timeout, or something in between) cutting them.")
	}
	return advice(vsOK, means, found, "Connections are clean.")
}

func adviseReplicationLag(m *vsModel) *vsVerdict {
	s := m.Series["replicationLag"]
	if s == nil {
		return nil
	}
	max := seriesMax(s, "seconds")
	means := "How far behind the source this replica is."
	found := fmt.Sprintf("%.0fs at peak", max)
	switch {
	case max >= 30:
		return advice(vsCrit, means, found,
			"Far enough behind to matter for reads and for failover. Usually a single-threaded "+
				"apply against a parallel-writing source: raise replica_parallel_workers, and "+
				"check the replica is not short of I/O.")
	case max >= 1:
		return advice(vsWarn, means, found,
			"Measurable lag. Tolerable for many uses, but reads here are not current.")
	}
	return advice(vsOK, means, found, "Keeping up with the source.")
}

func adviseGalera(m *vsModel) *vsVerdict {
	s := m.Series["galera"]
	if s == nil {
		return nil
	}
	// The mean, not the median: flow control arrives in bursts, so a node paused
	// a third of the capture shows ~100% on a few samples and 0% on the rest.
	// The median of that is zero — which is how this advisor once reported
	// "keeping up with cluster writes" for a node paused 32.5% of the time.
	paused := seriesMean(s, "flowControlPausedPct")
	recv := seriesMax(s, "recvQueue")
	means := "Cluster replication health. `flow control paused` is the share of time this node " +
		"told the cluster to stop sending because it could not keep up — while paused, the " +
		"whole cluster's write throughput is limited by this node. Note it is reported per " +
		"node: a *writer* being paused means some other member is the slow one, and the queue " +
		"below will be flat here and deep on that member."
	found := fmt.Sprintf("%.1f%% paused, peak recv queue %.0f", paused, recv)
	switch {
	case paused >= 10:
		return advice(vsCrit, means, found,
			"This node is holding the cluster back for a tenth of the time or more. It is "+
				"usually the slowest disk in the cluster, or a node doing less apply work in "+
				"parallel — check wsrep_slave_threads and the storage on this member.")
	case paused >= 1:
		return advice(vsWarn, means, found,
			"Some flow control. Worth watching if writes grow.")
	}
	return advice(vsOK, means, found, "The node is keeping up with cluster writes.")
}

// adviseOldestTransaction names the transaction that is holding things up.
//
// This is the advisor adviseHistoryList used to substitute for by telling the
// reader to "find it in the transaction table below" — an instruction to do by
// hand what the capture already contains. It reads two sources, in order of how
// much they can prove:
//
//   - LockWaits, from pt-stalk's INNODB_TRX join, when a lock wait was in
//     progress. This is the strong case: the blocker's thread, its statement,
//     how many transactions it is holding up, and idle_in_trx — seconds the
//     blocking session has been idle *inside* its transaction, which is the
//     "idle in transaction" signal by name.
//   - InnodbTrx otherwise, from SHOW ENGINE INNODB STATUS, which gives an age
//     and a query but cannot say whether anyone is blocked or whether the
//     session is idle or working.
//
// The distinction matters for what to tell someone. A transaction blocking
// others is an outage in progress; a long transaction blocking nobody is a purge
// problem that will surface later as history list growth.
func adviseOldestTransaction(m *vsModel) *vsVerdict {
	if len(m.LockWaits) > 0 {
		w := m.LockWaits[0]
		wait, idle, waiters := num(w["waitSeconds"]), num(w["idleInTrx"]), num(w["waiters"])
		means := "The transaction other transactions are waiting behind. `idle in trx` is how " +
			"long the blocking session sat idle *inside* its transaction — a session that ran " +
			"a statement, never committed, and went quiet."
		where := ""
		if t := w["table"]; t != "" {
			where = " on " + t
		}
		found := fmt.Sprintf("thread %s blocked %.0f transaction(s) for %.0fs%s",
			w["blockingThread"], waiters, wait, where)
		if idle > 0 {
			found += fmt.Sprintf(", idle in trx %.0fs", idle)
		}
		blocking := w["blockingQuery"]
		if blocking == "" {
			blocking = "(no statement — the session is idle, holding locks it already took)"
		}
		switch {
		case idle >= 60:
			return advice(vsCrit, means, found, fmt.Sprintf(
				"A session has been idle inside a transaction for %.0fs while holding locks. "+
					"This is almost always an application that opened a transaction and then "+
					"did something slow — or nothing — without committing. Find thread %s and "+
					"end it, then look for the missing COMMIT. Blocking statement: %s",
				idle, w["blockingThread"], blocking))
		case wait >= 30 || waiters >= 5:
			return advice(vsCrit, means, found, fmt.Sprintf(
				"Transactions have been queued behind this one long enough for users to notice. "+
					"Blocking statement: %s. Shorten it, or take its locks in the same order "+
					"everywhere so waiters do not pile up.", blocking))
		case wait >= 5:
			return advice(vsWarn, means, found, fmt.Sprintf(
				"Measurable lock waiting. Blocking statement: %s", blocking))
		}
		return advice(vsOK, means, found, "Brief waits only — normal for a busy server.")
	}

	if len(m.InnodbTrx) == 0 {
		return nil
	}
	t := m.InnodbTrx[0]
	age := num(t["active"])
	means := "The longest-running transaction seen during the capture. Its age is bounded by " +
		"the capture window, so this says it ran for at least this long, not that it started " +
		"here. Nothing was waiting on it — that would appear as a lock wait instead."
	found := fmt.Sprintf("thread %s active %.0fs", t["thread"], age)
	if l := num(t["rowLocks"]); l > 0 {
		found += fmt.Sprintf(", %.0f row locks", l)
	}
	q := t["query"]
	if q == "" {
		q = "(no statement — idle inside the transaction)"
	}
	switch {
	case age >= 300:
		return advice(vsCrit, means, found, fmt.Sprintf(
			"Open for the whole capture and then some. Purge cannot remove any row version "+
				"newer than this transaction's snapshot, so undo keeps growing for as long as "+
				"it lives — check the history list above. Statement: %s", q))
	case age >= 60:
		return advice(vsWarn, means, found, fmt.Sprintf(
			"Long enough to hold back purge. Statement: %s", q))
	}
	return advice(vsOK, means, found, "No long-running transaction in this capture.")
}
