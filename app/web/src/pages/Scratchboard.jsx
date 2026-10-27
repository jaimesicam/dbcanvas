// Scratchboard.jsx — canvases for notes, conversions and inspecting what you paste.
//
// A person has as many boards as they like, one tab each. By default nothing on them is saved:
// the boards live in this component's React state and nowhere else.
//
//   - no localStorage, sessionStorage or IndexedDB; everything is parsed and converted in this
//     page (components/scratch/Cards.jsx, lib/scratch/*);
//   - with the account's "Keep Scratchboard boards" preference on (Settings, off by default), a
//     board can be marked to be kept: only those are sent to the server, through
//     lib/scratchboardApi.js, and stored encrypted with the account (app/scratchboard.go). The
//     others stay temporary. Turning the preference off deletes every kept board;
//   - a board can be exported to a file, or its text to Markdown, only when its owner asks;
//   - other pages hand content over through lib/scratch/bus.js, in memory, never sendHandoff
//     (which writes to sessionStorage);
//   - useFollowedState is not used for card content, so a shared session does not sync it;
//   - every card body carries data-mirror-private, which the shared-session mirror blocks
//     (session/Mirror.jsx), until that card's "Show to session" is turned on;
//   - a session recording is a video of the tab and cannot be masked, so while one runs the board
//     says so.
//
// Closing the window or reloading the browser drops every temporary board; minimizing keeps them.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Button, ConfirmButton } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { Help } from '../components/Tooltip.jsx'
import { HELP } from '../lib/help.js'
import { zoomAt, clamp } from '../lib/canvas.js'
import { suggest } from '../lib/scratch/detect.js'
import { subscribeScratchboard } from '../lib/scratch/bus.js'
import { exportBoard, importBoard, boardMarkdown, searchBoards } from '../lib/scratch/boardfile.js'
import { scratchboardApi } from '../lib/scratchboardApi.js'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { recorder } from '../session/recorder.js'
import { usePageVisible } from '../lib/usePolling.jsx'
import { CARD_TYPES, CARD_FAMILIES } from '../components/scratch/Cards.jsx'

let seq = 0
const newId = (p = 'c') => `${p}${Date.now().toString(36)}${(seq++).toString(36)}`

const newBoard = (name) => ({ id: newId('b'), name, keep: false, cards: [], links: [], view: { x: 0, y: 0, z: 1 } })
const LABELS = Object.fromEntries(Object.entries(CARD_TYPES).map(([k, t]) => [k, t.label]))
const KNOWN = new Set(Object.keys(CARD_TYPES))
const topZ = (cards) => cards.reduce((m, c) => Math.max(m, c.z || 0), 0)
const inField = (t) => t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)

function download(name, text, type) {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
const fileName = (name, ext) => `${String(name || 'board').replace(/[^\w.-]+/g, '-').replace(/^-+|-+$/g, '') || 'board'}.${ext}`

// Card and font colours. A card colour alone picks a readable font for itself (inkFor); a font
// colour overrides that.
export const CARD_COLORS = ['#fef3c7', '#dcfce7', '#dbeafe', '#fce7f3', '#ede9fe', '#ffedd5', '#e5e7eb', '#1e293b', '#172554', '#14532d', '#4c0519']
export const FONT_COLORS = ['#111827', '#f9fafb', '#dc2626', '#ea580c', '#16a34a', '#2563eb', '#7c3aed']

function inkFor(hex) {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex || '')
  if (!m) return undefined
  const n = parseInt(m[1], 16)
  const lum = (0.299 * ((n >> 16) & 255) + 0.587 * ((n >> 8) & 255) + 0.114 * (n & 255)) / 255
  return lum > 0.6 ? '#111827' : '#f3f4f6'
}

// cardStyle recolours a card by overriding the theme's colour variables on it, so everything
// inside — fields, outputs, muted text — follows without each card knowing.
export function cardStyle(card) {
  const s = {}
  const fg = card.fg || (card.color ? inkFor(card.color) : undefined)
  if (card.color) {
    s['--surface'] = card.color
    s['--surface2'] = `color-mix(in srgb, ${card.color} 86%, #808080)`
    s['--bg'] = `color-mix(in srgb, ${card.color} 80%, ${fg || '#808080'} 6%)`
    s['--border'] = `color-mix(in srgb, ${card.color} 65%, #808080)`
  }
  if (fg) {
    s['--fg'] = fg
    s['--muted'] = `color-mix(in srgb, ${fg} 65%, transparent)`
  }
  return s
}

function Swatches({ label, colors, value, onPick }) {
  return (
    <div className="space-y-1">
      <div className="text-[11px] text-muted">{label}</div>
      <div className="flex flex-wrap items-center gap-1">
        <button className={`h-5 w-5 rounded border text-[10px] leading-none text-muted ${!value ? 'ring-2 ring-primary' : ''}`} title="Theme default" onClick={() => onPick('')}>×</button>
        {colors.map((c) => (
          <button key={c} className={`h-5 w-5 rounded border ${value === c ? 'ring-2 ring-primary' : ''}`} style={{ background: c }} title={c} onClick={() => onPick(c)} />
        ))}
        <input type="color" className="h-5 w-6 cursor-pointer rounded border bg-transparent p-0" title="Any colour"
          value={value || '#888888'} onChange={(e) => onPick(e.target.value)} />
      </div>
    </div>
  )
}

// ColorMenu is a card's colour picker: its background and its font.
export function ColorMenu({ card, update, onClose }) {
  return (
    <div className="absolute right-1 top-7 z-20 w-56 space-y-2 rounded-lg border bg-surface p-2 text-xs text-fg shadow-xl" onPointerDown={(e) => e.stopPropagation()}>
      <Swatches label="Card colour" colors={CARD_COLORS} value={card.color || ''} onPick={(color) => update({ color }, true)} />
      <Swatches label="Font colour" colors={FONT_COLORS} value={card.fg || ''} onPick={(fg) => update({ fg }, true)} />
      <div className="flex justify-end">
        <button className="rounded px-1.5 py-0.5 text-muted hover:text-fg" onClick={onClose}>Done</button>
      </div>
    </div>
  )
}

