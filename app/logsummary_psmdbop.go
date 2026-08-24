package main

// logsummary_psmdbop.go — the Percona Operator for MongoDB (PSMDB) and its backup agents.
//
// The second Kubernetes vocabulary, and it is deliberately built on the first: the PSMDB
// operator is the same controller-runtime process the PXC one is, writing the same
// tab-separated zap lines with the same trailing JSON object, so lsFoldOperator reads it
// unchanged and only the catalogue and the sniff are new. What is genuinely different is
// the third log.
//
//	<cluster> · operator     the psmdb-controller. Same format as the PXC operator's,
//	                         entirely different vocabulary — it has a cluster state
//	                         machine, a replica set to configure, and PBM to drive.
//	<cluster>-rs0-N          each member's mongod log, JSON, one object per line, read off
//	                         the volume at /data/db/logs/mongod.log. logsummary_mongo.go
//	                         reads it unchanged.
//	<cluster>-rs0-N · pbm    each member's pbm-agent sidecar. NOT one collector for the
//	                         cluster — one per member, and only ever ONE of them is doing
//	                         the work. See lsPSMDBFindingPITRWho.
//
// Four things the capture taught, each of which changed the code:
//
//  1. **The backup and the oplog slicer are NOMINATED, and the losers say nothing useful.**
//     Every agent logs the nomination; one wins; the other two write `skip after
//     nomination, probably started by another node` and go quiet. Read one agent's log and
//     point-in-time recovery looks like it is not running at all. Measured: of three
//     agents, rs0-2 won the PITR nomination and was the only one to write `streaming
//     started`.
//
//  2. **The pbm-agent log is two formats, and the second one only appears when things are
//     wrong.** PBM writes its log *into MongoDB*; when it cannot reach a primary it falls
//     back to stderr through Go's standard logger — `2026/08/22 08:42:19 [ERROR] writing
//     log: db: server selection error…`. So the Go-stdlib half of the file is, by
//     construction, the half written while the cluster was broken. The entrypoint wrapper
//     uses the same format for `starting pbm-agent` / `exited with code 1` / `restart in
//     5 sec`, which is the only record that the agent crash-looped at all.
//
//  3. **A partitioned member is NOT restarted, which is the opposite of PXC.** A PXC pod's
//     liveness probe asks wsrep whether the member is Primary, so a member on the wrong
//     side of a partition is killed within 25 seconds. A mongod's probe asks whether the
//     process answers. Measured: 3m 46s of 100% packet loss on a secondary, zero restarts,
//     no probe event — the member sat there serving stale reads exactly as MongoDB
//     intends, and the only trace was in its own log and its agent's.
//
//  4. **A stuck rolling restart is 245 INFO records saying two things.** A spec edit that
//     left one member unschedulable produced `StatefulSet is not up to date` ×164 and
//     `can't start/continue 'SmartUpdate': waiting for all replicas are ready` ×81, both at
//     INFO, forever, and nothing that says the cluster is stuck. The Pending pod is a
//     Kubernetes fact the operator never mentions.

import (
	"regexp"
	"strings"
	"time"
)

const (
	lsFlavourPSMDBOperator = "psmdboperator" // the percona-server-mongodb-operator controller log
	lsFlavourPBMAgent      = "pbmagent"      // one member's pbm-agent sidecar
)

// The operator's own state track, which is better than the PXC operator's: PSMDB keeps a
// cluster state in its custom resource and logs every transition of it, so the lane is the
// operator's opinion of the cluster rather than a fact about the controller process.
const (
	lsStatePSMDBReady = "CR-READY" // .status.state == ready
	lsStatePSMDBInit  = "CR-INIT"  // initializing: members being added, restarted or reconfigured
	lsStatePSMDBErr   = "CR-ERROR" // the operator could not bring the cluster to its spec
	// The agents' two states. Exactly one member holds the PITR lock at a time.
	lsStatePBMSlicing = "SLICING"  // this agent won the nomination and is streaming the oplog
	lsStatePBMIdle    = "PBM-IDLE" // running, lost the nomination — it is doing nothing
	lsStatePBMLost    = "PBM-LOST" // running and unable to reach the cluster: nothing is being written
)

