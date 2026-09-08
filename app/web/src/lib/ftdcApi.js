// FTDC Summary API — read MongoDB's diagnostic.data and turn it into charts.

async function toJSON(res) {
  const text = await res.text()
  let data = null
  try { data = text ? JSON.parse(text) : null } catch { /* not json */ }
  if (!res.ok) throw new Error(data?.error || text || `HTTP ${res.status}`)
  return data
}

// rangeQuery turns a zoom window into the query string both endpoints accept. A zoom is a
// second read of the same source narrowed to a window — not a crop of what was drawn —
// because the drawn line is thinned to 1,200 points and magnifying it adds nothing.
function rangeQuery(range) {
  if (!range || (!range.from && !range.to)) return ''
  const q = new URLSearchParams()
  if (range.from) q.set('from', Math.floor(range.from))
  if (range.to) q.set('to', Math.ceil(range.to))
  return `?${q}`
}

export const ftdcApi = {
  // Every running MongoDB node — each mongod keeps diagnostic.data with no configuration.
  nodes: async () => (await toJSON(await fetch('/api/ftdc/targets', { credentials: 'same-origin' }))) || [],

  // One or more metrics.* files, or a .tar.gz of a whole diagnostic.data directory.
  upload: async (files, range = null) => {
    const fd = new FormData()
    for (const f of files) fd.append('files', f)
    return toJSON(await fetch(`/api/ftdc/upload${rangeQuery(range)}`,
      { method: 'POST', body: fd, credentials: 'same-origin' }))
  },

  // Several members at once. Each capture is summarised on its own and overlaid here —
  // see compareCharts.
  compare: async (targets, range = null) =>
    toJSON(await fetch(`/api/ftdc/compare${rangeQuery(range)}`, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ targets }),
    })),

  fromNode: async (stackId, nodeId, range = null) =>
    toJSON(await fetch(`/api/stacks/${stackId}/nodes/${nodeId}/ftdc${rangeQuery(range)}`,
      { method: 'POST', credentials: 'same-origin' })),
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

// COMPARE_CHARTS are the charts worth reading across members, and which series of each
// carries the comparison. The rest of a chart's series stay on the single-member view: three
// members' cache charts overlaid is nine lines and no answer.
//
// Every one of these is a question about the SET rather than about a server, which is the
// whole reason to read three files at once — a member's own capture can only ever report
// what that member believed.
export const COMPARE_CHARTS = [
  { id: 'memberState', series: 0, title: 'Member state', why: 'Each member\'s own state, from its own file. Where two members disagree about who was primary, this is where it shows.' },
  { id: 'replLag', series: 0, title: 'Replication lag', why: 'Each member\'s view of how far behind the set it was.' },
  { id: 'oplogWindow', series: 0, title: 'Oplog window', why: 'The recovery budget on each member. They differ: a member with a smaller oplog is the one that needs a resync first.' },
  { id: 'tickets', series: 0, title: 'Read tickets available', why: 'Admission control on each member. A secondary out of tickets cannot apply the oplog either.' },
  { id: 'oplogApply', series: 0, title: 'Oplog application', why: 'How fast each secondary applied what the primary sent.' },
  { id: 'waiting', series: 0, title: 'Time operations spent waiting', why: 'Queueing on each member, side by side.' },
  { id: 'cache', series: 0, title: 'Cache in use', why: 'The cache on each member. A secondary evicting harder than the primary is a secondary that will fall behind.' },
  { id: 'processPressure', series: 0, title: 'I/O pressure', why: 'Which member was actually stalled on its disk.' },
  { id: 'diskSpace', series: 0, title: 'Free space', why: 'The member that runs out first takes the majority with it.' },
]

// compareSeries builds one overlay chart: the same chart id from every member, each drawn
// as one line named after the member.
//
// The members' captures do not share a clock — different sample counts, different start
// times, and a member that was down for part of the window has no samples for it. So the
// timestamps are merged and each member's value is carried forward from its last sample,
// which is what a step function of a gauge means. A member with no sample yet contributes
// nothing rather than a zero, because zero is a reading and "not running" is not.
export function compareSeries(members, spec) {
  const tracks = []
  for (const m of members) {
    const chart = m.model?.charts?.find((c) => c.id === spec.id)
    const s = chart?.series?.[spec.series]
    if (!chart || !s?.points?.length || !m.model.ts?.length) continue
    tracks.push({ label: m.label, ts: m.model.ts, values: s.points, unit: chart.unit })
  }
  if (tracks.length < 2) return null
  const all = new Set()
  for (const t of tracks) for (const x of t.ts) all.add(Math.round(x))
  const stamps = [...all].sort((a, b) => a - b)
  const idx = tracks.map(() => 0)
  const last = tracks.map(() => null)
  const points = stamps.map((t) => {
    const v = {}
    tracks.forEach((tr, i) => {
      while (idx[i] < tr.ts.length && Math.round(tr.ts[idx[i]]) <= t) {
        last[i] = tr.values[idx[i]] ?? last[i]
        idx[i]++
      }
      if (last[i] !== null) v[`s${i}`] = last[i]
    })
    return { t, v }
  })
  return {
    points,
    lines: tracks.map((t, i) => ({ key: `s${i}`, label: t.label, color: i })),
    unit: tracks[0].unit,
  }
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
