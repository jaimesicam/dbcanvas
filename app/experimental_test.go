package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Off is the default, and that is the property worth pinning: a feature tagged
// experimental lands hidden, and an installation that has never heard of this
// variable never sees one.
func TestExperimentalDefaultsOff(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	if experimentalEnabled() {
		t.Error("unset EXPERIMENTAL should be off")
	}
	for _, off := range []string{"off", "OFF", "false", "0", "no", " ", "maybe", "onish"} {
		t.Setenv(experimentalEnv, off)
		if experimentalEnabled() {
			t.Errorf("EXPERIMENTAL=%q should be off", off)
		}
	}
	// A boolean environment variable attracts every spelling of yes, and a feature
	// that silently did not turn on is a worse answer than accepting them.
	for _, on := range []string{"on", "ON", " On ", "true", "1", "yes", "y", "enabled"} {
		t.Setenv(experimentalEnv, on)
		if !experimentalEnabled() {
			t.Errorf("EXPERIMENTAL=%q should be on", on)
		}
	}
}

// The UI decides what to hide from this one field, so it has to ride on the payload
// every client already fetches — and it has to come from the environment on every
// read, not from anything a client sent back.
func TestSystemSettingsCarryExperimental(t *testing.T) {
	app := newTestApp(t)

	t.Setenv(experimentalEnv, "")
	if app.systemSettings("jaime").Experimental {
		t.Error("settings claim experimental features are on with EXPERIMENTAL unset")
	}
	t.Setenv(experimentalEnv, "on")
	if !app.systemSettings("jaime").Experimental {
		t.Error("settings do not carry EXPERIMENTAL=on")
	}

	// An admin's PUT sends the whole settings object back, this field included.
	// Nothing a client says about it may be stored or believed: the value on the
	// way out is re-derived, so a client cannot switch the feature on for everyone
	// by echoing true, nor off by echoing false.
	t.Setenv(experimentalEnv, "off")
	body := `{"maxUploadBytes":1073741824,"maxTokenDays":30,"experimental":true}`
	r := httptest.NewRequest("PUT", "/api/system/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	app.handleUpdateSystemSettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT returned %d: %s", w.Code, w.Body.String())
	}
	var out SystemSettings
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Experimental {
		t.Error("a client's `experimental: true` was echoed back as if it were the setting")
	}
	if app.systemSettings("").Experimental {
		t.Error("a client's claim reached the stored settings")
	}
}
