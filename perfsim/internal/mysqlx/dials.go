package mysqlx

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"perfsim/internal/core"
)

const groupLag = "Replication lag"

var holdParam = core.Param{Key: "hold", Label: "Maximum hold, then release", Type: "pills", Opts: []any{30, 60, 120, 300, 600}, Def: 120, Unit: "s"}

// Dials lists the replication-lag stressors, with whether this topology can
// offer each one. See core.Dial.
func (e *Engine) Dials() []core.Dial {
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()
	w := func(k string) string { return e.w(k) }
	galera, gr, async := topo == "galera", topo == "gr", topo == "async"
	none := "No replicas to fall behind: link a replication cluster, Group Replication or Galera to use this."

	avail := func(d core.Dial, ok bool, why string) core.Dial {
		d.Group, d.Available = groupLag, ok
		if topo == "standalone" {
			d.Available, d.Why = false, none
		} else if !ok {
			d.Why = why
		}
		return d
	}
	replicas := "replicas"
	if galera {
		replicas = "all"
	}

	bigtxn := core.Dial{ID: "bigtxn", Name: "Large transactions", Kind: "toggle", NeedsTable: "pk",
		Desc:  "One large UPDATE on a big table at a fixed interval. The replica applies it as one long event, and nothing behind it moves until it finishes.",
		Watch: []string{"Replication lag", "p99 latency"},
		Params: []core.Param{
			{Key: "rows", Label: "Rows per transaction", Type: "steps", Opts: []any{100000, 250000, 500000, 1000000, 2000000, 5000000}, Def: 1000000, Unit: "rows"},
			{Key: "every", Label: "One every", Type: "pills", Opts: []any{30, 60, 120}, Def: 60, Unit: "s"},
		},
		SQL: "UPDATE perfsim.{table} SET note = note + 1\n WHERE id <= {rows};  -- every {every} s, one transaction"}
	if galera {
		bigtxn.Desc = "One large write-set at a fixed interval. Every node certifies and applies it, so commits wait on the slowest node."
	}

	alter := core.Dial{ID: "alter", Name: "No-op ALTER TABLE", Kind: "run", NeedsTable: "any",
		Desc:   "Rebuilds the table without changing it. Online on the source; replicas apply it only after it commits, so their lag starts when the source finishes.",
		Watch:  []string{"Replication lag", "Write QPS"},
		Params: []core.Param{{Key: "algo", Label: "Algorithm", Type: "pills", Opts: []any{"INPLACE", "COPY"}, Def: "INPLACE"}},
		SQL:    "ALTER TABLE perfsim.{table} ENGINE=InnoDB, ALGORITHM={algo};"}
	if gr {
		alter.Desc = "Rebuilds the table without changing it. Secondaries apply it after the primary commits, and transactions behind it wait in their applier queue."
	}
	if galera {
		alter.Desc = "TOI runs it on every node at once and blocks writes cluster-wide while it runs. RSU runs it on the chosen node only, which desyncs and falls behind."
		alter.Params = []core.Param{{Key: "osu", Label: "Schema upgrade method", Type: "pills", Opts: []any{"TOI", "RSU"}, Def: "TOI"}}
		alter.Members, alter.Single = "all", true
		alter.Note = "The node is used with RSU only; TOI runs on every node."
		alter.Warn = "TOI blocks writes on every node while it runs."
		alter.SQL = "SET SESSION wsrep_OSU_method = '{osu}';\nALTER TABLE perfsim.{table} ENGINE=InnoDB;"
	}

	ftwrl := core.Dial{ID: "ftwrl", Name: "FLUSH TABLES WITH READ LOCK", Kind: "toggle", Members: replicas,
		Desc:   "Takes the global read lock on the chosen replicas. Their applier waits on it, so they fall behind second for second.",
		Watch:  []string{"Replication lag", "Sessions"},
		Params: []core.Param{holdParam},
		SQL:    "FLUSH TABLES WITH READ LOCK;\n-- held up to {hold} s, then:\nUNLOCK TABLES;"}
	if galera {
		ftwrl.Single = true
		ftwrl.Desc = "Takes the global read lock on one node. Galera desyncs that node while it holds the lock, so flow control ignores it: its receive queue grows and it falls behind while the rest of the cluster keeps committing. It catches up after the release."
	}

	stop := core.Dial{ID: "stopapplier", Name: "Stop the applier", Kind: "toggle", Members: "replicas",
		Desc:   "Stops the SQL thread while the receiver keeps going. Relay logs grow and lag climbs until it starts again.",
		Watch:  []string{"Replication lag", "Replica status"},
		Params: []core.Param{holdParam},
		SQL:    w("stop") + ";\n-- up to {hold} s later:\n" + w("start") + ";"}

	nopk := core.Dial{ID: "nopk", Name: "Writes to a table without a primary key", Kind: "toggle", NeedsTable: "nopk",
		Desc:   "Row-based changes to a table with no primary key or index. The replica has to search the table to find each changed row, so it falls further behind the bigger the table is.",
		Watch:  []string{"Replication lag"},
		Params: []core.Param{{Key: "rows", Label: "Rows changed per second", Type: "steps", Opts: []any{10, 50, 200, 1000}, Def: 50, Unit: "rows"}},
		SQL:    "UPDATE perfsim.{table} SET note = note + 1\n WHERE id BETWEEN ? AND ?;  -- {rows} rows each second"}

	single := core.Dial{ID: "singlethread", Name: "Single-threaded applier", Kind: "toggle", Members: replicas,
		Desc:   "Applies everything on one worker. Under enough write load the replica cannot keep up, so lag grows slowly.",
		Watch:  []string{"Replication lag"},
		Params: []core.Param{holdParam},
		SQL:    w("stop") + ";\nSET GLOBAL " + w("workers") + " = " + w("single") + ";\n" + w("start") + ";\n-- the previous value is restored on release"}
	if galera {
		single.Desc = "Sets the chosen node's applier to one thread. Its receive queue grows under write load."
		single.SQL = "SET GLOBAL " + w("applier") + " = 1;\n-- the previous value is restored on release"
	}

	delay := core.Dial{ID: "delay", Name: "Delayed replica", Kind: "toggle", Members: "replicas",
		Desc:   "Holds every event back by a fixed delay. Lag settles at the delay instead of growing.",
		Watch:  []string{"Replication lag"},
		Params: []core.Param{{Key: "delay", Label: "Delay", Type: "pills", Opts: []any{30, 60, 300}, Def: 60, Unit: "s"}, holdParam},
		SQL:    w("stop") + ";\n" + fmt.Sprintf(w("delay"), 0) + ";\n" + w("start") + ";"}
	delay.SQL = strings.Replace(delay.SQL, "= 0", "= {delay}", 1)

	mdl := core.Dial{ID: "mdl", Name: "Open transaction on a replica blocking DDL", Kind: "toggle", Members: "replicas", NeedsTable: "any",
		Desc:   "Keeps a transaction open on the chosen replicas after reading the table. A replicated ALTER of that table then waits for its metadata lock, and everything after it queues.",
		Watch:  []string{"Sessions", "Replication lag"},
		Note:   "Pairs with No-op ALTER TABLE on the same table.",
		Params: []core.Param{holdParam},
		SQL:    "START TRANSACTION;\nSELECT id FROM perfsim.{table} LIMIT 1;\n-- held open up to {hold} s, then ROLLBACK"}

	notGR := "Group Replication's applier is part of group membership; stopping it on its own is not possible."
	notGalera := "Galera nodes are not replicas with an applier thread to stop or delay."
	return []core.Dial{
		avail(bigtxn, true, ""),
		avail(alter, true, ""),
		avail(ftwrl, true, ""),
		avail(stop, async, map[bool]string{true: notGR, false: notGalera}[gr]),
		avail(nopk, async, map[bool]string{true: "Group Replication refuses changes to tables without a primary key.", false: "PXC strict mode refuses changes to tables without a primary key."}[gr]),
		avail(single, !gr, "Group Replication's applier threads can only be changed with the member out of the group."),
		avail(delay, async, map[bool]string{true: "Group Replication has no delayed members.", false: notGalera}[gr]),
		avail(mdl, !galera, "Galera applies DDL in total order and aborts a local transaction that holds the table, so it does not wait."),
	}
}