// CardFrame is one card: a title bar to drag it by, its controls, the body, a resize corner.
// Until it is resized by hand a card is as tall as its content; after, its body scrolls.
export function CardFrame({ card, highlight, onPointerDownMove, onPointerDownResize, onFitHeight, onLinkStart, onFocus, update, close, duplicate, spawn, onSelectText }) {
  const [colors, setColors] = useState(false)
  const T = CARD_TYPES[card.type]
  const I = Icon[T.icon] || Icon.Text
  const Body = T.Body
  const stop = (e) => e.stopPropagation()
  return (
    <div data-card-id={card.id} className={`absolute flex flex-col rounded-lg border bg-surface text-fg shadow-lg ${highlight ? 'ring-2 ring-primary' : ''}`}
      style={{ left: card.x, top: card.y, width: card.w, minHeight: card.collapsed ? 0 : 120, zIndex: card.z, ...cardStyle(card) }}
      onPointerDown={onFocus}>
      <div className={`flex cursor-move select-none items-center gap-1.5 rounded-t-lg ${card.collapsed ? 'rounded-b-lg' : 'border-b'} bg-surface2 px-2 py-1 text-xs`}
        onPointerDown={onPointerDownMove} onDoubleClick={() => update({ collapsed: !card.collapsed }, true)}>
        <button className="rounded text-muted hover:text-fg" title={card.collapsed ? 'Expand' : 'Collapse to the title bar'} onPointerDown={stop}
          onClick={() => update({ collapsed: !card.collapsed }, true)}>
          <Icon.Chevron size={12} style={{ transform: card.collapsed ? 'rotate(-90deg)' : undefined }} />
        </button>
        <I size={13} />
        <span className="min-w-0 flex-1 truncate font-medium text-fg">{card.title || T.label}</span>
        <button className={`rounded p-0.5 ${card.shared ? 'text-warning' : 'text-muted'} hover:text-fg`} onPointerDown={stop}
          title={card.shared ? 'Shown to session guests — click to hide it again' : 'Hidden from session guests — click to show this card to them'}
          onClick={() => update({ shared: !card.shared }, true)}>
          {card.shared ? <Icon.Eye size={13} /> : <Icon.EyeOff size={13} />}
        </button>
        <button className="rounded p-0.5 text-muted hover:text-fg" title="Card and font colour" onPointerDown={stop} onClick={() => setColors((o) => !o)}><Icon.Palette size={13} /></button>
        <button className="rounded p-0.5 text-muted hover:text-fg" title="Duplicate" onPointerDown={stop} onClick={duplicate}><Icon.Copy size={13} /></button>
        <button className="rounded p-0.5 text-muted hover:text-danger" title="Close (Undo brings it back)" onPointerDown={stop} onClick={close}><Icon.Close size={13} /></button>
      </div>
      {colors && <ColorMenu card={card} update={update} onClose={() => setColors(false)} />}
      {!card.collapsed && (
        /* The mirror blocks [data-mirror-private] — the card's content, never its frame. */
        <div className="flex-1 overflow-auto rounded-b-lg p-2" style={card.sized ? { maxHeight: card.h } : undefined} {...(card.shared ? {} : { 'data-mirror-private': '' })}>
          <Body state={card.state} update={(patch) => update(patch)} spawn={spawn} onSelectText={onSelectText} />
        </div>
      )}
      {/* Drag from here onto another card to draw an arrow to it. */}
      <div className="absolute -right-1.5 top-2.5 h-3 w-3 cursor-crosshair rounded-full border-2 border-primary bg-surface opacity-40 hover:opacity-100"
        title="Drag onto another card to draw an arrow to it" onPointerDown={onLinkStart} />
      {!card.collapsed && (
        <div className="absolute bottom-0 right-0 h-3 w-3 cursor-nwse-resize" onPointerDown={onPointerDownResize} onDoubleClick={onFitHeight}
          title="Resize — double-click to fit the content again" />
      )}
    </div>
  )
}

// anchor is where an arrow meets a card: the middle of its title bar's left or right edge.
const anchor = (c, side) => ({ x: side === 'right' ? c.x + c.w : c.x, y: c.y + 14 })
function arrowPath(a, b) {
  const right = b.x >= a.x + a.w / 2
  const p = anchor(a, right ? 'right' : 'left')
  const q = anchor(b, right ? 'left' : 'right')
  const dx = Math.max(40, Math.abs(q.x - p.x) / 2) * (right ? 1 : -1)
  return `M ${p.x} ${p.y} C ${p.x + dx} ${p.y}, ${q.x - dx} ${q.y}, ${q.x} ${q.y}`
}

// LinkLayer draws the arrows between cards, under them; clicking one removes it.
export function LinkLayer({ cards, links, drawing, onRemove }) {
  const by = new Map(cards.map((c) => [c.id, c]))
  return (
    <svg className="pointer-events-none absolute left-0 top-0 overflow-visible" width="1" height="1" style={{ zIndex: 0 }}>
      <defs>
        <marker id="sb-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M0 0 L10 5 L0 10 z" fill="var(--primary)" />
        </marker>
      </defs>
      {(links || []).map((l) => {
        const a = by.get(l.from)
        const b = by.get(l.to)
        if (!a || !b) return null
        const d = arrowPath(a, b)
        return (
          <g key={l.id}>
            <path d={d} fill="none" stroke="var(--primary)" strokeOpacity="0.55" strokeWidth="1.5" markerEnd="url(#sb-arrow)" />
            <path d={d} fill="none" stroke="transparent" strokeWidth="12" className="pointer-events-auto cursor-pointer"
              onPointerDown={(e) => e.stopPropagation()} onClick={() => onRemove(l.id)}><title>Click to remove this arrow</title></path>
          </g>
        )
      })}
      {drawing && (() => {
        const a = by.get(drawing.from)
        if (!a) return null
        const p = anchor(a, 'right')
        return <path d={`M ${p.x} ${p.y} L ${drawing.x} ${drawing.y}`} fill="none" stroke="var(--primary)" strokeDasharray="4 3" strokeWidth="1.5" markerEnd="url(#sb-arrow)" />
      })()}
    </svg>
  )
}

