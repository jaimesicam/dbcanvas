package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestTokenLimitRoutes(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("ci", "x", RoleUser, StatusApproved)
	secret, tok := tokenFor(t, app, u.ID, ScopeWrite, 30)
	app.store.SetAPITokenLimit(tok.ID, &tokenLimit{OwnStacks: true})
	plainSecret, _ := tokenFor(t, app, u.ID, ScopeWrite, 30)

	mine, _ := app.store.CreateStack("mine", u.ID, "4h", nil, []byte(`{}`))
	app.store.SetStackCreatedByToken(mine.ID, tok.ID)
	other, _ := app.store.CreateStack("other", u.ID, "4h", nil, []byte(`{}`))

	run := func(rt apiRoute, id int64, secret string) int {
		w := httptest.NewRecorder()
		r := bearerReq(rt.Method, rt.Path, secret)
		if id != 0 {
			r.SetPathValue("id", strconv.FormatInt(id, 10))
		}
		app.requireScope(rt, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })(w, r)
		return w.Code
	}
	deploy := apiRoute{Method: "POST", Path: "/api/stacks/{id}/deploy"}
	for _, c := range []struct {
		name   string
		rt     apiRoute
		id     int64
		secret string
		want   int
	}{
		{"its own stack", deploy, mine.ID, secret, 200},
		{"another stack of the same account", deploy, other.ID, secret, 403},
		{"an unlimited token on that stack", deploy, other.ID, plainSecret, 200},
		{"node routes of its own stack", apiRoute{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/exec"}, mine.ID, secret, 200},
		{"listing stacks", apiRoute{Method: "GET", Path: "/api/stacks"}, 0, secret, 200},
		{"composing", apiRoute{Method: "POST", Path: "/api/stacks/compose"}, 0, secret, 200},
		{"a catalogue", apiRoute{Method: "GET", Path: "/api/catalog/ps"}, 0, secret, 200},
		{"anything else", apiRoute{Method: "GET", Path: "/api/notifications"}, 0, secret, 403},
		{"minting a token", apiRoute{Method: "POST", Path: "/api/tokens"}, 0, secret, 403},
	} {
		if got := run(c.rt, c.id, c.secret); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTokenLimitByLabel(t *testing.T) {
	l := &tokenLimit{Labels: map[string]string{"tool": "ci"}}
	in := Stack{Labels: map[string]string{"tool": "ci", "run": "3"}}
	out := Stack{Labels: map[string]string{"tool": "other"}}
	if !l.allowsStack(9, in) || l.allowsStack(9, out) || l.allowsStack(9, Stack{}) {
		t.Fatal("label matching")
	}
	both := &tokenLimit{OwnStacks: true, Labels: map[string]string{"tool": "ci"}}
	in.CreatedByToken = 9
	if !both.allowsStack(9, in) || both.allowsStack(8, in) {
		t.Fatal("both limits must hold")
	}

	// A new stack gets the token's labels; a conflicting value is refused.
	r := withPrincipal(httptest.NewRequest("POST", "/api/stacks", nil), principal{Token: &APIToken{ID: 9, Limit: l}})
	got, err := labelsForNewStack(r, map[string]string{"run": "4"})
	if err != nil || got["tool"] != "ci" || got["run"] != "4" {
		t.Fatalf("merged %v %v", got, err)
	}
	if _, err := labelsForNewStack(r, map[string]string{"tool": "other"}); err == nil {
		t.Fatal("a conflicting label was accepted")
	}
	// The list is filtered to the limit.
	if kept := filterStacksForToken(r, []Stack{in, out}); len(kept) != 1 {
		t.Fatalf("kept %v", kept)
	}
}
