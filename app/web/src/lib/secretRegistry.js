// secretRegistry.js — the secret values this browser has on screen, for the mirror.
//
// "Mirror everything" (session/Mirror.jsx) sends the driver's page as it is drawn. With
// Hide secrets on, a value the app masks (components/Secret.jsx) must not reach a guest
// in some other spot where it is drawn in the clear: a connection string, a command to
// copy, a terminal line. So every value the Secret components are handed is noted here,
// and the recorder blanks each one wherever it appears in the page's text.

const values = new Set()

// Too short to be worth masking, and short strings would blank half the page.
const MIN = 4

export function noteSecret(v) {
  const s = v == null ? '' : String(v)
  if (s.length >= MIN) values.add(s)
}

// maskSecrets replaces every noted value in text with bullets of the same length.
export function maskSecrets(text) {
  if (!text || values.size === 0) return text
  let out = text
  for (const v of values) {
    if (out.includes(v)) out = out.split(v).join('•'.repeat(v.length))
  }
  return out
}
