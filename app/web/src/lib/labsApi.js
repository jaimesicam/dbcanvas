// Labs (experimental) API wrapper. Same conventions as lib/stackApi.js.

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

export const labsApi = {
  list: () => request('GET', '/api/labs'),
  myRuns: () => request('GET', '/api/labs/runs'),
  start: (labId) => request('POST', `/api/labs/${labId}/start`),
  finish: (labId) => request('POST', `/api/labs/${labId}/finish`),
  checkStep: (labId, stepId) => request('POST', `/api/labs/${labId}/steps/${stepId}/check`),
}
