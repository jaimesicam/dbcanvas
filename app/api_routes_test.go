package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// api_routes_test.go — the tests that make the route table trustworthy.
//
// Two of them do real work rather than checking a data structure. TestRouteTableIsRegistrable
// installs every route on a live mux, which is the only way to find out that Go's
// pattern parser accepts them and that no two conflict — ServeMux panics on both, at
// startup, in production. TestAdminRoutesUnchanged pins the five admin-only routes,
// so the conversion from 206 loose registrations to a table cannot have quietly
// widened access anywhere.

func TestEveryRouteIsDocumented(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range apiGroupOrder {
		groups[g] = true
	}
	for _, rt := range apiRoutes() {
		if rt.Group == "" {
			t.Errorf("%s: no Group — add it to a g* constant and to apiGroupOrder", rt.Pattern())
		} else if !groups[rt.Group] {
			t.Errorf("%s: group %q is missing from apiGroupOrder, so it renders last", rt.Pattern(), rt.Group)
		}
		switch {
		case rt.Summary == "":
			t.Errorf("%s: no Summary. One sentence saying what it does.", rt.Pattern())
		case !strings.HasSuffix(rt.Summary, ".") && !strings.HasSuffix(rt.Summary, "\""):
			t.Errorf("%s: Summary should be a sentence ending in a period: %q", rt.Pattern(), rt.Summary)
		case len(rt.Summary) < 20:
			t.Errorf("%s: Summary is too short to be useful: %q", rt.Pattern(), rt.Summary)
		}
		if rt.Handler == nil {
			t.Errorf("%s: no Handler", rt.Pattern())
		}
	}
}

func TestRoutePathsAreWellFormed(t *testing.T) {
	for _, rt := range apiRoutes() {
		switch rt.Method {
		case "GET", "POST", "PUT", "DELETE":
		default:
			t.Errorf("%s: unexpected method %q", rt.Pattern(), rt.Method)
		}
		if !strings.HasPrefix(rt.Path, "/api/") {
			t.Errorf("%s: every endpoint lives under /api/", rt.Pattern())
		}
		if strings.HasSuffix(rt.Path, "/") {
			t.Errorf("%s: trailing slash makes this a subtree pattern, which is not what is meant", rt.Pattern())
		}
		// Every wildcard needs an explanation, or the API page shows a blank one.
		for _, mt := range pathParamRe.FindAllStringSubmatch(rt.Path, -1) {
			if paramHelp(rt.Path, mt[1]) == "" {
				t.Errorf("%s: path parameter {%s} has no help — add it to paramDoc in apimeta.go",
					rt.Pattern(), mt[1])
			}
		}
	}
}

func TestNoDuplicateRoutes(t *testing.T) {
	seen := map[string]bool{}
	for _, rt := range apiRoutes() {
		if seen[rt.Pattern()] {
			t.Errorf("%s is registered twice; ServeMux would panic at startup", rt.Pattern())
		}
		seen[rt.Pattern()] = true
	}
}

// TestRouteTableIsRegistrable proves the table can actually be installed: a bad
// pattern or a conflicting pair panics inside ServeMux, and finding that out here
// beats finding it out when the container will not start.
func TestRouteTableIsRegistrable(t *testing.T) {
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("registering the route table panicked: %v", v)
		}
	}()
	app := &App{}
	app.registerRoutes(http.NewServeMux())
}

