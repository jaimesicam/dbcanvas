// Performance Sim dashboard. Polls api/state once a second and draws it; every
// control is one call to the sim's API. URLs are relative so the page works the
// same opened directly and inside DBCanvas's browser window, under a prefix.
(() => {
'use strict';
const $ = (s, el = document) => el.querySelector(s);
const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const fmtS = (s) => s < 1 ? (s > 0 ? s.toFixed(1) + ' s' : '0 s') : s < 60 ? Math.round(s) + ' s' : Math.floor(s / 60) + 'm ' + String(Math.round(s % 60)).padStart(2, '0') + 's';
const fmtClock = (s) => { s = Math.max(0, Math.round(s)); return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0'); };
const fmtN = (n) => n >= 1e6 ? +(n / 1e6).toFixed(1) + 'M' : n >= 10000 ? (n / 1000).toFixed(1) + 'k' : Math.round(n).toLocaleString('en-US');
const fmtMs = (ms) => ms >= 1000 ? (ms / 1000).toFixed(1) + ' s' : ms >= 100 ? Math.round(ms) + ' ms' : ms.toFixed(1) + ' ms';
const fmtBytes = (b) => b >= 1 << 30 ? (b / (1 << 30)).toFixed(1) + ' GB' : Math.max(0, Math.round(b / (1 << 20))) + ' MB';
const fmtUnit = (v, unit) => unit === 'rows' ? fmtN(v) : unit === 's' ? fmtS(v) : unit ? v + ' ' + unit : String(v);
const BAND = ['--band-a', '--band-b', '--band-c', '--band-d'];
const SERIES = ['--brand', '--info', '--teal', '--warning', '--band-b'];
const HISTORY = 900;

// ---------- state ----------
let S = null;                 // last state from the sim
let points = [];              // the timeline, kept here and extended each poll
let lastT = 0;
let lastOk = 0;
const ui = { open: { lag: true }, expanded: {}, draft: {}, tab: 'sessions', win: 300, paused: false, frozenT: null, hoverT: null, sessions: [], busy: {} };

async function api(method, path, body) {
  const r = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

async function poll() {
  try {
    const s = await api('GET', 'api/state?since=' + lastT);
    if (s.points && s.points.length) {
      points = points.concat(s.points);
      if (points.length > HISTORY) points = points.slice(points.length - HISTORY);
      lastT = points[points.length - 1].t;
    }
    const first = !S;
    S = s;
    lastOk = Date.now();
    if (first) buildCharts();
    render();
  } catch (e) {
    $('#fresh').textContent = 'Not reachable: ' + e.message;
  }
}

// ---------- helpers on state ----------
const topo = () => (S && S.info.topology) || 'standalone';
const galera = () => topo() === 'galera';
const writerID = () => (S && S.info.writer) || (S && S.members[0] && S.members[0].id) || '';
const others = () => (S ? S.members.filter((m) => m.id !== writerID()) : []);
const lagIDs = () => galera() ? S.members.map((m) => m.id) : others().map((m) => m.id);
const lagOf = (p) => { let v = 0; for (const id of lagIDs()) v = Math.max(v, (galera() ? p.recvq?.[id] : p.lag?.[id]) || 0); return v; };
const activeOf = (id) => (S.active || []).find((a) => a.id === id);
const dialOf = (id) => S.dials.find((d) => d.id === id);
const lastPoint = () => points[points.length - 1];

function draft(d) {
  if (!ui.draft[d.id]) {
    const saved = (S.params || {})[d.id] || {};
    const params = {};
    for (const p of d.params) params[p.key] = saved[p.key] ?? p.def;
    const memberIDs = d.members === 'all' ? S.members.map((m) => m.id) : others().map((m) => m.id);
    let targets = ((S.targets || {})[d.id] || []).filter((t) => memberIDs.includes(t));
    if (!targets.length && memberIDs.length) targets = [memberIDs[memberIDs.length - 1]];
    ui.draft[d.id] = { params, targets, table: saved.table || '' };
  }
  const dr = ui.draft[d.id];
  const tables = tablesFor(d);
  if (d.needsTable && !tables.some((t) => t.name === dr.table)) dr.table = tables.length ? tables[tables.length - 1].name : '';
  return dr;
}
function tablesFor(d) {
  const all = S.datasets || [];
  if (d.needsTable === 'pk') return all.filter((t) => t.pk);
  if (d.needsTable === 'nopk') return all.filter((t) => !t.pk);
  return all;
}

// ---------- header and KPIs ----------
function renderHeader() {
  $('#targetName').textContent = S.info.name || '…';
  const t = { standalone: 'standalone', async: 'async replication', gr: 'Group Replication', galera: 'Galera' }[topo()] || topo();
  $('#targetKind').textContent = S.ready ? `${S.info.flavor === 'percona' ? 'Percona Server' : S.info.flavor === 'mariadb' ? 'MariaDB' : 'MySQL'} ${S.info.version} · ${t}` : 'connecting';
  const conn = $('#conn');
  conn.hidden = S.ready;
  if (!S.ready) conn.textContent = `Connecting to ${S.info.name || 'the linked database'}${S.initErr ? ': ' + S.initErr : '…'}`;
  $('#recLabel').textContent = S.recording ? 'Recording ' + fmtClock(S.now - S.recording) : 'Record run';
  const n = (S.active || []).length;
  $('#activeCount').textContent = n + ' active';
  $('#activeCount').className = 'chip ' + (n ? 'warn' : '');
}

function spark(vals, color) {
  if (vals.length < 2) return '';
  const max = Math.max(...vals, 1e-9), min = Math.min(...vals, 0);
  const pts = vals.map((v, i) => `${(i / (vals.length - 1) * 84).toFixed(1)},${(24 - ((v - min) / (max - min || 1)) * 22).toFixed(1)}`).join(' ');
  return `<svg class="spark" viewBox="0 0 84 26" aria-hidden="true"><polyline fill="none" stroke="${color}" stroke-width="1.5" stroke-linejoin="round" points="${pts}"/></svg>`;
}
function renderKpis() {
  const r = lastPoint();
  if (!r) { $('#kpis').innerHTML = ''; return; }
  const b = S.baseline || { wq: r.wq, p99: r.p99, lag: lagOf(r), recvq: lagOf(r) };
  const last = points.slice(-60);
  const g = galera();
  const ids = lagIDs();
  const worst = ids.reduce((a, x) => ((g ? r.recvq?.[x] : r.lag?.[x]) || 0) > ((g ? r.recvq?.[a] : r.lag?.[a]) || 0) ? x : a, ids[0] || '');
  const held = (S.active || []).filter((a) => a.hold);
  const nearest = held.length ? held.reduce((a, x) => (x.start + x.hold < a.start + a.hold ? x : a)) : null;
  const pct = (v, base) => base ? ((v - base) / base) * 100 : 0;
  const delta = (d, goodUp, text) => {
    const bad = goodUp ? d < -10 : d > 10;
    const col = Math.abs(d) < 10 ? 'var(--muted)' : bad ? 'var(--danger)' : 'var(--success)';
    return `<span class="delta" style="color:${col}">${text}</span>`;
  };
  const lag = lagOf(r), wqD = pct(r.wq, b.wq), p99X = r.p99 / (b.p99 || 1);
  const noReplicas = !ids.length;
  const k = [
    g ? { cls: r.fc > 0.5 ? 'alert' : lag > 40 ? 'warnk' : '', label: 'Flow control', val: `${Math.round(r.fc * 100)}<small>% paused</small>`,
      sub: `<span class="delta muted">largest receive queue ${Math.round(lag)}${worst ? ' on ' + esc(worst) : ''}</span>`, spark: spark(last.map((x) => x.fc), css('--warning')) }
      : { cls: lag > 30 ? 'alert' : lag > 5 ? 'warnk' : '', label: 'Max ' + (S.info.lagTitle || 'lag').toLowerCase(),
        val: noReplicas ? '—' : `${lag < 10 ? lag.toFixed(1) : Math.round(lag)}<small>s</small>`,
        sub: noReplicas ? '<span class="delta muted">no replicas</span>' : `<span class="delta muted">${esc(worst)} · baseline ${(b.lag || 0).toFixed(1)} s</span>`,
        spark: spark(last.map(lagOf), css('--warning')) },
    { cls: wqD < -50 ? 'alert' : '', label: 'Write throughput', val: `${fmtN(r.wq)}<small>/s</small>`,
      sub: delta(wqD, true, `${wqD >= 0 ? '+' : '−'}${Math.abs(wqD).toFixed(0)}% vs baseline ${fmtN(b.wq)}/s`), spark: spark(last.map((x) => x.wq), css('--brand')) },
    { cls: p99X > 5 ? 'alert' : p99X > 2 ? 'warnk' : '', label: 'App p99 latency', val: fmtMs(r.p99).replace(/ (\w+)$/, '<small>$1</small>'),
      sub: delta((p99X - 1) * 100, false, `×${p99X.toFixed(1)} baseline ${fmtMs(b.p99)}`), spark: spark(last.map((x) => x.p99), css('--info')) },
    { cls: r.errs > 1 ? 'alert' : '', label: 'App errors', val: `${r.errs < 10 ? r.errs.toFixed(1) : Math.round(r.errs)}<small>/s</small>`,
      sub: `<span class="delta muted">${last.some((x) => x.errs > 0) ? 'failed statements in the last minute' : 'none in the last minute'}</span>`, spark: spark(last.map((x) => x.errs), css('--danger')) },
    { cls: held.length ? 'warnk' : '', label: 'Locks, pauses and limits held', val: `${held.length}`,
      sub: nearest ? `<span class="delta" style="color:var(--warning)">${esc(nearest.label)} · releases in ${fmtClock(nearest.start + nearest.hold - S.now)}</span>` : '<span class="delta muted">nothing held</span>', spark: '' },
  ];
  $('#kpis').innerHTML = k.map((x) => `<div class="card kpi ${x.cls}"><div class="label">${x.label}</div><div class="val">${x.val}</div>${x.sub}${x.spark}</div>`).join('');
}

// ---------- topology ----------
function badge(x, y, color, frac, kind) {
  const C = 2 * Math.PI * 13;
  const icon = kind === 'net' ? `<path d="M-7 1 h3 l2 -5 l3 9 l2 -6 l1.5 2 h2.5" fill="none" stroke="${color}" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round"/>`
    : kind === 'disk' ? `<ellipse cx="0" cy="-4" rx="6" ry="2.4" fill="none" stroke="${color}" stroke-width="1.5"/><path d="M-6 -4 v8 a6 2.4 0 0 0 12 0 v-8" fill="none" stroke="${color}" stroke-width="1.5"/>`
    : kind === 'gear' ? `<circle r="4" fill="none" stroke="${color}" stroke-width="1.6"/><path d="M0 -7 v3 M0 4 v3 M-7 0 h3 M4 0 h3" stroke="${color}" stroke-width="1.6"/>`
    : `<rect x="-5" y="-2" width="10" height="8" rx="1.5" fill="${color}"/><path d="M-3 -2 v-2.5 a3 3 0 0 1 6 0 v2.5" fill="none" stroke="${color}" stroke-width="1.6"/>`;
  return `<g transform="translate(${x},${y})"><circle r="13" fill="var(--surface)" stroke="var(--track)" stroke-width="3"/>
    <circle r="13" fill="none" stroke="${color}" stroke-width="3" stroke-dasharray="${(C * frac).toFixed(1)} ${C.toFixed(1)}" transform="rotate(-90)" stroke-linecap="round"/>${icon}</g>`;
}
const BADGE_KIND = { ftwrl: 'lock', mdl: 'lock', stopapplier: 'lock', delay: 'gear', singlethread: 'gear' };
function renderTopo() {
  const ms = S.members, g = galera(), W = 280;
  const H = ms.length > 4 ? 240 : 220;
  const chipCol = { ok: 'var(--success)', warn: 'var(--warning)', bad: 'var(--danger)', neutral: 'var(--neutral)' };
  const pos = {};
  const wid = writerID();
  if (g) {
    ms.forEach((m, i) => { const a = -Math.PI / 2 + i * 2 * Math.PI / ms.length; pos[m.id] = [W / 2 + Math.cos(a) * 92, 112 + Math.sin(a) * 72]; });
  } else {
    pos[wid] = [W / 2, 42];
    const rest = others();
    rest.forEach((m, i) => { pos[m.id] = [rest.length === 1 ? W / 2 : 52 + i * ((W - 104) / (rest.length - 1)), 165]; });
  }
  let edges = '', nodes = '';
  if (g) {
    for (let i = 0; i < ms.length; i++) for (let j = i + 1; j < ms.length; j++) {
      const [x1, y1] = pos[ms[i].id], [x2, y2] = pos[ms[j].id];
      const cut = ms[i].evicted || ms[j].evicted || !ms[i].up || !ms[j].up;
      const fc = (lastPoint()?.fc || 0) > 0.5;
      edges += `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" stroke="${cut ? 'var(--track)' : fc ? 'var(--danger)' : 'var(--border)'}" stroke-width="2" ${cut || fc ? 'stroke-dasharray="5 4"' : ''}/>`;
    }
  } else if (pos[wid]) {
    const [sx, sy] = pos[wid];
    for (const m of others()) {
      const [x, y] = pos[m.id], c = chipCol[m.chip] || 'var(--border)', v = m.lag;
      const stopped = /stopped|disconnected|lock/.test(m.chipText);
      edges += `<line x1="${sx}" y1="${sy + 20}" x2="${x}" y2="${y - 22}" stroke="${c}" stroke-width="${v > 5 ? 3 : 2}" ${stopped ? 'stroke-dasharray="6 5"' : ''}/>
        <g transform="translate(${(sx + x) / 2},${(sy + y) / 2 + 2})"><rect x="-27" y="-10" width="54" height="20" rx="10" fill="var(--surface)" stroke="${c}"/>
        <text text-anchor="middle" y="4" font-size="11" font-weight="600" fill="${c}" font-family="var(--mono)">${v < 0 ? '—' : v < 10 ? v.toFixed(1) + 's' : Math.round(v) + 's'}</text></g>`;
    }
  }
  for (const m of ms) {
    if (!pos[m.id]) continue;
    const [x, y] = pos[m.id];
    const isW = m.id === wid && !g;
    const c = !m.up || m.evicted ? 'var(--neutral)' : isW ? 'var(--brand)' : chipCol[m.chip] || 'var(--border)';
    const label = m.id.length > 11 ? m.id.slice(0, 10) + '…' : m.id;
    nodes += `<g><title>${esc(m.id)} · ${esc(m.chipText)}</title><rect x="${x - 44}" y="${y - 20}" width="88" height="40" rx="10" fill="var(--surface)" stroke="${isW ? 'var(--brand)' : 'var(--border)'}" stroke-width="${isW ? 1.5 : 1}"/>
      <circle cx="${x - 32}" cy="${y - 4}" r="4" fill="${c}"/>
      <text x="${x - 24}" y="${y}" font-size="12" font-weight="600" fill="var(--ink)" font-family="var(--mono)">${esc(label)}</text>
      <text x="${x - 32}" y="${y + 13}" font-size="10" fill="var(--muted)">${esc(m.role)}${g ? ' · q ' + Math.round(m.recvq) : ''}</text></g>`;
    const holders = (S.active || []).filter((a) => (a.targets || []).includes(m.id) && BADGE_KIND[a.id] || ((a.targets || []).includes(m.id) && /^(net|disk)/.test(a.id))).slice(0, 3);
    holders.forEach((a, i) => {
      const frac = a.hold ? Math.max(0, 1 - (S.now - a.start) / a.hold) : 1;
      const kind = a.id.startsWith('net') ? 'net' : a.id.startsWith('disk') ? 'disk' : BADGE_KIND[a.id];
      nodes += badge(Math.min(x + 44, W - 15), y - 20 + i * 28, css(BAND[a.color % 4]), frac, kind);
    });
  }
  $('#topoSvg').innerHTML = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Cluster topology">${edges}${nodes}</svg>
    <div class="topo-legend meta">${g ? '<span><span class="dot" style="background:var(--success)"></span> queue &lt; 20</span><span><span class="dot" style="background:var(--warning)"></span> 20–99</span><span><span class="dot" style="background:var(--danger)"></span> flow control</span>'
      : '<span><span class="dot" style="background:var(--success)"></span> &lt; 5 s</span><span><span class="dot" style="background:var(--warning)"></span> 5–30 s</span><span><span class="dot" style="background:var(--danger)"></span> &gt; 30 s</span><span>dashed: applier stopped or blocked</span>'}<span>badges: lock, setting; the ring counts down to release</span></div>`;
  $('#topoSub').textContent = ms.length + ' member' + (ms.length === 1 ? '' : 's');
  $('#topoBanner').innerHTML = g && (lastPoint()?.fc || 0) > 0.5
    ? `<div class="banner"><span class="dot" style="background:var(--danger);margin-top:5px"></span><span><b>Cluster paused by flow control.</b> A node's receive queue passed gcs.fc_limit, so no node can commit until it drains.</span></div>` : '';
}

// ---------- impact ----------
function renderImpact() {
  const b = S.baseline;
  $('#baseAge').textContent = b ? `baseline set ${fmtS(S.now - b.t)} ago` : 'no baseline yet';
  const bands = (S.bands || []).slice().sort((a, c) => c.seq - a.seq).slice(0, 3);
  if (!bands.length || !b) { $('#impact').innerHTML = '<div class="empty">Turn on a stressor to see its effect here, measured against the baseline.</div>'; return; }
  const g = galera();
  const fmtLag = (v) => g ? Math.round(v) + '' : (v < 10 ? v.toFixed(1) : Math.round(v)) + ' s';
  $('#impact').innerHTML = bands.map((band) => {
    const on = band.end == null;
    const inBand = points.filter((p) => p.t >= band.start && (on || p.t <= band.end + 2));
    const after = on ? [] : points.filter((p) => p.t > band.end);
    const peak = { lag: 0, p99: 0, wq: Infinity };
    for (const p of inBand.concat(after.slice(0, 120))) { peak.lag = Math.max(peak.lag, lagOf(p)); peak.p99 = Math.max(peak.p99, p.p99); peak.wq = Math.min(peak.wq, p.wq); }
    const r = lastPoint();
    const now = on ? { lag: lagOf(r), wq: r.wq, p99: r.p99 } : { lag: peak.lag, wq: peak.wq === Infinity ? r.wq : peak.wq, p99: peak.p99 };
    let state;
    if (on) state = `<span class="chip warn">on · ${fmtClock(S.now - band.start)}</span>`;
    else {
      const baseLag = g ? (b.recvq || 0) : b.lag;
      const rec = after.find((p) => lagOf(p) <= (g ? Math.max(8, baseLag * 2) : Math.max(2, baseLag + 1.5)) && p.p99 < b.p99 * 2.2);
      state = rec ? `<span class="chip ok">recovered in ${fmtS(rec.t - band.end)}</span>` : `<span class="chip">recovering · ${fmtS(S.now - band.end)}</span>`;
    }
    const lagScale = g ? 250 : Math.max(60, peak.lag * 1.1);
    const lagLabel = g ? 'Receive queue' : (S.info.lagTitle || 'Lag');
    return `<div class="imp">
      <div class="imp-h"><div style="display:flex;gap:8px;align-items:flex-start;min-width:0"><span class="swatch" style="background:var(${BAND[band.color % 4]});margin-top:4px"></span><b>${esc(band.label)}</b></div>${state}</div>
      <div class="imp-rows">
        <div class="imp-row"><span>${lagLabel} <span class="from">${fmtLag(g ? b.recvq || 0 : b.lag)} →</span></span><span class="to" style="color:${now.lag > (g ? 60 : 5) ? 'var(--danger)' : 'var(--ink)'}">${fmtLag(now.lag)}${on ? '' : ' peak'}</span>
          <div class="imp-bar"><i style="width:${Math.min(100, now.lag / lagScale * 100)}%;background:var(--warning)"></i></div></div>
        <div class="imp-row"><span>Write QPS <span class="from">${fmtN(b.wq)} →</span></span><span class="to">${fmtN(now.wq)}${on ? '' : ' low'}</span>
          <div class="imp-bar"><i style="width:${Math.min(100, now.wq / ((b.wq || 1) * 1.3) * 100)}%;background:var(--brand)"></i></div></div>
        <div class="imp-row"><span>p99 latency <span class="from">${fmtMs(b.p99)} →</span></span><span class="to">${fmtMs(now.p99)}${on ? '' : ' peak'}</span>
          <div class="imp-bar"><i style="width:${Math.min(100, Math.log10(now.p99 / (b.p99 || 1) + 1) / 3 * 100)}%;background:var(--info)"></i></div></div>
      </div></div>`;
  }).join('');
}

// ---------- charts ----------
function chartDefs() {
  const g = galera();
  const ids = lagIDs();
  return [
    { id: 'lag', title: S.info.lagTitle || 'Replication lag', sub: S.info.lagSub || '', h: 130, area: true,
      series: ids.map((r, i) => ({ name: r, color: SERIES[i % SERIES.length], get: (x) => (g ? x.recvq?.[r] : x.lag?.[r]) || 0 })),
      fmt: (v) => g ? Math.round(v) + '' : fmtS(v), thresh: g ? { v: 100, label: 'gcs.fc_limit' } : null },
    { id: 'qps', title: 'Throughput', sub: 'the workload’s statements per second', h: 100,
      series: [{ name: 'writes', color: '--brand', get: (x) => x.wq }, { name: 'reads', color: '--info', get: (x) => x.rq, dashed: true }],
      fmt: (v) => fmtN(v) + '/s', axis: (v) => v >= 1000 ? +(v / 1000).toFixed(1) + 'k' : Math.round(v) + '' },
    { id: 'lat', title: 'Application latency', sub: 'the workload’s statements, log scale', h: 100, log: true,
      series: [{ name: 'p99', color: '--danger', get: (x) => x.p99 }, { name: 'p50', color: '--teal', get: (x) => x.p50 }], fmt: (v) => fmtMs(v) },
    { id: 'srv', title: 'Writer', sub: 'row lock waits, threads running, connections ÷ 10', h: 90,
      series: [{ name: 'lock waits', color: '--warning', get: (x) => x.lockw }, { name: 'threads', color: '--brand', get: (x) => x.thr }, { name: 'conns÷10', color: '--neutral', get: (x) => x.conns / 10, dashed: true }],
      fmt: (v) => v.toFixed(0) },
  ];
}
let chartKey = '';
function buildCharts() {
  const defs = chartDefs();
  chartKey = defs.map((c) => c.id + ':' + c.series.map((s) => s.name).join(',')).join('|');
  $('#charts').innerHTML = defs.map((c) => `<div class="chart" data-c="${c.id}"><div class="chart-h"><div><b>${esc(c.title)}</b> <span class="meta">${esc(c.sub)}</span></div><div class="legend" id="lg-${c.id}"></div></div><canvas id="cv-${c.id}" height="${c.h}"></canvas></div>`).join('');
  for (const cv of document.querySelectorAll('#charts canvas')) hookHover(cv);
}
function hookHover(cv) {
  cv.addEventListener('mousemove', (ev) => {
    const rect = cv.getBoundingClientRect(), g = geom(rect.width), tEnd = ui.frozenT ?? lastT;
    ui.hoverT = Math.min(tEnd, Math.round(tEnd - ui.win + ((ev.clientX - rect.left - g.l) / g.w) * ui.win));
    drawAll();
  });
  cv.addEventListener('mouseleave', () => { ui.hoverT = null; drawAll(); });
}
const geom = (cw) => ({ l: 58, w: cw - 66 });
function setupCanvas(cv, h) {
  const dpr = window.devicePixelRatio || 1, cw = cv.clientWidth;
  if (cv.width !== Math.round(cw * dpr) || cv.height !== Math.round(h * dpr)) { cv.width = Math.round(cw * dpr); cv.height = Math.round(h * dpr); cv.style.height = h + 'px'; }
  const ctx = cv.getContext('2d'); ctx.setTransform(dpr, 0, 0, dpr, 0, 0); ctx.clearRect(0, 0, cw, h);
  return { ctx, cw };
}
function roundRect(ctx, x, y, w, h, r) { ctx.beginPath(); ctx.moveTo(x + r, y); ctx.arcTo(x + w, y, x + w, y + h, r); ctx.arcTo(x + w, y + h, x, y + h, r); ctx.arcTo(x, y + h, x, y, r); ctx.arcTo(x, y, x + w, y, r); ctx.closePath(); }
function drawLanes() {
  const cv = $('#lanes'), tEnd = ui.frozenT ?? lastT, t0 = tEnd - ui.win;
  const vis = (S.bands || []).filter((b) => (b.end ?? tEnd) >= t0).sort((a, c) => a.start - c.start);
  const rows = [];
  for (const b of vis) { let i = rows.findIndex((r) => r < b.start - 2); if (i < 0) { i = rows.length; rows.push(0); } rows[i] = b.end ?? Infinity; b._row = i; }
  const { ctx, cw } = setupCanvas(cv, Math.max(1, rows.length) * 20 + 2), g = geom(cw);
  const X = (t) => g.l + ((t - t0) / ui.win) * g.w;
  ctx.font = '11px ' + css('--sans');
  if (!vis.length) { ctx.fillStyle = css('--muted'); ctx.fillText('No stressors in this window', g.l, 13); return; }
  for (const b of vis) {
    const x1 = Math.max(g.l, X(b.start)), x2 = Math.min(g.l + g.w, X(b.end ?? tEnd)), y = b._row * 20 + 1, col = css(BAND[b.color % 4]);
    ctx.fillStyle = col + '33'; roundRect(ctx, x1, y, Math.max(3, x2 - x1), 17, 5); ctx.fill();
    ctx.fillStyle = col; ctx.fillRect(x1, y, 3, 17);
    ctx.fillStyle = css('--ink'); ctx.save(); ctx.beginPath(); ctx.rect(x1 + 6, y, Math.max(0, x2 - x1 - 8), 17); ctx.clip();
    ctx.fillText(b.label + (b.end == null ? '  · on' : ''), x1 + 8, y + 12.5); ctx.restore();
  }
}
function niceMax(v) { const p = Math.pow(10, Math.floor(Math.log10(v))); for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p; return 10 * p; }
function drawChart(c, isLast) {
  const cv = $('#cv-' + c.id); if (!cv) return;
  const { ctx, cw } = setupCanvas(cv, c.h + (isLast ? 16 : 0)), g = geom(cw);
  const tEnd = ui.frozenT ?? lastT, t0 = tEnd - ui.win;
  const pts = points.filter((x) => x.t > t0 && x.t <= tEnd);
  const top = 6, bot = c.h - 4, ph = bot - top;
  let max = 0, min = Infinity;
  for (const s of c.series) for (const p of pts) { const v = s.get(p); if (v > max) max = v; if (v < min) min = v; }
  if (c.thresh) max = Math.max(max, c.thresh.v * 1.15);
  let Y, ticks;
  if (c.log) { const lo = Math.log10(Math.max(0.05, (min === Infinity ? 1 : min) * 0.8)), hi = Math.log10(Math.max(max * 1.3, 10)); Y = (v) => bot - ((Math.log10(Math.max(v, 0.05)) - lo) / (hi - lo)) * ph; ticks = [lo, (lo + hi) / 2, hi].map((l) => Math.pow(10, l)); }
  else { max = niceMax(max || 1); Y = (v) => bot - (v / max) * ph; ticks = [0, max / 2, max]; }
  const X = (t) => g.l + ((t - t0) / ui.win) * g.w;
  for (const b of S.bands || []) { const e = b.end ?? tEnd; if (e < t0) continue; const x1 = Math.max(g.l, X(b.start)), x2 = Math.min(g.l + g.w, X(e)), col = css(BAND[b.color % 4]); ctx.fillStyle = col + '18'; ctx.fillRect(x1, top, x2 - x1, ph); ctx.fillStyle = col + '88'; ctx.fillRect(x1, top, 1, ph); }
  ctx.strokeStyle = css('--border'); ctx.lineWidth = 1; ctx.fillStyle = css('--muted'); ctx.font = '10px ' + css('--mono'); ctx.textAlign = 'right';
  for (const tv of ticks) { const y = Math.round(Y(tv)) + .5; ctx.beginPath(); ctx.moveTo(g.l, y); ctx.lineTo(g.l + g.w, y); ctx.stroke(); ctx.fillText((c.axis || c.fmt)(tv), g.l - 6, y + 3); }
  if (c.thresh) { const y = Y(c.thresh.v); ctx.setLineDash([4, 4]); ctx.strokeStyle = css('--danger'); ctx.beginPath(); ctx.moveTo(g.l, y); ctx.lineTo(g.l + g.w, y); ctx.stroke(); ctx.setLineDash([]); ctx.textAlign = 'left'; ctx.fillStyle = css('--danger'); ctx.fillText(c.thresh.label, g.l + 4, y - 4); }
  c.series.forEach((s) => {
    if (!pts.length) return;
    const col = css(s.color);
    if (c.area) { ctx.beginPath(); ctx.moveTo(X(pts[0].t), bot); for (const p of pts) ctx.lineTo(X(p.t), Y(s.get(p))); ctx.lineTo(X(pts[pts.length - 1].t), bot); ctx.closePath(); ctx.fillStyle = col + '14'; ctx.fill(); }
    ctx.beginPath(); pts.forEach((p, i) => { const x = X(p.t), y = Y(s.get(p)); i ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
    ctx.strokeStyle = col; ctx.lineWidth = 1.6; ctx.lineJoin = 'round'; ctx.setLineDash(s.dashed ? [4, 3] : []); ctx.stroke(); ctx.setLineDash([]);
    const lp = pts[pts.length - 1]; ctx.beginPath(); ctx.arc(X(lp.t), Y(s.get(lp)), 2.6, 0, 7); ctx.fillStyle = col; ctx.fill();
  });
  if (isLast) {
    ctx.fillStyle = css('--muted'); ctx.textAlign = 'center'; ctx.font = '10px ' + css('--mono');
    const step = ui.win <= 120 ? 30 : ui.win <= 300 ? 60 : 120;
    for (let k = 0; k <= ui.win; k += step) ctx.fillText(k === 0 ? (ui.frozenT != null ? 'paused' : 'now') : '−' + (k >= 60 ? k / 60 + 'm' : k + 's'), Math.min(Math.max(X(tEnd - k), g.l + 14), g.l + g.w - 14), c.h + 12);
  }
  let at = pts[pts.length - 1];
  if (ui.hoverT != null) {
    const hp = pts.find((p) => p.t === ui.hoverT);
    if (hp) {
      at = hp; const x = X(hp.t);
      ctx.strokeStyle = css('--ink'); ctx.globalAlpha = .45; ctx.beginPath(); ctx.moveTo(x + .5, top); ctx.lineTo(x + .5, bot); ctx.stroke(); ctx.globalAlpha = 1;
      for (const s of c.series) { ctx.beginPath(); ctx.arc(x, Y(s.get(hp)), 3.2, 0, 7); ctx.fillStyle = css('--surface'); ctx.fill(); ctx.strokeStyle = css(s.color); ctx.lineWidth = 2; ctx.stroke(); }
    }
  }
  if (at) $('#lg-' + c.id).innerHTML = c.series.map((s) => `<span><span class="swatch" style="background:var(${s.color})"></span>${esc(s.name)} <em>${c.fmt(s.get(at))}</em></span>`).join('');
}
function drawAll() {
  if (!S) return;
  const defs = chartDefs();
  const key = defs.map((c) => c.id + ':' + c.series.map((s) => s.name).join(',')).join('|');
  if (key !== chartKey) buildCharts();
  drawLanes();
  defs.forEach((c, i) => drawChart(c, i === defs.length - 1));
  if (ui.hoverT != null) {
    const on = (S.bands || []).filter((b) => b.start <= ui.hoverT && (b.end ?? lastT) >= ui.hoverT).map((b) => b.label);
    $('#chartHint').textContent = `At −${fmtS(lastT - ui.hoverT)}: ${on.length ? on.join(' + ') + ' on' : 'no stressor on'}.`;
  } else $('#chartHint').textContent = ui.frozenT != null ? 'Paused. The sim keeps recording; resume to catch up.' : 'Live. Times are relative to now.';
}

// ---------- stressor panel ----------
function paramHtml(d, p, dr, on) {
  const v = dr.params[p.key], uid = `${d.id}-${p.key}`;
  if (p.type === 'steps') {
    const i = Math.max(0, p.opts.findIndex((o) => String(o) === String(v)));
    return `<div class="field"><label for="${uid}">${esc(p.label)}<output id="o-${uid}">${esc(fmtUnit(p.opts[i], p.unit))}</output></label><input type="range" id="${uid}" min="0" max="${p.opts.length - 1}" step="1" value="${i}" data-m="${d.id}" data-k="${p.key}"></div>`;
  }
  return `<div class="field"><span class="flabel">${esc(p.label)}</span><div class="pills" role="group" aria-label="${esc(p.label)}">${p.opts.map((o, i) => `<button class="pill" aria-pressed="${String(o) === String(v)}" data-m="${d.id}" data-k="${p.key}" data-i="${i}">${esc(fmtUnit(o, p.unit))}</button>`).join('')}</div></div>`;
}
function sqlFor(d, dr) {
  if (!d.sql) return '';
  return d.sql.replace(/\{(\w+)\}/g, (_, k) => k === 'table' ? (dr.table || '<table>') : String(dr.params[k] ?? ''));
}
function dialCard(d) {
  const a = activeOf(d.id), on = !!a, dr = draft(d);
  const color = on ? BAND[a.color % 4] : '--brand';
  const exp = ui.expanded[d.id] ?? on;
  const blocked = !d.available;
  const state = liveState(d, a);
  return cardHtml(d, a, on, dr, color, exp, blocked, state);
}
function liveState(d, a) {
  let state = '';
  if (a) {
    const st = a.status || {};
    const right = a.hold ? 'releases in ' + fmtClock(a.start + a.hold - S.now) : 'on for ' + fmtClock(S.now - a.start);
    const prog = st.progress >= 0 ? st.progress : a.hold ? (S.now - a.start) / a.hold : -1;
    state = `<div class="m-state"><span>${esc(st.phase || 'On')}</span><span>${esc(right)}</span></div>${prog >= 0 ? `<div class="progress"><i style="width:${Math.min(100, prog * 100).toFixed(0)}%"></i></div>` : ''}${st.note ? `<div class="meta">${esc(st.note)}</div>` : ''}${st.err ? `<div class="err">${esc(st.err)}</div>` : ''}`;
  }
  return state;
}
function cardHtml(d, a, on, dr, color, exp, blocked, state) {
  const memberIDs = d.members === 'all' ? S.members.map((m) => m.id) : others().map((m) => m.id);
  const members = d.members ? `<div class="field"><span class="flabel">${d.members === 'all' ? 'Member' + (d.single ? '' : 's') : 'Replica' + (d.single ? '' : 's')}</span><div class="pills">${memberIDs.map((m) => `<button class="pill" data-tgt="${d.id}" data-v="${esc(m)}" aria-pressed="${dr.targets.includes(m)}" ${on ? 'disabled' : ''}>${esc(m)}</button>`).join('') || '<span class="meta">none</span>'}</div></div>` : '';
  const tables = tablesFor(d);
  const table = d.needsTable ? `<div class="field"><label for="tb-${d.id}">Table</label>${tables.length ? `<select id="tb-${d.id}" data-table="${d.id}" ${on ? 'disabled' : ''}>${tables.map((t) => `<option value="${esc(t.name)}" ${t.name === dr.table ? 'selected' : ''}>perfsim.${esc(t.name)} · ${fmtBytes(t.bytes)}</option>`).join('')}</select>` : `<div class="note warn">Needs a prepared table${d.needsTable === 'nopk' ? ' without a primary key' : ''}. Prepare one under Datasets.</div>`}</div>` : '';
  const sql = sqlFor(d, dr);
  const ctl = d.kind === 'run'
    ? `<button class="btn small ${on ? '' : 'primary'}" data-run="${d.id}" ${blocked || ui.busy[d.id] ? 'disabled' : ''}>${on ? 'Cancel' : 'Run'}</button>`
    : `<button class="switch" role="switch" aria-checked="${on}" aria-label="${esc(d.name)}" data-toggle="${d.id}" ${blocked || ui.busy[d.id] ? 'disabled' : ''} ${blocked ? `title="${esc(d.why)}"` : ''}></button>`;
  const sub = blocked ? 'Not available here' : d.kind === 'run' ? 'One action' : on ? 'On' : 'Off';
  return `<div class="m ${on ? 'on' : ''} ${blocked ? 'blocked' : ''}" style="--mcolor:var(${color})" data-expanded="${exp}" data-card="${d.id}">
    <div class="m-h"><button class="m-toggle t" data-expand="${d.id}" aria-expanded="${exp}"><b>${esc(d.name)}</b><span class="meta">${sub}${d.watch && !blocked ? ' · moves ' + esc(d.watch.join(', ').toLowerCase()) : ''}</span></button>${ctl}</div>
    <div class="m-b"><div class="m-live">${state}</div><div class="cap">${esc(d.desc)}</div>${blocked ? `<div class="note">${esc(d.why)}</div>` : ''}${!blocked && d.note ? `<div class="note">${esc(d.note)}</div>` : ''}${!blocked && d.warn ? `<div class="note warn">${esc(d.warn)}</div>` : ''}
    ${blocked ? '' : members + table + d.params.map((p) => paramHtml(d, p, dr, on)).join('')}
    ${!blocked && sql ? `<details class="sql"><summary><span>What it runs</span><button class="btn small" data-copy="${d.id}">Copy</button></summary><pre id="sql-${d.id}">${esc(sql)}</pre></details>` : ''}</div></div>`;
}
function datasetsHtml() {
  const ds = S.datasets || [];
  const noPK = topo() === 'async' || topo() === 'standalone';
  return `<div class="ds">${ds.length ? ds.map((t) => `<div class="ds-row"><span class="mono">perfsim.${esc(t.name)}</span><span class="chip ${t.pk ? '' : 'warn'}">${fmtBytes(t.bytes)}</span><span class="meta">${fmtN(t.rows)} rows · ${t.pk ? 'primary key' : 'no primary key'} · <button class="btn link" data-drop="${esc(t.name)}">Drop</button></span></div>`).join('') : '<div class="cap">No tables yet. Large transactions, the no-op ALTER and the DDL lock need one.</div>'}
    ${S.prep ? `<div class="ds-row"><span class="mono">perfsim.${esc(S.prep.name)}</span><span class="chip brand">${Math.round(S.prep.done * 100)}%</span><div class="progress" style="grid-column:1/-1"><i style="width:${S.prep.done * 100}%"></i></div><span class="meta">${fmtBytes(S.prep.bytes * S.prep.done)} of ${fmtBytes(S.prep.bytes)} · <button class="btn link" data-cancelprep>Cancel</button></span></div>` : ''}
    <div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap"><select id="prepSize" class="btn small" aria-label="Size">${[0.5, 1, 2, 5, 10, 20, 50].map((g) => `<option value="${g}" ${g === 2 ? 'selected' : ''}>${g < 1 ? g * 1024 + ' MB' : g + ' GB'}</option>`).join('')}</select>
    <select id="prepShape" class="btn small" aria-label="Shape"><option value="pk">with primary key</option>${noPK ? '<option value="nopk">without primary key</option>' : ''}</select>
    <button class="btn small" id="prepBtn" ${S.prep ? 'disabled' : ''}>Prepare table</button></div>
    <div class="meta">Written in 20,000-row batches, so preparing does not lag the replicas by itself. The table lives in the perfsim schema on the writer.</div></div>`;
}
function renderGroups() {
  if (!S.ready) { $('#groups').innerHTML = '<div class="panel-b cap">Stressors appear once the sim has connected.</div>'; return; }
  const groups = [
    { key: 'work', title: 'Application workload', sub: `${S.threads} threads · always on`, body: `<div class="field"><label for="threads">Threads<output id="o-threads">${S.threads}</output></label><input type="range" id="threads" min="0" max="64" step="2" value="${S.threads}"></div><div class="cap">Point reads, small range reads and single-row writes on perfsim.orders, through the cluster's write endpoint. Its latency is the application's view on the timeline; more threads make every stressor bite harder.</div>` },
    { key: 'data', title: 'Datasets', sub: `${(S.datasets || []).length} prepared${S.prep ? ' · preparing' : ''}`, body: datasetsHtml() },
  ];
  const byGroup = {};
  for (const d of S.dials) (byGroup[d.group] ||= []).push(d);
  for (const [gname, ds] of Object.entries(byGroup)) {
    const key = gname === 'Replication lag' ? 'lag' : gname;
    const avail = ds.filter((d) => d.available).length;
    groups.push({ key, title: gname, sub: avail ? `${avail} of ${ds.length} available for this ${{ async: 'replication', gr: 'Group Replication', galera: 'Galera cluster', standalone: 'server' }[topo()]}` : 'Not available here', body: ds.map(dialCard).join(''), on: ds.filter((d) => activeOf(d.id)).length });
  }
  $('#groups').innerHTML = groups.map((g) => {
    const open = ui.open[g.key] ?? false;
    return `<div class="group" data-open="${open}"><button data-group="${esc(g.key)}" aria-expanded="${open}"><span style="display:grid;gap:2px;min-width:0"><b>${esc(g.title)}</b><span class="meta">${esc(g.sub)}</span></span><span style="display:flex;gap:8px;align-items:center">${g.on ? `<span class="chip warn">${g.on} on</span>` : ''}<svg class="chev" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 6l6 6-6 6"/></svg></span></button><div class="group-body">${g.body}</div></div>`;
  }).join('');
}
// The panel is rebuilt only when something structural changes, so a slider being
// dragged or an open "What it runs" is not reset every second.
let panelKey = '';
function renderPanel() {
  const key = JSON.stringify([S.ready, topo(), (S.active || []).map((a) => [a.id, a.status?.err]), S.dials.map((d) => d.available), (S.datasets || []).map((d) => d.name + Math.round(d.bytes / 1e7)), S.prep && Math.round(S.prep.done * 100), S.threads, S.members.map((m) => m.id + m.role)]);
  if (key === panelKey) { refreshLive(); return; }
  const focus = document.activeElement && document.activeElement.id;
  const openSql = [...document.querySelectorAll('#groups details[open] pre')].map((p) => p.id);
  if (document.activeElement && document.activeElement.type === 'range') return; // mid-drag
  panelKey = key;
  renderGroups();
  for (const id of openSql) { const p = document.getElementById(id); if (p) p.parentElement.open = true; }
  if (focus) { const el = document.getElementById(focus); if (el) el.focus(); }
}

function refreshLive() {
  for (const a of S.active || []) {
    const el = document.querySelector(`[data-card="${a.id}"] .m-live`);
    if (el) el.innerHTML = liveState(dialOf(a.id), a);
  }
}

// ---------- tabs ----------
async function renderTab() {
  const el = $('#tabBody');
  $('#statusTabBtn').textContent = S.info.statusTab || 'Replica status';
  if (ui.tab === 'sessions') {
    const rows = ui.sessions;
    el.innerHTML = `<div class="cap" style="margin-bottom:8px">What is running on every member, refreshed every two seconds. Red rows hold something on purpose (a stressor's lock or open transaction); amber rows wait for one.</div>
      <table><thead><tr><th>Id</th><th>Member</th><th>User</th><th>State</th><th>Time</th><th>Waiting on</th><th>Query</th></tr></thead><tbody>
      ${rows.length ? rows.map((r) => `<tr class="${r.kind || ''}"><td class="mono">${esc(r.id)}</td><td class="mono">${esc(r.member)}</td><td>${esc(r.user)}</td><td>${esc(r.state)}</td><td class="tnum">${fmtS(r.time)}</td><td class="mono">${r.waits ? '← ' + esc(r.waits) : ''}</td><td class="q">${esc(r.query)}</td></tr>`).join('') : '<tr><td colspan="7" class="muted">Nothing running beyond the workload.</td></tr>'}</tbody></table>`;
  } else if (ui.tab === 'status') {
    el.innerHTML = `<div class="kv">${S.members.map((m) => `<div class="kv-card"><div style="display:flex;justify-content:space-between;align-items:center;gap:8px"><b class="mono">${esc(m.id)}</b><span class="chip ${m.chip === 'ok' ? 'ok' : m.chip === 'warn' ? 'warn' : m.chip === 'bad' ? 'bad' : ''}">${esc(m.chipText)}</span></div><div class="meta">${esc(m.role)}${m.lag >= 0 && m.id !== writerID() ? ' · ' + fmtS(m.lag) + ' behind' : ''}</div><dl>${(m.fields || []).map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}</dl></div>`).join('')}</div>`;
  } else if (ui.tab === 'events') {
    const col = { info: 'var(--brand)', warn: 'var(--warning)', ok: 'var(--success)', neutral: 'var(--neutral)' };
    el.innerHTML = `<div class="log">${(S.events || []).slice().reverse().slice(0, 60).map((x) => `<div class="log-row"><span class="meta tnum">−${fmtClock(S.now - x.t)}</span><span class="dot" style="background:${col[x.level] || 'var(--neutral)'}"></span><span>${esc(x.text)}</span></div>`).join('') || '<span class="muted">Nothing yet.</span>'}</div>`;
  } else {
    const runs = S.runs || [];
    el.innerHTML = `<div class="runs">${S.recording ? `<div class="run" style="border-color:var(--danger)"><div><b>Recording now</b><div class="meta">${fmtClock(S.now - S.recording)} · the timeline, stressors, events and baseline are kept with the run</div></div><button class="btn small" data-stoprec>Stop and save</button></div>` : '<div class="cap">Record run keeps the timeline, the stressors and their settings, the events and the baseline from now until you stop it. Runs are saved in the perfsim schema, so they survive a redeploy of the sim.</div>'}
      ${runs.map((r) => `<div class="run"><div><b>${esc(r.name)}</b><div class="meta">${new Date(r.start * 1000).toLocaleString()} · ${fmtS(r.end - r.start)} · peak ${galera() ? 'queue ' + Math.round(r.peakLag) : 'lag ' + fmtS(r.peakLag)} · p99 peak ${fmtMs(r.peakP99)} · writes low ${fmtN(Math.max(0, r.lowWq))}/s</div></div><div style="display:flex;gap:6px;flex-wrap:wrap"><a class="btn small" href="api/runs/${encodeURIComponent(r.id)}?download=1">Export JSON</a><button class="btn small" data-delrun="${esc(r.id)}">Delete</button></div></div>`).join('')}</div>`;
  }
}
async function refreshSessions() {
  if (ui.tab !== 'sessions' || !S || !S.ready) return;
  try { ui.sessions = await api('GET', 'api/sessions'); } catch { /* keep the last list */ }
}

// ---------- render ----------
function render() {
  renderHeader();
  renderKpis();
  if (S.ready) { renderTopo(); renderImpact(); }
  renderPanel();
  if (!(ui.tab === 'runs' && !S.recording && document.activeElement?.closest('#tabBody'))) renderTab();
  if (!ui.paused) drawAll(); else drawLanes();
}

// ---------- events ----------
function toast(msg) { const t = $('#toast'); t.textContent = msg; t.hidden = false; clearTimeout(toast._t); toast._t = setTimeout(() => (t.hidden = true), 3200); }
async function act(fn, ok) { try { await fn(); if (ok) toast(ok); } catch (e) { toast(e.message); } panelKey = ''; await poll(); }

document.addEventListener('click', async (ev) => {
  const t = ev.target.closest('button, a'); if (!t || !S) return;
  const d = t.dataset;
  if (d.group) { ui.open[d.group] = !(ui.open[d.group] ?? false); panelKey = ''; renderPanel(); return; }
  if (d.expand) { const card = t.closest('.m'), now = card.dataset.expanded !== 'true'; ui.expanded[d.expand] = now; card.dataset.expanded = now; t.setAttribute('aria-expanded', now); return; }
  if (d.toggle || d.run) {
    const id = d.toggle || d.run, dial = dialOf(id), dr = draft(dial);
    ui.busy[id] = true; panelKey = ''; renderPanel();
    if (activeOf(id)) await act(() => api('POST', `api/dials/${id}/stop`));
    else { ui.expanded[id] = true; await act(() => api('POST', `api/dials/${id}/start`, { params: { ...dr.params, table: dr.table }, targets: dr.targets })); }
    delete ui.busy[id]; panelKey = ''; renderPanel();
    return;
  }
  if (d.tgt) {
    const dial = dialOf(d.tgt), dr = draft(dial);
    if (dial.single) dr.targets = [d.v];
    else { dr.targets = dr.targets.includes(d.v) ? dr.targets.filter((x) => x !== d.v) : [...dr.targets, d.v]; if (!dr.targets.length) dr.targets = [d.v]; }
    panelKey = ''; renderPanel(); return;
  }
  if (d.m && d.i != null) { setParam(d.m, d.k, dialOf(d.m).params.find((p) => p.key === d.k).opts[Number(d.i)]); panelKey = ''; renderPanel(); return; }
  if (d.copy) {
    ev.preventDefault();
    const pre = $('#sql-' + d.copy);
    try { await navigator.clipboard.writeText(pre.textContent); toast('Copied'); } catch { const r = document.createRange(); r.selectNodeContents(pre); getSelection().removeAllRanges(); getSelection().addRange(r); toast('Selected; press Ctrl+C or ⌘C'); }
    return;
  }
  if (d.tab) { ui.tab = d.tab; document.querySelectorAll('.tabs button').forEach((b) => b.setAttribute('aria-selected', b === t)); await refreshSessions(); renderTab(); return; }
  if (d.win) { ui.win = Number(d.win); document.querySelectorAll('[data-win]').forEach((b) => b.setAttribute('aria-pressed', b === t)); drawAll(); return; }
  if (d.drop) { await act(() => api('POST', `api/datasets/${encodeURIComponent(d.drop)}/drop`), 'Dropped perfsim.' + d.drop); return; }
  if ('cancelprep' in d) { await act(() => api('POST', 'api/datasets/cancel')); return; }
  if ('stoprec' in d) { await act(() => api('POST', 'api/record/stop', {}), 'Run saved'); return; }
  if (d.delrun) { await act(() => api('DELETE', `api/runs/${encodeURIComponent(d.delrun)}`), 'Run deleted'); return; }
  switch (t.id) {
    case 'pauseBtn': ui.paused = !ui.paused; ui.frozenT = ui.paused ? lastT : null; t.textContent = ui.paused ? 'Resume' : 'Pause'; t.setAttribute('aria-pressed', ui.paused); drawAll(); return;
    case 'baselineBtn': await act(() => api('POST', 'api/baseline'), 'Baseline set from the last 20 seconds'); return;
    case 'stopAll': await act(async () => { const r = await api('POST', 'api/stop-all'); toast(r.stopped ? 'Everything stopped and released' : 'Nothing was on'); }); return;
    case 'recBtn': await act(() => api('POST', S.recording ? 'api/record/stop' : 'api/record/start', {}), S.recording ? 'Run saved' : 'Recording'); return;
    case 'prepBtn': await act(() => api('POST', 'api/datasets/prepare', { gb: Number($('#prepSize').value), pk: $('#prepShape').value === 'pk' }), 'Preparing the table'); return;
  }
});
function setParam(id, key, value) {
  const dial = dialOf(id), dr = draft(dial);
  dr.params[key] = value;
  const pre = $('#sql-' + id); if (pre) pre.textContent = sqlFor(dial, dr);
  if (activeOf(id)) api('POST', `api/dials/${id}/params`, { params: { [key]: value } }).catch((e) => toast(e.message));
}
document.addEventListener('input', (ev) => {
  const t = ev.target;
  if (t.id === 'threads') { $('#o-threads').textContent = t.value; return; }
  if (t.dataset.m && t.type === 'range') {
    const p = dialOf(t.dataset.m).params.find((x) => x.key === t.dataset.k), v = p.opts[Number(t.value)];
    $('#o-' + t.id).textContent = fmtUnit(v, p.unit);
    setParam(t.dataset.m, t.dataset.k, v);
  }
});
document.addEventListener('change', (ev) => {
  const t = ev.target;
  if (t.id === 'threads') { act(() => api('POST', 'api/threads', { threads: Number(t.value) })); return; }
  if (t.dataset.table) { draft(dialOf(t.dataset.table)).table = t.value; const pre = $('#sql-' + t.dataset.table); if (pre) pre.textContent = sqlFor(dialOf(t.dataset.table), draft(dialOf(t.dataset.table))); }
  if (t.type === 'range') { panelKey = ''; }
});

// ---------- boot ----------
poll();
setInterval(poll, 1000);
setInterval(async () => { await refreshSessions(); }, 2000);
setInterval(() => { if (S) $('#fresh').textContent = Date.now() - lastOk < 3000 ? `Live · updated ${Math.max(1, Math.round((Date.now() - lastOk) / 1000))}s ago` : 'Reconnecting…'; }, 500);
addEventListener('resize', () => drawAll());
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { panelKey = ''; render(); });
})();
