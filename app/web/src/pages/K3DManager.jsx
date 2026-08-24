import { useEffect, useState } from 'react'
import { Button, Badge, Field, ConfirmButton, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, k3dApi } from '../lib/stackApi.js'
import { SecretInline } from '../components/Secret.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

// K3DManager — a running k3s node of a K3D cluster frame.
//
// Everything Kubernetes happens on the *server* node: k3s ships kubectl and the admin kubeconfig,
// the operator source sits in /root, and that is where cr.yaml was applied from. So the panel
// leads with a root console on that node and the handful of commands worth having.

function CopyButton({ text }) {
  const [done, setDone] = useState(false)
  return (
    <button title="Copy"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={14} /> : <Icon.Copy size={14} />}
    </button>
  )
}

// KV renders one label/value row. `v` may be a React node — a link, a masked secret — so only
// primitives go through String(): an element would come out as "[object Object]".
function KV({ k, v, mono }) {
  const empty = v == null || v === ''
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>
        {empty ? '—' : typeof v === 'object' ? v : String(v)}
      </span>
    </div>
  )
}

function Code({ label, text }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-muted">{label}</span>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-60 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">{text}</pre>
    </div>
  )
}

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'kubectl', label: 'kubectl' },
  { id: 'kubeconfig', label: 'Kubeconfig' },
  { id: 'users', label: 'Users' },
  { id: 'operator', label: 'Operator' },
]

const ROLE_HELP = {
  view: 'read-only, this namespace',
  edit: 'read/write, this namespace (no RBAC/quota changes)',
  admin: 'full control, this namespace (including RBAC)',
  'cluster-admin': 'full control, every namespace',
}

