// MarketChaos dashboard — stage S0 scaffold. Mirrors the sibling sims'
// shape: GET /api/state polled every 2s is the authoritative recovery path;
// a WebSocket (/ws) is a convenience push channel only, never relied on
// exclusively. No charting library, no framework — plain DOM.

const errorBanner = document.getElementById('error-banner')
const connStatus = document.getElementById('conn-status')
const kindBadge = document.getElementById('kind-badge')
const versionBadge = document.getElementById('version-badge')
const agentsList = document.getElementById('agents-list')
const seedPanel = document.getElementById('seed-panel')
const mixSelect = document.getElementById('mix-select')
let mixSelectFocused = false
mixSelect.addEventListener('focus', () => { mixSelectFocused = true })
mixSelect.addEventListener('blur', () => { mixSelectFocused = false })

async function fetchState() {
  try {
    const res = await fetch('/api/state')
    const snap = await res.json()
    render(snap)
  } catch (e) {
    errorBanner.textContent = 'Could not reach MarketChaos itself: ' + e.message
    errorBanner.classList.remove('hidden')
    connStatus.classList.remove('ok')
  }
}

function render(snap) {
  if (snap.error) {
    errorBanner.textContent = snap.error
    errorBanner.classList.remove('hidden')
    connStatus.classList.remove('ok')
  } else {
    errorBanner.classList.add('hidden')
    connStatus.classList.add('ok')
  }
  kindBadge.textContent = (snap.control && snap.control.kind) || '—'
  versionBadge.textContent = snap.serverVersion || '—'
  document.getElementById('stat-state').textContent = (snap.control && snap.control.state) || '—'
  document.getElementById('stat-level').textContent = (snap.control && snap.control.level) || '—'
  document.getElementById('stat-uptime').textContent = snap.uptimeSeconds != null ? snap.uptimeSeconds : '—'

  document.querySelectorAll('.controls button[data-level]').forEach((btn) => {
    btn.classList.toggle('active', snap.control && btn.dataset.level === snap.control.level)
  })
  if (!mixSelectFocused && snap.control && snap.control.mix) {
    mixSelect.value = snap.control.mix
  }

  renderSeed(snap.seed)

  if (Array.isArray(snap.agents) && snap.agents.length > 0) {
    agentsList.innerHTML = ''
    for (const a of snap.agents) {
      const row = document.createElement('div')
      row.className = 'agent-row'
      const dot = document.createElement('span')
      dot.className = 'dot ' + (a.status === 'ok' ? 'ok' : '')
      row.appendChild(dot)
      const label = document.createElement('span')
      label.textContent = `${a.name} — ${a.status}${a.detail ? ' (' + a.detail + ')' : ''}`
      row.appendChild(label)
      agentsList.appendChild(row)
    }
  } else {
    agentsList.textContent = 'No agents running yet.'
  }

  const kind = (snap.control && snap.control.kind) || ''
  const isPXC = kind === 'pxcnode' || kind === 'pxc' || kind === 'haproxy-pxc'
  const isHAProxy = kind.startsWith('haproxy-')
  document.getElementById('pxc-panel').classList.toggle('hidden', !isPXC)
  document.getElementById('haproxy-panel').classList.toggle('hidden', !isHAProxy)
}

async function fetchDiagnostics() {
  try {
    const [stats, board] = await Promise.all([
      fetch('/api/diag/serverstats').then((r) => r.json()),
      fetch('/api/diag/leaderboard').then((r) => r.json()),
    ])
    renderServerStats(stats)
    renderLeaderboard(board)
  } catch (e) {
    // best-effort — the next poll recovers; the main error banner already
    // covers "can't reach MarketChaos at all"
  }
  if (!document.getElementById('pxc-panel').classList.contains('hidden')) {
    fetch('/api/diag/wsrep').then((r) => r.json()).then(renderWsrep).catch(() => {})
  }
  if (!document.getElementById('haproxy-panel').classList.contains('hidden')) {
    fetch('/api/diag/haproxy').then((r) => r.json()).then(renderHAProxy).catch(() => {})
  }
}

function pct(v) { return v == null ? '—' : (v * 100).toFixed(1) + '%' }
function num(v, digits) { return v == null ? '—' : Number(v).toFixed(digits != null ? digits : 1) }

