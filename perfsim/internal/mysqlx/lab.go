package mysqlx

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"perfsim/internal/core"
)

// Stressors that make the server show a condition its own counters report:
// many tables (table_open_cache), temporary tables (Created_tmp_disk_tables),
// full scans (Handler_read_rnd_next) and redo pressure (Innodb_os_log_written).
// None of them needs a replica.

// extraTablePrefix names the many-tables stressor's tables; nothing else in the
// sim uses it, so a name with it is always one of these.
const extraTablePrefix = "many_"

func (e *Engine) labDials() []core.Dial {
	return []core.Dial{
		{ID: "tables", Group: groupMem, Name: "Many tables", Kind: "toggle", Available: true,
			Desc:  "Creates small tables up to the number chosen, then keeps reading one row from each in turn. Every table read needs a handle in table_open_cache; touching more than the cache holds makes Opened_tables climb and cache misses appear.",
			Watch: []string{"p99 latency"},
			Params: []core.Param{
				{Key: "count", Label: "Tables", Type: "steps", Opts: []any{500, 1000, 2000, 5000}, Def: 1000},
				{Key: "after", Label: "When it stops", Type: "pills", Opts: []any{"keep the tables", "drop them"}, Def: "keep the tables"},
			},
			Note: "Creating thousands of tables takes a while the first time; kept, they make the next start quick.",
			SQL:  "CREATE TABLE perfsim.many_00001 (...);  -- up to {count}\nSELECT id FROM perfsim.many_NNNNN LIMIT 1;  -- each in turn, about 100 a second"},
		{ID: "temptables", Group: groupMem, Name: "Temporary tables", Kind: "toggle", Available: true,
			Desc:  "A rollup that cannot use an index: the server materialises every group before sorting it, in a temporary table. On disk, the session's tmp_table_size is shrunk so it spills every time; in memory, it is raised so it never does.",
			Watch: []string{"p99 latency", "Reads from disk"},
			Params: []core.Param{
				{Key: "mode", Label: "Temporary table", Type: "pills", Opts: []any{"disk", "memory"}, Def: "disk"},
				{Key: "every", Label: "One every", Type: "pills", Opts: []any{1, 3, 10}, Def: 3, Unit: "s"},
			},
			SQL: "SET SESSION tmp_table_size = 16384, max_heap_table_size = 16384;  -- disk\nSELECT customer, status, COUNT(*), SUM(amount) FROM perfsim.orders\n GROUP BY customer, status ORDER BY COUNT(*) DESC, SUM(amount) DESC LIMIT 5000;"},
		{ID: "fullscan", Group: groupIO, Name: "Full table scans", Kind: "toggle", NeedsTable: "any", Available: true,
			Desc:   "Queries with a predicate on a column that has no index, so the only plan is to read every row of a prepared table. They fill the slow query log and Handler_read_rnd_next, and push the workload's pages out of the buffer pool.",
			Watch:  []string{"Reads from disk", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Concurrent scans", Type: "steps", Opts: []any{1, 2, 4, 8}, Def: 2}},
			SQL:    "SELECT COUNT(*) FROM perfsim.{table} WHERE note BETWEEN ? AND ?;  -- note has no index"},
		{ID: "redo", Group: groupIO, Name: "Redo pressure", Kind: "toggle", Available: true,
			Desc:   "Transactions that each rewrite many rows of orders, back to back. They fill the redo log and dirty pages faster than the workload alone, so checkpoints and log flushes have to keep up.",
			Watch:  []string{"Write QPS", "p99 latency"},
			Params: []core.Param{{Key: "rows", Label: "Rows per transaction", Type: "steps", Opts: []any{1000, 10000, 50000}, Def: 10000, Unit: "rows"}},
			SQL:    "UPDATE perfsim.orders SET note = note + 1 WHERE id BETWEEN ? AND ? + {rows};  -- back to back"},
	}
}

func (e *Engine) startLab(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	var run func(context.Context, *core.Job)
	switch id {
	case "tables":
		run = e.runManyTables
	case "temptables":
		run = e.runTempTables
	case "fullscan":
		run = func(ctx context.Context, j *core.Job) { e.runFullScans(ctx, j, table) }
	case "redo":
		run = e.runRedo
	default:
		return nil, false
	}
	return core.NewJob(ctx, p, run), true
}

// globalCounter reads one global status counter.
func globalCounter(ctx context.Context, db *sql.DB, name string) (int64, bool) {
	var k, v string
	if db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(&k, &v) != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// counterRate reports a global counter's rate on the dial once a second,
// after phase and a note of the dial's own.
func counterRate(ctx context.Context, j *core.Job, db *sql.DB, name, what string, scale float64, phase func() (string, string)) {
	last, ok := globalCounter(ctx, db, name)
	t := time.Now()
	for ctx.Err() == nil {
		sleep(ctx, time.Second)
		now, ok2 := globalCounter(ctx, db, name)
		p, note := phase()
		if ok && ok2 {
			note = fmt.Sprintf("%.1f %s · %s", float64(now-last)/scale/time.Since(t).Seconds(), what, note)
		}
		j.Set(p, -1, note)
		last, ok, t = now, ok2, time.Now()
	}
}

func extraTableName(i int) string { return fmt.Sprintf("%s%05d", extraTablePrefix, i) }

func (e *Engine) runManyTables(ctx context.Context, j *core.Job) {
	want := j.Param().Int("count", 1000)
	db := e.wdb()
	have := map[string]bool{}
	if rows, err := db.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = 'perfsim' AND TABLE_NAME LIKE 'many\_%'`); err == nil {
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
		if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS perfsim."+name+
			" (id INT NOT NULL PRIMARY KEY, v INT NOT NULL) ENGINE=InnoDB"); err != nil {
			j.Fail(err)
			return
		}
		db.ExecContext(ctx, "INSERT IGNORE INTO perfsim."+name+" VALUES (1, 1), (2, 2)")
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
				db.ExecContext(dctx, "DROP TABLE IF EXISTS perfsim."+extraTableName(i))
			}
		}()
		return nil
	})
	var touched atomic.Int64
	go counterRate(ctx, j, db, "Opened_tables", "tables opened/s", 1, func() (string, string) {
		return fmt.Sprintf("Reading %d tables in turn", want), fmt.Sprintf("%d table reads", touched.Load())
	})
	// Evenly, about 100 a second, so the rate reads steadily.
	for next := 0; ctx.Err() == nil; {
		var id int
		db.QueryRowContext(ctx, "SELECT id FROM perfsim."+extraTableName(next%want+1)+" LIMIT 1").Scan(&id)
		touched.Add(1)
		next++
		sleep(ctx, 10*time.Millisecond)
	}
}

func (e *Engine) runTempTables(ctx context.Context, j *core.Job) {
	db := e.wdb()
	var runs, spilled atomic.Int64
	go func() {
		for ctx.Err() == nil {
			j.Set(fmt.Sprintf("Rollup every %d s, temporary table on %s", j.Param().Int("every", 3), j.Param().Str("mode", "disk")), -1,
				fmt.Sprintf("%d queries · %d spilled to disk", runs.Load(), spilled.Load()))
			sleep(ctx, time.Second)
		}
	}()
	for ctx.Err() == nil {
		p := j.Param()
		c, err := db.Conn(ctx)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		size := "268435456"
		if p.Str("mode", "disk") == "disk" {
			size = "16384"
		}
		c.ExecContext(ctx, "SET SESSION tmp_table_size = "+size+", max_heap_table_size = "+size)
		var k, before, after string
		c.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Created_tmp_disk_tables'").Scan(&k, &before)
		rows, err := c.QueryContext(ctx, `SELECT customer, status, COUNT(*), SUM(amount) FROM perfsim.orders
			GROUP BY customer, status ORDER BY COUNT(*) DESC, SUM(amount) DESC LIMIT 5000`)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
			runs.Add(1)
		}
		c.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Created_tmp_disk_tables'").Scan(&k, &after)
		if after != before {
			spilled.Add(1)
		}
		c.Close()
		sleep(ctx, time.Duration(p.Int("every", 3))*time.Second)
	}
}

func (e *Engine) runFullScans(ctx context.Context, j *core.Job, table string) {
	db := e.wdb()
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				c, cid, err := connID(ctx, db)
				if err != nil {
					sleep(ctx, time.Second)
					continue
				}
				lo := rand.IntN(1000)
				if execKillable(ctx, db, c, cid, "SELECT COUNT(*) FROM perfsim.`"+table+"` WHERE note BETWEEN ? AND ?", lo, lo+10) == nil {
					n.Add(1)
				}
				c.Close()
			}
		}()
	}
	go counterRate(ctx, j, db, "Handler_read_rnd_next", "million rows read by scans/s", 1e6, func() (string, string) {
		return fmt.Sprintf("%d scans at a time over perfsim.%s", workers, table), fmt.Sprintf("%d scans done", n.Load())
	})
	wg.Wait()
}

func (e *Engine) runRedo(ctx context.Context, j *core.Job) {
	db := e.wdb()
	var txns atomic.Int64
	go counterRate(ctx, j, db, "Innodb_os_log_written", "MB of redo/s", 1<<20, func() (string, string) {
		return fmt.Sprintf("Rewriting %d rows a transaction", j.Param().Int("rows", 10000)), fmt.Sprintf("%d transactions", txns.Load())
	})
	for ctx.Err() == nil {
		rows := j.Param().Int("rows", 10000)
		lo := 1 + rand.IntN(max(1, ordersRows-rows))
		c, cid, err := connID(ctx, db)
		if err != nil {
			sleep(ctx, time.Second)
			continue
		}
		if execKillable(ctx, db, c, cid, "UPDATE perfsim.orders SET note = note + 1 WHERE id BETWEEN ? AND ?", lo, lo+rows-1) == nil {
			txns.Add(1)
		} else {
			sleep(ctx, time.Second)
		}
		c.Close()
	}
}
