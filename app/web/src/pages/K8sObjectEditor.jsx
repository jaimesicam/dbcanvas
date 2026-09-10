import { useCallback, useEffect, useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import { Button, Badge, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { CopyButton } from '../components/Secret.jsx'
import { k3dApi } from '../lib/stackApi.js'

// K8sObjectEditor — the cluster's Secrets and ConfigMaps, as an editor.
//
// The cr.yaml editor beside it covers what the operator is told to do. This covers the two
// objects it is told it with, and on a Percona cluster they carry most of what people come to a
// lab to change: `<cluster>-secrets` (root, monitor, xtrabackup, replication), `internal-…` (the
// operator's own copy of them — the two disagreeing is a real, reportable failure), the `-ssl`
// chains, and the ConfigMap holding my.cnf or mongod.conf.
//
// It follows the cr.yaml editor's three rules, because they are the ones that make an editor over
// live objects usable rather than merely complete:
//
//   1. NOTHING IS APPLIED UNTIL YOU SAY SO — edits collect in a draft, the footer counts them.
//   2. CHECK BEFORE APPLY — every apply is server-side dry-run first, so an immutable object or a
//      key the Secret's own type refuses comes back as the API server's message with nothing
//      changed.
//   3. VALUES ARE NEVER ON SCREEN BY DEFAULT — a Secret's values arrive masked and are revealed
//      one key at a time. The list never carries values at all (the server does not send them),
//      so opening this tab in a screen-share shows names and sizes.
//
// A value that is not text is shown as `binary · N bytes` and cannot be edited: round-tripping a
// DER blob through a browser text box corrupts it silently, which is worse than not offering it.

// KINDS — the two the server accepts (app/k3dobjects.go's k3dObjKinds).
const KINDS = [
  { id: 'secret', label: 'Secrets' },
  { id: 'configmap', label: 'ConfigMaps' },
]

// A value worth a textarea rather than an input: a config file, a PEM chain, anything with a
// newline in it. The threshold is deliberately low — 60 characters of a one-line value is
// already unreadable in a panel-width input.
export const isMultiline = (v) => (v || '').includes('\n') || (v || '').length > 60

// objectPatchOf turns the draft back into what the server takes: the keys whose value differs
// from what was read, and the keys marked for removal. Pure and exported — this is the part that
// decides what leaves the browser, and it is worth a test that does not need a cluster.
export function objectPatchOf(obj, draft, removed) {
  const set = {}
  for (const e of obj?.entries || []) {
    if (e.binary || removed.includes(e.key)) continue
    const v = draft[e.key]
    if (v !== undefined && v !== e.value) set[e.key] = v
  }
  // Keys that are new — added in the editor, so they are in the draft but not in the object.
  for (const k of Object.keys(draft)) {
    if (removed.includes(k)) continue
    if (!(obj?.entries || []).some((e) => e.key === k)) set[k] = draft[k]
  }
  const remove = removed.filter((k) => (obj?.entries || []).some((e) => e.key === k))
  return { set, remove }
}

export const patchCount = ({ set, remove }) => Object.keys(set).length + remove.length

// A byte count that reads as one: keys here run from a 12-character password to a 6 KiB chain.
export const sizeLabel = (n) => (n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KiB`)

// ValueBox is one key's editor. A Secret's value starts masked: revealing is a deliberate act,
// and it is only needed to READ one — copying works while it is still hidden.
function ValueBox({ entry, kind, value, changed, onChange }) {
  const [show, setShow] = useState(kind !== 'secret')
  if (entry?.binary) {
    return (
      <div className="rounded-lg border border-dashed bg-bg px-2 py-1.5 text-xs text-muted">
        binary · {sizeLabel(entry.size)} — not editable here, and not sent back
      </div>
    )
  }
  const v = value ?? ''
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-1">
        {show ? (
          isMultiline(v) ? (
            <textarea rows={Math.min(18, Math.max(3, v.split('\n').length + 1))} spellCheck={false}
              className={`${inputCls} font-mono text-[11px] leading-relaxed`}
              value={v} onChange={(e) => onChange(e.target.value)} />
          ) : (
            <input className={`${inputCls} font-mono text-xs`} value={v} spellCheck={false}
              onChange={(e) => onChange(e.target.value)} />
          )
        ) : (
          <div className="flex-1 rounded-lg border bg-bg px-2 py-1.5 font-mono text-xs text-fg">
            {'•'.repeat(Math.min(44, v.length || 8))}
          </div>
        )}
        {/* Beside the box for a one-liner, stacked beside a textarea — a column of two icons
            next to a single-line input reads as a broken row. */}
        <div className={`flex shrink-0 items-center gap-0.5 ${show && isMultiline(v) ? 'flex-col self-start' : 'flex-row'}`}>
          <button title={show ? 'Hide' : 'Reveal'} onClick={() => setShow((s) => !s)}
            className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg">
            {show ? <Icon.EyeOff size={14} /> : <Icon.Eye size={14} />}
          </button>
          <CopyButton text={v} />
        </div>
      </div>
      {changed && <div className="text-[10px] font-medium text-primary">changed</div>}
    </div>
  )
}

export function K8sObjectEditor({ stackId, frame, isServer }) {
  const api = useMemo(() => (frame ? k3dApi(stackId, frame.id) : null), [stackId, frame])
  const [kind, setKind] = useState('secret')
  const [ns, setNs] = useState('')
  const [list, setList] = useState(null)
  const [sel, setSel] = useState('')
  const [obj, setObj] = useState(null)
  const [draft, setDraft] = useState({})
  const [removed, setRemoved] = useState([])
  const [adding, setAdding] = useState(null) // { key, value }
  const [query, setQuery] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const [result, setResult] = useState(null)
  const [review, setReview] = useState(false)
  // Same reason the cr.yaml editor has this: my.cnf in a 288px column is not an edit anyone can
  // make. Escape comes back to the panel.
  const [wide, setWide] = useState(false)
  useEffect(() => {
    if (!wide) return
    const esc = (e) => { if (e.key === 'Escape') setWide(false) }
    window.addEventListener('keydown', esc)
    return () => window.removeEventListener('keydown', esc)
  }, [wide])

  const loadList = useCallback(async (k, namespace) => {
    if (!api) return
    setErr('')
    setList(null)
    try {
      const r = await api.objects(k, namespace)
      setList(r)
      setNs(r.namespace)
    } catch (e) {
      setErr(e.message)
    }
  }, [api])
  useEffect(() => { if (isServer) loadList(kind, ns) }, [isServer, kind]) // eslint-disable-line react-hooks/exhaustive-deps

  const loadObject = useCallback(async (name) => {
    if (!api || !name) return
    setBusy('read')
    setResult(null)
    try {
      const o = await api.object(kind, ns, name)
      setObj(o)
      setDraft(Object.fromEntries((o.entries || []).filter((e) => !e.binary).map((e) => [e.key, e.value ?? ''])))
      setRemoved([])
      setAdding(null)
    } catch (e) {
      setErr(e.message)
    } finally {
      setBusy('')
    }
  }, [api, kind, ns])

  const patch = useMemo(() => objectPatchOf(obj, draft, removed), [obj, draft, removed])
  const pending = patchCount(patch)

  async function send(dryRun) {
    setBusy(dryRun ? 'check' : 'apply')
    setResult(null)
    try {
      const r = await api.objectPatch({ kind, namespace: ns, name: obj.name, ...patch, dryRun })
      setResult({ ok: true, message: r.message || (dryRun ? 'valid' : 'applied') })
      if (!dryRun) {
        setReview(false)
        // Re-read rather than trusting the draft: the API server is what stored it.
        if (r.object) {
          setObj(r.object)
          setDraft(Object.fromEntries((r.object.entries || []).filter((e) => !e.binary).map((e) => [e.key, e.value ?? ''])))
          setRemoved([])
        } else await loadObject(obj.name)
        loadList(kind, ns)
      }
    } catch (e) {
      setResult({ ok: false, message: e.message })
    } finally {
      setBusy('')
    }
  }

  if (!isServer) {
    return (
      <p className="rounded-lg bg-surface2 px-3 py-2 text-xs leading-snug text-muted">
        Secrets and ConfigMaps live in the cluster, and kubectl is on the
        <span className="font-medium text-fg"> server</span> node. Open that node to edit them.
      </p>
    )
  }

  const objects = (list?.objects || []).filter((o) => {
    const q = query.trim().toLowerCase()
    return !q || o.name.toLowerCase().includes(q) || (o.entries || []).some((e) => e.key.toLowerCase().includes(q))
  })
  const entries = obj ? (obj.entries || []) : []
  const addedKeys = Object.keys(draft).filter((k) => !entries.some((e) => e.key === k))

  const body = (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
          {KINDS.map((k) => (
            <button key={k.id}
              onClick={() => { setKind(k.id); setObj(null); setSel(''); setResult(null) }}
              className={`rounded-md px-2.5 py-1 text-xs font-medium transition ${kind === k.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
              {k.label}
            </button>
          ))}
        </div>
        <div className="flex items-center gap-2">
          <select className={`${inputCls} h-8 w-auto py-0 text-xs`} value={ns}
            onChange={(e) => { setNs(e.target.value); setObj(null); setSel(''); loadList(kind, e.target.value) }}>
            {(list?.namespaces || [ns]).map((n) => (
              <option key={n} value={n}>{n}{n === list?.clusterNamespace ? ' (this cluster)' : ''}</option>
            ))}
          </select>
          <Button size="sm" variant="outline" onClick={() => setWide((v) => !v)} title={wide ? 'Back to the panel (Esc)' : 'Edit full width'}>
            {wide ? <Icon.Minimize size={13} /> : <Icon.Maximize size={13} />}
          </Button>
          <Button size="sm" variant="outline" disabled={!!busy}
            onClick={() => { loadList(kind, ns); if (sel) loadObject(sel) }}>Reload</Button>
        </div>
      </div>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        The live objects, not a copy: on a Percona cluster this is where the credentials
        (<span className="font-mono">…-secrets</span>, <span className="font-mono">internal-…</span>), the TLS
        chains and the tuning file live. A write is dry-run against the API server first, and what happens
        next — a rolling restart, a credential rotation — is the operator's, which is usually the point.
      </div>

      {err && <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>}

      <div className="relative">
        <span className="absolute left-2 top-1/2 -translate-y-1/2 text-muted"><Icon.Search size={14} /></span>
        <input className={`${inputCls} pl-7`} value={query} spellCheck={false}
          placeholder={kind === 'secret' ? 'Find a secret, or a key in one…' : 'Find a ConfigMap, or a key in one…'}
          onChange={(e) => setQuery(e.target.value)} />
      </div>

      {!list && !err && <p className="text-sm text-muted">Reading the cluster…</p>}

      <div className="max-h-48 space-y-1 overflow-auto">
        {objects.map((o) => (
          <button key={o.name} onClick={() => { setSel(o.name); loadObject(o.name) }}
            className={`flex w-full items-center gap-2 rounded-lg border px-2 py-1.5 text-left text-xs transition ${sel === o.name ? 'border-primary bg-surface2' : 'hover:bg-surface2'}`}>
            <span className="min-w-0 flex-1 break-words font-medium text-fg">{o.name}</span>
            {o.immutable && <Badge tone="warning">immutable</Badge>}
            {o.type && o.type !== 'Opaque' && <span className="shrink-0 font-mono text-[10px] text-muted">{o.type}</span>}
            <span className="shrink-0 tabular-nums text-[10px] text-muted">{(o.entries || []).length} key{(o.entries || []).length === 1 ? '' : 's'}</span>
          </button>
        ))}
        {list && !objects.length && (
          <p className="text-sm text-muted">
            {query.trim() ? `Nothing matches “${query}”.` : `No ${kind === 'secret' ? 'Secrets' : 'ConfigMaps'} in ${ns}.`}
          </p>
        )}
      </div>

      {obj && (
        <div className="space-y-3 rounded-xl border p-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="min-w-0">
              <div className="break-words text-sm font-semibold text-fg">{obj.name}</div>
              <div className="text-[11px] text-muted">
                {obj.kind} · {obj.namespace}{obj.type ? ` · ${obj.type}` : ''}
              </div>
            </div>
            {obj.immutable && <Badge tone="warning">immutable — the API server will refuse an edit</Badge>}
          </div>

          {entries.map((e) => {
            const gone = removed.includes(e.key)
            return (
              <div key={e.key} className="space-y-1">
                <div className="flex items-center gap-2">
                  <span className={`min-w-0 flex-1 break-words font-mono text-xs ${gone ? 'text-muted line-through' : 'text-fg'}`}>{e.key}</span>
                  <span className="shrink-0 text-[10px] text-muted">{sizeLabel(e.size)}</span>
                  <button className="shrink-0 rounded p-1 text-muted hover:bg-surface2 hover:text-danger"
                    title={gone ? 'Keep this key' : 'Remove this key'}
                    onClick={() => setRemoved((rs) => (gone ? rs.filter((k) => k !== e.key) : [...rs, e.key]))}>
                    {gone ? <Icon.Plus size={13} /> : <Icon.Trash size={13} />}
                  </button>
                </div>
                {!gone && (
                  <ValueBox entry={e} kind={kind} value={draft[e.key]} changed={draft[e.key] !== e.value}
                    onChange={(v) => setDraft((d) => ({ ...d, [e.key]: v }))} />
                )}
              </div>
            )
          })}

          {addedKeys.map((k) => (
            <div key={k} className="space-y-1">
              <div className="flex items-center gap-2">
                <span className="min-w-0 flex-1 break-words font-mono text-xs text-primary">{k} <span className="text-[10px] text-muted">(new)</span></span>
                <button className="shrink-0 rounded p-1 text-muted hover:bg-surface2 hover:text-danger" title="Drop this key"
                  onClick={() => setDraft((d) => { const n = { ...d }; delete n[k]; return n })}>
                  <Icon.Trash size={13} />
                </button>
              </div>
              <ValueBox kind={kind} value={draft[k]} changed onChange={(v) => setDraft((d) => ({ ...d, [k]: v }))} />
            </div>
          ))}

          {adding ? (
            <div className="flex items-center gap-2">
              <input className={`${inputCls} font-mono text-xs`} autoFocus placeholder="new key" value={adding.key}
                onChange={(e) => setAdding({ key: e.target.value })} />
              <Button size="sm" disabled={!adding.key.trim() || draft[adding.key.trim()] !== undefined}
                onClick={() => { setDraft((d) => ({ ...d, [adding.key.trim()]: '' })); setAdding(null) }}>Add</Button>
              <Button size="sm" variant="ghost" onClick={() => setAdding(null)}>Cancel</Button>
            </div>
          ) : (
            <Button size="sm" variant="outline" onClick={() => setAdding({ key: '' })}>
              <Icon.Plus size={13} /> Add a key
            </Button>
          )}

          {/* The footer is the safety story, same as the cr.yaml editor's: it says how much is
              pending, and nothing leaves until one of these is pressed. */}
          <div className="sticky bottom-0 -mx-1 flex flex-wrap items-center gap-2 rounded-lg border bg-surface/95 px-3 py-2 backdrop-blur">
            <span className="text-xs text-muted">
              {pending ? `${pending} change${pending === 1 ? '' : 's'} pending` : 'No changes'}
            </span>
            <div className="ml-auto flex items-center gap-2">
              {!!pending && (
                <>
                  <Button size="sm" variant="outline" onClick={() => setReview((v) => !v)}>{review ? 'Hide' : 'Review'}</Button>
                  <Button size="sm" variant="outline" disabled={!!busy}
                    onClick={() => { setDraft(Object.fromEntries(entries.filter((e) => !e.binary).map((e) => [e.key, e.value ?? '']))); setRemoved([]) }}>
                    Discard
                  </Button>
                  <Button size="sm" variant="outline" disabled={!!busy} onClick={() => send(true)}>
                    {busy === 'check' ? 'Checking…' : 'Check'}
                  </Button>
                  <Button size="sm" disabled={!!busy} onClick={() => send(false)}>
                    {busy === 'apply' ? 'Applying…' : 'Apply'}
                  </Button>
                </>
              )}
            </div>
          </div>

          {review && !!pending && (
            <pre className="max-h-60 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">
              {reviewText(kind, patch)}
            </pre>
          )}

          {result && (
            <div className={`rounded-lg border px-3 py-2 text-xs ${result.ok ? 'border-success/30 bg-success/10 text-success' : 'border-danger/30 bg-danger/10 text-danger'}`}>
              <span className="whitespace-pre-wrap">{result.message}</span>
            </div>
          )}
        </div>
      )}
    </div>
  )

  if (wide) {
    return createPortal(
      <div className="fixed inset-0 z-50 flex flex-col bg-bg/95 p-4 backdrop-blur" onPointerDown={(e) => e.stopPropagation()}>
        <div className="mx-auto flex min-h-0 w-full max-w-4xl flex-1 flex-col overflow-auto rounded-xl border bg-surface p-4">
          {body}
        </div>
      </div>,
      document.body,
    )
  }
  return body
}

// reviewText is what "Review" shows. A ConfigMap's values are printed — reviewing a my.cnf change
// you cannot see is not a review. A SECRET'S ARE NOT: this panel's whole premise is that a
// password is revealed one key at a time and on purpose, and a review panel that prints all of
// them at once would undo that at exactly the moment somebody is sharing a screen to ask "does
// this look right?".
export function reviewText(kind, { set, remove }) {
  const lines = []
  for (const k of Object.keys(set).sort()) {
    lines.push(kind === 'secret'
      ? `${k}: •••••••• (${set[k].length} character${set[k].length === 1 ? '' : 's'})`
      : `${k}: |\n${String(set[k]).split('\n').map((l) => `  ${l}`).join('\n')}`)
  }
  for (const k of [...remove].sort()) lines.push(`${k}: <removed>`)
  return `data:\n${lines.map((l) => `  ${l}`).join('\n')}`
}

export default K8sObjectEditor
