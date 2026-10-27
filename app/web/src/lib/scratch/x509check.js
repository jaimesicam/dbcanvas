// x509check.js — the checks the TLS cards run, on what der.js read. The same four the K3D
// Certificates tab makes server-side (app/k3dcerts.go), so a Secret pasted here and the same
// Secret on that tab give the same verdict:
//
//   dates   the leaf is inside its validity window (a warning under 14 days)
//   chain   tls.crt verifies against ca.crt, with any further certificates in tls.crt as
//           intermediates — what a TLS library does
//   strict  the same with no intermediates — `openssl verify -CAfile ca.crt tls.crt`, which is
//           what Percona XtraDB Cluster's SST runs on a joiner
//   key     the private key is the certificate's
//
// Signatures are checked with the browser's WebCrypto (crypto.subtle), never by hand.

import { pemBlocks, parseCertificate, parseCSR, parsePrivateKey, hex, read, children, integerBytes } from './der.js'

export const SOON_DAYS = 14
const DAY = 86400000

// parseBundle reads every PEM block in a text: certificates, requests and keys.
export function parseBundle(text) {
  const out = { certs: [], csrs: [], keys: [], errors: [] }
  for (const b of pemBlocks(text)) {
    if (!b.der) { out.errors.push(`${b.type}: ${b.error}`); continue }
    try {
      if (b.type === 'CERTIFICATE') out.certs.push(parseCertificate(b.der))
      else if (b.type === 'CERTIFICATE REQUEST' || b.type === 'NEW CERTIFICATE REQUEST') out.csrs.push(parseCSR(b.der))
      else if (b.type.endsWith('PRIVATE KEY')) out.keys.push(parsePrivateKey(b.type, b.der))
    } catch (e) {
      out.errors.push(`${b.type}: ${e.message}`)
    }
  }
  return out
}

// ecdsaRaw turns a DER ECDSA signature (SEQUENCE { r, s }) into the r||s WebCrypto verifies.
function ecdsaRaw(sig, curve) {
  const size = { 'P-256': 32, 'P-384': 48, 'P-521': 66 }[curve] || 32
  const [r, s] = children(read(sig)).map(integerBytes)
  const out = new Uint8Array(size * 2)
  out.set(r.slice(-size), size - Math.min(size, r.length))
  out.set(s.slice(-size), size * 2 - Math.min(size, s.length))
  return out
}

// verifySignature checks that `signer`'s public key signed `signed` (a certificate or a CSR).
// Returns { ok, error }; an algorithm this browser cannot verify is said so, not called invalid.
export async function verifySignature(signed, signerKey) {
  const subtle = globalThis.crypto?.subtle
  if (!subtle) return { ok: false, error: 'this browser has no WebCrypto (crypto.subtle) — use https or localhost' }
  const alg = signed.sigAlg
  try {
    let importAlg, verifyAlg, sig = signed.signature
    switch (alg.kind) {
      case 'rsa':
        importAlg = { name: 'RSASSA-PKCS1-v1_5', hash: alg.hash }
        verifyAlg = { name: 'RSASSA-PKCS1-v1_5' }
        break
      case 'pss':
        importAlg = { name: 'RSA-PSS', hash: alg.hash }
        verifyAlg = { name: 'RSA-PSS', saltLength: alg.saltLength }
        break
      case 'ecdsa':
        importAlg = { name: 'ECDSA', namedCurve: signerKey.curve }
        verifyAlg = { name: 'ECDSA', hash: alg.hash }
        sig = ecdsaRaw(sig, signerKey.curve)
        break
      case 'ed25519':
        importAlg = { name: 'Ed25519' }
        verifyAlg = { name: 'Ed25519' }
        break
      default:
        return { ok: false, error: `signature algorithm ${alg.name} is not supported here` }
    }
    const key = await subtle.importKey('spki', signerKey.spki, importAlg, false, ['verify'])
    const ok = await subtle.verify(verifyAlg, key, sig, signed.tbs)
    return ok ? { ok: true } : { ok: false, error: 'the signature does not verify' }
  } catch (e) {
    return { ok: false, error: `cannot verify ${alg.name} in this browser: ${e.message}` }
  }
}

