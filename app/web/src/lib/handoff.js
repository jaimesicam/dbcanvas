import { useEffect, useState } from 'react'
import { usePageVisible } from './usePolling.jsx'

// handoff.js — one page sending another page something to open.
//
// "Capture a pt-stalk archive, then chart it in Stalk Summary." "Analyse this
// cluster-dump." "Open the timeline in Log Summary." Each is a button on one page
// that switches to another and tells it what to show, and each does it through
// sessionStorage because the destination is a whole page away.
//
// Every one of them used to read that value in a mount-only effect, which was
// correct while App rendered a single page: navigating away unmounted the last
// page and arriving always mounted the next one. Under the tabbed shell it is
// wrong, and silently so — a tab that is already open is never unmounted, so
// arriving at it runs no effect and the handoff is left sitting in sessionStorage
// while the page shows whatever it showed before. Found exactly that way: a
// cluster-dump handed to Log Summary appeared in its bundle list, with its events
// counted, and the page did not open it.
//
// So the trigger is not mounting, it is *becoming visible* — which covers both.

export function sendHandoff(key, value) {
  try { sessionStorage.setItem(key, value) } catch { /* private mode: the click just navigates */ }
}

// nudgeHandoff tells pages that are already on screen to look again. On the desktop
// two pages are visible at once, so the page a handoff is for may never change
// visibility to notice it — the desktop's stack icons hand the Stack Designer a
// stack while its window is open.
export function nudgeHandoff() {
  dispatchEvent(new Event('dbcanvas:handoff'))
}

// useHandoff calls onReceive once with whatever was left under key, on mount and
// on every transition to visible. The value is consumed — a handoff is a message,
// not a setting, and re-firing it every time you glance at the tab would be worse
// than not firing it at all.
export function useHandoff(key, onReceive) {
  const visible = usePageVisible()
  const [nudge, setNudge] = useState(0)
  useEffect(() => {
    const on = () => setNudge((n) => n + 1)
    addEventListener('dbcanvas:handoff', on)
    return () => removeEventListener('dbcanvas:handoff', on)
  }, [])
  useEffect(() => {
    if (!visible) return
    let raw = null
    try { raw = sessionStorage.getItem(key) } catch { return }
    if (!raw) return
    try { sessionStorage.removeItem(key) } catch { /* ignore */ }
    onReceive(raw)
    // onReceive is deliberately not a dependency: pages pass an inline closure,
    // and re-running on every render would consume the value repeatedly.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, visible, nudge])
}
