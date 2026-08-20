package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MySQL Replication labs — hands-on scenarios on a real, disposable 3-node
// async (or semi-sync, for one lab) Percona Server replication topology, with
// Airline Sim as the continuous write workload every lab either watches react
// to a change, or queries directly for a checkable, ground-truth fact (its
// `reservations` table lives in the very same database every check already
// reaches via mysqlLabExec — there's no separate connection path to Airline
// Sim itself, and its own container is distroless with no shell to Exec into).

// labMySQLReplDesign is a 1 primary + 2 secondary MySQL replication frame +
// Airline Sim + Intranet + VNC — GTID on, plain async replication.
var labMySQLReplDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"lab-mysql-1","type":"mysql","label":"mysql-1","role":"primary","frameId":"lab-mysql-repl","x":574,"y":66},
    {"id":"lab-mysql-2","type":"mysql","label":"mysql-2","role":"secondary","frameId":"lab-mysql-repl","x":702,"y":66},
    {"id":"lab-mysql-3","type":"mysql","label":"mysql-3","role":"secondary","frameId":"lab-mysql-repl","x":830,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220},
    {"id":"lab-airlinesim","type":"airlinesim","label":"airlinesim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-mysql-repl","type":"mysql","label":"lab-mysql-repl","os":"oraclelinux","osVersion":"9","psMajor":"8.0","gtid":true,"replMode":"async","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-as-edge","from":{"node":"lab-mysql-repl","port":"bottom"},"to":{"node":"lab-airlinesim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labMySQLReplSemiSyncDesign is identical except the frame is pre-configured
// for semi-synchronous replication — only the semi-sync lab uses this variant,
// since flipping modes live is a much bigger aside than that lab is about.
var labMySQLReplSemiSyncDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","x":40,"y":40},
    {"id":"lab-mysql-1","type":"mysql","label":"mysql-1","role":"primary","frameId":"lab-mysql-repl","x":574,"y":66},
    {"id":"lab-mysql-2","type":"mysql","label":"mysql-2","role":"secondary","frameId":"lab-mysql-repl","x":702,"y":66},
    {"id":"lab-mysql-3","type":"mysql","label":"mysql-3","role":"secondary","frameId":"lab-mysql-repl","x":830,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","x":40,"y":220},
    {"id":"lab-airlinesim","type":"airlinesim","label":"airlinesim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-mysql-repl","type":"mysql","label":"lab-mysql-repl","os":"oraclelinux","osVersion":"9","psMajor":"8.0","gtid":true,"replMode":"semisync","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-as-edge","from":{"node":"lab-mysql-repl","port":"bottom"},"to":{"node":"lab-airlinesim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var mysqlReplLabs = []Lab{
	{
		ID:          "mysql-repl-gtid",
		Title:       "GTID Auto-Positioning & Replica Status",
		Description: "Every transaction on the primary gets a globally unique ID before it's ever sent anywhere. A replica doesn't track file/position offsets to know what it has — it tracks which GTIDs it's already applied, and asks for the rest.",
		Difficulty:  "Beginner",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Replication Fundamentals",
		TimeLimit:   "2h",
		LectureNotes: `File-and-position replication's fragility

Classic MySQL replication tracked a replica's progress as a (binlog file, byte offset) pair on the source it was pointed at. That's fragile the moment topology changes: point a replica at a *different* source (after a failover, say) and its old file/offset means nothing there — a human has to work out the right new coordinates by hand, or risk replaying transactions twice or skipping some.

GTIDs: a transaction's ID travels with it

With GTID mode on (the default here), every committed transaction is stamped with a Global Transaction Identifier — <source-uuid>:<sequence-number> — assigned once, on the server that first executes it, and preserved everywhere it's replicated to. A replica's @@GLOBAL.gtid_executed is simply the set of every GTID it has applied. CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1 tells a replica to just ask its source "send me everything after what I already have" — the source computes the gap from GTID sets, no file/offset bookkeeping involved. This is what makes pointing a replica at a *new* source after a failover a matter of one command instead of a forensic exercise.

Two threads, two health signals

A replica runs two threads you'll see separately in SHOW REPLICA STATUS: the receiver thread (Replica_IO_Running) pulls the source's binlog into the replica's own relay log; the applier thread (Replica_SQL_Running) replays that relay log against the replica's data. Either can independently stop and fail (a network blip kills the IO thread; a data conflict kills the SQL thread) — "replication is broken" almost always means checking which of the two, and why (Last_IO_Error / Last_SQL_Error, both in the same \G output).`,
		DesignTemplate: labMySQLReplDesign,
		Steps: []LabStep{
			{
				ID:    "check-replica-status",
				Title: "Confirm both threads are running, then let a replica catch up",
				Instructions: "Open a terminal on mysql-2 or mysql-3.\n\n" +
					"Run `mysql -e \"SHOW REPLICA STATUS\\G\"` and find `Replica_IO_Running` and `Replica_SQL_Running` — both should read `Yes`.\n\n" +
					"Now open a terminal on mysql-1 (the primary) and run `mysql -e \"SELECT @@GLOBAL.gtid_executed\"`, then the same command " +
					"on the replica — watch the replica's GTID set grow toward the primary's as Airline Sim keeps booking.\n\n" +
					"Click Check Work once you're confident it's caught up.",
				Hint: "If either thread shows `No`, `Last_IO_Error` or `Last_SQL_Error` (a few lines down in the same \\G output) will say why. Set Airline Sim's level to Low if you want the GTID sets to converge faster for easier reading.",
			},
		},
	},
	{
		ID:          "mysql-repl-lag",
		Title:       "Replication Lag Under Load",
		Description: "A replica applies changes one relay-log event at a time, after the source already committed them. Push enough write throughput and that gap becomes visible — and asymmetric in a way pure async replication never fixes for you.",
		Difficulty:  "Beginner",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Replication Fundamentals",
		TimeLimit:   "2h",
		LectureNotes: `Seconds_Behind_Source isn't wall-clock lag on the network

It's the gap between "when the source committed this transaction" and "now", measured once the replica's SQL thread actually gets to applying it. A replica can be fully caught up on receiving binlog events (Replica_IO_Running healthy, nothing queued) while still showing real lag, because the *applying* thread is the bottleneck, not the network.

Why async replication never slows the primary down for this

This is the load-bearing contrast with Galera-style synchronous replication: a lagging async replica is the replica's own problem. The primary keeps committing at whatever rate its own hardware allows, indifferent to how far behind any replica falls — there's no cluster-wide throttle. That's exactly why async replication scales writes better than a synchronous cluster does, and exactly why it can't guarantee any replica has *any* particular transaction at any *particular* moment — that same unbounded, load-dependent gap is also what makes a deliberately delayed replica a useful safety feature rather than an accident.`,
		DesignTemplate: labMySQLReplDesign,
		Steps: []LabStep{
			{
				ID:    "cause-lag",
				Title: "Push enough write load to see real lag",
				Instructions: "Open Airline Sim's dashboard (from the VNC desktop's browser) and set its level to High.\n\n" +
					"On mysql-2 or mysql-3's terminal, repeatedly run `mysql -e \"SHOW REPLICA STATUS\\G\" | grep Seconds_Behind_Source` every few seconds until you see a value greater than 0.\n\n" +
					"Click Check Work.",
				Hint: "A 3-node lab cluster on modest hardware can genuinely fall behind at High — that's the point. If it's stubbornly at 0, give it a bit longer; the gap builds up gradually, not instantly.",
			},
			{
				ID:    "drain-lag",
				Title: "Let the replica catch back up",
				Instructions: "Set Airline Sim's level back to Low (or Stop).\n\n" +
					"Keep checking `Seconds_Behind_Source` on the same replica until it drains back to 0, then click Check Work.",
				Hint: "The replica has to apply everything it queued while you were at High — dropping the level doesn't erase the backlog, it just stops growing it. Give it a little time.",
			},
		},
	},
	{
		ID:          "mysql-repl-semisync",
		Title:       "Semi-Synchronous Replication: Durability vs. Fallback",
		Description: "Async replication can lose the last few committed transactions if the primary dies before any replica received them. Semi-sync closes that window — at the cost of every commit waiting on an acknowledgment first.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Durability & Consistency",
		TimeLimit:   "2h",
		LectureNotes: `What semi-sync actually guarantees

With rpl_semi_sync_source_enabled on, the primary's COMMIT doesn't return to the client until at least one semi-sync replica has acknowledged receiving the transaction into its relay log — not applying it, just durably receiving it. That's enough: if the primary dies right after, that transaction is guaranteed to exist somewhere else, recoverable by promoting the replica that acked it. Async replication offers no such guarantee at all — a replica might be arbitrarily far behind when the primary disappears.

The cost, and the escape hatch

This lab's whole point is watching that cost and its boundary directly: every booking Airline Sim makes now waits on a replica ack before its COMMIT returns, and if *no* replica is currently acking (both stopped, both unreachable), writes would stall forever without a release valve. rpl_semi_sync_source_timeout (10 seconds by default) is that valve — after waiting that long with zero acks, the source falls back to async and lets commits proceed unacknowledged, rather than freezing the application indefinitely. rpl_semi_sync_source_status flips from ON to OFF the moment that happens, and flips back to ON automatically once a replica starts acking again.

This is a deliberate trade dial, not a bug

Percona's own guidance is blunt about it: semi-sync buys a durability guarantee at write latency's expense, and the timeout-then-fallback behavior means that guarantee is itself only as strong as "at least one replica was reachable within the timeout window" — not an absolute promise under every failure mode. Production deployments tune the timeout and replica count with that trade-off explicit, not implicit.`,
		DesignTemplate: labMySQLReplSemiSyncDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-semisync-on",
				Title: "Confirm semi-sync is active with both replicas acking",
				Instructions: "On mysql-1 (the primary), run `mysql -e \"SHOW STATUS LIKE 'rpl_semi_sync_source_status'\"` — it should read `ON`.\n\n" +
					"Run `mysql -e \"SHOW STATUS LIKE 'rpl_semi_sync_source_clients'\"` to see how many replicas are currently acking.\n\n" +
					"Click Check Work.",
				Hint: "If status is OFF already, something's wrong with the lab's starting config — this should be ON from first boot.",
			},
			{
				ID:    "confirm-fallback",
				Title: "Stop every replica's ack, then watch Airline Sim recover on its own",
				Instructions: "On both mysql-2 and mysql-3, run `mysql -e \"STOP REPLICA\"`. Airline Sim's bookings will stall briefly.\n\n" +
					"Watch `SHOW STATUS LIKE 'rpl_semi_sync_source_status'` on the primary flip to `OFF` after about 10 seconds (rpl_semi_sync_source_timeout) — and watch Airline Sim's dashboard resume climbing right after.\n\n" +
					"Click Check Work.",
				Hint: "If it hasn't flipped to OFF yet, the timeout hasn't elapsed — give it the full 10+ seconds. Don't restart replication on either node until after Check Work passes.",
			},
		},
	},
	{
		ID:          "mysql-repl-readonly",
		Title:       "Read Scaling and Why Replicas Reject Writes",
		Description: "Every replica in this topology runs with super_read_only — not a suggestion, an enforced guarantee that whatever gets written only ever happens in one place.",
		Difficulty:  "Intermediate",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Read Scaling",
		TimeLimit:   "2h",
		LectureNotes: `Why "just write to whichever node is closest" doesn't work here

Async replication has exactly one place transactions originate: the primary. If a replica silently accepted a write, that write would exist nowhere else — not on the primary, not on any other replica — and the very next replicated event from the real primary could conflict with it in ways replication was never designed to reconcile. super_read_only isn't a performance setting; it's what keeps "one source of truth" true.

read_only vs. super_read_only

Plain read_only blocks ordinary client writes but still lets the replication applier thread itself write (it has to, to apply what it's replicating) and lets a user with SUPER privilege override it. super_read_only closes both of those: even SUPER-privileged manual writes are blocked, while the replication thread's own writes are unaffected (MySQL exempts the applier internally — that exemption is what makes replication possible on a read_only replica at all).

The other half: reads scale fine, right now, with zero extra machinery

Airline Sim itself always talks to the primary for everything — a deliberate simplification documented in its own code, not a limitation of MySQL replication itself. Any client that explicitly chooses to read from a replica instead gets a real scale-out win for free: more replicas, more aggregate read capacity, no coordination needed since reads never conflict with each other. Fronting a cluster with a proxy that load-balances reads across replicas automatically is the natural next step from what this lab has you do by hand.`,
		DesignTemplate: labMySQLReplDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-write-rejected",
				Title: "Watch a replica refuse a direct write",
				Instructions: "On mysql-2's terminal, try:\n\n" +
					"`mysql -e \"INSERT INTO airlinesim.reservation_requests (request_id, created_at) VALUES ('lab-probe', NOW())\"`\n\n" +
					"You should get an error mentioning `read-only`. Click Check Work.",
				Hint: "The exact error is `--super-read-only` (or `--read-only`) preventing the statement — that's the expected outcome, not a problem to fix.",
			},
			{
				ID:    "confirm-row-replicated",
				Title: "Confirm the replica still faithfully serves reads",
				Instructions: "On mysql-1 (the primary), run `mysql -N -e \"SELECT id FROM airlinesim.reservations ORDER BY created_at DESC LIMIT 1\"` to get the most recent booking's id.\n\n" +
					"On mysql-2, run `mysql -e \"SELECT * FROM airlinesim.reservations WHERE id='<that id>'\"` — it should be there. Click Check Work.",
				Hint: "Give it a second or two after copying the id — there's always a small, normal amount of replication delay even when observing it isn't the point of this step.",
			},
		},
	},
	{
		ID:          "mysql-repl-pitr",
		Title:       "Point-in-Time Recovery with Binary Logs",
		Description: "A backup alone only ever recovers you to the moment it was taken. Everything written after that — right up until the moment before your mistake — lives in the binary log, and only there.",
		Difficulty:  "Advanced",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Backup & Recovery",
		TimeLimit:   "3h",
		LectureNotes: `Why "restore the backup" alone is the wrong answer

A logical backup (mysqldump) freezes the database at one instant. Restore it after an accidental DELETE and you get back everything that existed *at backup time* — including undoing every legitimate booking Airline Sim made in the time between the backup and the accident. That's not recovery, that's a second, self-inflicted data loss on top of the first.

The binary log is the missing piece

Every committed transaction since the backup — good ones and the bad DELETE alike — is durably recorded in the primary's binary log, each one tagged with its own globally unique transaction ID (GTID). Point-in-time recovery is: restore the backup, then replay every binlog event *after* the backup up to, but excluding, the specific bad transaction — recovering every legitimate write the backup missed while surgically excluding only the mistake.

Why this matters far beyond "oops, I ran DELETE without a WHERE clause"

The exact same technique — mysqlbinlog with --start-position/--stop-position or GTID-based exclusion — is how real incidents get resolved: a bad migration, a buggy batch job, a compromised credential running unauthorized writes. The backup is not the recovery mechanism by itself; it's the known-good starting point the binlog replay resumes from. Percona and Oracle's own MySQL documentation both treat "backup + binlog retention" as the actual unit of recoverability, not the backup in isolation.`,
		DesignTemplate: labMySQLReplDesign,
		Steps: []LabStep{
			{
				ID:    "take-backup",
				Title: "Back up, and mark exactly where you are",
				Instructions: "On mysql-1 (the primary):\n\n" +
					"`mysqldump --single-transaction airlinesim reservations > /tmp/reservations_backup.sql`\n\n" +
					"`mysql -e \"SHOW BINARY LOG STATUS\\G\" > /tmp/gtid_at_backup.txt` (use `SHOW MASTER STATUS\\G` instead if that command isn't recognized)\n\n" +
					"Click Check Work.",
				Hint: "Both files need to be non-empty — if the dump command failed, `/tmp/reservations_backup.sql` will be 0 bytes.",
			},
			{
				ID:    "simulate-accident",
				Title: "Capture a baseline, then make the mistake",
				Instructions: "Still on mysql-1, first capture your point-in-time recovery target:\n\n" +
					"`mysql -N -e \"SELECT COUNT(*) FROM airlinesim.reservations\" > /tmp/count_before_accident.txt`\n\n" +
					"Then run the \"accident\": `mysql -e \"DELETE FROM airlinesim.reservations WHERE status='cancelled'\"`. Click Check Work.",
				Hint: "If nothing was deleted (count unchanged), Airline Sim hasn't cancelled any reservations yet in this run — wait a little longer for its cancellation agent, or raise its level, then try the DELETE again.",
			},
			{
				ID:    "recover",
				Title: "Restore, then replay the binlog up to (not including) the accident",
				Instructions: "Restore the dump: `mysql airlinesim < /tmp/reservations_backup.sql`.\n\n" +
					"Find the accidental DELETE's binlog position with:\n\n" +
					"`mysqlbinlog --database=airlinesim /var/lib/mysql/binlog.* | grep -B5 \"DELETE FROM \\`reservations\\`\"`\n\n" +
					"Replay everything from the backup's position up to (but excluding) that DELETE:\n\n" +
					"`mysqlbinlog --start-position=<backup position> --stop-position=<position just before the DELETE> /var/lib/mysql/binlog.* | mysql airlinesim`\n\n" +
					"Click Check Work once the row count is back to (or past) your pre-accident baseline.",
				Hint: "It's fine if the final count is a little *higher* than your baseline — Airline Sim never stops booking, so legitimate new rows after your recovery finishes are expected and correct.",
			},
		},
	},
	{
		ID:          "mysql-repl-delayed",
		Title:       "Delayed Replica: A Safety Window, Not a Shield",
		Description: "A replica configured to lag on purpose gives you a window to catch a mistake before it's replicated everywhere — but the window closes. This is about understanding exactly how long you actually have.",
		Difficulty:  "Advanced",
		Database:    "MySQL",
		Technology:  "MySQL Replication",
		Category:    "Backup & Recovery",
		TimeLimit:   "2h",
		LectureNotes: `SOURCE_DELAY: turning unavoidable lag into a deliberate feature

Every async replica already lags by some unpredictable, load-dependent amount. CHANGE REPLICATION SOURCE TO SOURCE_DELAY=N takes that same mechanism and pins it to a *minimum* — N seconds, always, regardless of load. A replica configured this way is intentionally behind, on purpose, so a human has N seconds after any mistake on the primary to notice and intervene before that mistake replicates to this node too.

It's a window, not a rollback

This is the one thing worth being precise about: a delayed replica doesn't protect the data it hasn't caught up to *forever* — it will apply that DELETE too, right on schedule, N seconds after the primary did. The safety it buys is purely temporal: time to notice, time to promote the delayed replica (or copy the still-safe row off it) before the mistake reaches it as well. Treat the delay as a countdown, not a backup.

A cheaper first move than a full point-in-time recovery

A delayed replica is cheaper and faster to react against than restoring from backup and replaying the binlog — no dump, no replay, just "the row is right there, right now, on a node that hasn't gotten to the bad statement yet." It's not a substitute for real backups (a delayed replica that itself fails loses everything, same as any other single node), but it's very often the first thing a real on-call engineer reaches for, precisely because it's already running and already has the data.`,
		DesignTemplate: labMySQLReplDesign,
		Steps: []LabStep{
			{
				ID:    "configure-delay",
				Title: "Configure mysql-3 as a delayed replica",
				Instructions: "On mysql-3: `mysql -e \"STOP REPLICA; CHANGE REPLICATION SOURCE TO SOURCE_DELAY=60; START REPLICA;\"`.\n\n" +
					"Confirm with `mysql -e \"SHOW REPLICA STATUS\\G\"` — look for `SQL_Delay: 60`. Click Check Work.",
				Hint: "SQL_Delay is a configured setting, always 60 once set — don't confuse it with SQL_Remaining_Delay, which counts down only while there's something queued to delay.",
			},
			{
				ID:    "confirm-shielded",
				Title: "Pause Airline Sim, delete a row, and watch mysql-3 not have caught up yet",
				Instructions: "Set Airline Sim's level to Stop from its dashboard (this isolates the one row you're about to delete from the normal ongoing booking/cancellation noise).\n\n" +
					"On mysql-1, run `mysql -e \"DELETE FROM airlinesim.reservations WHERE id = (SELECT id FROM airlinesim.reservations ORDER BY created_at DESC LIMIT 1)\"`.\n\n" +
					"Immediately (within the 60-second window) click Check Work.",
				Hint: "If this fails, you were probably too slow — the 60 seconds may have already elapsed. Set the level to Stop *before* deleting, and don't dawdle between the DELETE and clicking Check Work.",
			},
			{
				ID:           "confirm-elapsed",
				Title:        "Wait past the delay and confirm the shield is gone",
				Instructions: "Wait at least 60 seconds past your DELETE, then click Check Work again.",
				Hint:         "If it still shows the row shielded, the delay hasn't fully elapsed yet — the countdown starts from when the DELETE event was written on the primary, not from when you ran it, so give it a few extra seconds of margin.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, mysqlReplLabs...)
}

// --- Check Work ---

func (a *App) checkMySQLReplGTIDStatus(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for the primary and every secondary to finish deploying."}
	}
	secondary := secondaries[0]
	status, err := a.mysqlLabExec(ctx, secondary.ContainerID, `SHOW REPLICA STATUS\G`)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run SHOW REPLICA STATUS on " + nodeLabel(doc, secondary.NodeID) + "."}
	}
	if verticalField(status, "Replica_IO_Running") != "Yes" || verticalField(status, "Replica_SQL_Running") != "Yes" {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, secondary.NodeID) + "'s replication threads aren't both running yet (Replica_IO_Running / Replica_SQL_Running)."}
	}
	pCount, err1 := a.mysqlLabExec(ctx, primary.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	sCount, err2 := a.mysqlLabExec(ctx, secondary.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	if err1 != nil || err2 != nil || pCount == "" {
		return LabStepResult{Passed: false, Message: "Airline Sim's reservations table isn't seeded yet — give it another moment."}
	}
	if pCount != sCount {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Primary has %s reservations, %s has %s — still catching up.", pCount, nodeLabel(doc, secondary.NodeID), sCount)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, secondary.NodeID) + " is replicating (IO/SQL threads running) and has caught up to the primary's " + pCount + " reservations."}
}

func (a *App) checkMySQLReplLagPresent(ctx context.Context, st Stack, want bool) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	_, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for every replication member to finish deploying."}
	}
	secondary := secondaries[0]
	status, err := a.mysqlLabExec(ctx, secondary.ContainerID, `SHOW REPLICA STATUS\G`)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run SHOW REPLICA STATUS on " + nodeLabel(doc, secondary.NodeID) + "."}
	}
	lagStr := verticalField(status, "Seconds_Behind_Source")
	lag, _ := strconv.Atoi(lagStr)
	if want {
		if lagStr == "" || lagStr == "NULL" || lag <= 0 {
			return LabStepResult{Passed: false, Message: "No lag observed yet on " + nodeLabel(doc, secondary.NodeID) + " — set Airline Sim's level to High and wait a few seconds."}
		}
		return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %s is %d second(s) behind the primary.", nodeLabel(doc, secondary.NodeID), lag)}
	}
	if lag > 0 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("%s is still %d second(s) behind — give it more time to drain.", nodeLabel(doc, secondary.NodeID), lag)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, secondary.NodeID) + " has fully caught up (Seconds_Behind_Source is 0)."}
}

