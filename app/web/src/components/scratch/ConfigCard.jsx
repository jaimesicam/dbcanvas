// ConfigCard.jsx — the Config compare card: two configurations (my.cnf, SHOW VARIABLES,
// postgresql.conf, pg_settings, mongod.conf) compared by meaning.
//
// The comparison is lib/scratch/cnfdiff.js; this only renders it. Nothing is stored or sent.

import { useMemo } from 'react'
import { Input, Output, small } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { Icon } from '../Icons.jsx'
import { compareConfigs, visibleRows, CNF_FORMATS } from '../../lib/scratch/cnfdiff.js'

const KIND = {
  diff: ['different', 'warning'],
  onlyA: ['only in A', 'primary'],
  onlyB: ['only in B', 'primary'],
  written: ['same, written differently', 'success'],
  same: ['same', 'muted'],
}
const SHOW = { diff: 'Differences', written: 'Differences + written differently', all: 'All' }

const cell = (e) => (e ? <span title={e.line ? `line ${e.line}${e.section ? ` [${e.section}]` : ''}: ${e.raw}` : e.raw}>{e.value === '' ? '(empty)' : e.value}</span> : <span className="text-muted">—</span>)

// ConfigView renders a compareConfigs() result, filtered by `show` (see visibleRows).
export function ConfigView({ res, show = 'written' }) {
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  const rows = visibleRows(res, show)
  const scoped = new Set(res.rows.map((r) => r.scope)).size > 1 || res.rows.some((r) => r.scope && r.scope !== 'mysqld')
  const c = res.counts
  return (
    <div className="space-y-1.5 text-[11px]">
      <div className="text-muted">
        A: {CNF_FORMATS[res.a.format]} · B: {CNF_FORMATS[res.b.format]}
        <br />
        {c.diff} different, {c.onlyA} only in A, {c.onlyB} only in B, {c.written} the same but written differently, {c.same} the same
        {c.defaults > 0 && show !== 'all' ? ` — ${c.defaults} that only the server lists (defaults, or set elsewhere) are shown under All` : ''}.
      </div>
      {res.notes.map((n, i) => <div key={i} className="rounded border border-warning/30 bg-warning/10 px-1.5 py-0.5 text-fg">{n}</div>)}
      {rows.length === 0
        ? <div className="text-muted">{res.rows.length ? 'No differences.' : 'No options found.'}</div>
        : (
          <div className="max-h-[50vh] overflow-auto rounded border">
            <table className="w-full border-collapse font-mono text-[11px]">
              <thead className="sticky top-0 bg-surface2 text-left text-muted">
                <tr><th className="px-1 py-0.5 font-normal">option</th><th className="px-1 font-normal">A</th><th className="px-1 font-normal">B</th><th className="px-1 font-normal" /></tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={`${r.scope}/${r.name}`} className="border-t align-top">
                    <td className="break-all px-1 py-0.5 text-fg">
                      {scoped && r.scope && <span className="text-muted">[{r.scope}] </span>}{r.name}
                      {r.note && <div className="font-sans text-muted">{r.note}</div>}
                    </td>
                    <td className={`break-all px-1 py-0.5 ${r.kind === 'diff' ? 'text-warning' : 'text-fg'}`}>{cell(r.a)}</td>
                    <td className={`break-all px-1 py-0.5 ${r.kind === 'diff' ? 'text-warning' : 'text-fg'}`}>{cell(r.b)}</td>
                    <td className="whitespace-nowrap px-1 py-0.5 font-sans">
                      <Badge tone={r.defaultOnly ? 'muted' : KIND[r.kind][1]}>{r.defaultOnly ? 'server only' : KIND[r.kind][0]}</Badge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      {res.dupes.length > 0 && (
        <div className="space-y-0.5">
          <div className="text-muted">Set more than once in one file</div>
          {res.dupes.map((d) => (
            <div key={`${d.side}/${d.scope}/${d.name}`} className="font-mono text-fg">
              {d.side}{d.scope && d.scope !== '' ? ` [${d.scope}]` : ''} {d.name}:{' '}
              {d.entries.map((e) => `line ${e.line}${e.section && d.scope === 'mysqld' && e.section !== 'mysqld' ? ` [${e.section}]` : ''} = ${e.value}`).join(', ')}
              <span className="font-sans text-muted">{d.repeatable ? ' — repeatable: every line counts' : ` — line ${d.wins.line} wins (the last one read)`}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

export function ConfigCard({ state, update, spawn, onSelectText }) {
  const a = state.a || ''
  const b = state.b || ''
  const res = useMemo(() => compareConfigs(a, b, { fmtA: state.fmtA || 'auto', fmtB: state.fmtB || 'auto' }), [a, b, state.fmtA, state.fmtB])
  const fmtSelect = (key) => (
    <select className={`${small} min-w-0 flex-1`} value={state[key] || 'auto'} onChange={(e) => update({ [key]: e.target.value })}>
      <option value="auto">auto-detect{(a || b) ? ` (${CNF_FORMATS[key === 'fmtA' ? res.a.format : res.b.format]})` : ''}</option>
      {Object.entries(CNF_FORMATS).map(([k, label]) => <option key={k} value={k}>{label}</option>)}
    </select>
  )
  return (
    <div className="space-y-1">
      <div className="grid grid-cols-2 gap-1">
        <div className="space-y-0.5">
          <Input value={a} onChange={(v) => update({ a: v })} onSelectText={onSelectText} rows={4} placeholder="A — my.cnf, SHOW VARIABLES, postgresql.conf, pg_settings or mongod.conf" />
          <div className="flex">{fmtSelect('fmtA')}</div>
        </div>
        <div className="space-y-0.5">
          <Input value={b} onChange={(v) => update({ b: v })} onSelectText={onSelectText} rows={4} placeholder="B — the same kinds; a file can be compared with a live server's listing" />
          <div className="flex">{fmtSelect('fmtB')}</div>
        </div>
      </div>
      <div className="flex items-center gap-1">
        <select className={small} value={state.show || 'written'} onChange={(e) => update({ show: e.target.value })}>
          {Object.entries(SHOW).map(([k, label]) => <option key={k} value={k}>{label}</option>)}
        </select>
        {spawn && (a || b) && (
          <button className="ml-auto flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] text-muted hover:text-fg" title="Open both texts, as written, in the Text Diff card"
            onClick={() => spawn('diff', a, { b })}>
            <Icon.PopOut size={12} /> Text diff
          </button>
        )}
      </div>
      {(a.trim() || b.trim()) && <ConfigView res={res} show={state.show || 'written'} />}
    </div>
  )
}
