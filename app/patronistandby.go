package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// patronistandby.go — a Patroni cluster as a standby of another Patroni cluster.
//
// An async edge drawn between two Patroni *frames* makes the one the arrow points at a standby
// cluster of the other: Patroni's standby_cluster setting. The standby's leader (Patroni calls
// it the "standby leader") streams from the source and its own members replicate from it as in
// any Patroni cluster, so the standby is a whole read-only copy with its own failover. Nothing
// is written to it but what arrives from the source.
//
//   - **Where standby_cluster lives.** It is dynamic (DCS) configuration. A frame built from
//     scratch carries it under bootstrap.dcs, so the cluster is born a standby: its first
//     member is cloned from the source with pg_basebackup instead of running initdb. On a
//     running cluster it is read and changed through the REST API's /config.
//   - **Which source host.** Every source member, comma-separated: libpq tries them in turn,
//     and a replica of the source serves a base backup and streams WAL as well as the leader
//     does (cascading replication), so a source failover does not strand the standby.
//   - **No slot.** A slot lives on one member, and with several hosts the standby may connect
//     to one that does not have it. The source keeps wal_keep_size of WAL for it instead
//     (patroniStandbyWalKeep), set on the source by the reconcile below.
//
// ------------------------------------------------------------------------------- reconciling
//
// reconcilePatroniStandbys runs as a final deploy phase, like the replication phases. For each
// Patroni cluster it compares the link on the canvas with the cluster's dynamic configuration:
//
//	link, already a standby of that source     → nothing (or new source hosts, written in place)
//	link, not a standby (or of another cluster) → rebuilt as a standby: its data is replaced
//	no link, still a standby                   → promoted: standby_cluster is removed
//
// The rebuild is destructive, as the Kubernetes replica's seed restore is (k3drepl.go), and for
// the same reason: a cluster with its own history cannot start following another one. Which
// source a cluster was built from is recorded on its members (patroniConfig.StandbyOfID), so a
// standby whose link is redrawn to a different source is rebuilt rather than repointed.

// patroniStandbyWalKeep is the WAL a source keeps for its standby clusters, which stream
// without a slot. The same size the streaming-replication cluster keeps (pgstream.go).
const patroniStandbyWalKeep = "512MB"

// patroniStandby is what a standby cluster's configuration needs from its source.
type patroniStandby struct {
	SourceID    string
	SourceLabel string
	Hosts       []string // every source member's FQDN, by member label
	Port        int
}

// hostList is standby_cluster.host: libpq's comma-separated host list.
func (s *patroniStandby) hostList() string { return strings.Join(s.Hosts, ",") }

// patroniStandbyEdgeFrames resolves a replication edge to two Patroni frames, or reports that it
// is not one. The edge is stored source → standby.
func patroniStandbyEdgeFrames(doc designDoc, e designEdge) (src, dst designFrame, ok bool) {
	if !isReplEdge(e) {
		return designFrame{}, designFrame{}, false
	}
	s, ok1 := k3dFrameByID(doc, e.From.Node)
	d, ok2 := k3dFrameByID(doc, e.To.Node)
	if !ok1 || !ok2 || s.Type != "patroni" || d.Type != "patroni" {
		return designFrame{}, designFrame{}, false
	}
	return s, d, true
}

// patroniStandbySourceOf is the Patroni frame frameID is a standby of, if any.
func patroniStandbySourceOf(doc designDoc, frameID string) (designFrame, bool) {
	for _, e := range doc.Edges {
		if e.Type != "async" {
			continue
		}
		if src, dst, ok := patroniStandbyEdgeFrames(doc, e); ok && dst.ID == frameID {
			return src, true
		}
	}
	return designFrame{}, false
}

// patroniStandbyFor is the standby configuration of a frame, or nil when it is not a standby.
func patroniStandbyFor(doc designDoc, frameID, domain string) *patroniStandby {
	src, ok := patroniStandbySourceOf(doc, frameID)
	if !ok {
		return nil
	}
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == src.ID && n.Type == "patroni" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	s := &patroniStandby{SourceID: src.ID, SourceLabel: src.Label, Port: patroniPGPort}
	for _, n := range members {
		s.Hosts = append(s.Hosts, fqdnOf(hosts[n.ID], domain))
	}
	return s
}

// patroniStandbyDCS renders standby_cluster for bootstrap.dcs (four spaces in).
func patroniStandbyDCS(s *patroniStandby) string {
	var b strings.Builder
	b.WriteString("    standby_cluster:\n")
	fmt.Fprintf(&b, "      host: %s\n", s.hostList())
	fmt.Fprintf(&b, "      port: %d\n", s.Port)
	b.WriteString("      create_replica_methods:\n")
	b.WriteString("      - basebackup\n")
	return b.String()
}