func (a *App) checkMySQLSemiSyncStatus(ctx context.Context, st Stack, want string) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, _, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "Waiting for the primary and every secondary to finish deploying."}
	}
	statusOut, err := a.mysqlLabExec(ctx, primary.ContainerID, "SHOW STATUS LIKE 'rpl_semi_sync_source_status'")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not read rpl_semi_sync_source_status on the primary."}
	}
	got := statusValue(statusOut)
	if got != want {
		if want == "ON" {
			return LabStepResult{Passed: false, Message: "rpl_semi_sync_source_status is currently " + got + ", not ON."}
		}
		return LabStepResult{Passed: false, Message: "Semi-sync hasn't fallen back to async yet (status is " + got + ") — stop replication on both secondaries and wait past rpl_semi_sync_source_timeout (10s by default)."}
	}
	if want == "ON" {
		clientsOut, _ := a.mysqlLabExec(ctx, primary.ContainerID, "SHOW STATUS LIKE 'rpl_semi_sync_source_clients'")
		return LabStepResult{Passed: true, Message: "Confirmed: semi-sync is ON, with " + statusValue(clientsOut) + " replica(s) currently acking."}
	}
	before, err1 := a.airlineSimMetric(ctx, primary.ContainerID, "reservationsTotal")
	time.Sleep(3 * time.Second)
	after, err2 := a.airlineSimMetric(ctx, primary.ContainerID, "reservationsTotal")
	if err1 != nil || err2 != nil {
		return LabStepResult{Passed: false, Message: "Semi-sync fell back to async, but couldn't read Airline Sim's reservationsTotal counter to confirm bookings resumed."}
	}
	if after <= before {
		return LabStepResult{Passed: false, Message: "Semi-sync fell back to async, but Airline Sim's booking counter isn't increasing — check its level isn't set to Stop."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: semi-sync fell back to async (status OFF) and Airline Sim's bookings are flowing again."}
}

func (a *App) checkMySQLReplWriteRejected(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	_, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for every replication member to finish deploying."}
	}
	secondary := secondaries[0]
	_, err := a.mysqlLabExec(ctx, secondary.ContainerID,
		"INSERT INTO airlinesim.reservation_requests (request_id, created_at) VALUES ('lab-probe-"+fmt.Sprint(time.Now().UnixNano())+"', NOW())")
	if err == nil {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, secondary.NodeID) + " accepted a direct write — it should be running with super_read_only and reject this."}
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read-only") && !strings.Contains(strings.ToLower(err.Error()), "read_only") {
		return LabStepResult{Passed: false, Message: "Got an error, but not the expected read-only rejection: " + err.Error()}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, secondary.NodeID) + " rejected the write (super_read_only)."}
}

