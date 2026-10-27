// cnfdiff.js — the Config compare card: two configurations compared by what they mean, not by
// how they are written.
//
// Reads MySQL option files (my.cnf), MySQL `SHOW [GLOBAL] VARIABLES` output, postgresql.conf,
// PostgreSQL `pg_settings` / `SHOW ALL` output, and mongod.conf (YAML, or getCmdLineOpts JSON), so a
// file can be compared with a file or with what a running server reports.
//
// Names are normalised (my.cnf: `-` = `_`, `loose-` dropped, `skip-x` / `disable-x` = `x=OFF`,
// `enable-x` = `x=ON`, master/slave = source/replica), and so are values: ON/1/TRUE, `1G` and
// 1073741824, `128MB` and 16384 8kB pages, `5min` and 300s, list order. Rows that differ only in
// how they are written are kept apart from rows that differ.

import { readTables, readShellDoc } from './shellout.js'
import { parseSize, human } from './units.js'

export const CNF_FORMATS = {
  mycnf: 'MySQL option file (my.cnf)',
  mysqlvars: 'MySQL SHOW VARIABLES',
  pgconf: 'postgresql.conf',
  pgsettings: 'PostgreSQL pg_settings / SHOW ALL',
  mongod: 'mongod.conf (YAML) / getCmdLineOpts',
}
const FAMILY = { mycnf: 'mysql', mysqlvars: 'mysql', pgconf: 'postgresql', pgsettings: 'postgresql', mongod: 'mongodb' }
const LIVE = new Set(['mysqlvars', 'pgsettings'])

const MONGO_TOP = /^(storage|net|systemLog|replication|security|processManagement|operationProfiling|sharding|setParameter|auditLog|cloud|snmp)\s*:/m
const PG_NAMES = /^\s*(shared_buffers|listen_addresses|wal_level|work_mem|maintenance_work_mem|effective_cache_size|max_wal_size|hot_standby|archive_mode|archive_command|log_line_prefix|shared_preload_libraries|checkpoint_timeout|random_page_cost|autovacuum\w*|primary_conninfo|hba_file|data_directory)\b/m
const MY_NAMES = /^\s*(innodb[-_]\w+|server[-_]id|datadir|bind[-_]address|log[-_]bin|binlog[-_]\w+|gtid[-_]mode|skip[-_]\w+|socket|pid[-_]file|character[-_]set[-_]server|sql[-_]mode|wsrep[-_]\w+)\b/m

// detectFormat guesses which of CNF_FORMATS a text is.
export function detectFormat(text) {
  const t = String(text ?? '')
  if (/\bVariable_name\b/.test(t)) return 'mysqlvars'
  if (/^\s*\|?\s*name\s*\|\s*setting\b/m.test(t) || /^\s*name\s*\|\s*\S/m.test(t) && /^\s*-+\+-+/m.test(t)) return 'pgsettings'
  if (/^\s*\[[^\]\n]+\]\s*$/m.test(t) || /^\s*!include(dir)?\s/m.test(t)) return 'mycnf'
  if (MONGO_TOP.test(t) || (/^\s*\{/.test(t) && /\b(parsed|storage|net|systemLog)\b/.test(t))) return 'mongod'
  const pg = PG_NAMES.test(t) || /^\s*\w+\s*=\s*'/m.test(t) || /^\s*include(_if_exists|_dir)?\s*=?\s*'/m.test(t)
  const my = MY_NAMES.test(t) || /^\s*[a-z]+-[a-z-]+\s*(=|$)/m.test(t)
  if (my && !pg) return 'mycnf'
  if (/^\s*[A-Za-z_][\w.-]*\s*=/m.test(t)) return 'pgconf'
  if (/^\s*\w+:\s*$/m.test(t)) return 'mongod'
  return 'mycnf'
}

// ------------------------------------------------------------------ values

const BOOL = { on: true, off: false, true: true, false: false, yes: true, no: false, 1: true, 0: false }
const boolOf = (v) => BOOL[String(v ?? '').trim().toLowerCase()]

