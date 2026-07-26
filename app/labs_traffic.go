package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Lab CRUD traffic — while a Patroni lab is active, a slow steady trickle of
// real INSERT/UPDATE/DELETE/SELECT statements flows through the lab's HAProxy
// node (never straight to a database node), exactly like an application would.
// The point isn't load — it's motion: replication keeps streaming, HAProxy's
// write/read split keeps doing real routing work, and a switchover/failover/
// sync-replication lab has something to actually observe rather than a static,
// idle cluster. Learners can pause/resume it independently of the lab steps.

// labTrafficDispatchInterval is how often the dispatcher wakes up to release
// this tick's share of operations — fine-grained enough that even the top of
// the rate slider (1000/s) is paced smoothly rather than firing in bursts.
const labTrafficDispatchInterval = 100 * time.Millisecond

// labTrafficMinRate / labTrafficMaxRate bound the learner-adjustable rate
// slider — "1 query per second" as a floor (enough to keep replication moving
// without competing with anything the learner is doing by hand) up to 1000/s.
const (
	labTrafficMinRate     = 1
	labTrafficMaxRate     = 1000
	labTrafficDefaultRate = 1
)

// labTrafficMinThreads / labTrafficMaxThreads bound the learner-adjustable
// threads slider — the number of persistent workers concurrently issuing
// cycles. The total throughput stays capped at the Rate slider regardless of
// thread count (each active worker gets rate/threads of the total); more
// threads means that same total is spread across more concurrent
// connections, not more load.
const (
	labTrafficMinThreads     = 1
	labTrafficMaxThreads     = 8
	labTrafficDefaultThreads = 1
)

// labTrafficConnPoolSize is the write/read connection pool size — a couple
// more than labTrafficMaxThreads so a worker never blocks waiting on a
// connection another worker is about to release.
const labTrafficConnPoolSize = labTrafficMaxThreads + 2

// labTrafficMinTables / labTrafficMaxTables bound the learner-adjustable
// tables slider. All labTrafficMaxTables tables are precreated at startup
// regardless of the slider's current value — the slider only controls how
// many of them traffic is actually spread across, so raising it never needs
// a schema change mid-run.
const (
	labTrafficMinTables     = 1
	labTrafficMaxTables     = 30
	labTrafficDefaultTables = 1
)

// labTrafficMaxRows caps each table's growth (delete the oldest row once
// past this) so a multi-hour lab session doesn't grow it unbounded.
const labTrafficMaxRows = 500

// labNodeIPRefreshInterval is how often the container-IP -> nodeID map used
// for per-node attribution is rebuilt, so nodes that finish deploying after
// traffic starts (or reappear after a rebuild) get picked up.
const labNodeIPRefreshInterval = 5 * time.Second

// labTrafficRun is one lab's background CRUD generator. One per active lab
// run's disposable stack, keyed by stack ID (a learner has at most one active
// attempt per lab — see handleStartLab).
type labTrafficRun struct {
	stackID   int64
	labRunID  int64
	cancel    context.CancelFunc
	startedAt time.Time

	paused  atomic.Bool
	running atomic.Bool  // false once the generator loop has exited
	rate    atomic.Int64 // target operation cycles/sec (total, shared across threads), learner-adjustable 1-1000
	threads atomic.Int64 // active concurrent workers, learner-adjustable 1-8
	tables  atomic.Int64 // active table count (lab_traffic_1..N), learner-adjustable 1-30

	inserts atomic.Int64
	updates atomic.Int64
	deletes atomic.Int64
	selects atomic.Int64
	errors  atomic.Int64

	tickSeq atomic.Int64 // shared monotonic cycle counter across every worker

	lastError atomic.Value // string

	// nodeMu guards nodeIPs (container IP -> nodeID, refreshed periodically so
	// a switchover/failover moving roles around doesn't stale the mapping) and
	// nodes (nodeID -> that node's own attributed counters). Per-node
	// attribution lets the CRUD Traffic panel show which physical node is
	// actually answering right now, not just a cluster-wide total.
	nodeMu  sync.Mutex
	nodeIPs map[string]string
	nodes   map[string]*labNodeCounters
}

