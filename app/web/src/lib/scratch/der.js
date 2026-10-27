// der.js — a DER (ASN.1) reader and the X.509 structures the Scratchboard's TLS cards read:
// certificates, certificate requests (PKCS#10) and private keys (PKCS#1, PKCS#8, SEC1).
//
// Hand-written on purpose, and deliberately small: it reads exactly what those structures use —
// SEQUENCE, SET, INTEGER, BIT STRING, OCTET STRING, OBJECT IDENTIFIER, BOOLEAN, the string types,
// UTCTime / GeneralizedTime and context-specific tags — and nothing it does involves a secret.
// Every cryptographic step (checking a signature) is the browser's WebCrypto, in x509check.js.
//
// Everything runs in the page: nothing pasted into a card is sent anywhere.

// ------------------------------------------------------------------ PEM

// pemBlocks splits text into its PEM blocks: { type, der (Uint8Array), text }.
export function pemBlocks(text) {
  const out = []
  const re = /-----BEGIN ([A-Z0-9 ]+)-----([\s\S]*?)-----END \1-----/g
  let m
  while ((m = re.exec(String(text || '')))) {
    const body = m[2].split('\n').filter((l) => !l.includes(':')).join('').replace(/\s+/g, '')
    try {
      out.push({ type: m[1], der: b64ToBytes(body), text: m[0] })
    } catch {
      out.push({ type: m[1], der: null, text: m[0], error: 'the base64 body does not decode' })
    }
  }
  return out
}

export function b64ToBytes(b64) {
  const s = atob(b64.replace(/-/g, '+').replace(/_/g, '/'))
  const out = new Uint8Array(s.length)
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i)
  return out
}

export function bytesToB64(bytes) {
  let s = ''
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i])
  return btoa(s)
}

export function hex(bytes, sep = '') {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join(sep).toUpperCase()
}

// ------------------------------------------------------------------ TLV

// read reads one TLV at off: { tag, cls, constructed, num, start (value), end, hdr, bytes }.
export function read(bytes, off = 0) {
  if (off + 2 > bytes.length) throw new Error('truncated DER')
  const tag = bytes[off]
  let len = bytes[off + 1]
  let p = off + 2
  if (len & 0x80) {
    const n = len & 0x7f
    if (n === 0 || n > 4) throw new Error('unsupported DER length')
    len = 0
    for (let i = 0; i < n; i++) len = len * 256 + bytes[p++]
  }
  if (p + len > bytes.length) throw new Error('truncated DER')
  return { tag, cls: tag >> 6, constructed: !!(tag & 0x20), num: tag & 0x1f, start: p, end: p + len, hdr: off, bytes }
}

// children lists the TLVs inside a constructed one.
export function children(t) {
  const out = []
  for (let p = t.start; p < t.end;) {
    const c = read(t.bytes, p)
    out.push(c)
    p = c.end
  }
  return out
}

export const value = (t) => t.bytes.subarray(t.start, t.end)
export const raw = (t) => t.bytes.subarray(t.hdr, t.end)
const isCtx = (t, n) => t.cls === 2 && t.num === n

export function oid(t) {
  const b = value(t)
  const parts = [Math.floor(b[0] / 40), b[0] % 40]
  let v = 0
  for (let i = 1; i < b.length; i++) {
    v = v * 128 + (b[i] & 0x7f)
    if (!(b[i] & 0x80)) { parts.push(v); v = 0 }
  }
  return parts.join('.')
}

// integerBytes is an INTEGER's magnitude, without the sign-padding zero.
export function integerBytes(t) {
  let b = value(t)
  while (b.length > 1 && b[0] === 0) b = b.subarray(1)
  return b
}

function bitString(t) {
  const b = value(t)
  return { unused: b[0], bytes: b.subarray(1) }
}

function str(t) {
  const b = value(t)
  switch (t.tag) {
    case 0x0c: return new TextDecoder().decode(b) // UTF8String
    case 0x1e: { // BMPString
      let s = ''
      for (let i = 0; i + 1 < b.length; i += 2) s += String.fromCharCode((b[i] << 8) | b[i + 1])
      return s
    }
    default: return String.fromCharCode(...b) // Printable, IA5, T61, Visible
  }
}

