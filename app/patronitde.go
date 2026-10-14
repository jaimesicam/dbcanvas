package main

import (
	"context"
	"fmt"
)

// patronitde.go — pg_tde on a Patroni cluster, keyed to the stack's OpenBao node.
//
// The standalone node's pg_tde (pgtde.go) is one server with one key. A Patroni cluster is
// several servers that must share one, and that inverts the rule the MySQL-family clusters
// follow (one mount per server, mysqlVaultMount):
//
//   - Members are physical copies. A replica's encrypted relation files are the leader's, byte
//     for byte, and so are their internal keys — which only the principal key the leader used
//     can unwrap. A member with a key of its own could not read a single page it was given.
//   - The key provider's settings live in the data directory (pg_tde's own catalog), so they
//     replicate too: every member asks the same OpenBao mount, with a token read from the same
//     path. Percona's replication guide says exactly that — identical KMS configuration and file
//     paths on primary and standby.
//
// So: one KV mount and one scoped token per cluster, written to the same file on every member;
// pg_tde installed on every member (the build each server loads, pgTDEInstall*); pg_tde in the
// cluster's preload from bootstrap (patroniYAML); and, once there is a leader, the extension,
// the provider and the principal key created there, and tde_heap made the default through
// Patroni's own configuration — not ALTER SYSTEM, which only the leader would have, and a
// member promoted later would not.
//
// And Patroni runs pg_tde's rewind and base backup in place of the stock ones (bin_name in
// patroniYAML). This was found the hard way: with plain pg_rewind, a leader killed mid-write
// rejoined after failover, reported streaming with zero lag — and failed every read of an
// encrypted index with "page verification failed". pg_rewind does not carry pg_tde's per-relation
// key map with the pages it copies.
//
// WAL is not encrypted (pg_tde.wal_encrypt off), as on the standalone node: encrypted WAL also
// needs pg_tde_archive_decrypt in front of pgBackRest's archiving, which this does not set up.

// patroniTDE is what the members and the leader step need from the frame-level setup.
type patroniTDE struct {
	cfg   openbaoConfig
	mount string
	token string
	ca    string // CA file on the members, "" when OpenBao is plain HTTP
}

// patroniTDEMount is the cluster's KV mount: one per cluster, named after it.
func patroniTDEMount(f designFrame) string { return "postgresql-" + sanitizeName(f.Label) }

// patroniTDEKeyName is the cluster's principal key, named after the cluster rather than a host —
// every member uses it.
func patroniTDEKeyName(f designFrame) string { return sanitizeName(f.Label) + "-principal" }

// patroniTDEPrepare waits for OpenBao and gives the cluster its mount and token.
func (a *App) patroniTDEPrepare(ctx context.Context, st Stack, f designFrame, logln func(string)) (*patroniTDE, error) {
	if ok, why := pgTDEComponent().Available(f.OS, f.PGMajor, f.PGVersion); !ok {
		return nil, fmt.Errorf("%s", why)
	}
	cfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, f.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return nil, err
	}
	mount := patroniTDEMount(f)
	token, err := a.provisionVaultMount(ctx, baoCID, cfg, rootToken, mount, "kv-v2", "Percona Distribution for PostgreSQL (Patroni)", logln)
	if err != nil {
		return nil, err
	}
	t := &patroniTDE{cfg: cfg, mount: mount, token: token}
	if cfg.TLS {
		t.ca = caAnchorFor(f.OS)
	} else {
		logln("pg_tde → " + cfg.Addr + " over plain HTTP: the OpenBao node has SSL off, so the principal key crosses the stack network unencrypted")
	}
	return t, nil
}

// patroniTDEMember puts pg_tde and the token file on one member, before Patroni starts it.
func (a *App) patroniTDEMember(ctx context.Context, containerID string, f designFrame, t *patroniTDE, logln func(string)) error {
	major := ppgMajorOf(f.PGMajor)
	install := pgTDEInstallRHEL
	if isDebianOS(f.OS) {
		install = pgTDEInstallDebian
	}
	if err := a.runStep(ctx, containerID, install, []string{
		"PRODUCT=" + ppgProduct(major), "TDEPKG=" + pgTDEPackage(f.OS, major), "BINDIR=" + pgBinDir(f.OS, major),
	}, logln); err != nil {
		return fmt.Errorf("install pg_tde: %w", err)
	}
	return a.runStep(ctx, containerID, pgTDETokenScript, []string{
		"TDEDIR=" + pgTDEDir, "TOKENFILE=" + pgTDETokenFile, "TDETOKEN=" + t.token,
	}, logln)
}

