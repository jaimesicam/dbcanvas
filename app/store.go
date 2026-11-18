package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"dbcanvas/internal/seal"
)

// ErrStackNameTaken is returned when a stack of that name already exists. Names are
// compared case-insensitively, as the CLI resolves them, so "Lab" and "lab" are one name.
var ErrStackNameTaken = errors.New("stack name already in use")

// User is the one account: the person who runs dbcanvas, created on first start and
// named after the OS login (ctlsock.go). It has no password; a browser signs in by start
// link (soloweb.go).
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	CreatedAt string `json:"createdAt"`
	// The profile (profile.go): names and email sealed at rest, avatar an id from a fixed set.
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
	Avatar    string `json:"avatar"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
	// dexReady records that the Database Explorer's two tables have been created in
	// *this* database. They are made on first use rather than at startup, so an
	// installation that never opens the page pays nothing for it. Per-store rather
	// than package-level: see dexEnsureTables.
	dexReady atomic.Bool
	// seal encrypts the credential columns (encryption.go, internal/seal).
	seal *seal.Sealer
}

// schemaVersion is the user_version of a database this release's schema made.
const schemaVersion = 1

// OpenStore opens (creating it if new) the SQLite database at path, with the encryption
// key that belongs to it (see encryption.go).
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

	// The schema is created whole on a new database; there is no upgrade path from an
	// older one. user_version marks a database this schema made, so one from an earlier
	// release is refused with a message rather than failing on a missing column.
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	var tables int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'").Scan(&tables)
	if tables > 0 && version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("%s was made by an earlier release of DBCanvas and cannot be opened by this one — move its data folder aside to start fresh", path)
	}

	schema := `
