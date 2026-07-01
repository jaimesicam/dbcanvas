package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Roles and statuses.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusDisabled = "disabled"
)

// ErrUserExists is returned when a username is already taken.
var ErrUserExists = errors.New("username already exists")

// User is the JSON-facing representation of an account (never includes the hash).
type User struct {
	ID         int64   `json:"id"`
	Username   string  `json:"username"`
	Role       string  `json:"role"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"createdAt"`
	ApprovedAt *string `json:"approvedAt,omitempty"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (and migrates) the SQLite database at path.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Pure-Go SQLite is happiest with a single connection; this also avoids
	// "database is locked" under WAL with concurrent writers.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA foreign_keys=ON;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}

	schema := `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL,
  status        TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  approved_at   TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stacks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  ttl         TEXT NOT NULL,        -- '2h'|'4h'|'8h'|'24h'|'2w'|'infinity'
  status      TEXT NOT NULL,        -- 'draft'|'deployed'|'expired'
  created_at  TEXT NOT NULL,        -- RFC3339
  expires_at  TEXT,                 -- RFC3339, NULL for infinity
  design_json TEXT NOT NULL         -- canvas: {nodes,edges,view}
);
CREATE TABLE IF NOT EXISTS deployments (
  stack_id     INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  node_id      TEXT NOT NULL,
  container_id TEXT,
  state        TEXT NOT NULL,       -- pending|provisioning|running|stopped|error
  config_json  TEXT,
  secrets_json TEXT,
  PRIMARY KEY (stack_id, node_id)
);
CREATE TABLE IF NOT EXISTS notifications (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL,       -- owner (0 = admin-only broadcast)
  scope      TEXT NOT NULL,          -- 'user'|'admin'
  type       TEXT NOT NULL,          -- e.g. node.error, stack.deployed, datagen.done
  severity   TEXT NOT NULL,          -- info|success|warning|error
  title      TEXT NOT NULL,
  body       TEXT,
  stack_id   INTEGER,
  node_id    TEXT,
  job_id     TEXT,
  read_at    TEXT,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, id DESC);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	// Best-effort migrations for columns added after initial release (ignore the
	// "duplicate column name" error when they already exist).
	db.Exec("ALTER TABLE deployments ADD COLUMN progress_json TEXT")

	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// scanUser scans a user row, mapping the nullable approved_at column.
func scanUser(row interface {
	Scan(dest ...any) error
}) (User, error) {
	var u User
	var approved sql.NullString
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Status, &u.CreatedAt, &approved); err != nil {
		return User{}, err
	}
	if approved.Valid {
		u.ApprovedAt = &approved.String
	}
	return u, nil
}

const userCols = "id, username, role, status, created_at, approved_at"

// CountUsers returns the total number of user accounts.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// CreateUser inserts a new user. When status is approved, approved_at is set to now.
func (s *Store) CreateUser(username, hash, role, status string) (User, error) {
	created := nowRFC3339()
	var approved sql.NullString
	if status == StatusApproved {
		approved = sql.NullString{String: created, Valid: true}
	}
	res, err := s.db.Exec(
		"INSERT INTO users (username, password_hash, role, status, created_at, approved_at) VALUES (?,?,?,?,?,?)",
		username, hash, role, status, created, approved,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return s.GetUser(id)
}

// GetUser fetches a single user by id.
func (s *Store) GetUser(id int64) (User, error) {
	row := s.db.QueryRow("SELECT "+userCols+" FROM users WHERE id = ?", id)
	return scanUser(row)
}

// CredByUsername returns the user plus the stored password hash.
func (s *Store) CredByUsername(username string) (User, string, error) {
	row := s.db.QueryRow(
		"SELECT id, username, role, status, created_at, approved_at, password_hash FROM users WHERE username = ?",
		username,
	)
	var u User
	var approved sql.NullString
	var hash string
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &u.Status, &u.CreatedAt, &approved, &hash); err != nil {
		return User{}, "", err
	}
	if approved.Valid {
		u.ApprovedAt = &approved.String
	}
	return u, hash, nil
}

// ListUsers returns all users, pending first, then newest first.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		"SELECT " + userCols + " FROM users " +
			"ORDER BY (status = 'pending') DESC, created_at DESC, id DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetStatus updates a user's status; approving sets approved_at to now.
func (s *Store) SetStatus(id int64, status string) (User, error) {
	if status == StatusApproved {
		if _, err := s.db.Exec(
			"UPDATE users SET status = ?, approved_at = ? WHERE id = ?",
			status, nowRFC3339(), id,
		); err != nil {
			return User{}, err
		}
	} else {
		if _, err := s.db.Exec("UPDATE users SET status = ? WHERE id = ?", status, id); err != nil {
			return User{}, err
		}
	}
	return s.GetUser(id)
}

