// Log Summary API — several nodes' server logs read as one classified timeline.
//
// Same conventions as lib/pktApi.js: same-origin JSON, cookies ride along, throws an
// Error with .status on non-2xx. And the same reason for the shared `range` object on
// every list call: the server filters and buckets, the browser only draws, so a bundle
// of a hundred thousand events stays responsive because the page never holds more than
// one screen of it.

async function toJSON(res) {
  const text = await res.text()
  let data = null
  if (text) { try { data = JSON.parse(text) } catch { data = null } }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  return toJSON(await fetch(path, opts))
}

// qs drops empty values so an untouched filter doesn't become `q=`.
function qs(params) {
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v === '' || v === null || v === undefined) continue
    if (k === 'src' && Number(v) < 0) continue
    if (Array.isArray(v)) {
      if (v.length) p.set(k, v.join(','))
      continue
    }
    p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export const logApi = {
  targets: () => request('GET', '/api/logsummary/targets'),
  list: () => request('GET', '/api/logsummary/bundles'),
  get: (id) => request('GET', `/api/logsummary/bundles/${id}`),
  remove: (id) => request('DELETE', `/api/logsummary/bundles/${id}`),
  // Several nodes in ONE request: three members' logs pulled seconds apart are three
  // views of the same cluster, and fetching them one at a time would make the comparison
  // the user's problem.
  collect: (body) => request('POST', '/api/logsummary/collect', body),
  events: (id, range) => request('GET', `/api/logsummary/bundles/${id}/events${qs(range)}`),
  timeline: (id, range) => request('GET', `/api/logsummary/bundles/${id}/timeline${qs(range)}`),
  at: (id, ts) => request('GET', `/api/logsummary/bundles/${id}/at?at=${ts}`),
  context: (id, src, line, ctx) =>
    request('GET', `/api/logsummary/bundles/${id}/sources/${src}/raw${qs({ line, context: ctx })}`),
  rawURL: (id, src) => `/api/logsummary/bundles/${id}/sources/${src}/raw`,
  setOffset: (id, src, offset) => request('POST', `/api/logsummary/bundles/${id}/offset`, { src, offset }),
  upload: async (files, engine, label) => {
    const fd = new FormData()
    for (const f of files) fd.append('files', f)
    if (engine) fd.append('engine', engine)
    if (label) fd.append('label', label)
    return toJSON(await fetch('/api/logsummary/upload', {
      method: 'POST', body: fd, credentials: 'same-origin',
    }))
  },
}

// --- severity ---------------------------------------------------------------
//
// Four levels, three of which are highlighted. They come from what a record MEANS rather
// than from the log's own level field, because on a Galera node the level says almost
// nothing: a complete crash, an eviction, a state transfer and a rejoin produced 314
// [Note] records and no [ERROR] at all.
//
// Colour is never the only signal. The palette (--status-ok / --status-warn /
// --status-crit) was validated for colour-vision deficiency — see the note in index.css —
// and every row that uses it also carries the word.

export const SEVS = ['bad', 'warn', 'ok', 'info']

export const SEV_LABEL = { bad: 'Bad', warn: 'Warning', ok: 'Good', info: 'Background' }

export const SEV_TEXT = {
  bad: 'text-status-crit', warn: 'text-status-warn', ok: 'text-status-ok', info: 'text-muted',
}

export const SEV_ROW = {
  bad: 'border-l-2 border-l-status-crit bg-status-crit/[0.07]',
  warn: 'border-l-2 border-l-status-warn bg-status-warn/[0.07]',
  ok: 'border-l-2 border-l-status-ok bg-status-ok/[0.05]',
  info: 'border-l-2 border-l-border',
}

export const SEV_CARD = {
  bad: 'border-status-crit/45 border-l-4 border-l-status-crit bg-status-crit/10',
  warn: 'border-status-warn/45 border-l-4 border-l-status-warn bg-status-warn/10',
  ok: 'border-status-ok/35 border-l-4 border-l-status-ok bg-status-ok/10',
  info: 'border-border border-l-4 border-l-muted bg-surface2/40',
}

// SEV_MARK is the non-colour half of the signal: a glyph that survives greyscale, a
// monochrome display and every form of colour blindness.
export const SEV_MARK = { bad: '✕', warn: '!', ok: '✓', info: '·' }

// Fills for the timeline. Solid rather than tinted, because a swimlane stripe has no text
// on it to carry the meaning and needs the full separation of the validated palette.
export const SEV_FILL = {
  bad: 'bg-status-crit', warn: 'bg-status-warn', ok: 'bg-status-ok', info: 'bg-fg/20',
}

