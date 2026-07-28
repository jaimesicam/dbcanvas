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
setInterval(fetchState, 2000)
connectWS()
