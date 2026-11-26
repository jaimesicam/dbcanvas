package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLimaEngineLive drives a real VM through the Engine: create (boot and baseline), commands as
// root and as another user, files both ways, the terminal, lookups, stop and start, and removal.
// It needs limactl 2 or newer and takes minutes:
//
//	LIMA_IT=1 [LIMA_IT_HOME=<short folder>] [LIMA_IT_IMAGE=dbcanvas-systemd:oraclelinux-9-arm64-v0]
func TestLimaEngineLive(t *testing.T) {
	if os.Getenv("LIMA_IT") == "" {
		t.Skip("integration test; see the comment for the environment it needs")
	}
	home := firstNonEmpty(os.Getenv("LIMA_IT_HOME"), "/tmp/dbcanvas-lima-it")
	l, why := NewLima(home)
	if l == nil {
		t.Fatal(why)
	}
	image := firstNonEmpty(os.Getenv("LIMA_IT_IMAGE"), "dbcanvas-systemd:ubuntu-24.04-"+runtime.GOARCH+"-v0")
	name := "dbcanvas-it-1-n-lima-01"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	if old, ok, _ := l.ContainerByName(ctx, name); ok {
		l.ContainerRemove(ctx, old)
	}
	start := time.Now()
	id, err := l.ContainerCreate(ctx, ContainerSpec{Name: name, Image: image, Hostname: "lima-01", CPUs: 2, MemoryMB: 2048,
		Network: "dbcanvas-it-stack-1", IPv4Address: "10.99.0.200"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created %s from %s in %s", id, image, time.Since(start).Round(time.Second))
	defer func() {
		l.ContainerRemove(context.Background(), id)
		if _, ok, _ := l.ContainerByName(context.Background(), name); ok {
			t.Error("the VM survived ContainerRemove")
		}
	}()

	must := func(res ExecResult, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if res.Code != 0 {
			t.Fatalf("exit %d: %s", res.Code, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	if got := must(l.Exec(ctx, id, []string{"id", "-un"}, nil)); got != "root" {
		t.Errorf("Exec runs as %q, want root", got)
	}
	if got := must(l.ExecAs(ctx, id, "nobody", []string{"id", "-un"}, nil)); got != "nobody" {
		t.Errorf("ExecAs nobody runs as %q", got)
	}
	if got := must(l.Exec(ctx, id, []string{"sh", "-c", "echo $X"}, []string{"X=with space"})); got != "with space" {
		t.Errorf("env: %q", got)
	}
	if got := must(l.Exec(ctx, id, []string{"hostname"}, nil)); got != "lima-01" {
		t.Errorf("hostname %q", got)
	}
	for _, tool := range []string{"percona-release", "pt-summary", "tc", "sar", "git", "sudo"} {
		must(l.Exec(ctx, id, []string{"sh", "-c", "command -v " + tool}, nil))
	}
	if res, err := l.Exec(ctx, id, []string{"false"}, nil); err != nil || res.Code != 1 {
		t.Errorf("a failing command: code %d, err %v", res.Code, err)
	}
	if got := must(l.ExecInput(ctx, id, "", []string{"cat"}, nil, []byte("piped"))); got != "piped" {
		t.Errorf("stdin: %q", got)
	}

	// Files: CopyFile, an archive in, and the same path back out.
	if err := l.CopyFile(ctx, id, "/var/tmp/dbc-it", "a.txt", 0o640, []byte("alpha")); err != nil {
		t.Fatal(err)
	}
	if got := must(l.Exec(ctx, id, []string{"stat", "-c", "%a %s", "/var/tmp/dbc-it/a.txt"}, nil)); got != "640 5" {
		t.Errorf("CopyFile result: %q", got)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	tw.WriteHeader(&tar.Header{Name: "b.txt", Mode: 0o644, Size: 4})
	tw.Write([]byte("beta"))
	tw.Close()
	if err := l.PutArchive(ctx, id, "/var/tmp/dbc-it", tb.Bytes()); err != nil {
		t.Fatal(err)
	}
	rc, err := l.GetArchiveStream(ctx, id, "/var/tmp/dbc-it/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(tr)
	rc.Close()
	if hdr.Name != "b.txt" || string(body) != "beta" {
		t.Errorf("archive back: %s %q", hdr.Name, body)
	}

	// The terminal: a PTY that runs a command and resizes.
	conn, err := l.HijackExec(ctx, id, []string{"sh", "-c", "stty size; echo term-ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	l.ResizeExec(ctx, conn.ExecID, 100, 30)
	out, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(out), "term-ok") {
		t.Errorf("terminal output %q", out)
	}

	// Lookups.
	if got, ok, _ := l.ContainerByName(ctx, name); !ok || got != id {
		t.Errorf("ContainerByName: %s %v", got, ok)
	}
	if ids, _ := l.ContainersByNamePrefix(ctx, "dbcanvas-it-1-"); len(ids) != 1 || ids[0] != id {
		t.Errorf("ContainersByNamePrefix: %v", ids)
	}
	if ip, _ := l.ContainerIP(ctx, id, "dbcanvas-it-stack-1"); ip != "10.99.0.200" {
		t.Errorf("ContainerIP: %q", ip)
	}
	if list, _ := l.ListManaged(ctx); len(list) != 1 || list[0].Name != name || list[0].State != "running" {
		t.Errorf("ListManaged: %+v", list)
	}

	// A resource sample, twice so CPU has a previous one to compare with.
	l.ContainerStats(ctx, id)
	if st, err := l.ContainerStats(ctx, id); err != nil || st.MemLimit <= 0 || st.MemUsed <= 0 || st.NetRx <= 0 || st.BlkRead <= 0 {
		t.Errorf("ContainerStats: %+v %v", st, err)
	}

	// Stop and start keep the VM and what it holds.
	if err := l.ContainerStop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if st := l.state(ctx, id); st != "exited" {
		t.Errorf("after stop: %q", st)
	}
	if err := l.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := l.WaitSystemd(ctx, id, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := must(l.Exec(ctx, id, []string{"cat", "/var/tmp/dbc-it/a.txt"}, nil)); got != "alpha" {
		t.Errorf("after restart: %q", got)
	}
}