// labNodeCounters is one Patroni node's own slice of the cluster's CRUD
// traffic, attributed via inet_server_addr() (see runCycle). lastSeen (unix
// millis) is how the frontend tells "currently receiving traffic" apart from
// "hasn't answered anything in a while" (demoted, paused out of the read
// pool, or actually down).
type labNodeCounters struct {
	inserts  atomic.Int64
	updates  atomic.Int64
	deletes  atomic.Int64
	selects  atomic.Int64
	lastSeen atomic.Int64
}

const (
	labOpInsert = 'i'
	labOpUpdate = 'u'
	labOpDelete = 'd'
	labOpSelect = 's'
)

func (n *labNodeCounters) touch(op byte) {
	switch op {
	case labOpInsert:
		n.inserts.Add(1)
	case labOpUpdate:
		n.updates.Add(1)
	case labOpDelete:
		n.deletes.Add(1)
	case labOpSelect:
		n.selects.Add(1)
	}
	n.lastSeen.Store(time.Now().UnixMilli())
}

// recordByAddr attributes one operation to whichever node currently owns
// addr (a Postgres backend's own inet_server_addr()) — a no-op if addr is
// empty or isn't (yet) a known node IP.
func (t *labTrafficRun) recordByAddr(addr string, op byte) {
	if addr == "" {
		return
	}
	t.nodeMu.Lock()
	nodeID, ok := t.nodeIPs[addr]
	var c *labNodeCounters
	if ok {
		if t.nodes == nil {
			t.nodes = map[string]*labNodeCounters{}
		}
		c = t.nodes[nodeID]
		if c == nil {
			c = &labNodeCounters{}
			t.nodes[nodeID] = c
		}
	}
	t.nodeMu.Unlock()
	if c != nil {
		c.touch(op)
	}
}

// setNodeIPs replaces the container-IP -> nodeID map used to attribute
// traffic to physical nodes.
func (t *labTrafficRun) setNodeIPs(ips map[string]string) {
	if len(ips) == 0 {
		return
	}
	t.nodeMu.Lock()
	t.nodeIPs = ips
	t.nodeMu.Unlock()
}

// labNodeTrafficSnapshot is one Patroni node's CRUD breakdown, as reported to
// the frontend — present for every Patroni node in the design even if it has
// never answered anything (LastSeenMs stays 0), so a genuinely down node
// still shows up rather than silently vanishing from the panel.
type labNodeTrafficSnapshot struct {
	NodeID     string `json:"nodeId"`
	Label      string `json:"label"`
	Inserts    int64  `json:"inserts"`
	Updates    int64  `json:"updates"`
	Deletes    int64  `json:"deletes"`
	Selects    int64  `json:"selects"`
	LastSeenMs int64  `json:"lastSeenMs"`
}

// nodeSnapshots reports every Patroni node in doc, filled in with whatever
// this run has attributed to it so far (zeros for a node that's never
// answered a query).
func (t *labTrafficRun) nodeSnapshots(doc designDoc) []labNodeTrafficSnapshot {
	t.nodeMu.Lock()
	defer t.nodeMu.Unlock()
	out := make([]labNodeTrafficSnapshot, 0, len(doc.Nodes))
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		s := labNodeTrafficSnapshot{NodeID: n.ID, Label: n.Label}
		if c, ok := t.nodes[n.ID]; ok {
			s.Inserts = c.inserts.Load()
			s.Updates = c.updates.Load()
			s.Deletes = c.deletes.Load()
			s.Selects = c.selects.Load()
			s.LastSeenMs = c.lastSeen.Load()
		}
		out = append(out, s)
	}
	return out
}

// clampLabTrafficRate keeps a requested rate within the slider's bounds.
func clampLabTrafficRate(v int) int64 {
	if v < labTrafficMinRate {
		return labTrafficMinRate
	}
	if v > labTrafficMaxRate {
		return labTrafficMaxRate
	}
	return int64(v)
}

