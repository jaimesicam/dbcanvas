import { useEffect, useState } from 'react'
import { Icon } from './Icons.jsx'
import { Button } from './ui.jsx'
import { useSession } from '../session/SessionProvider.jsx'
import { useSettings } from '../settings/SettingsProvider.jsx'
import { shareApi } from '../lib/shareApi.js'
import Recordings from './Recordings.jsx'

// ShareDialog — a stack owner starting a shared session (app/share.go), and the
// transcripts of the ones before, and the host's recordings of them, to download
// until each one's purge date.

const DURATIONS = [15, 30, 60, 90, 120]

function loopback(host) {
  return /^(localhost|127\.|\[?::1\]?)/.test(host)
}

export default function ShareDialog({ stack, onClose }) {
  const session = useSession()
  const { system } = useSettings()
  const maxMin = system.maxGuestMinutes || 120
  const choices = DURATIONS.filter((m) => m <= maxMin)
  const [minutes, setMinutes] = useState(choices.includes(60) ? 60 : choices.at(-1) || maxMin)
  const [hide, setHide] = useState(false)
  const [mirror, setMirror] = useState(true)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [past, setPast] = useState([])

  useEffect(() => {
    if (!stack) return
    shareApi.transcripts(stack.id).then((l) => setPast(Array.isArray(l) ? l : [])).catch(() => {})
  }, [stack])

  const unreachable = !system.publicUrl && loopback(location.hostname)
  const start = async () => {
    setErr(''); setBusy(true)
    try {
      await session.start(stack?.id ?? null, Number(minutes), hide, mirror)
      onClose()
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onClick={onClose}>
      <div className="w-full max-w-md rounded-2xl border bg-surface p-5 shadow-xl" onClick={(e) => e.stopPropagation()}>
        <div className="mb-3 flex items-center gap-2">
          <Icon.Share size={18} />
          <h2 className="flex-1 text-base font-semibold">Share a live session</h2>
          <button onClick={onClose} className="rounded p-1 text-muted hover:bg-surface2"><Icon.Close size={14} /></button>
        </div>

        {!system.allowGuestSessions ? (
          <p className="text-sm text-muted">
            Shared sessions are turned off on this installation. An administrator can turn them on in
            <span className="font-medium text-fg"> Settings → Shared sessions</span>.
          </p>
        ) : session.active ? (
          <p className="text-sm text-muted">A session is already running — its panel is open beside the workspace.</p>
        ) : (
          <div className="space-y-3 text-sm">
            <p className="text-muted">
              Anyone with the link waits in a lobby until you admit them. The session is your whole workspace —
              every page, not only {stack ? 'this stack' : 'the one you are on'}. Guests see what you do and chat;
              one you give control to can do anything you can here, except reach your account.
            </p>
            <label className="flex items-center gap-2">
              <span className="w-24 text-muted">Lasts</span>
              <select value={minutes} onChange={(e) => setMinutes(e.target.value)} className="rounded-lg border bg-bg px-2 py-1">
                {choices.map((m) => <option key={m} value={m}>{m < 60 ? `${m} minutes` : `${m / 60} hour${m === 60 ? '' : 's'}`}</option>)}
              </select>
            </label>
            <label className="flex items-start gap-2">
              <input type="checkbox" checked={hide} onChange={(e) => setHide(e.target.checked)} className="mt-1" />
              <span>
                Hide secrets
                <span className="block text-xs text-muted">
                  Passwords are masked in everything guests are sent. A guest with control can still read a config file in a terminal.
                </span>
              </span>
            </label>
            <label className="flex items-start gap-2">
              <input type="checkbox" checked={mirror} onChange={(e) => setMirror(e.target.checked)} className="mt-1" />
              <span>
                Mirror everything
                <span className="block text-xs text-muted">
                  Everyone sees exactly the screen of whoever has control — the menus they open, the windows they drag,
                  the dialogs, what they type — so nobody gets lost. Off, each guest only follows to the same page.
                  You can switch it in the session panel at any time.
                </span>
              </span>
            </label>
            {unreachable && (
              <div className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
                This page is on {location.host}, so the link will be too — guests on other machines cannot open it.
                Set PUBLIC_URL (and APP_HOST) in .env to an address they can reach.
              </div>
            )}
            {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-xs text-danger">{err}</div>}
            <div className="flex justify-end gap-2">
              <Button variant="outline" onClick={onClose}>Cancel</Button>
              <Button variant="primary" onClick={start} disabled={busy}>{busy ? 'Starting…' : 'Start and get the link'}</Button>
            </div>
          </div>
        )}

        <div className="mt-4 border-t pt-3">
          <div className="mb-1.5 text-xs font-medium text-muted">
            Your recordings
            <span className="font-normal">
              {' · '}each is purged on its date — {system.recordingRetentionDays || 30} days after it starts unless you move it
            </span>
          </div>
          <div className="max-h-64 overflow-y-auto">
            <Recordings />
          </div>
        </div>

        {past.length > 0 && (
          <div className="mt-4 border-t pt-3">
            <div className="mb-1.5 text-xs font-medium text-muted">
              Earlier sessions on this stack
              <span className="font-normal">
                {' · '}{system.sessionRetentionDays ? `kept ${system.sessionRetentionDays} days after they end` : 'kept until the stack is deleted'}
              </span>
            </div>
            <div className="max-h-40 space-y-1 overflow-y-auto text-xs">
              {past.map((p) => (
                <div key={p.id} className="flex items-center gap-2">
                  <span className="flex-1 text-muted">
                    {new Date(p.createdAt).toLocaleString()} · {p.messages} line{p.messages === 1 ? '' : 's'}
                    {p.live ? ' · live' : p.endedReason ? ` · ${p.endedReason}` : ''}
                  </span>
                  <a className="text-primary hover:underline" href={shareApi.transcriptURL(p.id)}>Transcript</a>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
