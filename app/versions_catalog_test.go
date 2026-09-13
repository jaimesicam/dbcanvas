package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A Debian base image is built for the Linux Client alone (see productOSFamily): the
// generic catalog its picker reads must list it, and every per-product catalog must
// not — a product picker that offered Debian would advertise an install path nothing
// exercises.
const debianCatalogYAML = `image_prefix: dbcanvas-systemd
images:
  - os: ubuntu
    version: "24.04"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:ubuntu-24.04-amd64
    percona_server:
      "8.0":
        - 8.0.46-37.1
  - os: debian
    version: "13"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:debian-13-amd64
    percona_server:
      "8.0":
        - 8.0.46-37.1
`

func TestDebianImageIsGenericOnly(t *testing.T) {
	writeVersionsFile(t, debianCatalogYAML)

	generic := loadImagesCatalog()
	if len(generic) != 2 {
		t.Fatalf("generic catalog: got %d images, want 2 (%v)", len(generic), generic)
	}
	if !hasOS(generic, "debian") {
		t.Error("generic catalog dropped the Debian image — the Linux Client picker reads it")
	}

	ps := loadPSCatalog()
	if hasOS(ps, "debian") {
		t.Error("percona_server catalog offers Debian — no product install path is exercised on it")
	}
	if !hasOS(ps, "ubuntu") {
		t.Error("percona_server catalog lost the Ubuntu image")
	}
}

func hasOS(imgs []PXCImage, os string) bool {
	for _, i := range imgs {
		if i.OS == os {
			return true
		}
	}
	return false
}

// The two catalogs are two files on purpose: `make images` writes images.yaml (what was
// built) and `make versions` writes versions.yaml (what installs on it), so rebuilding an
// image no longer discards hours of probing. What that has to mean at runtime is that the
// OS matrix comes from images.yaml and the version maps from versions.yaml — including
// when the two disagree, which is the normal state between a build and the next probe.
func TestImageMatrixComesFromImagesFileAndVersionsFromVersionsFile(t *testing.T) {
	dir := t.TempDir()
	// images.yaml knows about an Oracle Linux 10 image the last probe never saw.
	imagesYAML := `image_prefix: dbcanvas-systemd
images:
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:oraclelinux-9-amd64
    base: oraclelinux:9
    built_at: 2026-09-03T01:54:49Z
  - os: oraclelinux
    version: "10"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:oraclelinux-10-amd64
    base: oraclelinux:10
    built_at: 2026-09-03T01:56:06Z
`
	versionsYAML := `images:
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    arch: amd64
    tag: dbcanvas-systemd:oraclelinux-9-amd64
    percona_server:
      "8.4":
        - 8.4.11-11.1
`
	writeFile(t, dir, "images.yaml", imagesYAML)
	writeFile(t, dir, "versions.yaml", versionsYAML)
	t.Setenv("IMAGES_FILE", filepath.Join(dir, "images.yaml"))
	t.Setenv("VERSIONS_FILE", filepath.Join(dir, "versions.yaml"))

	// The OS picker offers the freshly built image right away — it does not wait for
	// `make versions`, which is the whole point of no longer running it at install time.
	generic := loadImagesCatalog()
	if len(generic) != 2 {
		t.Fatalf("generic catalog: got %d images, want the 2 in images.yaml (%v)", len(generic), generic)
	}
	if generic[1].OSVersion != "10" {
		t.Errorf("generic catalog missed the image only images.yaml knows: %+v", generic)
	}

	// The version maps still come from versions.yaml, so the un-probed image simply has
	// no Percona Server versions to offer rather than inventing some.
	ps := loadPSCatalog()
	if len(ps) != 1 || ps[0].OSVersion != "9" {
		t.Fatalf("percona_server catalog = %+v, want only the probed Oracle Linux 9", ps)
	}
	if got := ps[0].Versions["8.4"]; len(got) != 1 || got[0] != "8.4.11-11.1" {
		t.Errorf("percona_server 8.4 = %v", got)
	}
}

// An installation that predates the split has no images.yaml at all; its image entries are
// in versions.yaml, where they have always been. The OS pickers must keep working until the
// next `make images` writes the new file.
func TestImagesFileFallsBackToVersionsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "versions.yaml", debianCatalogYAML)
	t.Setenv("VERSIONS_FILE", filepath.Join(dir, "versions.yaml"))
	t.Setenv("IMAGES_FILE", "")
	// Run from a directory with no images.yaml in it or above it, so the relative
	// candidates imagesFilePath tries cannot reach the repo's own copy.
	t.Chdir(filepath.Join(dir, "sub"))

	if got := len(loadImagesCatalog()); got != 2 {
		t.Fatalf("generic catalog with no images.yaml: got %d images, want 2 from versions.yaml", got)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// `make env` drops an `images: []` stand-in when images.yaml is missing, so that Docker
// cannot create a directory at the bind-mount source. That stand-in must not empty the OS
// pickers of an installation whose versions.yaml still describes images.
func TestEmptyImagesFileFallsBackToVersionsFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "images.yaml", "images: []\n")
	writeFile(t, dir, "versions.yaml", debianCatalogYAML)
	t.Setenv("IMAGES_FILE", filepath.Join(dir, "images.yaml"))
	t.Setenv("VERSIONS_FILE", filepath.Join(dir, "versions.yaml"))

	if got := len(loadImagesCatalog()); got != 2 {
		t.Fatalf("generic catalog with an empty images.yaml: got %d images, want 2 from versions.yaml", got)
	}
}
