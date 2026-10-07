import { useCallback, useEffect, useState } from 'react'
import { Icon } from './Icons.jsx'
import { useDialog } from './Dialog.jsx'
import { shareApi } from '../lib/shareApi.js'

// Recordings — a host's screen recordings of their shared sessions
// (app/sharerecord.go): download one and its chat transcript, play it, move the date
// it is purged on, or purge it now. The transcript is kept with the recording, so it
// can be downloaded for as long as the video, after the session's own records go. Every recording is purged on its date whatever happens; this is where
// that date is seen and changed.
//
// sessionId narrows the list to one session's (the panel after a session ends).

function fmtSize(n) {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  return `${Math.max(1, Math.round(n / 1024))} KB`
}

function fmtLength(ms) {
  const s = Math.round(ms / 1000)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = String(s % 60).padStart(2, '0')
  return h ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`
}

const day = (iso) => (iso ? iso.slice(0, 10) : '')
const tomorrow = () => new Date(Date.now() + 86400000).toISOString().slice(0, 10)

export default function Recordings({ sessionId = null, compact = false }) {
  const [list, setList] = useState(null)
  const [err, setErr] = useState('')
  const [dialog, ask] = useDialog()

  const load = useCallback(() => shareApi.recordings()
    .then((l) => setList(Array.isArray(l) ? l : []))
    .catch((e) => setErr(e.message)), [])
  useEffect(() => { load() }, [load])

  const shown = (list || []).filter((r) => sessionId == null || r.sessionId === sessionId)
  // A recording still being made or finished is polled until it is ready.
  const busy = shown.some((r) => r.state !== 'ready')
  useEffect(() => {
    if (!busy) return undefined
    const t = setInterval(load, 3000)
    return () => clearInterval(t)
  }, [busy, load])

  const update = async (r, patch) => {
    setErr('')
    try { await shareApi.updateRecording(r.id, patch); load() } catch (e) { setErr(e.message) }
  }
  const purge = async (r) => {
    const yes = await ask.confirm({
      title: 'Purge this recording?',
      body: `${r.title} is deleted now, file and all. This cannot be undone.`,
      confirmLabel: 'Purge', danger: true,
    })
    if (!yes) return
    try { await shareApi.deleteRecording(r.id); load() } catch (e) { setErr(e.message) }
  }
  const rename = async (r) => {
    const title = await ask.prompt({ title: 'Rename the recording', defaultValue: r.title, confirmLabel: 'Rename' })
    if (title && title.trim() && title !== r.title) update(r, { title: title.trim() })
  }

  if (list === null) return err ? <div className="text-xs text-danger">{err}</div> : null
  if (!shown.length) {
    return compact ? null : <div className="text-xs text-muted">No recordings. Start one from the session panel while you host a session.</div>
  }
  return (
    <div className="space-y-1.5 text-xs">
      {shown.map((r) => (
        <div key={r.id} className="rounded-lg border bg-bg px-2 py-1.5">
          <div className="flex items-center gap-1.5">
            <Icon.Record size={13} className={r.state === 'recording' ? 'animate-pulse text-danger' : 'text-muted'} />
            <button className="min-w-0 flex-1 truncate text-left font-medium hover:underline" title="Rename" onClick={() => rename(r)}>{r.title}</button>
            <button title="Purge now" onClick={() => purge(r)} className="rounded p-0.5 text-muted hover:bg-danger/10 hover:text-danger"><Icon.Trash size={13} /></button>
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-1.5 text-muted">
            <span>{new Date(r.startedAt).toLocaleString()}</span>
            {r.state === 'ready' && <><span>·</span><span>{fmtLength(r.durationMs)}</span></>}
            <span>·</span><span>{fmtSize(r.size)}</span>
            <span>·</span>
            <label className="inline-flex items-center gap-1" title="The recording is deleted on this date">
              purged on
              <input
                type="date" min={tomorrow()} value={day(r.purgeAt)}
                onChange={(e) => e.target.value && update(r, { purgeAt: e.target.value })}
                className="rounded border bg-surface px-1 py-0 text-xs text-fg"
              />
            </label>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-x-1.5">
            {r.state === 'ready' ? (
              <>
                <a className="text-primary hover:underline" href={shareApi.recordingURL(r.id)}>Download video</a>
                <span className="text-muted">·</span>
                <a className="text-primary hover:underline" href={shareApi.recordingURL(r.id, true)} target="_blank" rel="noreferrer">Play</a>
                <span className="text-muted">·</span>
                <a className="text-primary hover:underline" href={shareApi.recordingTranscriptURL(r.id)} title="The session's chat and events, as text">Transcript</a>
                <a className="text-muted hover:text-primary hover:underline" href={shareApi.recordingTranscriptURL(r.id, 'json')} title="The transcript as JSON">JSON</a>
              </>
            ) : (
              <span className="text-muted">{r.state === 'recording' ? 'Recording…' : 'Finishing…'}</span>
            )}
          </div>
        </div>
      ))}
      {err && <div className="text-danger">{err}</div>}
      {dialog}
    </div>
  )
}
