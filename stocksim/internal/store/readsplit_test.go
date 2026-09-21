package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
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

// The write pool must follow the primary, and the follower only exists when the DSN says where
// else to look. A single-host DSN has nowhere to go, so starting a goroutine to notice would be
// pure churn — and on a deployment deliberately pointed at a standby it would never stop.
func TestPgFollowPrimaryOnlyWithFallbacks(t *testing.T) {
	multi := "postgres://u:p@a.example.net:5432,b.example.net:5432,c.example.net:5432/db?target_session_attrs=read-write"
	cc, err := pgx.ParseConfig(multi)
	if err != nil {
		t.Fatalf("parse multi-host dsn: %v", err)
	}
	// pgx turns the extra hosts into Fallbacks and uses them on every new connection — but it
	// ALSO uses fallbacks for TLS negotiation, so their count is not the number of servers.
	// pgDistinctHosts is what openPostgres branches on, and this is why.
	if got := pgDistinctHosts(cc); got != 3 {
		t.Errorf("want 3 distinct hosts from a 3-host DSN, got %d (fallbacks: %d)", got, len(cc.Fallbacks))
	}
	if cc.Host != "a.example.net" {
		t.Errorf("first host = %q", cc.Host)
	}
	// And the attribute that makes it pick the writer rather than the first host that answers.
	if cc.ValidateConnect == nil {
		t.Error("target_session_attrs=read-write must install a connection validator")
	}

	single, err := pgx.ParseConfig("postgres://u:p@only.example.net:5432/db")
	if err != nil {
		t.Fatalf("parse single dsn: %v", err)
	}
	// The case the follower must NOT start for. Note it still has a fallback — sslmode
	// negotiation — which is exactly the trap pgDistinctHosts exists to avoid.
	if got := pgDistinctHosts(single); got != 1 {
		t.Errorf("a single-host DSN must have 1 distinct host, got %d", got)
	}
	prefer, err := pgx.ParseConfig("postgres://u:p@only.example.net:5432/db?sslmode=prefer")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := pgDistinctHosts(prefer); got != 1 {
		t.Errorf("sslmode=prefer must not look like a second server, got %d hosts (fallbacks: %d)",
			got, len(prefer.Fallbacks))
	}
}
