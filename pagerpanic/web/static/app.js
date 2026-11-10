'use strict';
// Pager Panic's page. It polls the game server and draws what it says; every move is a
// POST to api/play. Paths are relative because DBCanvas serves the page under a prefix.

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmt = (secs) => { secs = Math.max(0, Math.round(secs)); return `${String(Math.floor(secs / 60)).padStart(2, '0')}:${String(secs % 60).padStart(2, '0')}`; };

let S = null;            // the last state
let lastSeq = -1;        // the last event animated
let sel = null;          // the selected ticket's id
let wantMode = null;     // the mode the player picked, waiting for the systems check
let wantTask = '';
let wantSeed = 0;        // a replay's seed
let shownScore = 0;
let continueTimer = null;
let lastPhase = '';
const ticketBoxes = {};  // ticket id → last on-screen rectangle, for effects after it leaves

async function api(method, path, body) {
  const r = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}
const play = (body) => api('POST', 'api/play', body).catch((e) => { toast(e.message, 'error'); throw e; });

// ---------------------------------------------------------------- the room
(function buildRacks() {
  const racks = $('racks');
  for (let r = 0; r < 9; r++) {
    const rack = document.createElement('div');
    rack.className = 'rack';
    for (let u = 0; u < 18; u++) {
      const unit = document.createElement('u');
      for (let l = 0; l < 4; l++) {
        const led = document.createElement('b');
        if (Math.random() < 0.08) led.className = 'r';
        led.style.animationDelay = `${(Math.random() * 2).toFixed(2)}s`;
        unit.appendChild(led);
      }
      rack.appendChild(unit);
    }
    racks.appendChild(rack);
  }
})();
function alarm(level) {
  document.documentElement.style.setProperty('--alarm', String(Math.max(0, Math.min(1, level))));
  document.querySelectorAll('.rack u b').forEach((b, i) => {
    if (i % 7 === 0) b.classList.toggle('r', Math.random() < level * 0.9);
  });
}

// ---------------------------------------------------------------- sound
const Sound = {
  on: (() => { try { return localStorage.getItem('pp-sound') === 'on'; } catch { return false; } })(),
  ctx: null,
  tone(freq, dur, type = 'square', vol = 0.06, slide = 0, when = 0) {
    if (!this.on) return;
    try {
      this.ctx ||= new (window.AudioContext || window.webkitAudioContext)();
      const t = this.ctx.currentTime + when;
      const o = this.ctx.createOscillator();
      const g = this.ctx.createGain();
      o.type = type;
      o.frequency.setValueAtTime(freq, t);
      if (slide) o.frequency.exponentialRampToValueAtTime(Math.max(30, freq + slide), t + dur);
      g.gain.setValueAtTime(vol, t);
      g.gain.exponentialRampToValueAtTime(0.0001, t + dur);
      o.connect(g).connect(this.ctx.destination);
      o.start(t);
      o.stop(t + dur + 0.02);
    } catch { /* no audio */ }
  },
  seq(notes, type, gap = 0.09) { notes.forEach((n, i) => this.tone(n, gap * 1.4, type, 0.06, 0, i * gap)); },
  spawn() { this.tone(880, 0.08); this.tone(660, 0.08, 'square', 0.06, 0, 0.1); this.tone(880, 0.08, 'square', 0.06, 0, 0.2); },
  boss() { this.seq([196, 196, 147, 196, 262], 'sawtooth', 0.14); },
  clear() { this.seq([523, 659, 784, 1047], 'square', 0.06); },
  lose() { this.tone(300, 0.5, 'sawtooth', 0.09, -220); },
  stage() { this.seq([392, 523, 659, 784, 659, 784, 1047], 'triangle', 0.1); },
  win() { this.seq([523, 523, 523, 659, 784, 659, 784, 1047, 1047], 'square', 0.12); },
  over() { this.seq([392, 330, 262, 196, 131], 'triangle', 0.28); },
  blip() { this.tone(1200, 0.04, 'square', 0.03); },
  toggle() {
    this.on = !this.on;
    try { localStorage.setItem('pp-sound', this.on ? 'on' : 'off'); } catch { /* private mode */ }
    $('btnSound').classList.toggle('off', !this.on);
    if (this.on) this.blip();
  },
};
$('btnSound').classList.toggle('off', !Sound.on);

