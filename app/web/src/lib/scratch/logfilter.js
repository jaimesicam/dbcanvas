// logfilter.js — the Log filter card: keep or drop lines by text or regular expression, and
// collapse repeats. A repeat is a line that differs from another only in its numbers, hex ids and
// quoted values (timestamps, connection ids, thread ids…), so 400 "Aborted connection 1234 to db"
// lines read as one line ×400.

export function shape(line) {
  return line
    .replace(/\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:?\d\d)?/g, '<time>')
    .replace(/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, '<uuid>')
    .replace(/\b0x[0-9a-f]+\b/gi, '<hex>')
    .replace(/'[^']*'|"[^"]*"/g, '<str>')
    .replace(/\b\d+(\.\d+)?\b/g, '<n>')
}

// filterLines applies include/exclude (plain text, or /regex/flags) and returns { lines, error }.
export function filterLines(text, include, exclude) {
  const all = String(text ?? '').replace(/\r\n/g, '\n').split('\n')
  let inc, exc
  try {
    inc = matcher(include)
    exc = matcher(exclude)
  } catch (e) {
    return { lines: all, error: e.message, total: all.length }
  }
  const lines = all.filter((l) => (!inc || inc(l)) && (!exc || !exc(l)))
  return { lines, total: all.length }
}

function matcher(q) {
  const s = String(q ?? '').trim()
  if (!s) return null
  const re = /^\/(.+)\/([gimsuy]*)$/.exec(s)
  if (re) {
    const r = new RegExp(re[1], re[2].replace('g', ''))
    return (l) => r.test(l)
  }
  const needle = s.toLowerCase()
  return (l) => l.toLowerCase().includes(needle)
}

// collapse groups lines by shape, most frequent first: [{ shape, count, example }].
export function collapse(lines) {
  const groups = new Map()
  for (const l of lines) {
    if (!l.trim()) continue
    const k = shape(l)
    const g = groups.get(k)
    if (g) g.count++
    else groups.set(k, { shape: k, count: 1, example: l })
  }
  return [...groups.values()].sort((a, b) => b.count - a.count)
}
