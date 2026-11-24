package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// mysqlkeyringops.go — looking after a running MySQL-family server's keyring (a keyring file,
// keyringfile.go, or OpenBao, dbvault.go) from its Encryption tab: what is in it, rotating the
// master key, and taking a copy of a keyring file.
//
// A keyring file is the one piece of a keyring-encrypted server that cannot be recreated: lose it
// and every encrypted tablespace is unreadable. The tab therefore says where it is, how many keys
// it holds and when the master key was last rotated, and offers the file itself for download, so
// it can be kept somewhere other than the disk it protects.
//
// Rotation is per server. Every member of a cluster has a keyring of its own, and ALTER INSTANCE
// ROTATE INNODB MASTER KEY re-wraps only that server's tablespace keys, so rotating one member
// changes nothing on the others. With binary log encryption on, the binary log master key is
// rotated too.

// mysqlKeyringEngines are the node types whose deployment can carry a MySQL keyring.
var mysqlKeyringEngines = map[string]bool{"ps": true, "mysqlce": true, "pxc": true, "mysql": true, "mysqlcerepl": true}

// mysqlKeyringNode is a running MySQL-family node with a keyring, as the handlers need it.
type mysqlKeyringNode struct {
	st     Stack
	nodeID string
	dep    Deployment
	vault  vaultInfo
	rootPW string
	host   string
}

// mysqlKeyringLoad resolves the request's node, writing the HTTP error itself.
func (a *App) mysqlKeyringLoad(w http.ResponseWriter, r *http.Request) (mysqlKeyringNode, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return mysqlKeyringNode{}, false
	}
	k := mysqlKeyringNode{st: st, nodeID: r.PathValue("nid")}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	typ := ""
	for _, n := range doc.Nodes {
		if n.ID == k.nodeID {
			typ = n.Type
		}
	}
	if !mysqlKeyringEngines[typ] {
		writeErr(w, http.StatusNotFound, "not a MySQL-family node")
		return k, false
	}
	dep, err := a.store.GetDeployment(st.ID, k.nodeID)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return k, false
	}
	var cfg struct {
		Hostname string    `json:"hostname"`
		Vault    vaultInfo `json:"vault"`
	}
	json.Unmarshal(dep.Config, &cfg)
	if !cfg.Vault.Enabled {
		writeErr(w, http.StatusBadRequest, "this server has no keyring")
		return k, false
	}
	var sec pxcSecrets
	json.Unmarshal(dep.Secrets, &sec)
	k.dep, k.vault, k.rootPW, k.host = dep, cfg.Vault, sec.RootPassword, cfg.Hostname
	a.stampEngine(r, st, k.nodeID)
	return k, true
}

// mysqlKeyringStatusScript reports the keyring: its file (size, modified), the keys in it, the
// encrypted tablespaces, and the encryption variables. Every query is allowed to fail — 5.7 has
// no performance_schema.keyring_keys — and then reports nothing rather than an error.
// Env: ROOT_PW, KEYRING (the file, "" for OpenBao).
const mysqlKeyringStatusScript = `export MYSQL_PWD="$ROOT_PW"
Q() { mysql -u root -N -B -e "$1" 2>/dev/null; }
if [ -n "$KEYRING" ] && [ -e "$KEYRING" ]; then
  echo "FILE_BYTES=$(stat -c %s "$KEYRING")"
  echo "FILE_MODIFIED=$(date -u -d @"$(stat -c %Y "$KEYRING")" +%Y-%m-%dT%H:%M:%SZ)"
  echo "FILE_MODE=$(stat -c '%A %U:%G' "$KEYRING")"
fi
echo "KEYS=$(Q "SELECT COUNT(*) FROM performance_schema.keyring_keys")"
echo "MASTER_KEYS=$(Q "SELECT COUNT(*) FROM performance_schema.keyring_keys WHERE KEY_ID LIKE 'INNODBKey%'")"
echo "TABLESPACES=$(Q "SELECT COUNT(*) FROM information_schema.innodb_tablespaces WHERE FLAG & 8192 OR ENCRYPTION='Y'")"
for v in default_table_encryption binlog_encryption innodb_redo_log_encrypt innodb_undo_log_encrypt innodb_temp_tablespace_encrypt; do
  val=$(Q "SELECT @@GLOBAL.$v") && echo "VAR_$v=$val"
done
true`

