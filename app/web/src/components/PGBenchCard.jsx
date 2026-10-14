import { useState } from 'react'
import { Button } from './ui.jsx'
import { Icon } from './Icons.jsx'

// PGBenchCard runs pgbench against a node's (or a Patroni leader's) pgbench data and shows what it
// measured — app/pgbenchrun.go. The data is loaded at deploy from the form's pgbench scale; this is
// the "now push on it" half. `run` is pgApi/patroniApi .pgbench.
export default function PGBenchCard({ run, where }) {
  const [clients, setClients] = useState(8)
  const [seconds, setSeconds] = useState(30)
  const [readOnly, setReadOnly] = useState(false)
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState(null)
  const [err, setErr] = useState('')
  async function go() {
    setBusy(true)
    setErr('')
    setRes(null)
    try {
      setRes(await run({ clients, seconds, readOnly }))
    } catch (e) {
      setErr(e.message || 'pgbench failed.')
    } finally {
      setBusy(false)
    }
  }
  const num = 'w-16 rounded border bg-bg px-2 py-1 text-xs tabular-nums'
  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        pgbench against the <span className="font-mono">pgbench</span> database {where}: read/write is pgbench&apos;s
        TPC-B-like default, read-only is <span className="font-mono">-S</span> (selects only). It runs on the server
        over the local socket, so it measures the database rather than the network.
      </div>
      <div className="flex flex-wrap items-center gap-3 text-xs">
        <label className="flex items-center gap-1.5">Clients
          <input type="number" min="1" max="64" className={num} value={clients} disabled={busy}
            onChange={(e) => setClients(Math.max(1, Math.min(64, Number(e.target.value) || 1)))} />
        </label>
        <label className="flex items-center gap-1.5">Seconds
          <input type="number" min="5" max="300" className={num} value={seconds} disabled={busy}
            onChange={(e) => setSeconds(Math.max(5, Math.min(300, Number(e.target.value) || 5)))} />
        </label>
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={readOnly} disabled={busy} onChange={(e) => setReadOnly(e.target.checked)} /> Read-only
        </label>
      </div>
      <Button size="sm" className="w-full" disabled={busy} onClick={go}>
        <Icon.Play size={14} /> {busy ? `Running for ${seconds}s…` : 'Run pgbench'}
      </Button>
      {err && <p className="text-xs text-danger">{err}</p>}
      {res && (
        <div className="grid grid-cols-3 gap-2 rounded-lg border p-2 text-center">
          <div><div className="text-lg font-semibold tabular-nums">{res.tps.toFixed(0)}</div><div className="text-[11px] text-muted">TPS</div></div>
          <div><div className="text-lg font-semibold tabular-nums">{res.latencyMs.toFixed(2)}</div><div className="text-[11px] text-muted">ms avg latency</div></div>
          <div><div className={`text-lg font-semibold tabular-nums ${res.failed ? 'text-danger' : ''}`}>{res.failed}</div><div className="text-[11px] text-muted">failed of {res.transactions.toLocaleString()}</div></div>
        </div>
      )}
    </div>
  )
}
