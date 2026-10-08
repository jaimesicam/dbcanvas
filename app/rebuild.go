package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rebuild.go — "Rebuild replica from primary": throw a replica's data away and copy it afresh
// from the cluster's primary, then put it back into replication. The remedy for a replica whose
// replication broke and will not resume (a duplicate key, a missing row, an errant transaction),
// one that drifted, or one whose data directory was lost.
//
// Each topology is rebuilt with its own tool, on the replica, so the data never passes through
// DBCanvas:
//
//   - MySQL / Percona Server 8.0.17+ replication: the CLONE plugin. The replica pulls a physical
//     copy straight from the primary, restarts on it, and replication resumes from exactly where
//     the copy ends — by GTID, or without GTID from the binlog position the clone recorded.
//   - MariaDB, and MySQL older than 8.0.17: a logical copy, mariadb-dump / mysqldump run on the
//     replica against the primary in one consistent snapshot, with the primary's position in it.
//     Every database but the server's own (mysql, sys) is copied; accounts stay as they were.
//   - Group Replication / InnoDB Cluster: rejoin the group with the clone threshold at 1, so
//     distributed recovery clones the member from a donor rather than replaying binlogs.
//   - Patroni: patronictl reinit. repmgr: repmgr standby clone, then register.
//   - MongoDB replica set member: an empty dbPath, so the member does a full initial sync.
//   - Galera (PXC, MariaDB Galera): drop grastate.dat, so the member rejoins with a full SST.
//
// A rebuild runs in the background (a clone of a large dataset takes as long as it takes) and is
// followed through GET …/rebuild; the timeline and the notification bell say how it ended.

const rebuildTimeout = 60 * time.Minute

var rebuildKinds = map[string]string{
	"mysql": "mysql", "mysqlcerepl": "mysql",
	"mariadbrepl": "mariadb",
	"innodb":      "gr", "mysqlceinnodb": "gr",
	"patroni": "patroni",
	"repmgr":  "repmgr",
	"psmrs":   "mongo", "psmdb": "mongo",
	"pxc": "galera", "mariadbgalera": "galera",
}

var rebuildRefuse = map[string]string{
	"spock":         "every Spock node is its own origin — there is no primary to copy from",
	"valkeycluster": "a Valkey cluster node resyncs from its primary by itself",
}

// rebuildJob is one rebuild, as GET …/rebuild reports it.
type rebuildJob struct {
	NodeID    string   `json:"nodeId"`
	Label     string   `json:"label"`
	From      string   `json:"from"`
	Method    string   `json:"method"`
	State     string   `json:"state"` // running | done | failed
	Steps     []string `json:"steps"`
	Error     string   `json:"error,omitempty"`
	StartedAt int64    `json:"startedAt"`
	EndedAt   int64    `json:"endedAt,omitempty"`
}

var rebuildJobs = struct {
	mu sync.Mutex
	m  map[string]*rebuildJob // "stack:node"
}{m: map[string]*rebuildJob{}}

func (j *rebuildJob) add(f string, args ...any) {
	rebuildJobs.mu.Lock()
	j.Steps = append(j.Steps, time.Now().Format("15:04:05")+"  "+fmt.Sprintf(f, args...))
	rebuildJobs.mu.Unlock()
}

func (j *rebuildJob) snapshot() rebuildJob {
	rebuildJobs.mu.Lock()
	defer rebuildJobs.mu.Unlock()
	c := *j
	c.Steps = append([]string(nil), j.Steps...)
	return c
}

// rebuildPlan is what a rebuild of one node would do, or why it cannot.
type rebuildPlan struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Method    string `json:"method,omitempty"` // what it will do, in words
	From      string `json:"from,omitempty"`   // the node it copies from
	FromID    string `json:"fromId,omitempty"`
	Warning   string `json:"warning,omitempty"`

	t       swTopo
	target  swMember
	primary swMember
	dialect sqlDialect
	clone   bool // MySQL: CLONE rather than a dump
	gtid    bool
}

