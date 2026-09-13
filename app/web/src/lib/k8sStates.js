// k8sStates.js — the model behind the Kubernetes States canvas.
//
// The server samples a cluster (app/k3dstates.go) and hands back a flat list of objects,
// each with a tone, a summary and a few toned properties. That is a photograph. This file
// is what turns a series of photographs into something worth watching:
//
//   - what is NEW since the last sample (it should arrive with a flash, not appear as if
//     it had always been there)
//   - which PROPERTY changed, and what it changed FROM (3 → 2 replicas is the event; the
//     value alone is just a number)
//   - what has GONE, which the next sample cannot tell you because it is not in it — a
//     deleted pod leaves a tombstone that stays on the canvas until it is dismissed,
//     because "the pod that was crashing has disappeared" is the single most common thing
//     to miss while looking somewhere else.
//
// All of it is pure: merge takes the previous model, a sample and a clock, and returns the
// next model. That is what makes it testable without a cluster, a browser or a timer, and
// it is why the page itself holds no logic beyond drawing.

// HIGHLIGHT_MS is how long a changed property stays lit. Long enough to catch out of the
// corner of an eye at a 5s poll, short enough that a busy cluster is not permanently lit.
export const HIGHLIGHT_MS = 12000

// TONES orders the four states by attention, mirroring k3dToneRank in app/k3dstates.go.
// 'gone' is the browser's own fifth: the server never reports it, because an object that
// is gone is one the server no longer sees.
export const TONES = ['gone', 'done', 'ok', 'warn', 'bad']

export const toneRank = (t) => TONES.indexOf(t)

// TONE_LABEL is what the legend and the filter chips call each tone.
export const TONE_LABEL = {
  bad: 'Broken',
  warn: 'Changing',
  ok: 'Healthy',
  done: 'Finished',
  gone: 'Deleted',
}

// objKey identifies a card across samples. The server guarantees a uid for everything a
// real cluster returns and synthesises a stable one otherwise, so this is just a rename —
// but the canvas keys React children, tombstones and pins on it, so it gets a name.
export const objKey = (o) => o.uid

// propsOf turns a property list into a lookup, for comparing two samples.
function propsOf(obj) {
  const m = new Map()
  for (const p of obj.props || []) m.set(p.key, p.value)
  return m
}

// emptyModel is the state before the first sample.
export function emptyModel() {
  return { objects: [], byKey: new Map(), capturedAt: '', samples: 0 }
}

// mergeStates folds one sample into the model.
//
// Tombstones are the interesting half. An object missing from a sample is NOT removed: it
// is marked gone at `now` and kept, in place, until somebody dismisses it — so a pod that
// was deleted while you were reading another card is still there, in its last known state,
// with the tone that says it is no longer real. A uid that comes back (Kubernetes does not
// reuse them, but a hand-written key can) is treated as a new object rather than an
// undelete, because that is what it is.
export function mergeStates(prev, sample, now = Date.now()) {
  const previous = prev?.byKey ?? new Map()
  const seen = new Set()
  const objects = []

  for (const o of sample?.objects ?? []) {
    const key = objKey(o)
    seen.add(key)
    const was = previous.get(key)
    if (!was || was.gone) {
      objects.push({ ...o, key, firstSeen: now, updatedAt: now, changed: {}, gone: null, toneChangedAt: 0 })
      continue
    }
    const before = propsOf(was)
    const after = propsOf(o)
    // Carry the previous highlights forward: a 5s poll with a 12s highlight means most
    // changes are still lit when the next sample lands, and dropping them would make a
    // change visible for exactly one frame.
    const changed = { ...was.changed }
    let touched = false
    for (const [k, v] of after) {
      if (before.has(k) && before.get(k) === v) continue
      // A property that was not there before is a change, but with nothing to show as its
      // previous value — `from` stays null and the row says "new" rather than "was …".
      changed[k] = { at: now, from: before.has(k) ? before.get(k) : null }
      touched = true
    }
    for (const [k] of before) {
      // A row that disappeared — a container that stopped waiting, a condition that
      // cleared. Worth marking the object as having moved, but there is no row left to
      // light up, so it is not recorded as a property change.
      if (!after.has(k)) touched = true
    }
    objects.push({
      ...o,
      key,
      firstSeen: was.firstSeen,
      updatedAt: touched || was.tone !== o.tone ? now : was.updatedAt,
      changed,
      gone: null,
      toneChangedAt: was.tone !== o.tone ? now : was.toneChangedAt || 0,
      prevTone: was.tone !== o.tone ? was.tone : was.prevTone,
    })
  }

  // Everything the model knew that this sample did not mention.
  for (const was of prev?.objects ?? []) {
    if (seen.has(was.key)) continue
    objects.push(was.gone ? was : { ...was, gone: now, tone: 'gone', updatedAt: now })
  }

  const byKey = new Map(objects.map((o) => [o.key, o]))
  return { objects, byKey, capturedAt: sample?.capturedAt || '', samples: (prev?.samples ?? 0) + 1 }
}