const unquote = (v) => {
  const s = String(v ?? '').trim()
  if (s.length >= 2 && (s[0] === '"' || s[0] === "'") && s.endsWith(s[0])) return s.slice(1, -1).replace(s[0] === "'" ? /''/g : /""/g, s[0])
  return s
}

// stripComment removes a `# comment` that is not inside quotes. In YAML a # starts a comment only
// at the start of a line or after a space; in my.cnf and postgresql.conf it does anywhere.
function stripComment(line, yaml = false) {
  let q = null
  for (let i = 0; i < line.length; i++) {
    const c = line[i]
    if (q) { if (c === '\\') i++; else if (c === q) q = null; continue }
    if (c === '"' || c === "'") q = c
    else if (c === '#' && (!yaml || i === 0 || /\s/.test(line[i - 1]))) return line.slice(0, i)
  }
  return line
}

const fmtDur = (s) => {
  if (s === 0) return '0 s'
  for (const [u, f] of [['d', 86400], ['h', 3600], ['min', 60], ['s', 1], ['ms', 1e-3], ['µs', 1e-6]]) {
    if (Math.abs(s) >= f && Math.abs(Math.round((s / f) * 1000) / 1000 - s / f) < 1e-9) return `${Math.round((s / f) * 1000) / 1000} ${u}`
  }
  return `${s} s`
}
const fmtBytes = (b) => (Math.abs(b) < 1024 ? `${b} B` : `${human(b)} (${b.toLocaleString('en-US')} B)`)

// PostgreSQL: the unit a bare number is in, per setting (what pg_settings.unit says).
export const PG_UNITS = {
  shared_buffers: '8kB', effective_cache_size: '8kB', wal_buffers: '8kB', temp_buffers: '8kB',
  min_parallel_table_scan_size: '8kB', min_parallel_index_scan_size: '8kB',
  backend_flush_after: '8kB', bgwriter_flush_after: '8kB', checkpoint_flush_after: '8kB', wal_writer_flush_after: '8kB',
  work_mem: 'kB', maintenance_work_mem: 'kB', autovacuum_work_mem: 'kB', logical_decoding_work_mem: 'kB',
  max_stack_depth: 'kB', temp_file_limit: 'kB', gin_pending_list_limit: 'kB', log_rotation_size: 'kB', wal_skip_threshold: 'kB',
  huge_page_size: 'kB', log_temp_files: 'kB',
  max_wal_size: 'MB', min_wal_size: 'MB', wal_keep_size: 'MB', max_slot_wal_keep_size: 'MB',
  track_activity_query_size: 'B', wal_decode_buffer_size: 'B', log_parameter_max_length: 'B', log_parameter_max_length_on_error: 'B',
  checkpoint_timeout: 's', checkpoint_warning: 's', autovacuum_naptime: 's', archive_timeout: 's', authentication_timeout: 's',
  tcp_keepalives_idle: 's', tcp_keepalives_interval: 's', tcp_user_timeout: 'ms', wal_receiver_status_interval: 's',
  statement_timeout: 'ms', lock_timeout: 'ms', idle_in_transaction_session_timeout: 'ms', idle_session_timeout: 'ms',
  transaction_timeout: 'ms', deadlock_timeout: 'ms', log_min_duration_statement: 'ms', log_min_duration_sample: 'ms',
  log_autovacuum_min_duration: 'ms', autovacuum_vacuum_cost_delay: 'ms', vacuum_cost_delay: 'ms', wal_writer_delay: 'ms',
  bgwriter_delay: 'ms', wal_receiver_timeout: 'ms', wal_sender_timeout: 'ms', wal_retrieve_retry_interval: 'ms',
  max_standby_streaming_delay: 'ms', max_standby_archive_delay: 'ms', recovery_min_apply_delay: 'ms',
  log_rotation_age: 'min', commit_delay: 'us',
}
const PG_MEM = { B: 1, kB: 1024, MB: 2 ** 20, GB: 2 ** 30, TB: 2 ** 40 }
const PG_TIME = { us: 1e-6, ms: 1e-3, s: 1, min: 60, h: 3600, d: 86400 }

