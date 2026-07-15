package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	// Fill in the version each running node actually deployed with (once per node, in the
	// background — it shows up on the next poll). See nodeversion.go.
	a.ensureNodeVersions(st, deps)
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
	// While a deployment is in progress, the node set is frozen: reject adding or
	// removing nodes (option/position edits are still fine). This keeps the canvas
	// consistent with what the in-flight provisioners are building.
	if len(body.Design) > 0 && !sameNodeSet(st.Design, design) && a.deployInProgress(st.ID) {
		writeErr(w, http.StatusConflict, "cannot add or remove nodes while a deployment is in progress")
		return
	}
	if err := a.store.UpdateStack(st.ID, name, design); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update stack")
		return
	}
	// Real-time cleanup: a node removed from the canvas has its container + volumes
	// (and deployment record) torn down immediately, not deferred to the next deploy.
	a.cleanupRemovedNodes(st.ID, design)
	updated, err := a.store.GetStack(st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read stack")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// nodeIDSet returns the set of node ids declared in a design document.
func nodeIDSet(design json.RawMessage) map[string]bool {
	var doc designDoc
	json.Unmarshal(design, &doc)
	ids := make(map[string]bool, len(doc.Nodes))
	for _, n := range doc.Nodes {
		ids[n.ID] = true
	}
	return ids
}

// sameNodeSet reports whether two designs declare exactly the same set of node ids
// (order-independent). Used to allow option/position edits but freeze add/remove.
func sameNodeSet(a, b json.RawMessage) bool {
	as, bs := nodeIDSet(a), nodeIDSet(b)
	if len(as) != len(bs) {
		return false
	}
	for id := range as {
		if !bs[id] {
			return false
		}
	}
	return true
}

// deployInProgress reports whether any of a stack's nodes is still being provisioned.
func (a *App) deployInProgress(stackID int64) bool {
	deps, _ := a.store.ListDeployments(stackID)
	for _, d := range deps {
		if d.State == DeployPending || d.State == DeployProvisioning {
			return true
		}
	}
	return false
}

// cleanupRemovedNodes tears down the containers + volumes of any deployed node that no
// longer appears in the (just-saved) design, and drops it from the Intranet DNS. The
// docker work runs in the background so the save response stays snappy; the designer's
// deployment poll reflects the removal within a few seconds.
func (a *App) cleanupRemovedNodes(stackID int64, design json.RawMessage) {
	if a.docker == nil {
		return
	}
	inDesign := nodeIDSet(design)
	deps, _ := a.store.ListDeployments(stackID)
	var removed []Deployment
	for _, d := range deps {
		if !inDesign[d.NodeID] {
			removed = append(removed, d)
		}
	}
	if len(removed) == 0 {
		return
	}
	go func() {
		st, _ := a.store.GetStack(stackID)
		bg := context.Background()
		for _, d := range removed {
			a.removeNodeResources(bg, st, d)
		}
		a.reconcileStackDNS(bg, stackID)
		a.notifyStack(stackID, "node.removed", "info", "Node(s) removed",
			fmt.Sprintf("%d deployed node(s) removed from the canvas — their containers and volumes were deleted.", len(removed)), "")
	}()
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
		a.warnExpiringStacks()
		for range ticker.C {
			a.reapExpiredStacks()
			a.warnExpiringStacks()
		}
	}()
}

var expiryWarned sync.Map // stackID -> true, to warn only once

// warnExpiringStacks notifies owners ~15 min before a stack's TTL auto-destroys it.
func (a *App) warnExpiringStacks() {
	soon, err := a.store.ListStacksExpiringSoon(15 * time.Minute)
	if err != nil {
		return
	}
	for _, st := range soon {
		if _, done := expiryWarned.LoadOrStore(st.ID, true); done {
			continue
		}
		a.notifyStack(st.ID, "stack.expiring", "warning", "Stack expiring soon",
			st.Name+" will be auto-destroyed within 15 minutes (TTL). Extend it to keep it.", "")
	}
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
