import { useState } from 'react'
import { Icon } from './Icons.jsx'

// RepmgrGuide — how to operate a repmgr cluster, with the commands, on the cluster you are
// looking at.
//
// Same argument as BackupGuide, which this follows deliberately: the panel already knows every
// value these commands need — the config file (per major, and not where repmgr's own
// documentation puts it), the binary (not on postgres's PATH on the EL images), this node's
// name and id, and who the other members are — and until now it printed none of them. So a
// person who wanted to rehearse a switchover had to go and find all four before they could type
// anything, which is exactly the friction a lab is supposed to remove.
//
// Every command here is rendered from this deployment. Where a value is genuinely a choice —
// which node to switch over to — it comes from the cluster's own peer list rather than a
// placeholder.
//
// The ordering is the order somebody needs them in, not the order the manual has them: look at
// the cluster, then check it, then change it. The dangerous ones are last and marked.

function CopyButton({ text }) {
  const [done, setDone] = useState(false)
  return (
    <button
      title="Copy" aria-label="Copy command"
      onClick={async () => { try { await navigator.clipboard.writeText(text) } catch { /* */ } setDone(true); setTimeout(() => setDone(false), 1200) }}
      className="rounded p-1 text-muted hover:bg-surface2 hover:text-fg"
    >
      {done ? <Icon.Check size={13} /> : <Icon.Copy size={13} />}
    </button>
  )
}

function Step({ n, title, children, cmd, danger }) {
  return (
    <div className={`rounded-lg border px-2.5 py-2 ${danger ? 'border-danger/30 bg-danger/[0.06]' : ''}`}>
      <div className="flex items-baseline gap-2">
        <span className="shrink-0 rounded bg-surface2 px-1.5 py-0.5 text-[10px] font-medium text-muted">{n}</span>
        <span className="text-xs font-semibold">{title}</span>
      </div>
      {children && <p className="mt-1 text-[11px] leading-snug text-muted">{children}</p>}
      {cmd && (
        <div className="mt-1.5 flex items-start gap-1 rounded-md border bg-bg px-2 py-1.5">
          <pre className="min-w-0 flex-1 overflow-x-auto whitespace-pre font-mono text-[11px] leading-relaxed text-fg">{cmd}</pre>
          <CopyButton text={cmd} />
        </div>
      )}
    </div>
  )
}

function Section({ title, hint, children }) {
  return (
    <div className="space-y-1.5">
      <div className="text-[11px] font-semibold uppercase tracking-wide text-muted">{title}</div>
      {hint && <p className="text-[11px] leading-snug text-muted">{hint}</p>}
      {children}
    </div>
  )
}

