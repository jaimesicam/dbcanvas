package main

// nodeupload_test.go — the tar a drop turns into.
//
// The handler's own half is thin (auth, the destination whitelist, one
// PutArchive); what has to hold is the archive built from the multipart form:
// the field name is the path, a dropped folder keeps its shape, and nothing in
// the form can name a place outside the destination the user picked.

import (
	"archive/tar"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// uploadForm builds a parsed multipart form the way the browser posts one:
// field name = path relative to the destination.
func uploadForm(t *testing.T, files map[string]string) *multipart.Form {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for path, content := range files {
		w, err := mw.CreateFormFile(path, path)
		if err != nil {
			t.Fatalf("form file %s: %v", path, err)
		}
		w.Write([]byte(content))
	}
	mw.Close()
	mr := multipart.NewReader(&body, mw.Boundary())
	form, err := mr.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	t.Cleanup(func() { form.RemoveAll() })
	return form
}

// buildUploadTar plans and writes a drop in one step, the way the handler does.
// max defaults to the shipped ceiling unless a test cares about the limit.
func buildUploadTar(form *multipart.Form) ([]byte, []string, int64, error) {
	return buildUploadTarMax(form, defaultMaxUploadBytes)
}

func buildUploadTarMax(form *multipart.Form, max int64) ([]byte, []string, int64, error) {
	p, err := planUpload(form, max)
	if err != nil {
		return nil, nil, p.total, err
	}
	var buf bytes.Buffer
	if err := writeUploadTar(&buf, p); err != nil {
		return nil, nil, p.total, err
	}
	return buf.Bytes(), p.names, p.total, nil
}

// readTar returns the archive as name → content, with directory entries kept as
// their trailing-slash names so their presence can be asserted.
func readTar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			out[hdr.Name] = ""
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(data)
	}
	return out
}

// TestBuildUploadTarFlatFiles is the common drop: a handful of loose files.
func TestBuildUploadTarFlatFiles(t *testing.T) {
	form := uploadForm(t, map[string]string{
		"my.cnf":    "[mysqld]\n",
		"dump.sql":  "SELECT 1;\n",
		"notes.txt": "",
	})
	tarball, names, total, err := buildUploadTar(form)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if want := int64(len("[mysqld]\n") + len("SELECT 1;\n")); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	// Names come back sorted so the response reads the same way every time.
	if got := strings.Join(names, ","); got != "dump.sql,my.cnf,notes.txt" {
		t.Errorf("names = %q", got)
	}
	got := readTar(t, tarball)
	if got["my.cnf"] != "[mysqld]\n" || got["dump.sql"] != "SELECT 1;\n" {
		t.Errorf("tar contents = %#v", got)
	}
	// A zero-byte file is still a file the user dropped.
	if _, ok := got["notes.txt"]; !ok {
		t.Errorf("empty file dropped from the tar: %#v", got)
	}
}

// TestBuildUploadTarKeepsFolderShape covers dragging a whole directory: the
// relative paths survive (they ride in the field name precisely because Go
// would strip them off the filename), and every parent gets its own entry.
func TestBuildUploadTarKeepsFolderShape(t *testing.T) {
	form := uploadForm(t, map[string]string{
		"conf/mysql/my.cnf": "a",
		"conf/pg/pg.conf":   "b",
		"conf/README":       "c",
	})
	tarball, names, _, err := buildUploadTar(form)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("names = %v", names)
	}
	got := readTar(t, tarball)
	for _, want := range []string{"conf/", "conf/mysql/", "conf/pg/", "conf/mysql/my.cnf", "conf/pg/pg.conf", "conf/README"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q; tar holds %#v", want, got)
		}
	}
	if got["conf/mysql/my.cnf"] != "a" {
		t.Errorf("conf/mysql/my.cnf = %q", got["conf/mysql/my.cnf"])
	}
	// Each directory is emitted once even though three files share "conf".
	n := 0
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Name == "conf/" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("conf/ emitted %d times, want 1", n)
	}
}

// TestBuildUploadTarRejectsEscapes is the one that matters for safety: the
// destination is a whitelist, but a path in the form could still walk out of it.
func TestBuildUploadTarRejectsEscapes(t *testing.T) {
	for _, bad := range []string{
		"../etc/passwd",
		"a/../../etc/passwd",
		"/etc/passwd",
		`..\windows\system32`,
		"./x",
		"",
	} {
		t.Run(bad, func(t *testing.T) {
			form := uploadForm(t, map[string]string{bad: "x"})
			_, _, _, err := buildUploadTar(form)
			if err == nil {
				t.Fatalf("accepted %q", bad)
			}
			if got := uploadStatus(err); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (err %v)", got, err)
			}
		})
	}
}

