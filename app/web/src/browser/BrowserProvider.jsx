import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { useAuth } from '../auth/AuthProvider.jsx'
import { useSession } from '../session/SessionProvider.jsx'
import { Window, useWindowManager } from '../wm/WindowManager.jsx'

// BrowserProvider — node web UIs in a window of their own, through DBCanvas's port
// (app/browse.go).
//
// Every node UI is linked as http://<this host>:<published port>/…, which only works
// where that port is reachable: on the DBCanvas machine, or through a port forward per
// UI. A browser window reaches the same page through /_p/<key>/, so a shared-session
// guest or anyone on one SSH tunnel can use a VNC desktop, PMM or a simulator
// dashboard with nothing else set up.
//
// For a guest a plain click on such a link opens it in a browser window, because the
// raw port is exactly what a guest cannot reach. Right-click offers "Open in VNC
// Browser" and, for everyone else, "Open in new tab".
//
// "Open in VNC Browser" opens the page in Firefox on the stack's VNC desktop instead
// (app/browsedesk.go) and shows that desktop. A browser window shares only an
// address — each viewer loads their own copy with their own logins — so a guest
// watching Roundcube or PMM there sees a login page. On the desktop there is one copy
// of the page, and everyone sees exactly what the driver does.
//
// In a shared session a window the driver or the host opens is everyone's, like a
// shared terminal: it opens on every screen, follows the driver as they move around
// in it, and closes for everyone when they close it. Each browser resolves the link
// for itself, because what each may do differs — a watcher gets a VNC desktop they
// can see and not touch (app/browsevnc.go enforces it), and the desktop's password is
// filled in only for whoever may read it.

const Ctx = createContext(null)
export const useBrowser = () => useContext(Ctx) || { openBrowser: async () => {}, openInDesktop: async () => {} }

// nodeLink reports whether an href is a node's published web UI: this host (or
// loopback) on a port other than DBCanvas's own.
export function nodeLink(href) {
  try {
    const u = new URL(href, location.href)
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
    const host = u.hostname
    const local = host === location.hostname || host === 'localhost' || host === '127.0.0.1'
    const port = u.port || (u.protocol === 'https:' ? '443' : '80')
    const own = location.port || (location.protocol === 'https:' ? '443' : '80')
    if (!local || (port === own && u.host === location.host)) return null
    return u
  } catch {
    return null
  }
}

async function resolve(url) {
  const res = await fetch('/api/browse', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
    body: JSON.stringify({ url }),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`)
  return data
}

// desktopFor opens a node link in Firefox on the stack's VNC desktop and returns the
// desktop's own link.
async function desktopFor(url) {
  const res = await fetch('/api/browse/desktop', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin',
    body: JSON.stringify({ url }),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`)
  return data
}

const portOf = (href) => { try { return new URL(href, location.href).port } catch { return '' } }

let seq = 0

// srcOf is the address a window loads. A VNC desktop connects by itself, shared (so a
// second viewer never disconnects the first), with its password when this browser
// may know it, and view-only for a watcher — which the server enforces regardless.
function srcOf(w, viewOnly) {
  if (!w.base) return ''
  if (w.kind !== 'vnc') return w.base
  const u = new URL(w.base, location.href)
  const q = u.searchParams
  q.set('autoconnect', '1')
  q.set('shared', '1')
  if (!q.get('resize')) q.set('resize', 'scale')
  if (w.vncPassword) q.set('password', w.vncPassword)
  if (viewOnly) q.set('view_only', '1')
  else q.delete('view_only')
  return `${u.pathname}?${q.toString()}${u.hash}`
}

// A browser window's first size; the window manager (wm/WindowManager.jsx) places,
// stacks, snaps and minimizes it like any other window.
const WIN_SIZE = { w: 1100, h: 720 }
const wmId = (id) => `browser:${id}`

