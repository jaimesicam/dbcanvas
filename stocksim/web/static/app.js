// Stock Market Sim dashboard.
//
// Mirrors the sibling sims' shape: GET /api/state polled every 2s is the
// authoritative recovery path; a WebSocket (/ws) is a convenience push channel
// only, never relied on exclusively. No charting library, no framework — plain
// DOM. The sparklines are inline SVG polylines built from the tick endpoint.

const $ = (sel) => document.querySelector(sel)
const el = (tag, cls, text) => {
  const n = document.createElement(tag)
  if (cls) n.className = cls
  if (text !== undefined) n.textContent = text
  return n
}

async function api(path, opts) {
  const res = await fetch(path, opts)
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText)
  return res.status === 204 ? null : res.json()
}

const jsonReq = (method, body) => ({
  method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
})

// ------------------------------------------------------------------ format

const nf0 = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 })
const nf2 = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const money = (n) => '$' + nf2.format(n || 0)
const compact = (n) => {
  const a = Math.abs(n || 0)
  if (a >= 1e12) return '$' + (n / 1e12).toFixed(2) + 'T'
  if (a >= 1e9) return '$' + (n / 1e9).toFixed(2) + 'B'
  if (a >= 1e6) return '$' + (n / 1e6).toFixed(2) + 'M'
  if (a >= 1e3) return '$' + (n / 1e3).toFixed(1) + 'K'
  return money(n)
}
// dirClass/dirGlyph are the secondary encoding that keeps gain and loss legible
// without relying on colour — see the note on --up/--down in style.css.
const dirClass = (n) => (n > 0 ? 'up' : n < 0 ? 'down' : 'flat')
const dirGlyph = (n) => (n > 0 ? '▲' : n < 0 ? '▼' : '■')
const signed = (n, d = 2) => (n > 0 ? '+' : '') + n.toFixed(d)
const timeOf = (iso) => (iso ? new Date(iso).toLocaleTimeString() : '—')

// ------------------------------------------------------------------- state

let state = null
let sparkCache = new Map() // securityId -> number[]
let sectors = new Set()
const pages = { sec: 0, pf: 0, ord: 0 }
const PAGE = 25
let portfolioOptions = []
let securityOptions = []

// ------------------------------------------------------------- data fetches

async function fetchState() {
  try {
    state = await api('/api/state')
    setConn(true)
    renderHeader(state)
    renderBanners(state)
    renderSeed(state.seed)
    renderKPIs(state)
    renderTicker(state.ticker || [])
    renderAgents(state.agents || [])
    renderSchema(state.diag)
  } catch (e) {
    setConn(false)
    showError('Cannot reach the Stock Market Sim server: ' + e.message)
  }
}

async function fetchSecurities() {
  const q = new URLSearchParams({
    search: $('#sec-search').value, filter: $('#sec-sector').value,
    limit: PAGE, offset: pages.sec * PAGE,
  })
  const d = await api('/api/securities?' + q)
  securityOptions = d.securities || []
  renderSecTable(d)
}

async function fetchPortfolios() {
  const q = new URLSearchParams({
    search: $('#pf-search').value, limit: PAGE, offset: pages.pf * PAGE,
  })
  const d = await api('/api/portfolios?' + q)
  portfolioOptions = d.portfolios || []
  renderPfTable(d)
}

async function fetchOrders() {
  const q = new URLSearchParams({
    search: $('#ord-search').value, filter: $('#ord-status').value,
    limit: PAGE, offset: pages.ord * PAGE,
  })
  renderOrdTable(await api('/api/orders?' + q))
}

async function fetchEvents() {
  try {
    const d = await api('/api/events?limit=40')
    const list = $('#event-list')
    list.innerHTML = ''
    // The API returns oldest-first and pushEvent prepends, so iterating in
    // order leaves the newest event at the top — reversing here would put the
    // oldest there instead.
    ;(d.events || []).forEach(pushEvent)
  } catch (e) { /* the banner already reports connectivity */ }
}