function renderServerStats(s) {
  document.getElementById('dbp-qps').textContent = num(s.qps, 0)
  document.getElementById('dbp-tps').textContent = num(s.tps, 1)
  document.getElementById('dbp-rw').textContent = num(s.readWriteRatio, 2)
  document.getElementById('dbp-lockwait').textContent = num(s.lockWaitsPerSec, 2)
  document.getElementById('dbp-deadlocks').textContent = s.deadlocks != null ? s.deadlocks : '—'
  document.getElementById('dbp-tmpdisk').textContent = num(s.tmpDiskTablesPerSec, 2)
  document.getElementById('dbp-hitrate').textContent = pct(s.bufferPoolHitRate)
  document.getElementById('dbp-threads').textContent = s.threadsConnected != null ? s.threadsConnected : '—'
  document.getElementById('dbp-pool-cfg').textContent = s.poolConfigured != null ? s.poolConfigured : '—'
  document.getElementById('dbp-pool-inuse').textContent = s.poolInUse != null ? s.poolInUse : '—'
  document.getElementById('dbp-max-used').textContent = s.maxUsedConnections != null ? s.maxUsedConnections : '—'
}

function renderLeaderboard(rows) {
  const body = document.getElementById('leaderboard-body')
  if (!Array.isArray(rows) || rows.length === 0) {
    body.innerHTML = '<tr><td colspan="8" class="muted">No activity yet.</td></tr>'
    return
  }
  body.innerHTML = ''
  for (const r of rows.slice(0, 15)) {
    const tr = document.createElement('tr')
    tr.innerHTML = `<td>${r.label}</td><td>${r.agent}</td>` +
      `<td class="num">${r.calls}</td><td class="num">${r.avgMs.toFixed(2)}</td><td class="num">${r.maxMs.toFixed(2)}</td>` +
      `<td class="num">${r.rowsExamined}</td><td class="num">${r.noIndexUsed}</td><td class="num">${r.tmpDiskTables}</td>`
    body.appendChild(tr)
  }
}

function renderWsrep(w) {
  document.getElementById('pxc-state').textContent = w.LocalStateComment || '—'
  document.getElementById('pxc-size').textContent = w.ClusterSize != null ? w.ClusterSize : '—'
  document.getElementById('pxc-certfail').textContent = w.LocalCertFailures != null ? w.LocalCertFailures : '—'
  document.getElementById('pxc-bfaborts').textContent = w.LocalBFAborts != null ? w.LocalBFAborts : '—'
  document.getElementById('pxc-fc-paused').textContent = w.FlowControlPaused != null ? w.FlowControlPaused.toFixed(4) : '—'
  document.getElementById('pxc-recv-q').textContent = w.ReceiveQueueLen != null ? w.ReceiveQueueLen : '—'
  document.getElementById('pxc-send-q').textContent = w.SendQueueLen != null ? w.SendQueueLen : '—'
  document.getElementById('pxc-latency').textContent = w.ReplLatencyAvgMs != null ? w.ReplLatencyAvgMs.toFixed(2) : '—'
}

function renderHAProxy(rows) {
  const body = document.getElementById('haproxy-body')
  if (!Array.isArray(rows) || rows.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="muted">No data yet.</td></tr>'
    return
  }
  body.innerHTML = ''
  for (const r of rows) {
    const tr = document.createElement('tr')
    tr.innerHTML = `<td>${r.Backend}</td><td>${r.Server}</td><td>${r.Status}</td>` +
      `<td class="num">${r.Weight}</td><td class="num">${r.CurSess}</td><td class="num">${r.TotSess}</td>`
    body.appendChild(tr)
  }
}

document.querySelectorAll('.controls button[data-level]').forEach((btn) => {
  btn.addEventListener('click', async () => {
    await fetch('/api/control/level', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ level: btn.dataset.level }),
    })
    fetchState()
  })
})

mixSelect.addEventListener('change', async () => {
  await fetch('/api/control/mix', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mix: mixSelect.value }),
  })
  fetchState()
})

document.getElementById('reset-btn').addEventListener('click', async () => {
  await fetch('/api/control/reset', { method: 'POST' })
  fetchState()
})

