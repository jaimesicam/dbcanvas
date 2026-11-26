// scripts/screenshots.mjs — retake the documentation's screenshots from a running DBCanvas.
//
//   node scripts/screenshots.mjs SHOTS.json START_LINK [OUT_DIR]
//
// START_LINK is a one-time sign-in link (`dbcanvas open --print`); OUT_DIR defaults to
// docs/screenshots. SHOTS.json is a list of shots, each one page and the steps that bring it to
// what the picture shows:
//
//   [{ "file": "stacks-list.png", "path": "/#stacks", "height": 1000,
//      "steps": [{ "waitFor": "text=Database Stacks" }, { "click": "text=pxc-lab" }, { "wait": 1500 }] }]
//
// Steps: waitFor (a CSS selector, or text=… for the first element whose text contains it),
// click (the same), eval (JavaScript run in the page), type ({ "into": selector, "text": … }),
// select ({ "in": selector, "value": … } or { "in": …, "label": text of the option }, for a <select>), rightclick (a selector: its context menu),
// window (a desktop window's title: closes the others, brings it forward, maximises it),
// text (typed into whatever has the focus),
// key (a key name such as "Escape"), wait (milliseconds), scroll (a selector to scroll into view).
// Pictures are 1600 pixels wide at twice the pixel density, as the existing ones are.
//
// Take them from an instance whose owner and domains are neutral: a test user (testuser01) and
// example.com/.net/.org names; look at each picture before committing it.
import { spawn } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

const [shotsFile, startLink, outDir = 'docs/screenshots'] = process.argv.slice(2)
if (!shotsFile || !startLink) {
  console.error('usage: node scripts/screenshots.mjs SHOTS.json START_LINK [OUT_DIR]')
  process.exit(2)
}
const chrome = [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
].filter(Boolean).find((p) => existsSync(p))
if (!chrome) {
  console.error('no Chrome found; set CHROME to its binary')
  process.exit(2)
}

const shots = JSON.parse(readFileSync(shotsFile, 'utf8'))
const origin = new URL(startLink).origin
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const profile = join(tmpdir(), `dbc-shots-${process.pid}`)
const port = 9300 + (process.pid % 500)
const proc = spawn(chrome, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--hide-scrollbars', `--user-data-dir=${profile}`, `--remote-debugging-port=${port}`,
  // A fresh profile on macOS asks the login keychain for "Chrome Safe Storage" on every launch,
  // and the person at the screen gets the prompt: keep this Chrome off the keychain entirely.
  '--use-mock-keychain', '--password-store=basic', 'about:blank',
], { stdio: 'ignore' })

let ws
let seq = 0
const pending = new Map()
const trace = process.env.SHOTS_TRACE ? (...a) => console.error('trace', ...a) : () => {}
function send(method, params = {}) {
  const id = ++seq
  trace('->', id, method)
  ws.send(JSON.stringify({ id, method, params }))
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject, method }))
}

