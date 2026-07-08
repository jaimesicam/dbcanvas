import { useCallback, useEffect, useState } from 'react'
import { Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { pxcApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { PTStalkCard } from '../components/Diagnostics.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'creds', label: 'Credentials' },
  { id: 'cert', label: 'Certificate' },
  { id: 'diag', label: 'Diagnostics' },
]

function CopyButton({ text, size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button
      title="Copy"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
    >
      {done ? <Icon.Check size={size} /> : <Icon.Copy size={size} />}
    </button>
  )
}

function Err({ children }) {
  if (!children) return null
  return <div className="mb-2 rounded-lg border border-danger/30 bg-danger/15 px-2.5 py-1.5 text-xs text-danger">{children}</div>
}

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{(v ?? '') === '' ? '—' : String(v)}</span>
    </div>
  )
}

export default function PXCManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const api = pxcApi(stackId, nodeId)
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const arbiter = cfg.role === 'arbitrator'

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PXC · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => !(arbiter && t.id !== 'overview')).map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && (
        <Overview cfg={cfg} dep={dep} arbiter={arbiter}
          onDeleteNode={onDeleteNode}
          onOpenTerminal={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })} />
      )}
      {tab === 'creds' && !arbiter && <CredsTab cfg={cfg} sec={sec} />}
      {tab === 'cert' && !arbiter && <CertTab api={api} cfg={cfg} />}
      {tab === 'diag' && !arbiter && <PTStalkCard stackId={stackId} nodeId={nodeId} />}
    </div>
  )
}

function Overview({ cfg, dep, arbiter, onDeleteNode, onOpenTerminal }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const exportUrl = cfg.exportPort ? `${host}:${cfg.exportPort}` : null
  return (
    <div className="space-y-2 text-sm">
      <KV k="Cluster" v={cfg.cluster} />
      <KV k="Role" v={arbiter ? 'arbitrator (garbd)' : 'regular (data)'} />
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="server-id" v={cfg.serverId} />
      <KV k="Image" v={cfg.image} mono />
      <KV k="GTID" v={cfg.gtid ? 'on' : 'off'} />
      <KV k="TLS" v={cfg.generateCert ? 'Intranet CA' : 'none'} />
      <KV k="Monitored by" v={cfg.monitoredBy} mono />
      <KV k="Ports" v={(cfg.ports || []).join(', ')} mono />
      {!arbiter && exportUrl && (
        <div>
          <div className="text-xs text-muted">Host access (3306)</div>
          <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{exportUrl}</span>
            <CopyButton text={exportUrl} />
          </div>
        </div>
      )}
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="outline" size="sm" className="mt-2 w-full" onClick={onOpenTerminal}>
        <Icon.Nodes size={16} /> Open root console
      </Button>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function CredsTab({ cfg, sec }) {
  const rows = [
    { k: 'Root user', v: sec.rootUser || 'root' },
    { k: 'Root password', v: sec.rootPassword },
    { k: 'App user', v: sec.appUser || 'app' },
    { k: 'App password', v: sec.appPassword },
    { k: 'Repl user', v: sec.replUser || 'repl' },
    { k: 'Repl password', v: sec.replPassword },
    { k: 'Monitor user', v: sec.monitorUser || 'monitor' },
    { k: 'Monitor password', v: sec.monitorPassword },
  ]
  return (
    <div className="space-y-2">
      <div className="text-[11px] text-muted">App/repl/monitor passwords come from the host .env (APP_PASSWORD / REPL_PASSWORD / MONITOR_PASSWORD).</div>
      {rows.map((r) => (
        <div key={r.k}>
          <div className="text-xs text-muted">{r.k}</div>
          <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
            <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{r.v || '—'}</span>
            {r.v && <CopyButton text={r.v} />}
          </div>
        </div>
      ))}
    </div>
  )
}

function CertTab({ api, cfg }) {
  const [info, setInfo] = useState('')
  const [value, setValue] = useState(cfg.certTtlValue || 365)
  const [unit, setUnit] = useState(cfg.certTtlUnit || 'days')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    try { setInfo((await api.certInfo()).info || '') } catch (e) { setErr(e.message) }
  }, [api])
  useEffect(() => { load() }, [load])

  async function generate() {
    setBusy(true); setErr('')
    try { await api.certGenerate(Number(value), unit); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  return (
    <div className="space-y-3">
      <Err>{err}</Err>
      <div>
        <div className="mb-1 text-xs font-medium text-muted">Current certificate</div>
        <pre className="whitespace-pre-wrap break-all rounded-lg border bg-bg p-2 text-xs text-fg">{info || '—'}</pre>
        <div className="mt-1 text-xs text-muted">
          mysqld serves <span className="font-mono">/var/lib/mysql/server-cert.pem</span> (+ client-cert.pem, ca.pem). Signed by the Intranet CA.
        </div>
      </div>
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Re-issue from Intranet CA (overwrites the cert files in place)</div>
        <div className="flex gap-1">
          <input type="number" min="1" className={inputCls} value={value} onChange={(e) => setValue(e.target.value)} />
          <select className={inputCls} value={unit} onChange={(e) => setUnit(e.target.value)}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
        <Button size="sm" className="w-full" disabled={busy} onClick={generate}>
          {busy ? 'Generating…' : 'Generate certificate'}
        </Button>
        <div className="text-[11px] text-muted">Requires a running Intranet node. The new cert is written in place — restart this node's mysqld yourself to apply it.</div>
      </div>
    </div>
  )
}
