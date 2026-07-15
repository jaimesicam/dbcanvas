package main

// Query Runner — execution engine. Runs one or more queries concurrently, each
// against its own canvas-provisioned DB node (over TCP via a native driver), with
// per-query load parameters (count / threads / time limit) and an optional
// processlist-based "run condition" gate. All queries in a run start together and
// run in parallel; each gate watches only its own target's processlist. See
// docs/QUERY_RUNNER.md.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// qrMarker tags every statement the runner issues (load + processlist polls) so a
// gate never matches the tool's own activity and deadlocks (self-exclusion).
const qrMarker = "dbcanvas-qr"

const (
	qrMaxQueries = 16
	qrMaxThreads = 64
	qrLatSample  = 5000
)

// ---- request shapes ----

type qrGate struct {
	Enabled   bool   `json:"enabled"`
	Pattern   string `json:"pattern"`
	Condition string `json:"condition"` // "no_match" (fire when nothing matches) | "match" (fire when one matches)
	Check     string `json:"check"`     // "every" (each iteration) | "once" (gate the whole run once)
	PollMs    int    `json:"pollMs"`
}

type qrQuerySpec struct {
	StackID    int64  `json:"stackId"`
	NodeID     string `json:"nodeId"`
	Database   string `json:"database"`
	SQL        string `json:"sql"`
	Count      int64  `json:"count"` // 0 = run until time limit
	Threads    int    `json:"threads"`
	TimeLimitS int    `json:"timeLimitS"`
	Gate       qrGate `json:"gate"`
}

// ---- live run state ----

type qrQuery struct {
	spec   qrQuerySpec
	label  string
	engine string
	driver string
	dsn    string
	token  string // per-query marker; the gate ignores only THIS query's own statements
	re     *regexp.Regexp

	// Resolution inputs — the stack-network join + node-IP dial happen at run start
	// (off the request path), so the first run after the app boots doesn't disrupt its
	// own HTTP response when the app attaches to a stack network for the first time.
	stackID         int64
	nodeContainerID string
	dbUser          string
	dbPass          string
	database        string

	executed  int64 // atomic
	errs      int64 // atomic
	gateWaits int64 // atomic
	gateOpen  atomic.Bool

	mu        sync.Mutex
	status    string // pending|running|done|error|stopped
	lastError string
	latCount  int64
	latSum    float64
	latMin    float64
	latMax    float64
	lat       []float64 // reservoir sample for p95
}

type qrRun struct {
	id      string
	ownerID int64
	app     *App
	start   time.Time
	end     time.Time
	cancel  context.CancelFunc

	mu      sync.Mutex
	status  string // running|done|stopped
	queries []*qrQuery
}

var qrRuns = struct {
	sync.Mutex
	m     map[string]*qrRun
	order []string // insertion order, for pruning + history
}{m: map[string]*qrRun{}}

func qrRegister(run *qrRun) {
	qrRuns.Lock()
	defer qrRuns.Unlock()
	qrRuns.m[run.id] = run
	qrRuns.order = append(qrRuns.order, run.id)
	// Bound memory: keep the most recent 200 runs.
	for len(qrRuns.order) > 200 {
		old := qrRuns.order[0]
		qrRuns.order = qrRuns.order[1:]
		delete(qrRuns.m, old)
	}
}

func qrGet(id string) *qrRun {
	qrRuns.Lock()
	defer qrRuns.Unlock()
	return qrRuns.m[id]
}

// ---- execution ----

// launch starts every query in parallel and returns immediately; a watcher
// goroutine marks the run done when all queries finish.
func (run *qrRun) launch(ctx context.Context) {
	run.start = time.Now()
	var wg sync.WaitGroup
	for _, q := range run.queries {
		wg.Add(1)
		go func(q *qrQuery) {
			defer wg.Done()
			run.runQuery(ctx, q)
		}(q)
	}
	go func() {
		wg.Wait()
		run.mu.Lock()
		if run.status == "running" {
			run.status = "done"
		}
		run.end = time.Now()
		run.mu.Unlock()
	}()
}

// dialNodeDSN joins the node's stack Docker network (idempotent) and builds a native
// driver DSN dialing the node's container IP directly — Docker's embedded DNS doesn't
// know the Intranet's *.<domain> names. Shared by the Query Runner and Benchmark.
// An empty database means the engine default (MySQL: none; Postgres: "postgres").
func (a *App) dialNodeDSN(ctx context.Context, stackID int64, containerID, engine, user, pass, database string) (string, string, error) {
	netName := networkName(stackID)
	if err := a.engCtx(ctx).NetworkConnect(ctx, netName, qrAppContainerID()); err != nil {
		return "", "", fmt.Errorf("join stack network: %v", err)
	}
	ip, err := a.engCtx(ctx).ContainerIP(ctx, containerID, netName)
	if err != nil || ip == "" {
		return "", "", fmt.Errorf("could not resolve node address on the stack network")
	}
	if engine == "mysql" {
		return "mysql", qrMySQLDSN(user, pass, fmt.Sprintf("%s:3306", ip), database), nil
	}
	db := database
	if db == "" {
		db = "postgres"
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("%s:5432", ip),
		Path:     "/" + db,
		RawQuery: "sslmode=prefer&connect_timeout=10",
	}).String()
	return "pgx", dsn, nil
}

