package main

// logsummary_k8s.go — reading the logs of a Percona Operator for MySQL (PXC) cluster
// running on a K3D frame.
//
// Every other source the Log Summary reads is a file inside a container it can exec into.
// A Kubernetes cluster is one more hop: the container DBCanvas can reach is the k3s node,
// and the logs are inside pods it schedules. So the collector runs `kubectl` on the node —
// the same way every other K3D feature in this app does (see kubectl in k3d.go) — and the
// hop is the only thing that is different. What comes back is parsed by the same code that
// parses an uploaded file.
//
// THREE sources per cluster, and each is a different document:
//
//	<cr>/operator   the percona-xtradb-cluster-operator Deployment's log: the decisions —
//	                rolling restarts, backups, restores, which pod it calls primary
//	<cr>-pxc-N      each member's mysqld error log, an ordinary Galera log
//	<cr>/pitr       the binlog collector Deployment's log, when point-in-time recovery is
//	                on: the only place the state of PITR is written down
//
// Two things about reading the member logs turned out to matter, and both were found by
// doing it:
//
//  1. `kubectl logs <pod> -c pxc` is NOT the error log. The pxc container's stdout is the
//     entrypoint's `bash -x` trace — `+ echo 'set wsrep_on=1;'`, `+ file_env
//     MYSQL_DATABASE` — because mysqld is started with `--log-error` pointing at a file on
//     the volume. Reading stdout gets you the shell script that started the server and
//     almost none of what the server said.
//
//  2. There is a second copy of it and it is JSON-wrapped. The pod's `logs` container is a
//     log collector that tails that same file and re-emits every line inside an envelope:
//
//     {"log":"2026-08-22T05:21:16.881342Z 2 [Note] … Synchronized with group…\n",
//     "file":"/var/lib/mysql/mysqld-error.log"}
//
//     Which is worth having precisely when the first path cannot be used: `kubectl exec`
//     needs a running container, and the member whose log you most want to read is the one
//     that is not running. So the file is the primary path and the sidecar is the
//     fallback, unwrapped back into the raw error log the parser expects.
//
// A third thing is worth knowing and cannot be fixed here: a RESTORE erases these files.
// The restore replaces the members' data directories, and mysqld-error.log lives in the
// data directory — measured, a member's log went from 923 lines to 313, all of them after
// the restore. Read them before you restore, or read the sidecar's copy, which lives in
// the container runtime rather than on the volume.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// lsPathPITR is the pseudo-path given to the binlog collector's log. It is what tells the
// builder which of the two controller formats this source is without re-sniffing, and it
// is shown in the sources table as the place the text came from — which for a pod log is a
// container, not a file.
const lsPathPITR = "kubectl logs deploy/<cluster>-pitr"

// lsPathPBM is the same for a PSMDB member's pbm-agent sidecar. Unlike PXC's single
// collector Deployment there is one of these PER MEMBER, and only one of them is ever
// doing the work — see lsPSMDBFindingPITRWho.
const lsPathPBM = "kubectl logs <pod> -c backup-agent"

// lsK8sTarget identifies one readable log inside a K3D cluster.
//
// The node id is composite, the same shape All-in-One uses: `<canvas node id>#<what>`,
// where `<what>` is `operator`, `pitr`, or a pod name. It has to be composite because one
// canvas node — the k3s server — is many logs, exactly as one All-in-One node is many
// database instances.
const (
	lsK8sOperator = "operator"
	lsK8sPITR     = "pitr"
	lsK8sEvents   = "events"
	// A member's backup agent is a suffix on the member's own target rather than a target
	// of its own, because it is a container inside that pod: `<node>#<pod>~pbm`.
	lsK8sPBMSuffix = "~pbm"
)

// lsK8sOperatorDeploy is the Deployment name each supported operator installs itself as.
var lsK8sOperatorDeploy = map[string]string{
	"pxc":   "percona-xtradb-cluster-operator",
	"psmdb": "percona-server-mongodb-operator",
	"pg":    "percona-postgresql-operator",
	"pgo":   "pgo",
	"cnpg":  "cloudnative-pg",
	"ps":    "percona-server-mysql-operator",
}

