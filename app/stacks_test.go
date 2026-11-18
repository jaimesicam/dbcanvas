package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestApp opens a throwaway store in a temp dir and returns an App.
func newTestApp(t *testing.T) *App {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return &App{store: store}
}

func TestReaperExpiresPastStacks(t *testing.T) {
	app := newTestApp(t)
	_, err := app.store.CreateUser("admin")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	expired, err := app.store.CreateStack("old", "2h", &past, []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create expired stack: %v", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	live, err := app.store.CreateStack("live", "2h", &future, []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create live stack: %v", err)
	}
	month, err := app.store.CreateStack("month", ttlLongest, expiryFor(ttlLongest), []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create 30-day stack: %v", err)
	}

	app.reapExpiredStacks()

	if got, _ := app.store.GetStack(expired.ID); got.Status != StackExpired {
		t.Errorf("expired stack status = %q, want %q", got.Status, StackExpired)
	}
	if got, _ := app.store.GetStack(live.ID); got.Status != StackDraft {
		t.Errorf("live stack status = %q, want %q (must not be reaped)", got.Status, StackDraft)
	}
	if got, _ := app.store.GetStack(month.ID); got.Status != StackDraft {
		t.Errorf("30-day stack status = %q, want %q", got.Status, StackDraft)
	}
}

func TestExpiryForTTL(t *testing.T) {
	if validTTL("infinity") {
		t.Error("a stack must not be kept forever")
	}
	p := expiryFor(ttlLongest)
	if e, err := time.Parse(time.RFC3339, *p); err != nil || time.Until(e) < 29*24*time.Hour || time.Until(e) > 30*24*time.Hour {
		t.Errorf("30d expiry = %v", *p)
	}
	if p := expiryFor("24h"); p == nil {
		t.Error("24h TTL must have an expiry")
	}
	if !validTTL("2w") || validTTL("5m") {
		t.Error("validTTL gate is wrong")
	}
}

// The Database Stacks table's list (?detail=1): the canvas's node types, and how many
// of the deployed nodes run or failed — a node with no deployment counts in the total only.
func TestStackSummaries(t *testing.T) {
	app := newTestApp(t)
	_, err := app.store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	design := `{"nodes":[{"id":"a","type":"pg","label":"pg01"},{"id":"b","type":"pg","label":"pg02"},{"id":"c","type":"pmm","label":"pmm"}],"frames":[],"edges":[]}`
	st, err := app.store.CreateStack("lab", "2h", nil, []byte(design))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "a", State: DeployRunning})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "b", State: DeployError})

	stacks, _ := app.store.ListStacks()
	got := app.stackSummaries(stacks)
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	s := got[0]
	if s.Name != "lab" {
		t.Errorf("name = %q, want lab", s.Name)
	}
	if s.Nodes != (stackNodeCount{Total: 3, Running: 1, Error: 1}) {
		t.Errorf("nodes = %+v, want total 3, running 1, error 1", s.Nodes)
	}
	if s.Types["pg"] != 2 || s.Types["pmm"] != 1 {
		t.Errorf("types = %v, want pg:2 pmm:1", s.Types)
	}
}

// sessionSeq numbers the sessions signedIn makes.
var sessionSeq int

// signedIn is a request carrying a session of u's, and the session's token.
func signedIn(t *testing.T, app *App, u User, body string) (*http.Request, string) {
	t.Helper()
	sessionSeq++
	token := fmt.Sprintf("sess-%s-%d", u.Username, sessionSeq)
	if err := app.store.CreateSession(token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	return r, token
}

// Extending never takes a stack's end further than 30 days from now, whatever it asks for.
func TestExtendIsCappedAtThirtyDays(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.ensureOwner()
	in20 := time.Now().Add(20 * 24 * time.Hour).UTC().Format(time.RFC3339)
	st, _ := app.store.CreateStack("lab", "2w", &in20, []byte(defaultDesign))
	for _, by := range []string{"2w", "30d"} {
		w := httptest.NewRecorder()
		r := withPrincipal(httptest.NewRequest("POST", "/x", strings.NewReader(`{"by":"`+by+`"}`)), principal{User: u})
		r.SetPathValue("id", strconv.FormatInt(st.ID, 10))
		app.handleExtendStack(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("extend by %s: %d %s", by, w.Code, w.Body)
		}
		got, _ := app.store.GetStack(st.ID)
		e, _ := time.Parse(time.RFC3339, *got.ExpiresAt)
		if left := time.Until(e); left > 30*24*time.Hour || left < 30*24*time.Hour-time.Minute {
			t.Errorf("extend by %s: %v left, want 30 days", by, left)
		}
	}
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("POST", "/x", strings.NewReader(`{"by":"infinity"}`)), principal{User: u})
	r.SetPathValue("id", strconv.FormatInt(st.ID, 10))
	app.handleExtendStack(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "30 days") {
		t.Errorf("extend forever: %d %s", w.Code, w.Body)
	}
}
