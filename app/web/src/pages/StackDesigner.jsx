import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { stackApi, TTL_OPTIONS, DEPLOY_TONE } from '../lib/stackApi.js'
import IntranetManager from './IntranetManager.jsx'
import PMMManager from './PMMManager.jsx'
import PXCManager from './PXCManager.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import {
  PORTS, dist, portPoint, edgePath, screenToWorld, zoomAt,
} from '../lib/canvas.js'

const NODE_W = 212
const NODE_H = 104
const SNAP = 26

// Architecture options (must match images built by `make images`).
const ARCH_OPTIONS = [
  { id: 'amd64', label: 'amd64 (x86-64)' },
  { id: 'arm64', label: 'arm64 (aarch64)' },
]

// Node-type catalog.
const NODE_TYPES = {
  intranet: {
    label: 'Intranet',
    sub: 'Squid Proxy · DNS · Mail · OpenLDAP · CA',
    color: '#6366f1',
    icon: 'Server',
    singleton: true,
    ports: false, // self-contained; no connection endpoints
    osOptions: [{ id: 'oel9', label: 'Oracle Linux 9' }],
  },
  pmm: {
    label: 'PMM3',
    slug: 'pmm',
    sub: 'Percona Monitoring & Management',
    color: '#0ea5e9',
    icon: 'Monitor',
    singleton: false,
    ports: false,
    osOptions: [{ id: 'pmm', label: 'percona/pmm-server' }],
    defaults: { version: '', adminPassword: '', generateCert: false },
  },
  // PXC nodes live inside a PXC cluster frame (not added from the toolbar
  // directly); this entry only supplies the color/icon used to render them.
  pxc: {
    label: 'PXC Node',
    slug: 'pxc',
    sub: 'Percona XtraDB Cluster',
    color: '#a855f7',
    icon: 'Database',
  },
}

// ---------------------------------------------------------- PXC cluster frames
const PXC_NODE_W = 116
const PXC_NODE_H = 78
const FRAME_TITLE = 32
const FRAME_PAD = 14
const FRAME_GAP = 12
const FRAME_COLOR = '#a855f7'

// layoutFrame derives a frame's size and lays its member nodes out in a row.
function layoutFrame(frame, frameNodes) {
  const n = Math.max(1, frameNodes.length)
  const w = FRAME_PAD * 2 + n * PXC_NODE_W + (n - 1) * FRAME_GAP
  const h = FRAME_TITLE + FRAME_PAD * 2 + PXC_NODE_H
  const positioned = frameNodes.map((nd, i) => ({
    ...nd,
    x: frame.x + FRAME_PAD + i * (PXC_NODE_W + FRAME_GAP),
    y: frame.y + FRAME_TITLE + FRAME_PAD,
  }))
  return { frame: { ...frame, w, h }, nodes: positioned }
}

// nextClusterName → pxc-cluster-NN, unique across all PXC frames (from 00).
function nextClusterName(frames) {
  let max = -1
  for (const f of frames) {
    const m = (f.label || '').match(/^pxc-cluster-(\d+)$/)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `pxc-cluster-${String(max + 1).padStart(2, '0')}`
}

// nextPXCName → lowest pxcNN not already used by any PXC node in the stack.
function nextPXCName(usedSet) {
  for (let i = 1; ; i++) {
    const name = `pxc${String(i).padStart(2, '0')}`
    if (!usedSet.has(name)) return name
  }
}

const osLabel = (type, os) => (NODE_TYPES[type]?.osOptions.find((o) => o.id === os)?.label) || os

// Auto-numbered per-type labels: a non-singleton node is named "<slug>-NN" with
// NN zero-padded from 01 and increasing per node type (pmm-01, pmm-02, …, and in
// future psmysql-01, psmysql-02, …). These labels become the node hostnames in
// the Intranet DNS / FQDNs. The Intranet singleton keeps its plain label.
function nextLabel(type, nodes) {
  const def = NODE_TYPES[type]
  if (def.singleton) return def.label
  const base = def.slug || type
  const re = new RegExp(`^${base}-(\\d+)$`)
  let max = 0
  for (const n of nodes) {
    if (n.type !== type) continue
    const m = (n.label || '').match(re)
    if (m) max = Math.max(max, parseInt(m[1], 10))
  }
  return `${base}-${String(max + 1).padStart(2, '0')}`
}

// Small SVG progress ring (upper-right of a provisioning node).
function ProgressRing({ percent = 0, size = 24 }) {
  const r = (size - 5) / 2
  const c = 2 * Math.PI * r
  const off = c * (1 - Math.max(0, Math.min(100, percent)) / 100)
  const k = size / 2
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} title={`${percent}%`}>
      <circle cx={k} cy={k} r={r} fill="var(--surface)" stroke="var(--surface2)" strokeWidth="2.5" />
      <circle cx={k} cy={k} r={r} fill="none" stroke="var(--warning)" strokeWidth="2.5" strokeLinecap="round"
        strokeDasharray={c} strokeDashoffset={off} transform={`rotate(-90 ${k} ${k})`} />
    </svg>
  )
}

const STATUS_TONE = { draft: 'muted', deployed: 'success', expired: 'danger' }

