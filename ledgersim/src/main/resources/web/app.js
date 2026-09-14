/* Ledger Sim dashboard.
 *
 * One poll of /api/state drives everything. Form fields are only written from
 * the server when the user is not editing them (see `touched`) — otherwise a
 * 2-second refresh would rewrite a half-typed hostname out from under them,
 * which is the classic way a live dashboard becomes unusable as a form.
 */
'use strict'

const POLL_MS = 2000

const $ = (id) => document.getElementById(id)
const touched = new Set()
let catalog = []
let lastCommits = null
let lastCommitsAt = null

/* Any field the user has focused or changed is theirs until they apply. */
function guard (el) {
  if (!el) return
  el.addEventListener('focus', () => touched.add(el.id))
  el.addEventListener('input', () => touched.add(el.id))
}

function setIf (id, value) {
  const el = $(id)
  if (!el || touched.has(id)) return
  if (el.type === 'checkbox') el.checked = !!value
  else el.value = value === null || value === undefined ? '' : value
}

function text (id, value) {
  const el = $(id)
  if (el) el.textContent = value
}

async function api (path, body) {
  const res = await fetch(path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body)
  })
  let payload = null
  try { payload = await res.json() } catch (e) { payload = null }
  return { ok: res.ok, status: res.status, body: payload }
}

/* ------------------------------------------------------------------ render */

function renderCatalog (conn) {
  catalog = conn.catalog || []
  const engineSel = $('f-engine')
  if (engineSel && engineSel.options.length !== catalog.length) {
    engineSel.innerHTML = ''
    catalog.forEach((e) => {
      const o = document.createElement('option')
      o.value = e.id
      o.textContent = e.label
      engineSel.appendChild(o)
    })
  }
  renderDrivers(conn.engine, conn.driver)
}

/* The driver list is a function of the engine: pgJDBC cannot speak to MySQL and
   the server rejects the combination, so the UI never offers it. */
function renderDrivers (engineId, selected) {
  const sel = $('f-driver')
  if (!sel) return
  const engine = catalog.find((e) => e.id === engineId)
  const drivers = engine ? engine.drivers : []
  const want = drivers.map((d) => d.id).join(',')
  if (sel.dataset.for !== want) {
    sel.innerHTML = ''
    drivers.forEach((d) => {
      const o = document.createElement('option')
      o.value = d.id
      o.textContent = d.available ? `${d.label} — ${d.license}` : `${d.label} (not on classpath)`
      o.disabled = !d.available
      sel.appendChild(o)
    })
    sel.dataset.for = want
  }
  if (!touched.has('f-driver') && selected) sel.value = selected
}

function renderIsolationOptions (levels) {
  ;['w-isolation', 'p-transactionIsolation'].forEach((id) => {
    const sel = $(id)
    if (!sel || sel.options.length === levels.length) return
    sel.innerHTML = ''
    levels.forEach((l) => {
      const o = document.createElement('option')
      o.value = l
      o.textContent = l === '' ? '(driver default)' : l.replace('TRANSACTION_', '')
      sel.appendChild(o)
    })
  })
}

function renderProps (conn) {
  const tbody = $('props-table').querySelector('tbody')
  tbody.innerHTML = ''
  const effective = conn.effectiveProps || {}
  const auto = conn.autoProps || {}
  const user = conn.props || {}
  Object.keys(effective).forEach((k) => {
    const tr = document.createElement('tr')
    const fromUser = Object.prototype.hasOwnProperty.call(user, k)
    tr.innerHTML =
      `<td>${esc(k)}</td><td>${esc(effective[k])}</td>` +
      `<td class="${fromUser ? 'src-user' : 'src-auto'}">${fromUser ? 'set here' : 'auto'}</td>` +
      `<td></td>`
    if (fromUser) {
      const btn = document.createElement('button')
      btn.textContent = 'revert'
      btn.addEventListener('click', () => {
        delete user[k]
        applyConnection({ props: user })
      })
      tr.lastElementChild.appendChild(btn)
    } else if (Object.prototype.hasOwnProperty.call(auto, k)) {
      const btn = document.createElement('button')
      btn.textContent = 'remove'
      btn.title = 'Send an empty value, which drops the property from the URL'
      btn.addEventListener('click', () => {
        user[k] = ''
        applyConnection({ props: user })
      })
      tr.lastElementChild.appendChild(btn)
    }
    tbody.appendChild(tr)
  })
}