// ---------------------------------------------------------------- effects
const fx = $('fx');
const g2 = fx.getContext('2d');
let parts = [];
function sizeFx() { fx.width = innerWidth; fx.height = innerHeight; }
addEventListener('resize', sizeFx); sizeFx();
function burst(x, y, colors, n = 60, speed = 6, life = 60) {
  for (let i = 0; i < n; i++) {
    const a = Math.random() * Math.PI * 2;
    const v = speed * (0.4 + Math.random());
    parts.push({ x, y, vx: Math.cos(a) * v, vy: Math.sin(a) * v - 1.5, life, max: life, c: colors[i % colors.length], s: 3 + Math.random() * 4 });
  }
}
let fireworksUntil = 0;
function fireworks(ms) { fireworksUntil = performance.now() + ms; }
(function loop(t) {
  g2.clearRect(0, 0, fx.width, fx.height);
  if (t < fireworksUntil && Math.random() < 0.08) {
    burst(Math.random() * fx.width, fx.height * (0.15 + Math.random() * 0.4), ['#39ffb6', '#ff3d7f', '#ffcb3d', '#47c8ff', '#ffffff'], 90, 7, 80);
    Sound.tone(200 + Math.random() * 600, 0.12, 'triangle', 0.03);
  }
  parts = parts.filter((p) => p.life > 0);
  for (const p of parts) {
    p.x += p.vx; p.y += p.vy; p.vy += 0.14; p.vx *= 0.985; p.life--;
    g2.globalAlpha = p.life / p.max;
    g2.fillStyle = p.c;
    g2.fillRect(Math.round(p.x), Math.round(p.y), p.s, p.s);
  }
  g2.globalAlpha = 1;
  requestAnimationFrame(loop);
})(0);
function flash(good) {
  const f = $('flash');
  f.classList.toggle('good', !!good);
  f.classList.remove('go'); void f.offsetWidth; f.classList.add('go');
}
function shake() { document.body.classList.remove('shake'); void document.body.offsetWidth; document.body.classList.add('shake'); }
function banner(text, kind = '', sub = '') {
  const b = $('banner');
  b.className = 'banner ' + kind;
  b.innerHTML = esc(text) + (sub ? `<small>${esc(sub)}</small>` : '');
  b.classList.remove('hidden');
  b.style.animation = 'none'; void b.offsetWidth; b.style.animation = '';
  clearTimeout(banner.t);
  banner.t = setTimeout(() => b.classList.add('hidden'), kind === 'win' ? 3600 : 2300);
}
function floater(text, x, y, neg) {
  const el = document.createElement('div');
  el.className = 'floater' + (neg ? ' neg' : '');
  el.textContent = text;
  el.style.left = `${x}px`; el.style.top = `${y}px`;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 1300);
}
function toast(text, kind = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.textContent = text;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), kind === 'medal' ? 6000 : 4200);
}
function centerOf(id) {
  const el = document.querySelector(`.ticket[data-id="${CSS.escape(id || '')}"]`);
  const r = el ? el.getBoundingClientRect() : ticketBoxes[id];
  if (!r) return { x: innerWidth / 2, y: innerHeight / 3 };
  return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
}

// ---------------------------------------------------------------- the game's own dialog
// Every question the page asks is drawn in the game, not by the browser. ask() resolves
// true for the confirm button, false for cancel, Escape or a click outside.
let dlgDone = null;
function ask(title, text, yes = 'OK', no = 'CANCEL', copyText = null) {
  if (dlgDone) dlgDone(false);
  $('dlgTitle').textContent = title;
  $('dlgText').textContent = text;
  $('dlgYes').textContent = yes;
  $('dlgNo').textContent = no;
  $('dlgNo').classList.toggle('hidden', !no);
  const area = $('dlgCopy');
  area.classList.toggle('hidden', copyText === null);
  area.value = copyText ?? '';
  $('dialog').classList.remove('hidden');
  Sound.blip();
  setTimeout(() => (copyText !== null ? (area.focus(), area.select()) : $('dlgYes').focus()), 30);
  return new Promise((resolve) => {
    dlgDone = (v) => { dlgDone = null; $('dialog').classList.add('hidden'); resolve(v); };
  });
}
$('dlgYes').onclick = () => dlgDone && dlgDone(true);
$('dlgNo').onclick = () => dlgDone && dlgDone(false);
$('dialog').addEventListener('click', (ev) => { if (ev.target.id === 'dialog' && dlgDone) dlgDone(false); });
addEventListener('keydown', (ev) => {
  if (!dlgDone) return;
  if (ev.key === 'Escape') { ev.preventDefault(); dlgDone(false); }
  if (ev.key === 'Enter' && ev.target.tagName !== 'TEXTAREA') { ev.preventDefault(); dlgDone(true); }
}, true);
const confirmGiveUp = () => ask('GIVE UP?', 'This ticket scores nothing and resets your combo. It costs no life.', 'GIVE UP', 'KEEP GOING');

