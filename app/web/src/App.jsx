import { useCallback, useEffect, useRef, useState } from 'react'
import { useAuth } from './auth/AuthProvider.jsx'
import { PageVisibleProvider } from './lib/usePolling.jsx'
import { openTab as openTabRule, closeTab as closeTabRule, tabCounts, clampTabs } from './lib/tabs.js'
import { useTheme, LOOKS, THEMES } from './theme/ThemeProvider.jsx'
import { SettingsProvider, useSettings } from './settings/SettingsProvider.jsx'
import { Icon } from './components/Icons.jsx'
import { Badge, Button } from './components/ui.jsx'
import { TerminalProvider } from './terminal/TerminalProvider.jsx'
import { notifApi, relTime } from './lib/notifApi.js'

import Dashboard from './pages/Dashboard.jsx'
import OperatorSummary from './pages/OperatorSummary.jsx'
import StackDesigner from './pages/StackDesigner.jsx'
import DataGenerator from './pages/DataGenerator.jsx'
import QueryRunner from './pages/QueryRunner.jsx'
import Benchmark from './pages/Benchmark.jsx'
import PacketInspector from './pages/PacketInspector.jsx'
import OperatorDebugger from './pages/OperatorDebugger.jsx'
import CoreDumpAnalyzer from './pages/CoreDumpAnalyzer.jsx'
import StalkSummary from './pages/StalkSummary.jsx'
import LogSummary from './pages/LogSummary.jsx'
import FTDCSummary from './pages/FTDCSummary.jsx'
import ManageUsers from './pages/ManageUsers.jsx'
import Settings from './pages/Settings.jsx'
import Labs from './pages/Labs.jsx'
import Api from './pages/Api.jsx'
import { showExperimental, visible } from './lib/experimental.js'

// Exported for the smoke suite: the tags on these entries are what an installation
// with EXPERIMENTAL off never sees, and a tag is easy to lose in an edit.
export const NAV = [
  { id: 'dashboard', label: 'Dashboard', icon: 'Dashboard', page: Dashboard, hint: 'Widgets & live charts' },
  { id: 'stack-designer', label: 'Database Stacks', icon: 'Stacks', page: StackDesigner, hint: 'Design & deploy stacks' },
  { id: 'data-generator', label: 'Data Generator', icon: 'Table', page: DataGenerator, hint: 'Generate test data for stack tables' },
  { id: 'queryrun', label: 'Query Runner', icon: 'Database', page: QueryRunner, hint: 'Run parallel queries with processlist gating' },
  { id: 'benchmark', label: 'Benchmark', icon: 'Monitor', page: Benchmark, hint: 'OLTP/OLAP/RW/RO throughput + latency' },
  { id: 'packet-inspector', label: 'Packet Inspector', icon: 'Packet', page: PacketInspector, hint: 'tcpdump on a database node, decoded packet by packet — MySQL, PostgreSQL, MongoDB, Valkey' },
  { id: 'operator-debugger', label: 'Operator Debugger', icon: 'Bug', page: OperatorDebugger, hint: 'Step through a Kubernetes operator under Delve — breakpoints, stack, variables' },
  { id: 'core-dump', label: 'Core Dump Analyzer', icon: 'CoreDump', page: CoreDumpAnalyzer, hint: "Read a crashed server's core dump — threads, stack and arguments, without touching the server" },
  { id: 'stalk-summary', label: 'Stalk Summary', icon: 'Monitor', page: StalkSummary, hint: 'Charts from a pt-stalk archive' },
  { id: 'log-summary', label: 'Log Summary', icon: 'Logs', page: LogSummary, hint: "Several nodes' logs on one timeline — the good, the warning and the bad" },
  { id: 'ftdc-summary', label: 'FTDC Summary', icon: 'Monitor', page: FTDCSummary, hint: "MongoDB's diagnostic.data — the black box every mongod already writes" },
  { id: 'operator-summary', label: 'Operator Summary', icon: 'Kubernetes', page: OperatorSummary, hint: 'A pt-k8s-debug-collector cluster-dump, distilled — what is not running, and what the operator says about it' },
  // Tagged experimental, so it is in the menu only where EXPERIMENTAL is on (see
  // lib/experimental.js). Its scenarios are LLM-written and still moving, which is
  // what the label used to say and the flag now decides.
  { id: 'labs', label: 'Labs (experimental)', icon: 'Flask', page: Labs, experimental: true, hint: 'Hands-on scenarios with real check-work verification' },
  { id: 'api', label: 'API', icon: 'Code', page: Api, hint: 'Every endpoint, and the tokens that authenticate against them' },
  { id: 'settings', label: 'Settings', icon: 'Settings', page: Settings, hint: 'Terminal & appearance preferences' },
]
const ADMIN_NAV = { id: 'users', label: 'Manage Users', icon: 'Users', page: ManageUsers, hint: 'Approve & manage accounts' }

