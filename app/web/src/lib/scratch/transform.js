// transform.js — the Universal Transformer's conversions, and the Kubernetes Secret reader the
// Secret and TLS cards share. Pure functions over strings; YAML is the one thing not hand-written
// (the `yaml` package, loaded on first use so it costs nothing until a card needs it).

let yamlMod = null
export async function loadYaml() {
  if (!yamlMod) yamlMod = await import('yaml')
  return yamlMod
}

// jsonError turns a JSON.parse error into "line L, column C: message" where the engine gives a
// position.
function jsonError(text, e) {
  const m = /position (\d+)/.exec(e.message)
  if (!m) return e.message
  const pos = Number(m[1])
  const before = text.slice(0, pos).split('\n')
  return `line ${before.length}, column ${before[before.length - 1].length + 1}: ${e.message.replace(/ in JSON at position \d+.*/, '')}`
}

export function parseJSON(text) {
  try { return { value: JSON.parse(text) } } catch (e) { return { error: jsonError(text, e) } }
}

export function sortKeys(v) {
  if (Array.isArray(v)) return v.map(sortKeys)
  if (v && typeof v === 'object') return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sortKeys(v[k])]))
  return v
}

// utf8 Base64 both ways. Decoding says when the bytes are not text, and shows them as hex then.
export function b64encode(text) {
  const bytes = new TextEncoder().encode(text)
  let s = ''
  for (const b of bytes) s += String.fromCharCode(b)
  return btoa(s)
}

export function b64decode(text) {
  const clean = String(text).trim().replace(/\s+/g, '').replace(/-/g, '+').replace(/_/g, '/')
  const padded = clean + '='.repeat((4 - (clean.length % 4)) % 4)
  let bin
  try { bin = atob(padded) } catch { return { error: 'not valid Base64' } }
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
  // Text is valid UTF-8 with no control characters besides tab, newline and carriage return;
  // anything else (a keystore, a key in DER form) is shown as hex.
  const asHex = () => ({ value: Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join(' '), binary: true })
  if (bytes.some((b) => b < 0x20 && b !== 0x09 && b !== 0x0a && b !== 0x0d) || bytes.includes(0x7f)) return asHex()
  try {
    return { value: new TextDecoder('utf-8', { fatal: true }).decode(bytes) }
  } catch {
    return asHex()
  }
}

export const FORMATS = ['json', 'json-min', 'json-sorted', 'yaml', 'base64', 'base64-decoded', 'url', 'url-decoded']
export const FORMAT_LABEL = {
  json: 'JSON (pretty)', 'json-min': 'JSON (minified)', 'json-sorted': 'JSON (sorted keys)', yaml: 'YAML',
  base64: 'Base64 encode', 'base64-decoded': 'Base64 decode', url: 'URL encode', 'url-decoded': 'URL decode',
}

// convert reads `text` as `from` (json | yaml | text) and writes it as `to`. Returns
// { value } or { error }.
export async function convert(text, from, to) {
  const src = String(text ?? '')
  if (!src.trim()) return { value: '' }
  switch (to) {
    case 'base64': return { value: b64encode(src) }
    case 'base64-decoded': return b64decode(src)
    case 'url': return { value: encodeURIComponent(src) }
    case 'url-decoded':
      try { return { value: decodeURIComponent(src.replace(/\+/g, ' ')) } } catch { return { error: 'not valid percent-encoding' } }
    default: break
  }
  let data
  if (from === 'yaml') {
    const Y = await loadYaml()
    try {
      const docs = Y.parseAllDocuments(src)
      const bad = docs.find((d) => d.errors.length)
      if (bad) return { error: bad.errors[0].message }
      data = docs.length === 1 ? docs[0].toJS() : docs.map((d) => d.toJS())
    } catch (e) { return { error: e.message } }
  } else {
    const p = parseJSON(src)
    if (p.error) return { error: p.error }
    data = p.value
  }
  switch (to) {
    case 'json': return { value: JSON.stringify(data, null, 2) }
    case 'json-min': return { value: JSON.stringify(data) }
    case 'json-sorted': return { value: JSON.stringify(sortKeys(data), null, 2) }
    case 'yaml': {
      const Y = await loadYaml()
      return { value: Y.stringify(data, { lineWidth: 0 }) }
    }
    default: return { error: `unknown format ${to}` }
  }
}

// guessFormat says whether text reads as JSON or YAML (the transformer's input side).
export function guessFormat(text) {
  const t = String(text || '').trim()
  if (!t) return 'json'
  if (/^[[{]/.test(t) && !parseJSON(t).error) return 'json'
  return /^[\w"'-][^\n]*:\s|^-\s|^---/m.test(t) ? 'yaml' : 'json'
}

// readSecret reads a Kubernetes Secret pasted as YAML or JSON (or a `kubectl get secret -o yaml`
// List) into { name, type, entries: [{ key, value, binary }] }, decoding `data` and taking
// `stringData` as it is.
export async function readSecret(text) {
  const src = String(text || '')
  let obj
  const j = parseJSON(src.trim())
  if (!j.error) obj = j.value
  else {
    const Y = await loadYaml()
    try { obj = Y.parse(src) } catch (e) { return { error: e.message } }
  }
  if (obj?.kind === 'List' && Array.isArray(obj.items)) obj = obj.items.find((i) => i?.kind === 'Secret') || obj.items[0]
  if (!obj || typeof obj !== 'object' || (!obj.data && !obj.stringData)) {
    return { error: 'not a Kubernetes Secret — expected a `data:` or `stringData:` map' }
  }
  const entries = []
  for (const [key, v] of Object.entries(obj.data || {})) {
    const d = b64decode(String(v ?? ''))
    entries.push({ key, value: d.error ? `(${d.error})` : d.value, binary: !!d.binary, error: d.error })
  }
  for (const [key, v] of Object.entries(obj.stringData || {})) entries.push({ key, value: String(v ?? ''), stringData: true })
  entries.sort((a, b) => a.key.localeCompare(b.key))
  return { name: obj.metadata?.name || '', namespace: obj.metadata?.namespace || '', type: obj.type || '', entries }
}