// ---------------------------------------------------------------- events from the game
function animate(e) {
  const c = centerOf(e.ticket);
  switch (e.kind) {
    case 'spawn':
      if (e.text.startsWith('BOSS')) { banner('BOSS INCOMING', 'bad', e.text.replace('BOSS INCOMING · ', '')); Sound.boss(); shake(); }
      else Sound.spawn();
      break;
    case 'clear':
      burst(c.x, c.y, ['#39ffb6', '#ffcb3d', '#47c8ff', '#ffffff'], 70);
      floater(`+${e.points}`, c.x - 30, c.y - 20);
      if (e.text.startsWith('×')) floater(e.text.split(' · ')[0], c.x - 30, c.y + 10);
      flash(true); Sound.clear();
      break;
    case 'expire':
      shake(); flash(false); Sound.lose();
      burst(c.x, c.y, ['#ff4d4d', '#ff3d7f', '#5b1335'], 50, 5);
      if (S?.mode === 'arcade') {
        banner('TICKET ESCALATED', 'bad', e.text.replace(/^[^·]*· /, ''));
        const lives = document.querySelectorAll('.life:not(.gone)');
        const last = lives[lives.length - 1];
        if (last) last.classList.add('breaking');
      } else toast(e.text);
      break;
    case 'giveup': toast(e.text); break;
    case 'hint': floater(`${e.points}`, c.x - 20, c.y - 10, true); Sound.blip(); break;
    case 'stage': banner(e.text.split(' · ')[0], '', (e.text.split(' · ')[1] || '') + (e.points ? `  +${e.points}` : '')); Sound.stage(); fireworks(1200); break;
    case 'lifeup': toast('+1 LIFE', 'medal'); Sound.seq([660, 880, 1320], 'square', 0.08); break;
    case 'medal': toast('★ ' + e.text, 'medal'); break;
    case 'error': toast(e.text, 'error'); break;
    case 'victory': banner('SHIFT COMPLETE', 'win', `SCORE ${e.points}`); fireworks(6000); Sound.win(); break;
    case 'gameover':
      document.body.classList.add('lights-out');
      banner('GAME OVER', 'bad'); Sound.over(); shake();
      setTimeout(showContinue, 2400);
      break;
    case 'quit': banner('SHIFT ENDED EARLY'); break;
    case 'timeup': banner("TIME'S UP", 'win', `SCORE ${e.points}`); fireworks(3000); Sound.win(); break;
    case 'practice-done': banner('PRACTICE DONE'); break;
  }
}

// ---------------------------------------------------------------- polling
async function poll() {
  try {
    const since = lastSeq < 0 ? 1e9 : lastSeq;
    S = await api('GET', 'api/state?since=' + since);
    if (lastSeq < 0) lastSeq = S.seq; // the first look animates nothing that already happened
    for (const e of S.events) {
      if (e.seq > lastSeq) { animate(e); lastSeq = e.seq; }
    }
    render();
  } catch (e) {
    $('connStatus').textContent = 'The game server is not answering…';
  }
  setTimeout(poll, S && (S.phase === 'playing' || S.phase === 'checking') ? 500 : 1000);
}

function render() {
  const phase = S.phase;
  const screen = { title: 'title', checking: 'check', ready: 'check', playing: 'play', paused: 'play', ending: 'over', over: 'over' }[phase] || 'title';
  for (const s of ['title', 'check', 'play', 'over']) $('screen-' + s).classList.toggle('hidden', s !== screen);
  $('pause').classList.toggle('hidden', phase !== 'paused');
  if (phase !== lastPhase) {
    if (phase === 'title' || phase === 'checking') document.body.classList.remove('lights-out');
    if (phase === 'playing' && lastPhase !== 'paused') { sel = null; shownScore = 0; }
    lastPhase = phase;
  }
  if (screen === 'title') renderTitle();
  if (screen === 'check') renderCheck();
  if (screen === 'play') renderPlay();
  if (screen === 'over') renderOver();
  if (screen !== 'play') alarm(0.05);
}

