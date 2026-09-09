package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// k3drepl.go — cross-cluster replication between two PXC-operator Kubernetes clusters.
//
// A replication edge drawn between two K3D *frames* (not members: a Kubernetes cluster's identity
// on the canvas is its frame) makes the left one a replication source and the right one a replica
// of it. It is the manual procedure from Percona's "Restore to a new cluster" and "Replication"
// pages, done by the app:
//
//   1. the source's cr.yaml declares `replicationChannels: [{name, isSource: true}]` (k3dcr.go)
//      and its database pods are exposed per-pod on a LoadBalancer, which is what gives a replica
//      somewhere to connect to;
//   2. after both clusters are up, a PerconaXtraDBClusterBackup is taken on the source;
//   3. its `status.destination` seeds a PerconaXtraDBClusterRestore on the replica, pointed at the
//      *source's* bucket through `backupSource` (a restore inside one cluster names a backup
//      object; across clusters there is no such object, so the S3 location is spelled out);
//   4. and only then is the replica's own `replicationChannels` patched in, with the source's
//      per-pod addresses as its `sourcesList`.
//
// ------------------------------------------------------------------------------- what waits, and why
//
// The replica cluster does not wait for anything before deploying. Both clusters are provisioned
// concurrently by the frame loop; this runs as a final phase afterwards, beside
// reconcileReplication (its non-Kubernetes sibling, replication.go).
//
// The one long wait is step 2 — a backup takes minutes. It strictly dominates the wait people
// expect to have to manage, which is the source's LoadBalancer addresses: MetalLB assigns those
// within seconds of the Services being created, so by the time there is a `destination` string to
// restore from they have been readable for minutes. That is why step 4 *reads* them
// (k3dReplSourceHosts) instead of predicting them. There is nothing to pin and no race to lose.
//
// ------------------------------------------------------------------------------- ordering that does matter
//
// Step 4 is deliberately last, and is the reason the replica's cr.yaml ships without a channel.
// A cluster carrying `isSource: false` on its first reconcile attaches with AUTO_POSITION to a GTID
// set whose binary logs the source has long purged (its own pods joined by SST), and the IO thread
// stops with error 1236. Restoring first gives the replica the source's GTID history, and the
// channel then attaches to a set the source can still serve.
//
// ------------------------------------------------------------------------------- seeding happens once
//
// The seed is destructive — it replaces the replica's data with the source's. Every later deploy
// reconciles the *channel* (adding one that is new to the canvas, removing one that is gone) and
// never restores again: the destination it restored from is recorded on the replica's members
// (k3dConfig.ReplSeededFrom) and its presence is what makes this idempotent. Re-seeding is a
// deliberate act — the Replication tab's button — not a side effect of pressing Deploy.

const (
	// k3dReplPort is the port a replica dials on the source's per-pod Services. The operator
	// exposes 3306 there whatever the Service type.
	k3dReplPort = 3306
	// k3dReplWeight is every source's weight in a replica's sourcesList. They are equal on
	// purpose: the list is a set of interchangeable Galera nodes, not a preference order.
	k3dReplWeight = 100
	// k3dReplSeedName / k3dReplRestoreName prefix the Kubernetes objects the seed is made of. The
	// pair of cluster names is appended (k3dReplObjectName) so a stack with more than one link does
	// not have two backups fighting over one name.
	k3dReplSeedName    = "dbcanvas-seed"
	k3dReplRestoreName = "dbcanvas-seed-restore"
)

// k3dReplLink is one directed source → replica relationship between two Kubernetes frames.
type k3dReplLink struct {
	Src, Dst designFrame
	Channel  string // the MySQL channel name, identical on both ends
}

// k3dReplCapable reports whether a frame is a Kubernetes cluster that can take part in one of
// these links: the PXC operator is the only one of the six with cross-cluster replication in its
// custom resource.
func k3dReplCapable(f designFrame) bool { return f.Type == "k3d" && f.K3DOperator == "pxc" }

// k3dFrameByID finds a frame by id.
func k3dFrameByID(doc designDoc, id string) (designFrame, bool) {
	for _, f := range doc.Frames {
		if f.ID == id {
			return f, true
		}
	}
	return designFrame{}, false
}

// k3dReplChannelName is the channel both ends use, derived from the two cluster names so it is
// stable across redeploys and readable in SHOW REPLICA STATUS. MySQL channel names allow rather
// less than a Kubernetes name does, so everything outside [A-Za-z0-9_] becomes '_'.
func k3dReplChannelName(src, dst designFrame) string {
	safe := func(s string) string {
		var sb strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
				sb.WriteRune(r)
			} else {
				sb.WriteByte('_')
			}
		}
		return sb.String()
	}
	return safe(k3dCRName(src)) + "_to_" + safe(k3dCRName(dst))
}

// k3dReplObjectName builds a Kubernetes object name for one link.
//
// A channel name and a Kubernetes name are not the same alphabet, and this is where that bites:
// MySQL channel names take underscores and k3dReplChannelName uses them, but a Kubernetes object
// name is an RFC 1123 subdomain — lower case, alphanumerics and '-', starting and ending
// alphanumeric. Naming the seed backup after the channel had the API server reject it outright
// ("dbcanvas-seed-cluster1_to_cluster2 is invalid"), which is why this derives from the cluster
// names directly and enforces the rule rather than assuming the input already meets it. It is
// capped well short of the 253-character limit because the operator builds its Job and Pod names
// on top of this one.
func k3dReplObjectName(prefix string, src, dst designFrame) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("-")
	b.WriteString(k3dCRName(src))
	b.WriteString("-to-")
	b.WriteString(k3dCRName(dst))

	var out strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(b.String()) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && out.Len() > 0 {
			out.WriteByte('-')
			prevDash = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	if name == "" {
		name = prefix // every prefix here is already a legal name
	}
	return name
}

