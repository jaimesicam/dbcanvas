// Renders the printable report from a single GET /api/report.json.
//
// One fetch, one render, no polling: a report describes an instant, and a page
// that kept changing while you read or printed it would be a worse report. Use
// the browser's reload to take a fresh one.

const $ = (s) => document.querySelector(s)
const el = (tag, cls, text) => {
  const n = document.createElement(tag)
  if (cls) n.className = cls
  if (text !== undefined) n.textContent = text
  return n
}

const nf0 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 })
const nf2 = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const money = (n) => '$' + nf2.format(n || 0)
const compact = (n) => {
  const a = Math.abs(n || 0)
  if (a >= 1e12) return '$' + (n / 1e12).toFixed(2) + 'T'
  if (a >= 1e9) return '$' + (n / 1e9).toFixed(2) + 'B'
  if (a >= 1e6) return '$' + (n / 1e6).toFixed(2) + 'M'
  return money(n)
}
// Direction is carried by a glyph and a sign as well as by colour, so the
// report stays correct printed in black and white.
const dirClass = (n) => (n > 0 ? 'up' : n < 0 ? 'down' : 'flat')
const dirGlyph = (n) => (n > 0 ? '▲' : n < 0 ? '▼' : '■')
const signed = (n, d = 2) => (n > 0 ? '+' : '') + Number(n).toFixed(d)

const change = (s) => s.lastPrice - s.openPrice
const changePct = (s) => (s.openPrice ? (change(s) / s.openPrice) * 100 : 0)
const marketCap = (s) => s.lastPrice * s.sharesOutstanding

function table(target, headers, rows, footer) {
  const t = $(target)
  t.innerHTML = ''
  const thead = el('thead'), htr = el('tr')
  headers.forEach((h) => htr.append(el('th', h.num ? 'num' : null, h.label)))
  thead.append(htr)
  const tbody = el('tbody')
  if (!rows.length) {
    const tr = el('tr'), td = el('td', 'empty', 'Nothing to report.')
    td.colSpan = headers.length
    tr.append(td); tbody.append(tr)
  }
  rows.forEach((cells) => {
    const tr = el('tr')
    cells.forEach((c) => tr.append(el('td', c.cls || null, c.text)))
    tbody.append(tr)
  })
  t.append(thead, tbody)
  if (footer) {
    const tfoot = el('tfoot'), tr = el('tr')
    footer.forEach((c) => tr.append(el('td', c.cls || null, c.text)))
    tfoot.append(tr); t.append(tfoot)
  }
}

function securityRows(list) {
  return list.map((s) => {
    const pct = changePct(s)
    return [
      { text: s.symbol }, { text: s.name }, { text: s.sector || '—' },
      { text: nf2.format(s.openPrice), cls: 'num' },
      { text: nf2.format(s.lastPrice), cls: 'num' },
      { text: `${dirGlyph(pct)} ${signed(change(s))}`, cls: 'num ' + dirClass(pct) },
      { text: signed(pct) + '%', cls: 'num ' + dirClass(pct) },
      { text: nf2.format(s.dayHigh), cls: 'num' },
      { text: nf2.format(s.dayLow), cls: 'num' },
      { text: nf0.format(s.dayVolume), cls: 'num' },
    ]
  })
}

function moverRows(list) {
  return list.map((s) => {
    const pct = changePct(s)
    return [
      { text: s.symbol },
      { text: nf2.format(s.lastPrice), cls: 'num' },
      { text: `${dirGlyph(pct)} ${signed(pct)}%`, cls: 'num ' + dirClass(pct) },
    ]
  })
}

