package main

// logsummary_mongo_shard.go — sharded clusters: the router, the config servers and the
// metadata that ties them together.
//
// A sharded cluster is three kinds of process and only one of them is a database. A shard
// member and a config-server member are ordinary mongods and the replica-set catalogue next
// door reads them unchanged. A mongos is not a mongod at all: it stores nothing, replicates
// nothing, and its log is about where things ARE rather than what they are doing.
//
// Which makes the router's log the most misleading file in the set, and the reason this
// file exists. Driving a whole shard down under live traffic produced two client-visible
// failures — a query that could not be answered and a write that was refused — and the
// mongos log recorded neither. Not a warning, not an error. What it recorded was the
// shard's topology changing, over and over, at INFO. The operator is handed a log that
// looks completely healthy for an outage the application saw plainly.
//
// So the rules here are mostly about reading routing state out of records that are not
// phrased as problems, and the verdict ends with the honest note that the rest is not
// there at all.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// lsFlavourMongos marks a source as a query router rather than a database.
//
// It matters beyond labelling: a mongos has no replica-set state, no oplog and no storage
// engine, so every replica-set finding has to be kept away from it. Without that, a router
// whose shards are all healthy gets reported as a member that never became primary.
const lsFlavourMongos = "mongos"

// lsSniffMongos decides whether a MongoDB log came from a router.
//
// Two signals, because either alone has a failure mode.
//
// The precise one is id 471693. mongod and mongos both log "Updating the shard registry
// with confirmed replica set" and they use DIFFERENT IDS for it — 471691 on a mongod,
// 471693 on a mongos — which makes one record enough to tell them apart. Verified on
// 6.0.29-23 and 7.0.39-21: 471693 appears only in mongos logs, 471691 only in mongod ones.
//
// The structural one covers an excerpt that happens not to contain it: a mongos never logs
// a REPL, STORAGE or WiredTiger record, because it has none of those subsystems. Requiring
// a positive sharding signal as well keeps a standalone mongod's connection log from being
// mistaken for a router.
func lsSniffMongos(recs []lsMongoRecord) bool {
	sharding := false
	for _, r := range recs {
		switch r.ID {
		case 471693:
			return true
		case 471691, 22068:
			// The mongod half of the same pair, and the shard-identity update that only a
			// data-bearing member writes. Both are proof this is NOT a router, and they
			// have to be checked: a shard member's log is full of SHARDING records, so the
			// structural test below would otherwise promote a mongod excerpt that happened
			// to contain no replication records to a mongos.
			return false
		}
		switch r.Comp {
		case "REPL", "REPL_HB", "ELECTION", "ROLLBACK", "INITSYNC", "STORAGE", "RECOVERY", "WT", "WTCHKPT", "INDEX":
			return false
		case "SHARDING", "SH_REFR":
			sharding = true
		}
	}
	return sharding
}

// lsMongosNodeName pulls the router's own name out of its log.
//
// Harder than for a member: a mongos has no "Found self in config" record, because it is
// not in one. The startup banner carries the host it is running on and that is all there is.
func lsMongosNodeName(recs []lsMongoRecord) string {
	for _, r := range recs {
		if r.ID == 4615611 || r.ID == 23403 {
			if h := lsMongoShortHost(r.str("host")); h != "" {
				return h
			}
		}
	}
	return ""
}

