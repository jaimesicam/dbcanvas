package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVagrantE2E drives the real vagrant/VirtualBox toolchain: it boots one VM,
// execs into it, copies a file, checks IP/port bookkeeping, then destroys it. It is
// skipped unless DBCANVAS_VAGRANT_E2E=1 because it downloads a box and boots a VM
// (minutes). Override the box via DBCANVAS_E2E_IMAGE (a dbcanvas-systemd tag).
func TestVagrantE2E(t *testing.T) {
	if os.Getenv("DBCANVAS_VAGRANT_E2E") == "" {
		t.Skip("set DBCANVAS_VAGRANT_E2E=1 to run the real VirtualBox e2e")
	}
	v := NewVagrant()
	if v == nil {
		t.Fatal("NewVagrant returned nil (vagrant/VBoxManage/ssh missing)")
	}
	// Isolate state under a temp root so this never touches a real fleet.
	v.root = t.TempDir()
	os.MkdirAll(filepath.Join(v.root, "vms"), 0o755)

	ctx := context.Background()
	image := os.Getenv("DBCANVAS_E2E_IMAGE")
	if image == "" {
		image = "dbcanvas-systemd:ubuntu-22.04-amd64" // -> ubuntu/jammy64
	}
	const net = "dbcanvas-e2e"
	if err := v.NetworkEnsure(ctx, net); err != nil {
		t.Fatalf("NetworkEnsure: %v", err)
	}

	spec := ContainerSpec{
		Name:       "dbcanvas-e2e-node",
		Image:      image,
		Hostname:   "e2e",
		Network:    net,
		PublishMap: []PortMap{{ContainerPort: 5432, HostPort: 0}},
	}
	t.Logf("creating VM from %s (box download + boot may take minutes)…", image)
	id, err := v.ContainerCreate(ctx, spec)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() { v.ContainerRemove(context.Background(), id) })

	if err := v.WaitSystemd(ctx, id, 3*time.Minute); err != nil {
		t.Fatalf("WaitSystemd: %v", err)
	}

	// Exec runs as root (sudo) by default, like Docker's default exec user.
	if res, err := v.Exec(ctx, id, []string{"id", "-un"}, nil); err != nil || strings.TrimSpace(res.Stdout) != "root" {
		t.Fatalf("Exec id -un = %q (code %d, err %v), want root", res.Stdout, res.Code, err)
	}
	// ExecAs a named user maps to sudo -u.
	if res, err := v.ExecAs(ctx, id, "vagrant", []string{"id", "-un"}, nil); err != nil || strings.TrimSpace(res.Stdout) != "vagrant" {
		t.Fatalf("ExecAs vagrant id -un = %q (err %v), want vagrant", res.Stdout, err)
	}
	// CopyFile then read it back.
	if err := v.CopyFile(ctx, id, "/tmp/e2e", "hello.txt", 0o644, []byte("hi from dbcanvas")); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if res, err := v.Exec(ctx, id, []string{"cat", "/tmp/e2e/hello.txt"}, nil); err != nil || strings.TrimSpace(res.Stdout) != "hi from dbcanvas" {
		t.Fatalf("read back = %q (err %v)", res.Stdout, err)
	}
	// The guest sees its static private IP.
	ip, err := v.ContainerIP(ctx, id, net)
	if err != nil {
		t.Fatalf("ContainerIP: %v", err)
	}
	if res, err := v.Exec(ctx, id, []string{"sh", "-c", "ip -4 addr | grep -q " + ip + " && echo yes"}, nil); err != nil || strings.TrimSpace(res.Stdout) != "yes" {
		t.Fatalf("guest does not have private IP %s: %q (err %v)", ip, res.Stdout, err)
	}
	// The published port has a stable host mapping.
	if hp, err := v.ContainerPort(ctx, id, "5432/tcp"); err != nil || hp == "" {
		t.Fatalf("ContainerPort: %q (err %v)", hp, err)
	}
	t.Logf("e2e OK: VM %s up at %s, exec/copy/port all verified", id, ip)
}

