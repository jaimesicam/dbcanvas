# FTDC Summary

Every `mongod` and `mongos` writes FTDC (full-time diagnostic data capture) to a
`diagnostic.data` directory. About once a second it records `serverStatus`,
`replSetGetStatus`, WiredTiger statistics and the host's CPU, memory and disk counters, with
no setup needed. FTDC Summary decodes that directory and charts it, so you can see what the
server was doing at any moment in the recorded period. Most of these numbers, including
replication lag, never appear in the server log.

Open it from the sidebar (**FTDC Summary**).

![FTDC Summary: a decoded capture, with the charts worth looking at called out](screenshots/ftdc-summary.png)

> [!NOTE]
> A stack can also include a **Big Hole** node, a separate browser-based FTDC viewer (see
> [Stacks](STACKS.md)). FTDC Summary gives you findings and selected charts. Big Hole plots
> every metric so you can explore them yourself. Both read the same files.

## Load a capture

Use any of these:

- **From a running node.** Pick a MongoDB node under **Read from a running node** and click
  **Read diagnostic.data**. Every MongoDB process is listed, including shard members,
  config servers and mongos routers. On a mongos the directory sits next to its log
  (`mongos.log` → `mongos.diagnostic.data`), and both locations are checked.
- **Compare members.** Under **…or compare several members over the same window**, select
  2 to 4 nodes and click **Compare members**. Their charts are overlaid, so you can see the
  set from every member's point of view at once.
- **Upload.** Choose the `metrics.*` files, a `.tar.gz` or `.zip` of the directory, or the
  whole `diagnostic.data` folder, then click **Chart N file(s)**. When you pick a folder,
  files that are not `metrics.*` or archives are ignored.

To keep the files instead, right-click a MongoDB node in the stack designer and choose
**Download diagnostic.data with mongod.log** (or **mongos.log** on a router). You get both
in one `.tar.gz`.

Notes on uploads:

- Upload the **whole directory**. On a recently started `mongod`, most of the data is still
  in `metrics.interim`, and the numbered file may hold no samples yet.
- A partly written last chunk is normal. Chunks that cannot be decoded are skipped and
  counted rather than failing the whole read.

## Read the page

From top to bottom:

1. **Window.** The page starts on the whole capture. Drag across any chart to zoom in.
   The zoomed range is read again from the source at full resolution. **last 15 min** and
   **last hour** are presets, and **Back** and **Whole capture** undo a zoom.
2. **Summary.** Host, version, role (replica-set member, config server or router), replica
   set, time window, span, samples and metric count. **Capture header** expands to show
   what the server reported about itself. Gaps in the timeline are stated here. A chart
   draws a straight line across a gap, but nothing was recorded there, which usually means
   the server was not running.
3. **What stands out.** Every chart whose advisor raised a warning (**worth a look**) or a
   critical finding (**needs attention**), as jump links. The full set of charts is still
   below.
4. **Configuration.** Settings to change, or to keep, based on this capture's numbers.
   Each one shows **Now** and **Change to** where they apply, and the reason. Settings
   covered include `cacheSizeGB`, eviction threads, the execution ticket count,
   `journal.commitIntervalMs`, oplog size against cache size, `blockCompressor`,
   transparent huge pages, `ulimit -n` and CPU allocation.
5. **Charts**, grouped as below. Each chart has a note on what it measures and an advisor
   that reads this capture's values.

A chart appears only when the capture contains its metrics. On a mongos, the replication
and storage-engine charts are missing because a router has neither. On a primary, the
oplog-application charts are missing because a primary does not apply oplog.

## Charts

**Replication**

