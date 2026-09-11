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
  insertTemplateDesign, groupTemplates, templateSizeLabel, menuEntriesFor, submenuPos, menuPos, menuWidth,
  MClusterAdminForm, MClusterAdminManager, MCA_DEFAULT_ADMIN_PW, MCA_DEFAULT_RO_PW,
  BuildImageRow,
  BigHoleForm, BigHoleManager, BigHoleLink,
  nodeCardTip, memberCardTip,
  k8sReplLinkable, ReplicationLinkForm, ReplicationLinkChoices,
  associationBlocked, isReplEdge, k8sReplRoleOf, K3D_SOURCE_EXPOSE, K8sToolFields,
  frameHeaderW, layoutFrame, separateFrames, SIM_NODE_TYPES, frameVersionLabel, frameSubLabel,
  podMenuEntries, POD_SHELLS, POD_CLIENTS,
  Spinner, NodeStatus, nodeConfiguring, configPhaseOf, PITRFields,
} from '../src/pages/StackDesigner.jsx'
import { ReplicationView } from '../src/pages/K3DManager.jsx'
import OperatorSummary, { Verdicts as OpVerdicts, Findings as OpFindings, Workloads as OpWorkloads, Pods as OpPods, CRs as OpCRs, Operators as OpOperators, Deployment as OpDeployment, Images as OpImages, Secrets as OpSecrets, Backups as OpBackups, Certs as OpCerts, Storage as OpStorage, Logs as OpLogs, Galera as OpGalera, PodSummaries as OpPodSummaries, BackupLogs as OpBackupLogs, Extras as OpExtras } from '../src/pages/OperatorSummary.jsx'
import { PageVisibleProvider, usePolling, usePageVisible } from '../src/lib/usePolling.jsx'
import { openTab, closeTab, tabCounts, clampTabs, TABS_DEFAULT, TABS_MIN, TABS_MAX } from '../src/lib/tabs.js'
import { mongoDownloadURL } from '../src/lib/stackApi.js'
import { TabCount, TabCapNotice, NAV } from '../src/App.jsx'
import { showExperimental, visible, visibleGroups } from '../src/lib/experimental.js'
import MySQLManager from '../src/pages/MySQLManager.jsx'
import OidcLoginGuide from '../src/components/OidcLoginGuide.jsx'
import SeaweedFSManager from '../src/pages/SeaweedFSManager.jsx'
import BackupGuide, { PgBackRestGuide, BarmanGuide } from '../src/components/BackupGuide.jsx'
import CRFormEditor, {
  crPatch, changedPaths, yamlish, matchField, getAt, setAt, delAt,
} from '../src/pages/CRFormEditor.jsx'
import K8sObjectEditor, {
  objectPatchOf, patchCount, reviewText, isMultiline, sizeLabel,
} from '../src/pages/K8sObjectEditor.jsx'
import { K8sBackupManager, sizeLabel as bkSizeLabel, whenLabel, crumbsOf } from '../src/pages/K8sBackupManager.jsx'
import PacketInspector, {
  Timeline as PktTimeline, RangeControls as PktRangeControls, Filters as PktFilters,
  PacketList as PktList, PacketDetails as PktDetails, SummaryStrip as PktSummary,
  CaptureState as PktState, Pager as PktPager, ServerLogCard as PktServerLog,
  FilePick as PktFilePick,
} from '../src/pages/PacketInspector.jsx'
import { PORT_ROLE_TEXT, MONGO_KIND_TEXT, isSevereIssue } from '../src/lib/pktApi.js'
import OperatorDebugger, {
  PanelMaximize as DbgMaximize,
  Header as DbgHeader, NoTargets as DbgNoTargets, SessionBanner as DbgBanner,
  QuickBreakpoints as DbgQuick, BreakpointList as DbgBreakpoints, FileTree as DbgFiles,
  SourceView as DbgSource, CallStack as DbgStack, Variables as DbgVars,
  WatchBox as DbgWatch, EventLog as DbgLog,
  VarNode as DbgVarRow, varIsSummarised as __varIsSummarised,
} from '../src/pages/OperatorDebugger.jsx'
import { goHighlight, TOKEN_CLS, STATUS_TONE, STATUS_TEXT, shortFrameName } from '../src/lib/debugApi.js'
import Api, {
  Tokens as ApiTokens, CreateToken as ApiCreateToken, TokenTable as ApiTokenTable,
  FreshSecret as ApiFreshSecret, Endpoints as ApiEndpoints, EndpointRow as ApiEndpointRow,
  Snippet as ApiSnippet, GettingStarted as ApiGettingStarted, AdminTokens as ApiAdminTokens,
  CliCommands as ApiCliCommands, CliHelpText as ApiCliHelpText,
} from '../src/pages/Api.jsx'
import WhatsNew, { WhatsNewLink } from '../src/components/WhatsNew.jsx'
import { ChangePassword as SettingsChangePassword, LookOptions, TabLimit, TooltipOptions, TOOLTIP_MODES } from '../src/pages/Settings.jsx'
import { LOOKS, THEMES } from '../src/theme/ThemeProvider.jsx'
import {
  curlFor, cliFor, matches as epMatches, samplePath, expiryText, relDate,
  METHOD_TONE, SCOPE_TEXT, MEDIA_TEXT, MEDIA_LABEL, TOKEN_STATE_TONE, EXPIRY_CHOICES,
} from '../src/lib/apiApi.js'
import CoreDumpAnalyzer, {
  Header as GdbHeader, NoTargets as GdbNoTargets, CrashSummary as GdbSummary,
  CoreList as GdbCores, ThreadList as GdbThreads, Backtrace as GdbStack,
  FrameVars as GdbVars, EvaluateBox as GdbEval, ConsoleBox as GdbConsole,
  SourceView as GdbSource, GdbRecipe,
} from '../src/pages/CoreDumpAnalyzer.jsx'
import {
  crashSummary, shortFunc, sourceOf, isSystemFrame, formatBytes,
  GDB_STATUS_TONE, GDB_STATUS_TEXT,
} from '../src/lib/gdbApi.js'
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
import { Field, InfoRow } from '../src/components/ui.jsx'
import { Help, Hint, place } from '../src/components/Tooltip.jsx'
import { SettingsCtx } from '../src/settings/SettingsProvider.jsx'
import * as nodeFs from 'node:fs'
import { HELP, MENU_HELP, TOOL_HELP, DEP_HELP, MORE_HELP, FTDC_HELP, nodeHelp } from '../src/lib/help.js'
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

// The deployed-node surface: a manager's KV rows are the "what do I do with this value"
// question the tooltips were added for, and they are the half that cannot be checked by
// hovering a draft stack in a browser — it has no running containers. Rendering over a
// captured real deployment covers them instead.
check('tooltip: a deployed node\'s panel explains its rows', () => {
  const [nodeId, dep] = Object.entries(realDeps)[0]
  const html = renderToString(
    <TerminalProvider>
      <MySQLManager stackId={1} nodeId={nodeId} dep={dep} onDeleteNode={noop} />
    </TerminalProvider>,
  )
  // Only the trigger is in the markup: the bubble is portalled on open, which SSR never
  // does. That the trigger is there at all means `help` reached Help with a real string
  // — it renders nothing at all for an undefined one, which is what the dangling-
  // reference check above exists to catch.
  const triggers = (html.match(/What is this\?/g) || []).length
  if (triggers < 5) throw new Error(`only ${triggers} help triggers on a deployed panel`)
  return `${triggers} help triggers`
})

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

// The PostgreSQL guide has the same job as the Percona Server one: the roles exist on the node
// after deploy, so the panel names them and shows the password rather than printing a placeholder
// username and leaving the reader to find KEYCLOAK_USER_PASSWORD themselves.
check('OidcLoginGuide (pg): names the roles that exist and their password', () => {
  const info = {
    enabled: true, realm: 'dbcanvas', clientId: 'postgres',
    issuer: 'https://keycloak.example.net:8443/realms/dbcanvas',
    consoleUrl: 'https://keycloak.example.net:8443',
    nodeFqdn: 'pg1.example.net', users: ['jane', 'john'],
  }
  const guide = renderToString(<OidcLoginGuide engine="pg" info={info} secrets={{ oidcSamplePassword: 'keycloak_user_password' }} />)
  for (const want of ['pg_oidc_validator', 'jane, john', 'https://keycloak.example.net:8443', 'oauth_issuer=', 'pg1.example.net']) {
    if (!guide.includes(want)) throw new Error(`login guide omits ${want}`)
  }
  if (guide.includes('undefined')) throw new Error('login guide rendered a literal "undefined"')
  return guide
})

// A node deployed before the roles were created has no users in its config: the guide still has
// to render, falling back to the sample name rather than printing an empty list.
check('OidcLoginGuide (pg): an older deployment without users still renders', () => {
  const info = { enabled: true, realm: 'dbcanvas', clientId: 'postgres', issuer: 'https://kc/realms/dbcanvas', nodeFqdn: 'pg1.example.net' }
  const guide = renderToString(<OidcLoginGuide engine="pg" info={info} />)
  if (!guide.includes('user=jane')) throw new Error('the fallback username is gone')
  if (guide.includes('undefined')) throw new Error('login guide rendered a literal "undefined"')
  return guide
})

// --- backup guides ----------------------------------------------------------

// The point of the guide is that a command can be pasted as it stands, so the check is that
// the deployment's own facts reached it: no <stanza>, no <bucket>, no "undefined".
check('BackupGuide (pgbackrest): commands carry the stanza and the unit', () => {
  const cfg = {
    usePgBackRest: true, backupStanza: 'pg-01', backupBucket: 'backup2',
    backupRepo: 'pgbackrest → SeaweedFS S3 (backup2/pgbackrest)',
    service: 'postgresql-18', dataDir: '/var/lib/pgsql/18/data', hostname: 'pg-01',
  }
  if (!renderToString(<BackupGuide engine="pgbackrest" cfg={cfg} nodeLabel="pg-01" />).includes('How to back up and restore')) {
    throw new Error('the guide has no heading')
  }
  const html = renderToString(<PgBackRestGuide cfg={cfg} nodeLabel="pg-01" />)
  for (const want of [
    'pgbackrest --stanza=pg-01 info', 'pgbackrest --stanza=pg-01 check',
    'pgbackrest --stanza=pg-01 --type=full backup', 'expire --set=',
    'systemctl stop postgresql-18', '--type=time --target=',
  ]) {
    if (!html.includes(want)) throw new Error(`the pgBackRest guide omits ${want}`)
  }
  if (html.includes('&lt;stanza&gt;') || html.includes('undefined')) throw new Error('a placeholder survived into a command')
  return html
})

// The guide's body only renders once opened, so the commands themselves are checked on the
// inner components through the same props the tab passes.
check('BackupGuide (barman): every command names the endpoint, bucket and server', () => {
  const cfg = {
    useBarman: true, cluster: 'repmgr-cluster-01', backupServer: 'repmgr-cluster-01',
    backupBucket: 'backup2', backupEndpoint: 'https://seaweedfs-01.example.net:8333',
    backupS3Url: 's3://backup2/barman/repmgr-cluster-01', service: 'postgresql-18',
    dataDir: '/var/lib/pgsql/18/data',
  }
  const html = renderToString(<BarmanGuide cfg={cfg} nodeLabel="repmgr-cluster-01" />)
  for (const want of [
    'barman-cloud-backup-list --cloud-provider aws-s3 --endpoint-url https://seaweedfs-01.example.net:8333 s3://backup2/barman/repmgr-cluster-01 repmgr-cluster-01',
    'barman-cloud-backup-delete', '--backup-id', '--retention-policy',
    'barman-cloud-restore', 'barman-cloud-wal-restore', '/var/lib/pgsql/18/data',
  ]) {
    if (!html.includes(want)) throw new Error(`the barman guide omits ${want}`)
  }
  if (html.includes('undefined') || html.includes('&lt;bucket&gt;')) throw new Error('a placeholder survived into a command')
  return html
})

// --- SeaweedFS --------------------------------------------------------------

