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
| **PostgreSQL** | full catalogue — standalone, streaming replication and Patroni |
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

## MongoDB replica sets: the log that repeats itself

The fourth cluster vocabulary, and structurally the easiest: since 4.4 `mongod` writes one
JSON object per line, and every message carries a numeric `id` that is stable across
releases even when the English changes. A rule keyed on `21358` keeps working through an
upgrade that rewords the message — the opposite of MySQL's guarantee, and much the more
useful one.

The whole state machine is one record:

```json
{"t":{"$date":"…"},"s":"I","c":"REPL","id":21358,"ctx":"conn301",
 "msg":"Replica set state transition","attr":{"newState":"SECONDARY","oldState":"PRIMARY"}}
```

`newState` **and** `oldState`, which is what makes a fragment readable — a member already
PRIMARY when the excerpt begins never logs a transition into it. Its companion `21215`
reports the same about a *peer* (`{"hostAndPort":…,"newState":…}`), so one member's file
describes the whole set.

Every `mongod` log is parsed here rather than by the shared MongoDB classifier the Packet
Inspector uses, because the facts that matter live in `attr` and the shared entry type does
not carry it. The replica-set sniff decides only the **flavour** — which findings may speak
about this source — and a standalone `mongod` gets no swimlane of replica-set states,
because it has no member states, no elections and no rollbacks and inventing a topology for
it would be worse than saying nothing.

Every rule was written against live three-node Percona Server for MongoDB replica sets,
driven through `rs.stepDown()`, a SIGKILL on the primary under write load, a member cut off
port 27017, a **partitioned primary written to and then healed**, and a wiped data directory
resynced from scratch. The fixtures are `m*` under `app/testdata/logsummary/`.

### Sharded clusters: the router that logs nothing

A sharded cluster is three kinds of process and only one of them is a database. A shard
member and a config-server member are ordinary `mongod`s, and everything above reads them
unchanged. A **mongos** is not a `mongod` at all — it stores nothing, replicates nothing,
and its log is about where things *are* rather than what they are doing.

Which makes it the most misleading file in the set. Stopping an entire three-member shard
under live traffic on 6.0 and 7.0 produced two client-visible failures — a read that failed
with `FailedToSatisfyReadPreference` and a write refused outright — and the router's log
recorded **neither**. Not a warning, not an error. What it recorded was the shard's topology
changing, at `INFO`, over and over. An operator handed that log sees a healthy router for an
outage the application saw plainly.

So a router gets its own flavour, `mongos`, which does two things. It keeps every
replica-set finding away from it — a router monitors every shard and logs a great deal about
replica sets without being in one, and without the gate a perfectly healthy router is
reported as a member that never became primary. And it lets the verdict end with the honest
note, the sharded twin of the replication-lag one: **failed routing is not in this log.**

**One record covers the whole sharding vocabulary.** Every topology change a cluster makes —
a shard added or removed, a collection sharded, a chunk split or moved, every balancer round
— is written by whichever config server is primary under id **22080**, with the operation in
`attr.event.what`:

```json
{"id":22080,"msg":"About to log metadata event","attr":{"namespace":"changelog",
 "event":{"what":"addShard","ns":"","server":"cfg1:27017",
          "details":{"name":"rs0","host":"rs0/s0r1.example.net:27017,…"}}}}
```

One rule reads all of it, and keeps reading it when MongoDB adds an operation, because the
operation is *data* rather than a rule. It is also the only place any of it is recorded — a
bundle without that config server contains none of it, which the finding says out loud.

**A shard with no primary is readable from the router.** `4333213` carries a topology
description whose `topologyType` is `ReplicaSetNoPrimary`, so one router's log says which
shard could not take writes and for how long — including the config servers, which is the
worse case: while *they* have no primary the cluster's metadata is read-only, no chunk can
move, and any migration already in its critical section holds writes to that range.

Measuring that span is where the first version was wrong. Taking the first and last
`NoPrimary` record for a set reported one that was merely still being formed as an outage
"for 0s", and inflated a **twelve-second** config-server outage into **14.4 minutes**,
because the last such record was fourteen minutes after the first with a healthy period in
between. The span is now measured to the record that *ends* it.

The version sweep applies here too, and holds across all three. Between 6.0 and 7.0 MongoDB
replaced the distributed lock manager with DDL coordinators, reworded the shard-identity
warning from "--shardsvr" to "ShardServer role", and stopped auto-splitting chunks; 8.0 added
automatic chunk merging. Every one of those is a different message; **not one is a different
id**. The same driven incident produces the same nine findings on 6.0, 7.0 and 8.0.

8.0's `autoMerge` is the clearest evidence that keying the changelog on one id was right: it
appeared in the 8.0 capture through a rule written and tested entirely against 6.0 and 7.0,
with no code change, because the operation is *data* rather than a rule.

### Across versions: 6.0, 7.0 and 8.0

The design rests on one claim — that MongoDB's numeric ids are stable across releases even
when the English changes — and that claim was worth checking rather than believing. The same
incident was driven against **6.0.29-23** and **7.0.39-21** and the id namespaces diffed
against 8.0.

