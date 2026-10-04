package main

import (
	"context"
	"fmt"
	"strings"
)

// ------------------------------------------------- query analytics extension
//
// PMM's Query Analytics reads PostgreSQL's statement statistics from one of two
// extensions, and neither is on by default: both need shared memory, so each has to
// be in shared_preload_libraries before the server starts, and then exist as an
// extension in the `postgres` database PMM connects to. Without one of them PMM
// registers the service, shows its metrics, and QAN stays empty — which is the
// lab nobody wanted.
//
// PGQuerySource picks which one, and the same value is what `pmm-admin add
// postgresql --query-source` takes:
//
//   - "pgstatements"  pg_stat_statements, from contrib. Every PostgreSQL build has it,
//     so it is the only choice on the repmgr (PGDG) and Spock (source build) frames.
//   - "pgstatmonitor" Percona's pg_stat_monitor, a separate package in the Percona
//     repository — so standalone PostgreSQL and Patroni only. It preloads alongside
//     pg_stat_statements, as Percona's PMM docs configure it.
//
// The empty value means neither, which is what every design before the field had.
// The settings follow docs.percona.com/percona-monitoring-and-management/3/
// install-pmm/install-pmm-client/connect-database/postgresql.html.

const (
	pgQSNone       = ""
	pgQSStatements = "pgstatements"
	pgQSMonitor    = "pgstatmonitor"
)

// pgQuerySourceOf normalises the design value. Anything unrecognised is returned
// as-is so pgQuerySourceIssues can name it.
func pgQuerySourceOf(spec string) string {
	switch s := strings.ToLower(strings.TrimSpace(spec)); s {
	case "", "none":
		return pgQSNone
	case "pgstatements", "pg_stat_statements":
		return pgQSStatements
	case "pgstatmonitor", "pg_stat_monitor":
		return pgQSMonitor
	default:
		return s
	}
}

// pgQuerySourceFor is the extension a deployment actually gets. On the standalone
// node and the Patroni frame the choice is a PMM option, so it only applies while a
// PMM node is selected; repmgr and Spock offer pg_stat_statements on its own.
func pgQuerySourceFor(frameType, spec, pmmNodeID string) string {
	qs := pgQuerySourceOf(spec)
	switch frameType {
	case "pg", "patroni":
		if pmmNodeID == "" {
			return pgQSNone
		}
		return qs
	case "repmgr", "spock":
		if qs == pgQSStatements {
			return qs
		}
		return pgQSNone
	}
	return pgQSNone
}

// pgQueryPreload is what goes into shared_preload_libraries for it, in order.
func pgQueryPreload(qs string) []string {
	switch qs {
	case pgQSStatements:
		return []string{"pg_stat_statements"}
	case pgQSMonitor:
		return []string{"pg_stat_statements", "pg_stat_monitor"}
	}
	return nil
}

// pgPreloadValue joins the frame's own libraries (repmgr, spock, pg_tde) with the
// query-analytics ones. One setting, so it has to be written once with all of them:
// a second shared_preload_libraries line would silently replace the first.
func pgPreloadValue(own []string, qs string) string {
	return strings.Join(append(append([]string{}, own...), pgQueryPreload(qs)...), ",")
}

// pgQueryParams are the postgresql.conf parameters the PMM docs set alongside the
// preload, as name → value (unquoted; callers quote for their file format).
func pgQueryParams(qs string) [][2]string {
	switch qs {
	case pgQSStatements:
		return [][2]string{
			{"track_activity_query_size", "2048"},
			{"pg_stat_statements.track", "all"},
			{"track_io_timing", "on"},
		}
	case pgQSMonitor:
		return [][2]string{
			{"track_activity_query_size", "2048"},
			{"track_io_timing", "on"},
			{"pg_stat_monitor.pgsm_query_max_len", "2048"},
			{"pg_stat_monitor.pgsm_normalized_query", "1"},
			// Query plans make PMM's timings wrong (the PMM docs say so); off.
			{"pg_stat_monitor.pgsm_enable_query_plan", "off"},
		}
	}
	return nil
}

// pgQueryConfLines renders pgQueryParams as postgresql.conf lines.
func pgQueryConfLines(qs string) []string {
	var out []string
	for _, p := range pgQueryParams(qs) {
		out = append(out, p[0]+" = '"+p[1]+"'")
	}
	return out
}

// pgQueryExtension is the extension created in the postgres database.
func pgQueryExtension(qs string) string {
	switch qs {
	case pgQSStatements:
		return "pg_stat_statements"
	case pgQSMonitor:
		return "pg_stat_monitor"
	}
	return ""
}

