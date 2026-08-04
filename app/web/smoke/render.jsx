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
import { frameMemberSub, REPL_FRAME_TYPES } from '../src/pages/StackDesigner.jsx'
import MySQLManager from '../src/pages/MySQLManager.jsx'
import PacketInspector, {
  Timeline as PktTimeline, RangeControls as PktRangeControls, Filters as PktFilters,
  PacketList as PktList, PacketDetails as PktDetails, SummaryStrip as PktSummary,
  CaptureState as PktState, Pager as PktPager, ServerLogCard as PktServerLog,
  FilePick as PktFilePick,
} from '../src/pages/PacketInspector.jsx'
import { PORT_ROLE_TEXT, isSevereIssue } from '../src/lib/pktApi.js'
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

check('packet inspector: server error log (window mismatch)', () => {
  const html = renderToString(<PktServerLog onReload={noop}
    log={{ path: 'mysqld.log', source: 'upload', scanned: 40, inWindow: 0, mismatch: true,
      logFrom: 1785000000, logTo: 1785000600, windowFrom: 1785775330, windowTo: 1785775402,
      entries: [], top: [] }} />)
  if (!html.includes('none of them fall in this capture')) throw new Error('mismatch warning not shown')
  return html
})

if (failures > 0) {
  console.error(`\n${failures} render failure(s)`)
  process.exit(1)
}
console.log('\nall render checks passed')
