// ReplCard.jsx — the Replication status card: paste SHOW REPLICA STATUS\G, rs.status(), or
// pg_stat_replication / pg_stat_wal_receiver, and read a verdict per channel, member or standby.
//
// The reading is lib/scratch/replstatus.js; this only renders it. Nothing is stored or sent.

import { useMemo } from 'react'
import { Input, Output, KV } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { Icon } from '../Icons.jsx'
import { readReplication } from '../../lib/scratch/replstatus.js'

const TONE = { ok: 'success', warning: 'warning', error: 'danger', info: 'muted' }
const STATUS = { ok: 'OK', warning: 'check', error: 'problem' }
const ENGINE = { mysql: 'MySQL', mongodb: 'MongoDB', postgresql: 'PostgreSQL' }

// ReplView renders a readReplication() result.
export function ReplView({ res, spawn }) {
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  if (!res.engine) return null
  return (
    <div className="space-y-1.5 text-[11px]">
      <div className="flex flex-wrap items-center gap-1.5">
        <Badge tone="primary">{ENGINE[res.engine]}</Badge>
        <span className="text-fg">{res.summary}</span>
      </div>
      {res.findings.length > 0 && (
        <ul className="space-y-0.5">
          {res.findings.map((f, i) => (
            <li key={i} className="flex items-start gap-1.5">
              <span className="shrink-0"><Badge tone={TONE[f.severity]}>{f.severity}</Badge></span>
              <span className={`min-w-0 break-words ${f.severity === 'info' ? 'text-muted' : 'text-fg'}`}>{f.text}</span>
            </li>
          ))}
        </ul>
      )}
      {res.items.map((it) => (
        <div key={it.id} className="space-y-0.5 rounded border bg-surface2 p-1.5">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="font-medium text-fg">{it.title}</span>
            {it.subtitle && <span className="text-muted">{it.subtitle}</span>}
            <span className="ml-auto"><Badge tone={TONE[it.status]}>{STATUS[it.status]}</Badge></span>
          </div>
          {it.facts.map(([k, v]) => <KV key={k} k={k} v={v} />)}
          {it.gtid && (
            <div className="space-y-0.5 pt-0.5">
              <KV k="Retrieved" v={it.gtid.retrieved || '(empty)'} />
              <KV k="Executed" v={it.gtid.executed || '(empty)'} />
              {it.gtid.pending && <KV k="Not yet executed" v={it.gtid.pending} />}
              {spawn && (
                <button className="flex items-center gap-1 rounded border px-1.5 py-0.5 text-muted hover:text-fg"
                  title="Open Retrieved (A) and Executed (B) in the GTID sets card"
                  onClick={() => spawn('gtid', it.gtid.retrieved, { b: it.gtid.executed })}>
                  <Icon.PopOut size={12} /> Retrieved vs Executed in the GTID card
                </button>
              )}
            </div>
          )}
        </div>
      ))}
    </div>
  )
}

export function ReplCard({ state, update, spawn, onSelectText }) {
  const res = useMemo(() => readReplication(state.input), [state.input])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={4}
        placeholder={'SHOW REPLICA STATUS\\G (or SHOW SLAVE STATUS), rs.status(), or SELECT * FROM pg_stat_replication / pg_stat_wal_receiver'} />
      {String(state.input || '').trim() && <ReplView res={res} spawn={spawn} />}
    </div>
  )
}
