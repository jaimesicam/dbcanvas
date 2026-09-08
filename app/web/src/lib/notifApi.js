// Notifications API. Same-origin JSON; cookies ride along. Live updates arrive over the
// SSE stream at /api/notifications/stream (see NotificationBell in App.jsx).

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  const text = await res.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

export const notifApi = {
  list: () => request('GET', '/api/notifications'),
  markRead: (id) => request('POST', `/api/notifications/${id}/read`),
  markAll: () => request('POST', '/api/notifications/read-all'),
}

// severityTone maps a notification severity to a UI tone / dot color.
export const SEVERITY_TONE = {
  info: 'muted',
  success: 'primary',
  warning: 'warning',
  error: 'danger',
}

// relTime renders a compact relative time from an RFC3339 timestamp.
export function relTime(ts) {
  const t = new Date(ts).getTime()
  if (!t) return ''
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}