// Sparkline data is refreshed on a slower cadence than prices: the line's shape
// is what matters, and pulling 20 tick histories every two seconds would cost
// far more than it tells anyone.
async function refreshSparks() {
  if (!state || !state.ticker) return
  for (const row of state.ticker.slice(0, 24)) {
    try {
      const d = await api(`/api/securities/${row.id}/ticks?limit=40`)
      sparkCache.set(row.id, (d.ticks || []).map((t) => t.price))
    } catch (e) { /* leave the previous line in place */ }
  }
  renderTicker(state.ticker)
}

// ------------------------------------------------------------------ render

function setConn(ok) {
  $('#conn-dot').className = 'dot ' + (ok ? 'ok' : 'bad')
  $('#conn-text').textContent = ok ? 'connected' : 'disconnected'
}

function showError(msg) {
  const b = $('#banner-error')
  b.textContent = msg
  b.classList.toggle('hidden', !msg)
}

function renderBanners(s) {
  showError(s.error || '')
  const w = $('#banner-warn')
  w.textContent = s.warning ? 'Last background error — ' + s.warning : ''
  w.classList.toggle('hidden', !s.warning)
}

function renderHeader(s) {
  const c = s.control || {}
  $('#hdr-engine').textContent = c.engine || '—'
  $('#hdr-target').textContent = c.targetLabel || c.targetKind || '—'
  $('#hdr-db').textContent = c.database || '—'
  $('#dz-db').textContent = c.database || 'the database'
  $('#hdr-clock').textContent = c.simNow
    ? new Date(c.simNow).toISOString().replace('T', ' ').slice(0, 19) + ' UTC'
    : '—'
  $('#btn-pause').textContent = c.state === 'paused' ? 'Resume' : 'Pause'
  document.querySelectorAll('[data-level]').forEach((b) => {
    b.classList.toggle('active', b.dataset.level === c.level)
  })
}

function renderSeed(seed) {
  const panel = $('#seed-panel')
  if (!seed || (seed.done && !seed.error)) { panel.classList.add('hidden'); return }
  panel.classList.remove('hidden')
  $('#seed-fill').style.width = (seed.percent || 0) + '%'
  $('#seed-step').textContent = seed.error ? 'Failed: ' + seed.error : (seed.step || '')
}

function tile(value, label, cls) {
  const t = el('div', 'stat-tile')
  const v = el('div', 'v' + (cls ? ' ' + cls : ''), value)
  t.append(v, el('div', 'l', label))
  return t
}

function renderKPIs(s) {
  const m = s.summary ? s.summary : {}
  const g = $('#kpi-grid')
  g.innerHTML = ''
  g.append(
    tile(nf2.format(m.indexLevel || 0), 'Index level'),
    tile(String(m.securities || 0), 'Securities'),
    tile(`${dirGlyph(1)} ${m.advancers || 0}`, 'Advancing', 'up'),
    tile(`${dirGlyph(-1)} ${m.decliners || 0}`, 'Declining', 'down'),
    tile(compact(m.marketCap || 0), 'Market cap'),
    tile(nf0.format(m.dayVolume || 0), 'Session volume'),
    tile(nf0.format(m.openOrders || 0), 'Open orders'),
    tile(nf0.format(m.filledOrders || 0), 'Filled orders'),
    tile(compact(m.portfolioAum || 0), 'Assets under mgmt'),
    tile(String(m.portfolios || 0), 'Portfolios'),
  )
}

