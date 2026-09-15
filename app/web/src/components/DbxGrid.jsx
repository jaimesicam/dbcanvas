import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from './Icons.jsx'
import { Badge, Button } from './ui.jsx'
import {
  NULL_MARK, cellDisplay, cellText, cellNumber, copyText, csvField, download,
  filterRows, fmtNumber, isBinary, isNull, isNumericCol, looksJson, prettyJson,
  sortRows, toCSV, toJSON,
} from '../lib/dbxApi.js'

// DbxGrid — the result grid.
//
// The one hard requirement is that it must not fall over on a large result, and the
// answer is windowing: rows are a fixed height, only the visible slice is in the DOM,
// and the rest is two spacer divs. A thousand rows of thirty columns is thirty
// thousand cells, and rendering them all is how a browser tab stops responding to a
// query that the server answered in eight milliseconds.
//
// Everything else follows from being a tool for *looking at data*: NULL is visibly
// not the empty string, numbers are right-aligned so their magnitudes line up, a
// value too long to show is cut with the whole thing still one click away, binary is
// shown as a length and a hex head rather than as mojibake, and the header stays put.

const ROW_H = 28
const HEAD_H = 34
const OVERSCAN = 12
const DEFAULT_W = 168
const MIN_W = 56
const NUM_COL_W = 56

// widthFor is the first guess at a column's width, from its name and what kind of
// value it holds. A guess rather than a measurement: measuring every cell of a large
// result to lay out the header is the same mistake as rendering them all.
function widthFor(col) {
  const base = Math.min(320, Math.max(DEFAULT_W, col.name.length * 9 + 44))
  if (col.semanticType === 'boolean') return Math.min(base, 92)
  if (col.semanticType === 'integer' || col.semanticType === 'number') return Math.min(base, 140)
  if (col.semanticType === 'datetime') return Math.max(base, 200)
  return base
}

