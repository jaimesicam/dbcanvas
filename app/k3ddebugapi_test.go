package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// k3ddebugapi_test.go — target resolution, the source-path guard, and command dispatch.

// debugTestStack builds a stack whose design has one K3D frame with the debugger on, and a
// running server node carrying the k3dConfig a deploy would have recorded. tweak edits that
// config before it is stored, so a test can describe the state it wants.
func debugTestStack(t *testing.T, app *App, ownerID int64, tweak func(cfg *k3dConfig)) (Stack, designDoc) {
	t.Helper()
	doc := designDoc{
		Frames: []designFrame{{
			ID: "f1", Type: "k3d", Label: "k3d-01", K3DOperator: "pxc", K3DDebug: true,
		}},
		Nodes: []designNode{{ID: "n1", Type: "k3d", FrameID: "f1", Label: "k3d-01-1"}},
	}
	design, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal design: %v", err)
	}
	st, err := app.store.CreateStack("debug", ownerID, ttlInfinity, nil, design)
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	cfg := k3dConfig{
		Role: "server", Cluster: "dbcanvas-1-k3d-01", Namespace: "pxc",
		Operator: "pxc", OperatorVer: "1.20.0",
		OperatorSrc:   "/root/percona-xtradb-cluster-operator-1.20.0",
		DebugStatus:   "listening",
		DebugPort:     40000,
		DebugNodePort: 30400,
		DebugBuildDir: "/go/src/github.com/percona/percona-xtradb-cluster-operator",
	}
	if tweak != nil {
		tweak(&cfg)
	}
	raw, _ := json.Marshal(cfg)
	if err := app.store.UpsertDeployment(Deployment{
		StackID: st.ID, NodeID: "n1", ContainerID: "container1",
		State: DeployRunning, Config: raw,
	}); err != nil {
		t.Fatalf("upsert deployment: %v", err)
	}
	st, _ = app.store.GetStack(st.ID)
	return st, doc
}

// A frame deployed with the debugger resolves to a target with everything the session needs:
// where to dial, what prefix the binary was compiled with, and where its source is on the node.
func TestDebugResolveTarget(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)
	st, doc := debugTestStack(t, app, u.ID, nil)

	tgt, err := app.k3dDebugResolveTarget(st, doc, "f1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.ServerID != "container1" || tgt.Operator != "pxc" || tgt.NodePort != 30400 {
		t.Fatalf("target = %+v", tgt)
	}
	if tgt.BuildDir == "" || tgt.SrcDir == "" {
		t.Fatalf("target is missing its paths: %+v", tgt)
	}
	if tgt.Deployment != "percona-xtradb-cluster-operator" {
		t.Fatalf("deployment = %q", tgt.Deployment)
	}
	if tgt.key() != "1/f1" && !strings.HasSuffix(tgt.key(), "/f1") {
		t.Fatalf("key = %q", tgt.key())
	}
}

// The three ways there is nothing to debug need three different answers, because they need three
// different things done about them.
func TestDebugResolveTargetExplainsWhyNot(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)

	st, doc := debugTestStack(t, app, u.ID, func(cfg *k3dConfig) { cfg.DebugStatus = "" })
	if _, err := app.k3dDebugResolveTarget(st, doc, "f1"); err == nil ||
		!strings.Contains(err.Error(), "deploy again") {
		t.Fatalf("a frame deployed without the debugger: %v", err)
	}

	st2, doc2 := debugTestStack(t, app, u.ID, func(cfg *k3dConfig) {
		cfg.DebugStatus = "not attached: the debug build failed"
	})
	if _, err := app.k3dDebugResolveTarget(st2, doc2, "f1"); err == nil ||
		!strings.Contains(err.Error(), "the debug build failed") {
		t.Fatalf("a debugger that failed to attach should say so: %v", err)
	}

	if _, err := app.k3dDebugResolveTarget(st, doc, "nope"); err == nil {
		t.Fatal("an unknown frame should not resolve")
	}
}

// The source view must not become a way to read arbitrary files off the node.
func TestDebugCleanRel(t *testing.T) {
	ok := []string{"pkg/controller/pxc/controller.go", "cmd/manager/main.go", "go.mod"}
	bad := []string{"", "/etc/shadow", "../../etc/shadow", "pkg/../../x", "pkg//a.go", "./a.go"}
	for _, p := range ok {
		if got, err := k3dDebugCleanRel(p); err != nil || got != p {
			t.Errorf("%q should be accepted: %q, %v", p, got, err)
		}
	}
	for _, p := range bad {
		if _, err := k3dDebugCleanRel(p); err == nil {
			t.Errorf("%q should be refused", p)
		}
	}
}

