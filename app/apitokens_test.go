package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tokenFor mints a token for a user straight through the store, returning the
// secret. Most tests want a working credential, not the creation handler.
func tokenFor(t *testing.T, app *App, userID int64, scope string, days int) (string, APIToken) {
	t.Helper()
	secret, hash, prefix, err := newTokenSecret()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	var expires *string
	if days != 0 {
		e := time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
		expires = &e
	}
	tok, err := app.store.CreateAPIToken(userID, "test", hash, prefix, scope, expires)
	if err != nil {
		t.Fatalf("store token: %v", err)
	}
	return secret, tok
}

func bearerReq(method, path, secret string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	return r
}

// ------------------------------------------------------------- secrets

func TestTokenSecretShape(t *testing.T) {
	secret, hash, prefix, err := newTokenSecret()
	if err != nil {
		t.Fatalf("newTokenSecret: %v", err)
	}
	if !strings.HasPrefix(secret, tokenPrefix) {
		t.Errorf("secret %q does not start with %q — secret scanners key off that prefix", secret, tokenPrefix)
	}
	// 32 bytes as unpadded base64url is 43 characters.
	if want := len(tokenPrefix) + 43; len(secret) != want {
		t.Errorf("secret is %d characters, want %d", len(secret), want)
	}
	if len(hash) != 64 {
		t.Errorf("hash is %d characters, want 64 hex digits of SHA-256", len(hash))
	}
	if hash == secret || strings.Contains(hash, strings.TrimPrefix(secret, tokenPrefix)) {
		t.Error("the stored hash must not contain the secret")
	}
	if !strings.HasPrefix(secret, prefix) || len(prefix) != len(tokenPrefix)+8 {
		t.Errorf("prefix %q is not the first 8 characters of the secret", prefix)
	}
	if hashTokenSecret(secret) != hash {
		t.Error("hashTokenSecret is not stable")
	}
}

func TestTokenSecretsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, _, _, err := newTokenSecret()
		if err != nil {
			t.Fatalf("newTokenSecret: %v", err)
		}
		if seen[s] {
			t.Fatal("newTokenSecret repeated a secret")
		}
		seen[s] = true
	}
}

func TestBearerTokenParsing(t *testing.T) {
	for _, c := range []struct{ header, want string }{
		{"Bearer dbc_abc", "dbc_abc"},
		{"bearer dbc_abc", "dbc_abc"}, // RFC 7235 says the scheme is case-insensitive
		{"BEARER dbc_abc", "dbc_abc"},
		{"Bearer  dbc_abc ", "dbc_abc"}, // stray whitespace from a shell variable
		{"", ""},
		{"dbc_abc", ""}, // no scheme is not a bearer token
		{"Basic dXNlcjpwYXNz", ""},
		{"Bearer", ""},
	} {
		r := httptest.NewRequest("GET", "/api/me", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if got := bearerToken(r); got != c.want {
			t.Errorf("Authorization %q -> %q, want %q", c.header, got, c.want)
		}
	}
}

// ------------------------------------------------------------- verification

func TestTokenAuthAcceptsALiveToken(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)
	secret, tok := tokenFor(t, app, u.ID, ScopeWrite, 30)

	got, gotUser, err := app.tokenAuth(bearerReq("GET", "/api/me", secret))
	if err != nil {
		t.Fatalf("tokenAuth rejected a live token: %v", err)
	}
	if got.ID != tok.ID || gotUser.ID != u.ID {
		t.Errorf("resolved to token %d / user %d, want %d / %d", got.ID, gotUser.ID, tok.ID, u.ID)
	}
}

func TestTokenAuthRefusals(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)

	t.Run("unknown secret", func(t *testing.T) {
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", "dbc_nope")); err == nil {
			t.Error("an unknown secret was accepted")
		}
	})

	t.Run("revoked", func(t *testing.T) {
		secret, tok := tokenFor(t, app, u.ID, ScopeWrite, 30)
		if err := app.store.RevokeAPIToken(tok.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
			t.Error("a revoked token was accepted")
		}
	})

	t.Run("expired", func(t *testing.T) {
		secret, hash, prefix, _ := newTokenSecret()
		past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		if _, err := app.store.CreateAPIToken(u.ID, "stale", hash, prefix, ScopeWrite, &past); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
			t.Error("an expired token was accepted")
		}
	})

	t.Run("owner disabled", func(t *testing.T) {
		other, _ := app.store.CreateUser("gone", "x", RoleUser, StatusApproved)
		secret, _ := tokenFor(t, app, other.ID, ScopeWrite, 30)
		if _, err := app.store.SetStatus(other.ID, StatusDisabled); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
			t.Error("a token whose owner is disabled was accepted")
		}
	})

	t.Run("never expires is fine", func(t *testing.T) {
		secret, _ := tokenFor(t, app, u.ID, ScopeWrite, 0)
		if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err != nil {
			t.Errorf("a token with no expiry was rejected: %v", err)
		}
	})
}

