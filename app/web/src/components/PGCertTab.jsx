import { useCallback, useEffect, useState } from 'react'
import { Button, inputCls } from './ui.jsx'
import { pgApi } from '../lib/stackApi.js'

// PGCertTab is the properties-panel Certificate tab shared by every PostgreSQL-family
// node console (standalone PostgreSQL, Patroni, repmgr, Spock). It shows the node's
// current Intranet-CA server cert and re-issues it on demand. The re-issue only
// overwrites the cert files in place — PostgreSQL is left running; the operator
// reloads/restarts it (SELECT pg_reload_conf() or a service restart) to apply.
export default function PGCertTab({ stackId, nodeId }) {
  const api = pgApi(stackId, nodeId)
  const [info, setInfo] = useState('')
  const [value, setValue] = useState(365)
  const [unit, setUnit] = useState('days')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try { setInfo((await api.certInfo()).info || '') } catch (e) { setErr(e.message) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stackId, nodeId])
  useEffect(() => { load() }, [load])

  async function generate() {
    setBusy(true); setErr('')
    try { await api.certGenerate(Number(value), unit); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  return (
    <div className="space-y-3">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">{err}</div>}
      <div>
        <div className="mb-1 text-xs font-medium text-muted">Current certificate</div>
        <pre className="whitespace-pre-wrap break-all rounded-lg border bg-bg p-2 text-xs text-fg">{info || '—'}</pre>
        <div className="mt-1 text-xs text-muted">
          PostgreSQL serves <span className="font-mono">server.crt</span> (+ server.key, ca.crt), signed by the Intranet CA.
        </div>
      </div>
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Re-issue from Intranet CA (overwrites the cert files in place)</div>
        <div className="flex gap-1">
          <input type="number" min="1" className={inputCls} value={value} onChange={(e) => setValue(e.target.value)} />
          <select className={inputCls} value={unit} onChange={(e) => setUnit(e.target.value)}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
        <Button size="sm" className="w-full" disabled={busy} onClick={generate}>
          {busy ? 'Generating…' : 'Generate certificate'}
        </Button>
        <div className="text-[11px] text-muted">Requires a running Intranet node. The new cert is written in place — reload or restart this node's PostgreSQL yourself to apply it.</div>
      </div>
    </div>
  )
}
