package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Percona Backup for MongoDB (PBM) integration for the PSMDB sharded-cluster
// (Type=="psmdb") and PSMDB replica-set (Type=="psmrs") frames. The
// `percona-backup-mongodb` package is installed on **every** member node at deploy
// (so backups can be turned on later without a reinstall — like pmm-client). When
// the frame's "Enable PBM backup" option is set and a SeaweedFS node is selected,
// each mongod member runs a `pbm-agent` pointed at its local mongod, and the S3
// store is registered against the SeaweedFS node:
//
//   percona-release enable pbm   # repo
//   dnf/apt install percona-backup-mongodb
//   pbm-agent on every mongod member (PBM_MONGODB_URI=mongodb://pbm:…@localhost)
//   pbm config --file <s3 storage> (run once, on a config server / RS primary)
//
// pbm CLI / agents authenticate as a dedicated `pbm` user (the documented
// pbmAnyAction role + backup/restore/clusterMonitor/readWrite). Backups are taken
// on demand from the manager (POST …/frames/{fid}/pbm/backup).

// pbmFrameIssues validates a psmdb/psmrs frame's PBM backup option: when enabled it
// must point at a SeaweedFS node present in the design (mirrors the patroni rule).
func pbmFrameIssues(f designFrame, doc designDoc) []issue {
	if !f.EnablePBM {
		return nil
	}
	if f.SeaweedFSNodeID == "" {
		return []issue{{"error", "PS MongoDB cluster " + f.Label + " has PBM backup enabled but no SeaweedFS node selected"}}
	}
	for _, n := range doc.Nodes {
		if n.ID == f.SeaweedFSNodeID && n.Type == "seaweedfs" {
			return nil
		}
	}
	return []issue{{"error", "PS MongoDB cluster " + f.Label + ": the selected PBM SeaweedFS node is not in the design"}}
}

// pbmAgentEnvPath is the EnvironmentFile the pbm-agent systemd unit reads.
func pbmAgentEnvPath(os string) string {
	if isDebianOS(os) {
		return "/etc/default/pbm-agent"
	}
	return "/etc/sysconfig/pbm-agent"
}

// pbmMongoURI builds the local-node connection string the pbm-agent / pbm CLI use,
// percent-encoding the credentials so special characters in the password are safe.
func pbmMongoURI(user, pass string) string {
	return fmt.Sprintf("mongodb://%s:%s@localhost:27017/?authSource=admin",
		url.QueryEscape(user), url.QueryEscape(pass))
}

// pbmPrefix is the per-cluster path under the bucket, so several clusters can share
// one SeaweedFS bucket without colliding.
func pbmPrefix(label string) string {
	s := sanitizeName(strings.TrimSpace(label))
	if s == "" {
		s = "pbm"
	}
	return "pbm/" + s
}

// pbmBackupRepoLabel is the short human description shown in the manager.
func pbmBackupRepoLabel(sw seaweedConfig, label string) string {
	bucket := sw.Bucket
	if bucket == "" {
		bucket = "<bucket>"
	}
	return fmt.Sprintf("PBM → SeaweedFS S3 (%s/%s)", bucket, pbmPrefix(label))
}

// pbmUserJS builds the mongosh script that idempotently creates the pbmAnyAction
// role + the PBM user with the documented backup/restore roles.
func pbmUserJS(user, pass string) string {
	const roles = `[{db:"admin",role:"readWrite"},{db:"admin",role:"backup"},{db:"admin",role:"clusterMonitor"},{db:"admin",role:"restore"},{db:"admin",role:"pbmAnyAction"}]`
	const priv = `[{resource:{anyResource:true},actions:["anyAction"]}]`
	return fmt.Sprintf(`var a=db.getSiblingDB("admin");
try{a.createRole({role:"pbmAnyAction",privileges:%s,roles:[]})}catch(e){if(!/already exists/i.test(e.message))throw e}
try{a.createUser({user:%q,pwd:%q,roles:%s})}catch(e){if(/already exists/i.test(e.message)){a.updateUser(%q,{pwd:%q,roles:%s})}else throw e}`,
		priv, user, pass, roles, user, pass, roles)
}

