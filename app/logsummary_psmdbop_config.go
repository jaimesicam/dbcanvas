package main

// logsummary_psmdbop_config.go — tuning a PSMDB cluster from what its logs say it is.
//
// The PXC side of this (logsummary_pxcop_config.go) had one gift: every Galera member
// prints its whole effective provider configuration on start, so the tuning advice is
// built on ninety numbers read straight out of the log. MongoDB gives less. A mongod
// prints the engine's real cache size and its command-line options, which
// logsummary_mongo_config.go already reads — everything else in
// `spec.replsets[].configuration` reaches the member as a config file and is echoed
// nowhere.
//
// So this file does two things: it leans on lsMongoConfig for the engine, and it reads the
// operator's own records for the settings that decide the recovery point, which are the
// ones nothing else can see. Where a number is asserted below, the measurement behind it is
// named — all of them from one live cluster (PSMDB operator 1.23.0, percona-server-mongodb
// 8.0.26-11, PBM 2.15.0, three members on k3s v1.36.3) under continuous write load.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// lsPSMDBAdvice turns a bundle from an operator-managed replica set into advice.
//
// It takes the whole bundle rather than one source because half of what it advises about
// is not in any member's log: `oplogSpanMin` is only ever seen in the operator's
// `Setting pitr.oplogSpanMin` record, and how far behind the recoverable window runs is
// only in its `updating latest restorable time`.
func lsPSMDBAdvice(b *lsBundle, cfg *lsMongoConfig) []lsPXCTip {
	var out []lsPXCTip

	// 1. oplogSpanMin — the setting that IS your recovery point objective, and the one
	//    nobody looks at because it is not a MongoDB setting at all.
	span, sawSpan := lsPSMDBOplogSpan(b)
	switch {
	case sawSpan && span >= 10:
		out = append(out, lsPXCTip{
			Key: "spec.backup.pitr.oplogSpanMin", Is: fmt.Sprintf("%g minutes", span), Sev: lsSevWarn,
			Want: "1–5 minutes if you care about the recovery point; leave it high only if you have measured that the extra chunks cost you something",
			Why: "This is how often PBM closes an oplog chunk and uploads it, so it is your worst-case data loss almost exactly — the operator's `latestRestorableTime` trails the present by up to this much, by design. " +
				"10 minutes is the operator's default and it is a ten-minute RPO nobody chose. Measured on the corpus: at oplogSpanMin=1 the recoverable window tracked to within a minute.",
		})
	case sawSpan:
		out = append(out, lsPXCTip{
			Key: "spec.backup.pitr.oplogSpanMin", Is: fmt.Sprintf("%g minutes", span), Sev: lsSevOK,
			Want: "leave it",
			Why:  "The recoverable window trails the present by about this much, which is a recovery point somebody actually chose.",
		})
	}

	// 2. The horizon itself, when the operator recorded one. A number is worth more than
	//    advice about a setting.
	if latest, at, ok := lsPSMDBHorizon(b); ok {
		lag := at - latest
		sev := lsSevInfo
		if lag > 15*60 {
			sev = lsSevWarn
		}
		out = append(out, lsPXCTip{
			Key: "recoverable window", Is: "trailing by " + lsOpDur(lag) + " when last recorded", Sev: sev,
			Want: "alert on this number, not on whether PITR is 'enabled'",
			Why: "The operator writes `latestRestorableTime` on every reconcile and it is the only end-to-end proof that point-in-time recovery works: it moves when chunks are being uploaded and freezes when they are not. " +
				"Every other signal — the CR's status, `pitr.enabled`, `pbm status` — keeps reporting health through a stalled slicer.",
		})
	}

	// 3. The WiredTiger cache, from the engine's own line. The operator's default is
	//    MongoDB's own, which is a fraction of the HOST's memory and knows nothing about
	//    the pod's limit.
	if cfg != nil && cfg.CacheMB > 0 {
		switch {
		case !cfg.Pinned:
			out = append(out, lsPXCTip{
				Key: "storage.wiredTiger.engineConfig.cacheSizeGB",
				Is:  fmt.Sprintf("%.0f MB, derived (not pinned)", cfg.CacheMB), Sev: lsSevWarn,
				Want: "pin it, to about half of `spec.replsets[].resources.limits.memory` minus room for connections",
				Why: "WiredTiger sizes its cache from what it believes the machine has. In a container that is the HOST's memory unless the pod has a memory limit the runtime exposes — so three members on one node can each claim half the host and together promise several times what exists. " +
					"This package has the measurement from the non-Kubernetes side: three members each claiming a 14.5 GiB cache on a 29.4 GiB host ran a workload at 111 TPS with p95 710 ms, and the same workload with the caches pinned to 10/5/5 ran at 637 TPS with p95 71 ms.",
			})
		default:
			out = append(out, lsPXCTip{
				Key: "storage.wiredTiger.engineConfig.cacheSizeGB",
				Is:  fmt.Sprintf("%.0f MB, pinned", cfg.CacheMB), Sev: lsSevOK,
				Want: "leave it — but check it against the pod's memory limit rather than the host's",
				Why:  "A pinned cache is the setting somebody chose; the risk it removes is three members on one node each sizing themselves from the whole machine.",
			})
		}
	}

	// 4. The thing that has no setting, and is the difference between the two operators.
	out = append(out, lsPXCTip{
		Key: "restores are not fenced", Is: "how the operator works", Sev: lsSevInfo,
		Want: "scale the workload to zero, or take the Service away, before restoring",
		Why: "A logical PSMDB restore runs IN PLACE: the pods keep running and keep accepting connections while PBM drops the collections, re-creates them and replays the oplog on top. The PXC operator scales its cluster to zero and this cannot happen there. " +
			"Measured: a point-in-time restore that was exact to its target still ended with 32,000 documents that had never been in the backup, written by a client that was merely slow to shut down, during the seventeen minutes the replay took.",
	})

	// 5. And the one that decides how long that replay is.
	out = append(out, lsPXCTip{
		Key: "backup schedule vs. oplog length", Is: "whatever your schedule is", Sev: lsSevInfo,
		Want: "frequent full backups — the replay, not the dump, is what makes a restore slow",
		Why: "Measured on the corpus: restoring a dump of 414,500 documents took 2.3 seconds, and replaying roughly 92,000 oplog operations on top of it took 7 minutes 32 seconds — about 200 operations a second. " +
			"The cost of a point-in-time restore is set by how much was WRITTEN since the base backup, not by how big the dataset is, so a daily backup on a busy cluster is a very long recovery.",
	})
	return out
}

