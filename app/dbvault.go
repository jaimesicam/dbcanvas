package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// dbvault.go — data-at-rest encryption for the standalone Percona Server (ps), Percona Server
// for MongoDB (psm) and Percona Distribution for PostgreSQL (pg) nodes, keyed by an OpenBao node
// in the same stack (openbao.go).
//
// The client integrations differ by engine and version:
//
//   - Percona Server 8.4 → the **keyring_vault component**: a global manifest (mysqld.my) beside
//     the mysqld binary, and component_keyring_vault.cnf in plugin_dir (where the server resolves
//     `file://component_keyring_vault` — not beside mysqld). No my.cnf changes.
//   - Percona Server 5.7 / 8.0 → the **keyring_vault plugin** (the component does not exist
//     before 8.4): early-plugin-load=keyring_vault.so + keyring_vault_config=<conf> in my.cnf.
//   - PSMDB → mongod.conf `security.vault`. Encryption is written at the *first* mongod start,
//     so this is staged into mongod.conf before the server ever runs (mongoPrepareNode).
//   - PostgreSQL 17 / 18 → **pg_tde**, registered from SQL rather than from a config file:
//     pg_tde_add_global_key_provider_vault_v2() names the mount and a FILE holding the token.
//     See pgtde.go — it is the one engine here whose keyring is set up after the server is
//     already running, because registering a provider takes a psql connection.
//
// KV version follows what the client can actually speak: the 5.7 plugin predates KV v2, so that
// node gets a KV v1 mount; 8.0/8.4 and PSMDB get KV v2 (the only version PSMDB supports at all).
//
// Certificates: no new CA material is created or copied anywhere. There is exactly one CA in a
// stack — the Intranet CA — and every node already has it in its system trust store
// (catrust.go), so the keyring's vault_ca / mongod's serverCAFile just point at that file.
//
// Mounts: each database gets its OWN KV mount (mysql-<host> / mongodb-<host>) with a policy of
// the same name and a token bound to it. Percona is explicit that a secret_mount_point must be
// used by a single server — sharing one would corrupt keys — and it also keeps one node's master
// key unreadable to another. The generic mysql-v1/mysql-v2/mongodb-v2 mounts created with the
// OpenBao node stay as hand-rollable examples.

// intranetCAAnchor is where trustIntranetCA installs the stack CA on an RHEL-family node — the
// one CA in the stack, and the file every vault client here verifies OpenBao with.
const intranetCAAnchor = "/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt"

// intranetCADebian is the same file on a Debian/Ubuntu node.
const intranetCADebian = "/usr/local/share/ca-certificates/dbcanvas-ca.crt"

// caAnchorFor returns the trust-store path of the Intranet CA on a node of the given OS.
func caAnchorFor(nodeOS string) string {
	if isDebianOS(nodeOS) {
		return intranetCADebian
	}
	return intranetCAAnchor
}

const (
	// The MySQL keyring directory shipped by the Percona Server RPM (owned by mysql).
	mysqlKeyringDir  = "/var/lib/mysql-keyring"
	mysqlKeyringConf = mysqlKeyringDir + "/keyring_vault.conf"
	// PSMDB's token file, next to the certs dir the node already uses (/etc/mongo/certs).
	mongoVaultTokenFile = "/etc/mongo/vault.token"
)

// vaultInfo is persisted into a ps/psm node's Deployment.Config (key "vault") so the manager can
// show exactly how the node is encrypted. It never carries the token.
type vaultInfo struct {
	Enabled    bool   `json:"enabled"`
	Method     string `json:"method"`     // component_keyring_vault | keyring_vault plugin | security.vault
	Addr       string `json:"addr"`       // VAULT_ADDR of the OpenBao node
	OpenBao    string `json:"openbao"`    // OpenBao node FQDN
	Mount      string `json:"mount"`      // this node's own KV mount (= its policy name)
	KVVersion  string `json:"kvVersion"`  // 1 | 2
	SecretPath string `json:"secretPath"` // mongod's `secret` (PSMDB only)
	CACert     string `json:"caCert"`     // the Intranet CA in the node's trust store
	ConfFile   string `json:"confFile"`   // keyring conf / manifest location (MySQL only)
	TokenFile  string `json:"tokenFile"`  // PSMDB only (MySQL carries the token inside its conf)
}

