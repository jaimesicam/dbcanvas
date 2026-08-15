# FTDC Summary

Every `mongod` writes `diagnostic.data` in its dbPath. Once a second, with no configuration
and almost no cost, it captures `serverStatus`, `replSetGetStatus`, the WiredTiger
statistics and the host's CPU, memory and disk counters. It is the black box recorder, and
it is the only artefact that can answer *"what was the server doing at 04:12 last Tuesday"*
when nobody thought to be watching at the time.

It is also undocumented and awkward to read, which is presumably why so few people ever
open their own. This page decodes it and draws the thirty-odd charts that answer something.

---

## Getting data in

**From a running node.** Pick a MongoDB node and the whole `diagnostic.data` directory is
read out of the container and decoded. Nothing has to have been enabled beforehand — that
is the point of FTDC. Verified live against **6.0, 7.0 and 8.0** replica sets; see
[Across versions](#across-versions-60-70-and-80) for the metric paths that move between them.

**By upload.** The `metrics.*` files themselves (pick them all at once), or a `.tar.gz` of
the directory. Files are ordered by name before decoding, which is chronological because
`mongod` names each file after its first sample; `metrics.interim` — the file currently
being written — is always placed last. Several files decode into one continuous series.

A truncated final chunk is normal: `metrics.interim` is being written while it is copied.
Those chunks are counted and skipped rather than failing the parse, and the count is shown.

**Gaps are declared.** `mongod` writes FTDC only while it is running, so a hole in the sample
timeline is almost always a period when it was not — usually the most interesting thing in the
file. Every chart draws a straight line across such a gap, and a straight line reads as
"nothing changed" when it means "nothing was recorded", so the number, total and longest gap
are stated above the charts. The threshold is relative to the file's own median sample
spacing, because a deployment that samples every thirty seconds by configuration has no gaps
at all.

---

## The format

Established by decoding a real file from a live PSMDB 8.0.28-12 member, because the format
is not documented anywhere authoritative:

```
the file        a bare sequence of BSON documents, back to back, no header
each document   { _id: <date>, type: <0|1>, data: <binary> }
                  type 0 — metadata: build info, command line, host
                  type 1 — a metric CHUNK, which is where everything lives
chunk data      [uint32 LE uncompressed length][zlib stream]
the chunk       [reference document (BSON)]
                [uint32 LE metric count][uint32 LE delta count]
                [varint stream]
```

The reference document is one complete sample — the full `serverStatus` tree — and it
defines both the metric set and its order. Every metric is then a **column**: the varint
stream is column-major, all of metric 0's deltas, then all of metric 1's, each delta added
to that metric's running value. A metric that does not change costs almost nothing, which is
what makes a per-second capture of several thousand counters fit in a few hundred kilobytes.

The rest of the trick is the zero encoding: **a varint of 0 is not one zero**. It is
followed by a second varint saying how many *more* zeros follow. A gauge that sits still for
an hour is two bytes.

### Three ways to be silently wrong

All three produce output that looks like numbers, which is why the decoder is tested against
a real file rather than a synthetic one:

1. **The zero run length.** Read it as a value and every metric after it in the chunk is
   shifted.
2. **Column-major order.** Reading row-major gives one plausible-looking series per sample
   instead of per metric.
3. **A BSON timestamp is TWO metrics**, seconds then increment. Counting it as one shifts
   every subsequent column by one.

The guard against all three is `mongod`'s own metric count, written into each chunk header.
If the reference-document walk does not produce exactly that many metrics, the chunk is
refused rather than half-read — so a decoder that has drifted out of step shows up as
skipped chunks, not as quietly wrong numbers.

---

## The charts

A decoded file holds around four thousand distinct metrics per sample — 5,673 on the live
8.0 member this was built against. Almost none are worth a chart, and a page offering all of
them is a metric browser: useful to somebody who already knows what they are looking for, and
useless to somebody holding a directory they were sent. So the charts are chosen, grouped,
and each carries a plain-English note on what it is for plus an advisor on what *this*
capture's numbers say.

Thirty-odd of them is more than anybody reads in order, which is what the **shortlist at the
top** is for: every chart whose advisor came back amber or red, as jump links. Nothing is
hidden — the full page is still below, because "nothing flagged" is not "nothing happened"
and the reader is often looking for something no threshold knows about.

**Replication**

| chart | the question it answers |
| --- | --- |
| **Member state** | who was primary, and when did that change |
| **Members able to acknowledge a write** | could the set commit a majority write at all — see below |
| **Replication lag** | was a member behind, and by how much — **not in the server log at all** |
| **Majority commit point lag** | what a `w:majority` write and a `readConcern:majority` read were waiting for |
| **Time spent waiting for write concern** | replication lag expressed as milliseconds on the client's clock |
| **Oplog application** | was a lagging member unable to *apply* fast enough, or receiving nothing? Opposite fixes |
| **Oplog size** | could a member that went away still catch up, or does it need a resync |
| **Oplog fetched from the sync source** | is the oplog even reaching this member |
| **Sync source** | who was replicating from whom, and did a member keep changing its mind |
| **Elections called** | how many failovers, whether they were timeouts or handovers, and how many in-flight operations were killed |

**Work**

| chart | the question it answers |
| --- | --- |
| **Average operation latency** | how long reads, writes and commands actually took — the number an application feels |
| **Operations** | what the server was asked to do (a server doing nothing looks like a broken one without this) |
| **Commands by name** | *which* commands — and how much of the load is drivers and heartbeats rather than the application |
| **Documents examined per document returned** | is the work avoidable? Hundreds-to-one is a missing index |
| **Contention and avoidable work** | write conflicts retried invisibly, sorts with no index, operations queued on a lock |
| **Errors returned to clients** | is the application being told no? Nothing about this reaches the log at default verbosity |
| **Connections** | did a driver pool run away, and how close to the limit did it get |
| **Client network throughput** | is a query returning far more data than anybody needs |

**Storage engine**

| chart | the question it answers |
| --- | --- |
| **Execution tickets** | was WiredTiger itself the bottleneck |
| **Operations queued** | were operations waiting to get in |
| **WiredTiger cache** | how much cache was in use, and how much of it was dirty |
| **Cache pressure** | which side of WiredTiger's own eviction thresholds that put it on |
| **Eviction** | were application threads being conscripted to evict — the cause of "everything is slow and nothing is slow" |
| **Journal sync latency** | the floor under every durable write, and a property of the disk rather than the query |
| **Storage engine I/O** | is the engine reading from disk at all — the clearest statement that the cache is too small |
| **Checkpoint duration** | is the engine stalling periodically for no reason a query log explains |
| **History store** | is old-version retention eating the cache — why a cache fills on an idle server |
| **Memory** | resident against cache and heap — what gets a box OOM-killed |

**Host**

| chart | the question it answers |
| --- | --- |
| **CPU** | user, system and iowait as a share of the machine |
| **Resource pressure (PSI)** | how much time work spent *stalled* on cpu, io or memory — the best single answer to "was this the machine" |
| **Host memory** | what the kernel had left, which is what decides whether the OOM killer arrives |
| **Major faults and swapping** | memory that had to come from the disk while a thread waited |
| **Disk `<device>`** | per-device utilisation and service time, one chart per device that did any I/O |

### The four that came out of reading the namespace rather than the documentation

The first version of this page charted what anybody would think of: lag, cache, tickets, CPU.
Enumerating all 5,665 metrics in a real capture turned up four that are better than most of
them, and all four are invisible in the server log at any verbosity.

**Members able to acknowledge a write.** `replSetGetStatus.writableVotingMembersCount` looks
like this answer and is not — it comes from the replica-set *config*, so it reads `3`
throughout an outage in which two members are unreachable. The honest version has to be
counted per sample from each member's `health` and `state`: a member acknowledges a write only
if this node can reach it and it is carrying data. It is the chart for the failure everybody
meets eventually and nobody recognises — the primary is up, the log says nothing, and every
write hangs, because `w:majority` cannot be satisfied.

**Time spent waiting for write concern.** `getLastError.wtime.totalMillis` over
`.wtime.num` is the average time a `w>1` write spent waiting for *other members* after the
primary had finished its own work. An application reporting slow writes while every
server-side latency looks fine is usually waiting exactly here. `wtimeouts` alongside it is
worse than slow: the write was applied on the primary and then reported to the application as
failed, and it is not rolled back.

**Journal sync latency.** `log sync time duration (usecs)` over `log sync operations` is the
average WiredTiger journal `fsync`. Every `j:true` write and every majority write waits for
one, so it is the floor under durable write latency on that server — and it is a property of
the storage, which no query tuning moves.

**Eviction by application threads.** WiredTiger has threads whose job is eviction; when they
fall behind it makes the threads running user operations do the work instead, and those
operations pay for it out of their own latency. `application threads page write from cache to
disk count` against `pages written from cache` is that split, and it is one of the few places
on this page where a cause can be read off rather than inferred. The signature is everything
slowing down together with no individual operation looking guilty.

### Derived, not raw

Three of the most useful charts are not metrics at all — they are ratios of two cumulative
counters, and reading either counter on its own is meaningless:

| chart | Δnumerator / Δdenominator |
| --- | --- |
| operation latency | `opLatencies.<kind>.latency` / `opLatencies.<kind>.ops` |
| documents examined per returned | `metrics.queryExecutor.scannedObjects` / `metrics.document.returned` |
| ms per oplog batch | `metrics.repl.apply.batches.totalMillis` / `.batches.num` |
| write-concern wait | `metrics.getLastError.wtime.totalMillis` / `.wtime.num` |
| journal sync latency | `wiredTiger.log.log sync time duration (usecs)` / `log sync operations` |
| disk service time | `disks.<dev>.read_time_ms` / `disks.<dev>.reads` |

`opLatencies.reads.latency` is *total microseconds ever spent on reads*; charting it draws a
line going up and to the right for ever. An interval with no operations carries the previous
value forward rather than dropping to zero — a zero would draw a line to the floor and read
as "instant", when the truth is that nothing was measured.

### Across versions: 6.0, 7.0 and 8.0

The container format has not changed — the decoder reads all three with no skipped chunks —
but **five metric paths moved**, and every one of them fails *silently*. A chart built on a
key that is not there is not an error; it is an empty chart, which on somebody else's capture
is indistinguishable from a server that was doing nothing.

Each of these was found by decoding a real capture from each release and diffing the
namespaces, not by reading release notes — three of the five are not in any release note.

| what | 6.0 | 7.0 | 8.0 |
| --- | --- | --- | --- |
| execution tickets | `wiredTiger.concurrentTransactions.*` | ← | `queues.execution.*` |
| queued operations | `globalLock.currentQueue.*` | ← | `queues.execution.*.normalPriority.queueLength` |
| checkpoint timing | `wiredTiger.transaction.transaction checkpoint max time (msecs)` | ← | `wiredTiger.checkpoint.max time (msecs)` |
| oplog `collStats` | `local.oplog.rs.stats.size` | `…stats.storageStats.size` | ← |
| step-down kills | `metrics.repl.stateTransition.userOperationsKilled` | ← | `…totalOperationsKilled` |
| CPU core count | `systemMetrics.cpu.num_cpus` | `…num_cores_available_to_process` | ← |
| PSI pressure | `systemMetrics.pressure.*` | ← | also copied to `serverStatus.extra_info.pressure.*` |

Two are worth singling out.

**The checkpoint move is a WiredTiger change, not a MongoDB one.** WT-11171 added a top-level
`checkpoint` statistics category in WiredTiger 11.2, which shipped in MongoDB 7.1 — so 6.0
*and* 7.0 both keep the old `transaction checkpoint …` names under `transaction`.

**PSI is the one that moved the other way.** It arrived under `systemMetrics.pressure` in
6.0.8 and 7.0 (SERVER-45255); 8.0 additionally copies it into `serverStatus.extra_info`,
byte for byte identical. Reading only the newer location — which is what a page written
against an 8.0 server naturally does — left the pressure chart silently empty on both older
releases, on a chart this page calls the best single answer to *"was this the machine"*.

**The core count is the only one that produced a wrong chart rather than a missing one.**
Without `num_cpus`, the divisor falls back to 1, and the CPU chart on a twenty-core host
reported `[warn] iowait peaked at 61% of the machine`. The correct reading was 3%. A missing
chart is at least visibly missing; a warning generated entirely by arithmetic is worse than
no chart at all, and it is the reason the version fixtures exist.

Everything else is the member's **role**, not its version: `oplogApply`, `replNetwork` and
`writeConcern` do not build on a primary, because a primary applies no oplog, fetches nothing
from a sync source, and on a quiet set never serves a majority write.

### Two details that decide whether a chart works at all

**8.0 moved the tickets.** `wiredTiger.concurrentTransactions` became `queues.execution`.
Both are read, so the page works on either — a chart built on the documented-in-2019 name is
a chart that is silently always empty on a modern server. Every key on this page was read out
of a real file rather than out of documentation, for exactly that reason.

**Member names are not in FTDC.** Strings are not metrics, so `replSetGetStatus.members.0`
has a state, an optime and a ping but no name. The member-state chart says "member 0" and
marks which one is `self`; an index is honest and an invented name is not.

### Three deliberate choices in the maths

**Downsampling takes every nth point, and does not average.** These are gauges as often as
counters, and an averaged spike is a spike that did not happen. Taking every nth can miss
one; averaging *invents a shape*, which is worse in a diagnostic tool.

**Rates use the real sample spacing.** FTDC slows its own sampling under load, so assuming
one second would overstate every rate exactly when it matters most. A counter that goes
backwards — a restart — yields zero rather than a large negative spike that would invent an
event.

**A device earns a chart by doing something.** `io_time_ms` is cumulative, so a disk that was
busy once at boot has a non-zero value for ever. Filtering on presence rather than on activity
produced four charts of flat zero on a host with one working disk; the filter is the *rate*,
and devices are ordered busiest-first.

**Lag is measured against the freshest member, not against the primary.** During a failover
there may be no primary at all, and a lag chart that goes blank exactly when the incident
happens is the wrong chart.

**Shares are taken over the window, not peak against peak.** The eviction advisor divides
pages evicted by application threads by pages evicted in total. Dividing the two *peaks* is
the obvious way and it is wrong: they are rarely the same interval, so the ratio describes a
moment that never happened. The advisor also refuses to fire on share alone — on a near-idle
server almost all of the little eviction there is happens on application threads, because the
eviction threads have nothing to wake up for, and 90% of half a page a second is not a
finding.

**A device utilisation over 100% is not a decode error.** `/proc/diskstats` accumulates busy
time per queue, so a multi-queue NVMe or a virtio device under a hypervisor can report more
busy milliseconds than the wall clock had — `iostat` shows the same. "317% busy" reads as a
broken chart, so past 100% the advisor says *saturated* instead, which is the only thing the
number still means.

---

## What it is for, in one example

The Log Summary's MongoDB verdict ends with a note saying that replication lag is not in the
server log — because it is not, in any form. Reading the same replica set's
`diagnostic.data` through this page over the same window puts thirteen of its thirty-three
charts on the shortlist, and these are the first four:

```
crit  Members able to acknowledge a write  The set could not acknowledge a majority write for 30.0s
crit  Replication lag                      A member was 61.0s behind at its worst
crit  Time spent waiting for write concern Writes waited an average of 13775 ms for other members
warn  Sync source                          This member failed to find a sync source 47 time(s)
```

That is one incident told four ways: the set lost its write majority, a member fell a minute
behind, writes waited fourteen seconds each on the client's clock, and the member could not
find anywhere to replicate from. None of the four appears in the server log in any form, and
none of them can be inferred from the others. It is the same incident the Log Summary reads
from the other side, from the artefact that actually recorded it.
