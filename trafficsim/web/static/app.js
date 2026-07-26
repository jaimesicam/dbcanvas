// Valkey Traffic Lab — frontend. No build step, no framework: this file is served
// exactly as written. State recovery is REST-driven (poll /api/state); the
// WebSocket is a convenience for the live event feed and connection indicator only —
// losing it never loses the map (spec §15: recover via authoritative state, not by
// relying on every push having arrived).

const STATE_COLORS = {
  free: 'var(--free)', moderate: 'var(--moderate)', heavy: 'var(--heavy)',
  severe: 'var(--severe)', blocked: 'var(--blocked)', nodata: 'var(--nodata)',
};
const STATE_LETTER = { free: 'F', moderate: 'M', heavy: 'H', severe: 'S', blocked: 'B', nodata: 'N' };
const SIGNAL_COLORS = { green: '#2ecc71', yellow: '#f1c40f', red: '#e74c3c' };

const canvas = document.getElementById('map');
const ctx = canvas.getContext('2d');
let roadSegments = []; // [{roadId, x1,y1,x2,y2}] for click hit-testing
let lastSnapshot = null;

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || name;
}

async function fetchState() {
  try {
    const res = await fetch('/api/state');
    const snap = await res.json();
    lastSnapshot = snap;
    render(snap);
  } catch (e) {
    console.error('fetchState failed', e);
  }
}

function render(snap) {
  drawMap(snap.roads || [], snap.signals || [], snap.incidents || []);
  renderStats(snap.stats || {}, snap.control || {}, snap.uptimeSeconds || 0);
  renderRankings(snap.rankings || {});
  renderAgents(snap.agents || []);
  document.getElementById('traffic-level').value = snap.control?.level || 'low';
}

// ------------------------------------------------------------------ map

function projection(roads) {
  let minLon = Infinity, maxLon = -Infinity, minLat = Infinity, maxLat = -Infinity;
  for (const r of roads) {
    for (const [lon, lat] of [[+r.fromLon, +r.fromLat], [+r.toLon, +r.toLat]]) {
      if (lon < minLon) minLon = lon; if (lon > maxLon) maxLon = lon;
      if (lat < minLat) minLat = lat; if (lat > maxLat) maxLat = lat;
    }
  }
  const pad = 60;
  const w = canvas.width - pad * 2, h = canvas.height - pad * 2;
  const spanLon = (maxLon - minLon) || 1, spanLat = (maxLat - minLat) || 1;
  return (lon, lat) => [
    pad + ((lon - minLon) / spanLon) * w,
    // screen Y grows downward; lat grows "north" — flip so row 0 is at the top.
    pad + ((lat - minLat) / spanLat) * h,
  ];
}

function drawMap(roads, signals, incidents) {
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  if (!roads.length) {
    ctx.fillStyle = cssVar('--muted');
    ctx.fillText('Waiting for simulation data…', 20, 30);
    return;
  }
  const project = projection(roads);
  roadSegments = [];

  const incByRoad = {};
  for (const inc of incidents) incByRoad[inc.roadId] = inc;

  // Roads.
  for (const r of roads) {
    const [x1, y1] = project(+r.fromLon, +r.fromLat);
    const [x2, y2] = project(+r.toLon, +r.toLat);
    const state = r.state || 'nodata';
    ctx.strokeStyle = cssVar(`--${state}`);
    ctx.lineWidth = 5;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
    roadSegments.push({ roadId: r.id, x1, y1, x2, y2 });

    // Vehicle-count dots (aggregated, not exact per-vehicle) along the segment.
    const count = Math.min(10, +r.vehicleCount || 0);
    for (let i = 0; i < count; i++) {
      const t = (i + 1) / (count + 1);
      const dx = x1 + (x2 - x1) * t, dy = y1 + (y2 - y1) * t;
      ctx.fillStyle = '#e6ecf5';
      ctx.beginPath();
      ctx.arc(dx, dy, 2, 0, Math.PI * 2);
      ctx.fill();
    }

    // State letter badge at the midpoint — accessibility: never color alone.
    const mx = (x1 + x2) / 2, my = (y1 + y2) / 2;
    ctx.fillStyle = '#0e1420';
    ctx.beginPath(); ctx.arc(mx, my, 8, 0, Math.PI * 2); ctx.fill();
    ctx.fillStyle = cssVar(`--${state}`);
    ctx.font = 'bold 10px sans-serif';
    ctx.textAlign = 'center'; ctx.textBaseline = 'middle';
    ctx.fillText(STATE_LETTER[state] || '?', mx, my);

    // Incident marker.
    if (incByRoad[r.id]) {
      ctx.fillStyle = '#fff';
      ctx.font = '13px sans-serif';
      ctx.fillText('⚠', mx, my - 14);
    }
  }

  // Intersections (colored by their signal state).
  const sigByIx = {};
  for (const s of signals) sigByIx[s.intersectionId] = s.state;
  const ixCoords = {};
  for (const r of roads) {
    ixCoords[r.fromId] = [+r.fromLon, +r.fromLat];
    ixCoords[r.toId] = [+r.toLon, +r.toLat];
  }
  for (const [ixId, [lon, lat]] of Object.entries(ixCoords)) {
    const [x, y] = project(lon, lat);
    ctx.fillStyle = SIGNAL_COLORS[sigByIx[ixId]] || '#555';
    ctx.beginPath(); ctx.arc(x, y, 6, 0, Math.PI * 2); ctx.fill();
    ctx.strokeStyle = '#0e1420'; ctx.lineWidth = 2; ctx.stroke();
  }
}

