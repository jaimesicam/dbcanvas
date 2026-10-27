// uri.js — the Connection URI inspector: splits a MySQL, PostgreSQL or MongoDB URI (or a libpq
// key=value string) into its parts with the password masked, and says what its TLS settings make
// the client do.

const SCHEMES = {
  mysql: 'MySQL', mysqlx: 'MySQL (X protocol)', postgres: 'PostgreSQL', postgresql: 'PostgreSQL',
  mongodb: 'MongoDB', 'mongodb+srv': 'MongoDB (DNS seed list)',
}

// What each TLS setting means for the client. Keys are lower-case parameter=value.
const TLS = {
  // PostgreSQL / libpq
  'sslmode=disable': 'no TLS at all',
  'sslmode=allow': 'plain first, TLS only if the server insists — no verification',
  'sslmode=prefer': 'TLS when the server offers it, plain otherwise — no verification (libpq default)',
  'sslmode=require': 'TLS required, but the server certificate is not verified',
  'sslmode=verify-ca': 'TLS, and the server certificate must chain to sslrootcert — the host name is not checked',
  'sslmode=verify-full': 'TLS, the certificate must chain to sslrootcert AND name the host dialled',
  // MySQL (Connector/J style and mysql client style)
  'ssl-mode=disabled': 'no TLS', 'ssl-mode=preferred': 'TLS when offered, no verification (client default)',
  'ssl-mode=required': 'TLS required, no verification', 'ssl-mode=verify_ca': 'TLS, certificate must chain to ssl-ca — host name not checked',
  'ssl-mode=verify_identity': 'TLS, certificate must chain to ssl-ca AND name the host dialled',
  'sslmode=disabled': 'no TLS', 'sslmode=preferred': 'TLS when offered, no verification',
  'sslmode=required': 'TLS required, no verification', 'sslmode=verify_ca': 'TLS, certificate must chain to the CA — host name not checked',
  'sslmode=verify_identity': 'TLS, certificate must chain to the CA AND name the host dialled',
  // MongoDB
  'tls=true': 'TLS on; the certificate is verified against tlsCAFile and the host name unless relaxed below',
  'ssl=true': 'TLS on (legacy name for tls=true)',
  'tls=false': 'no TLS', 'tlsallowinvalidcertificates=true': 'certificate NOT verified (and host name not checked)',
  'tlsallowinvalidhostnames=true': 'certificate verified, but the host name NOT checked',
  'tlsinsecure=true': 'certificate and host name NOT verified',
}

function mask(v) {
  return v ? '•'.repeat(Math.min(12, v.length)) : ''
}

// parseURI returns { engine, user, passwordSet, hosts: [{ host, port }], database, params: [{ key,
// value }], tls: [{ setting, meaning }], masked } or { error }.
export function parseURI(input) {
  const s = String(input ?? '').trim()
  if (!s) return { error: 'paste a connection URI' }
  if (!/^[a-z+]+:\/\//i.test(s) && /\w+=\S/.test(s)) return parseKeyValue(s)
  const m = /^([a-z+]+):\/\/(?:([^:@/]*)(?::([^@/]*))?@)?([^/?]*)(?:\/([^?]*))?(?:\?(.*))?$/i.exec(s)
  if (!m) return { error: 'not a URI of the form scheme://user:password@host:port/database?options' }
  const scheme = m[1].toLowerCase()
  const dec = (v) => { try { return decodeURIComponent(v || '') } catch { return v || '' } }
  const out = {
    engine: SCHEMES[scheme] || scheme, scheme,
    user: dec(m[2]), passwordSet: m[3] !== undefined && m[3] !== '',
    hosts: m[4].split(',').filter(Boolean).map((h) => {
      const v6 = /^\[([^\]]+)\](?::(\d+))?$/.exec(h)
      if (v6) return { host: v6[1], port: v6[2] || '' }
      const [host, port] = h.split(':')
      return { host, port: port || '' }
    }),
    database: dec(m[5]),
    params: (m[6] || '').split('&').filter(Boolean).map((kv) => {
      const [k, ...v] = kv.split('=')
      return { key: dec(k), value: dec(v.join('=')) }
    }),
  }
  out.tls = tlsOf(out.params, scheme)
  out.masked = s.replace(/(\/\/[^:@/]*:)([^@/]*)@/, (_, a, p) => `${a}${mask(p)}@`)
  return out
}

function parseKeyValue(s) {
  const params = []
  for (const m of s.matchAll(/(\w+)\s*=\s*(?:'((?:[^'\\]|\\.)*)'|(\S+))/g)) params.push({ key: m[1], value: m[2] ?? m[3] })
  const get = (k) => params.find((p) => p.key.toLowerCase() === k)?.value || ''
  const out = {
    engine: 'PostgreSQL (libpq key=value)', scheme: 'libpq', user: get('user'), passwordSet: !!get('password'),
    hosts: (get('host') || get('hostaddr')).split(',').filter(Boolean).map((h, i) => ({ host: h, port: get('port').split(',')[i] || get('port') })),
    database: get('dbname'),
    params: params.filter((p) => !['user', 'password', 'host', 'hostaddr', 'port', 'dbname'].includes(p.key.toLowerCase())),
  }
  out.tls = tlsOf(out.params, 'postgresql')
  out.masked = s.replace(/(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)/i, (_, a, p) => `${a}${mask(p.replace(/^'|'$/g, ''))}`)
  return out
}

function tlsOf(params, scheme) {
  const out = []
  for (const p of params) {
    const k = p.key.toLowerCase()
    const meaning = TLS[`${k}=${p.value.toLowerCase()}`]
    if (meaning) out.push({ setting: `${p.key}=${p.value}`, meaning })
    else if (/^(sslrootcert|sslcert|sslkey|ssl-ca|sslca|ssl-cert|ssl-key|tlscafile|tlscertificatekeyfile|servercertificate|trustcertificatekeystoreurl)$/.test(k)) {
      out.push({ setting: `${p.key}=${p.value}`, meaning: k.includes('key') && !k.includes('ca') ? 'client certificate / key the client presents' : 'the CA the server certificate is verified against' })
    }
  }
  if (!out.some((t) => /sslmode|ssl-mode|tls=|ssl=/i.test(t.setting))) {
    const dflt = scheme.startsWith('postgres') ? 'no sslmode: libpq defaults to prefer — TLS if offered, never verified'
      : scheme.startsWith('mysql') ? 'no ssl-mode: the client defaults to PREFERRED — TLS if offered, never verified'
        : scheme === 'mongodb+srv' ? 'mongodb+srv turns TLS on by default, verified' : 'no tls option: plain connection unless the server requires TLS'
    out.unshift({ setting: '(default)', meaning: dflt })
  }
  return out
}
