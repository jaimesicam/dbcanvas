package mongo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"perfsim/internal/core"
)

// Table sets on MongoDB: a database per set, many small collections in it. Every
// collection is a WiredTiger table and every index another one, so a thousand of them is
// a thousand-plus files, handles and catalog entries; dropDatabase removes them in one
// command, the one-at-a-time drop one collection per command. See core/tablesets.go.
//
// Documents per collection are kept in the sim's own state rather than counted: dbStats
// on a set would walk every collection on every refresh.

const tsWorkers = 4

const tsStateKey = "tableset:"

func (e *Engine) TableSetWords() (string, string) { return "database", "collection" }

func (e *Engine) TableSets(ctx context.Context) ([]core.TableSet, error) {
	names, err := e.wr().ListDatabaseNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^perfsim_"}}}})
	if err != nil {
		return nil, err
	}
	saved, _ := e.Load(ctx, tsStateKey)
	var out []core.TableSet
	for _, n := range names {
		if !core.TableSetName.MatchString(n) {
			continue
		}
		cols, err := e.wr().Database(n).ListCollectionNames(ctx, bson.D{}, options.ListCollections().SetNameOnly(true))
		if err != nil {
			continue
		}
		per, _ := strconv.ParseInt(string(saved[tsStateKey+n]), 10, 64)
		out = append(out, core.TableSet{DB: n, Tables: len(cols), Rows: per * int64(len(cols))})
	}
	return out, nil
}

func (e *Engine) CreateTableSet(ctx context.Context, set string, tables, nrows int, done func(int)) error {
	if err := e.Save(ctx, tsStateKey+set, []byte(strconv.Itoa(nrows))); err != nil {
		return err
	}
	have := map[string]bool{}
	if names, err := e.SetTables(ctx, set); err == nil {
		for _, n := range names {
			have[n] = true
		}
	}
	var next, finished atomic.Int64
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for w := 0; w < tsWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wctx.Err() == nil {
				i := int(next.Add(1))
				if i > tables {
					return
				}
				name := core.SetTableName(i)
				if !have[name] {
					if err := e.createSetCollection(wctx, set, name, nrows); err != nil {
						once.Do(func() { firstErr = fmt.Errorf("%s: %w", name, err); cancel() })
						return
					}
				}
				done(int(finished.Add(1)))
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (e *Engine) createSetCollection(ctx context.Context, set, name string, nrows int) error {
	db := e.wr().Database(set)
	if err := db.CreateCollection(ctx, name); err != nil {
		var ce mongo.CommandError
		if !(errors.As(err, &ce) && ce.Code == 48) { // NamespaceExists
			return err
		}
	}
	const batch = 1000
	for lo := 1; lo <= nrows; lo += batch {
		hi := min(nrows, lo+batch-1)
		docs := make([]any, 0, hi-lo+1)
		for i := lo; i <= hi; i++ {
			docs = append(docs, bson.D{{Key: "_id", Value: i}, {Key: "v", Value: i % 1000}, {Key: "note", Value: "row " + strconv.Itoa(i)}})
		}
		if _, err := db.Collection(name).InsertMany(ctx, docs, options.InsertMany().SetOrdered(false)); err != nil && !mongo.IsDuplicateKeyError(err) {
			return err
		}
	}
	return nil
}

func (e *Engine) SetTables(ctx context.Context, set string) ([]string, error) {
	names, err := e.wr().Database(set).ListCollectionNames(ctx, bson.D{}, options.ListCollections().SetNameOnly(true))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func (e *Engine) DropSetTable(ctx context.Context, set, table string) error {
	return e.wr().Database(set).Collection(table).Drop(ctx)
}

func (e *Engine) DropTableSet(ctx context.Context, set string) error {
	if !core.TableSetName.MatchString(set) {
		return fmt.Errorf("%s is not a table set", set)
	}
	if err := e.wr().Database(set).Drop(ctx); err != nil {
		return err
	}
	e.Delete(ctx, tsStateKey+set)
	return nil
}

// The interface is optional to the hub, so a misspelt method would hide the section
// silently rather than fail to build. This makes it fail to build.
var _ core.TableSetter = (*Engine)(nil)
