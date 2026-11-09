package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// k3dpgstandby.go — a Percona Operator for PostgreSQL cluster as a standby of another one, on two
// K3D frames.
//
// An async edge drawn between two K3D frames that both run the "pg" operator makes the one the
// arrow points at a standby cluster: its custom resource carries spec.standby, and the operator
// runs it read-only, replaying from the source. The PXC operator's frame-to-frame link
// (k3drepl.go) is the same line on the canvas and an entirely different mechanism.
//
// The standby streams from the source and, when it can, also reads the source's pgBackRest
// repository — the operator's two standby modes together, which is what Percona documents:
//
//   - **Streaming** (spec.standby.host/port). The standby dials the source's primary Service from
//     another Kubernetes cluster, so the source's primary Service becomes a LoadBalancer (as a PXC
//     source's pods do). The replication connection is authenticated with certificates — the
//     standby presents its own replication client certificate, and the source checks it against
//     its root CA — so the two clusters must share one root CA. That is done without custom TLS
//     secrets: before the standby's cr.yaml is applied, the source's root CA secret is copied into
//     the standby's namespace under the name the operator looks for (<cluster>-cluster-ca-cert,
//     and pgo-root-cacert for operators before 2.5.0). The operator then signs everything the
//     standby needs with the source's CA instead of minting its own.
//   - **Repository** (spec.standby.repoName). When both frames back up to the same bucket of the
//     same SeaweedFS node, the standby's repo1 points at the source's repository path and the
//     standby is bootstrapped from the source's backup and replays its archived WAL. A standby
//     that falls further behind than the source keeps WAL for can then still catch up. Without a
//     shared repository the standby keeps a repository of its own and streams only.
//
// ------------------------------------------------------------------------------- reconciling
//
// The standby relationship is part of how a cluster is created, so a frame deployed with the
// link is simply born a standby (installPGOperator). reconcileK3DPGStandbys, a final deploy phase,
// handles the rest, comparing the canvas with what the frame recorded (k3dConfig.PGStandbyOfID):
//
//	link, built as a standby of that source     → nothing
//	link, built independently or for another one → the cluster is deleted with its volumes and
//	                                               created again as a standby (its data is replaced)
//	no link, built as a standby                  → promoted: spec.standby.enabled = false

// k3dPGStandbyPort is the port the standby dials on the source's primary Service.
const k3dPGStandbyPort = 5432

// k3dPGStandbyCapable reports whether a frame runs the Percona Operator for PostgreSQL.
func k3dPGStandbyCapable(f designFrame) bool { return f.Type == "k3d" && f.K3DOperator == "pg" }

// k3dPGStandbyEdgeFrames resolves a replication edge to two Percona PostgreSQL operator frames.
func k3dPGStandbyEdgeFrames(doc designDoc, e designEdge) (src, dst designFrame, ok bool) {
	if !isReplEdge(e) {
		return designFrame{}, designFrame{}, false
	}
	s, ok1 := k3dFrameByID(doc, e.From.Node)
	d, ok2 := k3dFrameByID(doc, e.To.Node)
	if !ok1 || !ok2 || !k3dPGStandbyCapable(s) || !k3dPGStandbyCapable(d) {
		return designFrame{}, designFrame{}, false
	}
	return s, d, true
}

// k3dPGStandbySourceOf is the frame frameID is a standby of, if any.
func k3dPGStandbySourceOf(doc designDoc, frameID string) (designFrame, bool) {
	for _, e := range doc.Edges {
		if e.Type != "async" {
			continue
		}
		if src, dst, ok := k3dPGStandbyEdgeFrames(doc, e); ok && dst.ID == frameID && src.ID != dst.ID {
			return src, true
		}
	}
	return designFrame{}, false
}

// k3dPGIsStandbySource reports whether some other frame is a standby of frameID.
func k3dPGIsStandbySource(doc designDoc, frameID string) bool {
	for _, e := range doc.Edges {
		if src, dst, ok := k3dPGStandbyEdgeFrames(doc, e); ok && e.Type == "async" && src.ID == frameID && dst.ID != frameID {
			return true
		}
	}
	return false
}

// k3dPGStandbySharesRepo reports whether the standby can read the source's repository: both back
// up to the same bucket of the same SeaweedFS node ("" is that node's first bucket on both).
func k3dPGStandbySharesRepo(src, dst designFrame) bool {
	return src.SeaweedFSNodeID != "" && src.SeaweedFSNodeID == dst.SeaweedFSNodeID &&
		src.SeaweedFSBucket == dst.SeaweedFSBucket
}

