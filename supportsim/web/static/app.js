'use strict';
// Vector Support Desk — dashboard. Plain JS, no build step, no CDN: the page is
// served from inside a lab network and must work with nothing but this container.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const pct = (a, b) => (b > 0 ? Math.round((100 * a) / b) : 0);
const f2 = (x) => (x == null || isNaN(x) ? '—' : Number(x).toFixed(2));
const fms = (x) => (x == null ? '—' : x < 10 ? x.toFixed(1) + ' ms' : Math.round(x) + ' ms');
const ago = (t) => { const s = Math.max(0, (Date.now() - new Date(t)) / 1000); return s < 60 ? Math.round(s) + 's ago' : Math.round(s / 60) + 'm ago'; };

async function api(path, body) {
  const r = await fetch('api/' + path, body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

const S = { status: null, metrics: null, recent: [], incidents: [], cat: null, teams: {}, articles: {}, lastDecision: null, pg: false };

// The desk runs on MongoDB (mongot) or on PostgreSQL (pgvector); V names things the
// way the engine in use does.
const V = {
  mongodb: { vec: '$vectorSearch', kw: '$search', vecEngine: 'mongot', shell: 'mongosh', db: 'MongoDB', rank: '$rankFusion', score: '$scoreFusion', ef: 'numCandidates', query: 'pipeline' },
  postgres: { vec: 'pgvector', kw: 'full-text search', vecEngine: 'pgvector', shell: 'psql', db: 'PostgreSQL', rank: 'RRF (SQL)', score: 'score fusion (SQL)', ef: 'hnsw.ef_search', query: 'SQL' },
};
const L = () => V[S.pg ? 'postgres' : 'mongodb'];
const engName = (e) => ({ mongot: 'mongot', app: 'in app', pgvector: 'pgvector', postgres: 'PostgreSQL FTS' }[e] || e || '');
const fuseName = (method) => (method === 'score' ? L().score : L().rank);
// A hit's cosine: pgvector reports it as the score; a cosine index in mongot reports (1 + cos) / 2.
const cosOf = (h) => (h.cosine != null ? h.cosine : 2 * h.score - 1);

function teamColor(t) { return S.teams[t] || '#8a94a6'; }
function teamChip(t) { return t ? `<span class="team"><i style="background:${teamColor(t)}"></i>${esc(t)}</span>` : '<span class="muted">—</span>'; }
function article(slug) { return S.articles[slug]?.title || slug || '—'; }

// ============================================================ boot & stream
async function boot() {
  const st = await api('state');
  S.cat = st.catalog;
  for (const t of S.cat.teams) S.teams[t.name] = t.color;
  for (const a of S.cat.articles) S.articles[a.slug] = a;
  S.status = st.status; S.metrics = st.metrics; S.recent = st.recent || []; S.incidents = st.incidents || [];
  S.pg = st.status.engine === 'postgres';
  document.body.classList.toggle('pg', S.pg);
  if (S.pg) {
    document.title = 'pgvector Support Desk';
    for (const sel of ['#fuMethod', '#tuMethod']) for (const o of $(sel).options) o.textContent = o.value === 'score' ? (sel === '#fuMethod' ? 'score fusion · by score' : 'score fusion') : (sel === '#fuMethod' ? 'RRF · by rank' : 'reciprocal rank fusion');
  }
  $('#incSpan').textContent = '3';
  renderStatus(); renderMetrics(); renderFeed(); renderIncidents();
  initShowdown(); initLab(); renderHood();
  showTab(location.hash.slice(1) || 'desk');
  connect();
  setInterval(() => $$('.tk-ago').forEach((el) => { el.textContent = ago(el.dataset.t); }), 5000);
}

function connect() {
  const es = new EventSource('api/events');
  es.onmessage = (m) => {
    const ev = JSON.parse(m.data);
    switch (ev.type) {
      case 'ticket': onTicket(ev.data); break;
      case 'update': onUpdate(ev.data); break;
      case 'incident': onIncident(ev.data); break;
      case 'metrics': S.metrics = ev.data; renderMetrics(); break;
      case 'status': S.status = ev.data; renderStatus(); break;
    }
  };
  es.onerror = () => { es.close(); setTimeout(async () => { try { const st = await api('state'); S.status = st.status; S.metrics = st.metrics; S.recent = st.recent || []; S.incidents = st.incidents || []; renderStatus(); renderMetrics(); renderFeed(); renderIncidents(); } catch (_) {} connect(); }, 3000); };
}

// ============================================================ header
function renderStatus() {
  const s = S.status; if (!s) return;
  const srv = s.server || {};
  const chips = [];
  if (S.pg) {
    chips.push(`<span class="chip ok"><span class="dot"></span>${esc(s.target || 'PostgreSQL')} · PostgreSQL ${esc(srv.version || '?')}${srv.setName ? ' · ' + esc(srv.setName) : ''}</span>`);
    if (s.mongot) chips.push(`<span class="chip ok"><span class="dot"></span>pgvector · ${s.indexes.length} indexes ready (HNSW + GIN)</span>`);
    else if (s.indexes && s.indexes.length) chips.push(`<span class="chip warn"><span class="dot"></span>building indexes: ${esc(s.indexes.map((i) => i.status).join(', '))}</span>`);
    else chips.push(`<span class="chip bad"><span class="dot"></span>no pgvector · in-app scan</span>`);
  } else chips.push(`<span class="chip ok"><span class="dot"></span>${esc(s.target || 'MongoDB')} · PSMDB ${esc(srv.version || '?')} · ${esc(srv.topology || '')}${srv.shards ? ` · tickets on ${srv.shards} shards` : ''}</span>`);
  if (S.pg) { /* chips above */ } else if (s.mongot) {
    chips.push(`<span class="chip ok"><span class="dot"></span>mongot · ${s.indexes.length} search indexes READY</span>`);
  } else if (s.indexes && s.indexes.length) {
    chips.push(`<span class="chip warn"><span class="dot"></span>mongot building: ${esc(s.indexes.map((i) => i.status).join(', '))}</span>`);
  } else {
    chips.push(`<span class="chip bad"><span class="dot"></span>no mongot · in-app scan</span>`);
  }
  chips.push(`<span class="chip"><span class="dot" style="background:var(--violet)"></span>${esc(s.model)} · ${s.dims}-d · in-process</span>`);
  chips.push(`<span class="chip"><span class="dot" style="background:var(--blue)"></span>${s.ticketDocs} tickets · ${s.kbDocs} articles</span>`);
  $('#chips').innerHTML = chips.join('');
  $('#pause').textContent = s.paused ? '▶ Resume' : '⏸ Pause';
  if (document.activeElement !== $('#rate')) { $('#rate').value = s.perMinute; $('#rateLabel').textContent = Math.round(s.perMinute) + '/min'; }
  const ph = $('#phase');
  if (s.phase !== 'running') {
    ph.className = 'phase'; ph.innerHTML = `<b>${esc(s.phase)}</b> — ${esc(s.detail)}`;
  } else if (!s.mongot) {
    ph.className = 'phase info';
    ph.innerHTML = S.pg
      ? (s.searchErr ? `<b>pgvector is not available in this database.</b> ${esc(s.searchErr)}` : `<b>PostgreSQL is building the vector indexes</b> — searches run in the app until they are valid.`)
      : s.searchErr ? `<b>Vector search is not on for this cluster.</b> ${esc(s.searchErr)}` : `<b>mongot is still building the search indexes</b> — searches run in the app until they are READY.`;
  } else ph.className = 'phase hidden';
  if ($('#tab-hood').classList.contains('active')) renderHood();
  if ($('#tab-lab').classList.contains('active')) renderIndexes();
  renderDeskFusion();
}

$('#pause').onclick = () => api('controls', { paused: !S.status?.paused }).then((s) => { S.status = s; renderStatus(); });
let rateT;
$('#rate').oninput = (e) => { $('#rateLabel').textContent = e.target.value + '/min'; clearTimeout(rateT); rateT = setTimeout(() => api('controls', { perMinute: Number(e.target.value) }), 300); };
$('#outage').onclick = async () => { const r = await api('outage', {}); flash(`Outage queued — watch several customers report “${(S.cat.outages.find((o) => o.id === r.outage) || {}).label || r.outage}” in their own words.`); };

function flash(msg) {
  const el = document.createElement('div');
  el.className = 'phase info'; el.style.position = 'fixed'; el.style.bottom = '18px'; el.style.right = '18px'; el.style.zIndex = 60; el.style.maxWidth = '420px';
  el.textContent = msg; document.body.appendChild(el); setTimeout(() => el.remove(), 5000);
}

// tabs — the hash names the open one (#lab), so a tab can be linked to and survives a reload
function showTab(name) {
  const b = $(`.tab[data-tab="${name}"]`); if (!b) return;
  $$('.tab').forEach((x) => x.classList.toggle('active', x === b));
  $$('.pane').forEach((p) => p.classList.toggle('active', p.id === 'tab-' + name));
  if (name === 'lab' && !mapState.data) loadMap();
  if (name === 'hood') renderHood();
  if (name === 'workshop') openWorkshop(); else closeWorkshop();
}
$$('.tab').forEach((b) => b.onclick = () => { history.replaceState(null, '', '#' + b.dataset.tab); showTab(b.dataset.tab); });

// ============================================================ live desk
function renderMetrics() {
  const m = S.metrics; if (!m) return;
  const sc = m.scored || 0;
  const autoPct = pct(m.autoAnswered, m.tickets), prec = pct(m.autoCorrect, m.autoAnswered);
  const hrs = m.minutesSaved >= 60 ? (m.minutesSaved / 60).toFixed(1) + ' hours' : m.minutesSaved + ' minutes';
  $('#hero').innerHTML = sc < 5
    ? `Warming up — ${m.tickets} ticket${m.tickets === 1 ? '' : 's'} so far. Each one is turned into a 384-number vector and compared with every resolved ticket and article in milliseconds.`
    : `Across <b>${m.tickets}</b> tickets, vector search answered <b>${autoPct}%</b> on its own (<b>${prec}%</b> of those correctly), merged <b>${m.duplicates}</b> repeat ticket${m.duplicates === 1 ? '' : 's'} and raised <b>${m.incidents}</b> incident${m.incidents === 1 ? '' : 's'}: about <b>${hrs}</b> of agent time not spent. Matching tickets straight to the docs, it picks the right article <b>${pct(m.answerDirect, sc)}%</b> of the time; keyword search, <span class="kw">${pct(m.answerKeyword, sc)}%</span>.`;
  const k = [
    ['Tickets handled', m.tickets, `${fms(m.embedLat?.p50)} to embed (p50)`],
    ['Answered automatically', m.autoAnswered, m.autoAnswered ? `${prec}% correct · ${m.autoReopened} reopened` : 'nearest past case ≥ 0.70 cos'],
    ['Repeat tickets merged', m.duplicates, m.duplicates ? `${pct(m.dupCorrect, m.duplicates)}% truly the same problem` : 'same customer, same problem'],
    ['Incidents raised', m.incidents, 'from bursts of similar tickets'],
    ['Agent time saved', m.minutesSaved >= 60 ? (m.minutesSaved / 60).toFixed(1) + ' h' : m.minutesSaved + ' min', '12 min/answer · 8 min/merge'],
    [`${L().vec} latency`, fms(m.vectorLat?.p50), `p95 ${fms(m.vectorLat?.p95)} · ${L().kw} p50 ${fms(m.keywordLat?.p50)}`],
  ];
  $('#kpis').innerHTML = k.map(([l, v, s]) => `<div class="kpi"><div class="l">${l}</div><div class="v">${v}</div><div class="s">${s}</div></div>`).join('');
  const barOf = (label, n, d, color, hint) => `<div class="bar-row"><div class="bl"><span>${esc(label)}</span><b>${pct(n, d)}%</b></div><div class="bar"><div style="width:${pct(n, d)}%;background:${color}"></div></div>${hint ? `<div class="muted small">${hint}</div>` : ''}</div>`;
  const bar = (label, n, color, hint) => `<div class="bar-row"><div class="bl"><span>${label}</span><b>${pct(n, sc)}%</b></div><div class="bar"><div style="width:${pct(n, sc)}%;background:${color}"></div></div>${hint ? `<div class="muted small">${hint}</div>` : ''}</div>`;
  $('#scoreboard').innerHTML = sc === 0 ? '<div class="muted small">Scores appear after the first tickets.</div>' : `
    <div class="grp">Searching past tickets · customers vs customers</div>
    <div class="bars">
      ${bar('Vector → right answer', m.answerVector, 'var(--accent)', 'vote of the answers that closed the 5 nearest resolved tickets')}
      ${bar('Keyword → right answer', m.answerKeywordCase, 'var(--warn)', `the same vote over the top 5 ${L().kw} hits`)}
      ${bar('Vector → right team', m.routeVector, 'var(--accent)')}
      ${bar('Keyword → right team', m.routeKeyword, 'var(--warn)')}
    </div>
    <div class="grp">Searching the docs · customers vs technical writers</div>
    <div class="bars">
      ${bar('Vector · ' + L().vec, m.answerDirect, 'var(--accent)')}
      ${barOf('Hybrid · ' + (S.status?.hybridName || L().rank), m.answerHybrid, m.hybridScored, 'var(--violet)', m.hybridScored < m.scored ? `since the blend was changed: ${m.hybridScored} tickets` : 'change it in the Search Showdown → Tune the blend')}
      ${bar('Keyword · ' + L().kw, m.answerKeyword, 'var(--warn)')}
    </div>
    <div class="lesson">See where keyword search holds up: comparing tickets with other tickets, customers use each other's words. Against articles someone else wrote, the words stop matching and only the meaning carries over. Hybrid inherits keyword's misses at equal weights: find a better blend under Search Showdown → Tune the blend.
    <br><br>And only vector scores can drive automation: cos ≥ 0.70 means "same problem" for every ticket. ${S.pg ? 'A ts_rank_cd of 0.4' : 'A BM25 score of 12'} means nothing on its own.</div>`;
}

function ticketCard(d) {
  const t = d.ticket;
  const el = document.createElement('div');
  el.className = 'tk'; el.dataset.id = t.id;
  el.innerHTML = cardHTML(d);
  el.onclick = () => openDecision(t.id);
  return el;
}

function cardHTML(d) {
  const t = d.ticket;
  const acts = [];
  if (d.incident) acts.push(`<span class="act inc">🚨 Part of incident #${d.incident.id}</span>`);
  if (d.duplicate) acts.push(`<span class="act dup">🔗 Repeat of #${esc(d.duplicate.id)} · cos ${f2(d.duplicate.cosine)}</span>`);
  if (d.incident) acts.push(`<span class="act">→ status page · ${teamChip('Incident Response')} ${mark(d.route.correct)}</span>`);
  else if (d.novel) acts.push(`<span class="act">❔ New kind of problem (nearest past case ${f2(d.neighbors?.[0]?.cosine)}): no guess, sent to a human</span>`);
  else {
    if (d.autoAnswered) acts.push(`<span class="act auto">⚡ Answered: “${esc(d.answer.title)}” ${mark(d.answer.correct)}</span>`);
    else if (d.answer.value) acts.push(`<span class="act">💡 Suggest: “${esc(d.answer.title)}” ${mark(d.answer.correct)}</span>`);
    acts.push(`<span class="act">→ ${teamChip(d.route.value)} ${mark(d.route.correct)}</span>`);
  }
  if (!d.incident && !d.novel && d.keywordAnswer.value && !d.keywordAnswer.correct && d.answer.correct)
    acts.push(`<span class="act kw">keyword would answer: “${esc(article(d.keywordAnswer.value))}” <span class="bad">✗</span></span>`);
  return `<div class="tk-head"><span class="tk-id">#${t.id}</span><span class="status ${t.status}">${t.status.replace('_', ' ')}</span><span>${esc(t.customer)} · ${esc(t.plan)}</span><span class="tk-ago" data-t="${t.createdAt}">${ago(t.createdAt)}</span><span style="margin-left:auto">nearest past case cos ${f2(d.neighbors?.[0]?.cosine)}</span></div>
    <div class="tk-subj">${esc(t.subject)}</div><div class="tk-body">${esc(t.body)}</div><div class="tk-acts">${acts.join('')}</div>`;
}

function renderFeed() {
  const feed = $('#feed'); feed.innerHTML = '';
  if (!S.recent.length) { feed.innerHTML = '<div class="muted">No tickets yet.</div>'; return; }
  for (const d of S.recent.slice(0, 40)) feed.appendChild(ticketCard(d));
}

function onTicket(d) {
  S.recent.unshift(d); S.recent = S.recent.slice(0, 150);
  S.lastDecision = d;
  const feed = $('#feed');
  if (feed.firstElementChild && !feed.firstElementChild.classList.contains('tk')) feed.innerHTML = '';
  feed.prepend(ticketCard(d));
  while (feed.children.length > 40) feed.lastElementChild.remove();
}

function onUpdate(u) {
  const d = S.recent.find((x) => x.ticket.id === u.id);
  if (d) { d.ticket.status = u.status; const el = $(`.tk[data-id="${u.id}"]`); if (el) el.innerHTML = cardHTML(d); }
}

function onIncident(inc) {
  const i = S.incidents.findIndex((x) => x.id === inc.id);
  if (i >= 0) S.incidents[i] = inc; else S.incidents.unshift(inc);
  renderIncidents();
}

function renderIncidents() {
  if (!S.incidents.length) return;
  $('#incidents').innerHTML = S.incidents.slice(0, 6).map((inc) => `<div class="inc"><b>🚨 Incident #${inc.id}: “${esc(inc.title)}”</b>
    <span class="small">${inc.tickets.length} tickets · first seen ${ago(inc.firstSeen)}</span><div class="small muted">${inc.tickets.slice(0, 12).map((t) => '#' + t).join(' ')}</div></div>`).join('');
}

// ------------------------------------------------------------ the "why" drawer
async function openDecision(id) {
  let d = S.recent.find((x) => x.ticket.id === id);
  try { d = await api('ticket?id=' + id); } catch (_) {}
  if (!d) return;
  const t = d.ticket;
  const nb = (d.neighbors || []).map((n, i) => `<div class="nb"><span class="muted">#${esc(n.id)}</span><span class="t" title="${esc(n.title)}">${i < 5 ? '' : '<span class="muted">(team vote only) </span>'}${esc(n.title)} <span class="muted small">→ ${esc(article(n.kb))}</span> ${n.kb === t.truth.kb ? '<span class="good">✓</span>' : ''}</span>
      <span style="display:flex;gap:6px;align-items:center"><span class="simbar" style="flex:1"><div style="width:${Math.max(0, n.cosine) * 100}%"></div></span><span class="small">${f2(n.cosine)}</span></span></div>`).join('');
  const th = S.cat.thresholds;
  const pipes = Object.entries(d.pipelines || {});
  $('#drawerBody').innerHTML = `
    <div class="tk-head"><span class="tk-id">#${t.id}</span><span class="status ${t.status}">${t.status.replace('_', ' ')}</span><span>${esc(t.customer)} · ${esc(t.plan)}</span></div>
    <h2 style="margin-top:6px">${esc(t.subject)}</h2><p class="muted">${esc(t.body)}</p>
    <div class="card"><div class="kv">
      <div>Ground truth</div><div>${esc(t.truth.archetype)} → ${teamChip(t.truth.team)} · “${esc(t.truth.kbTitle)}” <span class="muted small">(the simulator knows this; a real desk doesn't)</span></div>
      <div>Embedding</div><div>${d.tokens} tokens → 384 numbers in ${fms(d.embedMs)}, in this process</div>
      <div>Search engine</div><div>${d.engine === 'mongot' ? 'mongot ($vectorSearch / $search)' : d.engine === 'pgvector' ? 'PostgreSQL (pgvector HNSW / full-text GIN)' : `in-app scan (no ${L().vecEngine})`} · ${fms(d.vectorMs)} for the neighbour search</div>
    </div></div>
    <h3>1 · The 10 most similar resolved tickets</h3>
    <p class="muted small">Cosine similarity to this ticket. The first 5 vote on the answer and all 10 vote on the team, each vote weighted by its similarity.</p>
    ${nb || '<div class="muted">none</div>'}
    <h3 style="margin-top:16px">2 · What the desk decided</h3>
    <div class="kv">
      <div>Answer</div><div>“${esc(d.answer.title || '—')}” · ${Math.round((d.answer.share || 0) * 100)}% of the vote ${d.answer.correct ? '<span class="good">✓ right</span>' : '<span class="bad">✗ wrong</span>'}</div>
      <div>Sent automatically?</div><div>${d.autoAnswered ? '<b class="good">yes</b>' : 'no'} <span class="muted small">(rule: nearest cos ≥ ${th.autoAnswer} and ≥ ${th.agreement * 100}% agreement; here ${f2(d.neighbors?.[0]?.cosine)} and ${Math.round((d.answer.share || 0) * 100)}%)</span></div>
      <div>Team</div><div>${teamChip(d.route.value)} · ${Math.round((d.route.share || 0) * 100)}% of the vote ${d.route.correct ? '<span class="good">✓</span>' : '<span class="bad">✗</span>'}</div>
      <div>Repeat?</div><div>${d.duplicate ? `yes — same customer's open #${esc(d.duplicate.id)}, cos ${f2(d.duplicate.cosine)} (≥ ${th.duplicate})` : `no open ticket from ${esc(t.customer)} at cos ≥ ${th.duplicate}`}</div>
      <div>New kind of problem?</div><div>${d.novel ? `<b>yes</b> — nothing resolved is closer than ${th.novel} (nearest ${f2(d.neighbors?.[0]?.cosine)})${d.answer.value === 'service-status' ? ', or it looks like past outage reports' : ''}` : `no — nearest resolved ticket is ${f2(d.neighbors?.[0]?.cosine)} (≥ ${th.novel})`}</div>
      <div>Incident?</div><div>${d.incident ? `yes — incident #${d.incident.id} (${d.incident.tickets.length} tickets)` : d.novel ? `not yet — needs ${th.incidentMin} new-kind tickets at cos ≥ ${th.incident} within 3 min` : 'no — only new kinds of problem can start one'}</div>
    </div>
    <h3 style="margin-top:16px">3 · The same question, other ways</h3>
    <div class="kv">
      <div>Vector · KB articles</div><div>“${esc(article(d.directAnswer.value))}” ${mark(d.directAnswer.correct)}</div>
      <div>Hybrid · KB articles</div><div>“${esc(article(d.hybridAnswer.value))}” ${mark(d.hybridAnswer.correct)}</div>
      <div>Keyword · KB articles</div><div>“${esc(article(d.keywordAnswer.value))}” ${mark(d.keywordAnswer.correct)}</div>
      <div>Keyword · past tickets</div><div>“${esc(article(d.keywordCase.value))}” ${mark(d.keywordCase.correct)}</div>
      <div>Keyword · team</div><div>${teamChip(d.keywordRoute.value)} ${mark(d.keywordRoute.correct)}</div>
    </div>
    <h3 style="margin-top:16px">4 · The exact ${S.pg ? 'SQL' : 'pipelines'}</h3>
    <p class="muted small">${S.pg ? 'What the desk ran, statement by statement, with its parameters ($1, $2…) listed underneath. The tables are in the <code>supportsim</code> schema. The query vector is abbreviated.' : 'Paste any of these into mongosh connected to this cluster (<code>use supportsim</code>). The query vector is abbreviated.'}</p>
    <div class="subtabs">${pipes.map(([k], i) => `<button class="btn ${i === 0 ? 'on' : ''}" data-p="${k}">${pipeName(k)}</button>`).join('')}</div>
    <div id="pipeOut">${codeBlock(pipes[0]?.[1] || '')}</div>`;
  $$('#drawerBody .subtabs button').forEach((b) => b.onclick = () => { $$('#drawerBody .subtabs button').forEach((x) => x.classList.toggle('on', x === b)); $('#pipeOut').innerHTML = codeBlock(d.pipelines[b.dataset.p]); bindCopy(); });
  bindCopy();
  $('#drawer').classList.remove('hidden');
}
const mark = (ok) => (ok ? '<span class="good">✓</span>' : '<span class="bad">✗</span>');
const pipeName = (k) => ({ similar: 'similar resolved tickets', kbVector: 'KB · vector', kbKeyword: 'KB · keyword', kbHybrid: 'KB · hybrid', keywordRoute: 'team · keyword', duplicate: 'repeat check', incident: 'incident check' }[k] || k);
$('#drawerX').onclick = () => $('#drawer').classList.add('hidden');
$('#drawer').onclick = (e) => { if (e.target.id === 'drawer') $('#drawer').classList.add('hidden'); };
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') $('#drawer').classList.add('hidden'); });

function codeBlock(text) { return `<div class="code-wrap"><pre class="code">${esc(text)}</pre><button class="btn copy">copy</button></div>`; }
function bindCopy() { $$('.copy').forEach((b) => b.onclick = (e) => { e.stopPropagation(); navigator.clipboard?.writeText(b.previousElementSibling.textContent); b.textContent = 'copied'; setTimeout(() => (b.textContent = 'copy'), 1200); }); }

// ============================================================ showdown
const STOP = new Set('a an the and or but to of in on at for with my our we i is it was are be can cant can\'t how do does did this that from your you me us after since their they them its into as by not no so up out about what when why which will would could should have has had get got just any some all'.split(' '));
const words = (s) => (s.toLowerCase().match(/[a-z0-9']+/g) || []).map((w) => w.replace(/'s$/, '')).filter((w) => w.length > 2 && !STOP.has(w));

const FU = { method: 'rank', vectorWeight: 0.5, normalization: 'sigmoid' };
function fusionLabel(o) { return `${o.method === 'score' ? '$scoreFusion (' + o.normalization + ')' : '$rankFusion'} · vector ${(+o.vectorWeight).toFixed(1)} : keyword ${(1 - o.vectorWeight).toFixed(1)}`; }
function initShowdown() {
  const d = S.status?.hybrid; if (d) Object.assign(FU, d);
  $('#fuMethod').value = FU.method; $('#fuNorm').value = FU.normalization; $('#fuWeight').value = FU.vectorWeight;
  const sync = () => {
    FU.method = $('#fuMethod').value; FU.normalization = $('#fuNorm').value; FU.vectorWeight = Number($('#fuWeight').value);
    $('#fuNorm').classList.toggle('hidden', FU.method !== 'score');
    $('#fuWeightLbl').textContent = `${(1 - FU.vectorWeight).toFixed(1)} : ${FU.vectorWeight.toFixed(1)}`;
  };
  let t; const rerun = () => { sync(); clearTimeout(t); t = setTimeout(() => { if ($('#sdQuery').value.trim()) runShowdown(); }, 250); };
  ['#fuMethod', '#fuNorm', '#fuWeight'].forEach((id) => ($(id).oninput = rerun));
  sync(); renderDeskFusion();
  $('#tuGo').onclick = runTune;
  $('#sdExamples').innerHTML = S.cat.examples.map((e) => `<button class="ex" data-q="${esc(e.query)}">${esc(e.query)}</button>`).join('');
  $$('#sdExamples .ex').forEach((b) => b.onclick = () => { $('#sdQuery').value = b.dataset.q; runShowdown(); });
  $('#sdForm').onsubmit = (e) => { e.preventDefault(); runShowdown(); };
}

async function runShowdown() {
  const q = $('#sdQuery').value.trim(); if (!q) return;
  const coll = $('#sdColl').value;
  $('#sdCols').innerHTML = '<div class="muted">Searching…</div>';
  const r = await api('showdown', { query: q, collection: coll, k: 5, hybrid: { ...FU } });
  const qw = new Set(words(q));
  const hl = (s) => esc(s).replace(/[A-Za-z0-9']+/g, (w) => (qw.has(w.toLowerCase().replace(/'s$/, '')) ? `<mark>${w}</mark>` : w));
  const truth = r.truth;
  const rankOf = (res) => { const i = (res.hits || []).findIndex((h) => h.kb === truth); return i < 0 ? null : i + 1; };
  const col = (title, sub, res, color) => `<div class="sd-col" style="border-top:3px solid ${color}">
      <h3>${title}<span class="eng">${esc(engName(res.engine))} · ${fms(res.ms)}</span></h3>
      <div class="muted small">${sub}</div>
      ${res.error ? `<div class="note">${esc(res.error)}</div>` : ''}
      ${(res.hits || []).map((h, i) => `<div class="hit ${truth && h.kb === truth ? 'right' : ''}"><div class="ht"><span class="rank">${i + 1}</span><span>${hl(h.title)}</span>${truth ? `<span class="ok">${h.kb === truth ? '<span class="good">✓</span>' : ''}</span>` : ''}</div>
        <div class="meta">${teamChip(h.team)}${coll === 'tickets' ? `<span>→ ${esc(article(h.kb))}</span>` : ''}<span>score ${h.parts ? h.score.toFixed(4) : f2(h.score)}</span></div>${partsBar(h)}</div>`).join('') || '<div class="muted small" style="padding:8px 0">no results</div>'}
      ${res.note ? `<div class="note">${esc(res.note)}</div>` : ''}
      <details><summary>${L().query}</summary>${codeBlock(res.pipeline)}</details></div>`;
  $('#sdCols').innerHTML = S.pg
    ? col('Keyword', '<code>tsv @@ query</code> · ts_rank_cd over the words', r.keyword, 'var(--warn)') +
      col('Vector', '<code>embedding &lt;=&gt; $1</code> · nearest meaning', r.vector, 'var(--accent)') +
      col('Hybrid', `${FU.method === 'score' ? 'score fusion' : 'reciprocal rank fusion'} in one SQL statement · vector ${FU.vectorWeight.toFixed(1)} : keyword ${(1 - FU.vectorWeight).toFixed(1)}`, r.hybrid, 'var(--violet)')
    : col('Keyword', '<code>$search</code> · BM25 over the words', r.keyword, 'var(--warn)') +
      col('Vector', '<code>$vectorSearch</code> · nearest meaning', r.vector, 'var(--accent)') +
      col('Hybrid', `<code>${FU.method === 'score' ? '$scoreFusion' : '$rankFusion'}</code> · vector ${FU.vectorWeight.toFixed(1)} : keyword ${(1 - FU.vectorWeight).toFixed(1)}`, r.hybrid, 'var(--violet)');
  bindCopy();
  // verdict
  let v;
  if (truth) {
    const a = S.articles[truth];
    const shared = [...new Set(words(a.title + ' ' + (await api('article?slug=' + truth)).summary))].filter((w) => qw.has(w));
    const rk = rankOf(r.keyword), rv = rankOf(r.vector), rh = rankOf(r.hybrid);
    const say = (n) => (n ? `<b class="good">#${n}</b>` : '<b class="bad">not in the top 5</b>');
    v = `The answer is <b>“${esc(a.title)}”</b>. Keyword: ${say(rk)} · Vector: ${say(rv)} · Hybrid: ${say(rh)}.
      Your query shares <b>${shared.length}</b> meaningful word${shared.length === 1 ? '' : 's'} with that article${shared.length ? ` (${shared.map(esc).join(', ')})` : ''}${shared.length ? '' : ' — keyword search has nothing to hold on to; vector search doesn\'t need it'}.`;
    const lesson = !rv && !rk ? 'Nobody found it. A small general-purpose model doesn\'t know that "refuses writes after a network blip" means "no primary": that\'s domain knowledge. Real systems fix this with a model tuned on their own data, or by embedding example questions alongside each article.'
      : rk && (!rv || rk < rv) ? 'Here the words did match, and keyword search beat vector search. Neither signal is always better, which is why hybrid search exists: fused, the right answer ranks well either way.'
      : rv && (!rk || rv < rk) ? 'The customer\'s words aren\'t the article\'s words, so meaning is the only thing connecting them. This is the everyday case vector search exists for.'
      : 'Both found it: when the customer happens to use the article\'s own words, keyword search is fine.';
    v += `<div class="lesson" style="margin-top:8px">${lesson}</div>`;
  } else {
    v = `This is your own query, so there's no answer key. Read the columns: keyword results share your <mark>words</mark>, vector results share your <i>situation</i>. Query embedded in ${fms(r.query.ms)} (${r.query.tokens.length} tokens).`;
  }
  const coherence = (res) => { const hs = res.hits || []; if (!hs.length) return 0; const c = {}; hs.forEach((h) => (c[h.team] = (c[h.team] || 0) + 1)); return Math.max(...Object.values(c)); };
  v += `<div class="small muted" style="margin-top:6px">Staying on topic: the vector top ${r.vector.hits?.length || 0} share one team ${coherence(r.vector)}× (the runners-up are neighbours in meaning); the keyword top ${r.keyword.hits?.length || 0}, ${coherence(r.keyword)}×.</div>`;
  $('#sdVerdict').innerHTML = `<div class="verdict">${v}</div>`;
}

// partsBar draws a fused score's make-up: how much came from the vector ranking and
// how much from the keyword one (the server's scoreDetails).
function partsBar(h) {
  if (!h.parts || !h.parts.length) return '';
  const v = h.parts.filter((p) => p.pipeline === 'vector').reduce((a, p) => a + p.value, 0);
  const k = h.parts.filter((p) => p.pipeline === 'keyword').reduce((a, p) => a + p.value, 0);
  const tot = v + k || 1;
  const desc = h.parts.map((p) => `${p.pipeline}: ${p.rank ? 'rank ' + p.rank + ', ' : ''}raw ${f2(p.raw)}, weight ${f2(p.weight)} → ${p.value.toFixed(4)}`).join('\n');
  return `<div class="parts" title="${esc(desc)}"><div class="pv" style="width:${(100 * v) / tot}%"></div><div class="pk" style="width:${(100 * k) / tot}%"></div></div>
    <div class="small muted">${h.parts.map((p) => `${p.pipeline}${p.rank ? ' #' + p.rank : ''}`).join(' + ')}${h.parts.length === 1 ? ' only' : ''}</div>`;
}

function renderDeskFusion() { if (S.status) $('#fuDesk').innerHTML = `desk uses: <b>${esc(S.status.hybridName || '')}</b>`; }

async function runTune() {
  const body = { collection: $('#tuColl').value, method: $('#tuMethod').value, normalization: $('#tuNorm').value, queries: 80 };
  $('#tuGo').disabled = true; $('#tuOut').innerHTML = '<div class="muted">Sweeping 11 weights × 80 tickets… (about 10 s)</div>';
  try {
    const r = await api('hybrid/tune', body);
    const W = 640, H = 220, pl = 44, pb = 30, pt = 14, pr = 44;
    const accs = r.points.map((p) => p.accuracy);
    const lo = Math.max(0, Math.min(...accs) - 0.05), hi = Math.min(1, Math.max(...accs) + 0.05);
    const x = (w) => pl + w * (W - pl - pr), y = (a) => pt + (1 - (a - lo) / (hi - lo || 1)) * (H - pt - pb);
    const line = r.points.map((p, i) => `${i ? 'L' : 'M'}${x(p.vectorWeight)},${y(p.accuracy)}`).join(' ');
    const desk = S.status?.hybrid;
    const deskX = desk && desk.method === r.method ? x(desk.vectorWeight) : null;
    const ticks = [lo, (lo + hi) / 2, hi];
    const flat = Math.max(...accs) - Math.min(...accs.slice(1)) < 0.02;
    $('#tuOut').innerHTML = `
      <svg viewBox="0 0 ${W} ${H}" width="100%" style="max-width:${W}px" role="img" aria-label="Accuracy by vector weight">
        ${ticks.map((t) => `<line x1="${pl}" x2="${W - pr}" y1="${y(t)}" y2="${y(t)}" stroke="var(--line)"/><text x="${pl - 6}" y="${y(t) + 4}" text-anchor="end" font-size="11" class="muted">${Math.round(t * 100)}%</text>`).join('')}
        ${[0, 0.25, 0.5, 0.75, 1].map((w) => `<text x="${x(w)}" y="${H - 10}" text-anchor="middle" font-size="11" class="muted">${w === 0 ? 'keyword only' : w === 1 ? 'vector only' : w}</text>`).join('')}
        ${deskX != null ? `<line x1="${deskX}" x2="${deskX}" y1="${pt}" y2="${H - pb}" stroke="var(--violet)" stroke-dasharray="4 3"/><text x="${deskX + 4}" y="${pt + 10}" font-size="10" fill="var(--violet)">desk now</text>` : ''}
        <path d="${line}" fill="none" stroke="var(--accent)" stroke-width="2.5"/>
        ${r.points.map((p) => `<circle cx="${x(p.vectorWeight)}" cy="${y(p.accuracy)}" r="${p === r.best || p.vectorWeight === r.best.vectorWeight ? 6 : 3.5}" fill="${p.vectorWeight === r.best.vectorWeight ? 'var(--good)' : 'var(--accent)'}"><title>vector ${p.vectorWeight}: ${Math.round(p.accuracy * 100)}% right, ${fms(p.ms)}</title></circle>`).join('')}
      </svg>
      <div class="metrics-row"><span>best: vector <b>${r.best.vectorWeight.toFixed(1)}</b> : keyword <b>${(1 - r.best.vectorWeight).toFixed(1)}</b> → <b>${Math.round(r.best.accuracy * 100)}%</b> right</span>
        <span>keyword only <b>${Math.round(r.points[0].accuracy * 100)}%</b> · vector only <b>${Math.round(r.points[10].accuracy * 100)}%</b></span><span class="muted small">${esc(engName(r.engine))} · ${r.seconds.toFixed(1)} s</span></div>
      <div class="lesson">${flat ? 'The curve is flat: every blend that includes the vectors does about equally well here, so there is little to tune. Both engines already find these answers. Tuning matters where they disagree, as against the KB articles.'
        : r.best.vectorWeight >= 0.95 ? 'Here every bit of keyword weight costs accuracy: against articles written in other words, BM25 mostly adds noise. The honest answer is to let vector search decide. Hybrid search is not automatically better; it is better when the keyword signal is worth something.'
        : r.best.vectorWeight <= 0.05 ? 'Keyword alone does best here: the queries use the same words as the documents.'
        : `A blend beats either engine alone: at ${r.best.vectorWeight.toFixed(1)} the vector ranking leads, but the keyword ranking still rescues some results.`}</div>
      <div class="row"><button id="tuApply" class="btn primary">Make the desk use vector ${r.best.vectorWeight.toFixed(1)} : keyword ${(1 - r.best.vectorWeight).toFixed(1)} (${fuseName(r.method)})</button>
        <span class="small muted">The Live Desk's hybrid score restarts, so its bar measures the new blend only.</span></div>`;
    $('#tuApply').onclick = async () => {
      const st = await api('hybrid/apply', { method: r.method, vectorWeight: r.best.vectorWeight, normalization: r.normalization });
      S.status = st; renderStatus(); renderDeskFusion();
      Object.assign(FU, st.hybrid); $('#fuMethod').value = FU.method; $('#fuNorm').value = FU.normalization; $('#fuWeight').value = FU.vectorWeight; $('#fuWeight').oninput();
      flash('The desk now fuses with ' + st.hybridName + '. Watch the Hybrid bar on the Live Desk.');
    };
  } catch (e) { $('#tuOut').innerHTML = `<div class="note">${esc(e.message)}</div>`; } finally { $('#tuGo').disabled = false; }
}

// ============================================================ lab
function initLab() {
  // step 1
  $('#embGo').onclick = runEmbed; $('#embText').onkeydown = (e) => e.key === 'Enter' && runEmbed();
  // step 2
  const presets = [
    ['Paraphrase', 'my card was charged twice', 'I was billed two times for the same payment', 'Different words, same situation: the two share almost no words, yet they are close. This is what keyword search misses and vector search is for.'],
    ['Same topic, different problem', 'backups have been failing all week', 'how long do you keep our backups?', 'Two different requests about the same topic, and they score higher than some true paraphrases. Similarity measures topic, so no single threshold separates "same problem" from "same area". That is why the desk votes across several neighbours and checks they agree before answering on its own.'],
    ['Unrelated', 'my card was charged twice', 'the replica set has no primary', 'Nothing in common: the cosine sits near zero.'],
    ['Same word, other meaning', 'the primary went down and writes fail', 'the primary election results were announced', 'A database failover and an election share only the word "primary", yet they score about as close as the real paraphrase "the replica set has no primary" (try it). Small models still lean on words. Larger models separate these better, which is a reason to evaluate on your own data.'],
    ['⚠ Negation', 'I can log in to the console', 'I can\'t log in to the console', 'Opposite meanings, nearly identical vectors (above 0.9). Embeddings capture what a text is about far better than what it asserts. Remember this before automating decisions on similarity alone.'],
  ];

  $('#simPresets').innerHTML = presets.map((p, i) => `<button class="ex" data-i="${i}">${esc(p[0])}</button>`).join('');
  $$('#simPresets .ex').forEach((b) => b.onclick = () => { const p = presets[b.dataset.i]; $('#simA').value = p[1]; $('#simB').value = p[2]; runSim(p[3]); });
  $('#simGo').onclick = () => runSim('');
  // step 3
  $('#mapGo').onclick = locate; $('#mapText').onkeydown = (e) => e.key === 'Enter' && locate();
  $('#mapRefresh').onclick = () => loadMap(true);
  const cv = $('#map'); cv.onmousemove = mapHover; cv.onmouseleave = () => $('#mapTip').classList.add('hidden');
  // step 5
  for (const t of S.cat.teams) $('#bTeam').insertAdjacentHTML('beforeend', `<option>${esc(t.name)}</option>`);
  for (const p of S.cat.plans) $('#bPlan').insertAdjacentHTML('beforeend', `<option>${esc(p)}</option>`);
  $('#bK').oninput = (e) => ($('#bKv').textContent = e.target.value);
  $('#bN').oninput = (e) => ($('#bNv').textContent = e.target.value);
  $('#bGo').onclick = runBuilder;
  // step 6
  $('#frGo').onclick = runFresh;
  renderIndexes();
  runEmbed();
  runSim(presets[0][3]);
}

function divColor(x, max) {
  const t = Math.max(-1, Math.min(1, x / max));
  return t >= 0 ? `rgba(240,106,106,${0.12 + 0.88 * t})` : `rgba(90,162,255,${0.12 + 0.88 * -t})`;
}
function strip(vec, posNeg = true) {
  const max = Math.max(...vec.map(Math.abs)) || 1;
  return `<div class="strip">${vec.map((x, i) => `<div title="dimension ${i}: ${x.toFixed(4)}" style="background:${posNeg ? divColor(x, max) : (x >= 0 ? `rgba(52,199,123,${0.1 + 0.9 * Math.min(1, x / max)})` : `rgba(240,106,106,${0.1 + 0.9 * Math.min(1, -x / max)})`)}"></div>`).join('')}</div>`;
}

async function runEmbed() {
  const r = await api('embed', { text: $('#embText').value });
  $('#embOut').innerHTML = `
    <div class="small muted" style="margin-top:8px">Tokens: the model's vocabulary splits words into known pieces (<span class="token sub">##pieces</span> continue a word)</div>
    <div class="tokens">${r.tokens.map((t) => `<span class="token ${t.startsWith('[') ? 'special' : t.startsWith('##') ? 'sub' : ''}">${esc(t)}</span>`).join('')}</div>
    <div class="small muted">The vector: 384 numbers, one cell each (<span style="color:#f06a6a">red</span> positive, <span style="color:#5aa2ff">blue</span> negative). Hover a cell to read it.</div>
    ${strip(r.vector)}
    <div class="nums">[${r.vector.slice(0, 8).map((x) => x.toFixed(4)).join(', ')}, … ${r.vector.length - 8} more]</div>
    <div class="metrics-row"><span>computed in <b>${fms(r.ms)}</b></span><span>length (L2 norm) <b>${r.norm.toFixed(3)}</b> — normalized, so cosine = dot product</span></div>
    <div class="lesson">No single number means "login" or "phone". Meaning is spread across all 384. That's why you compare whole vectors, never individual dimensions.</div>`;
}

async function runSim(lesson) {
  const r = await api('similarity', { a: $('#simA').value, b: $('#simB').value });
  const pos = Math.max(0, Math.min(1, (r.cosine + 0.2) / 1.2)) * 100;
  // Bands for this model: same-problem ticket pairs sit around 0.54, unrelated ones around 0.17.
  const band = r.cosine >= 0.65 ? 'very similar' : r.cosine >= 0.45 ? 'same topic' : r.cosine >= 0.25 ? 'loosely related' : 'unrelated';
  $('#simOut').innerHTML = `
    <div class="gauge"><div><div class="big">${r.cosine.toFixed(3)}</div><div class="muted small">cosine · ${band}</div></div>
      <div class="gscale"><div class="needle" style="left:${pos}%"></div><div class="lbls"><span>−0.2</span><span>0</span><span>0.3</span><span>0.5</span><span>0.7</span><span>1.0</span></div></div></div>
    <div class="small" style="margin-top:20px">${S.pg
      ? `In pgvector, <code>a &lt;=&gt; b</code> is the cosine <i>distance</i>: <code>1 − ${r.cosine.toFixed(3)} = <b>${(1 - r.cosine).toFixed(3)}</b></code>. Queries <code>ORDER BY</code> it ascending, nearest first; <code>1 - (a &lt;=&gt; b)</code> turns it back into this similarity.`
      : `As a <code>$vectorSearch</code> score on a cosine index: <code>(1 + ${r.cosine.toFixed(3)}) / 2 = <b>${r.score.toFixed(3)}</b></code>`}</div>
    <div class="small muted" style="margin-top:8px">Where the similarity comes from: each cell is <code>a[i] × b[i]</code> (<span class="good">green</span> pulls them together, <span class="bad">red</span> apart). The 384 cells add up to ${r.cosine.toFixed(3)}.</div>
    ${strip(r.contrib, false)}
    ${lesson ? `<div class="lesson">${esc(lesson)}</div>` : ''}`;
}

// ------------------------------------------------------------ map
const mapState = { data: null, q: null, scale: null };
async function loadMap(force) {
  if (mapState.data && !force) return drawMap();
  mapState.data = await api('map');
  const legend = S.cat.teams.map((t) => `<span class="team"><i style="background:${t.color}"></i>${esc(t.name)}</span>`).join('');
  const ex = mapState.data.explained || [];
  $('#mapLegend').innerHTML = legend + `<span class="muted">● ticket · ◆ article · the two axes keep ${Math.round(100 * ((ex[0] || 0) + (ex[1] || 0)))}% of the variance: the picture is a shadow of the real space.</span>`;
  drawMap();
}
function drawMap() {
  const cv = $('#map'), ctx = cv.getContext('2d'); const W = cv.width, H = cv.height, pad = 24;
  const pts = mapState.data?.points || [];
  ctx.clearRect(0, 0, W, H);
  if (!pts.length) return;
  const xs = pts.map((p) => p.x), ys = pts.map((p) => p.y);
  const x0 = Math.min(...xs), x1 = Math.max(...xs), y0 = Math.min(...ys), y1 = Math.max(...ys);
  const sx = (x) => pad + ((x - x0) / (x1 - x0 || 1)) * (W - 2 * pad), sy = (y) => H - pad - ((y - y0) / (y1 - y0 || 1)) * (H - 2 * pad);
  mapState.scale = { sx, sy };
  for (const p of pts) {
    if (p.kind === 'kb') continue;
    ctx.fillStyle = teamColor(p.team) + 'b0';
    ctx.beginPath(); ctx.arc(sx(p.x), sy(p.y), 3.2, 0, 7); ctx.fill();
  }
  for (const p of pts) {
    if (p.kind !== 'kb') continue;
    const X = sx(p.x), Y = sy(p.y);
    ctx.fillStyle = teamColor(p.team); ctx.strokeStyle = '#fff'; ctx.lineWidth = 1.2;
    ctx.beginPath(); ctx.moveTo(X, Y - 7); ctx.lineTo(X + 7, Y); ctx.lineTo(X, Y + 7); ctx.lineTo(X - 7, Y); ctx.closePath(); ctx.fill(); ctx.stroke();
  }
  if (mapState.q) {
    const q = mapState.q, X = sx(q.x), Y = sy(q.y);
    ctx.strokeStyle = getComputedStyle(document.body).getPropertyValue('--fg'); ctx.lineWidth = 1.4; ctx.setLineDash([4, 3]);
    for (const n of q.neighbors) {
      const p = pts.find((p) => String(p.id) === String(n.id)); if (!p) continue;
      ctx.beginPath(); ctx.moveTo(X, Y); ctx.lineTo(sx(p.x), sy(p.y)); ctx.stroke();
    }
    ctx.setLineDash([]);
    ctx.fillStyle = '#ffd34d'; ctx.strokeStyle = '#000'; ctx.lineWidth = 1.5;
    star(ctx, X, Y, 11, 5); ctx.fill(); ctx.stroke();
  }
}
function star(ctx, x, y, r, n) { ctx.beginPath(); for (let i = 0; i < 2 * n; i++) { const a = (i * Math.PI) / n - Math.PI / 2, rr = i % 2 ? r / 2.3 : r; ctx.lineTo(x + rr * Math.cos(a), y + rr * Math.sin(a)); } ctx.closePath(); }
function mapHover(e) {
  if (!mapState.scale) return;
  const cv = $('#map'), b = cv.getBoundingClientRect(); const mx = ((e.clientX - b.left) * cv.width) / b.width, my = ((e.clientY - b.top) * cv.height) / b.height;
  let best = null, bd = 100;
  for (const p of mapState.data.points) { const d = Math.hypot(mapState.scale.sx(p.x) - mx, mapState.scale.sy(p.y) - my); if (d < bd) { bd = d; best = p; } }
  const tip = $('#mapTip');
  if (!best || bd > 12) return tip.classList.add('hidden');
  tip.innerHTML = `${best.kind === 'kb' ? '◆ article' : '● #' + esc(best.id)} · ${teamChip(best.team)}<br>${esc(best.label)}`;
  tip.style.left = (e.clientX - b.left + 14) + 'px'; tip.style.top = (e.clientY - b.top + 10) + 'px'; tip.classList.remove('hidden');
}
async function locate() {
  const text = $('#mapText').value.trim(); if (!text) return;
  if (!mapState.data) await loadMap();
  mapState.q = await api('map/locate', { text });
  drawMap();
  $('#mapNb').innerHTML = `<div class="small muted" style="margin-top:6px">Truly nearest (full 384-D cosine):</div>` + mapState.q.neighbors.map((n) => `<div class="nb"><span class="muted">${typeof n.id === 'string' ? '◆' : '#' + n.id}</span><span class="t">${esc(n.title)} ${teamChip(n.team)}</span><span class="small">cos ${f2(2 * n.score - 1)}</span></div>`).join('');
}

// ------------------------------------------------------------ indexes
function renderIndexes() {
  const s = S.status; const idx = s?.indexes || [];
  if (!idx.length) {
    $('#idxOut').innerHTML = `<div class="note">${esc(s?.searchErr || 'No search indexes reported yet.')}</div>`;
    return;
  }
  const isVec = (i) => i.type === 'vectorSearch' || String(i.type).startsWith('vector');
  $('#idxOut').innerHTML = (S.pg ? codeBlock(`CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE supportsim.tickets (
  id         bigint PRIMARY KEY,
  subject    text, body text, customer text, plan text, status text, team text,
  created_at timestamptz,
  embedding  vector(384),             -- all-MiniLM-L6-v2: one row, one vector
  tsv        tsvector GENERATED ALWAYS AS
               (to_tsvector('english', coalesce(subject, '') || ' ' || coalesce(body, ''))) STORED
);`) : '') + idx.filter(isVec).concat(idx.filter((i) => !isVec(i))).map((i) => `
    <div style="margin:10px 0"><b>${esc(i.name)}</b> <span class="muted small">on ${esc(i.collection)} · ${isVec(i) ? 'vector index' : 'full-text index'}</span> <span class="pill ${esc(i.status)}">${esc(i.status)}</span>${i.queryable ? ' <span class="small good">queryable</span>' : ''}
    ${codeBlock(i.shell)}</div>`).join('') + (S.pg
      ? `<div class="lesson">Check them yourself in psql: <code>\\di+ supportsim.*</code>. Without the <code>vector</code> extension the <code>CREATE TABLE</code> fails on the column type, which is how this app knows to fall back: it stores <code>real[]</code> and scans in the app.</div>`
      : `<div class="lesson">Check them yourself: <code>db.tickets.getSearchIndexes()</code>. On a cluster without mongot that command fails, which is how this app knows to fall back.</div>`);
  bindCopy();
}

// ------------------------------------------------------------ builder
async function runBuilder() {
  const body = { query: $('#bQuery').value, collection: $('#bColl').value, k: Number($('#bK').value), numCandidates: Number($('#bN').value), exact: $('#bExact').checked, filter: {} };
  if ($('#bTeam').value) body.filter.team = $('#bTeam').value;
  if ($('#bColl').value === 'tickets') {
    if ($('#bPlan').value) body.filter.plan = $('#bPlan').value;
    if ($('#bStatus').value) body.filter.status = [$('#bStatus').value];
  }
  $('#bOut').innerHTML = '<div class="muted">Running…</div>';
  const r = await api('builder', body);
  const exactIDs = new Set((r.exactResult?.hits || []).map((h) => String(h.id)));
  const langs = codeSamples(body, r);
  $('#bOut').innerHTML = `
    <div class="metrics-row"><span>embed <b>${fms(r.embedMs)}</b></span><span>${L().vec} <b>${fms(r.ms)}</b> <span class="muted small">(${esc(engName(r.engine))})</span></span>
      ${body.exact ? '<span>exact search: recall is 100% by definition</span>' : `<span>recall@${body.k} <b class="${r.recall >= 0.99 ? 'good' : r.recall >= 0.8 ? '' : 'bad'}">${Math.round(100 * r.recall)}%</b> <span class="muted small">vs exact (${fms(r.exactResult?.ms)})</span></span>`}</div>
    ${r.error ? `<div class="note">${esc(r.error)}</div>` : ''}
    ${(r.hits || []).map((h, i) => `<div class="nb"><span class="muted">${i + 1}</span><span class="t">${esc(h.title)} ${teamChip(h.team)} ${h.status ? `<span class="status ${h.status}">${h.status.replace('_', ' ')}</span>` : ''} ${!body.exact && !exactIDs.has(String(h.id)) ? '<span class="bad small">not in exact top-k</span>' : ''}</span><span class="small">${S.pg ? `distance ${f2(1 - cosOf(h))} · cos ${f2(cosOf(h))}` : `score ${f2(h.score)} · cos ${f2(cosOf(h))}`}</span></div>`).join('') || '<div class="muted small">no results — a filter with no matching documents?</div>'}
    ${r.note ? `<div class="note">${esc(r.note)}</div>` : ''}
    <div class="subtabs" style="margin-top:12px">${Object.keys(langs).map((k, i) => `<button class="btn ${i === 0 ? 'on' : ''}" data-l="${k}">${k}</button>`).join('')}</div>
    <div id="langOut">${codeBlock(Object.values(langs)[0])}</div>
    <div class="lesson">${S.pg
      ? 'Try it: drop <b>hnsw.ef_search</b> to its minimum (here it never goes below limit) and watch recall fall; filter by a team: <code>hnsw.iterative_scan</code> keeps the walk going until enough rows pass the WHERE; tick <b>exact</b> and compare the latency. <code>enable_seqscan = off</code> is a lab device: on a table this small the planner would scan every row anyway, and ef_search would change nothing. Leave it out of your app.'
      : 'Try it: drop <b>numCandidates</b> to 5 and watch recall fall; filter by a team and see results stay on-topic; tick <b>exact</b> and compare the latency.'}</div>`;
  $$('#bOut .subtabs button').forEach((b) => b.onclick = () => { $$('#bOut .subtabs button').forEach((x) => x.classList.toggle('on', x === b)); $('#langOut').innerHTML = codeBlock(langs[b.dataset.l]); bindCopy(); });
  bindCopy();
}

function codeSamples(b, r) {
  if (S.pg) return pgCodeSamples(b, r);
  const idx = b.collection === 'tickets' ? 'tickets_vector' : 'kb_vector';
  const filt = Object.entries(b.filter).map(([k, v]) => `${k}: ${Array.isArray(v) ? JSON.stringify(v[0]) : JSON.stringify(v)}`);
  const fjs = filt.length ? `,\n      filter: { ${filt.join(', ')} }` : '';
  const fpy = filt.length ? `,\n        "filter": {${Object.entries(b.filter).map(([k, v]) => `"${k}": ${JSON.stringify(Array.isArray(v) ? v[0] : v)}`).join(', ')}}` : '';
  const cand = b.exact ? 'exact: true' : `numCandidates: ${b.numCandidates}`;
  const candPy = b.exact ? '"exact": True' : `"numCandidates": ${b.numCandidates}`;
  const title = b.collection === 'tickets' ? 'subject' : 'title';
  return {
    mongosh: r.pipeline,
    Python: `# pip install pymongo sentence-transformers
from pymongo import MongoClient
from sentence_transformers import SentenceTransformer

model = SentenceTransformer("sentence-transformers/all-MiniLM-L6-v2")   # same model as the documents
db = MongoClient(MONGO_URI)["supportsim"]

query_vector = model.encode(${JSON.stringify(b.query)}, normalize_embeddings=True).tolist()
pipeline = [
    {"$vectorSearch": {
        "index": "${idx}",
        "path": "embedding",
        "queryVector": query_vector,
        ${candPy},
        "limit": ${b.k}${fpy}
    }},
    {"$project": {"${title}": 1, "team": 1, "score": {"$meta": "vectorSearchScore"}}},
]
for doc in db.${b.collection}.aggregate(pipeline):
    print(round(doc["score"], 3), doc["${title}"])

# Storing a document is just as plain: compute the vector once, save it in a field.
# db.${b.collection}.insert_one({"${title}": text, "embedding": model.encode(text, normalize_embeddings=True).tolist()})`,
    'Node.js': `// npm i mongodb @huggingface/transformers
import { MongoClient } from "mongodb";
import { pipeline } from "@huggingface/transformers";

const embed = await pipeline("feature-extraction", "Xenova/all-MiniLM-L6-v2");
const out = await embed(${JSON.stringify(b.query)}, { pooling: "mean", normalize: true });
const queryVector = Array.from(out.data);

const db = new MongoClient(process.env.MONGO_URI).db("supportsim");
const docs = await db.collection("${b.collection}").aggregate([
  { $vectorSearch: {
      index: "${idx}", path: "embedding", queryVector,
      ${cand}, limit: ${b.k}${fjs}
  } },
  { $project: { ${title}: 1, team: 1, score: { $meta: "vectorSearchScore" } } },
]).toArray();
console.table(docs);`,
    Go: `// go get go.mongodb.org/mongo-driver/v2
// queryVector comes from your embedding model ([]float32, 384 values for MiniLM).
pipeline := mongo.Pipeline{
	{{Key: "$vectorSearch", Value: bson.D{
		{Key: "index", Value: "${idx}"},
		{Key: "path", Value: "embedding"},
		{Key: "queryVector", Value: queryVector},
		${b.exact ? '{Key: "exact", Value: true},' : `{Key: "numCandidates", Value: ${b.numCandidates}},`}
		{Key: "limit", Value: ${b.k}},${filt.length ? `
		{Key: "filter", Value: bson.D{${Object.entries(b.filter).map(([k, v]) => `{Key: "${k}", Value: ${JSON.stringify(Array.isArray(v) ? v[0] : v)}}`).join(', ')}}},` : ''}
	}}},
	{{Key: "$project", Value: bson.D{{Key: "${title}", Value: 1}, {Key: "score", Value: bson.D{{Key: "$meta", Value: "vectorSearchScore"}}}}}},
}
cur, err := client.Database("supportsim").Collection("${b.collection}").Aggregate(ctx, pipeline)`,
  };
}

// The same query from an application, for PostgreSQL. The vector goes over the wire
// as a parameter; the pgvector client packages turn a list or array into one.
function pgCodeSamples(b, r) {
  const table = b.collection === 'tickets' ? 'tickets' : 'kb_articles';
  const title = b.collection === 'tickets' ? 'subject' : 'title';
  const conds = [], params = [];
  for (const [k, val] of Object.entries(b.filter)) { params.push(Array.isArray(val) ? val[0] : val); conds.push(`${k} = $${params.length + 1}`); }
  const sets = b.exact ? ['SET LOCAL enable_indexscan = off'] : [`SET LOCAL hnsw.ef_search = ${Math.max(b.numCandidates, b.k)}`].concat(conds.length ? ['SET LOCAL hnsw.iterative_scan = relaxed_order'] : []);
  // ph(n) spells placeholder n the way the driver wants it.
  const sql = (ph) => `SELECT id, ${title}, team, 1 - (embedding <=> ${ph(1)}) AS similarity
  FROM supportsim.${table}${conds.length ? '\n  WHERE ' + conds.map((c, i) => c.replace(/\$\d+/, ph(i + 2))).join(' AND ') : ''}
  ORDER BY embedding <=> ${ph(1)}
  LIMIT ${b.k}`;
  const lit = params.map((p) => ', ' + JSON.stringify(p)).join('');
  const tq = '"' + '""';
  return {
    SQL: r.pipeline,
    Python: `# pip install "psycopg[binary]" pgvector sentence-transformers
import psycopg
from pgvector.psycopg import register_vector
from sentence_transformers import SentenceTransformer

model = SentenceTransformer("sentence-transformers/all-MiniLM-L6-v2")   # same model as the rows
qv = model.encode(${JSON.stringify(b.query)}, normalize_embeddings=True)

with psycopg.connect(POSTGRES_DSN, dbname="supportsim") as conn:
    register_vector(conn)                      # numpy arrays <-> vector
    with conn.transaction():                   # SET LOCAL lasts until COMMIT
${sets.map((x) => `        conn.execute("${x}")`).join('\n')}
        rows = conn.execute(${tq}
${sql(() => '%s').replace(/^/gm, '            ')}
        ${tq}, (qv${lit}, qv)).fetchall()
    for row in rows:
        print(round(row[3], 3), row[1])

# Storing a row is just as plain: compute the vector once, put it in the column.
# conn.execute("INSERT INTO supportsim.${table} (${title}, embedding) VALUES (%s, %s)",
#              (text, model.encode(text, normalize_embeddings=True)))`,
    'Node.js': `// npm i pg pgvector @huggingface/transformers
import pg from "pg";
import pgvector from "pgvector/pg";
import { pipeline } from "@huggingface/transformers";

const embed = await pipeline("feature-extraction", "Xenova/all-MiniLM-L6-v2");
const out = await embed(${JSON.stringify(b.query)}, { pooling: "mean", normalize: true });
const qv = pgvector.toSql(Array.from(out.data));

const client = new pg.Client({ connectionString: process.env.POSTGRES_DSN });
await client.connect();
await pgvector.registerTypes(client);
await client.query("BEGIN");
${sets.map((x) => `await client.query("${x}");`).join('\n')}
const { rows } = await client.query(\`
${sql((n) => '$' + n)}\`, [qv${lit}]);
await client.query("COMMIT");
console.table(rows);`,
    Go: `// go get github.com/jackc/pgx/v5 github.com/pgvector/pgvector-go
// queryVector comes from your embedding model ([]float32, 384 values for MiniLM).
tx, err := pool.Begin(ctx)
if err != nil {
	return err
}
defer tx.Rollback(ctx)
${sets.map((x) => `tx.Exec(ctx, "${x}")`).join('\n')}
rows, err := tx.Query(ctx, \`
${sql((n) => '$' + n)}\`,
	pgvector.NewVector(queryVector)${lit})`,
  };
}

// ------------------------------------------------------------ freshness
async function runFresh() {
  $('#frGo').disabled = true; $('#frOut').innerHTML = '<div class="muted">Inserting and polling…</div>';
  try {
    const r = await api('freshness', {});
    if (r.engine === 'app') {
      $('#frOut').innerHTML = S.pg
        ? `<div class="note">No pgvector: the in-app scan sees the row immediately. With pgvector the index is part of the transaction, and this step shows who sees the row when.</div>`
        : `<div class="note">No mongot: the in-app scan sees the document immediately, because there is no separate index to update. With mongot there is, and this step measures the gap.</div>`;
      return;
    }
    if (S.pg) {
      $('#frOut').innerHTML = `
        <div class="metrics-row"><span>INSERT of the probe ticket: <b>${fms(r.insertMs)}</b>, inside an open transaction</span></div>
        <table class="idx"><tr><th>when</th><th>who asks</th><th>found by the vector search?</th><th>query</th></tr>
        ${(r.steps || []).map((x) => `<tr><td>${esc(x.when)}</td><td>${esc(x.session)}</td><td>${x.found ? '<b class="good">yes</b>' : '<b class="bad">no</b>'}</td><td>${fms(x.ms)}</td></tr>`).join('')}</table>
        <div class="lesson">No lag to measure, because there is no second system to catch up. The HNSW index was updated by the INSERT itself, under the same MVCC rules as the row: the writer sees its uncommitted ticket, nobody else does, and everybody does from the moment of COMMIT. Read-your-own-write just works, and a rolled-back insert never shows up in a search. The price is paid at write time: each insert walks the graph to link itself in.</div>
        <details><summary>the query each session ran</summary>${codeBlock(r.pipeline)}</details>`;
      bindCopy();
      return;
    }
    const total = r.insertMs + r.foundMs, w = (x) => Math.max(2, (100 * x) / total);
    $('#frOut').innerHTML = `
      <div class="timeline"><div class="seg" style="width:${w(r.insertMs)}%;background:var(--blue);border-radius:6px 0 0 6px"></div><div class="seg" style="width:${w(r.foundMs)}%;background:var(--accent);border-radius:0 6px 6px 0"></div></div>
      <div class="metrics-row"><span><span style="color:var(--blue)">■</span> insert acknowledged by mongod: <b>${fms(r.insertMs)}</b></span><span><span style="color:var(--accent)">■</span> then findable by $vectorSearch after <b>${fms(r.foundMs)}</b> <span class="muted small">(${r.attempts} polls)</span></span></div>
      ${r.found ? '' : '<div class="note">Not found within 30 s — is mongot healthy?</div>'}
      <div class="lesson">That second segment is mongot reading the insert from the change stream and adding it to its index. It's usually small, but it is never zero, so a $vectorSearch right after your own write can miss it.</div>
      <details><summary>the polling query</summary>${codeBlock(r.pipeline)}</details>`;
    bindCopy();
  } finally { $('#frGo').disabled = false; }
}

// ============================================================ under the hood
function renderHood() {
  const s = S.status; if (!s) return;
  if (S.pg) return renderHoodPG(s);
  const on = s.mongot;
  $('#arch').innerHTML = `<svg viewBox="0 0 640 300" width="100%" role="img" aria-label="Query path">
    <defs><marker id="ar" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="var(--accent)"/></marker></defs>
    <rect x="10" y="20" width="170" height="92" rx="12" fill="var(--card2)" stroke="var(--line)"/>
    <text x="95" y="46" text-anchor="middle" font-weight="700">this app</text>
    <text x="95" y="68" text-anchor="middle" class="muted" font-size="12">text → all-MiniLM-L6-v2</text>
    <text x="95" y="86" text-anchor="middle" class="muted" font-size="12">→ 384-d query vector</text>
    <text x="95" y="104" text-anchor="middle" class="muted" font-size="11">${fms(S.metrics?.embedLat?.p50)} p50</text>
    <rect x="235" y="20" width="170" height="92" rx="12" fill="var(--card2)" stroke="var(--line)"/>
    <text x="320" y="46" text-anchor="middle" font-weight="700">mongod</text>
    <text x="320" y="68" text-anchor="middle" class="muted" font-size="12">aggregate([$vectorSearch…])</text>
    <text x="320" y="86" text-anchor="middle" class="muted" font-size="12">loads matched documents</text>
    <text x="320" y="104" text-anchor="middle" class="muted" font-size="11">${esc(s.target)} · ${esc(s.server?.version || '')}</text>
    <rect x="460" y="20" width="170" height="92" rx="12" fill="${on ? 'color-mix(in srgb, var(--accent) 16%, var(--card2))' : 'var(--card2)'}" stroke="${on ? 'var(--accent)' : 'var(--bad)'}" stroke-dasharray="${on ? '' : '5 4'}"/>
    <text x="545" y="46" text-anchor="middle" font-weight="700">mongot</text>
    <text x="545" y="68" text-anchor="middle" class="muted" font-size="12">Lucene HNSW graph</text>
    <text x="545" y="86" text-anchor="middle" class="muted" font-size="12">returns _ids + scores</text>
    <text x="545" y="104" text-anchor="middle" font-size="11" fill="${on ? 'var(--good)' : 'var(--bad)'}">${on ? 'serving' : 'not available'}</text>
    <line x1="182" y1="56" x2="232" y2="56" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <line x1="407" y1="56" x2="457" y2="56" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <text x="432" y="48" text-anchor="middle" class="muted" font-size="10">gRPC :27028</text>
    <line x1="457" y1="84" x2="407" y2="84" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <line x1="232" y1="84" x2="182" y2="84" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <path d="M320 112 C320 190, 545 190, 545 116" fill="none" stroke="var(--violet)" stroke-width="2" stroke-dasharray="6 4" marker-end="url(#ar)"/>
    <text x="432" y="186" text-anchor="middle" font-size="12" fill="var(--violet)">change stream: every insert/update/delete</text>
    <text x="432" y="204" text-anchor="middle" class="muted" font-size="11">keeps the index current (eventually consistent, see Lab step 6)</text>
    ${s.server?.topology === 'sharded' ? `<text x="320" y="228" text-anchor="middle" font-size="12" fill="var(--accent)">Sharded: mongos sends the query to every shard holding tickets (${s.server.shards || '?'}); each asks its own mongot, and mongos merges by score.</text>` : ''}
    <text x="320" y="246" text-anchor="middle" class="muted" font-size="12">Writes never touch mongot: inserts go to mongod as usual. mongot follows behind.</text>
    <text x="320" y="266" text-anchor="middle" class="muted" font-size="12">If mongot stops, $vectorSearch and $search fail; every other query keeps working.</text>
  </svg>`;
  const idx = s.indexes || [];
  $('#hoodIdx').innerHTML = idx.length ? `<table class="idx"><tr><th>index</th><th>collection</th><th>type</th><th>status</th></tr>${idx.map((i) => `<tr><td><code>${esc(i.name)}</code></td><td>${esc(i.collection)}</td><td>${esc(i.type)}</td><td><span class="pill ${esc(i.status)}">${esc(i.status)}</span></td></tr>`).join('')}</table>
      <p class="muted small">${s.ticketDocs} tickets and ${s.kbDocs} articles, each with a 384-number <code>embedding</code>. Indexes are rebuilt by mongot from the data; they are not part of a backup.</p>`
    : `<div class="note">${esc(s.searchErr || 'none')}</div>`;
  const d = S.lastDecision || S.recent[0];
  if (d) {
    const t = d.ticket;
    $('#hoodDoc').textContent = `{
  _id: ${t.id},
  subject: ${JSON.stringify(t.subject)},
  body: ${JSON.stringify(t.body.slice(0, 80) + (t.body.length > 80 ? '…' : ''))},
  customer: ${JSON.stringify(t.customer)}, plan: ${JSON.stringify(t.plan)},
  createdAt: ISODate("${t.createdAt}"),
  status: ${JSON.stringify(t.status)}, team: ${JSON.stringify(t.team)},
  truth: { archetype: ${JSON.stringify(t.truth.archetype)}, team: ${JSON.stringify(t.truth.team)}, kb: ${JSON.stringify(t.truth.kb)} },
  embedding: [ /* 384 doubles from all-MiniLM-L6-v2 */ ]
}`;
  }
  $('#hoodCfg').innerHTML = `<p class="muted small">DBCanvas did this when the MongoDB frame had <b>Vector search</b> ticked: the same four things the Percona Operator does for <code>spec.search</code>.</p>
    <div class="small"><b>1.</b> every mongod (and mongos) got, before first start:</div>
    ${codeBlock(`setParameter:
  mongotHost: <first-member>:27028
  searchIndexManagementHostAndPort: <first-member>:27028
  skipAuthenticationToSearchIndexManagementServer: false
  skipAuthenticationToMongot: false
  searchTLSMode: disabled
  useGrpcForSearch: true`)}
    <div class="small"><b>2.</b> a user for mongot: <code>{ user: "mongot", roles: ["searchCoordinator"] }</code></div>
    <div class="small"><b>3.</b> <code>percona-search-mongodb</code> installed from the <code>ps4m</code> repository on the first member</div>
    <div class="small"><b>4.</b> <code>/etc/mongot/mongot.yml</code>: syncSource = the replica set's members, gRPC on :27028, health on :8080</div>
    <div class="lesson">Check it on the first member: <code>curl localhost:8080/health</code> → <code>{"status":"SERVING"}</code>, and <code>journalctl -u mongot</code> or <code>/var/log/mongot/mongot.log</code>.</div>`;
  bindCopy();
  renderIndexes();
}

function renderHoodPG(s) {
  const on = s.mongot;
  $('#arch').innerHTML = `<svg viewBox="0 0 640 300" width="100%" role="img" aria-label="Query path">
    <defs><marker id="ar" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="var(--accent)"/></marker></defs>
    <rect x="10" y="20" width="170" height="92" rx="12" fill="var(--card2)" stroke="var(--line)"/>
    <text x="95" y="46" text-anchor="middle" font-weight="700">this app</text>
    <text x="95" y="68" text-anchor="middle" class="muted" font-size="12">text → all-MiniLM-L6-v2</text>
    <text x="95" y="86" text-anchor="middle" class="muted" font-size="12">→ 384-d query vector</text>
    <text x="95" y="104" text-anchor="middle" class="muted" font-size="11">${fms(S.metrics?.embedLat?.p50)} p50</text>
    <rect x="260" y="20" width="370" height="150" rx="12" fill="${on ? 'color-mix(in srgb, var(--accent) 10%, var(--card2))' : 'var(--card2)'}" stroke="${on ? 'var(--accent)' : 'var(--bad)'}" stroke-dasharray="${on ? '' : '5 4'}"/>
    <text x="445" y="44" text-anchor="middle" font-weight="700">PostgreSQL backend · ${esc(s.target || '')} ${esc(s.server?.version || '')}</text>
    <rect x="276" y="58" width="160" height="96" rx="10" fill="var(--card2)" stroke="var(--line)"/>
    <text x="356" y="80" text-anchor="middle" font-size="12" font-weight="700">planner</text>
    <text x="356" y="100" text-anchor="middle" class="muted" font-size="11">ORDER BY embedding &lt;=&gt; $1</text>
    <text x="356" y="118" text-anchor="middle" class="muted" font-size="11">LIMIT k: Index Scan</text>
    <text x="356" y="136" text-anchor="middle" class="muted" font-size="11">or Seq Scan + Sort</text>
    <rect x="456" y="58" width="160" height="96" rx="10" fill="var(--card2)" stroke="var(--line)"/>
    <text x="536" y="80" text-anchor="middle" font-size="12" font-weight="700">pgvector</text>
    <text x="536" y="100" text-anchor="middle" class="muted" font-size="11">HNSW graph in 8 kB pages</text>
    <text x="536" y="118" text-anchor="middle" class="muted" font-size="11">shared_buffers, WAL</text>
    <text x="536" y="136" text-anchor="middle" font-size="11" fill="${on ? 'var(--good)' : 'var(--bad)'}">${on ? 'extension ' + esc(s.server?.setName || '') : 'not installed'}</text>
    <line x1="182" y1="66" x2="257" y2="66" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <text x="220" y="58" text-anchor="middle" class="muted" font-size="10">SQL :5432</text>
    <line x1="257" y1="96" x2="182" y2="96" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <text x="220" y="112" text-anchor="middle" class="muted" font-size="10">rows</text>
    <line x1="438" y1="106" x2="453" y2="106" stroke="var(--accent)" stroke-width="2" marker-end="url(#ar)"/>
    <text x="320" y="200" text-anchor="middle" font-size="12" fill="var(--violet)">One process, one transaction: the INSERT updates the table, the HNSW index and the GIN index together.</text>
    <text x="320" y="222" text-anchor="middle" class="muted" font-size="12">Indexes are WAL-logged like any other, so standbys have them and backups contain them.</text>
    <text x="320" y="244" text-anchor="middle" class="muted" font-size="12">A vector query can join, filter and aggregate like any other SELECT.</text>
    <text x="320" y="266" text-anchor="middle" class="muted" font-size="12">If pgvector is missing, CREATE EXTENSION fails and this app scans in its own memory instead.</text>
  </svg>`;
  const idx = s.indexes || [];
  $('#hoodIdx').innerHTML = idx.length ? `<table class="idx"><tr><th>index</th><th>table</th><th>type</th><th>status</th></tr>${idx.map((i) => `<tr><td><code>${esc(i.name)}</code></td><td>${esc(i.collection)}</td><td>${esc(i.type)}</td><td><span class="pill ${esc(i.status)}">${esc(i.status)}</span></td></tr>`).join('')}</table>
      <p class="muted small">${s.ticketDocs} tickets and ${s.kbDocs} articles, each with an <code>embedding vector(384)</code>. Sizes, scans and cache hits are in Index Workshop step 4.</p>`
    : `<div class="note">${esc(s.searchErr || 'none')}</div>`;
  const d = S.lastDecision || S.recent[0];
  if (d) {
    const t = d.ticket;
    $('#hoodDoc').textContent = `SELECT * FROM supportsim.tickets WHERE id = ${t.id};

id          | ${t.id}
subject     | ${t.subject}
body        | ${t.body.slice(0, 70) + (t.body.length > 70 ? '…' : '')}
customer    | ${t.customer}
plan        | ${t.plan}
created_at  | ${t.createdAt}
status      | ${t.status}
team        | ${t.team}
truth_*     | ${t.truth.archetype} · ${t.truth.team} · ${t.truth.kb}
embedding   | [ 384 floats from all-MiniLM-L6-v2 ]   -- vector(384), about 1.5 kB
tsv         | ( lexemes of subject and body )        -- GENERATED ALWAYS … STORED`;
  }
  $('#hoodCfg').innerHTML = `<p class="muted small">DBCanvas did this when the PostgreSQL node had <b>pgvector</b> ticked:</p>
    <div class="small"><b>1.</b> the pgvector package for this PostgreSQL major, from the same repository as the server: <code>percona-pgvector_&lt;major&gt;</code> (EL) or <code>percona-postgresql-&lt;major&gt;-pgvector</code> (Debian/Ubuntu) for Percona Distribution for PostgreSQL, PGDG's <code>pgvector_&lt;major&gt;</code> for community PostgreSQL</div>
    <div class="small"><b>2.</b> nothing in <code>postgresql.conf</code>: pgvector needs no <code>shared_preload_libraries</code> entry. The library loads the first time a session touches a vector</div>
    <div class="small"><b>3.</b> on Kubernetes, the Percona Operator's built-in extension instead:</div>
    ${codeBlock(`spec:
  extensions:
    builtin:
      pgvector: true      # the operator runs CREATE EXTENSION in every database`)}
    <div class="small"><b>4.</b> this app then ran, in its own <code>supportsim</code> database:</div>
    ${codeBlock(`CREATE EXTENSION IF NOT EXISTS vector;
CREATE INDEX ... USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ... USING gin (tsv);`)}
    <div class="lesson">Check it in psql: <code>\\dx vector</code> shows the installed version, <code>SELECT * FROM pg_available_extensions WHERE name = 'vector'</code> what the package provides.</div>`;
  bindCopy();
  renderIndexes();
}

// ============================================================ index workshop
const WS = { inited: false, timer: null, pgTimer: null, variants: [] };
const fbytes = (b) => (!b ? null : b < 1024 ? Math.round(b) + ' B' : b < 1048576 ? (b / 1024).toFixed(1) + ' KB' : (b / 1048576).toFixed(1) + ' MB');

async function openWorkshop() {
  if (!WS.inited) {
    WS.inited = true;
    $('#wsK').oninput = (e) => ($('#wsKv').textContent = e.target.value);
    $('#wsN').oninput = (e) => ($('#wsNv').textContent = e.target.value);
    $('#exK').oninput = (e) => ($('#exKv').textContent = e.target.value);
    $('#exN').oninput = (e) => ($('#exNv').textContent = e.target.value);
    $('#wsE').oninput = (e) => ($('#wsEv').textContent = e.target.value);
    $('#wsP').oninput = (e) => ($('#wsPv').textContent = e.target.value);
    $('#exP').oninput = (e) => ($('#exPv').textContent = e.target.value);
    if (S.pg) $('#exIndex').options[0].textContent = "tickets (the desk's table)";
    $('#wsBuild').onclick = async () => { try { await api('workshop/build', {}); } catch (e) { flash(e.message); } pollWorkshop(); };
    $('#wsBench').onclick = runBench;
    $('#exGo').onclick = runExplain;
    for (const t of S.cat.teams) $('#exTeam').insertAdjacentHTML('beforeend', `<option>${esc(t.name)}</option>`);
  }
  await pollWorkshop();
  loadPlayground();
  loadMetrics();
  clearInterval(WS.timer);
  WS.timer = setInterval(loadMetrics, 5000);
}
function closeWorkshop() { clearInterval(WS.timer); clearInterval(WS.pgTimer); WS.pgTimer = null; }

async function pollWorkshop() {
  const w = await api('workshop');
  WS.variants = w.variants;
  const sel = $('#exIndex');
  if (sel.options.length === 1) for (const v of w.variants) sel.insertAdjacentHTML('beforeend', `<option value="${v.name}">${v.name} · ${esc(S.pg ? v.storage : v.similarity)}${v.quantization ? ' · ' + v.quantization : ''}${S.pg ? '' : ' · ' + esc(v.storage)}</option>`);
  const st = w.state;
  const busy = st.phase === 'copying' || st.phase === 'indexing';
  $('#wsBuild').textContent = st.phase === 'ready' ? 'Rebuild the variant indexes' : 'Build the variant indexes';
  $('#wsBuild').disabled = busy || !S.status?.mongot;
  $('#wsBench').disabled = st.phase !== 'ready';
  $('#wsState').innerHTML = !S.status?.mongot ? (S.pg ? '<span class="bad">needs pgvector: turn it on for this PostgreSQL</span>' : '<span class="bad">needs mongot: turn on Vector search for this cluster</span>')
    : st.phase === 'idle' || !st.phase ? (S.pg ? 'Not built yet. Takes 10–30 s: nine tables, eight CREATE INDEX statements, each one timed.' : 'Not built yet. Takes about a minute: mongot builds nine indexes at once.')
      : `<b>${esc(st.phase)}</b> ${esc(st.detail)}${st.took ? ` · ${st.took.toFixed(0)} s` : ''}`;
  if (busy) setTimeout(pollWorkshop, 1500);
  return st;
}

async function runBench() {
  $('#wsBench').disabled = true; $('#wsOut').innerHTML = '<div class="muted">Asking nine indexes the same 60 questions…</div>';
  try {
    const r = await api('workshop/bench', { k: Number($('#wsK').value), numCandidates: Number($('#wsN').value), efSearch: Number($('#wsE').value), probes: Number($('#wsP').value), queries: 60 });
    if (S.pg) return renderBenchPG(r);
    const base = r.rows[0];
    const pctb = (x, color) => `<span class="minibar"><div style="width:${Math.round(x * 100)}%;background:${color}"></div></span>${(x * 100).toFixed(1)}%`;
    const rows = r.rows.map((x, i) => {
      const bad = x.recall < 0.9 || x.accuracy < base.accuracy - 0.05;
      const rc = x.recall >= 0.99 ? 'var(--good)' : x.recall >= 0.9 ? 'var(--warn)' : 'var(--bad)';
      return `<tr class="${i === 0 ? 'base' : bad ? 'badrow' : ''}">
        <td><code>${esc(x.name)}</code><div class="small muted">${esc(x.similarity)}${x.quantization ? ' · quantization: ' + esc(x.quantization) : ''}</div></td>
        <td class="small">${esc(x.storage)}</td>
        <td>${x.error ? `<span class="bad small">${esc(x.error)}</span>` : pctb(x.recall, rc)}</td>
        <td>${x.error ? '' : pctb(x.accuracy, 'var(--accent)')}</td>
        <td>${fms(x.p50Ms)}</td>
        <td>${f2(x.topScore)}</td>
        <td>${fbytes(x.fieldBytes) || '—'}</td>
        <td>${fbytes(x.indexBytes) || '<span class="muted small" title="mongot refreshes index sizes every few minutes">pending</span>'}</td>
      </tr><tr class="lessonrow"><td colspan="8">${esc(x.lesson)}${x.meanLength ? ` <b>Mean length of the vectors it returned: ${x.meanLength.toFixed(2)}.</b>` : ''}
        <details><summary>definition</summary>${codeBlock(x.shell)}</details></td></tr>`;
    }).join('');
    const by = (n) => r.rows.find((x) => x.name === n);
    const bin = by('v_binary'), dot = by('v_raw_dotproduct');
    $('#wsOut').innerHTML = `
      <div class="metrics-row"><span><b>${r.queries}</b> questions × <b>${r.rows.length}</b> indexes over <b>${r.docs}</b> tickets</span><span>limit ${r.k} · numCandidates ${r.numCandidates}</span><span class="muted small">${r.seconds.toFixed(1)} s · "top score" is the best score for “${esc(r.example)}”</span></div>
      <div style="overflow-x:auto"><table class="bench"><tr><th>index</th><th>vector stored as</th><th>recall@${r.k} vs exact</th><th>right answer</th><th>p50</th><th>top score</th><th>field size</th><th>index size</th></tr>${rows}</table></div>
      <div class="lesson">
        <b>Similarity:</b> on normalized vectors cosine, dotProduct and euclidean find the same neighbours. Pick by what your model outputs; only the score scale changes.
        <b>Storage:</b> BinData float32 and int8 cut each document's vector from ${fbytes(base.fieldBytes)} to ${fbytes(by('v_float32').fieldBytes)} and ${fbytes(by('v_int8').fieldBytes)}.
        <b>Quantization</b> shrinks the graph in mongot's memory, not the index on disk (compare the sizes): binary recall here is ${(bin.recall * 100).toFixed(1)}%${bin.recall < 0.95 ? '. Raise numCandidates and run again: compressed indexes need more candidates to rescore' : ''}.
        <b>Normalization:</b> un-normalized vectors with dotProduct keep only ${(dot.recall * 100).toFixed(0)}% of the right neighbours, because length beats meaning.
      </div>`;
    bindCopy();
  } catch (e) { $('#wsOut').innerHTML = `<div class="note">${esc(e.message)}</div>`; } finally { $('#wsBench').disabled = false; }
}

function renderBenchPG(r) {
  const by = (n) => r.rows.find((x) => x.name === n) || {};
  const pctb = (x, color) => `<span class="minibar"><div style="width:${Math.round(x * 100)}%;background:${color}"></div></span>${(x * 100).toFixed(1)}%`;
  const base = r.rows[0];
  const rows = r.rows.map((x, i) => {
    const bad = x.recall < 0.9 || x.accuracy < base.accuracy - 0.05;
    const rc = x.recall >= 0.99 ? 'var(--good)' : x.recall >= 0.9 ? 'var(--warn)' : 'var(--bad)';
    return `<tr class="${i === 0 ? 'base' : bad ? 'badrow' : ''}">
      <td><code>${esc(x.name)}</code><div class="small muted">${esc(x.similarity)}</div></td>
      <td class="small">${esc(x.storage)}</td>
      <td>${x.error ? `<span class="bad small">${esc(x.error)}</span>` : pctb(x.recall, rc)}</td>
      <td>${x.error ? '' : pctb(x.accuracy, 'var(--accent)')}</td>
      <td>${fms(x.p50Ms)}</td>
      <td>${x.buildMs ? fms(x.buildMs) : '—'}</td>
      <td>${fbytes(x.fieldBytes) || '—'}</td>
      <td>${fbytes(x.indexBytes) || '—'}</td>
    </tr><tr class="lessonrow"><td colspan="8">${esc(x.lesson)}${x.meanLength ? ` <b>Mean length of the vectors it returned: ${x.meanLength.toFixed(2)}.</b>` : ''}
      <details><summary>definition and query</summary>${codeBlock(x.shell + (x.query ? '\n\n-- the benchmark query, as run:\n' + x.query : ''))}</details></td></tr>`;
  }).join('');
  const hn = by('pg_hnsw_cosine'), m4 = by('pg_hnsw_m4'), ivf = by('pg_ivfflat'), half = by('pg_halfvec'), bin = by('pg_binary'), raw = by('pg_raw_ip');
  $('#wsOut').innerHTML = `
    <div class="metrics-row"><span><b>${r.queries}</b> questions × <b>${r.rows.length}</b> tables over <b>${r.docs}</b> tickets</span><span>LIMIT ${r.k} · hnsw.ef_search ${r.efSearch} · ivfflat.probes ${r.probes}</span><span class="muted small">${r.seconds.toFixed(1)} s</span></div>
    <div style="overflow-x:auto"><table class="bench"><tr><th>table / index</th><th>vector stored as</th><th>recall@${r.k} vs exact</th><th>right answer</th><th>p50</th><th>CREATE INDEX</th><th>vector per row</th><th>index size</th></tr>${rows}</table></div>
    <div class="lesson">
      <b>Operators:</b> on normalized vectors <code>&lt;=&gt;</code>, <code>&lt;-&gt;</code> and <code>&lt;#&gt;</code> find the same neighbours; pick the one your model is trained for, and build the index with the matching operator class.
      <b>HNSW vs IVFFlat:</b> HNSW is ${hn.recall != null ? (hn.recall * 100).toFixed(0) + '%' : '—'} accurate at ef_search ${r.efSearch} with no training; IVFFlat ${ivf.recall != null ? 'is ' + (ivf.recall * 100).toFixed(0) + '% at ' + r.probes + ' probe' + (r.probes === 1 ? '' : 's') : ''}, builds ${hn.buildMs && ivf.buildMs ? (hn.buildMs / ivf.buildMs).toFixed(1) + '× faster' : 'faster'}, and needs data in the table first, because its lists are k-means centres of the rows that exist at CREATE INDEX time.
      <b>Graph size:</b> m=4 builds ${m4.buildMs && hn.buildMs ? (hn.buildMs / m4.buildMs).toFixed(1) + '× faster' : 'faster'} into ${fbytes(m4.indexBytes) || '?'} instead of ${fbytes(hn.indexBytes) || '?'}, at ${m4.recall != null ? (m4.recall * 100).toFixed(0) + '%' : '?'} recall.
      <b>halfvec</b> halves each row's vector (${fbytes(base.fieldBytes)} → ${fbytes(half.fieldBytes)}) and the index (${fbytes(half.indexBytes)}) for a recall you can hardly see. <b>Binary quantization</b> indexes 384 bits per row (${fbytes(bin.indexBytes)}) and re-ranks the candidates with the full vectors: ${bin.recall != null ? (bin.recall * 100).toFixed(0) + '%' : '?'} recall here${bin.recall < 0.95 ? '; raise ef_search and run again, since re-ranking only helps if the right rows are among the candidates' : ''}.
      <b>Normalization:</b> un-normalized vectors ordered by <code>&lt;#&gt;</code> keep only ${raw.recall != null ? (raw.recall * 100).toFixed(0) + '%' : '?'} of the right neighbours, because length beats meaning.
    </div>`;
  bindCopy();
  $('#wsBench').disabled = false;
}

async function loadPlayground() {
  const v = await api('workshop/playground');
  $('#pgActions').innerHTML = v.actions.map((a) => `<button class="btn" data-a="${a.id}">${esc(a.label)}</button>`).join('');
  $$('#pgActions button').forEach((b) => b.onclick = async () => {
    const a = v.actions.find((x) => x.id === b.dataset.a);
    $('#pgShell').innerHTML = codeBlock(a.shell) + `<div class="lesson" style="margin-top:6px">${esc(a.hint)}</div>`; bindCopy();
    try { await api('workshop/playground', { action: a.id }); } catch (e) { flash(e.message); return; }
    clearInterval(WS.pgTimer); WS.pgTimer = setInterval(refreshPlayground, 1000); refreshPlayground();
  });
  renderPlayground(v);
  if (v.running && !WS.pgTimer) WS.pgTimer = setInterval(refreshPlayground, 1000);
}
async function refreshPlayground() { const v = await api('workshop/playground'); renderPlayground(v); if (!v.running) { clearInterval(WS.pgTimer); WS.pgTimer = null; } }
function probeCls(p) { return p.startsWith('error') ? 'probe-err' : p.startsWith('0 results') ? 'probe-empty' : 'probe-ok'; }
function renderPlayground(v) {
  if (!v.events?.length) { $('#pgTimeline').innerHTML = `<div class="muted small">Nothing yet: start with ${esc(v.actions?.[0]?.label || '1')}.</div>`; return; }
  $('#pgTimeline').innerHTML = `<div class="small"><b>${esc(v.action)}</b> ${v.running ? '<span class="muted">· watching…</span>' : '<span class="muted">· stopped watching after 3 minutes</span>'}</div>
    <table class="tl"><tr><th>t</th><th>${S.pg ? 'index' : 'status'}</th><th>${S.pg ? 'valid' : 'queryable'}</th><th>${S.pg ? 'plan and settings' : 'definition'}</th><th>${S.pg ? 'ORDER BY … LIMIT 10' : 'plain $vectorSearch'}</th><th>${S.pg ? 'WHERE vip … LIMIT 10' : 'with filter: { team: "Connectivity" }'}</th></tr>
    ${v.events.map((e) => `<tr><td>+${e.t.toFixed(1)} s</td><td><span class="pill ${esc(e.status)}">${esc(e.status)}</span></td><td>${e.queryable ? 'yes' : 'no'}</td>
      <td class="small">${e.version ? (S.pg ? esc(e.version) : 'latest v' + esc(e.version)) : ''}${e.detail ? '<div class="muted">' + esc(e.detail) + '</div>' : ''}</td>
      <td class="${probeCls(e.plain)}">${esc(e.plain)}</td><td class="${probeCls(e.filter)}">${esc(e.filter)}</td></tr>`).join('')}</table>`;
}

async function runExplain() {
  const body = { query: $('#exQuery').value, index: $('#exIndex').value, k: Number($('#exK').value), numCandidates: Number($('#exN').value), probes: Number($('#exP').value), exact: $('#exExact').checked, team: $('#exTeam').value };
  $('#exOut').innerHTML = '<div class="muted">Explaining…</div>';
  const r = await api('workshop/explain', body);
  if (r.error) { $('#exOut').innerHTML = `<div class="note">${esc(r.error)}</div>${codeBlock(r.pipeline || '')}`; bindCopy(); return; }
  if (S.pg) return renderExplainPG(body, r);
  const share = r.totalDocs ? r.visited / r.totalDocs : 0;
  const maxDocs = Math.max(1, ...(r.segments || []).map((g) => g.docs));
  const exact = /Exact/.test(r.queryType);
  $('#exOut').innerHTML = `
    <div class="metrics-row"><span>${exact ? 'scored' : 'HNSW visited'} <b>${Math.round(r.visited)}</b> of <b>${Math.round(r.totalDocs)}</b> vectors (<b>${(share * 100).toFixed(0)}%</b>)</span>
      <span>Lucene query <code>${esc(r.queryType)}</code></span><span>${r.segments?.length || 0} segments with graphs</span><span>query ${fms(r.queryMs)} · collect ${fms(r.collectorMs)}</span><span class="muted small">mongot ${esc(r.mongotVersion)}</span></div>
    ${r.segments?.length ? `<div class="segs">${[...r.segments].sort((a, b) => b.docs - a.docs).slice(0, 12).map((g) => `<div class="seg-row"><span class="muted">segment ${esc(g.id)}</span>
      <span class="seg-bar" title="${g.visited} visited of ${g.docs}"><div style="width:${(100 * g.docs) / maxDocs}%;background:var(--card)"></div><div style="width:${(100 * Math.min(g.visited, g.docs)) / maxDocs}%"></div></span>
      <span class="small">${g.mode} · ${Math.round(g.visited)}/${Math.round(g.docs)} · ${fms(g.ms)}</span></div>`).join('')}${r.segments.length > 12 ? `<div class="small muted">… and ${r.segments.length - 12} smaller segments. A collection that takes a steady stream of inserts, like the desk's tickets, has many small segments until Lucene merges them, and each one is searched on its own.</div>` : ''}</div>` : ''}
    <div class="lesson">${exact ? 'Exact search has no graph to walk: it scores every vector in the index. Perfect recall, cost growing with the collection. Fine for thousands, painful for millions.'
      : 'Each Lucene segment has its own HNSW graph, searched separately for numCandidates neighbours. That is why the visited count adds up across segments, and why small indexes visit a large share. Raise numCandidates and explain again: visits grow, and so does recall (step 1).'}${body.team ? ' With a filter, mongot only accepts neighbours whose team matches; a very selective filter can push it to exact search.' : ''}</div>
    ${codeBlock(r.pipeline)}
    <details><summary>raw explain output</summary>${codeBlock(r.raw)}</details>`;
  bindCopy();
}

function renderExplainPG(body, r) {
  const seq = /Seq Scan/.test(r.queryType);
  const desk = !body.index || body.index === 'tickets_vector';
  const isIvf = /ivfflat/.test(body.index), isBin = /binary/.test(body.index), isExactTbl = body.index === 'pg_exact';
  const lesson = body.exact ? 'Index scans are off, so PostgreSQL computes the distance of every row and sorts: the ground truth, at a cost that grows with the table.'
    : isExactTbl ? 'This table has no vector index at all: a Seq Scan and a top-k Sort (a heap of k rows, so memory stays small) is the only plan there is.'
    : seq && desk ? `The planner chose a sequential scan. The desk's table has only ${Math.round(r.totalDocs)} rows: reading them all and sorting by distance is cheaper than walking a graph, and the answer is exact. That is the planner doing its job, not the index failing. As the table grows past a few thousand rows the estimate flips and the same query becomes an Index Scan on the HNSW index. Pick a variant table (2,000 rows each) to see one.`
    : seq ? 'A sequential scan: either the table is small enough that the planner prefers it, or no index matches the ORDER BY (the operator must be the one the index was built for).'
    : isBin ? 'Two steps: the inner Index Scan walks the 48-byte bit index by Hamming distance and returns ef_search candidates, the outer Sort re-ranks them by the full vector. Read the inner and outer row counts.'
    : isIvf ? 'IVFFlat searched ivfflat.probes of its lists and scored every row in them. More probes: more rows read, better recall. Compare the buffers line at 1 and at 10 probes.'
    : 'An Index Scan on the HNSW index: the ORDER BY … LIMIT was answered by a graph walk that kept hnsw.ef_search candidates. The buffers line counts the 8 kB pages it touched, graph and heap.';
  $('#exOut').innerHTML = `
    <div class="metrics-row"><span>plan <b>${esc(r.queryType)}</b></span><span>execution <b>${fms(r.queryMs)}</b></span><span class="muted small">table of ${Math.round(r.totalDocs)} rows</span></div>
    ${r.summary?.length ? `<table class="idx">${r.summary.map((x) => `<tr><td class="muted">${esc(x.k)}</td><td>${esc(x.v)}</td></tr>`).join('')}</table>` : ''}
    <div class="lesson">${lesson}${body.team && !seq && !isExactTbl ? ' With the team filter, the WHERE is checked on each candidate the index returns; hnsw.iterative_scan lets the walk continue until LIMIT rows pass.' : ''}</div>
    <div class="small muted" style="margin-top:8px">The plan, as EXPLAIN printed it:</div>
    ${codeBlock(r.plan || r.raw || '')}
    <details><summary>the statement</summary>${codeBlock(r.pipeline)}</details>`;
  bindCopy();
}

function renderMetricsPG(v) {
  const m = v.pg || {};
  const vecIdx = (m.indexes || []).filter((x) => x.method !== 'gin'), ftIdx = (m.indexes || []).filter((x) => x.method === 'gin');
  const row = (x) => `<tr><td><code>${esc(x.name)}</code>${x.valid === false ? ' <span class="pill FAILED">INVALID</span>' : ''}</td><td>${esc(x.table)}</td><td>${esc(x.method)}</td><td>${fbytes(x.bytes) || '—'}</td><td>${x.scans}</td><td>${x.tuples}</td><td>${x.hitPct != null ? x.hitPct.toFixed(0) + '%' : '—'}</td></tr>`;
  const head = '<tr><th>index</th><th>table</th><th>type</th><th>size</th><th>scans</th><th>tuples read</th><th>cache hit</th></tr>';
  const desk = (m.indexes || []).find((x) => x.name === 'tickets_embedding_hnsw');
  $('#mtOut').innerHTML = `
    <div class="small muted">PostgreSQL ${esc(m.version || '?')} · pgvector ${esc(m.vector || '?')} · refreshed every 5 s</div>
    <div class="cards">${(m.settings || []).filter((x) => /^(hnsw|ivfflat)\./.test(x.k) || ['maintenance_work_mem', 'shared_buffers', 'max_parallel_maintenance_workers'].includes(x.k)).map((x) => `<div class="mcard"><div class="l">${esc(x.k)}</div><div class="v" style="font-size:18px">${esc(x.v)}</div></div>`).join('')}</div>
    <div style="overflow-x:auto"><table class="idx">${head}${vecIdx.map(row).join('')}${ftIdx.map(row).join('')}</table></div>
    ${desk && desk.scans === 0 ? `<div class="lesson">The desk's own <code>tickets_embedding_hnsw</code> shows 0 scans: on a table of a few hundred rows the planner reads the table instead (Explain shows why). The Workshop's variant tables are scanned through their indexes, so their counters move when you run the benchmark.</div>` : ''}
    <h4 style="margin:14px 0 4px">Vector queries in pg_stat_statements</h4>
    ${m.statements?.length ? `<div style="overflow-x:auto"><table class="idx"><tr><th>query</th><th>calls</th><th>mean</th><th>rows</th></tr>${m.statements.map((x) => `<tr><td><code class="small">${esc(x.query)}</code></td><td>${x.calls}</td><td>${fms(x.meanMs)}</td><td>${x.rows}</td></tr>`).join('')}</table></div>`
      : `<div class="note">${esc(m.stmtsNote || 'No vector statements recorded yet.')}</div>`}
    <details><summary>every setting read</summary>${codeBlock((m.settings || []).map((x) => x.k + ' = ' + x.v).join('\n'))}</details>`;
  bindCopy();
}

async function loadMetrics() {
  let v;
  try { v = await api('workshop/metrics'); } catch (e) { $('#mtOut').innerHTML = `<div class="note">${esc(e.message)}</div>`; return; }
  if ($('#mtRawWrap')?.open) return; // keep an open raw view still while it is being read
  if (v.engine === 'postgres') { if (!$('#mtOut details[open]')) renderMetricsPG(v); return; }
  if (!v.endpoints?.length) { $('#mtOut').innerHTML = '<div class="note">No mongot to read: this cluster has no vector search.</div>'; return; }
  const ok = v.mongots.filter((m) => !m.error);
  if (!ok.length) {
    $('#mtOut').innerHTML = `<div class="note">Could not reach ${v.endpoints.map(esc).join(', ')}: ${esc(v.mongots[0]?.error || '')}. On Kubernetes, mongot's metrics port is only reachable inside the cluster.</div>`;
    return;
  }
  const sum = (f) => ok.reduce((a, m) => a + f(m), 0);
  const q = (c) => `p50 <b>${fms(c.P50 * 1000)}</b> · p90 ${fms(c.P90 * 1000)} · p99 ${fms(c.P99 * 1000)}`;
  const m0 = ok[0];
  $('#mtOut').innerHTML = `
    <div class="small muted">${ok.length} mongot${ok.length > 1 ? 's' : ''}: ${ok.map((m) => esc(m.endpoint)).join(', ')} · refreshed every 5 s</div>
    <div class="cards">
      <div class="mcard"><div class="l">$vectorSearch inside mongot${ok.length > 1 ? ' (first)' : ''}</div><div class="v">${fms(m0.vectorSearch.P50 * 1000)}</div><div class="small">${q(m0.vectorSearch)}</div><div class="small muted">${sum((m) => m.vectorSearch.Count)} commands · ${sum((m) => m.vectorSearch.Failures)} failed</div></div>
      <div class="mcard"><div class="l">$search inside mongot</div><div class="v">${fms(m0.search.P50 * 1000)}</div><div class="small">${q(m0.search)}</div><div class="small muted">${sum((m) => m.search.Count)} commands · ${sum((m) => m.search.Failures)} failed</div></div>
      <div class="mcard"><div class="l">JVM heap</div><div class="v">${fbytes(sum((m) => m.heapUsed))}</div><div class="small muted">of ${fbytes(sum((m) => m.heapMax)) || '?'} max. mongot is a Java service; HNSW graphs live in this heap while queried</div></div>
    </div>
    <div style="overflow-x:auto"><table class="idx"><tr><th>index</th><th>collection</th><th>type</th><th>status</th><th>docs in index</th><th>size</th><th>lag</th></tr>
    ${v.indexes.map((x) => `<tr><td><code>${esc(x.name)}</code></td><td>${esc(x.collection)}</td><td>${esc(x.type)}</td><td><span class="pill ${esc(x.status)}">${esc(x.status)}</span>${x.mongotStatus ? ` <span class="small muted">${esc(x.mongotStatus)}</span>` : ''}</td>
      <td>${x.docs ? Math.round(x.docs) : '—'}</td><td>${fbytes(x.sizeBytes) || '<span class="muted small">pending</span>'}</td><td>${x.lagMs ? Math.round(x.lagMs) + ' ms' : '0'}</td></tr>`).join('')}</table></div>
    <details id="mtRawWrap"><summary>raw metrics</summary><input id="mtFilter" type="text" placeholder="filter, e.g. vectorSearch or indexSizeBytes" style="width:100%;margin:6px 0"><pre id="mtRaw" class="code">loading…</pre></details>`;
  $('#mtRawWrap').ontoggle = async () => {
    if (!$('#mtRawWrap').open) return;
    const raw = await api('workshop/metrics?raw=1');
    const lines = raw.mongots.flatMap((m) => (m.raw || []).map((l) => (raw.mongots.length > 1 ? m.endpoint + '  ' : '') + l));
    const draw = () => { const f = $('#mtFilter').value.toLowerCase(); $('#mtRaw').textContent = lines.filter((l) => !f || l.toLowerCase().includes(f)).slice(0, 800).join('\n'); };
    $('#mtFilter').oninput = draw; draw();
  };
}

boot().catch((e) => { $('#hero').textContent = 'Could not load the desk: ' + e.message; });
