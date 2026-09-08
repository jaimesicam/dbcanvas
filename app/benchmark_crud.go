package main

// CRUD workload — insert / update / delete / select against an EXISTING user table (rather
// than the bench_* star schema). Operations are chosen per iteration by configurable weights;
// UPDATE/DELETE/SELECT filter on the primary key (single or composite) or user-selected
// columns, randomly using a subset of them each time. A background sampler periodically pulls
// real rows so those ops target existing data. Reuses the Data Generator's table introspection
// (tableMeta) + per-column value generators (colGen).

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const crudSampleSize = 2000 // rows pulled per sampler refresh

// crudPlan is the introspected plan for a CRUD run against one table.
type crudPlan struct {
	tableRef     string   // quoted schema.table
	insertVerb   string   // "INSERT INTO " | "INSERT IGNORE INTO "
	insertCols   string   // "(c1,c2,...)"
	insertGens   []colGen // parallel to insertCols
	conflict     string   // trailing conflict clause (pg)
	filterCols   []string // quoted filter-column names, tuple order
	filterSelect string   // comma-joined filter cols (sampler SELECT list)
	filterN      int
	updateCols   []dgColumn // insertable, non-filter columns (UPDATE SET targets)
	nonce        string     // per-run nonce for unique columns
	rowSeq       int64      // atomic — row index for insert uniqueness
}

// crudSample is a shared pool of sampled filter-column tuples.
type crudSample struct {
	mu   sync.RWMutex
	rows [][]any
}

func (s *crudSample) set(rows [][]any) { s.mu.Lock(); s.rows = rows; s.mu.Unlock() }

func (s *crudSample) pick(rng *rand.Rand) []any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.rows) == 0 {
		return nil
	}
	return s.rows[rng.Intn(len(s.rows))]
}

// buildCRUDPlan introspects the target table (reusing tableMeta) and assembles the statements.
func (run *benchRun) buildCRUDPlan(ctx context.Context) (*crudPlan, error) {
	// Introspect via the Data Generator's admin connection (root/postgres over the container
	// CLI) — the benchmark's own workload user may not be authorised for the CLI/socket path.
	st, err := run.app.store.GetStack(run.cfg.StackID)
	if err != nil {
		return nil, fmt.Errorf("stack not found")
	}
	c, ok := run.app.dbConnFor(st, run.cfg.NodeID)
	if !ok {
		return nil, fmt.Errorf("node is not running")
	}
	schema := strings.TrimSpace(run.cfg.Schema)
	if run.engine == "mysql" {
		schema = run.cfg.Database
	} else if schema == "" {
		schema = "public"
	}
	meta, err := run.app.tableMeta(ctx, c, run.cfg.Database, schema, run.cfg.Table)
	if err != nil {
		return nil, fmt.Errorf("introspect table: %w", err)
	}
	byName := map[string]dgColumn{}
	for _, col := range meta.Columns {
		byName[col.Name] = col
	}

	// Filter columns: the user's choice, else the primary key.
	var filter []dgColumn
	if len(run.cfg.FilterColumns) > 0 {
		for _, name := range run.cfg.FilterColumns {
			col, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("filter column %q not found in table", name)
			}
			filter = append(filter, col)
		}
	} else {
		for _, col := range meta.Columns {
			if col.IsPrimaryKey {
				filter = append(filter, col)
			}
		}
	}
	if len(filter) == 0 {
		return nil, fmt.Errorf("table has no primary key — pick one or more filter columns for CRUD")
	}

	gens := resolveColGens(meta, dgGenConfig{})
	if len(gens) == 0 {
		return nil, fmt.Errorf("table has no insertable columns (all identity/generated/default)")
	}
	filterSet := map[string]bool{}
	for _, f := range filter {
		filterSet[f.Name] = true
	}
	var updatable []dgColumn
	for _, g := range gens {
		if !filterSet[g.col.Name] {
			updatable = append(updatable, g.col)
		}
	}

	cols := make([]string, len(gens))
	for i, g := range gens {
		cols[i] = qIdent(run.engine, g.col.Name)
	}
	fcols := make([]string, len(filter))
	for i, f := range filter {
		fcols[i] = qIdent(run.engine, f.Name)
	}

	p := &crudPlan{
		tableRef:     qIdent(run.engine, schema) + "." + qIdent(run.engine, run.cfg.Table),
		insertVerb:   "INSERT INTO ",
		insertCols:   "(" + strings.Join(cols, ",") + ")",
		insertGens:   gens,
		conflict:     " ON CONFLICT DO NOTHING",
		filterCols:   fcols,
		filterSelect: strings.Join(fcols, ","),
		filterN:      len(fcols),
		updateCols:   updatable,
		nonce:        qrNewID()[:8],
	}
	if run.engine == "mysql" {
		p.insertVerb, p.conflict = "INSERT IGNORE INTO ", ""
	}
	return p, nil
}

