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
  setup: (username, password) => request('POST', '/api/setup', { username, password }),
  register: (username, password) => request('POST', '/api/auth/register', { username, password }),
  login: (username, password) => request('POST', '/api/auth/login', { username, password }),
  logout: () => request('POST', '/api/auth/logout'),
  me: () => request('GET', '/api/me'),
  settings: () => request('GET', '/api/me/settings'),
  saveSettings: (s) => request('PUT', '/api/me/settings', s),
  // Instance-wide settings (app/syssettings.go). Readable by anyone signed in;
  // saving is admin-only and 403s otherwise.
  systemSettings: () => request('GET', '/api/system/settings'),
  saveSystemSettings: (s) => request('PUT', '/api/system/settings', s),
  listUsers: () => request('GET', '/api/users'),
  setUserStatus: (id, action) => request('POST', `/api/users/${id}/${action}`),
  deleteUser: (id) => request('DELETE', `/api/users/${id}`),
  // Release notes (app/whatsnew.go). The server decides whether there is anything
  // unread, so the client never has to compare version strings — getting that
  // subtly wrong would re-open the dialog on every page load.
  // Changing your own password (app/password.go). Deliberately cookie-only on the
  // server, so this cannot be done with an API token.
  changePassword: (currentPassword, newPassword, revokeTokens) =>
    request('POST', '/api/me/password', { currentPassword, newPassword, revokeTokens }),
  whatsNew: () => request('GET', '/api/whatsnew'),
  markWhatsNewSeen: () => request('POST', '/api/whatsnew/seen'),
}
