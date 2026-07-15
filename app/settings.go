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
	return s
}

func (a *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	js, err := a.store.UserSettings(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load settings")
		return
	}
	s := defaultSettings()
	if js != "" {
		json.Unmarshal([]byte(js), &s) // a corrupt row degrades to the defaults
	}
	writeJSON(w, http.StatusOK, s.normalize())
}

func (a *App) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in UserSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s := in.normalize()
	js, err := json.Marshal(s)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to encode settings")
		return
	}
	if err := a.store.SetUserSettings(u.ID, string(js)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	writeJSON(w, http.StatusOK, s)
}