// renderSeed shows the seed-progress panel while the initial (or a
// post-Reset) seed is running, and hides it once done — a market that's
// already seeded has nothing left to show here. RowsTotal is 0 until the
// seeder has counted the current table, so the bar only reflects real
// progress once a table is in flight, never a misleading 0/0 stall.
function renderSeed(seed) {
  if (!seed || seed.done) {
    seedPanel.classList.add('hidden')
    return
  }
  seedPanel.classList.remove('hidden')
  const pct = seed.rowsTotal > 0 ? Math.min(100, Math.round((seed.rowsDone / seed.rowsTotal) * 100)) : 0
  document.getElementById('seed-bar').style.width = pct + '%'
  document.getElementById('seed-table').textContent = seed.table || '—'
  document.getElementById('seed-rows').textContent = seed.rowsTotal ? `${seed.rowsDone.toLocaleString()} / ${seed.rowsTotal.toLocaleString()}` : '—'
  const elapsed = seed.startedAt ? Math.max(0, Math.round((Date.now() - new Date(seed.startedAt).getTime()) / 1000)) : null
  document.getElementById('seed-elapsed').textContent = elapsed != null ? elapsed : '—'
  const status = document.getElementById('seed-status')
  if (seed.error) {
    status.textContent = 'Seed failed: ' + seed.error
  } else {
    status.textContent = `Seeding ${seed.table || 'market data'}… the market isn't ready for traffic until this finishes.`
  }
}

// ----------------------------------------------------------- challenges

let challengeCatalog = null // fetched once — the catalog itself never changes for a running deployment

async function fetchChallenges() {
  try {
    if (!challengeCatalog) {
      challengeCatalog = await fetch('/api/challenges').then((r) => r.json())
    }
    const active = await fetch('/api/challenges/active').then((r) => r.json())
    renderChallenges(active)
  } catch (e) {
    // best-effort — the next poll recovers
  }
}

function renderChallenges(active) {
  const idle = document.getElementById('challenge-idle')
  const activeEl = document.getElementById('challenge-active')
  if (!active || !active.active) {
    idle.classList.remove('hidden')
    activeEl.classList.add('hidden')
    renderChallengeList()
    return
  }
  idle.classList.add('hidden')
  activeEl.classList.remove('hidden')
  renderActiveChallenge(active)
}

function renderChallengeList() {
  const list = document.getElementById('challenge-list')
  if (!Array.isArray(challengeCatalog) || challengeCatalog.length === 0) {
    list.innerHTML = '<p class="muted">No challenges available for this target.</p>'
    return
  }
  list.innerHTML = ''
  for (const c of challengeCatalog) {
    const card = document.createElement('div')
    card.className = 'challenge-card'
    const tags = [`<span class="tag">${c.difficulty}</span>`, `<span class="tag">${c.category}</span>`]
    if (c.requiresFamily === 'pxc') tags.push('<span class="tag pxc">PXC</span>')
    card.innerHTML = `<div class="title">${c.title}</div><div class="tags">${tags.join('')}</div>` +
      `<div class="muted">${c.hintCount} hints available</div>`
    const btn = document.createElement('button')
    btn.textContent = 'Start'
    btn.addEventListener('click', async () => {
      const res = await fetch(`/api/challenges/${c.id}/start`, { method: 'POST' })
      if (res.ok) {
        document.getElementById('ca-grade-result').classList.add('hidden')
        document.getElementById('ca-hints').innerHTML = ''
        fetchChallenges()
      } else {
        alert(await res.text())
      }
    })
    card.appendChild(btn)
    list.appendChild(card)
  }
}

function renderActiveChallenge(active) {
  document.getElementById('ca-title').textContent = active.title
  const tags = [`<span class="tag">${active.difficulty}</span>`, `<span class="tag">${active.category}</span>`, `<span class="tag">${active.mechanism === 'app' ? 'app-behavior fix' : 'SQL fix'}</span>`]
  document.getElementById('ca-badges').innerHTML = tags.join(' ')
  document.getElementById('ca-scenario').textContent = active.scenario
  document.getElementById('ca-symptom').textContent = active.symptom

  const diagField = document.getElementById('ca-diagnosis')
  if (document.activeElement !== diagField) diagField.value = active.diagnosis || ''

  const variantBlock = document.getElementById('ca-variant-block')
  const variantStatus = document.getElementById('ca-variant-status')
  if (active.mechanism === 'app') {
    variantBlock.classList.remove('hidden')
    if (active.appliedVariant) {
      variantStatus.textContent = 'Improved implementation applied.'
      document.getElementById('ca-variant-btn').disabled = true
    } else {
      document.getElementById('ca-variant-btn').disabled = false
      variantStatus.textContent = active.hintsUsed > 0 && active.diagnosis
        ? '' : 'Use a hint and save a diagnosis to unlock this.'
    }
  } else {
    variantBlock.classList.add('hidden')
  }

  const hintBtn = document.getElementById('ca-hint-btn')
  hintBtn.disabled = active.hintsUsed >= active.totalHints
  hintBtn.textContent = active.hintsUsed >= active.totalHints ? 'No more hints' : `Get a hint (${active.hintsUsed}/${active.totalHints} used)`

  const baselineBtn = document.getElementById('ca-baseline-btn')
  const validateBtn = document.getElementById('ca-validate-btn')
  const gradingStatus = document.getElementById('ca-grading-status')
  const haveBaseline = active.state === 'baseline' || active.state === 'graded'
  if (!gradingInFlight) {
    baselineBtn.disabled = haveBaseline
    validateBtn.disabled = !haveBaseline
    gradingStatus.textContent = haveBaseline
      ? 'Baseline captured — fix the problem, then Validate Solution.'
      : 'Capture a baseline first, while the problem is still active.'
  }
}

