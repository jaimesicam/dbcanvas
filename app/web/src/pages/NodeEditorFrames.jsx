import { useCallback, useEffect, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Button, Field, inputCls } from '../components/ui.jsx'
import { PORTS, PORT_DIR, clamp, dist, portPoint, edgePath } from '../lib/canvas.js'

// ---- constants ----
const NODE_W = 168
const NODE_H = 80
const PALETTE = ['#6366f1', '#0ea5e9', '#22c55e', '#f59e0b', '#ec4899', '#14b8a6']
const LINK_TYPES = [
  { id: 'directional', label: 'Directional', icon: 'Arrow' },
  { id: 'none', label: 'Plain line', icon: 'Line' },
  { id: 'bidirectional', label: 'Bidirectional', icon: 'Both' },
]
const SNAP = 26

// ---- geometry ----
function rectOf(id, nodes, frames) {
  const n = nodes.find((x) => x.id === id)
  if (n) return { x: n.x, y: n.y, w: NODE_W, h: NODE_H }
  const f = frames.find((x) => x.id === id)
  if (f) return { x: f.x, y: f.y, w: f.w, h: f.h }
  return null
}

function frameAt(cx, cy, frames) {
  // last (topmost) frame whose rect contains the point
  for (let i = frames.length - 1; i >= 0; i--) {
    const f = frames[i]
    if (cx >= f.x && cx <= f.x + f.w && cy >= f.y && cy <= f.y + f.h) return f.id
  }
  return null
}

function hitPort(world, excludeId, nodes, frames) {
  let best = null
  let bestD = SNAP
  const ends = [...nodes.map((n) => n.id), ...frames.map((f) => f.id)]
  for (const id of ends) {
    if (id === excludeId) continue
    const r = rectOf(id, nodes, frames)
    for (const port of PORTS) {
      const d = dist(world, portPoint(r, port))
      if (d < bestD) {
        bestD = d
        best = { id, port }
      }
    }
  }
  return best
}

// ---- seed ----
function seed() {
  const frames = [{ id: 'f1', x: 120, y: 120, w: 360, h: 260, label: 'Processing stage', color: '#6366f1' }]
  const nodes = [
    { id: 'n1', x: 150, y: 175, label: 'Ingest', sub: 'source feed', color: '#0ea5e9', frame: 'f1' },
    { id: 'n2', x: 150, y: 285, label: 'Transform', sub: 'normalize', color: '#22c55e', frame: 'f1' },
    { id: 'n3', x: 560, y: 160, label: 'Enrich', sub: 'lookup tables', color: '#f59e0b', frame: null },
    { id: 'n4', x: 560, y: 300, label: 'Sink', sub: 'warehouse', color: '#ec4899', frame: null },
  ]
  const edges = [
    { id: 'e1', from: { node: 'n1', port: 'right' }, to: { node: 'n3', port: 'left' }, type: 'directional' },
    { id: 'e2', from: { node: 'n2', port: 'right' }, to: { node: 'n4', port: 'left' }, type: 'directional' },
    { id: 'e3', from: { node: 'n1', port: 'bottom' }, to: { node: 'n2', port: 'top' }, type: 'none' },
    { id: 'e4', from: { node: 'f1', port: 'right' }, to: { node: 'n3', port: 'bottom' }, type: 'bidirectional' },
  ]
  return { frames, nodes, edges }
}

function sameEnd(ed, a, ap, b, bp) {
  return (
    (ed.from.node === a && ed.from.port === ap && ed.to.node === b && ed.to.port === bp) ||
    (ed.from.node === b && ed.from.port === bp && ed.to.node === a && ed.to.port === ap)
  )
}

