// Cards.jsx — the Scratchboard's cards (pages/Scratchboard.jsx).
//
// Each card is a body component taking { state, update, spawn }: `state` is the card's own data,
// `update(patch)` changes it, `spawn(type, input)` opens another card beside it. The logic is in
// lib/scratch/* as plain functions; these only render it, so every card renders under the smoke
// test without the board around it.
//
// Nothing here stores anything: state lives in the board's React state and dies with the window.

import { useMemo, useState } from 'react'
import { area, small, copy, Input, Output, KV, useAsync } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { Icon } from '../Icons.jsx'
import { Markdown } from '../../lib/scratch/markdown.jsx'
import { convert, guessFormat, readSecret, FORMATS, FORMAT_LABEL } from '../../lib/scratch/transform.js'
import { checkBundle, parseBundle, summarize, hostMatches } from '../../lib/scratch/x509check.js'
import { pemBlocks } from '../../lib/scratch/der.js'
import { diffLines, diffStats } from '../../lib/scratch/diff.js'
import { parseGtidSet, union, subtract, contains, count, formatGtidSet } from '../../lib/scratch/gtid.js'
import { parseTimestamp, forms, zoned, between, ZONES } from '../../lib/scratch/time.js'
import { parseSize, sizeTable, DECIMAL, BINARY, human, mycnf, PAGES, parseDuration, durationTable } from '../../lib/scratch/units.js'
import { objectId, extendedJSON } from '../../lib/scratch/mongo.js'
import { parseURI } from '../../lib/scratch/uri.js'
import { filterLines, collapse } from '../../lib/scratch/logfilter.js'
import { suggest } from '../../lib/scratch/detect.js'
import { DigestCard } from './DigestCard.jsx'
import { RedactCard } from './RedactCard.jsx'
import { JwtCard } from './JwtCard.jsx'
import { ReplCard } from './ReplCard.jsx'
import { ConfigCard } from './ConfigCard.jsx'
import { ExplainCard } from './ExplainCard.jsx'
import { InnodbCard } from './InnodbCard.jsx'

// ------------------------------------------------------------------ shared bits
// Input, Output, KV and useAsync live in parts.jsx so cards in their own files share them.

export { Input, Output }

// ------------------------------------------------------------------ notes

export function NoteCard({ state, update, onSelectText }) {
  return (
    <div className="space-y-1">
      <div className="flex gap-1 text-[11px]">
        {['edit', 'preview'].map((m) => (
          <button key={m} onClick={() => update({ preview: m === 'preview' })}
            className={`rounded px-1.5 py-0.5 ${!!state.preview === (m === 'preview') ? 'bg-surface2 text-fg' : 'text-muted'}`}>{m === 'edit' ? 'Edit' : 'Preview'}</button>
        ))}
      </div>
      {state.preview
        ? <div className="max-h-[50vh] overflow-auto rounded border bg-bg p-2"><Markdown text={state.text} /></div>
        : <Input value={state.text || ''} rows={8} onChange={(text) => update({ text })} onSelectText={onSelectText}
          placeholder="Notes — Markdown: # heading, **bold**, `code`, - lists, ``` blocks" />}
    </div>
  )
}

export function ChecklistCard({ state, update }) {
  const items = state.items || []
  const [draft, setDraft] = useState('')
  const set = (next) => update({ items: next })
  const done = items.filter((i) => i.done).length
  return (
    <div className="space-y-1 text-[11px]">
      {items.length > 0 && <div className="text-muted">{done} of {items.length} done</div>}
      {items.map((it, i) => (
        <div key={it.id} className="flex items-start gap-1.5">
          <input type="checkbox" className="mt-0.5" checked={!!it.done} onChange={(e) => set(items.map((x, j) => (j === i ? { ...x, done: e.target.checked } : x)))} />
          <input className={`min-w-0 flex-1 bg-transparent outline-none ${it.done ? 'text-muted line-through' : 'text-fg'}`} value={it.text}
            onChange={(e) => set(items.map((x, j) => (j === i ? { ...x, text: e.target.value } : x)))} />
          <button className="text-muted hover:text-danger" title="Remove" onClick={() => set(items.filter((_, j) => j !== i))}><Icon.Close size={11} /></button>
        </div>
      ))}
      <form className="flex gap-1" onSubmit={(e) => { e.preventDefault(); if (draft.trim()) { set([...items, { id: `${Date.now()}-${items.length}`, text: draft.trim(), done: false }]); setDraft('') } }}>
        <input className={`${small} min-w-0 flex-1`} placeholder="Add a step, Enter to add" value={draft} onChange={(e) => setDraft(e.target.value)} />
      </form>
    </div>
  )
}

