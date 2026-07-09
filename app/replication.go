package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cross-cluster replication links.
//
// A replication edge drawn between two cluster member nodes (PXC or Percona Server
// replication) that live in *different* frames sets up MySQL channel-based
// replication between them. It is applied as the final phase of a deploy and is
// reconciled on every (re)deploy — channels added on the canvas are created and
// channels removed from it are torn down.
//
//	async (edge.Type=="async")  — directed source → replica; one channel on the
//	                              replica pulling from the source.
//	bidir (edge.Type=="bidir")  — both nodes replicate from each other (two channels,
//	                              one on each side). Multi-writer; conflict-prone.
//
// Each link uses a named channel "xrepl_<source host>". When both clusters have GTID
// enabled the channel uses GTID auto-positioning (the replica fetches the GTIDs it is
// missing from the source). Otherwise it falls back to binary-log file/position
// replication, seeded from the source's *current* coordinates (so only writes made
// after the link is set up replicate — seed data first if the clusters aren't empty).
// No RESET is issued, so each node keeps its own cluster's data. The repl user and
// password are shared across clusters (REPL_PASSWORD) and created by every cluster's
// bootstrap, so the replica authenticates to the source's repl user.
//
// Only the modern, 8.0.23+/8.4-safe keywords are used: CHANGE REPLICATION SOURCE …
// FOR CHANNEL, START/STOP REPLICA FOR CHANNEL, SHOW REPLICA STATUS FOR CHANNEL,
// RESET REPLICA ALL FOR CHANNEL, and SHOW BINARY LOG STATUS (8.4) / SHOW MASTER
// STATUS (8.0) to read the source position.

const replChannelPrefix = "xrepl_"

// ------------------------------------------------------------------ deploy barrier
//
// A deploy sets up replication (intra-cluster attach and cross-cluster channels)
// only AFTER every MySQL-family replication participant (PXC + MySQL-replication
// members) in the stack has reached a clean, credentialed, GTID-reset baseline.
// The barrier is the rendezvous: each such node "arrives" once its server is up,
// its .env credentials are created, and its binlog/GTID has been reset; the MySQL
// replica-attach step and reconcileReplication both wait on it. This guarantees a
// shared empty GTID baseline across all clusters so cross-cluster channels attach
// cleanly (AUTO_POSITION with nothing to backfill).

type deployBarrier struct {
	mu      sync.Mutex
	pending map[string]bool
	done    chan struct{}
}

func newDeployBarrier(ids []string) *deployBarrier {
	b := &deployBarrier{pending: map[string]bool{}, done: make(chan struct{})}
	for _, id := range ids {
		b.pending[id] = true
	}
	if len(b.pending) == 0 {
		close(b.done)
	}
	return b
}

// arrive marks a participant as ready; it is idempotent and safe to call from the
// success path and from a deferred safety-net drain. When the last participant
// arrives, waiters are released.
func (b *deployBarrier) arrive(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.pending[id] {
		return
	}
	delete(b.pending, id)
	if len(b.pending) == 0 {
		select {
		case <-b.done:
		default:
			close(b.done)
		}
	}
}

// wait blocks until every participant has arrived or the timeout elapses (a stuck
// node must not hang the whole deploy — replication for it will simply be logged
// as failed downstream).
func (b *deployBarrier) wait(timeout time.Duration) {
	select {
	case <-b.done:
	case <-time.After(timeout):
	}
}

// setDeployBarrier installs the barrier for a stack's in-flight deploy, seeded with
// the node ids that must reach the reset baseline before replication is set up.
func (a *App) setDeployBarrier(stackID int64, ids []string) *deployBarrier {
	b := newDeployBarrier(ids)
	a.barriers.Store(stackID, b)
	return b
}

// deployBarrierFor returns the stack's current deploy barrier (nil if none).
func (a *App) deployBarrierFor(stackID int64) *deployBarrier {
	if v, ok := a.barriers.Load(stackID); ok {
		return v.(*deployBarrier)
	}
	return nil
}

func isReplEdge(e designEdge) bool { return e.Type == "async" || e.Type == "bidir" }

// replMember resolves a node id to a replication-capable cluster member (a PXC or
// Percona Server replication member) together with its owning frame.
func replMember(doc designDoc, nodeID string) (designNode, designFrame, bool) {
	for _, n := range doc.Nodes {
		if n.ID != nodeID || n.FrameID == "" || (n.Type != "pxc" && n.Type != "mysql") {
			continue
		}
		for _, f := range doc.Frames {
			if f.ID == n.FrameID && (f.Type == "pxc" || f.Type == "mysql") {
				return n, f, true
			}
		}
	}
	return designNode{}, designFrame{}, false
}

