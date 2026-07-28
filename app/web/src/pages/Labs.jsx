import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge } from '../components/ui.jsx'
import { labsApi } from '../lib/labsApi.js'
import { stackApi, DEPLOY_TONE, TTL_OPTIONS } from '../lib/stackApi.js'
import { useTerminals } from '../terminal/TerminalProvider.jsx'

// Labs (experimental) — hands-on scenarios. Starting one provisions a real,
// disposable stack through the same design-JSON + deploy pipeline Stack
// Designer uses; "Check Work" inspects that stack's actual live state rather
// than grading anything the learner typed.

export default function Labs() {
  const [labs, setLabs] = useState(null)
  const [myRuns, setMyRuns] = useState([])
  const [active, setActive] = useState(null) // { lab, labRun, stack }
  const [err, setErr] = useState('')
  const [starting, setStarting] = useState('')
  const [notesOpenId, setNotesOpenId] = useState(null)
  const [search, setSearch] = useState('')
  const [collapsedCats, setCollapsedCats] = useState(() => new Set())

  const load = () => {
    labsApi.list().then(setLabs).catch((e) => setErr(e.message))
    labsApi.myRuns().then((r) => setMyRuns(Array.isArray(r) ? r : [])).catch(() => {})
  }
  useEffect(load, [])

  // Poll the lab's stack while it deploys (and afterwards, so node status stays fresh).
  const pollRef = useRef(null)
  useEffect(() => {
    if (!active) return
    const tick = async () => {
      try {
        const s = await stackApi.get(active.stack.id)
        setActive((a) => (a ? { ...a, stack: s } : a))
      } catch { /* keep last known */ }
    }
    tick()
    pollRef.current = setInterval(tick, 3000)
    return () => clearInterval(pollRef.current)
  }, [active?.stack.id])

  async function startLab(lab) {
    setErr(''); setStarting(lab.id)
    try {
      const { labRun, stack } = await labsApi.start(lab.id)
      // Idempotent: a resumed run whose nodes are already running just no-ops here.
      await stackApi.deploy(stack.id).catch((e) => { if (e.status !== 409) throw e })
      setActive({ lab, labRun, stack })
    } catch (e) {
      setErr(e.message)
    } finally {
      setStarting('')
    }
  }

  async function endLab() {
    if (!active) return
    try { await labsApi.finish(active.lab.id) } catch { /* still remove */ }
    // remove (not destroy) — destroy only resets the stack to draft, which would
    // leave the finished lab's disposable stack sitting in Database Stacks forever.
    try { await stackApi.remove(active.stack.id) } catch { /* best effort */ }
    setActive(null)
    load()
  }

  if (err && !labs) return <div className="p-6 text-sm text-danger">{err}</div>
  if (!labs) return <div className="p-6 text-sm text-muted">Loading labs…</div>

  if (active) return <LabRun active={active} setActive={setActive} onEnd={endLab} />

  const activeRunFor = (lab) => myRuns.find((r) => r.labId === lab.id && !r.finishedAt)

  const q = search.trim().toLowerCase()
  const filtered = !q ? labs : labs.filter((lab) =>
    [lab.title, lab.description, lab.database, lab.technology, lab.category].some((s) => (s || '').toLowerCase().includes(q)))

  const resumable = filtered.filter((lab) => activeRunFor(lab))
  const rest = filtered.filter((lab) => !activeRunFor(lab))

  // Group Database -> Technology -> Category, preserving each level's
  // first-seen order from the catalog (already ordered sensibly
  // server-side) rather than alphabetizing. Each level collapses
  // independently, keyed by its full "Database::Technology::Category" path
  // so the same category name under a different technology/database
  // doesn't share collapse state.
  const dbOrder = []
  const byDb = new Map()
  for (const lab of rest) {
    const db = lab.database || 'Other'
    const tech = lab.technology || 'General'
    const cat = lab.category || 'Other'
    if (!byDb.has(db)) { byDb.set(db, { order: [], techs: new Map() }); dbOrder.push(db) }
    const dbNode = byDb.get(db)
    if (!dbNode.techs.has(tech)) { dbNode.techs.set(tech, { order: [], cats: new Map() }); dbNode.order.push(tech) }
    const techNode = dbNode.techs.get(tech)
    if (!techNode.cats.has(cat)) { techNode.cats.set(cat, []); techNode.order.push(cat) }
    techNode.cats.get(cat).push(lab)
  }

  function toggleGroup(key) {
    setCollapsedCats((s) => {
      const next = new Set(s)
      if (next.has(key)) next.delete(key); else next.add(key)
      return next
    })
  }

  // countLabs recursively sums how many labs sit under a Database or
  // Technology node, for the "(N)" count shown next to its header.
  function countInTechs(techs) {
    let n = 0
    for (const { cats } of techs.values()) for (const labsIn of cats.values()) n += labsIn.length
    return n
  }

  const cardProps = (lab) => ({
    lab,
    resuming: !!activeRunFor(lab),
    starting: starting === lab.id,
    onStart: () => startLab(lab),
    notesOpen: notesOpenId === lab.id,
    onToggleNotes: () => setNotesOpenId((id) => (id === lab.id ? null : lab.id)),
  })

  return (
    <div className="p-6 space-y-6">
      <div>
        <h1 className="flex items-center gap-2 text-lg font-semibold text-fg">
          <Icon.Flask size={20} /> Labs <span className="text-muted font-normal">(experimental)</span>
        </h1>
        <p className="mt-1 text-sm text-muted">
          Hands-on scenarios on a real, disposable stack. "Check Work" verifies your actual cluster
          state — it never grades typed answers.
        </p>
      </div>
      {err && <div className="text-sm text-danger">{err}</div>}

      <input
        type="search"
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search labs by title, description, database, technology, or category…"
        className="w-full rounded-lg border bg-surface px-3 py-2 text-sm text-fg"
      />

      {resumable.length > 0 && (
        <div>
          <h2 className="mb-2 text-sm font-semibold text-fg">Resume where you left off</h2>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {resumable.map((lab) => <LabCard key={lab.id} {...cardProps(lab)} />)}
          </div>
        </div>
      )}

      {dbOrder.length === 0 && resumable.length === 0 && (
        <p className="text-sm text-muted">No labs match "{search}".</p>
      )}

      {dbOrder.map((db) => {
        const dbNode = byDb.get(db)
        const dbKey = db
        const dbCollapsed = collapsedCats.has(dbKey)
        return (
          <div key={dbKey}>
            <button
              className="mb-2 flex w-full items-center gap-2 text-left text-base font-semibold text-fg"
              onClick={() => toggleGroup(dbKey)}
            >
              <Icon.Chevron size={16} className={dbCollapsed ? '-rotate-90' : ''} />
              {db} <span className="font-normal text-muted">({countInTechs(dbNode.techs)})</span>
            </button>
            {!dbCollapsed && (
              <div className="ml-2 space-y-4 border-l pl-4">
                {dbNode.order.map((tech) => {
                  const techNode = dbNode.techs.get(tech)
                  const techKey = `${db}::${tech}`
                  const techCollapsed = collapsedCats.has(techKey)
                  let techCount = 0
                  for (const labsIn of techNode.cats.values()) techCount += labsIn.length
                  return (
                    <div key={techKey}>
                      <button
                        className="mb-2 flex w-full items-center gap-2 text-left text-sm font-semibold text-fg"
                        onClick={() => toggleGroup(techKey)}
                      >
                        <Icon.Chevron size={14} className={techCollapsed ? '-rotate-90' : ''} />
                        {tech} <span className="font-normal text-muted">({techCount})</span>
                      </button>
                      {!techCollapsed && (
                        <div className="space-y-3">
                          {techNode.order.map((cat) => {
                            const catLabs = techNode.cats.get(cat)
                            const catKey = `${db}::${tech}::${cat}`
                            const catCollapsed = collapsedCats.has(catKey)
                            return (
                              <div key={catKey}>
                                <button
                                  className="mb-2 flex w-full items-center gap-2 text-left text-xs font-semibold text-muted"
                                  onClick={() => toggleGroup(catKey)}
                                >
                                  <Icon.Chevron size={12} className={catCollapsed ? '-rotate-90' : ''} />
                                  {cat} <span className="font-normal">({catLabs.length})</span>
                                </button>
                                {!catCollapsed && (
                                  <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                                    {catLabs.map((lab) => <LabCard key={lab.id} {...cardProps(lab)} />)}
                                  </div>
                                )}
                              </div>
                            )
                          })}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
          </div>
        )
      })}

      {myRuns.length > 0 && (
        <Card title="Recent attempts">
          <ul className="space-y-1 text-sm">
            {myRuns.slice(0, 8).map((r) => (
              <li key={r.id} className="flex items-center justify-between gap-3 text-muted">
                <span>{labs.find((l) => l.id === r.labId)?.title || r.labId}</span>
                <span className="flex items-center gap-2">
                  <span>{new Date(r.startedAt).toLocaleString()}</span>
                  <Badge tone={r.finishedAt ? 'muted' : 'success'}>{r.finishedAt ? 'finished' : 'active'}</Badge>
                </span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  )
}

// LabCard is one catalog entry — pulled out so it can render identically in
// both the pinned "Resume where you left off" section and each collapsible
// category section.
function LabCard({ lab, resuming, starting, onStart, notesOpen, onToggleNotes }) {
  return (
    <Card title={lab.title} subtitle={lab.difficulty}>
      <p className="text-sm text-muted">{lab.description}</p>
      <p className="mt-1 text-xs text-muted">
        Time limit: {TTL_OPTIONS.find((t) => t.id === lab.timeLimit)?.label || lab.timeLimit}
      </p>
      <LectureNotesToggle notes={lab.lectureNotes} open={notesOpen} onToggle={onToggleNotes} />
      <Button className="mt-4" onClick={onStart} disabled={starting}>
        {starting ? (resuming ? 'Resuming…' : 'Starting…') : (resuming ? 'Resume Lab' : 'Start Lab')}
      </Button>
    </Card>
  )
}

function LabRun({ active, setActive, onEnd }) {
  const { lab, stack } = active
  const { openTerminal } = useTerminals()
  const [results, setResults] = useState({}) // stepId -> {passed,message}
  const [checking, setChecking] = useState('')
  const [notesOpen, setNotesOpen] = useState(false)
  const [confirmEnd, setConfirmEnd] = useState(false)

  const deployments = stack.deployments || []
  const depByNode = {}
  for (const d of deployments) depByNode[d.nodeId] = d
  const design = safeParse(stack.design) || { nodes: [] }
  const patroniNodes = design.nodes.filter((n) => n.type === 'patroni')
  const provisioning = deployments.length === 0 || deployments.some((d) => d.state === 'pending' || d.state === 'provisioning')
  const failed = deployments.filter((d) => d.state === 'error')
  const expired = stack.status === 'expired'
  const remaining = !expired && stack.expiresAt ? formatRemaining(stack.expiresAt) : null

  async function checkStep(stepId) {
    setChecking(stepId)
    try {
      const res = await labsApi.checkStep(lab.id, stepId)
      setResults((r) => ({ ...r, [stepId]: res }))
    } catch (e) {
      setResults((r) => ({ ...r, [stepId]: { passed: false, message: e.message } }))
    } finally {
      setChecking('')
    }
  }

  return (
    <div className="p-6 space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-lg font-semibold text-fg">
            <Icon.Flask size={20} /> {lab.title}
            {expired
              ? <Badge tone="danger">expired</Badge>
              : remaining && <Badge tone={remaining.low ? 'warning' : 'muted'}>{remaining.label} remaining</Badge>}
          </h1>
          <p className="text-sm text-muted">{lab.description}</p>
          <LectureNotesToggle notes={lab.lectureNotes} open={notesOpen} onToggle={() => setNotesOpen((v) => !v)} />
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => setActive(null)}>Back to catalog</Button>
          <Button variant="danger" onClick={() => setConfirmEnd(true)}>End Lab</Button>
        </div>
      </div>

      {confirmEnd && (
        <EndLabConfirmModal
          labTitle={lab.title}
          onCancel={() => setConfirmEnd(false)}
          onConfirm={() => { setConfirmEnd(false); onEnd() }}
        />
      )}

      {expired ? (
        <Card title="Session expired">
          <p className="text-sm text-danger">
            This lab's time limit ({TTL_OPTIONS.find((t) => t.id === lab.timeLimit)?.label || lab.timeLimit}) has
            passed and its cluster was torn down. Go back to the catalog to start a fresh attempt.
          </p>
        </Card>
      ) : (
        <Card title="Cluster" subtitle={provisioning ? 'Provisioning your lab stack…' : 'Ready'}>
          <div className="flex flex-wrap gap-2">
            {design.nodes.map((n) => {
              const dep = depByNode[n.id]
              return (
                <div key={n.id} className="flex items-center gap-2 rounded-lg border px-3 py-1.5 text-sm">
                  <span className="text-fg">{n.label}</span>
                  <Badge tone={DEPLOY_TONE[dep?.state] || 'muted'}>{dep?.state || 'pending'}</Badge>
                  {n.type === 'vnc' && dep?.state === 'running' && dep?.config?.webPort && (
                    <a
                      href={`http://${location.hostname}:${dep.config.webPort}/vnc.html`}
                      target="_blank"
                      rel="noreferrer"
                      className="flex items-center gap-1 text-primary hover:underline"
                    >
                      <Icon.External size={13} /> Open VNC
                    </a>
                  )}
                  {dep?.state === 'running' && (
                    <button
                      className="text-primary hover:underline"
                      onClick={() => openTerminal({ stackId: stack.id, nodeId: n.id, title: n.label })}
                    >
                      Terminal
                    </button>
                  )}
                </div>
              )
            })}
          </div>
          {failed.length > 0 && (
            <p className="mt-3 text-sm text-danger">{failed.length} node(s) failed to deploy — check Stack Designer for details.</p>
          )}
          {provisioning && patroniNodes.length > 0 && (
            <p className="mt-3 text-sm text-muted">This can take a few minutes the first time (base images, systemd boot, etcd/Patroni bootstrap).</p>
          )}
        </Card>
      )}

      {!expired && lab.steps.map((step) => {
        const res = results[step.id]
        return (
          <Card key={step.id} title={step.title}>
            <p className="text-sm text-fg whitespace-pre-wrap">{step.instructions}</p>
            {step.hint && <p className="mt-2 text-xs text-muted">Hint: {step.hint}</p>}
            <div className="mt-4 flex items-center gap-3">
              <Button onClick={() => checkStep(step.id)} disabled={checking === step.id || provisioning}>
                {checking === step.id ? 'Checking…' : 'Check Work'}
              </Button>
              {res && (
                <span className={`text-sm ${res.passed ? 'text-success' : 'text-danger'}`}>
                  {res.passed ? <Icon.Check size={14} /> : null} {res.message}
                </span>
              )}
            </div>
          </Card>
        )
      })}
    </div>
  )
}

// EndLabConfirmModal guards "End Lab" — it tears down the lab's disposable
// stack for real (containers + volumes gone), so an accidental click
// shouldn't be able to do that without a second step.
function EndLabConfirmModal({ labTitle, onCancel, onConfirm }) {
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" onMouseDown={onCancel}>
      <div className="w-full max-w-sm rounded-xl border bg-surface p-5 shadow-2xl" onMouseDown={(e) => e.stopPropagation()}>
        <h3 className="mb-1 text-sm font-semibold">End "{labTitle}"?</h3>
        <p className="mb-4 text-xs text-muted">
          This <span className="font-semibold text-danger">permanently tears down</span> this lab's cluster (containers and volumes) and
          disconnects any open terminals. Your Check Work progress on this attempt won't be recoverable. This can't be undone.
        </p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={onCancel}>Cancel</Button>
          <Button variant="danger" size="sm" onClick={onConfirm}>End Lab</Button>
        </div>
      </div>
    </div>,
    document.body,
  )
}

// LectureNotesToggle is a link that expands the lab's bundled background
// reading on the technology it exercises — kept in-app rather than an
// external URL, so it's always available.
function LectureNotesToggle({ notes, open, onToggle }) {
  if (!notes) return null
  return (
    <div className="mt-2">
      <button className="text-sm text-primary hover:underline" onClick={onToggle}>
        {open ? 'Hide lecture notes' : 'Lecture notes'}
      </button>
      {open && (
        <div className="mt-2 max-h-80 overflow-y-auto rounded-lg border bg-bg p-3 text-sm text-fg whitespace-pre-wrap">
          {notes}
        </div>
      )}
    </div>
  )
}

// formatRemaining turns a stack's expiresAt into a short "1h 12m" label. `low`
// flags under 15 minutes left, so the badge can call it out.
function formatRemaining(expiresAt) {
  const ms = new Date(expiresAt).getTime() - Date.now()
  if (!Number.isFinite(ms) || ms <= 0) return { label: '0m', low: true }
  const mins = Math.round(ms / 60000)
  const h = Math.floor(mins / 60)
  const m = mins % 60
  return { label: h > 0 ? `${h}h ${m}m` : `${m}m`, low: mins <= 15 }
}

function safeParse(json) {
  try { return typeof json === 'string' ? JSON.parse(json) : json } catch { return null }
}
