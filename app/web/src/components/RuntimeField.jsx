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
