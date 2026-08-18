package main

// nodefs_test.go — the parts of the File Manager that are pure logic: reading
// stat's output back, and the two validators standing between a text box and a
// shell.

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// rec builds one NUL-terminated listing record the way fsListScript emits it.
func rec(fields ...string) string { return strings.Join(fields, ";") + "\x00" }

// TestParseFSListReadsStat covers the shapes a real listing holds: a file, a
// directory, a symlink with its target, and the setuid/sticky bits that a
// three-digit mode would silently drop.
func TestParseFSListReadsStat(t *testing.T) {
	out := rec("81a4", "168", "0", "0", "root", "root", "1787006000", "-rw-r--r--", "'/etc/hosts'") +
		rec("41ed", "4096", "0", "0", "root", "root", "1778529579", "drwxr-xr-x", "'/etc/security'") +
		rec("a1ff", "19", "0", "0", "root", "root", "1755000000", "lrwxrwxrwx", "'/etc/mtab' -> '../proc/self/mounts'") +
		rec("43ff", "4096", "0", "0", "root", "root", "1755000000", "drwxrwxrwt", "'/tmp'") +
		rec("89ed", "1234", "0", "0", "root", "root", "1755000000", "-rwsr-xr-x", "'/usr/bin/su'")

	got := parseFSList(out)
	if len(got) != 5 {
		t.Fatalf("parsed %d entries, want 5: %#v", len(got), got)
	}

	// Names come back as the leaf, not the path stat was handed.
	if got[0].Name != "hosts" || got[0].Type != "file" || got[0].Mode != "0644" || got[0].Size != 168 {
		t.Errorf("file entry = %#v", got[0])
	}
	if got[0].User != "root" || got[0].Group != "root" || got[0].ModTime != 1787006000 {
		t.Errorf("file ownership/time = %#v", got[0])
	}
	if got[1].Name != "security" || got[1].Type != "dir" || got[1].Mode != "0755" {
		t.Errorf("dir entry = %#v", got[1])
	}
	// A symlink keeps its own type and reports where it points.
	if got[2].Type != "link" || got[2].Name != "mtab" || got[2].Target != "../proc/self/mounts" {
		t.Errorf("link entry = %#v", got[2])
	}
	// Sticky and setuid live above the low nine bits — the reason Mode is four
	// digits and not three. The hex values above are real ones, read off a
	// running node with `stat -c '%f'` rather than worked out by hand.
	if got[3].Mode != "1777" {
		t.Errorf("sticky dir mode = %q, want 1777", got[3].Mode)
	}
	if got[4].Mode != "4755" {
		t.Errorf("setuid file mode = %q, want 4755", got[4].Mode)
	}
}

// TestParseFSListSurvivesJunk — one unreadable record must not cost the whole
// directory, and a name containing the field separator must still parse.
func TestParseFSListSurvivesJunk(t *testing.T) {
	out := rec("81a4", "1", "0", "0", "root", "root", "1", "-rw-r--r--", "'/tmp/a;b.txt'") +
		"garbage-with-no-fields\x00" +
		rec("notahexmode", "1", "0", "0", "root", "root", "1", "-rw-r--r--", "'/tmp/bad'") +
		rec("81a4", "2", "0", "0", "root", "root", "1", "-rw-r--r--", "'/tmp/good.txt'")

	got := parseFSList(out)
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2 (junk skipped): %#v", len(got), got)
	}
	// SplitN with a limit is what keeps the ';' inside the name.
	if got[0].Name != "a;b.txt" {
		t.Errorf("name with a separator = %q, want a;b.txt", got[0].Name)
	}
	if got[1].Name != "good.txt" {
		t.Errorf("entry after junk = %q", got[1].Name)
	}
}