// patroniStandbyIssues checks one standby link. seen collects the standby frames already
// linked, so a second source for one cluster is caught.
func patroniStandbyIssues(doc designDoc, src, dst designFrame, e designEdge, seen map[string]bool) []issue {
	var out []issue
	bad := func(format string, args ...any) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(format, args...)})
	}
	if e.Type != "async" {
		bad("The link between the Patroni clusters %s and %s must be one way: a standby cluster follows its source, and only the source takes writes", src.Label, dst.Label)
		return out
	}
	if src.ID == dst.ID {
		bad("A Patroni cluster cannot be a standby of itself")
		return out
	}
	if seen[dst.ID] {
		bad("Patroni cluster %s is the standby end of more than one link; a standby cluster follows one source", dst.Label)
	}
	seen[dst.ID] = true
	if ppgMajorOf(src.PGMajor) != ppgMajorOf(dst.PGMajor) {
		bad("Standby cluster %s runs PostgreSQL %s and its source %s runs %s; physical replication needs the same major version",
			dst.Label, ppgMajorOf(dst.PGMajor), src.Label, ppgMajorOf(src.PGMajor))
	}
	if patroniBuildName(src.Distribution) != patroniBuildName(dst.Distribution) {
		bad("Standby cluster %s and its source %s use different PostgreSQL builds (%s and %s); a standby replays its source's files and needs the same build",
			dst.Label, src.Label, patroniBuildName(dst.Distribution), patroniBuildName(src.Distribution))
	}
	if src.EnableVault || dst.EnableVault {
		bad("pg_tde (enableVault) is not available on a Patroni standby link (%s → %s): the standby would need the source's principal key", src.Label, dst.Label)
	}
	if dst.UsePgBackRest {
		bad("Standby cluster %s cannot use pgBackRest: its leader is read-only and cannot create a stanza or take a backup. Back up the source %s instead", dst.Label, src.Label)
	}
	// A chain (A → B → C) is fine: Patroni cascades. A loop is not, since nothing in it could
	// ever be written to.
	cur, hops := src, 0
	for hops <= len(doc.Frames) {
		up, ok := patroniStandbySourceOf(doc, cur.ID)
		if !ok {
			break
		}
		if up.ID == dst.ID {
			bad("The Patroni standby links through %s form a loop; one cluster in the chain must be a primary", dst.Label)
			break
		}
		cur, hops = up, hops+1
	}
	var ignored []string
	if dst.PGBenchScale > 0 {
		ignored = append(ignored, "pgbench data")
	}
	if dst.PGVector {
		ignored = append(ignored, "CREATE EXTENSION vector")
	}
	if len(pgFrameExtensions(dst)) > 0 {
		ignored = append(ignored, "CREATE EXTENSION "+pgExtNames(pgFrameExtensions(dst)))
	}
	if len(ignored) > 0 {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf(
			"Standby cluster %s is read-only, so %s is not done there; whatever %s has arrives by replication",
			dst.Label, strings.Join(ignored, ", "), src.Label)})
	}
	return out
}

// distributionLabel names a build for the messages above, where "" means Percona's.
func patroniBuildName(d string) string {
	if d == "" {
		return "percona"
	}
	return d
}

// ------------------------------------------------------------------------------ live state

// patroniDynamicConfig reads a member's view of the cluster's dynamic configuration.
func (a *App) patroniDynamicConfig(ctx context.Context, containerID string) (map[string]any, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c",
		fmt.Sprintf("curl -fsS http://127.0.0.1:%d/config", patroniRESTPort)}, nil)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("GET /config: %s", lastLines(res.Stderr+res.Stdout, 200))
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(res.Stdout), &m); err != nil {
		return nil, fmt.Errorf("GET /config: %w", err)
	}
	return m, nil
}

// patroniPatchConfig merges patch into the cluster's dynamic configuration; a null removes a key.
func (a *App) patroniPatchConfig(ctx context.Context, containerID string, patch map[string]any) error {
	body, _ := json.Marshal(patch)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c",
		fmt.Sprintf(`curl -fsS -XPATCH -H 'Content-Type: application/json' --data "$BODY" http://127.0.0.1:%d/config`, patroniRESTPort)},
		[]string{"BODY=" + string(body)})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("PATCH /config: %s", lastLines(res.Stderr+res.Stdout, 200))
	}
	return nil
}