// replLink is one directed source → replica relationship resolved from an edge,
// carrying both endpoints' frames (needed to pick GTID vs file/position).
type replLink struct {
	src, dst           designNode
	srcFrame, dstFrame designFrame
}

// replicationLinks expands the design's replication edges into directed links
// (async → one, bidirectional → two). Endpoints must be replication-capable members
// in different frames; anything else is skipped.
func replicationLinks(doc designDoc) []replLink {
	var out []replLink
	for _, e := range doc.Edges {
		if !isReplEdge(e) {
			continue
		}
		a, fa, ok1 := replMember(doc, e.From.Node)
		b, fb, ok2 := replMember(doc, e.To.Node)
		if !ok1 || !ok2 || fa.ID == fb.ID {
			continue
		}
		// async: From is the source, To the replica.
		out = append(out, replLink{src: a, dst: b, srcFrame: fa, dstFrame: fb})
		if e.Type == "bidir" {
			out = append(out, replLink{src: b, dst: a, srcFrame: fb, dstFrame: fa})
		}
	}
	return out
}

// replChannelName builds a MySQL-safe channel name from a source host.
func replChannelName(sourceHost string) string {
	var sb strings.Builder
	sb.WriteString(replChannelPrefix)
	for _, r := range sourceHost {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// memberServerID derives a member's server-id the same way its provisioner does, so
// validation can detect a collision between the two endpoints of a link.
func memberServerID(n designNode) int {
	host := sanitizeName(n.Label)
	if n.Type == "mysql" {
		return mysqlServerID(host)
	}
	return pxcServerID(host)
}

// frameMajor returns a cluster frame's MySQL major series ("8.0" | "8.4"), used to
// pick version-safe keywords.
func frameMajor(f designFrame) string {
	if f.Type == "mysql" {
		return psMajorOf(f.PSMajor)
	}
	if f.PXCMajor == "8.4" {
		return "8.4"
	}
	return "8.0"
}

// memberReplMajor returns the MySQL major series of the frame a replication member
// belongs to, used to pick series-safe channel scripts on the replica side.
func memberReplMajor(doc designDoc, n designNode) string {
	for _, f := range doc.Frames {
		if f.ID == n.FrameID {
			return frameMajor(f)
		}
	}
	return "8.0"
}

// sourceBinlogPos reads a source's current binary-log coordinates (for file/position
// replication when GTID is off). 8.4 renamed SHOW MASTER STATUS → SHOW BINARY LOG
// STATUS. The first two whitespace-separated fields are File and Position.
func (a *App) sourceBinlogPos(ctx context.Context, containerID, rootPW, major string) (string, string, error) {
	stmt := "SHOW MASTER STATUS"
	if major == "8.4" {
		stmt = "SHOW BINARY LOG STATUS"
	}
	res, err := a.docker.Exec(ctx, containerID, []string{"bash", "-c", `mysql -uroot -p"$ROOT_PW" -N -e "$STMT"`}, []string{"ROOT_PW=" + rootPW, "STMT=" + stmt})
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(fields) < 2 {
		return "", "", fmt.Errorf("no binary log position on source (is log_bin enabled?)")
	}
	return fields[0], fields[1], nil
}

// sourceGTIDExecuted reads a source's current @@global.gtid_executed as a single-line,
// whitespace-free GTID set. Used to seed a fresh GTID-auto channel's replica so it
// starts from "now" instead of walking the source's whole history — which fails when
// the source (e.g. an SST-joined PXC node) has purged its early binlogs. `--raw`
// disables MySQL's batch-mode escaping (a multi-UUID set is otherwise printed with a
// literal "\n" between UUIDs), then all whitespace is stripped to rejoin the UUIDs.
func (a *App) sourceGTIDExecuted(ctx context.Context, containerID, rootPW string) (string, error) {
	res, err := a.docker.Exec(ctx, containerID, []string{"bash", "-c", `mysql -uroot -p"$ROOT_PW" -N --raw -e "SELECT @@global.gtid_executed"`}, []string{"ROOT_PW=" + rootPW})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, r := range res.Stdout {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			sb.WriteRune(r)
		}
	}
	return sb.String(), nil
}

// waitNodeRunning blocks until a node's deployment reaches the running state (or
// errors / times out).
func (a *App) waitNodeRunning(stackID int64, nodeID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployRunning {
				return true
			}
			if dep.State == DeployError {
				return false
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// replLogln appends a line to a node's deployment progress log without disturbing
// its phase/percent — by the time replication is configured the node is already
// running, so we only annotate its log.
func (a *App) replLogln(stackID int64, nodeID, line string) {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return
	}
	var p provProgress
	json.Unmarshal(dep.Progress, &p)
	p.Log = append(p.Log, line)
	if len(p.Log) > 200 {
		p.Log = p.Log[len(p.Log)-200:]
	}
	b, _ := json.Marshal(p)
	a.store.SetDeploymentProgress(stackID, nodeID, b)
}

// reconcileReplication is the final deploy phase: it configures every cross-cluster
// replication channel described by the design's replication edges on the live
// containers, and prunes DBCanvas channels removed from the canvas. Runs in its own
// goroutine; it never fails a node (the cluster itself is already running) — a
// channel that won't come up is logged against the replica's deployment.
// reconcileReplication runs on the deploy's context (see deployrun.go) so a
// destroy mid-deploy cancels it instead of leaving it waiting on clusters that
// are being torn down.
func (a *App) reconcileReplication(ctx context.Context, st Stack, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	links := replicationLinks(doc)

	// Every replication-capable member that exists, so channels can be pruned from a
	// node that is no longer anyone's replica.
	members := map[string]designNode{}
	for _, n := range doc.Nodes {
		if n.FrameID != "" && (n.Type == "pxc" || n.Type == "mysql") {
			members[n.ID] = n
		}
	}
	if len(members) == 0 {
		return
	}

	// Cross-cluster channels are configured only after every MySQL-family
	// participant in this deploy has reached its reset baseline (credentials set,
	// binlog/GTID cleared). This gives all clusters a shared empty GTID baseline
	// before any replication link is created.
	if b := a.deployBarrierFor(st.ID); b != nil {
		b.wait(deployTimeout())
	}

	// Wait for every node referenced by a link to be running before configuring.
	involved := map[string]bool{}
	for _, l := range links {
		involved[l.src.ID] = true
		involved[l.dst.ID] = true
	}
	for id := range involved {
		if !a.waitNodeRunning(st.ID, id, deployTimeout()) {
			label := id
			if n, ok := members[id]; ok {
				label = n.Label
			}
			log.Printf("stack %d replication: node %s not running; its links may not configure", st.ID, label)
		}
	}

	// Desired channels per replica node. GTID auto-position when both clusters use
	// GTID; otherwise binlog file/position. The source's coordinates (gtid_executed
	// or binlog file/pos) are read at APPLY time, not here — see the ordering note in
	// the apply loop below.
	type chanSpec struct {
		channel, sourceFQDN, replUser, replPW, srcLabel string
		auto                                            bool
		srcNodeID, srcRootPW, srcMajor                  string // source, read for its position at apply time
	}
	desired := map[string][]chanSpec{}
	for _, l := range links {
		srcHost := hosts[l.src.ID]
		if srcHost == "" {
			srcHost = sanitizeName(l.src.Label)
		}
		sdep, err := a.store.GetDeployment(st.ID, l.src.ID)
		if err != nil {
			continue
		}
		var ssec pxcSecrets
		json.Unmarshal(sdep.Secrets, &ssec)
		desired[l.dst.ID] = append(desired[l.dst.ID], chanSpec{
			channel:    replChannelName(srcHost),
			sourceFQDN: fqdnOf(srcHost, domain),
			replUser:   ssec.ReplUser,
			replPW:     ssec.ReplPassword,
			srcLabel:   l.src.Label,
			auto:       l.srcFrame.GTID && l.dstFrame.GTID,
			srcNodeID:  l.src.ID,
			srcRootPW:  ssec.RootPassword,
			srcMajor:   frameMajor(l.srcFrame),
		})
	}

	// Configure replicas in replication-dependency order: a replica that is ITSELF a
	// source for another link is configured before the replicas it feeds. This matters
	// for chained/bidirectional topologies (e.g. mysql01 ↔ mysql04 → mysql07). A fresh
	// auto channel seeds the replica's gtid_purged from the source's *current*
	// gtid_executed; a source that is itself a downstream replica only acquires its
	// upstream's GTIDs (into its own gtid_purged, via that same seeding) once its own
	// channel is applied. If a chained replica were seeded before that, it would omit
	// those upstream GTIDs and later request them via AUTO_POSITION — but the source
	// holds them only in gtid_purged (never in a serveable binlog), yielding a fatal
	// 1236. Applying in dependency order and reading the source's position at apply time
	// makes each seed reflect the source's full, settled GTID set.
	replicaSet := map[string]bool{}
	for id := range desired {
		replicaSet[id] = true
	}
	order := replicaApplyOrder(links, replicaSet)
	inOrder := map[string]bool{}
	for _, id := range order {
		inOrder[id] = true
	}
	// Every other member is still visited so stale DBCanvas channels get pruned.
	var rest []string
	for id := range members {
		if !inOrder[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	for _, id := range order {
		n, ok := members[id]
		if !ok {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		var rsec pxcSecrets
		json.Unmarshal(dep.Secrets, &rsec)
		// The channel scripts run on the replica (this member), so pick the variant
		// that matches this node's series (5.7 uses the legacy CHANGE MASTER vocabulary).
		applyScript, pruneScript := replChannelApply, replChannelPrune
		if memberReplMajor(doc, n) == "5.7" {
			applyScript, pruneScript = replChannelApply57, replChannelPrune57
		}
		var keep []string
		for _, c := range desired[id] {
			env := []string{
				"ROOT_PW=" + rsec.RootPassword, "CHANNEL=" + c.channel,
				"SOURCE_HOST=" + c.sourceFQDN, "REPL_USER=" + c.replUser, "REPL_PW=" + c.replPW,
			}
			// Read the source's position NOW (in dependency order) so a chained source's
			// inherited upstream GTIDs are already reflected in the seed — see the note above.
			sdep, serr := a.store.GetDeployment(st.ID, c.srcNodeID)
			if serr != nil || sdep.ContainerID == "" {
				a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication from %s FAILED: source not available", c.srcLabel))
				log.Printf("stack %d replication %s←%s: source deployment unavailable", st.ID, n.Label, c.srcLabel)
				continue
			}
			method := "GTID auto-position"
			if c.auto {
				// Seed a freshly-created auto channel from the source's current position so
				// it replicates ongoing changes only. Best-effort: an empty/failed read just
				// falls back to plain auto-position (fine when the source kept all binlogs).
				srcGTID := ""
				if g, e := a.sourceGTIDExecuted(ctx, sdep.ContainerID, c.srcRootPW); e == nil {
					srcGTID = g
				} else {
					log.Printf("stack %d replication %s←%s: read source gtid_executed: %v", st.ID, n.Label, c.srcLabel, e)
				}
				env = append(env, "AUTO=1", "SRC_GTID="+srcGTID)
			} else {
				file, pos, e := a.sourceBinlogPos(ctx, sdep.ContainerID, c.srcRootPW, c.srcMajor)
				if e != nil {
					a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication from %s FAILED: %v", c.srcLabel, e))
					log.Printf("stack %d replication %s←%s: read source position: %v", st.ID, n.Label, c.srcLabel, e)
					continue
				}
				env = append(env, "AUTO=0", "LOG_FILE="+file, "LOG_POS="+pos)
				method = fmt.Sprintf("file/position %s:%s", file, pos)
			}
			if err := a.runStep(ctx, dep.ContainerID, applyScript, env, func(s string) { a.replLogln(st.ID, id, s) }); err != nil {
				a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication from %s FAILED: %v", c.srcLabel, err))
				log.Printf("stack %d replication %s←%s: %v", st.ID, n.Label, c.srcLabel, err)
				continue
			}
			a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication: %s → %s (channel %s, %s)", c.srcLabel, n.Label, c.channel, method))
			keep = append(keep, c.channel)
		}
		// Drop any DBCanvas channels this node should no longer carry (best-effort).
		a.runStep(ctx, dep.ContainerID, pruneScript, []string{"ROOT_PW=" + rsec.RootPassword, "KEEP=" + strings.Join(keep, " ")}, func(s string) { a.replLogln(st.ID, id, s) })
	}
	log.Printf("stack %d replication: reconciled %d link(s) across %d member(s)", st.ID, len(links), len(members))
}

// replicaApplyOrder orders the replica nodes (the keys of desired) so that a replica
// which is also a replication source for another replica is configured before the
// replicas it feeds. It is a topological sort over the "source→replica" edges that run
// between two replica nodes; edges whose source is not itself a replica (a plain cluster
// primary) impose no constraint. Bidirectional links form cycles, which are broken by
// emitting the least-depended-upon remaining node (smallest in-degree, ties by id) —
// within a cycle the seeds are mutually consistent, so any break is safe. See the note
// in reconcileReplication for why the order matters.
func replicaApplyOrder(links []replLink, replicas map[string]bool) []string {
	isRep := replicas
	adj := map[string][]string{}
	indeg := map[string]int{}
	for id := range replicas {
		indeg[id] = 0
	}
	seen := map[string]bool{}
	for _, l := range links {
		if !isRep[l.src.ID] || !isRep[l.dst.ID] || l.src.ID == l.dst.ID {
			continue
		}
		key := l.src.ID + "\x00" + l.dst.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		adj[l.src.ID] = append(adj[l.src.ID], l.dst.ID)
		indeg[l.dst.ID]++
	}
	placed := map[string]bool{}
	var out []string
	for len(out) < len(replicas) {
		// Pick the unplaced node with the smallest in-degree (ties by id): a ready node
		// (in-degree 0) whenever one exists, otherwise the node that breaks a cycle with
		// the fewest violated dependencies.
		cand := ""
		for id := range replicas {
			if placed[id] {
				continue
			}
			if cand == "" || indeg[id] < indeg[cand] || (indeg[id] == indeg[cand] && id < cand) {
				cand = id
			}
		}
		if cand == "" {
			break
		}
		placed[cand] = true
		out = append(out, cand)
		for _, m := range adj[cand] {
			if !placed[m] {
				indeg[m]--
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ scripts

// replChannelApply (re)configures one cross-cluster channel on a replica and starts
// it. With $AUTO=1 it uses GTID auto-positioning; otherwise it starts from the binlog
// file/position in $LOG_FILE/$LOG_POS. No RESET — the node keeps its own cluster's
// data. GET_SOURCE_PUBLIC_KEY lets the repl user's caching_sha2_password auth work
// over a non-TLS link.
//
// On a *fresh* auto channel, if $SRC_GTID (the source's current gtid_executed) is
// given, its transactions the replica doesn't yet have are added to the replica's
// gtid_purged (via GTID_SUBTRACT + the "+" form). This makes auto-position replicate
// only changes made *after* the link is set up — matching the file/position path — and
// avoids a fatal 1236 when the source (e.g. an SST-joined PXC node) has purged the
// early binlogs that plain auto-position would otherwise request. The channel-exists
// guard ensures this seeding happens once at creation, never on a later reconcile
// (which would wrongly skip transactions committed on the source since).
const replChannelApply = `set -e
mysql -uroot -p"$ROOT_PW" -e "STOP REPLICA FOR CHANNEL '$CHANNEL';" 2>/dev/null || true
if [ "$AUTO" = 1 ]; then
  POS="SOURCE_AUTO_POSITION=1"
  EXISTS=$(mysql -uroot -p"$ROOT_PW" -N -e "SELECT COUNT(*) FROM performance_schema.replication_connection_configuration WHERE CHANNEL_NAME='$CHANNEL'" 2>/dev/null)
  if [ "$EXISTS" = 0 ] && [ -n "$SRC_GTID" ]; then
    MISSING=$(mysql -uroot -p"$ROOT_PW" -N --raw -e "SELECT GTID_SUBTRACT('$SRC_GTID', @@global.gtid_executed)" 2>/dev/null | tr -d '[:space:]')
    [ -n "$MISSING" ] && mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL gtid_purged='+$MISSING';"
  fi
else
  POS="SOURCE_LOG_FILE='$LOG_FILE', SOURCE_LOG_POS=$LOG_POS"
fi
mysql -uroot -p"$ROOT_PW" -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST='$SOURCE_HOST', SOURCE_PORT=3306, SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW', GET_SOURCE_PUBLIC_KEY=1, $POS FOR CHANNEL '$CHANNEL';"
mysql -uroot -p"$ROOT_PW" -e "START REPLICA FOR CHANNEL '$CHANNEL';"
OK=0
for i in $(seq 1 15); do
  S=$(mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS FOR CHANNEL '$CHANNEL'\G" 2>/dev/null)
  if echo "$S" | grep -q "Replica_IO_Running: Yes" && echo "$S" | grep -q "Replica_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "channel $CHANNEL threads not running:"; mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS FOR CHANNEL '$CHANNEL'\G" 2>/dev/null | grep -iE 'Running|Last_(IO|SQL)_Error' | head -8; exit 1; }`

// replChannelPrune stops and removes any DBCanvas cross-cluster channels (xrepl_*)
// that are no longer wanted ($KEEP = space-separated channel names to keep).
const replChannelPrune = `set -e
for ch in $(mysql -uroot -p"$ROOT_PW" -N -e "SELECT CHANNEL_NAME FROM performance_schema.replication_connection_configuration WHERE CHANNEL_NAME LIKE 'xrepl\\_%'" 2>/dev/null); do
  keep=0
  for k in $KEEP; do [ "$ch" = "$k" ] && keep=1; done
  if [ "$keep" = 0 ]; then
    mysql -uroot -p"$ROOT_PW" -e "STOP REPLICA FOR CHANNEL '$ch';" 2>/dev/null || true
    mysql -uroot -p"$ROOT_PW" -e "RESET REPLICA ALL FOR CHANNEL '$ch';" 2>/dev/null || true
    echo "removed stale replication channel $ch"
  fi
done`

// replChannelApply57 / replChannelPrune57 are the Percona Server 5.7 counterparts of
// replChannelApply / replChannelPrune. 5.7 supports named channels but only through the
// legacy vocabulary: CHANGE MASTER TO … FOR CHANNEL, START/STOP SLAVE FOR CHANNEL,
// SHOW SLAVE STATUS FOR CHANNEL, RESET SLAVE ALL FOR CHANNEL (Slave_IO/SQL_Running).
// The repl user uses mysql_native_password, so no GET_SOURCE_PUBLIC_KEY is needed.
// The gtid_purged seed uses 5.7's incremental "+" form only best-effort — 5.7 rejects
// it when gtid_executed is non-empty, in which case plain auto-position is used.
const replChannelApply57 = `set -e
mysql -uroot -p"$ROOT_PW" -e "STOP SLAVE FOR CHANNEL '$CHANNEL';" 2>/dev/null || true
if [ "$AUTO" = 1 ]; then
  POS="MASTER_AUTO_POSITION=1"
  EXISTS=$(mysql -uroot -p"$ROOT_PW" -N -e "SELECT COUNT(*) FROM performance_schema.replication_connection_configuration WHERE CHANNEL_NAME='$CHANNEL'" 2>/dev/null)
  if [ "$EXISTS" = 0 ] && [ -n "$SRC_GTID" ]; then
    MISSING=$(mysql -uroot -p"$ROOT_PW" -N --raw -e "SELECT GTID_SUBTRACT('$SRC_GTID', @@global.gtid_executed)" 2>/dev/null | tr -d '[:space:]')
    [ -n "$MISSING" ] && mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL gtid_purged='+$MISSING';" 2>/dev/null || true
  fi
else
  POS="MASTER_LOG_FILE='$LOG_FILE', MASTER_LOG_POS=$LOG_POS"
fi
mysql -uroot -p"$ROOT_PW" -e "CHANGE MASTER TO MASTER_HOST='$SOURCE_HOST', MASTER_PORT=3306, MASTER_USER='$REPL_USER', MASTER_PASSWORD='$REPL_PW', $POS FOR CHANNEL '$CHANNEL';"
mysql -uroot -p"$ROOT_PW" -e "START SLAVE FOR CHANNEL '$CHANNEL';"
OK=0
for i in $(seq 1 15); do
  S=$(mysql -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS FOR CHANNEL '$CHANNEL'\G" 2>/dev/null)
  if echo "$S" | grep -q "Slave_IO_Running: Yes" && echo "$S" | grep -q "Slave_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "channel $CHANNEL threads not running:"; mysql -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS FOR CHANNEL '$CHANNEL'\G" 2>/dev/null | grep -iE 'Running|Last_(IO|SQL)_Error' | head -8; exit 1; }`

const replChannelPrune57 = `set -e
for ch in $(mysql -uroot -p"$ROOT_PW" -N -e "SELECT CHANNEL_NAME FROM performance_schema.replication_connection_configuration WHERE CHANNEL_NAME LIKE 'xrepl\\_%'" 2>/dev/null); do
  keep=0
  for k in $KEEP; do [ "$ch" = "$k" ] && keep=1; done
  if [ "$keep" = 0 ]; then
    mysql -uroot -p"$ROOT_PW" -e "STOP SLAVE FOR CHANNEL '$ch';" 2>/dev/null || true
    mysql -uroot -p"$ROOT_PW" -e "RESET SLAVE ALL FOR CHANNEL '$ch';" 2>/dev/null || true
    echo "removed stale replication channel $ch"
  fi
done`