// k3dReplLinks expands the design's replication edges into directed Kubernetes links. Endpoints
// must be two *different* PXC-operator K3D frames; anything else belongs to replication.go's
// node-to-node links and is left alone.
//
// "bidir" is deliberately not expanded into two links. The operator marks a cluster that carries an
// `isSource: false` channel read-only, so replicating both ways would leave a topology with nowhere
// to write; and a seed can only run in one direction anyway. The canvas does not offer it, and this
// treats one arriving (a hand-edited design, an imported template) as the async link it draws.
func k3dReplLinks(doc designDoc) []k3dReplLink {
	var out []k3dReplLink
	seen := map[string]bool{}
	for _, e := range doc.Edges {
		if !isReplEdge(e) {
			continue
		}
		src, ok1 := k3dFrameByID(doc, e.From.Node)
		dst, ok2 := k3dFrameByID(doc, e.To.Node)
		if !ok1 || !ok2 || !k3dReplCapable(src) || !k3dReplCapable(dst) || src.ID == dst.ID {
			continue
		}
		if seen[src.ID+"|"+dst.ID] {
			continue
		}
		seen[src.ID+"|"+dst.ID] = true
		out = append(out, k3dReplLink{Src: src, Dst: dst, Channel: k3dReplChannelName(src, dst)})
	}
	return out
}

// k3dReplRole reports what a frame is in the stack's Kubernetes replication topology: "source",
// "replica", "" — with the channel it does it on. A frame that is somehow both (A → B → C) is
// reported as a source, because that is the part that has to be baked into its cr.yaml; its replica
// half is patched in later like any other and needs nothing at build time.
func k3dReplRole(doc designDoc, frameID string) (role, channel string) {
	links := k3dReplLinks(doc)
	for _, l := range links {
		if l.Src.ID == frameID {
			return "source", l.Channel
		}
	}
	for _, l := range links {
		if l.Dst.ID == frameID {
			return "replica", l.Channel
		}
	}
	return "", ""
}

// ---------------------------------------------------------------- design-time validation

// k3dReplEdgeEnds are the two ends of a replication edge, as far as they resolve to K3D frames.
// Either may be absent — an edge with one Kubernetes end and one bare-metal end is a mistake worth
// naming precisely, so it is claimed here rather than left to the member-pair rule.
type k3dReplEdgeEnds struct {
	Src, Dst designFrame
	SrcOK    bool
	DstOK    bool
}

// k3dReplEdgeFrames reports whether a replication edge touches a K3D frame at all, and resolves
// whichever ends do. ok is false for an edge between two cluster *members*, which belongs to
// replication.go.
func k3dReplEdgeFrames(doc designDoc, e designEdge) (k3dReplEdgeEnds, bool) {
	var ends k3dReplEdgeEnds
	ends.Src, ends.SrcOK = k3dFrameByID(doc, e.From.Node)
	ends.Dst, ends.DstOK = k3dFrameByID(doc, e.To.Node)
	ends.SrcOK = ends.SrcOK && ends.Src.Type == "k3d"
	ends.DstOK = ends.DstOK && ends.Dst.Type == "k3d"
	return ends, ends.SrcOK || ends.DstOK
}

// k3dReplPerPodServices is how many per-pod LoadBalancer addresses a PXC-operator source needs: one
// per database pod. cr.yaml's `size: 3` is not one of the values crTransform rewrites, so a
// cluster is three pods — stated here as a named constant so the pool arithmetic below reads.
const k3dReplPerPodServices = 3

// k3dReplIssues validates one Kubernetes replication link at design time. `pairs` carries the links
// already seen, so a duplicate is reported once.
func k3dReplIssues(doc designDoc, ends k3dReplEdgeEnds, e designEdge, pairs map[string]bool) []issue {
	var out []issue
	err := func(msg string) { out = append(out, issue{Level: "error", Message: msg}) }
	warn := func(msg string) { out = append(out, issue{Level: "warning", Message: msg}) }

	if !ends.SrcOK || !ends.DstOK {
		err("A replication link on a Kubernetes cluster must connect it to another Kubernetes cluster — " +
			"an operator-managed cluster replicates through its custom resource, not from a node outside it")
		return out
	}
	src, dst := ends.Src, ends.Dst
	if src.ID == dst.ID {
		err("Replication link " + src.Label + " ↔ " + dst.Label + " must connect two different clusters")
		return out
	}
	key, rev := src.ID+"|"+dst.ID, dst.ID+"|"+src.ID
	if pairs[key] || pairs[rev] {
		err("Duplicate replication link between " + src.Label + " and " + dst.Label)
		return out
	}
	pairs[key] = true

	for _, f := range []designFrame{src, dst} {
		if f.K3DOperator != "pxc" {
			err("Replication link " + src.Label + " → " + dst.Label + ": " + f.Label +
				" must run the Percona XtraDB Cluster operator — it is the only operator whose cluster can replicate from another one")
			return out
		}
	}
	if src.K3DOperatorVer != dst.K3DOperatorVer {
		warn("Replication link " + src.Label + " → " + dst.Label +
			": the two clusters pin different operator versions, so their PXC images may differ — replicating into an older server can fail on a newer server's binary log events")
	}
	// The seed is a backup on one cluster restored onto the other, so both ends need a store: the
	// source to write it and the replica to read it back with its own credentials.
	if src.SeaweedFSNodeID == "" || dst.SeaweedFSNodeID == "" {
		err("Replication link " + src.Label + " → " + dst.Label +
			": both clusters need a SeaweedFS backup store — the replica is seeded from a backup of the source, and there is nowhere to put one")
	} else if src.SeaweedFSNodeID != dst.SeaweedFSNodeID {
		err("Replication link " + src.Label + " → " + dst.Label +
			": both clusters must use the same SeaweedFS node — the replica restores from the source's bucket, which it can only reach on the store it has credentials for")
	}
	// The source's database pods each take an address out of the frame's own MetalLB block, on top
	// of whatever its proxy tier takes. Running the block dry leaves Services Pending, which looks
	// like a hung deploy rather than a design that did not fit.
	if need := k3dReplPerPodServices + k3dProxyLBServices(src); need > k3dPoolSize {
		err(fmt.Sprintf("Replication link %s → %s: %s would need %d LoadBalancer addresses (%d database pods plus its proxy) "+
			"but a Kubernetes cluster's block is %d — put the proxy on a ClusterIP",
			src.Label, dst.Label, src.Label, need, k3dReplPerPodServices, k3dPoolSize))
	}

	if !strings.EqualFold(k3dExposeOf(src.K3DExposePXC, src.K3DExpose), "LoadBalancer") {
		warn("Replication link " + src.Label + " → " + dst.Label + ": " + src.Label +
			"'s database Service will be deployed as LoadBalancer instead of the type set on the frame — a replica in another cluster cannot reach a ClusterIP")
	}
	warn("Replication link " + src.Label + " → " + dst.Label + ": on deploy, " + dst.Label +
		" is restored from a backup of " + src.Label + " — anything already in it is replaced — and the operator then holds it read-only")
	if e.Type == "bidir" {
		warn("Replication link " + src.Label + " ↔ " + dst.Label +
			": bidirectional is not available between Kubernetes clusters (the operator holds a replica read-only, which would leave nowhere to write) — it will be set up one way, " +
			src.Label + " → " + dst.Label)
	}
	return out
}

