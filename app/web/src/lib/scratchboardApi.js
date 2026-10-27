// scratchboardApi.js — the Scratchboard boards an account keeps (app/scratchboard.go).
//
// The only way anything on the Scratchboard reaches the server, and only for a board marked to be
// kept while the account's "Keep Scratchboard boards" preference is on. Kept apart from
// lib/scratch/*, which must never send card content anywhere.

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
    err.body = data
    throw err
  }
  return data
}

export const scratchboardApi = {
  list: () => request('GET', '/api/me/scratchboards'),
  // save sends the rev this page last saw; a 409 (err.status) carries the stored board in err.body.
  save: (id, board, rev = 0) => request('PUT', `/api/me/scratchboards/${encodeURIComponent(id)}?rev=${rev}`, board),
  remove: (id) => request('DELETE', `/api/me/scratchboards/${encodeURIComponent(id)}`),
}
