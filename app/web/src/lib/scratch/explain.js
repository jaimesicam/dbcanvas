// explain.js — the EXPLAIN reader card: a query plan as a tree, and what in it is worth a look.
//
// Reads, as they are pasted (client borders and headers included):
//   - MySQL EXPLAIN FORMAT=JSON — the classic query_block form and explain_json_format_version=2
//   - MySQL EXPLAIN FORMAT=TREE and EXPLAIN ANALYZE (`-> ...` lines)
//   - MySQL tabular EXPLAIN, `|`-bordered or the `\G` vertical form
//   - PostgreSQL EXPLAIN / EXPLAIN (ANALYZE, BUFFERS) text, as psql prints it, and FORMAT JSON
//
// Every format becomes the same node shape, so the findings are computed once:
//   { id, depth, type, relation, alias, index, label, rowsEst, rowsAct, loops, cost, timeMs,
//     conds: [[name, text]], notes: [text], details: [text], neverExecuted, children, ... }
// Row counts follow each engine's own convention: estimated and actual rows are per loop.

const BIG_SCAN = 1000 // rows read by a full scan before it is reported
const MANY_LOOPS = 1000 // executions of a nested loop's inner side before it is reported
const OFF_BY = 10 // estimate vs actual ratio before it is reported
const MIN_ROWS = 100 // the larger of estimate and actual must reach this for a ratio to matter

const num = (v) => (v === undefined || v === null || v === '' || Number.isNaN(Number(v)) ? null : Number(v))
export const fmtN = (v) => (v === null || v === undefined ? '—' : Number(v).toLocaleString('en-US', { maximumFractionDigits: 2 }))

function mk(type, extra = {}) {
  return { type, relation: null, alias: null, index: null, label: null, rowsEst: null, rowsAct: null, loops: null,
    cost: null, timeMs: null, conds: [], notes: [], details: [], neverExecuted: false, children: [], ...extra }
}