export default function StackDesigner() {
  const [stacks, setStacks] = useState([])
  const [openId, setOpenId] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setError('')
    try {
      setStacks(await stackApi.list())
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  if (openId != null) {
    return <StackEditor stackId={openId} onBack={() => { setOpenId(null); load() }} />
  }

  return (
    <StackList
      stacks={stacks}
      loading={loading}
      error={error}
      onOpen={setOpenId}
      onCreated={(s) => setOpenId(s.id)}
      onChanged={load}
    />
  )
}

// ---------------------------------------------------------------- list view

function ttlLabel(id) {
  return TTL_OPTIONS.find((t) => t.id === id)?.label ?? id
}

function expiresIn(iso) {
  if (!iso) return 'never expires'
  const ms = new Date(iso) - new Date()
  if (ms <= 0) return 'expired'
  const h = Math.floor(ms / 3.6e6)
  if (h >= 24) return `expires in ${Math.floor(h / 24)}d`
  if (h >= 1) return `expires in ${h}h`
  return `expires in ${Math.max(1, Math.floor(ms / 6e4))}m`
}

function StackList({ stacks, loading, error, onOpen, onCreated, onChanged }) {
  const [showNew, setShowNew] = useState(false)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Database Stacks</h2>
          <p className="text-sm text-muted">Design, deploy, and manage container stacks.</p>
        </div>
        <Button onClick={() => setShowNew(true)}>
          <Icon.Plus size={16} /> New stack
        </Button>
      </div>

      {error && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}

      {loading ? (
        <div className="py-10 text-center text-muted">Loading…</div>
      ) : stacks.length === 0 ? (
        <Card>
          <div className="py-10 text-center text-muted">
            No stacks yet. Create one to start designing.
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {stacks.map((s) => (
            <Card key={s.id} className="transition hover:border-primary">
              <div className="flex items-start justify-between gap-2">
                <button onClick={() => onOpen(s.id)} className="min-w-0 text-left">
                  <div className="truncate text-sm font-semibold text-fg">{s.name}</div>
                  <div className="mt-0.5 text-xs text-muted">{expiresIn(s.expiresAt)}</div>
                </button>
                <Badge tone={STATUS_TONE[s.status] || 'muted'}>{s.status}</Badge>
              </div>
              <div className="mt-3 flex items-center justify-between">
                <Badge tone="primary">{ttlLabel(s.ttl)}</Badge>
                <div className="flex gap-1">
                  <Button size="sm" variant="outline" onClick={() => onOpen(s.id)}>Open</Button>
                  <ConfirmButton
                    size="sm"
                    variant="ghost"
                    title="Delete stack (tears down containers)"
                    confirmLabel="Delete?"
                    onConfirm={async () => { await stackApi.remove(s.id); onChanged() }}
                  >
                    <Icon.Trash size={16} />
                  </ConfirmButton>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}

      {showNew && (
        <NewStackModal
          onClose={() => setShowNew(false)}
          onCreated={(s) => { setShowNew(false); onCreated(s) }}
        />
      )}
    </div>
  )
}

function NewStackModal({ onClose, onCreated }) {
  const [name, setName] = useState('')
  const [ttl, setTtl] = useState('24h')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const s = await stackApi.create(name.trim() || 'Untitled stack', ttl)
      onCreated(s)
    } catch (err) {
      setError(err.message)
      setBusy(false)
    }
  }

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-4 text-sm font-semibold">New stack</h3>
        {error && <div className="mb-3 rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>}
        <form onSubmit={submit} className="space-y-3">
          <Field label="Name">
            <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} placeholder="My database stack" autoFocus />
          </Field>
          <Field label="Lifetime" hint="The stack and its containers are torn down when this elapses.">
            <select className={inputCls} value={ttl} onChange={(e) => setTtl(e.target.value)}>
              {TTL_OPTIONS.map((t) => (
                <option key={t.id} value={t.id}>{t.label}</option>
              ))}
            </select>
          </Field>
          <div className="flex justify-end gap-2 pt-1">
            <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
            <Button type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create'}</Button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// -------------------------------------------------------------- editor view

function StackEditor({ stackId, onBack }) {
  const [stack, setStack] = useState(null)
  const [error, setError] = useState('')
  const [nodes, setNodes] = useState([])
  const [edges, setEdges] = useState([])
  const [frames, setFrames] = useState([])
  const [view, setView] = useState({ x: 40, y: 20, z: 1 })
  const [selected, setSelected] = useState(null)
  const [menu, setMenu] = useState(null)
  const [connect, setConnect] = useState(null)
  const [saveState, setSaveState] = useState('saved') // saved | saving
  const [deployments, setDeployments] = useState([])
  const [issues, setIssues] = useState(null) // validate results panel
  const [busy, setBusy] = useState('') // 'validate' | 'deploy' | ''
  const [configNode, setConfigNode] = useState(null) // node whose profile is shown
  const [deployPanel, setDeployPanel] = useState('hidden') // 'open' | 'min' | 'hidden'
  const { openTerminal } = useTerminals()

  const wrapRef = useRef(null)
  const dragRef = useRef(null)
  const counter = useRef(0)
  const uid = (p) => `${p}-${Date.now().toString(36)}-${++counter.current}`

  const refs = useRef({})
  refs.current = { nodes, edges, frames, view }
  const stackRef = useRef(null)
  stackRef.current = stack
  const lastSaved = useRef('')

  // load
  useEffect(() => {
    let alive = true
    stackApi.get(stackId).then((s) => {
      if (!alive) return
      setStack(s)
      setDeployments(s.deployments || [])
      const d = s.design || {}
      const nz = d.nodes || []
      const ez = d.edges || []
      const fz = d.frames || []
      const vw = d.view || { x: 40, y: 20, z: 1 }
      setNodes(nz)
      setEdges(ez)
      setFrames(fz)
      setView(vw)
      lastSaved.current = JSON.stringify({ nodes: nz, edges: ez, frames: fz, view: vw })
    }).catch((err) => setError(err.message))
    return () => { alive = false }
  }, [stackId])

  // poll deployment state (does NOT touch the local design while editing)
  useEffect(() => {
    const t = setInterval(async () => {
      try {
        const s = await stackApi.get(stackId)
        setDeployments(s.deployments || [])
        setStack((prev) => (prev ? { ...prev, status: s.status } : prev))
      } catch {
        // ignore transient errors
      }
    }, 3000)
    return () => clearInterval(t)
  }, [stackId])

  const depByNode = {}
  for (const d of deployments) depByNode[d.nodeId] = d

  // auto-open the deployment console while anything is provisioning, but never
  // override the user's minimized choice.
  useEffect(() => {
    if (deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')) {
      setDeployPanel((p) => (p === 'hidden' ? 'open' : p))
    }
  }, [deployments])

  // debounced autosave — only when the design actually differs from the last
  // saved snapshot (so the 3s status poll never triggers a save).
  useEffect(() => {
    if (!stackRef.current) return
    const cur = JSON.stringify({ nodes, edges, frames, view })
    if (cur === lastSaved.current) return
    setSaveState('saving')
    const t = setTimeout(async () => {
      try {
        await stackApi.update(stackRef.current.id, stackRef.current.name, { nodes, edges, frames, view })
        lastSaved.current = cur
      } catch { /* keep dirty; will retry on next change */ }
      setSaveState('saved')
    }, 600)
    return () => clearTimeout(t)
  }, [nodes, edges, frames, view])

  const getWorld = useCallback((cx, cy) => {
    const rect = wrapRef.current.getBoundingClientRect()
    return screenToWorld(rect, refs.current.view, cx, cy)
  }, [])

  const rectOf = useCallback((id) => {
    const n = refs.current.nodes.find((x) => x.id === id)
    return n ? { x: n.x, y: n.y, w: NODE_W, h: NODE_H } : null
  }, [])

  function hitPort(world, excludeId) {
    let best = null
    let bestD = SNAP
    for (const n of refs.current.nodes) {
      if (n.id === excludeId) continue
      const r = { x: n.x, y: n.y, w: NODE_W, h: NODE_H }
      for (const port of PORTS) {
        const d = dist(world, portPoint(r, port))
        if (d < bestD) { bestD = d; best = { id: n.id, port } }
      }
    }
    return best
  }

  // global pointer move/up
  useEffect(() => {
    function onMove(e) {
      const d = dragRef.current
      if (!d) return
      if (d.kind === 'pan') {
        setView((v) => ({ ...v, x: d.ox + (e.clientX - d.sx), y: d.oy + (e.clientY - d.sy) }))
        return
      }
      const w = getWorld(e.clientX, e.clientY)
      if (d.kind === 'node') {
        setNodes((ns) => ns.map((n) => (n.id === d.id ? { ...n, x: w.x + d.offx, y: w.y + d.offy } : n)))
      } else if (d.kind === 'frame') {
        const nx = w.x + d.offx, ny = w.y + d.offy
        const frame = refs.current.frames.find((f) => f.id === d.id)
        setFrames((fs) => fs.map((f) => (f.id === d.id ? { ...f, x: nx, y: ny } : f)))
        if (frame) {
          const mine = refs.current.nodes.filter((n) => n.frameId === d.id)
          const laid = new Map(layoutFrame({ ...frame, x: nx, y: ny }, mine).nodes.map((n) => [n.id, n]))
          setNodes((ns) => ns.map((n) => laid.get(n.id) || n))
        }
      } else if (d.kind === 'connect') {
        const tgt = hitPort(w, d.fromId)
        const src = portPoint(rectOf(d.fromId), d.fromPort)
        const to = tgt ? portPoint(rectOf(tgt.id), tgt.port) : w
        d.lastTarget = tgt
        setConnect({ from: src, to, targetId: tgt?.id ?? null, targetPort: tgt?.port ?? null })
      }
    }
    function onUp() {
      const d = dragRef.current
      if (d?.kind === 'connect') {
        const t = d.lastTarget
        if (t && t.id !== d.fromId) {
          const dup = refs.current.edges.some(
            (ed) =>
              (ed.from.node === d.fromId && ed.from.port === d.fromPort && ed.to.node === t.id && ed.to.port === t.port) ||
              (ed.from.node === t.id && ed.from.port === t.port && ed.to.node === d.fromId && ed.to.port === d.fromPort),
          )
          if (!dup) {
            const id = uid('e')
            setEdges((es) => [...es, { id, from: { node: d.fromId, port: d.fromPort }, to: { node: t.id, port: t.port }, type: 'directional' }])
            setSelected({ kind: 'edge', id })
          }
        }
      }
      dragRef.current = null
      setConnect(null)
    }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => {
      removeEventListener('pointermove', onMove)
      removeEventListener('pointerup', onUp)
    }
  }, [getWorld, rectOf])

  // wheel zoom
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    function onWheel(e) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      setView((v) => zoomAt(v, e.clientX - rect.left, e.clientY - rect.top, e.deltaY))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [])

  // delete key
  useEffect(() => {
    function onKey(e) {
      if (e.key === 'Escape') setMenu(null)
      if (e.key !== 'Delete' && e.key !== 'Backspace') return
      const t = e.target
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return
      if (selected) { e.preventDefault(); deleteSelected() }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  })

  // interactions
  function startPan(e) {
    if (e.button !== 0) return
    setSelected(null)
    setMenu(null)
    dragRef.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, ox: view.x, oy: view.y }
  }
  function startNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const n = nodes.find((x) => x.id === id)
    dragRef.current = { kind: 'node', id, offx: n.x - w.x, offy: n.y - w.y }
  }
  function startFrame(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'frame', id })
    setMenu(null)
    const w = getWorld(e.clientX, e.clientY)
    const f = frames.find((x) => x.id === id)
    dragRef.current = { kind: 'frame', id, offx: f.x - w.x, offy: f.y - w.y }
  }
  function selectFrameNode(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu(null)
  }
  function startConnect(e, ownerId, port) {
    if (e.button !== 0) return
    e.stopPropagation()
    setMenu(null)
    const src = portPoint(rectOf(ownerId), port)
    dragRef.current = { kind: 'connect', fromId: ownerId, fromPort: port, lastTarget: null }
    setConnect({ from: src, to: src, targetId: null, targetPort: null })
  }
  function openMenu(e, id) {
    e.preventDefault()
    e.stopPropagation()
    setSelected({ kind: 'node', id })
    setMenu({ x: e.clientX, y: e.clientY, id })
  }

  // mutations
  const patchNode = (id, patch) => setNodes((ns) => ns.map((n) => (n.id === id ? { ...n, ...patch } : n)))
  const patchFrame = (id, patch) => setFrames((fs) => fs.map((f) => (f.id === id ? { ...f, ...patch } : f)))
  function deleteNode(id) {
    setNodes((ns) => ns.filter((n) => n.id !== id))
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? null : s))
  }
  function deleteEdge(id) {
    setEdges((es) => es.filter((e) => e.id !== id))
    setSelected((s) => (s?.kind === 'edge' && s.id === id ? null : s))
  }
  function deleteFrame(id) {
    setNodes((ns) => ns.filter((n) => n.frameId !== id))
    setFrames((fs) => fs.filter((f) => f.id !== id))
    setSelected((s) => (s && (s.id === id) ? null : s))
  }
  function deleteSelected() {
    if (selected?.kind === 'node') {
      const n = nodes.find((x) => x.id === selected.id)
      if (n?.frameId) removePXCNodeById(n.frameId, n.id)
      else deleteNode(selected.id)
    } else if (selected?.kind === 'edge') deleteEdge(selected.id)
    else if (selected?.kind === 'frame') deleteFrame(selected.id)
  }

  // --- PXC cluster frame operations ---
  // Re-lay a frame's member nodes (positions derive from the frame geometry).
  function relayout(frameId, framesArr, nodesArr) {
    const frame = framesArr.find((f) => f.id === frameId)
    if (!frame) return { frames: framesArr, nodes: nodesArr }
    const mine = nodesArr.filter((n) => n.frameId === frameId)
    const others = nodesArr.filter((n) => n.frameId !== frameId)
    const r = layoutFrame(frame, mine)
    return {
      frames: framesArr.map((f) => (f.id === frameId ? r.frame : f)),
      nodes: [...others, ...r.nodes],
    }
  }
  function addPXCCluster() {
    if (!nodes.some((n) => n.type === 'intranet')) return
    const fid = uid('frame')
    const fx = (-view.x + 200) / view.z
    const fy = (-view.y + 200) / view.z
    const frame = {
      id: fid, type: 'pxc', label: nextClusterName(frames), x: fx, y: fy, w: 0, h: 0,
      os: 'oraclelinux', osVersion: '9', arch: 'amd64', pxcMajor: '8.0', pxcVersion: '',
      rootPassword: '', pmmNodeId: '', useProxy: false, gtid: true,
      generateCert: false, certTtlValue: 365, certTtlUnit: 'days',
    }
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const newNodes = []
    for (let i = 0; i < 3; i++) {
      const name = nextPXCName(used)
      used.add(name)
      newNodes.push({ id: uid('pxc'), type: 'pxc', label: name, frameId: fid, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 })
    }
    const r = relayout(fid, [...frames, frame], [...nodes, ...newNodes])
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected({ kind: 'frame', id: fid })
  }
  function addPXCNode(frameId) {
    const used = new Set(nodes.filter((n) => n.type === 'pxc').map((n) => n.label))
    const name = nextPXCName(used)
    const newNode = { id: uid('pxc'), type: 'pxc', label: name, frameId, role: 'regular', exportEnabled: false, exportHostPort: 0, x: 0, y: 0 }
    const r = relayout(frameId, frames, [...nodes, newNode])
    setFrames(r.frames)
    setNodes(r.nodes)
  }
  function removePXCNode(frameId) {
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    removePXCNodeById(frameId, mine[mine.length - 1].id)
  }
  function removePXCNodeById(frameId, id) {
    const mine = nodes.filter((n) => n.frameId === frameId)
    if (mine.length <= 1) return // keep at least one node
    const r = relayout(frameId, frames, nodes.filter((n) => n.id !== id))
    setFrames(r.frames)
    setNodes(r.nodes)
    setSelected((s) => (s?.kind === 'node' && s.id === id ? { kind: 'frame', id: frameId } : s))
  }
  function addNode(type) {
    const def = NODE_TYPES[type]
    if (def.singleton && nodes.some((n) => n.type === type)) return
    // The Intranet is required first — it provides DNS/mail/LDAP/CA for the stack.
    if (type !== 'intranet' && !nodes.some((n) => n.type === 'intranet')) return
    const id = uid(type)
    const x = (-view.x + 220) / view.z
    const y = (-view.y + 160) / view.z
    setNodes((ns) => [...ns, { id, type, x, y, label: nextLabel(type, ns), os: def.osOptions[0].id, arch: 'amd64', ...(def.defaults || {}) }])
    setSelected({ kind: 'node', id })
  }

  const upsertDep = (ds, d) => {
    const next = ds.filter((x) => x.nodeId !== d.nodeId)
    next.push(d)
    return next
  }

  async function runValidate() {
    setBusy('validate')
    try {
      const r = await stackApi.validate(stack.id)
      setIssues(r.issues || [])
    } catch (err) {
      setIssues([{ level: 'error', message: err.message }])
    } finally {
      setBusy('')
    }
  }

  async function runDeploy() {
    setBusy('deploy')
    setIssues(null)
    try {
      const v = await stackApi.validate(stack.id)
      if (!v.ok) {
        setIssues(v.issues)
        return
      }
      const r = await stackApi.deploy(stack.id)
      setDeployments(r.deployments || [])
      setStack((p) => ({ ...p, status: 'deployed' }))
      setDeployPanel('open')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function runDestroy() {
    setBusy('destroy')
    setIssues(null)
    try {
      await stackApi.destroy(stack.id)
      setDeployments([])
      setStack((p) => ({ ...p, status: 'draft' }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy('')
    }
  }

  async function nodeAction(nid, action) {
    try {
      const d = await stackApi.nodeAction(stack.id, nid, action)
      setDeployments((ds) => upsertDep(ds, d))
    } catch (err) {
      setError(err.message)
    }
  }

  async function showConfig(nid) {
    try {
      setConfigNode(await stackApi.getNode(stack.id, nid))
    } catch (err) {
      setError(err.message)
    }
  }

  function nodeMenuActions(id) {
    const dep = depByNode[id]
    const actions = []
    if (dep) {
      actions.push({ label: 'View config / profile', fn: () => showConfig(id) })
      if (dep.state === 'running') {
        const node = nodes.find((n) => n.id === id)
        actions.push({ label: 'Enter root console', fn: () => openTerminal({ stackId: stack.id, nodeId: id, title: `${node?.label || 'node'} · root` }) })
        actions.push({ label: 'Stop', fn: () => nodeAction(id, 'stop') })
        actions.push({ label: 'Restart', fn: () => nodeAction(id, 'restart') })
      } else if (dep.state === 'stopped' || dep.state === 'error') {
        actions.push({ label: 'Start', fn: () => nodeAction(id, 'start') })
      }
      actions.push({ sep: true })
    }
    actions.push({ label: 'Delete node', danger: true, fn: () => deleteNode(id) })
    return actions
  }

  if (error) {
    return (
      <div className="space-y-3">
        <Button variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Back</Button>
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{error}</div>
      </div>
    )
  }
  if (!stack) return <div className="py-10 text-center text-muted">Loading…</div>

  const hasIntranet = nodes.some((n) => n.type === 'intranet')

  return (
    <div className="flex h-[78vh] gap-4">
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        {/* toolbar */}
        <div className="flex flex-wrap items-center gap-2 rounded-xl border bg-surface px-3 py-2">
          <Button size="sm" variant="ghost" onClick={onBack}><Icon.ArrowLeft size={16} /> Stacks</Button>
          <div className="mx-1 h-5 w-px bg-border" />
          <span className="text-sm font-semibold">{stack.name}</span>
          <Badge tone="primary">{ttlLabel(stack.ttl)}</Badge>
          <Badge tone={STATUS_TONE[stack.status] || 'muted'}>{stack.status}</Badge>
          <div className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" disabled={hasIntranet} onClick={() => addNode('intranet')}>
            <Icon.Plus size={16} /> Intranet
          </Button>
          <Button size="sm" disabled={!hasIntranet} title={hasIntranet ? '' : 'Add an Intranet node first'} onClick={() => addNode('pmm')}>
            <Icon.Plus size={16} /> PMM3
          </Button>
          <Button size="sm" disabled={!hasIntranet} title={hasIntranet ? '' : 'Add an Intranet node first'} onClick={addPXCCluster}>
            <Icon.Plus size={16} /> PXC Cluster
          </Button>
          <div className="mx-1 h-5 w-px bg-border" />
          <Button size="sm" variant="outline" disabled={!!busy} onClick={runValidate}>
            <Icon.Check size={15} /> {busy === 'validate' ? 'Validating…' : 'Validate'}
          </Button>
          <Button size="sm" disabled={!!busy || nodes.length === 0} onClick={runDeploy}>
            <Icon.Arrow size={15} /> {busy === 'deploy' ? 'Deploying…' : 'Deploy'}
          </Button>
          {(deployments.length > 0 || stack.status === 'deployed') && (
            <ConfirmButton size="sm" variant="outline" disabled={!!busy} confirmLabel="Destroy — sure?" onConfirm={runDestroy}>
              <Icon.Trash size={15} /> {busy === 'destroy' ? 'Destroying…' : 'Destroy'}
            </ConfirmButton>
          )}
          <div className="ml-auto flex items-center gap-3">
            <span className="text-xs text-muted">{saveState === 'saving' ? 'Saving…' : 'Saved'}</span>
            <span className="text-xs text-muted">{nodes.length} nodes · {edges.length} links</span>
            <Button size="sm" variant="ghost" onClick={() => setView({ x: 40, y: 20, z: 1 })}>
              <Icon.Move size={15} /> Reset view
            </Button>
          </div>
        </div>

        {issues && (
          <div className="rounded-xl border bg-surface p-3">
            <div className="mb-2 flex items-center justify-between">
              <span className="text-xs font-semibold text-muted">Validation</span>
              <button onClick={() => setIssues(null)} className="text-xs text-muted hover:text-fg">dismiss</button>
            </div>
            <ul className="space-y-1">
              {issues.map((it, i) => (
                <li key={i} className="flex items-center gap-2 text-sm">
                  <Badge tone={it.level === 'error' ? 'danger' : it.level === 'warning' ? 'warning' : 'success'}>{it.level}</Badge>
                  <span className="text-fg">{it.message}</span>
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* canvas */}
        <div
          ref={wrapRef}
          onPointerDown={startPan}
          onContextMenu={(e) => { e.preventDefault(); setMenu(null) }}
          className="relative flex-1 overflow-hidden rounded-xl border bg-bg"
          style={{ touchAction: 'none' }}
        >
          <div
            className="pointer-events-none absolute inset-0"
            style={{
              backgroundImage: 'radial-gradient(var(--grid) 1.4px, transparent 1.4px)',
              backgroundSize: `${24 * view.z}px ${24 * view.z}px`,
              backgroundPosition: `${view.x}px ${view.y}px`,
            }}
          />
          <div className="absolute left-0 top-0 origin-top-left" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}>
            <svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width="1" height="1">
              <defs>
                <marker id="stk-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                  <path d="M0,0 L10,5 L0,10 z" fill="context-stroke" />
                </marker>
              </defs>
              {edges.map((ed) => {
                const r0 = rectOf(ed.from.node)
                const r1 = rectOf(ed.to.node)
                if (!r0 || !r1) return null
                const d = edgePath(portPoint(r0, ed.from.port), ed.from.port, portPoint(r1, ed.to.port), ed.to.port)
                const on = selected?.kind === 'edge' && selected.id === ed.id
                return (
                  <g key={ed.id}>
                    <path d={d} fill="none" stroke="transparent" strokeWidth="16" className="pointer-events-auto cursor-pointer"
                      onPointerDown={(e) => { e.stopPropagation(); setSelected({ kind: 'edge', id: ed.id }) }} />
                    <path d={d} fill="none" stroke={on ? 'var(--primary)' : 'var(--muted)'} strokeWidth={on ? 3 : 2} markerEnd="url(#stk-arrow)" />
                  </g>
                )
              })}
              {connect && (
                <path d={edgePath(connect.from, 'right', connect.to, 'left')} fill="none" stroke="var(--primary)" strokeWidth="2" strokeDasharray="6 5" />
              )}
            </svg>

            {/* PXC cluster frames (rendered behind nodes, with their member nodes) */}
            {frames.map((f) => {
              const fdef = NODE_TYPES[f.type] || {}
              const on = selected?.kind === 'frame' && selected.id === f.id
              const kids = nodes.filter((n) => n.frameId === f.id)
              return (
                <div key={f.id} className="absolute" style={{ left: f.x, top: f.y, width: f.w, height: f.h }}>
                  <div className="absolute inset-0 rounded-xl border-2 border-dashed"
                    style={{ borderColor: on ? 'var(--primary)' : FRAME_COLOR, background: `color-mix(in srgb, ${FRAME_COLOR} 7%, transparent)` }} />
                  <div
                    onPointerDown={(e) => startFrame(e, f.id)}
                    onContextMenu={(e) => { e.preventDefault(); e.stopPropagation(); setSelected({ kind: 'frame', id: f.id }) }}
                    className="absolute inset-x-0 top-0 flex cursor-grab items-center gap-2 rounded-t-xl px-2 active:cursor-grabbing"
                    style={{ height: FRAME_TITLE, background: `color-mix(in srgb, ${FRAME_COLOR} 18%, transparent)` }}
                  >
                    <span style={{ color: FRAME_COLOR }}>{(Icon[fdef.icon] || Icon.Database)({ size: 15 })}</span>
                    <span className="truncate text-xs font-semibold text-fg">{f.label}</span>
                    <span className="text-[10px] text-muted">{kids.length} node{kids.length === 1 ? '' : 's'}</span>
                    <div className="ml-auto flex items-center gap-0.5">
                      <button title="Add PXC node" onPointerDown={(e) => e.stopPropagation()} onClick={() => addPXCNode(f.id)}
                        className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg">+</button>
                      <button title="Remove a node" onPointerDown={(e) => e.stopPropagation()} onClick={() => removePXCNode(f.id)}
                        className="rounded px-1.5 text-sm leading-none text-muted hover:bg-surface hover:text-fg">−</button>
                    </div>
                  </div>
                  {kids.map((n) => {
                    const non = selected?.kind === 'node' && selected.id === n.id
                    const dep = depByNode[n.id]
                    const arb = n.role === 'arbitrator'
                    return (
                      <div key={n.id}
                        onPointerDown={(e) => selectFrameNode(e, n.id)}
                        className={`absolute flex cursor-pointer flex-col overflow-hidden rounded-lg border bg-surface shadow-sm ${non ? 'ring-2 ring-primary' : ''}`}
                        style={{ left: n.x - f.x, top: n.y - f.y, width: PXC_NODE_W, height: PXC_NODE_H }}
                      >
                        <div className="h-1 w-full shrink-0" style={{ background: arb ? '#64748b' : FRAME_COLOR }} />
                        <div className="flex flex-1 flex-col justify-center px-2 py-1">
                          <div className="flex items-center gap-1">
                            <span className="min-w-0 flex-1 truncate text-xs font-semibold text-fg">{n.label}</span>
                            {dep?.state === 'provisioning' ? (
                              <ProgressRing percent={dep.progress?.percent || 0} size={15} />
                            ) : dep ? (
                              <span className="h-2 w-2 shrink-0 rounded-full" title={dep.state}
                                style={{ background: `var(--${DEPLOY_TONE[dep.state] === 'success' ? 'success' : dep.state === 'error' ? 'danger' : 'warning'})` }} />
                            ) : null}
                          </div>
                          <div className="mt-0.5 text-[10px] text-muted">{arb ? 'arbitrator' : 'regular'}</div>
                          {n.exportEnabled && <div className="text-[9px] font-medium text-primary">⇅ export</div>}
                        </div>
                      </div>
                    )
                  })}
                </div>
              )
            })}

            {nodes.filter((n) => !n.frameId).map((n) => {
              const def = NODE_TYPES[n.type] || NODE_TYPES.intranet
              const on = selected?.kind === 'node' && selected.id === n.id
              return (
                <div
                  key={n.id}
                  onPointerDown={(e) => startNode(e, n.id)}
                  onContextMenu={(e) => openMenu(e, n.id)}
                  className={`group absolute flex cursor-grab flex-col overflow-hidden rounded-xl border bg-surface shadow-sm active:cursor-grabbing ${on ? 'ring-2 ring-primary' : ''}`}
                  style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H }}
                >
                  <div className="h-1.5 w-full shrink-0" style={{ background: def.color }} />
                  <div className="flex flex-1 flex-col justify-center px-3 py-2">
                    <div className="flex items-start gap-2.5">
                      <span className="mt-0.5 shrink-0" style={{ color: def.color }}>
                        {(Icon[def.icon] || Icon.Server)({ size: 22 })}
                      </span>
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-2">
                          <div className="min-w-0 flex-1 truncate text-sm font-semibold text-fg">{n.label}</div>
                          <span className="shrink-0">
                            {depByNode[n.id]?.state === 'provisioning' ? (
                              <ProgressRing percent={depByNode[n.id].progress?.percent || 0} size={20} />
                            ) : depByNode[n.id] ? (
                              <Badge tone={DEPLOY_TONE[depByNode[n.id].state] || 'muted'}>{depByNode[n.id].state}</Badge>
                            ) : null}
                          </span>
                        </div>
                        <div className="mt-0.5 text-[11px] leading-snug text-muted">{def.sub}</div>
                        <div className="mt-1 text-[11px] font-medium text-fg/80">{osLabel(n.type, n.os)} · {n.arch || 'amd64'}</div>
                      </div>
                    </div>
                  </div>
                  {def.ports && (
                    <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                  )}
                </div>
              )
            })}
          </div>

          <div className="pointer-events-none absolute bottom-3 left-3 rounded-lg border bg-surface/80 px-3 py-2 text-xs text-muted backdrop-blur">
            Drag canvas to pan · scroll to zoom · drag a port to connect · right-click for actions
          </div>

          <Minimap nodes={nodes} view={view} setView={setView} wrapRef={wrapRef} selectedId={selected?.kind === 'node' ? selected.id : null} />
        </div>
      </div>

      <StackProperties
        selected={selected}
        stackId={stack.id}
        nodes={nodes}
        edges={edges}
        frames={frames}
        depByNode={depByNode}
        patchNode={patchNode}
        patchFrame={patchFrame}
        deleteNode={deleteNode}
        deleteEdge={deleteEdge}
        deleteFrame={deleteFrame}
      />

      {menu && (
        <ContextMenu menu={menu} onClose={() => setMenu(null)} actions={nodeMenuActions(menu.id)} />
      )}

      {configNode && <ConfigModal dep={configNode} onClose={() => setConfigNode(null)} />}

      {deployPanel === 'open' && (
        <DeploymentConsole deployments={deployments} nodes={nodes} onMinimize={() => setDeployPanel('min')} />
      )}
      {deployPanel === 'min' && createPortal(
        <button
          onClick={() => setDeployPanel('open')}
          className="fixed bottom-3 left-3 z-40 flex items-center gap-2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg hover:bg-surface2"
        >
          <Icon.Arrow size={16} /> Deployment
          {deployments.some((d) => d.state === 'pending' || d.state === 'provisioning') && (
            <span className="h-2 w-2 animate-pulse rounded-full bg-warning" />
          )}
        </button>,
        document.body,
      )}
    </div>
  )
}

const DEPLOY_KEY = 'dbcanvas-deploy-layout'
function loadDeployLayout() {
  try { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 }, ...JSON.parse(localStorage.getItem(DEPLOY_KEY) || '{}') } }
  catch { return { docked: true, height: 280, float: { x: 120, y: 120, w: 640, h: 360 } } }
}

function DeploymentConsole({ deployments, nodes, onMinimize }) {
  const [layout, setLayout] = useState(loadDeployLayout)
  const drag = useRef(null)
  useEffect(() => { try { localStorage.setItem(DEPLOY_KEY, JSON.stringify(layout)) } catch { /* */ } }, [layout])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'height') setLayout((l) => ({ ...l, height: Math.min(Math.max(160, d.h0 + (d.y0 - e.clientY)), window.innerHeight - 80) }))
      else if (d.kind === 'move') setLayout((l) => ({ ...l, float: { ...l.float, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) } }))
      else if (d.kind === 'wh') setLayout((l) => ({ ...l, float: { ...l.float, w: Math.max(360, d.w0 + (e.clientX - d.x0)), h: Math.max(200, d.h0 + (e.clientY - d.y0)) } }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const provisioning = deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')
  const failed = deployments.filter((d) => d.state === 'error')
  const done = !provisioning && deployments.length > 0
  const label = (nid) => nodes.find((n) => n.id === nid)?.label || nid

  const detached = !layout.docked
  const style = detached
    ? { position: 'fixed', left: layout.float.x, top: layout.float.y, width: layout.float.w, height: layout.float.h }
    : { position: 'fixed', left: 0, right: 0, bottom: 0, height: layout.height }

  return createPortal(
    <div className="z-40 flex flex-col border bg-surface shadow-2xl" style={style}>
      {!detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'height', y0: e.clientY, h0: layout.height } }}
          className="h-1.5 w-full cursor-ns-resize bg-border/60 hover:bg-primary" />
      )}
      <div
        className="flex items-center gap-2 border-b bg-surface2 px-3 py-1.5"
        onPointerDown={detached ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: layout.float.x, fy: layout.float.y } } : undefined}
        style={detached ? { cursor: 'move' } : undefined}
      >
        <span className="text-sm font-semibold">Deployment</span>
        {provisioning ? (
          <Badge tone="warning">provisioning…</Badge>
        ) : done ? (
          failed.length
            ? <Badge tone="danger">completed with errors — {failed.length} of {deployments.length} failed</Badge>
            : <Badge tone="success">deployment complete</Badge>
        ) : null}
        <div className="ml-auto flex items-center gap-1">
          <button title={detached ? 'Dock' : 'Detach'} onClick={() => setLayout((l) => ({ ...l, docked: !l.docked }))}
            className="rounded p-1 text-muted hover:bg-surface hover:text-fg"><Icon.Frame size={14} /></button>
          <button title="Minimize" onClick={onMinimize} className="rounded px-1.5 text-muted hover:bg-surface hover:text-fg">—</button>
        </div>
      </div>
      <div className="flex-1 space-y-3 overflow-auto p-3">
        {deployments.length === 0 && <div className="text-sm text-muted">No nodes deployed.</div>}
        {deployments.map((d) => {
          const p = d.progress || {}
          return (
            <div key={d.nodeId} className="rounded-lg border bg-bg p-2">
              <div className="mb-1 flex items-center gap-2 text-sm">
                <span className="font-medium">{label(d.nodeId)}</span>
                <Badge tone={DEPLOY_TONE[d.state] || 'muted'}>{d.state}</Badge>
                <span className="ml-auto text-xs text-muted">{p.phase || ''}</span>
              </div>
              <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface2">
                <div className={`h-full transition-all ${d.state === 'error' ? 'bg-danger' : d.state === 'running' ? 'bg-success' : 'bg-warning'}`} style={{ width: `${p.percent || 0}%` }} />
              </div>
              {p.message && <div className={`mt-1 text-xs ${d.state === 'error' ? 'text-danger' : 'text-muted'}`}>{p.message}</div>}
              {Array.isArray(p.log) && p.log.length > 0 && (
                <pre className="mt-1.5 max-h-32 overflow-auto whitespace-pre-wrap break-all rounded bg-surface2 p-1.5 text-[11px] leading-tight text-muted">{p.log.slice(-12).join('\n')}</pre>
              )}
            </div>
          )
        })}
      </div>
      {detached && (
        <div onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: layout.float.w, h0: layout.float.h } }}
          className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted">
          <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
        </div>
      )}
    </div>,
    document.body,
  )
}

