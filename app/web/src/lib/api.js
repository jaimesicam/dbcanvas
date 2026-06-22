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
  listUsers: () => request('GET', '/api/users'),
  setUserStatus: (id, action) => request('POST', `/api/users/${id}/${action}`),
  deleteUser: (id) => request('DELETE', `/api/users/${id}`),
}
