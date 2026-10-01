import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Button, inputCls } from '../components/ui.jsx'
import { useDialog } from '../components/Dialog.jsx'
import { Avatar } from '../components/Avatar.jsx'
import { useAuth } from '../auth/AuthProvider.jsx'
import { useRefresh } from '../lib/useRefresh.jsx'
import { usePageVisible } from '../lib/usePolling.jsx'
import { kanbanApi, LABEL_COLORS, labelHex, tint, BOARD_TEMPLATES, moveLocal, cardsIn } from '../lib/kanbanApi.js'

// Kanban — boards of columns of cards (app/kanban.go).
//
// The point of the page is moving work around, so moving is what it is built for:
//
//   - Drag a card anywhere: within its column, to another column, to an exact place
//     between two cards. A gap opens where it will land, the card follows the
//     pointer, the board scrolls when you reach an edge, and Escape puts it back.
//     On a touch screen, press and hold a card to pick it up (a plain swipe scrolls).
//   - Drag a column by its header to reorder the columns.
//   - From the keyboard: focus a card (Tab), Space picks it up, the arrow keys move
//     it — up and down within a column, left and right across columns — Space
//     drops it and Escape cancels. Enter opens it.
//   - A card added from a column's footer lands at the bottom; the composer stays
//     open for the next one. A deleted card can be undone for a few seconds.
//   - Cards and columns can carry a colour (the label palette): right-click a card,
//     or a column's ⋯, or the card's own dialog.
//
// Every change is shown at once and then saved; a refusal puts the board back as
// the server has it. A board open here is re-read every few seconds (only when it
// changed — app/kanban.go's rev), so a shared board shows what colleagues do.

const LAST_BOARD = 'dbcanvas-kanban-board'
const POLL_MS = 4000
const DRAG_PX = 5
const HOLD_MS = 300

function dueInfo(due) {
  if (!due) return null
  const today = new Date(); today.setHours(0, 0, 0, 0)
  const d = new Date(`${due}T00:00:00`)
  const days = Math.round((d - today) / 86400000)
  const label = d.toLocaleDateString([], { month: 'short', day: 'numeric' })
  if (days < 0) return { label, tone: 'bg-danger/15 text-danger', title: `Overdue by ${-days} day${days === -1 ? '' : 's'}` }
  if (days === 0) return { label: 'Today', tone: 'bg-warning/20 text-warning', title: 'Due today' }
  if (days === 1) return { label: 'Tomorrow', tone: 'bg-warning/10 text-warning', title: 'Due tomorrow' }
  return { label, tone: 'bg-surface2 text-muted', title: `Due in ${days} days` }
}

function readLast() { try { return Number(localStorage.getItem(LAST_BOARD)) || null } catch { return null } }
function writeLast(id) { try { localStorage.setItem(LAST_BOARD, String(id || '')) } catch { /* */ } }

