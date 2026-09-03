package main

import (
	"net/http"
)

// whatsnew.go — the release notes DBCanvas shows you once, the first time you open
// a build you have not seen.
//
// The notes are Go literals, the same way labs.go and templates_builtin.go carry
// their content. The alternative — parsing the README's "What's new" section at run
// time — was tempting because that prose already exists, but that section is
// <details> blocks, screenshots and relative links written for somebody reading
// GitHub. It is good prose and a bad data source. So these are written separately,
// and whatsnew_test.go asserts that every Title here appears in README.md, which
// catches the one thing that actually goes wrong: the two drifting apart.
//
// Read-state lives on the account (UserSettings.WhatsNewSeen), not in the browser.
// That follows settings.go's own rule — preferences follow the user across browsers
// and machines — and it is the right call here for a specific reason: a localStorage
// flag would re-open this dialog in every new browser and never again after a
// reinstall, which is exactly backwards.

// releaseNote is one entry. Body is plain prose, deliberately: this is read once, in
// a modal, by somebody who wants to know what changed and then get on with it.
type releaseNote struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	// Doc is a repo-relative path to the full guide, or "" when the note is the
	// whole story.
	Doc string `json:"doc,omitempty"`
}

// whatsNewNotes is newest first. Keep it that way: the dialog opens the top entry
// expanded, and the tests assume the ordering.
var whatsNewNotes = []releaseNote{
	{
		Version: "0.0.2",
		Date:    "2026-09-02",
		Title:   "An HTTP API for everything, with tokens that expire",
		Body: "Every one of the 200-odd things DBCanvas can do now has a documented HTTP endpoint, " +
			"and the new API page lists all of them — grouped, searchable, with a copyable curl line " +
			"and the access each one needs. The catalogue is generated from the same table that " +
			"installs the handlers, so it cannot drift out of date. Authenticate with an API token " +
			"you create yourself: give it a name, pick read or write, pick how long it should last, " +
			"and revoke it the moment you no longer want it. There is an OpenAPI document at " +
			"/api/meta/openapi.json for generated clients, Postman and Bruno.",
		Doc: "docs/API.md",
	},
	{
		Version: "0.0.2",
		Date:    "2026-09-02",
		Title:   "dbcanvas-cli — drive a stack from your terminal",
		Body: "Sign in once with `dbcanvas login` and the CLI stores a scoped, expiring token — never " +
			"your password. Then list, create, deploy and destroy stacks, start and stop nodes, open a " +
			"root console, and run the Data Generator, Query Runner and Benchmark, without leaving the " +
			"shell. Anything the curated commands do not cover, `dbcanvas api POST /api/…` does: the " +
			"whole surface is reachable from day one, and `dbcanvas endpoints` prints the catalogue.",
		Doc: "docs/CLI.md",
	},
	{
		Version: "0.0.2",
		Date:    "2026-09-02",
		Title:   "This dialog",
		Body: "When DBCanvas is updated, whoever signs in next gets these notes once — and then not " +
			"again until the next release. The What's new link beside the dashboard heading reopens " +
			"them whenever you want. A brand-new account never sees them: there is nothing new about " +
			"an installation you have only just met.",
	},
	{
		Version: "0.0.1",
		Date:    "2026-09-01",
		Title:   "Every control explains itself",
		Body: "Hover — or tab to — the small ? beside any field, value or button and DBCanvas says what " +
			"it is for, what happens if you leave it alone, and when you would change it. 345 pieces of " +
			"help, across every node form, every deployed node's panel, the node library and the toolbar.",
	},
	{
		Version: "0.0.1",
		Date:    "2026-09-01",
		Title:   "Reach a node on a remote install from your own machine",
		Body: "Set SSH_FORWARDING_HOST in .env and right-clicking a running node offers Copy SSH tunnel " +
			"command: the exact ssh -L line forwarding every port that node publishes, each to the same " +
			"port locally, so every address the UI shows works verbatim through the tunnel. The login " +
			"comes from whoever is signed in to DBCanvas.",
		Doc: "docs/CONFIGURATION.md",
	},
	{
		Version: "0.0.1",
		Date:    "2026-09-01",
		Title:   "Deployment templates",
		Body: "Eleven templates ship with the app, one per engine family. Pick one in New stack and the " +
			"whole design is on the canvas; or Insert template to merge one into a stack you are already " +
			"building. Save as template turns any canvas into a reusable one, without its passwords, host " +
			"paths or pinned ports, and templates export to a .json file you can hand to someone else.",
		Doc: "docs/STACKS.md",
	},
}

// notesNewerThan returns the notes a reader has not seen. seen == "" means they have
// seen nothing, which is every note.
func notesNewerThan(seen string) []releaseNote { return notesNewerThanIn(whatsNewNotes, seen) }

func notesNewerThanIn(notes []releaseNote, seen string) []releaseNote {
	out := []releaseNote{}
	for _, n := range notes {
		if seen == "" || compareVersions(n.Version, seen) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// hasUnseenNotes decides whether the dialog opens by itself.
//
// The decision is made here rather than in the browser on purpose: it needs version
// comparison, the client would have to reimplement compareVersions to do it, and a
// client that gets that subtly wrong shows a dialog nobody asked for on every page
// load.
//
// Both conditions are required, and the second is the one that is easy to forget:
// the build has to have moved on from what this account acknowledged, AND there has
// to be a note it has not read. Shipping a release with no note written for it shows
// nobody anything, which is the right outcome — an empty dialog is worse than none.
func hasUnseenNotes(seen, current string) bool {
	return hasUnseenIn(whatsNewNotes, seen, current)
}

func hasUnseenIn(notes []releaseNote, seen, current string) bool {
	// Nothing to show once the account has acknowledged this build. A `seen` ahead
	// of `current` means the instance was rolled back, which is also nothing to show.
	if seen != "" && compareVersions(seen, current) >= 0 {
		return false
	}
	return len(notesNewerThanIn(notes, seen)) > 0
}

func (a *App) handleWhatsNew(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	seen := a.userSettingsFor(u.ID).WhatsNewSeen
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   appVersion,
		"seen":      seen,
		"hasUnseen": hasUnseenNotes(seen, appVersion),
		"notes":     whatsNewNotes,
		// What the dialog opens with when it opens by itself. The link in the
		// dashboard header shows everything instead.
		"unseen": notesNewerThan(seen),
	})
}

// handleWhatsNewSeen records that the account has read the notes for this build.
//
// It exists as its own endpoint rather than leaving the client to PUT
// /api/me/settings because the dialog would otherwise have to hold, and echo back,
// the whole settings object to change one field — and a stale copy of that object
// would quietly revert somebody's theme.
func (a *App) handleWhatsNewSeen(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	s := a.userSettingsFor(u.ID)
	s.WhatsNewSeen = appVersion
	if err := a.saveUserSettings(u.ID, s); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seen": s.WhatsNewSeen})
}

// stampWhatsNewSeen marks a brand-new account as having already seen this build's
// notes. A first-time user has no "new" to be told about — everything is new — and
// opening a changelog over somebody's first look at the app is the wrong welcome.
// Best-effort: failing to stamp only means they see the dialog once.
func (a *App) stampWhatsNewSeen(userID int64) {
	s := a.userSettingsFor(userID)
	s.WhatsNewSeen = appVersion
	a.saveUserSettings(userID, s)
}