export default function RepmgrGuide({ cfg = {}, nodeLabel }) {
  const conf = cfg.repmgrConf || '/etc/repmgr/repmgr.conf'
  const bin = cfg.repmgrBin || 'repmgr'
  const svc = cfg.service || 'postgresql'
  const peers = cfg.peers || []
  // The switchover target: this node promotes itself, so the interesting peer is whichever one
  // is currently primary. The deployed roles are what we have; `cluster show` is what says now.
  const target = peers.find((p) => p.role === 'primary') || peers[0]
  // repmgr must run as postgres — it reads a postgres-owned config and writes to the data
  // directory — and the binary is not on that user's PATH on the EL images, hence the full path.
  const rp = (args) => `runuser -u postgres -- ${bin} -f ${conf} ${args}`

  return (
    <div className="space-y-4">
      <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
        <span className="font-medium text-fg">Run these on the node</span>, not on your machine: right-click
        {nodeLabel ? ` ${nodeLabel}` : ' this node'} on the canvas → <span className="font-medium">Enter root console</span>
        {' '}(or <span className="font-mono">dbcanvas node exec</span>). They step down to
        <span className="font-mono"> postgres</span> with <span className="font-mono">runuser</span>, because repmgr
        reads a postgres-owned config and writes into the data directory — run as root it either refuses or
        leaves files the server cannot read.
      </div>

      <Section
        title="Look at the cluster"
        hint={<>This node is <span className="font-mono">{cfg.hostname}</span> (node_id <span className="font-mono">{cfg.nodeId}</span>), deployed as <span className="font-mono">{cfg.role}</span>. Deployed as — repmgrd fails over on its own, so the commands below are the authority on what the roles are now.</>}
      >
        <Step n="1" title="Who is primary, and is everyone attached?" cmd={rp('cluster show')}>
          One line per member with its role and connection state. This is the first command to run and the one
          to run again after anything below. A member showing <span className="font-mono">? unreachable</span> is a
          node whose PostgreSQL is down or whose <span className="font-mono">conninfo</span> is wrong.
        </Step>
        <Step n="2" title="What has happened to it" cmd={rp('cluster event --limit=20')}>
          The cluster's history — registrations, failovers, promotions, repmgrd's own decisions — newest first.
          After an unexpected failover this is where the reason is. Narrow it with
          {' '}<span className="font-mono">--event=standby_promote</span>.
        </Step>
        <Step n="3" title="This node in detail" cmd={rp('node status')}>
          Role, upstream, replication slots, WAL positions and the state of the repmgrd daemon, for this member
          alone.
        </Step>
        <Step n="4" title="Is this node fit to do its job?" cmd={rp('node check')}>
          The pre-flight repmgr itself runs: replication lag, slots, WAL archiving, whether the data directory
          is where the config says. Run it on both ends before a switchover — a failing check here is the
          switchover failing later, but cheaper.
        </Step>
      </Section>

      <Section
        title="Switch the primary over"
        hint={<>A <span className="font-medium">switchover</span> is the planned one: the current primary is shut down cleanly, this node is promoted, and the other standbys are repointed. It runs <span className="font-medium">on the standby you want promoted</span> — this one — and needs SSH to the current primary, which the deploy sets up between all members.</>}
      >
        <Step n="5" title="Rehearse it first" cmd={rp('standby switchover --siblings-follow --dry-run')}>
          <span className="font-medium text-fg">Always run this before the real thing.</span> It performs every
          check — SSH, sudo, replication lag, the other standbys — and changes nothing.
          {' '}<span className="font-mono">--siblings-follow</span> is what repoints the <em>other</em> standbys at the new
          primary; without it they keep following the old one and fall out of the cluster.
        </Step>
        <Step n="6" title="Do it" cmd={rp('standby switchover --siblings-follow')} danger>
          Promotes this node and demotes {target ? <span className="font-mono">{target.nodeName}</span> : 'the current primary'} to
          a standby of it. Both databases restart. Nothing is lost — it is a clean shutdown, not a failover —
          but every open connection is dropped, so it is not a no-op on a cluster under load.
        </Step>
        <Step n="7" title="Confirm" cmd={rp('cluster show')}>
          The roles should have swapped and every sibling should now show this node as its upstream.
        </Step>
      </Section>

      <Section
        title="When the primary is already gone"
        hint={<>repmgrd does this automatically — it is running on every member. These are the manual equivalents, for when you have stopped repmgrd to watch the mechanism yourself, or when a node needs putting back afterwards.</>}
      >
        <Step n="8" title="Promote this standby" cmd={rp('standby promote')} danger>
          For a primary that is <em>already</em> down. It does not try to contact it — so running this while the
          primary is alive gives you two primaries and a split brain, which is why switchover exists.
        </Step>
        <Step n="9" title="Repoint a standby at the new primary" cmd={rp('standby follow')}>
          Run on each remaining standby after a manual promote. Switchover with
          {' '}<span className="font-mono">--siblings-follow</span> does this for you.
        </Step>
        <Step n="10" title="Bring an old primary back as a standby" cmd={rp(`node rejoin -d 'host=${target ? target.fqdn : '<new-primary>'} user=repmgr dbname=repmgr' --force-rewind`)}>
          Run on the node that used to be primary, naming the new one.
          {' '}<span className="font-mono">--force-rewind</span> runs <span className="font-mono">pg_rewind</span>, which is needed
          whenever the old primary accepted writes the new one never saw — the usual case after a real failover,
          and never after a clean switchover.
        </Step>
      </Section>

      <Section
        title="The daemon"
        hint={<>repmgrd is what makes failover automatic. It is enabled and running on every member; stop it when you want to drive the cluster by hand and watch what happens.</>}
      >
        <Step n="11" title="Is it running, everywhere?" cmd={rp('daemon status')}>
          One line per member. A standby whose repmgrd is not running will not be promoted, however healthy it is.
        </Step>
        <Step n="12" title="Stop or start it on this node" cmd={`systemctl stop repmgr-${cfg.pgMajor || ''} || systemctl stop repmgrd\nsystemctl start repmgr-${cfg.pgMajor || ''} || systemctl start repmgrd`}>
          The unit is <span className="font-mono">repmgr-{cfg.pgMajor}</span> on the Oracle Linux images and
          {' '}<span className="font-mono">repmgrd</span> on the Debian ones, so both spellings are here.
        </Step>
        <Step n="13" title="Watch it decide" cmd={`journalctl -u repmgr-${cfg.pgMajor || ''} -u repmgrd -f`}>
          Leave this running on a standby and stop PostgreSQL on the primary
          {' '}(<span className="font-mono">systemctl stop {svc}</span>) to watch the failover happen in real time.
        </Step>
      </Section>

      {peers.length > 0 && (
        <Section title="The other members" hint="As the cluster was deployed. Roles may have changed since — `cluster show` is the authority.">
          <div className="overflow-hidden rounded-lg border">
            <table className="w-full text-[11px]">
              <thead className="bg-surface2 text-muted">
                <tr>
                  <th className="px-2 py-1 text-left font-medium">node_id</th>
                  <th className="px-2 py-1 text-left font-medium">name</th>
                  <th className="px-2 py-1 text-left font-medium">host</th>
                  <th className="px-2 py-1 text-left font-medium">deployed as</th>
                </tr>
              </thead>
              <tbody>
                {peers.map((p) => (
                  <tr key={p.nodeId} className="border-t">
                    <td className="px-2 py-1 font-mono">{p.nodeId}</td>
                    <td className="px-2 py-1 font-mono">{p.nodeName}</td>
                    <td className="px-2 py-1 font-mono">{p.fqdn}</td>
                    <td className="px-2 py-1">{p.role}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="text-[11px] leading-snug text-muted">
            Every member can SSH to every other as <span className="font-mono">postgres</span>, which is what
            switchover needs and the only thing the key is good for. Check it by hand with
            {' '}<span className="font-mono">runuser -u postgres -- ssh {target ? target.fqdn : '<peer>'} hostname</span>.
          </p>
        </Section>
      )}
    </div>
  )
}
