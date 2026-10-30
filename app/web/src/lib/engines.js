// engines.js — which database engine a node is, and that engine's colour.
//
// engineOf mirrors dbEngineOf in app/dashboard.go, which is what the Dashboard's
// "By engine" counts come from, so a colour here and a count there always agree. Keep
// the two in step.
//
// The colours are not new: they are four of the five node-identity slots (--node-*,
// index.css), which were already searched and validated per theme surface. Checked
// again for this use with the dataviz skill's validate_palette.js --pairs all, as a
// four-slot set, on every surface the app draws on:
//
//   light  #ffffff #fbf1d3                        CVD ΔE 9.6, normal 16.6, all >= 3:1
//   dark   #161b24 #0e1530 #241546 #122019        all checks pass
//
// They replace the catalogue colours (NODE_TYPES[].color) wherever a colour is meant to
// say WHICH ENGINE: those were picked one type at a time and failed as a set — MySQL's
// #2563eb against Valkey's #7c3aed was ΔE 0.4 to a deuteranope (the same colour), 12.4
// to normal vision, and PostgreSQL's #336791 sat under the chroma floor and read grey.
//
// Anything that is not a database (proxies, monitoring, the Intranet) is neutral: on a
// stack the databases are what you are looking for, so they carry the colour and the
// supporting cast recedes. The engine is never only a colour — its name or the node's
// label is always printed beside it.

const ENGINE_OF = {
  pg: 'postgres', patroni: 'postgres', repmgr: 'postgres', spock: 'postgres',
  pxc: 'mysql', ps: 'mysql', mysql: 'mysql', innodb: 'mysql',
  mariadb: 'mysql', mariadbrepl: 'mysql', mariadbgalera: 'mysql',
  mysqlce: 'mysql', mysqlcerepl: 'mysql', mysqlceinnodb: 'mysql',
  psm: 'mongodb', psmdb: 'mongodb', psmrs: 'mongodb',
  valkey: 'valkey', valkeycluster: 'valkey',
}

export const ENGINE_TONE = {
  mysql: 'var(--node-2)',
  postgres: 'var(--node-1)',
  mongodb: 'var(--node-4)',
  valkey: 'var(--node-5)',
}
export const NEUTRAL_TONE = 'var(--muted)'

export const engineOf = (type) => ENGINE_OF[type] || ''
export const engineTone = (type) => ENGINE_TONE[engineOf(type)] || NEUTRAL_TONE

// NODE_CODE is the short code a node's tile shows in place of a drawn icon (desktop stack
// folders, canvas cards, cluster-member cards and cluster headers). A drawn icon could not
// tell 44 node types apart — PXC, Percona Server, MySQL, MariaDB and PostgreSQL all wore the
// same database glyph — where a code names the product outright, and the tile's colour
// (engineTone) still says which engine family it belongs to.
//
// Codes are two letters, written like element symbols (capital, then lower case) so that two
// that share a first letter are still easy to tell apart. The only three-letter codes are the
// products whose own short name is three letters: PXC and PMM. Every member type of a family
// shares the family's code (all Percona Server types are Ps, all PSMDB types Mo). The tile
// always carries the full name as its tooltip and accessible label.
export const NODE_CODE = {
  pxc: 'PXC', ps: 'Ps', mysql: 'Ps', innodb: 'Ps',
  mysqlce: 'My', mysqlcerepl: 'My', mysqlceinnodb: 'My',
  mariadb: 'Ma', mariadbrepl: 'Ma', mariadbgalera: 'Ma',
  psm: 'Mo', psmdb: 'Mo', psmrs: 'Mo',
  pg: 'Pg', patroni: 'Pa', repmgr: 'Rm', spock: 'Sp',
  valkey: 'Vk', valkeycluster: 'Vk',
  proxysql: 'Pr', haproxy: 'Ha', pgbouncer: 'Pb',
  pmm: 'PMM', pmm2: 'PMM', orchestrator: 'Or', watchtower: 'Wt',
  intranet: 'In', sambaad: 'Ad', keycloak: 'Kc', openbao: 'Ob', seaweedfs: 'Sw', ceph: 'Ce',
  repository: 'Re', linuxclient: 'Lc', vnc: 'Vn', k3d: 'K3', aio: 'Ai', bighole: 'Bh', mclusteradmin: 'Mc',
  stocksim: 'St', ledgersim: 'Lg', supportsim: 'Su', pgvectorsim: 'Pv', perfsim: 'Pf',
}

// nodeCode is a type's code; a type missing from the map (a new one) gets the first two
// letters of its name rather than nothing, so it is visible that it needs a code.
export const nodeCode = (type, label = '') => {
  if (NODE_CODE[type]) return NODE_CODE[type]
  const s = String(label || type || '?').replace(/[^A-Za-z0-9]/g, '')
  return (s[0] || '?').toUpperCase() + (s[1] || '').toLowerCase()
}
