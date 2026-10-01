package main

import (
	"database/sql"
	"errors"
	"time"
)

// share_store.go — the tables behind shared sessions (see share.go).
//
// What is here is what has to outlive a process or be read back later: the link,
// who came through it and what they were let do, and the transcript. Everything live
// — who is connected, who holds control, the last canvas position, the shared
// terminals — is the session hub's, in memory (sharehub.go), and is rebuilt from
// nothing when the process restarts.

const shareSchema = `
-- One row per share link. The token is shown to the host once, inside the link;
-- only its SHA-256 is kept, so a copy of the database is not a copy of the link.
-- expires_at is at most two hours after created_at, which share.go enforces on the
-- way in; ended_at is set by End session, a revoke, or the expiry. A session covers
-- the whole application; stack_id is only the stack it was started from, if any
-- (migrateShareSessions makes it nullable in older databases). mirror is the host's
-- "Mirror everything" switch (sharemirror.go).
CREATE TABLE IF NOT EXISTS share_sessions (` + shareSessionsColumns + `);
CREATE UNIQUE INDEX IF NOT EXISTS idx_share_sessions_token ON share_sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_share_sessions_host ON share_sessions(host_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_share_sessions_stack ON share_sessions(stack_id, id DESC);

-- One row per person who came through a link. Name and email are what they typed:
-- labels, not identity. The cookie is the credential, stored hashed like the link.
-- invite_hash is the link they came through (store.go adds it to older databases): a
-- guest who left, was removed or was denied cannot come back on that same link, only
-- on a new one the host issues (share.go, handleJoin).
CREATE TABLE IF NOT EXISTS share_guests (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER NOT NULL REFERENCES share_sessions(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  email        TEXT NOT NULL,
  cookie_hash  TEXT NOT NULL,
  state        TEXT NOT NULL,         -- waiting | admitted | denied | removed | left
  muted        INTEGER NOT NULL DEFAULT 0,
  remote_addr  TEXT NOT NULL DEFAULT '',
  joined_at    TEXT NOT NULL,
  admitted_at  TEXT,
  left_at      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_share_guests_cookie ON share_guests(cookie_hash);
CREATE INDEX IF NOT EXISTS idx_share_guests_session ON share_guests(session_id, id);

-- The transcript: chat and system events in one stream, in order. author is kept
-- as written at the time, so a transcript still reads right after its guests rows
-- are gone with the session.
CREATE TABLE IF NOT EXISTS share_messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER NOT NULL REFERENCES share_sessions(id) ON DELETE CASCADE,
  author_kind  TEXT NOT NULL,         -- host | guest | system
  guest_id     INTEGER NOT NULL DEFAULT 0,
  author       TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL,         -- chat | join | leave | lobby | link | control | action | expiry-warning | end
  body         TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_share_messages_session ON share_messages(session_id, id);

-- Every change a guest made while in control: one row per mutating request,
-- written before the request runs (status 0) and completed after.
CREATE TABLE IF NOT EXISTS share_actions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER NOT NULL REFERENCES share_sessions(id) ON DELETE CASCADE,
  guest_id     INTEGER NOT NULL,
  method       TEXT NOT NULL,
  path         TEXT NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  status       INTEGER NOT NULL DEFAULT 0,
  message_id   INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_share_actions_session ON share_actions(session_id, id);`

const shareSessionsColumns = `
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  host_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  stack_id     INTEGER REFERENCES stacks(id) ON DELETE SET NULL,
  token_hash   TEXT NOT NULL,
  hide_secrets INTEGER NOT NULL DEFAULT 0,
  mirror       INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  ended_at     TEXT,
  ended_reason TEXT NOT NULL DEFAULT ''
`

