package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestObjectsSurvivesAReadProxy is the integration half of the hostgroup fix, and it
// is skipped unless STOCKSIM_TEST_DSN points at a server. Point it at ProxySQL in
// front of a cluster (read/write split on, so plain SELECTs go to the reader
// hostgroup) and it reproduces the original failure if the fix is removed.
//
// What went wrong: Objects() issued SET SESSION information_schema_stats_expiry = 0
// and then read information_schema. ProxySQL does not track that variable, so it
// pinned the session to the writer and refused the SELECT with error 9006. Worse, the
// pin survived the connection going back to the pool, so every call left one more
// connection that would fail for whoever borrowed it next — which is why the visible
// errors were in the event feed and the working-set sampler.
//
// Calling repeatedly is the point: one call could pass by luck, and the old bug was
// cumulative.
func TestObjectsSurvivesAReadProxy(t *testing.T) {
	dsn := os.Getenv("STOCKSIM_TEST_DSN")
	if dsn == "" {
		t.Skip("set STOCKSIM_TEST_DSN to run this against a real server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	st, err := Open(ctx, Config{Engine: EngineMySQL, DSN: dsn, Database: "stocksim"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The pool opens with no default schema, so this is how the app points it at
	// one. Idempotent — CREATE ... IF NOT EXISTS throughout.
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	for i := 1; i <= 6; i++ {
		objs, err := st.Objects(ctx)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(objs) == 0 {
			t.Fatalf("call %d returned no tables", i)
		}
	}
	// And a plain read afterwards, which is what actually broke: it must not be
	// handed a connection some earlier Objects() call pinned to the writer.
	if _, _, err := st.ListSecurities(ctx, ListQuery{Limit: 1}); err != nil {
		t.Fatalf("a read after Objects(): %v", err)
	}
}