export function DbxGrid({
  columns = [], rows = [], truncated = false, rowLimit = 0, name = 'result',
  editable = false, onEditRow, onDeleteRow, onInsertRow,
  onFilterByValue, onOpenValue, emptyLabel = 'No rows.', footerExtra = null,
}) {
  const [sort, setSort] = useState({ index: null, dir: null })
  const [term, setTerm] = useState('')
  const [hidden, setHidden] = useState(() => new Set())
  const [order, setOrder] = useState(null)
  const [widths, setWidths] = useState(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewport, setViewport] = useState(520)
  const [sel, setSel] = useState(null) // {r, c}
  const [menu, setMenu] = useState(null)
  const [picker, setPicker] = useState(false)
  const scroller = useRef(null)

  // A new result is a new grid: the sort, the hidden columns and the widths belonged
  // to the old one and carrying them over would silently reorder somebody's rows.
  const colKey = useMemo(() => columns.map((c) => `${c.name}:${c.databaseType || ''}`).join('|'), [columns])
  useEffect(() => {
    setSort({ index: null, dir: null })
    setHidden(new Set())
    setOrder(columns.map((_, i) => i))
    setWidths(columns.map(widthFor))
    setSel(null)
    if (scroller.current) scroller.current.scrollTop = 0
    setScrollTop(0)
  }, [colKey]) // eslint-disable-line react-hooks/exhaustive-deps

  const colOrder = order || columns.map((_, i) => i)
  const colWidths = widths || columns.map(widthFor)
  const visibleCols = colOrder.filter((i) => !hidden.has(i))

  const filtered = useMemo(() => filterRows(rows, term), [rows, term])
  const view = useMemo(
    () => sortRows(filtered, sort.index, sort.dir, columns[sort.index]),
    [filtered, sort.index, sort.dir, columns],
  )

  useEffect(() => {
    const el = scroller.current
    if (!el || typeof ResizeObserver === 'undefined') return undefined
    const ro = new ResizeObserver(() => setViewport(el.clientHeight || 520))
    ro.observe(el)
    setViewport(el.clientHeight || 520)
    return () => ro.disconnect()
  }, [])

  const first = Math.max(0, Math.floor(scrollTop / ROW_H) - OVERSCAN)
  const count = Math.ceil(viewport / ROW_H) + OVERSCAN * 2
  const last = Math.min(view.length, first + count)
  const slice = view.slice(first, last)

  const toggleSort = useCallback((i) => {
    setSort((s) => {
      if (s.index !== i) return { index: i, dir: 'asc' }
      if (s.dir === 'asc') return { index: i, dir: 'desc' }
      return { index: null, dir: null } // a third click restores the database's order
    })
  }, [])

  const setWidth = useCallback((i, w) => {
    setWidths((ws) => {
      const next = [...(ws || columns.map(widthFor))]
      next[i] = Math.max(MIN_W, w)
      return next
    })
  }, [columns])

  const move = useCallback((from, to) => {
    setOrder((o) => {
      const next = [...(o || columns.map((_, i) => i))]
      const at = next.indexOf(from)
      const target = next.indexOf(to)
      if (at < 0 || target < 0) return next
      next.splice(at, 1)
      next.splice(target, 0, from)
      return next
    })
  }, [columns])

  const copyCell = () => sel && copyText(cellText(view[sel.r][sel.c]))
  const copyRow = () => sel && copyText(view[sel.r].map(csvField).join('\t'))

  // Keyboard: arrows move the selection, Ctrl/Cmd+C copies it. The handler is bound
  // to the grid rather than the document so it never steals keys from the editor.
  const onKeyDown = (e) => {
    if (!sel) return
    const vis = visibleCols
    const at = vis.indexOf(sel.c)
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      const r = Math.min(view.length - 1, Math.max(0, sel.r + (e.key === 'ArrowDown' ? 1 : -1)))
      setSel({ ...sel, r })
      if (scroller.current) {
        const top = r * ROW_H
        const el = scroller.current
        if (top < el.scrollTop) el.scrollTop = top
        else if (top + ROW_H > el.scrollTop + el.clientHeight - HEAD_H) el.scrollTop = top + ROW_H - el.clientHeight + HEAD_H
      }
    } else if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      e.preventDefault()
      const j = Math.min(vis.length - 1, Math.max(0, at + (e.key === 'ArrowRight' ? 1 : -1)))
      setSel({ ...sel, c: vis[j] })
    } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'c') {
      e.preventDefault()
      if (e.shiftKey) copyRow(); else copyCell()
    }
  }

  const totalW = NUM_COL_W + visibleCols.reduce((a, i) => a + colWidths[i], 0)

  if (!columns.length) {
    return <div className="flex h-full items-center justify-center p-8 text-sm text-muted">{emptyLabel}</div>
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <GridToolbar
        columns={columns} hidden={hidden} setHidden={setHidden} picker={picker} setPicker={setPicker}
        term={term} setTerm={setTerm} shown={view.length} total={rows.length}
        truncated={truncated} rowLimit={rowLimit}
        onCSV={() => download(`${name}.csv`, toCSV(visibleCols.map((i) => columns[i]), view.map((r) => visibleCols.map((i) => r[i]))), 'text/csv')}
        onJSON={() => download(`${name}.json`, toJSON(visibleCols.map((i) => columns[i]), view.map((r) => visibleCols.map((i) => r[i]))), 'application/json')}
        onInsertRow={editable ? onInsertRow : null}
        extra={footerExtra}
      />

      <div
        ref={scroller}
        tabIndex={0}
        onKeyDown={onKeyDown}
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        className="relative min-h-0 flex-1 overflow-auto outline-none focus:ring-1 focus:ring-primary/30"
      >
        <div style={{ width: totalW, minWidth: '100%' }}>
          <div className="sticky top-0 z-20 flex border-b bg-surface2" style={{ height: HEAD_H }}>
            <div className="shrink-0 border-r px-2 py-2 text-[11px] font-medium text-muted" style={{ width: NUM_COL_W }}>#</div>
            {visibleCols.map((i) => (
              <HeaderCell
                key={`${columns[i].name}-${i}`} col={columns[i]} index={i} width={colWidths[i]}
                sort={sort} onSort={() => toggleSort(i)} onWidth={(w) => setWidth(i, w)}
                onMove={move} onHide={() => setHidden((h) => new Set(h).add(i))}
              />
            ))}
          </div>

          <div style={{ height: first * ROW_H }} />
          {slice.map((row, k) => {
            const r = first + k
            return (
              <div
                key={r}
                className={`flex border-b border-border/40 ${sel && sel.r === r ? 'bg-primary/5' : 'hover:bg-surface2/60'}`}
                style={{ height: ROW_H }}
              >
                <div className="shrink-0 select-none border-r px-2 text-right text-[11px] leading-[28px] text-muted" style={{ width: NUM_COL_W }}>
                  {r + 1}
                </div>
                {visibleCols.map((i) => (
                  <Cell
                    key={i} value={row[i]} col={columns[i]} width={colWidths[i]}
                    selected={!!sel && sel.r === r && sel.c === i}
                    onSelect={() => setSel({ r, c: i })}
                    onMenu={(ev) => {
                      ev.preventDefault()
                      setSel({ r, c: i })
                      setMenu({ x: ev.clientX, y: ev.clientY, r, c: i })
                    }}
                  />
                ))}
              </div>
            )
          })}
          <div style={{ height: Math.max(0, (view.length - last) * ROW_H) }} />

          {view.length === 0 && (
            <div className="p-8 text-center text-sm text-muted">
              {rows.length ? 'Nothing in this result matches the filter.' : emptyLabel}
            </div>
          )}
        </div>
      </div>

      {menu && (
        <CellMenu
          at={menu}
          value={view[menu.r] ? view[menu.r][menu.c] : null}
          col={columns[menu.c]}
          row={view[menu.r]}
          columns={columns}
          editable={editable}
          onClose={() => setMenu(null)}
          onCopyCell={copyCell}
          onCopyRow={copyRow}
          onSort={(dir) => setSort({ index: menu.c, dir })}
          onFilterByValue={onFilterByValue}
          onOpenValue={onOpenValue}
          onEditRow={onEditRow}
          onDeleteRow={onDeleteRow}
        />
      )}
    </div>
  )
}

