// smoke/browser.mjs — run the browser-mount check (smoke/browser.jsx).
//
// `npm run smoke` renders components under node, where effects never run. That is
// deliberate (it needs no browser and no network) and it is also blind to the most
// common way a page breaks: an effect that throws on the first render, which React
// answers by unmounting the whole tree — a blank page. Two pages shipped exactly
// that way, from an effect reading `targets.length` while targets was still null.
//
// So this starts Vite in-process, mounts every heavy page in headless Chrome with
// fetch and sockets stubbed, and fails on anything React or the browser reports.
//
// Chrome is not a dependency of this project, so a machine without it SKIPS rather
// than fails: `npm run smoke` stays the hard gate. Point CHROME at a binary to
// override the search.
import { createServer } from 'vite'
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'

const CANDIDATES = [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/snap/bin/chromium',
].filter(Boolean)

const chrome = CANDIDATES.find((p) => existsSync(p))
if (!chrome) {
  console.log('browser mount check SKIPPED — no Chrome found. Set CHROME=<path> to run it.')
  process.exit(0)
}

const server = await createServer({ configFile: 'vite.config.js', server: { port: 0, host: '127.0.0.1' } })
await server.listen()
const { port } = server.httpServer.address()
const url = `http://127.0.0.1:${port}/smoke/browser.html`
console.log(`mounting pages in ${chrome.split('/').pop()} at ${url}`)

const dom = await new Promise((resolve, reject) => {
  const args = [
    '--headless=new', '--disable-gpu', '--no-sandbox', '--no-first-run',
    // The harness reports after a fixed delay; virtual time lets Chrome run the
    // page's timers as fast as it can rather than waiting in wall-clock seconds.
    '--virtual-time-budget=10000', '--dump-dom', url,
  ]
  const p = spawn(chrome, args, { stdio: ['ignore', 'pipe', 'ignore'] })
  let out = ''
  p.stdout.on('data', (d) => { out += d })
  p.on('error', reject)
  p.on('close', () => resolve(out))
})
await server.close()

const result = /<pre id="result">([\s\S]*?)<\/pre>/.exec(dom)
if (!result) {
  console.log('FAIL — the harness never reported. Did a page hang, or did the bundle fail to load?')
  console.log(dom.slice(0, 1500))
  process.exit(1)
}
const text = result[1]
  .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#x27;/g, "'").replace(/&amp;/g, '&')
if (!text.startsWith('ALL PAGES MOUNTED')) {
  console.log(text)
  console.log('\nbrowser mount check FAILED')
  process.exit(1)
}
console.log('  ok    every page mounted in a real browser')
console.log('\nbrowser mount check passed')
