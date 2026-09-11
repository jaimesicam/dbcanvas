import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Button, inputCls } from '../components/ui.jsx'
import { seaweedApi, seaweedNodes } from '../lib/stackApi.js'
import { useSettings } from '../settings/SettingsProvider.jsx'

// SeaweedFileManager — a two-pane file manager over the buckets of a stack's SeaweedFS
// nodes.
//
// The node panel's Buckets tab answers "did the backup land?". This answers the other
// half — get that object onto my machine, put this dump into the bucket, copy last
// night's artefact into the bucket the other cluster restores from. Two panes because
// the copy is the feature that shapes the rest: with a node + bucket picker per pane, a
// transfer is "the selection here, into the folder there", and browsing one bucket is
// the case where both panes point at the same node.
//
// A pane is (node, bucket, folder) rather than (node, path): a bucket is not a
// directory you can walk up out of, so the breadcrumb stops at the bucket and the
// bucket is a control of its own.
//
// Everything here goes through app/seaweedfs_files.go; see that file for why an object
// is staged through the container rather than read straight off a disk.

const fmtSize = (n, dir) => {
  if (dir) return '—'
  const U = [[1024 ** 3, 'G'], [1024 ** 2, 'M'], [1024, 'K']]
  for (const [size, label] of U) if (n >= size) return `${(n / size).toFixed(n / size < 10 ? 1 : 0)}${label}`
  return `${n}`
}