function GridToolbar({
  columns, hidden, setHidden, picker, setPicker, term, setTerm, shown, total,
  truncated, rowLimit, onCSV, onJSON, onInsertRow, extra,
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 border-b bg-surface px-2 py-1.5">
      <div className="relative flex items-center">
        <Icon.Search size={13} className="pointer-events-none absolute left-2 text-muted" />
        <input
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          placeholder="Filter rows…"
          className="ui-input w-44 rounded-md border bg-bg py-1 pl-7 pr-2 text-xs outline-none focus:border-primary"
        />
      </div>
      <span className="text-xs tabular-nums text-muted">
        {shown === total ? `${fmtNumber(total)} rows` : `${fmtNumber(shown)} of ${fmtNumber(total)} rows`}
      </span>
      {truncated && (
        <Badge tone="warning">
          capped at {fmtNumber(rowLimit || total)} — raise the limit to fetch more
        </Badge>
      )}
      <div className="flex-1" />
      {extra}
      {onInsertRow && (
        <button onClick={onInsertRow} className="rounded-md border px-2 py-1 text-xs hover:bg-surface2" title="Insert a row">
          <Icon.Plus size={12} className="inline" /> Row
        </button>
      )}
      <div className="relative">
        <button
          onClick={() => setPicker((v) => !v)}
          className="rounded-md border px-2 py-1 text-xs hover:bg-surface2"
          title="Show or hide columns"
        >
          Columns{hidden.size ? ` (${columns.length - hidden.size}/${columns.length})` : ''}
        </button>
        {picker && (
          <div className="absolute right-0 z-40 mt-1 max-h-72 w-56 overflow-auto rounded-lg border bg-surface p-1.5 shadow-lg">
            <div className="flex gap-1 border-b pb-1.5">
              <button className="flex-1 rounded px-1.5 py-1 text-xs hover:bg-surface2" onClick={() => setHidden(new Set())}>All</button>
              <button className="flex-1 rounded px-1.5 py-1 text-xs hover:bg-surface2" onClick={() => setHidden(new Set(columns.map((_, i) => i).slice(1)))}>None</button>
            </div>
            {columns.map((c, i) => (
              <label key={i} className="flex cursor-pointer items-center gap-2 rounded px-1.5 py-1 text-xs hover:bg-surface2">
                <input
                  type="checkbox"
                  checked={!hidden.has(i)}
                  onChange={() => setHidden((h) => {
                    const n = new Set(h)
                    if (n.has(i)) n.delete(i); else n.add(i)
                    return n
                  })}
                />
                <span className="truncate">{c.name}</span>
                <span className="ml-auto shrink-0 text-[10px] text-muted">{c.databaseType || c.semanticType}</span>
              </label>
            ))}
          </div>
        )}
      </div>
      <button onClick={onCSV} className="rounded-md border px-2 py-1 text-xs hover:bg-surface2" title="Download the rows shown as CSV">CSV</button>
      <button onClick={onJSON} className="rounded-md border px-2 py-1 text-xs hover:bg-surface2" title="Download the rows shown as JSON">JSON</button>
    </div>
  )
}

