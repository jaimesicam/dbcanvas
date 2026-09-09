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
		Version: "0.0.3",
		Date:    "2026-09-09",
		Title:   "Replicate one Kubernetes cluster into another",
		Body: "Draw a link between two Kubernetes frames that both run the PXC operator, pick a " +
			"direction, and Deploy: the second cluster becomes a replica of the first. DBCanvas runs " +
			"Percona's own two procedures in the order they have to happen in — the source declares " +
			"the channel and exposes its database pods so a replica can reach them, both clusters " +
			"build at the same time, a backup of the source is taken and restored onto the replica, " +
			"and only then is the replica's channel attached. Attaching it any earlier points the " +
			"replica at binary logs the source has already purged. The server node's new Replication " +
			"tab says which end a cluster is and whether the channel is running; Deploy reconciles the " +
			"channel and never re-seeds, because a seed replaces the replica's data — that is its own " +
			"button. Start from the new two-cluster template.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "MClusterAdmin — a MongoDB administration panel",
		Body: "A node that runs MClusterAdmin, a third-party web panel for MongoDB: topology and " +
			"replica-set status, sharding and the balancer, current operations, slow queries with " +
			"explain, indexes and profiling, users and roles, and oplog stats. Its UI is published to " +
			"a host port like PMM's, so it opens straight from your browser. Every MongoDB node and " +
			"cluster now has an Add MClusterAdmin credentials tick, which creates the two accounts the " +
			"panel expects — one for everything it does, one read-only for the dashboards — with " +
			"upstream's own least-privilege roles, neither of which can drop a database.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "Big Hole — MongoDB FTDC in the browser",
		Body: "A node that runs Big Hole, a third-party viewer for MongoDB's diagnostic.data: drag a " +
			"folder — or a whole support tarball, however it is nested — onto the page and it decodes " +
			"and charts it, every metric it can find, with the replica set laid out so an election is " +
			"somewhere to land rather than something to hunt for. There is no backend at all: nothing " +
			"you open in it leaves your browser, and it talks to no database, no API, not even to " +
			"DBCanvas. Open it on localhost — browsers only grant a page the on-disk storage it needs " +
			"there, and the node's panel hands you the ssh -L line when you are browsing from elsewhere.",
		Doc: "docs/STACKS.md",
	},
	{
		Version: "0.0.3",
		Date:    "2026-09-08",
		Title:   "Fixes across the app",
		Body: "A MongoDB node's log and its diagnostic.data are now one download, arriving together " +
			"under a directory named after the node instead of as two files that collide. The Core " +
			"Dump Analyzer shows the whole source file rather than a window around the crashing line, " +
			"and hands back the gdb command line that reproduces the session in your own terminal. A " +
			"long menu item wraps instead of being cut off mid-word. Three sidebar icons that were " +
			"drawing the wrong thing were redrawn. The Intranet image is built by make images, where " +
			"it belongs. And the API page now teaches the CLI — download it, put it on your PATH, sign " +
			"in, and read every command — rather than sending you elsewhere to find out how.",
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
