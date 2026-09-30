import { createContext, useContext, useEffect, useRef } from 'react'

// useRefresh.jsx — the Refresh button in the top bar, and how a page gets one.
//
// Tabs stay mounted, which is what keeps a page's filters and scroll position —
// and also what keeps its data from ever being re-read. A page loads its lists on
// mount, and under tabs "mount" happens once: a stack deployed from another tab,
// a capture finished on the server, a token made elsewhere, none of it shows until
// the page is closed and reopened or the browser is reloaded, and both of those
// throw away exactly the state tabs exist to keep.
//
// So a page says what "read it again" means for it — useRefresh(fn) — and the
// shell draws one button for whichever tab is on screen. The page keeps its own
// state; only what came from the server is fetched again. A page that registers
// nothing gets no button, which is the honest answer for one with nothing to
// re-read.
//
// Registration is per tab: two Benchmark tabs are two providers, and the button
// refreshes the one you are looking at.

const RefreshCtx = createContext(null)

// RefreshProvider is what the shell wraps each tab in. register(fn) adds fn to
// this tab's set and returns the function that takes it out again.
export function RefreshProvider({ register, children }) {
  return <RefreshCtx.Provider value={register}>{children}</RefreshCtx.Provider>
}

// useRefresh registers cb as (part of) this page's refresh. cb may return a
// promise; the button spins until every registered one settles. cb is held in a
// ref, so a page may pass a fresh closure every render. enabled=false withdraws it
// — for a page whose refresh only means something once, say, a file is open.
//
// Outside any provider (the smoke renders, a page embedded somewhere else) it does
// nothing, so a component may call it unconditionally.
export function useRefresh(cb, enabled = true) {
  const register = useContext(RefreshCtx)
  const cbRef = useRef(cb)
  cbRef.current = cb
  useEffect(() => {
    if (!register || !enabled) return undefined
    return register(() => cbRef.current())
  }, [register, enabled])
}

// runAll calls every registered refresher and waits for all of them, failures
// included: one list that could not be re-read is no reason to stop the spinner
// on the others, and each page already reports its own load errors.
export function runAll(fns) {
  return Promise.allSettled([...fns].map((fn) => {
    try { return Promise.resolve(fn()) } catch (e) { return Promise.reject(e) }
  }))
}
