package main

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// Tests see QEMU as installed, whatever the machine running them has; TestVMNeedsQEMU takes it away.
func init() {
	qemuLookPath = func(bin string) (string, error) { return "/usr/bin/" + bin, nil }
}

// vmTypeOff switches a node type's VM support off for one test.
func vmTypeOff(t *testing.T, typ string) {
	was := vmNodeTypes[typ]
	vmNodeTypes[typ] = false
	t.Cleanup(func() { vmNodeTypes[typ] = was })
}

func runtimeDoc() designDoc {
	return designDoc{
		Frames: []designFrame{{ID: "f1", Type: "pxc", Label: "pxc-cluster-01", Runtime: runtimeVM}},
		Nodes: []designNode{
			{ID: "intranet-01", Type: "intranet"},
			{ID: "pxc-1", Type: "pxc", FrameID: "f1", Runtime: runtimeDocker}, // overrides its cluster
			{ID: "pxc-2", Type: "pxc", FrameID: "f1"},                         // follows its cluster
			{ID: "aio-01", Type: "aio", Runtime: runtimeVM},
			{ID: "pg-01", Type: "pg"},
		},
	}
}

func TestVMNodeIDs(t *testing.T) {
	got := strings.Join(vmNodeIDs(runtimeDoc()), ",")
	if got != "aio-01,pxc-2" {
		t.Fatalf("VM nodes %q, want aio-01,pxc-2", got)
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
	vmTypeOff(t, "aio")

	// PXC is switched on, All in One is not: only the All in One node is refused.
	got := msgs(runtimeIssues(doc, ""))
	if !strings.Contains(got, "aio-01: running this node type as a VM is not available yet") {
		t.Errorf("a type that is not switched on was accepted:\n%s", got)
	}
	if strings.Contains(got, "pxc") || strings.Contains(got, "pg-01") {
		t.Errorf("a switched-on or container node was flagged:\n%s", got)
	}

	// Enabled for preview, on a machine with Lima: fine.
	t.Setenv("DBCANVAS_VM_PREVIEW", "aio")
	if got := runtimeIssues(doc, ""); len(got) != 0 {
		t.Errorf("enabled types refused: %s", msgs(got))
	}
	if !vmEnabled() {
		t.Error("vmEnabled is false with types in preview")
	}

	// Enabled, but Lima is missing: refused with the reason.
	got = msgs(runtimeIssues(doc, "limactl is not installed (https://lima-vm.io)"))
	if !strings.Contains(got, "aio-01: cannot run as a VM on this machine: limactl is not installed") {
		t.Errorf("no reason given without Lima:\n%s", got)
	}

	// Types that arrive as ready-made images never run as a VM, preview or not.
	t.Setenv("DBCANVAS_VM_PREVIEW", "intranet,vnc,k3d,pmm,stocksim")
	bad := designDoc{Nodes: []designNode{
		{ID: "intranet-01", Type: "intranet", Runtime: runtimeVM},
		{ID: "vnc-01", Type: "vnc", Runtime: runtimeVM},
		{ID: "pmm-01", Type: "pmm", Runtime: runtimeVM},
		{ID: "stocksim-01", Type: "stocksim", Runtime: runtimeVM},
		{ID: "x", Type: "ps", Runtime: "kvm"},
	}, Frames: []designFrame{{ID: "k", Type: "k3d", Runtime: runtimeVM}}}
	got = msgs(runtimeIssues(bad, ""))
	for _, id := range []string{"intranet-01", "vnc-01", "pmm-01", "stocksim-01", "k"} {
		if !strings.Contains(got, id+": this node type cannot run as a VM") {
			t.Errorf("%s accepted as a VM:\n%s", id, got)
		}
	}
	if !strings.Contains(got, `x: runtime "kvm" is not docker or vm`) {
		t.Errorf("unknown runtime accepted:\n%s", got)
	}
	got = msgs(runtimeIssues(designDoc{Nodes: []designNode{{ID: "ps-01", Type: "ps", Runtime: runtimeVM}}}, ""))
	if !strings.Contains(got, "add an Intranet node") {
		t.Errorf("a VM node without an Intranet was accepted:\n%s", got)
	}
	t.Setenv("DBCANVAS_VM_PREVIEW", "intranet,vnc,k3d,pmm")
	if got := strings.Join(vmEnabledTypes(), ","); got != "haproxy,innodb,kubeadm,linuxclient,mariadb,mariadbgalera,mariadbrepl,mysql,mysqlce,mysqlceinnodb,mysqlcerepl,openbao,orchestrator,patroni,pg,pgbouncer,pgstream,proxysql,ps,psm,psmdb,psmrs,pxc,repmgr,sambaad,spock,valkey,valkeycluster" {
		t.Errorf("enabled types %q with only ready-made types in preview", got)
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
	// A stack known only by its id still tells a VM's id from a container's.
	if r, ok := a.eng(Stack{ID: 7}).(*engineRouter); !ok || r.byID("dbc-0123456789ab") != Engine(a.lima) {
		t.Error("a stack without its design must still route a VM's id to Lima")
	}
	r, ok := a.eng(st).(*engineRouter)
	if !ok {
		t.Fatalf("a stack with VM nodes got %T", a.eng(st))
	}
	if r.byName(containerName(7, "pxc-2")) != Engine(a.lima) || r.byName(containerName(7, "aio-01")) != Engine(a.lima) {
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
	doc, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet"}, {Kind: "pxc", Count: 2, Runtime: runtimeVM}, {Kind: "mysql", Runtime: runtimeVM}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(vmNodeIDs(doc), ","); strings.Count(got, ",") != 2 {
		t.Fatalf("VM nodes %q: want both PXC members and the MySQL node", got)
	}
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet", Runtime: runtimeVM}}}); err == nil || !strings.Contains(err.Error(), `does not support "runtime"`) {
		t.Fatalf("the Intranet was allowed to run as a VM: %v", err)
	}
	if _, _, _, _, err := buildCompose(composeSpec{Name: "x", Nodes: []composeNodeSpec{{Kind: "intranet"}, {Kind: "pmm", Runtime: runtimeVM}}}); err == nil {
		t.Fatal("PMM, a ready-made image, was allowed to run as a VM")
	}
}

func TestVMSELinuxModes(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{
			{ID: "v", Type: "valkeycluster", Runtime: runtimeVM},
			{ID: "p", Type: "pxc", Runtime: runtimeVM, SELinux: selinuxPermissive},
		},
		Nodes: []designNode{
			{ID: "valkey-1", Type: "valkeycluster", FrameID: "v"},                            // its type's default
			{ID: "valkey-2", Type: "valkeycluster", FrameID: "v", SELinux: selinuxEnforcing}, // its own
			{ID: "pxc-1", Type: "pxc", FrameID: "p"},                                         // its cluster's
			{ID: "ps-01", Type: "ps", Runtime: runtimeVM},                                    // its type's default
			{ID: "valkey-01", Type: "valkey", Runtime: runtimeVM},
			{ID: "pg-01", Type: "pg"}, // a container: no mode
		},
	}
	design, _ := json.Marshal(doc)
	got := stackVMSELinux(Stack{ID: 3, Design: design})
	want := map[string]string{
		"valkey-1": selinuxPermissive, "valkey-2": selinuxEnforcing, "pxc-1": selinuxPermissive,
		"ps-01": selinuxEnforcing, "valkey-01": selinuxPermissive,
	}
	if len(got) != len(want) {
		t.Fatalf("modes %v", got)
	}
	for id, mode := range want {
		if got[containerName(3, id)] != mode {
			t.Errorf("%s: %q, want %q", id, got[containerName(3, id)], mode)
		}
	}
	if vmSELinuxDefault("aio") != selinuxPermissive || vmSELinuxDefault("patroni") != selinuxEnforcing {
		t.Error("type defaults")
	}
	bad := runtimeIssues(designDoc{Nodes: []designNode{{ID: "ps-01", Type: "ps", SELinux: "disabled"}}}, "")
	if len(bad) != 1 || !strings.Contains(bad[0].Message, `selinux "disabled" is not enforcing or permissive`) {
		t.Errorf("an unknown mode was accepted: %+v", bad)
	}
	if s := limaSELinuxScript(selinuxPermissive); !strings.Contains(s, "SELINUX=permissive") || !strings.Contains(s, "setenforce 0") {
		t.Errorf("permissive script: %s", s)
	}
	if limaSELinuxScript("") != "" {
		t.Error("no mode must leave the OS's own")
	}
}

