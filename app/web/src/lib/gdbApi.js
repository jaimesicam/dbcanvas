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

// COMMAND_IDLE are the processlist commands that are a connection sitting idle rather than running
// something. They are dimmed in the session table so the handful that were actually executing at
// the crash stand out of a list that is mostly parked workers.
const COMMAND_IDLE = new Set(['Sleep', 'Daemon', 'Binlog Dump', 'Binlog Dump GTID', ''])
export const isIdleSession = (s) => COMMAND_IDLE.has(s?.command || '') && !s?.query

// PRIO_* map the error-log priority (the integer perfschema stores) to a label and a tone. 0 is
// MySQL's "System", and the levels are not a severity order you can compare numerically — see the
// mysql-log-facts memory — so this is a lookup, not a threshold.
export const LOG_PRIO = {
  0: { label: 'System', tone: 'muted' },
  1: { label: 'Error', tone: 'danger' },
  2: { label: 'Warning', tone: 'warning' },
  3: { label: 'Note', tone: 'muted' },
}

// wsrepTone flags a PXC member whose own view of the cluster is the problem: a node that crashed
// while already out of the Primary component is a different story from one that crashed inside a
// healthy cluster, and the status line should look different too.
export function wsrepTone(w) {
  if (!w) return 'muted'
  const s = (w.clusterStatus || '').toLowerCase()
  if (w.ready === false || (s && s !== 'primary')) return 'danger'
  return 'success'
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

// formatStarted renders when the server started, from its start_time (unix seconds) read out of the
// core. Not an uptime: that needs the moment of the crash, and nothing in the core records it
// reliably — the newest error-log line can be minutes older (a quiet server logs nothing between
// "ready for connections" and the crash), and the core file's mtime is reset by a plain copy.
export function formatStarted(startSec) {
  if (!startSec) return ''
  return new Date(startSec * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })
}

// lsnGap is the distance between the flushed LSN and the last checkpoint: the redo the server would
// have had to replay on restart. A large gap on a crashed node is a long crash-recovery ahead.
export function lsnGap(innodb) {
  if (!innodb || innodb.flushedLsn == null || innodb.checkpointLsn == null) return null
  return innodb.flushedLsn - innodb.checkpointLsn
}

// ---------------------------------------------------------------- values

// canExpand is what a value looks like when gdb has something inside it to
// show: a struct (`{…}`), or a pointer, which gdb's variable objects open into
// the thing pointed at rather than into the pointer itself.
//
// It is a guess, deliberately. -stack-list-variables prints a value but does not
// say whether it has children, and asking gdb per variable would be one round
// trip per row of the panel. Guessing wrong in the permissive direction costs an
// expand that comes back "no members"; guessing wrong in the other direction
// hides the THD behind a pointer nobody can click.
export function canExpand(v) {
  const value = (v?.value || '').trim()
  if (!value || value === '<optimized out>') return false
  if (value.startsWith('{') || value.startsWith('@0x')) return true
  if (/^0x0*$/.test(value)) return false // a null pointer has nothing behind it
  return /^0x[0-9a-f]+$/i.test(value) || /^\(.*\)\s*0x[0-9a-f]+/i.test(value)
}

// valueSummary pulls the readable part out of a printed value.
//
// With gdb's Python pretty-printers off — they are, because auto-load is how a
// mounted directory becomes code execution — a std::string prints as forty
// characters of allocator boilerplate wrapped around the one thing anybody wants:
// the text. The same is true of a char* and of most of the server's own string
// types, and all of them put it in the same place, in quotes.
export function valueSummary(value) {
  if (!value) return ''
  const m = /"((?:[^"\\]|\\.){0,200})"/.exec(value)
  if (!m) return ''
  const text = m[1]
  // A one-character match inside a repeat marker ('\000' <repeats 23 times>) is
  // padding, not content.
  return text.length > 1 || /^[ -~]$/.test(text) ? text : ''
}

// stackSignature is what makes sixty threads readable: two threads parked in the
// same place have the same list of function names, and a core has a lot of those
// — every idle worker in the pool is one stack, printed twenty times, and
// `thread apply all bt` prints it twenty times.
export function stackSignature(stack) {
  return (stack?.frames || []).map((f) => shortFunc(f.func) || f.addr).join(' < ')
}

// groupThreadStacks folds the all-threads view into one row per distinct stack.
// Order is the one a reader wants: the thread that took the signal first, then
// the biggest groups, which is where a deadlock or a pile-up shows itself.
export function groupThreadStacks(stacks = []) {
  const groups = new Map()
  for (const st of stacks) {
    const sig = stackSignature(st)
    const g = groups.get(sig)
    if (g) { g.threads.push(st); g.signal = g.signal || st.signal }
    else groups.set(sig, { sig, threads: [st], signal: !!st.signal, stack: st })
  }
  return [...groups.values()].sort((a, b) =>
    (b.signal ? 1 : 0) - (a.signal ? 1 : 0) || b.threads.length - a.threads.length)
}

// threadStacksAsText renders the all-threads view the way gdb would have, for
// pasting into a ticket — the one thing the wall of text was actually good for.
export function threadStacksAsText(stacks = []) {
  return stacks.map((st) => {
    const head = `Thread ${st.thread}${st.name ? ` "${st.name}"` : ''} ${st.target || ''}`
      + (st.signal ? '  <- took the signal' : '')
      + (st.depth ? `  (${st.depth} frames)` : '')
    const body = (st.frames || []).map((f) => {
      const rep = f.repeat > 1 ? `  [x${f.repeat}]` : ''
      return `#${f.level}  ${f.func || '??'}${sourceOf(f) ? ` at ${sourceOf(f)}` : ''}${rep}`
    })
    if (st.more) body.push('  … deeper frames not loaded')
    if (st.error) body.push(`  ${st.error}`)
    return [head, ...body].join('\n')
  }).join('\n\n')
}