export default function Kanban() {
  const { user } = useAuth()
  const visible = usePageVisible()
  const [dialog, ask] = useDialog()
  const [boards, setBoards] = useState(null)
  const [boardId, setBoardId] = useState(readLast)
  const [data, setData] = useState(null) // { board, columns, cards }
  const [people, setPeople] = useState([])
  const [err, setErr] = useState('')
  const [creating, setCreating] = useState(false)
  const [openCard, setOpenCard] = useState(null) // card id
  const [toast, setToast] = useState(null) // { text, undo }
  const [query, setQuery] = useState('')
  const [who, setWho] = useState(null) // assignee filter
  const busy = useRef(0) // writes in flight: a poll then would show a half-applied state
  const dataRef = useRef(data)
  dataRef.current = data

  const loadBoards = useCallback(async () => {
    try {
      const l = await kanbanApi.boards()
      setBoards(l)
      return l
    } catch (e) { setErr(e.message); return [] }
  }, [])
  const loadBoard = useCallback(async (id, since) => {
    if (!id) { setData(null); return }
    try {
      const d = await kanbanApi.board(id, since)
      if (d.unchanged) return
      setData(d)
      setErr('')
      // The list beside the board keeps its count and name in step.
      setBoards((bs) => bs?.map((b) => (b.id === d.board.id ? { ...b, name: d.board.name, shared: d.board.shared, cards: d.cards.length } : b)))
    } catch (e) {
      if (e.status === 404) { setData(null); setBoardId(null); writeLast(null) } else setErr(e.message)
    }
  }, [])

  useEffect(() => {
    loadBoards().then((l) => {
      setBoardId((cur) => (cur && l.some((b) => b.id === cur) ? cur : l[0]?.id ?? null))
    })
    kanbanApi.people().then((p) => setPeople(Array.isArray(p) ? p : [])).catch(() => {})
  }, [loadBoards])
  useEffect(() => { writeLast(boardId); setOpenCard(null); loadBoard(boardId) }, [boardId, loadBoard])
  useRefresh(() => Promise.all([loadBoards(), loadBoard(boardId)]))

  // Follow colleagues on a shared board: a cheap "anything since rev N?" now and then.
  const dragging = useRef(false)
  useEffect(() => {
    if (!visible || !boardId) return undefined
    const t = setInterval(() => {
      if (dragging.current || busy.current > 0 || document.hidden) return
      loadBoard(boardId, dataRef.current?.board?.rev)
    }, POLL_MS)
    return () => clearInterval(t)
  }, [visible, boardId, loadBoard])

  // write runs a change: shown first (optimistic), then saved; on a refusal the
  // board is read back as the server has it and the reason is shown.
  const write = useCallback(async (optimistic, call) => {
    busy.current += 1
    if (optimistic) setData((d) => (d ? optimistic(d) : d))
    try {
      const r = await call()
      return r
    } catch (e) {
      setErr(e.message)
      return null
    } finally {
      busy.current -= 1
      if (busy.current === 0) loadBoard(boardId)
    }
  }, [boardId, loadBoard])

  // ---- board actions
  const createBoard = async (name, template, shared) => {
    const t = BOARD_TEMPLATES.find((x) => x.id === template) || BOARD_TEMPLATES[0]
    try {
      const b = await kanbanApi.createBoard(name, shared, t.columns)
      await loadBoards()
      setBoardId(b.id)
      setCreating(false)
    } catch (e) { setErr(e.message) }
  }
  const renameBoard = async (b) => {
    const name = await ask.prompt({ title: 'Rename board', label: 'Name', initial: b.name, confirmLabel: 'Rename' })
    if (!name?.trim()) return
    await kanbanApi.updateBoard(b.id, { name: name.trim() }).catch((e) => setErr(e.message))
    loadBoards(); if (b.id === boardId) loadBoard(boardId)
  }
  const shareBoard = async (b, shared) => {
    await kanbanApi.updateBoard(b.id, { shared }).catch((e) => setErr(e.message))
    loadBoards(); if (b.id === boardId) loadBoard(boardId)
  }
  const deleteBoard = async (b) => {
    const yes = await ask.confirm({
      title: `Delete “${b.name}”?`,
      body: `The board and its ${b.cards} card${b.cards === 1 ? '' : 's'} are deleted for good${b.shared ? ', for everyone it is shared with' : ''}.`,
      confirmLabel: 'Delete board', danger: true,
    })
    if (!yes) return
    await kanbanApi.deleteBoard(b.id).catch((e) => setErr(e.message))
    const l = await loadBoards()
    if (b.id === boardId) setBoardId(l[0]?.id ?? null)
  }

  // ---- column actions
  const addColumn = async () => {
    const name = await ask.prompt({ title: 'Add a column', label: 'Name', confirmLabel: 'Add column' })
    if (!name?.trim()) return
    write(null, () => kanbanApi.addColumn(boardId, name.trim()))
  }
  const renameColumn = (col, name) => write(
    (d) => ({ ...d, columns: d.columns.map((c) => (c.id === col.id ? { ...c, name } : c)) }),
    () => kanbanApi.updateColumn(col.id, name, col.wipLimit),
  )
  const setWip = async (col) => {
    const v = await ask.prompt({ title: `Work-in-progress limit for “${col.name}”`, label: 'Most cards (0 for no limit)', initial: String(col.wipLimit || 0), confirmLabel: 'Set limit' })
    if (v === null) return
    const n = Math.max(0, Math.min(999, parseInt(v, 10) || 0))
    write((d) => ({ ...d, columns: d.columns.map((c) => (c.id === col.id ? { ...c, wipLimit: n } : c)) }),
      () => kanbanApi.updateColumn(col.id, col.name, n))
  }
  const colorColumn = (col, color) => write(
    (d) => ({ ...d, columns: d.columns.map((c) => (c.id === col.id ? { ...c, color } : c)) }),
    () => kanbanApi.colorColumn(col.id, color),
  )
  const deleteColumn = async (col) => {
    const n = cardsIn(data.cards, col.id).length
    const yes = await ask.confirm({
      title: `Delete the “${col.name}” column?`,
      body: n ? `Its ${n} card${n === 1 ? ' goes' : 's go'} with it. Move ${n === 1 ? 'it' : 'them'} first to keep ${n === 1 ? 'it' : 'them'}.` : 'It is empty.',
      confirmLabel: 'Delete column', danger: true,
    })
    if (!yes) return
    write((d) => ({ ...d, columns: d.columns.filter((c) => c.id !== col.id), cards: d.cards.filter((c) => c.columnId !== col.id) }),
      () => kanbanApi.deleteColumn(col.id))
  }
  const moveColumn = (colId, index) => write(
    (d) => {
      const cols = d.columns.filter((c) => c.id !== colId)
      cols.splice(index, 0, d.columns.find((c) => c.id === colId))
      return { ...d, columns: cols.map((c, i) => ({ ...c, position: i })) }
    },
    () => kanbanApi.moveColumn(colId, index),
  )

  // ---- card actions
  const tempId = useRef(-1)
  const addCard = (colId, title, top) => {
    const id = tempId.current--
    const index = top ? 0 : cardsIn(dataRef.current.cards, colId).length
    const tmp = { id, columnId: colId, title, description: '', labels: [], assigneeId: 0, due: '', position: 1e9 }
    write(
      (d) => ({ ...d, cards: moveLocal([...d.cards, tmp], id, colId, index) }),
      () => kanbanApi.addCard(colId, { title, index }),
    )
  }
  const moveCard = (id, colId, index) => write(
    (d) => ({ ...d, cards: moveLocal(d.cards, id, colId, index) }),
    () => kanbanApi.moveCard(id, colId, index),
  )
  const colorCard = (card, color) => write(
    (d) => ({ ...d, cards: d.cards.map((c) => (c.id === card.id ? { ...c, color } : c)) }),
    () => kanbanApi.updateCard(card.id, { ...card, color }),
  )
  const saveCard = (card) => write(
    (d) => ({ ...d, cards: d.cards.map((c) => (c.id === card.id ? { ...c, ...card } : c)) }),
    () => kanbanApi.updateCard(card.id, card),
  )
  const deleteCard = (card) => {
    const index = cardsIn(dataRef.current.cards, card.columnId).findIndex((c) => c.id === card.id)
    setOpenCard(null)
    write((d) => ({ ...d, cards: d.cards.filter((c) => c.id !== card.id) }), () => kanbanApi.deleteCard(card.id))
    setToast({
      text: `Deleted “${card.title.length > 40 ? `${card.title.slice(0, 40)}…` : card.title}”`,
      undo: () => write(null, () => kanbanApi.addCard(card.columnId, {
        title: card.title, description: card.description, labels: card.labels, assigneeId: card.assigneeId, due: card.due, color: card.color, index,
      })),
    })
  }
  useEffect(() => {
    if (!toast) return undefined
    const t = setTimeout(() => setToast(null), 7000)
    return () => clearTimeout(t)
  }, [toast])

  const board = data?.board
  const mine = board && board.ownerId === user?.id
  const card = openCard && data?.cards.find((c) => c.id === openCard)
  const filtering = !!(query.trim() || who)
  const matches = useMemo(() => {
    const q = query.trim().toLowerCase()
    return (c) => (!who || c.assigneeId === who) && (!q || `${c.title} ${c.description} ${c.labels.map((l) => l.text).join(' ')}`.toLowerCase().includes(q))
  }, [query, who])

  return (
    <div className="flex h-full min-h-[480px] gap-4">
      {/* boards */}
      <aside className="flex w-56 shrink-0 flex-col rounded-xl border bg-surface">
        <div className="flex items-center justify-between border-b px-3 py-2">
          <span className="text-sm font-semibold">Boards</span>
          <button onClick={() => setCreating(true)} title="New board" className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"><Icon.Plus size={16} /></button>
        </div>
        <div className="min-h-0 flex-1 space-y-0.5 overflow-y-auto p-1.5">
          {boards?.length === 0 && <div className="px-2 py-3 text-xs text-muted">No boards yet.</div>}
          {boards?.map((b) => (
            <BoardRow key={b.id} b={b} on={b.id === boardId} mine={b.ownerId === user?.id} onOpen={() => setBoardId(b.id)}
              onRename={() => renameBoard(b)} onShare={(s) => shareBoard(b, s)} onDelete={() => deleteBoard(b)} />
          ))}
        </div>
        <div className="border-t p-2">
          <Button variant="outline" size="sm" className="w-full" onClick={() => setCreating(true)}><Icon.Plus size={14} /> New board</Button>
        </div>
      </aside>

      {/* the board */}
      <section className="flex min-w-0 flex-1 flex-col">
        {err && (
          <div className="mb-2 flex items-center gap-2 rounded-lg border border-danger/30 bg-danger/10 px-3 py-1.5 text-xs text-danger">
            <span className="flex-1">{err}</span><button onClick={() => setErr('')}>✕</button>
          </div>
        )}
        {!boards ? (
          <div className="p-6 text-sm text-muted">Loading…</div>
        ) : !board ? (
          <EmptyState onCreate={() => setCreating(true)} hasBoards={boards.length > 0} />
        ) : (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <h2 className="mr-1 min-w-0 truncate text-lg font-semibold" title={mine ? 'Double-click to rename' : undefined}
                onDoubleClick={() => mine && renameBoard(board)}>{board.name}</h2>
              {board.shared
                ? <span className="rounded-full bg-primary/15 px-2 py-0.5 text-[11px] text-primary" title="Every signed-in user can see and change it">Shared</span>
                : <span className="rounded-full bg-surface2 px-2 py-0.5 text-[11px] text-muted">Private</span>}
              {!mine && <span className="text-xs text-muted">by {board.ownerName}</span>}
              <div className="ml-auto flex items-center gap-2">
                <div className="relative">
                  <Icon.Search size={14} className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-muted" />
                  <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Filter cards…"
                    className={`${inputCls} h-8 w-48 pl-7 text-xs`} />
                </div>
                <PeopleFilter people={people} cards={data.cards} value={who} onChange={setWho} />
                <Button size="sm" variant="outline" onClick={addColumn}><Icon.Plus size={14} /> Column</Button>
              </div>
            </div>
            <Board data={data} people={people} filtering={filtering} matches={matches}
              dragging={dragging} onMoveCard={moveCard} onMoveColumn={moveColumn} onOpenCard={setOpenCard}
              onAddCard={addCard} onRenameColumn={renameColumn} onWip={setWip} onDeleteColumn={deleteColumn}
              onColorColumn={colorColumn} onColorCard={colorCard} onDeleteCard={deleteCard} />
          </>
        )}
      </section>

      {creating && <NewBoardDialog onClose={() => setCreating(false)} onCreate={createBoard} />}
      {card && (
        <CardDialog card={card} columns={data.columns} people={people} onClose={() => setOpenCard(null)}
          onSave={saveCard} onDelete={() => deleteCard(card)}
          onMove={(colId) => moveCard(card.id, colId, cardsIn(data.cards, colId).length)} />
      )}
      {toast && createPortal(
        <div className="fixed bottom-14 left-1/2 z-[95] flex -translate-x-1/2 items-center gap-3 rounded-lg border bg-surface px-4 py-2 text-sm shadow-xl">
          <span>{toast.text}</span>
          {toast.undo && <button className="font-medium text-primary hover:underline" onClick={() => { toast.undo(); setToast(null) }}>Undo</button>}
          <button className="text-muted hover:text-fg" onClick={() => setToast(null)}>✕</button>
        </div>,
        document.body,
      )}
      {dialog}
    </div>
  )
}

