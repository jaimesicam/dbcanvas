// innodb.js — the InnoDB status reader card: SHOW ENGINE INNODB STATUS split into its sections,
// with the latest deadlock broken into its transactions and the numbers worth a look listed.
//
// Reads the output as the mysql client prints it with \G (the `Type: InnoDB / Name: / Status:`
// rows included), the `| InnoDB | | ... |` table form, and the single-line form with literal `\n`
// escapes that some clients and log collectors produce, and a deadlock alone as
// innodb_print_all_deadlocks writes it to the error log. MySQL 5.7 and 8.0 layouts both parse: the
// sections are found by their dashed titles, and each reader takes only the lines it knows.

const HLL_WARN = 100000
const HLL_DANGER = 1000000
const OLD_TRX = 600 // seconds a transaction is ACTIVE before it is listed as old
const OLD_TRX_DANGER = 3600
const SEM_WAIT = 10 // seconds a thread waits on a semaphore before it is reported
const HIT_RATE = 990 // of 1000; below this the buffer pool hit rate is reported

const n = (v) => (v === undefined || v === null || v === '' ? null : Number(String(v).replace(/,/g, '')))
export const fmtN = (v) => (v === null || v === undefined ? '—' : Number(v).toLocaleString('en-US', { maximumFractionDigits: 2 }))

// unescape turns the pasted text back into the monitor's own lines.
function unescape(text) {
  let t = String(text ?? '').replace(/\r\n/g, '\n')
  const real = (t.match(/\n/g) || []).length
  const lit = (t.match(/\\n/g) || []).length
  if (lit > real) t = t.replace(/\\n/g, '\n').replace(/\\t/g, '\t').replace(/\\'/g, "'").replace(/\\"/g, '"')
  const lines = t.split('\n').map((l) => l.replace(/^\d{4}-\d\d-\d\dT\S+\s+\d+\s+\[\w+\]\s+\[MY-\d+\]\s+\[InnoDB\]\s?/, '').replace(/^\s*Status:\s?/, '').replace(/^\|\s*InnoDB\s*\|[^|]*\|\s?/, '').replace(/\s*\|\s*$/, ''))
  return lines.filter((l) => !/^\s*(Type:\s*InnoDB|Name:\s*)\s*$/.test(l) && !/^\*+ \d+\. row \*+\s*$/.test(l.trim()) && !/^\+[-+]+\+$/.test(l.trim()) && !/^\|\s*Type\s*\|/.test(l.trim()))
}

// sections splits by the monitor's titles: a line of dashes, the title, a line of dashes.
function sections(lines) {
  const out = new Map()
  let cur = null
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i].trim()
    if (/^-{3,}$/.test(l) && i + 2 < lines.length && /^[A-Z][A-Z /]+$/.test(lines[i + 1].trim()) && /^-{3,}$/.test(lines[i + 2].trim())) {
      cur = lines[i + 1].trim()
      out.set(cur, [])
      i += 2
      continue
    }
    if (/^={3,}$/.test(l) || /END OF INNODB MONITOR OUTPUT/.test(l)) { if (/END OF/.test(l)) cur = null; continue }
    if (cur) out.get(cur).push(lines[i])
  }
  return out
}