// standbyHostOf is standby_cluster.host in a dynamic configuration, or "" when there is none.
func standbyHostOf(cfg map[string]any) string {
	sc, _ := cfg["standby_cluster"].(map[string]any)
	if sc == nil {
		return ""
	}
	h, _ := sc["host"].(string)
	return h
}

// patroniRunningMembers is a frame's members with a container, by label.
func (a *App) patroniRunningMembers(st Stack, doc designDoc, frame designFrame) ([]designNode, []Deployment) {
	var ns []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "patroni" {
			ns = append(ns, n)
		}
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].Label < ns[j].Label })
	var members []designNode
	var deps []Deployment
	for _, n := range ns {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		members, deps = append(members, n), append(deps, dep)
	}
	return members, deps
}

// patroniAnyConfig reads the dynamic configuration through the first member that answers.
func (a *App) patroniAnyConfig(ctx context.Context, deps []Deployment) (map[string]any, string, error) {
	var lastErr error
	for _, d := range deps {
		cfg, err := a.patroniDynamicConfig(ctx, d.ContainerID)
		if err == nil {
			return cfg, d.ContainerID, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no member has a container")
	}
	return nil, "", lastErr
}

// ------------------------------------------------------------------------------ reconcile

// reconcilePatroniStandbys makes every Patroni cluster's standby state match the canvas. See the
// file comment for the three cases.
func (a *App) reconcilePatroniStandbys(ctx context.Context, st Stack, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	for _, f := range doc.Frames {
		if f.Type != "patroni" {
			continue
		}
		members, deps := a.patroniRunningMembers(st, doc, f)
		want := patroniStandbyFor(doc, f.ID, domain)
		recorded := patroniRecordedStandby(deps)
		if len(members) == 0 || (want == nil && recorded == "") {
			// Nothing built, or a plain cluster that was never a standby: the common case,
			// and not worth a wait or a REST call.
			continue
		}
		if _, _, err := a.waitPatroniRunning(ctx, st.ID, f, doc, domain, deployTimeout()); err != nil {
			continue // its own provisioning reported why
		}
		members, deps = a.patroniRunningMembers(st, doc, f)
		if err := a.patroniStandbyReconcileOne(ctx, st, doc, f, members, deps, want); err != nil {
			log.Printf("stack %d patroni %s: standby: %v", st.ID, f.Label, err)
			for _, n := range members {
				a.replLogln(st.ID, n.ID, "standby cluster: "+err.Error())
			}
		}
	}
}

// patroniRecordedStandby is the source frame id the members were built to follow, or "".
func patroniRecordedStandby(deps []Deployment) string {
	for _, d := range deps {
		var c patroniConfig
		if json.Unmarshal(d.Config, &c) == nil && c.StandbyOfID != "" {
			return c.StandbyOfID
		}
	}
	return ""
}

func (a *App) patroniStandbyReconcileOne(ctx context.Context, st Stack, doc designDoc, f designFrame,
	members []designNode, deps []Deployment, want *patroniStandby) error {
	say := func(line string) {
		for _, n := range members {
			a.replLogln(st.ID, n.ID, line)
		}
	}
	cfg, cid, err := a.patroniAnyConfig(ctx, deps)
	if err != nil {
		return err
	}
	have := standbyHostOf(cfg)
	recorded := patroniRecordedStandby(deps)

	if want == nil {
		if have == "" {
			if recorded != "" {
				a.patroniSaveStandby(st, members, nil)
			}
			return nil
		}
		say("no standby link on the canvas — promoting " + f.Label + " to a primary cluster")
		if err := a.patroniPatchConfig(ctx, cid, map[string]any{"standby_cluster": nil}); err != nil {
			return fmt.Errorf("promote: %w", err)
		}
		a.patroniSaveStandby(st, members, nil)
		say(f.Label + " promoted: its leader now takes writes")
		return nil
	}

	src, _ := k3dFrameByID(doc, want.SourceID)
	if _, _, err := a.waitPatroniRunning(ctx, st.ID, src, doc, envOr("DOMAIN", "example.net"), deployTimeout()); err != nil {
		return fmt.Errorf("source %s: %w", src.Label, err)
	}
	if err := a.patroniEnsureWalKeep(ctx, st, doc, src); err != nil {
		say("could not set wal_keep_size on " + src.Label + ": " + err.Error())
	}

	switch {
	case have != "" && recorded == want.SourceID:
		if have != want.hostList() {
			say("source members changed — standby_cluster.host is now " + want.hostList())
			if err := a.patroniPatchConfig(ctx, cid, map[string]any{
				"standby_cluster": map[string]any{"host": want.hostList(), "port": want.Port},
			}); err != nil {
				return fmt.Errorf("repoint: %w", err)
			}
		}
		a.patroniSaveStandby(st, members, want)
		return nil
	default:
		return a.patroniRebuildAsStandby(ctx, st, doc, f, members, deps, want)
	}
}

// patroniEnsureWalKeep sets wal_keep_size on a source cluster that has none.
func (a *App) patroniEnsureWalKeep(ctx context.Context, st Stack, doc designDoc, src designFrame) error {
	_, deps := a.patroniRunningMembers(st, doc, src)
	cfg, cid, err := a.patroniAnyConfig(ctx, deps)
	if err != nil {
		return err
	}
	pg, _ := cfg["postgresql"].(map[string]any)
	params, _ := pg["parameters"].(map[string]any)
	if v, ok := params["wal_keep_size"]; ok && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" {
		return nil
	}
	return a.patroniPatchConfig(ctx, cid, map[string]any{
		"postgresql": map[string]any{"parameters": map[string]any{"wal_keep_size": patroniStandbyWalKeep}},
	})
}

