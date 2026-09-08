package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeVersionsFile points VERSIONS_FILE at a synthetic catalog, so these tests do not depend on
// what `make versions` last discovered.
func writeVersionsFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSIONS_FILE", path)
}

const testVersionsYAML = `image_prefix: dbcanvas-systemd
images:
  el9:
    tags: ["x"]
operators:
  pxc:
    repository: percona/percona-xtradb-cluster-operator
    latest: "1.20.0"
    versions:
      - "1.20.0"
      - "1.19.1"
charts:
  cloudnative-pg:
    repository: https://cloudnative-pg.github.io/charts
    latest: "0.29.0"
    versions:
      - "0.29.0"
      - "0.28.3"
  cert-manager:
    repository: https://charts.jetstack.io
    latest: "v1.21.1"
    versions:
      - "v1.21.1"
chart_images:
  cnpg-postgresql:
    repository: ghcr.io/cloudnative-pg/postgresql
    latest: "18"
    versions:
      - "18"
      - "17"
k3s:
  repository: rancher/k3s
  latest: "v1.33.13-k3s1"
`

// The sections must not bleed into one another: a chart is not an operator, and both use the
// same repository/latest/versions shape, so a parser keyed on the wrong block would silently
// mix them.
func TestVersionSectionsAreIsolated(t *testing.T) {
	writeVersionsFile(t, testVersionsYAML)

	ops := loadOperatorCatalog()
	if _, ok := ops["cloudnative-pg"]; ok {
		t.Error("operators catalog leaked a chart entry")
	}
	if got := ops["pxc"].Latest; got != "1.20.0" {
		t.Errorf("pxc latest = %q, want 1.20.0", got)
	}

	charts := loadChartCatalog()
	if _, ok := charts["pxc"]; ok {
		t.Error("charts catalog leaked an operator entry")
	}
	if got := charts["cloudnative-pg"].Latest; got != "0.29.0" {
		t.Errorf("cloudnative-pg latest = %q, want 0.29.0", got)
	}
	if got := len(charts["cloudnative-pg"].Versions); got != 2 {
		t.Errorf("cloudnative-pg versions = %d, want 2", got)
	}
	// cert-manager publishes v-prefixed chart versions; the prefix must survive, since the
	// value goes straight back into a HelmChart's version field.
	if got := charts["cert-manager"].Latest; got != "v1.21.1" {
		t.Errorf("cert-manager latest = %q, want v1.21.1 (v prefix preserved)", got)
	}

	imgs := loadChartImageCatalog()
	if got := imgs["cnpg-postgresql"].Latest; got != "18" {
		t.Errorf("cnpg-postgresql latest = %q, want 18", got)
	}
	if got := imgs["cnpg-postgresql"].Repository; got != "ghcr.io/cloudnative-pg/postgresql" {
		t.Errorf("cnpg-postgresql repository = %q", got)
	}
}

func TestResolveChartVersion(t *testing.T) {
	writeVersionsFile(t, testVersionsYAML)
	charts := loadChartCatalog()

	if v, ok := charts.resolveChartVersion("cloudnative-pg", ""); !ok || v != "0.29.0" {
		t.Errorf("blank should resolve to latest, got %q ok=%v", v, ok)
	}
	if v, ok := charts.resolveChartVersion("cloudnative-pg", "0.28.3"); !ok || v != "0.28.3" {
		t.Errorf("known version should resolve, got %q ok=%v", v, ok)
	}
	// A version the catalog knows the chart for but does not list must be refused — that is
	// what keeps a hand-edited design from pinning a chart that does not exist.
	if _, ok := charts.resolveChartVersion("cloudnative-pg", "9.9.9"); ok {
		t.Error("unknown version of a known chart should not resolve")
	}
}

// With no catalog at all (`make versions` never run, or run offline) a CNPG frame must still be
// deployable: helm resolves a blank version to the repo's latest. An absent catalog is not a
// reason to refuse, unlike an absent *operator* catalog where the version is also a git tag.
func TestResolveChartVersionWithoutCatalog(t *testing.T) {
	writeVersionsFile(t, "images:\n  el9:\n    tags: [\"x\"]\n")
	charts := loadChartCatalog()
	if len(charts) != 0 {
		t.Fatalf("expected an empty chart catalog, got %+v", charts)
	}
	if v, ok := charts.resolveChartVersion("cloudnative-pg", ""); !ok || v != "" {
		t.Errorf("blank with no catalog should pass through, got %q ok=%v", v, ok)
	}
	if v, ok := charts.resolveChartVersion("cloudnative-pg", "0.29.0"); !ok || v != "0.29.0" {
		t.Errorf("pinned with no catalog should pass through, got %q ok=%v", v, ok)
	}
}

// The K3D frame validator must not reject a CNPG frame for missing from the Percona catalog,
// and must reject a chart version the catalog does not have.
func TestK3DFrameIssuesCNPGVersion(t *testing.T) {
	writeVersionsFile(t, testVersionsYAML)
	// A client on a dead socket keeps this hermetic: k3dFrameIssues asks the engine for host
	// resources, and HostResources returns zeroes on any error, which makes it skip that check.
	a := &App{docker: NewDocker(filepath.Join(t.TempDir(), "absent.sock"))}
	ops := loadOperatorCatalog()

	ok := a.k3dFrameIssues(t.Context(), designFrame{
		Label: "k8s-01", K3DOperator: "cnpg", K3DOperatorVer: "0.29.0",
		K3DCPUs: 8, K3DMemoryGB: 12,
	}, 1, ops)
	for _, iss := range ok {
		if iss.Level == "error" {
			t.Errorf("valid CNPG frame reported an error: %s", iss.Message)
		}
	}

	bad := a.k3dFrameIssues(t.Context(), designFrame{
		Label: "k8s-01", K3DOperator: "cnpg", K3DOperatorVer: "9.9.9",
		K3DCPUs: 8, K3DMemoryGB: 12,
	}, 1, ops)
	found := false
	for _, iss := range bad {
		if iss.Level == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown CNPG chart version should be an error, got %+v", bad)
	}
}
