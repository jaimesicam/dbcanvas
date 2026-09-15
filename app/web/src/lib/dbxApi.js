// Database Explorer — the API wrapper, and the pure helpers the grid, the chart
// builder and the exporters are built out of.
//
// The helpers live here rather than inside the components for one reason: they are
// the part with rules in it — what a NULL looks like, which columns a bar chart
// should suggest, how a CSV field is quoted — and rules are worth testing off-browser
// (see smoke/render.jsx). The components stay about layout.

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

// A connection id is opaque and goes in a path segment, so it is escaped rather than
// interpolated — it is the server's token, not a string this file gets to reason about.
const cid = (id) => encodeURIComponent(id)
const qs = (params) => {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== '') q.set(k, v)
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

export const dbxApi = {
  connections: () => request('GET', '/api/dbexplorer/connections'),
  connection: (id) => request('GET', `/api/dbexplorer/connections/${cid(id)}`),
  databases: (id) => request('GET', `/api/dbexplorer/connections/${cid(id)}/databases`),
  schemas: (id, database) => request('GET', `/api/dbexplorer/connections/${cid(id)}/schemas${qs({ database })}`),
  objects: (id, p) => request('GET', `/api/dbexplorer/connections/${cid(id)}/objects${qs(p)}`),
  object: (id, p) => request('GET', `/api/dbexplorer/connections/${cid(id)}/object${qs(p)}`),
  viewData: (id, p) => request('GET', `/api/dbexplorer/connections/${cid(id)}/viewdata${qs(p)}`),
  query: (req) => request('POST', '/api/dbexplorer/query', req),
  cancel: (queryId) => request('POST', '/api/dbexplorer/cancel', { queryId }),
  mutate: (req) => request('POST', '/api/dbexplorer/mutate', req),
  history: (limit) => request('GET', `/api/dbexplorer/history${qs({ limit })}`),
  clearHistory: () => request('DELETE', '/api/dbexplorer/history'),
  deleteHistory: (id) => request('DELETE', `/api/dbexplorer/history/${id}`),
  expose: (connectionId) => request('POST', '/api/dbexplorer/expose', { connectionId }),
  unexpose: (connectionId) => request('POST', '/api/dbexplorer/unexpose', { connectionId }),
  saved: () => request('GET', '/api/dbexplorer/saved'),
  save: (q) => request('POST', '/api/dbexplorer/saved', q),
  deleteSaved: (id) => request('DELETE', `/api/dbexplorer/saved/${id}`),
}

// ------------------------------------------------------------------ cell values

export const NULL_MARK = '∅'

// isNull distinguishes SQL NULL from every value that merely looks empty. The grid
// renders the two differently on purpose: an empty VARCHAR and a missing value are
// not the same fact, and a client that shows both as blank has thrown away the
// distinction somebody opened it to check.
export const isNull = (v) => v === null || v === undefined

export const isBinary = (v) => !!v && typeof v === 'object' && v.__dbx === 'binary'
export const isBigNum = (v) => !!v && typeof v === 'object' && v.__dbx === 'bignum'

