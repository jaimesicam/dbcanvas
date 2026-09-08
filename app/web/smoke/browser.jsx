// smoke/browser.jsx — mount whole pages in a REAL browser and fail on a crash.
//
// The SSR check next door renders components without ever running an effect, which
// is the half of a page most likely to throw: a mount fetch, a WebSocket, a
// useEffect that dereferences state the first render left null. A page that blanks
// on the way in passes that check and fails for the user.
//
// So this one mounts the page component the way App does, with fetch and WebSocket
// stubbed so nothing leaves the process, and records anything React or the browser
// reports. Failures land in the DOM and in document.title, so a headless
// --dump-dom is enough to read them — no devtools protocol needed.
import { createRoot } from 'react-dom/client'
import { StrictMode, Component } from 'react'
import { PageVisibleProvider } from '../src/lib/usePolling.jsx'
import { TerminalProvider } from '../src/terminal/TerminalProvider.jsx'
import OperatorDebugger from '../src/pages/OperatorDebugger.jsx'
import CoreDumpAnalyzer from '../src/pages/CoreDumpAnalyzer.jsx'
import OperatorSummary from '../src/pages/OperatorSummary.jsx'
import LogSummary from '../src/pages/LogSummary.jsx'
import FTDCSummary from '../src/pages/FTDCSummary.jsx'
import StalkSummary from '../src/pages/StalkSummary.jsx'
import PacketInspector from '../src/pages/PacketInspector.jsx'

const failures = []
const record = (what, err) => failures.push(`${what}: ${err?.message || err}\n${(err?.stack || '').split('\n').slice(1, 4).join('\n')}`)

addEventListener('error', (e) => record('window.onerror', e.error || e.message))
addEventListener('unhandledrejection', (e) => record('unhandled rejection', e.reason))

// The API, stubbed. Shapes come from the same fixtures the SSR check uses, so a
// page gets a target list it will actually try to render rather than an empty one.
const dbgTarget = {
  stackId: 1, frameId: 'f1', stackName: 'delve-verify', label: 'k3d-01',
  operator: 'pxc', operatorVer: '1.20.0', cr: 'k3d-01', namespace: 'pxc',
  buildDir: '/go/src/github.com/percona/percona-xtradb-cluster-operator',
  hostPort: 40000, nodePort: 30400, debugStatus: 'listening',
  startFile: 'pkg/controller/pxc/controller.go', presets: [],
}
const gdbTarget = {
  stackId: 1, stackName: 'crash lab', nodeId: 'lc1', label: 'linuxclient1',
  hostname: 'linuxclient1', os: 'oraclelinux', osVersion: '8',
  product: 'ps', major: '8.0', version: '8.0.16-7.1',
  binary: '/sysroot/mysqld', binaryFrom: 'mounted', buildId: '3f2a9c', hasSymbols: true,
  coreDir: '/srv/coredumps/db7/cores', libDir: '/srv/coredumps/db7/libs', status: 'ready',
}
const payloadFor = (url) => {
  if (url.includes('/k3d/debug/targets')) return { targets: [dbgTarget] }
  if (url.includes('/gdb/targets')) return { targets: [gdbTarget] }
  if (url.includes('/cores')) return { cores: [] }
  if (url.includes('/files')) return { files: [], entries: [] }
  if (url.includes('/breakpoints')) return { breakpoints: [] }
  if (url.includes('/stacks')) return { stacks: [] }
  if (url.includes('/bundles')) return { bundles: [] }
  if (url.includes('/archives')) return { archives: [] }
  if (url.includes('/dumps')) return { dumps: [] }
  if (url.includes('/captures')) return { captures: [] }
  if (url.includes('/settings')) return { terminalMode: 'docked', theme: 'dark', look: 'modern', maxTabs: 20 }
  // Default to an ARRAY. Most list endpoints return a bare JSON array, and an
  // object here invents crashes the app does not have: /api/ftdc/targets returns
  // [] and a {} stub made FTDCSummary's nodes.map throw. A check that cries wolf
  // gets ignored, which costs more than the check is worth.
  return []
}
window.fetch = (input) => {
  const url = typeof input === 'string' ? input : input?.url || ''
  return Promise.resolve({
    ok: true, status: 200, headers: { get: () => 'application/json' },
    json: () => Promise.resolve(payloadFor(url)),
    text: () => Promise.resolve(JSON.stringify(payloadFor(url))),
  })
}
// Nothing may open a socket: a page that reconnects on close would spin forever.
class DeadSocket {
  constructor() { this.readyState = 3 }
  send() {} close() {} addEventListener() {} removeEventListener() {}
}
window.WebSocket = DeadSocket
window.EventSource = class { constructor() {} close() {} addEventListener() {} }

class Boundary extends Component {
  constructor(p) { super(p); this.state = { err: null } }
  static getDerivedStateFromError(err) { return { err } }
  componentDidCatch(err) { record(`<${this.props.name}> threw during render`, err) }
  render() { return this.state.err ? null : this.props.children }
}

const PAGES = [
  ['OperatorDebugger', OperatorDebugger],
  ['CoreDumpAnalyzer', CoreDumpAnalyzer],
  ['OperatorSummary', OperatorSummary],
  ['LogSummary', LogSummary],
  ['FTDCSummary', FTDCSummary],
  ['StalkSummary', StalkSummary],
  ['PacketInspector', PacketInspector],
]

// Mounted the way App mounts a tab: inside the terminal + page-visible providers,
// visible, in StrictMode — which double-invokes effects, so an effect that cannot
// run twice is caught here too.
for (const [name, Page] of PAGES) {
  const host = document.createElement('div')
  host.id = `page-${name}`
  document.body.appendChild(host)
  try {
    createRoot(host).render(
      <StrictMode>
        <TerminalProvider>
          <PageVisibleProvider visible>
            <Boundary name={name}><Page /></Boundary>
          </PageVisibleProvider>
        </TerminalProvider>
      </StrictMode>,
    )
  } catch (err) {
    record(`<${name}> threw while mounting`, err)
  }
}

// Give effects, their microtasks and the stubbed fetches a turn, then report.
setTimeout(() => {
  const blank = PAGES.filter(([name]) => (document.getElementById(`page-${name}`)?.textContent || '').trim() === '')
    .map(([name]) => name)
  const out = document.createElement('pre')
  out.id = 'result'
  if (failures.length === 0 && blank.length === 0) {
    out.textContent = 'ALL PAGES MOUNTED'
    document.title = 'OK'
  } else {
    out.textContent = [
      ...(blank.length ? [`BLANK: ${blank.join(', ')}`] : []),
      ...failures,
    ].join('\n\n')
    document.title = `FAIL(${blank.length + failures.length})`
  }
  document.body.appendChild(out)
}, 1200)