// k3dPGStandbyIssues checks one standby link between two Percona PostgreSQL operator frames.
func k3dPGStandbyIssues(doc designDoc, src, dst designFrame, e designEdge, seen map[string]bool) []issue {
	var out []issue
	bad := func(format string, args ...any) {
		out = append(out, issue{Level: "error", Message: fmt.Sprintf(format, args...)})
	}
	warn := func(format string, args ...any) {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf(format, args...)})
	}
	name := src.Label + " → " + dst.Label
	if e.Type != "async" {
		bad("Standby link %s ↔ %s must be one way: a standby cluster follows its source, and only the source takes writes", src.Label, dst.Label)
		return out
	}
	if src.ID == dst.ID {
		bad("A Kubernetes cluster cannot be a standby of itself")
		return out
	}
	if seen[dst.ID] {
		bad("%s is the standby end of more than one link; a standby cluster follows one source", dst.Label)
	}
	seen[dst.ID] = true
	cur, hops := src, 0
	for hops <= len(doc.Frames) {
		up, ok := k3dPGStandbySourceOf(doc, cur.ID)
		if !ok {
			break
		}
		if up.ID == dst.ID {
			bad("The standby links through %s form a loop; one cluster in the chain must be a primary", dst.Label)
			break
		}
		cur, hops = up, hops+1
	}
	if src.K3DPGTDE || dst.K3DPGTDE {
		bad("Standby link %s: pg_tde is not available on a standby link — the standby would need the source's principal key", name)
	}
	// Replication is authenticated by certificate, so the source must trust the CA the standby's
	// replication certificate chains to. A standby without an issuer is given the source's root
	// CA; one with an issuer chains to the Intranet CA, which a source on its own CA does not trust.
	if (k3dIssuerOf(src) == "") != (k3dIssuerOf(dst) == "") {
		bad("Standby link %s: both clusters must issue their certificates the same way — both from the Intranet CA (or OpenBao), or both from the operator's own CA — "+
			"or the source refuses the standby's replication certificate", name)
	}
	if pgLogicalReplicas(dst) > 0 {
		bad("Standby link %s: %s cannot have logical replicas — a standby is read-only and has no publication to subscribe to", name, dst.Label)
	}
	if src.K3DOperatorVer != dst.K3DOperatorVer {
		warn("Standby link %s: the two clusters pin different operator versions, and each version's cr.yaml chooses the PostgreSQL major. A standby needs the source's major", name)
	}
	if k3dCRName(src) == k3dCRName(dst) && k3dPGStandbySharesRepo(src, dst) {
		// Not an error — the repository path is the source's on both — but the two clusters'
		// own scheduled backups would otherwise land in one place.
		warn("Standby link %s: both clusters are called %s, so their repository paths in the shared bucket are the same", name, k3dCRName(src))
	}
	if !strings.EqualFold(k3dExposeOf(src.K3DExposePG, src.K3DExpose), "LoadBalancer") {
		warn("Standby link %s: %s's primary Service will be deployed as LoadBalancer instead of the type set on the frame — a standby in another cluster cannot reach a ClusterIP", name, src.Label)
	}
	if k3dPGStandbySharesRepo(src, dst) {
		out = append(out, issue{Level: "info", Message: fmt.Sprintf(
			"Standby link %s: %s streams from %s and also reads its pgBackRest repository (same SeaweedFS bucket)", name, dst.Label, src.Label)})
	} else {
		warn("Standby link %s: the clusters do not back up to the same SeaweedFS bucket, so %s streams only. If it falls further behind than %s keeps WAL for, it has to be rebuilt; give both frames the same backup store and bucket to let it catch up from the repository", name, dst.Label, src.Label)
	}
	warn("Standby link %s: on deploy, %s is created as a copy of %s — a cluster that is already running is deleted with its volumes and created again — and the operator then holds it read-only", name, dst.Label, src.Label)
	return out
}

// ------------------------------------------------------------------------------ cr.yaml

// pgStandby is spec.standby in a standby cluster's cr.yaml.
type pgStandby struct {
	Host     string
	Port     int
	RepoName string // "" streams only
}