// lsK8sOperatorNS is the namespace an operator installs ITSELF into, when that is not the
// cluster's own. The four Percona operators live beside the cluster they manage; the two
// chart-installed ones do not — CloudNativePG's manager is a cluster-wide Deployment in
// its own namespace, so asking for it in the database's namespace finds nothing.
var lsK8sOperatorNS = map[string]string{"cnpg": "cnpg-system"}

// lsK8sMemberSelector is the label that finds a cluster's database pods, per operator.
//
// The PostgreSQL pair share Crunchy's label because Percona's operator is a fork of
// Crunchy's and drives the same custom resource — one of the few places the fork shows
// through. CloudNativePG has its own, and it is a label whose PRESENCE is the selector
// (`cnpg.io/instanceName`) rather than a value to match.
func lsK8sMemberSelector(operator, cr string) string {
	switch operator {
	case "psmdb":
		return "app.kubernetes.io/component=mongod,app.kubernetes.io/instance=" + cr
	case "pg", "pgo":
		return "postgres-operator.crunchydata.com/data=postgres," +
			"postgres-operator.crunchydata.com/cluster=" + cr
	case "cnpg":
		return "cnpg.io/cluster=" + cr
	case "ps":
		// The PS operator's members are `component=database, name=mysql` — the same pod
		// carries `component=database` for its router and orchestrator too, so both halves
		// are required.
		return "app.kubernetes.io/component=database,app.kubernetes.io/name=mysql," +
			"app.kubernetes.io/instance=" + cr
	}
	return "app.kubernetes.io/component=pxc,app.kubernetes.io/instance=" + cr
}

// listLogK8sTargets is every log a running K3D PXC cluster can offer.
//
// Only the `pxc` operator for now, and deliberately: the catalogue in
// logsummary_pxcop.go was written against that operator's vocabulary, and offering a
// PostgreSQL or MongoDB operator's log here would produce a source full of unclassified
// records with a PXC page's verdicts underneath it. The member logs of those clusters are
// a smaller step (they are ordinary PostgreSQL and mongod logs) and the shape here is
// ready for them.
func (a *App) listLogK8sTargets(u User) []qrTarget {
	out := []qrTarget{}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		for _, f := range doc.Frames {
			if f.Type != "k3d" {
				continue
			}
			if _, ok := lsK8sOperatorDeploy[f.K3DOperator]; !ok {
				continue
			}
			cfg, containerID, ok := a.k3dServerConfig(st.ID, doc, f.ID)
			if !ok || containerID == "" {
				continue
			}
			serverNode := lsK8sServerNodeID(doc, f.ID)
			if serverNode == "" {
				continue
			}
			out = append(out, qrTarget{
				StackID: st.ID, StackName: st.Name,
				NodeID: aioJoinTarget(serverNode, lsK8sOperator),
				Label:  cfg.ClusterName + " · operator",
				Engine: pktEngineOperator, Type: "k3d-" + f.K3DOperator + "-operator",
			})
			// The members, from the cluster itself rather than from cr.yaml's `size`: a
			// cluster mid-scale, mid-restore or with a member the scheduler could not
			// place has a different set of pods from the one its spec asks for, and the
			// pods that exist are the ones with logs.
			memberEngine := pktEngineMySQL
			switch f.K3DOperator {
			case "psmdb":
				memberEngine = pktEngineMongoDB
			case "pg", "pgo":
				memberEngine = pktEnginePostgres
			case "ps":
				memberEngine = pktEngineMySQL
			case "cnpg":
				// Not "postgres": a CloudNativePG member's log is its instance manager's,
				// with PostgreSQL's records wrapped inside it. The sniff splits them.
				memberEngine = pktEngineOperator
			}
			for _, pod := range a.lsK8sMemberPods(containerID, cfg.Namespace, cfg.ClusterName, f.K3DOperator) {
				out = append(out, qrTarget{
					StackID: st.ID, StackName: st.Name,
					NodeID: aioJoinTarget(serverNode, pod),
					Label:  pod,
					Engine: memberEngine, Type: "k3d-" + f.K3DOperator,
				})
				// PSMDB's backup agent is a sidecar in the member's own pod, so it is
				// offered beside it rather than as one collector for the cluster. Only one
				// of them is ever doing the work and none of them says which — that is
				// what makes reading all three the point.
				if f.K3DOperator == "psmdb" {
					out = append(out, qrTarget{
						StackID: st.ID, StackName: st.Name,
						NodeID: aioJoinTarget(serverNode, pod+lsK8sPBMSuffix),
						Label:  pod + " · backup agent",
						Engine: pktEngineOperator, Type: "k3d-psmdb-pbm",
					})
				}
			}
			// The namespace's Kubernetes Events, for every operator. Not a log — see
			// logsummary_k8sevents.go — and the only place the reason for a killed
			// container is written down.
			out = append(out, qrTarget{
				StackID: st.ID, StackName: st.Name,
				NodeID: aioJoinTarget(serverNode, lsK8sEvents),
				Label:  cfg.ClusterName + " · kubernetes events",
				Engine: pktEngineK8sEvents, Type: "k3d-events",
			})
			if f.K3DOperator == "pxc" && a.lsK8sHasPITR(containerID, cfg.Namespace, cfg.ClusterName) {
				out = append(out, qrTarget{
					StackID: st.ID, StackName: st.Name,
					NodeID: aioJoinTarget(serverNode, lsK8sPITR),
					Label:  cfg.ClusterName + " · binlog collector",
					Engine: pktEngineOperator, Type: "k3d-pxc-pitr",
				})
			}
		}
	}
	return out
}