// ------------------------------------------------------------------ transformer

export function TransformCard({ state, update, spawn, onSelectText }) {
  const from = state.from === 'auto' || !state.from ? guessFormat(state.input) : state.from
  const to = state.to || 'yaml'
  const res = useAsync(() => convert(state.input || '', from, to), [state.input, from, to])
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-1 text-[11px]">
        <select className={small} value={state.from || 'auto'} onChange={(e) => update({ from: e.target.value })}>
          <option value="auto">Read as: auto ({from.toUpperCase()})</option>
          <option value="json">Read as: JSON</option>
          <option value="yaml">Read as: YAML</option>
          <option value="text">Read as: text</option>
        </select>
        <button className="rounded p-0.5 text-muted hover:text-fg" title="Swap: the output becomes the input"
          onClick={() => res?.value && update({ input: res.value, from: to === 'yaml' ? 'yaml' : to.startsWith('json') ? 'json' : 'text', to: to === 'yaml' ? 'json' : to.startsWith('json') ? 'yaml' : to })}>
          <Icon.Both size={13} />
        </button>
        <select className={small} value={to} onChange={(e) => update({ to: e.target.value })}>
          {FORMATS.map((f) => <option key={f} value={f}>{FORMAT_LABEL[f]}</option>)}
        </select>
      </div>
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} placeholder="Paste JSON, YAML or text" />
      <Output text={res?.value} error={res?.error} spawn={spawn} />
    </div>
  )
}

// ------------------------------------------------------------------ TLS

// tlsInputs turns what was pasted — PEM, or a Kubernetes Secret — into tls.crt / ca.crt / tls.key.
export async function tlsInputs(text, caText) {
  const t = String(text || '')
  if (/tls\.crt/.test(t) && !/-----BEGIN/.test(t)) {
    const sec = await readSecret(t)
    if (sec.error) return { error: sec.error }
    const get = (k) => sec.entries.find((e) => e.key === k)?.value || ''
    return { tlsCrt: get('tls.crt'), caCrt: get('ca.crt') || caText || '', tlsKey: get('tls.key'), from: `Secret ${sec.name || ''}`.trim() }
  }
  const blocks = pemBlocks(t)
  const certs = blocks.filter((b) => b.type === 'CERTIFICATE' || b.type.includes('REQUEST')).map((b) => b.text).join('\n')
  const keys = blocks.filter((b) => b.type.endsWith('PRIVATE KEY')).map((b) => b.text).join('\n')
  return { tlsCrt: certs, caCrt: caText || '', tlsKey: keys, from: 'PEM' }
}

const CHECK_LABEL = { dates: 'Dates', chain: 'Chain to ca.crt', strict: 'openssl verify -CAfile (strict)', key: 'Private key matches' }
const TONE = { valid: 'success', warning: 'warning', invalid: 'danger', csr: 'primary' }
const fmtDate = (d) => (d ? d.toISOString().replace('T', ' ').replace('.000Z', ' UTC') : '—')

export function CertDetail({ c, title, now = new Date() }) {
  const days = Math.floor((c.notAfter - now) / 86400000)
  const names = [...c.ext.san.dns, ...c.ext.san.ips, ...c.ext.san.uris, ...c.ext.san.emails]
  return (
    <div className="space-y-0.5 rounded border bg-bg px-1.5 py-1">
      <div className="flex items-center gap-1 text-[11px]">
        <span className="font-medium text-fg">{title}</span>
        {c.ext.isCA && <Badge tone="accent">CA{c.ext.pathLen !== null ? ` · pathlen ${c.ext.pathLen}` : ''}</Badge>}
        {c.selfIssued && <Badge tone="muted">self-signed</Badge>}
        {c.notAfter && <span className={`ml-auto ${days < 0 ? 'text-danger' : days < 14 ? 'text-warning' : 'text-muted'}`}>{days < 0 ? 'expired' : `${days} days left`}</span>}
      </div>
      <KV k="Subject" v={c.subject.text} />
      {c.issuer && <KV k="Issuer" v={c.issuer.text} />}
      {c.notBefore && <KV k="Valid" v={`${fmtDate(c.notBefore)} → ${fmtDate(c.notAfter)}`} mono={false} />}
      {names.length > 0 && <KV k="SANs" v={names.join(', ')} />}
      <KV k="Key" v={`${c.key.label}${c.sigAlg ? ` · signed ${c.sigAlg.name}` : ''}`} mono={false} />
      {(c.ext.extKeyUsage.length > 0 || c.ext.keyUsage.length > 0) && <KV k="Usage" v={[...c.ext.extKeyUsage, ...c.ext.keyUsage].join(', ')} mono={false} />}
      {c.serial && <KV k="Serial" v={c.serial} />}
      {c.ext.ski && <KV k="Subject key ID" v={c.ext.ski} />}
      {c.ext.aki && <KV k="Authority key ID" v={c.ext.aki} />}
    </div>
  )
}

