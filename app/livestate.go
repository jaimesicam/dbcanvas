package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// livestate.go — the canvas's Live view: what each running node of a stack is doing right now.
//
// GET /api/stacks/{id}/live answers, per node, with three things of different cost:
//
//   - its container's CPU, memory, network and block I/O — one Docker stats call per container of
//     this stack, in parallel (not the dashboard's sampleStats, which walks every container on the
//     host six at a time: ~1s each, so a stack's poll would pay for everybody else's nodes);
//   - its replication role and whether it takes writes, asked of the engine itself (not read from
//     the design — the design says who was made primary, which a failover makes untrue);
//   - how much disk its data directory holds, and how full the filesystem under it is.
//
// The last two cost an exec each, so they are cached per node (roles for liveRoleTTL, disk for
// liveDiskTTL) and probed in parallel. Only a canvas with Live switched on asks, so nothing runs
// when nobody is looking — the same bargain the dashboard makes.
//
// K3D members get container figures only: the databases are pods inside them, and asking those
// pods is a different conversation (kubectl, per operator) left for later.

const (
	liveRoleTTL   = 4 * time.Second
	liveDiskTTL   = 30 * time.Second
	liveProbeTime = 6 * time.Second
)

// liveRole is a node's place in its topology as the engine reports it.
type liveRole struct {
	// Role: primary | replica | secondary | member | arbiter | router | standalone.
	Role string `json:"role"`
	// Access: rw | ro — whether the node accepts writes right now.
	Access string `json:"access,omitempty"`
	// State is an engine's own word for it, where it has one (PXC "Synced", Valkey "up").
	State string `json:"state,omitempty"`
	// LagSec is how far a replica's applied data trails its source, when the engine says.
	LagSec *float64 `json:"lagSec,omitempty"`
	// Replicas is how many replicas a primary is feeding, when the engine says.
	Replicas *int   `json:"replicas,omitempty"`
	Err      string `json:"error,omitempty"`
}

type liveDisk struct {
	Path      string `json:"path"`
	DataBytes int64  `json:"dataBytes"` // what the data directory holds
	FSUsed    int64  `json:"fsUsed"`    // the filesystem under it
	FSTotal   int64  `json:"fsTotal"`
}

type liveNode struct {
	State string `json:"state"`
	ContainerStat
	*liveIO
	Role *liveRole `json:"role,omitempty"`
	Disk *liveDisk `json:"disk,omitempty"`
}

// liveIO is what Docker's stats leave out on cgroup v2 — operation counts, I/O pressure, swap —
// read from the container's cgroup files (liveIOScript). Operation counts are cumulative; the
// client turns pairs of samples into IOPS.
type liveIO struct {
	ReadOps  int64 `json:"readOps"`
	WriteOps int64 `json:"writeOps"`
	// IOPressure is the share of the last 10s some task in the container stalled on I/O (PSI,
	// "some avg10") — the per-container iowait. Absent on a kernel without PSI, and then there is
	// no per-container iowait to give: /proc/stat's iowait is the host's, which is not the node's.
	IOPressure    *float64 `json:"ioPressure,omitempty"`
	SwapUsed      *int64   `json:"swapUsed,omitempty"`
	SwapMax       *int64   `json:"swapMax,omitempty"` // nil when unlimited
	HostSwapTotal int64    `json:"hostSwapTotal"`
}

type liveCacheEntry struct {
	at   time.Time
	role *liveRole
	disk *liveDisk
}

var liveCache = struct {
	mu   sync.Mutex
	role map[string]liveCacheEntry
	disk map[string]liveCacheEntry
}{role: map[string]liveCacheEntry{}, disk: map[string]liveCacheEntry{}}