function UsersTab({ stackId, frame, isServer }) {
  const api = frame ? k3dApi(stackId, frame.id) : null
  const [users, setUsers] = useState(null)
  const [err, setErr] = useState(null)
  const [busy, setBusy] = useState(false)
  const [form, setForm] = useState({ username: '', namespace: 'default', role: 'view' })

  const load = () => { if (api) api.users().then((r) => setUsers(r.users || [])).catch((e) => setErr(e.message)) }
  useEffect(load, [frame?.id])

  const run = async (fn) => {
    setErr(null); setBusy(true)
    try { await fn(); load() } catch (e) { setErr(e.message) } finally { setBusy(false) }
  }

  const copyKubeconfig = async (username) => {
    try {
      const r = await api.userKubeconfig(username)
      await navigator.clipboard.writeText(r.kubeconfig)
    } catch (e) { setErr(e.message) }
  }

  if (!isServer) {
    return (
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        RBAC users are cluster-wide — open the <span className="font-medium text-fg">server</span> node to manage them.
      </div>
    )
  }

  return (
    <div className="space-y-3">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Each user is a real Kubernetes <span className="font-mono">User</span> (an X.509 client certificate), bound to a
        built-in ClusterRole. Copy a user's kubeconfig, use it from another node on this stack (e.g. the Linux Client
        node's terminal) with <span className="font-mono">--kubeconfig</span>, and confirm what it can and can't do.
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>}

      <div className="space-y-2 rounded-lg border p-2">
        <div className="grid grid-cols-2 gap-2">
          <Field label="Username">
            <input className={inputCls} value={form.username}
              onChange={(e) => setForm((f) => ({ ...f, username: e.target.value }))} placeholder="alice" />
          </Field>
          <Field label="Role">
            <select className={inputCls} value={form.role}
              onChange={(e) => setForm((f) => ({ ...f, role: e.target.value }))}>
              {Object.keys(ROLE_HELP).map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </Field>
        </div>
        {form.role !== 'cluster-admin' && (
          <Field label="Namespace">
            <input className={inputCls} value={form.namespace}
              onChange={(e) => setForm((f) => ({ ...f, namespace: e.target.value }))} placeholder="default" />
          </Field>
        )}
        <div className="text-xs text-muted">{ROLE_HELP[form.role]}</div>
        <Button size="sm" disabled={busy || !form.username.trim()}
          onClick={() => run(() => api.userCreate(form))}>
          <Icon.Plus size={14} /> Create user
        </Button>
      </div>

      <div className="space-y-1.5">
        {users === null && <div className="text-xs text-muted">Loading…</div>}
        {users !== null && users.length === 0 && <div className="text-xs text-muted">No users yet.</div>}
        {(users || []).map((u) => (
          <div key={u.username} className="flex items-center justify-between gap-2 rounded-lg border px-2.5 py-1.5 text-xs">
            <div className="min-w-0">
              <div className="truncate font-mono font-medium text-fg">{u.username}</div>
              <div className="truncate text-muted">
                {u.role}{u.role !== 'cluster-admin' && u.namespace ? ` · ${u.namespace}` : ''}
              </div>
            </div>
            <div className="flex shrink-0 gap-1">
              <Button variant="outline" size="sm" onClick={() => copyKubeconfig(u.username)}>
                <Icon.Copy size={14} /> Kubeconfig
              </Button>
              <ConfirmButton variant="outline" size="sm" disabled={busy}
                onConfirm={() => run(() => api.userDelete(u.username))}>
                <Icon.Trash size={14} />
              </ConfirmButton>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

export default function K3DManager({ stackId, nodeId, frame, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const isServer = cfg.role === 'server'
  const [kubeconfig, setKubeconfig] = useState(null)
  const [kubeconfigErr, setKubeconfigErr] = useState(null)
  useEffect(() => {
    if (tab !== 'kubeconfig' || !isServer || !frame || kubeconfig) return
    k3dApi(stackId, frame.id).kubeconfig()
      .then((r) => setKubeconfig(r.kubeconfig))
      .catch((e) => setKubeconfigErr(e.message))
  }, [tab, isServer, frame, stackId, kubeconfig])
  const ns = cfg.namespace || 'default'
  const cr = cfg.crName || 'cluster1'
  const isMongo = cfg.operator === 'psmdb'
  const isPG = cfg.operator === 'pg'
  const isPS = cfg.operator === 'ps'
  // CloudNativePG has no proxy tier and its own expose/status shape, so the Percona-shaped
  // front-end and expose rows below do not apply to it.
  const isCNPG = cfg.operator === 'cnpg'
  // Crunchy PGO does have the Percona shape (a primary Service plus pgBouncer, and it reuses
  // exposePg/exposePgbouncer), so it keeps those rows and only adds its own status block.
  const isPGO = cfg.operator === 'pgo'
  // The four operators name the same ideas differently: PXC and PS put a proxy in front of the
  // database, PSMDB has routers (and only when sharded), PostgreSQL has a pgBouncer pool.
  const kind = isMongo ? 'psmdb' : isPG ? 'pg' : isPS ? 'ps' : 'pxc'
  const anyPG = isPG || isPGO
  const frontEnd = isMongo
    ? (cfg.sharding ? 'mongos routers' : 'none (replica set)')
    : anyPG ? 'pgBouncer'
      : isPS ? (cfg.proxy === 'router' ? 'MySQL Router' : 'HAProxy')
        : (cfg.proxy === 'proxysql' ? 'ProxySQL' : 'HAProxy')
  const exposeDb = (isMongo ? cfg.exposeReplset : anyPG ? cfg.exposePg : isPS ? cfg.exposeMysql : cfg.exposePxc) || cfg.expose
  const exposeFront = (isMongo ? cfg.exposeMongos : anyPG ? cfg.exposePgbouncer : cfg.exposeProxy) || cfg.expose

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-semibold">k3s {cfg.role || 'node'} · {cfg.hostname}</span>
        <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
      </div>

      {!isServer && (
        <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          This is a worker node. kubectl, the kubeconfig and the operator source live on the cluster's
          <span className="font-medium text-fg"> server</span> node — open that one to drive the cluster.
        </div>
      )}

      <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
        {TABS.filter((t) => t.id !== 'operator' || cfg.operator).map((t) => (
          <button key={t.id} onClick={() => setTab(t.id)}
            className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${tab === t.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'overview' && (
        <div className="space-y-2 text-sm">
          <KV k="Cluster" v={cfg.cluster} mono />
          <KV k="Role" v={cfg.role === 'server' ? 'server (control plane)' : 'agent (worker)'} />
          <KV k="FQDN" v={cfg.fqdn} mono />
          <KV k="Nodes" v={cfg.nodes} />
          <KV k="Kubernetes" v={cfg.k3sVersion || cfg.serverVersion} mono />
          <KV k="Budget" v={`${cfg.cpus} CPU · ${cfg.memoryGb} GiB (whole cluster)`} />
          {cfg.diskLimit && <KV k="Disk limit" v={cfg.diskLimit} />}
          <KV k="LoadBalancer pool" v={cfg.metallbRange || 'MetalLB not installed'} mono />
          <KV k="Operator" v={cfg.operator ? `${cfg.operator.toUpperCase()} ${cfg.operatorVer}` : 'none'} />
          {cfg.operator && <KV k="Namespace" v={ns} mono />}
          {cfg.operator && <KV k="Database cluster" v={cr} mono />}
          {cfg.operator && isCNPG && <KV k="Status" v={cfg.cnpgStatus || 'unknown'} />}
          {cfg.operator && isCNPG && <KV k="Instances" v={`${cfg.cnpgInstances} · ${cfg.cnpgStorageGb} GiB each`} />}
          {cfg.operator && isCNPG && <KV k="PostgreSQL" v={cfg.cnpgPgVersion || "operator default"} />}
          {cfg.operator && isCNPG && <KV k="Expose · Postgres" v={cfg.cnpgExpose || 'ClusterIP'} />}
          {cfg.operator && isCNPG && <KV k="Endpoint" v={cfg.cnpgEndpoint || '—'} mono />}
          {cfg.operator && isCNPG && cfg.cnpgPooler && <KV k="PgBouncer" v={`${cfg.cnpgPoolerInstances} pod(s) · ${cfg.cnpgPoolerMode} · ${cfg.cnpgPoolerExpose}`} />}
          {cfg.operator && isCNPG && cfg.cnpgPooler && <KV k="PgBouncer endpoint" v={cfg.cnpgPoolerEndpoint || '—'} mono />}
          {cfg.operator && isCNPG && <KV k="App role / database" v={`${cfg.cnpgAppUser || '—'} / ${cfg.cnpgAppDb || '—'}`} mono />}
          {cfg.operator && isCNPG && <KV k="Password in Secret" v={cfg.cnpgAppSecret || '—'} mono />}
          {cfg.operator && isPGO && <KV k="Status" v={cfg.pgoStatus || 'unknown'} />}
          {cfg.operator && isPGO && <KV k="Instances" v={`${cfg.pgoInstances} · ${cfg.pgoStorageGb} GiB each`} />}
          {cfg.operator && isPGO && <KV k="PostgreSQL" v={cfg.pgoPgVersion || '—'} />}
          {cfg.operator && isPGO && <KV k="Endpoint" v={cfg.pgoEndpoint || '—'} mono />}
          {cfg.operator && isPGO && <KV k="App role / database" v={`${cfg.pgoAppUser || '—'} / ${cfg.pgoAppDb || '—'}`} mono />}
          {cfg.operator && isPGO && <KV k="Password in Secret" v={cfg.pgoAppSecret || '—'} mono />}
          {cfg.operator && isPS && <KV k="Replication" v={cfg.clusterType === 'async' ? 'Async (Orchestrator)' : 'Group Replication'} />}
          {cfg.operator && !isCNPG && <KV k={isMongo ? 'Topology' : 'Front end'} v={isMongo ? (cfg.sharding ? 'Sharded (rs0 + config servers + mongos)' : 'Replica set (rs0)') : frontEnd} />}
          {cfg.operator && !isCNPG && <KV k={isMongo ? 'Expose · replica set' : 'Expose · database'} v={exposeDb} />}
          {cfg.operator && !isCNPG && (!isMongo || cfg.sharding) && (
            <KV k={isMongo ? 'Expose · mongos' : isPG ? 'Expose · pgBouncer' : 'Expose · proxy'} v={exposeFront} />
          )}
          <KV k="Backups" v={cfg.backupRepo || 'none'} />
          {cfg.debugStatus && (
            <KV k="Debugger"
              v={cfg.debugStatus === 'listening'
                ? (cfg.debugPort
                  ? `Delve on 127.0.0.1:${cfg.debugPort} (in-stack ${cfg.fqdn}:${cfg.debugNodePort})`
                  : `Delve in-stack only (${cfg.fqdn}:${cfg.debugNodePort})`)
                : cfg.debugStatus}
              mono={cfg.debugStatus === 'listening'} />
          )}
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          {cfg.monitoredBy && !isCNPG && !isPGO && <KV k="PMM service token" v={cfg.pmmToken || 'not created'} />}
          {cfg.grafanaUrl && (
            <KV k="Grafana" v={cfg.grafanaUrl === 'pending' ? 'awaiting a LoadBalancer address' : (
              <a className="text-accent underline" href={cfg.grafanaUrl} target="_blank" rel="noreferrer">{cfg.grafanaUrl}</a>
            )} />
          )}
          {/* The address above is a MetalLB one, so the Service it came from is worth naming:
              it is the single `kubectl get svc` that confirms it, and the only way to find the
              address again if the pool ever reassigns it. */}
          {cfg.grafanaService && <KV k="Grafana service" v={cfg.grafanaService} mono />}
          {/* Credentials, so signing in does not mean going and reading $GRAFANA_PASSWORD.
              The password is masked in place — same contract as every other secret row. */}
          {cfg.grafanaUrl && <KV k="Grafana user" v={cfg.grafanaUser || 'admin'} mono />}
          {cfg.grafanaUrl && sec.grafanaPassword && (
            <KV k="Grafana password" v={<SecretInline value={sec.grafanaPassword} />} />
          )}
          {/* Whether a dashboard landed is worth stating: Grafana with Prometheus wired up
              but nothing to look at is the state that reads as "monitoring doesn't work". */}
          {cfg.grafanaUrl && <KV k="Grafana dashboard" v={cfg.grafanaDashboard || 'none installed'} />}
          {cfg.manifestDir && <KV k="Manifests" v={cfg.manifestDir} mono />}
          <KV k="Container" v={dep.containerId ? dep.containerId.slice(0, 12) : '—'} mono />
          <Button variant="outline" size="sm" className="mt-2 w-full"
            onClick={() => openTerminal({ stackId, nodeId, title: `${cfg.hostname} · root` })}>
            <Icon.Nodes size={16} /> Open root console
          </Button>
          <Button variant="danger" size="sm" className="w-full" onClick={onDeleteNode}>
            <Icon.Trash size={16} /> Delete node
          </Button>
        </div>
      )}

      {tab === 'kubectl' && (
        <div className="space-y-3">
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            k3s ships kubectl and its own admin kubeconfig, so there is nothing to install or copy — open a root
            console on the <span className="font-medium text-fg">server</span> node and run these.
          </div>
          <Code label="The cluster" text={`kubectl get nodes -o wide
kubectl get pods -A`} />
          <Code label="MetalLB (LoadBalancer addresses come from the stack subnet)" text={`kubectl -n metallb-system get pods
kubectl -n metallb-system get ipaddresspool dbcanvas -o yaml`} />
          <Code label="From another node in the stack" text={`# LoadBalancer IPs are on the stack network, so any node (e.g. the Ubuntu VNC
# desktop) can reach them directly:
kubectl get svc -n ${ns}`} />
        </div>
      )}

      {tab === 'kubeconfig' && (
        isServer ? (
          <div className="space-y-3">
            <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
              This is the cluster's admin kubeconfig, pointed at k3d's own load balancer
              (<span className="font-mono">k3d-{cfg.cluster}-serverlb</span>) so it works from any other node
              on this stack — e.g. paste it into the <span className="font-medium text-fg">Linux Client</span> node's
              terminal after installing <span className="font-mono">kubectl</span> there. It is not reachable from
              your own machine unless you're on the stack's Docker network.
            </div>
            {kubeconfigErr && <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{kubeconfigErr}</div>}
            {kubeconfig ? <Code label="kubeconfig (admin)" text={kubeconfig} /> : !kubeconfigErr && <div className="text-xs text-muted">Loading…</div>}
          </div>
        ) : (
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            The kubeconfig comes from the cluster's <span className="font-medium text-fg">server</span> node — open that one.
          </div>
        )
      )}

      {tab === 'users' && <UsersTab stackId={stackId} frame={frame} isServer={isServer} />}

      {/* Crunchy PGO is installed from a Helm chart, not from a release tarball, so there is no
          bundle.yaml or cr.yaml to describe — what was applied is the archive on the server
          node. Everything below this branch is about the Percona operators' source tree. */}
      {tab === 'operator' && isPGO && (
        <div className="space-y-3">
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">Crunchy Postgres for Kubernetes {cfg.operatorVer}</span> is installed
            in <span className="font-mono">{ns}</span> from Crunchy's OCI Helm chart, through k3s' bundled
            helm-controller — so there is no release tarball on disk. The manifests DBCanvas generated and applied
            are archived on the server node instead.
          </div>
          <KV k="Manifests" v={cfg.manifestDir || '—'} mono />
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">The application role is deliberately not a superuser.</span>{' '}
            pgBouncer authenticates through an auth_query whose function excludes superusers, so a superuser
            application role can reach the primary directly but gets <span className="font-mono">no such user</span>{' '}
            from the pooler. Both tiers require TLS — connect with{' '}
            <span className="font-mono">sslmode=require</span>.
          </div>
          <Code label="The cluster the operator built" text={`kubectl get postgrescluster -n ${ns}
kubectl get pods -n ${ns}
kubectl get svc -n ${ns}          # EXTERNAL-IP comes from the MetalLB pool`} />
          <Code label={`Connect as ${cfg.pgoAppUser || cr}`} text={`kubectl -n ${ns} get secret ${cfg.pgoAppSecret || `${cr}-pguser-${cr}`} \\
  -o jsonpath='{.data.password}' | base64 -d; echo
# through the pgBouncer pool, from any node on the stack network:
psql "postgres://${cfg.pgoAppUser || cr}:<password>@${cfg.pgoEndpoint || '<EXTERNAL-IP>:5432'}/${cfg.pgoAppDb || cr}?sslmode=require"
# ...or straight from the primary pod:
kubectl -n ${ns} exec -it statefulset/${cr}-instance1 -c database -- psql -U postgres`} />
          <Code label="What was applied, in order" text={`ls ${cfg.manifestDir || '/root/pgo'}
cat ${cfg.manifestDir || '/root/pgo'}/README.md
# the user Secrets go on BEFORE the PostgresCluster: PGO adopts an existing
# <cluster>-pguser-<user> Secret only when it carries the labels it selects by.
cd ${cfg.manifestDir || '/root/pgo'} && for f in [0-9]*.yaml; do kubectl apply -f "$f"; done`} />
        </div>
      )}

      {tab === 'operator' && cfg.operator && !isPGO && (
        <div className="space-y-3">
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            The <span className="font-medium text-fg">{cfg.operator.toUpperCase()} operator {cfg.operatorVer}</span> is
            installed in <span className="font-mono">{ns}</span>. Its source — the tag's
            <span className="font-mono"> deploy/bundle.yaml</span> and the <span className="font-mono">cr.yaml</span> that
            was actually applied — is on the server node.
          </div>
          <KV k="Source" v={cfg.operatorSrc} mono />
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">cr.yaml was rewritten before it was applied:</span> every section's
            CPU/memory requests are commented out (the shipped requests do not fit this budget)
            {!isPG && <>, and anti-affinity is set to <span className="font-mono">none</span> — a 1–3 node cluster
              cannot place one database pod per node</>}.
            {isMongo ? (
              <> The cluster is a <span className="font-mono">{cfg.sharding ? 'sharded cluster' : 'replica set'}</span>,
                and its mongod pods are exposed as <span className="font-mono">{exposeDb}</span>
                {cfg.sharding && <> and the mongos routers as <span className="font-mono">{exposeFront}</span></>}.
              </>
            ) : isPG ? (
              <> PostgreSQL's own anti-affinity is already soft, so only the requests were touched. The primary is
                exposed as <span className="font-mono">{exposeDb}</span> and the pgBouncer pool as
                <span className="font-mono"> {exposeFront}</span>. Backups go to pgBackRest's
                <span className="font-mono"> repo1</span>.
              </>
            ) : (
              <> The front end is <span className="font-mono">{frontEnd}</span> (the other is disabled — the operator
                runs one). Services are exposed per section: the database as
                <span className="font-mono"> {cfg.exposePxc || cfg.expose}</span>, the proxy as
                <span className="font-mono"> {cfg.exposeProxy || cfg.expose}</span>.
              </>
            )}
          </div>
          <Code label="The cluster the operator built" text={`kubectl get ${kind} -n ${ns}
kubectl get pods -n ${ns}
kubectl get svc -n ${ns}          # EXTERNAL-IP comes from the MetalLB pool`} />
          {isPG ? (
            <Code label="Connect to it (the postgres password)" text={`kubectl -n ${ns} get secret ${cr}-pguser-postgres \\
  -o jsonpath='{.data.password}' | base64 -d; echo
# through the pgBouncer pool, from any node on the stack network:
psql "postgres://postgres:<password>@<EXTERNAL-IP>:5432/postgres"
# ...or straight from the primary pod:
kubectl -n ${ns} exec -it ${cr}-instance1-0 -c database -- psql -U postgres`} />
          ) : isMongo ? (
            <Code label="Connect to it (the userAdmin password)" text={`kubectl -n ${ns} get secret ${cr}-secrets \\
  -o jsonpath='{.data.MONGODB_USER_ADMIN_PASSWORD}' | base64 -d; echo
# from any node on the stack network (a LoadBalancer address, or from inside the cluster):
mongosh "mongodb://userAdmin:<password>@<EXTERNAL-IP>:27017/admin"
# ...or straight from a pod, which needs no exposed Service at all:
kubectl -n ${ns} exec -it ${cr}-rs0-0 -c mongod -- mongosh -u userAdmin -p <password>`} />
          ) : (
            <Code label="Connect to it (root password)" text={`kubectl -n ${ns} get secret ${cr}-secrets -o jsonpath='{.data.root}' | base64 -d; echo
# then, from any node on the stack network:
mysql -h <EXTERNAL-IP> -u root -p`} />
          )}
          {cfg.monitoredBy && (
            <Code label="Rotate the PMM service token (it expires)" text={`# create a new token on the PMM server (Admin role), then:
kubectl -n ${ns} patch secret ${isPG ? `${cr}-pmm-secret` : `${cr}-secrets`} --type='merge' \\
  -p='{"stringData": {"${isPG || isMongo ? 'PMM_SERVER_TOKEN' : 'pmmservertoken'}": "<new-token>"}}'
kubectl -n ${ns} rollout restart statefulset -l ${isPG
    ? `postgres-operator.crunchydata.com/cluster=${cr}`
    : `app.kubernetes.io/instance=${cr}`}`} />
          )}
          <Code label="The source, as applied" text={`ls ${cfg.operatorSrc}/deploy
# secrets.yaml was applied BEFORE cr.yaml (the operator reads the users while creating
# the cluster; a secret that arrives later changes nothing). Passwords come from .env.
kubectl apply -f ${cfg.operatorSrc}/deploy/secrets.yaml -n ${ns}
kubectl apply -f ${cfg.operatorSrc}/deploy/cr.yaml -n ${ns}   # re-apply after editing`} />
          {cfg.debugStatus && <IDEAttachGuide cfg={cfg} ns={ns} cr={cr} frameId={frame?.id} stackId={stackId} />}
        </div>
      )}
    </div>
  )
}

// Delve's listener inside the pod — k3ddebug.go's k3dDebugPort, which the NodePort fronts.
const K3D_DELVE_PORT = 40000

// IDEAttachGuide — everything needed to attach an *external* IDE to the operator running under
// Delve. The in-app alternative is the Operator Debugger page, which needs none of it; this is
// here for people who would rather stay in their own editor.
//
// The three steps are in the order they have to happen: a local clone at the tag the deployed
// binary was built from (or the source paths will not match), the launch configuration, and the
// annotation that forces a reconcile so a breakpoint in Reconcile is actually reached.
//
// substitutePath is the piece that is easy to miss. The binary was compiled inside a builder
// container, so the paths in its DWARF are that container's — /go/src/github.com/percona/... — and
// without the mapping the debugger stops at a breakpoint it cannot show source for.
function IDEAttachGuide({ cfg, ns, cr, stackId, frameId }) {
  const listening = cfg.debugStatus === 'listening'
  const published = listening && cfg.debugPort > 0
  const launch = `{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Attach ${cfg.operator.toUpperCase()} operator (dbcanvas)",
      "type": "go",
      "request": "attach",
      "mode": "remote",
      "host": "127.0.0.1",
      "port": ${cfg.debugPort},
      "substitutePath": [
        { "from": "\${workspaceFolder}", "to": "${cfg.debugBuildDir}" }
      ]
    }
  ]
}`
  // The operator only ever builds for Linux: it uses syscall.SIGUSR1, syscall.Mkfifo and
  // golang.org/x/sys/unix. A clone on Windows or macOS therefore type-checks as that OS and the
  // language server marks every one of those undefined — errors that are about the workspace's
  // platform, not the code. Pinning gopls to the platform the deployed binary was built for makes
  // them go away, and is a no-op on a Linux clone.
  const settings = `{
  "go.toolsEnvVars": { "GOOS": "linux", "GOARCH": "${cfg.debugGoarch || 'amd64'}" }
}`
  return (
    <div className="space-y-3">
      {listening && (
        <div className="flex items-center justify-between gap-3 rounded-lg border bg-surface2 px-3 py-2">
          <span className="text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">Debug it here instead.</span> The Operator Debugger
            page steps through this operator with no IDE, no clone and no Go toolchain — and can force
            a reconcile for you.
          </span>
          <Button size="sm" onClick={() => {
            try { sessionStorage.setItem('dbcanvas.debugTarget', `${stackId}/${frameId}`) } catch { /* private mode */ }
            location.hash = 'operator-debugger'
          }}>
            <Icon.Bug size={14} /> Open debugger
          </Button>
        </div>
      )}
      <div className={`rounded-lg px-3 py-2 text-[11px] leading-snug text-muted ${listening
        ? 'border border-accent/30 bg-accent/10' : 'border border-warning/30 bg-warning/10'}`}>
        {listening ? (
          <>
            <span className="font-medium text-fg">The operator is running under Delve.</span> It was rebuilt from
            the {cfg.operatorVer} source with the optimiser off and runs as
            <span className="font-mono"> dlv exec</span>, listening on
            <span className="font-mono"> 127.0.0.1:{cfg.debugPort}</span> here and
            <span className="font-mono"> {cfg.fqdn}:{cfg.debugNodePort}</span> from inside the stack. The pod keeps
            the released image — only its command changed — so the init containers it gives the database pods still
            resolve to the real operator image. Delve was started with
            <span className="font-mono"> --continue</span>, so the cluster is built whether or not you attach; the
            liveness probe and leader election are off, which is what lets you sit on a breakpoint without kubelet
            or the lease killing the process.
            <span className="mt-1 block">
              You can stop the debug session whenever you like: a <span className="font-mono">dbcanvas-debug-watchdog</span>{' '}
              sidecar clears any breakpoints your IDE left armed and resumes the operator within 10 seconds. Without it a
              leftover breakpoint fires on the next reconcile with nobody attached and the operator freezes silently —
              and the session after that shows the breakpoint as unverified. Check it with{' '}
              <span className="font-mono">kubectl -n {ns} logs deploy/percona-xtradb-cluster-operator -c dbcanvas-debug-watchdog</span>.
            </span>
            <span className="mt-1 block">
              On a Windows or macOS clone, set <span className="font-mono">GOOS</span> before anything else — the
              operator is Linux-only code (<span className="font-mono">syscall.SIGUSR1</span>,{' '}
              <span className="font-mono">syscall.Mkfifo</span>, <span className="font-mono">x/sys/unix</span>), so the
              language server otherwise fills the workspace with "undefined" errors that say nothing about the code.
              Breakpoints are unaffected either way: Delve resolves them, not gopls, and a Windows path is mapped by
              the <span className="font-mono">substitutePath</span> below.
            </span>
          </>
        ) : (
          <>
            <span className="font-medium text-fg">The debugger was requested but is not attached.</span>{' '}
            {cfg.debugStatus} — the operator is running normally from its released image.
          </>
        )}
      </div>
      {listening && !published && (
        <div className="rounded-lg border bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
          This frame was deployed without publishing the debugger to the host, so there is nothing for an
          external IDE to attach to — use the button above. To attach an IDE instead, deploy again with
          <span className="font-medium text-fg"> Also publish the debugger to the host</span> ticked on the frame;
          k3d can only publish a port while the cluster is being created.
        </div>
      )}
      {published && (
        <>
          <Code label="1 · clone the source your IDE will step through (the tag the binary was built from)"
            text={`git clone -b v${cfg.operatorVer} https://github.com/percona/percona-xtradb-cluster-operator.git
cd percona-xtradb-cluster-operator   # open THIS directory as the workspace`} />
          <Code label="2 · .vscode/settings.json — the operator is Linux-only code" text={settings} />
          <Code label="3 · .vscode/launch.json in that clone" text={launch} />
          <Code label="4 · break in Reconcile, then force one"
            text={`# breakpoint: pkg/controller/pxc/controller.go, in Reconcile() —
#   err := r.client.Get(ctx, request.NamespacedName, o)
# then, with the debugger attached:
kubectl -n ${ns} annotate pxc ${cr} debug="$(date +%s)" --overwrite`} />
          <Code label="Check on it from here" text={`kubectl -n ${ns} logs deploy/percona-xtradb-cluster-operator -c dbcanvas-debug-watchdog
kubectl -n ${ns} get svc percona-xtradb-cluster-operator-delve
kubectl -n ${ns} logs deployment/percona-xtradb-cluster-operator | head -3
# ...prints Delve's own "API server listening at: [::]:${K3D_DELVE_PORT}"`} />
        </>
      )}
    </div>
  )
}