export function TlsView({ res, host, now = new Date() }) {
  if (!res) return <div className="text-[11px] text-muted">Reading…</div>
  if (res.error) return <Output error={res.error} />
  const leaf = res.chain?.[0]
  const hostOK = leaf && host ? hostMatches(leaf, host) : null
  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-1.5 text-[11px]">
        {res.from && <span className="text-muted">{res.from}</span>}
        <Badge tone={TONE[res.status] || 'muted'}>{res.status === 'csr' ? 'certificate request' : res.status}</Badge>
      </div>
      {res.errors?.map((e) => <div key={e} className="text-[11px] text-danger">{e}</div>)}
      {leaf && <div className="rounded bg-surface2 px-1.5 py-1 text-[11px] text-fg">{summarize(leaf, res, now)}</div>}
      {res.checks?.map((c) => (
        <div key={c.name} className="flex items-start gap-1.5 text-[11px]">
          <span className={c.ok ? 'text-success' : 'text-danger'}>{c.ok ? '✓' : '✗'}</span>
          <span className="shrink-0 font-medium text-fg">{CHECK_LABEL[c.name] || c.name}</span>
          <span className="min-w-0 break-words text-muted">{c.detail}</span>
        </div>
      ))}
      {hostOK !== null && (
        <div className={`text-[11px] ${hostOK ? 'text-success' : 'text-danger'}`}>
          {hostOK ? '✓' : '✗'} {host} {hostOK ? 'is named by the certificate — a client checking the host name accepts it' : 'is NOT among the certificate\'s SANs — a client checking the host name refuses it'}
        </div>
      )}
      {(res.chain || []).map((c, i) => <CertDetail key={`t${i}`} c={c} now={now} title={i === 0 ? 'Certificate' : `Chain ${i}`} />)}
      {(res.ca || []).map((c, i) => <CertDetail key={`c${i}`} c={c} now={now} title={res.ca.length > 1 ? `ca.crt ${i + 1}` : 'ca.crt'} />)}
      {(res.csrs || []).map((c, i) => <CertDetail key={`r${i}`} c={c} now={now} title="Certificate request" />)}
    </div>
  )
}

export function TlsCard({ state, update, onSelectText }) {
  const res = useAsync(async () => {
    if (!String(state.input || '').trim()) return null
    const ins = await tlsInputs(state.input, state.ca)
    if (ins.error) return ins
    return { ...(await checkBundle(ins)), from: ins.from }
  }, [state.input, state.ca])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={4}
        placeholder="Paste a PEM bundle (certificate, chain, key, or a CSR) or a Kubernetes TLS Secret" />
      <Input value={state.ca || ''} onChange={(ca) => update({ ca })} rows={2} placeholder="ca.crt (optional — a Secret's own ca.crt is used when it has one)" />
      <input className={`${small} w-full`} placeholder="Check a host name, e.g. db.example.net" value={state.host || ''} onChange={(e) => update({ host: e.target.value })} />
      {res ? <TlsView res={res} host={state.host} /> : <div className="text-[11px] text-muted">Parsed in this browser; nothing is sent anywhere.</div>}
    </div>
  )
}

// ------------------------------------------------------------------ diff