// pgStandbyBlock renders spec.standby (two spaces in, under spec).
func pgStandbyBlock(s *pgStandby) string {
	var b strings.Builder
	b.WriteString("standby:\n  enabled: true\n")
	fmt.Fprintf(&b, "  host: %q\n", s.Host)
	fmt.Fprintf(&b, "  port: %d\n", s.Port)
	if s.RepoName != "" {
		fmt.Fprintf(&b, "  repoName: %s\n", s.RepoName)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// k3dPGRootCASecrets are the names the operator looks a namespace's root CA up by: its own
// (2.5.0+) and the one before it. A copy under both covers every version the catalogue offers.
func k3dPGRootCASecrets(cluster string) []string {
	return []string{cluster + "-cluster-ca-cert", "pgo-root-cacert"}
}

// k3dPGStandbyPrepare runs while a standby frame is being built, before its cr.yaml: it waits
// for the source cluster, makes its primary reachable, copies its root CA, and returns the
// spec.standby to write. sharedRepo is whether the standby's repo1 is the source's. namedIssuer
// is whether the standby issues from the Intranet CA (k3dissuer.go): then both clusters already
// chain to that CA, and a copied root CA would make the operator ignore the Issuer, since it
// prefers a plain root CA Secret it finds to cert-manager.
func (a *App) k3dPGStandbyPrepare(ctx context.Context, st Stack, doc designDoc, src designFrame, serverID string, cfg *k3dConfig, sharedRepo, namedIssuer bool, pr *pxcProg) (*pgStandby, string, error) {
	pr.phase("Waiting for source cluster "+src.Label, 84)
	srcServer, srcCfg, err := a.k3dPGWaitFrame(ctx, st, doc, src)
	if err != nil {
		return nil, "", err
	}
	if err := a.k3dPGWaitReady(ctx, srcServer, srcCfg, pr.logln); err != nil {
		return nil, "", err
	}
	srcNs, srcCluster := srcCfg.Namespace, srcCfg.ClusterName

	// The primary Service, as a LoadBalancer. A source deployed before the link was drawn still
	// has the type its frame chose; changing it is a Service edit the operator applies in place.
	if !strings.EqualFold(srcCfg.ExposePG, "LoadBalancer") {
		pr.logln("exposing " + srcCluster + "'s primary as a LoadBalancer so the standby can reach it")
		if _, err := a.kubectl(ctx, srcServer, "-n", srcNs, "patch", "perconapgcluster", srcCluster, "--type", "merge",
			"-p", `{"spec":{"expose":{"type":"LoadBalancer"}}}`); err != nil {
			return nil, "", fmt.Errorf("expose %s's primary: %w", srcCluster, err)
		}
		a.k3dPGSaveConfig(st, doc, src, func(c *k3dConfig) { c.ExposePG = "LoadBalancer"; c.Expose = "LoadBalancer" })
	}
	host := a.waitForLoadBalancerIP(ctx, srcServer, srcNs, srcCluster+"-ha", 3*time.Minute)
	if host == "" {
		return nil, "", fmt.Errorf("%s-ha has no LoadBalancer address", srcCluster)
	}
	pr.logln(fmt.Sprintf("standby of %s: streaming from %s:%d", srcCluster, host, k3dPGStandbyPort))

	// One root CA for both clusters.
	if namedIssuer {
		pr.logln("root CA not copied from " + srcCluster + ": both clusters issue from the Intranet CA")
	} else if err := a.k3dPGCopyRootCA(ctx, srcServer, srcNs, srcCluster, serverID, cfg, pr); err != nil {
		return nil, "", err
	}

	sb := &pgStandby{Host: host, Port: k3dPGStandbyPort}
	if sharedRepo {
		// The standby is bootstrapped from the source's backup, so there has to be one. The
		// operator takes it by itself once the cluster is up; wait for it rather than letting
		// the standby's restore job fail and retry.
		pr.phase("Waiting for "+srcCluster+"'s first backup", 86)
		a.k3dPGWaitFirstBackup(ctx, srcServer, srcNs, srcCluster, pr.logln)
		sb.RepoName = "repo1"
		pr.logln("standby of " + srcCluster + ": also reading its pgBackRest repository (repo1)")
	}
	return sb, host, nil
}

// k3dPGCopyRootCA copies the source cluster's root CA into the standby's namespace under the
// names the operator looks for, so it signs everything the standby needs with it.
func (a *App) k3dPGCopyRootCA(ctx context.Context, srcServer, srcNs, srcCluster, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	pr.phase("Copying "+srcCluster+"'s root CA", 85)
	var crt, key string
	for _, name := range k3dPGRootCASecrets(srcCluster) {
		c, e1 := a.kubectl(ctx, srcServer, "-n", srcNs, "get", "secret", name, "-o", `jsonpath={.data.root\.crt}`)
		k, e2 := a.kubectl(ctx, srcServer, "-n", srcNs, "get", "secret", name, "-o", `jsonpath={.data.root\.key}`)
		if e1 == nil && e2 == nil && strings.TrimSpace(c) != "" && strings.TrimSpace(k) != "" {
			crt, key = strings.TrimSpace(c), strings.TrimSpace(k)
			break
		}
	}
	if crt == "" {
		return fmt.Errorf("%s has no root CA secret (%s)", srcCluster, strings.Join(k3dPGRootCASecrets(srcCluster), ", "))
	}
	for _, name := range k3dPGRootCASecrets(cfg.ClusterName) {
		manifest := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n  root.crt: %s\n  root.key: %s\n",
			name, cfg.Namespace, crt, key)
		if err := a.kubectlApply(ctx, serverID, cfg.Namespace, []byte(manifest)); err != nil {
			return fmt.Errorf("copy the root CA as %s: %w", name, err)
		}
	}
	pr.logln("root CA copied from " + srcCluster + ": both clusters' certificates chain to it")
	return nil
}

// k3dPGWaitFrame waits for a Percona PostgreSQL operator frame's server to be running and
// returns it with its recorded config.
func (a *App) k3dPGWaitFrame(ctx context.Context, st Stack, doc designDoc, frame designFrame) (string, k3dConfig, error) {
	ids := k3dReplMembers(doc, frame)
	if len(ids) == 0 {
		return "", k3dConfig{}, fmt.Errorf("frame %s has no members", frame.Label)
	}
	deadline := time.Now().Add(deployTimeout())
	for {
		dep, err := a.store.GetDeployment(st.ID, ids[0])
		switch {
		case err == nil && dep.State == DeployRunning && dep.ContainerID != "":
			var cfg k3dConfig
			if err := json.Unmarshal(dep.Config, &cfg); err != nil {
				return "", k3dConfig{}, fmt.Errorf("frame %s has no config: %w", frame.Label, err)
			}
			if cfg.Operator != "pg" || cfg.ClusterName == "" {
				return "", k3dConfig{}, fmt.Errorf("frame %s does not run a Percona PostgreSQL cluster", frame.Label)
			}
			return dep.ContainerID, cfg, nil
		case err == nil && dep.State == DeployError:
			return "", k3dConfig{}, fmt.Errorf("frame %s failed to deploy", frame.Label)
		}
		if time.Now().After(deadline) {
			return "", k3dConfig{}, fmt.Errorf("timed out waiting for frame %s to finish deploying", frame.Label)
		}
		if !k3dSleep(ctx, 5*time.Second) {
			return "", k3dConfig{}, ctx.Err()
		}
	}
}

// k3dPGWaitReady waits for a cluster's custom resource to report ready.
func (a *App) k3dPGWaitReady(ctx context.Context, serverID string, cfg k3dConfig, say func(string)) error {
	deadline := time.Now().Add(deployTimeout())
	last := ""
	for {
		state, ready := a.k3dCRState(ctx, serverID, cfg)
		if ready {
			return nil
		}
		if state != "" && state != last {
			say(cfg.ClusterName + " is " + state)
			last = state
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not report ready (last state %s)", cfg.ClusterName, orDefault(last, "unknown"))
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return ctx.Err()
		}
	}
}

// k3dPGWaitFirstBackup waits for the operator's own first backup of a cluster to its repo1, which
// the PostgresCluster status records. Best effort: an older status without the field, or a
// backup still running when the wait ends, is logged and the standby is created anyway.
func (a *App) k3dPGWaitFirstBackup(ctx context.Context, serverID, ns, cluster string, say func(string)) {
	deadline := time.Now().Add(deployTimeout())
	for {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "postgrescluster", cluster, "-o",
			`jsonpath={.status.pgbackrest.repos[?(@.name=="repo1")].replicaCreateBackupComplete}`)
		if err != nil {
			say("could not read " + cluster + "'s backup status (" + err.Error() + ") — creating the standby anyway")
			return
		}
		if strings.TrimSpace(out) == "true" {
			say(cluster + "'s first backup is in the repository")
			return
		}
		if time.Now().After(deadline) {
			say(cluster + " has no completed backup yet — the standby's restore will retry until there is one")
			return
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return
		}
	}
}