// parseInnodb reads the whole output. Returns { header, deadlock, fkError, semaphores, transactions,
// bufferPool, log, rows, sections: [titles], findings } or { error }.
export function parseInnodb(text, opts = {}) {
  const lines = unescape(text)
  const all = lines.join('\n')
  // A deadlock on its own, as innodb_print_all_deadlocks writes it to the error log.
  if (!/INNODB MONITOR OUTPUT/.test(all) && /^\*\*\* \(1\) TRANSACTION:/m.test(all)) {
    const res = { header: { time: null, thread: null, seconds: null }, sections: ['LATEST DETECTED DEADLOCK'], deadlock: deadlock(lines), fkError: null, semaphores: null, transactions: null, bufferPool: null, log: null, rows: null }
    res.findings = findings(res)
    return res
  }
  if (!/INNODB MONITOR OUTPUT|^-+\n(SEMAPHORES|TRANSACTIONS|BUFFER POOL AND MEMORY)\n-+$/m.test(all)) {
    return { error: 'Not SHOW ENGINE INNODB STATUS output (no "INNODB MONITOR OUTPUT" header or section titles found)' }
  }
  const secs = sections(lines)
  const hm = /^(\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d)\s+(\S+)\s+INNODB MONITOR OUTPUT/m.exec(all)
  const pm = /Per second averages calculated from the last (\d+) seconds/.exec(all)
  const res = {
    header: { time: hm ? hm[1] : null, thread: hm ? hm[2] : null, seconds: pm ? Number(pm[1]) : null },
    sections: [...secs.keys()],
    deadlock: secs.has('LATEST DETECTED DEADLOCK') ? deadlock(secs.get('LATEST DETECTED DEADLOCK')) : null,
    fkError: secs.has('LATEST FOREIGN KEY ERROR') ? secs.get('LATEST FOREIGN KEY ERROR').filter((l) => l.trim()).slice(0, 12).join('\n') : null,
    semaphores: secs.has('SEMAPHORES') ? semaphores(secs.get('SEMAPHORES')) : null,
    transactions: secs.has('TRANSACTIONS') ? transactions(secs.get('TRANSACTIONS')) : null,
    bufferPool: secs.has('BUFFER POOL AND MEMORY') ? bufferPool(secs.get('BUFFER POOL AND MEMORY'), secs.get('INDIVIDUAL BUFFER POOL INFO')) : null,
    log: secs.has('LOG') ? log(secs.get('LOG'), opts.redoBytes) : null,
    rows: secs.has('ROW OPERATIONS') ? rowOps(secs.get('ROW OPERATIONS')) : null,
  }
  res.findings = findings(res)
  return res
}

// ------------------------------------------------------------------ deadlock

