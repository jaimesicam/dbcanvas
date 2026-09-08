// Plain JS, no framework, no build step — mirrors Airline Sim's/Hotel Sim's app.js shape.
// fetchState() polls every 2s (the authoritative recovery path); the WebSocket is
// only for the live event feed, and losing it never loses the dashboard.

let currentLevel = 'low';
let eventLog = [];
const EVENT_LOG_CAP = 100;

let fleetOffset = 0;
const FLEET_PAGE_SIZE = 50;

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
    renderErrorBanner(snap.error);
    if (snap.error) return;
    renderDashboard(snap);
    renderLocationGrid(snap.locations || []);
    renderFleetSummary(snap.fleet || {});
    renderAgents(snap.agents || []);
    renderPgPanel(snap.diag || {}, snap.control?.kind);
    currentLevel = snap.control?.level || currentLevel;
    highlightLevel(currentLevel);
  } catch (err) {
    setConn(false);
    renderErrorBanner('cannot reach the carsim server: ' + err.message);
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

async function fetchFleetPage() {
  const search = $('#fleet-search').value.trim();
  const status = $('#fleet-status-filter').value;
  const qs = new URLSearchParams({ limit: FLEET_PAGE_SIZE, offset: fleetOffset });
  if (search) qs.set('search', search);
  if (status) qs.set('status', status);
  try {
    const d = await api('/api/vehicles?' + qs.toString());
    renderFleetTable(d);
  } catch (e) { /* keep last render */ }
}

function setConn(ok) {
  const dot = $('#conn-status');
  dot.className = 'dot ' + (ok ? 'ok' : 'bad');
}

function renderErrorBanner(msg) {
  const b = $('#error-banner');
  if (msg) {
    b.textContent = '⚠ ' + msg;
    b.classList.remove('hidden');
  } else {
    b.classList.add('hidden');
  }
}

// --------------------------------------------------------------- rendering

function renderDashboard(snap) {
  const c = snap.summary || {};
  $('#kind-badge').textContent = snap.control?.kind || 'unknown';
  const tiles = [
    ['Reservations', c.reservationsTotal ?? 0],
    ['Cancellations', c.cancellationsTotal ?? 0],
    ['Modifications', c.modificationsTotal ?? 0],
    ['Check-outs', c.checkOutsTotal ?? 0],
    ['Check-ins', c.checkInsTotal ?? 0],
    ['No-shows', c.noShowsTotal ?? 0],
    ['Searches', c.searchesTotal ?? 0],
    ['Sold out', c.soldOut ?? 0],
    ['Duplicates rejected', c.duplicatesRejected ?? 0],
    ['Active renters', c.activeRenters ?? 0],
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

function renderLocationGrid(locations) {
  const grid = $('#location-grid');
  grid.innerHTML = '';
  for (const l of locations) {
    const tile = el('div', 'location-tile ' + (l.utilizationClass || ''));
    if (!l.inScope) tile.classList.add('outofscope');
    tile.onclick = () => showLocationDetail(l.id);
    tile.appendChild(el('span', 'name', `${l.code} ${l.name}`));
    const meta = el('div', 'meta');
    meta.appendChild(el('span', 'badge-letter', l.badge || '?'));
    meta.appendChild(el('span', '', (l.utilizationPct ?? 0) + '%'));
    tile.appendChild(meta);
    grid.appendChild(tile);
  }
}

function renderFleetSummary(fleet) {
  const byStatus = fleet.byStatus || {};
  const parts = Object.keys(byStatus).sort().map((k) => `${k}: ${byStatus[k]}`);
  $('#fleet-summary').textContent = `${fleet.total ?? 0} vehicles — ${parts.join(' · ')}`;
}

function renderFleetTable(d) {
  const div = $('#fleet-table');
  div.innerHTML = '';
  const table = el('table');
  const thead = el('thead');
  const hrow = el('tr');
  for (const h of ['VIN', 'Make/Model', 'Class', 'Home', 'Current location', 'Status']) hrow.appendChild(el('th', '', h));
  thead.appendChild(hrow);
  table.appendChild(thead);
  const tbody = el('tbody');
  for (const v of d.vehicles || []) {
    const row = el('tr');
    row.appendChild(el('td', '', v.vin));
    row.appendChild(el('td', '', v.makeModel));
    row.appendChild(el('td', '', v.classCode));
    row.appendChild(el('td', '', v.homeLocationId));
    const locCell = el('td');
    const link = el('a', '', v.currentLocationId);
    link.href = '#';
    link.onclick = (e) => { e.preventDefault(); showLocationDetail(v.currentLocationId); };
    locCell.appendChild(link);
    row.appendChild(locCell);
    const statusCell = el('td');
    statusCell.appendChild(el('span', 'status-pill ' + v.status, v.status));
    row.appendChild(statusCell);
    tbody.appendChild(row);
  }
  table.appendChild(tbody);
  div.appendChild(table);

  const pager = $('#fleet-pager');
  pager.innerHTML = '';
  const total = d.total ?? 0;
  const from = total === 0 ? 0 : fleetOffset + 1;
  const to = Math.min(fleetOffset + (d.vehicles || []).length, total);
  pager.appendChild(el('span', '', `${from}-${to} of ${total}`));
  const btns = el('div');
  const prev = el('button', '', '← prev');
  prev.disabled = fleetOffset === 0;
  prev.onclick = () => { fleetOffset = Math.max(0, fleetOffset - FLEET_PAGE_SIZE); fetchFleetPage(); };
  const next = el('button', '', 'next →');
  next.disabled = fleetOffset + FLEET_PAGE_SIZE >= total;
  next.onclick = () => { fleetOffset += FLEET_PAGE_SIZE; fetchFleetPage(); };
  btns.appendChild(prev);
  btns.appendChild(next);
  pager.appendChild(btns);
}

function renderAgents(agents) {
  const list = $('#agent-list');
  list.innerHTML = '';
  const now = Date.now();
  for (const a of [...agents].sort((x, y) => x.name.localeCompare(y.name))) {
    const stale = now - new Date(a.lastTick).getTime() > 30000;
    const row = el('div', 'agent-row' + (stale ? ' stale' : ''));
    const left = el('span');
    const dotClass = stale ? 'error' : (a.status === 'idle' ? 'idle' : 'ok');
    left.innerHTML = `<span class="status-dot ${dotClass}"></span>${a.name}`;
    row.appendChild(left);
    row.appendChild(el('span', '', `${a.detail || ''}${stale ? ' (stale)' : ''}`));
    list.appendChild(row);
  }
}

function renderPgPanel(m, kind) {
  const div = $('#pg-detail');
  div.innerHTML = '';
  const rows = [
    ['Target kind', kind || m.kind],
    ['Version', m.serverVersion],
    ['Active connections', m.activeConns],
    ['Uptime', m.uptime],
  ];
  if (m.patroniStatus) {
    for (const [k, v] of Object.entries(m.patroniStatus)) rows.push([k, v]);
  }
  if (m.repmgrStatus) {
    const keys = Object.keys(m.repmgrStatus);
    if (keys.length === 0) {
      rows.push(['repmgr status', '(repmgr schema not visible on this connection)']);
    } else {
      for (const [k, v] of Object.entries(m.repmgrStatus)) rows.push([k, v]);
    }
  }
  if (m.spockStatus) {
    const keys = Object.keys(m.spockStatus);
    if (keys.length === 0) {
      rows.push(['Spock subscriptions', '(none reported by this node)']);
    } else {
      for (const [k, v] of Object.entries(m.spockStatus)) rows.push(['sub: ' + k, v]);
    }
  }
  for (const [k, v] of rows) {
    if (v == null || v === '') continue;
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
    const row = el('div', 'query-row ' + (q.kind === 'targeted' ? 'targeted' : 'scatter'));
    const left = el('span', '', `${q.kind} · ${(q.durationMs ?? 0).toFixed(2)}ms${q.indexUsed ? ' · ' + q.indexUsed : ''}`);
    row.appendChild(left);
    row.appendChild(el('span', '', q.rowsExamined != null ? `${q.rowsExamined} rows examined` : ''));
    list.appendChild(row);
  }
}

function renderEvents() {
  const list = $('#event-list');
  list.innerHTML = '';
  for (const ev of eventLog.slice(0, 50)) {
    const row = el('div', 'event-row');
    row.appendChild(el('span', 'kind', ev.kind));
    row.appendChild(el('span', '', `${ev.locationId || ''} · ${new Date(ev.at).toLocaleTimeString()}`));
    list.appendChild(row);
  }
}

function pushEvent(ev) {
  eventLog.unshift(ev);
  if (eventLog.length > EVENT_LOG_CAP) eventLog.length = EVENT_LOG_CAP;
  renderEvents();
}

// ------------------------------------------------------------------- detail

async function showLocationDetail(id) {
  try {
    const d = await api(`/api/locations/${id}`);
    const body = $('#modal-body');
    body.innerHTML = '';
    body.appendChild(el('h2', '', `${d.location.code} ${d.location.name} (${d.location.locationId})`));
    body.appendChild(el('p', 'hint', `${d.location.region} · ${d.location.sizeTier} · ${d.location.operationalStatus} · utilization ${(d.location.currentUtilization * 100).toFixed(0)}%`));
    body.appendChild(el('p', 'hint', `Pooled vehicles: ${(d.vehiclePool || []).join(', ')}`));
    body.appendChild(el('h3', '', 'Recent reservations'));
    for (const r of (d.recentReservations || []).slice(0, 10)) {
      const row = el('div', 'kv');
      row.appendChild(el('span', '', `${r.id} · ${r.renterName}`));
      const link = el('a', '', r.status);
      link.href = '#';
      link.onclick = (e) => { e.preventDefault(); showReservationDetail(r.id, d.location.locationId, (r.pickupDate || '').slice(0, 10)); };
      row.appendChild(link);
      body.appendChild(row);
    }
    openModal();
  } catch (e) { /* ignore */ }
}

async function showReservationDetail(id, locationId, pickupDate) {
  try {
    const qs = locationId ? `?locationId=${encodeURIComponent(locationId)}&pickupDate=${encodeURIComponent(pickupDate || '')}` : '';
    const d = await api(`/api/reservations/${id}${qs}`);
    const body = $('#modal-body');
    body.innerHTML = '';
    body.appendChild(el('h2', '', `Reservation ${d.reservation.reservationId}`));
    const kvs = [
      ['Pickup', d.reservation.pickupLocationId], ['Drop-off', d.reservation.dropoffLocationId],
      ['Renter', d.reservation.renterName], ['Status', d.reservation.status],
      ['Pickup date', d.reservation.pickupDate], ['Return date', d.reservation.returnDate],
      ['Vehicle', d.reservation.vehicleVin || '(not checked out yet)'],
      ['Total', `${d.reservation.rateTotal} ${d.reservation.currency}`],
      ['Query routing', `${d.query.targeted ? 'targeted' : 'primary-key-only'} (${d.query.durationMs?.toFixed?.(2)}ms) — ${d.query.reason}`],
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
    if (!confirm('Reset wipes all simulated locations/vehicles/reservations and starts fresh. Continue?')) return;
    await fetch('/api/control/reset', { method: 'POST' });
    fetchState(); fetchEventsBackfill(); fetchQueries(); fetchFleetPage();
  });
  $('#modal-close').addEventListener('click', closeModal);
  $('#detail-modal').addEventListener('click', (e) => { if (e.target.id === 'detail-modal') closeModal(); });
  $('#fleet-search').addEventListener('input', debounce(() => { fleetOffset = 0; fetchFleetPage(); }, 300));
  $('#fleet-status-filter').addEventListener('change', () => { fleetOffset = 0; fetchFleetPage(); });

  fetchState();
  fetchEventsBackfill();
  fetchQueries();
  fetchFleetPage();
  connectWS();
  setInterval(fetchState, 2000);
  setInterval(fetchQueries, 5000);
});

function debounce(fn, ms) {
  let t;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
}
