package pgsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"perfsim/internal/core"
)

// The stressors beyond replication lag: connections, locks, I/O, CPU and
// memory. They act on the primary, through the sim's own tables, and each one
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
			Desc:  "Opens connections to the primary and holds them idle, up to the number chosen. Each is a backend process; past max_connections the server refuses new ones, the application's included.",
			Watch: []string{"Connections", "Errors"},
			Params: []core.Param{{Key: "conns", Label: "Connections", Type: "steps", Opts: []any{50, 100, 250, 500, 1000}, Def: 100},
				{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300}, Def: 120, Unit: "s"}},
			SQL: "-- {conns} connections, each idle after connecting"},
		{ID: "hotrow", Group: groupLocks, Name: "Hot-row contention", Kind: "toggle", Available: true,
			Desc:  "Workers update the same few rows in short transactions, each holding its row lock for a moment. They queue for the locks; with deadlocks on, they take two rows in opposite orders and the deadlock detector cancels one after deadlock_timeout.",
			Watch: []string{"Lock waits", "p99 latency"},
			Params: []core.Param{
				{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{4, 16, 32, 64}, Def: 16},
				{Key: "rows", Label: "Rows they share", Type: "pills", Opts: []any{1, 4, 16}, Def: 4},
				{Key: "deadlocks", Label: "Deadlocks", Type: "pills", Opts: []any{"off", "on"}, Def: "off"},
			},
			SQL: "BEGIN;\nSELECT n FROM hot WHERE id = $1 FOR UPDATE;  -- held 20 ms\nUPDATE hot SET n = n + 1 WHERE id = $1;\nCOMMIT;"},
		{ID: "longtxn", Group: groupLocks, Name: "Long-running transaction", Kind: "toggle", Available: true,
			Desc:   "Opens a repeatable-read transaction and keeps it open. Vacuum cannot remove any row version that snapshot might still see, so dead rows pile up as the workload updates orders; the snapshot's age in transactions climbs for as long as it is held.",
			Watch:  []string{"Oldest snapshot age"},
			Params: []core.Param{{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{60, 300, 600, 1800}, Def: 300, Unit: "s"}},
			SQL:    "BEGIN ISOLATION LEVEL REPEATABLE READ;\nSELECT count(*) FROM orders;\n-- left open up to {hold} s, then ROLLBACK"},
		{ID: "commits", Group: groupIO, Name: "Commit storm", Kind: "toggle", Available: true,
			Desc:   "Many single-row transactions from many workers. With synchronous_commit on, every commit waits for its WAL to be flushed, so the flush rate becomes the limit.",
			Watch:  []string{"Write QPS", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Workers", Type: "steps", Opts: []any{8, 32, 64, 128}, Def: 32}},
			SQL:    "INSERT INTO commits (id, n) VALUES ($1, 1)\n  ON CONFLICT (id) DO UPDATE SET n = commits.n + 1;  -- autocommit, from every worker"},
		{ID: "cpu", Group: groupIO, Name: "CPU-heavy queries", Kind: "toggle", Available: true,
			Desc:   "Queries that hash generated numbers and touch no table. They keep backends busy and compete for CPU with everything else on the server.",
			Watch:  []string{"Active sessions", "p50 latency"},
			Params: []core.Param{{Key: "workers", Label: "Concurrent queries", Type: "steps", Opts: []any{1, 4, 8, 16}, Def: 4}},
			SQL:    "SELECT count(md5(g::text)) FROM generate_series(1, 1000000) g;  -- back to back, from each worker"},
		{ID: "scan", Group: groupMem, Name: "Working set larger than memory", Kind: "toggle", NeedsTable: "pk", Available: true,
			Desc:   "Random point reads across a whole prepared table. When the table is larger than shared_buffers, most of them miss it and read the page from the operating system, and pages the workload needs are pushed out.",
			Watch:  []string{"Reads outside shared_buffers", "p99 latency"},
			Params: []core.Param{{Key: "workers", Label: "Readers", Type: "steps", Opts: []any{4, 16, 32}, Def: 16}},
			Note:   "Prepare a table bigger than shared_buffers for the effect; the dial says how they compare when it starts. A read that misses shared_buffers may still be served from the operating system's page cache.",
			SQL:    "SELECT * FROM {table} WHERE id = $1;  -- a random id each time"},
	}
}

