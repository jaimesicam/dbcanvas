import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, Button, Card, ConfirmButton, Field, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { CopyButton } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import {
  apiApi, curlFor, cliFor, matches, samplePath, expiryText, relDate,
  METHOD_TONE, SCOPE_TEXT, MEDIA_TEXT, MEDIA_LABEL, TOKEN_STATE_TONE, EXPIRY_CHOICES,
} from '../lib/apiApi.js'

// Api.jsx — the page that documents the API and hands out the credentials for it.
//
// Two halves that belong together for one reason: a reference is useless without a
// token, and a token is useless without knowing what to point it at. Somebody
// arriving here wants to make one HTTP call succeed, and this page is the shortest
// path from "I have a password" to "that returned JSON".
//
// The endpoint list is rendered from GET /api/meta/endpoints — never from anything
// written here. Both the curl line and the CLI line are derived from each route
// record in the browser, so there is exactly one description of the API and it lives
// on the server.

export default function Api() {
  // The role comes from GET /api/tokens rather than from AuthProvider. Both know
  // it, but taking it from the payload keeps this page renderable on its own —
  // which is what lets the render smoke test mount the whole thing and catch a
  // missing icon in the tab bar before anybody opens the page.
  const [isAdmin, setIsAdmin] = useState(false)
  const [tab, setTab] = useState('tokens')

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        {[
          { id: 'tokens', label: 'Tokens', icon: 'Key' },
          { id: 'endpoints', label: 'Endpoints', icon: 'Code' },
          { id: 'start', label: 'Getting started', icon: 'Help' },
        ].map((t) => {
          const Ico = Icon[t.icon]
          return (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={`inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-sm transition ${
                tab === t.id ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2 hover:text-fg'
              }`}
            >
              <Ico size={16} />
              <span className="font-medium">{t.label}</span>
            </button>
          )
        })}
      </div>

      {tab === 'tokens' && <Tokens isAdmin={isAdmin} onRole={(r) => setIsAdmin(r === 'admin')} />}
      {tab === 'endpoints' && <Endpoints />}
      {tab === 'start' && <GettingStarted />}
    </div>
  )
}

// ------------------------------------------------------------------ tokens

export function Tokens({ isAdmin, onRole }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState('')
  const [fresh, setFresh] = useState(null) // the one-time secret, if a token was just made

  const load = useCallback(() => {
    apiApi.listTokens()
      .then((d) => { setData(d); if (onRole) onRole(d.role) })
      .catch((e) => setErr(e.message))
  }, [onRole])
  useEffect(() => { load() }, [load])

  const revoke = async (id) => {
    try {
      await apiApi.revokeToken(id)
      if (fresh?.token?.id === id) setFresh(null)
      load()
    } catch (e) { setErr(e.message) }
  }

  return (
    <div className="space-y-3">
      {err && <Card><p className="text-sm text-danger">{err}</p></Card>}
      {fresh && <FreshSecret data={fresh} onDismiss={() => setFresh(null)} />}

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-3">
        <CreateToken
          scopes={data?.scopes || ['read', 'write']}
          maxDays={data?.maxDays ?? 90}
          canNeverExpire={!!data?.canNeverExpire}
          onCreated={(d) => { setFresh(d); load() }}
          onError={setErr}
        />
        <Card title="Your tokens" subtitle={data ? `${data.tokens.length} total` : ''} className="lg:col-span-2">
          <TokenTable tokens={data?.tokens} onRevoke={revoke} />
        </Card>
      </div>

      {isAdmin && <AdminTokens onError={setErr} />}
    </div>
  )
}

// FreshSecret is the only place a secret is ever shown. It is loud on purpose:
// closing this panel is the last chance to copy it, and the server cannot show it
// again because it never kept it.
export function FreshSecret({ data, onDismiss }) {
  return (
    <div className="rounded-xl border border-primary/40 bg-primary/5 p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="flex items-center gap-1.5 text-sm font-semibold">
            <Icon.Key size={16} /> Copy this token now
          </h3>
          <p className="mt-0.5 text-xs text-muted">
            <b>{data.token?.name}</b> · {data.token?.scope} scope · {expiryText(data.token || {})}.
            DBCanvas stores only a hash of it, so this is the one and only time it can be shown.
          </p>
        </div>
        <Button variant="ghost" size="sm" onClick={onDismiss} aria-label="Dismiss">
          <Icon.Close size={16} />
        </Button>
      </div>
      <div className="mt-3 flex items-center gap-1 rounded-lg border bg-bg px-2 py-2">
        <span className="min-w-0 flex-1 break-all font-mono text-xs text-fg">{data.secret}</span>
        <CopyButton text={data.secret} />
      </div>
      <p className="mt-2 text-xs text-muted">
        Use it as <code className="rounded bg-surface2 px-1">Authorization: Bearer …</code>, or hand it to
        the CLI with <code className="rounded bg-surface2 px-1">DBCANVAS_TOKEN</code>.
      </p>
    </div>
  )
}