// k3dProxyLBServices is how many LoadBalancer addresses a frame's proxy tier takes. HAProxy
// publishes two Services (primary and replicas); ProxySQL one.
func k3dProxyLBServices(f designFrame) int {
	expose := k3dExposeOf(f.K3DExposeHAProxy, f.K3DExpose)
	if k3dProxy(f) == "proxysql" {
		expose = k3dExposeOf(f.K3DExposeProxySQL, f.K3DExpose)
	}
	if !strings.EqualFold(expose, "LoadBalancer") {
		return 0
	}
	if k3dProxy(f) == "proxysql" {
		return 1
	}
	return 2
}

// ---------------------------------------------------------------- the reconciler

// reconcileK3DReplication is the deploy's final phase for Kubernetes replication links, run beside
// reconcileReplication. Each link is independent, so one failing (or timing out) leaves the others
// alone; nothing here is fatal to the deploy, because by the time it runs both clusters are up and
// usable — an unlinked pair is a worse stack, not a broken one.
func (a *App) reconcileK3DReplication(ctx context.Context, st Stack, doc designDoc) {
	links := k3dReplLinks(doc)
	// Not `len(links) == 0 → return`: taking the *last* link off the canvas leaves no links and a
	// cluster still replicating, and that is exactly the case that has to reach the prune below.
	// A stack with no PXC-operator Kubernetes clusters at all has nothing to do either way.
	capable := false
	for _, f := range doc.Frames {
		capable = capable || k3dReplCapable(f)
	}
	if !capable {
		return
	}
	for _, l := range links {
		if ctx.Err() != nil {
			return
		}
		if err := a.k3dReplLink(ctx, st, doc, l); err != nil {
			log.Printf("stack %d: k3d replication %s → %s: %v", st.ID, l.Src.Label, l.Dst.Label, err)
			a.k3dReplLogln(st, doc, l.Dst, "replication setup failed: "+err.Error())
		}
	}
	// Channels whose edge has been taken off the canvas are stopped. Done after the additions so a
	// link that was merely redrawn the other way round is not torn down and rebuilt.
	a.k3dReplPrune(ctx, st, doc, links)
}

// k3dReplLink brings one link up: wait, seed (once), then attach the channel.
func (a *App) k3dReplLink(ctx context.Context, st Stack, doc designDoc, l k3dReplLink) error {
	// Both frames first, and this really is a wait rather than a lookup: the deploy hands every
	// frame to a goroutine and returns, so when this phase starts the clusters it is about to link
	// have not been *created* yet, let alone become ready. Resolving straight to a container here
	// failed the link within a second of pressing Deploy, on the first live run of this code.
	srcServer, srcCfg, err := a.k3dReplWaitFrame(ctx, st, doc, l.Src)
	if err != nil {
		return fmt.Errorf("source cluster %s: %w", l.Src.Label, err)
	}
	dstServer, dstCfg, err := a.k3dReplWaitFrame(ctx, st, doc, l.Dst)
	if err != nil {
		return fmt.Errorf("replica cluster %s: %w", l.Dst.Label, err)
	}

	say := func(msg string) { a.k3dReplLogln(st, doc, l.Dst, msg) }

	// ---- 1. both clusters ready ----
	say(fmt.Sprintf("replication %s → %s: waiting for both clusters to be ready", l.Src.Label, l.Dst.Label))
	if err := a.k3dWaitPXCReady(ctx, srcServer, srcCfg.Namespace, srcCfg.ClusterName, deployTimeout()); err != nil {
		return fmt.Errorf("source %s never became ready: %w", srcCfg.ClusterName, err)
	}
	if err := a.k3dWaitPXCReady(ctx, dstServer, dstCfg.Namespace, dstCfg.ClusterName, deployTimeout()); err != nil {
		return fmt.Errorf("replica %s never became ready: %w", dstCfg.ClusterName, err)
	}

	// ---- 2 + 3. seed, once ----
	if dstCfg.ReplSeededFrom == "" {
		dest, err := a.k3dReplSeed(ctx, st, doc, l, srcServer, srcCfg, dstServer, dstCfg, say)
		if err != nil {
			return err
		}
		dstCfg.ReplSeededFrom = dest
		a.k3dReplSaveConfig(st, doc, l.Dst, func(c *k3dConfig) { c.ReplSeededFrom = dest })
		// The restore pauses and resumes the cluster; it is not ready the instant it reports
		// Succeeded.
		if err := a.k3dWaitPXCReady(ctx, dstServer, dstCfg.Namespace, dstCfg.ClusterName, deployTimeout()); err != nil {
			return fmt.Errorf("replica %s did not come back after the restore: %w", dstCfg.ClusterName, err)
		}
	} else {
		say("already seeded from " + dstCfg.ReplSeededFrom + " — reconciling the channel only")
	}

	// ---- 4. the channel ----
	hosts, err := a.k3dReplSourceHosts(ctx, srcServer, srcCfg.Namespace, srcCfg.ClusterName)
	if err != nil {
		return err
	}
	say(fmt.Sprintf("channel %s ← %s", l.Channel, strings.Join(hosts, ", ")))
	if err := a.k3dReplPatchChannel(ctx, dstServer, dstCfg.Namespace, dstCfg.ClusterName, l.Channel, hosts); err != nil {
		return fmt.Errorf("patch the replica's replicationChannels: %w", err)
	}
	a.k3dReplSaveConfig(st, doc, l.Dst, func(c *k3dConfig) {
		c.ReplRole, c.ReplChannel, c.ReplPeer, c.ReplSources = "replica", l.Channel, k3dCRName(l.Src), hosts
	})
	a.k3dReplSaveConfig(st, doc, l.Src, func(c *k3dConfig) {
		c.ReplRole, c.ReplChannel, c.ReplPeer = "source", l.Channel, k3dCRName(l.Dst)
	})

	// ---- 5. say whether it actually runs ----
	// The operator reconciles the channel on its own schedule, and its first attempts routinely
	// fail while the cluster settles ("get primary pxc pod: connection refused"). So this polls
	// rather than reading once, and reports what it found either way — a link that is not running
	// yet is worth saying out loud, but is not a failure of the deploy.
	if status := a.k3dReplWaitRunning(ctx, dstServer, dstCfg.Namespace, dstCfg.ClusterName, l.Channel, 5*time.Minute); status != "" {
		say("replication " + status)
	}
	return nil
}