// loadInit creates the tables the stressors use.
func (e *Engine) loadInit(ctx context.Context) error {
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS hot (id int PRIMARY KEY, n bigint NOT NULL DEFAULT 0)`,
		`INSERT INTO hot (id) SELECT g FROM generate_series(1, 16) g ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS commits (id int PRIMARY KEY, n bigint NOT NULL DEFAULT 0)`,
	} {
		if _, err := e.wdb().ExecContext(ctx, s); err != nil {
			return fmt.Errorf("creating the stressors' tables: %w", err)
		}
	}
	return nil
}

func (e *Engine) startLoad(ctx context.Context, id string, p core.Params, table string) (core.Running, bool) {
	var run func(context.Context, *core.Job)
	switch id {
	case "storm":
		run = e.runStorm
	case "hotrow":
		run = e.runHotRows
	case "longtxn":
		run = e.runLongTxn
	case "commits":
		run = e.runCommits
	case "cpu":
		run = e.runCPU
	case "scan":
		run = func(ctx context.Context, j *core.Job) { e.runScan(ctx, j, table) }
	default:
		return nil, false
	}
	return core.NewJob(ctx, p, run), true
}

// runStorm opens connections one by one and holds them. Closing them is the
// whole release.
func (e *Engine) runStorm(ctx context.Context, j *core.Job) {
	want := j.Param().Int("conns", 100)
	_, wid := e.writerDB()
	e.mu.Lock()
	host := e.writerHost
	e.mu.Unlock()
	pool := open(e.cfg.User, e.cfg.Password, host, e.cfg.Writer.Port, database, want)
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
			var pe *pgconn.PgError
			if errors.As(err, &pe) {
				lastErr = pe.Message
			}
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
	var commits, deadlocks atomic.Int64
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
				var pe *pgconn.PgError
				switch {
				case err == nil:
					commits.Add(1)
				case errors.As(err, &pe) && pe.Code == "40P01":
					deadlocks.Add(1)
					sleep(ctx, 100*time.Millisecond)
				case err != nil:
					sleep(ctx, 100*time.Millisecond)
				}
			}
		}(w)
	}
	go func() {
		for ctx.Err() == nil {
			j.Set(fmt.Sprintf("%d workers on %d rows", workers, j.Param().Int("rows", 4)), -1,
				fmt.Sprintf("%d commits · %d deadlocks", commits.Load(), deadlocks.Load()))
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
		if err := tx.QueryRowContext(ctx, "SELECT n FROM hot WHERE id = $1 FOR UPDATE", id).Scan(&n); err != nil {
			return err
		}
		sleep(ctx, 20*time.Millisecond)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE hot SET n = n + 1 WHERE id = $1", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// runLongTxn holds a snapshot open on the primary. Vacuum cannot pass it;
// ending the transaction (or the connection) is the release.
func (e *Engine) runLongTxn(ctx context.Context, j *core.Job) {
	d, wid := e.writerDB()
	c, pid, err := connPID(ctx, d)
	if err != nil {
		j.Finish(err)
		return
	}
	j.Hold(wid, fmt.Sprint(pid))
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		j.Finish(err)
		return
	}
	var n int64
	c.QueryRowContext(ctx, "SELECT count(*) FROM orders").Scan(&n)
	j.Set("Snapshot held open on "+wid, -1, "watch the oldest snapshot age on the Server internals chart")
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
				if _, err := e.wdb().ExecContext(ctx, "INSERT INTO commits (id, n) VALUES ($1, 1) ON CONFLICT (id) DO UPDATE SET n = commits.n + 1", rand.IntN(10000)); err == nil {
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
				c, pid, err := connPID(ctx, d)
				if err != nil {
					sleep(ctx, time.Second)
					continue
				}
				if execCancelable(ctx, d, c, pid, "SELECT count(md5(g::text)) FROM generate_series(1, 1000000) g") == nil {
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
	if err := e.wdb().QueryRowContext(ctx, "SELECT max(id) FROM "+table).Scan(&maxID); err != nil || !maxID.Valid {
		j.Finish(fmt.Errorf("%s is empty or unreadable", table))
		return
	}
	var size int64
	var buffers float64
	e.wdb().QueryRowContext(ctx, "SELECT pg_size_bytes(current_setting('shared_buffers'))::float8").Scan(&buffers)
	if ds, err := e.Datasets(ctx); err == nil {
		for _, d := range ds {
			if d.Name == table {
				size = d.Bytes
			}
		}
	}
	compare := fmt.Sprintf("table %s, shared_buffers %s", fmtBytes(float64(size)), fmtBytes(buffers))
	if size > 0 && float64(size) < buffers {
		compare += ": the table fits, so reads will soon all hit shared_buffers"
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	workers := j.Param().Int("workers", 16)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				// note is in the heap, so the read cannot be answered from the index alone.
				var note int64
				if err := e.wdb().QueryRowContext(ctx, "SELECT note FROM "+table+" WHERE id = $1", 1+rand.Int64N(maxID.Int64)).Scan(&note); err == nil || errors.Is(err, sql.ErrNoRows) {
					n.Add(1)
				} else {
					sleep(ctx, 200*time.Millisecond)
				}
			}
		}()
	}
	go j.RateNote(ctx, fmt.Sprintf("%d readers across %s", workers, table), &n, "reads/s · "+compare)
	wg.Wait()
}
