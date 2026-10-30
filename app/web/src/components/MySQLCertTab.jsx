import { useCallback, useEffect, useState } from 'react'
import { Button, inputCls } from './ui.jsx'
import { mysqlCertApi } from '../lib/stackApi.js'

// MySQLCertTab is the Certificate tab every MySQL-family node console shares (PXC,
// Percona Server, MySQL, MariaDB, InnoDB Cluster). It shows the server certificate and
// re-issues it from the Intranet CA with the lifetime chosen here, over the files it was
// installed in. mysqld is not restarted: it serves the new certificate once TLS is
// reloaded (ALTER INSTANCE RELOAD TLS, or FLUSH SSL on MariaDB) or the server restarts — which is how a short
// lifetime is tested on a server that keeps running.
export default function MySQLCertTab({ stackId, nodeId, ttlValue, ttlUnit }) {
  const api = mysqlCertApi(stackId, nodeId)
  const [info, setInfo] = useState('')
  const [value, setValue] = useState(ttlValue || 365)
  const [unit, setUnit] = useState(ttlUnit === 'minutes' || ttlUnit === 'hours' ? ttlUnit : 'days')
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
          mysqld serves <span className="font-mono">/var/lib/mysql/server-cert.pem</span> (+ server-key.pem, ca.pem), signed by the Intranet CA.
        </div>
      </div>
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Re-issue from the Intranet CA, valid for</div>
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
        <div className="text-[11px] text-muted">
          Needs a running Intranet node. The files are replaced where they are installed and mysqld keeps running;
          it serves the new certificate after <span className="font-mono">ALTER INSTANCE RELOAD TLS</span> (<span className="font-mono">FLUSH SSL</span> on MariaDB) or a restart.
        </div>
      </div>
    </div>
  )
}
