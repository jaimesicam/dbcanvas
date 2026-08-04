# Packet Inspector

The **Packet Inspector** captures traffic on a provisioned MySQL node with `tcpdump`
and reads it back as **decoded MySQL**: the statements, the responses, the response
times, and the network problems underneath them — retransmissions, gaps, resets, zero
windows. It's for the question "what actually crossed the wire, and what did the
server say back", which neither the slow log nor `SHOW PROCESSLIST` can answer.

Open it from the sidebar (**Packet Inspector**) or at `#packet-inspector`.

MVP scope: **MySQL** (Percona Server, PXC, MariaDB, MySQL Community, and All-in-One
MySQL instances). PostgreSQL and MongoDB nodes are not offered — the decoder speaks
MySQL, and a capture of anything else would be a list of "TCP data".

## Where the capture happens

Inside the **database node's own container**, on the interface holding its stack
address:

```
tcpdump -i eth0 -s 65535 -n -q -c 50000 port 3306 -w /var/tmp/dbcanvas-pkt/<id>.cap
```

The exact command is shown in the UI. Three details matter:

- **The interface is detected, not assumed.** The one carrying the node's stack
  address is chosen (`eth0` on a Docker node, `eth1` on a Vagrant node). `-i any`
  would yield Linux-cooked frames and duplicate loopback traffic.
- **The server is the vantage point.** A capture taken anywhere else misses traffic
  that never arrives — which is exactly what you are looking for when a client
  reports timeouts.
- **Loopback traffic is invisible here.** A client on the same node connecting over
  `127.0.0.1` or a socket does not appear on the stack interface. Capture from the
  node the client actually reaches over the network.

`tcpdump` is installed on the node on first use. Every capture is bounded three ways at
once, and ends at whichever comes first:

| Bound | Limit | Why |
| --- | --- | --- |
| **Duration** | up to **3600 s** (one hour) | long enough to sit and wait for an intermittent problem |
| **Packets** (`-c`) | up to **100,000** | what actually stops a capture on a busy server — 100k packets can arrive in seconds |
| **Size** | 192 MB on disk | a file bigger than this cannot be read back over the exec channel, so the capture is stopped and what was captured is kept (the UI says it was capped) |

Then the pcap is pulled back into the app, decoded, and deleted from the node. For a long
watch on a busy server, narrow the BPF filter — otherwise the packet ceiling will end the
capture long before the hour is up.

## What gets decoded

