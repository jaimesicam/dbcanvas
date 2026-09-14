import { useEffect, useMemo, useRef, useState } from 'react'
import { Icon } from '../components/Icons.jsx'
import { Card, Button, Badge, Field, inputCls } from '../components/ui.jsx'
import { useTerminals } from '../terminal/TerminalProvider.jsx'
import { usePolling } from '../lib/usePolling.jsx'
import { useHandoff } from '../lib/handoff.js'
import {
  sampleApi, sampleNodeApi, nodeKey, LOG_TONE, LOG_PREFIX, TLS_MODES, FILE_LANG, downloadFile,
} from '../lib/sampleApi.js'
import { TOOL_HELP } from '../lib/help.js'

// Sample Client Code — a runnable client program for a deployment on the canvas.
//
// The page is three choices and a button, and that is the whole design:
//
//   Target   mysql-01.example.net — Percona Server 8.4
//   Client   Python — mysql-connector-python
//   Example  Full CRUD
//                                            → Generate
//
// Everything else on the page is downstream of those three. The client list is filtered by
// the target's engine, so an irrelevant combination is not disabled — it is absent. The TLS
// posture starts at whatever DBCanvas derived from how the endpoint was actually deployed,
// and says why. And the generated code carries the real host, port, account, password and
// certificate path, because DBCanvas deployed the thing and knows all of them.
//
// Then three buttons that touch the node, in the order you would want them: save the
// project, prepare the environment, run it. Run does the other two first if they are
// needed — the point is one click — and every command any of them runs is echoed into the
// log before it runs. That is not debugging output. On a lab host, "what exactly did it
// install, and what would I have typed" is the thing being taught.

