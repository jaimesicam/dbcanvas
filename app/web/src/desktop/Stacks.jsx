import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Window } from '../wm/WindowManager.jsx'
import { stackApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { dashApi } from '../lib/dashApi.js'
import { nodeWebLinks } from '../lib/nodeLinks.js'
import { sendHandoff, nudgeHandoff } from '../lib/handoff.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import { useBrowser } from '../browser/BrowserProvider.jsx'
import { useAuth } from '../auth/AuthProvider.jsx'
import { useSession } from '../session/SessionProvider.jsx'

// Stacks.jsx — your stacks and their nodes on the desktop (desktop/Desktop.jsx).
//
// Each stack is an icon on the right of the desktop, with a dot for its state. Open
// one and its nodes come up in a folder window: an icon per node, lit by what its
// container is doing, with the things you most often want from a node one
// right-click away — its console, its web UIs (in a new tab or the stack's VNC
// Browser), and the stack on the canvas. Everything else stays in Database Stacks,
// which "Open in Database Stacks" takes you to with the stack already open.

const POLL_LIST_MS = 15000
const POLL_FOLDER_MS = 4000

const STACK_DOT = { deployed: 'bg-success', draft: 'bg-muted/60', expired: 'bg-danger' }
const TONE_DOT = { success: 'bg-success', warning: 'bg-warning', danger: 'bg-danger', muted: 'bg-muted/60' }
const stateDot = (state) => TONE_DOT[DEPLOY_TONE[state]] || 'bg-muted/40'

// A node's icon: the kind of thing it is, at a glance.
function nodeIcon(type) {
  if (type === 'vnc') return Icon.Monitor
  if (type === 'linuxclient') return Icon.Terminal
  if (type === 'k3d') return Icon.Kubernetes || Icon.Nodes
  if (/sim$|^mclusteradmin$|^bighole$|^pmm2?$|^orchestrator$/.test(type || '')) return Icon.Dashboard
  if (/^(intranet|sambaad|keycloak|openbao|repository|seaweedfs|watchtower)$/.test(type || '')) return Icon.Nodes
  return Icon.Database
}

// openStackInDesigner opens Database Stacks on a stack: the handoff it reads, the
// window brought forward, and a nudge in case that window was already on screen.
export function openStackInDesigner(onOpen, stackId) {
  sendHandoff('dbcanvas.openStack', String(stackId))
  onOpen('stack-designer')
  setTimeout(nudgeHandoff, 50)
}

// useStacks is the stack list, kept fresh: on a timer, and whenever the driver of a
// shared session (or this browser) changes something on the server.
export function useStacks() {
  const [stacks, setStacks] = useState([])
  const load = useCallback(() => stackApi.list().then((l) => setStacks(Array.isArray(l) ? l : [])).catch(() => {}), [])
  useEffect(() => {
    load()
    const t = setInterval(() => { if (!document.hidden) load() }, POLL_LIST_MS)
    let deb = null
    const on = () => { clearTimeout(deb); deb = setTimeout(load, 500) }
    addEventListener('dbcanvas:invalidate', on)
    addEventListener('focus', on)
    return () => { clearInterval(t); clearTimeout(deb); removeEventListener('dbcanvas:invalidate', on); removeEventListener('focus', on) }
  }, [load])
  return [stacks, load]
}

// PopMenu is a right-click menu at a point. An entry is { label, onClick } or
// { sep: true }; a disabled one carries its reason as a tooltip.
export function PopMenu({ at, items, onClose }) {
  return createPortal(
    <>
      <div className="fixed inset-0 z-[80]" onPointerDown={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }} />
      <div className="fixed z-[81] min-w-[200px] max-w-[300px] rounded-md border bg-surface py-1 text-xs shadow-xl"
        style={{ left: Math.min(at.x, innerWidth - 310), top: Math.min(at.y, innerHeight - 26 * items.length - 16) }}>
        {items.map((it, i) => (it.sep
          ? <div key={`sep${i}`} className="my-1 border-t" />
          : (
            <button key={it.label} disabled={it.disabled} title={it.title}
              onClick={() => { it.onClick(); onClose() }}
              className="block w-full truncate px-3 py-1.5 text-left text-fg hover:bg-surface2 disabled:cursor-not-allowed disabled:opacity-40">
              {it.label}
            </button>
          )))}
      </div>
    </>,
    document.body,
  )
}

