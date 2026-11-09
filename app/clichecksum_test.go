package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIChecksums(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("ann", "x", RoleUser, StatusApproved)
	secret, _ := tokenFor(t, app, u.ID, ScopeRead, 30)
	dir := t.TempDir()
	t.Setenv("CLI_DIR", dir)
	body := []byte("not really a binary")
	os.WriteFile(filepath.Join(dir, "dbcanvas-cli_linux_amd64"), body, 0o755)
	want := sha256.Sum256(body)

	rt := apiRoute{Method: "GET", Path: "/api/cli/checksums"}
	w := httptest.NewRecorder()
	app.requireScope(rt, app.handleCLIChecksums)(w, bearerReq("GET", "/api/cli/checksums", secret))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != hex.EncodeToString(want[:])+"  dbcanvas-cli_linux_amd64" {
		t.Fatalf("checksums: %d %q", w.Code, w.Body.String())
	}

	rt = apiRoute{Method: "GET", Path: "/api/cli/download"}
	w = httptest.NewRecorder()
	app.requireScope(rt, app.handleCLIDownload)(w, bearerReq("GET", "/api/cli/download?os=linux&arch=amd64", secret))
	if w.Code != 200 || w.Header().Get("X-Checksum-Sha256") != hex.EncodeToString(want[:]) || w.Body.String() != string(body) {
		t.Fatalf("download: %d %q %q", w.Code, w.Header().Get("X-Checksum-Sha256"), w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Digest"), "sha-256=") {
		t.Fatalf("no Digest header: %v", w.Header())
	}
}
