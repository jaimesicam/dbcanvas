package main

import (
	"context"
	"fmt"
	"strings"
)

// pgsecurity.go — pgAudit and set_user on Percona PostgreSQL (standalone and Patroni).
//
// The two are Percona Distribution for PostgreSQL's audit pair and come up together in
// compliance questions: pgAudit writes what was done to the server log, set_user records who
// became a superuser to do it (and, while they are one, blocks the ways of hiding it — ALTER
// SYSTEM, COPY PROGRAM, log_statement changes).
//
// Both are loaded the same way, so this file is a small generic path for a Percona extension:
// install the package the component table names (ppgcomponents.go), check its .control is there,
// add its library to the node's one shared_preload_libraries value, write its settings, and
// create it on the primary in postgres and template1. pg_tde does not go through here — it has a
// key provider to register after the server is up (pgtde.go) — but its library joins the same
// preload value (pgNodePreload).

// pgAuditDefaultLog is what pgAudit logs when the design does not say: schema changes and
// role/privilege changes. Enough to see it working without drowning a busy lab's log in reads.
const pgAuditDefaultLog = "ddl,role"

// pgExtension is one preloaded Percona extension.
type pgExtension struct {
	Comp    string      // ppgComponents ID: which package, and where it exists
	Name    string      // the extension (its .control file and CREATE EXTENSION name)
	Preload string      // the library for shared_preload_libraries
	Conf    [][2]string // postgresql.conf settings, unquoted
}

// pgNodeExtensions are a standalone node's; the Patroni frame's are pgFrameExtensions.
func pgNodeExtensions(n designNode) []pgExtension {
	return pgSecurityExtensions(n.PGAudit, n.PGAuditLog, n.PGSetUser)
}

func pgFrameExtensions(f designFrame) []pgExtension {
	return pgSecurityExtensions(f.PGAudit, f.PGAuditLog, f.PGSetUser)
}

// pgExtNames is "pgaudit, set_user", for progress phases.
func pgExtNames(exts []pgExtension) string {
	var out []string
	for _, e := range exts {
		out = append(out, e.Name)
	}
	return strings.Join(out, ", ")
}

// pgSecurityExtensions are the extensions a pg node or Patroni frame asked for.
func pgSecurityExtensions(audit bool, auditLog string, setUser bool) []pgExtension {
	var out []pgExtension
	if audit {
		out = append(out, pgExtension{
			Comp: "pgaudit", Name: "pgaudit", Preload: "pgaudit",
			Conf: [][2]string{
				{"pgaudit.log", pgAuditLogOr(auditLog)},
				// Statements against pg_catalog are psql's own \d and every client's
				// introspection; with them on, ddl,role is mostly noise about the tool.
				{"pgaudit.log_catalog", "off"},
				{"pgaudit.log_relation", "on"},
			},
		})
	}
	if setUser {
		// set_user's own defaults are the hardening (block_alter_system, block_copy_program
		// and block_log_statement all on while escalated), so nothing to set.
		out = append(out, pgExtension{Comp: "set_user", Name: "set_user", Preload: "set_user"})
	}
	return out
}

func pgAuditLogOr(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return pgAuditDefaultLog
}

// pgAuditClasses are the values pgaudit.log accepts, each optionally prefixed with "-" to
// subtract it (e.g. "all,-misc").
var pgAuditClasses = []string{"read", "write", "function", "role", "ddl", "misc", "misc_set", "all", "none"}

// pgAuditLogIssue reports what is wrong with a pgaudit.log value, or "". A bad class is a
// server that refuses to start (pgaudit rejects the setting at preload), so it is caught here.
func pgAuditLogIssue(v string) string {
	for _, c := range strings.Split(pgAuditLogOr(v), ",") {
		c = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c)), "-")
		if !containsStr(pgAuditClasses, c) {
			return fmt.Sprintf("pgAudit log class %q is not one of %s", c, strings.Join(pgAuditClasses, ", "))
		}
	}
	return ""
}