// patroniSaveStandby records on every member which cluster it follows (nil: none).
func (a *App) patroniSaveStandby(st Stack, members []designNode, s *patroniStandby) {
	id, label := "", ""
	if s != nil {
		id, label = s.SourceID, s.SourceLabel
	}
	for _, n := range members {
		kv := map[string]any{"standbyOfId": id, "standbyOf": label}
		if s == nil {
			if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil {
				var c patroniConfig
				if json.Unmarshal(dep.Config, &c) == nil && c.Role == "standby leader" {
					kv["role"] = "leader"
				}
			}
		}
		a.persistConfigKeys(st, n.ID, kv)
	}
}

// patroniRebuildAsStandby replaces a running cluster's data with a copy of its source and
// leaves it following that source: Patroni is stopped everywhere, the cluster's keys are
// removed from etcd and its data directories emptied, every member gets a configuration whose
// bootstrap says standby_cluster, and Patroni is started again — which then bootstraps the
// cluster the way a new standby is bootstrapped.
func (a *App) patroniRebuildAsStandby(ctx context.Context, st Stack, doc designDoc, f designFrame,
	members []designNode, deps []Deployment, want *patroniStandby) error {
	const phase = "Rebuilding as a standby cluster"
	for _, n := range members {
		a.setConfiguring(st.ID, n.ID, phase)
	}
	defer func() {
		for _, n := range members {
			a.clearConfiguringIf(st.ID, n.ID, phase)
		}
	}()
	say := func(line string) {
		for _, n := range members {
			a.replLogln(st.ID, n.ID, line)
		}
	}
	say(fmt.Sprintf("rebuilding %s as a standby of %s — its data is replaced with a copy of %s", f.Label, want.SourceLabel, want.SourceLabel))

	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	var etcdEndpoints []string
	for _, n := range members {
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), etcdClientPort))
	}
	sec := pgFamilySecrets()
	major := ppgMajorOf(f.PGMajor)
	dataDir := pgDataDir(f.OS, major)

	for _, d := range deps {
		if err := a.runStep(ctx, d.ContainerID, `systemctl stop patroni`, nil, say); err != nil {
			return fmt.Errorf("stop patroni: %w", err)
		}
	}
	scope := sanitizeName(f.Label)
	if err := a.runStep(ctx, deps[0].ContainerID,
		`ETCDCTL_API=3 etcdctl --endpoints=http://127.0.0.1:2379 del --prefix "/dbcanvas/$SCOPE/" >/dev/null`,
		[]string{"SCOPE=" + scope}, say); err != nil {
		return fmt.Errorf("clear the cluster's keys in etcd: %w", err)
	}
	for i, n := range members {
		host := hosts[n.ID]
		yml := patroniYAML(f, host, fqdnOf(host, domain), etcdEndpoints, sec, want)
		if err := a.engCtx(ctx).CopyFile(ctx, deps[i].ContainerID, "/etc/patroni", "postgresql.yml", 0o644, []byte(yml)); err != nil {
			return fmt.Errorf("%s: write patroni config: %w", n.Label, err)
		}
		if err := a.runStep(ctx, deps[i].ContainerID, `set -e
find "$DATADIR" -mindepth 1 -delete
chown postgres:postgres "$DATADIR"
chmod 700 "$DATADIR"`, []string{"DATADIR=" + dataDir}, say); err != nil {
			return fmt.Errorf("%s: empty the data directory: %w", n.Label, err)
		}
	}
	for _, d := range deps {
		if err := a.runStep(ctx, d.ContainerID, patroniStartScript, nil, say); err != nil {
			return fmt.Errorf("start patroni: %w", err)
		}
	}
	leaderID, err := a.patroniWaitCluster(ctx, st, f, members, deployTimeout())
	if err != nil {
		return err
	}
	a.patroniSaveStandby(st, members, want)
	for _, n := range members {
		role := "replica"
		if n.ID == leaderID {
			role = "standby leader"
		}
		a.persistConfigKeys(st, n.ID, map[string]any{"role": role})
	}
	say(fmt.Sprintf("%s is a standby of %s (streaming from %s)", f.Label, want.SourceLabel, want.hostList()))
	return nil
}

