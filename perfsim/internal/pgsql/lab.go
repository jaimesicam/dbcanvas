package pgsql

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"perfsim/internal/core"
)

// Stressors that make the server show a condition its own statistics report:
// many tables (relation cache and file handles), temporary files
// (pg_stat_database.temp_files), full scans (seq_tup_read) and WAL pressure
// (pg_current_wal_lsn). None of them needs a standby.

// tempWritten finds a plan node that wrote temporary blocks, or a sort on disk.
var tempWritten = regexp.MustCompile(`"Temp Written Blocks": [1-9]|"Sort Space Type": "Disk"`)

// extraTablePrefix names the many-tables stressor's tables; nothing else in the
// sim uses it, so a name with it is always one of these.
const extraTablePrefix = "many_"

func (e *Engine) labDials() []core.Dial {
	return []core.Dial{
		{ID: "tables", Group: groupMem, Name: "Many tables", Kind: "toggle", Available: true,
			Desc:  "Creates small tables up to the number chosen, then keeps reading one row from each in turn. Each backend caches what it has opened, and every table is a file and catalog entries, so the server's memory per backend and its file handles grow with them.",
			Watch: []string{"p99 latency"},
			Params: []core.Param{
				{Key: "count", Label: "Tables", Type: "steps", Opts: []any{500, 1000, 2000, 5000}, Def: 1000},
				{Key: "after", Label: "When it stops", Type: "pills", Opts: []any{"keep the tables", "drop them"}, Def: "keep the tables"},
			},
			Note: "Creating thousands of tables takes a while the first time; kept, they make the next start quick.",
			SQL:  "CREATE TABLE many_00001 (...);  -- up to {count}\nSELECT id FROM many_NNNNN LIMIT 1;  -- each in turn, about 100 a second"},
		{ID: "temptables", Group: groupMem, Name: "Temporary files", Kind: "toggle", Available: true,
			Desc:  "A rollup that sorts and aggregates every group of orders. On disk, the session's work_mem is shrunk so the sort spills to temporary files every time; in memory, it is raised so it never does.",
			Watch: []string{"p99 latency", "Reads outside shared_buffers"},
			Params: []core.Param{
				{Key: "mode", Label: "Sort", Type: "pills", Opts: []any{"disk", "memory"}, Def: "disk"},
				{Key: "every", Label: "One every", Type: "pills", Opts: []any{1, 3, 10}, Def: 3, Unit: "s"},
			},
			SQL: "SET work_mem = '64kB';  -- disk\nSELECT customer, status, count(*), sum(amount) FROM orders\n GROUP BY customer, status ORDER BY count(*) DESC, sum(amount) DESC LIMIT 5000;"},
		{ID: "fullscan", Group: groupIO, Name: "Full table scans", Kind: "toggle", NeedsTable: "any", Available: true,
			Desc:   "Queries with a predicate on a column that has no index, so the only plan is a sequential scan of a prepared table. They raise seq_tup_read, fill the slow query log, and use ring buffers that still push reads out to the operating system.",
			Watch:  []string{"Reads outside shared_buffers", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Concurrent scans", Type: "steps", Opts: []any{1, 2, 4, 8}, Def: 2}},
			SQL:    "SELECT count(*) FROM {table} WHERE note BETWEEN $1 AND $2;  -- note has no index"},
		{ID: "redo", Group: groupIO, Name: "WAL pressure", Kind: "toggle", Available: true,
			Desc:   "Transactions that each rewrite many rows of orders, back to back. Every new row version is written to the WAL, with full pages after a checkpoint, so WAL volume, checkpoints and the background writer all rise.",
			Watch:  []string{"Write QPS", "p99 latency"},
			Params: []core.Param{{Key: "rows", Label: "Rows per transaction", Type: "steps", Opts: []any{1000, 10000, 50000}, Def: 10000, Unit: "rows"}},
			SQL:    "UPDATE orders SET note = note + 1 WHERE id BETWEEN $1 AND $1 + {rows};  -- back to back"},
	}
}

func (e *Engine) startLab(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	var run func(context.Context, *core.Job)
	switch id {
	case "tables":
		run = e.runManyTables
	case "temptables":
		run = e.runTempFiles
	case "fullscan":
		run = func(ctx context.Context, j *core.Job) { e.runFullScans(ctx, j, table) }
	case "redo":
		run = e.runWAL
	default:
		return nil, false
	}
	return core.NewJob(ctx, p, run), true
}

// statRate reports a server figure's rate on the dial once a second, after
// phase and a note of the dial's own. read returns the figure and whether it
// could be read.
func statRate(ctx context.Context, j *core.Job, read func() (float64, bool), what string, scale float64, phase func() (string, string)) {
	last, ok := read()
	t := time.Now()
	for ctx.Err() == nil {
		sleep(ctx, time.Second)
		now, ok2 := read()
		p, note := phase()
		if ok && ok2 {
			note = fmt.Sprintf("%.1f %s · %s", (now-last)/scale/time.Since(t).Seconds(), what, note)
		}
		j.Set(p, -1, note)
		last, ok, t = now, ok2, time.Now()
	}
}

func queryFloat(ctx context.Context, d *sql.DB, q string) func() (float64, bool) {
	return func() (float64, bool) {
		var v float64
		return v, d.QueryRowContext(ctx, q).Scan(&v) == nil
	}
}

func extraTableName(i int) string { return fmt.Sprintf("%s%05d", extraTablePrefix, i) }

func (e *Engine) runManyTables(ctx context.Context, j *core.Job) {
	want := j.Param().Int("count", 1000)
	d := e.wdb()
	have := map[string]bool{}
	if rows, err := d.QueryContext(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' AND c.relname LIKE 'many\_%'`, e.schema()); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				have[n] = true
			}
		}
		rows.Close()
	}
	for i := 1; i <= want && ctx.Err() == nil; i++ {
		name := extraTableName(i)
		if have[name] {
			continue
		}
		if _, err := d.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+name+" (id int PRIMARY KEY, v int NOT NULL)"); err != nil {
			j.Fail(err)
			return
		}
		d.ExecContext(ctx, "INSERT INTO "+name+" VALUES (1, 1), (2, 2) ON CONFLICT DO NOTHING")
		if i%50 == 0 {
			j.Set("Creating tables", float64(i)/float64(want), fmt.Sprintf("%d of %d", i, want))
		}
	}
	j.OnRelease(func(context.Context) error {
		if j.Param().Str("after", "keep the tables") != "drop them" {
			return nil
		}
		// Dropping thousands of tables outlasts a release; it carries on alone.
		go func() {
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			for i := 1; i <= 5000; i++ {
				d.ExecContext(dctx, "DROP TABLE IF EXISTS "+extraTableName(i))
			}
		}()
		return nil
	})
	var touched atomic.Int64
	go j.RateNote(ctx, fmt.Sprintf("Reading %d tables in turn", want), &touched, "table reads/s")
	// Evenly, about 100 a second, so the rate reads steadily.
	for next := 0; ctx.Err() == nil; {
		var id int
		d.QueryRowContext(ctx, "SELECT id FROM "+extraTableName(next%want+1)+" LIMIT 1").Scan(&id)
		touched.Add(1)
		next++
		sleep(ctx, 10*time.Millisecond)
	}
}