// lsK8sServerNodeID is the canvas node holding the k3s server of a frame.
func lsK8sServerNodeID(doc designDoc, frameID string) string {
	best := ""
	for _, n := range doc.Nodes {
		if n.FrameID != frameID || n.Type != "k3d" {
			continue
		}
		if best == "" || n.Label < best {
			best = n.ID
		}
	}
	// The server is the lowest-labelled member (provisionK3DFrame sorts the same way), and
	// the id is what the collector needs, not the label.
	for _, n := range doc.Nodes {
		if n.FrameID == frameID && n.Type == "k3d" && n.ID == best {
			return n.ID
		}
	}
	return best
}

// lsK8sMemberPods lists the cluster's database pods, in ordinal order.
//
// The component label differs per operator and is the only reliable selector: a PSMDB
// cluster's pods also include mongos routers and config servers, which are not replica-set
// members of the shard and whose logs answer different questions.
func (a *App) lsK8sMemberPods(serverID, ns, cr, operator string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "pods",
		"-l", lsK8sMemberSelector(operator, cr),
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil
	}
	var pods []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			pods = append(pods, p)
		}
	}
	sort.Strings(pods)
	return pods
}

// lsK8sHasPITR reports whether the binlog collector Deployment exists. It only does when
// point-in-time recovery is enabled, and its absence is itself a fact the verdict uses.
func (a *App) lsK8sHasPITR(serverID, ns, cr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "deploy", cr+"-pitr",
		"-o", "jsonpath={.metadata.name}")
	return err == nil && strings.TrimSpace(out) != ""
}

