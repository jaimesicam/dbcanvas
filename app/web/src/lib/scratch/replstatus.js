// replstatus.js — the Replication status card: a plain verdict from what a replica or a replica
// set reports about itself.
//
// Reads MySQL `SHOW REPLICA STATUS\G` / `SHOW SLAVE STATUS\G` (vertical or table form, every
// channel), MongoDB `rs.status()` (as mongosh prints it, or as JSON), and PostgreSQL
// `pg_stat_replication` (on a primary) and `pg_stat_wal_receiver` (on a standby), as psql prints
// them aligned or expanded.
//
// readReplication(text) returns { engine, summary, items, findings } where each item is one
// channel / member / standby: { id, title, status: 'ok' | 'warning' | 'error', facts: [[k, v]] },
// and findings are [{ severity: 'error' | 'warning' | 'info', item, text }], worst first.

import { readTables, readShellDoc, Ts } from './shellout.js'
import { parseGtidSet, subtract, count, formatGtidSet } from './gtid.js'
import { human } from './units.js'

const RANK = { error: 0, warning: 1, info: 2 }

// Lag thresholds, in seconds and bytes: past the first a replica is behind, past the second it is
// far behind.
export const LAG_WARN = 30
export const LAG_ERROR = 300
const BYTES_WARN = 16 * 2 ** 20 // one WAL segment
const BYTES_ERROR = 2 ** 30

export function readReplication(text) {
  const t = String(text ?? '')
  if (!t.trim()) return { engine: null, items: [], findings: [] }
  let r
  if (/\b(Replica|Slave)_IO_Running\b|\b(Source|Master)_Host\b/.test(t)) r = mysql(t)
  else if (/\bmembers\b/.test(t) && /\bstateStr\b|\boptimeDate\b|"\$?set"|\bset\s*:/.test(t)) r = mongo(t)
  else if (/\bok\b.*\b0\b/.test(t) && /\b(errmsg|codeName)\b/.test(t)) r = mongo(t)
  else if (/\b(sent_lsn|replay_lsn|sent_location|replay_location|sync_state|flushed_lsn|receive_start_lsn|sender_host|received_lsn|written_lsn)\b/.test(t)) r = postgres(t)
  else return { error: 'Not recognised. Paste SHOW REPLICA STATUS\\G, rs.status(), or SELECT * FROM pg_stat_replication / pg_stat_wal_receiver.' }
  if (r.error) return r
  r.findings.sort((a, b) => RANK[a.severity] - RANK[b.severity])
  for (const it of r.items) {
    const own = r.findings.filter((f) => f.item === it.id)
    it.status = own.some((f) => f.severity === 'error') ? 'error' : own.some((f) => f.severity === 'warning') ? 'warning' : 'ok'
  }
  return r
}

// fmtSeconds writes a duration the way a person reads it: 45 s, 12 min 5 s, 3 h 2 min, 2 d 4 h.
export function fmtSeconds(s) {
  if (s === null || s === undefined || Number.isNaN(s)) return '—'
  const neg = s < 0
  let x = Math.abs(s)
  let out
  if (x < 1) out = `${Math.round(x * 1000)} ms`
  else if (x < 60) out = `${Math.round(x * 10) / 10} s`
  else if (x < 3600) out = `${Math.floor(x / 60)} min ${Math.round(x % 60)} s`
  else if (x < 86400) out = `${Math.floor(x / 3600)} h ${Math.floor((x % 3600) / 60)} min`
  else { x = Math.round(x / 3600); out = `${Math.floor(x / 24)} d ${x % 24} h` }
  return neg ? `−${out}` : out
}

const fmtBytes = (b) => (b === null || b === undefined ? '—' : Math.abs(b) < 1024 ? `${b} B` : `${human(Number(b))} (${b.toLocaleString('en-US')} B)`)

// ------------------------------------------------------------------ MySQL

// Old and new names: Master → Source, Slave → Replica (8.0.22+ prints the new ones).
const renameMy = (k) => k.replace(/Master/g, 'Source').replace(/Slave/g, 'Replica')