export function CreateToken({ scopes, maxDays, canNeverExpire, onCreated, onError }) {
  const [name, setName] = useState('')
  const [scope, setScope] = useState('write')
  const [days, setDays] = useState(90)
  const [busy, setBusy] = useState(false)

  // The choices offered are clamped to what this account can actually be given, so
  // nobody picks a lifetime the server is going to silently shorten.
  const choices = EXPIRY_CHOICES.filter((c) => (c.days === 0 ? canNeverExpire : c.days <= maxDays))

  const submit = async (e) => {
    e.preventDefault()
    setBusy(true)
    try {
      onCreated(await apiApi.createToken({ name: name.trim(), scope, days: Number(days) }))
      setName('')
    } catch (e2) {
      onError(e2.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card title="New token">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name" help={HELP.tokenName} hint="What it is for, so you know later what you are revoking.">
          <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)}
            placeholder="dbcanvas-cli on my laptop" maxLength={64} required />
        </Field>
        <Field label="Scope" help={HELP.tokenScope}>
          <select className={inputCls} value={scope} onChange={(e) => setScope(e.target.value)}>
            {scopes.map((s) => <option key={s} value={s}>{s}</option>)}
          </select>
        </Field>
        <p className="text-xs text-muted">{SCOPE_TEXT[scope]}</p>
        <Field label="Expires" help={HELP.tokenExpiry}
          hint={canNeverExpire ? undefined : `This installation allows at most ${maxDays} days.`}>
          <select className={inputCls} value={days} onChange={(e) => setDays(e.target.value)}>
            {choices.map((c) => <option key={c.days} value={c.days}>{c.label}</option>)}
          </select>
        </Field>
        <Button type="submit" className="w-full" disabled={busy || !name.trim()}>
          <Icon.Plus size={16} /> {busy ? 'Creating…' : 'Create token'}
        </Button>
        <p className="text-xs text-muted">
          Creating a token needs your password, which is why this page is the only place it can be
          done — a token can never create another one.
        </p>
      </form>
    </Card>
  )
}

