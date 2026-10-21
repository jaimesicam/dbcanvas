package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// lcclustersync.go — driving Percona ClusterSync for MongoDB (PCSM) from a Linux Client.
//
// linuxclient_db.go installs `pcsm` on a Linux Client and leaves it there, stopped: it needs a
// source and a target, and those are the stack's to offer. This is the rest — pick two MongoDB
// clusters in the stack, and the node runs a sync between them that the panel can watch, pause,
// resume, finalize and check.
//
// What a start does, in order:
//
//  1. Checks the pair against what PCSM supports (pcsmPairIssue): replica set → replica set,
//     sharded → sharded, sharded → replica set from 1.0.0; the same major or one of the
//     documented upgrade paths (6.0 → 7.0 / 8.0, 7.0 → 8.0); never a downgrade.
//  2. Creates a dedicated account on each side with the roles PCSM's authentication page lists —
//     backup, clusterMonitor, readAnyDatabase on the source; restore, clusterMonitor,
//     clusterManager, readWriteAnyDatabase on the target — rather than handing PCSM the stack's
//     root account. Done with mongosh on one of the cluster's own members (they all carry it),
//     which also reads back the server version for the minimum-version check (6.0.17 / 7.0.13 /
//     8.0.0).
//  3. Points the packaged pcsm.service at the pair through a drop-in, so the distro's own
//     environment file (/etc/sysconfig/pcsm on EL, /etc/default/pcsm on Debian — it ships
//     placeholder URIs) is left alone and the URIs, which carry passwords, live in a root-only
//     file of ours. Restart=on-failure, which the packaged unit lacks.
//  4. Clears PCSM's recovery state on the target when asked (`pcsm reset`) — a target that was
//     synced to before still has it, and PCSM would otherwise try to resume that run — then
//     restarts the service and sends `pcsm start`.
//
// Progress is PCSM's own GET /status, read on the node (the HTTP API listens on localhost only),
// plus the unit's state and the tail of its journal. The panel derives rates and a lag history
// from successive polls; nothing here keeps a history.

// pcsmEndpoint is a MongoDB cluster in the stack that a sync can read from or write to.
type pcsmEndpoint struct {
	ID       string `json:"id"` // the Sample Code target id ("<frameID>#frame", or a mongos node id)
	Label    string `json:"label"`
	Topology string `json:"topology"` // "replset" | "sharded"
	Major    string `json:"major"`
	Product  string `json:"product"`
	Hosts    string `json:"hosts"` // for the picker: where it is
	// ReplicaSet is the set name for a replica set ("" through a mongos) — for the by-hand
	// instructions' connection strings.
	ReplicaSet string `json:"replicaSet,omitempty"`

	target scTarget
}

// pcsmEndpoints lists the stack's running MongoDB clusters a sync can use. A replica set is
// offered as the set (PCSM needs every electable member, which is what the set target lists); a
// sharded cluster through its mongos. Single members and standalones are left out: PCSM reads
// change streams, which a standalone mongod has none of.
func (a *App) pcsmEndpoints(st Stack, clientOS string) []pcsmEndpoint {
	var out []pcsmEndpoint
	seenSharded := map[string]bool{}
	doc := buildDoc(st)
	for _, t := range a.scStackTargets(st, clientOS) {
		if t.Engine != scMongoDB {
			continue
		}
		e := pcsmEndpoint{ID: t.ID, Label: t.Label, Major: t.Major, Product: t.Product, target: t}
		switch {
		case t.Role == "router":
			// One mongos per sharded cluster is enough; the first running one stands for it.
			fid := nodeByID(doc, t.NodeID).FrameID
			if seenSharded[fid] {
				continue
			}
			seenSharded[fid] = true
			e.Topology = "sharded"
			if f := frameByID(doc, fid); f.ID != "" {
				e.Label = f.Label + " (sharded, via " + nodeByID(doc, t.NodeID).Label + ")"
			}
		case t.ReplicaSet != "" && strings.HasSuffix(t.ID, "#frame"):
			e.Topology = "replset"
		default:
			continue
		}
		e.Hosts = strings.Join(t.Addresses(), ",")
		if e.Topology == "replset" {
			e.ReplicaSet = t.ReplicaSet
		}
		out = append(out, e)
	}
	return out
}

