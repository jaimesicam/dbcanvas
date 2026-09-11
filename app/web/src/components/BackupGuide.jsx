import { useState } from 'react'
import { Icon } from './Icons.jsx'

// BackupGuide — how to back up and restore, with the commands, on a deployed PostgreSQL
// node or cluster.
//
// The Backup tab used to be a button and a sentence. The button is the easy half: it takes
// a full backup. Everything else somebody actually needs — which backups exist, is WAL
// archiving healthy, delete that one, restore last Tuesday — happens on the node, and the
// panel knew all the facts those commands need (stanza, bucket, endpoint, unit, data
// directory) without ever printing them.
//
// So: every command here is rendered from this deployment's own config, not from a
// template with placeholders. `pgbackrest --stanza=<your stanza>` is a command somebody has
// to finish; `pgbackrest --stanza=pg-01 info` is one they can paste. Where a value is
// genuinely theirs to choose — which backup to delete, which point in time to recover to —
// it stays a placeholder, and the prose says what to put there.
//
// Engines, not products: `pgbackrest` for standalone PostgreSQL and Patroni, `barman` for
// repmgr (barman-cloud). The Kubernetes operators are deliberately not here — a Percona,
// CNPG or Crunchy backup is a custom resource, not a shell command, and its panel says so.
//
// The two engine bodies are exported so the render suite can assert the thing that matters:
// that a rendered command carries this deployment's own values and no leftover placeholder.

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

// Step is one thing you might want to do: a title, why/when, and the command to run.
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

// Where every command below runs. Stated once, because getting this wrong is the most
// likely reason a pasted command fails: these are node commands, not host commands, and
// pgbackrest/barman-cloud both read the postgres user's own configuration.
function WhereToRun({ who }) {
  return (
    <div className="rounded-lg bg-surface2 px-3 py-2 text-[11px] leading-snug text-muted">
      <span className="font-medium text-fg">Run these on the node</span>, not on your machine: right-click
      {who ? ` ${who}` : ' the node'} on the canvas → <span className="font-medium">Enter root console</span> (or
      use <span className="font-mono">dbcanvas node exec</span>). They run as root and step down to
      <span className="font-mono"> postgres</span> with <span className="font-mono">runuser</span>, because that is
      the user whose configuration and S3 credentials the tools read.
    </div>
  )
}

// --- pgBackRest (standalone PostgreSQL and Patroni) -------------------------------------