// DeleteUser removes a user (cascading to their sessions).
func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec("DELETE FROM users WHERE id = ?", id)
	return err
}

// CreateSession stores a session token for a user with an expiry.
func (s *Store) CreateSession(token string, userID int64, expires time.Time) error {
	_, err := s.db.Exec(
		"INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)",
		token, userID, expires.UTC().Format(time.RFC3339),
	)
	return err
}

// SessionUser returns the user for a valid, unexpired token. Expired tokens are
// deleted and treated as missing.
func (s *Store) SessionUser(token string) (User, error) {
	var userID int64
	var expiresStr string
	err := s.db.QueryRow(
		"SELECT user_id, expires_at FROM sessions WHERE token = ?", token,
	).Scan(&userID, &expiresStr)
	if err != nil {
		return User{}, err
	}
	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil || time.Now().After(expires) {
		s.DeleteSession(token)
		return User{}, sql.ErrNoRows
	}
	return s.GetUser(userID)
}

// DeleteSession removes a single session token.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
	return err
}

// DeleteUserSessions removes all sessions for a user (used to revoke access).
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// --- stacks ---

// Stack statuses.
const (
	StackDraft    = "draft"
	StackDeployed = "deployed"
	StackExpired  = "expired"
)

// Deployment states.
const (
	DeployPending      = "pending"
	DeployProvisioning = "provisioning"
	DeployRunning      = "running"
	DeployStopped      = "stopped"
	DeployError        = "error"
)

// Stack is a designed (and possibly deployed) collection of nodes.
type Stack struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	OwnerID   int64           `json:"ownerId"`
	TTL       string          `json:"ttl"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"createdAt"`
	ExpiresAt *string         `json:"expiresAt,omitempty"`
	Design    json.RawMessage `json:"design,omitempty"`
}

// Deployment is the runtime record for one node in a stack.
type Deployment struct {
	StackID     int64           `json:"stackId"`
	NodeID      string          `json:"nodeId"`
	ContainerID string          `json:"containerId,omitempty"`
	State       string          `json:"state"`
	Config      json.RawMessage `json:"config,omitempty"`
	Secrets     json.RawMessage `json:"secrets,omitempty"`
	Progress    json.RawMessage `json:"progress,omitempty"`
}

