package main

// logsummary_pgop_verdict.go — what the three PostgreSQL operators' logs add up to.
//
// The set is small on purpose. Most of what matters about an operator-managed PostgreSQL
// cluster is already said by the Patroni and PostgreSQL verdicts this package has had
// since the Patroni frames were added — "the cluster had no primary for 2.7s", "the cluster
// changed primary", "a member was rebuilt from the leader" all fire on a Percona, Crunchy
// or CloudNativePG cluster with no operator-specific code at all. What is added here is
// only what those cannot say.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var lsPGOpFindings = []func(*lsBundle) []lsFinding{
	lsPGOpFindingSilentFailover,
	lsCNPGFindingWALArchive,
	lsCNPGFindingSwitchover,
	lsPGOpFindingBackup,
	lsPGOpFindingPerformance,
	lsPGOpFindingRestore,
}

// lsIsPGOpSrc gates these to the sources they were written for, the way lsIsPXCOpSrc and
// lsIsPSMDBSrc do — three operators now share a bundle's worth of classes between them.
func lsIsPGOpSrc(b *lsBundle, src int) bool {
	switch {
	case lsSrcIs(b, src, lsFlavourPerconaPG), lsSrcIs(b, src, lsFlavourCrunchyPGO),
		lsSrcIs(b, src, lsFlavourCNPG), lsSrcIs(b, src, lsFlavourCNPGManager):
		return true
	}
	return false
}

func lsPGOpSources(b *lsBundle, flavours ...string) []int {
	var out []int
	for _, s := range b.Sources {
		for _, f := range flavours {
			if s.Flavour == f {
				out = append(out, s.Idx)
			}
		}
	}
	return out
}