const fmtWhen = (iso) => {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (v) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const parentKey = (folder) => {
  const i = folder.lastIndexOf('/')
  return i < 0 ? '' : folder.slice(0, i)
}

// usePane owns one side: which node, which bucket, which folder, what is in it and
// what is selected. Both panes are identical, which is what makes the copy symmetric.
function usePane(stackId, initialNodeId, initialBucket) {
  const [nodeId, setNodeId] = useState(initialNodeId)
  const [bucket, setBucket] = useState(initialBucket)
  const [path, setPath] = useState('')
  const [objects, setObjects] = useState([])
  const [selected, setSelected] = useState(() => new Set())
  const [more, setMore] = useState(false)
  const [after, setAfter] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const api = useMemo(() => seaweedApi(stackId, nodeId), [stackId, nodeId])

  const load = useCallback(async (to) => {
    const folder = to ?? path
    if (!bucket) { setObjects([]); return }
    setLoading(true)
    setError('')
    try {
      const r = await api.objects(bucket, folder)
      setObjects(r.objects || [])
      setPath(r.path ?? folder)
      setMore(!!r.more)
      setAfter(r.after || '')
      setSelected(new Set())
    } catch (e) {
      setError(e.message)
      setObjects([])
      setMore(false)
    } finally {
      setLoading(false)
    }
  }, [api, bucket, path])

  // One page at a time, appended: the filer pages with lastFileName, and a bucket with
  // thousands of objects is walked rather than materialised.
  const loadMore = useCallback(async () => {
    setLoading(true)
    try {
      const r = await api.objects(bucket, path, after)
      setObjects((prev) => [...prev, ...(r.objects || [])])
      setMore(!!r.more)
      setAfter(r.after || '')
    } catch (e) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }, [api, bucket, path, after])

  // A folder from one bucket rarely exists in another, so changing node or bucket goes
  // back to the bucket root.
  useEffect(() => { setPath(''); load('') /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [nodeId, bucket])

  // The listing already carries each entry's bucket-relative path, so a selection is
  // read off the rows rather than rebuilt from names and the current folder.
  const selectedObjects = useMemo(() => objects.filter((o) => selected.has(o.name)), [objects, selected])

  return {
    nodeId, setNodeId, bucket, setBucket, path, objects, selected, setSelected,
    selectedObjects, more, loading, error, api, load, loadMore,
  }
}

export default function SeaweedFileManager({ stackId, nodeId, nodeLabel, buckets = [], onClose }) {
  const [nodes, setNodes] = useState([{ id: nodeId, label: nodeLabel, buckets }])
  const [split, setSplit] = useState(false)
  const [focus, setFocus] = useState('a')
  const [busy, setBusy] = useState('')
  const [flash, setFlash] = useState(null)
  const [confirm, setConfirm] = useState(null)
  const { system } = useSettings()

  const A = usePane(stackId, nodeId, buckets[0] || '')
  const B = usePane(stackId, nodeId, buckets[1] || buckets[0] || '')
  const src = focus === 'a' ? A : B
  const dst = focus === 'a' ? B : A

  useEffect(() => {
    seaweedNodes(stackId)
      .then((r) => { if (r?.nodes?.length) setNodes(r.nodes) })
      .catch(() => { /* keep the node we were opened on */ })
  }, [stackId])

  useEffect(() => {
    if (!flash) return
    const t = setTimeout(() => setFlash(null), 5000)
    return () => clearTimeout(t)
  }, [flash])

  const nodeOf = (id) => nodes.find((n) => n.id === id)
  const labelOf = (id) => nodeOf(id)?.label || id
  const bucketsOf = (id) => nodeOf(id)?.buckets || []

  // run wraps every mutation: one place for the busy flag, the flash and the reload
  // that has to follow a change nothing else will report.
  const run = async (what, fn, refresh = [src]) => {
    setBusy(what)
    try {
      const msg = await fn()
      if (msg) setFlash({ tone: 'ok', text: msg })
      for (const p of refresh) await p.load()
    } catch (e) {
      setFlash({ tone: 'err', text: e.message })
    } finally {
      setBusy('')
    }
  }

  // Exactly one object, because the endpoint streams one object: a selection would
  // have to be archived first, which for a pgBackRest repository is a different
  // feature with a different cost.
  const one = src.selectedObjects.length === 1 && !src.selectedObjects[0].dir
  const onDownload = () => {
    if (!one) return
    window.location.href = src.api.downloadURL(src.bucket, src.selectedObjects[0].path)
  }

  // Takes the pane explicitly rather than acting on the focused one: a drop lands on the
  // pane it was dropped on, and the setFocus that same drop fires has not been applied yet
  // when the upload starts.
  const uploadInto = (pane, files) => run('upload', async () => {
    await pane.api.upload(pane.bucket, pane.path, files)
    const n = files.length
    return `Uploaded ${n} file${n === 1 ? '' : 's'} to ${pane.bucket}/${pane.path || ''}`
  }, [pane])

  // Delete asks first, and the question says which of the two things is about to happen: a
  // folder here is a whole backup (pbm/<cluster>, pgbackrest/<cluster>/repo1), so the recursive
  // case is named rather than folded into a count.
  const onDelete = () => {
    const sel = src.selectedObjects
    if (sel.length === 0) return
    const dirs = sel.filter((o) => o.dir).length
    setConfirm({
      title: `Delete ${sel.length} item${sel.length === 1 ? '' : 's'} from ${src.bucket}?`,
      body: dirs
        ? `${dirs === sel.length ? 'That' : `${dirs} of them`} ${dirs === 1 ? 'is a folder' : 'are folders'} — everything inside comes with it. A backup prefix is one folder. This cannot be undone.`
        : 'This cannot be undone.',
      onConfirm: () => run('delete', async () => {
        await src.api.remove(src.bucket, sel.map((o) => o.path), dirs > 0)
        return `Deleted ${sel.length} item${sel.length === 1 ? '' : 's'} from ${src.bucket}.`
      }),
    })
  }

  const onTransfer = () => {
    if (!split) { setFlash({ tone: 'err', text: 'Open the second pane to pick a destination bucket.' }); return }
    const keys = src.selectedObjects.filter((o) => !o.dir).map((o) => o.path)
    if (keys.length === 0) { setFlash({ tone: 'err', text: 'Select one or more objects — folders are not copied.' }); return }
    if (src.nodeId === dst.nodeId && src.bucket === dst.bucket) {
      setFlash({ tone: 'err', text: 'Both panes are on the same bucket — pick a different destination.' })
      return
    }
    run('transfer', async () => {
      await src.api.transfer(src.bucket, keys, dst.nodeId, dst.bucket, dst.path)
      return `Copied ${keys.length} object${keys.length === 1 ? '' : 's'} to ${dst.bucket}/${dst.path || ''} on ${labelOf(dst.nodeId)}.`
    }, [dst])
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onMouseDown={onClose}>
      <div
        className="flex h-[min(85vh,900px)] w-full max-w-[min(1500px,95vw)] flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <header className="flex shrink-0 items-center gap-3 border-b px-4 py-2.5">
          <Icon.Bucket size={18} />
          <h2 className="text-sm font-semibold">Bucket File Manager</h2>
          <span className="text-xs text-muted">
            {nodes.length} running SeaweedFS node{nodes.length === 1 ? '' : 's'} in this stack
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button variant="ghost" size="sm" onClick={() => setSplit((s) => !s)}>
              {split ? 'Single pane' : 'Split (copy between buckets)'}
            </Button>
            <button onClick={onClose} title="Close file manager" className="rounded-md p-1 text-muted hover:bg-surface2 hover:text-fg">
              <Icon.Close size={16} />
            </button>
          </div>
        </header>

        <Toolbar
          pane={src} split={split} busy={busy} dst={dst} labelOf={labelOf} canDownload={one}
          onUp={() => src.load(parentKey(src.path))}
          onRefresh={() => src.load()}
          onUpload={(files) => uploadInto(src, files)}
          onDownload={onDownload}
          onDelete={onDelete}
          onTransfer={onTransfer}
          maxUpload={system.maxUploadBytes}
          onError={(text) => setFlash({ tone: 'err', text })}
        />

        {flash && (
          <div className={`shrink-0 border-b px-4 py-2 text-xs ${flash.tone === 'err' ? 'bg-danger/15 text-danger' : 'bg-success/10 text-success'}`}>
            {flash.text}
          </div>
        )}

        <div className="flex min-h-0 flex-1">
          <Pane pane={A} nodes={nodes} bucketsOf={bucketsOf} active={focus === 'a'} onFocus={() => setFocus('a')} split={split} onUpload={(files) => uploadInto(A, files)} maxUpload={system.maxUploadBytes} onError={(text) => setFlash({ tone: 'err', text })} />
          {split && <div className="w-px shrink-0 bg-border" />}
          {split && <Pane pane={B} nodes={nodes} bucketsOf={bucketsOf} active={focus === 'b'} onFocus={() => setFocus('b')} split onUpload={(files) => uploadInto(B, files)} maxUpload={system.maxUploadBytes} onError={(text) => setFlash({ tone: 'err', text })} />}
        </div>

        <footer className="shrink-0 border-t px-4 py-1.5 text-[11px] text-muted">
          Acting on the {focus === 'a' ? 'left' : 'right'} pane · {src.selected.size} selected
          {split && ` · copy target: ${labelOf(dst.nodeId)}:${dst.bucket}/${dst.path}`}
        </footer>
      </div>

      {confirm && (
        <ConfirmDialog
          title={confirm.title} body={confirm.body}
          onClose={() => setConfirm(null)}
          onConfirm={confirm.onConfirm}
        />
      )}
    </div>,
    document.body,
  )
}

// ConfirmDialog is the one question this manager asks. Same shape as the node file manager's,
// because the answer to "am I about to destroy something?" should look the same in both.
function ConfirmDialog({ title, body, onConfirm, onClose }) {
  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">{title}</h3>
        <p className="mb-4 text-xs text-muted">{body}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button size="sm" variant="danger" onClick={() => { onClose(); onConfirm() }}>Delete</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// Toolbar acts on the focused pane. Upload takes a file input here and a drop on the
// listing, both landing in that pane's current folder.
function Toolbar({ pane, split, busy, dst, labelOf, canDownload, onUp, onRefresh, onUpload, onDownload, onDelete, onTransfer, maxUpload, onError }) {
  const fileRef = useRef(null)
  const any = pane.selected.size > 0

  return (
    <div className="flex shrink-0 flex-wrap items-center gap-1.5 border-b px-4 py-2">
      <Button variant="ghost" size="sm" onClick={onUp} disabled={!pane.path}><Icon.ArrowLeft size={14} /> Up</Button>
      <Button variant="ghost" size="sm" onClick={onRefresh}>Refresh</Button>
      <span className="mx-1 h-5 w-px bg-border" />
      <Button variant="ghost" size="sm" disabled={!!busy || !pane.bucket} onClick={() => fileRef.current?.click()}>
        {busy === 'upload' ? 'Uploading…' : 'Upload'}
      </Button>
      <input
        ref={fileRef} type="file" multiple className="hidden"
        onChange={(e) => { pickUpload(e.target.files, maxUpload, onUpload, onError); e.target.value = '' }}
      />
      <Button variant="ghost" size="sm" onClick={onDownload} disabled={!canDownload}>Download</Button>
      {!canDownload && any && <span className="text-[11px] text-muted">one object at a time</span>}
      <Button variant="ghost" size="sm" onClick={onDelete} disabled={!any || !!busy}>
        <span className="text-danger">{busy === 'delete' ? 'Deleting…' : 'Delete'}</span>
      </Button>
      {split && (
        <>
          <span className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" onClick={onTransfer} disabled={!any || !!busy}>
            {busy === 'transfer' ? 'Copying…' : `Copy to ${labelOf(dst.nodeId)}:${dst.bucket}`}
          </Button>
        </>
      )}
    </div>
  )
}

// pickUpload turns a FileList into the upload's parts and refuses one that is over the
// instance's ceiling before anything is sent — the server refuses it too, but only
// after the whole body has been uploaded.
function pickUpload(fileList, maxUpload, onUpload, onError) {
  const files = [...fileList].map((file) => ({ path: file.webkitRelativePath || file.name, file }))
  if (files.length === 0) return
  const total = files.reduce((s, f) => s + f.file.size, 0)
  if (maxUpload > 0 && total > maxUpload) {
    onError("That selection is larger than this instance's upload limit. An admin can raise it in Settings.")
    return
  }
  onUpload(files)
}

// Pane is one node + one bucket + one folder.
function Pane({ pane, nodes, bucketsOf, active, onFocus, split, onUpload, maxUpload, onError }) {
  const { path, objects, selected, setSelected, loading, error } = pane
  const [lastIndex, setLastIndex] = useState(null)
  const [drop, setDrop] = useState(false)
  const paneBuckets = bucketsOf(pane.nodeId)

  // A node whose buckets arrive after the pane was built (the listing endpoint answers
  // later than the first render) may leave the pane on a bucket that node does not
  // have; snap to its first.
  useEffect(() => {
    if (paneBuckets.length && !paneBuckets.includes(pane.bucket)) pane.setBucket(paneBuckets[0])
  }, [paneBuckets, pane.bucket]) // eslint-disable-line react-hooks/exhaustive-deps

  // Range-select with shift, toggle with ctrl/meta, plain click replaces — the
  // selection model every file manager has, so muscle memory carries over.
  const onRowClick = (e, obj, i) => {
    onFocus()
    setSelected((prev) => {
      const next = new Set(prev)
      if (e.shiftKey && lastIndex !== null) {
        const [lo, hi] = lastIndex < i ? [lastIndex, i] : [i, lastIndex]
        for (let k = lo; k <= hi; k++) next.add(objects[k].name)
        return next
      }
      if (e.ctrlKey || e.metaKey) {
        next.has(obj.name) ? next.delete(obj.name) : next.add(obj.name)
        return next
      }
      return new Set([obj.name])
    })
    setLastIndex(i)
  }

  const crumbs = path ? path.split('/') : []

  return (
    <div
      className={`flex min-w-0 flex-1 flex-col ${active && split ? 'bg-primary/[0.03]' : ''}`}
      onMouseDown={onFocus}
      onDragOver={(e) => { e.preventDefault(); onFocus(); setDrop(true) }}
      onDragLeave={() => setDrop(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDrop(false)
        onFocus()
        pickUpload(e.dataTransfer.files, maxUpload, onUpload, onError)
      }}
    >
      <div className="flex shrink-0 items-center gap-2 border-b px-3 py-1.5">
        <div className="w-32 shrink-0">
          <select className={`${inputCls} h-7 py-0 text-xs`} value={pane.nodeId} onChange={(e) => pane.setNodeId(e.target.value)}>
            {nodes.map((n) => <option key={n.id} value={n.id}>{n.label}</option>)}
          </select>
        </div>
        <div className="w-28 shrink-0">
          <select className={`${inputCls} h-7 py-0 text-xs`} value={pane.bucket} onChange={(e) => pane.setBucket(e.target.value)}>
            {paneBuckets.length === 0 && <option value="">no buckets</option>}
            {paneBuckets.map((b) => <option key={b} value={b}>{b}</option>)}
          </select>
        </div>
        <div className="flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto whitespace-nowrap text-xs">
          <button className="rounded px-1 py-0.5 font-mono hover:bg-surface2" onClick={() => pane.load('')}>/</button>
          {crumbs.map((c, i) => (
            <span key={i} className="flex items-center">
              <button
                className="rounded px-1 py-0.5 font-mono hover:bg-surface2"
                onClick={() => pane.load(crumbs.slice(0, i + 1).join('/'))}
              >{c}</button>
              {i < crumbs.length - 1 && <span className="text-muted">/</span>}
            </span>
          ))}
        </div>
        {active && split && <span className="shrink-0 rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-medium text-primary">active</span>}
      </div>

      <div className={`min-h-0 flex-1 overflow-auto ${drop ? 'bg-primary/10' : ''}`}>
        {error && <div className="m-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{error}</div>}
        {loading && objects.length === 0 && <div className="p-4 text-xs text-muted">Reading {pane.bucket}/{path}…</div>}
        {!loading && !error && objects.length === 0 && (
          <div className="p-4 text-xs text-muted">{path ? 'This folder is empty.' : 'This bucket is empty.'} Drop files here to upload.</div>
        )}
        {objects.length > 0 && (
          <table className="w-full text-xs">
            <thead className="sticky top-0 bg-surface text-[10px] uppercase tracking-wide text-muted">
              <tr>
                <th className="w-full px-3 py-1.5 text-left font-medium">Name</th>
                <th className="whitespace-nowrap px-2 py-1.5 text-right font-medium">Size</th>
                <th className="whitespace-nowrap px-3 py-1.5 text-left font-medium">Modified</th>
              </tr>
            </thead>
            <tbody>
              {objects.map((o, i) => {
                const on = selected.has(o.name)
                return (
                  <tr
                    key={o.path}
                    onClick={(ev) => onRowClick(ev, o, i)}
                    onDoubleClick={() => o.dir && pane.load(o.path)}
                    className={`cursor-default select-none border-t border-border/40 ${on ? 'bg-primary/15' : 'hover:bg-surface2'}`}
                  >
                    <td className="max-w-0 px-3 py-1">
                      <div className="flex items-center gap-1.5">
                        <span className={`shrink-0 ${o.dir ? 'text-primary' : 'text-muted'}`}>
                          {o.dir ? <Icon.Folder size={13} /> : <Icon.File size={13} />}
                        </span>
                        <span className="truncate">{o.name}{o.dir ? '/' : ''}</span>
                      </div>
                    </td>
                    <td className="whitespace-nowrap px-2 py-1 text-right tabular-nums text-muted">{fmtSize(o.size, o.dir)}</td>
                    <td className="whitespace-nowrap px-3 py-1 tabular-nums text-muted">{fmtWhen(o.modified)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
        {pane.more && (
          <div className="p-2">
            <Button variant="outline" size="sm" className="w-full" onClick={pane.loadMore} disabled={loading}>
              {loading ? 'Loading…' : 'Load more'}
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}
