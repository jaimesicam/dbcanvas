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
import { ContextMenu, podMenuEntries } from '../src/pages/StackDesigner.jsx'

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

// ---------------------------------------------------------------- the pod console
//
// The one piece of UI in this app that only exists at hover time: the pod console's
// menu is four levels deep and its contents are FETCHED when the submenu opens, so
// neither the SSR check next door (no effects, no pointer) nor a --dump-dom of a
// mounted page can see any of it. Driving it with real mouse events in a real
// browser is the only way to find out that a namespace row opens a pod row, that a
// panel three levels in is on screen rather than clipped away inside its scrolling
// parent, and that the leaf actually calls back with all four parts.
const PODS = [
  { namespace: 'default', name: 'cluster1-pxc-0', phase: 'Running', containers: [
    { name: 'pxc', state: 'running', ready: true },
    { name: 'pxc-init', state: 'terminated', ready: true, init: true },
  ] },
  { namespace: 'kube-system', name: 'coredns-abc', phase: 'Running', containers: [
    { name: 'coredns', state: 'running', ready: true },
  ] },
]
let menuDone = false
async function drivePodMenu() {
  const picked = []
  let fetches = 0
  const host = document.createElement('div')
  document.body.appendChild(host)
  createRoot(host).render(
    <ContextMenu
      menu={{ x: 40, y: 40 }}
      onClose={() => {}}
      actions={[{
        label: 'Enter pod console',
        key: 'pods:k3s-01',
        empty: 'No pods',
        items: () => { fetches++; return Promise.resolve(podMenuEntries(PODS, (p) => picked.push(p))) },
      }]}
    />,
  )
  const tick = () => new Promise((r) => setTimeout(r, 20))
  // A submenu row's button holds its label, its child count and a chevron, so the
  // label is the first span when there is one and the whole button when there is not.
  const label = (b) => (b.querySelector('span') || b).textContent.trim()
  const row = (text) => [...document.querySelectorAll('button')].find((b) => label(b) === text)
  // React renders concurrently and the submenu's items arrive from a promise, so
  // every step waits for its row rather than assuming one turn was enough.
  const waitRow = async (text) => {
    for (let i = 0; i < 40; i++) {
      const el = row(text)
      if (el) return el
      await tick()
    }
    throw new Error(`no menu row "${text}" (have: ${[...document.querySelectorAll('button')].map(label).join(' | ')})`)
  }
  const open = async (text) => {
    const el = await waitRow(text)
    el.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }))
    await tick()
    return el
  }

  await open('Enter pod console')
  await open('default')
  await open('cluster1-pxc-0')
  // A container that is not running is offered and refused, not hidden.
  const init = await waitRow('pxc-init · init')
  if (!init || !init.disabled) throw new Error('the terminated init container should be shown and disabled')
  await open('pxc')

  // Five panels are open at once — the root plus one per level — which is the whole
  // claim the nesting makes. (Where each one SITS is menuPos/submenuPos's business
  // and is checked off-browser next door; this harness loads no stylesheet, so
  // nothing here is laid out where a user would see it.)
  const panels = document.querySelectorAll('[data-menu-panel]')
  if (panels.length !== 5) throw new Error(`expected 5 open panels (root + 4 levels), found ${panels.length}`)

  ;(await waitRow('bash')).click()
  if (picked.length !== 1) throw new Error(`the shell leaf called back ${picked.length} times`)
  const want = { namespace: 'default', name: 'cluster1-pxc-0', container: 'pxc', shell: 'bash' }
  if (JSON.stringify(picked[0]) !== JSON.stringify(want)) throw new Error(`picked ${JSON.stringify(picked[0])}`)

  // Hovering off the row and back on it must not ask the cluster again: the cache
  // lives as long as the open menu, and dies with it.
  await open('Enter pod console')
  if (fetches !== 1) throw new Error(`the pod list was fetched ${fetches} times in one open of the menu`)
  menuDone = true
}
drivePodMenu().catch((err) => record('pod console menu', err))

// Give effects, their microtasks and the stubbed fetches a turn, then report.
setTimeout(() => {
  const blank = PAGES.filter(([name]) => (document.getElementById(`page-${name}`)?.textContent || '').trim() === '')
    .map(([name]) => name)
  const out = document.createElement('pre')
  out.id = 'result'
  if (!menuDone && !failures.some((f) => f.startsWith('pod console menu'))) {
    record('pod console menu', new Error('never finished — did a submenu stop opening?'))
  }
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
}, 2500)
