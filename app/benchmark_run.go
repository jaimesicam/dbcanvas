package main

// Benchmark tool — execution engine + live stats. Orchestrates prepare → warmup →
// measure → cleanup, drives the workload with N worker goroutines, and tracks
// per-statement-type + per-transaction latency. See benchmark.go for handlers,
// schema, loader, and workload SQL, and docs/BENCHMARK_PLAN.md for the design.

import (
	"context"
	"database/sql"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ---- latency accumulator ----

type latAcc struct {
	mu     sync.Mutex
	count  int64
	errs   int64
	sum    float64
	min    float64
	max    float64
	sample []float64
}

func (l *latAcc) rec(d time.Duration) {
	ms := float64(d.Microseconds()) / 1000.0
	l.mu.Lock()
	l.count++
	l.sum += ms
	if l.count == 1 || ms < l.min {
		l.min = ms
	}
	if ms > l.max {
		l.max = ms
	}
	if len(l.sample) < qrLatSample {
		l.sample = append(l.sample, ms)
	} else if j := rand.Int63n(l.count); j < qrLatSample {
		l.sample[j] = ms
	}
	l.mu.Unlock()
}

func (l *latAcc) recErr() {
	l.mu.Lock()
	l.errs++
	l.mu.Unlock()
}

// ---- run state ----

type benchRun struct {
	id      string
	ownerID int64
	app     *App
	cfg     benchConfig

	engine          string
	driver          string
	label           string
	dbUser, dbPass  string
	nodeContainerID string

	cancel context.CancelFunc

	mu         sync.Mutex
	phase      string // preparing | loading | warmup | running | done | error | stopped
	message    string
	start      time.Time
	measureAt  time.Time
	measureEnd time.Time
	end        time.Time

	rowsLoaded int64 // atomic
	txns       int64 // atomic — measured units
	stmts      map[string]*latAcc
	txnLat     *latAcc

	nCust, nProd, nOrd      int64
	nextOrderID, nextItemID int64 // atomic

	crud   *crudPlan   // CRUD workload only
	sample *crudSample // CRUD workload only — sampled filter-key pool
}

func newBenchRun(a *App, ownerID int64, cfg benchConfig, engine, containerID, label, user, pass string) *benchRun {
	driver := "pgx"
	if engine == "mysql" {
		driver = "mysql"
	}
	run := &benchRun{
		id: qrNewID(), ownerID: ownerID, app: a, cfg: cfg,
		engine: engine, driver: driver, label: label,
		dbUser: user, dbPass: pass, nodeContainerID: containerID,
		phase: "preparing", stmts: map[string]*latAcc{}, txnLat: &latAcc{},
	}
	for _, t := range benchStmtTypes {
		run.stmts[t] = &latAcc{}
	}
	return run
}

var benchRuns = struct {
	sync.Mutex
	m     map[string]*benchRun
	order []string
}{m: map[string]*benchRun{}}

func benchRegister(run *benchRun) {
	benchRuns.Lock()
	defer benchRuns.Unlock()
	benchRuns.m[run.id] = run
	benchRuns.order = append(benchRuns.order, run.id)
	for len(benchRuns.order) > 200 {
		delete(benchRuns.m, benchRuns.order[0])
		benchRuns.order = benchRuns.order[1:]
	}
}

func benchGet(id string) *benchRun {
	benchRuns.Lock()
	defer benchRuns.Unlock()
	return benchRuns.m[id]
}

func benchHistoryFor(u User) []benchRunDTO {
	benchRuns.Lock()
	runs := make([]*benchRun, 0, len(benchRuns.order))
	for i := len(benchRuns.order) - 1; i >= 0; i-- {
		if r := benchRuns.m[benchRuns.order[i]]; r != nil && (r.ownerID == u.ID || u.Role == RoleAdmin) {
			runs = append(runs, r)
		}
	}
	benchRuns.Unlock()
	out := make([]benchRunDTO, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.snapshot())
	}
	return out
}

func (run *benchRun) setPhase(phase, msg string) {
	run.mu.Lock()
	run.phase = phase
	run.message = msg
	run.mu.Unlock()
}

func (run *benchRun) fail(msg string) {
	run.mu.Lock()
	run.phase = "error"
	run.message = msg
	run.mu.Unlock()
}