// ------------------------------------------------------------------------------ API

// patroniStandbyView is GET …/patroni/standby: what the canvas asks for and what runs.
type patroniStandbyView struct {
	// Source is the cluster the canvas makes this one a standby of ("" for none).
	Source string `json:"source"`
	// Replicas are the clusters that are standbys of this one.
	Replicas []string `json:"replicas"`
	// Standby is whether the running cluster's configuration has standby_cluster.
	Standby bool   `json:"standby"`
	Host    string `json:"host,omitempty"`
	// Members is Patroni's /cluster: each member's role, state and lag.
	Members json.RawMessage `json:"members,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func (a *App) patroniFrameFromRequest(w http.ResponseWriter, r *http.Request) (Stack, designDoc, designFrame, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, designDoc{}, designFrame{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return Stack{}, designDoc{}, designFrame{}, false
	}
	f, found := k3dFrameByID(doc, r.PathValue("fid"))
	if !found || f.Type != "patroni" {
		writeErr(w, http.StatusNotFound, "Patroni cluster not found")
		return Stack{}, designDoc{}, designFrame{}, false
	}
	return st, doc, f, true
}

// handlePatroniStandby reports a Patroni cluster's standby link and its live state.
func (a *App) handlePatroniStandby(w http.ResponseWriter, r *http.Request) {
	st, doc, f, ok := a.patroniFrameFromRequest(w, r)
	if !ok {
		return
	}
	view := patroniStandbyView{Replicas: []string{}}
	if src, ok := patroniStandbySourceOf(doc, f.ID); ok {
		view.Source = src.Label
	}
	for _, e := range doc.Edges {
		if s, d, ok := patroniStandbyEdgeFrames(doc, e); ok && s.ID == f.ID && e.Type == "async" {
			view.Replicas = append(view.Replicas, d.Label)
		}
	}
	_, deps := a.patroniRunningMembers(st, doc, f)
	if len(deps) > 0 {
		cfg, cid, err := a.patroniAnyConfig(r.Context(), deps)
		if err != nil {
			view.Error = err.Error()
		} else {
			view.Host = standbyHostOf(cfg)
			view.Standby = view.Host != ""
			if res, err := a.engCtx(r.Context()).Exec(r.Context(), cid, []string{"bash", "-c",
				fmt.Sprintf("curl -fsS http://127.0.0.1:%d/cluster", patroniRESTPort)}, nil); err == nil && res.Code == 0 && json.Valid([]byte(res.Stdout)) {
				view.Members = json.RawMessage(res.Stdout)
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePatroniPromote promotes a standby cluster and removes its standby link from the design,
// so the next deploy does not rebuild it as a standby again.
func (a *App) handlePatroniPromote(w http.ResponseWriter, r *http.Request) {
	st, doc, f, ok := a.patroniFrameFromRequest(w, r)
	if !ok {
		return
	}
	defer lockStackEdits(st.ID)()
	members, deps := a.patroniRunningMembers(st, doc, f)
	if len(deps) == 0 {
		writeErr(w, http.StatusConflict, "the cluster is not deployed")
		return
	}
	cfg, cid, err := a.patroniAnyConfig(r.Context(), deps)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if standbyHostOf(cfg) == "" {
		writeErr(w, http.StatusConflict, f.Label+" is not a standby cluster")
		return
	}
	if err := a.patroniPatchConfig(r.Context(), cid, map[string]any{"standby_cluster": nil}); err != nil {
		writeErr(w, http.StatusBadGateway, "promote: "+err.Error())
		return
	}
	a.patroniSaveStandby(st, members, nil)
	for _, n := range members {
		a.replLogln(st.ID, n.ID, f.Label+" promoted: its leader now takes writes")
	}

	// The link goes with it: left on the canvas, the next deploy would read the cluster as a
	// standby that has drifted and rebuild it from its old source.
	removed := a.dropStandbyEdges(st, doc, f.ID, func(sf designFrame) bool { return sf.Type == "patroni" })
	a.notifyStack(st.ID, "patroni.promoted", "success", "Standby cluster promoted", f.Label+" is now a primary cluster.", "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "promoted", "removedEdges": removed})
}
