package main

// ftdcupload_test.go — the upload endpoint, exercised the way the browser exercises it.
//
// The bug this covers was not in the parser: raw metrics.<timestamp> files always decoded.
// It was that the picker's accept list could never offer them, so nobody got as far as the
// parser with anything but a .tar.gz. The picker is the browser's, but this pins the half
// that has to hold once the files do arrive — a whole directory posted one part per file,
// under the names mongod actually writes.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ftdcTestDir is a real diagnostic.data directory, copied off a running mongod.
const ftdcTestDir = "testdata/diagnostic.data"

// ftdcAuthed returns an App and a request cookie for a logged-in user.
func ftdcAuthed(t *testing.T) (*App, *http.Cookie) {
	t.Helper()
	app := newTestApp(t)
	u, err := app.store.CreateUser("alice", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok := "ftdc-test-session"
	if err := app.store.CreateSession(tok, u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return app, &http.Cookie{Name: cookieName, Value: tok}
}

// ftdcPost posts the named files from dir as one multipart form, one part each.
func ftdcPost(t *testing.T, app *App, cookie *http.Cookie, dir string, names []string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		w, err := mw.CreateFormFile("files", n)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		w.Write(data)
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/ftdc/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handleFTDCUpload(rec, r)
	return rec
}

// TestFTDCUploadRawMetricsFiles posts a whole diagnostic.data directory the way a folder
// pick sends it: every file by its own name, none of them an archive.
func TestFTDCUploadRawMetricsFiles(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	ents, err := os.ReadDir(ftdcTestDir)
	if err != nil {
		t.Skipf("no %s: %v", ftdcTestDir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) < 2 {
		t.Fatalf("want a directory of several files, got %v", names)
	}

	rec := ftdcPost(t, app, cookie, ftdcTestDir, names)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %v: status %d, body %s", names, rec.Code, rec.Body.String())
	}
	var got fdModel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if got.Samples == 0 || got.From == 0 || got.To <= got.From {
		t.Fatalf("want a spanning series, got samples=%d from=%v to=%v", got.Samples, got.From, got.To)
	}
	if len(got.Charts) == 0 {
		t.Fatal("want charts from a real capture, got none")
	}
}

// TestFTDCUploadOneMetricsFile is the single-file case — one metrics.<timestamp> chosen on
// its own, which is what somebody does when they know which hour they want. It uses a
// captured file rather than the young directory above, for the reason the next test states.
func TestFTDCUploadOneMetricsFile(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	const one = "metrics.rs-mongo03"
	rec := ftdcPost(t, app, cookie, "testdata/ftdc", []string{one})
	if rec.Code != http.StatusOK {
		t.Fatalf("upload %s: status %d, body %s", one, rec.Code, rec.Body.String())
	}
	var got fdModel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if got.Samples == 0 {
		t.Fatalf("want samples from %s, got none", one)
	}
	if len(got.Charts) == 0 {
		t.Fatalf("want charts from %s, got none", one)
	}
}

// TestFTDCUploadYoungNumberedFileHasNoSamples pins a property of FTDC that reads as a bug
// when it is met without warning.
//
// Samples accumulate in metrics.interim and are flushed into the numbered file in batches,
// so a mongod that started minutes ago has a numbered file holding the metadata document
// and no chunks at all. Uploading that file on its own is answered with "no metric samples"
// — which is the truth about the file, not a failure to read it. Whoever changes the parser
// next should know this case exists before deciding the parser is at fault.
func TestFTDCUploadYoungNumberedFileHasNoSamples(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	const young = "metrics.2026-08-16T05-21-36Z-00000"
	rec := ftdcPost(t, app, cookie, ftdcTestDir, []string{young})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a numbered file written before the first flush, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no metric samples") {
		t.Fatalf("want the message to name what is missing, got %s", rec.Body.String())
	}
	// …and the same file alongside the interim it belongs with is a working capture, which
	// is what makes the whole directory the unit worth uploading.
	rec = ftdcPost(t, app, cookie, ftdcTestDir, []string{young, "metrics.interim"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want the pair to decode, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestFTDCUploadNeedsAuth — the endpoint takes files, so it checks the cookie first.
func TestFTDCUploadNeedsAuth(t *testing.T) {
	app := newTestApp(t)
	r := httptest.NewRequest(http.MethodPost, "/api/ftdc/upload", bytes.NewReader(nil))
	rec := httptest.NewRecorder()
	app.handleFTDCUpload(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a session, got %d", rec.Code)
	}
}
