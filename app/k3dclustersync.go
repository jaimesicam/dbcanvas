package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// k3dclustersync.go — Percona ClusterSync for MongoDB run by the Percona Operator for MongoDB.
//
// From operator 1.23.0 the CRD set has a PerconaServerMongoDBClusterSync (psmdb-clustersync): a
// custom resource that deploys PCSM as a Deployment beside a target cluster and drives it through
// spec.mode — running, paused, finalized. On a K3D frame running that operator this is one option:
// pick another MongoDB cluster in the stack as the source, and once the frame's own cluster is
// ready the deploy creates the sync user on the source, the credentials Secret, and the CR.
//
// What the operator's setup guide requires, and how each is met here:
//
//   - The target is the frame's cluster, and the Secret and CR go in its namespace.
//   - The source is reachable from the PCSM pod. The stack's VM-based clusters are, by FQDN:
//     every K3D cluster's CoreDNS forwards the stack domain to the Intranet (k3d.go).
//   - Same major version on both sides, and matching topology — replica set → replica set or
//     sharded → sharded (the operator guide is stricter here than PCSM itself, which also takes
//     the documented upgrade paths and sharded → replica set). Operator 1.23.x's cr.yaml runs
//     PSMDB 8.0, so the source has to be an 8.0 cluster — and vector search, which moves the
//     cluster to 8.3, rules ClusterSync out.
//   - The source credentials live in a Secret with username/password keys, never in the URI.
//
// The image is the one operator 1.23.x is certified with. PCSM 1.0.0 is published too, but the
// operator's controller was written against 0.9.0's API and has not been certified with 1.0.0.

const (
	psmdbClusterSyncMinVer = "1.23.0"
	psmdbClusterSyncImage  = "percona/percona-clustersync-mongodb:0.9.0"
	// psmdbClusterSyncServerMajor is the PSMDB series operator 1.23.x's cr.yaml deploys.
	psmdbClusterSyncServerMajor = "8.0"
)

// psmdbClusterSyncProbes replaces the probes operator 1.23.x generates for the PCSM pod. Those
// give GET /status four seconds, five times running, before the pod is killed — and while PCSM
// clones a collection of any size its /status can take longer than that to answer. Killed
// mid-clone, PCSM 0.9.0 comes back to a half-made initial sync that it refuses to resume
// ("cannot resume: replication is not started or not resuming from failure") and crash-loops,
// with the custom resource still saying running. Found on a 1.7 GB source in a lab k3d cluster.
// These allow 25s a check and ten minutes of failed checks before a restart.
const psmdbClusterSyncProbes = `  livenessProbe:
    exec:
      command: ["curl", "-fsS", "--connect-timeout", "2", "--max-time", "25", "http://127.0.0.1:2242/status"]
    initialDelaySeconds: 30
    periodSeconds: 30
    timeoutSeconds: 30
    failureThreshold: 20
  readinessProbe:
    exec:
      command: ["curl", "-fsS", "--connect-timeout", "2", "--max-time", "25", "http://127.0.0.1:2242/status"]
    initialDelaySeconds: 5
    periodSeconds: 15
    timeoutSeconds: 30
    failureThreshold: 3
`

// psmdbClusterSyncEnv sizes PCSM's workers to the frame rather than the host. PCSM derives its
// defaults from runtime.NumCPU, which inside a k3d pod is every CPU of the Docker host — 18 insert
// workers on a 9-CPU machine, writing into a cluster capped at 4 — and the clone then outruns the
// target until a member stops answering and PCSM loses its lease (it demotes itself to STANDBY and
// the sync stalls). Two insert workers per CPU of the cluster's budget, and one replication worker.
func psmdbClusterSyncEnv(cpus int) string {
	if cpus <= 0 {
		cpus = 4
	}
	return fmt.Sprintf(`  env:
  - name: PCSM_CLONE_NUM_INSERT_WORKERS
    value: "%d"
  - name: PCSM_CLONE_NUM_READ_WORKERS
    value: "%d"
  - name: PCSM_REPL_NUM_WORKERS
    value: "%d"
`, 2*cpus, max(1, cpus/2), cpus)
}

