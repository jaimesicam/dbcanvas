'use strict';
// Pager Panic's page. It polls the game server and draws what it says; every move is a
// POST to api/play. Paths are relative because DBCanvas serves the page under a prefix.

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const fmt = (secs) => { secs = Math.max(0, Math.round(secs)); return `${String(Math.floor(secs / 60)).padStart(2, '0')}:${String(secs % 60).padStart(2, '0')}`; };

let S = null;            // the last state
let lastSeq = -1;        // the last event animated
let sel = null;          // the selected ticket's id
let wantMode = null;     // the mode picked on the title screen
let wantTask = '';       // practice: the ticket picked
let wantSeed = 0;        // a replay's seed
let practiceCat = '';    // practice list filters: a category, a difficulty (0 is any)
let practiceDiff = 0;
let prechecked = false;  // the background systems check was asked for on this title screen
let starting = false;    // START was pressed and its answer is still coming
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
  on: (() => { try { return localStorage.getItem('pp-sound') !== 'off'; } catch { return true; } })(),
  ctx: null,
  tone(freq, dur, type = 'square', vol = 0.06, slide = 0, when = 0) {
    if (!this.on) return;
    try {
      this.ctx ||= new (window.AudioContext || window.webkitAudioContext)();
      if (this.ctx.state === 'suspended') this.ctx.resume();
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
  // The pointer moving onto something that can be chosen: a short, soft tick.
  hover() { this.tone(1760, 0.025, 'triangle', 0.025); },
  // Choosing it: a quick two-note rise.
  select() { this.tone(660, 0.05, 'square', 0.045); this.tone(990, 0.07, 'square', 0.045, 0, 0.05); },
  // A check the player ran came back not fixed: two low buzzes, falling.
  fail() { this.tone(196, 0.12, 'sawtooth', 0.07); this.tone(147, 0.2, 'sawtooth', 0.07, -40, 0.14); },
  toggle() {
    this.on = !this.on;
    try { localStorage.setItem('pp-sound', this.on ? 'on' : 'off'); } catch { /* private mode */ }
    $('btnSound').classList.toggle('off', !this.on);
    if (this.on) this.blip();
  },
};
$('btnSound').classList.toggle('off', !Sound.on);
// Sound is on from the start, but browsers hold audio until the first click or key.
for (const ev of ['pointerdown', 'keydown']) document.addEventListener(ev, () => Sound.ctx?.resume?.(), { capture: true });

// Everything that can be chosen sounds when the pointer reaches it and when it is chosen:
// every enabled button, and a ticket card. The sound button keeps its own blip.
const PICKABLE = 'button:not(:disabled):not(#btnSound), .ticket';
let hovered = null;
document.addEventListener('pointerover', (ev) => {
  if (ev.pointerType && ev.pointerType !== 'mouse') return; // a tap is a choice, not a hover
  const el = ev.target.closest?.(PICKABLE);
  if (el === hovered) return;
  hovered = el;
  if (el) Sound.hover();
});
document.addEventListener('click', (ev) => { if (ev.target.closest?.(PICKABLE)) Sound.select(); }, true);

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
    case 'notyet': {
      toast(e.text, 'error'); Sound.fail();
      const card = document.querySelector(`.ticket[data-id="${CSS.escape(e.ticket || '')}"]`);
      if (card) { card.classList.remove('nope'); void card.offsetWidth; card.classList.add('nope'); }
      break;
    }
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
// refresh fetches the state and draws it; poll does so on a timer, and a move refreshes
// at once so the screen answers the click without waiting for the next poll.
async function refresh() {
  try {
    const since = lastSeq < 0 ? 1e9 : lastSeq;
    S = await api('GET', 'api/state?since=' + since);
    if (lastSeq < 0) lastSeq = S.seq; // the first look animates nothing that already happened
    for (const e of S.events) {
      if (e.seq > lastSeq) { animate(e); lastSeq = e.seq; }
    }
    render();
    sayHello();
  } catch (e) {
    $('connStatus').textContent = 'The game server is not answering…';
  }
}
async function poll() {
  await refresh();
  setTimeout(poll, S && (S.phase === 'playing' || S.phase === 'checking') ? 500 : 1000);
}

function render() {
  const phase = S.phase;
  let screen = { title: 'title', checking: 'check', ready: 'check', playing: 'play', paused: 'play', ending: 'over', over: 'over' }[phase] || 'title';
  // A ticket just cleared stays on the field for a few seconds, even when it ended the shift.
  if (phase === 'ending' && (S.tickets || []).some((t) => t.state === 'cleared')) screen = 'play';
  for (const s of ['title', 'check', 'play', 'over']) $('screen-' + s).classList.toggle('hidden', s !== screen);
  $('pause').classList.toggle('hidden', phase !== 'paused');
  if (phase !== lastPhase) {
    if (phase === 'title' || phase === 'checking') document.body.classList.remove('lights-out');
    if (phase === 'playing' && lastPhase !== 'paused') { sel = null; shownScore = 0; }
    // The shift began: from START at once, or by itself when the check it waited on passed.
    if (phase === 'playing' && ['title', 'checking', 'ready'].includes(lastPhase)) Sound.stage();
    if (phase !== 'title') prechecked = false;
    lastPhase = phase;
  }
  if (screen === 'title') renderTitle();
  if (screen === 'check') renderCheck();
  if (screen === 'play') renderPlay();
  if (screen === 'over') renderOver();
  if (screen !== 'play') alarm(0.05);
}

// ---------------------------------------------------------------- title
// The title screen picks a mode, then START. Picking only chooses: the panel below shows
// the mode's rules (and for practice, the ticket list), and nothing starts until START.
// Meanwhile a systems check runs in the background, so START usually begins at once.
const MODES = ['arcade', 'marathon', 'practice'];
// setHTML redraws el only when what it would draw changed, so a poll does not reset the
// pointer's hover or the keyboard's focus.
const drawn = new WeakMap();
function setHTML(el, html) {
  if (drawn.get(el) === html) return;
  drawn.set(el, html);
  el.innerHTML = html;
}
function renderTitle() {
  $('bankTitle').textContent = S.bankTitle ? `${S.bankTitle} · ${S.stack?.frame || ''}` : 'loading the task bank…';
  $('clusterName').textContent = S.stack?.frame || 'the cluster';
  const waiting = S.waiting || '';
  $('connStatus').textContent = waiting ? waiting + '…' : '';
  document.querySelectorAll('.mode').forEach((b) => {
    const on = b.dataset.mode === wantMode;
    b.disabled = !!waiting;
    b.classList.toggle('on', on);
    b.setAttribute('aria-checked', String(on));
  });
  const hs = (S.highScores || []).slice(0, 10);
  $('highScores').innerHTML = hs.length ? hs.map((h) => `<li><b>${esc(h.initials)}</b><span>${h.score.toLocaleString()}</span><small>${esc(h.mode)} · ${esc(h.rank)}</small></li>`).join('') : '<li class="muted">No shifts played yet.</li>';
  $('medalShelf').innerHTML = (S.medals || []).length ? S.medals.map((m) => `<span class="medal" title="${esc(m.why)}">${esc(m.title)}</span>`).join('') : '<span class="muted">Medals you earn are kept here.</span>';
  renderModePanel();
  renderSysCheck();
  if (!waiting && !prechecked) {
    prechecked = true;
    api('POST', 'api/play', { do: 'precheck' }).catch(() => { prechecked = false; });
  }
}

// modeRules is what the panel says about a mode, from the bank's own numbers.
function modeRules(mode) {
  const mins = Math.round((S.clock?.shift || 3600) / 60);
  const stages = S.clock?.stages || 1;
  const lives = S.maxLives || 3;
  return {
    arcade: [
      `<b>${lives} lives.</b> A ticket that runs out of time escalates and costs one.`,
      `<b>${stages} stage${stages > 1 ? 's' : ''} over ${mins} minutes,</b> each harder than the last, with a boss in the final one.`,
      '<b>3 pauses</b> per shift.',
      '<b>Hints cost points.</b> Fast fixes build a combo.',
    ],
    marathon: [
      `<b>${mins} minutes,</b> ending on the clock.`,
      '<b>No lives.</b> A ticket that runs out of time scores nothing and breaks your combo.',
      '<b>No pauses, no hints.</b>',
      'Play for the highest score.',
    ],
    practice: [
      '<b>One ticket you choose,</b> with no time limit.',
      '<b>Hints are free.</b> Pause whenever you like.',
      'The debrief shows the solution.',
    ],
  }[mode] || [];
}
function renderModePanel() {
  const rules = $('modeRules');
  $('modeHint').classList.toggle('hidden', !!wantMode);
  rules.classList.toggle('hidden', !wantMode);
  const html = modeRules(wantMode).map((r) => `<li>${r}</li>`).join('');
  setHTML(rules, html);
  $('practicePick').classList.toggle('hidden', wantMode !== 'practice');
  if (wantMode === 'practice') renderPracticeList();
  const go = $('btnGo');
  go.textContent = starting ? 'STARTING…' : wantMode === 'practice' ? 'START PRACTICE' : 'START SHIFT';
  go.disabled = !wantMode || !!S.waiting || starting || (wantMode === 'practice' && !wantTask);
  go.title = !wantMode ? 'Pick a mode first' : wantMode === 'practice' && !wantTask ? 'Pick a ticket to practise' : '';
}
function renderPracticeList() {
  const cats = S.categories || [];
  const tasks = S.tasks || [];
  const maxDiff = Math.max(1, ...tasks.map((t) => t.difficulty || 1));
  const chip = (attr, val, on, label) => `<button class="filter ${on ? 'on' : ''}" ${attr}="${esc(val)}" aria-pressed="${on}">${label}</button>`;
  const filters = chip('data-cat', '', !practiceCat, 'ALL')
    + cats.map((c) => chip('data-cat', c.id, practiceCat === c.id, esc(c.title))).join('')
    + '<span class="sep"></span>'
    + chip('data-diff', '0', !practiceDiff, 'ANY ★')
    + Array.from({ length: maxDiff }, (_, i) => chip('data-diff', String(i + 1), practiceDiff === i + 1, '★'.repeat(i + 1))).join('');
  setHTML($('practiceFilters'), filters);
  const names = Object.fromEntries(cats.map((c) => [c.id, c.title]));
  const shown = tasks.filter((t) => (!practiceCat || t.category === practiceCat) && (!practiceDiff || t.difficulty === practiceDiff));
  const html = shown.map((t) => `<button data-task="${esc(t.id)}" role="option" aria-selected="${t.id === wantTask}" class="${t.id === wantTask ? 'on' : ''}" ${t.unsupported ? 'disabled' : ''}>
      <span><span class="chip cat-${esc(t.category)}">${esc(names[t.category] || t.category)}</span> ${esc(t.title)} ${t.boss ? '<span class="chip boss">boss</span>' : ''}</span>
      <small>${'★'.repeat(t.difficulty)}${t.unsupported ? ' · ' + esc(t.unsupported) : ''}</small></button>`).join('')
    || '<p class="muted">No ticket matches these filters.</p>';
  setHTML($('practiceList'), html);
}
const ago = (secs) => (secs < 60 ? 'just now' : `${Math.round(secs / 60)} min ago`);
// renderSysCheck is the line beside START: how the background systems check is doing.
function renderSysCheck() {
  const d = S.diag;
  let state = '', text, prog = null;
  if (S.waiting) text = 'Systems check: waiting for DBCanvas…';
  else if (!d) text = 'Systems check: starting…';
  else if (!d.done) { state = 'run'; prog = d.progress || 0; text = `Checking the cluster · ${Math.round(prog * 100)}%`; }
  else if (d.fresh) { state = 'ok'; text = `Systems OK · checked ${ago(d.age || 0)}`; }
  else if (d.ok) text = `Last check ${ago(d.age || 0)} · START checks again`;
  else { state = 'fail'; text = `Systems check failed${d.error ? ': ' + d.error : ''} · START checks again`; }
  $('sysLamp').className = 'lamp ' + state;
  $('sysText').textContent = text;
  $('sysBar').classList.toggle('hidden', prog === null);
  if (prog !== null) $('sysBar').firstElementChild.style.width = `${Math.max(3, Math.round(prog * 100))}%`;
}
function pickMode(mode) {
  if (!MODES.includes(mode) || S?.waiting) return;
  if (mode !== wantMode) { wantTask = ''; wantSeed = 0; }
  wantMode = mode;
  renderTitle();
}
// startShift is START: the game begins at once after a fresh check, or runs the check
// (the check screen shows it) and begins when it passes.
async function startShift() {
  if (!wantMode || starting) return;
  if (wantMode === 'practice' && !wantTask) { toast('Pick a ticket to practise'); return; }
  starting = true;
  renderModePanel();
  try {
    await play({ do: 'start', mode: wantMode, task: wantTask, seed: wantSeed });
    wantSeed = 0;
  } catch { /* the toast says why */ }
  starting = false;
  await refresh();
}
$('modes').addEventListener('click', (ev) => {
  const b = ev.target.closest('.mode');
  if (b && !b.disabled) pickMode(b.dataset.mode);
});
$('practiceFilters').addEventListener('click', (ev) => {
  const b = ev.target.closest('button');
  if (!b) return;
  if (b.dataset.cat !== undefined) practiceCat = b.dataset.cat;
  if (b.dataset.diff !== undefined) practiceDiff = Number(b.dataset.diff) || 0;
  renderPracticeList();
});
$('practiceList').addEventListener('click', (ev) => {
  const b = ev.target.closest('button[data-task]');
  if (!b || b.disabled) return;
  wantTask = b.dataset.task;
  renderModePanel();
});
$('practiceList').addEventListener('dblclick', (ev) => {
  const b = ev.target.closest('button[data-task]');
  if (b && !b.disabled) { wantTask = b.dataset.task; startShift(); }
});
$('btnGo').onclick = startShift;
// titleKeys: arrows choose the mode (and in practice, the ticket); Enter starts.
function titleKeys(ev, k) {
  if (k === 'arrowleft' || k === 'arrowright') {
    ev.preventDefault();
    const i = MODES.indexOf(wantMode);
    const next = i < 0 ? 0 : (i + (k === 'arrowright' ? 1 : MODES.length - 1)) % MODES.length;
    pickMode(MODES[next]);
    document.querySelector(`.mode[data-mode="${MODES[next]}"]`)?.focus();
  } else if ((k === 'arrowdown' || k === 'arrowup') && wantMode === 'practice') {
    ev.preventDefault();
    const opts = [...document.querySelectorAll('#practiceList button[data-task]:not(:disabled)')];
    if (!opts.length) return;
    const i = opts.findIndex((b) => b.dataset.task === wantTask);
    const b = opts[i < 0 ? 0 : (i + (k === 'arrowdown' ? 1 : opts.length - 1)) % opts.length];
    wantTask = b.dataset.task;
    renderModePanel();
    $('practiceList').querySelector(`[data-task="${CSS.escape(wantTask)}"]`)?.scrollIntoView({ block: 'nearest' });
  } else if (k === 'enter') {
    ev.preventDefault();
    startShift();
  }
}

// ---------------------------------------------------------------- systems check
// The check screen shows a check START is waiting on, or one run again by hand: a bar
// weighted by how long each step took last time, and each step's own time.
function renderCheck() {
  const d = S.diag || { steps: [], progress: 0, elapsed: 0 };
  const mode = S.pending?.mode || wantMode;
  const label = { arcade: 'ARCADE', marathon: 'MARATHON', practice: 'PRACTICE' }[mode] || '';
  $('checkFor').textContent = `${label ? label + ' · ' : ''}${S.bankTitle || ''} on ${S.stack?.frame || 'the cluster'}`;
  const mark = { pending: '···', running: '>>', ok: 'OK', warn: '!!', fail: 'XX' };
  const steps = d.steps || [];
  const time = (st) => (st.state === 'running' ? `${fmt(st.for || 0)}${st.est ? ` / ~${fmt(st.est)}` : ''}`
    : ['ok', 'warn', 'fail'].includes(st.state) ? fmt(st.for || 0) : '');
  const html = steps.length || d.done
    ? steps.map((st) => `<li class="${esc(st.state)}"><span class="st">${mark[st.state] || '··'}</span><b>${esc(st.name)}</b><em class="t">${time(st)}</em>${st.detail ? `<small>${esc(st.detail)}</small>` : ''}</li>`).join('')
    : `<li class="running"><span class="st">>></span><b>Contacting DBCanvas</b><em class="t">${fmt(d.elapsed || 0)}</em><small>The check starts on DBCanvas's next sync.</small></li>`;
  setHTML($('diagSteps'), html);
  const pct = Math.round((d.progress || 0) * 100);
  const bar = $('diagBar');
  bar.querySelector('i').style.width = `${Math.max(3, pct)}%`;
  bar.querySelector('span').textContent = `${pct}%`;
  bar.classList.toggle('failed', !!(d.done && !d.ok));
  const usual = steps.reduce((n, st) => n + (st.est || 0), 0);
  $('diagTime').textContent = d.done ? `took ${fmt(d.elapsed || 0)}` : `${fmt(d.elapsed || 0)} so far${usual ? ` · usually about ${fmt(usual)}` : ''}`;
  const v = $('diagVerdict');
  const ready = S.phase === 'ready';
  if (ready) { v.textContent = 'SYSTEMS NOMINAL'; v.className = 'verdict ok'; }
  else if (d.done && !d.ok) { v.textContent = 'SYSTEMS CHECK FAILED' + (d.error ? ' · ' + d.error : ''); v.className = 'verdict fail'; }
  else if (d.done && d.ok) { v.textContent = 'SYSTEMS NOMINAL · STARTING…'; v.className = 'verdict ok'; }
  else { v.textContent = S.pending ? 'checking… the shift starts when it passes' : 'checking…'; v.className = 'verdict'; }
  $('btnStart').classList.toggle('hidden', !ready);
  $('btnRecheck').classList.toggle('hidden', !(d.done && !d.ok) || ready);
  if (ready && !renderCheck.chimed) { renderCheck.chimed = true; Sound.seq([523, 784], 'square', 0.1); }
  if (!ready) renderCheck.chimed = false;
}
// The check screen's START, after a check run by hand: begins with the mode picked on
// the title screen, or goes back there to pick one.
$('btnStart').onclick = () => (wantMode ? startShift() : play({ do: 'title' }).catch(() => {}));
$('btnRecheck').onclick = () => play({ do: 'check-system' }).then(refresh).catch(() => {});
$('btnBack').onclick = () => play({ do: 'quit' }).then(refresh).catch(() => {});

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
    if (el.classList.contains('next-card')) continue;
    if (!ids.has(el.dataset.id)) { ticketBoxes[el.dataset.id] = el.getBoundingClientRect(); el.remove(); }
  }
  let red = 0, num = 0;
  S.tickets.forEach((t) => {
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
    const fixed = t.state === 'cleared';
    if (fixed) {
      if (!el.classList.contains('cleared')) {
        el.className = 'ticket cleared' + (sel === t.id ? ' sel' : '');
        el.querySelector('.left').style.strokeDashoffset = '0';
        el.querySelector('.ring span').textContent = '✓';
        el.querySelector('.t-title').innerHTML = `<span class="ok-tag">FIXED</span> ${esc(t.title)}`;
        el.querySelector('.t-meta').innerHTML = `<span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span>${t.earned ? `<span>+${t.earned.toLocaleString()} pts</span>` : ''}<span>in ${fmt(t.took)}</span>`;
      }
      el.classList.toggle('sel', sel === t.id);
      return;
    }
    const n = ++num;
    el.classList.toggle('incoming', incoming);
    el.classList.toggle('boss', !!t.boss);
    el.classList.toggle('sel', sel === t.id);
    el.classList.toggle('amber', !incoming && t.limit > 0 && frac < 0.5 && t.remaining > 30);
    el.classList.toggle('red', !incoming && t.limit > 0 && t.remaining <= 30 && t.remaining > 10);
    el.classList.toggle('shake', !incoming && t.limit > 0 && t.remaining <= 10);
    if (!incoming && t.limit > 0 && t.remaining <= 30) red++;
    el.querySelector('.left').style.strokeDashoffset = String(169.6 * (1 - Math.max(0, Math.min(1, frac))));
    el.querySelector('.ring span').textContent = incoming ? '...' : (t.limit > 0 ? fmt(t.remaining) : '∞');
    el.querySelector('.t-title').innerHTML = incoming ? `<span class="ringing">INCOMING PAGE…</span>` : `<span class="muted">${n}</span> ${esc(t.title)}`;
    let bar = el.querySelector('.t-prog');
    if (incoming) {
      if (!bar) { bar = document.createElement('div'); bar.className = 't-prog'; bar.innerHTML = '<i></i>'; el.appendChild(bar); }
      const p = S.next && S.next.stage === 'setup' ? S.next.progress : incomingPrep(t).progress;
      bar.firstChild.style.width = `${Math.max(3, Math.round(p * 100))}%`;
    } else if (bar) bar.remove();
    el.querySelector('.t-meta').innerHTML = `${t.boss ? '<span class="chip boss">boss</span>' : ''}<span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span><span class="chip">${esc(t.kind)}</span><span class="stars">${'★'.repeat(t.difficulty)}</span><span>${(t.points * (t.boss ? 3 : 1)).toLocaleString()} pts</span>${t.checking ? '<span class="muted">checking…</span>' : ''}`;
  });
  // While the next ticket is still to be drawn, a card in the queue counts down to it.
  let nc = box.querySelector('.next-card');
  if (S.next && S.next.stage === 'waiting') {
    if (!nc) { nc = document.createElement('div'); nc.className = 'next-card'; nc.innerHTML = '<b>NEXT PAGE</b><span></span><div class="t-prog"><i></i></div>'; box.appendChild(nc); }
    nc.querySelector('span').textContent = S.next.in > 0 ? `in ${fmt(S.next.in)} · then set up on the cluster` : 'coming in now…';
    nc.querySelector('.t-prog i').style.width = `${Math.max(3, Math.round(S.next.progress * 100))}%`;
    if (box.lastChild !== nc) box.appendChild(nc);
  } else if (nc) nc.remove();
  if (!S.tickets.length && !nc) box.innerHTML = `<div class="quiet">All quiet… for now.</div>`;
  else box.querySelectorAll('.quiet').forEach((n) => n.remove());
  if (sel && !ids.has(sel)) sel = null;
  if (!sel) { const first = S.tickets.find((t) => t.state === 'open') || S.tickets.find((t) => t.state === 'cleared'); if (first) sel = first.id; }
  renderDetail();
  Bench.render();
  App.render();
  const stageLevel = c.practicing ? 0.1 : (c.stage - 1) / Math.max(1, c.stages - 1);
  alarm(Math.min(1, stageLevel * 0.6 + red * 0.2));
}
function catTitle(id) { return (S.categories || []).find((c) => c.id === id)?.title || id; }
function renderDetail() {
  const d = $('detail');
  const t = S.tickets.find((x) => x.id === sel);
  if ((!t || t.state === 'incoming') && (S.next || t)) { drawPrep(d, S.next || incomingPrep(t)); return; }
  if (!t) { d.dataset.key = ''; d.innerHTML = '<div class="empty">Tickets appear here. Pick one to read it.<br>Right-click a ticket for more ways in.</div>'; return; }
  if (t.state === 'cleared') {
    if (d.dataset.key === 'cleared:' + t.id) return;
    d.dataset.key = 'cleared:' + t.id;
    const checks = (t.checks || []).map((c) => `<li class="ok"><span class="m">OK</span><span>${esc(c.name)}</span></li>`).join('');
    d.innerHTML = `<div class="d-head"><div><h4>${esc(t.title)}</h4><div class="t-meta"><span class="chip ok">fixed</span><span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span></div></div></div>
      <div class="fixed-note">✓ Fixed in ${fmt(t.took)}${t.earned ? ` · +${t.earned.toLocaleString()} pts` : ''}</div><ul class="checks">${checks}</ul>`;
    return;
  }
  const m = t.monitor;
  const key = JSON.stringify([t.id, t.brief, t.hints, t.checks, t.checking, t.hintsBought, m && [m.ok, m.status, m.error]]);
  if (d.dataset.key === key) return;
  d.dataset.key = key;
  const nextCost = t.hintCosts && t.hintsBought < t.hintCosts.length ? t.hintCosts[t.hintsBought] : null;
  const checks = (t.checks || []).map((c) => `<li class="${c.ok ? 'ok' : 'no'}"><span class="m">${c.ok ? 'OK' : 'XX'}</span><span>${esc(c.name)}${c.guard ? ' <span class="chip">rule</span>' : ''}</span>${c.ok ? '' : `<small>got ${esc(c.got)}</small>`}</li>`).join('');
  d.innerHTML = `<div class="d-head"><div><h4>${esc(t.title)}</h4>
    <div class="t-meta">${t.boss ? '<span class="chip boss">boss</span>' : ''}<span class="chip cat-${esc(t.category)}">${esc(catTitle(t.category))}</span><span class="chip">${esc(t.kind)}</span><span class="stars">${'★'.repeat(t.difficulty)}</span>${t.extra ? `<span class="chip ok">+${t.extra}s head start</span>` : ''}</div></div>
    ${m ? `<button class="app-chip ${m.ok ? 'ok' : 'bad'}" data-act="show-app" title="Show the app"><i></i>${esc(m.title)} · ${esc(appState(m))}</button>` : ''}</div>
    <div class="brief">${brief(t.brief)}</div>
    ${(t.hints || []).map((h) => `<div class="hint">${brief(h)}</div>`).join('')}
    <ul class="checks">${checks || '<li class="muted">Checked every 10 seconds; it clears the moment the cluster is right.</li>'}</ul>
    <div class="actions">
      <button class="btn small hot" data-act="check-now">CHECK NOW</button>
      ${nextCost !== null && S.mode !== 'marathon' ? `<button class="btn small" data-act="hint">HINT −${nextCost}</button>` : ''}
      <button class="btn small" data-act="give-up">GIVE UP</button>
      <button class="btn small" data-act="menu">MORE ▾</button>
    </div>`;
}
// incomingPrep is the preparing panel's model for one ticket being set up while others
// are open (the game reports S.next only while none is).
function incomingPrep(t) {
  const est = Math.max(t.setupEst || 30, (t.setupFor || 0) * 1.15);
  return { stage: 'setup', in: 0, setupFor: t.setupFor || 0, setupEst: t.setupEst || 30, cleaning: 0, progress: Math.min(0.97, (t.setupFor || 0) / est) };
}
// drawPrep shows the next ticket being prepared: a progress bar and what is happening
// now, so a quiet stretch never looks like a hang. It is built once and then updated in
// place, so the bar moves smoothly between polls.
function drawPrep(d, n) {
  if (d.dataset.key !== 'prep') {
    d.dataset.key = 'prep';
    d.innerHTML = `<div class="prep"><h4 class="prep-title">PREPARING THE NEXT TICKET</h4>
      <div class="prep-bar"><i></i><span></span></div>
      <ul class="prep-steps"></ul>
      <p class="prep-note">The cluster is being set up for it; this takes a little while. Its clock starts when it lands.</p></div>`;
  }
  const pct = Math.round((n.progress || 0) * 100);
  d.querySelector('.prep-bar i').style.width = `${Math.max(3, pct)}%`;
  d.querySelector('.prep-bar span').textContent = `${pct}%`;
  const left = Math.max(0, Math.round(n.setupEst - n.setupFor));
  const steps = [];
  if (n.cleaning) steps.push(['run', 'Removing the last fault from the cluster', '']);
  if (n.stage === 'waiting') {
    steps.push(['run', 'The next page comes in', n.in > 0 ? `in ${fmt(n.in)}` : 'now']);
    steps.push(['wait', 'Set up its fault on the cluster', `about ${fmt(n.setupEst)}`]);
  } else {
    steps.push(['ok', 'The next page came in', '']);
    steps.push(['run', 'Setting up its fault on the cluster', n.setupFor > n.setupEst ? `taking longer than usual · ${fmt(n.setupFor)}` : `about ${fmt(left)} left`]);
  }
  const mark = { ok: '✓', run: '<i class="spin-dot"></i>', wait: '·' };
  const html = steps.map(([st, text, sub]) => `<li class="${st}"><span class="m">${mark[st]}</span><span>${esc(text)}</span><small>${esc(sub)}</small></li>`).join('');
  const ul = d.querySelector('.prep-steps');
  if (ul.innerHTML !== html) ul.innerHTML = html;
}
function appState(m) { return m.error ? 'DOWN' : (m.ok ? `UP · ${m.status}` : `HTTP ${m.status}`); }
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
  sel = el.dataset.id; renderPlay();
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
  const act = b.dataset.act;
  if (act === 'show-app') { App.nudge(); return; }
  if (act === 'menu') { const r = b.getBoundingClientRect(); menu(r.left, r.bottom + 4, sel); return; }
  if (act === 'give-up' && !(await confirmGiveUp())) return;
  if (act) await play({ do: act, ticket: sel }).catch(() => {});
});

