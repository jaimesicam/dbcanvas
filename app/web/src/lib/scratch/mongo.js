// mongo.js — the MongoDB value decoder: ObjectId, and the Extended JSON wrappers that hide a value
// behind a type ($date, $numberLong, $timestamp, $numberDecimal, $binary, $uuid).

// objectId reads a 24-hex ObjectId: its creation time (the first 4 bytes, seconds since the epoch),
// the 5 random bytes (per process) and the 3-byte counter.
export function objectId(input) {
  const m = /(?:ObjectId\(\s*["']?)?([0-9a-f]{24})/i.exec(String(input ?? ''))
  if (!m) return null
  const h = m[1].toLowerCase()
  const secs = parseInt(h.slice(0, 8), 16)
  return {
    hex: h,
    created: new Date(secs * 1000),
    seconds: secs,
    random: h.slice(8, 18),
    counter: parseInt(h.slice(18), 16),
  }
}

// extendedJSON finds each typed value in a pasted snippet and says what it is.
export function extendedJSON(input) {
  const s = String(input ?? '')
  const out = []
  const push = (type, raw, meaning) => out.push({ type, raw, meaning })
  for (const m of s.matchAll(/\{\s*"\$date"\s*:\s*(?:"([^"]+)"|\{\s*"\$numberLong"\s*:\s*"(-?\d+)"\s*\})\s*\}/g)) {
    const d = m[1] ? new Date(m[1]) : new Date(Number(m[2]))
    push('$date', m[0], isNaN(d) ? 'an unreadable date' : `${d.toISOString()} (epoch ms ${d.getTime()})`)
  }
  for (const m of s.matchAll(/\{\s*"\$timestamp"\s*:\s*\{\s*"t"\s*:\s*(\d+)\s*,\s*"i"\s*:\s*(\d+)\s*\}\s*\}/g)) {
    push('$timestamp', m[0], `${new Date(Number(m[1]) * 1000).toISOString()}, operation #${m[2]} in that second — an oplog / cluster time`)
  }
  for (const m of s.matchAll(/\{\s*"\$numberLong"\s*:\s*"(-?\d+)"\s*\}/g)) {
    if (!/"\$date"\s*:\s*$/.test(s.slice(0, m.index))) push('$numberLong', m[0], `the 64-bit integer ${m[1]}`)
  }
  for (const m of s.matchAll(/\{\s*"\$numberDecimal"\s*:\s*"([^"]+)"\s*\}/g)) push('$numberDecimal', m[0], `the 128-bit decimal ${m[1]}`)
  for (const m of s.matchAll(/\{\s*"\$numberInt"\s*:\s*"(-?\d+)"\s*\}/g)) push('$numberInt', m[0], `the 32-bit integer ${m[1]}`)
  for (const m of s.matchAll(/\{\s*"\$uuid"\s*:\s*"([0-9a-f-]{36})"\s*\}/gi)) push('$uuid', m[0], `UUID ${m[1]} (binary subtype 4)`)
  for (const m of s.matchAll(/\{\s*"\$binary"\s*:\s*\{\s*"base64"\s*:\s*"([^"]*)"\s*,\s*"subType"\s*:\s*"([0-9a-f]{1,2})"\s*\}\s*\}/gi)) {
    const sub = { '00': 'generic', '03': 'old UUID', '04': 'UUID', '05': 'MD5', '06': 'encrypted', '07': 'compressed column' }[m[2].padStart(2, '0')] || 'user-defined'
    push('$binary', m[0], `${Math.floor((m[1].length * 3) / 4)} bytes, subtype ${m[2]} (${sub})`)
  }
  for (const m of s.matchAll(/(?:\{\s*"\$oid"\s*:\s*"|ObjectId\(\s*["'])([0-9a-f]{24})/gi)) {
    const o = objectId(m[1])
    push('ObjectId', m[0], `created ${o.created.toISOString()}, counter ${o.counter}`)
  }
  return out
}
