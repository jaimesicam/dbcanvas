package pgsql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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

func (e *Engine) Dials() []core.Dial {
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()
	pauseNote, delayMembers, delayNote := "", "replicas", ""
	delaySQL := "ALTER SYSTEM SET recovery_min_apply_delay = '{delay}s';\nSELECT pg_reload_conf();\n-- the previous value is restored on release"
	if e.patroni() {
		pauseNote = "Patroni resumes paused replay unless the cluster is in maintenance mode, so the cluster is put in maintenance mode while this holds: automatic failover is off until it is released."
		delayMembers = ""
		delayNote = "Patroni sets this parameter itself, so the sim sets it in Patroni's configuration, which applies it to every standby of the cluster."
		delaySQL = "PATCH /config {\"postgresql\": {\"parameters\": {\"recovery_min_apply_delay\": \"{delay}s\"}}}\n-- Patroni's REST API; the previous value is restored on release"
	}
	avail := func(d core.Dial) core.Dial {
		d.Group, d.Available = groupLag, topo != "standalone"
		if !d.Available {
			d.Why = "No standbys to fall behind: link a Patroni or repmgr cluster to use this."
		}
		return d
	}
	return append([]core.Dial{
		avail(core.Dial{ID: "bigtxn", Name: "Large transactions", Kind: "toggle", NeedsTable: "pk",
			Desc:  "One large UPDATE on a big table at a fixed interval. Every changed row is written to the WAL, and the standbys replay all of it before anything committed after it is visible there.",
			Watch: []string{"Replay lag", "p99 latency"},
			Params: []core.Param{
				{Key: "rows", Label: "Rows per transaction", Type: "steps", Opts: []any{100000, 250000, 500000, 1000000, 2000000, 5000000}, Def: 1000000, Unit: "rows"},
				{Key: "every", Label: "One every", Type: "pills", Opts: []any{30, 60, 120}, Def: 60, Unit: "s"},
			},
			SQL: "UPDATE perfsim.{table} SET note = note + 1\n WHERE id <= {rows};  -- every {every} s, one transaction"}),
		avail(core.Dial{ID: "rewrite", Name: "Table rewrite (VACUUM FULL)", Kind: "run", NeedsTable: "any",
			Desc:  "Rewrites the whole table, writing its size again to the WAL. Standbys replay it as fast as they can, and their lag shows how long that takes.",
			Watch: []string{"Replay lag", "Write QPS"}, Params: []core.Param{},
			SQL: "VACUUM FULL perfsim.{table};"}),
		avail(core.Dial{ID: "pause", Name: "Pause WAL replay", Kind: "toggle", Members: "replicas",
			Desc:   "Pauses replay on the chosen standbys. WAL keeps arriving, nothing is applied, and their replay lag grows second for second.",
			Watch:  []string{"Replay lag", "Replication status"},
			Params: []core.Param{holdParam}, Warn: pauseNote,
			SQL: "SELECT pg_wal_replay_pause();\n-- up to {hold} s later:\nSELECT pg_wal_replay_resume();"}),
		avail(core.Dial{ID: "conflict", Name: "Conflicting standby query", Kind: "toggle", Members: "replicas",
			Desc:   "A transaction on the chosen standbys keeps reading a table, while the primary takes an exclusive lock on it every 40 s. Replay must take that lock too, so it waits for the standby's transaction up to max_standby_streaming_delay, then cancels it, and starts again.",
			Watch:  []string{"Replay lag", "Sessions"},
			Params: []core.Param{holdParam},
			Note:   "How long replay waits is the standby's max_standby_streaming_delay (shown under Replication status).",
			SQL:    "-- on the standby, held open:\nBEGIN ISOLATION LEVEL REPEATABLE READ;\nSELECT count(*) FROM perfsim.conflict;\n-- on the primary, every 40 s:\nBEGIN; LOCK TABLE perfsim.conflict IN ACCESS EXCLUSIVE MODE; COMMIT;"}),
		avail(core.Dial{ID: "delay", Name: "Delayed standby", Kind: "toggle", Members: delayMembers,
			Desc:  "Holds every transaction back by a fixed delay before the standbys apply it. Lag settles at the delay instead of growing.",
			Watch: []string{"Replay lag"}, Note: delayNote,
			Params: []core.Param{{Key: "delay", Label: "Delay", Type: "pills", Opts: []any{30, 60, 300}, Def: 60, Unit: "s"}, holdParam},
			SQL:    delaySQL}),
	}, e.loadDials()...)
}

