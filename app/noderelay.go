package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// noderelay.go — the relay for containers without bash.
//
// The tunnel's relay (nodedial.go) is bash's /dev/tcp where the container has bash. A k3s
// node has none (nor nc or socat), and a K3D-only stack may have no other container to
// relay through, so DBCanvas carries a small static forwarder, cmd/dbcanvas-relay, for
// each Linux architecture, and copies it into such a container through the archive API
// the first time it is needed.

//go:embed relaybin
var relayBins embed.FS

// relayBinPath is where the forwarder is put inside a container.
const relayBinDir, relayBinName = "/tmp", ".dbcanvas-relay"

// relayBin returns the forwarder for a Linux architecture, or false when this build does
// not carry one.
func relayBin(arch string) ([]byte, bool) {
	gz, err := relayBins.ReadFile("relaybin/dbcanvas-relay-linux-" + arch + ".gz")
	if err != nil {
		return nil, false
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, false
	}
	b, err := io.ReadAll(r)
	return b, err == nil
}

// relayInstalled remembers the containers the forwarder has been copied into.
var relayInstalled = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// installRelayBin copies the forwarder into the container unless it is there already.
func (d *Docker) installRelayBin(ctx context.Context, containerID string) error {
	relayInstalled.Lock()
	done := relayInstalled.m[containerID]
	relayInstalled.Unlock()
	if done {
		return nil
	}
	arch, err := d.containerArch(ctx, containerID)
	if err != nil {
		return err
	}
	bin, ok := relayBin(arch)
	if !ok {
		return fmt.Errorf("%w: this build carries no relay for linux/%s", errRelayUnusable, arch)
	}
	if err := d.CopyFile(ctx, containerID, relayBinDir, relayBinName, 0o755, bin); err != nil {
		return fmt.Errorf("%w: copy the relay in: %v", errRelayUnusable, err)
	}
	relayInstalled.Lock()
	relayInstalled.m[containerID] = true
	relayInstalled.Unlock()
	return nil
}

// forgetRelayBin is called when running the forwarder found it missing (a recreated
// container keeps its ID only across a restart, but /tmp may be a tmpfs).
func forgetRelayBin(containerID string) {
	relayInstalled.Lock()
	delete(relayInstalled.m, containerID)
	relayInstalled.Unlock()
}

// containerArch is the architecture of the container's image (amd64, arm64).
func (d *Docker) containerArch(ctx context.Context, containerID string) (string, error) {
	resp, err := d.do(ctx, "GET", "/containers/"+containerID+"/json", nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", errBody("inspect container", resp)
	}
	var out struct {
		Image string `json:"Image"`
	}
	if err := json.Unmarshal(drain(resp), &out); err != nil {
		return "", err
	}
	return d.ImageArch(ctx, out.Image)
}
