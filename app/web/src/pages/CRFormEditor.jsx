import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Button, Badge, Toggle, inputCls } from '../components/ui.jsx'
import { Icon } from '../components/Icons.jsx'
import { Help } from '../components/Tooltip.jsx'
import { k3dApi } from '../lib/stackApi.js'

// CRFormEditor — cr.yaml as a form, generated from the operator's own CustomResourceDefinition.
//
// The thing being edited is the live custom resource, not the file: the file was applied minutes
// ago and the object is what the operator reconciles. Every control here comes from the CRD the
// cluster is actually running (app/k3dcrform.go builds the model), so a field this operator
// release does not have is not offered, and one it has just added is — without anybody updating
// a form.
//
// The three decisions that make it usable rather than merely complete:
//
//   1. NOTHING IS APPLIED UNTIL YOU SAY SO. Edits accumulate in a draft, the footer counts them,
//      and Review shows the exact merge patch that will be sent. A form that writes on every
//      keystroke cannot be used to think with — and this is a form whose fields restart database
//      pods.
//   2. CHECK BEFORE APPLY. The patch is dry-run against the API server, which validates it
//      against the same CRD this form was built from. A value the operator would refuse comes
//      back as its own message, with nothing changed.
//   3. THE SEARCH IS THE NAVIGATION. A Percona CRD has hundreds of fields across a dozen
//      sections; nobody browses to `backup.pitr.timeBetweenUploads`. Typing filters every group
//      at once and shows each match with its full path.
//
// Fields the form cannot usefully render — affinity, tolerations, sidecars, anything free-form —
// arrive marked `raw` and get a JSON box holding their current value, so the editor still covers
// the whole custom resource rather than the easy half of it.

// ---------------------------------------------------------------- small helpers

// getAt / setAt / delAt address the draft by the CRD's dotted path. They copy the objects they
// pass through, so a change never mutates the copy the diff is computed against.
export function getAt(obj, path) {
  let cur = obj
  for (const k of path.split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined
    cur = cur[k]
  }
  return cur
}

export function setAt(obj, path, value) {
  const keys = path.split('.')
  const out = Array.isArray(obj) ? [...obj] : { ...(obj || {}) }
  let cur = out
  for (let i = 0; i < keys.length - 1; i++) {
    const k = keys[i]
    const next = cur[k]
    cur[k] = Array.isArray(next) ? [...next] : { ...(next || {}) }
    cur = cur[k]
  }
  cur[keys[keys.length - 1]] = value
  return out
}

export function delAt(obj, path) {
  const keys = path.split('.')
  const out = Array.isArray(obj) ? [...obj] : { ...(obj || {}) }
  let cur = out
  for (let i = 0; i < keys.length - 1; i++) {
    const k = keys[i]
    if (cur[k] == null || typeof cur[k] !== 'object') return out
    cur[k] = Array.isArray(cur[k]) ? [...cur[k]] : { ...cur[k] }
    cur = cur[k]
  }
  delete cur[keys[keys.length - 1]]
  return out
}

const isObj = (v) => v != null && typeof v === 'object' && !Array.isArray(v)

// crPatch is the merge patch that turns `orig` into `draft`, and it is the whole contract with
// the server: what it returns is exactly what gets sent.
//
// Three rules, which are JSON merge patch's own:
//   - a key that is gone is sent as null (that is how a merge patch deletes);
//   - an array is replaced wholesale, never merged element-wise — merging lists by position is
//     how you end up with a schedule nobody wrote;
//   - an object recurses, and drops out of the patch entirely when nothing inside it changed,
//     so the patch names only what the user touched.
export function crPatch(orig, draft) {
  const patch = {}
  const keys = new Set([...Object.keys(orig || {}), ...Object.keys(draft || {})])
  for (const k of keys) {
    const a = orig?.[k]
    const b = draft?.[k]
    if (b === undefined) {
      if (a !== undefined) patch[k] = null
      continue
    }
    if (isObj(a) && isObj(b)) {
      const sub = crPatch(a, b)
      if (Object.keys(sub).length) patch[k] = sub
      continue
    }
    if (JSON.stringify(a) !== JSON.stringify(b)) patch[k] = b
  }
  return patch
}