var lsPSMDBStateMeaning = map[string]string{
	lsStatePSMDBReady: "the operator considers the cluster to match its spec — every member present, configured and up",
	lsStatePSMDBInit:  "the operator is changing the cluster: adding, restarting or reconfiguring members. Ordinary during a rollout, and a cluster that never leaves it is stuck",
	lsStatePSMDBErr:   "the operator could not bring the cluster to its spec and said so",
	lsStatePBMSlicing: "this agent holds the PITR lock and is streaming the oplog to object storage — the only member of its replica set that is",
	lsStatePBMIdle:    "the agent is up and lost the nomination: another member is doing the work, and this log will say almost nothing",
	lsStatePBMLost:    "the agent cannot reach the cluster, so it can neither slice the oplog nor record that it failed to — PBM keeps its log inside MongoDB",
}

// ---------------------------------------------------------------- folding

// lsPBMHeader matches pbm-agent's own line: RFC3339 with a numeric offset, a one-letter
// level, an optional [context] tag, then the message.
//
//	2026-08-22T08:35:40.000+0000 I [backup/2026-08-22T08:35:39Z] backup finished
//	2026-08-22T08:33:43.000+0000 W [agentCheckup] storage is not initialized
var lsPBMHeader = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{4}|[+-]\d{2}:\d{2}))\s+([DIWEF])\s+(?:\[([^\]]+)\]\s+)?(.*)$`)

// lsPBMGoLine matches the OTHER format in the same file, and it is the interesting one.
//
//	2026/08/22 08:32:15 [entrypoint] starting `pbm-agent`
//	2026/08/22 08:42:19 [ERROR] writing log: db: server selection error: …
//
// The first shape is the container's entrypoint wrapper, and it is the only place a
// crash-looping agent is recorded. The second is PBM's own logger falling back to stderr
// because it could not write into MongoDB — PBM keeps its log in the database, so every
// line in this shape was written while the database was unreachable.
var lsPBMGoLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})\s+(?:\[([^\]]+)\]\s+)?(.*)$`)

// lsFoldPBM splits a pbm-agent log into records, reading both of its formats.
//
// Both halves are wanted, not one or the other — the same shape as the PostgreSQL/Patroni
// pair and the Valkey/systemd one, and for the same reason: the two processes writing into
// this file describe different halves of the same incident, and a rule needs to be able to
// require one or the other. They are marked with different subsystems so it can.
func lsFoldPBM(data string) []lsRecord {
	var out []lsRecord
	var cur *lsRecord
	lastTS := 0.0
	push := func(r lsRecord) {
		if cur != nil {
			out = append(out, *cur)
		}
		cp := r
		cur = &cp
	}
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			if cur != nil {
				cur.Body = append(cur.Body, "")
			}
			continue
		}
		if m := lsPBMHeader.FindStringSubmatch(line); m != nil {
			rec := lsRecord{Line: i + 1, Time: m[1], Level: lsPBMLevel(m[2]),
				Subsys: lsSubsysPBM, Thread: m[3], Text: strings.TrimSpace(m[4])}
			rec.TS, lastTS = lsPBMTime(m[1], lastTS)
			rec.Approx = rec.TS == lastTS && rec.Time == ""
			push(rec)
			continue
		}
		if m := lsPBMGoLine.FindStringSubmatch(line); m != nil {
			// The tag decides which of the two writers this is, and therefore which
			// catalogue may speak about it.
			sub, level := lsSubsysPBMEntry, "INFO"
			if strings.EqualFold(m[2], "ERROR") {
				sub, level = lsSubsysPBM, "ERROR"
			}
			rec := lsRecord{Line: i + 1, Time: m[1], Level: level, Subsys: sub,
				Thread: m[2], Text: strings.TrimSpace(m[3])}
			if ts, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.UTC); err == nil {
				rec.TS = float64(ts.UnixNano()) / 1e9
				lastTS = rec.TS
			} else {
				rec.TS, rec.Approx = lastTS, lastTS > 0
			}
			push(rec)
			continue
		}
		if cur == nil {
			continue
		}
		cur.Body = append(cur.Body, line)
	}
	if cur != nil {
		out = append(out, *cur)
	}
	for i := range out {
		for len(out[i].Body) > 0 && out[i].Body[len(out[i].Body)-1] == "" {
			out[i].Body = out[i].Body[:len(out[i].Body)-1]
		}
	}
	return out
}

// lsSubsysPBM marks pbm-agent's own records; lsSubsysPBMEntry the container entrypoint's.
const (
	lsSubsysPBM      = "pbm-agent"
	lsSubsysPBMEntry = "entrypoint"
)

