// distribution.js — whose build a MongoDB or PostgreSQL node installs (app/distribution.go).
//
// The node types stay the same — psm/psmrs/psmdb are MongoDB, pg/patroni/pgstream are
// PostgreSQL — and a node or cluster frame carries `distribution`:
//   ''          Percona's build (PSMDB, Percona Distribution for PostgreSQL)
//   'community' MongoDB Community Server, from repo.mongodb.org
//   'pgdg'      the PostgreSQL Global Development Group's packages
// It is set by the library entry the node came from and never changes after, so the
// names below are what the user picked from the library: "MDB Replica Set",
// "Patroni Cluster (PGDG)". Members of a cluster frame take their frame's.

export const DIST_PERCONA = ''
export const DIST_MDB = 'community'
export const DIST_PGDG = 'pgdg'

const MONGO_TYPES = new Set(['psm', 'psmrs', 'psmdb'])
const PG_TYPES = new Set(['pg', 'patroni', 'pgstream'])

export const isMDB = (o) => (o?.distribution || '') === DIST_MDB
export const isPGDG = (o) => (o?.distribution || '') === DIST_PGDG

// distOf is the distribution a node runs under: its own (a standalone node) or its frame's.
export function distOf(node, frames) {
  if (!node) return ''
  if (node.frameId && frames) {
    const f = frames.find((x) => x.id === node.frameId)
    if (f) return f.distribution || ''
  }
  return node.distribution || ''
}

// The library's names, per type and distribution.
const LABELS = {
  [DIST_MDB]: { psm: 'MDB', psmrs: 'MDB Replica Set', psmdb: 'MDB Sharded' },
  [DIST_PGDG]: { pg: 'PostgreSQL (PGDG)', patroni: 'Patroni Cluster (PGDG)', pgstream: 'PostgreSQL Replication (PGDG)' },
}

// distTypeLabel names a node or frame type under a distribution; '' when the
// distribution does not rename it (the caller keeps its own label).
export function distTypeLabel(type, dist) {
  return LABELS[dist || '']?.[type] || ''
}

// distMemberLabel names a member node's type (an MDB replica set member is an
// "MDB RS member"), for the canvas tile under the member's name.
export function distMemberLabel(type, dist) {
  if (dist === DIST_MDB) return { psmrs: 'MDB RS member', psmdb: 'MDB Sharded member', psm: 'MDB' }[type] || ''
  if (dist === DIST_PGDG) return { patroni: 'Patroni (PGDG)', pgstream: 'PostgreSQL (PGDG)', pg: 'PostgreSQL (PGDG)' }[type] || ''
  return ''
}

// distBadge is the short tag a canvas tile or frame header shows beside the type, so a
// PSMDB replica set and an MDB one are told apart at a glance.
export function distBadge(dist) {
  if (dist === DIST_MDB) return 'Community'
  if (dist === DIST_PGDG) return 'PGDG'
  return ''
}

// The prefix new clusters and members are named with. Labels become host names, so an
// MDB replica set's members read mdbrs01, mdbrs02 rather than psmrs01 — the name says
// which server the host runs.
const NAME_PREFIX = {
  [DIST_MDB]: { psm: 'mdb', psmrs: 'mdbrs', psmdb: 'mdb', frame_psmrs: 'mdb-rs', frame_psmdb: 'mdb-sharded' },
  [DIST_PGDG]: { pg: 'pgdg', patroni: 'pgdgpatroni', pgstream: 'pgdgstream', frame_patroni: 'pgdg-patroni', frame_pgstream: 'pgdg-stream' },
}
export function distNamePrefix(key, dist, fallback) {
  return NAME_PREFIX[dist || '']?.[key] || fallback
}

// The product names forms and help text use.
export function mongoProduct(dist) {
  return dist === DIST_MDB ? 'MongoDB Community Server' : 'Percona Server for MongoDB'
}
export function mongoShort(dist) {
  return dist === DIST_MDB ? 'MongoDB' : 'PS MongoDB'
}
export function pgProduct(dist) {
  return dist === DIST_PGDG ? 'PGDG PostgreSQL' : 'Percona PostgreSQL'
}

// The series each MongoDB distribution offers. PSMDB's come from its catalogue; the
// Community Server's are fixed (app/distribution.go mdbMajors) and the catalogue says
// which of them each image has.
export const MDB_MAJORS = ['8.2', '8.0', '7.0', '6.0']

export const distAppliesTo = (type, dist) =>
  (dist === DIST_MDB && MONGO_TYPES.has(type)) || (dist === DIST_PGDG && PG_TYPES.has(type))
