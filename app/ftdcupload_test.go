package main

// ftdcupload_test.go — the upload endpoint, exercised the way the browser exercises it.
//
// The bug this covers was not in the parser: raw metrics.<timestamp> files always decoded.
// It was that the picker's accept list could never offer them, so nobody got as far as the
// parser with anything but a .tar.gz. The picker is the browser's, but this pins the half
// that has to hold once the files do arrive — a whole directory posted one part per file,
// under the names mongod actually writes.

import (
	"archive/zip"
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

// ftdcPostBlob posts one in-memory file under the given name, the way a reader who picked
// a single archive out of a ticket sends it.
func ftdcPostBlob(t *testing.T, app *App, cookie *http.Cookie, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("files", name)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	w.Write(data)
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/ftdc/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handleFTDCUpload(rec, r)
	return rec
}

// ftdcZipOf zips a directory's files under the given prefix, the way "Compress" on a Mac or
// "Send to → Compressed folder" on Windows produces one: paths inside, not bare names.
func ftdcZipOf(t *testing.T, dir, prefix string, extra map[string][]byte) []byte {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no %s: %v", dir, err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		w, err := zw.Create(prefix + e.Name())
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		w.Write(data)
	}
	for name, data := range extra {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestFTDCUploadZip — a zip of diagnostic.data decodes like the tar.gz does. A zip is what
// somebody on Windows or a Mac produces without installing anything, so refusing it meant
// refusing the archive most readers can actually make.
func TestFTDCUploadZip(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	raw := ftdcZipOf(t, ftdcTestDir, "diagnostic.data/", nil)
	rec := ftdcPostBlob(t, app, cookie, "diagnostic.data.zip", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for a zip of diagnostic.data, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if n, _ := got["samples"].(float64); n <= 0 {
		t.Fatalf("decoded model holds no samples: %s", rec.Body.String())
	}
	// The archive is only a container: the same directory posted file by file has to
	// produce the same model, or unpacking lost or reordered something.
	ents, err := os.ReadDir(ftdcTestDir)
	if err != nil {
		t.Skipf("no %s: %v", ftdcTestDir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	loose := ftdcPost(t, app, cookie, ftdcTestDir, names)
	if loose.Code != http.StatusOK {
		t.Fatalf("the same files posted loose failed: %d %s", loose.Code, loose.Body.String())
	}
	if loose.Body.String() != rec.Body.String() {
		t.Error("the zip decodes to a different model than the same files posted one by one")
	}
}

// TestFTDCUploadArchiveNameIsIgnored — the archive is recognised from its bytes, so the
// names archives actually arrive under work: a timestamp appended by whoever collected it,
// or no extension at all. This is the whole reason the picker has no `accept` list.
func TestFTDCUploadArchiveNameIsIgnored(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	raw := ftdcZipOf(t, ftdcTestDir, "diagnostic.data/", nil)
	for _, name := range []string{"ftdc.zip.20260814", "case-00123-diagnostic-data", "FTDC.ZIP"} {
		rec := ftdcPostBlob(t, app, cookie, name, raw)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: want 200 whatever the file is called, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestFTDCZipSkipsWhatIsNotAMetricsFile — a zip somebody made of the whole dbPath, plus the
// resource forks a Mac's zip carries. Neither is a metrics file, and a collection file is
// not something to read into memory to find that out.
func TestFTDCZipSkipsWhatIsNotAMetricsFile(t *testing.T) {
	raw := ftdcZipOf(t, ftdcTestDir, "diagnostic.data/", map[string][]byte{
		"collection-7-1234.wt":                   bytes.Repeat([]byte("x"), 4096),
		"__MACOSX/diagnostic.data/._metrics.foo": {0, 5, 22},
		"diagnostic.data/":                       nil,
	})
	files, err := ftdcFromZip(raw)
	if err != nil {
		t.Fatalf("ftdcFromZip: %v", err)
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Name, "metrics.") {
			t.Errorf("kept %q, which is not a metrics file", f.Name)
		}
		if strings.Contains(f.Name, "/") {
			t.Errorf("kept the archive path %q instead of the base name", f.Name)
		}
	}
	if len(files) == 0 {
		t.Fatal("every metrics file was dropped")
	}
}

// TestFTDCZipWithoutMetricsFilesSaysSo — one directory too high is the common mistake, and
// the message has to name what a diagnostic.data directory looks like.
func TestFTDCZipWithoutMetricsFilesSaysSo(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("dbpath/mongod.lock")
	w.Write([]byte("1234"))
	zw.Close()
	_, err := ftdcFromZip(buf.Bytes())
	if err == nil || !strings.Contains(err.Error(), "metrics.") {
		t.Fatalf("want an error naming metrics.<timestamp>, got %v", err)
	}
}

// ---------------------------------------------------------------- compare

// The comparison endpoint is the one that reads several members at once. Its two guards are
// worth a test even though the read itself needs containers: fewer than two members is not
// a comparison, and more than four is a thicket of lines whatever the palette does.
func TestFTDCCompareGuards(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/ftdc/compare", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.handleFTDCCompare(rec, r)
		return rec
	}
	if rec := post(`{"targets":[{"stackId":1,"nodeId":"a"}]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("one member: %d %s", rec.Code, rec.Body.String())
	}
	many := `{"targets":[{"stackId":1,"nodeId":"a"},{"stackId":1,"nodeId":"b"},{"stackId":1,"nodeId":"c"},{"stackId":1,"nodeId":"d"},{"stackId":1,"nodeId":"e"}]}`
	if rec := post(many); rec.Code != http.StatusBadRequest {
		t.Errorf("five members: %d %s", rec.Code, rec.Body.String())
	}
	// Two members that do not resolve still answer 200 with a per-member error, because a
	// comparison where one node is unreachable should show the ones that are.
	rec := post(`{"targets":[{"stackId":9999,"nodeId":"a"},{"stackId":9999,"nodeId":"b"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unresolvable members: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Members []struct {
			Error string `json:"error"`
		} `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 2 || got.Members[0].Error == "" {
		t.Errorf("want a per-member error, got %+v", got.Members)
	}
}

// A zoom window is read off the query string, and both ends are optional.
func TestFTDCRangeParsing(t *testing.T) {
	for _, tc := range []struct {
		url        string
		wantOK     bool
		from, upTo float64
	}{
		{"/x", false, 0, 0},
		{"/x?from=100&to=200", true, 100, 200},
		{"/x?from=100", true, 100, 0}, // to the end of the capture
		{"/x?to=200", true, 0, 200},   // from the beginning
		{"/x?from=abc", false, 0, 0},  // nonsense is not a window
	} {
		r := httptest.NewRequest(http.MethodPost, tc.url, nil)
		from, to, ok := ftdcRange(r)
		if ok != tc.wantOK {
			t.Errorf("%s: ok=%v", tc.url, ok)
			continue
		}
		if !ok {
			continue
		}
		if from != tc.from {
			t.Errorf("%s: from=%v want %v", tc.url, from, tc.from)
		}
		if tc.upTo > 0 && to != tc.upTo {
			t.Errorf("%s: to=%v want %v", tc.url, to, tc.upTo)
		}
		if tc.upTo == 0 && to < 1e9 {
			t.Errorf("%s: an open end became %v", tc.url, to)
		}
	}
}
