package main

// logsummary_pxcop_verdict.go — what an operator-managed PXC cluster's logs add up to.
//
// The findings here divide into three kinds, and the division is the argument for reading
// all three logs together:
//
//   - what only the OPERATOR log can say: that a rolling restart happened and in what
//     order, that a restore took the cluster down and for how long, that a backup ran,
//     which pod it considers the primary, how far point-in-time recovery currently reaches
//   - what only the COLLECTOR log can say: whether point-in-time recovery is actually
//     being collected, and whether its sequence has a hole in it
//   - what neither says, and the member logs do — which is most of what an operator is
//     woken up for. lsPXCOpFindingSilentOnMembers is that finding, and it exists because
//     the corpus produced it: a member force-deleted under write load, evicted, restarted
//     and rejoined by state transfer, with two minutes of `Updated PITR timelines` in the
//     operator's log over the same window and nothing else.

import (
	"fmt"
	"sort"
	"strings"
)

// lsPXCOpFindings is the set, run by lsFindings alongside every other family's.
var lsPXCOpFindings = []func(*lsBundle) []lsFinding{
	lsPXCOpFindingRestore,
	lsPXCOpFindingBackup,
	lsPXCOpFindingPITRGap,
	lsPXCOpFindingPITRAbsent,
	lsPXCOpFindingRollout,
	lsPXCOpFindingReconcileStuck,
	lsPXCOpFindingProbeRestart,
	lsPXCOpFindingSilentOnMembers,
	lsPXCOpFindingSettings,
}

// lsOpSources are the indices of the controller sources in a bundle, by flavour.
func lsOpSources(b *lsBundle, flavour string) []int {
	var out []int
	for _, s := range b.Sources {
		if s.Flavour == flavour {
			out = append(out, s.Idx)
		}
	}
	return out
}

func lsHasOperatorLog(b *lsBundle) bool { return len(lsOpSources(b, lsFlavourPXCOperator)) > 0 }

// lsIsPXCOpSrc gates every finding in this file to the two sources it was written for.
//
// It exists because a second Kubernetes vocabulary arrived and immediately proved the
// hazard lsSrcIs already warns about. The PSMDB operator's catalogue uses the same classes
// and some of the same labels — `Backup started`, and a PITR record filed bad — so on a
// MongoDB bundle these findings fired and explained a PBM problem in xtrabackup's
// vocabulary: "Gap in the collected binary logs" about a cluster that has no binary logs,
// and "4 backups started and no success was recorded" about backups that had all succeeded
// under a label this catalogue does not own. Caught by reading the live page.
func lsIsPXCOpSrc(b *lsBundle, src int) bool {
	return lsSrcIs(b, src, lsFlavourPXCOperator) || lsSrcIs(b, src, lsFlavourPXCPITR)
}

// lsPXCOpFindingRestore reports each restore, with the outage measured.
//
// A PXC restore is not an online operation and the operator's records say so in sequence:
// `stopping cluster` → `starting restore` → `preparing cluster` → (`point-in-time
// recovering`) → `starting cluster`. What the operator never writes is a record saying the
// restore FINISHED — the controller simply stops polling. So the end is the last
// `Waiting for cluster to start` plus one poll interval, which is exact to five seconds
// because that is how often the controller polls.
//
// One finding per restore, not one per bundle. Getting that wrong is not cosmetic: a
// bundle holding two restores twenty minutes apart, measured from the first one's start to
// the last member that came back, reported a 16-minute outage for a restore that took six.
const lsRestorePoll = 5.0