function renderNotes (notes) {
  const ul = $('conn-notes')
  ul.innerHTML = ''
  ;(notes || []).forEach((n) => {
    const li = document.createElement('li')
    li.textContent = n
    ul.appendChild(li)
  })
}

function renderConnection (conn) {
  renderCatalog(conn)
  text('hdr-engine', conn.engine)
  text('hdr-driver', conn.driverLabel)
  text('url-effective', conn.url)
  text('url-detected', conn.detectedUrl)
  $('url-detected-wrap').classList.toggle('hidden', conn.detectedUrl === conn.url)
  $('conn-modified').classList.toggle('hidden', !conn.modified)

  setIf('f-engine', conn.engine)
  setIf('f-host', (conn.hosts || []).join(','))
  setIf('f-port', conn.port)
  setIf('f-database', conn.database)
  setIf('f-user', conn.user)
  setIf('f-tls', conn.tls)
  setIf('f-url-override', conn.urlOverride)

  const p = conn.pool || {}
  Object.keys(p).forEach((k) => setIf('p-' + k, p[k]))

  const direct = p.mode === 'direct'
  $('pool-grid').style.opacity = direct ? '0.45' : '1'
  $('pool-settings-heading').textContent = direct
    ? 'HikariCP pool — not in use in direct mode'
    : 'HikariCP pool'
  text('mode-hint', direct
    ? 'Direct opens a connection per transaction and closes it after. Nothing is reused, ' +
      'so the pool settings below are inert — only isolation, init SQL, autoCommit and readOnly still apply.'
    : 'Pooled keeps connections open and hands them out. Switch to direct to measure what that is worth ' +
      'against this server — the workload is identical either way.')

  renderProps(conn)
  renderNotes(conn.notes)
}

/* Two shapes, because the two modes have genuinely different things to show.
   Pooled reports what the pool is holding; direct has no pool to report, so it
   shows what the mode costs instead — and the share of each transaction spent
   connecting is the number that makes the comparison land. */
function renderPool (m, metrics) {
  const direct = m.mode === 'direct'
  $('pool-stats').classList.toggle('hidden', direct)
  $('direct-stats').classList.toggle('hidden', !direct)
  text('conn-mode-tag', direct ? 'direct — no pool' : 'pooled')

  if (direct) {
    text('m-opened', num(m.connectionsOpened))
    text('m-connect', (m.avgConnectMs ?? 0) + ' ms')
    text('m-connect-fail', num(m.connectFailures))
    const p50 = metrics && metrics.p50Ms
    text('m-connect-share', p50 ? Math.round((m.avgConnectMs / p50) * 100) + '%' : '—')
    text('pool-hint', 'Every transaction opens a connection and closes it — TCP, TLS, ' +
      'authentication and session setup, paid each time. That is the cost a pool removes.')
  } else {
    text('m-active', num(m.active))
    text('m-idle', num(m.idle))
    text('m-total', num(m.total))
    text('m-awaiting', num(m.awaitingConnection))
    text('pool-hint', 'Threads waiting on a connection is the number to watch: a non-zero ' +
      'awaiting with an idle database means the pool, not the server, is the queue.')
  }

  text('hdr-mode', direct ? 'direct' : 'pooled')
  text('hdr-gen', m.generation === undefined ? '—' : m.generation)
  const dot = $('conn-dot')
  dot.classList.toggle('ok', !!m.up)
  dot.classList.toggle('bad', !m.up)
  text('conn-text', m.up ? 'connected' : 'down')
}