// SuggestMenu offers the cards that fit a piece of text, best first.
export function SuggestMenu({ text, x, y, onPick, onClose, title = 'Open as' }) {
  const list = suggest(text)
  return (
    <div className="fixed z-[100] min-w-[12rem] rounded-lg border bg-surface p-1 text-xs shadow-xl" style={{ left: x, top: y }}
      onPointerDown={(e) => e.stopPropagation()}>
      <div className="flex items-center px-1.5 py-0.5 text-[11px] text-muted">
        <span className="flex-1">{title}</span>
        <button className="hover:text-fg" onClick={onClose}><Icon.Close size={11} /></button>
      </div>
      {list.map((s, i) => (
        <button key={s.type} className={`block w-full rounded px-2 py-1 text-left hover:bg-surface2 ${i === 0 ? 'font-medium text-fg' : 'text-fg'}`}
          onClick={() => onPick(s.type)}>{s.label}</button>
      ))}
    </div>
  )
}

// AddCardMenu lists every card by family: the "Add card" button's menu, and the board's
// right-click menu.
export function AddCardMenu({ onPick, className = '', style }) {
  const families = CARD_FAMILIES.map((fam) => [fam, Object.entries(CARD_TYPES).filter(([, t]) => t.family === fam)])
  return (
    <div className={`z-50 grid w-[30rem] grid-cols-3 gap-2 rounded-lg border bg-surface p-2 text-xs shadow-xl ${className}`} style={style}
      onPointerDown={(e) => e.stopPropagation()} onContextMenu={(e) => e.preventDefault()}>
      {families.map(([fam, list]) => (
        <div key={fam} className="space-y-0.5">
          <div className="px-1 text-[11px] font-semibold text-muted">{fam}</div>
          {list.map(([type, t]) => {
            const I = Icon[t.icon] || Icon.Text
            return (
              <button key={type} className="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left text-fg hover:bg-surface2"
                onClick={() => onPick(type)}>
                <I size={13} /> {t.label}
              </button>
            )
          })}
        </div>
      ))}
    </div>
  )
}

// BoardTab is one board's tab: its name (double-click to rename), whether it is kept, and close.
export function BoardTab({ board, active, canKeep, onSelect, onRename, onKeep, onRemove, removable }) {
  const [editing, setEditing] = useState(false)
  return (
    <div className={`group flex items-center gap-1 rounded-t-md border border-b-0 px-2 py-1 text-xs ${active ? 'bg-surface text-fg' : 'bg-surface2 text-muted hover:text-fg'}`}
      onClick={onSelect}>
      {canKeep && (
        <button className={`rounded p-0.5 ${board.keep ? 'text-primary' : 'text-muted'} hover:text-fg`}
          title={board.keep ? 'Kept — saved to your account, encrypted. Click to make it temporary again and delete the saved copy' : 'Temporary — erased when the browser reloads. Click to keep it'}
          onClick={(e) => { e.stopPropagation(); onKeep(!board.keep) }}>
          <Icon.Pin size={12} />
        </button>
      )}
      {editing ? (
        <input autoFocus className="w-28 rounded border bg-bg px-1 text-xs text-fg outline-none" defaultValue={board.name}
          onClick={(e) => e.stopPropagation()}
          onBlur={(e) => { onRename(e.target.value.trim() || board.name); setEditing(false) }}
          onKeyDown={(e) => { if (e.key === 'Enter') e.target.blur(); if (e.key === 'Escape') setEditing(false) }} />
      ) : (
        <span className="max-w-[10rem] truncate" title="Double-click to rename" onDoubleClick={(e) => { e.stopPropagation(); setEditing(true) }}>{board.name}</span>
      )}
      {removable && (
        <button className="rounded p-0.5 text-muted opacity-60 group-hover:opacity-100 hover:text-danger" title="Remove board (Undo brings it back)"
          onClick={(e) => { e.stopPropagation(); onRemove() }}>
          <Icon.Close size={11} />
        </button>
      )}
    </div>
  )
}

// BoardMenu is the board's file actions: export, Markdown, import.
function BoardMenu({ board, onImport, onCopied, onClose }) {
  const fileRef = useRef(null)
  const item = 'flex w-full items-center gap-1.5 rounded px-2 py-1 text-left text-fg hover:bg-surface2'
  return (
    <div className="absolute right-0 top-full z-50 mt-1 w-64 rounded-lg border bg-surface p-1 text-xs shadow-xl" onPointerDown={(e) => e.stopPropagation()}>
      <button className={item} title="Every card's content, Secrets included, goes into the file"
        onClick={() => { download(fileName(board.name, 'json'), exportBoard(board), 'application/json'); onClose() }}>
        <Icon.File size={13} /> Export board (.json)
      </button>
      <button className={item} onClick={() => { download(fileName(board.name, 'md'), boardMarkdown(board, LABELS), 'text/markdown'); onClose() }}>
        <Icon.Text size={13} /> Download as Markdown (.md)
      </button>
      <button className={item} onClick={() => { try { navigator.clipboard.writeText(boardMarkdown(board, LABELS)); onCopied() } catch { /* no clipboard permission */ } onClose() }}>
        <Icon.Copy size={13} /> Copy as Markdown
      </button>
      <button className={item} onClick={() => fileRef.current?.click()}>
        <Icon.Folder size={13} /> Import a board (.json)…
      </button>
      <input ref={fileRef} type="file" accept=".json,application/json" className="hidden"
        onChange={async (e) => { const f = e.target.files?.[0]; e.target.value = ''; if (f) onImport(await f.text()); onClose() }} />
      <div className="px-2 pb-1 pt-1.5 text-[10px] text-muted">An exported file holds everything on the board, Secrets included.</div>
    </div>
  )
}

