package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLicenses(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"LICENSE": "GNU GENERAL PUBLIC LICENSE", "THIRD_PARTY_LICENSES-web.txt": "react MIT"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DBCANVAS_LICENSE_DIR", dir)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /licenses/", handleLicenses)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	idx := get("/licenses/")
	if idx.Code != 200 || !strings.Contains(idx.Body.String(), "without any warranty") || !strings.Contains(idx.Body.String(), sourceRepo) {
		t.Fatalf("index: %d %q", idx.Code, idx.Body.String())
	}
	if i, j := strings.Index(idx.Body.String(), `"/licenses/LICENSE"`), strings.Index(idx.Body.String(), `"/licenses/THIRD_PARTY`); i < 0 || j < 0 || i > j {
		t.Errorf("LICENSE should be listed first: %q", idx.Body.String())
	}
	f := get("/licenses/LICENSE")
	if f.Code != 200 || f.Body.String() != "GNU GENERAL PUBLIC LICENSE" || !strings.HasPrefix(f.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("LICENSE: %d %q %q", f.Code, f.Header().Get("Content-Type"), f.Body.String())
	}
	for _, p := range []string{"/licenses/missing.txt", "/licenses/..%2fetc%2fpasswd", "/licenses/.hidden"} {
		if c := get(p).Code; c != 404 {
			t.Errorf("%s: got %d, want 404", p, c)
		}
	}

	// A server with no texts (a development build) still gives the notice.
	t.Setenv("DBCANVAS_LICENSE_DIR", filepath.Join(dir, "none"))
	if b := get("/licenses/").Body.String(); !strings.Contains(b, "GNU General Public License") || !strings.Contains(b, "THIRD_PARTY_NOTICES.md") {
		t.Errorf("empty index: %q", b)
	}
}