// CreateStack inserts a new stack. expiresAt is nil for an infinite TTL.
func (s *Store) CreateStack(name string, ownerID int64, ttl string, expiresAt *string, design []byte) (Stack, error) {
	created := nowRFC3339()
	res, err := s.db.Exec(
		"INSERT INTO stacks (name, owner_id, ttl, status, created_at, expires_at, design_json) VALUES (?,?,?,?,?,?,?)",
		name, ownerID, ttl, StackDraft, created, nullStr(expiresAt), string(design),
	)
	if err != nil {
		return Stack{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Stack{}, err
	}
	return s.GetStack(id)
}

// ListStacks returns stacks visible to the user (own stacks; all when admin),
// newest first. Design JSON is omitted to keep the list light.
func (s *Store) ListStacks(ownerID int64, isAdmin bool) ([]Stack, error) {
	var rows *sql.Rows
	var err error
	if isAdmin {
		rows, err = s.db.Query("SELECT id, name, owner_id, ttl, status, created_at, expires_at FROM stacks ORDER BY created_at DESC, id DESC")
	} else {
		rows, err = s.db.Query("SELECT id, name, owner_id, ttl, status, created_at, expires_at FROM stacks WHERE owner_id = ? ORDER BY created_at DESC, id DESC", ownerID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stacks := []Stack{}
	for rows.Next() {
		var st Stack
		var exp sql.NullString
		if err := rows.Scan(&st.ID, &st.Name, &st.OwnerID, &st.TTL, &st.Status, &st.CreatedAt, &exp); err != nil {
			return nil, err
		}
		if exp.Valid {
			st.ExpiresAt = &exp.String
		}
		stacks = append(stacks, st)
	}
	return stacks, rows.Err()
}

// GetStack returns a single stack including its design JSON.
func (s *Store) GetStack(id int64) (Stack, error) {
	var st Stack
	var exp sql.NullString
	var design string
	err := s.db.QueryRow(
		"SELECT id, name, owner_id, ttl, status, created_at, expires_at, design_json FROM stacks WHERE id = ?", id,
	).Scan(&st.ID, &st.Name, &st.OwnerID, &st.TTL, &st.Status, &st.CreatedAt, &exp, &design)
	if err != nil {
		return Stack{}, err
	}
	if exp.Valid {
		st.ExpiresAt = &exp.String
	}
	st.Design = json.RawMessage(design)
	return st, nil
}

// UpdateStack updates a stack's name and design.
func (s *Store) UpdateStack(id int64, name string, design []byte) error {
	_, err := s.db.Exec("UPDATE stacks SET name = ?, design_json = ? WHERE id = ?", name, string(design), id)
	return err
}

// SetStackStatus updates a stack's lifecycle status.
func (s *Store) SetStackStatus(id int64, status string) error {
	_, err := s.db.Exec("UPDATE stacks SET status = ? WHERE id = ?", status, id)
	return err
}

// DeleteStack removes a stack (cascading to its deployments).
func (s *Store) DeleteStack(id int64) error {
	_, err := s.db.Exec("DELETE FROM stacks WHERE id = ?", id)
	return err
}

// ListExpiredStacks returns non-expired stacks whose expiry has passed.
func (s *Store) ListExpiredStacks() ([]Stack, error) {
	rows, err := s.db.Query(
		"SELECT id, name, owner_id, ttl, status, created_at, expires_at FROM stacks "+
			"WHERE expires_at IS NOT NULL AND expires_at < ? AND status != ?",
		nowRFC3339(), StackExpired,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stacks := []Stack{}
	for rows.Next() {
		var st Stack
		var exp sql.NullString
		if err := rows.Scan(&st.ID, &st.Name, &st.OwnerID, &st.TTL, &st.Status, &st.CreatedAt, &exp); err != nil {
			return nil, err
		}
		if exp.Valid {
			st.ExpiresAt = &exp.String
		}
		stacks = append(stacks, st)
	}
	return stacks, rows.Err()
}

// --- deployments ---

// UpsertDeployment inserts or updates a node's runtime record.
func (s *Store) UpsertDeployment(d Deployment) error {
	_, err := s.db.Exec(
		`INSERT INTO deployments (stack_id, node_id, container_id, state, config_json, secrets_json)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(stack_id, node_id) DO UPDATE SET
		   container_id=excluded.container_id, state=excluded.state,
		   config_json=excluded.config_json, secrets_json=excluded.secrets_json`,
		d.StackID, d.NodeID, nullStr(strPtr(d.ContainerID)), d.State,
		nullRaw(d.Config), nullRaw(d.Secrets),
	)
	return err
}

// SetDeploymentState updates just the state of a node deployment.
func (s *Store) SetDeploymentState(stackID int64, nodeID, state string) error {
	_, err := s.db.Exec("UPDATE deployments SET state = ? WHERE stack_id = ? AND node_id = ?", state, stackID, nodeID)
	return err
}

// SetDeploymentProgress updates just the provisioning progress JSON.
func (s *Store) SetDeploymentProgress(stackID int64, nodeID string, progress []byte) error {
	_, err := s.db.Exec("UPDATE deployments SET progress_json = ? WHERE stack_id = ? AND node_id = ?", nullRaw(progress), stackID, nodeID)
	return err
}

// ListDeployments returns all node deployments for a stack.
func (s *Store) ListDeployments(stackID int64) ([]Deployment, error) {
	rows, err := s.db.Query(
		"SELECT stack_id, node_id, container_id, state, config_json, secrets_json, progress_json FROM deployments WHERE stack_id = ?", stackID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		var cid, cfg, sec, prog sql.NullString
		if err := rows.Scan(&d.StackID, &d.NodeID, &cid, &d.State, &cfg, &sec, &prog); err != nil {
			return nil, err
		}
		d.ContainerID = cid.String
		if cfg.Valid {
			d.Config = json.RawMessage(cfg.String)
		}
		if sec.Valid {
			d.Secrets = json.RawMessage(sec.String)
		}
		if prog.Valid {
			d.Progress = json.RawMessage(prog.String)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDeployment returns one node's deployment record.
func (s *Store) GetDeployment(stackID int64, nodeID string) (Deployment, error) {
	var d Deployment
	var cid, cfg, sec, prog sql.NullString
	err := s.db.QueryRow(
		"SELECT stack_id, node_id, container_id, state, config_json, secrets_json, progress_json FROM deployments WHERE stack_id = ? AND node_id = ?",
		stackID, nodeID,
	).Scan(&d.StackID, &d.NodeID, &cid, &d.State, &cfg, &sec, &prog)
	if err != nil {
		return Deployment{}, err
	}
	d.ContainerID = cid.String
	if cfg.Valid {
		d.Config = json.RawMessage(cfg.String)
	}
	if sec.Valid {
		d.Secrets = json.RawMessage(sec.String)
	}
	if prog.Valid {
		d.Progress = json.RawMessage(prog.String)
	}
	return d, nil
}

// DeleteDeployment removes one node's deployment record.
func (s *Store) DeleteDeployment(stackID int64, nodeID string) error {
	_, err := s.db.Exec("DELETE FROM deployments WHERE stack_id = ? AND node_id = ?", stackID, nodeID)
	return err
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullRaw(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}
