// redact.js — the Redactor: masks addresses, host names, e-mail addresses, user names and secrets
// in a pasted text before it is shared. The same original gets the same replacement everywhere in
// the text (the first IP becomes 10.0.0.1, the next 10.0.0.2, …), and a replacement keeps the
// shape of what it replaces, so the text still reads — and still parses — afterwards.
//
// How it works: each rule finds spans; overlapping spans are settled by rule priority (a password
// inside a URI beats the e-mail address its `pw@host` looks like); then values found once in a
// clear context (a password, a user name, a host named in `host=`) are masked wherever else they
// occur. The mapping exists only in the returned object.
//
// Everything runs in the page: nothing pasted into a card is sent anywhere.

import { b64decode, b64encode } from './transform.js'

// CATEGORIES are the toggles, in the order the card shows them.
export const CATEGORIES = [
  { id: 'secrets', label: 'Passwords & secrets', on: true },
  { id: 'ipv4', label: 'IPv4', on: true },
  { id: 'ipv6', label: 'IPv6', on: true },
  { id: 'hosts', label: 'Host names', on: true },
  { id: 'emails', label: 'E-mail addresses', on: true },
  { id: 'users', label: 'User names', on: true },
  { id: 'macs', label: 'MAC addresses', on: true },
  { id: 'uuids', label: 'UUIDs (GTIDs keep their ranges)', on: false },
]
export const CATEGORY_LABEL = { ...Object.fromEntries(CATEGORIES.map((c) => [c.id, c.label])), words: 'Extra words' }
export const DEFAULT_OPTIONS = { ...Object.fromEntries(CATEGORIES.map((c) => [c.id, c.on])), keepWellKnown: true, style: 'numbered', words: '' }

// Rule priorities: lower wins an overlap.
const P = { pem: 0, k8s: 1, secret: 2, email: 3, user: 4, id: 5, ip: 6, ctxHost: 7, host: 8, word: 9, sweep: 10 }

// ------------------------------------------------------------------ vocabularies

// Last labels that make a dotted word a file name, not a host: my.cnf, mysqld.log, server-key.pem.
const FILE_EXT = new Set(('cnf cfg conf ini log err pid sock ibd ibt ibu frm myd myi sdi sql gz tgz bz2 xz zst lz4 tar zip ' +
  'txt json yaml yml xml csv tsv html htm md pem crt cer key csr der p12 pfx jks so sh bash py go js jsx mjs cjs ts tsx ' +
  'java class jar c h cc cpp hpp rb pl rs cs php lua out tmp bak old swp idx dat bin index service socket timer mount ' +
  'target lock db sqlite rpm deb in qp xbstream diff patch png jpg svg pdf toml env ps1 bat exe dll dylib a o').split(' '))
// Top-level labels that make a dotted word a host on their own.
const TLDS = new Set(('com net org io info biz edu gov mil int co local localdomain internal intranet lan corp home ' +
  'cloud dev app ai tech site online xyz svc arpa test example invalid us uk de fr jp cn br ru au ca nl eu ch se es ' +
  'pl dk fi ie nz sg hk kr tw mx ar za').split(' '))
// Domains that name software, not an installation: left as they are.
const PUBLIC_DOMAINS = ['mysql.com', 'oracle.com', 'percona.com', 'mariadb.com',
  'mariadb.org', 'mongodb.com', 'mongodb.org', 'postgresql.org', 'kubernetes.io', 'k8s.io', 'x-k8s.io', 'cert-manager.io',
  'github.com', 'githubusercontent.com', 'docker.io', 'docker.com', 'quay.io', 'gcr.io', 'golang.org', 'w3.org',
  'ietf.org', 'openssl.org', 'ubuntu.com', 'debian.org', 'redhat.com', 'centos.org', 'fedoraproject.org', 'openbao.org',
  'hashicorp.com', 'pgbackrest.org', 'patroni.readthedocs.io']
