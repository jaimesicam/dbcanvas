import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Window, useWindowManager, useWindowApi } from '../wm/WindowManager.jsx'
import { StackIcons, StackFolder, useStacks } from './Stacks.jsx'

// Desktop.jsx — DBCanvas as a desktop (lib/shellMode.js picks it; it is the default).
//
// Every page opens as a window (PageWindows) over a desktop (DesktopSurface) with an
// icon for each page, and the sidebar becomes the Start menu on the taskbar
// (StartButton). The pages are the same components the classic shell mounts, kept
// mounted the same way: a minimized window is hidden, not unmounted, so a page keeps
// its scroll position, its filters and its in-flight view — and stops polling, as a
// hidden tab does (lib/usePolling.jsx).
//
// The page windows share the window manager (wm/WindowManager.jsx) with the
// terminals, browser windows and file managers: one stack, one taskbar, one set of
// snapping and arranging rules for everything on screen.

// The Start menu's sections. A page not named here lands in the last one, so a new
// page is never lost from the menu, only filed under Other until it is placed.
const GROUPS = [
  { title: 'Workspace', ids: ['dashboard', 'stack-designer'] },
  { title: 'Data and queries', ids: ['data-generator', 'database-explorer', 'queryrun', 'benchmark', 'sample-code'] },
  { title: 'Diagnose', ids: ['packet-inspector', 'operator-debugger', 'core-dump', 'stalk-summary', 'log-summary', 'ftdc-summary', 'operator-summary', 'k8s-states'] },
  { title: 'Learn', ids: ['labs'] },
  { title: 'System', ids: ['profile', 'api', 'settings', 'users'] },
]

export function groupNav(nav) {
  const placed = new Set()
  const out = GROUPS.map((g) => {
    const items = g.ids.map((id) => nav.find((n) => n.id === id)).filter(Boolean)
    items.forEach((n) => placed.add(n.id))
    return { title: g.title, items }
  })
  const rest = nav.filter((n) => !placed.has(n.id))
  if (rest.length) out.push({ title: 'Other', items: rest })
  return out.filter((g) => g.items.length)
}

// pageWindowId is a page window's id in the window manager.
export const pageWindowId = (key) => `page:${key}`

// StartButton is the taskbar's first button and the menu it opens. It reads the
// page list and the opener through refs, so the window manager can hold one element
// for the life of the shell.
export function StartButton({ navRef, openRef }) {
  const [open, setOpen] = useState(false)
  const [q, setQ] = useState('')
  const btn = useRef(null)
  const input = useRef(null)
  useEffect(() => {
    if (!open) return undefined
    setQ('')
    const t = setTimeout(() => input.current?.focus(), 0)
    const onKey = (e) => { if (e.key === 'Escape') setOpen(false) }
    addEventListener('keydown', onKey)
    return () => { clearTimeout(t); removeEventListener('keydown', onKey) }
  }, [open])
  const nav = navRef.current || []
  const groups = groupNav(nav)
  const needle = q.trim().toLowerCase()
  const hits = needle ? nav.filter((n) => `${n.label} ${n.hint || ''}`.toLowerCase().includes(needle)) : null
  const pick = (id, another) => { openRef.current?.(id, another); setOpen(false) }
  const row = (n) => {
    const Ico = Icon[n.icon] || Icon.Dashboard
    return (
      <button key={n.id} onClick={() => pick(n.id)} title={`${n.hint || ''}\nRight-click to open another window of it`}
        onContextMenu={(e) => { e.preventDefault(); pick(n.id, true) }}
        className="flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-sm hover:bg-surface2">
        <Ico size={16} className="shrink-0 text-primary" />
        <span className="truncate">{n.label}</span>
      </button>
    )
  }
  return (
    <>
      <button ref={btn} onClick={() => setOpen((o) => !o)} title="Start — every page"
        className={`mr-1 flex shrink-0 items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-semibold ${open ? 'bg-primary text-primary-fg' : 'bg-primary/15 text-primary hover:bg-primary/25'}`}>
        <Icon.Brand size={15} /> Start
      </button>
      {open && createPortal(
        <>
          <div className="fixed inset-0 z-[80]" onPointerDown={() => setOpen(false)} />
          <div data-start-menu className="fixed bottom-10 left-2 z-[81] flex max-h-[min(640px,80vh)] w-[min(560px,94vw)] flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl">
            <div className="flex items-center gap-2 border-b px-3 py-2">
              <Icon.Search size={15} className="text-muted" />
              <input ref={input} value={q} onChange={(e) => setQ(e.target.value)} placeholder="Find a page…"
                onKeyDown={(e) => { if (e.key === 'Enter' && hits?.[0]) pick(hits[0].id) }}
                className="min-w-0 flex-1 bg-transparent text-sm outline-none" />
              <kbd className="rounded bg-surface2 px-1.5 text-[10px] text-muted">Esc</kbd>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {hits ? (
                hits.length ? hits.map(row) : <div className="px-2.5 py-3 text-sm text-muted">No page matches “{q}”.</div>
              ) : (
                <div className="grid grid-cols-1 gap-x-2 sm:grid-cols-2">
                  {groups.map((g) => (
                    <div key={g.title} className="mb-2">
                      <div className="px-2.5 pb-1 pt-1.5 text-[10px] font-semibold uppercase tracking-wide text-muted">{g.title}</div>
                      {g.items.map(row)}
                    </div>
                  ))}
                </div>
              )}
            </div>
            <div className="border-t px-3 py-1.5 text-[11px] text-muted">Right-click a page to open another window of it · ⌘K finds one from anywhere</div>
          </div>
        </>,
        document.body,
      )}
    </>
  )
}

