package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// TestEnsureImageCrossPlatform exercises EnsureImage against a real Docker daemon.
//
// It reproduces the macOS/Rosetta failure: a multi-arch image cached for the *other*
// platform makes the platform-blind ImageExists report "present", so a guard that
// skips the pull leaves containers/create?platform=… with no matching manifest
// ("image ... was found but does not provide the specified platform").
//
// Opt-in (needs the daemon socket and network): DOCKER_IT=1 go test -run EnsureImage
func TestEnsureImageCrossPlatform(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" {
		t.Skip("integration test; set DOCKER_IT=1 to run against /var/run/docker.sock")
	}
	const (
		repo = "alpine"
		tag  = "3.19"
		ref  = repo + ":" + tag
	)
	d := NewDocker("/var/run/docker.sock")
	ctx := context.Background()

	// Stage the failing situation: only the non-native platform is cached.
	if err := d.ImagePull(ctx, repo, tag, "linux/arm64"); err != nil {
		t.Fatalf("seed %s as arm64: %v", ref, err)
	}
	// The platform-blind check the old code relied on would skip the pull here.
	if ok, _ := d.ImageExists(ctx, ref); !ok {
		t.Fatalf("%s should appear to exist after the arm64 pull", ref)
	}

	if err := d.EnsureImage(ctx, repo, tag, "linux/amd64"); err != nil {
		t.Fatalf("EnsureImage for linux/amd64: %v", err)
	}
	id, err := d.ContainerCreate(ctx, ContainerSpec{
		Name:     "dbcanvas-ensureimage-it",
		Image:    ref,
		Platform: "linux/amd64",
		Cmd:      []string{"true"},
	})
	if err != nil {
		t.Fatalf("ContainerCreate for linux/amd64 after EnsureImage: %v", err)
	}
	d.ContainerRemove(ctx, id)
}

// imageArch returns the platform architecture a container was actually created with. It reads
// ImageManifestDescriptor.platform.architecture off the container inspect response rather than
// chasing .Image through /images/{ref}/json: under the containerd image store .Image resolves to
// the shared OCI index digest (the same one regardless of which platform variants are cached
// underneath it), whose own /images/.../json reports an empty Architecture. The manifest
// descriptor on the container is the one place that records which concrete platform variant this
// specific container actually got.
func imageArch(t *testing.T, d *Docker, ctx context.Context, containerID string) string {
	t.Helper()
	resp, err := d.do(ctx, "GET", "/containers/"+containerID+"/json", nil)
	if err != nil {
		t.Fatalf("inspect container: %v", err)
	}
	var c struct {
		ImageManifestDescriptor struct {
			Platform struct {
				Architecture string
			}
		}
	}
	if err := json.Unmarshal(drain(resp), &c); err != nil {
		t.Fatalf("decode container json: %v", err)
	}
	return c.ImageManifestDescriptor.Platform.Architecture
}

// TestK3DPlatformAmbiguity reproduces the bug reported live on Apple Silicon: a K3D cluster's
// nodes come up on the HOST's architecture even with DOCKER_PLATFORM correctly set, despite
// k3d.go's existing pre-pull of the target platform.
//
// Root cause (verified live against this daemon, which — like modern Docker Desktop — uses the
// containerd image store): a single tag can hold *multiple* platform variants at once. k3d
// creates its node containers via its own vendored Docker client with a nil platform (confirmed
// by reading k3d v5.8.3's pkg/runtimes/docker source), and the daemon resolves that ambiguity to
// the HOST's native architecture — not whichever variant was pulled most recently. Pre-pulling the
// target platform (EnsureImage) is therefore not sufficient on its own: it guarantees the target
// is present, not that it is the only thing present.
//
// The fix (k3d.go): Docker.ImageRemove(ref) before the platform-pinned EnsureImage pull, so only
// one variant is ever cached when k3d creates the node.
//
// Opt-in (needs the daemon socket and network): DOCKER_IT=1 go test -run K3DPlatformAmbiguity
func TestK3DPlatformAmbiguity(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" {
		t.Skip("integration test; set DOCKER_IT=1 to run against /var/run/docker.sock")
	}
	const (
		repo = "alpine"
		tag  = "3.19"
		ref  = repo + ":" + tag
	)
	d := NewDocker("/var/run/docker.sock")
	ctx := context.Background()

	// Mirror the user's report (host arm64, DOCKER_PLATFORM=linux/amd64) on whatever this test
	// happens to run on: the "target" platform is deliberately the one the HOST is not, so a
	// create that silently falls back to the host's arch is caught rather than accidentally
	// matching by coincidence.
	hostArch := dockerServerArch(t, d, ctx)
	target, targetPlatform := "arm64", "linux/arm64"
	if hostArch == "arm64" {
		target, targetPlatform = "amd64", "linux/amd64"
	}
	other := "linux/amd64"
	if targetPlatform == "linux/amd64" {
		other = "linux/arm64"
	}

	blindCreate := func(name string) string {
		id, err := d.ContainerCreate(ctx, ContainerSpec{Name: name, Image: ref, Cmd: []string{"true"}})
		if err != nil {
			t.Fatalf("platform-blind ContainerCreate (mimics k3d): %v", err)
		}
		return id
	}

	// ---- reproduce the bug: both platforms cached, no ImageRemove ----
	d.ImageRemove(ctx, ref)
	if err := d.EnsureImage(ctx, repo, tag, other); err != nil {
		t.Fatalf("seed %s as %s: %v", ref, other, err)
	}
	if err := d.EnsureImage(ctx, repo, tag, targetPlatform); err != nil {
		t.Fatalf("EnsureImage for %s (target platform): %v", targetPlatform, err)
	}
	id := blindCreate("dbcanvas-k3d-platform-it-bug")
	gotBug := imageArch(t, d, ctx, id)
	d.ContainerRemove(ctx, id)
	if gotBug != hostArch {
		t.Fatalf("expected the unpatched sequence to reproduce the bug (blind create falls back to host arch %q), got %q — the reproduction itself is stale", hostArch, gotBug)
	}

	// ---- confirm the fix: ImageRemove before the platform-pinned pull ----
	d.ImageRemove(ctx, ref)
	if err := d.EnsureImage(ctx, repo, tag, targetPlatform); err != nil {
		t.Fatalf("EnsureImage for %s after ImageRemove: %v", targetPlatform, err)
	}
	id = blindCreate("dbcanvas-k3d-platform-it-fixed")
	gotFixed := imageArch(t, d, ctx, id)
	d.ContainerRemove(ctx, id)
	if gotFixed != target {
		t.Fatalf("ImageRemove-before-pull should make a platform-blind create resolve to the pulled platform %q, got %q", target, gotFixed)
	}
}

// dockerServerArch returns the daemon's own architecture ("amd64", "arm64") via GET /version —
// distinct from HostArch's uname-style "x86_64", and matching the naming images.Architecture uses.
func dockerServerArch(t *testing.T, d *Docker, ctx context.Context) string {
	t.Helper()
	resp, err := d.do(ctx, "GET", "/version", nil)
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	var v struct {
		Arch string
	}
	if err := json.Unmarshal(drain(resp), &v); err != nil {
		t.Fatalf("decode /version: %v", err)
	}
	return v.Arch
}
