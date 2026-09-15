package main

// Database Explorer — query history and saved queries.
//
// Both live in DBCanvas's own SQLite, per user, and both record what was run and
// never what it was run as. There is no credential in either table and no column
// that could hold one: a history row names a connection by its id and by the label
// the tree showed, so re-running an entry re-resolves the connection through the same
// authorization path as any other request. An entry that outlives its stack simply
// stops resolving, which is the correct behaviour rather than a bug — the alternative
// would be a history that remembered how to reach a database after the permission to
// do so had gone.

import (
	"strings"
	"time"
)

// dexHistoryKeep is how many entries a user keeps. Old ones are pruned on write, so
// the table cannot grow without bound on a long-lived installation.
const dexHistoryKeep = 500

type dexHistoryEntry struct {
	ID           int64   `json:"id"`
	At           string  `json:"at"`
	ConnectionID string  `json:"connectionId"`
	Connection   string  `json:"connection"`
	Engine       string  `json:"engine"`
	Database     string  `json:"database"`
	Schema       string  `json:"schema,omitempty"`
	Statement    string  `json:"statement"`
	DurationMs   float64 `json:"durationMs"`
	RowCount     int     `json:"rowCount"`
	AffectedRows int64   `json:"affectedRows"`
	Success      bool    `json:"success"`
	Error        string  `json:"error,omitempty"`
}

type dexSavedQuery struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Statement   string `json:"statement"`
	Description string `json:"description,omitempty"`
	Database    string `json:"database,omitempty"`
	CreatedAt   string `json:"createdAt"`
}

// dexEnsureTables creates the two tables. It runs on first use rather than in
// OpenStore so that this feature adds nothing to the startup path of an installation
// that never opens it, and CREATE TABLE IF NOT EXISTS makes it idempotent.
//
// The "already done" flag is per *Store and not package-level, which matters even
// though production has exactly one store: a package-level flag makes the second
// store in a process (every test after the first) skip the creation the first one
// did, and then fail on a table that was never made in *its* database.
func (s *Store) dexEnsureTables() error {
	if s.dexReady.Load() {
		return nil
	}
	const schema = `
CREATE TABLE IF NOT EXISTS dex_history (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  at            TEXT NOT NULL,
  connection_id TEXT NOT NULL,   -- re-resolved on rerun; never a set of credentials
  connection    TEXT NOT NULL,   -- the label the tree showed, for reading the list
  engine        TEXT NOT NULL,
  database      TEXT,
  schema_name   TEXT,
  statement     TEXT NOT NULL,
  duration_ms   REAL NOT NULL DEFAULT 0,
  row_count     INTEGER NOT NULL DEFAULT 0,
  affected_rows INTEGER NOT NULL DEFAULT 0,
  success       INTEGER NOT NULL DEFAULT 1,
  error         TEXT
);
CREATE INDEX IF NOT EXISTS idx_dex_history_user ON dex_history(user_id, id DESC);
CREATE TABLE IF NOT EXISTS dex_saved (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  engine      TEXT NOT NULL,
  statement   TEXT NOT NULL,
  description TEXT,
  database    TEXT,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dex_saved_user ON dex_saved(user_id, id DESC);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	s.dexReady.Store(true)
	return nil
}

// dexMaxStatement bounds what is stored. A pasted statement can be megabytes, and a
// history list is for finding something again, not for archiving it.
const dexMaxStatement = 64 << 10

func (s *Store) DexAddHistory(userID int64, e dexHistoryEntry) (int64, error) {
	if err := s.dexEnsureTables(); err != nil {
		return 0, err
	}
	stmt := e.Statement
	if len(stmt) > dexMaxStatement {
		stmt = stmt[:dexMaxStatement] + "\n-- (truncated by DBCanvas for storage)"
	}
	res, err := s.db.Exec(`INSERT INTO dex_history
	  (user_id, at, connection_id, connection, engine, database, schema_name, statement,
	   duration_ms, row_count, affected_rows, success, error)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		userID, time.Now().UTC().Format(time.RFC3339), e.ConnectionID, e.Connection, e.Engine,
		e.Database, e.Schema, stmt, e.DurationMs, e.RowCount, e.AffectedRows,
		boolToInt(e.Success), nullIfEmpty(e.Error))
	if err != nil {
		return 0, err
	}
	// Prune to the retention limit, oldest first.
	s.db.Exec(`DELETE FROM dex_history WHERE user_id = ? AND id NOT IN
	  (SELECT id FROM dex_history WHERE user_id = ? ORDER BY id DESC LIMIT ?)`,
		userID, userID, dexHistoryKeep)
	return res.LastInsertId()
}