// sparkline builds an inline SVG polyline. One series, so no legend is needed —
// the tile's symbol names it. A flat or single-point history draws nothing
// rather than a misleading straight line at an arbitrary height.
function sparkline(points, direction) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg')
  svg.setAttribute('class', 'spark')
  svg.setAttribute('viewBox', '0 0 100 28')
  svg.setAttribute('preserveAspectRatio', 'none')
  svg.setAttribute('aria-hidden', 'true')
  if (!points || points.length < 2) return svg

  const min = Math.min(...points), max = Math.max(...points)
  const range = max - min || 1
  const step = 100 / (points.length - 1)
  const coords = points.map((p, i) => `${(i * step).toFixed(2)},${(25 - ((p - min) / range) * 22).toFixed(2)}`)

  const line = document.createElementNS('http://www.w3.org/2000/svg', 'polyline')
  line.setAttribute('points', coords.join(' '))
  line.setAttribute('fill', 'none')
  line.setAttribute('stroke-width', '2')
  line.setAttribute('stroke-linejoin', 'round')
  line.setAttribute('stroke-linecap', 'round')
  line.setAttribute('vector-effect', 'non-scaling-stroke')
  line.setAttribute('stroke', `var(--${direction === 'flat' ? 'flat' : direction})`)
  svg.append(line)
  return svg
}

function renderTicker(rows) {
  const g = $('#ticker-grid')
  g.innerHTML = ''
  $('#ticker-hint').textContent = rows.length
    ? `${rows.length} instruments, largest movers first`
    : 'No securities yet'
  for (const r of rows) {
    const d = dirClass(r.changePct)
    const tileEl = el('div', `ticker-tile ${d}` + (r.listed ? '' : ' delisted'))
    const top = el('div', 'tt-top')
    top.append(el('span', 'tt-sym', r.symbol), el('span', 'tt-name', r.name))
    const chg = el('div', `tt-chg ${d}`,
      `${dirGlyph(r.changePct)} ${signed(r.change)} (${signed(r.changePct)}%)`)
    tileEl.append(top, el('div', 'tt-price', nf2.format(r.last)), chg,
      sparkline(sparkCache.get(r.id), d))
    tileEl.onclick = () => openSecurityForm(r.id)
    g.append(tileEl)
    if (r.sector) sectors.add(r.sector)
  }
  syncSectorFilter()
}

function syncSectorFilter() {
  const sel = $('#sec-sector')
  if (sel.options.length - 1 === sectors.size) return
  const cur = sel.value
  sel.innerHTML = '<option value="">All sectors</option>'
  ;[...sectors].sort().forEach((s) => {
    const o = el('option', null, s); o.value = s; sel.append(o)
  })
  sel.value = cur
}

function buildTable(table, headers, rows) {
  table.innerHTML = ''
  const thead = el('thead'), tr = el('tr')
  headers.forEach((h) => {
    const th = el('th', h.num ? 'num' : null, h.label)
    tr.append(th)
  })
  thead.append(tr)
  const tbody = el('tbody')
  rows.forEach((cells) => {
    const r = el('tr')
    cells.forEach((c) => {
      const td = el('td', c.cls || null)
      if (c.node) td.append(c.node); else td.textContent = c.text
      r.append(td)
    })
    tbody.append(r)
  })
  table.append(thead, tbody)
}

function actionCell(onEdit, onDelete) {
  const d = el('div', 'row-actions')
  const e = el('button', 'btn', 'Edit'); e.onclick = onEdit
  const x = el('button', 'btn danger', 'Delete'); x.onclick = onDelete
  d.append(e, x)
  return d
}

function renderSecTable(d) {
  const rows = (d.securities || []).map((s) => {
    const chg = s.lastPrice - s.openPrice
    const pct = s.openPrice ? (chg / s.openPrice) * 100 : 0
    return [
      { text: s.symbol }, { text: s.name }, { text: s.sector || '—' },
      { text: nf2.format(s.lastPrice), cls: 'num' },
      { text: `${dirGlyph(pct)} ${signed(pct)}%`, cls: 'num ' + dirClass(pct) },
      { text: nf0.format(s.dayVolume), cls: 'num' },
      { text: s.listed ? 'listed' : 'delisted' },
      { node: actionCell(() => openSecurityForm(s.id), () => deleteSecurity(s)) },
    ]
  })
  buildTable($('#sec-table'), [
    { label: 'Symbol' }, { label: 'Name' }, { label: 'Sector' },
    { label: 'Last', num: true }, { label: 'Change', num: true },
    { label: 'Volume', num: true }, { label: 'Status' }, { label: '' },
  ], rows)
  $('#sec-count').textContent = pageLabel(d, pages.sec)
}