// The Buckets tab is the way into the bucket file manager, and it is no longer read-only.
check('SeaweedFSManager: the Buckets tab opens the file manager', () => {
  const dep = {
    state: 'running',
    config: {
      hostname: 'sw1', fqdn: 'sw1.example.net', bucket: 'backups', buckets: ['backups', 'dumps'],
      accessKey: 'dbcanvas', region: 'us-east-1', webPort: 18080,
      internalEndpoint: 'https://sw1.example.net:8333', tls: true,
    },
    secrets: { accessKey: 'dbcanvas', secretKey: 's3cret' },
  }
  const html = renderToString(<SeaweedFSManager stackId={1} nodeId="sw1" dep={dep} onDeleteNode={noop} />)
  if (!html.includes('Buckets')) throw new Error('the Buckets tab is missing')
  return html
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

// --- cross-cluster replication between two Kubernetes clusters (app/k3drepl.go) ---

// The one frame-to-frame line on the canvas. Its rule has to stay narrow: two DIFFERENT frames,
// both Kubernetes, both running the PXC operator — the only operator whose custom resource can
// replicate from another cluster. Every other pairing is a line that would deploy into nothing.
check('a Kubernetes replication link needs two different PXC-operator clusters', () => {
  const k = (id, operator) => ({ id, type: 'k3d', k3dOperator: operator })
  const a = k('f1', 'pxc')
  const b = k('f2', 'pxc')
  if (!k8sReplLinkable(a, b)) throw new Error('two PXC-operator clusters must be linkable')
  if (k8sReplLinkable(a, a)) throw new Error('a cluster must not link to itself')
  if (k8sReplLinkable(a, k('f2', 'psmdb'))) throw new Error('only the PXC operator replicates cross-cluster')
  if (k8sReplLinkable(a, k('f2', ''))) throw new Error('a frame with no operator has no cluster to replicate')
  if (k8sReplLinkable(a, { id: 'f3', type: 'pxc' })) throw new Error('a bare-metal PXC frame is not a Kubernetes cluster')
  if (k8sReplLinkable(a, null)) throw new Error('a missing frame is not linkable')
  return 'ok'
})

const k8sFrames = [
  { id: 'kf1', type: 'k3d', label: 'cluster1', k3dOperator: 'pxc' },
  { id: 'kf2', type: 'k3d', label: 'cluster2', k3dOperator: 'pxc' },
]
const k8sEdge = { id: 'e1', type: 'async', from: { node: 'kf1', port: 'right' }, to: { node: 'kf2', port: 'left' } }

// Both dialogs now take either two member nodes or two Kubernetes frames, and read their labels
// from a different list in each case. A frame endpoint looked up in `nodes` is undefined, which is
// how this used to render "node → node" with no way to tell the clusters apart.
check('the replication dialogs name Kubernetes clusters, and drop bidirectional', () => {
  const html = renderToString(
    <ReplicationLinkChoices prompt={{ e1: k8sEdge.from, e2: k8sEdge.to, kind: 'k8s' }}
      nodes={[]} frames={k8sFrames} onClose={noop} onChoose={noop} />)
  if (!html.includes('cluster1') || !html.includes('cluster2')) throw new Error('the cluster labels are missing')
  if (html.includes('↔')) throw new Error('bidirectional must not be offered between Kubernetes clusters')
  return html
})

check('the replication link form reads a frame endpoint', () => {
  const html = renderToString(
    <ReplicationLinkForm ed={k8sEdge} nodes={[]} frames={k8sFrames} patchEdge={noop} deleteEdge={noop} />)
  if (!html.includes('cluster1') || !html.includes('cluster2')) throw new Error('the cluster labels are missing')
  if (html.includes('bidirectional')) throw new Error('bidirectional must not be offered between Kubernetes clusters')
  return html
})

// ---- a replication link must not consume a cluster's association budget ----
// The live bug: a Stock Market Sim node could be attached to the REPLICA of a Kubernetes
// replication pair but not to its SOURCE. The replication edge is stored source →
// replica, the cardinality guard counted it, and so the source had already spent its one
// outgoing edge. Assert both ends now accept an application link, and that the guards
// still hold for the association edges they are actually about.
check('a replication link leaves both clusters connectable', () => {
  const sim = { id: 'ss1' }
  for (const end of ['kf1', 'kf2']) {
    const why = associationBlocked([k8sEdge], end, sim.id, { singleOutgoing: true })
    if (why) throw new Error(`${end} refused an app link: ${why}`)
  }
  // The guards themselves, on association edges, are unchanged.
  const assoc = { id: 'e2', type: 'directional', from: { node: 'kf1' }, to: { node: 'ss1' } }
  if (!associationBlocked([assoc], 'kf2', 'ss1', {})) throw new Error('a simulator must take only one incoming link')
  if (!associationBlocked([assoc], 'kf1', 'ss2', { singleOutgoing: true })) throw new Error('singleOutgoing must still bound the source')
  if (associationBlocked([assoc], 'kf1', 'ss2', {})) throw new Error('without singleOutgoing a second outgoing link is fine')
  if (!associationBlocked([assoc], 'ss1', 'kf1', {})) throw new Error('one line per pair, whichever way it was drawn')
  if (!isReplEdge(k8sEdge) || isReplEdge(assoc)) throw new Error('isReplEdge must tell the two kinds of line apart')
  return 'ok'
})

// A source's database Service is not the frame's choice — a replica in another cluster
// cannot dial a ClusterIP. k8sReplRoleOf is what the panel and the correcting effect both
// read, so it has to agree with the stored edge direction.
check('a Kubernetes replication source is known from the edge, and exposed', () => {
  if (k8sReplRoleOf('kf1', [k8sEdge], k8sFrames) !== 'source') throw new Error('the from end is the source')
  if (k8sReplRoleOf('kf2', [k8sEdge], k8sFrames) !== 'replica') throw new Error('the to end is the replica')
  if (k8sReplRoleOf('kf3', [k8sEdge], k8sFrames) !== '') throw new Error('an unrelated frame has no role')
  const assoc = { id: 'e3', type: 'directional', from: { node: 'kf1' }, to: { node: 'ss1' } }
  if (k8sReplRoleOf('kf1', [assoc], k8sFrames) !== '') throw new Error('an association line makes nobody a source')
  if (K3D_SOURCE_EXPOSE !== 'loadbalancer') throw new Error('a source must be exposed on a LoadBalancer')
  return 'ok'
})

// ---- a node that is running but not finished spins ----
// A k3s node reports running the moment cr.yaml is applied, and a replica cluster stays
// running through its whole seed restore. Both used to show the green dot that means ready.
check('a node still being configured spins instead of showing ready', () => {
  const ring = { state: 'provisioning', progress: { percent: 40 } }
  const busy = { state: 'running', progress: { configuring: true, configPhase: 'seeding the replica from a backup of the source' } }
  const ready = { state: 'running', progress: { percent: 100 } }
  const stopped = { state: 'stopped', progress: {} }
  if (!nodeConfiguring(busy)) throw new Error('a running node with the marker set is being configured')
  if (nodeConfiguring(ready)) throw new Error('a plain running node is ready')
  if (nodeConfiguring(ring)) throw new Error('provisioning keeps its progress ring — it carries a real number')
  if (nodeConfiguring({ state: 'stopped', progress: { configuring: true } })) throw new Error('only a running node spins')
  if (!configPhaseOf(busy).includes('seeding')) throw new Error('the phase has to reach the tooltip')
  if (configPhaseOf(ready) !== '') throw new Error('a ready node has no phase')
  // The three states have to render, and to differ.
  const spin = renderToString(<NodeStatus dep={busy} />)
  if (!spin.includes('animate-spin')) throw new Error('a configuring node must show a spinner')
  if (renderToString(<NodeStatus dep={ready} />).includes('animate-spin')) throw new Error('a ready node must not spin')
  if (renderToString(<NodeStatus dep={ring} />).includes('animate-spin')) throw new Error('provisioning must keep the ring')
  if (!renderToString(<NodeStatus dep={stopped} />).includes('stopped')) throw new Error('a stopped node keeps its word')
  return renderToString(<Spinner size={16} />)
})

// ---- point-in-time recovery ----
// The fields, and the sentence that says a replica's collector starts switched off.
check('PITR fields: the bucket defaults to the backups, and a replica says it waits', () => {
  const sw = { id: 'sw1', type: 'seaweedfs', label: 'seaweedfs-01', buckets: ['pxc-backups', 'pxc-binlogs'], bucket: 'pxc-backups' }
  const frame = { id: 'kf1', type: 'k3d', label: 'cluster1', k3dOperator: 'pxc', seaweedfsNodeId: 'sw1', k3dPitr: true }
  const on = renderToString(<PITRFields f={frame} nodes={[sw]} patchFrame={noop} deployed={false} />)
  if (!on.includes('same as backups (pxc-backups)')) throw new Error('the default binlog bucket is the backup bucket')
  if (!on.includes('pxc-binlogs')) throw new Error('a spare bucket has to be offered for the binlogs')
  if (on.includes('deploys with the collector')) throw new Error('a cluster with no replication link is not deferred')
  const asReplica = renderToString(<PITRFields f={frame} nodes={[sw]} patchFrame={noop} deployed={false} replRole="replica" />)
  if (!asReplica.includes('deploys with the collector')) throw new Error('a replica must say its collector starts off')
  // Off, and with no store: the checkbox explains itself instead of offering a bucket.
  const noStore = renderToString(<PITRFields f={{ id: 'kf2', type: 'k3d' }} nodes={[]} patchFrame={noop} deployed={false} />)
  if (!noStore.includes('Needs a SeaweedFS backup store')) throw new Error('PITR without a store has to say why it is unavailable')
  if (noStore.includes('Binlog bucket')) throw new Error('no store means no bucket picker')
  return 'ok'
})

// ---- a frame is wide enough for its own title ----
// A one-member frame used to be 144px, and its header — icon, two lines, ± buttons — was
// clipped to "clust…" / "k3s 1.3…". The name is the whole point of the card, so the frame
// is sized to fit it.
check('a frame fits its own header text', () => {
  const f = { id: 'kf1', type: 'k3d', label: 'k3d-cluster-00', k3dOperator: 'pxc', x: 0, y: 0 }
  const one = layoutFrame(f, [{ id: 'n1', frameId: 'kf1' }])
  // Both header lines have to fit inside the box, not just the members.
  const designSub = `${frameVersionLabel(f)} · 1 node`
  if (one.frame.w < frameHeaderW(f, 1)) throw new Error('the header does not fit the frame')
  if (one.frame.w <= 144) throw new Error(`a one-node frame is still member-sized (${one.frame.w}px) — the title would clip`)
  if (designSub.length < 10) throw new Error('the description line went missing')
  // Members stay inside, and centred.
  const [m] = one.nodes
  if (m.x < one.frame.x || m.x + 116 > one.frame.x + one.frame.w) throw new Error('the member fell outside its frame')
  // Three members are wider than any title, so the members set the width there.
  const three = layoutFrame(f, [{ id: 'n1' }, { id: 'n2' }, { id: 'n3' }])
  if (three.frame.w !== 14 * 2 + 3 * 116 + 2 * 12) throw new Error('a full frame must keep its member-derived width')
  // A pathological name is bounded rather than dragging a 900px box across the canvas.
  const long = layoutFrame({ ...f, label: 'x'.repeat(400) }, [{ id: 'n1' }])
  if (long.frame.w > 380) throw new Error('the frame width is unbounded')

  // Deployed, the header stops showing the design-time description and shows the version it
  // is running — which is much shorter. The box has to follow it down, or a deployed cluster
  // sits in a box sized for a sentence it is no longer displaying.
  const members = [{ id: 'n1', type: 'k3d', frameId: 'kf1' }]
  const deps = { n1: { state: 'running', config: { serverVersion: '1.36.4+k3s1' } } }
  const sub = frameSubLabel(f, members, deps)
  if (!sub.startsWith('k3s 1.36.4+k3s1')) throw new Error(`the deployed line is the version: ${sub}`)
  const deployed = layoutFrame(f, members, sub)
  if (deployed.frame.w >= one.frame.w) {
    throw new Error(`a deployed frame must shrink to its shorter line (${deployed.frame.w}px vs ${one.frame.w}px)`)
  }
  if (deployed.frame.w < frameHeaderW(f, 1, sub)) throw new Error('the deployed header must still fit')
  // The header and the layout must read the same line — they were two expressions saying the
  // same thing, which is how the box came to be sized for text the header was not showing.
  if (frameSubLabel(f, members, {}) === sub) throw new Error('an undeployed frame shows the design-time description')
  return `1 node: ${one.frame.w}px, deployed: ${deployed.frame.w}px, 3 nodes: ${three.frame.w}px`
})

// Frames now grow to fit their titles, which on an existing design can push one frame over
// the one beside it — two overlapping title bars, worse than the clipping being fixed. The
// repair runs once, on load, so it has to be right the first time.
check('a frame that grew is pushed clear of its neighbour', () => {
  // Two frames placed side by side when both were 144px wide, now 362 each: the second sits
  // across the first.
  const grown = [
    { id: 'f1', x: 0, y: 0, w: 362, h: 108 },
    { id: 'f2', x: 180, y: 0, w: 362, h: 108 },
  ]
  const [shift, ...rest] = separateFrames(grown)
  if (!shift || shift.id !== 'f2') throw new Error('the right-hand frame is the one that moves')
  if (180 + shift.dx < 362 + 24) throw new Error(`the frames still overlap after a ${shift.dx}px shift`)
  if (rest.length) throw new Error('one overlap, one shift')
  // Idempotent: applying the shifts and running again must find nothing left to do. This is
  // what makes it safe to run on every load rather than once.
  const fixed = grown.map((f) => (f.id === shift.id ? { ...f, x: f.x + shift.dx } : f))
  if (separateFrames(fixed).length) throw new Error('the repair must converge in one pass')
  // Nothing to do when they clear each other.
  if (separateFrames([{ id: 'f1', x: 0, y: 0, w: 100, h: 100 }, { id: 'f2', x: 400, y: 0, w: 100, h: 100 }]).length) {
    throw new Error('frames that clear each other must be left alone')
  }
  // Stacked vertically, they never overlap however wide they get.
  if (separateFrames([{ id: 'f1', x: 0, y: 0, w: 362, h: 100 }, { id: 'f2', x: 0, y: 300, w: 362, h: 100 }]).length) {
    throw new Error('a frame below another is not overlapping it')
  }
  // A chain: three frames, each pushing the next, resolved in one pass.
  const three = separateFrames(
    [{ id: 'a', x: 0, y: 0, w: 300, h: 100 }, { id: 'b', x: 150, y: 0, w: 300, h: 100 }, { id: 'c', x: 300, y: 0, w: 300, h: 100 }],
  )
  if (three.length !== 2) throw new Error(`a chain of three needs two shifts, got ${three.length}`)
  return `shift ${shift.dx}px`
})

// The simulators are what get the "app connection" caption on their association line.
check('the application simulators are named as such', () => {
  for (const t of ['stocksim', 'airlinesim', 'carsim', 'hotelsim', 'trafficsim', 'marketchaos']) {
    if (!SIM_NODE_TYPES.has(t)) throw new Error(`${t} is a simulator and must carry the app-connection caption`)
    if (!NODE_TYPES[t]?.ports) throw new Error(`${t} needs connection ports for its line to exist`)
  }
  // The two display-only panels draw no line at all, so they are not simulators here.
  for (const t of ['bighole', 'mclusteradmin']) {
    if (SIM_NODE_TYPES.has(t)) throw new Error(`${t} carries no association line`)
  }
  return 'ok'
})

// The member-to-member shape has to keep working unchanged — it is the same two components.
check('the replication dialogs still name cluster members', () => {
  const memberNodes = [
    { id: 'n1', type: 'pxc', label: 'pxc-01', frameId: 'f1' },
    { id: 'n2', type: 'pxc', label: 'pxc-02', frameId: 'f2' },
  ]
  const memberFrames = [{ id: 'f1', type: 'pxc', label: 'clusterA' }, { id: 'f2', type: 'pxc', label: 'clusterB' }]
  const ed = { id: 'e2', type: 'async', from: { node: 'n1', port: 'right' }, to: { node: 'n2', port: 'left' } }
  const modal = renderToString(
    <ReplicationLinkChoices prompt={{ e1: ed.from, e2: ed.to }} nodes={memberNodes} frames={memberFrames} onClose={noop} onChoose={noop} />)
  if (!modal.includes('pxc-01') || !modal.includes('clusterA')) throw new Error('the member and its cluster must both show')
  if (!modal.includes('↔')) throw new Error('bidirectional is still offered between cluster members')
  const form = renderToString(
    <ReplicationLinkForm ed={ed} nodes={memberNodes} frames={memberFrames} patchEdge={noop} deleteEdge={noop} />)
  if (!form.includes('bidirectional')) throw new Error('bidirectional is still offered between cluster members')
  return modal + form
})

// Every state the Replication tab can be in. `running` has three values, not two — absent means
// "nothing to read yet", and rendering that as "stopped" would be a lie about a healthy cluster.
check('the Replication tab renders every state', () => {
  const cases = [
    { isServer: false },
    { isServer: true, err: 'cluster is not running' },
    { isServer: true },
    { isServer: true, view: { role: '' } },
    { isServer: true, view: { role: 'source', channel: 'cluster1_to_cluster2', cluster: 'cluster1', peer: 'cluster2', exposed: ['172.20.255.248'] } },
    { isServer: true, view: { role: 'replica', channel: 'cluster1_to_cluster2', cluster: 'cluster2', peer: 'cluster1', sources: ['172.20.255.248'], seededFrom: 's3://backup1/cluster1-full', running: true } },
    { isServer: true, view: { role: 'replica', channel: 'cluster1_to_cluster2', cluster: 'cluster2', peer: 'cluster1', running: false, ioRunning: 'Connecting', sqlRunning: 'Yes', lastError: 'error connecting to source' } },
    { isServer: true, view: { role: 'replica', channel: 'cluster1_to_cluster2', cluster: 'cluster2', peer: 'cluster1' }, note: 're-seeding cluster2' },
  ]
  return cases.map((c) => renderToString(
    <ReplicationView view={c.view} err={c.err} note={c.note} busy={false} isServer={c.isServer} onRefresh={noop} onReseed={noop} />)).join('')
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

// ---- Operator Debugger -----------------------------------------------------
//
// The page renders before any socket exists (SSR runs no effects), which is exactly the
// state a user sees for the first second — and the state where a missing icon would blank
// the whole page.

const dbgTarget = {
  stackId: 1, frameId: 'f1', stackName: 'delve-verify', label: 'k3d-01',
  operator: 'pxc', operatorVer: '1.20.0', cr: 'k3d-01', namespace: 'pxc',
  buildDir: '/go/src/github.com/percona/percona-xtradb-cluster-operator',
  hostPort: 40000, nodePort: 30400, debugStatus: 'listening',
  startFile: 'pkg/controller/pxc/controller.go',
  presets: [
    { label: 'Reconcile', func: 'pxc.(*ReconcilePerconaXtraDBCluster).Reconcile', hint: 'the main loop' },
    { label: 'deploy', func: 'pxc.(*ReconcilePerconaXtraDBCluster).deploy', hint: 'the StatefulSets' },
  ],
}
const dbgState = {
  status: 'stopped', reason: 'breakpoint', threadId: 226,
  frames: [
    { id: 1000, name: 'pxc.(*ReconcilePerconaXtraDBCluster).Reconcile', file: 'pkg/controller/pxc/controller.go', line: 237, hasSource: true },
    { id: 1001, name: 'controller.(*Controller).reconcileHandler', file: '/go/pkg/mod/sigs.k8s.io/controller-runtime@v0.24.1/pkg/internal/controller/controller.go', line: 478, hasSource: false },
  ],
  breakpoints: [{ file: 'pkg/controller/pxc/controller.go', line: 237, verified: true }],
  functions: [{ name: 'pxc.(*ReconcilePerconaXtraDBCluster).Reconcile', verified: true, line: 236 }],
  allowCalls: false, idleSeconds: 300, subscribers: 1, target: dbgTarget,
}
const dbgSource = {
  path: 'pkg/controller/pxc/controller.go',
  buildPath: dbgTarget.buildDir + '/pkg/controller/pxc/controller.go',
  content: `package pxc

/* the reconcile loop */
func (r *ReconcilePerconaXtraDBCluster) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
\tlog := logf.FromContext(ctx)      // a comment
\to := &api.PerconaXtraDBCluster{}
\tif err := r.client.Get(ctx, request.NamespacedName, o); err != nil {
\t\treturn reconcile.Result{}, err
\t}
\treturn rr, nil
}
`,
}

check('OperatorDebugger page (before the socket opens)', () => renderToString(<OperatorDebugger />))
check('operator debugger: header', () =>
  renderToString(<DbgHeader targets={[dbgTarget]} value="1/f1" onChange={noop} state={dbgState}
    busy="" onAttach={noop} onDetach={noop} onReconcile={noop} />))
check('operator debugger: nothing to debug', () => renderToString(<DbgNoTargets />))
check('operator debugger: the banner in every state', () => {
  for (const status of ['running', 'stopped', 'detached', 'attaching']) {
    renderToString(<DbgBanner state={{ ...dbgState, status }} target={dbgTarget} />)
  }
  return 'ok'
})
check('operator debugger: quick breakpoints', () =>
  renderToString(<DbgQuick target={dbgTarget} state={dbgState} onToggle={noop} />))
check('operator debugger: breakpoint list counts function breakpoints too', () => {
  const html = renderToString(<DbgBreakpoints state={dbgState} onOpen={noop} onRemove={noop}
    onRemoveFn={noop} onClear={noop} />)
  // One source breakpoint and one quick breakpoint: a panel that says (1) while the operator
  // is stopped at the other one reads as a broken debugger.
  if (!html.includes('Breakpoints (2)')) throw new Error('function breakpoints are not counted')
  return html
})
check('operator debugger: file tree', () =>
  renderToString(<DbgFiles files={['cmd/manager/main.go', 'pkg/controller/pxc/controller.go']}
    value="pkg/controller/pxc/controller.go" filter="" onFilter={noop} onOpen={noop} />))
check('operator debugger: source with a breakpoint and the current line', () => {
  const html = renderToString(<DbgSource path={dbgSource.path} source={dbgSource} state={dbgState}
    stoppedLine={237} stoppedFile={dbgSource.path} onToggle={noop} onStep={noop} busy="" />)
  if (!html.includes('Continue') || !html.includes('Step over')) {
    throw new Error('the stepping toolbar is missing')
  }
  return html
})
check('operator debugger: call stack (with a frame that has no source)', () =>
  renderToString(<DbgStack frames={dbgState.frames} selected={1000} onSelect={noop} />))
check('operator debugger: variables', () =>
  renderToString(<DbgVars scopes={[{ name: 'Locals', variablesReference: 1000, expensive: false }]}
    stopped onExpand={() => Promise.resolve([])} onSet={() => Promise.resolve({})}
    onFull={() => Promise.resolve('')} />))

check('operator debugger: a value is never clipped, and is editable when Delve can name it', () => {
  // SSR runs no effects, so render the rows directly rather than through a scope's fetch.
  const settable = { name: 'Name', value: '"k3d-dbg"', type: 'string', evaluateName: 'request.NamespacedName.Name', variablesReference: 0 }
  const generated = { name: '~r0', value: 'reconcile.Result {Requeue: true, ...}', variablesReference: 1004 }
  const html = renderToString(
    <DbgVarRow v={settable} depth={0} containerRef={1003}
      onExpand={() => Promise.resolve([])} onSet={() => Promise.resolve({})} onFull={() => Promise.resolve('')} />)
  if (html.includes('truncate')) throw new Error('a value is still clipped by CSS')
  if (!html.includes('Set request.NamespacedName.Name')) throw new Error('a nameable variable offers no edit')

  // Delve refuses to set what it cannot name, so the row must not pretend otherwise.
  const noName = renderToString(
    <DbgVarRow v={generated} depth={0} containerRef={1000}
      onExpand={() => Promise.resolve([])} onSet={() => Promise.resolve({})} onFull={() => Promise.resolve('')} />)
  if (noName.includes('Set ')) throw new Error('a compiler-generated row offers an edit that cannot work')
  // ...but a summarised value still offers the whole thing — except that too needs a name.
  const summarised = renderToString(
    <DbgVarRow v={{ ...generated, evaluateName: 'rr' }} depth={0} containerRef={1000}
      onExpand={() => Promise.resolve([])} onSet={() => Promise.resolve({})} onFull={() => Promise.resolve('')} />)
  if (!summarised.includes('show all')) throw new Error('a summarised value offers no way to see all of it')
  return 'ok'
})

check('operator debugger: what counts as a summarised value', () => {
  const cut = ['reconcile.Result {Requeue: true, ...}', '"a long string"...', '[]int len: 300, cap: 300, [1,2,...]']
  const whole = ['"k3d-dbg"', 'true', '6534380408448', 'types.NamespacedName {Namespace: "pxc", Name: "k3d-dbg"}']
  for (const v of cut) if (!__varIsSummarised(v)) throw new Error(`${v} should read as summarised`)
  for (const v of whole) if (__varIsSummarised(v)) throw new Error(`${v} should read as complete`)
  return 'ok'
})
check('operator debugger: watches', () =>
  renderToString(<DbgWatch value="" onChange={noop} onAdd={noop}
    watches={[{ expr: 'request.NamespacedName', value: 'types.NamespacedName {Namespace: "pxc"}' }]}
    onRemove={noop} allowCalls={false} onAllowCalls={noop} idle={300} onIdle={noop} />))
check('operator debugger: event log', () =>
  renderToString(<DbgLog lines={[{ at: new Date().toISOString(), kind: 'info', text: 'attached' }]} />))

check('operator debugger: a panel maximizes and docks back', () => {
  // Without a provider there is no maximize button at all — the panels are rendered on their
  // own here and in the node panel, and a dead button would be worse than none.
  const bare = renderToString(<DbgStack frames={dbgState.frames} selected={1000} onSelect={noop} />)
  if (bare.includes('aria-label="Maximize"')) throw new Error('a maximize button with nothing to maximize')

  const docked = renderToString(
    <DbgMaximize value={null} onChange={noop}>
      <DbgStack frames={dbgState.frames} selected={1000} onSelect={noop} />
    </DbgMaximize>)
  if (!docked.includes('aria-label="Maximize"')) throw new Error('no maximize button inside a provider')

  const maxed = renderToString(
    <DbgMaximize value="stack" onChange={noop}>
      <DbgStack frames={dbgState.frames} selected={1000} onSelect={noop} />
    </DbgMaximize>)
  if (!maxed.includes('aria-label="Dock back"')) throw new Error('a maximized panel still offers Maximize')
  // The sizing classes have to go with it, or "maximized" is a tall panel in an empty page.
  if (maxed.includes('max-h-52')) throw new Error('a maximized panel kept its height cap')
  if (!maxed.includes('absolute inset-0')) throw new Error('a maximized panel does not cover the workspace')

  // The source view is a panel in everything but name, and maximizes the same way.
  const src = renderToString(
    <DbgMaximize value="source" onChange={noop}>
      <DbgSource path={dbgSource.path} source={dbgSource} state={dbgState} stoppedLine={237}
        stoppedFile={dbgSource.path} onToggle={noop} onStep={noop} busy="" />
    </DbgMaximize>)
  if (!src.includes('absolute inset-0')) throw new Error('the source view does not maximize')
  return 'ok'
})

check('operator debugger: every session status has a tone and a word', () => {
  for (const st of ['detached', 'attaching', 'running', 'stopped']) {
    if (!STATUS_TONE[st] || !STATUS_TEXT[st]) throw new Error(`status ${st} is not described`)
  }
  return 'ok'
})

check('operator debugger: the Go highlighter survives real source', () => {
  const lines = goHighlight(dbgSource.content)
  if (lines.length !== dbgSource.content.split('\n').length) {
    throw new Error('the highlighter lost or invented lines')
  }
  // Every token must round-trip: colouring must never change what the code says.
  const rebuilt = lines.map((toks) => toks.map(([, text]) => text).join('')).join('\n')
  if (rebuilt !== dbgSource.content) throw new Error('the highlighter changed the source text')
  for (const toks of lines) {
    for (const [cls] of toks) {
      if (TOKEN_CLS[cls] === undefined) throw new Error(`token class ${cls} has no colour`)
    }
  }
  if (shortFrameName('sigs.k8s.io/controller-runtime/pkg/internal.(*C).Reconcile') !== 'internal.(*C).Reconcile') {
    throw new Error('a frame name is not shortened to its package')
  }
  // A generic frame's type argument carries slashes of its own; trimming at the last slash
  // without removing it first leaves "types.NamespacedName }]).Reconcile", which names nothing.
  const generic = 'controller.(*Controller[go.shape.struct { k8s.io/apimachinery/pkg/types.NamespacedName }]).Reconcile'
  if (shortFrameName(generic) !== 'controller.(*Controller).Reconcile') {
    throw new Error(`a generic frame name is mangled: ${shortFrameName(generic)}`)
  }
  return 'ok'
})

// ---- Core Dump Analyzer ----------------------------------------------------
//
// The fixtures are the real crash this page was built for: PS 8.0.16 recursing through the FTS
// query AST until the stack runs out, with the SIGSEGV surfacing inside libc's allocator.

const gdbTargetFx = {
  stackId: 1, stackName: 'crash lab', nodeId: 'lc1', label: 'linuxclient1',
  hostname: 'linuxclient1', os: 'oraclelinux', osVersion: '8',
  product: 'ps', major: '8.0', version: '8.0.16-7.1',
  binary: '/sysroot/mysqld', binaryFrom: 'mounted', buildId: '3f2a9c', hasSymbols: true,
  coreDir: '/srv/coredumps/db7/cores', libDir: '/srv/coredumps/db7/libs', status: 'ready',
}

const gdbFramesFx = [
  { level: 0, addr: '0x7602eb1295de', func: '_int_malloc', from: '/lib64/libc.so.6' },
  { level: 1, addr: '0x7602eb12c0d6', func: 'calloc', from: '/lib64/libc.so.6' },
  { level: 2, addr: '0x1f67add', func: 'ut_allocator<unsigned char>::allocate(unsigned long, unsigned char const*, unsigned int, bool, bool)' },
  { level: 3, addr: '0x216283f', func: 'rbt_create(unsigned long, int (*)(void const*, void const*))', file: '/src/storage/innobase/ut/ut0rbt.cc', line: 52 },
  { level: 4, addr: '0x22f9c88', func: 'fts_query_visitor(fts_ast_oper_t, fts_ast_node_t*, void*)', file: '/src/storage/innobase/fts/fts0que.cc', line: 3707, repeat: 140 },
  { level: 5, addr: '0x2337a6a', func: 'fts_ast_visit(fts_ast_oper_t, fts_ast_node_t*, dberr_t (*)(fts_ast_oper_t, fts_ast_node_t*, void*), void*, bool*)' },
]

const gdbStateFx = {
  status: 'ready', core: 'core.mysqld.9712.1787625764',
  signal: 'SIGSEGV', signalText: 'Segmentation fault',
  threads: [
    { id: '1', target: 'Thread 0x7602d8112700 (LWP 9756)', frame: gdbFramesFx[0] },
    { id: '2', target: 'Thread 0x7602ed397380 (LWP 9712)', frame: { level: 0, func: 'poll', from: '/lib64/libc.so.6' } },
  ],
  thread: '1', totalThreads: 62, allowShell: false, subscribers: 1, target: gdbTargetFx,
}

const gdbCoresFx = [
  { name: 'core.mysqld.9712.1787625764', size: 811331584, modified: '2026-08-25T02:42:00Z',
    executable: '/usr/sbin/mysqld', signal: 'SIGSEGV', buildId: '3f2a9c', buildIdMatch: true,
    resolved: 41, missing: [] },
  { name: 'core.mysqld.older', size: 4096, modified: '2026-08-01T00:00:00Z',
    executable: '/usr/sbin/mysqld', buildId: 'deadbe', buildIdMatch: false,
    resolved: 3, missing: ['/lib64/libssl.so.1.1', '/lib64/libcrypto.so.1.1'] },
]

check('CoreDumpAnalyzer page (before the socket opens)', () => renderToString(<CoreDumpAnalyzer />))
check('core dump: header', () =>
  renderToString(<GdbHeader targets={[gdbTargetFx]} value="1/lc1" onChange={noop}
    state={gdbStateFx} busy="" onClose={noop} onRefresh={noop} />))
check('core dump: nothing to analyse', () => renderToString(<GdbNoTargets />))
const gdbVerdictFx = {
  class: 'stack-exhaustion',
  headline: 'The thread ran out of stack: fts_ast_visit_sub_exp recursed 212 times without a depth limit.',
  why: 'fts_query_visitor → fts_ast_visit → fts_ast_visit → fts_ast_visit → fts_ast_visit_sub_exp is a '
    + '5-frame cycle that repeats 212 times, 1060 of the stack\'s 1085 frames. The signal landed in '
    + '_int_malloc only because an allocation was the first thing to touch the guard page.',
  evidence: [
    'the stack is 1085 frames deep',
    '1060 of them are one repeating 5-frame cycle: fts_query_visitor → fts_ast_visit → fts_ast_visit → fts_ast_visit → fts_ast_visit_sub_exp',
    'the signal is SIGSEGV and the faulting frame is _int_malloc, in the C library',
    'the recursion was started by fts_query at frame #1068, working on query_str',
  ],
  depth: 1085,
  cycle: ['fts_query_visitor', 'fts_ast_visit', 'fts_ast_visit', 'fts_ast_visit', 'fts_ast_visit_sub_exp'],
  repeats: 212,
  trigger: {
    frame: 1068, func: 'fts_query(trx_t*, dict_index_t*, uint, char const*, ulint, fts_result_t**, ulonglong)',
    where: 'fts0que.cc:3760', name: 'query_str', value: '+(+(+(+(+(+(+(', extra: 'query_len = 2748',
  },
}

check('core dump: the verdict — what went wrong and why', () => {
  const html = renderToString(<GdbSummary state={{ ...gdbStateFx, verdict: gdbVerdictFx }}
    frames={gdbFramesFx} core={gdbCoresFx[0]} />)
  // The three things a stack alone does not tell you.
  if (!html.includes('ran out of stack')) throw new Error('the crash class is not named')
  if (!html.includes('1085')) throw new Error('the real stack depth is not shown')
  if (!html.includes('query_len = 2748')) throw new Error('the triggering input is not shown')
  if (!html.includes('frame #1068')) throw new Error('where the trigger was found is not shown')
  return 'ok'
})
check('core dump: with no verdict it still says what is known', () => {
  const html = renderToString(<GdbSummary state={gdbStateFx} frames={gdbFramesFx} core={gdbCoresFx[0]} />)
  if (!html.includes('SIGSEGV')) throw new Error('the signal is not shown without a verdict')
  return 'ok'
})

check('core dump: the summary in every state', () => {
  for (const status of ['idle', 'loading', 'error', 'ready']) {
    renderToString(<GdbSummary state={{ ...gdbStateFx, status, detail: 'x' }}
      frames={gdbFramesFx} core={gdbCoresFx[0]} />)
  }
  // A core from a different build must be called out — that is the failure that produces a
  // plausible, wrong stack rather than no stack.
  const html = renderToString(<GdbSummary state={gdbStateFx} frames={gdbFramesFx} core={gdbCoresFx[1]} />)
  if (!html.includes('different build')) throw new Error('a build-id mismatch is not reported')
  if (!html.includes('could not be')) throw new Error('missing objects are not reported')
  return 'ok'
})
check('core dump: core list with its verdicts', () =>
  renderToString(<GdbCores cores={gdbCoresFx} open={gdbCoresFx[0].name} busy=""
    target={gdbTargetFx} onOpen={noop} />))
check('core dump: core list with nothing mounted', () =>
  renderToString(<GdbCores cores={[]} open="" busy="" target={gdbTargetFx} onOpen={noop} />))
check('core dump: threads', () =>
  renderToString(<GdbThreads threads={gdbStateFx.threads} selected="1" signal="SIGSEGV" onSelect={noop} />))
check('core dump: the stack, with recursion collapsed', () => {
  const html = renderToString(<GdbStack frames={gdbFramesFx} selected={0} onSelect={noop}
    more busy="" state={gdbStateFx} onMore={noop} />)
  if (!html.includes('140')) throw new Error('a collapsed frame does not show its repeat count')
  return 'ok'
})
check('core dump: an empty stack explains itself', () =>
  renderToString(<GdbStack frames={[]} selected={0} onSelect={noop} more={false} busy="" state={{}} onMore={noop} />))
check('core dump: frame variables', () =>
  renderToString(<GdbVars frame={gdbFramesFx[4]} vars={[
    { name: 'oper', type: 'fts_ast_oper_t', value: 'FTS_EXIST', arg: true },
    { name: 'node', type: 'fts_ast_node_t *', value: '0x7602d0001234', arg: true },
    { name: 'state', type: 'fts_query_t *', value: '0x7602d0005678' },
  ]} />))
check('core dump: a frame with no symbols says why', () =>
  renderToString(<GdbVars frame={gdbFramesFx[0]} vars={[]} />))
check('core dump: the terminal recipe is offered once a core is open', () => {
  // The line comes from the server (app/gdbcore.go) with this node's own paths in it; what the
  // page owes it is that it is there to copy, and readable when opened.
  const recipe = [
    'docker exec -it dbcanvas-12-linuxclient-01 gdb -nx \\',
    '  -ex "set sysroot /sysroot" \\',
    '  -ex "set solib-search-path /sysroot:/sysroot/lib64" \\',
    '  /sysroot/mysqld /coredumps/core.1234',
  ].join('\n')
  const html = renderToString(<GdbRecipe recipe={recipe} />)
  if (!/Run this in your own terminal/.test(html)) throw new Error('the recipe is not offered')
  if (!/Copy/.test(html)) throw new Error('there is nothing to copy it with')
  // Collapsed by default — the page is the thing you are reading; this is for when it is not.
  if (html.includes('set solib-search-path')) throw new Error('the command should start collapsed')
  // And no core open means no offer, rather than an empty box or a line with holes in it.
  if (renderToString(<GdbRecipe recipe="" />) !== '') throw new Error('an empty recipe still rendered')
  if (renderToString(<GdbRecipe />) !== '') throw new Error('a missing recipe still rendered')
  return 'collapsed, copyable'
})

check('core dump: the source of the selected frame is the whole file', () => {
  const frame = { level: 6, func: 'temptable::Table::number_of_rows', file: '/usr/src/debug/percona-server-8.0.30-22.1.el9.x86_64/percona-server-8.0.30-22/storage/temptable/include/temptable/table.h', line: 190 }
  // What the server sends now: the file from line 1, not a window around the frame.
  const lines = Array.from({ length: 400 }, (_, i) => `  // line ${i + 1}`)
  lines[189] = '    return m_rows.size();'
  const html = renderToString(<GdbSource frame={frame} source={{ from: 1, line: 190, file: frame.file, lines }} />)
  if (!html.includes('return m_rows.size();')) throw new Error('the source line is not shown')
  // Numbered from `from`, so the frame's line carries the frame's number and the
  // rest of the file is where it actually is.
  if (!/>190</.test(html)) throw new Error('the gutter does not number the lines')
  if (!/>1</.test(html) || !/>400</.test(html)) throw new Error('the pane is not showing the whole file')
  if (!html.includes('400 lines')) throw new Error('the header does not say how long the file is')
  // The rows are cheap off screen, which is what makes a whole file affordable.
  if (!html.includes('content-visibility:auto')) throw new Error('rows lost their content-visibility')
  // A file past the read cap says so rather than just ending.
  const cut = renderToString(<GdbSource frame={frame} source={{ from: 1, line: 2, file: frame.file, lines, truncated: true }} />)
  if (!/cut off here/.test(cut)) throw new Error('a truncated file does not say it was cut')
  return `${lines.length} lines`
})
check('core dump: a frame with no source says which kind of gap it is', () => {
  renderToString(<GdbSource frame={{ level: 0, func: '_int_malloc', from: '/lib64/libc.so.6' }} source={null} />)
  return renderToString(<GdbSource frame={{ level: 5, func: 'x', file: '/usr/src/debug/a.cc', line: 9 }}
    source={{ error: '/usr/src/debug/a.cc is not on this node — install the debugsource package for this version' }} />)
})

check('core dump: evaluate', () =>
  renderToString(<GdbEval value="" onChange={noop} onAdd={noop} onRemove={noop} watches={[
    { expr: 'node->type', value: 'FTS_AST_OPER' },
    { expr: 'nope', error: 'No symbol "nope" in current context.' },
  ]} />))
check('core dump: the gdb console and its shell tick', () => {
  renderToString(<GdbConsole value="info sharedlibrary" onChange={noop} onRun={noop}
    output="From  To  Syms Read  Shared Object Library" busy="" allowShell={false} onAllowShell={noop} />)
  return renderToString(<GdbConsole value="" onChange={noop} onRun={noop} output="" busy=""
    allowShell onAllowShell={noop} />)
})

// The summary's whole job is picking the frame that names the bug, and the top frame is not it:
// a stack overflow surfaces inside the allocator every time.
check('core dump: the summary skips the C library to find the culprit', () => {
  const { culprit, recursion } = crashSummary(gdbFramesFx)
  if (!culprit || !culprit.func.startsWith('ut_allocator')) {
    throw new Error(`culprit = ${culprit && culprit.func} — libc frames must be skipped`)
  }
  if (!recursion.includes('fts_query_visitor') || !recursion.includes('140')) {
    throw new Error(`recursion = ${recursion}`)
  }
  if (!isSystemFrame(gdbFramesFx[0]) || isSystemFrame(gdbFramesFx[3])) {
    throw new Error('libc frames are not being told apart from the program\'s own')
  }
  if (shortFunc(gdbFramesFx[2].func) !== 'ut_allocator<unsigned char>::allocate') {
    throw new Error(`shortFunc mangles a C++ name: ${shortFunc(gdbFramesFx[2].func)}`)
  }
  if (sourceOf(gdbFramesFx[3]) !== 'ut0rbt.cc:52') throw new Error(sourceOf(gdbFramesFx[3]))
  if (sourceOf(gdbFramesFx[0]) !== 'libc.so.6') throw new Error(sourceOf(gdbFramesFx[0]))
  if (formatBytes(811331584) !== '774 MiB') throw new Error(formatBytes(811331584))
  for (const st of ['idle', 'loading', 'ready', 'error']) {
    if (!GDB_STATUS_TONE[st] || !GDB_STATUS_TEXT[st]) throw new Error(`status ${st} has no tone or word`)
  }
  return 'ok'
})

// --- deployment templates (StackDesigner.jsx) -------------------------------
//
// insertTemplateDesign is the one piece of template handling with no server-side
// counterpart to catch its mistakes: it rewrites ids, drops singletons and
// renumbers labels entirely in the browser, and a slip means a design that either
// fails to deploy or — worse — deploys wired to the wrong node.

// A template that carries an Intranet (a singleton), a PMM node, and a PXC frame
// whose members and frame both point at that PMM.
const tplFixture = {
  nodes: [
    { id: 't-intra', type: 'intranet', label: 'Intranet', x: 40, y: 40 },
    { id: 't-pmm', type: 'pmm', label: 'pmm-01', x: 40, y: 220 },
    { id: 't-pxc-1', type: 'pxc', label: 'pxc-1', frameId: 't-frame', pmmNodeId: 't-pmm', x: 574, y: 66 },
    { id: 't-pxc-2', type: 'pxc', label: 'pxc-2', frameId: 't-frame', pmmNodeId: 't-pmm', x: 702, y: 66 },
  ],
  frames: [
    { id: 't-frame', type: 'pxc', label: 'pxc-cluster-01', pmmNodeId: 't-pmm', x: 560, y: 20, w: 400, h: 138 },
  ],
  edges: [
    { id: 't-edge', from: { node: 't-frame', port: 'bottom' }, to: { node: 't-pmm', port: 'top' }, type: 'directional' },
  ],
}

let uidN = 0
const testUid = (p) => `${p}-new-${++uidN}`

check('template insert: into an empty canvas', () => {
  uidN = 0
  const r = insertTemplateDesign(tplFixture, { nodes: [], frames: [], edges: [] }, testUid)
  if (r.nodes.length !== 4 || r.frames.length !== 1 || r.edges.length !== 1) {
    throw new Error(`got ${r.nodes.length} nodes / ${r.frames.length} frames / ${r.edges.length} edges`)
  }
  if (r.skipped.length || r.renamed.length) throw new Error('nothing should have been skipped or renamed')
  // Every id is fresh, and every reference follows it.
  const ids = new Set(r.nodes.map((n) => n.id).concat(r.frames.map((f) => f.id)))
  for (const id of ids) {
    if (id.startsWith('t-')) throw new Error(`id ${id} was not remapped`)
  }
  const pmm = r.nodes.find((n) => n.type === 'pmm')
  const member = r.nodes.find((n) => n.type === 'pxc')
  const frame = r.frames[0]
  if (member.pmmNodeId !== pmm.id) throw new Error('member pmmNodeId does not follow the remapped PMM node')
  if (frame.pmmNodeId !== pmm.id) throw new Error('frame pmmNodeId does not follow the remapped PMM node')
  if (member.frameId !== frame.id) throw new Error('member frameId does not follow the remapped frame')
  if (!ids.has(r.edges[0].from.node) || !ids.has(r.edges[0].to.node)) throw new Error('edge endpoints dangle')
  return 'ok'
})

check('template insert: the singleton already on the canvas is kept, not duplicated', () => {
  uidN = 0
  const current = {
    nodes: [{ id: 'existing-intra', type: 'intranet', label: 'Intranet', x: 0, y: 0 }],
    frames: [], edges: [],
  }
  const r = insertTemplateDesign(tplFixture, current, testUid)
  if (r.nodes.filter((n) => n.type === 'intranet').length !== 1) {
    throw new Error('the stack ended up with two Intranet nodes')
  }
  if (!r.skipped.length) throw new Error('the skipped Intranet was not reported')
  if (r.added !== 3) throw new Error(`added ${r.added}, expected 3 (the Intranet was already there)`)
  // Nothing may still point at the template's dropped Intranet id.
  if (JSON.stringify(r).includes('t-intra')) throw new Error('a reference to the dropped Intranet survived')
  return 'ok'
})

check('template insert: colliding labels are renumbered, not duplicated', () => {
  uidN = 0
  const current = {
    nodes: [
      { id: 'e1', type: 'intranet', label: 'Intranet', x: 0, y: 0 },
      { id: 'e2', type: 'pmm', label: 'pmm-01', x: 0, y: 120 },
      { id: 'e3', type: 'pxc', label: 'pxc-1', frameId: 'ef', x: 0, y: 240 },
    ],
    frames: [{ id: 'ef', type: 'pxc', label: 'pxc-cluster-01', x: 0, y: 220, w: 200, h: 100 }],
    edges: [],
  }
  const r = insertTemplateDesign(tplFixture, current, testUid)
  const labels = r.nodes.map((n) => n.label)
  if (new Set(labels).size !== labels.length) throw new Error(`duplicate node labels: ${labels.join(', ')}`)
  const frameLabels = r.frames.map((f) => f.label)
  if (new Set(frameLabels).size !== frameLabels.length) throw new Error(`duplicate frame labels: ${frameLabels.join(', ')}`)
  if (!labels.includes('pmm-01-2') || !labels.includes('pxc-1-2')) {
    throw new Error(`expected -2 suffixes, got ${labels.join(', ')}`)
  }
  if (!r.renamed.length) throw new Error('the renames were not reported')
  return 'ok'
})

check('template insert: the block lands clear of what is already on the canvas', () => {
  uidN = 0
  const current = {
    nodes: [{ id: 'e1', type: 'intranet', label: 'Intranet', x: 0, y: 0 }],
    frames: [], edges: [],
  }
  const r = insertTemplateDesign(tplFixture, current, testUid)
  const inserted = r.nodes.filter((n) => n.id !== 'e1')
  const topOfInserted = Math.min(...inserted.map((n) => n.y))
  if (topOfInserted < 104) throw new Error(`inserted block overlaps the existing node (top ${topOfInserted})`)
  return 'ok'
})

check('template insert: an edge already drawn is not drawn twice', () => {
  uidN = 0
  // Insert the same template into a canvas that is its own previous insert. The
  // singletons resolve to the incumbents, so the edge between them repeats.
  const first = insertTemplateDesign(tplFixture, { nodes: [], frames: [], edges: [] }, testUid)
  const second = insertTemplateDesign(tplFixture, first, testUid)
  const keys = second.edges.map((e) => `${e.from.node}:${e.from.port}->${e.to.node}:${e.to.port}`)
  if (new Set(keys).size !== keys.length) throw new Error(`duplicate edges: ${keys.join(', ')}`)
  return 'ok'
})

check('template picker: grouped by category, built-ins first', () => {
  const tpls = [
    { id: '7', name: 'Mine', category: 'MySQL', nodes: 3, frames: 1, builtin: false },
    { id: 'builtin:a', name: 'Default', category: 'MySQL', nodes: 5, frames: 1, builtin: true },
    { id: '8', name: 'Loose', category: '', nodes: 1, frames: 0, builtin: false },
  ]
  const groups = groupTemplates(tpls)
  const mysql = groups.find((g) => g.category === 'MySQL')
  if (!mysql || mysql.items[0].name !== 'Default') throw new Error('built-ins do not sort first')
  if (!groups.some((g) => g.category === 'Uncategorized')) throw new Error('a template with no category was dropped')
  if (templateSizeLabel(tpls[0]) !== '3 nodes · 1 cluster') throw new Error(templateSizeLabel(tpls[0]))
  if (templateSizeLabel(tpls[2]) !== '1 node') throw new Error(templateSizeLabel(tpls[2]))
  return 'ok'
})

// --- tooltips -------------------------------------------------------------
// The help text is the point of the feature, so the checks are about the text
// arriving, not about the bubble: a tooltip only opens on hover, which SSR never
// does. What SSR *can* prove is that the trigger renders wherever a caller passed
// help, that it stays absent when nobody did (an empty "?" on every label would be
// worse than none), and that the catalog every call site indexes into is not full
// of holes — a typo'd key is `undefined`, which renders no trigger and no error.

check('tooltip: Field renders a help trigger only when given help', () => {
  const withHelp = renderToString(<Field label="Host port" help={HELP.hostPort}><input /></Field>)
  const without = renderToString(<Field label="Host port"><input /></Field>)
  if (!withHelp.includes('What is this?')) throw new Error('help was passed but no trigger rendered')
  if (without.includes('What is this?')) throw new Error('a trigger rendered for a field with no help')
  return withHelp
})

check('tooltip: Hint wraps its child and passes it through untouched', () => {
  const wrapped = renderToString(<Hint text="explain"><button>Deploy</button></Hint>)
  const bare = renderToString(<Hint text=""><button>Deploy</button></Hint>)
  if (!wrapped.includes('Deploy')) throw new Error('Hint swallowed its child')
  if (bare !== '<button>Deploy</button>') throw new Error(`Hint with no text should render the child alone, got ${bare}`)
  return wrapped
})

check('tooltip: InfoRow carries a label, its help, and its value', () => {
  const html = renderToString(<InfoRow label="Image" help={HELP.depImage}><span>percona:8.0</span></InfoRow>)
  if (!html.includes('Image') || !html.includes('percona:8.0')) throw new Error('InfoRow lost its label or value')
  if (!html.includes('What is this?')) throw new Error('InfoRow rendered no help trigger')
  return html
})

check('tooltip: every catalog entry is a non-empty string', () => {
  const catalogs = { HELP, MENU_HELP, TOOL_HELP, DEP_HELP, MORE_HELP, FTDC_HELP }
  const bad = []
  for (const [name, cat] of Object.entries(catalogs)) {
    for (const [k, v] of Object.entries(cat)) {
      // HELP.major / HELP.minor are functions of the product name.
      const text = typeof v === 'function' ? v('Percona Server') : v
      if (typeof text !== 'string' || text.trim().length < 20) bad.push(`${name}.${k}`)
    }
  }
  if (bad.length) throw new Error(`empty or stub help text: ${bad.join(', ')}`)
  return `${Object.values(catalogs).reduce((n, c) => n + Object.keys(c).length, 0)} entries`
})

check('tooltip: the node palette explains every type it can add', () => {
  // A palette entry with no blurb falls back to NODE_TYPES.sub, so this is about the
  // fallback existing at all — an entry with neither would show its own label back.
  const missing = Object.entries(NODE_TYPES)
    .filter(([type, def]) => !nodeHelp(type) && !def.sub)
    .map(([type]) => type)
  if (missing.length) throw new Error(`no blurb and no subtitle: ${missing.join(', ')}`)
  return `${Object.keys(NODE_TYPES).length} node types`
})

check('add menu: a category with nothing available is itself disabled', () => {
  // Before the Intranet is on the canvas every other entry is unavailable, so
  // every category would open onto nothing but greyed rows. The category row is
  // disabled instead, and carries the children's reason when they agree on one.
  const groups = [
    { title: 'Core', items: [{ label: 'Intranet' }] },
    { title: 'MySQL (Percona)', items: [{ label: 'PXC Cluster' }, { label: 'Percona Server' }] },
    { title: 'Identity & Secrets', items: [{ label: 'Keycloak' }, { label: 'OpenBao' }] },
  ]
  // No Intranet yet: everything but the Intranet itself is blocked, one shared reason.
  const noIntranet = (it) => it.label === 'Intranet'
    ? { label: it.label, disabled: false }
    : { label: it.label, disabled: true, help: 'Add an Intranet node first' }
  const before = menuEntriesFor(groups, noIntranet)
  const cats = before.filter((e) => e.items)
  if (!cats.length) throw new Error('expected submenu categories')
  for (const c of cats) {
    if (!c.disabled) throw new Error(`${c.label} should be disabled with no Intranet`)
    if (c.help !== 'Add an Intranet node first') throw new Error(`${c.label} help = ${c.help}`)
  }
  if (before.find((e) => e.label === 'Intranet').disabled) throw new Error('Intranet must stay available')

  // One usable entry is enough to keep its category open.
  const oneLeft = (it) => ({ label: it.label, disabled: it.label !== 'Percona Server', help: 'nope' })
  const mysql = menuEntriesFor(groups, oneLeft).find((e) => e.label === 'MySQL (Percona)')
  if (mysql.disabled) throw new Error('a category with one available entry must stay enabled')
  if (mysql.help !== undefined) throw new Error('an enabled category should carry no reason')

  // Children blocked for different reasons get the generic line, not one of theirs.
  const mixed = (it) => ({ label: it.label, disabled: true, help: `${it.label} is already on the canvas` })
  const ident = menuEntriesFor(groups, mixed).find((e) => e.label === 'Identity & Secrets')
  if (!ident.disabled) throw new Error('all-disabled category should be disabled')
  if (ident.help !== 'Nothing in this category can be added right now') throw new Error(`mixed help = ${ident.help}`)
  return 'blocked, partly available, mixed reasons'
})

check('pod console: the menu is namespace \u2192 pod \u2192 container \u2192 client or shell', () => {
  // The four levels are the address of a shell in Kubernetes, and this is the tree
  // built from what the cluster answered — not from anything on the canvas.
  const pods = [
    { namespace: 'default', name: 'cluster1-haproxy-0', phase: 'Running', containers: [
      { name: 'haproxy', state: 'running', ready: true, clients: ['mysql'] },
      { name: 'pxc-monit', state: 'running', ready: true, clients: ['mysql'] },
    ] },
    { namespace: 'default', name: 'cluster1-pxc-0', phase: 'Pending', containers: [
      { name: 'pxc', state: 'waiting', ready: false },
      { name: 'pxc-init', state: 'running', ready: false, init: true },
    ] },
    { namespace: 'kube-system', name: 'coredns-abc', phase: 'Running', containers: [
      { name: 'coredns', state: 'running', ready: true },
    ] },
  ]
  const picked = []
  const tree = podMenuEntries(pods, (p) => picked.push(p))

  if (tree.map((e) => e.label).join(',') !== 'default,kube-system') {
    throw new Error(`namespaces: ${tree.map((e) => e.label).join(',')}`)
  }
  const def = tree[0]
  if (def.items.length !== 2) throw new Error(`default should hold 2 pods, got ${def.items.length}`)
  // A Running pod reads as its own name; anything else carries its phase, because
  // that is the row somebody opening this menu is looking for.
  if (def.items[0].label !== 'cluster1-haproxy-0') throw new Error(`label = ${def.items[0].label}`)
  if (def.items[1].label !== 'cluster1-pxc-0 \u00b7 Pending') throw new Error(`label = ${def.items[1].label}`)

  // A container that is not running is shown and disabled, with the reason — never
  // hidden, and never offered as a shell that would fail at the far end.
  const pxc = def.items[1].items
  if (!pxc[0].disabled) throw new Error('a waiting container must not be selectable')
  if (!/waiting/.test(pxc[0].help)) throw new Error(`no reason on the disabled row: ${pxc[0].help}`)
  if (pxc[1].label !== 'pxc-init \u00b7 init' || pxc[1].disabled) throw new Error(`init row = ${JSON.stringify(pxc[1])}`)

  // The leaf is what to run, and picking one names all four parts. A database
  // container leads with its client and then a separator, because that is what
  // somebody opening a console on a pxc or haproxy container came for; the shells keep
  // their order under it.
  const leaf = def.items[0].items[0].items
  if (leaf.length !== 1 + 1 + POD_SHELLS.length) throw new Error(`leaf rows: ${leaf.length}`)
  if (leaf[0].label !== 'mysql') throw new Error(`first row = ${leaf[0].label}`)
  if (!leaf[1].sep) throw new Error('the client and the shells must be separated')
  leaf[0].fn()
  const wantMySQL = { namespace: 'default', name: 'cluster1-haproxy-0', container: 'haproxy', shell: 'mysql' }
  if (JSON.stringify(picked[0]) !== JSON.stringify(wantMySQL)) throw new Error(`picked ${JSON.stringify(picked[0])}`)
  leaf[3].fn()
  const want = { namespace: 'default', name: 'cluster1-haproxy-0', container: 'haproxy', shell: 'bash' }
  if (JSON.stringify(picked[1]) !== JSON.stringify(want)) throw new Error(`picked ${JSON.stringify(picked[1])}`)

  // A container the cluster reported no client for is the shells alone — unchanged
  // from before there were clients at all.
  const dns = tree[1].items[0].items[0].items
  if (dns.length !== POD_SHELLS.length || dns.some((r) => r.sep)) throw new Error(`coredns leaf: ${dns.length}`)

  // Every client id in the menu is one the server accepts, or the row opens a socket
  // that is refused before it upgrades.
  const ids = POD_CLIENTS.map((c) => c.id).join(',')
  if (ids !== 'mysql,psql,mongosh') throw new Error(`client ids = ${ids}`)
  return `${tree.length} namespaces, ${POD_CLIENTS.length} clients, ${POD_SHELLS.length} shells`
})

check('pod console: an empty cluster and a pod with no containers still build', () => {
  if (podMenuEntries([], () => {}).length !== 0) throw new Error('no pods should be no namespaces')
  if (podMenuEntries(null, () => {}).length !== 0) throw new Error('a missing list should be no namespaces')
  const bare = podMenuEntries([{ namespace: 'x', name: 'p', phase: 'Running' }], () => {})
  if (bare[0].items[0].items.length !== 0) throw new Error('a container-less pod should hold no rows')
  if (bare[0].items[0].empty !== 'No containers') throw new Error('an empty submenu needs something to say')
  return 'empty list, container-less pod'
})

check('add menu: the menu panel itself stays on screen near an edge', () => {
  // Right-clicking low on the canvas used to open a 14-row menu that hung below
  // the fold: the clamp was a flat `min(y, innerHeight - 160)`, sized for the short
  // node-action menu this component was originally written for. The rows down
  // there were simply unreachable.
  const rows = (n) => Array.from({ length: n }, (_, i) => ({ label: `row ${i}` }))
  const vw = 1400, vh = 900

  // Room below: opens exactly at the pointer.
  const a = menuPos(400, 200, rows(5), vw, vh)
  if (a.x !== 400 || a.y !== 200) throw new Error(`expected the pointer, got ${a.x},${a.y}`)

  // Near the bottom: pushed up far enough that the whole menu fits.
  const tall = rows(14)
  const b = menuPos(400, 840, tall, vw, vh)
  const h = 14 * 30 + 8
  if (b.y !== vh - h - 8) throw new Error(`expected a push up to ${vh - h - 8}, got ${b.y}`)
  if (b.y + h > vh) throw new Error('menu still runs off the bottom')

  // Near the right edge: pushed left, never off it.
  const c = menuPos(1390, 200, rows(5), vw, vh)
  if (c.x + 208 > vw) throw new Error(`menu runs off the right: ${c.x}`)

  // Taller than the viewport: pinned to the top and given a height budget that
  // fits, so it scrolls rather than overflowing.
  const d = menuPos(400, 800, rows(60), vw, vh)
  if (d.y !== 8) throw new Error(`a too-tall menu should pin to the top, got ${d.y}`)
  if (d.y + d.maxHeight > vh) throw new Error('maxHeight lets it overflow the viewport')

  // The height budget always keeps the panel inside the viewport, whatever the estimate.
  for (const [py, n] of [[0, 3], [450, 14], [899, 14], [899, 60]]) {
    const r = menuPos(400, py, rows(n), vw, vh)
    if (r.y < 0 || r.y + r.maxHeight > vh) throw new Error(`off screen at py=${py} n=${n}: ${JSON.stringify(r)}`)
  }
  // A menu holding a label too long for the panel is TALLER than one row per
  // action, because such a label wraps rather than truncating. Getting this wrong
  // is not cosmetic: the estimate is what pushes a menu opened near the bottom far
  // enough up to be fully reachable.
  const long = [...rows(4), { label: 'Download diagnostic.data with mongod.log' }]
  const e = menuPos(400, 880, long, vw, vh)
  const f = menuPos(400, 880, rows(5), vw, vh)
  if (!(e.y < f.y)) throw new Error('a wrapped row did not make the menu taller')
  return 'pointer, bottom push-up, right clamp, too-tall scrolls, wrapped rows'
})

check('add menu: a submenu flips and clamps to stay on screen', () => {
  // This exists because the first cut of the submenu was laid out *inside* the
  // menu panel, which scrolls — and a scroll container clips both axes, so the
  // panel was rendered correctly and then clipped out of existence. It is now
  // positioned against the viewport, which is only meaningful if the arithmetic
  // that places it is right. W = 208.
  const row = (top, left, right) => ({ top, left, right })
  const rows = (n) => Array.from({ length: n }, (_, i) => ({ label: `row ${i}` }))

  // Room on the right: opens just past the row.
  const a = submenuPos(row(100, 300, 500), rows(5), 1400, 900)
  if (a.x !== 504) throw new Error(`expected to open right at 504, got ${a.x}`)
  if (a.y !== 96) throw new Error(`expected to align near the row, got ${a.y}`)

  // Against the right edge there is no room, so it flips to the left of the row.
  const b = submenuPos(row(100, 1150, 1350), rows(5), 1400, 900)
  if (b.x !== 1150 - 208 - 4) throw new Error(`expected a left flip, got ${b.x}`)
  if (b.x + 208 > 1400) throw new Error('flipped panel still runs off the right')

  // A row near the bottom opens a panel that would run past the fold, so it rides up.
  const c = submenuPos(row(870, 300, 500), rows(6), 1400, 900)
  const h = Math.min(6 * 30 + 8, 900 * 0.6)
  if (c.y !== 900 - h - 8) throw new Error(`expected a bottom clamp, got ${c.y}`)
  if (c.y + h > 900) throw new Error('panel runs off the bottom')

  // And a row at the very top never goes negative.
  const d = submenuPos(row(0, 300, 500), rows(3), 1400, 900)
  if (d.y < 0) throw new Error(`panel runs off the top: ${d.y}`)

  // A very long category is capped rather than growing past the viewport.
  const e = submenuPos(row(400, 300, 500), rows(40), 1400, 900)
  if (e.y < 8) throw new Error(`a long submenu should still start on screen: ${e.y}`)

  // Rows that WRAP are taller, and the placement has to know it: a panel of nine
  // wrapping pod names placed as if they were one-liners starts too low.
  const long = Array.from({ length: 9 }, (_, i) => ({ label: `percona-xtradb-cluster-operator-6b5f75f65-gpdv${i}`, items: [] }))
  const f = submenuPos(row(700, 300, 500), long, 1400, 900, menuWidth(long))
  const g = submenuPos(row(700, 300, 500), rows(9), 1400, 900)
  if (f.y >= g.y) throw new Error('a panel of wrapping rows must ride further up than one of short rows')
  return 'right, left-flip, bottom-clamp, top, long, wrapping'
})

check('nav: every sidebar entry has an icon, and no two neighbours share one', () => {
  // `Icon[n.icon]` is looked up by name in four places (sidebar, tab strip, the mounted page,
  // Cmd+K). A name that does not resolve is `undefined` rendered as a component, which takes the
  // whole workspace down — so a typo here is not a missing picture, it is a blank app.
  for (const n of NAV) {
    if (typeof Icon[n.icon] !== 'function') throw new Error(`${n.id} names icon ${n.icon}, which does not exist`)
  }
  // The sidebar is read at 18px down a single column, where two entries drawn alike are two
  // entries you pick wrong. Dashboard and Database Stacks were four rounded squares each, and
  // Operator Summary wore the Operator Debugger's beetle; adjacency is what makes that cost
  // something, so neighbours must differ.
  for (let i = 1; i < NAV.length; i++) {
    if (NAV[i].icon === NAV[i - 1].icon) {
      throw new Error(`${NAV[i - 1].id} and ${NAV[i].id} are next to each other with the same ${NAV[i].icon} icon`)
    }
  }
  return `${NAV.length} entries`
})

check('experimental: tagged features are hidden until an installation asks for them', () => {
  // EXPERIMENTAL (app/experimental.go) rides on the system settings, and every
  // menu filters through these two functions. Off is the default and the important
  // case: a half-finished feature lands hidden rather than in everyone's menus.
  if (showExperimental(undefined)) throw new Error('no settings yet should read as off')
  if (showExperimental({})) throw new Error('settings without the field should read as off')
  if (showExperimental({ experimental: 'yes' })) throw new Error('only a real true is on')
  if (!showExperimental({ experimental: true })) throw new Error('experimental: true should be on')

  // Labs is the tagged page. Off, it is not in the nav at all — which is also what
  // makes #labs unreachable, since the tab strip and the pages read this same list.
  const off = visible(NAV, false).map((n) => n.id)
  const on = visible(NAV, true).map((n) => n.id)
  if (off.includes('labs')) throw new Error('Labs is in the menu with experimental off')
  if (!on.includes('labs')) throw new Error('Labs is missing with experimental on')
  if (on.length !== off.length + 1) throw new Error(`the switch moved ${on.length - off.length} nav entries`)
  if (!NAV.find((n) => n.id === 'labs')?.experimental) throw new Error('Labs lost its experimental tag')

  // A group is filtered item by item, and one left empty goes with its items
  // rather than leaving a heading over nothing.
  const groups = [
    { title: 'App Simulators', items: [{ label: 'Traffic Sim' }, { label: 'Unoptimized MySQL Challenge', experimental: true }] },
    { title: 'Not ready', items: [{ label: 'Only this', experimental: true }] },
    { title: 'Whole category', experimental: true, items: [{ label: 'Something' }] },
    { title: 'Core', items: [{ label: 'Intranet' }] },
  ]
  const titlesOff = visibleGroups(groups, false).map((g) => g.title)
  if (titlesOff.join(',') !== 'App Simulators,Core') throw new Error(`off shows ${titlesOff.join(',')}`)
  const sims = visibleGroups(groups, false)[0].items.map((i) => i.label)
  if (sims.join(',') !== 'Traffic Sim') throw new Error(`the challenge survived: ${sims.join(',')}`)
  if (visibleGroups(groups, true).length !== 4) throw new Error('on should show every category')
  // Filtering copies rather than mutating: the catalog is rebuilt on every render
  // from the same literal, and a filter that ate its input would empty it.
  if (groups[0].items.length !== 2) throw new Error('visibleGroups mutated the catalog')
  return 'nav, groups and items'
})

check('menu: a panel is as wide as its longest label, and nothing truncates', () => {
  // The pod console is what forced this: a submenu row used to truncate, and three
  // different pods came out as "percona-xtradb-cluste…", "k3d-00-pitr-588f5fdd5…",
  // "xb-dbcanvas-seed-k3…" — an ellipsis where the identifying half of the name was.
  const short = [{ label: 'Stop' }, { label: 'Restart' }]
  if (menuWidth(short) !== 208) throw new Error(`short labels should keep the 208px panel, got ${menuWidth(short)}`)

  // A wider label widens the panel, and a submenu row is allowed for its count and
  // its chevron on top of the text.
  const pods = [{ label: 'k3d-00-pitr-588f5fdd5c-2zx5j', items: [] }, { label: 'k3d-00-pxc-0', items: [] }]
  const w = menuWidth(pods)
  if (w <= 208) throw new Error(`a 28-character pod name needs more than 208px, got ${w}`)
  if (w > 340) throw new Error(`a panel must not grow past the ceiling: ${w}`)
  const leafOnly = menuWidth([{ label: 'k3d-00-pitr-588f5fdd5c-2zx5j' }])
  if (leafOnly >= w) throw new Error('a leaf row needs no room for a count and a chevron')

  // A name longer than the ceiling is capped — it wraps rather than widening the
  // panel until a four-level cascade walks off the canvas.
  const huge = menuWidth([{ label: 'percona-xtradb-cluster-operator-6b5f75f65-gpdvj', items: [] }])
  if (huge !== 340) throw new Error(`expected the ceiling, got ${huge}`)

  // Headings and separators have no label and must not shrink or widen anything.
  if (menuWidth([{ sep: true }, { heading: 'MySQL' }]) !== 208) throw new Error('a heading should not size a panel')
  if (menuWidth([]) !== 208 || menuWidth(null) !== 208) throw new Error('an empty panel keeps the minimum')
  return `short 208, pods ${w}, capped 340`
})

check('add menu: the canvas menu offers the whole catalog, once each', () => {
  // The right-click add menu and the docked library are two front ends onto one
  // array. This asserts the shaping rule rather than the catalog: nothing is lost
  // to the flattening, nothing is offered twice, and a one-entry category becomes
  // a direct action instead of a submenu you have to open to find a single row.
  const groups = [
    { title: 'Core', items: [{ label: 'Intranet' }] },
    { title: 'MySQL (Percona)', items: [{ label: 'PXC Cluster' }, { label: 'Percona Server' }] },
    { title: 'Kubernetes', items: [{ label: 'K3D Cluster' }] },
  ]
  const id = (it) => ({ label: it.label })
  const flat = menuEntriesFor(groups, id)

  const singles = flat.filter((e) => !e.items && !e.sep && !e.heading).map((e) => e.label)
  const subs = flat.filter((e) => e.items)
  if (singles.join(',') !== 'Intranet,K3D Cluster') throw new Error(`singletons not flattened: ${singles.join(',')}`)
  if (subs.length !== 1 || subs[0].label !== 'MySQL (Percona)') throw new Error('multi-item category should be a submenu')
  if (subs[0].items.length !== 2) throw new Error('submenu lost an entry')

  // Every catalog entry reachable exactly once.
  const reachable = [...singles, ...subs.flatMap((g) => g.items.map((i) => i.label))].sort()
  const all = groups.flatMap((g) => g.items.map((i) => i.label)).sort()
  if (reachable.join('|') !== all.join('|')) throw new Error(`reachable ${reachable.join(',')} != catalog ${all.join(',')}`)

  // No recently-used block: the menu is opened and closed in one gesture, so a
  // list that reorders itself between openings moves the row you were reaching
  // for. Recents stay in the docked library, where the panel holds still.
  if (flat.some((e) => e.heading || e.sep)) throw new Error('menu should be categories only')
  return `${all.length} entries, ${subs.length} submenu, ${singles.length} flattened`
})

// A model shaped exactly like app/opsummary.go emits, taken from what the Go
// tests assert about the real cluster-dump fixture.
const OP_MODEL = {
  source: 'cluster-dump.tar.gz', kubernetes: 'v1.36.4+k3s1',
  verdicts: [
    { key: 'nodes', tone: 'good', title: 'All 1 nodes Ready', detail: 'Kubernetes v1.36.4+k3s1' },
    { key: 'workloads', tone: 'bad', title: '2 workloads are short of replicas', detail: 'cluster1-pxc (0/3) · crasher (0/2)' },
    { key: 'pods', tone: 'bad', title: '4 pods need attention', detail: 'ImagePullBackOff x1 · Restarting x1' },
    { key: 'operator', tone: 'bad', title: 'The operator is not reconciling cleanly', detail: '1 custom resource(s) not ready' },
  ],
  findings: [
    { severity: 'critical', title: 'Pod cluster1-pxc-0 is Unschedulable', detail: '0/1 nodes are available: 1 Insufficient memory.', where: 'demo-db/cluster1-pxc-0' },
    { severity: 'warning', title: 'PerconaXtraDBCluster cluster1: pxc is 1/3', where: 'demo-db/cluster1' },
    { severity: 'note', title: 'The collector could not read something', detail: 'no resource type perconaxtradbclusterbackups' },
  ],
  workloads: [
    { kind: 'StatefulSet', namespace: 'demo-db', name: 'cluster1-pxc', desired: 3, ready: 0 },
    { kind: 'Deployment', namespace: 'demo-db', name: 'crasher', desired: 2, ready: 0 },
  ],
  pods: [
    { namespace: 'demo-db', name: 'crasher-1', phase: 'Running', reason: 'Restarting', restarts: 3, age: '5m',
      message: 'container boom has restarted 3 time(s); last exit 1 (Error)',
      containers: [{ name: 'boom', ready: false, restartCount: 3, state: 'Waiting', reason: 'CrashLoopBackOff' }],
      logTail: ['starting', 'fatal: cannot reach cluster1-pxc'] },
    { namespace: 'demo-db', name: 'puller', phase: 'Pending', reason: 'ImagePullBackOff', restarts: 0, containers: [] },
  ],
  crs: [
    { kind: 'PerconaXtraDBCluster', group: 'pxc.percona.com', namespace: 'demo-db', name: 'cluster1',
      state: 'initializing', version: '1.15.0', host: 'cluster1-haproxy.demo-db',
      components: [{ name: 'pxc', ready: 1, size: 3, status: 'initializing' }, { name: 'haproxy', ready: 2, size: 2, status: 'ready' }],
      conditions: [{ type: 'Error', status: 'True', reason: 'ErrorReconcile', message: 'secret cluster1-secrets not found' }] },
  ],
  operators: [
    { namespace: 'pxc-operator', pod: 'percona-xtradb-cluster-operator-abc', kind: 'pxc', lines: 900, errors: 47,
      groups: [{ level: 'error', count: 47, message: 'failed to reconcile users secret' }] },
  ],
  logs: { sources: 15, events: 565, findings: [{ severity: 'bad', title: 'A member was shut down while it had no primary component', detail: 'On Kubernetes that is the liveness probe, not an operator.' }],
    worst: [{ source: 'k3d-00-pxc-0/mysqld-error.log', node: 'k3d-00-pxc-0', severity: 'error', kind: 'shutdown', message: 'Received SHUTDOWN from user', at: '02:28:51' }] },
  galera: [
    { namespace: 'pxc', pod: 'k3d-00-pxc-0', uuid: '23044676-a808-11f1', seqno: '-1', safeToBootstrap: false, hasView: true },
    { namespace: 'pxc', pod: 'k3d-00-pxc-1', uuid: '23044676-a808-11f1', seqno: '-1', safeToBootstrap: false, hasView: true },
  ],
  podSummaries: [{ namespace: 'pxc', pod: 'k3d-00-pxc-0', facts: { Version: '8.4.8-8.1 Percona XtraDB Cluster', Databases: '4', wsrep_cluster_size: '3' } }],
  backupLogs: [
    { namespace: 'pxc', pod: 'k3d-00-pxc-0', file: 'innobackup.backup.log', bytes: 44598, completedOk: true, tail: ['completed OK!'] },
    { namespace: 'pxc', pod: 'k3d-00-pxc-1', file: 'innobackup.backup.log', bytes: 120, errors: ['xbcloud: Probe failed. Please check your credentials and endpoint settings.'] },
  ],
  schedules: [{ namespace: 'pxc', name: 'nightly', schedule: '0 0 * * *', suspended: true }],
  rollouts: [{ namespace: 'pxc', owner: 'percona-xtradb-cluster-operator', revisions: 1 }],
  budgets: [{ namespace: 'pxc', name: 'k3d-00-pxc', healthy: 3, desired: 2, disruptions: 1 }],
  rbac: [{ namespace: 'pxc', kind: 'Role', name: 'percona-xtradb-cluster-operator', rules: 8 }],
  config: [{ namespace: 'pxc', name: 'k3d-00-pxc', keys: ['my.cnf'], excerpt: '[mysqld]' }],
  deployment: { operator: 'pxc', operatorName: 'percona/percona-xtradb-cluster-operator', version: '1.20.0', crVersion: '1.20.0', namespace: 'pxc', pmm: 'percona/pmm-client:3.8.0', kubernetes: 'v1.36.4+k3s1' },
  images: [
    { image: 'percona/percona-xtradb-cluster:8.4.8-8.1', repo: 'percona/percona-xtradb-cluster', tag: '8.4.8-8.1', used: ['pxc/k3d-00-pxc-0'], count: 3 },
    { image: 'percona/pmm-client:3.8.0', repo: 'percona/pmm-client', tag: '3.8.0', used: ['pxc/k3d-00-pxc-0'], count: 5 },
  ],
  secrets: [
    { name: 'k3d-00-ssl', kind: 'tls', used: ['pxc/k3d-00-pxc-0'] },
    { name: 'k3d-00-backup-s3', kind: 'referenced', used: [] },
  ],
  backups: [
    { kind: 'PerconaXtraDBClusterBackup', namespace: 'pxc', name: 'backup-diag-1', cluster: 'k3d-00', state: 'Running',
      storage: 'seaweedfs', storageType: 's3', destination: 's3://backup/k3d-00-2026-09-04-02:47:46-full', image: 'percona/percona-xtrabackup:8.4.0-5.1' },
    { kind: 'PerconaServerMongoDBBackup', namespace: 'psmdb', name: 'backup-diag-1', state: 'error', error: 'starting deadline seconds exceeded' },
  ],
  certs: [
    { secret: 'k3d-00-ssl', namespace: 'pxc', entry: 'tls.crt', subject: 'O = PXC', issuer: 'O = Root CA', notAfter: 'Dec  3 02:22:38 2026 GMT', daysLeft: 89, selfSigned: true },
    { secret: 'k3d-00-ssl', namespace: 'pxc', entry: 'ca.crt', subject: 'O = Root CA', issuer: 'O = Root CA', notAfter: 'Sep  3 02:22:38 2029 GMT', daysLeft: 1094, selfSigned: true },
  ],
  storage: [
    { namespace: 'pxc', name: 'datadir-k3d-00-pxc-0', status: 'Bound', capacity: '6G', requested: '6G', storageClass: 'local-path' },
  ],
  collectorErrors: ['the server doesn\'t have a resource type "perconaxtradbclusterbackups"'],
  available: { nodes: true, pods: true, crs: true, operators: true, workloads: true },
}

check('validate: a missing node image offers to build itself', () => {
  // Renders before its fetch lands (no effects under SSR), which is the state the
  // panel is in for a moment on every open — it must not throw and must not claim
  // anything it does not know yet.
  const cold = renderToString(<BuildImageRow id="bighole" isAdmin onBuilt={noop} />)
  if (cold !== '') throw new Error('the row rendered something before it knew the image')
  return 'renders nothing until it has the catalogue'
})

check('canvas: a node card is one row, and its tooltip still answers for it', () => {
  // The card is icon + name + status now. The risk in trimming it is that the
  // detail is not moved but lost, so the tooltip is what this checks.
  const n = { id: 'n1', type: 'pmm', label: 'pmm-01', os: 'oraclelinux', osVersion: '9', exportEnabled: true }
  const def = { label: 'PMM3', sub: 'Percona Monitoring & Management' }
  const tip = renderToString(nodeCardTip(n, def, { state: 'running', config: { serverVersion: '3.4.0' } }, 'amd64'))
  for (const want of ['pmm-01', 'PMM3', 'Percona Monitoring', '3.4.0', 'amd64', 'running', 'exported']) {
    if (!tip.includes(want)) throw new Error(`the card tooltip does not carry ${want}: ${tip}`)
  }
  // Not yet deployed: no version and no state, but still the type and the OS.
  const design = renderToString(nodeCardTip(n, def, null, 'arm64'))
  if (!/PMM3/.test(design) || !/arm64/.test(design)) throw new Error('an undeployed card says too little')
  if (/Running/.test(design)) throw new Error('an undeployed card claims to be running')

  // A cluster member's card is smaller still: name and a status dot, with the role
  // — the part actually worth reading — in the tooltip.
  const m = { id: 'm1', type: 'psmrs', label: 'psmrs01' }
  const mem = renderToString(memberCardTip({ label: 'psmrs-00', type: 'psmrs', arch: 'amd64' }, m,
    { state: 'running' }, 'replica-set member', 'PSMDB 8.0.29-13', 'amd64'))
  for (const want of ['psmrs01', 'replica-set member', 'PSMDB 8.0.29-13', 'psmrs-00', 'running']) {
    if (!mem.includes(want)) throw new Error(`the member tooltip does not carry ${want}: ${mem}`)
  }
  // Undeployed member: the frame's own image line stands in for the version.
  const memDesign = renderToString(memberCardTip({ label: 'pxc-01', type: 'pxc', os: 'oraclelinux', osVersion: '9' }, m, null, 'Galera data node', '', 'amd64'))
  if (!/Galera data node/.test(memDesign)) throw new Error('the member role is missing')
  if (!/amd64/.test(memDesign)) throw new Error('the member tooltip drops the image line when undeployed')
  return 'both tooltips carry what the cards stopped printing'
})

check('bighole: the FTDC viewer says where it has to be opened', () => {
  const form = renderToString(
    <BigHoleForm node={{ id: 'bh1', type: 'bighole', label: 'bighole-01' }} patchNode={noop} deleteNode={noop} />)
  // The one thing that will otherwise waste somebody's afternoon: the app needs a
  // secure context, and over a host address it fails partway through ingest, which
  // reads like a broken decoder.
  if (!/localhost/.test(form)) throw new Error('the form does not say to open it on localhost')
  if (!/diagnostic\.data/.test(form)) throw new Error('the form does not say what to drag onto it')
  // And it points at the page in DBCanvas that reads the same files differently,
  // rather than pretending the two do not overlap.
  if (!/FTDC\s*Summary/.test(form.replace(/<[^>]+>/g, ' '))) {
    throw new Error('the form does not mention the built-in FTDC Summary')
  }

  const dep = { state: 'running', config: { httpPort: 41999, fqdn: 'bighole-01.example.net', image: 'dbcanvas-bighole:896984f', ref: '896984fe9e9a7f1f59cc4ce29237625f8aaa713f' } }
  const html = renderToString(<BigHoleManager dep={dep} onDeleteNode={noop} />)
  // The link is localhost, NOT location.hostname — under SSR there is no location
  // at all, which is the case that would throw if this used the sim link.
  if (!/http:\/\/localhost:41999/.test(html)) throw new Error('the link is not localhost')
  // The commit is the version, shown short.
  if (!/896984fe9e9a/.test(html)) throw new Error('the built-from commit is not shown')
  if (renderToString(<BigHoleLink port={0} />) !== '') throw new Error('a portless node still rendered a link')
  return 'form, manager and a localhost link'
})

check('mongo downloads: the menu link at the right endpoint', () => {
  // One right-click entry on every MongoDB node — diagnostic.data and the log in a
  // single archive. What is in it and what the directory inside is called are the
  // server's business (app/mongodl.go); what the browser must get right is the
  // endpoint, because a wrong one here is a 404 on a menu item nobody will report.
  const diag = mongoDownloadURL(12, 'psmrs-01')
  if (diag !== '/api/stacks/12/nodes/psmrs-01/mongo/diagnostic') throw new Error(`diagnostic URL is ${diag}`)
  return 'diagnostic.data with the log'
})

check('mclusteradmin: the panel owns the credentials, not a connection string', () => {
  const panel = { id: 'mca1', type: 'mclusteradmin', label: 'mclusteradmin-01', viewOnly: true }
  const form = renderToString(<MClusterAdminForm node={panel} patchNode={noop} deleteNode={noop} />)
  // The passwords live here, and an empty field shows what it will actually be.
  if (!form.includes(MCA_DEFAULT_ADMIN_PW) || !form.includes(MCA_DEFAULT_RO_PW)) {
    throw new Error('the form does not show the default account passwords')
  }
  // And it says where the URI comes from, since nothing on the canvas supplies it.
  if (!/panel itself/.test(form)) throw new Error('the form does not say the URI is entered in the panel')
  if (/association line from/.test(form)) throw new Error('the form still asks for a line this node has no ports for')
  // A read-only panel says which account matches it.
  if (!/madmin-ro/.test(form)) throw new Error('a read-only panel does not name the read-only account')

  const dep = {
    state: 'running',
    config: { httpPort: 41234, fqdn: 'mclusteradmin-01.example.net', version: '0.3.7', viewOnly: true },
    secrets: { adminUser: 'madmin', adminPassword: 'madmin_password', readonlyUser: 'madmin-ro', readonlyPassword: 'madmin_ro_password' },
  }
  const html = renderToString(<MClusterAdminManager dep={dep} onDeleteNode={noop} />).replaceAll('<!-- -->', '')
  if (!/41234/.test(html)) throw new Error('the published port is not offered as a link')
  if (!/madmin-ro/.test(html)) throw new Error('the read-only account is not shown')
  if (/madmin_password/.test(html)) throw new Error('an account password is rendered in the clear')
  // The URI shape is shown as a template to type, not as a prepared string: there
  // is no prepared string any more, and claiming one would be a lie.
  if (!/mongodb:\/\/madmin/.test(html)) throw new Error('the manager does not show the URI shape')
  if (/Connection string/.test(html)) throw new Error('the manager still offers a prepared connection string')
  return 'credentials, a URI template, and no line'
})

check('bighole: the FTDC viewer says where it has to be opened', () => {
  const form = renderToString(
    <BigHoleForm node={{ id: 'bh1', type: 'bighole', label: 'bighole-01' }} patchNode={noop} deleteNode={noop} />)
  // The one thing that will otherwise waste somebody's afternoon: the app needs a
  // secure context, and over a host address it fails partway through ingest, which
  // reads like a broken decoder.
  if (!/localhost/.test(form)) throw new Error('the form does not say to open it on localhost')
  if (!/diagnostic\.data/.test(form)) throw new Error('the form does not say what to drag onto it')
  // And it points at the page in DBCanvas that reads the same files differently,
  // rather than pretending the two do not overlap.
  if (!/FTDC\s*Summary/.test(form.replace(/<[^>]+>/g, ' '))) {
    throw new Error('the form does not mention the built-in FTDC Summary')
  }

  const dep = { state: 'running', config: { httpPort: 41999, fqdn: 'bighole-01.example.net', image: 'dbcanvas-bighole:896984f', ref: '896984fe9e9a7f1f59cc4ce29237625f8aaa713f' } }
  const html = renderToString(<BigHoleManager dep={dep} onDeleteNode={noop} />)
  // The link is localhost, NOT location.hostname — under SSR there is no location
  // at all, which is the case that would throw if this used the sim link.
  if (!/http:\/\/localhost:41999/.test(html)) throw new Error('the link is not localhost')
  // The commit is the version, shown short.
  if (!/896984fe9e9a/.test(html)) throw new Error('the built-from commit is not shown')
  if (renderToString(<BigHoleLink port={0} />) !== '') throw new Error('a portless node still rendered a link')
  return 'form, manager and a localhost link'
})

check('tabs: clicking a page you already have focuses it rather than opening another', () => {
  const one = [{ key: 'a', id: 'dashboard' }]
  const same = openTab(one, 'a', 'dashboard', 'b')
  if (same.tabs !== one) throw new Error('focus-or-open opened a second tab of the same page')
  if (same.activeKey !== 'a') throw new Error(`activeKey = ${same.activeKey}, want the existing tab`)

  // A different page opens a new tab and takes focus.
  const two = openTab(one, 'a', 'benchmark', 'b')
  if (two.tabs.length !== 2 || two.activeKey !== 'b') throw new Error(`got ${JSON.stringify(two)}`)

  // force is what the right-click gesture passes: three benchmarks at once was
  // the case this whole thing exists for.
  let t = { tabs: one, activeKey: 'a' }
  for (const k of ['b', 'c', 'd']) t = openTab(t.tabs, t.activeKey, 'benchmark', k, true)
  if (t.tabs.filter((x) => x.id === 'benchmark').length !== 3) {
    throw new Error(`force did not open three benchmark tabs: ${JSON.stringify(t.tabs)}`)
  }
  if (tabCounts(t.tabs).benchmark !== 3) throw new Error('the nav badge count is wrong')
  return 'focus, open, and three of the same'
})

check('tabs: the cap refuses rather than evicting, and says that it did', () => {
  // A tab can hold a running benchmark. Closing one to make room is not a
  // decision to make on someone's behalf, so a capped open changes nothing —
  // except to focus that page if it is already open.
  const full = Array.from({ length: TABS_DEFAULT }, (_, i) => ({ key: `k${i}`, id: i === 3 ? 'labs' : `page${i}` }))
  const blocked = openTab(full, 'k0', 'benchmark', 'new', false, TABS_DEFAULT)
  if (blocked.tabs !== full) throw new Error('the cap evicted or grew past the limit')
  if (blocked.activeKey !== 'k0') throw new Error('a refused open moved focus somewhere unrelated')
  // Refusing in silence reads as a broken sidebar: you click a page and nothing
  // happens. The rule has to tell the caller, or App has nothing to show.
  if (!blocked.capped) throw new Error('a refused open did not report that it was capped')

  const focused = openTab(full, 'k0', 'labs', 'new', false, TABS_DEFAULT)
  if (focused.tabs !== full) throw new Error('at the cap, an already-open page should focus not grow')
  if (focused.activeKey !== 'k3') throw new Error(`want focus on the existing labs tab, got ${focused.activeKey}`)
  if (focused.capped) throw new Error('focusing a page that was already open is not a refusal')

  // Even force cannot exceed the cap — and a forced open at the cap IS a refusal,
  // because a second tab of that page is what was asked for and did not happen.
  const forced = openTab(full, 'k0', 'labs', 'new', true, TABS_DEFAULT)
  if (forced.tabs.length > TABS_DEFAULT) throw new Error('force exceeded the cap')
  if (!forced.capped) throw new Error('a forced open at the cap did not report a refusal')

  // The cap is a per-user setting, so it is an argument rather than a constant:
  // the same tab set is full at one limit and not at another.
  const room = openTab(full, 'k0', 'benchmark', 'new', false, TABS_DEFAULT + 1)
  if (room.tabs.length !== TABS_DEFAULT + 1 || room.capped) throw new Error('a raised limit still refused')
  const lowered = openTab(full.slice(0, 3), 'k0', 'benchmark', 'new', false, 3)
  if (!lowered.capped) throw new Error('a lowered limit did not take effect')

  // Below the floor and above the ceiling are clamped, not rejected, and junk
  // falls back to the default — the input is a number field somebody is mid-edit.
  if (clampTabs(500) !== TABS_MAX) throw new Error('over the ceiling was not clamped')
  if (clampTabs(1) !== TABS_MIN) throw new Error('under the floor was not clamped')
  if (clampTabs(undefined) !== TABS_DEFAULT) throw new Error('a missing setting is not the default')
  if (clampTabs('') !== TABS_DEFAULT) throw new Error('an empty field is not the default')
  return `capped at ${TABS_DEFAULT} by default, ${TABS_MIN}-${TABS_MAX} by setting`
})

check('tabs: the window tells you when the cap refused a click', () => {
  // React's server renderer separates adjacent text nodes with <!-- -->, so the
  // interpolated number lands mid-sentence as "All <!-- -->20<!-- --> tabs".
  const notice = renderToString(<TabCapNotice max={20} onDismiss={noop} />).replaceAll('<!-- -->', '')
  // The three things a dead click needs answering: what happened, what to do, and
  // where the number that caused it lives.
  if (!/All 20 tabs/.test(notice)) throw new Error('the notice does not say what the limit is')
  if (!/Close a tab/.test(notice)) throw new Error('the notice does not say how to get out of it')
  if (!/Settings/.test(notice)) throw new Error('the notice does not say where the limit is set')

  // The counter earns its place only near the cap.
  if (renderToString(<TabCount open={5} max={20} />) !== '') {
    throw new Error('the tab counter shows when nowhere near the limit')
  }
  const near = renderToString(<TabCount open={18} max={20} />).replaceAll('<!-- -->', '')
  if (!/18\/20/.test(near)) throw new Error(`the counter does not read n/max: ${near}`)
  const at = renderToString(<TabCount open={20} max={20} />)
  if (!/text-warning/.test(at)) throw new Error('at the cap the counter is not marked')
  return 'notice, and a counter that appears near the limit'
})

check('tabs: closing focuses the neighbour, and the last tab never closes', () => {
  const three = [{ key: 'a', id: 'dashboard' }, { key: 'b', id: 'benchmark' }, { key: 'c', id: 'labs' }]

  // Closing the active middle tab lands on what took its place.
  const mid = closeTab(three, 'b', 'b')
  if (mid.tabs.length !== 2 || mid.activeKey !== 'c') throw new Error(`got ${JSON.stringify(mid)}`)

  // Closing the active last tab lands on the new last.
  const last = closeTab(three, 'c', 'c')
  if (last.activeKey !== 'b') throw new Error(`closing the last tab should focus ${'b'}, got ${last.activeKey}`)

  // Closing an inactive tab must not move focus.
  const other = closeTab(three, 'a', 'c')
  if (other.activeKey !== 'a') throw new Error('closing an inactive tab moved focus')

  // The window is never left empty.
  const one = [{ key: 'a', id: 'dashboard' }]
  if (closeTab(one, 'a', 'a').tabs.length !== 1) throw new Error('the last tab was closed')

  // Closing something that is not there changes nothing.
  if (closeTab(three, 'a', 'nope').tabs !== three) throw new Error('closing an unknown key mutated the set')
  return 'neighbour focus, inactive close, last tab kept'
})

check('polling: a page that is not on screen never starts a timer', () => {
  // The reason this hook exists: App renders one page today, so a mounted page is
  // always the visible one. The moment several can be open at once, keeping a page
  // alive to preserve its scroll position also keeps its timer alive — and five
  // open pages become five loops asking a server with nothing new to say.
  //
  // Effects do not run under renderToString, so this checks the decision rather
  // than the timer: that the gate is a value the shell supplies, that it defaults
  // to open so today's pages are unaffected, and that a provider can close it.
  let seen = null
  function Probe() {
    seen = usePageVisible()
    usePolling(() => { throw new Error('a hidden page must not poll') }, 1000)
    return <div>probe</div>
  }
  renderToString(<Probe />)
  if (seen !== true) throw new Error(`default visibility = ${seen}, want true so an unwrapped page behaves as it does now`)

  renderToString(<PageVisibleProvider visible={false}><Probe /></PageVisibleProvider>)
  if (seen !== false) throw new Error(`provider did not close the gate, got ${seen}`)

  renderToString(<PageVisibleProvider visible={true}><Probe /></PageVisibleProvider>)
  if (seen !== true) throw new Error(`provider did not reopen the gate, got ${seen}`)
  return 'default open, provider closes and reopens'
})

check('handoff: no page reads a handoff straight out of sessionStorage', () => {
  // A handoff is one page telling another what to open — "chart this capture",
  // "analyse this cluster-dump", "open this timeline". Every one of them used to
  // be read in a mount-only effect, which was correct while App rendered a single
  // page and is wrong under tabs: an already-open tab is never unmounted, so
  // arriving at it ran no effect and the handoff sat unread while the page showed
  // whatever it showed before. That is not a visible failure — it looks like the
  // button did nothing — so the rule is checked rather than remembered.
  const { readFileSync, readdirSync } = nodeFs
  const offenders = []
  for (const dir of ['src/pages', 'src/components']) {
    for (const file of readdirSync(new URL(`../${dir}`, import.meta.url))) {
      if (!/\.jsx$/.test(file)) continue
      const text = readFileSync(new URL(`../${dir}/${file}`, import.meta.url), 'utf8')
      if (/sessionStorage\.(getItem|setItem)/.test(text)) offenders.push(`${dir}/${file}`)
    }
  }
  if (offenders.length) {
    throw new Error(`these touch sessionStorage directly instead of lib/handoff.js, and will miss a handoff to an already-open tab: ${offenders.join(', ')}`)
  }
  return 'every handoff goes through useHandoff/sendHandoff'
})

check('polling: every page that polls goes through the shared hook', () => {
  // A page that keeps its own setInterval would keep running behind a hidden tab,
  // which is exactly the bug this hook exists to prevent — and it would do it
  // silently. So the rule is checked rather than remembered.
  const { readFileSync, readdirSync } = nodeFs
  const dir = 'src/pages'
  const offenders = []
  for (const file of readdirSync(new URL(`../${dir}`, import.meta.url))) {
    if (!/\.jsx$/.test(file)) continue
    const text = readFileSync(new URL(`../${dir}/${file}`, import.meta.url), 'utf8')
    // setInterval is the polling primitive; setTimeout is used for one-shot
    // niceties (a toast that fades, a debounce) and is not a loop.
    if (text.includes('setInterval(') && !text.includes("from '../lib/usePolling.jsx'")) {
      offenders.push(file)
    }
  }
  if (offenders.length) {
    throw new Error(`these pages poll with a raw setInterval and would keep running while hidden: ${offenders.join(', ')}`)
  }
  return 'no page polls outside the hook'
})

check('operator summary: the page mounts before any capture is loaded', () =>
  renderToString(<OperatorSummary />))

check('operator summary: every panel renders a full model', () => {
  renderToString(<OpVerdicts verdicts={OP_MODEL.verdicts} />)
  renderToString(<OpFindings findings={OP_MODEL.findings} />)
  renderToString(<OpWorkloads workloads={OP_MODEL.workloads} />)
  renderToString(<OpPods pods={OP_MODEL.pods} />)
  renderToString(<OpCRs crs={OP_MODEL.crs} />)
  renderToString(<OpOperators operators={OP_MODEL.operators} />)
  renderToString(<OpDeployment deployment={OP_MODEL.deployment} />)
  renderToString(<OpImages images={OP_MODEL.images} />)
  renderToString(<OpSecrets secrets={OP_MODEL.secrets} />)
  renderToString(<OpBackups backups={OP_MODEL.backups} />)
  renderToString(<OpCerts certs={OP_MODEL.certs} />)
  renderToString(<OpStorage storage={OP_MODEL.storage} />)
  renderToString(<OpLogs logs={OP_MODEL.logs} />)
  renderToString(<OpGalera galera={OP_MODEL.galera} />)
  renderToString(<OpPodSummaries summaries={OP_MODEL.podSummaries} />)
  renderToString(<OpBackupLogs logs={OP_MODEL.backupLogs} />)
  renderToString(<OpExtras model={OP_MODEL} />)
  return `${OP_MODEL.findings.length} findings, ${OP_MODEL.pods.length} pods, ${OP_MODEL.backups.length} backups, ${OP_MODEL.certs.length} certs`
})

check('operator summary: an empty model says so rather than rendering nothing', () => {
  // A healthy cluster is a real result, and each panel has to say which kind of
  // empty it is — "every pod is fine" and "this capture had no operator in it"
  // are different answers and must not both render as blank.
  for (const html of [
    renderToString(<OpPods pods={[]} />),
    renderToString(<OpCRs crs={[]} />),
    renderToString(<OpOperators operators={[]} />),
    renderToString(<OpWorkloads workloads={[]} />),
    renderToString(<OpFindings findings={[]} />),
    renderToString(<OpDeployment />),
    renderToString(<OpImages images={[]} />),
    renderToString(<OpSecrets secrets={[]} />),
    renderToString(<OpBackups backups={[]} />),
    renderToString(<OpCerts certs={[]} />),
    renderToString(<OpStorage storage={[]} />),
    renderToString(<OpLogs />),
    renderToString(<OpGalera galera={[]} />),
    renderToString(<OpPodSummaries summaries={[]} />),
    renderToString(<OpBackupLogs logs={[]} />),
    renderToString(<OpExtras model={{}} />),
  ]) {
    if (!/[A-Za-z]{4}/.test(html)) throw new Error(`empty panel rendered nothing: ${html}`)
  }
  // Undefined must be as safe as empty — a model from an older capture may not
  // carry every key.
  renderToString(<OpPods />)
  renderToString(<OpCRs />)
  renderToString(<OpVerdicts />)
  return 'empty and undefined both render'
})

check('palette: every Infrastructure Library label is unique', () => {
  // `paletteGroups` lives inside StackEditor and is not exported, so this reads it back
  // out of the source the way the dangling-help check above does. The uniqueness is
  // load-bearing, not cosmetic: a label is the stable id of a "recently used" entry
  // (`paletteItems.find((it) => it.label === l)`), so two entries sharing one make the
  // second unreachable from recents and silently add the first instead. That is exactly
  // what "InnoDB / GR" did, in MySQL (Percona) and MySQL (Community), until it was found
  // by reading the file rather than by anything failing.
  const text = nodeFs.readFileSync(new URL('../src/pages/StackDesigner.jsx', import.meta.url), 'utf8')
  const start = text.indexOf('const paletteGroups = ')
  const end = text.indexOf('\n  ]', start)
  if (start < 0 || end < 0) throw new Error('could not find paletteGroups in StackDesigner.jsx')
  const block = text.slice(start, end)
  // And the catalog is filtered before anything reads it, which is what keeps the
  // experimental switch covering the docked library, its search and the add menu
  // at once — they all build from this one array.
  if (!/const paletteGroups = visibleGroups\(\[/.test(text)) {
    throw new Error('paletteGroups no longer goes through visibleGroups — experimental entries would be offered to everyone')
  }
  const labels = [...block.matchAll(/label: '([^']*)'/g)].map((m) => m[1])
  const groups = [...block.matchAll(/title: '([^']*)'/g)].map((m) => m[1])
  const seen = new Set()
  const dupes = labels.filter((l) => (seen.has(l) ? true : (seen.add(l), false)))
  if (dupes.length) throw new Error(`duplicate palette labels: ${[...new Set(dupes)].join(', ')}`)
  if (new Set(groups).size !== groups.length) throw new Error('duplicate palette group titles')
  return `${labels.length} entries across ${groups.length} categories, all distinct`
})

check('docs links: every docURL() in the app points at a file that exists', () => {
  // The app links out to its own documentation, and a link to a renamed or deleted page still
  // looks like a link — it just 404s for whoever clicks it, which nothing in a build notices.
  // The changelog's own doc paths have this in Go (TestNoteDocsExist); this is the other half,
  // for the ones written into the pages.
  const { readFileSync, readdirSync, existsSync } = nodeFs
  const dirs = ['src/pages', 'src/components', 'src/lib', 'src/settings']
  const files = dirs.flatMap((d) =>
    readdirSync(new URL(`../${d}`, import.meta.url)).filter((f) => /\.jsx?$/.test(f)).map((f) => `${d}/${f}`))
  let found = 0
  for (const rel of files) {
    const text = readFileSync(new URL(`../${rel}`, import.meta.url), 'utf8')
    for (const m of text.matchAll(/docURL\('([^']+)'\)/g)) {
      found++
      // ../../../ from app/web/smoke back to the repository root.
      if (!existsSync(new URL(`../../../${m[1]}`, import.meta.url))) {
        throw new Error(`${rel} links to ${m[1]}, which is not in the repository`)
      }
    }
  }
  // And nothing writes the repository URL out longhand any more, which is what let one of these
  // drift in the first place.
  for (const rel of files) {
    const text = readFileSync(new URL(`../${rel}`, import.meta.url), 'utf8')
    if (/github\.com\/jaimesicam\/dbcanvas/.test(text) && !rel.endsWith('lib/repo.js')) {
      throw new Error(`${rel} writes the repository URL out longhand — use docURL()`)
    }
  }
  if (found === 0) throw new Error('no docURL() links found — has the helper been renamed?')
  return `${found} links`
})

check('tooltip: no call site references help text that does not exist', () => {
  // The catalogs are plain objects, so a typo'd or never-written key is `undefined` —
  // which Help and Hint quietly treat as "no tooltip". That is the right runtime
  // behaviour and a terrible failure mode to ship: the control simply has no
  // explanation and nothing says so. This reads the sources back and checks every
  // reference resolves. (It caught 19 of them: a catalog section that was never
  // actually written to the file, behind a shell command that had silently failed.)
  const { readFileSync, readdirSync } = nodeFs
  const catalogs = { HELP, MENU_HELP, TOOL_HELP, DEP_HELP, MORE_HELP, FTDC_HELP }
  const dirs = ['src/pages', 'src/components', 'src/lib', 'src/settings']
  const files = dirs.flatMap((d) =>
    readdirSync(new URL(`../${d}`, import.meta.url)).filter((f) => /\.jsx?$/.test(f)).map((f) => `${d}/${f}`))
  const dangling = []
  for (const rel of files) {
    const text = readFileSync(new URL(`../${rel}`, import.meta.url), 'utf8')
    for (const [name, cat] of Object.entries(catalogs)) {
      for (const m of text.matchAll(new RegExp(`\\b${name}\\.([A-Za-z_]\\w*)`, 'g'))) {
        if (!(m[1] in cat)) dangling.push(`${rel}: ${name}.${m[1]}`)
      }
      for (const m of text.matchAll(new RegExp(`\\b${name}\\['([^']+)'\\]`, 'g'))) {
        if (!(m[1] in cat)) dangling.push(`${rel}: ${name}['${m[1]}']`)
      }
    }
  }
  if (dangling.length) throw new Error(`${dangling.length} dangling: ${dangling.slice(0, 8).join(', ')}`)
  return `${files.length} files, all references resolve`
})

check('tooltip: placement flips and clamps to stay on screen', () => {
  globalThis.window = { innerWidth: 1000, innerHeight: 800 }
  const bubble = { width: 300, height: 100 }
  const r = (top, left) => ({ top, bottom: top + 20, left, right: left + 20, width: 20, height: 20 })

  // Room above: the preferred side is used, and the bubble is centred on the trigger.
  const above = place(r(400, 500), bubble, 'top')
  if (above.top !== 400 - 100 - 8) throw new Error(`expected to sit above, got top ${above.top}`)
  if (above.left !== 510 - 150) throw new Error(`expected to centre, got left ${above.left}`)

  // Against the top edge there is no room above, so it flips below.
  const flipped = place(r(10, 500), bubble, 'top')
  if (flipped.top !== 10 + 20 + 8) throw new Error(`expected to flip below, got top ${flipped.top}`)

  // A trigger at the right edge — a field in a docked panel — is clamped, not centred.
  const clamped = place(r(400, 980), bubble, 'top')
  if (clamped.left !== 1000 - 300 - 6) throw new Error(`expected a right-edge clamp, got left ${clamped.left}`)
  if (clamped.left + bubble.width > 1000) throw new Error('bubble runs off the right edge')

  // And at the left edge it never goes negative.
  const left = place(r(400, 0), bubble, 'top')
  if (left.left < 0) throw new Error(`bubble runs off the left edge: ${left.left}`)

  // Taller than the viewport in both directions: still fully on screen.
  const tall = place(r(400, 500), { width: 300, height: 900 }, 'top')
  if (tall.top < 0 || tall.top + 900 > 800 + 900) throw new Error(`unclamped vertical: ${tall.top}`)
  delete globalThis.window
  return 'ok'
})

check('tooltips: switching them off takes the "?" with them', () => {
  // The preference is only visible through the context, so each case is rendered under
  // one — which is also the closest thing to how it reaches a real component.
  const under = (tooltips, node) => renderToString(
    <SettingsCtx.Provider value={{ settings: { tooltips }, save: noop, system: {}, saveSystem: noop, loaded: true }}>
      {node}
    </SettingsCtx.Provider>,
  )

  // On: the "?" is a real button, reachable from the keyboard.
  const on = under('on', <Help text="what this control is for" />)
  if (!/<button/.test(on)) throw new Error(`no "?" button with tooltips on: ${on}`)

  // Off: nothing at all. A "?" that cannot answer is worse than no "?", and it would
  // still take its space beside the label.
  const off = under('off', <Help text="what this control is for" />)
  if (off.replaceAll('<!-- -->', '').trim() !== '') throw new Error(`tooltips off still rendered: ${off}`)

  // What Hint wraps is CONTENT, not an affordance, so it survives — only the
  // explanation goes, and it goes without the wrapper that would shift the layout.
  const hintOff = under('off', <Hint text="the pool size in bytes"><b>3 GiB</b></Hint>)
  if (!hintOff.includes('3 GiB')) throw new Error(`the value went with the tooltip: ${hintOff}`)
  if (/<span/.test(hintOff)) throw new Error(`a disabled tooltip left its wrapper behind: ${hintOff}`)
  const hintOn = under('on', <Hint text="the pool size in bytes"><b>3 GiB</b></Hint>)
  if (!/<span/.test(hintOn) || !hintOn.includes('3 GiB')) throw new Error(`hint on = ${hintOn}`)

  // An unset preference — a stored row from before the field, a component rendered
  // before the fetch lands — is ON. Losing every explanation to a value nobody chose
  // would be the worst way for this to fail.
  const unset = under(undefined, <Help text="what this control is for" />)
  if (!/<button/.test(unset)) throw new Error('an unset preference hid the tooltips')
  const noProvider = renderToString(<Help text="what this control is for" />)
  if (!/<button/.test(noProvider)) throw new Error('outside the provider the tooltips vanished')
  return 'on, off, unset, and no provider'
})

check('settings: the tooltip switch', () => {
  const html = renderToString(<TooltipOptions value="on" onPick={noop} />)
  // The hints quote the "?" itself, which arrives escaped.
  const flat = html.replaceAll('&quot;', '"').replaceAll('&#x27;', "'").replaceAll('&amp;', '&')
  for (const m of TOOLTIP_MODES) {
    if (!flat.includes(m.label)) throw new Error(`the ${m.id} choice is not offered`)
    if (!flat.includes(m.hint)) throw new Error(`the ${m.id} choice does not say what it does`)
  }
  // Which one you get if you never touch this has to be on the row itself.
  if (!/Shown[\s\S]{0,120}\(default\)/.test(html)) throw new Error('the row does not mark the default')
  // And the row says what stays behind, or "Hidden" reads as "lose the help entirely".
  if (!/hint under a field stays/.test(html)) throw new Error('the row does not say what survives')
  const off = renderToString(<TooltipOptions value="off" onPick={noop} />)
  if (off === html) throw new Error('the row renders identically whichever is selected')
  return html
})

// --- the API page ---------------------------------------------------------------
//
// These render with fixtures shaped exactly like the server's responses, because the
// failure this file exists to catch is a missing Icon (Code, Key and Sparkles are all
// new) blanking the page the first time somebody opens it.

const apiTokens = [
  { id: 1, name: 'dbcanvas-cli on thinkpad', prefix: 'dbc_a1b2c3d4', scope: 'write',
    createdAt: '2026-09-01T10:00:00Z', expiresAt: '2026-12-01T10:00:00Z',
    lastUsedAt: '2026-09-02T08:00:00Z', state: 'active', username: 'jaime' },
  { id: 2, name: 'read-only dashboard', prefix: 'dbc_9f8e7d6c', scope: 'read',
    createdAt: '2026-08-01T10:00:00Z', expiresAt: '2026-08-31T10:00:00Z',
    state: 'expired', username: 'jaime' },
  { id: 3, name: 'leaked, revoked', prefix: 'dbc_00112233', scope: 'admin',
    createdAt: '2026-07-01T10:00:00Z', revokedAt: '2026-07-02T10:00:00Z',
    state: 'revoked', username: 'admin' },
  // No expiry at all — the admin-only case, and the one a naive date format breaks on.
  { id: 4, name: 'never expires', prefix: 'dbc_44556677', scope: 'write',
    createdAt: '2026-06-01T10:00:00Z', state: 'active', username: 'admin' },
]

const apiEndpoints = [
  { method: 'GET', path: '/api/stacks', group: 'Stacks', summary: 'The stacks you own.', auth: 'user', scope: 'read' },
  { method: 'POST', path: '/api/stacks/{id}/deploy', group: 'Stacks', summary: 'Provision every node.',
    auth: 'user', scope: 'write', params: [{ name: 'id', help: "The stack's numeric id." }] },
  { method: 'DELETE', path: '/api/users/{id}', group: 'Users', summary: 'Delete an account.',
    auth: 'admin', scope: 'admin', params: [{ name: 'id', help: "The account's numeric id." }] },
  { method: 'POST', path: '/api/ftdc/upload', group: 'FTDC Summary', summary: 'Upload diagnostic data.',
    auth: 'user', scope: 'write', media: 'multipart' },
  { method: 'GET', path: '/api/notifications/stream', group: 'Notifications', summary: 'Live events.',
    auth: 'user', scope: 'read', media: 'sse' },
  { method: 'GET', path: '/api/stacks/{id}/nodes/{nid}/term', group: 'Nodes', summary: 'Open a console.',
    auth: 'user', scope: 'read', media: 'websocket',
    params: [{ name: 'id', help: 'stack' }, { name: 'nid', help: 'node' }] },
  { method: 'GET', path: '/api/templates/{id}/export', group: 'Templates', summary: 'Download a template.',
    auth: 'user', scope: 'read', media: 'download', params: [{ name: 'id', help: 'template' }] },
]

check('api page: the whole thing mounts', () => renderToString(<Api />))

check('api page: token table, every state', () =>
  renderToString(<ApiTokenTable tokens={apiTokens} onRevoke={() => {}} />))

check('api page: token table with owners (the admin view)', () =>
  renderToString(<ApiTokenTable tokens={apiTokens} onRevoke={() => {}} showOwner />))

check('api page: an empty token list says what to do', () => {
  const html = renderToString(<ApiTokenTable tokens={[]} />)
  if (!/No tokens yet/.test(html)) throw new Error('the empty state does not explain itself')
  return html
})

check('api page: a loading token table does not claim to be empty', () => {
  const html = renderToString(<ApiTokenTable tokens={undefined} />)
  if (/No tokens yet/.test(html)) throw new Error('a pending fetch rendered as "no tokens"')
  return html
})

check('api page: the create form, as a user and as an admin', () => {
  const asUser = renderToString(
    <ApiCreateToken scopes={['read', 'write']} maxDays={90} canNeverExpire={false}
      onCreated={() => {}} onError={() => {}} />)
  if (/>Never</.test(asUser)) throw new Error('a non-admin was offered a never-expiring token')
  const asAdmin = renderToString(
    <ApiCreateToken scopes={['read', 'write', 'admin']} maxDays={365} canNeverExpire
      onCreated={() => {}} onError={() => {}} />)
  if (!/>Never</.test(asAdmin)) throw new Error('an admin was not offered a never-expiring token')
  if (!/>admin</.test(asAdmin)) throw new Error('an admin was not offered the admin scope')
  return asUser + asAdmin
})

check('api page: a low instance ceiling hides the longer lifetimes', () => {
  const html = renderToString(
    <ApiCreateToken scopes={['read', 'write']} maxDays={14} canNeverExpire={false}
      onCreated={() => {}} onError={() => {}} />)
  if (/>90 days</.test(html)) throw new Error('offered 90 days when the ceiling is 14')
  if (!/>7 days</.test(html)) throw new Error('did not offer a lifetime under the ceiling')
  return html
})

check('api page: the one-time secret panel', () => {
  const html = renderToString(
    <ApiFreshSecret data={{ token: apiTokens[0], secret: 'dbc_' + 'x'.repeat(43) }} onDismiss={() => {}} />)
  if (!/one and only time/.test(html)) throw new Error('the panel does not warn that this is the last look')
  return html
})

check('api page: an endpoint row, collapsed and every media kind', () =>
  apiEndpoints.map((ep) => renderToString(<ApiEndpointRow ep={ep} />)).join(''))

check('api page: snippets', () =>
  renderToString(<ApiSnippet label="curl" text={'curl -s http://x/api/stacks'} />))

check('api page: the CLI quick start takes you from a download to a command', () => {
  const html = renderToString(<ApiGettingStarted />)
  // The four steps somebody who has never seen this binary needs, in order. Before this the card
  // was three example lines and a link to a manual on GitHub — which an installation on a private
  // network cannot even open.
  for (const step of ['Download it', 'Put it on your PATH', 'Sign in once', 'Then do something']) {
    if (!html.includes(step)) throw new Error(`the quick start has no "${step}" step`)
  }
  // The install line names the file the download actually hands over, and makes it `dbcanvas`.
  if (!/chmod \+x dbcanvas-cli_/.test(html)) throw new Error('nothing says to make it executable')
  if (!/\/usr\/local\/bin\/dbcanvas/.test(html)) throw new Error('nothing puts it on the PATH')
  if (!html.includes('dbcanvas login --url')) throw new Error('the sign-in line is gone')
  // And the way out of the four steps: the CLI's own command list, and the manual.
  if (!html.includes('Every command')) throw new Error('the command list is not offered')
  if (!html.includes('docs/CLI.md')) throw new Error('the manual link is gone')
  return 'download, PATH, login, run'
})

check('api page: the command list, in each state it can be in', () => {
  // Fetched from the running installation's own binary (app/clihelp.go), so the page has to cope
  // with not having it yet, having it, and there being none — a development checkout has no
  // bundled CLI, and that must read as an explanation rather than a broken panel.
  const loading = renderToString(<ApiCliHelpText state={null} />)
  if (!/Asking the CLI/.test(loading)) throw new Error('nothing says it is loading')

  const help = renderToString(<ApiCliHelpText state={{ help: 'Usage:\n  dbcanvas <command>\n\nCommands:\n  login  Sign in\n' }} />)
  if (!help.includes('login') || !help.includes('Usage:')) throw new Error('the usage text is not shown')

  const none = renderToString(<ApiCliHelpText state={{ unavailable: 'no bundled dbcanvas-cli — `make cli` builds it' }} />)
  if (!none.includes('make cli')) throw new Error('an unavailable CLI does not say why')
  if (!renderToString(<ApiCliHelpText state={{ error: 'network' }} />).includes('network')) {
    throw new Error('a failed fetch says nothing')
  }
  // Collapsed until asked for, so a page nobody expands fetches nothing.
  const collapsed = renderToString(<ApiCliCommands />)
  if (collapsed.includes('Asking the CLI')) throw new Error('the panel fetches before it is opened')
  return 'loading, help, unavailable, error'
})

check('api page: getting started, with the CLI downloads', () => {
  const html = renderToString(<ApiGettingStarted />)
  if (!/api\/cli\/download\?os=linux&(amp;)?arch=amd64/.test(html)) {
    throw new Error('the Linux download link is missing or malformed')
  }
  return html
})

check('api page: the endpoint browser and the admin token list mount before their fetch lands', () =>
  renderToString(<ApiEndpoints />) + renderToString(<ApiAdminTokens onError={() => {}} />))

check('api page: curl lines suit the media kind', () => {
  const by = Object.fromEntries(apiEndpoints.map((ep) => [`${ep.method} ${ep.path}`, ep]))
  const get = curlFor(by['GET /api/stacks'], 'http://x')
  if (!/^curl -s /.test(get) || /-X GET/.test(get)) throw new Error(`GET: ${get}`)
  const post = curlFor(by['POST /api/stacks/{id}/deploy'], 'http://x')
  if (!/-X POST/.test(post) || /\{id\}/.test(post)) throw new Error(`POST kept a placeholder brace: ${post}`)
  const upload = curlFor(by['POST /api/ftdc/upload'], 'http://x')
  if (!/-F file=@/.test(upload)) throw new Error(`upload: ${upload}`)
  const stream = curlFor(by['GET /api/notifications/stream'], 'http://x')
  if (!/curl -N/.test(stream)) throw new Error(`sse: ${stream}`)
  const dl = curlFor(by['GET /api/templates/{id}/export'], 'http://x')
  if (!/-OJ/.test(dl)) throw new Error(`download: ${dl}`)
  // A WebSocket cannot be curled, and pretending otherwise would be worse than
  // saying so.
  const ws = curlFor(by['GET /api/stacks/{id}/nodes/{nid}/term'], 'http://x')
  if (/^curl/.test(ws) || !/WebSocket/.test(ws)) throw new Error(`websocket: ${ws}`)
  return get + post + upload + stream + dl + ws
})

check('api page: every wildcard is substituted in the examples', () => {
  for (const ep of apiEndpoints) {
    // The websocket "example" is a comment naming the endpoint, so it quotes the
    // path verbatim — braces and all — on purpose. Every actual command must not.
    const lines = ep.media === 'websocket' ? [cliFor(ep)] : [curlFor(ep, 'http://x'), cliFor(ep)]
    for (const line of lines) {
      if (/\{[a-zA-Z0-9_]+\}/.test(line)) throw new Error(`${ep.method} ${ep.path}: ${line}`)
    }
  }
  if (samplePath('/api/stacks/{id}/nodes/{nid}/fs/list') !== '/api/stacks/1/nodes/node-1/fs/list') {
    throw new Error(samplePath('/api/stacks/{id}/nodes/{nid}/fs/list'))
  }
  return 'ok'
})

check('api page: search matches path, summary and method', () => {
  const ep = apiEndpoints[1] // POST /api/stacks/{id}/deploy — "Provision every node."
  const cases = [['', true], ['deploy', true], ['DEPLOY', true], ['provision', true],
    ['POST /api/stacks', true], ['stacks deploy', true], ['benchmark', false], ['deploy benchmark', false]]
  for (const [q, want] of cases) {
    if (epMatches(ep, q) !== want) throw new Error(`matches(${JSON.stringify(q)}) !== ${want}`)
  }
  return 'ok'
})

check('api page: expiry is described in the units that matter', () => {
  const at = (ms) => new Date(Date.now() + ms).toISOString()
  const cases = [
    [{ state: 'active', expiresAt: at(30 * 86400000) }, /30 days left/],
    [{ state: 'active', expiresAt: at(5 * 3600000) }, /5 hours left/],
    [{ state: 'active', expiresAt: at(60000) }, /Under an hour/],
    [{ state: 'expired', expiresAt: at(-86400000) }, /Expired/],
    [{ state: 'revoked', expiresAt: at(86400000) }, /Revoked/],
    [{ state: 'active' }, /Never expires/],
    [{ state: 'active', expiresAt: 'nonsense' }, /—/],
  ]
  for (const [tok, want] of cases) {
    const got = expiryText(tok)
    if (!want.test(got)) throw new Error(`${JSON.stringify(tok)} -> ${got}`)
  }
  if (relDate(undefined) !== '—' || relDate('nonsense') !== '—') throw new Error('relDate on a missing stamp')
  return 'ok'
})

check('api page: every method, scope and media kind has a label', () => {
  for (const mm of ['GET', 'POST', 'PUT', 'DELETE']) {
    if (!METHOD_TONE[mm]) throw new Error(`no tone for ${mm}`)
  }
  for (const sc of ['read', 'write', 'admin']) {
    if (!SCOPE_TEXT[sc]) throw new Error(`no explanation for scope ${sc}`)
    if (!TOKEN_STATE_TONE.active) throw new Error('no tone for an active token')
  }
  for (const md of ['multipart', 'download', 'sse', 'websocket']) {
    if (!MEDIA_TEXT[md] || !MEDIA_LABEL[md]) throw new Error(`no label/explanation for media ${md}`)
  }
  if (!EXPIRY_CHOICES.some((c) => c.days === 0)) throw new Error('no "never" choice')
  return 'ok'
})

check('settings: the tab limit', () => {
  const html = renderToString(<TabLimit value={20} onSave={noop} />).replaceAll('<!-- -->', '')
  if (!/value="20"/.test(html)) throw new Error('the field does not show the current limit')
  if (!new RegExp(`min="${TABS_MIN}"`).test(html) || !new RegExp(`max="${TABS_MAX}"`).test(html)) {
    throw new Error('the field does not bound itself to the range the server enforces')
  }
  // The row has to say the cost, or a limit of 40 looks like free money.
  if (!/memory budget/.test(html)) throw new Error('the row does not say why there is a ceiling')
  if (!/never closes a tab/.test(html)) throw new Error('the row does not say what lowering it does')
  return html
})

check('settings: the look picker', () => {
  const html = renderToString(<LookOptions value="modern" onPick={noop} />)
  // Every look is offered, and each preview carries the data-look that re-points
  // the shape tokens — without it the four cards render identically and the
  // picker silently becomes four names for the same thing.
  for (const l of LOOKS) {
    if (!html.includes(l.label)) throw new Error(`the look ${l.id} is not offered`)
    if (!html.includes(`data-look="${l.id}"`)) throw new Error(`no live preview for the look ${l.id}`)
  }
  // A look is a shape, not a colour: the two axes must not share an id, or a
  // saved preference for one would validate as the other.
  for (const t of THEMES) {
    if (LOOKS.some((l) => l.id === t.id)) throw new Error(`${t.id} is both a theme and a look`)
  }
  return html
})

check('settings: the change-password form', () => {
  const html = renderToString(<SettingsChangePassword />)
  // Three password inputs, and the browser told which is which — otherwise a
  // password manager offers to fill the current one into the new field.
  if ((html.match(/type="password"/g) || []).length !== 3) {
    throw new Error('expected three password inputs')
  }
  // Matched case-insensitively: the attribute is written autoComplete in JSX and
  // React's server renderer keeps that casing, which HTML treats as the same
  // attribute but a case-sensitive regex does not.
  if (!/autocomplete="current-password"/i.test(html) || !/autocomplete="new-password"/i.test(html)) {
    throw new Error('the password fields are not labelled for password managers')
  }
  if (!/Also revoke my API tokens/.test(html)) throw new Error('no token-revocation opt-in')
  if (!/dbcanvas_reset_password/.test(html)) {
    throw new Error('the form should say what to do when the password is forgotten entirely')
  }
  return html
})

// --- What's new ------------------------------------------------------------------

const whatsNewData = {
  version: '0.2.0',
  seen: '0.1.0',
  hasUnseen: true,
  notes: [
    { version: '0.2.0', date: '2026-09-02', title: 'An HTTP API', body: 'Every endpoint, documented.', doc: 'docs/API.md' },
    { version: '0.1.0', date: '2026-09-01', title: 'Tooltips', body: '345 pieces of help.' },
  ],
  unseen: [
    { version: '0.2.0', date: '2026-09-02', title: 'An HTTP API', body: 'Every endpoint, documented.', doc: 'docs/API.md' },
  ],
}

check("what's new: the dialog, opened by itself", () => {
  const html = renderToString(
    <WhatsNew data={whatsNewData} open acknowledging onClose={() => {}} />)
  if (!/What&#x27;s new in DBCanvas|What's new in DBCanvas/.test(html)) throw new Error('no heading')
  if (!/Got it/.test(html)) throw new Error('no acknowledge button')
  // Auto-opened, it shows only the unread notes.
  if (/Tooltips/.test(html)) throw new Error('an already-read note was shown in the auto-open')
  return html
})

check("what's new: reopened from the link, it shows everything", () => {
  const html = renderToString(
    <WhatsNew data={whatsNewData} open acknowledging={false} onClose={() => {}} />)
  if (!/Tooltips/.test(html)) throw new Error('the full list is missing older notes')
  if (/Got it/.test(html)) throw new Error('a deliberate reopen should not offer to acknowledge')
  return html
})

check("what's new: closed renders nothing, and neither does an empty changelog", () => {
  if (renderToString(<WhatsNew data={whatsNewData} open={false} onClose={() => {}} />) !== '') {
    throw new Error('a closed dialog rendered something')
  }
  const empty = { version: '0.2.0', hasUnseen: false, notes: [], unseen: [] }
  if (renderToString(<WhatsNew data={empty} open onClose={() => {}} />) !== '') {
    throw new Error('an empty changelog rendered an empty dialog')
  }
  return 'ok'
})

check("what's new: the header link, with and without something unread", () => {
  const unread = renderToString(<WhatsNewLink data={whatsNewData} onClick={() => {}} />)
  if (!/0\.2\.0/.test(unread)) throw new Error('the link does not show the version')
  const read = renderToString(
    <WhatsNewLink data={{ ...whatsNewData, hasUnseen: false }} onClick={() => {}} />)
  if (read.length >= unread.length) throw new Error('the unread dot is not rendered')
  // Before the fetch lands there is nothing to link to.
  if (renderToString(<WhatsNewLink data={null} onClick={() => {}} />) !== '') {
    throw new Error('the link rendered before its data arrived')
  }
  return unread + read
})


// ---- the cr.yaml editor -------------------------------------------------------------
// The form is generated from the operator's CRD, so what has to be right here is not any one
// control but the contract with the API server: the patch. It is a JSON merge patch, and every
// one of its rules is a way to destroy a running cluster if it is wrong — a missing null leaves
// a field set, a merged array rewrites a backup schedule nobody touched.
const crSchema = {
  kind: 'PerconaXtraDBCluster', resource: 'pxc', group: 'pxc.percona.com', version: 'v1',
  cluster: 'cluster1', namespace: 'pxc', operator: 'pxc', status: 'ready',
  groups: [
    { id: 'cluster', label: 'Cluster', sections: ['pause', 'unsafeFlags'], note: 'The top of cr.yaml.' },
    { id: 'database', label: 'Database', sections: ['pxc'] },
    { id: 'backup', label: 'Backup', sections: ['backup'] },
  ],
  sections: [
    { name: 'pause', path: 'pause', type: 'boolean', help: 'Stop the cluster without deleting it.' },
    { name: 'unsafeFlags', path: 'unsafeFlags', type: 'object', fields: [
      { name: 'pxcSize', path: 'unsafeFlags.pxcSize', type: 'boolean', help: 'Allow fewer than three PXC pods.' },
    ] },
    { name: 'pxc', path: 'pxc', type: 'object', fields: [
      { name: 'size', path: 'pxc.size', type: 'integer', min: 1, default: 3, help: 'How many Galera members.' },
      { name: 'image', path: 'pxc.image', type: 'string' },
      { name: 'configuration', path: 'pxc.configuration', type: 'string' },
      { name: 'mysqlAllocator', path: 'pxc.mysqlAllocator', type: 'string', enum: ['jemalloc', 'tcmalloc'] },
      { name: 'affinity', path: 'pxc.affinity', type: 'raw', raw: true, rawWhy: 'opaque' },
      { name: 'replicationChannels', path: 'pxc.replicationChannels', type: 'array', items: 'object', element: [
        { name: '', path: 'pxc.replicationChannels[]', type: 'object', fields: [
          { name: 'name', path: 'pxc.replicationChannels[].name', type: 'string' },
          { name: 'isSource', path: 'pxc.replicationChannels[].isSource', type: 'boolean' },
        ] },
      ] },
    ] },
    { name: 'backup', path: 'backup', type: 'object', fields: [
      { name: 'pitr', path: 'backup.pitr', type: 'object', fields: [
        { name: 'enabled', path: 'backup.pitr.enabled', type: 'boolean' },
        { name: 'storageName', path: 'backup.pitr.storageName', type: 'string' },
        { name: 'timeBetweenUploads', path: 'backup.pitr.timeBetweenUploads', type: 'integer' },
      ] },
      { name: 'storages', path: 'backup.storages', type: 'object', map: true, element: [
        { name: '', path: 'backup.storages.*', type: 'object', fields: [
          { name: 'type', path: 'backup.storages.*.type', type: 'string', enum: ['s3', 'filesystem', 'azure'] },
        ] },
      ] },
      { name: 'schedule', path: 'backup.schedule', type: 'array', items: 'object', element: [
        { name: '', path: 'backup.schedule[]', type: 'object', fields: [
          { name: 'name', path: 'backup.schedule[].name', type: 'string' },
          { name: 'storageName', path: 'backup.schedule[].storageName', type: 'string' },
        ] },
      ] },
    ] },
  ],
  spec: {
    pause: false,
    pxc: { size: 3, image: 'percona/percona-xtradb-cluster:8.4', affinity: { antiAffinityTopologyKey: 'none' } },
    backup: {
      pitr: { enabled: true, storageName: 'seaweedfs-binlog', timeBetweenUploads: 60 },
      storages: { seaweedfs: { type: 's3' } },
      schedule: [{ name: 'daily-backup', storageName: 'seaweedfs' }],
    },
  },
}

check('cr editor: the patch is a merge patch, with every rule it has', () => {
  const orig = crSchema.spec
  // A scalar changed deep in the tree carries only its own path.
  let patch = crPatch(orig, { ...orig, backup: { ...orig.backup, pitr: { ...orig.backup.pitr, enabled: false } } })
  if (JSON.stringify(patch) !== '{"backup":{"pitr":{"enabled":false}}}') {
    throw new Error(`a nested change must not carry its siblings: ${JSON.stringify(patch)}`)
  }
  // A removed key is null — that is how a merge patch deletes, and sending it absent instead
  // would leave the field set on the cluster.
  const noStorage = JSON.parse(JSON.stringify(orig))
  delete noStorage.backup.pitr.storageName
  patch = crPatch(orig, noStorage)
  if (patch.backup.pitr.storageName !== null) throw new Error('a removed field must be sent as null')
  // Arrays are replaced wholesale: merging them by position rewrites entries nobody edited.
  patch = crPatch(orig, { ...orig, backup: { ...orig.backup, schedule: [{ name: 'weekly', storageName: 'seaweedfs' }] } })
  if (!Array.isArray(patch.backup.schedule) || patch.backup.schedule.length !== 1) {
    throw new Error('an edited list is sent whole')
  }
  // No edit, no patch — this is what the footer counts, and what stops an empty Apply.
  if (Object.keys(crPatch(orig, orig)).length) throw new Error('an untouched form must produce no patch')
  if (Object.keys(crPatch(orig, JSON.parse(JSON.stringify(orig)))).length) {
    throw new Error('a deep copy is not a change')
  }
  // The paths the footer and the field markers read.
  const paths = changedPaths(crPatch(orig, { ...orig, pause: true, pxc: { ...orig.pxc, size: 5 } }))
  if (!paths.includes('pause') || !paths.includes('pxc.size') || paths.length !== 2) {
    throw new Error(`changed paths: ${paths.join(', ')}`)
  }
  return 'ok'
})

check('cr editor: paths address the draft without mutating it', () => {
  const base = { pxc: { size: 3 }, backup: { pitr: { enabled: true } } }
  const next = setAt(base, 'backup.pitr.enabled', false)
  if (base.backup.pitr.enabled !== true) throw new Error('setAt mutated the original — the diff would vanish')
  if (getAt(next, 'backup.pitr.enabled') !== false) throw new Error('setAt did not write')
  if (getAt(next, 'pxc.size') !== 3) throw new Error('setAt dropped a sibling')
  const gone = delAt(next, 'backup.pitr.enabled')
  if (getAt(gone, 'backup.pitr.enabled') !== undefined) throw new Error('delAt did not remove')
  if (getAt(next, 'backup.pitr.enabled') !== false) throw new Error('delAt mutated its input')
  // A path into nothing is undefined, not a crash: the form asks for values that are not set.
  if (getAt(base, 'nope.nothing.here') !== undefined) throw new Error('a missing path must read as undefined')
  // setAt creates the objects on the way down, which is how a field inside an unset section is set.
  if (getAt(setAt(base, 'pmm.serverHost', 'pmm-01'), 'pmm.serverHost') !== 'pmm-01') {
    throw new Error('setAt must create the parents it needs')
  }
  return 'ok'
})

check('cr editor: the review panel prints the patch as cr.yaml reads', () => {
  const out = yamlish({ backup: { pitr: { enabled: true, storageName: 'seaweedfs-binlog' } }, pxc: { size: 5 } }, 1)
  if (!out.includes('enabled: true') || !out.includes('storageName: seaweedfs-binlog')) {
    throw new Error(`scalars are not printed: ${out}`)
  }
  if (!out.includes('  backup:')) throw new Error('nesting is not indented')
  const list = yamlish({ schedule: [{ name: 'daily' }] }, 0)
  if (!list.includes('- ')) throw new Error('a list is not printed as a list')
  if (yamlish({ storageName: null }, 0).indexOf('null') < 0) throw new Error('a deletion must be visible in the review')
  return out
})

check('cr editor: search finds a field by its path, not just its name', () => {
  const backup = crSchema.sections.find((s) => s.name === 'backup')
  if (!matchField(backup, 'pitr')) throw new Error('a section must match on a field inside it')
  if (!matchField(backup, 'timebetweenuploads')) throw new Error('search is case-insensitive')
  if (!matchField(backup, 'storagename')) throw new Error('a field inside an array element must be findable')
  if (matchField(backup, 'jemalloc')) throw new Error('backup must not match a pxc field')
  const pxc = crSchema.sections.find((s) => s.name === 'pxc')
  if (!matchField(pxc, 'galera')) throw new Error('the help text is searchable too — it is the only prose the CRD has')
  if (!matchField(pxc, '')) throw new Error('an empty search shows everything')
  return 'ok'
})

check('cr editor: the whole form renders from a CRD model', () => {
  const html = renderToString(
    <CRFormEditor stackId={1} frame={{ id: 'f1' }} isServer preloaded={crSchema} />)
  // The header says what is being edited — this is a live object, not a file.
  if (!html.includes('PerconaXtraDBCluster') || !html.includes('cluster1')) throw new Error('the header does not name the object')
  // SSR splits interpolated text with comment markers, so compare on the text alone.
  const text = html.replace(/<!--.*?-->/g, '')
  if (!text.includes('pxc.percona.com/v1')) throw new Error('the CRD version is what makes the form trustworthy; show it')
  // The groups, the search, and the footer's resting state.
  for (const want of ['Cluster', 'Database', 'Backup', 'Find a field', 'No changes']) {
    if (!text.includes(want)) throw new Error(`missing from the editor: ${want}`)
  }
  // A field's live value, its type hint and its full path all reach the screen.
  if (!text.includes('spec.pause')) throw new Error('a field must show the path it patches')
  if (!text.includes('boolean')) throw new Error('the type hint is the only machine truth on screen; show it')
  return html.length + ' bytes'
})

check('secrets editor: the patch carries only what changed, encoded per kind', () => {
  // What leaves the browser is the whole safety question here: half of these values are
  // passwords, and an editor that re-sends every key on every apply rotates credentials nobody
  // asked to rotate.
  const obj = {
    kind: 'secret', name: 'k3d-00-secrets', namespace: 'default',
    entries: [
      { key: 'root', size: 13, value: 'root_password' },
      { key: 'monitor', size: 7, value: 'monitor' },
      { key: 'tls.key', size: 64, binary: true },
    ],
  }
  const draft = { root: 'new_password', monitor: 'monitor' }

  const p = objectPatchOf(obj, draft, [])
  if (Object.keys(p.set).join(',') !== 'root') throw new Error(`sent ${JSON.stringify(p.set)}`)
  if (p.set.root !== 'new_password') throw new Error('the new value did not travel')
  if (patchCount(p) !== 1) throw new Error(`counted ${patchCount(p)}`)

  // A binary key is never in the patch, even though it is on screen.
  if ('tls.key' in objectPatchOf(obj, { ...draft, 'tls.key': 'oops' }, []).set) {
    throw new Error('a binary key must not be writable')
  }

  // A removal names the key and drops any edit to it; a brand-new key is a set.
  const rm = objectPatchOf(obj, draft, ['monitor'])
  if (rm.remove.join(',') !== 'monitor') throw new Error(`remove = ${rm.remove}`)
  const added = objectPatchOf(obj, { ...draft, newkey: 'v' }, [])
  if (added.set.newkey !== 'v') throw new Error('a new key should be set')
  // Removing a key that was never in the object is nothing to send, not a null.
  if (objectPatchOf(obj, draft, ['ghost']).remove.length) throw new Error('a phantom removal was sent')
  return `${patchCount(rm)} pending`
})

check('secrets editor: review shows a config file and masks a password', () => {
  // Review exists so an apply is never a leap of faith — but a review panel that prints every
  // password at once undoes the point of revealing them one at a time.
  const secret = reviewText('secret', { set: { root: 'hunter2' }, remove: ['monitor'] })
  if (secret.includes('hunter2')) throw new Error('the review printed a password')
  if (!secret.includes('7 characters')) throw new Error('the review should say how long it is')
  if (!secret.includes('monitor: <removed>')) throw new Error('a removal must be visible in the review')

  const cm = reviewText('configmap', { set: { 'my.cnf': '[mysqld]\nmax_connections=1000' }, remove: [] })
  if (!cm.includes('max_connections=1000')) throw new Error('reviewing a my.cnf you cannot see is not a review')
  return 'masked, printed'
})

check('secrets editor: a value gets the editor its shape needs', () => {
  if (isMultiline('root_password')) throw new Error('a password is one line')
  if (!isMultiline('[mysqld]\nmax_connections=1000')) throw new Error('a config file needs a textarea')
  if (!isMultiline('x'.repeat(80))) throw new Error('80 characters in a panel-width input is unreadable')
  if (sizeLabel(13) !== '13 B' || sizeLabel(2048) !== '2.0 KiB') throw new Error(`sizeLabel: ${sizeLabel(13)}, ${sizeLabel(2048)}`)
  return 'input, textarea, sizes'
})

check('secrets editor: the panel mounts, and a worker node says where to go', () => {
  const html = renderToString(<K8sObjectEditor stackId={1} frame={{ id: 'f1' }} isServer />)
  const text = html.replace(/<!--.*?-->/g, '')
  for (const want of ['Secrets', 'ConfigMaps', 'Reading the cluster']) {
    if (!text.includes(want)) throw new Error(`missing from the editor: ${want}`)
  }
  const worker = renderToString(<K8sObjectEditor stackId={1} frame={{ id: 'f1' }} isServer={false} />)
  if (!worker.replace(/<!--.*?-->/g, '').includes('server')) throw new Error('a worker node should point at the server')
  return html.length + ' bytes'
})

check('cr editor: the sections a group names are the ones it renders', () => {
  const db = renderToString(<CRFormEditor stackId={1} frame={{ id: 'f1' }} isServer preloaded={crSchema} />)
  // The first group is selected, so its sections are on screen and another group's are not.
  if (!db.includes('unsafeFlags')) throw new Error('the first group renders its own sections')
  if (db.includes('mysqlAllocator')) throw new Error('another group’s fields must not render until it is chosen')
  // A worker node has no kubectl, and the editor says so rather than failing to load.
  const worker = renderToString(<CRFormEditor stackId={1} frame={{ id: 'f1' }} isServer={false} />)
  if (!worker.includes('server')) throw new Error('an agent node must be told where the custom resource lives')
  return 'ok'
})


// ---- kubectl / Helm on a Linux Client -------------------------------------------------
// The version is the whole point of choosing this at design time rather than typing it into the
// node's terminal: kubectl is supported one minor either side of the API server, and the cluster
// it will talk to is on the same canvas. So the form has to SAY which version, and change its mind
// when a cluster is added.
check('linux client: the kubectl version follows the cluster on the canvas', () => {
  const node = { id: 'lc1', type: 'linuxclient', label: 'linuxclient1', lcKubectl: true, lcHelm: true, useProxy: false }
  const withCluster = renderToString(
    <K8sToolFields node={node} patchNode={noop} deployed={false}
      frames={[{ id: 'f1', type: 'k3d', label: 'k3d-00', k3dK3sVersion: 'v1.36.4-k3s1' }]} />)
  if (!withCluster.includes('k3d-00')) throw new Error('the form must name the cluster it matches')
  if (!withCluster.includes('v1.36.4')) throw new Error('the form must show the kubectl version it will install')
  if (withCluster.includes('k3s1')) throw new Error('the k3s suffix is not a kubectl release')
  // No cluster yet: say what happens instead of showing a version pulled from nowhere.
  const alone = renderToString(<K8sToolFields node={node} patchNode={noop} deployed={false} frames={[]} />)
  if (!alone.includes('No Kubernetes cluster on this canvas yet')) throw new Error('a lone client must say where its version comes from')
  // A frame with no pinned k3s ("latest") still names the cluster — the server resolves the tag.
  const unpinned = renderToString(
    <K8sToolFields node={node} patchNode={noop} deployed={false} frames={[{ id: 'f1', type: 'k3d', label: 'k3d-01', k3dK3sVersion: '' }]} />)
  if (!unpinned.includes('k3d-01')) throw new Error('an unpinned frame still decides the version')
  // Both tools off: no kubeconfig advice, because there is nothing to advise about.
  const off = renderToString(<K8sToolFields node={{ id: 'lc2' }} patchNode={noop} deployed={false} frames={[]} />)
  if (off.includes('kubeconfig')) throw new Error('the kubeconfig note belongs to a node that installs something')
  if (!withCluster.includes('kubeconfig')) throw new Error('a node with the tools on must be told it has no kubeconfig')
  return 'ok'
})

// ---- the Backups tab ------------------------------------------------------------------
// The tab renders four panes over live cluster state, and every one of them is reached by a
// click rather than by a route — which is exactly the shape that used to ship broken, because
// `vite build` is happy to compile a component that throws the moment it is mounted.

check('backups: the panel mounts, and a worker node says where to go', () => {
  const html = renderToString(<K8sBackupManager stackId={1} frame={{ id: 'f1' }} isServer />)
  // With no data loaded yet (effects do not run under SSR) the panel must still render
  // something honest rather than crash on an absent response.
  if (!html.replace(/<!--.*?-->/g, '').includes('Loading')) {
    throw new Error('the panel should say it is loading before the first answer')
  }
  const worker = renderToString(<K8sBackupManager stackId={1} frame={{ id: 'f1' }} isServer={false} />)
  if (!worker.replace(/<!--.*?-->/g, '').includes('server')) {
    throw new Error('a worker node should point at the server')
  }
  return html.length + ' bytes'
})

check('backups: a byte count reads the same here as it does in the Go handler', () => {
  // app/k3dbucket.go's byteSizeLabel produces these exact strings, and the "too large to
  // stream" error quotes the cap using it — the two must not disagree in a screenshot.
  const cases = [[512, '512 B'], [2048, '2.0 KiB'], [5 << 20, '5.0 MiB'], [3 * (1 << 30), '3.0 GiB']]
  for (const [n, want] of cases) {
    if (bkSizeLabel(n) !== want) throw new Error(`sizeLabel(${n}) = ${bkSizeLabel(n)}, want ${want}`)
  }
  return 'sizes agree'
})

check('backups: a timestamp reads as an age, and a missing one as nothing', () => {
  if (whenLabel('') !== '—') throw new Error('a backup with no completion time must not render as Invalid Date')
  if (whenLabel('not a date') !== 'not a date') throw new Error('an unparseable time should be shown as sent')
  const justNow = new Date(Date.now() - 5000).toISOString()
  if (!whenLabel(justNow).endsWith('s ago')) throw new Error(`a fresh backup should read in seconds: ${whenLabel(justNow)}`)
  const anHour = new Date(Date.now() - 3600 * 1000).toISOString()
  if (whenLabel(anHour) !== '1h ago') throw new Error(`an hour should read as 1h ago: ${whenLabel(anHour)}`)
  return 'ages'
})

check('backups: the bucket breadcrumb walks back up the prefix', () => {
  const crumbs = crumbsOf('pgbackrest/cluster1/repo1')
  if (crumbs.length !== 3) throw new Error('three segments, three crumbs')
  if (crumbs[0].key !== 'pgbackrest') throw new Error('the first crumb is the first segment alone')
  // Each crumb's key must be the WHOLE path up to it, or clicking one jumps to the wrong folder.
  if (crumbs[2].key !== 'pgbackrest/cluster1/repo1') throw new Error(`last crumb: ${crumbs[2].key}`)
  if (crumbs[1].name !== 'cluster1') throw new Error('a crumb shows its own segment, not the path')
  if (crumbsOf('').length !== 0) throw new Error('the bucket root has no crumbs')
  if (crumbsOf('a//b').length !== 2) throw new Error('empty segments are not crumbs')
  return crumbs.map((c) => c.name).join(' / ')
})

if (failures > 0) {
  console.error(`\n${failures} render failure(s)`)
  process.exit(1)
}
console.log('\nall render checks passed')