function ConfigModal({ dep, onClose }) {
  let cfg = {}
  try { cfg = typeof dep.config === 'string' ? JSON.parse(dep.config) : dep.config || {} } catch { cfg = {} }
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="w-full max-w-md rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">Node profile</h3>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
        <dl className="space-y-1.5 text-sm">
          <Row k="FQDN" v={cfg.fqdn} />
          <Row k="Domain" v={cfg.domain} />
          <Row k="Base DN" v={cfg.baseDN} />
          <Row k="LDAP admin" v={cfg.ldapAdminDN} />
          <Row k="OS / arch" v={cfg.os ? `${cfg.os} · ${cfg.arch || ''}` : ''} />
          <Row k="Network alias" v={cfg.alias} />
          <Row k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} />
        </dl>
        {Array.isArray(cfg.services) && (
          <div className="mt-3">
            <div className="mb-1 text-xs font-medium text-muted">Services</div>
            <div className="flex flex-wrap gap-1">
              {cfg.services.map((s) => <Badge key={s} tone="primary">{s}</Badge>)}
            </div>
          </div>
        )}
      </div>
    </div>,
    document.body,
  )
}

function Row({ k, v }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-muted">{k}</dt>
      <dd className="truncate font-mono text-xs text-fg">{v || '—'}</dd>
    </div>
  )
}

