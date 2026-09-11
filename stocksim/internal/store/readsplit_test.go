package store

import (
	"context"
	"database/sql"
	"testing"
)

// The routing rule has to be safe by default: a context nobody marked reads from the
// writer. Everything the simulation's agents do arrives that way, and so does anything
// added later that forgets this file exists.
func TestReplicaOKDefaultsToTheWriter(t *testing.T) {
	if ReplicaOK(context.Background()) {
		t.Error("an unmarked context asked for a replica")
	}
	if !ReplicaOK(WithReplicaOK(context.Background())) {
		t.Error("a marked context did not survive the round trip")
	}
}

// openIdle is a pool that is never used: sql.Open does not connect, which is what makes
// it a usable stand-in for "some *sql.DB" in a routing test.
func openIdle(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Two conditions have to hold together for a read to leave the writer: the deployment has
// a read endpoint, and this particular request only displays. Either one alone is the
// writer — the first because there is nowhere else to go, the second because the answer is
// about to decide a write.
func TestReadDBRoutesOnlyMarkedReadsToTheReplica(t *testing.T) {
	w, ro := openIdle(t, "postgres://w/db"), openIdle(t, "postgres://r/db")
	marked, plain := WithReplicaOK(context.Background()), context.Background()

	split := &pgStore{db: w, ro: ro}
	if got := split.readDB(marked); got != ro {
		t.Error("pg: a displayed read did not go to the read endpoint")
	}
	if got := split.readDB(plain); got != w {
		t.Error("pg: an unmarked read left the writer")
	}

	single := &pgStore{db: w}
	if got := single.readDB(marked); got != w {
		t.Error("pg: a deployment with no read endpoint sent a read somewhere else")
	}

	msplit := &mysqlStore{db: w, ro: ro}
	if got := msplit.readDB(marked); got != ro {
		t.Error("mysql: a displayed read did not go to the read endpoint")
	}
	if got := msplit.readDB(plain); got != w {
		t.Error("mysql: an unmarked read left the writer")
	}
	if got := (&mysqlStore{db: w}).readDB(marked); got != w {
		t.Error("mysql: a deployment with no read endpoint sent a read somewhere else")
	}
}
