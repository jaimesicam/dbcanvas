package pgsql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"perfsim/internal/core"
)

// Table sets on PostgreSQL: a schema per set in the sim's database, many small tables in
// it. A schema rather than a database because a database is reached only by a connection
// of its own, and because DROP SCHEMA … CASCADE is where PostgreSQL shows the cost of many
// tables: it takes a lock on every table, index and TOAST relation in one transaction, and
// past max_locks_per_transaction × max_connections it fails with "out of shared memory".
// The one-at-a-time drop never does. See core/tablesets.go.
//
// Each table's row count is in its COMMENT, so the listing reads the catalog only.

const tsWorkers = 4

func (e *Engine) TableSetWords() (string, string) { return "schema", "table" }

func qi(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (e *Engine) TableSets(ctx context.Context) ([]core.TableSet, error) {
	db, _ := e.writerDB()
	rows, err := db.QueryContext(ctx, `SELECT n.nspname, count(c.oid),
		coalesce(sum(CASE WHEN d.description LIKE 'rows=%' THEN substr(d.description, 6)::bigint ELSE 0 END), 0)
		FROM pg_namespace n
		LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relkind = 'r'
		LEFT JOIN pg_description d ON d.objoid = c.oid AND d.classoid = 'pg_class'::regclass AND d.objsubid = 0
		WHERE n.nspname LIKE 'perfsim\_%'
		GROUP BY n.nspname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.TableSet
	for rows.Next() {
		var s core.TableSet
		if rows.Scan(&s.DB, &s.Tables, &s.Rows) == nil && core.TableSetName.MatchString(s.DB) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

func (e *Engine) CreateTableSet(ctx context.Context, set string, tables, nrows int, done func(int)) error {
	db, _ := e.writerDB()
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+qi(set)); err != nil {
		return err
	}
	have := map[string]bool{}
	if names, err := e.SetTables(ctx, set); err == nil {
		for _, n := range names {
			have[n] = true
		}
	}
	var next, finished atomic.Int64
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for w := 0; w < tsWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wctx.Err() == nil {
				i := int(next.Add(1))
				if i > tables {
					return
				}
				name := core.SetTableName(i)
				if !have[name] {
					if err := e.createSetTable(wctx, set, name, nrows); err != nil {
						once.Do(func() { firstErr = fmt.Errorf("%s: %w", name, err); cancel() })
						return
					}
				}
				done(int(finished.Add(1)))
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (e *Engine) createSetTable(ctx context.Context, set, name string, nrows int) error {
	db, _ := e.writerDB()
	full := qi(set) + "." + qi(name)
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS " + full + " (id int PRIMARY KEY, v int NOT NULL, note varchar(32) NOT NULL)",
		"COMMENT ON TABLE " + full + " IS 'rows=" + strconv.Itoa(nrows) + "'",
	}
	if nrows > 0 {
		stmts = append(stmts, "INSERT INTO "+full+" SELECT g, g % 1000, 'row ' || g FROM generate_series(1, "+
			strconv.Itoa(nrows)+") g ON CONFLICT DO NOTHING")
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) SetTables(ctx context.Context, set string) ([]string, error) {
	db, _ := e.writerDB()
	rows, err := db.QueryContext(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' ORDER BY c.relname`, set)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

func (e *Engine) DropSetTable(ctx context.Context, set, table string) error {
	db, _ := e.writerDB()
	_, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+qi(set)+"."+qi(table))
	return err
}

func (e *Engine) DropTableSet(ctx context.Context, set string) error {
	if !core.TableSetName.MatchString(set) {
		return fmt.Errorf("%s is not a table set", set)
	}
	db, _ := e.writerDB()
	_, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+qi(set)+" CASCADE")
	return err
}

// The interface is optional to the hub, so a misspelt method would hide the section
// silently rather than fail to build. This makes it fail to build.
var _ core.TableSetter = (*Engine)(nil)
