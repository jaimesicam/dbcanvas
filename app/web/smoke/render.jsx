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
import { Icon } from '../src/components/Icons.jsx'
import { AllInOneForm, AllInOneManager, connectString, credRows, __tabsForTest } from '../src/pages/AllInOne.jsx'
import { TerminalProvider } from '../src/terminal/TerminalProvider.jsx'
import { AIO_KINDS, kindOf } from '../src/lib/aioPorts.js'
import {
  MariaDBNodeForm, MariaDBFrameForm, MariaDBGaleraFrameForm,
  MySQLCENodeForm, MySQLCEFrameForm, MySQLCEInnoDBFrameForm,
  UpstreamMemberForm,
} from '../src/pages/UpstreamForms.jsx'
import { Comparison, Verdicts, Advisor, ChartCard, KeptCaptures, HeadToHead, VerdictBody, VerdictMark, ConfigAdvice as StalkConfig } from '../src/pages/StalkSummary.jsx'
import {
  frameMemberSub, REPL_FRAME_TYPES,
  NODE_TYPES, CONNECTABLE_FRAMES, SS_LINK_TYPES, SS_LINK_ENGINE,
  K3D_OPERATOR_LABEL, ssLinkEngine,
} from '../src/pages/StackDesigner.jsx'
import MySQLManager from '../src/pages/MySQLManager.jsx'
import OidcLoginGuide from '../src/components/OidcLoginGuide.jsx'
import PacketInspector, {
  Timeline as PktTimeline, RangeControls as PktRangeControls, Filters as PktFilters,
  PacketList as PktList, PacketDetails as PktDetails, SummaryStrip as PktSummary,
  CaptureState as PktState, Pager as PktPager, ServerLogCard as PktServerLog,
  FilePick as PktFilePick,
} from '../src/pages/PacketInspector.jsx'
import { PORT_ROLE_TEXT, MONGO_KIND_TEXT, isSevereIssue } from '../src/lib/pktApi.js'
import LogSummary, {
  Verdict as LogVerdict, splitFindings, EventColumns as LogEventColumns, Swimlane as LogSwimlane, Snapshot as LogSnapshot,
  SourcesCard as LogSources, EventList as LogEvents, EventDetail as LogDetail,
  Filters as LogFilters, TopStrip as LogTop, Legend as LogLegend,
  RangeControls as LogRange, UploadPanel as LogUpload, Pager as LogPager,
  NodeChip as LogNodeChip,
} from '../src/pages/LogSummary.jsx'
import {
  SEVS, SEV_TEXT, SEV_FILL, STATE_SEV, STATE_TEXT, CLASS_LABEL, logDur, FLAVOUR_LABEL,
  NODE_SLOTS, nodeFill, nodeTint, nodeEdge, nodeEdgeSoft,
} from '../src/lib/logApi.js'
import FTDCSummary, {
  Summary as FtdcSummary, ChartCard as FtdcChart, Advice as FtdcAdvice, Charts as FtdcCharts,
  Findings as FtdcFindings, ConfigAdvice as FtdcConfig,
} from '../src/pages/FTDCSummary.jsx'
import { chartPoints, chartLines, fmtSpan, fmtNum, ADVICE_TEXT, ADVICE_FILL, ADVICE_TONE } from '../src/lib/ftdcApi.js'
import realDeps from './real-deps.json' with { type: 'json' }

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

// PSMDB Sharded is fixed-topology (5 or 13). A design saved with anything else
// renders the picker's "not a supported topology" option — its own branch.
check('form with an unsupported PSMDB Sharded topology', () =>
  form([{ id: 'a', kind: 'psmdbsharded', name: 'sh01', members: 7 }]))

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

// ---- MariaDB / MySQL Community designer forms ----
// Same reasoning as the All-in-One cards above: these six forms are only reachable
// by selecting a node, so a bad element reference in one of them would blank the
// inspector and nothing else would notice. Effects (the catalog fetch) do not run
// under SSR, so each form renders against its *empty* catalog — which is also the
// real first-paint state, before the fetch resolves.
const upstreamNodes = [{ id: 'pmm1', type: 'pmm', label: 'pmm' }]

const mariadbNode = {
  id: 'md1', type: 'mariadb', label: 'mariadb01', os: 'oraclelinux', osVersion: '9', arch: 'amd64',
  mariadbMajor: '11.4', mariadbVersion: '', gtid: true, pmmNodeId: '', useProxy: false,
  generateCert: true, certTtlValue: 365, certTtlUnit: 'days', exportEnabled: true, exportHostPort: 0,
}
const mysqlceNode = {
  id: 'my1', type: 'mysqlce', label: 'mysql01', os: 'oraclelinux', osVersion: '9', arch: 'amd64',
  mysqlceMajor: '8.4', mysqlceVersion: '', gtid: true, pmmNodeId: '', useProxy: false,
  generateCert: false, exportEnabled: false, exportHostPort: 0,
}
const mdFrame = { id: 'f1', type: 'mariadbrepl', label: 'mariadb', os: 'oraclelinux', osVersion: '9', arch: 'amd64', mariadbMajor: '11.4', mariadbVersion: '', gtid: true, replMode: 'async' }
const galFrame = { ...mdFrame, id: 'f2', type: 'mariadbgalera', label: 'galera' }
const ceFrame = { id: 'f3', type: 'mysqlcerepl', label: 'mysqlce', os: 'oraclelinux', osVersion: '9', arch: 'amd64', mysqlceMajor: '8.4', mysqlceVersion: '', gtid: true, replMode: 'async' }
const idcFrame = { ...ceFrame, id: 'f4', type: 'mysqlceinnodb', label: 'myidc', replMode: 'innodbcluster', mysqlRouter: true }

// Two members: enough to exercise the "exactly one primary" and quorum warnings.
const frameMembers = (fid, type) => [
  { id: `${fid}n1`, type, label: `${type}01`, frameId: fid, role: 'primary', exportEnabled: false },
  { id: `${fid}n2`, type, label: `${type}02`, frameId: fid, role: 'secondary', exportEnabled: false },
]

check('MariaDBNodeForm', () => renderToString(
  <MariaDBNodeForm node={mariadbNode} nodes={upstreamNodes} patchNode={noop} deleteNode={noop} deployed={false} />))
check('MySQLCENodeForm', () => renderToString(
  <MySQLCENodeForm node={mysqlceNode} nodes={upstreamNodes} patchNode={noop} deleteNode={noop} deployed={false} />))
check('MariaDBFrameForm', () => renderToString(
  <MariaDBFrameForm frame={mdFrame} nodes={frameMembers('f1', 'mariadbrepl')} patchFrame={noop} deleteFrame={noop} deployed={false} />))
check('MariaDBGaleraFrameForm', () => renderToString(
  <MariaDBGaleraFrameForm frame={galFrame} nodes={frameMembers('f2', 'mariadbgalera')} patchFrame={noop} deleteFrame={noop} deployed={false} />))
check('MySQLCEFrameForm', () => renderToString(
  <MySQLCEFrameForm frame={ceFrame} nodes={frameMembers('f3', 'mysqlcerepl')} patchFrame={noop} deleteFrame={noop} deployed={false} />))
check('MySQLCEInnoDBFrameForm', () => renderToString(
  <MySQLCEInnoDBFrameForm frame={idcFrame} nodes={frameMembers('f4', 'mysqlceinnodb')} patchFrame={noop} deleteFrame={noop} deployed={false} />))
check('UpstreamMemberForm (with role)', () => renderToString(
  <UpstreamMemberForm node={frameMembers('f1', 'mariadbrepl')[0]} frame={mdFrame} patchNode={noop} deleteNode={noop} deployed={false} roles />))
check('UpstreamMemberForm (Galera, no role)', () => renderToString(
  <UpstreamMemberForm node={{ id: 'g1', label: 'galera01', exportEnabled: true, exportHostPort: 3307 }} frame={galFrame} patchNode={noop} deleteNode={noop} deployed roles={false} />))

// ---- canvas member descriptions ----
// frameMemberSub used to default to 'Galera data node', so every frame type added
// after PXC inherited it — MariaDB and MySQL replication members were labelled as
// Galera data nodes on the canvas. Assert each type answers for itself, and that
// nothing claims Galera unless it actually runs Galera.
check('every frame type describes its own members', () => {
  const galera = new Set(['pxc', 'mariadbgalera'])
  const expected = {
    pxc: 'Galera data node',
    mariadbgalera: 'Galera data node',
    proxysql: 'ProxySQL',
    mysql: 'Primary',
    mariadbrepl: 'Primary',
    mysqlcerepl: 'Primary',
    innodb: 'Cluster member',
    mysqlceinnodb: 'Cluster member',
    psmrs: 'replica-set member',
    patroni: 'Patroni node',
    repmgr: 'PostgreSQL + repmgr',
    spock: 'PostgreSQL + Spock',
    valkeycluster: 'Valkey shard',
  }
  const out = []
  for (const [type, want] of Object.entries(expected)) {
    const node = { id: 'n1', role: 'primary' }
    const got = frameMemberSub({ type }, node, [node])
    if (got !== want) throw new Error(`${type}: got "${got}", want "${want}"`)
    if (!galera.has(type) && got.toLowerCase().includes('galera')) {
      throw new Error(`${type} is described as Galera but does not run Galera`)
    }
    out.push(`${type}=${got}`)
  }
  // A secondary in a replication frame must read as read-only.
  for (const type of REPL_FRAME_TYPES) {
    const got = frameMemberSub({ type }, { id: 'n2', role: 'secondary' })
    if (!got.includes('read-only')) throw new Error(`${type} secondary: got "${got}"`)
  }
  // An arbitrator wins over the frame's own label.
  if (frameMemberSub({ type: 'pxc' }, { role: 'arbitrator' }) !== 'Arbitrator · garbd') {
    throw new Error('arbitrator description lost')
  }
  return out.join(' ')
})

// ---- the manager, against REAL deployed configs ----
// These payloads were captured from an actual Ubuntu 24.04 deploy of the six new
// node types, so this renders the manager over exactly the shape the backend
// produces — including MariaDB's mariadbConfig, which is a different Go struct from
// mysqlConfig and merely shares its JSON tags.
for (const [nodeId, dep] of Object.entries(realDeps)) {
  check(`MySQLManager over the real ${nodeId} deployment (${dep.config?.serverVersion || '?'})`, () => {
    const html = renderToString(
      <TerminalProvider>
        <MySQLManager stackId={1} nodeId={nodeId} dep={dep} onDeleteNode={noop} />
      </TerminalProvider>,
    )
    // The panel must actually show this node's identity and version, not blanks.
    for (const want of [dep.config.fqdn, dep.config.serverVersion]) {
      if (want && !html.includes(want)) throw new Error(`panel omits ${want}`)
    }
    if (html.includes('undefined')) throw new Error('panel rendered a literal "undefined"')
    return html
  })
}

// The Keycloak-SSO tab is driven entirely by dep.config.oidc, which the Go side writes as
// oidcInfo (pgoidc.go) — so this renders the guide over exactly the field names
// applyMySQLOIDC persists. A renamed field would otherwise show up as a blank instruction.
check('MySQLManager: the Keycloak SSO tab renders the accounts the deploy created', () => {
  const dep = {
    state: 'running', containerId: 'abc123def456',
    config: {
      hostname: 'ps1', fqdn: 'ps1.example.net', role: 'standalone', serverId: 1,
      image: 'dbcanvas-systemd:oraclelinux-9-amd64', psVersion: '8.4.11-11.1', ports: [3306],
      oidc: {
        enabled: true, realm: 'dbcanvas', clientId: 'mysql',
        issuer: 'https://keycloak.example.net:8443/realms/dbcanvas',
        consoleUrl: 'https://keycloak.example.net:8443',
        nodeFqdn: 'ps1.example.net', users: ['jane', 'john'],
        group: 'accounting', role: 'accounting', database: 'oidc_demo',
      },
    },
    secrets: { rootUser: 'root', rootPassword: 'root_password', oidcSamplePassword: 'keycloak_user_password' },
  }
  const html = renderToString(
    <TerminalProvider>
      <MySQLManager stackId={1} nodeId="ps1" dep={dep} onDeleteNode={noop} />
    </TerminalProvider>,
  )
  if (!html.includes('Keycloak SSO')) throw new Error('the SSO tab is missing')
  const guide = renderToString(<OidcLoginGuide engine="ps" info={dep.config.oidc} secrets={dep.secrets} />)
  // The facts have to be on the page, not left to the reader: where Keycloak is, which
  // accounts exist, and the password those accounts actually have.
  for (const want of [
    'oidc-login jane', 'auth_openid_connect', 'ps1.example.net', 'oidc_demo', 'SET ROLE accounting',
    'https://keycloak.example.net:8443', 'jane, john',
    // oidc-login is a wrapper DBCanvas writes, not an upstream tool — say where it is, or it
    // reads as an invented command.
    '/usr/local/bin/oidc-login',
  ]) {
    if (!guide.includes(want)) throw new Error(`login guide omits ${want}`)
  }
  if (guide.includes('undefined')) throw new Error('login guide rendered a literal "undefined"')
  return guide
})