function BoardRow({ b, on, mine, onOpen, onRename, onShare, onDelete }) {
  const [menu, setMenu] = useState(null)
  return (
    <div className={`group flex items-center gap-1 rounded-md ${on ? 'bg-primary/15 text-primary' : 'hover:bg-surface2'}`}
      onContextMenu={(e) => { if (mine) { e.preventDefault(); setMenu({ x: e.clientX, y: e.clientY }) } }}>
      <button onClick={onOpen} onDoubleClick={() => mine && onRename()} className="flex min-w-0 flex-1 items-center gap-2 px-2 py-1.5 text-left text-sm">
        <Icon.Kanban size={14} className="shrink-0" />
        <span className="min-w-0 flex-1 truncate">{b.name}</span>
        {b.shared && <Icon.Users size={12} className="shrink-0 text-muted" title="Shared" />}
        <span className="shrink-0 text-[10px] tabular-nums text-muted">{b.cards}</span>
      </button>
      {mine && (
        <button onClick={(e) => { const r = e.currentTarget.getBoundingClientRect(); setMenu({ x: r.left, y: r.bottom }) }}
          className="mr-1 rounded px-1 text-muted opacity-0 hover:bg-surface hover:text-fg group-hover:opacity-100" title="Board actions">⋯</button>
      )}
      {menu && (
        <Menu at={menu} onClose={() => setMenu(null)} items={[
          { label: 'Rename', onClick: onRename },
          { label: b.shared ? 'Make private' : 'Share with everyone', onClick: () => onShare(!b.shared) },
          { sep: true },
          { label: 'Delete board', danger: true, onClick: onDelete },
        ]} />
      )}
    </div>
  )
}

function Menu({ at, items, onClose }) {
  return createPortal(
    <>
      <div className="fixed inset-0 z-[90]" onPointerDown={onClose} onContextMenu={(e) => { e.preventDefault(); onClose() }} />
      <div className="fixed z-[91] min-w-[180px] rounded-md border bg-surface py-1 text-xs shadow-xl"
        style={{ left: Math.min(at.x, innerWidth - 190), top: Math.min(at.y, innerHeight - 30 * items.length - 10) }}>
        {items.map((it, i) => (it.sep ? <div key={i} className="my-1 border-t" /> : it.swatches ? (
          <div key={`sw${i}`} className="px-3 py-1.5">
            <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-muted">{it.label}</div>
            <Swatches value={it.value} onPick={(c) => { it.onPick(c); onClose() }} />
          </div>
        ) : (
          <button key={it.label} onClick={() => { it.onClick(); onClose() }}
            className={`block w-full px-3 py-1.5 text-left ${it.danger ? 'text-danger hover:bg-danger/10' : 'text-fg hover:bg-surface2'}`}>{it.label}</button>
        )))}
      </div>
    </>,
    document.body,
  )
}