// dismissGone removes one tombstone. Only a tombstone: dismissing a live object would put
// the canvas out of step with the cluster until the object changed, which is a lie a
// monitor must not tell.
export function dismissGone(model, key) {
  const target = model.byKey?.get(key)
  if (!target?.gone) return model
  const objects = model.objects.filter((o) => o.key !== key)
  return { ...model, objects, byKey: new Map(objects.map((o) => [o.key, o])) }
}

export function dismissAllGone(model) {
  const objects = model.objects.filter((o) => !o.gone)
  if (objects.length === model.objects.length) return model
  return { ...model, objects, byKey: new Map(objects.map((o) => [o.key, o])) }
}

// isHighlighted reports whether a property changed recently enough to still be lit.
export function isHighlighted(obj, key, now, ms = HIGHLIGHT_MS) {
  const c = obj?.changed?.[key]
  return !!c && now - c.at < ms
}

// changeNote is what a lit row says about itself: where the value came from.
export function changeNote(obj, key, now, ms = HIGHLIGHT_MS) {
  const c = obj?.changed?.[key]
  if (!c || now - c.at >= ms) return ''
  return c.from == null ? 'new' : `was ${c.from}`
}

// isFresh reports whether an object arrived recently — the card's own flash.
export function isFresh(obj, now, ms = HIGHLIGHT_MS) {
  return !obj.gone && now - obj.firstSeen < ms && obj.firstSeen > 0
}

// countTones summarises a list for the header: how many of each tone, tombstones included.
export function countTones(objects) {
  const counts = { bad: 0, warn: 0, ok: 0, done: 0, gone: 0 }
  for (const o of objects) counts[o.gone ? 'gone' : o.tone] = (counts[o.gone ? 'gone' : o.tone] ?? 0) + 1
  return counts
}

// filterStates narrows what the canvas draws. Everything is optional; nothing here sorts,
// because the order a card sits in is the server's and must not move under the pointer.
//
// The one rule worth stating: `problems` keeps tombstones. A deleted object is exactly the
// kind of thing somebody filtering for trouble is looking for, and dropping it would make
// the filter hide the evidence.
export function filterStates(objects, { namespace = '', hidden = null, query = '', problems = false } = {}) {
  const q = query.trim().toLowerCase()
  return objects.filter((o) => {
    if (namespace && o.namespace !== namespace) return false
    if (hidden?.has(o.kind)) return false
    if (problems && !(o.gone || o.tone === 'bad' || o.tone === 'warn')) return false
    if (!q) return true
    if (o.name.toLowerCase().includes(q)) return true
    if (o.kind.toLowerCase().includes(q)) return true
    if ((o.summary || '').toLowerCase().includes(q)) return true
    return (o.props || []).some((p) => `${p.key} ${p.value}`.toLowerCase().includes(q))
  })
}

// SECONDARY_KINDS are on the board but not in front of you.
//
// A pt-k8s-debug-collector capture contains every resource the API server serves, and a live
// cluster can be sampled for nearly as much — which is right, because the alternative is a
// tool that quietly decides what you are allowed to look at. But a real cluster has 468
// ClusterRoles and two StatefulSets, and a board that opens with the ClusterRoles is a board
// nobody scrolls. So these kinds start hidden, with their chip showing the count, one click
// from being there. Nothing is dropped; the default is just an opinion about what a database
// person came to look at.
export const SECONDARY_KINDS = new Set([
  'ReplicaSet', 'ControllerRevision', 'Endpoints', 'EndpointSlice', 'Lease', 'Event',
  'ConfigMap', 'Secret', 'ServiceAccount', 'StorageClass', 'Namespace', 'PodTemplate',
  'Role', 'RoleBinding', 'ClusterRole', 'ClusterRoleBinding', 'NetworkPolicy',
  'PriorityClass', 'RuntimeClass', 'IngressClass', 'LimitRange', 'ResourceQuota',
  'CustomResourceDefinition', 'APIService', 'ComponentStatus', 'VolumeAttachment',
  'CSIDriver', 'CSINode', 'MutatingWebhookConfiguration', 'ValidatingWebhookConfiguration',
])

// hiddenByDefault keeps whatever the reader has already decided and hides only kinds this
// board has not seen before — so turning ClusterRoles on and then reloading does not turn
// them off again, and a kind that appears mid-session still arrives hidden.
export function hiddenByDefault(kinds, seen = null, hidden = null) {
  const nextSeen = new Set(seen || [])
  const nextHidden = new Set(hidden || [])
  for (const k of kinds || []) {
    if (nextSeen.has(k)) continue
    nextSeen.add(k)
    if (SECONDARY_KINDS.has(k)) nextHidden.add(k)
  }
  return { seen: nextSeen, hidden: nextHidden }
}

// countKinds is what each chip shows: how many of that kind are on the board, before the
// kind filter itself is applied.
export function countKinds(objects) {
  const counts = new Map()
  for (const o of objects) counts.set(o.kind, (counts.get(o.kind) ?? 0) + 1)
  return counts
}