func lsPXCOpFindingRestore(b *lsBundle) []lsFinding {
	starts := lsPick(b, func(e lsEvent) bool { return lsIsPXCOpSrc(b, e.Src) && e.Label == "Restore is stopping the cluster" })
	if len(starts) == 0 {
		return nil
	}
	progress := lsPick(b, func(e lsEvent) bool { return lsIsPXCOpSrc(b, e.Src) && e.Class == lsClassRestore })
	var out []lsFinding
	for i, first := range starts {
		next := b.Summary.LastTS + 1
		if i+1 < len(starts) {
			next = starts[i+1].TS
		}
		end, pitrAt := first.TS, 0.0
		var evs []lsEvent
		for _, e := range progress {
			if e.TS < first.TS || e.TS >= next {
				continue
			}
			if e.TS > end {
				end = e.TS
			}
			if e.Label == "Applying binary logs (point-in-time recovery)" && pitrAt == 0 {
				pitrAt = e.TS
			}
			if e.Sev != lsSevInfo {
				evs = append(evs, e)
			}
		}
		end += lsRestorePoll
		kind, how := "A backup was restored", ""
		if pitrAt > 0 {
			kind = "A point-in-time restore ran"
			how = " It replayed binary logs on top of the full backup from " + lsClock(pitrAt) + "."
		}
		out = append(out, lsFinding{
			ID: "pxcop-restore", Sev: lsSevWarn,
			Title: kind + " — the cluster was down for " + lsOpDur(end-first.TS),
			Detail: fmt.Sprintf("%s. The operator scaled the cluster to zero at %s and stopped waiting for it %s later — a restore is a full outage, not a rolling one.%s "+
				"The operator writes no record saying a restore finished, so that end is its last `Waiting for cluster to start` poll, which is exact to %.0f seconds.",
				kind, lsClock(first.TS), lsOpDur(end-first.TS), how, lsRestorePoll),
			Advice: "Two consequences that are only visible afterwards. The restore replaces every member's data directory, and `mysqld-error.log` lives in it — so the members' own logs of everything before the restore are gone, which is also why a bundle read after a restore may report that its sources do not overlap. And the restore rewinds the GTID history, so the binlog collector's cache is invalidated and a new timeline begins: take a fresh full backup immediately, because point-in-time recovery cannot cross the boundary.",
			At:     first.TS, Until: end,
			Sources: lsSrcSet(evs), Events: lsEventNos(evs, 8),
		})
	}
	return out
}

// lsPXCOpFindingBackup reports backups that ran, and the ones that did not finish.
//
// The second half is the one worth having. A backup whose Job keeps failing leaves the
// PerconaXtraDBClusterBackup in `Running` — for seventeen minutes and five failed Jobs in
// the corpus — so a dashboard that reads the CR's status sees a backup in progress, not a
// backup that is not going to happen.
func lsPXCOpFindingBackup(b *lsBundle) []lsFinding {
	started := lsPick(b, func(e lsEvent) bool { return lsIsPXCOpSrc(b, e.Src) && e.Label == "Backup started" })
	done := lsPick(b, func(e lsEvent) bool { return lsIsPXCOpSrc(b, e.Src) && e.Label == "Backup succeeded" })
	if len(started) == 0 && len(done) == 0 {
		return nil
	}
	var out []lsFinding
	if len(done) > 0 {
		last := done[len(done)-1]
		out = append(out, lsFinding{
			ID: "pxcop-backup-ok", Sev: lsSevOK,
			Title:  fmt.Sprintf("%d backup(s) completed", len(done)),
			Detail: "The most recent finished at " + lsClock(last.TS) + ". A PXC backup streams with xtrabackup from one member, which desyncs for the duration — the member's own log names which one and for how long, under `Shifting SYNCED -> DONOR/DESYNCED`.",
			At:     last.TS, Sources: lsSrcSet(done), Events: lsEventNos(done, 8),
		})
	}
	if unfinished := len(started) - len(done); unfinished > 0 {
		last := started[len(started)-1]
		out = append(out, lsFinding{
			ID: "pxcop-backup-unfinished", Sev: lsSevWarn,
			Title:  fmt.Sprintf("%d backup(s) started and no success was recorded", unfinished),
			Detail: "A backup Job was created and the log holds no `Backup succeeded` for it. It may still be running, or its Job may be failing and being retried — the operator retries up to `spec.backup.backoffLimit` (6 by default), and while it does the PerconaXtraDBClusterBackup stays in `Running` rather than `Failed`. Measured on the corpus: a backup pointed at an unreachable S3 endpoint sat in `Running` for seventeen minutes behind five errored Jobs.",
			Advice: "`kubectl -n <ns> get jobs -l percona.com/backup-job=true` and the failed pod's log are where the reason is; it is not in the operator's. Set `spec.backup.activeDeadlineSeconds` so a backup that cannot work fails instead of retrying quietly.",
			At:     last.TS, Sources: lsSrcSet(started), Events: lsEventNos(started, 8),
		})
	}
	return out
}

