// Plain JS, no framework, no build step — mirrors Traffic Sim's app.js shape.
// fetchState() polls every 2s (the authoritative recovery path); the WebSocket
// is only for the live event feed, and losing it never loses the dashboard.

let currentLevel = 'low';
let eventLog = [];
const EVENT_LOG_CAP = 100;

function $(sel) { return document.querySelector(sel); }
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}

// ---------------------------------------------------------------- polling

async function fetchState() {
  try {
    const snap = await api('/api/state');
    setConn(true);
    renderMongoBanner(snap.mongoError);
    if (snap.mongoError) return;
    renderDashboard(snap);
    renderHotelGrid(snap.hotels || []);
    renderAgents(snap.agents || []);
    renderMongoPanel(snap.mongo || {}, snap.control?.topology);
    currentLevel = snap.control?.level || currentLevel;
    highlightLevel(currentLevel);
  } catch (err) {
    setConn(false);
    renderMongoBanner('cannot reach the hotelsim server: ' + err.message);
  }
}

async function fetchQueries() {
  try {
    const d = await api('/api/queries?limit=25');
    renderQueries(d.samples || []);
  } catch (e) { /* keep last render */ }
}

async function fetchEventsBackfill() {
  try {
    const d = await api('/api/events?limit=50');
    eventLog = (d.events || []).reverse();
    renderEvents();
  } catch (e) { /* keep last render */ }
}

function setConn(ok) {
  const dot = $('#conn-status');
  dot.className = 'dot ' + (ok ? 'ok' : 'bad');
}

function renderMongoBanner(msg) {
  const b = $('#mongo-error-banner');
  if (msg) {
    b.textContent = '⚠ ' + msg;
    b.classList.remove('hidden');
  } else {
    b.classList.add('hidden');
  }
}

// --------------------------------------------------------------- rendering

function renderDashboard(snap) {
  const s = snap.summary || {};
  const c = snap.counters || s.counters || {};
  $('#topology-badge').textContent = (snap.control?.topology || 'unknown') + ' · ' + (s.control?.bookingPath || '');
  const tiles = [
    ['Reservations', c.reservationsTotal ?? 0],
    ['Cancellations', c.cancellationsTotal ?? 0],
    ['Modifications', c.modificationsTotal ?? 0],
    ['Check-ins', c.checkInsTotal ?? 0],
    ['Check-outs', c.checkOutsTotal ?? 0],
    ['No-shows', c.noShowsTotal ?? 0],
    ['Searches', c.searchesTotal ?? 0],
    ['Sold out', c.soldOut ?? 0],
    ['Duplicates rejected', c.duplicatesRejected ?? 0],
    ['Write conflicts', c.writeConflicts ?? 0],
    ['Compensations', c.compensations ?? 0],
    ['Uptime (s)', snap.uptimeSeconds ?? 0],
  ];
  const grid = $('#stat-grid');
  grid.innerHTML = '';
  for (const [label, val] of tiles) {
    const t = el('div', 'stat-tile');
    t.appendChild(el('div', 'v', String(val)));
    t.appendChild(el('div', 'l', label));
    grid.appendChild(t);
  }
}

function renderHotelGrid(hotels) {
  const grid = $('#hotel-grid');
  grid.innerHTML = '';
  for (const h of hotels) {
    const tile = el('div', 'hotel-tile ' + (h.occupancyClass || ''));
    if (!h.inScope) tile.classList.add('outofscope');
    tile.onclick = () => showHotelDetail(h.id);
    tile.appendChild(el('span', 'name', h.name));
    const meta = el('div', 'meta');
    meta.appendChild(el('span', 'badge-letter', h.badge || '?'));
    meta.appendChild(el('span', '', (h.occupancyPct ?? 0) + '%'));
    tile.appendChild(meta);
    grid.appendChild(tile);
  }
}