// Swatches picks a palette colour, or none.
function Swatches({ value, onPick, size = 18 }) {
  return (
    <div className="flex flex-wrap items-center gap-1">
      <button type="button" title="No colour" onClick={() => onPick('')}
        className={`flex items-center justify-center rounded border text-[10px] text-muted ${!value ? 'ring-2 ring-primary ring-offset-1 ring-offset-surface' : ''}`}
        style={{ width: size, height: size }}>∅</button>
      {LABEL_COLORS.map((c) => (
        <button type="button" key={c.id} title={c.id} onClick={() => onPick(c.id)}
          className={`rounded ${value === c.id ? 'ring-2 ring-primary ring-offset-1 ring-offset-surface' : 'opacity-85 hover:opacity-100'}`}
          style={{ width: size, height: size, background: c.hex }} />
      ))}
    </div>
  )
}

function PeopleFilter({ people, cards, value, onChange }) {
  const assigned = useMemo(() => new Set(cards.map((c) => c.assigneeId).filter(Boolean)), [cards])
  const list = people.filter((p) => assigned.has(p.id))
  if (!list.length) return null
  return (
    <div className="flex items-center -space-x-1" title="Show only one person's cards">
      {list.slice(0, 8).map((p) => (
        <button key={p.id} onClick={() => onChange(value === p.id ? null : p.id)}
          className={`rounded-full ring-2 transition ${value === p.id ? 'z-10 ring-primary' : value ? 'opacity-40 ring-surface' : 'ring-surface hover:z-10'}`}>
          <Avatar avatar={p.avatar} name={p.name} size={24} />
        </button>
      ))}
    </div>
  )
}

function EmptyState({ onCreate, hasBoards }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center rounded-xl border border-dashed p-10 text-center">
      <Icon.Kanban size={36} className="mb-3 text-primary" />
      <div className="text-base font-semibold">{hasBoards ? 'Pick a board' : 'Plan the work on a board'}</div>
      <p className="mt-1 max-w-md text-sm text-muted">
        Columns for each stage, cards for each piece of work — drag them where they belong, in the order you want.
        Keep a board to yourself or share it with everyone here.
      </p>
      <Button className="mt-4" onClick={onCreate}><Icon.Plus size={15} /> New board</Button>
    </div>
  )
}

function NewBoardDialog({ onClose, onCreate }) {
  const [name, setName] = useState('')
  const [tpl, setTpl] = useState('basic')
  const [shared, setShared] = useState(false)
  const submit = (e) => { e.preventDefault(); if (name.trim()) onCreate(name.trim(), tpl, shared) }
  return createPortal(
    <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <form onSubmit={submit} onMouseDown={(e) => e.stopPropagation()} onKeyDown={(e) => e.key === 'Escape' && onClose()}
        className="w-full max-w-lg space-y-4 rounded-2xl border bg-surface p-5 shadow-xl">
        <h2 className="text-base font-semibold">New board</h2>
        <input autoFocus className={inputCls} placeholder="Board name — e.g. Release 1.0" value={name} onChange={(e) => setName(e.target.value)} maxLength={120} />
        <div>
          <div className="mb-1.5 text-xs font-medium text-muted">Start with</div>
          <div className="grid gap-2 sm:grid-cols-2">
            {BOARD_TEMPLATES.map((t) => (
              <button type="button" key={t.id} onClick={() => setTpl(t.id)}
                className={`rounded-lg border p-2.5 text-left transition ${tpl === t.id ? 'border-primary bg-primary/10' : 'hover:bg-surface2'}`}>
                <div className="text-sm font-medium">{t.label}</div>
                <div className="truncate text-[11px] text-muted">{t.columns.join(' → ')}</div>
              </button>
            ))}
          </div>
        </div>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={shared} onChange={(e) => setShared(e.target.checked)} className="mt-1" />
          <span>Share with everyone<span className="block text-xs text-muted">Every signed-in user can see it and work on its cards. Only you can rename, share or delete it.</span></span>
        </label>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={!name.trim()}>Create board</Button>
        </div>
      </form>
    </div>,
    document.body,
  )
}

// ------------------------------------------------------------------ the board