// planRebuild works out how nid would be rebuilt, asking the servers who is primary now.
func (a *App) planRebuild(ctx context.Context, st Stack, nid string) rebuildPlan {
	t, err := a.groupTopology(st, nid, rebuildKinds, rebuildRefuse, false)
	if err != nil {
		return rebuildPlan{Reason: err.Error()}
	}
	p := rebuildPlan{Kind: t.Kind, t: t}
	p.target, _ = t.member(nid)
	if len(t.Members) < 2 {
		return rebuildPlan{Reason: "no other member is running to copy from"}
	}
	states, pi := a.probeTopo(ctx, st, t)
	stateOf := map[string]swState{}
	for _, s := range states {
		stateOf[s.NodeID] = s
	}
	me := stateOf[nid]
	if t.Kind == "galera" {
		// Any Synced member can be the donor; Galera picks one.
		for _, m := range t.Members {
			if s := stateOf[m.Node.ID]; m.Node.ID != nid && s.Err == "" && !s.Down && s.State == "Synced" {
				p.primary = m
				break
			}
		}
		if p.primary.Node.ID == "" {
			return rebuildPlan{Reason: "no other member is Synced — a full state transfer needs a healthy donor"}
		}
		p.Supported, p.From, p.FromID = true, p.primary.Node.Label, p.primary.Node.ID
		p.Method = "Full state transfer (SST): the member leaves the cluster, forgets its state, and rejoins with a complete copy from a Synced donor."
		return p
	}
	switch {
	case pi == -2:
		return rebuildPlan{Reason: "more than one member reports itself primary — resolve that by hand first"}
	case pi < 0:
		return rebuildPlan{Reason: "no member reports itself primary right now — there is nothing to copy from"}
	case states[pi].NodeID == nid:
		return rebuildPlan{Reason: "this is the primary — rebuild a replica, or switch the primary first"}
	case me.Role == "arbiter":
		return rebuildPlan{Reason: "an arbiter holds no data"}
	}
	p.primary, _ = t.member(states[pi].NodeID)
	p.Supported, p.From, p.FromID = true, p.primary.Node.Label, p.primary.Node.ID
	switch t.Kind {
	case "mysql", "mariadb":
		mariadb := t.Kind == "mariadb"
		d := sqlDialect{MariaDB: mariadb, Client: "mysql"}
		if mariadb {
			d.Client = "mariadb"
		}
		ver, err := a.asyncSQL(ctx, st, p.primary, d, "SELECT @@version AS v")
		if err != nil {
			return rebuildPlan{Reason: "cannot ask the primary: " + err.Error()}
		}
		version := firstRow(ver)["v"]
		p.dialect = dialectFor(version, mariadb)
		if !mariadb {
			g, _ := a.asyncSQL(ctx, st, p.primary, d, "SELECT @@GLOBAL.gtid_mode AS g")
			p.gtid = firstRow(g)["g"] == "ON"
			p.clone = cloneCapable(version)
			if p.clone && !me.Down && me.Err == "" {
				// CLONE wants the same release on both ends.
				if tv, err := a.asyncSQL(ctx, st, p.target, d, "SELECT @@version AS v"); err == nil {
					if r := firstRow(tv)["v"]; swVersionRe.FindString(r) != swVersionRe.FindString(version) {
						p.clone = false
						p.Warning = fmt.Sprintf("%s runs %s and the primary %s — CLONE needs the same release, so this is a logical copy.", p.target.Node.Label, r, version)
					}
				}
			}
		} else {
			p.gtid = a.mariadbUsesGTID(ctx, st, t, d)
		}
		pos := "binlog position"
		if p.gtid {
			pos = "GTID"
		}
		if p.clone {
			p.Method = fmt.Sprintf("CLONE: %s pulls a physical copy of %s, restarts on it, and replicates again from where the copy ends (by %s).", p.target.Node.Label, p.primary.Node.Label, pos)
		} else {
			p.Method = fmt.Sprintf("Logical copy: %s on %s dumps every database of %s in one consistent snapshot and loads it, then replicates again from the snapshot's %s. Accounts are left as they are.",
				map[bool]string{true: "mariadb-dump", false: "mysqldump"}[mariadb], p.target.Node.Label, p.primary.Node.Label, pos)
		}
	case "gr":
		p.Method = "Group Replication clone: the member leaves the group and rejoins with the clone threshold at 1, so it is cloned from a donor and restarts on the copy."
	case "patroni":
		p.Method = "patronictl reinit: Patroni wipes the replica's data directory and takes a new base backup from the leader."
	case "repmgr":
		p.Method = "repmgr standby clone: PostgreSQL on the replica is stopped, its data directory emptied and cloned from the primary, then started and re-registered."
	case "mongo":
		p.Method = "Initial sync: mongod on the member is stopped, its dbPath emptied, and on restart it copies every collection from the replica set again."
	}
	return p
}

// cloneCapable: the CLONE plugin arrived in MySQL 8.0.17.
func cloneCapable(version string) bool {
	m := swVersionRe.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return maj > 8 || (maj == 8 && (min > 0 || patch >= 17))
}

// mariadbUsesGTID: whether this MariaDB cluster's replicas position by GTID — what any of them
// is doing now.
func (a *App) mariadbUsesGTID(ctx context.Context, st Stack, t swTopo, d sqlDialect) bool {
	for _, m := range t.Members {
		rows, err := a.asyncSQL(ctx, st, m, d, "SHOW SLAVE STATUS")
		if err != nil {
			continue
		}
		if u := firstRow(rows)["Using_Gtid"]; u != "" {
			return u != "No"
		}
	}
	return false
}

// ------------------------------------------------------------------------------- handlers

func rebuildKey(stackID int64, nid string) string { return fmt.Sprintf("%d:%s", stackID, nid) }