// TestDisablingAUserRevokesTheirTokens is the failure mode worth a test of its own:
// without this, taking someone's access away would close the browser and leave the
// API open.
func TestDisablingAUserRevokesTheirTokens(t *testing.T) {
	app := newTestApp(t)
	admin, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	victim, _ := app.store.CreateUser("victim", "x", RoleUser, StatusApproved)
	secret, _ := tokenFor(t, app, victim.ID, ScopeWrite, 30)

	// Through the handler, so the wiring is what is under test, not the store call.
	r := httptest.NewRequest("POST", "/api/users/"+strconv.FormatInt(victim.ID, 10)+"/disable", nil)
	r.SetPathValue("id", strconv.FormatInt(victim.ID, 10))
	r = withPrincipal(r, principal{User: admin})
	app.handleUserStatus(StatusDisabled)(httptest.NewRecorder(), r)

	if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
		t.Fatal("disabling an account left its API token working")
	}
	tokens, _ := app.store.ListAPITokens(victim.ID)
	if len(tokens) != 1 || tokens[0].State != "revoked" {
		t.Errorf("token state is %v, want revoked", tokens)
	}
}

// ------------------------------------------------------------- scopes

func TestScopeAllows(t *testing.T) {
	for _, c := range []struct {
		have, need string
		ok         bool
	}{
		{ScopeRead, ScopeRead, true},
		{ScopeRead, ScopeWrite, false},
		{ScopeRead, ScopeAdmin, false},
		{ScopeWrite, ScopeRead, true},
		{ScopeWrite, ScopeWrite, true},
		{ScopeWrite, ScopeAdmin, false},
		{ScopeAdmin, ScopeRead, true},
		{ScopeAdmin, ScopeWrite, true},
		{ScopeAdmin, ScopeAdmin, true},
		{"nonsense", ScopeRead, false}, // a hand-edited row cannot widen access
		{"", ScopeRead, false},
	} {
		if got := scopeAllows(c.have, c.need); got != c.ok {
			t.Errorf("scopeAllows(%q, %q) = %v, want %v", c.have, c.need, got, c.ok)
		}
	}
}

// TestEffectiveScopeIsCappedByRole is the reason a scope is not simply trusted: a
// demoted admin's admin-scope tokens have to narrow by themselves, without anybody
// remembering to chase them through the table.
func TestEffectiveScopeIsCappedByRole(t *testing.T) {
	adminTok := APIToken{Scope: ScopeAdmin}
	if got := effectiveScope(adminTok, User{Role: RoleAdmin}); got != ScopeAdmin {
		t.Errorf("an admin's admin token = %q, want %q", got, ScopeAdmin)
	}
	if got := effectiveScope(adminTok, User{Role: RoleUser}); got != ScopeWrite {
		t.Errorf("a demoted admin's admin token = %q, want %q", got, ScopeWrite)
	}
	if got := effectiveScope(APIToken{Scope: ScopeRead}, User{Role: RoleAdmin}); got != ScopeRead {
		t.Error("being an admin must not widen a read token")
	}
}

func TestScopesFor(t *testing.T) {
	if got := scopesFor(RoleUser); len(got) != 2 || got[1] != ScopeWrite {
		t.Errorf("a user may create %v, want read+write only", got)
	}
	if got := scopesFor(RoleAdmin); len(got) != 3 || got[2] != ScopeAdmin {
		t.Errorf("an admin may create %v, want read+write+admin", got)
	}
}