// PMMOptions renders the PMM-only node settings: minor-version picker (from the
// catalog produced by `make versions`), admin password (auto-generated when
// empty), and the Intranet-CA certificate toggle.
function PMMOptions({ n, patchNode, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.pmmCatalog().then((c) => { if (alive) setCat(c) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const versions = cat?.versions || []
  const defaultTag = cat?.defaultTag || '3'
  return (
    <>
      <Field
        label="PMM version"
        hint={deployed ? 'Locked — the node is deployed.' : `Default is the rolling latest (percona/pmm-server:${defaultTag}). Pick a minor version to pin it.`}
      >
        <select
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.version || ''}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { version: e.target.value })}
        >
          <option value="">latest ({defaultTag})</option>
          {versions.map((v) => (
            <option key={v} value={v}>{v}</option>
          ))}
        </select>
      </Field>
      <Field
        label="Admin password"
        hint={deployed ? 'Set at deploy time.' : 'Leave empty to auto-generate a strong password.'}
      >
        <input
          className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
          value={n.adminPassword || ''}
          disabled={deployed}
          placeholder="(auto-generate if empty)"
          onChange={(e) => patchNode(n.id, { adminPassword: e.target.value })}
        />
      </Field>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input
          type="checkbox"
          checked={!!n.generateCert}
          disabled={deployed}
          onChange={(e) => patchNode(n.id, { generateCert: e.target.checked })}
        />
        <span>Generate nginx certificate from Intranet CA</span>
      </label>
      {n.generateCert && !deployed && (
        <p className="text-xs text-muted">
          Requires an Intranet node in the stack. New certs are written to <span className="font-mono">/srv/nginx</span> at deploy.
        </p>
      )}
    </>
  )
}

