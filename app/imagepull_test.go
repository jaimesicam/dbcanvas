package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPullSourcesFollowTheRegistryNames(t *testing.T) {
	got := pullSources("reg.example.net/dbc", "dbcanvas-systemd:oraclelinux-9-arm64-v0.0.30", "x86_64")
	if len(got) != 1 || got[0].String() != "reg.example.net/dbc/dbcanvas-systemd:oraclelinux-9-arm64-v0.0.30" || got[0].platform != "linux/arm64" {
		t.Fatalf("base image: %+v", got)
	}
	// A demo application names no architecture: its architecture tag first, then the plain one,
	// both for the host's architecture.
	got = pullSources("reg.example.net/dbc", "dbcanvas-perfsim:v0.0.30", "aarch64")
	if len(got) != 2 || got[0].String() != "reg.example.net/dbc/dbcanvas-perfsim:v0.0.30-arm64" ||
		got[1].String() != "reg.example.net/dbc/dbcanvas-perfsim:v0.0.30" || got[1].platform != "linux/arm64" {
		t.Fatalf("demo application: %+v", got)
	}
	for ref, want := range map[string]string{
		"dbcanvas-systemd:ubuntu-24.04-amd64-v1": "images",
		"dbcanvas-vnc:ubuntu-24.04-amd64-v1":     "vnc-image",
		"dbcanvas-pagerpanic:v1":                 "pagerpanic-image",
	} {
		if got := imageMakeTarget(ref); got != want {
			t.Errorf("imageMakeTarget(%s) = %s, want %s", ref, got, want)
		}
	}
	if m := ownImageRe.FindString("Missing image dbcanvas-intranet:oraclelinux-9-amd64-v0.0.30 — run `make intranet-image` first"); m != "dbcanvas-intranet:oraclelinux-9-amd64-v0.0.30" {
		t.Fatalf("ownImageRe = %q", m)
	}
	if ownImage("percona/pmm-server:3") || !ownImage("dbcanvas-vnc:x") {
		t.Fatal("ownImage")
	}
}

// fakeDaemon is the part of the Docker API a pull touches.
type fakeDaemon struct {
	mu       sync.Mutex
	arch     string            // the host's, as /info says it
	images   map[string]string // local ref → architecture
	registry map[string]string // remote ref → architecture; missing is "manifest unknown"
	pulls    atomic.Int32
	tags     []string
	creates  int
}

func (f *fakeDaemon) serve(t *testing.T) *Docker {
	// A short directory: a socket path has a length limit (104 bytes on macOS), and
	// t.TempDir is named after the test.
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.handle)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewDocker(sock)
}