func TestKubeadmHasVM(t *testing.T) {
	mk := func(rt string) Stack {
		doc := designDoc{
			Frames: []designFrame{{ID: "k", Type: "kubeadm"}},
			Nodes: []designNode{
				{ID: "kube-1", Type: "kubeadm", FrameID: "k"},
				{ID: "kube-2", Type: "kubeadm", FrameID: "k", Runtime: rt},
			},
		}
		b, _ := json.Marshal(doc)
		return Stack{ID: 5, Design: b}
	}
	for rt, want := range map[string]bool{runtimeVM: true, "": false} {
		st := mk(rt)
		var doc designDoc
		json.Unmarshal(st.Design, &doc)
		if got := kubeadmHasVM(st, doc.Frames[0], doc); got != want {
			t.Errorf("member runtime %q: kubeadmHasVM %v, want %v", rt, got, want)
		}
	}
	if !strings.Contains(kubeadmCNIScript, `encapsulation: $ENCAP`) || !strings.Contains(kubeadmCNIScript, "--iface-can-reach=$REACH") {
		t.Error("the pod network script lost its stack-network settings")
	}
}

func TestVMDiskLimitsWarned(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "intranet-01", Type: "intranet"},
		{ID: "ps-01", Type: "ps", Runtime: runtimeVM, DeviceWriteMBps: 10},
		{ID: "ps-02", Type: "ps", DeviceWriteMBps: 10},
	}}
	var got []string
	for _, i := range runtimeIssues(doc, "") {
		got = append(got, i.Level+": "+i.Message)
	}
	if len(got) != 1 || !strings.Contains(got[0], "warning: ps-01: disk read/write limits apply to containers") {
		t.Fatalf("issues %v", got)
	}
}