function renderStatements(statements) {
  const host = $('#statements')
  host.innerHTML = ''
  if (!statements.length) {
    host.append(el('p', 'empty', 'No portfolios exist in this deployment.'))
    return
  }
  for (const st of statements) {
    const wrap = el('div', 'statement')
    const head = el('div', 'statement-head')
    head.append(
      el('span', 'who', `${st.portfolio.owner} — ${st.portfolio.name}`),
      el('span', 'tot', `Total value ${money(st.totalValue)}`),
    )
    wrap.append(head)

    const t = el('table', 'report-table')
    const thead = el('thead'), htr = el('tr')
    const headers = [
      { label: 'Symbol' }, { label: 'Quantity', num: true }, { label: 'Avg cost', num: true },
      { label: 'Last', num: true }, { label: 'Market value', num: true },
      { label: 'Cost basis', num: true }, { label: 'Unrealised P/L', num: true },
    ]
    headers.forEach((h) => htr.append(el('th', h.num ? 'num' : null, h.label)))
    thead.append(htr)
    const tbody = el('tbody')
    if (!st.holdings.length) {
      const tr = el('tr'), td = el('td', 'empty', 'No open positions — the account is entirely in cash.')
      td.colSpan = headers.length
      tr.append(td); tbody.append(tr)
    }
    for (const h of st.holdings) {
      const mv = h.lastPrice * h.quantity
      const cb = h.avgCost * h.quantity
      const pl = mv - cb
      const tr = el('tr')
      ;[
        { text: h.symbol },
        { text: nf0.format(h.quantity), cls: 'num' },
        { text: nf2.format(h.avgCost), cls: 'num' },
        { text: nf2.format(h.lastPrice), cls: 'num' },
        { text: money(mv), cls: 'num' },
        { text: money(cb), cls: 'num' },
        { text: `${dirGlyph(pl)} ${signed(pl)}`, cls: 'num ' + dirClass(pl) },
      ].forEach((c) => tr.append(el('td', c.cls || null, c.text)))
      tbody.append(tr)
    }
    t.append(thead, tbody)

    const tfoot = el('tfoot'), ftr = el('tr')
    ;[
      { text: 'Cash ' + money(st.portfolio.cash) },
      { text: '' }, { text: '' }, { text: '' },
      { text: money(st.marketValue), cls: 'num' },
      { text: money(st.costBasis), cls: 'num' },
      { text: `${dirGlyph(st.unrealisedPl)} ${signed(st.unrealisedPl)}`,
        cls: 'num ' + dirClass(st.unrealisedPl) },
    ].forEach((c) => ftr.append(el('td', c.cls || null, c.text)))
    tfoot.append(ftr); t.append(tfoot)

    wrap.append(t)
    host.append(wrap)
  }
}

function summarySentence(r) {
  const s = r.summary
  const dir = s.advancers > s.decliners ? 'advanced' : s.advancers < s.decliners ? 'declined' : 'was unchanged'
  return `The market ${dir} over the session, with ${s.advancers} of ${s.listed} listed `
    + `instruments higher and ${s.decliners} lower on volume of ${nf0.format(s.dayVolume)} shares. `
    + `Total market capitalisation stands at ${compact(s.marketCap)}, and the equal-weighted `
    + `index closed at ${nf2.format(s.indexLevel)}. Across ${r.portfolios.length} portfolios, `
    + `${compact(s.totalAum)} is under management — ${compact(s.totalCash)} in cash and `
    + `${compact(s.totalEquity)} in positions. Of all orders placed, `
    + `${s.fillRate.toFixed(1)}% have been filled.`
}

