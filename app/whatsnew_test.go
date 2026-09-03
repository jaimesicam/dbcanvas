package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNotesAreOrderedNewestFirst(t *testing.T) {
	for i := 1; i < len(whatsNewNotes); i++ {
		if compareVersions(whatsNewNotes[i-1].Version, whatsNewNotes[i].Version) < 0 {
			t.Errorf("note %d (%s) is older than the one before it (%s); the list is newest-first",
				i, whatsNewNotes[i].Version, whatsNewNotes[i-1].Version)
		}
	}
}

func TestEveryNoteIsComplete(t *testing.T) {
	for _, n := range whatsNewNotes {
		if n.Version == "" || n.Date == "" || n.Title == "" || n.Body == "" {
			t.Errorf("incomplete note: %+v", n)
		}
		if len(n.Body) < 80 {
			t.Errorf("%q: the body is too short to be worth a dialog: %q", n.Title, n.Body)
		}
		if len(n.Date) != 10 || strings.Count(n.Date, "-") != 2 {
			t.Errorf("%q: date %q is not YYYY-MM-DD", n.Title, n.Date)
		}
		if n.Doc != "" && !strings.HasSuffix(n.Doc, ".md") {
			t.Errorf("%q: Doc %q should be a repo-relative markdown path", n.Title, n.Doc)
		}
	}
}

// TestNoteDocsExist keeps a "read the full notes" link from 404ing. The paths are
// repo-relative, so the test walks up out of app/.
func TestNoteDocsExist(t *testing.T) {
	for _, n := range whatsNewNotes {
		if n.Doc == "" {
			continue
		}
		if _, err := os.Stat("../" + n.Doc); err != nil {
			t.Errorf("%q links to %s, which does not exist", n.Title, n.Doc)
		}
	}
}

// TestWhatsNewMatchesREADME is the drift guard. The dialog and the README's
// "What's new" section are written separately — deliberately, because one is read
// once in a modal and the other is read on GitHub — so the thing to check is that
// nobody added an entry to one and forgot the other.
func TestWhatsNewMatchesREADME(t *testing.T) {
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Skipf("README.md not readable from here: %v", err)
	}
	readme := string(raw)
	i := strings.Index(readme, "## What's new")
	if i < 0 {
		t.Fatal(`README.md has no "## What's new" section`)
	}
	// The README's version of a title carries markup — <code>dbcanvas-cli</code>,
	// <b>…</b> — so both sides are flattened before comparing. Matching the raw
	// text would fail on formatting rather than on drift, which would make this
	// guard something people disable instead of something they trust.
	section := flattenMarkup(readme[i:])
	for _, n := range whatsNewNotes {
		if !strings.Contains(section, flattenMarkup(n.Title)) {
			t.Errorf("note %q is not in the README's What's new section — add it there too", n.Title)
		}
	}
}