// TestVMNeedsQEMU: a VM of the architecture Apple's Virtualization framework does not run, and
// every VM on Linux, needs QEMU; validation says so before a deploy when it is missing.
func TestVMNeedsQEMU(t *testing.T) {
	for _, c := range []struct {
		goos, host, arch string
		want             bool
	}{
		{"darwin", "arm64", "arm64", false},
		{"darwin", "arm64", "amd64", true},
		{"darwin", "amd64", "amd64", false},
		{"linux", "amd64", "amd64", true},
		{"linux", "amd64", "arm64", true},
		{"linux", "arm64", "arm64", true},
	} {
		if got := limaNeedsQEMU(c.goos, c.host, c.arch); got != c.want {
			t.Errorf("limaNeedsQEMU(%s, %s, %s) = %v, want %v", c.goos, c.host, c.arch, got, c.want)
		}
	}
	if limaQEMUBinary("amd64") != "qemu-system-x86_64" || limaQEMUBinary("arm64") != "qemu-system-aarch64" {
		t.Errorf("QEMU binaries: %s, %s", limaQEMUBinary("amd64"), limaQEMUBinary("arm64"))
	}

	msgs := func(is []issue) string {
		var b []string
		for _, i := range is {
			b = append(b, i.Level+": "+i.Message)
		}
		return strings.Join(b, "\n")
	}
	was := qemuLookPath
	qemuLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { qemuLookPath = was })
	other := map[string]string{"arm64": "amd64", "amd64": "arm64"}[runtime.GOARCH]
	doc := designDoc{
		Frames: []designFrame{{ID: "f1", Label: "pxc-cluster", Type: "pxc", Runtime: "vm", Arch: other}},
		Nodes: []designNode{
			{ID: "i", Type: "intranet"},
			{ID: "m1", Label: "pxc-1", Type: "pxc", FrameID: "f1"},
			{ID: "m2", Label: "pxc-2", Type: "pxc", FrameID: "f1"},
			{ID: "ps", Label: "ps-1", Type: "ps", Arch: other}, // a container: needs nothing
		},
	}
	got := msgs(runtimeIssues(doc, ""))
	if strings.Count(got, "qemu-system-"+limaArch(other)+" is not installed") != 1 || !strings.Contains(got, "pxc-1: an "+other+" VM") {
		t.Errorf("a VM on QEMU without QEMU installed, said once:\n%s", got)
	}
	if strings.Contains(got, "ps-1") {
		t.Errorf("a container node was flagged:\n%s", got)
	}
	if got := msgs(runtimeIssues(doc, "limactl is not installed")); strings.Contains(got, "qemu") {
		t.Errorf("QEMU flagged where Lima itself is missing:\n%s", got)
	}
}