// cellText is the value as text: what gets copied, exported and searched. It is
// deliberately not what gets displayed — display adds the NULL mark and truncation,
// and a copied cell must be the value rather than the decoration around it.
export function cellText(v) {
  if (isNull(v)) return ''
  if (isBigNum(v)) return v.text
  if (isBinary(v)) return `0x${v.hex}${v.len * 2 > v.hex.length ? '…' : ''}`
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

// cellDisplay is what the grid paints. Long values are cut here rather than by CSS,
// so a 4 MB text column costs a 200-character DOM node and not a 4 MB one.
export const CELL_MAX = 220
export function cellDisplay(v) {
  if (isNull(v)) return NULL_MARK
  if (isBinary(v)) return `BINARY[${v.len}] 0x${v.hex}${v.len * 2 > v.hex.length ? '…' : ''}`
  const s = cellText(v)
  return s.length > CELL_MAX ? `${s.slice(0, CELL_MAX)}…` : s
}

export const isTruncated = (v) => !isNull(v) && !isBinary(v) && cellText(v).length > CELL_MAX

// looksJson says a cell is worth offering the JSON viewer for. A column typed json
// answers yes on its own; a string column that happens to hold an object is the
// common case that a type alone would miss.
export function looksJson(v, col) {
  if (col && ['json', 'document', 'array'].includes(col.semanticType)) return true
  if (typeof v !== 'string') return false
  const s = v.trim()
  if (!(s.startsWith('{') || s.startsWith('['))) return false
  try { JSON.parse(s); return true } catch { return false }
}

export function prettyJson(v) {
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  try { return JSON.stringify(JSON.parse(s), null, 2) } catch { return s }
}

// NUMERIC columns are right-aligned and are the candidates for a chart's value axis.
export const NUMERIC = new Set(['number', 'integer'])
export const isNumericCol = (c) => !!c && NUMERIC.has(c.semanticType)
export const isTemporalCol = (c) => !!c && ['datetime', 'date', 'time'].includes(c.semanticType)

// cellNumber is the numeric value of a cell, or null. It is what the chart builder
// and the numeric sort read — a bignum is parsed, because a count that exceeds 2^53
// is still a count.
export function cellNumber(v) {
  if (isNull(v)) return null
  if (isBigNum(v)) { const n = Number(v.text); return Number.isFinite(n) ? n : null }
  if (typeof v === 'number') return Number.isFinite(v) ? v : null
  if (typeof v === 'boolean') return v ? 1 : 0
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v)
    return Number.isFinite(n) ? n : null
  }
  return null
}

// ------------------------------------------------------------------ sort & filter

// compareCells orders two non-NULL cells of one column. NULL is not its business —
// see sortRows, which handles it outside the direction.
export function compareCells(a, b, col) {
  if (isNumericCol(col)) {
    const x = cellNumber(a); const y = cellNumber(b)
    if (x !== null && y !== null) return x - y
  }
  return cellText(a).localeCompare(cellText(b), undefined, { numeric: true, sensitivity: 'base' })
}

export function sortRows(rows, index, dir, col) {
  if (index == null || !dir) return rows
  const sign = dir === 'desc' ? -1 : 1
  // A copy, because the unsorted order is the database's answer and re-sorting must
  // not be destructive — clearing the sort has to be able to restore it.
  //
  // NULLs sort last in *both* directions, which is the convention every database
  // client settled on. That is why the null test sits outside the sign: multiplying
  // it by -1 along with everything else would put a screen of empty cells at the top
  // of a descending sort, which is the opposite of what the sort was for.
  return [...rows].sort((r1, r2) => {
    const a = r1[index]; const b = r2[index]
    const an = isNull(a); const bn = isNull(b)
    if (an && bn) return 0
    if (an) return 1
    if (bn) return -1
    return sign * compareCells(a, b, col)
  })
}

// filterRows is the in-result search: a case-insensitive substring across every
// column. It searches the text of a cell, so a NULL is not matched by typing
// "null" — which would otherwise make every empty cell a hit.
export function filterRows(rows, term) {
  const t = (term || '').trim().toLowerCase()
  if (!t) return rows
  return rows.filter((r) => r.some((c) => !isNull(c) && cellText(c).toLowerCase().includes(t)))
}

// ------------------------------------------------------------------ export

