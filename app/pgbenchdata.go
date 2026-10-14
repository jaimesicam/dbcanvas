package main

import (
	"context"
	"fmt"
	"strconv"
)

// pgbenchdata.go — sample data for a PostgreSQL node or Patroni cluster, made by pgbench.
//
// A lab with no rows in it reproduces nothing, and a sample database (pagila, dvdrental) is one
// more download and one more schema to learn. pgbench is already on every node — it ships with
// the server — its four tables are what every PostgreSQL benchmark and bug report uses, and its
// size is one number: scale 1 is 100,000 pgbench_accounts rows, about 16 MB.
//
// So the design carries a scale (PGBenchScale), and the deploy runs `pgbench -i` into a database
// of its own, `pgbench`, after the server is up and before the first pgBackRest backup — so the
// backup has the data in it, and on a pg_tde node the tables are created encrypted (tde_heap is
// the default access method by then).

// pgBenchMaxScale caps what a deploy will load: 1000 is ~16 GB and 100 million rows, already
// far past what a lab container's disk and a deploy's patience are for.
const pgBenchMaxScale = 1000

// pgBenchIssues validates a design's scale.
func pgBenchIssues(label string, scale int) []issue {
	switch {
	case scale < 0 || scale > pgBenchMaxScale:
		return []issue{{Level: "error", Message: fmt.Sprintf("%s: pgbench scale must be between 0 (none) and %d", label, pgBenchMaxScale)}}
	case scale > 100:
		return []issue{{Level: "warning", Message: fmt.Sprintf("%s: pgbench scale %d is about %d GB and makes the deploy wait for it", label, scale, scale*16/1000)}}
	}
	return nil
}

// loadPGBench creates the pgbench database and initialises it at scale on a primary; a standby
// is skipped (the data streams to it). bindir is the server's bin directory.
func (a *App) loadPGBench(ctx context.Context, containerID, bindir string, scale int, logln func(string)) error {
	if scale <= 0 {
		return nil
	}
	env := []string{"BINDIR=" + bindir, "SCALE=" + strconv.Itoa(scale)}
	if err := a.runStep(ctx, containerID, pgBenchInitScript, env, logln); err != nil {
		return fmt.Errorf("pgbench -i: %w", err)
	}
	logln(fmt.Sprintf("pgbench data loaded into database pgbench (scale %d, %d accounts)", scale, scale*100000))
	return nil
}

// Env: BINDIR, SCALE. --quiet keeps the progress lines (one per 100k rows) out of the deploy log;
// pipefail, because the last command pipes into tail and a failed load must not read as success.
const pgBenchInitScript = `set -eo pipefail
PSQL="$BINDIR/psql"
if [ "$(runuser -u postgres -- "$PSQL" -qAtX -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "t" ]; then
  echo "standby — pgbench data arrives by replication"; exit 0
fi
[ "$(runuser -u postgres -- "$PSQL" -qAtX -c "SELECT 1 FROM pg_database WHERE datname = 'pgbench'")" = "1" ] ||
  runuser -u postgres -- "$PSQL" -qAtX -c "CREATE DATABASE pgbench"
runuser -u postgres -- "$BINDIR/pgbench" -i -s "$SCALE" --quiet pgbench 2>&1 | tail -3`