// lsShardRules is the sharded catalogue, merged into the replica-set one.
//
// It is keyed on ids like the rest, and the version sweep is why that matters here more
// than anywhere: between 6.0 and 7.0 MongoDB replaced the distributed lock manager with DDL
// coordinators, changed the wording of the shardIdentity warning from "--shardsvr" to
// "ShardServer role", and stopped auto-splitting chunks. Every one of those is a different
// message; not one is a different id.
var lsShardRules = []lsMongoRule{
	// ---- the config server's changelog -----------------------------------------------
	//
	// The single most valuable record in a sharded cluster. Every topology change the
	// cluster makes — a shard added or removed, a collection sharded, a chunk split or
	// moved, the balancer started or stopped — is written to config.changelog by whichever
	// config server is primary, and announced in the log first, under one id, with the
	// operation in attr.event.what.
	//
	// One rule therefore covers the entire sharding vocabulary, and it keeps covering it
	// when MongoDB adds an operation, because the operation is data rather than a rule.
	{ids: []int{22080}, class: lsClassConfig, sev: lsSevInfo, label: "Cluster metadata changed",
		means: "The config servers' own record of a change to the cluster's shape. Every shard added, collection sharded, chunk split or moved, and every balancer round, passes through here — and only through here, on whichever config server was primary at the time. If this log is not from that member, none of it is anywhere.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			raw, ok := r.Attr["event"]
			if !ok {
				return
			}
			var ev struct {
				What    string          `json:"what"`
				NS      string          `json:"ns"`
				Server  string          `json:"server"`
				Details json.RawMessage `json:"details"`
			}
			if json.Unmarshal(raw, &ev) != nil || ev.What == "" {
				return
			}
			e.Message = ev.What
			if ev.NS != "" {
				e.Message += " " + ev.NS
			}
			e.Label = lsShardEventLabel(ev.What)
			e.Peer = lsMongoShortHost(ev.Server)
			// The events that cost something get the severity, so a page full of splits
			// does not bury the one that removed a shard.
			switch {
			case strings.HasPrefix(ev.What, "removeShard"), strings.HasPrefix(ev.What, "dropCollection"),
				strings.HasPrefix(ev.What, "dropDatabase"), strings.HasSuffix(ev.What, ".error"):
				e.Sev = lsSevWarn
			case strings.HasPrefix(ev.What, "moveChunk"), strings.HasPrefix(ev.What, "addShard"),
				strings.HasPrefix(ev.What, "shardCollection"), strings.HasPrefix(ev.What, "balancer"):
				e.Sev = lsSevOK
			}
		}},

	// ---- what the router can see of the shards ----------------------------------------
	//
	// 4333213 is written by every process that monitors a replica set, mongos included, and
	// on a router it is the whole story: it is how the file records that a shard changed
	// shape. The topology description says which type each member is, so "no primary" is
	// readable from a router's log even though the router has no replica set of its own.
	{ids: []int{4333213}, class: lsClassMember, sev: lsSevInfo, label: "A monitored replica set changed shape",
		means: "This process watches every shard and the config servers, and re-reads their membership whenever one of them changes. On a router these are the only records of a shard's health there are — and they are written at INFO whether the set is fine or has no primary at all.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			set := r.str("replicaSet")
			raw, ok := r.Attr["newTopologyDescription"]
			if !ok {
				e.Message = set
				return
			}
			var topo struct {
				Type string `json:"topologyType"`
			}
			_ = json.Unmarshal(raw, &topo)
			e.Peer = set
			e.Message = set + ": " + topo.Type
			// ReplicaSetNoPrimary is the one that matters. A shard in that state takes no
			// writes, and the router says so only in this field of this record.
			if strings.Contains(topo.Type, "NoPrimary") {
				e.Sev = lsSevWarn
				e.Label = set + " has no primary"
				e.Meaning = "The router could not find a primary for this replica set. If it is a shard, every write routed to it fails while this lasts; if it is the config servers, the whole cluster's metadata is read-only and no chunk can move."
			} else {
				e.Label = set + " → " + topo.Type
			}
		}},
	{ids: []int{6006301}, class: lsClassMember, sev: lsSevWarn, label: "A shard changed primary",
		means: "The router noticed that one of the replica sets it routes to has a new primary. From the router's side this is the whole of a shard failover: the writes it had in flight to the old primary failed, and it had to find the new one.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			set, prim := r.str("replicaSet"), lsMongoShortHost(r.str("primary"))
			e.Peer = set
			if prim != "" {
				e.Message = set + " → " + prim
				e.Label = set + " changed primary to " + prim
			}
		}},
	// 471693 is the router's; 471691 the mongod's. Same message, different call sites —
	// which is what makes them a usable discriminator in lsSniffMongos.
	{ids: []int{471693, 471691, 22846, 22068}, class: lsClassConfig, sev: lsSevInfo, label: "Shard registry updated",
		means: "This process refreshed its record of which hosts make up a shard. Routine after any membership change; a continuous stream of them is a shard that will not settle.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if cs := r.str("connectionString"); cs != "" {
				e.Message = cs
				if i := strings.Index(cs, "/"); i > 0 {
					e.Peer = cs[:i]
				}
			}
		}},

	// ---- a shard that does not know it is one -----------------------------------------
	{ids: []int{22074}, class: lsClassConfig, sev: lsSevWarn, label: "Started as a shard but is not in a cluster",
		means: "The server was started with the shard role but has no shardIdentity document, so it does not know which cluster it belongs to or what its own shard is called. Normal for the few seconds between starting a member and adding it to the cluster; permanent afterwards means the addShard never completed, and the member will serve nothing routed."},

	// ---- routing metadata on the shards themselves --------------------------------------
	{ids: []int{4619901, 3463204}, class: lsClassConfig, sev: lsSevInfo, label: "Chunk metadata refreshed",
		means: "This process re-read which ranges of a collection it owns. It happens after any migration, and an operation that arrives while it is happening waits for it.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if ns := r.str("namespace"); ns != "" {
				e.Message = ns
			}
		}},
	{ids: []int{4619902}, class: lsClassConfig, sev: lsSevInfo, label: "Collection found to be unsharded",
		means: "The routing table was consulted for a collection that is not sharded, so it lives whole on one shard. Worth noticing in bulk: a cluster where the busy collections are all unsharded is paying for sharding and not using it."},
	{ids: []int{5966302}, class: lsClassConfig, sev: lsSevInfo, label: "Dropped this shard's copy of a collection's metadata",
		means: "This shard discarded its cached chunk map for a collection, usually because the collection was dropped or re-sharded. The next operation against it has to fetch the map again from the config servers."},

	// ---- sharding start-up ---------------------------------------------------------------
	{ids: []int{22072, 22071}, class: lsClassStartup, sev: lsSevInfo, label: "Sharding components started",
		means: "The member has worked out its place in the cluster and is ready to be routed to. Until this appears after a restart, the member is up and the cluster cannot use it."},

	// ---- the lock that serialises schema changes, which is where 7.0 diverged ------------
	//
	// 6.0 used a distributed lock manager held in the config servers; 7.0 replaced it with
	// per-database DDL coordinators. Both make the same guarantee — one schema change at a
	// time — and both look the same from the outside, so they share a rule and the version
	// difference stays out of the reader's way.
	{ids: []int{22655, 22650, 22649, 570181, 6855301, 6855302, 5390510}, class: lsClassConfig, sev: lsSevInfo,
		label: "Cluster schema-change lock",
		means: "Sharded schema changes are serialised cluster-wide: creating, dropping or sharding a collection takes a lock the whole cluster respects. 6.0 does it with a distributed lock, 7.0 with a DDL coordinator, and both mean the same thing — a second schema change waited. One that is never released blocks every subsequent DDL on that database."},

	// ---- the router's own health -----------------------------------------------------------
	{ids: []int{5936503}, class: lsClassState, sev: lsSevInfo, label: "Router health state changed",
		means: "mongos runs a fault manager over its own health checks. Reaching a non-Ok state is the router declaring itself unfit, which on some deployments takes it out of a load balancer.",
		enrich: func(r lsMongoRecord, e *lsEvent) {
			if st := r.str("state"); st != "" {
				e.Message = st
				e.Label = "Router health: " + st
				if st != "Ok" && st != "StartupCheck" {
					e.Sev = lsSevWarn
				}
			}
		}},
}