export function DiffView({ ops, inline }) {
  const { added, removed } = diffStats(ops)
  const tone = { '+': 'bg-success/15 text-fg', '-': 'bg-danger/15 text-fg', '=': 'text-muted' }
  return (
    <div className="space-y-0.5">
      <div className="text-[11px] text-muted">{added === 0 && removed === 0 ? 'identical' : `+${added} −${removed}`}</div>
      <div className="max-h-[50vh] overflow-auto rounded border bg-bg font-mono text-[11px]">
        {inline
          ? ops.map((o, i) => <div key={i} className={`whitespace-pre-wrap break-all px-1 ${tone[o.op]}`}>{o.op === '=' ? ' ' : o.op} {o.text}</div>)
          : ops.map((o, i) => (
            <div key={i} className="grid grid-cols-2 gap-1">
              <div className={`whitespace-pre-wrap break-all px-1 ${o.op === '-' ? tone['-'] : o.op === '=' ? tone['='] : ''}`}>{o.op !== '+' ? o.text : ''}</div>
              <div className={`whitespace-pre-wrap break-all px-1 ${o.op === '+' ? tone['+'] : o.op === '=' ? tone['='] : ''}`}>{o.op !== '-' ? o.text : ''}</div>
            </div>
          ))}
      </div>
    </div>
  )
}

export function DiffCard({ state, update, onSelectText }) {
  const ops = useAsync(async () => {
    let a = state.a || ''
    let b = state.b || ''
    if (state.semantic) {
      // By meaning: both sides as JSON with sorted keys, so order and formatting drop out.
      const na = await convert(a, guessFormat(a), 'json-sorted')
      const nb = await convert(b, guessFormat(b), 'json-sorted')
      if (na.error || nb.error) return { error: `cannot compare by meaning: ${na.error || nb.error}` }
      a = na.value
      b = nb.value
    }
    return { ops: diffLines(a, b) }
  }, [state.a, state.b, state.semantic])
  return (
    <div className="space-y-1">
      <div className="grid grid-cols-2 gap-1">
        <Input value={state.a || ''} onChange={(a) => update({ a })} onSelectText={onSelectText} rows={4} placeholder="Left" />
        <Input value={state.b || ''} onChange={(b) => update({ b })} onSelectText={onSelectText} rows={4} placeholder="Right" />
      </div>
      <div className="flex gap-3 text-[11px] text-muted">
        <label className="flex items-center gap-1"><input type="checkbox" checked={!!state.semantic} onChange={(e) => update({ semantic: e.target.checked })} /> JSON/YAML by meaning</label>
        <label className="flex items-center gap-1"><input type="checkbox" checked={!!state.inline} onChange={(e) => update({ inline: e.target.checked })} /> inline</label>
      </div>
      {ops?.error ? <Output error={ops.error} /> : ops?.ops ? <DiffView ops={ops.ops} inline={state.inline} /> : null}
    </div>
  )
}

// ------------------------------------------------------------------ certificate compare

const COMPARE_FIELDS = [
  ['Subject', (c) => c.subject.text], ['Issuer', (c) => c.issuer.text], ['Serial', (c) => c.serial],
  ['Not before', (c) => fmtDate(c.notBefore)], ['Not after', (c) => fmtDate(c.notAfter)],
  ['DNS names', (c) => c.ext.san.dns.join(', ')], ['IP addresses', (c) => c.ext.san.ips.join(', ')],
  ['Key', (c) => c.key.label], ['Public key', (c) => (c.key.n || c.key.point || '').slice(0, 24)],
  ['Signature', (c) => c.sigAlg.name], ['Usage', (c) => c.ext.extKeyUsage.join(', ')], ['CA', (c) => (c.ext.isCA ? 'yes' : 'no')],
]

export function CompareView({ a, b }) {
  return (
    <div className="space-y-0.5 text-[11px]">
      {COMPARE_FIELDS.map(([label, get]) => {
        const va = a ? get(a) : ''
        const vb = b ? get(b) : ''
        const same = va === vb
        return (
          <div key={label} className={`grid grid-cols-[6rem_1fr_1fr] gap-1 rounded px-1 ${same ? '' : 'bg-warning/15'}`}>
            <span className="text-muted">{label}{same ? '' : ' ≠'}</span>
            <span className="break-all font-mono text-fg">{va || '—'}</span>
            <span className="break-all font-mono text-fg">{vb || '—'}</span>
          </div>
        )
      })}
    </div>
  )
}

export function CertCompareCard({ state, update, onSelectText }) {
  const pair = useAsync(async () => {
    const leafOf = async (t) => (String(t || '').trim() ? parseBundle((await tlsInputs(t)).tlsCrt || '').certs[0] || null : null)
    return { a: await leafOf(state.a), b: await leafOf(state.b) }
  }, [state.a, state.b])
  return (
    <div className="space-y-1">
      <div className="grid grid-cols-2 gap-1">
        <Input value={state.a || ''} onChange={(a) => update({ a })} onSelectText={onSelectText} rows={3} placeholder="Before — PEM or a TLS Secret" />
        <Input value={state.b || ''} onChange={(b) => update({ b })} onSelectText={onSelectText} rows={3} placeholder="After — PEM or a TLS Secret" />
      </div>
      {pair && (pair.a || pair.b) ? <CompareView a={pair.a} b={pair.b} /> : <div className="text-[11px] text-muted">Paste a certificate on each side; the differences are highlighted.</div>}
    </div>
  )
}

