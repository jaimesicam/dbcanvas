package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// stackevents.go — how a program learns what happened to a stack without polling for it.
//
// Two parts:
//
//   - Tombstones. Deleting a stack leaves a row saying when, why and by whom: "user" (its owner),
//     "admin" (an administrator), "account" (its account was deleted), "guest-idle" (an idle guest
//     account was removed). A request for the stack's id then answers 410 Gone with that record
//     instead of a bare 404, so "it is gone" is never confused with "it never existed" or "the id
//     is wrong". Kept 30 days. A stack whose TTL ran out is not deleted — it stays, status
//     "expired", torn down — so it needs no tombstone.
//
//   - GET /api/stacks/events, a server-sent event stream of stack lifecycle changes: created,
//     deploying, destroyed, deleted, expired, extended, labels. An account sees its own stacks
//     (an administrator every stack, with ?all=1), a limited token only those within its limit,
//     and ?label=k=v narrows it as on the list.

// StackEvent is one lifecycle change.
type StackEvent struct {
	Type    string            `json:"type"` // created | deploying | destroyed | deleted | expired | extended | labels
	StackID int64             `json:"stackId"`
	Name    string            `json:"name"`
	OwnerID int64             `json:"ownerId"`
	Labels  map[string]string `json:"labels,omitempty"`
	Reason  string            `json:"reason,omitempty"` // deleted: user | admin | account | guest-idle
	By      string            `json:"by,omitempty"`     // the account that did it, when one did
	At      string            `json:"at"`
	// createdByToken travels for the token-limit filter and is not sent.
	createdByToken int64
}

// stackEventBus fans events out to the open streams. A slow reader drops events rather than
// holding the publisher up; it can always re-read the stack list.
type stackEventBus struct {
	mu   sync.Mutex
	subs map[chan StackEvent]bool
}

var stackEvents = &stackEventBus{subs: map[chan StackEvent]bool{}}

func (b *stackEventBus) subscribe() chan StackEvent {
	ch := make(chan StackEvent, 64)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch
}

func (b *stackEventBus) unsubscribe(ch chan StackEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *stackEventBus) publish(e StackEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// stackEvent publishes a change to a stack.
func (a *App) stackEvent(st Stack, typ, reason, by string) {
	stackEvents.publish(StackEvent{
		Type: typ, StackID: st.ID, Name: st.Name, OwnerID: st.OwnerID, Labels: st.Labels,
		Reason: reason, By: by, At: nowRFC3339(), createdByToken: st.CreatedByToken,
	})
}

// stackEventByID publishes a change to a stack known by id.
func (a *App) stackEventByID(id int64, typ string) {
	if st, err := a.store.GetStack(id); err == nil {
		a.stackEvent(st, typ, "", "")
	}
}

// deleteStackRecord deletes a stack's row, leaving a tombstone and publishing the deletion. The
// caller has torn its containers down.
func (a *App) deleteStackRecord(st Stack, reason, by string) error {
	if err := a.store.DeleteStack(st.ID); err != nil {
		return err
	}
	a.store.AddStackTombstone(st, reason, by)
	a.store.DeleteAppSetting(dnsCustomKey(st.ID))
	a.stackEvent(st, "deleted", reason, by)
	return nil
}

// StackTombstone is what is left of a deleted stack.
type StackTombstone struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	OwnerID   int64             `json:"ownerId"`
	Labels    map[string]string `json:"labels,omitempty"`
	Reason    string            `json:"reason"`
	By        string            `json:"by,omitempty"`
	DeletedAt string            `json:"deletedAt"`
}

const stackTombstoneDays = 30

// AddStackTombstone records a deleted stack.
func (s *Store) AddStackTombstone(st Stack, reason, by string) {
	var labels any
	if len(st.Labels) > 0 {
		b, _ := json.Marshal(st.Labels)
		labels = string(b)
	}
	s.db.Exec(`INSERT OR REPLACE INTO stack_tombstones (id, name, owner_id, labels_json, reason, by_user, deleted_at)
		VALUES (?,?,?,?,?,?,?)`, st.ID, st.Name, st.OwnerID, labels, reason, by, nowRFC3339())
}

// StackTombstone finds a deleted stack's record.
func (s *Store) StackTombstone(id int64) (StackTombstone, error) {
	var t StackTombstone
	var labels sql.NullString
	err := s.db.QueryRow(`SELECT id, name, owner_id, labels_json, reason, by_user, deleted_at FROM stack_tombstones WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.OwnerID, &labels, &t.Reason, &t.By, &t.DeletedAt)
	t.Labels = decodeLabels(labels)
	return t, err
}

// PurgeStackTombstones drops records older than the retention.
func (s *Store) PurgeStackTombstones() {
	cut := time.Now().Add(-stackTombstoneDays * 24 * time.Hour).UTC().Format(time.RFC3339)
	s.db.Exec("DELETE FROM stack_tombstones WHERE deleted_at < ?", cut)
}

// writeStackGone answers a request for a deleted stack: 410 with its tombstone, to its owner or
// an administrator. Reports whether it answered.
func (a *App) writeStackGone(w http.ResponseWriter, u User, id int64) bool {
	t, err := a.store.StackTombstone(id)
	if err != nil || (t.OwnerID != u.ID && u.Role != RoleAdmin) {
		return false
	}
	msg := fmt.Sprintf("stack %d (%s) was deleted %s", id, t.Name, t.DeletedAt)
	switch t.Reason {
	case "user":
		msg += " by its owner"
	case "admin":
		msg += " by an administrator"
	case "account":
		msg += ", with the account that owned it"
	case "guest-idle":
		msg += ", with its guest account, after it went unused"
	}
	writeJSON(w, http.StatusGone, map[string]any{"error": msg, "deleted": t})
	return true
}

// handleStackEvents is GET /api/stacks/events.
func (a *App) handleStackEvents(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	sel, err := parseLabelSelector(r.URL.Query()["label"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	all := u.Role == RoleAdmin && r.URL.Query().Get("all") == "1"
	limit, tokID := limitOf(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := stackEvents.subscribe()
	defer stackEvents.unsubscribe(ch)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			if !all && e.OwnerID != u.ID {
				continue
			}
			if !matchesLabels(e.Labels, sel) {
				continue
			}
			if limit != nil && !limit.allowsStack(tokID, Stack{Labels: e.Labels, CreatedByToken: e.createdByToken}) {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			fl.Flush()
		}
	}
}
