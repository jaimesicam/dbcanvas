// diff.js — a line diff (Myers' O(ND) algorithm), hand-written for the Text Diff card.
//
// diffLines returns the edit script as [{ op: '=' | '-' | '+', text, a, b }], where a and b are the
// 1-based line numbers on each side (null where the line does not exist on that side).

export function diffLines(aText, bText) {
  const a = splitLines(aText)
  const b = splitLines(bText)
  const n = a.length
  const m = b.length
  const max = n + m
  const v = new Int32Array(2 * max + 2)
  const trace = []
  let found = false
  for (let d = 0; d <= max && !found; d++) {
    trace.push(v.slice())
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[max + k - 1] < v[max + k + 1]) ? v[max + k + 1] : v[max + k - 1] + 1
      let y = x - k
      while (x < n && y < m && a[x] === b[y]) { x++; y++ }
      v[max + k] = x
      if (x >= n && y >= m) { found = true; break }
    }
  }
  // Walk the trace back from (n, m) to build the script.
  const out = []
  let x = n
  let y = m
  for (let d = trace.length - 1; d >= 0; d--) {
    const vd = trace[d]
    const k = x - y
    const prevK = k === -d || (k !== d && vd[max + k - 1] < vd[max + k + 1]) ? k + 1 : k - 1
    const prevX = vd[max + prevK]
    const prevY = prevX - prevK
    while (x > prevX && y > prevY) { out.push({ op: '=', text: a[x - 1], a: x, b: y }); x--; y-- }
    if (d > 0) {
      if (x === prevX) out.push({ op: '+', text: b[y - 1], a: null, b: y })
      else out.push({ op: '-', text: a[x - 1], a: x, b: null })
    }
    x = prevX
    y = prevY
  }
  return out.reverse()
}

function splitLines(t) {
  const s = String(t ?? '')
  if (!s) return []
  return s.replace(/\r\n/g, '\n').replace(/\n$/, '').split('\n')
}

export function diffStats(ops) {
  return { added: ops.filter((o) => o.op === '+').length, removed: ops.filter((o) => o.op === '-').length }
}
