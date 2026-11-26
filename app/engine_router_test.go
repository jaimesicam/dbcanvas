package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func runtimeDoc() designDoc {
	return designDoc{
		Frames: []designFrame{{ID: "f1", Type: "pxc", Label: "pxc-cluster-01", Runtime: runtimeVM}},
		Nodes: []designNode{
			{ID: "intranet-01", Type: "intranet"},
			{ID: "pxc-1", Type: "pxc", FrameID: "f1", Runtime: runtimeDocker}, // overrides its cluster
			{ID: "pxc-2", Type: "pxc", FrameID: "f1"},                         // follows its cluster
			{ID: "ps-01", Type: "ps", Runtime: runtimeVM},
			{ID: "pg-01", Type: "pg"},
		},
	}
}

func TestVMNodeIDs(t *testing.T) {
	got := strings.Join(vmNodeIDs(runtimeDoc()), ",")
	if got != "ps-01,pxc-2" {
		t.Fatalf("VM nodes %q, want ps-01,pxc-2", got)
	}
}

func TestRuntimeIssues(t *testing.T) {
	msgs := func(is []issue) string {
		var b []string
		for _, i := range is {
			b = append(b, i.Level+": "+i.Message)
		}
		return strings.Join(b, "\n")
	}
	doc := runtimeDoc()

	// PXC is switched on, Percona Server is not: only the PS node is refused.
	got := msgs(runtimeIssues(doc, ""))
	if !strings.Contains(got, "ps-01: running this node type as a VM is not available yet") {
		t.Errorf("a type that is not switched on was accepted:\n%s", got)
	}
	if strings.Contains(got, "pxc") || strings.Contains(got, "pg-01") {
		t.Errorf("a switched-on or container node was flagged:\n%s", got)
	}

	// Enabled for preview, on a machine with Lima: fine.
	t.Setenv("DBCANVAS_VM_PREVIEW", "pxc,ps")
	if got := runtimeIssues(doc, ""); len(got) != 0 {
		t.Errorf("enabled types refused: %s", msgs(got))
	}
	if !vmEnabled() {
		t.Error("vmEnabled is false with types in preview")
	}

	// Enabled, but Lima is missing: refused with the reason.
	got = msgs(runtimeIssues(doc, "limactl is not installed (https://lima-vm.io)"))
	if !strings.Contains(got, "ps-01: cannot run as a VM on this machine: limactl is not installed") {
		t.Errorf("no reason given without Lima:\n%s", got)
	}

	// Types that arrive as ready-made images never run as a VM, preview or not.
	t.Setenv("DBCANVAS_VM_PREVIEW", "intranet,vnc,k3d,pmm")
	bad := designDoc{Nodes: []designNode{
		{ID: "intranet-01", Type: "intranet", Runtime: runtimeVM},
		{ID: "vnc-01", Type: "vnc", Runtime: runtimeVM},
		{ID: "pmm-01", Type: "pmm", Runtime: runtimeVM},
		{ID: "x", Type: "ps", Runtime: "kvm"},
	}, Frames: []designFrame{{ID: "k", Type: "k3d", Runtime: runtimeVM}}}
	got = msgs(runtimeIssues(bad, ""))
	for _, id := range []string{"intranet-01", "vnc-01", "pmm-01", "k"} {
		if !strings.Contains(got, id+": this node type cannot run as a VM") {
			t.Errorf("%s accepted as a VM:\n%s", id, got)
		}
	}
	if !strings.Contains(got, `x: runtime "kvm" is not docker or vm`) {
		t.Errorf("unknown runtime accepted:\n%s", got)
	}
	t.Setenv("DBCANVAS_VM_PREVIEW", "ps")
	got = msgs(runtimeIssues(designDoc{Nodes: []designNode{{ID: "ps-01", Type: "ps", Runtime: runtimeVM}}}, ""))
	if !strings.Contains(got, "add an Intranet node") {
		t.Errorf("a VM node without an Intranet was accepted:\n%s", got)
	}
	t.Setenv("DBCANVAS_VM_PREVIEW", "intranet,vnc,k3d,pmm")
	if got := strings.Join(vmEnabledTypes(), ","); got != "pxc" {
		t.Errorf("enabled types %q with only ready-made types in preview, want pxc", got)
	}
}

