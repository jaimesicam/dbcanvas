import { useCallback, useEffect, useState } from 'react'
import { Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import DbLdapAuthGuide from '../components/DbLdapAuthGuide.jsx'
import { intranetApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import { SecretValue } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { DEP_HELP } from '../lib/help.js'
import { useFollowedState } from '../session/SessionProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'email', label: 'Email' },
  { id: 'ldap', label: 'LDAP' },
  { id: 'dns', label: 'DNS' },
  { id: 'cert', label: 'Certificate' },
  { id: 'dbcerts', label: 'DB Certs' },
  { id: 'dbauth', label: 'DB Auth' },
  { id: 'creds', label: 'Credentials' },
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

export default function IntranetManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useFollowedState(`tab:${nodeId}`, 'overview')
  const api = intranetApi(stackId, nodeId)
  const { openTerminal } = useTerminals()
  const sec = dep.secrets || {}
  const cfg = dep.config || {}

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Intranet</span>
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
          onOpenTerminal={() => openTerminal({ stackId, nodeId, title: 'intranet · root' })}
        />
      )}
      {tab === 'email' && <EmailTab api={api} domain={sec.domain} webmailPort={cfg.webmailPort} />}
      {tab === 'ldap' && <LdapTab api={api} sec={sec} />}
      {tab === 'dns' && <DnsTab api={api} />}
      {tab === 'cert' && <CertTab api={api} />}
      {tab === 'dbcerts' && <DBCertTab api={api} domain={sec.domain} />}
      {tab === 'dbauth' && <DbLdapAuthGuide kind="ldap" fqdn={'intranet.' + sec.domain} baseDN={'ou=People,' + sec.baseDN} bindDN={sec.ldapAdminDN} />}
      {tab === 'creds' && <CredsTab sec={sec} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode, onOpenTerminal }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" help={DEP_HELP.FQDN} v={cfg.fqdn} mono />
      <KV k="Domain" help={DEP_HELP.Domain} v={cfg.domain} />
      <KV k="Base DN" help={DEP_HELP['Base DN']} v={cfg.baseDN} mono />
      <KV k="OS / arch" help={DEP_HELP['OS / arch']} v={cfg.os ? `${cfg.os} · ${cfg.arch || ''}` : ''} />
      <KV k="Network alias" help={DEP_HELP['Network alias']} v={cfg.alias} mono />
      <KV k="Container" help={DEP_HELP.Container} v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      {Array.isArray(cfg.services) && (
        <div className="flex flex-wrap gap-1 pt-1">
          {cfg.services.map((s) => <Badge key={s} tone="primary">{s}</Badge>)}
        </div>
      )}
      <Button variant="outline" size="sm" className="mt-2 w-full" onClick={onOpenTerminal}>
        <Icon.Nodes size={16} /> Open root console
      </Button>
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

function KV({ k, v, mono, help }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="flex shrink-0 items-center gap-1 text-muted">{k}<Help text={help} /></span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}

// ----------------------------------------------------------------- email tab