// flattenMarkup strips HTML tags and backticks so a title matches whether or not
// somebody wrapped part of it in <code>.
func flattenMarkup(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth > 0 || r == '`':
			// inside a tag, or a backtick: drop it
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ------------------------------------------------------------- unseen logic

// TestHasUnseenNotes drives the decision against a fixed note list rather than the
// shipped catalogue, so the expectations stay true as releases are added.
func TestHasUnseenNotes(t *testing.T) {
	notes := []releaseNote{
		{Version: "0.10.0", Title: "newest"},
		{Version: "0.2.0", Title: "middle"},
		{Version: "0.1.0", Title: "oldest"},
	}
	for _, c := range []struct {
		name, seen, current string
		want                bool
	}{
		{"never seen anything", "", "0.10.0", true},
		{"behind the current build", "0.2.0", "0.10.0", true},
		{"already read this build", "0.10.0", "0.10.0", false},
		{"ahead of it (rolled back)", "0.11.0", "0.10.0", false},
		// The comparison string ordering gets wrong: "0.10.0" < "0.2.0" lexically.
		{"seen a double-digit minor", "0.10.0", "0.2.0", false},
		{"a dev build does not suppress an unread note", "0.2.0", "dev", true},
		{"dev already acknowledged", "dev", "dev", false},
	} {
		if got := hasUnseenIn(notes, c.seen, c.current); got != c.want {
			t.Errorf("%s: hasUnseenIn(%q, %q) = %v, want %v", c.name, c.seen, c.current, got, c.want)
		}
	}

	// And the case that is easy to get wrong: the build moved on, but nobody wrote
	// a note for it. Nothing should open.
	if hasUnseenIn(notes, "0.10.0", "0.11.0") {
		t.Error("a release with no note written for it should show nothing at all")
	}
}

func TestNotesNewerThan(t *testing.T) {
	if got := len(notesNewerThan("")); got != len(whatsNewNotes) {
		t.Errorf("a reader who has seen nothing gets %d notes, want all %d", got, len(whatsNewNotes))
	}
	newest := whatsNewNotes[0].Version
	if got := len(notesNewerThan(newest)); got != 0 {
		t.Errorf("a reader on the newest release (%s) gets %d notes, want 0", newest, got)
	}
	oldest := whatsNewNotes[len(whatsNewNotes)-1].Version
	rest := notesNewerThan(oldest)
	if len(rest) == 0 {
		t.Fatalf("a reader on the oldest release (%s) should still have something to read", oldest)
	}
	for _, n := range rest {
		if compareVersions(n.Version, oldest) <= 0 {
			t.Errorf("%q is %s and should already have been seen", n.Title, n.Version)
		}
	}
}

// ------------------------------------------------------------- settings

// TestNormalizePreservesWhatsNewSeen guards the trap: every other field in
// UserSettings is clamped to a fixed set, so clamping this one too is the obvious
// next edit — and it would re-open the dialog for everybody on every save.
func TestNormalizePreservesWhatsNewSeen(t *testing.T) {
	for _, seen := range []string{"0.2.0", "dev", "99.99.99", ""} {
		in := UserSettings{TerminalMode: "nonsense", Theme: "nonsense", WhatsNewSeen: seen}
		if got := in.normalize().WhatsNewSeen; got != seen {
			t.Errorf("normalize() turned whatsNewSeen %q into %q", seen, got)
		}
	}
}

// TestSettingsPutCannotBlankWhatsNewSeen covers the older-client case: a PUT that
// omits the field must leave it alone rather than resetting it.
func TestSettingsPutCannotBlankWhatsNewSeen(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)
	app.saveUserSettings(u.ID, UserSettings{TerminalMode: TerminalDocked, Theme: "dark",
		DeploymentBackend: BackendDocker, WhatsNewSeen: "0.2.0"})

	r := httptest.NewRequest("PUT", "/api/me/settings",
		strings.NewReader(`{"terminalMode":"undocked","theme":"forest","deploymentBackend":"docker"}`))
	r = withPrincipal(r, principal{User: u})
	w := httptest.NewRecorder()
	app.handleUpdateSettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT returned %d: %s", w.Code, w.Body.String())
	}
	if got := app.userSettingsFor(u.ID); got.WhatsNewSeen != "0.2.0" {
		t.Errorf("whatsNewSeen is %q after a partial PUT, want 0.2.0", got.WhatsNewSeen)
	} else if got.Theme != "forest" {
		t.Errorf("the PUT did not apply: theme is %q", got.Theme)
	}
}

// TestNewAccountsAreStamped: a first-time user should not be met with a changelog.
func TestNewAccountsAreStamped(t *testing.T) {
	app := newTestApp(t)
	w := httptest.NewRecorder()
	app.handleSetup(w, httptest.NewRequest("POST", "/api/setup",
		strings.NewReader(`{"username":"admin","password":"password123"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("setup returned %d: %s", w.Code, w.Body.String())
	}
	var u User
	json.Unmarshal(w.Body.Bytes(), &u)
	s := app.userSettingsFor(u.ID)
	if s.WhatsNewSeen != appVersion {
		t.Errorf("a new admin's whatsNewSeen is %q, want %q", s.WhatsNewSeen, appVersion)
	}
	if hasUnseenNotes(s.WhatsNewSeen, appVersion) {
		t.Error("a brand-new account would be shown the release notes")
	}
}

func TestWhatsNewHandler(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("jaime", "x", RoleUser, StatusApproved)

	get := func() map[string]any {
		r := withPrincipal(httptest.NewRequest("GET", "/api/whatsnew", nil), principal{User: u})
		w := httptest.NewRecorder()
		app.handleWhatsNew(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/whatsnew returned %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// An existing account that has never acknowledged anything sees the dialog.
	if out := get(); out["hasUnseen"] != true {
		t.Errorf("hasUnseen is %v for an account that has read nothing, want true", out["hasUnseen"])
	}

	// Dismissing it is durable.
	r := withPrincipal(httptest.NewRequest("POST", "/api/whatsnew/seen", nil), principal{User: u})
	w := httptest.NewRecorder()
	app.handleWhatsNewSeen(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/whatsnew/seen returned %d: %s", w.Code, w.Body.String())
	}
	out := get()
	if out["hasUnseen"] != false {
		t.Errorf("hasUnseen is %v after dismissing, want false", out["hasUnseen"])
	}
	if out["seen"] != appVersion {
		t.Errorf("seen is %v, want %q", out["seen"], appVersion)
	}
	// The full list is always served, because the header link shows everything.
	if notes, _ := out["notes"].([]any); len(notes) != len(whatsNewNotes) {
		t.Errorf("notes has %d entries, want %d", len(notes), len(whatsNewNotes))
	}
	if unseen, _ := out["unseen"].([]any); len(unseen) != 0 {
		t.Errorf("unseen has %d entries after dismissing, want 0", len(unseen))
	}
}

func TestWhatsNewNeedsAuth(t *testing.T) {
	app := newTestApp(t)
	for _, h := range []http.HandlerFunc{app.handleWhatsNew, app.handleWhatsNewSeen} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("GET", "/api/whatsnew", nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated request returned %d, want 401", w.Code)
		}
	}
}