// csvField quotes per RFC 4180, and writes NULL as an empty *unquoted* field while an
// empty string is written as a quoted one — the same distinction the grid makes,
// carried into the file so a round trip does not lose it.
export function csvField(v) {
  if (isNull(v)) return ''
  const s = cellText(v)
  if (s === '' || /[",\n\r]/.test(s)) return `"${s.replace(/"/g, '""')}"`
  return s
}

export function toCSV(columns, rows) {
  const head = columns.map((c) => csvField(c.name)).join(',')
  if (!rows.length) return `${head}\n`
  return `${head}\n${rows.map((r) => r.map(csvField).join(',')).join('\n')}\n`
}

// toJSON exports rows as objects. A duplicate column name would collapse, so it is
// disambiguated rather than silently dropped.
export function toJSON(columns, rows) {
  const names = []
  const seen = new Map()
  for (const c of columns) {
    const n = seen.get(c.name)
    if (n === undefined) { seen.set(c.name, 1); names.push(c.name) } else { seen.set(c.name, n + 1); names.push(`${c.name}_${n + 1}`) }
  }
  return JSON.stringify(
    rows.map((r) => Object.fromEntries(names.map((n, i) => [n, isBinary(r[i]) ? cellText(r[i]) : r[i]]))),
    null, 2,
  )
}

// download hands the browser a file. Built from a Blob rather than a data: URL so a
// large export does not have to fit in a URL.
export function download(name, text, mime = 'text/plain') {
  const url = URL.createObjectURL(new Blob([text], { type: mime }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export async function copyText(s) {
  try {
    await navigator.clipboard.writeText(s)
    return true
  } catch {
    return false
  }
}

// ------------------------------------------------------------------ charts

export const CHART_TYPES = [
  { id: 'table', label: 'Table' },
  { id: 'bar', label: 'Bar' },
  { id: 'hbar', label: 'Horizontal bar' },
  { id: 'line', label: 'Line' },
  { id: 'area', label: 'Area' },
  { id: 'pie', label: 'Pie' },
  { id: 'donut', label: 'Donut' },
  { id: 'scatter', label: 'Scatter' },
  { id: 'histogram', label: 'Histogram' },
]

export const AGGREGATIONS = [
  { id: 'none', label: 'None (one point per row)' },
  { id: 'sum', label: 'Sum' },
  { id: 'avg', label: 'Average' },
  { id: 'min', label: 'Minimum' },
  { id: 'max', label: 'Maximum' },
  { id: 'count', label: 'Count of rows' },
]

// suggestChart reads the shape of a result and proposes a visualisation. It only ever
// proposes: the suggestion is written into the builder's controls where it can be
// seen and changed, and nothing is aggregated or reordered behind the user's back. A
// result that suggests nothing stays a table, which is not a failure.
//
//   date | orders | revenue   →  line, date on X, the first numeric on Y
//   country | count           →  bar, country as the category, count as the value
export function suggestChart(columns) {
  const cols = columns || []
  const idx = (pred) => cols.map((c, i) => ({ c, i })).filter(({ c }) => pred(c))
  const num = idx(isNumericCol)
  const time = idx(isTemporalCol)
  const cat = idx((c) => !isNumericCol(c) && !isTemporalCol(c))
  const base = { series: null, topN: 0, sort: 'none', bins: 20 }
  if (!num.length) return { ...base, type: 'table', x: null, y: null, agg: 'none' }
  if (time.length) return { ...base, type: 'line', x: time[0].i, y: num[0].i, agg: 'none', sort: 'x' }
  if (cat.length) return { ...base, type: 'bar', x: cat[0].i, y: num[0].i, agg: 'sum', topN: 20, sort: 'value-desc' }
  if (num.length >= 2) return { ...base, type: 'scatter', x: num[0].i, y: num[1].i, agg: 'none' }
  return { ...base, type: 'histogram', x: num[0].i, y: null, agg: 'count' }
}

const SERIES_SEP = ' '

// buildChartData applies the builder's settings to the rows. Everything it does is a
// client-side transformation of the result, never a change to the query — which is
// why the panel labels it as such: the numbers on a chart with an aggregation set are
// not numbers the database returned.
export function buildChartData(columns, rows, spec) {
  const { type, x, y, series, agg = 'none', topN = 0, sort = 'none', bins = 20 } = spec || {}
  if (x == null || !rows) return { points: [], series: [], warning: '', clientSide: false }
  const xCol = columns[x]
  const yCol = y != null ? columns[y] : null

  if (type === 'histogram') return buildHistogram(rows, x, bins, xCol)

  const label = (v) => (isNull(v) ? NULL_MARK : cellText(v))
  const buckets = new Map()
  const seriesNames = []
  for (const r of rows) {
    const k = label(r[x])
    const s = series == null ? '' : label(r[series])
    if (series != null && !seriesNames.includes(s)) seriesNames.push(s)
    const id = `${k}${SERIES_SEP}${s}`
    let b = buckets.get(id)
    if (!b) { b = { k, s, vals: [], n: 0 }; buckets.set(id, b) }
    b.n += 1
    const v = yCol ? cellNumber(r[y]) : 1
    if (v !== null) b.vals.push(v)
  }

  const reduce = (b) => {
    const sum = () => b.vals.reduce((a, v) => a + v, 0)
    switch (agg) {
      case 'sum': return sum()
      case 'avg': return b.vals.length ? sum() / b.vals.length : null
      case 'min': return b.vals.length ? Math.min(...b.vals) : null
      case 'max': return b.vals.length ? Math.max(...b.vals) : null
      case 'count': return b.n
      default: return b.vals.length ? b.vals[b.vals.length - 1] : null
    }
  }

  let points = [...buckets.values()].map((b) => ({ x: b.k, series: b.s, y: reduce(b) }))
  const collapsed = agg !== 'none' && buckets.size < rows.length

  if (sort === 'value-desc') points.sort((a, b) => (b.y ?? -Infinity) - (a.y ?? -Infinity))
  else if (sort === 'value-asc') points.sort((a, b) => (a.y ?? Infinity) - (b.y ?? Infinity))
  else if (sort === 'x') points.sort((a, b) => String(a.x).localeCompare(String(b.x), undefined, { numeric: true }))

  const notes = []
  if (topN > 0 && points.length > topN) {
    notes.push(`showing the top ${topN} of ${buckets.size} categories`)
    points = points.slice(0, topN)
  }
  if (collapsed) notes.push(`${rows.length} rows grouped into ${buckets.size} by ${xCol ? xCol.name : 'the category'}`)
  return {
    points,
    series: series == null ? [] : seriesNames,
    warning: notes.join('; '),
    clientSide: agg !== 'none' || topN > 0 || sort !== 'none',
  }
}

// buildHistogram buckets one numeric column into equal-width bins over the observed
// range, which is the histogram everyone means when they say histogram.
function buildHistogram(rows, x, bins, xCol) {
  const vals = rows.map((r) => cellNumber(r[x])).filter((v) => v !== null)
  const name = xCol ? xCol.name : 'this column'
  if (!vals.length) return { points: [], series: [], warning: `${name} has no numeric values to bucket`, clientSide: true }
  const min = Math.min(...vals)
  const max = Math.max(...vals)
  const n = Math.max(1, Math.min(100, Math.trunc(bins) || 20))
  if (min === max) {
    return { points: [{ x: fmtNumber(min), series: '', y: vals.length }], series: [], warning: `every value of ${name} is ${fmtNumber(min)}`, clientSide: true }
  }
  const width = (max - min) / n
  const counts = new Array(n).fill(0)
  for (const v of vals) counts[Math.min(n - 1, Math.floor((v - min) / width))] += 1
  return {
    points: counts.map((c, i) => ({ x: `${fmtNumber(min + i * width)}–${fmtNumber(min + (i + 1) * width)}`, series: '', y: c })),
    series: [],
    warning: `${vals.length} values bucketed into ${n} bins`,
    clientSide: true,
  }
}

// fmtNumber is the one number format the charts and the grid's summaries share, so an
// axis and a tooltip never disagree about what a value is.
export function fmtNumber(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—'
  const a = Math.abs(v)
  if (a >= 1e12) return `${(v / 1e12).toFixed(1)}T`
  if (a >= 1e9) return `${(v / 1e9).toFixed(1)}B`
  if (a >= 1e6) return `${(v / 1e6).toFixed(1)}M`
  if (a >= 1e3) return `${(v / 1e3).toFixed(1)}k`
  if (a === 0) return '0'
  if (a < 0.01) return v.toExponential(1)
  if (Number.isInteger(v)) return String(v)
  return v.toFixed(2)
}

export function fmtBytes(n) {
  if (n === null || n === undefined) return ''
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = Number(n)
  let i = 0
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i += 1 }
  return `${i === 0 ? v : v.toFixed(1)} ${u[i]}`
}

export function fmtDuration(ms) {
  if (ms === null || ms === undefined) return ''
  if (ms < 1) return `${ms.toFixed(2)} ms`
  if (ms < 1000) return `${ms.toFixed(1)} ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(2)} s`
  return `${Math.floor(ms / 60000)}m ${((ms % 60000) / 1000).toFixed(0)}s`
}

// ------------------------------------------------------------------ misc

export const ENGINE_LABEL = {
  mysql: 'MySQL', postgres: 'PostgreSQL', mongodb: 'MongoDB',
  valkey: 'Valkey', clickhouse: 'ClickHouse',
}

export const KIND_ICON = {
  database: 'Database', schema: 'Folder', table: 'Table', view: 'Grid',
  matview: 'Grid', sequence: 'Unit', function: 'Code', procedure: 'Code',
  trigger: 'Trigger', dictionary: 'Bucket', collection: 'Table',
  key: 'Key', keyspace: 'Database', folder: 'Folder',
}

// DESTRUCTIVE are the statements the editor asks about before running. It is not a
// security control — a lab database is meant to be written to — it is the difference
// between running a DROP and running one on purpose.
const DESTRUCTIVE = /^(?:\s|--[^\n]*\n|\/\*[\s\S]*?\*\/)*(DROP|TRUNCATE|DELETE|ALTER|RENAME|GRANT|REVOKE|SHUTDOWN|FLUSH|RESET|KILL)\b/i

export const isDestructiveSQL = (sql) => DESTRUCTIVE.test(sql || '')

// splitStatements finds statement boundaries outside quotes and comments — the same
// rule the server splits on, so "run the statement I am in" picks the statement the
// server would then run.
export function splitStatements(sql) {
  const out = []
  let start = 0
  let q = null
  for (let i = 0; i < sql.length; i += 1) {
    const c = sql[i]
    if (q) {
      if (c === '\\' && q !== '`') { i += 1; continue }
      if (c === q) q = null
      continue
    }
    if (c === "'" || c === '"' || c === '`') { q = c; continue }
    if (c === '-' && sql[i + 1] === '-') { while (i < sql.length && sql[i] !== '\n') i += 1; continue }
    if (c === '/' && sql[i + 1] === '*') { const j = sql.indexOf('*/', i + 2); i = j < 0 ? sql.length : j + 1; continue }
    if (c === ';') {
      if (sql.slice(start, i).trim()) out.push({ start, end: i })
      start = i + 1
    }
  }
  if (sql.slice(start).trim()) out.push({ start, end: sql.length })
  return out
}

// statementAt is the statement the caret sits in, for Run-current-statement.
export function statementAt(sql, caret) {
  const bounds = splitStatements(sql || '')
  if (!bounds.length) return ''
  for (const b of bounds) {
    if (caret >= b.start && caret <= b.end) return sql.slice(b.start, b.end).trim()
  }
  const last = bounds[bounds.length - 1]
  return sql.slice(last.start, last.end).trim()
}

// formatSQL is a light, predictable reformat: the major keywords start a line, and
// nothing else moves. It deliberately does not try to be a full SQL formatter — a
// half-clever one that reflows a query wrongly is worse than none, and this one only
// ever moves whitespace.
const FORMAT_KEYWORDS = [
  'SELECT', 'FROM', 'WHERE', 'GROUP BY', 'HAVING', 'ORDER BY', 'LIMIT', 'OFFSET',
  'INNER JOIN', 'LEFT JOIN', 'RIGHT JOIN', 'FULL JOIN', 'CROSS JOIN', 'JOIN',
  'UNION ALL', 'UNION', 'INSERT INTO', 'VALUES', 'UPDATE', 'SET', 'DELETE FROM',
  'RETURNING', 'ON CONFLICT', 'WITH',
]
export function formatSQL(sql) {
  if (!sql || !sql.trim()) return sql
  let out = sql.replace(/\s+/g, ' ').trim()
  for (const kw of FORMAT_KEYWORDS) {
    // The matched text is re-emitted as it was written: a formatter that also
    // upper-cased keywords would be changing the statement, not laying it out, and
    // somebody who writes lower-case SQL did not ask for that.
    out = out.replace(new RegExp(`\\s+(${kw.replace(/ /g, '\\s+')})\\s+`, 'gi'), (_m, k) => `\n${k} `)
  }
  return out.replace(/\n{2,}/g, '\n').trim()
}