// TestRequireScopeEnforcement drives the wrapper itself across the four outcomes
// that matter.
func TestRequireScopeEnforcement(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)
	readSecret, _ := tokenFor(t, app, u.ID, ScopeRead, 30)
	writeSecret, _ := tokenFor(t, app, u.ID, ScopeWrite, 30)

	reached := false
	next := func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }
	writeRoute := apiRoute{Method: "POST", Path: "/api/stacks"}
	readRoute := apiRoute{Method: "GET", Path: "/api/stacks"}

	run := func(rt apiRoute, secret string) int {
		reached = false
		w := httptest.NewRecorder()
		app.requireScope(rt, next)(w, bearerReq(rt.Method, rt.Path, secret))
		return w.Code
	}

	if code := run(readRoute, readSecret); code != http.StatusOK || !reached {
		t.Errorf("read token on a GET: %d (reached=%v), want 200", code, reached)
	}
	if code := run(writeRoute, readSecret); code != http.StatusForbidden || reached {
		t.Errorf("read token on a POST: %d (reached=%v), want 403", code, reached)
	}
	if code := run(writeRoute, writeSecret); code != http.StatusOK || !reached {
		t.Errorf("write token on a POST: %d (reached=%v), want 200", code, reached)
	}
	if code := run(writeRoute, "dbc_bogus"); code != http.StatusUnauthorized || reached {
		t.Errorf("bogus token: %d (reached=%v), want 401", code, reached)
	}

	// A read token may call a POST that changes nothing.
	if code := run(apiRoute{Method: "POST", Path: "/api/stacks/1/validate", ReadOnly: true}, readSecret); code != http.StatusOK {
		t.Errorf("read token on a ReadOnly POST: %d, want 200", code)
	}
	// No Authorization header at all is left entirely alone — this is what keeps
	// the cookie path unchanged.
	reached = false
	w := httptest.NewRecorder()
	app.requireScope(writeRoute, next)(w, httptest.NewRequest("POST", "/api/stacks", nil))
	if !reached {
		t.Error("a cookie-session request was intercepted by requireScope")
	}
}

// TestTokenCannotMintTokens pins the asymmetry the whole design rests on.
func TestTokenCannotMintTokens(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleAdmin, StatusApproved)
	secret, _ := tokenFor(t, app, u.ID, ScopeAdmin, 30)

	var create apiRoute
	for _, rt := range apiRoutes() {
		if rt.Pattern() == "POST /api/tokens" {
			create = rt
		}
	}
	if !create.NoToken {
		t.Fatal("POST /api/tokens must be marked NoToken")
	}
	reached := false
	w := httptest.NewRecorder()
	app.requireScope(create, func(http.ResponseWriter, *http.Request) { reached = true })(
		w, bearerReq("POST", "/api/tokens", secret))
	if w.Code != http.StatusForbidden || reached {
		t.Errorf("an admin-scope token created a token: %d (reached=%v), want 403", w.Code, reached)
	}
	if !strings.Contains(w.Body.String(), "password") {
		t.Errorf("the refusal should say what to do instead: %q", w.Body.String())
	}
}

// ------------------------------------------------------------- expiry

func TestTokenExpiry(t *testing.T) {
	// Clamped, not refused: asking for a year when the ceiling is 90 days gets 90.
	exp, err := tokenExpiry(400, RoleUser, 90)
	if err != nil {
		t.Fatalf("tokenExpiry(400): %v", err)
	}
	at, err := time.Parse(time.RFC3339, *exp)
	if err != nil {
		t.Fatalf("expiry is not RFC3339: %v", err)
	}
	if d := time.Until(at); d > 91*24*time.Hour || d < 89*24*time.Hour {
		t.Errorf("clamped expiry is %v away, want ~90 days", d)
	}

	if _, err := tokenExpiry(0, RoleUser, 90); err == nil {
		t.Error("a non-admin was allowed a token that never expires")
	}
	never, err := tokenExpiry(0, RoleAdmin, 90)
	if err != nil || never != nil {
		t.Errorf("an admin's never-expiring token: %v, %v", never, err)
	}
	if _, err := tokenExpiry(-1, RoleAdmin, 90); err == nil {
		t.Error("a negative lifetime was accepted")
	}
	if exp, err := tokenExpiry(7, RoleUser, 90); err != nil || exp == nil {
		t.Errorf("a 7-day token: %v, %v", exp, err)
	}
}