function EmailTab({ api, domain, webmailPort }) {
  const [users, setUsers] = useState([])
  const [u, setU] = useState('')
  const [p, setP] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [pwEdit, setPwEdit] = useState({ user: '', val: '' })

  const load = useCallback(async () => {
    try { setUsers((await api.emailList()).users || []) } catch (e) { setErr(e.message) }
  }, [api])
  useEffect(() => { load() }, [load])

  async function run(fn) {
    setBusy(true); setErr('')
    try { await fn(); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  // Roundcube is served by PHP's built-in server (php -S) at the port root, not under
  // httpd's /roundcubemail alias — so the URL is the bare host:port.
  const webmailUrl = webmailPort ? `http://${location.hostname}:${webmailPort}/` : null

  return (
    <div className="space-y-3">
      <Err>{err}</Err>
      {webmailUrl && (
        <a href={webmailUrl} target="_blank" rel="noreferrer"
          className="flex items-center justify-center gap-2 rounded-lg border border-primary/40 bg-primary/10 px-3 py-2 text-sm font-medium text-primary hover:bg-primary/15">
          <Icon.External size={15} /> Open RoundCube webmail
        </a>
      )}
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Add mailbox</div>
        <div className="flex gap-1">
          <input className={inputCls} placeholder="username" value={u} onChange={(e) => setU(e.target.value)} />
          <input className={inputCls} placeholder="password" value={p} onChange={(e) => setP(e.target.value)} />
        </div>
        <Button size="sm" disabled={busy || !u || !p} className="w-full"
          onClick={() => run(async () => { await api.emailAdd(u, p); setU(''); setP('') })}>
          <Icon.Plus size={15} /> Add
        </Button>
      </div>
      <div className="space-y-1">
        <div className="text-xs font-medium text-muted">Mailboxes {domain ? `@${domain}` : ''}</div>
        {users.length === 0 && <div className="text-xs text-muted">No mailboxes.</div>}
        {users.map((email) => (
          <div key={email} className="rounded-lg border bg-bg px-2 py-1.5 text-sm">
            <div className="flex items-center gap-1">
              <span className="min-w-0 flex-1 truncate">{email}</span>
              <button className="rounded p-1 text-muted hover:text-fg" title="Set password"
                onClick={() => setPwEdit(pwEdit.user === email ? { user: '', val: '' } : { user: email, val: '' })}>
                <Icon.Sliders size={15} />
              </button>
              <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?" onConfirm={() => run(() => api.emailDelete(email))}>
                <Icon.Trash size={15} />
              </ConfirmButton>
            </div>
            {pwEdit.user === email && (
              <div className="mt-1.5 flex gap-1">
                <input className={inputCls} placeholder="new password" value={pwEdit.val} autoFocus
                  onChange={(e) => setPwEdit({ user: email, val: e.target.value })} />
                <Button size="sm" disabled={busy || !pwEdit.val}
                  onClick={() => run(async () => { await api.emailPassword(email, pwEdit.val); setPwEdit({ user: '', val: '' }) })}>Save</Button>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

// ------------------------------------------------------------------ ldap tab

function LdapTab({ api, sec }) {
  const [users, setUsers] = useState([])
  const [groups, setGroups] = useState([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [sel, setSel] = useState(null) // selected uid for editing
  const [nu, setNu] = useState({ uid: '', password: '', cn: '', sn: '', givenName: '', mail: '' })
  const [edit, setEdit] = useState({ cn: '', sn: '', givenName: '', mail: '', pw: '' })
  const [gname, setGname] = useState('')

  const load = useCallback(async () => {
    try {
      setUsers((await api.ldapUsers()).users || [])
      setGroups((await api.ldapGroups()).groups || [])
    } catch (e) { setErr(e.message) }
  }, [api])
  useEffect(() => { load() }, [load])

  async function run(fn) {
    setBusy(true); setErr('')
    try { await fn(); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  function selectUser(usr) {
    setSel(usr.uid)
    setEdit({ cn: usr.cn || '', sn: usr.sn || '', givenName: usr.givenName || '', mail: usr.mail || '', pw: '' })
  }

  const userCmd = (uid) =>
    `ldapsearch -x -H ldap://intranet:389 -D "${sec.ldapAdminDN}" -w '${sec.ldapAdminPassword}' -b "uid=${uid},ou=People,${sec.baseDN}"`
  const groupCmd = (cn) =>
    `ldapsearch -x -H ldap://intranet:389 -D "${sec.ldapAdminDN}" -w '${sec.ldapAdminPassword}' -b "cn=${cn},ou=Groups,${sec.baseDN}"`

  return (
    <div className="space-y-4">
      <Err>{err}</Err>

      {/* users */}
      <div className="space-y-2">
        <div className="text-xs font-semibold text-muted">Users</div>
        {users.map((usr) => (
          <div key={usr.uid} className={`rounded-lg border px-2 py-1.5 text-sm ${sel === usr.uid ? 'border-primary bg-primary/5' : 'bg-bg'}`}>
            <div className="flex items-center gap-1">
              <button className="min-w-0 flex-1 truncate text-left" onClick={() => selectUser(usr)}>
                <span className="font-medium">{usr.uid}</span>
                {usr.cn ? <span className="text-muted"> — {usr.cn}</span> : null}
              </button>
              <CopyButton text={userCmd(usr.uid)} title="Copy ldapsearch command" />
              <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?" onConfirm={() => run(() => api.ldapUserDelete(usr.uid))}>
                <Icon.Trash size={15} />
              </ConfirmButton>
            </div>
            {sel === usr.uid && (
              <div className="mt-2 space-y-1.5 border-t pt-2">
                <div className="grid grid-cols-2 gap-1">
                  <input className={inputCls} placeholder="givenName" value={edit.givenName} onChange={(e) => setEdit({ ...edit, givenName: e.target.value })} />
                  <input className={inputCls} placeholder="sn" value={edit.sn} onChange={(e) => setEdit({ ...edit, sn: e.target.value })} />
                  <input className={inputCls} placeholder="cn" value={edit.cn} onChange={(e) => setEdit({ ...edit, cn: e.target.value })} />
                  <input className={inputCls} placeholder="mail" value={edit.mail} onChange={(e) => setEdit({ ...edit, mail: e.target.value })} />
                </div>
                <Button size="sm" disabled={busy} className="w-full" onClick={() => run(() => api.ldapUserUpdate({ uid: usr.uid, cn: edit.cn, sn: edit.sn, givenName: edit.givenName, mail: edit.mail }))}>Save attributes</Button>
                <div className="flex gap-1">
                  <input className={inputCls} placeholder="new password" value={edit.pw} onChange={(e) => setEdit({ ...edit, pw: e.target.value })} />
                  <Button size="sm" variant="outline" disabled={busy || !edit.pw}
                    onClick={() => run(async () => { await api.ldapUserPassword(usr.uid, edit.pw); setEdit({ ...edit, pw: '' }) })}>Set</Button>
                </div>
              </div>
            )}
          </div>
        ))}

        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs font-medium text-muted">Create user</div>
          <div className="grid grid-cols-2 gap-1">
            <input className={inputCls} placeholder="uid*" value={nu.uid} onChange={(e) => setNu({ ...nu, uid: e.target.value })} />
            <input className={inputCls} placeholder="password" value={nu.password} onChange={(e) => setNu({ ...nu, password: e.target.value })} />
            <input className={inputCls} placeholder="givenName" value={nu.givenName} onChange={(e) => setNu({ ...nu, givenName: e.target.value })} />
            <input className={inputCls} placeholder="sn" value={nu.sn} onChange={(e) => setNu({ ...nu, sn: e.target.value })} />
            <input className={inputCls} placeholder="cn" value={nu.cn} onChange={(e) => setNu({ ...nu, cn: e.target.value })} />
            <input className={inputCls} placeholder="mail" value={nu.mail} onChange={(e) => setNu({ ...nu, mail: e.target.value })} />
          </div>
          <Button size="sm" disabled={busy || !nu.uid} className="w-full"
            onClick={() => run(async () => { await api.ldapUserCreate(nu); setNu({ uid: '', password: '', cn: '', sn: '', givenName: '', mail: '' }) })}>
            <Icon.Plus size={15} /> Create user
          </Button>
        </div>
      </div>

      {/* groups */}
      <div className="space-y-2">
        <div className="text-xs font-semibold text-muted">Groups</div>
        {groups.map((g) => (
          <div key={g.cn} className="rounded-lg border bg-bg px-2 py-1.5 text-sm">
            <div className="flex items-center gap-1">
              <span className="min-w-0 flex-1 truncate"><span className="font-medium">{g.cn}</span>
                <span className="text-muted"> — {(g.members || []).join(', ') || 'no members'}</span>
              </span>
              <CopyButton text={groupCmd(g.cn)} title="Copy ldapsearch command" />
              <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?" onConfirm={() => run(() => api.ldapGroupDelete(g.cn))}>
                <Icon.Trash size={15} />
              </ConfirmButton>
            </div>
            <div className="mt-1.5 flex gap-1">
              <input className={inputCls} placeholder="uid1, uid2, …" defaultValue={(g.members || []).join(', ')}
                onKeyDown={(e) => { if (e.key === 'Enter') run(() => api.ldapGroupMembers(g.cn, e.target.value)) }} id={`m-${g.cn}`} />
              <Button size="sm" variant="outline" disabled={busy}
                onClick={() => run(() => api.ldapGroupMembers(g.cn, document.getElementById(`m-${g.cn}`).value))}>Set</Button>
            </div>
          </div>
        ))}
        <div className="flex gap-1">
          <input className={inputCls} placeholder="new group cn" value={gname} onChange={(e) => setGname(e.target.value)} />
          <Button size="sm" disabled={busy || !gname}
            onClick={() => run(async () => { await api.ldapGroupCreate(gname); setGname('') })}>
            <Icon.Plus size={15} /> Group
          </Button>
        </div>
      </div>
    </div>
  )
}

// ------------------------------------------------------------------- dns tab

// The stack's DNS as bind serves it: the records DBCanvas publishes for every node (read-only,
// rebuilt on every deploy) and the ones added here, which DBCanvas keeps and merges in each time
// (app/dnscustom.go). A custom record replaces the automatic one of the same name or address.
const DNS_TYPES = {
  A: { name: 'host (e.g. db or db.site1)', value: 'IPv4 address' },
  CNAME: { name: 'alias (e.g. mysql)', value: 'target host (bare = in-domain)' },
  PTR: { name: 'IPv4 address', value: 'host name it returns' },
}
const EMPTY_DNS = { type: 'A', name: '', value: '', ptr: true }

export function DnsRecordForm({ rec, setRec, domain }) {
  const ph = DNS_TYPES[rec.type]
  return (
    <div className="space-y-1">
      {/* inputCls carries w-full, which a width class beside it does not reliably beat — so
          the wrappers size the row: a fixed box for the type, the rest for the name. */}
      <div className="flex gap-1">
        <div className="w-24 shrink-0">
          <select className={inputCls} value={rec.type}
            onChange={(e) => setRec({ ...rec, type: e.target.value, ptr: e.target.value === 'A' })}>
            {Object.keys(DNS_TYPES).map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </div>
        <div className="min-w-0 flex-1">
          <input className={inputCls} placeholder={ph.name} value={rec.name} onChange={(e) => setRec({ ...rec, name: e.target.value })} />
        </div>
      </div>
      <input className={inputCls} placeholder={ph.value} value={rec.value} onChange={(e) => setRec({ ...rec, value: e.target.value })} />
      {rec.type === 'A' && (
        <label className="flex items-center gap-1.5 text-xs text-muted">
          <input type="checkbox" checked={!!rec.ptr} onChange={(e) => setRec({ ...rec, ptr: e.target.checked })} />
          Also publish the reverse (PTR) record
        </label>
      )}
      {rec.type !== 'PTR' && rec.name && domain && (
        <div className="text-[11px] text-muted">Resolves as <span className="font-mono">{rec.name.replace(/\.$/, '').replace(new RegExp(`\\.${domain.replace(/\./g, '\\.')}$`), '')}.{domain}</span></div>
      )}
    </div>
  )
}

// `initial` is a view to render before the first load returns (the smoke test's way in).
export function DnsTab({ api, initial = null }) {
  const [view, setView] = useState(initial)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [nr, setNr] = useState(EMPTY_DNS)
  const [edit, setEdit] = useState(null) // the custom record being edited
  const [showAuto, setShowAuto] = useState(false)

  const load = useCallback(async () => {
    try { setView(await api.dnsList()) } catch (e) { setErr(e.message) }
  }, [api])
  useEffect(() => { load() }, [load])

  // Every mutation answers with the new view, so no reload round trip.
  async function run(fn) {
    setBusy(true); setErr('')
    try { setView(await fn()); return true } catch (e) { setErr(e.message); return false } finally { setBusy(false) }
  }

  if (!view) return <div className="space-y-2"><Err>{err}</Err><div className="text-xs text-muted">Loading…</div></div>
  const domain = view.domain
  const fqdn = (n) => `${n}.${domain}`
  const shown = (r) => (r.type === 'PTR' ? `${r.name} → ${r.value}` : `${fqdn(r.name)} → ${r.value}`)
  const auto = view.automatic || []
  const overridden = auto.filter((r) => r.overridden).length

  return (
    <div className="space-y-4">
      <Err>{err}</Err>
      {!view.applied && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-2.5 py-1.5 text-xs text-warning">
          The Intranet is not running, so nothing is being served. Records saved now are published on its next deploy.
        </div>
      )}

      {/* custom records */}
      <div className="space-y-2">
        <div className="flex items-center gap-1 text-xs font-semibold text-muted">
          Custom records
          <Help text={`Added by hand and kept by DBCanvas, so they survive every redeploy. A custom A or CNAME replaces the automatic record of the same name; a custom PTR (or an A with its reverse ticked) replaces the automatic PTR of the same address. A PTR outside the stack network gets a /24 reverse zone of its own.`} />
        </div>
        {view.custom.length === 0 && <div className="text-xs text-muted">None yet.</div>}
        {view.custom.map((r) => (
          <div key={r.id} className={`rounded-lg border px-2 py-1.5 text-sm ${edit?.id === r.id ? 'border-primary bg-primary/5' : 'bg-bg'}`}>
            <div className="flex items-center gap-1">
              <Badge tone="primary">{r.type}</Badge>
              <button className="min-w-0 flex-1 truncate text-left font-mono text-xs" title="Edit"
                onClick={() => setEdit(edit?.id === r.id ? null : { ...r, ptr: !!r.ptr })}>
                {shown(r)}{r.type === 'A' && r.ptr ? <span className="text-muted"> · +PTR</span> : null}
              </button>
              <CopyButton text={r.type === 'PTR' ? `dig +short -x ${r.name}` : `dig +short ${fqdn(r.name)}`} title="Copy dig command" />
              <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?"
                onConfirm={() => run(async () => { if (edit?.id === r.id) setEdit(null); return api.dnsDelete(r.id) })}>
                <Icon.Trash size={15} />
              </ConfirmButton>
            </div>
            {edit?.id === r.id && (
              <div className="mt-2 space-y-1.5 border-t pt-2">
                <DnsRecordForm rec={edit} setRec={setEdit} domain={domain} />
                <Button size="sm" className="w-full" disabled={busy || !edit.name || !edit.value}
                  onClick={async () => { if (await run(() => api.dnsUpdate(edit))) setEdit(null) }}>Save</Button>
              </div>
            )}
          </div>
        ))}

        <div className="space-y-1.5 rounded-lg border border-dashed p-2">
          <div className="text-xs font-medium text-muted">Add record</div>
          <DnsRecordForm rec={nr} setRec={setNr} domain={domain} />
          <Button size="sm" className="w-full" disabled={busy || !nr.name || !nr.value}
            onClick={async () => { if (await run(() => api.dnsAdd(nr))) setNr({ ...EMPTY_DNS, type: nr.type, ptr: nr.type === 'A' }) }}>
            <Icon.Plus size={15} /> Add
          </Button>
        </div>
      </div>

      {/* automatic records */}
      <div className="space-y-1">
        <button className="flex w-full items-center gap-1 text-left text-xs font-semibold text-muted" onClick={() => setShowAuto(!showAuto)}>
          <Icon.Chevron size={13} className={`shrink-0 transition-transform ${showAuto ? '' : '-rotate-90'}`} />
          Automatic records ({auto.length}{overridden ? `, ${overridden} replaced` : ''})
          <Help text="Published by DBCanvas for every deployed node and rebuilt on every deploy, so they cannot be edited here — add a custom record with the same name or address to replace one." />
        </button>
        {showAuto && (
          <div className="space-y-0.5">
            {auto.length === 0 && <div className="text-xs text-muted">No running nodes.</div>}
            {auto.map((r) => (
              <div key={`${r.type}:${r.name}`} className={`flex items-center gap-1 rounded px-1.5 py-0.5 font-mono text-xs ${r.overridden ? 'text-muted line-through' : 'text-fg'}`}
                title={r.overridden ? 'Replaced by a custom record' : ''}>
                <span className="w-10 shrink-0 text-muted no-underline">{r.type}</span>
                <span className="min-w-0 flex-1 truncate">{shown(r)}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="text-[11px] text-muted">
        Zones served: <span className="font-mono">{[domain, ...(view.reverseZones || [])].join(' · ')}</span>.
        Every node, K3D pod and the VNC desktop resolve through the Intranet; your own machine does not.
      </div>
    </div>
  )
}

// ------------------------------------------------------------------ cert tab

function CertTab({ api }) {
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
        <div className="mt-1 text-xs text-muted">Stored at <span className="font-mono">/etc/pki/dbcanvas/intranet.crt</span> (serverAuth + clientAuth).</div>
      </div>
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Generate / renew (archives the existing cert)</div>
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
      </div>
    </div>
  )
}

// --------------------------------------------------------------- db certs tab

// A labelled, copyable code/PEM block.
function CodeBlock({ label, text, mono = true }) {
  if (!text) return null
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className={`max-h-40 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 text-[11px] leading-relaxed text-fg ${mono ? 'font-mono' : ''}`}>{text}</pre>
    </div>
  )
}

// dbInstructions builds server-config + client-invocation snippets for each engine,
// substituting the certificate's username/subject. `host` is a placeholder the operator
// replaces with the target DB node FQDN.
function dbInstructions(user, subject, host) {
  const subj = subject || `CN=${user},O=DBCanvas`
  return {
    MySQL: {
      server:
`-- On the MySQL server: trust the Intranet CA + require TLS
-- /etc/my.cnf  [mysqld]  (DBCanvas stores per-node certs in /var/lib/mysql)
ssl-ca=/var/lib/mysql/ca.pem
ssl-cert=/var/lib/mysql/server-cert.pem
ssl-key=/var/lib/mysql/server-key.pem
require_secure_transport=ON

-- Bind the account to this client certificate:
CREATE USER '${user}'@'%' REQUIRE SUBJECT '/O=DBCanvas/CN=${user}';
-- (or, to accept any cert signed by the CA:  REQUIRE X509; )`,
      client:
`mysql -h ${host} -u ${user} \\
  --ssl-ca=ca.crt --ssl-cert=${user}.crt --ssl-key=${user}.key \\
  --ssl-mode=VERIFY_CA`,
    },
    PostgreSQL: {
      server:
`# On the PostgreSQL server: postgresql.conf
# (DBCanvas keeps per-node certs in the data directory for a standalone server —
#  where these relative names resolve — or in /etc/patroni for a Patroni cluster)
ssl = on
ssl_ca_file   = 'ca.crt'
ssl_cert_file = 'server.crt'
ssl_key_file  = 'server.key'

# pg_hba.conf — require a client cert whose CN equals the role name
hostssl  all  ${user}  0.0.0.0/0  cert  clientcert=verify-full`,
      client:
`psql "host=${host} dbname=postgres user=${user} \\
  sslmode=verify-full sslrootcert=ca.crt sslcert=${user}.crt sslkey=${user}.key"`,
    },
    MongoDB: {
      server:
`# On the MongoDB server: mongod.conf
net:
  tls:
    mode: requireTLS
    certificateKeyFile: /etc/mongo/certs/server.pem   # server cert+key, concatenated
    CAFile: /etc/mongo/certs/ca.crt

// Create the X.509 user — its name is the certificate subject (RFC2253 order):
db.getSiblingDB("$external").runCommand({
  createUser: "${subj}",
  roles: [ { role: "readWrite", db: "admin" } ]
})`,
      client:
`# mongosh needs the cert and key in a single file:
cat ${user}.crt ${user}.key > ${user}.pem
mongosh --host ${host} --tls --tlsCAFile ca.crt \\
  --tlsCertificateKeyFile ${user}.pem \\
  --authenticationDatabase '$external' \\
  --authenticationMechanism MONGODB-X509`,
    },
  }
}

function DBCertTab({ api, domain }) {
  const [certs, setCerts] = useState([])
  const [username, setUsername] = useState('')
  const [value, setValue] = useState(365)
  const [unit, setUnit] = useState('days')
  const [result, setResult] = useState(null) // { username, subject, notAfter, cert, key, caCert }
  const [engine, setEngine] = useState('MySQL')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const host = `<db-host${domain ? `.${domain}` : ''}>`

  const load = useCallback(async () => {
    try { setCerts((await api.dbCertList()).certs || []) } catch (e) { setErr(e.message) }
  }, [api])
  useEffect(() => { load() }, [load])

  async function run(fn) {
    setBusy(true); setErr('')
    try { await fn(); await load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  const exists = certs.some((c) => c.username === username.trim())
  const instr = result ? dbInstructions(result.username, result.subject, host) : null

  return (
    <div className="space-y-3">
      <Err>{err}</Err>

      {/* generate form */}
      <div className="space-y-1.5 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Issue a client certificate (signed by the Intranet CA)</div>
        <input className={inputCls} placeholder="username (cert CN)" value={username} onChange={(e) => setUsername(e.target.value)} />
        <div className="flex gap-1">
          <input type="number" min="1" className={inputCls} value={value} onChange={(e) => setValue(e.target.value)} />
          <select className={inputCls} value={unit} onChange={(e) => setUnit(e.target.value)}>
            <option value="minutes">minutes</option>
            <option value="hours">hours</option>
            <option value="days">days</option>
          </select>
        </div>
        <Button size="sm" className="w-full" disabled={busy || !username.trim()}
          onClick={() => run(async () => { setResult(await api.dbCertGenerate(username.trim(), Number(value), unit)) })}>
          <Icon.Plus size={15} /> {busy ? 'Generating…' : exists ? 'Regenerate (overwrites)' : 'Generate certificate'}
        </Button>
        {exists && <div className="text-[11px] text-warning">A certificate for “{username.trim()}” already exists — generating overwrites it.</div>}
      </div>

      {/* existing certs */}
      <div className="space-y-1">
        <div className="text-xs font-medium text-muted">Issued certificates</div>
        {certs.length === 0 && <div className="text-xs text-muted">None yet.</div>}
        {certs.map((c) => (
          <div key={c.username} className={`rounded-lg border px-2 py-1.5 text-sm ${result?.username === c.username ? 'border-primary bg-primary/5' : 'bg-bg'}`}>
            <div className="flex items-center gap-1">
              <button className="min-w-0 flex-1 truncate text-left"
                onClick={() => run(async () => { setResult(await api.dbCertGet(c.username)) })}>
                <span className="font-medium">{c.username}</span>
                {c.notAfter ? <span className="text-muted"> — expires {c.notAfter}</span> : null}
              </button>
              <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?"
                onConfirm={() => run(async () => { if (result?.username === c.username) setResult(null); await api.dbCertDelete(c.username) })}>
                <Icon.Trash size={15} />
              </ConfirmButton>
            </div>
          </div>
        ))}
      </div>

      {/* generated / selected cert material + usage */}
      {result && (
        <div className="space-y-3 rounded-lg border border-primary/40 bg-primary/5 p-2">
          <div className="flex items-center justify-between">
            <span className="text-sm font-semibold">{result.username}</span>
            {result.notAfter && <Badge tone="primary">expires {result.notAfter}</Badge>}
          </div>
          {result.subject && <KV k="Subject" help={DEP_HELP.Subject} v={result.subject} mono />}

          <CodeBlock label="CA certificate — ca.crt" text={result.caCert} />
          <CodeBlock label={`Certificate — ${result.username}.crt`} text={result.cert} />
          <CodeBlock label={`Private key — ${result.username}.key`} text={result.key} />

          <div>
            <div className="mb-1 text-xs font-semibold text-muted">How to use</div>
            <div className="mb-2 flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
              {['MySQL', 'PostgreSQL', 'MongoDB'].map((e) => (
                <button key={e} onClick={() => setEngine(e)}
                  className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${engine === e ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
                  {e}
                </button>
              ))}
            </div>
            {instr && (
              <div className="space-y-2">
                <CodeBlock label="Server configuration" text={instr[engine].server} />
                <CodeBlock label="Client invocation" text={instr[engine].client} />
                <div className="text-[11px] text-muted">Replace <span className="font-mono">{host}</span> with the target database node’s FQDN. Save the three blocks above as <span className="font-mono">ca.crt</span>, <span className="font-mono">{result.username}.crt</span> and <span className="font-mono">{result.username}.key</span>.</div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

// ----------------------------------------------------------------- creds tab

function CredsTab({ sec }) {
  const rows = [
    { k: 'LDAP admin DN', v: sec.ldapAdminDN },
    { k: 'LDAP admin password', v: sec.ldapAdminPassword, secret: true },
    { k: 'Mail admin user', v: sec.mailAdminUser },
    { k: 'Mail admin password', v: sec.mailAdminPassword, secret: true },
  ]
  return (
    <div className="space-y-2">
      {rows.map((r) => (
        <div key={r.k}>
          <div className="text-xs text-muted">{r.k}</div>
          {r.secret
            ? <SecretValue value={r.v} />
            : (
              <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
                <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{r.v || '—'}</span>
                {r.v && <CopyButton text={r.v} />}
              </div>
            )}
        </div>
      ))}
    </div>
  )
}