func psmdbHasClusterSync(operatorVer string) bool {
	v := strings.TrimSpace(operatorVer)
	return v != "" && compareVersions(v, psmdbClusterSyncMinVer) >= 0
}

// k3dClusterSyncName is the CR (and so the PCSM Deployment, <name>-pcsm) for a cluster.
func k3dClusterSyncName(cluster string) string { return cluster + "-sync" }

// k3dSyncSourceFrame describes the frame a K3D frame syncs from, as validation sees it.
func k3dSyncSourceFrame(doc designDoc, id string) (f designFrame, topology, major string, ok bool) {
	f = frameByID(doc, id)
	switch f.Type {
	case "psmrs":
		return f, "replset", f.PSMDBMajor, true
	case "psmdb":
		return f, "sharded", f.PSMDBMajor, true
	}
	return designFrame{}, "", "", false
}

// k3dClusterSyncIssues validates a K3D frame's ClusterSync option.
func k3dClusterSyncIssues(f designFrame, doc designDoc, opCat OperatorCatalog) []issue {
	if !f.K3DClusterSync {
		return nil
	}
	name := "K3D cluster " + f.Label
	if f.K3DOperator != "psmdb" {
		return []issue{{Level: "warning", Message: name + " has ClusterSync on, which only the Percona Operator for MongoDB runs — it is ignored for " +
			orDefault(k3dOperatorLabel(f.K3DOperator), "a cluster with no operator")}}
	}
	ver, ok := opCat.resolveOperatorVersion("psmdb", f.K3DOperatorVer)
	switch {
	case !ok:
		return []issue{{Level: "error", Message: name + " asks for ClusterSync, which needs a known operator version to check against " + psmdbClusterSyncMinVer}}
	case !psmdbHasClusterSync(ver):
		return []issue{{Level: "error", Message: name + " asks for ClusterSync, which the Percona Operator for MongoDB has from " + psmdbClusterSyncMinVer +
			" — this frame pins " + ver + ", whose CRDs have no PerconaServerMongoDBClusterSync. Choose " + psmdbClusterSyncMinVer + " or newer"}}
	}
	return k3dClusterSyncFrameIssues(f, doc)
}

// k3dClusterSyncFrameIssues is the part of k3dClusterSyncIssues about the pair itself, once the
// operator is known to have ClusterSync: a source, matching topology, the same major.
func k3dClusterSyncFrameIssues(f designFrame, doc designDoc) []issue {
	name := "K3D cluster " + f.Label
	src, topo, major, ok := k3dSyncSourceFrame(doc, f.K3DClusterSyncSource)
	if !ok {
		return []issue{{Level: "error", Message: name + " has ClusterSync on but no source — pick a PS MongoDB replica set or sharded cluster in this stack"}}
	}
	var out []issue
	switch {
	case f.K3DSharding && topo != "sharded":
		out = append(out, issue{Level: "error", Message: name + " is sharded, and the operator syncs a sharded cluster only from a sharded source — " + src.Label + " is a replica set"})
	case !f.K3DSharding && topo != "replset":
		out = append(out, issue{Level: "error", Message: name + " is a replica set, and the operator syncs a replica set only from a replica set — " + src.Label + " is sharded"})
	}
	if f.K3DVectorSearch {
		out = append(out, issue{Level: "error", Message: name + " has vector search on, which runs PSMDB 8.3 — PCSM supports 6.0 to 8.0, so ClusterSync and vector search cannot go together"})
	} else if m := strings.TrimSpace(major); m != "" && m != psmdbClusterSyncServerMajor {
		out = append(out, issue{Level: "error", Message: name + " runs PSMDB " + psmdbClusterSyncServerMajor + ", and the operator syncs only from the same major — " +
			src.Label + " is " + m + ". Set it to " + psmdbClusterSyncServerMajor})
	}
	if f.K3DSharding && topo == "sharded" {
		out = append(out, issue{Level: "warning", Message: name + ": sharded ClusterSync is a tech preview in the operator"})
	}
	return out
}

