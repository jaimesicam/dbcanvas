package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestValidInstance(t *testing.T) {
	for _, ok := range []string{"dbcanvas", "dbcanvas-alice", "dbcanvas-jane-doe", "dbcanvas-u2", "lab-a1"} {
		if why := validInstance(ok); why != "" {
			t.Errorf("%q rejected: %s", ok, why)
		}
	}
	for _, bad := range []string{"", "DBCanvas", "dbcanvas-5", "dbcanvas-jane-2", "dbcanvas-s2", "dbcanvas--x", "-x", "x-", "9lab", "dbcanvas_x"} {
		if validInstance(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// withInstance runs f as the installation called name.
func withInstance(t *testing.T, name string, f func()) {
	t.Helper()
	old := instanceName
	instanceName = name
	defer func() { instanceName = old }()
	f()
}

// The installation called "dbcanvas" — every one made before DBCANVAS_INSTANCE — must name things
// exactly as it always has, or upgrading orphans its running stacks.
func TestLegacyInstanceNames(t *testing.T) {
	withInstance(t, legacyInstance, func() {
		f := designFrame{Label: "k3d-00"}
		for got, want := range map[string]string{
			containerName(12, "pg-1"): "dbcanvas-12-pg-1",
			networkName(12):           "dbcanvas-stack-12",
			k3dClusterName(12, f):     "k3d-00-s12",
			pmmDataVolume(12, "pmm"):  "dbcanvas-pmm-12-pmm",
			stackRuleComment(12):      "dbcanvas-stack-12",
		} {
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		}
	})
}

// Two installations on one daemon: neither may read the other's containers or clusters as its own.
func TestInstancesDoNotClaimEachOther(t *testing.T) {
	var legacyPG, janePG, legacyK3D, janeK3D string
	withInstance(t, legacyInstance, func() {
		legacyPG = containerName(3, "pg")
		legacyK3D = k3dNodeContainer(k3dClusterName(3, designFrame{Label: "k3d-00"}), 0)
	})
	withInstance(t, "dbcanvas-jane", func() {
		janePG = containerName(3, "pg")
		janeK3D = k3dNodeContainer(k3dClusterName(3, designFrame{Label: "k3d-00"}), 0)
		if janePG != "dbcanvas-jane-3-pg" || janeK3D != "k3d-k3d-00-s3-jane-server-0" {
			t.Fatalf("jane's names: %q %q", janePG, janeK3D)
		}
		if _, ok := stackIDFromInstanceName(legacyPG); ok {
			t.Errorf("jane claims %q", legacyPG)
		}
		if _, ok := k3dStackIDFromContainer(legacyK3D); ok {
			t.Errorf("jane claims %q", legacyK3D)
		}
		if id, ok := k3dStackIDFromContainer(janeK3D); !ok || id != 3 {
			t.Errorf("jane does not recognise her own %q (%d, %v)", janeK3D, id, ok)
		}
		if got := k3dClustersOfStack([]string{"k3d-00-s3", "k3d-00-s3-jane", "k3d-00-s3-x-jane"}, 3); len(got) != 1 || got[0] != "k3d-00-s3-jane" {
			t.Errorf("jane's stack 3 clusters: %v", got)
		}
	})
	withInstance(t, legacyInstance, func() {
		for _, n := range []string{janePG, "dbcanvas-jane-app-1", "dbcanvas-app-1", "dbcanvas-iomax-17"} {
			if _, ok := stackIDFromInstanceName(n); ok {
				t.Errorf("legacy claims %q", n)
			}
		}
		if id, ok := stackIDFromInstanceName(legacyPG); !ok || id != 3 {
			t.Errorf("legacy does not recognise its own %q", legacyPG)
		}
		if _, ok := k3dStackIDFromContainer(janeK3D); ok {
			t.Errorf("legacy claims %q", janeK3D)
		}
		if got := k3dClustersOfStack([]string{"k3d-00-s3", "k3d-00-s3-jane", "k3d-00-s31"}, 3); len(got) != 1 || got[0] != "k3d-00-s3" {
			t.Errorf("legacy stack 3 clusters: %v", got)
		}
	})
}

func TestHasRuleComment(t *testing.T) {
	line := `-A DOCKER-USER -s 172.20.0.0/16 -m comment --comment dbcanvas-stack-10 -j ACCEPT`
	if hasRuleComment(line, "dbcanvas-stack-1") {
		t.Error("stack 1 matched stack 10's rule")
	}
	if !hasRuleComment(line, "dbcanvas-stack-10") {
		t.Error("stack 10 did not match its own rule")
	}
	if !hasRuleComment(`-A X -m comment --comment "dbcanvas-stack-1" -j RETURN`, "dbcanvas-stack-1") {
		t.Error("quoted comment not matched")
	}
}

// The scripts that build images and the app that deploys them must agree on the release tag:
// image_release (images/platform.sh) reads VERSION, the app is stamped with the same file.
func TestImageReleaseMatchesScripts(t *testing.T) {
	ver, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Skip("no VERSION file")
	}
	out, err := exec.Command("bash", "-c", `. ../images/platform.sh && image_release ..`).Output()
	if err != nil {
		t.Fatalf("image_release: %v", err)
	}
	old := appVersion
	appVersion = strings.TrimSpace(string(ver))
	defer func() { appVersion = old }()
	if got, want := strings.TrimSpace(string(out)), imageRelease(); got != want {
		t.Errorf("images/platform.sh tags %q, the app deploys %q", got, want)
	}
}

func TestReleaseTagRe(t *testing.T) {
	old := appVersion
	appVersion = "0.0.14"
	defer func() { appVersion = old }()
	for msg, want := range map[string]string{
		"Missing image dbcanvas-systemd:oraclelinux-9-amd64-v0.0.14 — run `make images`": "dbcanvas-systemd:oraclelinux-9-amd64",
		"Missing image dbcanvas-carsim:v0.0.14 (make carsim-image)":                      "dbcanvas-carsim:latest",
		"Missing image dbcanvas-vnc:ubuntu-24.04-arm64-v0.0.14":                          "dbcanvas-vnc:ubuntu-24.04-arm64",
	} {
		m := releaseTagRe.FindStringSubmatch(msg)
		if m == nil || m[3] != "v0.0.14" {
			t.Errorf("%q: no release tag found (%v)", msg, m)
			continue
		}
		got := m[1] + ":" + strings.TrimSuffix(m[2], "-")
		if m[2] == "" {
			got = m[1] + ":latest"
		}
		if got != want {
			t.Errorf("%q: legacy %q, want %q", msg, got, want)
		}
	}
}