// k3dPGSaveConfig is k3dReplSaveConfig without its PXC-only server lookup.
func (a *App) k3dPGSaveConfig(st Stack, doc designDoc, frame designFrame, edit func(*k3dConfig)) {
	a.k3dReplSaveConfig(st, doc, frame, edit)
}

// k3dPGRecordedStandby is the source frame id a frame was built to follow, read without
// requiring the frame to be running.
func (a *App) k3dPGRecordedStandby(st Stack, doc designDoc, frame designFrame) string {
	for _, id := range k3dReplMembers(doc, frame) {
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) == nil && cfg.PGStandbyOfID != "" {
			return cfg.PGStandbyOfID
		}
	}
	return ""
}

// ------------------------------------------------------------------------------ reconcile

// reconcileK3DPGStandbys makes every Percona PostgreSQL operator frame's standby state match the
// canvas. See the file comment.
func (a *App) reconcileK3DPGStandbys(ctx context.Context, st Stack, doc designDoc) {
	for _, f := range doc.Frames {
		if !k3dPGStandbyCapable(f) || len(k3dReplMembers(doc, f)) == 0 {
			continue
		}
		src, want := k3dPGStandbySourceOf(doc, f.ID)
		recorded := a.k3dPGRecordedStandby(st, doc, f)
		if !want && recorded == "" {
			continue
		}
		if want && recorded == src.ID {
			continue
		}
		if err := a.k3dPGStandbyReconcileOne(ctx, st, doc, f, src, want); err != nil {
			log.Printf("stack %d k3d %s: standby: %v", st.ID, f.Label, err)
			a.k3dReplLogln(st, doc, f, "standby cluster: "+err.Error())
		}
	}
}

