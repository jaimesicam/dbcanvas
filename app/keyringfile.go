package main

import (
	"context"
	"encoding/json"
	"strings"
)

// keyringfile.go — data-at-rest encryption with a keyring FILE on the node itself, for Percona
// Server (standalone and replication), PXC and MySQL Community (standalone and replication). It
// is the alternative to the OpenBao keyring in dbvault.go when no key server is wanted: the
// master key is a file next to the server, so it needs nothing else in the stack.
//
// Which keyring depends on the series, exactly as it does for the vault keyring:
//
//   - 8.4 / 9.7 → **component_keyring_file**. The keyring plugins are gone from 8.4, so it is the
//     only file keyring there. A global manifest (mysqld.my) beside the mysqld binary names the
//     component, and component_keyring_file.cnf in plugin_dir names the keyring file.
//   - 5.7 / 8.0 → the **keyring_file plugin**: early-plugin-load=keyring_file.so and
//     keyring_file_data=<file> in the node's config. 8.0 has the component as well, but the
//     plugin is what every 8.0 minor offered here ships and what PXC 8.0's SST handles
//     everywhere, and it keeps the 8.0 keyring the same shape as the vault one.
//
// Both are staged before mysqld first starts, for the reasons dbvault.go spells out: a keyring
// must exist before InnoDB creates the tablespaces it protects, and a PXC member restarted into a
// keyring leaves and rejoins the cluster. Every member of a cluster gets a keyring of its own —
// keys never travel between members (a PXC joiner's SST re-encrypts the donor's tablespace keys
// under a master key it generates), so there is nothing to copy and nothing to share.
//
// The keyring file sits on the same disk as the data it protects. It guards a copied datadir or
// a stolen backup of one, not a host somebody can read in full; that is what the OpenBao keyring
// is for.

const (
	// The plugin's keyring (keyring_file_data). /var/lib/mysql-keyring is the directory the
	// Percona Server packages create for exactly this; the stage script creates it where a
	// package does not (MySQL Community, PXC).
	mysqlKeyringFileData = mysqlKeyringDir + "/keyring"
	// The component's keyring (the "path" in component_keyring_file.cnf).
	mysqlKeyringComponentData = mysqlKeyringDir + "/component_keyring_file"
)

// mysqlKeyringKind names the keyring a MySQL-family frame (or the synthetic frame of a standalone
// server) was designed with: "vault", "file" or "". OpenBao wins if both were somehow set;
// validation refuses that combination anyway (keyringFileIssues).
func mysqlKeyringKind(f designFrame) string {
	switch {
	case f.EnableVault:
		return "vault"
	case f.KeyringFile:
		return "file"
	}
	return ""
}

// mysqlKeyringFileOptions renders the [mysqld] lines a node with a keyring file needs: the
// plugin's two options on 5.7 / 8.0 (the component needs none — it is declared by the manifest),
// and the same default_table_encryption the vault keyring sets (see mysqlVaultOptions).
func mysqlKeyringFileOptions(major, version string) string {
	var b strings.Builder
	if !mysqlModernMajor(psMajorOf(major)) {
		b.WriteString("early-plugin-load=keyring_file.so\n")
		b.WriteString("keyring_file_data=" + mysqlKeyringFileData + "\n")
	}
	b.WriteString(mysqlDefaultTableEncryption(major, version))
	return b.String()
}

// mysqlDefaultTableEncryption is `default_table_encryption=ON` where the server has the variable,
// and nothing where it does not: it arrived in 8.0.16, so 5.7 and 8.0.15 would refuse to start on
// an unknown option. Tables there are encrypted with an explicit ENCRYPTION='Y'.
func mysqlDefaultTableEncryption(major, version string) string {
	switch psMajorOf(major) {
	case "5.7":
		return ""
	case "8.0":
		if strings.HasPrefix(version, "8.0.15-") || version == "8.0.15" {
			return ""
		}
	}
	return "default_table_encryption=ON\n"
}

// mysqlKeyringFileComponentConf renders component_keyring_file.cnf.
func mysqlKeyringFileComponentConf() string {
	b, _ := json.MarshalIndent(map[string]any{
		"path":      mysqlKeyringComponentData,
		"read_only": false,
	}, "", "  ")
	return string(b) + "\n"
}

