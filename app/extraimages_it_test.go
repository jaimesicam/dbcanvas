package main

import (
	"context"
	"os"
	"testing"
)

// TestBuildExtraImageForReal builds one of the catalogue's images through the same
// path the web interface uses, against a real Docker daemon.
//
// It is the only way to check the part that cannot be unit-tested: that a throwaway
// docker:cli container with the socket mounted can build a Dockerfile staged into
// it, that BuildKit's $BUILDPLATFORM/$TARGETARCH resolve there (they do not on the
// Engine API's own /build endpoint, which is why the container exists), and that the
// tag the node asks for is what comes out.
//
// Big Hole is the subject because it is the only one left that needs any of that:
// it is the last Dockerfile using $BUILDPLATFORM/$TARGETARCH, and unlike the
// Intranet, the VNC desktop and the collector it is not baked onto a systemd base
// that `make images` would have to produce first. (MClusterAdmin was the subject
// while DBCanvas built it; upstream publishes an image now, so the node pulls it and
// there is no build to exercise.) The cost is the npm build — minutes, not seconds.
//
// Opt-in (needs the daemon socket and network):
//
//	DOCKER_IT=1 go test -run BuildExtraImageForReal
func TestBuildExtraImageForReal(t *testing.T) {
	if os.Getenv("DOCKER_IT") == "" {
		t.Skip("integration test; set DOCKER_IT=1 to run against the Docker socket")
	}
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	// The Dockerfiles live in the repository here, not in an app image.
	if os.Getenv("DBCANVAS_IMAGES_DIR") == "" {
		t.Setenv("DBCANVAS_IMAGES_DIR", "../images")
	}

	a := &App{docker: NewDocker(sock)}
	ctx := context.Background()
	if err := a.docker.Ping(ctx); err != nil {
		t.Skipf("no daemon at %s: %v", sock, err)
	}

	e, ok := extraImageByID("bighole")
	if !ok {
		t.Fatal("no bighole in the catalogue")
	}

	// Start from nothing, so a pass means this build produced the image rather than
	// an earlier `make` having done it.
	a.docker.ImageRemove(ctx, e.Tag)
	if present, _ := a.docker.ImageExists(ctx, e.Tag); present {
		t.Fatalf("%s is still present after removing it — is a container holding it?", e.Tag)
	}

	if err := a.buildExtraImage(ctx, e); err != nil {
		t.Fatalf("build %s: %v", e.Tag, err)
	}
	present, err := a.docker.ImageExists(ctx, e.Tag)
	if err != nil || !present {
		t.Fatalf("the build reported success but %s is absent (%v)", e.Tag, err)
	}

	// A second build is a no-op the UI can trigger by accident (two admins, two
	// clicks): it must not fail, and must leave the tag in place.
	if err := a.buildExtraImage(ctx, e); err != nil {
		t.Errorf("rebuilding an image that is already there failed: %v", err)
	}
	if present, _ := a.docker.ImageExists(ctx, e.Tag); !present {
		t.Errorf("%s went missing after a rebuild", e.Tag)
	}
}