func (f *fakeDaemon) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case p == "/info":
		json.NewEncoder(w).Encode(map[string]string{"Architecture": f.arch})
	case p == "/images/create":
		f.pulls.Add(1)
		ref := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond) // long enough for other callers to join it
		f.mu.Lock()
		w.WriteHeader(200)
		arch, ok := f.registry[ref]
		if !ok {
			w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}` + "\n"))
			return
		}
		f.images[ref] = arch
		w.Write([]byte(`{"status":"Downloaded newer image"}` + "\n"))
	case strings.HasPrefix(p, "/distribution/"):
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/distribution/"), "/json")
		arch, ok := f.registry[ref]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"manifest unknown"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Platforms": []map[string]string{{"os": "linux", "architecture": arch}}})
	case strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
		src := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/tag")
		dst := r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
		f.images[dst] = f.images[src]
		f.tags = append(f.tags, dst)
		w.WriteHeader(201)
	case strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
		arch, ok := f.images[ref]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"No such image: ` + ref + `"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"Architecture": arch})
	case p == "/containers/create":
		var body struct{ Image string }
		json.NewDecoder(r.Body).Decode(&body)
		f.creates++
		if _, ok := f.images[body.Image]; !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"No such image: ` + body.Image + `"}`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"c1"}`))
	default:
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}
}

func resetRemoteAvail() {
	remoteAvail.Lock()
	remoteAvail.m = map[string]remoteAnswer{}
	remoteAvail.Unlock()
}

// A container whose DBCanvas image is missing gets it pulled from the registry, under the
// local name, and is created on the second try.
func TestCreatePullsAMissingImage(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "reg.example.net/dbc")
	ref := "dbcanvas-systemd:debian-12-amd64-v9.9.1"
	f := &fakeDaemon{arch: "x86_64", images: map[string]string{},
		registry: map[string]string{"reg.example.net/dbc/" + ref: "amd64"}}
	a := &App{docker: f.serve(t)}
	a.docker.pullMissing = a.pullMissing
	if _, err := a.docker.ContainerCreate(context.Background(), ContainerSpec{Name: "n1", Image: ref}); err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 || f.pulls.Load() != 1 || len(f.tags) == 0 || f.tags[0] != ref {
		t.Fatalf("creates %d, pulls %d, tags %v", f.creates, f.pulls.Load(), f.tags)
	}
}

// An image the registry cannot provide is the create's error, with what was tried, the
// registry's reason, and how to build it instead.
func TestAFailedPullSaysWhy(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "reg.example.net/dbc")
	ref := "dbcanvas-vnc:ubuntu-24.04-amd64-v9.9.2"
	f := &fakeDaemon{arch: "x86_64", images: map[string]string{}, registry: map[string]string{}}
	a := &App{docker: f.serve(t)}
	a.docker.pullMissing = a.pullMissing
	_, err := a.docker.ContainerCreate(context.Background(), ContainerSpec{Name: "n1", Image: ref})
	if err == nil {
		t.Fatal("created with no image")
	}
	for _, want := range []string{ref, "reg.example.net/dbc/" + ref, "manifest unknown", "make vnc-image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

// A pulled image of the wrong architecture is refused rather than run emulated.
func TestAPullOfTheWrongArchitectureIsRefused(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "reg.example.net/dbc")
	ref := "dbcanvas-intranet:oraclelinux-9-arm64-v9.9.3"
	f := &fakeDaemon{arch: "aarch64", images: map[string]string{},
		registry: map[string]string{"reg.example.net/dbc/" + ref: "amd64"}}
	a := &App{docker: f.serve(t)}
	err := a.ensureImage(context.Background(), a.docker, ref, nil)
	if err == nil || !strings.Contains(err.Error(), "not arm64") {
		t.Fatalf("err = %v", err)
	}
}

// Nodes that need one image at the same time share one pull.
func TestConcurrentNeedsShareOnePull(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "reg.example.net/dbc")
	ref := "dbcanvas-perfsim:v9.9.4"
	f := &fakeDaemon{arch: "x86_64", images: map[string]string{},
		registry: map[string]string{"reg.example.net/dbc/dbcanvas-perfsim:v9.9.4-amd64": "amd64"}}
	a := &App{docker: f.serve(t)}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = a.ensureImage(context.Background(), a.docker, ref, nil) }(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.pulls.Load(); n != 1 {
		t.Fatalf("%d pulls, want 1", n)
	}
	// The demo application also gets its architecture tag, as a local build has.
	if _, ok := f.images[ref+"-amd64"]; !ok {
		t.Fatalf("tags %v", f.tags)
	}
}

// Validate says a missing image will be pulled when the registry has it, and why not when not.
func TestValidateSaysWhatTheDeployWillPull(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "reg.example.net/dbc")
	resetRemoteAvail()
	have := "dbcanvas-intranet:oraclelinux-9-amd64-v9.9.5"
	lack := "dbcanvas-vnc:ubuntu-24.04-amd64-v9.9.5"
	f := &fakeDaemon{arch: "x86_64", images: map[string]string{},
		registry: map[string]string{"reg.example.net/dbc/" + have: "amd64"}}
	a := &App{docker: f.serve(t)}
	out := a.pullableImageIssues(context.Background(), []issue{
		{Level: "error", Message: "Missing image " + have + " — run `make intranet-image` first"},
		{Level: "error", Message: "Missing image " + lack + " — run `make vnc-image` first"},
		{Level: "error", Message: "Unknown PMM version"},
	})
	if out[0].Level != "warning" || !strings.Contains(out[0].Message, "the deploy pulls it from reg.example.net/dbc/"+have) {
		t.Errorf("pullable: %+v", out[0])
	}
	if out[1].Level != "error" || !strings.Contains(out[1].Message, "cannot be pulled") || !strings.Contains(out[1].Message, "manifest unknown") {
		t.Errorf("not pullable: %+v", out[1])
	}
	if out[2].Message != "Unknown PMM version" {
		t.Errorf("unrelated: %+v", out[2])
	}
	if f.pulls.Load() != 0 {
		t.Error("Validate pulled")
	}
}

// IMAGE_REGISTRY=off leaves a missing image an error that says so.
func TestPullingCanBeTurnedOff(t *testing.T) {
	t.Setenv("IMAGE_REGISTRY", "off")
	ref := "dbcanvas-vnc:ubuntu-24.04-amd64-v9.9.6"
	f := &fakeDaemon{arch: "x86_64", images: map[string]string{}, registry: map[string]string{}}
	a := &App{docker: f.serve(t)}
	err := a.ensureImage(context.Background(), a.docker, ref, nil)
	if err == nil || !strings.Contains(err.Error(), "IMAGE_REGISTRY=off") || f.pulls.Load() != 0 {
		t.Fatalf("err = %v, pulls %d", err, f.pulls.Load())
	}
}