func (run *benchRun) setRanges() {
	sf := int64(run.cfg.Scale)
	run.nCust = benchCustPerSF * sf
	run.nProd = benchProdPerSF * sf
	run.nOrd = benchOrderPerSF * sf
}

// ---- lifecycle ----

func (run *benchRun) launch(ctx context.Context) {
	run.start = time.Now()
	go run.execute(ctx)
}

func (run *benchRun) execute(ctx context.Context) {
	defer func() {
		run.mu.Lock()
		if run.phase != "error" {
			if ctx.Err() != nil {
				run.phase = "stopped"
			} else {
				run.phase = "done"
			}
		}
		run.end = time.Now()
		run.mu.Unlock()
	}()

	if run.cfg.Workload != "crud" {
		run.setRanges()
	}

	if run.cfg.CreateDB {
		run.setPhase("preparing", "creating database")
		if err := run.app.benchCreateDatabase(ctx, run); err != nil {
			run.fail("create database: " + err.Error())
			return
		}
	}

	_, dsn, err := run.app.dialNodeDSN(ctx, run.cfg.StackID, run.nodeContainerID, run.engine, run.dbUser, run.dbPass, run.cfg.Database)
	if err != nil {
		run.fail(err.Error())
		return
	}
	db, err := sql.Open(run.driver, dsn)
	if err != nil {
		run.fail("open: " + err.Error())
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(run.cfg.Threads + 2)
	db.SetMaxIdleConns(run.cfg.Threads + 2)
	pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
	perr := db.PingContext(pctx)
	pcancel()
	if perr != nil {
		run.fail("connect: " + perr.Error())
		return
	}

	if run.cfg.Workload == "crud" {
		run.setPhase("preparing", "introspecting table")
		plan, perr := run.buildCRUDPlan(ctx)
		if perr != nil {
			run.fail(perr.Error())
			return
		}
		run.crud = plan
		run.sample = &crudSample{}
		run.crudRefreshSample(ctx, db) // initial pool from existing rows
		go run.crudSampler(ctx, db)
	} else {
		if err := run.prepare(ctx, db); err != nil {
			run.fail(err.Error())
			return
		}
	}
	if ctx.Err() != nil {
		return
	}

	s := buildBenchSQL(run.engine)

	if run.cfg.WarmupS > 0 {
		run.setPhase("warmup", "")
		run.drive(ctx, db, s, time.Duration(run.cfg.WarmupS)*time.Second, false)
		if ctx.Err() != nil {
			return
		}
	}

	run.mu.Lock()
	run.phase = "running"
	run.message = ""
	run.measureAt = time.Now()
	run.mu.Unlock()
	run.drive(ctx, db, s, time.Duration(run.cfg.DurationS)*time.Second, true)
	run.mu.Lock()
	run.measureEnd = time.Now()
	run.mu.Unlock()

	if !run.cfg.KeepData && run.cfg.Workload != "crud" {
		// Fresh context so a stopped/expired run still cleans up its tables. CRUD never
		// drops the user's own table.
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		run.dropSchema(cctx, db)
		ccancel()
	}
}

// prepare loads the dataset (or reuses it when Keep-data is on and it matches).
func (run *benchRun) prepare(ctx context.Context, db *sql.DB) error {
	if run.cfg.KeepData && run.datasetMatches(ctx, db) {
		run.setPhase("preparing", "reusing existing dataset")
	} else {
		run.setPhase("preparing", "creating schema")
		if err := run.dropSchema(ctx, db); err != nil {
			return err
		}
		if err := run.createSchema(ctx, db); err != nil {
			return err
		}
		run.setPhase("loading", "loading data")
		if err := run.load(ctx, db); err != nil {
			return err
		}
		run.writeMeta(ctx, db)
	}
	return run.initCounters(ctx, db)
}

func (run *benchRun) dropSchema(ctx context.Context, db *sql.DB) error {
	_, drops := benchDDL(run.engine)
	for _, s := range drops {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (run *benchRun) createSchema(ctx context.Context, db *sql.DB) error {
	creates, _ := benchDDL(run.engine)
	for _, s := range creates {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (run *benchRun) datasetMatches(ctx context.Context, db *sql.DB) bool {
	var sv, sd string
	if db.QueryRowContext(ctx, "SELECT v FROM bench_meta WHERE k='scale'").Scan(&sv) != nil {
		return false
	}
	if db.QueryRowContext(ctx, "SELECT v FROM bench_meta WHERE k='seed'").Scan(&sd) != nil {
		return false
	}
	scale, _ := strconv.Atoi(sv)
	seed, _ := strconv.ParseInt(sd, 10, 64)
	return scale == run.cfg.Scale && seed == run.cfg.Seed
}

func (run *benchRun) writeMeta(ctx context.Context, db *sql.DB) {
	// scale/seed are integers → safe to interpolate (no per-engine placeholders).
	db.ExecContext(ctx, "INSERT INTO bench_meta (k,v) VALUES ('scale','"+strconv.Itoa(run.cfg.Scale)+"'),('seed','"+strconv.FormatInt(run.cfg.Seed, 10)+"')")
}

func (run *benchRun) initCounters(ctx context.Context, db *sql.DB) error {
	var mo, mi sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(order_id),0) FROM bench_order").Scan(&mo); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(item_id),0) FROM bench_order_item").Scan(&mi); err != nil {
		return err
	}
	start := mo.Int64
	if run.nOrd > start {
		start = run.nOrd
	}
	atomic.StoreInt64(&run.nextOrderID, start)
	atomic.StoreInt64(&run.nextItemID, mi.Int64)
	return nil
}

// ---- driving the workload ----

func (run *benchRun) drive(ctx context.Context, db *sql.DB, s benchSQL, dur time.Duration, record bool) {
	dctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var wg sync.WaitGroup
	for t := 0; t < run.cfg.Threads; t++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for dctx.Err() == nil {
				run.unit(dctx, db, s, rng, record)
			}
		}(run.cfg.Seed + int64(t) + 1)
	}
	wg.Wait()
}

func (run *benchRun) unit(ctx context.Context, db *sql.DB, s benchSQL, rng *rand.Rand, record bool) {
	t0 := time.Now()
	var err error
	switch run.cfg.Workload {
	case "olap":
		err = run.unitOLAP(ctx, db, s, rng, record)
	case "ro":
		err = run.unitRO(ctx, db, s, rng, record)
	case "rw":
		err = run.unitRW(ctx, db, s, rng, record)
	case "crud":
		err = run.unitCRUD(ctx, db, rng, record)
	default:
		err = run.unitOLTP(ctx, db, s, rng, record)
	}
	if record && ctx.Err() == nil {
		atomic.AddInt64(&run.txns, 1)
		if err == nil {
			run.txnLat.rec(time.Since(t0))
		}
	}
}

// tq runs a query, drains rows, and records latency/errors for its statement type.
func (run *benchRun) tq(ctx context.Context, e execer, typ string, record bool, query string, args ...any) error {
	t0 := time.Now()
	rows, err := e.QueryContext(ctx, query, args...)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	run.recStmt(ctx, typ, record, t0, err)
	return err
}

func (run *benchRun) te(ctx context.Context, e execer, typ string, record bool, query string, args ...any) error {
	t0 := time.Now()
	_, err := e.ExecContext(ctx, query, args...)
	run.recStmt(ctx, typ, record, t0, err)
	return err
}

// recStmt records one statement's latency (on success) or error (on failure).
// A failure whose context has been cancelled is a window-close/stop teardown, not a
// workload error, so it is dropped — mirroring the ctx.Err() guard in unit().
func (run *benchRun) recStmt(ctx context.Context, typ string, record bool, t0 time.Time, err error) {
	if !record {
		return
	}
	a := run.stmts[typ]
	if a == nil {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			a.recErr()
		}
		return
	}
	a.rec(time.Since(t0))
}