// k3dReplWaitFrame blocks until a frame's server node reports running, then resolves it. The
// deployment row is the rendezvous — provisionK3DFrame writes DeployRunning on every member only
// once the operator is installed and its custom resource applied — so waiting on it is what makes
// this phase independent of how long a cluster takes to build.
func (a *App) k3dReplWaitFrame(ctx context.Context, st Stack, doc designDoc, frame designFrame) (string, k3dConfig, error) {
	ids := k3dReplMembers(doc, frame)
	if len(ids) == 0 {
		return "", k3dConfig{}, fmt.Errorf("frame %s has no members", frame.Label)
	}
	deadline := time.Now().Add(deployTimeout())
	for {
		dep, err := a.store.GetDeployment(st.ID, ids[0])
		switch {
		case err == nil && dep.State == DeployRunning:
			return a.k3dReplServer(st, doc, frame)
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

// k3dReplSeed takes a backup on the source and restores it onto the replica, returning the S3
// destination it used. This is the long step.
func (a *App) k3dReplSeed(ctx context.Context, st Stack, doc designDoc, l k3dReplLink,
	srcServer string, srcCfg k3dConfig, dstServer string, dstCfg k3dConfig, say func(string)) (string, error) {

	if srcCfg.BackupRepo == "" || dstCfg.BackupRepo == "" {
		return "", fmt.Errorf("both clusters need a SeaweedFS backup store to seed a replica (%s: %q, %s: %q)",
			l.Src.Label, srcCfg.BackupRepo, l.Dst.Label, dstCfg.BackupRepo)
	}

	// ---- the backup, on the source ----
	name := k3dReplObjectName(k3dReplSeedName, l.Src, l.Dst)
	restoreName := k3dReplObjectName(k3dReplRestoreName, l.Src, l.Dst)
	say("taking a seed backup on " + srcCfg.ClusterName + " (this is the long part)")
	// The backup object's name is deterministic per link, so a re-seed would otherwise apply over
	// a Succeeded object from the last one, change nothing, and be handed that old backup's
	// destination — restoring the replica to where the source was an hour ago while reporting a
	// fresh seed. Pressing Re-seed means "make this replica match the source *now*", so the
	// previous object goes first and a genuinely new backup is taken. Deleting the object does not
	// touch what is in S3: the operator only removes that when the delete-backup finalizer is set,
	// and cr.yaml ships it commented out.
	_, _ = a.kubectl(ctx, srcServer, "-n", srcCfg.Namespace, "delete", "pxc-backup", name, "--ignore-not-found")
	backup := fmt.Sprintf(`apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterBackup
metadata:
  name: %s
spec:
  pxcCluster: %s
  storageName: %s
`, name, srcCfg.ClusterName, crStorageName)
	if err := a.kubectlApply(ctx, srcServer, srcCfg.Namespace, []byte(backup)); err != nil {
		return "", fmt.Errorf("create the seed backup: %w", err)
	}
	src, err := a.k3dWaitBackup(ctx, srcServer, srcCfg.Namespace, name, deployTimeout(), say)
	if err != nil {
		return "", err
	}
	say("seed backup finished: " + src.Destination)

	// ---- the restore, on the replica ----
	restore, err := k3dReplRestoreManifest(src, restoreName, dstCfg.ClusterName, dstCfg.ClusterName+"-backup-s3")
	if err != nil {
		return "", fmt.Errorf("build the seed restore: %w", err)
	}

	say("restoring it onto " + dstCfg.ClusterName + " (the cluster pauses while this runs)")
	// A restore object from an earlier attempt would be reconciled as already-finished, so the name
	// is reused only after the old one is gone.
	_, _ = a.kubectl(ctx, dstServer, "-n", dstCfg.Namespace, "delete", "pxc-restore", restoreName, "--ignore-not-found")
	if err := a.kubectlApply(ctx, dstServer, dstCfg.Namespace, restore); err != nil {
		return "", fmt.Errorf("create the seed restore: %w", err)
	}
	if err := a.k3dWaitRestore(ctx, dstServer, dstCfg.Namespace, restoreName, deployTimeout(), say); err != nil {
		return "", err
	}
	say("restore finished — " + dstCfg.ClusterName + " now carries " + srcCfg.ClusterName + "'s data and GTID history")
	return src.Destination, nil
}

// k3dReplRestoreManifest builds the replica's PerconaXtraDBClusterRestore from the source backup's
// status.
//
// Across clusters there is no backup *object* to name — `spec.backupName` only resolves inside one
// cluster — so the S3 location is spelled out in `spec.backupSource`, which has exactly the same
// schema as a backup's `status`. So it is built from what the source wrote rather than retyped, and
// `s3` is copied wholesale: that is what carries `forcePathStyle`, which SeaweedFS needs because it
// does not do virtual-host bucket addressing, and `prefix`, and whatever the next operator release
// adds, without this having to learn about any of them.
//
// Two deliberate departures from a straight copy:
//
//   - credentialsSecret becomes the *replica's* secret. Both clusters' secrets are seeded from the
//     same .env, so it opens the source's bucket; naming the source's would name a secret that
//     does not exist in this cluster.
//   - storageName and the two ssl secret names are dropped. They name resources in the source's
//     cluster: a storageName would resolve against the replica's OWN backup storage and quietly
//     restore it from the wrong bucket, and the ssl secrets are simply not there.
//
// An incomplete `s3` is rejected by the operator with "nil s3 backup status storage", and no
// restore Job is ever created.
func k3dReplRestoreManifest(src k3dReplBackupSource, name, cluster, credentialsSecret string) ([]byte, error) {
	if src.Destination == "" {
		return nil, fmt.Errorf("the seed backup reported no destination")
	}
	if len(src.S3) == 0 {
		return nil, fmt.Errorf("the seed backup reported no S3 location")
	}
	s3 := map[string]any{}
	for k, v := range src.S3 {
		s3[k] = v
	}
	s3["credentialsSecret"] = credentialsSecret
	backupSource := map[string]any{"destination": src.Destination, "s3": s3}
	if src.VerifyTLS != nil {
		backupSource["verifyTLS"] = *src.VerifyTLS
	}
	return json.Marshal(map[string]any{
		"apiVersion": "pxc.percona.com/v1",
		"kind":       "PerconaXtraDBClusterRestore",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"pxcCluster": cluster, "backupSource": backupSource},
	})
}

// ---------------------------------------------------------------- kubectl helpers

// k3dReplBackupSource is where a finished backup landed, read off the backup object's status.
//
// S3 is carried as the raw object rather than a handful of named fields on purpose: a restore's
// `spec.backupSource` has exactly the same schema as a backup's `status`, so the honest thing to
// hand the replica is what the source wrote, whatever is in it. That is what carries
// `forcePathStyle` — which SeaweedFS needs, because it does not do virtual-host bucket addressing
// — and `prefix`, and whatever the next operator release adds, without this having to learn about
// any of them.
type k3dReplBackupSource struct {
	Destination string
	VerifyTLS   *bool
	S3          map[string]any
}

// k3dWaitPXCReady blocks until a PerconaXtraDBCluster reports state "ready".
func (a *App) k3dWaitPXCReady(ctx context.Context, serverID, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pxc", name, "-o", "jsonpath={.status.state}")
		if err == nil {
			last = strings.TrimSpace(out)
			if last == "ready" {
				return nil
			}
			if last == "error" {
				return fmt.Errorf("cluster %s is in state %q", name, last)
			}
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for %s to be ready (last state %q)", name, last)
}

// k3dWaitBackup blocks until a PerconaXtraDBClusterBackup succeeds, and reads where it put itself.
func (a *App) k3dWaitBackup(ctx context.Context, serverID, ns, name string, timeout time.Duration, say func(string)) (k3dReplBackupSource, error) {
	deadline := time.Now().Add(timeout)
	said := ""
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return k3dReplBackupSource{}, ctx.Err()
		}
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pxc-backup", name, "-o", "json")
		if err == nil {
			var obj struct {
				Status struct {
					State       string         `json:"state"`
					Destination string         `json:"destination"`
					Error       string         `json:"error"`
					VerifyTLS   *bool          `json:"verifyTLS"`
					S3          map[string]any `json:"s3"`
				} `json:"status"`
			}
			if json.Unmarshal([]byte(out), &obj) == nil {
				st := obj.Status
				if st.State != said && st.State != "" {
					said = st.State
					say("seed backup: " + st.State)
				}
				switch st.State {
				case "Succeeded":
					if st.Destination == "" {
						return k3dReplBackupSource{}, fmt.Errorf("backup %s succeeded without a destination", name)
					}
					return k3dReplBackupSource{Destination: st.Destination, VerifyTLS: st.VerifyTLS, S3: st.S3}, nil
				case "Failed":
					return k3dReplBackupSource{}, fmt.Errorf("seed backup %s failed: %s", name, st.Error)
				}
			}
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return k3dReplBackupSource{}, ctx.Err()
		}
	}
	return k3dReplBackupSource{}, fmt.Errorf("timed out waiting for the seed backup %s", name)
}

// k3dWaitRestore blocks until a PerconaXtraDBClusterRestore succeeds.
func (a *App) k3dWaitRestore(ctx context.Context, serverID, ns, name string, timeout time.Duration, say func(string)) error {
	deadline := time.Now().Add(timeout)
	said := ""
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pxc-restore", name, "-o", "json")
		if err == nil {
			var obj struct {
				Status struct {
					State    string `json:"state"`
					Comments string `json:"comments"`
				} `json:"status"`
			}
			if json.Unmarshal([]byte(out), &obj) == nil {
				st := obj.Status
				if st.State != said && st.State != "" {
					said = st.State
					say("seed restore: " + st.State)
				}
				switch st.State {
				case "Succeeded":
					return nil
				case "Failed":
					return fmt.Errorf("seed restore %s failed: %s", name, st.Comments)
				}
			}
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for the seed restore %s", name)
}

// k3dReplSourceHosts reads the addresses a replica should dial: the external address of each
// per-pod Service the operator creates when `pxc.expose` is on.
//
// This is the step the whole "wait for the LoadBalancer IPs" question is really about, and by the
// time it runs there is nothing to wait for — the seed backup above took minutes and MetalLB
// assigns within seconds of the Service appearing. The short retry is for the case where this runs
// without a seed (an already-seeded redeploy) against a cluster that has only just come up.
func (a *App) k3dReplSourceHosts(ctx context.Context, serverID, ns, cluster string) ([]string, error) {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "svc",
			"-l", "app.kubernetes.io/instance="+cluster, "-o", "json")
		if err == nil {
			if hosts := k3dReplHostsFromServices(out); len(hosts) > 0 {
				return hosts, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no per-pod address for %s — is `pxc.expose` set to LoadBalancer or NodePort?", cluster)
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return nil, ctx.Err()
		}
	}
}

// k3dReplHostsFromServices picks the per-pod Services out of `kubectl get svc -o json` and returns
// the address each is reachable at, sorted so a redeploy produces the same list in the same order.
//
// Everything the cluster owns is fetched and the discrimination is done here, on the one property
// that actually means "this Service is one database pod": a `statefulset.kubernetes.io/pod-name`
// selector. Narrowing the kubectl query by label instead is what it looked like this should do, and
// it is wrong — the operator labels these `app.kubernetes.io/component: external-service`, not
// `pxc`, so a `component=pxc` query returns only the headless `<cluster>-pxc` Service and its
// `-unready` twin, and this reported that a LoadBalancer cluster had no addresses at all. The
// broad query also picks up the proxy tier, which this filter then drops for the same reason the
// headless pair is dropped: no pod-name selector, not a database pod.
func k3dReplHostsFromServices(js string) []string {
	var list struct {
		Items []struct {
			Spec struct {
				Type      string            `json:"type"`
				Selector  map[string]string `json:"selector"`
				ClusterIP string            `json:"clusterIP"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP       string `json:"ip"`
						Hostname string `json:"hostname"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(js), &list) != nil {
		return nil
	}
	var hosts []string
	for _, it := range list.Items {
		if it.Spec.Selector["statefulset.kubernetes.io/pod-name"] == "" {
			continue
		}
		switch it.Spec.Type {
		case "LoadBalancer":
			for _, in := range it.Status.LoadBalancer.Ingress {
				if in.IP != "" {
					hosts = append(hosts, in.IP)
				} else if in.Hostname != "" {
					hosts = append(hosts, in.Hostname)
				}
			}
		case "ClusterIP":
			// Reachable from inside this cluster only — useless to a replica in another one, and
			// saying so is better than handing over an address that will never answer.
			continue
		}
	}
	sort.Strings(hosts)
	return hosts
}

// k3dReplPatchChannel sets the replica's `replicationChannels` to exactly this one channel.
//
// A merge patch replaces the whole list rather than appending to it, which is what makes this
// reconcile rather than accumulate: a source that has moved leaves no stale entry behind.
func (a *App) k3dReplPatchChannel(ctx context.Context, serverID, ns, cluster, channel string, hosts []string) error {
	type source struct {
		Host   string `json:"host"`
		Port   int    `json:"port"`
		Weight int    `json:"weight"`
	}
	type ch struct {
		Name        string   `json:"name"`
		IsSource    bool     `json:"isSource"`
		SourcesList []source `json:"sourcesList"`
	}
	c := ch{Name: channel}
	for _, h := range hosts {
		c.SourcesList = append(c.SourcesList, source{Host: h, Port: k3dReplPort, Weight: k3dReplWeight})
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"pxc": map[string]any{"replicationChannels": []ch{c}}}})
	if err != nil {
		return err
	}
	_, err = a.kubectl(ctx, serverID, "-n", ns, "patch", "pxc", cluster, "--type=merge", "-p", string(patch))
	return err
}

// k3dReplClearChannels empties a cluster's `replicationChannels`, which is how the operator is told
// to stop replicating. Used when the edge is taken off the canvas.
func (a *App) k3dReplClearChannels(ctx context.Context, serverID, ns, cluster string) error {
	_, err := a.kubectl(ctx, serverID, "-n", ns, "patch", "pxc", cluster, "--type=merge",
		"-p", `{"spec":{"pxc":{"replicationChannels":[]}}}`)
	return err
}

// k3dReplWaitRunning polls SHOW REPLICA STATUS on the replica until the channel's two threads are
// running, and returns a one-line summary either way ("" if it could not be read at all).
func (a *App) k3dReplWaitRunning(ctx context.Context, serverID, ns, cluster, channel string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		io, sql, errText, ok := a.k3dReplStatus(ctx, serverID, ns, cluster, channel)
		if ok {
			if io == "Yes" && sql == "Yes" {
				return "is running on channel " + channel
			}
			last = fmt.Sprintf("is not running yet on channel %s (IO %s, SQL %s)", channel, io, sql)
			if errText != "" {
				last += ": " + errText
			}
		}
		if time.Now().After(deadline) || !k3dSleep(ctx, 15*time.Second) {
			return last
		}
	}
}

