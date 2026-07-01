package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Notifications — persisted events surfaced in the top-right bell, delivered live over SSE.
// A user sees events for stacks they own (user_id == them); admins see everything.

type Notification struct {
	ID        int64   `json:"id"`
	UserID    int64   `json:"userId"`
	Scope     string  `json:"scope"` // user | admin
	Type      string  `json:"type"`
	Severity  string  `json:"severity"` // info | success | warning | error
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	StackID   int64   `json:"stackId,omitempty"`
	NodeID    string  `json:"nodeId,omitempty"`
	JobID     string  `json:"jobId,omitempty"`
	ReadAt    *string `json:"readAt,omitempty"`
	CreatedAt string  `json:"createdAt"`
}

// ------------------------------------------------------------- store methods

func (s *Store) CreateNotification(n Notification) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO notifications
	  (user_id,scope,type,severity,title,body,stack_id,node_id,job_id,created_at)
	  VALUES (?,?,?,?,?,?,?,?,?,?)`,
		n.UserID, n.Scope, n.Type, n.Severity, n.Title, n.Body,
		nfInt(n.StackID), nfStr(n.NodeID), nfStr(n.JobID), n.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListNotifications(userID int64, isAdmin bool, limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id,user_id,scope,type,severity,title,COALESCE(body,''),
	  COALESCE(stack_id,0),COALESCE(node_id,''),COALESCE(job_id,''),read_at,created_at
	  FROM notifications`
	var rows *sql.Rows
	var err error
	if isAdmin {
		rows, err = s.db.Query(q+" ORDER BY id DESC LIMIT ?", limit)
	} else {
		rows, err = s.db.Query(q+" WHERE user_id=? ORDER BY id DESC LIMIT ?", userID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var read sql.NullString
		if err := rows.Scan(&n.ID, &n.UserID, &n.Scope, &n.Type, &n.Severity, &n.Title, &n.Body,
			&n.StackID, &n.NodeID, &n.JobID, &read, &n.CreatedAt); err != nil {
			return nil, err
		}
		if read.Valid {
			n.ReadAt = &read.String
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Store) CountUnread(userID int64, isAdmin bool) (int, error) {
	var n int
	var err error
	if isAdmin {
		err = s.db.QueryRow("SELECT COUNT(*) FROM notifications WHERE read_at IS NULL").Scan(&n)
	} else {
		err = s.db.QueryRow("SELECT COUNT(*) FROM notifications WHERE read_at IS NULL AND user_id=?", userID).Scan(&n)
	}
	return n, err
}

func (s *Store) MarkNotificationRead(id, userID int64, isAdmin bool) error {
	now := nowRFC3339()
	if isAdmin {
		_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE id=? AND read_at IS NULL", now, id)
		return err
	}
	_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE id=? AND user_id=? AND read_at IS NULL", now, id, userID)
	return err
}

func (s *Store) MarkAllRead(userID int64, isAdmin bool) error {
	now := nowRFC3339()
	if isAdmin {
		_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE read_at IS NULL", now)
		return err
	}
	_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE read_at IS NULL AND user_id=?", now, userID)
	return err
}

func nfInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func nfStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ------------------------------------------------------------- SSE hub

type notifSub struct {
	ch      chan Notification
	userID  int64
	isAdmin bool
}

type notifHub struct {
	mu   sync.Mutex
	subs map[*notifSub]struct{}
}

var notifBus = &notifHub{subs: map[*notifSub]struct{}{}}

func (h *notifHub) subscribe(userID int64, isAdmin bool) *notifSub {
	s := &notifSub{ch: make(chan Notification, 32), userID: userID, isAdmin: isAdmin}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *notifHub) unsubscribe(s *notifSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *notifHub) publish(n Notification) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.isAdmin || s.userID == n.UserID {
			select {
			case s.ch <- n:
			default: // slow client — drop; it will re-fetch on next open
			}
		}
	}
}

// ------------------------------------------------------------- emit helpers

// notify persists a notification and pushes it to live SSE subscribers.
func (a *App) notify(n Notification) {
	if n.Scope == "" {
		n.Scope = "user"
	}
	if n.Severity == "" {
		n.Severity = "info"
	}
	n.CreatedAt = nowRFC3339()
	id, err := a.store.CreateNotification(n)
	if err != nil {
		log.Printf("notify: %v", err)
		return
	}
	n.ID = id
	notifBus.publish(n)
}

// notifyStack emits a stack-scoped notification to the stack's owner.
func (a *App) notifyStack(stackID int64, typ, severity, title, body, nodeID string) {
	var owner int64
	if st, err := a.store.GetStack(stackID); err == nil {
		owner = st.OwnerID
	}
	a.notify(Notification{UserID: owner, Scope: "user", Type: typ, Severity: severity,
		Title: title, Body: body, StackID: stackID, NodeID: nodeID})
}

// ------------------------------------------------------------- HTTP

func (a *App) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	admin := u.Role == RoleAdmin
	items, err := a.store.ListNotifications(u.ID, admin, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unread, _ := a.store.CountUnread(u.ID, admin)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

func (a *App) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.store.MarkNotificationRead(id, u.ID, u.Role == RoleAdmin); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := a.store.MarkAllRead(u.ID, u.Role == RoleAdmin); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNotifStream streams notifications to the client over Server-Sent Events.
func (a *App) handleNotifStream(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := notifBus.subscribe(u.ID, u.Role == RoleAdmin)
	defer notifBus.unsubscribe(sub)
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
		case n := <-sub.ch:
			b, _ := json.Marshal(n)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}