// unitFactor reads a pg_settings unit ("8kB", "kB", "ms", "16MB") as { mem | time: factor }.
function unitFactor(u) {
  const m = /^(\d*)\s*(B|kB|MB|GB|TB|us|ms|s|min|h|d)$/.exec(String(u ?? '').trim())
  if (!m) return null
  const n = Number(m[1] || 1)
  return PG_MEM[m[2]] ? { mem: n * PG_MEM[m[2]] } : { time: n * PG_TIME[m[2]] }
}

// pgQuantity reads a PostgreSQL value with or without a unit into { bytes } or { seconds }, using
// the setting's base unit for a bare number. -1 and 0 are kept as they are (off / default).
function pgQuantity(name, v, unit) {
  const m = /^(-?\d+(?:\.\d+)?)\s*(B|kB|MB|GB|TB|us|ms|s|min|h|d)?$/.exec(String(v).trim())
  if (!m) return null
  const n = Number(m[1])
  if (m[2]) return PG_MEM[m[2]] ? { bytes: n * PG_MEM[m[2]] } : { seconds: n * PG_TIME[m[2]] }
  if (n === -1 || n === 0) return null
  const f = unitFactor(unit || PG_UNITS[name])
  if (!f) return null
  return f.mem ? { bytes: n * f.mem } : { seconds: n * f.time }
}

// mySize reads a MySQL number with an optional K/M/G/T/P suffix (binary, as MySQL reads it).
function mySize(v) {
  const s = String(v).trim()
  if (!/^\d+(\.\d+)?[kmgtp]?$/i.test(s)) return null
  const r = parseSize(s, 'mycnf')
  return r.error ? null : { bytes: r.bytes, suffix: !!r.suffix }
}

// sameMeaning compares two entries' values: { same, written, note }. `written` is true when they
// mean the same but are written differently.
export function sameMeaning(family, name, a, b) {
  const va = String(a.value ?? '').trim()
  const vb = String(b.value ?? '').trim()
  if (va === vb) return { same: true, written: false }
  const ba = boolOf(va)
  const bb = boolOf(vb)
  const word = (v) => /^[a-z]/i.test(v)
  if (ba !== undefined && bb !== undefined && (word(va) || word(vb))) {
    return ba === bb ? { same: true, written: true, note: `both ${ba ? 'ON' : 'OFF'}` } : { same: false, note: `${ba ? 'ON' : 'OFF'} vs ${bb ? 'ON' : 'OFF'}` }
  }
  if (family === 'mysql') {
    const sa = mySize(va)
    const sb = mySize(vb)
    if (sa && sb) {
      if (sa.bytes === sb.bytes) return { same: true, written: true, note: `${va} = ${vb} = ${fmtBytes(sa.bytes)}` }
      if (sa.suffix || sb.suffix) return { same: false, note: `${fmtBytes(sa.bytes)} vs ${fmtBytes(sb.bytes)}` }
    }
    // log_bin=<basename> turns binary logging on; SHOW VARIABLES then says ON.
    if (name === 'log_bin' && (bb === true || ba === true) && (ba === undefined || bb === undefined) && (va || vb)) {
      return { same: true, written: true, note: `log_bin=${ba === undefined ? va : vb} turns binary logging ON and sets its file name (log_bin_basename)` }
    }
  }
  if (family === 'postgresql') {
    const qa = pgQuantity(name, va, a.unit)
    const qb = pgQuantity(name, vb, b.unit)
    if (qa && qb && 'bytes' in qa && 'bytes' in qb) {
      return qa.bytes === qb.bytes ? { same: true, written: true, note: `both ${fmtBytes(qa.bytes)}` } : { same: false, note: `${fmtBytes(qa.bytes)} vs ${fmtBytes(qb.bytes)}` }
    }
    if (qa && qb && 'seconds' in qa && 'seconds' in qb) {
      return Math.abs(qa.seconds - qb.seconds) < 1e-9 ? { same: true, written: true, note: `both ${fmtDur(qa.seconds)}` } : { same: false, note: `${fmtDur(qa.seconds)} vs ${fmtDur(qb.seconds)}` }
    }
  }
  if (va.toLowerCase() === vb.toLowerCase()) return { same: true, written: true, note: 'differs only in letter case' }
  if (/^-?\d+(\.\d+)?(e[+-]?\d+)?$/i.test(va) && /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i.test(vb) && Number(va) === Number(vb)) {
    return { same: true, written: true, note: `both ${Number(va)}` }
  }
  if (/^[/.~]/.test(va) && /^[/.~]/.test(vb) && va.replace(/\/+$/, '') === vb.replace(/\/+$/, '')) return { same: true, written: true, note: 'the same path, with and without a trailing /' }
  if (/,/.test(va + vb)) {
    const set = (v) => v.split(',').map((x) => unquote(x).trim().toLowerCase()).filter(Boolean).sort().join(',')
    if (set(va) === set(vb)) return { same: true, written: true, note: 'the same items in a different order or spacing' }
  }
  return { same: false }
}

