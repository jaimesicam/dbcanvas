package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// mongoEODFilter matches the synthetic collections by name.
func mongoEODFilter() bson.M {
	return bson.M{"name": bson.M{"$regex": "^eod_summary_"}}
}

func mongoEmptyFilter() bson.M { return bson.M{} }

func mongoEODDoc(symbol string, close float64, volume int64) bson.M {
	return bson.M{"symbol": symbol, "closePrice": close, "volume": volume}
}

// MongoDB and Valkey decline most of the lab knobs, and each refusal has a
// reason worth stating rather than a control that quietly does nothing.

// ---- MongoDB ----

// MongoDB gets two of the six, and declines the other four for reasons worth
// stating rather than offering a control that quietly does nothing.
//
// It gets extra collections: collections are the closest thing it has to tables
// and WiredTiger keeps a data handle open per collection, so thousands of them
// is a real and measurable pressure — the same shape of problem as
// table_open_cache, reached differently. And it gets the scan, because a
// collection scan is exactly the same pathology as a table scan and MongoDB
// reports it honestly in explain's totalDocsExamined.
//
// The four it declines:
//
//   - Idle transaction. Multi-document transactions need a replica set, and a
//     Stock Market Sim node is as often pointed at a standalone mongod as at
//     one, so the control would work on some deployments and not others with
//     nothing on the canvas to say which.
//   - Temporary tables. An aggregation that spills is controlled by allowDiskUse
//     rather than by a memory limit, which measures the flag and not the server.
//   - Lock contention. WiredTiger uses optimistic concurrency with
//     document-level write conflicts, so what would be a wait on an engine with
//     row locks is a retry here — a different phenomenon that would be
//     misleading under the same label.
//   - Write pressure. A standalone mongod acknowledges writes from memory by
//     default and its journal flushes on an interval, so the number of writes
//     and the number of fsyncs are not related the way the knob assumes.
func (s *mongoStore) Capabilities() LabSupport {
	return LabSupport{ExtraTables: true, ScanQueries: true}
}

func (s *mongoStore) HoldIdleTransaction(ctx context.Context, d time.Duration) error {
	return ErrUnsupported
}

func (s *mongoStore) RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error) {
	return TempQueryResult{}, ErrUnsupported
}

func (s *mongoStore) EnsureLabTables(ctx context.Context) error { return nil }

func (s *mongoStore) RunContendedUpdate(ctx context.Context, mode string, worker int) (ContentionResult, error) {
	return ContentionResult{}, ErrUnsupported
}

func (s *mongoStore) RunWritePressure(ctx context.Context, mode string, n int, budget time.Duration) (WriteResult, error) {
	return WriteResult{}, ErrUnsupported
}

// RunScanQuery filters price ticks on a field with no index on it, so the
// server has to examine every document in the collection.
//
// Whether it really did is read from explain's executionStats: totalDocsExamined
// against nReturned is the same rows-read-versus-rows-returned gap the SQL
// engines report through their own counters, and COLLSCAN in the winning plan
// confirms there was no index to use. Running explain rather than the query
// itself is deliberate — with executionStats the query is genuinely executed,
// so this is the measurement of a real scan and not an estimate of a planned
// one.
func (s *mongoStore) RunScanQuery(ctx context.Context) (ScanResult, error) {
	lo, hi := scanRange()
	filter := bson.M{"volume": bson.M{"$gte": lo, "$lte": hi}}

	start := time.Now()
	var raw bson.M
	err := s.db.RunCommand(ctx, bson.D{
		{Key: "explain", Value: bson.D{
			{Key: "find", Value: "price_ticks"},
			{Key: "filter", Value: filter},
		}},
		{Key: "verbosity", Value: "executionStats"},
	}).Decode(&raw)
	if err != nil {
		return ScanResult{}, err
	}
	took := time.Since(start)

	returned, examined, collscan := mongoExplainStats(raw)
	return ScanResult{
		Rows: int(returned), RowsRead: examined, Scanned: collscan && examined > 0,
		Duration: took, Description: scanDescription(lo, hi),
	}, nil
}

