package mysqlx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"perfsim/internal/core"
)

// Table sets on MySQL: a database per set, many small InnoDB tables in it. See
// core/tablesets.go for why. The statements go to the writer member directly as the admin
// user — the application user behind a ProxySQL need not be allowed CREATE DATABASE.
//
// Each table records its own row count in its COMMENT. The listing then reads only the
// data dictionary: asking for TABLE_ROWS with fresh statistics would open every table of
// every set every ten seconds, which is exactly the load a set exists to measure.

const tsWorkers = 4

func (e *Engine) TableSetWords() (string, string) { return "database", "table" }

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func (e *Engine) TableSets(ctx context.Context) ([]core.TableSet, error) {
	db, _ := e.writerDB()
	rows, err := db.QueryContext(ctx, `SELECT s.SCHEMA_NAME, COUNT(t.TABLE_NAME),
		IFNULL(SUM(CASE WHEN t.TABLE_COMMENT LIKE 'rows=%' THEN CAST(SUBSTRING(t.TABLE_COMMENT, 6) AS UNSIGNED) ELSE 0 END), 0)
		FROM information_schema.SCHEMATA s
		LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
		WHERE s.SCHEMA_NAME LIKE 'perfsim\_%'
		GROUP BY s.SCHEMA_NAME`)
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
	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(set)); err != nil {
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
	full := quoteIdent(set) + "." + quoteIdent(name)
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+full+
		" (id INT NOT NULL PRIMARY KEY, v INT NOT NULL, note VARCHAR(32) NOT NULL) ENGINE=InnoDB COMMENT='rows="+strconv.Itoa(nrows)+"'"); err != nil {
		return err
	}
	const batch = 1000
	for lo := 1; lo <= nrows; lo += batch {
		hi := min(nrows, lo+batch-1)
		var b strings.Builder
		b.WriteString("INSERT IGNORE INTO " + full + " (id, v, note) VALUES ")
		for i := lo; i <= hi; i++ {
			if i > lo {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "(%d,%d,'row %d')", i, i%1000, i)
		}
		if _, err := db.ExecContext(ctx, b.String()); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) SetTables(ctx context.Context, set string) ([]string, error) {
	db, _ := e.writerDB()
	rows, err := db.QueryContext(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", set)
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
	_, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+quoteIdent(set)+"."+quoteIdent(table))
	return err
}

func (e *Engine) DropTableSet(ctx context.Context, set string) error {
	if !core.TableSetName.MatchString(set) {
		return fmt.Errorf("%s is not a table set", set)
	}
	db, _ := e.writerDB()
	_, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(set))
	return err
}

// The interface is optional to the hub, so a misspelt method would hide the section
// silently rather than fail to build. This makes it fail to build.
var _ core.TableSetter = (*Engine)(nil)
