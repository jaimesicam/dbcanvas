package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A catalogue served from a stand-in for the repository: kept when newer, left alone when
// not, refused when it is not a catalogue; reset removes it.
func TestVersionsUpdate(t *testing.T) {
	repo, err := os.ReadFile("../versions.yaml")
	if err != nil {
		t.Skip("needs the repository's versions.yaml")
	}
	stamp := func(at time.Time) string {
		lines := strings.Split(string(repo), "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, "versions_generated_at:") {
				lines[i] = "versions_generated_at: " + at.UTC().Format(time.RFC3339)
			}
		}
		return strings.Join(lines, "\n")
	}
	serve := stamp(time.Now().Add(24 * time.Hour)) // newer than anything built in
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(serve))
	}))
	defer srv.Close()
	dir := t.TempDir()
	t.Setenv("DBCANVAS_DATA", dir)
	t.Setenv("DBCANVAS_CATALOG_URL", srv.URL)
	t.Chdir(t.TempDir()) // no catalogue in the working directory
	t.Setenv("VERSIONS_FILE", "")
	t.Setenv("DOCKER_PLATFORM", "linux/amd64")

	if err := updateCatalog(); err != nil {
		t.Fatal(err)
	}
	for _, n := range catalogNames {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s not kept: %v", n, err)
		}
	}
	if p := versionsFilePath(); p != filepath.Join(dir, "versions.yaml") {
		t.Fatalf("reading %s, want the downloaded catalogue", p)
	}
	kept, _ := os.ReadFile(filepath.Join(dir, "versions.yaml"))

	// An older catalogue is not kept.
	serve = stamp(time.Now().Add(-365 * 24 * time.Hour))
	if err := updateCatalog(); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "versions.yaml")); string(now) != string(kept) {
		t.Fatal("an older catalogue replaced a newer one")
	}

	// Something that is not a catalogue is refused, and nothing changes.
	serve = "<html>not found</html>"
	if err := updateCatalog(); err == nil || !strings.Contains(err.Error(), "not a version catalogue") {
		t.Fatalf("garbage: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "versions.yaml")); string(now) != string(kept) {
		t.Fatal("a failed update changed the catalogue")
	}

	if err := cliVersions([]string{"reset"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "versions.yaml")); !os.IsNotExist(err) {
		t.Fatal("reset left the download")
	}
}