const sameName = (a, b) => hex(a.der) === hex(b.der)

// buildChain walks from the leaf to a certificate in `roots`, using `inters` for the links in
// between. Each link must be the named issuer, a CA, inside its dates, and must have signed the
// one below it — the rules a TLS library applies. Returns { ok, path, error }.
export async function buildChain(leaf, inters, roots, now) {
  const path = [leaf]
  let cur = leaf
  for (let depth = 0; depth < 8; depth++) {
    // Reached a root: the current certificate IS one of them (a self-signed leaf in its own ca.crt).
    if (roots.some((r) => hex(r.der) === hex(cur.der))) return { ok: true, path }
    const candidates = [...roots.map((c) => ({ c, root: true })), ...inters.map((c) => ({ c, root: false }))]
      .filter(({ c }) => sameName(c.subject, cur.issuer) && hex(c.der) !== hex(cur.der))
      .filter(({ c }) => !cur.ext.aki || !c.ext.ski || cur.ext.aki === c.ext.ski)
    if (!candidates.length) {
      return { ok: false, path, error: `no certificate for the issuer "${cur.issuer.text}"${depth === 0 ? ' in ca.crt' : ''}` }
    }
    let lastErr = ''
    let next = null
    for (const cand of candidates) {
      const { c } = cand
      if (!c.ext.isCA && !(cand.root && c.version === 1)) { lastErr = `"${c.subject.text}" is not a CA`; continue }
      if (now < c.notBefore || now > c.notAfter) { lastErr = `"${c.subject.text}" is outside its validity dates`; continue }
      const v = await verifySignature(cur, c.key)
      if (!v.ok) { lastErr = `"${c.subject.text}": ${v.error}`; continue }
      next = cand
      break
    }
    if (!next) return { ok: false, path, error: lastErr }
    path.push(next.c)
    if (next.root) return { ok: true, path }
    cur = next.c
  }
  return { ok: false, path, error: 'the chain is longer than 8 certificates' }
}

// keyMatches reports whether a parsed private key is the certificate's.
export async function keyMatches(cert, key) {
  if (!key) return null
  if (key.type === 'encrypted') return { ok: false, detail: 'the private key is encrypted — it cannot be compared' }
  if (key.type === 'RSA' && cert.key.type === 'RSA') {
    return key.n === cert.key.n && key.e === cert.key.e
      ? { ok: true, detail: 'the private key matches the certificate' }
      : { ok: false, detail: 'the private key does NOT match the certificate\'s public key' }
  }
  let point = key.point
  if (!point && key.pkcs8 && globalThis.crypto?.subtle) {
    // Ed25519, or an EC key that does not carry its public point: let WebCrypto derive it.
    try {
      const alg = key.type === 'EC' ? { name: 'ECDSA', namedCurve: key.curve } : { name: 'Ed25519' }
      const k = await crypto.subtle.importKey('pkcs8', key.pkcs8, alg, true, ['sign'])
      const jwk = await crypto.subtle.exportKey('jwk', k)
      const b = (s) => hex(Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0)))
      point = key.type === 'EC' ? '04' + b(jwk.x) + b(jwk.y) : b(jwk.x)
    } catch (e) {
      return { ok: false, detail: `cannot read the private key's public half in this browser: ${e.message}` }
    }
  }
  if (!point) return { ok: false, detail: `cannot compare a ${key.type} key with a ${cert.key.type} certificate` }
  return point === cert.key.point
    ? { ok: true, detail: 'the private key matches the certificate' }
    : { ok: false, detail: 'the private key does NOT match the certificate\'s public key' }
}

// hostMatches applies RFC 6125 name matching: SAN DNS names (one leftmost wildcard label), SAN IP
// addresses for an address, and never the Common Name — what modern clients do.
export function hostMatches(cert, host) {
  const h = String(host || '').trim().toLowerCase().replace(/\.$/, '')
  if (!h) return null
  if (/^[\d.]+$/.test(h) || h.includes(':')) {
    return cert.ext.san.ips.includes(h)
  }
  return cert.ext.san.dns.some((d) => {
    const n = d.toLowerCase().replace(/\.$/, '')
    if (!n.startsWith('*.')) return n === h
    const rest = n.slice(2)
    const dot = h.indexOf('.')
    return dot > 0 && h.slice(dot + 1) === rest
  })
}

