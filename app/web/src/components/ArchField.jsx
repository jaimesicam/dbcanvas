import { useSettings } from '../settings/SettingsProvider.jsx'
import { Field, inputCls } from './ui.jsx'

// ArchField is the architecture a node or cluster runs on: Automatic (the Docker host's own,
// saved as nothing, so a design follows the host it is deployed on), amd64 or arm64. Some are
// not free to choose, and say why (app/platforms.go): Kubernetes nodes run on the host's own
// architecture, a few images exist for amd64 only, a cluster member runs on its cluster's,
// and a deployed node keeps the one it was built with.

// K8S and AMD64_ONLY mirror kubernetesFrames and amd64Only in app/platforms.go.
export const K8S = { k3d: 'K3D', kubeadm: 'Kubeadm' }
export const AMD64_ONLY = { pmm2: 'PMM 2', mclusteradmin: 'MClusterAdmin' }

// PMM_NOTE says what a node's release decides (app/pmmarch.go).
const PMM_NOTE = { pmm: 'PMM publishes arm64 images from 3.10.0; an earlier release runs as amd64, emulated on an arm64 host.' }

// archFacts is what an item runs as, and whether and why that is fixed. member is set for a
// cluster member, with its cluster's arch ('' for Automatic).
export function archFacts(obj, system, member) {
  const host = system?.hostArch || ''
  const def = system?.defaultArch || host
  if (K8S[obj.type]) {
    return { arch: system?.kubernetesArch || host, fixed: `${K8S[obj.type]} nodes run on the Docker host's own architecture: Kubernetes cannot create pods under emulation.`, host, def }
  }
  if (AMD64_ONLY[obj.type]) return { arch: 'amd64', fixed: `${AMD64_ONLY[obj.type]} images exist for amd64 only.`, host, def }
  if (member !== undefined) return { arch: member || def, fixed: 'A cluster member runs on its cluster’s architecture.', host, def }
  return { arch: obj.arch || def, fixed: '', host, def }
}

export function ArchField({ obj, member, deployed, onChange }) {
  const { system } = useSettings()
  const f = archFacts(obj, system, member)
  const emulated = (a) => !!f.host && a !== f.host
  const label = (a) => (emulated(a) ? `${a} — emulated` : a)
  const note = emulated(f.arch)
    ? `Runs under emulation on this ${f.host} Docker host: it works, more slowly.`
    : ''
  if (f.fixed) {
    return (
      <Field label="Architecture" hint={[f.fixed, note].filter(Boolean).join(' ')}>
        <div className={`${inputCls} opacity-70`} aria-readonly="true">{f.arch ? label(f.arch) : 'the Docker host’s own'}</div>
      </Field>
    )
  }
  const hint = deployed
    ? `Built as ${f.arch}; it cannot change while the node is deployed.${note ? ' ' + note : ''}`
    : [note || 'Automatic follows the Docker host the stack is deployed on.', PMM_NOTE[obj.type]].filter(Boolean).join(' ')
  return (
    <Field label="Architecture" hint={hint}>
      <select className={inputCls} value={obj.arch || ''} disabled={deployed}
        onChange={(e) => onChange(e.target.value)}>
        <option value="">{`Automatic (${f.def || 'this Docker host'}${f.def && f.def === f.host ? ', this Docker host' : ''})`}</option>
        <option value="amd64">{label('amd64')}</option>
        <option value="arm64">{label('arm64')}</option>
      </select>
    </Field>
  )
}

// archCardLabel is the architecture as a node card's tooltip says it: "arm64", or
// "amd64 · emulated" when that is not the Docker host's own.
export function archCardLabel(f) {
  if (!f.arch) return ''
  return f.host && f.arch !== f.host ? `${f.arch} · emulated` : f.arch
}