// TestAdminRoutesUnchanged pins the admin-only set. These five were the routes
// wrapped in requireAdmin before the route table existed; if this list changes,
// somebody either widened or narrowed access, and it should be on purpose.
func TestAdminRoutesUnchanged(t *testing.T) {
	want := map[string]bool{
		"PUT /api/system/settings":      true,
		"GET /api/users":                true,
		"POST /api/users/{id}/approve":  true,
		"POST /api/users/{id}/reject":   true,
		"POST /api/users/{id}/disable":  true,
		"DELETE /api/users/{id}":        true,
		"GET /api/admin/tokens":         true, // added with API tokens
		"DELETE /api/admin/tokens/{id}": true, // added with API tokens
	}
	got := map[string]bool{}
	for _, rt := range apiRoutes() {
		if rt.Auth == authAdmin {
			got[rt.Pattern()] = true
		}
	}
	for p := range want {
		if !got[p] {
			t.Errorf("%s is no longer admin-only", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("%s is newly admin-only; add it here if that is intended", p)
		}
	}
}

// TestPublicRoutesUnchanged pins the unauthenticated set for the same reason. These
// are the only five endpoints reachable without a session, because they are the ones
// an account needs before it has one.
func TestPublicRoutesUnchanged(t *testing.T) {
	want := []string{
		"GET /api/setup/status",
		"POST /api/setup",
		"POST /api/auth/register",
		"POST /api/auth/login",
		"POST /api/auth/logout",
	}
	got := map[string]bool{}
	for _, rt := range apiRoutes() {
		if rt.Auth == authPublic {
			got[rt.Pattern()] = true
		}
	}
	if len(got) != len(want) {
		t.Errorf("expected %d public routes, found %d: %v", len(want), len(got), got)
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("%s is no longer public — sign-in would be impossible", p)
		}
	}
}

// TestReadOnlyRoutesAreNotGets catches the copy-paste error the ReadOnly flag
// invites: marking a GET read-only, which is meaningless, or marking a DELETE
// read-only, which is wrong.
func TestReadOnlyRoutesAreNotGets(t *testing.T) {
	for _, rt := range apiRoutes() {
		if !rt.ReadOnly {
			continue
		}
		if rt.Method == "GET" {
			t.Errorf("%s: ReadOnly on a GET is redundant", rt.Pattern())
		}
		if rt.Method == "DELETE" || rt.Method == "PUT" {
			t.Errorf("%s: a %s cannot be read-only", rt.Pattern(), rt.Method)
		}
	}
}

// TestRouteScopes checks the scope derivation on the cases that matter: a GET is a
// read, a plain POST is a write, a ReadOnly POST is a read, and an admin route needs
// admin however it is spelled.
func TestRouteScopes(t *testing.T) {
	byPattern := map[string]apiRoute{}
	for _, rt := range apiRoutes() {
		byPattern[rt.Pattern()] = rt
	}
	cases := []struct{ pattern, want string }{
		{"GET /api/stacks", ScopeRead},
		{"POST /api/stacks", ScopeWrite},
		{"DELETE /api/stacks/{id}", ScopeWrite},
		{"POST /api/stacks/{id}/deploy", ScopeWrite},
		{"POST /api/stacks/{id}/validate", ScopeRead}, // ReadOnly: builds nothing
		{"POST /api/ftdc/compare", ScopeRead},         // ReadOnly: reads two windows
		{"POST /api/datagen/stacks/{id}/nodes/{nid}/preview", ScopeRead},
		{"POST /api/datagen/stacks/{id}/nodes/{nid}/generate", ScopeWrite},
		{"GET /api/users", ScopeAdmin},
		{"PUT /api/system/settings", ScopeAdmin},
		{"GET /api/system/settings", ScopeRead},
	}
	for _, c := range cases {
		rt, ok := byPattern[c.pattern]
		if !ok {
			t.Fatalf("%s is not in the route table", c.pattern)
		}
		if got := routeScope(rt); got != c.want {
			t.Errorf("%s: scope %q, want %q", c.pattern, got, c.want)
		}
	}
}

// ------------------------------------------------------------- catalogue

func TestGroupedEndpointsCoversEveryRoute(t *testing.T) {
	n := 0
	for _, g := range groupedEndpoints() {
		if g.Name == "" {
			t.Error("a group came back with no name")
		}
		if len(g.Endpoints) == 0 {
			t.Errorf("group %q is empty", g.Name)
		}
		n += len(g.Endpoints)
	}
	if n != len(apiRoutes()) {
		t.Errorf("the catalogue lists %d endpoints, the table has %d", n, len(apiRoutes()))
	}
}