// mongoExplainStats digs the three numbers that matter out of an explain
// document. The shape differs between a standalone and a sharded cluster, so
// this reads defensively and reports what it found rather than assuming a path
// exists.
func mongoExplainStats(raw bson.M) (returned, examined int64, collscan bool) {
	stats, _ := raw["executionStats"].(bson.M)
	if stats == nil {
		return 0, 0, false
	}
	returned = mongoNum(stats["nReturned"])
	examined = mongoNum(stats["totalDocsExamined"])
	// A COLLSCAN anywhere in the winning plan means at least one shard had no
	// index to use, which is the condition being asked for.
	if qp, ok := raw["queryPlanner"].(bson.M); ok {
		collscan = strings.Contains(fmt.Sprint(qp["winningPlan"]), "COLLSCAN")
	}
	return returned, examined, collscan
}

// mongoNum coerces the several numeric types BSON may decode a count into.
func mongoNum(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// EnsureExtraTables creates or drops synthetic collections. Mongo creates a
// collection implicitly on first write, so this inserts a document rather than
// issuing DDL, and drops by name to remove one.
func (s *mongoStore) EnsureExtraTables(ctx context.Context, n int) (int, error) {
	n = ClampExtraTables(n)
	names, err := s.db.ListCollectionNames(ctx, mongoEODFilter())
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, name := range names {
		have[name] = true
	}
	want := map[string]bool{}
	for _, name := range ExtraTableNames(n) {
		want[name] = true
	}
	for name := range have {
		if !want[name] {
			if err := s.db.Collection(name).Drop(ctx); err != nil {
				return len(have), err
			}
			delete(have, name)
		}
	}
	for _, name := range ExtraTableNames(n) {
		if have[name] {
			continue
		}
		if _, err := s.db.Collection(name).InsertMany(ctx, []any{
			mongoEODDoc("ACME", 100, 1000),
			mongoEODDoc("BRVO", 200, 2000),
			mongoEODDoc("CDLA", 50, 3000),
		}); err != nil {
			return len(have), err
		}
		have[name] = true
	}
	return len(have), nil
}

func (s *mongoStore) TouchExtraTables(ctx context.Context, names []string) error {
	for _, name := range names {
		if !strings.HasPrefix(name, "eod_summary_") {
			continue
		}
		// FindOne opens the collection's data handle, which is the point.
		s.db.Collection(name).FindOne(ctx, mongoEmptyFilter())
	}
	return nil
}

// ---- Valkey ----

// Valkey declines all six, for the same underlying reason it has no size
// target: it is a data structure server held in memory, not a query engine with
// a planner, a snapshot isolation level or a table cache. There is no
// transaction that holds a read view (MULTI/EXEC is an atomic batch, not a
// snapshot), no per-table handle to exhaust, and no intermediate result for a
// query to materialise.
//
// The later three fail on the same ground. A single-threaded command loop has
// no lock to contend for — commands are serialised, so writers queue by
// construction and there is nothing pathological to provoke. There is no index
// to be missing, because there is no query planner to miss it. And durability
// is an interval (appendfsync everysec) or a background rewrite, not a flush
// per write, so a commit storm would measure the configuration rather than the
// server.
func (s *valkeyStore) Capabilities() LabSupport { return LabSupport{} }

func (s *valkeyStore) HoldIdleTransaction(ctx context.Context, d time.Duration) error {
	return ErrUnsupported
}

func (s *valkeyStore) EnsureExtraTables(ctx context.Context, n int) (int, error) {
	return 0, ErrUnsupported
}

func (s *valkeyStore) TouchExtraTables(ctx context.Context, names []string) error {
	return ErrUnsupported
}

func (s *valkeyStore) RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error) {
	return TempQueryResult{}, ErrUnsupported
}

func (s *valkeyStore) EnsureLabTables(ctx context.Context) error { return nil }

func (s *valkeyStore) RunContendedUpdate(ctx context.Context, mode string, worker int) (ContentionResult, error) {
	return ContentionResult{}, ErrUnsupported
}

func (s *valkeyStore) RunScanQuery(ctx context.Context) (ScanResult, error) {
	return ScanResult{}, ErrUnsupported
}

func (s *valkeyStore) RunWritePressure(ctx context.Context, mode string, n int, budget time.Duration) (WriteResult, error) {
	return WriteResult{}, ErrUnsupported
}
