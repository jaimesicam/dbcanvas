package main

import (
	"encoding/json"
	"net/http"
)

// settings.go — per-user UI preferences, stored as JSON on the users row and served to the
// signed-in account (not the browser): they follow the user across browsers and machines.
// Unknown/invalid values fall back to the defaults, so a hand-edited row can never wedge the UI.

// terminalMode values: where a node console opens.
const (
	TerminalDocked   = "docked"   // a tab in the bottom dock (default)
	TerminalUndocked = "undocked" // its own floating window
)

// deploymentBackend values: how a stack's nodes are provisioned when the user deploys.
const (
	BackendDocker  = "docker"  // Docker containers on the local daemon (default)
	BackendVagrant = "vagrant" // VirtualBox VMs driven by Vagrant (see vagrant.go)
)

// nodeLibrary values: how nodes get added to the stack canvas.
const (
	LibraryMenu   = "menu"   // right-click the canvas, add at the pointer (default)
	LibraryDocked = "docked" // the Infrastructure Library column, docked left
)

// tooltips values: whether the hover explanations are shown at all.
//
// A string from a fixed set, like every other choice on this row, and here that shape
// earns its keep rather than merely matching: the default is ON, so a bool would make
// the zero value mean "somebody turned them off" — indistinguishable from a client
// that has never heard of the field, a row written before it existed, or a corrupt
// one, any of which would silently strip the explanations off a UI full of decisions.
// An empty string normalises to on, which is what all three of those actually mean.
const (
	TooltipsOn  = "on"  // the "?" bubbles explain each control (default)
	TooltipsOff = "off" // no bubbles, and no "?" buttons left behind either
)

// terminalsOnTop values: whether terminal and file manager windows stay in front of
// every other window. The name predates the file managers joining in; it is kept so a
// stored setting carries over.
//
// A string for the same reason as tooltips: the default is ON, so an empty value (an
// older client, a row written before the field existed) has to mean on, not off.
// While on, touching a page or browser window gives it focus but does not raise it over
// a terminal or a file manager — those, and the terminal dock, only leave the front when
// they are minimized (or the dock is hidden). Among themselves they still stack by
// whichever was touched last.
const (
	TerminalsOnTopOn  = "on"  // terminals and file managers stay above other windows (default)
	TerminalsOnTopOff = "off" // they stack like every other window
)

// scratchboard values: whether the Scratchboard may keep boards on the server
// (scratchboard.go). Off by default — what is pasted there is often a certificate, a
// Secret or a connection string — so here the empty string means off, which is also
// what an older client or a row written before the field existed should mean.
// Turning it off deletes every board the account kept.
const (
	ScratchboardStoreOff = "off" // boards live in the browser tab only (default)
	ScratchboardStoreOn  = "on"  // boards marked to be kept are saved to the account
)

// themes recognised by the web UI (theme/ThemeProvider.jsx — keep in sync).
var validThemes = map[string]bool{
	"glass-dark": true, "glass-light": true,
	"light": true, "dark": true, "midnight": true,
	"solarized": true, "synthwave": true, "forest": true,
}

// maxTabs bounds how many pages the tabbed main window keeps open at once. Every
// open tab is a live component tree in the browser, so this is a memory budget
// rather than a preference — hence a ceiling on what anyone may ask for, not a
// free number. Mirrored in web/src/lib/tabs.js; keep the three in sync.
const (
	defaultMaxTabs = 20
	minMaxTabs     = 2
	maxMaxTabs     = 40
)

// looks recognised by the web UI (theme/ThemeProvider.jsx — keep in sync).
// A look is the shape of the UI — corner radius, border weight, elevation,
// density, heading typeface — and is chosen independently of the theme, which
// is only its colour. Every look works in every theme.
var validLooks = map[string]bool{
	"glass": true, "modern": true, "industrial": true, "editorial": true, "soft": true,
}

// UserSettings is a user's UI preferences. Defaults apply to every field that is missing or
// invalid, so the zero value is never served.
type UserSettings struct {
	TerminalMode      string `json:"terminalMode"`      // docked | undocked
	Theme             string `json:"theme"`             // one of validThemes
	Look              string `json:"look"`              // one of validLooks
	DeploymentBackend string `json:"deploymentBackend"` // docker | vagrant
	NodeLibrary       string `json:"nodeLibrary"`       // menu | docked
	Tooltips          string `json:"tooltips"`          // on | off
	TerminalsOnTop    string `json:"terminalsOnTop"`    // on | off
	MaxTabs           int    `json:"maxTabs"`           // between minMaxTabs and maxMaxTabs
	Scratchboard      string `json:"scratchboard"`      // off | on
	// WhatsNewSeen is the highest release whose notes this account has dismissed
	// (see whatsnew.go). Free-form on purpose: it holds whatever appVersion was,
	// including "dev", and normalize() must leave it exactly as it found it.
	WhatsNewSeen string `json:"whatsNewSeen,omitempty"`
}