func (s *Store) DexHistory(userID int64, limit int) ([]dexHistoryEntry, error) {
	if err := s.dexEnsureTables(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > dexHistoryKeep {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, at, connection_id, connection, engine,
	    COALESCE(database,''), COALESCE(schema_name,''), statement, duration_ms,
	    row_count, affected_rows, success, COALESCE(error,'')
	  FROM dex_history WHERE user_id = ? ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dexHistoryEntry{}
	for rows.Next() {
		var e dexHistoryEntry
		var ok int
		if err := rows.Scan(&e.ID, &e.At, &e.ConnectionID, &e.Connection, &e.Engine,
			&e.Database, &e.Schema, &e.Statement, &e.DurationMs, &e.RowCount,
			&e.AffectedRows, &ok, &e.Error); err != nil {
			return nil, err
		}
		e.Success = ok != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DexDeleteHistory(userID, id int64) error {
	if err := s.dexEnsureTables(); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM dex_history WHERE user_id = ? AND id = ?", userID, id)
	return err
}

func (s *Store) DexClearHistory(userID int64) error {
	if err := s.dexEnsureTables(); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM dex_history WHERE user_id = ?", userID)
	return err
}

func (s *Store) DexSaveQuery(userID int64, q dexSavedQuery) (dexSavedQuery, error) {
	if err := s.dexEnsureTables(); err != nil {
		return q, err
	}
	q.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`INSERT INTO dex_saved (user_id, name, engine, statement, description, database, created_at)
	  VALUES (?,?,?,?,?,?,?)`, userID, q.Name, q.Engine, q.Statement, q.Description, q.Database, q.CreatedAt)
	if err != nil {
		return q, err
	}
	q.ID, _ = res.LastInsertId()
	return q, nil
}

func (s *Store) DexSavedQueries(userID int64) ([]dexSavedQuery, error) {
	if err := s.dexEnsureTables(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, name, engine, statement, COALESCE(description,''),
	  COALESCE(database,''), created_at FROM dex_saved WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dexSavedQuery{}
	for rows.Next() {
		var q dexSavedQuery
		if err := rows.Scan(&q.ID, &q.Name, &q.Engine, &q.Statement, &q.Description, &q.Database, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *Store) DexDeleteSaved(userID, id int64) error {
	if err := s.dexEnsureTables(); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM dex_saved WHERE user_id = ? AND id = ?", userID, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// dexRecordHistory writes one entry for a finished submission. It is best-effort: a
// history write that failed must not turn a successful query into an error response,
// because the result is the thing the user asked for.
func (a *App) dexRecordHistory(u User, t dexTarget, req dexQueryRequest, res dexResult, statement string) {
	e := dexHistoryEntry{
		ConnectionID: t.Conn.ID, Connection: t.dexSummaryLine(), Engine: t.Conn.Engine,
		Database: req.Database, Schema: req.Schema, Statement: strings.TrimSpace(statement),
		DurationMs: res.DurationMs, Success: res.Error == nil,
	}
	for _, set := range res.Sets {
		e.RowCount += set.RowCount
		e.AffectedRows += set.AffectedRows
	}
	if res.Error != nil {
		e.Error = res.Error.Display
		if e.Error == "" {
			e.Error = res.Error.Message
		}
	}
	if e.Statement == "" {
		return
	}
	a.store.DexAddHistory(u.ID, e)
}

// dexStatementOf is what a submission is recorded as, per engine: the SQL for a SQL
// engine, the shell form for MongoDB, the command line for Valkey.
func dexStatementOf(req dexQueryRequest) string {
	if s := strings.TrimSpace(req.SQL); s != "" {
		return s
	}
	if s := strings.TrimSpace(req.Command); s != "" {
		return s
	}
	if req.Mongo.Collection != "" {
		if req.Mongo.Operation == "" && len(req.Mongo.Filter) == 0 && len(req.Mongo.Pipeline) == 0 {
			return "" // a key/collection open, not a statement anybody typed
		}
		return dexMongoStatement(req)
	}
	return ""
}
