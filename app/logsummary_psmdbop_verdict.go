package main

// logsummary_psmdbop_verdict.go — what a PSMDB operator's logs add up to.
//
// The PXC verdicts (logsummary_pxcop_verdict.go) are built on a controller that says very
// little; these are built on one that says a great deal and still leaves the two most
// dangerous facts to its backup agents. Three of the findings here exist because the
// corpus produced something nobody would have guessed:
//
//   - point-in-time recovery is running on exactly ONE member, and the other two agents'
//     logs look identical to an agent whose PITR is switched off (lsPSMDBFindingPITRWho)
//   - after a restore PBM refuses to slice the oplog until a NEW full backup exists, while
//     the custom resource still says enabled and `pbm status` still prints ON
//     (lsPSMDBFindingPITRStalled)
//   - a logical restore runs IN PLACE with the cluster still accepting writes, so anything
//     still connected writes into the dataset being restored and those writes survive
//     (lsPSMDBFindingRestoreWritable)

import (
	"fmt"
	"sort"
	"strings"
)

var lsPSMDBFindings = []func(*lsBundle) []lsFinding{
	lsPSMDBFindingPITRStalled,
	lsPSMDBFindingPITRWho,
	lsPSMDBFindingRestoreWritable,
	lsPSMDBFindingRolloutStuck,
	lsPSMDBFindingClusterState,
	lsPSMDBFindingAgentDown,
	lsPSMDBFindingBackup,
	lsPSMDBFindingSettings,
}

func lsHasPSMDBOperator(b *lsBundle) bool {
	return len(lsOpSources(b, lsFlavourPSMDBOperator)) > 0
}

// lsIsPSMDBSrc gates these findings to the sources they were written for — the mirror of
// lsIsPXCOpSrc, and for the same reason: a bundle can hold both operators, and a finding
// that picks on the shape of an event alone will happily explain one in the other's
// vocabulary.
func lsIsPSMDBSrc(b *lsBundle, src int) bool {
	return lsSrcIs(b, src, lsFlavourPSMDBOperator) || lsSrcIs(b, src, lsFlavourPBMAgent)
}

// lsPSMDBFindingPITRStalled is the one this catalogue exists for.
//
// After a restore, PBM will not resume slicing the oplog until a new full backup exists.
// It says so in the agent's log — `no backup found after the restored …, a new backup is
// required to resume PITR` — and nowhere else. `spec.backup.pitr.enabled` is still true,
// `pbm status` still prints `Status [ON]`, and the custom resource looks healthy. Nothing
// is being written to object storage.
func lsPSMDBFindingPITRStalled(b *lsBundle) []lsFinding {
	hits := lsPick(b, func(e lsEvent) bool {
		return e.Label == "PITR cannot resume — no backup since the restore"
	})
	if len(hits) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "psmdb-pitr-stalled", Sev: lsSevBad,
		Title: "Point-in-time recovery is switched on and is not running",
		Detail: "After a restore PBM refuses to slice the oplog until a NEW full backup exists. Everything above the agent still reports health: `spec.backup.pitr.enabled` is true, the custom resource is ready, and `pbm status` prints `Status [ON]`. " +
			"Between this record and the next full backup, nothing is being copied to object storage — so the recoverable window is frozen at the moment of the restore, and every write since then exists only in the database.",
		Advice:  "Take a full backup now (`PerconaServerMongoDBBackup`); slicing resumes by itself once one exists. Worth an alert of its own: the operator's `latestRestorableTime` stops advancing, and that is the only signal above the agent — the CR's own status will not change.",
		At:      hits[0].TS,
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 4),
	}}
}

