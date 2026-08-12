package store

import (
	"context"
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

// MongoDB gets one of the three.
//
// Collections are the closest thing it has to tables and WiredTiger keeps a data
// handle open per collection, so thousands of them is a real and measurable
// pressure — the same shape of problem as table_open_cache, reached differently.
//
// The other two it cannot do honestly. Multi-document transactions need a replica
// set, and a Stock Market Sim node is as often pointed at a standalone mongod as
// at one, so an idle-transaction control would work on some deployments and not
// others with nothing on the canvas to say which. And an aggregation that spills
// is controlled by allowDiskUse rather than by a memory limit, which measures the
// flag rather than the server.
func (s *mongoStore) Capabilities() LabSupport {
	return LabSupport{ExtraTables: true}
}

func (s *mongoStore) HoldIdleTransaction(ctx context.Context, d time.Duration) error {
	return ErrUnsupported
}

func (s *mongoStore) RunTempTableQuery(ctx context.Context, mode string) (TempQueryResult, error) {
	return TempQueryResult{}, ErrUnsupported
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

// Valkey declines all three, for the same underlying reason it has no size
// target: it is a data structure server held in memory, not a query engine with
// a planner, a snapshot isolation level or a table cache. There is no
// transaction that holds a read view (MULTI/EXEC is an atomic batch, not a
// snapshot), no per-table handle to exhaust, and no intermediate result for a
// query to materialise.
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