// crudRefreshSample repopulates the PK/filter pool from a random sample of the table.
func (run *benchRun) crudRefreshSample(ctx context.Context, db *sql.DB) {
	p := run.crud
	order := "RANDOM()"
	if run.engine == "mysql" {
		order = "RAND()"
	}
	q := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT %d", p.filterSelect, p.tableRef, order, crudSampleSize)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, p.filterN)
		ptrs := make([]any, p.filterN)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			return
		}
		out = append(out, vals)
	}
	if rows.Err() == nil {
		run.sample.set(out)
	}
}

// crudSampler refreshes the pool every few seconds until the context ends.
func (run *benchRun) crudSampler(ctx context.Context, db *sql.DB) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run.crudRefreshSample(ctx, db)
		}
	}
}

// crudPickOp chooses insert/update/delete/select by the configured weights.
func (run *benchRun) crudPickOp(rng *rand.Rand) string {
	w := run.cfg.Weights
	names := []string{"insert", "update", "delete", "select"}
	wts := []int{w.Insert, w.Update, w.Delete, w.Select}
	sum := 0
	for i := range wts {
		if wts[i] < 0 {
			wts[i] = 0
		}
		sum += wts[i]
	}
	if sum == 0 {
		return names[rng.Intn(4)]
	}
	r := rng.Intn(sum)
	for i, wt := range wts {
		if r < wt {
			return names[i]
		}
		r -= wt
	}
	return "select"
}

// crudWhere builds a WHERE from a random non-empty subset of the filter columns, using the
// sampled tuple's values as SQL literals (avoids driver placeholder pitfalls).
func (run *benchRun) crudWhere(rng *rand.Rand, tuple []any) string {
	p := run.crud
	idx := rng.Perm(p.filterN)[:1+rng.Intn(p.filterN)]
	parts := make([]string, len(idx))
	for i, j := range idx {
		parts[i] = p.filterCols[j] + " = " + crudLit(tuple[j])
	}
	return strings.Join(parts, " AND ")
}

func (run *benchRun) unitCRUD(ctx context.Context, db *sql.DB, rng *rand.Rand, record bool) error {
	switch run.crudPickOp(rng) {
	case "insert":
		return run.crudInsert(ctx, db, rng, record)
	case "update":
		return run.crudUpdate(ctx, db, rng, record)
	case "delete":
		return run.crudDelete(ctx, db, rng, record)
	default:
		return run.crudSelect(ctx, db, rng, record)
	}
}

func (run *benchRun) crudInsert(ctx context.Context, db *sql.DB, rng *rand.Rand, record bool) error {
	p := run.crud
	gc := &genCtx{rng: rng, engine: run.engine, uniq: p.nonce, row: atomic.AddInt64(&p.rowSeq, 1)}
	vals := make([]string, len(p.insertGens))
	for i, g := range p.insertGens {
		vals[i] = g.value(gc)
	}
	q := p.insertVerb + p.tableRef + " " + p.insertCols + " VALUES (" + strings.Join(vals, ",") + ")" + p.conflict
	return run.te(ctx, db, "insert", record, q)
}

func (run *benchRun) crudSelect(ctx context.Context, db *sql.DB, rng *rand.Rand, record bool) error {
	tuple := run.sample.pick(rng)
	if tuple == nil {
		return nil
	}
	return run.tq(ctx, db, "point_select", record, "SELECT * FROM "+run.crud.tableRef+" WHERE "+run.crudWhere(rng, tuple))
}

func (run *benchRun) crudDelete(ctx context.Context, db *sql.DB, rng *rand.Rand, record bool) error {
	tuple := run.sample.pick(rng)
	if tuple == nil {
		return nil
	}
	return run.te(ctx, db, "delete", record, "DELETE FROM "+run.crud.tableRef+" WHERE "+run.crudWhere(rng, tuple))
}

func (run *benchRun) crudUpdate(ctx context.Context, db *sql.DB, rng *rand.Rand, record bool) error {
	p := run.crud
	if len(p.updateCols) == 0 {
		return run.crudSelect(ctx, db, rng, record) // nothing to set → exercise a read instead
	}
	tuple := run.sample.pick(rng)
	if tuple == nil {
		return nil
	}
	col := p.updateCols[rng.Intn(len(p.updateCols))]
	gc := &genCtx{rng: rng, engine: run.engine, uniq: p.nonce, row: atomic.AddInt64(&p.rowSeq, 1)}
	lit := colGen{col: col, gen: col.Generator, opts: map[string]any{}}.value(gc)
	q := "UPDATE " + p.tableRef + " SET " + qIdent(run.engine, col.Name) + " = " + lit + " WHERE " + run.crudWhere(rng, tuple)
	return run.te(ctx, db, "update", record, q)
}

// crudLit formats a scanned value as a SQL literal for both engines.
func crudLit(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	case []byte:
		return sqlLit(string(x))
	case string:
		return sqlLit(x)
	case time.Time:
		return sqlLit(x.Format("2006-01-02 15:04:05.999999"))
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'f', -1, 64)
	}
	return sqlLit(fmt.Sprintf("%v", v))
}