function HeaderCell({ col, index, width, sort, onSort, onWidth, onMove, onHide }) {
  const dragging = useRef(null)
  const active = sort.index === index
  const start = (e) => {
    e.preventDefault()
    e.stopPropagation()
    dragging.current = { x: e.clientX, w: width }
    const onMove2 = (ev) => onWidth(dragging.current.w + (ev.clientX - dragging.current.x))
    const onUp = () => {
      removeEventListener('mousemove', onMove2)
      removeEventListener('mouseup', onUp)
    }
    addEventListener('mousemove', onMove2)
    addEventListener('mouseup', onUp)
  }
  return (
    <div
      className="group relative flex shrink-0 items-center gap-1 border-r px-2 text-xs font-medium"
      style={{ width }}
      draggable
      onDragStart={(e) => e.dataTransfer.setData('text/dbx-col', String(index))}
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault()
        const from = Number(e.dataTransfer.getData('text/dbx-col'))
        if (!Number.isNaN(from) && from !== index) onMove(from, index)
      }}
      title={`${col.name}${col.databaseType ? ` · ${col.databaseType}` : ''}`}
    >
      <button onClick={onSort} className="flex min-w-0 flex-1 items-center gap-1 text-left">
        <span className="truncate">{col.name}</span>
        {active && <span className="shrink-0 text-primary">{sort.dir === 'asc' ? '▲' : '▼'}</span>}
      </button>
      {/* The database's own type, always on screen. It is the thing you open a client
          to see, and hiding it behind a hover would be hiding the answer. */}
      <span className="shrink-0 text-[10px] font-normal lowercase text-muted">{col.databaseType || col.semanticType}</span>
      <button
        onClick={onHide}
        className="hidden shrink-0 text-muted hover:text-danger group-hover:block"
        title="Hide this column"
      >
        <Icon.Close size={11} />
      </button>
      <span
        onMouseDown={start}
        className="absolute right-0 top-0 h-full w-1.5 cursor-col-resize hover:bg-primary/40"
      />
    </div>
  )
}

function Cell({ value, col, width, selected, onSelect, onMenu }) {
  const numeric = isNumericCol(col)
  const nul = isNull(value)
  const bin = isBinary(value)
  const bool = col && col.semanticType === 'boolean'
  let body
  if (nul) {
    // NULL gets its own mark and its own colour, so it is never mistaken for an
    // empty string sitting in the same column.
    body = <span className="italic text-muted/70" title="NULL">{NULL_MARK}</span>
  } else if (bool) {
    const on = value === true || value === 1 || value === '1' || value === 'true' || value === 't'
    body = <span className={on ? 'text-success' : 'text-muted'}>{on ? '✓ true' : '✗ false'}</span>
  } else if (bin) {
    body = <span className="text-accent" title={`${value.len} bytes`}>{cellDisplay(value)}</span>
  } else {
    body = cellDisplay(value)
  }
  return (
    <div
      onMouseDown={onSelect}
      onContextMenu={onMenu}
      className={`shrink-0 truncate border-r px-2 leading-[28px] ${numeric ? 'text-right tabular-nums' : ''} ${
        selected ? 'bg-primary/15 ring-1 ring-inset ring-primary/50' : ''
      }`}
      style={{ width }}
      title={nul ? 'NULL' : cellText(value).slice(0, 2000)}
    >
      {body}
    </div>
  )
}