// A node without OIDC must not grow the tab (cfg.oidc is simply absent).
check('MySQLManager: no Keycloak SSO tab without it', () => {
  const dep = { state: 'running', config: { hostname: 'ps2', fqdn: 'ps2.example.net', role: 'standalone' }, secrets: {} }
  const html = renderToString(
    <TerminalProvider>
      <MySQLManager stackId={1} nodeId="ps2" dep={dep} onDeleteNode={noop} />
    </TerminalProvider>,
  )
  if (html.includes('Keycloak SSO')) throw new Error('the SSO tab showed on a node without OIDC')
  return 'hidden'
})

// --- Packet Inspector -------------------------------------------------------
// The page itself renders only its empty state under SSR (no effects, no fetch),
// so each data-bearing component is rendered over a fixture shaped exactly like
// the decoder's JSON — the packet list, the detail panel, the timeline strip and
// the range controls all read fields the Go side must keep producing.

const pktFixtureSummary = {
  packets: 13227, streams: 41, bytes: 8612851, firstTs: 1785775360.0, lastTs: 1785775372.6,
  protos: { MySQL: 4304, TCP: 1507, TLS: 7416 }, queries: 822, errors: 9, tlsStreams: 24,
  dropped: 0, truncated: 0, format: 'pcap', linkType: 1,
  issueTop: [
    { kind: 'MySQL error 1064', count: 6 },
    { kind: 'High latency', count: 4 },
    { kind: 'TCP zero window', count: 7 },
  ],
}
const pktFixtureCap = {
  id: 'abc123', label: 'mysql-1', stackName: 'packet-inspector-dev', state: 'ready',
  iface: 'eth0', port: 3306, source: 'node', bytes: 8612851, nodePackets: 13227,
  kernelDropped: 0, command: 'tcpdump -i eth0 -s 65535 -n -q -c 60000 port 3306 -w /var/tmp/x.cap',
  ports: { 3306: 'mysql' }, nodeType: 'mysql', summary: pktFixtureSummary,
}
const pktFixturePackets = [
  {
    no: 1221, ts: 1785775365.5, stream: 8, dir: 'c2s', src: '172.29.0.3:41236', dst: '172.29.0.4:3306',
    proto: 'MySQL', info: "Query: INSERT INTO t (v) VALUES ('light-1')", frameLen: 128, payloadLen: 62,
    flags: 'ACK,PSH', seq: 12, ack: 34, window: 502, command: 'COM_QUERY',
    query: "INSERT INTO t (v) VALUES ('light-1')",
  },
  {
    no: 1228, ts: 1785775365.6, stream: 8, dir: 's2c', src: '172.29.0.4:3306', dst: '172.29.0.3:41236',
    proto: 'MySQL', info: 'OK: 1 row(s) affected, insert_id 4', frameLen: 78, payloadLen: 11,
    status: 'Success', rows: 1, lagMs: 1.42, command: 'COM_QUERY',
  },
  {
    no: 1302, ts: 1785775366.1, stream: 8, dir: 's2c', src: '172.29.0.4:3306', dst: '172.29.0.3:41236',
    proto: 'MySQL', info: "Error 1054: Unknown column 'bogus' in 'field list'", frameLen: 120, payloadLen: 53,
    status: "Error 1054 (42S22): Unknown column 'bogus' in 'field list'", errCode: 1054, lagMs: 0.9,
    issues: ["MySQL error 1054: Unknown column 'bogus' in 'field list'"],
  },
  {
    no: 887, ts: 1785775364.2, stream: 1, dir: 'c2s', src: '172.29.0.3:36818', dst: '172.29.0.4:3306',
    proto: 'TCP', info: '[ACK] seq=1986407221 ack=3176508254 win=0', frameLen: 66, payloadLen: 0,
    flags: 'ACK', window: 0, issues: ['TCP zero window — receiver buffer full'],
  },
  {
    no: 4001, ts: 1785775367.0, stream: 11, dir: 'c2s', src: '172.29.0.3:43952', dst: '172.29.0.4:3306',
    proto: 'TLS', info: 'TLS 1.3 Application Data (283 bytes)', frameLen: 349, payloadLen: 288,
    status: 'Encrypted',
  },
]
const pktFixtureTimeline = {
  fromTs: 1785775360.0, toTs: 1785775372.6, fromNo: 1, toNo: 13227, total: 13227,
  buckets: [
    { ts: 1785775360.0, firstNo: 1, lastNo: 312, count: 312, bytes: 40000, warnings: 0, errors: 0, queries: 184 },
    { ts: 1785775361.0, firstNo: 313, lastNo: 432, count: 120, bytes: 12000, warnings: 1, errors: 0, queries: 72 },
    { ts: 1785775362.0, firstNo: 433, lastNo: 1153, count: 721, bytes: 90000, warnings: 0, errors: 2, queries: 262 },
    { ts: 1785775363.0, firstNo: 0, lastNo: 0, count: 0, bytes: 0, warnings: 0, errors: 0, queries: 0 },
  ],
  kinds: pktFixtureSummary.issueTop,
  streams: [
    { index: 8, client: '172.29.0.3:41236', server: '172.29.0.4:3306', label: '#8 client (admin)' },
    { index: 11, client: '172.29.0.3:43952', server: '172.29.0.4:3306', label: '#11 client TLS' },
  ],
}
const pktRange = { fromNo: '', toNo: '', fromTs: '', toTs: '', stream: -1, proto: '', dir: '', issue: '', q: '' }

check('PacketInspector page (empty state)', () => renderToString(<PacketInspector />))
check('packet inspector: capture state', () => renderToString(<PktState cap={pktFixtureCap} />))
check('packet inspector: summary strip', () =>
  renderToString(<PktSummary cap={pktFixtureCap} range={pktRange} setRange={noop} />))
check('packet inspector: timeline strip', () =>
  renderToString(<PktTimeline timeline={pktFixtureTimeline} first={pktFixtureSummary.firstTs} onSelect={noop} />))
check('packet inspector: timeline while loading', () =>
  renderToString(<PktTimeline timeline={null} first={0} onSelect={noop} />))
check('packet inspector: range controls', () =>
  renderToString(<PktRangeControls range={pktRange} setRange={noop} buckets={160} setBuckets={noop}
    summary={pktFixtureSummary} timeline={pktFixtureTimeline} span={12.6} />))
check('packet inspector: filters', () =>
  renderToString(<PktFilters range={pktRange} setRange={noop} summary={pktFixtureSummary}
    streams={pktFixtureTimeline.streams} />))
check('packet inspector: packet list', () => {
  const html = renderToString(<PktList packets={pktFixturePackets} first={pktFixtureSummary.firstTs}
    selectedNo={1228} onSelect={noop} />)
  // The row must show the decoded MySQL, the peers, and the issue text.
  // The list shows proto / info / issues — a TLS row is recognisable by both.
  for (const want of ['light-1', '172.29.0.4:3306', 'zero window', 'TLS', 'Application Data']) {
    if (!html.includes(want)) throw new Error(`packet list omits ${want}`)
  }
  if (html.includes('undefined')) throw new Error('packet list rendered a literal "undefined"')
  return html
})
check('packet inspector: packet list (empty range)', () =>
  renderToString(<PktList packets={[]} first={0} selectedNo={null} onSelect={noop} />))
// Every time-display mode has to render, and the absolute ones must actually show a
// date/time rather than the relative offset they replaced.
for (const mode of ['relative', 'clock', 'datetime', 'utc', 'delta']) {
  check(`packet inspector: packet list time mode ${mode}`, () => {
    const html = renderToString(<PktList packets={pktFixturePackets} first={pktFixtureSummary.firstTs}
      selectedNo={null} onSelect={noop} timeMode={mode} />)
    if (mode === 'datetime' && !/\d{4}-\d{2}-\d{2} /.test(html)) throw new Error('no date rendered')
    if (mode === 'utc' && !html.includes('Z')) throw new Error('no UTC timestamp rendered')
    if (mode === 'relative' && !/\+\d+\.\d{6}/.test(html)) throw new Error('no relative offset rendered')
    return html
  })
}
check('packet inspector: pager', () =>
  renderToString(<PktPager page={{ matched: 13227, offset: 400, limit: 200 }} onPage={noop} />))
for (const p of pktFixturePackets) {
  check(`packet inspector: details for #${p.no} (${p.proto})`, () => {
    const html = renderToString(<PktDetails
      d={{ packet: p, stream: { index: p.stream, version: '8.0.46-37', user: 'admin', tls: p.proto === 'TLS' }, hex: '0000  16 03 03  |...|', bytes: p.frameLen }}
      first={pktFixtureSummary.firstTs} />)
    if (html.includes('undefined')) throw new Error('details rendered a literal "undefined"')
    return html
  })
}

// The server-error-log panel: the events a capture cannot contain.
const pktLogFixture = {
  path: '/var/log/mysqld.log', scanned: 209, windowFrom: 1785775360, windowTo: 1785775372,
  stats: {
    verbosity: 3, suppressionList: '',
    counters: { Aborted_clients: '15', Aborted_connects: '9', Connection_errors_max_connections: '193' },
    hint: 'Aborted_clients is 15 — the server has counted that many clients disappearing without a clean QUIT.',
  },
  top: [{ label: 'Aborted connection', count: 4 }, { label: 'Too many connections', count: 63 }],
  entries: [
    {
      ts: 1785775361, time: '2026-08-03T19:19:01.501234Z', level: 'Note', code: 'MY-010914',
      subsystem: 'Server', class: 'aborted', label: 'Aborted connection',
      reason: 'Got an error reading communication packets',
      message: "Aborted connection 12 to db: 'pi_demo' user: 'app' host: 'mysql-2.example.net' (Got an error reading communication packets).",
      inWindow: true,
    },
    {
      ts: 1785775300, time: '2026-08-03T16:27:09.236842Z', level: 'Warning', code: 'MY-010055',
      subsystem: 'Server', class: 'dns', label: 'Client IP could not be resolved', reason: '',
      message: "IP address '172.29.0.5' could not be resolved: Name or service not known", inWindow: false,
    },
    {
      ts: 1785775362, time: '2026-08-03T19:19:02.000000Z', level: 'ERROR', code: 'MY-010262',
      subsystem: 'Server', class: 'listener', label: 'TCP listener problem', reason: '',
      message: "Can't start server: Bind on TCP/IP port: Address already in use", inWindow: true,
    },
  ],
}
// The upload control has to read as clickable: a bare file input renders as browser chrome
// that looks like static text, which is what it was before.
// The nav icon is its own component; render it at the sizes the sidebar uses so a broken
// path or a missing element is caught rather than shipped as a smudge.
for (const size of [16, 18, 24]) {
  check(`packet inspector: nav icon at ${size}px`, () => {
    const html = renderToString(<Icon.Packet size={size} />)
    if (!html.includes(`width="${size}"`)) throw new Error('size not applied')
    if (!html.includes('viewBox="0 0 24 24"')) throw new Error('wrong viewBox for the icon set')
    if (!html.includes('stroke="currentColor"')) throw new Error('icon must follow the theme colour')
    // Three list rows plus a lens (circle + handle) — five elements, no fill.
    const lines = (html.match(/<line /g) || []).length
    if (lines !== 4) throw new Error(`expected 4 lines (3 rows + handle), got ${lines}`)
    if (!html.includes('<circle')) throw new Error('the lens is missing')
    return html
  })
}

check('packet inspector: file picker (empty)', () => {
  const html = renderToString(<PktFilePick id="f1" accept=".pcap" file={null} onPick={noop}
    placeholder="Choose a capture, or drop it here" />)
  for (const want of ['Choose a capture, or drop it here', 'cursor-pointer', 'border-dashed', 'for="f1"']) {
    if (!html.includes(want)) throw new Error(`picker omits ${want}`)
  }
  // The native input must still be present and reachable, just not visible.
  if (!html.includes('type="file"') || !html.includes('sr-only')) {
    throw new Error('the native input must remain, hidden but focusable')
  }
  return html
})
check('packet inspector: file picker (file chosen)', () => {
  const html = renderToString(<PktFilePick id="f2" accept=".pcap"
    file={{ name: 'pxc01-tcpdump.pcap', size: 18687067 }} onPick={noop} placeholder="unused" />)
  if (!html.includes('pxc01-tcpdump.pcap')) throw new Error('the chosen file is not named')
  if (!html.includes('17.8 MB')) throw new Error('the size is not shown')
  if (!html.includes('remove')) throw new Error('no way to clear the choice')
  return html
})

check('packet inspector: server error log', () => {
  const html = renderToString(<PktServerLog log={pktLogFixture} onReload={noop} />)
  for (const want of ['Aborted connection', 'Got an error reading communication packets',
    'Aborted_clients', 'MY-010055', '/var/log/mysqld.log']) {
    if (!html.includes(want)) throw new Error(`server log panel omits ${want}`)
  }
  return html
})
check('packet inspector: server error log (nothing in window)', () =>
  renderToString(<PktServerLog log={{ path: '/var/log/mysqld.log', source: 'node', scanned: 12, entries: [], top: [], stats: {} }} onReload={noop} />))
