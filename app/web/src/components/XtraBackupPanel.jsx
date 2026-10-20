import { useEffect, useState } from 'react'
import { Badge, Button, ConfirmButton } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { xbApi } from '../lib/stackApi.js'

const fmtSize = (n) => (!n ? '' : n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB` : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`)

// XtraBackupPanel is a MySQL Backup tab (app/mysqlbackup.go): take a full or incremental
// Percona XtraBackup backup to SeaweedFS, see what is in the bucket, and restore — one backup as
// it was taken, the latest point, or a moment in time when binary logs are shipped. `scope` is
// 'nodes' for a standalone Percona Server node, 'frames' for a replication or PXC cluster, which
// changes what a restore does to the other members and so what the confirmation says.
export default function XtraBackupPanel({ stackId, scope, id, kind }) {
  const api = xbApi(stackId, scope, id)
  const [info, setInfo] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const [msg, setMsg] = useState(null)
  const [when, setWhen] = useState('')
  const load = () => api.backups().then((r) => { setInfo(r); setErr('') }).catch((e) => setErr(e.message))
  useEffect(() => {
    load()
    const on = () => load()
    window.addEventListener('dbcanvas:backups', on)
    return () => window.removeEventListener('dbcanvas:backups', on)
  }, [stackId, scope, id]) // eslint-disable-line react-hooks/exhaustive-deps

  const changed = () => { load(); window.dispatchEvent(new Event('dbcanvas:backups')) }
  async function run(what, fn, ok) {
    setBusy(what)
    setMsg(null)
    try {
      const r = await fn()
      setMsg({ tone: 'success', text: ok(r) })
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || 'Failed.' })
    } finally {
      setBusy('')
      changed()
    }
  }
  const backup = (type) => run(type, () => api.backup(type), (r) => `${type === 'full' ? 'Full' : 'Incremental'} backup ${r.backup.name} taken on ${r.backup.host}.`)
  const restore = (body, what) => run('restore', () => api.restore(body), (r) =>
    `Restored to ${r.restoredTo || what}.` + (r.backup ? ` Full backup ${r.backup} taken as the new base.` : r.backupError ? ` The follow-up full backup failed: ${r.backupError}` : ''))
  const remove = (name) => run('delete', () => api.remove(name), (r) => `Deleted ${r.deleted.join(', ')}.`)

  const backups = info?.backups || []
  const pitr = !!info?.pitr
  const win = info?.window
  const fmt = (iso) => new Date(iso).toLocaleString()
  // datetime-local is the browser's local time with no zone; the server wants RFC 3339.
  const whenISO = when ? new Date(when).toISOString() : ''
  const cluster = scope === 'frames'
  const restoreWhat = !cluster ? "Overwrite this node's data?" : kind === 'pxc' ? 'Restore the whole cluster?' : 'Restore every member?'
  const working = !!busy || info?.running

  return (
    <div className="space-y-3 text-sm">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Percona XtraBackup streams into the SeaweedFS bucket at <span className="font-mono">{info?.repo || 'xtrabackup/…'}</span>.
        An incremental continues the newest backup on the member that took it.
        {pitr ? ' Binary logs are shipped every minute, so a restore can also reach the latest point or any moment since the first backup.' : ' Binary-log shipping is off, so a restore puts back one backup exactly as it was taken.'}
      </div>
      <div className="grid grid-cols-2 gap-2">
        <Button size="sm" disabled={working} onClick={() => backup('full')}>
          <Icon.Arrow size={15} /> {busy === 'full' ? 'Backing up…' : 'Full backup'}
        </Button>
        <Button size="sm" variant="outline" disabled={working || backups.length === 0} onClick={() => backup('incremental')}
          title={backups.length ? 'Only what changed since the newest backup' : 'Take a full backup first'}>
          {busy === 'incremental' ? 'Backing up…' : 'Incremental'}
        </Button>
      </div>
      {msg && (
        <div className={`whitespace-pre-wrap break-words rounded-lg border px-2.5 py-1.5 text-xs ${msg.tone === 'danger' ? 'border-danger/30 bg-danger/15 text-danger' : 'border-success/30 bg-success/15 text-success'}`}>
          {msg.text}
        </div>
      )}

      <div className="space-y-2 rounded-lg border p-2.5">
        <div className="flex items-center justify-between">
          <span className="text-xs font-medium text-muted">Restore</span>
          {busy === 'restore' && <span className="text-[11px] text-muted">{cluster ? 'restoring every member — minutes…' : 'restoring — minutes…'}</span>}
          <button className="text-xs text-muted hover:text-fg" onClick={load} title="Reload the backup list"><Icon.Refresh size={13} /></button>
        </div>
        {cluster && (
          <p className="text-[11px] leading-snug text-muted">
            {kind === 'pxc'
              ? 'Restores the bootstrap member, starts it as a new cluster and lets the other members rejoin by SST. The cluster is down while it runs.'
              : 'Restores every member from the same backup, re-attaches the replicas to the primary and replays binary logs on the primary. Writes stop while it runs.'}
            {' '}A full backup is taken afterwards as the base for what comes next.
          </p>
        )}
        {err && <p className="text-xs text-danger">{err}</p>}
        {info && backups.length === 0 && <p className="text-xs text-muted">No backups in the bucket yet.</p>}
        {backups.length > 0 && (
          <>
            <div className="max-h-56 space-y-1 overflow-auto">
              {[...backups].reverse().map((b) => (
                <div key={b.name} className="flex items-center gap-2 rounded bg-surface2 px-2 py-1 text-xs">
                  <Badge tone={b.type === 'full' ? 'primary' : 'muted'}>{b.type === 'full' ? 'full' : 'incr'}</Badge>
                  <span className="min-w-0 flex-1 truncate" title={`${b.name} · ${b.host}${b.base ? ` · on top of ${b.base}` : ''}${b.binlogFile ? ` · ${b.binlogFile}:${b.binlogPos}` : ''}`}>
                    {fmt(b.end)} <span className="text-muted">· {b.host}{b.size ? ` · ${fmtSize(b.size)}` : ''}</span>
                  </span>
                  <ConfirmButton size="sm" variant="subtle" disabled={working} confirmLabel="Overwrite?"
                    onConfirm={() => restore({ mode: 'backup', name: b.name }, `backup ${b.name}`)}>Restore</ConfirmButton>
                  <ConfirmButton size="sm" variant="subtle" disabled={working} confirmLabel={b.type === 'full' ? 'Delete + its incrementals?' : 'Delete?'}
                    onConfirm={() => remove(b.name)} title="Delete this backup"><Icon.Trash size={13} /></ConfirmButton>
                </div>
              ))}
            </div>
            <ConfirmButton size="sm" className="w-full" variant="subtle" disabled={working} confirmLabel={restoreWhat}
              onConfirm={() => restore({ mode: 'latest' }, 'the latest point')}>
              {pitr ? 'Restore to latest (newest backup + every binary log since)' : 'Restore the newest backup'}
            </ConfirmButton>
            {pitr && (
              <div className="space-y-1">
                <label className="text-xs text-muted">
                  Point in time{win?.from ? ` — reachable from ${fmt(win.from)}${win.to ? ` to ${fmt(win.to)}` : ''}` : ''}
                </label>
                <div className="flex gap-2">
                  <input type="datetime-local" step="1" className="min-w-0 flex-1 rounded border bg-bg px-2 py-1 text-xs"
                    value={when} onChange={(e) => setWhen(e.target.value)} />
                  <ConfirmButton size="sm" disabled={working || !when} confirmLabel="Overwrite?"
                    onConfirm={() => restore({ mode: 'time', time: whenISO }, fmt(whenISO))}>Restore</ConfirmButton>
                </div>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}
