// Dashboard API. Same-origin JSON; cookies ride along.
// summary() is cheap (store counters); stats() is the focus-gated live OS sample.

async function request(method, path) {
  const res = await fetch(path, { method, credentials: 'same-origin' })
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

export const dashApi = {
  summary: () => request('GET', '/api/dashboard/summary'),
  stats: () => request('GET', '/api/dashboard/stats'),
}

export function fmtBytes(n) {
  n = Number(n) || 0
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i > 0 && n < 10 ? 1 : 0)} ${u[i]}`
}
