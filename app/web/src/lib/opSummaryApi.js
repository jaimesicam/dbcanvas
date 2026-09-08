// Operator Summary API — distil a pt-k8s-debug-collector cluster-dump into
// workload health, custom-resource status and operator errors.
//
// Its own module rather than a corner of stackApi, for the same reason stalkApi
// is: stackApi's request() JSON-encodes every body and sets an application/json
// content type, which an upload cannot use.

async function toJSON(res) {
  const text = await res.text()
  let data = null
  if (text) { try { data = JSON.parse(text) } catch { data = null } }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`)
    err.status = res.status
    throw err
  }
  return data
}

export const opSummaryApi = {
  // Kept captures, newest first. They outlive the clusters they came from, which
  // is the point: a cluster-dump is the evidence of a moment.
  dumps: async () =>
    (await toJSON(await fetch('/api/opsummary/dumps', { credentials: 'same-origin' })))?.dumps || [],
  fromDump: async (id) =>
    toJSON(await fetch(`/api/opsummary/dumps/${id}`, { method: 'POST', credentials: 'same-origin' })),
  upload: async (file) => {
    const fd = new FormData()
    fd.append('file', file)
    return toJSON(await fetch('/api/opsummary/upload', { method: 'POST', body: fd, credentials: 'same-origin' }))
  },
  // Capture and analyse in one request, from a K3D node's Diagnostics tab.
  fromCluster: async (stackId, frameId) =>
    toJSON(await fetch(`/api/stacks/${stackId}/frames/${frameId}/k3d/opsummary`, { method: 'POST', credentials: 'same-origin' })),

  downloadURL: (id) => `/api/k8sdumps/${id}/download`,
  remove: async (id) =>
    toJSON(await fetch(`/api/k8sdumps/${id}`, { method: 'DELETE', credentials: 'same-origin' })),
}