// ------------------------------------------------------------------ Kubernetes Secret

export function SecretView({ sec, spawn, source }) {
  const [shown, setShown] = useState({})
  if (!sec) return null
  if (sec.error) return <Output error={sec.error} />
  return (
    <div className="space-y-1 text-[11px]">
      <div className="text-muted">{sec.name || '(no name)'}{sec.namespace ? ` · ${sec.namespace}` : ''}{sec.type ? ` · ${sec.type}` : ''} · {sec.entries.length} key(s)</div>
      {sec.entries.map((e) => (
        <div key={e.key} className="rounded border bg-bg px-1.5 py-1">
          <div className="flex items-center gap-1">
            <span className="font-mono font-medium text-fg">{e.key}</span>
            {e.binary && <Badge tone="muted">binary (hex)</Badge>}
            {e.stringData && <Badge tone="muted">stringData</Badge>}
            <button className="ml-auto text-muted hover:text-fg" title={shown[e.key] ? 'Hide' : 'Show'} onClick={() => setShown({ ...shown, [e.key]: !shown[e.key] })}>
              {shown[e.key] ? <Icon.EyeOff size={12} /> : <Icon.Eye size={12} />}
            </button>
            <button className="text-muted hover:text-fg" title="Copy" onClick={() => copy(e.value)}><Icon.Copy size={12} /></button>
            {/-----BEGIN/.test(e.value) && spawn && (
              <button className="text-primary hover:underline" onClick={() => spawn('tls', source)}>Inspect</button>
            )}
          </div>
          <pre className="mt-0.5 max-h-40 overflow-auto whitespace-pre-wrap break-all font-mono text-fg" data-secret="">
            {shown[e.key] ? e.value : '•'.repeat(Math.min(24, e.value.length || 1))}
          </pre>
        </div>
      ))}
    </div>
  )
}

export function SecretCard({ state, update, spawn, onSelectText }) {
  const sec = useAsync(async () => (String(state.input || '').trim() ? readSecret(state.input) : null), [state.input])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={4} placeholder="Paste a Secret — kubectl get secret NAME -o yaml (or -o json)" />
      <SecretView sec={sec} spawn={spawn} source={state.input} />
    </div>
  )
}

// ------------------------------------------------------------------ GTID

export function GtidView({ a, b }) {
  const pa = parseGtidSet(a)
  const pb = parseGtidSet(b)
  if (pa.error || pb.error) return <Output error={pa.error || pb.error} />
  const rows = [
    ['A ∪ B', union(pa.set, pb.set)],
    ['A − B (on A, missing on B)', subtract(pa.set, pb.set)],
    ['B − A (on B only — errant if B is a replica of A)', subtract(pb.set, pa.set)],
  ]
  return (
    <div className="space-y-1 text-[11px]">
      <div className="text-muted">
        A has {count(pa.set).toString()} transaction(s), B has {count(pb.set).toString()}.{' '}
        {contains(pa.set, pb.set) && contains(pb.set, pa.set) ? 'The sets are equal.' : contains(pa.set, pb.set) ? 'A contains B.' : contains(pb.set, pa.set) ? 'B contains A.' : 'Neither contains the other.'}
      </div>
      {rows.map(([label, set]) => (
        <div key={label}>
          <div className="text-muted">{label} — {count(set).toString()}</div>
          <Output text={formatGtidSet(set) || '(empty)'} />
        </div>
      ))}
    </div>
  )
}

export function GtidCard({ state, update, onSelectText }) {
  return (
    <div className="space-y-1">
      <Input value={state.a || ''} onChange={(a) => update({ a })} onSelectText={onSelectText} rows={2} placeholder="Set A — e.g. the source's gtid_executed" />
      <Input value={state.b || ''} onChange={(b) => update({ b })} onSelectText={onSelectText} rows={2} placeholder="Set B — e.g. the replica's gtid_executed" />
      {(state.a || state.b) && <GtidView a={state.a} b={state.b} />}
    </div>
  )
}

// ------------------------------------------------------------------ time

