package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// asToken is a request made with a token carrying limit l.
func asToken(t *testing.T, app *App, u User, l *tokenLimit, method, path, body string) *http.Request {
	t.Helper()
	_, tok := tokenFor(t, app, u.ID, ScopeWrite, 1)
	tok.Limit = l
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	return withPrincipal(r, principal{User: u, Token: &tok})
}

func TestTokenMaxTTL(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.ensureOwner()
	l := &tokenLimit{MaxTTL: "8h"}
	for ttl, want := range map[string]int{"4h": http.StatusCreated, "8h": http.StatusCreated, "24h": http.StatusForbidden, "infinity": http.StatusForbidden} {
		w := httptest.NewRecorder()
		app.handleCreateStack(w, asToken(t, app, u, l, "POST", "/api/stacks", `{"name":"lab-`+ttl+`","ttl":"`+ttl+`"}`))
		if w.Code != want {
			t.Errorf("ttl %s: %d %s, want %d", ttl, w.Code, w.Body, want)
		}
	}
	st, _ := app.store.CreateStack("mine", u.ID, "4h", expiryFor("4h"), []byte(defaultDesign))
	w := httptest.NewRecorder()
	r := asToken(t, app, u, l, "POST", "/api/stacks/x/extend", `{"by":"2h"}`)
	r.SetPathValue("id", strconv.FormatInt(st.ID, 10))
	app.handleExtendStack(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("extend with a max TTL: %d %s", w.Code, w.Body)
	}
}

func TestTokenDeniedNodeTypes(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.ensureOwner()
	l := &tokenLimit{Deny: []string{"watchtower"}}
	design := `{"nodes":[{"id":"n1","type":"intranet","label":"intranet-01"},{"id":"n2","type":"watchtower","label":"wt"}],"edges":[]}`
	w := httptest.NewRecorder()
	app.handleCreateStack(w, asToken(t, app, u, l, "POST", "/api/stacks", `{"name":"lab","ttl":"2h","design":`+design+`}`))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "watchtower") {
		t.Errorf("a denied node type: %d %s", w.Code, w.Body)
	}
	if tokenDesignRefusal(asToken(t, app, u, l, "GET", "/", ""), json.RawMessage(`{"nodes":[{"id":"n1","type":"intranet"}]}`)) != "" {
		t.Error("a design without the denied type was refused")
	}
}

// A label-limited token applying a file cannot take over the owner's stack of that name, and
// what it creates carries its labels.
func TestTokenApplyStaysInItsStacks(t *testing.T) {
	app := newTestApp(t)
	app.docker = NewDocker(t.TempDir() + "/none.sock")
	u, _ := app.ensureOwner()
	app.store.CreateStack("lab", u.ID, "4h", expiryFor("4h"), []byte(defaultDesign))
	l := &tokenLimit{Labels: map[string]string{"kb-run": "1"}}
	file := func(name string) string {
		b, _ := json.Marshal(map[string]any{"file": "name: " + name + "\nttl: 2h\ndesign:\n  nodes: []\n  edges: []\n"})
		return string(b)
	}
	w := httptest.NewRecorder()
	app.handleApplyStackFile(w, asToken(t, app, u, l, "POST", "/api/stacks/apply", file("lab")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("applying over the owner's stack: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	app.handleApplyStackFile(w, asToken(t, app, u, l, "POST", "/api/stacks/apply", file("kbrun-1")))
	if w.Code != http.StatusOK {
		t.Fatalf("applying a new stack: %d %s", w.Code, w.Body)
	}
	id := app.store.StackIDByName(u.ID, "kbrun-1")
	st, _ := app.store.GetStack(id)
	if st.Labels["kb-run"] != "1" {
		t.Errorf("the new stack's labels: %v", st.Labels)
	}
}

func TestTokenLimitValidate(t *testing.T) {
	for l, bad := range map[*tokenLimit]bool{
		{MaxTTL: "8h"}: false, {MaxTTL: "infinity"}: true, {MaxTTL: "3h"}: true, {Deny: []string{" "}}: true,
	} {
		if err := l.validate(); (err != nil) != bad {
			t.Errorf("%+v: %v", *l, err)
		}
	}
	// A TTL cap alone does not narrow which stacks the token reaches.
	if (&tokenLimit{MaxTTL: "8h"}).stackLimited() || (&tokenLimit{MaxTTL: "8h"}).empty() {
		t.Error("a TTL cap is a limit, but not a stack limit")
	}
}
