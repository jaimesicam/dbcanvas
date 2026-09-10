package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A pod list as kubectl actually renders it, trimmed to the fields the console reads:
// one database pod with a sidecar, one pod still running its init container, and a
// completed job pod. The shapes that matter are all here — an absent initContainers,
// an absent containerStatuses entry (the kubelet has not reported yet), and a
// container that is not running.
const podListJSON = `{
  "apiVersion": "v1", "kind": "List",
  "items": [
    {
      "metadata": {"name": "cluster1-haproxy-0", "namespace": "default"},
      "spec": {"containers": [{"name": "haproxy"}, {"name": "pxc-monit"}]},
      "status": {"phase": "Running", "containerStatuses": [
        {"name": "pxc-monit", "ready": true, "state": {"running": {"startedAt": "2026-09-10T09:00:00Z"}}},
        {"name": "haproxy", "ready": true, "state": {"running": {"startedAt": "2026-09-10T09:00:00Z"}}}
      ]}
    },
    {
      "metadata": {"name": "cluster1-pxc-0", "namespace": "default"},
      "spec": {
        "containers": [{"name": "pxc"}],
        "initContainers": [{"name": "pxc-init"}]
      },
      "status": {"phase": "Pending",
        "containerStatuses": [{"name": "pxc", "ready": false, "state": {"waiting": {"reason": "PodInitializing"}}}],
        "initContainerStatuses": [{"name": "pxc-init", "ready": false, "state": {"running": {}}}]}
    },
    {
      "metadata": {"name": "coredns-abc", "namespace": "kube-system"},
      "spec": {"containers": [{"name": "coredns"}]},
      "status": {"phase": "Running"}
    }
  ]
}`