// Suffixes on their own (but not the names under them).
const BARE_SUFFIX = new Set(['cluster.local', 'svc.cluster.local', 'localdomain', 'localhost.localdomain'])
// Host suffixes a replacement keeps, so a Kubernetes service name still reads as one.
const KEEP_SUFFIX = ['.svc.cluster.local', '.cluster.local', '.svc', '.localdomain', '.local', '.internal']
// Accounts every installation has; kept unless "keep well-known" is off.
const WELL_KNOWN_USERS = new Set(['root', 'postgres', 'mysql.sys', 'mysql.session', 'mysql.infoschema', '__system',
  'system user', 'event_scheduler', 'mysqlxsys', 'unauthenticated user', 'admin'])
// Single words that are roles or programs rather than host names.
const HOST_STOP = new Set(['localhost', 'mysql', 'mysqld', 'mongod', 'mongos', 'postgres', 'primary', 'secondary',
  'master', 'replica', 'slave', 'source', 'default', 'true', 'false', 'null', 'none', 'nil', 'local', 'admin',
  'unknown', 'server', 'client', 'node', 'cluster', 'any', 'all', 'yes', 'no', 'on', 'off', 'auto', 'disabled',
  'enabled', 'required', 'preferred', 'self', 'internal', 'external', 'tcp', 'udp', 'unix', 'socket', 'ip'])

// A value that is already a placeholder, a variable or a yes/no is not a secret.
const PLACEHOLDER = /^(\*+|x{3,}|•+|\.{3}|<[^<>]*>|\$\{?[\w.-]+\}?|%s|\?|null|none|nil|true|false|yes|no|on|off|redacted|\[redacted\]|\[hidden\]|hidden|changeme)$/i
const PATHLIKE = /^(\/|\.\.?\/|~\/|[A-Za-z]:\\)|\.(pem|key|crt|cert|der|p12|pfx|jks|txt|json|ya?ml|cnf|conf)$/i