// --- node identity ----------------------------------------------------------
//
// Which node, never how it is doing. A lane on the timeline is filled with a STATUS colour
// (is this node serving?) while the node itself needs an identity, and if the two channels
// shared a hue a reader would have to work out each time whether magenta meant "pxc03" or
// "bad". So these five come from the cool arc only — cyan, blue, periwinkle, violet,
// magenta — and red, amber and green stay reserved for status. The hexes, how they were
// chosen and the measured separations are in index.css beside the tokens.
//
// Colour is never the sole signal: the node's NAME is printed beside every chip. That is
// what makes it safe to stop at five and give a sixth source a neutral, rather than
// generating a hue no colour-blind reader could separate from the other five.
export const NODE_SLOTS = 5

// Solid fills for the chip. A dot, never the label text — text wears text tokens, so a
// colour that clears the 3:1 mark floor is never asked to carry 4.5:1 of type.
const NODE_FILL = ['bg-node-1', 'bg-node-2', 'bg-node-3', 'bg-node-4', 'bg-node-5']
const NODE_TINT = ['bg-node-1/12', 'bg-node-2/12', 'bg-node-3/12', 'bg-node-4/12', 'bg-node-5/12']
const NODE_EDGE = ['border-node-1', 'border-node-2', 'border-node-3', 'border-node-4', 'border-node-5']
// Written out at full strength AND at 40%, because Tailwind generates only the class
// strings it can see in the source. Composing `${nodeEdge(i)}/40` at runtime produces a
// class nothing ever emitted, and the border silently falls back to the default.
const NODE_EDGE_SOFT = ['border-node-1/40', 'border-node-2/40', 'border-node-3/40',
  'border-node-4/40', 'border-node-5/40']

// nodeFill / nodeTint / nodeEdge take a SOURCE INDEX, which is stable for the life of a
// bundle — so a node keeps its colour when the list is filtered, which "colour follows the
// entity, never its rank" requires. Past the fifth the colour channel gives up and says so.
export const nodeFill = (idx) => NODE_FILL[idx] ?? 'bg-muted'
export const nodeTint = (idx) => NODE_TINT[idx] ?? 'bg-muted/10'
export const nodeEdge = (idx) => NODE_EDGE[idx] ?? 'border-border'
export const nodeEdgeSoft = (idx) => NODE_EDGE_SOFT[idx] ?? 'border-border'
export const nodeHasColour = (idx) => idx >= 0 && idx < NODE_SLOTS

// --- classes ----------------------------------------------------------------

export const CLASS_LABEL = {
  startup: 'Start-up',
  shutdown: 'Shutdown',
  crash: 'Crash',
  membership: 'Membership',
  state: 'Node state',
  transfer: 'State transfer',
  network: 'Network',
  quorum: 'Quorum',
  flowcontrol: 'Flow control',
  replication: 'Replication',
  conflict: 'Conflicts',
  client: 'Client connections',
  storage: 'Storage',
  config: 'Configuration',
  security: 'Security',
  // A Kubernetes controller's classes. They are the operator's verbs, not a database's —
  // a database never restores itself or restarts its own peers in order.
  reconcile: 'Reconcile',
  backup: 'Backup',
  restore: 'Restore',
  rollout: 'Rolling restart',
  pitr: 'Point-in-time recovery',
  other: 'Other',
}

// --- member states ----------------------------------------------------------
//
// The vocabulary the swimlane is drawn in. Three state machines share it, because three
// kinds of server can appear in one bundle and the page's whole point is putting them on
// one timeline:
//
//   Galera            SYNCED JOINED JOINER DONOR PRIMARY OPEN CLOSED
//   Group Replication ONLINE RECOVERING BLOCKED ERROR OFFLINE
//   not in a cluster  RUNNING STARTING
//
// The words are deliberately each technology's own. ONLINE and SYNCED mean the same thing
// to a reader — caught up and serving — but somebody looking at a Group Replication member
// wants the word that appears in replication_group_members, not Galera's.
//
// Only the serving states are ok: everything else either refuses queries outright, or is
// deliberately out of the read pool, or (BLOCKED) answers reads while refusing every write,
// which is the case most likely to be mistaken for health.

