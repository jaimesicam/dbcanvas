package sim

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// verifyAggregateSample runs the v1 driver's only path to an aggregation
// explain — there is no Collection.Explain helper, so the raw "explain" command
// wraps the pipeline directly — and updates the query sample with what actually
// happened server-side: how many shards the winning plan touched, and the
// examined/returned document ratio. Only called at Profile.ExplainRate: explaining
// every query would double the load and distort the very throughput numbers this
// panel exists to explain.
func (e *Engine) verifyAggregateSample(ctx context.Context, coll string, pipeline mongo.Pipeline) (shardsTouched int, docsExamined, nReturned int64, ok bool) {
	var out bson.M
	cmd := bson.D{
		{Key: "explain", Value: bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.D{}}}},
		{Key: "verbosity", Value: "executionStats"},
	}
	if err := e.Store.DB.RunCommand(ctx, cmd).Decode(&out); err != nil {
		return 0, 0, 0, false
	}
	if qp, ok := out["queryPlanner"].(bson.M); ok {
		if wp, ok := qp["winningPlan"].(bson.M); ok {
			if shards, ok := wp["shards"].(bson.A); ok {
				shardsTouched = len(shards)
			}
		}
	}
	if es, ok := out["executionStats"].(bson.M); ok {
		if v, ok := es["totalDocsExamined"].(int32); ok {
			docsExamined = int64(v)
		}
		if v, ok := es["nReturned"].(int32); ok {
			nReturned = int64(v)
		}
	}
	if shardsTouched == 0 {
		shardsTouched = 1 // unsharded / single-shard deployments still touch "one shard" conceptually
	}
	return shardsTouched, docsExamined, nReturned, true
}

// classifyFilter is the "static", always-on classification (free — no server
// round trip): does the filter carry an equality/$in on hotelId, the shard-key
// prefix? Used to pick a plain-English reason string for the query-education
// panel regardless of whether this particular sample was also verified.
func classifyFilter(hasHotelID bool) string {
	if hasHotelID {
		return "filter has equality/$in on hotelId (the shard-key prefix) -> routed to the owning shard(s) only"
	}
	return "filter has no hotelId -> not shard-key-prefixed -> broadcasts to every shard"
}