// lsPSMDBFindingPITRWho names the member doing the work, and says what the others' silence
// means.
//
// PBM nominates one agent per replica set. The winner writes `streaming started`; the
// losers write `skip after pitr nomination` and then nothing. An operator reading a single
// agent's log — the obvious thing to do — sees a log that is indistinguishable from an
// agent whose PITR is switched off entirely.
func lsPSMDBFindingPITRWho(b *lsBundle) []lsFinding {
	agents := lsOpSources(b, lsFlavourPBMAgent)
	if len(agents) == 0 {
		return nil
	}
	slicing := map[int]bool{}
	var evs []lsEvent
	for _, e := range b.Events {
		if e.Label == "This member is slicing the oplog" || e.Label == "Oplog chunk uploaded" {
			slicing[e.Src] = true
			evs = append(evs, e)
		}
	}
	if len(slicing) == 0 {
		sev := lsSevWarn
		detail := fmt.Sprintf("%d backup agent(s) are in this bundle and none of them recorded slicing the oplog. PBM nominates ONE agent per replica set to do it, so this is either point-in-time recovery switched off, or the member that won the nomination not being among the logs you read.", len(agents))
		if len(agents) >= 3 {
			sev = lsSevBad
			detail += " With every member's agent here, it is the first."
		}
		return []lsFinding{{
			ID: "psmdb-pitr-nobody", Sev: sev,
			Title:   "No member recorded slicing the oplog",
			Detail:  detail,
			Advice:  "`pbm status` settles it in one command, and `spec.backup.pitr.enabled` is the switch. Remember that a full backup has to exist before slicing can start at all.",
			Sources: agents,
		}}
	}
	var who []string
	for _, s := range b.Sources {
		if slicing[s.Idx] {
			who = append(who, lsNode(b, s.Idx))
		}
	}
	sort.Strings(who)
	sev, title := lsSevOK, "The oplog is being sliced by "+strings.Join(who, ", ")
	extra := ""
	if len(slicing) > 1 {
		sev = lsSevWarn
		title = "More than one member recorded slicing the oplog"
		extra = " Two members slicing at once is either a handover — read the `stale lock` records beside it — or two agents that could not see each other."
	}
	return []lsFinding{{
		ID: "psmdb-pitr-who", Sev: sev, Title: title,
		Detail: fmt.Sprintf("PBM nominates one agent per replica set and only the winner does any work. The other %d agent(s) here write `skip after nomination` and then go quiet — which is why one agent's log can never tell you whether backups are running, and why reading a single one of them is the mistake this page exists to prevent.%s",
			len(agents)-len(slicing), extra),
		Advice:  "If you alert on the agents, alert on the ABSENCE of `created chunk` across all of them together, not on any one of them.",
		Sources: agents, Events: lsEventNos(evs, 4),
	}}
}

// lsPSMDBFindingRestoreWritable is the difference between the two operators' restores, and
// it is a data-integrity difference.
//
// A PXC restore scales the cluster to zero: nothing can write to it, and the outage is
// visible to everybody. A PSMDB logical restore runs IN PLACE — the pods keep running and
// keep accepting connections while PBM drops the collections, re-creates them from the
// dump and replays the oplog on top. Anything still connected writes into the dataset
// being restored, and those writes are not removed.
//
// Measured on the corpus, exactly: a point-in-time restore to 09:02:35 left ZERO documents
// between the target and the moment PBM re-created the collection at 09:05:43 — the
// restore itself was precise — and 32,000 documents after it, written by a load generator
// that was still shutting down. The replay ran for another seventeen minutes with that
// traffic landing in it.
func lsPSMDBFindingRestoreWritable(b *lsBundle) []lsFinding {
	starts := lsPick(b, func(e lsEvent) bool {
		return lsIsPSMDBSrc(b, e.Src) &&
			(e.Label == "Restore started" || e.Label == "Restore started on this member")
	})
	if len(starts) == 0 {
		return nil
	}
	first := starts[0]
	end := first.TS
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Class == lsClassRestore }) {
		if e.TS > end {
			end = e.TS
		}
	}
	replay := lsPick(b, func(e lsEvent) bool { return e.Label == "Replaying the oplog" })
	how := ""
	if len(replay) > 0 {
		how = fmt.Sprintf(" The oplog replay alone ran from %s, and it is the slow phase: its cost is set by how much was WRITTEN since the base backup, not by how big the dataset is.", lsClock(replay[0].TS))
	}
	return []lsFinding{{
		ID: "psmdb-restore-writable", Sev: lsSevBad,
		Title: "A restore ran with the cluster still accepting writes (" + lsOpDur(end-first.TS) + ")",
		Detail: "A logical PSMDB restore happens IN PLACE. Unlike the PXC operator, which scales its cluster to zero, the pods keep running and keep accepting connections while PBM drops the collections, re-creates them from the dump and replays the oplog on top. " +
			"Anything still connected writes into the dataset being restored, and those writes are NOT removed by the rest of the restore." + how,
		Advice: "Fence the application off before restoring — scale the workload to zero, or take the Service away — and check afterwards for documents newer than the moment PBM re-created the collections, which is in the agent's log. " +
			"Measured on the corpus: a restore to a point in time was exact to the target and still ended with 32,000 documents that were never in the backup, all of them written after the collection was re-created by a client that was merely slow to shut down.",
		At: first.TS, Until: end,
		Sources: lsSrcSet(starts), Events: lsEventNos(append(starts, replay...), 6),
	}}
}

