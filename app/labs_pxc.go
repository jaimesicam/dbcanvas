package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// PXC (Percona XtraDB Cluster) labs — hands-on scenarios on a real, disposable
// Galera cluster, with Airline Sim as both the realistic background workload
// and, for the hot-row certification-conflict lab, a genuine third contender:
// its own booking/cancellation agents write to the same popular routes the
// learner's own conflict-storm targets, so Airline Sim's own txnRetries
// counter (see app/airlinesim.go's sqlBooker) is real evidence, not flavor.

// labPXCDesign is a 3-node PXC cluster + Airline Sim + Intranet + VNC — the
// minimum for genuine majority-quorum behavior (lose 1 = survive, lose 2 =
// don't).
var labPXCDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"lab-pxc-1","type":"pxc","label":"pxc-1","role":"regular","frameId":"lab-pxc","x":574,"y":66},
    {"id":"lab-pxc-2","type":"pxc","label":"pxc-2","role":"regular","frameId":"lab-pxc","x":702,"y":66},
    {"id":"lab-pxc-3","type":"pxc","label":"pxc-3","role":"regular","frameId":"lab-pxc","x":830,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220},
    {"id":"lab-airlinesim","type":"airlinesim","label":"airlinesim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-pxc","type":"pxc","label":"lab-pxc","os":"oraclelinux","osVersion":"9","pxcMajor":"8.0","gtid":true,"x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-as-edge","from":{"node":"lab-pxc","port":"bottom"},"to":{"node":"lab-airlinesim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPXC4Design adds a 4th regular member, already clustered from deploy (PXC
// provisioning has no "join a brand-new node later" affordance to expose
// through a static design template — mirrors labValkeyCluster4Design's same
// "it already exists, the lab removes/rejoins it" shape) — used only by the
// node-maintenance and SST/IST labs, both of which operate on node 4
// specifically so they never disturb the node Airline Sim is attached to.
var labPXC4Design = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"lab-pxc-1","type":"pxc","label":"pxc-1","role":"regular","frameId":"lab-pxc","x":574,"y":66},
    {"id":"lab-pxc-2","type":"pxc","label":"pxc-2","role":"regular","frameId":"lab-pxc","x":702,"y":66},
    {"id":"lab-pxc-3","type":"pxc","label":"pxc-3","role":"regular","frameId":"lab-pxc","x":830,"y":66},
    {"id":"lab-pxc-4","type":"pxc","label":"pxc-4","role":"regular","frameId":"lab-pxc","x":958,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220},
    {"id":"lab-airlinesim","type":"airlinesim","label":"airlinesim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-pxc","type":"pxc","label":"lab-pxc","os":"oraclelinux","osVersion":"9","pxcMajor":"8.0","gtid":true,"x":560,"y":20,"w":528,"h":138}
  ],
  "edges": [
    {"id":"lab-as-edge","from":{"node":"lab-pxc","port":"bottom"},"to":{"node":"lab-airlinesim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var pxcLabs = []Lab{
	{
		ID:          "pxc-cert-conflicts",
		Title:       "Galera Certification Conflicts on a Hot Row",
		Description: "PXC is multi-primary — any node can accept writes. Write to the same row from two nodes at once, though, and Galera's optimistic certification has to reject one of them after the fact.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Multi-Primary Writes",
		TimeLimit:   "2h",
		LectureNotes: `Optimistic, not pessimistic

A regular MySQL primary uses row locks to make conflicting writers wait for each other in real time. Galera doesn't — a transaction commits locally first, gets certified against every other node's recently-committed writesets as it replicates, and only THEN either finalizes everywhere or gets rolled back wherever it lost the certification race. That's what "optimistic" means here: the conflict is detected and resolved after the fact, not prevented up front.

What a certification failure actually looks like to a client

The loser doesn't get a graceful retry from Galera itself — it gets handed back error 1213, "Deadlock found when trying to get lock; try restarting transaction", the exact same error number ordinary InnoDB uses for a plain single-node deadlock. That's deliberate: MySQL clients already know how to handle 1213 (retry the whole transaction), so Galera reuses that existing contract instead of inventing a new one. Airline Sim's own booking engine (sqlBooker, if you want to read the actual source) does exactly this — catches 1213, waits a short jittered backoff, and retries the whole transaction, counting every retry in the txnRetries counter its dashboard shows.

Why this lab picks a hot row on purpose

A cluster with no contention never exercises this path at all — every write lands on a different row, nothing to certify against. This lab has you manufacture contention deliberately: two terminals on two different nodes, both hammering the exact same route's seat-inventory row. You'll see real 1213s in your own terminals. Airline Sim's dashboard picks the busy route you target and stays running throughout as realistic background traffic — but don't expect its own txnRetries counter to visibly climb from this alone: confirmed by actually running this exercise, its own agents write to a much narrower key (one specific route+class+date combination at a time) than "a busy route" as a whole, so a short storm rarely lands on the exact same row by chance. The certification conflict itself is 100% real either way; it just isn't Airline Sim's own traffic that's guaranteed to produce it within a couple of minutes.`,
		DesignTemplate: labPXCDesign,
		Steps: []LabStep{
			{
				ID:    "cause-conflicts",
				Title: "Manufacture real multi-node contention on a busy route",
				Instructions: "Open Airline Sim's dashboard (from the VNC desktop's browser) and set its level to High.\n\n" +
					"Look at the Route Network grid or the Summary panel's Top Routes for a route with a lot of bookings already — note its " +
					"route id (e.g. `R015`) and pick a seat class (e.g. `ECO`) and a nearby flight date.\n\n" +
					"Open a terminal on pxc-1 AND a separate terminal on pxc-2. In both, loop a conflicting update against the exact same row " +
					"(pick a real flight_inventory id you've confirmed exists first with a SELECT):\n\n" +
					"`while true; do mysql -e \"UPDATE airlinesim.flight_inventory SET booked_seats=booked_seats+1 WHERE id='R015|ECO|2026-08-01'\"; done`\n\n" +
					"Let both loops run for 20-30 seconds, then click Check Work — it independently re-probes the same row itself, so it'll " +
					"confirm the conflict even if your own terminals' timing was unlucky.",
				Hint: "If you don't see any 1213 errors printed in your own terminals, you may have picked a quiet row with too little overlap between the two loops — pick a busier route, or run more parallel loops. Check Work runs its own concurrent probe against the row regardless, so it'll still pass once the cluster is genuinely under multi-node write pressure.",
			},
		},
	},
	{
		ID:          "pxc-minority-loss",
		Title:       "Losing a Minority: Node Failure Under Quorum",
		Description: "Kill one of three PXC nodes. The cluster doesn't just survive — it keeps a Primary component with exactly the two nodes still standing, no human intervention required.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Availability & Quorum",
		TimeLimit:   "2h",
		LectureNotes: `Quorum: more than half, not "at least one"

Galera's gcomm layer continuously tracks group membership. Losing a node isn't automatically fatal — what matters is whether the *remaining* members still constitute a majority of the last known cluster size. With 3 nodes, losing 1 leaves 2 — a genuine majority (2 of 3) — so the survivors keep their Primary component status and keep accepting reads and writes exactly as before, with wsrep_cluster_size dropping to 2.

What a client actually experiences depends entirely on which node it was talking to

This is the lab's real point, and it's not really about PXC's own resilience (which is solid) — it's about the gap between "the cluster survived" and "my specific connection survived." Whichever single PXC node your client happened to have an open TCP connection to is now just gone, full stop, regardless of how healthy the other two are. A raw client with a fixed connection has no way to know to try a different node — it just sees a dead socket. This is exactly the situation Airline Sim itself is in here (it connected to one specific member at deploy time), and it's exactly the gap a proxy in front of the cluster is built to close, by routing every client to a live node automatically.

Not a data-loss risk

Nothing about this scenario risks losing committed data: everything on the two survivors is exactly as durable and correct as it was with three. The node you killed simply has stale (or in this exercise, gone) state — a normal state transfer when it comes back brings it current again.`,
		DesignTemplate: labPXCDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-quorum-survives",
				Title: "Kill one node, confirm the other two keep quorum",
				Instructions: "Pick any one of pxc-1/pxc-2/pxc-3 and stop it — either stop mysqld inside it (`systemctl stop mysql`) or stop the whole container from the stack view.\n\n" +
					"On one of the two survivors, run `mysql -e \"SHOW STATUS LIKE 'wsrep_cluster_status'\"` and `mysql -e \"SHOW STATUS LIKE 'wsrep_cluster_size'\"`. Click Check Work.",
				Hint: "Give Galera a few seconds to detect the failure and recompute quorum before checking — an immediate check right after killing the node may still show the old cluster_size.",
			},
		},
	},
	{
		ID:          "pxc-majority-loss",
		Title:       "Losing a Majority: Split-Brain Prevention",
		Description: "Kill two of three PXC nodes. The lone survivor doesn't keep serving as if nothing happened — it deliberately refuses, because it can no longer prove it isn't the smaller half of a split cluster.",
		Difficulty:  "Advanced",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Availability & Quorum",
		TimeLimit:   "2h",
		LectureNotes: `Why the survivor doesn't just keep going

One node out of a formerly-3-node cluster is not a majority of anything. If it kept accepting writes anyway, and the other two nodes were actually still alive and reachable *to each other* (a true network partition, rather than both genuinely being down), you'd end up with two groups both believing they're authoritative — classic split-brain, with two divergent histories that can never be automatically reconciled. Galera's quorum algorithm refuses to let that happen: without a majority, a node drops its Primary component status and simply stops serving.

Direct contrast with losing just one node

Losing 1 of 3 leaves a genuine majority (2) behind — the survivors keep Primary status without missing a beat. Losing 2 of 3 leaves no majority at all, and the single survivor's wsrep_cluster_status flips away from Primary specifically because it can't tell the difference between "the other two nodes crashed" and "the other two nodes are fine but I got cut off from them" — and refusing to write is the only safe response to that ambiguity.

Recovery here is a human decision, not automatic

Unlike the minority case, getting a cluster back from this state isn't just "restart the dead nodes and they'll SST back in" — if the remaining node was allowed to keep diverging, PXC has a pc.bootstrap mechanism for a human to explicitly declare "yes, I know this is the right primary component, force it" — a deliberate, auditable action, never something PXC does on its own.`,
		DesignTemplate: labPXCDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-quorum-lost",
				Title: "Kill two nodes, confirm the survivor refuses Primary status",
				Instructions: "Stop two of the three PXC nodes (mysqld or the whole container, either way), leaving exactly one running.\n\n" +
					"On the survivor, run `mysql -e \"SHOW STATUS LIKE 'wsrep_cluster_status'\"`. Click Check Work.",
				Hint: "If the query itself fails outright (connection refused, or an error about WSREP not being ready), that's also a valid — arguably even more convincing — sign that quorum was lost. Either outcome passes this check.",
			},
		},
	},
	{
		ID:          "pxc-node-maintenance",
		Title:       "Node Maintenance: Graceful Removal and Rejoin",
		Description: "A node leaving cleanly and a node coming back are two different, both-important events — and PXC handles a graceful departure differently from a crash.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `A clean shutdown tells the cluster it's leaving

Stop mysqld normally (systemctl stop mysql) on a Galera node and it notifies the group before it goes — the cluster's view of membership updates immediately and correctly, wsrep_cluster_size drops by exactly one, no failure-detection timeout involved. This is meaningfully different from the sudden, unannounced departures in the two quorum labs, where the survivors had to *notice* a node was gone.

Rejoining brings it back current — automatically

Restart that same node and it doesn't need any manual "resync" step — its own Galera provider recognizes it was a member before and negotiates catch-up with the cluster on its own, choosing between IST (if its local Galera cache still has everything it missed) and full SST (if not). From the operator's side, "take a node down for maintenance and bring it back" is meant to be this ordinary.

Why this matters operationally

This is the everyday case — OS patching, a config change needing a restart, planned hardware maintenance — as opposed to an unplanned node failure. Real PXC operations documentation treats planned single-node maintenance as routine specifically because rejoin is automatic; the operational discipline that matters is doing it one node at a time, never taking down enough nodes at once to risk losing majority quorum.`,
		DesignTemplate: labPXC4Design,
		Steps: []LabStep{
			{
				ID:    "graceful-removal",
				Title: "Stop node 4 cleanly",
				Instructions: "On pxc-4, run `systemctl stop mysql`.\n\n" +
					"On any other node, run `mysql -e \"SHOW STATUS LIKE 'wsrep_cluster_size'\"` — it should read 3. Click Check Work.",
				Hint: "A clean systemctl stop should update cluster size almost immediately — no failure-detection delay like an unannounced node departure.",
			},
			{
				ID:    "rejoin",
				Title: "Bring node 4 back and confirm it fully caught up",
				Instructions: "On pxc-4, run `systemctl start mysql`.\n\n" +
					"Wait for it to rejoin, then confirm on any node that `wsrep_cluster_size` is back to 4, and that " +
					"`SELECT COUNT(*) FROM airlinesim.reservations` matches between pxc-4 and the rest of the cluster. Click Check Work.",
				Hint: "If the counts don't match yet, node 4 is still applying its IST/SST catch-up — give it a bit more time, especially if Airline Sim's level is High.",
			},
		},
	},
	{
		ID:          "pxc-flow-control",
		Title:       "Flow Control: A Slow Node Throttles Everyone",
		Description: "Unlike plain async replication, Galera doesn't let one struggling node fall silently behind. It slows the entire cluster down instead — on purpose.",
		Difficulty:  "Advanced",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `The bounded-lag guarantee, and what it costs

Galera's design goal is that no node ever falls arbitrarily far behind the others — certification requires every node to eventually apply every writeset in the same order, and if one node's apply queue grows past a configured threshold (gcs.fc_limit), that node signals flow control: "pause, I need to catch up." Every other node in the cluster honors that pause by holding back new commits, cluster-wide, until the slow node's backlog drains. wsrep_flow_control_paused reports the fraction of time this node has spent in that paused state.

The direct, quantified contrast with async replication

With plain async replication, a struggling replica falls behind and the primary's own commit rate is completely unaffected — lag grows unbounded, but throughput never suffers on the source. Here, the exact opposite trade is made: PXC bounds how far any node can lag, at the direct cost of throttling every node's writes, including nodes that were never slow themselves. Airline Sim's own aggregate booking rate is the observable proof — it visibly drops cluster-wide the moment one node's flow control kicks in, not just on whichever node you slowed down.

Neither trade is "better" in the abstract

Async replication (bounded impact on the source, unbounded lag risk) and Galera (bounded lag, cluster-wide throughput cost under a struggling node) are different answers to the same underlying tension, and real deployments pick based on which failure mode they can tolerate less — a slow secondary nobody's reading from yet, or a synchronized cluster where uniform throughput matters more than raw peak write speed.`,
		DesignTemplate: labPXCDesign,
		Steps: []LabStep{
			{
				ID:    "cause-flow-control",
				Title: "Slow node 3 down and watch flow control engage",
				Instructions: "Set Airline Sim's level to High.\n\n" +
					"On pxc-3, start a long-running blocking statement to back up its apply queue in a loop (or throttle its CPU if you have cgroup tools available):\n\n" +
					"`mysql -e \"SELECT SLEEP(60) FROM airlinesim.reservations LIMIT 500000\"`\n\n" +
					"On any node, watch `mysql -e \"SHOW STATUS LIKE 'wsrep_flow_control_paused'\"` on pxc-3 rise above 0, and notice Airline Sim's overall booking rate slow down too. Click Check Work.",
				Hint: "wsrep_flow_control_paused resets to 0 on FLUSH STATUS or a restart — if it reads exactly 0 and you've genuinely been slowing the node, give it more concurrent load first.",
			},
			{
				ID:    "flow-control-clears",
				Title: "Stop the artificial slowdown and confirm it clears",
				Instructions: "Stop whatever you were running to slow pxc-3 down.\n\n" +
					"Wait for it to catch back up, then check `wsrep_flow_control_paused` on pxc-3 again — it should settle back near 0. Click Check Work.",
				Hint: "It won't necessarily hit exactly 0.000 — a small residual value is normal. This check accepts anything close to 0.",
			},
		},
	},
	{
		ID:          "pxc-sst-vs-ist",
		Title:       "SST vs. IST: How a Rejoining Node Catches Up",
		Description: "A node that missed 20 seconds catches up completely differently from one that missed everything since a fresh install — and the difference matters for how long maintenance windows actually take.",
		Difficulty:  "Advanced",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "Cluster Operations",
		TimeLimit:   "2h",
		LectureNotes: `IST: replay only what you actually missed

Every node keeps a Galera cache (gcache) — a ring buffer of recent writesets. A node that rejoins after a brief absence, if the cluster's gcache still holds every writeset it missed, gets an Incremental State Transfer: the donor just replays that specific gap, writeset by writeset. Fast, and proportional only to how much was actually missed, not to the size of the dataset.

SST: when there's nothing to replay from, copy everything

If a node's gcache has aged out (a long absence, past however much history the ring buffer retains) or its data is gone entirely (a wiped datadir, a genuinely new node), there's no incremental gap to replay — Galera falls back to a State Snapshot Transfer: a full physical copy of the donor's data (via xtrabackup, by default), proportional to total dataset size, not to how long the node was gone. This is categorically slower, and it also briefly stresses whichever node is chosen as the SST donor.

Two starting conditions, same underlying mechanism

A brief outage always rejoins via IST. This lab has you deliberately create both conditions on the same node — first a short outage (IST), then a wiped datadir (SST) — specifically so you see both paths on hardware you already have running, and understand which one a given maintenance window is actually going to trigger. In production, this is exactly the distinction behind advice like "don't let a node stay down longer than your gcache retention window if you want fast rejoins."`,
		DesignTemplate: labPXC4Design,
		Steps: []LabStep{
			{
				ID:    "trigger-ist",
				Title: "Brief outage: trigger an IST",
				Instructions: "On pxc-4: `systemctl stop mysql`.\n\n" +
					"Wait about 20 seconds (short enough that its gcache still holds everything it'll miss).\n\n" +
					"Run `systemctl start mysql`. Click Check Work once it's back up.",
				Hint: "If Check Work says it found SST markers instead of IST, the outage was probably too long, or Airline Sim's write volume filled the gcache faster than expected — try again with an even shorter stop.",
			},
			{
				ID:    "trigger-sst",
				Title: "Wipe node 4's data and trigger a full SST",
				Instructions: "On pxc-4: `systemctl stop mysql`.\n\n" +
					"Run `rm -rf /var/lib/mysql/*` (this genuinely destroys its local copy).\n\n" +
					"Run `systemctl start mysql`. This will take noticeably longer than the IST case. Click Check Work once it's back up and caught up.",
				Hint: "SST provisions the entire dataset from a donor via xtrabackup — for a lab-sized dataset this is still fast in absolute terms, but you should see it take meaningfully longer than the IST step did.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, pxcLabs...)
}

// --- Check Work ---

// pxcReachableMembers probes each given PXC deployment with a trivial query
// and returns only the ones that actually answer — the reachability signal
// every quorum-loss check in this file is built from, since a "killed" node
// might mean a stopped container (Exec itself fails) or just a stopped
// mysqld inside a still-running container (Exec succeeds, the mysql client
// call inside it doesn't).
func (a *App) pxcReachableMembers(ctx context.Context, members []Deployment) []Deployment {
	var up []Deployment
	for _, d := range members {
		if _, err := a.mysqlLabExec(ctx, d.ContainerID, "SELECT 1"); err == nil {
			up = append(up, d)
		}
	}
	return up
}

func (a *App) checkPXCCertConflicts(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 2 {
		return LabStepResult{Passed: false, Message: "Waiting for at least 2 PXC nodes to finish deploying."}
	}
	row, err := a.pxcHottestInventoryRow(ctx, members[0].ContainerID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Airline Sim's flight_inventory table isn't seeded yet — is it deployed and running?"}
	}
	execA := func(ctx context.Context, q string) (string, error) {
		return a.mysqlLabExec(ctx, members[0].ContainerID, q)
	}
	execB := func(ctx context.Context, q string) (string, error) {
		return a.mysqlLabExec(ctx, members[1].ContainerID, q)
	}
	const rounds = 15
	conflicts := pxcConflictProbe(ctx, row, rounds, execA, execB)
	if conflicts == 0 {
		return LabStepResult{Passed: false, Message: "No certification conflicts observed yet on this cluster — make sure two different PXC nodes are both writing to the same flight_inventory row concurrently, then try Check Work again."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d out of %d concurrent write pairs against the same row (%s) produced a real Galera certification conflict (error 1213) across two different PXC nodes.", conflicts, rounds, row)}
}

func (a *App) checkPXCMinorityQuorum(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Waiting for all 3 PXC nodes to finish deploying."}
	}
	up := a.pxcReachableMembers(ctx, members)
	down := len(members) - len(up)
	if down != 1 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected exactly 1 node down for this exercise — %d node(s) currently unreachable. Stop exactly one PXC node's mysqld (or container).", down)}
	}
	statusOut, err := a.mysqlLabExec(ctx, up[0].ContainerID, "SHOW STATUS LIKE 'wsrep_cluster_status'")
	sizeOut, _ := a.mysqlLabExec(ctx, up[0].ContainerID, "SHOW STATUS LIKE 'wsrep_cluster_size'")
	status := statusValue(statusOut)
	if err != nil || status != "Primary" {
		return LabStepResult{Passed: false, Message: "The surviving nodes haven't confirmed Primary component status yet (wsrep_cluster_status=" + status + ")."}
	}
	size := statusValue(sizeOut)
	return LabStepResult{Passed: true, Message: "Confirmed: the surviving nodes report wsrep_cluster_status=Primary with cluster size " + size + " — the cluster survived losing a minority."}
}

func (a *App) checkPXCMajorityLoss(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Waiting for all 3 PXC nodes to finish deploying."}
	}
	up := a.pxcReachableMembers(ctx, members)
	down := len(members) - len(up)
	if down < 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected at least 2 nodes down for a true majority loss — only %d are unreachable so far.", down)}
	}
	if len(up) == 0 {
		return LabStepResult{Passed: false, Message: "All nodes are unreachable — leave at least one running so you can observe its refusal to serve as Primary."}
	}
	statusOut, err := a.mysqlLabExec(ctx, up[0].ContainerID, "SHOW STATUS LIKE 'wsrep_cluster_status'")
	if err != nil {
		return LabStepResult{Passed: true, Message: "Confirmed: the surviving node is refusing SQL entirely — Galera has correctly given up its Primary component rather than risk a split-brain."}
	}
	status := statusValue(statusOut)
	if status == "Primary" {
		return LabStepResult{Passed: false, Message: "The surviving node still reports wsrep_cluster_status=Primary — that would mean quorum wasn't actually lost. Confirm at least 2 of the 3 nodes are really down."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the surviving node reports wsrep_cluster_status=" + status + " (not Primary) — it has correctly refused to keep serving as if it still had quorum."}
}

// pxc4NodeByLabel finds a specific PXC member (by exact node label, e.g.
// "pxc-4") among a lab's running deployments — the node-maintenance and
// SST/IST labs always operate on one specific member, never "any running one".
func pxc4NodeByLabel(doc designDoc, members []Deployment, label string) (Deployment, bool) {
	var nodeID string
	for _, n := range doc.Nodes {
		if n.Label == label {
			nodeID = n.ID
			break
		}
	}
	if nodeID == "" {
		return Deployment{}, false
	}
	for _, d := range members {
		if d.NodeID == nodeID {
			return d, true
		}
	}
	return Deployment{}, false
}

func (a *App) checkPXCNodeRemoved(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 4 {
		return LabStepResult{Passed: false, Message: "Waiting for all 4 PXC nodes to finish deploying."}
	}
	up := a.pxcReachableMembers(ctx, members)
	if len(up) != 3 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected exactly 3 reachable PXC nodes (pxc-4 stopped) — currently %d are reachable.", len(up))}
	}
	sizeOut, err := a.mysqlLabExec(ctx, up[0].ContainerID, "SHOW STATUS LIKE 'wsrep_cluster_size'")
	if err != nil || statusValue(sizeOut) != "3" {
		return LabStepResult{Passed: false, Message: "wsrep_cluster_size hasn't dropped to 3 yet."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: wsrep_cluster_size is 3 — pxc-4 gracefully left the cluster."}
}

func (a *App) checkPXCNodeRejoined(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 4 {
		return LabStepResult{Passed: false, Message: "Waiting for all 4 PXC nodes to finish deploying."}
	}
	up := a.pxcReachableMembers(ctx, members)
	if len(up) != 4 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected all 4 PXC nodes reachable — currently %d are.", len(up))}
	}
	sizeOut, err := a.mysqlLabExec(ctx, up[0].ContainerID, "SHOW STATUS LIKE 'wsrep_cluster_size'")
	if err != nil || statusValue(sizeOut) != "4" {
		return LabStepResult{Passed: false, Message: "wsrep_cluster_size isn't back to 4 yet."}
	}
	node4, ok := pxc4NodeByLabel(doc, members, "pxc-4")
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not find pxc-4 among the running members."}
	}
	clusterCount, err1 := a.mysqlLabExec(ctx, up[0].ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	node4Count, err2 := a.mysqlLabExec(ctx, node4.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	if err1 != nil || err2 != nil || clusterCount != node4Count {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("pxc-4's reservations count (%s) doesn't match the cluster's (%s) yet — still catching up.", node4Count, clusterCount)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: wsrep_cluster_size is back to 4 and pxc-4's data (" + node4Count + " reservations) matches the rest of the cluster."}
}

func (a *App) checkPXCFlowControl(ctx context.Context, st Stack, want bool) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Waiting for all 3 PXC nodes to finish deploying."}
	}
	node3, ok := pxc4NodeByLabel(doc, members, "pxc-3")
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not find pxc-3 among the running members."}
	}
	pausedOut, err := a.mysqlLabExec(ctx, node3.ContainerID, "SHOW STATUS LIKE 'wsrep_flow_control_paused'")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read wsrep_flow_control_paused on pxc-3."}
	}
	paused, _ := strconv.ParseFloat(statusValue(pausedOut), 64)
	if want {
		if paused <= 0 {
			return LabStepResult{Passed: false, Message: "No flow control pause observed yet — slow pxc-3 down further (a heavier blocking statement or CPU limit) while Airline Sim is at High."}
		}
		return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: wsrep_flow_control_paused is %.3f on pxc-3 — the cluster is throttling to let it catch up.", paused)}
	}
	if paused > 0.05 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("wsrep_flow_control_paused is still %.3f — give pxc-3 more time to catch back up.", paused)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: flow control has cleared (wsrep_flow_control_paused ~0) now that pxc-3 isn't artificially slowed anymore."}
}