async function connect() {
  for (let i = 0; i < 50; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${port}/json`)).json()
      const page = list.find((t) => t.type === 'page')
      if (page) {
        ws = new WebSocket(page.webSocketDebuggerUrl)
        await new Promise((r, j) => { ws.onopen = r; ws.onerror = j })
        ws.onmessage = (m) => {
          const msg = JSON.parse(m.data)
          if (msg.method === 'Page.javascriptDialogOpening') {
            // A dialog stops the page's scripts, and every evaluation with them: answer it.
            trace('dialog', msg.params.type, msg.params.message)
            send('Page.handleJavaScriptDialog', { accept: true }).catch(() => {})
          }
          if (msg.method === 'Runtime.exceptionThrown') trace('exception', msg.params.exceptionDetails?.exception?.description?.slice(0, 200))
          const p = msg.id && pending.get(msg.id)
          if (!p) return
          trace('<-', msg.id, p.method)
          pending.delete(msg.id)
          msg.error ? p.reject(new Error(`${p.method}: ${msg.error.message}`)) : p.resolve(msg.result)
        }
        return
      }
    } catch { /* not up yet */ }
    await sleep(200)
  }
  throw new Error('Chrome did not start')
}

// navigate loads a URL without waiting on Chrome's reply to Page.navigate, which a page that keeps
// a connection open (DBCanvas streams events) can hold back; the page is waited for instead.
async function navigate(url, ms = 20000) {
  send('Page.navigate', { url }).catch(() => {})
  const end = Date.now() + ms
  await sleep(500)
  while (Date.now() < end) {
    try {
      const r = await Promise.race([
        send('Runtime.evaluate', { expression: 'document.readyState', returnByValue: true }),
        sleep(2000).then(() => null),
      ])
      if (r?.result?.value === 'complete') return
    } catch { /* the page is changing */ }
    await sleep(300)
  }
}

async function evaluate(expr, ms = 10000) {
  const r = await Promise.race([
    send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true }),
    sleep(ms).then(() => { throw new Error('the page did not answer within ' + ms + ' ms') }),
  ])
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || 'evaluation failed')
  return r.result.value
}

// find returns page JavaScript that yields the element a selector names: CSS, or text=… for the
// innermost visible element whose text contains the string.
function find(sel) {
  if (sel.startsWith('text=')) {
    const t = JSON.stringify(sel.slice(5))
    return `(() => { const all = [...document.querySelectorAll('body *')].filter((e) => e.getClientRects().length > 0 && e.textContent.includes(${t}));
      return all.find((e) => ![...e.children].some((c) => c.textContent.includes(${t}))) || null })()`
  }
  return `document.querySelector(${JSON.stringify(sel)})`
}

// mouse presses a real mouse button at the centre of the element a selector names, through
// Chrome's input events: a canvas that reacts to pointer presses ignores a script's .click().
async function mouse(sel, button = 'left') {
  const r = await evaluate(`(() => { const e = ${find(sel)}; if (!e) return null; e.scrollIntoView({ block: 'nearest' });
    const b = e.getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 } })()`)
  if (!r) throw new Error(`nothing to click for ${sel}`)
  await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: r.x, y: r.y })
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: r.x, y: r.y, button, clickCount: 1 })
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: r.x, y: r.y, button, clickCount: 1 })
}

async function waitFor(sel, ms = 20000) {
  const end = Date.now() + ms
  while (Date.now() < end) {
    if (await evaluate(`!!${find(sel)}`)) return
    await sleep(250)
  }
  throw new Error(`timed out waiting for ${sel}`)
}

async function run() {
  await connect()
  await send('Page.enable')
  await send('Runtime.enable')
  await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 1000, deviceScaleFactor: 2, mobile: false })
  await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'light' }] })
  await navigate(startLink)
  await sleep(2000)
  mkdirSync(outDir, { recursive: true })
  for (const s of shots) {
    const height = s.height || 1000
    await send('Emulation.setDeviceMetricsOverride', { width: 1600, height, deviceScaleFactor: 2, mobile: false })
    // Through a blank page, so each shot starts from a fresh load: a hash change alone would keep
    // the previous shot's open menus and panels.
    await navigate('about:blank', 3000)
    await navigate(origin + (s.path || '/'))
    await sleep(s.settle || 2500)
    try {
      for (const st of s.steps || []) {
        if (st.waitFor) await waitFor(st.waitFor, st.timeout)
        if (st.click) {
          await waitFor(st.click, st.timeout)
          await mouse(st.click)
        }
        if (st.type) {
          await waitFor(st.type.into)
          await evaluate(`(() => { const e = ${find(st.type.into)}; e.focus();
            const set = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(e), 'value').set;
            set.call(e, ${JSON.stringify(st.type.text)}); e.dispatchEvent(new Event('input', { bubbles: true })) })()`)
        }
        if (st.window) {
          // The desktop keeps every window it had open: close the others, then bring this one
          // forward and maximise it, so clicks cannot land on a window behind.
          const has = `[...document.querySelectorAll('[data-wm-window] span.truncate')].some((e) => e.textContent.trim() === ${JSON.stringify(st.window)})`
          let end = Date.now() + 8000
          while (Date.now() < end && !(await evaluate(has))) await sleep(300)
          if (!(await evaluate(has))) {
            // Not restored with the desktop: open it from its desktop icon, as a person would.
            await evaluate(`${find('text=' + st.window)}?.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }))`)
            end = Date.now() + 8000
            while (Date.now() < end && !(await evaluate(has))) await sleep(300)
          }
          const ok = await evaluate(`(() => { const t = ${JSON.stringify(st.window)};
            const title = (w) => (w.querySelector('span.truncate')?.textContent || '').trim();
            const ws = [...document.querySelectorAll('[data-wm-window]')];
            for (const w of ws) if (title(w) !== t) w.querySelector('button[title=Close]')?.click();
            const w = ws.find((x) => title(x) === t);
            if (!w) return false;
            w.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
            w.querySelector('button[title=Maximize]')?.click();
            return true })()`)
          if (!ok) throw new Error(`no window titled ${st.window}`)
          await sleep(600)
        }
        if (st.rightclick) {
          await waitFor(st.rightclick, st.timeout)
          await mouse(st.rightclick, 'right')
        }
        if (st.select) {
          await waitFor(st.select.in)
          await evaluate(`(() => { const e = ${find(st.select.in)};
            const want = ${JSON.stringify(st.select.label || '')};
            const v = want ? [...e.options].find((o) => o.textContent.includes(want))?.value : ${JSON.stringify(st.select.value)};
            Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value').set.call(e, v);
            e.dispatchEvent(new Event('change', { bubbles: true })) })()`)
        }
        if (st.text) await send('Input.insertText', { text: st.text })
        if (st.key) {
          const k = { Enter: { code: 'Enter', windowsVirtualKeyCode: 13, text: '\r' }, Escape: { code: 'Escape', windowsVirtualKeyCode: 27 },
            Tab: { code: 'Tab', windowsVirtualKeyCode: 9, text: '\t' } }[st.key] || {}
          await send('Input.dispatchKeyEvent', { type: 'keyDown', key: st.key, ...k })
          await send('Input.dispatchKeyEvent', { type: 'keyUp', key: st.key, ...k })
        }
        if (st.scroll) await evaluate(`${find(st.scroll)}?.scrollIntoView({ block: 'center' })`)
        if (st.eval) await evaluate(st.eval)
        if (st.wait) await sleep(st.wait)
      }
      // The mouse is moved out of the way first, so no hover tooltip from the last click lands
      // in the picture: an empty stretch of the top bar, unless the shot names its own spot.
      const [mx, my] = s.mouseAway || [1100, 50]
      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: mx, y: my })
      await sleep(s.after || 800)
      if (process.env.SHOTS_TRACE) console.error('page', await evaluate('location.href + " | " + document.title + " | " + document.body.innerText.slice(0, 300).replace(/\\s+/g, " ")'))
      const { data } = await send('Page.captureScreenshot', { format: 'png' })
      writeFileSync(join(outDir, s.file), Buffer.from(data, 'base64'))
      console.log(`ok    ${s.file}`)
    } catch (e) {
      console.log(`FAIL  ${s.file}: ${e.message}`)
      try {
        console.log('      page: ' + (await evaluate('location.href + " | " + document.body.innerText.slice(0, 240).replace(/\\s+/g, " ")', 5000)))
      } catch { /* the page is gone too */ }
      process.exitCode = 1
    }
  }
}

run().catch((e) => { console.error(e.message); process.exitCode = 1 }).finally(() => {
  try { ws?.close() } catch { /* closing anyway */ }
  proc.kill()
})