// lsPSMDBFindingRolloutStuck measures a blocked rolling restart rather than counting it.
//
// The operator re-logs `can't start/continue 'SmartUpdate': waiting for all replicas are
// ready` on every reconcile for as long as the condition holds, at INFO, forever. Counting
// them says nothing; the span between the first and the last is the outage.
func lsPSMDBFindingRolloutStuck(b *lsBundle) []lsFinding {
	blocked := lsPick(b, func(e lsEvent) bool { return lsIsPSMDBSrc(b, e.Src) && e.Label == "Rolling restart is blocked" })
	if len(blocked) < 3 {
		return nil
	}
	span := blocked[len(blocked)-1].TS - blocked[0].TS
	if span < 60 {
		return nil
	}
	still := ""
	if b.Summary.LastTS-blocked[len(blocked)-1].TS < 120 {
		still = " It was still blocked at the end of this window."
	}
	return []lsFinding{{
		ID: "psmdb-rollout-stuck", Sev: lsSevBad,
		Title: "A rolling restart was blocked for " + lsOpDur(span),
		Detail: fmt.Sprintf("The operator will not restart the next member until every replica is ready, so one member that cannot become ready stops the rollout indefinitely — and it re-logs that at INFO on every reconcile rather than escalating. %d records between %s and %s.%s",
			len(blocked), lsClock(blocked[0].TS), lsClock(blocked[len(blocked)-1].TS), still),
		Advice: "The reason is a Kubernetes fact the operator never mentions: `kubectl get pods` and the pending pod's events. In the corpus it was anti-affinity — a spec edit that replaced the whole `replsets` array dropped `affinity.antiAffinityTopologyKey: none`, and the operator's default spreads members across nodes, which on a single-node cluster leaves the last one Pending forever. " +
			"A merge patch replaces a list rather than merging into it, so edit `replsets` with the whole object or use a JSON patch.",
		At: blocked[0].TS, Until: blocked[len(blocked)-1].TS,
		Sources: lsSrcSet(blocked), Events: lsEventNos(blocked, 3),
	}}
}

// lsPSMDBFindingClusterState reports what the operator thought the cluster was, and for how
// long — the one thing the PSMDB operator gives that the PXC one does not.
func lsPSMDBFindingClusterState(b *lsBundle) []lsFinding {
	if !lsHasPSMDBOperator(b) {
		return nil
	}
	var initTotal float64
	var worst float64
	var at float64
	for _, p := range b.Phases {
		if p.State != lsStatePSMDBInit {
			continue
		}
		d := p.To - p.From
		initTotal += d
		if d > worst {
			worst, at = d, p.From
		}
	}
	if worst < 120 {
		return nil // ordinary rollout churn
	}
	return []lsFinding{{
		ID: "psmdb-cr-initializing", Sev: lsSevWarn,
		Title: "The operator held the cluster in `initializing` for " + lsOpDur(worst),
		Detail: fmt.Sprintf("`initializing` is the operator's word for 'this does not match the spec yet' — members being added, restarted or reconfigured. It is ordinary during a rollout and it has no timeout: a cluster that enters it and does not come out is stuck, and the state alone will not say why. Total time in it across this window: %s.",
			lsOpDur(initTotal)),
		Advice: "Pair it with the rollout findings above and with `kubectl get pods`. The state is in `.status.state` on the custom resource, which makes it the cheapest thing to alert on.",
		At:     at, Until: at + worst,
		Sources: lsOpSources(b, lsFlavourPSMDBOperator),
	}}
}

// lsPSMDBFindingAgentDown reports agents that could not do their job, which is a different
// question from whether the database was up.
func lsPSMDBFindingAgentDown(b *lsBundle) []lsFinding {
	restarts := lsPick(b, func(e lsEvent) bool { return e.Label == "Agent restarting after a failure" })
	lost := lsPick(b, func(e lsEvent) bool {
		return e.Label == "Agent could not write its log into the database"
	})
	if len(restarts) < 2 && len(lost) == 0 {
		return nil
	}
	var parts []string
	if len(restarts) > 0 {
		parts = append(parts, fmt.Sprintf("%d agent restart(s) after a failed start", len(restarts)))
	}
	if len(lost) > 0 {
		parts = append(parts, fmt.Sprintf("%d record(s) that PBM could not write its own log into the database", len(lost)))
	}
	evs := append(append([]lsEvent{}, restarts...), lost...)
	return []lsFinding{{
		ID: "psmdb-agent-down", Sev: lsSevWarn,
		Title:   "A backup agent could not reach its cluster",
		Detail:  strings.Join(parts, "; ") + ". PBM keeps its log INSIDE MongoDB, so an agent that cannot reach the database cannot record its own failure there — the stderr lines in this source are the only copy, and by construction every one of them was written while something was wrong. An agent that is down takes no backups and slices no oplog, and the database itself may be perfectly healthy throughout.",
		Advice:  "Ordinary once or twice on a new cluster while the replica set is still being initiated (`Type: RSGhost`). Persistent restarts with `AuthenticationFailed` after a restore mean the restore replaced the users PBM logs in with — the operator recreates them, and until it does the agent cannot start.",
		At:      evs[0].TS,
		Sources: lsSrcSet(evs), Events: lsEventNos(evs, 4),
	}}
}

