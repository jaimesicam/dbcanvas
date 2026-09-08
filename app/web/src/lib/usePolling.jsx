import { createContext, useContext, useEffect, useRef } from 'react'

// usePolling.jsx — the one place a page decides when it is worth asking the server
// something again.
//
// Every long-lived page here polls: the Dashboard's live OS stats, a benchmark's
// progress, a query run's processlist, a capture's packet count. Mounted-and-
// forgotten those loops are free, because only one page is ever mounted — App
// renders a single <PageComponent /> and switching nav unmounts the last one.
//
// That stops being true the moment several pages can be open at once. Keeping a
// page alive so it does not lose its scroll position and its filters also keeps
// its timer alive, and five open pages become five loops asking a server that has
// nothing new to say. So visibility has to be something a page can be *told*,
// not only something it can observe about the browser tab.
//
// Two gates, and a page has to pass both:
//
//   the browser tab   document.visibilityState === 'visible' && document.hasFocus()
//   the app's own tab  PageVisibleProvider, which the shell sets per page
//
// The browser half is lifted from Dashboard's useFocusGatedInterval, which had it
// right and had it alone.

// PageVisibleCtx is what the shell uses to say "this page is on screen". Defaults
// to true so a page rendered outside any provider — which is every page today —
// behaves exactly as it does now.
const PageVisibleCtx = createContext(true)

export function PageVisibleProvider({ visible, children }) {
  return <PageVisibleCtx.Provider value={visible}>{children}</PageVisibleCtx.Provider>
}

export function usePageVisible() {
  return useContext(PageVisibleCtx)
}

// browserActive is the tab-level half of the question.
function browserActive() {
  if (typeof document === 'undefined') return true
  return document.visibilityState === 'visible' && document.hasFocus()
}

// usePolling runs cb now and every ms, but only while both gates are open. It
// stops on blur, on hide, when the shell says this page is not on screen, and on
// unmount — and runs cb once immediately when the gates reopen, so coming back to
// a page shows current data rather than whatever was true when you left.
//
// cb is held in a ref, so a page may pass a fresh closure every render without
// restarting the timer. ms of 0 or less disables polling entirely, which is how a
// page says "not while this run is finished".
export function usePolling(cb, ms, opts = {}) {
  const { onActive, enabled = true } = opts
  const cbRef = useRef(cb)
  cbRef.current = cb
  const shellVisible = usePageVisible()

  useEffect(() => {
    if (!enabled || !(ms > 0) || !shellVisible) {
      if (onActive) onActive(false)
      return undefined
    }
    let timer = null
    const tick = () => {
      if (browserActive()) cbRef.current()
    }
    const start = () => {
      if (!timer) {
        tick()
        timer = setInterval(tick, ms)
      }
    }
    const stop = () => {
      if (timer) {
        clearInterval(timer)
        timer = null
      }
    }
    const sync = () => {
      const on = browserActive()
      if (onActive) onActive(on)
      on ? start() : stop()
    }
    sync()
    document.addEventListener('visibilitychange', sync)
    window.addEventListener('focus', sync)
    window.addEventListener('blur', sync)
    return () => {
      stop()
      if (onActive) onActive(false)
      document.removeEventListener('visibilitychange', sync)
      window.removeEventListener('focus', sync)
      window.removeEventListener('blur', sync)
    }
    // onActive is intentionally not a dependency: pages pass an inline setter and
    // re-running this on every render would restart the timer forever.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ms, enabled, shellVisible])
}
