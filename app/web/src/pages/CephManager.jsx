import { useCallback, useState } from 'react'
import { Badge } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { DEPLOY_TONE, cephApi } from '../lib/stackApi.js'
import { SecretValue } from '../components/Secret.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { usePolling } from '../lib/usePolling.jsx'

// CephManager — a running Ceph node (app/ceph.go): its health, how full it is, and every RBD
// image on it with the Kubernetes volume it backs. A K3D cluster's volumes are made, grown and
// deleted from the cluster (its Storage tab); this is the other side of the same disks, which is
// where a grow shows up as a bigger image and a filling volume as one that holds more.

export function fmtBytes(n) {
  if (!n) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++ }
  return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${u[i]}`
}

// Bar is a used-of-total meter.
export function Bar({ used, total }) {
  const pct = total ? Math.min(100, (used / total) * 100) : 0
  const tone = pct >= 90 ? 'bg-danger' : pct >= 75 ? 'bg-warning' : 'bg-primary'
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface2" role="meter" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
      <div className={`h-full ${tone}`} style={{ width: `${pct}%` }} />
    </div>
  )
}

const HEALTH_TONE = { HEALTH_OK: 'success', HEALTH_WARN: 'warning', HEALTH_ERR: 'danger' }

export default function CephManager({ stackId, nodeId, dep }) {
  const cfg = dep.config || {}
  const sec = dep.secrets || {}
  const [st, setSt] = useState(null)
  const [err, setErr] = useState('')
  const [raw, setRaw] = useState(false)

  const load = useCallback(() => cephApi.status(stackId, nodeId)
    .then((s) => { setSt(s); setErr('') })
    .catch((e) => setErr(e.message)), [stackId, nodeId])
  usePolling(load, 5000)

  const images = st?.images || []
  return (
    <div className="space-y-3 text-sm">
      <div className="flex items-center justify-between">
        <span className="flex items-center gap-1.5 font-semibold"><Icon.Disk size={15} /> Ceph</span>
        <span className="flex items-center gap-1.5">
          {st?.health && <Badge tone={HEALTH_TONE[st.health] || 'muted'}>{st.health.replace('HEALTH_', '')}</Badge>}
          <Badge tone={DEPLOY_TONE[dep.state] || 'muted'}>{dep.state}</Badge>
        </span>
      </div>
      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-2 py-1.5 text-xs text-danger">{err}</div>}

      <div className="space-y-1 rounded-lg border p-2.5">
        <div className="flex items-baseline justify-between">
          <span className="text-muted">Used</span>
          <span>{st ? `${fmtBytes(st.usedBytes)} of ${fmtBytes(st.totalBytes)}` : '…'}</span>
        </div>
        <Bar used={st?.usedBytes || 0} total={st?.totalBytes || 0} />
        <div className="flex items-baseline justify-between text-xs text-muted">
          <span className="flex items-center gap-1">Promised to volumes <Help text={HELP.cephThin} /></span>
          <span className={st && st.provisionedBytes > st.totalBytes ? 'font-medium text-warning' : ''}>{st ? fmtBytes(st.provisionedBytes) : '…'}</span>
        </div>
      </div>

      <div className="space-y-1 text-xs">
        {[['Monitor', cfg.monitor], ['Cluster id (fsid)', cfg.fsid], ['Pool', cfg.pool], ['OSD', cfg.osdSizeGb ? `${cfg.osdSizeGb} GB sparse file` : ''],
          ['Image', cfg.image]].map(([k, v]) => (
          <div key={k} className="flex justify-between gap-3"><span className="text-muted">{k}</span><span className="truncate font-mono">{v || '—'}</span></div>
        ))}
        <div className="flex items-center justify-between gap-3">
          <span className="text-muted">Kubernetes key (client.{cfg.user || 'k8s'})</span>
          {sec.userKey ? <SecretValue value={sec.userKey} /> : <span>—</span>}
        </div>
      </div>

      <div>
        <div className="mb-1 flex items-center gap-1 text-xs font-semibold uppercase tracking-wide text-muted">
          Volumes ({images.length}) <Help text={HELP.cephImages} />
        </div>
        {images.length === 0 ? (
          <p className="text-xs text-muted">No volumes yet. A K3D cluster that keeps its volumes on this node creates them as its pods start.</p>
        ) : (
          <div className="space-y-1.5">
            {images.map((im) => (
              <div key={im.name} className="rounded-lg border px-2 py-1.5 text-xs">
                <div className="flex items-baseline justify-between gap-2">
                  <span className="truncate font-medium" title={im.name}>{im.pvc ? `${im.namespace}/${im.pvc}` : im.name}</span>
                  <span className="shrink-0 text-muted">{fmtBytes(im.usedBytes)} of {fmtBytes(im.provisionedBytes)}</span>
                </div>
                <Bar used={im.usedBytes} total={im.provisionedBytes} />
                {im.pvc && <div className="mt-0.5 truncate font-mono text-[11px] text-muted">{im.name}</div>}
              </div>
            ))}
          </div>
        )}
      </div>

      <button onClick={() => setRaw((v) => !v)} className="text-xs text-primary hover:underline">{raw ? 'Hide' : 'Show'} ceph -s</button>
      {raw && <pre className="max-h-64 overflow-auto rounded-lg bg-surface2 p-2 font-mono text-[11px] leading-snug">{st?.status || '…'}</pre>}
    </div>
  )
}
