// FTDC Summary API — read MongoDB's diagnostic.data and turn it into charts.

async function toJSON(res) {
  const text = await res.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { /* not json */ }
  if (!res.ok) throw new Error(data?.error || text || `HTTP ${res.status}`)
  return data
}

export const ftdcApi = {
  // Every running MongoDB node — each mongod keeps diagnostic.data with no configuration.
  nodes: async () => (await toJSON(await fetch('/api/ftdc/targets', { credentials: 'same-origin' }))) || [],

  // One or more metrics.* files, or a .tar.gz of a whole diagnostic.data directory.
  upload: async (files) => {
    const fd = new FormData()
    for (const f of files) fd.append('files', f)
    return toJSON(await fetch('/api/ftdc/upload', { method: 'POST', body: fd, credentials: 'same-origin' }))
  },

  fromNode: async (stackId, nodeId) =>
    toJSON(await fetch(`/api/stacks/${stackId}/nodes/${nodeId}/ftdc`, { method: 'POST', credentials: 'same-origin' })),
}

// --- advisor severities ------------------------------------------------------
//
// The same four levels the Stalk Summary's advisors use, and the same colours, because a
// reader moving between the two pages should not have to relearn what amber means. They
// map onto the validated status triad rather than the theme's success/warning/danger,
// which are decorative and fail a colour-vision check against each other.

export const ADVICE_TEXT = {
  ok: 'Nothing to do',
  warn: 'Worth a look',
  crit: 'Needs attention',
  info: 'For information',
}

export const ADVICE_FILL = {
  ok: 'bg-status-ok',
  warn: 'bg-status-warn',
  crit: 'bg-status-crit',
  info: 'bg-muted',
}

export const ADVICE_TONE = {
  ok: 'text-status-ok',
  warn: 'text-status-warn',
  crit: 'text-status-crit',
  info: 'text-muted',
}

// --- shaping -----------------------------------------------------------------

// chartPoints turns the model's parallel arrays into the {t, v:{…}} shape TimeChart wants.
//
// The backend sends one timestamp column and one array per series, which is how FTDC
// itself is laid out and keeps the payload small; TimeChart wants a row per sample. This
// is the join, done once per chart rather than inside the render.
export function chartPoints(ts, series) {
  if (!ts?.length || !series?.length) return []
  return ts.map((t, i) => {
    const v = {}
    for (let s = 0; s < series.length; s++) v[`s${s}`] = series[s].points?.[i] ?? 0
    return { t, v }
  })
}

export function chartLines(series) {
  return (series || []).map((s, i) => ({ key: `s${i}`, label: s.name, color: i }))
}

// fmtNum keeps big counters readable without pretending to more precision than a chart
// axis can show.
export function fmtNum(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '—'
  const a = Math.abs(n)
  if (a >= 1e9) return `${(n / 1e9).toFixed(1)}B`
  if (a >= 1e6) return `${(n / 1e6).toFixed(1)}M`
  if (a >= 1e3) return `${(n / 1e3).toFixed(1)}k`
  if (a >= 10) return n.toFixed(0)
  return n.toFixed(a >= 1 ? 1 : 2)
}

export function fmtSpan(from, to) {
  const s = Math.max(0, (to || 0) - (from || 0))
  if (s < 90) return `${s.toFixed(0)}s`
  if (s < 5400) return `${(s / 60).toFixed(1)} min`
  if (s < 172800) return `${(s / 3600).toFixed(1)} h`
  return `${(s / 86400).toFixed(1)} days`
}

export function fmtClock(ts) {
  if (!ts) return '—'
  const d = new Date(ts * 1000)
  return d.toISOString().slice(0, 19).replace('T', ' ')
}
