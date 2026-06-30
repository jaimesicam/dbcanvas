import { useCallback, useEffect, useState } from 'react'
import { Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { intranetApi, DEPLOY_TONE } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'email', label: 'Email' },
  { id: 'ldap', label: 'LDAP' },
  { id: 'cert', label: 'Certificate' },
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
  const [tab, setTab] = useState('overview')
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
      {tab === 'cert' && <CertTab api={api} />}
      {tab === 'creds' && <CredsTab sec={sec} />}
    </div>
  )
}

function Overview({ cfg, dep, onDeleteNode, onOpenTerminal }) {
  return (
    <div className="space-y-2 text-sm">
      <KV k="FQDN" v={cfg.fqdn} mono />
      <KV k="Domain" v={cfg.domain} />
      <KV k="Base DN" v={cfg.baseDN} mono />
      <KV k="OS / arch" v={cfg.os ? `${cfg.os} · ${cfg.arch || ''}` : ''} />
      <KV k="Network alias" v={cfg.alias} mono />
      <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
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

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
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

// ----------------------------------------------------------------- creds tab

function CredsTab({ sec }) {
  const rows = [
    { k: 'LDAP admin DN', v: sec.ldapAdminDN },
    { k: 'LDAP admin password', v: sec.ldapAdminPassword },
    { k: 'Mail admin user', v: sec.mailAdminUser },
    { k: 'Mail admin password', v: sec.mailAdminPassword },
  ]
  return (
    <div className="space-y-2">
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
