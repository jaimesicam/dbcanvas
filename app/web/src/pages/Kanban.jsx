import { useRef, useState } from 'react'
import { Badge } from '../components/ui.jsx'

const COLUMNS = [
  { id: 'backlog', label: 'Backlog' },
  { id: 'todo', label: 'To do' },
  { id: 'doing', label: 'In progress' },
  { id: 'done', label: 'Done' },
]

const SEED = [
  { id: 'c1', col: 'backlog', title: 'Define theme tokens', points: 3, tone: 'primary' },
  { id: 'c2', col: 'backlog', title: 'Sketch node editor', points: 8, tone: 'accent' },
  { id: 'c3', col: 'todo', title: 'Build auth flow', points: 5, tone: 'primary' },
  { id: 'c4', col: 'todo', title: 'Data table sorting', points: 3, tone: 'success' },
  { id: 'c5', col: 'doing', title: 'Port handles + bezier', points: 8, tone: 'accent' },
  { id: 'c6', col: 'doing', title: 'Command palette', points: 2, tone: 'primary' },
  { id: 'c7', col: 'done', title: 'Project scaffold', points: 1, tone: 'success' },
  { id: 'c8', col: 'done', title: 'Docker image', points: 5, tone: 'success' },
]

const TONE_BAR = { primary: 'bg-primary', accent: 'bg-accent', success: 'bg-success' }

export default function Kanban() {
  const [cards, setCards] = useState(SEED)
  const [over, setOver] = useState(null)
  const dragId = useRef(null)

  function onDrop(col) {
    const id = dragId.current
    if (id) setCards((cs) => cs.map((c) => (c.id === id ? { ...c, col } : c)))
    dragId.current = null
    setOver(null)
  }

  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
      {COLUMNS.map((col) => {
        const colCards = cards.filter((c) => c.col === col.id)
        const points = colCards.reduce((s, c) => s + c.points, 0)
        return (
          <div
            key={col.id}
            onDragOver={(e) => {
              e.preventDefault()
              setOver(col.id)
            }}
            onDragLeave={() => setOver((o) => (o === col.id ? null : o))}
            onDrop={() => onDrop(col.id)}
            className={`flex flex-col rounded-xl border bg-surface transition ${over === col.id ? 'ring-2 ring-primary' : ''}`}
          >
            <div className="flex items-center justify-between border-b px-3 py-2.5">
              <span className="text-sm font-semibold">{col.label}</span>
              <span className="flex items-center gap-2 text-xs text-muted">
                <Badge tone="muted">{colCards.length}</Badge>
                {points} pts
              </span>
            </div>
            <div className="flex min-h-[120px] flex-1 flex-col gap-2 p-2">
              {colCards.map((c) => (
                <div
                  key={c.id}
                  draggable
                  onDragStart={() => (dragId.current = c.id)}
                  onDragEnd={() => (dragId.current = null)}
                  className="cursor-grab rounded-lg border bg-bg p-3 shadow-sm transition hover:shadow active:cursor-grabbing"
                >
                  <div className={`mb-2 h-1 w-8 rounded-full ${TONE_BAR[c.tone]}`} />
                  <div className="text-sm font-medium text-fg">{c.title}</div>
                  <div className="mt-2 text-xs text-muted">{c.points} story points</div>
                </div>
              ))}
              {colCards.length === 0 && (
                <div className="flex flex-1 items-center justify-center rounded-lg border-2 border-dashed text-xs text-muted">
                  Drop here
                </div>
              )}
            </div>
          </div>
        )
      })}
    </div>
  )
}