// What a replication error number usually means.
export const MYSQL_ERRORS = {
  1007: 'the database already exists on the replica — the schemas have drifted',
  1008: 'the database does not exist on the replica — the schemas have drifted',
  1045: 'access denied for the replication user — check its password and its host in mysql.user',
  1050: 'the table already exists on the replica — the schemas have drifted',
  1051: 'the table does not exist on the replica — the schemas have drifted',
  1062: 'duplicate key on the replica — the data has drifted, or something writes to the replica',
  1032: 'the row to change is not on the replica — the data has drifted',
  1146: 'the table does not exist on the replica',
  1205: 'lock wait timeout in the applier — something on the replica holds the rows',
  1236: 'the source cannot send the binary log: a log the replica needs was purged, or the replica has transactions the source never had',
  13114: 'the source cannot send the binary log: a log the replica needs was purged, or the replica has transactions the source never had',
  1593: 'fatal replica error — often the replica has the same server_id or server_uuid as the source',
  1594: 'the relay log cannot be read (corrupt or missing relay log)',
  1677: 'a column cannot be converted — the table definitions differ between source and replica',
  2003: 'cannot connect to the source host and port',
  2005: 'the source host name does not resolve',
  2026: 'the TLS handshake with the source failed',
  2061: 'caching_sha2_password needs TLS or the source public key (SOURCE_SSL=1 or GET_SOURCE_PUBLIC_KEY=1)',
}

