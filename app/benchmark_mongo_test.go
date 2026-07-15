package main

import (
	"math/rand"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// The Mongo op buckets must be registered so a run pre-allocates their latAcc — recStmt looks the
// bucket up by name without a nil check.
func TestMongoStmtBucketsRegistered(t *testing.T) {
	run := newBenchRun(&App{}, 1, benchConfig{}, "mongodb", "cid", "m", "admin", "pw")
	if run.driver != "mongo" {
		t.Errorf("engine mongodb should map to driver mongo, got %q", run.driver)
	}
	for _, typ := range []string{"point_find", "range_find", "insert", "update", "delete",
		"agg_q1", "agg_q2", "agg_q3", "agg_q4", "agg_q5"} {
		if run.stmts[typ] == nil {
			t.Errorf("stmt bucket %q not pre-created", typ)
		}
	}
}

// buildOrderDoc must produce a well-formed embedded-document order: an items array, the
// denormalized country/segment the OLAP aggregations read, and a total that equals the sum of the
// line amounts.
func TestMongoBuildOrderDoc(t *testing.T) {
	run := &benchRun{nCust: 100, nProd: 50}
	doc := run.buildOrderDoc(rand.New(rand.NewSource(7)), 42)

	if doc["_id"].(int64) != 42 {
		t.Errorf("_id: got %v, want 42", doc["_id"])
	}
	for _, k := range []string{"customer_id", "order_ts", "status", "total_amount", "cust_country", "cust_segment", "items"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("order doc missing %q", k)
		}
	}
	items, ok := doc["items"].(bson.A)
	if !ok || len(items) == 0 {
		t.Fatalf("items must be a non-empty array, got %T", doc["items"])
	}
	if doc["item_count"].(int) != len(items) {
		t.Errorf("item_count %v != len(items) %d", doc["item_count"], len(items))
	}
	var sum float64
	for _, it := range items {
		m := it.(bson.M)
		if _, ok := m["category"]; !ok {
			t.Error("each item must carry a denormalized category")
		}
		sum += m["line_amount"].(float64)
	}
	if got := doc["total_amount"].(float64); got < sum-1e-6 || got > sum+1e-6 {
		t.Errorf("total_amount %v != sum of line_amounts %v", got, sum)
	}
	// The whole document must marshal to BSON (all values are driver-encodable).
	if _, err := bson.Marshal(doc); err != nil {
		t.Fatalf("order doc does not marshal: %v", err)
	}
}

// A CRUD filter must be a non-empty subset of the plan's filter fields, taking values from the
// sampled tuple.
func TestMongoCrudFilterOf(t *testing.T) {
	p := &mongoCrudPlan{filter: []string{"_id", "email", "country"}}
	tuple := bson.M{"_id": int64(5), "email": "a@b.c", "country": "USA"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		f := p.filterOf(rng, tuple)
		if len(f) == 0 || len(f) > len(p.filter) {
			t.Fatalf("filter subset size out of range: %d", len(f))
		}
		for k, v := range f {
			if tuple[k] != v {
				t.Errorf("filter field %q value %v != tuple %v", k, v, tuple[k])
			}
		}
	}
}

// crudPickOp (shared with SQL) must honor the weights — a zero-weighted op never fires.
func TestMongoCrudWeightsHonored(t *testing.T) {
	run := &benchRun{cfg: benchConfig{Weights: benchWeights{Select: 1}}} // reads only
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		if op := run.crudPickOp(rng); op != "select" {
			t.Fatalf("with only Select weighted, got %q", op)
		}
	}
}
