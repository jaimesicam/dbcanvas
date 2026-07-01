import { useEffect, useRef, useState } from 'react'
import { useAuth } from './auth/AuthProvider.jsx'
import { useTheme, THEMES } from './theme/ThemeProvider.jsx'
import { Icon } from './components/Icons.jsx'
import { Badge, Button } from './components/ui.jsx'
import { TerminalProvider } from './terminal/TerminalProvider.jsx'
import { notifApi, relTime } from './lib/notifApi.js'

import Dashboard from './pages/Dashboard.jsx'
import StackDesigner from './pages/StackDesigner.jsx'
import DataGenerator from './pages/DataGenerator.jsx'
import ManageUsers from './pages/ManageUsers.jsx'

const NAV = [
  { id: 'dashboard', label: 'Dashboard', icon: 'Dashboard', page: Dashboard, hint: 'Widgets & live charts' },
  { id: 'stack-designer', label: 'Database Stacks', icon: 'Grid', page: StackDesigner, hint: 'Design & deploy stacks' },
  { id: 'data-generator', label: 'Data Generator', icon: 'Table', page: DataGenerator, hint: 'Generate test data for stack tables' },
]
const ADMIN_NAV = { id: 'users', label: 'Manage Users', icon: 'Users', page: ManageUsers, hint: 'Approve & manage accounts' }

function initials(name) {
  return (name || '?').trim().slice(0, 2).toUpperCase()
}

export default function App() {
  const { user, logout } = useAuth()
  const isAdmin = user?.role === 'admin'
  const nav = isAdmin ? [...NAV, ADMIN_NAV] : NAV

  const [active, setActive] = useState(() => location.hash.replace('#', '') || 'dashboard')
  const [collapsed, setCollapsed] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)

  useEffect(() => {
    if (location.hash.replace('#', '') !== active) location.hash = active
  }, [active])

  useEffect(() => {
    const onHash = () => setActive(location.hash.replace('#', '') || 'dashboard')
    addEventListener('hashchange', onHash)
    return () => removeEventListener('hashchange', onHash)
  }, [])

  useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPaletteOpen((v) => !v)
      } else if (e.key === 'Escape') {
        setPaletteOpen(false)
      }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [])

  const current = nav.find((n) => n.id === active) ?? nav[0]
  const PageComponent = current.page

  return (
    <TerminalProvider>
    <div className="flex h-full bg-bg text-fg">
      <aside className={`flex flex-col border-r bg-surface transition-all ${collapsed ? 'w-[68px]' : 'w-60'}`}>
        <div className="flex items-center gap-2.5 border-b px-4 h-14">
          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-primary text-sm font-bold text-primary-fg">
            D
          </div>
          {!collapsed && (
            <div className="leading-tight">
              <div className="text-sm font-semibold">DBCanvas</div>
              <div className="text-xs text-muted">Database Interaction Lab</div>
            </div>
          )}
        </div>

        <nav className="flex-1 space-y-1 overflow-y-auto p-2">
          {nav.map((n) => {
            const Ico = Icon[n.icon]
            const on = n.id === active
            return (
              <button
                key={n.id}
                onClick={() => setActive(n.id)}
                title={collapsed ? n.label : undefined}
                className={`flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm transition ${
                  on ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'
                }`}
              >
                <Ico size={18} />
                {!collapsed && <span className="font-medium">{n.label}</span>}
              </button>
            )
          })}
        </nav>

        <button
          onClick={() => setCollapsed((v) => !v)}
          className="flex items-center gap-3 border-t px-4 py-3 text-sm text-muted hover:text-fg"
        >
          <span className={`transition-transform ${collapsed ? 'rotate-90' : '-rotate-90'}`}>
            <Icon.Chevron size={18} />
          </span>
          {!collapsed && <span>Collapse</span>}
        </button>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <Topbar
          title={current.label}
          hint={current.hint}
          onSearch={() => setPaletteOpen(true)}
          user={user}
          onLogout={logout}
        />
        <main className="flex-1 overflow-auto p-5">
          <div key={active} className="animate-fade-in">
            <PageComponent />
          </div>
        </main>
      </div>

      {paletteOpen && (
        <CommandPalette
          items={nav}
          onClose={() => setPaletteOpen(false)}
          onPick={(id) => {
            setActive(id)
            setPaletteOpen(false)
          }}
        />
      )}
    </div>
    </TerminalProvider>
  )
}