// ------------------------------------------------------------- PXC cluster forms

// PXCFrameForm edits a PXC cluster frame: version/OS/platform, credentials,
// monitoring/proxy/GTID/TLS options, and shows quorum guidance.
function PXCFrameForm({ frame: f, nodes, frameNodes, patchFrame, deleteFrame, deployed }) {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.pxcCatalog().then((c) => { if (alive) setCat(c.images || []) }).catch(() => { /* keep defaults */ })
    return () => { alive = false }
  }, [])
  const imgs = cat || []
  const lock = deployed ? 'opacity-70' : ''

  const osFamilies = [...new Set(imgs.filter((i) => Object.values(i.versions || {}).some((a) => a.length)).map((i) => i.os))]
  const osVersions = [...new Set(imgs.filter((i) => i.os === f.os).map((i) => i.osVersion))]
  const archs = [...new Set(imgs.filter((i) => i.os === f.os && i.osVersion === f.osVersion).map((i) => i.arch))]
  const entry = imgs.find((i) => i.os === f.os && i.osVersion === f.osVersion && i.arch === f.arch)
  const majors = entry ? Object.keys(entry.versions || {}).filter((m) => (entry.versions[m] || []).length) : []
  const minors = (entry?.versions?.[f.pxcMajor]) || []

  const pmmNodes = nodes.filter((n) => n.type === 'pmm')
  const regulars = frameNodes.filter((n) => n.role !== 'arbitrator').length
  const total = frameNodes.length

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PXC Cluster</span>
        <Badge tone="primary">{total} node{total === 1 ? '' : 's'}</Badge>
      </div>

      <Field label="Cluster name" hint="Must be unique across the stack.">
        <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
      </Field>

      <div className="grid grid-cols-2 gap-2">
        <Field label="OS" hint={deployed ? 'Locked.' : ''}>
          <select className={`${inputCls} ${lock}`} value={f.os} disabled={deployed} onChange={(e) => patchFrame(f.id, { os: e.target.value })}>
            {osFamilies.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="OS version">
          <select className={`${inputCls} ${lock}`} value={f.osVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { osVersion: e.target.value })}>
            {osVersions.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="Platform / arch">
          <select className={`${inputCls} ${lock}`} value={f.arch} disabled={deployed} onChange={(e) => patchFrame(f.id, { arch: e.target.value })}>
            {archs.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
        <Field label="PXC major">
          <select className={`${inputCls} ${lock}`} value={f.pxcMajor} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcMajor: e.target.value, pxcVersion: '' })}>
            {majors.map((o) => <option key={o} value={o}>{o}</option>)}
          </select>
        </Field>
      </div>

      <Field label="PXC minor version" hint={deployed ? 'Locked.' : 'Newest first; default is the latest.'}>
        <select className={`${inputCls} ${lock}`} value={f.pxcVersion} disabled={deployed} onChange={(e) => patchFrame(f.id, { pxcVersion: e.target.value })}>
          <option value="">latest{minors[0] ? ` (${minors[0]})` : ''}</option>
          {minors.map((v) => <option key={v} value={v}>{v}</option>)}
        </select>
      </Field>

      <Field label="Root password" hint={deployed ? 'Set at deploy.' : 'Leave empty to auto-generate.'}>
        <input className={`${inputCls} ${lock}`} value={f.rootPassword || ''} disabled={deployed} placeholder="(auto-generate if empty)" onChange={(e) => patchFrame(f.id, { rootPassword: e.target.value })} />
      </Field>

      <Field label="Monitored by (PMM)" hint="Optional — registers the cluster with a PMM node.">
        <select className={inputCls} value={f.pmmNodeId || ''} onChange={(e) => patchFrame(f.id, { pmmNodeId: e.target.value })}>
          <option value="">none</option>
          {pmmNodes.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
        </select>
      </Field>

      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!f.useProxy} onChange={(e) => patchFrame(f.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for egress</span>
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.gtid !== false} onChange={(e) => patchFrame(f.id, { gtid: e.target.checked })} />
        <span>Enable GTID</span>
      </label>
      <label className={`flex items-center gap-2 text-sm ${deployed ? 'opacity-70' : ''}`}>
        <input type="checkbox" checked={!!f.generateCert} disabled={deployed} onChange={(e) => patchFrame(f.id, { generateCert: e.target.checked })} />
        <span>Generate per-node certificates from Intranet CA</span>
      </label>
      {f.generateCert && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted">Cert TTL</span>
          <input type="number" min="1" className={`${inputCls} w-20`} value={f.certTtlValue || 365} onChange={(e) => patchFrame(f.id, { certTtlValue: Number(e.target.value) })} />
          <select className={inputCls} value={f.certTtlUnit || 'days'} onChange={(e) => patchFrame(f.id, { certTtlUnit: e.target.value })}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
      )}

      {(regulars < 3 || total % 2 === 0) && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          {regulars < 3 && <div>For HA, use at least 3 regular nodes ({regulars} now).</div>}
          {total % 2 === 0 && <div>An odd number of nodes keeps quorum on a split network ({total} now).</div>}
        </div>
      )}
      {regulars === 0 && (
        <div className="rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">At least one regular (data) node is required.</div>
      )}

      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
        <Icon.Trash size={16} /> Delete cluster
      </Button>
    </div>
  )
}

