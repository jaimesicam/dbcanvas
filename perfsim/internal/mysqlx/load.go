package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"perfsim/internal/core"
)

// The stressors beyond replication lag: connections, locks, I/O, CPU and
// memory. They act on the writer, through the sim's own tables, and each one
// stops what it started when it is turned off.

const (
	groupConns = "Connections"
	groupLocks = "Locks"
	groupIO    = "I/O and CPU"
	groupMem   = "Memory"
)

func (e *Engine) loadDials() []core.Dial {
	return []core.Dial{
		{ID: "storm", Group: groupConns, Name: "Connection storm", Kind: "toggle", Available: true,
			Desc:  "Opens connections to the writer and holds them idle, up to the number chosen. Past max_connections the server refuses new ones, the application's included.",
			Watch: []string{"Connections", "Errors"},
			Params: []core.Param{{Key: "conns", Label: "Connections", Type: "steps", Opts: []any{100, 250, 500, 1000, 2000}, Def: 250, Unit: ""},
				{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300}, Def: 120, Unit: "s"}},
			SQL: "-- {conns} connections, each idle after connecting"},
		{ID: "hotrow", Group: groupLocks, Name: "Hot-row contention", Kind: "toggle", Available: true,
			Desc:  "Workers update the same few rows in short transactions, each holding its row lock for a moment. They queue for the locks; with deadlocks on, they take two rows in opposite orders.",
			Watch: []string{"Lock waits", "p99 latency"},
			Params: []core.Param{
				{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{4, 16, 32, 64}, Def: 16, Unit: ""},
				{Key: "rows", Label: "Rows they share", Type: "pills", Opts: []any{1, 4, 16}, Def: 4, Unit: ""},
				{Key: "deadlocks", Label: "Deadlocks", Type: "pills", Opts: []any{"off", "on"}, Def: "off"},
			},
			SQL: "START TRANSACTION;\nSELECT n FROM perfsim.hot WHERE id = ? FOR UPDATE;  -- held 20 ms\nUPDATE perfsim.hot SET n = n + 1 WHERE id = ?;\nCOMMIT;"},
		{ID: "longtxn", Group: groupLocks, Name: "Long-running transaction", Kind: "toggle", Available: true,
			Desc:   "Opens a transaction with a consistent snapshot and keeps it open. Purge cannot remove any undo newer than that snapshot, so the history list grows with every write the workload makes.",
			Watch:  []string{"History list length"},
			Params: []core.Param{{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{60, 300, 600, 1800}, Def: 300, Unit: "s"}},
			SQL:    "START TRANSACTION WITH CONSISTENT SNAPSHOT;\nSELECT COUNT(*) FROM perfsim.orders;\n-- left open up to {hold} s, then ROLLBACK"},
		{ID: "commits", Group: groupIO, Name: "Commit storm", Kind: "toggle", Available: true,
			Desc:   "Many single-row transactions from many workers. With innodb_flush_log_at_trx_commit = 1 every commit waits for a log flush, so the flush rate becomes the limit.",
			Watch:  []string{"Write QPS", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{8, 32, 64, 128}, Def: 32, Unit: ""}},
			SQL:    "INSERT INTO perfsim.commits (id, n) VALUES (?, 1)\n  ON DUPLICATE KEY UPDATE n = n + 1;  -- autocommit, from every worker"},
		{ID: "cpu", Group: groupIO, Name: "CPU-heavy queries", Kind: "toggle", Available: true,
			Desc:   "Queries that hash in a loop and touch no data. They keep threads running and compete for CPU with everything else on the server.",
			Watch:  []string{"Threads running", "p50 latency"},
			Params: []core.Param{{Key: "workers", Label: "Concurrent queries", Type: "steps", Opts: []any{1, 4, 8, 16}, Def: 4, Unit: ""}},
			SQL:    "SELECT BENCHMARK(500000, SHA2(RAND(), 512));  -- back to back, from each worker"},
		{ID: "scan", Group: groupMem, Name: "Working set larger than memory", Kind: "toggle", NeedsTable: "pk", Available: true,
			Desc:  "Random point reads across a whole prepared table. When the table is larger than the buffer pool, most of them miss it and read from disk, and pages the workload needs are pushed out.",
			Watch: []string{"Reads from disk", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Readers", Type: "steps", Opts: []any{4, 16, 32}, Def: 16},
				{Key: "share", Label: "Working set", Type: "pills", Opts: []any{10, 25, 50, 100}, Def: 100, Unit: "%"}},
			Note: "Prepare a table bigger than innodb_buffer_pool_size for the effect; the dial says how they compare when it starts.",
			SQL:  "SELECT * FROM perfsim.{table} WHERE id = ?;  -- a random id each time"},
	}
}

