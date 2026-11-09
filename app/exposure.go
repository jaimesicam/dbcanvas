package main

import (
	"net"
	"strings"
)

// exposure.go — whether this installation is reachable beyond the machine it runs on, and what
// that means for guest sign-in.
//
// DBCanvas drives the Docker daemon and creates privileged containers, so an account that can
// deploy can do, on the host, whatever such a container can — in effect root. With guest sign-in
// on, "an account" is anybody who can open the sign-in page. On loopback that is the people with
// a shell on the host already; on any other interface it is everyone who can reach it. So while
// the installation is exposed, guest sign-in needs an administrator's explicit confirmation
// (settingGuestExposedAck), both to turn it on and on every guest sign-in.
//
// The host-side address is what decides it. In Docker the container always listens on 0.0.0.0
// and the publish binding (APP_HOST in .env) limits who reaches it; docker-compose.yml passes
// that binding in as PUBLISH_HOST. Run directly on the host, APP_HOST is the listen address
// itself.

const settingGuestExposedAck = "guestExposedAck"

// publishHost is the address DBCanvas is reachable on, as configured.
func publishHost() string {
	if h := strings.TrimSpace(envOr("PUBLISH_HOST", "")); h != "" {
		return h
	}
	return strings.TrimSpace(envOr("APP_HOST", "127.0.0.1"))
}

// isLoopbackHost reports whether a listen address is reachable from this machine only.
func isLoopbackHost(h string) bool {
	h = strings.Trim(strings.TrimSpace(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// exposed reports whether the installation is reachable beyond this machine.
func exposed() bool { return !isLoopbackHost(publishHost()) }

// guestExposedAck reports whether an administrator confirmed guest sign-in on an exposed
// installation.
func (a *App) guestExposedAck() bool {
	v, err := a.store.AppSetting(settingGuestExposedAck)
	return err == nil && v == "1"
}

// guestExposureBlocks is why guest sign-in may not happen now, or "".
func (a *App) guestExposureBlocks() string {
	if !exposed() || a.guestExposedAck() {
		return ""
	}
	return "DBCanvas is reachable on " + publishHost() + ", not only from this machine, and a guest can deploy " +
		"privileged containers on its Docker host — an administrator has to confirm guest sign-in for that " +
		"(guestExposedAck in Settings → Guest sign-in)"
}