// pcsmMinVersion is the oldest server of each major PCSM supports, from its deployment page.
var pcsmMinVersion = map[string]string{"6.0": "6.0.17", "7.0": "7.0.13", "8.0": "8.0.0"}

// pcsmUpgradePaths are the cross-version syncs PCSM documents; anything else must match majors.
var pcsmUpgradePaths = map[string][]string{"6.0": {"7.0", "8.0"}, "7.0": {"8.0"}}

// pcsmPairIssue says why a sync from src to dst cannot run, or "" when it can. pcsmVer is the
// installed PCSM ("" when unknown — the newest rules apply).
func pcsmPairIssue(src, dst pcsmEndpoint, pcsmVer string) string {
	switch {
	case src.ID == dst.ID:
		return "the source and the target are the same cluster"
	case src.Topology == "replset" && dst.Topology == "sharded":
		return "PCSM syncs a replica set only to a replica set — this target is sharded"
	case src.Topology == "sharded" && dst.Topology == "replset" && pcsmVer != "" && compareVersions(pcsmVer, "1.0.0") < 0:
		return "a sharded source into a replica set needs PCSM 1.0.0 or newer — this node has " + pcsmVer
	}
	if src.Major == dst.Major {
		if _, ok := pcsmMinVersion[src.Major]; !ok && src.Major != "" {
			return "PCSM supports MongoDB 6.0, 7.0 and 8.0 — this pair runs " + src.Major
		}
		return ""
	}
	if slices.Contains(pcsmUpgradePaths[src.Major], dst.Major) {
		return ""
	}
	if compareVersions(src.Major, dst.Major) > 0 {
		return "PCSM does not sync to an older major (" + src.Major + " → " + dst.Major + ")"
	}
	return "PCSM has no " + src.Major + " → " + dst.Major + " path — it supports the same major, or 6.0 → 7.0 / 8.0 and 7.0 → 8.0"
}

// pcsmSync is what the node is set to sync, kept on its deployment under "clusterSync" so the
// panel can say what is running without asking the node.
type pcsmSync struct {
	Source    pcsmSide `json:"source"`
	Target    pcsmSide `json:"target"`
	Include   []string `json:"include,omitempty"`
	Exclude   []string `json:"exclude,omitempty"`
	StartedAt int64    `json:"startedAt"`
}

type pcsmSide struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Topology string `json:"topology"`
	Version  string `json:"version"` // the server's own db.version(), read at start
	User     string `json:"user"`
}

const (
	pcsmSourceUser = "pcsm-source"
	pcsmTargetUser = "pcsm-target"
	pcsmEnvFile    = "/etc/dbcanvas/pcsm.env"
)

var (
	pcsmSourceRoles = []string{"backup", "clusterMonitor", "readAnyDatabase"}
	pcsmTargetRoles = []string{"restore", "clusterMonitor", "clusterManager", "readWriteAnyDatabase"}
)

