package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// dbvault.go — data-at-rest encryption for the standalone Percona Server (ps) and Percona Server
// for MongoDB (psm) nodes, keyed by an OpenBao node in the same stack (openbao.go).
//
// The three client integrations differ by engine and version:
//
//   - Percona Server 8.4 → the **keyring_vault component**: a global manifest (mysqld.my) beside
//     the mysqld binary, and component_keyring_vault.cnf in plugin_dir (where the server resolves
//     `file://component_keyring_vault` — not beside mysqld). No my.cnf changes.
//   - Percona Server 5.7 / 8.0 → the **keyring_vault plugin** (the component does not exist
//     before 8.4): early-plugin-load=keyring_vault.so + keyring_vault_config=<conf> in my.cnf.
//   - PSMDB → mongod.conf `security.vault`. Encryption is written at the *first* mongod start,
//     so this is staged into mongod.conf before the server ever runs (mongoPrepareNode).
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

// vaultIssues validates a ps/psm node's OpenBao selection.
func vaultIssues(n designNode, openbaoIDs map[string]bool) []issue {
	if !n.EnableVault {
		return nil
	}
	label := nodeKindLabel(n.Type)
	if !openbaoIDs[n.OpenBaoNodeID] {
		return []issue{{"error", label + " node " + n.Label + " has data-at-rest encryption enabled but is not linked to an OpenBao node — add an OpenBao node and select it"}}
	}
	return nil
}