// lsK8sResolveOp turns a composite target into everything the collector needs: the k3s
// node to run kubectl on, the namespace and cluster name, which log was asked for, and
// which operator the frame runs — the last of these decides where a member's log lives and
// what the operator's own Deployment is called.
func (a *App) lsK8sResolveOp(u User, stackID int64, target string) (serverID, ns, cr, what, operator string, ok bool) {
	nodeID, what := aioSplitTarget(target)
	if what == "" {
		return "", "", "", "", "", false
	}
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return "", "", "", "", "", false
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		return "", "", "", "", "", false
	}
	doc := buildDoc(st)
	for _, n := range doc.Nodes {
		if n.ID != nodeID || n.Type != "k3d" {
			continue
		}
		for _, f := range doc.Frames {
			if f.ID != n.FrameID {
				continue
			}
			if _, ok := lsK8sOperatorDeploy[f.K3DOperator]; !ok {
				continue
			}
			cfg, containerID, found := a.k3dServerConfig(stackID, doc, f.ID)
			if !found || containerID == "" {
				return "", "", "", "", "", false
			}
			return containerID, cfg.Namespace, cfg.ClusterName, what, f.K3DOperator, true
		}
	}
	return "", "", "", "", "", false
}

// lsK8sTail reads one of a cluster's three kinds of log and returns it with the "path" it
// came from — which for a pod is a kubectl invocation, printed so the sources table can
// say exactly what was run.
func (a *App) lsK8sTail(ctx context.Context, serverID, ns, cr, what, operator string, lines int) (text, path, engine string, err error) {
	n := strconv.Itoa(lines)
	switch {
	case what == lsK8sOperator:
		// The Deployment rather than a pod: an operator that was restarted has a new pod
		// name, and asking for the Deployment gets whichever one is running now.
		deploy := lsK8sOperatorDeploy[operator]
		if deploy == "" {
			return "", "", "", fmt.Errorf("no operator log for a %q frame", operator)
		}
		opNS := ns
		if v, ok := lsK8sOperatorNS[operator]; ok {
			opNS = v
		}
		out, err := a.kubectl(ctx, serverID, "-n", opNS, "logs", "deploy/"+deploy, "--tail="+n)
		if err != nil {
			return "", "", "", err
		}
		return out, "kubectl logs -n " + opNS + " deploy/" + deploy, pktEngineOperator, nil
	case what == lsK8sEvents:
		// `-o json` rather than the human table: the table drops the count and the
		// first-seen time, which are exactly what turns forty probe failures into one row
		// with a span instead of forty rows or one.
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "events", "-o", "json")
		if err != nil {
			return "", "", "", err
		}
		return out, "kubectl -n " + ns + " get events -o json", pktEngineK8sEvents, nil
	case what == lsK8sPITR:
		out, err := a.kubectl(ctx, serverID, "-n", ns, "logs", "deploy/"+cr+"-pitr",
			"-c", "pitr", "--tail="+n)
		if err != nil {
			return "", "", "", err
		}
		return out, lsPathPITR, pktEngineOperator, nil
	case strings.HasSuffix(what, lsK8sPBMSuffix):
		// A member's backup agent. There is no file to fall back to — pbm-agent writes its
		// log INTO MongoDB and only mirrors it to stderr — so the container log is the
		// only copy that exists outside the database.
		pod := strings.TrimSuffix(what, lsK8sPBMSuffix)
		out, err := a.kubectl(ctx, serverID, "-n", ns, "logs", pod, "-c", "backup-agent", "--tail="+n)
		if err != nil {
			return "", "", "", err
		}
		return out, pod + ": " + lsPathPBM, pktEngineOperator, nil
	}
	// A PostgreSQL member. The three operators split two ways and the split is the whole
	// point: Percona's and Crunchy's members run **Patroni**, whose stdout is what
	// `kubectl logs` returns, with PostgreSQL's own log a file beside it on the volume —
	// and both are wanted, for exactly the reason lsPGTailScript gives for a hand-built
	// Patroni node: the failover decision is in one and its consequence in the other.
	// CloudNativePG runs no Patroni and puts both into one JSON stream, which lsFoldCNPG
	// splits again.
	if operator == "pg" || operator == "pgo" {
		patroni, perr := a.kubectl(ctx, serverID, "-n", ns, "logs", what, "-c", "database", "--tail="+n)
		pg, _ := a.kubectl(ctx, serverID, "-n", ns, "exec", what, "-c", "database", "--",
			"bash", "-c", "tail -n "+n+" /pgdata/*/log/*.log 2>/dev/null")
		if perr != nil && strings.TrimSpace(pg) == "" {
			return "", "", "", perr
		}
		// PostgreSQL first, Patroni after: lsFoldPostgres recognises each line by its own
		// shape, and the builder sorts the merged events by time anyway.
		return strings.TrimSuffix(pg, "\n") + "\n" + patroni,
			what + ": /pgdata/*/log + patroni", pktEnginePostgres, nil
	}
	if operator == "cnpg" {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "logs", what, "-c", "postgres", "--tail="+n)
		if err != nil {
			return "", "", "", err
		}
		return out, what + ": instance manager + postgres", pktEngineOperator, nil
	}
	// A MySQL or MongoDB member. The log on the volume first — it is the raw thing the
	// engine's parser wants, and it holds more history than the container runtime keeps.
	logPath, container, engine := lsK8sMemberLog(operator)
	if logPath != "" {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "exec", what, "-c", container, "--",
			"tail", "-n", n, logPath)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, what + ":" + logPath, engine, nil
		}
	} else {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "logs", what, "-c", container, "--tail="+n)
		if err == nil && strings.TrimSpace(out) != "" {
			return out, what + ": " + container + " (stdout is the error log)", engine, nil
		}
	}
	// The sidecar's copy. This is the path that works when the member is not running —
	// which is the member whose log matters most.
	raw, err2 := a.kubectl(ctx, serverID, "-n", ns, "logs", what, "-c", "logs", "--tail="+n)
	if err2 != nil {
		return "", "", "", fmt.Errorf("%s: could not read the member's log (%v)", what, err2)
	}
	return lsK8sUnwrapCollector(raw), what + ": log-collector sidecar", engine, nil
}