// clampLabTrafficThreads keeps a requested thread count within the slider's bounds.
func clampLabTrafficThreads(v int) int64 {
	if v < labTrafficMinThreads {
		return labTrafficMinThreads
	}
	if v > labTrafficMaxThreads {
		return labTrafficMaxThreads
	}
	return int64(v)
}

// clampLabTrafficTables keeps a requested table count within the slider's bounds.
func clampLabTrafficTables(v int) int64 {
	if v < labTrafficMinTables {
		return labTrafficMinTables
	}
	if v > labTrafficMaxTables {
		return labTrafficMaxTables
	}
	return int64(v)
}

var (
	labTrafficMu sync.Mutex
	labTraffics  = map[int64]*labTrafficRun{} // stackID -> run
)

// labTrafficSnapshot is what the frontend polls for the CRUD Traffic panel.
type labTrafficSnapshot struct {
	Running   bool    `json:"running"`
	Paused    bool    `json:"paused"`
	Rate      int64   `json:"rate"`
	Threads   int64   `json:"threads"`
	Tables    int64   `json:"tables"`
	Inserts   int64   `json:"inserts"`
	Updates   int64   `json:"updates"`
	Deletes   int64   `json:"deletes"`
	Selects   int64   `json:"selects"`
	Errors    int64   `json:"errors"`
	OpsPerSec float64 `json:"opsPerSec"`
	LastError string  `json:"lastError,omitempty"`
	StartedAt string  `json:"startedAt,omitempty"`

	// Nodes is filled in by the handler (it needs the stack's design to know
	// which Patroni nodes exist), not by snapshot() itself — see
	// handleLabTrafficStatus.
	Nodes []labNodeTrafficSnapshot `json:"nodes,omitempty"`
}

func (t *labTrafficRun) snapshot() labTrafficSnapshot {
	s := labTrafficSnapshot{
		Running: t.running.Load(),
		Paused:  t.paused.Load(),
		Rate:    t.rate.Load(),
		Threads: t.threads.Load(),
		Tables:  t.tables.Load(),
		Inserts: t.inserts.Load(),
		Updates: t.updates.Load(),
		Deletes: t.deletes.Load(),
		Selects: t.selects.Load(),
		Errors:  t.errors.Load(),
	}
	if v, ok := t.lastError.Load().(string); ok {
		s.LastError = v
	}
	if !t.startedAt.IsZero() {
		s.StartedAt = t.startedAt.Format(time.RFC3339)
		if elapsed := time.Since(t.startedAt).Seconds(); elapsed > 0 {
			s.OpsPerSec = float64(s.Inserts+s.Updates+s.Deletes+s.Selects) / elapsed
		}
	}
	return s
}