// lsShardEventLabel turns a changelog `what` into something readable.
//
// The vocabulary is MongoDB's and it is not obvious: "multi-split" is one chunk becoming
// several, and the ".start"/".commit"/".error" suffixes are phases of one migration rather
// than three separate events.
func lsShardEventLabel(what string) string {
	base, phase := what, ""
	if i := strings.Index(what, "."); i > 0 {
		base, phase = what[:i], what[i+1:]
	}
	label := map[string]string{
		"addShard":        "Shard added to the cluster",
		"removeShard":     "Shard removed from the cluster",
		"shardCollection": "Collection sharded",
		"dropCollection":  "Collection dropped",
		"dropDatabase":    "Database dropped",
		"moveChunk":       "Chunk moved between shards",
		"movePrimary":     "Database's primary shard changed",
		"split":           "Chunk split",
		"multi-split":     "Chunk split into several",
		"balancer":        "Balancer",
		// 8.0 added automatic chunk merging, and it arrived in this catalogue without a
		// code change — the operation is data rather than a rule, which is the whole point
		// of keying on 22080. It is named here only so the label reads well.
		"autoMerge":           "Chunks merged automatically",
		"setClusterParameter": "Cluster parameter changed",
		"resharding":          "Collection re-sharded",
	}[base]
	if label == "" {
		label = "Cluster metadata: " + base
	}
	switch phase {
	case "start":
		label += " (started)"
	case "commit", "end":
		label += " (completed)"
	case "error":
		label += " — FAILED"
	}
	return label
}

// lsResolveMongos fills in the state lane for a router.
//
// A mongos has no state machine: it is up or it is not. Rather than leave the lane blank —
// which reads as "no data" — every event is marked as serving, so the router's lane is a
// flat statement that it was running, and the interesting rows are the shards it could see.
func lsResolveMongos(events []lsEvent) {
	state := lsStateStarting
	for i := range events {
		if events[i].Code == "23015" || events[i].Code == "4615611" {
			state = lsStateRouting
		}
		if events[i].State == "" {
			events[i].State = state
		}
	}
}

// lsStateRouting is the only state a router has.
const lsStateRouting = "ROUTING"

// lsShardChangeSummary counts what the config servers recorded, busiest first, for a
// verdict line that says what the cluster actually did.
func lsShardChangeSummary(b *lsBundle) []string {
	counts := map[string]int{}
	for _, e := range lsPick(b, func(e lsEvent) bool { return e.Code == "22080" && e.Message != "" }) {
		what := e.Message
		if i := strings.Index(what, " "); i > 0 {
			what = what[:i]
		}
		counts[what]++
	}
	if len(counts) == 0 {
		return nil
	}
	var kinds []string
	for what := range counts {
		kinds = append(kinds, what)
	}
	// Busiest first, then alphabetical, so the sentence is stable between runs.
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	var out []string
	for _, what := range kinds {
		out = append(out, fmt.Sprintf("%s ×%d", what, counts[what]))
	}
	return out
}
