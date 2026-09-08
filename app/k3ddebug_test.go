package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestK3DDebugOn(t *testing.T) {
	cases := []struct {
		name string
		f    designFrame
		want bool
	}{
		{"off by default", designFrame{K3DOperator: "pxc"}, false},
		{"on for pxc", designFrame{K3DOperator: "pxc", K3DDebug: true}, true},
		{"on for ps", designFrame{K3DOperator: "ps", K3DDebug: true}, true},
		{"on for psmdb", designFrame{K3DOperator: "psmdb", K3DDebug: true}, true},
		{"on for pg", designFrame{K3DOperator: "pg", K3DDebug: true}, true},
		// Asking for a debugger the operator has no build wired up for must not turn into a
		// deploy that tries anyway — k3dFrameIssues says so, and this stays off. The two
		// community PostgreSQL operators come from a Helm chart: no tarball, no source, no build.
		{"not wired up for cnpg", designFrame{K3DOperator: "cnpg", K3DDebug: true}, false},
		{"not wired up for pgo", designFrame{K3DOperator: "pgo", K3DDebug: true}, false},
		{"no operator at all", designFrame{K3DDebug: true}, false},
	}
	for _, c := range cases {
		if got := k3dDebugOn(c.f); got != c.want {
			t.Errorf("%s: k3dDebugOn = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestK3DDebugHostPort(t *testing.T) {
	if p := k3dDebugHostPort(designFrame{}); p != k3dDebugHostDflt {
		t.Errorf("unset port = %d, want the default %d", p, k3dDebugHostDflt)
	}
	if p := k3dDebugHostPort(designFrame{K3DDebugPort: 41234}); p != 41234 {
		t.Errorf("port = %d, want 41234", p)
	}
	if p := k3dDebugHostPort(designFrame{K3DDebugPort: 70000}); p != k3dDebugHostDflt {
		t.Errorf("out-of-range port = %d, want the default %d", p, k3dDebugHostDflt)
	}
}

// The create flag is the whole reason this is a deploy-time option: k3d fixes a cluster's port
// mappings when it creates it. The mapping is host → the server node's NodePort, not host →
// Delve's own port, which the pod does not expose to the node.
func TestK3DDebugCreateArgs(t *testing.T) {
	t.Setenv("CONTAINER_BIND_IP", "127.0.0.1")
	args := k3dDebugCreateArgs(designFrame{K3DOperator: "pxc", K3DDebug: true, K3DDebugPort: 40000})
	want := []string{"--port", "127.0.0.1:40000:30400/tcp@server:0"}
	if len(args) != len(want) || args[0] != want[0] || args[1] != want[1] {
		t.Fatalf("create args = %v, want %v", args, want)
	}
	if k3dDebugNodePort < 30000 || k3dDebugNodePort > 32767 {
		t.Errorf("nodePort %d is outside Kubernetes' service-node-port range", k3dDebugNodePort)
	}
}

// The published port must not land on the LAN by default: a Delve listener is remote code
// execution by design.
func TestK3DDebugCreateArgsBindIP(t *testing.T) {
	t.Setenv("CONTAINER_BIND_IP", "")
	args := k3dDebugCreateArgs(designFrame{})
	if !strings.HasPrefix(args[1], "127.0.0.1:") {
		t.Errorf("with no CONTAINER_BIND_IP the mapping is %q, want it bound to loopback", args[1])
	}
}

// A GitHub source tarball carries more than one go.mod, and the one that sorts first is a linter
// stub whose go directive means nothing. Matching on the suffix alone (what tarFile does) finds
// that one, so the root module has to be addressed by its full path.
func TestK3DDebugGoVersionPicksTheRootModule(t *testing.T) {
	const dir = "percona-xtradb-cluster-operator-1.20.0"
	tarball := testTarball(t, map[string]string{
		dir + "/.github/linters/go.mod": "module linters\n\ngo 1.19\n",
		dir + "/go.mod":                 "module github.com/percona/percona-xtradb-cluster-operator\n\ngo 1.26.0\n\nrequire (\n)\n",
	})
	got, err := k3dDebugGoVersion(tarball, dir)
	if err != nil {
		t.Fatalf("k3dDebugGoVersion: %v", err)
	}
	if got != "1.26" {
		t.Errorf("go tag = %q, want %q (the root module's directive, not the linter stub's)", got, "1.26")
	}
}

func TestK3DDebugGoVersionMissing(t *testing.T) {
	tarball := testTarball(t, map[string]string{"src/README.md": "hello"})
	if _, err := k3dDebugGoVersion(tarball, "src"); err == nil {
		t.Fatal("expected an error when the source carries no go.mod")
	}
}

// The builder cross-compiles to the *cluster's* architecture, which is not necessarily the
// daemon's — a K3D frame targets linux/amd64 by default even on an arm64 host.
func TestK3DDebugGOARCH(t *testing.T) {
	t.Setenv("K3D_PLATFORM", "linux/arm64")
	if got := k3dDebugGOARCH(); got != "arm64" {
		t.Errorf("GOARCH = %q, want arm64", got)
	}
	t.Setenv("K3D_PLATFORM", "linux/amd64")
	if got := k3dDebugGOARCH(); got != "amd64" {
		t.Errorf("GOARCH = %q, want amd64", got)
	}
}

// The patch is what actually swaps the operator for a debugger, and each piece of it is load
// bearing — the flags especially: without --only-same-user=false Delve accepts the connection only
// from a loopback peer, which a NodePort never is.
// The patch is the whole install, so it is asserted for every operator the debugger claims to
// support — the three things that differ between them (the container's name, the main package's
// binary, and how leader election is turned off) are exactly the three that fail silently.
func TestK3DDebugPatchJSON(t *testing.T) {
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					ShareProcessNamespace bool `json:"shareProcessNamespace"`
					Volumes               []struct {
						Name     string `json:"name"`
						HostPath struct {
							Path string `json:"path"`
							Type string `json:"type"`
						} `json:"hostPath"`
					} `json:"volumes"`
					Containers []struct {
						Name           string                         `json:"name"`
						Image          string                         `json:"image"`
						Command        []string                       `json:"command"`
						Args           []string                       `json:"args"`
						LivenessProbe  json.RawMessage                `json:"livenessProbe"`
						ReadinessProbe json.RawMessage                `json:"readinessProbe"`
						Env            []struct{ Name, Value string } `json:"env"`
						VolumeMounts   []struct {
							MountPath string `json:"mountPath"`
							ReadOnly  bool   `json:"readOnly"`
						} `json:"volumeMounts"`
						Ports []struct {
							Name          string `json:"name"`
							ContainerPort int    `json:"containerPort"`
						} `json:"ports"`
						SecurityContext struct {
							Capabilities struct{ Add []string } `json:"capabilities"`
						} `json:"securityContext"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}

	for op, prof := range k3dDebugProfiles {
		dep := k3dOperatorRepos[op]
		if dep == "" {
			t.Fatalf("%s has a debug profile but no source repository", op)
		}
		img := "percona/" + dep + ":1.2.3"
		// The leader-elect flag is decided per release (k3dDebugLeaderElectFlag), so the patch is
		// asserted with it present — the shape that has to be right is the `--` separator.
		opArgs := []string{"--leader-elect=false"}
		if err := json.Unmarshal([]byte(k3dDebugPatchJSON(dep, img, prof, opArgs)), &patch); err != nil {
			t.Fatalf("%s: the patch is not valid JSON: %v", op, err)
		}
		ps := patch.Spec.Template.Spec
		// shareProcessNamespace is what lets the watchdog see the operator's /proc entry at all.
		if !ps.ShareProcessNamespace {
			t.Errorf("%s: shareProcessNamespace is off — the watchdog cannot see a halted operator", op)
		}
		if len(ps.Containers) != 2 || ps.Containers[0].Name != prof.Container {
			t.Fatalf("%s: the patch must target container %q by name (strategic merge keys on it), plus the watchdog: %+v",
				op, prof.Container, ps.Containers)
		}
		c := ps.Containers[0]
		if len(c.Command) != 1 || !strings.HasSuffix(c.Command[0], "/dlv") {
			t.Errorf("%s: command = %v, want the mounted dlv", op, c.Command)
		}
		// The binary is named for the repository whatever main package it was built from, so
		// this is the Deployment's name even where the container's is not.
		if len(c.Args) < 2 || c.Args[0] != "exec" || c.Args[1] != k3dDebugPodMount+"/"+dep {
			t.Errorf("%s: args = %v, want `exec <mount>/%s`", op, c.Args, dep)
		}
		for _, flag := range []string{"--headless", "--accept-multiclient", "--only-same-user=false", "--continue"} {
			if !hasString(c.Args, flag) {
				t.Errorf("%s: args %v are missing %s", op, c.Args, flag)
			}
		}
		// The probes hit the operator's own endpoints, which stop answering at a breakpoint:
		// liveness kills the pod, readiness drops it out of the Delve Service's endpoints.
		if string(c.LivenessProbe) != "null" {
			t.Errorf("%s: livenessProbe = %s, want null (a breakpoint stops it answering)", op, c.LivenessProbe)
		}
		if string(c.ReadinessProbe) != "null" {
			t.Errorf("%s: readinessProbe = %s, want null (an unready pod leaves the Service with no endpoints)",
				op, c.ReadinessProbe)
		}
		// Leader election renews a lease every few seconds and the manager exits when it cannot;
		// a process sitting at a breakpoint cannot. Each operator switches it off its own way,
		// and one of the two ways has to be present.
		for _, e := range c.Env {
			if strings.Contains(e.Name, "LEADER_ELECTION") && e.Value != "false" {
				t.Errorf("%s: %s = %q, want \"false\"", op, e.Name, e.Value)
			}
		}
		// Anything meant for the operator has to land after dlv's own flags, behind the `--`
		// separator, or dlv reads them as its own and refuses to start.
		sep := -1
		for i, a := range c.Args {
			if a == "--" {
				sep = i
			}
		}
		if sep < 0 {
			t.Errorf("%s: args %v carry operator flags with no `--` separator", op, c.Args)
		} else if !hasString(c.Args[sep+1:], "--leader-elect=false") {
			t.Errorf("%s: args %v are missing --leader-elect=false after `--`", op, c.Args)
		}
		if !hasString(c.SecurityContext.Capabilities.Add, "SYS_PTRACE") {
			t.Errorf("%s: capabilities = %v, want SYS_PTRACE", op, c.SecurityContext.Capabilities.Add)
		}
		// The hostPath and the mount are the delivery mechanism; a mismatch leaves the pod stuck
		// in ContainerCreating, so they are asserted against the constants the drop actually uses.
		if len(ps.Volumes) != 1 || ps.Volumes[0].HostPath.Path != k3dDebugNodeDir {
			t.Errorf("%s: hostPath = %+v, want %s", op, ps.Volumes, k3dDebugNodeDir)
		}
		if ps.Volumes[0].HostPath.Type != "Directory" {
			t.Errorf("%s: hostPath type = %q, want Directory — an absent dir must fail loudly, not be created empty",
				op, ps.Volumes[0].HostPath.Type)
		}
		if w := ps.Containers[1]; w.Name != k3dDebugWatchdog || w.Image != img {
			t.Errorf("%s: watchdog sidecar = %q on image %q, want %q on the operator's own image %q",
				op, w.Name, w.Image, k3dDebugWatchdog, img)
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != k3dDebugPodMount {
			t.Errorf("%s: volumeMounts = %+v, want %s", op, c.VolumeMounts, k3dDebugPodMount)
		}
		if len(c.Ports) != 1 || c.Ports[0].ContainerPort != k3dDebugPort {
			t.Errorf("%s: ports = %+v, want containerPort %d", op, c.Ports, k3dDebugPort)
		}
	}
}

// With nothing to hand the operator, dlv gets no `--` at all: an empty separator is not harmless,
// it makes dlv treat the rest of its own line as the debuggee's.
func TestK3DDebugPatchJSONWithoutOperatorArgs(t *testing.T) {
	patch := k3dDebugPatchJSON("percona-xtradb-cluster-operator", "img:1",
		k3dDebugProfiles["pxc"], nil)
	if strings.Contains(patch, `"--"`) {
		t.Errorf("a patch with no operator args must not carry a `--` separator:\n%s", patch)
	}
}

// Whether the manager takes `--leader-elect` is a property of the release, and getting it wrong
// is silent both ways: pass the flag to a release that dropped it and the binary refuses to
// start; leave it out of one that has it and the operator elects, then exits the first time a
// breakpoint stops it renewing. So the answer is read out of the tarball rather than tabulated.
func TestK3DDebugLeaderElectFlag(t *testing.T) {
	tarWith := func(dir string, files map[string]string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: dir + "/" + name, Mode: 0o644,
				Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		return buf.Bytes()
	}
	const flagged = `flag.BoolVar(&enableLeaderElection, "leader-elect", true, "enable it")`
	const envOnly = "LeaderElection bool `default:\"true\" envconfig:\"PXCO_LEADER_ELECTION_ENABLED\"`"

	cases := []struct {
		name  string
		files map[string]string
		pkg   string
		want  bool
	}{
		{"the flag is registered", map[string]string{"cmd/manager/main.go": flagged}, "./cmd/manager", true},
		{"envconfig only", map[string]string{"cmd/manager/main.go": envOnly}, "./cmd/manager", false},
		// The PostgreSQL operator's main package is not cmd/manager, and a hit outside the
		// package that is actually built must not count.
		{"another package's flag does not count",
			map[string]string{"cmd/manager/main.go": flagged, "cmd/postgres-operator/main.go": envOnly},
			"./cmd/postgres-operator", false},
		{"the main package is elsewhere",
			map[string]string{"cmd/postgres-operator/main.go": flagged},
			"./cmd/postgres-operator", true},
		// Tests mention flags they do not register.
		{"a test file does not count",
			map[string]string{"cmd/manager/main.go": envOnly, "cmd/manager/main_test.go": flagged},
			"./cmd/manager", false},
		{"no such package", map[string]string{"cmd/manager/main.go": flagged}, "./cmd/nope", false},
	}
	for _, c := range cases {
		const dir = "percona-xtradb-cluster-operator-1.19.0"
		if got := k3dDebugLeaderElectFlag(tarWith(dir, c.files), dir, c.pkg); got != c.want {
			t.Errorf("%s: leader-elect flag = %v, want %v", c.name, got, c.want)
		}
	}
}

// A profile that names a container or a main package the release does not have produces a
// debugger that looks installed and is not, so the shape of every entry is pinned here and the
// values themselves are checked against a live cluster of each operator.
func TestK3DDebugProfilesAreComplete(t *testing.T) {
	for op, prof := range k3dDebugProfiles {
		if k3dOperatorRepos[op] == "" {
			t.Errorf("%s: no source repository, so there is nothing to build", op)
		}
		if prof.Container == "" {
			t.Errorf("%s: no container name — the strategic-merge patch keys on it", op)
		}
		if !strings.HasPrefix(prof.Pkg, "./cmd/") {
			t.Errorf("%s: main package %q, want a ./cmd/... path", op, prof.Pkg)
		}
		for _, kv := range prof.Env {
			if kv[0] == "" || kv[1] == "" {
				t.Errorf("%s: env entry %v is incomplete", op, kv)
			}
		}
		if !k3dDebuggableOperator(op) {
			t.Errorf("%s: has a profile but is not reported as debuggable", op)
		}
	}
	for _, op := range []string{"cnpg", "pgo", "", "nope"} {
		if k3dDebuggableOperator(op) {
			t.Errorf("%q must not be debuggable — it has no release tarball to compile", op)
		}
	}
}

// The Service is the other half of the published port. Its selector has to match the labels
// bundle.yaml puts on the operator pod, and its nodePort has to be the one k3d mapped.
func TestK3DDebugService(t *testing.T) {
	const dep = "percona-xtradb-cluster-operator"
	svc := string(k3dDebugService(dep))
	for _, want := range []string{
		"name: " + dep + "-delve",
		"type: NodePort",
		"app.kubernetes.io/name: " + dep,
		"publishNotReadyAddresses: true",
		"nodePort: 30400",
		"targetPort: 40000",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("the delve Service is missing %q:\n%s", want, svc)
		}
	}
	// The selector must be the one label all four operators share. component/instance are on
	// three of them and absent from percona-server-mysql-operator, whose pod carries
	// app.kubernetes.io/name alone — selecting on them leaves that Service with no endpoints.
	for _, unwanted := range []string{"app.kubernetes.io/component", "app.kubernetes.io/instance"} {
		if strings.Contains(svc, unwanted) {
			t.Errorf("the delve Service selects on %q, which not every operator sets:\n%s", unwanted, svc)
		}
	}
}

// The build directory is what the generated launch.json maps a local clone onto; an operator with
// no source repository has no such path rather than a wrong one. It is a forward-slash Linux path
// even when the clone is on Windows: Delve rewrites the separators in the part after the prefix
// (locspec.joinPath picks the separator from the "to" side), which is what lets a
// c:\Users\...\controller.go breakpoint resolve against a binary built in /go/src.
func TestK3DDebugBuildDir(t *testing.T) {
	if got := k3dDebugBuildDir("pxc"); got != "/go/src/github.com/percona/percona-xtradb-cluster-operator" {
		t.Errorf("build dir = %q", got)
	}
	if got := k3dDebugBuildDir("cnpg"); got != "" {
		t.Errorf("build dir for a chart-installed operator = %q, want empty", got)
	}
}

func hasString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// testTarball builds an uncompressed tar of the given path → content, the shape k3dFetchOperator
// hands around after gunzipping a GitHub source tarball.
func testTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The watchdog is what makes the debugger safe to walk away from, and every part of it is a
// literal string that has to be right: the port it scans for is hex in /proc/net, the socket state
// it counts as "attached" is 01 (ESTABLISHED, not the 08/CLOSE_WAIT a closed client leaves
// behind), and the heal answers Delve's exit prompt with `n` — `y` there kills the headless
// server, which takes the operator's container down with it.
func TestK3DDebugWatchdogScript(t *testing.T) {
	sh := k3dDebugWatchdogSh("percona-xtradb-cluster-operator")
	for _, want := range []string{
		`:9C40$`,     // Delve's port as /proc/net/tcp6 spells it
		`$4 == "01"`, // ESTABLISHED only
		"/proc/net/tcp6",
		"clearall", // leftovers are cleared, not kept: an IDE re-sends its breakpoints
		"exit -c",  // ...and the process is resumed on the way out
		k3dDebugPodMount + "/dlv",
		"127.0.0.1:40000",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("the watchdog script is missing %q:\n%s", want, sh)
		}
	}
	if strings.Contains(sh, `exit -c\ny\n`) {
		t.Error("the heal answers Delve's kill-the-headless-instance prompt with y — that stops the operator")
	}
	// It must do nothing until somebody has actually attached: a cluster nobody debugs should
	// never see a connection from this.
	// It must act on the condition, not on having witnessed the session: a session that starts
	// and ends between two ticks is invisible, and an operator frozen by one would stay frozen.
	for _, want := range []string{`"$st" = "t"`, "/proc/[0-9]*", "halted || continue", "attached && continue"} {
		if !strings.Contains(sh, want) {
			t.Errorf("the watchdog does not test halted-and-unattached directly (missing %q)", want)
		}
	}
	if !strings.Contains(sh, "sleep 10") {
		t.Errorf("watchdog tick is not %ds", k3dDebugWatchdogTick)
	}
}

// The port appears three ways — decimal in the listener flag, decimal in the dial, hex in the
// socket scan — and a mismatch is invisible until an operator silently stays frozen.
func TestK3DDebugPortHexMatchesListener(t *testing.T) {
	if got := fmt.Sprintf("%04X", k3dDebugPort); got != "9C40" {
		t.Errorf("port hex = %s, want 9C40 for port %d", got, k3dDebugPort)
	}
}

// A frame that only wants the in-app debugger publishes no host port: the port is fixed, so
// two clusters debugged at once would collide on it, and DBCanvas reaches Delve over the
// stack network anyway.
func TestK3DDebugNoPublish(t *testing.T) {
	f := designFrame{K3DDebug: true, K3DOperator: "pxc", K3DDebugNoPublish: true}
	if k3dDebugPublishes(f) {
		t.Fatal("a frame that opted out should not publish")
	}
	if args := k3dDebugCreateArgs(f); len(args) != 0 {
		t.Fatalf("create args = %v, want none", args)
	}
	// The debugger itself is unaffected — opting out of the host port is not opting out of
	// debugging, and the NodePort in front of the pod is what the app dials either way.
	if !k3dDebugOn(f) {
		t.Fatal("the frame should still deploy its operator under Delve")
	}

	// And the default — every design saved before this option existed — still publishes.
	old := designFrame{K3DDebug: true, K3DOperator: "pxc"}
	if !k3dDebugPublishes(old) {
		t.Fatal("a design from before the option must keep publishing")
	}
	if args := k3dDebugCreateArgs(old); len(args) != 2 {
		t.Fatalf("create args = %v, want the --port pair", args)
	}
}