func (a *App) checkPXCCatchupMethod(ctx context.Context, st Stack, want string) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 4 {
		return LabStepResult{Passed: false, Message: "Waiting for all 4 PXC nodes to finish deploying."}
	}
	up := a.pxcReachableMembers(ctx, members)
	node4, ok := pxc4NodeByLabel(doc, members, "pxc-4")
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not find pxc-4 among the members."}
	}
	inUp := false
	for _, d := range up {
		if d.NodeID == node4.NodeID {
			inUp = true
		}
	}
	if !inUp {
		return LabStepResult{Passed: false, Message: "pxc-4 isn't reachable yet — wait for it to finish restarting."}
	}
	logsRes, err := a.engCtx(ctx).Exec(ctx, node4.ContainerID, []string{"journalctl", "-u", "mysql", "--no-pager", "-n", "400"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read pxc-4's logs."}
	}
	logs := logsRes.Stdout
	var matched bool
	switch want {
	case "IST":
		matched = strings.Contains(logs, "IST received") || strings.Contains(logs, "receiving IST") || strings.Contains(logs, "Prepared IST")
	case "SST":
		matched = strings.Contains(logs, "State transfer required") || strings.Contains(logs, "receiving State Transfer") || strings.Contains(logs, "WSREP_SST")
	}
	if !matched {
		return LabStepResult{Passed: false, Message: "No sign of a " + want + " in pxc-4's recent logs yet."}
	}
	var anyOther Deployment
	for _, d := range up {
		if d.NodeID != node4.NodeID {
			anyOther = d
			break
		}
	}
	if anyOther.ContainerID == "" {
		return LabStepResult{Passed: false, Message: "Need at least one other reachable node to compare row counts against."}
	}
	clusterCount, err1 := a.mysqlLabExec(ctx, anyOther.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	node4Count, err2 := a.mysqlLabExec(ctx, node4.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	if err1 != nil || err2 != nil || clusterCount != node4Count {
		return LabStepResult{Passed: false, Message: "Logs show a " + want + " happened, but pxc-4 hasn't fully caught up yet (" + node4Count + " vs " + clusterCount + " rows)."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: pxc-4 rejoined via " + want + " and its " + node4Count + " reservations match the cluster."}
}
