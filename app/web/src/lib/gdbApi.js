// Core Dump Analyzer API. Same conventions as lib/debugApi.js, and for the same
// reason: reading a core is a conversation with a process that outlives any one
// request, so the session runs over a WebSocket carrying replies (matched by id)
// alongside state and log events that arrive on their own.
//
// The one shape this adds over the debugger's is the *verdict*. gdb prints a
// backtrace whether or not the libraries are the ones the process was running
// and whether or not the binary is the build that crashed; `cores()` is what asks
// that question, before any stack is on screen to be believed.

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

// targets: every Linux Client, across the user's stacks, deployed for core-dump
// analysis and running right now.
export const gdbApi = {
  targets: () => request('GET', '/api/gdb/targets').then((r) => r?.targets || []),
}

export function gdbNodeApi(stackId, nodeId) {
  const base = `/api/stacks/${stackId}/nodes/${nodeId}/gdb`
  return {
    cores: () => request('GET', `${base}/cores`),
  }
}

// openGDBSession opens the socket and returns the same handle shape the debugger
// uses — send(cmd) for fire-and-forget, call(cmd) for a promise, close() to leave.
export function openGDBSession(stackId, nodeId, on = {}) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const ws = new WebSocket(`${proto}://${location.host}/api/stacks/${stackId}/nodes/${nodeId}/gdb/ws`)

  let nextId = 0
  const pending = new Map()

  ws.onopen = () => on.open?.()
  ws.onclose = () => {
    pending.forEach(({ reject }) => reject(new Error('the gdb session closed')))
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
      else waiter.reject(new Error(msg.error || 'gdb refused that'))
      return
    }
    if (msg.type === 'state') on.state?.(msg.state)
    else if (msg.type === 'log') on.log?.(msg.line)
  }

  const ready = () => ws.readyState === WebSocket.OPEN

  return {
    ws,
    send(cmd) { if (ready()) ws.send(JSON.stringify({ ...cmd, id: 0 })) },
    call(cmd) {
      return new Promise((resolve, reject) => {
        if (!ready()) return reject(new Error('the gdb session is not open'))
        const id = ++nextId
        pending.set(id, { resolve, reject })
        ws.send(JSON.stringify({ ...cmd, id }))
      })
    },
    close() { try { ws.close() } catch { /* already gone */ } },
  }
}

export const GDB_STATUS_TONE = {
  idle: 'muted',
  loading: 'accent',
  ready: 'success',
  error: 'danger',
}

export const GDB_STATUS_TEXT = {
  idle: 'no core open',
  loading: 'reading the core…',
  ready: 'ready',
  error: 'failed',
}

// gdbTargetKey / gdbShortName mirror the debugger's helpers.
export const gdbTargetKey = (t) => `${t.stackId}/${t.nodeId}`

// shortFunc drops a C++ signature's arguments. An InnoDB frame is routinely 120
// characters of template parameters wrapping a name of twelve, and the name is
// the part a stack is read for; the full signature stays in the row's title.
export function shortFunc(fn) {
  if (!fn) return '??'
  const i = fn.indexOf('(')
  return i > 0 ? fn.slice(0, i) : fn
}

// libraryOf reduces "/lib64/libc.so.6" to "libc.so.6" for the frame rows.
export function libraryOf(from) {
  if (!from) return ''
  const i = from.lastIndexOf('/')
  return i >= 0 ? from.slice(i + 1) : from
}

// sourceOf renders a frame's source location, or the object it came from when
// there is none — which for a released server binary is most frames.
export function sourceOf(frame) {
  if (!frame) return ''
  if (frame.file) {
    const base = frame.file.slice(frame.file.lastIndexOf('/') + 1)
    return frame.line ? `${base}:${frame.line}` : base
  }
  return frame.from ? libraryOf(frame.from) : ''
}

// SYSTEM_OBJECTS are the C runtime's, mirroring gdbSystemObject in gdbapi.go. A
// frame in one of these is almost never the bug: a stack overflow surfaces inside
// _int_malloc, an assertion inside abort. The page dims them so the program's own
// frames are what the eye lands on.
//
// Matched on the *stem*, because both spellings turn up and which one you get
// depends on where the library came from: a distribution's /lib64 holds the soname
// (libc.so.6), while a flat copy taken off the crashed host holds the real file
// (libc-2.28.so). Matching only the first is how the frame that surfaces a stack
// overflow gets reported as the program's own code.
const SYSTEM_OBJECTS = new Set([
  'libc', 'libpthread', 'ld', 'libm', 'libgcc_s', 'libstdc++', 'librt', 'libdl',
])

export function isSystemFrame(frame) {
  const lib = libraryOf(frame?.from)
  if (!lib) return false
  const cut = lib.search(/[-.]/)
  return SYSTEM_OBJECTS.has(cut > 0 ? lib.slice(0, cut) : lib)
}

// crashSummary is the *fallback* heuristic, kept for the case where the server's analysis could
// not run: pick the first frame that is not the C library and note any repetition.
//
// It is deliberately not what the page shows when a verdict is available, and reading the real core
// is what settled that. This heuristic answers "ut_allocator::allocate" for a stack overflow —
// which is the allocation that happened to touch the guard page, not the bug — and it can only
// count repeats inside the window it was given, which understated a 1,060-frame recursion as 39.
// Both of those need the whole stack, so both belong on the server. See gdbdiag.go.
export function crashSummary(frames = []) {
  const culprit = frames.find((f) => f.func && f.func !== '??' && !isSystemFrame(f)) || frames[0] || null
  const repeated = frames.find((f) => (f.repeat || 0) > 2) || null
  return {
    culprit,
    recursion: repeated ? `${shortFunc(repeated.func)} repeats ${repeated.repeat} times` : '',
  }
}

// formatBytes is for core-file sizes, which are the size of a server's memory and
// therefore always want a unit.
export function formatBytes(n) {
  if (!n && n !== 0) return ''
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}
