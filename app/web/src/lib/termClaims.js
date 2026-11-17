// Shared terminals that another part of the page draws itself, so the terminal panel does
// not open a second view of them. A Pager Panic window shows the terminals its game opened
// inside the game (browser/BrowserProvider.jsx); the panel shows every other one.
const claimers = new Set()

// claimTerms registers fn(term) → true for a shared terminal the caller shows. It
// returns the function that takes the claim back.
export function claimTerms(fn) {
  claimers.add(fn)
  return () => claimers.delete(fn)
}

export function termClaimed(term) {
  for (const fn of claimers) if (fn(term)) return true
  return false
}