// migrateShareSessions brings an older share_sessions table to the one above: a
// session was once filed on exactly one stack (stack_id NOT NULL, deleted with it),
// and had no mirror switch. SQLite cannot drop a NOT NULL, so the table is rebuilt —
// the documented way: a new table, copy, drop, rename, with foreign keys off so the
// drop does not cascade to the guests and messages that point at it.
func migrateShareSessions(db *sql.DB) error {
	db.Exec("ALTER TABLE share_sessions ADD COLUMN mirror INTEGER NOT NULL DEFAULT 0")
	var notNull int
	if err := db.QueryRow(`SELECT "notnull" FROM pragma_table_info('share_sessions') WHERE name = 'stack_id'`).Scan(&notNull); err != nil || notNull == 0 {
		return err
	}
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer db.Exec("PRAGMA foreign_keys = ON")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const cols = `id, host_id, stack_id, token_hash, hide_secrets, mirror, created_at, expires_at, ended_at, ended_reason`
	for _, q := range []string{
		`CREATE TABLE share_sessions_new (` + shareSessionsColumns + `)`,
		`INSERT INTO share_sessions_new (` + cols + `) SELECT ` + cols + ` FROM share_sessions`,
		`DROP TABLE share_sessions`,
		`ALTER TABLE share_sessions_new RENAME TO share_sessions`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	// The indexes went with the old table.
	for _, q := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_share_sessions_token ON share_sessions(token_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_share_sessions_host ON share_sessions(host_id, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_share_sessions_stack ON share_sessions(stack_id, id DESC)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ShareSession is one share link and its lifetime.
type ShareSession struct {
	ID          int64   `json:"id"`
	HostID      int64   `json:"hostId"`
	HostName    string  `json:"hostName"`
	StackID     int64   `json:"stackId"`
	StackName   string  `json:"stackName"`
	HideSecrets bool    `json:"hideSecrets"`
	Mirror      bool    `json:"mirror"`
	CreatedAt   string  `json:"createdAt"`
	ExpiresAt   string  `json:"expiresAt"`
	EndedAt     *string `json:"endedAt,omitempty"`
	EndedReason string  `json:"endedReason,omitempty"`
}

// live reports whether guests may still use the session: not ended, not expired.
func (s ShareSession) live(now time.Time) bool {
	if s.EndedAt != nil {
		return false
	}
	exp, err := time.Parse(time.RFC3339, s.ExpiresAt)
	return err == nil && now.Before(exp)
}

func (s ShareSession) expires() time.Time {
	t, _ := time.Parse(time.RFC3339, s.ExpiresAt)
	return t
}

// ShareGuest is one person who came through a link.
type ShareGuest struct {
	ID         int64   `json:"id"`
	SessionID  int64   `json:"sessionId"`
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	State      string  `json:"state"`
	Muted      bool    `json:"muted"`
	RemoteAddr string  `json:"remoteAddr,omitempty"`
	JoinedAt   string  `json:"joinedAt"`
	AdmittedAt *string `json:"admittedAt,omitempty"`
	LeftAt     *string `json:"leftAt,omitempty"`
	InviteHash string  `json:"-"`
	// UserID and Account are set when the person signed in with their DBCanvas
	// account to join, rather than typing a name: who they are is then known, not
	// claimed. Avatar is theirs either way (profile.go's ids).
	UserID  int64  `json:"userId,omitempty"`
	Account string `json:"account,omitempty"`
	Avatar  string `json:"avatar,omitempty"`
}

// Guest states.
const (
	guestWaiting  = "waiting"
	guestAdmitted = "admitted"
	guestDenied   = "denied"
	guestRemoved  = "removed"
	guestLeft     = "left"
)

// ShareMessage is one line of the transcript.
type ShareMessage struct {
	ID         int64  `json:"id"`
	SessionID  int64  `json:"sessionId"`
	AuthorKind string `json:"authorKind"`
	GuestID    int64  `json:"guestId,omitempty"`
	Author     string `json:"author"`
	Kind       string `json:"kind"`
	Body       string `json:"body"`
	CreatedAt  string `json:"createdAt"`
}

// ShareAction is one change a guest made while driving.
type ShareAction struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"sessionId"`
	GuestID   int64  `json:"guestId"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Summary   string `json:"summary"`
	Status    int    `json:"status"`
	CreatedAt string `json:"createdAt"`
}

var errShareNotFound = errors.New("shared session not found")

const shareSessionCols = `s.id, s.host_id, COALESCE(u.username,''), COALESCE(s.stack_id,0), COALESCE(st.name,''),
  s.hide_secrets, s.mirror, s.created_at, s.expires_at, s.ended_at, s.ended_reason`

const shareSessionFrom = ` FROM share_sessions s
  LEFT JOIN users u ON u.id = s.host_id
  LEFT JOIN stacks st ON st.id = s.stack_id`

func scanShareSession(row interface{ Scan(...any) error }) (ShareSession, error) {
	var s ShareSession
	var hide, mirror int
	var ended sql.NullString
	err := row.Scan(&s.ID, &s.HostID, &s.HostName, &s.StackID, &s.StackName, &hide, &mirror,
		&s.CreatedAt, &s.ExpiresAt, &ended, &s.EndedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareSession{}, errShareNotFound
	}
	if err != nil {
		return ShareSession{}, err
	}
	s.HideSecrets = hide != 0
	s.Mirror = mirror != 0
	if ended.Valid {
		s.EndedAt = &ended.String
	}
	return s, nil
}

// CreateShareSession files a new session. stackID is the stack it was started from,
// or 0 for one started from anywhere else; it covers the whole application either way.
func (s *Store) CreateShareSession(hostID, stackID int64, tokenHash string, hideSecrets, mirror bool, expires time.Time) (ShareSession, error) {
	stack := sql.NullInt64{Int64: stackID, Valid: stackID != 0}
	res, err := s.db.Exec(`INSERT INTO share_sessions (host_id, stack_id, token_hash, hide_secrets, mirror, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?)`, hostID, stack, tokenHash, boolInt(hideSecrets), boolInt(mirror), nowRFC3339(), expires.UTC().Format(time.RFC3339))
	if err != nil {
		return ShareSession{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ShareSession{}, err
	}
	return s.GetShareSession(id)
}

func (s *Store) GetShareSession(id int64) (ShareSession, error) {
	return scanShareSession(s.db.QueryRow(`SELECT `+shareSessionCols+shareSessionFrom+` WHERE s.id = ?`, id))
}

func (s *Store) ShareSessionByToken(tokenHash string) (ShareSession, error) {
	return scanShareSession(s.db.QueryRow(`SELECT `+shareSessionCols+shareSessionFrom+` WHERE s.token_hash = ?`, tokenHash))
}

func (s *Store) listShareSessions(where string, arg any) ([]ShareSession, error) {
	rows, err := s.db.Query(`SELECT `+shareSessionCols+shareSessionFrom+` WHERE `+where+` ORDER BY s.id DESC LIMIT 200`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareSession{}
	for rows.Next() {
		ss, err := scanShareSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// SetShareSessionMirror turns a live session's "Mirror everything" on or off.
func (s *Store) SetShareSessionMirror(id int64, on bool) error {
	res, err := s.db.Exec(`UPDATE share_sessions SET mirror = ? WHERE id = ? AND ended_at IS NULL`, boolInt(on), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errShareNotFound
	}
	return nil
}

// SetShareSessionToken replaces a live session's link. The old one stops opening the
// join page; guests already through it keep their cookies.
func (s *Store) SetShareSessionToken(id int64, tokenHash string) error {
	res, err := s.db.Exec(`UPDATE share_sessions SET token_hash = ? WHERE id = ? AND ended_at IS NULL`, tokenHash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errShareNotFound
	}
	return nil
}

// ShareSessionTokenHash is the hash of a session's current link.
func (s *Store) ShareSessionTokenHash(id int64) (string, error) {
	var h string
	err := s.db.QueryRow(`SELECT token_hash FROM share_sessions WHERE id = ?`, id).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errShareNotFound
	}
	return h, err
}

// ListShareSessions is a host's sessions, newest first.
func (s *Store) ListShareSessions(hostID int64) ([]ShareSession, error) {
	return s.listShareSessions("s.host_id = ?", hostID)
}

// ListStackShareSessions is every session filed on a stack — its transcripts.
func (s *Store) ListStackShareSessions(stackID int64) ([]ShareSession, error) {
	return s.listShareSessions("s.stack_id = ?", stackID)
}

// EndShareSession marks a session ended. Ending one that already ended keeps the
// first reason: "expired" should not be overwritten by a late End click.
func (s *Store) EndShareSession(id int64, reason string) error {
	_, err := s.db.Exec(`UPDATE share_sessions SET ended_at = ?, ended_reason = ? WHERE id = ? AND ended_at IS NULL`,
		nowRFC3339(), reason, id)
	return err
}

// PurgeShareSessions deletes sessions that ended before `before`, and — by cascade —
// their guests, messages and actions. It returns how many went.
func (s *Store) PurgeShareSessions(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM share_sessions WHERE ended_at IS NOT NULL AND ended_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ExpiredShareSessions lists sessions past their expiry that nothing has ended yet.
func (s *Store) ExpiredShareSessions(now time.Time) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM share_sessions WHERE ended_at IS NULL AND expires_at <= ?`,
		now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const shareGuestCols = `id, session_id, name, email, state, muted, remote_addr, joined_at, admitted_at, left_at, invite_hash, user_id, account, avatar`

func (s *Store) scanShareGuest(row interface{ Scan(...any) error }) (ShareGuest, error) {
	var g ShareGuest
	var muted int
	var adm, left sql.NullString
	err := row.Scan(&g.ID, &g.SessionID, &g.Name, &g.Email, &g.State, &muted, &g.RemoteAddr, &g.JoinedAt, &adm, &left, &g.InviteHash,
		&g.UserID, &g.Account, &g.Avatar)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareGuest{}, errShareNotFound
	}
	if err != nil {
		return ShareGuest{}, err
	}
	// Name and email are sealed (encryption.go): what a guest typed is theirs.
	if g.Name, err = s.openVal(aadID("share_guests", "name", g.SessionID), g.Name); err != nil {
		return ShareGuest{}, err
	}
	if g.Email, err = s.openVal(aadID("share_guests", "email", g.SessionID), g.Email); err != nil {
		return ShareGuest{}, err
	}
	g.Muted = muted != 0
	if adm.Valid {
		g.AdmittedAt = &adm.String
	}
	if left.Valid {
		g.LeftAt = &left.String
	}
	return g, nil
}

// SetShareGuestIdentity records how a guest joined: their avatar, and the account
// they signed in with, if they did.
func (s *Store) SetShareGuestIdentity(id, userID int64, account, avatar string) error {
	_, err := s.db.Exec(`UPDATE share_guests SET user_id = ?, account = ?, avatar = ? WHERE id = ?`, userID, account, avatar, id)
	return err
}

func (s *Store) CreateShareGuest(sessionID int64, name, email, cookieHash, remoteAddr, inviteHash string) (ShareGuest, error) {
	name, err := s.sealVal(aadID("share_guests", "name", sessionID), name)
	if err != nil {
		return ShareGuest{}, err
	}
	if email, err = s.sealVal(aadID("share_guests", "email", sessionID), email); err != nil {
		return ShareGuest{}, err
	}
	res, err := s.db.Exec(`INSERT INTO share_guests (session_id, name, email, cookie_hash, state, remote_addr, joined_at, invite_hash)
		VALUES (?,?,?,?,?,?,?,?)`, sessionID, name, email, cookieHash, guestWaiting, remoteAddr, nowRFC3339(), inviteHash)
	if err != nil {
		return ShareGuest{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ShareGuest{}, err
	}
	return s.GetShareGuest(id)
}

func (s *Store) GetShareGuest(id int64) (ShareGuest, error) {
	return s.scanShareGuest(s.db.QueryRow(`SELECT `+shareGuestCols+` FROM share_guests WHERE id = ?`, id))
}

func (s *Store) ShareGuestByCookie(cookieHash string) (ShareGuest, error) {
	return s.scanShareGuest(s.db.QueryRow(`SELECT `+shareGuestCols+` FROM share_guests WHERE cookie_hash = ?`, cookieHash))
}

func (s *Store) ListShareGuests(sessionID int64) ([]ShareGuest, error) {
	rows, err := s.db.Query(`SELECT `+shareGuestCols+` FROM share_guests WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareGuest{}
	for rows.Next() {
		g, err := s.scanShareGuest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetShareGuestState moves a guest through the lobby. admitted stamps admitted_at;
// the three ways out stamp left_at.
func (s *Store) SetShareGuestState(id int64, state string) error {
	switch state {
	case guestAdmitted:
		_, err := s.db.Exec(`UPDATE share_guests SET state = ?, admitted_at = ? WHERE id = ?`, state, nowRFC3339(), id)
		return err
	case guestRemoved, guestLeft, guestDenied:
		_, err := s.db.Exec(`UPDATE share_guests SET state = ?, left_at = ? WHERE id = ?`, state, nowRFC3339(), id)
		return err
	default:
		_, err := s.db.Exec(`UPDATE share_guests SET state = ? WHERE id = ?`, state, id)
		return err
	}
}

func (s *Store) SetShareGuestMuted(id int64, muted bool) error {
	m := 0
	if muted {
		m = 1
	}
	_, err := s.db.Exec(`UPDATE share_guests SET muted = ? WHERE id = ?`, m, id)
	return err
}

func (s *Store) AddShareMessage(m ShareMessage) (ShareMessage, error) {
	if m.CreatedAt == "" {
		m.CreatedAt = nowRFC3339()
	}
	author, err := s.sealVal(aadID("share_messages", "author", m.SessionID), m.Author)
	if err != nil {
		return ShareMessage{}, err
	}
	body, err := s.sealVal(aadID("share_messages", "body", m.SessionID), m.Body)
	if err != nil {
		return ShareMessage{}, err
	}
	res, err := s.db.Exec(`INSERT INTO share_messages (session_id, author_kind, guest_id, author, kind, body, created_at)
		VALUES (?,?,?,?,?,?,?)`, m.SessionID, m.AuthorKind, m.GuestID, author, m.Kind, body, m.CreatedAt)
	if err != nil {
		return ShareMessage{}, err
	}
	m.ID, err = res.LastInsertId()
	return m, err
}

// ListShareMessages returns a session's transcript in order. limit <= 0 is all of it.
func (s *Store) ListShareMessages(sessionID int64, limit int) ([]ShareMessage, error) {
	q := `SELECT id, session_id, author_kind, guest_id, author, kind, body, created_at FROM share_messages
		WHERE session_id = ? ORDER BY id`
	args := []any{sessionID}
	if limit > 0 {
		// The last `limit`, still in order.
		q = `SELECT * FROM (SELECT id, session_id, author_kind, guest_id, author, kind, body, created_at FROM share_messages
			WHERE session_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareMessage{}
	for rows.Next() {
		var m ShareMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.AuthorKind, &m.GuestID, &m.Author, &m.Kind, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		if m.Author, err = s.openVal(aadID("share_messages", "author", m.SessionID), m.Author); err != nil {
			return nil, err
		}
		if m.Body, err = s.openVal(aadID("share_messages", "body", m.SessionID), m.Body); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CountShareMessages(sessionID int64) int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM share_messages WHERE session_id = ?`, sessionID).Scan(&n)
	return n
}

// StartShareAction records a guest's write before it runs; FinishShareAction adds the
// outcome. Two steps so a request that never returns (a crash, a hung deploy) still
// leaves a row saying it was attempted.
func (s *Store) StartShareAction(a ShareAction, messageID int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO share_actions (session_id, guest_id, method, path, summary, message_id, created_at)
		VALUES (?,?,?,?,?,?,?)`, a.SessionID, a.GuestID, a.Method, a.Path, a.Summary, messageID, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishShareAction(id int64, status int) error {
	_, err := s.db.Exec(`UPDATE share_actions SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) ListShareActions(sessionID int64) ([]ShareAction, error) {
	rows, err := s.db.Query(`SELECT id, session_id, guest_id, method, path, summary, status, created_at
		FROM share_actions WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareAction{}
	for rows.Next() {
		var a ShareAction
		if err := rows.Scan(&a.ID, &a.SessionID, &a.GuestID, &a.Method, &a.Path, &a.Summary, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