var tableName = regexp.MustCompile(`^[a-z0-9_]+$`)

func (e *Engine) Start(ctx context.Context, id string, p core.Params, targets []string, emit func(level, text string)) (core.Running, error) {
	table := p.Str("table", "")
	for _, d := range e.Dials() {
		if d.ID == id && d.NeedsTable != "" && (table == "" || !tableName.MatchString(table)) {
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
	case "rewrite":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runRewrite(ctx, j, table, emit) }), nil
	case "pause":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runPause(ctx, j, targets) }), nil
	case "conflict":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runConflict(ctx, j, targets, emit) }), nil
	case "delay":
		return core.NewJob(ctx, p, func(ctx context.Context, j *core.Job) { e.runDelay(ctx, j, targets, p.Int("delay", 60)) }), nil
	}
	if run, ok := e.startLoad(ctx, id, p, table); ok {
		return run, nil
	}
	return nil, fmt.Errorf("no such stressor %q", id)
}

func wrap(m string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", m, err)
}

func (e *Engine) runBigTxn(ctx context.Context, j *core.Job, table string, emit func(string, string)) {
	next := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil {
		p := j.Param()
		rows, every := p.Int("rows", 1000000), p.Int("every", 60)
		if wait := time.Until(next); wait > 0 {
			j.Set(fmt.Sprintf("Next in %d s", int(wait.Seconds()+0.5)), -1, "")
			sleep(ctx, min(wait, time.Second))
			continue
		}
		next = time.Now().Add(time.Duration(every) * time.Second)
		d, wid := e.writerDB()
		c, pid, err := connPID(ctx, d)
		if err != nil {
			j.Fail(err)
			continue
		}
		j.Set(fmt.Sprintf("Updating %s rows on %s", fmtN(rows), wid), -1, "")
		start := time.Now()
		err = execCancelable(ctx, d, c, pid, "UPDATE "+table+" SET note = note + 1 WHERE id <= $1", rows)
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

func (e *Engine) runRewrite(ctx context.Context, j *core.Job, table string, emit func(string, string)) {
	d, wid := e.writerDB()
	c, pid, err := connPID(ctx, d)
	if err != nil {
		j.Finish(err)
		return
	}
	defer c.Close()
	start := time.Now()
	stop := make(chan struct{})
	go func() {
		// VACUUM FULL reports its progress where CLUSTER does.
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			var done, total sql.NullFloat64
			d.QueryRowContext(ctx, "SELECT heap_blks_scanned, heap_blks_total FROM pg_stat_progress_cluster WHERE pid = $1", pid).Scan(&done, &total)
			prog := -1.0
			if total.Valid && total.Float64 > 0 {
				prog = done.Float64 / total.Float64
			}
			j.Set("Rewriting on "+wid, prog, fmtDur(time.Since(start))+" elapsed")
		}
	}()
	j.Set("Rewriting on "+wid, -1, "")
	err = execCancelable(ctx, d, c, pid, "VACUUM FULL "+table)
	close(stop)
	if err != nil {
		j.Finish(err)
		return
	}
	emit("neutral", fmt.Sprintf("VACUUM FULL %s finished on %s in %.0f s; standbys are replaying it", table, wid, time.Since(start).Seconds()))
	applyStart, settled := time.Now(), 0
	for ctx.Err() == nil {
		worst := 0.0
		for _, r := range e.replicaIDs() {
			if l := e.lagOf(r); l > worst {
				worst = l
			}
		}
		j.Set("Standbys replaying", -1, fmt.Sprintf("worst lag %.0f s · %s since it finished", worst, fmtDur(time.Since(applyStart))))
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

// runEach applies a change on every target at once and keeps each undo. As in
// the MySQL engine, the undo is registered whether or not the change landed.
func (e *Engine) runEach(ctx context.Context, j *core.Job, targets []string, phase string, apply func(context.Context, *sql.DB, string) (func(context.Context) error, error)) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []string
	j.Set("Applying on "+strings.Join(targets, ", "), -1, "")
	for _, m := range targets {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, 20*time.Second)
			undo, err := apply(actx, e.member(m), m)
			cancel()
			if undo != nil {
				j.OnRelease(undo)
			}
			if err != nil {
				j.Fail(wrap(m, err))
				return
			}
			mu.Lock()
			ok = append(ok, m)
			mu.Unlock()
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

func (e *Engine) runPause(ctx context.Context, j *core.Job, targets []string) {
	if e.patroni() {
		cfg, err := e.patroniConfig(ctx)
		if err != nil {
			j.Finish(fmt.Errorf("asking Patroni whether the cluster is in maintenance mode: %w", err))
			return
		}
		if !cfg.Pause {
			e.saveUndo(ctx, "undo:maintenance", undo{Kind: "maintenance"})
			j.OnRelease(func(ctx context.Context) error {
				err := e.restore(ctx, undo{Kind: "maintenance"})
				if err == nil {
					e.Delete(ctx, "undo:maintenance")
				}
				return err
			})
			if err := e.setPatroniMaintenance(ctx, true); err != nil {
				j.Finish(fmt.Errorf("putting the cluster in maintenance mode: %w", err))
				return
			}
			// Patroni acts on the change on its next loop.
			sleep(ctx, 12*time.Second)
		}
	}
	e.runEach(ctx, j, targets, "Replay paused", func(ctx context.Context, d *sql.DB, m string) (func(context.Context) error, error) {
		key := "undo:pause:" + m
		e.saveUndo(ctx, key, undo{Kind: "resume", Member: m})
		release := func(ctx context.Context) error {
			err := e.restore(ctx, undo{Kind: "resume", Member: m})
			if err == nil {
				e.Delete(ctx, key)
			}
			return wrap(m, err)
		}
		_, err := d.ExecContext(ctx, "SELECT pg_wal_replay_pause()")
		return release, err
	})
}

func (e *Engine) runDelay(ctx context.Context, j *core.Job, targets []string, delay int) {
	if e.patroni() {
		cfg, err := e.patroniConfig(ctx)
		if err != nil {
			j.Finish(fmt.Errorf("reading Patroni's configuration: %w", err))
			return
		}
		old := ""
		if v, ok := cfg.PostgreSQL.Parameters["recovery_min_apply_delay"]; ok && v != nil {
			old = fmt.Sprint(v)
		}
		u := undo{Kind: "patroni-delay", Value: old}
		e.saveUndo(ctx, "undo:patroni-delay", u)
		j.OnRelease(func(ctx context.Context) error {
			err := e.restore(ctx, u)
			if err == nil {
				e.Delete(ctx, "undo:patroni-delay")
			}
			return err
		})
		if err := e.restore(ctx, undo{Kind: "patroni-delay", Value: strconv.Itoa(delay) + "s"}); err != nil {
			j.Finish(err)
			return
		}
		j.Set(fmt.Sprintf("Every standby delayed by %d s (Patroni configuration)", delay), -1, "")
		<-ctx.Done()
		return
	}
	e.runEach(ctx, j, targets, fmt.Sprintf("Delayed by %d s", delay), func(ctx context.Context, d *sql.DB, m string) (func(context.Context) error, error) {
		var old string
		if err := d.QueryRowContext(ctx, "SELECT current_setting('recovery_min_apply_delay')").Scan(&old); err != nil {
			return nil, err
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
		return release, e.restore(ctx, undo{Kind: "delay", Member: m, Value: strconv.Itoa(delay) + "s"})
	})
}

// runConflict holds a reading transaction on each target and has the primary
// take an exclusive lock on the same table every 40 s. Replay of that lock
// conflicts with the standby's transaction; the standby cancels it after
// max_standby_streaming_delay, and the hold is opened again.
func (e *Engine) runConflict(ctx context.Context, j *core.Job, targets []string, emit func(string, string)) {
	var mu sync.Mutex
	cancelled := 0
	var wg sync.WaitGroup
	for _, m := range targets {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			d := e.member(m)
			for ctx.Err() == nil {
				c, pid, err := connPID(ctx, d)
				if err != nil {
					j.Fail(wrap(m, err))
					sleep(ctx, 3*time.Second)
					continue
				}
				j.Hold(m, strconv.FormatInt(pid, 10))
				_, err = c.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ")
				if err == nil {
					_, err = c.ExecContext(ctx, "SELECT count(*) FROM conflict")
				}
				for err == nil && ctx.Err() == nil {
					sleep(ctx, 2*time.Second)
					if ctx.Err() == nil {
						_, err = c.ExecContext(ctx, "SELECT 1")
					}
				}
				rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				c.ExecContext(rctx, "ROLLBACK")
				cancel()
				c.Close()
				if ctx.Err() == nil && err != nil {
					mu.Lock()
					cancelled++
					n := cancelled
					mu.Unlock()
					j.Note(fmt.Sprintf("%d standby transactions cancelled by replay so far", n))
					emit("neutral", fmt.Sprintf("%s cancelled the sim's transaction to let replay continue", m))
				}
			}
		}(m)
	}
	// The primary's side.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sleep(ctx, 3*time.Second)
		for ctx.Err() == nil {
			d, _ := e.writerDB()
			if tx, err := d.BeginTx(ctx, nil); err == nil {
				if _, err := tx.ExecContext(ctx, "LOCK TABLE conflict IN ACCESS EXCLUSIVE MODE"); err == nil {
					tx.Commit()
				} else {
					tx.Rollback()
				}
			}
			sleep(ctx, 40*time.Second)
		}
	}()
	j.Set("Standby transactions open on "+strings.Join(targets, ", "), -1, "")
	wg.Wait()
}

// undo is a change made to a member, recorded before it is made. See the
// MySQL engine's undo for why; a pause and a delay both outlive the sim that
// set them unless something takes them back.
type undo struct {
	Kind   string `json:"kind"` // resume | delay | maintenance | patroni-delay
	Member string `json:"member"`
	Value  string `json:"value,omitempty"`
}

var delayValue = regexp.MustCompile(`^[0-9]+(ms|s|min)?$`)

func (e *Engine) saveUndo(ctx context.Context, key string, u undo) {
	b, _ := json.Marshal(u)
	if err := e.Save(ctx, key, b); err != nil {
		log.Printf("perfsim: recording how to undo %s: %v", key, err)
	}
}

func (e *Engine) restore(ctx context.Context, u undo) error {
	switch u.Kind {
	case "maintenance":
		return e.setPatroniMaintenance(ctx, false)
	case "patroni-delay":
		if u.Value != "" && !delayValue.MatchString(u.Value) {
			return fmt.Errorf("refusing to set recovery_min_apply_delay to %q", u.Value)
		}
		return e.setPatroniParam(ctx, "recovery_min_apply_delay", u.Value)
	}
	d := e.member(u.Member)
	if d == nil {
		return fmt.Errorf("%s is no longer a member", u.Member)
	}
	switch u.Kind {
	case "resume":
		_, err := d.ExecContext(ctx, "SELECT pg_wal_replay_resume()")
		if err != nil && strings.Contains(err.Error(), "recovery is not in progress") {
			return nil // promoted since: nothing is paused
		}
		return err
	case "delay":
		if !delayValue.MatchString(u.Value) {
			return fmt.Errorf("refusing to set recovery_min_apply_delay to %q", u.Value)
		}
		if _, err := d.ExecContext(ctx, "ALTER SYSTEM SET recovery_min_apply_delay = '"+u.Value+"'"); err != nil {
			return err
		}
		if _, err := d.ExecContext(ctx, "SELECT pg_reload_conf()"); err != nil {
			return err
		}
		// A reload is asynchronous; check that it took, since a cluster manager
		// that owns the setting would put its own value back.
		want := u.Value
		for i := 0; i < 10; i++ {
			var got string
			d.QueryRowContext(ctx, "SELECT current_setting('recovery_min_apply_delay')").Scan(&got)
			if sameDelay(got, want) {
				return nil
			}
			sleep(ctx, 300*time.Millisecond)
		}
		return fmt.Errorf("recovery_min_apply_delay did not change to %s after a reload; the cluster's manager may own it", u.Value)
	}
	return fmt.Errorf("unknown undo %q", u.Kind)
}

// sameDelay compares two recovery_min_apply_delay spellings ("60s", "1min", "0").
func sameDelay(a, b string) bool {
	ms := func(s string) int {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasSuffix(s, "ms"):
			n, _ := strconv.Atoi(strings.TrimSuffix(s, "ms"))
			return n
		case strings.HasSuffix(s, "min"):
			n, _ := strconv.Atoi(strings.TrimSuffix(s, "min"))
			return n * 60000
		case strings.HasSuffix(s, "s"):
			n, _ := strconv.Atoi(strings.TrimSuffix(s, "s"))
			return n * 1000
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	return ms(a) == ms(b)
}

func (e *Engine) replayUndo(ctx context.Context) {
	all, err := e.Load(ctx, "undo:")
	if err != nil {
		return
	}
	for key, b := range all {
		var u undo
		if json.Unmarshal(b, &u) != nil {
			e.Delete(ctx, key)
			continue
		}
		if err := e.restore(ctx, u); err != nil {
			log.Printf("perfsim: could not undo %s left by an earlier run: %v", key, err)
			continue
		}
		log.Printf("perfsim: undid %s left by an earlier run", key)
		e.Delete(ctx, key)
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