func (a *App) checkMySQLReplReadReplicated(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for every replication member to finish deploying."}
	}
	secondary := secondaries[0]
	latestID, err := a.mysqlLabExec(ctx, primary.ContainerID, "SELECT id FROM airlinesim.reservations ORDER BY created_at DESC LIMIT 1")
	if err != nil || latestID == "" {
		return LabStepResult{Passed: false, Message: "Airline Sim hasn't booked anything yet — wait a few seconds and try again."}
	}
	found, err := a.mysqlLabExec(ctx, secondary.ContainerID, "SELECT id FROM airlinesim.reservations WHERE id="+sqlQuote(latestID))
	if err != nil || found != latestID {
		return LabStepResult{Passed: false, Message: "Reservation " + latestID + " hasn't replicated to " + nodeLabel(doc, secondary.NodeID) + " yet — give it another moment."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: reservation " + latestID + " (booked on the primary) is readable on " + nodeLabel(doc, secondary.NodeID) + " — replicas serve reads, they just never accept writes."}
}

func (a *App) checkMySQLPITRBackupTaken(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, _, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "Waiting for the primary to finish deploying."}
	}
	if res, err := a.engCtx(ctx).Exec(ctx, primary.ContainerID, []string{"test", "-s", "/tmp/reservations_backup.sql"}, nil); err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "No backup file found at /tmp/reservations_backup.sql yet — run mysqldump first."}
	}
	if res, err := a.engCtx(ctx).Exec(ctx, primary.ContainerID, []string{"test", "-s", "/tmp/gtid_at_backup.txt"}, nil); err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "No position marker found at /tmp/gtid_at_backup.txt yet."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the backup file and binlog position marker both exist."}
}

