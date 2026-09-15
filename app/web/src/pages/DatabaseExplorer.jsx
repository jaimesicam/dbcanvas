import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Badge, Button, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Help } from '../components/Tooltip.jsx'
import { DbxGrid, ValueViewer } from '../components/DbxGrid.jsx'
import { DbxChart } from '../components/DbxChart.jsx'
import { useHandoff, sendHandoff } from '../lib/handoff.js'
import {
  ENGINE_LABEL, KIND_ICON, cellText, copyText, dbxApi, fmtBytes, fmtDuration,
  fmtNumber, formatSQL, isDestructiveSQL, isNull, prettyJson, statementAt,
} from '../lib/dbxApi.js'

// Database Explorer — browse and query the databases DBCanvas deployed.
//
// The layout is an IDE's because the work is an IDE's: a tree of what exists on the
// left, tabs of what you are doing on the right, and a result pane under the editor.
// Both splits drag, because "how much of the screen is the result" is a question that
// changes several times a minute and has no good default.
//
// Nothing on this page holds a credential. A connection is an id the server gave us;
// every request sends that id back and the server re-resolves it against the caller's
// own stacks. There is no host, user or password field anywhere in this file, and no
// response it reads has one in it.
//
// See docs/DATABASE_EXPLORER.md. The Query Runner (#queryrun) is the other half of
// the pair and stays what it is — concurrency, gating, workload — with an
// "Open in Query Runner" hand-off from the editor here.

const DEFAULT_LIMIT = 500
const LIMITS = [100, 500, 1000, 5000, 10000, 50000]

let tabSeq = 0
const newTab = (patch = {}) => {
  tabSeq += 1
  return {
    key: `q${tabSeq}`,
    title: `Query ${tabSeq}`,
    connectionId: '',
    database: '',
    schema: '',
    sql: '',
    // MongoDB's query is not a string, so it is not stored as one.
    mongo: { collection: '', operation: 'find', filter: '{}', projection: '', sort: '', pipeline: '[\n  { "$match": {} }\n]', skip: 0 },
    command: '',
    limit: DEFAULT_LIMIT,
    result: null,
    running: false,
    queryId: '',
    view: 'results',
    // armed lifts a connection's read-only policy for this tab only, and only where
    // an administrator has allowed it at all. Per tab and not per connection: the
    // whole point is that arming is a deliberate act you can see, next to the editor
    // you are about to press Run in.
    armed: false,
    object: null, // when the tab was opened from an object: {ref, detail}
    ...patch,
  }
}

