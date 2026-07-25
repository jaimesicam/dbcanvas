package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Labs (experimental) — hands-on scenarios that provision a real, disposable
// stack (via the same design-JSON + deploy pipeline Stack Designer uses) and
// grade "Check Work" by inspecting that stack's actual live state, never by
// comparing typed text to a baked-in answer.

// Lab is a single scenario: starting it creates a Stack from DesignTemplate and
// deploys it exactly like a normal stack; its Steps are checked against the
// live cluster.
type Lab struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	Difficulty     string          `json:"difficulty"`
	DesignTemplate json.RawMessage `json:"-"`
	// LectureNotes is background reading on the technology the lab exercises —
	// bundled in-app (not an external URL) so it never 404s and doesn't depend
	// on any other project being deployed anywhere reachable.
	LectureNotes string `json:"lectureNotes"`
	// TimeLimit is the disposable stack's TTL (one of validTTL's tokens, e.g.
	// "2h") — the lab session auto-destroys and its terminals disconnect once
	// it passes, via the same reaper that expires any other stack.
	TimeLimit string    `json:"timeLimit"`
	Steps     []LabStep `json:"steps"`
}

// LabStep is one task within a lab.
type LabStep struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Instructions string `json:"instructions"`
	Hint         string `json:"hint"`
}

// labPatroniSwitchoverDesign is a 3-node Patroni PostgreSQL cluster + HAProxy +
// Intranet — the same node/frame/edge shape Stack Designer produces, sized to
// the minimum Patroni needs for etcd quorum (3 members).
var labPatroniSwitchoverDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64"},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster"},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster"},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster"},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64"}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16"}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":""},"to":{"node":"lab-patroni-cluster","port":""},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var labCatalog = []Lab{
	{
		ID:          "patroni-switchover",
		Title:       "Patroni Switchover",
		Description: "A 3-node Patroni PostgreSQL cluster is running behind HAProxy. Move leadership to a different node with a planned switchover, without losing etcd quorum.",
		Difficulty:  "Beginner",
		TimeLimit:   "2h",
		LectureNotes: `What is Patroni?

Patroni is a template for PostgreSQL high availability: it wraps a PostgreSQL instance on each node and continuously runs it as either a Leader (accepting writes) or a Replica (streaming from the Leader). It does not do the electing itself — it delegates that to a Distributed Configuration Store (DCS), which in this lab is etcd.

Why etcd and quorum matter

Every Patroni node writes a short-lived "leader lock" key into etcd. Whoever holds that key is the Leader; everyone else watches it. etcd itself only stays available and consistent if a majority (quorum) of its members agree — with 3 etcd members (one per node here), that means at least 2 must be reachable. That's why this cluster has exactly 3 nodes: it's the smallest odd number that can lose one member and still form a majority. Lose 2 of 3 and the surviving node can't get quorum, so Patroni correctly refuses to elect a new Leader rather than risk two Leaders writing at once ("split brain").

Leader election vs. switchover vs. failover

- Election: when no Leader lock exists (fresh cluster, or the old lock expired), every eligible node races to acquire it; whoever has replayed the most WAL (write-ahead log) usually wins.
- Failover: the Leader crashes or is partitioned away unexpectedly. Once its lock expires, the remaining nodes elect a new Leader automatically. This is the "your database survived a node loss" scenario — but it's reactive, and PostgreSQL couldn't do a clean shutdown first.
- Switchover: a planned, voluntary handover you trigger with "patronictl switchover" while everything is healthy. The current Leader does a clean checkpoint and demotes itself, a chosen Replica is caught up and promoted, and the lock moves over with (ideally) a few seconds of write downtime and no data loss. This lab exercises a switchover, not a failover — it's the tool you reach for before planned maintenance (an OS patch, a hardware move) on the node currently holding the lock.

Where HAProxy fits in

Applications shouldn't need to know which of the 3 nodes is the Leader today. HAProxy in front of the cluster polls each node's Patroni REST API (":8008/leader" — 200 only on the current Leader, 404 elsewhere) every couple of seconds and only routes the write port to whichever node answers 200. That's exactly the check "patroniLeaderContainer" (the same helper this lab's Check Work button uses) performs — so what you're verifying here is the same signal HAProxy itself relies on to keep routing correctly through the switchover.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "switchover",
				Title: "Perform a planned switchover",
				Instructions: "Open a terminal on any of the three Patroni nodes. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml list` to see the cluster and find the current Leader. " +
					"Then run `patronictl -c /etc/patroni/postgresql.yml switchover` and follow the prompts to promote a " +
					"different node. Confirm with `list` that a new node is Leader, then click Check Work.",
				Hint: "Non-interactive form: patronictl -c /etc/patroni/postgresql.yml switchover --leader <current-leader-hostname> --candidate <new-leader-hostname> --force",
			},
		},
	},
}

func findLab(id string) (Lab, bool) {
	for _, l := range labCatalog {
		if l.ID == id {
			return l, true
		}
	}
	return Lab{}, false
}

func findLabStep(lab Lab, stepID string) (LabStep, bool) {
	for _, s := range lab.Steps {
		if s.ID == stepID {
			return s, true
		}
	}
	return LabStep{}, false
}

func (a *App) handleListLabs(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, labCatalog)
}