func (a *App) k3dPGStandbyReconcileOne(ctx context.Context, st Stack, doc designDoc, f, src designFrame, want bool) error {
	serverID, cfg, err := a.k3dPGWaitFrame(ctx, st, doc, f)
	if err != nil {
		return err
	}
	if !want {
		a.k3dReplLogln(st, doc, f, "no standby link on the canvas — promoting "+cfg.ClusterName)
		if err := a.k3dPGPromote(ctx, serverID, cfg); err != nil {
			return err
		}
		a.k3dPGSaveConfig(st, doc, f, k3dPGClearStandby)
		a.k3dReplLogln(st, doc, f, cfg.ClusterName+" promoted: its primary now takes writes")
		return nil
	}
	return a.k3dPGRebuildAsStandby(ctx, st, doc, f, serverID, cfg)
}

// k3dPGClearStandby forgets a frame's standby relationship.
func k3dPGClearStandby(c *k3dConfig) {
	c.PGStandbyOf, c.PGStandbyOfID, c.PGStandbyHost, c.PGStandbyRepo = "", "", "", ""
}

// k3dPGPromote turns a standby cluster into a primary one.
func (a *App) k3dPGPromote(ctx context.Context, serverID string, cfg k3dConfig) error {
	if _, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "patch", "perconapgcluster", cfg.ClusterName, "--type", "merge",
		"-p", `{"spec":{"standby":{"enabled":false}}}`); err != nil {
		return fmt.Errorf("promote %s: %w", cfg.ClusterName, err)
	}
	return nil
}

