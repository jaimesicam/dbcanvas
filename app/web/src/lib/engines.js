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