// lockLine reads a RECORD LOCKS / TABLE LOCK line into { kind, table, index, mode, waiting, text }.
export function lockLine(line) {
  const l = line.trim()
  let m
  if ((m = /^RECORD LOCKS space id (\d+) page no (\d+) n bits \d+ index (\S+) of table (\S+) trx id (\d+) (.*)$/.exec(l))) {
    return { kind: 'record', index: m[3].replace(/`/g, ''), table: m[4].replace(/`/g, ''), trx: m[5], mode: m[6].replace(/\s*waiting\s*$/, ''), waiting: /\bwaiting\s*$/.test(m[6]), space: m[1], page: m[2], text: l }
  }
  if ((m = /^TABLE LOCK table (\S+) trx id (\d+) lock mode (\S+)(.*)$/.exec(l))) {
    return { kind: 'table', table: m[1].replace(/`/g, ''), index: null, trx: m[2], mode: `table lock ${m[3]}`, waiting: /waiting/.test(m[4]), text: l }
  }
  return null
}

// threadLine reads "MySQL thread id 12, OS thread handle 140..., query id 345 host user state".
function threadLine(t, line) {
  const m = /MySQL thread id (\d+), OS thread handle (\S+?),? query id (\d+)\s*(.*)$/.exec(line)
  if (!m) return false
  t.thread = m[1]
  t.queryId = m[3]
  t.client = m[4].trim()
  return true
}

function trxHead(t, line) {
  const m = /^TRANSACTION (\d+), ACTIVE (?:\(PREPARED\) )?(\d+) sec\s*(.*)$/.exec(line.trim())
  if (m) { t.trx = m[1]; t.active = Number(m[2]); t.state = m[3].trim(); return true }
  const s = /(\d+) lock struct\(s\), heap size (\d+), (\d+) row lock\(s\)(?:, undo log entries (\d+))?/.exec(line)
  if (s) { t.lockStructs = Number(s[1]); t.rowLocks = Number(s[3]); t.undo = s[4] ? Number(s[4]) : null; if (/LOCK WAIT/.test(line)) t.lockWait = true; return true }
  const u = /mysql tables in use (\d+), locked (\d+)/.exec(line)
  if (u) { t.tablesInUse = Number(u[1]); t.tablesLocked = Number(u[2]); return true }
  return false
}

function deadlock(lines) {
  const d = { time: null, trxs: [], rolledBack: null, note: null }
  const tm = lines.find((l) => /^\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d/.test(l.trim()))
  if (tm) d.time = tm.trim().split(/\s+/).slice(0, 2).join(' ')
  let t = null
  let part = null // 'trx' | 'holds' | 'waits'
  let query = null
  for (const raw of lines) {
    const line = raw.replace(/\s+$/, '')
    let m
    if ((m = /^\*\*\* \((\d+)\) TRANSACTION:/.exec(line))) {
      t = { n: Number(m[1]), query: '', holds: [], waits: [] }
      d.trxs.push(t)
      part = 'trx'
      query = null
      continue
    }
    if ((m = /^\*\*\* \((\d+)\) (HOLDS THE LOCK\(S\)|WAITING FOR THIS LOCK TO BE GRANTED):/.exec(line))) {
      t = d.trxs.find((x) => x.n === Number(m[1])) || t
      part = /HOLDS/.test(m[2]) ? 'holds' : 'waits'
      query = null
      continue
    }
    if ((m = /^\*\*\* WE ROLL BACK TRANSACTION \((\d+)\)/.exec(line))) { d.rolledBack = Number(m[1]); part = null; continue }
    if (/TOO DEEP OR LONG SEARCH/.test(line)) { d.note = line.replace(/^\*\*\*\s*/, ''); continue }
    if (!t || !part) continue
    if (part === 'trx') {
      if (trxHead(t, line)) continue
      if (threadLine(t, line)) { query = []; continue }
      if (query && line.trim()) { query.push(line.trim()); t.query = query.join('\n') }
      continue
    }
    const lk = lockLine(line)
    if (lk) t[part].push(lk)
  }
  return d
}

// ------------------------------------------------------------------ semaphores

function semaphores(lines) {
  const s = { reservations: null, signals: null, spins: [], waits: [], lines: [] }
  let w = null
  for (const raw of lines) {
    const line = raw.trim()
    if (!line) continue
    let m
    if ((m = /OS WAIT ARRAY INFO: reservation count (\d+)/.exec(line))) { s.reservations = Number(m[1]); continue }
    if ((m = /OS WAIT ARRAY INFO: signal count (\d+)/.exec(line))) { s.signals = Number(m[1]); continue }
    if ((m = /^--Thread (\d+) has waited at (\S+) line (\d+) for ([\d.]+) seconds the semaphore:/.exec(line))) {
      w = { thread: m[1], at: `${m[2]}:${m[3]}`, seconds: Number(m[4]), what: [] }
      s.waits.push(w)
      continue
    }
    if (/^(RW-shared|RW-excl|RW-sx|Mutex) spins/.test(line) || /^Spin rounds per wait/.test(line)) { s.spins.push(line); w = null; continue }
    if (w) { w.what.push(line); continue }
    s.lines.push(line)
  }
  return s
}

// ------------------------------------------------------------------ transactions

function transactions(lines) {
  const x = { counter: null, purge: null, hll: null, list: [], notStarted: 0 }
  let t = null
  let query = null
  let waitLock = false
  for (const raw of lines) {
    const line = raw.replace(/\s+$/, '')
    let m
    if ((m = /^Trx id counter (\d+)/.exec(line))) { x.counter = m[1]; continue }
    if (/^Purge done for/.test(line)) { x.purge = line.trim(); continue }
    if ((m = /^History list length (\d+)/.exec(line))) { x.hll = Number(m[1]); continue }
    if ((m = /^---TRANSACTION (\d+), (.*)$/.exec(line))) {
      t = null
      query = null
      waitLock = false
      if (/not started/.test(m[2])) { x.notStarted++; continue }
      t = { trx: m[1], active: null, state: '', query: '', waitingSec: null, waitingFor: null }
      const a = /ACTIVE (?:\(PREPARED\) )?(\d+) sec\s*(.*)$/.exec(m[2])
      if (a) { t.active = Number(a[1]); t.state = a[2].replace(/^,\s*/, '').trim() }
      if (/PREPARED/.test(m[2])) t.prepared = true
      x.list.push(t)
      continue
    }
    if (!t) continue
    if ((m = /^------- TRX HAS BEEN WAITING (\d+) SEC FOR THIS LOCK TO BE GRANTED:/.exec(line))) { t.waitingSec = Number(m[1]); waitLock = true; query = null; continue }
    if (waitLock) { const lk = lockLine(line); if (lk) { t.waitingFor = lk; waitLock = false } continue }
    if (/^------------------$/.test(line.trim())) { query = null; continue }
    if (trxHead(t, line)) continue
    if (threadLine(t, line)) { query = []; continue }
    if (/^(Trx read view|TABLE LOCK|RECORD LOCKS|Record lock|\s*\d+: len)/.test(line) || /^-{3,}/.test(line)) { query = null; continue }
    if (query && line.trim()) { query.push(line.trim()); t.query = query.join('\n') }
  }
  x.list.sort((a, b) => (b.active ?? 0) - (a.active ?? 0))
  return x
}

// ------------------------------------------------------------------ buffer pool, log, rows

function bufferPool(lines, individual) {
  const b = {}
  const take = (re, key) => { for (const l of lines) { const m = re.exec(l); if (m) { b[key] = n(m[1]); return } } }
  take(/^Total large memory allocated (\d+)/, 'totalMemory')
  take(/^Total memory allocated (\d+)/, 'totalMemory')
  take(/^Dictionary memory allocated (\d+)/, 'dictMemory')
  take(/^Buffer pool size\s+(\d+)/, 'size')
  take(/^Free buffers\s+(\d+)/, 'free')
  take(/^Database pages\s+(\d+)/, 'database')
  take(/^Old database pages\s+(\d+)/, 'old')
  take(/^Modified db pages\s+(\d+)/, 'modified')
  take(/^Pending reads\s+(\d+)/, 'pendingReads')
  for (const l of lines) {
    let m
    if ((m = /^Buffer pool hit rate (\d+) \/ (\d+)/.exec(l))) { b.hit = Number(m[1]); b.hitOf = Number(m[2]) }
    if ((m = /^Pages read (\d+), created (\d+), written (\d+)/.exec(l))) { b.read = Number(m[1]); b.created = Number(m[2]); b.written = Number(m[3]) }
    if ((m = /^([\d.]+) reads\/s, ([\d.]+) creates\/s, ([\d.]+) writes\/s/.exec(l))) b.rates = `${m[1]} reads/s, ${m[2]} creates/s, ${m[3]} writes/s`
    if (/No buffer pool page gets since the last printout/.test(l)) b.noGets = true
  }
  // Some layouts print the hit rate only per instance: take the lowest.
  if (b.hit === undefined && individual) {
    for (const l of individual) {
      const m = /^Buffer pool hit rate (\d+) \/ (\d+)/.exec(l)
      if (m && (b.hit === undefined || Number(m[1]) < b.hit)) { b.hit = Number(m[1]); b.hitOf = Number(m[2]); b.hitFrom = 'lowest of the instances' }
    }
  }
  return b
}

// log reads LSN and checkpoint lines; the checkpoint age is shown as a share of Max checkpoint
// age (Percona Server prints it) or of a redo capacity the reader typed.
function log(lines, redoBytes) {
  const g = { lines: [] }
  for (const raw of lines) {
    const l = raw.trim()
    if (!l) continue
    g.lines.push(l)
    let m
    if ((m = /^Log sequence number\s+(\d+)/.exec(l))) g.lsn = Number(m[1])
    else if ((m = /^Log flushed up to\s+(\d+)/.exec(l))) g.flushed = Number(m[1])
    else if ((m = /^Pages flushed up to\s+(\d+)/.exec(l))) g.pagesFlushed = Number(m[1])
    else if ((m = /^Last checkpoint at\s+(\d+)/.exec(l))) g.checkpoint = Number(m[1])
    else if ((m = /^Max checkpoint age\s+(\d+)/.exec(l))) g.maxAge = Number(m[1])
    else if ((m = /^Checkpoint age target\s+(\d+)/.exec(l))) g.ageTarget = Number(m[1])
    else if ((m = /^Checkpoint age\s+(\d+)/.exec(l))) g.ageReported = Number(m[1])
    else if ((m = /^Log capacity\s+(\d+)/i.exec(l))) g.capacity = Number(m[1])
  }
  if (g.lsn !== undefined && g.checkpoint !== undefined) g.age = g.ageReported ?? g.lsn - g.checkpoint
  if (g.age !== undefined) {
    if (g.maxAge) { g.pct = (100 * g.age) / g.maxAge; g.pctOf = 'Max checkpoint age' } else if (redoBytes) { g.pct = (100 * g.age) / redoBytes; g.pctOf = 'the redo capacity entered' }
  }
  return g
}

function rowOps(lines) {
  const r = { lines: [] }
  for (const raw of lines) {
    const l = raw.trim()
    if (!l) continue
    r.lines.push(l)
    let m
    if ((m = /^(\d+) queries inside InnoDB, (\d+) queries in queue/.exec(l))) { r.inside = Number(m[1]); r.queue = Number(m[2]) }
    if ((m = /^(\d+) read views open inside InnoDB/.exec(l))) r.readViews = Number(m[1])
    if ((m = /Main thread (?:process no\. \d+, )?id (\d+), state:? ?=?\s*(.*)$/i.exec(l)) || (m = /Main thread ID=(\d+)\s*,\s*state[:=]\s*(.*)$/.exec(l))) r.mainState = m[2]
    if ((m = /^Number of rows inserted (\d+), updated (\d+), deleted (\d+), read (\d+)/.exec(l))) r.totals = { inserted: +m[1], updated: +m[2], deleted: +m[3], read: +m[4] }
    if ((m = /^([\d.]+) inserts\/s, ([\d.]+) updates\/s, ([\d.]+) deletes\/s, ([\d.]+) reads\/s/.exec(l)) && !r.rates) r.rates = { inserts: +m[1], updates: +m[2], deletes: +m[3], reads: +m[4] }
  }
  return r
}

// ------------------------------------------------------------------ findings

// findings: [{ tone, section, text }], most serious first.
export function findings(res) {
  const out = []
  const add = (tone, section, text) => out.push({ tone, section, text })
  const d = res.deadlock
  if (d && d.trxs.length) {
    const tables = [...new Set(d.trxs.flatMap((t) => [...t.holds, ...t.waits].map((l) => l.table)))]
    add('danger', 'LATEST DETECTED DEADLOCK', `Deadlock${d.time ? ` at ${d.time}` : ''} between ${d.trxs.length} transactions${tables.length ? ` on ${tables.join(', ')}` : ''}${d.rolledBack ? `; transaction (${d.rolledBack}) was rolled back` : ''}.`)
  }
  if (res.fkError) add('muted', 'LATEST FOREIGN KEY ERROR', 'A foreign key error is recorded.')
  const x = res.transactions
  if (x?.hll !== null && x?.hll !== undefined && x.hll >= HLL_WARN) add(x.hll >= HLL_DANGER ? 'danger' : 'warning', 'TRANSACTIONS', `History list length is ${fmtN(x.hll)}: undo not yet purged.`)
  for (const t of x?.list || []) {
    if ((t.active ?? 0) >= OLD_TRX) add(t.active >= OLD_TRX_DANGER ? 'danger' : 'warning', 'TRANSACTIONS', `Transaction ${t.trx}${t.thread ? ` (thread ${t.thread})` : ''} has been ACTIVE ${duration(t.active)}${t.undo ? `, ${fmtN(t.undo)} undo log entries` : ''}${t.rowLocks ? `, ${fmtN(t.rowLocks)} row locks` : ''}.`)
    if (t.waitingSec !== null) add('warning', 'TRANSACTIONS', `Transaction ${t.trx}${t.thread ? ` (thread ${t.thread})` : ''} has waited ${t.waitingSec} s for a lock${t.waitingFor ? ` on ${t.waitingFor.table}${t.waitingFor.index ? ` (${t.waitingFor.index})` : ''}` : ''}.`)
  }
  for (const w of res.semaphores?.waits || []) {
    if (w.seconds >= SEM_WAIT) add(w.seconds >= 240 ? 'danger' : 'warning', 'SEMAPHORES', `Thread ${w.thread} has waited ${fmtN(w.seconds)} s at ${w.at}${w.what[0] ? `: ${w.what[0]}` : ''}.`)
  }
  const b = res.bufferPool
  if (b?.hit !== undefined && b.hitOf && (1000 * b.hit) / b.hitOf < HIT_RATE) add('warning', 'BUFFER POOL AND MEMORY', `Buffer pool hit rate is ${b.hit} / ${b.hitOf} (${((100 * b.hit) / b.hitOf).toFixed(1)}%)${b.hitFrom ? `, ${b.hitFrom}` : ''}.`)
  if (b?.free !== undefined && b.size && b.free === 0) add('muted', 'BUFFER POOL AND MEMORY', 'No free pages in the buffer pool.')
  const g = res.log
  if (g?.pct !== undefined && g.pct >= 75) add(g.pct >= 90 ? 'danger' : 'warning', 'LOG', `Checkpoint age is ${g.pct.toFixed(1)}% of ${g.pctOf}.`)
  const rank = { danger: 0, warning: 1, muted: 2 }
  return out.sort((a, c) => rank[a.tone] - rank[c.tone])
}

export function duration(sec) {
  if (sec === null || sec === undefined) return '—'
  if (sec < 120) return `${sec} s`
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const d = Math.floor(h / 24)
  return d ? `${sec} s (${d} d ${h % 24} h)` : h ? `${sec} s (${h} h ${m} min)` : `${sec} s (${m} min)`
}
