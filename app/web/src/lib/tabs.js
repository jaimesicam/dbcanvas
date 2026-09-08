// tabs.js — the rules behind the tabbed main window, kept out of App so they can
// be tested. A tab is { key, id }: the id says which page, the key is what makes
// two tabs of the *same* page possible — three benchmarks at once was the case
// that asked for this, and it needs identity to be the key rather than the name.

// How many pages may stay mounted at once. Every open tab is a live component
// tree — StackDesigner holds a whole canvas — so this is a memory budget rather
// than a matter of taste, which is why it is bounded rather than free: TABS_MAX
// is the most anyone may ask for, and the per-user setting (maxTabs, mirrored in
// app/settings.go) picks a number inside these.
export const TABS_DEFAULT = 20
export const TABS_MIN = 2
export const TABS_MAX = 40

// clampTabs turns whatever it is given — a missing field, a hand-edited row, the
// string in a number input somebody is halfway through typing — into a usable
// cap, matching normalize() in app/settings.go exactly so the browser and the
// server never disagree about what a stored value means.
//
// Out of range CLAMPS, because a number slightly too big still says what the user
// meant. Absent — blank, null, nothing, zero — takes the DEFAULT instead, because
// that is a client or an account that never had an opinion, not somebody asking
// for as few tabs as possible.
export function clampTabs(n) {
  const v = Math.round(Number(n))
  if (!Number.isFinite(v) || v <= 0) return TABS_DEFAULT
  return Math.min(TABS_MAX, Math.max(TABS_MIN, v))
}

// openTab applies the focus-or-open rule and returns the next {tabs, activeKey}.
// Pure: it allocates no keys of its own, so the caller owns the sequence and
// React may call it as often as it likes.
//
// At the cap it refuses rather than evicting. A tab can hold a running benchmark
// or a half-built canvas, and closing one to make room is not a decision to make
// on someone's behalf — so a capped open falls back to focusing whatever of that
// page is already there, and otherwise changes nothing.
//
// A refusal sets `capped`, and the caller is expected to SAY so. Refusing in
// silence was the earlier behaviour and it reads as a broken sidebar: you click a
// page, nothing happens, and nothing on screen explains why. Note that reaching
// this branch always means the click did not get what it asked for — the
// already-open case returned above — so `capped` needs no further condition, even
// though the focus consolation still applies to a forced open.
export function openTab(tabs, activeKey, id, key, force = false, max = TABS_DEFAULT) {
  const found = tabs.find((t) => t.id === id)
  if (!force && found) return { tabs, activeKey: found.key }
  if (tabs.length >= max) return { tabs, activeKey: found ? found.key : activeKey, capped: true }
  return { tabs: [...tabs, { key, id }], activeKey: key }
}

// closeTab removes a tab and moves focus to the one that took its place, which is
// where the eye already is. The last tab never closes: an empty main window is not
// a state this app has anything to say in.
export function closeTab(tabs, activeKey, key) {
  if (tabs.length <= 1) return { tabs, activeKey }
  const i = tabs.findIndex((t) => t.key === key)
  if (i < 0) return { tabs, activeKey }
  const next = tabs.filter((t) => t.key !== key)
  return {
    tabs: next,
    activeKey: activeKey === key ? next[Math.min(i, next.length - 1)].key : activeKey,
  }
}

// tabCounts is how many tabs each page has open, for the badge on the nav item.
export function tabCounts(tabs) {
  const out = {}
  for (const t of tabs) out[t.id] = (out[t.id] || 0) + 1
  return out
}