function mysql(t) {
  const rows = readTables(t)
    .flatMap((tb) => tb.rows)
    .map((r) => Object.fromEntries(Object.entries(r).map(([k, v]) => [renameMy(k), v])))
    .filter((r) => 'Replica_IO_Running' in r || 'Source_Host' in r)
  if (!rows.length) return { error: 'No replica status rows found — paste the whole SHOW REPLICA STATUS\\G output.' }
  const items = []
  const findings = []
  rows.forEach((r, n) => {
    const id = `ch${n}`
    const channel = r.Channel_Name || ''
    const add = (severity, text) => findings.push({ severity, item: id, text: rows.length > 1 || channel ? `${channel ? `channel "${channel}"` : 'default channel'}: ${text}` : text })
    const io = r.Replica_IO_Running || '?'
    const sql = r.Replica_SQL_Running || '?'
    const src = `${r.Source_Host || '?'}:${r.Source_Port || '?'}`
    const facts = []
    facts.push(['Source', `${src}${r.Source_User ? ` as ${r.Source_User}` : ''}${r.Source_UUID ? ` (uuid ${r.Source_UUID})` : ''}`])
    facts.push(['IO thread', `${io}${r.Replica_IO_State ? ` — ${r.Replica_IO_State}` : ''}`])
    facts.push(['SQL thread', `${sql}${r.Replica_SQL_Running_State ? ` — ${r.Replica_SQL_Running_State}` : ''}`])

    const lagRaw = r.Seconds_Behind_Source
    const lag = lagRaw === undefined || /^null$/i.test(lagRaw) || lagRaw === '' ? null : Number(lagRaw)
    const delay = Number(r.SQL_Delay || 0)
    facts.push(['Lag', lag === null ? 'unknown (NULL — a thread is not running)' : `${fmtSeconds(lag)}${delay ? ` (delayed on purpose by ${fmtSeconds(delay)})` : ''}`])

    // Binary log positions: how far the IO thread has read and the SQL thread has applied.
    if (r.Source_Log_File) {
      const sameFile = r.Source_Log_File === r.Relay_Source_Log_File
      const backlog = sameFile ? Number(r.Read_Source_Log_Pos) - Number(r.Exec_Source_Log_Pos) : null
      facts.push(['Read up to', `${r.Source_Log_File}:${r.Read_Source_Log_Pos}`])
      facts.push(['Applied up to', `${r.Relay_Source_Log_File}:${r.Exec_Source_Log_Pos}${backlog !== null ? ` (${backlog === 0 ? 'nothing waiting' : `${fmtBytes(backlog)} of binlog waiting`})` : ' (an older file — at least one whole binlog waiting)'}`])
    }

    const sslAllowed = r.Source_SSL_Allowed
    if (sslAllowed !== undefined) {
      const verify = r.Source_SSL_Verify_Server_Cert
      facts.push(['TLS', sslAllowed === 'Yes' ? `yes${r.Source_SSL_CA_File ? `, CA ${r.Source_SSL_CA_File}` : ''}${verify === 'Yes' ? ', host name verified' : ', host name not verified'}` : sslAllowed === 'Ignored' ? 'requested but this replica has no TLS support' : 'not configured (SOURCE_SSL=0)'])
      if (sslAllowed !== 'Yes') add('info', `TLS is not configured for the connection to ${src} (Source_SSL_Allowed: ${sslAllowed}).`)
    }
    const autoPos = r.Auto_Position
    if (autoPos !== undefined) facts.push(['Auto-position', autoPos === '1' ? 'on (GTID)' : 'off (file and position)'])

    // GTIDs: retrieved by the IO thread but not yet executed.
    let gtid = null
    const retrieved = (r.Retrieved_Gtid_Set || '').trim()
    const executed = (r.Executed_Gtid_Set || '').trim()
    if (retrieved || executed) {
      const pr = parseGtidSet(retrieved)
      const pe = parseGtidSet(executed)
      if (!pr.error && !pe.error) {
        const pending = subtract(pr.set, pe.set)
        const n2 = count(pending)
        gtid = { retrieved, executed, pending: formatGtidSet(pending), pendingCount: n2.toString() }
        facts.push(['GTIDs waiting', n2 === 0n ? 'none — everything retrieved is executed' : `${n2} transaction(s) retrieved, not yet executed`])
        if (n2 > 0n) add(sql === 'Yes' ? 'info' : 'warning', `${n2} retrieved transaction(s) not yet executed: ${formatGtidSet(pending, ', ')}.`)
      }
      if (autoPos === '0' && executed) add('info', 'GTIDs are in use but auto-position is off — the channel replicates by file and position.')
    }

    // Threads and errors.
    const ioErr = Number(r.Last_IO_Errno || 0)
    const sqlErr = Number(r.Last_SQL_Errno || 0)
    const errText = (no, msg, ts) => `error ${no}${ts ? ` at ${ts}` : ''}: ${(msg || '(no message)').replace(/\.\s*$/, '')}${MYSQL_ERRORS[no] ? ` — ${MYSQL_ERRORS[no]}` : ''}`
    if (ioErr) facts.push(['Last IO error', errText(ioErr, r.Last_IO_Error, r.Last_IO_Error_Timestamp)])
    if (sqlErr) facts.push(['Last SQL error', errText(sqlErr, r.Last_SQL_Error, r.Last_SQL_Error_Timestamp)])
    if (io === 'No') add('error', `the IO thread is stopped — nothing is received from ${src}${ioErr ? `; ${errText(ioErr, r.Last_IO_Error, r.Last_IO_Error_Timestamp)}` : ''}.`)
    else if (io === 'Connecting') add('error', `the IO thread cannot connect to ${src}${ioErr ? `; ${errText(ioErr, r.Last_IO_Error, r.Last_IO_Error_Timestamp)}` : ''}.`)
    else if (ioErr) add('warning', `the IO thread runs, but an earlier IO ${errText(ioErr, r.Last_IO_Error, r.Last_IO_Error_Timestamp)}.`)
    if (sql === 'No') add('error', `the SQL thread is stopped — nothing is applied${sqlErr ? `; ${errText(sqlErr, r.Last_SQL_Error, r.Last_SQL_Error_Timestamp)}` : ''}.`)
    else if (sqlErr) add('warning', `the SQL thread runs, but an earlier SQL ${errText(sqlErr, r.Last_SQL_Error, r.Last_SQL_Error_Timestamp)}.`)

    if (lag !== null) {
      const over = lag - delay
      if (over >= LAG_ERROR) add('error', `${fmtSeconds(lag)} behind the source${delay ? ` (${fmtSeconds(over)} beyond the SQL_Delay)` : ''}.`)
      else if (over >= LAG_WARN) add('warning', `${fmtSeconds(lag)} behind the source${delay ? ` (${fmtSeconds(over)} beyond the SQL_Delay)` : ''}.`)
    }
    if (delay) add('info', `delayed on purpose: SQL_Delay is ${fmtSeconds(delay)}${r.SQL_Remaining_Delay && !/null/i.test(r.SQL_Remaining_Delay) ? `, ${r.SQL_Remaining_Delay} s left on the current event` : ''}.`)
    if (io === 'Yes' && sql === 'Yes' && !ioErr && !sqlErr && (lag === null || lag - delay < LAG_WARN)) add('info', `running${lag !== null ? `, ${fmtSeconds(lag)} behind` : ''}.`)

    items.push({ id, title: channel ? `Channel ${channel}` : rows.length > 1 ? 'Default channel' : 'Replica', subtitle: src, facts, gtid })
  })
  const running = items.filter((_, n) => rows[n].Replica_IO_Running === 'Yes' && rows[n].Replica_SQL_Running === 'Yes').length
  return { engine: 'mysql', summary: `MySQL replica, ${items.length} channel(s), ${running} running.`, items, findings }
}

