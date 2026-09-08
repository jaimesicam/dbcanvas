// Benchmark API wrapper. Same conventions as lib/queryrunApi.js.

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

export const benchmarkApi = {
  targets: () => request('GET', '/api/benchmark/targets'),
  start: (cfg) => request('POST', '/api/benchmark/runs', cfg),
  status: (id) => request('GET', `/api/benchmark/runs/${id}`),
  stop: (id) => request('POST', `/api/benchmark/runs/${id}/stop`),
  history: () => request('GET', '/api/benchmark/history'),
}

export const benchTargetKey = (t) => `${t.stackId}:${t.nodeId}`