// lsPSMDBOplogSpan reads the chunk interval out of the operator's own record.
func lsPSMDBOplogSpan(b *lsBundle) (float64, bool) {
	for i := len(b.Events) - 1; i >= 0; i-- {
		e := b.Events[i]
		if !strings.HasPrefix(e.Label, "pitr.oplogSpanMin = ") {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimPrefix(e.Label, "pitr.oplogSpanMin = "), 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// lsPSMDBHorizon returns the last recoverable-window instant the operator recorded and when
// it recorded it, so the lag between them can be reported.
func lsPSMDBHorizon(b *lsBundle) (latest, at float64, ok bool) {
	for i := len(b.Events) - 1; i >= 0; i-- {
		e := b.Events[i]
		if !strings.HasPrefix(e.Label, "PITR can reach ") {
			continue
		}
		body := strings.TrimPrefix(e.Label, "PITR can reach ")
		if j := strings.Index(body, " (from "); j >= 0 {
			body = body[:j]
		}
		if ts, ok2 := lsPSMDBParseGoTime(body); ok2 {
			return ts, e.TS, true
		}
	}
	return 0, 0, false
}

// lsPSMDBParseGoTime reads the operator's field spelling, which is Go's default
// time.Time formatting rather than RFC3339: `2026-08-22 08:38:41 +0000 UTC`.
func lsPSMDBParseGoTime(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixNano()) / 1e9, true
		}
	}
	return 0, false
}