// DesktopSurface is what is behind the windows: a wallpaper, an icon for each page
// (double-click, or Enter, opens it), and a right-click menu for arranging.
export function DesktopSurface({ nav, onOpen }) {
  const api = useWindowApi()
  const [sel, setSel] = useState(null)
  const [menu, setMenu] = useState(null)
  const items = useMemo(() => groupNav(nav).flatMap((g) => g.items), [nav])
  // Stacks, on the right; a stack opens as a folder of its nodes (desktop/Stacks.jsx).
  const showStacks = nav.some((n) => n.id === 'stack-designer')
  const [stacks] = useStacks()
  const [folders, setFolders] = useState([])
  const openFolder = (st) => {
    setFolders((fs) => (fs.some((f) => f.id === st.id) ? fs : [...fs, st]))
    setTimeout(() => api.focus(`stackfolder:${st.id}`), 0)
  }
  return (
    <div
      data-desktop
      className="relative min-h-0 flex-1 overflow-hidden"
      style={{
        background: 'radial-gradient(ellipse at 12% 8%, color-mix(in srgb, var(--primary) 16%, transparent), transparent 55%),'
          + ' radial-gradient(ellipse at 90% 95%, color-mix(in srgb, var(--primary) 10%, transparent), transparent 50%), var(--bg)',
      }}
      onPointerDown={(e) => { if (e.target === e.currentTarget) setSel(null) }}
      // A folder window is a child here in React but not in the page (it is portalled
      // to the window layer), so its events bubble in; only the desktop's own count.
      onContextMenu={(e) => {
        if (!e.currentTarget.contains(e.target) || e.target.closest('[data-desktop-icon], [data-desktop-stack]')) return
        e.preventDefault()
        setMenu({ x: e.clientX, y: e.clientY })
      }}
    >
      <div className="flex h-full flex-col flex-wrap content-start gap-1 p-3">
        {items.map((n) => {
          const Ico = Icon[n.icon] || Icon.Dashboard
          const on = sel === n.id
          return (
            <button key={n.id} data-desktop-icon={n.id} title={n.hint}
              onClick={() => setSel(n.id)}
              onDoubleClick={() => onOpen(n.id)}
              onKeyDown={(e) => { if (e.key === 'Enter') onOpen(n.id) }}
              onContextMenu={(e) => { e.preventDefault(); setSel(n.id); setMenu({ x: e.clientX, y: e.clientY, id: n.id }) }}
              className={`flex w-24 flex-col items-center gap-1.5 rounded-lg px-1 py-2 text-center ${on ? 'bg-primary/20 ring-1 ring-primary/50' : 'hover:bg-surface/60'}`}>
              <span className="flex h-11 w-11 items-center justify-center rounded-xl border bg-surface/80 text-primary shadow-sm">
                <Ico size={22} />
              </span>
              <span className="line-clamp-2 text-[11px] leading-tight text-fg drop-shadow">{n.label}</span>
            </button>
          )
        })}
      </div>
      {showStacks && <StackIcons stacks={stacks} onOpenFolder={openFolder} onOpen={onOpen} />}
      {folders.map((st) => (
        <StackFolder key={st.id} stack={stacks.find((x) => x.id === st.id) || st} onOpen={onOpen}
          onClose={() => setFolders((fs) => fs.filter((f) => f.id !== st.id))} />
      ))}
      {menu && createPortal(
        <>
          <div className="fixed inset-0 z-[80]" onPointerDown={() => setMenu(null)} onContextMenu={(e) => { e.preventDefault(); setMenu(null) }} />
          <div className="fixed z-[81] min-w-[180px] rounded-md border bg-surface py-1 text-xs shadow-xl"
            style={{ left: Math.min(menu.x, innerWidth - 190), top: Math.min(menu.y, innerHeight - 170) }}>
            {(menu.id ? [
              { label: 'Open', onClick: () => onOpen(menu.id) },
              { label: 'Open another window', onClick: () => onOpen(menu.id, true) },
            ] : [
              { label: 'Tile windows', onClick: api.tile },
              { label: 'Cascade windows', onClick: api.cascade },
              { label: 'Minimize all', onClick: api.minimizeAll },
            ]).map((it) => (
              <button key={it.label} onClick={() => { it.onClick(); setMenu(null) }} className="block w-full px-3 py-1.5 text-left text-fg hover:bg-surface2">
                {it.label}
              </button>
            ))}
          </div>
        </>,
        document.body,
      )}
    </div>
  )
}