// ------------------------------------------------------------------ readers

// MySQL option groups mysqld reads: they form one scope, and later lines win across them.
const SERVER_GROUP = /^(mysqld|server|mariadb|mariadbd|galera)(-[\d.]+)?$/
// skip-* options that are variables in their own right (skip_name_resolve=ON), not x=OFF.
const SKIP_VARS = new Set(['skip_name_resolve', 'skip_networking', 'skip_external_locking', 'skip_show_database', 'skip_slave_start', 'skip_replica_start', 'skip_grant_tables'])
// Options that may be given many times, each one counting.
const MY_MULTI = new Set(['plugin_load_add', 'replicate_do_db', 'replicate_ignore_db', 'replicate_do_table', 'replicate_ignore_table',
  'replicate_wild_do_table', 'replicate_wild_ignore_table', 'replicate_rewrite_db', 'binlog_do_db', 'binlog_ignore_db', 'replica_skip_errors'])

// normMy turns a my.cnf option name and value into the variable name and the value it sets.
export function normMy(rawName, rawValue) {
  let n = rawName.trim().toLowerCase().replace(/-/g, '_')
  let v = rawValue === undefined ? undefined : unquote(rawValue)
  const notes = []
  if (n.startsWith('loose_')) { n = n.slice(6); notes.push('loose- prefix: ignored if the server does not know it') }
  const m = /^(skip|disable|enable)_(.+)$/.exec(n)
  if (m && !SKIP_VARS.has(n)) {
    const on = m[1] === 'enable'
    const t = v === undefined ? true : boolOf(v)
    if (t !== undefined) {
      n = m[2]
      v = (t ? on : !on) ? 'ON' : 'OFF'
      notes.push(`${rawName.trim()} means ${n.replace(/slave/g, 'replica').replace(/master/g, 'source')}=${v}`)
    }
  }
  if (v === undefined) { v = 'ON'; notes.push('given without a value: a boolean option, turned on') }
  const renamed = n.replace(/slave/g, 'replica').replace(/master/g, 'source')
  return { name: renamed, value: v, note: notes.join('; ') }
}