// patroniTDEEnable keys the running cluster, on its leader, and returns what the panel shows.
func (a *App) patroniTDEEnable(ctx context.Context, leaderCID string, f designFrame, t *patroniTDE, logln func(string)) (vaultInfo, error) {
	major := ppgMajorOf(f.PGMajor)
	key := patroniTDEKeyName(f)
	if err := a.runStep(ctx, leaderCID, patroniTDEEnableScript, []string{
		"PSQL=" + pgBinDir(f.OS, major) + "/psql", "CONF=/etc/patroni/postgresql.yml",
		"PROVIDER=" + pgTDEProvider, "KEYNAME=" + key, "TOKENFILE=" + pgTDETokenFile,
		"VAULTURL=" + t.cfg.Addr, "VAULTMOUNT=" + t.mount, "CAFILE=" + t.ca,
	}, logln); err != nil {
		return vaultInfo{}, err
	}
	logln("pg_tde keyed to " + t.cfg.Addr + " (KV v2 mount " + t.mount + ", one key for the cluster); tde_heap is the default table access method on every member")
	return vaultInfo{
		Enabled: true, Method: "pg_tde global key provider (vault_v2), one principal key for the cluster",
		Addr: t.cfg.Addr, OpenBao: t.cfg.FQDN, Mount: t.mount, KVVersion: "2",
		SecretPath: t.mount + "/data/" + key, CACert: t.ca, TokenFile: pgTDETokenFile,
	}, nil
}

// pgTDETokenScript writes the OpenBao token where pg_tde reads it, as the postgres user.
// Env: TDEDIR, TOKENFILE, TDETOKEN.
const pgTDETokenScript = `set -e
install -d -o postgres -g postgres -m 0700 "$TDEDIR"
printf '%s' "$TDETOKEN" > "$TOKENFILE"
chown postgres:postgres "$TOKENFILE"
chmod 0600 "$TOKENFILE"
echo "OpenBao token written to $TOKENFILE"`

// patroniTDEEnableScript is pgTDEConfigureScript's steps 3–7 for a Patroni leader: pg_tde is
// already preloaded (bootstrap DCS), and the default access method goes through patronictl so
// every member — and any member promoted later — has it.
// Env: PSQL, CONF, PROVIDER, KEYNAME, TOKENFILE, VAULTURL, VAULTMOUNT, CAFILE.
const patroniTDEEnableScript = `set -e
P() { runuser -u postgres -- "$PSQL" -h /var/run/postgresql -q -v ON_ERROR_STOP=1 "$@"; }
for DB in postgres template1; do
  printf '%s\n' "CREATE EXTENSION IF NOT EXISTS pg_tde;" | P -d "$DB"
done
psql_tde() { P -d postgres -v prov="$PROVIDER" -v url="$VAULTURL" -v mnt="$VAULTMOUNT" -v tok="$TOKENFILE" -v ca="$CAFILE" -v key="$KEYNAME"; }
if ! printf '%s\n' "SELECT pg_tde_add_global_key_provider_vault_v2(:'prov', :'url', :'mnt', :'tok', NULLIF(:'ca',''));" | psql_tde >/dev/null 2>&1; then
  printf '%s\n' "SELECT pg_tde_change_global_key_provider_vault_v2(:'prov', :'url', :'mnt', :'tok', NULLIF(:'ca',''));" | psql_tde
  echo "key provider $PROVIDER already registered — updated it"
else
  echo "key provider $PROVIDER registered against $VAULTURL (mount $VAULTMOUNT)"
fi
printf '%s\n' "SELECT pg_tde_create_key_using_global_key_provider(:'key', :'prov');" | psql_tde >/dev/null 2>&1 \
  || echo "principal key $KEYNAME already exists in $VAULTMOUNT — reusing it"
printf '%s\n' "SELECT pg_tde_set_default_key_using_global_key_provider(:'key', :'prov');" | psql_tde
ENC=$(printf '%s\n' \
  "DROP TABLE IF EXISTS dbcanvas_tde_check;" \
  "CREATE TABLE dbcanvas_tde_check (id int) USING tde_heap;" \
  "INSERT INTO dbcanvas_tde_check VALUES (1);" \
  "SELECT pg_tde_is_encrypted('dbcanvas_tde_check');" \
  | runuser -u postgres -- "$PSQL" -h /var/run/postgresql -tA -v ON_ERROR_STOP=1 -d postgres | tail -1)
printf '%s\n' "DROP TABLE IF EXISTS dbcanvas_tde_check;" | P -d postgres >/dev/null 2>&1 || true
[ "$ENC" = "t" ] || { echo "pg_tde is configured but a tde_heap table did not come back encrypted ('$ENC')"; exit 1; }
patronictl -c "$CONF" edit-config --force -p default_table_access_method=tde_heap >/dev/null
for i in $(seq 1 30); do
  [ "$(P -d postgres -tA -c 'SHOW default_table_access_method')" = "tde_heap" ] && { echo "pg_tde active: principal key $KEYNAME, tde_heap the cluster default"; exit 0; }
  sleep 1
done
echo "patronictl set default_table_access_method, but the leader still reports $(P -d postgres -tA -c 'SHOW default_table_access_method')"; exit 1`