// ---------------------------------------------------------------- terminals
// Inside DBCanvas a terminal is drawn here, in the workbench. DBCanvas opens the node's
// shell and relays it over a MessagePort (dbcanvas:term-attach), so this page gets a shell
// and never a credential. A terminal can also pop out into a DBCanvas window, and outside
// DBCanvas the docker exec command is the way in.
//
// In a DBCanvas shared session the terminal is everyone's: DBCanvas offers it to every
// copy of this page (dbcanvas:term-shared, with its port), and tells each whether it has
// control and so the keyboard (control). Without it the terminal is watched, not typed in.
const inDBCanvas = window.parent !== window;
const nodeOf = (id) => (S?.nodes || []).find((x) => x.nodeId === id);
const dockerCmd = (n) => `docker exec -it ${n.container} bash`;
const nodeLabel = (n) => `${n.host}${n.role === 'control-plane' ? ' (control plane)' : ''}`;
function popOut(nodeId) {
  const n = nodeOf(nodeId);
  if (!n) return;
  if (!inDBCanvas) { copy(dockerCmd(n), `Copied: ${dockerCmd(n)}`); return; }
  window.parent.postMessage({ type: 'dbcanvas:open-terminal', stackId: S.stack.id, nodeId: n.nodeId, title: `${n.host} · Pager Panic` }, location.origin);
  toast(`Opening a DBCanvas terminal on ${n.host}`);
}
const TERM_THEME = { background: '#0d081b', foreground: '#e9e4ff', cursor: '#39ffb6', cursorAccent: '#0d081b', selectionBackground: '#47c8ff55',
  black: '#140d26', red: '#ff4d4d', green: '#39ffb6', yellow: '#ffcb3d', blue: '#47c8ff', magenta: '#ff3d7f', cyan: '#5ee6e6', white: '#e9e4ff',
  brightBlack: '#6b6290', brightRed: '#ff7a7a', brightGreen: '#7dffd0', brightYellow: '#ffe07a', brightBlue: '#8adcff', brightMagenta: '#ff7aa8', brightCyan: '#9af2f2', brightWhite: '#ffffff' };