// ------------------------------------------------------------------ MongoDB

const optDate = (m) => {
  if (m.optimeDate instanceof Date) return m.optimeDate
  const ts = m.optime?.ts ?? m.optime
  return ts instanceof Ts ? new Date(ts.t * 1000) : null
}
const isoOf = (d) => (d instanceof Date && !isNaN(d) ? d.toISOString() : '—')

function mongo(t) {
  const r = readShellDoc(t)
  if (r.error) return { error: `Cannot read the rs.status() document: ${r.error}` }
  const doc = r.value
  if (doc && doc.ok === 0) {
    return { engine: 'mongodb', summary: 'rs.status() failed.', items: [], findings: [{ severity: 'error', item: null, text: `rs.status() returned an error: ${doc.errmsg || doc.codeName || 'ok: 0'}${doc.codeName === 'NotYetInitialized' ? ' — run rs.initiate().' : ''}` }] }
  }
  const members = Array.isArray(doc?.members) ? doc.members : null
  if (!members) return { error: 'No members array in the document.' }
  const items = []
  const findings = []
  const now = doc.date instanceof Date ? doc.date : null
  const primary = members.find((m) => m.stateStr === 'PRIMARY' || m.state === 1)
  const dataDates = members.filter((m) => m.stateStr !== 'ARBITER' && optDate(m)).map(optDate)
  const ref = primary ? optDate(primary) : dataDates.length ? new Date(Math.max(...dataDates)) : null
  if (!primary) findings.push({ severity: 'error', item: null, text: `No PRIMARY — the set${doc.set ? ` ${doc.set}` : ''} cannot take writes. Lag is measured against the newest member.` })

  members.forEach((m, n) => {
    const id = `m${n}`
    const add = (severity, text) => findings.push({ severity, item: id, text: `${m.name}: ${text}` })
    const state = m.stateStr || String(m.state)
    const facts = [['State', `${state}${m.self ? ' (this member)' : ''}`], ['Health', m.health === 1 ? 'up' : m.health === 0 ? 'down (unreachable)' : String(m.health ?? '—')]]
    const od = optDate(m)
    const lag = state !== 'ARBITER' && od && ref ? (ref - od) / 1000 : null
    if (state !== 'ARBITER') facts.push(['Last op', `${isoOf(od)}${lag !== null && m !== primary && lag > 0 ? ` — ${fmtSeconds(lag)} behind ${primary ? 'the primary' : 'the newest member'}` : ''}`])
    if (m.syncSourceHost !== undefined) facts.push(['Sync source', m.syncSourceHost || '(none)'])
    if (m.lastHeartbeat instanceof Date) facts.push(['Last heartbeat', `${isoOf(m.lastHeartbeat)}${now ? ` (${fmtSeconds((now - m.lastHeartbeat) / 1000)} before rs.status() ran)` : ''}`])
    if (m.pingMs !== undefined) facts.push(['Ping', `${m.pingMs} ms`])
    if (m.lastHeartbeatMessage) facts.push(['Heartbeat message', m.lastHeartbeatMessage])
    if (m.infoMessage) facts.push(['Info', m.infoMessage])
    if (m.uptime !== undefined) facts.push(['Uptime', fmtSeconds(m.uptime)])

    if (m.health === 0 || ['DOWN', '(not reachable/healthy)', 'UNKNOWN', 'REMOVED'].includes(state) || m.state === 8 || m.state === 6 || m.state === 10) {
      add('error', `${state}, not reachable${m.lastHeartbeatMessage ? ` — "${m.lastHeartbeatMessage}"` : ''}.`)
    } else if (['RECOVERING', 'ROLLBACK', 'STARTUP', 'STARTUP2'].includes(state) || [3, 9, 0, 5].includes(m.state)) {
      add('warning', `${state}${state === 'STARTUP2' ? ' — initial sync in progress' : state === 'RECOVERING' ? ' — not readable; too stale, in maintenance, or catching up' : state === 'ROLLBACK' ? ' — rolling back writes the set did not keep' : ''}${m.infoMessage ? ` (${m.infoMessage})` : ''}.`)
    }
    if (lag !== null && m !== primary && m.health !== 0 && state !== 'STARTUP2') { // an initial sync is behind by design
      if (lag >= LAG_ERROR) add('error', `${fmtSeconds(lag)} behind ${primary ? 'the primary' : 'the newest member'}.`)
      else if (lag >= LAG_WARN) add('warning', `${fmtSeconds(lag)} behind ${primary ? 'the primary' : 'the newest member'}.`)
    }
    if (state === 'SECONDARY' && m.syncSourceHost === '') add('warning', `has no sync source${m.infoMessage ? ` — ${m.infoMessage}` : ''}.`)
    if (now && m.lastHeartbeat instanceof Date && m.health === 1 && (now - m.lastHeartbeat) / 1000 > 10) add('warning', `last heartbeat ${fmtSeconds((now - m.lastHeartbeat) / 1000)} before rs.status() ran (heartbeats are every 2 s).`)
    if (m.lastHeartbeatMessage && m.health === 1) add('info', `heartbeat message: "${m.lastHeartbeatMessage}".`)
    if (m.pingMs > 100) add('info', `ping ${m.pingMs} ms — slow link to this member.`)

    items.push({ id, title: m.name, subtitle: state, facts })
  })
  const up = members.filter((m) => m.health === 1 && ['PRIMARY', 'SECONDARY', 'ARBITER'].includes(m.stateStr)).length
  if (doc.majorityVoteCount && up < doc.majorityVoteCount) {
    findings.push({ severity: 'error', item: null, text: `Only ${up} member(s) are up as PRIMARY / SECONDARY / ARBITER and a majority is ${doc.majorityVoteCount} — the set cannot elect a primary or acknowledge w: majority writes.` })
  }
  if (!findings.some((f) => f.severity !== 'info')) findings.push({ severity: 'info', item: null, text: `Healthy: ${up} of ${members.length} members up, none lagging.` })
  return {
    engine: 'mongodb',
    summary: `Replica set ${doc.set || '?'}${primary ? `, primary ${primary.name}` : ', no primary'}, ${members.length} member(s), ${up} up.${now ? ` Taken at ${isoOf(now)}.` : ''}`,
    items, findings,
  }
}

