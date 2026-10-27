// InnodbCard.jsx — the InnoDB status reader card: SHOW ENGINE INNODB STATUS read section by
// section, with the latest deadlock laid out per transaction and the numbers worth a look listed
// first. The reading is in lib/scratch/innodb.js; InnodbView renders an already-parsed status, so
// it renders under the smoke test without the board.

import { useMemo } from 'react'
import { Input, Output, KV, small } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { parseInnodb, fmtN, duration } from '../../lib/scratch/innodb.js'
import { parseSize } from '../../lib/scratch/units.js'

const TONE_TEXT = { danger: 'text-danger', warning: 'text-warning', muted: 'text-muted' }
const pct = (a, b) => (a !== undefined && b ? ` (${((100 * a) / b).toFixed(1)}%)` : '')
const mib = (bytes) => (bytes === undefined || bytes === null ? '—' : `${fmtN(bytes)} bytes (${(bytes / 2 ** 20).toFixed(1)} MiB)`)

function Section({ title, children }) {
  return (
    <div className="rounded border bg-bg p-1">
      <div className="mb-0.5 font-medium text-fg">{title}</div>
      {children}
    </div>
  )
}

function Lock({ lk, label }) {
  return (
    <div className="pl-2">
      <span className="text-muted">{label}: </span>
      <span className="font-mono text-fg">{lk.table}</span>
      {lk.index && <span className="font-mono text-success"> · {lk.index}</span>}
      <span className="font-mono text-warning"> · {lk.mode}</span>
      {lk.kind === 'record' && <span className="text-muted"> · space {lk.space} page {lk.page}</span>}
    </div>
  )
}

function DeadlockTrx({ t, rolledBack }) {
  return (
    <div className={`rounded border p-1 ${rolledBack ? 'border-danger/40' : ''}`}>
      <div className="flex flex-wrap items-center gap-1">
        <span className="font-medium text-fg">({t.n}) TRANSACTION {t.trx || '—'}</span>
        {t.thread && <span className="text-muted">thread {t.thread}</span>}
        {t.active !== undefined && <span className="text-muted">· ACTIVE {duration(t.active)}</span>}
        {t.state && <span className="text-muted">· {t.state}</span>}
        {rolledBack && <Badge tone="danger">rolled back</Badge>}
      </div>
      {t.client && <div className="text-muted">client: <span className="font-mono">{t.client}</span>{t.rowLocks !== undefined ? ` · ${t.lockStructs} lock struct(s), ${t.rowLocks} row lock(s)` : ''}</div>}
      {t.query && <pre className="my-0.5 whitespace-pre-wrap break-all rounded bg-surface2 p-1 font-mono text-[11px] text-fg">{t.query}</pre>}
      {t.holds.map((lk, i) => <Lock key={`h${i}`} lk={lk} label="holds" />)}
      {t.holds.length === 0 && <div className="pl-2 text-muted">holds: not printed (MySQL 5.7 prints the held locks for the second transaction only)</div>}
      {t.waits.map((lk, i) => <Lock key={`w${i}`} lk={lk} label="waits for" />)}
    </div>
  )
}

