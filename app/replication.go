package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
func (a *App) reconcileReplication(st Stack, doc designDoc) {
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

	// Wait for every node referenced by a link to be running before configuring.
	involved := map[string]bool{}
	for _, l := range links {
		involved[l.src.ID] = true
		involved[l.dst.ID] = true
	}
	for id := range involved {
		if !a.waitNodeRunning(st.ID, id, 15*time.Minute) {
			label := id
			if n, ok := members[id]; ok {
				label = n.Label
			}
			log.Printf("stack %d replication: node %s not running; its links may not configure", st.ID, label)
		}
	}

	ctx := context.Background()

	// Desired channels per replica node. GTID auto-position when both clusters use
	// GTID; otherwise binlog file/position captured from the source right now.
	type chanSpec struct {
		channel, sourceFQDN, replUser, replPW, srcLabel string
		auto                                            bool
		logFile, logPos                                 string
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
		spec := chanSpec{
			channel:    replChannelName(srcHost),
			sourceFQDN: fqdnOf(srcHost, domain),
			replUser:   ssec.ReplUser,
			replPW:     ssec.ReplPassword,
			srcLabel:   l.src.Label,
			auto:       l.srcFrame.GTID && l.dstFrame.GTID,
		}
		if !spec.auto {
			file, pos, e := a.sourceBinlogPos(ctx, sdep.ContainerID, ssec.RootPassword, frameMajor(l.srcFrame))
			if e != nil {
				a.replLogln(st.ID, l.dst.ID, fmt.Sprintf("cross-cluster replication from %s FAILED: %v", l.src.Label, e))
				log.Printf("stack %d replication %s←%s: read source position: %v", st.ID, l.dst.Label, l.src.Label, e)
				continue
			}
			spec.logFile, spec.logPos = file, pos
		}
		desired[l.dst.ID] = append(desired[l.dst.ID], spec)
	}

	for id, n := range members {
		dep, err := a.store.GetDeployment(st.ID, id)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		var rsec pxcSecrets
		json.Unmarshal(dep.Secrets, &rsec)
		var keep []string
		for _, c := range desired[id] {
			env := []string{
				"ROOT_PW=" + rsec.RootPassword, "CHANNEL=" + c.channel,
				"SOURCE_HOST=" + c.sourceFQDN, "REPL_USER=" + c.replUser, "REPL_PW=" + c.replPW,
			}
			method := "GTID auto-position"
			if c.auto {
				env = append(env, "AUTO=1")
			} else {
				env = append(env, "AUTO=0", "LOG_FILE="+c.logFile, "LOG_POS="+c.logPos)
				method = fmt.Sprintf("file/position %s:%s", c.logFile, c.logPos)
			}
			if err := a.runStep(ctx, dep.ContainerID, replChannelApply, env, func(s string) { a.replLogln(st.ID, id, s) }); err != nil {
				a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication from %s FAILED: %v", c.srcLabel, err))
				log.Printf("stack %d replication %s←%s: %v", st.ID, n.Label, c.srcLabel, err)
				continue
			}
			a.replLogln(st.ID, id, fmt.Sprintf("cross-cluster replication: %s → %s (channel %s, %s)", c.srcLabel, n.Label, c.channel, method))
			keep = append(keep, c.channel)
		}
		// Drop any DBCanvas channels this node should no longer carry (best-effort).
		a.runStep(ctx, dep.ContainerID, replChannelPrune, []string{"ROOT_PW=" + rsec.RootPassword, "KEEP=" + strings.Join(keep, " ")}, func(s string) { a.replLogln(st.ID, id, s) })
	}
	log.Printf("stack %d replication: reconciled %d link(s) across %d member(s)", st.ID, len(links), len(members))
}

// ------------------------------------------------------------------ scripts

// replChannelApply (re)configures one cross-cluster channel on a replica and starts
// it. With $AUTO=1 it uses GTID auto-positioning (fetches the GTIDs missing from the
// source); otherwise it starts from the binlog file/position in $LOG_FILE/$LOG_POS.
// No RESET — the node keeps its own cluster's data. GET_SOURCE_PUBLIC_KEY lets the
// repl user's caching_sha2_password auth work over a non-TLS link.
const replChannelApply = `set -e
mysql -uroot -p"$ROOT_PW" -e "STOP REPLICA FOR CHANNEL '$CHANNEL';" 2>/dev/null || true
if [ "$AUTO" = 1 ]; then
  POS="SOURCE_AUTO_POSITION=1"
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
