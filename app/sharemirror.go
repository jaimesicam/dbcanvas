package main

import (
	"encoding/json"
	"time"
)

// sharemirror.go — "Mirror everything": the driver's screen, as it is, on everyone's.
//
// Following (sharehub.go's follow) moves everyone to the same page, stack and panel,
// but each browser still draws its own copy, so whatever is not followed state — an
// open context menu, a window half-way through a drag, a dialog, a hover, the text
// being typed — is the driver's alone, and a guest loses track of what they are
// being shown. Mirroring sends the screen instead of the state: the driver's browser
// records its own page (rrweb: a snapshot of the DOM, then every change to it, the
// pointer, scrolls and inputs) and everyone else replays that stream, read-only, in
// place of their own workspace (web/src/session/Mirror.jsx).
//
// The hub only relays. It keeps nothing, so a browser arriving late — or one whose
// replay lost its place — asks for a resync, and the driver answers with a fresh
// full snapshot. Only the driver's stream is relayed; anyone else's is dropped.
//
// What a guest may not see stays out of the stream at its source: the driver's
// browser blocks the host-only pages and, with Hide secrets on, masks every secret
// (session/mirrorRecord.js). A recorded page is inert — rrweb strips scripts — and
// the replay has no pointer events, so a viewer can look and not touch.

const (
	shareMirrorMax    = 16 << 20 // one batch of events; a first snapshot of a big page is a few MB
	shareResyncMinGap = time.Second
)

// setMirror turns mirroring on or off for everyone.
func (h *shareHub) setMirror(on bool) {
	h.mu.Lock()
	if h.ended || h.mirror == on {
		h.mu.Unlock()
		return
	}
	h.mirror = on
	h.sess.Mirror = on
	h.mu.Unlock()
	if on {
		h.event("mirror", 0, "", h.sess.HostName+" turned on Mirror everything: everyone sees exactly the driver's screen")
	} else {
		h.event("mirror", 0, "", h.sess.HostName+" turned off Mirror everything")
	}
	h.broadcastPresence()
}

// requestResync asks the driver's browsers for a fresh snapshot, at most once a
// second however many viewers ask.
func (h *shareHub) requestResync() {
	h.mu.Lock()
	if !h.mirror || time.Since(h.lastResync) < shareResyncMinGap {
		h.mu.Unlock()
		return
	}
	h.lastResync = time.Now()
	drivers := []*hubClient{}
	for c := range h.clients {
		if c.guestID == h.controller {
			drivers = append(drivers, c)
		}
	}
	h.mu.Unlock()
	for _, c := range drivers {
		h.send(c, map[string]any{"t": "mirror-resync"})
	}
}

// relayMirror passes one batch of the driver's recording to everyone else.
func (h *shareHub) relayMirror(c *hubClient, driving bool, data json.RawMessage) {
	h.mu.Lock()
	on := h.mirror
	h.mu.Unlock()
	if !on || !driving || len(data) == 0 || len(data) > shareMirrorMax {
		return
	}
	h.broadcast(map[string]any{"t": "mirror", "data": data, "from": c.guestID}, c)
}