const MYSQL_CLI = /(?:^|[\s/'"`])(?:mysql|mysqladmin|mysqldump|mysqlpump|mysqlsh|mysqlbinlog|mysqlcheck|mysqlimport|mysqlslap|mysql_upgrade|mariadb[\w-]*|xtrabackup|innobackupex|pt-[\w-]+|mydumper|myloader)(?=[\s'"`]|$)/

// ------------------------------------------------------------------ shapes

const IPV4_RE = /(?<![\w.-])(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?![\w-]|\.\d)/g
export function isIPv4(s) {
  return /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$/.test(s)
}

// isIPv6 checks the grouping rules a candidate must follow: eight groups, or fewer around one `::`,
// optionally ending in a dotted IPv4 address. A candidate with no digit and fewer than eight groups
// (C++'s `a::b`) is not taken for one.
export function isIPv6(s) {
  if (!s.includes(':') || s.includes(':::')) return false
  const dbl = s.split('::').length - 1
  if (dbl > 1) return false
  let body = s
  let extra = 0
  const v4 = /:(\d+\.\d+\.\d+\.\d+)$/.exec(s)
  if (v4) {
    if (!isIPv4(v4[1])) return false
    body = s.slice(0, v4.index + 1) + '0'
    extra = 1
  }
  const groups = body.split(':')
  if (!groups.every((g) => /^[0-9a-f]{0,4}$/i.test(g))) return false
  const filled = groups.filter((g) => g !== '').length + extra
  if (dbl === 0 && filled !== 8) return false
  if (dbl === 1 && filled > 7) return false
  if (dbl === 1 && !/\d/.test(s)) return false
  if (dbl === 0 && groups.some((g) => g === '')) return false
  return true
}
const IPV6_RE = /(?<![\w:.])(?:[0-9a-f]{0,4}:){2,7}(?:\d{1,3}(?:\.\d{1,3}){3}|[0-9a-f]{0,4})(?![\w:]|\.\w)/gi

const MAC_RE = /(?<![\w:-])[0-9a-f]{2}([:-])(?:[0-9a-f]{2}\1){4}[0-9a-f]{2}(?![\w:-])/gi
const UUID_RE = /(?<![\w-])[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?![\w-])/gi
const EMAIL_RE = /(?<![\w.%+-])[a-z0-9][a-z0-9._%+-]*@(?:[a-z0-9-]+\.)+[a-z]{2,}(?![\w-]|\.[a-z0-9])/gi
const HOST_RE = /(?<![\w.@-])(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}(?![\w-]|\.[a-z0-9])/gi

// The documentation domains (example.com, .net, .org) are masked like any other, except in the
// shapes this file's own replacements take, so redacting a redacted text leaves it as it was.
const PLACEHOLDER_HOST = /^host\d+\.example\.net$/
const PLACEHOLDER_EMAIL = /^user\d+@example\.com$/i
const isPublic = (h) => BARE_SUFFIX.has(h) || PLACEHOLDER_HOST.test(h) || PUBLIC_DOMAINS.some((d) => h === d || h.endsWith('.' + d))

// hostPart trims a dotted word to its host: `db1.example.net.err` → `db1.example.net`, `my.cnf` →
// nothing. Returns the host, or '' when the word is a file name, a version or code.
function hostPart(word) {
  const labels = word.split('.')
  while (labels.length > 1 && FILE_EXT.has(labels[labels.length - 1].toLowerCase())) labels.pop()
  if (labels.length < 2) return ''
  const tld = labels[labels.length - 1].toLowerCase()
  if (!/^[a-z]{2,}$/.test(tld)) return ''
  const known = TLDS.has(tld) || (tld.length === 2 && labels.length >= 3 && !FILE_EXT.has(tld))
  if (!known) return ''
  const h = labels.join('.')
  return isPublic(h.toLowerCase()) ? '' : h
}

// A single-label host as it appears after `host=`, `@` or in a URI: db1, cluster1-pxc-0.
const SHORT_HOST = /^[a-z0-9][a-z0-9_-]{0,62}$/i
function hostLike(v, o) {
  if (!v || isIPv4(v) || isIPv6(v) || /^\d+$/.test(v)) return false
  if (o.keepWellKnown && HOST_STOP.has(v.toLowerCase())) return false
  if (SHORT_HOST.test(v)) return v.length >= 2 && /[a-z]/i.test(v)
  return /^[a-z0-9][\w-]*(\.[\w-]+)+\.?$/i.test(v) && !isPublic(v.toLowerCase()) && !FILE_EXT.has(v.split('.').pop().toLowerCase())
}

// splitWords turns the key of a key=value into its words: masterPassword → master password,
// MYSQL_ROOT_PASSWORD → mysql root password.
function keyWords(k) {
  return k.replace(/^-+/, '').replace(/([a-z0-9])([A-Z])/g, '$1 $2').toLowerCase().split(/[\s_.-]+/).filter(Boolean)
}
const SECRET_LAST = /^(?:\w*password|\w*passwd|passphrase|\w*secret|\w*token|pass|pwd|pw|key|apikey|credentials?|authentication_string)$/
function keyKind(k) {
  const w = keyWords(k)
  const last = w[w.length - 1] || ''
  const joined = w.join('_')
  const flat = w.join('')
  if (/^(authentication_string|auth_string|password_hash)$/.test(joined)) return 'secret'
  if (SECRET_LAST.test(last)) return 'secret'
  if (/^(user|username|usr|login|principal|principalname|pguser|dbuser|mysqluser)$/.test(last) || /^(username|principalname|dbuser|dbusername|mysqluser|pguser)$/.test(flat) || /^(u|user)$/.test(k.replace(/^-+/, ''))) return 'user'
  if (/^(host|hostname|hostaddr|servername|nodename|address|addr|remote|seeds?)$/.test(last) ||
      /^(wsrep_node_address|wsrep_cluster_address|wsrep_node_name|report_host|master_host|source_host|bind_address|server)$/.test(joined)) return 'host'
  return ''
}

// ------------------------------------------------------------------ the rules

function rules(src, o, push, sweep) {
  const { secrets: sweepSecrets, users: sweepUsers } = sweep
  const isLineStart = (i) => /^[ \t]*(?:- )?["']?$/.test(src.slice(src.lastIndexOf('\n', i - 1) + 1, i))

  // PEM private keys: the body goes, the armour stays.
  for (const m of src.matchAll(/-----BEGIN ((?:[A-Z0-9]+ )*)PRIVATE KEY-----([\s\S]*?)-----END \1PRIVATE KEY-----/dg)) {
    const [s, e] = m.indices[2]
    const escaped = !m[2].includes('\n') && m[2].includes('\\n')
    push('secrets', s, e, P.pem, { key: m[2].replace(/\s+|\\n/g, ''), fmt: (ph) => (escaped ? `\\n${ph}\\n` : `\n${ph}\n`) })
  }

  // Kubernetes Secrets: every value of `data:` (base64 — replaced by the base64 of its
  // placeholder, so it still decodes) and `stringData:`.
  if (/^kind:\s*["']?Secret["']?\s*$/m.test(src)) k8sYaml(src, push, sweepSecrets)
  if (/"kind"\s*:\s*"Secret"/.test(src) || /^kind:\s*["']?Secret/m.test(src)) k8sJson(src, push, sweepSecrets)

  // URIs: user, password and every host of the authority.
  for (const m of src.matchAll(/\b([a-z][a-z0-9+.-]*):\/\/(?:([^\s:@/?#'"]+)(?::([^\s/?#'"]*))?@)?([^\s/?#'"<>]*)/dgi)) {
    if (m[2] !== undefined) pushUser(m[2], m.indices[2][0], P.user)
    if (m[3]) pushSecret(m[3], m.indices[3][0], P.secret)
    if (m[4]) pushHostList(m[4], m.indices[4][0])
  }

  // SQL: IDENTIFIED BY / AS '…', PASSWORD '…', PASSWORD('…'), SET PASSWORD … = '…'.
  const sql = /\b(?:IDENTIFIED\s+(?:WITH\s+['"`]?[\w-]+['"`]?\s+)?(?:BY|AS)(?:\s+PASSWORD)?|(?:ENCRYPTED\s+)?PASSWORD\s*(?:FOR\s+\S+\s*)?(?:=\s*)?(?:\(\s*)?)\s*(['"])((?:\\.|''|(?!\1)[^\\\n])*)\1/dgi
  for (const m of src.matchAll(sql)) if (m[2]) pushSecret(m[2], m.indices[2][0], P.secret)

  // Authorization and API-key headers, cookies.
  for (const m of src.matchAll(/\b(?:Proxy-)?Authorization\s*[:=]\s*(?:(?:Bearer|Basic|Token|Negotiate|Digest)\s+)?([^\s'"]+)/dgi)) {
    pushSecret(m[1], m.indices[1][0], P.secret)
    const basic = /Basic\s+$/i.test(m[0].slice(0, m[0].length - m[1].length)) ? b64decode(m[1]) : null
    if (basic?.value && !basic.binary && basic.value.includes(':')) {
      const pw = basic.value.slice(basic.value.indexOf(':') + 1)
      if (pw.length >= 4) sweepSecrets.push({ value: pw, key: pw })
    }
  }
  for (const m of src.matchAll(/\b(?:X-[\w-]*(?:Token|Key|Secret)|Cookie|Set-Cookie)\s*:\s*([^\r\n'"]+)/dgi)) pushSecret(m[1].trimEnd(), m.indices[1][0], P.secret)

  // Tokens with a recognisable shape: JWTs (and JWEs), AWS access key ids, GitHub and Slack tokens.
  for (const m of src.matchAll(/\beyJ[\w-]{4,}(?:\.[\w-]*){2,4}/g)) pushSecret(m[0], m.index, P.secret)
  for (const m of src.matchAll(/\b(?:AKIA|ASIA|AGPA|AIDA|AROA)[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bxox[abprs]-[\w-]{10,}|\bglpat-[\w-]{20,}/g)) pushSecret(m[0], m.index, P.secret)

  // key=value and key: value, in command lines, configuration, YAML and JSON.
  const kv = /(?<![\w.-])(["']?)(-{0,2}[A-Za-z_][\w.-]*)\1[ \t]*(=|:(?!\/\/|:))[ \t]*(?:(["'])((?:\\.|(?!\4)[^\\\n])*)\4|([^\s"'`,;&|{}[\]()]+))/dg
  for (const m of src.matchAll(kv)) {
    const kind = keyKind(m[2])
    if (!kind) continue
    if (/^using password$/i.test(src.slice(Math.max(0, m.index - 6), m.index) + m[2])) continue
    let val = m[5] ?? m[6]
    let start = (m.indices[5] || m.indices[6])?.[0]
    if (val === undefined || start === undefined) continue
    // An unquoted YAML value runs to the end of its line (less a trailing comment).
    if (m[6] !== undefined && m[3] === ':' && !m[1] && isLineStart(m.index)) {
      const eol = src.indexOf('\n', start)
      const rest = src.slice(start, eol < 0 ? src.length : eol).replace(/\s+#.*$/, '').trimEnd()
      if (rest.length > val.length) val = rest
      if (/^[|>][-+]?$/.test(val)) continue
    }
    if (kind === 'secret') pushSecret(val, start, P.secret)
    else if (kind === 'user') pushUser(val, start, P.user)
    else if (kind === 'host') pushHostList(val, start)
  }

  // MySQL command lines: -pSECRET, -uUSER, -hHOST, and pt-tool DSNs (h=…,u=…,p=…).
  let lineStart = 0
  for (const line of src.split('\n')) {
    if (MYSQL_CLI.test(line) || /(?:^|[\s,])u=[^,\s]+,/.test(line)) {
      for (const m of line.matchAll(/(?<=\s)-([puh])(?![\s-])(?:'([^']*)'|"([^"]*)"|(\S+))/dg)) {
        const g = m[2] !== undefined ? 2 : m[3] !== undefined ? 3 : 4
        const v = m[g]
        const at = lineStart + m.indices[g][0]
        if (m[1] === 'p') pushSecret(v, at, P.secret)
        else if (m[1] === 'u') pushUser(v, at, P.user)
        else pushHostList(v, at)
      }
      for (const m of line.matchAll(/(?<=\s-u)\s+([^\s-]\S*)|(?<=\s-h)\s+([^\s-]\S*)/dg)) {
        if (m[1]) pushUser(m[1], lineStart + m.indices[1][0], P.user)
        if (m[2]) pushHostList(m[2], lineStart + m.indices[2][0])
      }
      for (const m of line.matchAll(/(?<![\w-])([hup])=([^,\s'"]+)/dg)) {
        const at = lineStart + m.indices[2][0]
        if (m[1] === 'p') pushSecret(m[2], at, P.secret)
        else if (m[1] === 'u') pushUser(m[2], at, P.user)
        else pushHostList(m[2], at)
      }
    }
    lineStart += line.length + 1
  }

  // 'user'@'host' (grants, error log), User@Host: (slow log), user@host (prompts, unquoted grants).
  for (const m of src.matchAll(/(['"`])([^'"`\s]+)\1@(['"`])([^'"`\s]*)\3/dg)) {
    pushUser(m[2], m.indices[2][0], P.user)
    pushHostList(m[4], m.indices[4][0])
  }
  for (const m of src.matchAll(/User@Host:\s*([^\s[]*)\[([^\]]*)\]\s*@\s*([^\s[]*)\s*\[([^\]]*)\]/dg)) {
    pushUser(m[1], m.indices[1][0], P.user)
    pushUser(m[2], m.indices[2][0], P.user)
    if (m[3]) pushHostList(m[3], m.indices[3][0])
  }
  for (const m of src.matchAll(/(?<![\w.%+\-/:])([A-Za-z_][\w.$-]{0,31})@(\[[0-9a-f:.]+\]|[A-Za-z0-9%][\w.%-]*)/dgi)) {
    if (/^User@Host$/i.test(m[0]) || PLACEHOLDER_EMAIL.test(m[0])) continue
    pushUser(m[1], m.indices[1][0], P.user)
    pushHostList(m[2].replace(/[.]$/, ''), m.indices[2][0])
  }
  // pt-summary's `Hostname | db1` and a bare `hostname` line in pt-stalk output.
  for (const m of src.matchAll(/^\s*Hostname\s*\|\s*(\S+)/dgim)) pushHostList(m[1], m.indices[1][0])

  // Shapes anywhere.
  for (const m of src.matchAll(EMAIL_RE)) {
    const v = m[0].toLowerCase()
    // user@host:3306 is a MySQL account and its server, not an address.
    if (/^:\d/.test(src.slice(m.index + m[0].length, m.index + m[0].length + 2))) continue
    if (!PLACEHOLDER_EMAIL.test(v) && !isPublic(v.split('@')[1])) push('emails', m.index, m.index + m[0].length, P.email, { key: v })
  }
  for (const m of src.matchAll(UUID_RE)) push('uuids', m.index, m.index + m[0].length, P.id, { key: m[0].toLowerCase(), fmt: caseLike(m[0]) })
  for (const m of src.matchAll(MAC_RE)) {
    const v = m[0].toLowerCase().replace(/-/g, ':')
    if (o.keepWellKnown && /^(00:){5}00$|^(ff:){5}ff$/.test(v)) continue
    push('macs', m.index, m.index + m[0].length, P.id, { key: v, fmt: (ph) => caseLike(m[0])(ph.replace(/:/g, m[1])) })
  }
  for (const m of src.matchAll(IPV6_RE)) {
    if (!isIPv6(m[0])) continue
    const v = m[0].toLowerCase()
    if (o.keepWellKnown && (v === '::1' || v === '::' || /^::ffff:127\./.test(v))) continue
    push('ipv6', m.index, m.index + m[0].length, P.ip, { key: v })
  }
  for (const m of src.matchAll(IPV4_RE)) {
    if (o.keepWellKnown && /^(127\.|0\.0\.0\.0$|255\.)/.test(m[0])) continue
    push('ipv4', m.index, m.index + m[0].length, P.ip, { key: m[0] })
  }
  for (const m of src.matchAll(HOST_RE)) {
    const h = hostPart(m[0])
    if (h) push('hosts', m.index, m.index + h.length, P.host, { key: h.toLowerCase().replace(/\.$/, '') })
  }

  // ---- helpers bound to this text

  function pushSecret(v, at, prio) {
    if (!v || v.length < 2 || PLACEHOLDER.test(v) || PATHLIKE.test(v) || /^\d{1,3}$/.test(v)) return
    push('secrets', at, at + v.length, prio, { key: v })
    if (v.length >= 4) sweepSecrets.push({ value: v, key: v })
  }
  function pushUser(v, at, prio) {
    if (!v || v === '%' || PLACEHOLDER.test(v) || /^\d+$/.test(v)) return
    if (o.keepWellKnown && WELL_KNOWN_USERS.has(v.toLowerCase())) return
    push('users', at, at + v.length, prio, { key: v })
    if (v.length >= 3) sweepUsers.add(v)
  }
  // pushHostList reads `db1:3306,db2:3306`, `gcomm://a,b`, `[fe80::1]:27017` or `10.0.0.%` and
  // masks each host in it; addresses are left to the address rules.
  function pushHostList(v, at) {
    let off = 0
    for (const part of String(v).split(',')) {
      let s = part
      let base = at + off
      off += part.length + 1
      const lead = /^\s*(?:[a-z][\w+.-]*:\/\/)?/i.exec(s)[0]
      s = s.slice(lead.length)
      base += lead.length
      if (s.startsWith('[')) continue
      const host = s.replace(/:\d*$/, '').replace(/\.$/, '')
      if (/^[\d.]*%$/.test(host) && /\d/.test(host)) {
        // A wildcard such as 10.0.0.% — the numbers go, the wildcard stays.
        push('ipv4', base, base + host.length, P.ctxHost, { key: host, net: true })
        continue
      }
      if (hostLike(host, o)) push('hosts', base, base + host.length, P.ctxHost, { key: host.toLowerCase() })
    }
  }
}

function k8sYaml(src, push, sweepSecrets) {
  const lines = src.split('\n')
  const starts = []
  let p = 0
  for (const l of lines) { starts.push(p); p += l.length + 1 }
  for (let i = 0; i < lines.length; i++) {
    const head = /^(\s*)(data|stringData):\s*$/.exec(lines[i])
    if (!head) continue
    const indent = head[1].length
    const encoded = head[2] === 'data'
    for (let j = i + 1; j < lines.length; j++) {
      const l = lines[j]
      if (!l.trim()) continue
      const ind = /^\s*/.exec(l)[0].length
      if (ind <= indent) break
      const kv = /^(\s*)(["']?)([^"':#][^:]*?)\2:[ \t]*(["']?)(.*?)\4[ \t]*$/.exec(l)
      if (!kv) continue
      let value = kv[5]
      let s = starts[j] + l.lastIndexOf(value)
      let e = s + value.length
      if (/^[|>][-+]?$/.test(value)) {
        // A block scalar: the indented lines under it.
        let k = j + 1
        let last = j
        while (k < lines.length && (!lines[k].trim() || /^\s*/.exec(lines[k])[0].length > ind)) { if (lines[k].trim()) last = k; k++ }
        if (last === j) continue
        s = starts[j + 1] + /^\s*/.exec(lines[j + 1])[0].length
        e = starts[last] + lines[last].length
        value = src.slice(s, e)
        j = last
      }
      if (!value) continue
      addK8sValue(push, sweepSecrets, value, s, e, encoded)
    }
  }
}

function k8sJson(src, push, sweepSecrets) {
  for (const m of src.matchAll(/"(data|stringData)"\s*:\s*\{([^{}]*)\}/dg)) {
    const base = m.indices[2][0]
    for (const e of m[2].matchAll(/"[^"]*"\s*:\s*"((?:\\.|[^"\\])*)"/dg)) {
      if (!e[1]) continue
      addK8sValue(push, sweepSecrets, e[1], base + e.indices[1][0], base + e.indices[1][1], m[1] === 'data')
    }
  }
}

function addK8sValue(push, sweepSecrets, value, s, e, encoded) {
  if (!encoded) {
    push('secrets', s, e, P.k8s, { key: value })
    if (value.length >= 4 && !value.includes('\n')) sweepSecrets.push({ value, key: value })
    return
  }
  // The placeholder is numbered by the decoded value, so the same password in a URI elsewhere
  // gets the same number; the base64 text and the decoded text are both swept.
  const d = b64decode(value)
  const plain = !d.error && !d.binary ? d.value : null
  const key = plain ?? value
  push('secrets', s, e, P.k8s, { key, fmt: (ph) => b64encode(ph) })
  sweepSecrets.push({ value, key })
  if (plain && plain.length >= 4 && plain.trim()) {
    sweepSecrets.push({ value: plain, key })
    for (const l of plain.split('\n')) if (l.trim().length >= 8 && l.trim() !== plain) sweepSecrets.push({ value: l.trim(), key: l.trim() })
  }
}

// caseLike returns a formatter that gives a placeholder the letter case of the original.
function caseLike(orig) {
  return orig === orig.toUpperCase() && /[A-F]/.test(orig) ? (ph) => ph.toUpperCase() : (ph) => ph
}

// ------------------------------------------------------------------ placeholders

function makers(o, originals, src) {
  const n = {}
  const next = (cat) => (n[cat] = (n[cat] || 0) + 1)
  return {
    ipv4: (key, span) => {
      if (span.net) {
        const i = next('ipnet')
        const octets = key.split('.').length - 1
        return ['10', String(100 + Math.floor(i / 256)), String(i % 256), '0'].slice(0, octets).join('.') + '.%'
      }
      // 10.0.0.1, 10.0.0.2, … skipping any address the text itself contains.
      for (;;) {
        const i = next('ipv4') - 1
        const cand = `10.${Math.floor(i / 64516) % 256}.${Math.floor(i / 254) % 254}.${(i % 254) + 1}`
        if (!originals.has(cand)) return cand
      }
    },
    ipv6: () => `2001:db8::${next('ipv6').toString(16)}`,
    // host1.example.net, user1@example.com, … skipping any the text itself already has.
    hosts: (key) => {
      const keep = key.includes('.') ? KEEP_SUFFIX.find((s) => key.endsWith(s) && key.length > s.length) || '.example.net' : ''
      for (;;) {
        const cand = `host${next('hosts')}${keep}`
        if (!keep || !src.includes(cand)) return cand
      }
    },
    emails: () => {
      for (;;) {
        const cand = `user${next('emails')}@example.com`
        if (!src.includes(cand)) return cand
      }
    },
    users: () => `user${next('users')}`,
    secrets: () => (o.style === 'stars' ? '********' : `<secret-${next('secrets')}>`),
    macs: () => {
      const i = next('macs')
      return `02:00:00:${((i >> 16) & 255).toString(16).padStart(2, '0')}:${((i >> 8) & 255).toString(16).padStart(2, '0')}:${(i & 255).toString(16).padStart(2, '0')}`
    },
    uuids: () => `00000000-0000-0000-0000-${next('uuids').toString(16).padStart(12, '0')}`,
    words: () => `word${next('words')}`,
  }
}

// ------------------------------------------------------------------ redact

const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

// redact returns { text, counts: { category: occurrences }, mapping: [{ cat, original,
// replacement, count }] } — the mapping in order of first appearance.
export function redact(input, options = {}) {
  const o = { ...DEFAULT_OPTIONS, ...options }
  const src = String(input ?? '')
  const words = String(o.words || '').split(/[,\n]/).map((w) => w.trim()).filter((w) => w.length >= 2)
  const enabled = (cat) => (cat === 'words' ? words.length > 0 : !!o[cat])
  const sweep = { secrets: [], users: new Set() }

  const cands = []
  const push = (cat, start, end, prio, extra = {}) => {
    if (!enabled(cat) || end <= start || start < 0) return
    cands.push({ cat, start, end, prio, key: extra.key ?? src.slice(start, end), ...extra })
  }
  rules(src, o, push, sweep)
  for (const w of words) {
    for (const m of src.matchAll(new RegExp(escapeRe(w), 'gi'))) push('words', m.index, m.index + m[0].length, P.word, { key: w.toLowerCase() })
  }

  // Settle overlaps: by priority, then the earlier and longer span.
  const covered = new Uint8Array(src.length + 1)
  const chosen = []
  const take = (list) => {
    list.sort((a, b) => a.prio - b.prio || a.start - b.start || (b.end - b.start) - (a.end - a.start))
    for (const c of list) {
      let free = true
      for (let i = c.start; i < c.end; i++) if (covered[i]) { free = false; break }
      if (!free) continue
      covered.fill(1, c.start, c.end)
      chosen.push(c)
    }
  }
  take(cands)

  // Sweeps: a value found once in a clear context is masked wherever else it appears.
  const sweeps = []
  if (o.secrets) {
    const seen = new Set()
    for (const { value, key } of sweep.secrets.sort((a, b) => b.value.length - a.value.length)) {
      if (seen.has(value) || PLACEHOLDER.test(value)) continue
      seen.add(value)
      let i = src.indexOf(value)
      while (i >= 0) {
        sweeps.push({ cat: 'secrets', start: i, end: i + value.length, prio: P.sweep, key })
        i = src.indexOf(value, i + value.length)
      }
    }
  }
  if (o.users) {
    for (const u of sweep.users) {
      for (const m of src.matchAll(new RegExp(`(?<![\\w$.@-])${escapeRe(u)}(?![\\w$-]|\\.\\w)`, 'g'))) {
        sweeps.push({ cat: 'users', start: m.index, end: m.index + u.length, prio: P.sweep, key: u })
      }
    }
  }
  if (o.hosts) {
    const shorts = new Set(chosen.filter((c) => c.cat === 'hosts' && !c.key.includes('.') && c.key.length >= 3).map((c) => c.key))
    for (const h of shorts) {
      for (const m of src.matchAll(new RegExp(`(?<![A-Za-z0-9])${escapeRe(h)}(?![A-Za-z0-9])`, 'gi'))) {
        const after = /^\.([A-Za-z][\w-]*)/.exec(src.slice(m.index + h.length))
        if (after && !FILE_EXT.has(after[1].toLowerCase())) continue
        sweeps.push({ cat: 'hosts', start: m.index, end: m.index + h.length, prio: P.sweep, key: h })
      }
    }
  }
  take(sweeps)

  // Placeholders, numbered in order of appearance.
  chosen.sort((a, b) => a.start - b.start)
  const originals = new Set(chosen.filter((c) => c.cat === 'ipv4').map((c) => c.key))
  const make = makers(o, originals, src)
  const map = new Map()
  const counts = {}
  let out = ''
  let pos = 0
  for (const c of chosen) {
    const id = `${c.cat}\u0000${c.key}`
    let row = map.get(id)
    if (!row) {
      row = { cat: c.cat, original: c.key, replacement: make[c.cat](c.key, c), count: 0 }
      map.set(id, row)
    }
    row.count++
    counts[c.cat] = (counts[c.cat] || 0) + 1
    out += src.slice(pos, c.start) + (c.fmt ? c.fmt(row.replacement) : row.replacement)
    pos = c.end
  }
  out += src.slice(pos)
  return { text: out, counts, mapping: [...map.values()] }
}