// lsPXCOpFindingPITRGap reports a hole in the collected binary logs.
//
// Both sources can say it and they say it differently: the operator names the missing GTID
// set, the collector names what it costs. Reported once, with whichever halves are present.
func lsPXCOpFindingPITRGap(b *lsBundle) []lsFinding {
	gaps := lsPick(b, func(e lsEvent) bool {
		return lsIsPXCOpSrc(b, e.Src) && e.Class == lsClassPITR && e.Sev == lsSevBad
	})
	if len(gaps) == 0 {
		return nil
	}
	missing := ""
	for _, e := range gaps {
		if i := strings.Index(e.Label, "missing "); i >= 0 {
			missing = e.Label[i+len("missing "):]
			break
		}
	}
	detail := "The binlog collector cannot continue the GTID sequence it had: a binary log it needed is no longer on any member. It keeps uploading — the bucket goes on growing and looks healthy — but a point-in-time restore cannot be built across the hole."
	if missing != "" {
		detail += " The missing range is " + missing + "."
	}
	return []lsFinding{{
		ID: "pxcop-pitr-gap", Sev: lsSevBad,
		Title:  "Point-in-time recovery has a gap and cannot cross it",
		Detail: detail,
		Advice: "Take a full backup now: everything after the gap becomes recoverable again only with a new base. The usual causes are all visible elsewhere in these logs — a restore that rewound the timeline (`invalidating binlog collector cache`), a member rebuilt by a full state transfer, or binary logs purged before the collector reached them. If it is the last of those, `spec.backup.pitr.timeBetweenUploads` (60 s by default) is racing your `binlog_expire_logs_seconds`.",
		At:     gaps[0].TS, Sources: lsSrcSet(gaps), Events: lsEventNos(gaps, 8),
	}}
}

// lsPXCOpFindingPITRAbsent is the honest note about the thing that leaves no record.
//
// Point-in-time recovery being OFF writes nothing anywhere. There is no "PITR disabled"
// record in the operator's log; the collector Deployment is simply deleted and its log
// stops existing. So the only evidence is the shape of the bundle itself: an operator log
// with no collector beside it.
func lsPXCOpFindingPITRAbsent(b *lsBundle) []lsFinding {
	if !lsHasOperatorLog(b) || len(lsOpSources(b, lsFlavourPXCPITR)) > 0 {
		return nil
	}
	horizon := lsPick(b, func(e lsEvent) bool { return strings.HasPrefix(e.Label, "PITR can reach ") })
	sev, title := lsSevWarn, "No binlog collector log is in this bundle — point-in-time recovery may not be running"
	detail := "Nothing here records point-in-time recovery being turned on or off: when `spec.backup.pitr.enabled` goes false the collector Deployment is deleted, and a deleted Deployment writes no farewell. The absence of its log is therefore either a collector that is not running or a collector you did not read."
	if len(horizon) > 0 {
		detail += " The operator was still updating PITR timelines in this window (" + horizon[len(horizon)-1].Label + "), so at that moment it was running — read `deploy/<cluster>-pitr` as well and the timeline will say so directly."
	} else {
		sev = lsSevWarn
		detail += " The operator log in this bundle contains no `Updated PITR timelines` record either, which is what a cluster with PITR switched off looks like: the only recovery point is the last full backup, and everything written since it would be lost."
	}
	return []lsFinding{{
		ID: "pxcop-pitr-absent", Sev: sev, Title: title, Detail: detail,
		Advice:  "`kubectl -n <ns> get deploy <cluster>-pitr` settles it in one command. If it is off and you want it on, `spec.backup.pitr.enabled: true` with a `storageName` — and take a full backup straight afterwards, because the collector has nothing to be a continuation of until you do.",
		Sources: lsOpSources(b, lsFlavourPXCOperator),
	}}
}

