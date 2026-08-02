// smoke/render.jsx — a render smoke test for the All-in-One components.
//
// Why this exists: `Icon.ChevronDown` did not exist, so React rendered
// `<undefined />`, threw "Element type is invalid", and blanked the entire page.
// Nothing caught it — `vite build` compiles a bad property lookup happily, Go's
// vet never sees JSX, and the Go unit suite does not render React. The bug only
// appeared once a user clicked a feature, because that is the first thing that
// renders an instance card.
//
// So: actually render the components. react-dom's renderToString is enough to
// catch every "element type is invalid" and every crash in render, and it needs
// NO new dependencies — react-dom is already here. Effects do not run under SSR,
// which is what keeps this hermetic (no fetch, no API, no DOM).
//
// Run with `npm run smoke`.

import { renderToString } from 'react-dom/server'
import { AllInOneForm, AllInOneManager, connectString, credRows, __tabsForTest } from '../src/pages/AllInOne.jsx'
import { TerminalProvider } from '../src/terminal/TerminalProvider.jsx'
import { AIO_KINDS, kindOf } from '../src/lib/aioPorts.js'

const noop = () => {}
let failures = 0

function check(name, fn) {
  try {
    const html = fn()
    if (typeof html !== 'string' || html.length === 0) {
      throw new Error('rendered nothing')
    }
    console.log(`  ok    ${name}`)
  } catch (err) {
    failures++
    console.log(`  FAIL  ${name}\n        ${err.message}`)
  }
}

// Canvas nodes the form's drop-downs resolve against.
const nodes = [
  { id: 'pmm1', type: 'pmm', label: 'pmm' },
  { id: 'bao1', type: 'openbao', label: 'openbao' },
  { id: 'sw1', type: 'seaweedfs', label: 'seaweedfs' },
  { id: 'intra1', type: 'intranet', label: 'intranet' },
  { id: 'orch1', type: 'orchestrator', label: 'orchestrator' },
]

// One instance of EVERY kind, so every per-kind branch in InstanceCard renders.
// This is the case that would have caught the chevron: a card only appears once
// a feature exists.
const everyKind = AIO_KINDS.map((k, i) => ({
  id: `i${i}`,
  kind: k.kind,
  name: `${k.kind}01`,
  members: k.cluster ? k.def : 1,
  gtid: true,
  replMode: k.kind === 'innodb' ? 'groupreplication' : 'async',
  generateCert: true,
  certTtlValue: 365,
  certTtlUnit: 'days',
  pmmNodeId: 'pmm1',
  // A proxy must front something, or its card renders the "no backend" path.
  backendInstanceId: k.family === 'proxysql' || k.family === 'haproxy' ? 'i0' : '',
  exportEnabled: true,
  exportHostPort: 0,
}))

const baseNode = (instances) => ({
  id: 'aio1', type: 'aio', label: 'aio1',
  os: 'oraclelinux', osVersion: '9', arch: 'amd64',
  aioPsMajor: '8.0', aioPxcMajor: '8.0',
  aioInstances: instances,
})

const form = (instances, deployed = false) =>
  renderToString(
    <AllInOneForm
      node={baseNode(instances)} nodes={nodes}
      patchNode={noop} deleteNode={noop}
      dep={deployed ? { state: 'running' } : null} deployed={deployed}
    />,
  )

console.log('All-in-One render smoke test')

check('form with no instances', () => form([]))
check('form with every kind (all instance cards)', () => form(everyKind))
check('form when deployed (controls locked)', () => form(everyKind, true))

// The MySQL flavor conflict renders a warning banner — a branch of its own.
check('form with a PXC + Percona Server conflict', () =>
  form([
    { id: 'a', kind: 'ps', name: 'ps01', members: 1 },
    { id: 'b', kind: 'pxc', name: 'pxc-cluster-01', members: 3 },
  ]))

// Each kind on its own, so a failure names the kind rather than "everything".
for (const k of AIO_KINDS) {
  check(`instance card: ${k.kind}`, () =>
    form([everyKind.find((i) => i.kind === k.kind)]))
}

