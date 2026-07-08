package main

// Benchmark tool — HTTP handlers, schema, data loader, and per-engine workload SQL.
// Drives a purpose-built star schema (bench_* tables) with one of four workload
// profiles (OLTP / OLAP / read-write / read-only) against a canvas-provisioned
// MySQL/PXC or PostgreSQL node, reporting throughput + latency. Reuses the Query
// Runner's connectivity (native TCP drivers + on-demand stack-network join). The
// execution engine + live stats live in benchmark_run.go. See docs/BENCHMARK_PLAN.md.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	benchMaxScale    = 50
	benchMaxThreads  = 128
	benchTSLayout    = "2006-01-02 15:04:05"
	benchLoadBatch   = 500  // orders/customers/products per INSERT
	benchItemBatch   = 1000 // order_items per INSERT
	benchDefaultDB   = "dbcanvas_bench"
	benchCustPerSF   = 10000
	benchProdPerSF   = 1000
	benchOrderPerSF  = 100000
	benchMaxItemsOrd = 4
)

var benchIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// benchStmtTypes are the latency buckets a run tracks (pre-created so workers never
// allocate under lock).
var benchStmtTypes = []string{
	"point_select", "range_select", "update", "insert", "delete",
	"olap_q1", "olap_q2", "olap_q3", "olap_q4", "olap_q5",
}

// benchWeights are the relative CRUD operation weights.
type benchWeights struct {
	Insert int `json:"insert"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Select int `json:"select"`
}

// benchConfig is the run request.
type benchConfig struct {
	StackID   int64  `json:"stackId"`
	NodeID    string `json:"nodeId"`
	Database  string `json:"database"`
	CreateDB  bool   `json:"createDb"`
	Workload  string `json:"workload"` // oltp | olap | rw | ro | crud
	Scale     int    `json:"scale"`
	Threads   int    `json:"threads"`
	DurationS int    `json:"durationS"`
	WarmupS   int    `json:"warmupS"`
	KeepData  bool   `json:"keepData"`
	Seed      int64  `json:"seed"`
	// CRUD workload: an existing table + how to filter/weight operations.
	Table         string       `json:"table"`
	Schema        string       `json:"schema"`
	FilterColumns []string     `json:"filterColumns"`
	Weights       benchWeights `json:"weights"`
}

// ------------------------------------------------------------------- handlers

func (a *App) handleBenchTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, a.listSQLTargets(u))
}

func (a *App) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var cfg benchConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch cfg.Workload {
	case "oltp", "olap", "rw", "ro":
	case "crud":
		if !benchIdentRe.MatchString(cfg.Table) {
			writeErr(w, http.StatusBadRequest, "CRUD requires an existing table (simple identifier)")
			return
		}
		if cfg.Schema != "" && !benchIdentRe.MatchString(cfg.Schema) {
			writeErr(w, http.StatusBadRequest, "invalid schema name")
			return
		}
		for _, col := range cfg.FilterColumns {
			if !benchIdentRe.MatchString(col) {
				writeErr(w, http.StatusBadRequest, "invalid filter column: "+col)
				return
			}
		}
	default:
		writeErr(w, http.StatusBadRequest, "workload must be one of oltp, olap, rw, ro, crud")
		return
	}
	if strings.TrimSpace(cfg.Database) == "" {
		cfg.Database = benchDefaultDB
	}
	if !benchIdentRe.MatchString(cfg.Database) {
		writeErr(w, http.StatusBadRequest, "database must be a simple identifier (letters, digits, underscore)")
		return
	}
	if cfg.Workload == "crud" {
		cfg.CreateDB = false // CRUD targets an existing table; never create/load
	}
	cfg.Scale = clampInt(cfg.Scale, 1, benchMaxScale)
	cfg.Threads = clampInt(cfg.Threads, 1, benchMaxThreads)
	cfg.DurationS = clampInt(cfg.DurationS, 1, 3600)
	cfg.WarmupS = clampInt(cfg.WarmupS, 0, 600)
	if cfg.Seed == 0 {
		cfg.Seed = time.Now().UnixNano()
	}

	engine, containerID, label, user, pass, err := a.resolveNodeCreds(u, cfg.StackID, cfg.NodeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	run := newBenchRun(a, u.ID, cfg, engine, containerID, label, user, pass)
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	benchRegister(run)
	run.launch(ctx)
	writeJSON(w, http.StatusOK, map[string]string{"runId": run.id})
}

func (a *App) handleBenchStatus(w http.ResponseWriter, r *http.Request) {
	run, ok := a.benchOwnedRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

func (a *App) handleBenchStop(w http.ResponseWriter, r *http.Request) {
	run, ok := a.benchOwnedRun(w, r)
	if !ok {
		return
	}
	run.mu.Lock()
	if run.phase == "running" || run.phase == "warmup" || run.phase == "preparing" || run.phase == "loading" {
		run.phase = "stopped"
	}
	run.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

func (a *App) handleBenchHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, benchHistoryFor(u))
}

func (a *App) benchOwnedRun(w http.ResponseWriter, r *http.Request) (*benchRun, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	run := benchGet(r.PathValue("id"))
	if run == nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	if run.ownerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your run")
		return nil, false
	}
	return run, true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ------------------------------------------------------- create database / schema

// benchCreateDatabase creates cfg.Database if missing, over a maintenance connection
// (MySQL: no default db; Postgres: the "postgres" db — CREATE DATABASE can't run in a
// transaction). The name is a validated identifier, so interpolation is safe.
func (a *App) benchCreateDatabase(ctx context.Context, run *benchRun) error {
	_, dsn, err := a.dialNodeDSN(ctx, run.cfg.StackID, run.nodeContainerID, run.engine, run.dbUser, run.dbPass, "")
	if err != nil {
		return err
	}
	db, err := sql.Open(run.driver, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	name := run.cfg.Database
	if run.engine == "mysql" {
		_, err = db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+name+"`")
		return err
	}
	var one int
	err = db.QueryRowContext(ctx, "SELECT 1 FROM pg_database WHERE datname=$1", name).Scan(&one)
	if err == sql.ErrNoRows {
		_, err = db.ExecContext(ctx, `CREATE DATABASE "`+name+`"`)
		return err
	}
	return err
}

