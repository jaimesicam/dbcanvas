package main

import (
	"context"
	"fmt"
	"strings"
)

// ------------------------------------------------- pgvector (vector similarity search)
//
// pgvector adds a `vector` column type, the distance operators <-> (L2), <=> (cosine) and
// <#> (negative inner product), and two approximate-nearest-neighbour index methods, HNSW
// and IVFFlat. It is a plain extension: unlike pg_stat_statements or pg_tde it allocates no
// shared memory at startup, so it is NOT a shared_preload_libraries entry — nothing about
// how the server starts changes, and a member only needs the files on disk.
//
// Where the files come from depends on whose PostgreSQL the shape runs:
//
//   - the standalone node and Patroni run Percona Distribution for PostgreSQL, so it is
//     Percona's package from the same ppg-<major> repository (percona-pgvector_<major> on
//     EL, percona-postgresql-<major>-pgvector on Debian/Ubuntu);
//   - repmgr runs PGDG's PostgreSQL, so it is PGDG's package (pgvector_<major> /
//     postgresql-<major>-pgvector) — a Percona build would be linked against the wrong server;
//   - Spock runs a patched PostgreSQL compiled from source into its own prefix, which no
//     package was built for, so pgvector is compiled against that tree too, at a pinned tag;
//   - the Percona Operator for PostgreSQL carries pgvector in its images already, and
//     spec.extensions.builtin.pgvector is all it takes (see pgVectorK3DMinVer).
//
// The extension is then created in two databases on the primary: `postgres`, which is where
// a person lands with psql, and `template1`, which every later CREATE DATABASE copies — so an
// application that creates its own database afterwards finds `vector` already there instead
// of failing on its first CREATE TABLE with a vector column. Standbys get both by streaming.

const (
	// pgVectorRef is the pgvector release the Spock source build checks out. The packaged
	// shapes get whatever their repository carries for the major, which is this release
	// or newer at the time of writing.
	pgVectorRef = "v0.8.6"
	// pgVectorK3DMinVer is the first Percona Operator for PostgreSQL whose CRD has
	// spec.extensions.builtin.pgvector. The 3.x operators keep accepting that spelling and
	// map it onto their newer extensions.pgvector.enabled, so one form serves every
	// version from here on.
	pgVectorK3DMinVer = "2.6.0"
)

// pgVectorPackage is pgvector's package for a node's OS and major. flavor is "ppg" for
// Percona's repository (the standalone node and Patroni) and "pgdg" for PGDG's (repmgr).
func pgVectorPackage(nodeOS, major, flavor string) string {
	m := ppgMajorOf(major)
	switch {
	case flavor == "pgdg" && isDebianOS(nodeOS):
		return "postgresql-" + m + "-pgvector"
	case flavor == "pgdg":
		return "pgvector_" + m
	case isDebianOS(nodeOS):
		return "percona-postgresql-" + m + "-pgvector"
	default:
		return "percona-pgvector_" + m
	}
}

// pgVectorSource is what the deployment config records, so the manager panel can say where
// this member's pgvector came from — the package name, or the tag Spock compiled.
func pgVectorSource(frameType, nodeOS, major string) string {
	switch frameType {
	case "spock":
		return "pgvector " + pgVectorRef + " (built from source)"
	case "repmgr":
		return pgVectorPackage(nodeOS, major, "pgdg") + " (PGDG)"
	}
	return pgVectorPackage(nodeOS, major, "ppg") + " (Percona)"
}

// pgHasPGVector reports whether a Percona Operator for PostgreSQL version understands
// spec.extensions.builtin.pgvector. An empty version is "unknown", not "newest", for
// pgHasClusterFeatures's reason: guessing wrong costs the whole cr.yaml.
func pgHasPGVector(operatorVer string) bool {
	v := strings.TrimSpace(operatorVer)
	return v != "" && compareVersions(v, pgVectorK3DMinVer) >= 0
}