| Layer | Read |
| --- | --- |
| Capture file | classic pcap (µs and ns) and pcapng — tcpdump writes the first, Wireshark saves the second |
| Link | Ethernet (incl. VLAN tags), Linux cooked v1/v2, raw IP, BSD loopback |
| Network | IPv4 / IPv6 |
| Transport | TCP: payload, flags, seq/ack, window |
| MySQL | greeting (server version, connection id), login (user), `COM_QUERY` and the rest, OK / ERR / EOF, result sets (columns, rows, bytes), prepared statements, and replication (`COM_BINLOG_DUMP` + the binlog event stream) |
| TLS | records by type and version once a connection upgrades — see [Encrypted traffic](#encrypted-traffic) |
| DNS | queries and responses: name, type, answers, response code, and the lookup's latency |
| ARP | who-has / is-at, gratuitous announcements, and who claims which address |

MySQL packets are **reassembled across TCP segments**, so a 40 KB result set or a
statement split over three frames reads as one thing, reported on the frame that
completed it.

### Captures that start mid-connection

Most connections on a busy server are **older than the capture**. Without the
connection's SYN there is no way to know whether a server packet is an OK or the
middle of a result set, so those frames are labelled *"capture joined
mid-connection"* rather than decoded into something plausible-looking.

Two things then recover most of it automatically, because MySQL is strictly
request/response:

- a complete **client command** proves the server's next packet begins a response, and
- a completed **response** proves the client's next packet begins a command.

So a long-lived connection becomes fully readable one round-trip into the capture. An
**ERR packet** is decoded even before that — it carries its own marker byte, code and
SQLSTATE — which is how a `1205 Lock wait timeout` on a connection that has been open
for hours still shows up. A **binlog event stream** is likewise recognised by shape,
because a replica's connection essentially never starts inside a capture window.

## The Traffic Timeline

The **Packet Inspection** panel is sticky on a wide screen: it stays in place while the
packet list scrolls, so a frame selected from deep in a capture can still be read.

The density strip is bucketed **server-side** over the range you are looking at, so a
400 000-packet capture draws instantly and the browser only ever holds one page of
rows. Bars are coloured by the worst thing in the bucket: errors red, warnings amber,
plain query traffic accented, everything else grey.

A range can be set five ways, and they all drive the same query:

| Control | Use |
| --- | --- |
| **Drag on the strip** | Select a window visually. |
| **From / To packet #** | Exact packet numbers — the way to quote a range to somebody else. |
| **From / To +s** | Seconds into the capture, to millisecond precision. |
| **Zoom in / out · Pan ◀ ▶** | Halve, double, or slide the current window. |
| **last 1s / 5s / 10s · Whole capture** | Presets for "what just happened". |

**Resolution** sets the bucket count (40–640), which is how you tell a burst apart
from a steady rate.

Alongside them: filter by **connection**, **protocol**, **direction**, **issue kind**,
and a free-text **search** over the info line, the SQL and the addresses. The issue
chips in the Summary are clickable and do the same thing.

## Timestamps

Every packet carries the absolute time it was captured — epoch seconds with a
microsecond fraction, or nanoseconds for a `pcap-ns` capture — so the same instant can
be read four ways. The **Time** control above the packet list chooses which:

| Mode | Shows | Use |
| --- | --- | --- |
| **Seconds since capture start** (default) | `+5.500000` | how long after the start something happened — the usual way to read a 20-second window |
| **Time of day** | `19:19:01.501234` | comparing against a wall clock |
| **Date and time** | `2026-08-03 19:19:01.501234` | a capture read the next day, or one of several |
| **UTC (ISO)** | `2026-08-03 11:19:01.501234Z` | pasting next to a server error-log line, which is UTC |
| **Delta from previous row** | `+0.000181` | reading a gap between two adjacent packets |

The Summary card shows the capture's own date and window (`2026-08-03 19:19:01.501 →
19:19:21.502 · 20.001s`), the timeline's axis is labelled with real times as well as
offsets, and the detail panel gives all three forms for the selected frame — local, UTC
and offset. Hovering any time in the list shows the UTC form.

Times are rendered in the **viewer's own timezone**; the UTC form is there because the
server's log is not.

## Issues

Flagged per packet, counted in the summary, and filterable:

| Issue | Means |
| --- | --- |
| **TCP retransmission** | Bytes already seen were sent again — something dropped them. |
| **TCP gap — N bytes missing** | The capture never saw bytes the sequence numbers account for. |
| **TCP duplicate ACK** | The peer is re-acknowledging the same byte: loss being signalled. |
| **TCP zero window** | The receiver's buffer is full and it has told the sender to stop. |
| **TCP reset** | Someone hung up hard, rather than closing. |
| **High latency — N ms** | Time from a command to the first response byte, over 100 ms. |
| **Heavy result set** | A single answer over 1 MB. |
| **MySQL error N** | An ERR packet, with the operational ones named: `1045` auth failed, `1040` too many connections, `1205` lock wait timeout, `1213` deadlock, `1290`/`1836` read-only, `1129`/`1130` host blocked. |

Warnings reported in an OK packet are shown in the info line but **not** flagged —
MySQL raises them for entirely ordinary statements, and flagging them buries the real
problems.

## PXC / Galera: four ports, four protocols

A cluster member's traffic is not just 3306. Capturing a PXC (or MariaDB Galera) target
covers all four ports, because a capture of 3306 alone contains none of the replication
that makes a cluster interesting:

    tcpdump -i eth0 -s 65535 -n -q -c 50000 (port 3306 or port 4444 or port 4567 or port 4568) -w …

| Port | Carries | Shown as |
| --- | --- | --- |
| 3306 | the client/server protocol | `MySQL` — fully decoded |
| 4567 | **group communication** (gcs/gcomm): heartbeats, quorum votes and write-set replication, between every member and every other member, continuously. TCP, and UDP when multicast is configured | `Galera/GCS` |
| 4568 | **IST** — incremental state transfer: a rejoining member catching up from the donor's writeset cache. The cheap path | `Galera/IST` |
| 4444 | **SST** — state snapshot transfer: a full physical copy of the dataset from a donor, streamed by xtrabackup/xbstream (or rsync, or mysqldump). The expensive path | `Galera/SST` |

**Galera's wire formats are not decoded, deliberately.** gcomm's message layout is
internal to Galera and not a documented, stable protocol, and an SST is an opaque backup
stream — running the MySQL decoder over either would manufacture exactly the confident
nonsense this tool exists to avoid. What is reported is what the wire says for certain:
volume, direction, continuity, and for a state transfer its cumulative size and which
stream format it is (the xbstream magic, rsync's greeting, gzip/zstd, or SQL text).

Three Galera-specific events are flagged:

| Event | Means |
| --- | --- |
| **Galera IST started** | a member is catching up incrementally — the outcome you want |
| **Galera SST started (xbstream / xtrabackup)** | a donor is streaming its whole dataset; on a large database this is the difference between a quick rejoin and an hour |
| **Galera SST is large — N transferred** | raised once per connection past 1 MB, not once per frame |

Everything at the TCP layer applies to these ports unchanged, which is the point:
**retransmissions and gaps on 4567 are the classic cause of a cluster that keeps
evicting members**, and they are now attributed to the port that explains them.

**All-in-One PXC instances use their slot's ports**, not the defaults — several
instances share one container, so 4567 can only belong to one of them. The capture uses
that instance's group/IST/SST ports (`aioPortsFor`), and the Capture card lists exactly
which ports it covered.

## The traffic that is not the database

A capture on a database port is mostly database traffic, but the frames that are not are
often the explanation — and both of these were showing up as `ARP frame, 42 bytes` and
`UDP … 43 bytes` until a real 50 000-frame PXC capture made the point.

**DNS.** Every connection begins with a name lookup, and a lookup that fails means no
connection was ever attempted — so nothing on port 3306 can explain it:

| Event | Means |
| --- | --- |
| `DNS query A mysql-1.example.net` / `DNS response A … → 10.0.0.5 (2.0 ms)` | the normal case, with the latency every connection pays |
| **DNS NXDOMAIN / SERVFAIL / REFUSED** | the name did not resolve — what a client reports as `2005 CR_UNKNOWN_HOST` ("Unknown MySQL server host") |
| **DNS returned no A record** | the name resolves but has nothing to connect to. Only flagged for `A`, `SRV` and `PTR`: a resolver asks `A` and `AAAA` in parallel, so every IPv4-only host answers `AAAA` with NOERROR and no records, and flagging that produced a dozen meaningless issues on a real capture |
| **DNS query unanswered** | the resolver never replied; the client waits out its timeout |
| **Slow DNS response** | over 100 ms, which is added to every connection that resolves that name |

**ARP.** Layer 2, where a host either exists or does not:

| Event | Means |
| --- | --- |
| `ARP who-has 10.0.0.5? tell 10.0.0.9` / `10.0.0.5 is-at aa:bb:…` | the normal exchange |
| **Gratuitous ARP** | an address announcing itself — how a virtual IP says it has moved, which is the moment a cluster's clients get disconnected |
| **ARP conflict** | two MAC addresses claiming one IP: connections to it can reach either host. Usually a VIP mid-move, or two nodes configured with the same address |
| **ARP unanswered** | nothing replied for that address, so it is unreachable at layer 2 — which surfaces much later as a connect timeout |

## MySQL communication errors: where each one is visible

MySQL's network failures split three ways, and the split decides where you can find
them. Nothing below is inferred from the others.

**1. Server ERR packets — on the wire, named by the tool.** The server told the client,
so a capture has it. All of these produce an Issues entry with the label, the code and
the symbol:

| Code | Symbol | Shown as |
| --- | --- | --- |
| 1152 / 1184 | ER_ABORTING_CONNECTION / ER_NEW_ABORTING_CONNECTION | Aborted connection |
| 1153 | ER_NET_PACKET_TOO_LARGE | Packet bigger than max_allowed_packet |
| 1154 | ER_NET_READ_ERROR_FROM_PIPE | Read error from the connection pipe |
| 1155 | ER_NET_FCNTL_ERROR | fcntl() error on the connection |
| 1156 | ER_NET_PACKETS_OUT_OF_ORDER | Packets out of order |
| 1157 | ER_NET_UNCOMPRESS_ERROR | Could not uncompress a packet |
| 1158 / 1159 | ER_NET_READ_ERROR / ER_NET_READ_INTERRUPTED | Error / timeout reading communication packets |
| 1160 / 1161 | ER_NET_ERROR_ON_WRITE / ER_NET_WRITE_INTERRUPTED | Error / timeout writing communication packets |
| 1835 | ER_MALFORMED_PACKET | Malformed communication packet |
| 1189 / 1190 | ER_SOURCE_NET_READ / ER_SOURCE_NET_WRITE | Replication: net error reading/writing source |
| 1040 / 1203 | ER_CON_COUNT_ERROR / ER_TOO_MANY_USER_CONNECTIONS | Too many connections / user over max_user_connections |
| 1042 | ER_BAD_HOST_ERROR | Cannot resolve the client's address |
| 1043 | ER_HANDSHAKE_ERROR | Bad handshake |
| 1045 | ER_ACCESS_DENIED_ERROR | Authentication failed |
| 1047 | ER_UNKNOWN_COM_ERROR | Unknown command |
| 1053 | ER_SERVER_SHUTDOWN | Server shutdown in progress |
| 1129 / 1130 | ER_HOST_IS_BLOCKED / ER_HOST_NOT_PRIVILEGED | Host blocked / not allowed to connect |

Plus the contention and topology errors chased alongside them: `1205` lock wait
timeout, `1213` deadlock, `1290`/`1836` read-only, `1301` result truncated at
max_allowed_packet, `1317` query interrupted. Any code not in the list is still
reported, with its message.

**One code is read by its message as well as its number.** PXC reuses `1047`
(`ER_UNKNOWN_COM_ERROR`) for *"WSREP has not yet prepared node for application use"* — a
node that is joining, donating or desynced, refusing queries. That reads as **Node not
ready for application use (1047 wsrep)** rather than "Unknown command", because a real
capture held 69 of them and what they meant was that clients were being turned away by a
node that was not ready.

A network-class error **during the handshake** reads "Connection dropped during
handshake" rather than "Login refused" — 1156 and 1045 mean different things.

**2. Client-side codes (2xxx) — never on the wire; the evidence is.** These are the
client library's own diagnoses. A capture cannot contain them, so the tool flags what
the client saw instead:

| The client reports | The capture shows |
| --- | --- |
| `2003` CR_CONN_HOST_ERROR | **Connection refused** — a RST answering the SYN; or **Connection attempt unanswered** — a SYN nothing ever replied to (the client then waits out its connect timeout) |
| `2013` CR_SERVER_LOST | **Server closed the connection with COM_QUERY in flight** — a RST while a command was unanswered |
| `2006` CR_SERVER_GONE_ERROR | the same, with a FIN instead of a RST |
| `2026` CR_SSL_CONNECTION_ERROR / `2075` | **TLS alert** — the handshake or session was rejected |
| `2027` CR_MALFORMED_PACKET / server `1156` | **MySQL packet sequence expected N, got M** — a break in MySQL's own per-packet numbering |
| `2020` CR_NET_PACKET_TOO_LARGE | the server's `1153`, usually followed by a reset |
| `2065` / `2066` compression codes | **Compressed protocol negotiated** — see below |

Windows named-pipe and shared-memory codes (2016–2018, 2038–2046) are out of scope:
those transports are not TCP and never appear in a capture.

**Compressed connections.** If a client negotiates `CLIENT_COMPRESS` or
`CLIENT_ZSTD_COMPRESSION_ALGORITHM`, every packet is wrapped in a zlib/zstd frame and
nothing below it can be read — the same ceiling as TLS. The tool says so on the
handshake and marks the rest of the connection `MySQL/compressed`, rather than
emitting nonsense. (This is also where `1157` and `2065`/`2066` live.)

**3. Error-log records (MY-xxxxxx) — in the log only.** See the next section.

## The server error log

Aborted connections, DNS failures, TLS/certificate problems and listener errors are
written by the server to **its own log** and sent to nobody. No capture can contain
them, however long it runs — by the time the server writes "Aborted connection …
(Got an error reading communication packets)" there is no client left to tell.

So a ready capture also shows the error log, classified and narrowed to the capture's own
window (±30 s, because the server notices an abort a little after the packets that
explain it). It comes from one of two places:

- **A capture taken on a node** tails that node's own log — no setup, it just appears.
- **An uploaded capture** uses a log **uploaded alongside the pcap**: a capture from
  somebody else's server has no node to ask. The upload form takes the log as a second,
  optional file, and both `log_timestamps` forms are accepted — UTC (`…Z`) and SYSTEM
  (`…+08:00`), the latter resolved to the same instant so a log from a server in another
  timezone still lines up with the packets.

If a log is uploaded whose records **do not overlap the capture at all**, the panel says
so and shows both time ranges, rather than reporting "no events" — a log from the wrong
day or the wrong server is the mistake this pairing actually makes.

**The pane follows the packet list.** It sits under *Packet Inspection* in the right-hand
column, and selecting a packet scrolls it to the record nearest that packet in time and
highlights it — with the exact offset stated (`Nearest record to frame #1221: −0.200 s`),
because a note the server wrote two seconds later is related and one written an hour later
is not. Records within ±2 s of the selection are tinted; the closest one is ringed and
labelled *nearest*. When nothing is close, it says that instead of highlighting something
irrelevant. **follow selection** turns the scrolling off for reading the log on its own.

**And back again.** Clicking a log record sends the **packet list** to the moment it
describes: the server finds the page holding the packet nearest that instant, selects it,
and tints everything within ±2 s of the record. Your **range and filters are untouched** —
only the paging moves, and only packets those filters admit are candidates, because jumping
to a row the list is filtering out would misrepresent what you are looking at. If nothing
in the current range is near the record, it says so rather than jumping somewhere arbitrary.

Families recognised:

| Class | Records |
| --- | --- |
| **aborted** | MY-010914 / MY-013104 / MY-013130 "Aborted connection …", with the parenthesised reason pulled out: got an error/timeout reading or writing communication packets, packets out of order, closed without authentication, disconnected for inactivity |
| **auth** | Access denied, host blocked, too many connections |
| **dns** | MY-010055–MY-010058: address/hostname could not be resolved, reverse-DNS mismatches |
| **listener** | MY-010249–MY-010271: socket creation, bind, listen, port or Unix-socket already in use |
| **tls** | MY-013595, MY-015005–MY-015011, MY-010068: certificate cannot be opened, validation failed, self-signed CA |
| **replication** | error connecting/reconnecting to source, net error reading/writing source, COM_REGISTER_REPLICA failed, binlog dump request failed |

Anything else well-formed is passed through with its MY- code and level intact.

**Two things that will otherwise mislead you**, both surfaced in the panel:

- **`log_error_verbosity` below 3 means aborted connections are never logged at all.**
  The panel says so when it sees a lower value.
- **Even at verbosity 3, a note is only written when the disconnect produced a real
  read/write error.** A client that simply vanishes increments `Aborted_clients`
  without a line in the log. The panel therefore shows the counters —
  `Aborted_clients`, `Aborted_connects`, `Connection_errors_*` — next to the records,
  because on a live node those were 15 and 9 while the log held nothing.

## Encrypted traffic

A plaintext capture stops at the TLS record header, and this tool says so instead of
guessing: the stream is marked **TLS**, the handshake steps are named
(ClientHello, ServerHello, Certificate…), application data is reported by size, and no
SQL is invented.

That is the normal case, not an edge case:

- MySQL 8's client defaults to `--ssl-mode=PREFERRED`, so an ordinary interactive
  session is encrypted without anyone choosing it.
- `caching_sha2_password` — the default plugin — **refuses to send a password over an
  unencrypted channel**. `--ssl-mode=DISABLED` on its own fails with
  `ERROR 2061 (HY000): Authentication requires secure connection`.

### Reading an encrypted session

**This tool does not decrypt anything.** What it gives you for a TLS connection is real
and often enough — the handshake steps, record sizes, timing, response latency, and every
TCP-level problem underneath — but the statements are not available, and it says so rather
than guessing.

If you need the statements themselves, there are two ways to get them, neither of which
involves this capture:

1. **Take TLS off for the diagnostic window** and capture again. `--ssl-mode=DISABLED`
   needs an account whose plugin allows a cleartext password — `mysql_native_password` —
   or a client that fetches the server's RSA key (`--get-server-public-key`).
2. **Ask the server what it ran.** The general log, or
   `performance_schema.events_statements_history`, has the statements regardless of how
   they arrived; use this tool alongside it for the timing and the TCP behaviour, which are
   readable either way. For replication this is the only option, since both ends are
   `mysqld`.

## Uploads and downloads

- **Upload a capture** decodes a pcap you already have — from a production server, or a
  file a colleague sent. Same decoder, so it reads exactly like one taken here. Give it
  the server port if the traffic is not on 3306, and optionally **the server's error log
  covering the same period**, which is then correlated with the capture's window exactly
  as a node's own log would be (see [The server error log](#the-server-error-log)).
- **download .pcap** hands over the raw file. Everything shown here is derived from it, so
  when an analysis needs a tool this one does not have, that is the way to continue it.

## Sample captures

No capture files ship with the repository, but you can generate a set for checking the
tool without deploying anything. They are built byte-exactly by the decoder's own test
builder, so each one contains exactly the faults it advertises and nothing incidental:

```
PKT_SAMPLE_DIR=/tmp/pkt-samples go test -run TestWriteSampleCaptures ./app
```

| File | Contents |
| --- | --- |
| `mysql-tcp-trouble.pcap` | 15 packets: a retransmission, a duplicate ACK, a gap, a zero window, a reset, a high-latency response — every transport-level flag in one small file. |
| `mysql-midstream-join.pcap` | A connection already running when the capture began, whose first server payload starts with the byte an OK packet starts with. Must read *"capture joined mid-connection"* rather than being decoded; a later client command re-anchors the stream. |
| `mysql-tls-upgrade.pcap` | Greeting in the clear, `SSLRequest`, handshake records by name, then application data — where a plaintext capture stops being able to read a connection. |
| `net-arp-dns.pcap` | 13 packets of the traffic underneath a database problem that is not database traffic: a resolving name with its latency, an `AAAA` answering NOERROR-with-nothing (normal, deliberately not flagged), an NXDOMAIN, a 280 ms lookup, an ARP who-has/is-at pair, a gratuitous ARP, an address conflict, and an unanswered who-has. |
| `mysql-oversized-blob.pcap` | ~17 MB, because a MySQL packet only splits above `0xffffff`: a 16 MB+ row arriving as a `0xffffff` chunk plus a remainder across 11 600 segments. Must come back as one complete row with a heavy-result-set flag. |

`TestSampleCapturesDecode` decodes the same bytes in memory on every test run, so a
change that stops one of them demonstrating its case fails the suite whether or not the
files have been written.

Any pcap from anywhere decodes here as long as the traffic is MySQL — the upload form
takes a server port, which matters for a capture taken off a non-standard port (an
All-in-One instance is on 13000-something, never 3306). To capture by hand the way the
tool does:

```
tcpdump -i eth0 -s 65535 -n -q -c 50000 port 3306 -w /tmp/mysql.cap
```

Two things worth knowing before you do:

- **On a busy server the `-c` ceiling ends the capture in seconds.** Airline Sim's load
  hits 8 000 packets in about three.
- **Loss injected on the server's own egress is invisible to a server-side capture.**
  `netem` drops after tcpdump's tap point, so the dropped packet is never recorded and its
  retransmission looks like a first transmission. Inject on the *client* to see
  retransmissions and gaps at the server.

## Oversized packets and blobs

A MySQL packet carries at most `0xffffff` (16 MiB) bytes, so anything larger — a
LONGBLOB, a huge `IN (…)` list, a multi-megabyte result row — is split into a
16 MiB chunk plus a remainder. Those are reassembled, and the completed packet is
reported on the frame that finished it, with a **heavy result set** flag over 1 MB.

Two related events are named rather than left as generic errors:

| Event | What happened |
| --- | --- |
| `1153` Packet bigger than max_allowed_packet | The client sent more than the server's `max_allowed_packet` allows. The server answers with the error **and then resets the connection** — both are visible in the capture, one after the other. |
| `1301` Result truncated at max_allowed_packet | A server-side expression (a big `REPEAT()`, `GROUP_CONCAT`) produced more than the limit and was truncated. Nothing oversized crossed the wire in this case. |

A capture that ends mid-transfer leaves the tail of a large packet unassembled; those
frames read as `[continuation] N bytes, M buffered`, which is the honest answer — the
completion never arrived.

## Limits & notes

- Captures live **in memory** in the app process (the newest 12, like Query Runner's
  and Benchmark's runs) and do **not** survive a restart. Download anything you want
  to keep.
- Ceilings: 300 s per capture, 400 000 packets decoded, 192 MB per capture file. A
  capture that hits the decode limit says how many packets it skipped.
- `packets dropped by kernel` in the summary means the load outran tcpdump's buffer —
  shorten the capture or narrow the filter; the decode is still valid for what was
  captured.
- The **extra BPF filter** is ANDed with the port filter, so you can narrow to one
  peer (`host 10.0.0.7`) but not accidentally widen past the database port. **All
  ports** drops the port term when you need to see something else entirely.
- A **snaplen** below 65535 truncates payloads; the summary counts how many frames
  were cut short, and a truncated MySQL packet reads as a continuation.
