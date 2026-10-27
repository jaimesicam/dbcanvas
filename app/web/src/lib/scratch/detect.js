// detect.js — Smart Paste: what a pasted text most likely is, and which cards fit it, best first.
//
// Each detector looks at the text alone (no parsing that can throw, no network) and returns a
// score; the board offers every card that scored, highest first, and the top one opens on a click.

import { parseJSON } from './transform.js'

const DETECTORS = [
  { type: 'tls', label: 'TLS Inspector', test: (t) => /-----BEGIN (CERTIFICATE|CERTIFICATE REQUEST|NEW CERTIFICATE REQUEST)-----/.test(t) ? 100 : 0 },
  { type: 'secret', label: 'Secret decoder', test: (t) => /\bkind:\s*Secret\b|"kind"\s*:\s*"Secret"/.test(t) ? 95 : (/^\s*(data|stringData):\s*$/m.test(t) ? 60 : 0) },
  { type: 'tls', label: 'TLS Inspector (Secret)', test: (t) => /tls\.crt/.test(t) && /\bkind:\s*Secret\b|"kind"\s*:\s*"Secret"/.test(t) ? 90 : 0 },
  { type: 'uri', label: 'Connection URI', test: (t) => /^\s*(mysql|mysqlx|postgres|postgresql|mongodb|mongodb\+srv):\/\//i.test(t) ? 95 : (/\bhost=\S+.*\bdbname=/.test(t) ? 70 : 0) },
  { type: 'gtid', label: 'GTID set calculator', test: (t) => /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:(?:[a-z_]\w*:)?\d+(-\d+)?/i.test(t) ? 85 : 0 },
  { type: 'mongo', label: 'MongoDB value decoder', test: (t) => /"\$(date|oid|numberLong|timestamp|binary|numberDecimal|uuid)"|ObjectId\(/.test(t) ? 80 : (/^\s*[0-9a-f]{24}\s*$/i.test(t) ? 75 : 0) },
  { type: 'time', label: 'Log time converter', test: (t) => /^\s*(\d{10}|\d{13}|\d{16}|\d{19})(\.\d+)?\s*$/.test(t) || /^\s*\d{4}-\d\d-\d\d[ T]\d\d:\d\d(:\d\d)?(\.\d+)?\s*(Z|UTC|[+-]\d\d(:?\d\d)?)?\s*$/i.test(t) ? 80 : 0 },
  { type: 'units', label: 'Database units', test: (t) => /^\s*-?\d+(\.\d+)?\s*([kmgtp]i?b?|bytes?)?\s*$/i.test(t) && !/^\s*\d{10,}\s*$/.test(t) ? 70 : 0 },
  { type: 'transform', label: 'Transformer (JSON)', test: (t) => /^\s*[[{]/.test(t) && !parseJSON(t.trim()).error ? 75 : 0 },
  { type: 'transform', label: 'Transformer (YAML)', test: (t) => /^(---|[\w"'-][^\n]*:\s)/m.test(t) && t.split('\n').length > 1 ? 50 : 0 },
  { type: 'transform', label: 'Transformer (Base64)', test: (t) => /^\s*[A-Za-z0-9+/_-]{16,}={0,2}\s*$/.test(t) && t.trim().length % 4 === 0 ? 55 : 0 },
  { type: 'logfilter', label: 'Log filter', test: (t) => t.split('\n').length > 5 && /\d{4}-\d\d-\d\d[T ]\d\d:\d\d/.test(t) ? 65 : 0 },
  { type: 'explain', label: 'EXPLAIN reader', test: (t) => /"query_block"\s*:|"Node Type"\s*:|\|\s*id\s*\|\s*select_type\s*\||\(cost=\d+\.\d+\.\.\d+\.\d+ rows=\d+ width=\d+\)|^\s*(EXPLAIN:\s*|\|\s*)?-> .*\((cost=|actual time=|never executed)/m.test(t) ? 95 : (/^\s*select_type:\s/m.test(t) && /^\s*possible_keys:\s/m.test(t)) || (/"operation"\s*:\s*"/.test(t) && /"access_type"\s*:/.test(t)) ? 90 : /\(actual (time|rows)=[\d.]+/.test(t) || /^\s*QUERY PLAN\s*$/m.test(t) ? 80 : 0 },
  { type: 'innodb', label: 'InnoDB status reader', test: (t) => /INNODB MONITOR OUTPUT/.test(t) ? 100 : /^\s*-+\s*(\n|\\n)(LATEST DETECTED DEADLOCK|BUFFER POOL AND MEMORY|SEMAPHORES|ROW OPERATIONS)\s*(\n|\\n)-+/m.test(t) ? 90 : /^(\S+ \d+ \[Note\] \[MY-\d+\] \[InnoDB\] )?\*\*\* \(1\) TRANSACTION:/m.test(t) ? 85 : 0 },
  { type: 'repl', label: 'Replication status', test: (t) => /\b(Replica|Slave)_(IO|SQL)_Running\b/.test(t) || (/\bmembers\b/.test(t) && /\bstateStr\b/.test(t)) || /\b(sent_lsn|sent_location)\b[\s\S]*\b(replay_lsn|replay_location)\b/.test(t) || /\bstatus\b[\s\S]*\bflushed_lsn\b|\breceive_start_lsn\b/.test(t) ? 92 : 0 },
  { type: 'cnfdiff', label: 'Config compare', test: (t) => /^\s*\[(mysqld|client|mysql|server|mariadb|mariadbd|galera|mysqld_safe|mysqldump|xtrabackup)(-[\d.]+)?\]\s*$/m.test(t) || /^\s*\|?\s*Variable_name\s*(\||\t)\s*Value\b/m.test(t) || /^\s*\|?\s*name\s*\|\s*setting\b/m.test(t) ? 75 : (/^(storage|net|systemLog|replication|security|processManagement):\s*$/m.test(t) && /^\s+(dbPath|port|bindIp|replSetName|path|authorization):/m.test(t)) || (t.match(/^\s*(shared_buffers|max_connections|work_mem|wal_level|listen_addresses|effective_cache_size|maintenance_work_mem|checkpoint_timeout|max_wal_size)\s*=/gm) || []).length >= 2 ? 65 : 0 },
  { type: 'jwt', label: 'JWT / OIDC decoder', test: (t) => /^\s*(?:(?:Proxy-)?Authorization:\s*)?(?:Bearer\s+)?eyJ[\w-]*(?:\.[\w-]*){2}\s*$/i.test(t) ? 95 : /^\s*eyJ[\w-]*(?:\.[\w-]*){4}\s*$/.test(t) ? 90 : /\beyJ[\w-]{4,}\.eyJ[\w-]{4,}\.[\w-]*/.test(t) ? 60 : 0 },
  { type: 'redact', label: 'Redactor', test: (t) => (t.trim().includes('\n') ? 15 : 0) },
  { type: 'digest', label: 'Query digest (slow log)', test: (t) => (/^# Query_time: \d/m.test(t) || (/^# Time: /m.test(t) && /^# User@Host: /m.test(t)) ? 90 : 0) },
  { type: 'digest', label: 'Query digest (queries)', test: (t) => ((t.match(/^\s*(SELECT|INSERT|UPDATE|DELETE|REPLACE|CALL|WITH|SHOW)\b[^;]*;\s*$/gim) || []).length >= 2 ? 60 : 0) },
  { type: 'diff', label: 'Text diff', test: () => 10 },
  { type: 'note', label: 'Quick Note', test: () => 20 },
]

// suggest returns [{ type, label, score }], best first, one entry per card type.
export function suggest(text) {
  const t = String(text ?? '')
  if (!t.trim()) return []
  const best = new Map()
  for (const d of DETECTORS) {
    const score = d.test(t)
    if (score > 0 && (!best.has(d.type) || best.get(d.type).score < score)) best.set(d.type, { type: d.type, label: d.label, score })
  }
  return [...best.values()].sort((a, b) => b.score - a.score)
}