// An uploaded capture with no log at all, and one whose log does not overlap the capture —
// the second is the mistake an upload pair actually makes.
check('packet inspector: server error log (none uploaded)', () => {
  const html = renderToString(<PktServerLog onReload={noop}
    log={{ path: '', source: 'upload', scanned: 0, entries: [], top: [],
      note: 'no server log was uploaded with this capture — upload one alongside the pcap to correlate' }} />)
  if (!html.includes('no server log was uploaded')) throw new Error('note not shown')
  return html
})
check('packet inspector: server error log (follows the selected packet)', () => {
  const html = renderToString(<PktServerLog log={pktLogFixture} onReload={noop}
    selectedTs={1785775361.2} selectedNo={1221} />)
  if (!html.includes('nearest')) throw new Error('the nearest record is not marked')
  // renderToString splits adjacent text nodes with comment markers, so the frame number
  // is not contiguous with the label in the HTML — assert on each part.
  if (!html.includes('Nearest record to frame')) throw new Error('no delta line')
  if (!html.includes('1221')) throw new Error('delta line omits the frame number')
  // The sign and the number are separate text nodes too, hence the loose match.
  if (!/[+−](<!-- -->)?\d+\.\d{3}/.test(html)) throw new Error('delta value not rendered')
  if (!html.includes('ring-primary')) throw new Error('the nearest record is not highlighted')
  return html
})
check('packet inspector: server error log (selection with nothing nearby)', () => {
  const html = renderToString(<PktServerLog log={pktLogFixture} onReload={noop}
    selectedTs={1785999999} selectedNo={99} />)
  if (!html.includes('nothing in the log is close to this packet')) {
    throw new Error('a far-away selection should say so')
  }
  return html
})
// Clicking a record is the reverse jump; the rows must advertise it and be clickable.
check('packet inspector: server error log (records are clickable)', () => {
  let picked = null
  const html = renderToString(<PktServerLog log={pktLogFixture} onReload={noop}
    onPick={(ts) => { picked = ts }} />)
  if (!html.includes('Click a record to send the packet list to that moment')) {
    throw new Error('the jump affordance is not shown')
  }
  if (!html.includes('cursor-pointer')) throw new Error('records are not clickable')
  return html
})
// …and the packet list tints the neighbourhood of the record that was clicked.
check('packet inspector: packet list marks a log record\'s moment', () => {
  const html = renderToString(<PktList packets={pktFixturePackets} first={pktFixtureSummary.firstTs}
    selectedNo={null} onSelect={noop} markTs={1785775365.5} />)
  if (!html.includes('bg-warning/10')) throw new Error('no rows tinted for the marked moment')
  return html
})
// ---- PostgreSQL. Same components, a different protocol: the point of the checks below
// is that nothing in the UI is MySQL-only, and that a PostgreSQL capture's own
// vocabulary (SQLSTATE, Patroni, etcd, WAL) actually reaches the screen.
const pgFixtureSummary = {
  packets: 22578, streams: 67, bytes: 14012044, firstTs: 1785824100.0, lastTs: 1785824120.0,
  protos: { PostgreSQL: 18252, TCP: 3656, 'etcd/raft': 596, 'Patroni/REST': 55, 'etcd/client': 19 },
  issueTop: [
    { kind: 'TCP reset', count: 23 },
    { kind: 'Replication lag', count: 2 },
    { kind: 'Deadlock detected', count: 1 },
    { kind: 'A write was attempted on a read-only connection', count: 1 },
  ],
  queries: 4684, errors: 3, tlsStreams: 1, dropped: 0, truncated: 0, format: 'pcap', linkType: 1,
}
const pgFixtureCap = {
  id: 'pg1', label: 'patroni01', stackName: 'pktinspect-pg', state: 'ready', engine: 'postgres',
  iface: 'eth0', port: 5432, source: 'node', bytes: 14012044, nodePackets: 22578, kernelDropped: 0,
  command: 'tcpdump -i eth0 -s 65535 -n -q -c 50000 (port 2379 or port 2380 or port 5432 or port 8008) -w /var/tmp/x.cap',
  ports: { 5432: 'postgres', 8008: 'patroni-rest', 2379: 'etcd-client', 2380: 'etcd-peer' },
  nodeType: 'patroni', summary: pgFixtureSummary,
}
const pgFixturePackets = [
  {
    no: 14, ts: 1785824100.2, stream: 3, dir: 'c2s', src: '172.29.0.6:35570', dst: '172.29.0.4:5432',
    proto: 'PostgreSQL', frameLen: 214, payloadLen: 148, flags: 'ACK,PSH', command: 'Execute',
    info: 'Bind "stmtcache_407f32e38a17…" → portal unnamed portal, 4 parameter(s), largest 10 B | Execute portal unnamed',
    query: 'UPDATE cars SET mileage = mileage + $1 WHERE id = $2',
  },
  {
    no: 61, ts: 1785824101.9, stream: 3, dir: 's2c', src: '172.29.0.4:5432', dst: '172.29.0.6:35570',
    proto: 'PostgreSQL', frameLen: 190, payloadLen: 124, flags: 'ACK,PSH',
    info: 'ERROR 40P01: deadlock detected | detail: Process 5539 waits for ShareLock…',
    status: 'Error 40P01 (deadlock_detected): deadlock detected', errState: '40P01', lagMs: 1204.5,
    issues: ['Deadlock detected — two transactions each held what the other needed'],
  },
  {
    no: 88, ts: 1785824102.4, stream: 9, dir: 's2c', src: '172.29.0.4:5432', dst: '172.29.0.7:44634',
    proto: 'PostgreSQL', frameLen: 1520, payloadLen: 1454, flags: 'ACK,PSH',
    info: 'XLogData: 1.4 KB WAL at 0/25211F38',
  },
  {
    no: 91, ts: 1785824102.5, stream: 9, dir: 'c2s', src: '172.29.0.7:44634', dst: '172.29.0.4:5432',
    proto: 'PostgreSQL', frameLen: 105, payloadLen: 39, flags: 'ACK,PSH',
    info: 'Standby status: write 0/25211F38, flush 0/25211E10, apply 0/25211E10, 24.0 MB behind',
    issues: ['Replication lag 24.0 MB — the standby has flushed 0/25211E10'],
  },
  {
    no: 234, ts: 1785824103.1, stream: 21, dir: 'c2s', src: '172.29.0.8:34420', dst: '172.29.0.4:8008',
    proto: 'Patroni/REST', frameLen: 140, payloadLen: 74, flags: 'ACK,PSH', command: 'GET /primary',
    info: 'GET /primary — "am I the leader?" — 200 yes, 503 no; this is HAProxy\'s write-port health check',
  },
  {
    no: 1291, ts: 1785824105.6, stream: 30, dir: 'c2s', src: '172.29.0.4:60824', dst: '172.29.0.4:2379',
    proto: 'etcd/client', frameLen: 160, payloadLen: 94, flags: 'ACK,PSH', command: 'POST /v3/lease/keepalive',
    info: 'POST /v3/lease/keepalive — a lease — the TTL behind the leader lock',
  },
]

check('packet inspector: PostgreSQL capture state (engine badge)', () => {
  const html = renderToString(<PktState cap={pgFixtureCap} />)
  if (!html.includes('PostgreSQL')) throw new Error('the decoded protocol is not shown')
  return html
})
check('packet inspector: PostgreSQL summary strip', () => {
  const html = renderToString(<PktSummary cap={pgFixtureCap} range={pktRange} setRange={noop} />)
  // The protocol mix and the issue kinds both belong here, and the errors stat must be
  // labelled for the engine that produced them rather than "MySQL errors".
  for (const want of ['Patroni/REST', 'etcd/raft', 'Replication lag', 'PostgreSQL errors']) {
    if (!html.includes(want)) throw new Error(`summary omits ${want}`)
  }
  if (html.includes('MySQL')) throw new Error('a PostgreSQL summary mentions MySQL')
  return html
})
check('packet inspector: TLS advice follows the engine', () => {
  const pg = renderToString(<PktSummary cap={pgFixtureCap} range={pktRange} setRange={noop} />)
  if (!pg.includes('sslmode=prefer') || !pg.includes('pg_stat_statements')) {
    throw new Error('PostgreSQL TLS advice missing')
  }
  if (pg.includes('caching_sha2_password')) throw new Error('MySQL TLS advice shown for PostgreSQL')
  const my = renderToString(<PktSummary cap={{ ...pktFixtureCap, engine: 'mysql',
    summary: { ...pktFixtureSummary, tlsStreams: 2 } }} range={pktRange} setRange={noop} />)
  if (!my.includes('caching_sha2_password')) throw new Error('MySQL TLS advice missing')
  if (my.includes('sslmode=prefer')) throw new Error('PostgreSQL TLS advice shown for MySQL')
  return pg + my
})
check('packet inspector: PostgreSQL packet list', () => {
  const html = renderToString(<PktList packets={pgFixturePackets} first={pgFixtureSummary.firstTs}
    selectedNo={61} onSelect={noop} />)
  // A PostgreSQL row has to show its own vocabulary: SQLSTATE, WAL positions, the
  // cluster's own protocols.
  for (const want of ['40P01', 'XLogData', 'Standby status', 'Patroni/REST', 'etcd/client', '24.0 MB behind']) {
    if (!html.includes(want)) throw new Error(`packet list omits ${want}`)
  }
  if (html.includes('undefined')) throw new Error('packet list rendered a literal "undefined"')
  return html
})
for (const p of pgFixturePackets) {
  check(`packet inspector: PostgreSQL details for #${p.no} (${p.proto})`, () => {
    const html = renderToString(<PktDetails
      d={{ packet: p, stream: { index: p.stream, version: '16.14', user: 'carsim', database: 'rental',
        role: 'postgres', roleLabel: p.proto }, hex: '0000  51 00 00 00  |Q...|', bytes: p.frameLen }}
      first={pgFixtureSummary.firstTs} />)
    if (html.includes('undefined')) throw new Error('details rendered a literal "undefined"')
    // A SQLSTATE is a string and has its own row; it must not be lost with MySQL's
    // numeric error code.
    if (p.errState && !html.includes(p.errState)) throw new Error('SQLSTATE not shown in details')
    return html
  })
}
check('packet inspector: PostgreSQL cluster ports explained', () => {
  const html = renderToString(<PacketInspector />)
  // The page renders in its empty state here; the port-role text itself is what the
  // capture card uses, so check the table that feeds it instead of the mounted card.
  if (!PORT_ROLE_TEXT['patroni-rest'].includes('REST')) throw new Error('no Patroni port role text')
  if (!PORT_ROLE_TEXT['etcd-client'].includes('leader lock')) throw new Error('no etcd port role text')
  if (!PORT_ROLE_TEXT.postgres.includes('WAL')) throw new Error('no PostgreSQL port role text')
  return html
})
check('packet inspector: PostgreSQL issues are severe, ordinary SQL errors are not', () => {
  const severe = ['Deadlock detected', 'Replication lag 24.0 MB', 'FATAL — the server closes',
    'A write was attempted on a read-only connection', 'Password authentication failed']
  for (const s of severe) {
    if (!isSevereIssue(s)) throw new Error(`${s} should be severe`)
  }
  // A unique violation from an application that expects them must not paint the
  // timeline red.
  for (const s of ['ERROR 23505: duplicate key value violates unique constraint',
    'syntax error at or near "form"']) {
    if (isSevereIssue(s)) throw new Error(`${s} should not be severe`)
  }
  return 'ok'
})
check('packet inspector: PostgreSQL server log', () => {
  const html = renderToString(<PktServerLog onReload={noop} log={{
    path: '/var/lib/pgsql/16/data/log/postgresql-Tue.log', source: 'node', scanned: 397, inWindow: 2,
    windowFrom: 1785824070, windowTo: 1785824150,
    top: [{ label: 'Password authentication failed', count: 1 }, { label: 'Database does not exist', count: 1 }],
    entries: [
      { ts: 1785824105.049, time: '2026-08-04 06:48:25.049 UTC', level: 'FATAL', class: 'auth',
        label: 'Password authentication failed', inWindow: true,
        message: 'password authentication failed for user "postgres" | DETAIL: Connection matched pg_hba.conf line 8' },
      { ts: 1785824105.057, time: '2026-08-04 06:48:25.057 UTC', level: 'FATAL', class: 'auth',
        label: 'Database does not exist', inWindow: true, message: 'database "nosuch" does not exist' },
    ],
    stats: { verbosity: 0, suppressionList: '', counters: {},
      hint: 'PostgreSQL logs a dropped or refused connection unconditionally — there is no verbosity setting to check.' },
  }} />)
  for (const want of ['postgresql-Tue.log', 'Password authentication failed', 'unconditionally']) {
    if (!html.includes(want)) throw new Error(`PostgreSQL server log omits ${want}`)
  }
  return html
})

