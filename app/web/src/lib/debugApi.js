// Operator Debugger API. Same conventions as lib/stackApi.js: same-origin JSON,
// cookies ride along, throws Error with .status on non-2xx.
//
// The difference from every other tool's API wrapper is the socket. A debugger is
// not request/response — the operator stops when it stops, which may be minutes
// after you asked for anything — so the session runs over one WebSocket carrying
// two kinds of message: replies to commands we sent (matched by id) and state or
// log events that arrive on their own.

async function request(method, path, body) {
  const opts = {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) {
    try { data = JSON.parse(text) } catch { data = null }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

// targets: every K3D frame, across the user's stacks, whose operator is running
// under Delve right now.
export const debugApi = {
  targets: () => request('GET', '/api/k3d/debug/targets').then((r) => r?.targets || []),
}

export function debugFrameApi(stackId, frameId) {
  const base = `/api/stacks/${stackId}/frames/${frameId}/k3d/debug`
  return {
    sources: () => request('GET', `${base}/sources`),
    source: (path) => request('GET', `${base}/source?path=${encodeURIComponent(path)}`),
    reconcile: () => request('POST', `${base}/reconcile`),
  }
}

// openDebugSession opens the socket and returns a handle:
//
//   send(cmd)  fire and forget — the state broadcast is the answer
//   call(cmd)  returns a promise resolved with the reply's data (or rejected
//              with the server's message)
//   close()    ends the session for this browser; the server decides what that
//              means for the debugger (a reload keeps it, a stopped operator is
//              resumed)
//
// on.state / on.log / on.open / on.close are called as those arrive.
export function openDebugSession(stackId, frameId, on = {}) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const ws = new WebSocket(
    `${proto}://${location.host}/api/stacks/${stackId}/frames/${frameId}/k3d/debug/ws`)

  let nextId = 0
  const pending = new Map()

  ws.onopen = () => on.open?.()
  ws.onclose = () => {
    // Nothing will answer these now; failing them is better than a spinner that
    // never stops.
    pending.forEach(({ reject }) => reject(new Error('the debug session closed')))
    pending.clear()
    on.close?.()
  }
  ws.onmessage = (ev) => {
    let msg
    try { msg = JSON.parse(ev.data) } catch { return }
    if (msg.type === 'reply') {
      const waiter = pending.get(msg.id)
      if (!waiter) return
      pending.delete(msg.id)
      if (msg.ok) waiter.resolve(msg.data || {})
      else waiter.reject(new Error(msg.error || 'the debugger refused that'))
      return
    }
    if (msg.type === 'state') on.state?.(msg.state)
    else if (msg.type === 'log') on.log?.(msg.line)
  }

  const ready = () => ws.readyState === WebSocket.OPEN

  return {
    ws,
    send(cmd) {
      if (ready()) ws.send(JSON.stringify({ ...cmd, id: 0 }))
    },
    call(cmd) {
      return new Promise((resolve, reject) => {
        if (!ready()) return reject(new Error('the debug session is not open'))
        const id = ++nextId
        pending.set(id, { resolve, reject })
        ws.send(JSON.stringify({ ...cmd, id }))
      })
    },
    close() {
      try { ws.close() } catch { /* already gone */ }
    },
  }
}

// STATUS_TONE maps a session status onto the Badge tones the rest of the app uses.
export const STATUS_TONE = {
  detached: 'muted',
  attaching: 'accent',
  running: 'success',
  stopped: 'warning',
}

export const STATUS_TEXT = {
  detached: 'not attached',
  attaching: 'attaching…',
  running: 'running',
  stopped: 'stopped',
}

// goTokens splits a line of Go into [class, text] pairs for colouring. A real
// lexer is not needed and would not be worth a dependency: a debugger's source
// pane wants comments, strings and keywords to recede or stand out, and nothing
// else. Multi-line comments and raw strings are handled by carrying a state
// across lines (see goHighlight).
const GO_KEYWORDS = new Set([
  'break', 'case', 'chan', 'const', 'continue', 'default', 'defer', 'else',
  'fallthrough', 'for', 'func', 'go', 'goto', 'if', 'import', 'interface',
  'map', 'package', 'range', 'return', 'select', 'struct', 'switch', 'type', 'var',
])
const GO_LITERALS = new Set([
  'nil', 'true', 'false', 'iota', 'err', 'error', 'string', 'int', 'int32', 'int64',
  'bool', 'byte', 'rune', 'float64', 'uint', 'uint64', 'ctx', 'context',
])

// goHighlight turns source into an array of lines, each an array of tokens.
// State (inside a block comment, inside a raw string) is carried line to line,
// which is the only reason this is one function over the whole file.
export function goHighlight(src) {
  const out = []
  let inBlockComment = false
  let inRawString = false
  for (const line of String(src ?? '').split('\n')) {
    const tokens = []
    let i = 0
    let plain = ''
    const flush = () => { if (plain) { tokens.push(['', plain]); plain = '' } }
    while (i < line.length) {
      if (inBlockComment) {
        const end = line.indexOf('*/', i)
        if (end === -1) { tokens.push(['c', line.slice(i)]); i = line.length }
        else { tokens.push(['c', line.slice(i, end + 2)]); i = end + 2; inBlockComment = false }
        continue
      }
      if (inRawString) {
        const end = line.indexOf('`', i)
        if (end === -1) { tokens.push(['s', line.slice(i)]); i = line.length }
        else { tokens.push(['s', line.slice(i, end + 1)]); i = end + 1; inRawString = false }
        continue
      }
      const two = line.slice(i, i + 2)
      if (two === '//') { flush(); tokens.push(['c', line.slice(i)]); i = line.length; continue }
      if (two === '/*') { flush(); inBlockComment = true; continue }
      const ch = line[i]
      if (ch === '`') { flush(); inRawString = true; tokens.push(['s', '`']); i++; continue }
      if (ch === '"' || ch === "'") {
        flush()
        let j = i + 1
        while (j < line.length) {
          if (line[j] === '\\') { j += 2; continue }
          if (line[j] === ch) { j++; break }
          j++
        }
        tokens.push(['s', line.slice(i, j)])
        i = j
        continue
      }
      if (/[A-Za-z_]/.test(ch)) {
        let j = i
        while (j < line.length && /[A-Za-z0-9_]/.test(line[j])) j++
        const word = line.slice(i, j)
        if (GO_KEYWORDS.has(word)) { flush(); tokens.push(['k', word]) }
        else if (GO_LITERALS.has(word)) { flush(); tokens.push(['l', word]) }
        else plain += word
        i = j
        continue
      }
      if (/[0-9]/.test(ch)) {
        let j = i
        while (j < line.length && /[0-9a-fA-FxX._]/.test(line[j])) j++
        flush()
        tokens.push(['n', line.slice(i, j)])
        i = j
        continue
      }
      plain += ch
      i++
    }
    flush()
    out.push(tokens)
  }
  return out
}

// TOKEN_CLS maps a token class onto Tailwind colours that work in both themes.
export const TOKEN_CLS = {
  c: 'text-muted italic',
  s: 'text-status-ok',
  k: 'text-primary',
  l: 'text-accent',
  n: 'text-accent',
  '': '',
}

// shortFrameName trims a Go symbol to what fits in a stack list: the package and the
// function, without the module path nobody reads twice.
//
// The bracket is the part that has to go first. A generic frame comes back as
// `controller.(*Controller[go.shape.struct { k8s.io/apimachinery/pkg/types.NamespacedName }]).Reconcile`
// — the type argument contains slashes of its own, so trimming at the last slash before
// removing it leaves `types.NamespacedName }]).Reconcile`, which names nothing.
export function shortFrameName(name) {
  let s = String(name || '')
  // Drop [...] type arguments, innermost first, so nested ones go too.
  for (let guard = 0; guard < 8 && s.includes('['); guard++) {
    const next = s.replace(/\[[^[\]]*\]/g, '')
    if (next === s) break
    s = next
  }
  const slash = s.lastIndexOf('/')
  return slash === -1 ? s : s.slice(slash + 1)
}
