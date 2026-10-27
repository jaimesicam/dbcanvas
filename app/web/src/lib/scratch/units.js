// units.js — the Database units card: sizes in bytes both ways, config-file suffixes, pages and
// durations.
//
// Every size is given in decimal units (1 KB = 1,000 B — disks, network, cloud quotas) and binary
// units (1 KiB = 1,024 B — memory, and every MySQL / MongoDB / PostgreSQL size setting) side by
// side, because a value read in one and typed into the other is wrong by 2.4% at KB and 10% at TB.
// my.cnf suffixes (K, M, G, T) are binary: MySQL reads `128M` as 134217728 bytes.

export const DECIMAL = [['B', 1], ['KB', 1e3], ['MB', 1e6], ['GB', 1e9], ['TB', 1e12], ['PB', 1e15]]
export const BINARY = [['B', 1], ['KiB', 2 ** 10], ['MiB', 2 ** 20], ['GiB', 2 ** 30], ['TiB', 2 ** 40], ['PiB', 2 ** 50]]
const ALL = Object.fromEntries([...DECIMAL, ...BINARY].map(([u, f]) => [u.toLowerCase(), f]))

// parseSize reads "134217728", "128M", "128 MiB", "1.5 GB", "2TiB" or "16k" into bytes.
// `mode` decides what a bare K/M/G/T suffix means: 'mycnf' (binary, MySQL's rule — the default)
// or 'decimal'. An explicit KB/KiB always means what it says. Returns { bytes } or { error }.
export function parseSize(input, mode = 'mycnf') {
  const s = String(input ?? '').trim().replace(/,/g, '').replace(/_/g, '')
  const m = /^(-?\d+(?:\.\d+)?(?:e[+-]?\d+)?)\s*([a-zA-Z]*)$/i.exec(s)
  if (!m) return { error: 'expected a number with an optional unit, e.g. 128M, 1.5 GB, 134217728' }
  const n = Number(m[1])
  const u = m[2].toLowerCase()
  if (!u || u === 'b' || u === 'bytes' || u === 'byte') return { bytes: n }
  if (/^[kmgtp]$/.test(u)) {
    const pow = 'kmgtp'.indexOf(u) + 1
    return { bytes: n * (mode === 'decimal' ? 1000 ** pow : 1024 ** pow), suffix: true }
  }
  if (ALL[u] !== undefined) return { bytes: n * ALL[u] }
  return { error: `unknown unit "${m[2]}" — use B, KB/MB/GB/TB (decimal), KiB/MiB/GiB/TiB (binary) or a bare K/M/G/T` }
}

const fmt = (v) => {
  if (v === 0) return '0'
  if (Math.abs(v) >= 1e15 || Math.abs(v) < 1e-6) return v.toExponential(4)
  return Number.isInteger(v) ? v.toLocaleString('en-US') : v.toLocaleString('en-US', { maximumFractionDigits: 6 })
}

// sizeTable is every unit of one family for a byte count.
export function sizeTable(bytes, family = DECIMAL) {
  return family.map(([unit, f]) => ({ unit, value: fmt(bytes / f) }))
}

// human is the largest unit of a family the value is at least 1 of.
export function human(bytes, family = BINARY) {
  let [unit, f] = family[0]
  for (const [u, g] of family) if (Math.abs(bytes) >= g) { unit = u; f = g }
  return `${fmt(Math.round((bytes / f) * 1000) / 1000)} ${unit}`
}

// mycnf writes bytes the way my.cnf would: the largest binary suffix that divides it exactly.
export function mycnf(bytes) {
  if (!Number.isInteger(bytes) || bytes < 0) return ''
  for (const [s, f] of [['T', 2 ** 40], ['G', 2 ** 30], ['M', 2 ** 20], ['K', 2 ** 10]]) {
    if (bytes >= f && bytes % f === 0) return `${bytes / f}${s}`
  }
  return String(bytes)
}

// Pages: how many of each engine's pages a size is.
export const PAGES = [
  { label: 'InnoDB pages (16 KiB)', size: 16384 },
  { label: 'PostgreSQL pages (8 KiB)', size: 8192 },
  { label: 'WiredTiger leaf pages (32 KiB default max)', size: 32768 },
]

// Durations: one value in each unit.
export const DURATIONS = [['ns', 1e-9], ['µs', 1e-6], ['ms', 1e-3], ['s', 1], ['min', 60], ['h', 3600], ['d', 86400]]

export function parseDuration(input) {
  const s = String(input ?? '').trim().replace(/,/g, '')
  const m = /^(-?\d+(?:\.\d+)?)\s*(ns|us|µs|ms|s|sec|secs|seconds?|m|min|mins|minutes?|h|hr|hours?|d|days?)?$/i.exec(s)
  if (!m) return { error: 'expected a number with an optional unit: ns, µs, ms, s, min, h, d' }
  const u = (m[2] || 's').toLowerCase()
  const f = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, m: 60, min: 60, mins: 60, h: 3600, hr: 3600, d: 86400 }[u]
    ?? (u.startsWith('min') ? 60 : u.startsWith('h') ? 3600 : u.startsWith('d') ? 86400 : 1)
  return { seconds: Number(m[1]) * f }
}

export function durationTable(seconds) {
  return DURATIONS.map(([unit, f]) => ({ unit, value: fmt(seconds / f) }))
}