const Bench = {
  sessions: [],  // {id, nodeId, host, n, term, fit, el, port, status}
  tab: null,     // the shown session's id
  count: 0,
  lastKey: '',

  // open shows the node's terminal, opening one if it has none (or always, with fresh).
  open(nodeId, fresh) {
    const n = nodeOf(nodeId);
    if (!n) return;
    if (!inDBCanvas || !window.Terminal || !window.FitAddon) { popOut(nodeId); return; }
    if (!fresh) {
      const have = this.sessions.find((x) => x.nodeId === nodeId && x.status !== 'closed');
      if (have) { this.show(have.id); return; }
    }
    const ch = new MessageChannel();
    const s = this.mount(nodeId, n.host, ch.port1, '');
    s.timer = setTimeout(() => { if (s.status === 'connecting') s.ended('DBCanvas did not answer; right-click a ticket to pop a terminal out'); }, 12000);
    window.parent.postMessage({ type: 'dbcanvas:term-attach', stackId: S.stack.id, nodeId, title: `${n.host} · Pager Panic` }, location.origin, [ch.port2]);
    this.show(s.id);
  },
  // adopt shows a shared terminal DBCanvas offered: one someone else in the session
  // opened, or this page's own from before it loaded again.
  adopt(m, port) {
    if (!window.Terminal || !window.FitAddon) { port.close(); return; }
    if (this.sessions.some((x) => x.shared === m.id && x.status !== 'closed')) { port.close(); return; }
    const host = nodeOf(m.nodeId)?.host || String(m.title || m.nodeId).split(' · ')[0];
    const s = this.mount(String(m.nodeId || ''), host, port, String(m.id));
    this.show(s.id, true);
  },
  // mount draws a terminal and wires it to the port DBCanvas relays its shell over.
  mount(nodeId, host, port, shared) {
    const id = `t${++this.count}`;
    const el = document.createElement('div');
    el.className = 'term-host';
    $('termPane').appendChild(el);
    const term = new window.Terminal({ fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace', fontSize: 13, lineHeight: 1.1,
      cursorBlink: true, scrollback: 5000, allowProposedApi: false, theme: TERM_THEME });
    const fit = new window.FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(el);
    const same = this.sessions.filter((x) => x.nodeId === nodeId).length;
    const s = { id, nodeId, host, n: same + 1, term, fit, el, port, status: 'connecting', shared, driving: true };
    this.sessions.push(s);
    const enc = new TextEncoder();
    const send = (m) => { if (s.status !== 'closed') s.port.postMessage(m); };
    s.ended = (why) => {
      if (s.status === 'closed') return;
      s.status = 'closed';
      clearTimeout(s.timer);
      term.write(`\r\n\x1b[33m[${why}]\x1b[0m\r\n`);
      this.render(true);
    };
    s.port.onmessage = (e) => {
      const m = e.data || {};
      if (m.type === 'data') term.write(typeof m.data === 'string' ? m.data : new Uint8Array(m.data));
      else if (m.type === 'open') {
        s.status = 'open'; clearTimeout(s.timer);
        this.fitOne(s);
        if (s.driving) send({ type: 'resize', cols: term.cols, rows: term.rows });
        this.render(true);
      } else if (m.type === 'reset') term.reset();
      else if (m.type === 'shared') { s.shared = String(m.id || ''); this.render(true); }
      else if (m.type === 'control') {
        s.driving = !!m.driving;
        term.options.disableStdin = !s.driving;
        if (s.driving && s.status === 'open') send({ type: 'resize', cols: term.cols, rows: term.rows });
        this.render(true);
      } else if (m.type === 'unavailable') {
        if (m.stay) { toast(m.reason || 'A terminal cannot be opened here', 'error'); this.close(s.id, true); return; }
        s.ended(m.reason || 'not available here');
        popOut(nodeId);
      } else if (m.type === 'closed' || m.type === 'error') s.ended(s.shared ? 'the shared terminal was closed' : 'session closed');
    };
    term.onData((d) => { if (s.driving) send({ type: 'data', data: enc.encode(d) }); });
    term.onBinary((d) => { if (s.driving) send({ type: 'data', data: Uint8Array.from(d, (c) => c.charCodeAt(0) & 255) }); });
    term.onResize(({ cols, rows }) => { if (s.status === 'open' && s.driving) send({ type: 'resize', cols, rows }); });
    return s;
  },
  // close takes the terminal off the workbench. A shared one closes for everyone when
  // this player has control, and otherwise only here. quiet: DBCanvas already ended it.
  close(id, quiet) {
    const i = this.sessions.findIndex((x) => x.id === id);
    if (i < 0) return;
    const s = this.sessions[i];
    try { if (s.status !== 'closed' && !quiet) s.port.postMessage({ type: 'close' }); } catch { /* */ }
    s.status = 'closed';
    clearTimeout(s.timer);
    s.port.close();
    s.term.dispose();
    s.el.remove();
    this.sessions.splice(i, 1);
    if (this.tab === id) this.tab = this.sessions[Math.max(0, i - 1)]?.id || null;
    this.render(true);
  },
  show(tab, noFocus) {
    this.tab = tab;
    this.render(true);
    const s = this.sessions.find((x) => x.id === tab);
    if (s && !noFocus) setTimeout(() => s.term.focus(), 0);
  },
  fitOne(s) {
    if (!s || !s.el.offsetWidth || !s.el.offsetHeight) return;
    try { s.fit.fit(); } catch { /* not laid out yet */ }
  },
  fitAll() { for (const s of this.sessions) if (s.el.style.display !== 'none') this.fitOne(s); },

  // render draws the tabs and shows the right pane. It runs on every poll, so it only
  // touches the DOM when something it draws changed (or force).
  render(force) {
    if (!S) return;
    const t = S.tickets.find((x) => x.id === sel && x.state === 'open');
    if (this.tab && !this.sessions.some((x) => x.id === this.tab)) this.tab = this.sessions[this.sessions.length - 1]?.id || null;
    const termTab = this.tab;
    const key = JSON.stringify([termTab, this.sessions.map((x) => [x.id, x.status, x.driving]), (S.nodes || []).map((n) => n.nodeId), t?.worker]);
    if (!force && key === this.lastKey) return;
    this.lastKey = key;

    const star = (n) => (t && n.host === t.worker ? ' ★' : '');
    const tabs = this.sessions.map((x) => {
      const watch = x.shared && !x.driving;
      const tip = `${x.host}${x.shared ? (watch ? ' · shared: watching, whoever has control types' : ' · shared: everyone in the session sees it') : ''}`;
      const closeTip = x.shared && x.driving ? 'Close for everyone' : 'Close';
      return `<button class="tab ${x.id === termTab ? 'on' : ''}${watch ? ' watch' : ''}" data-tab="${x.id}" title="${esc(tip)}"><span class="lamp ${x.status}"></span>${esc(x.host)}${x.n > 1 ? ' #' + x.n : ''}${x.shared ? `<span class="shared-tag">${watch ? 'WATCHING' : 'SHARED'}</span>` : ''}<span class="x" data-close="${x.id}" title="${closeTip}">×</span></button>`;
    }).join('');
    const adds = inDBCanvas ? (S.nodes || []).map((n) => `<button class="add" data-add="${esc(n.nodeId)}" title="New terminal on ${esc(nodeLabel(n))}">+ ${esc(n.host)}${star(n)}</button>`).join('') : '';
    const tools = termTab && inDBCanvas ? `<button class="tool" data-pop="${termTab}" title="Pop this terminal out into a DBCanvas window">⧉</button>` : '';
    $('benchTabs').innerHTML = `${tabs}${adds}<span class="spacer"></span>${tools}`;

    for (const x of this.sessions) x.el.style.display = x.id === termTab ? '' : 'none';
    const launch = $('termLaunch');
    launch.classList.toggle('hidden', !!termTab);
    if (!termTab) launch.innerHTML = this.launcher(t);
    requestAnimationFrame(() => this.fitAll());
  },
  launcher(t) {
    const nodes = S.nodes || [];
    const docker = nodes.map((n) => `<div class="cmd"><span class="who">${esc(n.host)}${t && n.host === t.worker ? ' ★' : ''}</span><code>${esc(dockerCmd(n))}</code><button data-copy="${esc(dockerCmd(n))}">COPY</button></div>`).join('');
    if (!inDBCanvas) {
      return `<h5>TERMINALS</h5><p>Terminals open right here when the game is opened from its node in DBCanvas (Play). From a shell on the Docker host:</p>${docker}`;
    }
    const btns = nodes.map((n) => `<button class="node ${t && n.host === t.worker ? 'star' : ''}" data-add="${esc(n.nodeId)}"><b>▸ ${esc(n.host)}${t && n.host === t.worker ? ' ★' : ''}</b><small>${n.role === 'control-plane' ? 'control plane · kubectl is here' : (t && n.host === t.worker ? "this ticket's worker" : 'worker')}</small></button>`).join('');
    return `<h5>OPEN A SHELL ON A NODE</h5><div class="nodes">${btns || '<span>Waiting for the cluster…</span>'}</div>
      <h5>OR FROM THE DOCKER HOST</h5>${docker}`;
  },

};

