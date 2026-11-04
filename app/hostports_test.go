package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// holdPort binds a free loopback port for the test, standing in for a host process.
func holdPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

func TestProbePorts(t *testing.T) {
	busy := holdPort(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	free := l.Addr().(*net.TCPAddr).Port
	l.Close()
	res, err := probePorts(probeRequest{IP: "127.0.0.1", Check: []int{busy, free}, Count: 3, Exclude: []int{free}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Free) != 1 || res.Free[0] != free {
		t.Errorf("free = %v, want [%d] (and not the held %d)", res.Free, free, busy)
	}
	seen := map[int]bool{}
	for _, p := range res.Picked {
		if p == busy || p == free || seen[p] {
			t.Errorf("picked %d: held, checked or twice (%v)", p, res.Picked)
		}
		seen[p] = true
	}
	if len(res.Picked) != 3 {
		t.Errorf("picked %v, want 3", res.Picked)
	}
}

func TestPickHostPorts(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	a, b := containerName(1, "pmm-1"), containerName(2, "pmm-2")

	// A first deploy: chosen and reserved.
	got, err := app.pickHostPorts(ctx, a, []PortMap{{ContainerPort: 8080}, {ContainerPort: 8443}})
	if err != nil || len(got) != 2 || got[0] == got[1] || got[0] == 0 {
		t.Fatalf("first pick: %v %v", got, err)
	}
	owners, _ := app.store.HostPortOwners()
	if owners[got[0]] != a || owners[got[1]] != a {
		t.Fatalf("not reserved for %s: %v", a, owners)
	}
	// A redeploy keeps its own ports while they are free on the host.
	again, err := app.pickHostPorts(ctx, a, []PortMap{{HostPort: got[0], Auto: true}, {HostPort: got[1], Auto: true}})
	if err != nil || again[0] != got[0] || again[1] != got[1] {
		t.Errorf("redeploy moved its ports: %v → %v (%v)", got, again, err)
	}
	// Another node asking for a's port: re-picked when DBCanvas chose it…
	other, err := app.pickHostPorts(ctx, b, []PortMap{{HostPort: got[0], Auto: true}})
	if err != nil || other[0] == got[0] {
		t.Errorf("b was handed a's port %d: %v %v", got[0], other, err)
	}
	// …refused, naming the holder, when a user typed it.
	_, err = app.pickHostPorts(ctx, b, []PortMap{{HostPort: got[0]}})
	var taken errPortTaken
	if !errors.As(err, &taken) || !strings.Contains(err.Error(), "node pmm-1 of stack 1") {
		t.Errorf("fixed port held by another node: %v", err)
	}

	// A host process holding a port nobody reserved.
	busy := holdPort(t)
	if p, err := app.pickHostPorts(ctx, b, []PortMap{{HostPort: busy, Auto: true}}); err != nil || p[0] == busy {
		t.Errorf("auto port held on the host was kept: %v %v", p, err)
	}
	if _, err := app.pickHostPorts(ctx, b, []PortMap{{HostPort: busy}}); err == nil || !strings.Contains(err.Error(), "another program") {
		t.Errorf("fixed port held on the host: %v", err)
	}

	// Its own reservation is no protection against a host process: re-picked.
	mine := holdPort(t)
	app.store.SetHostPorts(b, []int{mine})
	if p, err := app.pickHostPorts(ctx, b, []PortMap{{HostPort: mine, Auto: true}}); err != nil || p[0] == mine {
		t.Errorf("own auto port held on the host was kept: %v %v", p, err)
	}

	// An owner's set is exactly its last publish: the port it no longer uses is free again.
	app.pickHostPorts(ctx, a, []PortMap{{HostPort: got[0], Auto: true}})
	owners, _ = app.store.HostPortOwners()
	if _, ok := owners[got[1]]; ok {
		t.Errorf("port %d still reserved after %s stopped publishing it", got[1], a)
	}
}

func TestHostPortsReleased(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("alice", "x", RoleUser, StatusApproved)
	st, err := app.store.CreateStack("s", u.ID, "24h", nil, []byte(`{}`))
	if err != nil {
		t.Skipf("create stack: %v", err)
	}
	node := containerName(st.ID, "pmm-1")
	app.store.SetHostPorts(node, []int{40001, 40002})
	app.store.SetHostPorts(node+"#everest", []int{40003})
	app.store.SetHostPorts(containerName(st.ID, "vnc-1"), []int{40004})
	app.store.SetHostPorts(containerName(st.ID+1, "pmm-1"), []int{40005})

	app.store.DeleteDeployment(st.ID, "pmm-1")
	owners, _ := app.store.HostPortOwners()
	for _, p := range []int{40001, 40002, 40003} {
		if _, ok := owners[p]; ok {
			t.Errorf("port %d of the deleted node is still reserved", p)
		}
	}
	if owners[40004] == "" || owners[40005] == "" {
		t.Errorf("deleting one node released others: %v", owners)
	}
	app.store.DeleteStack(st.ID)
	owners, _ = app.store.HostPortOwners()
	if _, ok := owners[40004]; ok {
		t.Error("deleting the stack kept its ports")
	}
	if owners[40005] == "" {
		t.Error("deleting a stack released another stack's port (prefix 1 vs 11?)")
	}
}

// TestDockerStartErrorKeepsMessage: the reason Docker gives must reach the user. It
// used to be read and thrown away first, leaving "docker start:  (500)".
func TestDockerStartErrorKeepsMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"failed to bind host port 127.0.0.1:45499/tcp: address already in use"}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	err = NewDocker(sock).ContainerStart(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("err = %v", err)
	}
}

func TestDescribeOwner(t *testing.T) {
	if got := describeOwner(containerName(13, "pmm-mw28b7je-6")); got != "node pmm-mw28b7je-6 of stack 13" {
		t.Errorf("describeOwner = %q", got)
	}
	if got := describeOwner(containerName(4, "k3d-1") + "#everest"); got != "node k3d-1 of stack 4 (everest)" {
		t.Errorf("describeOwner = %q", got)
	}
	_ = strconv.Itoa
}