export default function NodeEditorFrames() {
  const init = useRef(seed()).current
  const [nodes, setNodes] = useState(init.nodes)
  const [frames, setFrames] = useState(init.frames)
  const [edges, setEdges] = useState(init.edges)
  const [view, setView] = useState({ x: 40, y: 20, z: 1 })
  const [selected, setSelected] = useState(null) // {kind,id}
  const [menu, setMenu] = useState(null) // {x,y,kind,id}
  const [linkType, setLinkType] = useState('directional')
  const [dropFrame, setDropFrame] = useState(null)
  const [connect, setConnect] = useState(null) // {from:{x,y}, to:{x,y}, targetId, targetPort}

  const wrapRef = useRef(null)
  const dragRef = useRef(null)
  const counter = useRef(10)
  const uid = (p) => `${p}-${++counter.current}`

  // mirror state into refs for use inside global pointer handlers
  const refs = useRef({})
  refs.current = { nodes, frames, edges, view, linkType }

  const getWorld = useCallback((clientX, clientY) => {
    const rect = wrapRef.current.getBoundingClientRect()
    const { view } = refs.current
    return {
      x: (clientX - rect.left - view.x) / view.z,
      y: (clientY - rect.top - view.y) / view.z,
    }
  }, [])

  // ---- global pointer move/up ----
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
        const nx = w.x + d.offx
        const ny = w.y + d.offy
        setNodes((ns) => ns.map((n) => (n.id === d.id ? { ...n, x: nx, y: ny } : n)))
        setDropFrame(frameAt(nx + NODE_W / 2, ny + NODE_H / 2, refs.current.frames))
      } else if (d.kind === 'frame') {
        const fx = w.x + d.offx
        const fy = w.y + d.offy
        setFrames((fs) => fs.map((f) => (f.id === d.id ? { ...f, x: fx, y: fy } : f)))
        setNodes((ns) =>
          ns.map((n) => {
            const c = d.children[n.id]
            return c ? { ...n, x: fx + c.dx, y: fy + c.dy } : n
          }),
        )
      } else if (d.kind === 'resize') {
        setFrames((fs) =>
          fs.map((f) =>
            f.id === d.id ? { ...f, w: Math.max(160, w.x - f.x), h: Math.max(110, w.y - f.y) } : f,
          ),
        )
      } else if (d.kind === 'connect') {
        const tgt = hitPort(w, d.fromId, refs.current.nodes, refs.current.frames)
        const src = portPoint(rectOf(d.fromId, refs.current.nodes, refs.current.frames), d.fromPort)
        const to = tgt
          ? portPoint(rectOf(tgt.id, refs.current.nodes, refs.current.frames), tgt.port)
          : w
        setConnect({ from: src, to, targetId: tgt?.id ?? null, targetPort: tgt?.port ?? null })
      }
    }

    function onUp() {
      const d = dragRef.current
      if (d?.kind === 'connect') {
        const c = refs.current
        const tgt = hitPortFromConnect()
        if (tgt && tgt.id !== d.fromId) {
          const dup = c.edges.some((ed) => sameEnd(ed, d.fromId, d.fromPort, tgt.id, tgt.port))
          if (!dup) {
            const id = uid('e')
            setEdges((es) => [
              ...es,
              { id, from: { node: d.fromId, port: d.fromPort }, to: { node: tgt.id, port: tgt.port }, type: c.linkType },
            ])
            setSelected({ kind: 'edge', id })
          }
        }
      } else if (d?.kind === 'node') {
        const node = refs.current.nodes.find((n) => n.id === d.id)
        if (node) {
          const fid = frameAt(node.x + NODE_W / 2, node.y + NODE_H / 2, refs.current.frames)
          setNodes((ns) => ns.map((n) => (n.id === d.id ? { ...n, frame: fid } : n)))
        }
      }
      dragRef.current = null
      setDropFrame(null)
      setConnect(null)
    }

    // read latest snap target straight from the connect state captured on move
    function hitPortFromConnect() {
      const d = dragRef.current
      if (!d) return null
      return d.lastTarget ?? null
    }

    addEventListener('pointermove', onMove)
    addEventListener('pointerup', onUp)
    return () => {
      removeEventListener('pointermove', onMove)
      removeEventListener('pointerup', onUp)
    }
  }, [getWorld])

  // keep dragRef.lastTarget in sync so pointerup can read the final snap
  useEffect(() => {
    if (dragRef.current?.kind === 'connect') {
      dragRef.current.lastTarget = connect?.targetId ? { id: connect.targetId, port: connect.targetPort } : null
    }
  }, [connect])

  // ---- wheel zoom (cursor-anchored) ----
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    function onWheel(e) {
      e.preventDefault()
      const rect = el.getBoundingClientRect()
      const mx = e.clientX - rect.left
      const my = e.clientY - rect.top
      setView((v) => {
        const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1
        const z = clamp(v.z * factor, 0.35, 2.2)
        const wx = (mx - v.x) / v.z
        const wy = (my - v.y) / v.z
        return { x: mx - wx * z, y: my - wy * z, z }
      })
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [])

  // ---- keyboard delete ----
  useEffect(() => {
    function onKey(e) {
      if (e.key === 'Escape') setMenu(null)
      if (e.key !== 'Delete' && e.key !== 'Backspace') return
      const t = e.target
      if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return
      if (selected) {
        e.preventDefault()
        deleteSelected()
      }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }) // re-bind each render so `selected` is fresh

  // ---- interaction starters ----
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
    const children = {}
    nodes.forEach((n) => {
      if (n.frame === id) children[n.id] = { dx: n.x - f.x, dy: n.y - f.y }
    })
    dragRef.current = { kind: 'frame', id, offx: f.x - w.x, offy: f.y - w.y, children }
  }

  function startResize(e, id) {
    if (e.button !== 0) return
    e.stopPropagation()
    setSelected({ kind: 'frame', id })
    dragRef.current = { kind: 'resize', id }
  }

  function startConnect(e, ownerId, port) {
    if (e.button !== 0) return
    e.stopPropagation()
    setMenu(null)
    const src = portPoint(rectOf(ownerId, nodes, frames), port)
    dragRef.current = { kind: 'connect', fromId: ownerId, fromPort: port, lastTarget: null }
    setConnect({ from: src, to: src, targetId: null, targetPort: null })
  }

  function openMenu(e, kind, id) {
    e.preventDefault()
    e.stopPropagation()
    setSelected({ kind, id })
    setMenu({ x: e.clientX, y: e.clientY, kind, id })
  }

  // ---- mutations ----
  const patchNode = (id, patch) => setNodes((ns) => ns.map((n) => (n.id === id ? { ...n, ...patch } : n)))
  const patchFrame = (id, patch) => setFrames((fs) => fs.map((f) => (f.id === id ? { ...f, ...patch } : f)))
  const patchEdge = (id, patch) => setEdges((es) => es.map((ed) => (ed.id === id ? { ...ed, ...patch } : ed)))

  function deleteNode(id) {
    setNodes((ns) => ns.filter((n) => n.id !== id))
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'node' && s.id === id ? null : s))
  }
  function deleteFrame(id) {
    setNodes((ns) => ns.map((n) => (n.frame === id ? { ...n, frame: null } : n)))
    setFrames((fs) => fs.filter((f) => f.id !== id))
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
    setSelected((s) => (s?.kind === 'frame' && s.id === id ? null : s))
  }
  function deleteEdge(id) {
    setEdges((es) => es.filter((e) => e.id !== id))
    setSelected((s) => (s?.kind === 'edge' && s.id === id ? null : s))
  }
  function deleteSelected() {
    if (!selected) return
    if (selected.kind === 'node') deleteNode(selected.id)
    else if (selected.kind === 'frame') deleteFrame(selected.id)
    else if (selected.kind === 'edge') deleteEdge(selected.id)
  }
  function disconnect(id) {
    setEdges((es) => es.filter((e) => e.from.node !== id && e.to.node !== id))
  }

  function addNode(at) {
    const id = uid('n')
    const x = at ? at.x : (-view.x + 200) / view.z
    const y = at ? at.y : (-view.y + 160) / view.z
    const color = PALETTE[counter.current % PALETTE.length]
    setNodes((ns) => [...ns, { id, x, y, label: 'New node', sub: 'untitled', color, frame: at?.frame ?? null }])
    setSelected({ kind: 'node', id })
  }
  function addFrame() {
    const id = uid('f')
    const x = (-view.x + 240) / view.z
    const y = (-view.y + 200) / view.z
    setFrames((fs) => [...fs, { id, x, y, w: 280, h: 180, label: 'New frame', color: PALETTE[0] }])
    setSelected({ kind: 'frame', id })
  }
  function duplicateNode(id) {
    const n = nodes.find((x) => x.id === id)
    if (!n) return
    const nid = uid('n')
    setNodes((ns) => [...ns, { ...n, id: nid, x: n.x + 28, y: n.y + 28 }])
    setSelected({ kind: 'node', id: nid })
  }

  // ---- render ----
  const counts = `${frames.length} frames · ${nodes.length} nodes · ${edges.length} links`

  return (
    <div className="flex h-[78vh] gap-4">
      <div className="flex min-w-0 flex-1 flex-col gap-3">
        <Toolbar
          linkType={linkType}
          setLinkType={setLinkType}
          counts={counts}
          onAddNode={() => addNode(null)}
          onAddFrame={addFrame}
          onReset={() => setView({ x: 40, y: 20, z: 1 })}
        />

        <div
          ref={wrapRef}
          onPointerDown={startPan}
          onContextMenu={(e) => {
            e.preventDefault()
            setMenu(null)
          }}
          className="relative flex-1 overflow-hidden rounded-xl border bg-bg"
          style={{ touchAction: 'none', cursor: dragRef.current?.kind === 'pan' ? 'grabbing' : 'default' }}
        >
          {/* dotted grid */}
          <div
            className="pointer-events-none absolute inset-0"
            style={{
              backgroundImage: 'radial-gradient(var(--grid) 1.4px, transparent 1.4px)',
              backgroundSize: `${24 * view.z}px ${24 * view.z}px`,
              backgroundPosition: `${view.x}px ${view.y}px`,
            }}
          />

          {/* world layer */}
          <div
            className="absolute left-0 top-0 origin-top-left"
            style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}
          >
            {/* frames */}
            {frames.map((f) => {
              const on = selected?.kind === 'frame' && selected.id === f.id
              const dropping = dropFrame === f.id
              return (
                <div
                  key={f.id}
                  onPointerDown={(e) => startFrame(e, f.id)}
                  onContextMenu={(e) => openMenu(e, 'frame', f.id)}
                  className={`group absolute rounded-xl border-2 border-dashed transition-colors ${
                    on || dropping ? 'border-primary' : ''
                  }`}
                  style={{
                    left: f.x,
                    top: f.y,
                    width: f.w,
                    height: f.h,
                    borderColor: on || dropping ? 'var(--primary)' : f.color,
                    background: `color-mix(in srgb, ${f.color} 10%, transparent)`,
                  }}
                >
                  <div
                    className="pointer-events-none absolute -top-6 left-0 text-xs font-medium"
                    style={{ color: f.color }}
                  >
                    {f.label}
                  </div>
                  <PortHandles ownerId={f.id} connecting={!!connect} snapPort={connect?.targetId === f.id ? connect.targetPort : null} onStart={startConnect} />
                  <div
                    onPointerDown={(e) => startResize(e, f.id)}
                    className="absolute -bottom-1.5 -right-1.5 h-3.5 w-3.5 cursor-nwse-resize rounded-sm border-2 bg-surface"
                    style={{ borderColor: f.color }}
                  />
                </div>
              )
            })}

            {/* edges */}
            <svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width="1" height="1">
              <defs>
                <marker id="arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                  <path d="M0,0 L10,5 L0,10 z" fill="context-stroke" />
                </marker>
              </defs>
              {edges.map((ed) => {
                const r0 = rectOf(ed.from.node, nodes, frames)
                const r1 = rectOf(ed.to.node, nodes, frames)
                if (!r0 || !r1) return null
                const p0 = portPoint(r0, ed.from.port)
                const p1 = portPoint(r1, ed.to.port)
                const d = edgePath(p0, ed.from.port, p1, ed.to.port)
                const on = selected?.kind === 'edge' && selected.id === ed.id
                return (
                  <g key={ed.id}>
                    <path
                      d={d}
                      fill="none"
                      stroke="transparent"
                      strokeWidth="16"
                      className="pointer-events-auto cursor-pointer"
                      onPointerDown={(e) => {
                        e.stopPropagation()
                        setSelected({ kind: 'edge', id: ed.id })
                      }}
                    />
                    <path
                      d={d}
                      fill="none"
                      stroke={on ? 'var(--primary)' : 'var(--muted)'}
                      strokeWidth={on ? 3 : 2}
                      markerEnd={ed.type !== 'none' ? 'url(#arrow)' : undefined}
                      markerStart={ed.type === 'bidirectional' ? 'url(#arrow)' : undefined}
                    />
                  </g>
                )
              })}
              {connect && (
                <path
                  d={edgePathPreview(connect)}
                  fill="none"
                  stroke="var(--primary)"
                  strokeWidth="2"
                  strokeDasharray="6 5"
                />
              )}
            </svg>

            {/* nodes */}
            {nodes.map((n) => {
              const on = selected?.kind === 'node' && selected.id === n.id
              return (
                <div
                  key={n.id}
                  onPointerDown={(e) => startNode(e, n.id)}
                  onContextMenu={(e) => openMenu(e, 'node', n.id)}
                  className={`group absolute cursor-grab rounded-xl border bg-surface shadow-sm transition-shadow active:cursor-grabbing ${
                    on ? 'ring-2 ring-primary' : ''
                  }`}
                  style={{ left: n.x, top: n.y, width: NODE_W, height: NODE_H }}
                >
                  <div className="h-1.5 w-full rounded-t-xl" style={{ background: n.color }} />
                  <div className="px-3 py-2">
                    <div className="truncate text-sm font-semibold text-fg">{n.label}</div>
                    <div className="truncate text-xs text-muted">{n.sub}</div>
                  </div>
                  <PortHandles ownerId={n.id} connecting={!!connect} snapPort={connect?.targetId === n.id ? connect.targetPort : null} onStart={startConnect} />
                </div>
              )
            })}
          </div>

          {/* hint overlay */}
          <div className="pointer-events-none absolute bottom-3 left-3 rounded-lg border bg-surface/80 px-3 py-2 text-xs text-muted backdrop-blur">
            Drag canvas to pan · scroll to zoom · drag a port to connect · right-click for actions
          </div>
        </div>
      </div>

      <Properties
        selected={selected}
        nodes={nodes}
        frames={frames}
        edges={edges}
        patchNode={patchNode}
        patchFrame={patchFrame}
        patchEdge={patchEdge}
        deleteNode={deleteNode}
        deleteFrame={deleteFrame}
        deleteEdge={deleteEdge}
      />

      {menu && (
        <ContextMenu
          menu={menu}
          inFrame={menu.kind === 'node' ? !!nodes.find((n) => n.id === menu.id)?.frame : false}
          onClose={() => setMenu(null)}
          actions={
            menu.kind === 'node'
              ? [
                  { label: 'Duplicate', fn: () => duplicateNode(menu.id) },
                  { label: 'Disconnect links', fn: () => disconnect(menu.id) },
                  ...(nodes.find((n) => n.id === menu.id)?.frame
                    ? [{ label: 'Remove from frame', fn: () => patchNode(menu.id, { frame: null }) }]
                    : []),
                  { sep: true },
                  { label: 'Delete node', danger: true, fn: () => deleteNode(menu.id) },
                ]
              : [
                  {
                    label: 'Add node here',
                    fn: () => {
                      const f = frames.find((x) => x.id === menu.id)
                      addNode({ x: f.x + f.w / 2 - NODE_W / 2, y: f.y + f.h / 2 - NODE_H / 2, frame: f.id })
                    },
                  },
                  { label: 'Disconnect links', fn: () => disconnect(menu.id) },
                  { sep: true },
                  { label: 'Delete frame', danger: true, fn: () => deleteFrame(menu.id) },
                ]
          }
        />
      )}
    </div>
  )
}

