package main

// logsummary_pxcop_rules.go — the catalogue: what each record the PXC operator and its
// binlog collector write actually means.
//
// The rules are `lsRule`s, the same shape the Galera, Group Replication, PostgreSQL and
// Valkey catalogues use, so the matching order and the level floor behave identically. Two
// things about this catalogue are its own:
//
//   - **The level floor is switched off for the operator, deliberately, in both
//     directions.** `INFO reconcile replication error` is a failure to reach the database
//     at all and would be filed as background by its level; `ERROR Reconciler error` is
//     emitted once per retry with an exponential backoff, so five seconds of one broken
//     thing is five "bad" events. Both are handled by the rules rather than by the level,
//     and everything the catalogue does NOT recognise still takes its level as a floor —
//     the same compromise the rest of the package makes.
//
//   - **Almost every record is about an object other than the sender.** The operator is
//     one process reconciling many things; the interesting fact in a record is nearly
//     always in the trailing field object, not the message. So most rules here have an
//     `enrich` that reads a field.

import (
	"strconv"
	"strings"
)

// lsOpRules is the operator's catalogue, most specific first.
var lsOpRules = []lsRule{
	// ---------------------------------------------------------------- backups
	{
		substr: []string{"Created a new backup job"},
		class:  lsClassBackup, sev: lsSevWarn,
		label: "Backup started",
		means: "The operator created the Job that runs xtrabackup. The cluster keeps serving throughout — a PXC backup streams from one member, which desyncs it for the duration; that member's own log is where to see which one and for how long.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("name"); v != "" {
				e.Peer = v
			}
		},
	},
	{
		substr: []string{"Backup succeeded"},
		class:  lsClassBackup, sev: lsSevOK,
		label: "Backup succeeded",
		means: "The backup Job finished and the operator marked the PerconaXtraDBClusterBackup Succeeded. What it does NOT say is how big it was or how long the donor was desynced.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.first("PerconaXtraDBClusterBackup"); v != "" {
				e.Peer = lsOpObjName(v)
			}
		},
	},
	{
		substr: []string{"Creating or updating backup job", "add new job"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "Scheduled job registered",
		means: "A cron entry the operator keeps for this cluster — a scheduled backup, the version check, or telemetry. The schedule is in the record, which makes this the only place in the logs that says when backups are meant to run.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			job, sched := f.last("name"), f.last("schedule")
			if job != "" && sched != "" {
				e.Label = "Scheduled job registered: " + job + " (" + sched + ")"
			}
		},
	},
	// ---------------------------------------------------------------- restores
	{
		substr: []string{"stopping cluster"},
		class:  lsClassRestore, sev: lsSevBad,
		label: "Restore is stopping the cluster",
		means: "A restore has begun. The operator scales the cluster to zero first — a PXC restore is not an online operation, and from here until 'starting cluster' finishes there is no database at all.",
	},
	{
		substr: []string{"starting restore"},
		class:  lsClassRestore, sev: lsSevWarn,
		label: "Restore job started",
		means: "The Job that pulls the backup out of object storage and writes it into the first member's volume.",
	},
	{
		substr: []string{"preparing cluster"},
		class:  lsClassRestore, sev: lsSevWarn,
		label: "Restore is preparing the cluster",
		means: "xtrabackup --prepare over the restored files, and the remaining members' volumes cleared so they will resync from the first.",
	},
	{
		substr: []string{"point-in-time recovering"},
		class:  lsClassRestore, sev: lsSevWarn,
		label: "Applying binary logs (point-in-time recovery)",
		means: "The restore is past the full backup and is replaying the collected binary logs up to the requested moment. This phase exists only when the restore asked for one, and it is the phase that can fail on a gap.",
	},
	{
		substr: []string{"starting cluster"},
		class:  lsClassRestore, sev: lsSevWarn,
		label: "Restore is starting the cluster",
		means: "The members are being brought back up on the restored data. Nothing serves until the first of them reaches SYNCED.",
	},
	{
		substr: []string{"invalidating binlog collector cache"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Binlog collector cache invalidated",
		means: "A restore rewinds the server's GTID history, so the collector's record of what it has already uploaded is now wrong and is thrown away. The collector restarts and re-reads the members from scratch — and this is the moment a new timeline begins, which is why a gap is usually reported shortly afterwards.",
	},
	{
		substr: []string{"Waiting for restore job to finish", "Waiting for prepare job to finish", "Waiting for cluster to start"},
		class:  lsClassRestore, sev: lsSevInfo,
		label: "Restore in progress",
		means: "The restore controller polls every five seconds and logs each poll. The count is the duration, not the number of things that happened — 185 of these is fifteen minutes of one restore, not 185 events.",
	},
	// ---------------------------------------------------------------- smart update
	{
		substr: []string{"statefulSet was changed, run smart update"},
		class:  lsClassRollout, sev: lsSevWarn,
		label: "Rolling restart started (smart update)",
		means: "Something in the pod template changed — a configuration key, an image, a resource — and the operator will restart every member to apply it. It restarts the secondaries first and the pod it considers primary last, so the write endpoint moves exactly once.",
	},
	{
		substr: []string{"primary pod"},
		class:  lsClassRollout, sev: lsSevInfo,
		label: "Primary pod identified",
		means: "Which member the operator treats as the primary — the one HAProxy sends writes to, and the last one a rolling restart will touch. Galera has no primary of its own, so this fact exists nowhere else in these logs.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			for _, k := range []string{"pod", "primary", "name"} {
				if v := f.last(k); v != "" && strings.Contains(v, "-pxc-") {
					e.Peer = v
					e.Label = "Primary pod: " + v
					return
				}
			}
		},
	},
	{
		substr: []string{"apply changes to primary pod", "apply changes to secondary pod"},
		class:  lsClassRollout, sev: lsSevWarn,
		label: "Restarting a member",
		means: "The operator is deleting this member's pod so it comes back on the new template. Its own log will show the shutdown and the rejoin, including whether that rejoin needed a state transfer.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("pod name"); v != "" {
				e.Peer = v
			} else if v := f.last("pod"); v != "" {
				e.Peer = v
			}
			if strings.Contains(r.Text, "primary") {
				e.Label = "Restarting the primary member"
			} else {
				e.Label = "Restarting a secondary member"
			}
		},
	},
	{
		substr: []string{"smart update finished"},
		class:  lsClassRollout, sev: lsSevOK,
		label: "Rolling restart finished",
		means: "Every member is running the new pod template. The span from 'run smart update' to here is how long the cluster spent with members of two different configurations in it.",
	},
	{
		substr: []string{"Pod is not updated", "Pod is not running", "pod is waiting"},
		class:  lsClassRollout, sev: lsSevInfo,
		label: "Waiting for a member to come back",
		means: "The rolling restart polling the member it just deleted. Ten seconds apart, so the count is the wait.",
	},
	{
		substr: []string{"Pod is updated, running and ready"},
		class:  lsClassRollout, sev: lsSevOK,
		label: "Member is back and ready",
		means: "The restarted member passed its readiness probe, which for a PXC pod means wsrep says Synced and Primary. The operator moves to the next one.",
	},
	// ---------------------------------------------------------------- PITR, as the operator sees it
	{
		substr: []string{"Gap detected in binary logs"},
		class:  lsClassPITR, sev: lsSevBad,
		label: "Gap in the collected binary logs",
		means: "The collector cannot continue the GTID sequence it had: a binary log it needed is no longer on any member — purged, or lost with a member's volume. Point-in-time recovery cannot cross this point. Everything written before it is recoverable only up to the gap; everything after it needs a NEW full backup as its base.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("missingGTIDSet"); v != "" {
				e.Label = "Gap in the collected binary logs — missing " + lsOpTrunc(v, 60)
			}
		},
	},
	{
		substr: []string{"Updated PITR timelines"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Point-in-time recovery horizon",
		means: "The newest moment a point-in-time restore could currently reach, and the full backup it would be built on. This is the only place either number is written down; a restore asking for a moment after 'latest' cannot be served.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			latest, last := f.last("latest"), f.last("lastBackup")
			switch {
			case latest != "" && last != "":
				e.Label = "PITR can reach " + latest + " (from backup " + last + ")"
			case latest != "":
				e.Label = "PITR can reach " + latest
			}
		},
	},
	// ---------------------------------------------------------------- the controller itself
	{
		substr: []string{"Successfully acquired lease"},
		class:  lsClassReconcile, sev: lsSevOK,
		label: "Operator became the leader",
		means: "This process holds the leader lease and is the one reconciling. Only one operator in a namespace does; the others watch.",
	},
	{
		substr: []string{"Attempting to acquire leader lease"},
		class:  lsClassReconcile, sev: lsSevInfo,
		label: "Waiting for the leader lease",
		means: "The operator is up but is not yet the one making decisions.",
	},
	{
		substr: []string{"Manager starting up"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "Operator starting",
		means: "The operator process started. Everything above this line in the file is a previous life of the same Deployment, and the gap between them is time during which nothing was reconciling the cluster.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("gitBranch"); v != "" {
				e.Label = "Operator starting (" + strings.TrimPrefix(strings.ReplaceAll(v, "-", "."), "release.") + ")"
			}
		},
	},
	{
		substr: []string{"Runs on"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "Kubernetes version",
		means: "The platform and API version the operator found. A cluster too old for the operator's CRDs is the failure this line is the evidence for.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("version"); v != "" {
				e.Label = "Running on Kubernetes " + v
			}
		},
	},
	{
		substr: []string{"update PXC version (fetched from db)"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "PXC version read from the database",
		means: "The operator asked a running member what version it is. It is the authoritative answer for what the cluster is actually running, as opposed to what the image tag says.",
		enrich: func(r lsRecord, e *lsEvent) {
			f := lsOpFieldsOf(r)
			if v := f.last("new version"); v != "" {
				e.Label = "Cluster is running PXC " + v
			}
		},
	},
	{
		// The one that reads as background and is not. It is INFO, and it means the
		// operator could not reach the database at all.
		substr: []string{"reconcile replication error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Operator could not reach the cluster",
		means: "Logged at INFO, and it is a failure: the operator could not open a connection through the proxy to find the primary. While this repeats, users, replication channels and PITR settings are not being reconciled — the database may be perfectly healthy and simply unreachable from the operator.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Operator could not reach the cluster: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"Reconciler error"},
		class:  lsClassReconcile, sev: lsSevWarn, overLevel: true,
		label: "Reconcile failed",
		means: "controller-runtime's own record that a reconcile returned an error; the Go stack trace underneath is its call path, not a crash. It is retried with an exponential backoff, so one persistent fault produces a long run of these — the FIRST one is when the fault began and the LAST is the last time it was still there.",
		enrich: func(r lsRecord, e *lsEvent) {
			if msg := lsOpErrText(lsOpFieldsOf(r)); msg != "" {
				e.Label = "Reconcile failed: " + lsOpTrunc(msg, 90)
			}
		},
	},
	{
		substr: []string{"granted privileges", "privileges granted", "Password expiration policy updated"},
		class:  lsClassSecurity, sev: lsSevInfo,
		label: "Operator maintaining its database users",
		means: "The operator owns the root, operator, monitor, xtrabackup and replication accounts and re-asserts their grants and password policy on every reconcile. A burst of these after a secret changes is a password rotation.",
	},
	{
		substr: []string{"Updated current TLS certificate", "Starting certificate poll+watcher"},
		class:  lsClassSecurity, sev: lsSevInfo,
		label: "Webhook certificate loaded",
		means: "The certificate the validating webhook serves with. It is the operator's own, not the cluster's.",
	},
	{
		substr: []string{"Starting EventSource", "Starting Controller", "Starting workers",
			"Registering Components", "Starting the Cmd", "Registering webhook",
			"Serving webhook server", "Starting webhook server", "Starting metrics server",
			"Serving metrics server", "starting server", "Feature gates"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Operator start-up",
		means: "controller-runtime wiring itself up. Present in every start of every healthy operator.",
	},
}

// lsOpObjName pulls the `name` out of a re-encoded object field such as
// `{"name":"backup1","namespace":"pxc"}`.
func lsOpObjName(v string) string {
	i := strings.Index(v, `"name":"`)
	if i < 0 {
		return ""
	}
	rest := v[i+len(`"name":"`):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// lsPITRRules is the binlog collector's catalogue.
//
// It is a small vocabulary and half of it is inventory: the collector prints one line per
// binary log it can see, every cycle, which on a busy cluster is hundreds of lines that
// say nothing. Those are dropped (lsPITRNoise) rather than collapsed, because they are not
// repeats of one event — they are a listing.
var lsPITRRules = []lsRule{
	{
		substr: []string{"Gap detected in the binary logs"},
		class:  lsClassPITR, sev: lsSevBad,
		label: "Gap in the binary logs — recovery cannot cross it",
		means: "The collector found that the next binary log it needed is gone, and says outright what that costs: it keeps uploading, so the bucket keeps growing and looks healthy, but a point-in-time restore cannot be built across the gap. A new full backup is what makes the logs after it usable again.",
	},
	{
		substr: []string{"Couldn't find the binlog that contains GTID set"},
		class:  lsClassPITR, sev: lsSevBad,
		label: "The binary log holding the next GTID is gone",
		means: "The collector's last uploaded position is no longer present on any member. Usually a purge, a member rebuilt from a state transfer, or a restore that rewound the timeline.",
	},
	{
		substr: []string{"switching PITR binlog source"},
		class:  lsClassPITR, sev: lsSevWarn,
		label: "Collector moved to another member",
		means: "The collector reads binary logs from ONE member at a time. It moves when that member stops being Synced/Primary or when another has older logs — which makes this line a second witness that a member was unhealthy, at a moment the operator's own log may say nothing about.",
		enrich: func(r lsRecord, e *lsEvent) {
			if _, to, ok := strings.Cut(r.Text, " to "); ok {
				if host, _, ok := strings.Cut(to, " because "); ok {
					e.Peer = lsPITRShortHost(strings.TrimSpace(host))
				}
			}
			if strings.Contains(r.Text, "not healthy") {
				e.Label = "Collector moved off an unhealthy member"
				e.Sev = lsSevWarn
			}
		},
	},
	{
		substr: []string{"running binlog collector"},
		class:  lsClassPITR, sev: lsSevOK,
		label: "Binlog collector running",
		means: "From here, transactions are being copied to object storage and point-in-time recovery can reach the present. Before it — or after it stops — the only recovery point is a full backup.",
	},
	{
		substr: []string{"initializing collector"},
		class:  lsClassStartup, sev: lsSevInfo,
		label: "Collector starting",
		means: "The collector process started. Every start after the first is either a restart of the pod or a restart the operator caused by invalidating its cache.",
	},
	{
		substr: []string{"successfully wrote binlog file"},
		class:  lsClassPITR, sev: lsSevOK,
		label: "Binary log uploaded",
		means: "One binary log is now in object storage. The interval between these is the most a point-in-time restore can lose: timeBetweenUploads, plus however long the upload takes.",
		enrich: func(r lsRecord, e *lsEvent) {
			if m := lsPITRBinlogWrote.FindStringSubmatch(r.Text); m != nil {
				e.Label = "Binary log uploaded: " + m[1]
			}
		},
	},
	{
		substr: []string{"cache file gtid-binlog-cache.json not found"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Collector has no cache and is rebuilding it",
		means: "Normal on the collector's first start and after a restore invalidated the cache. It re-reads every binary log on the source member to work out where it left off, which is why the first cycle is slow.",
	},
	{
		substr: []string{"installing binlog UDF component", "is already installed"},
		class:  lsClassConfig, sev: lsSevInfo,
		label: "Binlog UDF component",
		means: "Point-in-time recovery needs component_binlog_utils_udf inside the server to read GTID ranges out of the binary logs. The collector installs it itself on first use.",
	},
	{
		substr: []string{"Peer list updated"},
		class:  lsClassMember, sev: lsSevInfo,
		label: "Collector's view of the members",
		means: "Which members the collector can see, from the headless Service. The list is in the record's detail — a short list here while the cluster is meant to be three is the collector telling you a member is missing.",
	},
	{
		substr: []string{"PXC Node:"}, notSubstr: []string{"Peer finder"},
		class: lsClassState, sev: lsSevInfo,
		label: "Source member's wsrep state",
		means: "The collector checks its source member's wsrep state before reading from it, and prints the whole answer — ready, connected, local state, cluster status and size. It is a wsrep status snapshot from outside the member, on a schedule, which nothing else in these logs provides.",
		enrich: func(r lsRecord, e *lsEvent) {
			if strings.Contains(r.Text, "wsrep_local_state_comment:Synced") &&
				strings.Contains(r.Text, "wsrep_cluster_status:Primary") {
				e.Sev = lsSevOK
			} else {
				e.Sev = lsSevWarn
			}
			if i := strings.Index(r.Text, "wsrep_cluster_size:"); i >= 0 {
				if n, err := strconv.Atoi(strings.TrimSpace(r.Text[i+len("wsrep_cluster_size:"):])); err == nil {
					e.Members = n
				}
			}
			if host, _, ok := strings.Cut(strings.TrimPrefix(r.Text, "PXC Node: "), ":"); ok {
				e.Peer = lsPITRShortHost(host)
			}
		},
	},
	{
		substr: []string{"no binlogs to upload"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Nothing new to upload",
		means: "A collection cycle that found no new transactions. On an idle cluster this is every cycle and is the good news.",
	},
	{
		substr: []string{"invalidating cache for", "invalidate cache: failed to find cache"},
		class:  lsClassPITR, sev: lsSevInfo,
		label: "Collector cache housekeeping",
		means: "The collector keeps one cache entry per member and drops the ones it no longer reads from. 'failed to find cache' is the harmless half of that — there was nothing to drop.",
	},
}

// lsPITRShortHost turns cluster1-pxc-0.cluster1-pxc.pxc.svc.cluster.local into
// cluster1-pxc-0, which is the name the member's own log and `kubectl get pods` use.
func lsPITRShortHost(h string) string {
	h = strings.TrimSpace(h)
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

// lsPITRNoise is the collector's inventory: one line per binary log it can see, per cycle.
//
// Dropped rather than collapsed. Collapsing folds repeats of ONE event into a row with a
// count, and these are not that — they are a listing of different files, and 662 of them
// in a corpus of nine short captures would bury every record that means something. The raw
// file keeps them, as it keeps everything.
func lsPITRNoise(text string) bool {
	for _, p := range []string{
		"checking binlog.", "no cache entry for binlog.", "last uploaded GTID set:",
		"last uploaded end marker", "last uploaded binlog:", "updating binlog cache",
		"starting to process binlog with name", "Determined Domain to be",
		"execing: /opt/percona/get-pxc-state.sh", "connecting to cluster",
		"reading binlogs from pxc with hostname=", "ignoring timeout to populate the cache",
	} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	// `binlog.000018 (2872 bytes) [E:No]: <gtid range>` — the inventory itself.
	return lsPITRBinlogRead.MatchString(text)
}

// ---------------------------------------------------------------- classify

// lsClassifyOperator turns one operator record into an event.
func lsClassifyOperator(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsOpRules, nil)
}

// lsClassifyPITR turns one binlog-collector record into an event.
func lsClassifyPITR(r lsRecord) (lsEvent, bool) {
	return lsClassifyOpRecord(r, lsPITRRules, lsPITRNoise)
}

// lsClassifyOpRecord is the shared body of the two above: match the catalogue, then fall
// back to the record's own level.
func lsClassifyOpRecord(r lsRecord, rules []lsRule, noise func(string) bool) (lsEvent, bool) {
	e := lsEvent{
		TS: r.TS, Approx: r.Approx, Line: r.Line, Time: r.Time, Level: r.Level,
		Code: r.Code, Subsys: r.Subsys, Message: r.Text,
		Class: lsClassOther, Sev: lsSevInfo, Label: r.Text,
	}
	if len(r.Body) > 0 {
		e.Detail = strings.Join(r.Body, "\n")
	}
	for _, rule := range rules {
		if !lsRuleMatches(rule, r) {
			continue
		}
		e.Class, e.Sev, e.Label = rule.class, rule.sev, rule.label
		e.Meaning = rule.means
		if rule.enrich != nil {
			rule.enrich(r, &e)
		}
		if !rule.overLevel {
			e.Sev = lsWorse(e.Sev, lsOpLevelFloor(r.Level))
		}
		return e, true
	}
	if floor := lsOpLevelFloor(r.Level); lsSevRank[floor] >= lsSevRank[lsSevWarn] {
		e.Sev = floor
		e.Label = lsTruncateLabel(r.Text)
		return e, true
	}
	if noise != nil && noise(r.Text) {
		return lsEvent{}, false
	}
	e.Label = lsTruncateLabel(r.Text)
	return e, true
}

// lsOpLevelFloor is the minimum severity a controller record's level justifies.
//
// DPANIC/PANIC/FATAL are zap's levels above ERROR and all three mean the process is about
// to stop, which for an operator means nothing is reconciling the cluster until Kubernetes
// restarts it.
func lsOpLevelFloor(level string) string {
	switch strings.ToUpper(level) {
	case "ERROR", "DPANIC", "PANIC", "FATAL":
		return lsSevBad
	case "WARN", "WARNING":
		return lsSevWarn
	}
	return lsSevInfo
}

// ---------------------------------------------------------------- state track

// lsResolveOperator gives the operator source its two states.
//
// The lease is the whole question. An operator that is up but has not acquired the lease
// changes nothing, and an operator log that ends without ever acquiring it is a cluster
// nobody is reconciling — which looks exactly like a healthy quiet operator if you only
// count records.
func lsResolveOperator(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch e.Label {
		case "Operator became the leader":
			e.State = lsStateOpLeader
		case "Waiting for the leader lease":
			e.State = lsStateOpFollower
		}
		if strings.HasPrefix(e.Label, "Operator starting") {
			e.State = lsStateStarting
		}
	}
}

// lsResolvePITRCollector gives the collector its three.
func lsResolvePITRCollector(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch {
		case e.Label == "Binlog collector running", strings.HasPrefix(e.Label, "Binary log uploaded"):
			e.State = lsStatePITRUp
		case e.Class == lsClassPITR && e.Sev == lsSevBad:
			e.State = lsStatePITRGap
		case e.Label == "Collector starting":
			e.State = lsStateStarting
		}
	}
}