// mysqlKeyringFileStagePluginScript prepares the plugin's directory (5.7 / 8.0). The keyring file
// itself is created by the server at first start; the directory has to exist, be mysql-owned and
// be closed to everyone else, because the plugin creates its file with the directory's access.
// keyring_file.so is checked for first: it is loaded with early-plugin-load, and a missing early
// plugin is a server that does not start at all.
const mysqlKeyringFileStagePluginScript = `set -e
find /usr/lib64 /usr/lib -name keyring_file.so ! -path '*/debug/*' ! -path '*xtrabackup*' -print -quit 2>/dev/null | grep -q . || {
  echo "keyring_file.so is not installed on this node"; exit 1; }
install -d -o mysql -g mysql -m 0750 ` + mysqlKeyringDir + `
echo "keyring_file plugin: keyring at ` + mysqlKeyringFileData + `"`

// mysqlKeyringFileStageComponentScript writes the manifest and the component config (8.4 / 9.7),
// and creates the keyring file itself, empty and mysql-owned. Otherwise the component creates it
// during `mysqld --initialize`, which runs as root: the file comes out root-owned, and the server,
// running as mysql afterwards, fails with "Failed to read keyring file". An empty file is a valid,
// empty keyring.
// plugin_dir is asked of the mysqld binary, with the same fallback and the same debug/xtrabackup
// exclusions as the vault component (mysqlKeyringStageComponentScript explains both): the .cnf
// must be in the plugin_dir the server resolves file://component_keyring_file against.
const mysqlKeyringFileStageComponentScript = `set -e
BINDIR=$(dirname "$(readlink -f "$(command -v mysqld)")")
PLUGIN_DIR=$(mysqld --no-defaults --verbose --help 2>/dev/null | awk '$1=="plugin-dir"{print $2; exit}')
PLUGIN_DIR=${PLUGIN_DIR%/}
if [ -z "$PLUGIN_DIR" ] || [ ! -d "$PLUGIN_DIR" ]; then
  SO=$(find /usr/lib64 /usr/lib -name component_keyring_file.so \
         ! -path '*/debug/*' ! -path '*xtrabackup*' -print -quit 2>/dev/null || true)
  [ -n "$SO" ] || { echo "component_keyring_file.so is not installed on this node"; exit 1; }
  PLUGIN_DIR=$(dirname "$SO")
fi
[ -f "$PLUGIN_DIR/component_keyring_file.so" ] || {
  echo "component_keyring_file.so is not in $PLUGIN_DIR — this server has no file keyring to load"; exit 1; }
install -d -o mysql -g mysql -m 0750 ` + mysqlKeyringDir + `
[ -e ` + mysqlKeyringComponentData + ` ] || install -o mysql -g mysql -m 0640 /dev/null ` + mysqlKeyringComponentData + `
printf '%s' "$CONF" > "$PLUGIN_DIR/component_keyring_file.cnf"
chown mysql:mysql "$PLUGIN_DIR/component_keyring_file.cnf"
chmod 0640 "$PLUGIN_DIR/component_keyring_file.cnf"
printf '{ "components": "file://component_keyring_file" }\n' > "$BINDIR/mysqld.my"
chmod 0644 "$BINDIR/mysqld.my"
echo "component_keyring_file staged (manifest $BINDIR/mysqld.my, config $PLUGIN_DIR/component_keyring_file.cnf, keyring ` + mysqlKeyringComponentData + `)"`

// mysqlKeyringFileVerifyScript checks the keyring once the server is up and, on the writable
// member, creates and drops an encrypted table — the operation that makes the server generate a
// master key and write it to the keyring file. It then checks the file is there and not empty.
const mysqlKeyringFileVerifyScript = `set -e
export MYSQL_PWD="$ROOT_PW"
Q() { mysql -u root -N -B -e "$1" 2>/dev/null | tail -1; }
if [ "$MODE" = "component" ]; then
  STATUS=$(Q "SELECT STATUS_VALUE FROM performance_schema.keyring_component_status WHERE STATUS_KEY='Component_status'")
  [ "$STATUS" = "Active" ] || {
    echo "component_keyring_file is not Active (status: ${STATUS:-not loaded}):"
    grep -iE 'keyring|component' "$LOGERR" 2>/dev/null | tail -5; exit 1; }
  echo "component_keyring_file Active ($KEYRING)"
else
  STATUS=$(Q "SELECT PLUGIN_STATUS FROM information_schema.plugins WHERE PLUGIN_NAME='keyring_file'")
  [ "$STATUS" = "ACTIVE" ] || {
    echo "the keyring_file plugin is not ACTIVE (status: ${STATUS:-not loaded}):"
    grep -iE 'keyring' "$LOGERR" 2>/dev/null | tail -10; exit 1; }
  echo "keyring_file plugin ACTIVE ($KEYRING)"
fi
if [ "$SMOKE" = "1" ]; then
  mysql -u root -e "CREATE DATABASE IF NOT EXISTS dbcanvas_tde_check;
    CREATE TABLE dbcanvas_tde_check.probe (id INT PRIMARY KEY) ENCRYPTION='Y';
    DROP DATABASE dbcanvas_tde_check;" || {
    echo "the keyring loaded but could not store a master key:"
    grep -iE 'keyring' "$LOGERR" 2>/dev/null | tail -10; exit 1; }
  [ -s "$KEYRING" ] || { echo "the encrypted table was created but $KEYRING is empty or missing"; exit 1; }
  echo "wrote and dropped an encrypted table — the master key is in $KEYRING"
fi`