var tableName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Start launches a dial. See core.Engine.
func (e *Engine) Start(ctx context.Context, id string, p core.Params, targets []string, emit func(level, text string)) (core.Running, error) {
	table := p.Str("table", "")
	needs := ""
	for _, d := range e.Dials() {
		if d.ID == id {
			needs = d.NeedsTable
		}
	}
	if needs != "" {
		if table == "" || !tableName.MatchString(table) {
			return nil, fmt.Errorf("choose a table; prepare one under Datasets first")
		}
	}
	for _, t := range targets {
		if e.member(t) == nil {
			return nil, fmt.Errorf("%s is not a member of %s", t, e.cfg.Name)
		}
	}
	switch id {
	case "bigtxn":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runBigTxn(ctx, j, table, emit) }), nil
	case "alter":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runAlter(ctx, j, table, targets, emit) }), nil
	case "ftwrl":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) {
			e.runHold(ctx, j, targets, "FLUSH TABLES WITH READ LOCK", "UNLOCK TABLES", "Holding the global read lock")
		}), nil
	case "mdl":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) {
			e.runHold(ctx, j, targets, "START TRANSACTION", "ROLLBACK", "Transaction open", "SELECT id FROM perfsim.`"+table+"` LIMIT 1")
		}), nil
	case "stopapplier":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) {
			e.runEach(ctx, j, targets, "Applier stopped", func(ctx context.Context, db *sql.DB, m string) (func(context.Context) error, error) {
				key := "undo:stopapplier:" + m
				e.saveUndo(ctx, key, undo{Kind: "start", Member: m})
				release := func(ctx context.Context) error {
					err := e.releaseApplier(ctx, db, m)
					if err == nil {
						e.Delete(ctx, key)
					}
					return wrap(m, err)
				}
				return release, e.holdApplier(ctx, db, m)
			})
		}), nil
	case "singlethread":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runSingle(ctx, j, targets) }), nil
	case "delay":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runDelay(ctx, j, targets, p.Int("delay", 60)) }), nil
	case "nopk":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runNoPK(ctx, j, table) }), nil
	}
	return nil, fmt.Errorf("no such stressor %q", id)
}