// benchDDL returns the CREATE (parents→children) and DROP (children→parents)
// statements for the schema on the given engine.
func benchDDL(engine string) (create, drop []string) {
	drop = []string{
		"DROP TABLE IF EXISTS bench_order_item",
		"DROP TABLE IF EXISTS bench_order",
		"DROP TABLE IF EXISTS bench_product",
		"DROP TABLE IF EXISTS bench_customer",
		"DROP TABLE IF EXISTS bench_meta",
	}
	if engine == "mysql" {
		create = []string{
			`CREATE TABLE bench_customer (
			  customer_id BIGINT PRIMARY KEY, name VARCHAR(80), email VARCHAR(120),
			  city VARCHAR(60), country VARCHAR(40), segment VARCHAR(20), created_at TIMESTAMP NULL,
			  KEY idx_cust_geo (country, segment), KEY idx_cust_email (email)) ENGINE=InnoDB`,
			`CREATE TABLE bench_product (
			  product_id BIGINT PRIMARY KEY, name VARCHAR(120), category VARCHAR(40),
			  subcategory VARCHAR(40), price DECIMAL(10,2), cost DECIMAL(10,2), active SMALLINT,
			  KEY idx_prod_cat (category, subcategory)) ENGINE=InnoDB`,
			`CREATE TABLE bench_order (
			  order_id BIGINT PRIMARY KEY, customer_id BIGINT, order_ts TIMESTAMP NULL,
			  status VARCHAR(16), total_amount DECIMAL(12,2), item_count INT,
			  KEY idx_ord_cust (customer_id), KEY idx_ord_ts (order_ts), KEY idx_ord_status (status),
			  CONSTRAINT fk_ord_cust FOREIGN KEY (customer_id) REFERENCES bench_customer(customer_id)) ENGINE=InnoDB`,
			`CREATE TABLE bench_order_item (
			  item_id BIGINT PRIMARY KEY, order_id BIGINT, product_id BIGINT, quantity INT,
			  unit_price DECIMAL(10,2), line_amount DECIMAL(12,2),
			  KEY idx_item_ord (order_id), KEY idx_item_prod (product_id),
			  CONSTRAINT fk_item_ord FOREIGN KEY (order_id) REFERENCES bench_order(order_id) ON DELETE CASCADE,
			  CONSTRAINT fk_item_prod FOREIGN KEY (product_id) REFERENCES bench_product(product_id)) ENGINE=InnoDB`,
			`CREATE TABLE bench_meta (k VARCHAR(32) PRIMARY KEY, v VARCHAR(255)) ENGINE=InnoDB`,
		}
		return create, drop
	}
	// postgres
	create = []string{
		`CREATE TABLE bench_customer (
		  customer_id BIGINT PRIMARY KEY, name VARCHAR(80), email VARCHAR(120),
		  city VARCHAR(60), country VARCHAR(40), segment VARCHAR(20), created_at TIMESTAMP)`,
		`CREATE INDEX idx_cust_geo ON bench_customer (country, segment)`,
		`CREATE INDEX idx_cust_email ON bench_customer (email)`,
		`CREATE TABLE bench_product (
		  product_id BIGINT PRIMARY KEY, name VARCHAR(120), category VARCHAR(40),
		  subcategory VARCHAR(40), price DECIMAL(10,2), cost DECIMAL(10,2), active SMALLINT)`,
		`CREATE INDEX idx_prod_cat ON bench_product (category, subcategory)`,
		`CREATE TABLE bench_order (
		  order_id BIGINT PRIMARY KEY, customer_id BIGINT, order_ts TIMESTAMP,
		  status VARCHAR(16), total_amount DECIMAL(12,2), item_count INT,
		  CONSTRAINT fk_ord_cust FOREIGN KEY (customer_id) REFERENCES bench_customer(customer_id))`,
		`CREATE INDEX idx_ord_cust ON bench_order (customer_id)`,
		`CREATE INDEX idx_ord_ts ON bench_order (order_ts)`,
		`CREATE INDEX idx_ord_status ON bench_order (status)`,
		`CREATE TABLE bench_order_item (
		  item_id BIGINT PRIMARY KEY, order_id BIGINT, product_id BIGINT, quantity INT,
		  unit_price DECIMAL(10,2), line_amount DECIMAL(12,2),
		  CONSTRAINT fk_item_ord FOREIGN KEY (order_id) REFERENCES bench_order(order_id) ON DELETE CASCADE,
		  CONSTRAINT fk_item_prod FOREIGN KEY (product_id) REFERENCES bench_product(product_id))`,
		`CREATE INDEX idx_item_ord ON bench_order_item (order_id)`,
		`CREATE INDEX idx_item_prod ON bench_order_item (product_id)`,
		`CREATE TABLE bench_meta (k VARCHAR(32) PRIMARY KEY, v VARCHAR(255))`,
	}
	return create, drop
}

