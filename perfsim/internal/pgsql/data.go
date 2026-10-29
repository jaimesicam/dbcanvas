package pgsql

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"perfsim/internal/core"
)

const batchRows = 20000

// rowBytes is about what one row of a prepared table takes with its index.
const rowBytes = 300

var datasetName = regexp.MustCompile(`^big_[0-9a-z.]+$`)

func (e *Engine) Datasets(ctx context.Context) ([]core.Dataset, error) {
	// reltuples is -1 until the table is first analyzed; estimate from its size until then.
	rows, err := e.wdb().QueryContext(ctx, `SELECT c.relname, pg_total_relation_size(c.oid),
			(CASE WHEN c.reltuples > 0 THEN c.reltuples ELSE pg_relation_size(c.oid) / 270 END)::bigint
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' AND c.relname LIKE 'big\_%'
		ORDER BY c.relname`, e.schema())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Dataset
	for rows.Next() {
		var d core.Dataset
		if rows.Scan(&d.Name, &d.Bytes, &d.Rows) == nil {
			d.PK = true
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// Prepare builds a table of about the given size in 20,000-row inserts. A
// table without a primary key teaches nothing on PostgreSQL's physical
// replication, which replays pages rather than looking rows up, so every
// prepared table has one.
func (e *Engine) Prepare(ctx context.Context, name string, bytes int64, pk bool, progress func(float64)) error {
	if !pk {
		return fmt.Errorf("PostgreSQL's physical replication replays pages, not row lookups, so a table without a primary key behaves no differently here")
	}
	name = strings.ReplaceAll(name, ".", "_")
	if !datasetName.MatchString(name) {
		return fmt.Errorf("invalid table name %q", name)
	}
	create := "CREATE TABLE IF NOT EXISTS " + name + " (id bigserial PRIMARY KEY, customer int NOT NULL, note int NOT NULL DEFAULT 0, pad text NOT NULL)"
	if _, err := e.wdb().ExecContext(ctx, create); err != nil {
		return err
	}
	// On Spock the table must exist on every member and be replicated before
	// any row is written, or the rows stay on the writer.
	if e.spock() {
		for id, d := range e.mdb {
			if _, err := d.ExecContext(ctx, create); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
		}
		if err := e.spockReplicate(ctx, name); err != nil {
			return err
		}
	}
	target := bytes / rowBytes
	var have int64
	e.wdb().QueryRowContext(ctx, "SELECT COALESCE(max(id), 0) FROM "+name).Scan(&have)
	for have < target {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := e.wdb().ExecContext(ctx, "INSERT INTO "+name+" (customer, pad) SELECT (random()*100000)::int, rpad(md5(random()::text), 200, 'x') FROM generate_series(1, "+strconv.Itoa(batchRows)+")"); err != nil {
			return err
		}
		have += batchRows
		progress(float64(have) / float64(target))
	}
	progress(1)
	return nil
}

func (e *Engine) Drop(ctx context.Context, name string) error {
	if !datasetName.MatchString(name) {
		return fmt.Errorf("only prepared tables can be dropped")
	}
	_, err := e.wdb().ExecContext(ctx, "DROP TABLE IF EXISTS "+name)
	if err == nil && e.spock() {
		// No DDL is replicated: drop it on every member.
		for id, d := range e.mdb {
			if _, derr := d.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); derr != nil {
				return fmt.Errorf("%s: %w", id, derr)
			}
		}
	}
	return err
}

// Sessions lists what is running on every member, including the startup
// process (WAL replay) on a standby, which is what waits when replay conflicts.
func (e *Engine) Sessions(ctx context.Context, held map[string][]string) ([]core.Session, error) {
	var mu sync.Mutex
	var out []core.Session
	var wg sync.WaitGroup
	for _, mc := range e.cfg.Members {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			ss := e.memberSessions(ctx, m, held[m])
			mu.Lock()
			out = append(out, ss...)
			mu.Unlock()
		}(mc.ID)
	}
	wg.Wait()
	rank := map[string]int{"blocker": 0, "blocked": 1, "": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Time > out[j].Time
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out, nil
}

func (e *Engine) memberSessions(ctx context.Context, m string, held []string) []core.Session {
	heldSet := map[string]bool{}
	ids := []string{"0"}
	for _, h := range held {
		if _, err := strconv.Atoi(h); err == nil {
			heldSet[h] = true
			ids = append(ids, h)
		}
	}
	rows, err := e.member(m).QueryContext(ctx, `SELECT pid, COALESCE(usename, backend_type), COALESCE(state, ''), COALESCE(wait_event_type, ''), COALESCE(wait_event, ''),
			EXTRACT(EPOCH FROM (now() - COALESCE(xact_start, query_start, backend_start)))::bigint, left(COALESCE(query, ''), 300),
			array_to_string(pg_blocking_pids(pid), ','), backend_type
		FROM pg_stat_activity
		WHERE pid <> pg_backend_pid()
		  AND (backend_type = 'startup' OR (backend_type = 'client backend' AND (state <> 'idle' OR pid IN (`+strings.Join(ids, ",")+`))))`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []core.Session
	for rows.Next() {
		var pid, t int64
		var user, state, wtype, wevent, query, blocking, btype string
		if rows.Scan(&pid, &user, &state, &wtype, &wevent, &t, &query, &blocking, &btype) != nil {
			continue
		}
		if strings.Contains(query, "heartbeat") || strings.Contains(query, "pg_stat_activity") {
			continue
		}
		st := state
		if wtype != "" {
			st = strings.TrimSpace(st + " · " + wtype + ":" + wevent)
		}
		if btype == "startup" {
			user, query = "WAL replay", "(startup process)"
			if wtype == "" {
				continue // replaying without waiting: nothing to show
			}
		}
		s := core.Session{ID: strconv.FormatInt(pid, 10), Member: m, User: user, State: st, Time: t, Query: query}
		switch {
		case heldSet[s.ID]:
			s.Kind = "blocker"
			if s.Query == "" {
				s.Query = "(held open by the sim)"
			}
		case blocking != "":
			s.Kind, s.Waits = "blocked", strings.Split(blocking, ",")[0]
		case wtype == "Lock" && len(held) > 0:
			s.Kind, s.Waits = "blocked", held[0]
		}
		out = append(out, s)
	}
	return out
}
