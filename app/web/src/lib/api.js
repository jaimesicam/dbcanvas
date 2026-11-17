// Same-origin fetch wrapper. Cookies ride along automatically. Throws an Error
// with a `.status` property on non-2xx responses, using the server's `error`
// field as the message when present.

async function request(method, path, body) {
  const opts = {
    method,
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
  }
  if (body !== undefined) opts.body = JSON.stringify(body)

  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }

  if (!res.ok) {
    const msg = (data && data.error) || `Request failed (${res.status})`
    const err = new Error(msg)
    err.status = res.status
    throw err
  }
  return data
}

export const api = {
  status: () => request('GET', '/api/setup/status'),
  // profile: { firstName, lastName, avatar } (app/profile.go)
  updateProfile: (profile) => request('PUT', '/api/me/profile', profile),
  logout: () => request('POST', '/api/auth/logout'),
  me: () => request('GET', '/api/me'),
  settings: () => request('GET', '/api/me/settings'),
  saveSettings: (s) => request('PUT', '/api/me/settings', s),
  // Instance-wide settings (app/syssettings.go).
  systemSettings: () => request('GET', '/api/system/settings'),
  saveSystemSettings: (s) => request('PUT', '/api/system/settings', s),
  // Release notes (app/whatsnew.go). The server decides whether there is anything
  // unread, so the client never has to compare version strings — getting that
  // subtly wrong would re-open the dialog on every page load.
  whatsNew: () => request('GET', '/api/whatsnew'),
  markWhatsNewSeen: () => request('POST', '/api/whatsnew/seen'),
  // Asks the server to check GitHub for a newer build (app/updatecheck.go). Only
  // ever called from the dashboard's button.
  checkUpdates: () => request('GET', '/api/dashboard/updates'),
}