func (e *Engine) runTempFiles(ctx context.Context, j *core.Job) {
	d := e.wdb()
	var runs, spilled atomic.Int64
	go func() {
		for ctx.Err() == nil {
			j.Set(fmt.Sprintf("Rollup every %d s, sort in %s", j.Param().Int("every", 3), j.Param().Str("mode", "disk")), -1,
				fmt.Sprintf("%d queries · %d wrote temporary files", runs.Load(), spilled.Load()))
			sleep(ctx, time.Second)
		}
	}()
	for ctx.Err() == nil {
		p := j.Param()
		mem := "256MB"
		if p.Str("mode", "disk") == "disk" {
			mem = "64kB"
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		tx.ExecContext(ctx, "SET LOCAL work_mem = '"+mem+"'")
		// EXPLAIN ANALYZE runs the rollup in full and says whether it wrote
		// temporary blocks; the database-wide counter is updated too late to
		// tell one query from the next.
		var plan string
		if tx.QueryRowContext(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT customer, status, count(*), sum(amount) FROM orders
			GROUP BY customer, status ORDER BY count(*) DESC, sum(amount) DESC LIMIT 5000`).Scan(&plan) == nil {
			runs.Add(1)
			if tempWritten.MatchString(plan) {
				spilled.Add(1)
			}
		}
		tx.Rollback()
		sleep(ctx, time.Duration(p.Int("every", 3))*time.Second)
	}
}

func (e *Engine) runFullScans(ctx context.Context, j *core.Job, table string) {
	d := e.wdb()
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				c, pid, err := connPID(ctx, d)
				if err != nil {
					sleep(ctx, time.Second)
					continue
				}
				lo := rand.IntN(1000)
				if execCancelable(ctx, d, c, pid, "SELECT count(*) FROM "+table+" WHERE note BETWEEN $1 AND $2", lo, lo+10) == nil {
					n.Add(1)
				}
				c.Close()
			}
		}()
	}
	seq := queryFloat(ctx, d, "SELECT COALESCE(sum(seq_tup_read), 0)::float8 FROM pg_stat_user_tables")
	go statRate(ctx, j, seq, "million rows read by scans/s", 1e6, func() (string, string) {
		return fmt.Sprintf("%d scans at a time over %s", workers, table), fmt.Sprintf("%d scans done", n.Load())
	})
	wg.Wait()
}

func (e *Engine) runWAL(ctx context.Context, j *core.Job) {
	d := e.wdb()
	var txns atomic.Int64
	wal := queryFloat(ctx, d, "SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')::float8")
	go statRate(ctx, j, wal, "MB of WAL/s", 1<<20, func() (string, string) {
		return fmt.Sprintf("Rewriting %d rows a transaction", j.Param().Int("rows", 10000)), fmt.Sprintf("%d transactions", txns.Load())
	})
	for ctx.Err() == nil {
		rows := j.Param().Int("rows", 10000)
		lo := 1 + rand.IntN(max(1, ordersRows-rows))
		c, pid, err := connPID(ctx, d)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		if execCancelable(ctx, d, c, pid, "UPDATE orders SET note = note + 1 WHERE id BETWEEN $1 AND $2", lo, lo+rows-1) == nil {
			txns.Add(1)
		} else {
			sleep(ctx, time.Second)
		}
		c.Close()
	}
}
