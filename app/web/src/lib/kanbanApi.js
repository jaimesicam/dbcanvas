// kanbanApi.js — Kanban boards (app/kanban.go).

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
    throw err
  }
  return data
}

export const kanbanApi = {
  boards: () => request('GET', '/api/kanban/boards'),
  createBoard: (name, shared, columns) => request('POST', '/api/kanban/boards', { name, shared, columns }),
  board: (id, since) => request('GET', `/api/kanban/boards/${id}${since ? `?since=${since}` : ''}`),
  updateBoard: (id, patch) => request('PUT', `/api/kanban/boards/${id}`, patch),
  deleteBoard: (id) => request('DELETE', `/api/kanban/boards/${id}`),
  addColumn: (boardId, name, index) => request('POST', `/api/kanban/boards/${boardId}/columns`, { name, index }),
  updateColumn: (cid, name, wipLimit) => request('PUT', `/api/kanban/columns/${cid}`, { name, wipLimit }),
  deleteColumn: (cid) => request('DELETE', `/api/kanban/columns/${cid}`),
  moveColumn: (cid, index) => request('POST', `/api/kanban/columns/${cid}/move`, { index }),
  addCard: (cid, card) => request('POST', `/api/kanban/columns/${cid}/cards`, card),
  updateCard: (id, card) => request('PUT', `/api/kanban/cards/${id}`, card),
  deleteCard: (id) => request('DELETE', `/api/kanban/cards/${id}`),
  moveCard: (id, columnId, index) => request('POST', `/api/kanban/cards/${id}/move`, { columnId, index }),
  people: () => request('GET', '/api/kanban/people'),
}

// The label palette (app/kanban.go's kanbanLabelColors).
export const LABEL_COLORS = [
  { id: 'red', hex: '#ef4444' }, { id: 'orange', hex: '#f97316' }, { id: 'yellow', hex: '#eab308' },
  { id: 'green', hex: '#22c55e' }, { id: 'teal', hex: '#14b8a6' }, { id: 'blue', hex: '#3b82f6' },
  { id: 'purple', hex: '#a855f7' }, { id: 'pink', hex: '#ec4899' }, { id: 'gray', hex: '#6b7280' },
]
export const labelHex = (c) => LABEL_COLORS.find((x) => x.id === c)?.hex || '#6b7280'

// Board templates: the columns a new board starts with.
export const BOARD_TEMPLATES = [
  { id: 'basic', label: 'Basic', columns: ['To do', 'In progress', 'Done'] },
  { id: 'sprint', label: 'Sprint', columns: ['Backlog', 'To do', 'In progress', 'Review', 'Done'] },
  { id: 'incident', label: 'Incident', columns: ['Triage', 'Investigating', 'Mitigated', 'Resolved'] },
  { id: 'repro', label: 'Support repro', columns: ['Reported', 'Reproducing', 'Reproduced', 'Fix verified', 'Closed'] },
  { id: 'blank', label: 'Blank', columns: ['New column'] },
]

// moveLocal is a card move applied to the board as held in the browser: the same
// rule the server applies (MoveKanbanCard), so the screen shows the result at once.
export function moveLocal(cards, id, toCol, index) {
  const card = cards.find((c) => c.id === id)
  if (!card) return cards
  const fromCol = card.columnId
  const target = cards.filter((c) => c.columnId === toCol && c.id !== id).sort((a, b) => a.position - b.position)
  const i = Math.max(0, Math.min(index, target.length))
  target.splice(i, 0, { ...card, columnId: toCol })
  const pos = new Map(target.map((c, n) => [c.id, n]))
  if (fromCol !== toCol) {
    cards.filter((c) => c.columnId === fromCol && c.id !== id).sort((a, b) => a.position - b.position)
      .forEach((c, n) => pos.set(c.id, n))
  }
  return cards.map((c) => (c.id === id ? { ...c, columnId: toCol, position: pos.get(id) } : pos.has(c.id) ? { ...c, position: pos.get(c.id) } : c))
}

export function cardsIn(cards, colId) {
  return cards.filter((c) => c.columnId === colId).sort((a, b) => a.position - b.position || a.id - b.id)
}
