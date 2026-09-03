// apiApi.js — the API page's own client: the endpoint catalogue, and API tokens.

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  const text = await res.text()
  let data = null
  if (text) { try { data = JSON.parse(text) } catch { data = null } }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

export const apiApi = {
  endpoints: () => request('GET', '/api/meta/endpoints'),
  listTokens: () => request('GET', '/api/tokens'),
  createToken: (t) => request('POST', '/api/tokens', t),
  revokeToken: (id) => request('DELETE', `/api/tokens/${id}`),
  adminTokens: () => request('GET', '/api/admin/tokens'),
  adminRevokeToken: (id) => request('DELETE', `/api/admin/tokens/${id}`),
}

// METHOD_TONE colours a method badge. Semantic, not decorative: DELETE reads as the
// destructive one at a glance, which is the distinction that matters when scanning
// 200 rows.
export const METHOD_TONE = {
  GET: 'text-primary',
  POST: 'text-success',
  PUT: 'text-warning',
  DELETE: 'text-danger',
}

export const SCOPE_TEXT = {
  read: 'Any token can call this — it changes nothing.',
  write: 'Needs a write-scope token.',
  admin: 'Needs an admin-scope token, which only an administrator can create.',
}

// MEDIA_TEXT explains why an endpoint is not plain JSON. Empty for the majority
// that are.
export const MEDIA_TEXT = {
  multipart: 'Takes a multipart/form-data upload rather than a JSON body.',
  download: 'Responds with a file as an attachment, not JSON.',
  sse: "Responds with text/event-stream and stays open, pushing each event as it happens.",
  websocket: 'Upgrades the connection to a WebSocket.',
}

export const MEDIA_LABEL = {
  multipart: 'upload',
  download: 'file',
  sse: 'stream',
  websocket: 'websocket',
}

// samplePath fills a path's wildcards with something plausible, so a copied curl
// line is one edit away from working instead of containing literal braces.
const SAMPLE = { id: '1', nid: 'node-1', fid: 'frame-1', aid: '1', job: 'job-1', inst: 'mysql-8.0', no: '1', src: '0', user: 'appuser', username: 'developer', stepId: 'step-1' }

export function samplePath(path) {
  return path.replace(/\{([a-zA-Z0-9_]+)\}/g, (_, name) => SAMPLE[name] ?? `<${name}>`)
}

// curlFor builds the copyable line. It varies by media kind because a single
// generic example would be wrong for four of the five kinds — an upload needs -F, a
// download needs -OJ, and a websocket cannot be curled at all.
export function curlFor(ep, origin = '') {
  const url = `${origin}${samplePath(ep.path)}`
  const auth = `-H "Authorization: Bearer $DBCANVAS_TOKEN"`
  if (ep.media === 'websocket') {
    return `# ${ep.method} ${ep.path} upgrades to a WebSocket — use dbcanvas-cli or a WebSocket client,\n`
      + `# sending the same Authorization header on the handshake.`
  }
  if (ep.media === 'sse') return `curl -N ${auth} "${url}"`
  if (ep.media === 'download') return `curl -OJ ${auth} "${url}"`
  if (ep.media === 'multipart') return `curl ${auth} -F file=@./yourfile "${url}"`
  if (ep.method === 'GET') return `curl -s ${auth} "${url}"`
  return `curl -s -X ${ep.method} ${auth} \\\n  -H 'Content-Type: application/json' \\\n  -d '{}' "${url}"`
}

// cliFor is the equivalent dbcanvas-cli invocation. The raw `api` form is always
// correct, which is the point of that command existing.
export function cliFor(ep) {
  return `dbcanvas api ${ep.method} ${samplePath(ep.path)}`
}

// matches filters an endpoint against the search box: the path, the summary and the
// method, so both "deploy" and "POST /api/stacks" find things.
export function matches(ep, q) {
  if (!q) return true
  const hay = `${ep.method} ${ep.path} ${ep.summary} ${ep.group}`.toLowerCase()
  return q.toLowerCase().split(/\s+/).every((term) => hay.includes(term))
}

// relDate renders an RFC3339 stamp as a short date, or "—".
export function relDate(s) {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d)) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

// expiryText says how long a token has left in the terms somebody cares about: the
// number of days, until it is close enough that hours matter.
export function expiryText(tok) {
  if (tok.state === 'revoked') return 'Revoked'
  if (!tok.expiresAt) return 'Never expires'
  const ms = new Date(tok.expiresAt) - new Date()
  if (isNaN(ms)) return '—'
  if (ms <= 0) return 'Expired'
  const days = Math.floor(ms / 86400000)
  if (days >= 2) return `${days} days left`
  const hours = Math.floor(ms / 3600000)
  if (hours >= 1) return `${hours} hour${hours === 1 ? '' : 's'} left`
  return 'Under an hour left'
}

export const TOKEN_STATE_TONE = { active: 'primary', expired: 'warning', revoked: 'muted' }

// EXPIRY_CHOICES are the lifetimes offered. 0 means never, which the server only
// allows an administrator.
export const EXPIRY_CHOICES = [
  { days: 7, label: '7 days' },
  { days: 30, label: '30 days' },
  { days: 60, label: '60 days' },
  { days: 90, label: '90 days' },
  { days: 0, label: 'Never' },
]
