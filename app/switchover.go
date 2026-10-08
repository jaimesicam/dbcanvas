package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// switchover.go — making a replica the primary, from the canvas.
//
// One action, "promote this member", covers both directions: demoting a primary is promoting the
// replica that takes over from it, and the canvas asks which one. Each topology is switched the way
// its own tooling does it, and nothing is assumed from the design — the current primary is whoever
// the servers say it is (the Live view's probes), and the switch is refused rather than guessed at
// when that is unclear or a replica is not healthy.
//
//   - Patroni: patronictl switchover --candidate. Patroni demotes the leader and moves the lock.
//   - repmgr: repmgr standby switchover --siblings-follow on the candidate (the passwordless SSH it
//     needs is wired at deploy, repmgrssh.go).
//   - MongoDB replica set (and each shard / config replica set of a sharded cluster): freeze every
//     other secondary, step the primary down, wait for the candidate to win, unfreeze. Nothing in
//     the replica set config changes.
//   - Group Replication / InnoDB Cluster: group_replication_set_as_primary() — Group Replication
//     drains the old primary and moves writes itself. MySQL Router (InnoDB Cluster) follows.
//   - MySQL and MariaDB async replication, GTID or not, by hand, in the order that cannot lose a
//     transaction: freeze writes on the primary; read its final position; wait until the candidate
//     AND every other replica have applied exactly that much; stop the candidate and make it
//     writable; point the old primary and the other replicas at it — by GTID auto-positioning, or
//     without GTID from the candidate's own binlog coordinates, which are only meaningful because
//     everybody was made to stop at the same point first. Semi-sync roles move with the primary.
//     MariaDB's read_only does not stop accounts with SUPER (every admin/app account here), so its
//     freeze is a global read lock held for the length of the catch-up.
//
// HAProxy, ProxySQL, PgBouncer's follow timer and MySQL Router find the new primary themselves
// (by read_only, Patroni's REST API or pg_is_in_recovery()), so nothing on the proxy side is
// touched. The design is updated afterwards so a redeploy does not rebuild the old primary.

const switchoverTimeout = 4 * time.Minute

// switchKinds maps a frame type to the way it is switched over.
var switchKinds = map[string]string{
	"mysql": "mysql", "mysqlcerepl": "mysql",
	"mariadbrepl": "mariadb",
	"innodb":      "gr", "mysqlceinnodb": "gr",
	"patroni": "patroni",
	"repmgr":  "repmgr",
	"psmrs":   "mongo", "psmdb": "mongo",
}

// noPrimaryKinds are clusters with no single primary to move.
var noPrimaryKinds = map[string]string{
	"pxc":           "every member of a Galera cluster takes writes — there is no primary to move",
	"mariadbgalera": "every member of a Galera cluster takes writes — there is no primary to move",
	"spock":         "every Spock node takes writes — there is no primary to move",
	"valkeycluster": "this Valkey cluster has no replicas to promote",
}

type swMember struct {
	Node designNode
	Dep  Deployment
	Host string
	FQDN string
}

// swTopo is one replication group: a frame's members, or one replica set of a sharded cluster.
type swTopo struct {
	Kind    string
	Frame   designFrame
	Members []swMember
}

func (t swTopo) member(id string) (swMember, bool) {
	for _, m := range t.Members {
		if m.Node.ID == id {
			return m, true
		}
	}
	return swMember{}, false
}

// mongoGroup is which replica set a sharded-cluster member belongs to.
func mongoGroup(n designNode) string {
	if n.Role == "config" {
		return "cfg"
	}
	return fmt.Sprintf("rs%d", n.Shard)
}

// switchTopology finds the replication group node nid belongs to, or says why it has none.
func (a *App) switchTopology(st Stack, nid string) (swTopo, error) {
	return a.groupTopology(st, nid, switchKinds, noPrimaryKinds, true)
}

// groupTopology is switchTopology for any operation on a replication group: kinds maps the frame
// types it applies to, refuse the ones it explains it cannot; allRunning refuses a group with a
// member that is not running, and otherwise such members are left out.
func (a *App) groupTopology(st Stack, nid string, kinds, refuse map[string]string, allRunning bool) (swTopo, error) {
	doc := buildDoc(st)
	var node designNode
	found := false
	for _, n := range doc.Nodes {
		if n.ID == nid {
			node, found = n, true
		}
	}
	if !found {
		return swTopo{}, errors.New("no such node")
	}
	if node.FrameID == "" {
		return swTopo{}, errors.New("this node is not a member of a replicated cluster")
	}
	var frame designFrame
	for _, f := range doc.Frames {
		if f.ID == node.FrameID {
			frame = f
		}
	}
	if why, ok := refuse[frame.Type]; ok {
		return swTopo{}, errors.New(why)
	}
	kind, ok := kinds[frame.Type]
	if !ok {
		return swTopo{}, errors.New("this is not supported for this kind of cluster")
	}
	if frame.Type == "psmdb" && node.Role == "mongos" {
		return swTopo{}, errors.New("a mongos router has no replication role")
	}
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	t := swTopo{Kind: kind, Frame: frame}
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != frame.Type {
			continue
		}
		if frame.Type == "psmdb" && (n.Role == "mongos" || mongoGroup(n) != mongoGroup(node)) {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			if allRunning {
				return swTopo{}, fmt.Errorf("%s is not running — every member must be up to switch the primary", n.Label)
			}
			if n.ID == nid {
				return swTopo{}, fmt.Errorf("%s is not running — start the node first", n.Label)
			}
			continue
		}
		t.Members = append(t.Members, swMember{Node: n, Dep: dep, Host: hosts[n.ID], FQDN: fqdnOf(hosts[n.ID], domain)})
	}
	if len(t.Members) < 2 {
		return swTopo{}, errors.New("there is no other member to switch with")
	}
	return t, nil
}