function initials(name) {
  return (name || '?').trim().slice(0, 2).toUpperCase()
}

// App mounts the providers and Workspace is what lives inside them. They are two
// components rather than one because a component cannot use a context it renders
// itself: the tab cap is a per-user setting, and the tab state that enforces it
// is here.
export default function App() {
  return (
    <SettingsProvider>
      <TerminalProvider>
        <Workspace />
      </TerminalProvider>
    </SettingsProvider>
  )
}

function Workspace() {
  const { user, logout } = useAuth()
  const { settings, system } = useSettings()
  const isAdmin = user?.role === 'admin'
  // One filtered list, and everything downstream reads it: the sidebar, the tab
  // strip's labels, the pages the tabs mount, and the command palette. A hidden
  // page is therefore not reachable by #hash either — the lookup falls through to
  // the dashboard — which is the point of hiding it.
  const nav = visible(isAdmin ? [...NAV, ADMIN_NAV] : NAV, showExperimental(system))

  // Tabs. A tab is { key, id }: the id says which page, the key is what makes two
  // tabs of the *same* page possible — three benchmarks at once was the case that
  // asked for this, and it needs the key to be identity rather than the page name.
  //
  // Open tabs stay mounted and are hidden rather than unmounted, which is the
  // whole point: a page keeps its scroll position, its filters and its in-flight
  // view. That is also why lib/usePolling.jsx exists — a kept-alive page would
  // otherwise keep its timer running behind a tab nobody is looking at.
  const [tabs, setTabs] = useState(() => [{ key: 'tab-1', id: location.hash.replace('#', '') || 'dashboard' }])
  const [activeKey, setActiveKey] = useState('tab-1')
  const [collapsed, setCollapsed] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)
  // What the cap refused, or null. Held as an object rather than a boolean so a
  // second refusal re-arms the auto-dismiss timer instead of being swallowed as
  // "already showing" — clicking twice should not look like the app ignored you.
  const [capped, setCapped] = useState(null)
  const tabSeq = useRef(1)

  // The cap is the user's (Settings → Tabs), clamped here as well as on the
  // server: this reads a value that may still be the provider's default while the
  // account's settings are in flight.
  const maxTabs = clampTabs(settings.maxTabs)

  const activeTab = tabs.find((t) => t.key === activeKey) ?? tabs[0]
  const active = activeTab?.id ?? 'dashboard'

  // The rules live in lib/tabs.js so they can be tested; this wires them to state.
  // Updaters stay pure: React may call a setState updater twice, so the key is
  // allocated here rather than inside one.
  const openTab = useCallback((id, force = false) => {
    const key = `tab-${tabSeq.current + 1}`
    const next = openTabRule(tabs, activeKey, id, key, force, maxTabs)
    if (next.tabs !== tabs) {
      tabSeq.current += 1
      setTabs(next.tabs)
    }
    setActiveKey(next.activeKey)
    setCapped(next.capped ? { at: Date.now(), max: maxTabs } : null)
  }, [tabs, activeKey, maxTabs])

  const closeTab = useCallback((key) => {
    const next = closeTabRule(tabs, activeKey, key)
    setTabs(next.tabs)
    setActiveKey(next.activeKey)
    // Closing one is the answer to the warning, so it takes the warning with it.
    setCapped(null)
  }, [tabs, activeKey])

  // The notice is transient: it explains one refused click, and a banner that
  // outlives what it is about becomes furniture nobody reads.
  useEffect(() => {
    if (!capped) return
    const t = setTimeout(() => setCapped(null), 8000)
    return () => clearTimeout(t)
  }, [capped])

  // The hash carries the *active* page, not the tab set: existing links keep
  // working, a refresh gives you one tab of what you were looking at, and the
  // arrangement stays ephemeral the way an editor's does.
  useEffect(() => {
    if (location.hash.replace('#', '') !== active) location.hash = active
  }, [active])

  useEffect(() => {
    const onHash = () => openTab(location.hash.replace('#', '') || 'dashboard')
    addEventListener('hashchange', onHash)
    return () => removeEventListener('hashchange', onHash)
  }, [openTab])

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
  // How many tabs each page has open, for the count badge on the nav item.
  const openCount = tabCounts(tabs)

  return (
    <div className="flex h-full bg-bg text-fg">
      <aside className={`flex flex-col border-r bg-surface transition-all ${collapsed ? 'w-[68px]' : 'w-60'}`}>
        <div className="flex items-center gap-2.5 border-b px-4 h-14">
          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-fg">
            <Icon.Brand size={19} />
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
                onClick={() => openTab(n.id)}
                // Right-click opens another tab of the same page. Three benchmarks
                // side by side is the case; the browser menu is not what anyone
                // wants from a nav item.
                onContextMenu={(e) => { e.preventDefault(); openTab(n.id, true) }}
                title={collapsed ? `${n.label} — right-click to open another` : 'Right-click to open another tab'}
                className={`flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm transition ${
                  on ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'
                }`}
              >
                <Ico size={18} />
                {!collapsed && <span className="font-medium">{n.label}</span>}
                {!collapsed && openCount[n.id] > 1 && (
                  <span className="ml-auto rounded bg-surface2 px-1.5 text-[10px] tabular-nums text-muted">{openCount[n.id]}</span>
                )}
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
        {tabs.length > 1 && (
          // The tabs WRAP onto further rows rather than scrolling sideways. With a
          // cap of twenty a single row always overflows, and a tab scrolled off
          // the edge is a tab you forget is running a benchmark — the point of the
          // strip is that the whole set is there at a glance.
          //
          // And it grows rather than capping its height: a max-height would put
          // the rows behind a scrollbar, which is the problem again one axis over,
          // and it sliced the last visible row through the middle of the tabs. The
          // height is already bounded by something better — the tab limit itself,
          // which is the user's own setting and is what the counter reports.
          //
          // Two boxes rather than one: the tabs wrap, the counter does not — inside,
          // it would be carried off by the wrapping exactly when it has something
          // to say.
          <div className="flex shrink-0 items-stretch border-b bg-surface">
          <div className="flex min-w-0 flex-1 flex-wrap content-start items-stretch gap-1 px-2 pt-1.5">
            {tabs.map((t) => {
              const meta = nav.find((n) => n.id === t.id) ?? nav[0]
              const Ico = Icon[meta.icon]
              const on = t.key === activeKey
              return (
                <div
                  key={t.key}
                  onClick={() => setActiveKey(t.key)}
                  // Middle-click closes, the way every tab strip does.
                  onAuxClick={(e) => { if (e.button === 1) { e.preventDefault(); closeTab(t.key) } }}
                  className={`group flex cursor-pointer items-center gap-1.5 rounded-t-lg border-b-2 px-2.5 py-1.5 text-xs transition ${
                    on ? 'border-primary bg-bg font-medium text-fg' : 'border-transparent text-muted hover:bg-surface2 hover:text-fg'
                  }`}
                >
                  <Ico size={14} />
                  <span className="max-w-[9rem] truncate">{meta.label}</span>
                  <button
                    onClick={(e) => { e.stopPropagation(); closeTab(t.key) }}
                    title="Close tab"
                    className="ml-0.5 rounded px-1 text-muted opacity-0 transition group-hover:opacity-100 hover:bg-surface2 hover:text-fg"
                  >
                    ✕
                  </button>
                </div>
              )
            })}
          </div>
          <TabCount open={tabs.length} max={maxTabs} />
          </div>
        )}

        {capped && <TabCapNotice max={capped.max} onDismiss={() => setCapped(null)} />}
        <main className="flex-1 overflow-auto p-5">
          {/* Every open tab stays mounted and is hidden rather than unmounted, so a
              page keeps its scroll position, its filters and its in-flight view.
              PageVisibleProvider is what stops a hidden one from polling. */}
          {tabs.map((t) => {
            const meta = nav.find((n) => n.id === t.id) ?? nav[0]
            const Page = meta.page
            const on = t.key === activeKey
            return (
              <div key={t.key} className={on ? 'animate-fade-in' : 'hidden'} aria-hidden={!on}>
                <PageVisibleProvider visible={on}>
                  <Page />
                </PageVisibleProvider>
              </div>
            )
          })}
        </main>
      </div>

      {paletteOpen && (
        <CommandPalette
          items={nav}
          onClose={() => setPaletteOpen(false)}
          onPick={(id) => {
            openTab(id)
            setPaletteOpen(false)
          }}
        />
      )}
    </div>
  )
}

