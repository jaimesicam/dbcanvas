// DigestCard.jsx — the Query digest card: pt-query-digest over a pasted slow log or list of
// queries, computed in the browser by lib/scratch/ptdigest.js.
//
// The table is the "# Profile" with every class (the ones pt-query-digest folds into MISC are
// dimmed); a row opens its fingerprint, example and statistics. The text views are
// pt-query-digest's own output, to copy. Nothing is stored or sent.

import { useMemo } from 'react'
import { copy, Input, Output } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { Icon } from '../Icons.jsx'
import { digest, formatProfile, formatReport, formatTime, formatCount, formatFixed } from '../../lib/scratch/ptdigest.js'

const SORTS = {
  rank: { label: '#', key: (c) => c.rank, asc: true },
  time: { label: 'Response time', key: (c) => c.time },
  calls: { label: 'Calls', key: (c) => c.count },
  rcall: { label: 'R/Call', key: (c) => c.rCall },
  vm: { label: 'V/M', key: (c) => c.vm },
}

const STAT_ROWS = [
  ['Query_time', 'Exec time', formatTime],
  ['Lock_time', 'Lock time', formatTime],
  ['Rows_sent', 'Rows sent', formatCount],
  ['Rows_examined', 'Rows examined', formatCount],
]

function CopyBtn({ text, title }) {
  return (
    <button className="shrink-0 rounded p-0.5 text-muted hover:text-fg" title={title || 'Copy'} onClick={() => copy(text)}>
      <Icon.Copy size={11} />
    </button>
  )
}

function Values({ list }) {
  if (!list?.length) return <span className="text-muted">—</span>
  return list.map((v) => <span key={v.value} className="mr-2 font-mono">{v.value}{list.length > 1 ? <span className="text-muted"> ×{v.count}</span> : null}</span>)
}