// PXCNodeForm edits a single PXC cluster member: role and host port export.
function PXCNodeForm({ node: n, frame, nodes, patchNode, dep, deployed }) {
  return (
    <div className="space-y-3">
      {dep && (
        <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
          <span className="text-muted">Deployment</span>
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      )}
      <Field label="Node name" hint="Auto-assigned, unique across the stack.">
        <input className={`${inputCls} opacity-70`} value={n.label} readOnly />
      </Field>
      <Field label="Cluster"><input className={`${inputCls} opacity-70`} value={frame?.label || '—'} readOnly /></Field>
      <Field label="Role" hint={deployed ? 'Locked — the node is deployed.' : 'Arbitrator (garbd) votes for quorum but stores no data.'}>
        <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={n.role || 'regular'} disabled={deployed} onChange={(e) => patchNode(n.id, { role: e.target.value })}>
          <option value="regular">regular (data node)</option>
          <option value="arbitrator">arbitrator (garbd)</option>
        </select>
      </Field>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={!!n.exportEnabled} onChange={(e) => patchNode(n.id, { exportEnabled: e.target.checked })} />
        <span>Export DB port to the host</span>
      </label>
      {n.exportEnabled && (
        <Field label="Host port" hint="0 / empty = random unused port. Must not clash with another node.">
          <input type="number" min="0" max="65535" className={inputCls} value={n.exportHostPort || 0}
            onChange={(e) => patchNode(n.id, { exportHostPort: Number(e.target.value) })} />
        </Field>
      )}
    </div>
  )
}