func wrap(member string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", member, err)
}

// runBigTxn commits one large UPDATE on the writer every few seconds.
func (e *Engine) runBigTxn(ctx context.Context, j *core.Job, table string, emit func(string, string)) {
	next := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil {
		p := j.Param()
		rows, every := p.Int("rows", 1000000), p.Int("every", 60)
		if wait := time.Until(next); wait > 0 {
			j.Set(fmt.Sprintf("Next in %d s", int(wait.Seconds()+0.5)), -1, "")
			sleep(ctx, minDur(wait, time.Second))
			continue
		}
		next = time.Now().Add(time.Duration(every) * time.Second)
		db, wid := e.writerDB()
		c, cid, err := connID(ctx, db)
		if err != nil {
			j.Fail(err)
			continue
		}
		j.Set(fmt.Sprintf("Updating %s rows on %s", fmtN(rows), wid), -1, "")
		start := time.Now()
		err = execKillable(ctx, db, c, cid, "UPDATE perfsim.`"+table+"` SET note = note + 1 WHERE id <= ?", rows)
		c.Close()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			j.Fail(err)
			emit("warn", "Large transaction failed: "+err.Error())
		default:
			emit("neutral", fmt.Sprintf("Large transaction: %s rows committed on %s in %.1f s", fmtN(rows), wid, time.Since(start).Seconds()))
		}
	}
}

// runAlter rebuilds the table, then waits for the replicas to apply it.
func (e *Engine) runAlter(ctx context.Context, j *core.Job, table string, targets []string, emit func(string, string)) {
	p := j.Param()
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()
	db, where := e.writerDB()
	stmt := "ALTER TABLE perfsim.`" + table + "` ENGINE=InnoDB"
	pre := ""
	if topo == "galera" {
		osu := p.Str("osu", "TOI")
		if osu == "RSU" && len(targets) > 0 {
			db, where = e.member(targets[0]), targets[0]
		}
		pre = "SET SESSION wsrep_OSU_method = '" + map[bool]string{true: "RSU", false: "TOI"}[osu == "RSU"] + "'"
		if osu != "RSU" {
			where = "every node (TOI)"
		}
	} else {
		algo := "INPLACE"
		if p.Str("algo", "") == "COPY" {
			algo = "COPY"
		}
		stmt += ", ALGORITHM=" + algo
	}
	c, cid, err := connID(ctx, db)
	if err != nil {
		j.Finish(err)
		return
	}
	defer c.Close()
	if pre != "" {
		if _, err := c.ExecContext(ctx, pre); err != nil {
			j.Finish(err)
			return
		}
	}
	e.enableAlterProgress(ctx, db)
	start := time.Now()
	j.Set("Running on "+where, -1, "")
	stopProg := make(chan struct{})
	go e.watchAlterProgress(ctx, db, cid, j, "Running on "+where, start, stopProg)
	err = execKillable(ctx, db, c, cid, stmt)
	close(stopProg)
	if err != nil {
		j.Finish(err)
		return
	}
	took := time.Since(start)
	if topo == "galera" {
		emit("neutral", fmt.Sprintf("ALTER TABLE %s finished on %s in %.0f s", table, where, took.Seconds()))
		j.Finish(nil)
		return
	}
	emit("neutral", fmt.Sprintf("ALTER TABLE %s committed on %s in %.0f s; replicas are applying it", table, where, took.Seconds()))
	// Replicas apply it after the commit. It is done when every replica has
	// caught up again; a few seconds' grace first, because right after the
	// commit the heartbeat has not had time to fall behind.
	applyStart, settled := time.Now(), 0
	for ctx.Err() == nil {
		worst := 0.0
		for _, r := range e.replicaIDs() {
			if l := e.lagOf(r); l > worst {
				worst = l
			}
		}
		j.Set("Replicas applying", -1, fmt.Sprintf("worst lag %.0f s · %s since commit", worst, fmtDur(time.Since(applyStart))))
		if time.Since(applyStart) > 5*time.Second && worst < 2 {
			settled++
		} else {
			settled = 0
		}
		if settled >= 3 {
			j.Finish(nil)
			return
		}
		sleep(ctx, time.Second)
	}
}