// vaultIssues validates a ps/psm/pg node's OpenBao selection.
func vaultIssues(n designNode, openbaoIDs map[string]bool) []issue {
	if !n.EnableVault {
		return nil
	}
	label := nodeKindLabel(n.Type)
	var out []issue
	if !openbaoIDs[n.OpenBaoNodeID] {
		out = append(out, issue{Level: "error", Message: label + " node " + n.Label + " has data-at-rest encryption enabled but is not linked to an OpenBao node — add an OpenBao node and select it"})
	}
	// PostgreSQL's encryption is pg_tde, and pg_tde exists for exactly two majors. This is an
	// error rather than a silent no-op because the package simply is not in the repository for
	// the others — the deploy would install PostgreSQL, fail to find pg_tde and stop.
	if n.Type == "pg" && !pgTDEMajorOK(n.PGMajor) {
		out = append(out, issue{Level: "error", Message: label + " node " + n.Label + " has data-at-rest encryption enabled, which is pg_tde — Percona builds it for PostgreSQL " +
			strings.Join(pgTDEMajors, " and ") + " only, and this node is on " + ppgMajorOf(n.PGMajor) + ". Change the PostgreSQL major, or turn encryption off"})
	}
	return out
}

// vaultFrameIssues validates a cluster frame's OpenBao selection — the PXC and Percona Server
// replication frames, whose members are encrypted as a unit (each with its own mount; see
// mysqlVaultMount).
func vaultFrameIssues(f designFrame, openbaoIDs map[string]bool) []issue {
	if !f.EnableVault || !mysqlFamilyVaultFrame(f.Type) {
		return nil
	}
	if !openbaoIDs[f.OpenBaoNodeID] {
		return []issue{{Level: "error", Message: frameKindLabel(f.Type) + " " + f.Label +
			" has data-at-rest encryption enabled but is not linked to an OpenBao node — add an OpenBao node and select it"}}
	}
	return nil
}

// mysqlFamilyVaultFrame names the cluster frames that can be keyed to OpenBao. Percona builds
// keyring_vault for Percona Server and PXC; MariaDB's equivalent is a different plugin with a
// different key format, and the MySQL Community packages ship no vault keyring at all.
func mysqlFamilyVaultFrame(t string) bool { return t == "pxc" || t == "mysql" }

// frameKindLabel names a frame the way its form does, for validation messages.
func frameKindLabel(t string) string {
	switch t {
	case "pxc":
		return "PXC cluster"
	case "mysql":
		return "Percona Server replication cluster"
	}
	return t
}

// vaultMountFor returns the node's dedicated KV mount and the engine version to create it with.
// Only Percona Server 5.7 is stuck on KV v1: its keyring_vault plugin predates the v2 API.
func vaultMountFor(n designNode, host string) (mount, kv, version string) {
	if n.Type == "pg" {
		return "postgresql-" + host, "kv-v2", "2"
	}
	if n.Type == "psm" {
		return "mongodb-" + host, "kv-v2", "2"
	}
	return mysqlVaultMount(psMajorOf(n.PSMajor), host)
}

// mysqlVaultMount is the same answer for a MySQL-family server, keyed by its series rather than
// by a node type — a PXC member and a replication member carry no version of their own (it lives
// on the frame), and both need this.
//
// One mount per SERVER, never per cluster, and that is Percona's rule rather than a preference:
//
//	"Each secret_mount_point must serve only one Percona Server instance. Multiple servers that
//	 share a secret_mount_point write to the same Vault namespace" — with permanent key loss and
//	 cross-server key disclosure as the two named consequences.
//
// It holds for a cluster as much as for two unrelated servers: PXC members each keep their own
// master key (write-sets replicate data, not keys), and a joiner's SST re-encrypts the donor's
// tablespace keys with a master key it generates for itself.
func mysqlVaultMount(major, host string) (mount, kv, version string) {
	if psMajorOf(major) == "5.7" {
		return "mysql-" + host, "kv", "1"
	}
	return "mysql-" + host, "kv-v2", "2"
}

