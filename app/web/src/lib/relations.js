import { edgeControls } from './canvas.js'

// stackRelations derives the "relationship lines" the Database Stacks canvas can draw
// on top of its association edges. Those edges are only the links a user draws by
// hand (proxies, simulators, replication); everything else a node depends on is a
// picker in its properties panel — "Monitored by (PMM)", "Repository", "OpenBao",
// "SeaweedFS node", … — and was invisible on the canvas. This reads those pickers back
// out of the design so the canvas can show them as labelled lines.
//
// Each relation is { id, from, to, kind, label, detail }: from is the node or frame that
// holds the setting (a cluster member is folded into its frame), to is the node it
// points at, label is the short caption and detail the sentence its tooltip shows.
// A picker whose enabling checkbox is off (enableVault, ldapAuth, enablePBM, …) keeps
// its old value in the design, so those are checked here rather than drawn stale.

export const RELATION_KINDS = {
  monitor: { color: '#f59e0b' },
  orchestrate: { color: '#f97316' },
  repository: { color: '#0ea5e9' },
  encryption: { color: '#16a34a' },
  backup: { color: '#14b8a6' },
  auth: { color: '#8b5cf6' },
  upgrade: { color: '#64748b' },
  app: { color: '#ec4899' },
}

// Which settings point where. `when` says whether the setting is live on obj; `label`
// and `detail` may look at obj and the target node.
const RULES = [
  {
    field: 'pmmNodeId', kind: 'monitor',
    label: () => 'monitored by',
    detail: (s, t) => `${s} runs a PMM client that sends its metrics and query analytics to ${t}.`,
  },
  {
    field: 'orchestratorNodeId', kind: 'orchestrate',
    label: () => 'topology managed by',
    detail: (s, t) => `${t} discovers ${s}'s replication topology and handles its failover.`,
  },
  {
    field: 'repositoryNodeId', kind: 'repository',
    label: (o) => (o.type === 'k3d' ? 'pulls images from' : 'installs from'),
    detail: (s, t, o) => (o.type === 'k3d'
      ? `${s} pulls its operator images and charts from the ${t} mirror instead of upstream.`
      : `${s} installs its packages from the ${t} mirror instead of upstream repositories.`),
  },
  {
    field: 'openbaoNodeId', kind: 'encryption',
    when: (o) => o.enableVault !== false,
    label: () => 'encryption keys from',
    detail: (s, t) => `${s} keeps its data-at-rest encryption keys in ${t}.`,
  },
  {
    field: 'seaweedfsNodeId', kind: 'backup',
    when: (o) => {
      const gates = ['enablePBM', 'usePgBackRest', 'useBarman'].filter((k) => k in o)
      return gates.length === 0 || gates.some((k) => o[k])
    },
    label: () => 'backs up to',
    detail: (s, t) => `${s} stores its backups (and WAL/oplog archive, where enabled) in ${t}'s S3 bucket.`,
  },
  {
    field: 'keycloakNodeId', kind: 'auth',
    when: (o) => o.enableOIDC !== false,
    label: () => 'SSO (OIDC) via',
    detail: (s, t) => `${s} accepts single sign-on tokens issued by ${t}.`,
  },
  {
    field: 'ldapDirNodeId', kind: 'auth',
    when: (o) => !!o.ldapAuth,
    label: (o) => (o.kerberosAuth ? 'LDAP + Kerberos via' : 'LDAP auth via'),
    detail: (s, t, o) => `${s} authenticates its users against the ${t} directory${o.kerberosAuth ? ', with Kerberos single sign-on' : ''}.`,
  },
  {
    field: 'watchtowerNodeId', kind: 'upgrade',
    label: () => 'upgraded by',
    detail: (s, t) => `${t} lets ${s} upgrade itself from inside its own UI.`,
  },
  {
    field: 'ssAIONode', kind: 'app',
    when: (o) => o.ssMode === 'aio',
    label: () => 'app connection',
    detail: (s, t, o) => `${s} drives its workload against ${o.ssAIOInstance ? `the ${o.ssAIOInstance} instance on ` : ''}${t}.`,
  },
]

