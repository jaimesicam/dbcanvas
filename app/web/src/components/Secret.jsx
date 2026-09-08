import { useState } from 'react'
import { Icon } from './Icons.jsx'

// Secret — the one way a credential is shown in DBCanvas: masked, with a reveal toggle and a copy
// button. Passwords, tokens, keys and the connection URIs that embed them all go through this, so
// a node's Credentials tab can be opened in a screen-share or a demo without leaking anything.
//
// Copy works while the value is still masked — that is the common case (paste it into a client),
// and it means revealing is only ever needed to *read* a secret, not to use one.

export function CopyButton({ text, size = 14 }) {
  const [done, setDone] = useState(false)
  return (
    <button title="Copy"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
      {done ? <Icon.Check size={size} /> : <Icon.Copy size={size} />}
    </button>
  )
}

// SecretValue is the boxed value itself (no label) — same box the plain rows use, so a masked row
// and a clear one line up.
export function SecretValue({ value }) {
  const [show, setShow] = useState(false)
  const v = value ?? ''
  if (v === '') {
    return (
      <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">—</span>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-1 rounded-lg border bg-bg px-2 py-1.5">
      <span className="min-w-0 flex-1 truncate font-mono text-xs text-fg">
        {show ? v : '•'.repeat(Math.min(44, String(v).length))}
      </span>
      <button title={show ? 'Hide' : 'Reveal'} onClick={() => setShow((s) => !s)}
        className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
        {show ? <Icon.EyeOff size={14} /> : <Icon.Eye size={14} />}
      </button>
      <CopyButton text={String(v)} />
    </div>
  )
}

// SecretInline is the same thing for the compact "label … value" rows some node panels use
// (Keycloak, VNC, Valkey), where a boxed value would not fit.
export function SecretInline({ value }) {
  const [show, setShow] = useState(false)
  const v = value ?? ''
  if (v === '') return <span className="font-mono text-xs text-fg">—</span>
  return (
    <span className="flex min-w-0 items-center gap-1">
      <span className="min-w-0 truncate font-mono text-xs text-fg">
        {show ? v : '•'.repeat(Math.min(24, String(v).length))}
      </span>
      <button title={show ? 'Hide' : 'Reveal'} onClick={() => setShow((s) => !s)}
        className="shrink-0 rounded p-0.5 text-muted hover:text-fg">
        {show ? <Icon.EyeOff size={13} /> : <Icon.Eye size={13} />}
      </button>
      <CopyButton text={String(v)} size={13} />
    </span>
  )
}

// SecretRow is a labelled masked value — the shape every Credentials tab uses.
export default function SecretRow({ label, value }) {
  return (
    <div>
      {label && <div className="text-xs text-muted">{label}</div>}
      <SecretValue value={value} />
    </div>
  )
}
