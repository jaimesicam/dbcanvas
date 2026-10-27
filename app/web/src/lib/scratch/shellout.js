// shellout.js — reading what the database shells print: psql tables (aligned, boxed and `\x`
// expanded), mysql tables (boxed, `\G` vertical and tab-separated batch output), and mongosh
// documents (relaxed: ISODate(…), Timestamp({ t, i }), Long(…), unquoted keys, single quotes —
// or strict / Extended JSON).
//
// Used by the Replication status card (replstatus.js) and the Config compare card (cnfdiff.js).
// Every reader takes the pasted text as it is, prompts and row counts included, and skips what it
// does not recognise.

// ------------------------------------------------------------------ tables

// readTables finds every table in a paste and returns [{ form, columns, rows }], where each row is
// an object keyed by column name (as printed). `form` is 'aligned' (psql), 'boxed' (mysql, or psql
// with border 2), 'expanded' (psql \x), 'vertical' (mysql \G) or 'tabs' (mysql -e / --batch).
export function readTables(text) {
  const lines = String(text ?? '')
    .replace(/\r\n/g, '\n')
    .replace(/[│┃]/g, '|').replace(/[─━]/g, '-').replace(/[┼┬┴├┤┌┐└┘╋]/g, '+')
    .split('\n')
  const out = []
  let i = 0
  while (i < lines.length) {
    const l = lines[i]
    let r = null
    if (/^\*{5,} *\d+\. row *\*{5,}\s*$/.test(l)) r = readVertical(lines, i)
    else if (/^\s*-\[ RECORD \d+ \]/.test(l)) r = readExpanded(lines, i)
    else if (/^\s*\+-[-+]*\+\s*$/.test(l) && /^\s*\|/.test(lines[i + 1] || '')) r = readBoxed(lines, i)
    else if (/^\s*-+(\+-+)*\s*$/.test(l) && /-/.test(l) && i > 0 && lines[i - 1].includes('|') === l.includes('+') && lines[i - 1].trim()) r = readAligned(lines, i)
    else if (l.includes('\t') && !/^\s*\|/.test(l) && (lines[i + 1] || '').split('\t').length === l.split('\t').length) r = readTabs(lines, i)
    if (r) { out.push(r.table); i = r.next; continue }
    i++
  }
  // A \G paste without its "*** 1. row ***" line is still one row.
  if (!out.length) {
    const r = readVertical(['', ...lines], 0)
    if (r && Object.keys(r.table.rows[0] || {}).length >= 3) out.push(r.table)
  }
  return out
}

const END = /^\s*\(\d+ rows?\)\s*$|^\s*\d+ rows? in set|^\s*Empty set/

// mysql \G: "  Key: value" lines, a value running on until the next key line (GTID sets wrap).
function readVertical(lines, i) {
  const rows = []
  let row = null
  let last = null
  let j = i
  for (; j < lines.length; j++) {
    const l = lines[j]
    if (/^\*{5,} *\d+\. row *\*{5,}\s*$/.test(l) || (j === i && !row)) { row = {}; rows.push(row); last = null; if (j === i) continue }
    if (END.test(l) || /^\S*(mysql|MySQL|mariadb|MariaDB)[^>]*>/.test(l)) { j++; break }
    const m = /^\s*([A-Za-z_][A-Za-z0-9_]*): ?(.*)$/.exec(l)
    if (m) { row[m[1]] = m[2].trimEnd(); last = m[1]; continue }
    if (last && l.trim()) row[last] += `\n${l.trim()}`
  }
  const columns = [...new Set(rows.flatMap((r) => Object.keys(r)))]
  return { table: { form: 'vertical', columns, rows }, next: j }
}

// psql \x: "-[ RECORD n ]---" then "key | value"; an empty key continues the previous value.
function readExpanded(lines, i) {
  const rows = []
  let row = null
  let last = null
  let j = i
  for (; j < lines.length; j++) {
    const l = lines[j]
    if (/^\s*-\[ RECORD \d+ \]/.test(l)) { row = {}; rows.push(row); last = null; continue }
    if (!l.trim() || END.test(l)) { if (rows.length && !/^\s*-\[ RECORD/.test(lines[j + 1] || '')) { j++; break } continue }
    const bar = l.indexOf('|')
    if (bar < 0) break
    const k = l.slice(0, bar).trim()
    const v = l.slice(bar + 1).replace(/^ /, '').replace(/\s+\+$/, '').trimEnd()
    if (!k && last) { row[last] += `\n${v}`; continue }
    row[k] = v
    last = k
  }
  const columns = [...new Set(rows.flatMap((r) => Object.keys(r)))]
  return { table: { form: 'expanded', columns, rows }, next: j }
}

const cells = (l) => l.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim())