// TabCount is the running total, shown only once it is close enough to matter.
// Always on, it would be noise at two tabs; the job is to warn BEFORE a click
// goes nowhere, so the last few are the ones worth counting.
export function TabCount({ open, max }) {
  if (open < max - 2) return null
  return (
    <span
      title={`${open} of ${max} tabs open. The limit is in Settings → Tabs.`}
      className={`flex shrink-0 items-center whitespace-nowrap border-l px-2.5 text-[10px] tabular-nums ${
        open >= max ? 'font-medium text-warning' : 'text-muted'
      }`}
    >
      {open}/{max} tabs
    </span>
  )
}

// TabCapNotice explains a click that did nothing. Refusing in silence was the
// earlier behaviour and it reads as a broken sidebar — you click a page, nothing
// happens, and nothing on screen says why. So it gives the number, the way out,
// and where the number lives. It is transient (App clears it after a few seconds,
// and closing a tab clears it at once), because a banner that outlives what it is
// about becomes furniture nobody reads.
export function TabCapNotice({ max, onDismiss }) {
  return (
    <div className="flex shrink-0 items-start gap-2 border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
      <span className="mt-px shrink-0"><Icon.StatusWarn size={14} /></span>
      <span className="min-w-0">
        All {max} tabs are already open, so that page did not open. Close a tab first — or raise
        the limit in <b>Settings → Tabs</b>.
      </span>
      <button
        onClick={onDismiss}
        title="Dismiss"
        className="ml-auto shrink-0 rounded px-1.5 hover:bg-warning/20"
      >
        ✕
      </button>
    </div>
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
        <AppearancePicker />
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

// AppearancePicker is the quick switch for both halves of the appearance: the
// colour THEME and the LOOK (the shape — see index.css). Both save as the
// account's preference (Settings → Theme / Look), so this menu and the Settings
// page can't disagree. A failed save is not worth interrupting the user for: the
// choice still applies locally, and the Settings page reports save errors.
function AppearancePicker() {
  const { theme, setTheme, look, setLook } = useTheme()
  const { save } = useSettings()
  const [open, setOpen] = useState(false)
  const ref = useOutsideClose(open, setOpen)

  // Apply first, then persist — the UI must not wait on the network to repaint.
  const pick = (apply, patch) => {
    apply()
    save(patch).catch(() => { /* applied locally; Settings surfaces failures */ })
    setOpen(false)
  }

  return (
    <div className="relative" ref={ref}>
      <button onClick={() => setOpen((v) => !v)} title="Appearance"
        className="rounded-lg p-2 text-muted hover:bg-surface2 hover:text-fg">
        <Icon.Sun size={18} />
      </button>
      {open && (
        <div className="absolute right-0 z-20 mt-2 w-48 rounded-lg border bg-surface p-1 shadow-xl">
          <div className="px-2.5 pt-1.5 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Theme</div>
          {THEMES.map((t) => (
            <button
              key={t.id}
              onClick={() => pick(() => setTheme(t.id), { theme: t.id })}
              className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-sm hover:bg-surface2"
            >
              <span className="h-4 w-4 rounded-full border" style={{ background: t.swatch }} />
              <span className="flex-1 text-left">{t.label}</span>
              {theme === t.id && <Icon.Check size={16} />}
            </button>
          ))}
          <div className="mt-1 border-t px-2.5 pt-2 pb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">Look</div>
          {LOOKS.map((l) => (
            <button
              key={l.id}
              onClick={() => pick(() => setLook(l.id), { look: l.id })}
              className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-sm hover:bg-surface2"
            >
              {/* The chip is drawn in the look it offers, so the row shows its
                  corner radius and border weight rather than describing them. Its
                  size is in px rather than on the spacing scale, which the look
                  also moves — four chips of four sizes would read as a mistake. */}
              <span data-look={l.id} className="h-[16px] w-[16px] shrink-0 rounded-md border bg-surface2" />
              <span className="flex-1 text-left">{l.label}</span>
              {look === l.id && <Icon.Check size={16} />}
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