// InnodbView renders parseInnodb's result.
export function InnodbView({ res, maxTrx = 25 }) {
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  const { header: h, deadlock: d, semaphores: s, transactions: x, bufferPool: b, log: g, rows: r } = res
  const active = (x?.list || []).slice(0, maxTrx)
  return (
    <div className="space-y-1.5 text-[11px]">
      <div className="text-muted">
        {h.time ? <>Status at <span className="font-mono text-fg">{h.time}</span></> : 'Status'}
        {h.seconds !== null && <> · per second averages calculated from the last <span className="text-fg">{h.seconds}</span> seconds</>}
      </div>
      <Section title="Findings">
        {res.findings.length === 0 && <div className="text-muted">Nothing in the list of checks applies to this status.</div>}
        {res.findings.map((f, i) => (
          <div key={i} className="flex gap-1.5">
            <span className={`shrink-0 ${TONE_TEXT[f.tone]}`}>●</span>
            <span><span className="text-muted">{f.section} — </span><span className={f.tone === 'muted' ? 'text-muted' : 'text-fg'}>{f.text}</span></span>
          </div>
        ))}
      </Section>
      {d && (
        <Section title={`Latest detected deadlock${d.time ? ` · ${d.time}` : ''}`}>
          {d.note && <div className="text-warning">{d.note}</div>}
          <div className="space-y-1">
            {d.trxs.map((t) => <DeadlockTrx key={t.n} t={t} rolledBack={d.rolledBack === t.n} />)}
          </div>
          {d.rolledBack !== null && <div className="mt-0.5 text-fg">InnoDB rolled back transaction ({d.rolledBack}){d.trxs.find((t) => t.n === d.rolledBack)?.trx ? `, TRANSACTION ${d.trxs.find((t) => t.n === d.rolledBack).trx}` : ''}.</div>}
        </Section>
      )}
      {res.fkError && <Section title="Latest foreign key error"><pre className="whitespace-pre-wrap break-all font-mono text-muted">{res.fkError}</pre></Section>}
      {s && (
        <Section title="Semaphores">
          <KV k="OS waits" v={`reservation count ${fmtN(s.reservations)}, signal count ${fmtN(s.signals)}`} />
          {s.spins.map((l, i) => <div key={i} className="font-mono text-muted">{l}</div>)}
          {s.waits.length === 0 ? <div className="text-muted">No thread is waiting on a semaphore.</div> : s.waits.map((w, i) => (
            <div key={i} className="mt-0.5">
              <span className={w.seconds >= 240 ? 'text-danger' : w.seconds >= 10 ? 'text-warning' : 'text-fg'}>Thread {w.thread} has waited {fmtN(w.seconds)} s</span>
              <span className="text-muted"> at {w.at}</span>
              {w.what.map((l, j) => <div key={j} className="pl-2 font-mono text-muted">{l}</div>)}
            </div>
          ))}
        </Section>
      )}
      {x && (
        <Section title="Transactions">
          <KV k="History list" v={fmtN(x.hll)} />
          <KV k="Trx id counter" v={x.counter} />
          {x.purge && <KV k="Purge" v={x.purge.replace(/^Purge done for /, '')} />}
          <div className="mt-0.5 text-muted">{x.list.length} active, {x.notStarted} not started{x.list.length > maxTrx ? ` — the ${maxTrx} oldest shown` : ''}</div>
          {active.length > 0 && (
            <div className="overflow-x-auto">
              <table className="w-full text-left">
                <thead className="text-muted"><tr><th className="pr-2 font-normal">trx</th><th className="pr-2 font-normal">thread</th><th className="pr-2 font-normal">active</th><th className="pr-2 font-normal">locks</th><th className="pr-2 font-normal">undo</th><th className="font-normal">state / query</th></tr></thead>
                <tbody>
                  {active.map((t) => (
                    <tr key={t.trx} className="align-top">
                      <td className="pr-2 font-mono">{t.trx}</td>
                      <td className="pr-2 font-mono">{t.thread || '—'}</td>
                      <td className={`pr-2 ${t.active >= 3600 ? 'text-danger' : t.active >= 600 ? 'text-warning' : ''}`}>{duration(t.active)}</td>
                      <td className="pr-2">{t.lockStructs !== undefined ? `${fmtN(t.lockStructs)} / ${fmtN(t.rowLocks)} rows` : '—'}</td>
                      <td className="pr-2">{t.undo !== null && t.undo !== undefined ? fmtN(t.undo) : '—'}</td>
                      <td className="break-all">
                        <span className="text-muted">{[t.prepared && 'PREPARED', t.state, t.client].filter(Boolean).join(' · ')}</span>
                        {t.query && <div className="font-mono text-fg">{t.query}</div>}
                        {t.waitingSec !== null && <div className="text-warning">waiting {t.waitingSec} s{t.waitingFor ? ` for ${t.waitingFor.mode} on ${t.waitingFor.table}${t.waitingFor.index ? ` (${t.waitingFor.index})` : ''}` : ''}</div>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Section>
      )}
      {b && (
        <Section title="Buffer pool and memory">
          <KV k="Hit rate" v={b.hit !== undefined ? `${b.hit} / ${b.hitOf}${pct(b.hit, b.hitOf)}${b.hitFrom ? `, ${b.hitFrom}` : ''}` : b.noGets ? 'no page gets since the last printout' : null} />
          <KV k="Size (pages)" v={fmtN(b.size)} />
          <KV k="Free" v={b.free !== undefined ? `${fmtN(b.free)}${pct(b.free, b.size)}` : null} />
          <KV k="Database" v={b.database !== undefined ? `${fmtN(b.database)}${pct(b.database, b.size)}` : null} />
          <KV k="Modified" v={b.modified !== undefined ? `${fmtN(b.modified)}${pct(b.modified, b.database)} of database pages` : null} />
          {b.read !== undefined && <KV k="Pages" v={`read ${fmtN(b.read)}, created ${fmtN(b.created)}, written ${fmtN(b.written)}`} />}
          {b.rates && <KV k="Rates" v={b.rates} />}
        </Section>
      )}
      {g && (
        <Section title="Log">
          <KV k="LSN" v={fmtN(g.lsn)} />
          <KV k="Flushed up to" v={fmtN(g.flushed)} />
          <KV k="Pages flushed" v={fmtN(g.pagesFlushed)} />
          <KV k="Last checkpoint" v={fmtN(g.checkpoint)} />
          <KV k="Checkpoint age" v={g.age !== undefined ? `${mib(g.age)}${g.pct !== undefined ? ` — ${g.pct.toFixed(1)}% of ${g.pctOf}` : ''}` : null} />
          {g.maxAge && <KV k="Max age" v={mib(g.maxAge)} />}
        </Section>
      )}
      {r && (
        <Section title="Row operations">
          <KV k="Inside InnoDB" v={r.inside !== undefined ? `${r.inside} queries, ${r.queue} in queue, ${r.readViews ?? '—'} read views` : null} />
          {r.mainState && <KV k="Main thread" v={r.mainState} />}
          {r.totals && <KV k="Rows (total)" v={`inserted ${fmtN(r.totals.inserted)}, updated ${fmtN(r.totals.updated)}, deleted ${fmtN(r.totals.deleted)}, read ${fmtN(r.totals.read)}`} />}
          {r.rates && <KV k="Rows /s" v={`${r.rates.inserts} inserts, ${r.rates.updates} updates, ${r.rates.deletes} deletes, ${r.rates.reads} reads`} />}
        </Section>
      )}
    </div>
  )
}

export function InnodbCard({ state, update, onSelectText }) {
  const redo = String(state.redo || '').trim() ? parseSize(state.redo) : null
  const res = useMemo(() => (String(state.input || '').trim() ? parseInnodb(state.input, { redoBytes: redo?.bytes }) : null), [state.input, redo?.bytes])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={4} maxRows={12}
        placeholder="Paste SHOW ENGINE INNODB STATUS\G output (MySQL 5.7 or 8.0; the one-line \n-escaped form also reads)" />
      <div className="flex items-center gap-1 text-[11px] text-muted">
        <span>Redo capacity (optional, for checkpoint age %):</span>
        <input className={`${small} w-24 font-mono`} placeholder="e.g. 2G" value={state.redo || ''} onChange={(e) => update({ redo: e.target.value })} />
        {redo?.error && <span className="text-danger">{redo.error}</span>}
      </div>
      <InnodbView res={res} />
    </div>
  )
}
