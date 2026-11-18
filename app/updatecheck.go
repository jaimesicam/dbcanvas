package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// updatecheck.go — "is there a newer DBCanvas?", asked only when somebody asks.
//
// Nothing here runs on a timer or at startup. A lab host is often air-gapped or
// behind a proxy, and an app that phones GitHub on its own is one somebody has to
// find out about and turn off; so the dashboard has a button, and pressing it is
// the only thing that makes a request leave the machine.
//
// What it reads is the repository's latest GitHub release: its tag for the number and
// its assets for the dbcanvas binary that fits this machine (dbcanvas-<os>-<arch>), and
// app/whatsnew.go at that tag for what changed. The release notes are Go literals (see
// whatsnew.go for why), so they are read back with go/parser rather than scraped — the
// same source the What's New dialog is compiled from, so an update check shows exactly
// the notes the new binary will show, in the same shape.

// updateAPIBase is the repository on the GitHub API, and updateRawBase its raw files
// (a ref and a path follow). Vars so tests can point them at an httptest server.
var (
	updateAPIBase = "https://api.github.com/repos/jaimesicam/dbcanvas"
	updateRawBase = "https://raw.githubusercontent.com/jaimesicam/dbcanvas"
)

type updateCheck struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	// Dev is set when this build is unstamped, so there is no number to compare
	// against; the latest version is still reported.
	Dev bool `json:"dev,omitempty"`
	// ReleaseURL is the release's page; Download is its binary for this machine's
	// OS and architecture, when the release has one.
	ReleaseURL string `json:"releaseUrl,omitempty"`
	Download   string `json:"download,omitempty"`
	Asset      string `json:"asset,omitempty"`
	// Notes are the release notes for every version after Current up to and
	// including Latest, newest first.
	Notes []releaseNote `json:"notes"`
	// NotesError is set when the version was read but its notes could not be:
	// the answer to "is there an update" still stands.
	NotesError string `json:"notesError,omitempty"`
	CheckedAt  string `json:"checkedAt"`
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	res, err := checkForUpdates(ctx, appVersion)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not reach GitHub: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// latestRelease is the part of GitHub's release object the check reads.
type latestRelease struct {
	Tag     string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// releaseAsset is the binary for goos/goarch: dbcanvas-<os>-<arch>, bare or with an
// archive extension.
func releaseAsset(rel latestRelease, goos, goarch string) (name, url string) {
	// The binary itself, never its signature (want.sigstore.json); a packed one
	// (want.tar.gz) only when there is no bare binary.
	want := "dbcanvas-" + goos + "-" + goarch
	for _, a := range rel.Assets {
		if a.Name == want {
			return a.Name, a.URL
		}
	}
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, want+".") && !strings.HasSuffix(a.Name, ".sigstore.json") {
			return a.Name, a.URL
		}
	}
	return "", ""
}

func checkForUpdates(ctx context.Context, current string) (updateCheck, error) {
	res := updateCheck{Current: current, Notes: []releaseNote{}, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	raw, err := fetchURL(ctx, updateAPIBase+"/releases/latest")
	if errors.Is(err, errNotFound) {
		return res, fmt.Errorf("no dbcanvas release is published yet")
	}
	if err != nil {
		return res, err
	}
	var rel latestRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return res, fmt.Errorf("the latest release: %v", err)
	}
	res.Latest = strings.TrimPrefix(strings.TrimSpace(rel.Tag), "v")
	if res.Latest == "" {
		return res, fmt.Errorf("the latest release has no tag")
	}
	res.ReleaseURL = rel.HTMLURL
	res.Asset, res.Download = releaseAsset(rel, runtime.GOOS, runtime.GOARCH)
	if current == devVersion {
		res.Dev = true
		return res, nil
	}
	if compareVersions(res.Latest, current) <= 0 {
		return res, nil
	}
	res.Available = true

	src, err := fetchURL(ctx, updateRawBase+"/"+rel.Tag+"/app/whatsnew.go")
	if err == nil {
		var notes []releaseNote
		if notes, err = parseReleaseNotes(src); err == nil {
			res.Notes = notesBetween(notes, current, res.Latest)
		}
	}
	if err != nil {
		res.NotesError = err.Error()
	}
	return res, nil
}

// notesBetween keeps the notes newer than current and no newer than latest. The
// upper bound matters: main can carry notes for a release whose VERSION bump has
// not landed yet.
func notesBetween(notes []releaseNote, current, latest string) []releaseNote {
	out := []releaseNote{}
	for _, n := range notes {
		if compareVersions(n.Version, current) > 0 && compareVersions(n.Version, latest) <= 0 {
			out = append(out, n)
		}
	}
	return out
}

var errNotFound = errors.New("not found")

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dbcanvas/"+appVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// parseReleaseNotes reads the whatsNewNotes literal out of a whatsnew.go source.
// Only string literals and their concatenation are understood, which is all that
// file uses; an entry with anything else in it is an error rather than a guess.
func parseReleaseNotes(src []byte) ([]releaseNote, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "whatsnew.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse whatsnew.go: %w", err)
	}
	var lit *ast.CompositeLit
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if name.Name == "whatsNewNotes" && i < len(vs.Values) {
					lit, _ = vs.Values[i].(*ast.CompositeLit)
				}
			}
		}
	}
	if lit == nil {
		return nil, fmt.Errorf("whatsNewNotes not found in whatsnew.go")
	}
	notes := make([]releaseNote, 0, len(lit.Elts))
	for _, e := range lit.Elts {
		el, ok := e.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("unexpected release note entry")
		}
		var n releaseNote
		for _, kv := range el.Elts {
			kv, ok := kv.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("release note entry is not keyed")
			}
			key, _ := kv.Key.(*ast.Ident)
			if key == nil {
				continue
			}
			s, err := stringExpr(kv.Value)
			if err != nil {
				return nil, fmt.Errorf("release note %s: %w", key.Name, err)
			}
			switch key.Name {
			case "Version":
				n.Version = s
			case "Date":
				n.Date = s
			case "Title":
				n.Title = s
			case "Body":
				n.Body = s
			case "Doc":
				n.Doc = s
			}
		}
		notes = append(notes, n)
	}
	return notes, nil
}

func stringExpr(e ast.Expr) (string, error) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", fmt.Errorf("not a string literal")
		}
		return strconv.Unquote(v.Value)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", fmt.Errorf("unsupported operator %s", v.Op)
		}
		l, err := stringExpr(v.X)
		if err != nil {
			return "", err
		}
		r, err := stringExpr(v.Y)
		if err != nil {
			return "", err
		}
		return l + r, nil
	case *ast.ParenExpr:
		return stringExpr(v.X)
	}
	return "", fmt.Errorf("not a string literal")
}