function renderPfTable(d) {
  const rows = (d.portfolios || []).map((p) => [
    { text: p.name }, { text: p.owner },
    { text: money(p.cash), cls: 'num' },
    { text: new Date(p.createdAt).toLocaleDateString() },
    { node: actionCell(() => openPortfolioForm(p.id), () => deletePortfolio(p)) },
  ])
  buildTable($('#pf-table'), [
    { label: 'Name' }, { label: 'Owner' }, { label: 'Cash', num: true },
    { label: 'Opened' }, { label: '' },
  ], rows)
  $('#pf-count').textContent = pageLabel(d, pages.pf)
}

function renderOrdTable(d) {
  const rows = (d.orders || []).map((o) => {
    const pill = el('span', 'status-pill ' + o.status, o.status)
    return [
      { text: o.symbol }, { text: o.owner || '—' },
      { text: o.side, cls: dirClass(o.side === 'buy' ? 1 : -1) },
      { text: o.orderType },
      { text: nf0.format(o.quantity), cls: 'num' },
      { text: o.orderType === 'limit' ? nf2.format(o.limitPrice) : 'market', cls: 'num' },
      { node: pill },
      { text: timeOf(o.createdAt) },
      { node: actionCell(() => openOrderForm(o.id), () => deleteOrder(o)) },
    ]
  })
  buildTable($('#ord-table'), [
    { label: 'Symbol' }, { label: 'Owner' }, { label: 'Side' }, { label: 'Type' },
    { label: 'Qty', num: true }, { label: 'Limit', num: true },
    { label: 'Status' }, { label: 'Placed' }, { label: '' },
  ], rows)
  $('#ord-count').textContent = pageLabel(d, pages.ord)
}

function pageLabel(d, page) {
  const from = d.total === 0 ? 0 : page * PAGE + 1
  const to = Math.min((page + 1) * PAGE, d.total)
  return `${from}–${to} of ${d.total}`
}

function renderSchema(diag) {
  const objs = (diag && diag.objects) || []
  const rows = objs.map((o) => [
    { text: o.name }, { text: o.kind },
    { text: nf0.format(o.rows), cls: 'num' },
    { text: o.bytes ? (o.bytes / 1024).toFixed(0) + ' KB' : '—', cls: 'num' },
  ])
  buildTable($('#schema-table'), [
    { label: 'Object' }, { label: 'Kind' },
    { label: 'Rows (est.)', num: true }, { label: 'Size', num: true },
  ], rows)
  $('#schema-hint').textContent = diag
    ? `${diag.engine} ${diag.serverVersion || ''} · ${objs.length} objects in ${diag.database}`
    : ''
}

function renderAgents(agents) {
  const l = $('#agent-list')
  l.innerHTML = ''
  for (const a of agents) {
    const row = el('div', 'agent-row')
    const dot = el('span', 'dot ' + (a.status === 'ok' ? 'ok' : a.status === 'error' ? 'bad' : ''))
    row.append(dot, el('span', 'nm', a.agent), el('span', 'dt', a.detail || a.status))
    l.append(row)
  }
}

function pushEvent(ev) {
  const list = $('#event-list')
  const row = el('div', 'event-row')
  row.append(
    el('span', 'ts', timeOf(ev.ts)),
    el('span', 'kd', ev.kind || ''),
    el('span', null, ev.message),
  )
  list.prepend(row)
  while (list.childElementCount > 60) list.lastElementChild.remove()
}

// -------------------------------------------------------------------- modal

let modalSave = null

