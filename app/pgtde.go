package main

import (
	"context"
	"fmt"
	"strings"
)

// pgtde.go — data-at-rest encryption for the standalone PostgreSQL node (pg), keyed by an OpenBao
// node in the same stack. It is dbvault.go's third engine, and the odd one of the three.
//
// MySQL and MongoDB read their keyring out of a config file the server parses at startup, so
// DBCanvas writes a file and restarts. **pg_tde has no such file.** A key provider is registered
// by calling a SQL function against a running server:
//
//	SELECT pg_tde_add_global_key_provider_vault_v2('openbao', <url>, <mount>, <token file>, <ca>);
//	SELECT pg_tde_create_key_using_global_key_provider('<node>-principal', 'openbao');
//	SELECT pg_tde_set_default_key_using_global_key_provider('<node>-principal', 'openbao');
//
// so this step runs *after* PostgreSQL is up and its superuser password is set, not before it
// starts. Three consequences shape the whole file:
//
//   - **The token is a file, not a string.** pg_tde takes a path (its `secret_token_path`) and
//     reads it as the postgres user, so the token is written to disk at pgTDETokenFile with 0600
//     and postgres ownership rather than passed in the CR/config the way the other engines do.
//   - **pg_tde needs shared memory**, so `shared_preload_libraries = 'pg_tde'` goes into
//     postgresql.conf and the server is restarted once before any of the SQL above can run.
//   - **The extension is per database.** It is created in `postgres` *and* in `template1`, which
//     is what makes every database made afterwards inherit it — including the ones DBCanvas's own
//     Data Generator creates.
//
// Percona builds pg_tde for PostgreSQL 17 and 18 only (docs.percona.com/pg-tde), and from PPG
// 17.7 it stopped being bundled with the server: it is a package of its own,
// percona-pg_tde<major> on RHEL and percona-pg-tde-<major> on Debian. Up to 17.6 that package
// does not exist because the extension is already inside the server package — so the install
// step below treats a missing package as normal and checks for pg_tde.control instead, which is
// the thing that actually has to be true either way.

// pgTDEMajors are the PostgreSQL majors Percona builds pg_tde for.
var pgTDEMajors = []string{"17", "18"}

// pgTDEMajorOK reports whether a PostgreSQL major has pg_tde at all.
func pgTDEMajorOK(major string) bool {
	m := ppgMajorOf(major)
	for _, v := range pgTDEMajors {
		if m == v {
			return true
		}
	}
	return false
}

// pgTDEPackage is the separate pg_tde package for a major. It exists from PPG 17.7 onward; before
// that the extension ships inside the server package and this name is simply absent from the
// repository, which pgTDEInstall* handles rather than failing on.
func pgTDEPackage(nodeOS, major string) string {
	m := ppgMajorOf(major)
	if isDebianOS(nodeOS) {
		return "percona-pg-tde-" + m
	}
	return "percona-pg_tde" + m
}

const (
	// Where the OpenBao token lives on the node. pg_tde is handed this path, not the token
	// itself, and reads it as postgres — so the directory is postgres-owned and 0700, and the
	// file 0600. Named after the extension rather than after PostgreSQL: on Debian
	// /var/lib/postgresql is the server's own home and a keyring directory next to it reads
	// like part of the cluster.
	pgTDEDir       = "/etc/pg_tde"
	pgTDETokenFile = pgTDEDir + "/vault.token"
	// The key provider's name inside pg_tde. One per node, so it can be spelled the same way
	// everywhere and still be unambiguous — the *mount* is what keeps two nodes apart.
	pgTDEProvider = "openbao"
)

// pgTDEKeyName is the principal key's name inside the mount. It carries the hostname so that a
// mount someone later points two servers at still has two distinguishable keys — the mount is
// per-node today, and this is what keeps that from being load-bearing.
func pgTDEKeyName(host string) string { return host + "-principal" }