| Chart | What it shows |
| --- | --- |
| Replica-set member state | which member was primary, and when that changed |
| Members able to acknowledge a write | whether a `w:majority` write could be acknowledged at all |
| Replication lag | how far behind each member was, measured against the most up-to-date member |
| Majority commit point lag | what majority writes and reads were waiting for |
| Time spent waiting for write concern | replication lag as the client experiences it, in ms |
| Oplog application | whether a lagging member was slow to apply the oplog or was not receiving it |
| Oplog fetched from the sync source | whether the oplog is reaching this member |
| Oplog size · Oplog window | whether a member that falls behind can still catch up |
| Sync source | which member replicated from which, and how often that changed |
| Elections called | failovers, why they happened, and operations killed during step-down |
| Catch-up after an election | how a new primary caught up |
| Flow control | whether the primary was slowing writes to let secondaries keep up |

**Work**

| Chart | What it shows |
| --- | --- |
| Average operation latency | read, write and command latency |
| Operations · Commands by name | the load, and which commands made it up |
| Documents examined per document returned | missing indexes (hundreds to one) |
| Contention and avoidable work | write conflicts, sorts without an index, lock queueing |
| Time operations spent waiting | where operations spent their time waiting |
| Errors returned to clients | errors that are not logged at the default verbosity |
| Connections | connection pool growth, and how close it came to the limit |
| Client network throughput | how much data queries returned |
| Reads by preference and where they ran | read preference in practice |
| Session and retryable-write storage | session and retryable-write state |

**Storage engine**

| Chart | What it shows |
| --- | --- |
| Execution tickets available · Time waiting for an execution ticket · Operations queued | whether operations queued to get into WiredTiger |
| WiredTiger cache · Cache pressure against WiredTiger's thresholds | cache use, dirty data and eviction thresholds |
| Eviction | whether application threads were forced to evict pages |
| Journal sync latency | the minimum latency for every durable write, set by the disk |
| Storage engine I/O | disk reads, a sign that the cache is too small |
| Checkpoint duration | periodic checkpoint stalls |
| History store · The oplog's share of the cache | what was filling the cache |
| Memory · Allocator heap against memory in use | resident memory against cache and heap |
| Index build: the external sort | how much an index build spilled to disk |
| Data handles and the catalogue | open data handles |

**Host**

| Chart | What it shows |
| --- | --- |
| CPU | user, system and iowait as a share of the machine |
| Resource pressure (PSI) · Pressure on the mongod process | time stalled on CPU, I/O or memory |
| Host memory · Major faults and swapping | memory left, and paging |
| Disk `<device>` | utilisation and service time, one chart for each active device, busiest first |
| Free space | disk space |
| TCP trouble | retransmissions and other TCP errors |
| How long FTDC took to collect a sample | collection stalls, themselves a sign of a stalled host |

**Sharding**

| Chart | Where | What it shows |
| --- | --- | --- |
| Operations by how many shards they touched | mongos | queries broadcast to every shard because they lack the shard key |
| Round-trip time to each shard member · Router connections in use, by shard member | mongos | which shard member is slow, named by host |
| Shard operations by how long they took | mongos | latency of routed operations |
| Router connections to the shards | mongos | connection pool churn |
| Routing table refreshes | all | routers struggling to keep up with chunk moves |
| Chunk migrations · Writes blocked for migration commit · Orphan cleanup after migrations | shard | balancer activity and its effect on writes |

## Reading the numbers

- **Utilisation over 100%** on a multi-queue or virtual disk is real. The advisor reports
  it as **saturated**.
- **Rates use the actual sample spacing.** A counter that goes backwards, for example after
  a restart, counts as zero.
- **Long captures are downsampled** by keeping every nth point, not by averaging, so short
  spikes are not smoothed into shapes that never happened. Zoom in to see full resolution.
- **Members are numbered, not named.** FTDC records no member names, so charts say
  "member 0" and mark `self`. A mongos capture is the exception: it names every shard member
  by host.
- **Version differences are handled.** Metrics that MongoDB moved between releases, and the
  role-grouped layout a sharded cluster uses in 8.0 and later, are read from wherever the
  capture keeps them.

---

See also: [Log Summary](LOG_SUMMARY.md) · [Packet Inspector](PACKET_INSPECTOR.md) · [Stalk Summary](STALK_SUMMARY.md)
