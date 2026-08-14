# Log Summary

The **Log Summary** reads several database servers' logs **together**, on one timeline,
and separates the good, the warning and the bad. It is for the question three log files
are opened to answer and none of them answers alone:

> *What state was the cluster in at 01:49:35, and which node is telling the truth about it?*

Open it from the sidebar (**Log Summary**) or at `#log-summary`.

It is the other half of the [Packet Inspector](PACKET_INSPECTOR.md). That one answers
"what crossed the wire"; this one answers "what did the servers say about themselves" —
which for a cluster is where the entire story of an outage lives, because the events that
matter most are the ones the server never sends to anybody.

| Family | Depth |
| --- | --- |
| **MySQL / Percona XtraDB Cluster** | full — a Galera event catalogue, cluster-state reconstruction, and verdicts. See [PXC](#pxc-the-log-that-never-says-error). |
| **MySQL asynchronous replication** | full — a replication catalogue and its own verdicts. See [async replication](#asynchronous-replication-a-different-vocabulary). |
| **PostgreSQL** | the Packet Inspector's classifier, on the shared timeline |
| **MongoDB** | the Packet Inspector's classifier, on the shared timeline |
| **Valkey** | the Packet Inspector's classifier, on the shared timeline |

---

## Where the logs come from

Two ways, and both make one **bundle**:

- **Read them from the nodes.** Tick the members of a stack and press *Read N logs*. The
  logs are tailed from inside each container and parsed together. Several nodes in **one**
  request, deliberately: three members' logs pulled seconds apart are three views of the
  same cluster, and a UI that made you fetch and combine them by hand would be a worse
  version of `tail`.
- **Upload them.** One file per node, in a single pick. The format is read from the bytes,
  so nothing has to be declared.

The paths tried per engine are the same ones the Packet Inspector uses
(`/var/log/mysqld.log`, `/var/log/mysql/error.log`, PostgreSQL's day-of-week files plus
Patroni's own, MongoDB's, and for Valkey the file **or** the systemd journal, because
dbcanvas sets no `logfile` and Valkey writes to stdout).

Bundles live in memory, like the Packet Inspector's captures, and do not survive an app
restart. The raw text of every source is kept alongside the parse, so **Show this in the
file** and **download** both work on exactly what was read.

> **Upload every member's log, not just the one that looks broken.** A node reporting that
> a peer went inactive is telling you about its own network as much as about the peer, and
> the only way to tell those apart is the peer's own log for the same seconds.

---

## Severity is not the log's level

On a Percona XtraDB Cluster node the level field is nearly useless for this. A capture
containing a complete node crash, an eviction, a state transfer and a full rejoin held:

```
314 [Note]     8 [System]     5 [Warning]     0 [ERROR]
```

`declaring node with index 1 suspected`, `suspected node without join message, declaring
inactive` and `Shifting SYNCED -> OPEN` are the whole story of an outage, and every one of
them is a `[Note]`.

So severity here comes from what a record **means**:

| | | |
| --- | --- | --- |
| ✓ **Good** | the cluster reaching a healthy state | `Synchronized with group, ready for connections`, `Member synced with group`, `State transfer complete` |
| **!** **Warning** | degraded, transitional, or expensive but working | `Shifting SYNCED -> DONOR/DESYNCED`, `SST in progress`, `Applying backlog`, `Peer went quiet` |
| ✕ **Bad** | broken, gone, or refusing to serve | `Received NON-PRIMARY`, `declaring node … inactive`, `Will never receive state. Need to abort.` |
| · Background | real records, but not news | quorum results, view restatements, provider configuration |

A record the catalogue calls background but the server itself called an error is still an
error — an unrecognised `[ERROR]` is never filed as information.

Colour is never the only signal. The palette is the validated `--status-ok` /
`--status-warn` / `--status-crit` triad (see the note in `app/web/src/index.css`), and
every row carries the glyph and the word as well, so it survives greyscale and every form
of colour-vision deficiency.

### Which node, in its own colour

Severity answers "how is it doing". A node also needs an identity — "which one is this" —
and the two must not share a channel, or a reader has to work out each time whether magenta
means *pxc03* or *bad*.

So each node gets a colour of its own, from a **separate palette drawn only from the cool
arc**: cyan, blue, periwinkle, violet, magenta. Red, amber and green are the status
palette's and stay reserved. The chip appears everywhere a node is named — the timeline
lane, the sources table, every event row, the node filter, the instant readout — and always
beside the node's **name**, which is what makes the colour an accelerant rather than the
signal.

| Slot | Hue | Light | Dark |
| --- | --- | --- | --- |
| 1 | cyan | `#0e97b4` | `#26a2bf` |
| 2 | blue | `#03539e` | `#0f6ac3` |
| 3 | periwinkle | `#8273ff` | `#887efb` |
| 4 | violet | `#8708d9` | `#9f13fd` |
| 5 | magenta | `#910574` | `#c4039e` |

Chosen by search and checked with a validator, not picked by eye. Measured with `--pairs
all` — all-pairs rather than adjacent, because any two nodes can end up next to each other
in the event list — on every surface this app renders on (`#ffffff` and `#fbf1d3` light;
`#161b24`, `#0e1530`, `#241546`, `#122019` dark):

| | within the set | vs the status triad |
| --- | --- | --- |
| **light** | CVD ΔE 9.6 · normal 16.6 · all ≥ 3:1 | CVD ΔE 9.1 · normal 15.1 |
| **dark** | CVD ΔE 8.8 · normal 16.3 · all ≥ 3:1 | CVD ΔE 15.5 · normal 17.0 |

(ΔE is OKLab ×100; CVD is the worse of protanopia and deuteranopia under
Machado–Oliveira–Fernandes 2009 at severity 1.0. The gates are CVD ≥ 8 and normal-vision
≥ 15.)

Three details that are deliberate:

- **Slot N is the same hue family in both modes**, stepped for each surface. Searching the
  two modes independently produced two perfectly valid palettes made of *different* hues,
  which would have meant a node changing colour when the viewer changed theme — and the
  colour is its identity.
- **The dot carries the colour; the label stays in ink.** A hue validated to 3:1 against the
  surface is a legal mark, not legal type.
- **It stops at five.** A sixth hue in that arc could not be separated from the first five
  by a colour-blind reader, and cycling would put two nodes on screen wearing the same
  colour. So a sixth source gets a neutral chip and the page says so. That is only safe
  because the name is always there.

---

## Records, not lines

The most informative things a Galera node writes are **not single lines**:

```
2026-08-14T01:42:31.045860Z 0 [Note] [MY-000000] [Galera] Current view of cluster as seen by this node
view (view_id(PRIM,0bc20092-ac42,4)
memb {
	0bc20092-ac42,0
	23503af2-adb5,0
	}
joined {
	}
left {
	}
partitioned {
	119e686d-8942,0
	}
)
```

The header says nothing at all. Everything that matters — who is still here, who is gone,
and whether the view is the primary component — is in the lines below it. A line-at-a-time
reader throws all of it away. The same is true of `Quorum results:` (`members = 2/3`) and
of the `View:` block, which is the only place member UUIDs are paired with node names.

So a log is folded into **records**: a header plus every line under it that is not itself a
header. Two consequences worth knowing:

- **Untimestamped blocks are placed.** `Log of wsrep recovery (--wsrep-recover):` and its
  output carry no timestamps at all. They inherit from the record beside them and are
  marked `≈` in the list.
- **Repeats collapse.** A partitioned node writes the same peer-timeout line every three
  seconds for as long as the outage lasts. Identical records fold into one row with a count
  and a span (`×24 over 47.3s`) — but **never** state transitions, membership changes or
  state transfers, because folding those would destroy the timeline they are built from.

Roughly one event is kept per twenty raw lines of a Galera log; the rest is internal
bookkeeping. Nothing is hidden — the raw file is one click away on every event.

---

## The cluster timeline

One **lane per node**. The filled bar underneath is the state that node was in; the ticks
above it are events, coloured by the worst thing in each bucket.

The state track is the point. A row of event dots tells you something happened; a filled
lane tells you what each node **was** during and between them — which is the only way to
see at a glance that two members stayed green while a third went red for fifty seconds.

| State | Colour | Meaning |
| --- | --- | --- |
| `SYNCED` | good | up to date with the group and **serving queries** |
| `JOINED` | warning | has the data, still applying its backlog — not in the read pool, and holding flow control on everyone else |
| `JOINER` | warning | receiving a state transfer; answers nothing |
| `DONOR` | warning | feeding a transfer to a joiner, desynced while it does |
| `PRIMARY` | warning | in the primary component but not yet joined |
| `OPEN` | bad | no primary component — refuses every query with **1047** |
| `CLOSED` | bad | the provider is shut down |
| `DOWN` | bad | the server is not running |
| `UNKNOWN` | grey | the log does not say |

**Click** for a readout of every node at that instant. **Drag** to narrow the event list,
the ticks and the filters below to a window.

### Where the state track comes from when the log does not say

A log is almost always a fragment, and a node that was already `SYNCED` when it begins
never logs a transition *into* `SYNCED`. Two answers, and the UI distinguishes them:

1. **Stated.** The left-hand side of the first transition *out* of a state is not a guess:
   `Shifting SYNCED -> DONOR/DESYNCED` says outright what the node was doing a moment
   earlier.
2. **Deduced**, drawn at reduced opacity and labelled *deduced* in the tooltip. A member
   that logs no state transition at all did not change state during the window — every
   transient state necessarily ends and would have logged its end. If it also reports
   itself inside a primary component, `SYNCED` is the only state left.

Anything else stays `UNKNOWN`, which is an honest answer and is drawn as one.

### Transitions are not states

Galera walks several states in the same microsecond. Cutting a member off produced, within
370 µs: a view saying "1 member, non-primary" while the node was still nominally `SYNCED`,
then `NON-PRIMARY`, then `Shifting SYNCED -> OPEN`. All three are real records. A readout
that lands in the first of them reports "SYNCED, 1 member, non-primary" — three facts that
were momentarily all true and together describe nothing. So a state lookup skips phases
shorter than 50 ms; the timeline still draws them.

---

## Clocks

MySQL writes RFC3339 with an explicit zone (`Z` under the default `log_timestamps=UTC`, an
offset under `SYSTEM`), so records from nodes in **different timezones** land on the correct
absolute instant with nothing to configure.

What parsing cannot fix is host clock skew. Each source carries an adjustable **offset**,
and the bundle reports when two sources do not overlap at all — the shape of "you uploaded
logs from different days", which otherwise draws a perfectly plausible and completely wrong
timeline.

---

## PXC: the log that never says ERROR

The Galera catalogue was written against logs from a **real three-node PXC 8.0.46 cluster**,
captured while doing the things that produce them. The corpus is in
`app/testdata/logsummary/` and the tests read it:

| Fixture | What was done to the cluster |
| --- | --- |
| `s01-bootstrap` | a cluster created from nothing, then joined by two members |
| `s03-crash-kill9` | `kill -9` on pxc02 under write load → eviction, auto-restart, IST rejoin |
| `s04-graceful-restart` | `systemctl restart mysql` on pxc03 — the clean departure |
| `s05a-ftwrl-desync` | `FLUSH TABLES WITH READ LOCK` held 50 s → Donor/Desynced |
| `s05b-flow-control` | a hard write flood against a deliberately slowed member |
| `s06-network-partition` | pxc03 cut off ports 4567/4568/4444 with `tc netem` for 52 s |
| `s07-sst-rejoin` | the aborted node started again, rejoining by full SST |
| `s08-crash-signal11` | `kill -11` to mysqld on pxc02 under load — a real crash, with the handler's own block |

Four things that corpus taught, each of which changed the design:

### 1. `left {}` does not mean "shut down cleanly"

That was the first guess and the fixtures disproved it. Across **every** capture,
including a plain `systemctl restart mysql`, `left` is empty and the departing member
appears under `partitioned` — the EVS layer sees a closed socket either way.

What actually separates maintenance from an incident is what the survivors logged in the
seconds **before** the view changed:

| | Survivors' log in the seconds before the view change |
| --- | --- |
| `systemctl restart` | *nothing* — the view changes ~1 ms after the socket closes, because the leaving node announced itself |
| `kill -9` | ~5 s of `reconnecting to … attempt 0` and `Connection refused`, then `declaring node with index 1 suspected, timeout PT5S (evs.suspect_timeout)` and `declaring inactive` |

So that is what the verdict decides on. One more wrinkle the corpus supplied: a process
that **aborts** also closes its sockets on the way out, so from the survivors' side a crash
looks identical to a clean stop — the "clean departure" verdict is therefore withheld
whenever the bundle holds a crash around the same time.

### 2. A NON_PRIM view is a statement about *this* node

When a node leaves or is cut off it writes a `NON_PRIM` view whose `partitioned` list names
every **other** member. Read at face value, one node's departure becomes "the entire cluster
vanished". The view kind decides which of the two records this is.

### 3. `kill -9` is not a crash

The first version of this feature could not see a crash at all, and the corpus is why: its
crash fixture was made with `kill -9`. SIGKILL cannot be caught, so no handler runs, the
process simply stops, and the only trace is what the *survivors* logged. A process that
dies on a catchable signal is a different event, and it writes a block in a format of its
own — no thread column, no `[Level]`, no `[MY-nnnnnn]`, no subsystem, because the signal
handler cannot call into the normal logging path:

```
2026-08-14T07:51:05Z UTC - mysqld got signal 11 ;
Most likely, you have hit a bug, but this error can also be caused by malfunctioning hardware.
Server Version: 8.0.46-38.1 Percona XtraDB Cluster (GPL), Release rel38 …
Thread pointer: 0x0
stack_bottom = 0 thread_stack 0x100000
 #0 0x7c27e246dc2f <unknown>
 …
```

Since none of that matched a header, the whole block folded into the **body of the record
above it** — which on a live cluster was `Member synced with group`, severity **good**. A
crash was being reported in green. And when the block happened to start an excerpt, with no
record above to absorb it, it was dropped outright.

Crash blocks are now records of their own. The record names the signal and what it means,
the build, whether a statement was running (the handler prints it when there was one), and
whether the backtrace resolved — a package build with no debug symbols prints every frame
as `<unknown>`, which is worth saying rather than leaving the reader wondering. The handler's
whole output stays in the record's detail, because that is the bug report.

`TestLogSummaryCrashEvidenceIsNeverLost` asserts the general rule against the raw fixture
text rather than the parsed events: if a line in the file says the server crashed, an event
must say the server crashed, at that line. Folding is what makes this feature work and also
what makes it dangerous, and that test is the guard.

### 4. Flow control is very nearly invisible

Driving a three-node cluster hard enough to register **ten** received flow-control messages
and **91 ms** of measured pause (`wsrep_flow_control_recv`, `wsrep_flow_control_paused_ns`)
produced exactly **one line** in the error log — and it was the *threshold*, not a pause.

So the Log Summary never reports "no flow control" from silence. It says the log cannot
tell you, and where to look instead (`wsrep_flow_control_paused`, `_sent`, `_recv`,
`wsrep_local_recv_queue_avg`, or the same series in PMM). What the log *can* tell you is the
interval itself, and an unusually low one is promoted to a warning: the default is
`gcs.fc_limit` (100) scaled by member count — 141 for two members, 173 for three — so a
cluster reporting `[2, 2]` will pause every writer as soon as one node is a couple of
writesets behind.

Setting `gcs.fc_debug` does add `FC: queue size: …` records, at the cost of a lot of volume.

---

## Asynchronous replication: a different vocabulary

Galera is a *synchronous* cluster, and its log is about membership and quorum.
Asynchronous replication has neither. Its records are about channels, the I/O and SQL
threads, GTID sets and binary-log coordinates — and its failure modes differ in kind: a
Galera member that falls behind is held back by flow control, while a replica that falls
behind is simply allowed to, silently, for as long as you let it.

The rules were written against a live three-node Percona Server 8.0.46-37 GTID topology —
one source, two replicas — captured while doing this to it:

| Fixture | What was done |
| --- | --- |
| `r02-dupkey-conflict` | a row written straight onto a replica, then the same key from the source → the applier stops with **1062** |
| `r03-repl-auth-fail` | the replication user's password rotated under a running replica → **1045** |
| `r04-binlog-purged` | `PURGE BINARY LOGS` while a replica was stopped and behind → **1236** |
| `r05-replica-lag` | a replica held 61 s behind by a table lock |
| `r06-stop-start-replica` | a clean `STOP REPLICA` / `START REPLICA` |
| `r07-source-crash` | SIGSEGV to the source with both replicas connected |
| `r08-replica-crash` | SIGSEGV to a replica mid-apply, and its XA crash recovery |
| `r09-source-unreachable` | 100% packet loss on 3306 from a replica for 80 s |

Unlike Galera, the **level field is usable here**: replication failures arrive as real
`[ERROR]` records with MY- codes and the good news as `[System]`. What the level cannot
tell you is which of them stopped replication and which the server will quietly retry out
of — so severity still comes from meaning.

Three things that capture taught, each of which changed the code:

### A replica can stop and keep looking healthy

`Replica_IO_Running: Connecting` is not a state anything logs repeatedly. When the
replication user's password was rotated, the replica wrote **one** error and then said
nothing for the rest of its life:

```
Error connecting to source 'repl@mysql01.example.net:3306'. This was attempt 1/86400,
with a delay of 60 seconds between attempts. Message: Access denied … Error_code: MY-001045
```

`1/86400` at 60-second intervals is **sixty days** of silent retrying. The record says so
in words, because that retry policy is the difference between "it will fix itself" and "it
will sit there until somebody looks".

### Lag is invisible, and so is a network outage

A replica held 61 seconds behind its source, while the source wrote continuously, produced
**not one record** about it — the same shape of blindness as Galera's flow control, and
handled the same way: the page states that the log cannot tell you, and points at
`Seconds_Behind_Source` and `performance_schema.replication_applier_status_by_worker`.

Worse, cutting a replica off its source with 100% packet loss produced **no disconnect
record at all**. The I/O thread blocked until `slave_net_timeout` rather than erroring, so
an 80-second outage left exactly one trace: the line saying it had *connected*. So a
"connected to source" with no failure and no restart before it is itself the evidence —
`lsFindingSilentReconnect` reports it, and names the other thing that looks identical
(somebody running `START REPLICA`), because the log genuinely cannot separate them.

### A server that is not a cluster member has no wsrep state

The first version reused the Galera state vocabulary for every MySQL log, so a plain
replica sat in `CLOSED` from its first start-up record onward — nothing ever moved it out.
A live, entirely healthy replication topology was reported as three servers that had not
served a query in thirteen minutes. Non-cluster servers now get the only two states they
have, `RUNNING` and `DOWN` (plus `STARTING`), and "was it serving" is a predicate over both
vocabularies rather than "is it SYNCED".

### The verdicts it adds

| | |
| --- | --- |
| **Replication stopped** / **still broken at the end of the log** | per replica, the failure that caused it and — when there is one — the recovery that paired with it, with the outage measured |
| **The source purged binary logs this replica still needed** | the async twin of "IST impossible, falling back to SST": the missing GTID range, and that the replica has to be rebuilt |
| **A replica reconnected without anything saying it had disconnected** | see above |
| **Replication lag is not recorded in this log** | the honest note |

One more thing the fixtures produced, which is not about replication at all: Percona Server
writes the crash block **twice** — once raw and once again line by line through the error
log as `MY-013951`. Left alone, one crash became twenty-odd separate "Server crashed" rows,
the bug-report URL among them. The re-emitted block now folds into the raw one, so a crash
is one record whichever way the build writes it.

---

## Group Replication and InnoDB Cluster: the log that says what it means

The third cluster vocabulary, and the one that reads most easily. Where a Galera node
narrates an outage in `[Note]` records and multi-line view blocks, the Group Replication
plugin writes one coded, complete, single-line record per event and says what it is doing
in plain English:

    Plugin group_replication reported: 'Member with address gr03:3306 has become unreachable.'
    Plugin group_replication reported: 'Primary server with address gr01:3306 left the group. Electing new Primary.'
    Plugin group_replication reported: 'This server is not able to reach a majority of members
                                        in the group. This server will now block all updates.'

Every rule was written against a live three-node Percona Server 8.0.46-37 single-primary
group, deployed by DBCanvas in **both** of its modes — raw `groupreplication` and
MySQL-Shell-managed `innodbcluster` — and driven through fifteen scenarios. The fixtures
are the `g*` directories under `app/testdata/logsummary/`.

Member states are GR's own, spelled as `performance_schema.replication_group_members`
spells them, plus one the plugin describes but the table does not name:

| state | |
| --- | --- |
| `ONLINE` | in the group, caught up, serving |
| `RECOVERING` | in the group, still applying the backlog — it has the data, it is not usable |
| `BLOCKED` | cannot see a majority: every write refused, reads still served, **stale** |
| `ERROR` | the plugin stopped on an error and the member removed itself |
| `OFFLINE` | mysqld is up, Group Replication is not — the server is not in its cluster |

They are deliberately not folded into Galera's `SYNCED`/`JOINER`/`DONOR`. The two machines
answer the same question, but `RECOVERING` is not `JOINER` — a joiner has no data, a
recovering member has data and is catching up — and somebody reading a GR member wants the
word that appears in the table they are about to query.

### 1. A clean stop and a death *are* distinguishable here

This is the thing Galera cannot do (see `left {}` above), and GR settles it with one
record. A `systemctl stop` produces `Members removed from the group` with **nothing** in
front of it, because the leaving member announces itself. A `kill -9` produces
`Member with address … has become unreachable` **first**, and the removal follows one
expel timeout later — 16.0 s in the captures, twice, to the tenth of a second.

### 2. The two ways of leaving the group have opposite read-only outcomes

The single most dangerous thing in the file, and the reason the catalogue was worth
writing:

| exit | what the plugin does | left |
| --- | --- | --- |
| applier failure (`MY-011451`/`MY-011452`) | `MY-011712` — "set into read only mode" | **safe** |
| refused at join (`MY-011526`/`MY-011522`) | `Setting super_read_only=OFF` | **writable** |

Both read as "the member left the group" if you only match the leave record. A member out
of the second exit is up, accepting writes, and holding data the cluster does not have —
and the corpus caught it happening: the load generator running during the capture did what
any connection pool does, reconnected to the first member that answered, and wrote 1,263
rows into a server that was no longer part of the cluster.

### 3. A split does not heal itself, and the log does not say so

Cutting one of two remaining members off port 33061 produced `MY-011495` on **both** sides:
neither had a majority, both blocked every write, and every member was up and answering
reads perfectly happily throughout.

Restoring the link changed nothing. For the two and a half minutes until an operator
intervened, neither side logged another word. The plugin does own the vocabulary for
recovery — `MY-011494` *is reachable again*, `MY-011498` *has resumed contact with a
majority* — and it appeared only once somebody acted. So a block with no matching
`MY-011498` after it is reported as **still in force at the end of the window**, rather
than as something that presumably sorted itself out.

### 4. A member can be up, writable, and not in the group — silently

`kill -9` left systemd to restart mysqld, which came back, logged `ready for connections`,
and stopped there: `group_replication_start_on_boot` is OFF in raw GR mode, so nothing
rejoined. Measured at that moment: `super_read_only=0`, one row in
`replication_group_members` (itself), and 666 transactions behind the group. Its own log
never mentions any of it — a monitor that checks whether mysqld is up sees a healthy
server.

That is why `ready for connections` on a group member resolves to `OFFLINE` and not
`RUNNING`, and why a member whose own fragment contains no plugin records at all is still
recognised as one when **another** source names it as a member.

### 5. Flow control is invisible — more absolutely than in Galera

Measured, and pushed as far as the settings allow: `flow_control_mode=QUOTA`, both
thresholds at 1 (the most eager configuration there is), a member slowed to 120 ms RTT with
netem, and **1,364 transactions certified** through the flood. All three members wrote
**zero** lines. Galera at least writes its interval once when membership changes; GR does
not write even that. The numbers live in
`performance_schema.replication_group_member_stats`, and the finding says so instead of
letting silence read as health.

### 6. A healthy InnoDB Cluster is *full* of errors

Deploying a perfectly good three-node cluster through MySQL Shell wrote **26 `[ERROR]`
records** across the three members. Every one was Shell checking the instance: it opens a
throwaway replication channel called `mysqlsh.test` and deliberately fails to start it, to
learn whether the instance can replicate and whether its server id collides — and it asks
the server to start Group Replication before configuring it, so *"the
group_replication_group_name option is mandatory"* and *"Group Replication plugin is not
installed"* are answers to Shell's questions, not faults.

This is the exact mirror of the PXC lesson at the top of this document. There the level
**under**-reports the severity; here it wildly **over**-reports it. Trusting it would
report every InnoDB Cluster as broken on the day it was built, so those records are kept —
deleted evidence is not evidence — and filed as information with the explanation attached.
The one rule in the package that may overrule a record's own level is used here and
nowhere else.

The two modes differ in one more way worth knowing, and it is the difference between an
outage that ends by itself and one that does not: Shell sets
`group_replication_start_on_boot=ON`. The same `kill -9` that strands a raw GR member
outside its group leaves a Shell-managed one to rejoin on its own — verified by doing it
to both.

### The verdicts it adds

| | |
| --- | --- |
| **Every member lost its majority — the cluster stopped accepting writes** | who blocked, when, and whether anything ever recorded them recovering |
| **A member left the group and went back to accepting writes** | the split-brain check: a leave followed by `super_read_only=OFF` that was not an election |
| **A member has transactions the group does not, and was refused** | with the offending GTID ranges, and why raising the clone threshold cannot fix it |
| **A server restarted and never rejoined its group** | built on the absence of a plugin start after the last start-up |
| **The group elected a new primary** | with the write outage measured from the earliest notice that the old primary had gone quiet |
| **A member rebuilt itself from a clone** | including the two surprises: the recipient's data is erased first, and mysqld restarts itself at the end |
| **A member is stuck recovering and is not serving** | the donor-cycling loop, which never reports a final failure |
| **Some of these errors are MySQL Shell testing the instance, not failures** | see above |
| **Flow control leaves no trace in this log at all** | the honest note |

---

## The verdict

Everything in the verdict is a statement that could not be made from one node's log alone,
or that requires holding two distant records side by side.

| | |
| --- | --- |
| **A server stopped abnormally** | an abort, an assertion, a fatal signal, an inconsistency eviction |
| **A node restarted after an unclean stop** | `--wsrep-recover` found a *real* position. It runs on every start — including the first on an empty datadir, where it recovers the null UUID and seqno −1 — so only a real position is evidence |
| **A member was lost, not shut down** | a departure with a suspect timeout in front of it |
| **A member left the cluster cleanly** | a departure without one, or a shutdown record |
| **The cluster split — one side kept quorum, the other did not** | who was non-primary, and who was still serving while they were |
| **The nodes did not agree on who was in the cluster** | the same instant, described differently by two members. Downgraded to a warning when everyone was still primary and one was joining, which is ordinary lag rather than a split |
| **Time spent not answering queries** | per node, summed across every stretch outside `SYNCED`, with the reason. Rated *bad* only for time spent in `OPEN` — a running server that can see no primary component — because `JOINER`/`DONOR`/`CLOSED` is planned work |
| **A rejoin needed a full SST because the gcache was too small** | named against the **donor**, whose `gcache.size` is the setting to raise |
| **A full state snapshot transfer ran** / **A state transfer failed** | with duration, and both ends named from the record rather than from whose file it was found in |
| **A member desynced itself from the group** | with duration. Desyncs shorter than 2 s are ignored — a donor desyncs for milliseconds as part of every transfer |
| **A new cluster was bootstrapped** | *bad* rather than a warning when two different nodes did it, which produces two clusters that will never merge |
| **These logs do not cover a common period** | nothing here can be compared across nodes |
| **Nothing was written in this window** | a healthy PXC cluster under load writes **nothing** to its error log — measured: thirty seconds of continuous inserts across three nodes produced zero records on all three. So silence is reported as the good news it usually is, together with the other explanation for it |

---

## Reading a record

Selecting an event shows the classified record, **what it means** in words, the structured
facts pulled out of it (peer, state, member counts, sequence number), the folded
continuation lines, and **Show this in the file** — the surrounding lines of the original,
because a classifier is a *reading* of a log and the reader has to be able to check it.

Member UUIDs are translated to node names wherever the bundle knows one, with the UUID kept
beside the name so a line can still be found in the raw file. The mapping is pooled across
sources: one node's log usually names only some of the members, and the name for the UUID
in front of you is very often in a different file. That is the whole reason to read three
logs together rather than one at a time.
