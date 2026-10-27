// bus.js — "Send to Scratchboard", in memory.
//
// Other pages hand content to the board through this module-level queue rather than through
// sendHandoff (lib/handoff.js), which writes to sessionStorage: what goes onto the Scratchboard is
// never written to browser storage. If the board is not open yet the item waits here until it
// mounts and subscribes; a browser reload drops it, like everything else on the board.

const queue = []
const subs = new Set()

// sendToScratchboard queues { type, title, input, state } for the board and switches to it. type
// is optional (the board picks the card that fits the input); state is merged over the card's own
// starting state (e.g. the TLS Inspector's separate ca.crt).
export function sendToScratchboard(item) {
  queue.push({ ...item, at: Date.now() })
  for (const fn of subs) fn()
  if (typeof location !== 'undefined') location.hash = 'scratchboard'
}

// subscribeScratchboard delivers queued items to the board (and any that arrive later).
export function subscribeScratchboard(fn) {
  const drain = () => { while (queue.length) fn(queue.shift()) }
  subs.add(drain)
  drain()
  return () => subs.delete(drain)
}