The claim holds. Every id in the catalogue that fires at all carries the **identical
message** on all three releases; the same physical incident produces the same eight findings
on each. What the sweep did find was three things wrong on this side of the line:

**One record does not exist before 7.0.** `6984700` "Operations reverted by rollback" is the
obvious source for how much a rollback threw away, and a 6.0 rollback read through it reports
that data was lost without saying how much — the one number the reader actually needs.
`21612` "Rollback summary" carries the same counts on every version *and* names the affected
collections and the directory the discarded documents went to, so it is now preferred
outright. 7.0 and 8.0 had been reporting **less** than 6.0 could.

**One rule was a guess and the sweep disproved it.** `20557` sat in the catalogue as "Unclean
shutdown detected". SIGKILLing a `mongod` on 6.0, 7.0 and 8.0 never produced it once.
`22271` ("Detected unclean shutdown - Lock file is not empty"), `501401` and `20631` all did,
on every version — `mongod` says it three times from three subsystems, which is why they
share a rule and collapse into one row.

**A twenty-thousand-line tail with no REPL records in it.** One member's excerpt turned out
to be entirely the replica-set monitor complaining about an unreachable peer — `NETWORK` and
`CONNPOOL`, several thousand records, not one `REPL` among them. The sniff filed it as a
standalone `mongod`, which sent it through the shared classifier, which has no severity
filter: twenty thousand records became twenty thousand events of class *other*, and the
verdict layer read them as an asynchronous replica whose replication was broken. Two fixes:
the sniff now also accepts `attr.replicaSet`, which every replica-set-monitor record carries
on every version, and the MongoDB filter is applied to *every* `mongod` log rather than only
to recognised members.

### 1. A rollback is silent data loss, with a receipt

The one incident in this whole package where committed, acknowledged data is deliberately
discarded. A primary on the wrong side of a partition accepts writes, the majority elects
somebody else, and when the old primary rejoins it throws its writes away. The client was
told they succeeded and is never told otherwise.

The log says exactly how much went and where it was put:

```
21612    Rollback summary   {"rollbackCommandCounts":{"insert":300,"create":1},
                             "affectedNamespaces":["ftdctest.doomed"],
                             "rollbackDataFileDirectory":"/var/lib/mongo/rollback/4275fd4d-…"}
21609    Preparing to write deleted documents to a rollback file
         {"namespace":"lab.t","file":"/var/lib/mongo/rollback/…/removed.…bson"}
```

`21612` rather than the more obvious `6984700`, because `6984700` did not exist before 7.0 —
see the version notes above. The finding leads with the count, names the collections, and its
advice carries the file paths: that file is the only copy left, nothing deletes it for you and
nothing replays it for you.

### 2. It repeats itself, endlessly

A dead peer produces `Heartbeat failed after max retries` **every two seconds for as long as
it is dead** — 1,234 log lines in the forty seconds after one SIGKILL. MySQL and Galera
write a failure once and go quiet. Record collapsing is not a nicety here; without it one
member's outage buries every other record in the file. It is also an asset: the span of the
collapsed row *is* the length of the outage, as the survivors experienced it.

### 3. A cut-off secondary does nothing, and says almost nothing

Galera's minority node refuses queries with 1047 and Group Replication's blocks every write.
A MongoDB secondary that cannot see the primary just keeps serving reads of whatever it has,
falling further behind, logging only heartbeat failures. There is no state change to find.

### 4. Severity comes from the id, because the level is useless

A rollback — the one event here that loses data — is logged entirely at `"s":"I"`. So is an
election, an initial sync, and a member going DOWN.

### And three rules that were wrong

Written from memory rather than from the corpus, and all three caught by it: `22322` is
*"Shutting down checkpoint thread"*, not a fatal assertion; `21444` is a dry-run election
**succeeding**, not failing; `20698` is a restart marker, not a shutdown. Four further rules
have never matched a real record and are fenced off in the catalogue under a heading that
says so — an unexercised rule is a guess until a real log matches it.

### The verdicts it adds

| | |
| --- | --- |
| **Acknowledged writes were rolled back and lost** | how many operations, and the rollback files that hold the only copy |
| **The set had no primary for …** | measured from the peer reports, so one file is enough. A killed primary costs about `electionTimeoutMillis`; the fixture measures 9.6s against a 10s default |
| **A member could not be reached by the rest of the set** | measured from the heartbeat-failure span |
| **The set elected a primary** | and whether it was a requested step-up or a failure |
| **A member rebuilt itself from another member's data** | an initial sync, with how long it took |
| **Replication lag is not in this log** | the honest note — and it points at `diagnostic.data`, which is already on the machine. See [`FTDC_SUMMARY.md`](FTDC_SUMMARY.md) |

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


---

## PostgreSQL: the log that says the least

The fifth cluster vocabulary, and the one with the weakest guarantee of the lot.