// TestGDBClientNotAVM: a Linux Client set up for core-dump analysis is refused as a VM, since its
// core and libraries arrive through bind mounts; a plain Linux Client VM is fine.
func TestGDBClientNotAVM(t *testing.T) {
	doc := designDoc{Nodes: []designNode{
		{ID: "i", Type: "intranet"},
		{ID: "a", Label: "gdb-client", Type: "linuxclient", Runtime: "vm", GDBEnabled: true},
		{ID: "b", Label: "plain-client", Type: "linuxclient", Runtime: "vm"},
		{ID: "c", Label: "gdb-container", Type: "linuxclient", GDBEnabled: true},
	}}
	var got []string
	for _, i := range runtimeIssues(doc, "") {
		got = append(got, i.Level+": "+i.Message)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "error: gdb-client: a Linux Client for core-dump analysis runs in a container") {
		t.Errorf("an analysis client as a VM was not refused:\n%s", all)
	}
	if strings.Contains(all, "plain-client") || strings.Contains(all, "gdb-container") {
		t.Errorf("a plain client VM or an analysis container was flagged:\n%s", all)
	}
}

// TestVMNeedsALimaTemplate: a VM node on an OS with no Lima template (CentOS 7) is refused; a
// member's OS is its cluster's.
func TestVMNeedsALimaTemplate(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f", Label: "pxc-cluster", Type: "pxc", Runtime: "vm", OS: "oraclelinux", OSVersion: "9"}},
		Nodes: []designNode{
			{ID: "i", Type: "intranet"},
			{ID: "c7", Label: "client-el7", Type: "linuxclient", Runtime: "vm", OS: "centos", OSVersion: "7"},
			{ID: "u", Label: "client-noble", Type: "linuxclient", Runtime: "vm", OS: "ubuntu", OSVersion: "24.04"},
			{ID: "m", Label: "pxc-1", Type: "pxc", FrameID: "f", OS: "centos", OSVersion: "7"},
			{ID: "d", Label: "client-docker", Type: "linuxclient", OS: "centos", OSVersion: "7"},
		},
	}
	var got []string
	for _, i := range runtimeIssues(doc, "") {
		got = append(got, i.Message)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "client-el7: centos 7 cannot run as a VM") {
		t.Errorf("a CentOS 7 VM was not refused:\n%s", all)
	}
	for _, ok := range []string{"client-noble", "pxc-1", "client-docker"} {
		if strings.Contains(all, ok+":") {
			t.Errorf("%s was flagged:\n%s", ok, all)
		}
	}
}