// pgQueryPMMFlags is what `pmm-admin add postgresql` is told about it. Nothing for
// the empty value, so a node without either extension registers exactly as before.
func pgQueryPMMFlags(qs string) string {
	if qs == pgQSNone {
		return ""
	}
	return "--query-source=" + qs
}

// pgStatMonitorPackage is pg_stat_monitor's package in the Percona repository.
func pgStatMonitorPackage(nodeOS, major string) string {
	m := ppgMajorOf(major)
	if isDebianOS(nodeOS) {
		return "percona-pg-stat-monitor" + m
	}
	return "percona-pg_stat_monitor" + m
}

// pgQuerySourceIssues validates the design value for a node or frame of the given type.
func pgQuerySourceIssues(who, frameType, spec string) []issue {
	qs := pgQuerySourceOf(spec)
	switch qs {
	case pgQSNone, pgQSStatements:
		return nil
	case pgQSMonitor:
		if frameType == "repmgr" || frameType == "spock" {
			return []issue{{Level: "error", Message: who + ": pg_stat_monitor is a Percona package and this cluster does not run Percona Distribution for PostgreSQL — use pg_stat_statements"}}
		}
		return nil
	}
	return []issue{{Level: "error", Message: who + ": unknown query analytics extension " + `"` + spec + `"` + " (pgstatements or pgstatmonitor)"}}
}

// installPGStatMonitor installs Percona's pg_stat_monitor package for the major.
func (a *App) installPGStatMonitor(ctx context.Context, containerID, nodeOS, major string, logln func(string)) error {
	script := pgStatMonitorInstallRHEL
	if isDebianOS(nodeOS) {
		script = pgStatMonitorInstallDebian
	}
	env := []string{"PRODUCT=" + ppgProduct(major), "PKG=" + pgStatMonitorPackage(nodeOS, major), "BINDIR=" + pgBinDir(nodeOS, major)}
	if err := a.runStep(ctx, containerID, script, env, logln); err != nil {
		return fmt.Errorf("install pg_stat_monitor: %w", err)
	}
	return nil
}

// enablePGQueryExtension creates the chosen extension in the postgres database once
// the server is running with it preloaded. psql is the psql binary to use ("" →
// the one on PATH).
func (a *App) enablePGQueryExtension(ctx context.Context, containerID, qs, psql string, logln func(string)) error {
	ext := pgQueryExtension(qs)
	if ext == "" {
		return nil
	}
	env := []string{"EXT=" + ext}
	if psql != "" {
		env = append(env, "PSQL="+psql)
	}
	if err := a.runStep(ctx, containerID, pgQueryExtensionScript, env, logln); err != nil {
		return fmt.Errorf("enable %s: %w", ext, err)
	}
	return nil
}

// pgStatMonitorInstall{RHEL,Debian} install pg_stat_monitor and prove the extension
// files landed, since a preload of a library that is not there stops the server.
// Env: PRODUCT, PKG, BINDIR.
const pgStatMonitorInstallRHEL = `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
dnf -y -q install "$PKG" >/dev/null 2>&1 || yum -y -q install "$PKG" >/dev/null
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/pg_stat_monitor.control" ] || {
  echo "pg_stat_monitor.control not found under ${SHAREDIR:-<unknown sharedir>}/extension after installing $PKG"; exit 1; }
echo "installed $PKG"`

const pgStatMonitorInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
apt-get install -y -qq "$PKG" >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq "$PKG" >/dev/null; }
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/pg_stat_monitor.control" ] || {
  echo "pg_stat_monitor.control not found under ${SHAREDIR:-<unknown sharedir>}/extension after installing $PKG"; exit 1; }
echo "installed $PKG"`

// pgQueryExtensionScript creates the extension in the postgres database, on a
// primary only (a standby is read-only and gets it by replication). Env: EXT, and
// PSQL when the binary is not on PATH (the Spock source build).
const pgQueryExtensionScript = `set -e
PSQL="${PSQL:-psql}"
if [ "$(runuser -u postgres -- "$PSQL" -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "t" ]; then
  echo "$EXT: standby — the extension arrives by replication"; exit 0
fi
printf '%s\n' "CREATE EXTENSION IF NOT EXISTS $EXT SCHEMA public;" | runuser -u postgres -- "$PSQL" -q -v ON_ERROR_STOP=1 -d postgres
runuser -u postgres -- "$PSQL" -tAc "SHOW shared_preload_libraries" -d postgres | grep -q "$EXT" || {
  echo "$EXT is not in shared_preload_libraries — the server was not restarted with it"; exit 1; }
echo "$EXT enabled in database postgres"`
