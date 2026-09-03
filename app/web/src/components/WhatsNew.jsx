import { useEffect, useState } from 'react'
import { Badge, Button } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { api } from '../lib/api.js'

// WhatsNew — the release notes, shown once per build.
//
// Three decisions worth knowing about, because each one is the answer to a way this
// kind of dialog usually annoys people:
//
//   - It opens by itself only when the SERVER says there is something unread
//     (app/whatsnew.go decides; this component never compares version strings). A
//     client that got that comparison subtly wrong would re-open the dialog on every
//     page load, which is the exact failure worth designing out.
//   - Dismissing it is recorded against the ACCOUNT, not the browser, so it does not
//     come back on your other machine and does come back after a real update.
//   - Opening it from the header link deliberately does NOT record anything. Reading
//     the notes again should not change any state; and if it did, somebody who opened
//     it out of curiosity before the auto-open had fired would never see it fire.

export function useWhatsNew() {
  const [data, setData] = useState(null)
  const [open, setOpen] = useState(false)
  // Whether this opening should mark the notes read. False when the user asked for
  // it from the header link.
  const [acknowledging, setAcknowledging] = useState(false)

  useEffect(() => {
    let stale = false
    api.whatsNew()
      .then((d) => {
        if (stale) return
        setData(d)
        if (d.hasUnseen) {
          setAcknowledging(true)
          setOpen(true)
        }
      })
      .catch(() => { /* a missing changelog is not worth telling anybody about */ })
    return () => { stale = true }
  }, [])

  // showAll opens every note on request, without touching the read state.
  const showAll = () => { setAcknowledging(false); setOpen(true) }

  const close = async () => {
    setOpen(false)
    if (!acknowledging) return
    setAcknowledging(false)
    // Optimistic: the dialog is already closed, and re-opening it because a write
    // failed would be worse than it re-opening on the next visit.
    setData((d) => (d ? { ...d, hasUnseen: false, seen: d.version } : d))
    await api.markWhatsNewSeen().catch(() => {})
  }

  return { data, open, showAll, close, acknowledging }
}

// WhatsNewLink is the affordance in the dashboard header. It carries a dot while
// there is something unread, which is the only reason a permanent link needs any
// decoration at all.
export function WhatsNewLink({ data, onClick }) {
  if (!data) return null
  return (
    <button
      onClick={onClick}
      className="inline-flex items-center gap-1.5 text-xs text-muted transition hover:text-fg"
      title="Release notes"
    >
      <Icon.Sparkles size={14} />
      <span>What's new</span>
      {data.hasUnseen && <span className="h-1.5 w-1.5 rounded-full bg-primary" />}
      {data.version && <span className="text-muted/70">· {data.version}</span>}
    </button>
  )
}

export default function WhatsNew({ data, open, onClose, acknowledging }) {
  // When it opened by itself, lead with what is actually new; when somebody asked
  // for it, show the lot.
  const notes = (acknowledging ? data?.unseen : data?.notes) || []
  const [expanded, setExpanded] = useState(() => new Set([0]))

  useEffect(() => {
    if (open) setExpanded(new Set([0]))
  }, [open, acknowledging])

  useEffect(() => {
    if (!open) return
    const onKey = (e) => { if (e.key === 'Escape') onClose() }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open || !notes.length) return null

  const toggle = (i) => setExpanded((prev) => {
    const next = new Set(prev)
    next.has(i) ? next.delete(i) : next.add(i)
    return next
  })

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onClose}>
      <div
        className="flex max-h-[85vh] w-full max-w-2xl flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label="What's new in DBCanvas"
      >
        <div className="flex items-start justify-between gap-3 border-b px-5 py-4">
          <div className="min-w-0">
            <h2 className="flex items-center gap-2 text-base font-semibold">
              <Icon.Sparkles size={18} />
              {acknowledging ? "What's new in DBCanvas" : 'Release notes'}
            </h2>
            <p className="mt-0.5 text-xs text-muted">
              {acknowledging
                ? `Updated to ${data?.version}. ${notes.length} thing${notes.length === 1 ? '' : 's'} changed since you were last here.`
                : `This installation is on ${data?.version}.`}
            </p>
          </div>
          <button onClick={onClose} className="rounded-lg p-1 text-muted hover:bg-surface2 hover:text-fg" aria-label="Close">
            <Icon.Close size={18} />
          </button>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-3">
          <div className="divide-y">
            {notes.map((n, i) => {
              const on = expanded.has(i)
              return (
                <div key={`${n.version}-${n.title}`} className="py-3">
                  <button onClick={() => toggle(i)} className="flex w-full items-start gap-3 text-left">
                    <span className="min-w-0 flex-1">
                      <span className="block text-sm font-medium text-fg">{n.title}</span>
                      <span className="mt-0.5 flex items-center gap-2">
                        <Badge tone="muted">{n.version}</Badge>
                        <span className="text-[11px] text-muted">{n.date}</span>
                      </span>
                    </span>
                    <span className={`shrink-0 text-muted transition-transform ${on ? 'rotate-180' : ''}`}>
                      <Icon.Chevron size={16} />
                    </span>
                  </button>
                  {on && (
                    <div className="mt-2">
                      <p className="text-sm leading-relaxed text-muted">{n.body}</p>
                      {n.doc && (
                        <a
                          className="mt-1.5 inline-flex items-center gap-1 text-xs text-primary hover:underline"
                          href={`https://github.com/jaimesicam/dbcanvas/blob/main/${n.doc}`}
                          target="_blank"
                          rel="noreferrer"
                        >
                          Read the full notes <Icon.External size={12} />
                        </a>
                      )}
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        </div>

        <div className="flex items-center justify-between gap-3 border-t px-5 py-3">
          <span className="text-xs text-muted">
            {acknowledging
              ? "This won't show again until the next update."
              : 'Reopen these any time from the What\'s new link.'}
          </span>
          <Button onClick={onClose}>{acknowledging ? 'Got it' : 'Close'}</Button>
        </div>
      </div>
    </div>
  )
}
