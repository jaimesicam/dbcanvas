package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// apitokens.go — a revocable, expiring bearer credential that stands in for a
// password, for a script that calls the API over HTTP.
//
// The whole design turns on one asymmetry: a password is the credential that can
// create credentials, and a token is not. Everything else follows from that —
// POST /api/tokens refuses bearer auth (NoToken on the route), so a leaked token
// cannot mint a longer-lived sibling for itself; revoking is cheap and immediate;
// and a token reaches only what its scope and its stack limit allow.

// Token scopes, narrowest first. A scope is checked against the route being called
// (routeScope), which is only possible because api_routes.go knows the method and
// whether a POST actually changes anything.
const (
	ScopeRead  = "read"  // GET, plus the POSTs marked ReadOnly
	ScopeWrite = "write" // everything the browser can do
	ScopeAdmin = "admin" // additionally the system endpoints
)

// scopeRank orders the scopes so a check is a comparison rather than a set lookup.
var scopeRank = map[string]int{ScopeRead: 0, ScopeWrite: 1, ScopeAdmin: 2}

// tokenPrefix is on the front of every secret. It is not decoration: a literal,
// searchable marker is what lets a secret scanner, a pre-commit hook, or somebody
// grepping their shell history find a token that escaped.
const tokenPrefix = "dbc_"

// tokenBytes is the entropy behind a token. 32 bytes is why the stored hash can be
// SHA-256 rather than bcrypt — there is no dictionary to run against it.
const tokenBytes = 32

// maxTokenNameLen keeps a name usable as a label in the UI and in `dbcanvas token list`.
const maxTokenNameLen = 64

// ErrTokenNotFound is returned when no live token matches a secret.
var ErrTokenNotFound = errors.New("token not found")

// APIToken is a token as the owner sees it. The secret is never in here — it exists
// once, in the response to the request that created it, and nowhere else ever again.
type APIToken struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	Scope      string  `json:"scope"`
	CreatedAt  string  `json:"createdAt"`
	ExpiresAt  *string `json:"expiresAt,omitempty"`
	LastUsedAt *string `json:"lastUsedAt,omitempty"`
	RevokedAt  *string `json:"revokedAt,omitempty"`
	// Limit narrows the token to some stacks (tokenlimit.go); nil for none.
	Limit *tokenLimit `json:"limit,omitempty"`
	// State is derived rather than stored: active | expired | revoked. An expired
	// token stays listed precisely so that "why is my script getting 401" has a
	// visible answer.
	State string `json:"state"`
}

// state derives the token's state at time t.
func (t APIToken) state(now time.Time) string {
	if t.RevokedAt != nil {
		return "revoked"
	}
	if t.ExpiresAt != nil {
		if exp, err := time.Parse(time.RFC3339, *t.ExpiresAt); err == nil && now.After(exp) {
			return "expired"
		}
	}
	return "active"
}

// ------------------------------------------------------------- secrets

// newTokenSecret mints a secret and returns it with the hash and prefix to store.
func newTokenSecret() (secret, hash, prefix string, err error) {
	raw := make([]byte, tokenBytes)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", err
	}
	secret = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash = hashTokenSecret(secret)
	// Enough to tell two of your own tokens apart, far too little to guess the rest.
	prefix = secret[:len(tokenPrefix)+8]
	return secret, hash, prefix, nil
}

// hashTokenSecret is the stored form of a secret.
func hashTokenSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// bearerToken pulls the credential out of an Authorization header, or "" when there
// is no bearer token to be had. The scheme match is case-insensitive because RFC
// 7235 says it is, and clients disagree about the capitalisation.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// ------------------------------------------------------------- store