// readConfig reads one side into { format, family, live, scopes: Map(scope → Map(name → [entry])),
// includes, notes }. An entry is { name, rawName, raw, value, line, section, unit, note }.
export function readConfig(text, format = 'auto') {
  const fmt = format === 'auto' || !CNF_FORMATS[format] ? detectFormat(text) : format
  const out = { format: fmt, family: FAMILY[fmt], live: LIVE.has(fmt), scopes: new Map(), includes: [], notes: [] }
  const put = (scope, e) => {
    if (!out.scopes.has(scope)) out.scopes.set(scope, new Map())
    const m = out.scopes.get(scope)
    if (!m.has(e.name)) m.set(e.name, [])
    const list = m.get(e.name)
    // A live server can list a variable under its old and new name with one value: not a repeat.
    if (out.live && list.some((x) => x.value === e.value)) return
    list.push(e)
  }
  const lines = String(text ?? '').replace(/\r\n/g, '\n').split('\n')

  if (fmt === 'mycnf') {
    let section = null
    lines.forEach((l, i) => {
      const s = stripComment(l).trim()
      if (!s || s.startsWith(';')) return
      const inc = /^!(include|includedir)\s+(.+)$/.exec(s)
      if (inc) { out.includes.push({ line: i + 1, kind: `!${inc[1]}`, path: inc[2].trim() }); return }
      const sec = /^\[\s*([^\]]+?)\s*\]$/.exec(s)
      if (sec) { section = sec[1].toLowerCase(); return }
      const m = /^([^=\s]+)\s*(?:=\s*(.*))?$/.exec(s)
      if (!m) return
      const { name, value, note } = normMy(m[1], m[2])
      const scope = section === null ? '(no group)' : SERVER_GROUP.test(section) ? 'mysqld' : section
      put(scope, { name, rawName: m[1], raw: s, value, as: m[2]?.trim(), line: i + 1, section: section ?? '', note })
    })
    if (out.scopes.has('(no group)')) out.notes.push('Options before the first [group] — MySQL refuses such a file ("Found option without preceding group").')
  } else if (fmt === 'mysqlvars') {
    let rows = readTables(text).flatMap((t) => t.rows).filter((r) => 'Variable_name' in r)
    if (!rows.length) rows = lines.filter((l) => l.includes('\t')).map((l) => { const [Variable_name, Value] = l.split('\t'); return { Variable_name, Value } })
    rows.forEach((r, i) => {
      const { name, value } = normMy(r.Variable_name, r.Value ?? '')
      put('mysqld', { name, rawName: r.Variable_name, raw: `${r.Variable_name} = ${r.Value ?? ''}`, value, line: i + 1, section: '' })
    })
  } else if (fmt === 'pgconf') {
    lines.forEach((l, i) => {
      const s = stripComment(l).trim()
      if (!s) return
      const m = /^([A-Za-z_][\w.-]*)\s*(?:=\s*|\s+)(.*)$/.exec(s)
      if (!m) return
      const name = m[1].toLowerCase()
      if (['include', 'include_if_exists', 'include_dir'].includes(name)) { out.includes.push({ line: i + 1, kind: name, path: unquote(m[2]) }); return }
      put('', { name, rawName: m[1], raw: s, value: unquote(m[2]), as: m[2].trim(), line: i + 1, section: '' })
    })
  } else if (fmt === 'pgsettings') {
    const rows = readTables(text).flatMap((t) => t.rows).filter((r) => 'name' in r && ('setting' in r || Object.keys(r).length >= 2))
    rows.forEach((r, i) => {
      const value = r.setting ?? Object.values(r)[1]
      put('', { name: r.name.toLowerCase(), rawName: r.name, raw: `${r.name} = ${value}${r.unit ? ` (${r.unit})` : ''}`, value, unit: r.unit || '', line: i + 1, section: '', note: r.source ? `source: ${r.source}${r.sourcefile ? ` ${r.sourcefile}:${r.sourceline}` : ''}` : '' })
    })
  } else if (fmt === 'mongod') {
    for (const e of readMongo(text, out)) put('', e)
  }
  return out
}