// lsPXCOpFindingRollout reports a smart update: the operator restarting every member to
// apply a change.
//
// The order is the interesting part and it is the operator's own: secondaries first,
// the pod it calls primary last, so the write endpoint moves exactly once instead of
// once per member.
func lsPXCOpFindingRollout(b *lsBundle) []lsFinding {
	starts := lsPick(b, func(e lsEvent) bool {
		return lsIsPXCOpSrc(b, e.Src) && e.Label == "Rolling restart started (smart update)"
	})
	if len(starts) == 0 {
		return nil
	}
	ends := lsPick(b, func(e lsEvent) bool { return lsIsPXCOpSrc(b, e.Src) && e.Label == "Rolling restart finished" })
	primary := lsPick(b, func(e lsEvent) bool { return strings.HasPrefix(e.Label, "Primary pod: ") })
	first := starts[0]
	end, finished := first.TS, false
	if len(ends) > 0 {
		end, finished = ends[len(ends)-1].TS, true
	}
	who := ""
	if len(primary) > 0 && primary[0].Peer != "" {
		who = " The operator treated " + primary[0].Peer + " as the primary and restarted it last."
	}
	title := fmt.Sprintf("A rolling restart applied a configuration change to every member (%s)", lsOpDur(end-first.TS))
	sev := lsSevWarn
	if !finished {
		title = "A rolling restart began and this log does not show it finishing"
		sev = lsSevBad
	}
	return []lsFinding{{
		ID: "pxcop-rollout", Sev: sev, Title: title,
		Detail: "Something in the pod template changed — a configuration key, an image, a resource request — and the operator restarted the members one at a time to apply it." + who +
			" Each restarted member rejoins by state transfer; whether that was incremental or a full copy is in the member logs, and at the shipped gcache size it is usually a full copy.",
		Advice: "A rolling restart is the cheapest way to discover what your gcache is worth. If the members' logs show SST rather than IST here, that is what every future restart will cost too — see the configuration finding.",
		At:     first.TS, Until: end, Sources: lsSrcSet(starts), Events: lsEventNos(append(starts, primary...), 8),
	}}
}

// lsPXCOpFindingReconcileStuck reports the operator failing to reconcile, measured from
// the first failure to the last rather than counted.
//
// Counting is what a naive reading does and it is wrong twice over: controller-runtime
// retries with an exponential backoff, so the number of records is a function of how long
// the fault lasted and nothing else, and the same fault produces both an INFO record (the
// operator's own) and an ERROR one (the framework's).
func lsPXCOpFindingReconcileStuck(b *lsBundle) []lsFinding {
	fails := lsPick(b, func(e lsEvent) bool {
		return lsIsPXCOpSrc(b, e.Src) && e.Class == lsClassReconcile && e.Sev == lsSevWarn
	})
	if len(fails) == 0 {
		return nil
	}
	// Grouped by the error sentence, so two different faults are told apart — but reported
	// as ONE finding, because five findings whose titles differ only in a duration read as
	// five outages rather than as one operator having a bad afternoon.
	byErr := map[string][]lsEvent{}
	for _, e := range fails {
		key := e.Label
		if i := strings.Index(key, ": "); i > 0 {
			key = key[i+2:]
		}
		byErr[key] = append(byErr[key], e)
	}
	keys := make([]string, 0, len(byErr))
	for k := range byErr {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := byErr[keys[i]], byErr[keys[j]]
		return gi[len(gi)-1].TS-gi[0].TS > gj[len(gj)-1].TS-gj[0].TS
	})
	var lines []string
	var evs []lsEvent
	worst, at, until := 0.0, 0.0, 0.0
	still := false
	for _, k := range keys {
		g := byErr[k]
		span := g[len(g)-1].TS - g[0].TS
		if len(g) < 3 && span < 30 {
			continue // a single retry that succeeded is not a finding
		}
		if span > worst {
			worst, at, until = span, g[0].TS, g[len(g)-1].TS
		}
		if b.Summary.LastTS-g[len(g)-1].TS < 60 {
			still = true
		}
		lines = append(lines, fmt.Sprintf("%s — %d records over %s, from %s: %s",
			lsOpDur(span), len(g), lsOpDur(span), lsClock(g[0].TS), k))
		evs = append(evs, g[:minInt(len(g), 2)]...)
	}
	if len(lines) == 0 {
		return nil
	}
	tail := ""
	if still {
		tail = " At least one of them was still failing at the end of this window."
	}
	return []lsFinding{{
		ID: "pxcop-reconcile", Sev: lsSevWarn,
		Title: fmt.Sprintf("The operator could not reconcile the cluster (worst run %s)", lsOpDur(worst)),
		Detail: strings.Join(lines, "\n") + "\n\nThe record counts are the retry backoff, not the number of things that went wrong: " +
			"controller-runtime re-emits `Reconciler error` on every attempt." + tail,
		Advice: "While this repeats, nothing in the custom resource is being applied — users and grants are not reconciled, PITR settings are not pushed, and an edit to cr.yaml sits unapplied. The database itself may be entirely healthy: most of these are the operator failing to *reach* it, not the cluster failing.",
		At:     at, Until: until, Sources: lsSrcSet(evs), Events: lsEventNos(evs, 6),
	}}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// lsPXCOpFindingProbeRestart is the finding this whole catalogue exists for.