// k3dStartClusterSync wires the frame's ClusterSync once its cluster is ready. Never fatal: the
// cluster is up either way, and the reason goes in the frame's log.
func (a *App) k3dStartClusterSync(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg k3dConfig, say func(string)) {
	if !frame.K3DClusterSync || frame.K3DOperator != "psmdb" {
		return
	}
	if !psmdbHasClusterSync(cfg.OperatorVer) {
		say("ClusterSync skipped: operator " + cfg.OperatorVer + " has no PerconaServerMongoDBClusterSync (added in " + psmdbClusterSyncMinVer + ")")
		return
	}
	// The source deploys in parallel with this frame and is usually up long before an operator
	// cluster is — but not always.
	deadline := time.Now().Add(deployTimeout())
	for {
		if _, ok := pcsmEndpointForFrame(a.pcsmEndpoints(st, ""), doc, frame.K3DClusterSyncSource); ok {
			break
		}
		if time.Now().After(deadline) || !k3dSleep(ctx, 10*time.Second) {
			say("ClusterSync skipped: the source cluster is not running")
			return
		}
	}
	info, err := a.k3dCreateClusterSync(ctx, st, frame, doc, serverID, cfg, frame.K3DClusterSyncSource)
	if err != nil {
		say("ClusterSync skipped: " + err.Error())
		return
	}
	say("ClusterSync " + info.Name + " created: " + info.Source + " (MongoDB " + info.SourceVersion + ") → " + cfg.ClusterName + ", image " + info.Image +
		" — watch it on the frame's ClusterSync tab, or `kubectl get psmdb-clustersync -n " + cfg.Namespace + "`")
}

// k3dClusterSyncCR is the PerconaServerMongoDBClusterSync DBCanvas applies — also what the tab's
// "by hand" instructions show, so the two cannot drift apart. The source URI carries no
// credentials: the operator reads them from the Secret.
func k3dClusterSyncCR(name, cluster, image, uri string, cpus int) string {
	return fmt.Sprintf(`apiVersion: psmdb.percona.com/v1
kind: PerconaServerMongoDBClusterSync
metadata:
  name: %s
spec:
  clusterName: %s
  image: %s
  mode: running
  source:
    uri: %s
    credentialsSecret: %s
`, name, cluster, image, uri, name+"-source") + psmdbClusterSyncProbes + psmdbClusterSyncEnv(cpus)
}

// k3dSyncSourceURI is the source as the operator wants it: hosts, /admin, and the set name.
func k3dSyncSourceURI(src pcsmEndpoint) string {
	uri := "mongodb://" + strings.Join(src.target.Addresses(), ",") + "/admin"
	if src.Topology == "replset" {
		uri += "?replicaSet=" + src.target.ReplicaSet
	}
	return uri
}

// k3dCreateClusterSync creates the source account, the credentials Secret and the custom
// resource for a sync from srcFrameID into the frame's cluster, and records it on the frame's
// members. Shared by the deploy and the tab's Start.
func (a *App) k3dCreateClusterSync(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg k3dConfig, srcFrameID string) (k3dClusterSyncInfo, error) {
	src, ok := pcsmEndpointForFrame(a.pcsmEndpoints(st, ""), doc, srcFrameID)
	if !ok {
		return k3dClusterSyncInfo{}, fmt.Errorf("the source cluster is not running")
	}
	pw := genSecret("Pcsm")
	ver, err := a.pcsmEnsureUser(ctx, st, src.target, pcsmSourceUser, pw, pcsmSourceRoles)
	if err != nil {
		return k3dClusterSyncInfo{}, err
	}
	name := k3dClusterSyncName(cfg.ClusterName)
	uri := k3dSyncSourceURI(src)
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\ntype: Opaque\nstringData:\n  username: %s\n  password: %s\n---\n",
		name+"-source", pcsmSourceUser, pw)
	manifest := secret + k3dClusterSyncCR(name, cfg.ClusterName, psmdbClusterSyncImage, uri, frame.K3DCPUs)
	if err := a.kubectlApply(ctx, serverID, cfg.Namespace, []byte(manifest)); err != nil {
		return k3dClusterSyncInfo{}, fmt.Errorf("apply the PerconaServerMongoDBClusterSync: %s", lastLines(err.Error(), 300))
	}
	info := k3dClusterSyncInfo{Name: name, SourceFrame: srcFrameID, Source: k3dSourceLabel(src), SourceVersion: ver,
		URI: uri, Image: psmdbClusterSyncImage, StartedAt: time.Now().Unix()}
	a.k3dRecordClusterSync(st, doc, frame.ID, &info)
	return info, nil
}

