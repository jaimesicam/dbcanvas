package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
)

// sharedraw.go — drawing on the shared screen.
//
// Anyone in a session may draw on what everyone is looking at: a freehand line, or a
// line of text dropped at a point. The marks are the hub's, in memory like the rest
// of a session's live state: a browser arriving late gets them all with its hello,
// and they are gone when the session ends (a recording keeps them, sharerecord.go).
//
// A mark's coordinates are fractions of the frame it was drawn on — the workspace,
// or the driver's workspace inside a mirror — so it lands on the same spot of a
// screen of another size; its width and text size are fractions of the frame's
// height, so it keeps its weight too.
//
// Who may draw is the host's to say: they always may; guests may unless the host
// turned drawing off for everyone (share_sessions.draw) or for them (share_guests.
// draw_off). Erasing follows ownership: a guest erases their own marks, the host
// anyone's, and only the host clears the whole screen.

const (
	shareMarksMax  = 1000 // marks on the screen at once
	shareMarkPts   = 4000 // points in one line
	shareMarkText  = 500  // characters in one text
	shareMarkBytes = 96 << 10
)

// shareMark is one thing drawn. By is who drew it (0 = the host), set by the hub,
// never taken from the browser.
type shareMark struct {
	ID     string       `json:"id"`
	Kind   string       `json:"kind"` // pen | text
	By     int64        `json:"by"`
	Name   string       `json:"name"`
	Color  string       `json:"color"`
	Width  float64      `json:"width,omitempty"` // pen: a fraction of the frame's height
	Points [][2]float64 `json:"points,omitempty"`
	X      float64      `json:"x,omitempty"`
	Y      float64      `json:"y,omitempty"`
	Size   float64      `json:"size,omitempty"` // text: a fraction of the frame's height
	Text   string       `json:"text,omitempty"`
}

// shareMarkColors are the pens on offer; anything else is refused, so a mark is
// never a vehicle for CSS.
var shareMarkColors = map[string]bool{
	"#ef4444": true, "#f59e0b": true, "#22c55e": true, "#3b82f6": true,
	"#a855f7": true, "#ec4899": true, "#111827": true, "#ffffff": true,
}

func inUnit(v float64) bool { return !math.IsNaN(v) && v >= -0.05 && v <= 1.05 }

// valid checks a mark as the browser sent it.
func (m *shareMark) valid() bool {
	if m.ID == "" || len(m.ID) > 64 || !shareMarkColors[m.Color] {
		return false
	}
	switch m.Kind {
	case "pen":
		if len(m.Points) == 0 || len(m.Points) > shareMarkPts || !(m.Width > 0 && m.Width <= 0.05) {
			return false
		}
		for _, p := range m.Points {
			if !inUnit(p[0]) || !inUnit(p[1]) {
				return false
			}
		}
		m.Text, m.X, m.Y, m.Size = "", 0, 0, 0
	case "text":
		m.Text = strings.TrimSpace(m.Text)
		if m.Text == "" || len([]rune(m.Text)) > shareMarkText || !inUnit(m.X) || !inUnit(m.Y) || !(m.Size > 0 && m.Size <= 0.2) {
			return false
		}
		m.Points, m.Width = nil, 0
	default:
		return false
	}
	return true
}

// mayDraw is whether this browser may draw or erase right now.
func (h *shareHub) mayDraw(c *hubClient) bool {
	if c.isHost() {
		return true
	}
	h.mu.Lock()
	all := h.sess.GuestsDraw
	h.mu.Unlock()
	if !all {
		return false
	}
	g, err := h.app.store.GetShareGuest(c.guestID)
	return err == nil && g.State == guestAdmitted && !g.DrawOff
}

// markList is every mark, oldest first. Callers hold h.mu.
func (h *shareHub) markList() []*shareMark {
	out := make([]*shareMark, 0, len(h.markOrder))
	for _, id := range h.markOrder {
		if m := h.marks[id]; m != nil {
			out = append(out, m)
		}
	}
	return out
}

