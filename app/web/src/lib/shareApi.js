// shareApi.js — shared sessions (app/share.go).

async function request(method, path, body) {
  const opts = { method, headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin' }
  if (body !== undefined) opts.body = JSON.stringify(body)
  const res = await fetch(path, opts)
  let data = null
  const text = await res.text()
  if (text) { try { data = JSON.parse(text) } catch { data = null } }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

const enc = encodeURIComponent

export const shareApi = {
  // host
  start: (stackId, minutes, hideSecrets) => request('POST', `/api/stacks/${stackId}/share`, { minutes, hideSecrets }),
  live: () => request('GET', '/api/share/sessions?live=1'),
  get: (sid) => request('GET', `/api/share/sessions/${sid}`),
  admit: (sid, gid) => request('POST', `/api/share/sessions/${sid}/guests/${gid}/admit`),
  deny: (sid, gid) => request('POST', `/api/share/sessions/${sid}/guests/${gid}/deny`),
  remove: (sid, gid) => request('POST', `/api/share/sessions/${sid}/guests/${gid}/remove`),
  mute: (sid, gid, muted) => request('POST', `/api/share/sessions/${sid}/guests/${gid}/mute`, { muted }),
  control: (sid, to) => request('POST', `/api/share/sessions/${sid}/control`, { to }),
  newLink: (sid) => request('POST', `/api/share/sessions/${sid}/link`),
  end: (sid) => request('POST', `/api/share/sessions/${sid}/end`),
  transcripts: (stackId) => request('GET', `/api/stacks/${stackId}/share/transcripts`),
  transcriptURL: (sid, format = 'txt') => `/api/share/sessions/${sid}/transcript${format === 'json' ? '?format=json' : ''}`,
  // either side
  wsURL: (sid) => `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/share/sessions/${sid}/ws`,
  openTerm: (sid, spec) => request('POST', `/api/share/sessions/${sid}/terms`, spec),
  closeTerm: (sid, tid) => request('DELETE', `/api/share/sessions/${sid}/terms/${enc(tid)}`),
  termURL: (sid, tid) => `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/share/sessions/${sid}/terms/${enc(tid)}/ws`,
  // guest, before admission
  joinInfo: (token) => request('GET', `/api/join/${enc(token)}`),
  join: (token, name, email) => request('POST', `/api/join/${enc(token)}`, { name, email }),
  joinStatus: (token) => request('GET', `/api/join/${enc(token)}/status`),
  leave: (token) => request('POST', `/api/join/${enc(token)}/leave`),
}