export const STATE_TEXT = {
  // Galera
  SYNCED: 'Serving queries — up to date with the group',
  JOINED: 'Has the data, still applying its backlog — not in the read pool, and holding flow control on everyone else',
  JOINER: 'Receiving a state transfer — answers nothing',
  DONOR: 'Feeding a state transfer to another member, desynced while it does',
  'PRIMARY-COMP': 'In the primary component but not yet joined — a step on the way in',
  // PRIMARY is shared by MongoDB and PostgreSQL and means the same thing in both. Galera's
  // primary COMPONENT is a different idea and has its own spelling above, which is why the
  // backend renamed it rather than letting the two collide.
  PRIMARY: 'The one member accepting writes',
  OPEN: 'No primary component: refuses every query with 1047',
  CLOSED: 'The provider is shut down — not in the cluster',
  // Group Replication
  ONLINE: 'Serving queries — in the group and caught up',
  RECOVERING: 'In the group, still applying the backlog — it has the data, it is not yet usable',
  BLOCKED: 'Cannot see a majority: every write is refused until contact returns. Reads still succeed, and return stale data',
  ERROR: 'The plugin stopped on an error and the member removed itself from the group',
  OFFLINE: 'mysqld is up but Group Replication is not running on it — the server is not in its cluster, and its own log will not say so',
  // MongoDB
  SECONDARY: 'Applying the primary\'s oplog and serving reads',
  STARTUP2: 'Copying the whole dataset from another member — it serves nothing until that finishes',
  ROLLBACK: 'Discarding writes it had already accepted and acknowledged, because the rest of the set never agreed to them',
  ARBITER: 'Votes in elections and holds no data',
  REMOVED: 'Not in the replica-set configuration — running, and part of nothing',
  ROUTING: 'A mongos, up and routing — it holds no data of its own, so its lane says only that the router was running',
  // PostgreSQL
  STANDBY: 'Replaying the primary\'s WAL, answering reads, and refusing every write',
  PROMOTING: 'A promote was requested and has not finished — writes resume when it does',
  // Valkey. REPLICA is Valkey's own word for what MongoDB calls SECONDARY and PostgreSQL
  // calls STANDBY; CLUSTERDOWN has no counterpart in any of the others, and it is the one
  // worth reading the legend for — a node in it is completely healthy and answering nothing.
  REPLICA: 'Following a primary and answering reads — with no way to know from the log how far behind it is',
  SYNCING: 'Receiving a full copy of the primary\'s dataset — anything it answers meanwhile is stale by an unbounded amount',
  LOADING: 'Up and listening, and refusing every command with -LOADING while it reads its dataset off disk. A health check that only opens a socket sees a healthy node',
  CLUSTERDOWN: 'Up, healthy, and refusing every command because some OTHER shard\'s hash slots are uncovered. Nothing is wrong with this node',
  // The two Kubernetes sources. Neither is a database, and neither lane claims to say
  // anything about whether queries were being answered — the members' lanes do that.
  LEADER: 'The operator holds the leader lease — this is the process actually reconciling the cluster',
  'NOT-LEADER': 'The operator is running and does not hold the lease: it is watching, and changing nothing',
  COLLECTING: 'Binary logs are being uploaded — point-in-time recovery can reach the present',
  'PITR-GAP': 'The collector cannot continue its sequence: a binary log it needed is gone, and recovery cannot cross this point',
  'PITR-OFF': 'No binlog collector is running — from here, only a full backup can be restored',
  // The MongoDB operator's own cluster state, and its backup agents'. Exactly one agent
  // per replica set does any work; the others are SLICING's counterpart, PBM-IDLE, and
  // that is normal rather than a fault.
  'CR-READY': 'The operator considers the cluster to match its spec',
  'CR-INIT': 'The operator is changing the cluster — ordinary during a rollout, and a cluster that never leaves it is stuck',
  'CR-ERROR': 'The operator could not bring the cluster to its spec',
  SLICING: 'This member holds the PITR lock and is streaming the oplog to object storage — the only one of its replica set that is',
  'PBM-IDLE': 'The agent is up and another member won the nomination: normal, and its log will say almost nothing',
  'PBM-LOST': 'The agent cannot reach the cluster, so it can neither slice the oplog nor record that it failed to',
  // CloudNativePG runs the failover itself — no Patroni — so it is the only one of the
  // three PostgreSQL operators whose log can date a switchover.
  SWITCHOVER: 'CloudNativePG is moving the primary: no member is accepting writes until it finishes',
  MANAGING: 'The instance manager is up and looking after its member',
  // Not a cluster member at all
  RUNNING: 'Up and accepting connections',
  STARTING: 'Starting up — not accepting connections yet',
  // Neither stated nor deducible
  DOWN: 'The server is not running',
  UNKNOWN: 'The log does not say — nothing in this window states or implies a state',
}

