// Package store wraps the Valkey/Redis connection and key-naming scheme.
//
// Every key this app touches is namespaced under "ts:" (traffic-sim) — this is a hard
// rule, not a convention: labs match exact key names the *learner* creates during a
// Check Work step (lab:hello, tx:counter, app:config, scan:test:*, ...), and this app's
// own continuous traffic must never be mistaken for, or interfere with, that grading.
package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	StreamKey    = "ts:events"
	ConsumerGrp  = "ts:processors"
	NotifyChan   = "ts:notify"
	ControlState = "ts:control:state" // "running" | "paused"
	ControlLevel = "ts:control:level" // "off" | "low" | "medium" | "high"

	RankCongestion = "ts:rank:congestion"
	RankSlowest    = "ts:rank:slowest"
	RankIncidents  = "ts:rank:incidents"

	GeoVehicles  = "ts:geo:vehicles"
	GeoIncidents = "ts:geo:incidents"

	StatEventsTotal  = "ts:stats:events_total"
	StatVehiclesSeen = "ts:stats:vehicles_total"

	VehicleTTL = 30 * time.Second
	AgentTTL   = 15 * time.Second
)

func RoadKey(id string) string     { return "ts:road:" + id }
func VehicleKey(id string) string  { return "ts:vehicle:" + id }
func SignalKey(id string) string   { return "ts:signal:" + id }
func SensorKey(id string) string   { return "ts:sensor:" + id }
func IncidentKey(id string) string { return "ts:incident:" + id }
func AgentKey(id string) string    { return "ts:agent:" + id }

// Store holds the shared Valkey connection. redis.UniversalClient transparently
// selects a plain *redis.Client (one address) or *redis.ClusterClient (multiple
// addresses, auto-discovering the rest of the topology via CLUSTER SLOTS and
// following MOVED redirects on its own) behind one identical command surface — the
// rest of this app never needs to know or care which one it's talking to.
type Store struct {
	Client redis.UniversalClient
}

// New builds a Store from connection settings. addrs with len==1 connects standalone;
// len>1 connects in Cluster mode using those as seed nodes.
func New(addrs []string, password string) *Store {
	return &Store{Client: redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    addrs,
		Password: password,
	})}
}

func ParseAddrs(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

func (s *Store) Ping(ctx context.Context) error {
	return s.Client.Ping(ctx).Err()
}

// PublishEvent XADDs to the durable stream (the source of truth a reconnecting client
// replays from) and PUBLISHes the same payload for anyone currently connected — the
// live push is a convenience, never the only path a client can rely on.
func (s *Store) PublishEvent(ctx context.Context, kind, entity, roadID, agent, detail string) {
	fields := map[string]any{
		"kind": kind, "entity": entity, "roadId": roadID, "agent": agent,
		"detail": detail, "ts": time.Now().UTC().Format(time.RFC3339),
	}
	id, err := s.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey, MaxLen: 5000, Approx: true, Values: fields,
	}).Result()
	if err != nil {
		return
	}
	s.Client.Incr(ctx, StatEventsTotal)
	fields["id"] = id
	if payload, err := json.Marshal(fields); err == nil {
		s.Client.Publish(ctx, NotifyChan, string(payload))
	}
}

func (s *Store) Heartbeat(ctx context.Context, agentID, agentType, status string, events, errs int) {
	s.Client.HSet(ctx, AgentKey(agentID), map[string]any{
		"type": agentType, "status": status,
		"lastActivity": time.Now().UTC().Format(time.RFC3339),
		"events":       events, "errors": errs,
	})
	s.Client.Expire(ctx, AgentKey(agentID), AgentTTL)
}