// k3dRecordClusterSync writes (or, with nil, clears) the sync on every member of the frame — the
// panel is opened on whichever node was clicked.
func (a *App) k3dRecordClusterSync(st Stack, doc designDoc, frameID string, info *k3dClusterSyncInfo) {
	for _, n := range doc.Nodes {
		if n.FrameID == frameID && n.Type == "k3d" {
			a.persistConfigKey(st, n.ID, "clusterSync", info)
		}
	}
}

// k3dSourceLabel is the source's frame label without the target list's suffix.
func k3dSourceLabel(e pcsmEndpoint) string {
	l := e.Label
	if i := strings.Index(l, " ("); i > 0 {
		l = l[:i]
	}
	return l
}

// pcsmEndpointForFrame finds the endpoint standing for a frame: the replica set target, or the
// sharded cluster's mongos.
func pcsmEndpointForFrame(eps []pcsmEndpoint, doc designDoc, frameID string) (pcsmEndpoint, bool) {
	for _, e := range eps {
		if e.ID == frameID+"#frame" || (e.Topology == "sharded" && nodeByID(doc, e.target.NodeID).FrameID == frameID) {
			return e, true
		}
	}
	return pcsmEndpoint{}, false
}

// k3dClusterSyncInfo is what the frame's members record about the sync, under "clusterSync".
type k3dClusterSyncInfo struct {
	Name          string `json:"name"`
	SourceFrame   string `json:"sourceFrame,omitempty"`
	Source        string `json:"source"`
	SourceVersion string `json:"sourceVersion"`
	URI           string `json:"uri"`
	Image         string `json:"image"`
	StartedAt     int64  `json:"startedAt"`
}

// k3dPCSMStateDB is where PCSM keeps its heartbeat and recovery data on the target. Deleting the
// custom resource leaves it behind, and a ClusterSync created later against the same target
// finds it, tries to recover a run that no longer exists, and crash-loops ("cannot resume:
// replication is not started or not resuming from failure"). Stop drops it.
const k3dPCSMStateDB = "percona_clustersync_mongodb"

// ---------------------------------------------------------------- HTTP

// k3dSyncScope is a frame request, resolved.
type k3dSyncScope struct {
	ctx      context.Context
	st       Stack
	doc      designDoc
	frame    designFrame
	cfg      k3dConfig
	serverID string
	info     *k3dClusterSyncInfo // nil: no sync recorded
}

// k3dSyncRequest resolves a frame request to its running server node and recorded sync.
func (a *App) k3dSyncRequest(w http.ResponseWriter, r *http.Request) (k3dSyncScope, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return k3dSyncScope{}, false
	}
	doc := buildDoc(st)
	fid := r.PathValue("fid")
	frame := frameByID(doc, fid)
	cfg, serverID, ok := a.k3dServerConfig(st.ID, doc, fid)
	if !ok {
		writeErr(w, http.StatusConflict, "the Kubernetes cluster is not running")
		return k3dSyncScope{}, false
	}
	if cfg.Operator != "psmdb" {
		writeErr(w, http.StatusBadRequest, "ClusterSync runs on the Percona Operator for MongoDB")
		return k3dSyncScope{}, false
	}
	sc := k3dSyncScope{ctx: withEngine(r.Context(), a.nodeEngine(st, "k3d")), st: st, doc: doc, frame: frame, cfg: cfg, serverID: serverID}
	for _, n := range doc.Nodes {
		if n.FrameID != fid || n.Type != "k3d" {
			continue
		}
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil {
			var m struct {
				ClusterSync *k3dClusterSyncInfo `json:"clusterSync"`
			}
			if json.Unmarshal(dep.Config, &m) == nil && m.ClusterSync != nil && m.ClusterSync.Name != "" {
				sc.info = m.ClusterSync
				break
			}
		}
	}
	return sc, true
}