// enableAlterProgress turns on the stage instruments that report an InnoDB
// ALTER's progress. Best effort: without them the bar shows elapsed time only.
func (e *Engine) enableAlterProgress(ctx context.Context, db *sql.DB) {
	db.ExecContext(ctx, "UPDATE performance_schema.setup_instruments SET ENABLED = 'YES', TIMED = 'YES' WHERE NAME LIKE 'stage/innodb/alter%'")
	db.ExecContext(ctx, "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME LIKE 'events_stages_%'")
}

func (e *Engine) watchAlterProgress(ctx context.Context, db *sql.DB, cid int64, j *core.Job, phase string, start time.Time, stop chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-time.After(time.Second):
		}
		var done, est sql.NullFloat64
		db.QueryRowContext(ctx, `SELECT s.WORK_COMPLETED, s.WORK_ESTIMATED FROM performance_schema.events_stages_current s
			JOIN performance_schema.threads t ON t.THREAD_ID = s.THREAD_ID WHERE t.PROCESSLIST_ID = ?`, cid).Scan(&done, &est)
		prog := -1.0
		if est.Valid && est.Float64 > 0 {
			prog = done.Float64 / est.Float64
		}
		j.Set(phase, prog, fmtDur(time.Since(start))+" elapsed")
	}
}

// runHold opens a connection on each target, takes something (a lock, an open
// transaction), and keeps it until released.
func (e *Engine) runHold(ctx context.Context, j *core.Job, targets []string, take, release, phase string, after ...string) {
	held := 0
	for _, m := range targets {
		db := e.member(m)
		c, cid, err := connID(ctx, db)
		if err != nil {
			j.Fail(wrap(m, err))
			continue
		}
		j.Hold(m, strconv.FormatInt(cid, 10))
		j.OnRelease(func(rctx context.Context) error {
			defer c.Close()
			_, err := c.ExecContext(rctx, release)
			return wrap(m, err)
		})
		j.Set("Waiting for the lock on "+m, -1, "")
		if err := execKillable(ctx, db, c, cid, take); err != nil {
			if ctx.Err() == nil {
				j.Fail(wrap(m, err))
			}
			continue
		}
		for _, q := range after {
			rows, err := c.QueryContext(ctx, q)
			if err != nil {
				j.Fail(wrap(m, err))
				continue
			}
			for rows.Next() {
			}
			rows.Close()
		}
		held++
	}
	if held == 0 && ctx.Err() == nil {
		j.Finish(fmt.Errorf("nothing could be held: %s", j.Status().Err))
		return
	}
	j.Set(phase+" on "+strings.Join(targets, ", "), -1, "")
	<-ctx.Done()
}

// applyTimeout bounds one member's change. A change can wait behind a lock
// another stressor holds (STOP REPLICA behind FLUSH TABLES WITH READ LOCK), and
// that must not keep the other members waiting or leave the dial half-started.
const applyTimeout = 20 * time.Second