function openModal(title, bodyNode, onSave) {
  $('#modal-title').textContent = title
  const body = $('#modal-body')
  body.innerHTML = ''
  body.append(bodyNode)
  $('#modal-error').classList.add('hidden')
  modalSave = onSave
  $('#modal').classList.remove('hidden')
}

function closeModal() {
  $('#modal').classList.add('hidden')
  modalSave = null
}

function modalError(msg) {
  const e = $('#modal-error')
  e.textContent = msg
  e.classList.toggle('hidden', !msg)
}

function field(label, input, wide) {
  const l = el('label', wide ? 'wide' : null)
  l.append(el('span', null, label), input)
  return l
}

function input(type, value, attrs = {}) {
  const i = document.createElement('input')
  i.type = type
  i.value = value === undefined || value === null ? '' : value
  Object.entries(attrs).forEach(([k, v]) => i.setAttribute(k, v))
  return i
}

function select(options, value) {
  const s = document.createElement('select')
  options.forEach(([v, label]) => {
    const o = el('option', null, label); o.value = v; s.append(o)
  })
  s.value = value
  return s
}

// --------------------------------------------------------------- CRUD forms

async function openSecurityForm(id) {
  const existing = id ? await api('/api/securities/' + id) : null
  const grid = el('div', 'form-grid')
  const f = {
    symbol: input('text', existing?.symbol || '', { maxlength: 16, placeholder: 'ACME' }),
    name: input('text', existing?.name || '', { placeholder: 'Acme Industrial Corp' }),
    sector: input('text', existing?.sector || '', { placeholder: 'Industrials' }),
    currency: input('text', existing?.currency || 'USD', { maxlength: 3 }),
    openPrice: input('number', existing?.openPrice ?? 100, { step: '0.01', min: '0' }),
    shares: input('number', existing?.sharesOutstanding ?? 1000000, { step: '1', min: '0' }),
    listed: select([['true', 'Listed'], ['false', 'Delisted']],
      existing ? String(existing.listed) : 'true'),
  }
  grid.append(
    field('Symbol', f.symbol), field('Currency', f.currency),
    field('Name', f.name, true), field('Sector', f.sector),
    field('Status', f.listed),
    field(existing ? 'Session open price' : 'Starting price', f.openPrice),
    field('Shares outstanding', f.shares),
  )
  if (existing) {
    const note = el('p', 'hint',
      'Last price and volume are driven by the simulation and are not editable here. '
      + 'Changing the session open re-baselines the day’s change.')
    note.style.gridColumn = '1 / -1'
    grid.append(note)
  }

  openModal(existing ? 'Edit security' : 'New security', grid, async () => {
    const body = {
      symbol: f.symbol.value, name: f.name.value, sector: f.sector.value,
      currency: f.currency.value, openPrice: Number(f.openPrice.value),
      sharesOutstanding: Number(f.shares.value), listed: f.listed.value === 'true',
    }
    if (existing) await api('/api/securities/' + id, jsonReq('PUT', body))
    else await api('/api/securities', jsonReq('POST', body))
    await Promise.all([fetchSecurities(), fetchState()])
  })
}

async function deleteSecurity(s) {
  if (!confirm(`Delete ${s.symbol}?\n\nIts price history, holdings and open orders go too. `
    + `Executed trades are kept as a record of what happened.`)) return
  await api('/api/securities/' + s.id, { method: 'DELETE' })
  await Promise.all([fetchSecurities(), fetchState()])
}

async function openPortfolioForm(id) {
  const existing = id ? await api('/api/portfolios/' + id) : null
  const grid = el('div', 'form-grid')
  const f = {
    name: input('text', existing?.name || '', { placeholder: 'Growth Account' }),
    owner: input('text', existing?.owner || '', { placeholder: 'Alice' }),
    cash: input('number', existing?.cash ?? 1000000, { step: '0.01' }),
  }
  grid.append(field('Name', f.name, true), field('Owner', f.owner), field('Cash', f.cash))

  openModal(existing ? 'Edit portfolio' : 'New portfolio', grid, async () => {
    const body = { name: f.name.value, owner: f.owner.value, cash: Number(f.cash.value) }
    if (existing) await api('/api/portfolios/' + id, jsonReq('PUT', body))
    else await api('/api/portfolios', jsonReq('POST', body))
    await Promise.all([fetchPortfolios(), fetchState()])
  })
}