// handleRebuildInfo is what the confirm dialog shows: how the node would be rebuilt and from
// which member, or why it cannot be; and the running or last rebuild of it.
func (a *App) handleRebuildInfo(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	rebuildJobs.mu.Lock()
	j := rebuildJobs.m[rebuildKey(st.ID, nid)]
	rebuildJobs.mu.Unlock()
	resp := map[string]any{}
	if j != nil {
		resp["job"] = j.snapshot()
		if j.snapshot().State == "running" {
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	if r.URL.Query().Get("job") == "1" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	resp["plan"] = a.planRebuild(ctx, st, nid)
	writeJSON(w, http.StatusOK, resp)
}

// handleRebuild starts rebuilding {nid} from its primary.
func (a *App) handleRebuild(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	key := rebuildKey(st.ID, nid)
	rebuildJobs.mu.Lock()
	if j := rebuildJobs.m[key]; j != nil && j.State == "running" {
		rebuildJobs.mu.Unlock()
		writeErr(w, http.StatusConflict, "this node is already being rebuilt")
		return
	}
	rebuildJobs.mu.Unlock()
	pctx, pcancel := context.WithTimeout(r.Context(), 20*time.Second)
	p := a.planRebuild(pctx, st, nid)
	pcancel()
	if !p.Supported {
		writeErr(w, http.StatusConflict, p.Reason)
		return
	}
	// One operation per cluster at a time: a rebuild and a switchover of the same frame would
	// pull the primary in two directions.
	lockKey := fmt.Sprintf("%d:%s", st.ID, p.t.Frame.ID)
	switchLocks.mu.Lock()
	if switchLocks.m[lockKey] {
		switchLocks.mu.Unlock()
		writeErr(w, http.StatusConflict, "a switchover or another rebuild is running on this cluster")
		return
	}
	switchLocks.m[lockKey] = true
	switchLocks.mu.Unlock()

	j := &rebuildJob{NodeID: nid, Label: p.target.Node.Label, From: p.From, Method: p.Method, State: "running", StartedAt: time.Now().Unix()}
	rebuildJobs.mu.Lock()
	rebuildJobs.m[key] = j
	rebuildJobs.mu.Unlock()
	markNodeAction(st.ID, nid)
	a.recordStackEvent(st.ID, nid, "action", "info", fmt.Sprintf("Rebuild of %s from %s started", p.target.Node.Label, p.From), p.Method, u.Username)

	go func() {
		defer func() { switchLocks.mu.Lock(); delete(switchLocks.m, lockKey); switchLocks.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), rebuildTimeout)
		defer cancel()
		// Keep the watcher quiet about the replica it is taking apart, for as long as it takes.
		stopQuiet := make(chan struct{})
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopQuiet:
					return
				case <-t.C:
					markNodeAction(st.ID, nid)
				}
			}
		}()
		err := a.runRebuild(ctx, st, p, j)
		close(stopQuiet)
		markNodeAction(st.ID, nid)
		clearLiveRoles(st.ID)
		rebuildJobs.mu.Lock()
		j.EndedAt = time.Now().Unix()
		if err != nil {
			j.State, j.Error = "failed", err.Error()
		} else {
			j.State = "done"
		}
		rebuildJobs.mu.Unlock()
		took := time.Duration(j.EndedAt-j.StartedAt) * time.Second
		if err != nil {
			j.add("failed: %v", err)
			a.recordStackEvent(st.ID, nid, "action", "error", fmt.Sprintf("Rebuild of %s failed", p.target.Node.Label), err.Error(), u.Username)
			a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.rebuild", Severity: "error", StackID: st.ID, NodeID: nid,
				Title: "Rebuild failed", Body: fmt.Sprintf("%s: rebuilding %s from %s failed — %v", st.Name, p.target.Node.Label, p.From, err)})
			return
		}
		a.recordStackEvent(st.ID, nid, "action", "success", fmt.Sprintf("%s rebuilt from %s", p.target.Node.Label, p.From), "took "+took.String(), u.Username)
		a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.rebuild", Severity: "success", StackID: st.ID, NodeID: nid,
			Title: "Replica rebuilt", Body: fmt.Sprintf("%s: %s was rebuilt from %s in %s and is replicating.", st.Name, p.target.Node.Label, p.From, took)})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"job": j.snapshot()})
}