function Topbar({ title, hint, onSearch, user, onLogout }) {
  return (
    <header className="flex h-14 items-center gap-3 border-b bg-surface px-4">
      <div className="min-w-0">
        <h2 className="truncate text-sm font-semibold">{title}</h2>
        <p className="truncate text-xs text-muted">{hint}</p>
      </div>
      <div className="ml-auto flex items-center gap-2">
        <button
          onClick={onSearch}
          className="flex items-center gap-2 rounded-lg border bg-bg px-3 py-1.5 text-sm text-muted hover:text-fg"
        >
          <Icon.Search size={16} />
          <span className="hidden sm:inline">Search</span>
          <kbd className="hidden rounded bg-surface2 px-1.5 text-xs sm:inline">⌘K</kbd>
        </button>
        <ThemePicker />
        <NotificationBell />
        <AccountMenu user={user} onLogout={onLogout} />
      </div>
    </header>
  )
}

function useOutsideClose(open, setOpen) {
  const ref = useRef(null)
  useEffect(() => {
    if (!open) return
    const onClick = (e) => {
      if (ref.current && !ref.current.contains(e.target)) setOpen(false)
    }
    addEventListener('mousedown', onClick)
    return () => removeEventListener('mousedown', onClick)
  }, [open, setOpen])
  return ref
}

