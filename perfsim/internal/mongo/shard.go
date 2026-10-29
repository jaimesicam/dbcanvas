package mongo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// A sharded cluster is reached through mongos, and its members are the shards'
// mongods: several replica sets, each with its own primary. Lag is measured per
// shard, with one heartbeat document per shard. The heartbeat collection is
// sharded on a numeric _id and each shard owns one value through a zone, so a
// write through mongos lands on that shard's primary and replicates only
// within that shard.

// setupShardedHeartbeat shards perfsim.heartbeat and pins one _id to each shard.
// It returns each shard's replica set name with the _id it owns.
func (e *Engine) setupShardedHeartbeat(ctx context.Context) (map[string]int, error) {
	admin := e.writer.Database("admin")
	var res struct {
		Shards []struct {
			ID   string `bson:"_id"`
			Host string `bson:"host"`
		} `bson:"shards"`
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "listShards", Value: 1}}).Decode(&res); err != nil {
		return nil, fmt.Errorf("listShards: %w", err)
	}
	sort.Slice(res.Shards, func(i, j int) bool { return res.Shards[i].ID < res.Shards[j].ID })
	run := func(what string, cmd bson.D) {
		if err := admin.RunCommand(ctx, cmd).Err(); err != nil && !alreadyDone(err) {
			log.Printf("perfsim: %s: %v", what, err)
		}
	}
	run("enableSharding", bson.D{{Key: "enableSharding", Value: database}})
	ns := database + ".heartbeat"
	run("shardCollection", bson.D{{Key: "shardCollection", Value: ns}, {Key: "key", Value: bson.D{{Key: "_id", Value: 1}}}})
	idx := map[string]int{}
	for i, s := range res.Shards {
		// "rs-name/host:port,host:port", or just the host for a shard that is one
		// server; the replica set name is what its members report in hello.
		set := s.ID
		if name, _, ok := strings.Cut(s.Host, "/"); ok {
			set = name
		}
		idx[set] = i
		zone := "perfsim-" + s.ID
		if i > 0 {
			run("split", bson.D{{Key: "split", Value: ns}, {Key: "middle", Value: bson.D{{Key: "_id", Value: i}}}})
		}
		run("addShardToZone", bson.D{{Key: "addShardToZone", Value: s.ID}, {Key: "zone", Value: zone}})
		run("updateZoneKeyRange", bson.D{{Key: "updateZoneKeyRange", Value: ns},
			{Key: "min", Value: bson.D{{Key: "_id", Value: i}}}, {Key: "max", Value: bson.D{{Key: "_id", Value: i + 1}}}, {Key: "zone", Value: zone}})
		// The zone is what keeps the document there: when this move fails (a
		// busy or slow shard), the balancer makes it on one of its next rounds,
		// and that shard's lag reads "no heartbeat yet" until it has.
		run("moveChunk (the balancer will retry it)", bson.D{{Key: "moveChunk", Value: ns}, {Key: "find", Value: bson.D{{Key: "_id", Value: i}}}, {Key: "to", Value: s.ID}})
	}
	return idx, nil
}

// alreadyDone is an error that means the cluster is already as asked.
func alreadyDone(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"already", "is the same", "that is the same", "split point", "not split"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	var ce mongo.CommandError
	return asCommandError(err, &ce) && (ce.Code == 23 || ce.Code == 20)
}

// hbID is the heartbeat document a member of this replica set reads: its shard's
// on a sharded cluster, the one document otherwise.
func (e *Engine) hbID(set string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.topology != "sharded" {
		return 1
	}
	if i, ok := e.hbIdx[set]; ok {
		return i
	}
	return -1
}

// setPrimaryClient is a client on the primary of a member's replica set, where
// that set's configuration can be read and changed.
func (e *Engine) setPrimaryClient(set string) (*mongo.Client, string) {
	e.mu.Lock()
	id := e.setPrimary[set]
	e.mu.Unlock()
	if id == "" {
		return nil, ""
	}
	return e.mc[id], id
}

// setsOf groups member ids by the replica set they belong to.
func (e *Engine) setsOf(ids []string) map[string][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string][]string{}
	for _, id := range ids {
		out[e.memberSet[id]] = append(out[e.memberSet[id]], id)
	}
	return out
}

func asCommandError(err error, ce *mongo.CommandError) bool {
	return errors.As(err, ce)
}
