package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"testing"
)

// The update check reads release notes out of whatsnew.go's source on main. If that
// file ever grows something parseReleaseNotes does not understand, this fails here
// first, rather than on every installation's next check.
func TestParseReleaseNotesMatchesCompiledNotes(t *testing.T) {
	src, err := os.ReadFile("whatsnew.go")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseReleaseNotes(src)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, whatsNewNotes) {
		t.Fatalf("parsed %d notes, which differ from the %d compiled ones", len(got), len(whatsNewNotes))
	}
}

func TestCheckForUpdates(t *testing.T) {
	src := []byte(`package main
var whatsNewNotes = []releaseNote{
	{Version: "0.0.12", Date: "d", Title: "unreleased", Body: "x"},
	{Version: "0.0.11", Date: "d", Title: "eleven", Body: "a " + ` + "`b`" + `, Doc: "docs/X.md"},
	{Version: "0.0.10", Date: "d", Title: "ten", Body: "t"},
	{Version: "0.0.9", Date: "d", Title: "nine", Body: "n"},
}
`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/latest":
			w.Write([]byte(`{"tag_name":"v0.0.11","html_url":"https://example.com/r","assets":[` +
				`{"name":"dbcanvas-linux-amd64","browser_download_url":"https://example.com/l"},` +
				`{"name":"dbcanvas-` + runtime.GOOS + `-` + runtime.GOARCH + `.tar.gz","browser_download_url":"https://example.com/mine"}]}`))
		case "/raw/v0.0.11/app/whatsnew.go":
			w.Write(src)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI, oldRaw := updateAPIBase, updateRawBase
	updateAPIBase, updateRawBase = srv.URL+"/api", srv.URL+"/raw"
	defer func() { updateAPIBase, updateRawBase = oldAPI, oldRaw }()

	res, err := checkForUpdates(context.Background(), "0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Available || res.Latest != "0.0.11" {
		t.Fatalf("want 0.0.11 available, got %+v", res)
	}
	if res.ReleaseURL != "https://example.com/r" || res.Download != "https://example.com/mine" {
		t.Fatalf("release links: %+v", res)
	}
	var titles []string
	for _, n := range res.Notes {
		titles = append(titles, n.Title)
	}
	if !reflect.DeepEqual(titles, []string{"eleven", "ten"}) {
		t.Fatalf("notes = %v", titles)
	}
	if res.Notes[0].Body != "a b" || res.Notes[0].Doc != "docs/X.md" {
		t.Fatalf("note = %+v", res.Notes[0])
	}

	if res, _ = checkForUpdates(context.Background(), "0.0.11"); res.Available || len(res.Notes) != 0 {
		t.Fatalf("up to date reported as %+v", res)
	}
	if res, _ = checkForUpdates(context.Background(), devVersion); res.Available || !res.Dev || res.Latest != "0.0.11" {
		t.Fatalf("dev build reported as %+v", res)
	}
}

// The release lists each binary's signature beside it; the update dialog must link the binary.
func TestReleaseAssetIsTheBinaryNotItsSignature(t *testing.T) {
	var rel latestRelease
	json.Unmarshal([]byte(`{"assets":[`+
		`{"name":"dbcanvas-linux-amd64.sigstore.json","browser_download_url":"https://example.com/sig"},`+
		`{"name":"dbcanvas-checksums.txt","browser_download_url":"https://example.com/sum"},`+
		`{"name":"dbcanvas-linux-amd64","browser_download_url":"https://example.com/bin"}]}`), &rel)
	if name, url := releaseAsset(rel, "linux", "amd64"); name != "dbcanvas-linux-amd64" || url != "https://example.com/bin" {
		t.Errorf("got %q %q, want the binary", name, url)
	}
	if name, _ := releaseAsset(rel, "darwin", "arm64"); name != "" {
		t.Errorf("a missing platform resolved to %q", name)
	}
}