function ThemePicker() {
  const { theme, setTheme } = useTheme()
  const [open, setOpen] = useState(false)
  const ref = useOutsideClose(open, setOpen)
  return (
    <div className="relative" ref={ref}>
      <button onClick={() => setOpen((v) => !v)} className="rounded-lg p-2 text-muted hover:bg-surface2 hover:text-fg">
        <Icon.Sun size={18} />
      </button>
      {open && (
        <div className="absolute right-0 z-20 mt-2 w-44 rounded-lg border bg-surface p-1 shadow-xl">
          {THEMES.map((t) => (
            <button
              key={t.id}
              onClick={() => {
                setTheme(t.id)
                setOpen(false)
              }}
              className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-sm hover:bg-surface2"
            >
              <span className="h-4 w-4 rounded-full border" style={{ background: t.swatch }} />
              <span className="flex-1 text-left">{t.label}</span>
              {theme === t.id && <Icon.Check size={16} />}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function NotificationBell() {
  const [items, setItems] = useState([])
  const [unread, setUnread] = useState(0)
  const [open, setOpen] = useState(false)
  const ref = useOutsideClose(open, setOpen)

  useEffect(() => {
    notifApi.list().then((d) => { setItems(d.items || []); setUnread(d.unread || 0) }).catch(() => {})
  }, [])

  // Live push over SSE; the browser auto-reconnects on error.
  useEffect(() => {
    const es = new EventSource('/api/notifications/stream')
    es.onmessage = (e) => {
      try {
        const n = JSON.parse(e.data)
        setItems((prev) => [n, ...prev].slice(0, 50))
        setUnread((u) => u + 1)
      } catch { /* ignore heartbeats */ }
    }
    return () => es.close()
  }, [])

  const markAll = async () => {
    setUnread(0)
    setItems((prev) => prev.map((n) => (n.readAt ? n : { ...n, readAt: 'now' })))
    await notifApi.markAll().catch(() => {})
  }

  const routeFor = (n) => {
    if ((n.type || '').startsWith('datagen')) return 'data-generator'
    if ((n.type || '').startsWith('user')) return 'users'
    return 'stack-designer'
  }
  const clickItem = async (n) => {
    if (!n.readAt) {
      setUnread((u) => Math.max(0, u - 1))
      setItems((prev) => prev.map((x) => (x.id === n.id ? { ...x, readAt: 'now' } : x)))
      notifApi.markRead(n.id).catch(() => {})
    }
    location.hash = routeFor(n)
    setOpen(false)
  }

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        className="relative rounded-lg p-2 text-muted hover:bg-surface2 hover:text-fg"
        title="Notifications"
      >
        <Icon.Bell size={18} />
        {unread > 0 && (
          <span className="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-danger px-1 text-[10px] font-bold text-white">
            {unread > 99 ? '99+' : unread}
          </span>
        )}
      </button>
      {open && (
        <div className="absolute right-0 z-30 mt-2 w-80 overflow-hidden rounded-lg border bg-surface shadow-2xl">
          <div className="flex items-center justify-between border-b px-3 py-2">
            <span className="text-sm font-semibold">Notifications</span>
            <button onClick={markAll} className="text-xs text-muted hover:text-fg" disabled={unread === 0}>
              Mark all read
            </button>
          </div>
          <div className="max-h-96 overflow-y-auto">
            {items.length === 0 && <div className="px-3 py-8 text-center text-sm text-muted">No notifications yet</div>}
            {items.map((n) => (
              <button
                key={n.id}
                onClick={() => clickItem(n)}
                className={`flex w-full items-start gap-2.5 border-b px-3 py-2.5 text-left last:border-0 hover:bg-surface2 ${n.readAt ? 'opacity-60' : ''}`}
              >
                <span className={`mt-1.5 h-2 w-2 shrink-0 rounded-full ${dotColor(n.severity)}`} />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2">
                    <span className="truncate text-sm font-medium">{n.title}</span>
                    <span className="shrink-0 text-[11px] text-muted">{relTime(n.createdAt)}</span>
                  </span>
                  {n.body && <span className="mt-0.5 block break-words text-xs text-muted">{n.body}</span>}
                </span>
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function dotColor(sev) {
  return { info: 'bg-muted', success: 'bg-primary', warning: 'bg-warning', error: 'bg-danger' }[sev] || 'bg-muted'
}

function AccountMenu({ user, onLogout }) {
  const [open, setOpen] = useState(false)
  const ref = useOutsideClose(open, setOpen)
  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex h-9 w-9 items-center justify-center rounded-full bg-primary/15 text-sm font-semibold text-primary"
      >
        {initials(user?.username)}
      </button>
      {open && (
        <div className="absolute right-0 z-20 mt-2 w-52 rounded-lg border bg-surface p-2 shadow-xl">
          <div className="flex items-center justify-between gap-2 px-1 pb-2">
            <span className="truncate text-sm font-medium">{user?.username}</span>
            <Badge tone={user?.role === 'admin' ? 'primary' : 'muted'}>{user?.role}</Badge>
          </div>
          <Button variant="subtle" size="sm" className="w-full" onClick={onLogout}>
            <Icon.Logout size={16} /> Sign out
          </Button>
        </div>
      )}
    </div>
  )
}

function CommandPalette({ items, onClose, onPick }) {
  const [q, setQ] = useState('')
  const filtered = items.filter((i) => `${i.label} ${i.hint}`.toLowerCase().includes(q.toLowerCase()))

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/40 pt-[12vh]" onMouseDown={onClose}>
      <div
        className="w-full max-w-lg overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-center gap-2 border-b px-3">
          <Icon.Search size={18} />
          <input
            autoFocus
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && filtered[0]) onPick(filtered[0].id)
            }}
            placeholder="Jump to a page…"
            className="w-full bg-transparent py-3 text-sm outline-none placeholder:text-muted"
          />
        </div>
        <div className="max-h-72 overflow-y-auto p-1">
          {filtered.length === 0 && <div className="px-3 py-6 text-center text-sm text-muted">No matches</div>}
          {filtered.map((i) => {
            const Ico = Icon[i.icon]
            return (
              <button
                key={i.id}
                onClick={() => onPick(i.id)}
                className="flex w-full items-center gap-3 rounded-lg px-3 py-2 text-left text-sm hover:bg-surface2"
              >
                <Ico size={18} />
                <span className="font-medium">{i.label}</span>
                <span className="ml-auto text-xs text-muted">{i.hint}</span>
              </button>
            )
          })}
        </div>
      </div>
    </div>
  )
}