MongoDB gives every message a numeric id and promises it is stable. MySQL gives most of them
an `MY-nnnnnn` code. PostgreSQL gives **neither** — a server log line is a timestamp, a pid,
a level and a sentence, and the sentence is the only thing to match on. It is also
**translated**: a server running with `lc_messages` set to anything but English writes a log
this catalogue cannot read at all. That is a limitation worth stating rather than
discovering.

There is one escape and the catalogue uses it wherever it can. **SQLSTATE** is five
characters, defined by the standard, unchanged between releases and never translated — but it
reaches the log only if the operator puts `%e` in `log_line_prefix`, which is not the default
and which dbcanvas does not set either. So every rule that has one carries **both**, and a
server configured with `%e` gets the robust match while a server without it falls back to the
English:

```
2026-08-15 11:00:00.000 UTC [123] 53300 FATAL:  sorry, too many clients already
                                   ^^^^^ matched on this when it is there, on the words when it is not
```

### FATAL does not mean what it means everywhere else

The single most important thing about reading a PostgreSQL log, and the thing that made the
first version of this page wrong in both directions.

In MySQL and MongoDB the level is a reliable floor: an `[ERROR]` is a problem and nothing may
be filed below it. PostgreSQL does not work that way.

- **`FATAL` means THIS BACKEND is ending**, not that the server is failing. A client that
  connects while the server is still starting gets a FATAL. So does every connection the
  cluster manager terminates during a routine switchover, and so does a standby noticing that
  its primary has gone. On the corpus, taking the level as a floor produced **twenty-seven
  "bad" events for clients that arrived a second too early during an ordinary restart**.
- **`LOG` is where the important records are.** Every promotion, every timeline switch, every
  recovery, every checkpoint. A page that trusted the level would rank a failover below a
  mistyped password.

So rules that know better say so, and the ones that do not still take the floor.

### Three flavours, because three things share a log

| flavour | what it is | what may be said about it |
| --- | --- | --- |
| `postgres` | a server with no replication in evidence | nothing about clusters |
| `pgstream` | streaming replication, with or without repmgr | replication, promotion, timelines |
| `patroni` | a Patroni member | all of it, plus leader locks and the DCS |

A Patroni member's PostgreSQL log is indistinguishable from a streaming standby's — because
that is what it is. What separates them is the **second log**, and collecting it turned out
to be the first bug: `/var/log/patroni/` exists on an ordinary Patroni node and is **empty**,
because Patroni logs to the journal. The collector now reads the PostgreSQL file *and*
appends `journalctl -o cat -u patroni`, which prints the message without the syslog prefix —
exactly the shape Patroni's own format already has. Both halves are wanted, not one or the
other: Patroni decides the failover and PostgreSQL carries it out, so the decision is in one
file and its consequence in the other, seconds apart.

That also produced the second bug. Two logs concatenated means the file is **not** in time
order, and the state machine walked it as it arrived — carrying the state from the end of the
PostgreSQL log onto the beginning of the Patroni journal. The corpus showed it immediately:
records from the first seconds of a cluster were labelled `STANDBY`, a state that member did
not reach for another minute.

### The finding this catalogue exists for

Stop etcd on every member of a healthy Patroni cluster. Patroni writes:

```
INFO: demoting self because DCS is not accessible and I was a leader
```

and PostgreSQL writes:

```
LOG:  received fast shutdown request
```

**and nothing else.** An operator reading only the database log sees a primary that stopped
for no reason at all. Nothing was wrong with PostgreSQL — Patroni will not stay leader while
it cannot renew its lock, because it cannot tell an unreachable DCS from one that has already
given the lock to somebody else, so it steps down to guarantee there is never more than one
primary. The cause is a network problem between the leader and etcd, and it is in a different
file written by a different process.

The verdict says that outright, and a test asserts the other half of it: that PostgreSQL's
own records for the same window mention neither etcd nor the DCS.

### The honest note, and PostgreSQL's is the worst of the three

MySQL writes nothing when a replica falls behind. MongoDB writes nothing when a member does.
A PostgreSQL standby is worse than silent — it writes

```
LOG:  waiting for WAL to become available at 0/6AE1E88
```

steadily, and that message means the same thing whether the standby is idle and up to date or
**receiving nothing at all**. The corpus contains both cases and they are indistinguishable.
The reader of such a file is not merely uninformed; they are actively reassured.

The fixture proves it: `p02-streaming` contains that message interleaved with
`could not connect to the primary server` while the primary was stopped outright, and a test
asserts both halves are present so the claim stays supported by evidence.

### Three fixtures

`p01-patroni-cluster` is a live three-node Patroni cluster on PostgreSQL 16.14, driven
through a planned switchover, an unplanned failover with the leader SIGKILLed, and a
whole-DCS outage. `p02-streaming` is three nodes streaming with **no** cluster manager
running — the primary stopped, nothing promoted anything, and a standby was promoted by hand.
`p03-standalone` is one server on its own, and exists to prove the cluster findings stay
quiet: telling somebody with a single server that their cluster has no leader is worse than
saying nothing.

