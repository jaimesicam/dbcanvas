package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Each architecture has its own catalogue: what amd64 can install is in versions.yaml, what
// arm64 can install in versions-arm64.yaml, and a node picks from its own.
const archAMDYAML = `images:
  - os: oraclelinux
    version: "9"
    platform: linux/amd64
    percona_server:
      "8.0":
        - 8.0.46-37.1
`
const archARMYAML = `images:
  - os: oraclelinux
    version: "9"
    platform: linux/arm64
    percona_server:
      "8.4":
        - 8.4.6-6.1
      "8.0":
        - 8.0.45-36.1
`

// archCatalogs writes both catalogues side by side, as VERSIONS_FILE names them.
func archCatalogs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	amd := filepath.Join(dir, "versions.yaml")
	if err := os.WriteFile(amd, []byte(archAMDYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versions-arm64.yaml"), []byte(archARMYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", amd)
	t.Setenv("IMAGES_FILE", filepath.Join(dir, "absent.yaml"))
	t.Chdir(dir)
	t.Setenv("DOCKER_PLATFORM", "linux/arm64")
}

func TestCatalogHoldsBothArchitectures(t *testing.T) {
	archCatalogs(t)
	ps := loadPSCatalog()
	if len(ps) != 2 {
		t.Fatalf("want an entry per architecture, got %+v", ps)
	}
	for _, im := range ps {
		if im.Arch != "amd64" && im.Arch != "arm64" {
			t.Errorf("an entry without its architecture: %+v", im)
		}
	}
	if got := forArch(ps, "amd64"); len(got) != 1 || len(got[0].Versions["8.4"]) != 0 {
		t.Errorf("amd64: %+v", got)
	}
	// "" is the default platform, arm64 here.
	if got := forArch(ps, ""); len(got) != 1 || got[0].Arch != "arm64" {
		t.Errorf("default: %+v", got)
	}
	if imgs := loadImagesCatalog(); len(imgs) != 2 {
		t.Errorf("the image list should hold both architectures' images: %+v", imgs)
	}
}

// Compose and validation resolve a node's version in its own architecture's catalogue.
func TestVersionsResolvePerArchitecture(t *testing.T) {
	archCatalogs(t)
	ps := loadPSCatalog()
	// A series resolves to itself, with the newest of it chosen at deploy.
	if major, _, err := resolveCatalogVersion(ps, "oraclelinux", "9", "arm64", "8.4"); err != nil || major != "8.4" {
		t.Errorf("arm64 8.4: %q %v", major, err)
	}
	if _, _, err := resolveCatalogVersion(ps, "oraclelinux", "9", "amd64", "8.4"); err == nil {
		t.Error("amd64 has no 8.4 but resolved one")
	}
	// Automatic is the default platform's catalogue.
	if _, minor, err := resolveCatalogVersion(ps, "oraclelinux", "9", "", "8.0.45"); err != nil || minor != "8.0.45-36.1" {
		t.Errorf("automatic 8.0.45: %q %v", minor, err)
	}
	if _, _, err := resolveCatalogVersion(ps, "oraclelinux", "9", "amd64", "8.0.45"); err == nil {
		t.Error("amd64 has no 8.0.45 but resolved it")
	}
	gate := func(arch string) string {
		return emptyCatalogReason(ps, dbVersionSpec{Section: "percona_server", Product: "Percona Server", Major: "8.4", OS: "oraclelinux", OSVersion: "9", Arch: arch})
	}
	if why := gate("amd64"); why == "" {
		t.Error("the version check let an amd64 node have 8.4")
	}
	if why := gate("arm64"); why != "" {
		t.Errorf("arm64 8.4 refused: %s", why)
	}
}

// The catalogue routes answer for the architecture asked for, the default platform's otherwise.
func TestCatalogRoutesTakeArch(t *testing.T) {
	archCatalogs(t)
	app := newTestApp(t)
	u, _ := app.ensureOwner()
	get := func(q string) (out struct {
		Arch   string     `json:"arch"`
		Images []PXCImage `json:"images"`
	}) {
		r, _ := signedIn(t, app, u, "")
		r.Method, r.URL.RawQuery = "GET", q
		w := httptest.NewRecorder()
		app.handlePSCatalog(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if c := get("arch=amd64"); c.Arch != "amd64" || len(c.Images) != 1 || c.Images[0].Arch != "amd64" {
		t.Errorf("?arch=amd64: %+v", c)
	}
	if c := get(""); c.Arch != "arm64" || len(c.Images) != 1 || c.Images[0].Arch != "arm64" {
		t.Errorf("default: %+v", c)
	}
}

// images.yaml lists what this machine built, for one architecture; the other's images come
// from its catalogue.
func TestImageListFillsTheOtherArchitecture(t *testing.T) {
	archCatalogs(t)
	built := filepath.Join(t.TempDir(), "images.yaml")
	os.WriteFile(built, []byte("images:\n  - os: oraclelinux\n    version: \"10\"\n    arch: arm64\n"), 0o644)
	t.Setenv("IMAGES_FILE", built)
	imgs := loadImagesCatalog()
	var arm, amd []string
	for _, im := range imgs {
		if im.Arch == "arm64" {
			arm = append(arm, im.OSVersion)
		} else {
			amd = append(amd, im.OSVersion)
		}
	}
	if len(arm) != 1 || arm[0] != "10" || len(amd) != 1 || amd[0] != "9" {
		t.Errorf("arm64 from images.yaml %v, amd64 from versions.yaml %v", arm, amd)
	}
}