// Minimap: a scaled overview of the canvas in the bottom-right corner showing
// every node (colored by type) and the current viewport. Click or drag inside it
// to recenter the main view on that point.
const MINI_W = 184
const MINI_H = 124
const MINI_PAD = 8

function Minimap({ nodes, view, setView, wrapRef, selectedId }) {
  const drag = useRef(false)
  const rect = wrapRef.current?.getBoundingClientRect()
  const vw = rect?.width || 800
  const vh = rect?.height || 600

  // Current viewport expressed in world coordinates.
  const viewWorld = { x: -view.x / view.z, y: -view.y / view.z, w: vw / view.z, h: vh / view.z }

  // Bounds over all nodes plus the viewport, so both are always visible.
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  for (const n of nodes) {
    minX = Math.min(minX, n.x); minY = Math.min(minY, n.y)
    maxX = Math.max(maxX, n.x + NODE_W); maxY = Math.max(maxY, n.y + NODE_H)
  }
  minX = Math.min(minX, viewWorld.x); minY = Math.min(minY, viewWorld.y)
  maxX = Math.max(maxX, viewWorld.x + viewWorld.w); maxY = Math.max(maxY, viewWorld.y + viewWorld.h)
  if (!isFinite(minX)) { minX = 0; minY = 0; maxX = 1; maxY = 1 }

  const bw = (maxX - minX) || 1
  const bh = (maxY - minY) || 1
  const scale = Math.min((MINI_W - 2 * MINI_PAD) / bw, (MINI_H - 2 * MINI_PAD) / bh)
  const ox = MINI_PAD + ((MINI_W - 2 * MINI_PAD) - bw * scale) / 2 - minX * scale
  const oy = MINI_PAD + ((MINI_H - 2 * MINI_PAD) - bh * scale) / 2 - minY * scale
  const tx = (x) => ox + x * scale
  const ty = (y) => oy + y * scale

  function recenter(e) {
    const r = e.currentTarget.getBoundingClientRect()
    const wx = (e.clientX - r.left - ox) / scale
    const wy = (e.clientY - r.top - oy) / scale
    setView((v) => ({ ...v, x: vw / 2 - wx * v.z, y: vh / 2 - wy * v.z }))
  }

  return (
    <div
      className="absolute bottom-3 right-3 overflow-hidden rounded-lg border bg-surface/90 shadow backdrop-blur"
      style={{ width: MINI_W, height: MINI_H }}
      title="Minimap — click or drag to navigate"
    >
      <svg
        width={MINI_W}
        height={MINI_H}
        className="cursor-pointer"
        style={{ touchAction: 'none' }}
        onPointerDown={(e) => { e.stopPropagation(); drag.current = true; recenter(e) }}
        onPointerMove={(e) => { if (drag.current) recenter(e) }}
        onPointerUp={() => { drag.current = false }}
        onPointerLeave={() => { drag.current = false }}
      >
        <rect
          x={tx(viewWorld.x)} y={ty(viewWorld.y)}
          width={viewWorld.w * scale} height={viewWorld.h * scale}
          fill="var(--primary)" fillOpacity="0.12" stroke="var(--primary)" strokeWidth="1"
        />
        {nodes.map((n) => {
          const def = NODE_TYPES[n.type] || {}
          const on = selectedId === n.id
          return (
            <rect
              key={n.id}
              x={tx(n.x)} y={ty(n.y)}
              width={Math.max(2, NODE_W * scale)} height={Math.max(2, NODE_H * scale)}
              rx="1"
              fill={def.color || 'var(--muted)'} fillOpacity={on ? 1 : 0.8}
              stroke={on ? 'var(--fg)' : 'none'} strokeWidth="1"
            />
          )
        })}
      </svg>
    </div>
  )
}

function PortHandles({ ownerId, connecting, snapPort, onStart }) {
  const pos = {
    top: '-top-2 left-1/2 -translate-x-1/2',
    right: '-right-2 top-1/2 -translate-y-1/2',
    bottom: '-bottom-2 left-1/2 -translate-x-1/2',
    left: '-left-2 top-1/2 -translate-y-1/2',
  }
  return (
    <>
      {PORTS.map((port) => {
        const snap = snapPort === port
        return (
          <button
            key={port}
            onPointerDown={(e) => onStart(e, ownerId, port)}
            className={`absolute h-3 w-3 rounded-full border-2 border-primary bg-surface transition ${pos[port]} ${connecting ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'} ${snap ? 'pulse-ring scale-150 bg-primary' : ''}`}
          />
        )
      })}
    </>
  )
}

