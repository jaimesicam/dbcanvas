package main

// logsummary_pgop_rules.go — what the three PostgreSQL operators mean.
//
// Three catalogues, because three projects chose three vocabularies for the same job. What
// they have in common is the thing worth reporting: **two of the three delegate the
// database's own availability to Patroni and say nothing about it**, and the third does
// not use Patroni at all and therefore says everything.
//
// The member logs are deliberately NOT re-catalogued here. A Percona or Crunchy member's
// `database` container is Patroni, and PostgreSQL's own log is a file beside it — both are
// already read by logsummary_postgres.go, whose Patroni rules were written against a
// hand-built three-node cluster and work on an operator-managed one unchanged. A CNPG
// member's PostgreSQL records are unwrapped by lsFoldCNPG and sent to the same classifier.

import "strings"

// ---------------------------------------------------------------- Percona (zap)

var lsPerconaPGRules = []lsRule{
	{
		substr: []string{"Backup is starting"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "Backup starting",
		means: "The operator asked pgBackRest for a backup. Which repository and whether it is full, differential or incremental is in the record's fields — pgBackRest decides the type from what is already in the repo, so an 'incr' here means it found a full to build on.",
	},
	{
		substr: []string{"Backup succeeded"},
		class:  lsClassBackup, sev: lsSevOK,
		label: "Backup succeeded",
		means: "pgBackRest finished and the operator marked the PerconaPGBackup Succeeded.",
	},
	{
		substr: []string{"Waiting for restore to start", "Waiting for restore to complete"},
		class:  lsClassRestore, sev: lsSevBad,
		label: "Restore in progress",
		means: "A PerconaPGRestore is running. The operator takes every instance down, restores the repository onto one of them and lets the others rebuild from it — so from here until the cluster is ready again there is no database. Re-logged while it waits, so the count is the duration.",
	},
	{
		substr: []string{"Restore succeeded"},
		class:  lsClassRestore, sev: lsSevOK,
		label: "Restore succeeded",
		means: "The restore finished and the operator is bringing the cluster back. Measured on a three-instance cluster: about four minutes from the request to the leader serving again, most of it the two replicas rebuilding from the restored primary rather than the restore itself.",
	},
	{
		substr: []string{"failed to cleanup outdated backups"},
		class:  lsClassBackup, sev: lsSevWarn, overLevel: true,
		label: "Could not clean up superseded backups",
		means: "Logged at ERROR during a restore, and it is housekeeping rather than a failure of the restore: a point-in-time restore starts a new timeline, and the operator could not delete the backups the old one made. They stay in the repository and keep costing storage.",
	},
	{
		substr: []string{"WALWatcher"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "WAL watcher",
		means: "The operator's own watcher on the WAL archive, which is what keeps `Got latest restorable timestamp` current.",
	},
	{
		substr: []string{"Waiting for backup to complete"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "Waiting for a backup",
		means: "The controller polling the backup Job. Re-logged on every reconcile, so the count is the duration and not a number of events.",
	},
	{
		substr: []string{"There are no info about backups in the pgbackrest"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "pgBackRest reports no backups",
		means: "The repository is empty as far as pgBackRest can see. Normal before the first backup of a new cluster, and a real fault afterwards — it is what a misconfigured or unreachable repository looks like, and point-in-time recovery has nothing to build on until it changes.",
	},
	{
		substr: []string{"Got latest restorable timestamp"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Latest restorable time",
		means: "How far a point-in-time restore could currently reach. The one number that proves WAL archiving is working end to end: it moves while WAL is being shipped and freezes when it stops.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			for _, k := range []string{"latest restorable timestamp", "timestamp", "latestRestorableTime"} {
				if v := f.last(k); v != "" {
					e.Label = "Restorable up to " + v
					return
				}
			}
		},
	},
	{
		substr: []string{"Backup lease acquired", "Backup lease released", "Running finalizer", "Removing finalizer"},
		class:  lsClassBackup, sev: lsSevInfo,
		label: "Backup bookkeeping",
		means: "The operator serialises backups with a lease and cleans up with finalizers. A lease acquired and never released is a backup whose controller died holding it.",
	},
	{
		substr: []string{"v1 Endpoints is deprecated"},
		class:  lsClassOther, sev: lsSevInfo, overLevel: true,
		label: "Kubernetes API deprecation notice",
		means: "Written on nearly every reconcile against a recent Kubernetes. It is about the operator's own API usage and says nothing about the database — in the corpus it was the ONLY thing this operator logged while its cluster failed over.",
	},
	{
		substr: []string{"Reconciler error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Reconcile failed",
		means: "controller-runtime's retry record, with the Go stack of its call path underneath and an exponential backoff behind it, so one fault produces many.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Reconcile failed: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"Starting EventSource", "Starting Controller", "Starting workers", "starting server",
			"starting controller runtime manager", "feature gates", "legacy PostgresCluster CRD not found"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "controller-runtime wiring itself up.",
	},
}

// ---------------------------------------------------------------- Crunchy (logfmt)

var lsCrunchyRules = []lsRule{
	{
		substr: []string{"pgBackRest stanza creation completed successfully"},
		class:  lsClassBackup, sev: lsSevOK,
		label: "pgBackRest stanza created",
		means: "The repository is initialised and can accept backups and archived WAL. Until this succeeds, `archive_command` fails on every WAL segment and PostgreSQL keeps them on the volume.",
	},
	{
		substr: []string{"replaced configuration"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "Cluster configuration replaced",
		means: "The operator rewrote the ConfigMap the members read. On a Patroni-managed cluster this is how a `postgresql.parameters` change reaches the database — Patroni applies it, and the member's own log says whether a restart was needed.",
	},
	{
		substr: []string{"wrote PostgreSQL users"},
		class:  lsClassSecurity, sev: lsSevInfo,
		label: "Users written",
		means: "The operator owns the cluster's roles and re-asserts them from `spec.users`.",
	},
	{
		substr: []string{"applied PgBouncer objects"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "PgBouncer reconciled",
		means: "The connection pool in front of the primary. A change here moves every pooled client's connection.",
	},
	{
		substr: []string{"patched cluster status"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Cluster status patched",
		means: "The operator writing what it believes into the custom resource. It is the operator's opinion, refreshed on every reconcile, not an event.",
	},
	{
		substr: []string{"reconciled instance set", "reconciled cluster", "reconciled instance"},
		class:  lsClassReconcile, sev: lsSevInfo,
		label: "Reconcile completed",
		means: "The loop restating that it ran. In the corpus these were the ONLY records this operator wrote while its cluster elected a new leader — 14 of them, and not one mentions the failover.",
	},
	{
		substr: []string{"Starting EventSource", "Starting Controller", "Starting workers", "starting server",
			"Feature gate default state", "Found APIs"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "controller-runtime wiring itself up.",
	},
}

// ---------------------------------------------------------------- CloudNativePG (JSON)

var lsCNPGRules = []lsRule{
	{
		substr: []string{"There is a switchover or a failover in progress"},
		class:  lsClassState, sev: lsSevBad,
		label: "Switchover or failover in progress",
		means: "CloudNativePG runs the failover itself — there is no Patroni here — so unlike the two Patroni-based operators it dates the event. While this repeats there is no primary accepting writes.",
	},
	{
		substr: []string{"Switchover completed"},
		class:  lsClassState, sev: lsSevOK,
		label: "Switchover completed",
		means: "The new primary is serving. The span from the first 'in progress' record to this one is the write outage.",
	},
	{
		substr: []string{"Setting primary label"},
		class:  lsClassState, sev: lsSevOK,
		label: "Primary selected",
		means: "The instance this operator now sends writes to. The `-rw` Service follows this label, so it is the moment clients move.",
	},
	{
		substr: []string{"Setting replica label", "Old primary pod not found in managed instances"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Roles being re-labelled",
		means: "The bookkeeping around a role change. 'Old primary pod not found' means the pod it was demoting had already gone — which is what a force-deleted primary looks like from here.",
	},
	{
		substr: []string{"This is an old primary instance, waiting for the switchover to finish"},
		class:  lsClassState, sev: lsSevWarn,
		label: "Former primary waiting to rejoin",
		means: "This member knows it is no longer the primary and is waiting before it comes back as a replica. It is not serving writes and should not be sent any.",
	},
	{
		substr: []string{"Detected ready WAL files in a former primary, triggering WAL archiving"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Unarchived WAL on the former primary",
		means: "The demoted primary still held WAL segments that had never reached the archive. Until they are shipped, point-in-time recovery cannot cross the failover — and if the volume were discarded first, they would be gone.",
	},
	{
		substr: []string{"failed to run wal-archive command", "Error while getting server secret for plugin",
			"Error while connecting to plugin"},
		class: lsClassPITR, sev: lsSevBad,
		label: "WAL archiving is failing",
		means: "PostgreSQL's archive_command is failing, so WAL is accumulating on the volume and nothing is reaching object storage. The cluster goes on serving and reports healthy throughout — measured on the corpus, a Cluster stayed `Cluster in healthy state` while every WAL archive attempt failed and its first backup failed with it.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "WAL archiving is failing: " + lsOpTrunc(msg, 80)
			}
		},
	},
	{
		substr: []string{"no orphan PVCs found, skipping the restored cluster reconciliation",
			"The job finished, setting PVC as ready", "Creating new Job"},
		class: lsClassRestore, sev: lsSevWarn,
		label: "Recovering into a new cluster",
		means: "CloudNativePG does not restore in place. A recovery is a NEW Cluster bootstrapped from the object store — the original keeps running and serving throughout, and what you get is a second cluster beside it. That is the safest of the three models and the one that surprises people: nothing is rolled back, and the old cluster is still there to be switched away from deliberately.",
	},
	{
		substr: []string{"Cluster has become healthy"},
		class:  lsClassState, sev: lsSevOK,
		label: "Cluster has become healthy",
		means: "Every instance is up and the operator is satisfied. After a recovery this is the moment the new cluster is usable.",
	},
	{
		substr: []string{"Instance is still down, will retry"},
		class:  lsClassState, sev: lsSevWarn,
		label: "Instance is down",
		means: "The instance manager cannot reach its own PostgreSQL. Repeated once a second, so the count is the outage in seconds.",
	},
	{
		substr: []string{"startup probe failing"},
		class:  lsClassState, sev: lsSevWarn,
		label: "Startup probe failing",
		means: "Kubernetes is waiting for the instance to come up. Ordinary while a member restores or replays WAL; past the probe's budget the container is killed and started again.",
	},
	{
		substr: []string{"Cluster status"},
		class:  lsClassState, sev: lsSevInfo,
		label: "Cluster status",
		means: "The instance manager's periodic view of its cluster.",
	},
	{
		substr: []string{"Cannot extract Pod status", "Defaulting for Cluster", "Defaulting for ScheduledBackup",
			"Updating pvc metadata", "Creating new Pod to reattach a PVC"},
		class: lsClassReconcile, sev: lsSevInfo,
		label: "Reconcile bookkeeping",
		means: "Webhook defaulting and pod/PVC housekeeping, re-logged on every reconcile. 543 'Defaulting for Cluster' records in the corpus, none of them an event.",
	},
	{
		substr: []string{"Reconciler error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Reconcile failed",
		means: "controller-runtime's retry record with an exponential backoff behind it.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Reconcile failed: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"Starting EventSource", "Starting Controller", "Starting workers",
			"Registering webhook", "Registering a validating webhook", "Registering a mutating webhook",
			"Error loading plugins, retrying"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "The manager wiring itself up, including its plugin connections — the barman-cloud plugin is one of these, and a cluster whose backups are plugin-based cannot back up until it loads.",
	},
}

// ---------------------------------------------------------------- classify

func lsClassifyPerconaPG(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsPerconaPGRules, nil)
}

func lsClassifyCrunchy(r lsRecord) (lsEvent, bool) {
	// Crunchy logs at debug by default and most of it is the reconcile loop restating
	// itself. Anything the catalogue does not recognise at debug level is dropped rather
	// than filed as background: 132 "reconciled instance" records is not 132 events, and
	// an unrecognised debug line is by definition not news.
	if strings.EqualFold(r.Level, "DEBUG") {
		e, keep := lsClassifyOpRecord(r, lsCrunchyRules, nil)
		if !keep || e.Class == lsClassOther {
			return lsEvent{}, false
		}
		return e, true
	}
	return lsClassifyOpRecord(r, lsCrunchyRules, nil)
}

func lsClassifyCNPG(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsCNPGRules, nil)
}

// ---------------------------------------------------------------- state tracks

// lsResolvePGOperator gives the two Patroni-based operators their only state: whether the
// controller was running. They have nothing else to say — see the note at the top of
// logsummary_pgop.go.
func lsResolvePGOperator(events []lsEvent) {
	for i := range events {
		if events[i].Label == "Operator start-up" {
			events[i].State = lsStateStarting
		}
	}
}

// lsResolveCNPG gives a CloudNativePG source its states. It is the only one of the three
// with a real track, because it is the only one that runs the failover itself.
func lsResolveCNPG(events []lsEvent) {
	for i := range events {
		e := &events[i]
		if e.State != "" {
			continue
		}
		switch e.Label {
		case "Switchover or failover in progress", "Former primary waiting to rejoin":
			e.State = lsStateCNPGSwitch
		case "Switchover completed", "Primary selected", "Cluster status":
			e.State = lsStateCNPGHealth
		case "Instance is down":
			e.State = lsStateDown
		}
	}
}