const nameOf = (x) => x?.label || x?.name || x?.type || x?.id || '?'

export function stackRelations(nodes = [], frames = []) {
  const byId = new Map()
  for (const n of nodes) byId.set(n.id, n)
  for (const f of frames) byId.set(f.id, f)

  // Keyed by from → to, so two settings between the same pair share one line
  // ("monitored by · installs from") instead of drawing two on top of each other.
  const out = new Map()
  const add = (fromId, toId, rule, obj, who) => {
    const target = byId.get(toId)
    if (!target || toId === fromId) return
    const key = `${fromId}>${toId}`
    const label = rule.label(obj, target)
    const detail = rule.detail(who, nameOf(target), obj)
    const cur = out.get(key)
    if (!cur) {
      out.set(key, { id: `rel:${key}`, from: fromId, to: toId, kind: rule.kind, labels: [label], details: [detail] })
    } else {
      if (!cur.labels.includes(label)) cur.labels.push(label)
      if (!cur.details.includes(detail)) cur.details.push(detail)
    }
  }

  const visit = (obj, fromId) => {
    for (const rule of RULES) {
      const to = obj[rule.field]
      if (!to || (rule.when && !rule.when(obj))) continue
      add(fromId, to, rule, obj, nameOf(byId.get(fromId)))
    }
  }

  for (const f of frames) visit(f, f.id)
  for (const n of nodes) {
    // A cluster member is drawn inside its frame and the frame's settings already
    // cover it; only a member setting the frame does not have gets its own line.
    const fromId = n.frameId && byId.has(n.frameId) ? n.frameId : n.id
    visit(n, fromId)
    // An All-in-One node has no ports: every instance on it picks its own PMM,
    // OpenBao, … so each instance's settings are drawn from the node, named.
    for (const inst of n.aioInstances || []) {
      const who = `${inst.name || 'instance'} on ${nameOf(n)}`
      for (const rule of RULES) {
        const to = inst[rule.field]
        if (!to || (rule.when && !rule.when(inst))) continue
        add(n.id, to, { ...rule, label: (o, t) => `${inst.name}: ${rule.label(o, t)}` }, inst, who)
      }
      // orchestratorRef is "<nodeId>" or "inst:<id>" (an Orchestrator on this same
      // node, which has no line to draw).
      const ref = inst.orchestratorRef || ''
      if (ref && !ref.startsWith('inst:')) {
        const rule = RULES.find((r) => r.field === 'orchestratorNodeId')
        add(n.id, ref, { ...rule, label: () => `${inst.name}: ${rule.label()}` }, inst, who)
      }
    }
  }

  return [...out.values()].map((r) => ({
    id: r.id, from: r.from, to: r.to, kind: r.kind,
    label: r.labels.join(' · '), detail: r.details.join(' '),
  }))
}

// relationPorts picks the sides of two rectangles a relationship line should join:
// the pair facing each other along the axis they are furthest apart on.
export function relationPorts(a, b) {
  const ax = a.x + a.w / 2, ay = a.y + a.h / 2
  const bx = b.x + b.w / 2, by = b.y + b.h / 2
  const dx = bx - ax, dy = by - ay
  // Normalise by size so a wide frame beside a node still reads as "beside".
  if (Math.abs(dx) / (a.w + b.w) >= Math.abs(dy) / (a.h + b.h)) {
    return dx >= 0 ? ['right', 'left'] : ['left', 'right']
  }
  return dy >= 0 ? ['bottom', 'top'] : ['top', 'bottom']
}

// bezierMid is the point halfway along edgePath's cubic, where a caption sits on the
// line itself rather than on the straight chord between its ends.
export function bezierMid(p0, port0, p1, port1) {
  const [c0, c1] = edgeControls(p0, port0, p1, port1)
  return {
    x: 0.125 * p0.x + 0.375 * c0.x + 0.375 * c1.x + 0.125 * p1.x,
    y: 0.125 * p0.y + 0.375 * c0.y + 0.375 * c1.y + 0.125 * p1.y,
  }
}
