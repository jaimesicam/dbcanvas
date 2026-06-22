package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// TTL options and their durations. "infinity" maps to no expiry.
var ttlDurations = map[string]time.Duration{
	"2h":  2 * time.Hour,
	"4h":  4 * time.Hour,
	"8h":  8 * time.Hour,
	"24h": 24 * time.Hour,
	"2w":  14 * 24 * time.Hour,
}

const ttlInfinity = "infinity"

func validTTL(ttl string) bool {
	if ttl == ttlInfinity {
		return true
	}
	_, ok := ttlDurations[ttl]
	return ok
}

// expiryFor returns the RFC3339 expiry for a TTL, or nil for infinity.
func expiryFor(ttl string) *string {
	if ttl == ttlInfinity {
		return nil
	}
	t := time.Now().Add(ttlDurations[ttl]).UTC().Format(time.RFC3339)
	return &t
}

const defaultDesign = `{"nodes":[],"edges":[],"view":{"x":0,"y":0,"z":1}}`

// stackDetail is a stack plus its node deployment records.
type stackDetail struct {
	Stack
	Deployments []Deployment `json:"deployments"`
}

// loadOwnedStack resolves {id}, enforcing authentication and ownership (admins
// may access any stack). It writes the error response itself on failure.
func (a *App) loadOwnedStack(w http.ResponseWriter, r *http.Request) (Stack, User, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return Stack{}, User{}, false
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid stack id")
		return Stack{}, User{}, false
	}
	st, err := a.store.GetStack(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "stack not found")
			return Stack{}, User{}, false
		}
		writeErr(w, http.StatusInternalServerError, "failed to read stack")
		return Stack{}, User{}, false
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your stack")
		return Stack{}, User{}, false
	}
	return st, u, true
}

func (a *App) handleListStacks(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stacks, err := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list stacks")
		return
	}
	writeJSON(w, http.StatusOK, stacks)
}

func (a *App) handleCreateStack(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Name   string          `json:"name"`
		TTL    string          `json:"ttl"`
		Design json.RawMessage `json:"design"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Untitled stack"
	}
	if !validTTL(body.TTL) {
		writeErr(w, http.StatusBadRequest, "invalid ttl")
		return
	}
	design := []byte(defaultDesign)
	if len(body.Design) > 0 {
		design = body.Design
	}
	st, err := a.store.CreateStack(name, u.ID, body.TTL, expiryFor(body.TTL), design)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create stack")
		return
	}
	writeJSON(w, http.StatusCreated, st)
}

func (a *App) handleGetStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read deployments")
		return
	}
	writeJSON(w, http.StatusOK, stackDetail{Stack: st, Deployments: deps})
}

func (a *App) handleUpdateStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Name   string          `json:"name"`
		Design json.RawMessage `json:"design"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = st.Name
	}
	design := st.Design
	if len(body.Design) > 0 {
		design = body.Design
	}
	if err := a.store.UpdateStack(st.ID, name, design); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update stack")
		return
	}
	updated, err := a.store.GetStack(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read stack")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *App) handleDeleteStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	// Tear down any deployed containers before removing the record.
	a.teardownStack(st.ID)
	if err := a.store.DeleteStack(st.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete stack")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// startReaper runs the expiry reaper once at startup, then every 60s.
func (a *App) startReaper() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		a.reapExpiredStacks()
		for range ticker.C {
			a.reapExpiredStacks()
		}
	}()
}

// reapExpiredStacks marks stacks past their TTL as expired and tears down their
// containers. Runs periodically from main.
func (a *App) reapExpiredStacks() {
	expired, err := a.store.ListExpiredStacks()
	if err != nil {
		return
	}
	for _, st := range expired {
		a.teardownStack(st.ID)
		a.store.SetStackStatus(st.ID, StackExpired)
	}
}
