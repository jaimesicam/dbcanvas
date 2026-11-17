package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestStackFilePlatformParses(t *testing.T) {
	for in, want := range map[string]string{"linux/amd64": "amd64", "arm64": "arm64", "linux/arm64": "arm64"} {
		sf, err := parseStackFile([]byte("name: a\nplatform: " + in + "\nnodes: [ps]\n"))
		if err != nil || sf.Platform != want {
			t.Errorf("%s: %q %v", in, sf.Platform, err)
		}
	}
	if _, err := parseStackFile([]byte("name: a\nplatform: x86\nnodes: [ps]\n")); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("a bad platform: %v", err)
	}
}

// The stack's platform reaches a compose entry before its version is resolved, so the version
// comes from that architecture's catalogue; support nodes stay on Automatic.
func TestStackFilePlatformInComposeEntries(t *testing.T) {
	archCatalogs(t) // amd64: Percona Server 8.0 only; arm64: 8.0 and 8.4
	sf := stackFile{Name: "lab", Nodes: []composeNodeSpec{{Kind: "ps", OS: "el9", Version: "8.4"}}}
	if _, _, err := stackFileDesign(sf, nil); err != nil {
		t.Fatalf("arm64 (the default here) has 8.4: %v", err)
	}
	sf.Platform = "amd64"
	if _, _, err := stackFileDesign(sf, nil); err == nil {
		t.Fatal("amd64 has no 8.4, but the platform did not reach the version resolution")
	}
	sf.Nodes[0].Version = "8.0"
	d, _, err := stackFileDesign(sf, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc designDoc
	json.Unmarshal(d, &doc)
	for _, n := range doc.Nodes {
		switch n.Type {
		case "ps":
			if n.Arch != "amd64" {
				t.Errorf("ps: arch %q", n.Arch)
			}
		case "intranet":
			if n.Arch != "" {
				t.Errorf("the Intranet followed the platform: %q", n.Arch)
			}
		}
	}
}

func TestStampPlatform(t *testing.T) {
	design := `{"nodes":[` +
		`{"id":"i","type":"intranet"},{"id":"v","type":"vnc"},{"id":"m","type":"pmm"},` +
		`{"id":"p","type":"ps"},{"id":"q","type":"ps","arch":"arm64"},{"id":"r","type":"ps"},` +
		`{"id":"x1","type":"pxc","frameId":"x"},{"id":"k1","type":"k3d","frameId":"k"}],` +
		`"frames":[{"id":"x","type":"pxc"},{"id":"k","type":"k3d"}],"edges":[],"view":{"z":1}}`
	out, err := stampPlatform(json.RawMessage(design), "amd64", map[string]bool{"r": true})
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Nodes  []map[string]any `json:"nodes"`
		Frames []map[string]any `json:"frames"`
		View   map[string]any   `json:"view"`
	}
	json.Unmarshal(out, &d)
	got := map[string]any{}
	for _, it := range append(d.Nodes, d.Frames...) {
		got[it["id"].(string)] = it["arch"]
	}
	want := map[string]any{"i": nil, "v": nil, "m": nil, "p": "amd64", "q": "arm64", "r": nil, "x1": nil, "k1": nil, "x": "amd64", "k": nil}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: arch %v, want %v", id, got[id], w)
		}
	}
	if d.View == nil {
		t.Error("the design's other keys were not kept")
	}
}

// The plan says what each node runs as, and whether that is emulated.
func TestPlanSaysTheArchitecture(t *testing.T) {
	archCatalogs(t)
	app := newTestApp(t)
	app.docker = NewDocker(filepath.Join(t.TempDir(), "none.sock"))
	app.hostArch = "arm64"
	u, _ := app.ensureOwner()
	sf := stackFile{Name: "lab", Platform: "amd64", Nodes: []composeNodeSpec{{Kind: "ps", OS: "el9", Version: "8.0"}}}
	p, _, err := app.planStackFile(context.Background(), u.ID, sf)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]stackPlanItem{}
	for _, it := range p.Added {
		seen[it.Type] = it
	}
	if ps := seen["ps"]; ps.Arch != "amd64" || !ps.Emulated {
		t.Errorf("ps: %+v", ps)
	}
	if in := seen["intranet"]; in.Arch != "arm64" || in.Emulated {
		t.Errorf("intranet: %+v", in)
	}
}
