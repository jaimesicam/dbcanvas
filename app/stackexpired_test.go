package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

// Deploying an expired stack starts its lifetime over, from now, before anything is built:
// left past its expiry, the reaper would tear the new build straight down.
func TestDeployingAnExpiredStackStartsItsLifetimeOver(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)
	st, err := app.store.CreateStack("old", u.ID, "2h", nil, []byte(`{"nodes":[],"frames":[],"edges":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	app.store.SetStackExpiry(st.ID, "2h", &past)
	app.store.SetStackStatus(st.ID, StackExpired)
	st, _ = app.store.GetStack(st.ID)

	w := httptest.NewRecorder()
	if !app.renewExpiredStack(w, &st, u) {
		t.Fatalf("renewing refused: %d %s", w.Code, w.Body.String())
	}
	got, _ := app.store.GetStack(st.ID)
	if got.Status != StackDraft || got.TTL != "2h" || got.ExpiresAt == nil {
		t.Fatalf("after renewing: status %q, ttl %q, expiresAt %v; want draft, 2h, set", got.Status, got.TTL, got.ExpiresAt)
	}
	exp, _ := time.Parse(time.RFC3339, *got.ExpiresAt)
	if d := time.Until(exp); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Errorf("the new expiry is %s away, want 2h", d.Round(time.Second))
	}
	expired, _ := app.store.ListExpiredStacks()
	for _, e := range expired {
		if e.ID == st.ID {
			t.Error("the renewed stack is still one the reaper would tear down")
		}
	}
}