export default function DatabaseExplorer() {
  const [conns, setConns] = useState(null)
  const [connErr, setConnErr] = useState('')
  const [pmmWarning, setPmmWarning] = useState('')
  const [tabs, setTabs] = useState(() => [newTab()])
  const [active, setActive] = useState(() => 'q1')
  const [treeW, setTreeW] = useState(300)
  const [editorH, setEditorH] = useState(220)
  const [viewer, setViewer] = useState(null)
  const [history, setHistory] = useState([])
  const [saved, setSaved] = useState([])
  const [side, setSide] = useState('tree') // tree | history | saved
  const [editRow, setEditRow] = useState(null)
  const [toast, setToast] = useState('')

  const tab = tabs.find((t) => t.key === active) || tabs[0]
  const patchTab = useCallback((key, patch) => {
    setTabs((ts) => ts.map((t) => (t.key === key ? { ...t, ...patch } : t)))
  }, [])

  const flatConns = useMemo(() => {
    const out = []
    for (const s of conns || []) for (const g of s.groups || []) for (const c of g.connections || []) out.push(c)
    return out
  }, [conns])
  const conn = flatConns.find((c) => c.id === tab.connectionId) || null

  const load = useCallback(async () => {
    try {
      const d = await dbxApi.connections()
      setConns(d.stacks || [])
      setPmmWarning(d.pmmWarning || '')
      setConnErr('')
    } catch (e) {
      setConnErr(e.message)
      setConns([])
    }
  }, [])

  useEffect(() => { load() }, [load])
  useEffect(() => {
    dbxApi.history(200).then(setHistory).catch(() => {})
    dbxApi.saved().then(setSaved).catch(() => {})
  }, [])

  // "Open in Database Explorer" from a node panel elsewhere in the app.
  useHandoff('dbx-open', (raw) => {
    try {
      const h = JSON.parse(raw)
      if (h.connectionId) openTab({ connectionId: h.connectionId, database: h.database || '', sql: h.sql || '' })
    } catch { /* a malformed hand-off is not worth an error banner */ }
  })

  const say = (m) => { setToast(m); setTimeout(() => setToast(''), 2500) }

  const openTab = (patch) => {
    const t = newTab(patch)
    setTabs((ts) => [...ts, t])
    setActive(t.key)
    return t
  }
  const closeTab = (key) => {
    setTabs((ts) => {
      const next = ts.filter((t) => t.key !== key)
      const list = next.length ? next : [newTab()]
      if (key === active) setActive(list[Math.max(0, ts.findIndex((t) => t.key === key) - 1)]?.key || list[0].key)
      return list
    })
  }

  const refreshHistory = () => dbxApi.history(200).then(setHistory).catch(() => {})

  // run submits the current tab. The engine decides the shape of the request; the
  // server decides everything else, including the ceiling.
  const run = useCallback(async (t, override) => {
    if (!t.connectionId) { say('Pick a connection first.'); return }
    const c = flatConns.find((x) => x.id === t.connectionId)
    const engine = c ? c.engine : 'mysql'
    const queryId = `q-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
    const req = {
      connectionId: t.connectionId, queryId, database: t.database, schema: t.schema,
      limit: Number(t.limit) || DEFAULT_LIMIT, explain: !!(override && override.explain),
      // Only ever sent when the tab was armed. The server checks the administrator
      // setting as well, so this alone cannot unlock anything.
      allowWrites: !!(t.armed && c && c.unlockable),
    }
    if (engine === 'mongodb') {
      req.mongo = {
        collection: t.mongo.collection, operation: t.mongo.operation,
        filter: safeJSON(t.mongo.filter), projection: safeJSON(t.mongo.projection),
        sort: safeJSON(t.mongo.sort), pipeline: safeJSON(t.mongo.pipeline, '[]'),
        skip: Number(t.mongo.skip) || 0,
      }
    } else if (engine === 'valkey') {
      req.command = (override && override.sql) || t.command
    } else {
      req.sql = (override && override.sql) || t.sql
      if (!req.sql.trim()) { say('Nothing to run.'); return }
    }
    patchTab(t.key, { running: true, queryId, view: override && override.explain ? 'explain' : 'results' })
    try {
      const res = await dbxApi.query(req)
      patchTab(t.key, { running: false, result: res, queryId: '' })
    } catch (e) {
      patchTab(t.key, { running: false, queryId: '', result: { engine, error: { engine, message: e.message, display: e.message } } })
    }
    refreshHistory()
  }, [flatConns, patchTab])

  const cancel = async (t) => {
    if (!t.queryId) return
    await dbxApi.cancel(t.queryId).catch(() => {})
  }

  // Opening an object: one call that both describes it and fetches its first page,
  // which is what a click on a table should cost.
  const openObject = async (c, ref, arm = '') => {
    const t = openTab({
      connectionId: c.id, database: ref.database, schema: ref.schema,
      title: ref.name, object: { ref, detail: null }, view: 'overview',
    })
    try {
      const [detail, data] = await Promise.all([
        dbxApi.object(c.id, { database: ref.database, schema: ref.schema, name: ref.name, kind: ref.kind, allowWrites: arm }),
        dbxApi.viewData(c.id, { database: ref.database, schema: ref.schema, name: ref.name, kind: ref.kind, limit: DEFAULT_LIMIT, allowWrites: arm }),
      ])
      patchTab(t.key, {
        object: { ref, detail: detail && detail.error ? null : detail, error: detail && detail.error },
        result: data.result,
        sql: data.request && data.request.sql ? data.request.sql : '',
        mongo: data.request && data.request.mongo && data.request.mongo.collection
          ? { ...t.mongo, collection: data.request.mongo.collection, operation: data.request.mongo.operation || 'find', filter: '{}' }
          : t.mongo,
        command: c.engine === 'valkey' ? `GET ${ref.name}` : '',
      })
    } catch (e) {
      patchTab(t.key, { object: { ref, detail: null, error: { message: e.message, display: e.message } } })
    }
    refreshHistory()
  }

  // Re-describe a tab's object after its arm changes: whether a row can be edited is
  // part of what the server says about an object, and on an armed connection the
  // answer differs. A re-fetch rather than a local flag, because the identity rule
  // behind it is the server's to apply.
  const redescribe = useCallback(async (t) => {
    if (!t.object || !t.connectionId) return
    const ref = t.object.ref
    try {
      const detail = await dbxApi.object(t.connectionId, {
        database: ref.database, schema: ref.schema, name: ref.name, kind: ref.kind,
        allowWrites: t.armed ? '1' : '',
      })
      if (!detail.error) patchTab(t.key, { object: { ...t.object, detail } })
    } catch { /* the banner already said what the connection is */ }
  }, [patchTab])

  return (
    <div className="flex h-full min-h-0 flex-col gap-0 overflow-hidden rounded-xl border bg-surface">
      <Header
        conn={conn}
        onRefresh={load}
        side={side}
        setSide={setSide}
      />
      {connErr && (
        <div className="flex items-center gap-2 border-b border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
          <Icon.Bell size={15} /> Could not load connections: {connErr}
        </div>
      )}

      <div className="flex min-h-0 flex-1">
        <div className="flex min-h-0 shrink-0 flex-col border-r" style={{ width: treeW }}>
          {side === 'tree' && (
            <ConnectionTree
              stacks={conns} pmmWarning={pmmWarning}
              onRefresh={load}
              onPickConnection={(c) => patchTab(tab.key, { connectionId: c.id, database: '', schema: '', title: c.label })}
              onOpenObject={openObject}
              onNewQuery={(c, database, schema) => openTab({ connectionId: c.id, database: database || '', schema: schema || '', title: `${c.label}` })}
              activeConnId={tab.connectionId}
            />
          )}
          {side === 'history' && (
            <HistoryPanel
              entries={history}
              onRerun={(h) => openTab({ connectionId: h.connectionId, database: h.database, schema: h.schema, sql: h.statement, command: h.statement, title: h.connection })}
              onDelete={async (id) => { await dbxApi.deleteHistory(id); refreshHistory() }}
              onClear={async () => { await dbxApi.clearHistory(); refreshHistory() }}
            />
          )}
          {side === 'saved' && (
            <SavedPanel
              items={saved}
              onOpen={(q) => openTab({ sql: q.statement, command: q.statement, database: q.database || '', title: q.name })}
              onDelete={async (id) => { await dbxApi.deleteSaved(id); dbxApi.saved().then(setSaved) }}
            />
          )}
        </div>

        <Splitter onDrag={(dx) => setTreeW((w) => Math.min(620, Math.max(200, w + dx)))} />

        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
          <TabStrip tabs={tabs} active={active} setActive={setActive} onClose={closeTab} onNew={() => openTab({})} />
          {tab && (
            <TabBody
              key={tab.key}
              tab={tab} conn={conn} conns={flatConns}
              editorH={editorH} setEditorH={setEditorH}
              patch={(p) => patchTab(tab.key, p)}
              onArmChange={(armed) => redescribe({ ...tab, armed })}
              onRun={(o) => run(tab, o)}
              onCancel={() => cancel(tab)}
              onOpenValue={setViewer}
              onSave={async (q) => { await dbxApi.save(q); dbxApi.saved().then(setSaved); say('Saved.') }}
              onEditRow={setEditRow}
              say={say}
            />
          )}
        </div>
      </div>

      <ValueViewer open={viewer} onClose={() => setViewer(null)} />
      {editRow && (
        <RowEditor
          state={editRow}
          onClose={() => setEditRow(null)}
          onDone={(msg) => { setEditRow(null); say(msg); run(tab) }}
        />
      )}
      {toast && (
        <div className="pointer-events-none fixed bottom-6 left-1/2 z-50 -translate-x-1/2 rounded-lg border bg-surface px-3 py-2 text-sm shadow-lg">
          {toast}
        </div>
      )}
    </div>
  )
}

function safeJSON(s, fallback = '{}') {
  const t = (s || '').trim()
  if (!t) return fallback
  return t
}

function Header({ conn, onRefresh, side, setSide }) {
  const pill = (id, label, icon) => (
    <button
      onClick={() => setSide(id)}
      className={`flex items-center gap-1 rounded-md px-2 py-1 text-xs ${side === id ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2'}`}
    >
      {icon}{label}
    </button>
  )
  return (
    <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2">
      <Icon.Database size={16} className="text-primary" />
      <span className="text-sm font-semibold">Database Explorer</span>
      <span className="hidden text-xs text-muted sm:inline">
        browse and query the databases on your canvas — no credentials, no published ports
      </span>
      <div className="flex-1" />
      {conn && <Breadcrumbs conn={conn} />}
      <div className="flex items-center gap-1 rounded-lg border p-0.5">
        {pill('tree', 'Connections', <Icon.Stacks size={12} />)}
        {pill('history', 'History', <Icon.Timeline size={12} />)}
        {pill('saved', 'Saved', <Icon.Pin size={12} />)}
      </div>
      <button onClick={onRefresh} className="rounded-md border px-2 py-1 text-xs hover:bg-surface2" title="Re-read the connection list">
        <Icon.Check size={12} className="inline" /> Refresh
      </button>
    </div>
  )
}

function Breadcrumbs({ conn }) {
  return (
    <div className="flex min-w-0 items-center gap-1.5 text-xs text-muted">
      <span className="truncate">{conn.stackName}</span>
      <Icon.Chevron size={11} />
      <span className="truncate text-fg">{conn.label}</span>
      <Badge tone={conn.readOnly ? 'warning' : 'muted'}>{ENGINE_LABEL[conn.engine] || conn.engine}</Badge>
      {conn.readOnly && <Badge tone="warning">read only</Badge>}
      {conn.transport === 'exec' && (
        <span title="This database listens on loopback inside its container, so DBCanvas runs its client in there rather than dialling it.">
          <Badge tone="accent">in-container</Badge>
        </span>
      )}
    </div>
  )
}

// Splitter is a drag handle between two panes. Pointer events rather than mouse, so
// it works on a trackpad and a touchscreen alike.
function Splitter({ onDrag, horizontal = false }) {
  const last = useRef(null)
  const down = (e) => {
    e.preventDefault()
    last.current = horizontal ? e.clientY : e.clientX
    const move = (ev) => {
      const now = horizontal ? ev.clientY : ev.clientX
      onDrag(now - last.current)
      last.current = now
    }
    const up = () => { removeEventListener('pointermove', move); removeEventListener('pointerup', up) }
    addEventListener('pointermove', move)
    addEventListener('pointerup', up)
  }
  return (
    <div
      onPointerDown={down}
      className={horizontal
        ? 'h-1.5 shrink-0 cursor-row-resize bg-border/40 hover:bg-primary/40'
        : 'w-1.5 shrink-0 cursor-col-resize bg-border/40 hover:bg-primary/40'}
    />
  )
}

// ------------------------------------------------------------------ the tree

function ConnectionTree({ stacks, pmmWarning, onPickConnection, onOpenObject, onNewQuery, activeConnId, onRefresh }) {
  const [open, setOpen] = useState(() => new Set())
  const [filter, setFilter] = useState('')
  const toggle = (id) => setOpen((o) => {
    const n = new Set(o)
    if (n.has(id)) n.delete(id); else n.add(id)
    return n
  })

  if (stacks === null) return <TreeSkeleton />
  if (!stacks.length) {
    return (
      <div className="p-6 text-center text-sm text-muted">
        <Icon.Database size={22} className="mx-auto mb-2 opacity-40" />
        <p className="font-medium text-fg">No running databases</p>
        <p className="mt-1">Deploy a stack from <b>Database Stacks</b> and it will appear here — no credentials to enter.</p>
      </div>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="border-b p-2">
        <div className="relative flex items-center">
          <Icon.Search size={13} className="pointer-events-none absolute left-2 text-muted" />
          <input
            value={filter} onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter connections…"
            className="ui-input w-full rounded-md border bg-bg py-1 pl-7 pr-2 text-xs outline-none focus:border-primary"
          />
        </div>
      </div>
      <div className="min-h-0 flex-1 overflow-auto p-1 text-sm">
        {stacks.map((s) => (
          <div key={s.stackId} className="mb-1">
            <Row
              depth={0} icon="Stacks" label={s.stackName} badge={`${s.connections}`}
              open={!open.has(`s${s.stackId}`)} onToggle={() => toggle(`s${s.stackId}`)}
            />
            {!open.has(`s${s.stackId}`) && s.groups.map((g) => (
              <div key={g.name}>
                <Row depth={1} icon={g.engine === 'clickhouse' ? 'Monitor' : 'Database'} label={g.name} plain />
                {g.connections
                  .filter((c) => !filter || c.label.toLowerCase().includes(filter.toLowerCase()))
                  .map((c) => (
                    <ConnectionNode
                      key={c.id} conn={c} depth={2} open={open} toggle={toggle}
                      onPick={onPickConnection} onOpenObject={onOpenObject} onNewQuery={onNewQuery}
                      active={c.id === activeConnId} pmmWarning={pmmWarning} onRefresh={onRefresh}
                    />
                  ))}
              </div>
            ))}
          </div>
        ))}
      </div>
    </div>
  )
}

function TreeSkeleton() {
  return (
    <div className="space-y-2 p-3">
      {[0, 1, 2, 3, 4, 5].map((i) => (
        <div key={i} className="h-5 animate-pulse rounded bg-surface2" style={{ width: `${90 - i * 9}%`, marginLeft: (i % 3) * 12 }} />
      ))}
    </div>
  )
}

function Row({ depth = 0, icon, label, detail, badge, open, onToggle, onClick, onContext, active, plain, tone }) {
  const Ico = Icon[icon] || Icon.File
  return (
    <div
      onClick={onClick || onToggle}
      onContextMenu={onContext}
      className={`group flex cursor-pointer items-center gap-1.5 rounded px-1.5 py-1 ${
        active ? 'bg-primary/15 text-primary' : 'hover:bg-surface2'
      } ${plain ? 'cursor-default text-[11px] uppercase tracking-wide text-muted hover:bg-transparent' : ''}`}
      style={{ paddingLeft: 6 + depth * 12 }}
      title={detail || label}
    >
      {onToggle && !plain ? (
        <span className={`shrink-0 text-muted transition-transform ${open ? 'rotate-90' : ''}`}><Icon.Chevron size={11} /></span>
      ) : <span className="w-[11px] shrink-0" />}
      <Ico size={13} className={`shrink-0 ${tone || 'text-muted'}`} />
      <span className="min-w-0 flex-1 truncate">{label}</span>
      {badge && <span className="shrink-0 text-[10px] tabular-nums text-muted">{badge}</span>}
    </div>
  )
}

// ExposeControl gives a ClusterIP database an address the Query Runner and the
// Benchmark can dial, and takes it away again.
//
// It is in the tree rather than in a settings page because that is where the reason
// for it appears: the connection says it has no address outside the cluster, and this
// is the answer sitting next to the sentence. Nothing the operator owns is touched —
// a second Service is created beside its own — which is why removing it is safe and
// why the label says what it does.
function ExposeControl({ conn, onDone }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const exposed = !!conn.exposedBy

  const act = async () => {
    setBusy(true); setMsg('')
    try {
      const r = exposed ? await dbxApi.unexpose(conn.id) : await dbxApi.expose(conn.id)
      setMsg(r.message || 'done')
      if (onDone) onDone()
    } catch (e) {
      setMsg(e.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <button
        onClick={act}
        disabled={busy}
        title={exposed
          ? `Remove ${conn.exposedBy}, the Service DBCanvas added. The operator's own Service is untouched either way.`
          : 'Add a Service beside the operator\u2019s own so the Query Runner and Benchmark can reach this database. Reversible, and it does not modify anything the operator owns.'}
        className={`rounded border px-1.5 py-0.5 text-[10px] disabled:opacity-50 ${
          exposed ? 'border-success/40 text-success hover:bg-success/10' : 'hover:bg-surface2'}`}
      >
        {busy ? 'Working\u2026' : exposed ? 'Exposed \u2014 remove' : 'Expose for tools'}
      </button>
      {msg && <span className="basis-full text-[10px] leading-snug text-muted">{msg}</span>}
    </>
  )
}

function ConnectionNode({ conn, depth, open, toggle, onPick, onOpenObject, onNewQuery, active, pmmWarning, onRefresh }) {
  const id = `c${conn.id}`
  const expanded = open.has(id)
  const [dbs, setDbs] = useState(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    if (!expanded || dbs) return
    dbxApi.databases(conn.id)
      .then((d) => { if (d.error) setErr(d.error.display || d.error.message); else setDbs(d.nodes || []) })
      .catch((e) => setErr(e.message))
  }, [expanded, dbs, conn.id])

  return (
    <div>
      <Row
        depth={depth} icon={conn.engine === 'valkey' ? 'Key' : 'Database'}
        label={conn.label}
        detail={conn.note || conn.host}
        badge={conn.preferred ? '★' : ''}
        open={expanded}
        onToggle={() => { toggle(id); onPick(conn) }}
        active={active}
        tone={conn.readOnly ? 'text-warning' : conn.preferred ? 'text-primary' : 'text-muted'}
      />
      {expanded && (
        <div>
          <div className="px-2 py-1" style={{ paddingLeft: 6 + (depth + 1) * 12 }}>
            <div className="flex flex-wrap items-center gap-1 text-[10px] text-muted">
              <span className="font-mono">{conn.host}:{conn.port}</span>
              {conn.user && <span>· as {conn.user}</span>}
              {conn.version && <span>· {conn.version}</span>}
            </div>
            {conn.note && <p className="mt-0.5 text-[10px] leading-snug text-muted">{conn.note}</p>}
            {conn.readOnly && (
              <div className="mt-1 rounded border border-warning/40 bg-warning/10 p-1.5 text-[10px] leading-snug text-warning">
                <b>{conn.product}</b> — {conn.warning || pmmWarning}
              </div>
            )}
            <div className="mt-1 flex flex-wrap items-center gap-1">
              <button
                onClick={() => onNewQuery(conn)}
                className="rounded border px-1.5 py-0.5 text-[10px] hover:bg-surface2"
              >
                New query
              </button>
              {(conn.exposable || conn.exposedBy) && <ExposeControl conn={conn} onDone={onRefresh} />}
            </div>
          </div>
          {err && <div className="px-2 py-1 text-[11px] text-danger" style={{ paddingLeft: 6 + (depth + 1) * 12 }}>{err}</div>}
          {dbs === null && !err && <Row depth={depth + 1} icon="Database" label="Loading…" plain />}
          {(dbs || []).map((d) => (
            <DatabaseNode
              key={d.id} conn={conn} db={d} depth={depth + 1} open={open} toggle={toggle}
              onOpenObject={onOpenObject} onNewQuery={onNewQuery}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function DatabaseNode({ conn, db, depth, open, toggle, onOpenObject, onNewQuery }) {
  const id = `c${conn.id}:d${db.id}`
  const expanded = open.has(id)
  const [kids, setKids] = useState(null)
  const [err, setErr] = useState('')
  const hasSchemas = conn.capabilities && conn.capabilities.schemas

  useEffect(() => {
    if (!expanded || kids) return
    const p = hasSchemas
      ? dbxApi.schemas(conn.id, db.id).then((d) => ({ kind: 'schema', nodes: d.nodes || [], error: d.error }))
      : dbxApi.objects(conn.id, { database: db.id }).then((d) => ({ kind: 'object', nodes: d.nodes || [], folders: d.folders, cursor: d.cursor, more: d.more, error: d.error }))
    p.then((d) => { if (d.error) setErr(d.error.display || d.error.message); else setKids(d) }).catch((e) => setErr(e.message))
  }, [expanded, kids, conn.id, db.id, hasSchemas])

  return (
    <div>
      <Row
        depth={depth} icon={KIND_ICON[db.kind] || 'Database'} label={db.name}
        badge={db.rows != null ? fmtNumber(Number(db.rows)) : db.bytes != null ? fmtBytes(db.bytes) : ''}
        detail={db.detail} open={expanded} onToggle={() => toggle(id)}
        tone={db.system ? 'text-muted/60' : 'text-muted'}
      />
      {expanded && (
        <div>
          {err && <div className="px-2 py-1 text-[11px] text-danger" style={{ paddingLeft: 6 + (depth + 1) * 12 }}>{err}</div>}
          {!kids && !err && <Row depth={depth + 1} icon="Folder" label="Loading…" plain />}
          {kids && kids.kind === 'schema' && kids.nodes.map((s) => (
            <SchemaNode key={s.id} conn={conn} db={db} schema={s} depth={depth + 1} open={open} toggle={toggle} onOpenObject={onOpenObject} />
          ))}
          {kids && kids.kind === 'object' && (
            <ObjectList
              conn={conn} database={db.id} schema="" page={kids} depth={depth + 1}
              open={open} toggle={toggle} onOpenObject={onOpenObject}
            />
          )}
        </div>
      )}
    </div>
  )
}

function SchemaNode({ conn, db, schema, depth, open, toggle, onOpenObject }) {
  const id = `c${conn.id}:d${db.id}:s${schema.id}`
  const expanded = open.has(id)
  const [page, setPage] = useState(null)
  const [err, setErr] = useState('')
  useEffect(() => {
    if (!expanded || page) return
    dbxApi.objects(conn.id, { database: db.id, schema: schema.id })
      .then((d) => { if (d.error) setErr(d.error.display || d.error.message); else setPage(d) })
      .catch((e) => setErr(e.message))
  }, [expanded, page, conn.id, db.id, schema.id])
  return (
    <div>
      <Row depth={depth} icon="Folder" label={schema.name} detail={schema.detail} open={expanded}
        onToggle={() => toggle(id)} tone={schema.system ? 'text-muted/60' : 'text-muted'} />
      {expanded && (
        <div>
          {err && <div className="px-2 py-1 text-[11px] text-danger" style={{ paddingLeft: 6 + (depth + 1) * 12 }}>{err}</div>}
          {!page && !err && <Row depth={depth + 1} icon="Folder" label="Loading…" plain />}
          {page && (
            <ObjectList conn={conn} database={db.id} schema={schema.id} page={page} depth={depth + 1}
              open={open} toggle={toggle} onOpenObject={onOpenObject} />
          )}
        </div>
      )}
    </div>
  )
}

// ObjectList groups a listing under its folders and, for Valkey, keeps a SCAN cursor
// so "more keys" is a button rather than a promise the page cannot keep.
function ObjectList({ conn, database, schema, page, depth, open, toggle, onOpenObject }) {
  const [extra, setExtra] = useState([])
  const [cursor, setCursor] = useState(page.cursor || '')
  const [more, setMore] = useState(!!page.more)
  const [busy, setBusy] = useState(false)
  const [filter, setFilter] = useState('')
  const isValkey = conn.engine === 'valkey'

  const nodes = [...(page.nodes || []), ...extra]
  const folders = page.folders && page.folders.length ? page.folders : [...new Set(nodes.map((n) => n.folder || 'Objects'))]
  const byFolder = folders.map((f) => ({ f, items: nodes.filter((n) => (n.folder || 'Objects') === f) })).filter((g) => g.items.length)

  const loadMore = async () => {
    setBusy(true)
    try {
      const d = await dbxApi.objects(conn.id, { database, schema, cursor, filter })
      setExtra((x) => [...x, ...(d.nodes || [])])
      setCursor(d.cursor || '')
      setMore(!!d.more)
    } finally { setBusy(false) }
  }

  const search = async (term) => {
    setFilter(term)
    setBusy(true)
    try {
      const d = await dbxApi.objects(conn.id, { database, schema, filter: term })
      setExtra([])
      setCursor(d.cursor || '')
      setMore(!!d.more)
      page.nodes = d.nodes || [] // eslint-disable-line no-param-reassign
    } finally { setBusy(false) }
  }

  return (
    <div>
      {isValkey && (
        <div className="px-2 py-1" style={{ paddingLeft: 6 + depth * 12 }}>
          <input
            defaultValue=""
            onKeyDown={(e) => { if (e.key === 'Enter') search(e.currentTarget.value) }}
            placeholder="key prefix, e.g. users: — Enter to SCAN"
            className="ui-input w-full rounded border bg-bg px-2 py-1 text-[11px] outline-none focus:border-primary"
          />
          <p className="mt-0.5 text-[10px] leading-snug text-muted">
            Browsed with <b>SCAN</b>, never <code>KEYS</code> — the keyspace is paged, not loaded.
          </p>
        </div>
      )}
      {byFolder.map(({ f, items }) => {
        const fid = `c${conn.id}:d${database}:s${schema}:f${f}`
        const shut = open.has(fid)
        return (
          <div key={f}>
            <Row depth={depth} icon="Folder" label={f} badge={String(items.length)} open={!shut} onToggle={() => toggle(fid)} />
            {!shut && items.map((n) => (
              <Row
                key={`${n.folder}-${n.id}`} depth={depth + 1} icon={KIND_ICON[n.kind] || 'Table'}
                label={n.name}
                detail={[n.detail, n.badge, n.rows != null ? `~${fmtNumber(Number(n.rows))} rows` : '', n.bytes != null ? fmtBytes(n.bytes) : '']
                  .filter(Boolean).join(' · ')}
                badge={n.rows != null ? `~${fmtNumber(Number(n.rows))}` : n.badge || ''}
                onClick={() => onOpenObject(conn, { database, schema, name: n.name, kind: n.kind })}
                tone={n.system ? 'text-muted/60' : 'text-muted'}
              />
            ))}
          </div>
        )
      })}
      {!nodes.length && <div className="px-2 py-2 text-[11px] text-muted" style={{ paddingLeft: 6 + depth * 12 }}>Nothing here.</div>}
      {more && (
        <div className="px-2 py-1" style={{ paddingLeft: 6 + depth * 12 }}>
          <button onClick={loadMore} disabled={busy} className="rounded border px-2 py-0.5 text-[10px] hover:bg-surface2 disabled:opacity-50">
            {busy ? 'Scanning…' : 'Load more'}
          </button>
        </div>
      )}
    </div>
  )
}

// ------------------------------------------------------------------ tabs

function TabStrip({ tabs, active, setActive, onClose, onNew }) {
  return (
    <div className="flex items-center gap-0.5 overflow-x-auto border-b bg-surface2/50 px-1 py-1">
      {tabs.map((t) => (
        <div
          key={t.key}
          onClick={() => setActive(t.key)}
          className={`group flex shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2.5 py-1 text-xs ${
            t.key === active ? 'bg-surface font-medium shadow-sm' : 'text-muted hover:bg-surface/60'
          }`}
        >
          {t.running && <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-primary" />}
          <span className="max-w-[160px] truncate">{t.title}</span>
          <button
            onClick={(e) => { e.stopPropagation(); onClose(t.key) }}
            className="opacity-0 transition group-hover:opacity-100 hover:text-danger"
          >
            <Icon.Close size={11} />
          </button>
        </div>
      ))}
      <button onClick={onNew} className="shrink-0 rounded-md px-2 py-1 text-muted hover:bg-surface" title="New query tab">
        <Icon.Plus size={13} />
      </button>
    </div>
  )
}

function TabBody({ tab, conn, conns, editorH, setEditorH, patch, onRun, onCancel, onOpenValue, onSave, onEditRow, say, onArmChange }) {
  const engine = conn ? conn.engine : ''
  const caps = (conn && conn.capabilities) || {}
  const res = tab.result

  const sets = (res && res.sets) || []
  const primary = sets.find((s) => s.columns && s.columns.length) || sets[0] || null

  const views = [
    { id: 'results', label: 'Results' },
    ...(tab.object ? [{ id: 'overview', label: 'Overview' }, { id: 'columns', label: 'Columns' }, { id: 'indexes', label: 'Indexes' }, { id: 'ddl', label: 'DDL' }] : []),
    ...(engine === 'mongodb' ? [{ id: 'documents', label: 'Documents' }, { id: 'json', label: 'Raw JSON' }] : []),
    ...(caps.charts ? [{ id: 'chart', label: 'Chart' }] : []),
    ...(caps.explain ? [{ id: 'explain', label: 'Explain' }] : []),
    { id: 'messages', label: 'Messages' },
  ]

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <QueryBar tab={tab} conn={conn} conns={conns} patch={patch} onRun={onRun} onCancel={onCancel} onSave={onSave} say={say} />
      {conn && conn.readOnly && <ReadOnlyBanner tab={tab} conn={conn} patch={patch} onArmChange={onArmChange} />}
      <div className="shrink-0 overflow-auto border-b" style={{ height: editorH }}>
        {engine === 'mongodb'
          ? <MongoEditor tab={tab} patch={patch} onRun={onRun} />
          : engine === 'valkey'
            ? <ValkeyEditor tab={tab} patch={patch} onRun={onRun} />
            : <SqlEditor tab={tab} patch={patch} onRun={onRun} readOnly={!!(conn && conn.readOnly && !tab.armed)} />}
      </div>
      <Splitter horizontal onDrag={(dy) => setEditorH((h) => Math.min(640, Math.max(90, h + dy)))} />

      <div className="flex shrink-0 items-center gap-1 border-b bg-surface2/40 px-2 py-1">
        {views.map((v) => (
          <button
            key={v.id}
            onClick={() => patch({ view: v.id })}
            className={`rounded px-2 py-1 text-xs ${tab.view === v.id ? 'bg-surface font-medium shadow-sm' : 'text-muted hover:bg-surface/60'}`}
          >
            {v.label}
          </button>
        ))}
        <div className="flex-1" />
        {res && <ResultSummary res={res} />}
      </div>

      <div className="min-h-0 flex-1 overflow-hidden">
        <ResultBody
          tab={tab} conn={conn} res={res} primary={primary} sets={sets}
          onOpenValue={onOpenValue} onEditRow={onEditRow} patch={patch} onRun={onRun}
        />
      </div>
    </div>
  )
}

function ResultSummary({ res }) {
  if (res.error) return <span className="text-xs text-danger">error · {fmtDuration(res.error.elapsedMs || res.durationMs)}</span>
  const rows = (res.sets || []).reduce((a, s) => a + (s.rowCount || 0), 0)
  const aff = (res.sets || []).reduce((a, s) => a + (s.affectedRows || 0), 0)
  return (
    <span className="flex items-center gap-2 text-xs text-muted">
      <span className="tabular-nums">{fmtDuration(res.durationMs)}</span>
      {rows > 0 && <span>· {fmtNumber(rows)} rows</span>}
      {aff > 0 && <span>· {fmtNumber(aff)} affected</span>}
      {(res.sets || []).some((s) => s.truncated) && <Badge tone="warning">truncated</Badge>}
      {res.unlocked && <Badge tone="danger">ran with writes armed</Badge>}
    </span>
  )
}

// ReadOnlyBanner is the standing statement that this connection is PMM's own, and —
// where an administrator has unlocked it — the control that arms this tab for writes.
//
// It stays on screen while the tab is armed rather than fading, and it is red rather
// than amber, because an armed tab is a state you should not be able to forget you
// are in. Disarming is one click and needs no confirmation; arming needs two.
function ReadOnlyBanner({ tab, conn, patch, onArmChange }) {
  const [confirming, setConfirming] = useState(false)
  useEffect(() => {
    if (!confirming) return undefined
    const t = setTimeout(() => setConfirming(false), 5000)
    return () => clearTimeout(t)
  }, [confirming])

  const armed = !!tab.armed && conn.unlockable
  return (
    <div className={`flex flex-wrap items-center gap-2 border-b px-3 py-1.5 text-xs ${
      armed ? 'border-danger/40 bg-danger/10 text-danger' : 'bg-warning/5 text-warning'}`}>
      <Icon.StatusWarn size={14} className="shrink-0" />
      <span className="font-semibold">{armed ? 'WRITES ARMED' : conn.product}</span>
      <span className="min-w-0 flex-1 text-muted">
        {armed
          ? 'This tab can write to PMM\u2019s internal database. Modifying PMM internal data may corrupt or break the PMM installation.'
          : conn.warning}
      </span>
      {conn.unlockable ? (
        armed ? (
          <Button size="sm" variant="subtle" onClick={() => { patch({ armed: false }); if (onArmChange) onArmChange(false) }}>Disarm</Button>
        ) : confirming ? (
          <Button size="sm" variant="danger" onClick={() => { patch({ armed: true }); setConfirming(false); if (onArmChange) onArmChange(true) }}>
            Yes \u2014 allow writes in this tab
          </Button>
        ) : (
          <Button size="sm" variant="outline" onClick={() => setConfirming(true)}>Arm writes\u2026</Button>
        )
      ) : (
        <span className="shrink-0 text-muted" title="An administrator can allow this in Settings \u2192 Writes to internal databases.">
          read-only \u2014 an administrator can unlock it in Settings
        </span>
      )}
    </div>
  )
}

function QueryBar({ tab, conn, conns, patch, onRun, onCancel, onSave, say }) {
  const caps = (conn && conn.capabilities) || {}
  return (
    <div className="flex flex-wrap items-center gap-2 border-b px-2 py-1.5">
      <select
        value={tab.connectionId}
        onChange={(e) => {
          const c = conns.find((x) => x.id === e.target.value)
          // Arming belongs to the connection it was granted for, so changing the
          // connection disarms — you do not inherit a write unlock by switching.
          patch({ connectionId: e.target.value, database: '', schema: '', result: null, armed: false, title: c ? c.label : tab.title })
        }}
        className={`${inputCls} w-auto max-w-[280px] py-1 text-xs`}
      >
        <option value="">Connection…</option>
        {conns.map((c) => (
          <option key={c.id} value={c.id}>{c.stackName} · {c.label}{c.readOnly ? ' (read only)' : ''}</option>
        ))}
      </select>
      <DatabasePicker tab={tab} conn={conn} patch={patch} />
      <select
        value={tab.limit}
        onChange={(e) => patch({ limit: Number(e.target.value) })}
        className={`${inputCls} w-auto py-1 text-xs`}
        title="The server caps every result at this many rows. Nothing here ever fetches a whole table."
      >
        {LIMITS.map((n) => <option key={n} value={n}>{fmtNumber(n)} rows</option>)}
      </select>
      <div className="flex-1" />
      {tab.running ? (
        <Button size="sm" variant="danger" onClick={onCancel}><Icon.Pause size={12} /> Cancel</Button>
      ) : (
        <Button size="sm" variant="primary" onClick={() => onRun()} disabled={!tab.connectionId}>
          <Icon.Play size={12} /> Run
          <span className="ml-1 opacity-60">⌘⏎</span>
        </Button>
      )}
      {caps.explain && !tab.running && (
        <Button size="sm" variant="subtle" onClick={() => onRun({ explain: true })} disabled={!tab.connectionId}>
          Explain
        </Button>
      )}
      <button
        onClick={() => {
          const name = typeof prompt === 'function' ? prompt('Save this query as:') : ''
          if (name) onSave({ name, engine: conn ? conn.engine : '', statement: tab.sql || tab.command, database: tab.database })
        }}
        className="rounded-md border px-2 py-1 text-xs hover:bg-surface2"
      >
        Save
      </button>
      {conn && ['mysql', 'postgres'].includes(conn.engine) && (
        <button
          onClick={() => {
            sendHandoff('qr-sql', JSON.stringify({ stackId: conn.stackId, nodeId: conn.nodeId, database: tab.database, sql: tab.sql }))
            location.hash = 'queryrun'
          }}
          className="rounded-md border px-2 py-1 text-xs hover:bg-surface2"
          title="Take this statement to the Query Runner, which runs it repeatedly and in parallel."
        >
          Open in Query Runner
        </button>
      )}
    </div>
  )
}

function DatabasePicker({ tab, conn, patch }) {
  const [dbs, setDbs] = useState([])
  const [schemas, setSchemas] = useState([])
  useEffect(() => {
    if (!tab.connectionId) { setDbs([]); return }
    dbxApi.databases(tab.connectionId).then((d) => setDbs(d.nodes || [])).catch(() => setDbs([]))
  }, [tab.connectionId])
  useEffect(() => {
    if (!tab.connectionId || !tab.database || !(conn && conn.capabilities && conn.capabilities.schemas)) { setSchemas([]); return }
    dbxApi.schemas(tab.connectionId, tab.database).then((d) => setSchemas(d.nodes || [])).catch(() => setSchemas([]))
  }, [tab.connectionId, tab.database, conn])
  return (
    <>
      <select value={tab.database} onChange={(e) => patch({ database: e.target.value, schema: '' })} className={`${inputCls} w-auto max-w-[200px] py-1 text-xs`}>
        <option value="">Database…</option>
        {dbs.map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}
      </select>
      {schemas.length > 0 && (
        <select value={tab.schema} onChange={(e) => patch({ schema: e.target.value })} className={`${inputCls} w-auto max-w-[180px] py-1 text-xs`}>
          <option value="">Schema…</option>
          {schemas.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
        </select>
      )}
    </>
  )
}

// ------------------------------------------------------------------ editors

function SqlEditor({ tab, patch, onRun, readOnly }) {
  const ref = useRef(null)
  const [confirm, setConfirm] = useState(null)

  const submit = (selectionOnly) => {
    const el = ref.current
    let sql = tab.sql
    if (selectionOnly && el) {
      const sel = tab.sql.slice(el.selectionStart, el.selectionEnd).trim()
      sql = sel || statementAt(tab.sql, el.selectionStart)
    }
    if (!sql.trim()) return
    // A destructive statement is never run on a keystroke. This is not a security
    // control — a lab database is meant to be written to — it is the difference
    // between running a DROP and running one on purpose.
    if (isDestructiveSQL(sql)) { setConfirm(sql); return }
    onRun({ sql })
  }

  const onKey = (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      submit(e.shiftKey)
    }
  }

  return (
    <div className="relative h-full">
      <textarea
        ref={ref}
        value={tab.sql}
        onChange={(e) => patch({ sql: e.target.value })}
        onKeyDown={onKey}
        spellCheck={false}
        placeholder={readOnly
          ? 'SELECT … — this connection is read-only, and the database refuses writes on it as well.'
          : 'SELECT * FROM …   ⌘/Ctrl+Enter to run · ⌘/Ctrl+Shift+Enter to run the selection'}
        className="h-full w-full resize-none bg-bg p-3 font-mono text-[13px] leading-relaxed outline-none"
      />
      <div className="absolute bottom-2 right-3 flex gap-1">
        <button onClick={() => patch({ sql: formatSQL(tab.sql) })} className="rounded border bg-surface/90 px-1.5 py-0.5 text-[10px] hover:bg-surface2">Format</button>
        <button onClick={() => patch({ sql: '' })} className="rounded border bg-surface/90 px-1.5 py-0.5 text-[10px] hover:bg-surface2">Clear</button>
      </div>
      {confirm && (
        <Confirm
          title="Run this statement?"
          body={confirm}
          note="This changes data or schema. DBCanvas will not run it on a keystroke alone."
          onCancel={() => setConfirm(null)}
          onConfirm={() => { const s = confirm; setConfirm(null); onRun({ sql: s }) }}
        />
      )}
    </div>
  )
}

function MongoEditor({ tab, patch, onRun }) {
  const m = tab.mongo
  const set = (p) => patch({ mongo: { ...m, ...p } })
  const box = 'ui-input w-full rounded-md border bg-bg p-2 font-mono text-xs outline-none focus:border-primary'
  const onKey = (e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); onRun() } }
  return (
    <div className="h-full overflow-auto p-3" onKeyDown={onKey}>
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <input value={m.collection} onChange={(e) => set({ collection: e.target.value })}
          placeholder="collection" className={`${inputCls} w-auto max-w-[220px] py-1 text-xs`} />
        <div className="flex items-center gap-1 rounded-lg border p-0.5">
          {['find', 'aggregate', 'count', 'indexes', 'stats', 'command'].map((op) => (
            <button key={op} onClick={() => set({ operation: op })}
              className={`rounded px-2 py-0.5 text-xs ${m.operation === op ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2'}`}>
              {op}
            </button>
          ))}
        </div>
        <span className="text-[11px] text-muted">JSON — Extended JSON works, e.g. {'{"_id": {"$oid": "…"}}'}</span>
      </div>
      {m.operation === 'aggregate' ? (
        <label className="block">
          <span className="text-[10px] uppercase tracking-wide text-muted">Pipeline</span>
          <textarea value={m.pipeline} onChange={(e) => set({ pipeline: e.target.value })} rows={8} className={box} spellCheck={false} />
        </label>
      ) : m.operation === 'command' ? (
        <label className="block">
          <span className="text-[10px] uppercase tracking-wide text-muted">Command document</span>
          <textarea value={m.filter} onChange={(e) => set({ filter: e.target.value })} rows={6} className={box} spellCheck={false}
            placeholder={'{ "serverStatus": 1 }'} />
        </label>
      ) : (
        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
          <label className="block md:col-span-2">
            <span className="text-[10px] uppercase tracking-wide text-muted">Filter</span>
            <textarea value={m.filter} onChange={(e) => set({ filter: e.target.value })} rows={4} className={box} spellCheck={false}
              placeholder={'{ "status": "active" }'} />
          </label>
          <label className="block">
            <span className="text-[10px] uppercase tracking-wide text-muted">Projection</span>
            <textarea value={m.projection} onChange={(e) => set({ projection: e.target.value })} rows={2} className={box} spellCheck={false}
              placeholder={'{ "_id": 0, "name": 1 }'} />
          </label>
          <label className="block">
            <span className="text-[10px] uppercase tracking-wide text-muted">Sort</span>
            <textarea value={m.sort} onChange={(e) => set({ sort: e.target.value })} rows={2} className={box} spellCheck={false}
              placeholder={'{ "createdAt": -1 }'} />
          </label>
        </div>
      )}
    </div>
  )
}

function ValkeyEditor({ tab, patch, onRun }) {
  const onKey = (e) => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); onRun() } }
  return (
    <div className="flex h-full flex-col p-3">
      <textarea
        value={tab.command}
        onChange={(e) => patch({ command: e.target.value })}
        onKeyDown={onKey}
        spellCheck={false}
        placeholder={'GET foo\nHGETALL customer:100\nINFO\nSLOWLOG GET 20\nSCAN 0 MATCH users:* COUNT 100'}
        className="min-h-0 flex-1 resize-none rounded-md border bg-bg p-2 font-mono text-[13px] outline-none focus:border-primary"
      />
      <p className="mt-1 text-[11px] text-muted">
        One command per run. <code>KEYS</code> is refused — the key browser uses <b>SCAN</b>, and so should you.
      </p>
    </div>
  )
}

function Confirm({ title, body, note, onCancel, onConfirm }) {
  return (
    <div className="absolute inset-0 z-30 flex items-center justify-center bg-black/30 p-4">
      <div className="w-full max-w-lg rounded-xl border bg-surface p-4 shadow-2xl">
        <h3 className="text-sm font-semibold">{title}</h3>
        {note && <p className="mt-1 text-xs text-muted">{note}</p>}
        <pre className="mt-3 max-h-40 overflow-auto rounded-md border bg-bg p-2 font-mono text-xs">{body}</pre>
        <div className="mt-3 flex justify-end gap-2">
          <Button size="sm" variant="subtle" onClick={onCancel}>Cancel</Button>
          <Button size="sm" variant="danger" onClick={onConfirm}>Run it</Button>
        </div>
      </div>
    </div>
  )
}

// ------------------------------------------------------------------ results

function ResultBody({ tab, conn, res, primary, sets, onOpenValue, onEditRow, patch, onRun }) {
  const detail = tab.object && tab.object.detail

  if (tab.view === 'overview' && tab.object) return <Overview tab={tab} conn={conn} detail={detail} error={tab.object.error} />
  if (tab.view === 'columns' && tab.object) return <ColumnsView detail={detail} />
  if (tab.view === 'indexes' && tab.object) return <IndexesView detail={detail} />
  if (tab.view === 'ddl' && tab.object) return <DDLView detail={detail} />

  if (!res) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center text-sm text-muted">
        <Icon.Table size={22} className="opacity-40" />
        <p>Run something, or click a table in the tree to see its data.</p>
      </div>
    )
  }
  if (res.error) return <ErrorPanel err={res.error} />

  if (tab.view === 'messages') return <MessagesView res={res} />
  if (tab.view === 'json' || tab.view === 'documents') {
    return <DocumentsView sets={sets} raw={tab.view === 'json'} />
  }
  if (tab.view === 'chart') {
    return <DbxChart columns={(primary && primary.columns) || []} rows={(primary && primary.rows) || []} name={tab.title} />
  }
  if (tab.view === 'explain') {
    const ex = sets.find((s) => s.kind === 'explain') || primary
    if (!ex) return <div className="p-6 text-sm text-muted">Press <b>Explain</b> to ask this database for a plan.</div>
    return <ExplainView set={ex} />
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {sets.length > 1 && (
        <div className="flex shrink-0 gap-1 border-b bg-surface2/40 px-2 py-1 text-xs text-muted">
          {sets.length} result sets — showing each in order below
        </div>
      )}
      {sets.map((s, i) => (
        <div key={i} className={sets.length > 1 ? 'min-h-0 flex-1 border-b' : 'min-h-0 flex-1'}>
          {s.columns && s.columns.length ? (
            <DbxGrid
              columns={s.columns} rows={s.rows || []} truncated={s.truncated} rowLimit={res.limit}
              name={tab.title}
              editable={!!(detail && detail.editable)}
              onOpenValue={onOpenValue}
              onInsertRow={detail && detail.editable ? () => onEditRow({ mode: 'insert', tab, conn, detail }) : null}
              onEditRow={(row) => onEditRow({ mode: 'update', tab, conn, detail, row, columns: s.columns })}
              onDeleteRow={(row) => onEditRow({ mode: 'delete', tab, conn, detail, row, columns: s.columns })}
              footerExtra={detail && !detail.editable && detail.editReason
                ? <span className="text-[11px] text-muted" title={detail.editReason}>read-only grid</span>
                : null}
            />
          ) : (
            <div className="p-4 text-sm text-muted">
              {s.affectedRows ? `${fmtNumber(s.affectedRows)} rows affected.` : (s.message || 'Statement completed with no result set.')}
              {s.insertId ? ` Last insert id ${s.insertId}.` : ''}
            </div>
          )}
        </div>
      ))}
      {!sets.length && <div className="p-4 text-sm text-muted">No result.</div>}
    </div>
  )
}

function ErrorPanel({ err }) {
  return (
    <div className="h-full overflow-auto p-4">
      <div className="rounded-lg border border-danger/40 bg-danger/5 p-3">
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone="danger">{ENGINE_LABEL[err.engine] || err.engine}</Badge>
          {err.code && <Badge tone="muted">code {err.code}</Badge>}
          {err.sqlState && <Badge tone="muted">SQLSTATE {err.sqlState}</Badge>}
          {err.name && <Badge tone="muted">{err.name}</Badge>}
          {err.elapsedMs != null && <span className="text-xs text-muted">{fmtDuration(err.elapsedMs)}</span>}
        </div>
        <pre className="mt-2 whitespace-pre-wrap break-words font-mono text-[13px] text-danger">{err.display || err.message}</pre>
        {err.position > 0 && <p className="mt-1 text-xs text-muted">{`at character ${err.position} of the statement`}</p>}
        {err.detail && <p className="mt-2 text-xs text-muted"><b>Detail:</b> {err.detail}</p>}
        {err.hint && <p className="mt-1 text-xs text-muted"><b>Hint:</b> {err.hint}</p>}
      </div>
    </div>
  )
}

function MessagesView({ res }) {
  const lines = []
  if (res.durationMs != null) lines.push(`Completed in ${fmtDuration(res.durationMs)}.`)
  for (const s of res.sets || []) {
    if (s.statement) lines.push(`> ${s.statement}`)
    if (s.rowCount) lines.push(`  ${fmtNumber(s.rowCount)} rows returned${s.truncated ? ' (truncated at the result limit)' : ''}.`)
    if (s.affectedRows) lines.push(`  ${fmtNumber(s.affectedRows)} rows affected.`)
    if (s.message) lines.push(`  ${s.message}`)
  }
  for (const w of res.warnings || []) lines.push(`! ${w}`)
  if (res.readOnly) lines.push('This connection is read-only; the database refuses writes on it as well as DBCanvas.')
  return <pre className="h-full overflow-auto p-4 font-mono text-xs leading-relaxed">{lines.join('\n') || 'Nothing to report.'}</pre>
}

function ExplainView({ set }) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="border-b px-3 py-1.5 text-xs text-muted">
        The plan as the database reported it. <b>EXPLAIN</b> only — never <code>ANALYZE</code>, which would run the statement.
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        {set.payload
          ? <pre className="p-3 font-mono text-xs leading-relaxed">{prettyJson(typeof set.payload === 'string' ? set.payload : JSON.stringify(set.payload))}</pre>
          : <DbxGrid columns={set.columns || []} rows={set.rows || []} name="explain" emptyLabel="No plan returned." />}
      </div>
    </div>
  )
}

function DocumentsView({ sets, raw }) {
  const docs = sets.flatMap((s) => s.documents || [])
  if (!docs.length) return <div className="p-6 text-sm text-muted">This result has no documents.</div>
  if (raw) {
    return (
      <pre className="h-full overflow-auto p-4 font-mono text-xs leading-relaxed">
        {prettyJson(`[${docs.map((d) => (typeof d === 'string' ? d : JSON.stringify(d))).join(',')}]`)}
      </pre>
    )
  }
  return (
    <div className="h-full overflow-auto p-2">
      {docs.map((d, i) => <DocumentCard key={i} index={i} doc={d} />)}
    </div>
  )
}

function DocumentCard({ index, doc }) {
  const value = useMemo(() => {
    try { return typeof doc === 'string' ? JSON.parse(doc) : doc } catch { return doc }
  }, [doc])
  return (
    <div className="mb-1.5 rounded-lg border bg-bg">
      <div className="flex items-center justify-between border-b px-2 py-1 text-[11px] text-muted">
        <span>#{index + 1}</span>
        <button onClick={() => copyText(typeof doc === 'string' ? doc : JSON.stringify(doc))} className="hover:text-fg" title="Copy this document">
          <Icon.Copy size={11} />
        </button>
      </div>
      <div className="p-2 font-mono text-xs">
        <JsonNode value={value} name={null} depth={0} defaultOpen />
      </div>
    </div>
  )
}

// JsonNode is the collapsible document viewer. Objects and arrays open and close;
// scalars are typed by colour and never truncated into something unreadable.
function JsonNode({ value, name, depth, defaultOpen = false }) {
  const [open, setOpen] = useState(defaultOpen || depth < 2)
  const isObj = value && typeof value === 'object' && !Array.isArray(value)
  const isArr = Array.isArray(value)
  const label = name !== null && name !== undefined ? <span className="text-primary">{name}: </span> : null

  if (!isObj && !isArr) {
    let cls = 'text-fg'
    let text = String(value)
    if (value === null) { cls = 'italic text-muted'; text = 'null' } else if (typeof value === 'string') { cls = 'text-success'; text = JSON.stringify(value) } else if (typeof value === 'number') cls = 'text-accent'
    else if (typeof value === 'boolean') cls = 'text-warning'
    return <div style={{ paddingLeft: depth * 12 }}>{label}<span className={cls}>{text}</span></div>
  }
  const entries = isArr ? value.map((v, i) => [i, v]) : Object.entries(value)
  return (
    <div style={{ paddingLeft: depth * 12 }}>
      <button onClick={() => setOpen((o) => !o)} className="text-left hover:underline">
        {label}
        <span className="text-muted">
          {open ? (isArr ? '[' : '{') : `${isArr ? `[ ${entries.length} items ]` : `{ ${entries.length} fields }`}`}
        </span>
      </button>
      {open && (
        <>
          {entries.map(([k, v]) => <JsonNode key={k} name={k} value={v} depth={depth + 1} />)}
          <div className="text-muted" style={{ paddingLeft: 0 }}>{isArr ? ']' : '}'}</div>
        </>
      )}
    </div>
  )
}

// ------------------------------------------------------------------ object views

function Overview({ tab, conn, detail, error }) {
  if (error) return <ErrorPanel err={error} />
  if (!detail) return <div className="p-6 text-sm text-muted">Reading the object…</div>
  const ref = tab.object.ref
  return (
    <div className="h-full overflow-auto p-4">
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Icon.Table size={18} className="text-primary" />
        <h2 className="text-base font-semibold">{ref.name}</h2>
        <Badge tone="muted">{ref.kind}</Badge>
        {conn && <span className="text-xs text-muted">{ref.database}{ref.schema ? `.${ref.schema}` : ''} · {conn.label}</span>}
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Rows (estimate)" value={detail.rowEstimate != null ? `~${fmtNumber(Number(detail.rowEstimate))}` : '—'}
          help="An estimate from the server's own statistics. Counting every row to draw a panel is what this feature deliberately does not do." />
        <Stat label="Size" value={detail.bytes != null ? fmtBytes(detail.bytes) : '—'} />
        <Stat label="Columns" value={detail.columns ? detail.columns.length : 0} />
        <Stat label="Primary key" value={detail.primaryKey && detail.primaryKey.length ? detail.primaryKey.join(', ') : 'none'} />
      </div>
      {detail.props && detail.props.length > 0 && (
        <div className="mt-4 rounded-lg border">
          {detail.props.map((p) => (
            <div key={p.label} className="flex items-start justify-between gap-4 border-b px-3 py-1.5 text-sm last:border-b-0">
              <span className="text-muted">{p.label}</span>
              <span className="text-right font-mono text-xs">{p.value}</span>
            </div>
          ))}
        </div>
      )}
      {detail.foreignKeys && detail.foreignKeys.length > 0 && (
        <Section title="Foreign keys">
          {detail.foreignKeys.map((f) => (
            <div key={f.name} className="border-b px-3 py-1.5 text-xs last:border-b-0">
              <span className="font-medium">{f.name}</span>
              <span className="text-muted"> ({f.columns.join(', ')}) → {f.refSchema ? `${f.refSchema}.` : ''}{f.refTable} ({f.refColumns.join(', ')})</span>
              {(f.onUpdate || f.onDelete) && <span className="text-muted"> · ON UPDATE {f.onUpdate || 'NO ACTION'} / ON DELETE {f.onDelete || 'NO ACTION'}</span>}
            </div>
          ))}
        </Section>
      )}
      {detail.constraints && detail.constraints.length > 0 && (
        <Section title="Constraints">
          {detail.constraints.map((c) => (
            <div key={c.name} className="border-b px-3 py-1.5 text-xs last:border-b-0">
              <span className="font-medium">{c.name}</span> <Badge tone="muted">{c.type}</Badge>
              <div className="font-mono text-[11px] text-muted">{c.definition}</div>
            </div>
          ))}
        </Section>
      )}
      {detail.triggers && detail.triggers.length > 0 && (
        <Section title="Triggers">
          {detail.triggers.map((t) => (
            <div key={t.id} className="border-b px-3 py-1.5 text-xs last:border-b-0">
              <span className="font-medium">{t.name}</span>
              <div className="font-mono text-[11px] text-muted">{t.detail}</div>
            </div>
          ))}
        </Section>
      )}
      {!detail.editable && detail.editReason && (
        <p className="mt-4 rounded-lg border border-warning/40 bg-warning/5 p-2 text-xs text-warning">
          <b>Rows cannot be edited here.</b> {detail.editReason}
        </p>
      )}
    </div>
  )
}

function Section({ title, children }) {
  return (
    <div className="mt-4">
      <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-muted">{title}</h3>
      <div className="rounded-lg border">{children}</div>
    </div>
  )
}

function Stat({ label, value, help }) {
  return (
    <div className="rounded-lg border bg-bg p-3">
      <div className="flex items-center gap-1 text-[11px] uppercase tracking-wide text-muted">
        {label}{help && <Help text={help} />}
      </div>
      <div className="mt-0.5 truncate text-lg font-semibold tabular-nums">{value}</div>
    </div>
  )
}

function ColumnsView({ detail }) {
  if (!detail || !detail.columns) return <div className="p-6 text-sm text-muted">No column information.</div>
  const columns = [
    { name: '#', semanticType: 'integer' }, { name: 'name', semanticType: 'string' },
    { name: 'type', semanticType: 'string' }, { name: 'nullable', semanticType: 'boolean' },
    { name: 'key', semanticType: 'string' }, { name: 'default', semanticType: 'string' },
    { name: 'extra', semanticType: 'string' }, { name: 'comment', semanticType: 'string' },
  ]
  const rows = detail.columns.map((c, i) => [i + 1, c.name, c.type, c.nullable, c.key || '', c.default || '', c.extra || '', c.comment || ''])
  return <DbxGrid columns={columns} rows={rows} name="columns" />
}

function IndexesView({ detail }) {
  if (!detail || !detail.indexes || !detail.indexes.length) return <div className="p-6 text-sm text-muted">No indexes.</div>
  const columns = [
    { name: 'name', semanticType: 'string' }, { name: 'columns', semanticType: 'string' },
    { name: 'unique', semanticType: 'boolean' }, { name: 'primary', semanticType: 'boolean' },
    { name: 'type', semanticType: 'string' }, { name: 'size', semanticType: 'string' },
    { name: 'partial', semanticType: 'string' },
  ]
  const rows = detail.indexes.map((x) => [x.name, (x.columns || []).join(', '), x.unique, x.primary, x.type || '', x.size != null ? fmtBytes(x.size) : '', x.partial || ''])
  return <DbxGrid columns={columns} rows={rows} name="indexes" />
}

function DDLView({ detail }) {
  const [copied, setCopied] = useState(false)
  if (!detail || !detail.ddl) return <div className="p-6 text-sm text-muted">This database does not expose a definition for this object.</div>
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-2 border-b px-3 py-1.5 text-xs text-muted">
        <span>The object&apos;s definition.</span>
        <div className="flex-1" />
        <button
          onClick={async () => { setCopied(await copyText(detail.ddl)); setTimeout(() => setCopied(false), 1500) }}
          className="rounded border px-2 py-0.5 hover:bg-surface2"
        >
          <Icon.Copy size={11} className="inline" /> {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre className="min-h-0 flex-1 overflow-auto p-3 font-mono text-xs leading-relaxed">{highlightSQL(detail.ddl)}</pre>
    </div>
  )
}

// highlightSQL is a deliberately small tokeniser: keywords, strings, numbers and
// comments. Enough to make a CREATE TABLE readable, and small enough that a weird
// dialect cannot make it render nonsense.
const SQL_KEYWORDS = /\b(CREATE|TABLE|VIEW|MATERIALIZED|INDEX|UNIQUE|PRIMARY|FOREIGN|KEY|REFERENCES|NOT|NULL|DEFAULT|CONSTRAINT|CHECK|ENGINE|CHARSET|COLLATE|AUTO_INCREMENT|GENERATED|ALWAYS|AS|STORED|SELECT|FROM|WHERE|ORDER|BY|GROUP|PARTITION|SETTINGS|ON|UPDATE|DELETE|CASCADE|RESTRICT|ACTION|WITH|AND|OR|IN|IS)\b/gi
function highlightSQL(sql) {
  const out = []
  let i = 0
  const text = String(sql)
  const push = (s, cls) => out.push(cls ? <span key={out.length} className={cls}>{s}</span> : s)
  const re = /(--[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^']|'')*')|(`[^`]*`|"[^"]*")|(\b\d+(?:\.\d+)?\b)/g
  let m = re.exec(text)
  while (m) {
    if (m.index > i) pushKeywords(text.slice(i, m.index), push)
    if (m[1]) push(m[1], 'text-muted italic')
    else if (m[2]) push(m[2], 'text-success')
    else if (m[3]) push(m[3], 'text-fg')
    else push(m[4], 'text-accent')
    i = m.index + m[0].length
    m = re.exec(text)
  }
  if (i < text.length) pushKeywords(text.slice(i), push)
  return out
}
function pushKeywords(chunk, push) {
  let last = 0
  let m = SQL_KEYWORDS.exec(chunk)
  while (m) {
    if (m.index > last) push(chunk.slice(last, m.index))
    push(m[0], 'font-semibold text-primary')
    last = m.index + m[0].length
    m = SQL_KEYWORDS.exec(chunk)
  }
  SQL_KEYWORDS.lastIndex = 0
  if (last < chunk.length) push(chunk.slice(last))
}

// ------------------------------------------------------------------ row editing

// RowEditor is the insert/edit/delete dialog. Nothing it does happens without the
// statement being shown first: the server builds the mutation and hands back exactly
// what it would run, and only a second, explicit click executes it.
function RowEditor({ state, onClose, onDone }) {
  const { mode, tab, conn, detail, row, columns } = state
  const cols = (detail && detail.columns) || []
  const [values, setValues] = useState(() => {
    const v = {}
    if (mode === 'update' && row && columns) {
      columns.forEach((c, i) => { v[c.name] = isNull(row[i]) ? '' : cellText(row[i]) })
    }
    return v
  })
  const [nulls, setNulls] = useState(() => {
    const n = {}
    if (mode === 'update' && row && columns) columns.forEach((c, i) => { n[c.name] = isNull(row[i]) })
    return n
  })
  const [preview, setPreview] = useState('')
  const [err, setErr] = useState(null)
  const [busy, setBusy] = useState(false)

  const identity = useMemo(() => {
    const out = {}
    if (!row || !columns) return out
    const keys = (detail && detail.primaryKey && detail.primaryKey.length)
      ? detail.primaryKey
      : ((detail && detail.indexes) || []).filter((x) => x.unique).flatMap((x) => x.columns).slice(0, 4)
    for (const k of keys) {
      const i = columns.findIndex((c) => c.name === k)
      if (i >= 0) out[k] = isNull(row[i]) ? null : coerce(row[i])
    }
    return out
  }, [row, columns, detail])

  const payload = (wantPreview) => ({
    connectionId: conn.id, database: tab.database, schema: tab.schema,
    allowWrites: !!(tab.armed && conn.unlockable),
    object: tab.object ? tab.object.ref.name : '',
    op: mode === 'delete' ? 'delete' : mode,
    values: mode === 'delete' ? undefined : Object.fromEntries(
      cols.filter((c) => values[c.name] !== undefined || nulls[c.name])
        .map((c) => [c.name, nulls[c.name] ? null : coerce(values[c.name])]),
    ),
    identity,
    preview: wantPreview,
    confirm: !wantPreview,
  })

  const doPreview = async () => {
    setBusy(true); setErr(null)
    try {
      const r = await dbxApi.mutate(payload(true))
      if (r.error) setErr(r.error); else setPreview(r.preview)
    } catch (e) { setErr({ message: e.message, display: e.message }) } finally { setBusy(false) }
  }
  useEffect(() => { doPreview() }, []) // eslint-disable-line react-hooks/exhaustive-deps

  const apply = async () => {
    setBusy(true); setErr(null)
    try {
      const r = await dbxApi.mutate(payload(false))
      if (r.error) { setErr(r.error); return }
      onDone(`${fmtNumber(r.affectedRows)} ${r.affectedRows === 1 ? 'row' : 'rows'} ${mode === 'delete' ? 'deleted' : mode === 'insert' ? 'inserted' : 'updated'}.`)
    } catch (e) { setErr({ message: e.message, display: e.message }) } finally { setBusy(false) }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div className="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-xl border bg-surface shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between border-b px-4 py-2.5">
          <h3 className="text-sm font-semibold">
            {mode === 'insert' ? 'Insert row' : mode === 'update' ? 'Edit row' : 'Delete row'}
            <span className="ml-2 font-normal text-muted">{tab.object ? tab.object.ref.name : ''}</span>
          </h3>
          <Button size="sm" variant="ghost" onClick={onClose}><Icon.Close size={14} /></Button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto p-4">
          {mode !== 'delete' && (
            <div className="space-y-2">
              {cols.map((c) => (
                <div key={c.name} className="flex items-center gap-2">
                  <span className="w-40 shrink-0 truncate text-xs" title={`${c.name} ${c.type}`}>
                    {c.name}<span className="ml-1 text-muted">{c.type}</span>
                  </span>
                  <input
                    value={nulls[c.name] ? '' : (values[c.name] ?? '')}
                    disabled={nulls[c.name]}
                    onChange={(e) => { setValues((v) => ({ ...v, [c.name]: e.target.value })); setPreview('') }}
                    className={`${inputCls} py-1 font-mono text-xs disabled:opacity-40`}
                    placeholder={c.default || ''}
                  />
                  <label className="flex shrink-0 items-center gap-1 text-[11px] text-muted" title="Write SQL NULL, which is not the same as an empty string.">
                    <input type="checkbox" checked={!!nulls[c.name]} disabled={!c.nullable}
                      onChange={(e) => { setNulls((n) => ({ ...n, [c.name]: e.target.checked })); setPreview('') }} />
                    NULL
                  </label>
                </div>
              ))}
            </div>
          )}
          <div className="mt-4">
            <div className="mb-1 flex items-center gap-2 text-xs font-medium text-muted">
              Statement to run
              <button onClick={doPreview} disabled={busy} className="rounded border px-1.5 py-0.5 text-[10px] hover:bg-surface2">Refresh</button>
            </div>
            <pre className="max-h-40 overflow-auto rounded-md border bg-bg p-2 font-mono text-xs">{preview || (busy ? 'Building…' : '—')}</pre>
            <p className="mt-1 text-[11px] text-muted">
              Values are bound as parameters; the statement above shows them inline only so it can be read.
            </p>
          </div>
          {err && (
            <div className="mt-3 rounded-lg border border-danger/40 bg-danger/5 p-2 text-xs text-danger">
              {err.display || err.message}
              {err.hint && <div className="mt-1 text-muted">{err.hint}</div>}
            </div>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t px-4 py-2.5">
          <Button size="sm" variant="subtle" onClick={onClose}>Cancel</Button>
          {mode === 'delete' ? (
            <ConfirmButton size="sm" variant="danger" onConfirm={apply} confirmLabel="Really delete?">Delete row</ConfirmButton>
          ) : (
            <Button size="sm" variant="primary" onClick={apply} disabled={busy || !preview}>
              {mode === 'insert' ? 'Insert' : 'Save changes'}
            </Button>
          )}
        </div>
      </div>
    </div>
  )
}

// coerce turns a text field back into the JSON value the server binds. A blank is an
// empty string, not NULL — NULL is the checkbox, which is the only way the two can
// stay distinguishable in a text input.
function coerce(v) {
  if (v === null || v === undefined) return null
  if (typeof v === 'object') return v
  const s = String(v)
  if (s.trim() === '') return ''
  if (/^-?\d+$/.test(s.trim())) return Number(s)
  if (/^-?\d*\.\d+$/.test(s.trim())) return Number(s)
  if (s === 'true') return true
  if (s === 'false') return false
  return s
}

// ------------------------------------------------------------------ side panels

function HistoryPanel({ entries, onRerun, onDelete, onClear }) {
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center justify-between border-b px-2 py-1.5">
        <span className="text-xs font-medium">History</span>
        <ConfirmButton size="sm" variant="ghost" onConfirm={onClear} confirmLabel="Clear all?">Clear</ConfirmButton>
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        {!entries.length && <p className="p-4 text-xs text-muted">Nothing yet. Every query you run here is recorded — never its password, because there is not one to record.</p>}
        {entries.map((h) => (
          <div key={h.id} className="group border-b px-2 py-1.5 text-xs hover:bg-surface2/60">
            <div className="flex items-center gap-1.5">
              <span className={h.success ? 'text-success' : 'text-danger'}>{h.success ? '●' : '●'}</span>
              <span className="min-w-0 flex-1 truncate text-muted" title={h.connection}>{h.connection}</span>
              <span className="shrink-0 tabular-nums text-muted">{fmtDuration(h.durationMs)}</span>
            </div>
            <div className="mt-0.5 line-clamp-2 break-words font-mono text-[11px]" title={h.statement}>{h.statement}</div>
            <div className="mt-1 flex items-center gap-2 text-[10px] text-muted">
              <span>{new Date(h.at).toLocaleTimeString()}</span>
              {h.database && <span>· {h.database}</span>}
              {h.rowCount > 0 && <span>· {fmtNumber(h.rowCount)} rows</span>}
              {!h.success && <span className="truncate text-danger" title={h.error}>· {h.error}</span>}
              <div className="flex-1" />
              <button onClick={() => onRerun(h)} className="opacity-0 hover:underline group-hover:opacity-100">rerun</button>
              <button onClick={() => copyText(h.statement)} className="opacity-0 hover:underline group-hover:opacity-100">copy</button>
              <button onClick={() => onDelete(h.id)} className="opacity-0 hover:text-danger group-hover:opacity-100">delete</button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function SavedPanel({ items, onOpen, onDelete }) {
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="border-b px-2 py-1.5 text-xs font-medium">Saved queries</div>
      <div className="min-h-0 flex-1 overflow-auto">
        {!items.length && <p className="p-4 text-xs text-muted">Nothing saved. Press <b>Save</b> above an editor to keep a query.</p>}
        {items.map((q) => (
          <div key={q.id} className="group border-b px-2 py-1.5 text-xs hover:bg-surface2/60">
            <div className="flex items-center gap-2">
              <button onClick={() => onOpen(q)} className="min-w-0 flex-1 truncate text-left font-medium hover:underline">{q.name}</button>
              <Badge tone="muted">{ENGINE_LABEL[q.engine] || q.engine}</Badge>
              <button onClick={() => onDelete(q.id)} className="opacity-0 hover:text-danger group-hover:opacity-100"><Icon.Trash size={11} /></button>
            </div>
            {q.description && <p className="mt-0.5 text-[11px] text-muted">{q.description}</p>}
            <div className="mt-0.5 line-clamp-2 break-words font-mono text-[11px] text-muted">{q.statement}</div>
          </div>
        ))}
      </div>
    </div>
  )
}

export {
  ConnectionTree, DatabasePicker, HistoryPanel, SavedPanel, Overview, ColumnsView,
  IndexesView, DDLView, ErrorPanel, MessagesView, DocumentsView, JsonNode, RowEditor,
  SqlEditor, MongoEditor, ValkeyEditor, TabStrip, highlightSQL, coerce, ReadOnlyBanner,
}