function CellMenu({
  at, value, col, row, columns, editable, onClose, onCopyCell, onCopyRow,
  onSort, onFilterByValue, onOpenValue, onEditRow, onDeleteRow,
}) {
  useEffect(() => {
    const close = () => onClose()
    addEventListener('mousedown', close)
    addEventListener('keydown', close)
    return () => {
      removeEventListener('mousedown', close)
      removeEventListener('keydown', close)
    }
  }, [onClose])
  const item = (label, fn, tone = '') => (
    <button
      onMouseDown={(e) => { e.stopPropagation(); fn(); onClose() }}
      className={`block w-full rounded px-2 py-1 text-left text-xs hover:bg-surface2 ${tone}`}
    >
      {label}
    </button>
  )
  const json = looksJson(value, col)
  return (
    <div
      className="fixed z-50 w-52 rounded-lg border bg-surface p-1 shadow-xl"
      style={{ left: Math.min(at.x, (typeof window !== 'undefined' ? window.innerWidth : 1200) - 220), top: at.y }}
      onMouseDown={(e) => e.stopPropagation()}
    >
      <div className="truncate px-2 py-1 text-[10px] uppercase tracking-wide text-muted">{col ? col.name : 'cell'}</div>
      {item('Copy cell', onCopyCell)}
      {item('Copy row (TSV)', onCopyRow)}
      {item('Copy column name', () => copyText(col ? col.name : ''))}
      <div className="my-1 border-t" />
      {item('View full value', () => onOpenValue && onOpenValue({ value, col, row, columns }))}
      {json && item('View as JSON', () => onOpenValue && onOpenValue({ value, col, row, columns, json: true }))}
      <div className="my-1 border-t" />
      {item('Sort ascending', () => onSort('asc'))}
      {item('Sort descending', () => onSort('desc'))}
      {onFilterByValue && item('Filter by this value', () => onFilterByValue({ col, value }))}
      {editable && onEditRow && (
        <>
          <div className="my-1 border-t" />
          {item('Edit row…', () => onEditRow(row))}
          {item('Delete row…', () => onDeleteRow(row), 'text-danger')}
        </>
      )}
    </div>
  )
}

// ValueViewer is the "this cell is bigger than a cell" panel: the whole value, JSON
// pretty-printed when it is JSON, and a copy button — because the reason to open a
// long value is usually to take it somewhere else.
export function ValueViewer({ open, onClose }) {
  const [copied, setCopied] = useState(false)
  if (!open) return null
  const { value, col, json } = open
  const text = isNull(value) ? 'NULL' : cellText(value)
  const shown = json ? prettyJson(text) : text
  const n = cellNumber(value)
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div
        className="flex max-h-[80vh] w-full max-w-3xl flex-col rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between gap-3 border-b px-4 py-2.5">
          <div className="min-w-0">
            <div className="truncate text-sm font-semibold">{col ? col.name : 'Value'}</div>
            <div className="text-xs text-muted">
              {col && col.databaseType ? `${col.databaseType} · ` : ''}
              {isNull(value) ? 'NULL' : `${text.length} characters`}
              {n !== null && !isNull(value) ? ` · numeric ${n}` : ''}
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Button size="sm" variant="subtle" onClick={async () => { setCopied(await copyText(text)); setTimeout(() => setCopied(false), 1500) }}>
              <Icon.Copy size={13} /> {copied ? 'Copied' : 'Copy'}
            </Button>
            <Button size="sm" variant="ghost" onClick={onClose}><Icon.Close size={14} /></Button>
          </div>
        </div>
        <pre className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words p-4 font-mono text-xs leading-relaxed">
          {isBinary(value) ? `${value.len} bytes\nbase64: ${value.b64}` : shown}
        </pre>
      </div>
    </div>
  )
}

export default DbxGrid