func (a *App) runRebuild(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	j.add("rebuilding %s from %s", p.target.Node.Label, p.From)
	if p.Warning != "" {
		j.add("%s", p.Warning)
	}
	switch p.Kind {
	case "mysql":
		if p.clone {
			return a.rebuildClone(ctx, st, p, j)
		}
		return a.rebuildDump(ctx, st, p, j)
	case "mariadb":
		return a.rebuildDump(ctx, st, p, j)
	case "gr":
		return a.rebuildGR(ctx, st, p, j)
	case "patroni":
		return a.rebuildPatroni(ctx, st, p, j)
	case "repmgr":
		return a.rebuildRepmgr(ctx, st, p, j)
	case "mongo":
		return a.rebuildMongo(ctx, st, p, j)
	case "galera":
		return a.rebuildGalera(ctx, st, p, j)
	}
	return errors.New("not supported")
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sqlAs runs statements on a member as root with the given password (a clone replaces the
// recipient's accounts with the donor's) and returns its --vertical rows.
func (a *App) sqlAs(ctx context.Context, st Stack, m swMember, client, pw, sql string, force bool) ([]map[string]string, error) {
	c := withEngine(ctx, a.depEngine(st, m.Node.ID))
	args := []string{client, "-uroot", "--vertical"}
	if force {
		args = append(args, "--force")
	}
	res, err := a.engCtx(c).ExecInput(c, m.Dep.ContainerID, "", args, []string{"MYSQL_PWD=" + pw}, []byte(sql+";\n"))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", m.Node.Label, err)
	}
	if res.Code != 0 && !force {
		return nil, fmt.Errorf("%s: %s", m.Node.Label, lastLines(strings.TrimSpace(res.Stderr), 300))
	}
	return verticalBlocks(res.Stdout), nil
}

// installPluginSQL installs a plugin unless it is there.
func installPluginSQL(name, so string) string {
	return fmt.Sprintf(`SET @have := (SELECT COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_NAME='%s');
SET @s := IF(@have = 0, "INSTALL PLUGIN %s SONAME '%s'", 'DO 0'); PREPARE p FROM @s; EXECUTE p; DEALLOCATE PREPARE p`, name, name, so)
}

// waitReplicating waits for a MySQL/MariaDB replica's both threads to run, and reports its lag.
func (a *App) waitReplicating(ctx context.Context, st Stack, m swMember, d sqlDialect, j *rebuildJob) error {
	deadline := time.Now().Add(2 * time.Minute)
	var last map[string]string
	for {
		rows, err := a.asyncSQL(ctx, st, m, d, d.status())
		if err == nil {
			last = firstRow(rows)
			if _, ok := replicaHealth(last); ok {
				j.add("%s is replicating (lag %ss)", m.Node.Label, firstNonEmpty(last["Seconds_Behind_Source"], last["Seconds_Behind_Master"], "?"))
				return nil
			}
		}
		if time.Now().After(deadline) {
			why := firstNonEmpty(last["Last_IO_Error"], last["Last_SQL_Error"], "the threads did not start")
			return fmt.Errorf("%s did not start replicating: %s", m.Node.Label, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------- MySQL CLONE

func (a *App) rebuildClone(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	d := p.dialect
	prim, tgt := p.primary, p.target
	psec := mysqlSecretsOf(prim.Dep)
	tsec := mysqlSecretsOf(tgt.Dep)
	user, pw := "dbcanvas_clone", randHex(16)
	donor := prim.FQDN + ":3306"

	// 1. The donor: the plugin, and an account the recipient clones as. Both replicate to the
	//    other replicas, and the drop afterwards does too, so every member ends where it started.
	if _, err := a.sqlAs(ctx, st, prim, d.Client, psec.RootPassword, installPluginSQL("clone", "mysql_clone.so")+fmt.Sprintf(`;
DROP USER IF EXISTS '%[1]s'@'%%';
CREATE USER '%[1]s'@'%%' IDENTIFIED BY '%[2]s';
GRANT BACKUP_ADMIN ON *.* TO '%[1]s'@'%%'`, user, pw), false); err != nil {
		return fmt.Errorf("prepare the donor: %v", err)
	}
	j.add("%s: clone plugin ready, temporary clone account created", prim.Node.Label)
	defer func() {
		a.sqlAs(context.Background(), st, prim, d.Client, psec.RootPassword, fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user), false)
	}()

	// 2. The recipient: replication stopped, writable for the plugin install, then the clone.
	//    super_read_only refuses INSTALL PLUGIN; the clone replaces everything anyway.
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, d.stop(), true); err != nil {
		return fmt.Errorf("stop replication: %v", err)
	}
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, "SET GLOBAL super_read_only=OFF;\n"+installPluginSQL("clone", "mysql_clone.so")+
		fmt.Sprintf(";\nSET GLOBAL clone_valid_donor_list='%s'", donor), false); err != nil {
		return fmt.Errorf("prepare the recipient: %v", err)
	}
	j.add("%s: replication stopped; cloning from %s — the copy takes as long as the data is large", tgt.Node.Label, donor)
	cloneSQL := fmt.Sprintf("CLONE INSTANCE FROM '%s'@'%s':3306 IDENTIFIED BY '%s'", user, prim.FQDN, pw)
	_, cerr := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, cloneSQL, false)
	// The statement ends with the server restarting under it, which the client reports as a lost
	// connection — or, on a server without a supervisor, as ERROR 3707. Either way the outcome is
	// read from performance_schema.clone_status once the server is back.
	if cerr != nil {
		j.add("clone statement returned: %s", lastLines(cerr.Error(), 160))
	}

	// 3. Wait for the restarted server and the clone's verdict. After the clone its accounts are
	//    the donor's, so root signs in with the donor's password.
	var status map[string]string
	deadline := time.Now().Add(15 * time.Minute)
	for {
		for _, pwTry := range []string{psec.RootPassword, tsec.RootPassword} {
			rows, err := a.sqlAs(ctx, st, tgt, d.Client, pwTry,
				"SELECT STATE AS state, ERROR_NO AS errno, ERROR_MESSAGE AS msg, BINLOG_FILE AS file, BINLOG_POSITION AS pos FROM performance_schema.clone_status", false)
			if err == nil {
				status = firstRow(rows)
				break
			}
		}
		if status != nil && (status["state"] == "Completed" || status["state"] == "Failed") {
			break
		}
		if status != nil && status["state"] == "In Progress" && strings.Contains(fmt.Sprint(cerr), "3707") {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the clone did not complete within 15 minutes")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		status = nil
	}
	if status["state"] != "Completed" {
		if status["state"] == "In Progress" {
			return errors.New("the copy finished but mysqld could not restart itself (it is not run by a supervisor) — restart the node, then rebuild again")
		}
		return fmt.Errorf("clone failed: %s %s", status["errno"], status["msg"])
	}
	j.add("clone completed; %s restarted on the copy (binlog %s:%s)", tgt.Node.Label, status["file"], status["pos"])

	// 4. Replicate again. The semi-sync plugins came with the copy (the donor's), so the replica's
	//    side is installed before the server is made read-only again.
	pwNow := psec.RootPassword
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, pwNow, "SELECT 1", false); err != nil {
		pwNow = tsec.RootPassword
	}
	if mysqlReplMode(p.t.Frame.ReplMode) == "semisync" {
		// The server came back super_read_only (it is persisted), which INSTALL PLUGIN respects;
		// read-only is put back with the replication below.
		a.sqlAs(ctx, st, tgt, d.Client, pwNow, "SET GLOBAL super_read_only=OFF", false)
		if err := a.swapSemisync(ctx, st, p.t, d, tgt, false); err != nil {
			j.add("semi-sync on %s: %v", tgt.Node.Label, err)
		} else {
			j.add("%s: semi-sync replica side enabled", tgt.Node.Label)
		}
	}
	pos, _ := strconv.ParseInt(status["pos"], 10, 64)
	change := d.changeSource(prim.FQDN, psec.ReplUser, psec.ReplPassword, p.gtid, status["file"], pos)
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, pwNow, d.resetAll()+";\n"+change+";\n"+d.start()+";\n"+d.readOnly(true), false); err != nil {
		return fmt.Errorf("re-attach replication: %v", err)
	}
	j.add("%s replicates from %s again (%s), read-only", tgt.Node.Label, prim.Node.Label, map[bool]string{true: "GTID auto-position", false: "from the clone's binlog position"}[p.gtid])
	return a.waitReplicating(ctx, st, tgt, d, j)
}