// mysqlLabReadCountFile execs `cat` on a marker file a lab step's own
// instructions had the learner write via shell redirection, and parses it as
// an integer count — the file-marker technique this lab's multi-step recovery
// exercise uses instead of a new database column, since the baseline only
// needs to survive within one lab run's own container filesystem.
func (a *App) mysqlLabReadCountFile(ctx context.Context, containerID, path string) (int, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"cat", path}, nil)
	if err != nil || res.Code != 0 {
		return 0, fmt.Errorf("no marker file at %s yet", path)
	}
	n, perr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if perr != nil {
		return 0, fmt.Errorf("could not parse %s", path)
	}
	return n, nil
}

func (a *App) checkMySQLPITRAccidentSimulated(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, _, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "Waiting for the primary to finish deploying."}
	}
	before, err := a.mysqlLabReadCountFile(ctx, primary.ContainerID, "/tmp/count_before_accident.txt")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Capture the row count into /tmp/count_before_accident.txt before running the DELETE."}
	}
	nowStr, err := a.mysqlLabExec(ctx, primary.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	now, perr := strconv.Atoi(nowStr)
	if err != nil || perr != nil {
		return LabStepResult{Passed: false, Message: "Could not read the current reservations count."}
	}
	if now >= before {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Count is still %d (baseline was %d) — run the DELETE FROM airlinesim.reservations WHERE status='cancelled' now.", now, before)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d row(s) were deleted (from %d down to %d).", before-now, before, now)}
}