// ------------------------------------------------------------------ workload SQL

// benchSQL holds the engine-correct statement text for a run.
type benchSQL struct {
	selOrderByID, selItemsByOrder, selCustByID, selProdByID, selOrdersByCust string
	updOrderStatus, updProductPrice, insOrder, insItem, delOrder             string
	olapQ1, olapQ2, olapQ3, olapQ4, olapQ5                                   string
}

// pgRebind rewrites `?` placeholders to Postgres `$1, $2, …`.
func pgRebind(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '?' {
			n++
			b.WriteByte('$')
			fmt.Fprintf(&b, "%d", n)
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func buildBenchSQL(engine string) benchSQL {
	q := func(s string) string {
		if engine == "mysql" {
			return s
		}
		return pgRebind(s)
	}
	s := benchSQL{
		selOrderByID:    q("SELECT customer_id, order_ts, status, total_amount, item_count FROM bench_order WHERE order_id = ?"),
		selItemsByOrder: q("SELECT item_id, product_id, quantity, line_amount FROM bench_order_item WHERE order_id = ?"),
		selCustByID:     q("SELECT name, email, country, segment FROM bench_customer WHERE customer_id = ?"),
		selProdByID:     q("SELECT name, category, price FROM bench_product WHERE product_id = ?"),
		selOrdersByCust: q("SELECT order_id, order_ts, total_amount FROM bench_order WHERE customer_id = ? ORDER BY order_ts DESC LIMIT 20"),
		updOrderStatus:  q("UPDATE bench_order SET status = ? WHERE order_id = ?"),
		updProductPrice: q("UPDATE bench_product SET price = price * ? WHERE product_id = ?"),
		insOrder:        q("INSERT INTO bench_order (order_id, customer_id, order_ts, status, total_amount, item_count) VALUES (?,?,?,?,?,?)"),
		insItem:         q("INSERT INTO bench_order_item (item_id, order_id, product_id, quantity, unit_price, line_amount) VALUES (?,?,?,?,?,?)"),
		delOrder:        q("DELETE FROM bench_order WHERE order_id = ?"),
		olapQ1:          "SELECT p.category, SUM(oi.line_amount) rev, COUNT(*) n FROM bench_order_item oi JOIN bench_product p ON p.product_id = oi.product_id GROUP BY p.category ORDER BY rev DESC",
		olapQ3:          "SELECT c.customer_id, c.name, SUM(o.total_amount) spend FROM bench_order o JOIN bench_customer c ON c.customer_id = o.customer_id GROUP BY c.customer_id, c.name ORDER BY spend DESC LIMIT 50",
		olapQ4:          "SELECT c.country, c.segment, AVG(o.total_amount) aov, COUNT(*) n FROM bench_order o JOIN bench_customer c ON c.customer_id = o.customer_id GROUP BY c.country, c.segment ORDER BY aov DESC",
	}
	if engine == "mysql" {
		s.olapQ2 = "SELECT DATE_FORMAT(order_ts,'%Y-%m') m, COUNT(*) n, SUM(total_amount) rev FROM bench_order GROUP BY m ORDER BY m"
		s.olapQ5 = "SELECT p.product_id, p.name, SUM(oi.quantity) units, SUM(oi.line_amount) rev FROM bench_order_item oi JOIN bench_product p ON p.product_id = oi.product_id JOIN bench_order o ON o.order_id = oi.order_id WHERE o.order_ts >= ? GROUP BY p.product_id, p.name ORDER BY rev DESC LIMIT 100"
	} else {
		s.olapQ2 = "SELECT to_char(date_trunc('month', order_ts),'YYYY-MM') m, COUNT(*) n, SUM(total_amount) rev FROM bench_order GROUP BY m ORDER BY m"
		s.olapQ5 = pgRebind("SELECT p.product_id, p.name, SUM(oi.quantity) units, SUM(oi.line_amount) rev FROM bench_order_item oi JOIN bench_product p ON p.product_id = oi.product_id JOIN bench_order o ON o.order_id = oi.order_id WHERE o.order_ts >= ? GROUP BY p.product_id, p.name ORDER BY rev DESC LIMIT 100")
	}
	return s
}

// ------------------------------------------------------------------ data loader

var (
	benchCountries  = []string{"USA", "Canada", "UK", "Germany", "France", "Japan", "Brazil", "India"}
	benchCities     = []string{"Metropolis", "Springfield", "Rivertown", "Lakeside", "Harbor", "Hillcrest", "Fairview"}
	benchSegments   = []string{"consumer", "corporate", "smb"}
	benchCategories = []string{"Electronics", "Clothing", "Home", "Sports", "Toys", "Books", "Grocery"}
	benchStatuses   = []string{"new", "paid", "shipped", "cancelled"}
)

func benchStatusStr(rng *rand.Rand) string { return benchStatuses[rng.Intn(len(benchStatuses))] }

func (run *benchRun) insertBatch(ctx context.Context, db *sql.DB, table, cols string, tuples []string) error {
	if len(tuples) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, "INSERT INTO "+table+" ("+cols+") VALUES "+strings.Join(tuples, ","))
	return err
}

// load populates the schema deterministically from cfg.Seed at the configured scale.
func (run *benchRun) load(ctx context.Context, db *sql.DB) error {
	rng := rand.New(rand.NewSource(run.cfg.Seed))
	now := time.Now()

	// Customers.
	var buf []string
	for id := int64(1); id <= run.nCust; id++ {
		created := now.Add(-time.Duration(rng.Intn(3*365*24)) * time.Hour)
		buf = append(buf, fmt.Sprintf("(%d,'Customer %d','user%d@example.com','%s','%s','%s','%s')",
			id, id, id, benchCities[rng.Intn(len(benchCities))], benchCountries[rng.Intn(len(benchCountries))],
			benchSegments[rng.Intn(len(benchSegments))], created.Format(benchTSLayout)))
		if len(buf) >= benchLoadBatch {
			if err := run.insertBatch(ctx, db, "bench_customer", "customer_id,name,email,city,country,segment,created_at", buf); err != nil {
				return err
			}
			buf = buf[:0]
			atomic.AddInt64(&run.rowsLoaded, benchLoadBatch)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if err := run.insertBatch(ctx, db, "bench_customer", "customer_id,name,email,city,country,segment,created_at", buf); err != nil {
		return err
	}
	buf = buf[:0]

	// Products.
	for id := int64(1); id <= run.nProd; id++ {
		cat := benchCategories[rng.Intn(len(benchCategories))]
		price := float64(rng.Intn(99000)+100) / 100.0
		buf = append(buf, fmt.Sprintf("(%d,'Product %d','%s','%s-%d',%.2f,%.2f,1)",
			id, id, cat, cat, rng.Intn(5)+1, price, price*0.6))
		if len(buf) >= benchLoadBatch {
			if err := run.insertBatch(ctx, db, "bench_product", "product_id,name,category,subcategory,price,cost,active", buf); err != nil {
				return err
			}
			buf = buf[:0]
			atomic.AddInt64(&run.rowsLoaded, benchLoadBatch)
		}
	}
	if err := run.insertBatch(ctx, db, "bench_product", "product_id,name,category,subcategory,price,cost,active", buf); err != nil {
		return err
	}

	// Orders + items (parents before children per batch to satisfy the FK).
	var obuf, ibuf []string
	itemID := int64(0)
	flush := func() error {
		if err := run.insertBatch(ctx, db, "bench_order", "order_id,customer_id,order_ts,status,total_amount,item_count", obuf); err != nil {
			return err
		}
		if err := run.insertBatch(ctx, db, "bench_order_item", "item_id,order_id,product_id,quantity,unit_price,line_amount", ibuf); err != nil {
			return err
		}
		atomic.AddInt64(&run.rowsLoaded, int64(len(obuf)+len(ibuf)))
		obuf, ibuf = obuf[:0], ibuf[:0]
		return nil
	}
	for oid := int64(1); oid <= run.nOrd; oid++ {
		cust := rng.Int63n(run.nCust) + 1
		ts := now.Add(-time.Duration(rng.Intn(365*24)) * time.Hour)
		nItems := rng.Intn(benchMaxItemsOrd) + 1
		total := 0.0
		for k := 0; k < nItems; k++ {
			itemID++
			qty := rng.Intn(5) + 1
			price := float64(rng.Intn(9900)+100) / 100.0
			line := price * float64(qty)
			total += line
			ibuf = append(ibuf, fmt.Sprintf("(%d,%d,%d,%d,%.2f,%.2f)", itemID, oid, rng.Int63n(run.nProd)+1, qty, price, line))
		}
		obuf = append(obuf, fmt.Sprintf("(%d,%d,'%s','%s',%.2f,%d)", oid, cust, ts.Format(benchTSLayout), benchStatusStr(rng), total, nItems))
		if len(obuf) >= benchLoadBatch || len(ibuf) >= benchItemBatch {
			if err := flush(); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return flush()
}
