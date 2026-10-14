import { useEffect, useState } from 'react'
import { Badge, ConfirmButton } from './ui.jsx'
import { Icon } from './Icons.jsx'

// RestorePanel is the other half of a pgBackRest Backup tab: what is in the repository, and
// getting it back. Three ways — everything archived, up to a moment (PITR), or one backup set as
// it was taken — for the standalone node (app/pgrestore.go) and a Patroni cluster
// (app/patronirestore.go), which differ in what a restore does, not in what is asked. Each
// overwrites data, so each is a two-click ConfirmButton. `api` is pgApi or patroniApi; `cluster`
// says so in the confirmation, since a cluster restore rebuilds every member.
export default function RestorePanel({ api, busy, setBusy, setMsg, cluster = false }) {
  const [info, setInfo] = useState(null)
  const [err, setErr] = useState('')
  const [when, setWhen] = useState('')
  const load = () => api.backups().then((r) => { setInfo(r); setErr('') }).catch((e) => setErr(e.message))
  useEffect(() => {
    load()
    const on = () => load()
    window.addEventListener('dbcanvas:pg-backups', on)
    return () => window.removeEventListener('dbcanvas:pg-backups', on)
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  async function restore(body, what) {
    setBusy(true)
    setMsg(null)
    try {
      const r = await api.restore(body)
      setMsg({ tone: 'success', text: cluster
        ? `Restored to ${what}. ${r.leader} leads; the replicas were rebuilt, and a full backup was taken on the new timeline.`
        : `Restored to ${what}${r.timeline ? `, now on timeline ${r.timeline}` : ''}. Take a full backup now, so the next point-in-time restore has a clean starting point on this timeline.` })
      load()
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || 'Restore failed.' })
    } finally {
      setBusy(false)
    }
  }
  // datetime-local is the browser's local time with no zone; the server wants RFC 3339.
  const whenISO = when ? new Date(when).toISOString() : ''
  const fmt = (iso) => new Date(iso).toLocaleString()
  const backups = info?.backups || []

  return (
    <div className="space-y-2 rounded-lg border p-2.5">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-muted">Restore</span>
        {cluster && busy && <span className="text-[11px] text-muted">stopping, restoring, rebuilding — minutes…</span>}
        <button className="text-xs text-muted hover:text-fg" onClick={load} title="Reload the backup list"><Icon.Refresh size={13} /></button>
      </div>
      {cluster && (
        <p className="text-[11px] leading-snug text-muted">
          A cluster restore stops every member, restores the leader, starts it as the leader of a new
          timeline, takes a full backup there and rebuilds each replica from it. The cluster is down
          while it runs.
        </p>
      )}
      {err && <p className="text-xs text-danger">{err}</p>}
      {info && backups.length === 0 && <p className="text-xs text-muted">No backups in the repository yet.</p>}
      {backups.length > 0 && (
        <>
          <div className="max-h-48 space-y-1 overflow-auto">
            {[...backups].reverse().map((b) => (
              <div key={b.label} className="flex items-center gap-2 rounded bg-surface2 px-2 py-1 text-xs">
                <Badge tone={b.type === 'full' ? 'primary' : 'muted'}>{b.type}</Badge>
                <span className="min-w-0 flex-1 truncate" title={b.label}>{fmt(b.stop)}</span>
                <ConfirmButton size="sm" variant="subtle" disabled={busy} confirmLabel="Overwrite?"
                  onConfirm={() => restore({ mode: 'backup', set: b.label }, `backup ${b.label}`)}>Restore</ConfirmButton>
              </div>
            ))}
          </div>
          <ConfirmButton size="sm" className="w-full" variant="subtle" disabled={busy} confirmLabel={cluster ? 'Restore every member?' : "Overwrite this node's data?"}
            onConfirm={() => restore({ mode: 'latest' }, 'the latest point')}>
            Restore to latest (replay all archived WAL)
          </ConfirmButton>
          <div className="space-y-1">
            <label className="text-xs text-muted">Point in time — reachable from {fmt(info.earliest)}</label>
            <div className="flex gap-2">
              <input type="datetime-local" step="1" className="min-w-0 flex-1 rounded border bg-bg px-2 py-1 text-xs"
                value={when} onChange={(e) => setWhen(e.target.value)} />
              <ConfirmButton size="sm" disabled={busy || !when} confirmLabel="Overwrite?"
                onConfirm={() => restore({ mode: 'time', time: whenISO }, fmt(whenISO))}>Restore</ConfirmButton>
            </div>
          </div>
        </>
      )}
    </div>
  )
}
