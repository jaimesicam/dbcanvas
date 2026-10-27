// jwt.js — the JWT / OIDC decoder: reads a JSON Web Token's header and claims, says what the
// standard and Keycloak claims mean (times in UTC and relative to now), and checks the signature
// against a key that is pasted — a JWKS, a single JWK, a PEM public key or certificate, or an
// HMAC secret. A JWKS URL is never fetched: the key set is pasted like everything else.
//
// Signatures are checked with the browser's WebCrypto (crypto.subtle), never by hand. An
// encrypted token (JWE) has only its header read.
//
// Everything runs in the page: nothing pasted into a card is sent anywhere.

import { pemBlocks, parseCertificate, read, children, integerBytes, b64ToBytes, bytesToB64, oid } from './der.js'

// ------------------------------------------------------------------ base64url

export function b64url(input) {
  const bytes = typeof input === 'string' ? new TextEncoder().encode(input) : new Uint8Array(input)
  return bytesToB64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function fromB64url(s) {
  const clean = String(s).replace(/=+$/, '')
  if (!/^[A-Za-z0-9_-]*$/.test(clean)) throw new Error('not base64url')
  return b64ToBytes(clean + '='.repeat((4 - (clean.length % 4)) % 4))
}

function jsonPart(s, what) {
  let text
  try { text = new TextDecoder('utf-8', { fatal: true }).decode(fromB64url(s)) } catch { throw new Error(`the ${what} is not base64url-encoded UTF-8`) }
  try { return { value: JSON.parse(text), text } } catch { return { value: null, text } }
}

// ------------------------------------------------------------------ token

// extractToken finds the token in what was pasted: the token itself, `Bearer …`, or an
// `Authorization: Bearer …` header line (with -H '…' around it).
export function extractToken(text) {
  const t = String(text || '').trim()
  const m = /eyJ[A-Za-z0-9_-]*(?:\.[A-Za-z0-9_-]*){2,4}/.exec(t)
  return m ? m[0] : t.replace(/^.*?\bBearer\s+/i, '').replace(/['"]+$/, '').trim()
}

// decodeJwt splits a token into { token, kind: 'jws' | 'jwe', header, payload, payloadText,
// signingInput, signature } or { error }.
export function decodeJwt(text) {
  const token = extractToken(text)
  if (!token) return { error: 'paste a JWT' }
  const parts = token.split('.')
  if (parts.length !== 3 && parts.length !== 5) return { error: `a JWT has 3 parts (or 5 when encrypted); this has ${parts.length}` }
  try {
    const header = jsonPart(parts[0], 'header')
    if (!header.value || typeof header.value !== 'object') return { error: 'the header is not a JSON object' }
    if (parts.length === 5) return { token, kind: 'jwe', header: header.value }
    const payload = jsonPart(parts[1], 'payload')
    return {
      token, kind: 'jws', header: header.value, payload: payload.value, payloadText: payload.text,
      signingInput: new TextEncoder().encode(`${parts[0]}.${parts[1]}`), signature: fromB64url(parts[2]),
    }
  } catch (e) {
    return { error: e.message }
  }
}

// ------------------------------------------------------------------ claims

// relative says how far a time is from now: "in 4 min", "2 h ago".
export function relative(sec, now) {
  const d = Math.round(sec - now / 1000)
  const a = Math.abs(d)
  const v = a < 60 ? `${a} s` : a < 3600 ? `${Math.round(a / 60)} min` : a < 172800 ? `${Math.round(a / 3600)} h` : `${Math.round(a / 86400)} days`
  return d >= 0 ? `in ${v}` : `${v} ago`
}

const utc = (sec) => new Date(sec * 1000).toISOString().replace('T', ' ').replace(/\.000Z$/, ' UTC').replace(/Z$/, ' UTC')
const list = (v) => (Array.isArray(v) ? v.join(', ') : typeof v === 'string' ? v : JSON.stringify(v))

const TIME_CLAIMS = {
  exp: (rel, past) => (past ? `expired ${rel}` : `expires ${rel}`),
  nbf: (rel, past) => (past ? `valid since ${rel}` : `not valid until ${rel}`),
  iat: (rel) => `issued ${rel}`,
  auth_time: (rel) => `the user signed in ${rel}`,
}
const TEXT_CLAIMS = {
  iss: 'issuer — for Keycloak, the realm URL; must equal the issuer the client is configured with',
  sub: 'subject — the user (or service account) the token is about',
  aud: 'audience — who the token is for; the receiving service must find itself here',
  azp: 'authorized party — the client the token was issued to',
  typ: 'token type (Keycloak: Bearer, ID, Refresh)',
  sid: 'the sign-in session',
  jti: 'the token\'s own unique id',
  nonce: 'the value the client sent, echoed to stop replays',
  acr: 'how strongly the user authenticated',
  amr: 'how the user authenticated',
  scope: 'scopes granted (space-separated)',
  scp: 'scopes granted',
  preferred_username: 'the user name',
  email: 'e-mail address',
  email_verified: 'whether the e-mail address was verified',
  name: 'display name',
  groups: 'groups the user is in',
  roles: 'roles granted',
  client_id: 'the client the token was issued to',
}

// explainClaims lists [{ claim, value, meaning, tone }] in a fixed order: identity, times, access.
export function explainClaims(p, now = Date.now()) {
  if (!p || typeof p !== 'object') return []
  const out = []
  for (const c of ['iss', 'sub', 'aud', 'azp', 'client_id', 'typ', 'preferred_username', 'email', 'email_verified', 'name', 'sid', 'jti', 'nonce', 'acr', 'amr']) {
    if (p[c] !== undefined) out.push({ claim: c, value: list(p[c]), meaning: TEXT_CLAIMS[c] })
  }
  for (const c of Object.keys(TIME_CLAIMS)) {
    if (typeof p[c] !== 'number') continue
    const past = p[c] * 1000 <= now
    const tone = c === 'exp' ? (past ? 'danger' : p[c] * 1000 - now < 300000 ? 'warning' : 'success') : c === 'nbf' && !past ? 'danger' : ''
    out.push({ claim: c, value: `${utc(p[c])} (${p[c]})`, meaning: TIME_CLAIMS[c](relative(p[c], now), past), tone })
  }
  if (typeof p.exp === 'number' && typeof p.iat === 'number') {
    const life = p.exp - p.iat
    out.push({ claim: 'lifetime', value: life >= 3600 ? `${(life / 3600).toFixed(life % 3600 ? 1 : 0)} h` : `${Math.round(life / 60)} min`, meaning: 'exp − iat' })
  }
  for (const c of ['scope', 'scp', 'roles', 'groups']) if (p[c] !== undefined) out.push({ claim: c, value: list(p[c]), meaning: TEXT_CLAIMS[c] })
  if (Array.isArray(p.realm_access?.roles)) out.push({ claim: 'realm_access.roles', value: p.realm_access.roles.join(', '), meaning: 'Keycloak realm roles' })
  if (p.resource_access && typeof p.resource_access === 'object') {
    for (const [client, v] of Object.entries(p.resource_access)) {
      if (Array.isArray(v?.roles)) out.push({ claim: `resource_access.${client}.roles`, value: v.roles.join(', '), meaning: `Keycloak client roles on "${client}"` })
    }
  }
  return out
}

// findings lists what is wrong or of note: [{ tone: 'danger' | 'warning' | 'info', text }].
export function findings(dec, now = Date.now()) {
  const out = []
  const h = dec.header || {}
  const p = dec.payload
  if (String(h.alg || '').toLowerCase() === 'none') out.push({ tone: 'danger', text: 'alg is "none": the token is unsigned and anyone can have written it' })
  if (!p || typeof p !== 'object') {
    out.push({ tone: 'warning', text: 'the payload is not a JSON object' })
    return out
  }
  if (typeof p.exp !== 'number') out.push({ tone: 'warning', text: 'no exp: the token never expires' })
  else if (p.exp * 1000 <= now) out.push({ tone: 'danger', text: `expired ${relative(p.exp, now)}` })
  if (typeof p.nbf === 'number' && p.nbf * 1000 > now) out.push({ tone: 'danger', text: `not yet valid — nbf is ${relative(p.nbf, now)}` })
  if (typeof p.iat === 'number' && p.iat * 1000 > now + 60000) out.push({ tone: 'warning', text: `issued in the future (${relative(p.iat, now)}) — the issuer's clock is ahead of this one` })
  if (p.aud === undefined) out.push({ tone: 'warning', text: 'no aud: any service that trusts the issuer would accept it' })
  if (!h.kid && h.alg && !/^HS/i.test(h.alg) && String(h.alg).toLowerCase() !== 'none') out.push({ tone: 'info', text: 'no kid in the header: the verifier has to try every key of the issuer' })
  return out
}

// ------------------------------------------------------------------ keys

const HASH = { 256: 'SHA-256', 384: 'SHA-384', 512: 'SHA-512' }
const EC_CURVE = { ES256: 'P-256', ES384: 'P-384', ES512: 'P-521' }
const EC_SIZE = { 'P-256': 32, 'P-384': 48, 'P-521': 66 }

// algFamily says which key type an alg needs: RSA, EC, oct (HMAC) or OKP (EdDSA).
export function algFamily(alg) {
  const a = String(alg || '')
  if (/^(RS|PS)(256|384|512)$/.test(a)) return 'RSA'
  if (/^ES(256|384|512)$/.test(a)) return 'EC'
  if (/^HS(256|384|512)$/.test(a)) return 'oct'
  if (a === 'EdDSA' || a === 'Ed25519') return 'OKP'
  return ''
}

// spkiToJwk reads the public half of an RSA, EC or Ed25519 SubjectPublicKeyInfo into a JWK.
function spkiToJwk(spki) {
  const [algT, bits] = children(read(spki))
  const [o, params] = children(algT)
  const id = oid(o)
  const bs = bits.bytes.subarray(bits.start + 1, bits.end)
  if (id === '1.2.840.113549.1.1.1') {
    const [n, e] = children(read(bs))
    return { kty: 'RSA', n: b64url(integerBytes(n)), e: b64url(integerBytes(e)) }
  }
  if (id === '1.2.840.10045.2.1') {
    const crv = { '1.2.840.10045.3.1.7': 'P-256', '1.3.132.0.34': 'P-384', '1.3.132.0.35': 'P-521' }[params ? oid(params) : ''] || 'unknown'
    const size = (bs.length - 1) / 2
    return { kty: 'EC', crv, x: b64url(bs.subarray(1, 1 + size)), y: b64url(bs.subarray(1 + size)) }
  }
  if (id === '1.3.101.112') return { kty: 'OKP', crv: 'Ed25519', x: b64url(bs) }
  throw new Error(`a public key of type ${id} is not supported`)
}

// readKeys turns the pasted key text into { keys: [JWK], source } or { secret, source } or
// { error } or {} when nothing was pasted.
export function readKeys(text, { secretBase64 = false } = {}) {
  const t = String(text ?? '')
  if (!t.trim()) return {}
  if (/^\s*[{[]/.test(t)) {
    let j
    try { j = JSON.parse(t) } catch (e) { return { error: `the key is not valid JSON: ${e.message}` } }
    const keys = Array.isArray(j?.keys) ? j.keys : Array.isArray(j) ? j : [j]
    if (!keys.every((k) => k && typeof k === 'object' && k.kty)) return { error: 'expected a JWKS ({"keys":[…]}) or a JWK with "kty"' }
    return { keys, source: keys.length > 1 || j?.keys ? `JWKS with ${keys.length} key(s)` : 'JWK' }
  }
  const blocks = pemBlocks(t)
  if (blocks.length) {
    const keys = []
    for (const b of blocks) {
      if (!b.der) return { error: `${b.type}: ${b.error}` }
      try {
        if (b.type === 'PUBLIC KEY') keys.push(spkiToJwk(b.der))
        else if (b.type === 'CERTIFICATE') keys.push({ ...spkiToJwk(parseCertificate(b.der).key.spki), fromCert: parseCertificate(b.der).subject.text })
        else if (b.type === 'RSA PUBLIC KEY') {
          const [n, e] = children(read(b.der))
          keys.push({ kty: 'RSA', n: b64url(integerBytes(n)), e: b64url(integerBytes(e)) })
        } else if (b.type.endsWith('PRIVATE KEY')) return { error: 'that is a private key — paste the public key or certificate; a private key is never needed to verify' }
      } catch (e) {
        return { error: `${b.type}: ${e.message}` }
      }
    }
    if (!keys.length) return { error: 'no public key or certificate in the PEM' }
    return { keys, source: keys[0].fromCert ? `certificate ${keys[0].fromCert}` : 'PEM public key' }
  }
  const raw = t.replace(/\r?\n$/, '')
  if (secretBase64) {
    try { return { secret: fromB64url(raw.trim().replace(/\+/g, '-').replace(/\//g, '_')), source: 'HMAC secret (Base64)' } } catch { return { error: 'the secret is not valid Base64' } }
  }
  return { secret: new TextEncoder().encode(raw), source: 'HMAC secret' }
}

const NEEDS = { RSA: 'an RSA public key', EC: 'an EC public key', oct: 'a shared secret', OKP: 'an Ed25519 (OKP) public key' }

// pickKeys chooses the JWKs to try for a header: the one with its kid, else every key whose type
// fits the alg. Returns { keys, error, status }.
function pickKeys(keys, header) {
  const fam = algFamily(header.alg)
  if (header.kid !== undefined) {
    const byKid = keys.filter((k) => k.kid === header.kid)
    if (byKid.length) return { keys: byKid }
    if (keys.some((k) => k.kid !== undefined)) {
      return { status: 'nokey', error: `no key with kid "${header.kid}" — the key set has ${keys.map((k) => `"${k.kid ?? '(none)'}"`).join(', ')}` }
    }
  }
  const usable = keys.filter((k) => k.use !== 'enc')
  const fit = usable.filter((k) => k.kty === fam && (!k.alg || k.alg === header.alg || fam === 'OKP'))
  if (fit.length) return { keys: fit }
  return { status: 'mismatch', error: `${header.alg} needs ${NEEDS[fam]}; the key given is ${[...new Set(keys.map((k) => k.kty))].join(', ')}${fit.length === 0 && usable.some((k) => k.kty === fam) ? ` for ${usable.find((k) => k.kty === fam).alg}` : ''}` }
}

// verifyWith checks the signature with one key. Returns { ok } or { error, status }.
async function verifyWith(dec, key, secret) {
  const subtle = globalThis.crypto?.subtle
  const alg = dec.header.alg
  const bits = alg.slice(2)
  const hash = HASH[bits]
  let importAlg
  let verifyAlg
  let sig = dec.signature
  const fam = algFamily(alg)
  if (key && key.kty !== fam) return { status: 'mismatch', error: `${alg} needs ${NEEDS[fam]}, not ${key.kty}` }
  if (!key && fam !== 'oct') return { status: 'mismatch', error: `${alg} needs ${NEEDS[fam]} (JWKS, JWK or PEM), not a shared secret` }
  switch (true) {
    case /^RS/.test(alg): importAlg = { name: 'RSASSA-PKCS1-v1_5', hash }; verifyAlg = { name: 'RSASSA-PKCS1-v1_5' }; break
    case /^PS/.test(alg): importAlg = { name: 'RSA-PSS', hash }; verifyAlg = { name: 'RSA-PSS', saltLength: Number(bits) / 8 }; break
    case /^ES/.test(alg): {
      const crv = EC_CURVE[alg]
      if (key.crv !== crv) return { status: 'mismatch', error: `${alg} needs a ${crv} key; this key is ${key.crv}` }
      if (sig.length !== EC_SIZE[crv] * 2) return { status: 'invalid', error: `an ${alg} signature is ${EC_SIZE[crv] * 2} bytes; this one is ${sig.length}` }
      importAlg = { name: 'ECDSA', namedCurve: crv }; verifyAlg = { name: 'ECDSA', hash }; break
    }
    case /^HS/.test(alg): importAlg = { name: 'HMAC', hash }; verifyAlg = { name: 'HMAC' }; break
    default:
      if (key.crv !== 'Ed25519') return { status: 'unsupported', error: `EdDSA with ${key.crv} cannot be verified here (Ed25519 only)` }
      importAlg = { name: 'Ed25519' }; verifyAlg = { name: 'Ed25519' }
  }
  let cryptoKey
  try {
    if (fam === 'oct') {
      const raw = key ? fromB64url(key.k || '') : secret
      cryptoKey = await subtle.importKey('raw', raw, importAlg, false, ['verify'])
    } else {
      // Only the public members: alg, use and key_ops in a JWKS can make an import refuse.
      const pub = { kty: key.kty, n: key.n, e: key.e, crv: key.crv, x: key.x, y: key.y, ext: true }
      for (const k of Object.keys(pub)) if (pub[k] === undefined) delete pub[k]
      let jwk = pub
      if (key.kty === 'RSA' && !key.n && Array.isArray(key.x5c) && key.x5c[0]) jwk = spkiToJwk(parseCertificate(b64ToBytes(key.x5c[0])).key.spki)
      cryptoKey = await subtle.importKey('jwk', jwk, importAlg, false, ['verify'])
    }
  } catch (e) {
    if (fam === 'OKP') return { status: 'unsupported', error: `this browser cannot verify Ed25519: ${e.message}` }
    return { status: 'error', error: `the key cannot be read: ${e.message}` }
  }
  try {
    const ok = await subtle.verify(verifyAlg, cryptoKey, sig, dec.signingInput)
    return ok ? { ok: true } : { status: 'invalid', error: 'the signature does not verify with this key' }
  } catch (e) {
    return { status: 'error', error: `cannot verify ${alg}: ${e.message}` }
  }
}

// verifyJwt checks a decoded token's signature against the pasted key text. Returns
// { status: 'valid' | 'invalid' | 'nokey' | 'none' | 'mismatch' | 'unsupported' | 'error' |
// 'encrypted', detail, kid, source }.
export async function verifyJwt(dec, keyText, opts = {}) {
  if (dec.kind === 'jwe') return { status: 'encrypted', detail: `encrypted with ${dec.header.alg || '?'} / ${dec.header.enc || '?'} — only the holder of the private key can read the claims` }
  const alg = dec.header.alg
  if (String(alg || '').toLowerCase() === 'none') return { status: 'none', detail: 'alg none: there is no signature to verify' }
  if (!algFamily(alg)) return { status: 'unsupported', detail: `alg ${alg || '(missing)'} is not one this card verifies` }
  if (!globalThis.crypto?.subtle) return { status: 'error', detail: 'this browser has no WebCrypto (crypto.subtle) — use https or localhost' }
  const k = readKeys(keyText, opts)
  if (k.error) return { status: 'error', detail: k.error }
  if (!k.keys && !k.secret) return { status: 'nokey', detail: `paste the issuer's JWKS (for Keycloak: <realm URL>/protocol/openid-connect/certs), a public key or certificate${algFamily(alg) === 'oct' ? ', or the shared secret' : ''}` }
  if (k.secret) {
    const r = await verifyWith(dec, null, k.secret)
    return r.ok ? { status: 'valid', detail: `signature verified with the ${k.source}`, source: k.source } : { status: r.status, detail: r.error, source: k.source }
  }
  const pick = pickKeys(k.keys, dec.header)
  if (pick.error) return { status: pick.status, detail: pick.error, source: k.source }
  let last = null
  for (const key of pick.keys) {
    const r = await verifyWith(dec, key, null)
    if (r.ok) return { status: 'valid', detail: `signature verified with ${key.kid ? `kid "${key.kid}"` : `the ${k.source}`}`, kid: key.kid, source: k.source }
    last = r
  }
  return { status: last.status, detail: last.error + (pick.keys.length > 1 ? ` (tried ${pick.keys.length} keys)` : ''), source: k.source }
}

// analyzeJwt is the card's one call: decode, explain, verify. `now` is in milliseconds.
export async function analyzeJwt(text, keyText = '', { now = Date.now(), secretBase64 = false } = {}) {
  const dec = decodeJwt(text)
  if (dec.error) return { error: dec.error }
  const out = { kind: dec.kind, header: dec.header, payload: dec.payload, payloadText: dec.payloadText, now }
  out.verify = await verifyJwt(dec, keyText, { secretBase64 })
  if (dec.kind === 'jwe') {
    out.findings = [{ tone: 'info', text: 'an encrypted token (JWE): the header is readable, the claims are not' }]
    out.claims = []
    return out
  }
  out.findings = findings(dec, now)
  out.claims = explainClaims(dec.payload, now)
  return out
}