// lsPSMDBFindingBackup reports backups, and where the detail actually is.
func lsPSMDBFindingBackup(b *lsBundle) []lsFinding {
	done := lsPick(b, func(e lsEvent) bool { return lsIsPSMDBSrc(b, e.Src) && e.Label == "Backup finished on this member" })
	started := lsPick(b, func(e lsEvent) bool {
		return lsIsPSMDBSrc(b, e.Src) &&
			(e.Label == "Backup started" || e.Label == "Backup command received")
	})
	failed := lsPick(b, func(e lsEvent) bool {
		return strings.HasPrefix(e.Label, "Backup state:") && e.Sev == lsSevBad
	})
	if len(done) == 0 && len(started) == 0 && len(failed) == 0 {
		return nil
	}
	var out []lsFinding
	if len(done) > 0 {
		last := done[len(done)-1]
		out = append(out, lsFinding{
			ID: "psmdb-backup-ok", Sev: lsSevOK,
			Title:  fmt.Sprintf("%d backup(s) finished", len(done)),
			Detail: "The most recent finished at " + lsClock(last.TS) + " on " + lsNode(b, last.Src) + " — the member that won the nomination, and the only one whose log holds the dump's collections and sizes. A backup also stops the oplog slicer for its duration.",
			At:     last.TS, Sources: lsSrcSet(done), Events: lsEventNos(done, 4),
		})
	}
	if len(failed) > 0 {
		out = append(out, lsFinding{
			ID: "psmdb-backup-failed", Sev: lsSevBad,
			Title:   fmt.Sprintf("%d backup(s) failed", len(failed)),
			Detail:  "The operator recorded the PerconaServerMongoDBBackup moving to an error state. The reason is not in its log — it is in the nominated agent's, which is the one that ran the job.",
			Advice:  "Read every agent in the set: which one ran it is not knowable in advance, and the two that did not will say nothing about the failure.",
			At:      failed[0].TS,
			Sources: lsSrcSet(failed), Events: lsEventNos(failed, 4),
		})
	}
	return out
}

// lsPSMDBFindingSettings is the configuration report for an operator-managed replica set.
//
// It leans on lsMongoScanConfig, which already reads the engine's own `Opening WiredTiger`
// line off any mongod log — the cache size the engine actually opened with, not what the
// config asked for. What this adds is the operator's half: the settings that live in the
// custom resource and appear nowhere in the member's log at all.
func lsPSMDBFindingSettings(b *lsBundle) []lsFinding {
	if !lsHasPSMDBOperator(b) && len(lsOpSources(b, lsFlavourPBMAgent)) == 0 {
		return nil
	}
	var cfg *lsMongoConfig
	src := -1
	for _, s := range b.Sources {
		if s.MongoCfg != nil && s.MongoCfg.CacheMB > 0 {
			cfg, src = s.MongoCfg, s.Idx
			break
		}
	}
	tips := lsPSMDBAdvice(b, cfg)
	if len(tips) == 0 {
		return nil
	}
	sev := lsSevInfo
	var lines []string
	for _, t := range tips {
		sev = lsWorse(sev, t.Sev)
		lines = append(lines, fmt.Sprintf("%s = %s → %s. %s", t.Key, t.Is, t.Want, t.Why))
	}
	srcs := lsOpSources(b, lsFlavourPSMDBOperator)
	if src >= 0 {
		srcs = append(srcs, src)
	}
	return []lsFinding{{
		ID: "psmdb-settings", Sev: sev,
		Title: "What this cluster is actually configured with",
		Detail: "The engine's numbers come from the member's own `Opening WiredTiger` line, which is what the engine opened with rather than what the config asked for. The operator's come from the records where it pushes them into PBM. " +
			"Everything else in `spec.replsets[].configuration` reaches the member as a config file and is not echoed anywhere, so a setting you cannot see here is a setting only `kubectl get psmdb -o yaml` can show you.\n\n" +
			strings.Join(lines, "\n\n"),
		Advice: "These are set through `spec.replsets[].configuration` (a mongod config-file fragment) and `spec.backup.pitr` in the custom resource. Changing either triggers a rolling restart, secondaries first — so batch the changes. " +
			"And edit `replsets` as a whole object: it is a LIST, and a merge patch replaces a list rather than merging into it, which is how a cluster in this corpus lost its anti-affinity setting and stopped rolling out entirely.",
		Sources: srcs,
	}}
}