// ------------------------------------------------------------------ PostgreSQL

// parseLSN reads "16/B374D848" into a byte position (BigInt).
export function parseLSN(s) {
  const m = /^\s*([0-9A-F]{1,8})\/([0-9A-F]{1,8})\s*$/i.exec(String(s ?? ''))
  return m ? (BigInt(`0x${m[1]}`) << 32n) + BigInt(`0x${m[2]}`) : null
}

// parseInterval reads a PostgreSQL interval ("00:00:01.5", "1 day 02:00:00", "00:00:00.000312")
// into seconds.
export function parseInterval(s) {
  const v = String(s ?? '').trim()
  if (!v) return null
  let secs = 0
  let rest = v
  const d = /(-?\d+)\s+days?/.exec(rest)
  if (d) { secs += Number(d[1]) * 86400; rest = rest.replace(d[0], '') }
  const h = /(-?)(\d+):(\d\d):(\d\d(?:\.\d+)?)/.exec(rest)
  if (h) secs += (h[1] ? -1 : 1) * (Number(h[2]) * 3600 + Number(h[3]) * 60 + Number(h[4]))
  else if (!d) return null
  return secs
}

const PG_OLD = { sent_location: 'sent_lsn', write_location: 'write_lsn', flush_location: 'flush_lsn', replay_location: 'replay_lsn', received_lsn: 'flushed_lsn' }
const diff = (a, b) => (a !== null && b !== null ? Number(a - b) : null)