function renderMetrics (m) {
  /* commits/s is derived from the delta between polls rather than reported by the
     server: the server counts totals, and a rate needs two observations. */
  const now = Date.now()
  if (lastCommits !== null && now > lastCommitsAt) {
    const rate = (m.commits - lastCommits) / ((now - lastCommitsAt) / 1000)
    text('m-tps', rate >= 0 ? rate.toFixed(1) : '—')
  }
  lastCommits = m.commits
  lastCommitsAt = now

  /* The window is shown because a reconfiguration resets it: a p95 that spans
     two drivers describes neither, so the panel says how much time it covers. */
  const win = m.windowMs === undefined ? null : Math.round(m.windowMs / 1000)
  text('m-window', win === null ? '—' : win + ' s')
  text('m-p50', m.p50Ms + ' ms')
  text('m-p95', m.p95Ms + ' ms')
  text('m-p99', m.p99Ms + ' ms')
  text('m-deadlocks', num(m.deadlocks))
  text('m-locktimeouts', num(m.lockTimeouts))
  text('m-retries', num(m.retries))
  text('m-errors', num(m.errors))

  const tbody = $('errors-table').querySelector('tbody')
  tbody.innerHTML = ''
  ;(m.recentErrors || []).forEach((e) => {
    const tr = document.createElement('tr')
    tr.innerHTML = `<td>${new Date(e.at).toLocaleTimeString()}</td><td>${esc(e.op)}</td>` +
      `<td>${esc(e.sqlState || '—')}</td><td>${esc(e.vendorCode)}</td><td>${esc(e.message || '')}</td>`
    tbody.appendChild(tr)
  })
}

function renderDataset (d) {
  text('d-accounts', num(d.accounts))
  text('d-orders', num(d.orders))
  text('d-lines', num(d.orderLines))
  text('d-entries', num(d.ledgerEntries))
  const el = $('d-balance')
  if (d.balanceCheckMinor === undefined) {
    text('d-balance', '—')
    return
  }
  /* Glyph as well as colour: the balance check is the one number on this page
     where being wrong matters, and it must read as wrong without colour. */
  el.textContent = (d.balanced ? '✓ ' : '✗ ') + d.balanceCheckMinor
  el.classList.toggle('ok', !!d.balanced)
  el.classList.toggle('bad', !d.balanced)
}

function renderWorkload (w) {
  Object.keys(w).forEach((k) => setIf('w-' + k, w[k]))
  const btn = $('btn-run')
  btn.textContent = w.running ? 'Stop' : 'Start'
  btn.dataset.run = w.running ? 'stop' : 'start'
}

/* ------------------------------------------------------------------ actions */

function connectionPayload (overrides) {
  const payload = {
    engine: $('f-engine').value,
    driver: $('f-driver').value,
    host: $('f-host').value,
    port: Number($('f-port').value || 0),
    database: $('f-database').value,
    user: $('f-user').value,
    tls: $('f-tls').value,
    urlOverride: $('f-url-override').value,
    pool: {
      mode: $('p-mode').value,
      maximumPoolSize: Number($('p-maximumPoolSize').value || 10),
      minimumIdle: Number($('p-minimumIdle').value ?? -1),
      connectionTimeoutMs: Number($('p-connectionTimeoutMs').value || 30000),
      idleTimeoutMs: Number($('p-idleTimeoutMs').value || 0),
      maxLifetimeMs: Number($('p-maxLifetimeMs').value || 0),
      keepaliveTimeMs: Number($('p-keepaliveTimeMs').value || 0),
      validationTimeoutMs: Number($('p-validationTimeoutMs').value || 5000),
      leakDetectionThresholdMs: Number($('p-leakDetectionThresholdMs').value || 0),
      transactionIsolation: $('p-transactionIsolation').value,
      connectionInitSql: $('p-connectionInitSql').value,
      autoCommit: $('p-autoCommit').checked,
      readOnly: $('p-readOnly').checked
    }
  }
  /* An untouched password box means "leave it alone" — the field is never
     populated from the server, so sending its empty value would clear it. */
  const pw = $('f-password').value
  if (pw !== '') payload.password = pw
  return Object.assign(payload, overrides || {})
}