// waitOpenBaoReady blocks until the linked OpenBao node is running and returns what a client
// needs from it: its config (addr/TLS), the root token (to mint a scoped token) and its
// container id (the bao CLI runs there).
func (a *App) waitOpenBaoReady(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (openbaoConfig, string, string, error) {
	if nodeID == "" {
		return openbaoConfig{}, "", "", fmt.Errorf("no OpenBao node is selected")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return openbaoConfig{}, "", "", fmt.Errorf("the OpenBao node failed to provision")
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				var cfg openbaoConfig
				var sec openbaoSecrets
				json.Unmarshal(dep.Secrets, &sec)
				if json.Unmarshal(dep.Config, &cfg) == nil && cfg.Addr != "" && sec.RootToken != "" {
					return cfg, sec.RootToken, dep.ContainerID, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return openbaoConfig{}, "", "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return openbaoConfig{}, "", "", fmt.Errorf("timed out waiting for the OpenBao node to be ready")
}

// openbaoTokenScript mints a token bound to one policy and prints it (nothing else).
const openbaoTokenScript = `set -e
export BAO_TOKEN="$ROOT_TOKEN"
bao token create -policy="$POLICY" -period=768h -field=token`

// provisionVaultMount creates a database's own KV mount + policy on the OpenBao node and returns
// a token scoped to it. Runs on the OpenBao container (that is where bao and the CA live).
func (a *App) provisionVaultMount(ctx context.Context, baoCID string, cfg openbaoConfig, rootToken, mount, kv, engine string, logln func(string)) (string, error) {
	policy := openbaoPolicy(mount, kv, engine)
	if err := a.engCtx(ctx).CopyFile(ctx, baoCID, openbaoConfDir, "policy-"+mount+".hcl", 0o644, []byte(policy)); err != nil {
		return "", fmt.Errorf("write policy: %w", err)
	}
	env := append(baoClientEnv(cfg),
		"ROOT_TOKEN="+rootToken, "MOUNT="+mount, "KV="+kv,
		"POLICY_FILE="+fmt.Sprintf("%s/policy-%s.hcl", openbaoConfDir, mount))
	if err := a.runStep(ctx, baoCID, openbaoMountScript, env, logln); err != nil {
		return "", fmt.Errorf("create mount %s: %w", mount, err)
	}
	out, err := a.execScript(ctx, baoCID, openbaoTokenScript,
		append(baoClientEnv(cfg), "ROOT_TOKEN="+rootToken, "POLICY="+mount))
	if err != nil {
		return "", fmt.Errorf("mint token for %s: %w", mount, err)
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("OpenBao returned an empty token for policy %s", mount)
	}
	return token, nil
}

// ------------------------------------------------------------------ Percona Server (MySQL)

// mysqlKeyringPluginConf renders the keyring_vault *plugin* config (Percona Server 5.7 / 8.0).
// secret_mount_point_version only exists from 8.0 — 5.7 speaks KV v1 only, so it is omitted
// there (and the mount is created as KV v1 to match).
func mysqlKeyringPluginConf(addr, mount, token, caFile, kvVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "vault_url = %s\n", addr)
	fmt.Fprintf(&b, "secret_mount_point = %s\n", mount)
	fmt.Fprintf(&b, "token = %s\n", token)
	if caFile != "" {
		fmt.Fprintf(&b, "vault_ca = %s\n", caFile)
	}
	if kvVersion == "2" {
		b.WriteString("secret_mount_point_version = 2\n")
	}
	return b.String()
}

// mysqlKeyringComponentConf renders component_keyring_vault.cnf (Percona Server 8.4). The
// component autodetects the KV version with "AUTO".
func mysqlKeyringComponentConf(addr, mount, token, caFile string) string {
	m := map[string]any{
		"timeout":                    15,
		"vault_url":                  addr,
		"secret_mount_point":         mount,
		"secret_mount_point_version": "AUTO",
		"token":                      token,
	}
	if caFile != "" {
		m["vault_ca"] = caFile
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b) + "\n"
}

// ---------------------------------------------------------------- staging, before the first start

// The keyring is put in place BEFORE mysqld has ever run, and that is the whole shape of this
// half of the file.
//
// The obvious alternative — install the server, start it, then add the keyring and restart — is
// what this used to do, and it is wrong in three separate ways once a cluster is involved:
//
//   - **A restart is not free in PXC.** A member that restarts leaves the cluster and rejoins,
//     which at best is an IST and at worst is a full SST from a donor, in the middle of a deploy
//     that just finished building the cluster.
//   - **The keyring has to exist before the data does.** InnoDB writes the master key id into
//     every encrypted tablespace it creates; a server that initialises its data directory without
//     a keyring and gets one afterwards is a server whose earlier tablespaces cannot be encrypted
//     in place. Staging first is also what the PSMDB path does (mongod establishes encryption at
//     its first start and never again) — the reasons rhyme.
//   - **The options belong in the node's own config.** The old path appended `early-plugin-load`
//     to /etc/my.cnf, which is the right file on Oracle Linux and a file nothing reads on
//     Ubuntu — Debian-family packages split the config across `!includedir` drop-ins. Returning
//     the options to the caller instead means they land in whichever file mysqlWriteNodeCnf
//     writes, which is correct on both by construction.
//
// So prepareMySQLVault mints the mount and the token, stages the files, and hands back the lines
// the node's config must carry. verifyMySQLVault then checks — once the server is up — that the
// keyring is actually loaded, because a keyring that silently did not load is the one failure
// this whole feature exists to prevent.

// mysqlVaultPrep is what one server needs before it starts: its config lines, and what to record
// about the arrangement afterwards.
type mysqlVaultPrep struct {
	Options string    // extra [mysqld] lines for this node's own config file
	Info    vaultInfo // persisted to the deployment; drives the manager's Encryption tab
}

// mysqlVaultOptions renders the [mysqld] lines a node with a keyring needs.
//
// `default_table_encryption=ON` is here rather than left to the operator on purpose, and it is
// the same decision the PostgreSQL path already makes when it sets default_table_access_method to
// tde_heap: a node that was asked to encrypt its data should encrypt the tables somebody creates
// on it, without their having to remember `ENCRYPTION='Y'` on every CREATE TABLE. It is a
// per-schema default, so an explicit clause still wins either way.
//
// The plugin route additionally needs the two options that load it. They are `early-plugin-load`
// because the keyring must be up before InnoDB opens its first tablespace — a keyring loaded at
// the normal time is a keyring that arrives after the data it was needed for.
func mysqlVaultOptions(major string) string {
	var b strings.Builder
	if !mysqlModernMajor(psMajorOf(major)) {
		fmt.Fprintf(&b, "early-plugin-load=keyring_vault.so\n")
		fmt.Fprintf(&b, "keyring_vault_config=%s\n", mysqlKeyringConf)
	}
	b.WriteString("default_table_encryption=ON\n")
	return b.String()
}

// mysqlKeyringStagePluginScript writes the plugin's config file (Percona Server / PXC 5.7 and
// 8.0). The my.cnf options are NOT written here — see mysqlVaultOptions.
//
// 0600 and mysql-owned because the file carries the Vault token, and because the plugin refuses a
// config any other user can read.
//
// The plugin's own .so is checked for first, for the same reason the component's is: the config
// it is about to be given is loaded with `early-plugin-load`, and a missing early plugin is a
// server that will not start at all — a failure with this sentence in front of it is worth more
// than a mysqld that exits during bootstrap.
const mysqlKeyringStagePluginScript = `set -e
find /usr/lib64 /usr/lib -name keyring_vault.so -print -quit 2>/dev/null | grep -q . || {
  echo "keyring_vault.so is not installed on this node"; exit 1; }
install -d -o mysql -g mysql -m 0750 ` + mysqlKeyringDir + `
printf '%s' "$CONF" > ` + mysqlKeyringConf + `
chown mysql:mysql ` + mysqlKeyringConf + `
chmod 0600 ` + mysqlKeyringConf + `
echo "keyring_vault plugin config staged at ` + mysqlKeyringConf + ` (mount $MOUNT)"`

// mysqlKeyringStageComponentScript writes the manifest + component config (8.4 and up, where the
// plugin no longer exists).
//
// The two files do NOT live in the same place. The global manifest (mysqld.my) must sit beside the
// mysqld binary, and the component it names is then configured from **plugin_dir** — the server
// resolves `file://component_keyring_vault` there and reads
// <plugin_dir>/component_keyring_vault.cnf. Putting the .cnf next to mysqld instead leaves the
// component loaded-but-Disabled, and the first encrypted table then kills the server.
//
// plugin_dir cannot be read from a running server here — nothing is running yet — so it is asked
// of the *binary*: `mysqld --verbose --help` prints its compiled-in defaults without starting
// anything, and that is the same value the server will use at startup.
//
// The fallback underneath it is a search for the component's own .so, and its two exclusions are
// the reason this is not simply a search in the first place. A PXC node carries three copies of
// component_keyring_vault.so:
//
//	/usr/lib64/mysql/plugin/component_keyring_vault.so         <- the one the server loads
//	/usr/lib64/mysql/plugin/debug/component_keyring_vault.so   <- the debug build, shipped beside it
//	/usr/lib64/xtrabackup/plugin/component_keyring_vault.so    <- XtraBackup's, for reading a backup
//
// and `find … -print -quit` returns the debug one first. That is not a hypothetical: the config
// went to plugin/debug/, and the bootstrap died with "Keyring configuration doesn't exists:
// /usr/lib64/mysql/plugin//component_keyring_vault.cnf" — the server naming, in its own error,
// exactly the directory it expected.
//
// Either way the .so is checked for in the directory that was chosen, so a series whose packages
// do not ship it fails here, with a sentence, instead of at the first encrypted table.
const mysqlKeyringStageComponentScript = `set -e
BINDIR=$(dirname "$(readlink -f "$(command -v mysqld)")")
PLUGIN_DIR=$(mysqld --no-defaults --verbose --help 2>/dev/null | awk '$1=="plugin-dir"{print $2; exit}')
PLUGIN_DIR=${PLUGIN_DIR%/}
if [ -z "$PLUGIN_DIR" ] || [ ! -d "$PLUGIN_DIR" ]; then
  SO=$(find /usr/lib64 /usr/lib -name component_keyring_vault.so \
         ! -path '*/debug/*' ! -path '*xtrabackup*' -print -quit 2>/dev/null || true)
  [ -n "$SO" ] || { echo "component_keyring_vault.so is not installed on this node"; exit 1; }
  PLUGIN_DIR=$(dirname "$SO")
fi
[ -f "$PLUGIN_DIR/component_keyring_vault.so" ] || {
  echo "component_keyring_vault.so is not in $PLUGIN_DIR — this server has no vault keyring to load"; exit 1; }
printf '%s' "$CONF" > "$PLUGIN_DIR/component_keyring_vault.cnf"
chown mysql:mysql "$PLUGIN_DIR/component_keyring_vault.cnf"
chmod 0600 "$PLUGIN_DIR/component_keyring_vault.cnf"
printf '{ "components": "file://component_keyring_vault" }\n' > "$BINDIR/mysqld.my"
chmod 0644 "$BINDIR/mysqld.my"
echo "component_keyring_vault staged (manifest $BINDIR/mysqld.my, config $PLUGIN_DIR, mount $MOUNT)"`

// mysqlKeyringVerifyScript checks the keyring after the server is up, and — on the one node that
// can write — proves it end to end.
//
// The status query is not enough on its own. It says the component or plugin loaded, not that it
// can reach OpenBao with the token it was given: a wrong mount or an expired token loads fine and
// fails at the first key. So the writable member also creates an encrypted table, which is the
// operation that makes the server ask the keyring for a master key and store it. It carries a
// primary key because PXC's pxc_strict_mode=ENFORCING refuses a table without one, and it is
// dropped again immediately.
const mysqlKeyringVerifyScript = `set -e
export MYSQL_PWD="$ROOT_PW"
Q() { mysql -u root -N -B -e "$1" 2>/dev/null | tail -1; }
if [ "$MODE" = "component" ]; then
  STATUS=$(Q "SELECT STATUS_VALUE FROM performance_schema.keyring_component_status WHERE STATUS_KEY='Component_status'")
  [ "$STATUS" = "Active" ] || {
    echo "component_keyring_vault is not Active (status: ${STATUS:-not loaded}):"
    grep -iE 'keyring|component' "$LOGERR" 2>/dev/null | tail -5; exit 1; }
  echo "component_keyring_vault Active (mount $MOUNT)"
else
  STATUS=$(Q "SELECT PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_vault'")
  [ "$STATUS" = "ACTIVE" ] || {
    echo "the keyring_vault plugin is not ACTIVE (status: ${STATUS:-not loaded}):"
    grep -iE 'keyring|vault' "$LOGERR" 2>/dev/null | tail -10; exit 1; }
  echo "keyring_vault plugin ACTIVE (mount $MOUNT)"
fi
if [ "$SMOKE" = "1" ]; then
  mysql -u root -e "CREATE DATABASE IF NOT EXISTS dbcanvas_tde_check;
    CREATE TABLE dbcanvas_tde_check.probe (id INT PRIMARY KEY) ENCRYPTION='Y';
    DROP DATABASE dbcanvas_tde_check;" || {
    echo "the keyring loaded but could not store a master key — check the token, the mount and the OpenBao policy:"
    grep -iE 'keyring|vault' "$LOGERR" 2>/dev/null | tail -10; exit 1; }
  echo "wrote and dropped an encrypted table — the master key round-tripped through OpenBao"
fi`

// prepareMySQLVault gives one MySQL-family server its own mount, token and keyring files, before
// mysqld has started. The caller puts prep.Options into the node's config and, once the server is
// up, calls verifyMySQLVault.
func (a *App) prepareMySQLVault(ctx context.Context, st Stack, openbaoNodeID, nodeOS, major, host, containerID string, pr *pxcProg) (mysqlVaultPrep, error) {
	pr.phase("Configuring keyring (OpenBao)", 50)
	baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, openbaoNodeID, deployTimeout())
	if err != nil {
		return mysqlVaultPrep{}, err
	}
	mount, kv, kvVersion := mysqlVaultMount(major, host)
	token, err := a.provisionVaultMount(ctx, baoCID, baoCfg, rootToken, mount, kv, "Percona Server for MySQL", pr.logln)
	if err != nil {
		return mysqlVaultPrep{}, err
	}

	// The one CA in the stack, already in this node's trust store — nothing to copy. A
	// non-TLS OpenBao has no CA to verify at all.
	caFile := ""
	if baoCfg.TLS {
		caFile = caAnchorFor(nodeOS)
	}
	prep := mysqlVaultPrep{
		Options: mysqlVaultOptions(major),
		Info: vaultInfo{
			Enabled: true, Addr: baoCfg.Addr, OpenBao: baoCfg.FQDN,
			Mount: mount, KVVersion: kvVersion, CACert: caFile,
		},
	}
	script, conf := mysqlKeyringStagePluginScript, mysqlKeyringPluginConf(baoCfg.Addr, mount, token, caFile, kvVersion)
	prep.Info.Method, prep.Info.ConfFile = "keyring_vault plugin", mysqlKeyringConf
	if mysqlModernMajor(psMajorOf(major)) {
		script, conf = mysqlKeyringStageComponentScript, mysqlKeyringComponentConf(baoCfg.Addr, mount, token, caFile)
		prep.Info.Method, prep.Info.ConfFile = "component_keyring_vault", "component_keyring_vault.cnf (in plugin_dir)"
	}
	if err := a.runStep(ctx, containerID, script, []string{"CONF=" + conf, "MOUNT=" + mount}, pr.logln); err != nil {
		return mysqlVaultPrep{}, err
	}
	pr.logln("keyring: " + prep.Info.Method + " → " + baoCfg.Addr + " (mount " + mount + ", KV v" + kvVersion + ")")
	return prep, nil
}

// verifyMySQLVault confirms the keyring is live on a server that is now running. smoke asks for
// the end-to-end check (an encrypted table), which only a writable member can do — the primary of
// a replication pair, the node that bootstrapped a cluster.
func (a *App) verifyMySQLVault(ctx context.Context, containerID, nodeOS, major, mount, rootPW string, smoke bool, pr *pxcProg) error {
	mode := "plugin"
	if mysqlModernMajor(psMajorOf(major)) {
		mode = "component"
	}
	env := []string{"MODE=" + mode, "MOUNT=" + mount, "ROOT_PW=" + rootPW, "LOGERR=" + pxcLogError(nodeOS)}
	if smoke {
		env = append(env, "SMOKE=1")
	}
	return a.runStep(ctx, containerID, mysqlKeyringVerifyScript, env, pr.logln)
}

// ------------------------------------------------------------------------ PSMDB (MongoDB)

// mongoVault carries what mongoPrepareNode has to stage before mongod's first start: the
// security.vault block for mongod.conf, and the token file mongod reads. Encryption at rest is
// only established on an empty dbPath, so both must exist before mongod ever runs.
type mongoVault struct {
	Block string // rendered lines appended inside the mongod.conf `security:` block
	Token string // raw Vault token → mongoVaultTokenFile (mongod-only, 0600)
}

// mongoVaultBlock renders the mongod.conf security.vault settings. `secret` must be
// <mount>/data/<name> (KV v2), and PSMDB verifies the listener with serverCAFile — the Intranet
// CA already on the node. A non-TLS OpenBao needs disableTLSForTesting, which is exactly what it
// is: a testing shortcut.
func mongoVaultBlock(baoFQDN, secretPath, caFile string, tls bool) string {
	var b strings.Builder
	b.WriteString("  enableEncryption: true\n")
	b.WriteString("  vault:\n")
	fmt.Fprintf(&b, "    serverName: %s\n", baoFQDN)
	fmt.Fprintf(&b, "    port: %d\n", openbaoAPIPort)
	fmt.Fprintf(&b, "    secret: %s\n", secretPath)
	fmt.Fprintf(&b, "    tokenFile: %s\n", mongoVaultTokenFile)
	if tls {
		fmt.Fprintf(&b, "    serverCAFile: %s\n", caFile)
	} else {
		b.WriteString("    disableTLSForTesting: true\n")
	}
	return b.String()
}

// mongoVaultTokenScript writes the token file mongod reads. mongod refuses to start if the file
// is group/world readable, so it is 0600 and owned by mongod.
const mongoVaultTokenScript = `set -e
install -d -o mongod -g mongod -m 0755 /etc/mongo
printf '%s' "$TOKEN" > ` + mongoVaultTokenFile + `
chown mongod:mongod ` + mongoVaultTokenFile + `
chmod 0600 ` + mongoVaultTokenFile + `
echo "vault token staged at ` + mongoVaultTokenFile + `"`

// prepareMongoVault mints this PSMDB node's token + mount on the linked OpenBao node and returns
// what mongoPrepareNode must stage before the first mongod start, plus the vaultInfo to persist.
func (a *App) prepareMongoVault(ctx context.Context, st Stack, n designNode, host string, pr *pxcProg) (*mongoVault, vaultInfo, error) {
	baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, n.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return nil, vaultInfo{}, err
	}
	mount, kv, kvVersion := vaultMountFor(n, host)
	token, err := a.provisionVaultMount(ctx, baoCID, baoCfg, rootToken, mount, kv, "Percona Server for MongoDB", pr.logln)
	if err != nil {
		return nil, vaultInfo{}, err
	}
	caFile := ""
	if baoCfg.TLS {
		caFile = caAnchorFor(n.OS)
	}
	// KV v2 keeps data under <mount>/data/<name>; one key per node keeps two servers from
	// sharing (and clobbering) a master key.
	secretPath := fmt.Sprintf("%s/data/%s", mount, host)
	info := vaultInfo{
		Enabled: true, Method: "security.vault", Addr: baoCfg.Addr, OpenBao: baoCfg.FQDN,
		Mount: mount, KVVersion: kvVersion, SecretPath: secretPath,
		CACert: caFile, TokenFile: mongoVaultTokenFile,
	}
	mv := &mongoVault{
		Block: mongoVaultBlock(baoCfg.FQDN, secretPath, caFile, baoCfg.TLS),
		Token: token,
	}
	pr.logln("encryption at rest: security.vault → " + baoCfg.Addr + " (secret " + secretPath + ")")
	return mv, info, nil
}
