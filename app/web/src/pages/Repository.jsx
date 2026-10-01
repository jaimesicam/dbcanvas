import { useEffect, useMemo, useState } from 'react'
import { Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { stackApi, repositoryApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { useFollowedState } from '../session/SessionProvider.jsx'

// Repository — a yum/apt mirror of the Percona repositories plus a Docker registry, carrying only
// what the design names (app/repository.go). Three things live here:
//
//   RepositoryForm    — the design: OS releases × architectures × repositories (optionally narrowed
//                       to versions and package names), images, and whole operator releases.
//   RepositoryManager — the running node: what it holds, how to use it (yum, apt, cr.yaml, Helm,
//                       containerd), adding more, and the sync log.
//   RepositoryPicker  — the field every Percona-installing node and frame gets to use one.
//
// The design and the "Add" tab share one editor, because they ask the same question: what should
// this node carry?

const OPERATOR_LABEL = {
  pxc: 'Percona XtraDB Cluster',
  ps: 'Percona Server for MySQL',
  psmdb: 'Percona Server for MongoDB',
  pg: 'Percona PostgreSQL',
}

function useRepositoryCatalog() {
  const [cat, setCat] = useState(null)
  useEffect(() => {
    let alive = true
    stackApi.repositoryCatalog().then((c) => { if (alive) setCat(c) }).catch(() => { /* the editor still takes typed names */ })
    return () => { alive = false }
  }, [])
  return cat
}

function CopyButton({ text, size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button
      title="Copy"
      type="button"
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

function Snippet({ children }) {
  const text = String(children).replace(/^\n+|\s+$/g, '')
  return (
    <div className="relative rounded-lg border bg-bg">
      <div className="absolute right-1 top-1"><CopyButton text={text} /></div>
      <pre className="overflow-x-auto whitespace-pre px-3 py-2 pr-8 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

function KV({ k, v, mono, help }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="flex shrink-0 items-center gap-1 text-muted">{k}<Help text={help} /></span>
      <span className={`truncate text-right text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

const bytes = (n) => {
  if (!n) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++ }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`
}

const splitWords = (s) => String(s || '').split(/[\s,]+/).map((x) => x.trim()).filter(Boolean)

// ------------------------------------------------------------------ the content editor

// RepoContentEditor edits { packages, images, operators }. `packages` rows are
// { repo, versions: [], packages: [] }: versions empty → the newest build, "*" → every build.
export function RepoContentEditor({ value, onChange, cat, disabled = false }) {
  const packages = value.packages || []
  const operators = value.operators || []
  const images = value.images || []
  const sugg = cat?.suggestions || []
  const byRepo = useMemo(() => Object.fromEntries(sugg.map((s) => [s.repo, s])), [sugg])
  const groups = useMemo(() => {
    const g = {}
    for (const s of sugg) (g[s.group] = g[s.group] || []).push(s)
    return g
  }, [sugg])
  const set = (patch) => onChange({ packages, images, operators, ...patch })
  const setRow = (i, patch) => set({ packages: packages.map((p, j) => (j === i ? { ...p, ...patch } : p)) })
  const setOp = (i, patch) => set({ operators: operators.map((o, j) => (j === i ? { ...o, ...patch } : o)) })
  const lock = disabled ? 'opacity-70' : ''

  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <div className="flex items-center gap-1 text-sm font-medium">Package repositories<Help text={HELP.repoPackages} /></div>
        {packages.length === 0 && <p className="text-xs text-muted">None yet.</p>}
        {packages.map((p, i) => {
          const known = byRepo[p.repo]
          const versions = p.versions || []
          const offered = (known?.versions || []).filter((v) => !versions.includes(v))
          return (
            <div key={i} className="space-y-2 rounded-lg border bg-surface2/60 p-2">
              <div className="flex items-center gap-2">
                <input className={`${inputCls} font-mono ${lock} ${!p.repo ? 'border-danger' : ''}`} list="dbc-repo-suggestions" placeholder="e.g. ps-84-lts"
                  value={p.repo || ''} disabled={disabled} autoFocus={!p.repo && !disabled} onChange={(e) => setRow(i, { repo: e.target.value.trim() })} />
                {!disabled && (
                  <button type="button" title="Remove" className="rounded p-1 text-muted hover:text-fg"
                    onClick={() => set({ packages: packages.filter((_, j) => j !== i) })}><Icon.Trash size={15} /></button>
                )}
              </div>
              {known && <div className="text-[11px] text-muted">{known.label}</div>}
              {!p.repo && <div className="text-[11px] text-danger">Type a repository name (e.g. ps-84-lts) or remove this row. It is ignored while blank.</div>}
              <div className="flex flex-wrap items-center gap-1">
                <span className="text-xs text-muted">Versions:</span>
                {versions.length === 0 && <span className="rounded bg-surface px-1.5 py-0.5 text-[11px]">newest build only</span>}
                {versions.map((v) => (
                  <span key={v} className="inline-flex items-center gap-1 rounded bg-primary/10 px-1.5 py-0.5 font-mono text-[11px] text-primary">
                    {v === '*' ? 'every build' : v}
                    {!disabled && <button type="button" onClick={() => setRow(i, { versions: versions.filter((x) => x !== v) })}>×</button>}
                  </span>
                ))}
              </div>
              {!disabled && (
                <div className="flex gap-2">
                  <select className={`${inputCls} text-xs`} value=""
                    onChange={(e) => { if (e.target.value) setRow(i, { versions: e.target.value === '*' ? ['*'] : [...versions.filter((x) => x !== '*'), e.target.value] }) }}>
                    <option value="">Add a version…</option>
                    {offered.map((v) => <option key={v} value={v}>{v}</option>)}
                    <option value="*">Every build (large)</option>
                  </select>
                  <input className={`${inputCls} text-xs font-mono`} placeholder="or type one, Enter"
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter') return
                      e.preventDefault()
                      const v = e.currentTarget.value.trim()
                      if (v && !versions.includes(v)) setRow(i, { versions: [...versions.filter((x) => x !== '*'), v] })
                      e.currentTarget.value = ''
                    }} />
                </div>
              )}
              <input className={`${inputCls} text-xs font-mono ${lock}`} disabled={disabled}
                placeholder="Packages (blank = all). e.g. percona-server-server percona-xtrabackup-*"
                defaultValue={(p.packages || []).join(' ')}
                onBlur={(e) => setRow(i, { packages: splitWords(e.target.value) })} />
            </div>
          )
        })}
        {!disabled && (
          <select className={`${inputCls} text-sm`} value=""
            onChange={(e) => {
              if (!e.target.value) return
              set({ packages: [...packages, { repo: e.target.value === '__other' ? '' : e.target.value, versions: [], packages: [] }] })
            }}>
            <option value="">+ Add a repository…</option>
            {Object.entries(groups).map(([g, list]) => (
              <optgroup key={g} label={g}>
                {list.map((s) => <option key={s.repo} value={s.repo}>{s.label} ({s.repo})</option>)}
              </optgroup>
            ))}
            <option value="__other">Other (type its name)…</option>
          </select>
        )}
        <datalist id="dbc-repo-suggestions">
          {sugg.map((s) => <option key={s.repo} value={s.repo}>{s.label}</option>)}
        </datalist>
      </div>

      <div className="space-y-2">
        <div className="flex items-center gap-1 text-sm font-medium">Kubernetes operators<Help text={HELP.repoOperators} /></div>
        {operators.map((o, i) => {
          const vers = cat?.operators?.[o.kind]?.versions || []
          return (
            <div key={i} className="flex items-center gap-2">
              <select className={`${inputCls} ${lock}`} value={o.kind} disabled={disabled} onChange={(e) => setOp(i, { kind: e.target.value, version: '' })}>
                {Object.entries(OPERATOR_LABEL).map(([k, l]) => <option key={k} value={k}>{l}</option>)}
              </select>
              <select className={`${inputCls} w-36 font-mono ${lock}`} value={o.version || ''} disabled={disabled} onChange={(e) => setOp(i, { version: e.target.value })}>
                <option value="">latest{cat?.operators?.[o.kind]?.latest ? ` (${cat.operators[o.kind].latest})` : ''}</option>
                {vers.map((v) => <option key={v} value={v}>{v}</option>)}
              </select>
              {!disabled && (
                <button type="button" title="Remove" className="rounded p-1 text-muted hover:text-fg"
                  onClick={() => set({ operators: operators.filter((_, j) => j !== i) })}><Icon.Trash size={15} /></button>
              )}
            </div>
          )
        })}
        {!disabled && (
          <Button size="sm" variant="ghost" onClick={() => set({ operators: [...operators, { kind: 'pxc', version: '' }] })}>
            <Icon.Plus size={14} /> Add an operator release
          </Button>
        )}
      </div>

      <Field label="Extra container images" help={HELP.repoImages} hint="One per line, e.g. percona/pmm-client:3 or quay.io/org/image:tag.">
        <textarea className={`${inputCls} h-20 font-mono text-xs ${lock}`} disabled={disabled}
          defaultValue={images.join('\n')} onBlur={(e) => set({ images: splitWords(e.target.value) })} />
      </Field>
    </div>
  )
}

// ------------------------------------------------------------------ design form

export function RepositoryForm({ node: n, patchNode, deleteNode, dep, deployed }) {
  const cat = useRepositoryCatalog()
  const targets = n.repoTargets?.length ? n.repoTargets : ['oraclelinux-9']
  const arches = n.repoArches?.length ? n.repoArches : [cat?.defaultArch || 'amd64']
  const toggle = (list, v) => (list.includes(v) ? list.filter((x) => x !== v) : [...list, v])
  const lock = deployed ? 'opacity-70' : ''
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Repository</span>
        {dep && <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>}
      </div>
      <p className="text-xs text-muted">
        A yum/apt mirror of the Percona repositories and a container registry, carrying only what you
        list here. Database nodes, clusters and K3D frames install from it once you pick it in their
        Repository field. Anything it does not carry still comes from upstream.
      </p>
      <Field label="Label" help={HELP.label} hint="Becomes the node hostname; must be unique.">
        <input className={inputCls} value={n.label} onChange={(e) => patchNode(n.id, { label: e.target.value })} />
      </Field>

      <div className="space-y-1">
        <div className="flex items-center gap-1 text-sm font-medium">OS releases<Help text={HELP.repoTargets} /></div>
        <div className="grid grid-cols-2 gap-1">
          {(cat?.targets || [{ os: 'oraclelinux', version: '9', label: 'Oracle Linux 9 (el9)' }]).map((t) => {
            const id = `${t.os}-${t.version}`
            return (
              <label key={id} className={`flex items-center gap-2 text-xs ${lock}`}>
                <input type="checkbox" disabled={deployed} checked={targets.includes(id)}
                  onChange={() => { const next = toggle(targets, id); if (next.length) patchNode(n.id, { repoTargets: next }) }} />
                {t.label}
              </label>
            )
          })}
        </div>
      </div>
      <div className="space-y-1">
        <div className="flex items-center gap-1 text-sm font-medium">Architectures<Help text={HELP.repoArches} /></div>
        <div className="flex gap-4">
          {['amd64', 'arm64'].map((a) => (
            <label key={a} className={`flex items-center gap-2 text-xs ${lock}`}>
              <input type="checkbox" disabled={deployed} checked={arches.includes(a)}
                onChange={() => { const next = toggle(arches, a); if (next.length) patchNode(n.id, { repoArches: next }) }} />
              {a}
            </label>
          ))}
        </div>
      </div>

      <RepoContentEditor cat={cat} disabled={deployed}
        value={{ packages: n.repoPackages || [], images: n.repoImages || [], operators: n.repoOperators || [] }}
        onChange={(v) => patchNode(n.id, { repoPackages: v.packages, repoImages: v.images, repoOperators: v.operators })} />

      <label className={`flex items-center gap-2 text-sm ${lock}`}>
        <input type="checkbox" checked={!!n.repoDebug} disabled={deployed} onChange={(e) => patchNode(n.id, { repoDebug: e.target.checked })} />
        <span>Include debug symbols (-debuginfo / -dbgsym)</span><Help text={HELP.repoDebug} />
      </label>
      <label className={`flex items-center gap-2 text-sm ${lock}`}>
        <input type="checkbox" checked={!!n.repoStrict} disabled={deployed} onChange={(e) => patchNode(n.id, { repoStrict: e.target.checked })} />
        <span>Strict: nodes use nothing else from repo.percona.com</span><Help text={HELP.repoStrict} />
      </label>
      <label className={`flex items-center gap-2 text-sm ${lock}`}>
        <input type="checkbox" checked={!!n.useProxy} disabled={deployed} onChange={(e) => patchNode(n.id, { useProxy: e.target.checked })} />
        <span>Use Intranet proxy (Squid) for downloads</span><Help text={HELP.proxy} />
      </label>
      {!deployed && <p className="text-xs text-muted">Deploying downloads everything listed, so the first deploy takes as long as that takes. You can add more once it is running.</p>}
      <Button variant="danger" size="sm" className="w-full" onClick={() => deleteNode(n.id)}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// ------------------------------------------------------------------ association picker

// RepositoryPicker is the field a node or frame uses to install from a Repository node. It is not
// shown until the canvas has a Repository, so it costs nothing on a stack that does not use one.
export function RepositoryPicker({ value, nodes, deployed, onChange, k3d = false }) {
  const repos = (nodes || []).filter((x) => x.type === 'repository')
  if (!repos.length && !value) return null
  const missing = value && !repos.some((r) => r.id === value)
  return (
    <Field label="Repository" help={k3d ? HELP.repositoryK3D : HELP.repository}
      hint={missing ? 'That Repository is no longer on the canvas.'
        : value ? (k3d ? 'Images pull through its registry; the operator comes from it when it carries that release.'
          : 'Percona packages come from it, for what it carries; the rest from repo.percona.com.')
          : 'Packages come from repo.percona.com.'}>
      <select className={`${inputCls} ${deployed ? 'opacity-70' : ''}`} value={value || ''} disabled={deployed}
        onChange={(e) => onChange(e.target.value)}>
        <option value="">none — upstream</option>
        {repos.map((r) => <option key={r.id} value={r.id}>{r.label}</option>)}
        {missing && <option value={value}>(missing)</option>}
      </select>
    </Field>
  )
}

// ------------------------------------------------------------------ running node

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'guide', label: 'How to use' },
  { id: 'add', label: 'Add packages' },
  { id: 'log', label: 'Sync log' },
]

export function RepositoryManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useFollowedState(`tab:${nodeId}`, 'overview')
  const cfg = dep.config || {}
  const syncing = cfg.sync?.state === 'running'
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Repository</span>
        <div className="flex items-center gap-1">
          {syncing && <Badge tone="warn">syncing</Badge>}
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </div>
      </div>
      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.map((t) => (
          <button key={t.id} type="button" onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>
      {tab === 'overview' && <Overview stackId={stackId} nodeId={nodeId} cfg={cfg} dep={dep} onDeleteNode={onDeleteNode} />}
      {tab === 'guide' && <GuideTab cfg={cfg} />}
      {tab === 'add' && <AddTab stackId={stackId} nodeId={nodeId} cfg={cfg} onAdded={() => setTab('log')} />}
      {tab === 'log' && <LogTab cfg={cfg} />}
    </div>
  )
}

function useUrls(cfg) {
  const host = typeof location !== 'undefined' ? location.hostname : 'localhost'
  return {
    browse: cfg.httpPort ? `http://${host}:${cfg.httpPort}/` : null,
    base: `http://${cfg.fqdn}`,
    registry: `${cfg.fqdn}:5000`,
    hostRegistry: cfg.registryPort ? `${host === 'localhost' || host === '127.0.0.1' ? 'localhost' : host}:${cfg.registryPort}` : null,
  }
}

function SyncStatus({ cfg }) {
  const s = cfg.sync || {}
  if (!s.state) return null
  const tone = s.state === 'running' ? 'text-warn' : s.state === 'error' ? 'text-danger' : 'text-success'
  return (
    <div className="rounded-lg bg-surface2 px-3 py-2 text-xs">
      <div className={`font-medium ${tone}`}>{s.state === 'running' ? (s.phase || 'Syncing…') : s.message}</div>
      <div className="text-muted">{s.state === 'running' ? `started ${s.started || ''}` : `finished ${s.finished || ''}`}</div>
    </div>
  )
}

function Overview({ stackId, nodeId, cfg, dep, onDeleteNode }) {
  const urls = useUrls(cfg)
  const c = cfg.contents || {}
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const repos = c.repos || []
  const images = c.images || []
  return (
    <div className="space-y-3 text-sm">
      {urls.browse && (
        <a href={urls.browse} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Browse the repository
        </a>
      )}
      <SyncStatus cfg={cfg} />
      <div className="space-y-1.5">
        <KV k="Inside the stack" v={urls.base + '/'} mono />
        <KV k="Registry" v={urls.registry} mono />
        <KV k="Registry from this host" v={urls.hostRegistry} mono />
        <KV k="OS releases" v={(cfg.targets || []).join(', ')} mono />
        <KV k="Architectures" v={(cfg.arches || []).join(', ')} mono />
        <KV k="On disk" v={bytes(c.diskBytes)} />
        <KV k="Mode" v={cfg.strict ? 'strict — nodes use nothing else from repo.percona.com' : 'fallback — what it lacks comes from upstream'} />
      </div>

      <Section title={`Package repositories (${repos.filter((r) => !r.error).length})`}>
        {repos.length === 0 && <p className="text-xs text-muted">None.</p>}
        {repos.map((r, i) => (
          <div key={i} className="flex items-baseline justify-between gap-2 border-b py-1 text-xs last:border-0">
            <span className="min-w-0">
              <span className="font-mono">{r.repo}</span>{' '}
              <span className="text-muted">{r.target} · {r.arch}</span>
              {r.error && <div className="text-danger">{r.error}</div>}
              {!r.error && <div className="text-muted">{(r.versions || []).join(', ')}</div>}
            </span>
            {!r.error && <span className="shrink-0 text-muted">{r.packages} pkgs · {bytes(r.bytes)}</span>}
          </div>
        ))}
      </Section>

      {(c.operators || []).length > 0 && (
        <Section title="Operators">
          {c.operators.map((o, i) => (
            <div key={i} className="border-b py-1 text-xs last:border-0">
              <span className="font-mono">{o.kind} {o.version}</span>
              <span className="text-muted"> · {(o.images || []).length} images{o.charts?.length ? ` · ${o.charts.join(', ')}` : ''}</span>
              {o.error && <div className="text-danger">{o.error}</div>}
            </div>
          ))}
        </Section>
      )}

      <Section title={`Images (${images.filter((x) => !x.error).length})`}>
        {images.length === 0 && <p className="text-xs text-muted">None.</p>}
        {images.map((m, i) => (
          <div key={i} className="border-b py-1 text-xs last:border-0">
            <div className="truncate font-mono">{m.source}</div>
            {m.error ? <div className="text-danger">{m.error}</div> : m.from && <div className="text-muted">from {m.from}</div>}
          </div>
        ))}
      </Section>

      {err && <p className="text-xs text-danger">{err}</p>}
      <Button size="sm" variant="ghost" className="w-full" disabled={busy || cfg.sync?.state === 'running'}
        onClick={async () => {
          setBusy(true); setErr('')
          try { await repositoryApi(stackId, nodeId).sync() } catch (e) { setErr(e.message || String(e)) }
          setBusy(false)
        }}>
        <Icon.Refresh size={14} /> Re-sync from upstream
      </Button>
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function Section({ title, children }) {
  return (
    <div className="rounded-lg border px-3 py-2">
      <div className="mb-1 text-xs font-medium text-fg/80">{title}</div>
      {children}
    </div>
  )
}

// GuideTab answers "how do I point X at this?" for each kind of client, with this node's own names
// and a repository it actually carries in every snippet.
const GUIDES = [
  { id: 'yum', label: 'yum / dnf' },
  { id: 'apt', label: 'apt' },
  { id: 'cr', label: 'Operator (cr.yaml)' },
  { id: 'helm', label: 'Operator (Helm)' },
  { id: 'mirror', label: 'containerd / Docker' },
]

export function GuideTab({ cfg, initial = 'yum' }) {
  const [g, setG] = useState(initial)
  const urls = useUrls(cfg)
  const c = cfg.contents || {}
  const ok = (c.repos || []).filter((r) => !r.error)
  const yumRepo = ok.find((r) => r.family === 'yum')?.repo || 'ps-84-lts'
  const aptRepo = ok.find((r) => r.family === 'apt')?.repo || yumRepo
  const op = (c.operators || []).find((o) => !o.error)
  const chart = (c.charts || []).find((x) => op && x.name.startsWith(op.kind + '-operator'))
  const dbChart = (c.charts || []).find((x) => op && x.name.startsWith(op.kind + '-db'))
  const firstImage = (c.images || []).find((x) => !x.error)
  const B = urls.base
  return (
    <div className="space-y-3 text-sm">
      <div className="flex flex-wrap gap-1">
        {GUIDES.map((x) => (
          <button key={x.id} type="button" onClick={() => setG(x.id)}
            className={`rounded-full border px-2.5 py-0.5 text-xs ${g === x.id ? 'border-primary bg-primary/10 text-primary' : 'text-muted'}`}>
            {x.label}
          </button>
        ))}
      </div>
      <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
        <b className="text-fg/80">Nodes on this canvas:</b> pick this Repository in the node's or frame's
        Repository field before deploying and DBCanvas does all of this for you. The steps below are for
        doing it by hand: on a Linux Client, a node deployed without it, or anything else that resolves{' '}
        <span className="font-mono">{cfg.fqdn}</span>. From outside the stack, use{' '}
        <span className="font-mono">{urls.browse || '(its published port)'}</span> instead.
      </div>

      {g === 'yum' && (
        <div className="space-y-2">
          <p className="text-xs">The tree has the same layout as repo.percona.com, under <span className="font-mono">/percona/</span>. The RPMs are Percona's own and keep Percona's signature, so <span className="font-mono">gpgcheck</span> stays on.</p>
          <div className="text-xs font-medium">Keep percona-release, change the host</div>
          <Snippet>{`percona-release setup -y ${yumRepo}
sed -i -E 's#https?://repo\\.percona\\.com/(${yumRepo})/#${B}/percona/\\1/#' /etc/yum.repos.d/percona-*.repo
dnf clean metadata
dnf install percona-server-server   # or whatever ${yumRepo} carries`}</Snippet>
          <div className="text-xs font-medium">Or write the .repo file yourself</div>
          <Snippet>{`cat >/etc/yum.repos.d/dbcanvas-${yumRepo}.repo <<'EOF'
[dbcanvas-${yumRepo}]
name=${yumRepo} from ${cfg.hostname}
baseurl=${B}/percona/${yumRepo}/yum/release/$releasever/RPMS/$basearch/
gpgcheck=1
gpgkey=${B}/percona/yum/PERCONA-PACKAGING-KEY
enabled=1
EOF
dnf --disablerepo='*' --enablerepo='dbcanvas-${yumRepo}' list available`}</Snippet>
          <p className="text-xs text-muted">What a node that uses this Repository runs is in <span className="font-mono">/usr/local/sbin/dbcanvas-repo-rewrite</span>: it does the <span className="font-mono">sed</span> above before every dnf, for the repositories listed in <span className="font-mono">{B}/dbcanvas/carried/</span>.</p>
        </div>
      )}

      {g === 'apt' && (
        <div className="space-y-2">
          <p className="text-xs">The .deb files are Percona's, but the index is a subset, so this Repository signs its own <span className="font-mono">Release</span>. Trust its key instead of Percona's for these lines.</p>
          <Snippet>{`install -d /etc/apt/keyrings
curl -fsSL ${B}/dbcanvas-repo.gpg -o /etc/apt/keyrings/dbcanvas-repo.gpg
echo "deb [signed-by=/etc/apt/keyrings/dbcanvas-repo.gpg] ${B}/percona/${aptRepo}/apt $(. /etc/os-release; echo $VERSION_CODENAME) main" \\
  >/etc/apt/sources.list.d/dbcanvas-${aptRepo}.list
apt-get update
apt-cache policy percona-server-server   # the versions it carries`}</Snippet>
          <p className="text-xs text-muted">To keep a <span className="font-mono">percona-release</span> line instead, change its host to <span className="font-mono">{B}/percona</span>, its <span className="font-mono">signed-by</span> to the key above, and comment out its <span className="font-mono">deb-src</span> line: source packages are not mirrored.</p>
        </div>
      )}

      {g === 'cr' && (
        <div className="space-y-2">
          {!op && <p className="text-xs text-warn">This Repository carries no operator release yet. Add one on the Add packages tab.</p>}
          <p className="text-xs">Each operator release has the upstream <span className="font-mono">bundle.yaml</span>, <span className="font-mono">cr.yaml</span> and <span className="font-mono">secrets.yaml</span>, plus <span className="font-mono">*.local.yaml</span> copies with every <span className="font-mono">image:</span> pointed at <span className="font-mono">{urls.registry}</span>.</p>
          <div className="text-xs font-medium">With images rewritten</div>
          <Snippet>{`kubectl create namespace db
kubectl apply --server-side -n db -f ${B}/operators/${op?.kind || 'pxc'}/${op?.version || '<version>'}/bundle.local.yaml
kubectl apply -n db -f ${B}/operators/${op?.kind || 'pxc'}/${op?.version || '<version>'}/cr.local.yaml`}</Snippet>
          <p className="text-xs text-muted">The registry is plain HTTP, so the cluster's containerd has to be told it may use it. For k3s, that is the <span className="font-mono">configs</span>/<span className="font-mono">mirrors</span> file on the containerd / Docker tab. With that mirror in place you can apply the <b>unmodified</b> <span className="font-mono">bundle.yaml</span> and <span className="font-mono">cr.yaml</span> instead: containerd fetches docker.io images from here first.</p>
          <div className="text-xs font-medium">Just the source</div>
          <Snippet>{`curl -fsSLO ${B}/operators/${op?.kind || 'pxc'}/${op?.version || '<version>'}/source.tar.gz   # = the GitHub tag tarball`}</Snippet>
        </div>
      )}

      {g === 'helm' && (
        <div className="space-y-2">
          {!chart && <p className="text-xs text-warn">No Helm chart is mirrored yet. Charts come with an operator release (Add packages → Kubernetes operators).</p>}
          <Snippet>{`helm repo add dbcanvas ${B}/charts
helm repo update
helm search repo dbcanvas
helm install ${op?.kind || 'pxc'}-operator dbcanvas/${chart?.name || 'pxc-operator'}${chart ? ` --version ${chart.version}` : ''} -n db --create-namespace
helm install my-db dbcanvas/${dbChart?.name || 'pxc-db'}${dbChart ? ` --version ${dbChart.version}` : ''} -n db`}</Snippet>
          <p className="text-xs text-muted">The charts are Percona's and still name docker.io images. With the containerd mirror (next tab) that needs no change. Without it, override each <span className="font-mono">image</span> value with <span className="font-mono">{urls.registry}/…</span>; <span className="font-mono">helm show values dbcanvas/{dbChart?.name || 'pxc-db'} | grep -n image</span> lists them.</p>
        </div>
      )}

      {g === 'mirror' && (
        <div className="space-y-2">
          <div className="text-xs font-medium">k3s / k3d (containerd)</div>
          <p className="text-xs">A K3D frame whose Repository field names this node gets this file at create time. By hand:</p>
          <Snippet>{`curl -fsSL ${B}/dbcanvas/registries.yaml -o /etc/rancher/k3s/registries.yaml
systemctl restart k3s        # k3s reads it only at start
# k3d: k3d cluster create --registry-config registries.yaml …`}</Snippet>
          <p className="text-xs text-muted">containerd tries this registry first and falls back to the real one for anything it does not hold.</p>
          <div className="text-xs font-medium">Docker / Podman on this host</div>
          <Snippet>{`docker pull ${urls.hostRegistry || 'localhost:<port>'}/${firstImage?.local || 'percona/percona-xtradb-cluster:8.0'}
curl -s http://${urls.hostRegistry || 'localhost:<port>'}/v2/_catalog`}</Snippet>
          <p className="text-xs text-muted">Docker allows plain HTTP to <span className="font-mono">localhost</span>. From anywhere else, add <span className="font-mono">"{urls.registry}"</span> to <span className="font-mono">insecure-registries</span> in <span className="font-mono">/etc/docker/daemon.json</span>.</p>
        </div>
      )}
    </div>
  )
}

function AddTab({ stackId, nodeId, cfg, onAdded }) {
  const cat = useRepositoryCatalog()
  const [value, setValue] = useState({ packages: [], images: [], operators: [] })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [key, setKey] = useState(0)
  const syncing = cfg.sync?.state === 'running'
  const empty = !value.packages.some((p) => p.repo) && !value.operators.length && !value.images.length
  const added = cfg.added || {}
  return (
    <div className="space-y-3 text-sm">
      <p className="text-xs text-muted">
        Downloads more into this Repository, for its OS releases ({(cfg.targets || []).join(', ')}) and
        architectures ({(cfg.arches || []).join(', ')}). What is already there stays. Nodes that already use
        it pick up newly carried repositories on their next dnf or apt run.
      </p>
      <RepoContentEditor key={key} cat={cat} value={value} onChange={setValue} />
      {err && <p className="text-xs text-danger">{err}</p>}
      {syncing && <p className="text-xs text-warn">A sync is running. Add these when it finishes.</p>}
      <Button className="w-full" disabled={busy || syncing || empty}
        onClick={async () => {
          setBusy(true); setErr('')
          try {
            await repositoryApi(stackId, nodeId).add({
              packages: value.packages.filter((p) => p.repo),
              images: value.images,
              operators: value.operators,
            })
            setValue({ packages: [], images: [], operators: [] })
            setKey((k) => k + 1)
            onAdded()
          } catch (e) { setErr(e.message || String(e)) }
          setBusy(false)
        }}>
        <Icon.Plus size={15} /> Download and add
      </Button>
      {(added.packages?.length || added.images?.length || added.operators?.length) ? (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] text-muted">
          <div className="mb-1 font-medium text-fg/80">Added since deploy</div>
          {(added.packages || []).map((p, i) => <div key={`p${i}`} className="font-mono">{p.repo} {(p.versions || []).join(' ') || 'latest'} {(p.packages || []).join(' ')}</div>)}
          {(added.operators || []).map((o, i) => <div key={`o${i}`} className="font-mono">operator {o.kind} {o.version || 'latest'}</div>)}
          {(added.images || []).map((m) => <div key={m} className="font-mono">{m}</div>)}
        </div>
      ) : null}
    </div>
  )
}

export function LogTab({ cfg }) {
  const log = cfg.sync?.log || []
  return (
    <div className="space-y-2">
      <SyncStatus cfg={cfg} />
      <pre className="max-h-96 overflow-auto rounded-lg border bg-bg px-3 py-2 font-mono text-[11px] leading-relaxed text-fg">
        {log.length ? log.join('\n') : 'No sync has run yet.'}
      </pre>
    </div>
  )
}
