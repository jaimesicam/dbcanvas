package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	for h, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.5": true, "localhost": true, "::1": true, "[::1]": true,
		"0.0.0.0": false, "192.168.1.10": false, "10.0.4.12": false, "": false, "::": false,
	} {
		if got := isLoopbackHost(h); got != want {
			t.Errorf("%q: %v", h, got)
		}
	}
}

func TestGuestSignInNeedsConfirmationWhenExposed(t *testing.T) {
	app := newTestApp(t)
	app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	t.Setenv("PUBLISH_HOST", "0.0.0.0")
	settings := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		app.handleUpdateSystemSettings(w, httptest.NewRequest("PUT", "/api/system/settings", strings.NewReader(body)))
		return w
	}
	if w := settings(`{"guestLogin":true}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "0.0.0.0") {
		t.Fatalf("turning guest sign-in on while exposed: %d %s", w.Code, w.Body)
	}
	if app.guestLoginEnabled() {
		t.Fatal("guest sign-in was switched on anyway")
	}
	// Turned on some other way (an older setting), a guest is still refused without the confirmation.
	app.store.SetAppSetting(settingGuestLogin, "1")
	w := httptest.NewRecorder()
	app.handleGuestLogin(w, httptest.NewRequest("POST", "/api/auth/guest", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("guest sign-in while exposed and unconfirmed: %d", w.Code)
	}
	if w := settings(`{"guestLogin":true,"guestExposedAck":true}`); w.Code != http.StatusOK {
		t.Fatalf("with the confirmation: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	app.handleGuestLogin(w, httptest.NewRequest("POST", "/api/auth/guest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("guest sign-in once confirmed: %d %s", w.Code, w.Body)
	}
	// On loopback nothing needs confirming.
	t.Setenv("PUBLISH_HOST", "127.0.0.1")
	app.store.SetAppSetting(settingGuestExposedAck, "0")
	if app.guestExposureBlocks() != "" {
		t.Fatal("loopback asked for a confirmation")
	}
}

// DBCanvas will not start reachable beyond this machine unless APP_HOST_OVERRIDE is on. The host
// address is the publish binding in Docker (PUBLISH_HOST), else APP_HOST.
func TestExposureNeedsTheOverride(t *testing.T) {
	cases := []struct {
		publish, appHost, override string
		refused                    bool
	}{
		{"127.0.0.1", "0.0.0.0", "", false}, // Docker: the container listens on 0.0.0.0, published on loopback
		{"", "127.0.0.1", "", false},
		{"", "::1", "off", false},
		{"0.0.0.0", "0.0.0.0", "", true},
		{"0.0.0.0", "0.0.0.0", "off", true},
		{"10.0.4.12", "0.0.0.0", "nope", true},
		{"", "0.0.0.0", "off", true}, // run on the host directly
		{"0.0.0.0", "0.0.0.0", "on", false},
		{"0.0.0.0", "0.0.0.0", "TRUE", false},
		{"", "192.168.1.10", "1", false},
	}
	for _, c := range cases {
		t.Setenv("PUBLISH_HOST", c.publish)
		t.Setenv("APP_HOST", c.appHost)
		t.Setenv("APP_HOST_OVERRIDE", c.override)
		why := exposureRefusal()
		if (why != "") != c.refused {
			t.Errorf("publish %q, APP_HOST %q, override %q: refusal %q, want refused=%v", c.publish, c.appHost, c.override, why, c.refused)
		}
		if why != "" && !strings.Contains(why, "APP_HOST_OVERRIDE") {
			t.Errorf("the refusal does not say how to allow it: %s", why)
		}
	}
}