func (a *App) checkMySQLPITRRecovered(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, _, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok {
		return LabStepResult{Passed: false, Message: "Waiting for the primary to finish deploying."}
	}
	before, err := a.mysqlLabReadCountFile(ctx, primary.ContainerID, "/tmp/count_before_accident.txt")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Complete the earlier steps first — no pre-accident baseline found."}
	}
	nowStr, err := a.mysqlLabExec(ctx, primary.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	now, perr := strconv.Atoi(nowStr)
	if err != nil || perr != nil {
		return LabStepResult{Passed: false, Message: "Could not read the current reservations count."}
	}
	if now < before {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Count is %d, still short of the pre-accident baseline of %d — finish restoring and replaying the binlog.", now, before)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: recovered to %d row(s) — at or past the exact state right before the accident, with every booking made since the backup preserved.", now)}
}

// mysqlDelayedReplica returns the lab's designated "delayed" replica — by
// convention the last secondary in doc.Nodes order (mysql-3), since the lab's
// own instructions always configure that specific node.
func mysqlDelayedReplica(secondaries []Deployment) Deployment {
	return secondaries[len(secondaries)-1]
}

func (a *App) checkMySQLDelayConfigured(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	_, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for every replication member to finish deploying."}
	}
	delayed := mysqlDelayedReplica(secondaries)
	status, err := a.mysqlLabExec(ctx, delayed.ContainerID, `SHOW REPLICA STATUS\G`)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run SHOW REPLICA STATUS on " + nodeLabel(doc, delayed.NodeID) + "."}
	}
	n, _ := strconv.Atoi(verticalField(status, "SQL_Delay"))
	if n < 30 {
		return LabStepResult{Passed: false, Message: "SQL_Delay isn't set (or is too small) on " + nodeLabel(doc, delayed.NodeID) + " — run CHANGE REPLICATION SOURCE TO SOURCE_DELAY=60."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %s is configured with a %ds delay.", nodeLabel(doc, delayed.NodeID), n)}
}

func (a *App) checkMySQLDelayShield(ctx context.Context, st Stack, wantShielded bool) LabStepResult {
	doc, frame, ok := mysqlReplFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No MySQL replication frame found in this lab's stack."}
	}
	primary, secondaries, ok := a.mysqlReplMembers(st, doc, frame)
	if !ok || len(secondaries) == 0 {
		return LabStepResult{Passed: false, Message: "Waiting for every replication member to finish deploying."}
	}
	delayed := mysqlDelayedReplica(secondaries)
	pCountStr, err1 := a.mysqlLabExec(ctx, primary.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	dCountStr, err2 := a.mysqlLabExec(ctx, delayed.ContainerID, "SELECT COUNT(*) FROM airlinesim.reservations")
	pCount, perr1 := strconv.Atoi(pCountStr)
	dCount, perr2 := strconv.Atoi(dCountStr)
	if err1 != nil || err2 != nil || perr1 != nil || perr2 != nil {
		return LabStepResult{Passed: false, Message: "Could not read reservation counts from the primary and the delayed replica."}
	}
	if wantShielded {
		if dCount <= pCount {
			return LabStepResult{Passed: false, Message: fmt.Sprintf("%s's count (%d) isn't ahead of the primary's (%d) yet — delete a reservation on the primary, and make sure Airline Sim's level is Stop.", nodeLabel(doc, delayed.NodeID), dCount, pCount)}
		}
		return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %s still has %d row(s) the primary already dropped to %d — the delay is buying you a recovery window.", nodeLabel(doc, delayed.NodeID), dCount, pCount)}
	}
	if dCount > pCount {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("%s is still ahead by %d row(s) — wait for the configured delay to fully elapse.", nodeLabel(doc, delayed.NodeID), dCount-pCount)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: counts match again (%d) — the delay was a window, not a permanent shield.", pCount)}
}