// dial resolves the query's connection at run start (off the HTTP handler).
func (run *qrRun) dial(ctx context.Context, q *qrQuery) error {
	driver, dsn, err := run.app.dialNodeDSN(ctx, q.stackID, q.nodeContainerID, q.engine, q.dbUser, q.dbPass, q.database)
	if err != nil {
		return err
	}
	q.driver, q.dsn = driver, dsn
	return nil
}

func (run *qrRun) runQuery(ctx context.Context, q *qrQuery) {
	q.setStatus("running")
	// Join the stack's Docker network and resolve the node's IP now (off the request
	// path). Docker's embedded DNS doesn't know the Intranet's *.<domain> names, so we
	// dial the container IP directly.
	if err := run.dial(ctx, q); err != nil {
		q.fail(err.Error())
		return
	}
	db, err := sql.Open(q.driver, q.dsn)
	if err != nil {
		q.fail(fmt.Sprintf("open: %v", err))
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(q.spec.Threads)
	db.SetMaxIdleConns(q.spec.Threads)
	db.SetConnMaxLifetime(0)

	// Per-query deadline; also honors the run-wide cancel (Stop).
	dur := time.Duration(q.spec.TimeLimitS) * time.Second
	qctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	pctx, pcancel := context.WithTimeout(qctx, 10*time.Second)
	perr := db.PingContext(pctx)
	pcancel()
	if perr != nil {
		q.fail(fmt.Sprintf("connect: %v", perr))
		return
	}

	// Processlist gate.
	if q.spec.Gate.Enabled && q.re != nil {
		poll := time.Duration(q.spec.Gate.PollMs) * time.Millisecond
		if poll < 100*time.Millisecond {
			poll = 100 * time.Millisecond
		}
		go q.pollGate(qctx, db, poll)
		if q.spec.Gate.Check == "once" {
			// Wait until the gate opens once, then run the whole loop ungated.
			if !q.waitGate(qctx) {
				q.finish(ctx)
				return
			}
		}
	}

	stmt := "/* " + q.token + " */ " + q.spec.SQL

	bounded := q.spec.Count > 0 // constant: is there a count budget at all?
	remaining := q.spec.Count   // shared atomic budget across threads (only touched atomically)
	gateEvery := q.spec.Gate.Enabled && q.re != nil && q.spec.Gate.Check == "every"

	var wg sync.WaitGroup
	for t := 0; t < q.spec.Threads; t++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if qctx.Err() != nil {
					return
				}
				if bounded && atomic.AddInt64(&remaining, -1) < 0 {
					return
				}
				if gateEvery && !q.waitGate(qctx) {
					return
				}
				t0 := time.Now()
				_, err := db.ExecContext(qctx, stmt)
				if qctx.Err() != nil && err != nil {
					// Deadline/cancel mid-flight — don't count as a query error.
					return
				}
				atomic.AddInt64(&q.executed, 1)
				if err != nil {
					atomic.AddInt64(&q.errs, 1)
					q.setLastError(err.Error())
				} else {
					q.recordLat(time.Since(t0))
				}
			}
		}()
	}
	wg.Wait()
	q.finish(ctx)
}

// waitGate blocks until the gate is open or the context ends. Returns false if the
// context ended first. Counts one "gate wait" each time it actually has to block.
func (q *qrQuery) waitGate(ctx context.Context) bool {
	if q.gateOpen.Load() {
		return true
	}
	atomic.AddInt64(&q.gateWaits, 1)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
			if q.gateOpen.Load() {
				return true
			}
		}
	}
}

// pollGate periodically reads the target's processlist and updates gateOpen.
func (q *qrQuery) pollGate(ctx context.Context, db *sql.DB, poll time.Duration) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	q.gateOpen.Store(q.evalGate(ctx, db))
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			q.gateOpen.Store(q.evalGate(ctx, db))
		}
	}
}

// evalGate returns whether the gate is currently open per its condition.
func (q *qrQuery) evalGate(ctx context.Context, db *sql.DB) bool {
	var sqlText string
	if q.engine == "mysql" {
		sqlText = "/* " + q.token + " */ SELECT INFO FROM information_schema.PROCESSLIST WHERE INFO IS NOT NULL"
	} else {
		sqlText = "/* " + q.token + " */ SELECT query FROM pg_stat_activity WHERE state='active' AND query IS NOT NULL"
	}
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		return false // can't read the processlist → treat gate as closed
	}
	defer rows.Close()
	matched := false
	for rows.Next() {
		var info string
		if rows.Scan(&info) != nil {
			continue
		}
		if strings.Contains(info, q.token) {
			continue // self-exclusion: ignore only THIS query's own statements (its load + polls)
		}
		if q.re.MatchString(info) {
			matched = true
			break
		}
	}
	if q.spec.Gate.Condition == "match" {
		return matched
	}
	return !matched // "no_match": open when nothing matches
}