// checkBundle runs the four checks on a tls.crt / ca.crt / tls.key set, the way k3dcerts.go does.
export async function checkBundle({ tlsCrt, caCrt = '', tlsKey = '', now = new Date() }) {
  const chainP = parseBundle(tlsCrt)
  const caP = parseBundle(caCrt)
  const keyP = parseBundle(tlsKey)
  const res = { chain: chainP.certs, ca: caP.certs, csrs: chainP.csrs, checks: [], status: 'valid',
    errors: [...chainP.errors, ...caP.errors, ...keyP.errors] }
  // A bundle pasted as one blob may carry its key alongside.
  const key = keyP.keys[0] || chainP.keys[0] || null
  if (!chainP.certs.length) {
    res.status = chainP.csrs.length ? 'csr' : 'invalid'
    return res
  }
  const leaf = chainP.certs[0]
  const add = (name, ok, detail) => res.checks.push({ name, ok, detail })

  if (now < leaf.notBefore) add('dates', false, `not valid until ${leaf.notBefore.toISOString()}`)
  else if (now > leaf.notAfter) add('dates', false, `expired ${leaf.notAfter.toISOString()}`)
  else add('dates', true, `valid until ${leaf.notAfter.toISOString()} (${Math.floor((leaf.notAfter - now) / DAY)} days)`)

  // With no ca.crt, a self-signed leaf is its own root; anything else has nothing to verify against.
  const roots = caP.certs.length ? caP.certs : (leaf.selfIssued ? [leaf] : [])
  if (!roots.length) {
    add('chain', false, 'no ca.crt to verify against — paste the CA below, or a bundle ending in it')
  } else {
    const full = await buildChain(leaf, chainP.certs.slice(1), roots, now)
    add('chain', full.ok, full.ok ? `chains to "${full.path[full.path.length - 1].subject.text}" in ${full.path.length - 1} step(s)` : full.error)
    res.path = full.path
    if (chainP.certs.length > 1) {
      const strict = await buildChain(leaf, [], roots, now)
      add('strict', strict.ok, strict.ok
        ? 'verifies against ca.crt alone, as `openssl verify -CAfile` does'
        : `fails \`openssl verify -CAfile ca.crt tls.crt\` (no intermediates), which PXC's SST runs: ${strict.error}`)
    }
  }
  const km = await keyMatches(leaf, key)
  if (km) add('key', km.ok, km.detail)

  if (res.checks.some((c) => !c.ok)) res.status = 'invalid'
  else if (leaf.notAfter - now < SOON_DAYS * DAY) res.status = 'warning'
  return res
}

// summarize says in a sentence or two what a certificate is for. It describes the certificate;
// whether a client accepts it is the checks' job, and the summary says which ones failed.
export function summarize(cert, res, now = new Date()) {
  const names = [...cert.ext.san.dns, ...cert.ext.san.ips]
  const who = names.length
    ? `Identifies ${names[0]}${names.length > 1 ? ` and ${names.length - 1} more name${names.length > 2 ? 's' : ''}` : ''}`
    : `Identifies "${cert.subject.cn || cert.subject.text}" (no SAN names, so modern clients will not match it to a host)`
  const by = cert.selfIssued ? 'self-signed' : `issued by ${cert.issuer.cn || cert.issuer.text}`
  const days = Math.floor((cert.notAfter - now) / DAY)
  const when = days < 0 ? `expired ${-days} days ago` : `valid for ${days} more days`
  const use = cert.ext.extKeyUsage.length ? cert.ext.extKeyUsage.join(' and ') : (cert.ext.isCA ? 'a CA' : 'no extended key usage')
  const parts = [`${who}; ${by}; ${when}; ${use}; ${cert.key.label} key.`]
  if (res?.checks?.length) {
    const failed = res.checks.filter((c) => !c.ok).map((c) => c.name)
    parts.push(failed.length ? `Failed: ${failed.join(', ')}.` : 'Every check passed.')
  }
  return parts.join(' ')
}