// ---------------------------------------------------------------- sessions

// accountOf renders the "who" of a session from its security context: user@host, with the peer IP
// in brackets when it differs from the host (a networked client), so the common root@localhost
// stays short and app@db-01 [10.0.0.5] carries the address it is worth carrying.
export function accountOf(s) {
  if (!s) return ''
  const user = s.user || ''
  const host = s.host || s.ip || ''
  const base = user ? (host ? `${user}@${host}` : user) : host
  if (!base) return ''
  const ip = s.ip && s.ip !== s.host ? s.ip : ''
  return ip ? `${base} [${ip}]` : base
}

// crashTimeRef is the best estimate of *when* the core was taken, so a session's age can be shown
// as "how long it had been running". Nothing in a core records the crash instant reliably (see
// formatStarted), so this takes the latest evidence of life: the newest error-log line, or the most
// recent command start, whichever is later. Returned in unix seconds, or null if there is nothing.
export function crashTimeRef(data) {
  let ref = 0
  for (const e of data?.errorLog || []) if (e.ts) ref = Math.max(ref, Math.floor(e.ts / 1e6))
  for (const s of data?.sessions || []) if (s.startTime) ref = Math.max(ref, s.startTime)
  return ref || null
}

// formatAge turns a span in seconds into a compact "2h 5m" / "3m 12s" / "45s". Used for how long a
// session had been on its current command at the crash.
export function formatAge(sec) {
  if (sec == null || sec < 0) return ''
  if (sec < 60) return `${sec}s`
  const m = Math.floor(sec / 60)
  if (m < 60) return sec % 60 ? `${m}m ${sec % 60}s` : `${m}m`
  const h = Math.floor(m / 60)
  if (h < 24) return m % 60 ? `${h}h ${m % 60}m` : `${h}h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d}d ${h % 24}h` : `${d}d`
}

// serverVerdict reads the one-line shape of the server at the crash from its own state — the
// Server-state tab's answer to the Stack tab's crash verdict. It is deliberately about the server,
// not the signal: a core is often a pile-up or a partitioned node, and that story lives in the
// transactions, the cluster view and the sessions, not in the faulting frame. Returns the single
// most important finding plus a few supporting chips, each with a tone that drives its colour.
export function serverVerdict(data, state) {
  if (!data) return null
  const sessions = data.sessions || []
  const running = sessions.filter((s) => !isIdleSession(s))
  const trx = data.innodb?.transactions || []
  const waiters = trx.filter((t) => t.waiting && t.blockedByTrx != null)
  const errored = sessions.filter((s) => s.lastErrno)
  const killed = sessions.filter((s) => s.killed)
  const w = data.wsrep

  // Supporting chips, always shown, lightest to heaviest handled by the caller's ordering.
  const chips = []
  chips.push({ label: `${running.length} running`, sub: `${sessions.length} connected`, tone: running.length ? 'accent' : 'muted' })
  if (errored.length) chips.push({ label: `${errored.length} in error`, tone: 'warning' })
  if (killed.length) chips.push({ label: `${killed.length} killed`, tone: 'danger' })
  if (data.innodb?.historyListLength > 100000) chips.push({ label: 'history list high', sub: data.innodb.historyListLength.toLocaleString(), tone: 'warning' })

  // The headline is the heaviest condition that holds.
  let tone = 'accent'
  let headline = ''
  let why = ''
  if (w && wsrepTone(w) === 'danger') {
    tone = 'danger'
    headline = `This PXC node was ${w.ready === false ? 'not ready' : `in a ${w.clusterStatus || 'non-Primary'} component`} when it crashed`
    why = 'It had already left the Primary component — it was cut off from the cluster before the process fell over, so look at the partition first and the crash second.'
  } else if (waiters.length) {
    tone = 'warning'
    const blockers = [...new Set(waiters.map((x) => x.blockedByTrx))]
    headline = `${waiters.length} transaction${waiters.length === 1 ? '' : 's'} blocked — a lock pile-up`
    why = blockers.length === 1
      ? `Everything was queued behind transaction ${blockers[0]}. Start there.`
      : `Queued behind transactions ${blockers.join(', ')}.`
  } else if (state?.verdict?.fault || /^SIG/.test(state?.signal || '')) {
    // The stack tab found a real fault — a kernel siginfo address or a signal name — so the crash,
    // not a stall, is the story; echo its headline and point at it. A gcore snapshot has neither and
    // falls through to the server-centric summary below (verdict.headline/culprit get set either way).
    headline = state.verdict?.headline || `${state.signal}${state.signalText ? ` — ${state.signalText}` : ''}`
    why = 'No lock pile-up and the cluster view is clean — this reads as a genuine crash, not a stall. The stack tab has the fault.'
  } else if (running.length) {
    headline = `${running.length} session${running.length === 1 ? ' was' : 's were'} running when the core was taken`
    why = 'No lock pile-up, a clean cluster view, and no recorded crash signal — a snapshot of a healthy server.'
  } else {
    tone = 'muted'
    headline = 'No sessions were running when the core was taken'
    why = 'Every connection was idle.'
  }
  return { tone, headline, why, chips, blocker: waiters.length ? [...new Set(waiters.map((x) => x.blockedByTrx))][0] : null }
}