function ContextMenu({ menu, onClose, actions }) {
  const x = Math.min(menu.x, window.innerWidth - 200)
  const y = Math.min(menu.y, window.innerHeight - 160)
  return createPortal(
    <div className="fixed inset-0 z-50" onClick={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div className="absolute w-52 rounded-lg border bg-surface p-1 shadow-xl" style={{ left: x, top: y }} onClick={(e) => e.stopPropagation()}>
        {actions.map((a, i) =>
          a.sep ? (
            <div key={i} className="my-1 h-px bg-border" />
          ) : (
            <button
              key={i}
              onClick={() => { a.fn(); onClose() }}
              className={`block w-full rounded-md px-2.5 py-1.5 text-left text-sm hover:bg-surface2 ${a.danger ? 'text-danger' : 'text-fg'}`}
            >
              {a.label}
            </button>
          ),
        )}
      </div>
    </div>,
    document.body,
  )
}

const PROPS_KEY = 'dbcanvas-props-layout'
function loadProps() {
  try { return JSON.parse(localStorage.getItem(PROPS_KEY) || '{}') } catch { return {} }
}

function StackProperties({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, deleteNode, deleteEdge, deleteFrame }) {
  const selNode = selected?.kind === 'node' ? nodes.find((n) => n.id === selected.id) : null
  const selDep = selNode ? depByNode[selNode.id] : null
  const wide = (selDep && selDep.state === 'running' && (selNode.type === 'intranet' || selNode.type === 'pmm' || selNode.type === 'pxc')) || selected?.kind === 'frame'

  const saved = useRef(loadProps()).current
  const [docked, setDocked] = useState(saved.docked !== false)
  const [width, setWidth] = useState(saved.width || 288)
  const [flt, setFlt] = useState(saved.float || { x: Math.max(20, (typeof window !== 'undefined' ? window.innerWidth : 1200) - 500), y: 96, w: 460, h: 540 })
  const drag = useRef(null)

  // give management tabs room when a running Intranet is selected (docked)
  useEffect(() => { if (wide && docked && width < 440) setWidth(440) }, [wide, docked, width])
  useEffect(() => { try { localStorage.setItem(PROPS_KEY, JSON.stringify({ docked, width, float: flt })) } catch { /* */ } }, [docked, width, flt])

  useEffect(() => {
    const onMove = (e) => {
      const d = drag.current
      if (!d) return
      if (d.kind === 'w') setWidth(Math.min(680, Math.max(260, d.w0 + (d.x0 - e.clientX))))
      else if (d.kind === 'move') setFlt((f) => ({ ...f, x: d.fx + (e.clientX - d.x0), y: d.fy + (e.clientY - d.y0) }))
      else if (d.kind === 'wh') setFlt((f) => ({ ...f, w: Math.max(280, d.w0 + (e.clientX - d.x0)), h: Math.max(220, d.h0 + (e.clientY - d.y0)) }))
    }
    const onUp = () => { drag.current = null }
    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => { removeEventListener('pointermove', onMove); removeEventListener('pointerup', onUp) }
  }, [])

  const Header = ({ move }) => (
    <div
      className="mb-3 flex items-center justify-between"
      onPointerDown={move ? (e) => { if (e.target.closest('button')) return; drag.current = { kind: 'move', x0: e.clientX, y0: e.clientY, fx: flt.x, fy: flt.y } } : undefined}
      style={move ? { cursor: 'move' } : undefined}
    >
      <h3 className="text-sm font-semibold">Properties</h3>
      <button onClick={() => setDocked((d) => !d)} title={docked ? 'Detach' : 'Dock'} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
        <Icon.Frame size={14} />
      </button>
    </div>
  )
  const body = <Body selected={selected} stackId={stackId} nodes={nodes} edges={edges} frames={frames} depByNode={depByNode} patchNode={patchNode} patchFrame={patchFrame} deleteNode={deleteNode} deleteEdge={deleteEdge} deleteFrame={deleteFrame} />

  if (docked) {
    return (
      <div className="relative shrink-0" style={{ width }}>
        <div
          onPointerDown={(e) => { drag.current = { kind: 'w', x0: e.clientX, w0: width } }}
          className="absolute left-0 top-0 z-10 h-full w-1.5 -translate-x-1 cursor-ew-resize hover:bg-primary"
          title="Drag to resize"
        />
        <div className="h-full overflow-auto rounded-xl border bg-surface p-4">
          <Header move={false} />
          {body}
        </div>
      </div>
    )
  }
  return createPortal(
    <div className="fixed z-40 flex flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
      style={{ left: flt.x, top: flt.y, width: flt.w, height: flt.h }}>
      <div className="flex-1 overflow-auto p-4">
        <Header move />
        {body}
      </div>
      <div
        onPointerDown={(e) => { drag.current = { kind: 'wh', x0: e.clientX, y0: e.clientY, w0: flt.w, h0: flt.h } }}
        className="absolute bottom-0 right-0 h-4 w-4 cursor-nwse-resize text-muted"
      >
        <svg viewBox="0 0 10 10" className="h-full w-full"><path d="M9 1 L1 9 M9 5 L5 9" stroke="currentColor" fill="none" /></svg>
      </div>
    </div>,
    document.body,
  )
}

function Body({ selected, stackId, nodes, edges, frames, depByNode, patchNode, patchFrame, deleteNode, deleteEdge, deleteFrame }) {
  if (!selected) return <p className="text-sm text-muted">Select a node, link or PXC cluster to edit it. Add an Intranet node from the toolbar to begin.</p>

  if (selected.kind === 'frame') {
    const f = frames.find((x) => x.id === selected.id)
    if (!f) return null
    const frameNodes = nodes.filter((n) => n.frameId === f.id)
    const deployed = frameNodes.some((n) => depByNode[n.id])
    return <PXCFrameForm frame={f} nodes={nodes} frameNodes={frameNodes} patchFrame={patchFrame} deleteFrame={deleteFrame} deployed={deployed} />
  }

  if (selected.kind === 'node') {
    const n = nodes.find((x) => x.id === selected.id)
    if (!n) return null
    const dep = depByNode[n.id]
    const deployed = !!dep

    // PXC cluster member node.
    if (n.type === 'pxc') {
      if (dep && dep.state === 'running') {
        return <PXCManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
      }
      return <PXCNodeForm node={n} frame={frames.find((fr) => fr.id === n.frameId)} nodes={nodes} patchNode={patchNode} dep={dep} deployed={deployed} />
    }

    const def = NODE_TYPES[n.type] || NODE_TYPES.intranet

    // Deployed + running Intranet → full management console.
    if (dep && dep.state === 'running' && n.type === 'intranet') {
      return <IntranetManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    // Deployed + running PMM → PMM management console.
    if (dep && dep.state === 'running' && n.type === 'pmm') {
      return <PMMManager stackId={stackId} nodeId={n.id} dep={dep} onDeleteNode={() => deleteNode(n.id)} />
    }
    return (
      <div className="space-y-3">
        {dep && (
          <div className="flex items-center justify-between rounded-lg bg-surface2 px-3 py-2 text-sm">
            <span className="text-muted">Deployment</span>
            <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
          </div>
        )}
        <Field label="Label">
          <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
        </Field>
        <Field label="Type">
          <input className={`${inputCls} opacity-70`} value={def.label} readOnly />
        </Field>
        <Field label="Operating system" hint={deployed ? 'Locked — the node is deployed.' : 'Locked once the stack is deployed.'}>
          <select
            className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
            value={n.os}
            disabled={deployed}
            onChange={(e) => patchNode(n.id, { os: e.target.value })}
          >
            {def.osOptions.map((o) => (
              <option key={o.id} value={o.id}>{o.label}</option>
            ))}
          </select>
        </Field>
        <Field label="Architecture" hint={deployed ? 'Locked — the node is deployed.' : 'Must have a matching image built (make images).'}>
          <select
            className={`${inputCls} ${deployed ? 'opacity-70' : ''}`}
            value={n.arch || 'amd64'}
            disabled={deployed}
            onChange={(e) => patchNode(n.id, { arch: e.target.value })}
          >
            {ARCH_OPTIONS.map((o) => (
              <option key={o.id} value={o.id}>{o.label}</option>
            ))}
          </select>
        </Field>
        {n.type === 'pmm' && <PMMOptions n={n} patchNode={patchNode} deployed={deployed} />}
        <div className="grid grid-cols-2 gap-2">
          <Field label="X">
            <input type="number" className={inputCls} value={Math.round(n.x)} onChange={(e) => patchNode(n.id, { x: +e.target.value })} />
          </Field>
          <Field label="Y">
            <input type="number" className={inputCls} value={Math.round(n.y)} onChange={(e) => patchNode(n.id, { y: +e.target.value })} />
          </Field>
        </div>
        {!deployed && <p className="text-xs text-muted">Management tabs (LDAP, email, certificate, credentials, terminal) appear here after deploy.</p>}
        <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
          <Icon.Trash size={16} /> Delete node
        </Button>
      </div>
    )
  }

  const ed = edges.find((x) => x.id === selected.id)
  if (!ed) return null
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-sm">
        <span className="font-mono text-xs">{ed.from.node}.{ed.from.port}</span>
        <span className="mx-1 text-muted">→</span>
        <span className="font-mono text-xs">{ed.to.node}.{ed.to.port}</span>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete link
      </Button>
    </div>
  )
}
