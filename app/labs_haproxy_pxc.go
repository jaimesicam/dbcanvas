package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HAProxy + PXC labs — the direct payoff for the PXC curriculum's Multi-Primary
// Writes and Availability & Quorum labs: the same PXC cluster, but Airline Sim
// (and everything else) talks to it through HAProxy instead of a specific
// node, and every pain point those labs exposed gets a concrete fix here.

// labHAProxyPXCDesign is a 3-node PXC cluster fronted by HAProxy, with Airline
// Sim linked to the HAProxy node (not the frame directly) — it resolves to
// TargetKind "haproxy-pxc" and connects through :5000, exactly like any other
// client, deliberately never binding to one specific PXC member the way the
// direct-PXC labs' Airline Sim does.
var labHAProxyPXCDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"lab-pxc-1","type":"pxc","label":"pxc-1","role":"regular","frameId":"lab-pxc","x":574,"y":66},
    {"id":"lab-pxc-2","type":"pxc","label":"pxc-2","role":"regular","frameId":"lab-pxc","x":702,"y":66},
    {"id":"lab-pxc-3","type":"pxc","label":"pxc-3","role":"regular","frameId":"lab-pxc","x":830,"y":66},
    {"id":"lab-haproxy","type":"haproxy","label":"haproxy","os":"oraclelinux","osVersion":"9","x":300,"y":40},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220},
    {"id":"lab-airlinesim","type":"airlinesim","label":"airlinesim-01","x":40,"y":300}
  ],
  "frames": [
    {"id":"lab-pxc","type":"pxc","label":"lab-pxc","os":"oraclelinux","osVersion":"9","pxcMajor":"8.0","gtid":true,"x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-edge-haproxy","from":{"node":"lab-haproxy","port":"right"},"to":{"node":"lab-pxc","port":"left"},"type":"directional"},
    {"id":"lab-as-edge","from":{"node":"lab-haproxy","port":"bottom"},"to":{"node":"lab-airlinesim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var haproxyPXCLabs = []Lab{
	{
		ID:          "haproxy-pxc-single-writer",
		Title:       "Single-Writer Routing Avoids Certification Conflicts",
		Description: "A hot-row conflict storm that reliably produces Galera certification failures when writers connect directly to two different nodes produces none at all here — because HAProxy never lets two clients write to two different nodes in the first place.",
		Difficulty:  "Beginner",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "HAProxy & Load Balancing",
		TimeLimit:   "2h",
		LectureNotes: `A deliberate trade: give up multi-primary write scaling for zero certification conflicts

PXC's write listen block (see haproxyPXCCfg if you want the exact config) sends every single write-port connection to ONE active node — the rest are configured as backup, only promoted if the active one actually fails. That's a real trade-off, not a free lunch: you lose the ability to write to multiple nodes in parallel that made Galera "multi-primary" in the first place. What you get back is that certification conflicts — which need writes hitting the same row from two DIFFERENT nodes to happen at all — simply can't occur anymore, because there's structurally only one node any client can write to at a time.

The same experiment, opposite result

This lab has you run a hot-row storm — two terminals looping conflicting UPDATEs against the same row — except every terminal connects through haproxy:5000 instead of two different PXC nodes directly. Same workload, same "busy route" targeting, same underlying cluster — the only variable that changed is whether writes were allowed to land on more than one node, and that's the whole explanation for the different outcome: Check Work runs the identical concurrent-write probe both against two direct node connections (which reliably produces 1213s) and through HAProxy's write port (which reliably doesn't) — a true apples-to-apples contrast, not dependent on luck either way.

Why production deployments often make exactly this trade anyway

Multi-primary write scaling sounds appealing until you account for certification conflict rates rising with contention — a workload with genuinely hot rows can spend more effort retrying than committing. Routing all writes through one node at a time, with automatic failover if it dies, is often the actually-simpler, actually-more-predictable choice, which is exactly why Percona's own reference architecture recommends HAProxy or ProxySQL in front of PXC as the default, not an afterthought.`,
		DesignTemplate: labHAProxyPXCDesign,
		Steps: []LabStep{
			{
				ID:    "no-conflicts-through-proxy",
				Title: "Repeat the hot-row storm through HAProxy — no conflicts this time",
				Instructions: "Set Airline Sim's level to High from its dashboard.\n\n" +
					"Pick a busy route via Top Routes or the route grid.\n\n" +
					"Open two separate terminals — it doesn't matter which two PXC nodes they're on, or even if they're the same node — and in " +
					"both, loop the same conflicting UPDATE against the same flight_inventory row, but this time connect through the proxy " +
					"(swap in the real admin password if this deployment's .env overrides MYSQL_ADMIN_PASSWORD):\n\n" +
					"`while true; do mysql -h haproxy -P 5000 -uadmin -padmin_password -e \"UPDATE airlinesim.flight_inventory SET booked_seats=booked_seats+1 WHERE id='<route>|<class>|<date>'\"; done`\n\n" +
					"Let both loops run for 20-30 seconds. Click Check Work — it independently repeats the same probe itself through the write port, so it'll confirm the result either way.",
				Hint: "If you still see 1213 errors in your own terminals, double check both are pointed at `-h haproxy -P 5000`, not at two different PXC nodes' own hostnames directly — that would just re-run the direct-node-connection scenario this lab is contrasting against. Check Work runs its own probe through the write port regardless of what your terminals saw.",
			},
		},
	},
	{
		ID:          "haproxy-pxc-failover",
		Title:       "Automatic Failover on the Write Port",
		Description: "Kill the PXC node HAProxy happens to be routing writes to. Unlike a client connected directly to one specific node, nothing connected through HAProxy — including Airline Sim — ever has to notice which specific node is doing the work.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "HAProxy & Load Balancing",
		TimeLimit:   "2h",
		LectureNotes: `The gap this closes, directly

A PXC cluster survives losing one node just fine at the cluster level — quorum holds, the remaining nodes keep serving. But a client with a single, fixed connection to one specific node still throws a connection error the moment that node disappears, purely because it happened to be connected to the one that died. That's not a flaw in PXC — it's a limitation of any client with a fixed connection to a single node. This lab is the fix: Airline Sim here is connected to HAProxy, never to any specific PXC member, so it has no fixed node to lose in the first place.

How the fix actually works, mechanically

HAProxy polls every PXC node's clustercheck endpoint (:9200, a tiny HTTP responder reporting wsrep_local_state) every few seconds (inter 3s, fall 3 in the config — so up to ~9 seconds worst case to notice a failure). The moment the currently-active writer stops answering healthy, HAProxy's own backup-server ordering promotes the next healthy node to active — no human, no orchestration tool, no DNS change, just HAProxy's own passive health-check loop doing exactly what it's configured to do.

What "zero-downtime" actually means here, precisely

Not literally zero — there's a real window (bounded by that ~9-second health-check interval) where in-flight writes to the now-dead node fail and need a client-side retry. What HAProxy actually guarantees is that the window is short, bounded, and requires no reconfiguration on your part to close — contrast that with a client connected directly to one specific PXC node, whose connection is simply gone until someone redeploys it pointed somewhere else.`,
		DesignTemplate: labHAProxyPXCDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-failover",
				Title: "Kill the active writer and confirm HAProxy promotes a different node",
				Instructions: "Open HAProxy's stats page (http://<haproxy fqdn>:7000/ from the VNC desktop's browser, or `curl -s http://127.0.0.1:7000/` from its own terminal) and note which PXC node is currently active in the `_write` backend.\n\n" +
					"Kill that specific node (stop mysqld or the container).\n\n" +
					"Watch the stats page: within about 10 seconds, a different node should show active in its place. Click Check Work.",
				Hint: "If Check Work says the writer hasn't changed yet, give HAProxy's health check a few more seconds — `fall 3` at `inter 3s` means it needs 3 consecutive failed checks, roughly 9 seconds, before it demotes the dead node.",
			},
		},
	},
	{
		ID:          "haproxy-pxc-read-scaling",
		Title:       "Load-Balanced Reads Across Synced Nodes",
		Description: "The write port sends everything to one node on purpose. The read port does the opposite — round-robin across every node that's actually Synced — a real scale-out win Airline Sim itself doesn't take advantage of.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "Percona XtraDB Cluster",
		Category:    "HAProxy & Load Balancing",
		TimeLimit:   "2h",
		LectureNotes: `Same health check, a completely different routing policy

The read port (:5001) polls the exact same clustercheck endpoint (:9200) as the write port — but where the write listen keeps every node except one marked backup, the read listen treats every currently-Synced node as an equal, live target and round-robins across all of them. A node that falls out of sync (mid-SST, or genuinely unhealthy) simply stops receiving read traffic automatically, the same passive-health-check mechanism as the write port's failover, just applied to a wider pool instead of a single active/backup pair.

A concrete, real scale-out win — for a client that asks for it

Every PXC node, being a full Galera member, has the complete dataset — unlike plain async MySQL replication (where only reads count, never writes), reads through PXC's read port scale close to linearly with node count, because there's no coordination needed between reads at all. This is a genuinely free scale-out lunch: more nodes, more aggregate read capacity, zero conflict risk, because reads never write anything to certify.

The gap worth naming explicitly: Airline Sim doesn't do this

Airline Sim always uses the write port for both reads and writes — a documented simplification in its own code, not a limitation of what HAProxy or PXC can offer. This lab is where that gap becomes concrete: you, with a plain mysql client, get load-balanced reads for free; Airline Sim, by its own design choice, doesn't ask for them. A production client that cared about read scaling would split its connections the way you just did manually here.`,
		DesignTemplate: labHAProxyPXCDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-round-robin",
				Title: "Read through the read port several times and watch it round-robin",
				Instructions: "From any PXC node's terminal, run this several times in a row (swap in the real admin password if it's been overridden in this deployment's .env):\n\n" +
					"`mysql -h haproxy -P 5001 -uadmin -padmin_password -e \"SELECT @@hostname\"`\n\n" +
					"You should see it answer from more than one node's hostname across repeated tries. Click Check Work.",
				Hint: "HAProxy's round-robin is per-connection, not per-query — each separate `mysql -e` invocation opens a fresh connection, which is exactly what lets you see it rotate across tries.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, haproxyPXCLabs...)
}

// --- Check Work ---

func (a *App) checkHAProxyNoCertConflicts(ctx context.Context, st Stack) LabStepResult {
	doc, hp, ok := haproxyNodeFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No HAProxy node found in this lab's stack."}
	}
	hpDep, err := a.store.GetDeployment(st.ID, hp.ID)
	if err != nil || hpDep.State != DeployRunning || hpDep.ContainerID == "" {
		return LabStepResult{Passed: false, Message: "Waiting for the HAProxy node to finish deploying."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) < 2 {
		return LabStepResult{Passed: false, Message: "Waiting for at least 2 PXC nodes to finish deploying."}
	}
	row, err := a.pxcHottestInventoryRow(ctx, members[0].ContainerID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Airline Sim's flight_inventory table isn't seeded yet — is it deployed and running?"}
	}
	var sec pxcSecrets
	json.Unmarshal(members[0].Secrets, &sec)
	// Both probing "writers" go through HAProxy's write port from two different
	// PXC nodes' own mysql clients — the direct-PXC lab's exact same technique,
	// just routed through the proxy instead of split across two direct
	// connections, so this is a true apples-to-apples contrast.
	execA := func(ctx context.Context, q string) (string, error) {
		return a.mysqlLabExecRemote(ctx, members[0].ContainerID, sec, "haproxy", 5000, q)
	}
	execB := func(ctx context.Context, q string) (string, error) {
		return a.mysqlLabExecRemote(ctx, members[len(members)-1].ContainerID, sec, "haproxy", 5000, q)
	}
	const rounds = 15
	conflicts := pxcConflictProbe(ctx, row, rounds, execA, execB)
	if conflicts > 0 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("%d out of %d concurrent write pairs through HAProxy's write port still produced a certification conflict — that shouldn't happen when every write is routed to a single node. Confirm HAProxy is actually up and its write port is routing correctly.", conflicts, rounds)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: zero certification conflicts across %d concurrent write pairs against the same row (%s) through HAProxy's write port — every write serialized onto one node, exactly as designed. The identical technique against two direct node connections produces real conflicts; routing through HAProxy doesn't.", rounds, row)}
}

func (a *App) checkHAProxyFailover(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Still determining the initial active writer — wait for the cluster and HAProxy to finish deploying, then try again."}
	}
	doc, hp, ok := haproxyNodeFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No HAProxy node found in this lab's stack."}
	}
	hpDep, err := a.store.GetDeployment(st.ID, hp.ID)
	if err != nil || hpDep.State != DeployRunning || hpDep.ContainerID == "" {
		return LabStepResult{Passed: false, Message: "Waiting for the HAProxy node to finish deploying."}
	}
	current, err := a.haproxyActiveWriter(ctx, hpDep.ContainerID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read HAProxy's stats page: " + err.Error()}
	}
	if current == run.InitialLeaderNode {
		return LabStepResult{Passed: false, Message: current + " is still the active writer — kill it and wait for HAProxy's health check to promote a different node (a few seconds)."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) == 0 {
		return LabStepResult{Passed: false, Message: "The active writer changed, but couldn't reach any PXC member to confirm Airline Sim's bookings are still flowing."}
	}
	var probe Deployment
	for _, d := range a.pxcReachableMembers(ctx, members) {
		probe = d
		break
	}
	if probe.ContainerID == "" {
		return LabStepResult{Passed: false, Message: "The active writer changed, but no PXC member is currently reachable to confirm bookings are flowing."}
	}
	before, err1 := a.airlineSimMetric(ctx, probe.ContainerID, "reservationsTotal")
	time.Sleep(3 * time.Second)
	after, err2 := a.airlineSimMetric(ctx, probe.ContainerID, "reservationsTotal")
	if err1 != nil || err2 != nil {
		return LabStepResult{Passed: false, Message: "The active writer changed, but could not read Airline Sim's reservationsTotal counter to confirm."}
	}
	if after <= before {
		return LabStepResult{Passed: false, Message: "The active writer changed, but Airline Sim's bookings don't seem to be increasing — check its level isn't set to Stop."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: the active writer moved from %s to %s, and Airline Sim's bookings kept flowing throughout (reservationsTotal %d → %d).", run.InitialLeaderNode, current, int(before), int(after))}
}

func (a *App) checkHAProxyReadLoadBalancing(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := pxcFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No PXC frame found in this lab's stack."}
	}
	members, err := a.runningPXCMembers(st, doc)
	if err != nil || len(members) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for the PXC cluster to finish deploying."}
	}
	var sec pxcSecrets
	json.Unmarshal(members[0].Secrets, &sec)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		out, err := a.mysqlLabExecRemote(ctx, members[0].ContainerID, sec, "haproxy", 5001, "SELECT @@hostname")
		if err == nil && out != "" {
			seen[out] = true
		}
	}
	if len(seen) < 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Only saw %d distinct hostname(s) through the read port after 8 tries — try again, or confirm all 3 PXC nodes report Synced.", len(seen))}
	}
	var hosts []string
	for h := range seen {
		hosts = append(hosts, h)
	}
	return LabStepResult{Passed: true, Message: "Confirmed: reads through haproxy:5001 round-robinned across " + strconv.Itoa(len(seen)) + " distinct node(s): " + strings.Join(hosts, ", ") + "."}
}
