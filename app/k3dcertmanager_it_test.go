package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestInstallCertManagerOnRealCluster runs the real thing: it creates a throwaway k3d cluster,
// installs cert-manager into it the way a ticked frame does, and checks the two claims the
// feature makes — that the release lands, and that it is *serving* before the function returns,
// which is the whole reason it runs before the operator.
//
// Opt-in, because it creates a Kubernetes cluster and pulls ~500 MB:
//
//	K3D_IT=1 go test -run InstallCertManager -timeout 20m ./app
//
// The cluster is deleted at the end, including after a failure.
func TestInstallCertManagerOnRealCluster(t *testing.T) {
	if os.Getenv("K3D_IT") == "" {
		t.Skip("integration test; set K3D_IT=1 to create a throwaway k3d cluster")
	}
	bin, err := k3dBinary()
	if err != nil {
		t.Skipf("no k3d binary: %v", err)
	}
	const cluster = "dbcanvas-certmgr-it"
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()

	// k3d reads DOCKER_SOCK itself and bind-mounts what it names into the nodes, which fails on
	// a Docker Desktop / Rancher Desktop socket. It is set here only to tell the App under test
	// where the daemon is, so it is stripped from k3d's own environment.
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOCKER_SOCK=") {
			env = append(env, kv)
		}
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	_, _ = run("cluster", "delete", cluster) // a leftover from a failed run
	if out, err := run("cluster", "create", cluster, "--agents", "0", "--wait"); err != nil {
		t.Fatalf("create the cluster: %v\n%s", err, lastLines(out, 400))
	}
	defer func() {
		if out, err := run("cluster", "delete", cluster); err != nil {
			t.Errorf("the throwaway cluster was NOT deleted: %v\n%s", err, out)
		}
	}()

	a := &App{docker: NewDocker(envOr("DOCKER_SOCK", "/var/run/docker.sock"))}
	serverID, ok, err := a.docker.ContainerByName(ctx, k3dNodeContainer(cluster, 0))
	if err != nil || !ok {
		t.Fatalf("find the server node container: ok=%v err=%v", ok, err)
	}

	// Before: no cert-manager, and the probe proves it — a Certificate cannot even be dry-run
	// against a cluster that has no such kind. This is what the wait is protecting against.
	if a.certManagerPresent(ctx, serverID) {
		t.Fatal("a fresh k3d cluster should not have cert-manager on it")
	}
	if err := a.waitCertManagerWebhook(withTimeout(ctx, 10*time.Second), serverID); err == nil {
		t.Error("the webhook probe passed on a cluster with no cert-manager — it is not a check")
	}

	var log []string
	ver, err := a.installCertManager(ctx, serverID, func(s string) { log = append(log, s); t.Log(s) })
	if err != nil {
		t.Fatalf("installCertManager: %v", err)
	}
	if ver != certManagerVersion {
		t.Errorf("installed %q, expected the pinned %q", ver, certManagerVersion)
	}
	if !a.certManagerPresent(ctx, serverID) {
		t.Error("cert-manager reports absent right after installing it")
	}
	// The version on the cluster is the version that was pinned — a manifest that quietly
	// redirects to something else is exactly what pinning is for.
	out, err := a.kubectl(ctx, serverID, "-n", certManagerNamespace, "get", "deploy", "cert-manager",
		"-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		t.Fatalf("read the controller image: %v", err)
	}
	if !strings.Contains(out, certManagerVersion) {
		t.Errorf("the controller runs %q, expected %s", out, certManagerVersion)
	}
	// And the claim that matters for the ordering: a Certificate is admitted, so an operator
	// starting now would find a cert-manager that works.
	if err := a.waitCertManagerWebhook(ctx, serverID); err != nil {
		t.Errorf("the webhook is not admitting Certificates after install: %v", err)
	}
	if len(log) == 0 {
		t.Error("the install said nothing to the deployment log")
	}
}

func withTimeout(ctx context.Context, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(ctx, d)
	_ = cancel // the caller's ctx bounds it; this is a test helper
	return c
}