func benchPickStatus(rng *rand.Rand) string { return benchStatuses[rng.Intn(len(benchStatuses))] }

// insertOrder inserts one new order + its items (unique ids from the shared counters).
func (run *benchRun) insertOrder(ctx context.Context, e execer, s benchSQL, rng *rand.Rand, record bool) error {
	oid := atomic.AddInt64(&run.nextOrderID, 1)
	nItems := rng.Intn(benchMaxItemsOrd) + 1
	total := 0.0
	if err := run.te(ctx, e, "insert", record, s.insOrder, oid, rng.Int63n(run.nCust)+1, time.Now(), "new", 0.0, nItems); err != nil {
		return err
	}
	for i := 0; i < nItems; i++ {
		iid := atomic.AddInt64(&run.nextItemID, 1)
		qty := rng.Intn(5) + 1
		price := float64(rng.Intn(9900)+100) / 100.0
		total += price * float64(qty)
		if err := run.te(ctx, e, "insert", record, s.insItem, iid, oid, rng.Int63n(run.nProd)+1, qty, price, price*float64(qty)); err != nil {
			return err
		}
	}
	return nil
}

func (run *benchRun) unitOLTP(ctx context.Context, db *sql.DB, s benchSQL, rng *rand.Rand, record bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ord := rng.Int63n(run.nOrd) + 1
	cust := rng.Int63n(run.nCust) + 1
	prod := rng.Int63n(run.nProd) + 1
	if err := run.tq(ctx, tx, "point_select", record, s.selOrderByID, ord); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "range_select", record, s.selItemsByOrder, ord); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "point_select", record, s.selCustByID, cust); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "point_select", record, s.selProdByID, prod); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "range_select", record, s.selOrdersByCust, cust); err != nil {
		return err
	}
	if err := run.te(ctx, tx, "update", record, s.updOrderStatus, benchPickStatus(rng), ord); err != nil {
		return err
	}
	if err := run.insertOrder(ctx, tx, s, rng, record); err != nil {
		return err
	}
	if err := run.te(ctx, tx, "delete", record, s.delOrder, rng.Int63n(run.nOrd)+1); err != nil {
		return err
	}
	return tx.Commit()
}

