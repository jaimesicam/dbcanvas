package main

import "testing"

func TestUserSettingsNormalize(t *testing.T) {
	def := defaultSettings()

	if got := (UserSettings{}).normalize(); got != def {
		t.Fatalf("empty settings = %+v, want defaults %+v", got, def)
	}
	if got := (UserSettings{TerminalMode: "floaty", Theme: "chartreuse", Look: "brutalist", MaxTabs: 0}).normalize(); got != def {
		t.Fatalf("junk settings = %+v, want defaults %+v", got, def)
	}
	want := UserSettings{TerminalMode: TerminalUndocked, Theme: "forest", Look: "industrial", DeploymentBackend: BackendVagrant, NodeLibrary: LibraryDocked, MaxTabs: 12}
	if got := want.normalize(); got != want {
		t.Fatalf("valid settings = %+v, want %+v (unchanged)", got, want)
	}

	// A tab count out of range is clamped to the nearest bound rather than reset:
	// asking for 500 tabs means "as many as I can have", not "give me the default".
	if got := (UserSettings{MaxTabs: 500}).normalize().MaxTabs; got != maxMaxTabs {
		t.Errorf("MaxTabs 500 normalised to %d, want the ceiling %d", got, maxMaxTabs)
	}
	if got := (UserSettings{MaxTabs: 1}).normalize().MaxTabs; got != minMaxTabs {
		t.Errorf("MaxTabs 1 normalised to %d, want the floor %d", got, minMaxTabs)
	}
	// Zero is a client that does not know the field — it gets the default, and a
	// PUT without the field keeps what is stored (handleUpdateSettings seeds it).
	if got := (UserSettings{}).normalize().MaxTabs; got != defaultMaxTabs {
		t.Errorf("MaxTabs 0 normalised to %d, want the default %d", got, defaultMaxTabs)
	}
}

// A user's settings persist and are readable back; an untouched user has none.
func TestUserSettingsRoundTrip(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("jane", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	js, err := app.store.UserSettings(u.ID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if js != "" {
		t.Fatalf("new user settings = %q, want empty", js)
	}

	if err := app.store.SetUserSettings(u.ID, `{"terminalMode":"undocked","theme":"forest"}`); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	js, err = app.store.UserSettings(u.ID)
	if err != nil {
		t.Fatalf("re-read settings: %v", err)
	}
	if js != `{"terminalMode":"undocked","theme":"forest"}` {
		t.Fatalf("settings = %q, want the saved JSON", js)
	}
}
