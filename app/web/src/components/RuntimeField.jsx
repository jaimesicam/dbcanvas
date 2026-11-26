import { useSettings } from '../settings/SettingsProvider.jsx'
import { Field, inputCls } from './ui.jsx'

// RuntimeField is where a node or cluster runs: in a container (the default, saved as nothing)
// or as a virtual machine on Lima (app/engine_router.go). It shows only for the types that may
// run as a VM (system.vmTypes); on a machine without Lima it says why it cannot be chosen. A
// cluster member follows its cluster unless it says otherwise, so one member can run as a VM in
// a cluster of containers.

const LABELS = { '': 'Container (Docker)', docker: 'Container (Docker)', vm: 'Virtual machine (Lima)' }

export function runtimeOffered(system, type) {
  return (system?.vmTypes || []).includes(type)
}

export function RuntimeField({ obj, member, deployed, onChange }) {
  const { system } = useSettings()
  if (!runtimeOffered(system, obj.type)) return null
  const unavailable = system?.vmUnavailable || ''
  const value = obj.runtime || ''
  const memberOf = member !== undefined
  const effective = value || (memberOf ? member || '' : '')
  let hint = effective === 'vm'
    ? 'A virtual machine with its own kernel, SELinux or AppArmor and systemd, on the stack network through the Intranet. Slower to start than a container.'
    : 'A container on the stack network, sharing the Docker host’s kernel.'
  if (deployed) hint = 'It cannot change while the node is deployed.'
  if (unavailable && value !== 'vm' && member !== 'vm') hint = `A virtual machine cannot be chosen on this machine: ${unavailable}`
  return (
    <Field label="Runs on" hint={hint}>
      <select className={inputCls} value={value} disabled={deployed || (!!unavailable && value !== 'vm')}
        data-runtime-field="" onChange={(e) => onChange(e.target.value)}>
        {memberOf && <option value="">{`As its cluster (${LABELS[member || '']})`}</option>}
        {memberOf ? <option value="docker">{LABELS.docker}</option> : <option value="">{LABELS['']}</option>}
        <option value="vm">{LABELS.vm}</option>
      </select>
    </Field>
  )
}

// SELINUX_PERMISSIVE mirrors vmSELinuxPermissive in app/engine_router.go: the types whose VMs run
// SELinux permissive unless set otherwise.
export const SELINUX_PERMISSIVE = { aio: 'All in One', valkey: 'Valkey', valkeycluster: 'Valkey' }

// SELinuxField is the SELinux mode of a node that runs as a VM, on an OS with SELinux (Oracle
// Linux): its type's default (saved as nothing), Enforcing or Permissive. A cluster member
// follows its cluster unless it says otherwise. It shows only while the node runs as a VM.
export function SELinuxField({ obj, member, memberRuntime, deployed, onChange }) {
  const memberOf = member !== undefined
  const runtime = obj.runtime || (memberOf ? memberRuntime || '' : '')
  if (runtime !== 'vm') return null
  const def = SELINUX_PERMISSIVE[obj.type] ? 'permissive' : 'enforcing'
  const name = (m) => (m === 'permissive' ? 'Permissive' : 'Enforcing')
  const inherited = memberOf && member ? member : def
  const hint = deployed
    ? 'It cannot change while the node is deployed.'
    : SELINUX_PERMISSIVE[obj.type]
      ? `${SELINUX_PERMISSIVE[obj.type]} runs permissive by default: there is no SELinux policy for it. Applies on Oracle Linux; Ubuntu and Debian use AppArmor.`
      : 'Applies on Oracle Linux; Ubuntu and Debian use AppArmor.'
  return (
    <div className="mt-3">
      <Field label="SELinux" hint={hint}>
        <select className={inputCls} value={obj.selinux || ''} disabled={deployed}
          data-selinux-field="" onChange={(e) => onChange(e.target.value)}>
          <option value="">{memberOf ? `As its cluster (${name(inherited)})` : `Default (${name(def)})`}</option>
          <option value="enforcing">Enforcing</option>
          <option value="permissive">Permissive</option>
        </select>
      </Field>
    </div>
  )
}

// isVMNode reports whether a deployment runs as a VM: a Lima instance's id starts with dbc-, a
// Docker container's id (hexadecimal) never does (app/engine_router.go, isLimaID).
export function isVMNode(dep) {
  return !!dep?.containerId && dep.containerId.startsWith('dbc-')
}

// limaShellCommand is the root shell on a VM node from one's own terminal, the counterpart of
// `docker exec -it … bash`. LIMA_HOME is quoted when the path needs it.
export function limaShellCommand(limaHome, instance) {
  const home = limaHome || '~/.dbcanvas/lima'
  const quoted = /^[\w./~+-]+$/.test(home) ? home : `'${home.replace(/'/g, `'\\''`)}'`
  return `LIMA_HOME=${quoted} limactl shell ${instance} sudo -i`
}
