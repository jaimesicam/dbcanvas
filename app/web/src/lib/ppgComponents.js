// ppgComponents.js — which Percona PostgreSQL components (pg_tde, pg_oidc_validator, pgaudit, …)
// a node can have, read from the server's table (app/ppgcomponents.go) rather than kept here.
//
// The designer used to carry its own copies of the rules (PG_TDE_MAJORS, a `=== '18'` for the
// OIDC validator), which knew nothing about the OS or the minor and drifted from the validator.
// Now the table comes with the PPG catalogue, and ppgAvailable() answers with the same words
// ppgComponent.Available does, so the form and the deploy-time error say the same thing.
import { useEffect, useState } from 'react'
import { stackApi } from './stackApi.js'

let cache = null
let pending = null

function load() {
  pending ||= stackApi.ppgCatalog()
    .then((c) => { cache = c.components || []; return cache })
    .catch(() => { pending = null; return null })
  return pending
}

// usePPGComponents returns the component table, or null until it has loaded. A component whose
// table has not loaded yet counts as available: the form must not grey out a checkbox on a
// guess, and validation still has the last word.
export function usePPGComponents() {
  const [comps, setComps] = useState(cache)
  useEffect(() => {
    if (cache) return
    let alive = true
    load().then((c) => { if (alive && c) setComps(c) })
    return () => { alive = false }
  }, [])
  return comps
}

// ppgComponents is the table as last loaded, for code that cannot hold a hook (a pin's warn()).
// Callers render under a usePPGComponents() so they re-render once it arrives.
export const ppgComponents = () => cache

const isDebian = (os) => os === 'ubuntu' || os === 'debian'
const minorOf = (v) => String(v || '').split('-')[0]
function versionLess(a, b) {
  const pa = a.split('.').map(Number), pb = b.split('.').map(Number)
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const x = pa[i] || 0, y = pb[i] || 0
    if (x !== y) return x < y
  }
  return false
}
const joinOr = (xs) => (xs.length < 2 ? xs.join('') : `${xs.slice(0, -1).join(', ')} or ${xs[xs.length - 1]}`)

// ppgAvailable mirrors ppgComponent.Available: { ok, why, pkg } for component `id` on a node or
// frame with os / pgMajor / pgVersion. pkg is the package DBCanvas installs for it there.
export function ppgAvailable(comps, id, { os, pgMajor, pgVersion }) {
  const c = (comps || []).find((x) => x.id === id)
  const major = pgMajor || '16'
  if (!c) return { ok: true, why: '', pkg: '' }
  const pat = isDebian(os) ? c.debian : c.rhel
  const pkg = pat ? pat.replace('%s', major) : ''
  if (!c.majors.includes(major)) return { ok: false, why: `${c.label} is packaged for PostgreSQL ${joinOr(c.majors)} only, not ${major}`, pkg }
  const min = c.minMinor?.[major]
  if (min && pgVersion && versionLess(minorOf(pgVersion), min)) return { ok: false, why: `${c.label} needs PostgreSQL ${min} or newer, and this node is pinned to ${minorOf(pgVersion)}`, pkg }
  return { ok: true, why: '', pkg }
}