// TestHybridConnectivityE2E is the Part-2 cross-engine spike: it stands up one VM
// (host-only subnet) and one Docker container (bridge subnet) on the same stack
// network, drives the real reconcileStackRouting (which installs the DOCKER-USER
// FORWARD rules and the VM's route to the Docker subnet), then proves bidirectional
// reachability both ways. Skipped unless DBCANVAS_VAGRANT_E2E=1 (boots a VM; minutes)
// and requires a reachable Docker daemon with the `alpine` image (busybox ping/httpd).
func TestHybridConnectivityE2E(t *testing.T) {
	if os.Getenv("DBCANVAS_VAGRANT_E2E") == "" {
		t.Skip("set DBCANVAS_VAGRANT_E2E=1 to run the real cross-engine e2e")
	}
	v := NewVagrant()
	if v == nil {
		t.Fatal("NewVagrant returned nil (vagrant/VBoxManage/ssh missing)")
	}
	// DBCANVAS_E2E_KEEP=1 leaves the peers, networks and rules up after the test (and
	// uses a stable root so the VM dir survives for `vagrant ssh`) for live debugging.
	keep := os.Getenv("DBCANVAS_E2E_KEEP") != ""
	if keep {
		v.root = filepath.Join(os.TempDir(), "dbcanvas-e2e-keep")
	} else {
		v.root = t.TempDir()
	}
	os.MkdirAll(filepath.Join(v.root, "vms"), 0o755)
	t.Logf("vagrant root: %s (keep=%v)", v.root, keep)

	d := NewDocker(envOr("DOCKER_SOCK", "/var/run/docker.sock"))
	ctx := context.Background()
	if err := d.Ping(ctx); err != nil {
		t.Fatalf("Docker not reachable: %v", err)
	}
	a := &App{docker: d, vagrant: v}

	const stackID = int64(990001)
	net := networkName(stackID)
	if ok, _ := d.ImageExists(ctx, "alpine:latest"); !ok {
		if err := d.EnsureImage(ctx, "alpine", "latest", ""); err != nil {
			t.Fatalf("pull alpine: %v", err)
		}
	}
	if err := d.NetworkEnsure(ctx, net); err != nil {
		t.Fatalf("docker NetworkEnsure: %v", err)
	}
	if err := v.NetworkEnsure(ctx, net); err != nil {
		t.Fatalf("vagrant NetworkEnsure: %v", err)
	}
	t.Cleanup(func() {
		if keep {
			return
		}
		bg := context.Background()
		a.unlinkStackNetworks(bg, stackID)
		d.NetworkRemove(bg, net)
		v.NetworkRemove(bg, net)
	})

	// Docker peer: an alpine container holding a persistent TCP/8080 listener (busybox
	// nc in a restart loop) so a VM can probe it, and carrying busybox ping so it can
	// probe the VM. (alpine's busybox has no httpd applet, so nc is used instead.)
	dkName := fmt.Sprintf("dbcanvas-%d-dk", stackID)
	dkID, err := d.ContainerCreate(ctx, ContainerSpec{
		Name: dkName, Image: "alpine:latest", Hostname: "dk", Network: net,
		Cmd: []string{"sh", "-c", "while true; do nc -l -p 8080 >/dev/null 2>&1; done"},
	})
	if err != nil {
		t.Fatalf("docker ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		if !keep {
			d.ContainerRemove(context.Background(), dkID)
		}
	})
	if err := d.ContainerStart(ctx, dkID); err != nil {
		t.Fatalf("docker ContainerStart: %v", err)
	}
	dockerIP, err := d.ContainerIP(ctx, dkID, net)
	if err != nil || dockerIP == "" {
		t.Fatalf("docker ContainerIP: %q (err %v)", dockerIP, err)
	}

	// VM peer on the host-only subnet.
	image := os.Getenv("DBCANVAS_E2E_IMAGE")
	if image == "" {
		image = "dbcanvas-systemd:ubuntu-22.04-amd64"
	}
	vmName := fmt.Sprintf("dbcanvas-%d-vm", stackID)
	t.Logf("creating VM %s (box download + boot may take minutes)…", vmName)
	vmID, err := v.ContainerCreate(ctx, ContainerSpec{Name: vmName, Image: image, Hostname: "vm", Network: net})
	if err != nil {
		t.Fatalf("vagrant ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		if !keep {
			v.ContainerRemove(context.Background(), vmID)
		}
	})
	if err := v.WaitSystemd(ctx, vmID, 3*time.Minute); err != nil {
		t.Fatalf("WaitSystemd: %v", err)
	}
	vmIP, err := v.ContainerIP(ctx, vmID, net)
	if err != nil || vmIP == "" {
		t.Fatalf("vagrant ContainerIP: %q (err %v)", vmIP, err)
	}
	t.Logf("peers up: docker %s @ %s, vm %s @ %s", dkID, dockerIP, vmID, vmIP)

	// Drive the real routing reconcile: FORWARD rules + the VM's route to the Docker
	// subnet. The VM node is typed "ps" (a VM type); the docker node "seaweedfs".
	st := Stack{ID: stackID, Backend: BackendVagrant,
		Design: []byte(`{"nodes":[{"id":"vm","type":"ps"},{"id":"dk","type":"seaweedfs"}]}`)}
	deps := []Deployment{
		{StackID: stackID, NodeID: "vm", ContainerID: vmID},
		{StackID: stackID, NodeID: "dk", ContainerID: dkID},
	}
	a.reconcileStackRouting(ctx, st, deps)

	// The FORWARD rules must now be present, tagged for this stack.
	if out, err := runHost(ctx, "iptables", "-S", "DOCKER-USER"); err != nil || !strings.Contains(out, stackRuleComment(stackID)) {
		t.Fatalf("FORWARD rules not installed: err %v, chain:\n%s", err, out)
	}
	// The VM must have a route to the Docker subnet via the host-only gateway.
	if res, err := v.Exec(ctx, vmID, []string{"ip", "route", "get", dockerIP}, nil); err != nil || res.Code != 0 {
		t.Fatalf("VM `ip route get %s` failed: %q (code %d, err %v)", dockerIP, res.Stdout, res.Code, err)
	} else {
		t.Logf("VM route to docker: %s", strings.TrimSpace(res.Stdout))
	}

	// Probe both directions, gathering diagnostics before asserting so one run shows
	// the full picture.
	// Docker -> VM: alpine's busybox ping; the VM kernel replies (no VM listener).
	dres, derr := d.Exec(ctx, dkID, []string{"ping", "-c", "2", "-W", "3", vmIP}, nil)
	dockerToVM := derr == nil && dres.Code == 0
	if !dockerToVM {
		t.Logf("Docker->VM ping %s -> code %d err %v\n out: %s\n err: %s", vmIP, dres.Code, derr, strings.TrimSpace(dres.Stdout), strings.TrimSpace(dres.Stderr))
	}
	// VM -> Docker: prefer ICMP (kernel reply), fall back to a TCP connect via bash
	// /dev/tcp so the check does not depend on ping being present in the box.
	vmToDocker := ""
	if pres, perr := v.Exec(ctx, vmID, []string{"ping", "-c", "2", "-W", "3", dockerIP}, nil); perr == nil && pres.Code == 0 {
		vmToDocker = "icmp"
	} else {
		t.Logf("VM ping %s -> code %d err %v\n out: %s", dockerIP, pres.Code, perr, strings.TrimSpace(pres.Stdout))
		probe := fmt.Sprintf("exec 3<>/dev/tcp/%s/8080 && echo REACHED", dockerIP)
		if tres, terr := v.Exec(ctx, vmID, []string{"bash", "-c", probe}, nil); terr == nil && strings.Contains(tres.Stdout, "REACHED") {
			vmToDocker = "tcp/8080"
		} else {
			t.Logf("VM tcp %s:8080 -> code %d err %v\n err: %s", dockerIP, tres.Code, terr, strings.TrimSpace(tres.Stderr))
		}
	}

	if !dockerToVM || vmToDocker == "" {
		for _, args := range [][]string{
			{"iptables", "-S", "DOCKER-USER", "-v"},
			{"iptables", "-t", "nat", "-S", "POSTROUTING", "-v"},
			{"conntrack", "-L", "-d", dockerIP},
		} {
			if out, _ := runHost(ctx, args[0], args[1:]...); out != "" {
				t.Logf("$ %s\n%s", strings.Join(args, " "), out)
			}
		}
		if res, _ := v.Exec(ctx, vmID, []string{"sh", "-c", "ip route get " + dockerIP + "; ping -c1 -W2 192.168.56.1 | tail -2"}, nil); res.Stdout != "" {
			t.Logf("VM path check:\n%s", res.Stdout)
		}
		t.Fatalf("cross-engine reachability failed: Docker->VM=%v, VM->Docker=%q", dockerToVM, vmToDocker)
	}
	t.Logf("Docker->VM ping OK (%s -> %s); VM->Docker OK via %s (%s -> %s)", dockerIP, vmIP, vmToDocker, vmIP, dockerIP)

	// Teardown must remove the FORWARD rules.
	a.unlinkStackNetworks(ctx, stackID)
	if out, _ := runHost(ctx, "iptables", "-S", "DOCKER-USER"); strings.Contains(out, stackRuleComment(stackID)) {
		t.Fatalf("FORWARD rules survived unlink:\n%s", out)
	}
	t.Logf("cross-engine e2e OK: bidirectional reachability proven, routing rules cleaned up")
}