// ---- MongoDB. One port, many kinds of conversation: the checks below are that the kind
// reaches the screen, that MongoDB's own vocabulary does, and that nothing is MySQL-only.
const mgFixtureSummary = {
  packets: 80157, streams: 97, bytes: 41022044, firstTs: 1785830100.0, lastTs: 1785830220.0,
  protos: {
    'MongoDB/replpos': 34008, 'MongoDB/oplog': 19726, TCP: 15850, MongoDB: 9100,
    'MongoDB/heartbeat': 998, 'MongoDB/monitor': 472, 'MongoDB/election': 3,
  },
  issueTop: [
    { kind: 'TCP duplicate ACK', count: 113 },
    { kind: 'Unauthorized (13)', count: 4 },
    { kind: 'DuplicateKey (11000)', count: 3 },
    { kind: 'Election in progress', count: 1 },
  ],
  queries: 1251, errors: 37, tlsStreams: 0, dropped: 0, truncated: 0, format: 'pcap', linkType: 1,
}
const mgFixtureCap = {
  id: 'mg1', label: 'psmrs01', stackName: 'pktinspect-mongo', state: 'ready', engine: 'mongodb',
  iface: 'eth0', port: 27017, source: 'node', bytes: 41022044, nodePackets: 80157, kernelDropped: 0,
  command: 'tcpdump -i eth0 -s 65535 -n -q -c 100000 port 27017 -w /var/tmp/x.cap',
  ports: { 27017: 'mongodb' }, nodeType: 'psmrs', summary: mgFixtureSummary,
}
const mgFixturePackets = [
  {
    no: 19, ts: 1785830100.4, stream: 3, dir: 'c2s', src: '172.30.0.5:45001', dst: '172.30.0.4:27017',
    proto: 'MongoDB/replpos', frameLen: 640, payloadLen: 574, flags: 'ACK,PSH',
    command: 'replSetUpdatePosition',
    info: '[snappy] replSetUpdatePosition admin — optimes: [{…8 fields}, {…8 fields}, {…8 fields}]',
  },
  {
    no: 8742, ts: 1785830104.1, stream: 9, dir: 'c2s', src: '172.30.0.6:45002', dst: '172.30.0.4:27017',
    proto: 'MongoDB/oplog', frameLen: 320, payloadLen: 254, flags: 'ACK,PSH', command: 'find',
    query: 'local.oplog.rs',
    info: '[snappy] find local.oplog.rs — filter {ts: {…1 fields}}, batch 13981010, tailable',
  },
  {
    no: 11965, ts: 1785830108.9, stream: 12, dir: 's2c', src: '172.30.0.4:27017', dst: '172.30.0.9:49636',
    proto: 'MongoDB', frameLen: 410, payloadLen: 344, flags: 'ACK,PSH', lagMs: 2.4, errCode: 11000,
    status: 'Write error 11000: E11000 duplicate key error collection: hotelsim.bookings index: _id_',
    info: 'insert → 1 write error(s), first 11000 DuplicateKey: E11000 duplicate key error',
    issues: ['DuplicateKey (11000) — A unique index rejected the document'],
  },
  {
    no: 12001, ts: 1785830109.2, stream: 14, dir: 'c2s', src: '172.30.0.5:45004', dst: '172.30.0.4:27017',
    proto: 'MongoDB/election', frameLen: 260, payloadLen: 194, flags: 'ACK,PSH',
    command: 'replSetRequestVotes',
    info: 'replSetRequestVotes admin — term: 2, setName: "psmrs-00"',
    issues: ['Election in progress — replSetRequestVotes: a member is standing for primary'],
  },
  {
    no: 12100, ts: 1785830110.0, stream: 16, dir: 'c2s', src: '172.30.0.7:45010', dst: '172.30.0.4:27017',
    proto: 'MongoDB/routed', frameLen: 300, payloadLen: 234, flags: 'ACK,PSH', command: 'find',
    query: 'shlab.orders',
    info: 'find shlab.orders — filter {sk: 42} [shardVersion Timestamp(1785830373, 4)]',
  },
]

check('packet inspector: MongoDB capture state (engine badge)', () => {
  const html = renderToString(<PktState cap={mgFixtureCap} />)
  if (!html.includes('MongoDB')) throw new Error('the decoded protocol is not shown')
  return html
})
check('packet inspector: MongoDB summary strip', () => {
  const html = renderToString(<PktSummary cap={mgFixtureCap} range={pktRange} setRange={noop} />)
  for (const want of ['MongoDB/oplog', 'MongoDB/heartbeat', 'MongoDB errors', 'Election in progress']) {
    if (!html.includes(want)) throw new Error(`summary omits ${want}`)
  }
  if (html.includes('MySQL') || html.includes('PostgreSQL')) {
    throw new Error('a MongoDB summary mentions another engine')
  }
  return html
})
check('packet inspector: MongoDB packet list', () => {
  const html = renderToString(<PktList packets={mgFixturePackets} first={mgFixtureSummary.firstTs}
    selectedNo={11965} onSelect={noop} />)
  for (const want of ['MongoDB/replpos', 'MongoDB/oplog', 'MongoDB/election', 'MongoDB/routed',
    'local.oplog.rs', 'DuplicateKey', 'shardVersion', 'snappy']) {
    if (!html.includes(want)) throw new Error(`packet list omits ${want}`)
  }
  if (html.includes('undefined')) throw new Error('packet list rendered a literal "undefined"')
  return html
})
for (const p of mgFixturePackets) {
  check(`packet inspector: MongoDB details for #${p.no} (${p.proto})`, () => {
    const html = renderToString(<PktDetails
      d={{ packet: p, stream: { index: p.stream, user: 'hotelsim', database: 'hotelsim',
        role: p.proto === 'MongoDB' ? 'client' : p.proto.slice(8), roleLabel: p.proto },
        hex: '0000  e6 00 00 00  |....|', bytes: p.frameLen }}
      first={mgFixtureSummary.firstTs} />)
    if (html.includes('undefined')) throw new Error('details rendered a literal "undefined"')
    return html
  })
}
check('packet inspector: MongoDB connection kinds are explained', () => {
  for (const [kind, needle] of [['heartbeat', '2 seconds'], ['oplog', 'local.oplog.rs'],
    ['routed', 'shard version'], ['replpos', 'write concern'], ['election', 'primary changes']]) {
    if (!MONGO_KIND_TEXT[kind] || !MONGO_KIND_TEXT[kind].includes(needle)) {
      throw new Error(`MONGO_KIND_TEXT.${kind} does not explain ${needle}`)
    }
  }
  return 'ok'
})
check('packet inspector: MongoDB issues are severe, ordinary ones are not', () => {
  for (const s of ['NotWritablePrimary (10107) — This member is not the primary',
    'Election in progress — replSetRequestVotes', 'StaleConfig (13388) — The shard refused',
    'WriteConcernFailed (64)', 'CursorNotFound (43)', 'Chunk migration (moveChunk)']) {
    if (!isSevereIssue(s)) throw new Error(`${s} should be severe`)
  }
  // A unique index doing its job and a driver probing for optional commands are not faults.
  for (const s of ['DuplicateKey (11000) — A unique index rejected the document',
    'CommandNotFound (59)', 'NamespaceNotFound (26)']) {
    if (isSevereIssue(s)) throw new Error(`${s} should not be severe`)
  }
  return 'ok'
})
check('packet inspector: MongoDB TLS advice', () => {
  const html = renderToString(<PktSummary cap={{ ...mgFixtureCap,
    summary: { ...mgFixtureSummary, tlsStreams: 3 } }} range={pktRange} setRange={noop} />)
  if (!html.includes('no in-band upgrade') || !html.includes('system.profile')) {
    throw new Error('MongoDB TLS advice missing')
  }
  if (html.includes('sslmode=prefer') || html.includes('caching_sha2_password')) {
    throw new Error('another engine\'s TLS advice shown for MongoDB')
  }
  return html
})
check('packet inspector: MongoDB server log', () => {
  const html = renderToString(<PktServerLog onReload={noop} log={{
    path: '/var/log/mongo/mongod.log', source: 'node', scanned: 412, inWindow: 3,
    windowFrom: 1785830070, windowTo: 1785830250,
    top: [{ label: 'Slow query', count: 2 }, { label: 'Election succeeded — this member is now primary', count: 1 }],
    entries: [
      { ts: 1785830106.958, time: '2026-08-04T07:39:06.958+00:00', level: 'INFO', class: 'other',
        label: 'Slow query', code: '51803', subsystem: 'WRITE', inWindow: true,
        message: 'Slow query | ns=hotelsim.dailyInventory planSummary=IXSCAN docsExamined=5600 durationMillis=151' },
      { ts: 1785830110.0, time: '2026-08-04T07:40:00.000+00:00', level: 'INFO', class: 'cluster',
        label: 'Election succeeded — this member is now primary', code: '20698', subsystem: 'ELECTION',
        inWindow: true, message: 'Election succeeded, assuming primary role | term=2' },
    ],
    stats: { verbosity: 0, suppressionList: '', counters: {},
      hint: 'MongoDB logs every connection accepted and ended (ids 22943 and 22944) at its default verbosity.' },
  }} />)
  for (const want of ['mongod.log', 'planSummary=IXSCAN', 'Election succeeded', '22943']) {
    if (!html.includes(want)) throw new Error(`MongoDB server log omits ${want}`)
  }
  return html
})

// ---- Valkey. Two protocols on two ports, and a client port that also carries
// replication — so the checks are that the kind reaches the screen and that RESP's own
// vocabulary (MOVED, FULLRESYNC, the cluster bus) does.
const vkFixtureSummary = {
  packets: 897, streams: 40, bytes: 214012, firstTs: 1785840100.0, lastTs: 1785840160.0,
  protos: { TCP: 529, Valkey: 221, 'Valkey/replication': 144, 'Valkey/bus': 78, 'Valkey/pubsub': 3 },
  issueTop: [
    { kind: 'AUTH on an unencrypted connection', count: 35 },
    { kind: 'MOVED', count: 4 },
    { kind: 'KEYS *', count: 1 },
    { kind: 'FULLRESYNC', count: 1 },
  ],
  queries: 105, errors: 13, tlsStreams: 0, dropped: 0, truncated: 0, format: 'pcap', linkType: 1,
}
const vkFixtureCap = {
  id: 'vk1', label: 'valkey01', stackName: 'pktinspect-valkey', state: 'ready', engine: 'valkey',
  iface: 'eth0', port: 6379, source: 'node', bytes: 214012, nodePackets: 897, kernelDropped: 0,
  command: "tcpdump -i eth0 -s 65535 -n -q -c 40000 '(port 6379 or port 16379 or port 26379)' -w /var/tmp/x.cap",
  ports: { 6379: 'valkey', 16379: 'valkey-bus', 26379: 'valkey-sentinel' },
  nodeType: 'valkeycluster', summary: vkFixtureSummary,
}
const vkFixturePackets = [
  {
    no: 116, ts: 1785840100.3, stream: 2, dir: 'c2s', src: '172.31.0.6:44100', dst: '172.31.0.7:6379',
    proto: 'Valkey', frameLen: 120, payloadLen: 54, flags: 'ACK,PSH', command: 'SET',
    query: 'SET session:abc', info: 'SET session:abc ← user=1000;cart=3 (16 bytes) [EX 1800]',
  },
  {
    no: 117, ts: 1785840100.31, stream: 2, dir: 's2c', src: '172.31.0.7:6379', dst: '172.31.0.6:44100',
    proto: 'Valkey', frameLen: 100, payloadLen: 34, flags: 'ACK,PSH', lagMs: 1.2, errState: 'MOVED',
    status: 'Error MOVED: 12182 172.31.0.5:6379',
    info: 'SET → -MOVED 12182 172.31.0.5:6379',
    issues: ['MOVED → slot 12182 is on 172.31.0.5:6379. MOVED — the slot this key belongs to is served by another node'],
  },
  {
    no: 24, ts: 1785840101.0, stream: 5, dir: 's2c', src: '172.31.0.7:6379', dst: '172.31.0.4:44210',
    proto: 'Valkey/replication', frameLen: 126, payloadLen: 60, flags: 'ACK,PSH',
    info: '+FULLRESYNC replid 31b51a3dbeef7ab0… offset 22238 — a full dataset transfer follows',
    issues: ['FULLRESYNC — the primary is about to send its ENTIRE dataset as an RDB snapshot'],
  },
  {
    no: 28, ts: 1785840101.4, stream: 5, dir: 's2c', src: '172.31.0.7:6379', dst: '172.31.0.4:44210',
    proto: 'Valkey/replication', frameLen: 7306, payloadLen: 7240, flags: 'ACK,PSH',
    info: 'RDB payload (diskless), 14.1 KB so far',
  },
  {
    no: 158, ts: 1785840102.0, stream: 5, dir: 's2c', src: '172.31.0.7:6379', dst: '172.31.0.4:44210',
    proto: 'Valkey/replication', frameLen: 104, payloadLen: 38, flags: 'ACK,PSH',
    info: 'propagated: SET prop3:1 ← v1 (2 bytes)',
  },
  {
    no: 1, ts: 1785840100.0, stream: 0, dir: 'c2s', src: '172.31.0.6:52000', dst: '172.31.0.7:16379',
    proto: 'Valkey/bus', frameLen: 2322, payloadLen: 2256, flags: 'ACK,PSH', command: 'bus PING',
    info: 'PING from 00089dc7c673…, claims 5461 slot(s), epoch 3/1, offset 0, 1 gossip section(s)',
  },
]

