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
		// Asking for a debugger the operator has no build wired up for must not turn into a
		// deploy that tries anyway — k3dFrameIssues says so, and this stays off.
		{"not wired up for psmdb", designFrame{K3DOperator: "psmdb", K3DDebug: true}, false},
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
func TestK3DDebugPatchJSON(t *testing.T) {
	const dep = "percona-xtradb-cluster-operator"
	const img = "percona/percona-xtradb-cluster-operator:1.20.0"
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name     string `json:"name"`
						HostPath struct {
							Path string `json:"path"`
							Type string `json:"type"`
						} `json:"hostPath"`
					} `json:"volumes"`
					Containers []struct {
						Name          string                         `json:"name"`
						Image         string                         `json:"image"`
						Command       []string                       `json:"command"`
						Args          []string                       `json:"args"`
						LivenessProbe json.RawMessage                `json:"livenessProbe"`
						Env           []struct{ Name, Value string } `json:"env"`
						VolumeMounts  []struct {
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
	if err := json.Unmarshal([]byte(k3dDebugPatchJSON(dep, img)), &patch); err != nil {
		t.Fatalf("the patch is not valid JSON: %v", err)
	}
	ps := patch.Spec.Template.Spec
	if len(ps.Containers) != 2 || ps.Containers[0].Name != dep {
		t.Fatalf("the patch must target the operator container by name (strategic merge keys on it), plus the watchdog: %+v", ps.Containers)
	}
	c := ps.Containers[0]
	if len(c.Command) != 1 || !strings.HasSuffix(c.Command[0], "/dlv") {
		t.Errorf("command = %v, want the mounted dlv", c.Command)
	}
	if len(c.Args) < 2 || c.Args[0] != "exec" || c.Args[1] != k3dDebugPodMount+"/"+dep {
		t.Errorf("args = %v, want `exec <mount>/<operator>`", c.Args)
	}
	for _, flag := range []string{"--headless", "--accept-multiclient", "--only-same-user=false", "--continue"} {
		if !hasString(c.Args, flag) {
			t.Errorf("args %v are missing %s", c.Args, flag)
		}
	}
	// The probe hits the operator's own endpoint, which stops answering at a breakpoint; leader
	// election renews a lease every 10s, and a paused process cannot. Both would kill the session.
	if string(c.LivenessProbe) != "null" {
		t.Errorf("livenessProbe = %s, want null (a breakpoint stops it answering)", c.LivenessProbe)
	}
	leader := ""
	for _, e := range c.Env {
		if e.Name == "PXCO_LEADER_ELECTION_ENABLED" {
			leader = e.Value
		}
	}
	if leader != "false" {
		t.Errorf("PXCO_LEADER_ELECTION_ENABLED = %q, want \"false\"", leader)
	}
	if !hasString(c.SecurityContext.Capabilities.Add, "SYS_PTRACE") {
		t.Errorf("capabilities = %v, want SYS_PTRACE", c.SecurityContext.Capabilities.Add)
	}
	// The hostPath and the mount are the delivery mechanism; a mismatch leaves the pod stuck in
	// ContainerCreating, so they are asserted against the constants the drop actually uses.
	if len(ps.Volumes) != 1 || ps.Volumes[0].HostPath.Path != k3dDebugNodeDir {
		t.Errorf("hostPath = %+v, want %s", ps.Volumes, k3dDebugNodeDir)
	}
	if ps.Volumes[0].HostPath.Type != "Directory" {
		t.Errorf("hostPath type = %q, want Directory — an absent dir must fail loudly, not be created empty",
			ps.Volumes[0].HostPath.Type)
	}
	if w := ps.Containers[1]; w.Name != k3dDebugWatchdog || w.Image != img {
		t.Errorf("watchdog sidecar = %q on image %q, want %q on the operator's own image %q",
			w.Name, w.Image, k3dDebugWatchdog, img)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != k3dDebugPodMount {
		t.Errorf("volumeMounts = %+v, want %s", c.VolumeMounts, k3dDebugPodMount)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != k3dDebugPort {
		t.Errorf("ports = %+v, want containerPort %d", c.Ports, k3dDebugPort)
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
		"app.kubernetes.io/component: operator",
		"app.kubernetes.io/instance: " + dep,
		"app.kubernetes.io/name: " + dep,
		"nodePort: 30400",
		"targetPort: 40000",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("the delve Service is missing %q:\n%s", want, svc)
		}
	}
}

// The build directory is what the generated launch.json maps a local clone onto; an operator with
// no source repository has no such path rather than a wrong one.
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