export function PgBackRestGuide({ cfg, patroni, nodeLabel }) {
  const stanza = cfg.backupStanza || cfg.cluster || nodeLabel || 'main'
  const pg = (c) => `runuser -u postgres -- ${c}`
  const svc = cfg.service || 'postgresql'
  return (
    <div className="space-y-4">
      <WhereToRun who={patroni ? 'any member' : nodeLabel} />

      <Section
        title="Look at what is there"
        hint={<>Every command takes the stanza, which for this {patroni ? 'cluster' : 'node'} is <span className="font-mono">{stanza}</span>. <span className="font-mono">info</span> is the list: each backup with its type, timestamp, size and the WAL range it needs.</>}
      >
        <Step n="1" title="List the backups" cmd={pg(`pgbackrest --stanza=${stanza} info`)}>
          Add <span className="font-mono">--output=json</span> to parse it. A stanza with no backups yet prints
          “no valid backups”.
        </Step>
        <Step n="2" title="Check that archiving works" cmd={pg(`pgbackrest --stanza=${stanza} check`)}>
          Verifies the repository is reachable and that PostgreSQL can actually archive a WAL segment to it.
          This is the command that tells you a backup <em>would</em> work, before you need one.
        </Step>
        <Step n="3" title="Is WAL archiving keeping up?" cmd={pg(`psql -x -c 'SELECT * FROM pg_stat_archiver'`)}>
          <span className="font-mono">failed_count</span> climbing, or <span className="font-mono">last_failed_wal</span> set,
          means archiving is broken even though backups may still succeed.
        </Step>
      </Section>

      <Section title="Take a backup" hint="The button above takes a full backup. These are the same thing by hand, plus the cheaper kinds.">
        <Step n="4" title="Full backup" cmd={pg(`pgbackrest --stanza=${stanza} --type=full backup`)}>
          A complete copy. Everything else is measured against it.
        </Step>
        <Step n="5" title="Incremental (or differential)" cmd={pg(`pgbackrest --stanza=${stanza} --type=incr backup`)}>
          Only what changed since the last backup — <span className="font-mono">--type=diff</span> for “since the last
          full”. Both need that full backup to still exist.
        </Step>
      </Section>

      <Section
        title="Delete a backup"
        hint={<>pgBackRest deletes by <em>expiring</em>, and it will refuse to orphan anything that depends on what you are removing — an incremental is expired with its full.</>}
      >
        <Step n="6" title="Delete one backup set" cmd={pg(`pgbackrest --stanza=${stanza} expire --set=<backup label>`)}>
          The label is the first column of <span className="font-mono">info</span>, e.g.
          <span className="font-mono"> 20260911-134257F</span>. A <span className="font-mono">…F</span> label is a full
          backup and takes its dependent incrementals with it.
        </Step>
        <Step n="7" title="Or keep only the last N fulls" cmd={pg(`pgbackrest --stanza=${stanza} --repo1-retention-full=2 expire`)}>
          The retention policy, applied once. Put <span className="font-mono">repo1-retention-full</span> in
          <span className="font-mono"> /etc/pgbackrest/pgbackrest.conf</span> to have every backup enforce it.
        </Step>
      </Section>

      {patroni ? (
        <Section
          title="Restore"
          hint="On a Patroni cluster, Patroni owns PostgreSQL — start and stop it through patronictl, never systemctl, or Patroni will simply put it back."
        >
          <Step n="8" title="Rebuild one member from the backup" cmd={`patronictl -c /etc/patroni/postgresql.yml reinit ${cfg.cluster || '<cluster>'} <member>`}>
            The usual repair: throws away that member's data directory and re-creates it from the pgBackRest
            repository (this cluster's <span className="font-mono">create_replica</span> method), then lets it
            stream from the leader. The rest of the cluster stays up.
          </Step>
          <Step n="9" title="Point-in-time recovery of the whole cluster" danger
            cmd={`# 1. on every member\npatronictl -c /etc/patroni/postgresql.yml pause ${cfg.cluster || '<cluster>'}\nsystemctl stop patroni\n\n# 2. on the member that will become the new leader\n${pg(`pgbackrest --stanza=${stanza} --delta --type=time --target="2026-09-11 13:40:00" restore`)}\n\n# 3. start it, let it finish recovery, then resume\nsystemctl start patroni\npatronictl -c /etc/patroni/postgresql.yml resume ${cfg.cluster || '<cluster>'}\n\n# 4. rebuild the others\npatronictl -c /etc/patroni/postgresql.yml reinit ${cfg.cluster || '<cluster>'} <each other member>`}>
            This rewinds the whole cluster to a moment in the past and discards everything after it. Pausing
            first is what stops Patroni from failing over mid-restore. The other members must be reinitialised
            afterwards — their timeline no longer matches.
          </Step>
        </Section>
      ) : (
        <Section title="Restore" hint="A restore overwrites the data directory, so PostgreSQL has to be stopped for it.">
          <Step n="8" title="Restore the latest backup" danger
            cmd={`systemctl stop ${svc}\n${pg(`pgbackrest --stanza=${stanza} --delta restore`)}\nsystemctl start ${svc}`}>
            <span className="font-mono">--delta</span> only replaces the files that differ, which is much faster than
            emptying <span className="font-mono">{cfg.dataDir || 'the data directory'}</span> first. Everything written
            since that backup is gone.
          </Step>
          <Step n="9" title="Recover to a point in time" danger
            cmd={`systemctl stop ${svc}\n${pg(`pgbackrest --stanza=${stanza} --delta --type=time --target="2026-09-11 13:40:00" restore`)}\nsystemctl start ${svc}`}>
            Replays the archived WAL up to that timestamp and stops — the way to undo a bad migration or a
            DELETE without a WHERE. Watch the log on start-up: recovery only reaches the target if the WAL
            between the backup and that moment is all in the repository.
          </Step>
        </Section>
      )}
    </div>
  )
}

// --- barman-cloud (repmgr) --------------------------------------------------------------

export function BarmanGuide({ cfg, nodeLabel }) {
  const server = cfg.backupServer || cfg.cluster || 'repmgr'
  const s3 = cfg.backupS3Url || `s3://${cfg.backupBucket || '<bucket>'}/barman/${server}`
  const ep = cfg.backupEndpoint || '<endpoint>'
  // Every barman-cloud command takes the same three positional-ish arguments; naming them
  // once keeps the commands short enough to read.
  const args = `--cloud-provider aws-s3 --endpoint-url ${ep} ${s3} ${server}`
  const pg = (c) => `runuser -u postgres -- ${c}`
  const svc = cfg.service || 'postgresql'
  return (
    <div className="space-y-4">
      <WhereToRun who="the primary" />

      <Section
        title="Look at what is there"
        hint={<>barman-cloud is a client, not a server — there is no daemon to ask, so every command names the store: endpoint <span className="font-mono">{ep}</span>, bucket <span className="font-mono">{cfg.backupBucket || '—'}</span>, server <span className="font-mono">{server}</span>.</>}
      >
        <Step n="1" title="List the backups" cmd={pg(`barman-cloud-backup-list ${args}`)}>
          The backup id is the first column (e.g. <span className="font-mono">20260911T134257</span>) — every other
          command takes it.
        </Step>
        <Step n="2" title="Is WAL archiving keeping up?" cmd={pg(`psql -x -c 'SELECT * FROM pg_stat_archiver'`)}>
          WAL reaches the store through <span className="font-mono">archive_command</span> on the primary, not through
          a backup. <span className="font-mono">failed_count</span> climbing means the base backups on their own will
          not restore to a recent point.
        </Step>
        <Step n="3" title="Read the archiver's own failure" cmd={`journalctl -u ${svc} -n 50 --no-pager | grep -i archive`}>
          An archive_command failure is logged with the reason — credentials, endpoint, or TLS.
        </Step>
      </Section>

      <Section title="Take a backup" hint="The button above runs exactly this on the current primary.">
        <Step n="4" title="Base backup" cmd={pg(`barman-cloud-backup ${args}`)}>
          Run it on the primary. Add <span className="font-mono">-z</span> to compress, or
          <span className="font-mono"> --immediate-checkpoint</span> to stop it waiting for a spread checkpoint.
        </Step>
      </Section>

      <Section title="Delete a backup" hint="By id, or by a retention policy applied on the spot.">
        <Step n="5" title="Delete one backup" cmd={pg(`barman-cloud-backup-delete ${args} --backup-id <id>`)}>
          The id comes from the list above.
        </Step>
        <Step n="6" title="Or apply a retention policy" cmd={pg(`barman-cloud-backup-delete ${args} --retention-policy "RECOVERY WINDOW OF 7 DAYS"`)}>
          Also accepts <span className="font-mono">"REDUNDANCY 2"</span>. Both delete the WAL the removed backups
          needed, which is where the space actually is.
        </Step>
      </Section>

      <Section title="Restore" hint="barman-cloud-restore only puts the files back; PostgreSQL does the recovery on start-up, and it needs to be told how to fetch WAL.">
        <Step n="7" title="Restore into an empty data directory" danger
          cmd={`systemctl stop ${svc}\nrm -rf ${cfg.dataDir || '<data dir>'}\n${pg(`barman-cloud-restore ${args} <backup id> ${cfg.dataDir || '<data dir>'}`)}`}>
          The target directory must not exist or must be empty — barman-cloud-restore will not write into a
          populated one. On a standby you are usually better off re-cloning with
          <span className="font-mono"> repmgr standby clone --force</span> instead.
        </Step>
        <Step n="8" title="Tell PostgreSQL how to fetch WAL, then start it" danger
          cmd={`cat >> ${cfg.dataDir || '<data dir>'}/postgresql.auto.conf <<'EOF'\nrestore_command = 'barman-cloud-wal-restore ${args} %f %p'\nrecovery_target_time = '2026-09-11 13:40:00'\nrecovery_target_action = 'promote'\nEOF\nrunuser -u postgres -- touch ${cfg.dataDir || '<data dir>'}/recovery.signal\nsystemctl start ${svc}`}>
          Drop the two <span className="font-mono">recovery_target_*</span> lines to recover as far as the WAL goes
          instead of to a moment. After it promotes, the other members are on a dead timeline — re-register them
          with <span className="font-mono">repmgr standby clone --force</span> then
          <span className="font-mono"> repmgr standby register --force</span>.
        </Step>
      </Section>
    </div>
  )
}

export default function BackupGuide({ engine, cfg = {}, nodeLabel, patroni }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="rounded-lg border">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-2.5 py-2 text-left text-xs font-semibold hover:bg-surface2"
      >
        <Icon.Chevron size={14} className={open ? 'transition' : '-rotate-90 transition'} />
        How to back up and restore — the commands
        <span className="ml-auto text-[11px] font-normal text-muted">{open ? 'hide' : 'show'}</span>
      </button>
      {open && (
        <div className="border-t p-2.5">
          {engine === 'barman'
            ? <BarmanGuide cfg={cfg} nodeLabel={nodeLabel} />
            : <PgBackRestGuide cfg={cfg} nodeLabel={nodeLabel} patroni={patroni} />}
        </div>
      )}
    </div>
  )
}
