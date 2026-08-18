import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Button, inputCls } from '../components/ui.jsx'
import { fsApi, fsNodes } from '../lib/stackApi.js'
import { useSettings } from '../settings/SettingsProvider.jsx'

// FileManager — a two-pane file manager over a deployed node's filesystem.
//
// Two panes rather than one, because "copy between two nodes" is the feature
// that shapes everything else: with a node picker per pane, a transfer is just
// "the selection in this pane, into the other pane's directory", and browsing
// one node is the degenerate case where both panes point at the same place.
//
// Everything the panes do goes through app/nodefs.go; see that file for why
// this is allowed to be arbitrary read/write.

const fmtSize = (n, type) => {
  if (type === 'dir') return '—'
  const U = [[1024 ** 3, 'G'], [1024 ** 2, 'M'], [1024, 'K']]
  for (const [size, label] of U) if (n >= size) return `${(n / size).toFixed(n / size < 10 ? 1 : 0)}${label}`
  return `${n}`
}

const fmtTime = (unix) => {
  if (!unix) return ''
  const d = new Date(unix * 1000)
  const pad = (v) => String(v).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// joinPath keeps the parent/child arithmetic in one place — "/" + name would
// give "//name", and every pane needs both directions.
const joinPath = (dir, name) => (dir === '/' ? `/${name}` : `${dir}/${name}`)
const parentPath = (dir) => {
  if (dir === '/') return '/'
  const i = dir.lastIndexOf('/')
  return i <= 0 ? '/' : dir.slice(0, i)
}

const ICON_FOR = { dir: 'Folder', link: 'Link', file: 'File', other: 'File' }

// usePane owns one side: which node, which directory, what is there, and what
// is selected. Both panes are identical, which is what makes the transfer
// symmetric.
function usePane(stackId, initialNodeId) {
  const [nodeId, setNodeId] = useState(initialNodeId)
  const [path, setPath] = useState('/')
  const [entries, setEntries] = useState([])
  const [selected, setSelected] = useState(() => new Set())
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const api = useMemo(() => fsApi(stackId, nodeId), [stackId, nodeId])

  const load = useCallback(async (to) => {
    const target = to ?? path
    setLoading(true)
    setError('')
    try {
      const r = await api.list(target)
      setEntries(r.entries || [])
      setPath(r.path)
      setSelected(new Set())
    } catch (e) {
      setError(e.message)
      setEntries([])
    } finally {
      setLoading(false)
    }
  }, [api, path])

  // Re-read whenever the node changes; a path from one node rarely exists on
  // another, so switching nodes goes back to the root.
  useEffect(() => { setPath('/'); load('/') /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [nodeId])

  const selectedPaths = useMemo(() => [...selected].map((n) => joinPath(path, n)), [selected, path])
  const selectedEntries = useMemo(() => entries.filter((e) => selected.has(e.name)), [entries, selected])

  return { nodeId, setNodeId, path, setPath, entries, selected, setSelected, selectedPaths, selectedEntries, loading, error, setError, api, load }
}

export default function FileManager({ stackId, nodeId, nodeLabel, onClose }) {
  const [nodes, setNodes] = useState([{ id: nodeId, label: nodeLabel }])
  const [split, setSplit] = useState(false) // second pane shown?
  const [focus, setFocus] = useState('a')   // which pane actions apply to
  const [dialog, setDialog] = useState(null)
  const [busy, setBusy] = useState('')
  const [flash, setFlash] = useState(null)
  const { system } = useSettings()

  const A = usePane(stackId, nodeId)
  const B = usePane(stackId, nodeId)
  const src = focus === 'a' ? A : B
  const dst = focus === 'a' ? B : A

  useEffect(() => {
    fsNodes(stackId).then((r) => { if (r?.nodes?.length) setNodes(r.nodes) }).catch(() => { /* keep the one node we were opened on */ })
  }, [stackId])

  useEffect(() => {
    if (!flash) return
    const t = setTimeout(() => setFlash(null), 5000)
    return () => clearTimeout(t)
  }, [flash])

  const labelOf = (id) => nodes.find((n) => n.id === id)?.label || id

  // run wraps every mutation: one place for the busy flag, the error banner,
  // and the reload that has to follow a change nobody else will tell us about.
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

  const onTransfer = () => {
    if (!split) { setFlash({ tone: 'err', text: 'Open the second pane to pick a destination node.' }); return }
    if (src.selectedPaths.length === 0) return
    if (src.nodeId === dst.nodeId) { setFlash({ tone: 'err', text: 'Both panes are on the same node — copying there is a move, not a transfer.' }); return }
    const n = src.selectedPaths.length
    run('transfer', async () => {
      await src.api.transfer(src.selectedPaths, dst.nodeId, dst.path)
      return `Copied ${n} item${n === 1 ? '' : 's'} to ${dst.path} on ${labelOf(dst.nodeId)}.`
    }, [dst])
  }

  const onDownload = () => {
    if (src.selectedPaths.length === 0) return
    // A plain navigation, so the browser's own download machinery handles a
    // multi-gigabyte file instead of buffering it in a fetch.
    window.location.href = src.api.downloadURL(src.selectedPaths)
  }

  const onDelete = () => onDeleteIn(src)
  const onDeleteIn = (pane) => {
    const paths = pane.selectedPaths
    if (paths.length === 0) return
    const hasDir = pane.selectedEntries.some((e) => e.type === 'dir')
    setDialog({
      kind: 'confirm',
      title: `Delete ${paths.length} item${paths.length === 1 ? '' : 's'}?`,
      body: hasDir
        ? 'The selection includes a directory; deleting removes it and everything inside. This cannot be undone.'
        : 'This cannot be undone.',
      danger: true,
      confirmLabel: 'Delete',
      onConfirm: () => run('delete', async () => {
        await pane.api.remove(paths, hasDir)
        return `Deleted ${paths.length} item${paths.length === 1 ? '' : 's'}.`
      }, [pane]),
    })
  }

  // paneAction is what a row's right-click menu triggers. It routes to the same
  // handlers the toolbar uses, so the two can never drift apart.
  const paneAction = (pane) => async (action, entry) => {
    setFocus(pane === A ? 'a' : 'b')
    switch (action) {
      case 'open':
        return pane.load(joinPath(pane.path, entry.name))
      case 'edit':
        return openEditor(pane, entry)
      case 'download':
        return (window.location.href = pane.api.downloadURL(pane.selectedPaths))
      case 'rename':
        return setDialog({ kind: 'rename', pane, entry })
      case 'props':
        return setDialog({ kind: 'props', pane })
      case 'delete':
        return onDeleteIn(pane)
      default:
    }
  }

  // openEditor pulls the file first, so the dialog opens with content rather
  // than a spinner that might turn into "too large" or "that is binary".
  const openEditor = async (pane, entry) => {
    const p = joinPath(pane.path, entry.name)
    setBusy('open')
    try {
      const f = await pane.api.read(p)
      setDialog({ kind: 'edit', pane, file: f, name: entry.name })
    } catch (e) {
      setFlash({ tone: 'err', text: e.message })
    } finally {
      setBusy('')
    }
  }

  // newFile opens the editor on an empty buffer rather than creating the file
  // straight away, so a new file and an existing one are the same dialog — and
  // nothing is written until Save, which is what "Cancel" has to mean.
  const newFile = (pane, name) => {
    // Refuse a name already in the directory: this dialog starts from an empty
    // buffer, so saving over an existing file would silently empty it.
    if (pane.entries.some((e) => e.name === name)) {
      setFlash({ tone: 'err', text: `${name} already exists here — open it with Edit… instead.` })
      return
    }
    setDialog({
      kind: 'edit', pane, name, isNew: true,
      // mode/uid/gid are what the save writes: a plain root-owned 0644 file,
      // the same thing `touch` would leave. modTime 0 skips the stale check,
      // which is how the write handler recognises a file that does not exist.
      file: { path: joinPath(pane.path, name), content: '', mode: '0644', uid: 0, gid: 0, modTime: 0 },
    })
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onMouseDown={onClose}>
      <div
        className="flex h-[min(85vh,900px)] w-full max-w-[min(1500px,95vw)] flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <header className="flex shrink-0 items-center gap-3 border-b px-4 py-2.5">
          <Icon.Folder size={18} />
          <h2 className="text-sm font-semibold">File Manager</h2>
          <span className="text-xs text-muted">{nodes.length} running node{nodes.length === 1 ? '' : 's'} in this stack</span>
          <div className="ml-auto flex items-center gap-2">
            <Button variant="ghost" size="sm" onClick={() => setSplit((s) => !s)}>
              {split ? 'Single pane' : 'Split (transfer between nodes)'}
            </Button>
            <button onClick={onClose} title="Close file manager" className="rounded-md p-1 text-muted hover:bg-surface2 hover:text-fg">
              <Icon.Close size={16} />
            </button>
          </div>
        </header>

        <Toolbar
          pane={src} split={split} busy={busy} dst={dst} labelOf={labelOf}
          onUp={() => src.load(parentPath(src.path))}
          onRefresh={() => src.load()}
          onNewFolder={() => setDialog({ kind: 'mkdir', pane: src })}
          onNewFile={() => setDialog({ kind: 'newfile', pane: src })}
          onRename={() => setDialog({ kind: 'rename', pane: src, entry: src.selectedEntries[0] })}
          onProps={() => setDialog({ kind: 'props', pane: src })}
          onDelete={onDelete}
          onDownload={onDownload}
          onUpload={(files) => run('upload', async () => {
            await src.api.upload(src.path, files)
            return `Uploaded ${files.length} file${files.length === 1 ? '' : 's'} to ${src.path}.`
          })}
          onTransfer={onTransfer}
          onEdit={() => openEditor(src, src.selectedEntries[0])}
          maxUpload={system.maxUploadBytes}
          onError={(text) => setFlash({ tone: 'err', text })}
        />

        {flash && (
          <div className={`shrink-0 border-b px-4 py-2 text-xs ${flash.tone === 'err' ? 'bg-danger/15 text-danger' : 'bg-success/10 text-success'}`}>
            {flash.text}
          </div>
        )}

        <div className="flex min-h-0 flex-1">
          <Pane pane={A} nodes={nodes} active={focus === 'a'} onFocus={() => setFocus('a')} split={split} onAction={paneAction(A)} />
          {split && <div className="w-px shrink-0 bg-border" />}
          {split && <Pane pane={B} nodes={nodes} active={focus === 'b'} onFocus={() => setFocus('b')} split onAction={paneAction(B)} />}
        </div>

        <footer className="shrink-0 border-t px-4 py-1.5 text-[11px] text-muted">
          Acting on the {focus === 'a' ? 'left' : 'right'} pane · {src.selected.size} selected
          {split && ` · transfer target: ${labelOf(dst.nodeId)}:${dst.path}`}
        </footer>
      </div>

      {dialog?.kind === 'confirm' && <ConfirmDialog {...dialog} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'mkdir' && (
        <PromptDialog
          title="New folder" label="Name" confirmLabel="Create"
          onClose={() => setDialog(null)}
          onSubmit={(name) => run('mkdir', async () => {
            await dialog.pane.api.mkdir(joinPath(dialog.pane.path, name))
            return `Created ${name}.`
          }, [dialog.pane])}
        />
      )}
      {dialog?.kind === 'newfile' && (
        <PromptDialog
          title="New file" label="Name" confirmLabel="Create"
          onClose={() => setDialog(null)}
          onSubmit={(name) => newFile(dialog.pane, name)}
        />
      )}
      {dialog?.kind === 'rename' && dialog.entry && (
        <PromptDialog
          title="Rename" label="New name" initial={dialog.entry.name} confirmLabel="Rename"
          onClose={() => setDialog(null)}
          onSubmit={(name) => run('rename', async () => {
            await dialog.pane.api.rename(joinPath(dialog.pane.path, dialog.entry.name), joinPath(dialog.pane.path, name))
            return `Renamed to ${name}.`
          }, [dialog.pane])}
        />
      )}
      {dialog?.kind === 'edit' && (
        <EditorDialog
          file={dialog.file} name={dialog.name} pane={dialog.pane} isNew={dialog.isNew}
          onClose={() => setDialog(null)}
          onSaved={(msg) => { setFlash({ tone: 'ok', text: msg }); dialog.pane.load() }}
          onError={(text) => setFlash({ tone: 'err', text })}
        />
      )}
      {dialog?.kind === 'props' && (
        <PropertiesDialog
          pane={dialog.pane}
          onClose={() => setDialog(null)}
          onApply={(fn, msg) => run('props', async () => { await fn(); return msg }, [dialog.pane])}
        />
      )}
    </div>,
    document.body,
  )
}

// Toolbar acts on the focused pane. Upload takes a file input and a drop on the
// listing, both landing in that pane's current directory.
function Toolbar({ pane, split, busy, dst, labelOf, onUp, onRefresh, onNewFolder, onNewFile, onRename, onProps, onDelete, onDownload, onUpload, onTransfer, onEdit, maxUpload, onError }) {
  const fileRef = useRef(null)
  const n = pane.selected.size
  const one = n === 1
  const any = n > 0
  // Only one regular file can go to the editor; a directory or a multi-select
  // has nothing to open.
  const editable = one && pane.selectedEntries[0]?.type === 'file'

  const pick = (fileList) => {
    const files = [...fileList].map((file) => ({ path: file.name, file }))
    if (files.length === 0) return
    const total = files.reduce((s, f) => s + f.file.size, 0)
    if (maxUpload > 0 && total > maxUpload) {
      onError(`That selection is larger than this instance's upload limit. An admin can raise it in Settings.`)
      return
    }
    onUpload(files)
  }

  return (
    <div className="flex shrink-0 flex-wrap items-center gap-1.5 border-b px-4 py-2">
      <Button variant="ghost" size="sm" onClick={onUp} disabled={pane.path === '/'}><Icon.ArrowLeft size={14} /> Up</Button>
      <Button variant="ghost" size="sm" onClick={onRefresh}>Refresh</Button>
      <span className="mx-1 h-5 w-px bg-border" />
      <Button variant="ghost" size="sm" onClick={onNewFolder}>New folder</Button>
      <Button variant="ghost" size="sm" onClick={onNewFile}>New file</Button>
      <Button variant="ghost" size="sm" onClick={() => fileRef.current?.click()} disabled={!!busy}>
        {busy === 'upload' ? 'Uploading…' : 'Upload'}
      </Button>
      <input ref={fileRef} type="file" multiple className="hidden" onChange={(e) => { pick(e.target.files); e.target.value = '' }} />
      <Button variant="ghost" size="sm" onClick={onDownload} disabled={!any}>Download</Button>
      <span className="mx-1 h-5 w-px bg-border" />
      <Button variant="ghost" size="sm" onClick={onEdit} disabled={!editable}>Edit</Button>
      <Button variant="ghost" size="sm" onClick={onRename} disabled={!one}>Rename</Button>
      <Button variant="ghost" size="sm" onClick={onProps} disabled={!any}>Permissions…</Button>
      <Button variant="ghost" size="sm" onClick={onDelete} disabled={!any || !!busy}>
        <span className="text-danger">{busy === 'delete' ? 'Deleting…' : 'Delete'}</span>
      </Button>
      {split && (
        <>
          <span className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" onClick={onTransfer} disabled={!any || !!busy}>
            {busy === 'transfer' ? 'Copying…' : `Copy to ${labelOf(dst.nodeId)}`}
          </Button>
        </>
      )}
    </div>
  )
}

// Pane is one node + one directory. Clicking the header's node picker swaps
// which node this side is looking at.
function Pane({ pane, nodes, active, onFocus, split, onAction }) {
  const { path, entries, selected, setSelected, loading, error } = pane
  const [lastIndex, setLastIndex] = useState(null)
  const [menu, setMenu] = useState(null) // { x, y, entry }

  // Range-select with shift, toggle with ctrl/meta, plain click replaces —
  // the selection model every file manager has, so muscle memory carries over.
  const onRowClick = (e, entry, i) => {
    onFocus()
    setSelected((prev) => {
      const next = new Set(prev)
      if (e.shiftKey && lastIndex !== null) {
        const [lo, hi] = lastIndex < i ? [lastIndex, i] : [i, lastIndex]
        for (let k = lo; k <= hi; k++) next.add(entries[k].name)
        return next
      }
      if (e.ctrlKey || e.metaKey) {
        next.has(entry.name) ? next.delete(entry.name) : next.add(entry.name)
        return next
      }
      return new Set([entry.name])
    })
    setLastIndex(i)
  }

  // Double-click follows a directory, and opens a file in the editor — the two
  // things "open" means, per row type.
  const openEntry = (entry) => {
    if (entry.type === 'dir' || entry.type === 'link') return pane.load(joinPath(path, entry.name))
    onAction('edit', entry)
  }

  // Right-click selects the row it lands on (unless it is already part of a
  // multi-selection, which would be a surprising thing to discard) and opens
  // the per-row menu.
  const onRowContext = (e, entry, i) => {
    e.preventDefault()
    e.stopPropagation()
    onFocus()
    if (!selected.has(entry.name)) {
      setSelected(new Set([entry.name]))
      setLastIndex(i)
    }
    setMenu({ x: e.clientX, y: e.clientY, entry })
  }

  // Breadcrumb segments, so any ancestor is one click away.
  const crumbs = path === '/' ? [] : path.split('/').filter(Boolean)

  return (
    <div
      className={`flex min-w-0 flex-1 flex-col ${active && split ? 'bg-primary/[0.03]' : ''}`}
      onMouseDown={onFocus}
    >
      <div className="flex shrink-0 items-center gap-2 border-b px-3 py-1.5">
        {/* Fixed-width wrapper: inputCls is w-full, which beats any width class
            set on the control itself. */}
        <div className="w-36 shrink-0">
          <select
            className={`${inputCls} h-7 py-0 text-xs`}
            value={pane.nodeId}
            onChange={(e) => pane.setNodeId(e.target.value)}
          >
            {nodes.map((n) => <option key={n.id} value={n.id}>{n.label}</option>)}
          </select>
        </div>
        <div className="flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto whitespace-nowrap text-xs">
          <button className="rounded px-1 py-0.5 font-mono hover:bg-surface2" onClick={() => pane.load('/')}>/</button>
          {crumbs.map((c, i) => (
            <span key={i} className="flex items-center">
              <button
                className="rounded px-1 py-0.5 font-mono hover:bg-surface2"
                onClick={() => pane.load('/' + crumbs.slice(0, i + 1).join('/'))}
              >{c}</button>
              {i < crumbs.length - 1 && <span className="text-muted">/</span>}
            </span>
          ))}
        </div>
        {active && split && <span className="shrink-0 rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-medium text-primary">active</span>}
      </div>

      <div className="min-h-0 flex-1 overflow-auto">
        {error && <div className="m-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{error}</div>}
        {loading && <div className="p-4 text-xs text-muted">Reading {path}…</div>}
        {!loading && !error && entries.length === 0 && <div className="p-4 text-xs text-muted">This directory is empty.</div>}
        {!loading && entries.length > 0 && (
          <table className="w-full text-xs">
            <thead className="sticky top-0 bg-surface text-[10px] uppercase tracking-wide text-muted">
              <tr>
                <th className="w-full px-3 py-1.5 text-left font-medium">Name</th>
                <th className="whitespace-nowrap px-2 py-1.5 text-right font-medium">Size</th>
                <th className="whitespace-nowrap px-2 py-1.5 text-left font-medium">Permissions</th>
                <th className="whitespace-nowrap px-2 py-1.5 text-left font-medium">Owner</th>
                <th className="whitespace-nowrap px-2 py-1.5 text-left font-medium">Group</th>
                <th className="whitespace-nowrap px-3 py-1.5 text-left font-medium">Modified</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e, i) => {
                const on = selected.has(e.name)
                return (
                  <tr
                    key={e.name}
                    onClick={(ev) => onRowClick(ev, e, i)}
                    onDoubleClick={() => openEntry(e)}
                    onContextMenu={(ev) => onRowContext(ev, e, i)}
                    className={`cursor-default select-none border-t border-border/40 ${on ? 'bg-primary/15' : 'hover:bg-surface2'}`}
                  >
                    <td className="max-w-0 px-3 py-1">
                      <div className="flex items-center gap-1.5">
                        <span className={`shrink-0 ${e.type === 'dir' ? 'text-primary' : 'text-muted'}`}>
                          {(Icon[ICON_FOR[e.type]] || Icon.File)({ size: 13 })}
                        </span>
                        <span className="truncate">{e.name}</span>
                        {e.target && <span className="truncate text-muted">→ {e.target}</span>}
                      </div>
                    </td>
                    <td className="whitespace-nowrap px-2 py-1 text-right tabular-nums text-muted">{fmtSize(e.size, e.type)}</td>
                    <td className="whitespace-nowrap px-2 py-1 font-mono text-muted">{e.perms} <span className="opacity-60">{e.mode}</span></td>
                    <td className="whitespace-nowrap px-2 py-1 text-muted">{e.user}</td>
                    <td className="whitespace-nowrap px-2 py-1 text-muted">{e.group}</td>
                    <td className="whitespace-nowrap px-3 py-1 tabular-nums text-muted">{fmtTime(e.modTime)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>

      {menu && (
        <RowMenu
          menu={menu} count={selected.size}
          onClose={() => setMenu(null)}
          onPick={(action) => { setMenu(null); onAction(action, menu.entry) }}
        />
      )}
    </div>
  )
}

// RowMenu is the per-row right-click menu. Which entries appear depends on the
// row: only a regular file can be opened in the editor, and only a directory
// can be entered.
function RowMenu({ menu, count, onClose, onPick }) {
  const { entry } = menu
  const one = count <= 1
  const items = []
  if (entry.type === 'dir' || entry.type === 'link') items.push(['open', 'Open'])
  if (entry.type === 'file') items.push(['edit', 'Edit…'])
  items.push(['download', count > 1 ? `Download ${count} items` : 'Download'])
  if (one) items.push(['rename', 'Rename…'])
  items.push(['props', 'Permissions…'])
  items.push([null, null])
  items.push(['delete', count > 1 ? `Delete ${count} items` : 'Delete'])

  const x = Math.max(8, Math.min(menu.x, window.innerWidth - 200))
  const y = Math.max(8, Math.min(menu.y, window.innerHeight - 240))
  return createPortal(
    <div className="fixed inset-0 z-[70]" onMouseDown={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div className="absolute w-48 rounded-lg border bg-surface p-1 shadow-xl" style={{ left: x, top: y }} onMouseDown={(e) => e.stopPropagation()}>
        {items.map(([action, label], i) => (action === null ? (
          <div key={i} className="my-1 h-px bg-border" />
        ) : (
          <button
            key={action}
            onClick={() => onPick(action)}
            className={`block w-full rounded-md px-2.5 py-1.5 text-left text-xs hover:bg-surface2 ${action === 'delete' ? 'text-danger' : 'text-fg'}`}
          >{label}</button>
        )))}
      </div>
    </div>,
    document.body,
  )
}

// PropertiesDialog edits mode and ownership for the selection.
//
// The mode is edited both as a 3×3 grid of checkboxes and as the octal it
// produces, kept in sync, because the two ways of thinking about permissions
// suit different moments — "make it group-writable" versus "make it 0644".
function PropertiesDialog({ pane, onClose, onApply }) {
  const sel = pane.selectedEntries
  const many = sel.length > 1
  const first = sel[0] || {}
  const anyDir = sel.some((e) => e.type === 'dir')

  const [mode, setMode] = useState(() => (many ? '' : (first.mode || '0644')).replace(/^0+(?=\d{3})/, '') || '644')
  const [owner, setOwner] = useState(many ? '' : first.user || '')
  const [group, setGroup] = useState(many ? '' : first.group || '')
  const [recursive, setRecursive] = useState(false)
  const [ids, setIds] = useState({ users: [], groups: [] })
  const [err, setErr] = useState('')

  useEffect(() => { pane.api.identities().then(setIds).catch(() => { /* free text still works */ }) }, [pane.api])

  const octal = (mode.match(/^[0-7]{3,4}$/) ? mode : '').padStart(4, '0')
  const bits = octal ? parseInt(octal, 8) : 0
  const has = (b) => (bits & b) !== 0
  const toggle = (b) => {
    if (!octal) { setErr('Switch to a numeric mode to use the grid.'); return }
    setErr('')
    setMode(((bits ^ b) & 0o7777).toString(8).padStart(3, '0'))
  }

  const CLASSES = [['Owner', 6], ['Group', 3], ['Other', 0]]
  const PERMS = [['r', 4], ['w', 2], ['x', 1]]

  const apply = async () => {
    setErr('')
    const paths = pane.selectedPaths
    const what = `${paths.length} item${paths.length === 1 ? '' : 's'}${recursive ? ', recursively' : ''}`
    const modeChanged = mode.trim() !== '' && (many || mode.replace(/^0+/, '') !== (first.mode || '').replace(/^0+/, ''))
    const ownerChanged = owner.trim() !== '' && (many || owner !== first.user)
    const groupChanged = group.trim() !== '' && (many || group !== first.group)
    if (!modeChanged && !ownerChanged && !groupChanged) { setErr('Nothing to change.'); return }
    onClose()
    onApply(async () => {
      if (modeChanged) await pane.api.chmod(paths, mode.trim(), recursive)
      if (ownerChanged || groupChanged) {
        await pane.api.chown(paths, ownerChanged ? owner.trim() : '', groupChanged ? group.trim() : '', recursive)
      }
    }, `Updated ${what}.`)
  }

  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-md rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="text-sm font-semibold">Permissions & ownership</h3>
        <p className="mb-4 mt-0.5 truncate text-xs text-muted">
          {many ? `${sel.length} selected items` : `${first.name} · ${first.perms || ''}`}
        </p>

        <div className="mb-3">
          <div className="mb-1.5 text-xs font-medium">Mode</div>
          <table className="mb-2 text-xs">
            <thead>
              <tr className="text-[10px] uppercase text-muted">
                <th className="pr-3 text-left font-medium"> </th>
                {PERMS.map(([p]) => <th key={p} className="px-2 font-medium">{p}</th>)}
              </tr>
            </thead>
            <tbody>
              {CLASSES.map(([label, shift]) => (
                <tr key={label}>
                  <td className="pr-3 text-muted">{label}</td>
                  {PERMS.map(([p, bit]) => (
                    <td key={p} className="px-2 text-center">
                      <input type="checkbox" checked={has(bit << shift)} onChange={() => toggle(bit << shift)} />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
          <div className="flex items-center gap-2">
            <input
              className={`${inputCls} w-32 font-mono`} value={mode}
              placeholder={many ? 'leave blank' : ''}
              onChange={(e) => { setMode(e.target.value); setErr('') }}
            />
            <span className="text-[11px] text-muted">octal (644) or symbolic (u+x)</span>
          </div>
        </div>

        <div className="mb-3 grid grid-cols-2 gap-3">
          <IdentityField label="Owner" value={owner} onChange={setOwner} options={ids.users} many={many} />
          <IdentityField label="Group" value={group} onChange={setGroup} options={ids.groups} many={many} />
        </div>

        <label className={`mb-1 flex items-start gap-2 text-xs ${anyDir ? '' : 'opacity-50'}`}>
          <input type="checkbox" className="mt-0.5" checked={recursive} disabled={!anyDir} onChange={(e) => setRecursive(e.target.checked)} />
          <span>
            Apply to everything inside selected directories
            {!anyDir && <span className="block text-muted">No directory is selected.</span>}
          </span>
        </label>

        {err && <div className="mt-2 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}

        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button size="sm" onClick={apply}>Apply</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// IdentityField is a free-text box backed by the node's own users/groups, so a
// name that exists is one pick away and a uid nobody named is still typeable.
function IdentityField({ label, value, onChange, options, many }) {
  const listId = `fs-${label.toLowerCase()}-options`
  return (
    <div>
      <div className="mb-1 text-xs font-medium">{label}</div>
      <input
        className={inputCls} list={listId} value={value}
        placeholder={many ? 'leave blank to keep' : ''}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id={listId}>
        {options.map((o) => <option key={o.name} value={o.name}>{`${o.name} (${o.id})`}</option>)}
      </datalist>
    </div>
  )
}

function PromptDialog({ title, label, initial = '', confirmLabel, onClose, onSubmit }) {
  const [v, setV] = useState(initial)
  const ref = useRef(null)
  useEffect(() => { ref.current?.focus(); ref.current?.select() }, [])
  const go = () => { const t = v.trim(); if (!t) return; onClose(); onSubmit(t) }
  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-3 text-sm font-semibold">{title}</h3>
        <div className="mb-1 text-xs font-medium">{label}</div>
        <input
          ref={ref} className={inputCls} value={v}
          onChange={(e) => setV(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') go(); if (e.key === 'Escape') onClose() }}
        />
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button size="sm" onClick={go}>{confirmLabel}</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

function ConfirmDialog({ title, body, danger, confirmLabel, onConfirm, onClose }) {
  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">{title}</h3>
        <p className="mb-4 text-xs text-muted">{body}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onClose}>Cancel</Button>
          <Button size="sm" variant={danger ? 'danger' : undefined} onClick={() => { onClose(); onConfirm() }}>{confirmLabel}</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// EditorDialog is the notepad: the file's text, and a Save that puts it back
// with the mode and ownership it arrived with.
//
// Three things it is careful about, all learned from what an in-place editor
// normally gets wrong:
//   - It saves the mode/uid/gid it was handed, so editing a config does not
//     quietly turn it into a root-owned 0644 file and break the service that
//     cared who owned it.
//   - It refuses to overwrite a file that changed on the node since it opened
//     (someone else's console, or a daemon rewriting its own config), rather
//     than silently winning.
//   - Closing with unsaved changes asks first.
export function EditorDialog({ file, name, pane, isNew, onClose, onSaved, onError }) {
  const [text, setText] = useState(file.content)
  const [modTime, setModTime] = useState(file.modTime)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')
  const [confirmClose, setConfirmClose] = useState(false)
  const [saved, setSaved] = useState(false)
  const ref = useRef(null)
  const edited = text !== file.content
  // A file that does not exist yet is always savable — otherwise there would be
  // no way to create an empty one — but it is only "unsaved work" once typed in.
  const dirty = edited || (isNew && !saved)
  const canSave = edited || (isNew && !saved)

  useEffect(() => { ref.current?.focus() }, [])

  const save = async () => {
    setSaving(true)
    setErr('')
    try {
      const r = await pane.api.write({
        path: file.path, content: text,
        mode: file.mode, uid: file.uid, gid: file.gid,
        ifModTime: modTime,
      })
      // Adopt the new mtime so a second save in the same session is not
      // mistaken for a stale one.
      setModTime(r.modTime || 0)
      setSaved(true)
      onSaved(`${isNew ? 'Created' : 'Saved'} ${name} (${r.bytes} bytes).`)
      onClose()
    } catch (e) {
      setErr(e.message)
    } finally {
      setSaving(false)
    }
  }

  // Only actual typing is worth a confirmation: an untouched New file dialog has
  // nothing to lose, even though it counts as "dirty" for the Create button.
  const tryClose = () => (edited ? setConfirmClose(true) : onClose())

  // Ctrl/Cmd-S saves, Escape closes — what anyone will try first.
  const onKeyDown = (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') { e.preventDefault(); if (!saving) save() }
    if (e.key === 'Escape') { e.preventDefault(); tryClose() }
  }

  const lines = text === '' ? 0 : text.split('\n').length

  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/50 p-4" onMouseDown={tryClose}>
      <div
        className="flex h-[min(80vh,760px)] w-full max-w-3xl flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={onKeyDown}
      >
        <header className="flex shrink-0 items-center gap-2 border-b px-4 py-2.5">
          <Icon.File size={15} />
          <h3 className="truncate text-sm font-semibold">{name}</h3>
          {dirty && (
            <span className="shrink-0 rounded bg-warning/15 px-1.5 py-0.5 text-[10px] font-medium text-warning">
              {isNew && !edited ? 'not created yet' : 'unsaved'}
            </span>
          )}
          <span className="ml-auto shrink-0 font-mono text-[11px] text-muted">{file.path}</span>
        </header>

        <textarea
          ref={ref}
          spellCheck={false}
          className="min-h-0 flex-1 resize-none bg-bg px-4 py-3 font-mono text-xs leading-relaxed text-fg outline-none"
          value={text}
          onChange={(e) => { setText(e.target.value); setErr('') }}
        />

        {err && <div className="shrink-0 border-t border-danger/30 bg-danger/15 px-4 py-2 text-xs text-danger">{err}</div>}

        <footer className="flex shrink-0 items-center gap-3 border-t px-4 py-2.5 text-[11px] text-muted">
          <span>{lines} line{lines === 1 ? '' : 's'} · {new Blob([text]).size} bytes</span>
          {/* Named so it is obvious a save keeps them, not resets them. */}
          <span className="font-mono">
            mode {file.mode} · uid {file.uid} · gid {file.gid} {isNew && !saved ? '(on create)' : '(preserved on save)'}
          </span>
          <div className="ml-auto flex gap-2">
            <Button variant="ghost" size="sm" onClick={tryClose}>Close</Button>
            <Button size="sm" onClick={save} disabled={saving || !canSave}>{saving ? 'Saving…' : isNew ? 'Create' : 'Save'}</Button>
          </div>
        </footer>
      </div>

      {confirmClose && (
        <ConfirmDialog
          title="Discard changes?"
          body={isNew
            ? `${name} has not been created yet. Closing loses what you typed.`
            : `${name} has unsaved edits. Closing loses them.`}
          danger confirmLabel="Discard"
          onClose={() => setConfirmClose(false)}
          onConfirm={onClose}
        />
      )}
    </div>,
    document.body,
  )
}
