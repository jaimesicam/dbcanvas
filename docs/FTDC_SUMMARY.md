# FTDC Summary

Every `mongod` writes `diagnostic.data` in its dbPath. Once a second, with no configuration
and almost no cost, it captures `serverStatus`, `replSetGetStatus`, the WiredTiger
statistics and the host's CPU, memory and disk counters. It is the black box recorder, and
it is the only artefact that can answer *"what was the server doing at 04:12 last Tuesday"*
when nobody thought to be watching at the time.

It is also undocumented and awkward to read, which is presumably why so few people ever
open their own. This page decodes it and draws the nine charts that answer something.

---

## Getting data in

**From a running node.** Pick a MongoDB node and the whole `diagnostic.data` directory is
read out of the container and decoded. Nothing has to have been enabled beforehand — that
is the point of FTDC.

**By upload.** The `metrics.*` files themselves (pick them all at once), or a `.tar.gz` of
the directory. Files are ordered by name before decoding, which is chronological because
`mongod` names each file after its first sample; `metrics.interim` — the file currently
being written — is always placed last. Several files decode into one continuous series.

A truncated final chunk is normal: `metrics.interim` is being written while it is copied.
Those chunks are counted and skipped rather than failing the parse, and the count is shown.

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

A decoded file holds around four thousand distinct metrics. Almost none are worth a chart,
and a page offering all four thousand is a metric browser — useful to somebody who already
knows what they are looking for, and useless to somebody holding a directory they were sent.
So the charts are chosen:

| chart | the question it answers |
| --- | --- |
| **Replica-set member state** | who was primary, and when did that change |
| **Replication lag** | was a secondary behind, and by how much — **not in the server log at all** |
| **Oplog size** | could a member that went away still catch up, or does it need a resync |
| **Execution tickets** | was the storage engine itself the bottleneck |
| **Operations queued** | were operations waiting to get in |
| **Connections** | did a driver pool run away, and how close to the limit did it get |
| **WiredTiger cache** | was eviction keeping up with writes |
| **Operations** | what was the server actually asked to do |
| **CPU** | was any of this the machine rather than the database |

Each carries a **why** line and an **advisor** — what this capture's numbers actually say,
in the same four levels the Stalk Summary uses.

### Two details that decide whether a chart works at all

**8.0 moved the tickets.** `wiredTiger.concurrentTransactions` became `queues.execution`.
Both are read, so the page works on either — a chart built on the documented-in-2019 name is
a chart that is silently always empty on a modern server. Every key on this page was read out
of a real file rather than out of documentation, for exactly that reason.

**Member names are not in FTDC.** Strings are not metrics, so `replSetGetStatus.members.0`
has a state, an optime and a ping but no name. The member-state chart says "member 0" and
marks which one is `self`; an index is honest and an invented name is not.

### Two deliberate choices in the maths

**Downsampling takes every nth point, and does not average.** These are gauges as often as
counters, and an averaged spike is a spike that did not happen. Taking every nth can miss
one; averaging *invents a shape*, which is worse in a diagnostic tool.

**Rates use the real sample spacing.** FTDC slows its own sampling under load, so assuming
one second would overstate every rate exactly when it matters most. A counter that goes
backwards — a restart — yields zero rather than a large negative spike that would invent an
event.

**Lag is measured against the freshest member, not against the primary.** During a failover
there may be no primary at all, and a lag chart that goes blank exactly when the incident
happens is the wrong chart.

---

## What it is for, in one example

The Log Summary's MongoDB verdict ends with a note saying that replication lag is not in the
server log — because it is not, in any form. Reading the same replica set's
`diagnostic.data` through this page over the same window reports:

```
replLag   Replication lag   [crit] A member was 61.0s behind at its worst
```

That is the same incident, from the artefact that recorded it. Neither page can tell you
that on its own.
