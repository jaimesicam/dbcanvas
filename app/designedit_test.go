package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// designEditFixture is a stack owned by a user, and a way to call the design handlers
// as that user.
type designEditFixture struct {
	app *App
	u   User
	st  Stack
}

func newDesignEditFixture(t *testing.T, design string) *designEditFixture {
	t.Helper()
	app := newTestApp(t)
	u, err := app.store.CreateUser("ada", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	st, err := app.store.CreateStack("s", u.ID, "8h", nil, []byte(design))
	if err != nil {
		t.Fatal(err)
	}
	return &designEditFixture{app: app, u: u, st: st}
}

// call runs a handler with the stack id and any extra path values ("nid", "x"…).
func (f *designEditFixture) call(t *testing.T, h func(*App, http.ResponseWriter, *http.Request), method, body string, pv ...string) (int, designEditResult, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest(method, "/x", strings.NewReader(body)), principal{User: f.u})
	r.SetPathValue("id", strconv.FormatInt(f.st.ID, 10))
	for i := 0; i+1 < len(pv); i += 2 {
		r.SetPathValue(pv[i], pv[i+1])
	}
	h(f.app, w, r)
	var res designEditResult
	json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res, w.Body.String()
}

func (f *designEditFixture) design(t *testing.T) *rawDesign {
	t.Helper()
	st, err := f.app.store.GetStack(f.st.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := parseRawDesign(st.Design)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDesignEditNodesAndRelations(t *testing.T) {
	f := newDesignEditFixture(t, `{"nodes":[{"id":"intranet-1","type":"intranet","label":"Intranet","x":10,"y":20,"note":"kept"}],"frames":[],"edges":[],"view":{"x":1,"y":2,"z":1}}`)
	add := (*App).handleDesignAddNode

	code, res, body := f.call(t, add, "POST", `{"type":"pmm","label":"pmm-a"}`)
	if code != http.StatusOK || res.Node == nil {
		t.Fatalf("add pmm: %d %s", code, body)
	}
	pmmID := mstr(res.Node, "id")
	if mstr(res.Node, "label") != "pmm-a" || !strings.HasPrefix(pmmID, "pmm-") {
		t.Errorf("pmm node: %v", res.Node)
	}

	// Refused: an unknown option, a relation field written as an option, a wrong JSON
	// type, a value outside a fixed set, a label in use, a relation to the wrong type.
	for _, c := range []struct{ body, want string }{
		{`{"type":"psm","options":{"noSuchField":1}}`, `"noSuchField" is not an option of psm`},
		{`{"type":"psm","options":{"pmmNodeId":"x"}}`, `the "monitor" relation`},
		{`{"type":"pmm","options":{"generateCert":"yes"}}`, `wants a JSON boolean`},
		{`{"type":"psm","label":"pmm-a"}`, `already used`},
		{`{"type":"psm","relations":{"monitor":"intranet-1"}}`, `must be one of pmm`},
		{`{"type":"psm","relations":{"backup":"intranet-1"}}`, `has no "backup" relation`},
		{`{"type":"nosuch"}`, `unknown node type`},
		{`{"type":"psmrs"}`, `create one with POST`},
		{`{"type":"intranet"}`, `at most one intranet`},
	} {
		code, _, body := f.call(t, add, "POST", c.body)
		var e struct{ Error string }
		json.Unmarshal([]byte(body), &e)
		if code < 400 || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s (want %q)", c.body, code, body, c.want)
		}
	}

	code, res, body = f.call(t, add, "POST", `{"type":"psm","relations":{"monitor":"`+pmmID+`"}}`)
	if code != http.StatusOK || mstr(res.Node, "pmmNodeId") != pmmID {
		t.Fatalf("psm with monitor: %d %s", code, body)
	}
	psmID := mstr(res.Node, "id")

	// Update: an option set, then cleared with null; the relation cleared.
	upd := (*App).handleDesignUpdateNode
	if code, res, body = f.call(t, upd, "PUT", `{"label":"mongo-a","x":500}`, "nid", psmID); code != http.StatusOK || mstr(res.Node, "label") != "mongo-a" || mnum(res.Node, "x") != 500 {
		t.Errorf("update: %d %s", code, body)
	}
	if code, res, body = f.call(t, upd, "PUT", `{"type":"pmm"}`, "nid", psmID); code != http.StatusBadRequest {
		t.Errorf("type change allowed: %d %s", code, body)
	}
	if code, res, body = f.call(t, upd, "PUT", `{"relations":{"monitor":null}}`, "nid", psmID); code != http.StatusOK || res.Node["pmmNodeId"] != nil {
		t.Errorf("clear relation: %d %s", code, body)
	}
	f.call(t, upd, "PUT", `{"relations":{"monitor":"`+pmmID+`"}}`, "nid", psmID)

	// A line to the PMM node, then the PMM node removed: the line and the relation go too.
	code, res, body = f.call(t, (*App).handleDesignAddEdge, "POST", `{"from":"`+psmID+`","to":"`+pmmID+`"}`)
	if code != http.StatusOK || res.Edge == nil {
		t.Fatalf("edge: %d %s", code, body)
	}
	if code, _, body = f.call(t, (*App).handleDesignAddEdge, "POST", `{"from":"`+pmmID+`","to":"`+psmID+`"}`); code != http.StatusConflict {
		t.Errorf("duplicate edge: %d %s", code, body)
	}
	if code, _, body = f.call(t, (*App).handleDesignDeleteNode, "DELETE", ``, "nid", pmmID); code != http.StatusOK {
		t.Fatalf("delete pmm: %d %s", code, body)
	}
	d := f.design(t)
	if d.node(pmmID) != nil || len(d.Edges) != 0 || mstr(d.node(psmID), "pmmNodeId") != "" {
		t.Errorf("after deleting pmm: nodes %v edges %v", d.Nodes, d.Edges)
	}
	// Keys the structs do not declare survive the edits.
	if mstr(d.node("intranet-1"), "note") != "kept" || string(d.top["view"]) != `{"x":1,"y":2,"z":1}` {
		t.Errorf("unknown keys lost: %v %s", d.node("intranet-1"), d.top["view"])
	}
}

func TestDesignEditClusters(t *testing.T) {
	f := newDesignEditFixture(t, `{"nodes":[{"id":"intranet-1","type":"intranet","label":"Intranet"}],"frames":[],"edges":[]}`)
	addFrame := (*App).handleDesignAddFrame

	code, res, body := f.call(t, addFrame, "POST", `{"type":"psmrs","members":3,"label":"rs0"}`)
	if code != http.StatusOK || res.Frame == nil || len(res.Members) != 3 {
		t.Fatalf("add psmrs: %d %s", code, body)
	}
	fid := mstr(res.Frame, "id")
	for _, m := range res.Members {
		if mstr(m, "frameId") != fid || mstr(m, "type") != "psmrs" {
			t.Errorf("member %v", m)
		}
	}
	if code, _, body = f.call(t, addFrame, "POST", `{"type":"psmrs","members":40}`); code != http.StatusBadRequest {
		t.Errorf("40 members: %d %s", code, body)
	}

	// A member added to the cluster, then removed; the frame grows to hold it.
	code, res, body = f.call(t, (*App).handleDesignAddNode, "POST", `{"frameId":"`+fid+`"}`)
	if code != http.StatusOK || mstr(res.Node, "frameId") != fid {
		t.Fatalf("add member: %d %s", code, body)
	}
	if len(f.design(t).members(fid)) != 4 {
		t.Error("member not added")
	}
	if code, _, body = f.call(t, (*App).handleDesignDeleteNode, "DELETE", ``, "nid", mstr(res.Node, "id")); code != http.StatusOK {
		t.Errorf("remove member: %d %s", code, body)
	}

	// Moving the frame moves its members.
	before := mnum(f.design(t).members(fid)[0], "x")
	if code, _, body = f.call(t, (*App).handleDesignUpdateFrame, "PUT", `{"x":`+strconv.Itoa(int(mnum(f.design(t).frame(fid), "x"))+100)+`}`, "fid", fid); code != http.StatusOK {
		t.Fatalf("move frame: %d %s", code, body)
	}
	if after := mnum(f.design(t).members(fid)[0], "x"); after != before+100 {
		t.Errorf("member x %v → %v", before, after)
	}

	// A sharded cluster takes its shape from psmdbSetup, not a count.
	code, res, body = f.call(t, addFrame, "POST", `{"type":"psmdb","options":{"psmdbSetup":"minimum"}}`)
	if code != http.StatusOK || len(res.Members) != 5 {
		t.Fatalf("psmdb minimum: %d %s", code, body)
	}
	roles := map[string]int{}
	for _, m := range res.Members {
		roles[mstr(m, "role")]++
	}
	if roles["mongos"] != 1 || roles["config"] != 1 || roles["shard"] != 3 {
		t.Errorf("psmdb roles %v", roles)
	}
	if code, _, body = f.call(t, addFrame, "POST", `{"type":"psmdb","members":5}`); code != http.StatusBadRequest {
		t.Errorf("psmdb with a count: %d %s", code, body)
	}

	// Removing the cluster removes its members.
	if code, res, body = f.call(t, (*App).handleDesignDeleteFrame, "DELETE", ``, "fid", fid); code != http.StatusOK || len(res.Removed) != 4 {
		t.Errorf("delete frame: %d %s", code, body)
	}
	if d := f.design(t); d.frame(fid) != nil || len(d.members(fid)) != 0 {
		t.Error("frame or members left behind")
	}
}

func TestComposeOptionsAreCatalogued(t *testing.T) {
	_, _, _, _, err := buildCompose(composeSpec{Nodes: []composeNodeSpec{{Kind: "pmm", Options: map[string]json.RawMessage{"noSuchField": json.RawMessage(`1`)}}}})
	if err == nil || !strings.Contains(err.Error(), `"noSuchField" is not an option of pmm`) {
		t.Errorf("unknown option: %v", err)
	}
	_, _, _, _, err = buildCompose(composeSpec{Nodes: []composeNodeSpec{{Kind: "pmm", MemberOptions: map[string]json.RawMessage{"generateCert": json.RawMessage(`true`)}}}})
	if err == nil || !strings.Contains(err.Error(), "memberOptions is for a cluster") {
		t.Errorf("memberOptions on a node: %v", err)
	}
	doc, _, _, _, err := buildCompose(composeSpec{Nodes: []composeNodeSpec{{Kind: "pmm", Options: map[string]json.RawMessage{"generateCert": json.RawMessage(`true`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range doc.Nodes {
		if n.Type == "pmm" && !n.GenerateCert {
			t.Error("option not applied")
		}
	}
}
