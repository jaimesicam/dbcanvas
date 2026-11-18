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

// TTL options and their durations. Every stack expires: the longest lifetime is 30 days,
// and no extension takes a stack's end further than that from now (maxStackLife).
var ttlDurations = map[string]time.Duration{
	"2h":  2 * time.Hour,
	"4h":  4 * time.Hour,
	"8h":  8 * time.Hour,
	"24h": 24 * time.Hour,
	"2w":  14 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// ttlLongest is the longest TTL, and maxStackLife how far from now a stack may end.
const ttlLongest = "30d"

var maxStackLife = ttlDurations[ttlLongest]

// ttlChoices names the TTLs for a message.
const ttlChoices = "2h, 4h, 8h, 24h, 2w or 30d"

func validTTL(ttl string) bool {
	_, ok := ttlDurations[ttl]
	return ok
}

// ttlRefusal says why ttl is not a lifetime DBCanvas gives, for an error message.
func ttlRefusal(ttl string) string {
	if ttl == "infinity" {
		return "a stack cannot be kept forever: the longest lifetime is 30d (30 days)"
	}
	return fmt.Sprintf("ttl %q: use %s", ttl, ttlChoices)
}

// expiryFor returns the RFC3339 expiry for a TTL.
func expiryFor(ttl string) *string {
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
			// Deleted rather than never there: say how (stackevents.go).
			if !a.writeStackGone(w, id) {
				writeErr(w, http.StatusNotFound, "stack not found")
			}
			return Stack{}, User{}, false
		}
		writeErr(w, http.StatusInternalServerError, "failed to read stack")
		return Stack{}, User{}, false
	}
	return st, u, true
}

func (a *App) handleListStacks(w http.ResponseWriter, r *http.Request) {
	_, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	stacks, err := a.store.ListStacks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list stacks")
		return
	}
	// ?label=tool=ci&label=run=12 keeps the stacks carrying every term (stacklabels.go).
	sel, err := parseLabelSelector(r.URL.Query()["label"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	stacks = filterStacksForToken(r, filterStacksByLabel(stacks, sel))
	if r.URL.Query().Get("detail") != "1" {
		writeJSON(w, http.StatusOK, stacks)
		return
	}
	writeJSON(w, http.StatusOK, a.stackSummaries(stacks))
}

// stackSummary is a stack as the Database Stacks table lists it (?detail=1): whose it is, how many
// of its nodes are up, and what they are. CPU and memory are not here — the page takes them from
// /api/dashboard/stats, the sample the dashboard already makes, rather than this list making one.
type stackSummary struct {
	Stack
	Nodes stackNodeCount `json:"nodes"`
	Types map[string]int `json:"types"` // node type → how many on the canvas
}

type stackNodeCount struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Error   int `json:"error"`
}

func (a *App) stackSummaries(stacks []Stack) []stackSummary {
	out := make([]stackSummary, 0, len(stacks))
	for _, s := range stacks {
		d := stackSummary{Stack: s, Types: map[string]int{}}
		if full, err := a.store.GetStack(s.ID); err == nil {
			for _, n := range buildDoc(full).Nodes {
				d.Types[n.Type]++
				d.Nodes.Total++
			}
		}
		deps, _ := a.store.ListDeployments(s.ID)
		for _, dep := range deps {
			switch dep.State {
			case DeployRunning:
				d.Nodes.Running++
			case DeployError:
				d.Nodes.Error++
			}
		}
		out = append(out, d)
	}
	return out
}