// changedPaths lists the dotted paths a patch touches, for the footer count and for marking the
// fields themselves. A null (a deletion) is a change like any other.
export function changedPaths(patch, prefix = '') {
  const out = []
  for (const [k, v] of Object.entries(patch || {})) {
    const p = prefix ? `${prefix}.${k}` : k
    if (isObj(v) && Object.keys(v).length) out.push(...changedPaths(v, p))
    else out.push(p)
  }
  return out
}

// yamlish renders a patch the way cr.yaml reads, for the review panel. Not a YAML library — the
// app has none, and this only ever prints a merge patch: scalars, maps and lists of those.
export function yamlish(value, indent = 0) {
  const pad = '  '.repeat(indent)
  if (value === null) return 'null'
  if (Array.isArray(value)) {
    if (!value.length) return '[]'
    return '\n' + value.map((v) => `${pad}- ${yamlish(v, indent + 1).replace(/^\n/, '')}`).join('\n')
  }
  if (isObj(value)) {
    const keys = Object.keys(value)
    if (!keys.length) return '{}'
    return '\n' + keys.map((k) => `${pad}${k}:${(() => {
      const r = yamlish(value[k], indent + 1)
      return r.startsWith('\n') ? r : ` ${r}`
    })()}`).join('\n')
  }
  if (typeof value === 'string' && (value === '' || /[:#\n]/.test(value))) return JSON.stringify(value)
  return String(value)
}

// matchField reports whether a field (or anything under it) matches the search. Matching on the
// path, not only the name, is what makes "pitr" find `backup.pitr.timeBetweenUploads` and
// "storage" find every storageName in the document.
export function matchField(field, q) {
  if (!q) return true
  const needle = q.toLowerCase()
  if (field.path.toLowerCase().includes(needle)) return true
  if ((field.help || field.desc || '').toLowerCase().includes(needle)) return true
  return (field.fields || []).some((f) => matchField(f, needle)) ||
    (field.element || []).some((f) => matchField(f, needle))
}

// ---------------------------------------------------------------- controls

function FieldLabel({ field, changed, onReset, right }) {
  const doc = field.desc || field.help
  return (
    <div className="flex items-start justify-between gap-2">
      <span className="flex min-w-0 items-center gap-1">
        <span className={`truncate text-xs font-medium ${changed ? 'text-primary' : 'text-fg'}`}>{field.name}</span>
        {field.required && <span className="text-[10px] text-danger" title="required">*</span>}
        {doc && <Help text={doc} />}
        {changed && (
          <button onClick={onReset} title="Undo this change"
            className="rounded px-1 text-[10px] font-medium text-primary hover:bg-surface2">changed ⟲</button>
        )}
      </span>
      {right}
    </div>
  )
}

// TypeHint is the line under a control: what the CRD says this field is. With no descriptions in
// the Percona CRDs (see app/k3dcrhelp.go) this is often the only machine-truth on screen, so it
// carries the type, the default and the bounds rather than being decoration.
function TypeHint({ field }) {
  const bits = [field.type === 'array' ? `array of ${field.items || 'items'}` : field.type]
  if (field.format) bits.push(field.format)
  if (field.min != null || field.max != null) bits.push(`${field.min ?? '−∞'}…${field.max ?? '∞'}`)
  if (field.default !== undefined) bits.push(`default ${JSON.stringify(field.default)}`)
  return <span className="mt-0.5 block text-[10px] text-muted">{bits.join(' · ')}</span>
}

// Scalar is one leaf control: a switch, a select, a number or a text box, chosen from the schema.
function Scalar({ field, value, onChange, onUnset }) {
  const set = value !== undefined
  const common = `${inputCls} ${set ? '' : 'text-muted'}`
  const clear = set && !field.required
    ? (
      <button onClick={onUnset} title="Unset this field (removes it from the custom resource)"
        className="rounded px-1 text-[10px] text-muted hover:bg-surface2 hover:text-danger">unset</button>
    ) : null

  if (field.type === 'boolean') {
    return (
      <div className="flex items-center gap-2">
        <Toggle checked={value === true} onChange={(v) => onChange(v)} />
        <span className="text-xs text-muted">{value === true ? 'true' : value === false ? 'false' : 'not set'}</span>
        {clear}
      </div>
    )
  }
  if (field.enum?.length) {
    return (
      <div className="flex items-center gap-2">
        <select className={common} value={value ?? ''} onChange={(e) => (e.target.value === '' ? onUnset() : onChange(e.target.value))}>
          <option value="">not set{field.default !== undefined ? ` (${field.default})` : ''}</option>
          {field.enum.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
        {clear}
      </div>
    )
  }
  if (field.type === 'integer' || field.type === 'number') {
    return (
      <div className="flex items-center gap-2">
        <input type="number" className={`${common} w-40`} value={value ?? ''}
          min={field.min ?? undefined} max={field.max ?? undefined}
          placeholder={field.default !== undefined ? String(field.default) : 'not set'}
          onChange={(e) => (e.target.value === '' ? onUnset() : onChange(Number(e.target.value)))} />
        {clear}
      </div>
    )
  }
  // A long string is a config file — my.cnf, mongod.conf — and a one-line input is unusable for
  // it. The schema does not say which strings are long, so the value does: anything with a
  // newline in it, and the fields whose name says so.
  const multiline = /configuration|hookScript|caBundle|script/i.test(field.name) || String(value || '').includes('\n')
  if (multiline) {
    return (
      <div className="space-y-1">
        <textarea className={`${common} min-h-[120px] font-mono text-[11px]`} value={value ?? ''}
          spellCheck={false}
          onChange={(e) => (e.target.value === '' ? onUnset() : onChange(e.target.value))} />
        <div className="flex justify-end">{clear}</div>
      </div>
    )
  }
  return (
    <div className="flex items-center gap-2">
      <input className={common} value={value ?? ''} spellCheck={false}
        placeholder={field.default !== undefined ? String(field.default) : 'not set'}
        onChange={(e) => (e.target.value === '' ? onUnset() : onChange(e.target.value))} />
      {clear}
    </div>
  )
}

// RawBox edits a subtree the form does not render — affinity, sidecars, anything free-form — as
// JSON. It parses on every keystroke so the error appears while you are typing, but only commits
// a value that parses: a half-typed object must never become the draft.
function RawBox({ field, value, onChange, onUnset }) {
  const [text, setText] = useState(() => (value === undefined ? '' : JSON.stringify(value, null, 2)))
  const [err, setErr] = useState('')
  const last = useRef(JSON.stringify(value))
  useEffect(() => {
    // Re-sync when the value changes underneath us (a reload, or an undo), never on our own edit.
    const now = JSON.stringify(value)
    if (now !== last.current) {
      last.current = now
      setText(value === undefined ? '' : JSON.stringify(value, null, 2))
      setErr('')
    }
  }, [value])
  const why = {
    opaque: 'Kubernetes scheduling and security plumbing — the same shape in every section, and long. Edited as JSON here.',
    large: 'Too many fields to render as a form without burying everything else.',
    deep: 'Nested deeper than the generated form goes.',
    'free-form': 'The CRD does not declare a shape for this, so there is nothing to generate a form from.',
  }[field.rawWhy] || ''
  return (
    <div className="space-y-1">
      <textarea
        className={`${inputCls} font-mono text-[11px] ${text.trim() ? 'min-h-[110px]' : 'min-h-[56px]'} ${err ? 'border-danger' : ''}`}
        value={text} spellCheck={false} placeholder="not set"
        onChange={(e) => {
          const t = e.target.value
          setText(t)
          if (t.trim() === '') { setErr(''); last.current = undefined; onUnset(); return }
          try {
            const parsed = JSON.parse(t)
            setErr('')
            last.current = JSON.stringify(parsed)
            onChange(parsed)
          } catch (ex) { setErr(ex.message) }
        }} />
      {err
        ? <span className="block text-[10px] text-danger">{err} — the last valid value is still in the draft</span>
        : why && <span className="block text-[10px] text-muted">{why}</span>}
    </div>
  )
}

// StringList is an array of scalars as chips. Arrays are replaced wholesale by a merge patch, so
// this always writes the entire list — which is also what makes removing an element work.
function StringList({ value, onChange, onUnset }) {
  const list = Array.isArray(value) ? value : []
  const [draft, setDraft] = useState('')
  const commit = () => {
    const v = draft.trim()
    if (!v) return
    onChange([...list, v])
    setDraft('')
  }
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap gap-1">
        {list.map((v, i) => (
          <span key={`${v}-${i}`} className="inline-flex items-center gap-1 rounded-full bg-surface2 px-2 py-0.5 text-xs text-fg">
            {String(v)}
            <button className="text-muted hover:text-danger"
              onClick={() => { const next = list.filter((_, j) => j !== i); next.length ? onChange(next) : onUnset() }}>×</button>
          </span>
        ))}
        {!list.length && <span className="text-xs text-muted">not set</span>}
      </div>
      <div className="flex gap-1">
        <input className={`${inputCls} h-8`} value={draft} placeholder="add an entry…" spellCheck={false}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); commit() } }} />
        <Button size="sm" variant="outline" onClick={commit}>Add</Button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------- structure

// Group is a collapsible object. Sections start open, everything nested starts closed: the whole
// spec expanded is thousands of rows, and the point of the search box is that you do not scroll
// to a field anyway.
function Group({ field, value, path, ctx, depth = 0, defaultOpen = false }) {
  const changedInside = ctx.changed.some((p) => p === path || p.startsWith(path + '.'))
  const [open, setOpen] = useState(defaultOpen || changedInside)
  useEffect(() => { if (ctx.query) setOpen(true) }, [ctx.query])
  const kids = (field.fields || []).filter((f) => matchField(f, ctx.query))
  if (!kids.length && ctx.query) return null
  return (
    <div className={depth === 0 ? '' : 'rounded-lg border bg-surface/50'}>
      <button onClick={() => setOpen(!open)}
        className={`flex w-full items-center gap-1.5 px-2 py-1.5 text-left ${depth === 0 ? '' : 'hover:bg-surface2'}`}>
        <span className={`text-muted transition-transform ${open ? '' : '-rotate-90'}`}><Icon.Chevron size={13} /></span>
        <span className={`text-xs font-semibold ${changedInside ? 'text-primary' : 'text-fg'}`}>{field.name}</span>
        {(field.desc || field.help) && <Help text={field.desc || field.help} />}
        <span className="ml-auto text-[10px] text-muted">{kids.length} field{kids.length === 1 ? '' : 's'}</span>
      </button>
      {open && (
        <div className={`space-y-2.5 ${depth === 0 ? 'pt-1' : 'border-t px-2.5 py-2'}`}>
          {kids.map((f) => <Node key={f.path} field={f} path={`${path}.${f.name}`} ctx={ctx} depth={depth + 1} />)}
        </div>
      )}
    </div>
  )
}

// ObjectList is an array of objects — `backup.schedule`, `pxc.replicationChannels`. Each element
// is a card with the element schema rendered into it; the list is written back whole.
function ObjectList({ field, value, path, ctx, depth }) {
  const list = Array.isArray(value) ? value : []
  const element = field.element?.[0]
  const write = (next) => (next.length ? ctx.set(path, next) : ctx.unset(path))
  return (
    <div className="space-y-2">
      {list.map((item, i) => (
        <div key={i} className="rounded-lg border bg-surface/50 p-2">
          <div className="mb-1.5 flex items-center justify-between">
            <span className="text-[11px] font-medium text-muted">
              {field.name}[{i}]{item?.name ? ` · ${item.name}` : ''}
            </span>
            <button className="rounded px-1.5 text-xs text-muted hover:bg-surface2 hover:text-danger"
              onClick={() => write(list.filter((_, j) => j !== i))}>Remove</button>
          </div>
          {element?.fields?.length
            ? (
              <div className="space-y-2.5">
                {element.fields.filter((f) => matchField(f, ctx.query)).map((f) => (
                  <Node key={f.path} field={f} path={`${path}.${i}.${f.name}`} ctx={ctx} depth={depth + 1} />
                ))}
              </div>
            )
            : <RawBox field={{ ...field, rawWhy: 'free-form' }} value={item}
              onChange={(v) => write(list.map((x, j) => (j === i ? v : x)))}
              onUnset={() => write(list.filter((_, j) => j !== i))} />}
        </div>
      ))}
      <Button size="sm" variant="outline" onClick={() => write([...list, {}])}>
        <Icon.Plus size={13} /> Add {field.name.replace(/s$/, '')}
      </Button>
    </div>
  )
}

// NamedMap is an object whose keys the user chooses — `backup.storages` is the one everybody
// meets, where the key is the storage name that `pitr.storageName` and a schedule refer to. So
// the key is editable-by-creation and shown prominently: getting it wrong is how a custom
// resource is rejected for naming a storage that does not exist.
function NamedMap({ field, value, path, ctx, depth }) {
  const entries = isObj(value) ? Object.entries(value) : []
  const element = field.element?.[0]
  const [name, setName] = useState('')
  return (
    <div className="space-y-2">
      {entries.map(([key, item]) => (
        <div key={key} className="rounded-lg border bg-surface/50 p-2">
          <div className="mb-1.5 flex items-center justify-between">
            <span className="font-mono text-[11px] font-medium text-fg">{key}</span>
            <button className="rounded px-1.5 text-xs text-muted hover:bg-surface2 hover:text-danger"
              onClick={() => ctx.unset(`${path}.${key}`)}>Remove</button>
          </div>
          {element?.fields?.length
            ? (
              <div className="space-y-2.5">
                {element.fields.filter((f) => matchField(f, ctx.query)).map((f) => (
                  <Node key={f.path} field={f} path={`${path}.${key}.${f.name}`} ctx={ctx} depth={depth + 1} />
                ))}
              </div>
            )
            // A map of scalars — `resources.limits: {cpu: 500m, memory: 1G}` is the one in every
            // section — is a value box per key, not JSON. The quantities have no `type` in the
            // schema at all (they are int-or-string), which is what used to send them here as
            // free-form text.
            : element && !element.raw && element.type !== 'object'
              ? <Scalar field={{ ...element, name: key }} value={item}
                onChange={(v) => ctx.set(`${path}.${key}`, v)} onUnset={() => ctx.unset(`${path}.${key}`)} />
              : <RawBox field={{ ...field, rawWhy: 'free-form' }} value={item}
                onChange={(v) => ctx.set(`${path}.${key}`, v)} onUnset={() => ctx.unset(`${path}.${key}`)} />}
        </div>
      ))}
      <div className="flex gap-1">
        <input className={`${inputCls} h-8`} value={name} placeholder={`new ${field.name.replace(/s$/, '')} name…`}
          spellCheck={false} onChange={(e) => setName(e.target.value)} />
        <Button size="sm" variant="outline" disabled={!name.trim() || entries.some(([k]) => k === name.trim())}
          onClick={() => { ctx.set(`${path}.${name.trim()}`, {}); setName('') }}>Add</Button>
      </div>
    </div>
  )
}

// Node dispatches one field to the control its schema calls for.
function Node({ field, path, ctx, depth }) {
  if (!matchField(field, ctx.query)) return null
  const value = getAt(ctx.draft, path)
  const changed = ctx.changed.includes(path) || ctx.changed.some((p) => p.startsWith(path + '.'))

  if (field.type === 'object' && field.map) {
    return (
      <div className="space-y-1">
        <FieldLabel field={field} changed={changed} onReset={() => ctx.reset(path)} />
        <NamedMap field={field} value={value} path={path} ctx={ctx} depth={depth} />
      </div>
    )
  }
  if (field.type === 'object' && field.fields?.length) {
    return <Group field={field} value={value} path={path} ctx={ctx} depth={depth} />
  }
  if (field.type === 'array' && field.items === 'object') {
    return (
      <div className="space-y-1">
        <FieldLabel field={field} changed={changed} onReset={() => ctx.reset(path)} />
        <ObjectList field={field} value={value} path={path} ctx={ctx} depth={depth} />
      </div>
    )
  }
  return (
    <div className="space-y-1">
      <FieldLabel field={field} changed={changed} onReset={() => ctx.reset(path)} />
      {field.raw || field.type === 'raw'
        ? <RawBox field={field} value={value} onChange={(v) => ctx.set(path, v)} onUnset={() => ctx.unset(path)} />
        : field.type === 'array'
          ? <StringList value={value} onChange={(v) => ctx.set(path, v)} onUnset={() => ctx.unset(path)} />
          : <Scalar field={field} value={value} onChange={(v) => ctx.set(path, v)} onUnset={() => ctx.unset(path)} />}
      <TypeHint field={field} />
      <span className="block font-mono text-[10px] text-muted/70">spec.{path}</span>
    </div>
  )
}

// ---------------------------------------------------------------- the editor

// `preloaded` short-circuits the fetch with a form model that is already in hand. It is what the
// render tests drive the whole editor with — a form generated from a schema is exactly the thing
// that cannot be checked by rendering its empty state — and it costs one branch.
export function CRFormEditor({ stackId, frame, isServer, preloaded }) {
  const api = useMemo(() => (frame ? k3dApi(stackId, frame.id) : null), [stackId, frame])
  const [schema, setSchema] = useState(preloaded || null)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(!preloaded)
  const [draft, setDraft] = useState(preloaded?.spec || {})
  const [group, setGroup] = useState(preloaded?.groups?.[0]?.id || '')
  const [query, setQuery] = useState('')
  const [review, setReview] = useState(false)
  const [busy, setBusy] = useState('')
  const [result, setResult] = useState(null)
  // The properties panel is a column; this is a form over a CRD with hundreds of fields in it.
  // Expanding to the full window is not decoration — at panel width a nested group is four words
  // per line, and the review panel (which is the safety story) cannot be read at all.
  const [wide, setWide] = useState(false)
  useEffect(() => {
    if (!wide) return
    const esc = (e) => { if (e.key === 'Escape') setWide(false) }
    window.addEventListener('keydown', esc)
    return () => window.removeEventListener('keydown', esc)
  }, [wide])

  const load = useMemo(() => async () => {
    if (!api) return
    setLoading(true)
    setErr('')
    try {
      const s = await api.cr()
      setSchema(s)
      setDraft(s.spec || {})
      setGroup((g) => g || s.groups?.[0]?.id || '')
    } catch (e) {
      setErr(e.message)
    } finally {
      setLoading(false)
    }
  }, [api])
  useEffect(() => { if (!preloaded) load() }, [load, preloaded])

  const orig = schema?.spec || {}
  const patch = useMemo(() => crPatch(orig, draft), [orig, draft])
  const changed = useMemo(() => changedPaths(patch), [patch])

  const ctx = useMemo(() => ({
    draft,
    query: query.trim().toLowerCase(),
    changed,
    set: (path, v) => setDraft((d) => setAt(d, path, v)),
    unset: (path) => setDraft((d) => delAt(d, path)),
    // Undo is per-field, and it means "put back what the server has" — including putting back a
    // field that was unset, which is why it is not simply a delete.
    reset: (path) => setDraft((d) => {
      const was = getAt(orig, path)
      return was === undefined ? delAt(d, path) : setAt(d, path, was)
    }),
  }), [draft, query, changed, orig])

  async function send(dryRun) {
    setBusy(dryRun ? 'check' : 'apply')
    setResult(null)
    try {
      const r = await api.crPatch(patch, dryRun)
      setResult({ ok: true, message: r.message || (dryRun ? 'valid' : 'applied') })
      if (!dryRun) {
        // Re-read rather than trusting the draft: the API server defaults and prunes on the way
        // in, so what it stored is not always what was sent.
        if (r.spec) { setSchema((s) => ({ ...s, spec: r.spec, status: r.status ?? s.status })); setDraft(r.spec) }
        else await load()
        setReview(false)
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
        The custom resource lives in the cluster, and kubectl is on the
        <span className="font-medium text-fg"> server</span> node. Open that node to edit it.
      </p>
    )
  }
  if (loading) return <p className="text-sm text-muted">Reading the CustomResourceDefinition…</p>
  if (err) {
    return (
      <div className="space-y-2">
        <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">{err}</div>
        <Button size="sm" variant="outline" onClick={load}>Try again</Button>
      </div>
    )
  }
  if (!schema) return null

  const groups = schema.groups || []
  const active = groups.find((g) => g.id === group) || groups[0]
  const byName = Object.fromEntries((schema.sections || []).map((f) => [f.name, f]))
  // While searching, every group is searched — a field you cannot name the section of is exactly
  // the field you use a search box for.
  const shown = query.trim()
    ? (schema.sections || []).filter((f) => matchField(f, query.trim().toLowerCase()))
    : (active?.sections || []).map((n) => byName[n]).filter(Boolean)

  const body = (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold text-fg">{schema.kind} · {schema.cluster}</div>
          <div className="truncate text-[11px] text-muted">
            {schema.group}/{schema.version} · namespace {schema.namespace}
          </div>
        </div>
        <div className="flex items-center gap-2">
          {schema.status && <Badge tone={schema.status === 'ready' ? 'success' : 'warning'}>{schema.status}</Badge>}
          <Button size="sm" variant="outline" onClick={() => setWide((v) => !v)} title={wide ? 'Back to the panel (Esc)' : 'Edit full width'}>
            {wide ? <Icon.Minimize size={13} /> : <Icon.Maximize size={13} />}
          </Button>
          <Button size="sm" variant="outline" onClick={load} disabled={!!busy}>
            Reload
          </Button>
        </div>
      </div>

      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        Every field here comes from this cluster's own CRD, so it is what this operator version
        accepts — not a fixed list. Edits are collected as a patch and applied only when you say so;
        <span className="font-medium text-fg"> Check</span> validates them against the API server
        without changing anything.
      </div>

      <div className="relative">
        <span className="absolute left-2 top-1/2 -translate-y-1/2 text-muted"><Icon.Search size={14} /></span>
        <input className={`${inputCls} pl-7`} value={query} spellCheck={false}
          placeholder="Find a field — try pitr, size, image, storage…"
          onChange={(e) => setQuery(e.target.value)} />
        {query && (
          <button onClick={() => setQuery('')} className="absolute right-2 top-1/2 -translate-y-1/2 text-muted hover:text-fg">×</button>
        )}
      </div>

      {!query.trim() && groups.length > 1 && (
        <div className="flex flex-wrap gap-1 rounded-lg bg-surface2 p-1">
          {groups.map((g) => {
            const n = changed.filter((p) => g.sections.some((s) => p === s || p.startsWith(s + '.'))).length
            return (
              <button key={g.id} onClick={() => setGroup(g.id)}
                className={`flex items-center gap-1 rounded-md px-2.5 py-1 text-xs font-medium transition ${group === g.id ? 'bg-surface text-fg shadow' : 'text-muted'}`}>
                {g.label}
                {n > 0 && <span className="rounded-full bg-primary px-1.5 text-[10px] text-white">{n}</span>}
              </button>
            )
          })}
        </div>
      )}
      {!query.trim() && active?.note && <p className="text-[11px] leading-snug text-muted">{active.note}</p>}

      <div className="space-y-3">
        {shown.map((f) => (
          <div key={f.path} className="rounded-xl border p-2">
            {f.type === 'object' && f.fields?.length
              ? <Group field={f} value={getAt(draft, f.path)} path={f.path} ctx={ctx} depth={0} defaultOpen={shown.length <= 3} />
              : <Node field={f} path={f.path} ctx={ctx} depth={0} />}
          </div>
        ))}
        {!shown.length && <p className="text-sm text-muted">No field matches “{query}”.</p>}
      </div>

      {/* The footer is the whole safety story: it says what will be sent, and nothing leaves
          until one of these buttons is pressed. */}
      <div className="sticky bottom-0 -mx-1 flex flex-wrap items-center gap-2 rounded-lg border bg-surface/95 px-3 py-2 backdrop-blur">
        <span className="text-xs text-muted">
          {changed.length
            ? <>{changed.length} change{changed.length === 1 ? '' : 's'} pending</>
            : 'No changes'}
        </span>
        <div className="ml-auto flex items-center gap-2">
          {!!changed.length && (
            <>
              <Button size="sm" variant="outline" onClick={() => setReview((r) => !r)}>
                {review ? 'Hide patch' : 'Review patch'}
              </Button>
              <Button size="sm" variant="outline" onClick={() => setDraft(orig)} disabled={!!busy}>Discard</Button>
              <Button size="sm" variant="outline" onClick={() => send(true)} disabled={!!busy}>
                {busy === 'check' ? 'Checking…' : 'Check'}
              </Button>
              <Button size="sm" onClick={() => send(false)} disabled={!!busy}>
                {busy === 'apply' ? 'Applying…' : 'Apply'}
              </Button>
            </>
          )}
        </div>
      </div>

      {review && !!changed.length && (
        <pre className="max-h-72 overflow-auto whitespace-pre rounded-lg border bg-bg p-2 font-mono text-[11px] leading-relaxed text-fg">
          {`spec:${yamlish(patch, 1)}`}
        </pre>
      )}

      {result && (
        <div className={`rounded-lg border px-3 py-2 text-xs ${result.ok ? 'border-success/30 bg-success/10 text-success' : 'border-danger/30 bg-danger/10 text-danger'}`}>
          <span className="whitespace-pre-wrap">{result.message}</span>
        </div>
      )}
    </div>
  )

  // Full width is a portal rather than a panel that grows, so it escapes the properties column's
  // own scrolling: a sticky footer inside a scrolled column is not sticky to anything useful.
  // The form itself is the same tree — one editor, two sizes, no second implementation to drift.
  if (wide) {
    return createPortal(
      <div className="fixed inset-0 z-50 flex flex-col bg-bg/95 p-4 backdrop-blur"
        onPointerDown={(e) => e.stopPropagation()}>
        <div className="mx-auto flex min-h-0 w-full max-w-5xl flex-1 flex-col overflow-auto rounded-xl border bg-surface p-4">
          {body}
        </div>
      </div>,
      document.body,
    )
  }
  return body
}

export default CRFormEditor
