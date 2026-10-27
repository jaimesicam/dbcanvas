// JwtCard.jsx — the JWT / OIDC decoder card (lib/scratch/jwt.js): a token's header and claims,
// what the claims mean, and whether the signature verifies with a pasted JWKS, public key,
// certificate or shared secret. The key set is pasted, never fetched.

import { Input, Output, KV, useAsync } from './parts.jsx'
import { Badge } from '../ui.jsx'
import { analyzeJwt } from '../../lib/scratch/jwt.js'

const STATUS = {
  valid: { tone: 'success', label: 'signature valid' },
  invalid: { tone: 'danger', label: 'invalid signature' },
  nokey: { tone: 'muted', label: 'no key given' },
  none: { tone: 'danger', label: 'alg none — unsigned' },
  mismatch: { tone: 'warning', label: 'key type mismatch' },
  unsupported: { tone: 'warning', label: 'cannot verify here' },
  error: { tone: 'danger', label: 'key error' },
  encrypted: { tone: 'accent', label: 'encrypted (JWE)' },
}
const FIND_TONE = { danger: 'text-danger', warning: 'text-warning', info: 'text-muted' }
const VALUE_TONE = { danger: 'text-danger', warning: 'text-warning', success: 'text-success' }

// JwtView renders an analyzeJwt() result.
export function JwtView({ res, spawn }) {
  if (!res) return null
  if (res.error) return <Output error={res.error} />
  const st = STATUS[res.verify?.status] || STATUS.nokey
  const h = res.header || {}
  return (
    <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-1.5 text-[11px]">
        <Badge tone={st.tone}>{st.label}</Badge>
        <span className="font-mono text-muted">{[h.alg, h.enc, h.kid && `kid ${h.kid}`, h.typ].filter(Boolean).join(' · ')}</span>
      </div>
      {res.verify?.detail && <div className="text-[11px] text-muted">{res.verify.detail}</div>}
      {res.findings?.length > 0 && (
        <div className="space-y-0.5">
          {res.findings.map((f) => (
            <div key={f.text} className={`text-[11px] ${FIND_TONE[f.tone] || 'text-muted'}`}>{f.tone === 'info' ? '•' : '⚠'} {f.text}</div>
          ))}
        </div>
      )}
      {res.claims?.length > 0 && (
        <div className="space-y-0.5 rounded border bg-bg px-1.5 py-1">
          {res.claims.map((c) => (
            <div key={c.claim} className="grid grid-cols-[7.5rem_1fr] gap-2 text-[11px]">
              <span className="break-all font-mono text-muted">{c.claim}</span>
              <span className="min-w-0">
                <span className="break-all font-mono text-fg">{c.value}</span>
                {c.meaning && <span className={`ml-1.5 ${VALUE_TONE[c.tone] || 'text-muted'}`}>— {c.meaning}</span>}
              </span>
            </div>
          ))}
        </div>
      )}
      <div>
        <div className="text-[11px] text-muted">Header</div>
        <Output text={JSON.stringify(res.header, null, 2)} spawn={spawn} />
      </div>
      {res.kind === 'jws' && (
        <div>
          <div className="text-[11px] text-muted">Payload</div>
          <Output text={res.payload ? JSON.stringify(res.payload, null, 2) : res.payloadText} spawn={spawn} />
        </div>
      )}
      {res.verify?.source && <KV k="Key" v={res.verify.source} mono={false} />}
    </div>
  )
}

export function JwtCard({ state, update, spawn, onSelectText }) {
  const res = useAsync(async () => (String(state.input || '').trim()
    ? analyzeJwt(state.input, state.key || '', { secretBase64: !!state.secretBase64 })
    : null), [state.input, state.key, state.secretBase64])
  return (
    <div className="space-y-1">
      <Input value={state.input || ''} onChange={(input) => update({ input })} onSelectText={onSelectText} rows={3}
        placeholder="Paste a JWT — also as Bearer … or an Authorization header line" />
      <Input value={state.key || ''} onChange={(key) => update({ key })} rows={2}
        placeholder="Key to verify with (optional): the JWKS JSON, a JWK, a PEM public key or certificate, or the HMAC secret" />
      <label className="flex items-center gap-1 text-[11px] text-muted">
        <input type="checkbox" checked={!!state.secretBase64} onChange={(e) => update({ secretBase64: e.target.checked })} /> HMAC secret is Base64
      </label>
      {res ? <JwtView res={res} spawn={spawn} /> : <div className="text-[11px] text-muted">Decoded and verified in this browser; the JWKS is pasted, never fetched.</div>}
    </div>
  )
}