function time(t) {
  const s = String.fromCharCode(...value(t))
  const m = t.tag === 0x17
    ? /^(\d\d)(\d\d)(\d\d)(\d\d)(\d\d)(\d\d)?Z$/.exec(s)
    : /^(\d{4})(\d\d)(\d\d)(\d\d)(\d\d)(\d\d)?(?:\.\d+)?Z$/.exec(s)
  if (!m) return null
  let y = Number(m[1])
  if (t.tag === 0x17) y += y < 50 ? 2000 : 1900
  return new Date(Date.UTC(y, Number(m[2]) - 1, Number(m[3]), Number(m[4]), Number(m[5]), Number(m[6] || 0)))
}

// ------------------------------------------------------------------ names

const NAME_OIDS = {
  '2.5.4.3': 'CN', '2.5.4.6': 'C', '2.5.4.7': 'L', '2.5.4.8': 'ST', '2.5.4.9': 'STREET',
  '2.5.4.10': 'O', '2.5.4.11': 'OU', '2.5.4.5': 'SERIALNUMBER', '2.5.4.17': 'POSTALCODE',
  '0.9.2342.19200300.100.1.25': 'DC', '0.9.2342.19200300.100.1.1': 'UID',
  '1.2.840.113549.1.9.1': 'emailAddress',
}

// name reads a Name into its attributes, in DER order, and the string Go's pkix.Name.String()
// prints (most specific first, comma-separated) — the same form the K3D Certificates tab shows.
function name(t) {
  const attrs = []
  for (const rdn of children(t)) {
    for (const atv of children(rdn)) {
      const [o, v] = children(atv)
      const id = oid(o)
      attrs.push({ oid: id, key: NAME_OIDS[id] || id, value: str(v) })
    }
  }
  const text = [...attrs].reverse().map((a) => `${a.key}=${a.value}`).join(',')
  return { attrs, text, der: raw(t), cn: attrs.find((a) => a.key === 'CN')?.value || '' }
}

// ------------------------------------------------------------------ algorithms

export const SIG_ALGS = {
  '1.2.840.113549.1.1.5': { name: 'SHA1-RSA', kind: 'rsa', hash: 'SHA-1' },
  '1.2.840.113549.1.1.11': { name: 'SHA256-RSA', kind: 'rsa', hash: 'SHA-256' },
  '1.2.840.113549.1.1.12': { name: 'SHA384-RSA', kind: 'rsa', hash: 'SHA-384' },
  '1.2.840.113549.1.1.13': { name: 'SHA512-RSA', kind: 'rsa', hash: 'SHA-512' },
  '1.2.840.113549.1.1.10': { name: 'RSA-PSS', kind: 'pss' },
  '1.2.840.10045.4.3.2': { name: 'ECDSA-SHA256', kind: 'ecdsa', hash: 'SHA-256' },
  '1.2.840.10045.4.3.3': { name: 'ECDSA-SHA384', kind: 'ecdsa', hash: 'SHA-384' },
  '1.2.840.10045.4.3.4': { name: 'ECDSA-SHA512', kind: 'ecdsa', hash: 'SHA-512' },
  '1.3.101.112': { name: 'Ed25519', kind: 'ed25519' },
}
const HASH_OIDS = { '1.3.14.3.2.26': 'SHA-1', '2.16.840.1.101.3.4.2.1': 'SHA-256', '2.16.840.1.101.3.4.2.2': 'SHA-384', '2.16.840.1.101.3.4.2.3': 'SHA-512' }
export const CURVES = { '1.2.840.10045.3.1.7': 'P-256', '1.3.132.0.34': 'P-384', '1.3.132.0.35': 'P-521' }

function algorithm(t) {
  const [o, params] = children(t)
  const id = oid(o)
  const alg = { oid: id, ...(SIG_ALGS[id] || { name: id, kind: 'unknown' }) }
  if (alg.kind === 'pss') {
    // RSASSA-PSS-params: [0] hashAlgorithm, [1] maskGen, [2] saltLength — defaults SHA-1 / 20.
    alg.hash = 'SHA-1'
    alg.saltLength = 20
    for (const p of params ? children(params) : []) {
      if (isCtx(p, 0)) alg.hash = HASH_OIDS[oid(children(children(p)[0])[0])] || alg.hash
      if (isCtx(p, 2)) alg.saltLength = Number(BigInt('0x' + (hex(integerBytes(children(p)[0])) || '0')))
    }
    alg.name = `RSA-PSS-${alg.hash.replace('SHA-', 'SHA')}`
  }
  return alg
}