func (run *benchRun) unitRW(ctx context.Context, db *sql.DB, s benchSQL, rng *rand.Rand, record bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := run.insertOrder(ctx, tx, s, rng, record); err != nil {
		return err
	}
	if err := run.te(ctx, tx, "update", record, s.updOrderStatus, benchPickStatus(rng), rng.Int63n(run.nOrd)+1); err != nil {
		return err
	}
	if err := run.te(ctx, tx, "update", record, s.updProductPrice, 1.0, rng.Int63n(run.nProd)+1); err != nil {
		return err
	}
	if err := run.te(ctx, tx, "delete", record, s.delOrder, rng.Int63n(run.nOrd)+1); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "point_select", record, s.selOrderByID, rng.Int63n(run.nOrd)+1); err != nil {
		return err
	}
	if err := run.tq(ctx, tx, "point_select", record, s.selCustByID, rng.Int63n(run.nCust)+1); err != nil {
		return err
	}
	return tx.Commit()
}

func (run *benchRun) unitRO(ctx context.Context, db *sql.DB, s benchSQL, rng *rand.Rand, record bool) error {
	for i := 0; i < 10; i++ {
		var err error
		switch i % 3 {
		case 0:
			err = run.tq(ctx, db, "point_select", record, s.selOrderByID, rng.Int63n(run.nOrd)+1)
		case 1:
			err = run.tq(ctx, db, "point_select", record, s.selCustByID, rng.Int63n(run.nCust)+1)
		default:
			err = run.tq(ctx, db, "point_select", record, s.selProdByID, rng.Int63n(run.nProd)+1)
		}
		if err != nil {
			return err
		}
	}
	if err := run.tq(ctx, db, "range_select", record, s.selOrdersByCust, rng.Int63n(run.nCust)+1); err != nil {
		return err
	}
	return run.tq(ctx, db, "range_select", record, s.selItemsByOrder, rng.Int63n(run.nOrd)+1)
}

func (run *benchRun) unitOLAP(ctx context.Context, db *sql.DB, s benchSQL, rng *rand.Rand, record bool) error {
	switch rng.Intn(5) {
	case 0:
		return run.tq(ctx, db, "olap_q1", record, s.olapQ1)
	case 1:
		return run.tq(ctx, db, "olap_q2", record, s.olapQ2)
	case 2:
		return run.tq(ctx, db, "olap_q3", record, s.olapQ3)
	case 3:
		return run.tq(ctx, db, "olap_q4", record, s.olapQ4)
	default:
		since := time.Now().Add(-time.Duration(rng.Intn(300)+30) * 24 * time.Hour)
		return run.tq(ctx, db, "olap_q5", record, s.olapQ5, since)
	}
}

