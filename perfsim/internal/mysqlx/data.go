package mysqlx

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

// rowBytes is about what one row of a prepared table takes on disk, data and
// page overhead together, so a size asked for in GB becomes a row count.
const rowBytes = 300

// batchRows is one INSERT of a preparation: perfsim.seq's 20,000 rows, about
// 6 MB, so replicas never get a transaction big enough to lag on by itself.
const batchRows = 20000

var datasetName = regexp.MustCompile(`^(big|nopk)_[0-9a-z.]+$`)

// Datasets lists the prepared tables. See core.Engine.
func (e *Engine) Datasets(ctx context.Context) ([]core.Dataset, error) {
	c, err := e.wdb().Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	// MySQL 8 caches table sizes for a day by default; a table being prepared
	// would show the size it had when first looked at.
	c.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
	rows, err := c.QueryContext(ctx, `SELECT t.TABLE_NAME, IFNULL(t.DATA_LENGTH + t.INDEX_LENGTH, 0), IFNULL(t.TABLE_ROWS, 0),
		(SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS k
		  WHERE k.TABLE_SCHEMA = t.TABLE_SCHEMA AND k.TABLE_NAME = t.TABLE_NAME AND k.CONSTRAINT_TYPE = 'PRIMARY KEY')
		FROM information_schema.TABLES t
		WHERE t.TABLE_SCHEMA = 'perfsim' AND (t.TABLE_NAME LIKE 'big\_%' OR t.TABLE_NAME LIKE 'nopk\_%')
		ORDER BY t.TABLE_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Dataset
	for rows.Next() {
		var d core.Dataset
		var pk int
		if rows.Scan(&d.Name, &d.Bytes, &d.Rows, &pk) == nil {
			d.PK = pk > 0
			out = append(out, d)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// TABLES reports sizes from InnoDB's statistics, which are refreshed in the
	// background and trail a table that is growing. The tablespace file size is
	// exact: INNODB_TABLESPACES on MySQL, INNODB_SYS_TABLESPACES on MariaDB.
	for _, q := range []string{
		"SELECT NAME, FILE_SIZE FROM information_schema.INNODB_TABLESPACES WHERE NAME LIKE 'perfsim/%'",
		"SELECT NAME, FILE_SIZE FROM information_schema.INNODB_SYS_TABLESPACES WHERE NAME LIKE 'perfsim/%'",
	} {
		ts, err := c.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		size := map[string]int64{}
		for ts.Next() {
			var name string
			var b int64
			if ts.Scan(&name, &b) == nil {
				size[strings.TrimPrefix(name, "perfsim/")] = b
			}
		}
		ts.Close()
		for i := range out {
			if b, ok := size[out[i].Name]; ok && b > 0 {
				out[i].Bytes = b
			}
		}
		break
	}
	return out, nil
}

// Prepare builds a table of about the given size in 20,000-row inserts.
func (e *Engine) Prepare(ctx context.Context, name string, bytes int64, pk bool, progress func(float64)) error {
	if !datasetName.MatchString(name) {
		return fmt.Errorf("invalid table name %q", name)
	}
	e.mu.Lock()
	topo := e.topology
	e.mu.Unlock()
	if !pk && (topo == "gr" || topo == "galera") {
		return fmt.Errorf("this cluster refuses changes to tables without a primary key")
	}
	ddl := "CREATE TABLE IF NOT EXISTS perfsim.`" + name + "` (id BIGINT NOT NULL AUTO_INCREMENT, customer INT NOT NULL, note INT NOT NULL DEFAULT 0, pad VARCHAR(255) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB"
	if !pk {
		ddl = "CREATE TABLE IF NOT EXISTS perfsim.`" + name + "` (id BIGINT NOT NULL, customer INT NOT NULL, note INT NOT NULL DEFAULT 0, pad VARCHAR(255) NOT NULL) ENGINE=InnoDB"
	}
	if _, err := e.wdb().ExecContext(ctx, ddl); err != nil {
		return err
	}
	target := bytes / rowBytes
	var have int64
	e.wdb().QueryRowContext(ctx, "SELECT IFNULL(MAX(id), 0) FROM perfsim.`"+name+"`").Scan(&have)
	ins := "INSERT INTO perfsim.`" + name + "` (customer, pad) SELECT FLOOR(RAND()*100000), RPAD(MD5(RAND()), 200, 'x') FROM perfsim.seq"
	if !pk {
		ins = "INSERT INTO perfsim.`" + name + "` (id, customer, pad) SELECT n + ? + 1, FLOOR(RAND()*100000), RPAD(MD5(RAND()), 200, 'x') FROM perfsim.seq"
	}
	for have < target {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		if pk {
			_, err = e.wdb().ExecContext(ctx, ins)
		} else {
			_, err = e.wdb().ExecContext(ctx, ins, have)
		}
		if err != nil {
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
	_, err := e.wdb().ExecContext(ctx, "DROP TABLE IF EXISTS perfsim.`"+name+"`")
	return err
}

// Sessions lists what is running on every member. See core.Engine.
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
	db := e.member(m)
	heldSet := map[string]bool{}
	for _, id := range held {
		heldSet[id] = true
	}
	in := "0"
	if len(held) > 0 {
		in = strings.Join(held, ",")
	}
	rows, err := db.QueryContext(ctx, `SELECT ID, IFNULL(USER, ''), IFNULL(COMMAND, ''), IFNULL(TIME, 0), IFNULL(STATE, ''), IFNULL(LEFT(INFO, 300), '')
		FROM information_schema.PROCESSLIST
		WHERE ID <> CONNECTION_ID() AND USER <> 'event_scheduler'
		  AND (COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump', 'Binlog Dump GTID') OR ID IN (`+in+`))`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []core.Session
	for rows.Next() {
		var id int64
		var user, cmd, state, info string
		var t int64
		if rows.Scan(&id, &user, &cmd, &t, &state, &info) != nil {
			continue
		}
		if strings.Contains(info, "perfsim.heartbeat") || strings.Contains(info, "information_schema.PROCESSLIST") {
			continue
		}
		s := core.Session{ID: strconv.FormatInt(id, 10), Member: m, User: user, State: nz(state, cmd), Time: t, Query: info}
		ls := strings.ToLower(state)
		switch {
		case heldSet[s.ID]:
			s.Kind = "blocker"
			if s.Query == "" {
				s.Query = "(holding: idle in an open lock or transaction)"
			}
		case strings.Contains(ls, "waiting for") && strings.Contains(ls, "lock"):
			s.Kind = "blocked"
			if len(held) > 0 {
				s.Waits = held[0]
			}
		}
		out = append(out, s)
	}
	// Row lock waits, with the session that blocks each one.
	if lw, err := db.QueryContext(ctx, "SELECT waiting_pid, blocking_pid FROM sys.innodb_lock_waits"); err == nil {
		defer lw.Close()
		for lw.Next() {
			var w, b int64
			if lw.Scan(&w, &b) != nil {
				continue
			}
			for i := range out {
				switch out[i].ID {
				case strconv.FormatInt(w, 10):
					out[i].Kind, out[i].Waits = "blocked", strconv.FormatInt(b, 10)
				case strconv.FormatInt(b, 10):
					if out[i].Kind == "" {
						out[i].Kind = "blocker"
					}
				}
			}
		}
	}
	return out
}