// StackIcons is the column of stacks on the right of the desktop.
export function StackIcons({ stacks, onOpenFolder, onOpen }) {
  const [sel, setSel] = useState(null)
  const [menu, setMenu] = useState(null)
  if (!stacks.length) return null
  return (
    <div className="pointer-events-none absolute inset-y-0 right-0 flex flex-col flex-wrap-reverse content-start gap-1 p-3">
      <div className="pointer-events-none w-24 px-1 pb-0.5 text-center text-[10px] font-semibold uppercase tracking-wide text-muted">Stacks</div>
      {stacks.map((st) => (
        <button key={st.id} data-desktop-stack={st.id} title={`${st.name} — ${st.status}. Double-click for its nodes.`}
          onClick={() => setSel(st.id)}
          onDoubleClick={() => onOpenFolder(st)}
          onKeyDown={(e) => { if (e.key === 'Enter') onOpenFolder(st) }}
          onContextMenu={(e) => { e.preventDefault(); e.stopPropagation(); setSel(st.id); setMenu({ x: e.clientX, y: e.clientY, st }) }}
          className={`pointer-events-auto flex w-24 flex-col items-center gap-1.5 rounded-lg px-1 py-2 text-center ${sel === st.id ? 'bg-primary/20 ring-1 ring-primary/50' : 'hover:bg-surface/60'}`}>
          <span className="relative flex h-11 w-11 items-center justify-center rounded-xl border bg-surface/80 text-primary shadow-sm">
            <Icon.Stacks size={22} />
            <span className={`absolute -right-0.5 -top-0.5 h-2.5 w-2.5 rounded-full ring-2 ring-surface ${STACK_DOT[st.status] || 'bg-muted/60'}`} />
          </span>
          <span className="line-clamp-2 break-all text-[11px] leading-tight text-fg drop-shadow">{st.name}</span>
        </button>
      ))}
      {menu && (
        <PopMenu at={menu} onClose={() => setMenu(null)} items={[
          { label: 'Show nodes', onClick: () => onOpenFolder(menu.st) },
          { label: 'Open in Database Stacks', onClick: () => openStackInDesigner(onOpen, menu.st.id) },
        ]} />
      )}
    </div>
  )
}