// CreateAPIToken inserts a token row and returns it.
func (s *Store) CreateAPIToken(name, hash, prefix, scope string, expires *string) (APIToken, error) {
	created := nowRFC3339()
	res, err := s.db.Exec(
		`INSERT INTO api_tokens (name,token_hash,prefix,scope,created_at,expires_at)
		 VALUES (?,?,?,?,?,?)`,
		name, hash, prefix, scope, created, nfStr2(expires))
	if err != nil {
		return APIToken{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return APIToken{}, err
	}
	return s.GetAPIToken(id)
}

const apiTokenCols = `id, name, prefix, scope, created_at, expires_at, last_used_at, revoked_at, limit_json`

func scanAPIToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var expires, lastUsed, revoked, limit sql.NullString
	if err := row.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope,
		&t.CreatedAt, &expires, &lastUsed, &revoked, &limit); err != nil {
		return APIToken{}, err
	}
	t.Limit = decodeTokenLimit(limit.String)
	if expires.Valid {
		t.ExpiresAt = &expires.String
	}
	if lastUsed.Valid {
		t.LastUsedAt = &lastUsed.String
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.String
	}
	t.State = t.state(time.Now())
	return t, nil
}

func (s *Store) GetAPIToken(id int64) (APIToken, error) {
	return scanAPIToken(s.db.QueryRow("SELECT "+apiTokenCols+" FROM api_tokens WHERE id = ?", id))
}

// ListAPITokens returns the tokens, newest first.
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query("SELECT " + apiTokenCols + " FROM api_tokens ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// APITokenByHash finds a token by its stored hash, with the account it acts as. The
// caller checks its state.
func (s *Store) APITokenByHash(hash string) (APIToken, User, error) {
	t, err := scanAPIToken(s.db.QueryRow("SELECT "+apiTokenCols+" FROM api_tokens WHERE token_hash = ?", hash))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIToken{}, User{}, ErrTokenNotFound
		}
		return APIToken{}, User{}, err
	}
	u, err := s.Owner()
	if err != nil {
		return APIToken{}, User{}, err
	}
	return t, u, nil
}

// RevokeAPIToken marks a token revoked. The row stays: an audit list with holes in
// it is not an audit list.
func (s *Store) RevokeAPIToken(id int64) error {
	_, err := s.db.Exec("UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL",
		nowRFC3339(), id)
	return err
}