// App is the small browser under the queue: the selected ticket's app as DBCanvas last
// fetched it from the cluster. When the fix lands it shakes, stamps FIXED, and stays up
// with the working page until the game drops the ticket a few seconds later.
const App = {
  ticket: '', rev: 0, fetching: false, wasOK: {}, quaked: {},
  render() {
    const pane = $('appPane');
    const t = S.tickets.find((x) => x.id === sel && (x.state === 'open' || x.state === 'cleared'));
    const m = t?.monitor || null;
    pane.classList.toggle('hidden', !m);
    $('tickets').classList.toggle('with-app', !!m);
    if (!m) return;
    const fixed = t.state === 'cleared';
    // Tell the player when the app comes back, or goes down.
    const was = this.wasOK[t.id];
    if (!fixed && was === false && m.ok) { toast(`✓ ${m.title} is back up`); Sound.blip(); }
    this.wasOK[t.id] = m.ok;
    pane.classList.toggle('fixed', fixed);
    if (fixed && !this.quaked[t.id]) {
      this.quaked[t.id] = true;
      pane.classList.remove('quake'); void pane.offsetWidth; pane.classList.add('quake');
    }
    $('appUrl').textContent = m.url.replace(/^https?:\/\//, '');
    const st = $('appStatus');
    st.textContent = m.error ? 'DOWN' : `${m.status}`;
    st.className = 'b-status ' + (m.ok ? 'ok' : 'bad');
    const rl = $('appReload');
    rl.disabled = fixed || !!t.checking;
    rl.classList.toggle('spin', !!t.checking);
    $('appFoot').textContent = fixed ? `${m.title} answers again · the fix is confirmed`
      : `Fetched from the cluster with every check · ${m.age < 2 ? 'just now' : m.age + 's ago'}${m.ok ? '' : ' · it comes back when the ticket is fixed'}`;
    if (this.ticket === t.id && this.rev === m.rev) return;
    if (this.fetching) return;
    this.fetching = true;
    const want = t.id;
    api('GET', 'api/monitor?ticket=' + encodeURIComponent(want)).then((d) => {
      if (sel !== want) return;
      this.ticket = want; this.rev = d.rev;
      $('appView').srcdoc = pageDoc(d.page);
    }).catch(() => {}).finally(() => { this.fetching = false; });
  },
  // nudge points the player at the browser (the app chip in the ticket).
  nudge() {
    const pane = $('appPane');
    pane.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    pane.classList.remove('nudge'); void pane.offsetWidth; pane.classList.add('nudge');
  },
};

// pageDoc is what the browser pane shows: the page itself, or an error page like a
// browser's. Either way it runs no script and loads nothing (sandbox, and the CSP).
const CSP = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:">`;
function pageDoc(p) {
  const host = esc(String(p.url || '').replace(/^https?:\/\//, '').split('/')[0]);
  const errPage = (title, line, detail, code) => `<!doctype html>${CSP}<style>
    body{margin:0;font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;background:#f6f6f8;color:#3c3c43;display:flex;align-items:center;justify-content:center;min-height:100vh}
    .w{max-width:520px;padding:28px}.i{font-size:42px;color:#9a98a6;margin-bottom:8px}h1{font-size:22px;font-weight:600;color:#202124;margin:0 0 10px}
    pre{white-space:pre-wrap;word-break:break-word;background:#ececf1;border-radius:6px;padding:10px 12px;font:12px ui-monospace,Menlo,monospace;color:#55536a;max-height:40vh;overflow:auto}
    .c{font:12px ui-monospace,Menlo,monospace;color:#8a8899;letter-spacing:.04em;margin-top:14px}</style>
    <div class="w"><div class="i">☹</div><h1>${title}</h1><p>${line}</p>${detail ? `<pre>${esc(detail)}</pre>` : ''}${code ? `<div class="c">${esc(code)}</div>` : ''}</div>`;
  if (p.error) {
    const m = /^(.*?)\s*\((ERR_[A-Z_]+)\)$/.exec(p.error);
    return errPage(esc(m ? m[1] : p.error), `<b>${host}</b> did not answer.`, p.detail, m ? m[2] : '');
  }
  const body = String(p.body || '');
  const type = String(p.type || '').toLowerCase();
  if (!body.trim()) return errPage(`HTTP ${p.status}`, `<b>${host}</b> answered with an empty page.`, '', '');
  if (type.includes('html') || (!type && /^\s*</.test(body))) {
    const dt = /^\s*<!doctype[^>]*>/i.exec(body);
    return dt ? dt[0] + CSP + body.slice(dt[0].length) : CSP + body;
  }
  return `<!doctype html>${CSP}<style>body{margin:0;background:#fff}pre{margin:0;padding:12px;font:12px/1.5 ui-monospace,Menlo,monospace;white-space:pre-wrap;word-break:break-word}</style><pre>${esc(body)}</pre>`;
}

$('benchTabs').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-close],[data-tab],[data-add],[data-pop]');
  if (!b) return;
  if (b.dataset.close) { ev.stopPropagation(); Bench.close(b.dataset.close); return; }
  if (b.dataset.tab) { Bench.show(b.dataset.tab); return; }
  if (b.dataset.add) { Bench.open(b.dataset.add, true); return; }
  if (b.dataset.pop) { const s = Bench.sessions.find((x) => x.id === b.dataset.pop); if (s) popOut(s.nodeId); }
});
$('benchTabs').addEventListener('auxclick', (ev) => {
  const b = ev.target.closest('[data-tab]');
  if (ev.button === 1 && b) Bench.close(b.dataset.tab);
});
$('termLaunch').addEventListener('click', (ev) => {
  const b = ev.target.closest('button');
  if (!b) return;
  if (b.dataset.add) Bench.open(b.dataset.add);
  else if (b.dataset.copy) copy(b.dataset.copy);
});
$('appReload').onclick = () => { if (sel) play({ do: 'check-now', ticket: sel }).catch(() => {}); };
// DBCanvas offers a shared session's terminals of this window, each with its own port.
addEventListener('message', (ev) => {
  const m = ev.data;
  if (ev.source !== window.parent || ev.origin !== location.origin || !m || m.type !== 'dbcanvas:term-shared') return;
  const port = ev.ports?.[0];
  if (port) Bench.adopt(m, port);
});
// sayHello tells DBCanvas this page is up, once it knows its stack: DBCanvas then offers
// the shared terminals it should show.
let helloSent = false;
function sayHello() {
  if (helloSent || !inDBCanvas || !S?.stack?.id) return;
  helloSent = true;
  window.parent.postMessage({ type: 'dbcanvas:term-hello', stackId: S.stack.id }, location.origin);
}
// The page pings DBCanvas's relay while it lives; a page that goes away closes its shells.
setInterval(() => { for (const s of Bench.sessions) if (s.status !== 'closed') s.port.postMessage({ type: 'ping' }); }, 10000);
// unload: the page is going, not the player closing the tab — a shared terminal stays
// open for everyone else, and comes back when the page loads again.
addEventListener('pagehide', () => { for (const s of Bench.sessions) if (s.status !== 'closed') s.port.postMessage({ type: 'close', unload: true }); });
new ResizeObserver(() => { Bench.render(true); }).observe($('bench'));

