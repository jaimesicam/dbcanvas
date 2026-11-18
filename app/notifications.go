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

type Notification struct {
	ID        int64   `json:"id"`
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
	  (type,severity,title,body,stack_id,node_id,job_id,created_at)
	  VALUES (?,?,?,?,?,?,?,?)`,
		n.Type, n.Severity, n.Title, n.Body,
		nfInt(n.StackID), nfStr(n.NodeID), nfStr(n.JobID), n.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListNotifications(limit int) ([]Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,type,severity,title,COALESCE(body,''),
	  COALESCE(stack_id,0),COALESCE(node_id,''),COALESCE(job_id,''),read_at,created_at
	  FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var read sql.NullString
		if err := rows.Scan(&n.ID, &n.Type, &n.Severity, &n.Title, &n.Body,
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

func (s *Store) CountUnread() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM notifications WHERE read_at IS NULL").Scan(&n)
	return n, err
}

func (s *Store) MarkNotificationRead(id int64) error {
	_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE id=? AND read_at IS NULL", nowRFC3339(), id)
	return err
}

func (s *Store) MarkAllRead() error {
	_, err := s.db.Exec("UPDATE notifications SET read_at=? WHERE read_at IS NULL", nowRFC3339())
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
	ch chan Notification
}

type notifHub struct {
	mu   sync.Mutex
	subs map[*notifSub]struct{}
}

var notifBus = &notifHub{subs: map[*notifSub]struct{}{}}

func (h *notifHub) subscribe() *notifSub {
	s := &notifSub{ch: make(chan Notification, 32)}
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
		select {
		case s.ch <- n:
		default: // slow client — drop; it will re-fetch on next open
		}
	}
}

// ------------------------------------------------------------- emit helpers

// notify persists a notification and pushes it to live SSE subscribers.
func (a *App) notify(n Notification) {
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

// notifyStack emits a stack-scoped notification.
func (a *App) notifyStack(stackID int64, typ, severity, title, body, nodeID string) {
	a.notify(Notification{Type: typ, Severity: severity,
		Title: title, Body: body, StackID: stackID, NodeID: nodeID})
}

// ------------------------------------------------------------- HTTP

func (a *App) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	items, err := a.store.ListNotifications(50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unread, _ := a.store.CountUnread()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

func (a *App) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.store.MarkNotificationRead(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := a.store.MarkAllRead(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNotifStream streams notifications to the client over Server-Sent Events.
func (a *App) handleNotifStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
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

	sub := notifBus.subscribe()
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