// TestBuildUploadTarEmpty — a drop that carried no file parts is a client error,
// not an empty tar handed to the engine.
func TestBuildUploadTarEmpty(t *testing.T) {
	_, _, _, err := buildUploadTar(&multipart.Form{})
	if err == nil {
		t.Fatal("accepted an empty form")
	}
	if got := uploadStatus(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// TestNodeUploadDests pins the closed set the handler accepts — the UI menu and
// this map have to stay the same four paths.
func TestNodeUploadDests(t *testing.T) {
	for _, ok := range []string{"/", "/home", "/root", "/tmp"} {
		if !nodeUploadDests[ok] {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, no := range []string{"", "/etc", "/tmp/", "tmp", "/root/../etc", "/var/lib/mysql"} {
		if nodeUploadDests[no] {
			t.Errorf("%q should be rejected", no)
		}
	}
}

// TestPlanUploadEnforcesConfiguredLimit — the cap is the instance's configured
// maxUploadBytes, not a constant, and the refusal names both numbers so an
// operator knows what to raise it to.
func TestPlanUploadEnforcesConfiguredLimit(t *testing.T) {
	form := uploadForm(t, map[string]string{"big.bin": strings.Repeat("x", 4096)})

	if _, err := planUpload(form, 1024); err == nil {
		t.Fatal("accepted 4 KiB under a 1 KiB limit")
	} else {
		if got := uploadStatus(err); got != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", got)
		}
		if !strings.Contains(err.Error(), "limit for this instance") {
			t.Errorf("error does not name the limit: %v", err)
		}
	}
	// Exactly at the limit is allowed; the check is "over", not "at".
	if _, err := planUpload(form, 4096); err != nil {
		t.Errorf("rejected an upload exactly at the limit: %v", err)
	}
}

// TestSystemSettingsDefaultAndClamp pins the shipped ceiling and the bounds an
// admin-entered value is clamped into.
func TestSystemSettingsDefaultAndClamp(t *testing.T) {
	if defaultMaxUploadBytes != 4<<30 {
		t.Errorf("default max upload = %d, want 4 GiB", defaultMaxUploadBytes)
	}
	for _, tc := range []struct{ in, want int64 }{
		{0, defaultMaxUploadBytes},       // unset degrades to the default
		{-1, defaultMaxUploadBytes},      // so does nonsense
		{1, minMaxUploadBytes},           // clamped up, not rejected
		{1 << 50, maxMaxUploadBytes},     // clamped down
		{8 << 30, 8 << 30},               // an in-range value is kept
		{defaultMaxUploadBytes, 4 << 30}, //
	} {
		got := SystemSettings{MaxUploadBytes: tc.in}.normalize().MaxUploadBytes
		if got != tc.want {
			t.Errorf("normalize(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestSystemSettingsRoundTrip — the store is the source of truth, and an absent
// or corrupt row degrades to the default rather than erroring.
func TestSystemSettingsRoundTrip(t *testing.T) {
	app := newTestApp(t)
	if got := app.maxUploadBytes(); got != defaultMaxUploadBytes {
		t.Errorf("unset limit = %d, want the default %d", got, defaultMaxUploadBytes)
	}
	if err := app.store.SetAppSetting(settingMaxUploadBytes, "2147483648"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := app.maxUploadBytes(); got != 2<<30 {
		t.Errorf("stored limit = %d, want 2 GiB", got)
	}
	// A hand-edited row can never wedge an upload.
	app.store.SetAppSetting(settingMaxUploadBytes, "not a number")
	if got := app.maxUploadBytes(); got != defaultMaxUploadBytes {
		t.Errorf("corrupt limit = %d, want the default", got)
	}
	// Nor can one below the floor.
	app.store.SetAppSetting(settingMaxUploadBytes, "5")
	if got := app.maxUploadBytes(); got != minMaxUploadBytes {
		t.Errorf("tiny limit = %d, want the floor %d", got, minMaxUploadBytes)
	}
}

// TestHumanLimit — the limit reads as a whole number in the UI and in the
// over-size refusal, which is the whole reason it is not stalksummary's
// humanBytes.
func TestHumanLimit(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{4 << 30, "4 GiB"},
		{1 << 40, "1 TiB"},
		{256 << 20, "256 MiB"},
		{1 << 20, "1 MiB"},
		{(3 << 30) + (512 << 20), "3.5 GiB"},
		{1500, "1500 bytes"},
	} {
		if got := humanLimit(tc.n); got != tc.want {
			t.Errorf("humanLimit(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