// pcsmURI is a connection string for t as user — every member and the set name for a replica
// set, the router for a sharded cluster.
func pcsmURI(t scTarget, user, pass string) string {
	u := url.URL{Scheme: "mongodb", User: url.UserPassword(user, pass), Host: strings.Join(t.Addresses(), ","), Path: "/"}
	q := url.Values{"authSource": {"admin"}}
	if t.ReplicaSet != "" && t.Role != "router" {
		q.Set("replicaSet", t.ReplicaSet)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// pcsmUserScript creates (or re-passwords) the sync account on a cluster and prints the server
// version, run with mongosh on one of the cluster's own members as its admin.
const pcsmUserScript = `set -e
mongosh "$ADMIN_URI" --quiet --eval '
const a = db.getSiblingDB("admin"), u = process.env.PCSM_USER, p = process.env.PCSM_PW;
const roles = JSON.parse(process.env.PCSM_ROLES);
if (a.getUser(u)) a.updateUser(u, { pwd: p, roles: roles }); else a.createUser({ user: u, pwd: p, roles: roles });
print("VERSION=" + db.version());'`

// pcsmEnsureUser runs pcsmUserScript against side, returning the server version.
func (a *App) pcsmEnsureUser(ctx context.Context, st Stack, t scTarget, user, pass string, roles []string) (string, error) {
	dep, err := a.store.GetDeployment(st.ID, t.NodeID)
	if err != nil || dep.ContainerID == "" {
		return "", fmt.Errorf("%s is not running", t.Label)
	}
	rolesJSON, _ := json.Marshal(roles)
	out, err := a.execScript(withEngine(ctx, a.depEngine(st, t.NodeID)), dep.ContainerID, pcsmUserScript, []string{
		"ADMIN_URI=" + pcsmURI(t, t.User, t.Password),
		"PCSM_USER=" + user, "PCSM_PW=" + pass, "PCSM_ROLES=" + string(rolesJSON),
	})
	if err != nil {
		return "", fmt.Errorf("create %s on %s: %v", user, t.Label, lastLines(err.Error(), 300))
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "VERSION="); ok {
			return v, nil
		}
	}
	return "", nil
}

// pcsmConfigureScript points pcsm.service at the pair and (re)starts it, then sends `pcsm start`.
// Env: SOURCE_URI, TARGET_URI, FRESH (1 → pcsm reset first), START_ARGS.
const pcsmConfigureScript = `set -e
command -v pcsm >/dev/null || { echo "pcsm is not installed on this node"; exit 1; }
install -d -m 0700 /etc/dbcanvas
umask 077
printf 'PCSM_SOURCE_URI="%s"\nPCSM_TARGET_URI="%s"\n' "$SOURCE_URI" "$TARGET_URI" > ` + pcsmEnvFile + `
install -d -m 0755 /etc/systemd/system/pcsm.service.d
cat > /etc/systemd/system/pcsm.service.d/dbcanvas.conf <<'EOF'
# Written by DBCanvas: the sync the node's panel set up. The distro's own environment file is
# read first and holds placeholders; this one is read after it, so it wins.
[Service]
EnvironmentFile=` + pcsmEnvFile + `
Restart=on-failure
RestartSec=5
EOF
systemctl daemon-reload
systemctl stop pcsm 2>/dev/null || true
if [ "$FRESH" = "1" ]; then
  pcsm reset --target "$TARGET_URI" >/tmp/pcsm-reset.log 2>&1 || { tail -3 /tmp/pcsm-reset.log; echo "pcsm reset failed"; exit 1; }
fi
systemctl enable pcsm >/dev/null 2>&1 || true
systemctl reset-failed pcsm 2>/dev/null || true
systemctl start pcsm
for i in $(seq 1 30); do
  pcsm status >/dev/null 2>&1 && break
  sleep 1
done
pcsm status >/dev/null 2>&1 || { journalctl -u pcsm -n 5 --no-pager -o cat; echo "pcsm did not come up"; exit 1; }
pcsm start $START_ARGS 2>&1 | tail -3
echo "STARTED"`

// pcsmStatusScript is the node's view: PCSM's /status, the unit's state and its log tail.
const pcsmStatusScript = `echo "ACTIVE=$(systemctl is-active pcsm 2>/dev/null)"
echo "---STATUS"
curl -fsS --max-time 3 http://localhost:2242/status 2>/dev/null || pcsm status 2>/dev/null || true
echo
echo "---LOG"
journalctl -u pcsm -n 40 --no-pager -o cat 2>/dev/null | tail -40`

// pcsmCompareScript counts every user collection on one side, as JSON {"db.coll": n}.
const pcsmCompareScript = `mongosh "$ADMIN_URI" --quiet --eval '
const out = {};
for (const d of db.adminCommand({ listDatabases: 1, nameOnly: true }).databases) {
  if (["admin", "config", "local"].includes(d.name) || d.name.startsWith("percona_")) continue;
  const x = db.getSiblingDB(d.name);
  for (const c of x.getCollectionInfos({ type: "collection" })) {
    if (c.name.startsWith("system.")) continue;
    out[d.name + "." + c.name] = x.getCollection(c.name).countDocuments({});
  }
}
print("JSON=" + JSON.stringify(out));'`

// ---------------------------------------------------------------- HTTP

// pcsmRequest resolves a request to a running Linux Client that has pcsm.
func (a *App) pcsmRequest(w http.ResponseWriter, r *http.Request) (Stack, Deployment, linuxClientConfig, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, Deployment{}, linuxClientConfig{}, false
	}
	nid := r.PathValue("nid")
	if nodeTypeOf(st, nid) != "linuxclient" {
		writeErr(w, http.StatusBadRequest, "ClusterSync runs on a Linux Client node")
		return Stack{}, Deployment{}, linuxClientConfig{}, false
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return Stack{}, Deployment{}, linuxClientConfig{}, false
	}
	a.stampEngine(r, st, nid)
	dep = a.reconcileContainerID(r.Context(), st.ID, nid, dep)
	var cfg linuxClientConfig
	json.Unmarshal(dep.Config, &cfg)
	return st, dep, cfg, true
}

