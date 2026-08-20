package main

import "testing"

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
	if got := pxcImage("oraclelinux", "9", ""); got != "dbcanvas-systemd:oraclelinux-9-arm64" {
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
	if got := intranetImage(""); got != "dbcanvas-intranet:oraclelinux-9-arm64" {
		t.Errorf("intranetImage(\"\") = %q", got)
	}
	if got := vncImage(""); got != "dbcanvas-vnc:ubuntu-24.04-arm64" {
		t.Errorf("vncImage(\"\") = %q", got)
	}
}
