package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPMMArch(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	app := newTestApp(t)
	registryPlatforms.Lock()
	registryPlatforms.m["percona/pmm-server:3.10.0"] = platformsAt{platforms: []string{"linux/amd64", "linux/arm64"}, at: time.Now()}
	registryPlatforms.m["percona/pmm-server:3.9.0"] = platformsAt{platforms: []string{"linux/amd64"}, at: time.Now()}
	registryPlatforms.Unlock()
	ctx := context.Background()
	cases := []struct {
		version, arch, want, note, err string
	}{
		{"3.10.0", "", "arm64", "", ""},
		{"3.10.0", "arm64", "arm64", "", ""},
		{"3.10.0", "amd64", "amd64", "", ""},
		{"3.9.0", "", "amd64", "runs as amd64 under emulation", ""},
		{"3.9.0", "arm64", "", "", "publishes arm64 from 3.10.0"},
	}
	for _, c := range cases {
		arch, note, err := app.pmmArch(ctx, 0, designNode{ID: "p", Type: "pmm", Version: c.version, Arch: c.arch})
		if arch != c.want || !strings.Contains(note, c.note) || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %q: arch %q note %q err %v", c.version, c.arch, arch, note, err)
		}
	}
	// A deployed PMM keeps what it was built as, whatever its release has now.
	u, _ := app.ensureOwner()
	st, _ := app.store.CreateStack("lab", u.ID, "2h", expiryFor("2h"), []byte(defaultDesign))
	cfg, _ := json.Marshal(pmmConfig{Arch: "amd64"})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "p", State: DeployRunning, Config: cfg})
	if arch, _, _ := app.pmmArch(ctx, st.ID, designNode{ID: "p", Type: "pmm", Version: "3.10.0"}); arch != "amd64" {
		t.Errorf("a deployed amd64 PMM moved to %s", arch)
	}
}
