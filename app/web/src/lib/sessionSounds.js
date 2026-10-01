// sessionSounds.js — the shared session's alert tones (session/SessionProvider.jsx).
//
// Synthesised with Web Audio rather than shipped as files: a handful of short sine
// chimes, quiet, each one distinct enough to tell apart without looking — a message,
// someone in the lobby, a request for control, control handed to you, the clock
// running out, the end.
//
// The preference is per browser (localStorage), on by default. A browser will not play
// sound before the person has interacted with the page; the context is created on the
// first tone and resumed on the next click or key if it started suspended.

const KEY = 'dbcanvas.session.sounds'

// Each tone is a list of [frequency Hz, start s, length s].
const TONES = {
  message: [[880, 0, 0.12], [1175, 0.09, 0.16]],
  join: [[660, 0, 0.12], [880, 0.1, 0.18]],
  lobby: [[523, 0, 0.14], [659, 0.12, 0.14], [784, 0.24, 0.22]],
  request: [[988, 0, 0.1], [988, 0.16, 0.1], [1319, 0.32, 0.2]],
  control: [[784, 0, 0.12], [1047, 0.1, 0.24]],
  warning: [[440, 0, 0.2], [440, 0.3, 0.2]],
  end: [[659, 0, 0.18], [523, 0.16, 0.18], [392, 0.32, 0.3]],
}

let ctx = null
let lastAt = 0
const listeners = new Set()

function read() {
  try { return localStorage.getItem(KEY) !== 'off' } catch { return true }
}

let enabled = read()

export function soundsEnabled() { return enabled }

export function setSoundsEnabled(on) {
  enabled = !!on
  try { localStorage.setItem(KEY, enabled ? 'on' : 'off') } catch { /* private window */ }
  listeners.forEach((fn) => fn())
  if (enabled) playSound('message')
}

// For useSyncExternalStore.
export function subscribeSounds(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

function audio() {
  if (typeof window === 'undefined') return null
  const AC = window.AudioContext || window.webkitAudioContext
  if (!AC) return null
  if (!ctx) {
    try { ctx = new AC() } catch { return null }
    const wake = () => { ctx?.resume?.().catch(() => {}) }
    window.addEventListener('pointerdown', wake, { capture: true })
    window.addEventListener('keydown', wake, { capture: true })
  }
  return ctx
}

export function playSound(kind) {
  if (!enabled) return
  const tone = TONES[kind]
  if (!tone) return
  // A burst of messages chimes once, not ten times.
  const now = Date.now()
  if (now - lastAt < 250) return
  lastAt = now
  const ac = audio()
  if (!ac) return
  if (ac.state === 'suspended') ac.resume().catch(() => {})
  const t0 = ac.currentTime + 0.01
  for (const [freq, start, len] of tone) {
    const osc = ac.createOscillator()
    const gain = ac.createGain()
    osc.type = 'sine'
    osc.frequency.value = freq
    const t = t0 + start
    gain.gain.setValueAtTime(0, t)
    gain.gain.linearRampToValueAtTime(0.12, t + 0.015)
    gain.gain.exponentialRampToValueAtTime(0.0001, t + len)
    osc.connect(gain).connect(ac.destination)
    osc.start(t)
    osc.stop(t + len + 0.02)
  }
}