// k3dReplStatus reads one channel's SHOW REPLICA STATUS through the replica's first database pod.
// Reported as raw field values rather than a verdict so callers can say what they saw.
//
// The password is fetched inside the script and only ever referenced as "$PW", so it never appears
// in a command line or a log; the shell does not re-parse an expanded value, so a password holding
// a quote or a `$` is safe. The channel goes into the SQL literally, which is only sound because
// k3dReplChannelName has already reduced it to [A-Za-z0-9_].
//
// A channel that does not exist yet makes mysql exit non-zero (ERROR 3074), which is reported as
// "could not read" rather than "stopped" — the difference between a replica still being wired up
// and one that has failed.
func (a *App) k3dReplStatus(ctx context.Context, serverID, ns, cluster, channel string) (ioRunning, sqlRunning, errText string, ok bool) {
	script := fmt.Sprintf(
		`PW=$(kubectl -n %s get secret internal-%s -o jsonpath='{.data.root}' | base64 -d) && `+
			`kubectl -n %s exec %s-pxc-0 -c pxc -- mysql -uroot -p"$PW" -e "SHOW REPLICA STATUS FOR CHANNEL '%s'\G"`,
		ns, cluster, ns, cluster, channel)
	res, err := a.engCtx(ctx).Exec(ctx, serverID, []string{"sh", "-c", script}, []string{"KUBECONFIG=" + k3dKubeconfig})
	if err != nil || res.Code != 0 {
		return "", "", "", false
	}
	field := func(name string) string {
		for _, ln := range strings.Split(res.Stdout, "\n") {
			k, v, found := strings.Cut(strings.TrimSpace(ln), ":")
			if found && strings.TrimSpace(k) == name {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	io := field("Replica_IO_Running")
	if io == "" {
		return "", "", "", false // no such channel yet
	}
	e := field("Last_IO_Error")
	if e == "" {
		e = field("Last_SQL_Error")
	}
	return io, field("Replica_SQL_Running"), e, true
}

// ---------------------------------------------------------------- pruning + plumbing

// k3dReplPrune stops replication on any cluster that was a replica on the last deploy and is not
// one on this design any more.
func (a *App) k3dReplPrune(ctx context.Context, st Stack, doc designDoc, links []k3dReplLink) {
	wanted := map[string]bool{}
	for _, l := range links {
		wanted[l.Dst.ID] = true
	}
	for _, f := range doc.Frames {
		if !k3dReplCapable(f) || wanted[f.ID] {
			continue
		}
		// The recorded role is read first, from whatever state the frame is in, and only a cluster
		// that WAS a replica is then waited for. Asking k3dReplServer straight out would skip a
		// frame that is still coming up on this same deploy — and a redeploy does re-provision the
		// frame, so that is not a rare case. The stale channel would survive it: it was added with
		// `kubectl patch`, so re-applying a cr.yaml that never mentioned it does not take it away.
		if a.k3dReplRecordedRole(st, doc, f) != "replica" {
			continue
		}
		server, cfg, err := a.k3dReplWaitFrame(ctx, st, doc, f)
		if err != nil {
			log.Printf("stack %d: stop replication on %s: %v", st.ID, f.Label, err)
			continue
		}
		if err := a.k3dReplClearChannels(ctx, server, cfg.Namespace, cfg.ClusterName); err != nil {
			log.Printf("stack %d: stop replication on %s: %v", st.ID, f.Label, err)
			continue
		}
		a.k3dReplLogln(st, doc, f, "replication link removed from the canvas — channel "+cfg.ReplChannel+" stopped")
		a.k3dReplSaveConfig(st, doc, f, func(c *k3dConfig) {
			c.ReplRole, c.ReplChannel, c.ReplPeer, c.ReplSources = "", "", "", nil
			// ReplSeededFrom is deliberately kept: the replica still holds the source's data, and
			// that is worth being able to read afterwards. Re-linking the pair re-seeds only if it
			// is cleared, which is what the Replication tab's Re-seed button does.
		})
	}
}

// k3dReplMembers returns a frame's member node ids, in the order provisionK3DFrame created them —
// the first is the server node, the one kubectl runs on.
func k3dReplMembers(doc designDoc, frame designFrame) []string {
	var ids []string
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "k3d" {
			ids = append(ids, n.ID)
		}
	}
	return ids
}

// k3dReplServer resolves a frame to its server node's container and the config recorded on it.
func (a *App) k3dReplServer(st Stack, doc designDoc, frame designFrame) (string, k3dConfig, error) {
	ids := k3dReplMembers(doc, frame)
	if len(ids) == 0 {
		return "", k3dConfig{}, fmt.Errorf("frame %s has no members", frame.Label)
	}
	dep, err := a.store.GetDeployment(st.ID, ids[0])
	if err != nil {
		return "", k3dConfig{}, fmt.Errorf("frame %s is not deployed: %w", frame.Label, err)
	}
	if dep.State != DeployRunning || dep.ContainerID == "" {
		return "", k3dConfig{}, fmt.Errorf("frame %s is not running (%s)", frame.Label, dep.State)
	}
	var cfg k3dConfig
	if err := json.Unmarshal(dep.Config, &cfg); err != nil {
		return "", k3dConfig{}, fmt.Errorf("frame %s has no config: %w", frame.Label, err)
	}
	if cfg.Operator != "pxc" {
		return "", k3dConfig{}, fmt.Errorf("frame %s does not run the PXC operator", frame.Label)
	}
	return dep.ContainerID, cfg, nil
}

// k3dReplRecordedRole is the role written on a frame's members by the last deploy, read without
// requiring the frame to be running — which is what makes it usable as a cheap "was this ever a
// replica?" test before committing to a wait.
func (a *App) k3dReplRecordedRole(st Stack, doc designDoc, frame designFrame) string {
	for _, id := range k3dReplMembers(doc, frame) {
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) == nil && cfg.ReplRole != "" {
			return cfg.ReplRole
		}
	}
	return ""
}

// k3dReplSeededFrom reads back the destination a frame was last seeded from, from whichever member
// still carries it. Called while a frame is being re-provisioned — before its Deployment rows are
// rewritten — so a redeploy of an already-linked pair does not restore over a working replica.
func (a *App) k3dReplSeededFrom(st Stack, doc designDoc, frame designFrame) string {
	for _, id := range k3dReplMembers(doc, frame) {
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) == nil && cfg.ReplSeededFrom != "" {
			return cfg.ReplSeededFrom
		}
	}
	return ""
}

