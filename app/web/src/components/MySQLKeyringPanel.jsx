import { useCallback, useEffect, useState } from 'react'
import { Button, ConfirmButton } from './ui.jsx'

// MySQLKeyringPanel — the live half of a keyring-encrypted MySQL-family server's Encryption tab
// (app/mysqlkeyringops.go): what the keyring holds right now, when the master key was last
// rotated, a Rotate button, and — for a keyring file — a copy of the file to keep somewhere other
// than the disk it protects. VaultGuide, above it, says how the keyring was configured.

async function call(method, path) {
  const res = await fetch(path, { method, credentials: 'same-origin' })
  const text = await res.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { /* not JSON */ }
  if (!res.ok) throw new Error((data && data.error) || `Request failed (${res.status})`)
  return data
}

function Row({ k, v }) {
  return (
    <div className="flex justify-between gap-3 text-sm">
      <span className="text-muted">{k}</span>
      <span className="truncate font-mono text-xs text-fg">{v === undefined || v === null || v === '' ? '—' : String(v)}</span>
    </div>
  )
}

const onOff = (v) => (v === '1' || v === 'ON' ? 'ON' : v === '0' || v === 'OFF' ? 'OFF' : v)

// ago is a short "3 days ago" for a timestamp, or '' when there is none.
function ago(iso) {
  if (!iso) return ''
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000)
  if (s < 90) return 'just now'
  if (s < 5400) return `${Math.round(s / 60)} minutes ago`
  if (s < 129600) return `${Math.round(s / 3600)} hours ago`
  return `${Math.round(s / 86400)} days ago`
}

export default function MySQLKeyringPanel({ stackId, nodeId, cluster = false }) {
  const base = `/api/stacks/${stackId}/nodes/${nodeId}/mysql/keyring`
  const [st, setSt] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  const load = useCallback(() => {
    call('GET', base).then((r) => { setSt(r); setErr('') }).catch((e) => setErr(e.message))
  }, [base])
  useEffect(() => { load() }, [load])

  async function rotate() {
    setBusy(true)
    setMsg(null)
    try {
      const r = await call('POST', `${base}/rotate`)
      setMsg({ ok: true, text: (r.log || 'Rotated.').replace(/\n/g, '; ') })
      load()
    } catch (e) {
      setMsg({ ok: false, text: e.message })
    } finally {
      setBusy(false)
    }
  }

  if (err) return <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">Keyring status: {err}</div>
  if (!st) return <div className="text-xs text-muted">Reading the keyring…</div>
  const file = st.provider === 'file'
  const vars = st.variables || {}
  const never = !st.rotatedAt
  return (
    <div className="space-y-2 rounded-lg border p-2" data-keyring-panel="">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-muted">Keyring now</span>
        <div className="flex items-center gap-1">
          {file && (
            <Button size="sm" variant="outline" onClick={() => { window.location.href = `${base}/file` }}>Download keyring file</Button>
          )}
          <ConfirmButton size="sm" variant="outline" disabled={busy} confirmLabel="Rotate now?" onConfirm={rotate}>
            {busy ? 'Rotating…' : 'Rotate master key'}
          </ConfirmButton>
        </div>
      </div>
      {file && <Row k="Keyring file" v={st.keyringFile} />}
      {file && <Row k="Size · owner" v={st.fileBytes !== undefined ? `${st.fileBytes} bytes · ${st.fileMode || ''}` : 'missing'} />}
      {file && <Row k="Last written" v={st.fileModified ? `${st.fileModified} (${ago(st.fileModified)})` : ''} />}
      <Row k="Keys in the keyring" v={st.keys !== undefined ? `${st.keys} (${st.masterKeys ?? 0} InnoDB master)` : 'not reported by this release'} />
      <Row k="Encrypted tablespaces" v={st.tablespaces} />
      <Row k="Master key rotated" v={never ? 'not from here yet' : `${st.rotatedAt} (${ago(st.rotatedAt)})`} />
      {Object.keys(vars).length > 0 && (
        <div className="space-y-1 border-t pt-2">
          {Object.entries(vars).map(([k, v]) => <Row key={k} k={k} v={onOff(v)} />)}
        </div>
      )}
      {msg && <p className={`text-[11px] ${msg.ok ? 'text-success' : 'text-danger'}`}>{msg.text}</p>}
      <p className="text-[11px] leading-snug text-muted">
        Rotating re-wraps this server&apos;s tablespace keys under a new master key; no data is rewritten
        {vars.binlog_encryption === '1' ? ', and the binary log master key is rotated too' : ''}.
        {cluster ? ' Every member has a keyring of its own, so this rotates this member only.' : ''}
        {file ? ' Without the keyring file the encrypted tablespaces cannot be opened: keep a copy apart from the data, and take a new one after each rotation.' : ''}
      </p>
    </div>
  )
}