// loadInit creates the tables the stressors use.
func (e *Engine) loadInit(ctx context.Context) error {
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS perfsim.hot (id INT NOT NULL PRIMARY KEY, n BIGINT NOT NULL DEFAULT 0) ENGINE=InnoDB`,
		`INSERT IGNORE INTO perfsim.hot (id) SELECT n + 1 FROM perfsim.seq WHERE n < 16`,
		`CREATE TABLE IF NOT EXISTS perfsim.commits (id INT NOT NULL PRIMARY KEY, n BIGINT NOT NULL DEFAULT 0) ENGINE=InnoDB`,
	} {
		if _, err := e.wdb().ExecContext(ctx, s); err != nil {
			return fmt.Errorf("creating the stressors' tables: %w", err)
		}
	}
	return nil
}

func (e *Engine) startLoad(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	switch id {
	case "storm":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runStorm(ctx, j) }), true
	case "hotrow":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runHotRows(ctx, j) }), true
	case "longtxn":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runLongTxn(ctx, j) }), true
	case "commits":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runCommits(ctx, j) }), true
	case "cpu":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runCPU(ctx, j) }), true
	case "scan":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runScan(ctx, j, table) }), true
	}
	return nil, false
}

// runStorm opens connections one by one and holds them. They are plain
// connections, not statements, so closing them is the whole release.
func (e *Engine) runStorm(ctx context.Context, j *core.Job) {
	want := j.Param().Int("conns", 250)
	wu, wp := e.cfg.User, e.cfg.Password
	if e.cfg.WriterUser != "" {
		wu, wp = e.cfg.WriterUser, e.cfg.WriterPassword
	}
	_, wid := e.writerDB()
	host, port := e.cfg.Writer.Host, e.cfg.Writer.Port
	pool := open(wu, wp, host, port, want, e.cfg.TLS)
	defer pool.Close()
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	refused := 0
	var lastErr string
	for len(conns) < want && ctx.Err() == nil {
		c, err := pool.Conn(ctx)
		if err == nil {
			err = c.PingContext(ctx)
		}
		if err != nil {
			if c != nil {
				c.Close()
			}
			refused++
			lastErr = err.Error()
			j.Set(fmt.Sprintf("%d of %d connections open", len(conns), want), float64(len(conns))/float64(want), "refused: "+short(lastErr, 80))
			sleep(ctx, 500*time.Millisecond)
			continue
		}
		conns = append(conns, c)
		if len(conns)%25 == 0 {
			j.Set(fmt.Sprintf("Opening connections to %s", wid), float64(len(conns))/float64(want), fmt.Sprintf("%d of %d", len(conns), want))
		}
	}
	note := ""
	if refused > 0 {
		note = fmt.Sprintf("%d refused, last: %s", refused, short(lastErr, 80))
	}
	j.Set(fmt.Sprintf("Holding %d connections", len(conns)), -1, note)
	<-ctx.Done()
}

func (e *Engine) runHotRows(ctx context.Context, j *core.Job) {
	var commits, waits, deadlocks atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 16)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for ctx.Err() == nil {
				p := j.Param()
				rows := p.Int("rows", 4)
				a := rand.IntN(rows) + 1
				ids := []int{a}
				if p.Str("deadlocks", "off") == "on" && rows > 1 {
					b := a%rows + 1
					if w%2 == 1 {
						a, b = b, a
					}
					ids = []int{a, b}
				}
				err := e.hotTxn(ctx, ids)
				var me *mysql.MySQLError
				switch {
				case err == nil:
					commits.Add(1)
				case errors.As(err, &me) && me.Number == 1213:
					deadlocks.Add(1)
					sleep(ctx, 100*time.Millisecond)
				case errors.As(err, &me) && me.Number == 1205:
					waits.Add(1)
				}
			}
		}(w)
	}
	go func() {
		for ctx.Err() == nil {
			j.Set(fmt.Sprintf("%d workers on %d rows", workers, j.Param().Int("rows", 4)), -1,
				fmt.Sprintf("%d commits · %d deadlocks · %d lock wait timeouts", commits.Load(), deadlocks.Load(), waits.Load()))
			sleep(ctx, time.Second)
		}
	}()
	wg.Wait()
}

func (e *Engine) hotTxn(ctx context.Context, ids []int) error {
	tx, err := e.wdb().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var n int64
		if err := tx.QueryRowContext(ctx, "SELECT n FROM perfsim.hot WHERE id = ? FOR UPDATE", id).Scan(&n); err != nil {
			return err
		}
		sleep(ctx, 20*time.Millisecond)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE perfsim.hot SET n = n + 1 WHERE id = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// runLongTxn holds a consistent snapshot open on the writer. Purge cannot pass
// it; ending the transaction (or the connection) is the release.
func (e *Engine) runLongTxn(ctx context.Context, j *core.Job) {
	d, wid := e.writerDB()
	c, cid, err := connID(ctx, d)
	if err != nil {
		j.Finish(err)
		return
	}
	j.Hold(wid, fmt.Sprint(cid))
	defer c.Close()
	if _, err := c.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT"); err != nil {
		j.Finish(err)
		return
	}
	var n int64
	c.QueryRowContext(ctx, "SELECT COUNT(*) FROM perfsim.orders").Scan(&n)
	j.Set("Snapshot held open on "+wid, -1, "watch the history list length on the Server internals chart")
	<-ctx.Done()
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c.ExecContext(rctx, "ROLLBACK")
	cancel()
}

func (e *Engine) runCommits(ctx context.Context, j *core.Job) {
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 32)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := e.wdb().ExecContext(ctx, "INSERT INTO perfsim.commits (id, n) VALUES (?, 1) ON DUPLICATE KEY UPDATE n = n + 1", rand.IntN(10000)); err == nil {
					n.Add(1)
				} else {
					sleep(ctx, 200*time.Millisecond)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d workers committing", workers), &n, "commits/s")
	wg.Wait()
}

func (e *Engine) runCPU(ctx context.Context, j *core.Job) {
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				d, _ := e.writerDB()
				c, cid, err := connID(ctx, d)
				if err != nil {
					sleep(ctx, time.Second)
					continue
				}
				if execKillable(ctx, d, c, cid, "SELECT BENCHMARK(500000, SHA2(RAND(), 512))") == nil {
					n.Add(1)
				}
				c.Close()
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d hashing queries at a time", workers), &n, "queries/s")
	wg.Wait()
}

func (e *Engine) runScan(ctx context.Context, j *core.Job, table string) {
	var maxID sql.NullInt64
	if err := e.wdb().QueryRowContext(ctx, "SELECT MAX(id) FROM perfsim.`"+table+"`").Scan(&maxID); err != nil || !maxID.Valid {
		j.Finish(fmt.Errorf("%s is empty or unreadable", table))
		return
	}
	// The dataset list measures the tablespace file; TABLES statistics can be
	// far behind on a table that was just prepared.
	var pool, size int64
	e.wdb().QueryRowContext(ctx, "SELECT @@innodb_buffer_pool_size").Scan(&pool)
	if ds, err := e.Datasets(ctx); err == nil {
		for _, d := range ds {
			if d.Name == table {
				size = d.Bytes
			}
		}
	}
	compare := fmt.Sprintf("table %s, buffer pool %s", fmtGB(size), fmtGB(pool))
	if size > 0 && size < pool {
		compare += ": the table fits, so reads will soon all hit memory"
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 16)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				var id int64
				if err := e.wdb().QueryRowContext(ctx, "SELECT id FROM perfsim.`"+table+"` WHERE id = ?", 1+rand.Int64N(max(1, maxID.Int64*int64(j.Param().Int("share", 100))/100))).Scan(&id); err == nil || errors.Is(err, sql.ErrNoRows) {
					n.Add(1)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d readers across perfsim.%s", workers, table), &n, "reads/s · "+compare)
	wg.Wait()
}

func fmtGB(b int64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MB", b>>20)
}