// lsK8sMemberLog is where each operator's database container writes its log, which
// container it is, and which engine parses it.
//
// Both operators point the server at a file on the volume and run a log-collector sidecar
// that re-emits it — but at different paths, and PXC's `pxc` container additionally prints
// its entrypoint's `bash -x` trace to stdout while PSMDB's `mongod` container prints the
// JSON log itself. The file is the primary path for both, which makes that difference not
// matter.
func lsK8sMemberLog(operator string) (path, container, engine string) {
	switch operator {
	case "psmdb":
		return lsK8sMongoLogPath, "mongod", pktEngineMongoDB
	case "ps":
		// The one member log that needs no file at all: this operator's `mysql` container
		// prints the error log to stdout, so `kubectl logs` is already the right thing and
		// the exec path below simply falls through to it.
		return "", "mysql", pktEngineMySQL
	}
	return lsK8sErrorLogPath, "pxc", pktEngineMySQL
}

// lsK8sMongoLogPath is where the PSMDB image points mongod's systemLog.path. There is a
// second file beside it, `mongod.full.log`, which is the log-collector's own on-disk copy
// with an envelope around every line — the plain one is the log.
const lsK8sMongoLogPath = "/data/db/logs/mongod.log"

// lsK8sErrorLogPath is where the PXC image points mysqld's --log-error.
const lsK8sErrorLogPath = "/var/lib/mysql/mysqld-error.log"

// lsK8sUnwrapCollector turns the log-collector sidecar's JSON envelopes back into the
// error log they wrap.
//
// Each line is {"log":"<one error-log line>\n","file":"/var/lib/mysql/mysqld-error.log"}.
// A line that is not an envelope is kept as it is rather than dropped: the sidecar is
// fluent-bit and prints its own start-up banner in plain text, and silently swallowing
// anything unrecognised is how a parser hides the one line that mattered.
func lsK8sUnwrapCollector(raw string) string {
	var b strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		var env struct {
			Log  string `json:"log"`
			File string `json:"file"`
		}
		if json.Unmarshal([]byte(line), &env) == nil && env.Log != "" {
			b.WriteString(env.Log)
			if !strings.HasSuffix(env.Log, "\n") {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