let gradingInFlight = false

async function runGradingStep(btnId, path, onDone) {
  if (gradingInFlight) return
  gradingInFlight = true
  const status = document.getElementById('ca-grading-status')
  document.getElementById('ca-baseline-btn').disabled = true
  document.getElementById('ca-validate-btn').disabled = true
  status.textContent = 'Measuring… this takes about 15 seconds.'
  try {
    const res = await fetch(path, { method: 'POST' })
    if (!res.ok) {
      status.textContent = await res.text()
      return
    }
    onDone(await res.json())
  } finally {
    gradingInFlight = false
    fetchChallenges()
  }
}

document.getElementById('ca-baseline-btn').addEventListener('click', () => {
  runGradingStep('ca-baseline-btn', '/api/challenges/baseline', () => {
    document.getElementById('ca-grading-status').textContent = 'Baseline captured — fix the problem, then Validate Solution.'
  })
})

document.getElementById('ca-validate-btn').addEventListener('click', () => {
  runGradingStep('ca-validate-btn', '/api/challenges/validate', renderGrade)
})

function renderGrade(g) {
  const box = document.getElementById('ca-grade-result')
  box.classList.remove('hidden')
  document.getElementById('ca-grading-status').textContent = ''
  document.getElementById('ca-grade-total').textContent = `${g.totalScore} / 100`
  document.getElementById('ca-grade-band').textContent = g.grade
  document.getElementById('ca-grade-gate').textContent = g.passed
    ? (g.functionalNote || '')
    : `Correctness gate failed: ${g.correctnessFailure} — score is 0 regardless of everything else.`
  document.getElementById('ca-grade-functional').textContent = g.functionalPoints
  document.getElementById('ca-grade-performance').textContent = g.performancePoints
  document.getElementById('ca-grade-regression').textContent = g.regressionPoints
  document.getElementById('ca-grade-diagnosis').textContent = g.diagnosisPoints
  document.getElementById('ca-grade-metric').textContent =
    `${g.performanceMetric}: ${g.performanceBefore.toFixed(2)} baseline -> ${g.performanceAfter.toFixed(2)} now` +
    (g.regressionNote ? ` · ${g.regressionNote}` : '')
}

document.getElementById('ca-hint-btn').addEventListener('click', async () => {
  const res = await fetch('/api/challenges/hint', { method: 'POST' })
  if (!res.ok) return
  const hint = await res.json()
  const hints = document.getElementById('ca-hints')
  const div = document.createElement('div')
  div.className = 'ca-hint'
  div.textContent = `Hint ${hint.tier}: ${hint.text}`
  hints.appendChild(div)
  fetchChallenges()
})

document.getElementById('ca-reset-btn').addEventListener('click', async () => {
  document.getElementById('ca-hints').innerHTML = ''
  document.getElementById('ca-grade-result').classList.add('hidden')
  await fetch('/api/challenges/reset', { method: 'POST' })
  fetchChallenges()
})

document.getElementById('ca-diagnosis-btn').addEventListener('click', async () => {
  const text = document.getElementById('ca-diagnosis').value
  await fetch('/api/challenges/diagnosis', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text }),
  })
  fetchChallenges()
})

document.getElementById('ca-variant-btn').addEventListener('click', async () => {
  const res = await fetch('/api/challenges/apply-variant', { method: 'POST' })
  if (!res.ok) { alert(await res.text()); return }
  fetchChallenges()
})

// ------------------------------------------------------------- market panels