function showResult (res) {
  const el = $('test-result')
  el.classList.remove('hidden', 'ok', 'bad')
  const body = res.body || {}
  el.classList.add(body.ok ? 'ok' : 'bad')
  el.textContent = JSON.stringify(body, null, 2)
}

async function applyConnection (overrides) {
  const res = await api('/api/connection/apply', connectionPayload(overrides))
  showResult(res)
  if (res.ok) {
    touched.clear()
    $('f-password').value = ''
  }
  refresh()
}

function num (v) {
  return v === undefined || v === null ? '—' : Number(v).toLocaleString()
}

function esc (s) {
  return String(s === undefined || s === null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

/* ------------------------------------------------------------------ poll */

async function refresh () {
  const res = await api('/api/state')
  if (!res.ok || !res.body) {
    $('banner-error').textContent = 'dashboard cannot reach the simulator'
    $('banner-error').classList.remove('hidden')
    return
  }
  const s = res.body
  $('banner-error').classList.toggle('hidden', !s.lastError)
  if (s.lastError) $('banner-error').textContent = s.lastError

  renderIsolationOptions(s.isolationLevels || [''])
  renderConnection(s.connection)
  renderPool(s.poolMetrics || {}, s.metrics || {})
  renderWorkload(s.workload || {})
  renderMetrics(s.metrics || {})
  renderDataset(s.dataset || {})
  text('hdr-target', s.label || '—')
}

function wire () {
  document.querySelectorAll('input, select').forEach(guard)

  $('f-engine').addEventListener('change', () => {
    /* Switching engine invalidates the driver choice, so re-offer it immediately
       rather than waiting for the next poll to correct it. */
    touched.delete('f-driver')
    renderDrivers($('f-engine').value, null)
    const engine = catalog.find((e) => e.id === $('f-engine').value)
    if (engine && !touched.has('f-port')) $('f-port').value = engine.defaultPort
  })

  $('btn-test').addEventListener('click', async () => {
    showResult(await api('/api/connection/test', connectionPayload()))
  })
  $('btn-apply').addEventListener('click', () => applyConnection())
  $('btn-reset').addEventListener('click', async () => {
    showResult(await api('/api/connection/reset', {}))
    touched.clear()
    refresh()
  })
  $('btn-prop-add').addEventListener('click', () => {
    const k = $('prop-key').value.trim()
    if (!k) return
    const props = {}
    document.querySelectorAll('#props-table tbody tr').forEach((tr) => {
      if (tr.children[2].textContent === 'set here') {
        props[tr.children[0].textContent] = tr.children[1].textContent
      }
    })
    props[k] = $('prop-val').value
    $('prop-key').value = ''
    $('prop-val').value = ''
    applyConnection({ props })
  })

  $('btn-run').addEventListener('click', async () => {
    await api('/api/workload', { run: $('btn-run').dataset.run })
    refresh()
  })
  $('btn-workload-apply').addEventListener('click', async () => {
    const body = { run: '' }
    ;['threads', 'ratePerSec', 'isolation', 'linesPerOrder', 'customers', 'revenueShards',
      'hotAccountShare', 'hotAccountCount', 'deadlockShare', 'lockWaitSeconds', 'maxRetries',
      'weightPlace', 'weightSettle', 'weightRefund', 'weightRead'].forEach((k) => {
      const el = $('w-' + k)
      if (!el) return
      body[k] = el.tagName === 'SELECT' ? el.value : Number(el.value)
      if (k === 'isolation') body[k] = el.value
    })
    const res = await api('/api/workload', body)
    if (!res.ok) showResult(res)
    touched.clear()
    refresh()
  })
  $('btn-seed').addEventListener('click', async () => {
    showResult(await api('/api/dataset/seed', {}))
    refresh()
  })
}

wire()
refresh()
setInterval(refresh, POLL_MS)
