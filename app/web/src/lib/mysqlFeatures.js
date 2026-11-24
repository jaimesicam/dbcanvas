// mysqlFeatures.js — which MySQL-family server features a release has. The table is the
// server's own (app/mysqlfeatures.go), copied into mysqlFeatures.json and kept in step by
// TestMySQLFeaturesJSON; the web app ships inside the same binary, so the copy cannot be older
// than the server it talks to. The validator and the deploy scripts read the same table, so the
// form, the error a deploy would give and the my.cnf DBCanvas writes agree about every version.
// (GET /api/catalog/mysql-features serves it to scripts.)
import table from './mysqlFeatures.json'

// useMySQLFeatures returns the table. It is a hook so a caller does not change if the table ever
// has to be fetched.
export function useMySQLFeatures() {
  return table
}

// compareVersions mirrors app/version.go: dotted segments compared as integers, a segment read
// up to its first non-digit ("45-36" is 45), a missing segment zero.
export function compareVersions(a, b) {
  const seg = (s) => { const m = /^\d+/.exec(s || ''); return m ? Number(m[0]) : 0 }
  const pa = String(a).split('.'), pb = String(b).split('.')
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = seg(pa[i]) - seg(pb[i])
    if (d !== 0) return d < 0 ? -1 : 1
  }
  return 0
}

// mysqlHas mirrors mysqlHasOn in app/mysqlfeatures.go: { ok, why } for feature `key` on a server
// of series `major`, pinned to `version` ('' — the newest of the series); `community` is Oracle's
// MySQL, which has none of the Percona-only options.
export function mysqlHas(features, key, major, version, community = false) {
  const f = (features || table).find((x) => x.key === key)
  if (!f) return { ok: false, why: '' }
  if (community && f.perconaOnly) return { ok: false, why: `${f.label} is a Percona Server option — MySQL Community does not have it` }
  const v = version || `${major || '8.0'}.9999`
  const shown = version || major
  if (f.since && compareVersions(v, f.since) < 0) return { ok: false, why: `${f.label} needs ${f.since} or later, and this server is ${shown}` }
  if (f.until && compareVersions(v, f.until) >= 0) return { ok: false, why: `${f.label} is gone from ${f.until}` }
  return { ok: true, why: '' }
}
