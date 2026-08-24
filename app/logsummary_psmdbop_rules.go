package main

// logsummary_psmdbop_rules.go — what the PSMDB operator and its pbm-agents mean.
//
// Two catalogues, because two processes write these logs and running one rule list over
// both would match the wrong things — the same argument the PostgreSQL/Patroni and
// Valkey/systemd pairs already make in this package.
//
// The PSMDB operator's log is more informative than the PXC operator's in one respect and
// less in another, and both are worth knowing. It keeps a **cluster state machine** and
// logs every transition of it, so `Cluster state changed {previous: ready, current:
// initializing}` gives its source a real coloured lane. But its rolling restart is a
// treadmill: `StatefulSet is not up to date` and `can't start/continue 'SmartUpdate'` are
// re-logged on every reconcile for as long as the condition holds, which in the corpus
// meant 770 and 381 records respectively. Neither is an event. Both are a *condition*, and
// the verdict measures how long it was true rather than how often it was written.

import "strings"

// lsPSMDBRules is the operator's catalogue, most specific first.
var lsPSMDBRules = []lsRule{
	// ---------------------------------------------------------------- cluster state
	{
		substr: []string{"Cluster state changed"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Cluster state changed",
		means: "The operator's own opinion of the cluster, from its custom resource. `initializing` is ordinary during a rollout; a cluster that enters it and never leaves is stuck, and nothing else in this log will say so.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			prev, cur := f.last("previous"), f.last("current")
			if cur == "" {
				return
			}
			e.Label = "Cluster state: " + prev + " → " + cur
			e.From, e.State = lsPSMDBState(prev), lsPSMDBState(cur)
			e.Sev = lsStateSev(e.State)
		},
	},
	{
		substr: []string{"is the writable primary"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Primary member identified",
		means: "Which member the operator found holding the primary role. rs.status() on any member says the same thing, but only at the moment you ask it — this is a dated record of it, which is what makes an election visible after the fact.",
		enrich: func(r lsRecord, e *lsEvent) {
			if host, _, ok := strings.Cut(r.Text, " is the writable primary"); ok {
				e.Peer = strings.TrimSpace(host)
				e.Label = "Primary is " + e.Peer
			}
		},
	},
	{
		substr: []string{"replset initialized", "initiating replset"},
		class:  lsClassMember, sev: lsSevOK,
		label: "Replica set initiated",
		means: "The operator ran rs.initiate() — this is the moment the replica set came into existence. Seeing it in a log from a cluster that has been running for weeks means the data directory was empty, which is not what anybody wanted.",
	},
	{
		substr: []string{"Adding new member to replset", "Adding new nodes"},
		class:  lsClassMember, sev: lsSevWarn,
		label: "Member added to the replica set",
		means: "The operator reconfigured the set to include a member. The member itself then performs an initial sync, which is in its own log and can take a long time on a large dataset.",
	},
	{
		substr: []string{"Configuring member votes and priorities", "Fixing member configurations", "Tags changed"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "Replica-set configuration adjusted",
		means: "The operator owns the replica-set configuration — votes, priorities and tags — and re-asserts it. A hand edit through rs.reconfig() is reverted by this.",
	},
	// ---------------------------------------------------------------- rollout
	{
		substr: []string{"can't start/continue 'SmartUpdate'"},
		class:  lsClassRollout, sev: lsSevWarn, overLevel: true,
		label: "Rolling restart is blocked",
		means: "The operator will not restart the next member until every replica is ready, so a member that cannot become ready stops the rollout indefinitely. It re-logs this on every reconcile — the count is how long the condition lasted, not how many times something happened — and it never escalates. Measured: a member left unschedulable by a spec edit produced 381 of these and nothing that said the cluster was stuck.",
	},
	{
		substr: []string{"StatefulSet is changed, starting smart update"},
		class:  lsClassRollout, sev: lsSevWarn,
		label: "Rolling restart started (smart update)",
		means: "The pod template changed and the operator will restart every member to apply it, secondaries first and the primary last. Like the record above, this is re-logged while the condition holds rather than once per rollout.",
	},
	{
		substr: []string{"apply changes to secondary pod", "apply changes to primary pod"},
		class:  lsClassRollout, sev: lsSevWarn,
		label: "Restarting a member",
		means: "The operator is deleting this member's pod so it comes back on the new template. Its own log shows the shutdown, the rejoin and whether an initial sync was needed.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			for _, k := range []string{"pod", "pod name", "name"} {
				if v := f.last(k); v != "" && strings.Contains(v, "-rs") {
					e.Peer = v
				}
			}
			if strings.Contains(r.Text, "primary") {
				e.Label = "Restarting the primary member"
			}
		},
	},
	{
		substr: []string{"smart update finished"},
		class:  lsClassRollout, sev: lsSevOK,
		label: "Rolling restart finished",
		means: "Every member is running the new pod template.",
	},
	{
		substr: []string{"StatefulSet is not up to date", "Waiting for the pods", "Pod started"},
		class:  lsClassRollout, sev: lsSevInfo,
		label: "Waiting for members",
		means: "The reconcile loop restating a condition. On a healthy cluster this stops; on a stuck one it is the whole log.",
	},
	// ---------------------------------------------------------------- backups
	{
		substr: []string{"Starting backup", "Sending backup command"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "Backup started",
		means: "The operator told PBM to take a backup. Which member actually runs it is decided by a nomination between the agents, and only the agents' logs record the outcome.",
	},
	{
		substr: []string{"Backup state changed"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "Backup state changed",
		means: "The PerconaServerMongoDBBackup's own status. `ready` is a finished backup; `error` is a failed one, with the reason in the agents' logs rather than here.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			prev, cur := f.last("previous"), f.last("current")
			if cur == "" {
				return
			}
			e.Label = "Backup state: " + prev + " → " + cur
			switch cur {
			case "ready":
				e.Sev = lsSevOK
			case "error", "rejected":
				e.Sev = lsSevBad
			}
		},
	},
	{
		substr: []string{"Acquiring the backup lock", "Releasing backup lock", "Waiting for backup metadata"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "Backup bookkeeping",
		means: "PBM serialises backups with a lock held in the database. A lock that is acquired and never released is a backup that died with its agent — PBM calls that a stale lock and another agent breaks it on the next cycle.",
	},
	// ---------------------------------------------------------------- restore
	{
		substr: []string{"Starting restore", "Starting logical restore", "Starting physical restore", "Sending restore command"},
		class:  lsClassRestore, sev: lsSevBad,
		label: "Restore started",
		means: "A restore has begun. A LOGICAL restore runs in place — the pods keep running and the data is replaced underneath them, so there is no scale-to-zero to see. What there is instead is a window in which the cluster is serving a half-restored dataset.",
	},
	{
		substr: []string{"Waiting for PITR to be disabled"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Restore is switching point-in-time recovery off",
		means: "A restore cannot run while the oplog slicer does, so the operator turns PITR off first. It does NOT turn it back on in a way that resumes: after the restore, PBM refuses to slice until a NEW full backup exists, while `spec.backup.pitr.enabled` is still true and `pbm status` still says ON. See the agents' logs for the refusal.",
	},
	{
		substr: []string{"Restore state changed"},
		class:  lsClassRestore, sev: lsSevInfo,
		label: "Restore state changed",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			prev, cur := f.last("previous"), f.last("current")
			if cur == "" {
				return
			}
			e.Label = "Restore state: " + prev + " → " + cur
			switch cur {
			case "ready":
				e.Sev = lsSevOK
			case "error", "rejected":
				e.Sev = lsSevBad
			}
		},
		means: "The PerconaServerMongoDBRestore's own status. `running` covers the oplog replay, which on a point-in-time restore is by far the longest phase.",
	},
	{
		substr: []string{"Waiting for restore metadata"},
		class:  lsClassRestore, sev: lsSevInfo,
		label: "Waiting for restore metadata",
		means: "The operator polling for the restore document PBM writes into the database.",
	},
	// ---------------------------------------------------------------- PBM, as the operator drives it
	{
		substr: []string{"updating latest restorable time"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Point-in-time recovery horizon",
		means: "The newest moment a restore could reach, and the full backup it would be built on. It is the one number worth alerting on, and it trails the present by up to `spec.backup.pitr.oplogSpanMin` — 10 minutes by default. A horizon that stops advancing while everything reports healthy is what a broken slicer looks like.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			t, bk := f.last("latestRestorableTime"), f.last("backup")
			switch {
			case t != "" && bk != "":
				e.Label = "PITR can reach " + t + " (from " + bk + ")"
			case t != "":
				e.Label = "PITR can reach " + t
			}
		},
	},
	{
		substr: []string{"Setting pitr.enabled", "Setting pitr.oplogSpanMin", "Setting pitr.compression", "Setting config"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "PBM configuration pushed",
		means: "The operator owns PBM's configuration and writes it from the custom resource. This is where the settings that decide your recovery point actually take effect.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			for _, k := range []string{"value", "oplogSpanMin", "enabled", "compressionType"} {
				if v := f.last(k); v != "" {
					e.Label = strings.TrimPrefix(r.Text, "Setting ") + " = " + v
					return
				}
			}
		},
	},
	{
		substr: []string{"configuration changed or resync is needed", "waiting for resync to start"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "PBM resyncing its view of the storage",
		means: "PBM re-reads the object store to rebuild its list of backups and oplog chunks. It happens whenever the storage configuration changes, and until it finishes `pbm status` under-reports what is there.",
	},
	{
		substr: []string{"pbm-agent version"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "PBM agent version",
		means: "The agent build the operator found in the pods.",
	},
	// ---------------------------------------------------------------- the controller itself
	{
		substr: []string{"update Mongo version"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "MongoDB version read from the database",
		means: "The operator asked a running member what it is. It is the authoritative answer for what the cluster runs, as opposed to what the image tag says.",
		enrich: func(r lsRecord, e *lsEvent) {
			if _, v, ok := strings.Cut(r.Text, "update Mongo version to "); ok {
				if ver, _, ok := strings.Cut(v, " "); ok {
					e.Label = "Cluster is running MongoDB " + ver
				}
			}
		},
	},
	{
		substr: []string{"Creating user", "user admin created", "creating user admin", "Created a new mongo key"},
		class:  lsClassSecurity, sev: lsSevInfo,
		label: "Operator maintaining its database users",
		means: "The operator owns the userAdmin, clusterAdmin, clusterMonitor, backup and databaseAdmin accounts and the replica set's key file. A burst of these after a restore is the operator putting back the users the restore overwrote.",
	},
	{
		substr: []string{"Multi-cluster services (MCS) are not available"},
		class:  lsClassStartup, sev: lsSevInfo, overLevel: true,
		label: "Multi-cluster services not available",
		means: "Written on every start of every ordinary cluster: the operator looked for the ServiceExport CRD that multi-cluster deployments use and did not find one. It is not a fault.",
	},
	{
		substr: []string{"Reconciler error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Reconcile failed",
		means: "controller-runtime's own record that a reconcile returned an error, with the Go stack trace of its call path underneath. It is retried with an exponential backoff, so one persistent fault produces a long run of these.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Reconcile failed: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"Manager starting up", "Starting EventSource", "Starting Controller", "Starting workers",
			"starting server", "Starting metrics server", "Serving metrics server", "server version",
			"add new job"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "controller-runtime wiring itself up. Present in every start of every healthy operator.",
	},
}

// lsPSMDBState maps the operator's own words onto the swimlane's.
func lsPSMDBState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ready":
		return lsStatePSMDBReady
	case "initializing", "paused", "stopping":
		return lsStatePSMDBInit
	case "error":
		return lsStatePSMDBErr
	}
	return ""
}

// lsPBMRules is the agents' catalogue.
var lsPBMRules = []lsRule{
	{
		substr: []string{"streaming started from"},
		class:  lsClassPITR, sev: lsSevOK,
		label: "This member is slicing the oplog",
		means: "This agent won the nomination and is the ONE member of its replica set streaming the oplog to object storage. The other agents write nothing about it. Read only their logs and point-in-time recovery looks like it is not running.",
	},
	{
		substr: []string{"created chunk"},
		class:  lsClassPITR, sev: lsSevOK,
		label: "Oplog chunk uploaded",
		means: "One slice of the oplog is now in object storage, and the record also says when the next one is due. The interval between them is your worst-case data loss: `spec.backup.pitr.oplogSpanMin`, 10 minutes by default.",
	},
	{
		substr: []string{"no backup found after the restored", "a new backup is required to resume PITR"},
		class:  lsClassPITR, sev: lsSevBad,
		label: "PITR cannot resume — no backup since the restore",
		means: "The single most dangerous line in these logs. After a restore, PBM refuses to slice the oplog until a NEW full backup exists — while `spec.backup.pitr.enabled` is still true and `pbm status` still prints `Status [ON]`. Nothing is being written to object storage, and nothing above the agent says so.",
	},
	{
		substr: []string{"PITR slicer stopped", "stopping oplog slicer on this node"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Oplog slicing stopped on this member",
		means: "Both a backup and a restore stop the slicer — a backup briefly, a restore until a new base backup exists. Between the stop and the next `streaming started` nothing is being collected.",
	},
	{
		substr: []string{"skip after pitr nomination", "skip after nomination"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "This member lost the nomination",
		means: "PBM elects one agent per replica set to do the work. This one is not it, and its log will say almost nothing for as long as that holds — which is why one agent's log is never enough to tell whether backups are running.",
	},
	{
		substr: []string{"storage is not initialized"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "Backup storage not initialised",
		means: "The agent can see no backup metadata in the object store. Normal before the first backup of a new cluster — 93 of these in the corpus, all before the first backup — and a genuine fault afterwards, because it means PBM cannot see what it has written.",
	},
	{
		substr: []string{"got command backup"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "Backup command received",
		means: "Every agent receives the command; one of them wins the nomination and runs it.",
	},
	{
		substr: []string{"backup finished"},
		class:  lsClassBackup, sev: lsSevOK,
		label: "Backup finished on this member",
		means: "This is the member that actually took the backup. Its own log is the only place the dump's collections and sizes are recorded.",
	},
	{
		substr: []string{"dump finished for RS", "dump collection"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "Backup dump progress",
		means: "The per-collection detail of a logical backup, on the nominated member only.",
	},
	{
		substr: []string{"got command restore", "recovery started"},
		class:  lsClassRestore, sev: lsSevBad,
		label: "Restore started on this member",
		means: "From here the member's data is being replaced. A logical restore drops and re-creates the collections in place.",
	},
	{
		substr: []string{"starting oplog replay", "applying"},
		class:  lsClassRestore, sev: lsSevWarn,
		label: "Replaying the oplog",
		means: "The point-in-time phase, and by far the slowest one. Measured: a dump of 414,500 documents restored in 2.3 seconds, and replaying ~92,000 oplog operations on top of it took 7 minutes 32 seconds — about 200 operations a second. The cost of a point-in-time restore is set by how much was WRITTEN since the base backup, not by how big the dataset is.",
	},
	{
		substr: []string{"Exit: connect to PBM", "Exit: connect to the node"},
		class:  lsClassStartup, sev: lsSevWarn,
		label: "Agent could not start and exited",
		means: "The agent could not reach or authenticate to its mongod and gave up; the container's entrypoint restarts it. Ordinary once or twice while a new cluster's replica set is still being initiated (`Type: RSGhost`), and a real fault when it persists — an `AuthenticationFailed` here after a restore means the restore replaced the users PBM logs in with.",
	},
	{
		substr: []string{"`pbm-agent` exited with code"},
		class:  lsClassStartup, sev: lsSevWarn,
		label: "Agent process exited",
		means: "Written by the container's entrypoint wrapper, not by PBM. It is the only record that the agent died at all — PBM keeps its own log inside MongoDB, so an agent that cannot reach the database cannot record its own failure there.",
	},
	{
		substr: []string{"[entrypoint] starting"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "Agent starting",
		means: "The entrypoint launching pbm-agent. The version banner underneath it is the agent's build.",
	},
	{
		substr: []string{"restart in"},
		class:  lsClassStartup, sev: lsSevWarn,
		label: "Agent restarting after a failure",
		means: "The entrypoint's retry loop. A run of these is an agent that cannot start at all, which means no backups and no oplog slicing from this member.",
	},
	{
		substr: []string{"writing log: db:"},
		class:  lsClassNetwork, sev: lsSevWarn,
		label: "Agent could not write its log into the database",
		means: "PBM keeps its log inside MongoDB, so this line — printed to stderr through Go's standard logger instead — is by construction written while the cluster was unreachable. Every one of these is a moment PBM could not record anywhere else.",
	},
	{
		substr: []string{"stale lock"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Took over a stale PBM lock",
		means: "The agent that held the lock stopped without releasing it — it was killed, partitioned or restarted — and this one has broken the lock and taken over. It names the member that held it, which is the member that had the problem.",
	},
	{
		substr: []string{"starting PITR routine", "listening for the commands"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "Agent ready",
		means: "The agent is connected and waiting for commands.",
	},
}

// ---------------------------------------------------------------- state tracks

// lsResolvePSMDBOperator gives the operator source the cluster state it reports.
func lsResolvePSMDBOperator(events []lsEvent) {
	for i := range events {
		e := &events[i]
		if e.State != "" {
			continue // a Cluster-state-changed record already said it
		}
		if strings.HasPrefix(e.Label, "Operator start") {
			e.State = lsStateStarting
		}
	}
}

// lsResolvePBMAgent gives an agent its three.
//
// The distinction that matters is between an agent that is idle because another member
// won the nomination — which is completely normal and is what two of every three agents
// are doing — and one that is idle because it cannot reach the cluster.
func lsResolvePBMAgent(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch e.Label {
		case "This member is slicing the oplog", "Oplog chunk uploaded":
			e.State = lsStatePBMSlicing
		case "This member lost the nomination", "Agent ready":
			e.State = lsStatePBMIdle
		case "Oplog slicing stopped on this member", "PITR cannot resume — no backup since the restore":
			e.State = lsStatePBMIdle
		case "Agent could not write its log into the database", "Agent restarting after a failure",
			"Agent could not start and exited":
			e.State = lsStatePBMLost
		case "Agent starting":
			e.State = lsStateStarting
		}
	}
}

// ---------------------------------------------------------------- classify

func lsClassifyPSMDBOperator(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsPSMDBRules, nil)
}

func lsClassifyPBM(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsPBMRules, lsPBMNoise)
}
