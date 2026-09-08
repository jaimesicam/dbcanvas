import { useCallback, useEffect, useState } from 'react'
import { Button, inputCls } from './ui.jsx'
import { mongoNodeApi } from '../lib/stackApi.js'

// MongoCertReissue is the re-issue control shown at the top of a MongoDB node's TLS
// tab. It re-signs the node's per-node cert (/etc/mongo/certs/server.pem) from the
// Intranet CA and overwrites it in place — mongod is left running; the operator
// applies it via the TLS config shown below (a restart is an all-members step).
export default function MongoCertReissue({ stackId, nodeId }) {
  const api = mongoNodeApi(stackId, nodeId)
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
    <div className="space-y-2 rounded-lg border border-dashed p-2">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">{err}</div>}
      <div className="text-xs font-medium text-muted">Current certificate</div>
      <pre className="whitespace-pre-wrap break-all rounded-lg border bg-bg p-2 text-xs text-fg">{info || '—'}</pre>
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
      <div className="text-[11px] text-muted">Requires a running Intranet node. The new cert is written in place — apply it with the config below (restart mongod / roll the members yourself).</div>
    </div>
  )
}
