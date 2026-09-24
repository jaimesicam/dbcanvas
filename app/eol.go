package main

import (
	"os"
	"strings"
)

// eol.go — EOL: the switch that decides whether software past its end of life is offered.
//
// A support lab gets asked about releases the vendor stopped supporting years ago: the customer
// still runs CentOS 7, or PMM 2, and the question is about *their* system, not the one they should
// be on. Those releases can be deployed — the images still pull, and the package archives are
// frozen rather than gone — but they get no updates, some of their mirrors are dead, and a design
// that picks one by accident is a design that will not be fixed when it breaks. So they are behind
// a switch, off by default, and turned on by an installation that has a reason to want them.
//
// What it covers today:
//
//   - CentOS 7 as a Linux Client release (linuxclient_el7.go), from the stock centos:7 image with
//     its repositories pointed at vault.centos.org.
//   - PMM 2 as a monitoring node of its own (pmm2.go), deployed at a pinned version and nothing
//     more.
//
// Like EXPERIMENTAL (experimental.go) it is a presentation switch, deliberately. It decides what
// the pickers and the node library offer; it does not refuse an API call or touch a stack that
// already has an end-of-life node on its canvas. Turning it off must never break what somebody
// already built with it on.

// eolEnv is the variable, and "off" is what it is when nobody has set it.
const eolEnv = "EOL"

// eolEnabled reports whether end-of-life releases are offered. Same spellings as
// experimentalEnabled, for the same reason: refusing a plausible way of writing "on" is just a
// switch that silently did not turn on.
func eolEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(eolEnv))) {
	case "on", "true", "1", "yes", "y", "enabled":
		return true
	default:
		return false
	}
}