// k3dPgVectorIssues validates a K3D frame's pgvector option.
//
// The version gate is an error for the reason k3dPGFeatureIssues gives at length: below 2.6.0
// the CRD has no spec.extensions.builtin, so the API server rejects the entire custom resource
// and no cluster is created at all. `deployed` turns that into a warning for a frame that is
// already running — the deploy skips it, so nothing would re-apply its cr.yaml anyway.
func k3dPgVectorIssues(f designFrame, opCat OperatorCatalog, deployed bool) []issue {
	if f.Type != "k3d" || !f.K3DPgVector {
		return nil
	}
	lvl, tail := "error", ""
	if deployed {
		lvl, tail = "warning", " (this cluster is already running, so nothing here is re-applied — destroy the frame to change it)"
	}
	name := f.Label
	switch f.K3DOperator {
	case "pg":
	case "cnpg", "pgo":
		// Their images carry pgvector already; DBCanvas only creates the extension
		// (cnpg.go, k3dpgo.go). No operator version gate: every release offered here has
		// the bootstrap SQL hook.
		return nil
	default:
		return []issue{{Level: "warning", Message: "K3D cluster " + name + " has pgvector on, which only the " +
			"PostgreSQL operators (Percona, CloudNativePG, Crunchy PGO) provide — it is ignored for " + orDefault(k3dOperatorLabel(f.K3DOperator), "a cluster with no operator")}}
	}
	ver, ok := opCat.resolveOperatorVersion("pg", f.K3DOperatorVer)
	switch {
	case !ok:
		return []issue{{Level: lvl, Message: "K3D cluster " + name + " asks for pgvector, which needs a known " +
			"operator version to check against " + pgVectorK3DMinVer + " — pick one from the list, or run `make versions`" + tail}}
	case !pgHasPGVector(ver):
		return []issue{{Level: lvl, Message: "K3D cluster " + name + " asks for pgvector, which the Percona " +
			"Operator for PostgreSQL only has from " + pgVectorK3DMinVer + " — this frame pins " + ver + ", whose CRD has no " +
			"`spec.extensions.builtin`, so it would reject the whole cr.yaml and create no cluster at all. Choose " +
			pgVectorK3DMinVer + " or newer, or turn pgvector off" + tail}}
	}
	return nil
}

// installPGVector installs pgvector's package from the repository the node's PostgreSQL came
// from. product is the Percona repository to enable first ("ppg-17"), or "" for PGDG, whose
// repository the repmgr install has already configured.
func (a *App) installPGVector(ctx context.Context, containerID, nodeOS, major, flavor string, logln func(string)) error {
	script := pgVectorInstallRHEL
	if isDebianOS(nodeOS) {
		script = pgVectorInstallDebian
	}
	pkg := pgVectorPackage(nodeOS, major, flavor)
	env := []string{"PKG=" + pkg, "BINDIR=" + pgBinDir(nodeOS, major)}
	if flavor == "ppg" {
		env = append(env, "PRODUCT="+ppgProduct(major))
	}
	if err := a.runStep(ctx, containerID, script, env, logln); err != nil {
		return fmt.Errorf("install pgvector: %w", err)
	}
	logln("installed " + pkg)
	return nil
}

// buildPGVector compiles pgvector against the Spock frame's source-built PostgreSQL in prefix.
func (a *App) buildPGVector(ctx context.Context, containerID, prefix string, logln func(string)) error {
	if err := a.runStep(ctx, containerID, pgVectorBuildScript, []string{"PREFIX=" + prefix, "PGVECTOR_REF=" + pgVectorRef}, logln); err != nil {
		return fmt.Errorf("build pgvector %s: %w", pgVectorRef, err)
	}
	logln("pgvector " + pgVectorRef + " compiled + installed to " + prefix)
	return nil
}

// enablePGVector creates the extension in postgres and template1 on a primary (a standby is
// skipped: it is read-only and gets both by replication), then logs the version it got.
// psql is the psql binary to use ("" → the one on PATH).
func (a *App) enablePGVector(ctx context.Context, containerID, psql string, logln func(string)) error {
	var env []string
	if psql != "" {
		env = append(env, "PSQL="+psql)
	}
	if err := a.runStep(ctx, containerID, pgVectorEnableScript, env, logln); err != nil {
		return fmt.Errorf("enable pgvector: %w", err)
	}
	// runStep keeps a script's output only when it fails, so the version is asked for
	// separately — it is the one fact worth having in the deploy log.
	if res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pgVectorVersionScript}, env); err == nil && res.Code == 0 {
		if out := strings.TrimSpace(res.Stdout); out != "" {
			logln(out)
		}
	}
	return nil
}