// ---------------------------------------------------------------- title
function renderTitle() {
  $('bankTitle').textContent = S.bankTitle ? `${S.bankTitle} · ${S.stack?.frame || ''}` : 'loading the task bank…';
  $('clusterName').textContent = S.stack?.frame || 'the cluster';
  const waiting = S.waiting || '';
  $('connStatus').textContent = waiting ? waiting + '…' : (S.diag && S.diag.done && !S.diag.ok ? 'The last systems check failed: ' + (S.diag.error || 'see the check') : '');
  document.querySelectorAll('.mode').forEach((b) => { b.disabled = !!waiting; b.classList.toggle('on', b.dataset.mode === wantMode); });
  $('pressStart').classList.toggle('hidden', !!waiting);
  const hs = (S.highScores || []).slice(0, 10);
  $('highScores').innerHTML = hs.length ? hs.map((h) => `<li><b>${esc(h.initials)}</b><span>${h.score.toLocaleString()}</span><small>${esc(h.mode)} · ${esc(h.rank)}</small></li>`).join('') : '<li class="muted">No shifts played yet.</li>';
  $('medalShelf').innerHTML = (S.medals || []).length ? S.medals.map((m) => `<span class="medal" title="${esc(m.why)}">${esc(m.title)}</span>`).join('') : '<span class="muted">Medals you earn are kept here.</span>';
  if (wantMode === 'practice') renderPracticeList();
}
function renderPracticeList() {
  const list = $('practiceList');
  list.classList.remove('hidden');
  if (list.dataset.n === String(S.tasks?.length || 0)) return;
  list.dataset.n = String(S.tasks?.length || 0);
  const cats = Object.fromEntries((S.categories || []).map((c) => [c.id, c.title]));
  list.innerHTML = (S.tasks || []).map((t) => `<button data-task="${esc(t.id)}" ${t.unsupported ? 'disabled' : ''}>
      <span><span class="chip cat-${esc(t.category)}">${esc(cats[t.category] || t.category)}</span> ${esc(t.title)} ${t.boss ? '<span class="chip boss">boss</span>' : ''}</span>
      <small>${'★'.repeat(t.difficulty)}${t.unsupported ? ' · ' + esc(t.unsupported) : ''}</small></button>`).join('');
}
$('modes').addEventListener('click', async (ev) => {
  const b = ev.target.closest('.mode');
  if (!b || b.disabled) return;
  Sound.blip();
  wantMode = b.dataset.mode; wantSeed = 0; wantTask = '';
  if (wantMode === 'practice') { renderPracticeList(); render(); return; }
  $('practiceList').classList.add('hidden');
  await play({ do: 'check-system' }).catch(() => {});
});
$('practiceList').addEventListener('click', async (ev) => {
  const b = ev.target.closest('button[data-task]');
  if (!b || b.disabled) return;
  wantTask = b.dataset.task;
  await play({ do: 'check-system' }).catch(() => {});
});

// ---------------------------------------------------------------- systems check
function renderCheck() {
  const d = S.diag || { steps: [] };
  const label = { arcade: 'ARCADE', marathon: 'MARATHON', practice: 'PRACTICE' }[wantMode] || '';
  $('checkFor').textContent = `${label ? label + ' · ' : ''}${S.bankTitle || ''} on ${S.stack?.frame || 'the cluster'}`;
  const mark = { pending: '···', running: '>>', ok: 'OK', warn: '!!', fail: 'XX' };
  $('diagSteps').innerHTML = (d.steps || []).map((s) => `<li class="${esc(s.state)}"><span class="st">${mark[s.state] || '··'}</span><b>${esc(s.name)}</b>${s.detail ? `<small>${esc(s.detail)}</small>` : ''}</li>`).join('');
  const v = $('diagVerdict');
  const ready = S.phase === 'ready';
  if (ready) { v.textContent = 'SYSTEMS NOMINAL'; v.className = 'verdict ok'; }
  else if (d.done && !d.ok) { v.textContent = 'SYSTEMS CHECK FAILED' + (d.error ? ' · ' + d.error : ''); v.className = 'verdict fail'; }
  else { v.textContent = 'checking…'; v.className = 'verdict'; }
  $('btnStart').classList.toggle('hidden', !ready);
  $('btnRecheck').classList.toggle('hidden', !(d.done && !d.ok) || ready);
  if (ready && !renderCheck.chimed) { renderCheck.chimed = true; Sound.seq([523, 784], 'square', 0.1); }
  if (!ready) renderCheck.chimed = false;
  if (ready && wantSeed && wantMode) begin(); // a replay starts as soon as the check passes
}
async function begin() {
  if (!wantMode) { toast('Pick a mode first'); return; }
  const seed = wantSeed; wantSeed = 0;
  await play({ do: 'begin', mode: wantMode, task: wantTask, seed }).catch(() => {});
  Sound.stage();
}
$('btnStart').onclick = begin;
$('btnRecheck').onclick = () => play({ do: 'check-system' }).catch(() => {});
$('btnBack').onclick = () => play({ do: 'quit' }).catch(() => {});