// --------------------------------------------------------------------------- logical copy

var dumpPosRe = regexp.MustCompile(`(?:MASTER|SOURCE)_LOG_FILE='([^']+)',\s*(?:MASTER|SOURCE)_LOG_POS=(\d+)`)
var dumpGTIDRe = regexp.MustCompile(`gtid_slave_pos='([^']*)'`)

// rebuildDumpScript runs on the replica: dump the primary's databases into a file and print the
// position the dump recorded. Nothing on the replica changes yet, so a dump that fails leaves it
// as it was. The account is a temporary one on the primary; --no-defaults, because root's
// ~/.my.cnf would otherwise sign the dump in as root, over MYSQL_PWD.
const rebuildDumpScript = `set -eo pipefail
F=/var/tmp/dbcanvas-rebuild.sql
rm -f "$F"
trap '[ $? -eq 0 ] || rm -f "$F"' EXIT
export MYSQL_PWD="$DUMP_PW"
mapfile -t DBARR <<<"$DBS"
"$DUMPER" --no-defaults -h "$PRIMARY" -u "$DUMP_USER" --single-transaction $POSFLAGS $DBFLAGS --databases "${DBARR[@]}" >"$F"
echo "dumped $(wc -c <"$F") bytes"
grep -m1 -oE "(MASTER|SOURCE)_LOG_FILE='[^']+', *(MASTER|SOURCE)_LOG_POS=[0-9]+" "$F" || true
grep -m1 -oE "gtid_slave_pos='[^']*'" "$F" || true`

// rebuildLoadScript loads the dump with binary logging off, and removes it either way.
const rebuildLoadScript = `set -eo pipefail
F=/var/tmp/dbcanvas-rebuild.sql
trap 'rm -f "$F"' EXIT
{ echo "SET SESSION sql_log_bin=0;"; cat "$F"; } | "$CLIENT" -uroot
echo loaded`

