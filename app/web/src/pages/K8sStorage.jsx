import { useCallback, useState } from 'react'
import { Badge, ConfirmButton, Field, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { k3dApi } from '../lib/stackApi.js'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { usePolling } from '../lib/usePolling.jsx'
import { Bar, fmtBytes } from './CephManager.jsx'

// K8sStorage — a K3D cluster's volumes, and growing them (app/k3dstorage.go).
//
// Every PVC in the cluster's namespace, with what it asked for, what it got and what it holds.
// Growing is one number: the database volume size, written into the custom resource for the
// operator to resize — offered only where it can happen (Ceph volumes, and an operator release
// with volume scaling), and only upwards, since Kubernetes cannot shrink a volume. While a resize
// runs, a volume shows Kubernetes' own words for where it is.

const GiB = 1 << 30

function quantityBytes(q) {
  const m = String(q || '').match(/^([\d.]+)\s*(Ki|Mi|Gi|Ti|k|K|M|G|T)?$/)
  if (!m) return 0
  const mult = { Ki: 1024, Mi: 1024 ** 2, Gi: GiB, Ti: 1024 ** 4, k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12 }[m[2]] || 1
  return Number(m[1]) * mult
}

export function K8sStorage({ stackId, frame, isServer }) {
  const api = frame ? k3dApi(stackId, frame.id) : null
  const [view, setView] = useState(null)
  const [err, setErr] = useState('')
  const [size, setSize] = useState('')
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState('')

  const load = useCallback(async () => {
    if (!api) return
    try { setView(await api.storage()); setErr('') } catch (e) { setErr(e.message) }
  }, [api?.storage]) // eslint-disable-line react-hooks/exhaustive-deps
  const resizing = !!view?.volumes?.some((v) => v.resizing || (v.data && v.requested !== v.capacity))
  usePolling(load, resizing || busy ? 3000 : 10000, { enabled: isServer && !!frame })

  if (!isServer) return <p className="text-xs text-muted">Volumes are read from the cluster&apos;s server node.</p>

  const data = (view?.volumes || []).filter((v) => v.data)
  // A new size must be larger than every database volume is and asks to be.
  const floor = Math.max(quantityBytes(view?.dataSize), ...data.map((v) => Math.max(quantityBytes(v.capacity), quantityBytes(v.requested))), 0)
  const minGiB = Math.floor(floor / GiB) + 1
  const want = Math.round(Number(size) || 0)
  const valid = want >= minGiB && want <= (view?.maxGiB || 1000)

  const grow = async () => {
    setBusy(true); setErr(''); setNote('')
    try {
      const r = await api.storageGrow(want)
      setNote(`${r.patched} now asks for ${r.size}. The operator resizes each volume; they update below.`)
      setSize('')
      await load()
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-3 text-sm">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-2 py-1.5 text-xs text-danger">{err}</div>}
      {!view ? <p className="text-xs text-muted">Reading the volumes…</p> : (
        <>
          <div className="flex flex-wrap items-center gap-1.5">
            <Icon.Disk size={14} />
            <span className="font-medium">{view.storage === 'ceph' ? 'Ceph RBD' : 'Local (local-path)'}</span>
            <Badge tone="muted">{view.storageClass}</Badge>
            {view.operatorVersion && <span className="text-xs text-muted">· operator {view.operatorVersion}</span>}
            <Help text={HELP.k8sVolumes} />
          </div>

          {view.monitorMoved && (
            <div className="rounded-lg border border-warning/30 bg-warning/10 px-2 py-1.5 text-xs text-warning">
              The Ceph node restarted on another address ({view.monitorMoved}). Ceph CSI still has the old one, so a volume
              cannot be mapped until it is told — growing the volumes re-points it first.
            </div>
          )}

          {view.canGrow ? (
            <div className="space-y-2 rounded-lg border p-2.5">
              <div className="flex items-center gap-1 text-xs font-semibold uppercase tracking-wide text-muted">
                Grow the database volumes <Help text={HELP.k8sGrow} />
              </div>
              <p className="text-xs text-muted">
                Now {view.dataSize || '—'} each{data.length ? ` (${data.length} volume${data.length === 1 ? '' : 's'})` : ''}. The new size goes into the custom
                resource — {view.scaling === 'dataVolumeClaimSpec' ? 'each instance’s dataVolumeClaimSpec' : 'the volumeSpec, with ' + (view.scaling === 'storageScaling' ? 'storageScaling' : 'enableVolumeExpansion') + ' on'} —
                and the operator resizes every volume while the database runs. Volumes cannot shrink.
              </p>
              <div className="flex items-end gap-2">
                <Field label="New size (GiB per volume)">
                  <input type="number" min={minGiB} max={view.maxGiB} className={`${inputCls} w-32`} value={size}
                    placeholder={String(minGiB)} onChange={(e) => setSize(e.target.value)} />
                </Field>
                <ConfirmButton variant="primary" disabled={!valid || busy} onConfirm={grow} confirmLabel={`Grow to ${want} GiB?`}>
                  {busy ? 'Growing…' : 'Grow'}
                </ConfirmButton>
              </div>
              {size !== '' && !valid && (
                <p className="text-xs text-danger">A new size is {minGiB} to {view.maxGiB} GiB — larger than the volumes are now.</p>
              )}
              {note && <p className="text-xs text-success">{note}</p>}
            </div>
          ) : (
            <p className="rounded-lg bg-surface2 px-2 py-1.5 text-xs text-muted">These volumes cannot grow: {view.why}.</p>
          )}

          <div className="space-y-1.5">
            {view.volumes.length === 0 && <p className="text-xs text-muted">No volumes in {frame?.label}&apos;s namespace yet.</p>}
            {view.volumes.map((v) => {
              const changing = v.data && v.requested && v.capacity && v.requested !== v.capacity
              return (
                <div key={v.name} className="rounded-lg border px-2 py-1.5 text-xs">
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="flex min-w-0 items-center gap-1.5">
                      <span className="truncate font-medium" title={v.name}>{v.name}</span>
                      {v.data && <Badge tone="primary">data</Badge>}
                    </span>
                    <span className="shrink-0 text-muted">
                      {changing ? <><span className="text-warning">{v.capacity} → {v.requested}</span></> : (v.capacity || v.requested || v.phase)}
                    </span>
                  </div>
                  {v.capacityBytes > 0 && (
                    <>
                      <Bar used={v.usedBytes} total={v.capacityBytes} />
                      <div className="mt-0.5 flex justify-between text-[11px] text-muted">
                        <span>{fmtBytes(v.usedBytes)} used of {fmtBytes(v.capacityBytes)} (filesystem)</span>
                        {v.pod && <span className="truncate">{v.pod}</span>}
                      </div>
                    </>
                  )}
                  {v.resizing && <div className="mt-0.5 text-[11px] text-warning">{v.resizing}</div>}
                </div>
              )
            })}
          </div>
        </>
      )}
    </div>
  )
}