// mysql (and psql border 2): +---+ / | header | / +---+ / | row | ... / +---+.
// A value with a newline in it breaks the row; the broken-off lines are joined back.
function readBoxed(lines, i) {
  const body = []
  let j = i + 1
  let rules = 1
  for (; j < lines.length; j++) {
    const l = lines[j]
    if (/^\s*\+-[-+]*\+\s*$/.test(l)) { rules++; if (rules >= 3) { j++; break } continue }
    if (/^\s*\|/.test(l)) body.push(l)
    else if (body.length && l.trim() && !END.test(l)) body[body.length - 1] += `\n${l}`
    else break
  }
  if (!body.length) return null
  const columns = cells(body[0])
  const rows = body.slice(1).map((l) => {
    const c = cells(l.replace(/\n/g, '\u0000')).map((x) => x.replace(/\u0000/g, '\n'))
    return Object.fromEntries(columns.map((k, n) => [k, c[n] ?? '']))
  })
  return { table: { form: 'boxed', columns, rows }, next: j }
}

// psql aligned: " a | b " / "---+---" / rows / "(n rows)".
function readAligned(lines, i) {
  // No outer border here, so a trailing "|" is an empty last cell, not a border.
  const cells = (l) => l.split('|').map((c) => c.trim())
  const columns = cells(lines[i - 1])
  const rows = []
  let j = i + 1
  for (; j < lines.length; j++) {
    const l = lines[j]
    if (!l.trim() || END.test(l)) { j++; break }
    const c = cells(l)
    if (c.length !== columns.length) continue
    // A multi-line value: psql ends the cell with "+" and carries on in the next line, whose
    // other cells are empty.
    const prev = rows[rows.length - 1]
    if (prev && !c[0] && columns.length > 1 && Object.values(prev).some((v) => /\+$/.test(v))) {
      c.forEach((x, n) => { if (x) prev[columns[n]] = `${prev[columns[n]].replace(/\s*\+$/, '')}\n${x}` })
      continue
    }
    rows.push(Object.fromEntries(columns.map((k, n) => [k, c[n]])))
  }
  return { table: { form: 'aligned', columns, rows }, next: j }
}

// mysql -e / --batch: tab-separated, a header line first.
function readTabs(lines, i) {
  const columns = lines[i].split('\t').map((c) => c.trim())
  const rows = []
  let j = i + 1
  for (; j < lines.length; j++) {
    const l = lines[j]
    if (!l.includes('\t')) break
    const c = l.split('\t')
    rows.push(Object.fromEntries(columns.map((k, n) => [k, (c[n] ?? '').trim()])))
  }
  return { table: { form: 'tabs', columns, rows }, next: j }
}

// ------------------------------------------------------------------ mongosh documents

// A BSON Timestamp: seconds since the epoch and an ordinal within that second.
export class Ts {
  constructor(t, i) { this.t = Number(t); this.i = Number(i) }
  toString() { return `Timestamp(${this.t}, ${this.i})` }
}