// handleDraw is one drawing message: draw (a new mark, or a line still being drawn,
// sent again as it grows), draw-erase, or draw-clear.
func (h *shareHub) handleDraw(c *hubClient, t string, data json.RawMessage) {
	if len(data) > shareMarkBytes {
		return
	}
	if !h.mayDraw(c) {
		h.send(c, map[string]any{"t": "error", "error": "the host has turned drawing off for you"})
		return
	}
	switch t {
	case "draw":
		var m shareMark
		if json.Unmarshal(data, &m) != nil || !m.valid() {
			return
		}
		m.By, m.Name = c.guestID, c.name
		h.mu.Lock()
		if h.ended {
			h.mu.Unlock()
			return
		}
		if cur := h.marks[m.ID]; cur != nil {
			if cur.By != c.guestID { // someone else's id: not theirs to redraw
				h.mu.Unlock()
				return
			}
		} else {
			if len(h.marks) >= shareMarksMax {
				h.mu.Unlock()
				h.send(c, map[string]any{"t": "error", "error": "the screen is full of drawings — erase some first"})
				return
			}
			h.markOrder = append(h.markOrder, m.ID)
		}
		h.marks[m.ID] = &m
		h.mu.Unlock()
		h.broadcast(map[string]any{"t": "draw", "mark": m}, c)
	case "draw-erase":
		var in struct {
			IDs []string `json:"ids"`
		}
		if json.Unmarshal(data, &in) != nil || len(in.IDs) == 0 || len(in.IDs) > shareMarksMax {
			return
		}
		gone := []string{}
		h.mu.Lock()
		for _, id := range in.IDs {
			if m := h.marks[id]; m != nil && (c.isHost() || m.By == c.guestID) {
				delete(h.marks, id)
				gone = append(gone, id)
			}
		}
		h.compactMarks()
		h.mu.Unlock()
		if len(gone) > 0 {
			h.broadcast(map[string]any{"t": "draw-erase", "ids": gone}, nil)
		}
	case "draw-clear":
		// {"all": true} is the host wiping the screen; anything else clears your own.
		var in struct {
			All bool `json:"all"`
		}
		json.Unmarshal(data, &in)
		all := in.All && c.isHost()
		gone := []string{}
		h.mu.Lock()
		for id, m := range h.marks {
			if all || m.By == c.guestID {
				delete(h.marks, id)
				gone = append(gone, id)
			}
		}
		h.compactMarks()
		h.mu.Unlock()
		if all {
			h.event("draw", 0, "", h.sess.HostName+" cleared the drawings")
		}
		if len(gone) > 0 {
			h.broadcast(map[string]any{"t": "draw-erase", "ids": gone}, nil)
		}
	}
}

// compactMarks drops erased ids from the order. Callers hold h.mu.
func (h *shareHub) compactMarks() {
	keep := h.markOrder[:0]
	for _, id := range h.markOrder {
		if h.marks[id] != nil {
			keep = append(keep, id)
		}
	}
	h.markOrder = keep
}

// setGuestsDraw turns drawing on or off for every guest.
func (h *shareHub) setGuestsDraw(on bool) {
	h.mu.Lock()
	if h.ended || h.sess.GuestsDraw == on {
		h.mu.Unlock()
		return
	}
	h.sess.GuestsDraw = on
	h.mu.Unlock()
	if on {
		h.event("draw", 0, "", h.sess.HostName+" let guests draw on the screen")
	} else {
		h.event("draw", 0, "", h.sess.HostName+" turned drawing off for guests")
	}
	h.broadcastPresence()
}

// handleShareDraw is the host turning drawing on or off for every guest:
// {"draw": false}.
func (a *App) handleShareDraw(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	var in struct {
		Draw bool `json:"draw"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	h := a.hubFor(sess) // before the row changes, so the hub sees it change
	if err := a.store.SetShareSessionDraw(sess.ID, in.Draw); err != nil {
		writeErr(w, http.StatusConflict, "this session has ended")
		return
	}
	h.setGuestsDraw(in.Draw)
	writeJSON(w, http.StatusOK, map[string]any{"draw": in.Draw})
}