check('packet inspector: Valkey capture state (engine badge)', () => {
  const html = renderToString(<PktState cap={vkFixtureCap} />)
  if (!html.includes('Valkey')) throw new Error('the decoded protocol is not shown')
  return html
})
check('packet inspector: Valkey summary strip', () => {
  const html = renderToString(<PktSummary cap={vkFixtureCap} range={pktRange} setRange={noop} />)
  for (const want of ['Valkey/bus', 'Valkey/replication', 'Valkey errors', 'FULLRESYNC', 'MOVED']) {
    if (!html.includes(want)) throw new Error(`summary omits ${want}`)
  }
  return html
})
check('packet inspector: Valkey packet list', () => {
  const html = renderToString(<PktList packets={vkFixturePackets} first={vkFixtureSummary.firstTs}
    selectedNo={117} onSelect={noop} />)
  for (const want of ['Valkey/bus', 'Valkey/replication', 'MOVED', 'FULLRESYNC',
    'RDB payload', 'propagated: SET', 'claims 5461 slot(s)']) {
    if (!html.includes(want)) throw new Error(`packet list omits ${want}`)
  }
  if (html.includes('undefined')) throw new Error('packet list rendered a literal "undefined"')
  return html
})
for (const p of vkFixturePackets) {
  check(`packet inspector: Valkey details for #${p.no} (${p.proto})`, () => {
    const html = renderToString(<PktDetails
      d={{ packet: p, stream: { index: p.stream, user: 'default',
        role: p.proto === 'Valkey/bus' ? 'valkey-bus' : 'client', roleLabel: p.proto },
        hex: '0000  2a 33 0d 0a 24 33  |*3..$3|', bytes: p.frameLen }}
      first={vkFixtureSummary.firstTs} />)
    if (html.includes('undefined')) throw new Error('details rendered a literal "undefined"')
    if (p.errState && !html.includes(p.errState)) throw new Error('the error code is not shown')
    return html
  })
}
check('packet inspector: Valkey port roles are explained', () => {
  if (!PORT_ROLE_TEXT.valkey.includes('replication')) throw new Error('no Valkey client-port text')
  if (!PORT_ROLE_TEXT['valkey-bus'].includes('gossip')) throw new Error('no cluster-bus text')
  if (!PORT_ROLE_TEXT['valkey-sentinel'].includes('Sentinel')) throw new Error('no sentinel text')
  return 'ok'
})
check('packet inspector: Valkey issues are severe, ordinary ones are not', () => {
  for (const s of ['MOVED → slot 12182 is on 172.31.0.5:6379', 'READONLY — a write reached a replica',
    'OOM — used_memory is above maxmemory', 'MISCONF — writes are refused',
    'FULLRESYNC — the primary is about to send its ENTIRE dataset',
    'KEYS * — this walks the ENTIRE keyspace', 'FAIL message — a node is telling the cluster',
    'Replication lag 12.0 MB']) {
    if (!isSevereIssue(s)) throw new Error(`${s} should be severe`)
  }
  for (const s of ['WRONGTYPE Operation against a key holding the wrong kind of value',
    'NOSCRIPT No matching script']) {
    if (isSevereIssue(s)) throw new Error(`${s} should not be severe`)
  }
  return 'ok'
})
check('packet inspector: Valkey TLS advice', () => {
  const html = renderToString(<PktSummary cap={{ ...vkFixtureCap,
    summary: { ...vkFixtureSummary, tlsStreams: 2 } }} range={pktRange} setRange={noop} />)
  if (!html.includes('tls-port') || !html.includes('SLOWLOG')) {
    throw new Error('Valkey TLS advice missing')
  }
  return html
})
check('packet inspector: Valkey server log', () => {
  const html = renderToString(<PktServerLog onReload={noop} log={{
    path: 'journal:valkey', source: 'node', scanned: 240, inWindow: 4,
    windowFrom: 1785840070, windowTo: 1785840190,
    top: [{ label: 'Full resync — the whole dataset is being transferred', count: 1 },
      { label: 'Cluster state OK', count: 1 }],
    entries: [
      { ts: 1785840101.0, time: '04 Aug 2026 12:16:19.361', level: 'NOTICE', class: 'replication',
        code: '253', subsystem: 'primary', inWindow: true,
        label: 'Full resync — the whole dataset is being transferred',
        message: 'Starting BGSAVE for SYNC with target: replicas sockets' },
      { ts: 1785840102.0, time: '04 Aug 2026 12:16:20.001', level: 'NOTICE', class: 'cluster',
        code: '253', subsystem: 'primary', inWindow: true, label: 'Cluster state OK',
        message: 'Cluster state changed: ok' },
    ],
    stats: { verbosity: 0, suppressionList: '', counters: {},
      hint: 'Valkey has no aborted-connection counters: INFO\'s stats section counts rejected_connections.' },
  }} />)
  for (const want of ['journal:valkey', 'Full resync', 'Cluster state OK', 'rejected_connections']) {
    if (!html.includes(want)) throw new Error(`Valkey server log omits ${want}`)
  }
  return html
})

check('packet inspector: server error log (window mismatch)', () => {
  const html = renderToString(<PktServerLog onReload={noop}
    log={{ path: 'mysqld.log', source: 'upload', scanned: 40, inWindow: 0, mismatch: true,
      logFrom: 1785000000, logTo: 1785000600, windowFrom: 1785775330, windowTo: 1785775402,
      entries: [], top: [] }} />)
  if (!html.includes('none of them fall in this capture')) throw new Error('mismatch warning not shown')
  return html
})

// --------------------------------------------------------------- canvas wiring
//
// A target the backend accepts is still unusable if nothing on the canvas can
// start or finish a line to it. That is not a render failure — the page looks
// fine — so nothing above would catch it, and it is exactly how Stock Market
// Sim shipped able to drive a standalone Percona Server that no user could
// draw a line to: NODE_TYPES.ps had ports:false, so the node drew no handles.
//
// Every kind SS_LINK_TYPES names must therefore be reachable: a node type needs
// ports:true, a frame type needs to be in CONNECTABLE_FRAMES.
check('every Stock Market Sim link target is reachable on the canvas', () => {
  // Several names are both a frame type and the type of the member nodes
  // inside it ('pxc', 'patroni', …). Reachable either way is reachable: a
  // framed member is never the target, the frame around it is.
  const unreachable = Object.keys(SS_LINK_TYPES).filter(
    (kind) => !CONNECTABLE_FRAMES.has(kind) && !NODE_TYPES[kind]?.ports)
  if (unreachable.length) {
    throw new Error('no way to draw a line to: ' + unreachable.join(', '))
  }
  return 'ok'
})

// The engine map decides the driver and whether a size target is possible, so
// every target whose engine the kind alone settles must be in it. The routers
// and the Kubernetes frame are the exceptions, and each has its own check
// below: their engine is a property of what they front, or of the operator the
// frame runs.
check('every Stock Market Sim link target maps to an engine', () => {
  const byOther = new Set(['haproxy', 'proxysql', 'k3d'])
  const missing = Object.keys(SS_LINK_TYPES).filter((k) => !byOther.has(k) && !SS_LINK_ENGINE[k])
  if (missing.length) throw new Error('no engine for: ' + missing.join(', '))
  return 'ok'
})

// A Kubernetes frame is one canvas target with six databases behind it, so the
// engine comes from the frame's operator. Every operator the frame's own picker
// offers has to resolve, or a user selects one and the sim node then refuses to
// deploy against it — which is precisely the gap this replaced.
check('every K3D operator maps a Stock Market Sim node to an engine', () => {
  const missing = Object.keys(K3D_OPERATOR_LABEL)
    .filter((op) => !ssLinkEngine({ kind: 'k3d', operator: op }))
  if (missing.length) throw new Error('no engine for operator: ' + missing.join(', '))
  // ...and a frame with no operator has no database to drive, which the form
  // reports rather than guessing an engine for.
  if (ssLinkEngine({ kind: 'k3d', operator: '' })) throw new Error('an operator-less frame should have no engine')
  return 'ok'
})

// ---- Stalk Summary: verdicts and the two-archive comparison ----
//
// The numbers below are the two real captures this feature was built against:
// one server at innodb_buffer_pool_size=128M and the same server at 4G.

const vsCapture = (host, at, facts, findings, verdicts) => ({
  source: { host, engine: 'mysql', capturedAt: at },
  summary: { facts, findings },
  verdicts,
  series: {},
  available: {},
})

const vs128 = vsCapture('ps-01', '2026-08-12T15:34:04Z',
  { bufferPoolSize: '134217728', flushMethod: 'fsync', redoLogCapacity: '104857600', syncBinlog: '1', flushLogAtTrxCommit: '1' },
  { qps: 1514, bpMissRatioPct: 8.3, bpFreePages: 342, innodbReadMiBs: 1841.9, deviceReadMiBs: 0, fsyncsPerSec: 381, cpuBusyPct: 49.3, cpuIowaitPct: 5, diskUtilPct: 23.5, maxCheckpointAgePctOfRedo: 10.3 },
  [{ id: 'bufferPool', title: 'Buffer pool sizing', level: 'crit', headline: '8.30% of reads miss the pool (117.7k/s)', detail: 'x' },
   { id: 'pageCache', title: 'Do buffer pool misses reach a disk?', level: 'warn', headline: 'InnoDB reads 1842 MiB/s · devices serve 0 MiB/s', detail: 'x' }])

const vs4G = vsCapture('ps-01', '2026-08-12T13:47:00Z',
  { bufferPoolSize: '4294967296', flushMethod: 'fsync', redoLogCapacity: '104857600', syncBinlog: '1', flushLogAtTrxCommit: '1' },
  { qps: 4583, bpMissRatioPct: 0, bpFreePages: 105660, innodbReadMiBs: 0, deviceReadMiBs: 0, fsyncsPerSec: 108, cpuBusyPct: 77.7, cpuIowaitPct: 0.4, diskUtilPct: 11.3, maxCheckpointAgePctOfRedo: 11.3 },
  [{ id: 'bufferPool', title: 'Buffer pool sizing', level: 'ok', headline: '105660 of 262144 pages still free', detail: 'x' }])

check('stalk summary: verdicts card', () => renderToString(<Verdicts verdicts={vs128.verdicts} />))
check('stalk summary: verdicts card with none', () => renderToString(<div><Verdicts verdicts={[]} /></div>))
check('stalk summary: comparison of two captures', () => renderToString(<Comparison a={vs128} b={vs4G} />))
check('stalk summary: comparison when nothing differs', () => renderToString(<Comparison a={vs128} b={vs128} />))
check('stalk summary: comparison with a missing finding on one side', () =>
  renderToString(<Comparison a={vs128} b={vsCapture('ps-01', '2026-08-12T16:00:00Z', {}, { qps: 900 }, [])} />))

// Per-chart advisors. The collapsed state is what everyone sees, but the
// expanded one is the whole point, so render both. Icon.ChevronDown does not
// exist in this codebase — an advisor reaching for it renders <undefined /> and
// blanks the page, which is the bug this whole file was written for.
const anAdvisor = {
  id: 'bufferPoolReads', level: 'crit',
  headline: '408.9k requests/s, 34.2k misses/s (8.30%)',
  detail: 'Logical reads against the pool, and how many did not find their page in it. The working set is much larger than the pool.',
}
check('stalk summary: advisor (collapsed)', () => renderToString(<Advisor a={anAdvisor} />))
check('stalk summary: advisor with no data', () => renderToString(<div><Advisor a={null} /></div>))
for (const level of ['ok', 'info', 'warn', 'crit']) {
  check(`stalk summary: advisor level ${level}`, () =>
    renderToString(<Advisor a={{ ...anAdvisor, level }} />))
}
// The split explanation. Verdicts built by advice() carry means/action; the few
// assembled field-by-field carry only detail, and both paths have to render —
// an advisor that shows nothing at all is the same class of bug as one that
// blanks the page.
const aSplitAdvisor = {
  ...anAdvisor,
  means: 'Logical reads against the pool, and how many did not find their page in it.',
  action: 'Raise innodb_buffer_pool_size, or reduce what the workload touches.',
}
check('stalk summary: advisor with means and action', () =>
  renderToString(<Advisor a={aSplitAdvisor} />))
check('stalk summary: advisor with means but no action', () =>
  renderToString(<Advisor a={{ ...aSplitAdvisor, action: '' }} />))
check('stalk summary: advisor with neither, only detail', () =>
  renderToString(<Advisor a={{ ...anAdvisor, means: '', action: '' }} />))
check('stalk summary: advisor with no text at all', () =>
  renderToString(<Advisor a={{ id: 'x', level: 'ok', headline: '0/s' }} />))
check('stalk summary: verdict body standalone', () =>
  renderToString(<VerdictBody v={aSplitAdvisor} />))
check('stalk summary: verdict body with nothing', () =>
  renderToString(<div><VerdictBody v={null} /></div>))
