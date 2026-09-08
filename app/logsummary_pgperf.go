package main

// logsummary_pgperf.go — the PostgreSQL performance advisor, driven by what the server
// itself wrote down.
//
// Every other advisor in this package reads a *configuration* line and reasons about it:
// Galera prints its whole provider config, a mongod prints the cache it opened with. A
// PostgreSQL server prints neither. What it does instead is better — it reports the
// SYMPTOMS, in its own words, and for one of them it names the parameter to change:
//
//	LOG:  checkpoints are occurring too frequently (10 seconds apart)
//	HINT: Consider increasing the configuration parameter "max_wal_size".
//	LOG:  temporary file: path "base/pgsql_tmp/…", size 8192
//	LOG:  checkpoint complete: wrote 12 buffers (0.1%); … write=1.127 s, sync=0.028 s, total=1.199 s
//	LOG:  duration: 2409.738 ms  statement: copy pgbench_accounts from stdin …
//	LOG:  automatic vacuum of table "postgres.public.pgbench_branches": index scans: 1
//
// So the advice here is a *reading of evidence* rather than a lint of settings, and it can
// only speak about what the server was told to log. That gate is the first finding: on all
// three operators, `log_min_duration_statement`, `log_temp_files` and `log_lock_waits` are
// off by default, so the log is silent about slow queries, sorts that spilled and lock
// waits — and silence reads as health.
//
// **The measurements behind the numbers below.** Three clusters were deployed side by side
// on one host — Percona Operator for PostgreSQL and Crunchy PGO with three instances each,
// CloudNativePG with two — and driven with pgbench at identical scale and client counts,
// first on the operators' own defaults and then with the settings everybody reaches for
// first:
//
//	                    defaults              shared_buffers 1GB, max_wal_size 4GB,
//	                                          work_mem 16MB, effective_cache_size 3GB
//	Percona PG          2,150 tps / 7.43 ms   1,981 tps / 8.06 ms   −8%
//	Crunchy PGO         2,150 tps / 7.43 ms   2,185 tps / 7.31 ms   +1.6%
//	CloudNativePG       2,974 tps / 5.37 ms   3,006 tps / 5.31 ms   +1.1%
//
// All three ship the SAME PostgreSQL defaults — `shared_buffers=128MB`, `max_wal_size=1GB`,
// `work_mem=4MB` — and raising them did essentially nothing on this workload, and on one
// cluster made it worse. That is why nothing in this file tells anybody to raise
// shared_buffers because it is small. It tells them what the server complained about.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// lsPGPerf is what one server's log says about how it was performing.
type lsPGPerf struct {
	// Checkpoints.
	CheckpointsTooFrequent int     `json:"checkpointsTooFrequent,omitempty"`
	CheckpointGapSecs      float64 `json:"checkpointGapSecs,omitempty"` // the smallest interval it complained about
	Checkpoints            int     `json:"checkpoints,omitempty"`
	CheckpointSyncMax      float64 `json:"checkpointSyncMax,omitempty"` // worst sync= seconds
	CheckpointReqRatio     float64 `json:"checkpointReqRatio,omitempty"`
	CheckpointsRequested   int     `json:"checkpointsRequested,omitempty"`
	// Sorts and hashes that did not fit in work_mem.
	TempFiles int   `json:"tempFiles,omitempty"`
	TempBytes int64 `json:"tempBytes,omitempty"`
	TempMax   int64 `json:"tempMax,omitempty"`
	// Statements over log_min_duration_statement.
	SlowQueries int     `json:"slowQueries,omitempty"`
	SlowMaxMS   float64 `json:"slowMaxMs,omitempty"`
	SlowTotalMS float64 `json:"slowTotalMs,omitempty"`
	// Contention and maintenance.
	LockWaits   int `json:"lockWaits,omitempty"`
	Deadlocks   int `json:"deadlocks,omitempty"`
	Autovacuum  int `json:"autovacuum,omitempty"`
	Autoanalyze int `json:"autoanalyze,omitempty"`
	// What the server was told to record. Without these the counts above are not low —
	// they are unknown, and the difference matters more than any of the numbers.
	SawSlowLogging       bool `json:"sawSlowLogging,omitempty"`
	SawTempLogging       bool `json:"sawTempLogging,omitempty"`
	SawCheckpointLogging bool `json:"sawCheckpointLogging,omitempty"`
}

var (
	lsPGCkptFreq  = regexp.MustCompile(`checkpoints are occurring too frequently \((\d+) seconds? apart\)`)
	lsPGCkptDone  = regexp.MustCompile(`checkpoint complete:.*?write=([\d.]+) s, sync=([\d.]+) s, total=([\d.]+) s`)
	lsPGCkptStart = regexp.MustCompile(`^checkpoint starting: (.*)$`)
	lsPGTempFile  = regexp.MustCompile(`temporary file: path "[^"]*", size (\d+)`)
	lsPGDuration  = regexp.MustCompile(`^duration: ([\d.]+) ms`)
)

