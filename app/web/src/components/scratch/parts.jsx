// parts.jsx — the pieces every Scratchboard card is built from (Cards.jsx and the cards in their
// own files): a text field, a result box, a key/value row, and useAsync.
//
// Like the cards, nothing here stores or sends anything.

import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Icon } from '../Icons.jsx'
import { suggest } from '../../lib/scratch/detect.js'

export const area = 'w-full resize-y rounded border bg-bg p-1.5 font-mono text-[11px] leading-snug text-fg outline-none focus:border-primary'
export const small = 'rounded border bg-bg px-1.5 py-0.5 text-[11px] text-fg outline-none focus:border-primary'

export function copy(text) {
  try { navigator.clipboard.writeText(text) } catch { /* no clipboard permission */ }
}

// Input is a textarea that grows with what is in it — from `rows` lines up to `maxRows`, then it
// scrolls — and reports a selection to the board (select-to-spawn). Its corner still resizes it
// by hand; the next change to its text fits it to the text again.
export function Input({ value, onChange, placeholder, rows = 5, maxRows = 30, onSelectText }) {
  const ref = useRef(null)
  useLayoutEffect(() => {
    const t = ref.current
    if (!t) return
    t.style.height = 'auto'
    const cs = getComputedStyle(t)
    const line = parseFloat(cs.lineHeight) || 14
    const border = t.offsetHeight - t.clientHeight
    const max = maxRows * line + parseFloat(cs.paddingTop || 0) + parseFloat(cs.paddingBottom || 0) + border
    const want = t.scrollHeight + border
    t.style.height = `${Math.min(want, max)}px`
    t.style.overflowY = want > max ? 'auto' : 'hidden'
  }, [value, maxRows])
  return (
    <textarea ref={ref} className={area} rows={rows} value={value} placeholder={placeholder} spellCheck={false}
      onChange={(e) => onChange(e.target.value)}
      onMouseUp={(e) => {
        const t = e.target
        const sel = t.value.slice(t.selectionStart, t.selectionEnd)
        if (sel.trim() && onSelectText) onSelectText(sel, e.clientX, e.clientY)
      }} />
  )
}

// Output is a read-only result with copy and "open in a new card".
export function Output({ text, error, spawn, rows }) {
  if (error) return <div className="rounded border border-danger/30 bg-danger/10 px-1.5 py-1 text-[11px] text-danger">{error}</div>
  return (
    <div className="relative">
      <pre className="max-h-[50vh] min-h-[2rem] overflow-auto whitespace-pre-wrap break-all rounded border bg-surface2 p-1.5 font-mono text-[11px] text-fg"
        style={rows ? { minHeight: `${rows * 1.3}em` } : undefined}>{text || ' '}</pre>
      {text && (
        <div className="absolute right-1 top-1 flex gap-0.5">
          <button className="rounded bg-surface p-0.5 text-muted hover:text-fg" title="Copy" onClick={() => copy(text)}><Icon.Copy size={12} /></button>
          {spawn && (
            <button className="rounded bg-surface p-0.5 text-muted hover:text-fg" title="Open in a new card"
              onClick={() => spawn(suggest(text)[0]?.type || 'note', text)}><Icon.PopOut size={12} /></button>
          )}
        </div>
      )}
    </div>
  )
}

export function KV({ k, v, mono = true }) {
  return (
    <div className="flex gap-2 text-[11px]">
      <span className="w-24 shrink-0 text-muted">{k}</span>
      <span className={`min-w-0 break-all text-fg ${mono ? 'font-mono' : ''}`}>{v || '—'}</span>
    </div>
  )
}

// useAsync runs an async computation when its inputs change, keeping the last result.
export function useAsync(fn, deps) {
  const [res, setRes] = useState(null)
  useEffect(() => {
    let live = true
    Promise.resolve().then(fn).then((r) => live && setRes(r), (e) => live && setRes({ error: e.message }))
    return () => { live = false }
  }, deps) // eslint-disable-line react-hooks/exhaustive-deps
  return res
}