// publicKey reads a SubjectPublicKeyInfo: its algorithm, size or curve, and the parts a private
// key is matched against.
function publicKey(t) {
  const [algT, bits] = children(t)
  const [o, params] = children(algT)
  const id = oid(o)
  const key = { spki: raw(t), oid: id }
  const bs = bitString(bits).bytes
  if (id === '1.2.840.113549.1.1.1' || id === '1.2.840.113549.1.1.10') {
    const [n, e] = children(read(bs))
    key.type = 'RSA'
    key.n = hex(integerBytes(n))
    key.e = hex(integerBytes(e))
    key.bits = integerBytes(n).length * 8 - Math.clz32(integerBytes(n)[0]) + 24
    key.label = `RSA ${key.bits}`
  } else if (id === '1.2.840.10045.2.1') {
    key.type = 'EC'
    key.curve = (params && CURVES[oid(params)]) || 'unknown curve'
    key.point = hex(bs)
    key.label = `ECDSA ${key.curve}`
  } else if (id === '1.3.101.112') {
    key.type = 'Ed25519'
    key.point = hex(bs)
    key.label = 'Ed25519'
  } else {
    key.type = id
    key.label = id
  }
  return key
}

// ------------------------------------------------------------------ extensions

const KEY_USAGE = ['digital signature', 'non-repudiation', 'key encipherment', 'data encipherment',
  'key agreement', 'cert sign', 'CRL sign', 'encipher only', 'decipher only']
const EKU = {
  '1.3.6.1.5.5.7.3.1': 'server auth', '1.3.6.1.5.5.7.3.2': 'client auth', '1.3.6.1.5.5.7.3.3': 'code signing',
  '1.3.6.1.5.5.7.3.4': 'email', '1.3.6.1.5.5.7.3.8': 'time stamping', '1.3.6.1.5.5.7.3.9': 'OCSP signing',
  '2.5.29.37.0': 'any',
}

function ip(b) {
  if (b.length === 4) return Array.from(b).join('.')
  const parts = []
  for (let i = 0; i < b.length; i += 2) parts.push(((b[i] << 8) | b[i + 1]).toString(16))
  return parts.join(':').replace(/(^|:)0(:0)+(:|$)/, '::')
}

function generalNames(t, into) {
  for (const g of children(t)) {
    if (g.cls !== 2) continue
    const v = value(g)
    if (g.num === 2) into.dns.push(String.fromCharCode(...v))
    else if (g.num === 7) into.ips.push(ip(v))
    else if (g.num === 6) into.uris.push(String.fromCharCode(...v))
    else if (g.num === 1) into.emails.push(String.fromCharCode(...v))
  }
}

function extensions(list) {
  const ext = { san: { dns: [], ips: [], uris: [], emails: [] }, keyUsage: [], extKeyUsage: [], isCA: false, pathLen: null,
    basicConstraints: false, ski: '', aki: '', other: [] }
  for (const e of list) {
    const parts = children(e)
    const id = oid(parts[0])
    const critical = parts.length === 3 && value(parts[1])[0] !== 0
    const body = read(value(parts[parts.length - 1]))
    switch (id) {
      case '2.5.29.17': generalNames(body, ext.san); break
      case '2.5.29.15': {
        const { bytes } = bitString(body)
        KEY_USAGE.forEach((u, i) => { if (bytes[i >> 3] & (0x80 >> (i & 7))) ext.keyUsage.push(u) })
        break
      }
      case '2.5.29.37': ext.extKeyUsage = children(body).map((o) => EKU[oid(o)] || oid(o)); break
      case '2.5.29.19':
        ext.basicConstraints = true
        for (const c of children(body)) {
          if (c.tag === 0x01) ext.isCA = value(c)[0] !== 0
          if (c.tag === 0x02) ext.pathLen = Number(BigInt('0x' + (hex(integerBytes(c)) || '0')))
        }
        break
      case '2.5.29.14': ext.ski = hex(value(body), ':'); break
      case '2.5.29.35':
        for (const c of children(body)) if (isCtx(c, 0)) ext.aki = hex(value(c), ':')
        break
      default: ext.other.push({ oid: id, critical })
    }
  }
  return ext
}