// pcsmVersionOf is the pcsm a node reported at deploy, without the leading "v" ("" when absent).
func pcsmVersionOf(cfg linuxClientConfig) string {
	for _, c := range cfg.DBClients {
		if c.Tool == lcToolClusterSync.ID {
			return strings.TrimPrefix(c.Version, "v")
		}
	}
	return ""
}

// pcsmSyncOf reads the node's recorded sync, if any.
func pcsmSyncOf(dep Deployment) *pcsmSync {
	var m struct {
		ClusterSync *pcsmSync `json:"clusterSync"`
	}
	json.Unmarshal(dep.Config, &m)
	return m.ClusterSync
}

// GET — what the node has, what it could sync, and how the current sync is doing.
func (a *App) handlePCSMState(w http.ResponseWriter, r *http.Request) {
	st, dep, cfg, ok := a.pcsmRequest(w, r)
	if !ok {
		return
	}
	ver := pcsmVersionOf(cfg)
	eps := a.pcsmEndpoints(st, cfg.OS)
	// Every ordered pair, with the reason it cannot run if it cannot — the panel greys those out
	// with the reason as their hint rather than letting the server refuse them.
	type pair struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Issue  string `json:"issue,omitempty"`
	}
	pairs := []pair{}
	for _, s := range eps {
		for _, t := range eps {
			if s.ID != t.ID {
				pairs = append(pairs, pair{s.ID, t.ID, pcsmPairIssue(s, t, ver)})
			}
		}
	}
	resp := map[string]any{
		"installed": ver != "", "version": ver, "endpoints": eps, "pairs": pairs,
		"sync": pcsmSyncOf(dep),
		// For the by-hand instructions: the package's environment file is OS-specific, and the
		// accounts' roles are PCSM's documented ones.
		"os": cfg.OS, "sourceUser": pcsmSourceUser, "targetUser": pcsmTargetUser,
		"sourceRoles": pcsmSourceRoles, "targetRoles": pcsmTargetRoles,
	}
	if ver != "" {
		out, _ := a.execScript(r.Context(), dep.ContainerID, pcsmStatusScript, nil)
		active, status, logLines := pcsmParseState(out)
		resp["service"] = active
		resp["status"] = status
		resp["log"] = logLines
	}
	writeJSON(w, http.StatusOK, resp)
}

// pcsmParseState splits pcsmStatusScript's output into its three parts.
func pcsmParseState(out string) (active string, status json.RawMessage, logLines []string) {
	head, rest, _ := strings.Cut(out, "---STATUS")
	statusPart, logPart, _ := strings.Cut(rest, "---LOG")
	active = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(head), "ACTIVE="))
	if s := strings.TrimSpace(statusPart); json.Valid([]byte(s)) && s != "" {
		status = json.RawMessage(s)
	}
	return active, status, pcsmLogLines(logPart)
}

// ansiRe matches the colour escapes pcsm writes to its log even under journald.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