export function BrowserProvider({ children }) {
  const { guest } = useAuth()
  const session = useSession()
  const sessRef = useRef(session)
  sessRef.current = session
  const dismissed = useRef(new Set())
  const viewOnly = session.active && session.isGuest && !session.isDriver
  const [wins, setWins] = useState([]) // {id, title, base, kind, link, error, pageTitle}
  const [menu, setMenu] = useState(null) // {x, y, href}
  const wm = useWindowManager()
  const wmRef = useRef(wm)
  wmRef.current = wm
  const winsRef = useRef(wins)
  winsRef.current = wins

  const focus = useCallback((id) => wmRef.current.focus(wmId(id)), [])

  // openBrowser opens a window on a node link. remote is a window someone else in
  // the session opened, arriving here; otherwise, in a session, the driver's or the
  // host's window is announced to everyone.
  const openBrowser = useCallback(async (href, remote) => {
    const sh = sessRef.current
    const share = !remote && sh.active && (sh.isDriver || sh.isHost)
    const id = remote?.id || `${sh.me?.guestId ?? 'x'}-${Date.now().toString(36)}-${++seq}`
    setWins((ws) => [...ws, { id, title: remote?.title || 'Opening…', base: '', link: href,
      shared: !!(remote || share), followPath: remote?.path || '' }])
    try {
      const r = await resolve(href)
      const title = `${r.title} · ${r.stackName}`
      setWins((ws) => ws.map((w) => (w.id === id ? { ...w, base: r.url, kind: r.kind, vncPassword: r.vncPassword, title } : w)))
      if (share) sh.shareBrowser('browser-open', { id, link: href, title })
    } catch (e) {
      setWins((ws) => ws.map((w) => (w.id === id ? { ...w, title: 'Could not open', error: e.message } : w)))
    }
  }, [])

  // openInDesktop puts the page on the stack's VNC desktop, then shows the desktop:
  // the window already open on it if there is one, else a new one — which, in a
  // session, opens on everyone's screen like any other.
  const openInDesktop = useCallback(async (href) => {
    try {
      const r = await desktopFor(href)
      const port = portOf(r.desktopLink)
      const open = winsRef.current.find((w) => w.kind === 'vnc' && !w.error && portOf(w.link) === port)
      if (open) {
        focus(open.id)
      } else {
        await openBrowser(r.desktopLink)
      }
    } catch (e) {
      const id = `err-${Date.now().toString(36)}-${++seq}`
      setWins((ws) => [...ws, { id, title: 'Could not open in VNC Browser', base: '', link: href, error: e.message }])
    }
  }, [focus, openBrowser])

  // A shared window is closed by whoever has control, for everyone. Anyone else —
  // the host too, while a guest drives — cannot take it off the screens (the hub
  // refuses it as well).
  const close = useCallback((id) => {
    const sh = sessRef.current
    const w = winsRef.current.find((x) => x.id === id)
    if (w?.shared && sh.active) {
      if (!sh.isDriver) { sh.notice('Only whoever has control can close a shared window.'); return }
      sh.shareBrowser('browser-close', { id })
    }
    setWins((ws) => ws.filter((x) => x.id !== id))
  }, [])

  // The session's shared windows, kept in step: open what others opened, follow the
  // driver's path, close what they closed.
  const opening = useRef(new Set())
  useEffect(() => {
    const list = session.active ? session.browsers : []
    for (const b of list) {
      if (dismissed.current.has(b.id)) continue
      const cur = winsRef.current.find((w) => w.id === b.id)
      if (!cur && !opening.current.has(b.id)) {
        opening.current.add(b.id)
        openBrowser(b.link, b)
      } else if (cur && cur.followPath !== (b.path || '')) {
        setWins((ws) => ws.map((w) => (w.id === b.id ? { ...w, followPath: b.path || '' } : w)))
      }
    }
    const live = new Set(list.map((b) => b.id))
    if (winsRef.current.some((w) => w.shared && w.base && !live.has(w.id))) {
      setWins((ws) => ws.filter((w) => !w.shared || !w.base || live.has(w.id)))
    }
  }, [session.active, session.browsers, openBrowser])

  const onNav = useCallback((id, path) => {
    const sh = sessRef.current
    if (sh.active && sh.isDriver) sh.shareBrowser('browser-nav', { id, path })
  }, [])
  const patch = useCallback((id, p) => setWins((ws) => ws.map((w) => (w.id === id ? { ...w, ...p } : w))), [])

  // Node links: right-click for the menu; for a guest, a plain click opens the window.
  useEffect(() => {
    const linkOf = (e) => {
      const a = e.target?.closest?.('a[href]')
      if (!a || a.closest('[data-browser-window]')) return null
      return nodeLink(a.getAttribute('href')) ? a : null
    }
    const onContext = (e) => {
      const a = linkOf(e)
      if (!a) return
      e.preventDefault()
      setMenu({ x: e.clientX, y: e.clientY, href: a.href })
    }
    const onClick = (e) => {
      if (!guest || e.button !== 0 || e.metaKey || e.ctrlKey) return
      const a = linkOf(e)
      if (!a) return
      e.preventDefault()
      openBrowser(a.href)
    }
    document.addEventListener('contextmenu', onContext, true)
    document.addEventListener('click', onClick, true)
    return () => {
      document.removeEventListener('contextmenu', onContext, true)
      document.removeEventListener('click', onClick, true)
    }
  }, [guest, openBrowser])

  return (
    <Ctx.Provider value={{ openBrowser, openInDesktop }}>
      {children}
      {wins.map((w) => (
        <BrowserWindow key={w.id} win={w} src={srcOf(w, viewOnly)} viewOnly={viewOnly && w.kind === 'vnc'}
          driving={session.active && session.isDriver} following={session.following}
          canClose={!w.shared || !session.active || session.isDriver}
          onNav={(path) => w.shared && onNav(w.id, path)}
          onClose={() => close(w.id)} onPatch={(p) => patch(w.id, p)} />
      ))}
      {menu && (
        <>
          <div className="fixed inset-0 z-[70]" onClick={() => setMenu(null)} onContextMenu={(e) => { e.preventDefault(); setMenu(null) }} />
          <div className="fixed z-[71] min-w-[210px] max-w-[290px] rounded-md border bg-surface py-1 text-sm shadow-xl"
            style={{ left: Math.min(menu.x, innerWidth - 300), top: Math.min(menu.y, innerHeight - 180) }}>
            {!viewOnly && (
              <MenuItem icon={<Icon.Share size={14} />} onClick={() => { openInDesktop(menu.href); setMenu(null) }}
                hint={session.active ? 'One copy on the stack’s desktop — everyone sees what you see' : 'In Firefox on the stack’s VNC desktop'}>
                Open in VNC Browser
              </MenuItem>
            )}
            {!guest && <MenuItem icon={<Icon.External size={14} />} onClick={() => { window.open(menu.href, '_blank', 'noreferrer'); setMenu(null) }}>Open in new tab</MenuItem>}
            <MenuItem icon={<Icon.Copy size={14} />} onClick={() => { navigator.clipboard?.writeText(menu.href); setMenu(null) }}>Copy link</MenuItem>
            <div className="mt-1 border-t px-3 pt-1.5 text-[11px] text-muted">Through DBCanvas's own port — no port forward needed.</div>
          </div>
        </>
      )}
    </Ctx.Provider>
  )
}