-- The one account (ctlsock.go's ensureOwner). Names and email are sealed (profile.go).
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  settings_json TEXT,
  first_name    TEXT NOT NULL DEFAULT '',
  last_name     TEXT NOT NULL DEFAULT '',
  avatar        TEXT NOT NULL DEFAULT '',
  email         TEXT NOT NULL DEFAULT ''
);
-- Browser sessions, by the SHA-256 of the cookie.
CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stacks (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  name             TEXT NOT NULL,
  ttl              TEXT NOT NULL,        -- '2h'|'4h'|'8h'|'24h'|'2w'|'30d'
  status           TEXT NOT NULL,        -- 'draft'|'deployed'|'expired'
  created_at       TEXT NOT NULL,        -- RFC3339
  expires_at       TEXT,                 -- RFC3339
  design_json      TEXT NOT NULL,        -- canvas: {nodes,edges,view}
  backend          TEXT,
  labels_json      TEXT,                 -- stacklabels.go
  created_by_token INTEGER               -- tokenlimit.go
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_stacks_name ON stacks(name COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS deployments (
  stack_id      INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  node_id       TEXT NOT NULL,
  container_id  TEXT,
  state         TEXT NOT NULL,       -- pending|provisioning|running|stopped|error
  config_json   TEXT,
  secrets_json  TEXT,
  progress_json TEXT,
  PRIMARY KEY (stack_id, node_id)
);
-- What is left of a deleted stack, for 30 days (stackevents.go).
CREATE TABLE IF NOT EXISTS stack_tombstones (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  labels_json TEXT,
  deleted_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notifications (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
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
-- pt-stalk archives kept by capture time. The tarball itself lives on disk under
-- the data directory (they are megabytes each, and a blob column would bloat
-- every backup of this database); this table is the index over them.
--
-- Deliberately NOT keyed to the deployment: a node can be recreated, and the
-- whole point of keeping captures is to compare across the change that recreated
-- it. Rows outlive the container they came from, and survive the node entirely.
CREATE TABLE IF NOT EXISTS ptstalk_archives (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  stack_id    INTEGER NOT NULL,
  node_id     TEXT NOT NULL,
  host        TEXT NOT NULL DEFAULT '',
  captured_at TEXT NOT NULL,
  size_bytes  INTEGER NOT NULL DEFAULT 0,
  note        TEXT NOT NULL DEFAULT '',
  path        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ptstalk_node ON ptstalk_archives(stack_id, node_id, captured_at DESC);
-- Kept pt-k8s-debug-collector captures, the K3D equivalent of the table above and
-- kept for the same reason: a cluster-dump is the evidence of a moment, and the
-- useful thing to do with one is compare it against the moment before. Keyed to
-- the *frame* rather than a node, because the capture is of the whole cluster —
-- the k3s node it was taken through is an implementation detail, and a cluster
-- outlives any one of its nodes.
CREATE TABLE IF NOT EXISTS k8s_dumps (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  stack_id    INTEGER NOT NULL,
  frame_id    TEXT NOT NULL,
  cluster     TEXT NOT NULL DEFAULT '',
  operator    TEXT NOT NULL DEFAULT '',
  captured_at TEXT NOT NULL,
  size_bytes  INTEGER NOT NULL DEFAULT 0,
  note        TEXT NOT NULL DEFAULT '',
  path        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_k8sdump_frame ON k8s_dumps(stack_id, frame_id, captured_at DESC);
-- Saved deployment templates: a reusable canvas design, detached from any one stack.
-- Only user-saved templates live here; the built-in defaults are Go literals with
-- "builtin:" ids (see templates_builtin.go).
--
-- design_json is sanitized on the way in (see sanitizeTemplateDesign) — no secrets,
-- no host paths, no fixed host ports — because a template is the one design document
-- meant to be copied between stacks and exported to a file.
CREATE TABLE IF NOT EXISTS stack_templates (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  category    TEXT NOT NULL DEFAULT '',
  design_json TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
-- Settings, one row per key so a new knob needs no schema change; see syssettings.go
-- for the typed view over it. users.settings_json holds the per-browser preferences.
CREATE TABLE IF NOT EXISTS app_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
-- API tokens: a revocable, expiring bearer credential that stands in for a password
-- for a script that calls the API over HTTP. See apitokens.go.
--
-- token_hash is SHA-256, not bcrypt, and that is deliberate. bcrypt exists to make a
-- LOW-entropy secret expensive to guess; a token here is 32 bytes from crypto/rand,
-- so there is nothing to guess. What it does have that a password does not is a
-- verification on EVERY request, and bcrypt at its default cost would put ~100 ms on
-- all of them. The secret itself is never stored, so a copy of this database still
-- yields no usable token.
--
-- Expired and revoked rows are kept (and reaped only after 30 days) because "why did
-- my script start getting 401s" is answered by the row, not by its absence.
CREATE TABLE IF NOT EXISTS api_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT NOT NULL,        -- what it is for: "a backup script"
  token_hash   TEXT NOT NULL,        -- hex SHA-256 of the secret
  prefix       TEXT NOT NULL,        -- "dbc_a1b2c3d4", the only displayable part
  scope        TEXT NOT NULL,        -- read | write | admin
  created_at   TEXT NOT NULL,
  expires_at   TEXT,                 -- RFC3339; NULL = never
  last_used_at TEXT,
  revoked_at   TEXT,
  limit_json   TEXT                  -- tokenlimit.go
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(token_hash);
-- The node accounts' generated passwords (nodepasswords.go), sealed.
CREATE TABLE IF NOT EXISTS node_passwords (
  name  TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`
	// The features' own tables, kept beside their code.
	for _, q := range []string{schema, shareSchema, hostPortsSchema, kanbanSchema, scratchboardSchema, watchSchema,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}

	s := &Store{db: db}
	if err := s.setupEncryption(path); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// scanUser scans a user row and unseals its profile.
func (s *Store) scanUser(row interface {
	Scan(dest ...any) error
}) (User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.CreatedAt, &u.FirstName, &u.LastName, &u.Avatar, &u.Email); err != nil {
		return User{}, err
	}
	return s.openProfile(u)
}

// openProfile unseals a scanned user's names (profile.go).
func (s *Store) openProfile(u User) (User, error) {
	var err error
	if u.FirstName, err = s.openVal(aadID("users", "first_name", u.ID), u.FirstName); err != nil {
		return User{}, err
	}
	if u.LastName, err = s.openVal(aadID("users", "last_name", u.ID), u.LastName); err != nil {
		return User{}, err
	}
	if u.Email, err = s.openVal(aadID("users", "email", u.ID), u.Email); err != nil {
		return User{}, err
	}
	return u, nil
}

const userCols = "id, username, created_at, first_name, last_name, avatar, email"

// CreateUser inserts the account. There is only ever one (ensureOwner).
func (s *Store) CreateUser(username string) (User, error) {
	res, err := s.db.Exec("INSERT INTO users (username, created_at) VALUES (?,?)", username, nowRFC3339())
	if err != nil {
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
	return s.scanUser(row)
}

// Owner returns the account; sql.ErrNoRows before it is created.
func (s *Store) Owner() (User, error) {
	return s.scanUser(s.db.QueryRow("SELECT " + userCols + " FROM users ORDER BY id LIMIT 1"))
}

// UserSettings returns a user's raw settings JSON ("" when they have never saved any).
func (s *Store) UserSettings(id int64) (string, error) {
	var js sql.NullString
	if err := s.db.QueryRow("SELECT settings_json FROM users WHERE id = ?", id).Scan(&js); err != nil {
		return "", err
	}
	return js.String, nil
}

// SetUserSettings replaces a user's settings JSON.
func (s *Store) SetUserSettings(id int64, js string) error {
	_, err := s.db.Exec("UPDATE users SET settings_json = ? WHERE id = ?", js, id)
	return err
}

// AppSetting returns one instance-wide setting, or "" when it has never been
// set (the caller applies its own default — an absent row is not an error).
func (s *Store) AppSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetAppSetting writes one instance-wide setting.
func (s *Store) SetAppSetting(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO app_settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

// AppSettingKeys lists the setting keys that start with prefix.
func (s *Store) AppSettingKeys(prefix string) ([]string, error) {
	rows, err := s.db.Query("SELECT key FROM app_settings WHERE key LIKE ? ESCAPE '\\'", strings.NewReplacer("%", "\\%", "_", "\\_").Replace(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// DeleteAppSetting removes one instance-wide setting.
func (s *Store) DeleteAppSetting(key string) error {
	_, err := s.db.Exec("DELETE FROM app_settings WHERE key = ?", key)
	return err
}

// CreateSession stores a browser session with an expiry. Only its hash is kept, so a
// copy of the database signs nobody in; every lookup hashes the cookie.
func (s *Store) CreateSession(token string, expires time.Time) error {
	_, err := s.db.Exec("INSERT INTO sessions (token, expires_at) VALUES (?,?)",
		seal.HashSecret(token), expires.UTC().Format(time.RFC3339))
	return err
}

// SessionUser returns the account for a valid, unexpired token. Expired tokens are
// deleted and treated as missing.
func (s *Store) SessionUser(token string) (User, error) {
	var expiresStr string
	if err := s.db.QueryRow("SELECT expires_at FROM sessions WHERE token = ?", seal.HashSecret(token)).Scan(&expiresStr); err != nil {
		return User{}, err
	}
	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil || time.Now().After(expires) {
		s.DeleteSession(token)
		return User{}, sql.ErrNoRows
	}
	return s.Owner()
}

// DeleteSession removes a single session token.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", seal.HashSecret(token))
	return err
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
	TTL       string          `json:"ttl"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"createdAt"`
	ExpiresAt *string         `json:"expiresAt,omitempty"`
	Design    json.RawMessage `json:"design,omitempty"`
	// Backend is the provisioning engine the stack was first deployed with
	// ("docker" | "vagrant"). Empty until the first deploy stamps it; callers
	// treat empty as "docker" (see App.eng). Only populated by GetStack.
	Backend string `json:"backend,omitempty"`
	// Labels are key=value tags a program can select stacks by (stacklabels.go).
	Labels map[string]string `json:"labels,omitempty"`
	// CreatedByToken is the API token that created the stack, 0 for any other way
	// (tokenlimit.go: a token limited to its own stacks).
	CreatedByToken int64 `json:"createdByToken,omitempty"`
}

// SetStackCreatedByToken records the token a stack was created with.
func (s *Store) SetStackCreatedByToken(id, tokenID int64) error {
	_, err := s.db.Exec("UPDATE stacks SET created_by_token = ? WHERE id = ?", tokenID, id)
	return err
}

// decodeLabels reads a labels_json column; nil when empty or unreadable.
func decodeLabels(ns sql.NullString) map[string]string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var m map[string]string
	if json.Unmarshal([]byte(ns.String), &m) != nil || len(m) == 0 {
		return nil
	}
	return m
}

// SetStackLabels replaces a stack's labels (nil or empty clears them).
func (s *Store) SetStackLabels(id int64, labels map[string]string) error {
	var v any
	if len(labels) > 0 {
		b, _ := json.Marshal(labels)
		v = string(b)
	}
	_, err := s.db.Exec("UPDATE stacks SET labels_json = ? WHERE id = ?", v, id)
	return err
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
	// ContainerName is derived, not stored: deploymentContainerName, filled on the
	// read paths so the UI can show/copy the `docker exec` command without
	// re-deriving the naming rule client-side. UpsertDeployment ignores it.
	ContainerName string `json:"containerName,omitempty"`
}

// CreateStack inserts a new stack. Stack names are unique (ErrStackNameTaken).
func (s *Store) CreateStack(name string, ttl string, expiresAt *string, design []byte) (Stack, error) {
	created := nowRFC3339()
	tx, err := s.db.Begin()
	if err != nil {
		return Stack{}, err
	}
	defer tx.Rollback()
	if taken, err := stackNameTaken(tx, name, 0); err != nil {
		return Stack{}, err
	} else if taken {
		return Stack{}, ErrStackNameTaken
	}
	res, err := tx.Exec(
		"INSERT INTO stacks (name, ttl, status, created_at, expires_at, design_json) VALUES (?,?,?,?,?,'')",
		name, ttl, StackDraft, created, nullStr(expiresAt),
	)
	if err != nil {
		return Stack{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Stack{}, err
	}
	sealed, err := s.sealVal(aadID("stacks", "design_json", id), string(design))
	if err != nil {
		return Stack{}, err
	}
	if _, err := tx.Exec("UPDATE stacks SET design_json = ? WHERE id = ?", sealed, id); err != nil {
		return Stack{}, err
	}
	if err := tx.Commit(); err != nil {
		return Stack{}, err
	}
	return s.GetStack(id)
}

// ListStacks returns the stacks, newest first. Design JSON is omitted to keep the list light.
func (s *Store) ListStacks() ([]Stack, error) {
	rows, err := s.db.Query("SELECT id, name, ttl, status, created_at, expires_at, labels_json, created_by_token FROM stacks ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stacks := []Stack{}
	for rows.Next() {
		var st Stack
		var exp, labels sql.NullString
		var byTok sql.NullInt64
		if err := rows.Scan(&st.ID, &st.Name, &st.TTL, &st.Status, &st.CreatedAt, &exp, &labels, &byTok); err != nil {
			return nil, err
		}
		if exp.Valid {
			st.ExpiresAt = &exp.String
		}
		st.Labels = decodeLabels(labels)
		st.CreatedByToken = byTok.Int64
		stacks = append(stacks, st)
	}
	return stacks, rows.Err()
}

// SetStackExpiry sets a stack's TTL label and expiry (nil: never).
func (s *Store) SetStackExpiry(id int64, ttl string, expiresAt *string) error {
	_, err := s.db.Exec("UPDATE stacks SET ttl = ?, expires_at = ? WHERE id = ?", ttl, expiresAt, id)
	return err
}

// GetStack returns a single stack including its design JSON.
func (s *Store) GetStack(id int64) (Stack, error) {
	var st Stack
	var exp sql.NullString
	var backend, labels sql.NullString
	var byTok sql.NullInt64
	var design string
	err := s.db.QueryRow(
		"SELECT id, name, ttl, status, created_at, expires_at, design_json, backend, labels_json, created_by_token FROM stacks WHERE id = ?", id,
	).Scan(&st.ID, &st.Name, &st.TTL, &st.Status, &st.CreatedAt, &exp, &design, &backend, &labels, &byTok)
	if err != nil {
		return Stack{}, err
	}
	if exp.Valid {
		st.ExpiresAt = &exp.String
	}
	st.Backend = backend.String
	st.Labels = decodeLabels(labels)
	st.CreatedByToken = byTok.Int64
	design, err = s.openVal(aadID("stacks", "design_json", st.ID), design)
	if err != nil {
		return Stack{}, err
	}
	st.Design = json.RawMessage(design)
	return st, nil
}

// SetStackBackend records the provisioning engine a stack was deployed with.
// Called once, on the first deploy, so redeploy/manage/teardown stay on the same
// engine for the stack's life.
func (s *Store) SetStackBackend(id int64, backend string) error {
	_, err := s.db.Exec("UPDATE stacks SET backend = ? WHERE id = ?", backend, id)
	return err
}

// UpdateStack updates a stack's name and design.
func (s *Store) UpdateStack(id int64, name string, design []byte) error {
	sealed, err := s.sealVal(aadID("stacks", "design_json", id), string(design))
	if err != nil {
		return err
	}
	if taken, err := stackNameTaken(s.db, name, id); err != nil {
		return err
	} else if taken {
		return ErrStackNameTaken
	}
	_, err = s.db.Exec("UPDATE stacks SET name = ?, design_json = ? WHERE id = ?", name, sealed, id)
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
	if err == nil {
		releaseStackPorts(s.db, id)
	}
	return err
}

// ListExpiredStacks returns non-expired stacks whose expiry has passed.
// ListStacksExpiringSoon returns deployed stacks whose TTL runs out within the window.
func (s *Store) ListStacksExpiringSoon(within time.Duration) ([]Stack, error) {
	now := time.Now().UTC()
	rows, err := s.db.Query(
		"SELECT id, name, ttl, status, created_at, expires_at FROM stacks "+
			"WHERE expires_at IS NOT NULL AND expires_at >= ? AND expires_at < ? AND status = ?",
		now.Format(time.RFC3339), now.Add(within).Format(time.RFC3339), StackDeployed,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stacks := []Stack{}
	for rows.Next() {
		var st Stack
		var exp sql.NullString
		if err := rows.Scan(&st.ID, &st.Name, &st.TTL, &st.Status, &st.CreatedAt, &exp); err != nil {
			return nil, err
		}
		if exp.Valid {
			st.ExpiresAt = &exp.String
		}
		stacks = append(stacks, st)
	}
	return stacks, rows.Err()
}

func (s *Store) ListExpiredStacks() ([]Stack, error) {
	rows, err := s.db.Query(
		"SELECT id, name, ttl, status, created_at, expires_at FROM stacks "+
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
		if err := rows.Scan(&st.ID, &st.Name, &st.TTL, &st.Status, &st.CreatedAt, &exp); err != nil {
			return nil, err
		}
		if exp.Valid {
			st.ExpiresAt = &exp.String
		}
		stacks = append(stacks, st)
	}
	return stacks, rows.Err()
}

// --- stack templates ---

// StackTemplate is a reusable canvas design. ID is a string on the wire because
// the picker mixes two populations: rows from this table (a decimal id) and the
// built-in defaults (an id prefixed "builtin:"). Everything downstream — apply,
// export, the frontend — treats it as an opaque handle; see templateIsBuiltin.
type StackTemplate struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Builtin     bool            `json:"builtin"`
	CreatedAt   string          `json:"createdAt,omitempty"`
	UpdatedAt   string          `json:"updatedAt,omitempty"`
	Design      json.RawMessage `json:"design,omitempty"`
	// Nodes/Frames are a summary for the picker, so the list response can stay
	// light (no design) and still say how big each template is.
	Nodes  int `json:"nodes"`
	Frames int `json:"frames"`
}

const templateCols = "id, name, description, category, created_at, updated_at"

// scanTemplate reads a row without its design (the list shape).
func scanTemplate(row interface {
	Scan(dest ...any) error
}) (StackTemplate, error) {
	var t StackTemplate
	var id int64
	if err := row.Scan(&id, &t.Name, &t.Description, &t.Category, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return StackTemplate{}, err
	}
	t.ID = strconv.FormatInt(id, 10)
	return t, nil
}

// CreateStackTemplate stores a new template and returns it (with its design).
func (s *Store) CreateStackTemplate(name, description, category string, design []byte) (StackTemplate, error) {
	now := nowRFC3339()
	res, err := s.db.Exec(
		"INSERT INTO stack_templates (name, description, category, design_json, created_at, updated_at) VALUES (?,?,?,?,?,?)",
		name, description, category, string(design), now, now,
	)
	if err != nil {
		return StackTemplate{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return StackTemplate{}, err
	}
	return s.GetStackTemplate(id)
}

// ListStackTemplates returns the saved templates, newest first. Designs are omitted to
// keep the list light.
func (s *Store) ListStackTemplates() ([]StackTemplate, error) {
	rows, err := s.db.Query("SELECT " + templateCols + " FROM stack_templates ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StackTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetStackTemplate returns one template including its design.
func (s *Store) GetStackTemplate(id int64) (StackTemplate, error) {
	var t StackTemplate
	var rid int64
	var design string
	err := s.db.QueryRow(
		"SELECT "+templateCols+", design_json FROM stack_templates WHERE id = ?", id,
	).Scan(&rid, &t.Name, &t.Description, &t.Category, &t.CreatedAt, &t.UpdatedAt, &design)
	if err != nil {
		return StackTemplate{}, err
	}
	t.ID = strconv.FormatInt(rid, 10)
	t.Design = json.RawMessage(design)
	return t, nil
}

// UpdateStackTemplate replaces a template's metadata and design.
func (s *Store) UpdateStackTemplate(id int64, name, description, category string, design []byte) error {
	_, err := s.db.Exec(
		"UPDATE stack_templates SET name = ?, description = ?, category = ?, design_json = ?, updated_at = ? WHERE id = ?",
		name, description, category, string(design), nowRFC3339(), id,
	)
	return err
}

// DeleteStackTemplate removes a template.
func (s *Store) DeleteStackTemplate(id int64) error {
	_, err := s.db.Exec("DELETE FROM stack_templates WHERE id = ?", id)
	return err
}

// --- deployments ---

// UpsertDeployment inserts or updates a node's runtime record.
func (s *Store) UpsertDeployment(d Deployment) error {
	var secrets any // NULL when there are none, as before sealing
	if len(d.Secrets) > 0 {
		v, err := s.sealVal(aadDeployment(d.StackID, d.NodeID), string(d.Secrets))
		if err != nil {
			return err
		}
		secrets = v
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments (stack_id, node_id, container_id, state, config_json, secrets_json)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(stack_id, node_id) DO UPDATE SET
		   container_id=excluded.container_id, state=excluded.state,
		   config_json=excluded.config_json, secrets_json=excluded.secrets_json`,
		d.StackID, d.NodeID, nullStr(strPtr(d.ContainerID)), d.State,
		nullRaw(d.Config), secrets,
	)
	return err
}

// SetDeploymentState updates just the state of a node deployment.
func (s *Store) SetDeploymentState(stackID int64, nodeID, state string) error {
	_, err := s.db.Exec("UPDATE deployments SET state = ? WHERE stack_id = ? AND node_id = ?", state, stackID, nodeID)
	return err
}

// InFlightDeployments returns every deployment, across all stacks, still pending or
// provisioning — at startup, the ones a previous server process was building.
func (s *Store) InFlightDeployments() ([]Deployment, error) {
	rows, err := s.db.Query("SELECT stack_id, node_id FROM deployments WHERE state IN (?, ?)",
		DeployPending, DeployProvisioning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.StackID, &d.NodeID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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
			v, err := s.openVal(aadDeployment(d.StackID, d.NodeID), sec.String)
			if err != nil {
				return nil, err
			}
			d.Secrets = json.RawMessage(v)
		}
		if prog.Valid {
			d.Progress = json.RawMessage(prog.String)
		}
		d.ContainerName = deploymentContainerName(d)
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
		v, err := s.openVal(aadDeployment(d.StackID, d.NodeID), sec.String)
		if err != nil {
			return Deployment{}, err
		}
		d.Secrets = json.RawMessage(v)
	}
	if prog.Valid {
		d.Progress = json.RawMessage(prog.String)
	}
	d.ContainerName = deploymentContainerName(d)
	return d, nil
}

// DeleteDeployment removes one node's deployment record.
func (s *Store) DeleteDeployment(stackID int64, nodeID string) error {
	_, err := s.db.Exec("DELETE FROM deployments WHERE stack_id = ? AND node_id = ?", stackID, nodeID)
	if err == nil {
		releaseNodePorts(s.db, stackID, nodeID)
	}
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

// deploymentContainerName is the name of the container a deployment runs in.
//
// Every node DBCanvas creates is containerName(StackID, NodeID). A k3s node is not: k3d
// creates it and names it k3d-<cluster>-server-0 / -agent-N (k3dNodeContainer), so the
// generic name pointed `docker exec` at a container that does not exist. Its config says
// which: Container since it was recorded, and before that the server can still be worked
// out from the cluster. An agent from before then cannot — which agent it was is not in
// its config — so it gets no name rather than a wrong one.
func deploymentContainerName(d Deployment) string {
	var k struct {
		Container  string `json:"container"`
		Cluster    string `json:"cluster"`
		Role       string `json:"role"`
		K3SVersion string `json:"k3sVersion"`
	}
	if len(d.Config) == 0 || json.Unmarshal(d.Config, &k) != nil || k.Cluster == "" || k.K3SVersion == "" {
		return containerName(d.StackID, d.NodeID)
	}
	switch {
	case k.Container != "":
		return k.Container
	case k.Role == "server":
		return k3dNodeContainer(k.Cluster, 0)
	}
	return ""
}

// queryRower is what stackNameTaken needs from a *sql.DB or a *sql.Tx.
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// stackNameTaken reports whether another stack (not exceptID) has this name, compared
// case-insensitively. The unique index on the name backs it against two racing creates.
func stackNameTaken(q queryRower, name string, exceptID int64) (bool, error) {
	var n int
	err := q.QueryRow("SELECT COUNT(*) FROM stacks WHERE name = ? COLLATE NOCASE AND id != ?",
		name, exceptID).Scan(&n)
	return n > 0, err
}

// StackIDByName is the id of the stack of this name (case-insensitive), or 0.
func (s *Store) StackIDByName(name string) int64 {
	var id int64
	s.db.QueryRow("SELECT id FROM stacks WHERE name = ? COLLATE NOCASE", name).Scan(&id)
	return id
}

// FreeStackName is base, or "base 2", "base 3", … — the first name no stack has.
func (s *Store) FreeStackName(base string) string {
	name := base
	for i := 2; s.StackIDByName(name) != 0; i++ {
		name = fmt.Sprintf("%s %d", base, i)
	}
	return name
}
