package main

import (
	"os"
	"strings"
)

// experimental.go — EXPERIMENTAL: the switch that decides whether the parts of
// DBCanvas that are not finished yet appear at all.
//
// Some features ship before they are ready — Labs writes its scenarios with an LLM
// and the Unoptimized MySQL Challenge is a teaching toy that is still moving. They
// were marked "(experimental)" in their own labels, which is a warning and not a
// choice: an installation that did not want them still had them in its menus, and
// the only way to hold a new feature back was to not merge it.
//
// So the label becomes a flag. Anything not ready is TAGGED experimental — in the
// UI, next to the thing itself (NAV in App.jsx, the node palette in
// StackDesigner.jsx) — and this decides whether tagged things are shown. Off by
// default, which is the useful default for the switch to have: a feature can land
// hidden and be turned on by the people who want to try it, rather than waiting for
// a release.
//
// It is a presentation switch, deliberately. It hides what is not ready from the
// menus; it does not delete anything, refuse an API call, or touch a stack that
// already has an experimental node on its canvas. Turning it off must never break
// what somebody already built with it on.

// experimentalEnv is the variable, and "off" is what it is when nobody has set it.
const experimentalEnv = "EXPERIMENTAL"

// experimentalEnabled reports whether experimental features are shown.
//
// The documented value is on/off, because that is what the .env comment offers and
// what an operator types; true/1/yes/enabled are accepted too, since a boolean
// environment variable attracts all of them and refusing a plausible spelling is
// just a feature that silently did not turn on. Anything else — including an empty
// or unset variable — is off.
func experimentalEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(experimentalEnv))) {
	case "on", "true", "1", "yes", "y", "enabled":
		return true
	default:
		return false
	}
}