export function TimeView({ a, b, zone }) {
  const pa = parseTimestamp(a)
  if (pa.error) return <Output error={pa.error} />
  const pb = String(b || '').trim() ? parseTimestamp(b) : null
  return (
    <div className="space-y-0.5">
      <div className="text-[11px] text-muted">Read as {pa.source}</div>
      {forms(pa.date, pa.micros).map((f) => <KV key={f.label} k={f.label} v={f.value} />)}
      <KV k={zone || 'UTC'} v={zoned(pa.date, pa.micros, zone || 'UTC')} />
      <KV k="This browser" v={zoned(pa.date, pa.micros, Intl.DateTimeFormat().resolvedOptions().timeZone)} />
      {pb && (pb.error ? <Output error={`second timestamp: ${pb.error}`} /> : <KV k="A → B" v={between(pa, pb).text} />)}
    </div>
  )
}

export function TimeCard({ state, update }) {
  return (
    <div className="space-y-1">
      <input className={`${small} w-full font-mono`} placeholder="Timestamp — epoch, ISO, MySQL, mongod $date" value={state.a || ''} onChange={(e) => update({ a: e.target.value })} />
      <input className={`${small} w-full font-mono`} placeholder="Second timestamp (optional) — the time between them" value={state.b || ''} onChange={(e) => update({ b: e.target.value })} />
      <select className={small} value={state.zone || 'UTC'} onChange={(e) => update({ zone: e.target.value })}>
        {ZONES.map((z) => <option key={z}>{z}</option>)}
      </select>
      {state.a && <TimeView a={state.a} b={state.b} zone={state.zone} />}
    </div>
  )
}

// ------------------------------------------------------------------ units

export function UnitsView({ size, mode, duration }) {
  const p = String(size || '').trim() ? parseSize(size, mode) : null
  const d = String(duration || '').trim() ? parseDuration(duration) : null
  return (
    <div className="space-y-1.5 text-[11px]">
      {p && (p.error ? <Output error={p.error} /> : (
        <>
          <div className="text-fg"><span className="font-mono">{p.bytes.toLocaleString('en-US')}</span> bytes · {human(p.bytes, DECIMAL)} · {human(p.bytes, BINARY)}{mycnf(p.bytes) ? <> · my.cnf <span className="font-mono">{mycnf(p.bytes)}</span></> : null}</div>
          <div className="grid grid-cols-2 gap-2">
            <div>
              <div className="text-muted">Decimal (1 KB = 1,000 B)</div>
              {sizeTable(p.bytes, DECIMAL).map((r) => <KV key={r.unit} k={r.unit} v={r.value} />)}
            </div>
            <div>
              <div className="text-muted">Binary (1 KiB = 1,024 B)</div>
              {sizeTable(p.bytes, BINARY).map((r) => <KV key={r.unit} k={r.unit} v={r.value} />)}
            </div>
          </div>
          <div>
            {PAGES.map((pg) => <KV key={pg.label} k={pg.label.split(' (')[0]} v={`${(p.bytes / pg.size).toLocaleString('en-US', { maximumFractionDigits: 2 })} (${pg.label.split(' (')[1].replace(')', '')})`} mono={false} />)}
          </div>
        </>
      ))}
      {d && (d.error ? <Output error={d.error} /> : (
        <div>
          <div className="text-muted">Duration</div>
          {durationTable(d.seconds).map((r) => <KV key={r.unit} k={r.unit} v={r.value} />)}
        </div>
      ))}
    </div>
  )
}

export function UnitsCard({ state, update }) {
  return (
    <div className="space-y-1">
      <div className="flex gap-1">
        <input className={`${small} min-w-0 flex-1 font-mono`} placeholder="Size — 134217728, 128M, 1.5 GB, 2 TiB" value={state.size || ''} onChange={(e) => update({ size: e.target.value })} />
        <select className={small} value={state.mode || 'mycnf'} onChange={(e) => update({ mode: e.target.value })} title="What a bare K/M/G/T means">
          <option value="mycnf">K/M/G = binary (my.cnf)</option>
          <option value="decimal">K/M/G = decimal</option>
        </select>
      </div>
      <input className={`${small} w-full font-mono`} placeholder="Duration (optional) — 250ms, 1.5s, 2h" value={state.duration || ''} onChange={(e) => update({ duration: e.target.value })} />
      <UnitsView size={state.size} mode={state.mode} duration={state.duration} />
    </div>
  )
}

// ------------------------------------------------------------------ MongoDB values