//
// A member cut off from the cluster shifts SYNCED → OPEN, which is Galera working
// correctly: it has no primary component, so it refuses queries and waits to rejoin. On
// Kubernetes it does not get to wait. The liveness probe asks wsrep whether the member is
// Primary, the answer is no, and kubelet kills the container — measured at 25.0 seconds
// after the shift, twice.
//
// What the member's own log then says is:
//
//	[System] [MY-013172] Received SHUTDOWN from user <via user signal>
//
// which is exactly what a deliberate `systemctl stop` writes. Read alone, that record
// takes this package's own Galera verdict to "a member left the cluster cleanly" — a
// reassuring sentence about a member that was killed. The pairing is what separates them,
// and neither log states it: the operator's says nothing at all, and the reason lives in a
// Kubernetes Event, which is not a log file anybody tails.
func lsPXCOpFindingProbeRestart(b *lsBundle) []lsFinding {
	var hits []lsEvent
	for i, e := range b.Events {
		if e.Class != lsClassShutdown || e.State == "" && !strings.Contains(strings.ToLower(e.Message), "shutdown") {
			continue
		}
		if !strings.Contains(e.Message, "Received SHUTDOWN") && !strings.Contains(e.Message, "shutdown signal") {
			continue
		}
		// Was this member outside a primary component in the minute before?
		for j := i - 1; j >= 0; j-- {
			p := b.Events[j]
			if p.Src != e.Src {
				continue
			}
			if e.TS-p.TS > 60 {
				break
			}
			if p.State == lsStateOpen || p.Primary == "no" {
				hits = append(hits, e)
				break
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	names := map[string]bool{}
	for _, e := range hits {
		names[lsNode(b, e.Src)] = true
	}
	who := make([]string, 0, len(names))
	for n := range names {
		who = append(who, n)
	}
	sort.Strings(who)
	return []lsFinding{{
		ID: "pxcop-probe-restart", Sev: lsSevBad,
		Title: "A member was shut down while it had no primary component — on Kubernetes that is the liveness probe, not an operator",
		Detail: strings.Join(who, ", ") + " left the primary component and then received a shutdown signal within a minute, with nothing in the operator's log about it. " +
			"A PXC pod's liveness probe asks wsrep whether the member is Primary; a member on the wrong side of a partition is not, so the probe fails and kubelet kills the container. " +
			"The member's own log records that as `Received SHUTDOWN from user <via user signal>`, which is identical to what a deliberate stop writes — so read on its own it looks like maintenance.",
		Advice:  "`kubectl -n <ns> get events` is the only place the reason is written: `Container pxc failed liveness probe, will be restarted`. If this is happening during ordinary network events rather than real failures, raise `spec.pxc.livenessProbe.failureThreshold` or `timeoutSeconds` — a member left alone would have rejoined by itself, and a killed one has to be rebuilt.",
		At:      hits[0].TS,
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 8),
	}}
}

// lsPXCOpFindingSilentOnMembers is the honest note, and it is the largest in this package.
//
// The operator log is where people look first, because it is the one thing a Kubernetes
// operator obviously owns. It is not where an outage is. Force-deleting a member under
// write load produced, in the two survivors' logs, five seconds of reconnect attempts, an
// evs.suspect_timeout, an eviction, a view change, a rejoin and a state transfer — and in
// the operator's log, over the same two minutes, four `Updated PITR timelines` records and
// nothing else.
func lsPXCOpFindingSilentOnMembers(b *lsBundle) []lsFinding {
	if !lsHasOperatorLog(b) {
		return nil
	}
	// Something that mattered, in a member's log.
	incidents := lsPick(b, func(e lsEvent) bool {
		switch e.Class {
		case lsClassMember, lsClassCrash, lsClassTransfer:
			return e.Sev == lsSevBad || e.Sev == lsSevWarn
		}
		return e.State == lsStateOpen
	})
	if len(incidents) == 0 {
		return nil
	}
	// Did the operator say anything about it? Anything at all in its own log inside the
	// window, other than its two heartbeats.
	quiet := 0
	for _, inc := range incidents {
		spoke := false
		for _, e := range b.Events {
			if e.Src == inc.Src || e.TS < inc.TS-30 || e.TS > inc.TS+120 {
				continue
			}
			if !lsIsOperatorSrc(b, e.Src) {
				continue
			}
			switch e.Class {
			case lsClassPITR, lsClassSecurity, lsClassStartup, lsClassOther:
				continue // the heartbeats: PITR timelines, grants, controller wiring
			}
			spoke = true
			break
		}
		if !spoke {
			quiet++
		}
	}
	if quiet == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "pxcop-operator-silent", Sev: lsSevWarn,
		Title: fmt.Sprintf("The operator's log says nothing about %d of the %d incident(s) in the members' logs", quiet, len(incidents)),
		Detail: "The operator reconciles a desired state; it does not watch the cluster. A member evicted, restarted, rejoining by state transfer, or sitting outside the primary component is not something it reports — its log during such a window is `Updated PITR timelines` and its user-grant housekeeping. " +
			"Reading the operator's log to find out whether the database is healthy will therefore reassure you.",
		Advice:  "The member logs are the ones that answer it, and this page has them on the same timeline. For the Kubernetes half — probe failures, evictions, image pulls, scheduling — `kubectl get events` is the third source, and it is not a log file.",
		Sources: lsOpSources(b, lsFlavourPXCOperator),
	}}
}