// TestGroupedEndpointsFollowsOrder checks that the presentation order is the one
// apiGroupOrder asks for, since that is the only thing making the API page read as a
// tour rather than a dump.
func TestGroupedEndpointsFollowsOrder(t *testing.T) {
	got := []string{}
	for _, g := range groupedEndpoints() {
		got = append(got, g.Name)
	}
	want := []string{}
	for _, name := range apiGroupOrder {
		for _, g := range got {
			if g == name {
				want = append(want, name)
				break
			}
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("group %d is %q, want %q (order: %v)", i, got[i], want[i], got)
		}
	}
}

func TestEndpointsHandlerNeedsAuth(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.handleAPIEndpoints(w, httptest.NewRequest("GET", "/api/meta/endpoints", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /api/meta/endpoints returned %d, want 401", w.Code)
	}
}

// ------------------------------------------------------------- OpenAPI

func TestOpenAPIDocumentIsWellFormed(t *testing.T) {
	doc := openAPIDocument()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("the OpenAPI document does not marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the OpenAPI document does not round-trip: %v", err)
	}
	if back["openapi"] != "3.1.0" {
		t.Errorf("openapi version is %v", back["openapi"])
	}
	paths, ok := back["paths"].(map[string]any)
	if !ok {
		t.Fatal("no paths object")
	}
	// Every distinct path in the table must appear exactly once, carrying each of
	// its methods.
	wantMethods := map[string]int{}
	for _, rt := range apiRoutes() {
		wantMethods[rt.Path]++
	}
	if len(paths) != len(wantMethods) {
		t.Errorf("document has %d paths, the table has %d distinct ones", len(paths), len(wantMethods))
	}
	for p, n := range wantMethods {
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Errorf("%s is missing from the document", p)
			continue
		}
		if len(item) != n {
			t.Errorf("%s carries %d operations, want %d", p, len(item), n)
		}
	}
}

func TestOpenAPIOperationIDsAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, rt := range apiRoutes() {
		id := operationID(rt)
		if id == "" {
			t.Errorf("%s: empty operationId", rt.Pattern())
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("operationId %q is shared by %s and %s — a generated client would collide",
				id, prev, rt.Pattern())
		}
		seen[id] = rt.Pattern()
	}
}

func TestOpenAPIPublicRoutesOptOutOfSecurity(t *testing.T) {
	doc := openAPIDocument()
	paths := doc["paths"].(map[string]any)
	for _, rt := range apiRoutes() {
		op := paths[rt.Path].(map[string]any)[strings.ToLower(rt.Method)].(map[string]any)
		sec, has := op["security"]
		if rt.Auth == authPublic {
			if !has {
				t.Errorf("%s is public but inherits the global bearer requirement", rt.Pattern())
			} else if list, _ := sec.([]any); len(list) != 0 {
				t.Errorf("%s: public routes carry an empty security list, got %v", rt.Pattern(), sec)
			}
			continue
		}
		if has {
			t.Errorf("%s overrides security but is not public", rt.Pattern())
		}
	}
}

// TestOperationIDShape documents what the ids look like, because a generated
// client's method names come from them and quietly changing the shape renames
// somebody's code.
func TestOperationIDShape(t *testing.T) {
	byPattern := map[string]apiRoute{}
	for _, rt := range apiRoutes() {
		byPattern[rt.Pattern()] = rt
	}
	for pattern, want := range map[string]string{
		"GET /api/stacks":                  "getStacks",
		"POST /api/stacks/{id}/deploy":     "postStacksByIdDeploy",
		"GET /api/stacks/{id}":             "getStacksById",
		"GET /api/stacks/{id}/nodes/{nid}": "getStacksByIdNodesByNid",
		"GET /api/meta/openapi.json":       "getMetaOpenapijson",
		"DELETE /api/admin/tokens/{id}":    "deleteAdminTokensById",
	} {
		rt, ok := byPattern[pattern]
		if !ok {
			t.Fatalf("%s is not in the route table", pattern)
		}
		if got := operationID(rt); got != want {
			t.Errorf("%s: operationId %q, want %q", pattern, got, want)
		}
	}
}
