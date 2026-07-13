package main

import "testing"

func TestUserSettingsNormalize(t *testing.T) {
	def := defaultSettings()

	if got := (UserSettings{}).normalize(); got != def {
		t.Fatalf("empty settings = %+v, want defaults %+v", got, def)
	}
	if got := (UserSettings{TerminalMode: "floaty", Theme: "chartreuse"}).normalize(); got != def {
		t.Fatalf("junk settings = %+v, want defaults %+v", got, def)
	}
	want := UserSettings{TerminalMode: TerminalUndocked, Theme: "forest"}
	if got := want.normalize(); got != want {
		t.Fatalf("valid settings = %+v, want %+v (unchanged)", got, want)
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
