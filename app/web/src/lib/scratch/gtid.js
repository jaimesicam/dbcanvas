// gtid.js — MySQL GTID sets: parse, union, subtract, contains, format.
//
// A set is `source:interval[:interval]...`, comma-separated, where an interval is `n` or `n-m` and
// (MySQL 8.3+) a source may carry a tag: `uuid:tag:1-5`. Kept as a Map of "uuid" or "uuid:tag" →
// sorted, merged [start, end] intervals (BigInt — transaction numbers are 63-bit).

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function parseGtidSet(text) {
  const set = new Map()
  const src = String(text ?? '').replace(/\s+/g, '').replace(/\\n/g, '')
  if (!src) return { set }
  for (const part of src.split(',')) {
    if (!part) continue
    const fields = part.split(':')
    const uuid = fields.shift().toLowerCase()
    if (!UUID.test(uuid)) return { error: `"${uuid}" is not a server UUID` }
    let tag = ''
    let key = uuid
    for (const f of fields) {
      if (/^[a-z_][a-z0-9_]*$/i.test(f)) { tag = f.toLowerCase(); key = `${uuid}:${tag}`; continue }
      const m = /^(\d+)(?:-(\d+))?$/.exec(f)
      if (!m) return { error: `"${f}" is not an interval in "${part}"` }
      const a = BigInt(m[1])
      const b = BigInt(m[2] ?? m[1])
      if (a < 1n || b < a) return { error: `"${f}" is not a valid interval` }
      add(set, key, [a, b])
    }
  }
  return { set }
}

function add(set, key, iv) {
  const list = set.get(key) || []
  list.push(iv)
  set.set(key, normalize(list))
}

function normalize(list) {
  const s = [...list].sort((x, y) => (x[0] < y[0] ? -1 : x[0] > y[0] ? 1 : 0))
  const out = []
  for (const iv of s) {
    const last = out[out.length - 1]
    if (last && iv[0] <= last[1] + 1n) last[1] = iv[1] > last[1] ? iv[1] : last[1]
    else out.push([iv[0], iv[1]])
  }
  return out
}

export function union(a, b) {
  const out = new Map()
  for (const [k, l] of [...a, ...b]) out.set(k, normalize([...(out.get(k) || []), ...l]))
  return out
}

// subtract is a − b: the transactions in a that b does not have.
export function subtract(a, b) {
  const out = new Map()
  for (const [k, list] of a) {
    let rest = list.map((iv) => [...iv])
    for (const [s, e] of b.get(k) || []) {
      const next = []
      for (const [x, y] of rest) {
        if (e < x || s > y) { next.push([x, y]); continue }
        if (s > x) next.push([x, s - 1n])
        if (e < y) next.push([e + 1n, y])
      }
      rest = next
    }
    if (rest.length) out.set(k, rest)
  }
  return out
}

export const isEmpty = (s) => [...s.values()].every((l) => !l.length)
export const contains = (a, b) => isEmpty(subtract(b, a)) // a ⊇ b

export function count(set) {
  let n = 0n
  for (const l of set.values()) for (const [x, y] of l) n += y - x + 1n
  return n
}

// format writes a set the way MySQL prints gtid_executed: sources sorted, one per line.
export function formatGtidSet(set, sep = ',\n') {
  return [...set.keys()].sort().filter((k) => set.get(k).length)
    .map((k) => `${k}:${set.get(k).map(([x, y]) => (x === y ? `${x}` : `${x}-${y}`)).join(':')}`)
    .join(sep)
}