// The grip between the ticket and the workbench; the workbench's height is remembered.
(function grip() {
  // The workbench's share of the desk, so it grows and shrinks with the window.
  const g = $('grip');
  const desk = $('desk');
  const KEY = 'pp-bench-share';
  const setShare = (px) => {
    const total = desk.clientHeight;
    if (!total) return null;
    const h = Math.max(150, Math.min(total - 110, px));
    const share = Math.round((h / total) * 1000) / 10;
    $('bench').style.setProperty('--bench', `${share}%`);
    return share;
  };
  try {
    localStorage.removeItem('pp-bench'); // the old setting, in pixels
    const v = Number(localStorage.getItem(KEY));
    if (v > 0 && v < 100) $('bench').style.setProperty('--bench', `${v}%`);
  } catch { /* */ }
  g.addEventListener('pointerdown', (ev) => {
    ev.preventDefault();
    g.setPointerCapture(ev.pointerId);
    g.classList.add('drag');
    const bottom = desk.getBoundingClientRect().bottom;
    let share = null;
    const move = (e) => { share = setShare(bottom - e.clientY - 6); };
    const up = () => {
      g.classList.remove('drag');
      g.removeEventListener('pointermove', move);
      g.removeEventListener('pointerup', up);
      try { if (share) localStorage.setItem(KEY, String(share)); } catch { /* */ }
    };
    g.addEventListener('pointermove', move);
    g.addEventListener('pointerup', up);
  });
  g.addEventListener('dblclick', () => { $('bench').style.removeProperty('--bench'); try { localStorage.removeItem(KEY); } catch { /* */ } });
})();

