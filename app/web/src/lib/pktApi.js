// Packet Inspector API wrapper. Same conventions as lib/queryrunApi.js: same-origin
// JSON, cookies ride along, throws Error with .status on non-2xx.
//
// Every list call takes the same `range` object, which is what makes the timeline
// configurable: the server filters and buckets, the browser only draws. A range is
// {fromNo,toNo,fromTs,toTs,stream,proto,dir,issue,q} — all optional.

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) {
    try { data = JSON.parse(text) } catch { data = null }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

// qs drops empty values so a blank filter box doesn't become `q=`.
function qs(params) {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v === '' || v === null || v === undefined) continue
    if (k === 'stream' && Number(v) < 0) continue
    p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const pktApi = {
  targets: () => request('GET', '/api/pktinspect/targets'),
  list: () => request('GET', '/api/pktinspect/captures'),
  start: (body) => request('POST', '/api/pktinspect/captures', body),
  get: (id) => request('GET', `/api/pktinspect/captures/${id}`),
  stop: (id) => request('POST', `/api/pktinspect/captures/${id}/stop`),
  packets: (id, range) => request('GET', `/api/pktinspect/captures/${id}/packets${qs(range)}`),
  packet: (id, no) => request('GET', `/api/pktinspect/captures/${id}/packets/${no}`),
  timeline: (id, range) => request('GET', `/api/pktinspect/captures/${id}/timeline${qs(range)}`),
  downloadURL: (id) => `/api/pktinspect/captures/${id}/download`,
  // The error-log side: records a capture cannot contain by construction (aborted
  // connections, DNS, TLS, listener), narrowed to the capture's own window.
  serverLog: (id, opts) => request('GET', `/api/pktinspect/captures/${id}/serverlog${qs(opts)}`),
  // An upload may carry the server's error log beside the pcap: an uploaded capture has
  // no node to read one from, and the log holds the events a capture cannot contain.
  // engine is '' for "work it out from the bytes", which is the default.
  upload: async (file, port, logFile, engine) => {
    const fd = new FormData()
    fd.append('file', file)
    if (logFile) fd.append('log', logFile)
    if (port) fd.append('port', String(port))
    if (engine) fd.append('engine', engine)
    const res = await fetch('/api/pktinspect/upload', { method: 'POST', body: fd, credentials: 'same-origin' })
    const text = await res.text()
    let data = null
    try { data = JSON.parse(text) } catch { /* non-JSON error body */ }
    if (!res.ok) {
      const err = new Error((data && data.error) || `Upload failed (${res.status})`)
      err.status = res.status
      throw err
    }
    return data
  },
}

export const pktTargetKey = (t) => `${t.stackId}:${t.nodeId}`

// --- formatting ------------------------------------------------------------

// A packet's timestamp comes off the wire as epoch seconds with a fraction (µs, or ns
// for a nanosecond-precision capture), so all of these are views of the same number —
// what changed is that the UI used to show only the relative one and never a date.

const pad = (n, w = 2) => String(n).padStart(w, '0')

// pktTimeOfDay is HH:MM:SS.mmmmmm in the viewer's own timezone.
export function pktTimeOfDay(ts, decimals = 6) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  const frac = String(Math.floor((ts % 1) * 1e6)).padStart(6, '0').slice(0, decimals)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}${decimals ? '.' + frac : ''}`
}

// pktDateTime is the full local date and time — the answer to "when was this capture
// taken", which a relative offset can never give.
export function pktDateTime(ts, decimals = 6) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pktTimeOfDay(ts, decimals)}`
}

// pktISO is the unambiguous form, UTC with an explicit offset — what to paste into a
// ticket next to a server's own log line.
export function pktISO(ts) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  const frac = String(Math.floor((ts % 1) * 1e6)).padStart(6, '0')
  return `${d.toISOString().slice(0, 19).replace('T', ' ')}.${frac}Z`
}

// Kept for callers that want the old short form.
export const pktTime = (ts) => pktTimeOfDay(ts, 3)