function edgePathPreview(c) {
  // approximate ports for a live preview using direction toward the cursor
  const dx = c.to.x - c.from.x
  const dy = c.to.y - c.from.y
  const k = clamp(Math.hypot(dx, dy) / 2, 40, 170)
  const sign = (v) => (v >= 0 ? 1 : -1)
  const horiz = Math.abs(dx) > Math.abs(dy)
  const d0 = horiz ? [sign(dx), 0] : [0, sign(dy)]
  const c0 = { x: c.from.x + d0[0] * k, y: c.from.y + d0[1] * k }
  const c1 = { x: c.to.x - d0[0] * k, y: c.to.y - d0[1] * k }
  return `M ${c.from.x} ${c.from.y} C ${c0.x} ${c0.y} ${c1.x} ${c1.y} ${c.to.x} ${c.to.y}`
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
            className={`absolute h-3 w-3 rounded-full border-2 border-primary bg-surface transition ${pos[port]} ${
              connecting ? 'opacity-100' : 'opacity-0 group-hover:opacity-100'
            } ${snap ? 'pulse-ring scale-150 bg-primary' : ''}`}
          />
        )
      })}
    </>
  )
}

function Toolbar({ linkType, setLinkType, counts, onAddNode, onAddFrame, onReset }) {
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-xl border bg-surface px-3 py-2">
      <Button size="sm" onClick={onAddNode}>
        <Icon.Plus size={16} /> Node
      </Button>
      <Button size="sm" variant="outline" onClick={onAddFrame}>
        <Icon.Frame size={16} /> Frame
      </Button>
      <div className="mx-1 h-5 w-px bg-border" />
      <span className="text-xs text-muted">New link</span>
      <div className="flex gap-1 rounded-lg bg-surface2 p-1">
        {LINK_TYPES.map((lt) => {
          const Ico = Icon[lt.icon]
          const on = linkType === lt.id
          return (
            <button
              key={lt.id}
              title={lt.label}
              onClick={() => setLinkType(lt.id)}
              className={`flex items-center gap-1 rounded-md px-2 py-1 text-xs transition ${on ? 'bg-surface text-fg shadow' : 'text-muted'}`}
            >
              <Ico size={15} />
            </button>
          )
        })}
      </div>
      <div className="ml-auto flex items-center gap-3">
        <span className="text-xs text-muted">{counts}</span>
        <Button size="sm" variant="ghost" onClick={onReset}>
          <Icon.Move size={15} /> Reset view
        </Button>
      </div>
    </div>
  )
}