async function render() {
  let r
  try {
    const res = await fetch('/api/report.json?trades=100')
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText)
    r = await res.json()
  } catch (e) {
    const box = $('#error')
    box.textContent = 'Could not build the report: ' + e.message
    box.classList.remove('hidden')
    $('#report').classList.add('hidden')
    return
  }

  $('#m-session').textContent = r.sessionDate
  $('#m-generated').textContent = new Date(r.generatedAt).toISOString().replace('T', ' ').slice(0, 19) + ' UTC'
  $('#m-database').textContent = `${r.engine} / ${r.database}`
  $('#m-server').textContent = r.serverVersion || '—'
  $('#f-target').textContent = r.targetLabel
    ? `${r.targetLabel} (${r.targetKind || r.engine})`
    : `${r.engine} ${r.serverVersion || ''}`.trim()

  $('#summary-text').textContent = summarySentence(r)

  const s = r.summary
  const kpis = [
    [nf2.format(s.indexLevel), 'Index level'],
    [compact(s.marketCap), 'Market cap'],
    [`${s.advancers} / ${s.decliners}`, 'Advancing / declining'],
    [nf0.format(s.dayVolume), 'Session volume'],
    [compact(s.totalAum), 'Assets under mgmt'],
    [compact(s.totalCash), 'Cash'],
    [compact(s.totalEquity), 'Positions'],
    [s.fillRate.toFixed(1) + '%', 'Order fill rate'],
  ]
  const row = $('#kpi-row')
  row.innerHTML = ''
  kpis.forEach(([v, l]) => {
    const k = el('div', 'kpi')
    k.append(el('div', 'v', v), el('div', 'l', l))
    row.append(k)
  })

  const byS = [...r.securities].sort((a, b) => a.symbol.localeCompare(b.symbol))
  table('#t-market', [
    { label: 'Symbol' }, { label: 'Name' }, { label: 'Sector' },
    { label: 'Open', num: true }, { label: 'Last', num: true },
    { label: 'Change', num: true }, { label: '%', num: true },
    { label: 'High', num: true }, { label: 'Low', num: true },
    { label: 'Volume', num: true },
  ], securityRows(byS))

  const moverHeaders = [
    { label: 'Symbol' }, { label: 'Last', num: true }, { label: 'Change', num: true },
  ]
  table('#t-gainers', moverHeaders, moverRows(r.topGainers || []))
  table('#t-losers', moverHeaders, moverRows(r.topLosers || []))

  table('#t-traded', [
    { label: 'Symbol' }, { label: 'Name' },
    { label: 'Volume', num: true }, { label: 'Last', num: true },
    { label: 'Market cap', num: true },
  ], (r.mostTraded || []).map((x) => [
    { text: x.symbol }, { text: x.name },
    { text: nf0.format(x.dayVolume), cls: 'num' },
    { text: nf2.format(x.lastPrice), cls: 'num' },
    { text: compact(marketCap(x)), cls: 'num' },
  ]))

  renderStatements(r.statements || [])

  const oc = r.orderCounts || {}
  const total = Object.values(oc).reduce((a, b) => a + b, 0)
  table('#t-orders', [
    { label: 'Status' }, { label: 'Orders', num: true }, { label: 'Share', num: true },
  ], ['open', 'filled', 'cancelled', 'rejected'].map((k) => [
    { text: k },
    { text: nf0.format(oc[k] || 0), cls: 'num' },
    { text: total ? ((oc[k] || 0) / total * 100).toFixed(1) + '%' : '—', cls: 'num' },
  ]), [
    { text: 'Total' }, { text: nf0.format(total), cls: 'num' },
    { text: `${nf0.format(r.totalTrades || 0)} trades, ${nf0.format(r.totalVolume || 0)} shares`, cls: 'num' },
  ])

  $('#objects-note').textContent =
    `Objects this application created in ${r.engine} database "${r.database}". `
    + `Row counts are the engine's own estimates.`
  table('#t-objects', [
    { label: 'Object' }, { label: 'Kind' },
    { label: 'Rows', num: true }, { label: 'Size', num: true },
  ], (r.objects || []).map((o) => [
    { text: o.name }, { text: o.kind },
    { text: nf0.format(o.rows), cls: 'num' },
    { text: o.bytes ? (o.bytes / 1024).toFixed(0) + ' KB' : '—', cls: 'num' },
  ]))
}

document.addEventListener('DOMContentLoaded', render)
