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
setInterval(fetchState, 2000)
setInterval(fetchDiagnostics, 2000)
connectWS()