// mongoEnsurePBMUser creates/updates the PBM user on a replica-set primary (or the
// config-RS primary). It authenticates as admin when those creds work, otherwise
// falls back to the localhost exception (sharded shards have no admin user). Reuses
// mongoPMMUserScript, which already implements exactly this auth-or-bootstrap flow.
func (a *App) mongoEnsurePBMUser(ctx context.Context, st Stack, node designNode, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return nil
	}
	js := pbmUserJS(sec.PBMUser, sec.PBMPassword)
	env := []string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "PMM_JS=" + js}
	if err := a.runStep(ctx, dep.ContainerID, mongoPMMUserScript, env, pr.logln); err != nil {
		pr.logln("PBM user creation failed: " + err.Error())
		return err
	}
	pr.logln("PBM backup user ready")
	return nil
}

// mongoSetupPBMAgent writes the pbm-agent EnvironmentFile (PBM_MONGODB_URI →
// the local mongod) and enables + starts the pbm-agent service. Run on every
// mongod member (config + shard members; all RS members) — never on mongos.
func (a *App) mongoSetupPBMAgent(ctx context.Context, st Stack, node designNode, os string, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return nil
	}
	envPath := pbmAgentEnvPath(os)
	dir, base := splitPath(envPath)
	content := fmt.Sprintf("PBM_MONGODB_URI=%s\n", pbmMongoURI(sec.PBMUser, sec.PBMPassword))
	if err := a.docker.CopyFile(ctx, dep.ContainerID, dir, base, 0o600, []byte(content)); err != nil {
		pr.logln("write pbm-agent env failed: " + err.Error())
		return err
	}
	if err := a.runStep(ctx, dep.ContainerID, pbmAgentStartScript, nil, pr.logln); err != nil {
		pr.logln("pbm-agent start failed: " + err.Error())
		return err
	}
	pr.logln("pbm-agent running on " + node.Label)
	return nil
}

// mongoConfigurePBMStorage registers the SeaweedFS S3 store with PBM (run once per
// cluster, on a config-server member for sharded clusters or the RS primary for a
// replica set). The agents pick up the config from the cluster.
func (a *App) mongoConfigurePBMStorage(ctx context.Context, st Stack, node designNode, frame designFrame, sw seaweedConfig, swSec seaweedSecrets, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return nil
	}
	yaml := pbmStorageYAML(frame.Label, sw, swSec)
	if err := a.docker.CopyFile(ctx, dep.ContainerID, "/etc", "pbm-storage.yaml", 0o600, []byte(yaml)); err != nil {
		pr.logln("write pbm-storage.yaml failed: " + err.Error())
		return err
	}
	env := []string{"PBM_URI=" + pbmMongoURI(sec.PBMUser, sec.PBMPassword)}
	if err := a.runStep(ctx, dep.ContainerID, pbmConfigScript, env, pr.logln); err != nil {
		pr.logln("PBM storage config failed: " + err.Error())
		return err
	}
	pr.logln("PBM S3 store configured (" + pbmBackupRepoLabel(sw, frame.Label) + ")")
	return nil
}

// pbmStorageYAML renders the PBM storage config for the SeaweedFS S3 endpoint.
// SeaweedFS requires path-style addressing; TLS verification is skipped (the
// endpoint is plain HTTP, or a self-signed / Intranet-CA cert the node may not trust).
func pbmStorageYAML(label string, sw seaweedConfig, sec seaweedSecrets) string {
	endpoint := sw.InternalEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", sw.FQDN, seaweedS3Port)
	}
	ak := sw.AccessKey
	if ak == "" {
		ak = sec.AccessKey
	}
	region := sw.Region
	if region == "" {
		region = seaweedRegion
	}
	var b strings.Builder
	fmt.Fprintf(&b, "storage:\n")
	fmt.Fprintf(&b, "  type: s3\n")
	fmt.Fprintf(&b, "  s3:\n")
	fmt.Fprintf(&b, "    region: %s\n", region)
	fmt.Fprintf(&b, "    endpointUrl: %s\n", endpoint)
	fmt.Fprintf(&b, "    forcePathStyle: true\n")
	if sw.TLS {
		fmt.Fprintf(&b, "    insecureSkipTLSVerify: true\n")
	}
	fmt.Fprintf(&b, "    bucket: %s\n", sw.Bucket)
	fmt.Fprintf(&b, "    prefix: %s\n", pbmPrefix(label))
	fmt.Fprintf(&b, "    credentials:\n")
	fmt.Fprintf(&b, "      access-key-id: %s\n", ak)
	fmt.Fprintf(&b, "      secret-access-key: %s\n", sec.SecretKey)
	return b.String()
}

