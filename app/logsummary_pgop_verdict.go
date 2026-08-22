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
	"strings"
)

var lsPGOpFindings = []func(*lsBundle) []lsFinding{
	lsPGOpFindingSilentFailover,
	lsCNPGFindingWALArchive,
	lsCNPGFindingSwitchover,
	lsPGOpFindingBackup,
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