// Every level has to resolve to a real icon. An unknown level must fall back
// rather than render <undefined /> — the bug this file exists for.
for (const level of ['ok', 'info', 'warn', 'crit', 'banana', undefined]) {
  check(`stalk summary: verdict mark ${level}`, () =>
    renderToString(<VerdictMark level={level} />))
}
// The lock-waits table and the transaction advisor that sits under it. An
// advisor whose data comes from a table rather than a chart still has to render.
const aLockWaitAdvisor = {
  id: 'innodbTrx', level: 'crit',
  headline: 'thread 8214 blocked 3 transaction(s) for 43s on lab.t, idle in trx 90s',
  means: 'The transaction other transactions are waiting behind.',
  action: 'Find thread 8214 and end it, then look for the missing COMMIT.',
}
check('stalk summary: lock wait advisor', () => renderToString(<Advisor a={aLockWaitAdvisor} />))
check('stalk summary: transaction advisor with no lock wait', () =>
  renderToString(<Advisor a={{ id: 'innodbTrx', level: 'warn', headline: 'thread 7139 active 86s, 1 row locks', means: 'The longest-running transaction seen.', action: 'Long enough to hold back purge.' }} />))

// The panels added for DDL blocking, the network, and the table cache. Each
// advisor renders from a table rather than a chart, and each must survive the
// level it reports at.
for (const [id, level, headline] of [
  ['metadataLocks', 'crit', '1 pending on lab.t, 1 holder(s)'],
  ['tcp', 'crit', '77 of 989 segments retransmitted (7.786%)'],
  ['errorLog', 'crit', '3 membership, 1 state transfer'],
  ['tableCache', 'info', '200 opens/s, 200 misses/s, 0 overflows/s'],
]) {
  check(`stalk summary: advisor ${id}`, () =>
    renderToString(<Advisor a={{ id, level, headline, means: 'what it measures', action: 'what to do' }} />))
}

check('stalk summary: chart card carrying an advisor', () =>
  renderToString(<ChartCard title="Buffer pool reads" subtitle="/s" advisor={anAdvisor}><div /></ChartCard>))
check('stalk summary: chart card without one', () =>
  renderToString(<ChartCard title="Memory" subtitle="MB"><div /></ChartCard>))

// Kept captures + the N-way head-to-head. The comparison payload is built by
// the backend, so these render exactly what buildComparison emits.
const keptArchives = [
  { id: 3, capturedAt: '2026-08-12T15:34:04Z', host: 'ps-01', nodeLabel: 'ps-01', stackName: 'stack', sizeBytes: 1866761, note: 'after 4G pool' },
  { id: 2, capturedAt: '2026-08-12T13:47:00Z', host: 'ps-01', nodeLabel: 'ps-01', stackName: 'stack', sizeBytes: 1820176, note: '' },
]
check('stalk summary: kept captures list', () =>
  renderToString(<KeptCaptures archives={keptArchives} picked={[2]} onAnalyze={noop} onToggle={noop} onCompare={noop} onDelete={noop} onClear={noop} />))
check('stalk summary: kept captures, none kept yet', () =>
  renderToString(<div><KeptCaptures archives={[]} picked={[]} onAnalyze={noop} onToggle={noop} onCompare={noop} onDelete={noop} onClear={noop} /></div>))

const headToHead = {
  captures: [
    { archiveId: 2, host: 'ps-01', capturedAt: '2026-08-12T13:47:00Z', note: '' },
    { archiveId: 3, host: 'ps-01', capturedAt: '2026-08-12T15:34:04Z', note: 'after 4G pool' },
  ],
  settings: [{ key: 'bufferPoolSize', label: 'innodb_buffer_pool_size', values: ['134217728', '4294967296'], bytes: true }],
  metrics: [
    { key: 'qps', label: 'Throughput', unit: '/s', values: [1514, 4583], have: [true, true], changePct: 202.7, better: 'up', improved: true, meaningful: true },
    { key: 'cpuBusyPct', label: 'CPU busy', unit: '%', values: [38.9, 72.4], have: [true, true], changePct: 86.1, better: '', meaningful: true },
    { key: 'bpMissRatioPct', label: 'Buffer pool read-miss', unit: '%', values: [8.3, 0], have: [true, false], changePct: -100, better: 'down', improved: true, meaningful: true },
  ],
  verdicts: [{ id: 'comparePool', title: 'Did the buffer pool change help?', level: 'ok', headline: 'read-miss 8.30% -> 0.00% (-100%)', detail: 'cause and effect' }],
}
check('stalk summary: head to head', () => renderToString(<HeadToHead cmp={headToHead} />))
check('stalk summary: head to head with no verdicts or settings', () =>
  renderToString(<HeadToHead cmp={{ ...headToHead, verdicts: [], settings: [] }} />))
check('stalk summary: head to head with nothing', () =>
  renderToString(<div><HeadToHead cmp={null} /></div>))

// ---- Log Summary: the swimlane, the verdict and the event list ----
//
// The fixture is a scaled-down version of the network-partition capture the Go rules were
// written against (app/testdata/logsummary/s06-network-partition): two members keep quorum
// while a third is cut off, goes non-primary and aborts.

const logSources = [
  { idx: 0, name: 'pxc01.err', node: 'pxc01', engine: 'mysql', flavour: 'galera', origin: 'upload',
    bytes: 40000, lines: 207, records: 190, events: 44, firstTs: 1000, lastTs: 1058,
    counts: { ok: 9, warn: 30, bad: 3, info: 2 } },
  { idx: 1, name: 'pxc02.err', node: 'pxc02', engine: 'mysql', flavour: 'galera', origin: 'upload',
    bytes: 38000, lines: 197, records: 180, events: 41, firstTs: 1000, lastTs: 1058,
    counts: { ok: 8, warn: 29, bad: 2, info: 2 } },
  { idx: 2, name: 'pxc03.err', node: 'pxc03', engine: 'mysql', flavour: 'galera', origin: 'upload',
    bytes: 45000, lines: 223, records: 210, events: 45, firstTs: 1000, lastTs: 1058,
    counts: { ok: 6, warn: 17, bad: 22, info: 0 } },
]
const logSummary = {
  sources: 3, events: 130, firstTs: 1000, lastTs: 1058, overlap: 58, disjoint: false,
  counts: { ok: 23, warn: 96, bad: 27, info: 54 },
  classes: { membership: 30, network: 40, state: 20, quorum: 12, transfer: 18, crash: 2, other: 8 },
  top: [
    { label: 'Peer declared inactive', class: 'membership', sev: 'bad', count: 8 },
    { label: 'Lost the primary component', class: 'quorum', sev: 'bad', count: 4 },
    { label: 'Peer went quiet', class: 'network', sev: 'warn', count: 24 },
    { label: 'Member synced with group', class: 'state', sev: 'ok', count: 6 },
  ],
}
const logBundle = {
  id: 'log-1', label: 'pxc-cluster · 3 node(s)', origin: 'node', created: '2026-08-14T01:49:00Z',
  sources: logSources, summary: logSummary,
}
const logFindings = [
  { id: 'crash', sev: 'bad', title: 'A server stopped abnormally',
    detail: 'pxc03: Aborting: will never receive state; mysqld terminated',
    advice: 'Read the records just before each of these.', at: 1052, sources: [2], events: [90] },
  { id: 'quorum', sev: 'bad', title: 'The cluster split — one side kept quorum, the other did not',
    detail: 'pxc03 could not see a majority of the cluster.', at: 1003, until: 1052, sources: [2] },
  { id: 'flow-control', sev: 'info', title: 'Flow-control pauses are not recorded in this log',
    detail: 'Galera writes the interval, never the pause.',
    advice: 'Watch wsrep_flow_control_paused instead.' },
  { id: 'healthy', sev: 'ok', title: 'No problems found in this window', detail: 'all routine.' },
]
const logPhases = [
  { src: 0, from: 1000, to: 1058, state: 'SYNCED', sev: 'ok', members: 2, primary: 'yes' },
  { src: 1, from: 1000, to: 1058, state: 'SYNCED', sev: 'ok', members: 2, primary: 'yes', inferred: true },
  { src: 2, from: 1000, to: 1003, state: 'SYNCED', sev: 'ok', members: 3, primary: 'yes' },
  { src: 2, from: 1003, to: 1052, state: 'OPEN', sev: 'bad', members: 1, primary: 'no' },
  { src: 2, from: 1052, to: 1058, state: 'DOWN', sev: 'bad' },
]
const logTimeline = {
  fromTs: 1000, toTs: 1058, matched: 130,
  buckets: [
    { src: 0, i: 0, ts: 1000, ok: 1, warn: 2, bad: 0, info: 1, count: 4 },
    { src: 0, i: 1, ts: 1029, ok: 0, warn: 3, bad: 0, info: 0, count: 3 },
    { src: 1, i: 0, ts: 1000, ok: 0, warn: 1, bad: 0, info: 0, count: 1 },
    { src: 1, i: 1, ts: 1029, ok: 2, warn: 0, bad: 0, info: 1, count: 3 },
    { src: 2, i: 0, ts: 1000, ok: 0, warn: 4, bad: 6, info: 0, count: 10 },
    { src: 2, i: 1, ts: 1029, ok: 0, warn: 0, bad: 2, info: 0, count: 2 },
  ],
  phases: logPhases,
}
const logEventsFixture = [
  { no: 1, src: 2, ts: 1003.001, line: 42, time: '2026-08-14T01:49:35.823Z', level: 'Note',
    subsystem: 'Galera', class: 'quorum', sev: 'bad', label: 'Lost the primary component',
    meaning: 'This node can no longer see a majority of the cluster.',
    message: 'Received NON-PRIMARY.', primary: 'no', members: 1 },
  { no: 2, src: 0, ts: 1003.5, line: 61, level: 'Note', subsystem: 'Galera',
    class: 'network', sev: 'warn', label: 'Peer went quiet', message: 'no messages seen in PT3S',
    peer: '172.27.0.4', repeat: 24, endTs: 1050.2 },
  { no: 3, src: 1, ts: 1052.1, line: 130, level: 'Note', subsystem: 'Galera',
    class: 'membership', sev: 'ok', label: 'Member synced with group',
    message: '3 member(s)', detail: 'view (view_id(PRIM,0bc20092-ac42,9)\nmemb {\n\t0bc20092-ac42,0\n\t}' },
  { no: 4, src: 2, ts: 1052.4, line: 200, level: 'ERROR', subsystem: 'Galera', code: 'MY-000000',
    class: 'crash', sev: 'bad', label: 'Aborting: will never receive state',
    meaning: 'The node asked for a state transfer and the donor went away.',
    message: 'Will never receive state. Need to abort.', approx: false },
]
const logSnapshot = {
  at: 1003.5, agree: false,
  nodes: [
    { src: 0, node: 'pxc01', state: 'SYNCED', sev: 'ok', members: 2, primary: 'yes',
      meaning: STATE_TEXT.SYNCED, since: 1000, until: 1058, covered: true },
    { src: 1, node: 'pxc02', state: 'SYNCED', sev: 'ok', members: 2, primary: 'yes', covered: true },
    { src: 2, node: 'pxc03', state: 'OPEN', sev: 'bad', members: 1, primary: 'no',
      meaning: STATE_TEXT.OPEN, covered: false },
  ],
  before: logEventsFixture[0],
  after: logEventsFixture[3],
}
const logRange = { fromTs: '', toTs: '', src: -1, class: '', q: '', sev: [] }

check('log summary: page shell', () => renderToString(<LogSummary />))
check('log summary: legend', () => renderToString(<LogLegend />))
check('log summary: sources card', () =>
  renderToString(<LogSources bundle={logBundle} id="log-1" />))
check('log summary: sources card with a disjoint bundle', () =>
  renderToString(<LogSources bundle={{ ...logBundle, note: 'could not read: pxc04', summary: { ...logSummary, disjoint: true } }} id="log-1" />))
check('log summary: verdict', () => renderToString(<LogVerdict findings={logFindings} onGo={noop} />))
check('log summary: verdict with nothing', () =>
  renderToString(<div><LogVerdict findings={[]} onGo={noop} /></div>))
for (const sev of ['bad', 'warn', 'ok', 'info', 'banana', undefined]) {
  check(`log summary: verdict severity ${sev}`, () =>
    renderToString(<LogVerdict findings={[{ id: 'x', sev, title: 't', detail: 'd' }]} onGo={noop} />))
}
// The verdict narrows with the timeline. A reader who drags a window on the swimlane is
// asking "what does THIS stretch add up to", so the conclusions that do not touch it are
// taken out — but the undated ones never are, because they stay true of any window and one
// of them is usually the most important line on the page.
check('log summary: verdict narrowed to a window', () =>
  renderToString(<LogVerdict findings={logFindings} onGo={noop} from={1040} to={1060} onClear={noop} />))
check('log summary: verdict narrowed to a window with nothing in it', () =>
  renderToString(<LogVerdict findings={logFindings} onGo={noop} from={1200} to={1300} onClear={noop} />))