// applyPGVault gives a standalone PostgreSQL node encryption at rest keyed to the stack's OpenBao
// node, and returns what the node's panel should say about it.
//
// The OpenBao half is dbvault.go's, unchanged: the node gets its OWN KV v2 mount and a token
// scoped to a policy of the same name (never the root token), and verifies OpenBao with the
// Intranet CA already in its trust store. Only the client half is PostgreSQL's.
func (a *App) applyPGVault(ctx context.Context, st Stack, n designNode, containerID, host, confDir, service string, pr *pxcProg) (vaultInfo, error) {
	pr.phase("Configuring pg_tde (OpenBao)", 90)
	if !pgTDEMajorOK(n.PGMajor) {
		return vaultInfo{}, fmt.Errorf("pg_tde is built for PostgreSQL %s only, not %s",
			strings.Join(pgTDEMajors, "/"), ppgMajorOf(n.PGMajor))
	}
	cfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, n.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return vaultInfo{}, err
	}
	mount, kv, kvVersion := vaultMountFor(n, host)
	token, err := a.provisionVaultMount(ctx, baoCID, cfg, rootToken, mount, kv, "Percona Distribution for PostgreSQL", pr.logln)
	if err != nil {
		return vaultInfo{}, err
	}

	// The CA only when OpenBao actually serves TLS: pg_tde's ca_path is optional, and pointing it
	// at a file for a plaintext listener is noise at best.
	caFile := ""
	if cfg.TLS {
		caFile = caAnchorFor(n.OS)
	} else {
		pr.logln("pg_tde → " + cfg.Addr + " over plain HTTP: the OpenBao node has SSL off, so the principal key crosses the stack network unencrypted")
	}

	major := ppgMajorOf(n.PGMajor)
	install := pgTDEInstallRHEL
	if isDebianOS(n.OS) {
		install = pgTDEInstallDebian
	}
	if err := a.runStep(ctx, containerID, install, []string{
		"PRODUCT=" + ppgProduct(major), "TDEPKG=" + pgTDEPackage(n.OS, major), "BINDIR=" + pgBinDir(n.OS, major),
	}, pr.logln); err != nil {
		return vaultInfo{}, fmt.Errorf("install pg_tde: %w", err)
	}
	if err := a.runStep(ctx, containerID, pgTDEConfigureScript, []string{
		"CONFDIR=" + confDir, "SERVICE=" + service,
		"TDEDIR=" + pgTDEDir, "TOKENFILE=" + pgTDETokenFile, "TDETOKEN=" + token,
		"PROVIDER=" + pgTDEProvider, "KEYNAME=" + pgTDEKeyName(host),
		"VAULTURL=" + cfg.Addr, "VAULTMOUNT=" + mount, "CAFILE=" + caFile,
	}, pr.logln); err != nil {
		return vaultInfo{}, err
	}
	pr.logln("pg_tde keyed to " + cfg.Addr + " (KV v2 mount " + mount + "); tde_heap is the default table access method")
	return vaultInfo{
		Enabled: true, Method: "pg_tde global key provider (vault_v2)",
		Addr: cfg.Addr, OpenBao: cfg.FQDN, Mount: mount, KVVersion: kvVersion,
		SecretPath: mount + "/data/" + pgTDEKeyName(host),
		CACert:     caFile, TokenFile: pgTDETokenFile,
	}, nil
}

// pgTDEInstallRHEL installs the separate pg_tde package when the repository has one.
//
// A MISSING PACKAGE IS NOT AN ERROR. Up to PPG 17.6 pg_tde is inside percona-postgresql17-server
// and `percona-pg_tde17` does not exist; from 17.7 it is its own package and the server has none.
// Testing for the package would therefore be testing the wrong thing, so the step ends by testing
// for what has to be true in both worlds: pg_tde.control on disk, under this build's own sharedir.
//
// Not pin_install: VER is this node's PostgreSQL minor, and pg_tde carries its own version series,
// so pinning it would only produce a "no such build" note on every deploy.
// Env: PRODUCT (ppg-NN), TDEPKG, BINDIR.
const pgTDEInstallRHEL = `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
if [ -n "$(dnf -q repoquery "$TDEPKG" 2>/dev/null)" ]; then
  dnf -y -q install "$TDEPKG"
  echo "installed $TDEPKG"
else
  echo "note: $TDEPKG is not in this repository — up to 17.6 pg_tde ships inside the server package"
fi
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/pg_tde.control" ] || {
  echo "pg_tde.control not found under ${SHAREDIR:-<unknown sharedir>}/extension — this PostgreSQL build has no pg_tde"; exit 1; }
echo "pg_tde extension files present"`

const pgTDEInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1 || true
apt-get update -qq >/dev/null
if apt-cache show "$TDEPKG" >/dev/null 2>&1; then
  apt-get install -y -qq "$TDEPKG" >/dev/null
  echo "installed $TDEPKG"
else
  echo "note: $TDEPKG is not in this repository — up to 17.6 pg_tde ships inside the server package"
fi
SHAREDIR=$("$BINDIR/pg_config" --sharedir 2>/dev/null)
[ -n "$SHAREDIR" ] && [ -f "$SHAREDIR/extension/pg_tde.control" ] || {
  echo "pg_tde.control not found under ${SHAREDIR:-<unknown sharedir>}/extension — this PostgreSQL build has no pg_tde"; exit 1; }
echo "pg_tde extension files present"`

// pgTDEConfigureScript is everything after the package: the token file, the preload, the
// extension, the key provider, the principal key, and a real encrypted table to prove it works.
//
// Notes on the parts that are easy to get wrong:
//
//   - The SQL is fed to psql on **stdin** with -v variables, never with -c: psql expands :'var'
//     for stdin and files only, and interpolating a URL or a path into a command string by hand
//     is how a quote ends up inside a SQL literal.
//   - ca_path is optional in pg_tde, and the empty string is not the same as absent — hence
//     NULLIF(:'ca',”), which passes a real NULL when OpenBao is plaintext.
//   - Every step is written to survive a redeploy: the provider is changed rather than added when
//     one of that name is already registered, and an existing principal key is reused rather than
//     re-created (creating it again would fail, and rotating it on every deploy would be worse).
//   - default_table_access_method is set LAST, and only after pg_tde exists in template1. Percona
//     is explicit that a database without the extension cannot create a table once this is
//     global — so the extension has to be inheritable before the setting goes in.
//
// Env: CONFDIR, SERVICE, TDEDIR, TOKENFILE, TDETOKEN, PROVIDER, KEYNAME, VAULTURL, VAULTMOUNT, CAFILE.
const pgTDEConfigureScript = `set -e
CONF="$CONFDIR/postgresql.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }

# 1) The token file. pg_tde is given this PATH and reads it as the postgres user.
install -d -o postgres -g postgres -m 0700 "$TDEDIR"
printf '%s' "$TDETOKEN" > "$TOKENFILE"
chown postgres:postgres "$TOKENFILE"
chmod 0600 "$TOKENFILE"

# 2) pg_tde needs shared memory, so it loads at startup. The marker keeps a redeploy from
#    stacking a second copy of the line.
sed -i "/# dbcanvas-tde/d" "$CONF"
echo "shared_preload_libraries = 'pg_tde' # dbcanvas-tde" >> "$CONF"
systemctl restart "$SERVICE"
for i in $(seq 1 30); do runuser -u postgres -- psql -tAc 'select 1' >/dev/null 2>&1 && break; sleep 1; done
runuser -u postgres -- psql -tAc 'select 1' >/dev/null 2>&1 || {
  echo "PostgreSQL did not come back after loading pg_tde:"; journalctl -u "$SERVICE" --no-pager 2>/dev/null | tail -20; exit 1; }

# 3) The extension, in postgres and in template1 — template1 is what makes every database
#    created afterwards inherit it.
for DB in postgres template1; do
  printf '%s\n' "CREATE EXTENSION IF NOT EXISTS pg_tde;" \
    | runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 -d "$DB"
done

psql_tde() { runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 -d postgres \
  -v prov="$PROVIDER" -v url="$VAULTURL" -v mnt="$VAULTMOUNT" -v tok="$TOKENFILE" -v ca="$CAFILE" -v key="$KEYNAME"; }

# 4) The global key provider. Registering it validates the mount against OpenBao, so a bad
#    token or a KV v1 mount fails here rather than at the first encrypted write.
if ! printf '%s\n' "SELECT pg_tde_add_global_key_provider_vault_v2(:'prov', :'url', :'mnt', :'tok', NULLIF(:'ca',''));" | psql_tde >/dev/null 2>&1; then
  printf '%s\n' "SELECT pg_tde_change_global_key_provider_vault_v2(:'prov', :'url', :'mnt', :'tok', NULLIF(:'ca',''));" | psql_tde
  echo "key provider $PROVIDER already registered — updated it"
else
  echo "key provider $PROVIDER registered against $VAULTURL (mount $VAULTMOUNT)"
fi

# 5) The principal key. Creating one that already exists in the mount is an error, and a
#    redeploy must not rotate the key the existing data is encrypted with — so a failure here
#    means "already there" and the next step just points the server at it.
printf '%s\n' "SELECT pg_tde_create_key_using_global_key_provider(:'key', :'prov');" | psql_tde >/dev/null 2>&1 \
  || echo "principal key $KEYNAME already exists in $VAULTMOUNT — reusing it"
printf '%s\n' "SELECT pg_tde_set_default_key_using_global_key_provider(:'key', :'prov');" | psql_tde

# 6) Prove it end to end before claiming the node is encrypted: a real tde_heap table, written
#    through the TDE storage manager with a key fetched from OpenBao.
ENC=$(printf '%s\n' \
  "DROP TABLE IF EXISTS dbcanvas_tde_check;" \
  "CREATE TABLE dbcanvas_tde_check (id int) USING tde_heap;" \
  "INSERT INTO dbcanvas_tde_check VALUES (1);" \
  "SELECT pg_tde_is_encrypted('dbcanvas_tde_check');" \
  | runuser -u postgres -- psql -tA -v ON_ERROR_STOP=1 -d postgres | tail -1)
printf '%s\n' "DROP TABLE IF EXISTS dbcanvas_tde_check;" | runuser -u postgres -- psql -q -d postgres >/dev/null 2>&1 || true
[ "$ENC" = "t" ] || { echo "pg_tde is configured but a tde_heap table did not come back encrypted (pg_tde_is_encrypted returned '$ENC')"; exit 1; }

# 7) Encrypt by default. Deliberately last: Percona is explicit that a database WITHOUT the
#    extension cannot create a table once this is set cluster-wide, so it only goes in after
#    template1 has pg_tde and every new database inherits it. A database made from template0
#    still needs CREATE EXTENSION pg_tde before it can create a table.
sed -i "/# dbcanvas-tde-default/d" "$CONF"
echo "default_table_access_method = 'tde_heap' # dbcanvas-tde-default" >> "$CONF"
systemctl reload "$SERVICE" 2>/dev/null || systemctl restart "$SERVICE"
echo "pg_tde active: default principal key $KEYNAME, tde_heap is the default table access method"`
