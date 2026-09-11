import { useCallback, useEffect, useMemo, useState } from 'react'
import { Button, Badge, ConfirmButton, Toggle, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { CopyButton } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { TOOL_HELP } from '../lib/help.js'
import { k3dApi } from '../lib/stackApi.js'
import { usePolling } from '../lib/usePolling.jsx'

// K8sBackupManager — an operator-managed cluster's backups, its restores, and the bucket they
// live in.
//
// The three panes are the three questions, in the order they get asked: what backups exist and
// can one go back on, what the restores did, and what is actually in the object store — which is
// the only one of the three Kubernetes cannot answer, because an S3 bucket is not an API object.
//
// Two things make this a lab tool rather than a control panel, and they are worth keeping:
//
//   1. EVERY ACTION SHOWS ITS MANIFEST. Taking a backup and restoring one both apply a custom
//      resource this panel wrote, and the document is shown after the fact and filed on the node
//      under the operator's own deploy/backup. The point is to be able to stop using the panel:
//      what it did is a file you can read, edit and re-apply.
//   2. THE DESTRUCTIVE OPTIONS ARE SPELT OUT, NOT DEFAULTED. Deleting a backup object and
//      deleting a backup are different acts and get different buttons. A bucket delete offers a
//      dry run first, and prefers it.

// POLL_MS — a backup takes minutes and its state is the whole reason the table is on screen.
const POLL_MS = 5000

// TONE maps an operator's reported state onto the badge palette. The four operators agree on
// these words; anything else shows muted, which is the right look for "the operator said
// something we do not have an opinion about".
const TONE = {
  Succeeded: 'success',
  Ready: 'success',
  Failed: 'danger',
  Error: 'danger',
  Running: 'primary',
  Starting: 'primary',
  Requested: 'primary',
  Waiting: 'warning',
  Pending: 'muted',
  New: 'muted',
}

// sizeLabel matches byteSizeLabel in app/k3dbucket.go, so the size in the table and the size in
// a "too large to stream" error are the same number written the same way.
export const sizeLabel = (n) => {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${n} B`
}

// whenLabel renders a timestamp as something readable at a glance. The absolute time goes in the
// title attribute: "4 minutes ago" is what you want while a backup runs, and the exact second is
// what you want when you are matching it against a log.
export function whenLabel(ts) {
  if (!ts) return '—'
  const t = Date.parse(ts)
  if (Number.isNaN(t)) return ts
  const secs = Math.round((Date.now() - t) / 1000)
  if (secs < 0) return new Date(t).toLocaleString()
  if (secs < 60) return `${secs}s ago`
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`
  return new Date(t).toLocaleDateString()
}

// crumbsOf turns a prefix into the breadcrumb trail above the bucket listing.
export function crumbsOf(prefix) {
  const parts = (prefix || '').split('/').filter(Boolean)
  return parts.map((name, i) => ({ name, key: parts.slice(0, i + 1).join('/') }))
}

function Code({ label, text, tone = '' }) {
  if (!text) return null
  return (
    <div>
      {label && (
        <div className="mb-1 flex items-center justify-between gap-2">
          <span className="text-xs font-medium text-muted">{label}</span>
          <CopyButton text={text} />
        </div>
      )}
      <pre className={`max-h-72 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg ${tone}`}>{text}</pre>
    </div>
  )
}

// Note is the one-line result of the last action: what happened, and the commands that did it.
// It stays until the next action rather than fading, because the manifest it carries is the part
// worth reading and a toast would take it away mid-sentence.
function Note({ note, onClose }) {
  if (!note) return null
  const tone = note.error
    ? 'border-danger/30 bg-danger/10 text-danger'
    : 'border-accent/30 bg-accent/10 text-muted'
  return (
    <div className={`space-y-2 rounded-lg border px-3 py-2 text-[11px] leading-snug ${tone}`}>
      <div className="flex items-start justify-between gap-2">
        <span className={note.error ? '' : 'text-fg'}>{note.message}</span>
        <button onClick={onClose} className="shrink-0 rounded p-0.5 text-muted hover:bg-surface2 hover:text-fg">
          <Icon.Close size={13} />
        </button>
      </div>
      {note.warning && <div className="text-warning">{note.warning}</div>}
      {note.manifest && <Code label={`applied · ${note.archived || 'not archived'}`} text={note.manifest} />}
      {!!note.steps?.length && <Code label="what ran" text={note.steps.join('\n')} />}
      {note.command && <Code label="what ran" text={note.kubectl || note.command} />}
      {note.output && <Code label="output" text={note.output} />}
      {note.watch && <Code label="follow it" text={note.watch} />}
    </div>
  )
}

// ---------------------------------------------------------------- backups

function BackupsPane({ api, data, reload, setNote }) {
  const [busy, setBusy] = useState('')
  const [retain, setRetain] = useState(false)
  const [name, setName] = useState('')
  const [restoring, setRestoring] = useState(null) // the backup row a restore is being confirmed for
  const [pgOptions, setPgOptions] = useState('')

  const run = async (key, fn) => {
    setBusy(key); setNote(null)
    try { setNote(await fn()) } catch (e) { setNote({ error: true, message: e.message }) } finally { setBusy(''); reload() }
  }

  const take = (dryRun) => run(dryRun ? 'preview' : 'take',
    () => api.backupCreate({ name: name.trim(), retain, dryRun }))

  const backups = data.backups || []
  const canRestore = data.restoreByName

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Each row is a <span className="font-mono">{data.backupKind}</span> object. Taking one applies a manifest this
        panel writes and files under <span className="font-mono">{data.manifestDir}</span> on the server node —
        so every backup here is one you could have taken with <span className="font-mono">kubectl apply</span>,
        and can take that way next time.
      </div>

      <div className="space-y-2 rounded-lg border p-2">
        <div className="flex items-end gap-2">
          <label className="flex-1">
            <span className="mb-1 block text-xs font-medium text-muted">Name</span>
            <input className={inputCls} value={name} spellCheck={false}
              onChange={(e) => setName(e.target.value)} placeholder={`${data.cluster}-backup-<timestamp>`} />
          </label>
          <Button size="sm" disabled={!!busy} onClick={() => take(false)}>
            {busy === 'take' ? 'Taking…' : <><Icon.Play size={14} /> Take a backup</>}
          </Button>
          <Button variant="outline" size="sm" disabled={!!busy} onClick={() => take(true)}>
            Preview YAML
          </Button>
        </div>
        {data.finalizer && (
          <div className="flex items-start gap-2 pt-1">
            <Toggle checked={retain} onChange={setRetain} />
            <span className="text-[11px] leading-snug text-muted">
              <span className="font-medium text-fg">Delete the data with the object.</span>
              <Help text={TOOL_HELP.k8sBackupRetain} /> Sets the{' '}
              <span className="font-mono">{'percona.com/delete-backup'}</span> finalizer, so removing this backup
              later also clears what it wrote to <span className="font-mono">{data.bucket || 'the store'}</span>.
              Off is how the operator ships it: the object goes and the bytes stay.
            </span>
          </div>
        )}
      </div>

      {data.backupsError && (
        <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          {data.backupsError}
        </div>
      )}

      {backups.length === 0 && !data.backupsError && (
        <div className="rounded-lg border border-dashed px-3 py-6 text-center text-xs text-muted">
          No backups yet. The button above takes the first one.
        </div>
      )}

      <div className="space-y-2">
        {backups.map((b) => (
          <div key={b.name} className="space-y-1.5 rounded-lg border p-2">
            <div className="flex items-start justify-between gap-2">
              <div className="min-w-0">
                <div className="truncate font-mono text-xs text-fg">{b.name}</div>
                <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[10px] text-muted">
                  <Badge tone={TONE[b.state] || 'muted'}>{b.state}</Badge>
                  {b.storage && <span className="font-mono">{b.storage}</span>}
                  {b.type && <span>{b.type}</span>}
                  <span title={b.created}>{whenLabel(b.completed || b.created)}</span>
                  {b.retained && <Badge tone="warning">deletes its data</Badge>}
                </div>
              </div>
              <div className="flex shrink-0 items-center gap-1">
                {canRestore && b.state === 'Succeeded' && (
                  <Button variant="outline" size="sm" disabled={!!busy}
                    onClick={() => setRestoring(restoring === b.name ? null : b.name)}>
                    Restore…
                  </Button>
                )}
                {/* Only where deleting is one act. Where the operator can also clear the
                    storage, the choice is spelt out in the row below rather than hidden
                    behind an icon whose meaning would depend on a finalizer. */}
                {!data.finalizer && (
                  <ConfirmButton variant="ghost" size="sm" disabled={!!busy} confirmLabel="Delete object?"
                    onConfirm={() => run('del' + b.name, () => api.backupDelete(b.name, false))}>
                    <Icon.Trash size={14} />
                  </ConfirmButton>
                )}
              </div>
            </div>
            {b.destination && (
              <div className="truncate font-mono text-[10px] text-muted" title={b.destination}>{b.destination}</div>
            )}
            {b.error && <div className="text-[11px] leading-snug text-danger">{b.error}</div>}

            {/* The two deletes are separate buttons with separate words, because they are
                separate acts and the difference is the whole lesson. */}
            {data.finalizer && (
              <div className="flex flex-wrap items-center gap-2 border-t pt-1.5 text-[10px] text-muted">
                <span>Delete:</span>
                <ConfirmButton variant="outline" size="sm" disabled={!!busy} confirmLabel="Object only?"
                  onConfirm={() => run('del' + b.name, () => api.backupDelete(b.name, false))}>
                  the object
                </ConfirmButton>
                <ConfirmButton variant="danger" size="sm" disabled={!!busy} confirmLabel="Object AND data?"
                  onConfirm={() => run('deldata' + b.name, () => api.backupDelete(b.name, true))}>
                  the object and its data
                </ConfirmButton>
              </div>
            )}

            {restoring === b.name && (
              <div className="space-y-2 rounded-lg border border-warning/30 bg-warning/10 p-2">
                <div className="text-[11px] leading-snug text-muted">
                  <span className="font-medium text-fg">A restore replaces the cluster's data.</span>
                  <Help text={TOOL_HELP.k8sBackupRestore} />{' '}
                  {data.cluster} stops serving while the operator runs it, and what is in it now is gone —
                  on the MySQL operators the GTID history goes with it. This is not undoable; take a backup
                  first if what is there now matters.
                </div>
                <div className="flex items-center gap-2">
                  <Button variant="outline" size="sm" disabled={!!busy}
                    onClick={() => run('preview', () => api.restore({ backup: b.name, dryRun: true }))}>
                    Preview YAML
                  </Button>
                  <ConfirmButton variant="danger" size="sm" disabled={!!busy} confirmLabel="Restore over the cluster?"
                    onConfirm={() => { setRestoring(null); run('restore', () => api.restore({ backup: b.name })) }}>
                    Restore {data.cluster} from this
                  </ConfirmButton>
                </div>
              </div>
            )}
          </div>
        ))}
      </div>

      {/* PostgreSQL restores a pgBackRest repository rather than a backup object, so it gets one
          control at the bottom instead of a button per row — there is nothing per-row to press. */}
      {!canRestore && (
        <div className="space-y-2 rounded-lg border border-warning/30 bg-warning/10 p-2">
          <div className="text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">A PostgreSQL restore names a repository, not a backup.</span>{' '}
            <span className="font-mono">PerconaPGRestore</span> hands <span className="font-mono">{data.storage}</span>{' '}
            to pgBackRest and restores the latest backup in it. Add pgBackRest options to pick another —{' '}
            <span className="font-mono">--set=&lt;label&gt;</span> for a specific one, or{' '}
            <span className="font-mono">--type=time --target=…</span> for point-in-time. The cluster stops while it runs.
          </div>
          <input className={`${inputCls} font-mono text-xs`} value={pgOptions} spellCheck={false}
            onChange={(e) => setPgOptions(e.target.value)}
            placeholder="--type=immediate   (space-separated, optional)" />
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" disabled={!!busy}
              onClick={() => run('preview', () => api.restore({ options: pgOptions.split(/\s+/).filter(Boolean), dryRun: true }))}>
              Preview YAML
            </Button>
            <ConfirmButton variant="danger" size="sm" disabled={!!busy} confirmLabel="Restore over the cluster?"
              onConfirm={() => run('restore', () => api.restore({ options: pgOptions.split(/\s+/).filter(Boolean) }))}>
              Restore {data.cluster} from {data.storage}
            </ConfirmButton>
          </div>
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- restores

function RestoresPane({ api, data, reload, setNote }) {
  const [busy, setBusy] = useState('')
  const restores = data.restores || []

  const del = async (name) => {
    setBusy(name); setNote(null)
    try { setNote(await api.restoreDelete(name)) } catch (e) { setNote({ error: true, message: e.message }) }
    finally { setBusy(''); reload() }
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Each row is a <span className="font-mono">{data.restoreKind}</span> object — a restore that was asked for,
        and what became of it. Deleting one removes the record only: a restore that has run has already replaced
        the cluster's data, and there is nothing to undo by tidying the list.
      </div>
      {data.restoresError && (
        <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
          {data.restoresError}
        </div>
      )}
      {restores.length === 0 && !data.restoresError && (
        <div className="rounded-lg border border-dashed px-3 py-6 text-center text-xs text-muted">
          Nothing has been restored onto this cluster.
        </div>
      )}
      {restores.map((r) => (
        <div key={r.name} className="space-y-1 rounded-lg border p-2">
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0">
              <div className="truncate font-mono text-xs text-fg">{r.name}</div>
              <div className="mt-0.5 flex flex-wrap items-center gap-1.5 text-[10px] text-muted">
                <Badge tone={TONE[r.state] || 'muted'}>{r.state}</Badge>
                {r.backup && <span className="font-mono">from {r.backup}</span>}
                {!r.backup && r.storage && <span className="font-mono">from {r.storage}</span>}
                <span title={r.created}>{whenLabel(r.completed || r.created)}</span>
              </div>
            </div>
            <ConfirmButton variant="ghost" size="sm" disabled={busy === r.name} confirmLabel="Delete record?"
              onConfirm={() => del(r.name)}>
              <Icon.Trash size={14} />
            </ConfirmButton>
          </div>
          {r.error && <div className="text-[11px] leading-snug text-danger">{r.error}</div>}
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------- the bucket

function BucketPane({ api, data, setNote }) {
  const [prefix, setPrefix] = useState('')
  const [listing, setListing] = useState(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const [toolbox, setToolbox] = useState(null)
  const [loading, setLoading] = useState(false)

  // The toolbox status is read on mount but nothing is started: opening a tab should not pull an
  // image. The listing below is what starts the pod, and only once somebody asks for one.
  useEffect(() => { api.toolbox().then(setToolbox).catch(() => {}) }, [api])

  const load = useCallback(async (p, after) => {
    setLoading(true); setErr('')
    try {
      const r = await api.bucket(p, after)
      setListing((prev) => (after && prev
        ? { ...r, objects: [...prev.objects, ...r.objects] }
        : r))
      setToolbox((t) => ({ ...(t || {}), pod: r.pod, phase: 'Running' }))
    } catch (e) { setErr(e.message) } finally { setLoading(false) }
  }, [api])

  const go = (p) => { setPrefix(p); setListing(null); load(p) }

  const run = async (key, fn) => {
    setBusy(key); setNote(null)
    try { setNote(await fn()) } catch (e) { setNote({ error: true, message: e.message }) }
    finally { setBusy(''); load(prefix) }
  }

  const stop = async () => {
    setBusy('toolbox'); setNote(null)
    try { setNote(await api.toolboxAction('stop')); setToolbox((t) => ({ ...t, phase: '' })); setListing(null) }
    catch (e) { setNote({ error: true, message: e.message }) } finally { setBusy('') }
  }

  const crumbs = crumbsOf(prefix)
  const objs = listing?.objects || []

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-medium text-fg">This is the bucket, not Kubernetes.</span>
        <Help text={TOOL_HELP.k8sBucketToolbox} /> Every operation here runs{' '}
        <span className="font-mono">aws</span> in a pod on the cluster —{' '}
        <span className="font-mono">{toolbox?.pod || `${data.cluster}-dbcanvas-s3`}</span>, from an image that
        carries the AWS CLI — using the cluster's own backup credentials and endpoint. That is what lets it see
        what no API object reports: the binlogs the PITR collector is uploading, a prefix a failed backup left
        behind, a pgBackRest repository nothing has expired. The pod's manifest is filed with the rest, and the
        exact command line comes back with every answer.
      </div>

      <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg border p-2">
        <div className="min-w-0 text-[11px] text-muted">
          <span className="font-mono text-fg">s3://{data.bucket}</span>
          <span className="ml-2">{data.endpoint}</span>
          <div className="mt-0.5">
            toolbox{' '}
            {toolbox?.phase === 'Running'
              ? <Badge tone="success">running</Badge>
              : <Badge tone="muted">not running</Badge>}
            {toolbox?.image && <span className="ml-1.5 font-mono">{toolbox.image}</span>}
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Button variant="outline" size="sm" disabled={loading || !!busy} onClick={() => go(prefix)}>
            {loading ? 'Listing…' : 'List the bucket'}
          </Button>
          {toolbox?.phase === 'Running' && (
            <ConfirmButton variant="ghost" size="sm" disabled={!!busy} confirmLabel="Stop the pod?" onConfirm={stop}>
              Stop toolbox
            </ConfirmButton>
          )}
        </div>
      </div>

      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>}

      {listing && (
        <>
          <div className="flex flex-wrap items-center gap-1 text-[11px] text-muted">
            <button className="rounded px-1 hover:bg-surface2 hover:text-fg" onClick={() => go('')}>
              {data.bucket}
            </button>
            {crumbs.map((c) => (
              <span key={c.key} className="flex items-center gap-1">
                /
                <button className="rounded px-1 font-mono hover:bg-surface2 hover:text-fg" onClick={() => go(c.key)}>
                  {c.name}
                </button>
              </span>
            ))}
            {prefix && (
              <ConfirmButton variant="ghost" size="sm" className="ml-1" disabled={!!busy}
                confirmLabel={`Delete everything under ${prefix}/?`}
                onConfirm={() => run('rmdir', () => api.bucketDelete({ key: prefix, recursive: true }))}>
                <Icon.Trash size={12} /> delete this prefix
              </ConfirmButton>
            )}
            {prefix && (
              <Button variant="ghost" size="sm" disabled={!!busy}
                onClick={() => run('rmdry', () => api.bucketDelete({ key: prefix, recursive: true, dryRun: true }))}>
                dry run
              </Button>
            )}
          </div>

          {objs.length === 0 && (
            <div className="rounded-lg border border-dashed px-3 py-6 text-center text-xs text-muted">
              Nothing here.
            </div>
          )}

          <div className="divide-y rounded-lg border">
            {objs.map((o) => (
              <div key={o.key} className="flex items-center justify-between gap-2 px-2 py-1.5">
                <button className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
                  disabled={!o.dir} onClick={() => o.dir && go(o.key)}>
                  {o.dir ? <Icon.Folder size={14} /> : <Icon.File size={14} />}
                  <span className={`truncate font-mono text-xs ${o.dir ? 'text-accent' : 'text-fg'}`}>{o.name}</span>
                </button>
                <div className="flex shrink-0 items-center gap-1 text-[10px] text-muted">
                  {!o.dir && <span>{sizeLabel(o.size)}</span>}
                  {o.modified && <span title={o.modified}>{whenLabel(o.modified)}</span>}
                  {!o.dir && (
                    <a href={api.bucketDownloadURL(o.key)} download={o.name} title="Download"
                      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
                      <Icon.External size={13} />
                    </a>
                  )}
                  <ConfirmButton variant="ghost" size="sm" disabled={!!busy}
                    confirmLabel={o.dir ? 'Delete the whole prefix?' : 'Delete this object?'}
                    onConfirm={() => run('rm' + o.key, () => api.bucketDelete({ key: o.key, recursive: !!o.dir }))}>
                    <Icon.Trash size={13} />
                  </ConfirmButton>
                </div>
              </div>
            ))}
          </div>

          {listing.more && (
            <Button variant="outline" size="sm" className="w-full" disabled={loading}
              onClick={() => load(prefix, listing.after)}>
              {loading ? 'Loading…' : 'Load more'}
            </Button>
          )}
          {listing.kubectl && <Code label="the listing you just saw, as a command" text={listing.kubectl} />}
        </>
      )}
    </div>
  )
}

// ---------------------------------------------------------------- manifests

function ManifestsPane({ api }) {
  const [files, setFiles] = useState(null)
  const [dir, setDir] = useState('')
  const [open, setOpen] = useState(null)
  const [body, setBody] = useState('')
  const [err, setErr] = useState('')

  useEffect(() => {
    api.backupManifests().then((r) => { setFiles(r.files || []); setDir(r.dir) }).catch((e) => setErr(e.message))
  }, [api])

  const show = async (name) => {
    if (open === name) { setOpen(null); return }
    setOpen(name); setBody('')
    try { setBody((await api.backupManifests(name)).content) } catch (e) { setBody('# ' + e.message) }
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Everything this tab has applied, as files on the server node in{' '}
        <span className="font-mono">{dir || 'the operator source tree'}</span> — beside the{' '}
        <span className="font-mono">backup.yaml</span> and <span className="font-mono">restore.yaml</span> samples
        the release ships. Each carries the commands to apply it again, so the panel is a way to learn this
        rather than a thing you have to keep using.
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>}
      {files?.length === 0 && (
        <div className="rounded-lg border border-dashed px-3 py-6 text-center text-xs text-muted">
          Nothing filed yet — take a backup and it will appear here.
        </div>
      )}
      {files?.map((f) => (
        <div key={f} className="rounded-lg border">
          <button className="flex w-full items-center justify-between gap-2 px-2 py-1.5 text-left"
            onClick={() => show(f)}>
            <span className="truncate font-mono text-xs text-fg">{f}</span>
            <Icon.Chevron size={14} className={open === f ? 'rotate-180' : ''} />
          </button>
          {open === f && <div className="border-t p-2"><Code text={body || 'Loading…'} /></div>}
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------- the tab

const PANES = [
  { id: 'backups', label: 'Backups' },
  { id: 'restores', label: 'Restores' },
  { id: 'bucket', label: 'Bucket' },
  { id: 'manifests', label: 'Manifests' },
]

export function K8sBackupManager({ stackId, frame, isServer }) {
  const api = useMemo(() => (frame ? k3dApi(stackId, frame.id) : null), [stackId, frame])
  const [pane, setPane] = useState('backups')
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')
  const [note, setNote] = useState(null)

  const reload = useCallback(() => {
    if (!api) return
    api.backups().then((r) => { setData(r); setErr('') }).catch((e) => setErr(e.message))
  }, [api])

  // Only the two Kubernetes tables are polled. The bucket is not: a listing costs a pod exec,
  // and an object store does not change on its own — it changes when the operator writes to it,
  // which is what the backup table is already reporting.
  //
  // Through the shared hook rather than a setInterval of our own, so the loop stops when the tab
  // is hidden or the page is off screen. A raw timer here would go on asking a k3s node for two
  // resource listings every five seconds behind whatever the user actually switched to.
  usePolling(reload, POLL_MS, { enabled: isServer })

  if (!isServer) {
    return (
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Backups are a cluster-wide thing — open the <span className="font-medium text-fg">server</span> node to manage them.
      </div>
    )
  }
  if (err) {
    return <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>
  }
  if (!data) return <div className="text-xs text-muted">Loading…</div>

  // A cluster with no object store can still take backups (to a PVC) and restore them, so the
  // tab is not withheld — only the pane that has nothing to show is.
  const hasBucket = !!data.bucket
  const panes = PANES.filter((p) => p.id !== 'bucket' || hasBucket)
  const shown = panes.some((p) => p.id === pane) ? pane : 'backups'

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-1">
        {panes.map((p) => (
          <button key={p.id} onClick={() => setPane(p.id)}
            className={`rounded-lg px-2 py-1 text-xs font-medium transition ${shown === p.id
              ? 'bg-primary text-white' : 'text-muted hover:bg-surface2 hover:text-fg'}`}>
            {p.label}
            {p.id === 'backups' && data.backups?.length ? ` · ${data.backups.length}` : ''}
            {p.id === 'restores' && data.restores?.length ? ` · ${data.restores.length}` : ''}
          </button>
        ))}
      </div>

      <div className="flex flex-wrap items-center gap-1.5 text-[10px] text-muted">
        <span className="font-mono text-fg">{data.cluster}</span>
        <span>in</span>
        <span className="font-mono">{data.namespace}</span>
        <span>·</span>
        <span>{data.repo || 'no object store'}</span>
        {data.storage && <><span>·</span><span className="font-mono">{data.storageField}: {data.storage}</span></>}
      </div>

      <Note note={note} onClose={() => setNote(null)} />

      {shown === 'backups' && <BackupsPane api={api} data={data} reload={reload} setNote={setNote} />}
      {shown === 'restores' && <RestoresPane api={api} data={data} reload={reload} setNote={setNote} />}
      {shown === 'bucket' && <BucketPane api={api} data={data} setNote={setNote} />}
      {shown === 'manifests' && <ManifestsPane api={api} />}
    </div>
  )
}
