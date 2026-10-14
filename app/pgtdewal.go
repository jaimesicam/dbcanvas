package main

import "strings"

// pgtdewal.go — WAL encryption for pg_tde (pg_tde.wal_encrypt), on the standalone node and on
// Patroni, and what it changes about archiving and restoring.
//
// Table encryption leaves the WAL in the clear: a row written to an encrypted table is readable
// in the WAL that carries it, and in the archive pgBackRest keeps. pg_tde.wal_encrypt closes that,
// and Percona supports it on one condition (docs.percona.com/pg-tde, command-line-tools):
//
//   - archive_command goes through pg_tde_archive_decrypt, which decrypts each segment to a file
//     in /dev/shm and runs the real archive command on that — pgBackRest stores WAL it can read
//     (verify, expire, restore to another server) and the archive is protected by the store;
//   - restore_command goes through pg_tde_restore_encrypt, which fetches the plain segment and
//     encrypts it into pg_wal;
//   - pgBackRest's archive-async, archive-header-check and checksum-page are off — it cannot
//     check headers or pages it is not the first reader of.
//
// One more thing that is not in those pages, from Percona's forum and confirmed in the restore
// path here: `pgbackrest restore` writes ITS OWN restore_command into postgresql.auto.conf,
// which would replay archived (plain) WAL into an encrypting server unwrapped. Every restore
// therefore passes the wrapped command with --recovery-option (pgRestoreTDEArgs).
//
// %f and %p appear twice in each command: PostgreSQL expands the outer pair, and the wrapper the
// inner one — which postgresql.conf (and Patroni, which writes postgresql.conf) therefore needs
// escaped as %%f / %%p. The temporary path in /dev/shm has no spaces, so the inner command needs
// no quoting of its own.

// pgTDEArchiveCommand is archive_command with WAL encryption, as postgresql.conf takes it. The
// wrapper is named by its full path: PostgreSQL runs archive_command through /bin/sh with the
// server's environment, whose PATH does not have the server's bin directory — the first deploy
// archived nothing, "pg_tde_archive_decrypt: command not found".
func pgTDEArchiveCommand(bindir, stanza string) string {
	return bindir + `/pg_tde_archive_decrypt %f %p "pgbackrest --stanza=` + stanza + ` archive-push %%p"`
}

// pgTDERestoreCommand is restore_command with WAL encryption, as postgresql.conf takes it.
func pgTDERestoreCommand(bindir, stanza string) string {
	return bindir + `/pg_tde_restore_encrypt %f %p "pgbackrest --stanza=` + stanza + ` archive-get %%f %%p"`
}

// pgTDEBackRestOptions are the stanza options pgBackRest needs to archive through the wrapper.
var pgTDEBackRestOptions = []string{"archive-async=n", "archive-header-check=n", "checksum-page=n"}

// pgRestoreTDEArgs is what a pgBackRest restore of a WAL-encrypting server adds: the wrapped
// restore_command, so the one pgBackRest would write does not win.
func pgRestoreTDEArgs(bindir, stanza string) []string {
	return []string{"--recovery-option=restore_command=" + pgTDERestoreCommand(bindir, stanza)}
}

// pgTDEWALIssues validates the WAL-encryption switch: it is part of pg_tde, so it needs
// encryption on.
func pgTDEWALIssues(label string, wal, vault bool) []issue {
	if wal && !vault {
		return []issue{{Level: "error", Message: label + " encrypts WAL, which is pg_tde — turn on data-at-rest encryption (OpenBao) as well"}}
	}
	return nil
}

// yamlQuote is a YAML single-quoted scalar: the commands above carry double quotes, and a single
// quote is doubled to escape it.
func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