// Board draws the columns and runs the dragging: cards by their body, columns by
// their header, with the pointer or the keyboard.
function Board({ data, people, filtering, matches, dragging, onMoveCard, onMoveColumn, onOpenCard, onAddCard, onRenameColumn, onWip, onDeleteColumn, onColorColumn, onColorCard, onDeleteCard }) {
  const scroller = useRef(null)
  const colEls = useRef(new Map()) // colId -> column element
  const bodyEls = useRef(new Map()) // colId -> card list element
  const ghost = useRef(null)
  const pending = useRef(null)
  const live = useRef(null) // the drag, for the listeners
  const [drag, setDrag] = useState(null) // { kind, id, w, h, overCol, overIndex }
  const [kbd, setKbd] = useState(null) // { id, col, index } — a card lifted from the keyboard
  const [announce, setAnnounce] = useState('')
  const cols = data.columns
  const byCol = useMemo(() => Object.fromEntries(cols.map((c) => [c.id, cardsIn(data.cards, c.id)])), [cols, data.cards])
  const personOf = useMemo(() => Object.fromEntries(people.map((p) => [p.id, p])), [people])

  // A card's place, as the cards on screen show it, turned into its place in the
  // whole column (a filter hides some).
  const realIndex = (colId, visibleIndex, id) => {
    const all = byCol[colId].filter((c) => c.id !== id)
    if (!filtering) return visibleIndex
    const vis = all.filter(matches)
    if (visibleIndex < vis.length) return all.indexOf(vis[visibleIndex])
    return vis.length ? all.indexOf(vis[vis.length - 1]) + 1 : all.length
  }

  // ---- pointer dragging
  const target = (x, y, d) => {
    if (d.kind === 'column') {
      let index = 0
      for (const c of cols) {
        if (c.id === d.id) continue
        const r = colEls.current.get(c.id)?.getBoundingClientRect()
        if (r && r.left + r.width / 2 < x) index++
      }
      return { overIndex: index }
    }
    let best = null
    for (const c of cols) {
      const r = colEls.current.get(c.id)?.getBoundingClientRect()
      if (!r) continue
      const dist = x < r.left ? r.left - x : x > r.right ? x - r.right : 0
      if (!best || dist < best.dist) best = { id: c.id, dist }
    }
    if (!best) return {}
    const body = bodyEls.current.get(best.id)
    let index = 0
    body?.querySelectorAll('[data-card-id]').forEach((el) => {
      if (Number(el.dataset.cardId) === d.id) return
      const r = el.getBoundingClientRect()
      if (r.top + r.height / 2 < y) index++
    })
    return { overCol: best.id, overIndex: index }
  }

  const begin = (p, x, y) => {
    const r = p.el.getBoundingClientRect()
    const d = { kind: p.kind, id: p.id, w: r.width, h: r.height, ox: x - r.left, oy: y - r.top, x, y }
    Object.assign(d, target(x, y, d))
    live.current = d
    dragging.current = true
    document.body.style.userSelect = 'none'
    document.body.style.cursor = 'grabbing'
    setDrag(d)
  }
  const placeGhost = () => {
    const d = live.current
    if (d && ghost.current) ghost.current.style.transform = `translate(${d.x - d.ox}px, ${d.y - d.oy}px) rotate(${d.kind === 'card' ? 2 : 1}deg)`
  }
  useLayoutEffect(placeGhost, [drag])

  useEffect(() => {
    let raf = 0
    const tick = () => {
      // Near an edge, the board (sideways) and the column (up and down) scroll.
      const d = live.current
      if (!d) return
      const sc = scroller.current
      if (sc) {
        const r = sc.getBoundingClientRect()
        const edge = 70
        if (d.x < r.left + edge) sc.scrollLeft -= Math.ceil((r.left + edge - d.x) / 5)
        else if (d.x > r.right - edge) sc.scrollLeft += Math.ceil((d.x - (r.right - edge)) / 5)
      }
      if (d.kind === 'card' && d.overCol) {
        const b = bodyEls.current.get(d.overCol)
        if (b) {
          const r = b.getBoundingClientRect()
          if (d.y < r.top + 50) b.scrollTop -= Math.ceil((r.top + 50 - d.y) / 4)
          else if (d.y > r.bottom - 50) b.scrollTop += Math.ceil((d.y - (r.bottom - 50)) / 4)
        }
      }
      const t = target(d.x, d.y, d)
      if (t.overCol !== d.overCol || t.overIndex !== d.overIndex) {
        Object.assign(d, t)
        setDrag({ ...d })
      }
      raf = requestAnimationFrame(tick)
    }
    const move = (e) => {
      const p = pending.current
      if (p && !live.current) {
        const dist = Math.hypot(e.clientX - p.sx, e.clientY - p.sy)
        if (p.touch) { if (dist > 8) { clearTimeout(p.timer); pending.current = null } return }
        if (dist < DRAG_PX) return
        pending.current = null
        begin(p, e.clientX, e.clientY)
        raf = requestAnimationFrame(tick)
      }
      const d = live.current
      if (!d) return
      e.preventDefault()
      d.x = e.clientX; d.y = e.clientY
      placeGhost()
    }
    const finish = (commit) => {
      const p = pending.current
      if (p) { clearTimeout(p.timer); pending.current = null }
      const d = live.current
      if (!d) return
      live.current = null
      cancelAnimationFrame(raf)
      document.body.style.userSelect = ''
      document.body.style.cursor = ''
      setDrag(null)
      setTimeout(() => { dragging.current = false }, 0)
      if (!commit) return
      if (d.kind === 'column') {
        const now = cols.findIndex((c) => c.id === d.id)
        if (d.overIndex !== now) onMoveColumn(d.id, d.overIndex)
      } else if (d.overCol) {
        const idx = realIndex(d.overCol, d.overIndex, d.id)
        const card = data.cards.find((c) => c.id === d.id)
        const now = byCol[card.columnId].findIndex((c) => c.id === d.id)
        if (d.overCol !== card.columnId || idx !== now) onMoveCard(d.id, d.overCol, idx)
      }
    }
    const up = () => finish(true)
    const cancel = () => finish(false)
    const key = (e) => { if (e.key === 'Escape' && live.current) { e.preventDefault(); finish(false) } }
    // Once a card is lifted on a touch screen, a finger moving is the card moving,
    // not the page scrolling.
    const noScroll = (e) => { if (live.current) e.preventDefault() }
    addEventListener('pointermove', move, { passive: false })
    addEventListener('pointerup', up)
    addEventListener('pointercancel', cancel)
    addEventListener('keydown', key)
    addEventListener('touchmove', noScroll, { passive: false })
    if (live.current) raf = requestAnimationFrame(tick)
    return () => {
      cancelAnimationFrame(raf)
      removeEventListener('pointermove', move)
      removeEventListener('pointerup', up)
      removeEventListener('pointercancel', cancel)
      removeEventListener('keydown', key)
      removeEventListener('touchmove', noScroll)
    }
  })

  const startPress = (kind, id) => (e) => {
    if (e.button !== 0 || e.target.closest('button, input, textarea, select, a, [data-no-drag]')) return
    const el = e.currentTarget
    const p = { kind, id, el, sx: e.clientX, sy: e.clientY, touch: e.pointerType === 'touch' }
    // A mouse press here is the start of a drag, not of a text selection across the
    // board. (Keyboard focus still comes by Tab; a click still opens the card.)
    if (!p.touch) e.preventDefault()
    if (p.touch) {
      p.timer = setTimeout(() => {
        if (pending.current !== p) return
        pending.current = null
        navigator.vibrate?.(10)
        begin(p, p.sx, p.sy)
      }, HOLD_MS)
    }
    pending.current = p
  }

  // ---- keyboard moving
  const kbdKey = (c) => (e) => {
    if (kbd && kbd.id === c.id) {
      const ci = cols.findIndex((x) => x.id === kbd.col)
      const len = byCol[kbd.col].filter((x) => x.id !== c.id).length
      let next = null
      if (e.key === 'ArrowUp') next = { ...kbd, index: Math.max(0, kbd.index - 1) }
      else if (e.key === 'ArrowDown') next = { ...kbd, index: Math.min(len, kbd.index + 1) }
      else if (e.key === 'ArrowLeft' && ci > 0) next = { ...kbd, col: cols[ci - 1].id, index: Math.min(kbd.index, byCol[cols[ci - 1].id].filter((x) => x.id !== c.id).length) }
      else if (e.key === 'ArrowRight' && ci < cols.length - 1) next = { ...kbd, col: cols[ci + 1].id, index: Math.min(kbd.index, byCol[cols[ci + 1].id].filter((x) => x.id !== c.id).length) }
      else if (e.key === ' ' || e.key === 'Enter') {
        e.preventDefault()
        setKbd(null)
        const now = byCol[c.columnId].findIndex((x) => x.id === c.id)
        if (kbd.col !== c.columnId || kbd.index !== now) onMoveCard(c.id, kbd.col, kbd.index)
        setAnnounce(`Dropped in ${cols.find((x) => x.id === kbd.col)?.name}, position ${kbd.index + 1}.`)
        return
      } else if (e.key === 'Escape') { e.preventDefault(); setKbd(null); setAnnounce('Move cancelled.'); return }
      if (next) {
        e.preventDefault()
        setKbd(next)
        setAnnounce(`${cols.find((x) => x.id === next.col)?.name}, position ${next.index + 1}.`)
      }
      return
    }
    if (e.key === ' ' && !filtering) {
      e.preventDefault()
      const index = byCol[c.columnId].findIndex((x) => x.id === c.id)
      setKbd({ id: c.id, col: c.columnId, index })
      setAnnounce(`Picked up “${c.title}”. Arrow keys move it, Space drops it, Escape cancels.`)
    } else if (e.key === 'Enter') {
      e.preventDefault()
      onOpenCard(c.id)
    }
  }
  // Keep focus on a card the keyboard is moving, wherever it is redrawn.
  useEffect(() => {
    if (kbd) document.querySelector(`[data-card-id="${kbd.id}"]`)?.focus()
  }, [kbd])

  const draggedCard = drag?.kind === 'card' ? data.cards.find((c) => c.id === drag.id) : null
  const draggedCol = drag?.kind === 'column' ? cols.find((c) => c.id === drag.id) : null

  // The columns as drawn: while a column is dragged, a gap where it will land.
  const shownCols = []
  const rest = cols.filter((c) => c.id !== draggedCol?.id)
  rest.forEach((c, i) => {
    if (draggedCol && drag.overIndex === i) shownCols.push({ gap: true })
    shownCols.push(c)
  })
  if (draggedCol && drag.overIndex >= rest.length) shownCols.push({ gap: true })

  return (
    <div ref={scroller} className="flex min-h-0 flex-1 items-start gap-3 overflow-x-auto pb-2">
      <div className="sr-only" aria-live="assertive">{announce}</div>
      {shownCols.map((c, i) => (c.gap
        ? <div key={`gap${i}`} className="h-40 shrink-0 rounded-xl border-2 border-dashed border-primary/40 bg-primary/5" style={{ width: drag.w }} />
        : (
          <Column key={c.id} col={c} cards={byCol[c.id]} matches={filtering ? matches : null} personOf={personOf}
            setColEl={(el) => (el ? colEls.current.set(c.id, el) : colEls.current.delete(c.id))}
            setBodyEl={(el) => (el ? bodyEls.current.set(c.id, el) : bodyEls.current.delete(c.id))}
            drag={drag} kbd={kbd} kbdCard={kbd ? data.cards.find((x) => x.id === kbd.id) : null} onHeaderPress={startPress('column', c.id)} onCardPress={(id) => startPress('card', id)}
            onCardKey={kbdKey} onOpenCard={(id) => { if (!dragging.current) onOpenCard(id) }} onAddCard={onAddCard}
            onRename={(name) => onRenameColumn(c, name)} onWip={() => onWip(c)} onDelete={() => onDeleteColumn(c)}
            onColor={(color) => onColorColumn(c, color)} onColorCard={onColorCard} onDeleteCard={onDeleteCard}
            onMoveCard={(card, top) => onMoveCard(card.id, c.id, top ? 0 : byCol[c.id].length)} />
        )))}
      {drag && createPortal(
        <div ref={ghost} className="pointer-events-none fixed left-0 top-0 z-[100] opacity-95" style={{ width: drag.w }}>
          {draggedCard && <CardFace card={draggedCard} person={personOf[draggedCard.assigneeId]} lifted />}
          {draggedCol && (
            <div className="rounded-xl border-2 border-primary/60 bg-surface p-3 shadow-2xl">
              <div className="text-sm font-semibold">{draggedCol.name}</div>
              <div className="text-xs text-muted">{byCol[draggedCol.id].length} cards</div>
            </div>
          )}
        </div>,
        document.body,
      )}
    </div>
  )
}