export default function SampleCode() {
  const { openTerminal } = useTerminals()

  const [nodes, setNodes] = useState(null)
  const [catalog, setCatalog] = useState(null)
  const [nodeId, setNodeId] = useState('')
  const [targets, setTargets] = useState([])
  const [certs, setCerts] = useState([])

  const [targetId, setTargetId] = useState('')
  const [clientId, setClientId] = useState('') // "language/client"
  const [scenario, setScenario] = useState('crud')
  const [tls, setTls] = useState('') // "" = whatever DBCanvas derived
  const [clientCert, setClientCert] = useState('')

  const [gen, setGen] = useState(null)
  const [activeFile, setActiveFile] = useState(0)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState('')
  const [jobId, setJobId] = useState('')
  const [job, setJob] = useState(null)
  const [copied, setCopied] = useState('')

  const node = useMemo(() => (nodes || []).find((n) => nodeKey(n) === nodeId) || null, [nodes, nodeId])
  const api = useMemo(() => (node ? sampleNodeApi(node.stackId, node.nodeId) : null), [node])
  const target = useMemo(() => targets.find((t) => t.id === targetId) || null, [targets, targetId])

  // The clients that can speak the selected endpoint's engine — the filter that makes an
  // irrelevant combination unreachable rather than merely discouraged.
  const clients = useMemo(() => {
    if (!catalog || !target) return []
    return (catalog.databases.find((d) => d.id === target.engine)?.clients) || []
  }, [catalog, target])
  const client = useMemo(() => clients.find((c) => c.id === clientId) || null, [clients, clientId])

  // ---- what exists -----------------------------------------------------------

  useEffect(() => {
    sampleApi.catalog().then(setCatalog).catch((e) => setErr(e.message))
    sampleApi.nodes().then(setNodes).catch((e) => { setNodes([]); setErr(e.message) })
  }, [])

  // The node panel's "Sample Client Code" button leaves the Linux Client it wants here. Held as
  // state rather than consumed inside the fetch: arriving at a tab that is already open
  // re-runs no mount effect (see lib/handoff.js).
  const [want, setWant] = useState('')
  useHandoff('dbcanvas.sampleCodeNode', setWant)
  useEffect(() => {
    if (!nodes?.length) return
    const found = nodes.find((n) => nodeKey(n) === want)
    setNodeId(nodeKey(found || nodes[0]))
  }, [nodes, want])

  // The endpoints belong to the selected node's own stack.
  useEffect(() => {
    if (!api) { setTargets([]); setCerts([]); return }
    let alive = true
    api.targets()
      .then((r) => {
        if (!alive) return
        setTargets(r?.targets || [])
        setCerts(r?.clientCerts || [])
      })
      .catch((e) => { if (alive) { setTargets([]); setErr(e.message) } })
    return () => { alive = false }
  }, [api])

  // Keep the dependent selections valid as the ones above them change. Each of these is a
  // snap rather than a reset: changing the endpoint from one MySQL node to another should
  // not throw away the client and example already chosen.
  useEffect(() => {
    if (!targets.length) { setTargetId(''); return }
    if (!targets.some((t) => t.id === targetId)) setTargetId(targets[0].id)
  }, [targets]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!clients.length) { setClientId(''); return }
    if (!clients.some((c) => c.id === clientId)) setClientId(clients[0].id)
  }, [clients]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!client) return
    if (!client.scenarios.includes(scenario)) setScenario(client.scenarios[client.scenarios.length - 1])
  }, [client]) // eslint-disable-line react-hooks/exhaustive-deps
  // A TLS override belongs to the endpoint that was chosen when it was made.
  useEffect(() => { setTls(''); setClientCert('') }, [targetId])

  const sampleId = target && client && scenario ? `${target.engine}/${client.id}/${scenario}` : ''
  const request = () => ({ sample: sampleId, target: targetId, tls, clientCert })

  // ---- generate --------------------------------------------------------------

  // Re-generate whenever the selection changes. It costs one request and changes nothing on
  // the node, so there is no reason to make the user press a button to see the code — the
  // Generate button stays, because the flow the feature describes has one and because it is
  // how you get the code back after an error.
  useEffect(() => {
    if (!api || !sampleId) { setGen(null); return }
    let alive = true
    setErr('')
    api.generate(request())
      .then((g) => { if (alive) { setGen(g); setActiveFile(0) } })
      .catch((e) => { if (alive) { setGen(null); setErr(e.message) } })
    return () => { alive = false }
  }, [api, sampleId, tls, clientCert]) // eslint-disable-line react-hooks/exhaustive-deps

  async function generate() {
    if (!api || !sampleId) return
    setErr(''); setBusy('generate')
    try {
      setGen(await api.generate(request()))
      setActiveFile(0)
    } catch (e) { setErr(e.message) } finally { setBusy('') }
  }

  // ---- the node --------------------------------------------------------------

  const running = job?.status === 'running'
  usePolling(async () => {
    if (!api || !jobId) return
    try { setJob(await api.job(jobId)) } catch { /* keep the last snapshot */ }
  }, 700, { enabled: !!jobId && running })

  async function act(action) {
    if (!api || !sampleId) return
    setErr(''); setBusy(action); setJob(null)
    try {
      const id = await api.run({ ...request(), action })
      setJobId(id)
      setJob(await api.job(id))
    } catch (e) { setErr(e.message) } finally { setBusy('') }
  }

  async function stop() {
    if (!api || !jobId) return
    try { setJob(await api.stop(jobId)) } catch (e) { setErr(e.message) }
  }

  async function copy(text, what) {
    try { await navigator.clipboard.writeText(text) } catch { /* clipboard denied */ }
    setCopied(what)
    setTimeout(() => setCopied(''), 1200)
  }

  // ---- render ----------------------------------------------------------------

  if (nodes === null) return <div className="py-10 text-center text-muted">Loading…</div>

  if (!nodes.length) {
    return (
      <Card title="Sample Client Code" subtitle="Runnable client code for a deployment on your canvas">
        <p className="text-sm text-muted">
          These samples run on a <span className="font-medium text-fg">Linux Client</span> node, and there is
          no running one in your stacks. Add one from the designer's <span className="font-medium text-fg">Storage
          &amp; Clients</span> group and deploy it — it is a bare OS host, and DBCanvas installs whatever a
          sample needs on it when you ask for one.
        </p>
      </Card>
    )
  }

  const files = gen?.files || []
  const file = files[activeFile] || null

  return (
    <div className="space-y-4">
      {err && <div className="rounded-lg border border-danger/30 bg-danger/15 px-3 py-2 text-sm text-danger">{err}</div>}

      <Card
        title="Sample Client Code"
        subtitle="Pick an endpoint, a client library and an example. DBCanvas generates it against the real deployment, installs what it needs, and runs it."
        action={node && (
          <Button size="sm" variant="ghost" onClick={() => openTerminal({
            stackId: node.stackId, nodeId: node.nodeId, title: `${node.label} · root`,
          })}>
            <Icon.Terminal size={15} /> Open terminal
          </Button>
        )}
      >
        <div className="grid gap-3 md:grid-cols-2">
          <Field label="Linux Client" help={TOOL_HELP.scNode}>
            <select className={inputCls} value={nodeId} onChange={(e) => setNodeId(e.target.value)}>
              {nodes.map((n) => (
                <option key={nodeKey(n)} value={nodeKey(n)}>
                  {n.stackName} · {n.label} ({n.os}{n.osVersion ? ` ${n.osVersion}` : ''})
                </option>
              ))}
            </select>
          </Field>
          <Field label="Target" help={TOOL_HELP.scTarget}
            hint={target?.note || (targets.length ? '' : 'No running database in this stack yet.')}>
            <select className={inputCls} value={targetId} disabled={!targets.length}
              onChange={(e) => setTargetId(e.target.value)}>
              {!targets.length && <option value="">—</option>}
              {targets.map((t) => (
                <option key={t.id} value={t.id}>{t.label} — {t.product}{t.major ? ` ${t.major}` : ''}</option>
              ))}
            </select>
          </Field>
          <Field label="Client" help={TOOL_HELP.scClient} hint={client?.summary || ''}>
            <select className={inputCls} value={clientId} disabled={!clients.length}
              onChange={(e) => setClientId(e.target.value)}>
              {!clients.length && <option value="">—</option>}
              {clients.map((c) => (
                <option key={c.id} value={c.id}>{c.languageLabel} — {c.label}</option>
              ))}
            </select>
          </Field>
          <Field label="Example" help={TOOL_HELP.scScenario}
            hint={catalog?.scenarios.find((s) => s.id === scenario)?.blurb || ''}>
            <select className={inputCls} value={scenario} disabled={!client}
              onChange={(e) => setScenario(e.target.value)}>
              {(catalog?.scenarios || []).filter((s) => client?.scenarios.includes(s.id)).map((s) => (
                <option key={s.id} value={s.id}>{s.label}</option>
              ))}
            </select>
          </Field>
        </div>

        {target && (
          <div className="mt-3 grid gap-3 md:grid-cols-2">
            <Field label="TLS" help={TOOL_HELP.scTLS}
              hint={tls ? 'Chosen here rather than derived from the deployment.' : target.tls.why}>
              <select className={inputCls} value={tls || target.tls.mode} onChange={(e) => setTls(e.target.value)}>
                {TLS_MODES.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.label}{m.id === target.tls.mode ? ' — as deployed' : ''}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Client certificate (mutual TLS)" help={TOOL_HELP.scClientCert}
              hint={certs.length
                ? 'Issued by the Intranet CA. DBCanvas copies it onto this node with the project; the key never passes through your browser.'
                : "None issued yet — the Intranet node's Certificates tab issues one per database username."}>
              <select className={inputCls} value={clientCert} disabled={!certs.length}
                onChange={(e) => setClientCert(e.target.value)}>
                <option value="">None</option>
                {certs.map((c) => <option key={c} value={c}>{c}</option>)}
              </select>
            </Field>
          </div>
        )}

        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Button onClick={generate} disabled={!sampleId || busy === 'generate'}>
            <Icon.Sparkles size={15} /> Generate
          </Button>
          {target && (
            <span className="font-mono text-xs text-muted">
              {target.host}:{target.port} · {target.user} · {target.database}
            </span>
          )}
        </div>
      </Card>

      {gen && (
        <>
          <Card title={gen.title} subtitle={gen.explain}>
            <div className="flex flex-wrap gap-1.5">
              {(gen.requires || []).map((r) => (
                <span key={r} className="rounded-full bg-surface2 px-2 py-0.5 font-mono text-[11px] text-muted">{r}</span>
              ))}
            </div>
            {!!gen.deps?.length && (
              <div className="mt-3 space-y-1 rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
                <div className="font-medium text-fg">Third-party dependencies this example installs</div>
                {gen.deps.map((d) => (
                  <div key={d.name}>
                    <a className="font-mono text-fg hover:underline" href={d.url} target="_blank" rel="noreferrer">{d.name}</a>
                    {' — '}{d.license}
                    {d.note && <div className="pl-3 text-muted">{d.note}</div>}
                  </div>
                ))}
                <div className="pt-1">
                  DBCanvas installs these from their own ecosystems at run time and does not redistribute them;
                  they keep their own licences. The project it generates for you carries no licence of its own —
                  it is yours to use however you like.
                </div>
              </div>
            )}
          </Card>

          <Card
            title="Project"
            subtitle={`Written to ${gen.dir} on ${node?.label}`}
            action={file && (
              <div className="flex gap-1.5">
                <Button size="sm" variant="ghost" onClick={() => copy(file.body, file.name)}>
                  {copied === file.name ? <Icon.Check size={15} /> : <Icon.Copy size={15} />} Copy
                </Button>
                <Button size="sm" variant="ghost" onClick={() => downloadFile(file.name, file.body)}>
                  <Icon.File size={15} /> Download
                </Button>
              </div>
            )}
          >
            <div className="mb-2 flex flex-wrap gap-1">
              {files.map((f, i) => (
                <button key={f.name} onClick={() => setActiveFile(i)}
                  className={`rounded-md px-2 py-1 font-mono text-xs ${i === activeFile
                    ? 'bg-primary/15 text-primary' : 'text-muted hover:bg-surface2'}`}>
                  {f.name}
                  <span className="ml-1.5 text-[10px] opacity-70">{FILE_LANG[f.lang] || f.lang}</span>
                </button>
              ))}
            </div>
            {file && (
              <pre className="max-h-[32rem] overflow-auto whitespace-pre rounded-lg border bg-bg p-3 font-mono text-[11px] leading-relaxed text-fg">
                {file.body}
              </pre>
            )}
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <Button onClick={() => act('run')} disabled={!!busy || running}>
                <Icon.Play size={15} /> Run
              </Button>
              <Button variant="ghost" onClick={() => act('prepare')} disabled={!!busy || running}>
                <Icon.Sliders size={15} /> Prepare environment
              </Button>
              <Button variant="ghost" onClick={() => act('save')} disabled={!!busy || running}>
                <Icon.File size={15} /> Save to Linux Client
              </Button>
              <Button variant="ghost" onClick={() => act('reset')} disabled={!!busy || running}>
                <Icon.Trash size={15} /> Reset
              </Button>
              {running && <Button variant="danger" onClick={stop}><Icon.Pause size={15} /> Stop</Button>}
              <span className="font-mono text-xs text-muted">{gen.runCmd}</span>
            </div>
          </Card>
        </>
      )}

      {job && <JobLog job={job} />}
    </div>
  )
}

// JobLog is the transcript: every check, every command, every line of output, and the
// verdict. It is the part of the page that makes the feature teachable rather than magic.
export function JobLog({ job }) {
  const endRef = useRef(null)
  useEffect(() => { endRef.current?.scrollIntoView({ block: 'nearest' }) }, [job?.log?.length])

  const tone = { running: 'primary', done: 'success', error: 'danger', canceled: 'warning' }[job.status] || 'muted'
  const title = { save: 'Saving', prepare: 'Preparing environment', run: 'Running', reset: 'Reset' }[job.action] || job.action
  return (
    <Card
      title={title}
      subtitle={job.ran && job.status !== 'running' ? `The program exited ${job.exit}.` : job.message || ''}
      action={<Badge tone={tone}>{job.status}</Badge>}
    >
      <pre className="max-h-[26rem] overflow-auto whitespace-pre-wrap rounded-lg border bg-bg p-3 font-mono text-[11px] leading-relaxed">
        {(job.log || []).map((ln, i) => (
          <div key={i} className={LOG_TONE[ln.kind] || 'text-fg'}>
            {(LOG_PREFIX[ln.kind] || '') + ln.text}
          </div>
        ))}
        <div ref={endRef} />
      </pre>
    </Card>
  )
}