// pgExtensionIssues validates the extensions a node asked for against its OS and version.
func pgExtensionIssues(label, nodeOS, major, minor string, audit bool, auditLog string, setUser bool) []issue {
	var out []issue
	for _, e := range pgSecurityExtensions(audit, auditLog, setUser) {
		c, _ := ppgComponentByID(e.Comp)
		if ok, why := c.Available(nodeOS, major, minor); !ok {
			out = append(out, issue{Level: "error", Message: label + " asks for " + e.Name + " — " + why})
		}
	}
	if audit {
		if why := pgAuditLogIssue(auditLog); why != "" {
			out = append(out, issue{Level: "error", Message: label + ": " + why})
		}
	}
	return out
}

// pgExtPreload is the extensions' libraries, in order, for pgPreloadValue's `own` list.
func pgExtPreload(exts []pgExtension) []string {
	var out []string
	for _, e := range exts {
		if e.Preload != "" {
			out = append(out, e.Preload)
		}
	}
	return out
}

// pgExtConfLines renders the extensions' settings as postgresql.conf lines.
func pgExtConfLines(exts []pgExtension) []string {
	var out []string
	for _, e := range exts {
		for _, kv := range e.Conf {
			out = append(out, kv[0]+" = '"+kv[1]+"'")
		}
	}
	return out
}

// pgExtParams is the same settings for Patroni's postgresql.parameters.
func pgExtParams(exts []pgExtension) [][2]string {
	var out [][2]string
	for _, e := range exts {
		out = append(out, e.Conf...)
	}
	return out
}

// installPGExtensions installs each extension's package, pinned to the node's minor so the
// install cannot pull a newer server in behind it, then checks its .control exists — the thing
// that has to be true for CREATE EXTENSION, whatever the package was called.
func (a *App) installPGExtensions(ctx context.Context, containerID, nodeOS, major, minor string, exts []pgExtension, logln func(string)) error {
	if len(exts) == 0 {
		return nil
	}
	var pkgs, names []string
	for _, e := range exts {
		c, _ := ppgComponentByID(e.Comp)
		pkgs = append(pkgs, c.Package(nodeOS, major))
		names = append(names, e.Name)
	}
	script := pgExtInstallRHEL
	if isDebianOS(nodeOS) {
		script = pgExtInstallDebian
	}
	env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " "), "VER=" + minor,
		"BINDIR=" + pgBinDir(nodeOS, major), "EXTS=" + strings.Join(names, " ")}
	if err := a.runStep(ctx, containerID, script, env, logln); err != nil {
		return fmt.Errorf("install %s: %w", strings.Join(names, ", "), err)
	}
	logln("installed " + strings.Join(pkgs, " "))
	return nil
}

// createPGExtensions creates the extensions in postgres and template1 on a primary; a standby
// is skipped, it gets them by replication. psql is the binary to use ("" → PATH).
func (a *App) createPGExtensions(ctx context.Context, containerID, psql string, exts []pgExtension, logln func(string)) error {
	if len(exts) == 0 {
		return nil
	}
	var names []string
	for _, e := range exts {
		names = append(names, e.Name)
	}
	env := []string{"EXTS=" + strings.Join(names, " ")}
	if psql != "" {
		env = append(env, "PSQL="+psql)
	}
	if err := a.runStep(ctx, containerID, pgExtCreateScript, env, logln); err != nil {
		return fmt.Errorf("create %s: %w", strings.Join(names, ", "), err)
	}
	logln(strings.Join(names, ", ") + " created in postgres and template1")
	return nil
}

// Env: PRODUCT, PKGS, VER, BINDIR, EXTS.
const pgExtCheck = `SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
for e in $EXTS; do
  [ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/$e.control" ] || {
    echo "$e.control not found under ${SHAREDIR:-<unknown sharedir>}/extension after installing $PKGS"; exit 1; }
done`

const pgExtInstallRHEL = pinInstallRHEL + `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
pin_install $PKGS
` + pgExtCheck

const pgExtInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
apt-get update -qq >/dev/null
pin_install $PKGS
` + pgExtCheck

// Env: EXTS, PSQL.
const pgExtCreateScript = `set -e
PSQL="${PSQL:-psql}"
if [ "$(runuser -u postgres -- "$PSQL" -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "t" ]; then
  echo "standby — $EXTS arrive by replication"; exit 0
fi
for DB in postgres template1; do
  for e in $EXTS; do
    printf 'CREATE EXTENSION IF NOT EXISTS %s;\n' "$e" | runuser -u postgres -- "$PSQL" -q -v ON_ERROR_STOP=1 -d "$DB"
  done
done`