async function deletePortfolio(p) {
  if (!confirm(`Close ${p.name} (${p.owner})?\n\nIts positions and open orders go too.`)) return
  await api('/api/portfolios/' + p.id, { method: 'DELETE' })
  await Promise.all([fetchPortfolios(), fetchState()])
}

async function openOrderForm(id) {
  const existing = id ? await api('/api/orders/' + id) : null
  // Refresh the reference lists so the drop-downs cannot offer something that
  // was deleted a moment ago.
  const [secs, pfs] = await Promise.all([
    api('/api/securities?limit=500'), api('/api/portfolios?limit=500'),
  ])
  const grid = el('div', 'form-grid')
  const f = {
    portfolio: select((pfs.portfolios || []).map((p) => [p.id, `${p.name} (${p.owner})`]),
      existing?.portfolioId || ''),
    security: select((secs.securities || []).map((s) => [s.id, `${s.symbol} — ${s.name}`]),
      existing?.securityId || ''),
    side: select([['buy', 'Buy'], ['sell', 'Sell']], existing?.side || 'buy'),
    type: select([['market', 'Market'], ['limit', 'Limit']], existing?.orderType || 'market'),
    qty: input('number', existing?.quantity ?? 100, { step: '1', min: '1' }),
    limit: input('number', existing?.limitPrice ?? 0, { step: '0.01', min: '0' }),
    status: select([['open', 'Open'], ['filled', 'Filled'],
      ['cancelled', 'Cancelled'], ['rejected', 'Rejected']], existing?.status || 'open'),
  }
  const limitField = field('Limit price', f.limit)
  const syncLimit = () => limitField.classList.toggle('hidden', f.type.value !== 'limit')
  f.type.onchange = syncLimit

  if (existing) {
    grid.append(field('Side', f.side), field('Type', f.type),
      field('Quantity', f.qty), limitField, field('Status', f.status))
    const note = el('p', 'hint', `${existing.symbol} for ${existing.owner}. `
      + `Cancelling keeps the order in the book’s history.`)
    note.style.gridColumn = '1 / -1'
    grid.append(note)
  } else {
    grid.append(field('Portfolio', f.portfolio, true), field('Security', f.security, true),
      field('Side', f.side), field('Type', f.type), field('Quantity', f.qty), limitField)
  }
  syncLimit()

  openModal(existing ? 'Edit order' : 'New order', grid, async () => {
    if (existing) {
      await api('/api/orders/' + id, jsonReq('PUT', {
        side: f.side.value, orderType: f.type.value, quantity: Number(f.qty.value),
        limitPrice: Number(f.limit.value), status: f.status.value,
      }))
    } else {
      await api('/api/orders', jsonReq('POST', {
        portfolioId: f.portfolio.value, securityId: f.security.value,
        side: f.side.value, orderType: f.type.value,
        quantity: Number(f.qty.value), limitPrice: Number(f.limit.value),
      }))
    }
    await Promise.all([fetchOrders(), fetchState()])
  })
}

async function deleteOrder(o) {
  if (!confirm(`Delete this ${o.side} order for ${o.symbol}?`)) return
  await api('/api/orders/' + o.id, { method: 'DELETE' })
  await Promise.all([fetchOrders(), fetchState()])
}

// ---------------------------------------------------------------- controls

async function control(path, body) {
  await api(path, body ? jsonReq('POST', body) : { method: 'POST' })
  await fetchState()
}

