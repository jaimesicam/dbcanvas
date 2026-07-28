// diagnostics.go backs the dashboard's read-only diagnostics tools —
// processlist, InnoDB transactions, lock waits, table sizes, and an
// on-demand EXPLAIN — deliberately read-only: fixes go through the
// existing DBCanvas Terminal/Query Runner, never from here.
package store

import (
	"context"
	"database/sql"
	"strconv"
)

type ProcessRow struct {
	ID      int64
	User    string
	Host    string
	DB      string
	Command string
	Time    int64
	State   string
	Info    string
}

// Processlist lists every connection this app's own app user can see —
// same visibility any learner's own `SHOW PROCESSLIST` would have.
func (s *Store) Processlist(ctx context.Context) ([]ProcessRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT ID, USER, HOST, COALESCE(DB,''), COMMAND, TIME, COALESCE(STATE,''), COALESCE(INFO,'') FROM information_schema.PROCESSLIST ORDER BY TIME DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProcessRow
	for rows.Next() {
		var p ProcessRow
		if rows.Scan(&p.ID, &p.User, &p.Host, &p.DB, &p.Command, &p.Time, &p.State, &p.Info) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

type LockWaitRow struct {
	WaitingTrxID  string
	WaitingThread int64
	WaitingQuery  string
	BlockingTrxID string
	BlockingQuery string
}

// LockWaits reports current lock waits via performance_schema.data_lock_waits
// (the 8.0 replacement for the removed innodb_lock_waits table), joined
// against processlist for readable query text.
func (s *Store) LockWaits(ctx context.Context) ([]LockWaitRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT
			w.REQUESTING_ENGINE_TRANSACTION_ID, r.THREAD_ID,
			COALESCE(pw.INFO, ''),
			w.BLOCKING_ENGINE_TRANSACTION_ID,
			COALESCE(pb.INFO, '')
		FROM performance_schema.data_lock_waits w
		JOIN performance_schema.threads r ON r.THREAD_ID = w.REQUESTING_THREAD_ID
		LEFT JOIN information_schema.PROCESSLIST pw ON pw.ID = r.PROCESSLIST_ID
		JOIN performance_schema.threads b ON b.THREAD_ID = w.BLOCKING_THREAD_ID
		LEFT JOIN information_schema.PROCESSLIST pb ON pb.ID = b.PROCESSLIST_ID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LockWaitRow
	for rows.Next() {
		var l LockWaitRow
		if rows.Scan(&l.WaitingTrxID, &l.WaitingThread, &l.WaitingQuery, &l.BlockingTrxID, &l.BlockingQuery) == nil {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

type TableSizeRow struct {
	Table     string
	Rows      int64
	DataBytes int64
	IdxBytes  int64
}

// TableSizes reports every one of this app's own tables' approximate row
// count and on-disk size (InnoDB's own estimates — TABLE_ROWS is not exact,
// same caveat as any `information_schema.TABLES` read).
func (s *Store) TableSizes(ctx context.Context) ([]TableSizeRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT TABLE_NAME, TABLE_ROWS, DATA_LENGTH, INDEX_LENGTH
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?
		ORDER BY (DATA_LENGTH + INDEX_LENGTH) DESC`, s.Schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableSizeRow
	for rows.Next() {
		var t TableSizeRow
		if rows.Scan(&t.Table, &t.Rows, &t.DataBytes, &t.IdxBytes) == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

type ExplainRow struct {
	ID           int64
	SelectType   string
	Table        string
	Type         string
	PossibleKeys string
	Key          string
	Rows         int64
	Extra        string
}

// Explain runs EXPLAIN against a learner-supplied SELECT — the one place
// this app runs a query it didn't write itself, so it's restricted to
// EXPLAIN's own output only (EXPLAIN never executes the inner statement,
// only plans it — this can't itself mutate data regardless of what the
// learner types).
func (s *Store) Explain(ctx context.Context, sqlText string) ([]ExplainRow, error) {
	rows, err := s.DB.QueryContext(ctx, "EXPLAIN "+sqlText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []ExplainRow
	for rows.Next() {
		// Scan into sql.RawBytes uniformly rather than `any` — traditional
		// EXPLAIN's numeric columns (id, rows) come back from the driver as
		// int64 in some cases and as text in others depending on version/
		// protocol quirks; scanning everything as raw text and parsing
		// ourselves sidesteps needing to track every possible driver type.
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		out = append(out, explainRowFrom(cols, vals))
	}
	return out, rows.Err()
}

// explainRowFrom maps EXPLAIN's columns by name rather than fixed position
// — the traditional (non-JSON, non-tree) EXPLAIN format's column set is
// stable within 8.0 but this is cheap insurance against a minor-version
// difference reordering them.
func explainRowFrom(cols []string, vals []sql.RawBytes) ExplainRow {
	get := func(name string) string {
		for i, c := range cols {
			if c == name {
				return string(vals[i])
			}
		}
		return ""
	}
	toInt := func(s string) int64 {
		i, _ := strconv.ParseInt(s, 10, 64)
		return i
	}
	return ExplainRow{
		ID:           toInt(get("id")),
		SelectType:   get("select_type"),
		Table:        get("table"),
		Type:         get("type"),
		PossibleKeys: get("possible_keys"),
		Key:          get("key"),
		Rows:         toInt(get("rows")),
		Extra:        get("Extra"),
	}
}