func lsIsOperatorSrc(b *lsBundle, idx int) bool {
	for _, s := range b.Sources {
		if s.Idx == idx {
			return s.Engine == pktEngineOperator
		}
	}
	return false
}

// lsPXCOpFindingSettings is the configuration report: what the cluster is running with,
// where that came from, and what to change.
//
// It fires on any Galera source, operator-managed or not — the provider configuration is
// read from the member's own log either way — but its advice is written for a cluster
// whose settings come from cr.yaml, because that is the case where the numbers are
// invisible everywhere else.
func lsPXCOpFindingSettings(b *lsBundle) []lsFinding {
	var cfg *lsPXCConfig
	var src int
	for _, s := range b.Sources {
		if s.PXCCfg != nil && s.PXCCfg.Provider != nil {
			cfg, src = s.PXCCfg, s.Idx
			break
		}
	}
	if cfg == nil {
		return nil
	}
	ist, sst := 0, 0
	for _, e := range b.Events {
		if e.Class != lsClassTransfer {
			continue
		}
		if strings.Contains(e.Label, "IST") || strings.Contains(e.Message, "IST received") {
			ist++
		}
		if strings.Contains(e.Label, "SST") || strings.Contains(e.Message, "SST received") {
			sst++
		}
	}
	tips := lsPXCAdvice(cfg, ist, sst)
	if len(tips) == 0 {
		return nil
	}
	sev := lsSevInfo
	var lines []string
	for _, t := range tips {
		sev = lsWorse(sev, t.Sev)
		lines = append(lines, fmt.Sprintf("%s = %s → %s. %s", t.Key, t.Is, t.Want, t.Why))
	}
	ver := ""
	if cfg.Version != "" {
		ver = " (PXC " + cfg.Version + ")"
	}
	return []lsFinding{{
		ID: "pxcop-settings", Sev: sev,
		Title: "What this cluster is actually configured with" + ver,
		Detail: "Read from the member's own `Passing config to GCS` line, which is the effective wsrep provider configuration after every default and override has been resolved. " +
			"On a cluster deployed from the operator's cr.yaml this is the only place any of these numbers appears: the shipped file has no `spec.pxc.configuration` at all, so nothing in Kubernetes shows them.\n\n" +
			strings.Join(lines, "\n\n"),
		Advice:  "Every one of these is set through `spec.pxc.configuration` in the custom resource — a `[mysqld]` section, applied by the operator through a ConfigMap. Changing it triggers a smart update: the operator restarts every member, secondaries first and the primary last, and each rejoins by state transfer. So change several at once rather than one at a time.",
		Sources: []int{src},
	}}
}