// StackFolder is one stack's nodes, in a window.
export function StackFolder({ stack, onClose, onOpen }) {
  const [detail, setDetail] = useState(null)
  const [err, setErr] = useState('')
  const [sel, setSel] = useState(null)
  const [menu, setMenu] = useState(null)
  const { openTerminal } = useTerminals() || {}
  const { openBrowser, openInDesktop } = useBrowser()
  const { guest } = useAuth()
  const session = useSession()
  const watcher = session.active && !session.isDriver

  useEffect(() => {
    let alive = true
    const load = () => stackApi.get(stack.id)
      .then((d) => { if (alive) { setDetail(d); setErr('') } })
      .catch((e) => { if (alive) setErr(e.message) })
    load()
    const t = setInterval(() => { if (!document.hidden) load() }, POLL_FOLDER_MS)
    addEventListener('dbcanvas:invalidate', load)
    return () => { alive = false; clearInterval(t); removeEventListener('dbcanvas:invalidate', load) }
  }, [stack.id])

  const design = useMemo(() => {
    const d = detail?.design
    if (!d) return { nodes: [], frames: [] }
    try { return typeof d === 'string' ? JSON.parse(d) : d } catch { return { nodes: [], frames: [] } }
  }, [detail])
  const deps = useMemo(() => Object.fromEntries((detail?.deployments || []).map((d) => [d.nodeId, d])), [detail])
  const frameName = useMemo(() => Object.fromEntries((design.frames || []).map((f) => [f.id, f.label || f.name || f.id])), [design])
  const vncUp = (design.nodes || []).some((n) => n.type === 'vnc' && deps[n.id]?.state === 'running')

  // Nodes, grouped by the cluster (frame) they belong to; loose ones first.
  const groups = useMemo(() => {
    const g = new Map()
    for (const n of design.nodes || []) {
      const k = n.frameId ? frameName[n.frameId] || 'Cluster' : ''
      if (!g.has(k)) g.set(k, [])
      g.get(k).push(n)
    }
    return [...g.entries()].sort(([a], [b]) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))
  }, [design, frameName])

  const label = (n) => n.label || n.id
  const terminal = (n, user, suffix = 'root') => openTerminal?.({ stackId: stack.id, nodeId: n.id, title: `${label(n)} · ${suffix}`, user })
  const actionsFor = (n) => {
    const dep = deps[n.id]
    const running = dep?.state === 'running'
    const links = running ? nodeWebLinks(n.type, dep) : []
    const items = []
    if (running) {
      if (n.type === 'pmm') {
        items.push({ label: 'Enter root console', onClick: () => terminal(n, '0') })
        items.push({ label: 'Enter PMM console', onClick: () => terminal(n, undefined, 'pmm') })
      } else {
        items.push({ label: 'Enter root console', onClick: () => terminal(n) })
      }
    }
    if (links.length) {
      items.push({ sep: true })
      for (const l of links) {
        const name = links.length > 1 ? ` — ${l.label}` : ''
        if (!guest) items.push({ label: `Open in new tab${name}`, onClick: () => window.open(l.url, '_blank', 'noreferrer') })
        else items.push({ label: `Open in a browser window${name}`, onClick: () => openBrowser(l.url) })
        if (n.type !== 'vnc' && !watcher) {
          items.push({ label: `Open in VNC Browser${name}`, disabled: !vncUp, title: vncUp ? '' : 'Add and deploy an Ubuntu VNC node in this stack first.',
            onClick: () => openInDesktop(l.url) })
        }
      }
    }
    items.push({ sep: true })
    items.push({ label: 'Open in Database Stacks', onClick: () => openStackInDesigner(onOpen, stack.id) })
    return items
  }
  // A double-click does the obvious thing: a node's web UI if it has one, else its
  // console, else the stack on the canvas.
  const primary = (n) => {
    const dep = deps[n.id]
    const links = dep?.state === 'running' ? nodeWebLinks(n.type, dep) : []
    if (links.length) return guest ? openBrowser(links[0].url) : window.open(links[0].url, '_blank', 'noreferrer')
    if (dep?.state === 'running') return terminal(n, n.type === 'pmm' ? '0' : undefined)
    return openStackInDesigner(onOpen, stack.id)
  }

  const nodes = design.nodes || []
  return (
    <Window id={`stackfolder:${stack.id}`} title={stack.name} icon={<Icon.Stacks size={13} />} size={{ w: 640, h: 420 }}
      onClose={onClose}
      header={(
        <span className="flex min-w-0 flex-1 items-center justify-end gap-2">
          <span className="text-[11px] text-muted">{nodes.length} node{nodes.length === 1 ? '' : 's'} · {detail?.status || stack.status}</span>
          <button onClick={() => openStackInDesigner(onOpen, stack.id)} className="rounded px-2 py-0.5 text-[11px] text-primary hover:bg-primary/10">
            Open in Database Stacks
          </button>
        </span>
      )}>
      <div className="absolute inset-0 overflow-auto bg-bg p-3" onPointerDown={(e) => { if (e.target === e.currentTarget) setSel(null) }}>
        {err && <div className="mb-2 rounded border border-danger/30 bg-danger/10 px-2 py-1 text-xs text-danger">{err}</div>}
        {!detail && !err && <div className="p-4 text-sm text-muted">Loading…</div>}
        {detail && nodes.length === 0 && (
          <div className="p-4 text-sm text-muted">This stack has no nodes yet. <button className="text-primary hover:underline" onClick={() => openStackInDesigner(onOpen, stack.id)}>Design it</button>.</div>
        )}
        {groups.map(([g, list]) => (
          <div key={g || '_'} className="mb-3">
            {g && <div className="mb-1 px-1 text-[10px] font-semibold uppercase tracking-wide text-muted">{g}</div>}
            <div className="flex flex-wrap gap-1">
              {list.map((n) => {
                const Ico = nodeIcon(n.type)
                const dep = deps[n.id]
                return (
                  <button key={n.id} data-folder-node={n.id} title={`${label(n)} · ${n.type} · ${dep?.state || 'not deployed'}`}
                    onClick={() => setSel(n.id)} onDoubleClick={() => primary(n)}
                    onContextMenu={(e) => { e.preventDefault(); setSel(n.id); setMenu({ x: e.clientX, y: e.clientY, n }) }}
                    className={`flex w-24 flex-col items-center gap-1 rounded-lg px-1 py-2 text-center ${sel === n.id ? 'bg-primary/20 ring-1 ring-primary/50' : 'hover:bg-surface2'}`}>
                    <span className="relative flex h-10 w-10 items-center justify-center rounded-xl border bg-surface text-primary">
                      <Ico size={20} />
                      <span className={`absolute -right-0.5 -top-0.5 h-2.5 w-2.5 rounded-full ring-2 ring-bg ${stateDot(dep?.state)}`} />
                    </span>
                    <span className="line-clamp-2 break-all text-[11px] leading-tight">{label(n)}</span>
                    <span className="text-[10px] text-muted">{dep?.state || 'not deployed'}</span>
                  </button>
                )
              })}
            </div>
          </div>
        ))}
      </div>
      {menu && <PopMenu at={menu} items={actionsFor(menu.n)} onClose={() => setMenu(null)} />}
    </Window>
  )
}

