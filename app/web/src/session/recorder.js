import { shareApi } from '../lib/shareApi.js'

// recorder.js — the host recording a shared session (app/sharerecord.go).
//
// The browser records its own tab: getDisplayMedia, which asks the host to pick what
// to capture (Chrome offers this very tab first), then MediaRecorder. The tab is the
// whole session as the host sees it — the workspace or the driver's mirrored screen,
// the drawings over it (session/DrawLayer.jsx), and the session panel with the chat —
// so that one picture is the recording.
//
// MediaRecorder hands over a chunk every few seconds, and each goes straight to the
// server in order, numbered, retried until it is taken: a recording is never only in
// this tab's memory for long, so a crash loses seconds, not the session. Stopping
// waits for the last chunk, then tells the server to finish the file.
//
// One module-level recorder, not a component: it must outlive the session panel,
// which the host can hide, and the page it was started from.

const CHUNK_MS = 4000
const BITRATE = 2_500_000
const TYPES = ['video/webm;codecs=vp9', 'video/webm;codecs=vp8', 'video/webm', 'video/mp4']

let state = { status: 'idle', sid: null, rid: null, startedAt: 0, error: '', pending: 0, uploaded: 0 }
const subs = new Set()
const set = (patch) => {
  state = { ...state, ...patch }
  for (const fn of subs) fn()
}

export const recorder = {
  subscribe: (fn) => { subs.add(fn); return () => subs.delete(fn) },
  get: () => state,
  // supported: this browser can capture a tab and record it.
  supported: () => typeof navigator !== 'undefined' && !!navigator.mediaDevices?.getDisplayMedia &&
    typeof MediaRecorder !== 'undefined' && TYPES.some((t) => MediaRecorder.isTypeSupported(t)),
  start,
  stop,
}

let mr = null
let stream = null
let queue = []
let seq = 0
let draining = null

async function start(sid) {
  if (state.status !== 'idle') return
  set({ status: 'starting', sid, error: '', uploaded: 0, pending: 0 })
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({
      // At the tab's own resolution: the default is far smaller, and a recording of
      // a page is read, so small text has to survive it.
      video: {
        displaySurface: 'browser', frameRate: { ideal: 15, max: 30 },
        width: { ideal: Math.min(3840, Math.round(innerWidth * devicePixelRatio)) },
        height: { ideal: Math.min(2160, Math.round(innerHeight * devicePixelRatio)) },
      },
      audio: false,
      // Chrome: offer this tab, and only tabs — the session is in this one.
      preferCurrentTab: true,
      selfBrowserSurface: 'include',
      surfaceSwitching: 'exclude',
      monitorTypeSurfaces: 'exclude',
    })
  } catch (e) {
    stream = null
    set({ status: 'idle', error: e?.name === 'NotAllowedError' ? '' : `Could not capture the screen: ${e.message}` })
    return
  }
  const mime = TYPES.find((t) => MediaRecorder.isTypeSupported(t))
  let rec
  try {
    rec = await shareApi.recordStart(sid, mime)
  } catch (e) {
    stream.getTracks().forEach((t) => t.stop())
    stream = null
    set({ status: 'idle', error: e.message })
    return
  }
  queue = []
  seq = 0
  mr = new MediaRecorder(stream, { mimeType: mime, videoBitsPerSecond: BITRATE })
  mr.ondataavailable = (e) => {
    if (!e.data?.size) return
    queue.push({ seq: seq++, blob: e.data })
    set({ pending: queue.length })
    drain()
  }
  mr.onstop = async () => {
    stream?.getTracks().forEach((t) => t.stop())
    stream = null
    // ondataavailable fires once more before onstop; wait for that last chunk.
    await drain()
    try { await shareApi.recordFinish(rec.id, Date.now() - state.startedAt) } catch { /* the server finishes it on its own */ }
    mr = null
    removeEventListener('beforeunload', warn)
    set({ status: 'idle', rid: null, sid: null, pending: 0 })
  }
  // The browser's own "Stop sharing" button ends the capture: stop with it.
  stream.getVideoTracks()[0]?.addEventListener('ended', stop)
  addEventListener('beforeunload', warn)
  mr.start(CHUNK_MS)
  set({ status: 'recording', rid: rec.id, startedAt: Date.now() })
}

function stop() {
  if (!mr || mr.state === 'inactive') return
  set({ status: 'stopping' })
  mr.stop()
}

function warn(e) {
  e.preventDefault()
  e.returnValue = ''
}

// drain uploads the queue in order, one chunk at a time; it resolves when the queue
// is empty. A chunk that fails is retried with a growing pause rather than skipped:
// a gap in the middle of a WebM breaks everything after it.
function drain() {
  if (draining) return draining
  draining = (async () => {
    let wait = 1000
    while (queue.length) {
      const c = queue[0]
      try {
        const res = await fetch(shareApi.recordChunkURL(state.rid, c.seq), {
          method: 'POST', body: c.blob, credentials: 'same-origin',
          headers: { 'Content-Type': 'application/octet-stream' },
        })
        const body = await res.json().catch(() => ({}))
        if (res.ok || (res.status === 409 && body.next > c.seq)) {
          queue.shift()
          wait = 1000
          set({ pending: queue.length, uploaded: state.uploaded + c.blob.size })
          continue
        }
        if (res.status === 413 || res.status === 409 || res.status === 404) {
          // The server stopped this recording (its size limit, or it was purged), or
          // lost its place in it: nothing more can be appended.
          queue = []
          set({ error: body.error || 'The server stopped the recording.' })
          if (mr && mr.state !== 'inactive') { set({ status: 'stopping' }); mr.stop() }
          break
        }
        throw new Error(body.error || `upload failed (${res.status})`)
      } catch {
        await new Promise((r) => setTimeout(r, wait))
        wait = Math.min(wait * 2, 15000)
      }
    }
  })().finally(() => { draining = null })
  return draining
}