// swState is one member as the servers report it right now.
type swState struct {
	NodeID   string   `json:"nodeId"`
	Label    string   `json:"label"`
	Role     string   `json:"role,omitempty"`
	Access   string   `json:"access,omitempty"`
	State    string   `json:"state,omitempty"`
	LagSec   *float64 `json:"lagSec,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Down     bool     `json:"down,omitempty"`
	Err      string   `json:"error,omitempty"`
	Primary  bool     `json:"primary"`
	// Eligible says whether this member could be made primary now; Why says why not.
	Eligible bool   `json:"eligible"`
	Why      string `json:"why,omitempty"`
}

// probeTopo asks every member for its role, in parallel, and works out the primary. It is the
// Live view's probe, uncached: a switch must not act on a role four seconds old.
func (a *App) probeTopo(ctx context.Context, st Stack, t swTopo) ([]swState, int) {
	out := make([]swState, len(t.Members))
	var wg sync.WaitGroup
	for i, m := range t.Members {
		wg.Add(1)
		go func(i int, m swMember) {
			defer wg.Done()
			c := withEngine(ctx, a.depEngine(st, m.Node.ID))
			r := a.probeLiveRole(c, st, m.Node.ID, m.Node.Type, 0)
			s := swState{NodeID: m.Node.ID, Label: m.Node.Label}
			if r != nil {
				s.Role, s.Access, s.State, s.LagSec, s.Problems, s.Down, s.Err = r.Role, r.Access, r.State, r.LagSec, r.Problems, r.Down, r.Err
			}
			s.Primary = s.Err == "" && (s.Role == "primary" || (s.Role == "standalone" && s.Access == "rw"))
			out[i] = s
		}(i, m)
	}
	wg.Wait()
	primary := -1
	for i, s := range out {
		if s.Primary {
			if primary >= 0 {
				primary = -2 // two members claim it: a split the canvas will not try to fix
			} else if primary == -1 {
				primary = i
			}
		}
	}
	for i := range out {
		s := &out[i]
		switch {
		case s.Primary:
			s.Why = "already the primary"
		case s.Down || s.Err != "":
			s.Why = "its database is not answering"
		case s.Role == "arbiter":
			s.Why = "an arbiter holds no data"
		case len(s.Problems) > 0 && (t.Kind == "mysql" || t.Kind == "mariadb" || t.Kind == "repmgr" || t.Kind == "patroni"):
			s.Why = "its replication is not healthy: " + s.Problems[0]
		default:
			s.Eligible = true
		}
	}
	return out, primary
}

// ------------------------------------------------------------------------------- handlers

// handleSwitchoverInfo is what the canvas builds the node's "Replication role" menu from: who is
// primary, and which members could take over.
func (a *App) handleSwitchoverInfo(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	t, err := a.switchTopology(st, nid)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false, "reason": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	states, primary := a.probeTopo(ctx, st, t)
	resp := map[string]any{"supported": true, "kind": t.Kind, "members": states}
	switch {
	case primary == -2:
		resp["supported"], resp["reason"] = false, "more than one member reports itself primary — resolve that by hand first"
	case primary < 0:
		resp["supported"], resp["reason"] = false, "no member reports itself primary right now"
	default:
		resp["primary"] = states[primary].NodeID
	}
	writeJSON(w, http.StatusOK, resp)
}

var switchLocks = struct {
	mu sync.Mutex
	m  map[string]bool
}{m: map[string]bool{}}

// handlePromote makes {nid} the primary of its replication group.
func (a *App) handlePromote(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	nid := r.PathValue("nid")
	// Only this cluster's own deploy blocks a switch, and switchTopology already refuses unless
	// every member is running. A deploy elsewhere in the stack — a node being added, another
	// cluster still provisioning — has nothing to do with this one.
	t, err := a.switchTopology(st, nid)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	lockKey := fmt.Sprintf("%d:%s", st.ID, t.Frame.ID)
	switchLocks.mu.Lock()
	if switchLocks.m[lockKey] {
		switchLocks.mu.Unlock()
		writeErr(w, http.StatusConflict, "a switchover is already running on this cluster")
		return
	}
	switchLocks.m[lockKey] = true
	switchLocks.mu.Unlock()
	defer func() { switchLocks.mu.Lock(); delete(switchLocks.m, lockKey); switchLocks.mu.Unlock() }()

	// Not the request's context: a browser that navigates away must not abandon a switch halfway.
	ctx, cancel := context.WithTimeout(context.Background(), switchoverTimeout)
	defer cancel()
	steps, status, err := a.switchPrimary(ctx, st, t, nid, u)
	if err != nil {
		if steps == nil {
			writeErr(w, status, err.Error())
			return
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "steps": steps})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "primary": nid, "steps": steps})
}

// switchPrimary makes nid the primary of t, with the frame's lock already held by the caller. It
// returns the steps taken; on failure an HTTP status for it, and nil steps when it was refused
// before anything was touched.
func (a *App) switchPrimary(ctx context.Context, st Stack, t swTopo, nid string, u User) ([]string, int, error) {
	states, pi := a.probeTopo(ctx, st, t)
	if pi < 0 {
		return nil, http.StatusConflict, errors.New("cannot tell which member is the primary right now — check the Live view")
	}
	var target swState
	for _, s := range states {
		if s.NodeID == nid {
			target = s
		}
	}
	if !target.Eligible {
		return nil, http.StatusConflict, fmt.Errorf("%s cannot be made primary: %s", target.Label, target.Why)
	}
	primary, _ := t.member(states[pi].NodeID)
	cand, _ := t.member(nid)
	var err error
	var others []swMember
	for _, m := range t.Members {
		if m.Node.ID != primary.Node.ID && m.Node.ID != cand.Node.ID {
			others = append(others, m)
		}
	}

	log := &swLog{}
	log.add("%s is primary; making %s the primary", primary.Node.Label, cand.Node.Label)
	markPlannedSwitch(st.ID)
	switch t.Kind {
	case "patroni":
		err = a.switchPatroni(ctx, st, t, primary, cand, log)
	case "repmgr":
		err = a.switchRepmgr(ctx, st, t, primary, cand, log)
	case "mongo":
		err = a.switchMongo(ctx, st, t, primary, cand, others, log)
	case "gr":
		err = a.switchGR(ctx, st, primary, cand, log)
	case "mysql", "mariadb":
		err = a.switchAsync(ctx, st, t, primary, cand, others, log)
	default:
		err = errors.New("not supported")
	}
	clearLiveRoles(st.ID)
	markPlannedSwitch(st.ID) // the window runs from the end of the switch, however long it took
	// A partial switch still moved the primary: the design must say so, or a redeploy rebuilds
	// the old one.
	var part *partialSwitch
	if errors.As(err, &part) {
		if changed, rerr := a.recordNewPrimary(st, t, primary, cand); rerr == nil && changed {
			log.add("design updated: %s is now marked primary", cand.Node.Label)
		}
	}
	if err != nil {
		log.add("failed: %v", err)
		a.recordStackEvent(st.ID, cand.Node.ID, "action", "error", fmt.Sprintf("Switchover to %s failed", cand.Node.Label), err.Error(), u.Username)
		a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.switchover", Severity: "error",
			Title: "Switchover failed", Body: fmt.Sprintf("%s: making %s primary failed — %v", st.Name, cand.Node.Label, err)})
		return log.lines, http.StatusConflict, err
	}
	if changed, err := a.recordNewPrimary(st, t, primary, cand); err != nil {
		log.add("the switch worked, but the design could not be updated: %v", err)
	} else if changed {
		log.add("design updated: %s is now marked primary", cand.Node.Label)
	}
	a.recordStackEvent(st.ID, cand.Node.ID, "action", "info", fmt.Sprintf("Switchover: %s made primary (was %s)", cand.Node.Label, primary.Node.Label), "", u.Username)
	a.notify(Notification{UserID: u.ID, Scope: "user", Type: "stack.switchover", Severity: "success",
		Title: "Primary switched", Body: fmt.Sprintf("%s: %s is now the primary (was %s).", st.Name, cand.Node.Label, primary.Node.Label)})
	return log.lines, http.StatusOK, nil
}

// partialSwitch is a switch whose candidate became primary, with something after that (a
// follower's re-point) left undone.
type partialSwitch struct{ msg string }

func (p *partialSwitch) Error() string { return p.msg }

type swLog struct{ lines []string }

func (l *swLog) add(f string, args ...any) { l.lines = append(l.lines, fmt.Sprintf(f, args...)) }

// clearLiveRoles drops a stack's cached roles so the Live view shows the switch at once.
func clearLiveRoles(stackID int64) {
	prefix := fmt.Sprintf("%d:", stackID)
	liveCache.mu.Lock()
	for k := range liveCache.role {
		if strings.HasPrefix(k, prefix) {
			delete(liveCache.role, k)
		}
	}
	liveCache.mu.Unlock()
}

// ----------------------------------------------------------------------------- exec helpers

// swExec runs a bash script in a member's container and returns its output, or an error carrying
// the end of it.
func (a *App) swExec(ctx context.Context, st Stack, m swMember, script string, env []string) (string, error) {
	c := withEngine(ctx, a.depEngine(st, m.Node.ID))
	res, err := a.engCtx(c).Exec(c, m.Dep.ContainerID, []string{"bash", "-c", script}, env)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if res.Code != 0 {
		return out, fmt.Errorf("%s", lastLines(out, 300))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ------------------------------------------------------------------------------- Patroni

func (a *App) switchPatroni(ctx context.Context, st Stack, t swTopo, primary, cand swMember, log *swLog) error {
	// --force with no --leader takes the current leader — the same call on every Patroni release,
	// where --leader/--master changed name between them.
	out, err := a.swExec(ctx, st, cand, `set -e
PCTL=$(command -v patronictl || echo /usr/local/bin/patronictl)
"$PCTL" -c /etc/patroni/postgresql.yml switchover --candidate "$CAND" --force 2>&1`, []string{"CAND=" + cand.Host})
	if err != nil {
		return fmt.Errorf("patronictl switchover: %v", err)
	}
	log.add("patronictl: %s", lastLines(out, 200))
	return a.waitPrimary(ctx, st, t, cand, log)
}

// waitPrimary polls until the candidate reports itself primary and every other member a replica.
func (a *App) waitPrimary(ctx context.Context, st Stack, t swTopo, cand swMember, log *swLog) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		states, pi := a.probeTopo(ctx, st, t)
		if pi >= 0 && states[pi].NodeID == cand.Node.ID {
			log.add("%s reports itself primary", cand.Node.Label)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become primary within 90s", cand.Node.Label)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// -------------------------------------------------------------------------------- repmgr

func (a *App) switchRepmgr(ctx context.Context, st Stack, t swTopo, primary, cand swMember, log *swLog) error {
	var cfg struct {
		RepmgrConf string `json:"repmgrConf"`
		RepmgrBin  string `json:"repmgrBin"`
	}
	json.Unmarshal(cand.Dep.Config, &cfg)
	if cfg.RepmgrConf == "" || cfg.RepmgrBin == "" {
		return errors.New("this node's deployment does not record where repmgr is — redeploy it")
	}
	out, err := a.swExec(ctx, st, cand, `runuser -u postgres -- "$BIN" -f "$CONF" standby switchover --siblings-follow 2>&1`,
		[]string{"BIN=" + cfg.RepmgrBin, "CONF=" + cfg.RepmgrConf})
	if err != nil {
		return fmt.Errorf("repmgr standby switchover: %v", err)
	}
	log.add("repmgr: %s", lastLines(out, 300))
	return a.waitPrimary(ctx, st, t, cand, log)
}

// ------------------------------------------------------------------------------- MongoDB

func (a *App) switchMongo(ctx context.Context, st Stack, t swTopo, primary, cand swMember, others []swMember, log *swLog) error {
	run := func(m swMember, cmd bson.D, out any) error {
		c, ok := a.dbConnFor(st, m.Node.ID)
		if !ok {
			return fmt.Errorf("%s: no connection", m.Node.Label)
		}
		cctx := withEngine(ctx, a.depEngine(st, m.Node.ID))
		client, closer, err := a.mongoClientFor(cctx, c)
		if err != nil {
			return fmt.Errorf("%s: %v", m.Node.Label, err)
		}
		defer closer()
		res := client.Database("admin").RunCommand(cctx, cmd)
		if out != nil {
			return res.Decode(out)
		}
		return res.Err()
	}
	// The candidate must be electable: priority 0 and hidden members never become primary.
	var conf struct {
		Config struct {
			Members []struct {
				Host     string  `bson:"host"`
				Priority float64 `bson:"priority"`
				Hidden   bool    `bson:"hidden"`
			} `bson:"members"`
		} `bson:"config"`
	}
	if err := run(primary, bson.D{{Key: "replSetGetConfig", Value: 1}}, &conf); err != nil {
		return fmt.Errorf("read the replica set config: %v", err)
	}
	for _, m := range conf.Config.Members {
		if strings.HasPrefix(m.Host, cand.FQDN+":") && (m.Priority == 0 || m.Hidden) {
			return fmt.Errorf("%s has priority 0 or is hidden — it can never be elected", cand.Node.Label)
		}
	}
	// Freeze everybody else, so the candidate is the only member that can win the election.
	var frozen []swMember
	defer func() {
		for _, m := range frozen {
			if err := run(m, bson.D{{Key: "replSetFreeze", Value: 0}}, nil); err == nil {
				log.add("%s unfrozen", m.Node.Label)
			}
		}
	}()
	for _, m := range others {
		if err := run(m, bson.D{{Key: "replSetFreeze", Value: 120}}, nil); err != nil {
			if strings.Contains(err.Error(), "arbiter") {
				continue
			}
			return fmt.Errorf("freeze %s: %v", m.Node.Label, err)
		}
		frozen = append(frozen, m)
		log.add("%s frozen for the election", m.Node.Label)
	}
	// secondaryCatchUpPeriodSecs: the primary refuses to step down until a secondary has caught up,
	// so no acknowledged write is left behind.
	err := run(primary, bson.D{{Key: "replSetStepDown", Value: 60}, {Key: "secondaryCatchUpPeriodSecs", Value: 30}}, nil)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "connection") {
		return fmt.Errorf("step down %s: %v", primary.Node.Label, err)
	}
	log.add("%s stepped down", primary.Node.Label)
	return a.waitPrimary(ctx, st, t, cand, log)
}

// ---------------------------------------------------------------- Group Replication / InnoDB

func (a *App) switchGR(ctx context.Context, st Stack, primary, cand swMember, log *swLog) error {
	sec := mysqlSecretsOf(cand.Dep)
	uuid, err := a.swExec(ctx, st, cand, `mysql -uroot -N -B -e "SELECT @@server_uuid"`, []string{"MYSQL_PWD=" + sec.RootPassword})
	if err != nil {
		return fmt.Errorf("read %s's server_uuid: %v", cand.Node.Label, err)
	}
	// Run on the current primary: group_replication_set_as_primary waits for its in-flight
	// transactions, then hands the role over.
	out, err := a.swExec(ctx, st, primary, `mysql -uroot -N -B -e "SELECT group_replication_set_as_primary('$UUID')"`,
		[]string{"MYSQL_PWD=" + sec.RootPassword, "UUID=" + strings.TrimSpace(uuid)})
	if err != nil {
		return fmt.Errorf("group_replication_set_as_primary: %v", err)
	}
	log.add("Group Replication: %s", out)
	return nil
}

func mysqlSecretsOf(d Deployment) pxcSecrets {
	var s pxcSecrets
	json.Unmarshal(d.Secrets, &s)
	if s.RootUser == "" {
		s.RootUser = "root"
	}
	if s.ReplUser == "" {
		s.ReplUser = "repl"
	}
	return s
}

// -------------------------------------------------------------------- MySQL / MariaDB async

// sqlDialect is the replication vocabulary of one server: MySQL 8.0.23+ says SOURCE/REPLICA,
// older MySQL and every MariaDB say MASTER/SLAVE.
type sqlDialect struct {
	MariaDB bool
	Major   string // "5.7", "8.0", "8.4", "9.7", "10.11", "11.4"…
	Modern  bool   // CHANGE REPLICATION SOURCE / START REPLICA (MySQL 8.0.23+)
	Client  string
}

var swVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

func dialectFor(version string, mariadb bool) sqlDialect {
	d := sqlDialect{MariaDB: mariadb, Client: "mysql"}
	if mariadb {
		d.Client = "mariadb"
	}
	m := swVersionRe.FindStringSubmatch(version)
	if m == nil {
		return d
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	d.Major = m[1] + "." + m[2]
	if !mariadb {
		d.Modern = maj > 8 || (maj == 8 && (min > 0 || patch >= 23))
	}
	return d
}

func (d sqlDialect) stop() string {
	if d.Modern {
		return "STOP REPLICA FOR CHANNEL ''"
	}
	if d.MariaDB {
		return "STOP SLAVE"
	}
	return "STOP SLAVE FOR CHANNEL ''"
}
func (d sqlDialect) start() string {
	if d.Modern {
		return "START REPLICA FOR CHANNEL ''"
	}
	if d.MariaDB {
		return "START SLAVE"
	}
	return "START SLAVE FOR CHANNEL ''"
}
func (d sqlDialect) resetAll() string {
	if d.Modern {
		return "RESET REPLICA ALL FOR CHANNEL ''"
	}
	if d.MariaDB {
		return "RESET SLAVE ALL"
	}
	return "RESET SLAVE ALL FOR CHANNEL ''"
}
func (d sqlDialect) status() string {
	if d.Modern {
		return "SHOW REPLICA STATUS FOR CHANNEL ''"
	}
	if d.MariaDB {
		return "SHOW SLAVE STATUS"
	}
	return "SHOW SLAVE STATUS FOR CHANNEL ''"
}

// binlogStatus is SHOW BINARY LOG STATUS from 8.2 on, SHOW MASTER STATUS before (and on MariaDB).
func (d sqlDialect) binlogStatus() string {
	if !d.MariaDB && d.Major != "" && d.Major != "5.7" && d.Major != "8.0" && d.Major != "8.1" {
		return "SHOW BINARY LOG STATUS"
	}
	return "SHOW MASTER STATUS"
}

// posWait waits for the replica's SQL thread to reach a source position; >= 0 means reached.
func (d sqlDialect) posWait(file string, pos int64, timeout int) string {
	switch {
	case d.MariaDB:
		return fmt.Sprintf("SELECT MASTER_POS_WAIT('%s', %d, %d) AS w", file, pos, timeout)
	case d.Major == "5.7" || (d.Major == "8.0" && !d.Modern):
		return fmt.Sprintf("SELECT MASTER_POS_WAIT('%s', %d, %d, '') AS w", file, pos, timeout)
	}
	return fmt.Sprintf("SELECT SOURCE_POS_WAIT('%s', %d, %d, '') AS w", file, pos, timeout)
}

// changeSource points the default channel at a new source, by GTID or from a binlog position.
func (d sqlDialect) changeSource(host, user, pw string, gtid bool, file string, pos int64) string {
	q := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	if d.MariaDB {
		p := "MASTER_USE_GTID=slave_pos"
		if !gtid {
			p = fmt.Sprintf("MASTER_LOG_FILE='%s', MASTER_LOG_POS=%d, MASTER_USE_GTID=no", q(file), pos)
		}
		return fmt.Sprintf("CHANGE MASTER TO MASTER_HOST='%s', MASTER_PORT=3306, MASTER_USER='%s', MASTER_PASSWORD='%s', %s",
			q(host), q(user), q(pw), p)
	}
	if d.Modern {
		p := "SOURCE_AUTO_POSITION=1"
		if !gtid {
			p = fmt.Sprintf("SOURCE_AUTO_POSITION=0, SOURCE_LOG_FILE='%s', SOURCE_LOG_POS=%d", q(file), pos)
		}
		return fmt.Sprintf("CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=3306, SOURCE_USER='%s', SOURCE_PASSWORD='%s', %s, GET_SOURCE_PUBLIC_KEY=1 FOR CHANNEL ''",
			q(host), q(user), q(pw), p)
	}
	p := "MASTER_AUTO_POSITION=1"
	if !gtid {
		p = fmt.Sprintf("MASTER_AUTO_POSITION=0, MASTER_LOG_FILE='%s', MASTER_LOG_POS=%d", q(file), pos)
	}
	extra := ""
	if d.Major == "8.0" {
		extra = ", GET_MASTER_PUBLIC_KEY=1"
	}
	return fmt.Sprintf("CHANGE MASTER TO MASTER_HOST='%s', MASTER_PORT=3306, MASTER_USER='%s', MASTER_PASSWORD='%s', %s%s FOR CHANNEL ''",
		q(host), q(user), q(pw), p, extra)
}

// readOnly makes a server read-only (on) or writable (off), persistently. MySQL's super_read_only
// stops every account; MariaDB has none, so its read_only is persisted in the drop-in the
// provisioner writes (mariadbAttachScript).
// mariadbStrictRO reports whether this MariaDB has read_only=NO_LOCK_NO_ADMIN (11.0+): read-only
// for admin accounts too — every application account here holds ALL PRIVILEGES — while replication
// still applies. It is what MySQL calls super_read_only.
func (d sqlDialect) mariadbStrictRO() bool {
	if !d.MariaDB {
		return false
	}
	maj, _ := strconv.Atoi(strings.SplitN(d.Major, ".", 2)[0])
	return maj >= 11
}

// roValue is the read_only setting that actually stops writes on this server.
func (d sqlDialect) roValue() string {
	if d.mariadbStrictRO() {
		return "NO_LOCK_NO_ADMIN"
	}
	return "ON"
}

func (d sqlDialect) readOnly(on bool) string {
	v := "OFF"
	if on {
		v = d.roValue()
	}
	if d.MariaDB {
		return fmt.Sprintf("SET GLOBAL read_only=%s", v)
	}
	scope := "PERSIST"
	if d.Major == "5.7" {
		scope = "GLOBAL"
	}
	if on {
		return fmt.Sprintf("SET %[1]s read_only=ON; SET %[1]s super_read_only=ON", scope)
	}
	return fmt.Sprintf("SET %[1]s super_read_only=OFF; SET %[1]s read_only=OFF", scope)
}

// asyncSQL runs statements on one member as root and returns its --vertical output as rows.
func (a *App) asyncSQL(ctx context.Context, st Stack, m swMember, d sqlDialect, sql string) ([]map[string]string, error) {
	sec := mysqlSecretsOf(m.Dep)
	c := withEngine(ctx, a.depEngine(st, m.Node.ID))
	res, err := a.engCtx(c).ExecInput(c, m.Dep.ContainerID, "", []string{d.Client, "-uroot", "--vertical"},
		[]string{"MYSQL_PWD=" + sec.RootPassword}, []byte(sql+";\n"))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", m.Node.Label, err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("%s: %s", m.Node.Label, lastLines(strings.TrimSpace(res.Stderr), 300))
	}
	return verticalBlocks(res.Stdout), nil
}

func firstRow(rows []map[string]string) map[string]string {
	if len(rows) == 0 {
		return map[string]string{}
	}
	return rows[0]
}

// replicaHealth reads one member's default channel: its source and whether both threads run.
func replicaHealth(row map[string]string) (source string, running bool) {
	source = firstNonEmpty(row["Source_Host"], row["Master_Host"])
	io := firstNonEmpty(row["Replica_IO_Running"], row["Slave_IO_Running"])
	sq := firstNonEmpty(row["Replica_SQL_Running"], row["Slave_SQL_Running"])
	return source, io == "Yes" && sq == "Yes"
}

func (a *App) switchAsync(ctx context.Context, st Stack, t swTopo, primary, cand swMember, others []swMember, log *swLog) error {
	mariadb := t.Kind == "mariadb"
	d := sqlDialect{MariaDB: mariadb, Client: "mysql"}
	if mariadb {
		d.Client = "mariadb"
	}
	ver, err := a.asyncSQL(ctx, st, primary, d, "SELECT @@version AS v")
	if err != nil {
		return err
	}
	version := firstRow(ver)["v"]
	d = dialectFor(version, mariadb)
	replicas := append([]swMember{cand}, others...)

	// GTID is what the servers run with, not what the design asked for. MariaDB always has GTIDs;
	// whether its replicas position by them is the candidate's Using_Gtid, read below.
	gtid := false
	if !mariadb {
		g, err := a.asyncSQL(ctx, st, primary, d, "SELECT @@GLOBAL.gtid_mode AS g")
		if err != nil {
			return err
		}
		gtid = firstRow(g)["g"] == "ON"
	}

	// Preflight: every replica must be replicating from the primary, healthy, before anything
	// changes. A broken one would be left pointing at a server that is no longer primary.
	for _, m := range replicas {
		rows, err := a.asyncSQL(ctx, st, m, d, d.status())
		if err != nil {
			return err
		}
		row := firstRow(rows)
		src, ok := replicaHealth(row)
		if !strings.EqualFold(strings.TrimSuffix(src, "."), primary.FQDN) && !strings.EqualFold(src, primary.Host) {
			return fmt.Errorf("%s replicates from %q, not from the primary %s — fix that first", m.Node.Label, src, primary.FQDN)
		}
		if !ok {
			return fmt.Errorf("%s is not replicating (both threads must be running) — fix that first", m.Node.Label)
		}
		if mariadb && m.Node.ID == cand.Node.ID {
			gtid = row["Using_Gtid"] != "" && row["Using_Gtid"] != "No"
		}
	}
	mode := "binlog file/position (no GTID)"
	if gtid {
		mode = "GTID"
	}
	log.add("replication by %s; %s %s", mode, map[bool]string{true: "MariaDB", false: "MySQL"}[mariadb], version)

	// 0. Semi-sync: the old primary will need the replica plugin, and INSTALL PLUGIN writes a
	//    system table, which super_read_only refuses once the primary is frozen. Installing it
	//    changes nothing by itself; the switch only flips the enable variables later.
	if !mariadb && mysqlReplMode(t.Frame.ReplMode) == "semisync" {
		if err := a.semisyncInstall(ctx, st, d, primary, false); err != nil {
			return fmt.Errorf("prepare semi-sync on %s: %v", primary.Node.Label, err)
		}
	}

	// 1. Freeze writes on the primary. MySQL: super_read_only stops every account. MariaDB: a
	//    global read lock held by a background session for the length of the catch-up, because
	//    read_only alone does not stop accounts with SUPER.
	unfreeze := func() {}
	if mariadb && d.mariadbStrictRO() {
		if _, err := a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=NO_LOCK_NO_ADMIN"); err != nil {
			return fmt.Errorf("freeze writes on %s: %v", primary.Node.Label, err)
		}
		unfreeze = func() { a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=OFF") }
		log.add("%s frozen: read_only=NO_LOCK_NO_ADMIN (refuses admin accounts too)", primary.Node.Label)
	} else if mariadb {
		if _, err := a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=ON"); err != nil {
			return err
		}
		env := []string{"MYSQL_PWD=" + mysqlSecretsOf(primary.Dep).RootPassword}
		// Released on every way out from here — including a lock that was asked for but not
		// confirmed, which would otherwise be granted later and hold writes off by itself.
		release := func() { a.swExec(ctx, st, primary, mariadbLockRelease, env) }
		if _, err := a.swExec(ctx, st, primary, mariadbLockTake, env); err != nil {
			release()
			a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=OFF")
			return fmt.Errorf("freeze writes on %s: %v", primary.Node.Label, err)
		}
		unfreeze = func() { release(); a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=OFF") }
		defer release()
		log.add("%s frozen: read_only on and a global read lock held", primary.Node.Label)
	} else {
		if _, err := a.asyncSQL(ctx, st, primary, d, "SET GLOBAL read_only=ON; SET GLOBAL super_read_only=ON"); err != nil {
			return fmt.Errorf("freeze writes on %s: %v", primary.Node.Label, err)
		}
		unfreeze = func() { a.asyncSQL(ctx, st, primary, d, "SET GLOBAL super_read_only=OFF; SET GLOBAL read_only=OFF") }
		log.add("%s frozen: super_read_only on", primary.Node.Label)
	}

	// 2. Where the primary stopped. MySQL with GTID waits on the GTID set: gtid_executed counts
	//    what a server wrote itself as well as what it replicated. MariaDB always waits on binlog
	//    coordinates, GTID or not: MASTER_GTID_WAIT compares against gtid_slave_pos, which leaves
	//    out what a server wrote itself — so a former primary never "reaches" a position whose
	//    last transaction it originated, and the switch back to it timed out. Every replica is
	//    replicating from this primary (the preflight checked), so its file/position is exact.
	var waitSQL string
	gtidWait := gtid && !mariadb
	if gtidWait {
		rows, err := a.asyncSQL(ctx, st, primary, d, "SELECT @@GLOBAL.gtid_executed AS g")
		if err != nil {
			unfreeze()
			return err
		}
		g := strings.ReplaceAll(firstRow(rows)["g"], "\n", "")
		waitSQL = "SELECT " + fmt.Sprintf("WAIT_FOR_EXECUTED_GTID_SET('%s', 60)", g) + " AS w"
		log.add("%s stopped at GTID %s", primary.Node.Label, clipLine(g, 120))
	} else {
		rows, err := a.asyncSQL(ctx, st, primary, d, d.binlogStatus())
		if err != nil {
			unfreeze()
			return err
		}
		f := firstRow(rows)
		pos, _ := strconv.ParseInt(f["Position"], 10, 64)
		if f["File"] == "" {
			unfreeze()
			return fmt.Errorf("%s has no binary log position — is log_bin on?", primary.Node.Label)
		}
		waitSQL = d.posWait(f["File"], pos, 60)
		log.add("%s stopped at %s:%d", primary.Node.Label, f["File"], pos)
	}

	// 3. Every replica catches up to exactly that point.
	for _, m := range replicas {
		rows, err := a.asyncSQL(ctx, st, m, d, waitSQL)
		if err != nil {
			unfreeze()
			return err
		}
		w := firstRow(rows)["w"]
		n, perr := strconv.Atoi(w)
		ok := perr == nil && n >= 0
		if gtidWait {
			ok = perr == nil && n == 0
		}
		if !ok {
			unfreeze()
			return fmt.Errorf("%s did not catch up with the primary within 60s (wait returned %q) — nothing was changed, writes are back on %s",
				m.Node.Label, w, primary.Node.Label)
		}
		log.add("%s caught up", m.Node.Label)
	}

	// 4. Stop every replica — not only the candidate — and note where the candidate's own binlog
	//    ends. With no GTID, that is the position the old primary and the other replicas resume
	//    from, and it is only right if none of them applies anything else first: once the old
	//    primary follows the candidate it writes the candidate's new transactions into its own
	//    binlog (log_replica_updates), and a replica still attached to it would apply them there,
	//    then again from the candidate — a duplicate key. Stopped now, nothing arrives by that route.
	stopped := []swMember{}
	for _, m := range replicas {
		if _, err := a.asyncSQL(ctx, st, m, d, d.stop()); err != nil {
			for _, s := range stopped {
				a.asyncSQL(ctx, st, s, d, d.start())
			}
			unfreeze()
			return err
		}
		stopped = append(stopped, m)
	}
	log.add("replication stopped on %d replicas", len(stopped))
	var cFile string
	var cPos int64
	if !gtid {
		rows, err := a.asyncSQL(ctx, st, cand, d, d.binlogStatus())
		if err != nil {
			for _, m := range stopped {
				a.asyncSQL(ctx, st, m, d, d.start())
			}
			unfreeze()
			return err
		}
		cFile = firstRow(rows)["File"]
		cPos, _ = strconv.ParseInt(firstRow(rows)["Position"], 10, 64)
		if cFile == "" {
			for _, m := range stopped {
				a.asyncSQL(ctx, st, m, d, d.start())
			}
			unfreeze()
			return fmt.Errorf("%s has no binary log position — it cannot act as a source", cand.Node.Label)
		}
		log.add("%s's binary log ends at %s:%d", cand.Node.Label, cFile, cPos)
	}

	// 5. The candidate becomes the primary. From here the switch goes forward, not back.
	stmts := []string{d.resetAll(), d.readOnly(false)}
	if _, err := a.asyncSQL(ctx, st, cand, d, strings.Join(stmts, "; ")); err != nil {
		return fmt.Errorf("make %s writable: %v", cand.Node.Label, err)
	}
	if mariadb {
		a.mariadbReadOnlyFile(ctx, st, t, d, cand, false)
	}
	log.add("%s is writable and replicates from nobody", cand.Node.Label)
	if err := a.swapSemisync(ctx, st, t, d, cand, true); err != nil {
		log.add("semi-sync on %s: %v", cand.Node.Label, err)
	}

	sec := mysqlSecretsOf(cand.Dep)
	// 6. The old primary follows the candidate. MariaDB's GTID replica position starts from what it
	//    has itself written; the lock is released before it can apply anything.
	if mariadb && !d.mariadbStrictRO() {
		env := []string{"MYSQL_PWD=" + mysqlSecretsOf(primary.Dep).RootPassword}
		a.swExec(ctx, st, primary, mariadbKillClients, env)
		a.swExec(ctx, st, primary, mariadbLockRelease, env)
		log.add("%s: client sessions ended and the read lock released", primary.Node.Label)
	}
	follow := func(m swMember, old bool) error {
		var s []string
		if !old {
			s = append(s, d.stop())
		}
		change := d.changeSource(cand.FQDN, sec.ReplUser, sec.ReplPassword, gtid, cFile, cPos)
		if mariadb && gtid && old {
			// A demoted MariaDB primary has replicated nothing: its position is what it wrote
			// itself, which current_pos takes from its binlog. Setting gtid_slave_pos instead
			// writes a system table, which read_only=NO_LOCK_NO_ADMIN refuses.
			change = strings.Replace(change, "MASTER_USE_GTID=slave_pos", "MASTER_USE_GTID=current_pos", 1)
		}
		s = append(s, change, d.start())
		if old {
			s = append(s, d.readOnly(true))
		}
		if _, err := a.asyncSQL(ctx, st, m, d, strings.Join(s, "; ")); err != nil {
			return err
		}
		if mariadb && old {
			a.mariadbReadOnlyFile(ctx, st, t, d, m, true)
		}
		return nil
	}
	if err := a.swapSemisync(ctx, st, t, d, primary, false); err != nil {
		log.add("semi-sync on %s: %v", primary.Node.Label, err)
	}
	var failed []string
	if err := follow(primary, true); err != nil {
		failed = append(failed, err.Error())
	} else {
		log.add("%s is read-only and replicates from %s", primary.Node.Label, cand.Node.Label)
	}
	for _, m := range others {
		if err := follow(m, false); err != nil {
			failed = append(failed, err.Error())
		} else {
			log.add("%s now replicates from %s", m.Node.Label, cand.Node.Label)
		}
	}

	// 7. Every follower's threads must come up.
	for _, m := range append([]swMember{primary}, others...) {
		ok := false
		for i := 0; i < 15 && !ok; i++ {
			rows, err := a.asyncSQL(ctx, st, m, d, d.status())
			if err == nil {
				_, ok = replicaHealth(firstRow(rows))
				if !ok && i == 14 {
					r := firstRow(rows)
					failed = append(failed, fmt.Sprintf("%s: replication not running — %s", m.Node.Label,
						firstNonEmpty(r["Last_IO_Error"], r["Last_SQL_Error"], "threads stopped")))
				}
			}
			if !ok {
				time.Sleep(2 * time.Second)
			}
		}
		if ok {
			log.add("%s replicating", m.Node.Label)
		}
	}
	if len(failed) > 0 {
		return &partialSwitch{fmt.Sprintf("%s is the new primary, but: %s", cand.Node.Label, strings.Join(failed, "; "))}
	}
	return nil
}

// mariadbLockTake freezes a MariaDB primary: a background session takes FLUSH TABLES WITH READ
// LOCK and sleeps holding it. The session is found again by the column alias in its query — not
// a comment, which the mariadb client strips before sending. The sleep is the ceiling on how long
// a lock left behind by a failed switch could hold writes off.
const mariadbLockTake = `nohup mariadb -uroot -e "FLUSH TABLES WITH READ LOCK; SELECT SLEEP(240) AS dbcanvas_switchover_lock" >/dev/null 2>&1 &
for i in $(seq 1 30); do
  mariadb -uroot -N -e "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO LIKE '%AS dbcanvas_switchover_lock%' AND INFO NOT LIKE '%PROCESSLIST%' AND STATE LIKE 'User sleep%'" | grep -q '^1$' && exit 0
  sleep 1
done
echo "could not take the global read lock within 30s (a long transaction holds it off)"; exit 1`

// mariadbKillClients ends every application session on a MariaDB about to be demoted while its
// global read lock still holds them: a write queued behind the lock would otherwise run the
// moment the lock goes — on a server that is no longer primary, since read_only before 11.0 does
// not stop accounts with SUPER. The client gets an error and reconnects, through its proxy, to the
// new primary. Replication threads, system threads and the lock session itself are spared.
const mariadbKillClients = `for id in $(mariadb -uroot -N -e "SELECT ID FROM information_schema.PROCESSLIST WHERE USER NOT IN ('system user','event_scheduler','root') AND COMMAND NOT IN ('Daemon','Binlog Dump','Binlog Dump GTID','Slave_IO','Slave_SQL','Slave_worker') AND ID <> CONNECTION_ID()"); do mariadb -uroot -e "KILL CONNECTION $id" 2>/dev/null; done; true`

// mariadbLockRelease ends every switchover lock session, granted or still waiting for the lock.
const mariadbLockRelease = `for id in $(mariadb -uroot -N -e "SELECT ID FROM information_schema.PROCESSLIST WHERE INFO LIKE '%AS dbcanvas_switchover_lock%' AND INFO NOT LIKE '%PROCESSLIST%'"); do mariadb -uroot -e "KILL $id" 2>/dev/null; done; true`

// swapSemisync moves the semi-sync role with the primary, when the frame uses semi-sync.
func (a *App) swapSemisync(ctx context.Context, st Stack, t swTopo, d sqlDialect, m swMember, toPrimary bool) error {
	if mysqlReplMode(t.Frame.ReplMode) != "semisync" {
		return nil
	}
	if d.MariaDB {
		on, off := "rpl_semi_sync_master_enabled", "rpl_semi_sync_slave_enabled"
		if !toPrimary {
			on, off = off, on
		}
		_, err := a.swExec(ctx, st, m, `set -e
mariadb -uroot -e "SET GLOBAL $ON=ON; SET GLOBAL $OFF=OFF"
sed -i -E '/^rpl_semi_sync_(master|slave)_enabled/d' "$CNF"
printf '%s=ON\n' "$ON" >>"$CNF"`, []string{"MYSQL_PWD=" + mysqlSecretsOf(m.Dep).RootPassword, "ON=" + on, "OFF=" + off, "CNF=" + mariadbCnfPath(t.Frame.OS)})
		return err
	}
	major := d.Major
	if major != "5.7" && major != "8.0" {
		major = "8.4" // the source/replica plugin names, from 8.4 on
	}
	sp, sso, sv := semisyncSource(major)
	rp, rso, rv := semisyncReplica(major)
	install, enable, disable := sp, sv, rv
	so := sso
	if !toPrimary {
		install, enable, disable, so = rp, rv, sv, rso
	}
	scope := persistScope(major)
	sql := fmt.Sprintf(`SET @have := (SELECT COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_NAME='%s');
SET @s := IF(@have = 0, "INSTALL PLUGIN %s SONAME '%s'", 'DO 0'); PREPARE p FROM @s; EXECUTE p; DEALLOCATE PREPARE p;
SET %s %s=ON;
SET @s := IF((SELECT COUNT(*) FROM performance_schema.global_variables WHERE VARIABLE_NAME='%s') > 0, 'SET %s %s=OFF', 'DO 0'); PREPARE p FROM @s; EXECUTE p; DEALLOCATE PREPARE p`,
		install, install, so, scope, enable, disable, scope, disable)
	_, err := a.asyncSQL(ctx, st, m, d, sql)
	return err
}

// semisyncInstall installs the semi-sync plugin a member will need in its new role, if it is
// missing — while the member is still writable.
func (a *App) semisyncInstall(ctx context.Context, st Stack, d sqlDialect, m swMember, toPrimary bool) error {
	major := d.Major
	if major != "5.7" && major != "8.0" {
		major = "8.4"
	}
	plugin, so, _ := semisyncReplica(major)
	if toPrimary {
		plugin, so, _ = semisyncSource(major)
	}
	_, err := a.asyncSQL(ctx, st, m, d, fmt.Sprintf(`SET @have := (SELECT COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_NAME='%s');
SET @s := IF(@have = 0, "INSTALL PLUGIN %s SONAME '%s'", 'DO 0'); PREPARE p FROM @s; EXECUTE p; DEALLOCATE PREPARE p`, plugin, plugin, so))
	return err
}

// mariadbReadOnlyFile writes or removes the read_only drop-in, so a restart keeps the role.
func (a *App) mariadbReadOnlyFile(ctx context.Context, st Stack, t swTopo, d sqlDialect, m swMember, on bool) {
	dir, _ := mariadbCnfDir(t.Frame.OS)
	script := `rm -f "$DIR/zz-dbcanvas-readonly.cnf"`
	if on {
		script = `printf '[mysqld]\nread_only=%s\n' "$RO" >"$DIR/zz-dbcanvas-readonly.cnf"`
	}
	a.swExec(ctx, st, m, script, []string{"DIR=" + dir, "RO=" + d.roValue()})
}

// ------------------------------------------------------------------------------- the design

// recordNewPrimary writes the switch into the design and the deployments, so a redeploy builds the
// cluster as it now is and the Live view's "≠ design" does not fire. The design is edited as raw
// JSON — it holds canvas fields the Go structs do not model — and only "role" changes, and only
// where the member was marked primary to begin with.
// It reports whether the design changed: clusters whose members carry no primary role (Patroni,
// repmgr, MongoDB, Group Replication) have nothing there to update.
func (a *App) recordNewPrimary(st Stack, t swTopo, primary, cand swMember) (bool, error) {
	var doc map[string]any
	if err := json.Unmarshal(st.Design, &doc); err != nil {
		return false, err
	}
	changed := false
	nodes, _ := doc["nodes"].([]any)
	var oldNode, newNode map[string]any
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		switch m["id"] {
		case primary.Node.ID:
			oldNode = m
		case cand.Node.ID:
			newNode = m
		}
	}
	if oldNode != nil && newNode != nil && oldNode["role"] == "primary" {
		was := newNode["role"]
		if was == nil || was == "" || was == "primary" {
			was = "secondary"
		}
		oldNode["role"], newNode["role"] = was, "primary"
		b, err := json.Marshal(doc)
		if err != nil {
			return false, err
		}
		if err := a.store.UpdateStack(st.ID, st.Name, b); err != nil {
			return false, err
		}
		changed = true
	}
	// The deployments' recorded roles, where they have one (primary|secondary, leader|replica,
	// primary|standby) — what the node's profile shows.
	for _, m := range t.Members {
		dep, err := a.store.GetDeployment(st.ID, m.Node.ID)
		if err != nil {
			continue
		}
		var cfg map[string]any
		if json.Unmarshal(dep.Config, &cfg) != nil || cfg == nil {
			continue
		}
		role, _ := cfg["role"].(string)
		var next string
		switch {
		case m.Node.ID == cand.Node.ID:
			next = map[string]string{"secondary": "primary", "replica": "leader", "standby": "primary"}[role]
		case role == "primary" || role == "leader":
			next = map[string]string{"primary": "secondary", "leader": "replica"}[role]
			if t.Kind == "repmgr" {
				next = "standby"
			}
		}
		_, hasSource := cfg["sourceHost"]
		if next == "" && !hasSource {
			continue
		}
		if next != "" {
			cfg["role"] = next
		}
		if hasSource {
			if m.Node.ID == cand.Node.ID {
				cfg["sourceHost"] = ""
			} else {
				cfg["sourceHost"] = cand.FQDN
			}
		}
		if _, ok := cfg["readOnly"]; ok {
			cfg["readOnly"] = m.Node.ID != cand.Node.ID
		}
		dep.Config = mustJSON(cfg)
		a.store.UpsertDeployment(dep)
	}
	return changed, nil
}