// ---- stat helpers ----

func (q *qrQuery) setStatus(s string) {
	q.mu.Lock()
	q.status = s
	q.mu.Unlock()
}

func (q *qrQuery) fail(msg string) {
	q.mu.Lock()
	q.status = "error"
	q.lastError = msg
	q.mu.Unlock()
}

func (q *qrQuery) setLastError(msg string) {
	q.mu.Lock()
	q.lastError = msg
	q.mu.Unlock()
}

// finish sets the terminal status: "stopped" if the run was canceled, else "done".
func (q *qrQuery) finish(ctx context.Context) {
	q.mu.Lock()
	if q.status != "error" {
		if ctx.Err() != nil {
			q.status = "stopped"
		} else {
			q.status = "done"
		}
	}
	q.mu.Unlock()
}

func (q *qrQuery) recordLat(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	q.mu.Lock()
	q.latCount++
	q.latSum += ms
	if q.latCount == 1 || ms < q.latMin {
		q.latMin = ms
	}
	if ms > q.latMax {
		q.latMax = ms
	}
	if len(q.lat) < qrLatSample {
		q.lat = append(q.lat, ms)
	} else if j := rand.Int63n(q.latCount); j < qrLatSample {
		q.lat[j] = ms
	}
	q.mu.Unlock()
}

// ---- JSON snapshots ----

type qrQueryDTO struct {
	Index     int     `json:"index"`
	Label     string  `json:"label"`
	Engine    string  `json:"engine"`
	Status    string  `json:"status"`
	Executed  int64   `json:"executed"`
	Errors    int64   `json:"errors"`
	LastError string  `json:"lastError,omitempty"`
	Gated     bool    `json:"gated"`
	GateOpen  bool    `json:"gateOpen"`
	GateWaits int64   `json:"gateWaits"`
	LatMinMs  float64 `json:"latMinMs"`
	LatAvgMs  float64 `json:"latAvgMs"`
	LatMaxMs  float64 `json:"latMaxMs"`
	LatP95Ms  float64 `json:"latP95Ms"`
}

type qrRunDTO struct {
	ID      string       `json:"id"`
	Status  string       `json:"status"`
	Start   string       `json:"start"`
	End     string       `json:"end,omitempty"`
	Queries []qrQueryDTO `json:"queries"`
}

func (q *qrQuery) snapshot(i int) qrQueryDTO {
	q.mu.Lock()
	defer q.mu.Unlock()
	d := qrQueryDTO{
		Index: i, Label: q.label, Engine: q.engine, Status: q.status,
		Executed: atomic.LoadInt64(&q.executed), Errors: atomic.LoadInt64(&q.errs),
		LastError: q.lastError, Gated: q.spec.Gate.Enabled,
		GateOpen: q.gateOpen.Load(), GateWaits: atomic.LoadInt64(&q.gateWaits),
	}
	if q.latCount > 0 {
		d.LatMinMs = round2(q.latMin)
		d.LatMaxMs = round2(q.latMax)
		d.LatAvgMs = round2(q.latSum / float64(q.latCount))
		d.LatP95Ms = round2(percentile(q.lat, 0.95))
	}
	return d
}

func (run *qrRun) snapshot() qrRunDTO {
	run.mu.Lock()
	defer run.mu.Unlock()
	dto := qrRunDTO{ID: run.id, Status: run.status, Start: run.start.Format(time.RFC3339)}
	if !run.end.IsZero() {
		dto.End = run.end.Format(time.RFC3339)
	}
	for i, q := range run.queries {
		dto.Queries = append(dto.Queries, q.snapshot(i))
	}
	return dto
}

func percentile(sample []float64, p float64) float64 {
	if len(sample) == 0 {
		return 0
	}
	s := append([]float64(nil), sample...)
	sort.Float64s(s)
	idx := int(p * float64(len(s)-1))
	return s[idx]
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// qrMySQLDSN builds a go-sql-driver DSN (Config handles credential escaping).
func qrMySQLDSN(user, pass, addr, db string) string {
	c := mysql.NewConfig()
	c.User = user
	c.Passwd = pass
	c.Net = "tcp"
	c.Addr = addr
	c.DBName = db
	c.Timeout = 10 * time.Second
	c.Params = map[string]string{"parseTime": "true"}
	c.AllowNativePasswords = true
	return c.FormatDSN()
}