// pcsmLogLines splits a log tail into non-empty lines without colour escapes.
func pcsmLogLines(out string) []string {
	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(ansiRe.ReplaceAllString(out, "")), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// POST — start a sync. Body: {source, target, include[], exclude[], fresh}.
func (a *App) handlePCSMStart(w http.ResponseWriter, r *http.Request) {
	st, dep, cfg, ok := a.pcsmRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Source  string   `json:"source"`
		Target  string   `json:"target"`
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
		Fresh   *bool    `json:"fresh"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ver := pcsmVersionOf(cfg)
	if ver == "" {
		writeErr(w, http.StatusConflict, "pcsm is not installed on this node — turn on Percona ClusterSync for MongoDB in its design and redeploy, or install it from the terminal")
		return
	}
	var src, dst pcsmEndpoint
	for _, e := range a.pcsmEndpoints(st, cfg.OS) {
		if e.ID == body.Source {
			src = e
		}
		if e.ID == body.Target {
			dst = e
		}
	}
	if src.ID == "" || dst.ID == "" {
		writeErr(w, http.StatusBadRequest, "pick a running MongoDB replica set or sharded cluster for both the source and the target")
		return
	}
	if why := pcsmPairIssue(src, dst, ver); why != "" {
		writeErr(w, http.StatusBadRequest, why)
		return
	}
	include, err := pcsmNamespaces(body.Include)
	if err == nil {
		body.Exclude, err = pcsmNamespaces(body.Exclude)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	srcPW, dstPW := genSecret("Pcsm"), genSecret("Pcsm")
	srcVer, err := a.pcsmEnsureUser(ctx, st, src.target, pcsmSourceUser, srcPW, pcsmSourceRoles)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	dstVer, err := a.pcsmEnsureUser(ctx, st, dst.target, pcsmTargetUser, dstPW, pcsmTargetRoles)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	for _, s := range []struct{ label, ver string }{{src.Label, srcVer}, {dst.Label, dstVer}} {
		if min, ok := pcsmMinVersion[majorOf(s.ver)]; ok && compareVersions(s.ver, min) < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s runs MongoDB %s; PCSM needs %s or newer in that series", s.label, s.ver, min))
			return
		}
	}

	var args []string
	if len(include) > 0 {
		args = append(args, "--include-namespaces="+strings.Join(include, ","))
	}
	if len(body.Exclude) > 0 {
		args = append(args, "--exclude-namespaces="+strings.Join(body.Exclude, ","))
	}
	fresh := body.Fresh == nil || *body.Fresh
	out, err := a.execScript(ctx, dep.ContainerID, pcsmConfigureScript, []string{
		"SOURCE_URI=" + pcsmURI(src.target, pcsmSourceUser, srcPW),
		"TARGET_URI=" + pcsmURI(dst.target, pcsmTargetUser, dstPW),
		"FRESH=" + boolEnv(fresh), "START_ARGS=" + strings.Join(args, " "),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, lastLines(err.Error(), 400))
		return
	}
	if !strings.Contains(out, "STARTED") || strings.Contains(strings.ToLower(out), "error") {
		writeErr(w, http.StatusBadGateway, "pcsm start: "+lastLines(out, 400))
		return
	}
	sync := pcsmSync{
		Source:  pcsmSide{ID: src.ID, Label: src.Label, Topology: src.Topology, Version: srcVer, User: pcsmSourceUser},
		Target:  pcsmSide{ID: dst.ID, Label: dst.Label, Topology: dst.Topology, Version: dstVer, User: pcsmTargetUser},
		Include: include, Exclude: body.Exclude, StartedAt: time.Now().Unix(),
	}
	a.persistConfigKey(st, dep.NodeID, "clusterSync", sync)
	a.persistSecretKeys(st, dep.NodeID, map[string]any{"pcsmSourcePassword": srcPW, "pcsmTargetPassword": dstPW})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync": sync})
}

// pcsmNamespaces cleans a namespace list: "db.*" or "db.collection", no blanks, no shell.
func pcsmNamespaces(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		for _, ns := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			db, coll, ok := strings.Cut(ns, ".")
			if !ok || db == "" || coll == "" || strings.ContainsAny(ns, "'\"`$;&|<>\\") {
				return nil, fmt.Errorf("%q is not a namespace — write db.* or db.collection", ns)
			}
			out = append(out, ns)
		}
	}
	return out, nil
}