function Detail({ c, kind, spawn }) {
  return (
    <div className="space-y-1 rounded border bg-surface2 p-1.5 text-[11px]">
      <div className="flex items-center gap-1">
        <span className="w-20 shrink-0 text-muted">Query ID</span>
        <span className="min-w-0 break-all font-mono text-fg">{c.id}</span>
        <CopyBtn text={c.id} title="Copy the Query ID" />
      </div>
      <div className="flex items-start gap-1">
        <span className="w-20 shrink-0 text-muted">Fingerprint</span>
        <span className="min-w-0 flex-1 whitespace-pre-wrap break-all font-mono text-fg">{c.fingerprint}</span>
        <CopyBtn text={c.fingerprint} title="Copy the fingerprint" />
      </div>
      <div className="flex gap-1">
        <span className="w-20 shrink-0 text-muted">Item</span>
        <span className="min-w-0 break-all font-mono text-fg">{c.item || '—'}</span>
      </div>
      {kind === 'slowlog' && (
        <table className="w-full font-mono">
          <thead>
            <tr className="text-muted">
              {['', 'total', 'min', 'max', 'avg', '95%', 'stddev', 'median'].map((h) => <th key={h} className="px-1 text-right font-normal first:text-left">{h}</th>)}
            </tr>
          </thead>
          <tbody>
            <tr><td className="px-1 text-muted">Count</td><td className="px-1 text-right">{c.count}</td><td colSpan={6} /></tr>
            {STAT_ROWS.map(([a, label, f]) => {
              const s = c.stats[a]
              if (!s) return null
              return (
                <tr key={a}>
                  <td className="px-1 text-muted">{label}</td>
                  {[s.sum, s.min, s.max, s.avg, s.pct_95, s.stddev, s.median].map((v, i) => <td key={i} className="px-1 text-right text-fg">{f(v ?? 0)}</td>)}
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
      {kind === 'slowlog' && (
        <>
          <div className="flex gap-1"><span className="w-20 shrink-0 text-muted">Databases</span><span className="min-w-0 break-all"><Values list={c.dbs} /></span></div>
          <div className="flex gap-1"><span className="w-20 shrink-0 text-muted">Users</span><span className="min-w-0 break-all"><Values list={c.users} /></span></div>
          <div className="flex gap-1"><span className="w-20 shrink-0 text-muted">Hosts</span><span className="min-w-0 break-all"><Values list={c.hosts} /></span></div>
        </>
      )}
      <div className="text-muted">{kind === 'slowlog' ? 'Example (the slowest call)' : 'Example (the last one)'}</div>
      <Output text={c.sample} spawn={spawn} />
    </div>
  )
}

// DigestView renders a digest() result. `sort`/`desc` order the table, `selected` is the checksum
// of the open row, `mode` is 'table', 'profile' or 'report'.
export function DigestView({ res, sort = 'rank', desc, selected, mode = 'table', onSort, onSelect, onMode, spawn }) {
  const rows = useMemo(() => {
    if (!res?.classes) return []
    const s = SORTS[sort] || SORTS.rank
    const dir = (desc ?? !s.asc) ? -1 : 1
    return [...res.classes].sort((a, b) => (s.key(a) - s.key(b)) * dir || a.rank - b.rank)
  }, [res, sort, desc])
  const text = useMemo(() => (res?.classes && mode === 'profile' ? formatProfile(res) : res?.classes && mode === 'report' ? formatReport(res) : ''), [res, mode])
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  if (!res.classes.length) return <div className="text-[11px] text-muted">No queries found. Paste a MySQL slow log, or queries each ending with ; at the end of a line.</div>
  const slow = res.kind === 'slowlog'
  const total = res.ea.globals.Query_time?.sum || 0
  const sel = rows.find((c) => c.checksum === selected)
  const th = (k, cls = 'text-right') => (
    <th className={`cursor-pointer select-none px-1 font-normal hover:text-fg ${cls} ${sort === k ? 'text-fg' : ''}`}
      onClick={() => onSort?.(k, sort === k ? !(desc ?? !SORTS[k].asc) : !SORTS[k].asc)}>
      {SORTS[k].label}{sort === k ? ((desc ?? !SORTS[k].asc) ? ' ↓' : ' ↑') : ''}
    </th>
  )
  return (
    <div className="space-y-1 text-[11px]">
      <div className="flex flex-wrap items-center gap-1.5">
        <Badge tone={slow ? 'primary' : 'muted'}>{slow ? 'slow log' : 'query list'}</Badge>
        <span className="text-muted">
          {res.events} {slow ? 'events' : 'queries'} · {res.classes.length} fingerprint{res.classes.length === 1 ? '' : 's'}
          {slow ? ` · ${formatTime(total)} total` : ''}
          {res.other.length ? ` · ${res.other.length} in MISC` : ''}
        </span>
        <span className="ml-auto flex gap-0.5">
          {[['table', 'Table'], ['profile', 'Profile text'], ['report', 'Full report']].map(([m, label]) => (
            <button key={m} onClick={() => onMode?.(m)}
              className={`rounded px-1.5 py-0.5 ${mode === m ? 'bg-surface2 text-fg' : 'text-muted hover:text-fg'}`}>{label}</button>
          ))}
        </span>
      </div>
      {mode !== 'table' ? <Output text={text} /> : (
        <>
          <div className="max-h-[40vh] overflow-auto rounded border">
            <table className="w-full font-mono">
              <thead className="sticky top-0 bg-surface2 text-muted">
                <tr>
                  {th('rank')}
                  <th className="px-1 text-left font-normal">Query ID</th>
                  {slow && th('time')}
                  {th('calls')}
                  {slow && th('rcall')}
                  {slow && th('vm')}
                  <th className="px-1 text-left font-normal">Item</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((c) => (
                  <tr key={c.checksum} onClick={() => onSelect?.(c.checksum === selected ? '' : c.checksum)}
                    className={`cursor-pointer border-t hover:bg-surface2 ${c.checksum === selected ? 'bg-surface2' : ''} ${c.reported ? 'text-fg' : 'text-muted'}`}
                    title={c.reported ? c.fingerprint : 'Folded into MISC by pt-query-digest (--limit 95%:20)'}>
                    <td className="px-1 text-right">{c.rank}</td>
                    <td className="px-1" title={c.id}>{c.id.slice(0, 12)}…</td>
                    {slow && <td className="whitespace-nowrap px-1 text-right">{formatFixed(c.time, 4)} <span className="text-muted">{formatFixed(c.timePct, 1)}%</span></td>}
                    <td className="px-1 text-right">{c.count}</td>
                    {slow && <td className="px-1 text-right">{formatFixed(c.rCall, 4)}</td>}
                    {slow && <td className={`px-1 text-right ${c.vm >= 1 ? 'text-warning' : ''}`}>{formatFixed(c.vm, 2)}</td>}
                    <td className="max-w-[12rem] truncate px-1">{c.item}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {sel ? <Detail c={sel} kind={res.kind} spawn={spawn} /> : <div className="text-muted">Click a row for its fingerprint, example and statistics.</div>}
        </>
      )}
    </div>
  )
}

export function DigestCard({ state, update, spawn, onSelectText }) {
  const input = state.input || ''
  const res = useMemo(() => {
    if (!input.trim()) return null
    try { return digest(input) } catch (e) { return { error: e.message } }
  }, [input])
  return (
    <div className="space-y-1">
      <Input value={input} rows={4} maxRows={12} onChange={(v) => update({ input: v })} onSelectText={onSelectText}
        placeholder="Paste a MySQL slow log, or queries each ending with ; — fingerprints, Query IDs and the profile as pt-query-digest computes them" />
      {!input.trim() && <div className="text-[11px] text-muted">Computed in this browser; nothing is sent anywhere.</div>}
      <DigestView res={res} sort={state.sort} desc={state.desc} selected={state.sel} mode={state.mode}
        onSort={(sort, desc) => update({ sort, desc })} onSelect={(sel) => update({ sel })} onMode={(mode) => update({ mode })} spawn={spawn} />
    </div>
  )
}
