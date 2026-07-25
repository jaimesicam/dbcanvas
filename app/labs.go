package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	{
		ID:          "patroni-failover",
		Title:       "Patroni Failover",
		Description: "Simulate an unplanned crash on the current Leader and watch Patroni elect a new one automatically — no switchover command involved.",
		Difficulty:  "Beginner",
		TimeLimit:   "2h",
		LectureNotes: `Planned vs. unplanned: switchover vs. failover

The "Patroni Switchover" lab covered the planned handover: you ask for it, the Leader checkpoints cleanly and demotes itself, and a Replica is promoted with no data loss. This lab is the other half — an unplanned failover, where the Leader simply disappears (a crash, a kernel panic, a network partition) and the remaining nodes have to notice and react on their own.

Why stopping "patroni" takes PostgreSQL down too

Patroni doesn't sit next to PostgreSQL as an independent systemd-managed service — it starts PostgreSQL itself as a supervised child process and owns its entire lifecycle. So "systemctl stop patroni" isn't just "the HA agent went away while the database kept running" — PostgreSQL goes down with it, with no checkpoint. That's what makes this a faithful crash simulation rather than a cosmetic one.

What the survivors do about it

Every Patroni node watches the Leader lock in etcd. Once the crashed Leader stops renewing it and the lock's TTL (30s in this lab) expires, the remaining nodes race to acquire it — same election mechanics as a fresh cluster. Whichever Replica has replayed the most WAL (write-ahead log) usually wins, so the promoted node is normally the one that was "most caught up," not an arbitrary pick.

The real cost of an unplanned failover

Unlike a switchover, there's no guarantee every transaction the old Leader had committed had already streamed to every Replica — Patroni's default replication here is asynchronous. A failover can lose the last few transactions that were acknowledged to the client but hadn't replicated yet. That gap is exactly why you reach for a switchover instead whenever you have the choice (planned maintenance, a graceful drain) and only rely on failover for the failures you can't schedule.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "failover",
				Title: "Simulate an unplanned crash",
				Instructions: "Open a terminal on the current Leader (check with `patronictl -c /etc/patroni/postgresql.yml list`). " +
					"Run `systemctl stop patroni` on that node — Patroni manages PostgreSQL as its own child process, so this takes " +
					"PostgreSQL down too, with no clean checkpoint. Wait about 15–30 seconds, then run " +
					"`patronictl -c /etc/patroni/postgresql.yml list` on one of the other two nodes and confirm a new Leader was elected " +
					"automatically. When you're done, run `systemctl start patroni` on the node you stopped so it rejoins as a Replica, " +
					"then click Check Work.",
				Hint: "If nothing's changed after 30s, double-check you stopped patroni on the actual Leader, not a Replica — patronictl list marks the Leader in the Role column.",
			},
		},
	},
	{
		ID:          "patroni-pause-resume",
		Title:       "Pause & Resume Autofailover",
		Description: "Put the cluster into maintenance mode so Patroni stops reacting to failures automatically, then bring it back.",
		Difficulty:  "Beginner",
		TimeLimit:   "2h",
		LectureNotes: `A mode, not an event

The Switchover and Failover labs both exercise one-time events: something happens (you ask for it, or a node crashes) and the cluster reacts once. "patronictl pause" is different — it's a persistent mode. It writes "pause: true" into the cluster's shared configuration in etcd (the same object "patronictl edit-config" edits), so it applies to every member immediately and survives until you explicitly "resume" it — even across a Patroni restart.

Why you'd want that

Imagine patching the OS on all three nodes in turn. Each restart briefly looks like a crash to the other members. Without pausing, that could trigger a real failover (and, on the node coming back up, a rejoin as a Replica) purely because of your own planned maintenance — not because you needed one. Pausing tells Patroni "keep monitoring and reporting status, but don't act on what you see" for the duration of the work.

What pause does NOT do

It doesn't stop PostgreSQL, and it doesn't stop replication — Replicas keep streaming from the Leader the whole time. It only disables Patroni's automatic reactions (promotions, demotions, restarts triggered by health checks). A manual "patronictl switchover" or "patronictl restart" still works while paused — you've only turned off the automatic part.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "pause",
				Title: "Pause autofailover",
				Instructions: "Open a terminal on any Patroni node and run `patronictl -c /etc/patroni/postgresql.yml pause`. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml list` again — the footer notes the cluster is in maintenance mode. " +
					"Then click Check Work.",
				Hint: "pause is cluster-wide (stored in etcd) — it doesn't matter which of the three nodes you run it from.",
			},
			{
				ID:    "resume",
				Title: "Resume autofailover",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml resume` on any node, confirm the maintenance-mode footer " +
					"is gone from `list`, then click Check Work.",
				Hint: "If Check Work still reports paused, give the change a few seconds to reach every member and try again.",
			},
		},
	},
	{
		ID:          "patroni-config-change",
		Title:       "Cluster-wide Configuration Change",
		Description: "Change a PostgreSQL setting on the whole cluster at once with patronictl edit-config, instead of editing config files node by node.",
		Difficulty:  "Intermediate",
		TimeLimit:   "2h",
		LectureNotes: `Who owns postgresql.conf?

On a Patroni cluster, you don't hand-edit postgresql.conf on each node — Patroni does, generating it from configuration it keeps in the DCS (the same etcd-backed config "pause" toggles and "edit-config" writes to). Edit a file locally and Patroni will happily overwrite your change on the next config refresh, because as far as it knows the DCS is still the source of truth.

Why edit-config instead

"patronictl edit-config" writes the change to that shared config once, and every member picks it up independently — no risk of three nodes drifting out of sync because you fat-fingered one file, and no risk of a Replica coming back after maintenance with stale settings from before the change.

Dynamic vs. static parameters

PostgreSQL settings split into two camps: dynamic (reloadable) parameters like "work_mem" take effect on the next config reload (SIGHUP) — no restart, no interruption. Static parameters (things like "max_connections" or "shared_buffers") need a full PostgreSQL restart to take effect, which on the Leader means a brief interruption (or you'd switchover first). This lab uses "work_mem" specifically because it's dynamic — you'll see it apply without anything restarting.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "config-change",
				Title: "Change work_mem cluster-wide",
				Instructions: "Open a terminal on any Patroni node. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml edit-config lab-patroni -p work_mem=32MB --force`. This is a dynamic " +
					"(reloadable) setting, so Patroni applies it without restarting PostgreSQL. Confirm on a couple of nodes with " +
					"`psql -U postgres -c \"show work_mem;\"`, then click Check Work.",
				Hint: "Pick any value other than the 4MB default — Check Work just confirms every member agrees and it's no longer the default.",
			},
		},
	},
	{
		ID:          "patroni-etcd-quorum",
		Title:       "etcd Quorum Loss",
		Description: "Stop etcd on two of the three nodes and watch Patroni refuse to keep a Leader without quorum — then bring it back.",
		Difficulty:  "Advanced",
		TimeLimit:   "2h",
		LectureNotes: `Quorum, revisited — this time by breaking it on purpose

The Switchover lab explained that this cluster runs 3 etcd members (one per node) so it can lose one and still keep a majority. This lab breaks that on purpose: stop etcd on 2 of the 3 nodes and the single survivor can't reach a majority either — etcd itself becomes unable to confirm or change anything, including who holds the Leader lock.

What Patroni does when it can't confirm it's still the Leader

This cluster's Patroni config uses "ttl: 30" and "loop_wait: 10" (visible via "patronictl show-config"). Once the Leader can no longer renew its lock — because etcd itself has no quorum to renew anything against — it can't prove to itself it's still safely the Leader. Rather than risk a second node getting promoted later and colliding with it (split brain), Patroni errs toward unavailability: expect no member to report as Leader once the TTL passes, until quorum returns. This is the same split-brain-avoidance trade-off the Switchover lab's lecture notes mentioned, just triggered from the DCS side instead of a node crash.

A real-world nuance this lab simplifies

Here, etcd is colocated 1:1 with each database node purely to keep the lab's footprint small — lose a database node and you also lose an etcd vote. Most production Patroni deployments run etcd as its own independently-sized cluster (often 3 or 5 dedicated members) precisely so that database node failures and DCS availability are decoupled — losing a database node doesn't put etcd's quorum at any more risk than usual.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "break-quorum",
				Title: "Break etcd quorum",
				Instructions: "Open a terminal on two of the three Patroni nodes (any two) and run `systemctl stop patroni etcd` on each " +
					"— stopping only one of the three etcd members still leaves a majority (2 of 3), so both are needed. Wait about a " +
					"minute (this lab's leader lock has a 30s TTL). Click Check Work — it passes once no node can confirm being Leader.",
				Hint: "patronictl commands may hang or time out once etcd has no quorum — that's expected; use Check Work rather than list to see the result.",
			},
			{
				ID:    "restore-quorum",
				Title: "Restore quorum",
				Instructions: "Run `systemctl start etcd patroni` again on the two nodes you stopped. Wait about 30 seconds for etcd to " +
					"reform a quorum and Patroni to elect a Leader again, then click Check Work.",
				Hint: "If it's still failing after a minute, double-check both etcd and patroni are started on both nodes: `systemctl status etcd patroni`.",
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

	ctx := r.Context()
	var result LabStepResult
	switch lab.ID + ":" + step.ID {
	case "patroni-switchover:switchover", "patroni-failover:failover":
		result = a.checkLeaderChanged(ctx, run, st)
	case "patroni-pause-resume:pause":
		result = a.checkPatroniPauseState(ctx, st, true)
	case "patroni-pause-resume:resume":
		result = a.checkPatroniPauseState(ctx, st, false)
	case "patroni-config-change:config-change":
		result = a.checkPatroniConfigChange(ctx, st)
	case "patroni-etcd-quorum:break-quorum":
		result = a.checkNoPatroniLeader(ctx, st)
	case "patroni-etcd-quorum:restore-quorum":
		result = a.checkPatroniLeaderPresent(ctx, st)
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

// checkLeaderChanged passes once the cluster's current leader (queried live via
// each member's Patroni REST API, same helper the app uses for backups) is a
// different node than the one recorded as leader when the cluster came up.
// Shared by the Switchover lab (a planned handover) and the Failover lab (an
// unplanned crash) — both are verified by the same real-world fact: did
// leadership actually move.
func (a *App) checkLeaderChanged(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
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
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is still the leader — promote a different node first."}
	}
	oldLabel, newLabel := nodeLabel(doc, run.InitialLeaderNode), nodeLabel(doc, currentNodeID)
	return LabStepResult{Passed: true, Message: "Leadership moved from " + oldLabel + " to " + newLabel + "."}
}

// patroniFrameFromStack parses a lab stack's design and returns its (single)
// Patroni cluster frame — every check in labs.go needs this first.
func patroniFrameFromStack(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return doc, designFrame{}, false
	}
	frame, ok := patroniFrameOf(doc)
	return doc, frame, ok
}

// checkPatroniPauseState passes once every running member's dynamic
// configuration ("patronictl show-config" — the same object "pause"/"resume"/
// "edit-config" all write to) agrees the cluster is in the wanted pause state.
func (a *App) checkPatroniPauseState(ctx context.Context, st Stack, wantPaused bool) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var running []Deployment
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	if len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	verb := "pausing"
	if !wantPaused {
		verb = "resuming"
	}
	for _, d := range running {
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"patronictl", "-c", "/etc/patroni/postgresql.yml", "show-config"}, nil)
		if err != nil || res.Code != 0 {
			return LabStepResult{Passed: false, Message: "Could not read the cluster config from " + nodeLabel(doc, d.NodeID) + " — is it still starting up?"}
		}
		paused := strings.Contains(strings.ToLower(res.Stdout), "pause: true")
		if paused != wantPaused {
			return LabStepResult{Passed: false, Message: nodeLabel(doc, d.NodeID) + " hasn't picked up the change yet — you may still be " + verb + ". Wait a few seconds and check again."}
		}
	}
	if wantPaused {
		return LabStepResult{Passed: true, Message: "Confirmed: the cluster is paused (autofailover disabled) on every running member."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the cluster is resumed (autofailover enabled) on every running member."}
}

// checkPatroniConfigChange passes once every running member reports the same
// non-default work_mem — proof the edit-config change actually applied
// cluster-wide, not just that it was written to the DCS.
func (a *App) checkPatroniConfigChange(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var values []string
	var labels []string
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.State != DeployRunning || d.ContainerID == "" {
			return LabStepResult{Passed: false, Message: n.Label + " is not running yet — wait for the cluster to be ready."}
		}
		res, err := a.engCtx(ctx).ExecAs(ctx, d.ContainerID, "postgres",
			[]string{"psql", "-U", "postgres", "-d", "postgres", "-tAqc", "show work_mem;"}, nil)
		if err != nil || res.Code != 0 {
			return LabStepResult{Passed: false, Message: "Could not query " + n.Label + " — is PostgreSQL up?"}
		}
		values = append(values, strings.TrimSpace(res.Stdout))
		labels = append(labels, n.Label)
	}
	if len(values) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni members found."}
	}
	for i, v := range values {
		if v == "" || v == "4MB" {
			return LabStepResult{Passed: false, Message: labels[i] + " is still the default work_mem (4MB) — run patronictl edit-config to change it cluster-wide."}
		}
	}
	for i, v := range values[1:] {
		if v != values[0] {
			return LabStepResult{Passed: false, Message: labels[0] + " reports " + values[0] + " but " + labels[i+1] + " reports " + v + " — wait for Patroni to finish reloading the config on every member."}
		}
	}
	return LabStepResult{Passed: true, Message: "work_mem = " + values[0] + " confirmed on all " + labels[0] + ", " + strings.Join(labels[1:], ", ") + "."}
}

// checkNoPatroniLeader passes once no member can confirm being Leader — the
// expected state once etcd has lost quorum and the old Leader's lock has
// expired without anyone able to renew or contest it.
func (a *App) checkNoPatroniLeader(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	if containerID := a.patroniLeaderContainer(ctx, st, frame, doc); containerID != "" {
		return LabStepResult{Passed: false, Message: "A leader is still reachable — either quorum hasn't broken yet (stop etcd on two nodes, not one) or the lock hasn't expired. Wait a bit and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: no member can currently confirm being Leader — etcd has no quorum to grant or renew the lock."}
}

// checkPatroniLeaderPresent passes once a member can confirm being Leader
// again — the expected state once quorum has returned and Patroni has held a
// fresh election.
func (a *App) checkPatroniLeaderPresent(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "Still no leader — give etcd and Patroni a bit longer to reform quorum and elect one, then check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, nodeIDForContainer(deps, containerID)) + " is Leader — quorum is back."}
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
