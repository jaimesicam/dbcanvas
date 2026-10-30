package main

import (
	"path/filepath"
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
	u, err := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	expired, err := app.store.CreateStack("old", u.ID, "2h", &past, []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create expired stack: %v", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	live, err := app.store.CreateStack("live", u.ID, "2h", &future, []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create live stack: %v", err)
	}
	inf, err := app.store.CreateStack("forever", u.ID, ttlInfinity, nil, []byte(defaultDesign))
	if err != nil {
		t.Fatalf("create infinite stack: %v", err)
	}

	app.reapExpiredStacks()

	if got, _ := app.store.GetStack(expired.ID); got.Status != StackExpired {
		t.Errorf("expired stack status = %q, want %q", got.Status, StackExpired)
	}
	if got, _ := app.store.GetStack(live.ID); got.Status != StackDraft {
		t.Errorf("live stack status = %q, want %q (must not be reaped)", got.Status, StackDraft)
	}
	if got, _ := app.store.GetStack(inf.ID); got.Status != StackDraft {
		t.Errorf("infinite stack status = %q, want %q (never expires)", got.Status, StackDraft)
	}
}

func TestExpiryForTTL(t *testing.T) {
	if expiryFor(ttlInfinity) != nil {
		t.Error("infinity TTL must have nil expiry")
	}
	if p := expiryFor("24h"); p == nil {
		t.Error("24h TTL must have an expiry")
	}
	if !validTTL("2w") || validTTL("5m") {
		t.Error("validTTL gate is wrong")
	}
}

// The Database Stacks table's list (?detail=1): the owner, the canvas's node types, and how many
// of the deployed nodes run or failed — a node with no deployment counts in the total only.
func TestStackSummaries(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	design := `{"nodes":[{"id":"a","type":"pg","label":"pg01"},{"id":"b","type":"pg","label":"pg02"},{"id":"c","type":"pmm","label":"pmm"}],"frames":[],"edges":[]}`
	st, err := app.store.CreateStack("lab", u.ID, "2h", nil, []byte(design))
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "a", State: DeployRunning})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "b", State: DeployError})

	stacks, _ := app.store.ListStacks(u.ID, false)
	got := app.stackSummaries(stacks)
	if len(got) != 1 {
		t.Fatalf("got %d summaries, want 1", len(got))
	}
	s := got[0]
	if s.Owner != "alice" || s.Name != "lab" {
		t.Errorf("owner/name = %q/%q, want alice/lab", s.Owner, s.Name)
	}
	if s.Nodes != (stackNodeCount{Total: 3, Running: 1, Error: 1}) {
		t.Errorf("nodes = %+v, want total 3, running 1, error 1", s.Nodes)
	}
	if s.Types["pg"] != 2 || s.Types["pmm"] != 1 {
		t.Errorf("types = %v, want pg:2 pmm:1", s.Types)
	}
}
