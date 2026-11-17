package main

import (
	"strings"
	"testing"
)

// With no catalogue file anywhere, the copy built into the binary is read. Needs
// `make catalog` (the copies are not in git); skipped without it.
func TestEmbeddedCatalogIsReadWithoutAFile(t *testing.T) {
	if embeddedCatalog("versions.yaml") == "" {
		t.Skip("no embedded catalogue: run `make catalog`")
	}
	t.Chdir(t.TempDir())
	t.Setenv("VERSIONS_FILE", "")
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		t.Setenv("DOCKER_PLATFORM", platform)
		path := versionsFilePath()
		if !strings.HasPrefix(path, embeddedPrefix) {
			t.Skipf("%s: a catalogue file exists at %s on this machine", platform, path)
		}
		if ps := loadPSCatalog(); len(ps) == 0 {
			t.Errorf("%s: no Percona Server versions from %s", platform, path)
		}
		if ops := loadOperatorCatalog(); len(ops) == 0 {
			t.Errorf("%s: no operators from %s", platform, path)
		}
		if _, ok := versionsGeneratedAt(); !ok {
			t.Errorf("%s: no generation time from %s", platform, path)
		}
	}
}