// k3dReplSaveConfig applies an edit to the k3dConfig recorded on every member of a frame, so the
// properties panel shows the same thing whichever node was clicked.
func (a *App) k3dReplSaveConfig(st Stack, doc designDoc, frame designFrame, edit func(*k3dConfig)) {
	for _, id := range k3dReplMembers(doc, frame) {
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil {
			continue
		}
		var cfg k3dConfig
		if json.Unmarshal(dep.Config, &cfg) != nil {
			continue
		}
		edit(&cfg)
		if b, err := json.Marshal(cfg); err == nil {
			dep.Config = b
			a.store.UpsertDeployment(dep)
		}
	}
}

// k3dReplLogln appends a line to a frame's server node's deployment log, where the node card and
// the deployment console already read from.
func (a *App) k3dReplLogln(st Stack, doc designDoc, frame designFrame, msg string) {
	ids := k3dReplMembers(doc, frame)
	if len(ids) == 0 {
		return
	}
	a.replLogln(st.ID, ids[0], msg)
}

// k3dSleep waits, or returns false if the deploy was cancelled first.
func k3dSleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ---------------------------------------------------------------- HTTP

// k3dReplView is what the Replication tab renders: what the design says this cluster is, what was
// recorded when the link was last set up, and — for a replica — what MySQL says right now.
type k3dReplView struct {
	Role       string   `json:"role"`       // "source" | "replica" | ""
	Channel    string   `json:"channel"`    //
	Peer       string   `json:"peer"`       // the other cluster's CR name
	Cluster    string   `json:"cluster"`    // this cluster's CR name
	Sources    []string `json:"sources"`    // replica: the addresses it dials
	SeededFrom string   `json:"seededFrom"` // replica: the backup destination it was restored from
	// Live, read from the replica on request. Running is nil when there is nothing to read (a
	// source, or a replica whose channel has not been attached yet).
	Running   *bool  `json:"running,omitempty"`
	IORunning string `json:"ioRunning,omitempty"`
	SQLState  string `json:"sqlRunning,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// Exposed is the addresses a replica would dial this cluster on — read for a source, so the
	// tab can show that the per-pod Services actually got an address.
	Exposed []string `json:"exposed,omitempty"`
}

func (a *App) handleK3DReplication(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	if cfg.Operator != "pxc" {
		writeErr(w, http.StatusConflict, "replication links are a Percona XtraDB Cluster operator feature")
		return
	}
	view := k3dReplView{
		Role: cfg.ReplRole, Channel: cfg.ReplChannel, Peer: cfg.ReplPeer,
		Cluster: cfg.ClusterName, Sources: cfg.ReplSources, SeededFrom: cfg.ReplSeededFrom,
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) == nil && view.Role == "" {
		// The design is the authority on what this cluster is *meant* to be — the recorded role
		// only catches up once the deploy's replication phase has finished — so a link drawn since
		// the last deploy, or one still being set up, shows here rather than reading as "not part
		// of a replication link".
		for _, l := range k3dReplLinks(doc) {
			switch frame.ID {
			case l.Src.ID:
				view.Role, view.Channel, view.Peer = "source", l.Channel, k3dCRName(l.Dst)
			case l.Dst.ID:
				view.Role, view.Channel, view.Peer = "replica", l.Channel, k3dCRName(l.Src)
			}
		}
	}
	switch view.Role {
	case "replica":
		if view.Channel != "" {
			if io, sql, errText, read := a.k3dReplStatus(r.Context(), dep.ContainerID, cfg.Namespace, cfg.ClusterName, view.Channel); read {
				running := io == "Yes" && sql == "Yes"
				view.Running, view.IORunning, view.SQLState, view.LastError = &running, io, sql, errText
			}
		}
	case "source":
		view.Exposed, _ = a.k3dReplSourceHostsOnce(r.Context(), dep.ContainerID, cfg.Namespace, cfg.ClusterName)
	}
	writeJSON(w, http.StatusOK, view)
}

// handleK3DReplicationReseed clears the record of the seed restore and runs the link again, which
// restores the replica from a fresh backup of the source. Destructive by definition — it is what
// the deploy path deliberately will not do on its own — so it is its own explicit action.
func (a *App) handleK3DReplicationReseed(w http.ResponseWriter, r *http.Request) {
	st, frame, _, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	var link k3dReplLink
	for _, l := range k3dReplLinks(doc) {
		if l.Dst.ID == frame.ID {
			link = l
			break
		}
	}
	if link.Channel == "" {
		writeErr(w, http.StatusConflict, "this cluster is not the replica end of a replication link")
		return
	}
	a.k3dReplSaveConfig(st, doc, frame, func(c *k3dConfig) { c.ReplSeededFrom = "" })
	ctx, end := a.deployScope(st.ID, a.eng(st))
	go func() {
		defer end()
		if err := a.k3dReplLink(ctx, st, doc, link); err != nil {
			log.Printf("stack %d: re-seed %s → %s: %v", st.ID, link.Src.Label, link.Dst.Label, err)
			a.k3dReplLogln(st, doc, link.Dst, "re-seed failed: "+err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "started",
		"channel": link.Channel,
		"message": "re-seeding " + link.Dst.Label + " from a fresh backup of " + link.Src.Label + " — watch the node's deployment log",
	})
}

// k3dReplSourceHostsOnce is k3dReplSourceHosts without the retry, for the properties panel: a
// request that renders a tab must not sit there for three minutes because a Service is Pending.
func (a *App) k3dReplSourceHostsOnce(ctx context.Context, serverID, ns, cluster string) ([]string, error) {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "svc",
		"-l", "app.kubernetes.io/instance="+cluster, "-o", "json")
	if err != nil {
		return nil, err
	}
	return k3dReplHostsFromServices(out), nil
}