// PageWindows is one window per open page. renderPage(tab, visible) is the page
// itself with the providers the shell gives every page; it comes from App so this
// file does not import the pages.
export function PageWindows({ tabs, nav, activeKey, setActiveKey, closeTab, renderPage, privateIds }) {
  const wm = useWindowManager()
  // The front page window is the active page: the top bar's title and Refresh, the
  // address, what a follower is shown.
  const front = wm.front
  useEffect(() => {
    if (!front?.startsWith('page:')) return
    const key = front.slice(5)
    if (key !== activeKey && tabs.some((t) => t.key === key)) setActiveKey(key)
  }, [front]) // eslint-disable-line react-hooks/exhaustive-deps

  const a = wm.area()
  const size = { w: Math.round(a.w * 0.86), h: Math.round(a.h * 0.88) }
  const seen = {}
  return tabs.map((t) => {
    const meta = nav.find((n) => n.id === t.id) ?? nav[0]
    const Ico = Icon[meta.icon] || Icon.Dashboard
    seen[t.id] = (seen[t.id] || 0) + 1
    const n = seen[t.id]
    const min = !!wm.wins[pageWindowId(t.key)]?.min
    return (
      <Window key={t.key} id={pageWindowId(t.key)} title={n > 1 ? `${meta.label} · ${n}` : meta.label}
        icon={<Ico size={13} />} size={size} onClose={() => closeTab(t.key)} private={privateIds.has(t.id)} bodyClass="bg-bg">
        <div className="absolute inset-0 overflow-auto p-5">
          <div className={meta.fill ? 'h-full' : ''}>{renderPage(t, !min)}</div>
        </div>
      </Window>
    )
  })
}