// The presets are what most sessions actually use, so they have to be spelled the way Delve
// resolves a function breakpoint — package-qualified, with the pointer receiver.
func TestDebugPresetsLookLikeDelveSpecs(t *testing.T) {
	for op, presets := range k3dDebugPresets {
		if len(presets) == 0 {
			t.Errorf("no presets for %s", op)
		}
		for _, p := range presets {
			if p.Label == "" || p.Hint == "" {
				t.Errorf("%s: preset %+v needs a label and a hint", op, p)
			}
			if !strings.Contains(p.Func, ".(*") || !strings.Contains(p.Func, ").") {
				t.Errorf("%s: preset %q is not a package-qualified method spec", op, p.Func)
			}
		}
		// A "Reconcile" landmark is the one every operator must have — it is what the page tells
		// people to start from, and the Force-a-reconcile button exists to make it fire.
		if len(presets) > 0 && presets[0].Label != "Reconcile" {
			t.Errorf("%s: the first preset is %q, want Reconcile", op, presets[0].Label)
		}
	}
	// Every operator with presets must also have somewhere for the panel to open, or it starts
	// on a blank pane.
	for op := range k3dDebugPresets {
		if k3dDebugStartFile[op] == "" {
			t.Errorf("operator %q has presets but no start file", op)
		}
		if k3dDebugCRKind[op] == "" {
			t.Errorf("operator %q has presets but no CR kind to annotate", op)
		}
	}
	// And every debuggable operator is one of them — a frame that can be deployed under Delve
	// with no landmarks and no start file would open on nothing.
	for op := range k3dDebugProfiles {
		if len(k3dDebugPresets[op]) == 0 {
			t.Errorf("operator %q can be debugged but has no presets", op)
		}
	}
}

// Command dispatch: an unknown command is an error rather than a silent no-op, and the ones that
// need a debugger say so instead of panicking when there is none.
func TestDebugRunDispatch(t *testing.T) {
	app := &App{}
	sess := &k3dDebugSession{
		a: app, status: "detached",
		bps: map[string]map[int]k3dDebugBP{}, fns: map[string]k3dDebugFnBP{},
		subs: map[int]chan []byte{},
	}
	ctx := context.Background()
	tgt := k3dDebugTarget{Operator: "pxc"}

	if _, err := app.k3dDebugRun(ctx, sess, tgt, k3dDebugWSCmd{Cmd: "nonsense"}); err == nil {
		t.Fatal("an unknown command should be refused")
	}
	if _, err := app.k3dDebugRun(ctx, sess, tgt, k3dDebugWSCmd{Cmd: "continue"}); err == nil {
		t.Fatal("stepping with no debugger attached should be refused")
	}
	// state works whether or not anything is attached — it is what the panel draws on connect.
	data, err := app.k3dDebugRun(ctx, sess, tgt, k3dDebugWSCmd{Cmd: "state"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if _, ok := data.(map[string]any)["state"]; !ok {
		t.Fatalf("state reply = %+v", data)
	}
	// A breakpoint set with nothing attached is remembered, so it is armed on the next attach.
	if _, err := app.k3dDebugRun(ctx, sess, tgt, k3dDebugWSCmd{
		Cmd: "breakpoint", File: "pkg/controller/pxc/controller.go", Line: 236, On: true,
	}); err != nil {
		t.Fatalf("breakpoint: %v", err)
	}
	if st := sess.snapshot(); len(st.Breakpoints) != 1 {
		t.Fatalf("breakpoints = %+v", st.Breakpoints)
	}
}

// The targets endpoint only offers frames that are actually debuggable right now.
func TestDebugTargetsEndpoint(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("owner", "x", RoleAdmin, StatusApproved)
	debugTestStack(t, app, u.ID, nil)
	debugTestStack(t, app, u.ID, func(cfg *k3dConfig) { cfg.DebugStatus = "" }) // deployed without it

	if err := app.store.CreateSession("token1", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	r := httptest.NewRequest("GET", "/api/k3d/debug/targets", nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: "token1"})
	w := httptest.NewRecorder()
	app.handleK3DDebugTargets(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Targets []struct {
			Label     string           `json:"label"`
			Operator  string           `json:"operator"`
			StartFile string           `json:"startFile"`
			Presets   []k3dDebugPreset `json:"presets"`
			Attached  bool             `json:"attached"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Targets) != 1 {
		t.Fatalf("targets = %d, want only the one whose debugger is listening", len(body.Targets))
	}
	got := body.Targets[0]
	if got.Operator != "pxc" || got.StartFile == "" || len(got.Presets) == 0 || got.Attached {
		t.Fatalf("target = %+v", got)
	}
}