async function fetchMarketPanels() {
  try {
    const [overview, trades, portfolio] = await Promise.all([
      fetch('/api/market/overview').then((r) => r.json()),
      fetch('/api/market/trades?limit=20').then((r) => r.json()),
      fetch('/api/market/portfolio?account=1').then((r) => r.json()),
    ])
    renderMarketOverview(overview)
    renderRecentTrades(trades)
    renderPortfolio(portfolio)
  } catch (e) {
    // best-effort — next poll recovers
  }
  const symbol = document.getElementById('orderbook-symbol').value.trim()
  if (symbol) {
    fetch('/api/market/orderbook?symbol=' + encodeURIComponent(symbol))
      .then((r) => (r.ok ? r.json() : null))
      .then((levels) => { if (levels) renderOrderBook(levels) })
      .catch(() => {})
  }
}

function moverRow(m) {
  const cls = m.pctChange >= 0 ? 'pct-up' : 'pct-down'
  const sign = m.pctChange >= 0 ? '+' : ''
  return `<tr><td>${m.symbol}</td><td class="num">$${m.lastPrice.toFixed(2)}</td><td class="num ${cls}">${sign}${m.pctChange.toFixed(2)}%</td></tr>`
}

function renderMarketOverview(ov) {
  document.getElementById('market-gainers').innerHTML = (ov.topGainers || []).map(moverRow).join('') || '<tr><td class="muted">No data yet.</td></tr>'
  document.getElementById('market-losers').innerHTML = (ov.topLosers || []).map(moverRow).join('') || '<tr><td class="muted">No data yet.</td></tr>'
  const sectors = document.getElementById('market-sectors')
  sectors.innerHTML = (ov.sectors || []).map((s) =>
    `<div class="stat-tile"><div class="v">${s.volume.toLocaleString()}</div><div class="l">${s.sectorName}</div></div>`).join('')
}

function renderRecentTrades(trades) {
  const body = document.getElementById('trades-body')
  if (!Array.isArray(trades) || trades.length === 0) {
    body.innerHTML = '<tr><td colspan="4" class="muted">No trades yet.</td></tr>'
    return
  }
  body.innerHTML = trades.map((t) =>
    `<tr><td>${t.symbol}</td><td class="num">$${t.price.toFixed(2)}</td><td class="num">${t.quantity}</td><td>${t.executedAt}</td></tr>`).join('')
}

function renderOrderBook(levels) {
  const box = document.getElementById('orderbook-body')
  if (!Array.isArray(levels) || levels.length === 0) {
    box.innerHTML = '<div class="muted">No open orders for that symbol (or symbol not found).</div>'
    return
  }
  box.innerHTML = levels.map((l) =>
    `<div class="stat-tile"><div class="v">${l.count} / ${l.qty.toLocaleString()}</div><div class="l">${l.side} orders / qty</div></div>`).join('')
}

function renderPortfolio(pf) {
  document.getElementById('pf-cash').textContent = '$' + (pf.cashBalance || 0).toFixed(2)
  document.getElementById('pf-holdings').textContent = '$' + (pf.holdingsValue || 0).toFixed(2)
  document.getElementById('pf-total').textContent = '$' + (pf.totalValue || 0).toFixed(2)
  const body = document.getElementById('pf-holdings-body')
  if (!Array.isArray(pf.holdings) || pf.holdings.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="muted">No open positions for this account.</td></tr>'
    return
  }
  body.innerHTML = pf.holdings.map((h) => {
    const cls = h.unrealizedPl >= 0 ? 'pct-up' : 'pct-down'
    return `<tr><td>${h.symbol}</td><td class="num">${h.quantity}</td><td class="num">$${h.averageCost.toFixed(2)}</td>` +
      `<td class="num">$${h.lastPrice.toFixed(2)}</td><td class="num">$${h.marketValue.toFixed(2)}</td>` +
      `<td class="num ${cls}">$${h.unrealizedPl.toFixed(2)}</td></tr>`
  }).join('')
}

document.getElementById('orderbook-symbol').addEventListener('change', fetchMarketPanels)

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${location.host}/ws`)
  ws.onmessage = (ev) => {
    try {
      const msg = JSON.parse(ev.data)
      if (msg.type === 'seed' && msg.seed) renderSeed(msg.seed)
    } catch (e) {
      // ignore malformed pushes — the next /api/state poll recovers state
    }
  }
  ws.onclose = () => setTimeout(connectWS, 2000)
  ws.onerror = () => ws.close()
}

fetchState()
fetchDiagnostics()
fetchChallenges()
fetchMarketPanels()
setInterval(fetchState, 2000)
setInterval(fetchDiagnostics, 2000)
setInterval(fetchChallenges, 2000)
setInterval(fetchMarketPanels, 3000)
connectWS()
