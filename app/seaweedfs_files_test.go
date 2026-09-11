package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The HEAD fixtures below are real chrislusf/seaweedfs responses, captured from a live filer
// (weed server -s3) while this was written — a file, a directory, and a .html file, which is the
// case that rules out deciding "folder" from the content type.

const seaweedHeadFile = "HTTP/1.1 200 OK\r\n" +
	"Accept-Ranges: bytes\r\n" +
	"Content-Disposition: inline; filename=a.txt\r\n" +
	"Content-Length: 13\r\n" +
	"Content-Type: text/plain\r\n" +
	"Etag: \"08e5fe0dc9671a2a437a5ab0198bfba8\"\r\n" +
	"Last-Modified: Fri, 11 Sep 2026 13:21:52 GMT\r\n"

const seaweedHeadDir = "HTTP/1.1 200 OK\r\n" +
	"Server: SeaweedFS 30GB 4.46\r\n" +
	"X-Amz-Request-Id: 18D446E7A1D36F56AE933CEC\r\n" +
	"Date: Fri, 11 Sep 2026 13:21:52 GMT\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n"

const seaweedHeadHTMLFile = "HTTP/1.1 200 OK\r\n" +
	"Accept-Ranges: bytes\r\n" +
	"Content-Length: 15\r\n" +
	"Content-Type: text/html\r\n" +
	"Etag: \"bd915d7222fd5f2e550377bb792f6bfa\"\r\n"

func TestParseSeaweedHead(t *testing.T) {
	cases := []struct {
		name string
		head string
		dir  bool
		size int64
	}{
		{"file", seaweedHeadFile, false, 13},
		{"directory", seaweedHeadDir, true, 0},
		{"an .html object is still a file", seaweedHeadHTMLFile, false, 15},
	}
	for _, c := range cases {
		got := parseSeaweedHead(c.head)
		if got.Dir != c.dir {
			t.Errorf("%s: Dir = %v, want %v", c.name, got.Dir, c.dir)
		}
		if got.Size != c.size {
			t.Errorf("%s: Size = %d, want %d", c.name, got.Size, c.size)
		}
	}
}

func TestSeaweedStatus(t *testing.T) {
	cases := []struct {
		name    string
		res     ExecResult
		execErr error
		body    string
		wantErr string // "" = no error, "404" = errNoSuchObject, else a substring
	}{
		{name: "ok", res: ExecResult{Stdout: "Etag: x\n200"}, body: "Etag: x"},
		{name: "created", res: ExecResult{Stdout: "\n201"}},
		// What a delete answers, whether or not the key was there — which is why
		// handleSeaweedDelete stats first instead of counting on this.
		{name: "deleted", res: ExecResult{Stdout: "\n204"}},
		{name: "missing", res: ExecResult{Stdout: "\n404"}, wantErr: "404"},
		{name: "filer down", res: ExecResult{Stdout: "\n000", Code: 7}, wantErr: "not answering"},
		{name: "no output at all", res: ExecResult{Stdout: ""}, wantErr: "not answering"},
		{name: "other status", res: ExecResult{Stdout: "\n503"}, wantErr: "answered 503"},
		{name: "exec failed", execErr: errors.New("container is not running"), wantErr: "not running"},
		// Only one line: curl printed the status and nothing else, so there is no body.
		{name: "status only", res: ExecResult{Stdout: "200"}},
	}
	for _, c := range cases {
		body, _, err := seaweedStatus(c.res, c.execErr)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr == "404" && !errors.Is(err, errNoSuchObject):
			t.Errorf("%s: error = %v, want errNoSuchObject", c.name, err)
		case c.wantErr != "" && c.wantErr != "404":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: error = %v, want one mentioning %q", c.name, err, c.wantErr)
			}
		}
		if c.wantErr == "" && body != c.body {
			t.Errorf("%s: body = %q, want %q", c.name, body, c.body)
		}
	}
}

// An object key is arbitrary text. It never reaches a shell, but it does reach a URL, so the
// escaping has to survive the characters S3 allows and HTTP does not.
func TestSeaweedFilerEnv(t *testing.T) {
	env := seaweedFilerEnv("my.bucket", "dir one/we ird #name.txt")
	want := map[string]string{
		"BUCKET": "my.bucket",
		"KEY":    "dir%20one/we%20ird%20%23name.txt",
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if got["PORT"] != "8888" {
		t.Errorf("PORT = %q, want the filer port", got["PORT"])
	}
}

// The staging name must be ours alone: inside the temp directory, unpredictable, and with
// nothing of the key in it (see the file header for why).
func TestSeaweedTempPath(t *testing.T) {
	a, b := seaweedTempPath(), seaweedTempPath()
	if a == b {
		t.Error("two staging paths collided")
	}
	for _, p := range []string{a, b} {
		if !strings.HasPrefix(p, seaweedTmpDir+"/") {
			t.Errorf("%q is not under %q", p, seaweedTmpDir)
		}
		if name := strings.TrimPrefix(p, seaweedTmpDir+"/"); len(name) != 32 || strings.ContainsAny(name, "/.") {
			t.Errorf("%q is not a plain hex name", name)
		}
	}
}

// The transfer endpoint's destination is named by the client, so "is that a SeaweedFS node?"
// has to be answerable from the deployment rather than trusted.
func TestSeaweedConfigOf(t *testing.T) {
	seaweed, _ := json.Marshal(seaweedConfig{Bucket: "backups", Buckets: []string{"backups", "dumps"}})
	if cfg, err := seaweedConfigOf(Deployment{Config: seaweed}); err != nil || len(cfg.Buckets) != 2 {
		t.Errorf("a SeaweedFS deployment: cfg = %+v, err = %v", cfg, err)
	}
	other, _ := json.Marshal(map[string]any{"fqdn": "pg1.example.net", "pgMajor": "18"})
	if _, err := seaweedConfigOf(Deployment{Config: other}); err == nil {
		t.Error("a PostgreSQL deployment was accepted as a SeaweedFS node")
	}
	if _, err := seaweedConfigOf(Deployment{}); err == nil {
		t.Error("a deployment with no config was accepted as a SeaweedFS node")
	}
}

// A folder is a backup prefix, so the recursive flag is never implied: the filer refuses a
// non-empty folder without it (500, "fail to delete non-empty folder"), which is the behaviour
// handleSeaweedDelete relies on having asked the caller about first.
func TestSeaweedRecurseQuery(t *testing.T) {
	if got := seaweedRecurseQuery(true); got != "?recursive=true" {
		t.Errorf("recursive query = %q", got)
	}
	if got := seaweedRecurseQuery(false); got != "" {
		t.Errorf("non-recursive query = %q, want empty", got)
	}
}