func (a *App) rebuildDump(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	d := p.dialect
	prim, tgt := p.primary, p.target
	psec := mysqlSecretsOf(prim.Dep)
	tsec := mysqlSecretsOf(tgt.Dep)
	user, pw := "dbcanvas_rebuild", randHex(16)

	// The databases to copy: everything but the server's own.
	rows, err := a.asyncSQL(ctx, st, prim, d, `SELECT SCHEMA_NAME AS db FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ('mysql','sys','information_schema','performance_schema') ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("list the primary's databases: %v", err)
	}
	var dbs []string
	for _, r := range rows {
		if r["db"] != "" {
			dbs = append(dbs, r["db"])
		}
	}

	// A temporary reader on the primary, out of the binary log: it exists only for the dump,
	// and the replica need not know about it.
	grant := "SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES, RELOAD, PROCESS, REPLICATION CLIENT"
	extra := ""
	if !d.MariaDB && d.Major != "5.7" {
		extra = fmt.Sprintf(";\nGRANT BACKUP_ADMIN ON *.* TO '%s'@'%%'", user)
	}
	if _, err := a.sqlAs(ctx, st, prim, d.Client, psec.RootPassword, fmt.Sprintf(`SET SESSION sql_log_bin=0;
DROP USER IF EXISTS '%[1]s'@'%%';
CREATE USER '%[1]s'@'%%' IDENTIFIED BY '%[2]s';
GRANT %[3]s ON *.* TO '%[1]s'@'%%'%[4]s`, user, pw, grant, extra), false); err != nil {
		return fmt.Errorf("create the dump account: %v", err)
	}
	defer a.sqlAs(context.Background(), st, prim, d.Client, psec.RootPassword,
		fmt.Sprintf("SET SESSION sql_log_bin=0;\nDROP USER IF EXISTS '%s'@'%%'", user), false)

	dumper := "mysqldump"
	posFlags := "--master-data=2"
	if d.MariaDB {
		dumper = "mariadb-dump"
		posFlags = "--master-data=2 --gtid"
	} else if d.Modern {
		posFlags = "--source-data=2"
	}
	if !d.MariaDB && p.gtid {
		posFlags += " --set-gtid-purged=ON"
	} else if !d.MariaDB && d.Major != "5.7" && !p.gtid {
		posFlags += " --set-gtid-purged=OFF"
	}
	dbFlags := "--add-drop-database --routines --events --triggers"
	dbList := strings.Join(dbs, "\n")
	if len(dbs) == 0 {
		// Nothing to copy but the position: a dump of nothing still records it.
		dbFlags, dbList = "--no-data --no-create-info --no-create-db --skip-triggers", "mysql"
	}
	j.add("dumping %d database(s) of %s on %s: %s", len(dbs), prim.Node.Label, tgt.Node.Label, firstNonEmpty(strings.Join(dbs, ", "), "none"))
	out, err := a.swExec(ctx, st, tgt, rebuildDumpScript, []string{
		"DUMPER=" + dumper, "PRIMARY=" + prim.FQDN, "DUMP_USER=" + user, "DUMP_PW=" + pw,
		"POSFLAGS=" + posFlags, "DBFLAGS=" + dbFlags, "DBS=" + dbList,
	})
	if err != nil {
		return fmt.Errorf("dump (nothing on %s was changed): %v", tgt.Node.Label, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "dumped ") {
			j.add("%s", line)
		}
	}
	m := dumpPosRe.FindStringSubmatch(out)
	if m == nil {
		a.swExec(ctx, st, tgt, `rm -f /var/tmp/dbcanvas-rebuild.sql`, nil)
		return fmt.Errorf("the dump did not record the primary's position (nothing on %s was changed)", tgt.Node.Label)
	}
	file := m[1]
	pos, _ := strconv.ParseInt(m[2], 10, 64)
	var gtidPos string
	if d.MariaDB && p.gtid {
		g := dumpGTIDRe.FindStringSubmatch(out)
		if g == nil {
			a.swExec(ctx, st, tgt, `rm -f /var/tmp/dbcanvas-rebuild.sql`, nil)
			return fmt.Errorf("the dump did not record the primary's GTID position (nothing on %s was changed)", tgt.Node.Label)
		}
		gtidPos = g[1]
	}
	j.add("the snapshot is at %s:%d%s", file, pos, map[bool]string{true: ", GTID " + gtidPos, false: ""}[gtidPos != ""])

	// The replica: replication stopped and forgotten, writable for the load, its own copies of
	// databases the primary no longer has dropped, and — with GTID on MySQL — its GTID history
	// cleared, since the dump sets gtid_purged to the primary's.
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, d.stop()+";\n"+d.resetAll(), true); err != nil {
		return fmt.Errorf("stop replication: %v", err)
	}
	prep := "SET GLOBAL read_only=OFF"
	if !d.MariaDB {
		prep = "SET GLOBAL super_read_only=OFF; SET GLOBAL read_only=OFF"
		if p.gtid {
			if d.binlogStatus() == "SHOW BINARY LOG STATUS" {
				prep += "; RESET BINARY LOGS AND GTIDS"
			} else {
				prep += "; RESET MASTER"
			}
		}
	}
	trows, _ := a.asyncSQL(ctx, st, tgt, d, `SELECT SCHEMA_NAME AS db FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ('mysql','sys','information_schema','performance_schema')`)
	keep := map[string]bool{}
	for _, db := range dbs {
		keep[db] = true
	}
	for _, r := range trows {
		if db := r["db"]; db != "" && !keep[db] {
			prep += fmt.Sprintf("; DROP DATABASE `%s`", strings.ReplaceAll(db, "`", "``"))
			j.add("%s: dropping %s, which the primary does not have", tgt.Node.Label, db)
		}
	}
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, "SET SESSION sql_log_bin=0; "+prep, false); err != nil {
		return fmt.Errorf("prepare %s: %v", tgt.Node.Label, err)
	}
	restoreRO := func() {
		a.sqlAs(context.Background(), st, tgt, d.Client, tsec.RootPassword, d.readOnly(true), false)
	}

	if _, err := a.swExec(ctx, st, tgt, rebuildLoadScript, []string{"CLIENT=" + d.Client, "MYSQL_PWD=" + tsec.RootPassword}); err != nil {
		restoreRO()
		return fmt.Errorf("load: %v", err)
	}
	j.add("loaded into %s", tgt.Node.Label)

	sql := d.changeSource(prim.FQDN, psec.ReplUser, psec.ReplPassword, p.gtid, file, pos)
	if gtidPos != "" {
		sql = fmt.Sprintf("SET GLOBAL gtid_slave_pos='%s';\n%s", gtidPos, sql)
	}
	if _, err := a.sqlAs(ctx, st, tgt, d.Client, tsec.RootPassword, sql+";\n"+d.start()+";\n"+d.readOnly(true), false); err != nil {
		restoreRO()
		return fmt.Errorf("re-attach replication: %v", err)
	}
	if d.MariaDB {
		a.mariadbReadOnlyFile(ctx, st, p.t, d, tgt, true)
	}
	j.add("%s replicates from %s again, read-only", tgt.Node.Label, prim.Node.Label)
	return a.waitReplicating(ctx, st, tgt, d, j)
}