func defaultSettings() UserSettings {
	return UserSettings{
		TerminalMode: TerminalDocked, Theme: "glass-light", Look: "glass",
		DeploymentBackend: BackendDocker, NodeLibrary: LibraryMenu,
		Tooltips: TooltipsOn, TerminalsOnTop: TerminalsOnTopOn, MaxTabs: defaultMaxTabs,
		Scratchboard: ScratchboardStoreOff,
	}
}

// normalize replaces unrecognised values with the defaults.
func (s UserSettings) normalize() UserSettings {
	def := defaultSettings()
	if s.TerminalMode != TerminalDocked && s.TerminalMode != TerminalUndocked {
		s.TerminalMode = def.TerminalMode
	}
	if !validThemes[s.Theme] {
		s.Theme = def.Theme
	}
	if !validLooks[s.Look] {
		s.Look = def.Look
	}
	if s.DeploymentBackend != BackendDocker && s.DeploymentBackend != BackendVagrant {
		s.DeploymentBackend = def.DeploymentBackend
	}
	if s.NodeLibrary != LibraryMenu && s.NodeLibrary != LibraryDocked {
		s.NodeLibrary = def.NodeLibrary
	}
	if s.Tooltips != TooltipsOn && s.Tooltips != TooltipsOff {
		s.Tooltips = def.Tooltips
	}
	if s.TerminalsOnTop != TerminalsOnTopOn && s.TerminalsOnTop != TerminalsOnTopOff {
		s.TerminalsOnTop = def.TerminalsOnTop
	}
	if s.Scratchboard != ScratchboardStoreOn {
		s.Scratchboard = ScratchboardStoreOff
	}
	// Clamped rather than reset, unlike the fields above: those are values from a
	// fixed set, where anything else is meaningless, but a number slightly out of
	// range still says what the user meant. Zero is the exception — that is a
	// client that does not know the field, not somebody asking for no tabs.
	switch {
	case s.MaxTabs <= 0:
		s.MaxTabs = def.MaxTabs
	case s.MaxTabs < minMaxTabs:
		s.MaxTabs = minMaxTabs
	case s.MaxTabs > maxMaxTabs:
		s.MaxTabs = maxMaxTabs
	}
	// WhatsNewSeen is deliberately not normalized. It is a version string, not a
	// value from a fixed set, and every other field in here is clamped to one — so
	// the obvious next edit to this function is to clamp this too, which would
	// silently re-open the release notes for everybody on every save.
	return s
}

// userSettingsFor loads an account's preferences, degrading to the defaults for a
// missing or corrupt row.
func (a *App) userSettingsFor(userID int64) UserSettings {
	s := defaultSettings()
	js, err := a.store.UserSettings(userID)
	if err != nil || js == "" {
		return s
	}
	json.Unmarshal([]byte(js), &s)
	return s.normalize()
}

// saveUserSettings persists an account's preferences.
func (a *App) saveUserSettings(userID int64, s UserSettings) error {
	js, err := json.Marshal(s.normalize())
	if err != nil {
		return err
	}
	return a.store.SetUserSettings(userID, string(js))
}

func (a *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, a.userSettingsFor(u.ID))
}

func (a *App) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Seeded from what is stored, so a client that PUTs a settings object without
	// whatsNewSeen (an older UI, or the CLI) cannot blank it and re-open the
	// release notes for that account.
	prev := a.userSettingsFor(u.ID)
	in := prev
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s := in.normalize()
	// Switching keeping off deletes what was kept, before the switch is saved, so a
	// failure leaves the setting on rather than leaving boards behind it.
	if prev.Scratchboard == ScratchboardStoreOn && s.Scratchboard != ScratchboardStoreOn {
		if err := a.store.DeleteScratchboards(u.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to delete the kept Scratchboard boards")
			return
		}
	}
	if err := a.saveUserSettings(u.ID, s); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	writeJSON(w, http.StatusOK, s)
}
