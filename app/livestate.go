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
	"go.mongodb.org/mongo-driver/mongo"
)

// livestate.go — the canvas's Live view: what each running node of a stack is doing right now.
//
// GET /api/stacks/{id}/live answers, per node, with three things of different cost — and, for a
// node that is deployed but not running, just that (its state), so a stopped node is a red popup
// rather than a missing one:
//
//   - its container's CPU, memory, network and block I/O — one Docker stats call per container of
//     this stack, in parallel (not the dashboard's sampleStats, which walks every container on the
//     host six at a time: ~1s each, so a stack's poll would pay for everybody else's nodes);
//   - its replication role and whether it takes writes, asked of the engine itself (not read from
//     the design — the design says who was made primary, which a failover makes untrue), with
//     whatever is wrong with its replication as the engine sees it (livehealth.go), and whether
//     the database answers at all;
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
	Replicas *int `json:"replicas,omitempty"`
	// Problems are what is wrong with this node's replication, one line each, as the engine
	// reports it: a stopped channel, a peer it cannot reach, a slot nobody reads (livehealth.go).
	Problems []string `json:"problems,omitempty"`
	// Down is a database that did not answer while its container is running; Err says how.
	Down bool   `json:"down,omitempty"`
	Err  string `json:"error,omitempty"`
	// Load is the database's own work and connections (livehealth.go), when the engine says.
	Load *liveLoad `json:"load,omitempty"`
	// Source is the host this node replicates from, as the engine reports it (a MySQL channel's
	// source, PostgreSQL's WAL sender, MongoDB's sync source, Valkey's primary); SourceNode is the
	// node of this stack that host is, when it is one — what the canvas draws the actual
	// replication arrows from.
	Source     string `json:"source,omitempty"`
	SourceNode string `json:"sourceNode,omitempty"`
	// Channels is every replication channel of a MySQL-family node — a cluster primary can also
	// be the replica end of a link from another cluster, and a bidirectional link is a channel
	// at both ends — so the canvas can colour each designed replication line by its own channel.
	Channels []liveChannel `json:"channels,omitempty"`
	// CrossCluster: this node's sources are all outside its own cluster — the primary of a
	// cluster fed by a replication link from another one.
	CrossCluster bool `json:"crossCluster,omitempty"`
}

// liveChannel is one replication channel as its replica reports it.
type liveChannel struct {
	Source     string   `json:"source"`
	SourceNode string   `json:"sourceNode,omitempty"`
	Name       string   `json:"name,omitempty"`
	Running    bool     `json:"running"`
	LagSec     *float64 `json:"lagSec,omitempty"`
	Problem    string   `json:"problem,omitempty"`
}

// liveLoad is what the database itself is doing. Queries and Commits are counters since the server
// started, which the client (and the history sampler) turn into QPS and TPS from two samples; an
// engine that does not count one leaves it out, rather than reporting zero.
type liveLoad struct {
	Queries  *int64 `json:"queries,omitempty"`  // statements (MySQL Questions, MongoDB opcounters, Valkey commands)
	Commits  *int64 `json:"commits,omitempty"`  // transactions committed or rolled back
	Conns    *int   `json:"conns,omitempty"`    // client connections open now
	MaxConns *int   `json:"maxConns,omitempty"` // the most the server accepts
	Active   *int   `json:"active,omitempty"`   // connections running something right now
	// AtMs is when the counters were read. A role is cached for liveRoleTTL, so two polls can see
	// one sample; rates are taken between samples' own times, not the polls'.
	AtMs int64 `json:"atMs"`
}

type liveDisk struct {
	Path      string `json:"path"`
	DataBytes int64  `json:"dataBytes"` // what the data directory holds
	FSUsed    int64  `json:"fsUsed"`    // the filesystem under it
	FSTotal   int64  `json:"fsTotal"`
}

type liveNode struct {
	// State is "running", the deployment's state for a node that is not ("stopped", "error"), or
	// "unreachable" for a running deployment whose container does not answer.
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
	out, err := a.liveSnapshot(r.Context(), st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read deployments")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sampledAtSec": time.Now().Unix(), "nodes": out})
}

