package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGroupPolicyTTL(t *testing.T) {
	p := GroupPolicy{MaxTTL: "24h"}
	for ttl, want := range map[string]bool{"2h": true, "24h": true, "2w": false, "infinity": false} {
		if ok, _ := p.ttlAllowed(ttl); ok != want {
			t.Errorf("%s: %v", ttl, ok)
		}
	}
	if ok, _ := (GroupPolicy{}).ttlAllowed("infinity"); !ok {
		t.Error("no limit refused infinity")
	}
	soon := time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339)
	late := time.Now().Add(30 * time.Hour).UTC().Format(time.RFC3339)
	if ok, _ := p.expiryAllowed(&soon); !ok {
		t.Error("an expiry inside the limit was refused")
	}
	if ok, _ := p.expiryAllowed(&late); ok {
		t.Error("an extension past the limit was allowed")
	}
	if ok, _ := p.expiryAllowed(nil); ok {
		t.Error("never expiring was allowed under a 24h limit")
	}
}

func TestGroupPolicyLimits(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("ann", "x", RoleUser, StatusApproved)
	w := httptest.NewRecorder()
	app.handleSetGroupPolicies(w, httptest.NewRequest("PUT", "/api/security/policies", strings.NewReader(
		`{"policies": {"user": {"maxStacks": 1, "maxContainers": 2, "deniedNodeTypes": ["k3d"]}}}`)))
	if w.Code != 200 {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if p := app.policyFor(u); p.MaxStacks != 1 || p.MaxContainers != 2 || p.DeniedNodeTypes[0] != "k3d" {
		t.Fatalf("stored %+v", p)
	}
	if ok, _ := app.stackCountAllowed(u); !ok {
		t.Fatal("the first stack was refused")
	}
	st, _ := app.store.CreateStack("one", u.ID, "4h", nil, []byte(`{}`))
	if ok, why := app.stackCountAllowed(u); ok || !strings.Contains(why, "allows 1 stack per account") {
		t.Fatalf("a second stack: %v %q", ok, why)
	}
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Type: "k3d", Label: "k8s"}},
		Nodes:  []designNode{{ID: "a", Type: "pg", Label: "pg-01"}, {ID: "b", Type: "pmm", Label: "pmm-01"}, {ID: "c", Type: "pmm", Label: "pmm-02"}},
	}
	if is := app.nodeTypeIssues(st, doc); len(is) != 1 || !strings.Contains(is[0].Message, "k8s (k3d)") {
		t.Fatalf("node types: %+v", is)
	}
	if ok, _ := app.containerCountAllowed(st, doc); ok {
		t.Fatal("three nodes passed a limit of two")
	}
	doc.Nodes = doc.Nodes[:2]
	if ok, why := app.containerCountAllowed(st, doc); !ok {
		t.Fatalf("two nodes: %s", why)
	}
	// Other groups are unaffected.
	if p := app.groupPolicies()["admin"]; p.MaxStacks != 0 || len(p.DeniedNodeTypes) != 0 {
		t.Fatalf("admin got %+v", p)
	}
	w = httptest.NewRecorder()
	app.handleSetGroupPolicies(w, httptest.NewRequest("PUT", "/api/security/policies", strings.NewReader(`{"policies": {"user": {"maxTtl": "1y"}}}`)))
	if w.Code != 400 {
		t.Fatalf("a bad maxTtl: %d", w.Code)
	}
}