// ---------------------------------------------------------------- play
function renderPlay() {
  const max = S.maxLives || 3;
  const lives = $('lives');
  if (S.mode === 'arcade') {
    if (lives.children.length !== max || lives.dataset.lives !== String(S.lives)) {
      lives.dataset.lives = String(S.lives);
      lives.innerHTML = Array.from({ length: max }, (_, i) => `<span class="life ${i < S.lives ? '' : 'gone'}"></span>`).join('');
    }
  } else lives.innerHTML = `<span class="cap">${S.mode === 'marathon' ? 'MARATHON' : 'PRACTICE'}</span>`;
  if (S.score !== shownScore) {
    const el = $('score');
    el.classList.remove('score-bump'); void el.offsetWidth; el.classList.add('score-bump');
    const from = shownScore, to = S.score, t0 = performance.now();
    shownScore = to;
    (function roll(t) { const k = Math.min(1, (t - t0) / 600); el.textContent = Math.round(from + (to - from) * k).toLocaleString(); if (k < 1) requestAnimationFrame(roll); })(t0);
  }
  $('combo').textContent = `×${S.multiplier.toFixed(2).replace(/\.?0+$/, '')}`;
  $('combo').classList.toggle('combo-hot', S.streak >= 3);
  const c = S.clock;
  $('stage').textContent = c.practicing ? '—' : `${c.stage}/${c.stages}`;
  $('shiftClock').textContent = c.practicing ? fmt(c.played) : fmt(c.shiftLeft);
  $('shiftBar').style.width = c.practicing ? '0%' : `${(100 * c.played / c.shift).toFixed(2)}%`;
  $('btnPause').disabled = S.mode === 'marathon' || S.pausesLeft <= 0;
  $('pausesLeft').textContent = S.mode === 'arcade' ? `${S.pausesLeft} pause(s) left this shift` : '';

  const box = $('tickets');
  const ids = new Set(S.tickets.map((t) => t.id));
  for (const el of [...box.children]) {
    if (!ids.has(el.dataset.id)) { ticketBoxes[el.dataset.id] = el.getBoundingClientRect(); el.remove(); }
  }
  let red = 0;
  S.tickets.forEach((t, i) => {
    let el = box.querySelector(`.ticket[data-id="${CSS.escape(t.id)}"]`);
    if (!el) {
      el = document.createElement('button');
      el.className = 'ticket';
      el.dataset.id = t.id;
      el.innerHTML = `<div class="ring"><svg viewBox="0 0 64 64"><circle class="track" cx="32" cy="32" r="27"/><circle class="left" cx="32" cy="32" r="27" stroke-dasharray="169.6" stroke-dashoffset="0"/></svg><span></span></div><div><div class="t-title"></div><div class="t-meta"></div></div>`;
      box.appendChild(el);
    }
    const frac = t.limit > 0 ? t.remaining / t.limit : 1;
    const incoming = t.state === 'incoming';
    el.classList.toggle('incoming', incoming);
    el.classList.toggle('boss', !!t.boss);
    el.classList.toggle('sel', sel === t.id);
    el.classList.toggle('amber', !incoming && t.limit > 0 && frac < 0.5 && t.remaining > 30);
    el.classList.toggle('red', !incoming && t.limit > 0 && t.remaining <= 30 && t.remaining > 10);
    el.classList.toggle('shake', !incoming && t.limit > 0 && t.remaining <= 10);
    if (!incoming && t.limit > 0 && t.remaining <= 30) red++;
    el.querySelector('.left').style.strokeDashoffset = String(169.6 * (1 - Math.max(0, Math.min(1, frac))));
    el.querySelector('.ring span').textContent = incoming ? '...' : (t.limit > 0 ? fmt(t.remaining) : '∞');
    el.querySelector('.t-title').innerHTML = incoming ? `<span class="ringing">INCOMING PAGE…</span>` : `<span class="muted">${i + 1}</span> ${esc(t.title)}`;
    el.querySelector('.t-meta').innerHTML = `${t.boss ? '<span class="chip boss">boss</span>' : ''}<span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span><span class="chip">${esc(t.kind)}</span><span class="stars">${'★'.repeat(t.difficulty)}</span><span>${(t.points * (t.boss ? 3 : 1)).toLocaleString()} pts</span>${t.checking ? '<span class="muted">checking…</span>' : ''}`;
  });
  if (!S.tickets.length) box.innerHTML = `<div class="muted" style="text-align:center;margin-top:120px">All quiet… for now.</div>`;
  else box.querySelectorAll('.muted[style]').forEach((n) => n.remove());
  if (sel && !ids.has(sel)) sel = null;
  if (!sel) { const first = S.tickets.find((t) => t.state === 'open'); if (first) sel = first.id; }
  renderDetail();
  const stageLevel = c.practicing ? 0.1 : (c.stage - 1) / Math.max(1, c.stages - 1);
  alarm(Math.min(1, stageLevel * 0.6 + red * 0.2));
}
function catTitle(id) { return (S.categories || []).find((c) => c.id === id)?.title || id; }
function renderDetail() {
  const d = $('detail');
  const t = S.tickets.find((x) => x.id === sel);
  if (!t) { d.innerHTML = '<div class="empty">Tickets appear here. Pick one to read it.<br><br>Right-click a ticket for a terminal.</div>'; return; }
  if (t.state === 'incoming') { d.innerHTML = `<div class="empty"><span class="ringing">INCOMING PAGE…</span><br><br>The fault is being set up on the cluster.<br>Its clock starts when it lands.</div>`; return; }
  const key = JSON.stringify([t.id, t.brief, t.hints, t.checks, t.checking, t.hintsBought]);
  if (d.dataset.key === key) return;
  d.dataset.key = key;
  const nextCost = t.hintCosts && t.hintsBought < t.hintCosts.length ? t.hintCosts[t.hintsBought] : null;
  const checks = (t.checks || []).map((c) => `<li class="${c.ok ? 'ok' : 'no'}"><span class="m">${c.ok ? 'OK' : 'XX'}</span><span>${esc(c.name)}${c.guard ? ' <span class="chip">rule</span>' : ''}</span>${c.ok ? '' : `<small>got ${esc(c.got)}${c.want ? ' · want ' + esc(c.want) : ''}</small>`}</li>`).join('');
  d.innerHTML = `<h4>${esc(t.title)}</h4>
    <div class="t-meta">${t.boss ? '<span class="chip boss">boss</span>' : ''}<span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span><span class="chip">${esc(t.kind)}</span><span class="stars">${'★'.repeat(t.difficulty)}</span></div>
    <div class="brief">${brief(t.brief)}</div>
    ${(t.hints || []).map((h) => `<div class="hint">${brief(h)}</div>`).join('')}
    <ul class="checks">${checks || '<li class="muted">Checked every 10 seconds; it clears the moment the cluster is right.</li>'}</ul>
    <div class="actions">
      <button class="btn small hot" data-act="check-now">CHECK NOW</button>
      ${nextCost !== null && S.mode !== 'marathon' ? `<button class="btn small" data-act="hint">HINT −${nextCost}</button>` : ''}
      <button class="btn small" data-act="give-up">GIVE UP</button>
      <button class="btn small" data-act="menu">TERMINAL ▾</button>
    </div>
    <div class="nodes">Open a terminal: ${(S.nodes || []).map((n) => `<button data-node="${esc(n.nodeId)}">${esc(n.host)}${n.role === 'control-plane' ? ' (control plane)' : ''}${n.host === t.worker ? ' ★' : ''}</button>`).join('')}</div>`;
}
// brief joins the lines a YAML block wrapped, keeping blank lines and indented lines
// (a list a task lays out on purpose) on lines of their own.
function brief(s) {
  const out = []
  for (const line of String(s ?? '').split('\n')) {
    const prev = out[out.length - 1]
    const plain = (l) => l !== '' && !/^\s/.test(l)
    if (prev !== undefined && plain(prev) && plain(line)) out[out.length - 1] = prev + ' ' + line
    else out.push(line)
  }
  const joined = out.join('\n')
  return esc(joined).replace(/`([^`]+)`/g, '<code>$1</code>')
}

$('tickets').addEventListener('click', (ev) => {
  const el = ev.target.closest('.ticket');
  if (!el) return;
  sel = el.dataset.id; Sound.blip(); renderPlay();
});
$('tickets').addEventListener('contextmenu', (ev) => {
  const el = ev.target.closest('.ticket');
  if (!el) return;
  ev.preventDefault();
  sel = el.dataset.id; renderPlay();
  menu(ev.clientX, ev.clientY, sel);
});
$('detail').addEventListener('click', async (ev) => {
  const b = ev.target.closest('button');
  if (!b) return;
  if (b.dataset.node) { terminal(b.dataset.node); return; }
  const act = b.dataset.act;
  if (act === 'menu') { const r = b.getBoundingClientRect(); menu(r.left, r.bottom + 4, sel); return; }
  if (act === 'give-up' && !(await confirmGiveUp())) return;
  if (act) await play({ do: act, ticket: sel }).catch(() => {});
});

// ---------------------------------------------------------------- terminals and the context menu
const inDBCanvas = window.parent !== window;
function terminal(nodeId) {
  const n = (S.nodes || []).find((x) => x.nodeId === nodeId);
  if (!n) return;
  if (inDBCanvas) {
    window.parent.postMessage({ type: 'dbcanvas:open-terminal', stackId: S.stack.id, nodeId: n.nodeId, title: `${n.host} · Pager Panic` }, location.origin);
    toast(`Opening a terminal on ${n.host}`);
  } else copy(`docker exec -it ${n.container} bash`, `Copied: docker exec -it ${n.container} bash`);
}
function copy(text, msg) {
  const manual = () => ask('COPY', 'Select the text and copy it.', 'DONE', '', text);
  // The clipboard API exists only on https or localhost; anywhere else, show the text to copy.
  if (!navigator.clipboard?.writeText) { manual(); return; }
  navigator.clipboard.writeText(text).then(() => toast(msg || 'Copied'), manual);
}
function menu(x, y, ticketId) {
  const m = $('ctx');
  const t = S.tickets.find((z) => z.id === ticketId);
  const nodes = (S.nodes || []).map((n) => `<button data-node="${esc(n.nodeId)}">▸ Terminal: ${esc(n.host)}${n.role === 'control-plane' ? ' (control plane)' : ''}${t && n.host === t.worker ? ' ★' : ''}</button>`).join('');
  const docker = (S.nodes || []).map((n) => `<button data-copy="docker exec -it ${esc(n.container)} bash">⧉ docker exec: ${esc(n.host)}</button>`).join('');
  m.innerHTML = `${inDBCanvas ? '<small>DBCanvas terminal</small>' + nodes + '<button data-stack="1">▸ Open the stack in DBCanvas</button><hr>' : ''}
    <small>From a shell on the Docker host</small>${docker}
    ${t && t.state === 'open' ? `<hr><button data-act="check-now">✓ Check now</button>${t.hintCosts && t.hintsBought < t.hintCosts.length && S.mode !== 'marathon' ? `<button data-act="hint">? Hint (−${t.hintCosts[t.hintsBought]})</button>` : ''}<button data-act="give-up">✕ Give up</button>` : ''}`;
  m.style.left = `${Math.min(x, innerWidth - 280)}px`;
  m.style.top = `${Math.min(y, innerHeight - m.offsetHeight - 20)}px`;
  m.classList.remove('hidden');
  m.dataset.ticket = ticketId || '';
}
$('ctx').addEventListener('click', async (ev) => {
  const b = ev.target.closest('button');
  const m = $('ctx');
  m.classList.add('hidden');
  if (!b) return;
  if (b.dataset.node) terminal(b.dataset.node);
  else if (b.dataset.copy) copy(b.dataset.copy);
  else if (b.dataset.stack) window.parent.postMessage({ type: 'dbcanvas:open-stack', stackId: S.stack.id }, location.origin);
  else if (b.dataset.act) {
    if (b.dataset.act === 'give-up' && !(await confirmGiveUp())) return;
    await play({ do: b.dataset.act, ticket: m.dataset.ticket }).catch(() => {});
  }
});
addEventListener('click', (ev) => { if (!ev.target.closest('#ctx') && !ev.target.closest('[data-act="menu"]')) $('ctx').classList.add('hidden'); });
addEventListener('keydown', (ev) => {
  if (ev.target.tagName === 'INPUT' || dlgDone) return; // the dialog owns the keyboard while it is open
  const k = ev.key.toLowerCase();
  if (k === 'm') Sound.toggle();
  if (k === 'escape') $('ctx').classList.add('hidden');
  if (!S) return;
  if (k === 'p') play({ do: S.phase === 'paused' ? 'resume' : 'pause' }).catch(() => {});
  if (S.phase === 'playing' && ['1', '2', '3'].includes(k)) {
    const t = S.tickets[Number(k) - 1];
    if (t) { sel = t.id; Sound.blip(); renderPlay(); }
  }
  if (S.phase === 'playing' && k === 'h' && sel) play({ do: 'hint', ticket: sel }).catch(() => {});
  if (S.phase === 'playing' && k === 'c' && sel) play({ do: 'check-now', ticket: sel }).catch(() => {});
});
$('btnSound').onclick = () => Sound.toggle();
$('btnPause').onclick = () => play({ do: 'pause' }).catch(() => {});
$('btnResume').onclick = () => play({ do: 'resume' }).catch(() => {});
$('btnQuit').onclick = async () => {
  if (await ask('END THE SHIFT?', 'Open tickets count as unsolved, and the debrief shows how to fix each one.', 'END SHIFT', 'KEEP PLAYING')) {
    play({ do: 'quit' }).catch(() => {});
  }
};

// ---------------------------------------------------------------- the end
function showContinue() {
  if (!S || !S.debrief) return;
  const o = $('continue');
  $('contTitle').textContent = S.debrief.headline;
  o.classList.remove('hidden');
  let n = 10;
  $('contCount').textContent = String(n);
  clearInterval(continueTimer);
  continueTimer = setInterval(() => {
    n--;
    $('contCount').textContent = String(n);
    Sound.tone(220, 0.08, 'square', 0.04);
    if (n <= 0) { clearInterval(continueTimer); o.classList.add('hidden'); }
  }, 1000);
}
function hideContinue() { clearInterval(continueTimer); $('continue').classList.add('hidden'); }
$('btnDebrief').onclick = hideContinue;
$('btnReplay').onclick = async () => {
  hideContinue();
  wantSeed = S.debrief?.seed || 0;
  wantMode = S.debrief?.mode || 'arcade';
  await play({ do: 'title' }).catch(() => {});
  await play({ do: 'check-system' }).catch(() => {});
};
$('btnNew').onclick = $('btnAgain').onclick = async () => { hideContinue(); wantSeed = 0; wantMode = null; await play({ do: 'title' }).catch(() => {}); };

function renderOver() {
  const d = S.debrief;
  if (!d) return;
  $('overHeadline').textContent = d.headline;
  $('finalScore').textContent = d.score.toLocaleString();
  $('finalRank').textContent = d.rank || '—';
  $('finalStage').textContent = d.mode === 'practice' ? '—' : `${d.stage}`;
  $('finalCombo').textContent = `×${d.bestStreak}`;
  $('cleaning').textContent = d.cleaningUp ? 'Putting the cluster back…' : '';
  $('initials').classList.toggle('hidden', !S.enterInitials);
  $('newMedals').innerHTML = d.newMedals.map((m) => `<span class="medal new">${esc(m)}</span>`).join('');
  $('catResults').innerHTML = d.byCategory.map((c) => `<div class="cat"><span class="cap">${esc(c.title.toUpperCase())}</span><b>${c.cleared}/${c.total}</b><div class="bar"><i style="width:${c.total ? (100 * c.cleared / c.total) : 0}%"></i></div></div>`).join('');
  const key = JSON.stringify([d.tickets.length, d.cleaningUp, d.score]);
  if ($('debrief').dataset.key === key) return;
  $('debrief').dataset.key = key;
  const label = { cleared: 'CLEARED', expired: 'ESCALATED', 'given-up': 'GAVE UP', unsolved: 'UNSOLVED', 'not reached': 'NOT REACHED', skipped: 'SKIPPED' };
  $('debrief').innerHTML = d.tickets.map((t) => `<details class="dt" ${t.outcome !== 'cleared' && t.outcome !== 'skipped' ? 'open' : ''}>
      <summary><span class="out ${esc(t.outcome.split(' ')[0])}">${label[t.outcome] || esc(t.outcome)}</span>
        <span><b>${esc(t.title)}</b><br><span class="muted">${esc(catTitle(t.category))} · ${esc(t.kind)} · ${'★'.repeat(t.difficulty)}${t.boss ? ' · boss' : ''} · stage ${t.stage}</span></span>
        <span class="muted">${t.limit ? fmt(t.took) + ' / ' + fmt(t.limit) : fmt(t.took)}${t.hintsUsed ? ` · ${t.hintsUsed} hint(s)` : ''}${t.earned ? ` · +${t.earned}` : ''}</span></summary>
      <div class="body">
        ${t.error ? `<p class="wrong">Not played: ${esc(t.error)}</p>` : ''}
        ${t.brief ? `<span class="lbl">THE TICKET</span><div class="brief">${brief(t.brief)}</div>` : ''}
        ${t.wrong.length ? `<span class="lbl">STILL WRONG AT THE END</span><ul class="checks">${t.wrong.map((c) => `<li class="no"><span class="m">XX</span><span>${esc(c.name)}</span><small>got ${esc(c.got)}${c.want ? ' · want ' + esc(c.want) : ''}</small></li>`).join('')}</ul>` : ''}
        ${t.solution ? `<span class="lbl">SOLUTION</span><pre>${esc(t.solution)}</pre>` : ''}
        ${t.explain ? `<span class="lbl">WHY</span><p>${brief(t.explain)}</p>` : ''}
      </div></details>`).join('') || '<p class="muted">No tickets were drawn.</p>';
}
$('btnInitials').onclick = () => play({ do: 'initials', name: $('initialsInput').value }).then(() => { Sound.stage(); toast('Score saved'); }).catch(() => {});
$('initialsInput').addEventListener('keydown', (ev) => { if (ev.key === 'Enter') $('btnInitials').click(); });

function markdown(d) {
  const out = [`# Pager Panic: ${d.headline}`, '', `Mode ${d.mode} · score ${d.score} · rank ${d.rank || '—'} · stage ${d.stage} · best combo ×${d.bestStreak} · seed ${d.seed}`, ''];
  for (const t of d.tickets) {
    out.push(`## ${t.title} (${t.outcome})`, '', `${catTitle(t.category)} · ${t.kind} · difficulty ${t.difficulty}${t.boss ? ' · boss' : ''} · ${fmt(t.took)}${t.limit ? ' of ' + fmt(t.limit) : ''}`, '');
    if (t.brief) out.push(t.brief, '');
    if (t.wrong.length) { out.push('Still wrong at the end:', ''); for (const c of t.wrong) out.push(`- ${c.name}: got ${c.got}${c.want ? `, want ${c.want}` : ''}`); out.push(''); }
    if (t.solution) out.push('Solution:', '', '```sh', t.solution, '```', '');
    if (t.explain) out.push(t.explain, '');
  }
  return out.join('\n');
}
$('btnCopy').onclick = () => S?.debrief && copy(markdown(S.debrief), 'Debrief copied as Markdown');
$('btnJSON').onclick = () => {
  if (!S?.debrief) return;
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([JSON.stringify(S.debrief, null, 2)], { type: 'application/json' }));
  a.download = `pager-panic-${S.debrief.seed}.json`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 2000);
};

poll();