canvas.addEventListener('click', (ev) => {
  const rect = canvas.getBoundingClientRect();
  const x = (ev.clientX - rect.left) * (canvas.width / rect.width);
  const y = (ev.clientY - rect.top) * (canvas.height / rect.height);
  let best = null, bestDist = 14;
  for (const seg of roadSegments) {
    const d = distToSegment(x, y, seg.x1, seg.y1, seg.x2, seg.y2);
    if (d < bestDist) { bestDist = d; best = seg; }
  }
  if (best) showRoadDetail(best.roadId);
  else hideRoadDetail();
});

function distToSegment(px, py, x1, y1, x2, y2) {
  const dx = x2 - x1, dy = y2 - y1;
  const len2 = dx * dx + dy * dy || 1;
  let t = ((px - x1) * dx + (py - y1) * dy) / len2;
  t = Math.max(0, Math.min(1, t));
  const ex = x1 + t * dx, ey = y1 + t * dy;
  return Math.hypot(px - ex, py - ey);
}

function showRoadDetail(roadId) {
  const road = (lastSnapshot?.roads || []).find((r) => r.id === roadId);
  if (!road) return;
  const el = document.getElementById('road-detail');
  const travelTimeMin = ((+road.lengthM / 1000) / Math.max(1, +road.avgSpeed) * 60).toFixed(1);
  el.innerHTML = `
    <span class="close" onclick="hideRoadDetail()">✕</span>
    <h3>${road.name}</h3>
    <div class="row"><span>State</span><b>${STATE_LETTER[road.state]} ${road.state}</b></div>
    <div class="row"><span>Congestion</span><b>${(+road.congestionScore).toFixed(0)}/100</b></div>
    <div class="row"><span>Avg speed</span><b>${(+road.avgSpeed).toFixed(0)} km/h</b></div>
    <div class="row"><span>Speed limit</span><b>${road.speedLimit} km/h</b></div>
    <div class="row"><span>Vehicles</span><b>${road.vehicleCount}</b></div>
    <div class="row"><span>Occupancy</span><b>${(+road.occupancy * 100).toFixed(0)}%</b></div>
    <div class="row"><span>Est. travel time</span><b>${travelTimeMin} min</b></div>
    <div class="row"><span>Last update</span><b>${new Date(road.lastUpdate).toLocaleTimeString()}</b></div>
  `;
  el.classList.remove('hidden');
}
window.hideRoadDetail = () => document.getElementById('road-detail').classList.add('hidden');

// ------------------------------------------------------------------ side panels

function renderStats(stats, control, uptime) {
  const grid = document.getElementById('stat-grid');
  const items = [
    ['Active vehicles', stats.vehiclesActive ?? 0],
    ['Active incidents', stats.incidentsActive ?? 0],
    ['Events total', stats.eventsTotal ?? 0],
    ['Vehicles seen', stats.vehiclesTotal ?? 0],
    ['State', control.state || '—'],
    ['Uptime', `${uptime}s`],
  ];
  grid.innerHTML = items.map(([l, v]) => `<div class="stat"><div class="v">${v}</div><div class="l">${l}</div></div>`).join('');
}

function renderRankings(rankings) {
  const list = document.getElementById('ranking-list');
  const rows = rankings.congestion || [];
  list.innerHTML = rows.map((r) => `<li><span>${r.name}</span><span class="score">${r.score.toFixed(0)}</span></li>`).join('') || '<li>No data yet</li>';
}

function renderAgents(agents) {
  const list = document.getElementById('agent-list');
  const now = Date.now();
  list.innerHTML = agents.map((a) => {
    const age = a.lastActivity ? (now - new Date(a.lastActivity).getTime()) / 1000 : 999;
    const stale = age > 20;
    return `<li><span>${a.type}</span><span class="status ${stale ? 'stale' : 'ok'}">${stale ? 'stale' : 'ok'} · ${a.events || 0} ev</span></li>`;
  }).join('') || '<li>No agents reporting</li>';
}

const eventList = document.getElementById('event-list');
function pushEvent(ev) {
  const li = document.createElement('li');
  const t = new Date(ev.ts || Date.now()).toLocaleTimeString();
  li.innerHTML = `<b>${ev.kind}</b> — ${ev.detail || ''} <span style="opacity:.6">(${t})</span>`;
  eventList.prepend(li);
  while (eventList.children.length > 60) eventList.removeChild(eventList.lastChild);
}

// ------------------------------------------------------------------ websocket (live event feed only)

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws`);
  const dot = document.getElementById('conn-status');
  ws.onopen = () => dot.classList.add('up');
  ws.onclose = () => { dot.classList.remove('up'); setTimeout(connectWS, 2000); };
  ws.onerror = () => ws.close();
  ws.onmessage = (msg) => {
    try { pushEvent(JSON.parse(msg.data)); } catch { /* ignore malformed */ }
  };
}

// ------------------------------------------------------------------ controls

async function post(path, body) {
  await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  fetchState();
}
document.getElementById('btn-start').onclick = () => post('/api/control/start');
document.getElementById('btn-pause').onclick = () => post('/api/control/pause');
document.getElementById('btn-resume').onclick = () => post('/api/control/resume');
document.getElementById('btn-reset').onclick = () => { if (confirm('Reset the whole simulation?')) post('/api/control/reset'); };
document.getElementById('btn-incident').onclick = () => post('/api/control/incident', {});
document.getElementById('traffic-level').onchange = (e) => post('/api/control/traffic-level', { level: e.target.value });

// ------------------------------------------------------------------ boot

fetchState();
connectWS();
setInterval(fetchState, 2000);
