package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every lab ships its starting design as an embedded json.RawMessage literal. They are
// package-level vars with no registry to range over, so this parses them out of the
// sources — the same trick TestMySQLStatusPollsAvoidBackslashG uses.
//
// It exists because the designs were edited in bulk to drop their pinned "arch" fields
// (they hardcoded amd64, which an arm64 installation never builds), and a stray comma
// would have shipped a lab that fails to open rather than one that fails a test.
var labDesignRE = regexp.MustCompile("(?s)json\\.RawMessage\\(`(.*?)`\\)")

func TestLabDesignsAreValidJSON(t *testing.T) {
	files, err := filepath.Glob("labs*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no lab sources found: %v", err)
	}
	found := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range labDesignRE.FindAllStringSubmatch(string(src), -1) {
			body := m[1]
			if !strings.Contains(body, "\"nodes\"") && !strings.Contains(body, "\"frames\"") {
				continue // not a design literal
			}
			found++
			var doc designDoc
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Errorf("%s: a lab design does not parse: %v", f, err)
				continue
			}
			if len(doc.Nodes) == 0 && len(doc.Frames) == 0 {
				t.Errorf("%s: a lab design has neither nodes nor frames", f)
			}
			// No design may pin an architecture: the installation targets one platform
			// and the server resolves it (archOr). A pinned amd64 is an image an arm64
			// installation never built.
			if strings.Contains(body, "\"arch\"") {
				t.Errorf("%s: a lab design pins an architecture", f)
			}
		}
	}
	if found == 0 {
		t.Fatal("no lab designs were found to check — the regexp has gone stale")
	}
	t.Logf("checked %d lab designs", found)
}