// TestParseFSListUnknownIdentity — a uid with no passwd entry is routine after
// copying between nodes; busybox says UNKNOWN, and the number is the answer
// worth showing.
func TestParseFSListUnknownIdentity(t *testing.T) {
	got := parseFSList(rec("81a4", "6", "48", "12", "UNKNOWN", "UNKNOWN", "1", "-rwx------", "'/tmp/x'"))
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	if got[0].User != "48" || got[0].Group != "12" {
		t.Errorf("unmapped ids = %q/%q, want 48/12", got[0].User, got[0].Group)
	}
}

func TestParseStatName(t *testing.T) {
	for _, tc := range []struct{ in, name, target string }{
		{"'/etc/hosts'", "/etc/hosts", ""},
		{"'/etc/mtab' -> '../proc/self/mounts'", "/etc/mtab", "../proc/self/mounts"},
		{"'/tmp/it'\\''s'", "/tmp/it's", ""},
		{"'/tmp/a -> b'", "/tmp/a -> b", ""}, // an arrow inside a name is not a link
	} {
		name, target := parseStatName(tc.in)
		if name != tc.name || target != tc.target {
			t.Errorf("parseStatName(%q) = (%q,%q), want (%q,%q)", tc.in, name, target, tc.name, tc.target)
		}
	}
}

func TestFSCleanPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/etc/", "/etc"},
		{"etc", "/etc"},
		{"/etc/../etc/hosts", "/etc/hosts"},
		{"/../../..", "/"},          // cannot climb above the container root
		{"/tmp/./a//b", "/tmp/a/b"}, //
		{"  /tmp/x  ", "/tmp/x"},
	} {
		if got := fsCleanPath(tc.in); got != tc.want {
			t.Errorf("fsCleanPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidChmodMode — the mode reaches a shell (via the environment, so it
// cannot become a second command), but a value that is not a mode at all should
// never get as far as chmod.
func TestValidChmodMode(t *testing.T) {
	for _, ok := range []string{"644", "0644", "755", "4755", "1777", "7777", "u+x", "go-w", "a=rx", "u+x,g-w", "u+X"} {
		if !validChmodMode(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, no := range []string{"", "8888", "10000", "0644; rm -rf /", "$(id)", "`id`", "644 /etc", "rwx!"} {
		if validChmodMode(no) {
			t.Errorf("%q should be rejected", no)
		}
	}
}

func TestValidIdentity(t *testing.T) {
	for _, ok := range []string{"", "root", "www-data", "systemd-network", "48", "user.name", "svc$"} {
		if !validIdentity(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, no := range []string{"root; touch /pwned", "a b", "$(id)", "`id`", "a|b", "a/b", strings.Repeat("x", 65)} {
		if validIdentity(no) {
			t.Errorf("%q should be rejected", no)
		}
	}
}

// TestFSPathsBodyRefusesRoot — every mutating endpoint shares this guard, and
// "/" is the one path where a recursive chmod or rm is never what was meant.
func TestFSPathsBodyRefusesRoot(t *testing.T) {
	if _, err := (fsPathsBody{Paths: []string{"/tmp/a", "/"}}).cleanPaths(); err == nil {
		t.Error("accepted / in the selection")
	}
	if _, err := (fsPathsBody{Paths: []string{"/tmp/../"}}).cleanPaths(); err == nil {
		t.Error("accepted a path that normalizes to /")
	}
	if _, err := (fsPathsBody{}).cleanPaths(); err == nil {
		t.Error("accepted an empty selection")
	}
	got, err := (fsPathsBody{Paths: []string{"/tmp/a/", "b"}}).cleanPaths()
	if err != nil {
		t.Fatalf("rejected a valid selection: %v", err)
	}
	if got[0] != "/tmp/a" || got[1] != "/b" {
		t.Errorf("cleanPaths = %v", got)
	}
}

func TestFSTypeOf(t *testing.T) {
	for _, tc := range []struct {
		raw  uint64
		want string
	}{
		{0o100644, "file"}, {0o040755, "dir"}, {0o120777, "link"},
		{0o140755, "other"}, // socket
		{0o010644, "other"}, // fifo
	} {
		if got := fsTypeOf(tc.raw); got != tc.want {
			t.Errorf("fsTypeOf(%o) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestFSShellHint — a distroless node cannot be browsed, and "exec: no such
// file or directory" does not say that to anyone.
func TestFSShellHint(t *testing.T) {
	for _, in := range []string{
		`exec: "sh": executable file not found in $PATH`,
		"failed to create shim task: starting container process caused: exec: sh",
	} {
		if got := fsShellHint(in); !strings.Contains(got, "no shell") {
			t.Errorf("fsShellHint(%q) = %q, want the no-shell explanation", in, got)
		}
	}
	// An ordinary error is passed through untouched.
	if got := fsShellHint("chown: invalid user: 'nope'"); got != "chown: invalid user: 'nope'" {
		t.Errorf("rewrote an ordinary error: %q", got)
	}
}

// --- the editor's guards ---------------------------------------------------

// tarOf builds a one-entry tar the way GetArchiveStream hands one back, so the
// read path can be exercised without a container.
func tarOf(t *testing.T, name string, mode int64, uid, gid int, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Mode: mode, Uid: uid, Gid: gid,
		Size: int64(len(body)), ModTime: time.Unix(1787010518, 0), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	return buf.Bytes()
}

// TestEditorReadsHeaderMetadata — the reason reading goes through the archive
// endpoint rather than `cat` is that the header carries what a save must put
// back. Pin that the header round-trips.
func TestEditorReadsHeaderMetadata(t *testing.T) {
	raw := tarOf(t, "motd.conf", 0o640, 48, 12, []byte("greeting = hello\n"))
	tr := tar.NewReader(bytes.NewReader(raw))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%04o", hdr.Mode&0o7777); got != "0640" {
		t.Errorf("mode = %s, want 0640", got)
	}
	if hdr.Uid != 48 || hdr.Gid != 12 {
		t.Errorf("ownership = %d:%d, want 48:12", hdr.Uid, hdr.Gid)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != "greeting = hello\n" {
		t.Errorf("body = %q", body)
	}
}

// TestEditorRejectsBinary — a textarea would corrupt a binary file, so the two
// signals that say "not text" have to catch it. Real ELF bytes, not a guess.
func TestEditorRejectsBinary(t *testing.T) {
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0}
	if utf8.Valid(elf) && bytes.IndexByte(elf, 0) < 0 {
		t.Error("ELF header should be caught as binary")
	}
	// Invalid UTF-8 with no NUL is still not editable text.
	if utf8.Valid([]byte{0xff, 0xfe, 0x41}) {
		t.Error("invalid UTF-8 should be caught")
	}
	// Ordinary text, including non-ASCII, must pass both checks.
	for _, ok := range []string{"", "hello\n", "héllo → ok\n", "tab\tand\r\nCRLF\n"} {
		if !utf8.Valid([]byte(ok)) || bytes.IndexByte([]byte(ok), 0) >= 0 {
			t.Errorf("%q should be editable", ok)
		}
	}
}

// TestFSMaxEditIsBelowUploadCeiling — the editor's cap is its own, far below
// the configurable upload ceiling, because the constraint is a textarea rather
// than the wire.
func TestFSMaxEditIsBelowUploadCeiling(t *testing.T) {
	if fsMaxEdit != 2<<20 {
		t.Errorf("fsMaxEdit = %d, want 2 MiB", fsMaxEdit)
	}
	if fsMaxEdit >= defaultMaxUploadBytes {
		t.Errorf("the edit cap (%d) should be well under the upload ceiling (%d)", fsMaxEdit, defaultMaxUploadBytes)
	}
	if got := humanLimit(fsMaxEdit); got != "2 MiB" {
		t.Errorf("the refusal would quote %q", got)
	}
}