// lsPGScanPerf reads the performance evidence out of one PostgreSQL source's records.
//
// It runs over the RECORDS rather than the classified events on purpose: most of these are
// filed as background by the catalogue (a checkpoint is not news; ten thousand of them in
// an hour is), and an advisor that could only see what the event list kept would be
// reading a filtered copy of its own evidence.
func lsPGScanPerf(recs []lsRecord) *lsPGPerf {
	var p lsPGPerf
	for _, r := range recs {
		if r.Subsys == lsSubsysPatroni {
			continue // Patroni's own log says nothing about query performance
		}
		t := r.Text
		switch {
		case lsPGCkptFreq.MatchString(t):
			m := lsPGCkptFreq.FindStringSubmatch(t)
			p.CheckpointsTooFrequent++
			if n, err := strconv.ParseFloat(m[1], 64); err == nil {
				if p.CheckpointGapSecs == 0 || n < p.CheckpointGapSecs {
					p.CheckpointGapSecs = n
				}
			}
		case strings.HasPrefix(t, "checkpoint complete:"):
			p.Checkpoints++
			p.SawCheckpointLogging = true
			if m := lsPGCkptDone.FindStringSubmatch(t); m != nil {
				if s, err := strconv.ParseFloat(m[2], 64); err == nil && s > p.CheckpointSyncMax {
					p.CheckpointSyncMax = s
				}
			}
		case lsPGCkptStart.MatchString(t):
			p.SawCheckpointLogging = true
			// "checkpoint starting: wal" is a checkpoint forced by max_wal_size rather
			// than by checkpoint_timeout, and the ratio of the two is the whole diagnosis.
			if m := lsPGCkptStart.FindStringSubmatch(t); m != nil && strings.Contains(m[1], "wal") {
				p.CheckpointsRequested++
			}
		case strings.HasPrefix(t, "temporary file: path"):
			p.TempFiles++
			p.SawTempLogging = true
			if m := lsPGTempFile.FindStringSubmatch(t); m != nil {
				if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					p.TempBytes += n
					if n > p.TempMax {
						p.TempMax = n
					}
				}
			}
		case lsPGDuration.MatchString(t):
			p.SlowQueries++
			p.SawSlowLogging = true
			if m := lsPGDuration.FindStringSubmatch(t); m != nil {
				if ms, err := strconv.ParseFloat(m[1], 64); err == nil {
					p.SlowTotalMS += ms
					if ms > p.SlowMaxMS {
						p.SlowMaxMS = ms
					}
				}
			}
		case strings.Contains(t, "still waiting for") && strings.Contains(t, "Lock"):
			p.LockWaits++
		case strings.Contains(t, "deadlock detected"):
			p.Deadlocks++
		case strings.HasPrefix(t, "automatic vacuum of table"):
			p.Autovacuum++
		case strings.HasPrefix(t, "automatic analyze of table"):
			p.Autoanalyze++
		}
	}
	if p == (lsPGPerf{}) {
		return nil
	}
	if p.Checkpoints > 0 {
		p.CheckpointReqRatio = float64(p.CheckpointsRequested) / float64(p.Checkpoints)
	}
	return &p
}