function Column({ col, cards, matches, personOf, setColEl, setBodyEl, drag, kbd, kbdCard, onHeaderPress, onCardPress, onCardKey, onOpenCard, onAddCard, onRename, onWip, onDelete, onColor, onColorCard, onDeleteCard, onMoveCard }) {
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(col.name)
  const [menu, setMenu] = useState(null)
  const [cardMenu, setCardMenu] = useState(null) // { x, y, card }
  const [adding, setAdding] = useState(null) // 'top' | 'bottom'
  useEffect(() => { if (!editing) setName(col.name) }, [col.name, editing])
  const over = col.wipLimit > 0 && cards.length > col.wipLimit

  // The cards as drawn: the one being dragged (or keyboard-moved) leaves its place,
  // and a gap — or the card itself, for the keyboard — shows where it will land.
  const draggingCard = drag?.kind === 'card'
  let shown = cards.filter((c) => !(draggingCard && c.id === drag.id) && !(kbd && c.id === kbd.id))
  if (matches) shown = shown.filter(matches)
  const items = shown.map((c) => ({ card: c }))
  if (draggingCard && drag.overCol === col.id) items.splice(Math.min(drag.overIndex, items.length), 0, { gap: drag.h })
  if (kbd && kbd.col === col.id && kbdCard) items.splice(Math.min(kbd.index, items.length), 0, { card: kbdCard, lifted: true })

  const commitName = () => {
    setEditing(false)
    const n = name.trim()
    if (n && n !== col.name) onRename(n)
    else setName(col.name)
  }

  return (
    <div ref={setColEl} className={`flex max-h-full w-72 shrink-0 flex-col rounded-xl border ${col.color ? '' : 'bg-surface2/60'} ${drag?.kind === 'card' && drag.overCol === col.id ? 'ring-2 ring-primary/40' : ''}`}
      style={col.color ? { background: tint(col.color, 14, 'var(--surface2)'), borderTop: `3px solid ${labelHex(col.color)}` } : undefined}>
      <div onPointerDown={onHeaderPress} onDoubleClick={() => setEditing(true)}
        className="flex cursor-grab items-center gap-2 px-3 pb-1.5 pt-2.5 active:cursor-grabbing" title="Drag to move the column · double-click to rename">
        {editing ? (
          <input autoFocus value={name} onChange={(e) => setName(e.target.value)} onBlur={commitName} maxLength={120}
            onKeyDown={(e) => { if (e.key === 'Enter') commitName(); if (e.key === 'Escape') { setName(col.name); setEditing(false) } }}
            className={`${inputCls} h-7 flex-1 text-sm font-semibold`} />
        ) : (
          <span className="min-w-0 flex-1 truncate text-sm font-semibold">{col.name}</span>
        )}
        <span className={`rounded-full px-1.5 text-[11px] tabular-nums ${over ? 'bg-danger/15 text-danger' : 'bg-surface text-muted'}`}
          title={col.wipLimit ? `${cards.length} of a limit of ${col.wipLimit}` : `${cards.length} cards`}>
          {cards.length}{col.wipLimit ? `/${col.wipLimit}` : ''}
        </span>
        <button onClick={(e) => { const r = e.currentTarget.getBoundingClientRect(); setMenu({ x: r.left - 140, y: r.bottom + 4 }) }}
          className="rounded px-1 text-muted hover:bg-surface hover:text-fg" title="Column actions">⋯</button>
      </div>
      <div ref={setBodyEl} className="min-h-[3rem] flex-1 space-y-2 overflow-y-auto px-2 pb-1 pt-0.5">
        {adding === 'top' && <Composer onAdd={(t) => onAddCard(col.id, t, true)} onClose={() => setAdding(null)} />}
        {items.map((it, i) => (it.gap
          ? <div key={`gap${i}`} className="rounded-lg border-2 border-dashed border-primary/50 bg-primary/5" style={{ height: it.gap }} />
          : it.card ? (
            <div key={it.card.id} data-card-id={it.card.id} tabIndex={0}
              onPointerDown={onCardPress(it.card.id)} onKeyDown={onCardKey(it.card)}
              onClick={() => onOpenCard(it.card.id)}
              onContextMenu={(e) => { e.preventDefault(); setCardMenu({ x: e.clientX, y: e.clientY, card: it.card }) }}
              className="rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-primary"
              style={{ touchAction: 'manipulation' }}
              aria-label={`${it.card.title}. Space to move, Enter to open.`}>
              <CardFace card={it.card} person={personOf[it.card.assigneeId]} lifted={it.lifted} />
            </div>
          ) : null))}
        {!matches && cards.length === 0 && !draggingCard && adding !== 'bottom' && (
          <div className="rounded-lg border border-dashed px-3 py-4 text-center text-xs text-muted">Drop cards here</div>
        )}
        {adding === 'bottom' && <Composer onAdd={(t) => onAddCard(col.id, t, false)} onClose={() => setAdding(null)} />}
      </div>
      {adding !== 'bottom' && (
        <button onClick={() => setAdding('bottom')} className="m-1.5 flex items-center gap-1.5 rounded-md px-2 py-1.5 text-left text-xs text-muted hover:bg-surface hover:text-fg">
          <Icon.Plus size={13} /> Add a card
        </button>
      )}
      {menu && (
        <Menu at={menu} onClose={() => setMenu(null)} items={[
          { label: 'Add a card at the top', onClick: () => setAdding('top') },
          { label: 'Rename', onClick: () => setEditing(true) },
          { label: col.wipLimit ? `Work-in-progress limit (${col.wipLimit})…` : 'Set a work-in-progress limit…', onClick: onWip },
          { swatches: true, label: 'Column colour', value: col.color, onPick: onColor },
          { sep: true },
          { label: 'Delete column', danger: true, onClick: onDelete },
        ]} />
      )}
      {cardMenu && (
        <Menu at={cardMenu} onClose={() => setCardMenu(null)} items={[
          { label: 'Open', onClick: () => onOpenCard(cardMenu.card.id) },
          { label: 'Move to the top', onClick: () => onMoveCard(cardMenu.card, true) },
          { label: 'Move to the bottom', onClick: () => onMoveCard(cardMenu.card, false) },
          { swatches: true, label: 'Card colour', value: cardMenu.card.color, onPick: (c) => onColorCard(cardMenu.card, c) },
          { sep: true },
          { label: 'Delete card', danger: true, onClick: () => onDeleteCard(cardMenu.card) },
        ]} />
      )}
    </div>
  )
}

