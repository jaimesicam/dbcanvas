// time.js — the Log time converter: reads the timestamp forms database logs use and writes them in
// the others, in any time zone.
//
//   epoch            seconds, milliseconds, microseconds or nanoseconds (told apart by size)
//   ISO 8601         2026-10-27T08:02:29.331039Z, with or without an offset
//   MySQL            2026-10-27 08:02:29[.ffffff] (no zone: read as UTC, as the error log writes it)
//   mongod           {"$date": "2026-10-27T08:20:38.000+00:00"} or {"$date": {"$numberLong": "…"}}
//   PostgreSQL       2026-10-27 08:02:29.123 UTC / +00

// parseTimestamp returns { date, micros, source } or { error }. micros keeps the sub-millisecond
// digits a Date cannot.
export function parseTimestamp(input) {
  let s = String(input ?? '').trim()
  if (!s) return { error: 'paste a timestamp' }
  const mongo = /"\$date"\s*:\s*(?:"([^"]+)"|\{\s*"\$numberLong"\s*:\s*"(-?\d+)"\s*\}|(-?\d+))/.exec(s)
  if (mongo) {
    if (mongo[1]) s = mongo[1]
    else return epoch(mongo[2] || mongo[3], 'mongod $date (ms)', 'ms')
  }
  if (/^-?\d+(\.\d+)?$/.test(s)) return epoch(s)
  // MySQL / PostgreSQL forms: a space instead of T, optional zone word.
  const m = /^(\d{4}-\d\d-\d\d)[ T](\d\d:\d\d(?::\d\d)?)(?:\.(\d{1,9}))?\s*(Z|UTC|GMT|[+-]\d\d(?::?\d\d)?)?$/i.exec(s)
  if (!m) return { error: 'not a timestamp this reads: epoch, ISO 8601, MySQL, PostgreSQL or a mongod $date' }
  const frac = (m[3] || '').padEnd(9, '0')
  let zone = (m[4] || 'Z').toUpperCase()
  if (zone === 'UTC' || zone === 'GMT') zone = 'Z'
  if (/^[+-]\d\d$/.test(zone)) zone += ':00'
  if (/^[+-]\d{4}$/.test(zone)) zone = `${zone.slice(0, 3)}:${zone.slice(3)}`
  const date = new Date(`${m[1]}T${m[2].length === 5 ? `${m[2]}:00` : m[2]}.${frac.slice(0, 3)}${zone}`)
  if (isNaN(date)) return { error: 'the date or time is out of range' }
  return { date, micros: Number(frac.slice(3, 6)), source: m[4] ? 'with zone' : 'no zone given — read as UTC' }
}

function epoch(str, label, unit) {
  const n = Number(str)
  const digits = String(str).replace(/^-/, '').split('.')[0].length
  const u = unit || (digits <= 11 ? 's' : digits <= 14 ? 'ms' : digits <= 17 ? 'µs' : 'ns')
  const ms = { s: n * 1000, ms: n, 'µs': n / 1000, ns: n / 1e6 }[u]
  const date = new Date(Math.floor(ms))
  if (isNaN(date)) return { error: 'out of range' }
  return { date, micros: Math.round((ms - Math.floor(ms)) * 1000), source: label || `epoch ${u}` }
}

const pad = (n, w = 2) => String(n).padStart(w, '0')

// zoned formats a date in an IANA time zone as YYYY-MM-DD HH:MM:SS.ffffff ±HH:MM.
export function zoned(date, micros, timeZone) {
  try {
    const parts = Object.fromEntries(new Intl.DateTimeFormat('en-CA', {
      timeZone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
      second: '2-digit', hourCycle: 'h23', timeZoneName: 'longOffset',
    }).formatToParts(date).map((p) => [p.type, p.value]))
    const off = (parts.timeZoneName || 'GMT').replace('GMT', '') || '+00:00'
    return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}.${pad(date.getUTCMilliseconds(), 3)}${pad(micros, 3)} ${off}`
  } catch {
    return `unknown time zone "${timeZone}"`
  }
}

// forms writes one instant in every form the card shows.
export function forms(date, micros = 0) {
  const ms = date.getTime()
  const iso = date.toISOString()
  return [
    { label: 'ISO 8601 (UTC)', value: iso.replace(/\.(\d{3})Z$/, `.$1${pad(micros, 3)}Z`) },
    { label: 'MySQL', value: iso.replace('T', ' ').replace(/\.(\d{3})Z$/, `.$1${pad(micros, 3)}`) },
    { label: 'mongod $date', value: `{"$date": "${iso.replace('Z', '+00:00')}"}` },
    { label: 'epoch seconds', value: String(Math.floor(ms / 1000)) },
    { label: 'epoch milliseconds', value: String(ms) },
    { label: 'epoch microseconds', value: `${ms}${pad(micros, 3)}` },
  ]
}

// between describes the time from a to b.
export function between(a, b) {
  let ms = b.date.getTime() - a.date.getTime() + ((b.micros || 0) - (a.micros || 0)) / 1000
  const sign = ms < 0 ? '−' : ''
  ms = Math.abs(ms)
  const d = Math.floor(ms / 86400000)
  const h = Math.floor((ms % 86400000) / 3600000)
  const m = Math.floor((ms % 3600000) / 60000)
  const s = (ms % 60000) / 1000
  const parts = []
  if (d) parts.push(`${d}d`)
  if (h || d) parts.push(`${h}h`)
  if (m || h || d) parts.push(`${m}m`)
  parts.push(`${s.toFixed(3).replace(/\.?0+$/, '') || '0'}s`)
  return { text: sign + parts.join(' '), ms: (sign ? -1 : 1) * ms }
}

export const ZONES = ['UTC', 'America/New_York', 'America/Chicago', 'America/Los_Angeles', 'Europe/London',
  'Europe/Berlin', 'Asia/Kolkata', 'Asia/Manila', 'Asia/Singapore', 'Asia/Tokyo', 'Australia/Sydney']