// handleStartLab provisions (or resumes) the learner's disposable stack for a
// lab. It only creates the stack + lab_run row — the frontend deploys it via
// the existing POST /api/stacks/{id}/deploy, exactly like Stack Designer.
func (a *App) handleStartLab(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	lab, ok := findLab(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "lab not found")
		return
	}
	// One active attempt per lab per learner — resume it instead of piling up
	// disposable stacks on repeated visits to the page. Not resumable once its
	// stack has hit the time limit and been reaped (status flips to "expired"
	// and its containers are gone) — fall through and provision a fresh one.
	if run, err := a.store.GetActiveLabRun(lab.ID, u.ID); err == nil {
		if st, serr := a.store.GetStack(run.StackID); serr == nil && st.Status != StackExpired {
			writeJSON(w, http.StatusOK, map[string]any{"labRun": run, "stack": st})
			return
		}
		a.store.FinishLabRun(run.ID) // its stack is gone or expired
	}
	st, err := a.store.CreateStack("Lab: "+lab.Title, u.ID, lab.TimeLimit, expiryFor(lab.TimeLimit), lab.DesignTemplate)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to provision lab stack")
		return
	}
	run, err := a.store.CreateLabRun(lab.ID, u.ID, st.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to record lab run")
		return
	}
	go a.captureLabInitialLeader(run.ID, st.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"labRun": run, "stack": st})
}

// captureLabInitialLeader polls the lab's cluster until a leader emerges, then
// records it as the baseline Check Work compares against. Runs detached from
// the request — the cluster is usually still deploying when Start returns.
func (a *App) captureLabInitialLeader(runID, stackID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		st, err := a.store.GetStack(stackID)
		if err != nil {
			return
		}
		var doc designDoc
		if json.Unmarshal(st.Design, &doc) != nil {
			continue
		}
		frame, ok := patroniFrameOf(doc)
		if !ok {
			continue
		}
		containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
		if containerID == "" {
			continue
		}
		deps, err := a.store.ListDeployments(stackID)
		if err != nil {
			continue
		}
		nodeID := nodeIDForContainer(deps, containerID)
		if nodeID == "" {
			continue
		}
		a.store.SetLabRunLeader(runID, nodeID)
		return
	}
}

func patroniFrameOf(doc designDoc) (designFrame, bool) {
	for _, f := range doc.Frames {
		if f.Type == "patroni" {
			return f, true
		}
	}
	return designFrame{}, false
}

func nodeIDForContainer(deps []Deployment, containerID string) string {
	for _, d := range deps {
		if d.ContainerID == containerID {
			return d.NodeID
		}
	}
	return ""
}

func nodeLabel(doc designDoc, nodeID string) string {
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			return n.Label
		}
	}
	return nodeID
}

// handleCheckLabStep is the "Check Work" button: it inspects the learner's own
// lab stack's real, live state and reports pass/fail — it never runs the
// learner's commands for them, and never grades typed text.
func (a *App) handleCheckLabStep(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	lab, ok := findLab(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "lab not found")
		return
	}
	step, ok := findLabStep(lab, r.PathValue("stepId"))
	if !ok {
		writeErr(w, http.StatusNotFound, "lab step not found")
		return
	}
	run, err := a.store.GetActiveLabRun(lab.ID, u.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "start the lab before checking your work")
		return
	}
	st, err := a.store.GetStack(run.StackID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read lab stack")
		return
	}

	var result LabStepResult
	switch lab.ID + ":" + step.ID {
	case "patroni-switchover:switchover":
		result = a.checkPatroniSwitchover(r.Context(), run, st)
	default:
		writeErr(w, http.StatusNotImplemented, "no check available for this step")
		return
	}
	result.LabRunID = run.ID
	result.StepID = step.ID
	result.CheckedAt = nowRFC3339()
	a.store.RecordLabStepResult(result)
	writeJSON(w, http.StatusOK, result)
}

// checkPatroniSwitchover passes once the cluster's current leader (queried live
// via each member's Patroni REST API, same helper the app uses for backups) is
// a different node than the one recorded as leader when the cluster came up.
func (a *App) checkPatroniSwitchover(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return LabStepResult{Passed: false, Message: "Could not read the lab stack's design."}
	}
	frame, ok := patroniFrameOf(doc)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — the cluster may be mid-election. Wait a few seconds and check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID == run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is still the leader — run `patronictl switchover` to promote a different node."}
	}
	oldLabel, newLabel := nodeLabel(doc, run.InitialLeaderNode), nodeLabel(doc, currentNodeID)
	return LabStepResult{Passed: true, Message: "Switchover confirmed: leadership moved from " + oldLabel + " to " + newLabel + "."}
}

// handleFinishLab ends the learner's active attempt (the frontend also calls
// the existing stack destroy endpoint to tear down its resources).
func (a *App) handleFinishLab(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	lab, ok := findLab(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "lab not found")
		return
	}
	run, err := a.store.GetActiveLabRun(lab.ID, u.ID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no active attempt for this lab")
		return
	}
	if err := a.store.FinishLabRun(run.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to finish lab")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListMyLabRuns backs a "my progress" view: every attempt the learner
// has made, newest first.
func (a *App) handleListMyLabRuns(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runs, err := a.store.ListLabRuns(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list lab runs")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