// Composer is the quick way in: type, Enter, type the next. Shift+Enter is a new
// line, Escape (or an empty Enter) closes it.
function Composer({ onAdd, onClose }) {
  const [t, setT] = useState('')
  const ref = useRef(null)
  useEffect(() => { ref.current?.focus() }, [])
  const add = () => {
    const v = t.trim()
    if (!v) { onClose(); return }
    onAdd(v)
    setT('')
    ref.current?.focus()
  }
  return (
    <div className="rounded-lg border bg-surface p-2 shadow-sm" data-no-drag>
      <textarea ref={ref} rows={2} value={t} onChange={(e) => setT(e.target.value)} placeholder="What needs doing?"
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); add() }
          if (e.key === 'Escape') onClose()
        }}
        className="w-full resize-none bg-transparent text-sm outline-none" maxLength={300} />
      <div className="mt-1 flex items-center gap-2">
        <Button size="sm" onClick={add}>Add card</Button>
        <button onClick={onClose} className="text-xs text-muted hover:text-fg">Cancel</button>
        <span className="ml-auto text-[10px] text-muted">Enter adds · Esc closes</span>
      </div>
    </div>
  )
}

function CardFace({ card, person, lifted }) {
  const due = dueInfo(card.due)
  return (
    <div className={`cursor-pointer select-none rounded-lg border bg-surface p-2.5 text-sm transition-shadow ${lifted ? 'border-primary/60 shadow-2xl ring-2 ring-primary/40' : 'shadow-sm hover:border-primary/40 hover:shadow'}`}
      style={card.color ? { background: tint(card.color, 18), borderLeft: `4px solid ${labelHex(card.color)}` } : undefined}>
      {card.labels?.length > 0 && (
        <div className="mb-1.5 flex flex-wrap gap-1">
          {card.labels.map((l, i) => (
            <span key={i} className="rounded px-1.5 py-px text-[10px] font-medium text-white" style={{ background: labelHex(l.color) }}>
              {l.text || '   '}
            </span>
          ))}
        </div>
      )}
      <div className="whitespace-pre-wrap break-words leading-snug">{card.title}</div>
      {(due || card.description || person) && (
        <div className="mt-2 flex items-center gap-2 text-[11px] text-muted">
          {due && <span className={`rounded px-1.5 py-px ${due.tone}`} title={due.title}>{due.label}</span>}
          {card.description && <span title="Has a description"><Icon.Logs size={12} /></span>}
          {person && <span className="ml-auto"><Avatar avatar={person.avatar} name={person.name} size={20} /></span>}
        </div>
      )}
    </div>
  )
}