// TIME_MODES are the ways the packet list can label a row, the same choice Wireshark
// offers under View → Time Display Format. Relative is the default because a 20-second
// capture is usually read as "how long after the start"; the others exist because a
// capture also has to be correlated with a server log, which needs a date.
export const TIME_MODES = [
  { v: 'relative', label: 'Seconds since capture start' },
  { v: 'clock', label: 'Time of day' },
  { v: 'datetime', label: 'Date and time' },
  { v: 'utc', label: 'UTC (ISO)' },
  { v: 'delta', label: 'Delta from previous row' },
]

// pktFormatTime renders one row's time in the chosen mode. prev is the previous row's
// timestamp, used only by the delta mode.
export function pktFormatTime(mode, ts, first, prev) {
  switch (mode) {
    case 'clock':
      return pktTimeOfDay(ts)
    case 'datetime':
      return pktDateTime(ts)
    case 'utc':
      return pktISO(ts)
    case 'delta':
      return prev ? `+${(ts - prev).toFixed(6)}` : '—'
    default:
      return `+${(ts - first).toFixed(6)}`
  }
}

export function pktBytesFmt(n) {
  if (n === null || n === undefined) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

// PROTO_TONE maps a protocol to a Badge tone, so the list reads at a glance:
// decoded MySQL is the signal, TLS is the "can't see inside this" case.
export const PROTO_TONE = {
  MySQL: 'primary',
  'MySQL/compressed': 'warning',
  PostgreSQL: 'primary',
  TLS: 'warning',
  // PXC's cluster ports: a different protocol on each, none of them MySQL.
  'Galera/GCS': 'success',
  'Galera/IST': 'primary',
  'Galera/SST': 'warning',
  'Galera/GCS/UDP': 'success',
  // A Patroni member's cluster ports. Patroni's REST API and etcd are not the
  // PostgreSQL protocol, and its replication is — it rides port 5432 like any other
  // connection, so there is no separate label for it.
  'Patroni/REST': 'success',
  'etcd/client': 'success',
  'etcd/raft': 'accent',
  // MongoDB puts everything on 27017, so the label carries the connection's KIND —
  // decided from what is in it, not from which port it arrived on. On a real member most
  // rows are cluster chatter rather than the application.
  MongoDB: 'primary',
  'MongoDB/oplog': 'primary',
  'MongoDB/routed': 'primary',
  'MongoDB/heartbeat': 'success',
  'MongoDB/replpos': 'success',
  'MongoDB/monitor': 'muted',
  'MongoDB/config': 'accent',
  'MongoDB/election': 'warning',
  'MongoDB/internal': 'accent',
  // Valkey: RESP on the client port (which also carries replication), and a binary
  // gossip bus on the client port + 10000.
  Valkey: 'primary',
  'Valkey/replication': 'primary',
  'Valkey/pubsub': 'accent',
  'Valkey/monitor': 'muted',
  'Valkey/bus': 'success',
  'Valkey/sentinel': 'success',
  // The traffic underneath the database: name resolution and layer 2.
  DNS: 'accent',
  ARP: 'accent',
  TCP: 'muted',
  UDP: 'muted',
  ICMP: 'warning',
}

// PORT_ROLE_TEXT explains a captured port in one line, for the Capture card.
export const PORT_ROLE_TEXT = {
  mysql: 'client/server protocol',
  'galera-gcs': 'Galera group communication — heartbeats, quorum, write-sets',
  'galera-ist': 'Galera IST — incremental catch-up from a donor',
  'galera-sst': 'Galera SST — full dataset copy from a donor',
  postgres: 'PostgreSQL frontend/backend protocol — client sessions and WAL streaming',
  valkey: 'RESP: client commands and replication, which share this port',
  'valkey-bus': 'the cluster bus: binary gossip — heartbeats, failure detection and failover votes',
  'valkey-sentinel': 'Sentinel: monitoring and failover for a non-clustered primary/replica pair',
  mongodb: 'the MongoDB wire protocol — clients, heartbeats, oplog tailing and routing all share this port',
  'patroni-rest': "Patroni's REST API — HAProxy's health checks and patronictl",
  'etcd-client': 'etcd client API — where the Patroni leader lock is taken and renewed',
  'etcd-peer': 'etcd peer traffic — raft heartbeats between the etcd members',
}

// ENGINE_LABEL names the protocol a capture was decoded as. Shown because an upload
// may have been decided by the sniffer rather than chosen.
export const ENGINE_LABEL = {
  mysql: 'MySQL', postgres: 'PostgreSQL', mongodb: 'MongoDB', valkey: 'Valkey',
}

// MONGO_KIND_TEXT explains the connection kinds MongoDB multiplexes onto one port. It is
// the analogue of PORT_ROLE_TEXT, keyed by kind instead of by port, because for MongoDB
// there is no second port to explain — mongod, mongos and the config servers all listen
// on 27017 and the difference is in the messages.
export const MONGO_KIND_TEXT = {
  client: 'an application connection',
  monitor: 'hello/isMaster monitoring — a driver or a member watching the topology',
  heartbeat: 'replica-set heartbeats: every member checks every other every 2 seconds, forever',
  election: 'an election — the seconds in which the primary changes',
  oplog: 'oplog tailing: a secondary reading local.oplog.rs — this IS MongoDB replication',
  replpos: 'replSetUpdatePosition: how far secondaries have applied, which is what write concern waits on',
  config: "config database traffic: the sharded cluster's routing table",
  routed: 'mongos → shard: a routed command carrying the shard version that decides whether it is answered',
  internal: 'internal cluster traffic, authenticated as __system',
}

// SEVERE marks the issue kinds drawn as errors rather than warnings — the same split
// the server applies when bucketing the timeline (pktSevereIssues in Go, which is
// generated from pktErrCatalog). Matched as substrings, so "Aborted connection (1152
// ER_ABORTING_CONNECTION)" and "Packets out of order (1156 …)" both land here.
const SEVERE = [
  // transport
  'TCP retransmission', 'TCP reset', 'TCP gap', 'TCP zero window', 'TCP duplicate ACK',
  'unreachable',
  // what the client would report (2003 / 2006 / 2013 / 2026 / 2027)
  'Connection refused', 'Connection attempt unanswered', 'Server closed the connection',
  'TLS alert', 'MySQL packet sequence', 'Compressed protocol',
  // server-side communication + handshake errors
  'Aborted connection', 'Packet bigger than max_allowed_packet', 'Packets out of order',
  'Error reading communication packets', 'Error writing communication packets',
  'Timeout reading communication packets', 'Timeout writing communication packets',
  'Could not uncompress', 'Malformed communication packet', 'Read error from the connection pipe',
  'Authentication failed', 'Bad handshake', 'Host blocked', 'Host not allowed',
  'Too many connections', 'max_user_connections', 'Server shutdown in progress',
  'Cannot resolve the client', 'Unknown command',
  // contention / topology
  'Deadlock detected', 'Lock wait timeout', 'Read-only server', 'Replication:',
  'Node not ready for application use',
  // ARP / DNS: a name that will not resolve or an address nothing answers for is why a
  // connection never happened at all.
  'DNS NXDOMAIN', 'DNS SERVFAIL', 'DNS REFUSED', 'DNS FORMERR', 'DNS query unanswered',
  'DNS returned no', 'ARP unanswered', 'ARP conflict',
  // PostgreSQL. The wording comes from pgErrCatalog and pktpg.go, and the same rule
  // applies as above: something wrong with the server, the cluster or the connection —
  // never an ordinary SQL mistake, so no unique violation or syntax error here.
  'FATAL', 'PANIC', 'Protocol violation', 'Connection failure', 'Connection exception',
  'Authorisation failed', 'Password authentication failed', 'No pg_hba.conf entry',
  'The database named at connect time does not exist', 'The role is not permitted to log in',
  'Disk full', 'Out of memory', 'A configuration limit was hit',
  'Lock not available', 'The object is in use elsewhere', 'The object is not in a state',
  'Query cancelled', 'Statement cancelled', 'Serialisation failure',
  'Administrator shutdown', 'Crash shutdown', 'The server cannot accept connections yet',
  'A write was attempted on a read-only connection', 'The transaction has already failed',
  'Replication lag', 'The primary is asking the standby',
  'Requested WAL segment', 'Internal error', 'Data corruption', 'Index corruption',
  'I/O error', 'Cleartext password authentication', 'SSL refused by the server',
  'Unrecognised protocol version', 'Patroni REST returned', 'etcd answered',
  // MongoDB. Every one of these comes from mongoErrCatalog or pktmongorepl.go, and the
  // same rule holds: something wrong with the server, the cluster or the connection.
  // DuplicateKey, NamespaceNotFound and CommandNotFound are deliberately absent — a
  // unique index doing its job and a driver probing for optional commands are not faults.
  'NotWritablePrimary', 'PrimarySteppedDown', 'InterruptedDueToReplStateChange',
  'NotPrimaryNoSecondaryOk', 'NotPrimaryOrSecondary', 'FailedToSatisfyReadPreference',
  'ShutdownInProgress', 'HostUnreachable', 'HostNotFound', 'NetworkTimeout',
  'SocketException', 'AuthenticationFailed', 'Unauthorized', 'UserNotFound',
  'WriteConcernFailed', 'UnknownReplWriteConcern', 'UnsatisfiableWriteConcern',
  'WriteConflict', 'LockTimeout', 'NoSuchTransaction', 'TransactionTooOld',
  'StaleConfig', 'StaleShardVersion', 'CursorNotFound', 'CursorKilled',
  'MaxTimeMSExpired', 'ExceededTimeLimit', 'QueryExceededMemoryLimit', 'OutOfDiskSpace',
  'Election in progress', 'replSetStepDown', 'replSetStepUp', 'Chunk migration',
  'Replica-set configuration change', 'Write concern not satisfied',
  'legacy opcode removed', 'legacy read path',
  // Valkey. From valkeyErrCatalog and pktvalkey.go/pktvalkeybus.go. WRONGTYPE, NOSCRIPT
  // and a plain ERR are absent on purpose: those are the application's business.
  'MOVED', 'ASK ', 'TRYAGAIN', 'CROSSSLOT', 'CLUSTERDOWN', 'MASTERDOWN',
  'LOADING', 'MISCONF', 'READONLY', 'OOM', 'NOREPLICAS',
  'NOAUTH', 'WRONGPASS', 'NOPERM', 'BUSY', 'UNKILLABLE',
  'FULLRESYNC', 'Replication lag', 'KEYS *', 'FLUSHALL', 'FLUSHDB', 'DEBUG SLEEP',
  'SHUTDOWN', 'SAVE —', 'SWAPDB', 'SCRIPT FLUSH', 'CONFIG SET',
  'MONITOR —', 'FAILOVER —', 'CLUSTER FAILOVER', 'FAIL message', 'MEET from',
  'FAILOVER_AUTH', 'Cluster epoch rose', 'Inline command', 'maxclients reached',
  'Protocol error', 'AUTH on an unencrypted connection', 'Client-side cache invalidation',
]

export const isSevereIssue = (s) => SEVERE.some((k) => (s || '').includes(k))

// issueKind mirrors pktIssueKind in Go: the part of the message before the detail,
// which is what the summary counts and the filter matches on.
export function issueKind(s) {
  const em = (s || '').indexOf(' — ')
  if (em > 0) return s.slice(0, em)
  const colon = (s || '').indexOf(':')
  if (colon > 0) return s.slice(0, colon)
  const paren = (s || '').indexOf(' (')
  if (paren > 0) return s.slice(0, paren)
  return s || ''
}
