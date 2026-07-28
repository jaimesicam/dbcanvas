// MarketChaos dashboard — stage S0 scaffold. Mirrors the sibling sims'
// shape: GET /api/state polled every 2s is the authoritative recovery path;
// a WebSocket (/ws) is a convenience push channel only, never relied on
// exclusively. No charting library, no framework — plain DOM.

const errorBanner = document.getElementById('error-banner')
const connStatus = document.getElementById('conn-status')
const kindBadge = document.getElementById('kind-badge')
const versionBadge = document.getElementById('version-badge')
const agentsList = document.getElementById('agents-list')

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

document.getElementById('reset-btn').addEventListener('click', async () => {
  await fetch('/api/control/reset', { method: 'POST' })
  fetchState()
})

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${location.host}/ws`)
  ws.onclose = () => setTimeout(connectWS, 2000)
  ws.onerror = () => ws.close()
  // No event payloads to consume yet (stage S0 has no agents publishing to
  // the bus) — the connection exists so the dashboard proves the WS bridge
  // itself works end to end.
}

fetchState()
setInterval(fetchState, 2000)
connectWS()
