// Visual Summary API — parse a pt-stalk archive (uploaded or from a node) into a chart model.

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

export const visualApi = {
  // MySQL/PXC nodes that could have a pt-stalk capture (reuses the Query Runner targets).
  nodes: async () => {
    const all = await toJSON(await fetch('/api/queryrun/targets', { credentials: 'same-origin' }))
    return (all || []).filter((t) => t.engine === 'mysql')
  },
  upload: async (file) => {
    const fd = new FormData()
    fd.append('file', file)
    return toJSON(await fetch('/api/visualsummary/upload', { method: 'POST', body: fd, credentials: 'same-origin' }))
  },
  fromNode: async (stackId, nodeId) =>
    toJSON(await fetch(`/api/stacks/${stackId}/nodes/${nodeId}/visualsummary`, { method: 'POST', credentials: 'same-origin' })),
}
