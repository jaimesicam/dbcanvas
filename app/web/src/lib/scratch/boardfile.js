// boardfile.js — a Scratchboard board as a file, as Markdown, and searched.
//
// A board exported here is the whole board — every card's content, Secrets included — written
// to a file the person chose to download; importing one opens it as a new temporary board.
// Markdown is for notes and write-ups: each card under its title, its text as it is.

const FILE_KIND = 'dbcanvas-scratchboard'

// exportBoard is the JSON file for one board.
export function exportBoard(board) {
  return JSON.stringify({ kind: FILE_KIND, version: 1, name: board.name, view: board.view, cards: board.cards, links: board.links || [] }, null, 2)
}

// importBoard reads a file written by exportBoard; known is the set of card types this build has.
export function importBoard(text, known) {
  let doc
  try { doc = JSON.parse(text) } catch { return { error: 'This is not a Scratchboard board file (not JSON).' } }
  if (!doc || doc.kind !== FILE_KIND || !Array.isArray(doc.cards)) return { error: 'This is not a Scratchboard board file.' }
  const cards = doc.cards.filter((c) => c && known.has(c.type) && c.id && c.state && typeof c.state === 'object')
  const ids = new Set(cards.map((c) => c.id))
  const links = (Array.isArray(doc.links) ? doc.links : []).filter((l) => l && ids.has(l.from) && ids.has(l.to))
  return {
    name: String(doc.name || 'Imported board').slice(0, 80),
    view: doc.view && Number.isFinite(doc.view.z) ? doc.view : { x: 0, y: 0, z: 1 },
    cards, links, skipped: doc.cards.length - cards.length,
  }
}

const fence = (text) => {
  const t = String(text)
  const ticks = (t.match(/`{3,}/g) || []).reduce((m, s) => Math.max(m, s.length + 1), 3)
  return `${'`'.repeat(ticks)}\n${t.replace(/\n$/, '')}\n${'`'.repeat(ticks)}`
}

// cardMarkdown is one card: a note as written, a checklist as a task list, and any other card's
// text fields each in a code block under the field's name.
export function cardMarkdown(card, label) {
  const head = `## ${card.title || label || card.type}`
  const st = card.state || {}
  if (card.type === 'note') return `${head}\n\n${st.text || ''}`.trim()
  if (card.type === 'checklist') return `${head}\n\n${(st.items || []).map((i) => `- [${i.done ? 'x' : ' '}] ${i.text}`).join('\n')}`.trim()
  const parts = Object.entries(st).filter(([, v]) => typeof v === 'string' && v.trim()).map(([k, v]) => (v.includes('\n') || v.length > 60 ? `**${k}**\n\n${fence(v)}` : `**${k}:** \`${v.replaceAll('`', '')}\``))
  return [head, ...parts].join('\n\n')
}

// boardMarkdown is the whole board, cards top to bottom, left to right.
export function boardMarkdown(board, labels = {}) {
  const cards = [...board.cards].sort((a, b) => a.y - b.y || a.x - b.x)
  return [`# ${board.name}`, ...cards.map((c) => cardMarkdown(c, labels[c.type]))].join('\n\n') + '\n'
}

// cardText is everything searchable on a card.
function cardText(card, label) {
  const out = [card.title || '', label || '']
  const walk = (v) => {
    if (typeof v === 'string') out.push(v)
    else if (Array.isArray(v)) v.forEach(walk)
    else if (v && typeof v === 'object') Object.values(v).forEach(walk)
  }
  walk(card.state)
  return out.join('\n')
}

// searchBoards finds q (case-insensitive) on every card of every board:
// [{ boardId, boardName, cardId, title, snippet }], at most `limit`.
export function searchBoards(boards, q, labels = {}, limit = 50) {
  const needle = String(q || '').trim().toLowerCase()
  if (!needle) return []
  const out = []
  for (const b of boards) {
    for (const c of b.cards) {
      const text = cardText(c, labels[c.type])
      const i = text.toLowerCase().indexOf(needle)
      if (i < 0) continue
      const from = Math.max(0, i - 30)
      const snippet = (from ? '…' : '') + text.slice(from, i + needle.length + 40).replace(/\s+/g, ' ')
      out.push({ boardId: b.id, boardName: b.name, cardId: c.id, title: c.title || labels[c.type] || c.type, snippet })
      if (out.length >= limit) return out
    }
  }
  return out
}
