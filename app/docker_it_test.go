package main

import (
	"context"
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
