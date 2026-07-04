// Query Runner API wrapper. Same conventions as lib/datagenApi.js: same-origin JSON,
// cookies ride along, throws Error with .status on non-2xx.

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) {
    try { data = JSON.parse(text) } catch { data = null }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

export const queryrunApi = {
  targets: () => request('GET', '/api/queryrun/targets'),
  start: (queries) => request('POST', '/api/queryrun/runs', { queries }),
  status: (id) => request('GET', `/api/queryrun/runs/${id}`),
  stop: (id) => request('POST', `/api/queryrun/runs/${id}/stop`),
  history: () => request('GET', '/api/queryrun/history'),
}

export const targetKey = (t) => `${t.stackId}:${t.nodeId}`
