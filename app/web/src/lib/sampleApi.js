// Sample Client Code API. Same conventions as lib/gdbApi.js — same-origin JSON, cookies ride
// along, a non-2xx throws an Error carrying .status.
//
// The split here is the one the feature is built on: the *catalogue* (what can be
// generated) is static and global, the *endpoints* belong to one Linux Client's stack,
// and everything that touches the node is a job you poll. Generating is deliberately not
// a job: it changes nothing, so it answers in one request and the page can re-render the
// code as fast as the picker changes.

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
    try { data = JSON.parse(text) } catch { data = null }
  }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

export const sampleApi = {
  // Every sample DBCanvas can generate, with the dependencies and licences each implies.
  catalog: () => request('GET', '/api/samplecode/catalog'),
  // Every running Linux Client, across the caller's stacks.
  nodes: () => request('GET', '/api/samplecode/nodes').then((r) => r?.nodes || []),
}

// sampleNodeApi is everything scoped to one Linux Client.
export function sampleNodeApi(stackId, nodeId) {
  const base = `/api/stacks/${stackId}/nodes/${nodeId}/samplecode`
  return {
    targets: () => request('GET', `${base}/targets`),
    generate: (req) => request('POST', `${base}/generate`, req),
    run: (req) => request('POST', `${base}/runs`, req).then((r) => r?.jobId),
    job: (id) => request('GET', `${base}/runs/${id}`),
    stop: (id) => request('POST', `${base}/runs/${id}/stop`),
  }
}

// nodeKey identifies one Linux Client across reloads — the same shape gdbTargetKey uses,
// and what the node panel's "Sample Client Code" button hands over.
export const nodeKey = (n) => (n ? `${n.stackId}/${n.nodeId}` : '')

// LOG_TONE maps a job log line's kind to how it is drawn. The distinction that matters is
// cmd (what DBCanvas is about to run) against out/err (what it said): a lab is exactly
// where someone wants to read the command and then type it themselves.
export const LOG_TONE = {
  step: 'text-fg font-semibold',
  cmd: 'text-primary',
  out: 'text-fg',
  err: 'text-warning',
  ok: 'text-success',
  fail: 'text-danger',
  info: 'text-muted',
}

export const LOG_PREFIX = {
  step: '',
  cmd: '$ ',
  out: '',
  err: '',
  ok: '✓ ',
  fail: '✗ ',
  info: '· ',
}

// TLS_MODES are the three postures, in the app's own vocabulary rather than any one
// driver's. The generated code spells each of them the way its own client wants.
export const TLS_MODES = [
  { id: 'off', label: 'Off', hint: 'Plaintext. Everything on the wire is readable — which is worth seeing once, in the Packet Inspector.' },
  { id: 'require', label: 'Require', hint: 'Encrypted, but the server is not identified. All a self-signed server certificate supports.' },
  { id: 'verify', label: 'Verify', hint: "Encrypted, and the certificate chain and hostname are checked against the stack CA already in this node's trust store." },
]

// FILE_LANG maps a generated file to the language label shown on its tab.
export const FILE_LANG = {
  python: 'Python', javascript: 'JavaScript', go: 'Go', java: 'Java', csharp: 'C#',
  xml: 'XML', json: 'JSON', shell: 'Shell', text: 'Text',
}

// downloadFiles hands the generated project to the browser as individual files. There is
// no archive endpoint on purpose: a sample is two or three small files, and a download
// per file needs nothing on the server and no library here.
export function downloadFile(name, body) {
  const blob = new Blob([body], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name.replace(/\//g, '-')
  document.body.appendChild(a)
  a.click()
  a.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