func TestParseK3DPods(t *testing.T) {
	pods, err := parseK3DPods([]byte(podListJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pods) != 3 {
		t.Fatalf("expected 3 pods, got %d", len(pods))
	}
	// Sorted by namespace then name, so the menu is stable between two opens of it.
	got := []string{}
	for _, p := range pods {
		got = append(got, p.Namespace+"/"+p.Name)
	}
	want := "default/cluster1-haproxy-0 default/cluster1-pxc-0 kube-system/coredns-abc"
	if strings.Join(got, " ") != want {
		t.Errorf("order:\n got %s\nwant %s", strings.Join(got, " "), want)
	}

	// Containers keep the pod's own declaration order — NOT the order the kubelet
	// happens to report their statuses in, which is what the fixture inverts.
	hap := pods[0]
	if hap.Containers[0].Name != "haproxy" || hap.Containers[1].Name != "pxc-monit" {
		t.Errorf("container order came from containerStatuses: %+v", hap.Containers)
	}
	if hap.Containers[0].State != "running" || !hap.Containers[0].Ready {
		t.Errorf("haproxy should be running and ready: %+v", hap.Containers[0])
	}

	// An init container is listed after the regular ones and tagged, because a pod
	// stuck in Init is exactly when somebody wants a shell in one.
	pxc := pods[1]
	if len(pxc.Containers) != 2 || !pxc.Containers[1].Init || pxc.Containers[1].Name != "pxc-init" {
		t.Fatalf("init container missing or out of place: %+v", pxc.Containers)
	}
	if pxc.Containers[0].State != "waiting" {
		t.Errorf("pxc should be waiting, got %q", pxc.Containers[0].State)
	}

	// No containerStatuses at all: the container is still offered, with no state, so
	// the menu can say "not running" rather than pretend the pod has no containers.
	dns := pods[2]
	if len(dns.Containers) != 1 || dns.Containers[0].State != "" || dns.Containers[0].Ready {
		t.Errorf("a status-less container should be listed with no state: %+v", dns.Containers)
	}
}

func TestParseK3DPodsRejectsNonJSON(t *testing.T) {
	if _, err := parseK3DPods([]byte("error: the server doesn't have a resource type \"pods\"")); err == nil {
		t.Error("kubectl's error output must not parse as an empty pod list")
	}
}

func TestK3DPodExecCmd(t *testing.T) {
	cmd := k3dPodExecCmd("default", "cluster1-pxc-0", "pxc", "auto")
	joined := strings.Join(cmd, " ")
	for _, want := range []string{"kubectl -n default exec -i -t cluster1-pxc-0 -c pxc --", "command -v bash"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	// The names travel as separate argv elements, never interpolated into the script
	// the container's shell runs — the script is the last element and mentions neither.
	if strings.Contains(cmd[len(cmd)-1], "cluster1-pxc-0") {
		t.Error("the pod name reached the shell script")
	}
	if s := k3dPodExecCmd("d", "p", "c", "bash")[len(cmd)-1]; s != "exec bash -i" {
		t.Errorf("bash script = %q", s)
	}
	if s := k3dPodExecCmd("d", "p", "c", "sh")[len(cmd)-1]; s != "exec /bin/sh -i" {
		t.Errorf("sh script = %q", s)
	}
}

func TestK3DPodConsoleRequest(t *testing.T) {
	// No pod named: a plain node console, and not an error.
	r := httptest.NewRequest("GET", "/api/stacks/1/nodes/k3s-01/term", nil)
	if _, _, _, _, ok, err := k3dPodConsoleRequest(r); ok || err != nil {
		t.Errorf("a plain node console should be ok=false err=nil, got ok=%v err=%v", ok, err)
	}

	r = httptest.NewRequest("GET", "/api/stacks/1/nodes/k3s-01/term?namespace=default&pod=cluster1-pxc-0&container=pxc", nil)
	ns, pod, container, shell, ok, err := k3dPodConsoleRequest(r)
	if !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ns != "default" || pod != "cluster1-pxc-0" || container != "pxc" || shell != "auto" {
		t.Errorf("got %q %q %q %q", ns, pod, container, shell)
	}

	// Anything that is not a Kubernetes name, and any shell that is not one of the
	// three, is refused before the socket is upgraded.
	for _, bad := range []string{
		"pod=x&namespace=de%20fault&container=c",
		"pod=Cluster1-PXC-0&namespace=default&container=c",
		"pod=x%20%26%20rm&namespace=default&container=c",
		"pod=x&namespace=default&container=../etc",
		"pod=x&namespace=default&container=c&shell=zsh",
		"pod=x&namespace=default",
	} {
		r = httptest.NewRequest("GET", "/api/stacks/1/nodes/k3s-01/term?"+bad, nil)
		if _, _, _, _, ok, err := k3dPodConsoleRequest(r); ok || err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The database clients are the fourth part of a pod console when the container is a
// database's. Two things are asserted about every one of them, and they are the two
// that would make a terminal useless: that nothing is exec'd without being probed for
// first, and that a script which cannot start its client opens a shell instead of
// exiting — an exit closes the window and takes the explanation with it.
func TestK3DPodClientScripts(t *testing.T) {
	for _, c := range k3dPodClients {
		if !strings.Contains(c.Script, "command -v "+c.ID) {
			t.Errorf("%s: exec without a `command -v` probe first", c.ID)
		}
		if !strings.Contains(c.Script, "exec bash -i") {
			t.Errorf("%s: a script that cannot start its client must fall back to a shell", c.ID)
		}
		if strings.Contains(c.Script, "exit 1") {
			t.Errorf("%s: exiting closes the terminal and hides the reason", c.ID)
		}
	}

	// The credential is read INSIDE the container, from the mount the operator made.
	// Nothing here may look like a password handed down from the app — that is the
	// whole reason this is worth doing rather than merely possible.
	mysql := k3dPodConsoleScript("mysql")
	for _, want := range []string{"/etc/mysql/mysql-users-secret/root", "MYSQL_ROOT_PASSWORD", "export MYSQL_PWD"} {
		if !strings.Contains(mysql, want) {
			t.Errorf("mysql script is missing %q", want)
		}
	}
	// MYSQL_PWD does not survive into the fallback shell.
	if !strings.Contains(mysql, "unset MYSQL_PWD") {
		t.Error("mysql script leaks MYSQL_PWD into the shell it falls back to")
	}
	if !strings.Contains(k3dPodConsoleScript("mongosh"), "/etc/users-secret/MONGODB_DATABASE_ADMIN_USER") {
		t.Error("mongosh script does not read the operator's users secret mount")
	}
	// psql needs no credential at all: the container runs as postgres, and peer
	// authentication is the login. A script that went looking for a password would be
	// asserting something untrue about the operator's pod.
	if psql := k3dPodConsoleScript("psql"); !strings.Contains(psql, "psql -U postgres") || strings.Contains(psql, "PGPASSWORD") {
		t.Errorf("psql script = %s", psql)
	}

	// An unknown target is a shell, which is what every non-database container gets.
	if k3dPodConsoleScript("auto") != k3dPodShellScript("auto") {
		t.Error("auto stopped being the shell script")
	}
}

func TestK3DPodClientsFor(t *testing.T) {
	for _, tc := range []struct{ container, want string }{
		{"pxc", "mysql"},      // PXC
		{"mysql", "mysql"},    // PS
		{"haproxy", "mysql"},  // the proxy carries the client and the same secret
		{"mongod", "mongosh"}, // PSMDB
		{"database", "psql"},  // Percona PG
		{"postgres", "psql"},  // CloudNativePG
		{"pxc-init", ""},      // an init container is a shell and nothing else
		{"coredns", ""},       // and so is anything that is not a database
	} {
		if got := strings.Join(k3dPodClientsFor(tc.container), ","); got != tc.want {
			t.Errorf("%s: clients = %q, want %q", tc.container, got, tc.want)
		}
	}

	// The inventory carries the hint, so the menu does not have to know the operators'
	// container names itself.
	pods, err := parseK3DPods([]byte(podListJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(pods[0].Containers[0].Clients, ","); got != "mysql" {
		t.Errorf("haproxy container clients = %q", got)
	}
	if pods[2].Containers[0].Clients != nil {
		t.Errorf("coredns should offer no clients, got %v", pods[2].Containers[0].Clients)
	}
}

func TestK3DPodConsoleRequestAcceptsClients(t *testing.T) {
	for _, target := range []string{"mysql", "psql", "mongosh"} {
		r := httptest.NewRequest("GET", "/api/stacks/1/nodes/k3s-01/term?namespace=default&pod=cluster1-pxc-0&container=pxc&shell="+target, nil)
		_, _, _, shell, ok, err := k3dPodConsoleRequest(r)
		if !ok || err != nil || shell != target {
			t.Errorf("%s: ok=%v err=%v shell=%q", target, ok, err, shell)
		}
	}
	// The error still names everything that is accepted, or it sends the caller to the
	// source to find out.
	r := httptest.NewRequest("GET", "/api/stacks/1/nodes/k3s-01/term?namespace=default&pod=p&container=c&shell=mongo", nil)
	_, _, _, _, _, err := k3dPodConsoleRequest(r)
	if err == nil || !strings.Contains(err.Error(), "mongosh") {
		t.Errorf("err = %v", err)
	}
}