function postgres(t) {
  const tables = readTables(t).map((tb) => ({
    ...tb,
    columns: tb.columns.map((c) => PG_OLD[c] || c),
    rows: tb.rows.map((r) => Object.fromEntries(Object.entries(r).map(([k, v]) => [PG_OLD[k] || k, v]))),
  }))
  const items = []
  const findings = []
  const standbys = []
  const receivers = []
  let sawRepl = false
  let sawRecv = false
  for (const tb of tables) {
    const has = (c) => tb.columns.includes(c)
    if (has('sent_lsn') || (has('sync_state') && has('application_name'))) { sawRepl = true; standbys.push(...tb.rows) }
    else if (has('status') && (has('flushed_lsn') || has('sender_host') || has('written_lsn') || has('receive_start_lsn'))) { sawRecv = true; receivers.push(...tb.rows) }
  }
  if (!sawRepl && !sawRecv) return { error: 'No pg_stat_replication or pg_stat_wal_receiver table found — paste the psql output with its header.' }
  if (sawRepl && !standbys.length) findings.push({ severity: 'warning', item: null, text: 'pg_stat_replication is empty: no standby is streaming from this server.' })
  if (sawRecv && !receivers.length) findings.push({ severity: 'warning', item: null, text: 'pg_stat_wal_receiver is empty: no WAL receiver is running (not a standby, or it is not connected to its primary).' })

  standbys.forEach((r, n) => {
    const id = `s${n}`
    const name = r.application_name || `standby ${n + 1}`
    const where = `${r.client_addr || r.client_hostname || 'local socket'}${r.client_port && r.client_port !== '-1' ? `:${r.client_port}` : ''}`
    const add = (severity, text) => findings.push({ severity, item: id, text: `${name} (${where}): ${text}` })
    const sent = parseLSN(r.sent_lsn)
    const write = parseLSN(r.write_lsn)
    const flush = parseLSN(r.flush_lsn)
    const replay = parseLSN(r.replay_lsn)
    const current = parseLSN(r.pg_current_wal_lsn ?? r.current_lsn ?? r.pg_current_xlog_location)
    const lags = { write: parseInterval(r.write_lag), flush: parseInterval(r.flush_lag), replay: parseInterval(r.replay_lag) }
    const facts = [
      ['State', r.state || '—'],
      ['Sync', `${r.sync_state || '—'}${r.sync_priority && r.sync_priority !== '0' ? ` (priority ${r.sync_priority})` : ''}`],
      ['User', r.usename || '—'],
      ['LSN sent', r.sent_lsn || '—'],
      ['Written / flushed', `${r.write_lsn || '—'} / ${r.flush_lsn || '—'}${diff(sent, flush) !== null ? ` — ${fmtBytes(diff(sent, flush))} sent, not yet flushed` : ''}`],
      ['Replayed', `${r.replay_lsn || '—'}${diff(sent, replay) !== null ? ` — ${fmtBytes(diff(sent, replay))} sent, not yet replayed` : ''}`],
    ]
    if (current !== null) facts.push(['Behind current', `${fmtBytes(diff(current, replay))} (pg_current_wal_lsn − replay_lsn)`])
    facts.push(['Lag (write / flush / replay)', `${fmtSeconds(lags.write)} / ${fmtSeconds(lags.flush)} / ${fmtSeconds(lags.replay)}${lags.replay === null ? ' (empty: idle and caught up, or never measured)' : ''}`])
    if (r.ssl !== undefined) facts.push(['TLS', /^(t|true|on)$/i.test(r.ssl) ? `yes${r.version ? `, ${r.version}` : ''}` : 'no'])
    if (r.backend_start) facts.push(['Connected since', r.backend_start])
    if (r.reply_time) facts.push(['Last reply', r.reply_time])

    const st = r.state
    if (st && st !== 'streaming') {
      add(st === 'backup' ? 'info' : 'warning', st === 'catchup' ? 'catching up — not yet streaming in step with the primary.' : st === 'backup' ? 'this is a pg_basebackup in progress, not a standby.' : `state is ${st}.`)
    }
    const behind = current !== null ? diff(current, replay) : diff(sent, replay)
    if (lags.replay !== null && lags.replay >= LAG_ERROR) add('error', `replay lag ${fmtSeconds(lags.replay)}.`)
    else if (lags.replay !== null && lags.replay >= LAG_WARN) add('warning', `replay lag ${fmtSeconds(lags.replay)}.`)
    if (behind !== null && behind >= BYTES_ERROR) add('error', `${fmtBytes(behind)} of WAL not yet replayed.`)
    else if (behind !== null && behind >= BYTES_WARN) add('warning', `${fmtBytes(behind)} of WAL not yet replayed.`)
    if (['sync', 'quorum'].includes(r.sync_state)) {
      add(st === 'streaming' ? 'info' : 'error', `synchronous standby (${r.sync_state}) — commits on the primary wait for it${st !== 'streaming' ? ', and it is not streaming' : ''}.`)
    }
    if (r.ssl !== undefined && !/^(t|true|on)$/i.test(r.ssl)) add('info', 'the replication connection is not encrypted.')
    if (!findings.some((f) => f.item === id && f.severity !== 'info')) add('info', `${st || 'streaming'}, replay lag ${fmtSeconds(lags.replay ?? 0)}${behind !== null ? `, ${fmtBytes(behind)} behind` : ''}.`)
    items.push({ id, title: name, subtitle: `${where} · ${r.sync_state || ''}`, facts })
  })

  receivers.forEach((r, n) => {
    const id = `r${n}`
    const host = r.sender_host ? `${r.sender_host}${r.sender_port ? `:${r.sender_port}` : ''}` : (/host=(\S+)/.exec(r.conninfo || '') || [])[1] || '?'
    const add = (severity, text) => findings.push({ severity, item: id, text: `WAL receiver from ${host}: ${text}` })
    const ssl = (/sslmode=(\S+)/.exec(r.conninfo || '') || [])[1]
    const written = parseLSN(r.written_lsn)
    const flushed = parseLSN(r.flushed_lsn)
    const sendT = r.last_msg_send_time ? new Date(r.last_msg_send_time.replace(' ', 'T').replace(/([+-]\d\d)$/, '$1:00')) : null
    const recvT = r.last_msg_receipt_time ? new Date(r.last_msg_receipt_time.replace(' ', 'T').replace(/([+-]\d\d)$/, '$1:00')) : null
    const facts = [
      ['Status', r.status || '—'],
      ['Primary', host],
      ['Slot', r.slot_name || '(none)'],
      ['Timeline', r.received_tli || '—'],
      ['Written / flushed', `${r.written_lsn || '—'} / ${r.flushed_lsn || '—'}${diff(written, flushed) ? ` — ${fmtBytes(diff(written, flushed))} written, not yet flushed` : ''}`],
      ['Primary reported', `${r.latest_end_lsn || '—'} at ${r.latest_end_time || '—'}`],
      ['Last message', `sent ${r.last_msg_send_time || '—'}, received ${r.last_msg_receipt_time || '—'}${sendT && recvT && !isNaN(sendT) && !isNaN(recvT) ? ` (${fmtSeconds((recvT - sendT) / 1000)} in transit, clocks permitting)` : ''}`],
    ]
    if (ssl) facts.push(['TLS', `sslmode=${ssl}`])
    if (r.status && r.status !== 'streaming') add(r.status === 'stopped' ? 'error' : 'warning', `status is ${r.status}${r.status === 'waiting' ? ' — waiting to (re)connect to the primary' : ''}.`)
    if (!r.slot_name) add('info', 'no replication slot — the primary may remove WAL this standby still needs.')
    if (ssl === 'disable') add('info', 'sslmode=disable: the replication connection is not encrypted.')
    else if (['allow', 'prefer'].includes(ssl)) add('info', `sslmode=${ssl}: encrypted only if the primary offers it, and the primary's certificate is not checked.`)
    items.push({ id, title: `WAL receiver ${r.pid || ''}`.trim(), subtitle: host, facts })
  })
  return {
    engine: 'postgresql',
    summary: [standbys.length || sawRepl ? `${standbys.length} standby connection(s) (pg_stat_replication)` : '', receivers.length || sawRecv ? `${receivers.length} WAL receiver(s) (pg_stat_wal_receiver)` : ''].filter(Boolean).join(', ') + '.',
    items, findings,
  }
}