// TouchAPITokens records last-used times in one statement per flush (see
// tokenUsage). Failure is ignored by the caller: this column is for the human
// reading the list, and nothing about authentication depends on it.
func (s *Store) TouchAPITokens(seen map[int64]time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("UPDATE api_tokens SET last_used_at = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, at := range seen {
		if _, err := stmt.Exec(at.UTC().Format(time.RFC3339), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeExpiredAPITokens deletes tokens that expired or were revoked longer ago than
// keep. They are kept for a while on purpose — "my script broke last Tuesday" is
// answered by the row, not by its absence.
func (s *Store) PurgeExpiredAPITokens(keep time.Duration) error {
	cutoff := time.Now().Add(-keep).UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM api_tokens
		WHERE (expires_at IS NOT NULL AND expires_at < ?)
		   OR (revoked_at IS NOT NULL AND revoked_at < ?)`, cutoff, cutoff)
	return err
}

// nfStr2 maps an optional string to a nullable column value.
func nfStr2(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ------------------------------------------------------------- last-used batching

// tokenUsage buffers last-used stamps in memory and writes them out on a timer.
//
// This exists because the store runs SQLite with SetMaxOpenConns(1): an UPDATE per
// authenticated request would put every API call in a queue behind the write lock,
// to maintain a column whose whole purpose is to be roughly right in a list. One
// minute of granularity costs nothing anybody can perceive.
var tokenUsage = struct {
	mu   sync.Mutex
	seen map[int64]time.Time
}{seen: map[int64]time.Time{}}

func noteTokenUse(id int64) {
	tokenUsage.mu.Lock()
	tokenUsage.seen[id] = time.Now()
	tokenUsage.mu.Unlock()
}

// flushTokenUsage drains the buffer into the store.
func (a *App) flushTokenUsage() {
	tokenUsage.mu.Lock()
	if len(tokenUsage.seen) == 0 {
		tokenUsage.mu.Unlock()
		return
	}
	batch := tokenUsage.seen
	tokenUsage.seen = map[int64]time.Time{}
	tokenUsage.mu.Unlock()
	a.store.TouchAPITokens(batch)
}

// startTokenMaintenance flushes last-used stamps every minute and purges long-dead
// tokens once an hour.
func (a *App) startTokenMaintenance() {
	go func() {
		flush := time.NewTicker(60 * time.Second)
		purge := time.NewTicker(time.Hour)
		defer flush.Stop()
		defer purge.Stop()
		for {
			select {
			case <-flush.C:
				a.flushTokenUsage()
			case <-purge.C:
				a.store.PurgeExpiredAPITokens(30 * 24 * time.Hour)
			}
		}
	}()
}

// ------------------------------------------------------------- authentication

// principal is who is making a request, and how. Token is nil for a cookie session.
type principal struct {
	User  User
	Token *APIToken
	// Guest is set when a shared-session guest sent the request; User is then the
	// host they act as (share.go).
	Guest *guestPrincipal
	// Socket is set on a request that came in on the owner-only control socket, which
	// acts as the owner with no other credential (ctlsock.go).
	Socket bool
}

type principalKey struct{}

// withPrincipal stashes a resolved principal on the request so currentUser does not
// look the token up a second time.
func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// principalOf returns the resolved principal, if the request carried one.
func principalOf(r *http.Request) (principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(principal)
	return p, ok
}

// tokenAuth resolves a bearer token to its owner.
//
// A token that is revoked or expired is not a credential.
func (a *App) tokenAuth(r *http.Request) (APIToken, User, error) {
	secret := bearerToken(r)
	if secret == "" {
		return APIToken{}, User{}, ErrTokenNotFound
	}
	tok, u, err := a.store.APITokenByHash(hashTokenSecret(secret))
	if err != nil {
		// A real database error must not read as "no such token": conflating the
		// two is how an ambiguous column in the lookup query once presented itself
		// as every token being invalid. The caller still answers 401 either way.
		if !errors.Is(err, ErrTokenNotFound) {
			log.Printf("api token lookup failed: %v", err)
		}
		return APIToken{}, User{}, ErrTokenNotFound
	}
	if tok.state(time.Now()) != "active" {
		return APIToken{}, User{}, ErrTokenNotFound
	}
	return tok, u, nil
}

// ------------------------------------------------------------- scopes

// routeScope is the least scope a token needs to call a route.
func routeScope(rt apiRoute) string {
	if rt.Auth == authAdmin {
		return ScopeAdmin
	}
	if rt.Mutates() {
		return ScopeWrite
	}
	return ScopeRead
}

// scopeAllows reports whether a token's scope covers what a route needs.
func scopeAllows(have, need string) bool {
	h, ok := scopeRank[have]
	if !ok {
		return false
	}
	n, ok := scopeRank[need]
	return ok && h >= n
}

// requireScope is the wrapper every route gets. For a cookie session it does
// nothing at all — the handler's own checks are unchanged, which is what keeps the
// route-table refactor behaviour-preserving. For a bearer token it resolves the
// principal once, enforces the scope, and refuses the endpoints a token may never
// reach whatever its scope.
func (a *App) requireScope(rt apiRoute, next http.HandlerFunc) http.HandlerFunc {
	need := routeScope(rt)
	return func(w http.ResponseWriter, r *http.Request) {
		// The control socket's request is the owner's, resolved before routing.
		if p, ok := principalOf(r); ok && p.Socket {
			next(w, r)
			return
		}
		// A shared-session guest acts as the host with a read or write scope, so it
		// is resolved here, beside the token that it resembles (share.go).
		if isGuestRequest(r) && bearerToken(r) == "" {
			a.serveGuest(rt, next, w, r)
			return
		}
		if bearerToken(r) == "" {
			// A cookie session is resolved here once, and stashed so the handler's
			// currentUser does not look it up again.
			if rt.Auth != authPublic {
				if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
					if u, err := a.store.SessionUser(c.Value); err == nil {
						r = withPrincipal(r, principal{User: u})
					}
				}
			}
			next(w, r)
			return
		}
		tok, u, err := a.tokenAuth(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid, expired or revoked API token")
			return
		}
		if rt.NoToken {
			writeErr(w, http.StatusForbidden,
				"this endpoint cannot be used with an API token — open dbcanvas in the browser")
			return
		}
		if !scopeAllows(tok.Scope, need) {
			writeErr(w, http.StatusForbidden, scopeRefusal(need, tok.Scope))
			return
		}
		if why := a.tokenLimitAllows(rt, r, tok); why != "" {
			writeErr(w, http.StatusForbidden, why)
			return
		}
		noteTokenUse(tok.ID)
		next(w, withPrincipal(r, principal{User: u, Token: &tok}))
	}
}

// ------------------------------------------------------------- expiry

// tokenExpiry resolves a requested lifetime to a stored expiry: days == 0 is "never",
// asked for on purpose; more than the ceiling (maxTokenDays) is clamped to it.
func tokenExpiry(days int, maxDays int) (*string, error) {
	if days < 0 {
		return nil, errors.New("expiry must be a number of days, or 0 for never")
	}
	if days == 0 {
		return nil, nil
	}
	if days > maxDays {
		days = maxDays
	}
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
	return &exp, nil
}

// ------------------------------------------------------------- handlers

func (a *App) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	tokens, err := a.store.ListAPITokens()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tokens":  tokens,
		"maxDays": a.maxTokenDays(),
	})
}

// tokenRequest is the create body. Days is a count rather than a date because that
// is how people say it ("a token for the next month"), and it keeps the client from
// having to reason about the server's clock.
type tokenRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Days  int    `json:"days"`
	// Limit narrows the token to some stacks (tokenlimit.go).
	Limit *tokenLimit `json:"limit"`
}

func (a *App) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	_, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in tokenRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "give the token a name, so you can tell later what it was for")
		return
	}
	if len(name) > maxTokenNameLen {
		name = name[:maxTokenNameLen]
	}
	scope := in.Scope
	if scope == "" {
		scope = ScopeWrite
	}
	if _, ok := scopeRank[scope]; !ok {
		writeErr(w, http.StatusBadRequest, "scope must be read, write or admin")
		return
	}
	expires, err := tokenExpiry(in.Days, a.maxTokenDays())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	secret, hash, prefix, err := newTokenSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to generate a token")
		return
	}
	if err := in.Limit.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "limit: "+err.Error())
		return
	}
	tok, err := a.store.CreateAPIToken(name, hash, prefix, scope, expires)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the token")
		return
	}
	if !in.Limit.empty() {
		if err := a.store.SetAPITokenLimit(tok.ID, in.Limit); err != nil {
			a.store.RevokeAPIToken(tok.ID)
			writeErr(w, http.StatusInternalServerError, "failed to save the token's limit")
			return
		}
		tok.Limit = in.Limit
	}
	a.notify(Notification{Type: "token.created", Severity: "info",
		Title: "API token created", Body: name + " (" + scope + " scope) can now act as you."})
	// The one and only time the secret is served.
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "secret": secret})
}

func (a *App) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid token id")
		return
	}
	if _, err := a.store.GetAPIToken(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "token not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "failed to read token")
		return
	}
	if err := a.store.RevokeAPIToken(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revoke token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}

// maxTokenDaysFromSetting parses the stored ceiling, clamping it into range.
func maxTokenDaysFromSetting(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultMaxTokenDays
	}
	if n < minMaxTokenDays {
		return minMaxTokenDays
	}
	if n > maxMaxTokenDays {
		return maxMaxTokenDays
	}
	return n
}

// SetAPITokenLimit stores a token's stack limit (tokenlimit.go); nil clears it.
func (s *Store) SetAPITokenLimit(id int64, l *tokenLimit) error {
	_, err := s.db.Exec("UPDATE api_tokens SET limit_json = ? WHERE id = ?", encodeTokenLimit(l), id)
	return err
}

// scopeRefusal is the 403 for a token whose scope is too narrow, with how to get one that is
// wide enough.
func scopeRefusal(need, have string) string {
	return "this endpoint needs a token with " + need + " scope; this token has " + have +
		" — create one with `dbcanvas token create NAME --scope " + need + "`, or on the API page"
}
