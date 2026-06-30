import { useCallback, useEffect, useState } from 'react'
import { Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { pmmApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'access', label: 'Access' },
  { id: 'cert', label: 'Certificate' },
]

function CopyButton({ text, title = 'Copy', size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button
      title={title}
      onClick={async () => {
        try { await navigator.clipboard.writeText(text) } catch { /* ignore */ }
        setDone(true)
        setTimeout(() => setDone(false), 1200)
      }}
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
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

export default function PMMManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const api = pmmApi(stackId, nodeId)
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">PMM3</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && (
        <Overview
          cfg={cfg}
          dep={dep}
          onDeleteNode={onDeleteNode}
          onRootConsole={() => openTerminal({ stackId, nodeId, title: 'pmm · root', user: '0' })}
          onPmmConsole={() => openTerminal({ stackId, nodeId, title: 'pmm · pmm' })}
        />
      )}
      {tab === 'access' && <AccessTab cfg={cfg} sec={sec} />}
      {tab === 'cert' && <CertTab api={api} generateCert={cfg.generateCert} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode, onRootConsole, onPmmConsole }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="Image" v={cfg.image} mono />
      <KV k="Version" v={cfg.version} />
      <KV k="Arch" v={cfg.arch} />
      <KV k="Network alias" v={cfg.alias} mono />
      <KV k="Grafana SMTP" v={cfg.smtpHost} mono />
      <KV k="TLS certificate" v={cfg.generateCert ? 'Intranet CA-signed' : 'self-signed (default)'} />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      {Array.isArray(cfg.services) && (
        <div className="flex flex-wrap gap-1 pt-1">
          {cfg.services.map((s) => <Badge key={s} tone="primary">{s}</Badge>)}
        </div>
      )}
      <div className="mt-2 grid grid-cols-2 gap-2">
        <Button variant="outline" size="sm" onClick={onRootConsole}>
          <Icon.Nodes size={16} /> Root console
        </Button>
        <Button variant="outline" size="sm" onClick={onPmmConsole}>
          <Icon.Nodes size={16} /> PMM console
        </Button>
      </div>
      <p className="text-[11px] text-muted">The PMM console logs in as the unprivileged <span className="font-mono">pmm</span> user; the root console execs as uid 0.</p>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ----------------------------------------------------------------- access tab

function AccessTab({ cfg, sec }) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  const httpUrl = cfg.httpPort ? `http://${host}:${cfg.httpPort}/` : null
  const httpsUrl = cfg.httpsPort ? `https://${host}:${cfg.httpsPort}/` : null
  const rows = [
    { k: 'Admin user', v: cfg.adminUser || sec.adminUser || 'admin' },
    { k: 'Admin password', v: sec.adminPassword },
  ]
  return (
    <div className="space-y-3">
      {httpsUrl && (
        <a href={httpsUrl} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open PMM (HTTPS · 8443)
        </a>
      )}
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Service endpoints</div>
        <Endpoint label="HTTP · 8080" url={httpUrl} />
        <Endpoint label="HTTPS · 8443" url={httpsUrl} />
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Credentials</div>
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
    </div>
  )
}

function Endpoint({ label, url }) {
  if (!url) return null
  return (
    <div>
      <div className="text-xs text-muted">{label}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <a href={url} target="_blank" rel="noreferrer" className="min-w-0 flex-1 truncate font-mono text-xs text-primary hover:underline">{url}</a>
        <CopyButton text={url} />
      </div>
    </div>
  )
}

// ------------------------------------------------------------------ cert tab

function CertTab({ api, generateCert }) {
  const [info, setInfo] = useState('')
  const [value, setValue] = useState(365)
  const [unit, setUnit] = useState('days')
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
          nginx serves <span className="font-mono">/srv/nginx/certificate.crt</span>.
          {generateCert ? ' Signed by the Intranet CA.' : ' Currently the image default (self-signed).'}
        </div>
      </div>
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Generate from Intranet CA (archives the existing certs)</div>
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
        <div className="text-[11px] text-muted">
          Existing <span className="font-mono">/srv/nginx</span> certs are archived to
          <span className="font-mono"> /srv/nginx/archive/&lt;timestamp&gt;</span> before replacement. Requires a running Intranet node.
        </div>
      </div>
    </div>
  )
}