// mysqlKeyringRotateScript rotates the InnoDB master key, and the binary log master key when
// binary log encryption is on. Env: ROOT_PW.
const mysqlKeyringRotateScript = `set -e
export MYSQL_PWD="$ROOT_PW"
mysql -u root -e "ALTER INSTANCE ROTATE INNODB MASTER KEY"
echo "rotated the InnoDB master key"
if [ "$(mysql -u root -N -B -e "SELECT @@GLOBAL.binlog_encryption" 2>/dev/null)" = 1 ]; then
  mysql -u root -e "ALTER INSTANCE ROTATE BINLOG MASTER KEY"
  echo "rotated the binary log master key"
fi`

// handleMySQLKeyring is GET …/nodes/{nid}/mysql/keyring: the keyring's state.
func (a *App) handleMySQLKeyring(w http.ResponseWriter, r *http.Request) {
	k, ok := a.mysqlKeyringLoad(w, r)
	if !ok {
		return
	}
	file := ""
	if k.vault.Provider == "file" {
		file = k.vault.KeyringFile
	}
	c := withEngine(r.Context(), a.depEngine(k.st, k.nodeID))
	res, err := a.engCtx(c).Exec(c, k.dep.ContainerID, []string{"bash", "-c", mysqlKeyringStatusScript},
		[]string{"ROOT_PW=" + k.rootPW, "KEYRING=" + file})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := map[string]any{"provider": orDefault(k.vault.Provider, "vault"), "method": k.vault.Method,
		"keyringFile": file, "rotatedAt": k.vault.RotatedAt, "encrypts": k.vault.Encrypts}
	vars := map[string]string{}
	for _, l := range strings.Split(res.Stdout, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok || val == "" {
			continue
		}
		switch {
		case strings.HasPrefix(key, "VAR_"):
			vars[strings.TrimPrefix(key, "VAR_")] = val
		case key == "FILE_BYTES" || key == "KEYS" || key == "MASTER_KEYS" || key == "TABLESPACES":
			var n int
			fmt.Sscan(val, &n)
			out[map[string]string{"FILE_BYTES": "fileBytes", "KEYS": "keys", "MASTER_KEYS": "masterKeys", "TABLESPACES": "tablespaces"}[key]] = n
		case key == "FILE_MODIFIED":
			out["fileModified"] = val
		case key == "FILE_MODE":
			out["fileMode"] = val
		}
	}
	out["variables"] = vars
	writeJSON(w, http.StatusOK, out)
}

// handleMySQLKeyringRotate is POST …/nodes/{nid}/mysql/keyring/rotate.
func (a *App) handleMySQLKeyringRotate(w http.ResponseWriter, r *http.Request) {
	k, ok := a.mysqlKeyringLoad(w, r)
	if !ok {
		return
	}
	c := withEngine(r.Context(), a.depEngine(k.st, k.nodeID))
	res, err := a.engCtx(c).Exec(c, k.dep.ContainerID, []string{"bash", "-c", mysqlKeyringRotateScript}, []string{"ROOT_PW=" + k.rootPW})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "rotate the master key: "+lastLines(strings.TrimSpace(res.Stderr+"\n"+res.Stdout), 400))
		return
	}
	v := k.vault
	v.RotatedAt = time.Now().UTC().Format(time.RFC3339)
	a.persistConfigKey(k.st, k.nodeID, "vault", v)
	a.notifyStack(k.st.ID, "keyring.rotated", "success", "Master key rotated", orDefault(k.host, k.nodeID)+": "+strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; ")+".", k.nodeID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "rotatedAt": v.RotatedAt, "log": strings.TrimSpace(res.Stdout)})
}

// handleMySQLKeyringDownload is GET …/nodes/{nid}/mysql/keyring/file: the keyring file itself,
// as a download. OpenBao keyrings have no file; their keys are in OpenBao.
func (a *App) handleMySQLKeyringDownload(w http.ResponseWriter, r *http.Request) {
	k, ok := a.mysqlKeyringLoad(w, r)
	if !ok {
		return
	}
	if k.vault.Provider != "file" || k.vault.KeyringFile == "" {
		writeErr(w, http.StatusBadRequest, "this server's keys are in OpenBao, not in a keyring file")
		return
	}
	c := withEngine(r.Context(), a.depEngine(k.st, k.nodeID))
	res, err := a.engCtx(c).Exec(c, k.dep.ContainerID, []string{"bash", "-c", `base64 -w0 "$F"`}, []string{"F=" + k.vault.KeyringFile})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if res.Code != 0 {
		writeErr(w, http.StatusNotFound, "cannot read "+k.vault.KeyringFile+": "+strings.TrimSpace(res.Stderr))
		return
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "the keyring file did not come back intact")
		return
	}
	name := sanitizeName(orDefault(k.host, k.nodeID)) + "-" + strings.TrimPrefix(k.vault.KeyringFile[strings.LastIndex(k.vault.KeyringFile, "/")+1:], "/")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}