// What is saved of a kept board: everything but its id, which is the key.
const boardDoc = (b) => ({ name: b.name, view: b.view, cards: b.cards, links: b.links || [] })
const fromDoc = (id, doc, keep) => ({
  id, keep, name: doc.name || 'Board',
  cards: Array.isArray(doc.cards) ? doc.cards.filter((c) => KNOWN.has(c.type)) : [],
  links: Array.isArray(doc.links) ? doc.links : [],
  view: doc.view || { x: 0, y: 0, z: 1 },
})

export default function Scratchboard() {
  const { settings } = useSettings()
  const canKeep = settings.scratchboard === 'on'
  const [boards, setBoards] = useState(() => [newBoard('Board 1')])
  const [activeId, setActiveId] = useState(() => boards[0].id)
  const [menu, setMenu] = useState(null) // { text, x, y, title, at, from }
  const [addMenu, setAddMenu] = useState(null) // { button } or { x, y, at } for a right-click
  const [fileMenu, setFileMenu] = useState(false)
  const [notice, setNotice] = useState('')
  const [saveErr, setSaveErr] = useState('')
  const [conflicts, setConflicts] = useState({}) // board id → { rev, board? }
  const [undo, setUndo] = useState([]) // [{ label, boardId, apply(boards) → boards }]
  const [query, setQuery] = useState('')
  const [flash, setFlash] = useState(null) // a card found by search, ringed for a moment
  const [drawing, setDrawing] = useState(null) // { from, x, y } while an arrow is dragged
  const [recording, setRecording] = useState(recorder.get().status !== 'idle')
  const wrapRef = useRef(null)
  const visible = usePageVisible()

  const board = boards.find((b) => b.id === activeId) || boards[0]
  const cards = board.cards
  const view = board.view

  useEffect(() => recorder.subscribe(() => setRecording(recorder.get().status !== 'idle')), [])
  useEffect(() => {
    if (!notice) return
    const t = setTimeout(() => setNotice(''), 2500)
    return () => clearTimeout(t)
  }, [notice])
  useEffect(() => {
    if (!flash) return
    const t = setTimeout(() => setFlash(null), 1800)
    return () => clearTimeout(t)
  }, [flash])

  // Changes to the active board. setCards and setView take a value or an updater.
  const patchBoard = useCallback((id, fn) => setBoards((bs) => bs.map((b) => (b.id === id ? fn(b) : b))), [])
  const setCards = useCallback((v) => patchBoard(board.id, (b) => ({ ...b, cards: typeof v === 'function' ? v(b.cards) : v })), [board.id, patchBoard])
  const setView = useCallback((v) => patchBoard(board.id, (b) => ({ ...b, view: typeof v === 'function' ? v(b.view) : v })), [board.id, patchBoard])

  // ---- undo ---------------------------------------------------------------------------------
  // What can be lost by a click is undoable: closing a card, clearing a board, removing a board,
  // removing an arrow. Each entry knows how to put back what it took.
  const pushUndo = (label, boardId, apply) => setUndo((u) => [...u.slice(-29), { label, boardId, apply }])
  const runUndo = useCallback(() => {
    const last = undo[undo.length - 1]
    if (!last) return
    setUndo(undo.slice(0, -1))
    setBoards(last.apply)
    setActiveId(last.boardId)
  }, [undo])

  const closeCard = useCallback((boardId, cardId) => {
    const b = boards.find((x) => x.id === boardId)
    const card = b?.cards.find((c) => c.id === cardId)
    if (!card) return
    const links = (b.links || []).filter((l) => l.from === cardId || l.to === cardId)
    patchBoard(boardId, (x) => ({ ...x, cards: x.cards.filter((c) => c.id !== cardId), links: (x.links || []).filter((l) => l.from !== cardId && l.to !== cardId) }))
    pushUndo(`close ${card.title || LABELS[card.type]}`, boardId, (bs) => bs.map((x) => (x.id === boardId ? { ...x, cards: [...x.cards, card], links: [...(x.links || []), ...links] } : x)))
  }, [boards, patchBoard])

  const clearBoard = () => {
    const { id, cards: old, links: oldLinks } = board
    patchBoard(id, (x) => ({ ...x, cards: [], links: [] }))
    pushUndo('clear board', id, (bs) => bs.map((x) => (x.id === id ? { ...x, cards: [...old, ...x.cards], links: [...(oldLinks || []), ...(x.links || [])] } : x)))
  }

  const removeLink = (linkId) => {
    const id = board.id
    const link = (board.links || []).find((l) => l.id === linkId)
    if (!link) return
    patchBoard(id, (x) => ({ ...x, links: x.links.filter((l) => l.id !== linkId) }))
    pushUndo('remove arrow', id, (bs) => bs.map((x) => (x.id === id ? { ...x, links: [...(x.links || []), link] } : x)))
  }
  const addLink = useCallback((boardId, from, to) => {
    if (!from || !to || from === to) return
    patchBoard(boardId, (x) => ((x.links || []).some((l) => l.from === from && l.to === to) ? x : { ...x, links: [...(x.links || []), { id: newId('l'), from, to }] }))
  }, [patchBoard])

  // ---- kept boards -----------------------------------------------------------------------
  // Loaded once the preference is known to be on; an untouched first board makes way for them.
  // lastSaved holds what the server has of each kept board, so only a real change is sent, and
  // revs the rev it is at, so a save from a stale tab is refused rather than overwriting.
  const lastSaved = useRef(new Map())
  const revs = useRef(new Map())
  const keptIds = useRef(new Set())
  keptIds.current = new Set(boards.filter((b) => b.keep).map((b) => b.id))

  useEffect(() => {
    if (!canKeep) {
      // Switched off (the server has deleted them): every board is temporary again.
      lastSaved.current.clear()
      revs.current.clear()
      setConflicts({})
      setBoards((bs) => (bs.some((b) => b.keep) ? bs.map((b) => ({ ...b, keep: false })) : bs))
      return
    }
    let stale = false
    scratchboardApi.list().then((res) => {
      if (stale || !res?.boards?.length) return
      const kept = res.boards.map((d) => {
        const doc = d.data || {}
        lastSaved.current.set(d.id, JSON.stringify(doc))
        revs.current.set(d.id, d.rev || 0)
        return fromDoc(d.id, doc, true)
      })
      setBoards((bs) => {
        const have = new Set(bs.map((b) => b.id))
        const rest = bs.filter((b) => b.keep || b.cards.length || bs.length > 1)
        const next = [...rest, ...kept.filter((k) => !have.has(k.id))]
        if (!rest.length) setActiveId(next[0].id)
        return next
      })
    }).catch((e) => { if (!stale) setSaveErr(`Could not load your kept boards: ${e.message}`) })
    return () => { stale = true }
  }, [canKeep])

  // Kept boards are saved a moment after they stop changing. A board in conflict waits for its
  // owner to choose a version.
  useEffect(() => {
    if (!canKeep) return
    const dirty = boards.filter((b) => b.keep && !conflicts[b.id] && lastSaved.current.get(b.id) !== JSON.stringify(boardDoc(b)))
    if (!dirty.length) return
    const t = setTimeout(() => {
      for (const b of dirty) {
        const doc = boardDoc(b)
        scratchboardApi.save(b.id, doc, revs.current.get(b.id) || 0)
          .then((r) => {
            // Unpinned while this save was on its way: the copy it just wrote goes too.
            if (!keptIds.current.has(b.id)) {
              scratchboardApi.remove(b.id).catch(() => {})
              return
            }
            lastSaved.current.set(b.id, JSON.stringify(doc))
            revs.current.set(b.id, r?.rev || 0)
            setSaveErr('')
          })
          .catch((e) => {
            if (e.status === 409 && e.body?.conflict) setConflicts((c) => ({ ...c, [b.id]: { rev: e.body.rev || 0, board: e.body.board } }))
            else setSaveErr(`“${b.name}” was not saved: ${e.message}`)
          })
      }
    }, 800)
    return () => clearTimeout(t)
  }, [boards, canKeep, conflicts])

  const resolveConflict = (id, take) => {
    const c = conflicts[id]
    if (!c) return
    revs.current.set(id, c.rev)
    if (take === 'theirs' && c.board) {
      const doc = c.board.data || {}
      lastSaved.current.set(id, JSON.stringify(doc))
      patchBoard(id, (b) => fromDoc(id, doc, true))
    } else if (take === 'temporary') {
      lastSaved.current.delete(id)
      revs.current.delete(id)
      patchBoard(id, (b) => ({ ...b, keep: false }))
    } else {
      lastSaved.current.delete(id) // ours is saved again on the next pass
    }
    setConflicts(({ [id]: _, ...rest }) => rest)
  }

  const forget = (id) => {
    if (!lastSaved.current.has(id) && !revs.current.has(id)) return
    lastSaved.current.delete(id)
    revs.current.delete(id)
    scratchboardApi.remove(id).catch((e) => setSaveErr(`The saved copy was not deleted: ${e.message}`))
  }

  const addBoard = (init = {}) => {
    const names = new Set(boards.map((b) => b.name))
    let n = boards.length + 1
    while (names.has(`Board ${n}`)) n++
    const b = { ...newBoard(`Board ${n}`), ...init }
    setBoards((bs) => [...bs, b])
    setActiveId(b.id)
    return b
  }
  const removeBoard = (id) => {
    const i = boards.findIndex((b) => b.id === id)
    const gone = boards[i]
    if (!gone) return
    forget(id)
    setConflicts(({ [id]: _, ...rest }) => rest)
    const next = boards.filter((b) => b.id !== id)
    const fresh = next.length ? null : newBoard('Board 1')
    setBoards(fresh ? [fresh] : next)
    if (id === activeId) setActiveId((fresh || next[Math.max(0, i - 1)]).id)
    pushUndo(`remove “${gone.name}”`, id, (bs) => {
      const without = bs.filter((b) => b.id !== id && !(fresh && b.id === fresh.id && !b.cards.length))
      return [...without.slice(0, i), gone, ...without.slice(i)]
    })
  }
  const keepBoard = (id, keep) => {
    if (!keep) forget(id)
    patchBoard(id, (b) => ({ ...b, keep }))
  }
  const importFile = (text) => {
    const r = importBoard(text, KNOWN)
    if (r.error) { setSaveErr(r.error); return false }
    addBoard({ name: r.name, cards: r.cards, links: r.links, view: r.view })
    setNotice(r.skipped ? `Imported “${r.name}” — ${r.skipped} card(s) of a kind this version does not have were left out` : `Imported “${r.name}” as a temporary board`)
    return true
  }

  // ---- placing cards -----------------------------------------------------------------------
  // Where a new card goes. Cards opened one after another are laid out left to right from the
  // last one placed, wrapping to a new row at the right edge of what is on screen, so a batch
  // (several cards sent at once) does not land in one pile. The first one, or the first after
  // the view moved away, goes near the top-left of what is on screen. A ref rather than state:
  // a batch is placed within one render, before any state update would be seen.
  const lastPlaced = useRef(null)
  useEffect(() => { lastPlaced.current = null }, [board.id])
  const place = useCallback((w) => {
    const el = wrapRef.current
    const vw = (el?.clientWidth || 1200) / view.z
    const vh = (el?.clientHeight || 700) / view.z
    const left = -view.x / view.z + 24
    const top = -view.y / view.z + 24
    const last = lastPlaced.current
    let at = { x: left, y: top }
    const onScreen = last && last.x >= left - 24 && last.x < left + vw && last.y >= top - 24 && last.y < top + vh
    if (onScreen) {
      at = { x: last.x + last.w + 24, y: last.y }
      if (at.x + w > left + vw) at = { x: left, y: last.y + 460 }
    }
    lastPlaced.current = { ...at, w }
    return at
  }, [view])

  // spawn opens a card; opts.from (a card's id) draws an arrow from the card it came out of.
  const spawn = useCallback((type, input, opts = {}) => {
    const T = CARD_TYPES[type] || CARD_TYPES.note
    const at = opts.at || place(T.w)
    const id = newId()
    setCards((cs) => [...cs, {
      id, type: CARD_TYPES[type] ? type : 'note', title: opts.title || '', shared: false,
      x: at.x, y: at.y, w: T.w, h: T.h, z: topZ(cs) + 1, state: { ...T.init(input), ...(opts.state || {}) },
    }])
    if (opts.from) addLink(board.id, opts.from, id)
  }, [place, setCards, addLink, board.id])

  // Content handed over from other pages, in memory, onto the board that is open.
  useEffect(() => subscribeScratchboard((item) => spawn(item.type || suggest(item.input)[0]?.type || 'note', item.input, { title: item.title, state: item.state })), [spawn])

  const patchCard = (id, fn) => setCards((cs) => cs.map((c) => (c.id === id ? fn(c) : c)))
  const focus = (id) => setCards((cs) => {
    const top = topZ(cs)
    return cs.map((c) => (c.id === id && c.z !== top ? { ...c, z: top + 1 } : c))
  })

  // ---- search -------------------------------------------------------------------------------
  const results = useMemo(() => searchBoards(boards, query, LABELS), [boards, query])
  const matchIds = useMemo(() => new Set(results.filter((r) => r.boardId === board.id).map((r) => r.cardId)), [results, board.id])
  const goTo = (r) => {
    const b = boards.find((x) => x.id === r.boardId)
    const c = b?.cards.find((x) => x.id === r.cardId)
    if (!c) return
    const el = wrapRef.current
    const z = b.view.z
    const top = topZ(b.cards)
    patchBoard(b.id, (x) => ({
      ...x,
      view: { ...x.view, x: (el?.clientWidth || 1200) / 2 - (c.x + c.w / 2) * z, y: 60 - c.y * z },
      cards: x.cards.map((k) => (k.id === c.id ? { ...k, z: top + 1, collapsed: false } : k)),
    }))
    setActiveId(b.id)
    setFlash(c.id)
  }

  // Smart Paste: text pasted on the board itself (not into a field) offers the cards that fit it.
  useEffect(() => {
    if (!visible) return
    const onPaste = (e) => {
      if (inField(e.target)) return
      if (!wrapRef.current) return
      const text = e.clipboardData?.getData('text/plain')
      if (!text?.trim()) return
      e.preventDefault()
      const r = wrapRef.current.getBoundingClientRect()
      setMenu({ text, x: r.left + r.width / 2 - 100, y: r.top + 80, title: 'Pasted — open as' })
    }
    addEventListener('paste', onPaste)
    return () => removeEventListener('paste', onPaste)
  }, [visible])

  // Escape closes the menus; Delete closes the top card and Ctrl/Cmd+Z undoes, when no field has
  // the focus.
  useEffect(() => {
    if (!visible) { setMenu(null); setAddMenu(null); return }
    const onKey = (e) => {
      if (e.key === 'Escape') { setMenu(null); setAddMenu(null); setFileMenu(false) }
      if (inField(e.target)) return
      if (e.key === 'Delete') {
        const top = cards.reduce((a, c) => (!a || c.z > a.z ? c : a), null)
        if (top) closeCard(board.id, top.id)
      }
      if ((e.ctrlKey || e.metaKey) && !e.shiftKey && e.key.toLowerCase() === 'z') {
        e.preventDefault()
        runUndo()
      }
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [visible, cards, board.id, closeCard, runUndo])

  // Wheel zoom, anchored at the cursor; a wheel over a card's own scrolling content scrolls it.
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    const onWheel = (e) => {
      if (e.target !== el && !e.target.dataset?.board) return
      e.preventDefault()
      const r = el.getBoundingClientRect()
      setView((v) => zoomAt(v, e.clientX - r.left, e.clientY - r.top, e.deltaY, 0.3, 2))
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [setView])

  // Drags: the board pans, a title bar moves its card, the corner resizes it, the dot draws an
  // arrow.
  const drag = (e, apply, done) => {
    e.preventDefault()
    e.stopPropagation()
    const sx = e.clientX
    const sy = e.clientY
    const move = (ev) => apply(ev.clientX - sx, ev.clientY - sy, ev)
    const up = (ev) => { removeEventListener('pointermove', move); removeEventListener('pointerup', up); done?.(ev) }
    addEventListener('pointermove', move)
    addEventListener('pointerup', up)
  }
  const toBoard = (clientX, clientY) => {
    const r = wrapRef.current.getBoundingClientRect()
    return { x: (clientX - r.left - view.x) / view.z, y: (clientY - r.top - view.y) / view.z }
  }
  const onBoard = (e) => e.target === wrapRef.current || e.target.dataset?.board
  const panStart = (e) => {
    if (e.button !== 0 || !onBoard(e)) return
    setMenu(null)
    setAddMenu(null)
    setFileMenu(false)
    const v0 = view
    drag(e, (dx, dy) => setView({ ...v0, x: v0.x + dx, y: v0.y + dy }))
  }
  // Right-click on the board itself offers every card, and the one picked lands at the pointer.
  const onContextMenu = (e) => {
    if (!onBoard(e)) return
    e.preventDefault()
    setMenu(null)
    setAddMenu({ x: Math.max(8, Math.min(e.clientX, innerWidth - 500)), y: Math.max(8, Math.min(e.clientY, innerHeight - 440)), at: toBoard(e.clientX, e.clientY) })
  }
  const moveStart = (card) => (e) => {
    if (e.button !== 0) return
    focus(card.id)
    drag(e, (dx, dy) => patchCard(card.id, (c) => ({ ...c, x: card.x + dx / view.z, y: card.y + dy / view.z })))
  }
  const resizeStart = (card) => (e) => {
    const h0 = card.sized ? card.h : Math.max(120, (e.currentTarget.parentElement?.offsetHeight || card.h) - 30)
    drag(e, (dx, dy) => patchCard(card.id, (c) => ({
      ...c, sized: true, w: clamp(card.w + dx / view.z, 240, 1400), h: clamp(h0 + dy / view.z, 120, 2400),
    })))
  }
  const linkStart = (card) => (e) => {
    if (e.button !== 0) return
    const id = board.id
    drag(e, (dx, dy, ev) => setDrawing({ from: card.id, ...toBoard(ev.clientX, ev.clientY) }), (ev) => {
      setDrawing(null)
      const target = document.elementFromPoint(ev.clientX, ev.clientY)?.closest?.('[data-card-id]')?.dataset.cardId
      addLink(id, card.id, target)
    })
  }

  // Files dropped on the board are read here, as text: a board export opens as a board, anything
  // else is offered like a paste.
  const onDrop = async (e) => {
    e.preventDefault()
    const f = e.dataTransfer?.files?.[0]
    if (!f) return
    if (f.size > 5 * 1024 * 1024) { setMenu({ text: `${f.name} is larger than 5 MB — not opened`, x: e.clientX, y: e.clientY, title: 'Not opened' }); return }
    const text = await f.text()
    if (/\.json$/i.test(f.name) && text.includes('"dbcanvas-scratchboard"') && importFile(text)) return
    setMenu({ text, x: e.clientX, y: e.clientY, title: `${f.name} — open as` })
  }

  const lastUndo = undo[undo.length - 1]
  const conflictList = Object.entries(conflicts).map(([id, c]) => ({ id, c, b: boards.find((x) => x.id === id) })).filter((x) => x.b)

  return (
    <div className="flex h-full min-h-[60vh] flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative">
          <Button size="sm" onClick={() => setAddMenu((m) => (m?.button ? null : { button: true }))}><Icon.Plus size={14} /> Add card</Button>
          {addMenu?.button && (
            <AddCardMenu className="absolute left-0 top-full mt-1" onPick={(type) => { spawn(type, ''); setAddMenu(null) }} />
          )}
        </div>
        <div className="relative">
          <div className="flex items-center gap-1 rounded border bg-bg px-1.5">
            <Icon.Search size={12} />
            <input className="w-44 bg-transparent py-1 text-xs text-fg outline-none" placeholder="Search every board" value={query}
              onChange={(e) => setQuery(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && results[0]) goTo(results[0]); if (e.key === 'Escape') setQuery('') }} />
            {query && <button className="text-muted hover:text-fg" onClick={() => setQuery('')}><Icon.Close size={11} /></button>}
          </div>
          {query.trim() && (
            <div className="absolute left-0 top-full z-50 mt-1 max-h-80 w-96 overflow-auto rounded-lg border bg-surface p-1 text-xs shadow-xl">
              {!results.length && <div className="px-2 py-1 text-muted">Nothing on any board matches.</div>}
              {results.map((r) => (
                <button key={`${r.boardId}:${r.cardId}`} className="block w-full rounded px-2 py-1 text-left hover:bg-surface2" onClick={() => goTo(r)}>
                  <div className="truncate text-fg"><span className="text-muted">{r.boardName} ›</span> {r.title}</div>
                  <div className="truncate font-mono text-[10px] text-muted">{r.snippet}</div>
                </button>
              ))}
            </div>
          )}
        </div>
        <span className="flex-1" />
        {notice && <span className="text-[11px] text-success">{notice}</span>}
        <Button variant="outline" size="sm" disabled={!lastUndo} onClick={runUndo} title={lastUndo ? `Undo ${lastUndo.label} (Ctrl/⌘+Z)` : 'Nothing to undo'}>
          <Icon.ArrowLeft size={14} /> Undo
        </Button>
        <Help text={HELP.scratchboard} />
        <Button variant="outline" size="sm" onClick={() => setView({ x: 0, y: 0, z: 1 })} title="Reset pan and zoom">{Math.round(view.z * 100)}%</Button>
        <div className="relative">
          <Button variant="outline" size="sm" onClick={() => setFileMenu((o) => !o)} title="Export, Markdown, import"><Icon.File size={14} /> Board</Button>
          {fileMenu && <BoardMenu board={board} onImport={importFile} onCopied={() => setNotice('Copied the board as Markdown')} onClose={() => setFileMenu(false)} />}
        </div>
        <ConfirmButton variant="outline" size="sm" confirmLabel="Close every card?" disabled={!cards.length} onConfirm={clearBoard}>
          <Icon.Trash size={14} /> Clear board
        </ConfirmButton>
      </div>

      {recording && (
        <div className="rounded-lg border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs text-warning">
          A session recording is running. It is a video of this tab, so it captures every card on screen — shared or not.
        </div>
      )}
      {conflictList.map(({ id, c, b }) => (
        <div key={id} className="flex flex-wrap items-center gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs text-fg">
          {c.board ? (
            <>
              <span className="flex-1">“{b.name}” was saved from another tab or browser since this one loaded it. Which version should be kept?</span>
              <Button size="sm" variant="outline" onClick={() => resolveConflict(id, 'theirs')}>Load that version</Button>
              <Button size="sm" variant="outline" onClick={() => resolveConflict(id, 'ours')}>Keep this one</Button>
            </>
          ) : (
            <>
              <span className="flex-1">“{b.name}” was unpinned in another tab or browser, so its saved copy is gone.</span>
              <Button size="sm" variant="outline" onClick={() => resolveConflict(id, 'ours')}>Keep it again</Button>
              <Button size="sm" variant="outline" onClick={() => resolveConflict(id, 'temporary')}>Make it temporary here</Button>
            </>
          )}
        </div>
      ))}
      {saveErr && (
        <div className="flex items-center rounded-lg border border-danger/30 bg-danger/10 px-3 py-1.5 text-xs text-danger">
          <span className="flex-1">{saveErr}</span>
          <button className="hover:text-fg" onClick={() => setSaveErr('')}><Icon.Close size={12} /></button>
        </div>
      )}

      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex flex-wrap items-end gap-0.5">
          {boards.map((b) => (
            <BoardTab key={b.id} board={b} active={b.id === board.id} canKeep={canKeep} removable={boards.length > 1 || b.cards.length > 0 || b.keep}
              onSelect={() => { setActiveId(b.id); setMenu(null); setAddMenu(null) }}
              onRename={(name) => patchBoard(b.id, (x) => ({ ...x, name: name.slice(0, 80) }))}
              onKeep={(keep) => keepBoard(b.id, keep)}
              onRemove={() => removeBoard(b.id)} />
          ))}
          <button className="mb-0.5 rounded p-1 text-muted hover:bg-surface2 hover:text-fg" title="New board" onClick={() => addBoard()}><Icon.Plus size={13} /></button>
          <span className="flex-1" />
          <span className="mb-1 text-[11px] text-muted">
            {canKeep
              ? (board.keep ? 'This board is kept — saved to your account, encrypted.' : 'This board is temporary — pin it to keep it.')
              : 'Boards are temporary. Keeping them can be turned on in Settings.'}
          </span>
        </div>

        <div ref={wrapRef} data-board="1" className="relative flex-1 overflow-hidden rounded-lg rounded-tl-none border bg-bg"
          style={{ backgroundImage: 'radial-gradient(rgba(128, 128, 128, 0.28) 1px, transparent 1px)', backgroundSize: `${24 * view.z}px ${24 * view.z}px`, backgroundPosition: `${view.x}px ${view.y}px` }}
          onPointerDown={panStart} onContextMenu={onContextMenu} onDragOver={(e) => e.preventDefault()} onDrop={onDrop}>
          {!cards.length && (
            <div data-board="1" className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-1 text-center text-sm text-muted">
              <div className="text-base font-medium text-fg">Scratchboard</div>
              <div>Paste anything here — a certificate, a Secret, JSON, a slow log, an EXPLAIN, a GTID set, a URI, log lines — or right-click to add a card.</div>
              <div className="text-xs">
                {board.keep
                  ? 'This board is kept: it is saved to your account, encrypted, until you unpin it or remove it.'
                  : 'Nothing on this board is saved: closing this window or reloading the browser clears it.'} Cards are hidden from session guests unless you show them.
              </div>
            </div>
          )}
          <div data-board="1" className="absolute left-0 top-0 origin-top-left" style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})` }}>
            <LinkLayer cards={cards} links={board.links} drawing={drawing} onRemove={removeLink} />
            {cards.map((c) => (
              <CardFrame key={c.id} card={c} highlight={flash === c.id || matchIds.has(c.id)}
                onFocus={() => focus(c.id)}
                onPointerDownMove={moveStart(c)}
                onPointerDownResize={resizeStart(c)}
                onFitHeight={() => patchCard(c.id, (x) => ({ ...x, sized: false }))}
                onLinkStart={linkStart(c)}
                update={(patch, frame) => patchCard(c.id, (x) => (frame ? { ...x, ...patch } : { ...x, state: { ...x.state, ...patch } }))}
                close={() => closeCard(board.id, c.id)}
                duplicate={() => setCards((cs) => [...cs, { ...c, id: newId(), x: c.x + 24, y: c.y + 24, z: topZ(cs) + 1, state: structuredClone(c.state) }])}
                spawn={(type, input, state) => spawn(type, input, { at: { x: c.x + c.w + 24, y: c.y }, state, from: c.id })}
                onSelectText={(text, x, y) => setMenu({ text, x, y: y + 8, title: 'Selection — open as', at: { x: c.x + c.w + 24, y: c.y }, from: c.id })} />
            ))}
          </div>
        </div>
      </div>

      {/* The menus open at the pointer, in viewport coordinates: they render into the
          document body, as the page's tab wrapper keeps a transform from its fade-in, which
          would make "fixed" relative to the wrapper rather than the window. */}
      {addMenu?.at && createPortal(
        <AddCardMenu className="fixed" style={{ left: addMenu.x, top: addMenu.y }}
          onPick={(type) => { spawn(type, '', { at: addMenu.at }); setAddMenu(null) }} />,
        document.body,
      )}
      {menu && createPortal(
        <SuggestMenu text={menu.text} x={menu.x} y={menu.y} title={menu.title} onClose={() => setMenu(null)}
          onPick={(type) => { spawn(type, menu.text, { ...(menu.at ? { at: menu.at } : {}), from: menu.from }); setMenu(null) }} />,
        document.body,
      )}
    </div>
  )
}