// pgVectorInstall{RHEL,Debian} install the package and prove the extension files landed in
// the server's own sharedir — a package for another major would install cleanly and leave
// CREATE EXTENSION failing later with a far less useful message. Env: PKG, BINDIR, and
// PRODUCT for Percona's repository.
const pgVectorInstallRHEL = `set -e
[ -z "$PRODUCT" ] || percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
dnf -y -q install "$PKG" >/dev/null 2>&1 || yum -y -q install "$PKG" >/dev/null
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/vector.control" ] || {
  echo "vector.control not found under ${SHAREDIR:-<unknown sharedir>}/extension after installing $PKG"; exit 1; }
echo "installed $PKG"`

const pgVectorInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
[ -z "$PRODUCT" ] || percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
apt-get install -y -qq "$PKG" >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq "$PKG" >/dev/null; }
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/vector.control" ] || {
  echo "vector.control not found under ${SHAREDIR:-<unknown sharedir>}/extension after installing $PKG"; exit 1; }
echo "installed $PKG"`

// pgVectorBuildScript compiles pgvector against $PREFIX/bin/pg_config, the way
// spockCompileScript builds Spock (failures print the tail of the log rather than all of it).
// The build host is the node itself, so pgvector's default -march=native is the right CPU.
// Idempotent like the Spock build: a redeploy that already has it installed is a no-op.
// Env: PREFIX, PGVECTOR_REF.
const pgVectorBuildScript = `set -e
SHAREDIR=$("$PREFIX/bin/pg_config" --sharedir)
if [ -f "$SHAREDIR/extension/vector.control" ] && [ -f "$PREFIX/lib/vector.so" ]; then echo "pgvector already built at $PREFIX"; exit 0; fi
rm -rf /usr/src/pgvector
git clone --depth 1 --branch "$PGVECTOR_REF" https://github.com/pgvector/pgvector /usr/src/pgvector >/tmp/pgvector-clone.log 2>&1 || { echo "clone pgvector ($PGVECTOR_REF) failed:"; tail -6 /tmp/pgvector-clone.log; exit 1; }
cd /usr/src/pgvector
make with_llvm=no PG_CONFIG="$PREFIX/bin/pg_config" >/tmp/pgvector-build.log 2>&1 || { echo "pgvector build failed:"; grep -iE "error:|fatal" /tmp/pgvector-build.log | head -8; exit 1; }
make with_llvm=no PG_CONFIG="$PREFIX/bin/pg_config" install >>/tmp/pgvector-build.log 2>&1 || { echo "pgvector install failed:"; tail -10 /tmp/pgvector-build.log; exit 1; }
[ -f "$SHAREDIR/extension/vector.control" ] || { echo "vector.control not installed under $SHAREDIR/extension"; exit 1; }
echo "pgvector $PGVECTOR_REF installed to $PREFIX"`

// pgVectorEnableScript creates the extension in postgres and template1, on a primary only.
// IF NOT EXISTS makes it safe to run again on a redeploy. Env: PSQL when the binary is not
// on PATH (the Spock source build).
const pgVectorEnableScript = `set -e
PSQL="${PSQL:-psql}"
if [ "$(runuser -u postgres -- "$PSQL" -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "t" ]; then
  echo "pgvector: standby — the extension arrives by replication"; exit 0
fi
for DB in postgres template1; do
  printf '%s\n' "CREATE EXTENSION IF NOT EXISTS vector;" | runuser -u postgres -- "$PSQL" -q -v ON_ERROR_STOP=1 -d "$DB"
done
echo "pgvector enabled in postgres and template1"`

// pgVectorVersionScript reports what enablePGVector created. Env: PSQL, as above.
const pgVectorVersionScript = `PSQL="${PSQL:-psql}"
v=$(runuser -u postgres -- "$PSQL" -tAc "SELECT extversion FROM pg_extension WHERE extname='vector'" -d template1 2>/dev/null)
[ -n "$v" ] && echo "pgvector $v enabled in postgres and template1"`