// k3dPGRebuildAsStandby deletes a running cluster with its volumes and creates it again from
// installPGOperator, which now finds the link and builds it as a standby.
func (a *App) k3dPGRebuildAsStandby(ctx context.Context, st Stack, doc designDoc, f designFrame, serverID string, cfg k3dConfig) error {
	const phase = "Rebuilding as a standby cluster"
	a.k3dReplConfiguring(st, doc, f, phase)
	defer func() {
		for _, id := range k3dReplMembers(doc, f) {
			a.clearConfiguringIf(st.ID, id, phase)
		}
	}()
	say := func(s string) { a.k3dReplLogln(st, doc, f, s) }
	ns, name := cfg.Namespace, cfg.ClusterName
	say("rebuilding " + name + " as a standby cluster — the cluster and its volumes are deleted and created again")
	if _, err := a.kubectl(ctx, serverID, "-n", ns, "delete", "perconapgcluster", name, "--wait=true", "--timeout=10m"); err != nil &&
		!strings.Contains(err.Error(), "NotFound") {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	// The operator removes its pods with the cluster; the volumes stay unless the delete-pvc
	// finalizer was set, so they are removed here — a new cluster on old volumes would start
	// from the old data.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, _ := a.kubectl(ctx, serverID, "-n", ns, "get", "pods", "-l", "postgres-operator.crunchydata.com/cluster="+name, "-o", "name")
		if strings.TrimSpace(out) == "" {
			break
		}
		if !k3dSleep(ctx, 5*time.Second) {
			return ctx.Err()
		}
	}
	if _, err := a.kubectl(ctx, serverID, "-n", ns, "delete", "pvc", "-l", "postgres-operator.crunchydata.com/cluster="+name, "--wait=true"); err != nil {
		say("removing the old volumes: " + err.Error())
	}
	// The root CA the old cluster minted would be reused as it stands; the source's replaces it.
	for _, s := range k3dPGRootCASecrets(name) {
		a.kubectl(ctx, serverID, "-n", ns, "delete", "secret", s, "--ignore-not-found")
	}

	ids := k3dReplMembers(doc, f)
	pr := &pxcProg{a: a, stackID: st.ID, nodeID: ids[0], p: &provProgress{}, relay: say}
	if err := a.installPGOperator(ctx, st, f, doc, serverID, &cfg, pr); err != nil {
		return err
	}
	a.k3dPGSaveConfig(st, doc, f, func(c *k3dConfig) {
		c.PGStandbyOf, c.PGStandbyOfID, c.PGStandbyHost, c.PGStandbyRepo = cfg.PGStandbyOf, cfg.PGStandbyOfID, cfg.PGStandbyHost, cfg.PGStandbyRepo
	})
	if err := a.k3dPGWaitReady(ctx, serverID, cfg, say); err != nil {
		return err
	}
	say(name + " is a standby of " + cfg.PGStandbyOf)
	return nil
}

// ------------------------------------------------------------------------------ API

// handleK3DPGPromote promotes a standby Percona PostgreSQL cluster and removes its standby link
// from the design, so the next deploy does not rebuild it.
func (a *App) handleK3DPGPromote(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	defer lockStackEdits(st.ID)()
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	f, found := k3dFrameByID(doc, r.PathValue("fid"))
	if !found || !k3dPGStandbyCapable(f) {
		writeErr(w, http.StatusNotFound, "no Percona PostgreSQL operator cluster with that id")
		return
	}
	ids := k3dReplMembers(doc, f)
	if len(ids) == 0 {
		writeErr(w, http.StatusConflict, "the cluster is not deployed")
		return
	}
	dep, err := a.store.GetDeployment(st.ID, ids[0])
	if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
		writeErr(w, http.StatusConflict, "the cluster is not running")
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	if cfg.PGStandbyOfID == "" {
		writeErr(w, http.StatusConflict, cfg.ClusterName+" is not a standby cluster")
		return
	}
	if err := a.k3dPGPromote(r.Context(), dep.ContainerID, cfg); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.k3dPGSaveConfig(st, doc, f, k3dPGClearStandby)
	a.k3dReplLogln(st, doc, f, cfg.ClusterName+" promoted: its primary now takes writes")
	removed := a.dropStandbyEdges(st, doc, f.ID, k3dPGStandbyCapable)
	a.notifyStack(st.ID, "k3d.pg.promoted", "success", "Standby cluster promoted", f.Label+" is now a primary cluster.", "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "promoted", "removedEdges": removed})
}

// dropStandbyEdges removes the async edges into frameID whose source frame passes isSource from
// the stored design, and returns their ids. A promoted standby's link goes with it: left on the
// canvas, the next deploy would rebuild the cluster as a standby again.
func (a *App) dropStandbyEdges(st Stack, doc designDoc, frameID string, isSource func(designFrame) bool) []string {
	removed := []string{}
	d, err := parseRawDesign(st.Design)
	if err != nil {
		return removed
	}
	kept := d.Edges[:0]
	for _, e := range d.Edges {
		to, _ := e["to"].(map[string]any)
		from, _ := e["from"].(map[string]any)
		if mstr(e, "type") == "async" && to != nil && from != nil && mstr(to, "node") == frameID {
			if sf, ok := k3dFrameByID(doc, mstr(from, "node")); ok && isSource(sf) {
				removed = append(removed, mstr(e, "id"))
				continue
			}
		}
		kept = append(kept, e)
	}
	d.Edges = kept
	if len(removed) > 0 {
		if b, err := d.encode(); err == nil {
			a.store.UpdateStack(st.ID, st.Name, b)
		}
	}
	return removed
}