function renderAgents(agents) {
  const list = $('#agent-list');
  list.innerHTML = '';
  for (const a of agents.sort((x, y) => x.name.localeCompare(y.name))) {
    const row = el('div', 'agent-row' + (a.stale ? ' stale' : ''));
    const left = el('span');
    const dotClass = a.stale ? 'error' : (a.status === 'idle' ? 'idle' : 'ok');
    left.innerHTML = `<span class="status-dot ${dotClass}"></span>${a.name}`;
    row.appendChild(left);
    row.appendChild(el('span', '', `${a.events || 0} ev / ${a.errors || 0} err${a.stale ? ' (stale)' : ''}`));
    list.appendChild(row);
  }
}

function renderMongoPanel(m, topology) {
  const div = $('#mongo-detail');
  div.innerHTML = '';
  const rows = [
    ['Topology', topology || m.topology],
    ['Version', m.version],
    ['Connections', m.connections ? JSON.stringify(m.connections.current) + ' current' : '—'],
  ];
  if (m.dbStats) {
    rows.push(['Collections', m.dbStats.collections]);
    rows.push(['Objects', m.dbStats.objects]);
    rows.push(['Data size', m.dbStats.dataSize]);
  }
  if (m.replicaSet) {
    for (const mem of m.replicaSet.members || []) {
      rows.push([mem.name, `${mem.stateStr}${mem.self ? ' (self)' : ''}`]);
    }
  }
  if (m.sharded) {
    for (const sh of m.sharded.shards || []) {
      rows.push([`Shard ${sh.id}`, JSON.stringify(sh.docCounts || {})]);
    }
  }
  for (const [k, v] of rows) {
    if (v == null) continue;
    const row = el('div', 'kv');
    row.appendChild(el('span', 'k', k));
    row.appendChild(el('span', '', String(v)));
    div.appendChild(row);
  }
}

function renderQueries(samples) {
  const list = $('#query-list');
  list.innerHTML = '';
  for (const q of samples) {
    const row = el('div', 'query-row ' + (q.targeted ? 'targeted' : 'scatter'));
    const left = el('span', '', `${q.agent} · ${q.collection}.${q.op} ${q.targeted ? '(targeted)' : '(scatter-gather)'}`);
    row.appendChild(left);
    row.appendChild(el('span', '', q.reason || ''));
    list.appendChild(row);
  }
}

function renderEvents() {
  const list = $('#event-list');
  list.innerHTML = '';
  for (const ev of eventLog.slice(0, 50)) {
    const row = el('div', 'event-row');
    row.appendChild(el('span', 'kind', ev.kind));
    row.appendChild(el('span', '', `${ev.hotelName || ev.hotelId || ''} · ${new Date(ev.at).toLocaleTimeString()}`));
    list.appendChild(row);
  }
}

function pushEvent(ev) {
  eventLog.unshift(ev);
  if (eventLog.length > EVENT_LOG_CAP) eventLog.length = EVENT_LOG_CAP;
  renderEvents();
}

// ------------------------------------------------------------------- detail

async function showHotelDetail(id) {
  try {
    const d = await api(`/api/hotels/${id}`);
    const body = $('#modal-body');
    body.innerHTML = '';
    body.appendChild(el('h2', '', `${d.hotel.name} (${d.hotel.hotelId})`));
    body.appendChild(el('p', 'hint', `${d.hotel.region} · ${d.hotel.sizeTier} · ${d.hotel.category} · ${d.hotel.totalRooms} rooms`));
    const kvs = [
      ['Occupancy today', `${d.stats.occupancyPct}% (${d.stats.badge})`],
      ['In-house', d.stats.inHouse], ['Arrivals today', d.stats.arrivalsToday], ['Departures today', d.stats.departuresToday],
      ['Query routing', `${d.query.targeted ? 'targeted' : 'scatter-gather'} — ${d.query.reason}`],
    ];
    for (const [k, v] of kvs) {
      const row = el('div', 'kv');
      row.appendChild(el('span', 'k', k));
      row.appendChild(el('span', '', String(v)));
      body.appendChild(row);
    }
    body.appendChild(el('h3', '', 'Recent reservations'));
    for (const r of (d.recentReservations || []).slice(0, 10)) {
      const row = el('div', 'kv');
      row.appendChild(el('span', '', r.reservationId));
      const link = el('a', '', r.status);
      link.href = '#';
      link.onclick = (e) => { e.preventDefault(); showReservationDetail(r.reservationId, r.hotelId, r.checkInDate); };
      row.appendChild(link);
      body.appendChild(row);
    }
    openModal();
  } catch (e) { /* ignore */ }
}

