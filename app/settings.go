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

// themes recognised by the web UI (theme/ThemeProvider.jsx — keep in sync).
var validThemes = map[string]bool{
	"light": true, "dark": true, "midnight": true,
	"solarized": true, "synthwave": true, "forest": true,
}

// UserSettings is a user's UI preferences. Defaults apply to every field that is missing or
// invalid, so the zero value is never served.
type UserSettings struct {
	TerminalMode      string `json:"terminalMode"`      // docked | undocked
	Theme             string `json:"theme"`             // one of validThemes
	DeploymentBackend string `json:"deploymentBackend"` // docker | vagrant
	// WhatsNewSeen is the highest release whose notes this account has dismissed
	// (see whatsnew.go). Free-form on purpose: it holds whatever appVersion was,
	// including "dev", and normalize() must leave it exactly as it found it.
	WhatsNewSeen string `json:"whatsNewSeen,omitempty"`
}

func defaultSettings() UserSettings {
	return UserSettings{TerminalMode: TerminalDocked, Theme: "dark", DeploymentBackend: BackendDocker}
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
	if s.DeploymentBackend != BackendDocker && s.DeploymentBackend != BackendVagrant {
		s.DeploymentBackend = def.DeploymentBackend
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
	in := a.userSettingsFor(u.ID)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s := in.normalize()
	if err := a.saveUserSettings(u.ID, s); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	writeJSON(w, http.StatusOK, s)
}
