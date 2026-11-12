import { useEffect, useState } from 'react'

// stackGone.js — the stacks this browser knows are being deleted or are gone.
//
// The stack list, the desktop's stack icons and their folder windows each poll on
// their own clock, so without this a deleted stack stayed on screen, and could be
// opened, until the next poll came round. A delete from here marks the stack
// 'deleting' while the server tears it down, then 'gone'; a read that answers 404
// (a delete from another tab, the CLI or the expiry reaper) marks it 'gone' too.
// Everything that shows or opens a stack checks here first.

const EVT = 'dbcanvas:stacks-gone'
const state = new Map() // stack id -> 'deleting' | 'gone'

function set(id, s) {
  const k = Number(id)
  if (!k) return
  if (s) state.set(k, s)
  else state.delete(k)
  dispatchEvent(new CustomEvent(EVT, { detail: { id: k, state: s } }))
}

export const stackGoneState = (id) => state.get(Number(id)) || ''
// unavailable is true for a stack that may no longer be opened.
export const stackUnavailable = (id) => state.has(Number(id))
export const markStackDeleting = (id) => set(id, 'deleting')
export const markStackGone = (id) => set(id, 'gone')
// A delete that failed: the stack is back.
export const clearStackGone = (id) => set(id, '')

// noteStackError marks a stack gone when a read of it answered 404.
export function noteStackError(id, err) {
  if (err?.status === 404) markStackGone(id)
}

// useStacksGone re-renders the caller whenever a stack's state here changes, and
// returns a counter that changes with it (for effect dependencies).
export function useStacksGone() {
  const [n, setN] = useState(0)
  useEffect(() => {
    const on = () => setN((x) => x + 1)
    addEventListener(EVT, on)
    return () => removeEventListener(EVT, on)
  }, [])
  return n
}