func (a *App) handleCreateStack(w http.ResponseWriter, r *http.Request) {
	_, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Name   string          `json:"name"`
		TTL    string          `json:"ttl"`
		Design json.RawMessage `json:"design"`
		// TemplateID seeds the new stack from a deployment template (templates.go).
		// The server reads the design out of the template rather than taking a
		// round-tripped copy from the client, so what gets created is what the
		// template actually says. Ignored when Design is given explicitly.
		TemplateID string `json:"templateId"`
		// DefaultName is used, numbered to the first free one ("Untitled stack 2"), when
		// Name is empty: a name the user did not type should not fail on a clash.
		DefaultName string `json:"defaultName"`
		// Labels are set on the new stack (stacklabels.go).
		Labels map[string]string `json:"labels"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = a.store.FreeStackName(orDefault(strings.TrimSpace(body.DefaultName), "Untitled stack"))
	}
	if err := validateLabels(body.Labels); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	labels, err := labelsForNewStack(r, body.Labels)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	body.Labels = labels
	if !validTTL(body.TTL) {
		writeErr(w, http.StatusBadRequest, ttlRefusal(body.TTL))
		return
	}
	if why := tokenTTLRefusal(r, body.TTL); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	design := []byte(defaultDesign)
	if len(body.Design) > 0 {
		design = body.Design
	} else if body.TemplateID != "" {
		t, err := a.loadTemplate(body.TemplateID)
		if err != nil {
			writeTemplateLoadErr(w, err)
			return
		}
		// A template's node ids only have to be unique within one stack, and this
		// is a brand-new one — so the design is copied verbatim. Merging a template
		// into an *existing* canvas is the case that needs remapping, and that one
		// runs in the designer (see StackDesigner.jsx's insertTemplate).
		design = t.Design
	}
	if why := tokenDesignRefusal(r, design); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	st, err := a.store.CreateStack(name, body.TTL, expiryFor(body.TTL), design)
	if errors.Is(err, ErrStackNameTaken) {
		a.writeStackNameTaken(w, name)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create stack")
		return
	}
	if len(body.Labels) > 0 {
		a.store.SetStackLabels(st.ID, body.Labels)
		st.Labels = body.Labels
	}
	a.recordStackCreator(r, st.ID)
	if fresh, err := a.store.GetStack(st.ID); err == nil {
		a.stackEvent(fresh, "created")
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
	if !a.saveStackDesign(w, st, name, design) {
		return
	}
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
	// Also drops the Intranet DNS tab's records (dnscustom.go) and leaves a tombstone.
	if err := a.deleteStackRecord(st); err != nil {
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
		lastPurge := time.Time{}
		for range ticker.C {
			a.reapExpiredStacks()
			a.warnExpiringStacks()
			// Old tombstones are a slower clock: hourly is plenty.
			if time.Since(lastPurge) > time.Hour {
				lastPurge = time.Now()
				a.store.PurgeStackTombstones()
			}
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

// handleExtendStack pushes a stack's expiry out by one of the TTL steps, from whichever is later of
// now and its current expiry, but never further than maxStackLife from now. The canvas offers it
// when the stack is close to its end, so a lab in use is not torn down under somebody.
func (a *App) handleExtendStack(w http.ResponseWriter, r *http.Request) {
	st, u, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		By string `json:"by"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil || !validTTL(body.By) {
		why := "by must be one of the TTLs: " + ttlChoices
		if body.By == "infinity" {
			why = ttlRefusal(body.By)
		}
		writeErr(w, http.StatusBadRequest, why)
		return
	}
	if why := tokenExtendRefusal(r); why != "" {
		writeErr(w, http.StatusForbidden, why)
		return
	}
	if st.Status == StackExpired {
		writeErr(w, http.StatusConflict, "this stack has already expired and been torn down")
		return
	}
	ttl := st.TTL
	now := time.Now()
	from := now
	if st.ExpiresAt != nil {
		if t, err := time.Parse(time.RFC3339, *st.ExpiresAt); err == nil && t.After(from) {
			from = t
		}
	}
	end := from.Add(ttlDurations[body.By])
	if limit := now.Add(maxStackLife); end.After(limit) {
		end = limit
	}
	e := end.UTC().Format(time.RFC3339)
	exp := &e
	if !validTTL(ttl) {
		ttl = body.By
	}
	if err := a.store.SetStackExpiry(st.ID, ttl, exp); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to extend the stack")
		return
	}
	a.stackEvent(st, "extended")
	expiryWarned.Delete(st.ID) // warn again before the new end
	a.recordStackEvent(st.ID, "", "action", "info", "Lifetime extended: expires "+*exp, "", u.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ttl": ttl, "expiresAt": exp})
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
		a.stackEvent(st, "expired")
	}
}

// writeStackNameTaken answers a create or rename that would give two stacks one name.
func (a *App) writeStackNameTaken(w http.ResponseWriter, name string) {
	msg := fmt.Sprintf("a stack called %q already exists — choose another name", name)
	if id := a.store.StackIDByName(name); id != 0 {
		msg = fmt.Sprintf("a stack called %q already exists (id %d) — choose another name", name, id)
	}
	writeErr(w, http.StatusConflict, msg)
}