check('log summary: the verdict filter keeps spans that overlap the window', () => {
  const { inWindow, always, hidden, narrowed } = splitFindings(logFindings, 1040, 1060)
  if (!narrowed) throw new Error('a window was given and the verdict did not narrow')
  const ids = inWindow.map((f) => f.id).sort().join(',')
  // `crash` is an instant at 1052, inside. `quorum` is a span 1003–1052 that OVERLAPS the
  // window without being contained by it — containment would drop exactly the finding a
  // reader zooms into the middle of.
  if (ids !== 'crash,quorum') throw new Error(`in window: ${ids}`)
  // The two undated ones are never hidden, and are not counted as hidden either.
  if (always.length !== 2) throw new Error(`undated: ${always.length}`)
  if (hidden !== 0) throw new Error(`hidden: ${hidden}`)
  return 'ok'
})

check('log summary: the verdict filter hides only dated conclusions outside the window', () => {
  const { inWindow, always, hidden } = splitFindings(logFindings, 1200, 1300)
  if (inWindow.length !== 0) throw new Error(`nothing happened there: ${inWindow.length}`)
  if (hidden !== 2) throw new Error(`hidden: ${hidden}`)
  if (always.length !== 2) throw new Error(`undated: ${always.length}`)
  return 'ok'
})

check('log summary: no window means no narrowing at all', () => {
  for (const [from, to] of [[0, 0], [1040, 0], [0, 1060], [1060, 1040]]) {
    const { inWindow, always, narrowed } = splitFindings(logFindings, from, to)
    if (narrowed) throw new Error(`from=${from} to=${to} narrowed on a window that is not one`)
    if (inWindow.length !== logFindings.length || always.length !== 0) {
      throw new Error(`from=${from} to=${to} did not pass everything through`)
    }
  }
  return 'ok'
})

check('log summary: swimlane', () =>
  renderToString(<LogSwimlane timeline={logTimeline} sources={logSources} first={1000} onSelect={noop} onPick={noop} />))
check('log summary: swimlane before the timeline loads', () =>
  renderToString(<div><LogSwimlane timeline={null} sources={logSources} first={0} onSelect={noop} onPick={noop} /></div>))
check('log summary: swimlane with no buckets or phases', () =>
  renderToString(<LogSwimlane timeline={{ fromTs: 1000, toTs: 1058, buckets: [], phases: [], matched: 0 }}
    sources={logSources} first={1000} onSelect={noop} onPick={noop} />))
check('log summary: instant readout', () =>
  renderToString(<LogSnapshot snap={logSnapshot} sources={logSources} onClose={noop} />))
check('log summary: instant readout when the nodes agree', () =>
  renderToString(<LogSnapshot snap={{ ...logSnapshot, agree: true, before: null, after: null }}
    sources={logSources} onClose={noop} />))
check('log summary: event list', () =>
  renderToString(<LogEvents events={logEventsFixture} sources={logSources} first={1000} selectedNo={2} onSelect={noop} />))
// The per-node column view: the same events, one column per source, still in time order.
check('log summary: events by node', () =>
  renderToString(<LogEventColumns events={logEventsFixture} sources={logSources} first={1000}
    selectedNo={2} onSelect={noop} />))
check('log summary: events by node with nothing matching', () =>
  renderToString(<div><LogEventColumns events={[]} sources={logSources} first={1000} onSelect={noop} /></div>))
check('log summary: events by node puts every event under its own source', () => {
  const html = renderToString(<LogEventColumns events={logEventsFixture} sources={logSources} first={1000}
    onSelect={noop} />)
  // One header cell per source plus the frozen time column, and every event's label present
  // exactly once — a label appearing twice would mean a row rendered it in more than one
  // column, which is the bug this view could most easily have.
  for (const e of logEventsFixture) {
    const n = html.split(e.label).length - 1
    if (n !== 1) throw new Error(`${e.label} appears ${n} times`)
  }
  if (!html.includes('Time')) throw new Error('no frozen time column')
  // The header and the time column have to be sticky, or scrolling loses the thing you are
  // reading against.
  if (!html.includes('sticky')) throw new Error('nothing is sticky')
  return html
})

check('log summary: event list with nothing matching', () =>
  renderToString(<LogEvents events={[]} sources={logSources} first={1000} onSelect={noop} />))
for (const e of logEventsFixture) {
  check(`log summary: event detail #${e.no}`, () =>
    renderToString(<LogDetail e={e} bundle={logBundle} id="log-1" first={1000} />))
}
check('log summary: filters', () =>
  renderToString(<LogFilters range={logRange} setRange={noop} summary={logSummary} sources={logSources} />))
check('log summary: filters with a severity picked', () =>
  renderToString(<LogFilters range={{ ...logRange, sev: ['bad'] }} setRange={noop} summary={logSummary} sources={logSources} />))
check('log summary: what happened most', () =>
  renderToString(<LogTop summary={logSummary} range={logRange} setRange={noop} />))
check('log summary: what happened most, with nothing', () =>
  renderToString(<div><LogTop summary={{ ...logSummary, top: [] }} range={logRange} setRange={noop} /></div>))
check('log summary: range controls', () =>
  renderToString(<LogRange range={logRange} setRange={noop} buckets={180} setBuckets={noop} summary={logSummary} span={58} />))
check('log summary: upload panel', () =>
  renderToString(<LogUpload files={[]} setFiles={noop} busy={false} onUpload={noop} onCancel={noop} />))
check('log summary: upload panel with several files', () =>
  renderToString(<LogUpload files={[{ name: 'pxc01.err', size: 40000 }, { name: 'pxc02.err', size: 38000 }]}
    setFiles={noop} busy={false} onUpload={noop} onCancel={noop} />))
check('log summary: pager', () =>
  renderToString(<LogPager page={{ matched: 900, offset: 200, limit: 200 }} onPage={noop} />))

for (const size of ['sm', 'lg']) {
  check(`log summary: node chip (${size})`, () =>
    renderToString(<LogNodeChip src={0} name="pxc01" size={size} />))
}
check('log summary: node chip past the palette', () =>
  renderToString(<LogNodeChip src={NODE_SLOTS + 3} name="pxc09" />))
check('log summary: sources card beyond the node palette', () => {
  const many = Array.from({ length: NODE_SLOTS + 2 }, (_, i) => ({
    ...logSources[0], idx: i, name: `pxc0${i + 1}.err`, node: `pxc0${i + 1}`,
  }))
  return renderToString(<LogSources bundle={{ ...logBundle, sources: many }} id="log-1" />)
})

// The node palette is a fixed set of literal class names, because Tailwind only emits the
// strings it can see in the source — a class composed at runtime silently renders as
// nothing at all. Every slot must resolve to its own slot, and the slot past the end must
// fall back rather than quietly reuse a colour.
check('log summary: every node slot has real classes', () => {
  for (let i = 0; i < NODE_SLOTS; i++) {
    for (const [what, cls] of [['fill', nodeFill(i)], ['tint', nodeTint(i)],
      ['edge', nodeEdge(i)], ['edge-soft', nodeEdgeSoft(i)]]) {
      if (!cls || cls.includes('undefined')) throw new Error(`node slot ${i} ${what}: ${cls}`)
      if (!cls.includes(`node-${i + 1}`)) throw new Error(`node slot ${i} ${what} is not slot ${i + 1}: ${cls}`)
    }
  }
  for (const f of [nodeFill, nodeTint, nodeEdge, nodeEdgeSoft]) {
    const cls = f(NODE_SLOTS)
    if (!cls || cls.includes('node-')) throw new Error(`slot past the end reused a node colour: ${cls}`)
  }
  return 'ok'
})

// Every severity and state the backend can emit must have a colour, a word and a glyph —
// colour is never the only signal, so a missing entry is a rendering bug waiting to happen.
check('log summary: every severity is styled', () => {
  for (const sev of SEVS) {
    if (!SEV_TEXT[sev] || !SEV_FILL[sev]) throw new Error(`severity ${sev} has no style`)
  }
  for (const st of Object.keys(STATE_TEXT)) {
    if (!STATE_SEV[st]) throw new Error(`state ${st} has no severity`)
  }
  // Three state machines share this vocabulary and a missing entry paints the lane 'info'
  // — a state nobody has an opinion about — where a Group Replication member sitting at
  // BLOCKED or OFFLINE has to read as bad. Named explicitly so adding a state to the Go
  // catalogue and forgetting the JS side fails here rather than in front of somebody.
  for (const st of ['SYNCED', 'JOINER', 'DONOR', 'OPEN', 'CLOSED',
                    'ONLINE', 'RECOVERING', 'BLOCKED', 'ERROR', 'OFFLINE',
                    'PRIMARY', 'SECONDARY', 'STARTUP2', 'ROLLBACK', 'ARBITER', 'REMOVED', 'ROUTING',
                    'STANDBY', 'PROMOTING',
                    'REPLICA', 'SYNCING', 'LOADING', 'CLUSTERDOWN',
                    'RUNNING', 'STARTING', 'DOWN', 'UNKNOWN']) {
    if (!STATE_TEXT[st]) throw new Error(`state ${st} has no explanation`)
    if (!SEV_FILL[STATE_SEV[st]]) throw new Error(`state ${st} has no fill`)
  }
  // CLUSTERDOWN is the one state in the vocabulary that means "healthy and answering
  // nothing", and it has to read as bad. A Valkey Cluster node in it is not the node that
  // failed — painting its lane anything but red is the specific way this page would mislead.
  if (STATE_SEV.CLUSTERDOWN !== 'bad') throw new Error('CLUSTERDOWN must read as bad')
  if (STATE_SEV.REPLICA !== 'ok') throw new Error('a Valkey replica answering reads must read as ok')
  for (const cls of Object.keys(logSummary.classes)) {
    if (!CLASS_LABEL[cls]) throw new Error(`class ${cls} has no label`)
  }
  if (logDur(0) !== '0s') throw new Error('logDur(0) should be 0s')
  return 'ok'
})

// A Valkey bundle renders the same three panels as every other engine's, and the two things
// worth asserting are the two that are new: the flavour badge beside the node chip, and a
// CLUSTERDOWN lane painted as the outage it is. A node chip with no badge would read as a
// standalone cache, which is the opposite of a cluster member whose cluster is down.
const logValkeySources = [
  { idx: 0, name: 'vkc1.log', node: 'vkc1', engine: 'valkey', flavour: 'valkeycluster', origin: 'node',
    bytes: 9000, lines: 27, records: 27, events: 21, firstTs: 1000, lastTs: 1058,
    counts: { ok: 6, warn: 9, bad: 4, info: 2 } },
  { idx: 1, name: 'vkc2.log', node: 'vkc2', engine: 'valkey', flavour: 'valkeycluster', origin: 'node',
    bytes: 26000, lines: 94, records: 94, events: 48, firstTs: 1000, lastTs: 1058,
    counts: { ok: 9, warn: 20, bad: 5, info: 14 } },
  { idx: 2, name: 'vkb.log', node: 'vkb', engine: 'valkey', flavour: 'valkeyrepl', origin: 'node',
    bytes: 30000, lines: 128, records: 128, events: 60, firstTs: 1000, lastTs: 1058,
    counts: { ok: 11, warn: 24, bad: 3, info: 22 } },
]
check('log summary: Valkey sources card names the member kind', () => {
  const html = renderToString(<LogSources
    bundle={{ ...logBundle, sources: logValkeySources }} id="log-vk" />)
  for (const want of ['Valkey Cluster member', 'Valkey replication', 'vkc1', 'vkb']) {
    if (!html.includes(want)) throw new Error(`the sources card does not mention ${want}`)
  }
  return html
})
check('log summary: Valkey swimlane with a CLUSTERDOWN stretch', () => {
  const phases = [
    { src: 0, from: 1000, to: 1020, state: 'PRIMARY', sev: 'ok' },
    { src: 0, from: 1020, to: 1045, state: 'CLUSTERDOWN', sev: 'bad' },
    { src: 0, from: 1045, to: 1058, state: 'PRIMARY', sev: 'ok' },
    { src: 1, from: 1000, to: 1020, state: 'REPLICA', sev: 'ok' },
    { src: 1, from: 1020, to: 1030, state: 'LOADING', sev: 'warn' },
    { src: 1, from: 1030, to: 1058, state: 'DOWN', sev: 'bad' },
    { src: 2, from: 1000, to: 1058, state: 'REPLICA', sev: 'ok' },
  ]
  return renderToString(<LogSwimlane
    timeline={{ fromTs: 1000, toTs: 1058, buckets: [], phases, matched: 0 }}
    sources={logValkeySources} first={1000} onSelect={noop} onPick={noop} />)
})
check('log summary: Valkey verdict', () => renderToString(<LogVerdict onGo={noop} findings={[
  { id: 'vk-cluster-down', sev: 'bad', title: 'The cluster refused every client for 24.6s',
    detail: 'from 23:08:48 for 24.6s, reported by vkc1, vkc3.', at: 1020, until: 1045, sources: [0, 1] },
  { id: 'vk-killed', sev: 'bad', title: 'A server was killed, not stopped',
    detail: 'vkb at 23:07:26 — systemd recorded the process being terminated by a signal.', at: 1030 },
  { id: 'vk-invisible', sev: 'info', title: 'Evictions, MISCONF refusals and failed logins are not in this log',
    detail: 'Three things a Valkey server does are entirely absent from its log.' },
]} />))