async function showReservationDetail(id, hotelId, checkInDate) {
  try {
    const qs = hotelId ? `?hotelId=${encodeURIComponent(hotelId)}&checkInDate=${encodeURIComponent((checkInDate || '').slice(0, 10))}` : '';
    const d = await api(`/api/reservations/${id}${qs}`);
    const body = $('#modal-body');
    body.innerHTML = '';
    body.appendChild(el('h2', '', `Reservation ${d.reservation.reservationId}`));
    const kvs = [
      ['Hotel', d.hotel.name], ['Guest', d.reservation.guestName], ['Status', d.reservation.status],
      ['Check-in', d.reservation.checkInDate], ['Check-out', d.reservation.checkOutDate],
      ['Total', `${d.reservation.totalAmount} ${d.reservation.currency}`],
      ['Query routing', `${d.query.targeted ? 'targeted' : 'scatter-gather'} (${d.query.shardsTouched} shard(s), ${d.query.durationMs?.toFixed?.(2)}ms) — ${d.query.reason}`],
    ];
    for (const [k, v] of kvs) {
      const row = el('div', 'kv');
      row.appendChild(el('span', 'k', k));
      row.appendChild(el('span', '', String(v)));
      body.appendChild(row);
    }
    body.appendChild(el('h3', '', 'History'));
    for (const h of d.history || []) {
      const row = el('div', 'kv');
      row.appendChild(el('span', '', h.action));
      row.appendChild(el('span', '', new Date(h.at).toLocaleString()));
      body.appendChild(row);
    }
    openModal();
  } catch (e) { /* ignore */ }
}

function openModal() { $('#detail-modal').classList.remove('hidden'); }
function closeModal() { $('#detail-modal').classList.add('hidden'); }

// -------------------------------------------------------------------- ws

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/ws`);
  ws.onmessage = (msg) => {
    try {
      const ev = JSON.parse(msg.data);
      if (ev.kind) pushEvent(ev);
    } catch (e) { /* ignore malformed */ }
  };
  ws.onclose = () => setTimeout(connectWS, 2000);
  ws.onerror = () => ws.close();
}

// -------------------------------------------------------------- controls

function highlightLevel(level) {
  for (const b of document.querySelectorAll('.controls button[data-level]')) {
    b.classList.toggle('active', b.dataset.level === level);
  }
}

async function setLevel(level) {
  await fetch('/api/control/level', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ level }) });
  currentLevel = level;
  highlightLevel(level);
}

document.addEventListener('DOMContentLoaded', () => {
  for (const b of document.querySelectorAll('.controls button[data-level]')) {
    b.addEventListener('click', () => setLevel(b.dataset.level));
  }
  $('#btn-reset').addEventListener('click', async () => {
    if (!confirm('Reset wipes all simulated hotels/reservations and starts fresh. Continue?')) return;
    await fetch('/api/control/reset', { method: 'POST' });
    fetchState(); fetchEventsBackfill(); fetchQueries();
  });
  $('#modal-close').addEventListener('click', closeModal);
  $('#detail-modal').addEventListener('click', (e) => { if (e.target.id === 'detail-modal') closeModal(); });

  fetchState();
  fetchEventsBackfill();
  fetchQueries();
  connectWS();
  setInterval(fetchState, 2000);
  setInterval(fetchQueries, 5000);
});
