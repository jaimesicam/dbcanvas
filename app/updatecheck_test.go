package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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
		case "/VERSION":
			w.Write([]byte("0.0.11\n"))
		case "/app/whatsnew.go":
			w.Write(src)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := updateRawBase
	updateRawBase = srv.URL
	defer func() { updateRawBase = old }()

	res, err := checkForUpdates(context.Background(), "0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Available || res.Latest != "0.0.11" {
		t.Fatalf("want 0.0.11 available, got %+v", res)
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