// readMongo reads mongod.conf YAML (block maps, `- ` lists, flow { } and [ ]) or getCmdLineOpts
// JSON into flattened dotted names.
function readMongo(text, out) {
  const entries = []
  const flat = (obj, prefix, line) => {
    for (const [k, v] of Object.entries(obj)) {
      const name = prefix ? `${prefix}.${k}` : k
      if (v && typeof v === 'object' && !Array.isArray(v) && !(v instanceof Date)) flat(v, name, line)
      else entries.push({ name, rawName: name, raw: `${name}: ${Array.isArray(v) ? v.join(', ') : v}`, value: Array.isArray(v) ? v.join(',') : String(v), line, section: '' })
    }
  }
  const t = String(text ?? '')
  if (/^\s*\{/.test(t)) {
    const d = readShellDoc(t)
    if (d.error) { out.notes.push(`Cannot read the JSON: ${d.error}`); return entries }
    flat(d.value.parsed && typeof d.value.parsed === 'object' ? d.value.parsed : d.value, '', null)
    return entries
  }
  const stack = [] // [{ indent, key }]
  let list = null // the entry a `- item` adds to
  t.replace(/\r\n/g, '\n').split('\n').forEach((l, i) => {
    const raw = stripComment(l, true).replace(/\s+$/, '')
    if (!raw.trim() || /^\s*(---|\.\.\.)\s*$/.test(raw)) return
    const indent = raw.length - raw.trimStart().length
    const s = raw.trim()
    if (s.startsWith('- ') || s === '-') {
      const item = unquote(s.slice(1).trim())
      if (list) { list.items.push(item); list.entry.value = list.items.join(','); list.entry.raw = `${list.entry.name}: [${list.items.join(', ')}]` }
      return
    }
    const m = /^("[^"]*"|'[^']*'|[^:]+?)\s*:(?:\s+(.*))?$/.exec(s)
    if (!m) return
    while (stack.length && stack[stack.length - 1].indent >= indent) stack.pop()
    const key = unquote(m[1])
    const name = [...stack.map((x) => x.key), key].join('.')
    const v = (m[2] ?? '').trim()
    list = null
    if (!v) {
      stack.push({ indent, key })
      const entry = { name, rawName: name, raw: `${name}:`, value: '', line: i + 1, section: '' }
      list = { entry, items: [], pending: true }
      entries.push(entry)
      return
    }
    // Flow style, one level deep: [a, b] and { k: v, k2: v2 }.
    const flow = /^\[(.*)\]$/.exec(v) || /^\{(.*)\}$/.exec(v)
    if (flow) {
      const parts = flow[1].split(',').map((x) => x.trim()).filter(Boolean)
      if (v.startsWith('[')) entries.push({ name, rawName: name, raw: `${name}: ${v}`, value: parts.map(unquote).join(','), as: v, line: i + 1, section: '' })
      else for (const kv of parts) {
        const [k, ...rest] = kv.split(':')
        const val = rest.join(':').trim()
        entries.push({ name: `${name}.${unquote(k.trim())}`, rawName: `${name}.${unquote(k.trim())}`, raw: `${name}.${unquote(k.trim())}: ${val}`, value: unquote(val), as: val, line: i + 1, section: '' })
      }
      return
    }
    entries.push({ name, rawName: name, raw: `${name}: ${v}`, value: unquote(v), as: v, line: i + 1, section: '' })
  })
  // A key with neither a value nor list items was a map: drop its placeholder.
  return entries.filter((e) => e.value !== '' || !entries.some((x) => x !== e && x.name.startsWith(`${e.name}.`)))
}

// ------------------------------------------------------------------ compare

const KIND_ORDER = { diff: 0, onlyA: 1, onlyB: 2, written: 3, same: 4 }

// final is the value an option ends up with: the last line, or every line for a repeatable one.
function final(list, family) {
  const last = list[list.length - 1]
  if (family === 'mysql' && MY_MULTI.has(last.name) && list.length > 1) {
    return { ...last, value: list.map((e) => e.value).join(','), raw: list.map((e) => e.raw).join(' + '), repeatable: true }
  }
  return last
}

// compareConfigs compares two texts. Returns { a, b, rows, dupes, notes, counts } where a row is
// { scope, name, kind: 'diff' | 'onlyA' | 'onlyB' | 'written' | 'same', a, b, note, defaultOnly }
// and a dupe is { side, scope, name, entries, wins }.
export function compareConfigs(aText, bText, { fmtA = 'auto', fmtB = 'auto' } = {}) {
  const A = readConfig(aText, fmtA)
  const B = readConfig(bText, fmtB)
  const notes = [...A.notes.map((n) => `A: ${n}`), ...B.notes.map((n) => `B: ${n}`)]
  if (A.family !== B.family && String(aText).trim() && String(bText).trim()) notes.push(`A is ${CNF_FORMATS[A.format]} and B is ${CNF_FORMATS[B.format]} — different databases; set the format by hand if a guess is wrong.`)
  for (const [side, X] of [['A', A], ['B', B]]) {
    for (const inc of X.includes) notes.push(`${side} line ${inc.line}: ${inc.kind} ${inc.path} — not followed; paste its options into the same side to include them.`)
  }

  // A live server has one scope (mysqld); compare only the file's server groups against it.
  let scopes = [...new Set([...A.scopes.keys(), ...B.scopes.keys()])]
  if (A.family === 'mysql' && (A.live || B.live) && A.live !== B.live) {
    const file = A.live ? B : A
    const skipped = [...file.scopes.keys()].filter((s) => s !== 'mysqld')
    if (skipped.length) notes.push(`Only the server groups ([mysqld], [server], [mysqld-8.0], [mariadb]…) are compared with SHOW VARIABLES; not compared: ${skipped.map((s) => `[${s}]`).join(', ')}.`)
    scopes = ['mysqld']
  }

  const rows = []
  const dupes = []
  for (const scope of scopes) {
    const ma = A.scopes.get(scope) || new Map()
    const mb = B.scopes.get(scope) || new Map()
    for (const [side, m, X] of [['A', ma, A], ['B', mb, B]]) {
      for (const [name, list] of m) {
        if (list.length > 1) {
          const rep = X.family === 'mysql' && MY_MULTI.has(name)
          dupes.push({ side, scope, name, entries: list, wins: rep ? null : list[list.length - 1], repeatable: rep })
        }
      }
    }
    for (const name of [...new Set([...ma.keys(), ...mb.keys()])]) {
      const ea = ma.has(name) ? final(ma.get(name), A.family) : null
      const eb = mb.has(name) ? final(mb.get(name), B.family) : null
      const row = { scope, name, a: ea, b: eb }
      if (ea && eb) {
        const r = sameMeaning(A.family, name, ea, eb)
        const notes = [r.note, ea.note && `A: ${ea.note}`, eb.note && `B: ${eb.note}`]
        // Written differently: the values differ in form, or the names do (innodb-buffer-pool-size,
        // loose-…, skip-…, slave/replica) where neither side is a server listing.
        const renamed = ea.rawName !== eb.rawName && !A.live && !B.live
        if (renamed) notes.push(`named ${ea.rawName} in A and ${eb.rawName} in B`)
        const quoted = r.same && !r.written && ea.as !== undefined && eb.as !== undefined && ea.as !== eb.as
        if (quoted) notes.push(`written ${ea.as} in A and ${eb.as} in B`)
        row.kind = !r.same ? 'diff' : r.written || renamed || quoted || ea.note || eb.note ? 'written' : 'same'
        row.note = notes.filter(Boolean).join('; ')
      } else if (ea) {
        row.kind = 'onlyA'
        row.note = ea.note || ''
        if (B.live) row.note = [row.note, `the server does not report ${name} — an option that is not a variable, a plugin that is not loaded, or a misspelling`].filter(Boolean).join('; ')
      } else {
        row.kind = 'onlyB'
        row.note = eb.note || ''
        if (B.live && !A.live) row.defaultOnly = true // set on the server, not in the file: its default or set elsewhere
        if (A.live && !B.live) row.note = [row.note, `the server does not report ${name}`].filter(Boolean).join('; ')
      }
      if (A.live && !B.live && row.kind === 'onlyA') row.defaultOnly = true
      rows.push(row)
    }
  }
  rows.sort((x, y) => KIND_ORDER[x.kind] - KIND_ORDER[y.kind] || x.scope.localeCompare(y.scope) || x.name.localeCompare(y.name))
  const counts = { diff: 0, onlyA: 0, onlyB: 0, written: 0, same: 0, defaults: 0 }
  for (const r of rows) { counts[r.kind]++; if (r.defaultOnly) counts.defaults++ }
  return { a: { format: A.format, live: A.live }, b: { format: B.format, live: B.live }, rows, dupes, notes, counts }
}

// visibleRows applies the card's filter: 'diff' (differences), 'written' (and the rows written
// differently) or 'all'. Options only a live server reports are left out unless 'all'.
export function visibleRows(res, show = 'written') {
  return res.rows.filter((r) => {
    if (show === 'all') return true
    if (r.defaultOnly) return false
    if (r.kind === 'same') return false
    if (r.kind === 'written') return show === 'written'
    return true
  })
}