// vaultMountFor returns the node's dedicated KV mount and the engine version to create it with.
// Only Percona Server 5.7 is stuck on KV v1: its keyring_vault plugin predates the v2 API.
func vaultMountFor(n designNode, host string) (mount, kv, version string) {
	if n.Type == "psm" {
		return "mongodb-" + host, "kv-v2", "2"
	}
	if psMajorOf(n.PSMajor) == "5.7" {
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
	if err := a.docker.CopyFile(ctx, baoCID, openbaoConfDir, "policy-"+mount+".hcl", 0o644, []byte(policy)); err != nil {
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

// mysqlKeyringPluginScript installs the plugin config, loads the plugin and verifies it is
// ACTIVE (Percona Server 5.7 / 8.0 — no keyring component exists before 8.4).
//
// The options go into /etc/my.cnf, NOT a my.cnf.d drop-in: Percona Server's packaged /etc/my.cnf
// has no `!includedir`, so a drop-in is silently never read (the same trap mysqlDirAuthScript
// documents). The keyring has to be up before InnoDB opens a tablespace, hence early-plugin-load.
// The config carries the Vault token, so it is mysql-only 0600 — the plugin refuses a config any
// other user can read.
const mysqlKeyringPluginScript = `set -e
install -d -o mysql -g mysql -m 0750 ` + mysqlKeyringDir + `
printf '%s' "$CONF" > ` + mysqlKeyringConf + `
chown mysql:mysql ` + mysqlKeyringConf + `
chmod 0600 ` + mysqlKeyringConf + `
CNF=/etc/my.cnf
sed -i "/# dbcanvas-keyring-begin/,/# dbcanvas-keyring-end/d" "$CNF"
cat >> "$CNF" <<EOF

# dbcanvas-keyring-begin
[mysqld]
early-plugin-load=keyring_vault.so
keyring_vault_config=` + mysqlKeyringConf + `
# dbcanvas-keyring-end
EOF
systemctl restart "$UNIT"
for i in $(seq 1 30); do mysqladmin ping >/dev/null 2>&1 && break; sleep 2; done
mysql -N -e "SELECT PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_vault'" | grep -q ACTIVE || {
  echo "keyring_vault plugin is not ACTIVE:"; grep -iE 'keyring|vault' /var/log/mysqld.log 2>/dev/null | tail -10; exit 1; }
echo "keyring_vault plugin ACTIVE (mount $MOUNT)"`

// mysqlKeyringComponentScript installs the manifest + component config and verifies the component
// is Active. Percona Server 8.4 only.
//
// The two files do NOT live in the same place. The global manifest (mysqld.my) must sit beside
// the mysqld binary, but the component reads its configuration from **plugin_dir** — the server
// resolves `file://component_keyring_vault` there, and looks for <plugin_dir>/component_keyring_vault.cnf.
// Putting the .cnf next to mysqld instead leaves the component loaded-but-Disabled, and the first
// encrypted table then kills the server.
const mysqlKeyringComponentScript = `set -e
BINDIR=$(dirname "$(readlink -f "$(command -v mysqld)")")
PLUGIN_DIR=$(mysql -N -e "SELECT @@plugin_dir" 2>/dev/null | tail -1)
[ -d "$PLUGIN_DIR" ] || { echo "cannot resolve plugin_dir"; exit 1; }
printf '%s' "$CONF" > "$PLUGIN_DIR/component_keyring_vault.cnf"
chown mysql:mysql "$PLUGIN_DIR/component_keyring_vault.cnf"
chmod 0600 "$PLUGIN_DIR/component_keyring_vault.cnf"
printf '{ "components": "file://component_keyring_vault" }\n' > "$BINDIR/mysqld.my"
chmod 0644 "$BINDIR/mysqld.my"
systemctl restart "$UNIT"
for i in $(seq 1 30); do mysqladmin ping >/dev/null 2>&1 && break; sleep 2; done
STATUS=$(mysql -N -e "SELECT STATUS_VALUE FROM performance_schema.keyring_component_status WHERE STATUS_KEY='Component_status'" 2>/dev/null | tail -1)
[ "$STATUS" = "Active" ] || {
  echo "component_keyring_vault is not Active (status: ${STATUS:-not loaded}):"
  grep -iE 'keyring|component' /var/log/mysqld.log 2>/dev/null | tail -5; exit 1; }
echo "component_keyring_vault Active (manifest $BINDIR/mysqld.my, config $PLUGIN_DIR, mount $MOUNT)"`

// applyMySQLVault wires a standalone Percona Server node to OpenBao as its keyring. 8.4 uses the
// keyring_vault component; 5.7 and 8.0 use the keyring_vault plugin (no component exists there).
// Returns the vaultInfo to persist. The node keeps running if this fails — the caller logs it.
func (a *App) applyMySQLVault(ctx context.Context, st Stack, n designNode, doc designDoc, containerID, host string, pr *pxcProg) (vaultInfo, error) {
	pr.phase("Configuring keyring (OpenBao)", 88)
	baoCfg, rootToken, baoCID, err := a.waitOpenBaoReady(ctx, st.ID, n.OpenBaoNodeID, deployTimeout())
	if err != nil {
		return vaultInfo{}, err
	}
	mount, kv, kvVersion := vaultMountFor(n, host)
	token, err := a.provisionVaultMount(ctx, baoCID, baoCfg, rootToken, mount, kv, "Percona Server for MySQL", pr.logln)
	if err != nil {
		return vaultInfo{}, err
	}

	// The one CA in the stack, already in this node's trust store — nothing to copy. A
	// non-TLS OpenBao has no CA to verify at all.
	caFile := ""
	if baoCfg.TLS {
		caFile = caAnchorFor(n.OS)
	}
	major := psMajorOf(n.PSMajor)
	unit := mysqlUnit(n.OS)

	info := vaultInfo{
		Enabled: true, Addr: baoCfg.Addr, OpenBao: baoCfg.FQDN,
		Mount: mount, KVVersion: kvVersion, CACert: caFile,
	}
	if major == "8.4" {
		info.Method = "component_keyring_vault"
		info.ConfFile = "component_keyring_vault.cnf (in plugin_dir)"
		conf := mysqlKeyringComponentConf(baoCfg.Addr, mount, token, caFile)
		if err := a.runStep(ctx, containerID, mysqlKeyringComponentScript,
			[]string{"CONF=" + conf, "UNIT=" + unit, "MOUNT=" + mount}, pr.logln); err != nil {
			return vaultInfo{}, err
		}
	} else {
		info.Method = "keyring_vault plugin"
		info.ConfFile = mysqlKeyringConf
		conf := mysqlKeyringPluginConf(baoCfg.Addr, mount, token, caFile, kvVersion)
		if err := a.runStep(ctx, containerID, mysqlKeyringPluginScript,
			[]string{"CONF=" + conf, "UNIT=" + unit, "MOUNT=" + mount}, pr.logln); err != nil {
			return vaultInfo{}, err
		}
	}
	pr.logln("keyring: " + info.Method + " → " + baoCfg.Addr + " (mount " + mount + ", KV v" + kvVersion + ")")
	return info, nil
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
