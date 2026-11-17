package main

import (
	"runtime"
	"testing"
)

// The socket k3d and the image builder bind-mount is the daemon's: on macOS the client's
// path (a Docker Desktop or Rancher Desktop socket in the home folder) is not in the VM.
func TestDaemonSocketIsTheDaemonsPath(t *testing.T) {
	t.Setenv("DOCKER_SOCK", "/home/example/.rd/docker.sock")
	want := "/var/run/docker.sock"
	if runtime.GOOS == "linux" {
		want = "/home/example/.rd/docker.sock"
	}
	if got := daemonSocket(); got != want {
		t.Fatalf("daemonSocket() = %q, want %q", got, want)
	}
}