export function TokenTable({ tokens, onRevoke, showOwner = false }) {
  if (!tokens) return <div className="py-6 text-center text-sm text-muted">Loading…</div>
  if (!tokens.length) {
    return (
      <div className="py-8 text-center text-sm text-muted">
        No tokens yet. Create one to call the API from a script or the CLI.
      </div>
    )
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-xs text-muted">
            <th className="pb-2 pr-3 font-medium">Name</th>
            {showOwner && <th className="pb-2 pr-3 font-medium">Owner</th>}
            <th className="pb-2 pr-3 font-medium">Prefix</th>
            <th className="pb-2 pr-3 font-medium">Scope</th>
            <th className="pb-2 pr-3 font-medium">Expiry</th>
            <th className="pb-2 pr-3 font-medium">Last used</th>
            <th className="pb-2 font-medium" />
          </tr>
        </thead>
        <tbody>
          {tokens.map((tk) => (
            <tr key={tk.id} className={`border-b last:border-0 ${tk.state !== 'active' ? 'opacity-60' : ''}`}>
              <td className="py-2 pr-3">
                <div className="font-medium">{tk.name}</div>
                <div className="text-xs text-muted">created {relDate(tk.createdAt)}</div>
              </td>
              {showOwner && <td className="py-2 pr-3">{tk.username || '—'}</td>}
              <td className="py-2 pr-3 font-mono text-xs">{tk.prefix}…</td>
              <td className="py-2 pr-3"><Badge tone={tk.scope === 'admin' ? 'warning' : 'muted'}>{tk.scope}</Badge></td>
              <td className="py-2 pr-3">
                <Badge tone={TOKEN_STATE_TONE[tk.state] || 'muted'}>{expiryText(tk)}</Badge>
              </td>
              <td className="py-2 pr-3 text-xs text-muted">{relDate(tk.lastUsedAt)}</td>
              <td className="py-2 text-right">
                {tk.state === 'active' && onRevoke && (
                  <ConfirmButton variant="ghost" size="sm" onConfirm={() => onRevoke(tk.id)}>
                    <Icon.Trash size={14} /> Revoke
                  </ConfirmButton>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-xs text-muted">
        Expired and revoked tokens stay listed for 30 days — when a script starts failing, the row
        is what tells you why.
      </p>
    </div>
  )
}

export function AdminTokens({ onError }) {
  const [tokens, setTokens] = useState(null)
  const load = useCallback(() => {
    apiApi.adminTokens().then((d) => setTokens(d.tokens)).catch((e) => onError(e.message))
  }, [onError])
  useEffect(() => { load() }, [load])

  const revoke = async (id) => {
    try { await apiApi.adminRevokeToken(id); load() } catch (e) { onError(e.message) }
  }
  return (
    <Card title="All tokens" subtitle="Every account on this installation — administrators only">
      <TokenTable tokens={tokens} onRevoke={revoke} showOwner />
    </Card>
  )
}

// ------------------------------------------------------------------ endpoints

export function Endpoints() {
  const [cat, setCat] = useState(null)
  const [err, setErr] = useState('')
  const [q, setQ] = useState('')
  const [scope, setScope] = useState('')
  const [method, setMethod] = useState('')

  useEffect(() => {
    apiApi.endpoints().then(setCat).catch((e) => setErr(e.message))
  }, [])

  const groups = useMemo(() => {
    if (!cat) return []
    return cat.groups
      .map((g) => ({
        ...g,
        endpoints: g.endpoints.filter((ep) =>
          matches(ep, q) && (!scope || ep.scope === scope) && (!method || ep.method === method)),
      }))
      .filter((g) => g.endpoints.length > 0)
  }, [cat, q, scope, method])

  const shown = groups.reduce((n, g) => n + g.endpoints.length, 0)

  if (err) return <Card><p className="text-sm text-danger">{err}</p></Card>
  if (!cat) return <Card><p className="py-6 text-center text-sm text-muted">Loading the catalogue…</p></Card>

  return (
    <div className="space-y-3">
      <Card>
        <div className="flex flex-wrap items-end gap-3">
          <div className="min-w-[220px] flex-1">
            <Field label="Search" help={HELP.apiSearch}>
              <input className={inputCls} value={q} onChange={(e) => setQ(e.target.value)}
                placeholder="deploy, backup, POST /api/stacks…" />
            </Field>
          </div>
          <div className="w-36">
            <Field label="Method">
              <select className={inputCls} value={method} onChange={(e) => setMethod(e.target.value)}>
                <option value="">Any</option>
                {['GET', 'POST', 'PUT', 'DELETE'].map((mm) => <option key={mm} value={mm}>{mm}</option>)}
              </select>
            </Field>
          </div>
          <div className="w-40">
            <Field label="Scope needed" help={HELP.apiScopeFilter}>
              <select className={inputCls} value={scope} onChange={(e) => setScope(e.target.value)}>
                <option value="">Any</option>
                {['read', 'write', 'admin'].map((s) => <option key={s} value={s}>{s}</option>)}
              </select>
            </Field>
          </div>
        </div>
        <p className="mt-3 text-xs text-muted">
          {shown} of {cat.total} endpoints. Your account can create {(cat.scopes || []).join(', ')} tokens.
          The full machine-readable version is at{' '}
          <a className="text-primary hover:underline" href="/api/meta/openapi.json" target="_blank" rel="noreferrer">
            /api/meta/openapi.json
          </a>{' '}
          (OpenAPI 3.1).
        </p>
      </Card>

      {shown === 0 && (
        <Card><p className="py-8 text-center text-sm text-muted">Nothing matches those filters.</p></Card>
      )}

      {groups.map((g) => (
        <Card key={g.name} title={g.name} subtitle={`${g.endpoints.length} endpoint${g.endpoints.length === 1 ? '' : 's'}`}>
          <div className="divide-y">
            {g.endpoints.map((ep) => <EndpointRow key={`${ep.method} ${ep.path}`} ep={ep} />)}
          </div>
        </Card>
      ))}
    </div>
  )
}

export function EndpointRow({ ep }) {
  const [open, setOpen] = useState(false)
  const origin = typeof location === 'undefined' ? '' : location.origin
  return (
    <div className="py-2">
      <button onClick={() => setOpen((v) => !v)} className="flex w-full items-start gap-3 text-left">
        <span className={`w-16 shrink-0 pt-0.5 font-mono text-xs font-semibold ${METHOD_TONE[ep.method] || ''}`}>
          {ep.method}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block break-all font-mono text-xs text-fg">{ep.path}</span>
          <span className="mt-0.5 block text-xs text-muted">{ep.summary}</span>
        </span>
        <span className="flex shrink-0 items-center gap-1.5">
          {ep.media && <Badge tone="accent">{MEDIA_LABEL[ep.media] || ep.media}</Badge>}
          <Badge tone={ep.scope === 'admin' ? 'warning' : ep.scope === 'write' ? 'muted' : 'primary'}>
            {ep.scope}
          </Badge>
          <span className={`text-muted transition-transform ${open ? 'rotate-180' : ''}`}>
            <Icon.Chevron size={16} />
          </span>
        </span>
      </button>

      {open && (
        <div className="mt-2 space-y-3 pl-16">
          {ep.media && MEDIA_TEXT[ep.media] && (
            <p className="text-xs text-warning">{MEDIA_TEXT[ep.media]}</p>
          )}
          <p className="text-xs text-muted">{SCOPE_TEXT[ep.scope]}</p>

          {ep.params?.length > 0 && (
            <div>
              <div className="mb-1 text-xs font-medium text-muted">Path parameters</div>
              <div className="space-y-1">
                {ep.params.map((p) => (
                  <div key={p.name} className="flex items-start gap-2 text-xs">
                    <code className="shrink-0 rounded bg-surface2 px-1 font-mono">{p.name}</code>
                    <span className="text-muted">{p.help}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          <Snippet label="curl" text={curlFor(ep, origin)} />
          <Snippet label="dbcanvas-cli" text={cliFor(ep)} />
          {ep.params?.length > 0 && (
            <p className="text-xs text-muted">
              The examples use placeholder values ({samplePath(ep.path)}) — substitute your own.
            </p>
          )}
        </div>
      )}
    </div>
  )
}

export function Snippet({ label, text }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className="overflow-x-auto rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">
        {text}
      </pre>
    </div>
  )
}

// ------------------------------------------------------------------ getting started

export function GettingStarted() {
  const origin = typeof location === 'undefined' ? 'http://localhost:8080' : location.origin
  const PLATFORMS = [
    { os: 'linux', arch: 'amd64', label: 'Linux · x86-64' },
    { os: 'linux', arch: 'arm64', label: 'Linux · arm64' },
    { os: 'darwin', arch: 'arm64', label: 'macOS · Apple silicon' },
    { os: 'darwin', arch: 'amd64', label: 'macOS · Intel' },
    { os: 'windows', arch: 'amd64', label: 'Windows · x86-64' },
  ]
  return (
    <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
      <Card title="Call it with curl" subtitle="Three lines from a password to JSON">
        <ol className="mb-3 list-decimal space-y-1 pl-5 text-sm text-muted">
          <li>Create a token on the <b>Tokens</b> tab and copy it.</li>
          <li>Put it in your environment.</li>
          <li>Call anything on the <b>Endpoints</b> tab.</li>
        </ol>
        <Snippet
          label="shell"
          text={`export DBCANVAS_TOKEN=dbc_…\n\ncurl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" \\\n  ${origin}/api/stacks | jq .`}
        />
      </Card>

      <Card title="dbcanvas-cli" subtitle="The same API, without writing curl">
        <Snippet
          label="shell"
          text={`dbcanvas login --url ${origin}\ndbcanvas stack list\ndbcanvas stack deploy my-pxc-lab --wait`}
        />
        <p className="mt-3 text-xs text-muted">
          <code className="rounded bg-surface2 px-1">login</code> trades your password for a scoped,
          expiring token and stores only the token. Your password is never written to disk.
        </p>
        <div className="mt-3">
          <div className="mb-1.5 flex items-center gap-1 text-xs font-medium text-muted">
            <span>Download</span>
            <Help text={HELP.cliDownload} />
          </div>
          <div className="flex flex-wrap gap-1.5">
            {PLATFORMS.map((p) => (
              <a
                key={`${p.os}-${p.arch}`}
                href={`/api/cli/download?os=${p.os}&arch=${p.arch}`}
                className="inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-fg hover:bg-surface2"
              >
                <Icon.External size={13} /> {p.label}
              </a>
            ))}
          </div>
        </div>
      </Card>

      <Card title="How access works" className="lg:col-span-2">
        <div className="space-y-2.5 text-sm text-muted">
          <p>
            <b className="text-fg">A token acts as your account.</b> It is not a service account and
            has no permissions of its own: it reaches the stacks you can reach, and it stops working
            the moment your account does.
          </p>
          <p>
            <b className="text-fg">Scope is checked against the endpoint.</b> A <code>read</code> token
            can call every <code>GET</code>, plus the handful of <code>POST</code>s that compute
            something without changing anything — validate, compare, preview. Anything else needs{' '}
            <code>write</code>, and the admin endpoints need <code>admin</code>, which only an
            administrator can create.
          </p>
          <p>
            <b className="text-fg">Creating a token requires your password.</b> So a token that leaks
            cannot mint a longer-lived replacement for itself — whoever has it still needs your
            password to get anything durable.
          </p>
          <p>
            <b className="text-fg">Revoking is immediate</b>, and losing access loses the API with it:
            disabling an account revokes its tokens in the same breath as its browser sessions.
          </p>
          <p>
            Full detail in <a className="text-primary hover:underline" href="https://github.com/jaimesicam/dbcanvas/blob/main/docs/API.md" target="_blank" rel="noreferrer">docs/API.md</a>.
          </p>
        </div>
      </Card>
    </div>
  )
}