async function dropEverything() {
  const db = state?.control?.database || ''
  const typed = prompt(
    `This drops every table this application created in "${db}".\n\n`
    + `The database itself is left in place, and nothing else inside it is `
    + `touched — but all data this app has written will be gone.\n\n`
    + `Type the database name to confirm:`)
  if (typed === null) return
  try {
    await api('/api/control/drop', jsonReq('POST', { confirm: typed.trim() }))
    await Promise.all([fetchState(), fetchSecurities(), fetchPortfolios(), fetchOrders()])
  } catch (e) {
    alert('Not dropped: ' + e.message)
  }
}

// -------------------------------------------------------------- websocket

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${location.host}/ws`)
  ws.onmessage = (msg) => {
    try {
      const m = JSON.parse(msg.data)
      if (m.type === 'event' && m.event) pushEvent(m.event)
      if (m.type === 'seed' && m.seed) renderSeed(m.seed)
    } catch (e) { /* a malformed frame is never worth breaking the page over */ }
  }
  ws.onclose = () => setTimeout(connectWS, 2000)
  ws.onerror = () => ws.close()
}

function debounce(fn, ms) {
  let t
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms) }
}

// -------------------------------------------------------------------- boot

document.addEventListener('DOMContentLoaded', () => {
  $('#btn-pause').onclick = () =>
    control(state?.control?.state === 'paused' ? '/api/control/resume' : '/api/control/pause')
  document.querySelectorAll('[data-level]').forEach((b) => {
    b.onclick = () => control('/api/control/level', { level: b.dataset.level })
  })

  $('#btn-new-security').onclick = () => openSecurityForm(null)
  $('#btn-new-portfolio').onclick = () => openPortfolioForm(null)
  $('#btn-new-order').onclick = () => openOrderForm(null)

  $('#modal-close').onclick = closeModal
  $('#modal-cancel').onclick = closeModal
  $('#modal-save').onclick = async () => {
    if (!modalSave) return
    const btn = $('#modal-save')
    btn.disabled = true
    try {
      await modalSave()
      closeModal()
    } catch (e) {
      modalError(e.message)
    } finally {
      btn.disabled = false
    }
  }

  const reload = { sec: fetchSecurities, pf: fetchPortfolios, ord: fetchOrders }
  document.querySelectorAll('[data-page]').forEach((b) => {
    b.onclick = () => {
      const k = b.dataset.page
      pages[k] = Math.max(0, pages[k] + Number(b.dataset.dir))
      reload[k]()
    }
  })
  const onSearch = (k) => debounce(() => { pages[k] = 0; reload[k]() }, 300)
  $('#sec-search').oninput = onSearch('sec')
  $('#pf-search').oninput = onSearch('pf')
  $('#ord-search').oninput = onSearch('ord')
  $('#sec-sector').onchange = () => { pages.sec = 0; fetchSecurities() }
  $('#ord-status').onchange = () => { pages.ord = 0; fetchOrders() }

  $('#btn-reset').onclick = async () => {
    if (!confirm('Delete all rows and re-create the seed universe?')) return
    await control('/api/control/reset')
    await Promise.all([fetchSecurities(), fetchPortfolios(), fetchOrders()])
  }
  $('#btn-wipe').onclick = async () => {
    if (!confirm('Delete all rows, keeping the tables in place?')) return
    await control('/api/control/wipe')
    await Promise.all([fetchSecurities(), fetchPortfolios(), fetchOrders()])
  }
  $('#btn-drop').onclick = dropEverything

  // Sparklines are chained after the first state fetch, not fired alongside it:
  // refreshSparks needs the ticker's security ids, so calling it in parallel
  // makes it a no-op and leaves the tiles blank until the 15s interval fires.
  fetchState().then(refreshSparks)
  fetchSecurities(); fetchPortfolios(); fetchOrders(); fetchEvents()
  connectWS()

  setInterval(fetchState, 2000)
  setInterval(refreshSparks, 15000)
  setInterval(() => { fetchSecurities(); fetchOrders() }, 10000)
})
