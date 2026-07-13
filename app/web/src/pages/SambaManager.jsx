import { useEffect, useState } from 'react'
import { Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, sambaApi } from '../lib/stackApi.js'
import DbLdapAuthGuide from '../components/DbLdapAuthGuide.jsx'
import { SecretValue } from '../components/Secret.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'ldap', label: 'LDAP' },
  { id: 'kerberos', label: 'Kerberos' },
  { id: 'cert', label: 'Certificate' },
  { id: 'dbauth', label: 'DB Auth' },
  { id: 'creds', label: 'Credentials' },
]

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{v || '—'}</span>
    </div>
  )
}
function Row({ k, v, secret }) {
  const [done, setDone] = useState(false)
  if (!v) return null
  if (secret) {
    return (
      <div>
        <div className="text-xs text-muted">{k}</div>
        <SecretValue value={v} />
      </div>
    )
  }
  return (
    <div>
      <div className="text-xs text-muted">{k}</div>
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">{v}</span>
        <button className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
          onClick={async () => { try { await navigator.clipboard.writeText(v) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}>
          {done ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
        </button>
      </div>
    </div>
  )
}
function Note({ tone = 'muted', children }) {
  if (!children) return null
  const c = tone === 'danger' ? 'border-danger/30 bg-danger/15 text-danger' : tone === 'success' ? 'border-success/30 bg-success/15 text-success' : 'border-border bg-surface2 text-muted'
  return <div className={`rounded-lg border px-2.5 py-1.5 text-xs ${c}`}>{children}</div>
}

export default function SambaManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const api = sambaApi(stackId, nodeId)

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">Samba AD DC · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>
      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>{t.label}</button>
        ))}
      </div>

      {tab === 'overview' && <Overview cfg={cfg} dep={dep} onDeleteNode={onDeleteNode} />}
      {tab === 'ldap' && <LdapTab api={api} cfg={cfg} sec={sec} />}
      {tab === 'kerberos' && <KerberosTab api={api} />}
      {tab === 'cert' && <CertTab api={api} cfg={cfg} />}
      {tab === 'dbauth' && <DbLdapAuthGuide kind="ad" fqdn={cfg.fqdn} baseDN={cfg.baseDN} bindDN={cfg.bindDN} />}
      {tab === 'creds' && <Creds cfg={cfg} sec={sec} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="Realm" v={cfg.realm} mono />
      <KV k="Workgroup" v={cfg.workgroup} mono />
      <KV k="Domain" v={cfg.domain} mono />
      <KV k="FQDN (DC / KDC)" v={cfg.fqdn} mono />
      <KV k="Base DN" v={cfg.baseDN} mono />
      <KV k="LDAP" v="ldap://:389 (plain binds allowed) · ldaps://:636" />
      <KV k="TLS" v={cfg.tls ? 'Intranet-CA cert' : 'self-signed (default)'} />
      <KV k="OS" v="Ubuntu 24.04" />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
      <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
        <Icon.Trash size={16} /> Delete node
      </Button>
    </div>
  )
}

// CopyBtn copies text to the clipboard, flashing a check.
function CopyBtn({ text, title }) {
  const [done, setDone] = useState(false)
  return (
    <button title={title || 'Copy'} className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}>
      {done ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
    </button>
  )
}