func TestMaxTokenDaysFromSetting(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"30", 30},
		{"", defaultMaxTokenDays},
		{"nonsense", defaultMaxTokenDays},
		{"0", defaultMaxTokenDays},
		{"-5", defaultMaxTokenDays},
		{"9999", maxMaxTokenDays},
		{" 45 ", 45},
	} {
		if got := maxTokenDaysFromSetting(c.in); got != c.want {
			t.Errorf("maxTokenDaysFromSetting(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSystemSettingsCarryTokenCeiling(t *testing.T) {
	app := newTestApp(t)
	if got := app.maxTokenDays(); got != defaultMaxTokenDays {
		t.Errorf("default ceiling is %d, want %d", got, defaultMaxTokenDays)
	}
	if err := app.store.SetAppSetting(settingMaxTokenDays, "14"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := app.maxTokenDays(); got != 14 {
		t.Errorf("stored ceiling is %d, want 14", got)
	}
	// A hand-edited nonsense row degrades to the default rather than wedging things.
	app.store.SetAppSetting(settingMaxTokenDays, "banana")
	if got := app.maxTokenDays(); got != defaultMaxTokenDays {
		t.Errorf("nonsense ceiling is %d, want the default %d", got, defaultMaxTokenDays)
	}
}

// ------------------------------------------------------------- handlers

func TestCreateTokenHandler(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/tokens", bytes.NewBufferString(body))
		r = withPrincipal(r, principal{User: u})
		w := httptest.NewRecorder()
		app.handleCreateToken(w, r)
		return w
	}

	w := post(`{"name":"laptop","scope":"read","days":30}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Token  APIToken `json:"token"`
		Secret string   `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Secret == "" || !strings.HasPrefix(got.Secret, tokenPrefix) {
		t.Errorf("no usable secret in the response: %q", got.Secret)
	}
	if got.Token.Scope != ScopeRead || got.Token.ExpiresAt == nil {
		t.Errorf("token came back as %+v", got.Token)
	}

	// The secret is served once and never again.
	list := httptest.NewRecorder()
	app.handleListTokens(list, withPrincipal(httptest.NewRequest("GET", "/api/tokens", nil), principal{User: u}))
	if strings.Contains(list.Body.String(), got.Secret) {
		t.Fatal("the token listing leaked the secret")
	}
	if !strings.Contains(list.Body.String(), got.Token.Prefix) {
		t.Error("the listing should show the prefix so tokens can be told apart")
	}

	// A name is required — an unlabelled token is one nobody dares revoke.
	if w := post(`{"scope":"read","days":30}`); w.Code != http.StatusBadRequest {
		t.Errorf("a nameless token returned %d, want 400", w.Code)
	}
	// A non-admin cannot mint an admin-scope token.
	if w := post(`{"name":"x","scope":"admin","days":30}`); w.Code != http.StatusForbidden {
		t.Errorf("a user creating an admin token returned %d, want 403", w.Code)
	}
	// An unknown scope is a typo, not a silent downgrade.
	if w := post(`{"name":"x","scope":"root","days":30}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown scope returned %d, want 400", w.Code)
	}
	// Scope defaults to write, which is what the CLI wants.
	w = post(`{"name":"cli","days":7}`)
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Token.Scope != ScopeWrite {
		t.Errorf("default scope is %q, want %q", got.Token.Scope, ScopeWrite)
	}
}

func TestRevokeTokenIsOwnerOnly(t *testing.T) {
	app := newTestApp(t)
	mine, _ := app.store.CreateUser("mine", "x", RoleUser, StatusApproved)
	theirs, _ := app.store.CreateUser("theirs", "x", RoleAdmin, StatusApproved)
	_, tok := tokenFor(t, app, mine.ID, ScopeWrite, 30)

	revoke := func(as User) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/tokens/"+strconv.FormatInt(tok.ID, 10), nil)
		r.SetPathValue("id", strconv.FormatInt(tok.ID, 10))
		r = withPrincipal(r, principal{User: as})
		w := httptest.NewRecorder()
		app.handleRevokeToken(w, r)
		return w
	}

	// Even an admin is refused here: they use the admin endpoint, which says so.
	if w := revoke(theirs); w.Code != http.StatusForbidden {
		t.Errorf("somebody else revoking my token returned %d, want 403", w.Code)
	}
	if w := revoke(mine); w.Code != http.StatusOK {
		t.Errorf("revoking my own token returned %d: %s", w.Code, w.Body.String())
	}
	got, _ := app.store.GetAPIToken(tok.ID)
	if got.State != "revoked" {
		t.Errorf("state is %q, want revoked", got.State)
	}
	if got.RevokedAt == nil {
		t.Error("the row should stay, marked revoked, rather than disappearing")
	}
}

func TestAdminRevokeAnyToken(t *testing.T) {
	app := newTestApp(t)
	victim, _ := app.store.CreateUser("victim", "x", RoleUser, StatusApproved)
	secret, tok := tokenFor(t, app, victim.ID, ScopeWrite, 30)

	r := httptest.NewRequest("DELETE", "/api/admin/tokens/"+strconv.FormatInt(tok.ID, 10), nil)
	r.SetPathValue("id", strconv.FormatInt(tok.ID, 10))
	w := httptest.NewRecorder()
	app.handleAdminRevokeToken(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("admin revoke returned %d: %s", w.Code, w.Body.String())
	}
	if _, _, err := app.tokenAuth(bearerReq("GET", "/api/me", secret)); err == nil {
		t.Error("the token still works after an admin revoked it")
	}
	// The owner is told, because a credential vanishing without explanation is
	// how an afternoon gets lost.
	notes, _ := app.store.ListNotifications(victim.ID, false, 10)
	found := false
	for _, n := range notes {
		if n.Type == "token.revoked" {
			found = true
		}
	}
	if !found {
		t.Error("the owner was not notified that their token was revoked")
	}
}

func TestAdminTokenListingCarriesOwners(t *testing.T) {
	app := newTestApp(t)
	a, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	b, _ := app.store.CreateUser("bob", "x", RoleUser, StatusApproved)
	tokenFor(t, app, a.ID, ScopeRead, 30)
	tokenFor(t, app, b.ID, ScopeWrite, 30)

	all, err := app.store.ListAllAPITokens()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d tokens, want 2", len(all))
	}
	names := map[string]bool{}
	for _, tk := range all {
		names[tk.Username] = true
	}
	if !names["alice"] || !names["bob"] {
		t.Errorf("the admin listing lost the owners: %v", names)
	}
}

// ------------------------------------------------------------- last-used

func TestTokenUsageIsBatched(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)
	_, tok := tokenFor(t, app, u.ID, ScopeWrite, 30)

	// Noting a use must not touch the database — the whole point is that a
	// single-connection SQLite never takes a write on the request path.
	noteTokenUse(tok.ID)
	if got, _ := app.store.GetAPIToken(tok.ID); got.LastUsedAt != nil {
		t.Error("noteTokenUse wrote to the store synchronously")
	}
	app.flushTokenUsage()
	if got, _ := app.store.GetAPIToken(tok.ID); got.LastUsedAt == nil {
		t.Error("flushTokenUsage did not persist the stamp")
	}
	// Flushing an empty buffer is a no-op, not an error.
	app.flushTokenUsage()
}

func TestPurgeKeepsRecentlyDeadTokens(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)

	// Expired yesterday: kept, so "why did my script break" has an answer.
	_, hash, prefix, _ := newTokenSecret()
	yesterday := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	app.store.CreateAPIToken(u.ID, "recent", hash, prefix, ScopeRead, &yesterday)
	// Expired a year ago: reaped.
	_, hash2, prefix2, _ := newTokenSecret()
	longAgo := time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	app.store.CreateAPIToken(u.ID, "ancient", hash2, prefix2, ScopeRead, &longAgo)

	if err := app.store.PurgeExpiredAPITokens(30 * 24 * time.Hour); err != nil {
		t.Fatalf("purge: %v", err)
	}
	tokens, _ := app.store.ListAPITokens(u.ID)
	if len(tokens) != 1 || tokens[0].Name != "recent" {
		t.Errorf("after the purge: %v, want just \"recent\"", tokens)
	}
}

// TestTokenStateDerivation covers the three states without going near the clock of
// a real request.
func TestTokenStateDerivation(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	if got := (APIToken{}).state(now); got != "active" {
		t.Errorf("a token with no expiry is %q, want active", got)
	}
	if got := (APIToken{ExpiresAt: &future}).state(now); got != "active" {
		t.Errorf("an unexpired token is %q, want active", got)
	}
	if got := (APIToken{ExpiresAt: &past}).state(now); got != "expired" {
		t.Errorf("an expired token is %q, want expired", got)
	}
	// Revoked wins over expired: it is the more specific thing that happened.
	if got := (APIToken{ExpiresAt: &past, RevokedAt: &past}).state(now); got != "revoked" {
		t.Errorf("a revoked and expired token is %q, want revoked", got)
	}
	// An unparseable expiry must not silently mean "expired"; it means the column
	// is broken, and refusing to authenticate on a corrupt row is the safe read —
	// but state() reports active and tokenAuth's other checks apply.
	bad := "not-a-date"
	if got := (APIToken{ExpiresAt: &bad}).state(now); got != "active" {
		t.Errorf("an unparseable expiry is %q, want active", got)
	}
}
