// RedactCard.jsx — the Redactor card (lib/scratch/redact.js): paste a log, a Secret or a command
// line, pick what to mask, and copy the redacted text. The mapping from original to replacement
// is masked until "Show originals" is clicked, so a shared session does not see it by default.

import { useMemo, useState } from 'react'
import { small, Input, Output } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { Icon } from '../Icons.jsx'
import { redact, CATEGORIES, CATEGORY_LABEL, DEFAULT_OPTIONS } from '../../lib/scratch/redact.js'

const clip = (s, n = 80) => (s.length > n ? `${s.slice(0, n)}…` : s)

// RedactView renders a redact() result: counts, the text, and the mapping table.
export function RedactView({ res, spawn, showOriginals = false, onToggle }) {
  if (!res) return null
  const cats = Object.entries(res.counts)
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-1 text-[11px]">
        {cats.length === 0
          ? <span className="text-muted">Nothing to mask was found.</span>
          : cats.map(([cat, n]) => <Badge key={cat} tone="primary">{CATEGORY_LABEL[cat] || cat}: {n}</Badge>)}
      </div>
      <Output text={res.text} spawn={spawn} />
      {res.mapping.length > 0 && (
        <div className="space-y-0.5">
          <div className="flex items-center gap-1 text-[11px] text-muted">
            <span>Mapping — {res.mapping.length} value(s)</span>
            {onToggle && (
              <button className="ml-auto flex items-center gap-1 hover:text-fg" onClick={onToggle}>
                {showOriginals ? <Icon.EyeOff size={12} /> : <Icon.Eye size={12} />}{showOriginals ? 'Hide originals' : 'Show originals'}
              </button>
            )}
          </div>
          <div className="max-h-48 overflow-auto rounded border bg-bg text-[11px]">
            {res.mapping.map((m) => (
              <div key={`${m.cat}:${m.original}`} className="grid grid-cols-[6.5rem_1fr_1fr_2rem] gap-1 border-b px-1 py-0.5 last:border-b-0">
                <span className="text-muted">{CATEGORY_LABEL[m.cat] || m.cat}</span>
                <span className="min-w-0 break-all font-mono text-fg" data-secret="">
                  {showOriginals ? clip(m.original) : '•'.repeat(Math.min(12, m.original.length || 1))}
                </span>
                <span className="min-w-0 break-all font-mono text-fg">{m.replacement}</span>
                <span className="text-right text-muted">×{m.count}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

export function RedactCard({ state, update, spawn, onSelectText }) {
  const [showOriginals, setShowOriginals] = useState(false)
  const opts = { ...DEFAULT_OPTIONS, ...(state.opts || {}) }
  const setOpt = (patch) => update({ opts: { ...opts, ...patch } })
  const input = state.input || ''
  const res = useMemo(() => (input.trim() ? redact(input, opts) : null), [input, JSON.stringify(opts)]) // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <div className="space-y-1">
      <Input value={input} onChange={(v) => update({ input: v })} onSelectText={onSelectText} rows={5}
        placeholder="Paste a log, a Kubernetes Secret, a config or a command line — addresses, hosts, users and secrets are masked" />
      <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-[11px] text-muted">
        {CATEGORIES.map((c) => (
          <label key={c.id} className="flex items-center gap-1">
            <input type="checkbox" checked={!!opts[c.id]} onChange={(e) => setOpt({ [c.id]: e.target.checked })} /> {c.label}
          </label>
        ))}
        <label className="flex items-center gap-1" title="127.0.0.1, ::1, root, postgres, admin and other names every installation has">
          <input type="checkbox" checked={!!opts.keepWellKnown} onChange={(e) => setOpt({ keepWellKnown: e.target.checked })} /> keep loopback and built-in accounts
        </label>
      </div>
      <div className="flex gap-1">
        <input className={`${small} min-w-0 flex-1`} placeholder="Extra words to mask, comma-separated (company, project, cluster names)"
          value={opts.words || ''} onChange={(e) => setOpt({ words: e.target.value })} />
        <select className={small} value={opts.style} onChange={(e) => setOpt({ style: e.target.value })} title="How secrets are replaced">
          <option value="numbered">{'<secret-1>'}</option>
          <option value="stars">********</option>
        </select>
      </div>
      {res
        ? <RedactView res={res} spawn={spawn} showOriginals={showOriginals} onToggle={() => setShowOriginals(!showOriginals)} />
        : <div className="text-[11px] text-muted">Redacted in this browser; nothing is sent anywhere. Check the result before sharing it.</div>}
    </div>
  )
}
