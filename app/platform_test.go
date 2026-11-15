package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An installation targets exactly one Docker platform: images/platform.sh resolves
// DOCKER_PLATFORM once and `make images` builds only that, so there is never more than
// one architecture of dbcanvas-systemd/-intranet/-vnc on disk. An unset node arch must
// therefore resolve to that platform and not to the host's own architecture — on an
// Apple Silicon host with the default DOCKER_PLATFORM=linux/amd64 the two disagree, and
// the node would be given an image tag that was never built.
func TestArchOrFollowsTheInstallationPlatform(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	if got := archOr(""); got != "arm64" {
		t.Errorf("archOr(\"\") on an arm64 installation = %q, want arm64", got)
	}
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
	if got := archOr(""); got != "amd64" {
		t.Errorf("archOr(\"\") on an amd64 installation = %q, want amd64", got)
	}
	// Unset is amd64, matching DOCKER_PLATFORM_DEFAULT and the compose fallback.
	t.Setenv("DOCKER_PLATFORM", "")
	if got := archOr(""); got != "amd64" {
		t.Errorf("archOr(\"\") with no DOCKER_PLATFORM = %q, want amd64", got)
	}
	// A design that saved an explicit architecture still gets it: the picker is gone
	// from the fixed-platform node types, but old designs carry the field.
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
	if got := archOr("arm64"); got != "arm64" {
		t.Errorf("archOr(%q) = %q — an explicitly saved arch must win", "arm64", got)
	}
	// Anything else is not an architecture and falls back rather than reaching a tag.
	if got := archOr("i386"); got != "amd64" {
		t.Errorf("archOr(%q) = %q, want the platform fallback", "i386", got)
	}
}

// Every OS node resolves its image through pxcImage/archOr, and the Valkey node had its
// own hardcoded amd64 fallback that bypassed it.
func TestNodeImagesFollowThePlatform(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	if got := pxcImage("oraclelinux", "9", ""); got != "dbcanvas-systemd:oraclelinux-9-arm64-"+imageRelease() {
		t.Errorf("pxcImage with no arch = %q", got)
	}
	if _, _, arch := valkeyNodeOS("", "", ""); arch != "arm64" {
		t.Errorf("valkeyNodeOS arch = %q, want the installation platform", arch)
	}
}

// The image tags the fixed-platform node types resolve to are built by images/service.sh
// for the one platform, so they must follow the same rule.
func TestPrebakedImagesFollowThePlatform(t *testing.T) {
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	if got := intranetImage(""); got != "dbcanvas-intranet:oraclelinux-9-arm64-"+imageRelease() {
		t.Errorf("intranetImage(\"\") = %q", got)
	}
	if got := vncImage(""); got != "dbcanvas-vnc:ubuntu-24.04-arm64-"+imageRelease() {
		t.Errorf("vncImage(\"\") = %q", got)
	}
}

// Each platform reads its own catalog, and an arm64 installation never falls back to the amd64
// one: those versions were probed on amd64 images.
func TestVersionsFileFollowsThePlatform(t *testing.T) {
	dir := t.TempDir()
	amd := filepath.Join(dir, "versions.yaml")
	arm := filepath.Join(dir, "versions-arm64.yaml")
	if err := os.WriteFile(amd, []byte("images: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", amd)
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
	if got := versionsFilePath(); got != amd {
		t.Errorf("amd64 reads %q, want %q", got, amd)
	}
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	if got := versionsFilePath(); got == amd {
		t.Error("arm64 fell back to the amd64 catalog")
	}
	if err := os.WriteFile(arm, []byte("images: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := versionsFilePath(); got != arm {
		t.Errorf("arm64 reads %q, want %q", got, arm)
	}
}

// images.yaml can hold both platforms' bases; the pickers offer only this installation's.
func TestImageListIsThisPlatformsOnly(t *testing.T) {
	imgs := []PXCImage{{OS: "oraclelinux", OSVersion: "9", Arch: "amd64"}, {OS: "oraclelinux", OSVersion: "9", Arch: "arm64"}, {OS: "ubuntu", OSVersion: "24.04"}}
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
	got := onPlatform(imgs)
	if len(got) != 2 || got[0].Arch != "arm64" || got[1].Arch != "" {
		t.Errorf("arm64 kept %+v", got)
	}
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")
	if got := onPlatform(imgs); len(got) != 2 || got[0].Arch != "amd64" {
		t.Errorf("amd64 kept %+v", got)
	}
}