// lsPGOpFindingSilentFailover is the finding this catalogue exists for, and it is a
// statement about an absence.
//
// Percona's operator and Crunchy's both delegate the database's availability to **Patroni**
// and never mention it. Killing the leader of each produced, in the following minute:
// seven copies of a Kubernetes API deprecation notice from Percona's, and fourteen
// "reconciled instance" records from Crunchy's. The whole story — the lost lock, the
// election, the new leader, the rejoin — is in the members' `database` containers.
//
// CloudNativePG is deliberately excluded: it runs the failover itself and does report it,
// which is what makes the contrast worth drawing.
func lsPGOpFindingSilentFailover(b *lsBundle) []lsFinding {
	ops := lsPGOpSources(b, lsFlavourPerconaPG, lsFlavourCrunchyPGO)
	if len(ops) == 0 {
		return nil
	}
	// Something the members recorded that the operator ought to have cared about.
	incidents := lsPick(b, func(e lsEvent) bool {
		if !lsSrcIs(b, e.Src, lsFlavourPatroni) && !lsSrcIs(b, e.Src, lsFlavourPGStream) {
			return false
		}
		return e.Sev == lsSevBad || e.Class == lsClassState && e.Sev == lsSevWarn
	})
	if len(incidents) == 0 {
		return nil
	}
	quiet := 0
	for _, inc := range incidents {
		spoke := false
		for _, e := range b.Events {
			if e.TS < inc.TS-30 || e.TS > inc.TS+120 || !lsIsPGOpSrc(b, e.Src) {
				continue
			}
			switch e.Class {
			case lsClassReconcile, lsClassStartup, lsClassOther, lsClassConfig:
				continue // the loop restating itself, and API deprecation notices
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
	names := map[string]bool{}
	for _, idx := range ops {
		names[lsNode(b, idx)] = true
	}
	who := make([]string, 0, len(names))
	for n := range names {
		who = append(who, n)
	}
	sort.Strings(who)
	return []lsFinding{{
		ID: "pgop-silent-failover", Sev: lsSevWarn,
		Title: fmt.Sprintf("The operator's log says nothing about %d of the %d incident(s) in the members' logs", quiet, len(incidents)),
		Detail: strings.Join(who, ", ") + " delegates the database's availability to Patroni, which runs inside each member's `database` container. " +
			"A lost leader lock, an election and a rejoin are Patroni's business and the operator never mentions them — measured on this corpus, killing the leader produced seven copies of a Kubernetes API deprecation notice from one operator and fourteen `reconciled instance` records from the other, and nothing else. " +
			"Reading the operator's log to find out whether the database is available will therefore reassure you.",
		Advice:  "The members' lanes on this page are the answer, and they are read by the same Patroni rules as a hand-built cluster. CloudNativePG is the exception among the three: it runs the failover itself and does date it.",
		Sources: ops,
	}}
}

// lsCNPGFindingWALArchive reports WAL archiving failing, which is silent everywhere else.
//
// Measured on the corpus: a CloudNativePG cluster whose `archive_command` failed on every
// segment went on serving, reported `Cluster in healthy state`, and only failed visibly
// when somebody asked it for a backup. Until then the sole evidence was in the instance
// manager's log.
func lsCNPGFindingWALArchive(b *lsBundle) []lsFinding {
	hits := lsPick(b, func(e lsEvent) bool {
		return lsIsPGOpSrc(b, e.Src) && strings.HasPrefix(e.Label, "WAL archiving is failing")
	})
	if len(hits) == 0 {
		return nil
	}
	span := hits[len(hits)-1].TS - hits[0].TS
	return []lsFinding{{
		ID: "cnpg-wal-archive", Sev: lsSevBad,
		Title: fmt.Sprintf("WAL archiving has been failing for %s", lsOpDur(span)),
		Detail: fmt.Sprintf("%d failed archive attempts between %s and %s. PostgreSQL keeps every segment it cannot archive, so the volume fills at the rate the cluster writes WAL, and point-in-time recovery has had nothing to work with for the whole of this window. "+
			"The cluster keeps serving throughout and its status stays healthy — the first thing that fails visibly is usually a backup.",
			len(hits), lsClock(hits[0].TS), lsClock(hits[len(hits)-1].TS)),
		Advice: "Check the object store the plugin or `barmanObjectStore` points at, and its credentials — in this corpus the cause was an S3 endpoint the pod could not verify. `SELECT * FROM pg_stat_archiver` gives the same answer from inside the database, and `pg_wal` filling is the consequence to watch for.",
		At:     hits[0].TS, Until: hits[len(hits)-1].TS,
		Sources: lsSrcSet(hits), Events: lsEventNos(hits, 4),
	}}
}

// lsCNPGFindingSwitchover measures the window in which no member was accepting writes.
//
// Only CloudNativePG can supply this: it is the operator that performs the switchover, so
// its log brackets it. On a Patroni-managed cluster the same measurement comes from the
// members' own logs and is made by the PostgreSQL verdicts.
func lsCNPGFindingSwitchover(b *lsBundle) []lsFinding {
	starts := lsPick(b, func(e lsEvent) bool { return e.Label == "Switchover or failover in progress" })
	if len(starts) == 0 {
		return nil
	}
	ends := lsPick(b, func(e lsEvent) bool { return e.Label == "Switchover completed" })
	first := starts[0]
	end, done := starts[len(starts)-1].TS, false
	for _, e := range ends {
		if e.TS >= first.TS {
			end, done = e.TS, true
			break
		}
	}
	title := "CloudNativePG moved the primary — no writes for " + lsOpDur(end-first.TS)
	sev := lsSevWarn
	if !done {
		title = "CloudNativePG began moving the primary and this log does not show it finishing"
		sev = lsSevBad
	}
	return []lsFinding{{
		ID: "cnpg-switchover", Sev: sev, Title: title,
		Detail: "CloudNativePG runs the failover itself rather than delegating to Patroni, so it brackets the event: from the first `switchover or failover in progress` to `Switchover completed`, nothing is accepting writes and the `-rw` Service has no endpoint. " +
			"Watch for `Detected ready WAL files in a former primary` beside this — a demoted primary holding unarchived WAL is a gap in point-in-time recovery that survives the failover.",
		At: first.TS, Until: end,
		Sources: lsSrcSet(starts), Events: lsEventNos(append(starts, ends...), 6),
	}}
}

// lsPGOpFindingBackup reports what the operators say about pgBackRest and barman.
func lsPGOpFindingBackup(b *lsBundle) []lsFinding {
	ok := lsPick(b, func(e lsEvent) bool { return lsIsPGOpSrc(b, e.Src) && e.Label == "Backup succeeded" })
	empty := lsPick(b, func(e lsEvent) bool {
		return lsIsPGOpSrc(b, e.Src) && e.Label == "pgBackRest reports no backups"
	})
	var out []lsFinding
	if len(ok) > 0 {
		last := ok[len(ok)-1]
		out = append(out, lsFinding{
			ID: "pgop-backup-ok", Sev: lsSevOK,
			Title:  fmt.Sprintf("%d backup(s) succeeded", len(ok)),
			Detail: "The most recent finished at " + lsClock(last.TS) + ". pgBackRest chooses full, differential or incremental from what is already in the repository, so an incremental here means it found a full to build on.",
			At:     last.TS, Sources: lsSrcSet(ok), Events: lsEventNos(ok, 4),
		})
	}
	if len(empty) > 0 {
		out = append(out, lsFinding{
			ID: "pgop-no-backups", Sev: lsSevWarn,
			Title:   "pgBackRest reports no backups in the repository",
			Detail:  "The operator asked pgBackRest what it had and was told nothing. Ordinary in the minutes before a new cluster's first backup, and a real fault afterwards: it is what an unreachable or misconfigured repository looks like, and until it changes there is nothing for a restore to start from.",
			Advice:  "`pgbackrest info` from inside a member's `pgbackrest` container is the same question asked directly, and its stanza-creation records say whether the repository was ever initialised.",
			At:      empty[0].TS,
			Sources: lsSrcSet(empty), Events: lsEventNos(empty, 3),
		})
	}
	return out
}

// lsPGOpFindingPerformance is the advisor, and it is per source rather than per bundle.
//
// Three members of one cluster do not share a performance story: only the primary takes
// the writes, so only the primary checkpoints hard and only the primary logs the slow
// statements. Averaging them would hide exactly the member you are looking for.
func lsPGOpFindingPerformance(b *lsBundle) []lsFinding {
	window := b.Summary.LastTS - b.Summary.FirstTS
	var out []lsFinding
	for _, s := range b.Sources {
		tips := lsPGPerfAdvice(s.PGPerf, window)
		if len(tips) == 0 {
			continue
		}
		sev := lsSevInfo
		var lines []string
		for _, t := range tips {
			sev = lsWorse(sev, t.Sev)
			lines = append(lines, fmt.Sprintf("%s — %s → %s. %s", t.Key, t.Is, t.Want, t.Why))
		}
		out = append(out, lsFinding{
			ID: "pgperf-" + strconv.Itoa(s.Idx), Sev: sev,
			Title: "How " + lsNode(b, s.Idx) + " was performing, from its own log",
			Detail: "PostgreSQL prints no configuration, so none of this is a lint of settings — it is what the server reported about itself.\n\n" +
				strings.Join(lines, "\n\n"),
			Advice: "Measured on three operator-managed clusters driven with pgbench at identical scale: all three ship the same PostgreSQL defaults (`shared_buffers=128MB`, `max_wal_size=1GB`, `work_mem=4MB`), and raising them moved throughput by between −8% and +1.6%. " +
				"That is why nothing here says to raise a setting because it looks small. Change what the server complained about, and measure.",
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsPGOpFindingRestore reports a restore, and says which of the three models it was —
// because they differ in the one way that matters to whoever is watching.
func lsPGOpFindingRestore(b *lsBundle) []lsFinding {
	inPlace := lsPick(b, func(e lsEvent) bool {
		return lsIsPGOpSrc(b, e.Src) && e.Label == "Restore in progress"
	})
	newCluster := lsPick(b, func(e lsEvent) bool {
		return lsIsPGOpSrc(b, e.Src) && e.Label == "Recovering into a new cluster"
	})
	done := lsPick(b, func(e lsEvent) bool {
		return lsIsPGOpSrc(b, e.Src) && e.Label == "Restore succeeded"
	})
	switch {
	case len(inPlace) > 0:
		first := inPlace[0]
		end := inPlace[len(inPlace)-1].TS
		sev, tail := lsSevBad, " This log does not show it finishing."
		if len(done) > 0 {
			end, sev, tail = done[len(done)-1].TS, lsSevWarn, ""
		}
		return []lsFinding{{
			ID: "pgop-restore", Sev: sev,
			Title: "A restore took the cluster down for " + lsOpDur(end-first.TS),
			Detail: "The Percona operator restores IN PLACE: every instance is stopped, the repository is restored onto one of them, and the others rebuild from it. There is no database between those two points." + tail +
				" Most of the elapsed time on a multi-instance cluster is the replicas rebuilding, not the restore — their own logs say which.",
			Advice: "A point-in-time restore starts a new timeline, and the backups from the old one are not usable as a base for it. Take a fresh full backup immediately; `failed to cleanup outdated backups` beside this is the operator saying it could not remove the superseded ones itself.",
			At:     first.TS, Until: end,
			Sources: lsSrcSet(inPlace), Events: lsEventNos(append(inPlace, done...), 6),
		}}
	case len(newCluster) > 0:
		return []lsFinding{{
			ID: "pgop-recovery-new", Sev: lsSevWarn,
			Title:   "A recovery created a new cluster beside the original",
			Detail:  "CloudNativePG does not restore in place. A recovery bootstraps a NEW Cluster from the object store while the original keeps running and serving — so nothing was rolled back, and there are now two clusters holding the same data from different moments.",
			Advice:  "This is the safest of the three models and the one that surprises people: the application is still pointed at the ORIGINAL. Switching to the recovered cluster is a deliberate act — move the Service or the connection string — and the original keeps accruing WAL until you delete it.",
			At:      newCluster[0].TS,
			Sources: lsSrcSet(newCluster), Events: lsEventNos(newCluster, 4),
		}}
	}
	return nil
}