// majorOf is "8.0" from "8.0.12-4".
func majorOf(v string) string {
	f := strings.SplitN(v, ".", 3)
	if len(f) < 2 {
		return v
	}
	return f[0] + "." + f[1]
}

// POST — pause, resume or finalize the running sync: PCSM's own commands, sent on the node.
func (a *App) handlePCSMAction(w http.ResponseWriter, r *http.Request) {
	_, dep, _, ok := a.pcsmRequest(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	cmd := map[string]string{
		"pause": "pcsm pause", "resume": "pcsm resume", "finalize": "pcsm finalize",
		// A failed sync resumes from where it stopped rather than from the start.
		"resume-failed": "pcsm resume --from-failure",
	}[action]
	if cmd == "" {
		writeErr(w, http.StatusNotFound, "unknown action "+action)
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, cmd+" 2>&1", nil)
	if err != nil || strings.Contains(out, `"ok":false`) || strings.Contains(strings.ToLower(out), "error") {
		msg := out
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, http.StatusBadGateway, "pcsm "+action+": "+lastLines(msg, 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(out)})
}

// POST — stop the service and forget the sync, leaving whatever was copied on the target.
func (a *App) handlePCSMStop(w http.ResponseWriter, r *http.Request) {
	st, dep, _, ok := a.pcsmRequest(w, r)
	if !ok {
		return
	}
	if _, err := a.execScript(r.Context(), dep.ContainerID, "systemctl stop pcsm 2>/dev/null; systemctl reset-failed pcsm 2>/dev/null; true", nil); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.persistConfigKey(st, dep.NodeID, "clusterSync", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET — document counts per collection on both sides, to see the copy is complete.
func (a *App) handlePCSMCompare(w http.ResponseWriter, r *http.Request) {
	st, dep, cfg, ok := a.pcsmRequest(w, r)
	if !ok {
		return
	}
	sync := pcsmSyncOf(dep)
	if sync == nil {
		writeErr(w, http.StatusConflict, "no sync has been started on this node")
		return
	}
	var src, dst scTarget
	for _, e := range a.pcsmEndpoints(st, cfg.OS) {
		if e.ID == sync.Source.ID {
			src = e.target
		}
		if e.ID == sync.Target.ID {
			dst = e.target
		}
	}
	if src.ID == "" || dst.ID == "" {
		writeErr(w, http.StatusConflict, "the source or the target is not running")
		return
	}
	count := func(t scTarget) (map[string]int64, error) {
		dep, err := a.store.GetDeployment(st.ID, t.NodeID)
		if err != nil {
			return nil, err
		}
		out, err := a.execScript(withEngine(r.Context(), a.depEngine(st, t.NodeID)), dep.ContainerID, pcsmCompareScript,
			[]string{"ADMIN_URI=" + pcsmURI(t, t.User, t.Password)})
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.Label, lastLines(err.Error(), 200))
		}
		m := map[string]int64{}
		for _, l := range strings.Split(out, "\n") {
			if j, ok := strings.CutPrefix(strings.TrimSpace(l), "JSON="); ok {
				json.Unmarshal([]byte(j), &m)
			}
		}
		return m, nil
	}
	sc, err := count(src)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	tc, err := count(dst)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	type row struct {
		Namespace string `json:"namespace"`
		Source    *int64 `json:"source"`
		Target    *int64 `json:"target"`
	}
	var names []string
	for k := range sc {
		names = append(names, k)
	}
	for k := range tc {
		if _, ok := sc[k]; !ok {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	rows := []row{}
	for _, k := range names {
		rw := row{Namespace: k}
		if v, ok := sc[k]; ok {
			rw.Source = &v
		}
		if v, ok := tc[k]; ok {
			rw.Target = &v
		}
		rows = append(rows, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "at": time.Now().Unix()})
}

// persistSecretKeys is persistSecretKey for several keys in one write.
func (a *App) persistSecretKeys(st Stack, nodeID string, kv map[string]any) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	m := map[string]any{}
	if len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &m)
	}
	for k, v := range kv {
		m[k] = v
	}
	if b, e := json.Marshal(m); e == nil {
		dep.Secrets = b
		a.store.UpsertDeployment(dep)
	}
}
