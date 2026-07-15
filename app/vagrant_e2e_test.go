package main

import (
	"context"
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