function ContextMenu({ menu, onClose, actions }) {
  const x = Math.min(menu.x, window.innerWidth - 200)
  const y = Math.min(menu.y, window.innerHeight - 220)
  return (
    <div className="fixed inset-0 z-50" onClick={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }}>
      <div
        className="absolute w-48 rounded-lg border bg-surface p-1 shadow-xl"
        style={{ left: x, top: y }}
        onClick={(e) => e.stopPropagation()}
      >
        {actions.map((a, i) =>
          a.sep ? (
            <div key={i} className="my-1 h-px bg-border" />
          ) : (
            <button
              key={i}
              onClick={() => {
                a.fn()
                onClose()
              }}
              className={`block w-full rounded-md px-2.5 py-1.5 text-left text-sm hover:bg-surface2 ${a.danger ? 'text-danger' : 'text-fg'}`}
            >
              {a.label}
            </button>
          ),
        )}
      </div>
    </div>
  )
}

function Properties({ selected, nodes, frames, edges, patchNode, patchFrame, patchEdge, deleteNode, deleteFrame, deleteEdge }) {
  return (
    <div className="w-72 shrink-0 overflow-y-auto rounded-xl border bg-surface p-4">
      <h3 className="mb-3 text-sm font-semibold">Properties</h3>
      <PropertiesBody
        selected={selected}
        nodes={nodes}
        frames={frames}
        edges={edges}
        patchNode={patchNode}
        patchFrame={patchFrame}
        patchEdge={patchEdge}
        deleteNode={deleteNode}
        deleteFrame={deleteFrame}
        deleteEdge={deleteEdge}
      />
    </div>
  )
}