export function MongoView({ input }) {
  const oid = /^\s*[0-9a-f]{24}\s*$/i.test(input) ? objectId(input) : null
  const found = extendedJSON(input)
  if (!oid && !found.length) return <div className="text-[11px] text-muted">No ObjectId or Extended JSON value found.</div>
  return (
    <div className="space-y-0.5">
      {oid && (
        <>
          <KV k="ObjectId" v={oid.hex} />
          <KV k="Created" v={`${oid.created.toISOString()} (epoch ${oid.seconds})`} />
          <KV k="Random" v={oid.random} />
          <KV k="Counter" v={String(oid.counter)} />
        </>
      )}
      {found.map((f, i) => <KV key={i} k={f.type} v={f.meaning} mono={false} />)}
    </div>
  )
}

export function MongoCard({ state, update, onSelectText }) {
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={3} placeholder="An ObjectId, or a document with $date / $oid / $timestamp / $numberLong" />
      {state.input && <MongoView input={state.input} />}
    </div>
  )
}

// ------------------------------------------------------------------ connection URI

export function UriView({ input }) {
  const u = parseURI(input)
  if (u.error) return <Output error={u.error} />
  return (
    <div className="space-y-0.5">
      <KV k="Engine" v={u.engine} mono={false} />
      <KV k="URI" v={u.masked} />
      <KV k="User" v={`${u.user || '—'}${u.passwordSet ? ' (password set, hidden)' : ''}`} />
      {u.hosts.map((h, i) => <KV key={i} k={i === 0 ? 'Hosts' : ''} v={`${h.host}${h.port ? `:${h.port}` : ''}`} />)}
      <KV k="Database" v={u.database} />
      {u.params.filter((p) => !/password/i.test(p.key)).map((p) => <KV key={p.key} k={p.key} v={p.value} />)}
      <div className="pt-1 text-[11px] text-muted">TLS</div>
      {u.tls.map((t) => <KV key={t.setting} k={t.setting} v={t.meaning} mono={false} />)}
    </div>
  )
}

export function UriCard({ state, update }) {
  return (
    <div className="space-y-1">
      <input className={`${small} w-full font-mono`} placeholder="mysql://, postgresql://, mongodb:// — or host=… dbname=…" value={state.input || ''} onChange={(e) => update({ input: e.target.value })} />
      {state.input && <UriView input={state.input} />}
    </div>
  )
}

// ------------------------------------------------------------------ log filter

export function LogFilterView({ input, include, exclude, grouped }) {
  const r = useMemo(() => filterLines(input, include, exclude), [input, include, exclude])
  const groups = useMemo(() => (grouped ? collapse(r.lines) : null), [grouped, r.lines])
  return (
    <div className="space-y-0.5">
      {r.error && <Output error={`pattern: ${r.error}`} />}
      <div className="text-[11px] text-muted">{r.lines.length} of {r.total} line(s){groups ? ` · ${groups.length} distinct` : ''}</div>
      <div className="max-h-[50vh] overflow-auto rounded border bg-bg font-mono text-[11px]">
        {groups
          ? groups.map((g, i) => <div key={i} className="flex gap-2 px-1"><span className="w-10 shrink-0 text-right text-primary">×{g.count}</span><span className="min-w-0 whitespace-pre-wrap break-all text-fg">{g.example}</span></div>)
          : r.lines.map((l, i) => <div key={i} className="whitespace-pre-wrap break-all px-1 text-fg">{l || ' '}</div>)}
      </div>
    </div>
  )
}

export function LogFilterCard({ state, update, onSelectText }) {
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={4} placeholder="Paste log lines" />
      <div className="flex gap-1">
        <input className={`${small} min-w-0 flex-1`} placeholder="keep lines with… (text or /regex/i)" value={state.include || ''} onChange={(e) => update({ include: e.target.value })} />
        <input className={`${small} min-w-0 flex-1`} placeholder="drop lines with…" value={state.exclude || ''} onChange={(e) => update({ exclude: e.target.value })} />
      </div>
      <label className="flex items-center gap-1 text-[11px] text-muted"><input type="checkbox" checked={!!state.grouped} onChange={(e) => update({ grouped: e.target.checked })} /> collapse repeats (numbers, ids and quoted values ignored)</label>
      {state.input && <LogFilterView input={state.input} include={state.include} exclude={state.exclude} grouped={state.grouped} />}
    </div>
  )
}

// ------------------------------------------------------------------ registry