// parseExplain reads any supported plan. Returns { format, nodes, summary: [[k, v]], findings }
// (nodes flattened in tree order, each with id and depth) or { error }.
export function parseExplain(text) {
  const raw = String(text ?? '').replace(/\r\n/g, '\n')
  if (!raw.trim()) return { error: 'Paste an EXPLAIN output' }
  let out
  try {
    if (/^\s*\|\s*id\s*\|\s*select_type\s*\|/im.test(raw)) out = mysqlTable(raw)
    else if (/^\s*select_type:\s/m.test(raw) && /^\s*(type|key|rows):\s/m.test(raw)) out = mysqlVertical(raw)
    else {
      const t = unframe(raw)
      const trimmed = t.trim()
      if (/^[[{]/.test(trimmed)) out = fromJSON(trimmed)
      else if (/^->/.test(trimmed)) out = mysqlTree(t)
      else if (/\(cost=[\d.]+\.\.[\d.]+ rows=\d+ width=\d+\)|\(actual (time|rows)=|^\s*->\s{2}/m.test(t) || /^(Seq|Index|Bitmap|Hash|Nested|Merge|Sort|Aggregate|HashAggregate|GroupAggregate|Limit|Gather|Append|Result|Update|Insert|Delete|Unique|WindowAgg|CTE|Subquery|Materialize|Parallel)\b/.test(trimmed)) out = pgText(t)
      else return { error: 'Not a plan this card reads: MySQL EXPLAIN (table, \\G, FORMAT=JSON, FORMAT=TREE, ANALYZE) or PostgreSQL EXPLAIN (text or FORMAT JSON)' }
    }
  } catch (e) {
    return { error: e.message }
  }
  if (out.error) return out
  const nodes = flatten(out.roots)
  if (!nodes.length) return { error: 'No plan nodes found' }
  return { format: out.format, nodes, summary: out.summary || [], findings: findings(nodes) }
}

// unframe strips what clients print around a plan: psql's QUERY PLAN header, row count and `+`
// continuations; the mysql client's +---+ borders, | cells |, `*** 1. row ***` and `EXPLAIN:`.
function unframe(text) {
  const keep = []
  const lines = text.split('\n')
  const plus = lines.filter((l) => /\+\s*$/.test(l) && !/^\+[-+]+\+\s*$/.test(l)).length >= 2
  for (let line of lines) {
    if (plus) line = line.replace(/\s*\+\s*$/, '')
    if (/^\s*QUERY PLAN\s*$/.test(line) || /^\s*-{3,}\s*$/.test(line) || /^\s*\(\d+ rows?\)\s*$/.test(line)) continue
    if (/^\s*-\[ RECORD \d+ \]/.test(line) || /^\+[-+]+\+\s*$/.test(line) || /^\|\s*EXPLAIN\s*\|\s*$/.test(line)) continue
    if (/^\*+ \d+\. row \*+\s*$/.test(line.trim()) || /^\s*\d+ rows? in set/.test(line) || /^\s*(mysql|postgres|psql)[>=#-]/.test(line)) continue
    line = line.replace(/^\s*QUERY PLAN\s*\|\s?/, '').replace(/^\s*EXPLAIN:\s?/, '')
    line = line.replace(/^\|\s?/, '').replace(/\s*\|\s*$/, '')
    keep.push(line)
  }
  return keep.join('\n')
}

function flatten(roots) {
  const out = []
  let id = 0
  const walk = (n, depth, parent) => {
    n.id = ++id
    n.depth = depth
    n.parent = parent ? parent.id : null
    out.push(n)
    for (const c of n.children) walk(c, depth + 1, n)
  }
  for (const r of roots) walk(r, 0, null)
  return out
}

// ------------------------------------------------------------------ JSON

function fromJSON(t) {
  let v
  try {
    v = JSON.parse(t)
  } catch {
    const end = Math.max(t.lastIndexOf('}'), t.lastIndexOf(']'))
    try { v = JSON.parse(t.slice(0, end + 1)) } catch (e) { return { error: `Not valid JSON: ${e.message}` } }
  }
  const first = Array.isArray(v) ? v[0] : v
  if (first && first.Plan) return pgJSON(Array.isArray(v) ? v : [v])
  if (first && first.query_block) return { format: 'MySQL FORMAT=JSON', roots: [myBlock(first.query_block)], summary: first.query_block.cost_info ? [['Query cost', first.query_block.cost_info.query_cost]] : [] }
  if (first && (first.operation || first.inputs)) return myV2(first)
  return { error: 'JSON, but not a MySQL or PostgreSQL plan' }
}

// PostgreSQL FORMAT JSON: [{ "Plan": {...}, "Planning Time": n, "Execution Time": n }]
function pgJSON(arr) {
  const COND = ['Filter', 'Index Cond', 'Hash Cond', 'Merge Cond', 'Join Filter', 'Recheck Cond', 'One-Time Filter', 'TID Cond']
  const node = (p) => {
    const n = mk(p['Node Type'] + (p['Join Type'] && p['Join Type'] !== 'Inner' && /Join|Loop/.test(p['Node Type']) ? ` ${p['Join Type']}` : ''), {
      relation: p['Relation Name'] || p['CTE Name'] || p['Function Name'] || null,
      alias: p.Alias && p.Alias !== p['Relation Name'] ? p.Alias : null,
      index: p['Index Name'] || null,
      label: p['Subplan Name'] || null,
      rowsEst: num(p['Plan Rows']), rowsAct: num(p['Actual Rows']), loops: num(p['Actual Loops']),
      cost: p['Total Cost'] !== undefined ? `${p['Startup Cost']}..${p['Total Cost']}` : null,
      timeMs: num(p['Actual Total Time']),
      engine: 'pg',
    })
    if (p['Node Type'] === 'Aggregate') n.type = { Hashed: 'HashAggregate', Sorted: 'GroupAggregate', Mixed: 'MixedAggregate' }[p.Strategy] || 'Aggregate'
    for (const k of COND) if (p[k]) n.conds.push([k, p[k]])
    n.removed = num(p['Rows Removed by Filter']) ?? num(p['Rows Removed by Join Filter'])
    if (p['Rows Removed by Filter'] !== undefined) n.details.push(`Rows Removed by Filter: ${p['Rows Removed by Filter']}`)
    if (p['Rows Removed by Join Filter'] !== undefined) n.details.push(`Rows Removed by Join Filter: ${p['Rows Removed by Join Filter']}`)
    if (p['Sort Key']) n.details.push(`Sort Key: ${[].concat(p['Sort Key']).join(', ')}`)
    if (p['Sort Method']) {
      n.details.push(`Sort Method: ${p['Sort Method']}  ${p['Sort Space Type'] || ''}: ${p['Sort Space Used'] ?? ''}kB`)
      if (p['Sort Space Type'] === 'Disk' || /external/i.test(p['Sort Method'])) n.diskSort = `${p['Sort Method']}, ${fmtN(p['Sort Space Used'])} kB on disk`
    }
    if (p['Hash Batches'] !== undefined) {
      n.details.push(`Buckets: ${p['Hash Buckets']}  Batches: ${p['Hash Batches']}  Memory Usage: ${p['Peak Memory Usage']}kB`)
      n.batches = num(p['Hash Batches'])
    }
    const buf = ['Shared Hit Blocks', 'Shared Read Blocks', 'Temp Read Blocks', 'Temp Written Blocks'].filter((k) => p[k])
    if (buf.length) n.details.push(`Buffers: ${buf.map((k) => `${k.replace(' Blocks', '').toLowerCase()}=${p[k]}`).join(' ')}`)
    if (p['Heap Fetches'] !== undefined) n.details.push(`Heap Fetches: ${p['Heap Fetches']}`)
    if (p['Workers Planned'] !== undefined) n.details.push(`Workers Planned: ${p['Workers Planned']}${p['Workers Launched'] !== undefined ? `, Launched: ${p['Workers Launched']}` : ''}`)
    if (p['Actual Loops'] === 0) n.neverExecuted = true
    n.fullScan = /Seq Scan$/.test(p['Node Type'])
    n.children = (p.Plans || []).map(node)
    return n
  }
  const roots = arr.map((e) => node(e.Plan))
  const summary = []
  const e = arr[0]
  if (e['Planning Time'] !== undefined) summary.push(['Planning time', `${e['Planning Time']} ms`])
  if (e['Execution Time'] !== undefined) summary.push(['Execution time', `${e['Execution Time']} ms`])
  if (roots[0]?.cost) summary.push(['Total cost', roots[0].cost])
  return { format: e['Execution Time'] !== undefined ? 'PostgreSQL EXPLAIN ANALYZE (JSON)' : 'PostgreSQL EXPLAIN (JSON)', roots, summary }
}

// MySQL classic FORMAT=JSON: query_block → table / nested_loop / ordering_operation / ...
const WRAP = { ordering_operation: 'Order', grouping_operation: 'Group', duplicates_removal: 'Distinct', windowing: 'Window', buffer_result: 'Buffer result' }

function myBlock(qb, label) {
  const n = mk('Query block', { label: label || (qb.select_id !== undefined ? `select #${qb.select_id}` : null), cost: qb.cost_info?.query_cost ?? null, engine: 'mysql' })
  if (qb.message) n.notes.push(qb.message)
  myWalk(qb, n)
  return n
}

function myWalk(obj, parent) {
  for (const [k, v] of Object.entries(obj || {})) {
    if (!v || typeof v !== 'object') continue
    if (k === 'table') parent.children.push(myTable(v))
    else if (k === 'nested_loop' && Array.isArray(v)) {
      const n = mk('Nested loop', { engine: 'mysql' })
      for (const x of v) myWalk(x, n)
      parent.children.push(n)
    } else if (WRAP[k]) {
      const n = mk(WRAP[k], { engine: 'mysql' })
      if (v.using_filesort) { n.filesort = true; n.notes.push('Using filesort') }
      if (v.using_temporary_table) { n.temporary = true; n.notes.push('Using temporary') }
      myWalk(v, n)
      parent.children.push(n)
    } else if (/^(union|except|intersect)_result$/.test(k)) {
      const n = mk(k.split('_')[0].replace(/^./, (c) => c.toUpperCase()), { relation: v.table_name || null, engine: 'mysql' })
      if (v.using_temporary_table) { n.temporary = true; n.notes.push('Using temporary') }
      for (const q of v.query_specifications || []) n.children.push(myBlock(q.query_block || q))
      parent.children.push(n)
    } else if (k === 'query_block') parent.children.push(myBlock(v))
    else if (/subquer(y|ies)$/.test(k) && Array.isArray(v)) {
      for (const s of v) if (s.query_block) parent.children.push(myBlock(s.query_block, `${k.replace(/_/g, ' ')}${s.query_block.select_id !== undefined ? ` #${s.query_block.select_id}` : ''}`))
    } else if (k === 'materialized_from_subquery') {
      const n = mk('Materialized subquery', { engine: 'mysql' })
      myWalk(v, n)
      parent.children.push(n)
    }
  }
}

function myTable(t) {
  const n = mk(accessLabel(t.access_type), {
    relation: t.table_name || null, index: t.key || null,
    rowsEst: num(t.rows_examined_per_scan), cost: t.cost_info?.prefix_cost ?? t.cost_info?.read_cost ?? null, engine: 'mysql',
  })
  n.access = t.access_type
  n.possibleKeys = t.possible_keys ? [].concat(t.possible_keys).join(',') : null
  n.filtered = num(t.filtered)
  n.fullScan = t.access_type === 'ALL'
  if (t.attached_condition) n.conds.push(['Condition', t.attached_condition])
  if (t.index_condition) n.conds.push(['Index condition', t.index_condition])
  if (t.ref) n.conds.push(['ref', [].concat(t.ref).join(', ')])
  if (t.used_key_parts) n.details.push(`key parts: ${[].concat(t.used_key_parts).join(', ')}`)
  if (n.possibleKeys) n.details.push(`possible_keys: ${n.possibleKeys}`)
  if (t.rows_produced_per_join !== undefined) n.details.push(`rows produced per join: ${fmtN(t.rows_produced_per_join)}, filtered ${t.filtered}%`)
  if (t.using_index) n.notes.push('Using index')
  if (t.using_join_buffer) { n.joinBuffer = true; n.notes.push(`Using join buffer (${t.using_join_buffer})`) }
  if (t.using_filesort) { n.filesort = true; n.notes.push('Using filesort') }
  if (t.using_temporary_table) { n.temporary = true; n.notes.push('Using temporary') }
  if (t.message) n.notes.push(t.message)
  myWalk({ materialized_from_subquery: t.materialized_from_subquery, attached_subqueries: t.attached_subqueries }, n)
  return n
}

const ACCESS = { ALL: 'Full table scan', index: 'Full index scan', range: 'Index range scan', ref: 'Index lookup', eq_ref: 'Unique index lookup', const: 'Constant row', system: 'System table', ref_or_null: 'Index lookup or NULL', index_merge: 'Index merge', unique_subquery: 'Unique subquery', index_subquery: 'Index subquery', fulltext: 'Full-text' }
const accessLabel = (a) => (a ? `${ACCESS[a] || a} (${a})` : 'Table')

// MySQL explain_json_format_version=2: { operation, access_type, inputs: [...] } (8.3+).
function myV2(root) {
  const node = (o) => {
    const n = mk('', {
      rowsEst: num(o.estimated_rows), rowsAct: num(o.actual_rows), loops: num(o.actual_loops),
      cost: o.estimated_total_cost !== undefined ? fmtN(o.estimated_total_cost) : null,
      timeMs: num(o.actual_last_row_ms), engine: 'mysql',
    })
    readMyOp(n, String(o.operation || o.access_type || 'Operation'))
    if (o.table_name) n.relation = o.alias && o.alias !== o.table_name ? `${o.table_name} ${o.alias}` : o.table_name
    if (o.index_name) n.index = o.index_name
    n.access = o.access_type
    n.fullScan = o.access_type === 'table' && !/temporary/i.test(o.table_name || '')
    if (/^Sort\b/i.test(n.type) || o.access_type === 'sort') n.filesort = true
    if (/temporary/i.test(n.type) || o.access_type === 'materialize' || o.access_type === 'temp_table_aggregate') n.temporary = /temporary/i.test(n.type)
    if (o.condition && !n.conds.some(([, v]) => v === o.condition)) n.conds.push(['Condition', o.condition])
    if (o.join_algorithm === 'hash') n.notes.push('hash join')
    if (o.actual_loops === 0) n.neverExecuted = true
    for (const [k, v] of Object.entries(o)) {
      if (Array.isArray(v)) for (const c of v) if (c && typeof c === 'object' && (c.operation || c.inputs)) n.children.push(node(c))
      if (k === 'subquery' && v && typeof v === 'object' && v.operation) n.children.push(node(v))
    }
    return n
  }
  const r = node(root)
  const summary = []
  if (root.query) summary.push(['Query', root.query])
  if (r.cost) summary.push(['Total cost', r.cost])
  return { format: root.actual_rows !== undefined ? 'MySQL EXPLAIN ANALYZE FORMAT=JSON (v2)' : 'MySQL FORMAT=JSON (v2)', roots: [r], summary }
}

// ------------------------------------------------------------------ MySQL tree and ANALYZE

function mysqlTree(text) {
  const roots = []
  const stack = []
  let last = null
  let analyze = false
  for (const line of text.split('\n')) {
    const m = /^(\s*)->\s+(.*)$/.exec(line)
    if (!m) {
      if (line.trim() && last) last.details.push(line.trim())
      continue
    }
    const indent = m[1].length
    let body = m[2]
    const n = mk('', { engine: 'mysql' })
    const ac = /\(actual time=([\d.]+)\.\.([\d.]+) rows=([\d.e+]+) loops=(\d+)\)/.exec(body)
    if (ac) { analyze = true; n.timeMs = num(ac[2]); n.rowsAct = num(ac[3]); n.loops = num(ac[4]); body = body.replace(ac[0], '') }
    if (/\(never executed\)/.test(body)) { analyze = true; n.neverExecuted = true; n.loops = 0; body = body.replace('(never executed)', '') }
    const co = /\(cost=([\d.e+]+)(?:\.\.([\d.e+]+))? rows=([\d.e+]+)\)/.exec(body)
    if (co) { n.cost = co[2] ? `${co[1]}..${co[2]}` : co[1]; n.rowsEst = num(co[3]); body = body.replace(co[0], '') }
    body = body.trim()
    readMyOp(n, body)
    while (stack.length && stack[stack.length - 1].indent >= indent) stack.pop()
    if (stack.length) stack[stack.length - 1].node.children.push(n)
    else roots.push(n)
    stack.push({ indent, node: n })
    last = n
  }
  return { format: analyze ? 'MySQL EXPLAIN ANALYZE' : 'MySQL FORMAT=TREE', roots, summary: roots[0]?.cost ? [['Total cost', roots[0].cost]] : [] }
}

function readMyOp(n, body) {
  let m
  if ((m = /^(.*?(?:scan|lookup|search|Constant row from|Rows fetched before execution|skip scan)[^]*?) on (\S+?)(?: using (\S+))?(?:\s+over\s+(.*)|\s+(\(.*\)))?(?:,\s*(.*))?$/i.exec(body))) {
    n.type = m[1]
    n.relation = m[2]
    n.index = m[3] || null
    if (m[4] || m[5]) n.conds.push(['Condition', m[4] || m[5]])
    if (m[6]) n.notes.push(m[6])
    n.fullScan = /^Table scan$/i.test(m[1]) && !/^<.*>$/.test(m[2])
  } else if ((m = /^([^:()]+?):\s+(.*)$/.exec(body))) {
    n.type = m[1]
    n.conds.push([/^Filter$/i.test(m[1]) ? 'Condition' : m[1], m[2]])
  } else if ((m = /^(.*?join.*?)\s+(\(.*\))$/i.exec(body))) {
    n.type = m[1]
    n.conds.push(['Condition', m[2]])
  } else n.type = body
  if (/^Sort\b/i.test(n.type)) n.filesort = true
  if (/temporary/i.test(body)) n.temporary = true
}

// ------------------------------------------------------------------ MySQL tabular

const COLS = ['id', 'select_type', 'table', 'partitions', 'type', 'possible_keys', 'key', 'key_len', 'ref', 'rows', 'filtered', 'Extra']

function mysqlTable(text) {
  const lines = text.split('\n').map((l) => l.trim()).filter((l) => l.startsWith('|'))
  const cells = (l) => l.replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim())
  const head = cells(lines[0])
  const rows = lines.slice(1).map((l) => Object.fromEntries(cells(l).map((v, i) => [head[i], v])))
  return myRows(rows, 'MySQL EXPLAIN (table)')
}

function mysqlVertical(text) {
  const rows = []
  let cur = null
  for (const line of text.split('\n')) {
    if (/^\*+ \d+\. row \*+/.test(line.trim())) { cur = {}; rows.push(cur); continue }
    const m = /^\s*(\w+):\s?(.*)$/.exec(line)
    if (!m) continue
    if (!cur || (m[1] === 'id' && cur.id !== undefined)) { cur = {}; rows.push(cur) }
    cur[m[1]] = m[2].trim()
  }
  return myRows(rows, 'MySQL EXPLAIN (\\G)')
}

function myRows(rows, format) {
  const v = (x) => (x === undefined || x === 'NULL' || x === '' ? null : x)
  const roots = []
  let block = null
  for (const r of rows) {
    if (!COLS.some((c) => r[c] !== undefined)) continue
    if (!block || block.qid !== r.id) {
      block = mk('Query block', { label: `id ${v(r.id) ?? '—'} · ${r.select_type || ''}`.trim(), engine: 'mysql' })
      block.qid = r.id
      roots.push(block)
    }
    const n = mk(accessLabel(v(r.type)), { relation: v(r.table), index: v(r.key), rowsEst: num(v(r.rows)), engine: 'mysql' })
    n.access = v(r.type)
    n.possibleKeys = v(r.possible_keys)
    n.filtered = num(v(r.filtered))
    n.fullScan = n.access === 'ALL'
    if (r.select_type && r.select_type !== block.label.split(' · ')[1]) n.label = r.select_type
    if (v(r.ref)) n.conds.push(['ref', r.ref])
    if (n.possibleKeys) n.details.push(`possible_keys: ${n.possibleKeys}`)
    if (v(r.key_len)) n.details.push(`key_len: ${r.key_len}`)
    if (v(r.partitions)) n.details.push(`partitions: ${r.partitions}`)
    if (n.filtered !== null) n.details.push(`filtered: ${r.filtered}%`)
    for (const e of String(v(r.Extra) || '').split(/;\s*/).filter(Boolean)) {
      n.notes.push(e)
      if (/^Using filesort/i.test(e)) n.filesort = true
      if (/^Using temporary/i.test(e)) n.temporary = true
      if (/join buffer/i.test(e)) n.joinBuffer = true
    }
    if (!n.access && !n.relation) { block.notes.push(...n.notes); continue }
    block.children.push(n)
  }
  if (!roots.length) return { error: 'No EXPLAIN rows found' }
  return { format, roots, summary: [] }
}

// ------------------------------------------------------------------ PostgreSQL text

const PG_COND = /^(Filter|Index Cond|Hash Cond|Merge Cond|Join Filter|Recheck Cond|One-Time Filter|TID Cond|Cache Key):\s*(.*)$/

function pgText(text) {
  const lines = text.split('\n').filter((l) => l.trim())
  const roots = []
  const stack = []
  const summary = []
  let last = null
  let rootIndent = null
  let pendingLabel = null
  let inSummary = false
  let analyze = false
  for (const line of lines) {
    const indent = line.length - line.trimStart().length
    const body = line.trim()
    const arrow = /^->\s+/.exec(body)
    const isNode = arrow || rootIndent === null || (/\((cost=|actual |never executed)/.test(body) && !/^[\w ]+:\s/.test(body))
    if (rootIndent !== null && !arrow && indent <= rootIndent && !/^(InitPlan|SubPlan|CTE)\b/.test(body)) inSummary = true
    if (inSummary) {
      // Planning Time / Execution Time at the root's indent; "Planning:" and "JIT:" carry
      // indented lines of their own, which join the entry above them.
      const m = /^([^:]+):\s*(.*)$/.exec(body)
      if (indent > rootIndent && summary.length) summary[summary.length - 1][1] += (summary[summary.length - 1][1] ? '; ' : '') + body
      else if (m) summary.push([m[1], m[2]])
      else summary.push([body, ''])
      continue
    }
    if (/^(InitPlan|SubPlan|CTE)\b/.test(body) && !/\(cost=/.test(body)) { pendingLabel = body; continue }
    if (!isNode) {
      if (!last) continue
      const c = PG_COND.exec(body)
      if (c) last.conds.push([c[1], c[2]])
      else last.details.push(body)
      readPgDetail(last, body)
      continue
    }
    let b = arrow ? body.slice(arrow[0].length) : body
    const n = mk('', { engine: 'pg', label: pendingLabel })
    pendingLabel = null
    const co = /\(cost=([\d.]+)\.\.([\d.]+) rows=(\d+) width=\d+\)/.exec(b)
    if (co) { n.cost = `${co[1]}..${co[2]}`; n.rowsEst = num(co[3]); b = b.replace(co[0], '') }
    const ac = /\(actual time=([\d.]+)\.\.([\d.]+) rows=([\d.]+) loops=(\d+)\)/.exec(b) || /\(actual rows=([\d.]+) loops=(\d+)\)/.exec(b)
    if (ac) {
      analyze = true
      if (ac.length === 5) { n.timeMs = num(ac[2]); n.rowsAct = num(ac[3]); n.loops = num(ac[4]) } else { n.rowsAct = num(ac[1]); n.loops = num(ac[2]) }
      b = b.replace(ac[0], '')
    }
    if (/\(never executed\)/.test(b)) { analyze = true; n.neverExecuted = true; n.loops = 0; b = b.replace('(never executed)', '') }
    readPgName(n, b.trim())
    const col = arrow ? indent : -1
    if (rootIndent === null) rootIndent = indent
    while (stack.length && stack[stack.length - 1].col >= col) stack.pop()
    if (stack.length) stack[stack.length - 1].node.children.push(n)
    else roots.push(n)
    stack.push({ col, node: n })
    last = n
  }
  if (roots[0]?.cost) summary.push(['Total cost', roots[0].cost])
  return { format: analyze ? 'PostgreSQL EXPLAIN ANALYZE' : 'PostgreSQL EXPLAIN', roots, summary }
}

function readPgName(n, name) {
  let m
  if ((m = /^(.*?) using ("[^"]+"|\S+) on ("[^"]+"|\S+)(?: ("[^"]+"|\S+))?$/.exec(name))) {
    n.type = m[1]; n.index = m[2]; n.relation = m[3]; n.alias = m[4] || null
  } else if ((m = /^(.*?) on ("[^"]+"|\S+)(?: ("[^"]+"|\S+))?$/.exec(name))) {
    n.type = m[1]
    if (/Bitmap Index Scan/.test(m[1])) n.index = m[2]
    else { n.relation = m[2]; n.alias = m[3] || null }
  } else n.type = name
  n.fullScan = /Seq Scan$/.test(n.type)
}

function readPgDetail(n, body) {
  let m
  if ((m = /Rows Removed by (?:Join )?Filter:\s*(\d+)/.exec(body))) n.removed = (n.removed || 0) + Number(m[1])
  if (!n.diskSort && (m = /Sort Method:\s*(external \w+)\s+Disk:\s*(\d+)kB/.exec(body))) n.diskSort = `${m[1]}, ${fmtN(m[2])} kB on disk`
  if ((m = /Batches:\s*(\d+)/.exec(body)) && /Buckets:/.test(body)) n.batches = Math.max(n.batches || 0, Number(m[1]))
}

// ------------------------------------------------------------------ findings

const name = (n) => [n.type, n.relation && `on ${n.relation}`, n.index && `(${n.index})`].filter(Boolean).join(' ')

// findings lists what in a plan is worth a look: [{ tone, node, text }], most serious first.
export function findings(nodes) {
  const out = []
  const add = (tone, n, text) => out.push({ tone, node: n.id, at: name(n), text })
  for (const n of nodes) {
    const ran = !n.neverExecuted && n.loops !== 0
    const read = Math.max(n.rowsEst ?? 0, (n.rowsAct ?? 0) + (n.removed ?? 0))
    if (n.fullScan && read >= BIG_SCAN) add('warning', n, `Full scan of ${n.relation || 'a table'}: about ${fmtN(read)} rows read${n.loops > 1 ? ` per loop, ${fmtN(n.loops)} loops` : ''}.`)
    else if (n.fullScan && n.engine === 'mysql' && n.access === 'ALL') add('muted', n, `Full table scan of ${n.relation || 'a table'} (${fmtN(n.rowsEst)} rows estimated).`)
    if (n.possibleKeys && !n.index) add('warning', n, `possible_keys lists ${n.possibleKeys}, but no index is used (key is NULL).`)
    if (n.filesort) add(n.engine === 'mysql' && n.notes.includes('Using filesort') ? 'warning' : 'muted', n, n.notes.includes('Using filesort') ? 'Using filesort: rows are sorted after they are read, not read in index order.' : 'Sort step: rows are sorted after they are read (filesort).')
    if (n.temporary) add('warning', n, 'Using temporary: an internal temporary table is built.')
    if (n.joinBuffer) add('warning', n, `Joined through a join buffer (${n.notes.find((x) => /join buffer/i.test(x)) || 'no index on the join column'}).`)
    if (ran && n.rowsEst !== null && n.rowsAct !== null) {
      const hi = Math.max(n.rowsEst, n.rowsAct)
      const lo = Math.max(Math.min(n.rowsEst, n.rowsAct), 1)
      if (hi >= MIN_ROWS && hi / lo >= OFF_BY) add('warning', n, `Estimated ${fmtN(n.rowsEst)} rows, actual ${fmtN(n.rowsAct)} (${n.rowsAct > n.rowsEst ? 'under' : 'over'}estimated ${fmtN(Math.round(hi / lo))}×).`)
    }
    if (n.removed && n.removed >= BIG_SCAN && n.removed >= OFF_BY * Math.max(n.rowsAct ?? 0, 1)) add('warning', n, `Filter removes ${fmtN(n.removed)} rows per loop and keeps ${fmtN(n.rowsAct ?? 0)}.`)
    if (n.filtered !== null && n.filtered !== undefined && n.filtered < 10 && (n.rowsEst ?? 0) >= BIG_SCAN) add('muted', n, `filtered ${n.filtered}%: most of the ${fmtN(n.rowsEst)} rows read are expected to be discarded by the condition.`)
    if (n.diskSort) add('warning', n, `Sort spilled to disk: ${n.diskSort}.`)
    if (n.batches > 1) add('warning', n, `Hash ran in ${n.batches} batches: the hash table did not fit in work_mem.`)
    if (/nested loop/i.test(n.type)) {
      for (const c of n.children.slice(1)) {
        if ((c.loops ?? 0) >= MANY_LOOPS) add('warning', c, `Runs ${fmtN(c.loops)} times as the inner side of ${n.type} (node ${n.id}).`)
      }
    }
  }
  const rank = { danger: 0, warning: 1, muted: 2 }
  out.sort((a, b) => rank[a.tone] - rank[b.tone] || a.node - b.node)
  return out
}