// liveSnapshot is every deployed node of a stack as Live sees it now — what the canvas polls, and
// what the history sampler (livewatch.go) records.
func (a *App) liveSnapshot(ctx context.Context, st Stack) (map[string]*liveNode, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	doc := buildDoc(st)
	types := map[string]string{}
	frameOf := map[string]string{}
	members := map[string]int{}
	for _, n := range doc.Nodes {
		types[n.ID] = n.Type
		if n.FrameID != "" {
			frameOf[n.ID] = n.FrameID
			members[n.FrameID]++
		}
	}
	var ids []string
	for _, dep := range deps {
		if dep.State == DeployRunning && dep.ContainerID != "" {
			ids = append(ids, dep.ContainerID)
		}
	}
	stats := a.liveContainerStats(ctx, st, deps, ids)

	out := map[string]*liveNode{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for _, dep := range deps {
		if _, inDesign := types[dep.NodeID]; !inDesign {
			continue
		}
		if dep.State == DeployStopped || dep.State == DeployError {
			out[dep.NodeID] = &liveNode{State: dep.State}
			continue
		}
		if dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		cs, found := stats[dep.ContainerID]
		// Docker answers a stats call for a stopped container with zeros rather than an error, and
		// a running container always holds some memory: a container killed or stopped outside
		// DBCanvas, whose deployment still says running.
		_, isVM := a.depEngine(st, dep.NodeID).(*Vagrant) // a VM reports no stats at all
		if !found || (!isVM && cs.st.MemUsed == 0 && cs.st.MemLimit == 0) {
			out[dep.NodeID] = &liveNode{State: "unreachable"}
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
			role := a.liveRoleCached(ctx, key, st, dep.NodeID, typ, members[frameOf[dep.NodeID]])
			disk := a.liveDiskCached(ctx, key, cid)
			mu.Lock()
			n.Role, n.Disk = role, disk
			if role != nil && strings.Contains(role.Err, "is not running") {
				n.State, n.Role, n.Disk = "unreachable", nil, nil
			}
			mu.Unlock()
		}(dep, typ, dep.ContainerID)
	}
	wg.Wait()
	detachedReplicas(doc, out)
	resolveSources(doc, out)
	return out, nil
}

// resolveSources names the node each replica's source host is — for the node's first source and
// for each of its channels. A host can be given as the short name, the FQDN, or (MongoDB,
// PostgreSQL) an address; the first two are matched.
//
// A member whose every channel comes from outside its own cluster, and which feeds replicas or
// is the cluster's designed primary, is that cluster's primary — fed by a replication link from
// another cluster — and is reported as such rather than as a replica.
func resolveSources(doc designDoc, nodes map[string]*liveNode) {
	hosts := stackHostnames(doc)
	byHost := map[string]string{}
	for id, h := range hosts {
		if h == "" {
			continue
		}
		byHost[strings.ToLower(h)] = id
	}
	frameOf := map[string]string{}
	design := map[string]designNode{}
	for _, n := range doc.Nodes {
		frameOf[n.ID] = n.FrameID
		design[n.ID] = n
	}
	resolve := func(host, self string) string {
		h := strings.ToLower(strings.TrimSuffix(host, "."))
		short, _, _ := strings.Cut(h, ".")
		src := firstNonEmpty(byHost[h], byHost[short])
		if src == self {
			return ""
		}
		return src
	}
	for id, ln := range nodes {
		if ln == nil || ln.Role == nil || (ln.Role.Source == "" && len(ln.Role.Channels) == 0) {
			continue
		}
		r := *ln.Role
		if r.Source != "" {
			r.SourceNode = resolve(r.Source, id)
		}
		r.Channels = append([]liveChannel(nil), r.Channels...)
		inFrame := false
		for i := range r.Channels {
			r.Channels[i].SourceNode = resolve(r.Channels[i].Source, id)
			if f := frameOf[id]; f != "" && frameOf[r.Channels[i].SourceNode] == f {
				inFrame = true
			}
		}
		if r.Role == "replica" && frameOf[id] != "" && len(r.Channels) > 0 && !inFrame &&
			((r.Replicas != nil && *r.Replicas > 0) || design[id].Role == "primary") {
			r.Role, r.CrossCluster = "primary", true
			if r.State == "replicating" {
				r.State = ""
			}
		}
		ln.Role = &r
	}
}
// asyncReplFrames are the clusters held together by plain source → replica replication, where a
// member with no source at all is a member that has silently left.
var asyncReplFrames = map[string]bool{"mysql": true, "mysqlcerepl": true, "mariadbrepl": true}

// detachedReplicas flags the member of a replication cluster that has no replication configured
// while another member is the primary: nothing is broken on it — there is simply nothing to break
// — so the engine reports no problem, and the member drifts unnoticed. (A replica whose
// replication was reset, a rebuild that stopped halfway.)
func detachedReplicas(doc designDoc, nodes map[string]*liveNode) {
	frameType := map[string]string{}
	for _, f := range doc.Frames {
		frameType[f.ID] = f.Type
	}
	members := map[string][]string{}
	for _, n := range doc.Nodes {
		if n.FrameID != "" && asyncReplFrames[frameType[n.FrameID]] {
			members[n.FrameID] = append(members[n.FrameID], n.ID)
		}
	}
	for _, ids := range members {
		primary := ""
		for _, id := range ids {
			if ln := nodes[id]; ln != nil && ln.Role != nil && ln.Role.Role == "primary" {
				primary = id
			}
		}
		if primary == "" {
			continue
		}
		for _, id := range ids {
			ln := nodes[id]
			if ln == nil || ln.Role == nil || ln.Role.Role != "standalone" || ln.Role.Err != "" {
				continue
			}
			// The role cache is shared with other callers: annotate a copy.
			r := *ln.Role
			r.Problems = append(append([]string(nil), r.Problems...), "not replicating: no replication source is configured on this member")
			ln.Role = &r
		}
	}
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
// out, and handleStackLive reports its node as unreachable.
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

func (a *App) liveRoleCached(ctx context.Context, key string, st Stack, nid, typ string, members int) *liveRole {
	liveCache.mu.Lock()
	e, ok := liveCache.role[key]
	liveCache.mu.Unlock()
	if ok && time.Since(e.at) < liveRoleTTL {
		return e.role
	}
	role := a.probeLiveRole(ctx, st, nid, typ, members)
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
// guessing — an engine that is restarting is exactly when a stale role would mislead. members is
// how many members the design gives the node's cluster, 0 outside one.
func (a *App) probeLiveRole(ctx context.Context, st Stack, nid, typ string, members int) *liveRole {
	r := a.probeLiveRoleNow(ctx, st, nid, typ, members)
	if r != nil && r.Load != nil {
		r.Load.AtMs = time.Now().UnixMilli()
	}
	return r
}

func (a *App) probeLiveRoleNow(ctx context.Context, st Stack, nid, typ string, members int) *liveRole {
	if typ == "valkey" || typ == "valkeycluster" {
		return a.probeValkeyRole(ctx, st, nid, typ == "valkeycluster")
	}
	c, ok := a.dbConnFor(st, nid)
	if !ok {
		return &liveRole{Err: "not reachable"}
	}
	switch c.Engine {
	case "mysql":
		return a.probeMySQLRole(ctx, c, members)
	case "postgres":
		return a.probePGRole(ctx, c)
	case "mongodb":
		return a.probeMongoRole(ctx, c)
	}
	return nil
}

func (a *App) probeMySQLRole(ctx context.Context, c dbConn, members int) *liveRole {
	res, err := c.engine().ExecInput(ctx, c.ContainerID, "",
		append(c.client("mysql"), "-u", c.Super, "--force", "--vertical"),
		append([]string{"MYSQL_PWD=" + c.Password}, c.Env...), []byte(liveMySQLProbe))
	if err != nil {
		return &liveRole{Err: err.Error()}
	}
	if _, ok := verticalFields(res.Stdout)["ro"]; !ok {
		msg := strings.TrimSpace(res.Stderr)
		return &liveRole{Down: mysqlUnreachable(msg), Err: lastLines(msg, 200)}
	}
	return parseMySQLLive(res.Stdout, members)
}

// probePGRole: lag is the age of the last replayed transaction only while the replica has WAL it
// has not applied yet — on an idle primary that age just grows, and a caught-up replica showing
// "lag 55s" is the kind of number that sends somebody looking for a problem that is not there.
func (a *App) probePGRole(ctx context.Context, c dbConn) *liveRole {
	var out pgLive
	if err := a.queryJSON(ctx, c, "postgres", liveProbePG, &out); err != nil {
		return &liveRole{Down: pgUnreachable(err.Error()), Err: lastLines(err.Error(), 200)}
	}
	return pgLiveRole(out)
}

func (a *App) probeMongoRole(ctx context.Context, c dbConn) *liveRole {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return &liveRole{Down: true, Err: lastLines(err.Error(), 200)}
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
		return &liveRole{Down: true, Err: lastLines(err.Error(), 200)}
	}
	var r *liveRole
	switch {
	case h.Msg == "isdbgrid":
		return &liveRole{Role: "router", Access: "rw", Load: a.mongoLoad(ctx, client)}
	case h.Arbiter:
		r = &liveRole{Role: "arbiter", Access: "ro", State: h.SetName}
	case h.Writable && h.SetName != "":
		r = &liveRole{Role: "primary", Access: "rw", State: h.SetName}
	case h.Secondary:
		r = &liveRole{Role: "secondary", Access: "ro", State: h.SetName}
	case h.SetName != "":
		r = &liveRole{Role: "member", Access: "ro", State: h.SetName} // recovering, startup…
	default:
		return &liveRole{Role: "standalone", Access: "rw", Load: a.mongoLoad(ctx, client)}
	}
	// The set's members as this one sees them (livehealth.go). A status that will not come back
	// costs the popup its problem lines, not its role.
	var rs struct {
		Members []mongoMember `bson:"members"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&rs); err == nil {
		r.Problems, r.LagSec = mongoReplHealth(rs.Members)
		for _, m := range rs.Members {
			if m.Self && m.SyncSource != "" {
				r.Source, _, _ = strings.Cut(m.SyncSource, ":")
			}
		}
	}
	r.Load = a.mongoLoad(ctx, client)
	return r
}

// mongoLoad reads serverStatus: every operation counter as the queries (commands included — the
// shell's own hello is one), connections current against current + available, and the clients
// running something now. MongoDB has no commit counter worth the name outside a transaction.
func (a *App) mongoLoad(ctx context.Context, client *mongo.Client) *liveLoad {
	var ss struct {
		Op struct {
			Insert, Query, Update, Delete, Getmore, Command int64
		} `bson:"opcounters"`
		Conn struct {
			Current   int `bson:"current"`
			Available int `bson:"available"`
		} `bson:"connections"`
		Lock struct {
			Active struct {
				Total int `bson:"total"`
			} `bson:"activeClients"`
		} `bson:"globalLock"`
	}
	cmd := bson.D{{Key: "serverStatus", Value: 1}, {Key: "repl", Value: 0}, {Key: "metrics", Value: 0}, {Key: "locks", Value: 0}, {Key: "wiredTiger", Value: 0}}
	if err := client.Database("admin").RunCommand(ctx, cmd).Decode(&ss); err != nil {
		return nil
	}
	q := ss.Op.Insert + ss.Op.Query + ss.Op.Update + ss.Op.Delete + ss.Op.Getmore + ss.Op.Command
	// The driver holds a few connections of its own (a monitor beside the one asking), so the
	// count includes the probe's; how many depends on the driver, so none is taken off.
	limit := ss.Conn.Current + ss.Conn.Available
	conns, act := ss.Conn.Current, ss.Lock.Active.Total
	return &liveLoad{Queries: &q, Conns: &conns, MaxConns: &limit, Active: &act}
}

func (a *App) probeValkeyRole(ctx context.Context, st Stack, nid string, cluster bool) *liveRole {
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil {
		return &liveRole{Err: "not deployed"}
	}
	var sec valkeySecrets
	json.Unmarshal(dep.Secrets, &sec)
	flag := "0"
	if cluster {
		flag = "1"
	}
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"sh", "-c", liveValkeyScript},
		[]string{"REDISCLI_AUTH=" + sec.Password, "CLUSTER=" + flag})
	if err != nil {
		return &liveRole{Down: true, Err: err.Error()}
	}
	return parseValkeyLive(res.Stdout)
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