// groupByKind lays the canvas out: one column per kind, in the order the kinds first
// appear (which is the server's ordering — nodes, pods, workloads, then the operator's own
// objects). A column keeps its place even when it empties out, so a cluster losing all its
// pods does not reshuffle the board.
export function groupByKind(objects) {
  const cols = []
  const index = new Map()
  for (const o of objects) {
    let col = index.get(o.kind)
    if (!col) {
      col = { kind: o.kind, objects: [] }
      index.set(o.kind, col)
      cols.push(col)
    }
    col.objects.push(o)
  }
  return cols
}

// summariseWarnings counts the warning events attached to a list, for the header's badge.
export function summariseWarnings(objects) {
  return objects.reduce((n, o) => n + (o.events?.length ?? 0), 0)
}

// targetKey identifies a cluster across stacks, the same shape the picker's value uses.
export const targetKey = (t) => (t ? `${t.stackId}/${t.frameId}` : '')

// parseTargetKey turns it back into the two ids an API call needs.
export function parseTargetKey(key) {
  const [stackId, frameId] = String(key || '').split('/')
  return stackId && frameId ? { stackId: Number(stackId), frameId } : null
}

// ---------------------------------------------------------------- panes
//
// A pane is one object seen one way: {key, view} where view is 'state' | 'logs' | 'yaml'.
// Pins are a list of panes rather than a list of objects, which is the whole point — the
// crashing container's log and the custom resource's state are two different things to keep
// on screen, and they may well be two views of the same object.

export const paneId = (p) => `${p.key}::${p.view}`
export const samePane = (a, b) => !!a && !!b && a.key === b.key && a.view === b.view

// movePaneView switches a pinned pane's view in place. Three cases, and the last two are why
// this is a function rather than a map(): switching to a view that is ALREADY pinned would
// otherwise leave two identical panes, and switching a pane that is not pinned at all must
// not add one.
export function movePaneView(pins, from, to) {
  if (!pins.some((p) => samePane(p, from))) return pins
  if (pins.some((p) => samePane(p, to))) return pins.filter((p) => !samePane(p, from))
  return pins.map((p) => (samePane(p, from) ? to : p))
}

// sortContainers orders a pod's containers for the log picker: the unhealthy first, then the
// rest in declaration order, init containers keeping their place among their own. On a
// six-container Percona pod the one you want is nearly always the one that is not running.
export function sortContainers(containers) {
  const rank = { bad: 0, warn: 1, done: 2, ok: 3, '': 3 }
  return [...(containers || [])]
    .map((c, i) => ({ c, i }))
    .sort((a, b) => (rank[a.c.tone] ?? 3) - (rank[b.c.tone] ?? 3) || a.i - b.i)
    .map(({ c }) => c)
}

// defaultContainer is the one a log pane opens on: the worst, or the first real container.
// '' when the pod reports none, which means kubectl picks for us — the right answer for a
// single-container pod.
export function defaultContainer(containers) {
  const sorted = sortContainers(containers)
  if (sorted.length === 0) return ''
  const worst = sorted[0]
  if (worst.tone === 'bad' || worst.tone === 'warn') return worst.name
  const real = sorted.find((c) => !c.init)
  return (real || sorted[0]).name
}

// defaultLogTarget is what a log pane asks for FIRST, and it depends on the source rather
// than on the pod: a live pod is read by container, so the pane opens on the worst one; a
// capture is read by file, and which files the collector kept for that pod is something only
// the server knows. So an archive is asked for nothing and the reply names one — asking for
// the pod's worst CONTAINER instead used to be answered with "no such file in this capture",
// which hid logs.txt, summary.txt and everything the collector pulled off the pod's disk.
export function defaultLogTarget(obj, archive) {
  return archive ? '' : defaultContainer(obj?.containers)
}

// ---------------------------------------------------------------- sources
//
// The board reads from a live cluster, a kept capture, or an uploaded archive. One string
// identifies any of them, because it has to survive a <select> — "live:8/frame-x",
// "dump:12", "upload:<token>".

export const SOURCE_LIVE = 'live'

export const sourceId = (kind, id) => `${kind}:${id}`

export function parseSourceId(value) {
  const raw = String(value || '')
  const i = raw.indexOf(':')
  if (i < 0) return null
  const kind = raw.slice(0, i)
  const id = raw.slice(i + 1)
  if (!id) return null
  if (kind === SOURCE_LIVE) {
    const ids = parseTargetKey(id)
    return ids ? { kind, ...ids, id } : null
  }
  if (kind === 'dump' || kind === 'upload') return { kind, id }
  return null
}

// isLive says whether the board should poll. A capture is one instant; polling it would ask
// the same question of the same file every five seconds forever.
export const isLive = (src) => src?.kind === SOURCE_LIVE

// POLL_CHOICES are the sample rates the page offers. Each sample is three kubectl calls
// inside the k3s node, so the fast end is for watching a failover and the slow end is for
// leaving open beside something else.
export const POLL_CHOICES = [
  { value: 2000, label: '2s' },
  { value: 5000, label: '5s' },
  { value: 15000, label: '15s' },
  { value: 60000, label: '1m' },
  { value: 0, label: 'paused' },
]