// k3dSyncSources is every MongoDB cluster on the canvas this frame could sync from, each with the
// reason it cannot when it cannot — the operator's rules, as k3dClusterSyncFrameIssues applies them.
func (a *App) k3dSyncSources(sc k3dSyncScope) []map[string]any {
	running := map[string]pcsmEndpoint{}
	for _, f := range sc.doc.Frames {
		if e, ok := pcsmEndpointForFrame(a.pcsmEndpoints(sc.st, ""), sc.doc, f.ID); ok {
			running[f.ID] = e
		}
	}
	out := []map[string]any{}
	for _, f := range sc.doc.Frames {
		_, topo, major, ok := k3dSyncSourceFrame(sc.doc, f.ID)
		if !ok {
			continue
		}
		probe := sc.frame
		probe.K3DClusterSync, probe.K3DClusterSyncSource = true, f.ID
		issue := ""
		for _, i := range k3dClusterSyncFrameIssues(probe, sc.doc) {
			if i.Level == "error" {
				issue = i.Message
				break
			}
		}
		e, up := running[f.ID]
		if issue == "" && !up {
			issue = f.Label + " is not running"
		}
		src := map[string]any{"id": f.ID, "label": f.Label, "topology": topo, "major": major, "issue": issue}
		if up {
			src["hosts"] = strings.Join(e.target.Addresses(), ",")
			src["replicaSet"] = e.ReplicaSet
			src["uri"] = k3dSyncSourceURI(e)
		}
		out = append(out, src)
	}
	return out
}