// ---------------------------------------------------------------- the context menu
function copy(text, msg) {
  const manual = () => ask('COPY', 'Select the text and copy it.', 'DONE', '', text);
  // The clipboard API exists only on https or localhost; anywhere else, show the text to copy.
  if (!navigator.clipboard?.writeText) { manual(); return; }
  navigator.clipboard.writeText(text).then(() => toast(msg || 'Copied'), manual);
}
function menu(x, y, ticketId) {
  const m = $('ctx');
  const t = S.tickets.find((z) => z.id === ticketId);
  const star = (n) => (t && n.host === t.worker ? ' ★' : '');
  const here = (S.nodes || []).map((n) => `<button data-node="${esc(n.nodeId)}">▸ ${esc(nodeLabel(n))}${star(n)}</button>`).join('');
  const out = (S.nodes || []).map((n) => `<button data-pop="${esc(n.nodeId)}">⧉ ${esc(n.host)}${star(n)}</button>`).join('');
  const docker = (S.nodes || []).map((n) => `<button data-copy="${esc(dockerCmd(n))}">⧉ docker exec: ${esc(n.host)}${star(n)}</button>`).join('');
  m.innerHTML = `${inDBCanvas ? `<small>Terminal in the game</small>${here}<small>Terminal in a DBCanvas window</small>${out}<button data-stack="1">▸ Open the stack in DBCanvas</button><hr>` : ''}
    <small>Copy, for a shell on the Docker host</small>${docker}
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
  if (b.dataset.node) Bench.open(b.dataset.node);
  else if (b.dataset.pop) popOut(b.dataset.pop);
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
  if (ev.target.tagName === 'TEXTAREA' || ev.target.closest?.('.xterm')) return; // typing in a terminal
  const k = ev.key.toLowerCase();
  if (k === 'm') Sound.toggle();
  if (k === 'escape') $('ctx').classList.add('hidden');
  if (!S) return;
  if (S.phase === 'title') { titleKeys(ev, k); return; }
  if ((S.phase === 'checking' || S.phase === 'ready') && k === 'escape') { $('btnBack').click(); return; }
  if (S.phase === 'ready' && k === 'enter') { ev.preventDefault(); $('btnStart').click(); return; }
  if (k === 'p') play({ do: S.phase === 'paused' ? 'resume' : 'pause' }).catch(() => {});
  if (S.phase === 'playing' && ['1', '2', '3'].includes(k)) {
    const t = S.tickets.filter((x) => x.state !== 'cleared')[Number(k) - 1];
    if (t) { sel = t.id; Sound.select(); renderPlay(); }
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
  const seed = S.debrief?.seed || 0;
  wantMode = S.debrief?.mode || 'arcade';
  await play({ do: 'title' }).catch(() => {});
  wantSeed = seed;
  await startShift();
};
// A new shift goes back to the title screen with the last mode still picked.
$('btnNew').onclick = $('btnAgain').onclick = async () => { hideContinue(); wantSeed = 0; await play({ do: 'title' }).catch(() => {}); await refresh(); };

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
