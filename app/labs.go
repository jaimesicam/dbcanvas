package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
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
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Difficulty  string `json:"difficulty"`
	// Database -> Technology -> Category is the catalog's three-level grouping
	// (collapsible sections nested in that order) — purely a frontend
	// organization aid, not tied to anything structural about the lab itself.
	// Database is the engine family (e.g. "PostgreSQL", "Valkey"); Technology
	// is the specific tool/mode within it (e.g. "Patroni", "Valkey Cluster");
	// Category is the topic within that technology (e.g. "Replication").
	Database       string          `json:"database"`
	Technology     string          `json:"technology"`
	Category       string          `json:"category"`
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
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster","x":574,"y":66},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster","x":702,"y":66},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64","x":300,"y":40}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-patroni-cluster","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPatroniBackupDesign extends labPatroniSwitchoverDesign's topology with a
// SeaweedFS node backing pgBackRest — TLS must be on (S3 TLS is required for
// pgBackRest's S3 client; see pgBackRestSeaweedIssues in intranet.go) and it
// needs at least one bucket named up front (SeaweedFS nodes don't get a
// default bucket).
var labPatroniBackupDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster","x":574,"y":66},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster","x":702,"y":66},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64","x":300,"y":40},
    {"id":"lab-seaweed","type":"seaweedfs","label":"seaweed","arch":"amd64","bucket":"lab-backups","tls":true,"x":560,"y":220}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","usePgBackRest":true,"seaweedfsNodeId":"lab-seaweed","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-patroni-cluster","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPatroniManualRewindDesign is labPatroniSwitchoverDesign's topology with
// use_pg_rewind turned off cluster-wide (disablePgRewind) — without it,
// Patroni won't automatically rewind (or silently reclone) a node whose
// timeline has diverged from the current Leader, so the "manual pg_rewind"
// lab's learner has to run pg_rewind themselves before Patroni can bring that
// node back at all.
var labPatroniManualRewindDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster","x":574,"y":66},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster","x":702,"y":66},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64","x":300,"y":40}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","disablePgRewind":true,"x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-patroni-cluster","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPatroniStandbyClusterDesign adds a standalone PostgreSQL node
// ("external-primary") alongside the usual 3-node Patroni cluster — this
// lab's whole point is following (and later detaching from) a primary that
// Patroni itself doesn't manage, so that primary has to exist outside the
// Patroni frame. captureLabStandbyClusterUpstream (see below) configures it
// to accept a physical replication connection, since ordinary standalone
// nodes are never provisioned as a replication source.
var labPatroniStandbyClusterDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster","x":574,"y":66},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster","x":702,"y":66},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64","x":300,"y":40},
    {"id":"lab-pg-external","type":"pg","label":"external-primary","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","x":560,"y":200}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-patroni-cluster","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPatroniCallbacksDesign is labPatroniSwitchoverDesign's topology with
