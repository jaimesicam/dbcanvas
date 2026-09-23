import { useEffect, useState } from 'react'
import { Badge, Button } from './ui.jsx'
import { Icon } from './Icons.jsx'
import { docURL } from '../lib/repo.js'
import { api } from '../lib/api.js'

// UpdateCheck — "is there a newer DBCanvas?", answered only when the button is pressed.
//
// Nothing here fetches on mount: the server only contacts GitHub when this asks it
// to (app/updatecheck.go), so an installation nobody clicks this on never makes a
// request off the machine. The server also does the version comparison, for the
// same reason WhatsNew leaves it there.

// How long an answer that needs no action ("Up to date", a failed check) stays on screen. Long
// enough to be read, short enough that the button is back to being an invitation to ask again.
// An available update does not fade: it is the one answer somebody has to act on.
const STATUS_MS = 60_000
const FADE_MS = 700

export function UpdateCheckButton() {
  const [state, setState] = useState({ phase: 'idle' }) // idle | checking | done | error
  const [open, setOpen] = useState(false)
  const [fading, setFading] = useState(false)

  const res = state.res
  const transient = state.phase === 'error' || (state.phase === 'done' && !res?.available)

  useEffect(() => {
    if (!transient) return
    setFading(false)
    const fade = setTimeout(() => setFading(true), STATUS_MS - FADE_MS)
    const clear = setTimeout(() => setState({ phase: 'idle' }), STATUS_MS)
    return () => { clearTimeout(fade); clearTimeout(clear) }
  }, [state, transient])

  const check = async () => {
    setFading(false)
    setState({ phase: 'checking' })
    try {
      const res = await api.checkUpdates()
      setState({ phase: 'done', res })
      if (res.available) setOpen(true)
    } catch (e) {
      setState({ phase: 'error', error: e?.message || 'Update check failed' })
    }
  }

  const pill = (tone, icon, text, title) => (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 font-medium transition-opacity duration-700 ${tone} ${fading ? 'opacity-0' : 'opacity-100'}`}
    >
      {icon}
      {text}
    </span>
  )

  let status = null
  if (state.phase === 'error') {
    status = pill('bg-danger/15 text-danger', <Icon.StatusCrit size={13} />, "Couldn't reach GitHub", state.error)
  } else if (res?.available) {
    status = (
      <button
        onClick={() => setOpen(true)}
        className="inline-flex items-center gap-1 rounded-full bg-primary/15 px-2 py-0.5 font-medium text-primary hover:bg-primary/25"
      >
        <Icon.Sparkles size={13} />
        {res.latest} available
      </button>
    )
  } else if (res?.dev) {
    status = pill('bg-muted/15 text-fg', <Icon.StatusInfo size={13} />, `Dev build · latest is ${res.latest}`,
      'This build is unstamped, so there is no version to compare')
  } else if (res) {
    status = pill('bg-success/15 text-success', <Icon.StatusOk size={13} />, `Up to date · ${res.current}`,
      `Nothing newer than ${res.current} on GitHub`)
  }

  const checking = state.phase === 'checking'
  return (
    <span className="inline-flex items-center gap-2 text-xs text-muted">
      {status}
      <button
        onClick={check}
        disabled={checking}
        className="inline-flex items-center gap-1.5 transition hover:text-fg disabled:opacity-60"
        title="Check GitHub for a newer DBCanvas"
      >
        <Icon.Refresh size={14} className={checking ? 'animate-spin' : ''} />
        <span>{checking ? 'Checking…' : 'Check for updates'}</span>
      </button>
      {open && res?.available && <UpdateDialog res={res} onClose={() => setOpen(false)} />}
    </span>
  )
}

function UpdateDialog({ res, onClose }) {
  const notes = res.notes || []
  const [expanded, setExpanded] = useState(() => new Set([0]))

  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose() }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [onClose])

  const toggle = (i) => setExpanded((prev) => {
    const next = new Set(prev)
    next.has(i) ? next.delete(i) : next.add(i)
    return next
  })

  // Group by version, keeping the server's newest-first order.
  const versions = []
  notes.forEach((n, i) => {
    const last = versions[versions.length - 1]
    if (last && last.version === n.version) last.items.push({ n, i })
    else versions.push({ version: n.version, items: [{ n, i }] })
  })

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 text-left" onMouseDown={onClose}>
      <div
        className="flex max-h-[85vh] w-full max-w-2xl flex-col overflow-hidden rounded-xl border bg-surface shadow-2xl"
        onMouseDown={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label="DBCanvas update available"
      >
        <div className="flex items-start justify-between gap-3 border-b px-5 py-4">
          <div className="min-w-0">
            <h2 className="flex items-center gap-2 text-base font-semibold text-fg">
              <Icon.Sparkles size={18} />
              DBCanvas {res.latest} is available
            </h2>
            <p className="mt-0.5 text-xs text-muted">
              This installation is on {res.current}. What changed from the next version up to {res.latest}:
            </p>
          </div>
          <button onClick={onClose} className="rounded-lg p-1 text-muted hover:bg-surface2 hover:text-fg" aria-label="Close">
            <Icon.Close size={18} />
          </button>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-3">
          {res.notesError && (
            <p className="py-2 text-sm text-muted">The release notes could not be read ({res.notesError}).</p>
          )}
          {!res.notesError && !notes.length && (
            <p className="py-2 text-sm text-muted">No release notes were written for these versions.</p>
          )}
          {versions.map((g) => (
            <div key={g.version} className="py-2">
              <h3 className="flex items-center gap-2 text-sm font-semibold text-fg">
                <Badge tone="primary">{g.version}</Badge>
                <span className="text-[11px] font-normal text-muted">
                  {g.items.length} change{g.items.length === 1 ? '' : 's'}
                </span>
              </h3>
              <div className="divide-y">
                {g.items.map(({ n, i }) => {
                  const on = expanded.has(i)
                  return (
                    <div key={`${n.version}-${n.title}`} className="py-3">
                      <button onClick={() => toggle(i)} className="flex w-full items-start gap-3 text-left">
                        <span className="min-w-0 flex-1">
                          <span className="block text-sm font-medium text-fg">{n.title}</span>
                          <span className="mt-0.5 block text-[11px] text-muted">{n.date}</span>
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
                              href={docURL(n.doc)}
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
          ))}
        </div>

        <div className="flex items-center justify-between gap-3 border-t px-5 py-3">
          <span className="text-xs text-muted">
            Update with <code className="rounded bg-surface2 px-1">git pull && make compose</code>
          </span>
          <Button onClick={onClose}>Close</Button>
        </div>
      </div>
    </div>
  )
}
