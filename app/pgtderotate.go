package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// pgtderotate.go — rotating pg_tde's principal key, on a standalone node or a Patroni cluster.
//
// Rotation is pg_tde's own operation, not a re-encryption of the data: a new key is created in
// OpenBao, made the default (tables) and the server key (WAL), and pg_tde re-wraps the internal
// keys it already has under it — the pages are not rewritten. On Patroni it runs once, on the
// leader: the key map is WAL-logged, so the replicas follow it, and they fetch the new principal
// key from the same mount the next time they need it (checked by restarting a replica after a
// rotation: it read the encrypted rows with the new key).
//
// The new key is named after the old one's base and the time — "<host>-principal-20261014093000"
// — so OpenBao keeps both, and the old one can still be read if a backup from before the
// rotation is restored.

// pgTDERotateKeyName is the next key's name: the base without any earlier timestamp, plus now.
func pgTDERotateKeyName(secretPath string, now time.Time) string {
	base := secretPath
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.Index(base, "-principal"); i >= 0 {
		base = base[:i+len("-principal")]
	}
	if base == "" {
		base = "principal"
	}
	return base + "-" + now.UTC().Format("20060102150405")
}

// pgTDERotate runs the rotation in a container and returns the new key's name.
func (a *App) pgTDERotate(ctx context.Context, containerID, psql string, info vaultInfo) (string, error) {
	key := pgTDERotateKeyName(info.SecretPath, time.Now())
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", pgTDERotateScript},
		[]string{"PSQL=" + psql, "PROVIDER=" + pgTDEProvider, "KEYNAME=" + key})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("%s", lastLines(res.Stderr+res.Stdout, 400))
	}
	return key, nil
}

// rotatedVault is a node's vault info after a rotation: the new key's path and when.
func rotatedVault(v vaultInfo, key string) vaultInfo {
	if i := strings.LastIndex(v.SecretPath, "/"); i >= 0 {
		v.SecretPath = v.SecretPath[:i+1] + key
	}
	v.RotatedAt = time.Now().UTC().Format(time.RFC3339)
	return v
}

// handlePGTDERotate rotates a standalone node's principal key.
func (a *App) handlePGTDERotate(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return
	}
	var cfg pgConfig
	if json.Unmarshal(dep.Config, &cfg) != nil || !cfg.Vault.Enabled {
		writeErr(w, http.StatusBadRequest, "this node is not encrypted with pg_tde")
		return
	}
	a.stampEngine(r, st, nid)
	key, err := a.pgTDERotate(r.Context(), dep.ContainerID, pgBinDir(cfg.OS, cfg.PGMajor)+"/psql", cfg.Vault)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rotate the principal key: "+err.Error())
		return
	}
	nv := rotatedVault(cfg.Vault, key)
	a.persistConfigKey(st, nid, "vault", nv)
	a.notifyStack(st.ID, "tde.rotated", "success", "Principal key rotated", cfg.Hostname+": pg_tde now uses "+key+".", nid)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "key": key, "vault": nv})
}

// handlePatroniTDERotate rotates a Patroni cluster's principal key, on the leader.
func (a *App) handlePatroniTDERotate(w http.ResponseWriter, r *http.Request) {
	t, ok := a.patroniFrameMembers(w, r)
	if !ok {
		return
	}
	ldep, _ := a.store.GetDeployment(t.st.ID, t.leader.id)
	var cfg patroniConfig
	if json.Unmarshal(ldep.Config, &cfg) != nil || !cfg.Vault.Enabled {
		writeErr(w, http.StatusBadRequest, "this cluster is not encrypted with pg_tde")
		return
	}
	key, err := a.pgTDERotate(r.Context(), t.leader.container, pgBinDir(t.frame.OS, ppgMajorOf(t.frame.PGMajor))+"/psql", cfg.Vault)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rotate the principal key: "+err.Error())
		return
	}
	nv := rotatedVault(cfg.Vault, key)
	for _, id := range t.memberIDs {
		var mc patroniConfig
		if d, err := a.store.GetDeployment(t.st.ID, id); err == nil && json.Unmarshal(d.Config, &mc) == nil {
			mv := nv
			mv.Build = mc.Vault.Build // the build is each member's own
			a.persistConfigKey(t.st, id, "vault", mv)
		}
	}
	a.notifyStack(t.st.ID, "tde.rotated", "success", "Principal key rotated", t.frame.Label+": pg_tde now uses "+key+" on every member.", "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "key": key, "vault": nv})
}

// pgTDERotateScript creates the new key in OpenBao, makes it the default (tables) and the server
// key (WAL), and checks both read back from the provider. A standby refuses it — rotation is
// the primary's — so that is said rather than shown as a SQL error.
// Env: PSQL, PROVIDER, KEYNAME.
const pgTDERotateScript = `set -e
P() { runuser -u postgres -- "$PSQL" -h /var/run/postgresql -d postgres -qAtX -v ON_ERROR_STOP=1 -v prov="$PROVIDER" -v key="$KEYNAME"; }
[ "$(printf '%s\n' "SELECT pg_is_in_recovery();" | P)" = "f" ] || { echo "this server is a standby — rotate on the primary"; exit 1; }
printf '%s\n' \
  "SELECT pg_tde_create_key_using_global_key_provider(:'key', :'prov');" \
  "SELECT pg_tde_set_default_key_using_global_key_provider(:'key', :'prov');" \
  "SELECT pg_tde_set_server_key_using_global_key_provider(:'key', :'prov');" \
  "SELECT pg_tde_verify_default_key();" \
  "SELECT pg_tde_verify_server_key();" | P >/dev/null
[ "$(printf '%s\n' "SELECT key_name FROM pg_tde_default_key_info();" | P)" = "$KEYNAME" ] || { echo "the default key did not change to $KEYNAME"; exit 1; }
echo "rotated to $KEYNAME"`
