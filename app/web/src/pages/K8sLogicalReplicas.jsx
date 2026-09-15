import { useEffect, useState } from 'react'
import { Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { k3dApi } from '../lib/stackApi.js'

// K8sLogicalReplicas — the "Logical replicas" tab of a Percona Operator for PostgreSQL cluster.
//
// A logical replica is an extra read-only PostgreSQL instance in the same cluster, seeded from a
// full copy and then kept in sync by logical replication. It is NOT a high-availability replica:
// Patroni does not manage it, never promotes it, and never fails over to it.
//
// The tab is a list of operations rather than a form, because that is the shape of the feature:
// the Operator does not rebuild a replica in place, so there is no edit — only add, reseed and
// remove. See app/k3dlogrepl.go for why each one does what it does.

const STATE_TONE = { ready: 'success', bootstrapping: 'warning', broken: 'danger' }

function Row({ k, v, mono }) {
  if (!v) return null
  return (
    <div className="flex justify-between gap-3 text-xs">
      <span className="shrink-0 text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono' : ''}`}>{v}</span>
    </div>
  )
}

function CopyLine({ text }) {
  const [done, setDone] = useState(false)
  return (
    <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
      <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-fg">{text}</span>
      <button title="Copy" className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
        onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}>
        {done ? <Icon.Check size={13} /> : <Icon.Copy size={13} />}
      </button>
    </div>
  )
}