export const STATE_SEV = {
  SYNCED: 'ok', ONLINE: 'ok', RUNNING: 'ok',
  PRIMARY: 'ok', SECONDARY: 'ok', STANDBY: 'ok', ROUTING: 'ok', REPLICA: 'ok',
  JOINED: 'warn', JOINER: 'warn', DONOR: 'warn', 'PRIMARY-COMP': 'warn',
  RECOVERING: 'warn', STARTING: 'warn', STARTUP2: 'warn', ARBITER: 'warn', PROMOTING: 'warn',
  SYNCING: 'warn', LOADING: 'warn',
  OPEN: 'bad', CLOSED: 'bad', DOWN: 'bad',
  BLOCKED: 'bad', ERROR: 'bad', OFFLINE: 'bad', ROLLBACK: 'bad', REMOVED: 'bad',
  CLUSTERDOWN: 'bad',
  LEADER: 'ok', COLLECTING: 'ok',
  'NOT-LEADER': 'warn',
  'PITR-GAP': 'bad', 'PITR-OFF': 'bad',
  'CR-READY': 'ok', SLICING: 'ok',
  'CR-INIT': 'warn', 'PBM-IDLE': 'warn',
  'CR-ERROR': 'bad', 'PBM-LOST': 'bad',
  MANAGING: 'ok', SWITCHOVER: 'warn',
  UNKNOWN: 'info',
}

// --- time -------------------------------------------------------------------

const pad = (n, w = 2) => String(n).padStart(w, '0')

export function logTimeOfDay(ts, decimals = 3) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  const frac = String(Math.floor((ts % 1) * 1e6)).padStart(6, '0').slice(0, decimals)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}${decimals ? '.' + frac : ''}`
}

// logStamp is a moment on the timeline: the date AND the time.
//
// Time alone was the default here and it was wrong for the case this page is for. A bundle
// is several nodes' logs read together, and those routinely span days — a collect of 200,000
// lines from a quiet server reaches back a week, and an uploaded pair of files can be from
// different days entirely. "15:20:06" in that bundle names three different moments and the
// reader cannot tell which. The month is spelled rather than numbered because 08-09 is the
// 8th of September to half the world and the 9th of August to the other half.
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
export function logStamp(ts, decimals = 3) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return `${pad(d.getDate())} ${MONTHS[d.getMonth()]} ${logTimeOfDay(ts, decimals)}`
}

export function logDateTime(ts, decimals = 3) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${logTimeOfDay(ts, decimals)}`
}

// logISO is the unambiguous form — what to paste into a ticket beside somebody else's log.
export function logISO(ts) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return `${d.toISOString().slice(0, 19).replace('T', ' ')}Z`
}

// logDur renders a span the way an operator reads one.
export function logDur(sec) {
  if (!isFinite(sec) || sec <= 0) return '0s'
  if (sec < 1) return `${Math.round(sec * 1000)} ms`
  if (sec < 90) return `${sec.toFixed(1)}s`
  if (sec < 5400) return `${(sec / 60).toFixed(1)} min`
  return `${(sec / 3600).toFixed(1)} h`
}

export function logBytes(n) {
  if (n === null || n === undefined) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

// FLAVOUR_LABEL names what KIND of member a source is, beside its node chip.
//
// The engine is not enough: three PostgreSQL servers can be a standalone, a streaming
// standby and a Patroni member, and which one decides what the page is entitled to say
// about it. Only the flavours worth distinguishing from their engine are listed — a plain
// mysqld or a standalone mongod gets no badge, because "MySQL" already said it.
export const FLAVOUR_LABEL = {
  galera: 'Galera member',
  grouprepl: 'Group Replication member',
  innodbcluster: 'InnoDB Cluster member',
  mongors: 'replica-set member',
  mongos: 'mongos router',
  pgstream: 'streaming replication',
  patroni: 'Patroni member',
  valkeyrepl: 'Valkey replication',
  valkeycluster: 'Valkey Cluster member',
  pxcoperator: 'PXC operator',
  pxcpitr: 'binlog collector',
  psmdboperator: 'MongoDB operator',
  pbmagent: 'backup agent',
  perconapgoperator: 'Percona PG operator',
  crunchypgo: 'Crunchy PGO',
  cnpgoperator: 'CloudNativePG operator',
  cnpginstance: 'CloudNativePG instance',
  psoperator: 'Percona Server operator',
  k8sevents: 'Kubernetes Events',
}

export const ENGINE_LABEL = {
  mysql: 'MySQL / PXC', postgres: 'PostgreSQL', mongodb: 'MongoDB', valkey: 'Valkey',
  // Not a database, and spelled so. A Kubernetes controller has no port, no query
  // language and no data of its own; filing it under MySQL because it manages MySQL
  // would make the sources table claim something about it that is not true.
  operator: 'Kubernetes operator',
  // Not a log at all: an API object list. Spelled distinctly so the sources table does
  // not imply somebody tailed a file.
  k8sevents: 'Kubernetes API',
}