func TestEngineChoice(t *testing.T) {
	a := &App{docker: &Docker{}}
	design, _ := json.Marshal(runtimeDoc())
	st := Stack{ID: 7, Design: design}
	plain, _ := json.Marshal(designDoc{Nodes: []designNode{{ID: "ps-01", Type: "ps"}}})

	// Without Lima everything is Docker, VM nodes or not (validation refuses those).
	if a.eng(st) != Engine(a.docker) || a.anyEngine() != Engine(a.docker) || a.engCtx(t.Context()) != Engine(a.docker) {
		t.Fatal("without Lima the engine must be Docker itself")
	}

	a.lima = &Lima{}
	// A stack with no VM node keeps Docker itself, so it behaves as it always did.
	if a.eng(Stack{ID: 8, Design: plain}) != Engine(a.docker) {
		t.Fatal("a container-only stack must use Docker directly")
	}
	r, ok := a.eng(st).(*engineRouter)
	if !ok {
		t.Fatalf("a stack with VM nodes got %T", a.eng(st))
	}
	if r.byName(containerName(7, "pxc-2")) != Engine(a.lima) || r.byName(containerName(7, "ps-01")) != Engine(a.lima) {
		t.Error("a VM node is not created on Lima")
	}
	if r.byName(containerName(7, "pxc-1")) != Engine(a.docker) || r.byName(containerName(7, "intranet-01")) != Engine(a.docker) {
		t.Error("a container node is not created on Docker")
	}
	if r.byID("dbc-0123456789ab") != Engine(a.lima) || r.byID(strings.Repeat("ab", 32)) != Engine(a.docker) {
		t.Error("ids are not told apart")
	}
	if asDocker(r, "dbc-0123456789ab") != nil || asDocker(r, "abc123") != a.docker || asDocker(a.docker, "x") != a.docker {
		t.Error("asDocker")
	}
}

// TestVMsOptional: without limactl, doctor passes with a note and only VM nodes are refused.
func TestVMsOptional(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DBCANVAS_DATA", t.TempDir())
	l, why := NewLima(limaHomeFor(t.TempDir()))
	if l != nil || !strings.Contains(why, "limactl is not installed") {
		t.Fatalf("NewLima without limactl: %v %q", l, why)
	}
	a := &App{docker: &Docker{}, limaWhy: why}
	checks := a.vmDoctorChecks()
	if len(checks) != 1 || checks[0].Status != "ok" || !strings.Contains(checks[0].Detail, "optional, not available") {
		t.Fatalf("doctor without Lima: %+v", checks)
	}
	if got := runtimeIssues(designDoc{Nodes: []designNode{{ID: "ps-01", Type: "ps"}}}, a.vmUnavailable()); len(got) != 0 {
		t.Fatalf("a container-only stack was refused: %+v", got)
	}
}

func TestComposeRuntime(t *testing.T) {
	composeCatalogFixture(t)
	spec := composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet"}, {Kind: "pxc", Count: 2, Runtime: runtimeVM}, {Kind: "ps", Runtime: runtimeVM}}}
	if _, _, _, _, err := buildCompose(spec); err == nil || !strings.Contains(err.Error(), `ps does not support "runtime"`) {
		t.Fatalf("runtime=vm accepted for a kind not switched on: %v", err)
	}
	t.Setenv("DBCANVAS_VM_PREVIEW", "pxc,ps")
	doc, _, _, _, err := buildCompose(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(vmNodeIDs(doc), ","); strings.Count(got, ",") != 2 {
		t.Fatalf("VM nodes %q: want both PXC members and the PS node", got)
	}
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet", Runtime: runtimeVM}}}); err == nil {
		t.Fatal("the Intranet was allowed to run as a VM")
	}
}