export function K8sLogicalReplicas({ stackId, frame, isServer }) {
  const [data, setData] = useState(null)
  const [err, setErr] = useState(null)
  const [busy, setBusy] = useState('')
  const [msg, setMsg] = useState(null)
  const [log, setLog] = useState(null)
  const [form, setForm] = useState({ name: '', databases: '', bootstrapMethod: 'pgbackrest', storageGb: 1, expose: 'ClusterIP' })
  const api = frame ? k3dApi(stackId, frame.id) : null

  async function load() {
    if (!api) return
    try { setData(await api.logicalReplicas()); setErr(null) } catch (e) { setErr(e.message || String(e)) }
  }
  // Polled: bootstrapping is the state people sit and watch, and it is the one that turns into
  // `broken` without anything else changing on the screen.
  useEffect(() => {
    if (!isServer || !frame) return
    load()
    const t = setInterval(load, 10000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stackId, frame?.id, isServer])

  if (!isServer) {
    return <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">
      Logical replicas are managed from the cluster's <span className="font-medium text-fg">server</span> node — open that one.
    </div>
  }
  if (err) return <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>
  if (!data) return <div className="text-xs text-muted">Loading…</div>

  const blocking = (data.checks || []).filter((c) => c.blocks && !c.ok)
  const canAdd = blocking.length === 0

  async function run(what, fn) {
    setBusy(what); setMsg(null)
    try {
      const r = await fn()
      setMsg({ tone: r?.ok === false ? 'warning' : 'success', text: r?.message || 'done' })
      await load()
    } catch (e) {
      setMsg({ tone: 'danger', text: e.message || String(e) })
    } finally { setBusy('') }
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        A logical replica is an extra <span className="font-medium text-fg">read-only</span> PostgreSQL instance in
        this cluster, with its own volume and Service. The Operator copies the whole data directory once, then keeps
        the named databases in sync by logical replication. It is <span className="font-medium text-fg">not</span> a
        high-availability replica: Patroni does not manage it and never promotes it. Tech preview.
      </div>

      {/* The Operator's own verdict on whether the primary can feed a replica at all — it is the
          answer to "why is it still bootstrapping", so it is shown verbatim. */}
      {data.ready && data.ready.status !== 'True' && (
        <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
          <span className="font-medium text-fg">ReadyForLogicalReplication: {data.ready.status}</span>
          {data.ready.reason ? <> · <span className="font-mono">{data.ready.reason}</span></> : null}
          {data.ready.message ? <div className="mt-1">{data.ready.message}</div> : null}
        </div>
      )}

      <div className="space-y-1.5 rounded-lg border p-2">
        <div className="text-xs font-medium text-muted">Requirements</div>
        {(data.checks || []).map((c) => (
          <div key={c.name} className="flex items-start gap-2 text-[11px]">
            <span className={`mt-0.5 shrink-0 ${c.ok ? 'text-success' : c.blocks ? 'text-danger' : 'text-warning'}`}>
              {c.ok ? <Icon.Check size={13} /> : c.blocks ? <Icon.StatusCrit size={13} /> : <Icon.StatusWarn size={13} />}
            </span>
            <span><span className={c.ok ? 'text-fg' : 'font-medium text-fg'}>{c.name}</span>
              <span className="block text-muted">{c.detail}</span></span>
          </div>
        ))}
      </div>

      {msg && (
        <div className={`rounded-lg px-3 py-2 text-[11px] leading-snug ${msg.tone === 'danger' ? 'border border-danger/30 bg-danger/10 text-danger'
          : msg.tone === 'warning' ? 'border border-warning/30 bg-warning/10 text-muted' : 'border border-success/30 bg-success/10 text-muted'}`}>
          {msg.text}
        </div>
      )}

      <div className="space-y-2">
        <div className="text-xs font-medium text-muted">Replicas</div>
        {(data.replicas || []).length === 0 && (
          <div className="rounded-lg bg-surface2 px-3 py-2 text-xs text-muted">This cluster has no logical replicas.</div>
        )}
        {(data.replicas || []).map((rep) => (
          <div key={rep.name} className="space-y-1.5 rounded-lg border p-2">
            <div className="flex items-center justify-between gap-2">
              <span className="font-mono text-sm font-medium text-fg">{rep.name}</span>
              <Badge tone={STATE_TONE[rep.state] || 'muted'}>{rep.state || (rep.inSpec ? 'pending' : 'removing')}</Badge>
            </div>
            {rep.reason && <Row k="Reason" v={rep.reason} mono />}
            {rep.message && <div className="text-[11px] leading-snug text-muted">{rep.message}</div>}
            <Row k="Databases" v={(rep.databases || []).join(', ')
              || ((rep.specDatabases || []).length ? rep.specDatabases.join(', ') : 'all non-template databases except postgres')} />
            <Row k="Seeded" v={rep.seededAt} />
            <Row k="Bootstrap" v={rep.bootstrapMethod} mono />
            <Row k="Volume" v={rep.storage} mono />
            <Row k="Service" v={rep.service} mono />
            {/* The replica is not behind pgBouncer and not in the HA Services, so the address is
                worth spelling out — connecting to the wrong one is the usual first mistake. */}
            {rep.endpoint && (
              <div className="pt-1">
                <div className="mb-1 text-[11px] text-muted">psql (its own Service — not pgBouncer, not <span className="font-mono">-ha</span>)</div>
                <CopyLine text={`psql "host=${rep.endpoint.split(':')[0]} port=${rep.endpoint.split(':')[1] || 5432} user=${data.appUser} dbname=${(rep.databases || [])[0] || data.appUser} sslmode=verify-ca"`} />
                <div className="mt-1 text-[11px] text-muted">Password: the <span className="font-mono">{data.userSecret}</span> Secret.</div>
              </div>
            )}
            <div className="flex flex-wrap gap-2 pt-1">
              <Button size="sm" variant="secondary" disabled={!!busy}
                onClick={async () => { setLog(null); const r = await api.logicalReplicaBootstrapLog(rep.name); setLog({ name: rep.name, ...r }) }}>
                Bootstrap log
              </Button>
              <ConfirmButton size="sm" variant="secondary" disabled={!!busy || !rep.inSpec}
                confirmLabel="Reseed — rebuilds it"
                onConfirm={() => run('reseed', () => api.logicalReplicaReseed(rep.name))}>
                {busy === 'reseed' ? 'Reseeding…' : 'Reseed'}
              </ConfirmButton>
              <ConfirmButton size="sm" variant="danger" disabled={!!busy || !rep.inSpec}
                confirmLabel="Remove — deletes its volume"
                onConfirm={() => run('remove', () => api.logicalReplicaRemove(rep.name))}>
                Remove
              </ConfirmButton>
            </div>
          </div>
        ))}
      </div>

      {log && (
        <div className="space-y-1">
          <div className="flex items-center justify-between">
            <span className="text-xs font-medium text-muted">Bootstrap job · <span className="font-mono">{log.job}</span>
              {log.status ? <span className="ml-1 font-mono">(succeeded/failed {log.status})</span> : null}</span>
            <button className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg" onClick={() => setLog(null)}><Icon.Close size={14} /></button>
          </div>
          <pre className="max-h-72 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">
            {log.log || log.error || 'no log yet'}
          </pre>
        </div>
      )}

      <div className="space-y-2 rounded-lg border border-dashed p-2">
        <div className="text-xs font-medium text-muted">Add a logical replica</div>
        {!canAdd && (
          <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-[11px] leading-snug text-danger">
            {blocking.map((c) => <div key={c.name}>{c.name}: {c.detail}</div>)}
          </div>
        )}
        <div className="grid grid-cols-2 gap-2">
          <Field label="Name" hint={`Becomes the Service name; at most ${data.nameMax} characters.`}>
            <input className={inputCls} value={form.name} maxLength={data.nameMax} placeholder="analytics"
              onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Field>
          <Field label="Storage (GiB)">
            <input type="number" min="1" max="512" className={inputCls} value={form.storageGb}
              onChange={(e) => setForm({ ...form, storageGb: Number(e.target.value) })} />
          </Field>
        </div>
        <Field label="Databases" hint="Comma-separated. Blank = every non-template database except postgres.">
          <input className={inputCls} value={form.databases} placeholder="all databases"
            onChange={(e) => setForm({ ...form, databases: e.target.value })} />
        </Field>
        {/* The real list, as chips. The Operator WAITS for a database it cannot find rather than
            failing, so a typo becomes a replica that bootstraps forever with nothing on screen to
            explain it — picking from the cluster's own list is what removes that failure. */}
        {(data.availableDatabases || []).length > 0 && (
          <div className="flex flex-wrap items-center gap-1">
            <span className="text-[11px] text-muted">On this cluster:</span>
            {data.availableDatabases.map((db) => {
              const picked = form.databases.split(',').map((x) => x.trim()).filter(Boolean)
              const on = picked.includes(db)
              return (
                <button key={db} type="button"
                  className={`rounded-full border px-2 py-0.5 font-mono text-[11px] ${on ? 'border-primary bg-primary/10 text-fg' : 'text-muted hover:bg-surface2'}`}
                  onClick={() => setForm({
                    ...form,
                    databases: (on ? picked.filter((x) => x !== db) : [...picked, db]).join(', '),
                  })}>
                  {db}
                </button>
              )
            })}
          </div>
        )}
        <div className="grid grid-cols-2 gap-2">
          <Field label="Seeded by" hint="Read only during bootstrap — changing it later has no effect.">
            <select className={inputCls} value={form.bootstrapMethod}
              onChange={(e) => setForm({ ...form, bootstrapMethod: e.target.value })}>
              <option value="pgbackrest">pgBackRest — from the backup repository (default)</option>
              <option value="pg_basebackup">pg_basebackup — straight off the primary</option>
            </select>
          </Field>
          <Field label="Expose">
            <select className={inputCls} value={form.expose} onChange={(e) => setForm({ ...form, expose: e.target.value })}>
              <option value="ClusterIP">ClusterIP — in-cluster only</option>
              <option value="NodePort">NodePort</option>
              <option value="LoadBalancer">LoadBalancer</option>
            </select>
          </Field>
        </div>
        {/* The single most likely way to get a broken replica, and the only one the panel cannot
            decide for you: nothing records when a database was created, so the backup's own clock
            goes on screen next to the choice that depends on it. */}
        {form.bootstrapMethod === 'pgbackrest' && (
          <p className={`text-[11px] leading-snug ${form.databases.trim() ? 'text-warning' : 'text-muted'}`}>
            A pgBackRest seed restores{' '}
            {data.backups?.completed
              ? <>the backup that finished at <span className="font-mono">{data.backups.completed}</span></>
              : <>the newest backup in the repository</>}.
            {' '}Every database this replica follows must have existed by then — if you created one since, take a new
            backup on the <span className="font-medium text-fg">Backups</span> tab first, or the bootstrap stops with{' '}
            <span className="font-mono">database &quot;…&quot; does not exist</span> and the replica is marked broken.
          </p>
        )}
        <Button size="sm" disabled={!canAdd || !!busy || !form.name.trim()}
          onClick={() => run('add', () => api.logicalReplicaAdd({
            name: form.name.trim(),
            databases: form.databases.split(',').map((d) => d.trim()).filter(Boolean),
            bootstrapMethod: form.bootstrapMethod,
            storageGb: form.storageGb,
            expose: form.expose,
          }))}>
          {busy === 'add' ? 'Adding…' : 'Add replica'}
        </Button>
      </div>

      {/* Straight from the Operator's "Implementation specifics". These are the behaviours that
          make a replica quietly wrong rather than obviously broken, which is exactly the set worth
          having on the screen you operate it from. */}
      <details className="rounded-lg border p-2">
        <summary className="cursor-pointer text-xs font-medium text-muted">What a logical replica does not do</summary>
        <ul className="mt-2 space-y-1.5 text-[11px] leading-snug text-muted">
          <li><span className="font-medium text-fg">Do not write to it.</span> A local write can stop replication; recovering means a reseed.</li>
          <li><span className="font-medium text-fg">Schema changes are not replicated.</span> Run <span className="font-mono">ALTER TABLE</span> on both — add columns on the replica first, drop them on the primary first. Never during bootstrap.</li>
          <li><span className="font-medium text-fg">Only row changes continue</span> after the seed (INSERT/UPDATE/DELETE/TRUNCATE) for the named databases. New tables and databases are ignored; sequences and large objects drift. Updates and deletes need a primary key, a non-null unique key, or <span className="font-mono">REPLICA IDENTITY FULL</span>.</li>
          <li><span className="font-medium text-fg">Failover often breaks it.</span> Slots stay on the old primary (<span className="font-mono">SourceSlotMissing</span>) — reseed.</li>
          <li><span className="font-medium text-fg">A major upgrade invalidates it.</span> Reseed afterwards; remove it first if replication is already unhealthy.</li>
          <li><span className="font-medium text-fg">A failed bootstrap never retries on the same volume.</span> That is what Reseed does: remove, wait for it to leave status, add back.</li>
          <li><span className="font-medium text-fg">It is not in PMM</span>, not behind pgBouncer, and not in the <span className="font-mono">-ha</span> / <span className="font-mono">-replicas</span> Services.</li>
          <li><span className="font-medium text-fg">Removing it always deletes its volume</span>, whatever the cluster's <span className="font-mono">delete-pvc</span> finalizer says.</li>
          <li><span className="font-medium text-fg">Pausing the cluster stops the replica Pod</span>, and apply workers fail while the primary is down.</li>
          <li><span className="font-medium text-fg">Not supported with transparent data encryption.</span> Percona documents this as a limitation to be removed later.</li>
        </ul>
      </details>
    </div>
  )
}
