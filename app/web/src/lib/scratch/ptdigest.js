// ptdigest.js — pt-query-digest (Percona Toolkit 3.x) in the browser: query fingerprints, Query
// IDs, the distilled "Item", a slow log parser, the event aggregator and the text report.
//
// It is a line-by-line port of the Perl packages pt-query-digest is built from — QueryRewriter
// (fingerprint, distill, shorten, convert_to_select), QueryParser (get_tables), SlowLogParser,
// EventAggregator (with its 1.05-wide log buckets, which is where 95%, stddev, median and V/M come
// from — they are not exact values), and QueryReportFormatter/ReportFormatter — with
// pt-query-digest's defaults: --group-by fingerprint, --order-by Query_time:sum, --limit 95%:20,
// --outliers Query_time:1:10, --expected-range 5,10, --attribute-aliases db|Schema,
// --inherit-attributes db,ts, no --preserve-embedded-numbers. The same input therefore gives the
// same fingerprints, the same Query IDs (0x + the 32 hex digits of the fingerprint's MD5, as 3.x
// prints them) and the same numbers.
//
// The Perl regexes are kept as they are and translated by P(): Perl's \s is [\t\n\v\f\r ] (JS adds
// Unicode spaces), '.' matches \r, \A \Z and $ differ, /m's ^ only follows \n. A plain list of
// queries is split the way pt-fingerprint splits a file (";" at the end of a line) and aggregated
// the way pt-query-digest --type rawlog does: every query counted once with Query_time 0, ranked
// by count.

import { md5Hex } from './md5.js'

// ------------------------------------------------------------------ Perl regex and value helpers

const WS = '\\t\\n\\v\\f\\r '
const cache = new Map()

// P compiles a Perl regex (a RegExp literal or source string, Perl flags i/m/s/g/y) into a JS one
// with Perl's meaning of \s \S . ^ $ \A \Z \z.
function P(re, flags = '') {
  const src = typeof re === 'string' ? re : re.source
  const key = flags + '\u0000' + src
  if (cache.has(key)) return cache.get(key)
  const dotall = flags.includes('s')
  const multi = flags.includes('m')
  let out = ''
  let inClass = false
  for (let i = 0; i < src.length; i++) {
    const c = src[i]
    if (c === '\\') {
      const n = src[++i]
      if (n === 's') out += inClass ? WS : `[${WS}]`
      else if (n === 'S') {
        if (inClass) throw new Error('\\S inside a class')
        out += `[^${WS}]`
      } else if (!inClass && n === 'A') out += '(?<![\\s\\S])'
      else if (!inClass && n === 'Z') out += '(?=\\n?(?![\\s\\S]))'
      else if (!inClass && n === 'z') out += '(?![\\s\\S])'
      else out += '\\' + n
      continue
    }
    if (inClass) {
      if (c === ']') inClass = false
      out += c
      continue
    }
    if (c === '[') {
      inClass = true
      out += c
      if (src[i + 1] === '^') { out += '^'; i++ }
      if (src[i + 1] === ']') { out += '\\]'; i++ }
      continue
    }
    if (c === '.' && !dotall) { out += '[^\\n]'; continue }
    if (c === '^') { out += multi ? '(?:(?<![\\s\\S])|(?<=\\n))' : '(?<![\\s\\S])'; continue }
    if (c === '$') { out += multi ? '(?=\\n|(?![\\s\\S]))' : '(?=\\n?(?![\\s\\S]))'; continue }
    out += c
  }
  const rx = new RegExp(out, flags.replace(/[mx]/g, ''))
  cache.set(key, rx)
  return rx
}

const esc = (s) => s.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')
// lc and uc only touch ASCII letters, as Perl does on a byte string.
const lc = (s) => s.replace(/[A-Z]+/g, (m) => m.toLowerCase())
const uc = (s) => s.replace(/[a-z]+/g, (m) => m.toUpperCase())
// perlTrue is Perl truthiness: undef, '', '0' and 0 are false.
const perlTrue = (v) => !(v === undefined || v === null || v === '' || v === '0' || v === 0)

// perlSplit is Perl's split(/re/, str): trailing empty fields are dropped.
function perlSplit(re, str) {
  const parts = String(str).split(re)
  while (parts.length && parts[parts.length - 1] === '') parts.pop()
  return parts
}

