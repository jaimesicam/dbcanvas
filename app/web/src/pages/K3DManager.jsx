import { useState } from 'react'
import { Button, Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE } from '../lib/stackApi.js'
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

function KV({ k, v, mono }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-muted">{k}</span>
      <span className={`truncate text-fg ${mono ? 'font-mono text-xs' : ''}`}>{(v ?? '') === '' ? '—' : String(v)}</span>
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
  { id: 'operator', label: 'Operator' },
]

export default function K3DManager({ stackId, nodeId, dep, onDeleteNode }) {
  const [tab, setTab] = useState('overview')
  const { openTerminal } = useTerminals()
  const cfg = dep.config || {}
  const isServer = cfg.role === 'server'
  const ns = cfg.namespace || 'default'
  const cr = cfg.crName || 'cluster1'

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
          <KV k="Budget" v={`${cfg.cpus} CPU · ${cfg.memoryGb} GiB (whole cluster)`} />
          <KV k="LoadBalancer pool" v={cfg.metallbRange || 'MetalLB not installed'} mono />
          <KV k="Operator" v={cfg.operator ? `${cfg.operator.toUpperCase()} ${cfg.operatorVer}` : 'none'} />
          {cfg.operator && <KV k="Namespace" v={ns} mono />}
          {cfg.operator && <KV k="Proxy" v={cfg.proxy === 'proxysql' ? 'ProxySQL' : 'HAProxy'} />}
          {cfg.operator && <KV k="Expose · database" v={cfg.exposePxc || cfg.expose} />}
          {cfg.operator && <KV k="Expose · proxy" v={cfg.exposeProxy || cfg.expose} />}
          <KV k="Backups" v={cfg.backupRepo || 'none'} />
          <KV k="Monitored by" v={cfg.monitoredBy} mono />
          {cfg.monitoredBy && <KV k="PMM service token" v={cfg.pmmToken || 'not created'} />}
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

      {tab === 'operator' && cfg.operator && (
        <div className="space-y-3">
          <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
            The <span className="font-medium text-fg">{cfg.operator.toUpperCase()} operator {cfg.operatorVer}</span> is
            installed in <span className="font-mono">{ns}</span>. Its source — the tag's
            <span className="font-mono"> deploy/bundle.yaml</span> and the <span className="font-mono">cr.yaml</span> that
            was actually applied — is on the server node.
          </div>
          <KV k="Source" v={cfg.operatorSrc} mono />
          <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-snug text-muted">
            <span className="font-medium text-fg">cr.yaml was rewritten before it was applied:</span> anti-affinity set
            to <span className="font-mono">none</span> (a 1–3 node cluster cannot place one database pod per node) and
            every section's CPU/memory requests commented out (the shipped requests do not fit this budget).
            The front end is <span className="font-mono">{cfg.proxy === 'proxysql' ? 'ProxySQL' : 'HAProxy'}</span> (the
            other is disabled — the operator runs one). Services are exposed per section: the database as
            <span className="font-mono"> {cfg.exposePxc || cfg.expose}</span>, the proxy as
            <span className="font-mono"> {cfg.exposeProxy || cfg.expose}</span>.
          </div>
          <Code label="The cluster the operator built" text={`kubectl get pxc -n ${ns}
kubectl get pods -n ${ns}
kubectl get svc -n ${ns}          # EXTERNAL-IP comes from the MetalLB pool`} />
          <Code label="Connect to it (root password)" text={`kubectl -n ${ns} get secret ${cr}-secrets -o jsonpath='{.data.root}' | base64 -d; echo
# then, from any node on the stack network:
mysql -h <EXTERNAL-IP> -u root -p`} />
          {cfg.monitoredBy && (
            <Code label="Rotate the PMM service token (it expires)" text={`# create a new token on the PMM server (Admin role), then:
kubectl -n ${ns} patch secret ${cr}-secrets --type='merge' \\
  -p='{"stringData": {"pmmservertoken": "<new-token>"}}'
kubectl -n ${ns} rollout restart statefulset ${cr}-pxc`} />
          )}
          <Code label="The source, as applied" text={`ls ${cfg.operatorSrc}/deploy
# secrets.yaml was applied BEFORE cr.yaml (the operator reads the users while creating
# the cluster; a secret that arrives later changes nothing). Passwords come from .env.
kubectl apply -f ${cfg.operatorSrc}/deploy/secrets.yaml -n ${ns}
kubectl apply -f ${cfg.operatorSrc}/deploy/cr.yaml -n ${ns}   # re-apply after editing`} />
        </div>
      )}
    </div>
  )
}