function MenuItem({ icon, onClick, hint, children }) {
  return (
    <button onClick={onClick} className="flex w-full items-start gap-2 px-3 py-1.5 text-left hover:bg-surface2">
      <span className="mt-0.5 text-muted">{icon}</span>
      <span className="min-w-0">
        <span className="block">{children}</span>
        {hint && <span className="block text-[11px] text-muted">{hint}</span>}
      </span>
    </button>
  )
}

// BrowserWindow is one proxied page in a floating frame. The frame is same-origin, so
// its history and address are readable for the toolbar.
function BrowserWindow({ win, src, viewOnly, driving, following, canClose, onNav, onClose, onPatch }) {
  const frame = useRef(null)
  const [path, setPath] = useState('')
  const [edit, setEdit] = useState(null)
  const prefix = src ? src.split('/').slice(0, 3).join('/') : ''
  const lastSent = useRef('')

  // The window's own address, without the password a VNC link carries.
  const here = () => {
    const l = frame.current.contentWindow.location
    const q = new URLSearchParams(l.search)
    q.delete('password')
    const qs = q.toString()
    return (l.pathname.startsWith(prefix) ? l.pathname.slice(prefix.length) : l.pathname) + (qs ? `?${qs}` : '') + l.hash
  }
  const sync = () => {
    try {
      setPath(here())
      const t = frame.current.contentDocument?.title
      if (t) onPatch({ pageTitle: t })
    } catch { /* navigated somewhere it cannot be read */ }
  }
  const go = (fn) => { try { fn(frame.current.contentWindow) } catch { /* */ } }

  // The driver's moves inside a shared window, sent out; single-page apps change the
  // address without a load event, so it is read once a second.
  useEffect(() => {
    if (!driving || !win.shared || win.kind === 'vnc' || !src) return undefined
    const t = setInterval(() => {
      try {
        const p = here()
        if (p !== lastSent.current) { lastSent.current = p; onNav(p) }
      } catch { /* */ }
    }, 1000)
    return () => clearInterval(t)
  }, [driving, win.shared, win.kind, src]) // eslint-disable-line react-hooks/exhaustive-deps

  // …and followed by everyone else.
  useEffect(() => {
    if (driving || !following || !win.followPath || win.kind === 'vnc' || !src) return
    try {
      if (here() !== win.followPath) frame.current.contentWindow.location.href = prefix + win.followPath
    } catch { /* not loaded yet; the next change will catch up */ }
  }, [win.followPath, following, driving]) // eslint-disable-line react-hooks/exhaustive-deps

  const btn = 'rounded p-1 text-muted hover:bg-surface hover:text-fg disabled:opacity-40'
  const toolbar = (
    <>
      {win.shared && <span className="shrink-0 rounded bg-primary/15 px-1.5 text-[10px] text-primary">shared</span>}
      {viewOnly && <span className="shrink-0 rounded bg-surface px-1.5 text-[10px] text-muted">view only</span>}
      <button className={btn} title="Back" disabled={!src} onClick={() => go((w) => w.history.back())}><Icon.ArrowLeft size={13} /></button>
      <button className={btn} title="Forward" disabled={!src} onClick={() => go((w) => w.history.forward())}><Icon.Arrow size={13} /></button>
      <button className={btn} title="Reload" disabled={!src} onClick={() => go((w) => w.location.reload())}><Icon.Refresh size={13} /></button>
      <form className="min-w-0 flex-1" onSubmit={(e) => {
        e.preventDefault()
        const p = (edit ?? path).trim()
        go((w) => { w.location.href = prefix + (p.startsWith('/') ? p : `/${p}`) })
        setEdit(null)
      }}>
        <input value={edit ?? path} onChange={(e) => setEdit(e.target.value)} onBlur={() => setEdit(null)} disabled={!src}
          title={win.link} className="w-full rounded border bg-bg px-2 py-0.5 font-mono text-[11px]" />
      </form>
      <button className={btn} title="Open in a browser tab (still through DBCanvas)" disabled={!src}
        onClick={() => window.open(prefix + path, '_blank')}><Icon.External size={13} /></button>
    </>
  )
  return (
    <Window id={wmId(win.id)} title={win.pageTitle || win.title} size={WIN_SIZE}
      icon={<Icon.Monitor size={14} />} header={toolbar}
      onClose={onClose} canClose={canClose} closeTitle="Only whoever has control can close a shared window">
      <div data-browser-window className="absolute inset-0 bg-white">
        {win.error ? (
          <div className="flex h-full items-center justify-center bg-bg p-6 text-center text-sm text-muted">
            <div><div className="mb-1 font-medium text-fg">{win.title}</div>{win.error}</div>
          </div>
        ) : src ? (
          <iframe ref={frame} src={src} title={win.title} onLoad={sync} className="absolute inset-0 h-full w-full border-0"
            allow="clipboard-read; clipboard-write; fullscreen" />
        ) : (
          <div className="flex h-full items-center justify-center bg-bg"><div className="h-6 w-6 animate-spin rounded-full border-2 border-surface2 border-t-primary" /></div>
        )}
      </div>
    </Window>
  )
}