// ---- FTDC Summary: the charts and the advisor ----
//
// The fixture is the shape ftdcSummarise actually emits — one timestamp column and one
// array per series, which is how FTDC itself is laid out — so a change to that contract
// breaks here rather than in front of somebody holding a diagnostic.data directory.
const ftdcModel = {
  host: 'mongo03', version: '8.0.28-12', replSet: 'rs0',
  from: 1786730000, to: 1786730013, samples: 14, chunks: 3, metrics: 3954,
  ts: [1786730000, 1786730001, 1786730002, 1786730003],
  charts: [
    {
      id: 'memberState', group: 'Replication', title: 'Replica-set member state', unit: 'state',
      why: '1 PRIMARY · 2 SECONDARY · 9 ROLLBACK.',
      series: [
        { name: 'member 0', points: [1, 1, 2, 2] },
        { name: 'member 1 (this one)', points: [2, 2, 1, 1] },
      ],
      advice: { level: 'warn', headline: '2 member state change(s) in this window', detail: 'A failover.', action: 'Line it up against Log Summary.' },
    },
    {
      id: 'replLag', group: 'Replication', title: 'Replication lag', unit: 's', why: 'Not in the log at all.',
      series: [{ name: 'member 0', points: [0, 0, 61, 12] }],
      advice: { level: 'crit', headline: 'A member was 61.0s behind at its worst' },
    },
    {
      id: 'ops', group: 'Work', title: 'Operations', unit: 'ops/s', stack: true, why: 'The operation mix.',
      series: [{ name: 'insert', points: [1, 2, 3, 4] }, { name: 'query', points: [0, 1, 0, 1] }],
      advice: { level: 'info', headline: 'Peak roughly 5 operations/s' },
    },
  ],
  notes: ['1 chunk(s) would not decode and were skipped.'],
}

check('ftdc summary: page shell', () => renderToString(<FTDCSummary />))
// The upload box, pinned, because this is where the page was broken: an `accept` list can
// only ever be extensions, a metrics file's "extension" is its timestamp, and the filter
// therefore hid every file the page exists to read — leaving .tar.gz as the only upload
// that worked. One input takes files, the other takes the directory.
check('ftdc summary: the file picker filters nothing and offers a folder', () => {
  const html = renderToString(<FTDCSummary />)
  const inputs = html.match(/<input[^>]*type="file"[^>]*>/g) || []
  if (inputs.length !== 2) throw new Error(`want a file picker and a folder picker, got ${inputs.length}`)
  for (const i of inputs) {
    if (i.includes('accept=')) throw new Error(`a metrics.<timestamp> file cannot survive an accept list: ${i}`)
  }
  if (!inputs.some((i) => i.includes('webkitdirectory'))) {
    throw new Error('no directory picker — diagnostic.data is a folder')
  }
  if (!inputs.some((i) => i.includes('multiple') && !i.includes('webkitdirectory'))) {
    throw new Error('the file picker takes one file at a time; a directory means nothing one file at a time')
  }
  return html
})
check('ftdc summary: file summary', () => renderToString(<FtdcSummary model={ftdcModel} />))
check('ftdc summary: file summary with nothing', () => renderToString(<div><FtdcSummary model={null} /></div>))
for (const c of ftdcModel.charts) {
  check(`ftdc summary: chart ${c.id}`, () => renderToString(<FtdcChart chart={c} ts={ftdcModel.ts} />))
}
check('ftdc summary: chart with no advice', () =>
  renderToString(<FtdcChart chart={{ ...ftdcModel.charts[0], advice: null }} ts={ftdcModel.ts} />))
check('ftdc summary: chart with nothing', () => renderToString(<div><FtdcChart chart={null} ts={[]} /></div>))
check('ftdc summary: grouped chart list', () => renderToString(<FtdcCharts model={ftdcModel} />))
check('ftdc summary: grouped list with nothing', () => renderToString(<div><FtdcCharts model={{ charts: [] }} /></div>))
check('ftdc summary: a group heading is printed once per group', () => {
  const html = renderToString(<FtdcCharts model={ftdcModel} />)
  // Count the heading ELEMENTS, not the words: "Replication" also appears inside the
  // chart title "Replication lag", which is what made the first version of this check
  // fail against correct output.
  const heads = html.match(/<h2[^>]*>([^<]*)<\/h2>/g) || []
  if (heads.length !== 2) throw new Error(`want 2 headings for 3 charts in 2 groups, got ${heads.length}`)
  if (!heads[0].includes('Replication') || !heads[1].includes('Work')) {
    throw new Error(`headings are wrong or out of order: ${heads.join(' | ')}`)
  }
  return 'ok'
})
check('ftdc summary: findings strip', () => renderToString(<FtdcFindings model={ftdcModel} />))
check('ftdc summary: findings strip picks only warn and crit', () => {
  const html = renderToString(<FtdcFindings model={ftdcModel} />)
  // Thirty-odd charts is more than anybody reads in order, so the shortlist is the part of
  // the page that has to be right: an "ok" chart appearing here would send the reader to a
  // chart with nothing on it, and a crit missing from it is worse.
  if (!html.includes('Replication lag')) throw new Error('the crit chart is missing from the shortlist')
  if (!html.includes('Replica-set member state')) throw new Error('the warn chart is missing from the shortlist')
  if (html.includes('Operations')) throw new Error('an info chart should not be in the shortlist')
  // SSR splits adjacent text nodes with <!-- --> markers, so the count has to be read from
  // the stripped string rather than the raw one.
  const flat = html.replace(/<!--[^>]*-->/g, '')
  if (!flat.includes('2 of 3 charts')) throw new Error(`the count is wrong: ${flat.slice(0, 200)}`)
  return 'ok'
})
check('ftdc summary: findings strip says so when nothing is flagged', () => {
  const quiet = { ...ftdcModel, charts: [{ ...ftdcModel.charts[2] }] }
  const html = renderToString(<FtdcFindings model={quiet} />)
  if (!html.includes('crossed a threshold')) throw new Error('a quiet capture should say so rather than render empty')
  return 'ok'
})

for (const level of ['ok', 'warn', 'crit', 'info', 'banana', undefined]) {
  check(`ftdc summary: advice level ${level}`, () =>
    renderToString(<FtdcAdvice a={{ level, headline: 'h', detail: 'd', action: 'a' }} />))
}
// The configuration block is the one part of the page that tells somebody to change a
// server setting, so an unrendered field there is a recommendation nobody acts on.
// The variables block is the only part of this page that tells somebody to change a
// server setting, and the Cost line is the half that must never be dropped: a page that
// recommends sync_binlog=0 without saying what is lost is giving somebody else's benchmark.
check('stalk summary: configuration advice renders every field, cost included', () => {
  const config = [
    { level: 'crit', variable: 'innodb_buffer_pool_size', current: '128 MiB',
      suggest: '16 GiB to start', why: '29.4 GiB of RAM and InnoDB is allowed 128 MiB of it.',
      effect: '119 TPS became 792 TPS.' },
    { level: 'info', variable: 'sync_binlog, innodb_flush_log_at_trx_commit', current: '1, 1',
      suggest: '0 and 2 if this data can be rebuilt', why: '607 fsyncs/s.',
      risk: 'A power cut loses up to a second of committed transactions.' },
    { level: 'ok', variable: 'innodb_buffer_pool_size', suggest: 'leave it', why: 'The working set fits.' },
  ]
  const html = renderToString(<StalkConfig config={config} />)
  for (const want of ['innodb_buffer_pool_size', '128 MiB', '16 GiB to start', 'Cost:', 'power cut', '792 TPS']) {
    if (!html.includes(want)) throw new Error(`configuration advice dropped ${want}`)
  }
  const flat = html.replace(/<!--[^>]*-->/g, '')
  if (!flat.includes('2 variables worth changing')) throw new Error(`the count includes the keeps: ${flat.slice(0, 300)}`)
  return 'ok'
})
check('stalk summary: configuration advice stays quiet with nothing to say', () => {
  if (renderToString(<StalkConfig config={[]} />) !== '') throw new Error('an empty configuration block should render nothing')
  if (renderToString(<StalkConfig config={undefined} />) !== '') throw new Error('a capture with no config block should render nothing')
  return 'ok'
})
check('ftdc summary: configuration advice renders every field', () => {
  const model = { config: [
    { level: 'crit', setting: 'storage.wiredTiger.engineConfig.cacheSizeGB', current: 'unset — mongod derived 14.2 GiB',
      suggest: 'pin it, across every mongod on this host', why: 'The host had 473 MiB available.', effect: '111 TPS became 637 TPS.' },
    { level: 'ok', setting: 'storageEngineConcurrentReadTransactions', suggest: 'leave them alone', why: 'Tickets ran out with no wait behind them.' },
  ] }
  const html = renderToString(<FtdcConfig model={model} />)
  for (const want of ['cacheSizeGB', 'unset', 'pin it', '473 MiB', '637 TPS', 'leave them alone']) {
    if (!html.includes(want)) throw new Error(`configuration advice dropped ${want}`)
  }
  // "Keep this as it is" is an answer, not a defect, and must not be counted as a change.
  const flat = html.replace(/<!--[^>]*-->/g, '')
  if (!flat.includes('1 setting worth changing')) throw new Error(`the count includes the keeps: ${flat.slice(0, 300)}`)
  if (!html.includes('KEEP') && !html.includes('keep')) throw new Error('an ok recommendation should read as keep, not as a change')
  return 'ok'
})
check('ftdc summary: configuration advice stays quiet when there is nothing to change', () => {
  if (renderToString(<FtdcConfig model={{ config: [] }} />) !== '') throw new Error('an empty configuration block should render nothing at all')
  const html = renderToString(<FtdcConfig model={{ config: [{ level: 'ok', setting: 'x', why: 'y' }] }} />)
  if (!html.includes('Nothing here needs changing')) throw new Error('all-keep should say so')
  return 'ok'
})
check('ftdc summary: every advice level is styled', () => {
  for (const lvl of ['ok', 'warn', 'crit', 'info']) {
    if (!ADVICE_TEXT[lvl] || !ADVICE_FILL[lvl] || !ADVICE_TONE[lvl]) throw new Error(`advice ${lvl} unstyled`)
  }
  // The shaping helpers are the join between the backend's column layout and TimeChart's
  // row layout, and getting it wrong draws a chart that is silently all zeroes.
  const pts = chartPoints(ftdcModel.ts, ftdcModel.charts[0].series)
  if (pts.length !== 4) throw new Error(`want 4 points, got ${pts.length}`)
  if (pts[2].v.s0 !== 2 || pts[2].v.s1 !== 1) throw new Error('series values did not line up with their samples')
  const ln = chartLines(ftdcModel.charts[0].series)
  if (ln.length !== 2 || ln[0].key !== 's0') throw new Error('lines do not match series')
  if (chartPoints([], []).length !== 0) throw new Error('empty input should give no points')
  if (fmtSpan(0, 120) !== '2.0 min') throw new Error(`fmtSpan: ${fmtSpan(0, 120)}`)
  if (fmtNum(1500) !== '1.5k') throw new Error(`fmtNum: ${fmtNum(1500)}`)
  return 'ok'
})

if (failures > 0) {
  console.error(`\n${failures} render failure(s)`)
  process.exit(1)
}
console.log('\nall render checks passed')

// Every state the backend can put in a lane has to have a colour and a sentence here, or a
// PostgreSQL cluster renders lanes the legend cannot explain. This is the check that the
// two halves have not drifted: the list is the states the Go side emits.
check('log summary: every state the backend emits is styled and explained', () => {
  const emitted = [
    // Galera
    'SYNCED', 'JOINED', 'JOINER', 'DONOR', 'PRIMARY-COMP', 'OPEN', 'CLOSED',
    // Group Replication
    'ONLINE', 'RECOVERING', 'BLOCKED', 'ERROR', 'OFFLINE',
    // MongoDB
    'PRIMARY', 'SECONDARY', 'STARTUP2', 'ROLLBACK', 'ARBITER', 'REMOVED', 'ROUTING',
    // PostgreSQL
    'STANDBY', 'PROMOTING',
    // neither
    'RUNNING', 'STARTING', 'DOWN', 'UNKNOWN',
  ]
  for (const st of emitted) {
    if (!STATE_TEXT[st]) throw new Error(`state ${st} has no explanation`)
    if (!STATE_SEV[st]) throw new Error(`state ${st} has no severity`)
  }
  // PRIMARY is shared by MongoDB and PostgreSQL and must not carry Galera's meaning: a
  // Galera primary COMPONENT is a different idea with its own spelling.
  if (STATE_TEXT.PRIMARY.includes('component')) {
    throw new Error("PRIMARY still explains Galera's primary component")
  }
  if (STATE_SEV.PRIMARY !== 'ok') throw new Error('PRIMARY should read as serving')
  return 'ok'
})

check('log summary: each member kind is named beside its node', () => {
  for (const f of ['galera', 'grouprepl', 'mongors', 'mongos', 'pgstream', 'patroni']) {
    if (!FLAVOUR_LABEL[f]) throw new Error(`flavour ${f} has no label`)
  }
  // A plain server gets no badge — the engine name already said it.
  if (FLAVOUR_LABEL.postgres || FLAVOUR_LABEL.mongodb) {
    throw new Error('a standalone server should not be badged')
  }
  return 'ok'
})