// GET — the recorded sync and its live state; or, with none, what the cluster could sync from.
// Either way the CR DBCanvas applies (or would), for the tab's "by hand" instructions.
func (a *App) handleK3DClusterSync(w http.ResponseWriter, r *http.Request) {
	sc, ok := a.k3dSyncRequest(w, r)
	if !ok {
		return
	}
	ctx, cfg, ns := sc.ctx, sc.cfg, sc.cfg.Namespace
	name := k3dClusterSyncName(cfg.ClusterName)
	resp := map[string]any{
		"sync": sc.info, "target": cfg.ClusterName, "namespace": ns, "name": name, "sharding": cfg.Sharding,
		"operatorVer": cfg.OperatorVer, "supported": psmdbHasClusterSync(cfg.OperatorVer),
		"image": psmdbClusterSyncImage, "user": pcsmSourceUser, "roles": pcsmSourceRoles,
		// The URI is a placeholder the tab fills from the chosen source.
		"crTemplate": k3dClusterSyncCR(name, cfg.ClusterName, psmdbClusterSyncImage, "__SOURCE_URI__", sc.frame.K3DCPUs),
		"stateDB":    k3dPCSMStateDB,
		"targetPod":  k3dTargetAdminPod(cfg),
	}
	if sc.info == nil {
		resp["sources"] = a.k3dSyncSources(sc)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if out, err := a.kubectl(ctx, sc.serverID, "-n", ns, "get", "psmdb-clustersync", sc.info.Name, "-o", "json"); err == nil {
		var cr struct {
			Spec   json.RawMessage `json:"spec"`
			Status json.RawMessage `json:"status"`
		}
		if json.Unmarshal([]byte(out), &cr) == nil {
			resp["cr"] = map[string]json.RawMessage{"spec": cr.Spec, "status": cr.Status}
		}
	} else {
		resp["crError"] = lastLines(err.Error(), 200)
	}
	deploy := "deploy/" + sc.info.Name + "-pcsm"
	if out, err := a.kubectl(ctx, sc.serverID, "-n", ns, "exec", deploy, "--", "pcsm", "status"); err == nil {
		if s := strings.TrimSpace(out); json.Valid([]byte(s)) {
			resp["status"] = json.RawMessage(s)
		}
	}
	if out, err := a.kubectl(ctx, sc.serverID, "-n", ns, "get", "pods", "-l", "app.kubernetes.io/component=pcsm,app.kubernetes.io/instance="+sc.info.Name,
		"-o", "jsonpath={.items[0].status.phase}"); err == nil {
		resp["service"] = strings.ToLower(strings.TrimSpace(out))
	}
	if out, err := a.kubectl(ctx, sc.serverID, "-n", ns, "logs", deploy, "--tail=40"); err == nil {
		resp["log"] = pcsmLogLines(out)
	}
	writeJSON(w, http.StatusOK, resp)
}

// k3dTargetAdminPod is the pod and container that reach the whole target cluster: a mongos for a
// sharded cluster, the first rs0 member otherwise.
func k3dTargetAdminPod(cfg k3dConfig) [2]string {
	if cfg.Sharding {
		return [2]string{cfg.ClusterName + "-mongos-0", "mongos"}
	}
	return [2]string{cfg.ClusterName + "-rs0-0", "mongod"}
}

// POST — start a sync from the tab. Body: {"source": "<frame id>"}.
func (a *App) handleK3DClusterSyncStart(w http.ResponseWriter, r *http.Request) {
	sc, ok := a.k3dSyncRequest(w, r)
	if !ok {
		return
	}
	if sc.info != nil {
		writeErr(w, http.StatusConflict, "this cluster already has a ClusterSync ("+sc.info.Name+") — stop it first")
		return
	}
	if !psmdbHasClusterSync(sc.cfg.OperatorVer) {
		writeErr(w, http.StatusBadRequest, "operator "+sc.cfg.OperatorVer+" has no PerconaServerMongoDBClusterSync (added in "+psmdbClusterSyncMinVer+")")
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	probe := sc.frame
	probe.K3DClusterSync, probe.K3DClusterSyncSource = true, body.Source
	for _, i := range k3dClusterSyncFrameIssues(probe, sc.doc) {
		if i.Level == "error" {
			writeErr(w, http.StatusBadRequest, i.Message)
			return
		}
	}
	ctx, cancel := context.WithTimeout(sc.ctx, 2*time.Minute)
	defer cancel()
	// A ClusterSync DBCanvas did not create (one applied by hand) owns the target's PCSM state;
	// leave both alone. Otherwise any state there is a previous run's — a stop whose cleanup
	// failed, or a hand-made sync deleted without it — and would make the new one crash-loop.
	if out, err := a.kubectl(ctx, sc.serverID, "-n", sc.cfg.Namespace, "get", "psmdb-clustersync", "-o", "name"); err == nil && strings.TrimSpace(out) != "" {
		writeErr(w, http.StatusConflict, "this namespace already has a ClusterSync DBCanvas did not create ("+strings.TrimSpace(out)+") — delete it first")
		return
	}
	if err := a.k3dDropPCSMState(ctx, sc.serverID, sc.cfg); err != nil {
		writeErr(w, http.StatusBadGateway, "clear a previous sync's state on "+sc.cfg.ClusterName+": "+lastLines(err.Error(), 200))
		return
	}
	info, err := a.k3dCreateClusterSync(ctx, sc.st, sc.frame, sc.doc, sc.serverID, sc.cfg, body.Source)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync": info})
}

// k3dDropStateScript drops PCSM's state database on the target as the operator's databaseAdmin.
// Env: NS, SECRET (the users Secret), POD, CONTAINER, DB. POSIX sh: a k3s node image has no bash.
const k3dDropStateScript = `set -e
PW=$(kubectl -n "$NS" get secret "$SECRET" -o jsonpath='{.data.MONGODB_DATABASE_ADMIN_PASSWORD}' | base64 -d)
kubectl -n "$NS" exec "$POD" -c "$CONTAINER" -- mongosh "mongodb://databaseAdmin:$PW@localhost:27017/admin" --quiet \
  --eval "db.getSiblingDB('$DB').dropDatabase().ok"`

// k3dDropPCSMState drops PCSM's state database on the frame's cluster.
func (a *App) k3dDropPCSMState(ctx context.Context, serverID string, cfg k3dConfig) error {
	pod := k3dTargetAdminPod(cfg)
	res, err := a.engCtx(ctx).Exec(ctx, serverID, []string{"sh", "-c", k3dDropStateScript}, []string{
		"NS=" + cfg.Namespace, "SECRET=" + cfg.ClusterName + "-secrets", "POD=" + pod[0], "CONTAINER=" + pod[1], "DB=" + k3dPCSMStateDB,
	})
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("%s", strings.TrimSpace(res.Stderr+" "+res.Stdout))
	}
	return err
}

// POST — stop the sync: delete the custom resource (the operator removes PCSM's Deployment with
// it) and the source Secret, drop PCSM's state on the target so a later sync starts fresh, and
// forget the record. What was copied stays on the target.
func (a *App) handleK3DClusterSyncStop(w http.ResponseWriter, r *http.Request) {
	sc, ok := a.k3dSyncRequest(w, r)
	if !ok {
		return
	}
	if sc.info == nil {
		writeErr(w, http.StatusConflict, "this cluster has no ClusterSync")
		return
	}
	ctx, cancel := context.WithTimeout(sc.ctx, 3*time.Minute)
	defer cancel()
	ns := sc.cfg.Namespace
	if _, err := a.kubectl(ctx, sc.serverID, "-n", ns, "delete", "psmdb-clustersync", sc.info.Name, "--ignore-not-found", "--wait=true", "--timeout=90s"); err != nil {
		writeErr(w, http.StatusBadGateway, "delete the PerconaServerMongoDBClusterSync: "+lastLines(err.Error(), 300))
		return
	}
	a.kubectl(ctx, sc.serverID, "-n", ns, "delete", "secret", sc.info.Name+"-source", "--ignore-not-found") // best-effort
	// The Deployment goes with the CR, but asynchronously: a PCSM still running when its state is
	// dropped would write a fresh heartbeat straight back.
	for i := 0; i < 30; i++ {
		if _, err := a.kubectl(ctx, sc.serverID, "-n", ns, "get", "deploy", sc.info.Name+"-pcsm"); err != nil {
			break
		}
		if !k3dSleep(ctx, 2*time.Second) {
			break
		}
	}
	note := ""
	if err := a.k3dDropPCSMState(ctx, sc.serverID, sc.cfg); err != nil {
		note = "PCSM's state on the target could not be cleared (" + lastLines(err.Error(), 200) + ") — drop the " + k3dPCSMStateDB +
			" database on " + sc.cfg.ClusterName + " before starting another sync (\"Do it by hand\", last step), or it will try to resume this one"
	}
	a.k3dRecordClusterSync(sc.st, sc.doc, sc.frame.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": note})
}

// POST — pause, resume or finalize: spec.mode on the CR, which the operator acts on.
func (a *App) handleK3DClusterSyncMode(w http.ResponseWriter, r *http.Request) {
	sc, ok := a.k3dSyncRequest(w, r)
	if !ok {
		return
	}
	if sc.info == nil {
		writeErr(w, http.StatusConflict, "this cluster has no ClusterSync")
		return
	}
	mode := map[string]string{"pause": "paused", "resume": "running", "finalize": "finalized"}[r.PathValue("action")]
	if mode == "" {
		writeErr(w, http.StatusNotFound, "unknown action "+r.PathValue("action"))
		return
	}
	patch := `{"spec":{"mode":"` + mode + `"}}`
	if _, err := a.kubectl(sc.ctx, sc.serverID, "-n", sc.cfg.Namespace, "patch", "psmdb-clustersync", sc.info.Name, "--type=merge", "-p", patch); err != nil {
		writeErr(w, http.StatusBadGateway, "set mode "+mode+": "+lastLines(err.Error(), 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode})
}