// DesktopStatus is the taskbar's status corner: what is running, the shared session,
// and the time.
export function DesktopStatus() {
  const [sum, setSum] = useState(null)
  const [now, setNow] = useState(() => new Date())
  const session = useSession()
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    const load = () => dashApi.summary().then((d) => { if (alive.current) setSum(d) }).catch(() => {})
    load()
    const t = setInterval(() => { if (!document.hidden) load() }, POLL_LIST_MS)
    const c = setInterval(() => setNow(new Date()), 15000)
    addEventListener('dbcanvas:invalidate', load)
    return () => { alive.current = false; clearInterval(t); clearInterval(c); removeEventListener('dbcanvas:invalidate', load) }
  }, [])
  const n = sum?.nodes
  const left = session.active && session.presence?.expiresAt
    ? Math.max(0, Math.round((new Date(session.presence.expiresAt) - now) / 60000)) : null
  return (
    <div className="flex shrink-0 items-center gap-3 border-l px-3 text-[11px] text-muted">
      {n && (
        <span title={`${sum.stacks?.deployed ?? 0} of ${sum.stacks?.total ?? 0} stacks deployed`} className="flex items-center gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-success" />{n.running} running
          {n.other > 0 && <><span className="ml-1 h-1.5 w-1.5 rounded-full bg-warning" />{n.other} other</>}
          {n.error > 0 && <><span className="ml-1 h-1.5 w-1.5 rounded-full bg-danger" /><span className="text-danger">{n.error} error</span></>}
        </span>
      )}
      {session.active && (
        <span className="flex items-center gap-1 text-primary" title="Shared session">
          <Icon.Share size={12} />
          {session.isDriver ? 'you drive' : `${session.controllerName || '…'} drives`}
          {left !== null && <span className="text-muted">· {left} min</span>}
        </span>
      )}
      <span className="tabular-nums">{now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
    </div>
  )
}