// runEach applies a change on every target at once and keeps each undo for
// release. apply returns its undo even when the change failed or timed out: the
// server may still finish a statement the client gave up on, so the release
// has to run regardless.
func (e *Engine) runEach(ctx context.Context, j *core.Job, targets []string, phase string, apply func(context.Context, *sql.DB, string) (func(context.Context) error, error)) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []string
	j.Set("Applying on "+strings.Join(targets, ", "), -1, "")
	for _, m := range targets {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, applyTimeout)
			undo, err := apply(actx, e.member(m), m)
			timedOut := actx.Err() == context.DeadlineExceeded
			cancel()
			if undo != nil {
				j.OnRelease(undo)
			}
			switch {
			case timedOut:
				j.Fail(fmt.Errorf("%s: no answer after %s; another stressor may be holding a lock there", m, applyTimeout))
			case err != nil:
				j.Fail(wrap(m, err))
			default:
				mu.Lock()
				ok = append(ok, m)
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	if len(ok) == 0 {
		j.Finish(fmt.Errorf("could not apply on any member: %s", j.Status().Err))
		return
	}
	sort.Strings(ok)
	j.Set(phase+" on "+strings.Join(ok, ", "), -1, "")
	<-ctx.Done()
}

func (e *Engine) runSingle(ctx context.Context, j *core.Job, targets []string) {
	e.mu.Lock()
	galera := e.topology == "galera"
	e.mu.Unlock()
	e.runEach(ctx, j, targets, "One applier thread", func(ctx context.Context, db *sql.DB, m string) (func(context.Context) error, error) {
		v, single := e.w("workers"), e.w("single")
		if galera {
			v, single = e.w("applier"), "1"
		}
		old := globalVar(ctx, db, v)
		if old == "" {
			return nil, fmt.Errorf("%s: cannot read %s", m, v)
		}
		key := "undo:singlethread:" + m
		u := undo{Kind: "var", Member: m, Var: v, Value: old, Applier: !galera}
		e.saveUndo(ctx, key, u)
		release := func(ctx context.Context) error {
			err := e.restore(ctx, u)
			if err == nil {
				e.Delete(ctx, key)
			}
			return wrap(m, err)
		}
		return release, e.restore(ctx, undo{Kind: "var", Member: m, Var: v, Value: single, Applier: !galera})
	})
}

func (e *Engine) runDelay(ctx context.Context, j *core.Job, targets []string, delay int) {
	e.runEach(ctx, j, targets, fmt.Sprintf("Delayed by %d s", delay), func(ctx context.Context, db *sql.DB, m string) (func(context.Context) error, error) {
		old := "0"
		if row, ok := queryRow(ctx, db, e.w("status")); ok && row["SQL_Delay"] != "" {
			old = row["SQL_Delay"]
		}
		key := "undo:delay:" + m
		u := undo{Kind: "delay", Member: m, Value: old}
		e.saveUndo(ctx, key, u)
		release := func(ctx context.Context) error {
			err := e.restore(ctx, u)
			if err == nil {
				e.Delete(ctx, key)
			}
			return wrap(m, err)
		}
		return release, e.restore(ctx, undo{Kind: "delay", Member: m, Value: strconv.Itoa(delay)})
	})
}

// runNoPK changes a band of rows each second in a table with no index, so the
// replica has to search for every one of them.
func (e *Engine) runNoPK(ctx context.Context, j *core.Job, table string) {
	var maxID sql.NullInt64
	j.Set("Finding the table's range", -1, "")
	if err := e.wdb().QueryRowContext(ctx, "SELECT MAX(id) FROM perfsim.`"+table+"`").Scan(&maxID); err != nil || !maxID.Valid {
		j.Finish(fmt.Errorf("%s is empty or unreadable", table))
		return
	}
	n, total := 0, time.Duration(0)
	for ctx.Err() == nil {
		rows := int64(j.Param().Int("rows", 50))
		lo := randBetween(1, maxID.Int64-rows)
		start := time.Now()
		_, err := e.wdb().ExecContext(ctx, "UPDATE perfsim.`"+table+"` SET note = note + 1 WHERE id BETWEEN ? AND ?", lo, lo+rows-1)
		if err != nil && ctx.Err() == nil {
			j.Fail(err)
		}
		took := time.Since(start)
		n++
		total += took
		j.Set(fmt.Sprintf("Changing %d rows a second", rows), -1, fmt.Sprintf("%d statements · %.0f ms each on the writer", n, float64(total.Milliseconds())/float64(n)))
		sleep(ctx, time.Second-took)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func fmtN(n int) string {
	switch {
	case n >= 1000000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', -1, 64) + "M"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', -1, 64) + "k"
	}
	return strconv.Itoa(n)
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}