// ------------------------------------------------------------------ certificate

// parseCertificate reads a DER certificate. tbs, signature and the algorithms are kept for
// x509check.js to verify with WebCrypto.
export function parseCertificate(der) {
  const cert = read(der)
  const [tbsT, sigAlgT, sigT] = children(cert)
  const f = children(tbsT)
  let i = 0
  let version = 1
  if (isCtx(f[0], 0)) { version = Number(value(children(f[0])[0])[0]) + 1; i++ }
  const serial = hex(integerBytes(f[i++]))
  i++ // the inner signature algorithm repeats the outer one
  const issuer = name(f[i++])
  const [nb, na] = children(f[i++])
  const subject = name(f[i++])
  const key = publicKey(f[i++])
  let ext = extensions([])
  for (; i < f.length; i++) if (isCtx(f[i], 3)) ext = extensions(children(children(f[i])[0]))
  return {
    kind: 'certificate', version, serial, issuer, subject, key,
    notBefore: time(nb), notAfter: time(na),
    sigAlg: algorithm(sigAlgT), signature: bitString(sigT).bytes,
    tbs: raw(tbsT), der: raw(cert), ext,
    selfIssued: hex(issuer.der) === hex(subject.der),
  }
}

// parseCSR reads a PKCS#10 certificate request, with the names it asks for (extensionRequest).
export function parseCSR(der) {
  const req = read(der)
  const [infoT, sigAlgT, sigT] = children(req)
  const [, subjT, spkiT, attrsT] = children(infoT)
  let ext = extensions([])
  if (attrsT && isCtx(attrsT, 0)) {
    for (const a of children(attrsT)) {
      const [o, vals] = children(a)
      if (oid(o) === '1.2.840.113549.1.9.14') ext = extensions(children(children(vals)[0]))
    }
  }
  return {
    kind: 'csr', subject: name(subjT), key: publicKey(spkiT), ext,
    sigAlg: algorithm(sigAlgT), signature: bitString(sigT).bytes, tbs: raw(infoT),
  }
}

// parsePrivateKey reads the public half of a private key where the key carries it: n/e for RSA
// (PKCS#1, or PKCS#8 around it), the point for EC when SEC1 includes it. `pkcs8` is set when
// WebCrypto has to derive what is missing (Ed25519, an EC key without its point).
export function parsePrivateKey(type, der) {
  const t = read(der)
  if (type === 'RSA PRIVATE KEY') return rsaPrivate(t)
  if (type === 'EC PRIVATE KEY') return ecPrivate(t, null)
  if (type === 'ENCRYPTED PRIVATE KEY') return { type: 'encrypted' }
  // PKCS#8: version, algorithm, OCTET STRING private key
  const [, algT, keyT] = children(t)
  const [o, params] = children(algT)
  const id = oid(o)
  if (id === '1.2.840.113549.1.1.1' || id === '1.2.840.113549.1.1.10') return rsaPrivate(read(value(keyT)))
  if (id === '1.2.840.10045.2.1') return { ...ecPrivate(read(value(keyT)), params ? CURVES[oid(params)] : null), pkcs8: der }
  if (id === '1.3.101.112') return { type: 'Ed25519', pkcs8: der }
  return { type: id }
}

function rsaPrivate(t) {
  const [, n, e] = children(t)
  return { type: 'RSA', n: hex(integerBytes(n)), e: hex(integerBytes(e)) }
}

function ecPrivate(t, curve) {
  const out = { type: 'EC', curve }
  for (const c of children(t)) {
    if (isCtx(c, 0)) out.curve = CURVES[oid(children(c)[0])] || out.curve
    if (isCtx(c, 1)) out.point = hex(bitString(children(c)[0]).bytes)
  }
  return out
}
