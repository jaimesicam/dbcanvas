package main

import "testing"

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