// ------------------------------------------------------------------- Group Replication

func (a *App) rebuildGR(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	d := sqlDialect{Client: "mysql"}
	sec := mysqlSecretsOf(p.target.Dep)
	// Every possible donor needs the clone plugin; a secondary is super_read_only, which
	// INSTALL PLUGIN respects, so it is lifted around the install.
	for _, m := range p.t.Members {
		if _, err := a.sqlAs(ctx, st, m, d.Client, sec.RootPassword, `SET @ro := @@GLOBAL.super_read_only;
SET GLOBAL super_read_only=OFF;
`+installPluginSQL("clone", "mysql_clone.so")+`;
SET GLOBAL super_read_only=@ro`, false); err != nil {
			j.add("clone plugin on %s: %v", m.Node.Label, err)
		}
	}
	j.add("clone plugin ready on every member")
	if _, err := a.sqlAs(ctx, st, p.target, d.Client, sec.RootPassword, `STOP GROUP_REPLICATION;
SET GLOBAL group_replication_clone_threshold=1;
START GROUP_REPLICATION`, false); err != nil {
		// START returns before the clone restarts the server — or fails with the reason.
		if !strings.Contains(err.Error(), "2013") && !strings.Contains(err.Error(), "Lost connection") {
			return fmt.Errorf("rejoin with clone: %v", err)
		}
	}
	j.add("%s left the group and is rejoining by clone; it restarts on the copy", p.target.Node.Label)
	err := a.waitHealthy(ctx, st, p, j, 20*time.Minute, func(r *liveRole) bool {
		return r.Role == "secondary" && len(r.Problems) == 0
	})
	a.sqlAs(context.Background(), st, p.target, d.Client, sec.RootPassword, "SET GLOBAL group_replication_clone_threshold=9223372036854775807", true)
	return err
}

