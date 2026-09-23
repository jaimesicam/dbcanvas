package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
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
// What it reads is the repository's main branch, raw: VERSION for the number, and
// app/whatsnew.go for what changed. The release notes are Go literals (see
// whatsnew.go for why), so they are read back with go/parser rather than scraped —
// the same source the What's New dialog is compiled from, which means an update
// check shows exactly the notes the next build would show, in the same shape. The
// repository has no tags or GitHub releases to read instead, and main is what
// `git pull` delivers.

// updateRawBase is where the repository's files are fetched from. A var so tests
// can point it at an httptest server.
var updateRawBase = "https://raw.githubusercontent.com/jaimesicam/dbcanvas/main"

type updateCheck struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	// Dev is set when this build is unstamped, so there is no number to compare
	// against; the latest version is still reported.
	Dev bool `json:"dev,omitempty"`
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

func checkForUpdates(ctx context.Context, current string) (updateCheck, error) {
	res := updateCheck{Current: current, Notes: []releaseNote{}, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	raw, err := fetchRepoFile(ctx, "VERSION")
	if err != nil {
		return res, err
	}
	res.Latest = strings.TrimSpace(string(raw))
	if res.Latest == "" {
		return res, fmt.Errorf("VERSION on main is empty")
	}
	if current == devVersion {
		res.Dev = true
		return res, nil
	}
	if compareVersions(res.Latest, current) <= 0 {
		return res, nil
	}
	res.Available = true

	src, err := fetchRepoFile(ctx, "app/whatsnew.go")
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

func fetchRepoFile(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateRawBase+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
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