// lsPGPerfAdvice turns that evidence into advice, worst first.
//
// Every rule here fires on something the server SAID. There is deliberately no rule of the
// form "setting X looks small": the measurement at the top of this file is why — the same
// three clusters, the same workload, and raising the obvious settings moved throughput by
// between −8% and +1.6%.
func lsPGPerfAdvice(p *lsPGPerf, window float64) []lsPXCTip {
	if p == nil {
		return nil
	}
	var out []lsPXCTip

	// 1. The one PostgreSQL diagnoses itself, and names the parameter.
	if p.CheckpointsTooFrequent > 0 {
		out = append(out, lsPXCTip{
			Key: "max_wal_size", Is: fmt.Sprintf("too small — %d complaint(s), the closest %s apart",
				p.CheckpointsTooFrequent, lsOpDur(p.CheckpointGapSecs)), Sev: lsSevWarn,
			Want: "raise it until the complaints stop — PostgreSQL prints the parameter to change in its own HINT, and it is the only advice in this file the server gives you itself",
			Why: "A checkpoint forced by running out of WAL space writes every dirty buffer at once instead of spreading the work over `checkpoint_timeout`, and it re-writes each page's full image to WAL the first time it is touched afterwards. " +
				"The cost lands on your writes, not on the checkpointer. This is the cheapest real win in PostgreSQL tuning and the server asks for it by name.",
		})
	}
	// 2. Checkpoints forced by WAL rather than by time, even without the complaint.
	if p.Checkpoints >= 5 && p.CheckpointReqRatio > 0.5 && p.CheckpointsTooFrequent == 0 {
		out = append(out, lsPXCTip{
			Key: "checkpoint cause", Is: fmt.Sprintf("%.0f%% of %d checkpoints were forced by WAL volume, not by time",
				p.CheckpointReqRatio*100, p.Checkpoints), Sev: lsSevWarn,
			Want: "raise `max_wal_size` so time, not volume, decides when a checkpoint happens",
			Why:  "`checkpoint starting: wal` means the server hit `max_wal_size`; `checkpoint starting: time` means it reached `checkpoint_timeout`. A cluster where most checkpoints are the first kind is checkpointing as fast as it writes.",
		})
	}
	// 3. The slow half of a checkpoint is the one that stalls commits.
	if p.CheckpointSyncMax >= 1 {
		out = append(out, lsPXCTip{
			Key: "checkpoint sync time", Is: fmt.Sprintf("worst sync=%.1fs", p.CheckpointSyncMax), Sev: lsSevWarn,
			Want: "spread the write with `checkpoint_completion_target` (0.9), and look at the volume underneath before touching anything else",
			Why:  "`write=` is the checkpointer dirtying pages, which is paced. `sync=` is fsync waiting on the storage, which is not — while it runs, commits wait too. A sync time in whole seconds is a storage answer, not a PostgreSQL one.",
		})
	}
	// 4. work_mem, from the sorts that did not fit.
	if p.TempFiles > 0 {
		out = append(out, lsPXCTip{
			Key: "work_mem", Is: fmt.Sprintf("%d sort(s)/hash(es) spilled to disk, %s in total, largest %s",
				p.TempFiles, lsPGBytes(p.TempBytes), lsPGBytes(p.TempMax)), Sev: lsSevWarn,
			Want: "raise `work_mem` toward the largest of these — but per connection, not globally: it is allocated per sort, so `work_mem × connections × sorts` is the real ceiling",
			Why:  "A query whose sort or hash does not fit writes it to `base/pgsql_tmp` and reads it back. The log gives you the exact size it needed, which is a better number than any rule of thumb — and a single query with a bad plan can produce these on a perfectly well-sized server, so read the statements beside them before changing anything.",
		})
	}
	// 5. Slow statements.
	if p.SlowQueries > 0 {
		share := ""
		if window > 0 {
			share = fmt.Sprintf(" — %.1f%% of the window was spent inside them", p.SlowTotalMS/10/window)
		}
		out = append(out, lsPXCTip{
			Key: "slow statements", Is: fmt.Sprintf("%d over the threshold, worst %.0f ms", p.SlowQueries, p.SlowMaxMS),
			Sev:  lsSevWarn,
			Want: "read them in the event list — they are recorded with their full text",
			Why: "`log_min_duration_statement` is the cheapest observability PostgreSQL has and the only one that names the statement." + share +
				" For the shape of the problem rather than the instances, `pg_stat_statements` is the companion and it is not enabled by default on any of the three operators.",
		})
	}
	// 6. Contention.
	if p.LockWaits > 0 || p.Deadlocks > 0 {
		sev := lsSevWarn
		if p.Deadlocks > 0 {
			sev = lsSevBad
		}
		out = append(out, lsPXCTip{
			Key: "lock contention", Is: fmt.Sprintf("%d lock wait(s), %d deadlock(s)", p.LockWaits, p.Deadlocks),
			Sev: sev, Want: "read the waiting and blocking statements in the records — both are named",
			Why: "A lock wait is logged only after `deadlock_timeout` (1s by default), so every one of these is a session that waited at least that long. A deadlock is worse than slow: one transaction was aborted and the client saw an error.",
		})
	}
	// 7. The gate, and it comes last because it is about everything above it.
	var off []string
	if !p.SawSlowLogging {
		off = append(off, "`log_min_duration_statement` (slow statements)")
	}
	if !p.SawTempLogging {
		off = append(off, "`log_temp_files` (sorts that spilled to disk)")
	}
	if !p.SawCheckpointLogging {
		off = append(off, "`log_checkpoints` (checkpoint frequency and sync time)")
	}
	if len(off) > 0 {
		out = append(out, lsPXCTip{
			Key: "what this log was allowed to record", Is: "no records from " + strings.Join(off, ", "),
			Sev:  lsSevInfo,
			Want: "turn them on before you need them — `log_min_duration_statement` at a few hundred ms, `log_temp_files: 0`, `log_lock_waits: on`, `log_checkpoints: on`",
			Why: "All three operators ship with these off or unset, so the absence of slow queries, temp files and lock waits in this window is not evidence that there were none — it is evidence that nobody asked. " +
				"That is the difference between a quiet log and a healthy server, and it is the one thing on this page you can fix before the next incident rather than during it.",
		})
	}
	return out
}

// lsPGBytes renders a byte count the way a DBA reads one.
func lsPGBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