// ---- snapshot ----

type benchStmtDTO struct {
	Type   string  `json:"type"`
	Count  int64   `json:"count"`
	Errors int64   `json:"errors"`
	AvgMs  float64 `json:"avgMs"`
	P95Ms  float64 `json:"p95Ms"`
	P99Ms  float64 `json:"p99Ms"`
}

type benchRunDTO struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"` // = phase
	Message    string         `json:"message,omitempty"`
	Workload   string         `json:"workload"`
	Engine     string         `json:"engine"`
	Label      string         `json:"label"`
	Database   string         `json:"database"`
	Table      string         `json:"table,omitempty"`
	Scale      int            `json:"scale"`
	Threads    int            `json:"threads"`
	DurationS  int            `json:"durationS"`
	KeepData   bool           `json:"keepData"`
	RowsLoaded int64          `json:"rowsLoaded"`
	RowsTarget int64          `json:"rowsTarget"`
	ElapsedS   float64        `json:"elapsedS"`
	Txns       int64          `json:"txns"`
	Queries    int64          `json:"queries"`
	TPS        float64        `json:"tps"`
	QPS        float64        `json:"qps"`
	TxnP50Ms   float64        `json:"txnP50Ms"`
	TxnP95Ms   float64        `json:"txnP95Ms"`
	TxnP99Ms   float64        `json:"txnP99Ms"`
	Stmts      []benchStmtDTO `json:"stmts"`
	Start      string         `json:"start"`
	End        string         `json:"end,omitempty"`
}

func (l *latAcc) stmtDTO(typ string) benchStmtDTO {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := benchStmtDTO{Type: typ, Count: l.count, Errors: l.errs}
	if l.count > 0 {
		d.AvgMs = round2(l.sum / float64(l.count))
		d.P95Ms = round2(percentile(l.sample, 0.95))
		d.P99Ms = round2(percentile(l.sample, 0.99))
	}
	return d
}

func (run *benchRun) snapshot() benchRunDTO {
	run.mu.Lock()
	defer run.mu.Unlock()
	dto := benchRunDTO{
		ID: run.id, Status: run.phase, Message: run.message,
		Workload: run.cfg.Workload, Engine: run.engine, Label: run.label,
		Database: run.cfg.Database, Table: run.cfg.Table, Scale: run.cfg.Scale, Threads: run.cfg.Threads,
		DurationS: run.cfg.DurationS, KeepData: run.cfg.KeepData,
		RowsLoaded: atomic.LoadInt64(&run.rowsLoaded),
		RowsTarget: run.nCust + run.nProd + run.nOrd*3,
		Txns:       atomic.LoadInt64(&run.txns),
		Start:      run.start.Format(time.RFC3339),
	}
	if !run.end.IsZero() {
		dto.End = run.end.Format(time.RFC3339)
	}
	// Measured window elapsed.
	if !run.measureAt.IsZero() {
		end := run.measureEnd
		if end.IsZero() {
			end = time.Now()
		}
		dto.ElapsedS = round2(end.Sub(run.measureAt).Seconds())
	}
	var queries int64
	for _, typ := range benchStmtTypes {
		s := run.stmts[typ].stmtDTO(typ)
		if s.Count > 0 || s.Errors > 0 {
			dto.Stmts = append(dto.Stmts, s)
			queries += s.Count
		}
	}
	dto.Queries = queries
	if dto.ElapsedS > 0 {
		dto.TPS = round2(float64(dto.Txns) / dto.ElapsedS)
		dto.QPS = round2(float64(queries) / dto.ElapsedS)
	}
	run.txnLat.mu.Lock()
	if run.txnLat.count > 0 {
		dto.TxnP50Ms = round2(percentile(run.txnLat.sample, 0.50))
		dto.TxnP95Ms = round2(percentile(run.txnLat.sample, 0.95))
		dto.TxnP99Ms = round2(percentile(run.txnLat.sample, 0.99))
	}
	run.txnLat.mu.Unlock()
	return dto
}