// The deployed-node manager renders from a deployment config, not the design.
const runtime = AIO_KINDS.filter((k) => kindOf(k.kind)).map((k, i) => ({
  inst: `${k.kind}01`, kind: k.kind, family: k.family,
  group: k.cluster ? `${k.kind}01` : '', role: k.cluster ? 'bootstrap' : 'standalone',
  unit: `aio-${k.kind}01`, fqdn: `${k.kind}01.example.net`,
  dataDir: `/opt/aio/${k.kind}01/data`, conf: '', client: 'mysql',
  ports: { base: 13000 + i * 10, client: 13000 + i * 10, admin: 13001 + i * 10 },
  state: 'active', export: 0,
}))

// The manager has four tabs. Only the default renders on mount, so each is
// exercised by rendering with a deployment whose config supplies the rows —
// otherwise a crash in Connect or Credentials would ship unseen, which is
// exactly how the chevron got through.
const managerDep = {
  state: 'running',
  config: { hostname: 'aio1', flavor: 'ps', instances: runtime },
  secrets: {
    adminUser: 'admin', adminPassword: 'pw', rootUser: 'root', rootPassword: 'pw',
    replUser: 'repl', replPassword: 'pw', superPassword: 'pw', valkeyPassword: 'pw',
  },
}

// The manager calls useTerminals(), whose context is null outside its provider —
// App.jsx wraps the whole tree in one, so the harness must too rather than the
// component being made to tolerate its absence.
const managed = (dep) =>
  renderToString(
    <TerminalProvider>
      <AllInOneManager stackId={1} nodeId="aio1" dep={dep} onDeleteNode={noop} />
    </TerminalProvider>,
  )

check('manager (default tab)', () => managed(managerDep))
check('manager with an empty deployment', () => managed({ state: 'running' }))

// Only the DEFAULT tab renders under SSR, so each other tab is rendered
// directly. Without this a bad reference in Connect / Credentials / Ports is
// invisible to the harness — the exact hole that let Icon.ChevronDown ship.
const { ConnectTab, CredentialsTab, PortsTab } = __tabsForTest
const dbOnly = runtime.filter((r) => ['mysql', 'postgres', 'mongodb'].includes(r.family))
const published = runtime.map((r, i) => (i === 0 ? { ...r, export: 34000 } : r))

check('tab: Connect', () =>
  renderToString(<ConnectTab rows={dbOnly} sec={managerDep.secrets} onConsole={noop} />))
check('tab: Connect (published host port)', () =>
  renderToString(<ConnectTab rows={published.filter((r) => ['mysql', 'postgres', 'mongodb'].includes(r.family))}
    sec={managerDep.secrets} onConsole={noop} />))
check('tab: Connect (no databases)', () =>
  renderToString(<ConnectTab rows={[]} sec={managerDep.secrets} onConsole={noop} />))
check('tab: Credentials', () =>
  renderToString(<CredentialsTab rows={runtime} sec={managerDep.secrets} />))
check('tab: Credentials (no secrets)', () =>
  renderToString(<CredentialsTab rows={runtime} sec={{}} />))
check('tab: Ports', () => renderToString(<PortsTab rows={runtime} />))
check('tab: Ports (with a published port)', () => renderToString(<PortsTab rows={published} />))

// The non-default tabs cannot be reached by SSR (no click), so their pure
// helpers are exercised directly — they are where the formatting logic lives and
// where an undefined field would throw.
check('connect strings for every database family', () => {
  const out = runtime
    .filter((r) => ['mysql', 'postgres', 'mongodb'].includes(r.family))
    .map((r) => connectString(r, managerDep.secrets))
  if (!out.length) throw new Error('no database rows to build a connect string from')
  for (const s of out) {
    if (!s || s.includes('undefined')) throw new Error(`bad connect string: ${s}`)
  }
  return out.join('\n')
})

check('credential rows for every family present', () => {
  const rows = credRows(runtime, managerDep.secrets)
  if (!rows.length) throw new Error('no credential rows built')
  for (const r of rows) {
    if (!r.label || !r.user || !r.pass) throw new Error(`incomplete credential row: ${JSON.stringify(r)}`)
  }
  return JSON.stringify(rows)
})

if (failures > 0) {
  console.error(`\n${failures} render failure(s)`)
  process.exit(1)
}
console.log('\nall render checks passed')