// enableRoleChangeCallback set — every member gets an on_role_change script
// staged and wired into patroni.yml, so the "Patroni Callbacks" lab's
// Check Work can read a real, append-only log of role-change events instead
// of only inferring them from cluster state.
var labPatroniCallbacksDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-pg-1","type":"patroni","label":"pg-node-1","frameId":"lab-patroni-cluster","x":574,"y":66},
    {"id":"lab-pg-2","type":"patroni","label":"pg-node-2","frameId":"lab-patroni-cluster","x":702,"y":66},
    {"id":"lab-pg-3","type":"patroni","label":"pg-node-3","frameId":"lab-patroni-cluster","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","arch":"amd64","x":300,"y":40}
  ],
  "frames": [
    {"id":"lab-patroni-cluster","type":"patroni","label":"lab-patroni","os":"oraclelinux","osVersion":"9","arch":"amd64","pgMajor":"16","enableRoleChangeCallback":true,"x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-patroni-cluster","port":"left"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var labCatalog = []Lab{
	{
		ID:          "patroni-switchover",
		Title:       "Patroni Switchover",
		Description: "A 3-node Patroni PostgreSQL cluster is running behind HAProxy. Move leadership to a different node with a planned switchover, without losing etcd quorum.",
		Difficulty:  "Beginner",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Leadership & Failover",
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
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Leadership & Failover",
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
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
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
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
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
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "DCS & Quorum",
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
	{
		ID:          "patroni-rolling-restart",
		Title:       "Rolling Restart & Static Config",
		Description: "Change a PostgreSQL setting that needs a restart to take effect, and roll it out cluster-wide without write downtime.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `Reload vs. restart

Not every PostgreSQL setting can be changed with a config reload (SIGHUP). Dynamic parameters like "work_mem" (from the Cluster-wide Configuration Change lab) take effect immediately on reload. Static parameters — things that affect shared memory layout or other fixed-size structures decided at startup, like "max_connections" — can only change by restarting the postmaster. "patronictl edit-config" writes to the same shared config either way; the difference shows up in "patronictl list", which flags every member "Pending restart" once you change a static parameter, because Patroni already knows a reload alone won't apply it.

Why order matters

Restarting a Replica is nearly free — it drops and immediately re-establishes its streaming connection, with no client-visible impact. Restarting the Leader briefly interrupts every write in flight. So the standard pattern is: restart every Replica first (in any order), and restart the Leader last — or, better still, switch over away from it first and then restart it as a plain Replica, avoiding even that brief interruption. This lab has you restart the Leader directly so you can see the trade-off; combine it with the Switchover lab's technique whenever that interruption isn't acceptable.

patronictl restart vs. a manual systemctl restart

You could just "systemctl restart patroni" on each node yourself — but "patronictl restart" goes through Patroni's own DCS-coordinated handshake first, so it correctly refuses if, say, that member is mid-election or another restart is already in flight. It's the same safety net that makes switchover/failover preferable to killing processes by hand.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "rolling-restart",
				Title: "Change max_connections and roll out the restart",
				Instructions: "Open a terminal on any Patroni node. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml edit-config lab-patroni -p max_connections=300 --force`, then " +
					"`patronictl -c /etc/patroni/postgresql.yml list` — every member now shows \"Pending restart\". Restart the two " +
					"Replicas first, one at a time: `patronictl -c /etc/patroni/postgresql.yml restart lab-patroni <replica-hostname> --force`. " +
					"Restart the Leader last, the same way. Confirm on a couple of nodes with `psql -U postgres -c \"show max_connections;\"`, " +
					"then click Check Work.",
				Hint: "patronictl restart takes the cluster name and a specific member hostname — there's no \"restart all\" shortcut, so repeat it three times.",
			},
		},
	},
	{
		ID:          "patroni-manual-failover",
		Title:       "Manual Failover with a Candidate",
		Description: "The cluster is paused and its Leader has crashed — no automatic election will happen. Use patronictl failover (not switchover) to manually promote a candidate, then resume monitoring.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Leadership & Failover",
		TimeLimit:   "2h",
		LectureNotes: `failover vs. switchover, precisely

Both promote a Replica to Leader, but they start from different premises. "switchover" assumes a healthy, reachable current Leader — it asks that Leader to checkpoint cleanly and step down, then promotes your chosen candidate. "failover" makes no such assumption: it's what you reach for when there's no Leader to ask — it crashed, or, as in this lab, the cluster is paused so nothing auto-promoted on its own. failover just tells Patroni "elect this node now," full stop.

Manual commands still work while paused

"pause" only switches off Patroni's automatic reactions to what it observes — it doesn't lock out the operator. switchover, failover and restart all still work normally while the cluster is paused; that's exactly why this lab pairs them: pause first so a crash doesn't trigger an unwanted auto-election, deal with the crash by hand, then resume.

The gotcha this lab is designed to catch

It's easy to pause a cluster for maintenance, finish the maintenance, and walk away — forgetting to resume. A paused cluster looks completely normal day to day (still serving reads and writes) right up until the moment a node actually fails and nothing reacts to it. Always resume once you're done — this lab's second Check Work doesn't pass until you do.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "pause-and-crash",
				Title: "Pause the cluster, then simulate a crash",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml pause` on any node. Then find the current Leader with " +
					"`patronictl -c /etc/patroni/postgresql.yml list` and run `systemctl stop patroni` on it. Because the cluster is paused, " +
					"the other two nodes won't auto-elect a replacement — confirm with `list` that there's no Leader, then click Check Work.",
				Hint: "If Check Work says a leader is still reachable, double-check you stopped patroni on the actual Leader, not a Replica.",
			},
			{
				ID:    "failover-and-resume",
				Title: "Fail over manually, then resume",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml failover lab-patroni --candidate <hostname> --force` naming one " +
					"of the two remaining nodes — no `--leader` flag is needed, since there isn't one to demote. Confirm a new Leader appears " +
					"in `list`, then run `patronictl -c /etc/patroni/postgresql.yml resume` so autofailover protects you again. Click Check Work.",
				Hint: "Check Work checks both halves — a Leader must exist AND the cluster must no longer be paused.",
			},
		},
	},
	{
		ID:          "patroni-synchronous-replication",
		Title:       "Synchronous Replication",
		Description: "Enable synchronous replication so a commit isn't acknowledged until a Replica has confirmed it — closing the data-loss window an unplanned failover can open.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `Closing the gap the Failover lab opened

That lab's lecture notes flagged a real risk: with the default asynchronous replication, a Leader can acknowledge a commit to the client before any Replica has received it — an unplanned failover can then lose those last few transactions. Synchronous replication closes that gap: once enabled, the Leader won't acknowledge a commit until at least one synchronous Replica confirms it has received (not necessarily applied — just received) the WAL.

Patroni picks the synchronous replica for you

You don't hand-name which Replica is synchronous. Once "synchronous_mode" is on, Patroni continuously chooses among the healthy Replicas and rewrites the Leader's "synchronous_standby_names" to match — including automatically promoting a different Replica to synchronous status if the current one falls behind or disappears. That's the same DCS-coordinated pattern behind everything else in this curriculum: Patroni keeps every member's configuration consistent with one shared decision, rather than you configuring each node by hand.

The trade-off: durability costs availability

This closes the data-loss gap, but it isn't free. If the synchronous Replica becomes unreachable, the Leader can't get a commit confirmation from anyone — by default, writes simply stall until a synchronous Replica is available again. Enabling "synchronous_mode" is a deliberate choice to favor durability over write availability during a Replica outage.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "enable-sync",
				Title: "Enable synchronous replication",
				Instructions: "Open a terminal on any Patroni node. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml edit-config lab-patroni --set synchronous_mode=true --force`. Patroni will " +
					"automatically pick one Replica as the synchronous standby and update the Leader's synchronous_standby_names for you — " +
					"no need to name it yourself. Confirm on the Leader with `psql -U postgres -c \"select application_name, sync_state from " +
					"pg_stat_replication;\"` (one row should show sync_state = sync), then click Check Work.",
				Hint: "If every row still shows async after a few seconds, double-check you used --set (not --pg-param) — synchronous_mode is a Patroni-level setting, not a postgresql.conf parameter.",
			},
		},
	},
	{
		ID:          "patroni-failsafe-mode",
		Title:       "DCS Failsafe Mode",
		Description: "Enable failsafe_mode, then break etcd (leaving Patroni itself running everywhere) and watch the Leader stay up instead of demoting.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "DCS & Quorum",
		TimeLimit:   "2h",
		LectureNotes: `The etcd Quorum Loss lab's other ending

That lab stopped both etcd and Patroni on two nodes, and showed the Leader correctly step down rather than risk split-brain once it couldn't renew its lock. This lab breaks etcd the same way but leaves Patroni running everywhere — deliberately, so you can see the alternative Patroni offers for exactly that situation: failsafe_mode.

How it stays safe without the DCS

With "failsafe_mode" on, a Leader that suddenly can't reach etcd doesn't immediately demote. Instead, it asks every other member directly over their Patroni REST APIs — bypassing etcd entirely — whether any of them thinks it's the Leader. If it can reach all of them and none disagree, it keeps operating, DCS or no DCS. That's why this lab needs Patroni itself left running on the other nodes: the Leader's failsafe check depends on being able to ask them directly.

Why it's opt-in, not the default

"failsafe_mode" trades a small amount of split-brain risk (it relies on direct network reachability instead of a DCS-arbitrated lock) for meaningfully better availability during a DCS-only outage — a real and not uncommon failure mode, since etcd/Consul/ZooKeeper clusters have their own failure modes independent of your database nodes. Whether that trade-off is right for you depends on how much you trust your network versus how much unavailability you're willing to tolerate during a DCS blip.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "enable-failsafe",
				Title: "Enable failsafe mode",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml edit-config lab-patroni --set failsafe_mode=true --force` on " +
					"any node. Confirm with `patronictl -c /etc/patroni/postgresql.yml show-config` that `failsafe_mode: true` appears, then " +
					"click Check Work.",
				Hint: "This must be enabled BEFORE you break etcd in the next step — it can't help a Leader that's already lost contact.",
			},
			{
				ID:    "break-etcd-only",
				Title: "Break etcd only — leave Patroni running",
				Instructions: "Note the current Leader with `patronictl -c /etc/patroni/postgresql.yml list`. Open a terminal on the other " +
					"two nodes and run `systemctl stop etcd` on each — this time, do NOT stop patroni; it needs to keep running so the Leader " +
					"can still reach it directly. Wait about 30–60 seconds, then click Check Work — it passes once the same node is still Leader " +
					"despite etcd having no quorum.",
				Hint: "If Check Work says the leader changed, double check failsafe_mode was actually enabled before you stopped etcd.",
			},
			{
				ID:    "restore-etcd",
				Title: "Restore etcd",
				Instructions: "Run `systemctl start etcd` on the two nodes you stopped it on. Wait about 30 seconds for etcd to reform a " +
					"quorum, then click Check Work.",
				Hint: "The Leader should already have been up the whole time — this step just confirms normal DCS-backed operation resumed.",
			},
		},
	},
	{
		ID:          "patroni-backup-restore",
		Title:       "Backup & Restore with pgBackRest",
		Description: "This cluster backs up to S3-compatible storage (SeaweedFS). Take a backup yourself, then use it to reclone a Replica instead of streaming a fresh copy from the Leader.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Backup & Recovery",
		TimeLimit:   "2h",
		LectureNotes: `Why an HA cluster still needs backups

Replication protects against a node dying — there are still two other copies. It does nothing to protect you against a bad "DELETE" or "DROP TABLE": that mistake replicates to every Replica in milliseconds, just as faithfully as a good write. Backups are the only thing in this stack that gives you a version of the data from before a mistake happened. This lab covers restoring a single member from backup, the routine operational case (a corrupted disk, a member you want to reclone) — recovering a whole cluster to a moment before a specific mistake (point-in-time recovery) is a related, more advanced operation this lab doesn't attempt.

Physical backups, not pg_dump

pgBackRest takes physical, block-level backups — a copy of the actual data directory plus continuously archived WAL — not the logical SQL-statement dumps "pg_dump" produces. That's what makes it fast enough to restore a multi-gigabyte cluster member in minutes instead of hours, and it's what lets Patroni treat "restore from backup" as just another way to create a replica.

Patroni already knows how to use your backups

Look at "patronictl list" after this lab's cluster first comes up: a full backup already happened automatically, right after the stanza was created. That's because this Patroni cluster's "create_replica_methods" includes pgbackrest — so whenever Patroni needs to (re)create a member from scratch, it restores from the S3 repo instead of streaming a fresh "pg_basebackup" copy from the Leader. That's faster, and it doesn't load extra I/O and network bandwidth onto the Leader while it's serving live traffic — a real reason to configure backups even on a cluster that "already has replicas."

Why the S3 store needs TLS

pgBackRest's S3 client refuses plain HTTP; this lab's SeaweedFS node runs with a self-signed TLS certificate (pgbackrest.conf sets "repo1-s3-verify-tls=n" so it doesn't need to trust the CA — only that the endpoint speaks HTTPS at all).`,
		DesignTemplate: labPatroniBackupDesign,
		Steps: []LabStep{
			{
				ID:    "take-backup",
				Title: "Take a full backup",
				Instructions: "This cluster already took one pgBackRest backup automatically when it was created — see for yourself with " +
					"`pgbackrest --stanza=lab-patroni info` on any node. Now take a fresh one yourself: run " +
					"`pgbackrest --stanza=lab-patroni --type=full backup` (this works from any node — it just needs to reach the same S3 " +
					"repo, not necessarily the Leader). Wait for it to finish without errors, then click Check Work.",
				Hint: "Check Work is looking for more backups than existed when the cluster first came up — the automatic initial one alone won't pass it.",
			},
			{
				ID:    "restore-replica",
				Title: "Reclone a Replica from backup",
				Instructions: "Find a Replica with `patronictl -c /etc/patroni/postgresql.yml list` (anyone that isn't the Leader), then run " +
					"`patronictl -c /etc/patroni/postgresql.yml reinit lab-patroni <replica-hostname> --force` on any node. This wipes that " +
					"member's data directory and reclones it — and because pgBackRest is configured, Patroni restores from your S3 backup " +
					"rather than streaming a fresh copy from the Leader. Wait about a minute, confirm with `list` that it's back to Role: " +
					"Replica / State: streaming, then click Check Work.",
				Hint: "reinit only touches the one member you name — the Leader and the other Replica are never affected, so this is safe to try.",
			},
		},
	},
	{
		ID:          "patroni-basebackup-rebuild",
		Title:       "Manual Rebuild with pg_basebackup",
		Description: "A Replica's disk is gone. Rebuild it from scratch with pg_basebackup — no pgBackRest, no S3 — the same tool Patroni itself falls back on when no backup repository is configured.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Backup & Recovery",
		TimeLimit:   "2h",
		LectureNotes: `What Patroni's reinit actually does when there's no backup repository

The Backup & Restore lab showed "patronictl reinit" recloning a member from a pgBackRest/S3 backup. Most Patroni clusters never have that configured — this cluster doesn't. With no backup tool listed in "create_replica_methods", Patroni's only way to build a new replica is its own "basebackup" method — a thin wrapper around PostgreSQL's own "pg_basebackup", which streams a byte-for-byte physical copy of the current Leader's data directory over the replication protocol. Anything "patronictl reinit" can do without a configured backup tool, "pg_basebackup" itself can do directly — this lab has you run that exact tool by hand, so you can use it without Patroni's help.

Why you'd ever do this by hand

"patronictl reinit" needs a healthy DCS and a running Patroni on the target — most of the time that's fine. But if etcd has no quorum (see the etcd Quorum Loss lab) or Patroni itself won't start, patronictl can't reach it either. "pg_basebackup" has no such dependency: it only needs network access to a running PostgreSQL Leader and a replication-privileged login. That makes it the tool of last resort when the higher-level tooling that normally does this for you isn't available.

What it actually copies, and what it doesn't

"pg_basebackup -D <dir> -Fp -Xs" streams the Leader's entire on-disk cluster (tablespaces, configuration files, everything under the data directory) as of a consistent checkpoint, plus ("-X stream") the WAL generated during the copy itself — so the result is immediately valid and needs no separate WAL replay to reach consistency. It intentionally does not copy replication settings into postgresql.auto.conf the way "-R" would — this cluster leaves that to Patroni, which reasserts the correct standby configuration itself the moment it starts against a data directory the DCS says shouldn't be Leader, regardless of how that directory got there.

Permissions matter as much as the copy

PostgreSQL refuses to start against a data directory it doesn't own. Whatever OS user creates the copy is who has to own it afterward — that's why this lab has you run pg_basebackup as the "postgres" OS user directly (via "runuser"), rather than as root and fixing permissions afterward.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "wipe-replica",
				Title: "Simulate the loss of a Replica's disk",
				Instructions: "Open a terminal on any Patroni node and run `patronictl -c /etc/patroni/postgresql.yml list` to find a " +
					"Replica (not the Leader). Open a terminal on that Replica and run `systemctl stop patroni` — this stops both Patroni " +
					"and the PostgreSQL it supervises. Then simulate the disk itself being gone: `rm -rf /var/lib/pgsql/16/data/*`. " +
					"Click Check Work.",
				Hint: "Check Work looks for the actual PG_VERSION marker file being gone, not just the service being stopped — a real disk loss, not a paused one.",
			},
			{
				ID:    "rebuild-basebackup",
				Title: "Rebuild it with pg_basebackup",
				Instructions: "On any healthy node, run `patronictl -c /etc/patroni/postgresql.yml list` to confirm the current Leader's " +
					"hostname. Back on the node you wiped, run pg_basebackup as the postgres OS user, so the copy comes out with the right " +
					"ownership:\n\n`runuser -u postgres -- bash -c \"PGPASSWORD=repl_password pg_basebackup -h <leader-hostname> -U " +
					"replicator -D /var/lib/pgsql/16/data -Fp -Xs -P\"`\n\nWait for it to finish (it prints a progress percentage), then " +
					"run `systemctl start patroni`. Patroni starts PostgreSQL against the freshly-copied directory and reconfigures it as " +
					"a streaming Replica against the current Leader automatically. Confirm with `patronictl list` that it shows Role: " +
					"Replica, State: streaming, then click Check Work.",
				Hint: "If pg_basebackup fails with an authentication error, double-check you used the `replicator` user, not `postgres` — only replicator is granted replication access in pg_hba.",
			},
		},
	},
	{
		ID:          "patroni-pgbackrest-manual-restore",
		Title:       "Disaster Recovery: Manual pgBackRest Restore",
		Description: "etcd has no quorum and patronictl won't respond — but a Replica's disk is gone and you have pgBackRest backups. Rebuild the node yourself, the same way patronictl reinit would, without patronictl.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Backup & Recovery",
		TimeLimit:   "2h",
		LectureNotes: `What patronictl reinit is actually a shortcut for

The Backup & Restore lab used "patronictl reinit", which — for a pgBackRest-enabled cluster like this one — tells the target member's own Patroni to run exactly one command: "pgbackrest --stanza=<cluster> --delta restore" (visible in that member's patroni.yml, under "pgbackrest.command"). reinit is a convenience: it finds a healthy Patroni to ask, which then runs that restore locally and reports back through the DCS. This lab strips that convenience away.

Why you might not have it

patronictl is a client to Patroni's REST API, arbitrated through etcd. If etcd has lost quorum (the etcd Quorum Loss lab) or the target's own Patroni process won't come up at all, there may be nobody for patronictl to ask — but pgBackRest itself doesn't care about any of that. It's a standalone CLI that only needs its own config (/etc/pgbackrest/pgbackrest.conf) and network access to the S3 repository. That independence is exactly why it's the tool you fall back to when the orchestration layer on top of it is the thing that's broken.

--delta, not a plain restore

"pgbackrest restore" alone assumes an empty target and copies everything. "--delta" instead compares what's already on disk against the backup's manifest (checksums + sizes) and only copies what's missing or different — the same flag Patroni's own pgbackrest.command always uses, whether the target is empty (this lab) or only partially damaged. Using --delta here isn't just for speed; it's so the muscle memory matches what you'd actually reach for on a partially-damaged node too, not only a fully wiped one.

Patroni still finishes the job

The restored data directory alone isn't enough — pgBackRest brings PostgreSQL back to a consistent point, but doesn't know this node should stream as a Replica instead of running standalone. That's still Patroni's job: the moment you "systemctl start patroni", it checks the DCS, sees this member isn't the Leader, and configures replication against whoever currently is — the same reconciliation the pg_basebackup lab relies on, regardless of which tool actually produced the on-disk copy.`,
		DesignTemplate: labPatroniBackupDesign,
		Steps: []LabStep{
			{
				ID:    "wipe-replica",
				Title: "Simulate the loss of a Replica's disk",
				Instructions: "Open a terminal on any Patroni node and run `patronictl -c /etc/patroni/postgresql.yml list` to find a " +
					"Replica (not the Leader). Open a terminal on that Replica and run `systemctl stop patroni` — this stops both Patroni " +
					"and the PostgreSQL it supervises. Then simulate the disk itself being gone: `rm -rf /var/lib/pgsql/16/data/*`. " +
					"Click Check Work.",
				Hint: "Check Work looks for the actual PG_VERSION marker file being gone, not just the service being stopped — a real disk loss, not a paused one.",
			},
			{
				ID:    "manual-restore",
				Title: "Restore it by hand — no patronictl",
				Instructions: "Back on the node you wiped, restore it directly with pgBackRest, bypassing patronictl entirely:\n\n" +
					"`runuser -u postgres -- pgbackrest --stanza=lab-patroni --delta restore`\n\nWait for it to report success, then run " +
					"`systemctl start patroni`. Patroni starts PostgreSQL against the restored directory and reconfigures it as a " +
					"streaming Replica against the current Leader. Confirm with `patronictl -c /etc/patroni/postgresql.yml list` that it " +
					"shows Role: Replica, State: streaming, then click Check Work.",
				Hint: "If the restore fails claiming the repo is unreachable, check the SeaweedFS node is running — `pgbackrest --stanza=lab-patroni info` (from the Backup & Restore lab) confirms it can reach the S3 repo before you retry.",
			},
		},
	},
	{
		ID:          "patroni-pg-rewind",
		Title:       "Diverged Timeline — Automatic pg_rewind Recovery",
		Description: "Crash the Leader hard enough to leave committed data no Replica ever received, then watch Patroni reattach it with pg_rewind instead of a full reclone.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `Why a crash can leave a node "ahead" of everyone else

Every earlier lab that crashes or stops the Leader (the Failover lab, the Manual Failover lab) uses "systemctl stop patroni", which still asks PostgreSQL to shut down through Patroni's normal stop path before exiting. This lab does something rougher: "systemctl kill -s KILL patroni" sends SIGKILL straight to the whole service, PostgreSQL included, with no chance to finish anything. Whatever the Leader had already committed locally — durably, on its own disk — but hadn't yet streamed to a Replica when the signal landed is now only on that one node. When it comes back, its timeline has moved further than everyone else's.

The two ways Patroni can reconcile that

If the crashed node's extra data can be safely discarded and the divergence is recoverable, Patroni's "use_pg_rewind: true" (set in this cluster's patroni.yml) lets it run PostgreSQL's own "pg_rewind" tool: it identifies exactly where the two timelines forked, copies back only the pages that changed since, and re-attaches the node as a Replica of the current Leader — usually in seconds, not the minutes a full reclone (the pg_basebackup / pgBackRest rebuild labs) would take. If rewind isn't possible for some reason (too little WAL retained to find the fork point, checksums disabled, etc.), Patroni falls back to a full reclone automatically — it's a bonus fast path, not a requirement.

Why this only works because streaming replication is asynchronous

This is the same asynchronous-replication trade-off the Failover lab's notes and the Synchronous Replication lab exist to illustrate: a Leader can acknowledge a commit before a Replica has it. That's normally framed as a risk (lost transactions on failover). Here it's the mechanism's whole reason to exist: pg_rewind reconciles a node that has that "extra" locally-committed data safely, rather than treating it as unrecoverable corruption.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "crash-leader",
				Title: "Crash the Leader hard",
				Instructions: "This lab's CRUD Traffic (above) is already writing continuously, so the Leader always has some just-committed " +
					"data the Replicas haven't caught up to yet — exactly the condition a real crash needs to actually diverge. Find the " +
					"current Leader with `patronictl -c /etc/patroni/postgresql.yml list`, open a terminal on it, and run " +
					"`systemctl kill -s KILL patroni` — a hard kill, not a graceful stop, so PostgreSQL gets no chance to finish streaming " +
					"what it had already committed locally. Wait about 15–30 seconds, confirm from another node that a new Leader was " +
					"elected, then click Check Work.",
				Hint: "`systemctl stop patroni` (used in the Failover lab) still lets Patroni ask PostgreSQL to shut down cleanly first — `kill -s KILL` skips that entirely, which is what actually risks a diverged timeline.",
			},
			{
				ID:    "rejoin-rewind",
				Title: "Bring it back and verify pg_rewind rescued it",
				Instructions: "On the node you crashed, run `systemctl start patroni`. Patroni starts PostgreSQL, discovers this node's " +
					"timeline has diverged from the new Leader's, and — because this cluster has use_pg_rewind enabled — rewinds it back " +
					"onto the current timeline instead of requiring a full reclone. Confirm with " +
					"`patronictl -c /etc/patroni/postgresql.yml list` that it's back to Role: Replica, State: streaming, then click " +
					"Check Work.",
				Hint: "If it's stuck, check `journalctl -u patroni -n 100` on that node — pg_rewind needs the Leader's connection info and a superuser login, both of which this cluster's patroni.yml already provides automatically.",
			},
		},
	},
	{
		ID:          "patroni-pg-rewind-manual",
		Title:       "Manual pg_rewind — No Automatic Rescue",
		Description: "This cluster has use_pg_rewind disabled. Crash the Leader hard enough to diverge its timeline, then run pg_rewind yourself — Patroni won't reattach it for you this time.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `The same crash, a different safety net turned off

The previous lab ("Diverged Timeline — Automatic pg_rewind Recovery") crashed the Leader hard and let Patroni's own "use_pg_rewind: true" quietly fix the diverged node the moment it restarted. This cluster's patroni.yml sets "use_pg_rewind: false" instead — the exact same kind of crash now leaves the node stuck, because Patroni has no automatic tool left to reconcile a diverged timeline with, and (with no pgBackRest configured here either) it won't silently fall back to a full reclone on its own on a plain restart. Bringing that node back is now entirely on you.

pg_rewind is just a CLI tool — Patroni never owned it

"pg_rewind" ships with PostgreSQL itself; Patroni's "use_pg_rewind" setting only controls whether Patroni calls it automatically on your behalf. The tool itself doesn't care who invokes it. Given a target data directory (the diverged node, not currently running) and a source connection (the current Leader), it walks both timelines' history files to find the exact point they forked, then copies back only the data blocks that changed on the target after that point, replaying whatever WAL it needs from the target's own pg_wal to reach consistency first. The result is a data directory that's valid to start as a Replica, without ever needing a full pg_basebackup-style copy of the entire cluster.

Why this lab's command has no "--restore-target-wal"

That flag tells pg_rewind to fetch WAL it can't find locally via the target's own "restore_command" — i.e. from a WAL archive like pgBackRest. This cluster has no archiving configured at all (no pgBackRest, no restore_command), so the flag has nothing to call and pg_rewind refuses outright ("restore_command is not set in the target cluster") if you pass it. Since you're rewinding right after the crash, the WAL it needs is still sitting in the target's own pg_wal — nothing to restore from an archive in the first place.

Why an operator reaches for it by hand

Patroni's own automatic rewind is convenient, but real operations sometimes need to run pg_rewind directly instead: policy might require it disabled by default and only run under a human's supervision after reviewing what diverged; Patroni or the DCS itself might be unreachable (the same class of problem the pgBackRest manual-restore lab covers for backups); or you might simply be diagnosing a divergence before deciding whether rewinding it is even the right call, rather than trusting Patroni to decide that alone at startup.

What actually makes this possible on a hard-killed node

pg_rewind requires the target to reach a consistent state before it can compare timelines — historically that meant the target had to have been shut down cleanly first. Modern pg_rewind (PostgreSQL 11+) handles this itself: given a target that crashed, it runs a brief local crash-recovery pass to reach consistency before comparing timelines, so a "systemctl kill -s KILL patroni" crash — the same rough kill the automatic lab uses — is a perfectly valid starting point, not something you need to work around.`,
		DesignTemplate: labPatroniManualRewindDesign,
		Steps: []LabStep{
			{
				ID:    "crash-leader",
				Title: "Crash the Leader hard",
				Instructions: "This lab's CRUD Traffic (above) is already writing continuously, so the Leader always has some just-committed " +
					"data the Replicas haven't caught up to yet. Find the current Leader with " +
					"`patronictl -c /etc/patroni/postgresql.yml list`, open a terminal on it, and run `systemctl kill -s KILL patroni` " +
					"— a hard kill, not a graceful stop, so PostgreSQL gets no chance to finish streaming what it had already committed " +
					"locally. Wait about 15–30 seconds, confirm from another node that a new Leader was elected, then click Check Work. " +
					"Do NOT start patroni on the crashed node yet — that's the next step, and order matters this time.",
				Hint: "`systemctl stop patroni` still lets Patroni ask PostgreSQL to shut down cleanly first — `kill -s KILL` skips that entirely, which is what actually risks a diverged timeline.",
			},
			{
				ID:    "manual-rewind",
				Title: "Rewind it yourself, then start Patroni",
				Instructions: "This cluster has use_pg_rewind disabled, so simply starting Patroni on the crashed node won't reattach it " +
					"— run pg_rewind by hand first, pointing it at the new Leader (from `patronictl list` on another node):\n\n" +
					"`runuser -u postgres -- pg_rewind --target-pgdata=/var/lib/pgsql/16/data --source-server=\"host=<leader-hostname> " +
					"port=5432 user=postgres dbname=postgres password=postgres_password\"`\n\n" +
					"Wait for it to report \"Done!\", then run `systemctl start patroni` on that same node. Patroni starts PostgreSQL " +
					"against the now-rewound data directory and reconfigures it as a streaming Replica of the current Leader. Confirm with " +
					"`patronictl -c /etc/patroni/postgresql.yml list` that it shows Role: Replica, State: streaming, then click Check Work.",
				Hint: "If pg_rewind refuses with a \"target server needs to be shut down\" error, patroni is probably still trying to restart Postgres on that node — stop it again first, then re-run pg_rewind before starting patroni. Don't add --restore-target-wal: this cluster has no restore_command configured (no pgBackRest/archiving), so that flag just errors out — the WAL pg_rewind needs is still in the target's own pg_wal right after a crash.",
			},
		},
	},
	{
		ID:          "patroni-dcs-loss",
		Title:       "Total DCS Loss — Rebuilding etcd from Scratch",
		Description: "etcd's own storage is gone on all three nodes — not just stopped, erased. PostgreSQL's data is fine. Rebuild the DCS from nothing and get the cluster to recognize its existing data instead of wiping it.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "DCS & Quorum",
		TimeLimit:   "2h",
		LectureNotes: `What actually lives in etcd, and what doesn't

Every earlier disaster in this curriculum leaves PostgreSQL's data directories untouched — even the etcd Quorum Loss lab only ever stops etcd, never erases it. This lab asks a harder question: what if the DCS itself — the leader lock, the cluster's shared configuration, its record of who's even a member — is permanently gone, but every node's PostgreSQL data directory is completely fine? Patroni's own state (namespace "/dbcanvas/", scoped to this cluster's name) lives entirely in etcd; nothing about "who was Leader" or "what this cluster even is" survives on a database node's own disk.

Why you bring back exactly one node first

With an empty DCS, Patroni has no record of this cluster ever existing — from etcd's point of view, any node that starts now looks like it's bootstrapping a brand-new cluster. Patroni's actual behavior when it finds a non-empty, valid data directory alongside an empty DCS is to adopt that data directory as-is rather than reinitializing over it — but only for whichever node manages to register itself as Leader in the (now-blank) DCS first. Start more than one node at once and that's a race: you could end up promoting a Replica that was seconds behind, silently discarding the little bit of extra history the actual former Leader had. Bringing back the former Leader alone, confirming it's Leader, and only then starting the others removes the race entirely.

Why the other two don't just resume as Replicas

Even though their own PostgreSQL data is intact and was a valid Replica of the old cluster a minute ago, the new DCS has no way to know that — as far as it's concerned, this is a different cluster instance that happens to share a name. Patroni treats them the same way it would treat a brand-new node: reclone from the current Leader, using whatever this cluster's create_replica_methods provides. The old data on disk is simply overwritten in the process — this is the one recovery in this curriculum where "the data was fine, but replicate anyway" is expected and correct, not a wasted opportunity to pg_rewind.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "wipe-dcs",
				Title: "Simulate total, unrecoverable DCS loss",
				Instructions: "On all three Patroni nodes, run `systemctl stop patroni etcd`. Then, on all three, wipe etcd's own storage " +
					"— not PostgreSQL's — with `rm -rf /var/lib/etcd/*`. PostgreSQL's data directories are untouched; only the DCS's memory " +
					"of the cluster is gone. Click Check Work.",
				Hint: "Check Work confirms both halves on every node: Patroni unreachable and etcd's data directory actually empty — stopping the services alone isn't enough.",
			},
			{
				ID:    "rebuild-seed-node",
				Title: "Rebuild etcd, then bring back the right node first",
				Instructions: "Restart etcd on all three nodes: `systemctl start etcd`. Because their data directories are empty and their " +
					"config still lists all three peers, they'll bootstrap a brand-new, empty three-member etcd cluster — with no memory of " +
					"anything that happened before. Now the critical part: start Patroni on only ONE node — the one that was Leader before " +
					"this exercise — with `systemctl start patroni`. Do not start the other two yet. Since the DCS has no record of any " +
					"cluster, this node will register itself as the new cluster's Leader using its own existing (undamaged) data — no " +
					"reinitialization, no data loss. Confirm with `patronictl -c /etc/patroni/postgresql.yml list` that it shows up alone " +
					"as Leader, then click Check Work.",
				Hint: "If you start more than one node at once here, whichever one's Patroni reaches etcd first wins Leader — possibly not the one you meant. That race is exactly why production runbooks for this scenario always bring back one trusted node at a time.",
			},
			{
				ID:    "rebuild-remaining",
				Title: "Bring the other two back as fresh Replicas",
				Instructions: "Run `systemctl start patroni` on the remaining two nodes. Each one asks the DCS who the Leader is, finds " +
					"the node from the previous step, and — since its own data directory now belongs to a cluster identity the DCS has " +
					"never heard of — reclones itself from the current Leader rather than trying to reuse its old data. Wait for it to " +
					"finish (a minute or two), confirm with `list` that all three show up healthy, then click Check Work.",
				Hint: "This reclone uses whatever this cluster's create_replica_methods provides — the same basebackup-vs-pgBackRest mechanics from the earlier rebuild labs, just triggered by rejoining a brand-new DCS instead of a manual reinit.",
			},
		},
	},
	{
		ID:          "patroni-scheduled-switchover",
		Title:       "Scheduled Switchover & Flush",
		Description: "Schedule a switchover for a future time instead of running it immediately — then practice both cancelling it and letting it fire on its own.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `A switchover doesn't have to happen right now

Every switchover so far in this curriculum has run immediately. "patronictl switchover --scheduled <timestamp>" instead tells Patroni to carry it out at a specific future moment — you keep working, and the DCS itself remembers to do it. This is the same tool behind "schedule the failover for the maintenance window at 2am" instead of someone needing to be at a keyboard exactly then.

Where the schedule actually lives

Once scheduled, the pending switchover is written into the DCS as its own record — "patronictl list" grows a footer ("Switchover scheduled at: ...") and every member's REST API (":8008/cluster") exposes it under a top-level "scheduled_switchover" key, distinct from the members list. It survives exactly like the leader lock or the pause flag: any member can see it, and it doesn't depend on the connection that requested it staying open.

Cancelling: flush

"patronictl flush <cluster> switchover" deletes that pending record — the cluster carries on exactly as if it had never been scheduled, and nothing about leadership changes. "flush" isn't switchover-specific in general (it also cancels other kinds of scheduled events), but scheduled switchovers and scheduled restarts are the two you'll actually reach for it with.

Why you'd schedule one at all

The obvious use is a real maintenance window. The less obvious one is testing exactly this: confirming your on-call runbook's "cancel the scheduled maintenance" step actually works, before you're relying on it during a real change freeze.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "schedule-and-flush",
				Title: "Schedule a switchover, then cancel it",
				Instructions: "Find the current Leader and a Replica candidate with `patronictl -c /etc/patroni/postgresql.yml list`. Compute a " +
					"timestamp a few minutes out, e.g. `date -u -d '+5 minutes' '+%Y-%m-%dT%H:%M:%S+00:00'`, then schedule a switchover for it: " +
					"`patronictl -c /etc/patroni/postgresql.yml switchover --leader <leader> --candidate <replica> --scheduled <timestamp> --force`. " +
					"Confirm with `list` that the footer shows \"Switchover scheduled at: ...\". Now cancel it before it fires: " +
					"`patronictl -c /etc/patroni/postgresql.yml flush lab-patroni switchover --force`. Confirm the footer is gone, then click Check Work.",
				Hint: "Check Work fails if a switchover is still scheduled, or if leadership already moved — it's checking that the cancellation actually took effect, not just that you typed the flush command.",
			},
			{
				ID:    "schedule-and-let-fire",
				Title: "Schedule one and let it run",
				Instructions: "Schedule another switchover, this time only about a minute out (same `switchover --scheduled` form as before, naming " +
					"a different candidate if you like). This time do NOT flush it — wait for the scheduled time to pass, then confirm with `list` " +
					"that leadership actually moved to the candidate on its own, with nobody running `switchover` at that exact moment. Click Check Work.",
				Hint: "If Check Work still shows the original Leader, double-check the scheduled time has actually passed — Patroni won't act early.",
			},
		},
	},
	{
		ID:          "patroni-nofailover-tag",
		Title:       "Excluding a Replica from Failover",
		Description: "Tag one replica so Patroni will never promote it automatically, by editing its own local config and reloading — not the shared cluster config.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `Two different configs, two different commands

Every earlier config-change lab in this curriculum ("Cluster-wide Configuration Change", "Rolling Restart") used "patronictl edit-config" — that writes to the cluster's shared, DCS-stored configuration, and every member picks up the same value. Per-member tags are the other half of Patroni's configuration: they live in each node's own local "postgresql.yml" (the same file "-c" points patronictl at), under a "tags:" block, and only that one node reads them. Changing them means editing that file directly on that node and telling Patroni to re-read it — "patronictl reload" — rather than editing anything in the DCS.

What "nofailover" actually does

Set "nofailover: true" on a member's local tags and Patroni excludes it from ever becoming a candidate during an automatic election — whether triggered by a crash (failover) or normal health monitoring. It doesn't touch "switchover", which lets you name any member as "--candidate" explicitly regardless of tags; nofailover only narrows who Patroni itself is allowed to pick when nobody names a candidate.

Why you'd want this

A common real case: a replica that's intentionally behind (used for point-in-time analytics queries, or deliberately delayed) or on weaker hardware that shouldn't ever end up serving production writes. Tagging it "nofailover: true" lets it keep replicating and serving reads normally — it's simply removed from the pool Patroni considers when the current Leader disappears.

Why reload, and not a restart

"patronictl reload" only asks Patroni to re-read its local config file — it's a signal, not a process restart, so there's no interruption to PostgreSQL or replication. Static Postgres parameters (like the ones in the Rolling Restart lab) genuinely need a restart to take effect; per-member Patroni tags like this one take effect the moment Patroni re-reads its own file.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "tag-nofailover",
				Title: "Tag a replica nofailover and reload it",
				Instructions: "Pick a Replica (not the Leader) with `patronictl -c /etc/patroni/postgresql.yml list`. Open a terminal on that node " +
					"and edit its local config: `sed -i 's/nofailover: false/nofailover: true/' /etc/patroni/postgresql.yml`. Apply it with " +
					"`patronictl -c /etc/patroni/postgresql.yml reload lab-patroni <that-node> --force`. Confirm with `list` that its Tags column " +
					"now shows \"nofailover: true\", then click Check Work.",
				Hint: "Reload can take a few seconds to apply — if the Tags column is still blank, wait and check `list` again before assuming it didn't work.",
			},
			{
				ID:    "crash-and-confirm",
				Title: "Crash the Leader and confirm it's skipped",
				Instructions: "Find the current Leader with `list` and run `systemctl stop patroni` on it. Wait about 15-30 seconds for the " +
					"remaining two nodes to elect a new Leader, then confirm with `list` that the tagged node is still a Replica, not the new " +
					"Leader. Click Check Work.",
				Hint: "If the tagged node somehow became Leader, double-check step one actually took — `list`'s Tags column is the source of truth, not just that you ran the sed command.",
			},
		},
	},
	{
		ID:          "patroni-decommission",
		Title:       "Decommissioning a Patroni Cluster",
		Description: "Remove a cluster from the DCS entirely — the real decommission workflow, including the part every runbook forgets: doing it while Patroni is still running just gets undone within seconds.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "DCS & Quorum",
		TimeLimit:   "2h",
		LectureNotes: `"remove" doesn't stop anything by itself

"patronictl remove" deletes a cluster's entire record from the DCS — the leader lock, the shared config, every member's registration. What it does NOT do is stop Patroni on any node. If Patroni is still running when you remove its DCS record, nothing durable happens: each member's own HA loop (every "loop_wait", 10 seconds in this cluster) re-registers itself the moment it next checks in, silently recreating almost everything you just deleted.

Why this lab has you stop Patroni first

To make a removal actually stick, every member's Patroni process has to be stopped before you remove the cluster — with nothing left running to re-register it. This is the real-world order of operations for retiring a Patroni-managed cluster: stop Patroni everywhere, then remove the DCS record, in that order. Skipping the first step is the most common way "I decommissioned it" turns out not to have worked.

Stopping Patroni here means the databases go down too

The Failover lab's notes already covered this: in this cluster, Patroni doesn't sit next to PostgreSQL as an independent, separately-managed service — it starts PostgreSQL itself as a supervised child process. So "decommissioning" this cluster genuinely ends with every node fully offline, not "still quietly serving reads with no HA layer watching it." Some real-world Patroni deployments run PostgreSQL as its own separate systemd unit that Patroni merely adopts rather than spawns — in that shape, decommissioning Patroni really can leave the databases running unmanaged. This cluster isn't built that way, so don't expect that middle state here.

The confirmation prompts are deliberately annoying

"remove" has no non-interactive/"--force" form — it always asks you to retype the cluster's name, then type the literal phrase "Yes I am aware", and (if the DCS's last known state still looked healthy) name the current Leader too. That friction is intentional: unlike almost every other patronictl command in this curriculum, there's no scriptable way to remove a cluster by accident.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "stop-everywhere",
				Title: "Stop Patroni on every node",
				Instructions: "Open a terminal on each of the three Patroni nodes and run `systemctl stop patroni` on all three (leave etcd " +
					"running — you're not touching the DCS's own storage, just Patroni, and the PostgreSQL it supervises on each node). Wait " +
					"about 30-40 seconds so the old leader lock naturally expires too, then click Check Work.",
				Hint: "Check Work confirms none of the three still answer as Leader or Replica over their own REST API — stopping the service is what actually matters, not just that a moment has passed.",
			},
			{
				ID:    "remove-cluster",
				Title: "Remove the cluster from the DCS",
				Instructions: "On any node, run `patronictl -c /etc/patroni/postgresql.yml remove lab-patroni` and follow the prompts: type the " +
					"cluster name (\"lab-patroni\") to confirm, then type the literal phrase `Yes I am aware` when asked. (Since the cluster no " +
					"longer looks healthy once the leader lock has expired, it likely won't also ask you to name the current Leader — if it does, " +
					"use whichever node `list` showed as Leader before you stopped Patroni.) Once it finishes, confirm the DCS itself has nothing " +
					"left under this cluster's key prefix: `ETCDCTL_API=3 etcdctl --endpoints=http://127.0.0.1:2379 get /dbcanvas/lab-patroni " +
					"--prefix --keys-only` should print nothing. Click Check Work.",
				Hint: "If `remove` refuses or times out, double-check etcd is still running on that node (`systemctl status etcd`) — only Patroni was supposed to be stopped, not the DCS itself.",
			},
		},
	},
	{
		ID:          "patroni-cascading-replication",
		Title:       "Cascading Replication Topology",
		Description: "Point one replica at another replica instead of the Leader, so it streams second-hand — the same fan-out pattern used to keep a remote region's replicas off the primary's direct connection count.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `Every replica so far has streamed from the Leader directly

That's Patroni's default and it's fine for a small, single-site cluster like this one. It stops being fine once replicas are spread across regions: every one of them opening its own direct connection to the primary multiplies the primary's replication connection count and its outbound WAL bandwidth, once per remote replica, no matter how many of them are actually in the same remote datacenter.

replicatefrom: streaming from a peer instead

Setting the "replicatefrom" tag (a per-member local tag, the same kind of setting the nofailover lab covers — edited in "postgresql.yml", applied with "patronictl reload") on a replica tells Patroni to configure its "primary_conninfo" against another cluster member instead of the Leader. That member becomes a cascading upstream: it streams from the Leader as normal, and now also serves as the replication source for whichever peers name it. The primary's connection count and bandwidth only grow with the number of direct upstreams, not the total replica count.

Seeing it happen

"patronictl topology" draws the member tree with indentation reflecting exactly this — a cascading replica nests one level deeper than its upstream, instead of every replica appearing as a flat sibling under the Leader. The underlying fact is visible in PostgreSQL itself too: "pg_stat_replication" on the upstream member gains a new row for the cascading replica, while the Leader's own "pg_stat_replication" no longer lists it at all.

The trade-off

A cascading replica's data is now one hop further from the Leader — if its upstream peer has any replication lag, the cascading replica inherits that lag on top of its own. For a same-site cluster like this lab's, that trade-off rarely makes sense; it's specifically a WAN/multi-region tool, trading a little extra lag for a lot less connection and bandwidth load on the primary.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "cascade-replica",
				Title: "Cascade one replica behind another",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml list` to see the current Leader and its two Replicas. Pick one " +
					"Replica to become the cascading node, and the other to be its upstream. On the cascading node, edit its local config: " +
					"`sed -i 's/clonefrom: false/clonefrom: false\\n  replicatefrom: <upstream-node-name>/' /etc/patroni/postgresql.yml` (replace " +
					"`<upstream-node-name>` with the other Replica's name, e.g. `pg-node-2`). Apply it: " +
					"`patronictl -c /etc/patroni/postgresql.yml reload lab-patroni <cascading-node> --force`. Wait about 15-20 seconds, then run " +
					"`patronictl -c /etc/patroni/postgresql.yml topology` and confirm the cascading node is now indented under its upstream, not " +
					"the Leader. Click Check Work.",
				Hint: "Check Work confirms it via `pg_stat_replication`, not just the topology drawing — the upstream node should show the cascading node as a subscriber, and the Leader should not.",
			},
		},
	},
	{
		ID:          "patroni-incident-history",
		Title:       "Reconstructing Incident History",
		Description: "After a scripted crash and a planned handover, use patronictl history to reconstruct exactly what happened and in what order — the same audit trail a real postmortem would start from.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Leadership & Failover",
		TimeLimit:   "2h",
		LectureNotes: `Live state only tells you where things stand now

"patronictl list" answers "who is Leader right now" — it has no memory of how the cluster got there. Every earlier lab in this curriculum checks the live state (has leadership moved, is a leader reachable), which is exactly right for verifying an action's immediate effect, but it can't answer "how many leadership changes have happened, and in what order" once time has passed.

history is the DCS's own audit trail

Every time Patroni's leader changes — whether an automatic failover or a requested switchover — it appends a record to a history kept in the DCS itself: the timeline number, the LSN at the moment of the change, a reason, a timestamp, and who the new Leader became. "patronictl history" reads that log back, oldest first. Unlike "list", it survives regardless of which node you happen to be Leader is right now, because it isn't derived from current state at all.

Why this matters operationally

A real postmortem after an incident almost never starts with "what does the cluster look like now" — by the time anyone's investigating, it's already stabilized. It starts with "what actually happened, in order, and when." "patronictl history" is that record for Patroni specifically: distinct from PostgreSQL's own logs, distinct from your monitoring's alert history, and always available as long as the DCS itself survived the incident.

One log, two very different causes

The history log doesn't distinguish "a planned switchover you asked for" from "an unplanned failover after a crash" — both just show up as a timeline change with a reason and a new Leader. Telling them apart means correlating the history log's timestamps against what you know you did (or didn't do) at that time — which is exactly the exercise this lab sets up.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "cause-two-changes",
				Title: "Cause an unplanned failover, then a planned switchover",
				Instructions: "First, an unplanned change: find the current Leader with `patronictl -c /etc/patroni/postgresql.yml list` and run " +
					"`systemctl stop patroni` on it. Wait 15-30 seconds for the remaining two nodes to elect a new Leader. Then, a planned " +
					"change: run `patronictl -c /etc/patroni/postgresql.yml switchover --force` and follow the prompts to hand leadership to a " +
					"different node. Confirm with `list` that a third node is now Leader (all three should have held it at some point across this " +
					"lab), then click Check Work.",
				Hint: "If Check Work says too few changes are recorded, confirm both the crash and the switchover actually completed — `list` should show a different Leader after each one.",
			},
			{
				ID:    "read-history",
				Title: "Reconstruct the sequence from history",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml history` and read it top to bottom — each row is one leadership " +
					"change, oldest first, with the timeline number, the reason, when it happened, and who became Leader. Confirm it shows at " +
					"least two changes since the cluster started, ending with the node you just switched over to. Click Check Work.",
				Hint: "`history -f json` (any patronictl command accepts `-f json`) gives you the same records as a plain array if you want to look at the raw fields.",
			},
		},
	},
	{
		ID:          "patroni-standby-cluster",
		Title:       "Standby Cluster: Following an External Primary",
		Description: "Demote this entire Patroni cluster to follow a PostgreSQL primary Patroni doesn't manage, then promote it back to run fully independent.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `Everything so far has been one self-contained cluster

Every other lab in this curriculum treats "lab-patroni" as the whole picture: it elects its own Leader, and that Leader is the ultimate source of truth. A "standby cluster" is different — the whole 3-node Patroni cluster follows a PostgreSQL primary that lives outside Patroni's management entirely (this lab's "external-primary" node, a plain standalone PostgreSQL instance). One member becomes a "standby leader" that streams directly from the external primary; the other two stream from the standby leader exactly like normal replicas stream from an ordinary Leader.

demote-cluster: pointing the whole cluster at someone else

"patronictl demote-cluster --host <host> --port <port>" reconfigures the entire cluster to follow that external address. This is a live, dynamic change — this cluster didn't need to be designed as a standby cluster from the start; any running Patroni cluster can be demoted into following an external primary, and (as the next step shows) promoted back out of it.

Why you'd want a whole cluster following an external primary

The real-world case is disaster recovery across two systems that don't share Patroni's DCS: a warm-standby copy of a database that lives on completely different infrastructure (a different cloud provider, a system migrating away from Patroni, or a primary managed by different tooling entirely) that you still want automatic failover among your own replicas for, without your cluster ever becoming the source of truth.

promote-cluster: cutting the cord

"patronictl promote-cluster" reverses it — the standby leader stops following the external primary and becomes a fully independent Leader, with its own timeline going forward. This is the moment you'd reach for during an actual disaster recovery cutover: the external primary is gone (or you're deliberately migrating away from it), and this cluster needs to become the source of truth in its own right rather than a follower.`,
		DesignTemplate: labPatroniStandbyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "demote-to-standby",
				Title: "Demote the cluster to follow the external primary",
				Instructions: "This lab's `external-primary` node is a plain standalone PostgreSQL instance, already configured to accept a " +
					"replication connection. From any Patroni node, run `patronictl -c /etc/patroni/postgresql.yml demote-cluster lab-patroni " +
					"--host external-primary.example.net --port 5432 --force`. Wait about 20-30 seconds, then confirm with " +
					"`patronictl -c /etc/patroni/postgresql.yml list` that one member now shows role \"Standby Leader\" (streaming directly from " +
					"the external primary) with the other two as ordinary Replicas beneath it. Click Check Work.",
				Hint: "If it won't demote, confirm the external primary is actually reachable first: `psql -h external-primary.example.net -U postgres -c \"select 1;\"` from any Patroni node.",
			},
			{
				ID:    "promote-independent",
				Title: "Promote the cluster back to independent",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml promote-cluster lab-patroni --force`. Wait about 15-20 seconds, " +
					"then confirm with `list` that the former Standby Leader now shows as an ordinary Leader — no longer following the external " +
					"primary — with its own timeline moving forward. Click Check Work.",
				Hint: "A higher timeline number than before the demotion (visible in `list`'s TL column) is expected — that's the promotion breaking a fresh timeline, the same way any promotion does.",
			},
		},
	},
	{
		ID:          "patroni-noloadbalance-drain",
		Title:       "Draining Reads with noloadbalance",
		Description: "Tag a replica so HAProxy's read pool stops sending it traffic, without touching HAProxy itself — then watch its Retrieve line in the CRUD Traffic graph go quiet.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `HAProxy never decides who's in the read pool — Patroni does

This lab's HAProxy read front-end (:5001) health-checks every member with "GET /replica" and only routes to whoever answers 200. It has no independent opinion about which replicas are "good" — it's entirely downstream of what Patroni's own REST API reports. Draining a node from the read pool is a Patroni-side decision, not an HAProxy config change.

noloadbalance: the tag that flips the health check

The "noloadbalance" per-member tag (edited locally, applied with "patronictl reload" — the same mechanism as the nofailover and replicatefrom labs) tells that member's own Patroni to answer /replica with a failing status even while it's a perfectly healthy, streaming replica. HAProxy's health check starts failing within a few check intervals, marks that backend server DOWN, and — with "on-marked-down shutdown-sessions" already in this lab's haproxy.cfg — even resets whatever read connections were already assigned to it.

Why you'd deliberately drain a healthy replica

A replica you're about to take down for maintenance, or one you want to reserve for a heavy ad-hoc analytics query without it fighting production read traffic for resources, is a healthy node you still don't want serving application reads. noloadbalance lets you pull it out of rotation cleanly and put it back the same way — nothing about its replication status changes, only its eligibility for load-balanced reads.

What this is not

noloadbalance doesn't touch nofailover — a drained replica can still be elected Leader if the current one disappears (nofailover is what would exclude it from that separately). Draining and failover-eligibility are two independent knobs on the same member.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "drain-replica",
				Title: "Drain a replica from the read pool",
				Instructions: "Pick a Replica (not the Leader) with `patronictl -c /etc/patroni/postgresql.yml list`. Open a terminal on it and edit " +
					"its local config: `sed -i 's/noloadbalance: false/noloadbalance: true/' /etc/patroni/postgresql.yml`. Apply it with " +
					"`patronictl -c /etc/patroni/postgresql.yml reload lab-patroni <that-node> --force`. Watch that node's Retrieve line in the " +
					"CRUD Traffic graph above flatten out over the next few seconds as HAProxy's health check starts failing it. Click Check Work.",
				Hint: "You can confirm it directly too: `curl -o /dev/null -w '%{http_code}\\n' http://127.0.0.1:8008/replica` on that node should now print something other than 200.",
			},
			{
				ID:    "undrain-replica",
				Title: "Put it back in rotation",
				Instructions: "On the same node, run `sed -i 's/noloadbalance: true/noloadbalance: false/' /etc/patroni/postgresql.yml` then " +
					"`patronictl -c /etc/patroni/postgresql.yml reload lab-patroni <that-node> --force` again. Give HAProxy's health check a few " +
					"seconds to mark it back up, then click Check Work.",
				Hint: "If Check Work still fails after a bit, double-check the tag actually flipped back — `list`'s Tags column should show no noloadbalance entry (or `noloadbalance: false`) for that node.",
			},
		},
	},
	{
		ID:          "patroni-permanent-slots",
		Title:       "Permanent Replication Slots",
		Description: "Create a replication slot that survives failover — the tool CDC consumers (Debezium and friends) actually depend on, since an ordinary slot just vanishes with the Leader that created it.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Replication",
		TimeLimit:   "2h",
		LectureNotes: `The slots you've already seen aren't the kind this lab is about

Every Patroni cluster in this curriculum runs with "use_slots: true" — that's what keeps each Replica's own slot ("pg_node_1", "pg_node_2", etc., visible in "pg_replication_slots") so the Leader retains exactly the WAL each Replica still needs. Those slots are entirely Patroni's own bookkeeping: created automatically per member, and gone the moment that member is gone. They're not meant for anything external to attach to.

A permanent slot is different: it's yours

"patronictl edit-config --set slots.<name>.type=physical" (or "=logical" for logical replication) declares a slot that Patroni itself doesn't own the lifecycle of — it exists because you told the cluster's shared config it should, independent of any particular member. A logical-replication consumer (Debezium, a custom CDC pipeline, "pg_recvlogical") holds onto a slot exactly like this one so the Leader retains the WAL it hasn't consumed yet.

Why "permanent" is the whole point

An ordinary slot created directly on today's Leader (bypassing Patroni's config) vanishes the instant that Leader stops being Leader — nothing recreates it on whoever takes over. A permanent slot is different: Patroni watches the shared config's "slots:" section and makes sure whichever member is currently Leader has a slot by that name, recreating it on the new Leader within moments of any switchover or failover. Your CDC consumer's connection still breaks (it was talking to a specific host), but the slot — and the WAL retention it guarantees — is waiting for it to reconnect to whoever's Leader now.

The real failure mode this prevents

Without a permanent slot, a failover during a CDC pipeline's downtime is catastrophic: the WAL it still needed was only retained by a slot that no longer exists anywhere, and the consumer has an unrecoverable gap. With one, the retention guarantee survives the failover even though the consumer itself has to reconnect.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "create-slot",
				Title: "Create a permanent slot",
				Instructions: "Open a terminal on any Patroni node. Run " +
					"`patronictl -c /etc/patroni/postgresql.yml edit-config lab-patroni --set slots.lab_permanent_slot.type=physical --force`. " +
					"Confirm it exists on the current Leader: `psql -U postgres -c \"select slot_name, slot_type, active from " +
					"pg_replication_slots;\"` should list `lab_permanent_slot` alongside the automatic per-member ones. Click Check Work.",
				Hint: "The slot shows active = f — that's expected, nothing is consuming it yet. Patroni still keeps it retaining WAL either way.",
			},
			{
				ID:    "slot-survives-failover",
				Title: "Switch over and confirm the slot follows",
				Instructions: "Run `patronictl -c /etc/patroni/postgresql.yml switchover --force` and follow the prompts to hand leadership to a " +
					"different node. Once it settles, confirm on the new Leader: `psql -U postgres -c \"select slot_name from " +
					"pg_replication_slots where slot_name = 'lab_permanent_slot';\"` should still return a row. Click Check Work.",
				Hint: "If the slot is missing on the new Leader, give Patroni a few more seconds — it recreates permanent slots on its next HA loop iteration after a leadership change, not instantaneously.",
			},
		},
	},
	{
		ID:          "patroni-rest-api",
		Title:       "Driving Patroni via Its REST API",
		Description: "patronictl is just a client — talk to Patroni's own REST API directly with curl instead, the same API HAProxy's health checks and every Check Work in this curriculum already depend on.",
		Difficulty:  "Beginner",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `You've been using this API the whole time without touching it directly

Every "patronictl" command in this curriculum is a thin client around Patroni's REST API on port 8008. "patronictl list" calls "GET /cluster"; "patronictl switchover" calls "POST /switchover"; "patronictl edit-config" calls "PATCH /config". HAProxy's own health checks ("GET /primary", "GET /replica") are this same API too — it's not a separate monitoring integration, it's the exact same interface. This lab has you call it yourself instead of through patronictl.

No authentication, on purpose for this lab environment

This cluster's Patroni REST API takes no credentials — any request that reaches port 8008 is accepted. Production deployments typically lock this down (restapi.authentication in patroni.yml, or a firewall restricting who can reach 8008 at all), precisely because "POST /switchover" and "PATCH /config" are real, consequential actions — this lab's open access is a simplification for learning the API's shape, not a pattern to copy into a real deployment.

Why you'd ever reach for the raw API instead of patronictl

Any tool that needs to integrate with Patroni programmatically — a custom health dashboard, an internal automation script, a different orchestrator entirely — talks to this API directly rather than shelling out to patronictl. Understanding that "GET /replica" is the literal signal HAProxy's read pool depends on (the same one the noloadbalance lab's Check Work watches) makes every other lab's mechanics a little less like magic.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "rest-switchover",
				Title: "Switch over via a raw POST, not patronictl",
				Instructions: "Find the current Leader by probing each node directly — `curl -o /dev/null -w '%{http_code}\\n' " +
					"http://127.0.0.1:8008/primary` prints 200 only on the Leader, 503 everywhere else. On any node, POST a switchover " +
					"directly: `curl -X POST http://127.0.0.1:8008/switchover -H \"Content-Type: application/json\" -d " +
					"'{\"leader\": \"<current-leader-name>\", \"candidate\": \"<a-replica-name>\"}'`. Confirm with `patronictl -c " +
					"/etc/patroni/postgresql.yml list` that leadership moved, then click Check Work.",
				Hint: "A 200 response body of `Successfully switched over to \"<name>\"` means it worked — anything else (e.g. a members mismatch) means the leader/candidate names you posted didn't match what the cluster actually reports.",
			},
			{
				ID:    "rest-patch-config",
				Title: "Change the shared config via a raw PATCH",
				Instructions: "On any node, run `curl -X PATCH http://127.0.0.1:8008/config -d '{\"ttl\": 45}'`. Confirm it took effect by " +
					"reading the config back — `curl http://127.0.0.1:8008/config` (from any node — it's a shared, DCS-backed object) should now " +
					"show `\"ttl\": 45`. Click Check Work.",
				Hint: "This is exactly what `patronictl edit-config --set ttl=45` would have done under the hood — PATCH only needs to name the fields you're changing, not the whole config.",
			},
		},
	},
	{
		ID:          "patroni-failover-priority",
		Title:       "Steering Elections with failover_priority",
		Description: "nofailover excludes a replica entirely — failover_priority is the softer tool: rank candidates so your preferred one wins when several are equally caught up.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Leadership & Failover",
		TimeLimit:   "2h",
		LectureNotes: `Binary exclusion vs. a preference ranking

The "Excluding a Replica from Failover" lab covered nofailover: a member either can or can't ever be elected, full stop. failover_priority is the finer-grained tool for the far more common case — you don't want to rule any replica out, you just want to steer which one wins when it's a close call. It's a per-member local tag, edited and applied exactly like nofailover and replicatefrom: the file, then "patronictl reload".

How Patroni actually uses it

When an election happens, Patroni's first and overriding criterion is always how caught-up each candidate's WAL replay is — a replica that's meaningfully behind never wins just because it has a high failover_priority. Among candidates that are equally (or close enough to equally) caught up, failover_priority breaks the tie: higher wins, default is 0 for every untagged member.

Why you'd want this instead of nofailover

Picture two replicas: one on beefier hardware you'd genuinely prefer as the next Leader, and one you'd still accept in a pinch if the first one were unreachable. nofailover on the weaker one would remove it as a safety net entirely. failover_priority lets you express "prefer this one" while keeping every replica eligible — the weaker one only ever wins if the preferred one genuinely isn't a viable candidate at that moment.

A tag, not a guarantee

failover_priority only matters when Patroni is actually choosing among multiple caught-up candidates. It's a preference among equals, not an override of the replication-lag check that comes first — that ordering is exactly why it's safe to set without worrying it could promote a replica that's dangerously behind.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "tag-priority",
				Title: "Give a replica a higher failover_priority",
				Instructions: "Pick a Replica (not the Leader) with `patronictl -c /etc/patroni/postgresql.yml list`. Open a terminal on it and " +
					"add the tag: `sed -i '/nosync: false/a\\  failover_priority: 5' /etc/patroni/postgresql.yml`. Apply it with " +
					"`patronictl -c /etc/patroni/postgresql.yml reload lab-patroni <that-node> --force`. Confirm with `list` that its Tags " +
					"column shows `failover_priority: 5`, then click Check Work.",
				Hint: "Reload can take a few seconds to apply — if the Tags column is still blank, wait and check `list` again before assuming it didn't work.",
			},
			{
				ID:    "crash-and-confirm",
				Title: "Crash the Leader and confirm it wins",
				Instructions: "Find the current Leader with `list` and run `systemctl stop patroni` on it (make sure it isn't the node you just " +
					"tagged). Wait about 15-30 seconds for the remaining two nodes to elect a new Leader, then confirm with `list` that the " +
					"tagged node — not the other untouched replica — became Leader. Click Check Work.",
				Hint: "If the untagged replica won instead, double-check you crashed the actual Leader (not the tagged node itself) — `list`'s Role column is the source of truth.",
			},
		},
	},
	{
		ID:          "patroni-scheduled-restart",
		Title:       "Scheduled Restart & Flush",
		Description: "Schedule a PostgreSQL restart for a future maintenance window instead of running it immediately — then practice both cancelling it and letting it fire.",
		Difficulty:  "Intermediate",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `The same scheduling mechanism, a different action

The Scheduled Switchover lab covered scheduling a leadership handover for a future time. "patronictl restart --scheduled <timestamp>" applies the identical idea to a plain restart — useful for exactly the case the Rolling Restart lab's static-parameter change (max_connections) leaves you with: every member flagged "Pending restart", and you'd rather that happen at 2am than the moment you finish the config change.

Where a scheduled restart actually lives

Unlike a scheduled switchover (a single cluster-wide record), a scheduled restart is per-member — visible in that specific member's own REST API response ("scheduled_restart", alongside its "postmaster_start_time") and as a "Scheduled restart" column in "patronictl list". Schedule restarts on multiple members and each one fires independently at its own requested time.

Cancelling: flush, the same tool either way

"patronictl flush <cluster> restart" cancels scheduled restarts across the cluster, exactly like "flush ... switchover" cancels a scheduled switchover — "flush" is the general-purpose "cancel whatever's scheduled" command, not two unrelated features that happen to share a name.

Why you'd schedule a restart specifically

A static parameter change (the Rolling Restart lab's max_connections) needs a restart to take effect but doesn't need it *now* — scheduling it lets you make the config change the moment you decide on it, while the actual client-visible interruption happens later, during an actual announced maintenance window, on your terms.`,
		DesignTemplate: labPatroniSwitchoverDesign,
		Steps: []LabStep{
			{
				ID:    "schedule-and-flush",
				Title: "Schedule a restart, then cancel it",
				Instructions: "Pick any node and compute a timestamp a few minutes out: `date -u -d '+5 minutes' '+%Y-%m-%dT%H:%M:%S+00:00'`. " +
					"Schedule a restart on it: `patronictl -c /etc/patroni/postgresql.yml restart lab-patroni <node-name> --scheduled " +
					"<timestamp> --force`. Confirm with `list` that its \"Scheduled restart\" column is populated. Now cancel it: " +
					"`patronictl -c /etc/patroni/postgresql.yml flush lab-patroni restart --force`. Confirm the column is empty again, then " +
					"click Check Work.",
				Hint: "Check Work fails if any member still has a restart scheduled — it's checking the cancellation actually took effect.",
			},
			{
				ID:    "schedule-and-let-fire",
				Title: "Schedule one and let it run",
				Instructions: "Schedule another restart, this time only about a minute out. Do NOT flush it — wait for the scheduled time to " +
					"pass, then confirm with `curl http://127.0.0.1:8008/patroni` on that node that `postmaster_start_time` is now recent and " +
					"`scheduled_restart` is gone. Click Check Work.",
				Hint: "If it still looks unfired after a couple of minutes, double-check the scheduled time has actually passed — Patroni won't act early.",
			},
		},
	},
	{
		ID:          "patroni-callbacks",
		Title:       "Patroni Callbacks: Reacting to Role Changes",
		Description: "Wire a script into Patroni's on_role_change callback and watch it fire for real during a failover — the hook real deployments use to update DNS, page someone, or notify a service registry.",
		Difficulty:  "Advanced",
		Database:    "PostgreSQL",
		Technology:  "Patroni",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `Every check in this curriculum has been Patroni's state — this lab is Patroni's actions

Every other lab verifies something by reading cluster state after the fact (who's Leader, what a tag says, what a slot looks like). Callbacks are different: they're code Patroni itself runs, synchronously, at the moment specific lifecycle events happen — "on_start", "on_stop", "on_restart", and the one this lab is about, "on_role_change" (fired whenever a member's role changes, most notably becoming Leader).

What Patroni actually passes your script

Patroni execs the configured script as "<script> <action> <role> <cluster_name>" — this lab's cluster runs a script that just appends that line, verbatim, to /tmp/patroni-callback.log on whichever member ran it. Nothing here is application-specific; production callbacks receive the exact same three arguments and use them to decide what to do ("primary" is the role value this Patroni version reports for a Leader — some other versions/configurations report "master" instead, so real callback scripts typically handle both).

Why this is how real integrations hook into Patroni

DNS updates ("point the write CNAME at whoever's Leader now"), paging a human, updating a service registry, or emitting a metrics event on every failover — all of these are ordinary on_role_change callbacks in real deployments. Patroni doesn't have a plugin system beyond this: a callback is just an executable it runs at the right moment, and what that executable does is entirely up to you.

Why nothing is logged until you actually cause a failover

on_role_change fires on a genuine role transition — it does not fire during a fresh node's initial bootstrap, since there's no previous role for that to be a change from. So right after this lab's cluster first comes up, every member's callback log is simply empty; the first line any of them ever gets is the one this lab's crash produces on whichever node gets promoted.`,
		DesignTemplate: labPatroniCallbacksDesign,
		Steps: []LabStep{
			{
				ID:    "cause-role-change",
				Title: "Crash the Leader and check the new Leader's callback log",
				Instructions: "Find the current Leader with `patronictl -c /etc/patroni/postgresql.yml list` and run `systemctl stop patroni` on " +
					"it. Wait about 15-30 seconds for the remaining two nodes to elect a new Leader, then open a terminal on that new Leader and " +
					"run `cat /tmp/patroni-callback.log` — it was empty before this (on_role_change never fired at bootstrap), and should now " +
					"show one line with `action=on_role_change` from this promotion. Click Check Work.",
				Hint: "If the log has no on_role_change line yet, give it a few more seconds — the callback fires as part of the promotion sequence, which can take a moment after the election itself completes.",
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
			// startLabTraffic is idempotent (no-ops if already running for this
			// stack) — needed here because the generator is purely in-memory: an
			// app restart drops it silently, and without this a learner resuming
			// an already-active run would see "Resume Lab" succeed but CRUD
			// Traffic (and its per-node graph) just never come back.
			go a.startLabTraffic(run.ID, st.ID)
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
	go a.captureLabInitialBackupCount(run.ID, st.ID)
	go a.startLabTraffic(run.ID, st.ID)
	go a.prepareStandbyClusterUpstream(run.ID, st.ID)
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

// captureLabInitialBackupCount polls until pgBackRest reports its automatic
// initial backup (taken right after the cluster's stanza is created), then
// records the count as the baseline the Backup & Restore lab's first Check
// Work compares against — otherwise checking immediately after deploy would
// trivially "pass" on that automatic backup alone. A no-op (returns quickly)
// for every other lab, whose stacks have pgBackRest disabled.
func (a *App) captureLabInitialBackupCount(runID, stackID int64) {
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
		doc, frame, ok := patroniFrameFromStack(st)
		if !ok || !frame.UsePgBackRest {
			return
		}
		running, err := a.runningPatroniMembers(st, doc)
		if err != nil || len(running) == 0 {
			continue
		}
		count, ok := a.pgBackRestBackupCount(ctx, patroniStanza(frame.Label), running[0].ContainerID)
		if !ok {
			continue
		}
		a.store.SetLabRunBackupCount(runID, count)
		return
	}
}

// pgBackRestInfo is the minimal shape of `pgbackrest info --output=json`
// (one entry per stanza) this app reads — just enough to count backups.
type pgBackRestInfo struct {
	Backup []struct{} `json:"backup"`
}

// pgBackRestBackupCount runs `pgbackrest info` on the given container and
// returns how many backups exist for the stanza. ok is false if the command
// failed or returned something unparseable (e.g. pgBackRest isn't installed
// on this lab's image, or the stanza isn't ready yet).
func (a *App) pgBackRestBackupCount(ctx context.Context, stanza, containerID string) (int, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID,
		[]string{"pgbackrest", "--stanza=" + stanza, "info", "--output=json"}, nil)
	if err != nil || res.Code != 0 {
		return 0, false
	}
	var info []pgBackRestInfo
	if json.Unmarshal([]byte(res.Stdout), &info) != nil || len(info) == 0 {
		return 0, false
	}
	return len(info[0].Backup), true
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
	case "patroni-rolling-restart:rolling-restart":
		result = a.checkPatroniRollingRestart(ctx, st)
	case "patroni-manual-failover:pause-and-crash":
		result = a.checkPausedNoLeader(ctx, st)
	case "patroni-manual-failover:failover-and-resume":
		result = a.checkLeaderPresentAndResumed(ctx, st)
	case "patroni-synchronous-replication:enable-sync":
		result = a.checkSynchronousReplication(ctx, st)
	case "patroni-failsafe-mode:enable-failsafe":
		result = a.checkFailsafeModeEnabled(ctx, st)
	case "patroni-failsafe-mode:break-etcd-only":
		result = a.checkFailsafeLeaderHeld(ctx, run, st)
	case "patroni-failsafe-mode:restore-etcd":
		result = a.checkPatroniLeaderPresent(ctx, st)
	case "patroni-backup-restore:take-backup":
		result = a.checkPgBackRestFreshBackup(ctx, run, st)
	case "patroni-backup-restore:restore-replica":
		result = a.checkPatroniClusterHealthy(ctx, st)
	case "patroni-basebackup-rebuild:wipe-replica", "patroni-pgbackrest-manual-restore:wipe-replica":
		result = a.checkReplicaDataWiped(ctx, st)
	case "patroni-basebackup-rebuild:rebuild-basebackup", "patroni-pgbackrest-manual-restore:manual-restore":
		result = a.checkPatroniClusterHealthy(ctx, st)
	case "patroni-pg-rewind:crash-leader", "patroni-pg-rewind-manual:crash-leader":
		result = a.checkLeaderChanged(ctx, run, st)
	case "patroni-pg-rewind:rejoin-rewind", "patroni-pg-rewind-manual:manual-rewind":
		result = a.checkPgRewindRecovered(ctx, run, st)
	case "patroni-dcs-loss:wipe-dcs":
		result = a.checkAllPatroniAndEtcdWiped(ctx, st)
	case "patroni-dcs-loss:rebuild-seed-node":
		result = a.checkOriginalLeaderReborn(ctx, run, st)
	case "patroni-dcs-loss:rebuild-remaining":
		result = a.checkPatroniClusterHealthy(ctx, st)
	case "patroni-scheduled-switchover:schedule-and-flush":
		result = a.checkSwitchoverScheduleFlushed(ctx, run, st)
	case "patroni-scheduled-switchover:schedule-and-let-fire":
		result = a.checkLeaderChanged(ctx, run, st)
	case "patroni-nofailover-tag:tag-nofailover":
		result = a.checkNofailoverTagged(ctx, st)
	case "patroni-nofailover-tag:crash-and-confirm":
		result = a.checkNofailoverNeverPromoted(ctx, st)
	case "patroni-decommission:stop-everywhere":
		result = a.checkAllPatroniStopped(ctx, st)
	case "patroni-decommission:remove-cluster":
		result = a.checkClusterRemovedFromDCS(ctx, st)
	case "patroni-cascading-replication:cascade-replica":
		result = a.checkCascadingReplica(ctx, st)
	case "patroni-incident-history:cause-two-changes", "patroni-incident-history:read-history":
		result = a.checkIncidentHistoryRecorded(ctx, st)
	case "patroni-standby-cluster:demote-to-standby":
		result = a.checkStandbyLeaderPresent(ctx, st)
	case "patroni-standby-cluster:promote-independent":
		result = a.checkClusterPromotedIndependent(ctx, st)
	case "patroni-noloadbalance-drain:drain-replica":
		result = a.checkReplicaDrained(ctx, st)
	case "patroni-noloadbalance-drain:undrain-replica":
		result = a.checkReplicaUndrained(ctx, st)
	case "patroni-permanent-slots:create-slot":
		result = a.checkPermanentSlotOnLeader(ctx, st)
	case "patroni-permanent-slots:slot-survives-failover":
		result = a.checkPermanentSlotSurvivedFailover(ctx, run, st)
	case "patroni-rest-api:rest-switchover":
		result = a.checkLeaderChanged(ctx, run, st)
	case "patroni-rest-api:rest-patch-config":
		result = a.checkRestConfigPatched(ctx, st)
	case "patroni-failover-priority:tag-priority":
		result = a.checkFailoverPriorityTagged(ctx, st)
	case "patroni-failover-priority:crash-and-confirm":
		result = a.checkFailoverPriorityWon(ctx, st)
	case "patroni-scheduled-restart:schedule-and-flush":
		result = a.checkScheduledRestartFlushed(ctx, st)
	case "patroni-scheduled-restart:schedule-and-let-fire":
		result = a.checkScheduledRestartFired(ctx, st)
	case "patroni-callbacks:cause-role-change":
		result = a.checkRoleChangeCallbackFired(ctx, run, st)
	case "valkey-hash-slots:route-a-key":
		result = a.checkValkeyKeyRouted(ctx, st)
	case "valkey-resharding:reshard-slots":
		result = a.checkValkeyReshardOccurred(ctx, st)
	case "valkey-manual-failover:build-replica":
		result = a.checkValkeyReplicaBuilt(ctx, run, st)
	case "valkey-manual-failover:manual-failover":
		result = a.checkValkeyManualFailover(ctx, run, st)
	case "valkey-persistence:tune-fsync":
		result = a.checkValkeyFsyncAlways(ctx, st)
	case "valkey-persistence:observe-durability-cost":
		result = a.checkValkeyFsyncLatencyRecorded(ctx, st)
	case "valkey-memory-eviction:configure-eviction":
		result = a.checkValkeyEvictionConfigured(ctx, st)
	case "valkey-memory-eviction:trigger-eviction":
		result = a.checkValkeyEvictionOccurred(ctx, st)
	case "valkey-acl:create-restricted-user":
		result = a.checkValkeyACLUserCreated(ctx, st)
	case "valkey-acl:verify-enforcement":
		result = a.checkValkeyACLEnforced(ctx, st)
	case "valkey-transactions:run-transaction":
		result = a.checkValkeyTransactionRan(ctx, st)
	case "valkey-transactions:atomic-lua":
		result = a.checkValkeyLuaLimitHeld(ctx, st)
	case "valkey-slowlog:catch-slow-command":
		result = a.checkValkeySlowlogCaught(ctx, st)
	case "valkey-slowlog:latency-diagnostics":
		result = a.checkValkeyLatencyCommandRecorded(ctx, st)
	case "valkey-backup-restore:take-backup":
		result = a.checkValkeyBackupTaken(ctx, st)
	case "valkey-backup-restore:verify-backup":
		result = a.checkValkeyBackupVerified(ctx, st)
	case "valkey-cluster-resize:shrink-cluster":
		result = a.checkValkeyClusterShrunk(ctx, run, st)
	case "valkey-cluster-resize:grow-cluster":
		result = a.checkValkeyClusterGrown(ctx, run, st)
	case "valkey-cross-slot:hit-crossslot-error":
		result = a.checkValkeyCrossSlotErrorSeen(ctx, st)
	case "valkey-cross-slot:fix-with-hash-tags":
		result = a.checkValkeyHashTagFixed(ctx, st)
	case "valkey-replication:establish-replication":
		result = a.checkValkeyReplicationEstablished(ctx, st)
	case "valkey-replication:verify-read-only-replica":
		result = a.checkValkeyReplicaReadOnly(ctx, st)
	case "valkey-sentinel:setup-replication-and-sentinel":
		result = a.checkValkeySentinelWatching(ctx, st)
	case "valkey-sentinel:crash-and-failover":
		result = a.checkValkeySentinelFailedOver(ctx, st)
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

// runningPatroniMembers returns the running Patroni deployments for a lab
// stack — every check below starts by narrowing to these.
func (a *App) runningPatroniMembers(st Stack, doc designDoc) ([]Deployment, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var running []Deployment
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	return running, nil
}

// showConfigFlagOnAllMembers reports whether every running Patroni member's
// dynamic configuration ("patronictl show-config" — the same object "pause"/
// "resume"/"edit-config" all write to) agrees a boolean flag (e.g. "pause",
// "failsafe_mode") is in the wanted state. ok is false (with a human message)
// if any member hasn't converged yet or none are running.
func (a *App) showConfigFlagOnAllMembers(ctx context.Context, st Stack, flag string, want bool) (bool, string) {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return false, "No Patroni cluster found in this lab's stack."
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil {
		return false, "Failed to read the lab stack's deployments."
	}
	if len(running) == 0 {
		return false, "No Patroni node is running yet — wait for the cluster to finish deploying."
	}
	for _, d := range running {
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"patronictl", "-c", "/etc/patroni/postgresql.yml", "show-config"}, nil)
		if err != nil || res.Code != 0 {
			return false, "Could not read the cluster config from " + nodeLabel(doc, d.NodeID) + " — is it still starting up?"
		}
		got := strings.Contains(strings.ToLower(res.Stdout), flag+": true")
		if got != want {
			return false, nodeLabel(doc, d.NodeID) + " hasn't picked up the change yet — wait a few seconds and check again."
		}
	}
	return true, ""
}

// checkPatroniPauseState passes once every running member agrees the cluster
// is in the wanted pause state.
func (a *App) checkPatroniPauseState(ctx context.Context, st Stack, wantPaused bool) LabStepResult {
	ok, msg := a.showConfigFlagOnAllMembers(ctx, st, "pause", wantPaused)
	if !ok {
		return LabStepResult{Passed: false, Message: msg}
	}
	if wantPaused {
		return LabStepResult{Passed: true, Message: "Confirmed: the cluster is paused (autofailover disabled) on every running member."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the cluster is resumed (autofailover enabled) on every running member."}
}

// checkFailsafeModeEnabled passes once every running member agrees
// failsafe_mode is on.
func (a *App) checkFailsafeModeEnabled(ctx context.Context, st Stack) LabStepResult {
	ok, msg := a.showConfigFlagOnAllMembers(ctx, st, "failsafe_mode", true)
	if !ok {
		return LabStepResult{Passed: false, Message: msg}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: failsafe_mode is enabled on every running member."}
}

// checkGUCConsistentAcrossMembers passes once every running Patroni member
// reports the same value for a PostgreSQL setting, and that value isn't the
// stock default — proof a patronictl edit-config change actually applied
// everywhere, not just that it was written to the DCS.
func (a *App) checkGUCConsistentAcrossMembers(ctx context.Context, st Stack, guc, defaultVal string) LabStepResult {
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
			[]string{"psql", "-U", "postgres", "-d", "postgres", "-tAqc", "show " + guc + ";"}, nil)
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
		if v == "" || v == defaultVal {
			return LabStepResult{Passed: false, Message: labels[i] + " is still the default " + guc + " (" + defaultVal + ") — change it with patronictl edit-config, and for a static parameter, restart every member."}
		}
	}
	for i, v := range values[1:] {
		if v != values[0] {
			return LabStepResult{Passed: false, Message: labels[0] + " reports " + values[0] + " but " + labels[i+1] + " reports " + v + " — wait for every member to pick up the change."}
		}
	}
	return LabStepResult{Passed: true, Message: guc + " = " + values[0] + " confirmed on all " + labels[0] + ", " + strings.Join(labels[1:], ", ") + "."}
}

// checkPatroniConfigChange verifies the dynamic (reloadable) work_mem lab.
func (a *App) checkPatroniConfigChange(ctx context.Context, st Stack) LabStepResult {
	return a.checkGUCConsistentAcrossMembers(ctx, st, "work_mem", "4MB")
}

// checkPatroniRollingRestart verifies the static (restart-required)
// max_connections lab — same underlying check, since the real-world fact
// being tested (does every member agree, and is it non-default) is identical;
// only the parameter and the operational steps to get there differ.
func (a *App) checkPatroniRollingRestart(ctx context.Context, st Stack) LabStepResult {
	return a.checkGUCConsistentAcrossMembers(ctx, st, "max_connections", "100")
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

// checkPausedNoLeader passes once the cluster is confirmed paused AND no
// member can confirm being Leader — the setup for the Manual Failover lab's
// second step (pause first, so a crashed Leader isn't auto-replaced).
func (a *App) checkPausedNoLeader(ctx context.Context, st Stack) LabStepResult {
	ok, msg := a.showConfigFlagOnAllMembers(ctx, st, "pause", true)
	if !ok {
		return LabStepResult{Passed: false, Message: "Pause the cluster and stop patroni on the Leader — " + msg}
	}
	doc, frame, fok := patroniFrameFromStack(st)
	if !fok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	if containerID := a.patroniLeaderContainer(ctx, st, frame, doc); containerID != "" {
		return LabStepResult{Passed: false, Message: "A leader is still reachable — stop patroni on the current Leader (check `patronictl list` for who that is)."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the cluster is paused and no member can currently confirm being Leader."}
}

// checkLeaderPresentAndResumed passes once a Leader exists again (promoted
// manually via patronictl failover) AND the cluster has been resumed — the
// Manual Failover lab's operational point is that it's easy to forget the
// second half.
func (a *App) checkLeaderPresentAndResumed(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader yet — run patronictl failover with a --candidate to promote one."}
	}
	okFlag, msg := a.showConfigFlagOnAllMembers(ctx, st, "pause", false)
	if !okFlag {
		return LabStepResult{Passed: false, Message: "A leader exists, but the cluster still looks paused — run patronictl resume. (" + msg + ")"}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, nodeIDForContainer(deps, containerID)) + " is Leader and autofailover is resumed."}
}

// checkSynchronousReplication passes once the Leader reports at least one
// synchronous standby in pg_stat_replication — proof synchronous_mode isn't
// just configured, but has actually taken effect.
func (a *App) checkSynchronousReplication(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — wait for the cluster to settle and check again."}
	}
	res, err := a.engCtx(ctx).ExecAs(ctx, containerID, "postgres",
		[]string{"psql", "-U", "postgres", "-d", "postgres", "-tAqc",
			"select count(*) from pg_stat_replication where sync_state = 'sync';"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not query the Leader — is PostgreSQL up?"}
	}
	n := strings.TrimSpace(res.Stdout)
	if n == "" || n == "0" {
		return LabStepResult{Passed: false, Message: "No synchronous replica yet — run patronictl edit-config with --set synchronous_mode=true and give Patroni a few seconds to pick one."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the Leader has a synchronous replica — commits now wait for it to confirm before returning."}
}

// checkFailsafeLeaderHeld passes once the cluster's Leader is still the same
// node recorded as leader when it came up, even though etcd has no quorum —
// proof failsafe_mode kept it available instead of demoting (contrast with
// the etcd Quorum Loss lab, where Patroni itself was also stopped on the
// other nodes and demotion was the correct, expected outcome).
func (a *App) checkFailsafeLeaderHeld(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — failsafe_mode may not be enabled yet, or Patroni (not just etcd) was stopped on the other nodes. Wait a bit and check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID != run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is Leader now, not the original " + nodeLabel(doc, run.InitialLeaderNode) + " — leadership moved instead of holding, which shouldn't happen with failsafe_mode on. Make sure you enabled it before stopping etcd."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, currentNodeID) + " is still Leader even though etcd has no quorum — failsafe_mode kept it available."}
}

// checkPgBackRestFreshBackup passes once the live pgBackRest backup count is
// higher than the baseline captured when the cluster first came up — proof
// the learner took a backup themselves, not just relying on the automatic
// initial one taken at stanza creation.
func (a *App) checkPgBackRestFreshBackup(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	if !frame.UsePgBackRest {
		return LabStepResult{Passed: false, Message: "pgBackRest isn't enabled on this lab's cluster."}
	}
	if run.InitialBackupCount == 0 {
		return LabStepResult{Passed: false, Message: "Still waiting to see the cluster's automatic initial backup — wait for the cluster to finish deploying, then try again."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	stanza := patroniStanza(frame.Label)
	count, ok := a.pgBackRestBackupCount(ctx, stanza, running[0].ContainerID)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read pgBackRest's backup list — run `pgbackrest --stanza=" + stanza + " info` yourself to check for errors."}
	}
	if count <= run.InitialBackupCount {
		return LabStepResult{Passed: false, Message: "Only the cluster's automatic initial backup is visible — run `pgbackrest --stanza=" + stanza + " --type=full backup` yourself, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + strconv.Itoa(count) + " backups now exist (started with " + strconv.Itoa(run.InitialBackupCount) + ") — your backup was taken."}
}

// allPatroniMembersHealthy reports whether every Patroni member's own REST
// API confirms it's up (as Leader or a healthy running Replica) — the same
// per-node probe patroniLeaderContainer already uses, just checked on every
// member instead of stopping at the first Leader found.
func (a *App) allPatroniMembersHealthy(ctx context.Context, doc designDoc, deps []Deployment) (bool, string) {
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.State != DeployRunning || d.ContainerID == "" {
			return false, n.Label
		}
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil)
		if err != nil || strings.TrimSpace(res.Stdout) == "" {
			return false, n.Label
		}
	}
	return true, ""
}

// checkPatroniClusterHealthy passes once every Patroni member is confirmed up
// — used after a `patronictl reinit` to verify the reclone finished, rather
// than just checking the container is still running (which stays true the
// whole time reinit is in progress, since only Postgres/Patroni state changes).
func (a *App) checkPatroniClusterHealthy(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	healthy, badLabel := a.allPatroniMembersHealthy(ctx, doc, deps)
	if !healthy {
		return LabStepResult{Passed: false, Message: badLabel + " isn't back to a healthy running state yet — give the reclone more time and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: every Patroni member is healthy and running again."}
}

// checkReplicaDataWiped passes once exactly one Patroni member is down AND its
// data directory's PG_VERSION marker is actually gone — real-state proof a
// "lost node" simulation destroyed data, not just stopped a service whose
// disk is still intact — while the rest of the cluster stays healthy enough
// to be the rebuild's source. Shared by the pg_basebackup and manual
// pgBackRest restore labs' first step; they differ only in how the second
// step rebuilds the node.
func (a *App) checkReplicaDataWiped(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
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
	dataDir := pgDataDir(frame.OS, ppgMajorOf(frame.PGMajor))
	var healthy, wiped int
	var wipedLabel string
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.State != DeployRunning || d.ContainerID == "" {
			return LabStepResult{Passed: false, Message: n.Label + " isn't running — wait for the cluster to be ready."}
		}
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) != "" {
			healthy++
			continue
		}
		vres, verr := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"bash", "-c", "test -f " + dataDir + "/PG_VERSION && echo present || echo missing"}, nil)
		if verr != nil || strings.TrimSpace(vres.Stdout) != "missing" {
			return LabStepResult{Passed: false, Message: n.Label + " is down but its data directory is still intact — actually remove its contents (rm -rf " + dataDir + "/*), not just stop the service."}
		}
		wiped++
		wipedLabel = n.Label
	}
	if wiped == 0 {
		return LabStepResult{Passed: false, Message: "No member is down yet — stop patroni on a Replica, then wipe its data directory."}
	}
	if wiped > 1 {
		return LabStepResult{Passed: false, Message: "More than one member is down — this lab wipes a single node; the rest of the cluster needs to stay up as the source to rebuild from."}
	}
	if healthy < 2 {
		return LabStepResult{Passed: false, Message: "Not enough healthy members left to rebuild from — the other two members must both still be reachable."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + wipedLabel + " is down and its data directory is gone, with the rest of the cluster healthy to rebuild from."}
}

// checkPgRewindRecovered passes once the node that was hard-killed (the lab's
// baseline Leader) is healthy again as part of the cluster — the real proof
// it rejoined at all, regardless of mechanism. It also looks for evidence in
// that node's own Patroni journal that pg_rewind specifically ran (rather
// than a full reclone) and mentions it in the passing message when found —
// but doesn't require it, since matching an exact log line is far less
// certain than the node's actual live health.
func (a *App) checkPgRewindRecovered(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	healthy, badLabel := a.allPatroniMembersHealthy(ctx, doc, deps)
	if !healthy {
		return LabStepResult{Passed: false, Message: badLabel + " isn't back to a healthy running state yet — give it more time and check again."}
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	label := nodeLabel(doc, run.InitialLeaderNode)
	crashed, ok := byNode[run.InitialLeaderNode]
	if !ok || crashed.ContainerID == "" {
		return LabStepResult{Passed: true, Message: "Confirmed: every Patroni member is healthy again."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, crashed.ContainerID,
		[]string{"bash", "-c", "journalctl -u patroni --no-pager -n 300 2>/dev/null | grep -qi rewind && echo yes || echo no"}, nil)
	if err == nil && strings.TrimSpace(res.Stdout) == "yes" {
		return LabStepResult{Passed: true, Message: "Confirmed: every Patroni member is healthy again, and " + label + "'s own log confirms pg_rewind ran to reattach it — no full reclone needed."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: every Patroni member is healthy again, including " + label + "."}
}

// checkAllPatroniAndEtcdWiped passes once every Patroni member is unreachable
// AND its etcd data directory is actually empty on every node — real-state
// proof of a total DCS loss, not just three stopped services.
func (a *App) checkAllPatroniAndEtcdWiped(ctx context.Context, st Stack) LabStepResult {
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
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.ContainerID == "" {
			return LabStepResult{Passed: false, Message: n.Label + " isn't deployed yet — wait for the cluster to finish deploying."}
		}
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) != "" {
			return LabStepResult{Passed: false, Message: n.Label + " still has Patroni running — stop it with systemctl stop patroni etcd."}
		}
		vres, verr := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"bash", "-c", `[ -z "$(ls -A /var/lib/etcd 2>/dev/null)" ] && echo empty || echo notempty`}, nil)
		if verr != nil || strings.TrimSpace(vres.Stdout) != "empty" {
			return LabStepResult{Passed: false, Message: n.Label + "'s etcd data directory still has contents — actually remove them (rm -rf /var/lib/etcd/*), not just stop the service."}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: every node is down and its etcd storage is empty — the DCS's memory of this cluster is completely gone."}
}

// checkOriginalLeaderReborn passes once a Leader is reachable again AND it's
// specifically the node recorded as Leader before the DCS was wiped — proof
// the learner seeded the fresh cluster from the most caught-up node, not
// whichever one happened to win the restart race.
func (a *App) checkOriginalLeaderReborn(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader yet — make sure etcd is healthy on all three nodes, then start patroni on the node that was Leader before the outage."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID != run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " became Leader instead of " + nodeLabel(doc, run.InitialLeaderNode) + " — only start patroni on " + nodeLabel(doc, run.InitialLeaderNode) + " first; don't start the other two yet."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, currentNodeID) + " — the original Leader — reclaimed leadership of the freshly-bootstrapped DCS using its own existing data."}
}

// patroniClusterInfo is the shape of a Patroni member's own REST /cluster
// response that the checks below need — scheduled switchovers (a top-level
// field, distinct from any member) and per-member tags (nofailover,
// replicatefrom, ...), neither of which "patronictl list -f json" exposes.
type patroniClusterInfo struct {
	ScheduledSwitchover json.RawMessage `json:"scheduled_switchover"`
	Members             []struct {
		Name             string          `json:"name"`
		Role             string          `json:"role"`
		Tags             map[string]any  `json:"tags"`
		ScheduledRestart json.RawMessage `json:"scheduled_restart"`
	} `json:"members"`
}

// fetchPatroniClusterInfo execs curl against a running member's own REST API
// (127.0.0.1:8008/cluster is always reachable from inside that member's own
// container) and parses the result. ok is false if the exec failed or the
// response wasn't valid JSON.
func (a *App) fetchPatroniClusterInfo(ctx context.Context, containerID string) (patroniClusterInfo, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"curl", "-fsS", "http://127.0.0.1:8008/cluster"}, nil)
	if err != nil || res.Code != 0 {
		return patroniClusterInfo{}, false
	}
	var out patroniClusterInfo
	if json.Unmarshal([]byte(res.Stdout), &out) != nil {
		return patroniClusterInfo{}, false
	}
	return out, true
}

// fetchPatroniClusterInfoAny tries each running member in turn and returns
// the first one that answers. running[0] alone isn't reliable here: it
// reflects this app's own deployment-state tracking (the container is up),
// not whether that specific member's own Patroni REST API is currently
// reachable — exactly the case right after the learner has crashed or
// stopped Patroni on whichever node happens to be first in the list.
func (a *App) fetchPatroniClusterInfoAny(ctx context.Context, running []Deployment) (patroniClusterInfo, bool) {
	for _, d := range running {
		if info, ok := a.fetchPatroniClusterInfo(ctx, d.ContainerID); ok {
			return info, true
		}
	}
	return patroniClusterInfo{}, false
}

// checkSwitchoverScheduleFlushed passes once no switchover is scheduled AND
// leadership is still the original leader — proof the learner's flush
// actually cancelled it, not just that a switchover was never scheduled.
func (a *App) checkSwitchoverScheduleFlushed(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status — is Patroni still starting up?"}
	}
	if len(info.ScheduledSwitchover) > 0 {
		return LabStepResult{Passed: false, Message: "A switchover is still scheduled — cancel it with `patronictl flush lab-patroni switchover --force`."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — wait a moment and check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID != run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is Leader now, not the original " + nodeLabel(doc, run.InitialLeaderNode) + " — the switchover must have fired before you flushed it. Try again with a longer lead time."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: no switchover is scheduled and " + nodeLabel(doc, currentNodeID) + " is still Leader — the flush cancelled it."}
}

// checkNofailoverTagged passes once any Patroni member's own tags report
// nofailover: true — it doesn't matter which member the learner picked.
func (a *App) checkNofailoverTagged(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status — is Patroni still starting up?"}
	}
	for _, m := range info.Members {
		if v, _ := m.Tags["nofailover"].(bool); v {
			return LabStepResult{Passed: true, Message: "Confirmed: " + m.Name + " is now excluded from failover (nofailover: true)."}
		}
	}
	return LabStepResult{Passed: false, Message: "No member has nofailover set yet — edit that replica's local postgresql.yml and reload it."}
}

// checkNofailoverNeverPromoted passes once a Leader exists AND it isn't
// whichever member currently has nofailover: true — proof the tag actually
// kept it out of the election, not merely that some other node was crashed.
func (a *App) checkNofailoverNeverPromoted(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the crash to settle and check again."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	var taggedName, leaderName string
	for _, m := range info.Members {
		if v, _ := m.Tags["nofailover"].(bool); v {
			taggedName = m.Name
		}
		if m.Role == "leader" {
			leaderName = m.Name
		}
	}
	if taggedName == "" {
		return LabStepResult{Passed: false, Message: "No member has nofailover set — complete the previous step first."}
	}
	if leaderName == "" {
		return LabStepResult{Passed: false, Message: "No leader is reachable yet — crash the current Leader and wait a bit for the election."}
	}
	if leaderName == taggedName {
		return LabStepResult{Passed: false, Message: taggedName + " (tagged nofailover) became Leader anyway — double-check the tag actually applied (`list`'s Tags column) before you crashed the old Leader."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + taggedName + " (nofailover) was never promoted — " + leaderName + " is Leader instead."}
}

// checkAllPatroniStopped passes once none of the Patroni frame's members
// answer as Leader or Replica over their own REST API — the prerequisite for
// a "remove" that actually sticks, since a running Patroni re-registers
// itself in the DCS within one loop_wait otherwise.
func (a *App) checkAllPatroniStopped(ctx context.Context, st Stack) LabStepResult {
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
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.ContainerID == "" {
			return LabStepResult{Passed: false, Message: n.Label + " isn't deployed yet — wait for the cluster to finish deploying."}
		}
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) != "" {
			return LabStepResult{Passed: false, Message: n.Label + " still answers as " + strings.TrimSpace(res.Stdout) + " — run `systemctl stop patroni` on it too."}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: Patroni is stopped on every node — safe to remove the cluster from the DCS now."}
}

// checkClusterRemovedFromDCS passes once no Patroni member's REST API
// responds (proof Patroni is gone, not just that "remove" returned success)
// AND etcd itself has no keys left under this cluster's own namespace/scope
// prefix — the literal, direct proof "remove" actually erased the DCS
// record, rather than inferring it from any node's live behavior.
func (a *App) checkClusterRemovedFromDCS(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
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
	var etcdContainerID string
	for _, n := range doc.Nodes {
		if n.Type != "patroni" {
			continue
		}
		d, ok := byNode[n.ID]
		if !ok || d.ContainerID == "" {
			continue
		}
		etcdContainerID = d.ContainerID
		if res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"bash", "-c", "curl -fsS -o /dev/null http://127.0.0.1:8008/cluster 2>/dev/null && echo up || true"}, nil); err == nil && strings.TrimSpace(res.Stdout) == "up" {
			return LabStepResult{Passed: false, Message: n.Label + "'s Patroni REST API still responds — make sure `systemctl stop patroni` actually ran there before removing."}
		}
	}
	if etcdContainerID == "" {
		return LabStepResult{Passed: false, Message: "No Patroni node is deployed yet."}
	}
	prefix := "/dbcanvas/" + sanitizeName(frame.Label)
	res, err := a.engCtx(ctx).Exec(ctx, etcdContainerID,
		[]string{"bash", "-c", "ETCDCTL_API=3 etcdctl --endpoints=http://127.0.0.1:2379 get " + prefix + " --prefix --keys-only"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not query etcd — is it still running? (`systemctl status etcd`)"}
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return LabStepResult{Passed: false, Message: "etcd still has keys under " + prefix + " — run `patronictl remove lab-patroni` and complete its prompts."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: no member's Patroni is reachable, and etcd has no keys left under " + prefix + " — the cluster is fully removed from the DCS."}
}

// checkCascadingReplica passes once the designated upstream member's own
// pg_stat_replication shows the cascading replica as a subscriber, proof the
// replicatefrom tag actually rewired physical streaming rather than just
// changing how `topology` draws the tree.
func (a *App) checkCascadingReplica(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	var cascadingName, upstreamName string
	for _, m := range info.Members {
		if v, _ := m.Tags["replicatefrom"].(string); v != "" {
			cascadingName, upstreamName = m.Name, v
		}
	}
	if cascadingName == "" {
		return LabStepResult{Passed: false, Message: "No member has a replicatefrom tag yet — tag one replica and reload it."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var upstreamContainerID string
	for _, n := range doc.Nodes {
		if n.Type == "patroni" && n.Label == upstreamName {
			if d, ok := deps2ByNode(deps)[n.ID]; ok {
				upstreamContainerID = d.ContainerID
			}
		}
	}
	if upstreamContainerID == "" {
		return LabStepResult{Passed: false, Message: upstreamName + " isn't running — wait for the cluster to settle and check again."}
	}
	res, err := a.engCtx(ctx).ExecAs(ctx, upstreamContainerID, "postgres",
		[]string{"psql", "-U", "postgres", "-tAc", "select count(*) from pg_stat_replication where application_name = '" + cascadingName + "';"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not query " + upstreamName + " — is PostgreSQL up?"}
	}
	if strings.TrimSpace(res.Stdout) == "0" || strings.TrimSpace(res.Stdout) == "" {
		return LabStepResult{Passed: false, Message: cascadingName + " isn't streaming from " + upstreamName + " yet — give the reload a few more seconds and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + upstreamName + " shows " + cascadingName + " as a direct subscriber — it's cascading, not streaming from the Leader."}
}

// deps2ByNode is the same nodeID -> Deployment map built inline elsewhere —
// factored out for checkCascadingReplica's lookup-by-label detour.
func deps2ByNode(deps []Deployment) map[string]Deployment {
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	return byNode
}

// containerIDForPatroniMember resolves a Patroni member's own reported name
// (from /cluster) back to its container ID via the design's node labels —
// every check that needs to single out one specific member by name (rather
// than just "any running member" or "the current Leader") needs this.
func containerIDForPatroniMember(doc designDoc, deps []Deployment, memberName string) string {
	byNode := deps2ByNode(deps)
	for _, n := range doc.Nodes {
		if n.Type == "patroni" && n.Label == memberName {
			if d, ok := byNode[n.ID]; ok {
				return d.ContainerID
			}
		}
	}
	return ""
}

// checkReplicaDrained passes once any member tagged noloadbalance: true
// actually fails Patroni's own /replica health probe — the same probe
// HAProxy's read backend polls, so this is proof the node is really
// excluded from the read pool, not just that the tag was set.
func (a *App) checkReplicaDrained(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	var taggedName string
	for _, m := range info.Members {
		if v, _ := m.Tags["noloadbalance"].(bool); v {
			taggedName = m.Name
		}
	}
	if taggedName == "" {
		return LabStepResult{Passed: false, Message: "No member has noloadbalance set yet — tag a replica and reload it."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	containerID := containerIDForPatroniMember(doc, deps, taggedName)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: taggedName + " isn't running."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID,
		[]string{"bash", "-c", "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8008/replica"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not query " + taggedName + "'s REST API."}
	}
	if strings.TrimSpace(res.Stdout) == "200" {
		return LabStepResult{Passed: false, Message: taggedName + "'s /replica health check still returns 200 — give the reload a few more seconds and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + taggedName + "'s /replica health check now fails (" + strings.TrimSpace(res.Stdout) + ") — HAProxy's read pool will stop routing to it."}
}

// checkReplicaUndrained passes once every current replica's /replica health
// check passes again — proof clearing the tag actually restored it to the
// read pool, not just that the tag field is gone.
func (a *App) checkReplicaUndrained(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var checked int
	for _, m := range info.Members {
		if m.Role != "replica" {
			continue
		}
		if v, _ := m.Tags["noloadbalance"].(bool); v {
			return LabStepResult{Passed: false, Message: m.Name + " still has noloadbalance set — clear the tag and reload it."}
		}
		containerID := containerIDForPatroniMember(doc, deps, m.Name)
		if containerID == "" {
			continue
		}
		res, err := a.engCtx(ctx).Exec(ctx, containerID,
			[]string{"bash", "-c", "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8008/replica"}, nil)
		if err != nil {
			return LabStepResult{Passed: false, Message: "Could not query " + m.Name + "."}
		}
		if strings.TrimSpace(res.Stdout) != "200" {
			return LabStepResult{Passed: false, Message: m.Name + "'s /replica health check still isn't passing (" + strings.TrimSpace(res.Stdout) + ") — give it a few more seconds."}
		}
		checked++
	}
	if checked == 0 {
		return LabStepResult{Passed: false, Message: "No replicas are reachable yet — wait for the cluster to settle and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: every replica's /replica health check passes again — none are excluded from the read pool."}
}

// checkPermanentSlotOnLeader passes once lab_permanent_slot exists on
// whichever node is currently Leader — reused as-is for both this lab's
// creation step and (after a switchover) its survival step, since the
// real-world fact being checked ("does the current Leader have this slot")
// is identical either way.
func (a *App) checkPermanentSlotOnLeader(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — wait a moment and check again."}
	}
	res, err := a.engCtx(ctx).ExecAs(ctx, containerID, "postgres",
		[]string{"psql", "-U", "postgres", "-tAc", "select 1 from pg_replication_slots where slot_name = 'lab_permanent_slot';"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not query the Leader — is PostgreSQL up?"}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	leaderLabel := nodeLabel(doc, nodeIDForContainer(deps, containerID))
	if strings.TrimSpace(res.Stdout) != "1" {
		return LabStepResult{Passed: false, Message: "No lab_permanent_slot found on the current Leader (" + leaderLabel + ") — create it with patronictl edit-config first."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lab_permanent_slot exists on " + leaderLabel + ", the current Leader."}
}

// checkPermanentSlotSurvivedFailover passes once leadership has actually
// moved from the original Leader AND the new Leader has lab_permanent_slot —
// proof Patroni recreated the slot on its own, not that the slot was simply
// never removed because nobody switched over.
func (a *App) checkPermanentSlotSurvivedFailover(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — wait a moment and check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID == run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is still the original Leader — switch over to a different node first."}
	}
	res, err := a.engCtx(ctx).ExecAs(ctx, containerID, "postgres",
		[]string{"psql", "-U", "postgres", "-tAc", "select 1 from pg_replication_slots where slot_name = 'lab_permanent_slot';"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not query the new Leader — is PostgreSQL up?"}
	}
	newLeaderLabel := nodeLabel(doc, currentNodeID)
	if strings.TrimSpace(res.Stdout) != "1" {
		return LabStepResult{Passed: false, Message: newLeaderLabel + " (the new Leader) doesn't have lab_permanent_slot yet — give Patroni a few more seconds to recreate it."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lab_permanent_slot exists on " + newLeaderLabel + ", the new Leader after switchover — Patroni recreated it automatically."}
}

// checkRestConfigPatched passes once the cluster's shared config (read via
// GET :8008/config on any node, the same DCS-backed object edit-config
// writes) reports ttl: 45 — proof a raw PATCH actually reached the DCS, with
// no patronictl involved.
func (a *App) checkRestConfigPatched(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"curl", "-fsS", "http://127.0.0.1:8008/config"}, nil)
		if err != nil || res.Code != 0 {
			continue
		}
		var cfg struct {
			TTL int `json:"ttl"`
		}
		if json.Unmarshal([]byte(res.Stdout), &cfg) != nil {
			continue
		}
		if cfg.TTL == 45 {
			return LabStepResult{Passed: true, Message: "Confirmed: the cluster's shared config now reports ttl: 45 — applied via a raw PATCH, no patronictl involved."}
		}
		return LabStepResult{Passed: false, Message: "ttl is still " + strconv.Itoa(cfg.TTL) + " — PATCH http://127.0.0.1:8008/config with {\"ttl\": 45}."}
	}
	return LabStepResult{Passed: false, Message: "Could not read the cluster's config from any node."}
}

// checkFailoverPriorityTagged passes once any member's tags report a
// failover_priority above the default of 0 — it doesn't matter which member
// the learner picked.
func (a *App) checkFailoverPriorityTagged(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	for _, m := range info.Members {
		if v, ok := m.Tags["failover_priority"]; ok {
			if f, ok2 := v.(float64); ok2 && f > 0 {
				return LabStepResult{Passed: true, Message: "Confirmed: " + m.Name + " has failover_priority " + strconv.FormatFloat(f, 'f', -1, 64) + "."}
			}
		}
	}
	return LabStepResult{Passed: false, Message: "No member has a failover_priority above the default yet — tag a replica and reload it."}
}

// checkFailoverPriorityWon passes once the member with the highest
// failover_priority is the current Leader — proof the tag actually steered
// the election, not just that some node was crashed and something got
// elected.
func (a *App) checkFailoverPriorityWon(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the crash to settle and check again."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	var taggedName string
	var bestPriority float64
	for _, m := range info.Members {
		if v, ok := m.Tags["failover_priority"]; ok {
			if f, ok2 := v.(float64); ok2 && f > bestPriority {
				bestPriority, taggedName = f, m.Name
			}
		}
	}
	if taggedName == "" {
		return LabStepResult{Passed: false, Message: "No member has a failover_priority above the default — complete the previous step first."}
	}
	var leaderName string
	for _, m := range info.Members {
		if m.Role == "leader" {
			leaderName = m.Name
		}
	}
	if leaderName == "" {
		return LabStepResult{Passed: false, Message: "No leader is reachable yet — crash the current Leader and wait a bit for the election."}
	}
	if leaderName != taggedName {
		return LabStepResult{Passed: false, Message: leaderName + " became Leader instead of " + taggedName + " (the higher failover_priority member) — double-check the tag actually applied (`list`'s Tags column) before you crashed the old Leader."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + taggedName + " — the higher failover_priority member — won the election."}
}

// checkScheduledRestartFlushed passes once no member has a restart
// scheduled.
func (a *App) checkScheduledRestartFlushed(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	for _, m := range info.Members {
		if len(m.ScheduledRestart) > 0 {
			return LabStepResult{Passed: false, Message: m.Name + " still has a restart scheduled — cancel it with `patronictl flush lab-patroni restart --force`."}
		}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: no member has a restart scheduled — the flush cancelled it."}
}

// checkScheduledRestartFired passes once some member shows no pending
// scheduled restart AND a postmaster_start_time within the last few
// minutes — proof PostgreSQL actually restarted recently (the scheduled
// restart firing), not merely that nothing is pending.
func (a *App) checkScheduledRestartFired(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) < 2 {
		return LabStepResult{Passed: false, Message: "Not enough Patroni nodes running yet to compare — wait for the cluster to settle and check again."}
	}
	var times []time.Time
	for _, d := range running {
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"curl", "-fsS", "http://127.0.0.1:8008/patroni"}, nil)
		if err != nil || res.Code != 0 {
			continue
		}
		var info struct {
			PostmasterStartTime string          `json:"postmaster_start_time"`
			ScheduledRestart    json.RawMessage `json:"scheduled_restart"`
		}
		if json.Unmarshal([]byte(res.Stdout), &info) != nil {
			continue
		}
		if len(info.ScheduledRestart) > 0 {
			return LabStepResult{Passed: false, Message: "A restart is still scheduled — wait for the scheduled time to pass, then check again."}
		}
		tStr := info.PostmasterStartTime
		if len(tStr) > 19 {
			tStr = tStr[:19]
		}
		if t, err := time.Parse("2006-01-02 15:04:05", tStr); err == nil {
			times = append(times, t)
		}
	}
	if len(times) < 2 {
		return LabStepResult{Passed: false, Message: "Could not read enough members' status to compare restart times — wait a moment and check again."}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	// A member that just restarted independently of the others has a
	// postmaster_start_time meaningfully later than its peers — comparing
	// members against each other (not against wall-clock "now") avoids
	// mistaking the cluster's own original bootstrap for a restart that
	// hasn't actually happened yet.
	if times[len(times)-1].Sub(times[0]) > 20*time.Second {
		return LabStepResult{Passed: true, Message: "Confirmed: one member's PostgreSQL restarted independently of the others, and no restart is still scheduled."}
	}
	return LabStepResult{Passed: false, Message: "Every member's PostgreSQL start time still matches the others — nothing has restarted yet. Wait for the scheduled time to pass, then check again."}
}

// checkRoleChangeCallbackFired passes once leadership has moved from the
// original Leader AND the new Leader's own callback log (appended to by
// /etc/patroni/callbacks/on_role_change.sh, wired in via this lab's
// EnableRoleChangeCallback frame flag) records an on_role_change call —
// checking the NEW leader specifically (not just any member) means this
// entry can only be from the failover just caused, since that node was a
// plain replica at bootstrap and would have logged a replica role then, not
// a leader one.
func (a *App) checkRoleChangeCallbackFired(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "The cluster is still starting up — wait for all three Patroni nodes to finish deploying, then try again."}
	}
	doc, frame, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	containerID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if containerID == "" {
		return LabStepResult{Passed: false, Message: "No leader is currently reachable — wait a moment and check again."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	currentNodeID := nodeIDForContainer(deps, containerID)
	if currentNodeID == run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, currentNodeID) + " is still the original Leader — crash it to trigger a failover first."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, containerID,
		[]string{"bash", "-c", "grep -q 'action=on_role_change' /tmp/patroni-callback.log 2>/dev/null && echo yes || echo no"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read the callback log on " + nodeLabel(doc, currentNodeID) + "."}
	}
	newLeaderLabel := nodeLabel(doc, currentNodeID)
	if strings.TrimSpace(res.Stdout) != "yes" {
		return LabStepResult{Passed: false, Message: newLeaderLabel + " (the new Leader) has no on_role_change entry in its callback log yet — give it a few more seconds."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + newLeaderLabel + "'s callback log recorded an on_role_change call from its own promotion."}
}

// checkIncidentHistoryRecorded passes once patronictl history (read via -f
// json, the same structured form every patronictl command supports) shows at
// least two timeline changes — one for the crash, one for the switchover.
func (a *App) checkIncidentHistoryRecorded(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	var stdout string
	var found bool
	for _, d := range running {
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"patronictl", "-c", "/etc/patroni/postgresql.yml", "history", "-f", "json"}, nil)
		if err == nil && res.Code == 0 {
			stdout, found = res.Stdout, true
			break
		}
	}
	if !found {
		return LabStepResult{Passed: false, Message: "Could not read patronictl history — is the cluster still starting up?"}
	}
	var rows []struct {
		NewLeader string `json:"New Leader"`
	}
	if json.Unmarshal([]byte(stdout), &rows) != nil {
		return LabStepResult{Passed: false, Message: "Could not parse patronictl history output."}
	}
	if len(rows) < 2 {
		return LabStepResult{Passed: false, Message: "Only " + strconv.Itoa(len(rows)) + " leadership change(s) recorded so far — cause both the crash and the switchover, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + strconv.Itoa(len(rows)) + " leadership changes recorded, most recently to " + rows[len(rows)-1].NewLeader + "."}
}

// checkStandbyLeaderPresent passes once exactly one Patroni member reports
// role "standby_leader" — Patroni's designation for the member directly
// streaming from an external primary once the cluster has been demoted.
func (a *App) checkStandbyLeaderPresent(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	for _, m := range info.Members {
		if m.Role == "standby_leader" {
			return LabStepResult{Passed: true, Message: "Confirmed: " + m.Name + " is Standby Leader, following the external primary."}
		}
	}
	return LabStepResult{Passed: false, Message: "No member is Standby Leader yet — run patronictl demote-cluster with the external primary's host/port and wait a bit."}
}

// checkClusterPromotedIndependent passes once a member's role is plainly
// "leader" (not "standby_leader") — a Standby Leader still answers Patroni's
// own /leader REST probe with 200 (it does hold the leader lock, just while
// following an external primary), so patroniLeaderContainer's generic "is
// there a leader" check can't tell the two apart; only the member's own
// reported role (from /cluster) can.
func (a *App) checkClusterPromotedIndependent(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := patroniFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Patroni cluster found in this lab's stack."}
	}
	running, err := a.runningPatroniMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Patroni node is running yet — wait for the cluster to finish deploying."}
	}
	info, ok := a.fetchPatroniClusterInfoAny(ctx, running)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status."}
	}
	for _, m := range info.Members {
		if m.Role == "standby_leader" {
			return LabStepResult{Passed: false, Message: m.Name + " is still Standby Leader — run patronictl promote-cluster and wait a bit."}
		}
	}
	for _, m := range info.Members {
		if m.Role == "leader" {
			return LabStepResult{Passed: true, Message: "Confirmed: " + m.Name + " is an independent Leader again — no longer following the external primary."}
		}
	}
	return LabStepResult{Passed: false, Message: "No independent Leader yet — run patronictl promote-cluster and wait a bit."}
}

// prepareStandbyClusterUpstream configures this lab's standalone "pg" node
// (the "external primary" demote-cluster follows) to accept a physical
// replication connection — wal_level, max_wal_senders and a pg_hba
// replication line aren't part of the app's standard standalone-PostgreSQL
// provisioning (provisionPG in pg.go), since an ordinary standalone node is
// never a replication source. A no-op (returns quickly) for every other lab,
// whose designs have no "pg" node.
func (a *App) prepareStandbyClusterUpstream(runID, stackID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var containerID, os_, major string
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
		var nodeID string
		for _, n := range doc.Nodes {
			if n.Type == "pg" {
				nodeID, os_, major = n.ID, n.OS, n.PGMajor
				break
			}
		}
		if nodeID == "" {
			return // this lab's design has no external-primary "pg" node
		}
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		containerID = dep.ContainerID
		break
	}
	eng := a.engCtx(ctx)
	if _, err := eng.ExecAs(ctx, containerID, "postgres",
		[]string{"psql", "-U", "postgres", "-c",
			"ALTER SYSTEM SET wal_level = 'replica'; ALTER SYSTEM SET max_wal_senders = 10;"}, nil); err != nil {
		log.Printf("stack %d standby-cluster upstream: alter system: %v", stackID, err)
		return
	}
	confDir := pgConfDir(os_, major)
	hbaScript := `grep -q dbcanvas-standby-cluster ` + confDir + `/pg_hba.conf || { echo "# dbcanvas-standby-cluster"; echo "host replication postgres 0.0.0.0/0 scram-sha-256"; } >> ` + confDir + `/pg_hba.conf`
	if _, err := eng.Exec(ctx, containerID, []string{"bash", "-c", hbaScript}, nil); err != nil {
		log.Printf("stack %d standby-cluster upstream: pg_hba: %v", stackID, err)
		return
	}
	service := pgServiceName(os_, major)
	if _, err := eng.Exec(ctx, containerID, []string{"systemctl", "restart", service}, nil); err != nil {
		log.Printf("stack %d standby-cluster upstream: restart: %v", stackID, err)
	}
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