// handleStackLive is the Live view's poll.
func (a *App) handleStackLive(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read deployments")
		return
	}
	types := map[string]string{}
	for _, n := range buildDoc(st).Nodes {
		types[n.ID] = n.Type
	}
	var ids []string
	for _, dep := range deps {
		if dep.State == DeployRunning && dep.ContainerID != "" {
			ids = append(ids, dep.ContainerID)
		}
	}
	stats := a.liveContainerStats(r.Context(), st, deps, ids)

	out := map[string]*liveNode{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, dep := range deps {
		if dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		cs, found := stats[dep.ContainerID]
		if !found {
			continue
		}
		n := &liveNode{State: "running", ContainerStat: cs.st, liveIO: cs.io}
		out[dep.NodeID] = n
		typ := types[dep.NodeID]
		if !liveProbed(typ) {
			continue
		}
		wg.Add(1)
		go func(dep Deployment, typ, cid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(withEngine(context.Background(), a.depEngine(st, dep.NodeID)), liveProbeTime)
			defer cancel()
			key := fmt.Sprintf("%d:%s:%s", st.ID, dep.NodeID, cid)
			role := a.liveRoleCached(ctx, key, st, dep.NodeID, typ)
			disk := a.liveDiskCached(ctx, key, cid)
			mu.Lock()
			n.Role, n.Disk = role, disk
			mu.Unlock()
		}(dep, typ, dep.ContainerID)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"sampledAtSec": time.Now().Unix(), "nodes": out})
}

var liveStatCache = struct {
	mu sync.Mutex
	m  map[string]liveStatEntry
}{m: map[string]liveStatEntry{}}

type liveStatEntry struct {
	at time.Time
	st ContainerStat
	io *liveIO
}

type liveSample struct {
	st ContainerStat
	io *liveIO
}

// liveIOScript prints key=value lines from the container's cgroup and the host's /proc. A
// privileged node shares the host's cgroup namespace, so its own cgroup is a directory under
// /sys/fs/cgroup named for its id; any other container sees its own cgroup at the root. Busybox-safe
// (a k3s node has nothing else).
const liveIOScript = `CG=
for p in /sys/fs/cgroup/docker/$CID /sys/fs/cgroup/system.slice/docker-$CID.scope; do [ -d "$p" ] && CG=$p && break; done
[ -z "$CG" ] && grep -q '^0::/$' /proc/self/cgroup 2>/dev/null && CG=/sys/fs/cgroup
if [ -n "$CG" ]; then
  [ -r "$CG/io.stat" ] && awk '{for(i=2;i<=NF;i++){split($i,a,"=");if(a[1]=="rios")r+=a[2];if(a[1]=="wios")w+=a[2]}} END{print "rios="r+0; print "wios="w+0}' "$CG/io.stat"
  [ -r "$CG/io.pressure" ] && awk '/^some/{split($2,a,"=");print "iopsi="a[2]}' "$CG/io.pressure"
  [ -r "$CG/memory.swap.current" ] && echo "swap=$(cat "$CG/memory.swap.current")"
  [ -r "$CG/memory.swap.max" ] && echo "swapmax=$(cat "$CG/memory.swap.max")"
fi
awk '/^SwapTotal:/{print "hostswap="$2*1024}' /proc/meminfo`

func (a *App) probeLiveIO(ctx context.Context, cid string) *liveIO {
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"sh", "-c", liveIOScript}, []string{"CID=" + cid})
	if err != nil || res.Code != 0 {
		return nil
	}
	return parseLiveIO(res.Stdout)
}

func parseLiveIO(out string) *liveIO {
	io := &liveIO{}
	num := func(v string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64); return n }
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "rios":
			io.ReadOps = num(v)
		case "wios":
			io.WriteOps = num(v)
		case "iopsi":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				io.IOPressure = &f
			}
		case "swap":
			n := num(v)
			io.SwapUsed = &n
		case "swapmax":
			if v != "max" {
				n := num(v)
				io.SwapMax = &n
			}
		case "hostswap":
			io.HostSwapTotal = num(v)
		}
	}
	return io
}

