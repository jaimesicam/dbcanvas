// ExplainCard.jsx — the EXPLAIN reader card: a MySQL or PostgreSQL plan as an indented tree, with
// what in it is worth a look listed first. The reading is in lib/scratch/explain.js; ExplainView
// renders an already-parsed plan, so it renders under the smoke test without the board.

import { useMemo } from 'react'
import { Input, Output, KV } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { parseExplain, fmtN } from '../../lib/scratch/explain.js'

const TONE_TEXT = { danger: 'text-danger', warning: 'text-warning', muted: 'text-muted' }

function Metrics({ n }) {
  const parts = []
  if (n.rowsEst !== null) parts.push(`est ${fmtN(n.rowsEst)}`)
  if (n.neverExecuted) parts.push('never executed')
  else if (n.rowsAct !== null) parts.push(`actual ${fmtN(n.rowsAct)}${n.loops !== null ? ` × ${fmtN(n.loops)} loop${n.loops === 1 ? '' : 's'}` : ''}`)
  if (n.cost !== null) parts.push(`cost ${n.cost}`)
  if (n.timeMs !== null) parts.push(`${fmtN(n.timeMs)} ms`)
  if (!parts.length) return null
  return <span className="text-muted">{parts.join(' · ')}</span>
}

function PlanNode({ n, flagged, focused, onFocus, details }) {
  return (
    <div className={`border-l-2 py-0.5 pr-1 ${focused ? 'bg-primary/10' : ''} ${flagged ? 'border-warning' : 'border-transparent'}`}
      style={{ paddingLeft: 4 + n.depth * 14 }} onClick={onFocus ? () => onFocus(n.id) : undefined}>
      <div className="flex flex-wrap items-baseline gap-x-1.5 text-[11px]">
        <span className="text-muted">#{n.id}</span>
        {n.label && <span className="text-muted">[{n.label}]</span>}
        <span className="font-medium text-fg">{n.type}</span>
        {n.relation && <span className="font-mono text-fg">on {n.relation}{n.alias ? ` ${n.alias}` : ''}</span>}
        {n.index && <span className="font-mono text-success">using {n.index}</span>}
        <Metrics n={n} />
      </div>
      {n.conds.map(([k, v], i) => (
        <div key={i} className="break-all pl-3 font-mono text-[11px] text-muted"><span className="text-fg/70">{k}:</span> {v}</div>
      ))}
      {n.notes.length > 0 && (
        <div className="flex flex-wrap gap-1 pl-3">
          {n.notes.map((x, i) => <Badge key={i} tone={/filesort|temporary|join buffer/i.test(x) ? 'warning' : 'muted'}>{x}</Badge>)}
        </div>
      )}
      {details && n.details.map((d, i) => <div key={i} className="break-all pl-3 font-mono text-[11px] text-muted">{d}</div>)}
    </div>
  )
}

// ExplainView renders parseExplain's result: summary, findings (each naming its node), the tree.
export function ExplainView({ res, focus, onFocus, details = true }) {
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  const flagged = new Set(res.findings.filter((f) => f.tone !== 'muted').map((f) => f.node))
  return (
    <div className="space-y-1.5 text-[11px]">
      <div className="flex flex-wrap items-center gap-1">
        <Badge tone="primary">{res.format}</Badge>
        <span className="text-muted">{res.nodes.length} node{res.nodes.length === 1 ? '' : 's'}</span>
      </div>
      {res.summary.length > 0 && <div>{res.summary.map(([k, v], i) => <KV key={i} k={k} v={v} />)}</div>}
      <div className="rounded border bg-surface2 p-1">
        <div className="mb-0.5 font-medium text-fg">Findings</div>
        {res.findings.length === 0 && <div className="text-muted">Nothing in the list of checks applies to this plan.</div>}
        {res.findings.map((f, i) => (
          <button key={i} type="button" className="flex w-full gap-1.5 text-left hover:bg-surface" onClick={onFocus ? () => onFocus(f.node) : undefined}>
            <span className={`shrink-0 ${TONE_TEXT[f.tone]}`}>#{f.node}</span>
            <span className="min-w-0"><span className="text-muted">{f.at} — </span><span className={f.tone === 'muted' ? 'text-muted' : 'text-fg'}>{f.text}</span></span>
          </button>
        ))}
      </div>
      <div className="max-h-[60vh] overflow-auto rounded border bg-bg py-0.5">
        {res.nodes.map((n) => <PlanNode key={n.id} n={n} flagged={flagged.has(n.id)} focused={focus === n.id} onFocus={onFocus} details={details} />)}
      </div>
    </div>
  )
}

export function ExplainCard({ state, update, onSelectText }) {
  const res = useMemo(() => (String(state.input || '').trim() ? parseExplain(state.input) : null), [state.input])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input, focus: null })} onSelectText={onSelectText} rows={4} maxRows={12}
        placeholder="Paste EXPLAIN output — MySQL (table, \G, FORMAT=JSON, FORMAT=TREE, EXPLAIN ANALYZE) or PostgreSQL (text as psql prints it, or FORMAT JSON)" />
      {res && !res.error && (
        <label className="flex items-center gap-1 text-[11px] text-muted">
          <input type="checkbox" checked={state.details !== false} onChange={(e) => update({ details: e.target.checked })} /> show node details (buffers, sort keys, possible_keys…)
        </label>
      )}
      <ExplainView res={res} focus={state.focus} onFocus={(focus) => update({ focus: state.focus === focus ? null : focus })} details={state.details !== false} />
    </div>
  )
}