// lsPBMLevel spells out PBM's one-letter level. F is Fatal, which for an agent means the
// process is about to exit and the entrypoint is about to restart it.
func lsPBMLevel(c string) string {
	switch c {
	case "D":
		return "DEBUG"
	case "I":
		return "INFO"
	case "W":
		return "WARNING"
	case "E":
		return "ERROR"
	case "F":
		return "FATAL"
	}
	return c
}

// lsPBMTime parses pbm-agent's stamp. It writes a four-digit offset (+0000) rather than
// RFC3339's +00:00, which time.RFC3339 will not accept, so both are tried.
func lsPBMTime(s string, last float64) (float64, float64) {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if ts, err := time.Parse(layout, s); err == nil {
			v := float64(ts.UnixNano()) / 1e9
			return v, v
		}
	}
	return last, last
}

// ---------------------------------------------------------------- sniffing

// lsSniffPSMDB recognises the operator's log and the agents'.
//
// The operator's giveaway is `psmdb.percona.com`, which every reconcile record carries.
// It has to be checked before the PXC sniff for the same reason the mongos sniff runs
// before the replica-set one: the two operators write identically shaped lines and only
// the controller group tells them apart.
func lsSniffPSMDB(data string) string {
	head := data
	if len(head) > 256<<10 {
		head = head[:256<<10]
	}
	if strings.Contains(head, "psmdb.percona.com") ||
		strings.Contains(head, "PerconaServerMongoDB") ||
		strings.Contains(head, "percona-server-mongodb-operator") {
		return lsFlavourPSMDBOperator
	}
	for _, s := range []string{
		"[agentCheckup]", "starting `pbm-agent`", "pbm-agent exited", "[pitr]",
		"pitr nomination", "nomination list for", "[entrypoint] starting",
	} {
		if strings.Contains(head, s) {
			return lsFlavourPBMAgent
		}
	}
	return ""
}

// lsPSMDBNodeName names the operator source after the cluster it reconciles, the way
// lsOpNodeName does for PXC.
func lsPSMDBNodeName(recs []lsRecord) string {
	for _, r := range recs {
		for _, ln := range r.Body {
			if v, ok := strings.CutPrefix(ln, "PerconaServerMongoDB: "); ok {
				if n := lsOpObjName(v); n != "" {
					return "operator/" + n
				}
			}
		}
	}
	return ""
}

// lsPBMNodeName names an agent after the member it runs beside.
//
// The agent prints its own identity once per start, in the version block:
//
//	I node: rs0/my-cluster-name-rs0-1.my-cluster-name-rs0.psmdb.svc.cluster.local:27017
//
// which is the only line in the file that is unambiguously about ITSELF — everything else
// it writes about nominations and locks names other members too. Reading it matters more
// here than for any other source in this package, because a bundle holds one agent per
// member and three lanes all called "pbm" cannot be told apart. Every other candidate line
// gets this wrong: the nomination records list all three hosts, and the connection errors
// name whichever member the agent was trying to reach.
func lsPBMNodeName(recs []lsRecord) string {
	for _, r := range recs {
		if host, ok := lsPBMSelfHost(r.Text); ok {
			return host
		}
		// The version block is folded into the start record's body, so the line is
		// usually a continuation rather than a header of its own.
		for _, ln := range r.Body {
			if host, ok := lsPBMSelfHost(ln); ok {
				return host
			}
		}
	}
	return ""
}

// lsPBMSelfHost reads `node: <replset>/<host>:<port>` and returns the short host.
func lsPBMSelfHost(line string) (string, bool) {
	_, rest, ok := strings.Cut(strings.TrimSpace(line), "node: ")
	if !ok {
		return "", false
	}
	if _, hostport, ok := strings.Cut(rest, "/"); ok {
		rest = hostport
	}
	host, _, _ := strings.Cut(rest, ":")
	host = lsPITRShortHost(host)
	if host == "" {
		return "", false
	}
	return host, true
}

// lsPBMNoise is the agent's own bookkeeping: the nomination chatter every losing member
// writes on every cycle, and the debug records that describe the protocol rather than the
// outcome. Dropped rather than collapsed, for the same reason the PITR collector's
// inventory is — they are a running commentary, not repeats of one event.
func lsPBMNoise(text string) bool {
	for _, p := range []string{
		"waiting pitr nomination", "waiting for cluster ready status",
		"nomination list for", "nomination rs", "set candidates",
		"got epoch", "init backup meta", "start_ok", "start_catchup",
		"dropping tmp collections", "releasing lock", "uploading \"",
		"setting last write timestamp", "setting last common write timestamp",
	} {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}