function PropertiesBody({ selected, nodes, frames, edges, patchNode, patchFrame, patchEdge, deleteNode, deleteFrame, deleteEdge }) {
  if (!selected) {
    return <p className="text-sm text-muted">Select a node, frame, or link to edit its properties.</p>
  }

  if (selected.kind === 'node') {
    const n = nodes.find((x) => x.id === selected.id)
    if (!n) return null
    return (
      <div className="space-y-3">
        <Field label="Label">
          <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
        </Field>
        <Field label="Subtitle">
          <input className={inputCls} value={n.sub} onChange={(e) => patchNode(n.id, { sub: e.target.value })} />
        </Field>
        <ColorPicker value={n.color} onChange={(c) => patchNode(n.id, { color: c })} />
        <div className="grid grid-cols-2 gap-2">
          <Field label="X">
            <input type="number" className={inputCls} value={Math.round(n.x)} onChange={(e) => patchNode(n.id, { x: +e.target.value })} />
          </Field>
          <Field label="Y">
            <input type="number" className={inputCls} value={Math.round(n.y)} onChange={(e) => patchNode(n.id, { y: +e.target.value })} />
          </Field>
        </div>
        <Field label="Member of frame">
          <select className={inputCls} value={n.frame ?? ''} onChange={(e) => patchNode(n.id, { frame: e.target.value || null })}>
            <option value="">— none —</option>
            {frames.map((f) => (
              <option key={f.id} value={f.id}>{f.label}</option>
            ))}
          </select>
        </Field>
        <p className="text-xs text-muted">This node exposes 4 ports — hover it and drag from any port to connect.</p>
        <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
          <Icon.Trash size={16} /> Delete node
        </Button>
      </div>
    )
  }

  if (selected.kind === 'frame') {
    const f = frames.find((x) => x.id === selected.id)
    if (!f) return null
    const count = nodes.filter((n) => n.frame === f.id).length
    return (
      <div className="space-y-3">
        <Field label="Title">
          <input className={inputCls} value={f.label} onChange={(e) => patchFrame(f.id, { label: e.target.value })} />
        </Field>
        <ColorPicker value={f.color} onChange={(c) => patchFrame(f.id, { color: c })} />
        <div className="grid grid-cols-2 gap-2">
          <Field label="X">
            <input type="number" className={inputCls} value={Math.round(f.x)} onChange={(e) => patchFrame(f.id, { x: +e.target.value })} />
          </Field>
          <Field label="Y">
            <input type="number" className={inputCls} value={Math.round(f.y)} onChange={(e) => patchFrame(f.id, { y: +e.target.value })} />
          </Field>
          <Field label="W">
            <input type="number" className={inputCls} value={Math.round(f.w)} onChange={(e) => patchFrame(f.id, { w: Math.max(160, +e.target.value) })} />
          </Field>
          <Field label="H">
            <input type="number" className={inputCls} value={Math.round(f.h)} onChange={(e) => patchFrame(f.id, { h: Math.max(110, +e.target.value) })} />
          </Field>
        </div>
        <p className="text-xs text-muted">Contains {count} {count === 1 ? 'node' : 'nodes'}.</p>
        <Button variant="danger" size="sm" className="w-full" onClick={() => deleteFrame(f.id)}>
          <Icon.Trash size={16} /> Delete frame
        </Button>
      </div>
    )
  }

  // edge
  const ed = edges.find((x) => x.id === selected.id)
  if (!ed) return null
  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-sm">
        <span className="font-mono text-xs">{ed.from.node}.{ed.from.port}</span>
        <span className="mx-1 text-muted">→</span>
        <span className="font-mono text-xs">{ed.to.node}.{ed.to.port}</span>
      </div>
      <div className="space-y-1">
        <span className="block text-xs font-medium text-muted">Style</span>
        <div className="grid gap-1">
          {LINK_TYPES.map((lt) => {
            const Ico = Icon[lt.icon]
            const on = ed.type === lt.id
            return (
              <button
                key={lt.id}
                onClick={() => patchEdge(ed.id, { type: lt.id })}
                className={`flex items-center gap-2 rounded-lg border px-3 py-2 text-sm transition ${on ? 'border-primary bg-primary/10 text-primary' : 'text-fg hover:bg-surface2'}`}
              >
                <Ico size={16} /> {lt.label}
              </button>
            )
          })}
        </div>
      </div>
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteEdge(ed.id)}>
        <Icon.Trash size={16} /> Delete link
      </Button>
    </div>
  )
}

function ColorPicker({ value, onChange }) {
  return (
    <div>
      <span className="mb-1 block text-xs font-medium text-muted">Color</span>
      <div className="flex items-center gap-1.5">
        {PALETTE.map((c) => (
          <button
            key={c}
            onClick={() => onChange(c)}
            className={`h-6 w-6 rounded-full border-2 transition ${value === c ? 'border-fg scale-110' : 'border-transparent'}`}
            style={{ background: c }}
          />
        ))}
        <input type="color" value={value} onChange={(e) => onChange(e.target.value)} className="h-6 w-8 cursor-pointer rounded border bg-transparent" />
      </div>
    </div>
  )
}