function CardDialog({ card, columns, people, onClose, onSave, onDelete, onMove }) {
  const [c, setC] = useState(() => ({ ...card, labels: card.labels || [] }))
  const dirty = JSON.stringify([c.title, c.description, c.labels, c.assigneeId, c.due, c.color || '']) !== JSON.stringify([card.title, card.description, card.labels || [], card.assigneeId, card.due, card.color || ''])
  const save = () => { if (c.title.trim()) { onSave({ ...c, title: c.title.trim() }); onClose() } }
  const close = () => { if (dirty && c.title.trim()) onSave({ ...c, title: c.title.trim() }); onClose() }
  const setLabel = (i, patch) => setC((x) => ({ ...x, labels: x.labels.map((l, n) => (n === i ? { ...l, ...patch } : l)) }))
  const creator = people.find((p) => p.id === card.createdBy)
  return createPortal(
    <div className="fixed inset-0 z-[90] flex items-start justify-center overflow-y-auto bg-black/40 p-4 pt-[8vh]" onMouseDown={close}>
      <div onMouseDown={(e) => e.stopPropagation()}
        onKeyDown={(e) => { if (e.key === 'Escape') close(); if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) save() }}
        className="w-full max-w-2xl rounded-2xl border bg-surface p-5 shadow-2xl">
        <div className="flex items-start gap-3">
          <textarea value={c.title} onChange={(e) => setC({ ...c, title: e.target.value })} rows={2} maxLength={300} autoFocus
            className="min-w-0 flex-1 resize-none rounded-md bg-transparent px-1 text-lg font-semibold outline-none focus:bg-bg" />
          <button onClick={close} className="rounded p-1 text-muted hover:bg-surface2" title="Close (saves)"><Icon.Close size={16} /></button>
        </div>
        <div className="mt-1 flex items-center gap-2 px-1 text-xs text-muted">
          in
          <select value={card.columnId} onChange={(e) => onMove(Number(e.target.value))} className="rounded border bg-bg px-1.5 py-0.5 text-xs text-fg">
            {columns.map((col) => <option key={col.id} value={col.id}>{col.name}</option>)}
          </select>
          {creator && <span>· added by {creator.name}</span>}
        </div>

        <div className="mt-4 grid gap-4 sm:grid-cols-[1fr_200px]">
          <div className="space-y-1">
            <div className="text-xs font-medium text-muted">Description</div>
            <textarea value={c.description} onChange={(e) => setC({ ...c, description: e.target.value })} rows={9}
              placeholder="Details, steps, links…" className={`${inputCls} resize-y text-sm`} />
          </div>
          <div className="space-y-4">
            <div className="space-y-1">
              <div className="text-xs font-medium text-muted">Assignee</div>
              <select value={c.assigneeId || 0} onChange={(e) => setC({ ...c, assigneeId: Number(e.target.value) })} className={`${inputCls} text-sm`}>
                <option value={0}>Nobody</option>
                {people.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
              </select>
            </div>
            <div className="space-y-1">
              <div className="text-xs font-medium text-muted">Due</div>
              <div className="flex gap-1">
                <input type="date" value={c.due} onChange={(e) => setC({ ...c, due: e.target.value })} className={`${inputCls} text-sm`} />
                {c.due && <button onClick={() => setC({ ...c, due: '' })} className="rounded px-1.5 text-muted hover:bg-surface2" title="Clear">✕</button>}
              </div>
            </div>
            <div className="space-y-1.5">
              <div className="text-xs font-medium text-muted">Card colour</div>
              <Swatches value={c.color || ''} onPick={(color) => setC({ ...c, color })} size={20} />
            </div>
            <div className="space-y-1.5">
              <div className="text-xs font-medium text-muted">Labels</div>
              {c.labels.map((l, i) => (
                <div key={i} className="flex items-center gap-1">
                  <span className="h-5 w-5 shrink-0 rounded" style={{ background: labelHex(l.color) }} />
                  <input value={l.text} onChange={(e) => setLabel(i, { text: e.target.value })} maxLength={32} placeholder="Label"
                    className={`${inputCls} h-7 min-w-0 flex-1 text-xs`} />
                  <button onClick={() => setC({ ...c, labels: c.labels.filter((_, n) => n !== i) })} className="rounded px-1 text-muted hover:text-danger">✕</button>
                </div>
              ))}
              {c.labels.length < 8 && (
                <div className="flex flex-wrap gap-1">
                  {LABEL_COLORS.map((col) => (
                    <button key={col.id} title={`Add a ${col.id} label`} onClick={() => setC({ ...c, labels: [...c.labels, { color: col.id, text: '' }] })}
                      className="h-5 w-5 rounded opacity-80 ring-offset-1 hover:opacity-100 hover:ring-2" style={{ background: col.hex }} />
                  ))}
                </div>
              )}
            </div>
          </div>
        </div>

        <div className="mt-5 flex items-center gap-2 border-t pt-3">
          <Button variant="danger" size="sm" onClick={onDelete}><Icon.Trash size={14} /> Delete</Button>
          <span className="ml-auto text-[11px] text-muted">{dirty ? 'Closing saves · ⌘/Ctrl+Enter' : 'Saved'}</span>
          <Button size="sm" onClick={save} disabled={!c.title.trim()}>Done</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}