function LdapTab({ api, cfg, sec }) {
  const [users, setUsers] = useState([])
  const [groups, setGroups] = useState([])
  const [sel, setSel] = useState(null) // uid being edited
  const [edit, setEdit] = useState({ cn: '', sn: '', givenName: '', mail: '', pw: '' })
  const [nu, setNu] = useState({ username: '', password: '', givenName: '', surname: '', mail: '' })
  const [ng, setNg] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState(null)

  const load = () => { api.users().then((r) => setUsers(r.users || [])).catch(() => {}); api.groups().then((r) => setGroups(r.groups || [])).catch(() => {}) }
  useEffect(load, []) // eslint-disable-line react-hooks/exhaustive-deps
  const run = async (fn) => { setErr(null); setBusy(true); try { await fn(); load() } catch (e) { setErr(e.message) } finally { setBusy(false) } }
  const selectUser = (u) => { if (sel === u.uid) { setSel(null); return } setSel(u.uid); setEdit({ cn: u.cn || '', sn: u.sn || '', givenName: u.givenName || '', mail: u.mail || '', pw: '' }) }
  const searchCmd = (name) =>
    `ldapsearch -x -H ldap://${cfg.fqdn} -D "${cfg.bindDN}" -w '${sec.bindPassword || '<bind password>'}' -b "${cfg.baseDN}" "(sAMAccountName=${name})"`

  return (
    <div className="space-y-3">
      <Note tone="danger">{err}</Note>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Users</div>
        <div className="max-h-56 overflow-auto rounded-lg border">
          {users.length === 0 && <div className="px-3 py-3 text-center text-xs text-muted">no users</div>}
          {users.map((u) => (
            <div key={u.uid} className="border-b last:border-0">
              <div className="flex items-center justify-between px-3 py-1.5 text-sm">
                <button className="min-w-0 flex-1 truncate text-left" onClick={() => selectUser(u)}>
                  <span className="font-mono">{u.uid}</span>
                  {(u.givenName || u.sn) && <span className="text-muted"> — {[u.givenName, u.sn].filter(Boolean).join(' ')}</span>}
                </button>
                <CopyBtn text={searchCmd(u.uid)} title="Copy ldapsearch command" />
                <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?" onConfirm={() => run(() => api.userDelete(u.uid))}><Icon.Trash size={14} /></ConfirmButton>
              </div>
              {sel === u.uid && (
                <div className="space-y-2 border-t bg-surface2/50 px-3 py-2">
                  <div className="grid grid-cols-2 gap-2">
                    <input className={inputCls} placeholder="givenName" value={edit.givenName} onChange={(e) => setEdit({ ...edit, givenName: e.target.value })} />
                    <input className={inputCls} placeholder="surname" value={edit.sn} onChange={(e) => setEdit({ ...edit, sn: e.target.value })} />
                    <input className={inputCls} placeholder="display name" value={edit.cn} onChange={(e) => setEdit({ ...edit, cn: e.target.value })} />
                    <input className={inputCls} placeholder="mail" value={edit.mail} onChange={(e) => setEdit({ ...edit, mail: e.target.value })} />
                  </div>
                  <Button size="sm" className="w-full" disabled={busy} onClick={() => run(() => api.userUpdate({ username: u.uid, cn: edit.cn, surname: edit.sn, givenName: edit.givenName, mail: edit.mail }))}>Save attributes</Button>
                  <div className="flex gap-2">
                    <input className={inputCls} type="password" placeholder="new password" value={edit.pw} onChange={(e) => setEdit({ ...edit, pw: e.target.value })} />
                    <Button size="sm" variant="outline" disabled={busy || !edit.pw} onClick={() => run(async () => { await api.userPassword(u.uid, edit.pw); setEdit({ ...edit, pw: '' }) })}>Set</Button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
        <div className="grid grid-cols-2 gap-2">
          <input className={inputCls} placeholder="username*" value={nu.username} onChange={(e) => setNu({ ...nu, username: e.target.value })} />
          <input className={inputCls} type="password" placeholder="password*" value={nu.password} onChange={(e) => setNu({ ...nu, password: e.target.value })} />
          <input className={inputCls} placeholder="given name" value={nu.givenName} onChange={(e) => setNu({ ...nu, givenName: e.target.value })} />
          <input className={inputCls} placeholder="surname" value={nu.surname} onChange={(e) => setNu({ ...nu, surname: e.target.value })} />
          <input className={`${inputCls} col-span-2`} placeholder="email (optional)" value={nu.mail} onChange={(e) => setNu({ ...nu, mail: e.target.value })} />
        </div>
        <Button size="sm" className="w-full" disabled={busy || !nu.username || !nu.password} onClick={() => run(async () => { await api.userCreate(nu); setNu({ username: '', password: '', givenName: '', surname: '', mail: '' }) })}>
          <Icon.Plus size={15} /> Add user
        </Button>
      </div>

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Groups</div>
        <div className="max-h-48 overflow-auto rounded-lg border">
          {groups.length === 0 && <div className="px-3 py-3 text-center text-xs text-muted">no groups</div>}
          {groups.map((g) => (
            <div key={g.cn} className="space-y-1 border-b px-3 py-1.5 last:border-0">
              <div className="flex items-center justify-between text-sm">
                <span className="min-w-0 flex-1 truncate"><span className="font-mono">{g.cn}</span><span className="text-muted"> — {(g.members || []).join(', ') || 'no members'}</span></span>
                <CopyBtn text={searchCmd(g.cn)} title="Copy ldapsearch command" />
                <ConfirmButton variant="ghost" size="sm" confirmLabel="Delete?" onConfirm={() => run(() => api.groupDelete(g.cn))}><Icon.Trash size={14} /></ConfirmButton>
              </div>
              <div className="flex gap-2">
                <input className={inputCls} placeholder="user1, user2, …" defaultValue={(g.members || []).join(', ')} id={`m-${g.cn}`} />
                <Button size="sm" variant="outline" disabled={busy} onClick={() => run(() => api.groupMembers(g.cn, document.getElementById(`m-${g.cn}`).value))}>Set</Button>
              </div>
            </div>
          ))}
        </div>
        <div className="flex gap-2">
          <input className={inputCls} placeholder="new group name" value={ng} onChange={(e) => setNg(e.target.value)} />
          <Button size="sm" disabled={busy || !ng} onClick={() => run(async () => { await api.groupCreate(ng); setNg('') })}><Icon.Plus size={15} /> Add</Button>
        </div>
      </div>
    </div>
  )
}

function KerberosTab({ api }) {
  const [targets, setTargets] = useState({ postgres: [], mongodb: [] })
  const [principals, setPrincipals] = useState([])
  const [service, setService] = useState('postgres')
  const [fqdn, setFqdn] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState(null)

  const load = () => api.principals().then((r) => setPrincipals(r.principals || [])).catch(() => {})
  useEffect(() => { api.targets().then(setTargets).catch(() => {}); load() }, []) // eslint-disable-line react-hooks/exhaustive-deps
  const opts = service === 'postgres' ? targets.postgres : targets.mongodb
  useEffect(() => { setFqdn((opts && opts[0]) || '') }, [service, targets]) // eslint-disable-line react-hooks/exhaustive-deps

  const create = async () => {
    setErr(null); setBusy(true)
    try { await api.principalCreate(service, fqdn); load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  return (
    <div className="space-y-3">
      <Note tone="danger">{err}</Note>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">krb5.conf</div>
        <a href={api.krb5URL()} download className="inline-flex items-center justify-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-white transition hover:opacity-90">
          <Icon.External size={15} /> Download krb5.conf
        </a>
        <p className="text-[11px] text-muted">Place at <span className="font-mono">/etc/krb5.conf</span> on a DB node to use this realm's KDC.</p>
      </div>

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Create a server principal</div>
        <div className="flex gap-2">
          <select className={inputCls} value={service} onChange={(e) => setService(e.target.value)}>
            <option value="postgres">postgres</option>
            <option value="mongodb">mongodb</option>
          </select>
          <select className={inputCls} value={fqdn} onChange={(e) => setFqdn(e.target.value)}>
            {(!opts || opts.length === 0) && <option value="">no {service} nodes in stack</option>}
            {opts && opts.map((f) => <option key={f} value={f}>{f}</option>)}
          </select>
        </div>
        <div className="text-[11px] text-muted">Principal: <span className="font-mono">{service}/{fqdn || '<fqdn>'}</span></div>
        <Button size="sm" disabled={busy || !fqdn} onClick={create}><Icon.Plus size={15} /> Create principal</Button>
      </div>

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Principals + keytabs</div>
        <div className="max-h-44 overflow-auto rounded-lg border">
          {principals.length === 0 && <div className="px-3 py-3 text-center text-xs text-muted">none yet</div>}
          {principals.map((p) => (
            <div key={p} className="flex items-center justify-between border-b px-3 py-1.5 text-sm last:border-0">
              <span className="font-mono text-xs">{p}</span>
              <a href={api.keytabURL(p)} download className="inline-flex items-center gap-1 text-primary hover:opacity-80">
                <Icon.External size={13} /> keytab
              </a>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}

function CertTab({ api, cfg }) {
  const [value, setValue] = useState(cfg.certTtlValue || 365)
  const [unit, setUnit] = useState(cfg.certTtlUnit || 'days')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState(null)
  const gen = async () => {
    setBusy(true); setMsg(null)
    try { await api.certGenerate(value, unit); setMsg({ tone: 'success', text: 'TLS certificate re-issued from the Intranet CA and the DC restarted.' }) }
    catch (e) { setMsg({ tone: 'danger', text: e.message }) } finally { setBusy(false) }
  }
  return (
    <div className="space-y-3">
      <p className="text-xs text-muted">Issue an LDAPS certificate for <span className="font-mono">{cfg.fqdn}</span> signed by the Intranet CA (served on :636). Requires an Intranet node.</p>
      <div className="flex items-center gap-2">
        <span className="text-xs text-muted">TTL</span>
        <input type="number" min="1" className={`${inputCls} w-24`} value={value} onChange={(e) => setValue(Number(e.target.value))} />
        <select className={inputCls} value={unit} onChange={(e) => setUnit(e.target.value)}>
          <option value="minutes">minutes</option><option value="hours">hours</option><option value="days">days</option>
        </select>
      </div>
      <Button size="sm" disabled={busy} onClick={gen}><Icon.Arrow size={15} /> {busy ? 'Issuing…' : 'Generate / renew certificate'}</Button>
      {msg && <Note tone={msg.tone}>{msg.text}</Note>}
    </div>
  )
}

function Creds({ cfg, sec }) {
  return (
    <div className="space-y-3">
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Domain administrator</div>
        <Row k="Username" v={cfg.adminUser || 'Administrator'} />
        <Row k="Password" v={sec.adminPassword} secret />
        <p className="text-[11px] text-muted">From <span className="font-mono">SAMBA_PASSWORD</span>. Bind DN for admin: <span className="font-mono">{cfg.adminUser}@{cfg.domain}</span></p>
      </div>
      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">LDAP bind account (for DB auth)</div>
        <Row k="Bind DN" v={cfg.bindDN} />
        <Row k="Bind password" v={sec.bindPassword} secret />
      </div>
    </div>
  )
}