// startLabTraffic launches (once) the CRUD generator for a freshly-created lab
// run's stack. Runs detached from the request, like captureLabInitialLeader —
// the cluster + HAProxy are usually still deploying when Start returns.
func (a *App) startLabTraffic(labRunID, stackID int64) {
	labTrafficMu.Lock()
	if _, exists := labTraffics[stackID]; exists {
		labTrafficMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &labTrafficRun{stackID: stackID, labRunID: labRunID, cancel: cancel}
	t.rate.Store(labTrafficDefaultRate)
	t.threads.Store(labTrafficDefaultThreads)
	t.tables.Store(labTrafficDefaultTables)
	labTraffics[stackID] = t
	labTrafficMu.Unlock()

	writeDB, readDB, ok := a.waitLabHAProxyDial(ctx, stackID)
	if !ok {
		labTrafficMu.Lock()
		delete(labTraffics, stackID)
		labTrafficMu.Unlock()
		return
	}
	defer writeDB.Close()
	defer readDB.Close()

	// Precreate every table the slider can ever select up front — raising it
	// mid-run just means more of these get used, never a schema change.
	for i := 1; i <= labTrafficMaxTables; i++ {
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS lab_traffic_%d (
			id BIGSERIAL PRIMARY KEY, val TEXT, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`, i)
		if _, err := writeDB.ExecContext(ctx, ddl); err != nil {
			log.Printf("stack %d lab traffic: create table %d: %v", stackID, i, err)
			labTrafficMu.Lock()
			delete(labTraffics, stackID)
			labTrafficMu.Unlock()
			return
		}
	}

	t.startedAt = time.Now()
	t.running.Store(true)
	defer t.running.Store(false)

	// Per-node attribution needs a container-IP -> nodeID map (built from the
	// stack's own deployments) — refreshed every labNodeIPRefreshTicks ticks so
	// a switchover/failover moving roles around, or a node that only just
	// finished deploying, doesn't leave the mapping stale. Runs detached from
	// the workers since it makes engine calls (container inspects) that could
	// otherwise stall a worker's own pacing.
	t.setNodeIPs(a.buildLabNodeIPMap(ctx, stackID))
	go func() {
		ticker := time.NewTicker(labNodeIPRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			t.setNodeIPs(a.buildLabNodeIPMap(ctx, stackID))
		}
	}()

	// Exactly labTrafficMaxThreads persistent workers are always running;
	// each one only actually issues cycles while its own index is below the
	// current thread count, so raising/lowering the threads slider takes
	// effect immediately on the next loop iteration of every worker, with no
	// goroutines to start or stop. Each active worker gets its own share
	// (rate/threads) of the total target rate, so total throughput stays
	// capped at the Rate slider regardless of how many threads are active.
	var wg sync.WaitGroup
	for i := 0; i < labTrafficMaxThreads; i++ {
		wg.Add(1)
		go func(workerIdx int64) {
			defer wg.Done()
			t.runWorker(ctx, writeDB, readDB, workerIdx)
		}(int64(i))
	}
	wg.Wait()
}

// runWorker is one of up to labTrafficMaxThreads persistent workers. It only
// issues cycles while workerIdx is below the current thread count — idling
// (checked every labTrafficDispatchInterval) otherwise — and paces itself to
// its own share (rate/threads) of the total target rate, sleeping between
// cycles rather than firing them concurrently, so at most one cycle per
// active worker is ever in flight at once. A slow or unreachable cluster
// naturally throttles each worker's achieved rate (the next cycle simply
// can't start until the last one returns) instead of piling up goroutines.
func (t *labTrafficRun) runWorker(ctx context.Context, writeDB, readDB *sql.DB, workerIdx int64) {
	for {
		if ctx.Err() != nil {
			return
		}
		threads := t.threads.Load()
		if threads < labTrafficMinThreads {
			threads = labTrafficMinThreads
		}
		if workerIdx >= threads || t.paused.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(labTrafficDispatchInterval):
			}
			continue
		}
		rate := t.rate.Load()
		if rate < labTrafficMinRate {
			rate = labTrafficMinRate
		}
		perWorkerRate := float64(rate) / float64(threads)
		interval := time.Duration(float64(time.Second) / perWorkerRate)
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		t.runCycle(ctx, writeDB, readDB, t.tickSeq.Add(1))
	}
}

// runCycle performs one lab-traffic operation cycle: always an INSERT (the
// signal that must keep flowing so replication has something to replicate),
// occasionally an UPDATE and a bounding DELETE via the write port, and always
// a SELECT via the read port (HAProxy's replica round-robin) so reads keep
// exercising the cluster too. Every statement also asks its own
// inet_server_addr() in the same round trip (via a CTE, so it's always the
// backend that actually ran the write/read, not just whichever connection
// happened to be idle in the pool) so the operation can be attributed to the
// physical node that answered it — that's what lets the CRUD Traffic panel
// show per-node activity instead of only a cluster-wide total.
//
// All four statements operate on the same one of lab_traffic_1..N chosen at
// random for this cycle (N = the current tables setting) — every table this
// run could ever use was already precreated at startup, so raising the
// slider only changes which of them get picked, never triggers DDL. Table
// names can't be bind parameters, but tbl is always our own "lab_traffic_"
// + an int in [1, labTrafficMaxTables], never user input, so building the
// query string with it is safe.
func (t *labTrafficRun) runCycle(ctx context.Context, writeDB, readDB *sql.DB, tick int64) {
	note := func(err error) {
		if err != nil {
			t.errors.Add(1)
			t.lastError.Store(err.Error())
		}
	}
	tables := t.tables.Load()
	if tables < labTrafficMinTables {
		tables = labTrafficMinTables
	}
	tbl := fmt.Sprintf("lab_traffic_%d", 1+rand.Intn(int(tables)))
	var addr string

	err := writeDB.QueryRowContext(ctx, fmt.Sprintf(
		`WITH ins AS (INSERT INTO %s (val, updated_at) VALUES ($1, now()) RETURNING 1)
		 SELECT host(inet_server_addr()) FROM ins`, tbl), fmt.Sprintf("lab-%d", tick)).Scan(&addr)
	note(err)
	if err == nil {
		t.inserts.Add(1)
		t.recordByAddr(addr, labOpInsert)
	}

	if tick%3 == 0 {
		err := writeDB.QueryRowContext(ctx, fmt.Sprintf(
			`WITH upd AS (
			   UPDATE %[1]s SET val = $1, updated_at = now()
			   WHERE id = (SELECT id FROM %[1]s ORDER BY random() LIMIT 1)
			   RETURNING 1
			 )
			 SELECT host(inet_server_addr()), (SELECT count(*) FROM upd)`, tbl), fmt.Sprintf("touched-%d", tick)).Scan(&addr, new(int64))
		note(err)
		if err == nil {
			t.updates.Add(1)
			t.recordByAddr(addr, labOpUpdate)
		}
	}

	if tick%5 == 0 {
		err := writeDB.QueryRowContext(ctx, fmt.Sprintf(
			`WITH del AS (
			   DELETE FROM %[1]s WHERE id IN (
			     SELECT id FROM %[1]s ORDER BY id ASC LIMIT GREATEST((SELECT count(*) FROM %[1]s) - $1, 0))
			   RETURNING 1
			 )
			 SELECT host(inet_server_addr()), (SELECT count(*) FROM del)`, tbl), labTrafficMaxRows).Scan(&addr, new(int64))
		note(err)
		if err == nil {
			t.deletes.Add(1)
			t.recordByAddr(addr, labOpDelete)
		}
	}

	var n int64
	err = readDB.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*), host(inet_server_addr()) FROM %s", tbl)).Scan(&n, &addr)
	note(err)
	if err == nil {
		t.selects.Add(1)
		t.recordByAddr(addr, labOpSelect)
	}
}

// buildLabNodeIPMap resolves every currently-running Patroni node's
// container IP within the stack's own network, keyed for recordByAddr to
// match against inet_server_addr(). Best-effort: a node that isn't deployed
// yet, or whose engine call fails, is simply left out of the map (and
// therefore reported as never-seen, i.e. down) until the next refresh.
func (a *App) buildLabNodeIPMap(ctx context.Context, stackID int64) map[string]string {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return nil
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return nil
	}
	deps, err := a.store.ListDeployments(stackID)
	if err != nil {
		return nil
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	netName := networkName(stackID)
	out := map[string]string{}
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.State != DeployRunning || d.ContainerID == "" {
			continue
		}
		eng := a.dialEngine(stackID, d.ContainerID)
		ip, err := eng.ContainerIP(ctx, d.ContainerID, netName)
		if err != nil || ip == "" {
			continue
		}
		out[ip] = n.ID
	}
	return out
}

// waitLabHAProxyDial polls until the lab's HAProxy node and its backing
// Patroni cluster are both running, then returns two connection pools dialed
// straight at HAProxy's own container — one at the write front-end (:5000,
// routed to the Leader) and one at the read front-end (:5001, round-robined
// across Replicas) — exactly the ports a real application would use. ok is
// false if the lab has no HAProxy node, or nothing came up within the wait
// window.
func (a *App) waitLabHAProxyDial(ctx context.Context, stackID int64) (writeDB, readDB *sql.DB, ok bool) {
	deadline := time.Now().Add(15 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-ticker.C:
		}
		st, err := a.store.GetStack(stackID)
		if err != nil {
			return nil, nil, false
		}
		var doc designDoc
		if json.Unmarshal(st.Design, &doc) != nil {
			continue
		}
		var haproxyNodeID string
		for _, n := range doc.Nodes {
			if n.Type == "haproxy" {
				haproxyNodeID = n.ID
				break
			}
		}
		if haproxyNodeID == "" {
			return nil, nil, false // this lab's design has no HAProxy node
		}
		hdep, err := a.store.GetDeployment(stackID, haproxyNodeID)
		if err != nil || hdep.State != DeployRunning || hdep.ContainerID == "" {
			continue
		}
		running, err := a.runningPatroniMembers(st, doc)
		if err != nil || len(running) == 0 {
			continue
		}
		var sec pgSecrets
		json.Unmarshal(running[0].Secrets, &sec)
		if sec.SuperPassword == "" {
			continue
		}

		netName := networkName(stackID)
		eng := a.dialEngine(stackID, hdep.ContainerID)
		if err := a.joinStackForDial(ctx, eng, netName); err != nil {
			continue
		}
		ip, err := eng.ContainerIP(ctx, hdep.ContainerID, netName)
		if err != nil || ip == "" {
			continue
		}

		wDSN := labHAProxyDSN(sec, ip, haproxyWritePort)
		rDSN := labHAProxyDSN(sec, ip, haproxyReadPort)
		wDB, err := sql.Open("pgx", wDSN)
		if err != nil {
			continue
		}
		rDB, err := sql.Open("pgx", rDSN)
		if err != nil {
			wDB.Close()
			continue
		}
		wDB.SetMaxOpenConns(labTrafficConnPoolSize)
		rDB.SetMaxOpenConns(labTrafficConnPoolSize)
		// database/sql defaults to 2 idle connections — with up to
		// labTrafficMaxThreads workers each holding a connection, that would
		// close and re-dial most of them on every single cycle instead of
		// reusing them, which at a few hundred ops/sec exhausts ephemeral
		// source ports (surfaces as "cannot assign requested address" dial
		// errors).
		wDB.SetMaxIdleConns(labTrafficConnPoolSize)
		rDB.SetMaxIdleConns(labTrafficConnPoolSize)
		if err := wDB.PingContext(ctx); err != nil {
			wDB.Close()
			rDB.Close()
			continue
		}
		return wDB, rDB, true
	}
	return nil, nil, false
}

func labHAProxyDSN(sec pgSecrets, ip string, port int) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(sec.Super(), sec.SuperPassword),
		Host:     fmt.Sprintf("%s:%d", ip, port),
		Path:     "/postgres",
		RawQuery: "sslmode=prefer&connect_timeout=10",
	}).String()
}

// stopLabTraffic cancels a stack's CRUD generator, if one is running. Called
// from teardownStack — the single choke point for both an explicit "End Lab"
// destroy and TTL-reaper expiry — so it never outlives the cluster it targets.
func stopLabTraffic(stackID int64) {
	labTrafficMu.Lock()
	t, ok := labTraffics[stackID]
	if ok {
		delete(labTraffics, stackID)
	}
	labTrafficMu.Unlock()
	if ok {
		t.cancel()
	}
}

// labTrafficFor returns the running generator for a stack, if any.
func labTrafficFor(stackID int64) (*labTrafficRun, bool) {
	labTrafficMu.Lock()
	defer labTrafficMu.Unlock()
	t, ok := labTraffics[stackID]
	return t, ok
}

// ------------------------------------------------------------------- HTTP

// handleLabTrafficStatus returns the learner's active lab run's CRUD traffic
// stats (zeros/running=false if no generator is active yet — the cluster may
// still be deploying, or this lab's design has no HAProxy node).
func (a *App) handleLabTrafficStatus(w http.ResponseWriter, r *http.Request) {
	run, ok := a.activeLabRunForRequest(w, r)
	if !ok {
		return
	}
	t, ok := labTrafficFor(run.StackID)
	if !ok {
		writeJSON(w, http.StatusOK, labTrafficSnapshot{})
		return
	}
	snap := t.snapshot()
	if st, err := a.store.GetStack(run.StackID); err == nil {
		var doc designDoc
		if json.Unmarshal(st.Design, &doc) == nil {
			snap.Nodes = t.nodeSnapshots(doc)
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleLabTrafficPause / handleLabTrafficResume toggle the generator without
// stopping it — the connections and counters stay alive, only the operation
// cycle is skipped while paused.
func (a *App) handleLabTrafficPause(w http.ResponseWriter, r *http.Request) {
	a.setLabTrafficPaused(w, r, true)
}

func (a *App) handleLabTrafficResume(w http.ResponseWriter, r *http.Request) {
	a.setLabTrafficPaused(w, r, false)
}

func (a *App) setLabTrafficPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	run, ok := a.activeLabRunForRequest(w, r)
	if !ok {
		return
	}
	t, ok := labTrafficFor(run.StackID)
	if !ok {
		writeErr(w, http.StatusConflict, "no traffic generator is running for this lab yet")
		return
	}
	t.paused.Store(paused)
	writeJSON(w, http.StatusOK, t.snapshot())
}

// handleLabTrafficRate sets the generator's target rate (1-1000 operation
// cycles/sec) from the frontend's slider — clamped rather than rejected out
// of range, so a stray value never 400s the UI.
func (a *App) handleLabTrafficRate(w http.ResponseWriter, r *http.Request) {
	run, ok := a.activeLabRunForRequest(w, r)
	if !ok {
		return
	}
	t, ok := labTrafficFor(run.StackID)
	if !ok {
		writeErr(w, http.StatusConflict, "no traffic generator is running for this lab yet")
		return
	}
	var body struct {
		Rate int `json:"rate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	t.rate.Store(clampLabTrafficRate(body.Rate))
	writeJSON(w, http.StatusOK, t.snapshot())
}

// handleLabTrafficThreads sets the generator's active worker count (1-8)
// from the frontend's slider — clamped rather than rejected out of range.
func (a *App) handleLabTrafficThreads(w http.ResponseWriter, r *http.Request) {
	run, ok := a.activeLabRunForRequest(w, r)
	if !ok {
		return
	}
	t, ok := labTrafficFor(run.StackID)
	if !ok {
		writeErr(w, http.StatusConflict, "no traffic generator is running for this lab yet")
		return
	}
	var body struct {
		Threads int `json:"threads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	t.threads.Store(clampLabTrafficThreads(body.Threads))
	writeJSON(w, http.StatusOK, t.snapshot())
}

// handleLabTrafficTables sets how many of the precreated lab_traffic_1..30
// tables traffic is currently spread across (1-30) from the frontend's
// slider — clamped rather than rejected out of range.
func (a *App) handleLabTrafficTables(w http.ResponseWriter, r *http.Request) {
	run, ok := a.activeLabRunForRequest(w, r)
	if !ok {
		return
	}
	t, ok := labTrafficFor(run.StackID)
	if !ok {
		writeErr(w, http.StatusConflict, "no traffic generator is running for this lab yet")
		return
	}
	var body struct {
		Tables int `json:"tables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	t.tables.Store(clampLabTrafficTables(body.Tables))
	writeJSON(w, http.StatusOK, t.snapshot())
}

// activeLabRunForRequest resolves + authenticates the caller's active run of
// the lab named in the URL — the shared prelude for the traffic endpoints.
func (a *App) activeLabRunForRequest(w http.ResponseWriter, r *http.Request) (LabRun, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return LabRun{}, false
	}
	lab, ok := findLab(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "lab not found")
		return LabRun{}, false
	}
	run, err := a.store.GetActiveLabRun(lab.ID, u.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "start the lab before checking traffic")
		return LabRun{}, false
	}
	return run, true
}