// readShellDoc parses the first { … } or [ … ] in a paste as mongosh (or the legacy mongo shell)
// prints it, or as strict / Extended JSON. ISODate and $date become Date, Timestamp and $timestamp
// become Ts, Long / NumberLong / $numberLong become numbers, ObjectId / $oid become hex strings.
// Returns { value } or { error }.
export function readShellDoc(text) {
  const s = String(text ?? '')
  // A shell prompt ("rs0 [direct: primary] admin>") can come first: try each { or [ in turn.
  let first = null
  const starts = [...s.matchAll(/[{[]/g)].map((m) => m.index).slice(0, 20)
  for (const start of starts) {
    const p = { s, i: start }
    try {
      return { value: unwrap(value(p)) }
    } catch (e) {
      first ??= `${e.message} (at character ${p.i})`
    }
  }
  return { error: first || 'no { … } document found' }
}

function ws(p) {
  for (;;) {
    while (p.i < p.s.length && /\s/.test(p.s[p.i])) p.i++
    if (p.s.startsWith('//', p.i)) { while (p.i < p.s.length && p.s[p.i] !== '\n') p.i++; continue }
    if (p.s.startsWith('/*', p.i)) { const e = p.s.indexOf('*/', p.i + 2); p.i = e < 0 ? p.s.length : e + 2; continue }
    return
  }
}

function value(p) {
  ws(p)
  const c = p.s[p.i]
  if (c === '{') return object(p)
  if (c === '[') return array(p)
  if (c === '"' || c === "'") return string(p)
  const num = /^-?(?:\d+\.?\d*(?:e[+-]?\d+)?|\.\d+|Infinity)/i.exec(p.s.slice(p.i, p.i + 64))
  if (num) { p.i += num[0].length; return Number(num[0]) }
  const id = /^[A-Za-z_$][\w$.]*/.exec(p.s.slice(p.i, p.i + 64))
  if (!id) throw new Error(`unexpected "${c ?? 'end of text'}"`)
  p.i += id[0].length
  let name = id[0]
  if (name === 'new') { ws(p); const n = /^[A-Za-z_$][\w$]*/.exec(p.s.slice(p.i)); p.i += n[0].length; name = n[0] }
  ws(p)
  if (p.s[p.i] !== '(') return { true: true, false: false, null: null, undefined: null, NaN: NaN }[name] ?? name
  p.i++
  const args = []
  ws(p)
  while (p.s[p.i] !== ')') {
    if (p.i >= p.s.length) throw new Error(`unterminated ${name}(`)
    args.push(value(p))
    ws(p)
    if (p.s[p.i] === ',') { p.i++; ws(p) }
  }
  p.i++
  return construct(name, args)
}

function construct(name, args) {
  const a = args[0]
  switch (name) {
    case 'ISODate': case 'Date': return a === undefined ? null : new Date(a)
    case 'Timestamp':
      if (a && typeof a === 'object') return new Ts(a.t, a.i)
      return new Ts(a, args[1] ?? 0)
    case 'Long': case 'NumberLong': case 'NumberInt': case 'Int32': case 'Double': case 'Decimal128': case 'NumberDecimal':
      return Number(a)
    default: return a === undefined ? name : String(a)
  }
}

function object(p) {
  const o = {}
  p.i++
  for (;;) {
    ws(p)
    if (p.s[p.i] === '}') { p.i++; return o }
    if (p.i >= p.s.length) throw new Error('unterminated {')
    let key
    const c = p.s[p.i]
    if (c === '"' || c === "'") key = string(p)
    else {
      const m = /^[^\s:{}[\],]+/.exec(p.s.slice(p.i, p.i + 256))
      if (!m) throw new Error(`expected a key, found "${c}"`)
      key = m[0]
      p.i += key.length
    }
    ws(p)
    if (p.s[p.i] !== ':') throw new Error(`expected ":" after ${key}`)
    p.i++
    o[key] = value(p)
    ws(p)
    if (p.s[p.i] === ',') p.i++
  }
}

function array(p) {
  const a = []
  p.i++
  for (;;) {
    ws(p)
    if (p.s[p.i] === ']') { p.i++; return a }
    if (p.i >= p.s.length) throw new Error('unterminated [')
    if (p.s.startsWith('...', p.i)) { while (p.i < p.s.length && !/[,\]]/.test(p.s[p.i])) p.i++ } // "... 3 more items"
    else a.push(value(p))
    ws(p)
    if (p.s[p.i] === ',') p.i++
  }
}

function string(p) {
  const q = p.s[p.i++]
  let out = ''
  while (p.i < p.s.length && p.s[p.i] !== q) {
    if (p.s[p.i] === '\\') {
      const e = p.s[++p.i]
      if (e === 'u') { out += String.fromCharCode(parseInt(p.s.slice(p.i + 1, p.i + 5), 16)); p.i += 5; continue }
      out += { n: '\n', t: '\t', r: '\r', b: '\b', f: '\f' }[e] ?? e
      p.i++
      continue
    }
    out += p.s[p.i++]
  }
  p.i++
  return out
}

// unwrap turns Extended JSON wrappers ({ "$date": … }, { "$timestamp": … }, …) into values.
function unwrap(v) {
  if (Array.isArray(v)) return v.map(unwrap)
  if (!v || typeof v !== 'object' || v instanceof Date || v instanceof Ts) return v
  const keys = Object.keys(v)
  if (keys.length === 1) {
    const [k] = keys
    const x = v[k]
    if (k === '$date') return new Date(typeof x === 'object' && x ? Number(x.$numberLong) : x)
    if (k === '$timestamp') return new Ts(x.t, x.i)
    if (['$numberLong', '$numberInt', '$numberDouble', '$numberDecimal'].includes(k)) return Number(x)
    if (k === '$oid') return String(x)
  }
  return Object.fromEntries(keys.map((k) => [k, unwrap(v[k])]))
}