// prepareMySQLKeyring stages whichever keyring the frame asks for, before mysqld has started, and
// records it on the node. It returns the [mysqld] lines for the node's config ("" for none).
func (a *App) prepareMySQLKeyring(ctx context.Context, st Stack, frame designFrame, major, version, host, nodeID, containerID string, pr *pxcProg) (string, error) {
	switch mysqlKeyringKind(frame) {
	case "vault":
		prep, err := a.prepareMySQLVault(ctx, st, frame.OpenBaoNodeID, frame.OS, major, host, containerID, pr)
		if err != nil {
			return "", err
		}
		a.persistConfigKey(st, nodeID, "vault", prep.Info)
		return prep.Options, nil
	case "file":
		pr.phase("Configuring keyring (file)", 50)
		script, env := mysqlKeyringFileStagePluginScript, []string(nil)
		info := vaultInfo{Enabled: true, Provider: "file", Method: "keyring_file plugin",
			ConfFile: "my.cnf (early-plugin-load, keyring_file_data)", KeyringFile: mysqlKeyringFileData}
		if mysqlModernMajor(psMajorOf(major)) {
			script, env = mysqlKeyringFileStageComponentScript, []string{"CONF=" + mysqlKeyringFileComponentConf()}
			info.Method, info.ConfFile, info.KeyringFile = "component_keyring_file",
				"component_keyring_file.cnf (in plugin_dir)", mysqlKeyringComponentData
		}
		if err := a.runStep(ctx, containerID, script, env, pr.logln); err != nil {
			return "", err
		}
		a.persistConfigKey(st, nodeID, "vault", info)
		pr.logln("keyring: " + info.Method + " → " + info.KeyringFile)
		return mysqlKeyringFileOptions(major, version), nil
	}
	return "", nil
}

// verifyMySQLKeyring confirms the frame's keyring is live on a running server; smoke asks for the
// end-to-end check, which only a writable member can run; pct is the progress the step shows at.
// A frame with no keyring passes.
func (a *App) verifyMySQLKeyring(ctx context.Context, frame designFrame, containerID, major, host, rootPW string, smoke bool, pct int, pr *pxcProg) error {
	switch mysqlKeyringKind(frame) {
	case "vault":
		pr.phase("Verifying keyring (OpenBao)", pct)
		mount, _, _ := mysqlVaultMount(major, host)
		return a.verifyMySQLVault(ctx, containerID, frame.OS, major, mount, rootPW, smoke, pr)
	case "file":
		pr.phase("Verifying keyring (file)", pct)
		mode, keyring := "plugin", mysqlKeyringFileData
		if mysqlModernMajor(psMajorOf(major)) {
			mode, keyring = "component", mysqlKeyringComponentData
		}
		env := []string{"MODE=" + mode, "KEYRING=" + keyring, "ROOT_PW=" + rootPW, "LOGERR=" + pxcLogError(frame.OS)}
		if smoke {
			env = append(env, "SMOKE=1")
		}
		return a.runStep(ctx, containerID, mysqlKeyringFileVerifyScript, env, pr.logln)
	}
	return nil
}

// keyringFileTypes are the node and frame types that offer a keyring file. The InnoDB Cluster /
// Group Replication frames and MariaDB do not: MariaDB's file key management is a different
// plugin with a different key format.
var keyringFileTypes = map[string]bool{"ps": true, "mysqlce": true, "mysql": true, "pxc": true, "mysqlcerepl": true}

// keyringFileIssues validates a keyring-file selection on a node or frame.
func keyringFileIssues(who, typ string, keyringFile, enableVault bool) []issue {
	if !keyringFile {
		return nil
	}
	if !keyringFileTypes[typ] {
		return []issue{{Level: "error", Message: who + " has keyringFile set, which is offered on Percona Server, PXC and MySQL Community only"}}
	}
	if enableVault {
		return []issue{{Level: "error", Message: who + " has both a keyring file and OpenBao encryption turned on — a server has one keyring; pick one"}}
	}
	return nil
}