// handleMongoPBMBackup runs an on-demand PBM backup for a psmdb/psmrs frame
// (owner-scoped). It picks a running mongod member to coordinate from — a config
// server for a sharded cluster, else any running member — and runs `pbm backup`.
func (a *App) handleMongoPBMBackup(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	fid := r.PathValue("fid")
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	var frame designFrame
	found := false
	for _, f := range doc.Frames {
		if f.ID == fid && (f.Type == "psmdb" || f.Type == "psmrs") {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "PS MongoDB cluster not found")
		return
	}
	if !frame.EnablePBM {
		writeErr(w, http.StatusBadRequest, "PBM backup is not enabled for this cluster")
		return
	}

	// Prefer a config-server member (sharded) — PBM coordinates from there; else any
	// running mongod member of the frame.
	var coord, fallback designNode
	haveCoord, haveFallback := false, false
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || (n.Type != "psmdb" && n.Type != "psmrs") || n.Role == "mongos" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		if !haveFallback {
			fallback, haveFallback = n, true
		}
		if n.Role == "config" && !haveCoord {
			coord, haveCoord = n, true
		}
	}
	node := coord
	if !haveCoord {
		node = fallback
	}
	if !haveCoord && !haveFallback {
		writeErr(w, http.StatusConflict, "no running mongod member found for this cluster")
		return
	}

	var sec mongoSecrets
	dep, _ := a.store.GetDeployment(st.ID, node.ID)
	json.Unmarshal(dep.Secrets, &sec)
	ctx := r.Context()
	env := []string{"PBM_URI=" + pbmMongoURI(sec.PBMUser, sec.PBMPassword)}
	if res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", pbmBackupScript}, env); err != nil {
		writeErr(w, http.StatusInternalServerError, "PBM backup failed: "+err.Error())
		return
	} else if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "PBM backup failed: "+lastLines(res.Stderr+res.Stdout, 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ------------------------------------------------------------------ scripts

// pbmInstall{RHEL,Debian} enable the pbm repo and install percona-backup-mongodb.
// Run on every member node so PBM can be configured later without a reinstall.
const pbmInstallRHEL = `set -e
percona-release enable -y pbm >/dev/null 2>&1 || percona-release enable pbm >/dev/null 2>&1 || percona-release setup -y pbm >/dev/null 2>&1
dnf -y -q install percona-backup-mongodb >/dev/null`

const pbmInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable -y pbm >/dev/null 2>&1 || percona-release enable pbm >/dev/null 2>&1 || percona-release setup -y pbm >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq percona-backup-mongodb >/dev/null`

// pbmAgentStartScript enables + (re)starts the pbm-agent unit after its
// EnvironmentFile has been written, and verifies it stays active.
const pbmAgentStartScript = `set -e
systemctl daemon-reload 2>/dev/null || true
systemctl reset-failed pbm-agent 2>/dev/null || true
systemctl enable pbm-agent >/dev/null 2>&1 || true
systemctl restart pbm-agent
sleep 2
systemctl is-active --quiet pbm-agent || { echo "pbm-agent failed to start:"; journalctl -u pbm-agent --no-pager 2>/dev/null | tail -20; exit 1; }`

// pbmConfigScript registers the S3 storage with PBM (idempotent — re-running just
// re-applies the same config). PBM_URI points the CLI at the cluster.
const pbmConfigScript = `set -e
export PBM_MONGODB_URI="$PBM_URI"
# Give the agents a moment to connect before applying the storage config.
for i in $(seq 1 10); do pbm status >/dev/null 2>&1 && break; sleep 2; done
pbm config --file /etc/pbm-storage.yaml`

// pbmBackupScript runs an on-demand logical backup and waits for PBM to accept it.
const pbmBackupScript = `set -e
export PBM_MONGODB_URI="$PBM_URI"
pbm backup`