// num is Perl's string-to-number: the leading number, or 0.
function num(x) {
  if (typeof x === 'number') return x
  const m = /^[\t\n\v\f\r ]*([+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)/.exec(String(x ?? ''))
  return m ? Number(m[1]) : 0
}

// perlStr is Perl's number-to-string (%.15g); strings are returned as they are.
function perlStr(x) {
  if (typeof x === 'string') return x
  if (x === undefined || x === null) return ''
  if (!Number.isFinite(x)) return Number.isNaN(x) ? 'NaN' : x > 0 ? 'Inf' : '-Inf'
  if (x === 0) return '0'
  if (Number.isInteger(x) && Math.abs(x) < 1e15) return String(x)
  const [m, e] = x.toExponential(14).split('e')
  const exp = Number(e)
  const trim = (s) => (s.includes('.') ? s.replace(/0+$/, '').replace(/\.$/, '') : s)
  if (exp < -4 || exp >= 15) return trim(m) + 'e' + (exp < 0 ? '-' : '+') + String(Math.abs(exp)).padStart(2, '0')
  return trim(x.toFixed(14 - exp))
}

// fixed is C's %.<p>f: correctly rounded, exact halves to even (toFixed rounds them up).
function fixed(x, p) {
  const v = num(x)
  if (!Number.isFinite(v)) return perlStr(v)
  const neg = v < 0 || Object.is(v, -0)
  const a = Math.abs(v)
  let r = a.toFixed(p)
  if (a < 1e21) {
    const full = a.toFixed(100)
    const dot = full.indexOf('.')
    const tail = full.slice(dot + 1 + p)
    if (/^50*$/.test(tail)) {
      const trunc = full.slice(0, p ? dot + 1 + p : dot)
      if (Number(trunc[trunc.length - 1]) % 2 === 0) r = trunc
    }
  }
  return (neg ? '-' : '') + r
}

const str = (v) => (typeof v === 'number' ? perlStr(v) : String(v ?? ''))
const padL = (s, w) => String(s).padStart(w)
const padR = (s, w) => String(s).padEnd(w)
const utf8Len = (s) => new TextEncoder().encode(s).length

// ------------------------------------------------------------------ Transformers

// microT formats seconds the way pt-query-digest prints times (500ms, 12us, 3s).
export function microT(t, { pMs = 0, pS = 0 } = {}) {
  let tv = num(t)
  let ts = str(t)
  if (tv < 0) { tv = 0; ts = '0' }
  if (/e/.test(ts)) ts = fixed(tv, 17)
  ts = ts.replace(/\.(\d{1,6})\d*/, '.$1')
  tv = num(ts)
  if (tv > 0 && tv <= 0.000999) return perlStr(tv * 1000000) + 'us'
  if (tv >= 0.001 && tv <= 0.999999) return perlStr(num(fixed(tv * 1000, pMs))) + 'ms'
  if (tv >= 1) return perlStr(num(fixed(tv, pS))) + 's'
  return '0'
}

const UNITS = ['', 'k', 'M', 'G', 'T', 'P', 'E', 'Z', 'Y']

// shortenNum is Transformers::shorten: 1.50k, 2.00M, 12.
export function shortenNum(x, { p = 2, d = 1024 } = {}) {
  let v = num(x)
  let n = 0
  while (v >= d && n < UNITS.length - 1) { v /= d; n++ }
  const s = n === 0 ? str(x) : perlStr(v)
  return /\./.test(s) || n ? fixed(v, p) + UNITS[n] : String(Math.trunc(v))
}

function percentageOf(is, of) {
  return String(Math.trunc((num(is) * 100) / (num(of) || 1)))
}

// checksum is make_checksum: the uppercase MD5 hex of the fingerprint. pt-query-digest 3.x prints
// all 32 digits as the Query ID; 2.x printed the last 16.
export function checksum(fingerprint) {
  return md5Hex(fingerprint).toUpperCase()
}

export function queryId(fingerprint) {
  return '0x' + checksum(fingerprint)
}

// ------------------------------------------------------------------ QueryRewriter::fingerprint

const MLC = P(/\/\*[^!].*?\*\//, 'gs')
const OLC = P(/(?:--|#)[^'"\r\n]*(?=[\r\n]|\Z)/, 'g')
const VLC = P(/\/\*.*?[0-9]+.*?\*\//, 'gs')
const VLC_RF = P(/^(?:SHOW).*?\/\*![0-9]+(.*?)\*\//, 'ism')

// fingerprint is QueryRewriter::fingerprint: the query with its values abstracted (? and ?+),
// comments removed, whitespace collapsed and lowercased. `chomp` is the line ending Perl's chomp
// removes ("\n" in pt-query-digest, ";\n" in pt-fingerprint).
export function fingerprint(query, { chomp = '\n', embeddedNumbers = false } = {}) {
  let q = String(query ?? '')
  if (/^SELECT \/\*!40001 SQL_NO_CACHE \*\/ \* FROM `/.test(q)) return 'mysqldump'
  if (/\/\*\w+\.\w+:[0-9]\/[0-9]\*\//.test(q)) return 'percona-toolkit'
  if (/^administrator command: /.test(q)) return q
  let m = P(/\A\s*(call\s+\S+)\(/, 'i').exec(q)
  if (m) return lc(m[1])
  m = P(/\A((?:INSERT|REPLACE)(?: IGNORE)?\s+INTO.+?VALUES\s*\(.*?\))\s*,\s*\(/, 'is').exec(q)
  if (m) q = m[1]

  q = q.replace(MLC, '').replace(OLC, '')
  const use = P(/\Ause \S+\Z/, 'i')
  if (use.test(q)) return q.replace(use, 'use ?')

  q = q.replace(/([^\\])(\\')/g, '$1')
  q = q.replace(/([^\\])(\\")/g, '$1')
  q = q.replace(/\\\\/g, '')
  q = q.replace(/\\'/g, '')
  q = q.replace(/\\"/g, '')
  q = q.replace(/([^\\])("[\s\S]*?[^\\]?")/g, '$1?')
  q = q.replace(/([^\\])('[\s\S]*?[^\\]?')/g, '$1?')
  q = q.replace(/\bfalse\b|\btrue\b/gi, '?')
  q = embeddedNumbers ? q.replace(/\b[0-9+-][0-9a-f.xb+-]*/g, '?') : q.replace(/[0-9+-][0-9a-f.xb+-]*/g, '?')
  q = q.replace(/[xb.+-]\?/g, '?')

  q = q.replace(P(/\A\s+/), '')
  if (chomp && q.endsWith(chomp)) q = q.slice(0, -chomp.length)
  q = q.replace(/[ \n\t\r\f]+/g, ' ')
  q = lc(q)
  q = q.replace(/\bnull\b/g, '?')
  q = q.replace(P(/\b(in|values?)(?:[\s,]*\([\s?,]*\))+/, 'g'), '$1(?+)')
  q = q.replace(P(/\b(select\s.*?)(?:(\sunion(?:\sall)?)\s\1)+/, 'g'), '$1 /*repeat$2*/')
  q = q.replace(/\blimit \?(?:, ?\?| offset \?)?/, 'limit ?')

  const ob = /\bORDER BY /i.exec(q)
  if (ob) {
    const at = ob.index + ob[0].length
    q = q.slice(0, at) + q.slice(at).replace(P(/(.+?)\s+ASC/, 'giy'), '$1')
  }
  return q
}

// ------------------------------------------------------------------ QueryParser::get_tables

const TBL_IDENT = '(?:`[^`]+`|\\w+)(?:\\.(?:`[^`]+`|\\w+))?'
const TBL_REGEX = P(`\\b(?:FROM|JOIN|(?<!KEY\\s)UPDATE|INTO)\\b\\s*\\(?(${TBL_IDENT}(?:(?:\\s+(?:AS\\s+)?\\w+)?,\\s*${TBL_IDENT})*)`, 'gi')
const DDL = P(/^\s*((?:CREATE|ALTER|TRUNCATE|DROP|RENAME))\b/, 'i')

// getTables is QueryParser::get_tables: the table names a query reads or writes, as written.
export function getTables(query) {
  if (!perlTrue(query)) return []
  let q = String(query)
  const ddl = DDL.exec(q)
  if (ddl) {
    const stmt = ddl[1]
    q = q.replace(P(/IF\s+(?:NOT\s+)?EXISTS/, 'i'), '')
    if (P(esc(stmt) + ' DATABASE\\b', 'i').test(q)) return []
    if (/CREATE/i.test(stmt) && P(esc(stmt) + '\\b.+?\\bSELECT\\b', 'i').test(q)) {
      return getTables(P(/\b(SELECT\b.+)/, 'is').exec(q)[1])
    }
    const t = P(`TABLE\\s+(${TBL_IDENT})(\\s+.*)?`, 'i').exec(q)
    return t ? [t[1]] : []
  }
  q = q.replace(P(/(?:LOW_PRIORITY|IGNORE|STRAIGHT_JOIN|DELAYED)\s+/, 'gi'), ' ')
  const lock = P(/^\s*LOCK TABLES\s+/, 'i')
  if (lock.test(q)) {
    q = q.replace(lock, '')
    q = q.replace(P(/\s+(?:READ(?:\s+LOCAL)?|WRITE)\s*/, 'gi'), '')
    q = 'FROM ' + q
  }
  q = q.replace(/\\["']/g, '')
  q = q.replace(/"[\s\S]*?"/g, '?')
  q = q.replace(/'[\s\S]*?'/g, '?')
  if (P(/\A\s*(?:INSERT|REPLACE)(?!\s+INTO)/, 'i').test(q)) q = q.replace(P(/\A\s*((?:INSERT|REPLACE))\s+/, 'i'), '$1 INTO ')
  if (P(/\A\s*LOAD DATA/, 'i').test(q)) {
    const t = P(/INTO TABLE\s+(\S+)/, 'i').exec(q)
    return t ? [t[1]] : []
  }
  const tables = []
  for (const m of q.matchAll(TBL_REGEX)) {
    const tbls = m[1]
    if (P(/\ASELECT\b/, 'i').test(tbls)) continue
    for (let tbl of perlSplit(/,/, tbls)) {
      tbl = tbl.replace(P(`\\s*(${TBL_IDENT})(\\s+.*)?`, 'gi'), '$1')
      if (!/[a-zA-Z]/.test(tbl)) continue
      tables.push(tbl)
    }
  }
  return tables
}

// ------------------------------------------------------------------ QueryRewriter::distill

function stripComments(query) {
  if (!perlTrue(query)) return query
  let q = query.replace(MLC, '').replace(OLC, '')
  const m = VLC_RF.exec(q)
  if (m) {
    const qualifier = m[1] || ''
    q = q.replace(VLC, () => qualifier)
  }
  return q
}

const VERBS = P(/\b(^SHOW|^FLUSH|^COMMIT|^ROLLBACK|^BEGIN|SELECT|INSERT|UPDATE|DELETE|REPLACE|^SET|UNION|^START|^LOCK)\b/, 'gi')

function dedupe(list) {
  let last = ''
  return list.filter((v) => { const pass = v !== last; last = v; return pass })
}

function distillVerbs(query) {
  let m
  if ((m = P(/\A\s*call\s+(\S+)\(/, 'i').exec(query))) return ['CALL ' + m[1]]
  if (P(/\A\s*use\s+/).test(query)) return ['USE']
  if (P(/\A\s*UNLOCK TABLES/, 'i').test(query)) return ['UNLOCK']
  if ((m = P(/\A\s*xa\s+(\S+)/, 'i').exec(query))) return ['XA_' + m[1]]
  if (P(/\A\s*LOAD/, 'i').test(query)) {
    const t = P(/INTO TABLE\s+(\S+)/, 'i').exec(query)
    let tbl = t ? t[1] : ''
    if (!perlTrue(tbl)) tbl = ''
    return ['LOAD DATA ' + tbl.replace(/`/g, '')]
  }
  if (/^administrator command:/.test(query)) return [uc(query.replace('administrator command:', 'ADMIN'))]

  let q = stripComments(query) ?? ''
  if (P(/\A\s*SHOW\s+/, 'i').test(q)) {
    q = uc(q)
    q = q.replace(P(/\s+(?:SESSION|FULL|STORAGE|ENGINE)\b/, 'g'), ' ')
    q = q.replace(P(/\s+COUNT[^)]+\)/, 'g'), '')
    q = q.replace(P(/\s+(?:FOR|FROM|LIKE|WHERE|LIMIT|IN)\b.+/, 'ms'), '')
    q = q.replace(P(/\A(SHOW(?:\s+\S+){1,2}).*\Z/, 's'), '$1')
    q = q.replace(P(/\s+/, 'g'), ' ')
    return [q]
  }
  const dds = DDL.exec(q)
  if (dds) {
    const d = dds[1]
    q = q.replace(P(/\s+IF(?:\s+NOT)?\s+EXISTS/, 'i'), ' ')
    const o = P(esc(d) + '.+(DATABASE|TABLE)\\b', 'i').exec(q)
    const obj = o ? uc(o[1]) : ''
    const t = P(`(?:TABLE|DATABASE)\\s+(${TBL_IDENT})(\\s+.*)?`, 'i').exec(q)
    return [uc(d + (obj ? ' ' + obj : '')), t ? t[1] : undefined]
  }
  let verbs = dedupe([...q.matchAll(VERBS)].map((x) => uc(x[1])))
  if (verbs[0] === 'SELECT' && verbs.length > 1) verbs = verbs.includes('UNION') ? ['SELECT', 'UNION'] : ['SELECT']
  return [verbs.join(' ')]
}

// distill is QueryRewriter::distill, the profile's "Item": the verbs and the tables, e.g.
// "SELECT orders order_items". Numbers in table names become ?.
export function distill(query) {
  const q = String(query ?? '')
  const [verbs, table] = distillVerbs(q)
  if (verbs && /^SHOW/.test(verbs)) {
    let v = verbs
    for (const [a, b] of [['SCHEMA', 'DATABASE'], ['KEYS', 'INDEX'], ['INDEXES', 'INDEX']]) v = v.replace(a, b)
    return v
  }
  if (verbs && /^LOAD DATA/.test(verbs)) return verbs
  const tables = getTables(q).filter((t) => t !== undefined).map((t) => t.replace(/`/g, '').replace(/(_?)[0-9]+/g, '$1?'))
  if (perlTrue(table)) tables.push(table)
  return [verbs, ...dedupe(tables)].join(' ')
}

// ------------------------------------------------------------------ QueryRewriter::shorten and convert_to_select

function shortenList(snippet) {
  const vals = perlSplit(/,/, snippet)
  if (vals.length <= 20) return snippet
  return vals.slice(0, 20).join(',') + '/*... omitted ' + (vals.length - 20) + ' items ...*/'
}

// shortenQuery is QueryRewriter::shorten: a multi-row INSERT keeps its first row, and IN() lists
// longer than 20 items are cut when the query is over `length` characters.
export function shortenQuery(query, length) {
  let q = String(query ?? '').replace(
    P(/\A((?:INSERT|REPLACE)(?:\s+LOW_PRIORITY|DELAYED|HIGH_PRIORITY|IGNORE)?(?:\s\w+)*\s+\S+\s+VALUES\s*\(.*?\))\s*,\s*\(.*?(ON\s+DUPLICATE|\Z)/, 'si'),
    (_, a, b) => `${a} /*... omitted ...*/${b ?? ''}`)
  if (!P(/IN\s*\(\s*(?!select)/, 'i').test(q)) return q
  // The Perl loop compares against a length it never updates, so it runs at most once.
  if (length > 0 && q.length > length) {
    q = q.replace(P(/(\bIN\s*\()([^\)]+)(?=\))/, 'gsi'), (_, a, b) => a + shortenList(b))
  }
  return q
}

function balancedEnd(s, k) {
  let depth = 0
  for (let i = k; i < s.length; i++) {
    if (s[i] === '(') depth++
    else if (s[i] === ')') { depth--; if (depth === 0) return i + 1 }
  }
  return -1
}

// splitValues is the /($quote_re|[^,]*${bal}[^,]*|[^,]+)/g list match of __insert_to_select.
function splitValues(s) {
  const quote = P(/"(?:(?!(?<!\\)").)*"|'(?:(?!(?<!\\)').)*'/, 'y')
  const out = []
  let i = 0
  while (i < s.length) {
    quote.lastIndex = i
    const qm = quote.exec(s)
    if (qm) { out.push(qm[0]); i += qm[0].length; continue }
    let comma = s.indexOf(',', i)
    if (comma < 0) comma = s.length
    let end = -1
    for (let k = comma - 1; k >= i; k--) {
      if (s[k] !== '(') continue
      const e = balancedEnd(s, k)
      if (e < 0) continue
      let c2 = s.indexOf(',', e)
      end = c2 < 0 ? s.length : c2
      break
    }
    if (end < 0 && s[i] !== ',') end = comma
    if (end < 0) { i++; continue }
    out.push(s.slice(i, end))
    i = end
  }
  return out
}

// convertToSelect is QueryRewriter::convert_to_select: an UPDATE, DELETE or INSERT rewritten as
// the SELECT that reads the same rows (for EXPLAIN). Undefined when it cannot.
export function convertToSelect(query) {
  if (!perlTrue(query)) return undefined
  let q = String(query)
  if (P(/=\s*\(\s*SELECT /, 'i').test(q)) return undefined
  const U = P(/\A.*?update(?:\s+(?:low_priority|ignore))?\s+(.*?)\s+set\b(.*?)(?:\s*where\b(.*?))?(limit\s*[0-9]+(?:\s*,\s*[0-9]+)?)?\Z/, 'si')
  const I = P(/\A.*?(?:insert(?:\s+ignore)?|replace)\s+.*?\binto\b(.*?)\(([^\)]+)\)\s*values?\s*(\(.*?\))\s*(?:\blimit\b|on\s+duplicate\s+key.*)?\s*\Z/, 'si')
  const IS = P(/\A.*?(?:insert(?:\s+ignore)?|replace)\s+(?:.*?\binto)\b(.*?)\s*set\s+(.*?)\s*(?:\blimit\b|on\s+duplicate\s+key.*)?\s*\Z/, 'si')
  const D = P(/\A.*?delete\s+(.*?)\bfrom\b(.*)\Z/, 'si')
  if (U.test(q)) {
    q = q.replace(U, (_, from, set, where, limit) => `select ${set ?? ''} from ${from ?? ''} ` + (perlTrue(where) ? `where ${where}` : '') + (perlTrue(limit) ? ` ${limit} ` : ''))
  } else if (I.test(q)) {
    q = q.replace(I, (_, tbl, cols, vals) => {
      const c = perlSplit(/,/, cols)
      const v = splitValues(vals.replace(P(/^\(|\)$/, 'g'), ''))
      return c.length === v.length
        ? `select * from ${tbl} where ` + c.map((col, i) => `${col}=${v[i]}`).join(' and ')
        : `select * from ${tbl} limit 1`
    })
  } else if (IS.test(q)) {
    q = q.replace(IS, (_, from, set) => `select * from ${from} where ${set.replace(/,/g, ' and ')} `)
  } else if (D.test(q)) {
    q = q.replace(D, (_, del, join) => (/\bjoin\b/.test(join) ? `select 1 from ${join}` : `select * from ${join}`))
  }
  q = q.replace(P(/\s*on\s+duplicate\s+key\s+update.*\Z/, 'si'), '')
  q = q.replace(P(/\A.*?(?=\bSELECT\s*\b)/, 'ism'), '')
  return q
}

// ------------------------------------------------------------------ input

function makeReader(text) {
  let pos = 0
  return {
    next(sep) {
      if (pos >= text.length) return undefined
      const i = text.indexOf(sep, pos)
      const end = i < 0 ? text.length : i + sep.length
      const chunk = text.slice(pos, end)
      pos = end
      return chunk
    },
    tell: () => pos,
  }
}

// byteOffsets turns character offsets into the byte offsets Perl's tell() reports.
function byteOffsets(text) {
  if (!/[^\x00-\x7f]/.test(text)) return (i) => i
  let lastC = 0, lastB = 0
  return (i) => {
    if (i < lastC) { lastC = 0; lastB = 0 }
    lastB += utf8Len(text.slice(lastC, i))
    lastC = i
    return lastB
  }
}

const TS_LINE = /^# Time: ((?:[0-9: ]{15})|(?:[-0-9: T]{19}))/
const UH_LINE = P(/# User@Host: ([^\[]+|\[[^[]+\]).*?@ (\S*) \[(.*)\]\s*(?:Id:\s*(\d+))?/)
const HD_LINE = P(/^(?:T[cC][pP]\s[pP]ort:\s+\d+|[\/A-Z].*mysqld,\sVersion.*(?:started\swith:|embedded\slibrary)|Time\s+Id\s+Command).*\n/, 'gm')
const SEP = ';\n#'

// parseSlowLog is SlowLogParser: one event per query, with every "# Name: value" attribute, the
// time, user, host, database (use ...;) and the query text as `arg`.
export function parseSlowLog(text) {
  const src = String(text ?? '')
  const r = makeReader(src)
  const bytes = byteOffsets(src)
  const pending = []
  const events = []
  for (;;) {
    let posInLog = r.tell()
    let ev = null
    while (!ev) {
      let stmt = pending.length ? pending.shift() : r.next(SEP)
      if (stmt === undefined) return events
      const props = [['cmd', 'Query'], ['pos_in_log', bytes(posInLog)]]
      posInLog = r.tell()
      HD_LINE.lastIndex = 0
      if (HD_LINE.test(stmt)) {
        stmt = stmt.replace(HD_LINE, '')
        const chunks = perlSplit(SEP, stmt)
        if (chunks.length > 1) {
          stmt = chunks.shift()
          pending.unshift(...chunks)
        } else if (chunks.length === 1) stmt = chunks[0]
        else stmt = ''
      }
      if (!stmt.startsWith('#')) stmt = '#' + stmt
      stmt = stmt.replace(P(/;\n#?\Z/), '')

      let gotTs = false, gotUh = false, gotAc = false, gotDb = false, gotSet = false
      let foundArg = false
      let skip = false
      const len = stmt.length
      let start = 0
      while (start < len || (start === 0 && len === 0)) {
        const nl = stmt.indexOf('\n', start)
        const end = nl < 0 ? len : nl
        const line = stmt.slice(start, end)
        const pos = end
        start = end + 1
        if (/^(?:#|use |SET (?:last_insert_id|insert_id|timestamp))/.test(line)) {
          let m
          const uh = () => {
            const u = UH_LINE.exec(line)
            if (!u) return false
            props.push(['user', u[1]], ['host', perlTrue(u[2]) ? u[2] : u[3]], ['ip', u[3]])
            if (perlTrue(u[4])) props.push(['Thread_id', u[4]])
            gotUh = true
            return true
          }
          if (!gotTs && (m = TS_LINE.exec(line))) {
            props.push(['ts', m[1]])
            gotTs = true
            if (!gotUh) uh()
          } else if (!gotUh && uh()) {
            // user and host
          } else if (!gotAc && /^# (?:administrator command:[^\n]*)$/.test(line)) {
            const arg = line.replace(P(/^#\s+/), '')
            props.push(['cmd', 'Admin'], ['arg', arg], ['bytes', utf8Len(arg)])
            foundArg = true
            gotAc = true
          } else if (/^# +[A-Z][A-Za-z_]+: [^\t\n\v\f\r ]+/.test(line)) {
            let l = line
            if (P(/Schema:\s+\w+: /).test(l)) l = l.replace(P(/Schema:\s+/), '')
            for (const p of l.matchAll(P(/(\w+):\s+(\S+|$)/, 'g'))) props.push([p[1], p[2]])
          } else if (!gotDb && (m = /^use ([^;]+)/.exec(line))) {
            props.push(['db', m[1]])
            gotDb = true
          } else if (!gotSet && (m = P(/^SET\s+([^;]*)/).exec(line))) {
            const kv = perlSplit(P(/,|\s*=\s*/), m[1])
            for (let i = 0; i < kv.length; i += 2) props.push([kv[i], kv[i + 1]])
            gotSet = true
          }
          if (!foundArg && pos === len) {
            const l = r.next(';\n')
            if (l !== undefined) {
              if (P(/^\s*[A-Z][a-z_]+: /).test(l)) {
                const rest = r.next(SEP)
                pending.push(l + (rest ?? ''))
                props.push(['cmd', 'Query'], ['arg', '/* No query */'], ['bytes', 0])
                foundArg = true
              } else {
                let a = l.endsWith(';\n') ? l.slice(0, -2) : l
                a = a.replace(P(/^\s+/), '')
                props.push(['cmd', 'Admin'], ['arg', a], ['bytes', utf8Len(a)])
                foundArg = true
              }
            } else {
              skip = true
              break
            }
          }
        } else {
          const arg = stmt.slice(pos - line.length)
          props.push(['arg', arg], ['bytes', utf8Len(arg)])
          break
        }
        if (len === 0) break
      }
      if (skip) continue
      ev = {}
      for (const [k, v] of props) if (k !== undefined && v !== undefined) ev[k] = v
    }
    events.push(ev)
  }
}

// parseQueryList splits queries the way pt-fingerprint reads a file — a query ends with ";" at
// the end of a line, "#" comment lines are dropped — and makes rawlog events of them.
export function parseQueryList(text) {
  const src = String(text ?? '')
  const r = makeReader(src)
  const bytes = byteOffsets(src)
  const events = []
  for (;;) {
    const pos = r.tell()
    let q = r.next(';\n')
    if (q === undefined) return events
    if (q.endsWith(';\n')) q = q.slice(0, -2)
    q = q.replace(P(/^#.+$/, 'mg'), '')
    q = q.replace(P(/^\s+/), '')
    if (!/^\w/.test(q)) continue
    events.push({ pos_in_log: bytes(pos), cmd: 'Query', bytes: utf8Len(q), Query_time: 0, arg: q })
  }
}

// isSlowLog tells a slow log from a list of queries.
export function isSlowLog(text) {
  return /^# (?:Time|User@Host|Query_time|Thread_id|Schema):/m.test(String(text ?? ''))
}

// ------------------------------------------------------------------ EventAggregator

const BUCK_SIZE = 1.05
const BASE_LOG = Math.log(BUCK_SIZE)
const BASE_OFFSET = Math.abs(1 - Math.log(0.000001) / BASE_LOG)
const NUM_BUCK = 1000
const MIN_BUCK = 0.000001
const BUCK_VALS = Array.from({ length: NUM_BUCK }, (_, b) => (b === 0 ? 0 : BUCK_SIZE ** (b - 1) * MIN_BUCK))

export function bucketIdx(v) {
  if (v < MIN_BUCK) return 0
  const idx = Math.trunc(BASE_OFFSET + Math.log(v) / BASE_LOG)
  return idx > NUM_BUCK - 1 ? NUM_BUCK - 1 : idx
}

let buckTens = null
function bucketsOf() {
  if (buckTens) return buckTens
  buckTens = []
  let start = 0
  const starts = [0]
  for (let i = 1; i <= 7; i++) starts.push(10 ** i * MIN_BUCK)
  for (let b = 0; b < starts.length - 1; b++) {
    const next = bucketIdx(starts[b + 1])
    for (let i = start; i < next; i++) buckTens[i] = b
    start = next
  }
  for (let i = start; i < NUM_BUCK; i++) buckTens[i] = 7
  return buckTens
}

const IGNORE = new Set(['arg', 'cmd', 'insert_id', 'ip', 'port', 'Thread_id', 'timestamp', 'exptime', 'flags', 'key', 'res', 'val', 'server_id', 'offset', 'end_log_pos', 'Xid'])
const TYPE_FOR = {
  val: 'string', key_print: 'string', Status_code: 'string', Statement_id: 'string', Error_no: 'string', Last_errno: 'string',
  Thread_id: 'string', InnoDB_trx_id: 'string', host: 'string', ip: 'string', port: 'string', Killed: 'bool', rate_limit: 'string',
}
const NUM_RE = /^(?:\d+|[+-]?(?:(?=\d|[.])\d+(?:[.])\d{0,})(?:E[+-]?\d+)?)$/i
const UNROLL_LIMIT = 1000

class Aggregator {
  constructor(worst) {
    this.worst = worst
    this.attributes = []
    this.attributeSet = new Set()
    this.typeFor = { ...TYPE_FOR }
    this.handlers = new Map()
    this.classes = new Map()
    this.globals = {}
    this.samples = new Map()
    this.n = 0
    this.frozen = null
  }

  makeHandler(attrib, ev) {
    const first = ev[attrib]
    if (first === undefined || first === null) return null
    const type = this.typeFor[attrib] || (/_crc$/.test(attrib) ? 'string' : NUM_RE.test(str(first)) ? 'num' : /^(?:Yes|No)$/.test(str(first)) ? 'bool' : 'string')
    this.typeFor[attrib] = type
    const numeric = type === 'num'
    const sum = type === 'num' || type === 'bool'
    const unq = type === 'bool' || type === 'string'
    const lt = numeric ? (a, b) => num(a) < num(b) : (a, b) => str(a) < str(b)
    const gt = numeric ? (a, b) => num(a) > num(b) : (a, b) => str(a) > str(b)
    const worst = this.worst === attrib
    return (e, cls, glob, g) => {
      let v = e[attrib]
      if (v === undefined || v === null) return
      if (type === 'bool') v = (perlTrue(v) ? v : '') === 'Yes' ? 1 : 0
      if (attrib === 'Query_time') {
        const m = P(/^(\d+(?:\.\d+)?).*/).exec(str(v))
        if (m) v = m[1]
        e[attrib] = v
      }
      for (const place of [cls, glob]) {
        place.cnt = (place.cnt || 0) + 1
        if (/_crc$/.test(attrib)) v = num(v) % 1000
        if (place.min === undefined || lt(v, place.min)) place.min = v
        if (place.max === undefined || gt(v, place.max)) place.max = v
        if (sum) place.sum = (place.sum || 0) + num(v)
        if (numeric) {
          place.all ||= {}
          const i = bucketIdx(num(v))
          place.all[i] = (place.all[i] || 0) + 1
        }
      }
      if (unq) {
        cls.unq ||= {}
        cls.unq[str(v)] = (cls.unq[str(v)] || 0) + 1
      }
      if (worst && (numeric ? num(v) >= num(perlTrue(cls.max) ? cls.max : 0) : str(v) >= str(perlTrue(cls.max) ? cls.max : 0))) this.samples.set(g, e)
    }
  }

  aggregate(ev) {
    const g = ev.fingerprint
    if (g === undefined) return
    this.n++
    if (this.n <= UNROLL_LIMIT) {
      for (const k of Object.keys(ev)) {
        if (k !== 'fingerprint' && !this.attributeSet.has(k) && !IGNORE.has(k)) { this.attributeSet.add(k); this.attributes.push(k) }
      }
      for (const a of this.attributes) {
        if (!(a in ev)) continue
        if (!this.classes.has(g)) this.classes.set(g, {})
        const cls = this.classes.get(g)
        cls[a] ||= {}
        this.globals[a] ||= {}
        let h = this.handlers.get(a)
        if (!h) { h = this.makeHandler(a, ev); if (h) this.handlers.set(a, h) }
        if (!h) continue
        if (!this.samples.has(g)) this.samples.set(g, ev)
        h(ev, cls[a], this.globals[a], g)
      }
      return
    }
    // Past 1,000 events pt-query-digest compiles a loop over the attributes it has handlers for
    // and stops looking for new ones.
    this.frozen ||= this.attributes.filter((a) => this.handlers.has(a))
    if (!this.classes.has(g)) this.classes.set(g, Object.fromEntries(this.frozen.map((a) => [a, {}])))
    const cls = this.classes.get(g)
    if (!this.samples.has(g)) this.samples.set(g, ev)
    for (const a of this.frozen) this.handlers.get(a)(ev, cls[a] || {}, this.globals[a], g)
  }
}

// calcMetrics is EventAggregator::_calc_metrics: 95th percentile, stddev and median from buckets.
function calcMetrics(vals, args) {
  const res = { pct_95: 0, stddev: 0, median: 0, cutoff: undefined }
  if (!vals || !Object.keys(vals).length || !args.cnt) return res
  const n = args.cnt
  if (n === 1 || num(args.max) === num(args.min)) {
    const v = perlTrue(args.max) ? args.max : 0
    return { pct_95: v, stddev: 0, median: v, cutoff: n }
  }
  if (n === 2) {
    const v = perlTrue(args.max) ? args.max : 0
    const mean = (num(perlTrue(args.min) ? args.min : 0) + num(v)) / 2
    return { pct_95: v, stddev: Math.sqrt(((num(v) - mean) ** 2) * 2), median: mean, cutoff: n }
  }
  const cutoff = n >= 10 ? Math.trunc(n * 0.95) : n
  res.cutoff = cutoff
  let totalLeft = n
  const topVals = n - cutoff
  let sumExcl = 0, sum = 0, sumsq = 0
  const mid = Math.trunc(n / 2)
  let median = 0
  let prev = NUM_BUCK - 1
  let bucket95 = 0
  for (let b = NUM_BUCK - 1; b >= 0; b--) {
    const val = vals[b] || 0
    if (!val) continue
    totalLeft -= val
    sumExcl += val
    if (!bucket95 && sumExcl > topVals) bucket95 = b
    if (!median && totalLeft <= mid) {
      median = (cutoff % 2) || val > 1 ? BUCK_VALS[b] : (BUCK_VALS[b] + BUCK_VALS[prev]) / 2
    }
    sum += val * BUCK_VALS[b]
    sumsq += val * BUCK_VALS[b] ** 2
    prev = b
  }
  const variance = sumsq / n - (sum / n) ** 2
  let stddev = variance > 0 ? Math.sqrt(variance) : 0
  const maxstdev = (num(args.max || 0) - num(args.min || 0)) / 2
  if (stddev > maxstdev) stddev = maxstdev
  res.stddev = stddev
  res.pct_95 = BUCK_VALS[bucket95]
  res.median = median
  return res
}

// ------------------------------------------------------------------ the digest

const EXPECTED_RANGE = [5, 10]

function topEvents(classes, metrics, { attrib, orderby, total, count, olAttrib, olLimit, olFreq }) {
  const val = (g) => classes.get(g)?.[attrib]?.[orderby]
  const sorted = [...classes.keys()].filter((g) => val(g) !== undefined)
    .sort((a, b) => (num(val(a)) === num(val(b)) ? (a < b ? -1 : a > b ? 1 : 0) : num(val(a)) - num(val(b))))
    .reverse()
  const chosen = [], other = []
  let t = 0, c = 0
  for (const g of sorted) {
    if ((!total || t < total) && (!count || c < count)) chosen.push([g, 'top', c + 1])
    else if (olAttrib && (!olFreq || (classes.get(g)[olAttrib]?.cnt || 0) >= olFreq)) {
      const st = metrics.get(g)?.[olAttrib]
      if (num(st?.pct_95 || 0) >= olLimit) chosen.push([g, 'outlier', c + 1])
      else other.push([g, 'misc', c + 1])
    } else other.push([g, 'misc', c + 1])
    t += num(val(g))
    c++
  }
  return { chosen, other }
}

// digest runs pt-query-digest over a slow log, or over a list of queries (see parseQueryList).
// `limit` is --limit (95%:20). It returns the aggregate and the ranked classes;
// formatReport/formatProfile print it.
export function digest(text, { kind, limit = '95%:20' } = {}) {
  let src = String(text ?? '')
  if (src && !src.endsWith('\n')) src += '\n'
  const k = kind || (isSlowLog(src) ? 'slowlog' : 'queries')
  const events = k === 'slowlog' ? parseSlowLog(src) : parseQueryList(src)
  const orderby = k === 'slowlog' ? 'sum' : 'cnt'
  const ea = new Aggregator('Query_time')
  const last = {}
  for (const ev of events) {
    if (!perlTrue(ev.arg)) continue
    ev.fingerprint = fingerprint(ev.arg, { chomp: k === 'slowlog' ? '\n' : ';\n' })
    if (ev.db !== undefined) { ev.db = ev.db.replace(/^`/, '').replace(/`$/, ''); delete ev.Schema }
    else if (ev.Schema !== undefined) { ev.db = ev.Schema.replace(/^`/, '').replace(/`$/, ''); delete ev.Schema }
    for (const a of ['db', 'ts']) {
      if (ev[a] !== undefined) last[a] = ev[a]
      else if (last[a] !== undefined) ev[a] = last[a]
    }
    if (/^administrator/.test(ev.arg)) for (const a of ['Rows_sent', 'Rows_examined', 'Rows_read', 'Rows_affected']) if (a in ev) ev[a] = 0
    if (perlTrue(ev.Log_slow_rate_limit)) {
      ev.rate_limit = (perlTrue(ev.Log_slow_rate_type) ? ev.Log_slow_rate_type : 'session') + ':' + ev.Log_slow_rate_limit
      delete ev.Log_slow_rate_limit
      delete ev.Log_slow_rate_type
    }
    ea.aggregate(ev)
  }

  const metrics = new Map()
  const globalMetrics = {}
  for (const [attrib, g] of Object.entries(ea.globals)) {
    if (g.all) globalMetrics[attrib] = calcMetrics(g.all, g)
    for (const [cls, store] of ea.classes) {
      if (store[attrib]?.all) {
        if (!metrics.has(cls)) metrics.set(cls, {})
        metrics.get(cls)[attrib] = calcMetrics(store[attrib].all, store[attrib])
      }
    }
  }

  const attrib = 'Query_time'
  const qtSum = ea.globals[attrib]?.sum || 0
  let total, count
  if (/^\d+$/.test(limit)) count = Number(limit)
  else {
    const n = String(limit).match(/\d+/g) || []
    total = Number(n[0] || 0) * (qtSum / 100)
    count = n[1] === undefined ? undefined : Number(n[1])
  }
  const { chosen, other } = topEvents(ea.classes, metrics, {
    attrib, orderby, total, count, olAttrib: 'Query_time', olLimit: 1, olFreq: 10,
  })
  const explainWhy = chosen.length < EXPECTED_RANGE[0] || chosen.length > EXPECTED_RANGE[1]

  const classMetric = (g, a) => {
    const store = ea.classes.get(g)?.[a] || {}
    const m = metrics.get(g)?.[a] || {}
    return {
      cnt: store.cnt, sum: store.sum, min: store.min, max: store.max,
      avg: perlTrue(store.sum) && store.cnt ? store.sum / store.cnt : 0,
      median: perlTrue(m.median) ? m.median : 0, pct_95: perlTrue(m.pct_95) ? m.pct_95 : 0, stddev: perlTrue(m.stddev) ? m.stddev : 0,
    }
  }

  // Every class, in pt-query-digest's ranking order; `reported` is false for the ones its profile
  // folds into MISC.
  const ranked = [...chosen, ...other].sort((a, b) => a[2] - b[2])
  const classes = ranked.map(([g, reason, rank]) => {
    const qt = classMetric(g, 'Query_time')
    const sample = ea.samples.get(g) || {}
    const store = ea.classes.get(g)
    const unq = (a) => Object.entries(store[a]?.unq || {}).sort((x, y) => y[1] - x[1] || (x[0] < y[0] ? -1 : 1)).map(([v, n]) => ({ value: v, count: n }))
    const stat = (a) => (store[a]?.cnt ? { ...classMetric(g, a), pct: percentageOf(store[a].sum, ea.globals[a]?.sum) } : null)
    return {
      rank, reason, reported: reason !== 'misc',
      fingerprint: g, checksum: checksum(g), id: '0x' + checksum(g),
      item: distill(sample.arg || '').replace(P(/^\s+/), '').replace(P(/\s+$/), '').replace(/\n/g, ' '),
      count: qt.cnt || 0, time: num(qt.sum || 0),
      timePct: qtSum ? (num(qt.sum || 0) / qtSum) * 100 : 0,
      rCall: qt.cnt ? num(qt.sum || 0) / qt.cnt : 0,
      vm: num(qt.stddev) ** 2 / (num(qt.avg) || 1),
      sample: sample.arg || '', db: sample.db,
      stats: { Query_time: stat('Query_time'), Lock_time: stat('Lock_time'), Rows_sent: stat('Rows_sent'), Rows_examined: stat('Rows_examined') },
      dbs: unq('db'), users: unq('user'), hosts: unq('host'),
    }
  })

  return { kind: k, events: ea.n, ea, metrics, globalMetrics, chosen, other, explainWhy, orderby, classes }
}

// ------------------------------------------------------------------ ReportFormatter

function reportTable(title, cols, lines, lineWidth = 74) {
  cols = cols.map((c) => ({ ...c }))
  let used = 0
  const auto = []
  cols.forEach((c, i) => {
    if (c.width) c.widthPct = Math.ceil((c.width * 100) / lineWidth)
    if (c.widthPct) used += c.widthPct
    else { c.auto = true; auto.push(i) }
    c.minVal = 0; c.maxVal = 0; c.headerWidth = c.name.length
    c.rightMost = i === cols.length - 1
  })
  if (auto.length) {
    const per = Math.trunc((100 - used) / auto.length)
    for (const i of auto) cols[i].widthPct = per
  }
  let minHdr = cols.reduce((s, c) => s + c.name.length, 0) + (cols.length - 1)
  const truncHeaders = minHdr > lineWidth
  const rows = lines.map((vals) => vals.map((v, i) => {
    let s = str(v).replace(P(/^\s+/, 'g'), '').replace(P(/\s+$/), '')
    const c = cols[i]
    c.minVal = Math.min(s.length, c.minVal || s.length)
    c.maxVal = Math.max(s.length, c.maxVal || s.length)
    return s
  }))
  let extra = 0
  for (const c of cols) {
    let pw = Math.trunc(lineWidth * (c.widthPct / 100))
    if (c.auto) {
      if (c.minVal && pw < c.minVal) pw = c.minVal
      else if (c.maxVal && pw > c.maxVal) { extra += pw - c.maxVal; pw = c.maxVal }
    }
    c.printWidth = pw
  }
  while (extra-- > 0) {
    for (const c of cols) if (c.auto && (c.printWidth < c.maxVal || c.printWidth < c.headerWidth)) c.printWidth++
  }
  const trunc = (c, val, width, side) => {
    if (val.length <= width) return val
    if (c.rightMost) return val
    const mark = '...'
    return (side || 'right') === 'right' ? val.slice(0, width - mark.length) + mark : mark + val.slice(-width + mark.length)
  }
  if (truncHeaders) for (const c of cols) if (c.name.length > c.printWidth) c.name = trunc(c, c.name, c.printWidth, 'left')
  for (const r of rows) r.forEach((v, i) => { const c = cols[i]; if (c.printWidth && v.length > c.printWidth) r[i] = trunc(c, v, c.printWidth) })
  const fmt = (vals, leftAll) => '# ' + cols.map((c, i) => {
    const v = vals[i]
    if (c.rightMost && !c.rightJustify) return v
    return c.rightJustify && !leftAll ? padL(v, c.printWidth) : padR(v, c.printWidth)
  }).join(' ')
  const cut = (line, strip) => {
    if (strip) line = line.replace(P(/\s+$/), '')
    return line.length > lineWidth ? line.slice(0, lineWidth) : line
  }
  const out = []
  if (title) out.push('# ' + title)
  out.push(cut(fmt(cols.map((c) => c.name), true), true))
  out.push(cut(fmt(cols.map((c) => '='.repeat(c.printWidth))), false))
  for (const r of rows) out.push(fmt(r.map((v) => v.replace(/\n/g, ' '))))
  return out.join('\n') + '\n'
}

// ------------------------------------------------------------------ QueryReportFormatter

const LINE_LENGTH = 74
const LABEL_WIDTH = 12
const HIDDEN = new Set(['arg', 'fingerprint', 'pos_in_log', 'ts'])
const NUM_ORDER = ['Query_time', 'Exec_orig_time', 'Transmit_time', 'Lock_time', 'Rows_sent', 'Rows_examined', 'Rows_affected', 'Rows_read', 'Bytes_sent', 'Merge_passes', 'Tmp_tables', 'Tmp_disk_tables', 'Tmp_table_sizes', 'bytes']

const numFormat = (...v) => `# ${padR(v[0], LABEL_WIDTH)} ${padL(v[1] ?? '', 3)} ${[2, 3, 4, 5, 6, 7, 8].map((i) => padL(v[i] ?? '', 7)).join(' ')}`
const rstrip = (s) => s.replace(P(/\s+$/), '')
const cmp = (a, b) => (a < b ? -1 : a > b ? 1 : 0)

function sortAttribs(ea) {
  const groups = { num: [], innodb: [], bool: [], string: [] }
  for (const a of ea.attributes) {
    if (HIDDEN.has(a)) continue
    const type = ea.typeFor[a] || 'string'
    if (type === 'num') (/^InnoDB_/.test(a) ? groups.innodb : groups.num).push(a)
    else if (type === 'bool') groups.bool.push(a)
    else if (type === 'string') groups.string.push(a)
  }
  const order = (a) => { const i = NUM_ORDER.indexOf(a); return i < 0 ? undefined : i }
  groups.num.sort((a, b) => {
    const oa = order(a), ob = order(b)
    if (oa === undefined && ob === undefined) return cmp(a, b)
    if (oa !== undefined && ob !== undefined) return oa - ob
    return oa === undefined ? 1 : -1
  })
  for (const k of ['innodb', 'bool', 'string']) groups[k].sort((a, b) => cmp(uc(a), uc(b)))
  return groups
}

function makeLabel(val) {
  if (!perlTrue(val)) return ''
  let v = val.replace(/_/g, ' ')
  if (/^InnoDB/.test(v)) {
    v = v.replace(/^InnoDB /, '')
    v = v === 'trx id' ? 'InnoDB trxID' : v.slice(0, LABEL_WIDTH)
  }
  const alias = { user: 'Users', db: 'Databases', 'Query time': 'Exec time', host: 'Hosts', 'Error no': 'Errors', bytes: 'Query size', 'Tmp disk tables': 'Tmp disk tbl', 'Tmp table sizes': 'Tmp tbl size' }
  return alias[v] ?? v.slice(0, LABEL_WIDTH)
}

function formatStringList(vals, classCnt) {
  if (!vals.unq) return str(vals.cnt)
  const keys = Object.keys(vals.unq)
  if (keys.length === 1) {
    let s = keys[0]
    if (s.length > LINE_LENGTH - 30) s = s.slice(0, LINE_LENGTH - 30) + '...'
    return s
  }
  const top = keys.sort((a, b) => vals.unq[b] - vals.unq[a] || cmp(a, b))
  let line = ''
  let i = 0
  for (const s of top) {
    let p = /(?:\d+\.){3}\d+/.test(s) ? s : s.length > 10 ? s.slice(0, 10) + '...' : s
    p += ` (${vals.unq[s]}/${percentageOf(vals.unq[s], classCnt)}%)`
    if (line.length + p.length > LINE_LENGTH - 27) break
    line += p + ', '
    i++
  }
  line = line.replace(/, $/, '')
  if (i < top.length) line += '... ' + (top.length - i) + ' more'
  return line
}

const MYSQL_TS = /^(\d\d)(\d\d)(\d\d) +(\d+):(\d+):(\d+)(\.\d+)?$/
const PROPER_TS = /^(\d\d\d\d)-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)(\.\d+)?$/

function parseTimestamp(val) {
  const m = MYSQL_TS.exec(val)
  if (m) {
    const [, y, mo, d, h, i, s, f] = m
    const sec = f !== undefined ? fixed(Number(s) + Number(f), 6).padStart(9, '0') : String(Number(s)).padStart(2, '0')
    return `${Number(y) + 2000}-${mo.padStart(2, '0')}-${String(Number(d)).padStart(2, '0')} ${String(Number(h)).padStart(2, '0')}:${String(Number(i)).padStart(2, '0')}:${sec}`
  }
  return val
}

// unixTimestamp only feeds differences (QPS, concurrency), so the time zone does not matter.
function unixTimestamp(val) {
  const m = PROPER_TS.exec(val)
  if (!m) return num(val)
  const [, y, mo, d, h, i, s, us] = m
  let t = Date.UTC(+y, +mo - 1, +d, +h, +i, +s) / 1000
  if (us !== undefined) t = num(String(t) + fixed(Number(us), 6).replace(/^0\./, '.'))
  return t
}

function formatTimeRange(vals) {
  const min = parseTimestamp(str(vals.min ?? ''))
  const max = parseTimestamp(str(vals.max ?? ''))
  if (min && max && min === max) return 'all events occurred at ' + min
  const minDay = min ? min.split(' ')[0] : ''
  const maxDay = max ? max.split(' ')[0] : ''
  let mx = max
  if (minDay === maxDay) mx = max.split(' ')[1]
  return min && mx ? `${min} to ${mx}` : ''
}

// tsDiff is the seconds between a class's first and last `ts`, when they differ.
function tsDiff(store, cnt) {
  if (!cnt || !store || !(str(store.max ?? '') > str(store.min ?? ''))) return undefined
  return unixTimestamp(parseTimestamp(str(store.max))) - unixTimestamp(parseTimestamp(str(store.min)))
}

function header(res) {
  const { ea } = res
  const g = ea.globals
  const cnt = g.Query_time?.cnt || 0
  let qps = 0, conc = 0
  const diff = tsDiff(g.ts, cnt)
  if (diff !== undefined) {
    qps = cnt / (diff || 1)
    conc = diff ? num(g.Query_time.sum) / diff : 0
  }
  const out = []
  if (g.rate_limit) out.push('# Rate limits apply')
  let line = `# Overall: ${shortenNum(cnt, { d: 1000 })} total, ${shortenNum(ea.classes.size, { d: 1000 })} unique, ${shortenNum(qps, { d: 1000 })} QPS, ${shortenNum(conc, { d: 1000 })}x concurrency `
  line += '_'.repeat(Math.max(0, LINE_LENGTH - line.length + LABEL_WIDTH - 12))
  out.push(line)
  if (g.ts) out.push('# Time range: ' + (formatTimeRange(g.ts) || 'unknown'))
  out.push(numFormat('Attribute', '', 'total', 'min', 'max', 'avg', '95%', 'stddev', 'median'))
  out.push(numFormat('='.repeat(LABEL_WIDTH), '   ', ...Array(7).fill('='.repeat(7))))
  const attribs = sortAttribs(ea)
  for (const type of ['num', 'innodb']) {
    if (type === 'innodb' && attribs.innodb.length) out.push('# InnoDB:')
    for (const a of attribs[type]) {
      const store = g[a]
      if (!store) continue
      const m = res.globalMetrics[a] || {}
      const f = /time|wait$/.test(a) ? microT : shortenNum
      const vals = [store.sum, store.min, store.max, num(store.sum) / store.cnt, m.pct_95, m.stddev, m.median].map((v) => (v === undefined ? '' : f(v)))
      out.push(numFormat(makeLabel(a), '', ...vals))
    }
  }
  if (attribs.bool.length) {
    out.push('# Boolean:')
    let printed = false
    for (const a of attribs.bool) {
      const store = g[a]
      if (!store) continue
      if (num(store.sum) > 0) {
        out.push(`# ${padR(makeLabel(a), LABEL_WIDTH)} ${padL(percentageOf(store.sum, store.cnt), 3)}% yes, ${padL(percentageOf(store.cnt - store.sum, store.cnt), 3)}% no`)
        printed = true
      }
    }
    if (!printed) out.pop()
  }
  return out.map(rstrip).join('\n') + '\n'
}

// formatProfile prints pt-query-digest's "# Profile" block.
export function formatProfile(res) {
  const { ea, chosen, other } = res
  const totalR = num(ea.globals.Query_time?.sum || 0)
  const lines = []
  const byRank = new Map(res.classes.map((c) => [c.fingerprint, c]))
  for (const [g, , rank] of [...chosen].sort((a, b) => a[2] - b[2])) {
    const c = byRank.get(g)
    const store = ea.classes.get(g).Query_time
    const r = num(store.sum)
    lines.push([rank, c.id, `${padL(fixed(r, 4), 10)} ${padL(fixed((r / (totalR || 1)) * 100, 1), 4)}%`, store.cnt, padL(fixed(r / store.cnt, 4), 8), padL(fixed(c.vm, 2), 4), distill(ea.samples.get(g)?.arg || '')])
  }
  if (other.length) {
    let r = 0, cnt = 0
    for (const [g] of other) { r += num(ea.classes.get(g).Query_time.sum); cnt += ea.classes.get(g).Query_time.cnt }
    lines.push(['MISC', '0xMISC', `${padL(fixed(r, 4), 10)} ${padL(fixed((r / (totalR || 1)) * 100, 1), 4)}%`, cnt, padL(fixed(r / cnt, 4), 8), '0.0', `<${other.length} ITEMS>`])
  }
  return reportTable('Profile', [
    { name: 'Rank', rightJustify: true },
    { name: 'Query ID', width: 35 },
    { name: 'Response time', rightJustify: true },
    { name: 'Calls', rightJustify: true },
    { name: 'R/Call', rightJustify: true },
    { name: 'V/M', rightJustify: true, width: 5 },
    { name: 'Item' },
  ], lines)
}

function chartDistro(store) {
  const vals = store?.all
  if (!vals || !Object.keys(vals).length) return ''
  const tens = bucketsOf()
  const distro = Array(8).fill(0)
  for (let i = 1; i < NUM_BUCK; i++) distro[tens[i]] += vals[i] || 0
  const maxVal = Math.max(0, ...distro)
  const perMark = maxVal / 64
  const labels = ['1us', '10us', '100us', '1ms', '10ms', '100ms', '1s', '10s+']
  const out = ['# Query_time distribution']
  distro.forEach((n, i) => {
    let marks = n / (perMark || 1)
    if (marks < 1 && n > 0) marks = 1
    out.push(`# ${padL(labels[i], 5)}${marks ? '  ' : ''}${'#'.repeat(Math.trunc(marks))}`)
  })
  return out.join('\n') + '\n'
}

function splitUnquote(dbTbl) {
  let [db, tbl] = dbTbl.split('.')
  if (!perlTrue(tbl)) { tbl = db; db = undefined }
  const clean = (s) => (perlTrue(s) ? s.replace(/^`/, '').replace(/`$/, '').replace(/``/g, '`') : s)
  return [clean(db), clean(tbl)]
}

function eventReport(res, g, rank, reason) {
  const { ea } = res
  const store = ea.classes.get(g)
  const globalCnt = ea.globals.Query_time?.cnt
  const classCnt = store.Query_time?.cnt
  let qps = 0, conc = 0
  const diff = tsDiff(store.ts, globalCnt)
  if (diff) {
    qps = classCnt / diff
    conc = num(store.Query_time.sum) / diff
  }
  const qt = ea.classes.get(g).Query_time
  const m = res.metrics.get(g)?.Query_time || {}
  const avg = perlTrue(qt.sum) && qt.cnt ? qt.sum / qt.cnt : 0
  const vm = num(perlTrue(m.stddev) ? m.stddev : 0) ** 2 / (avg || 1)
  const out = []
  let line = `# Query ${rank}: ${shortenNum(qps, { d: 1000 })} QPS, ${shortenNum(conc, { d: 1000 })}x concurrency, ID 0x${checksum(g)} at byte ${fixed(ea.samples.get(g)?.pos_in_log || 0, 0)} `
  line += '_'.repeat(Math.max(0, LINE_LENGTH - line.length + LABEL_WIDTH - 12))
  out.push(line)
  if (reason) out.push('# This item is included in the report because it matches ' + (reason === 'top' ? '--limit.' : '--outliers.'))
  out.push(`# Scores: V/M = ${fixed(vm, 2)}`)
  if (store.ts) out.push('# Time range: ' + (formatTimeRange(store.ts) || 'unknown'))
  out.push(numFormat('Attribute', 'pct', 'total', 'min', 'max', 'avg', '95%', 'stddev', 'median'))
  out.push(numFormat('='.repeat(LABEL_WIDTH), '===', ...Array(7).fill('='.repeat(7))))
  out.push(numFormat('Count', percentageOf(classCnt, globalCnt), classCnt))
  const attribs = sortAttribs(ea)
  for (const type of ['num', 'innodb']) {
    const rows = []
    for (const a of attribs[type]) {
      const vals = store[a]
      if (!vals || !Object.keys(vals).length) continue
      const f = /time|wait$/.test(a) ? microT : shortenNum
      const mm = res.metrics.get(g)?.[a] || {}
      const v = [vals.sum, vals.min, vals.max, num(vals.sum) / vals.cnt, mm.pct_95, mm.stddev, mm.median].map((x) => (x === undefined ? '' : f(x)))
      rows.push(numFormat(makeLabel(a), percentageOf(vals.sum, ea.globals[a]?.sum), ...v))
    }
    if (type === 'innodb' && rows.length) out.push('# InnoDB:')
    out.push(...rows)
  }
  const bools = []
  for (const a of attribs.bool) {
    const vals = store[a]
    if (!vals || !Object.keys(vals).length) continue
    if (num(vals.sum) > 0) bools.push(`# ${padR(makeLabel(a), LABEL_WIDTH)} ${padL(percentageOf(vals.sum, vals.cnt), 3)}% yes, ${padL(percentageOf(vals.cnt - vals.sum, vals.cnt), 3)}% no`)
  }
  if (bools.length) out.push('# Boolean:', ...bools)
  const strs = []
  for (const a of attribs.string) {
    const vals = store[a]
    if (!vals || !Object.keys(vals).length) continue
    strs.push(`# ${padR(makeLabel(a), LABEL_WIDTH)} ${formatStringList(vals, classCnt)}`)
  }
  if (strs.length) out.push('# String:', ...strs)
  return out.map(rstrip).join('\n') + '\n'
}

// formatQueryReport prints the per-query sections that follow the profile.
export function formatQueryReport(res) {
  const { ea } = res
  let report = ''
  for (const [g, why, rank] of res.chosen) {
    const sample = ea.samples.get(g) || {}
    const samp = sample.arg || ''
    const store = ea.classes.get(g)
    const defaultDb = perlTrue(sample.db) ? sample.db : store.db?.unq ? String(Object.keys(store.db.unq).length) : undefined
    const seen = new Set()
    const tables = []
    for (const t of getTables(samp)) {
      if (!perlTrue(t) || seen.has(t)) continue
      seen.add(t)
      const [db, tbl] = splitUnquote(t)
      tables.push([perlTrue(db) ? db : defaultDb, tbl])
    }
    if (rank > 1) report += '\n'
    report += eventReport(res, g, rank, res.explainWhy ? why : '')
    report += chartDistro(store.Query_time)
    const shortened = shortenQuery(samp, 1024)
    if (tables.length) {
      report += '# Tables\n'
      for (const [db, tbl] of tables) {
        report += '#    SHOW TABLE STATUS' + (perlTrue(db) ? ` FROM \`${db}\`` : '') + ` LIKE '${tbl}'\\G\n`
        report += '#    SHOW CREATE TABLE ' + [db, tbl].filter(perlTrue).map((v) => '`' + v.replace(/`/g, '``') + '`').join('.') + '\\G\n'
      }
    }
    if (/^(?:[(\t\n\v\f\r ]*select|insert|replace)/.test(g)) {
      if (/^(?:insert|replace)/.test(g)) report += `${shortened}\\G\n`
      else report += `# EXPLAIN /*!50100 PARTITIONS*/\n${shortened}\\G\n`
    } else {
      report += `${shortened}\\G\n`
      const converted = convertToSelect(shortened)
      if (perlTrue(converted) && P(/^[\(\s]*select/, 'i').test(converted)) report += `# Converted for EXPLAIN\n# EXPLAIN /*!50100 PARTITIONS*/\n${converted}\\G\n`
    }
  }
  return report
}

// formatReport prints what pt-query-digest prints after its rusage/date/hostname/files lines.
export function formatReport(res) {
  return header(res) + '\n' + formatProfile(res) + '\n' + formatQueryReport(res)
}

// The report's number formats, for the card: 500ms, 1.50k, and C's %.Nf.
export { microT as formatTime, shortenNum as formatCount, fixed as formatFixed }