// liveContainerStats samples the given containers at once. A sample younger than 1.5s is reused, so
// two viewers of one stack (or a fast interval) cost the daemon one call per container, not two.
// A container that does not answer — stopped, or gone since the deployment was written — is left
// out, and its node gets no popup.
func (a *App) liveContainerStats(ctx context.Context, st Stack, deps []Deployment, ids []string) map[string]liveSample {
	out := map[string]liveSample{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	nodeOf := map[string]string{}
	for _, dep := range deps {
		nodeOf[dep.ContainerID] = dep.NodeID
	}
	for _, id := range ids {
		liveStatCache.mu.Lock()
		e, ok := liveStatCache.m[id]
		liveStatCache.mu.Unlock()
		if ok && time.Since(e.at) < 1500*time.Millisecond {
			out[id] = liveSample{e.st, e.io}
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			c := withEngine(ctx, a.depEngine(st, nodeOf[id]))
			var io *liveIO
			done := make(chan struct{})
			go func() { // the stats call waits ~1s for its second CPU reading; read the cgroup meanwhile
				defer close(done)
				pc, cancel := context.WithTimeout(c, liveProbeTime)
				defer cancel()
				io = a.probeLiveIO(pc, id)
			}()
			cs, err := a.engCtx(c).ContainerStats(c, id)
			<-done
			if err != nil {
				return
			}
			mu.Lock()
			out[id] = liveSample{cs, io}
			mu.Unlock()
			liveStatCache.mu.Lock()
			liveStatCache.m[id] = liveStatEntry{at: time.Now(), st: cs, io: io}
			if len(liveStatCache.m) > 512 {
				for k, v := range liveStatCache.m {
					if time.Since(v.at) > time.Minute {
						delete(liveStatCache.m, k)
					}
				}
			}
			liveStatCache.mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

// liveProbed is whether a node type has an engine the Live view asks about its role and data.
func liveProbed(typ string) bool {
	return engineForType(typ) != "" || typ == "valkey" || typ == "valkeycluster"
}

func (a *App) liveRoleCached(ctx context.Context, key string, st Stack, nid, typ string) *liveRole {
	liveCache.mu.Lock()
	e, ok := liveCache.role[key]
	liveCache.mu.Unlock()
	if ok && time.Since(e.at) < liveRoleTTL {
		return e.role
	}
	role := a.probeLiveRole(ctx, st, nid, typ)
	liveCache.mu.Lock()
	liveCache.role[key] = liveCacheEntry{at: time.Now(), role: role}
	pruneLiveCache(liveCache.role)
	liveCache.mu.Unlock()
	return role
}

func (a *App) liveDiskCached(ctx context.Context, key, cid string) *liveDisk {
	liveCache.mu.Lock()
	e, ok := liveCache.disk[key]
	liveCache.mu.Unlock()
	if ok && time.Since(e.at) < liveDiskTTL {
		return e.disk
	}
	disk := a.probeLiveDisk(ctx, cid)
	liveCache.mu.Lock()
	liveCache.disk[key] = liveCacheEntry{at: time.Now(), disk: disk}
	pruneLiveCache(liveCache.disk)
	liveCache.mu.Unlock()
	return disk
}

// pruneLiveCache drops entries nobody has asked for in a while — a destroyed stack's nodes, a
// recreated container's old id. Called with liveCache.mu held.
func pruneLiveCache(m map[string]liveCacheEntry) {
	if len(m) < 256 {
		return
	}
	for k, e := range m {
		if time.Since(e.at) > 5*time.Minute {
			delete(m, k)
		}
	}
}

// probeLiveRole asks a node's engine what it is. A probe that fails says so in Err rather than
// guessing — an engine that is restarting is exactly when a stale role would mislead.
func (a *App) probeLiveRole(ctx context.Context, st Stack, nid, typ string) *liveRole {
	if typ == "valkey" || typ == "valkeycluster" {
		return a.probeValkeyRole(ctx, st, nid)
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		return &liveRole{Err: "not reachable"}
	}
	switch c.Engine {
	case "mysql":
		return a.probeMySQLRole(ctx, c)
	case "postgres":
		return a.probePGRole(ctx, c)
	case "mongodb":
		return a.probeMongoRole(ctx, c)
	}
	return nil
}

// liveMySQLProbe is one batch for every MySQL flavour — Percona Server, MySQL, MariaDB, PXC,
// Galera, Group Replication. Each statement ends in \G so the answers come back as uniform
// "name: value" lines, and the client runs with --force so a statement a flavour does not have
// (super_read_only on MariaDB, SHOW SLAVE on 8.4, performance_schema on old MariaDB) is skipped,
// not fatal.
const liveMySQLProbe = `SELECT @@global.read_only AS ro\G
SELECT @@global.super_read_only AS sro\G
SELECT VARIABLE_VALUE AS wsrep FROM performance_schema.global_status WHERE VARIABLE_NAME='wsrep_local_state_comment'\G
SELECT VARIABLE_VALUE AS wsrep FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='WSREP_LOCAL_STATE_COMMENT'\G
SELECT MEMBER_ROLE AS gr_role FROM performance_schema.replication_group_members WHERE MEMBER_ID=@@server_uuid\G
SELECT COUNT(*) AS dumps FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%'\G
SHOW REPLICA STATUS\G
SHOW SLAVE STATUS\G
`

func (a *App) probeMySQLRole(ctx context.Context, c dbConn) *liveRole {
	res, err := c.engine().ExecInput(ctx, c.ContainerID, "",
		append(c.client("mysql"), "-u", c.Super, "--force"),
		append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte(liveMySQLProbe))
	if err != nil {
		return &liveRole{Err: err.Error()}
	}
	f := verticalFields(res.Stdout)
	if _, ok := f["ro"]; !ok {
		return &liveRole{Err: lastLines(strings.TrimSpace(res.Stderr), 200)}
	}
	r := &liveRole{Access: "rw"}
	if f["ro"] == "1" || f["sro"] == "1" {
		r.Access = "ro"
	}
	source := firstNonEmpty(f["Source_Host"], f["Master_Host"])
	switch {
	case f["wsrep"] != "":
		r.Role, r.State = "member", f["wsrep"]
	case f["gr_role"] != "":
		r.Role = map[string]string{"PRIMARY": "primary", "SECONDARY": "secondary"}[f["gr_role"]]
		if r.Role == "" {
			r.Role = "member"
		}
	case source != "":
		r.Role = "replica"
		io := firstNonEmpty(f["Replica_IO_Running"], f["Slave_IO_Running"])
		sql := firstNonEmpty(f["Replica_SQL_Running"], f["Slave_SQL_Running"])
		r.State = "replicating"
		if io != "Yes" || sql != "Yes" {
			r.State = "stopped"
		}
		if lag, err := strconv.ParseFloat(firstNonEmpty(f["Seconds_Behind_Source"], f["Seconds_Behind_Master"]), 64); err == nil {
			r.LagSec = &lag
		}
	default:
		r.Role = "standalone"
		if n, _ := strconv.Atoi(f["dumps"]); n > 0 {
			r.Role, r.Replicas = "primary", &n
		}
	}
	return r
}

// verticalFields reads every "name: value" line of mysql \G output; the first answer for a name
// wins (the statements are ordered so the preferred source of a value comes first).
func verticalFields(out string) map[string]string {
	f := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok || strings.HasPrefix(k, "*") {
			continue
		}
		if _, seen := f[k]; !seen {
			f[k] = strings.TrimSpace(v)
		}
	}
	return f
}

// probePGRole: lag is the age of the last replayed transaction only while the replica has WAL it
// has not applied yet — on an idle primary that age just grows, and a caught-up replica showing
// "lag 55s" is the kind of number that sends somebody looking for a problem that is not there.
func (a *App) probePGRole(ctx context.Context, c dbConn) *liveRole {
	var out struct {
		Rec     bool     `json:"rec"`
		RO      string   `json:"ro"`
		Senders int      `json:"senders"`
		Lag     *float64 `json:"lag"`
		Datadir string   `json:"datadir"`
	}
	err := a.queryJSON(ctx, c, "postgres", `SELECT json_build_object(
  'rec', pg_is_in_recovery(),
  'ro', current_setting('default_transaction_read_only'),
  'senders', (SELECT count(*) FROM pg_stat_replication),
  'lag', CASE WHEN NOT pg_is_in_recovery() THEN NULL
              WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
              ELSE EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()) END)`, &out)
	if err != nil {
		return &liveRole{Err: lastLines(err.Error(), 200)}
	}
	r := &liveRole{Access: "rw"}
	if out.Rec || out.RO == "on" {
		r.Access = "ro"
	}
	switch {
	case out.Rec:
		r.Role, r.LagSec = "replica", out.Lag
	case out.Senders > 0:
		r.Role, r.Replicas = "primary", &out.Senders
	default:
		r.Role = "standalone"
	}
	return r
}

func (a *App) probeMongoRole(ctx context.Context, c dbConn) *liveRole {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return &liveRole{Err: lastLines(err.Error(), 200)}
	}
	defer closer()
	var h struct {
		Writable  bool   `bson:"isWritablePrimary"`
		Secondary bool   `bson:"secondary"`
		Arbiter   bool   `bson:"arbiterOnly"`
		SetName   string `bson:"setName"`
		Msg       string `bson:"msg"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&h); err != nil {
		return &liveRole{Err: lastLines(err.Error(), 200)}
	}
	switch {
	case h.Msg == "isdbgrid":
		return &liveRole{Role: "router", Access: "rw"}
	case h.Arbiter:
		return &liveRole{Role: "arbiter", Access: "ro", State: h.SetName}
	case h.Writable && h.SetName != "":
		return &liveRole{Role: "primary", Access: "rw", State: h.SetName}
	case h.Secondary:
		return &liveRole{Role: "secondary", Access: "ro", State: h.SetName}
	case h.SetName != "":
		return &liveRole{Role: "member", Access: "ro", State: h.SetName} // recovering, startup…
	}
	return &liveRole{Role: "standalone", Access: "rw"}
}

func (a *App) probeValkeyRole(ctx context.Context, st Stack, nid string) *liveRole {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil {
		return &liveRole{Err: "not deployed"}
	}
	var sec valkeySecrets
	json.Unmarshal(dep.Secrets, &sec)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "--no-auth-warning", "INFO", "replication"},
		[]string{"REDISCLI_AUTH=" + sec.Password})
	if err != nil || res.Code != 0 {
		return &liveRole{Err: "valkey-cli did not answer"}
	}
	f := map[string]string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			f[k] = v
		}
	}
	switch f["role"] {
	case "slave", "replica":
		r := &liveRole{Role: "replica", Access: "rw", State: f["master_link_status"]}
		if f["slave_read_only"] == "1" || f["replica_read_only"] == "1" {
			r.Access = "ro"
		}
		return r
	case "master":
		n, _ := strconv.Atoi(f["connected_slaves"])
		if n > 0 {
			return &liveRole{Role: "primary", Access: "rw", Replicas: &n}
		}
		return &liveRole{Role: "standalone", Access: "rw"}
	}
	return &liveRole{Err: "no role in INFO replication"}
}

// liveDiskScript prints "<path> <bytes in it> <fs used> <fs size>" for the first data directory
// that exists. The figures are the data itself (du) and the filesystem under it (df) — in a
// container that filesystem is usually the Docker host's, which is what fills up and stops the
// database, so it is the one worth a bar.
const liveDiskScript = `for p in /var/lib/mysql /var/lib/pgsql/*/data /var/lib/pgsql/data /var/lib/postgresql/*/main /var/lib/postgresql/data /data/db /var/lib/mongo /var/lib/mongodb /var/lib/valkey /data; do
  [ -d "$p" ] || continue
  d=$(timeout 4 du -sb "$p" 2>/dev/null | cut -f1)
  echo "$p ${d:-0} $(df -P -B1 "$p" | awk 'NR==2{print $3, $2}')"
  exit 0
done`

func (a *App) probeLiveDisk(ctx context.Context, cid string) *liveDisk {
	res, err := a.engCtx(ctx).Exec(ctx, cid, []string{"sh", "-c", liveDiskScript}, nil)
	if err != nil || res.Code != 0 {
		return nil
	}
	f := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(f) != 4 {
		return nil
	}
	d := &liveDisk{Path: f[0]}
	d.DataBytes, _ = strconv.ParseInt(f[1], 10, 64)
	d.FSUsed, _ = strconv.ParseInt(f[2], 10, 64)
	d.FSTotal, _ = strconv.ParseInt(f[3], 10, 64)
	return d
}
