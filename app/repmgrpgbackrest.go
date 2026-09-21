package main

import "fmt"

// repmgrpgbackrest.go — pgBackRest as the repmgr frame's backup engine, beside Barman.
//
// A repmgr cluster could only ever back up with barman-cloud, which is an odd place for the
// choice to be made: every other PostgreSQL kind DBCanvas deploys (standalone, Patroni) uses
// pgBackRest, so the one cluster type whose whole subject is controlled failover was also the
// one where the backup tool was different from everything you would compare it against. The two
// are genuinely different tools worth having both of — this makes it a frame setting rather than
// a property of the cluster type.
//
// Almost nothing here is new code. `patroniPgBackRestConf`, `patroniPgBackRestDirsScript`,
// `patroniBackupScript` and `patroniStanza` are already shared between Patroni and standalone
// PostgreSQL — the "patroni" in their names is history, not scope — and a repmgr member is the
// same shape to pgBackRest as a standalone node: one PostgreSQL, one data directory, one stanza.
// So this file is the selector, the archive command, and the one thing that genuinely differs.
//
// ---------------------------------------------------------------- what genuinely differs
//
// **The package comes from a different repository.** Patroni installs Percona PostgreSQL and
// takes `percona-pgbackrest`; the repmgr frame is PGDG (repmgr is not in Percona's repo, which
// is why the whole frame uses PGDG), and PGDG's package is plain `pgbackrest`. Installing
// Percona's build on a PGDG node drags a second PostgreSQL in behind it.
//
// **TLS is not optional.** barman-cloud is boto3 and speaks plain HTTP, so a Barman repmgr
// cluster works against any SeaweedFS node. pgBackRest's S3 client is HTTPS-only, so the store
// must have S3 TLS on — the same rule the Patroni frame already has, enforced by the same
// pgBackRestSeaweedIssues.

// repmgrBackupEngine is which backup tool a repmgr frame was designed with: "barman",
// "pgbackrest", or "" for none.
//
// The two are mutually exclusive and validation says so, but this resolves rather than trusting
// that: a design saved with both — by hand, or by an older build that only had UseBarman — has
// to mean something rather than configure two archive commands into one postgresql.conf, where
// the second silently wins. pgBackRest is preferred because it is the newer, explicit choice; a
// frame that has both was a Barman frame somebody has since ticked pgBackRest on.
func repmgrBackupEngine(f designFrame) string {
	switch {
	case f.UsePgBackRest:
		return "pgbackrest"
	case f.UseBarman:
		return "barman"
	}
	return ""
}

// repmgrUsesBackups reports whether a frame backs up at all, whichever tool it picked.
func repmgrUsesBackups(f designFrame) bool { return repmgrBackupEngine(f) != "" }

// repmgrStanza is the pgBackRest stanza for a repmgr cluster. The same rule Patroni uses (the
// sanitized frame label), so a stack running both reads consistently and the Backup tab's
// commands are the ones the deploy actually ran.
func repmgrStanza(label string) string { return patroniStanza(label) }

// repmgrPgBackRestArchiveCommand is postgresql.conf's archive_command for a pgBackRest cluster.
//
// It goes into the *primary's* postgresql.conf, and the standbys inherit it: `repmgr standby
// clone` copies the primary's configuration, so after a switchover the newly promoted member is
// already archiving. That is why this is set once, on the primary, rather than per member.
func repmgrPgBackRestArchiveCommand(label string) string {
	return fmt.Sprintf("pgbackrest --stanza=%s archive-push %%p", repmgrStanza(label))
}

// repmgrPgBackRestInstallRHEL installs PGDG's pgbackrest. The PGDG repository is already
// configured by repmgrInstallRHEL, which runs first.
//
// Deliberately NOT percona-pgbackrest: that is the Percona repo's build, and on a node whose
// PostgreSQL came from PGDG its dependencies pull a second PostgreSQL in alongside the first.
// pin_install, so pgbackrest's own dependencies cannot pull this node's PostgreSQL up a minor —
// the same reason barmanInstallRHEL uses it.
const repmgrPgBackRestInstallRHEL = pinInstallRHEL + `set -e
pin_install pgbackrest >/dev/null
command -v pgbackrest >/dev/null || { echo "pgbackrest not on PATH after install"; exit 1; }
pgbackrest version`

// repmgrPgBackRestInstallDebian is the same from apt.postgresql.org, added by
// repmgrInstallDebian.
const repmgrPgBackRestInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
pin_install pgbackrest >/dev/null 2>&1 || { apt-get update -qq >/dev/null; pin_install pgbackrest >/dev/null; }
command -v pgbackrest >/dev/null || { echo "pgbackrest not on PATH after install"; exit 1; }
pgbackrest version`