// CARD_TYPES: what the board can open. `init(input)` is the state a card opens with when it is
// spawned from pasted or selected text.
export const CARD_TYPES = {
  note: { label: 'Quick Note', family: 'Notes', icon: 'Text', w: 320, h: 240, Body: NoteCard, init: (t) => ({ text: t || '' }) },
  checklist: { label: 'Checklist', family: 'Notes', icon: 'Check', w: 300, h: 220, Body: ChecklistCard,
    init: (t) => ({ items: String(t || '').split('\n').map((s) => s.replace(/^\s*([-*]|\d+\.)?\s*(\[[ x]\])?\s*/i, '').trim()).filter(Boolean).map((text, i) => ({ id: `i${i}`, text, done: false })) }) },
  transform: { label: 'Transformer', family: 'Transformers', icon: 'Both', w: 380, h: 320, Body: TransformCard, init: (t) => ({ input: t || '', from: 'auto', to: /^\s*[[{]/.test(t || '') ? 'yaml' : 'json' }) },
  tls: { label: 'TLS Inspector', family: 'Inspectors', icon: 'Key', w: 460, h: 420, Body: TlsCard, init: (t) => ({ input: t || '' }) },
  diff: { label: 'Text Diff', family: 'Transformers', icon: 'Code', w: 520, h: 320, Body: DiffCard, init: (t) => ({ a: t || '', b: '' }) },
  certcompare: { label: 'Certificate Compare', family: 'Inspectors', icon: 'Key', w: 560, h: 380, Body: CertCompareCard, init: (t) => ({ a: t || '', b: '' }) },
  secret: { label: 'Secret decoder', family: 'Inspectors', icon: 'Vault', w: 420, h: 360, Body: SecretCard, init: (t) => ({ input: t || '' }) },
  gtid: { label: 'GTID sets', family: 'Inspectors', icon: 'Database', w: 460, h: 360, Body: GtidCard, init: (t) => ({ a: t || '', b: '' }) },
  time: { label: 'Log time', family: 'Transformers', icon: 'Timeline', w: 400, h: 300, Body: TimeCard, init: (t) => ({ a: String(t || '').trim(), zone: 'UTC' }) },
  units: { label: 'Database units', family: 'Transformers', icon: 'Disk', w: 420, h: 340, Body: UnitsCard, init: (t) => ({ size: String(t || '').trim(), mode: 'mycnf' }) },
  mongo: { label: 'MongoDB values', family: 'Inspectors', icon: 'Database', w: 380, h: 240, Body: MongoCard, init: (t) => ({ input: t || '' }) },
  uri: { label: 'Connection URI', family: 'Inspectors', icon: 'Link', w: 440, h: 300, Body: UriCard, init: (t) => ({ input: String(t || '').trim() }) },
  logfilter: { label: 'Log filter', family: 'Transformers', icon: 'Logs', w: 480, h: 360, Body: LogFilterCard, init: (t) => ({ input: t || '', grouped: true }) },
  explain: { label: 'EXPLAIN reader', family: 'Inspectors', icon: 'Table', w: 560, h: 420, Body: ExplainCard, init: (t) => ({ input: t || '' }) },
  innodb: { label: 'InnoDB status reader', family: 'Inspectors', icon: 'Database', w: 560, h: 460, Body: InnodbCard, init: (t) => ({ input: t || '' }) },
  repl: { label: 'Replication status', family: 'Inspectors', icon: 'Nodes', w: 520, h: 420, Body: ReplCard, init: (t) => ({ input: t || '' }) },
  cnfdiff: { label: 'Config compare', family: 'Inspectors', icon: 'Sliders', w: 620, h: 440, Body: ConfigCard, init: (t) => ({ a: t || '', b: '', fmtA: 'auto', fmtB: 'auto', show: 'written' }) },
  redact: { label: 'Redactor', family: 'Transformers', icon: 'Eraser', w: 520, h: 460, Body: RedactCard, init: (t) => ({ input: t || '' }) },
  jwt: { label: 'JWT / OIDC decoder', family: 'Inspectors', icon: 'Key', w: 480, h: 460, Body: JwtCard, init: (t) => ({ input: String(t || '').trim(), key: '' }) },
  digest: { label: 'Query digest', family: 'Inspectors', icon: 'Table', w: 560, h: 420, Body: DigestCard, init: (t) => ({ input: t || '' }) },
}

export const CARD_FAMILIES = ['Notes', 'Transformers', 'Inspectors']