// waitHealthy polls the rebuilt member's live role until ok says it is back.
func (a *App) waitHealthy(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob, within time.Duration, ok func(*liveRole) bool) error {
	deadline := time.Now().Add(within)
	last := ""
	for {
		c := withEngine(ctx, a.depEngine(st, p.target.Node.ID))
		pc, cancel := context.WithTimeout(c, 15*time.Second)
		r := a.probeLiveRole(pc, st, p.target.Node.ID, p.target.Node.Type, len(p.t.Members))
		cancel()
		if r != nil && r.Err == "" && !r.Down && ok(r) {
			lag := ""
			if r.LagSec != nil {
				lag = fmt.Sprintf(", lag %.1fs", *r.LagSec)
			}
			j.add("%s is back: %s%s", p.target.Node.Label, firstNonEmpty(r.State, r.Role), lag)
			return nil
		}
		now := "starting"
		if r != nil {
			now = firstNonEmpty(r.Err, strings.Join(r.Problems, "; "), r.State, r.Role)
		}
		if now != last {
			j.add("%s: %s", p.target.Node.Label, clipLine(now, 160))
			last = now
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was not healthy within %s: %s", p.target.Node.Label, within, clipLine(now, 200))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ----------------------------------------------------------------------------- PostgreSQL

func (a *App) rebuildPatroni(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	out, err := a.swExec(ctx, st, p.target, `set -e
CONF=/etc/patroni/postgresql.yml
PCTL=$(command -v patronictl || echo /usr/local/bin/patronictl)
SCOPE=$(awk '/^scope:/{print $2; exit}' "$CONF" | tr -d "'\"")
NAME=$(awk '/^name:/{print $2; exit}' "$CONF" | tr -d "'\"")
"$PCTL" -c "$CONF" reinit "$SCOPE" "$NAME" --force --wait 2>&1 | tail -20`, nil)
	if err != nil {
		return fmt.Errorf("patronictl reinit: %v", err)
	}
	j.add("patronictl: %s", lastLines(out, 300))
	return a.waitHealthy(ctx, st, p, j, 20*time.Minute, func(r *liveRole) bool {
		return r.Role == "replica" && len(r.Problems) == 0
	})
}

func (a *App) rebuildRepmgr(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	var cfg repmgrConfig
	json.Unmarshal(p.target.Dep.Config, &cfg)
	var sec pgSecrets
	json.Unmarshal(p.target.Dep.Secrets, &sec)
	if cfg.PGMajor == "" || cfg.RepmgrConf == "" {
		return errors.New("this node's deployment does not record its PostgreSQL major — redeploy it")
	}
	osFam := firstNonEmpty(p.t.Frame.OS, cfg.OS)
	service := firstNonEmpty(cfg.Service, pgServiceName(osFam, cfg.PGMajor))
	dataDir := firstNonEmpty(cfg.DataDir, pgDataDir(osFam, cfg.PGMajor))
	bin := pgBinDir(osFam, cfg.PGMajor)
	if _, err := a.swExec(ctx, st, p.target, `systemctl stop "$SERVICE"`, []string{"SERVICE=" + service}); err != nil {
		return fmt.Errorf("stop PostgreSQL: %v", err)
	}
	j.add("%s: PostgreSQL stopped; cloning from %s", p.target.Node.Label, p.primary.FQDN)
	out, err := a.swExec(ctx, st, p.target, repmgrStandbyCloneScript, []string{
		"BINDIR=" + bin, "DATADIR=" + dataDir, "PRIMARY=" + p.primary.FQDN, "REPLUSER=" + firstNonEmpty(sec.ReplUser, "repmgr"), "CONF=" + cfg.RepmgrConf,
	})
	if err != nil {
		return fmt.Errorf("repmgr standby clone: %v", err)
	}
	j.add("repmgr: %s", lastLines(out, 200))
	if p.t.Frame.GenerateCert {
		if err := a.pgApplyCert(ctx, p.target.Dep.ContainerID, a.intranetContainerID(ctx, st), p.target.FQDN, dataDir,
			p.t.Frame.CertTTLValue, p.t.Frame.CertTTLUnit, func(s string) {}); err != nil {
			return fmt.Errorf("re-issue the certificate: %v", err)
		}
		j.add("TLS certificate re-issued")
	}
	if _, err := a.swExec(ctx, st, p.target, pgStartScript, []string{"SERVICE=" + service}); err != nil {
		return fmt.Errorf("start PostgreSQL: %v", err)
	}
	if out, err := a.swExec(ctx, st, p.target, repmgrStandbyRegisterScript, []string{"BINDIR=" + bin, "CONF=" + cfg.RepmgrConf}); err != nil {
		return fmt.Errorf("repmgr standby register: %v", err)
	} else {
		j.add("repmgr: %s", lastLines(out, 160))
	}
	return a.waitHealthy(ctx, st, p, j, 10*time.Minute, func(r *liveRole) bool {
		return r.Role == "replica" && len(r.Problems) == 0
	})
}

// -------------------------------------------------------------------------------- MongoDB

func (a *App) rebuildMongo(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	out, err := a.swExec(ctx, st, p.target, `set -e
DBPATH=$(awk '/dbPath:/{print $2; exit}' /etc/mongod.conf 2>/dev/null | tr -d "'\"")
DBPATH=${DBPATH:-/var/lib/mongo}
case "$DBPATH" in /|/etc*|/usr*|/var) echo "refusing to empty $DBPATH"; exit 1;; esac
systemctl stop mongod
find "$DBPATH" -mindepth 1 -delete
systemctl start mongod
echo "emptied $DBPATH"`, nil)
	if err != nil {
		return fmt.Errorf("empty the data directory: %v", err)
	}
	j.add("%s: %s; mongod restarted and is copying from the replica set", p.target.Node.Label, lastLines(out, 120))
	return a.waitHealthy(ctx, st, p, j, 30*time.Minute, func(r *liveRole) bool {
		return r.Role == "secondary"
	})
}

// --------------------------------------------------------------------------------- Galera

func (a *App) rebuildGalera(ctx context.Context, st Stack, p rebuildPlan, j *rebuildJob) error {
	unit := "mysql"
	if p.t.Frame.Type == "mariadbgalera" {
		unit = mariadbUnit()
	}
	// Leave the cluster and forget the state: without grastate.dat the member asks for a full
	// SST when it starts. The .cache directory an emulated (Rosetta) host leaves in mysql's home,
	// the data directory, is cleared too: the SST's cleanup cannot remove it and fails on it.
	if _, err := a.swExec(ctx, st, p.target, `set -e
for u in "$UNIT@bootstrap" "$UNIT"; do systemctl is-active --quiet "$u" && systemctl stop "$u" || true; done
rm -rf /var/lib/mysql/grastate.dat /var/lib/mysql/gvwstate.dat /var/lib/mysql/.cache
systemctl reset-failed "$UNIT" 2>/dev/null || true`, []string{"UNIT=" + unit}); err != nil {
		return fmt.Errorf("leave the cluster: %v", err)
	}
	j.add("%s left the cluster and forgot its state; starting it for a full state transfer", p.target.Node.Label)
	var out string
	var err error
	if unit == "mysql" {
		// PXC: the provisioner's own join, with its recovery for that same .cache directory.
		out, err = a.swExec(ctx, st, p.target, pxcJoinScript, []string{"ROOT_PW=" + mysqlSecretsOf(p.target.Dep).RootPassword})
	} else {
		out, err = a.swExec(ctx, st, p.target, `for i in 1 2 3; do
  systemctl reset-failed "$UNIT" 2>/dev/null || true
  systemctl start "$UNIT" 2>/dev/null && { echo "joined"; exit 0; }
  echo "start $i failed; clearing /var/lib/mysql/.cache and retrying"
  rm -rf /var/lib/mysql/.cache
done
exit 1`, []string{"UNIT=" + unit})
	}
	if out != "" {
		j.add("%s", lastLines(out, 200))
	}
	if err != nil {
		return fmt.Errorf("rejoin with SST: %v", err)
	}
	return a.waitHealthy(ctx, st, p, j, 30*time.Minute, func(r *liveRole) bool {
		return r.State == "Synced" && len(r.Problems) == 0
	})
}
