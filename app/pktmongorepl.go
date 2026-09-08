package main

// pktmongorepl.go — telling MongoDB's conversations apart when they all share a port.
//
// Galera puts its cluster traffic on 4567/4568/4444 and Patroni puts its on 8008 and
// etcd's two ports, so a capture of either can be classified before a byte of payload
// is read. MongoDB listens on **27017** and puts everything there: application queries,
// replica-set heartbeats, elections, oplog tailing, mongos→shard routing, mongos→config
// reads, and every driver's monitoring. A capture of a busy member is mostly *not* the
// application, and a tool that cannot say which is which is a tool that shows you 90 %
// heartbeats.
//
// So this file classifies by content, using the things the protocol itself states:
//
//	replSetHeartbeat                    a member checking another member, every 2 s
//	replSetRequestVotes, replSetStepUp  an election in progress — the interesting seconds
//	replSetUpdatePosition               secondaries reporting their oplog position
//	find/getMore on local.oplog.rs      a secondary tailing the primary's oplog
//	hello with a "client" document      a connection being set up, and by what
//	hello with topologyVersion          a driver's streaming monitor, which BLOCKS on
//	                                    purpose and must not be called slow
//	$db: "config", ns config.*          a mongos reading the routing table
//	shardVersion / databaseVersion      a mongos routing to a shard, with the version
//	                                    that decides whether it gets StaleConfig back
//	saslStart with __system             internal authentication between members
//
// The classification is per connection, not per message, because that is how it is
// useful: "this connection is the oplog tail" filters a capture down usefully, while
// "this message is a getMore" does not.

import (
	"strings"
)

// Connection kinds, coarsest first. These are what the UI's protocol column shows
// after "MongoDB/", so they are short on purpose.
const (
	mongoKindClient    = "client"    // an application's connection
	mongoKindMonitor   = "monitor"   // a driver's or a member's hello monitoring
	mongoKindHeartbeat = "heartbeat" // replSetHeartbeat between members
	mongoKindElection  = "election"  // replSetRequestVotes / replSetStepUp / freeze
	mongoKindOplog     = "oplog"     // a secondary tailing local.oplog.rs
	mongoKindReplPos   = "replpos"   // replSetUpdatePosition
	mongoKindConfig    = "config"    // reads/writes of the config database
	mongoKindRouted    = "routed"    // mongos → shard, carrying a shard version
	mongoKindInternal  = "internal"  // authenticated as __system, otherwise unclassified
)

// mongoKindLabel is how a kind appears in the protocol column.
func mongoKindLabel(kind string) string {
	switch kind {
	case mongoKindHeartbeat:
		return "MongoDB/heartbeat"
	case mongoKindElection:
		return "MongoDB/election"
	case mongoKindOplog:
		return "MongoDB/oplog"
	case mongoKindReplPos:
		return "MongoDB/replpos"
	case mongoKindMonitor:
		return "MongoDB/monitor"
	case mongoKindConfig:
		return "MongoDB/config"
	case mongoKindRouted:
		return "MongoDB/routed"
	case mongoKindInternal:
		return "MongoDB/internal"
	case mongoKindClient:
		return "MongoDB"
	}
	return "MongoDB"
}

// mongoKindRank orders the kinds by how much they say about a connection. A connection
// that heartbeats is a heartbeat connection whatever else it does; one that runs queries
// is a client even though it opened with hello like every other driver connection; and
// "monitor" is only the answer when nothing else has been seen.
func mongoKindRank(kind string) int {
	switch kind {
	case "":
		return 0
	case mongoKindMonitor:
		return 1
	case mongoKindClient:
		return 2
	}
	return 3
}

// mongoKindDescription explains a kind once, for the UI and the docs.
func mongoKindDescription(kind string) string {
	switch kind {
	case mongoKindHeartbeat:
		return "replica-set heartbeats: every member checks every other member every 2 seconds, forever"
	case mongoKindElection:
		return "an election: replSetRequestVotes and replSetStepUp are the seconds in which the primary changes"
	case mongoKindOplog:
		return "oplog tailing: a secondary reading local.oplog.rs from the primary — this IS MongoDB replication"
	case mongoKindReplPos:
		return "replSetUpdatePosition: secondaries reporting how far they have applied, which is what write concern waits on"
	case mongoKindMonitor:
		return "monitoring: hello/isMaster, either a driver watching the topology or a member watching its peers"
	case mongoKindConfig:
		return "config database traffic: the sharded cluster's routing table"
	case mongoKindRouted:
		return "mongos → shard: a routed command carrying the shard version that decides whether it is answered or refused"
	case mongoKindInternal:
		return "internal cluster traffic, authenticated as __system"
	case mongoKindClient:
		return "an application connection"
	}
	return ""
}

// pktMongoClassify decides (once) what a connection is for, and flags the events on it
// that matter. It is called for every client-side command, because the first command on
// a connection is not always the one that identifies it — a member's connection opens
// with hello and only then starts sending heartbeats.
func pktMongoClassify(p *pktPacket, c *pktConn, cmd, ns string, doc []bsonElem) {
	mc := c.mongoConn()

	kind := ""
	switch cmd {
	case "replSetHeartbeat":
		kind = mongoKindHeartbeat
		mc.heartbeats++
	case "replSetRequestVotes", "replSetStepUp", "replSetFreeze", "replSetStepDown", "replSetAbortPrimaryCatchUp":
		kind = mongoKindElection
	case "replSetUpdatePosition":
		kind = mongoKindReplPos
	case "hello", "isMaster", "ismaster":
		if mc.kind == "" {
			kind = mongoKindMonitor
		}
	case "find", "getMore", "aggregate", "count", "distinct", "listCollections", "listIndexes":
		if strings.HasPrefix(ns, "local.oplog") {
			kind = mongoKindOplog
		} else if strings.HasPrefix(ns, "config.") || ns == "config" {
			kind = mongoKindConfig
		}
	case "saslStart", "saslContinue", "authenticate":
		if u := bsonStr(mustGet(doc, "user")); u == "__system" {
			kind = mongoKindInternal
		}
		if m := bsonStr(mustGet(doc, "mechanism")); m != "" {
			mc.authMech = m
		}
	}
	// A shard version on any command means this arrived from a mongos, whatever the
	// command is — that is the field the shard checks before answering.
	if kind == "" || kind == mongoKindClient {
		if _, ok := bsonGet(doc, "shardVersion"); ok {
			kind = mongoKindRouted
		} else if _, ok := bsonGet(doc, "databaseVersion"); ok {
			kind = mongoKindRouted
		}
	}
	// Config-database access by any command.
	if kind == "" && (strings.HasPrefix(ns, "config.") || strings.HasPrefix(ns, "admin.system.version")) {
		kind = mongoKindConfig
	}
	// Any command that is actual work makes this a client connection — which matters
	// because EVERY driver connection opens with hello, so a first-command-wins rule
	// labelled real application connections "monitor" and left them there.
	if kind == "" && !mongoIsChatter(cmd) {
		kind = mongoKindClient
	}
	if kind == "" && mc.kind == "" {
		kind = mongoKindMonitor
	}
	// Precedence, not first-past-the-post: a specific kind is sticky, a client beats a
	// monitor, and a monitor is only ever the answer for a connection that has done
	// nothing else.
	if mongoKindRank(kind) >= mongoKindRank(mc.kind) && kind != "" {
		mc.kind = kind
	}
	c.stream.Role = mc.kind
	c.stream.RoleLabel = mongoKindLabel(mc.kind)

	// The client metadata in a handshake says which driver and application this is,
	// which is the fact a capture of a shared cluster is most often taken to establish.
	if cmd == "hello" || cmd == "isMaster" || cmd == "ismaster" {
		if e, ok := bsonGet(doc, "client"); ok {
			sub := bsonSub(e)
			if d, ok2 := bsonGet(sub, "driver"); ok2 {
				dd := bsonSub(d)
				mc.driver = strings.TrimSpace(bsonStr(mustGet(dd, "name")) + " " + bsonStr(mustGet(dd, "version")))
			}
			if a, ok2 := bsonGet(sub, "application"); ok2 {
				mc.appName = bsonStr(mustGet(bsonSub(a), "name"))
			}
			if mc.appName != "" {
				// c.user, not c.stream.User: pktDecode's final pass copies c.user into
				// every stream, so writing the stream directly here was silently undone.
				// MongoDB has no session user until authentication, and the application
				// name from the handshake is the closest thing — and often more useful,
				// since it says which service opened the connection.
				c.user = mc.appName
			}
		}
	}
	if u := bsonStr(mustGet(doc, "user")); u != "" && mc.authUser == "" {
		mc.authUser = u
		c.user = u // an authenticated user outranks the application name
	}
	if db := bsonStr(mustGet(doc, "$db")); db != "" && c.database == "" {
		c.database = db
	}

	// Events worth flagging, each of which is a thing that happened rather than a thing
	// that is wrong.
	switch cmd {
	case "replSetRequestVotes":
		p.Issues = append(p.Issues,
			"Election in progress — replSetRequestVotes: a member is standing for primary. Every write is refused with NotWritablePrimary until one wins, which is what an application sees as a brief outage")
	case "replSetStepDown":
		p.Issues = append(p.Issues,
			"replSetStepDown — the primary is being told to stand down; connections to it will start failing with NotWritablePrimary (10107)")
	case "replSetStepUp":
		p.Issues = append(p.Issues,
			"replSetStepUp — a member is asking to be made primary immediately, which is how a planned failover starts")
	case "replSetInitiate", "replSetReconfig":
		p.Issues = append(p.Issues,
			"Replica-set configuration change ("+cmd+") — the set's membership or settings are being rewritten")
	case "moveChunk", "_configsvrMoveChunk", "_shardsvrMoveRange", "moveRange":
		p.Issues = append(p.Issues,
			"Chunk migration ("+cmd+") — the balancer is moving data between shards; this competes with production traffic and briefly blocks writes on the range")
	case "abortTransaction":
		p.Issues = append(p.Issues,
			"Transaction aborted by the client — everything it wrote is discarded")
	case "killCursors":
		// Not a finding on its own; a driver kills cursors it no longer needs.
	case "shutdown":
		p.Issues = append(p.Issues,
			"shutdown command — this member is being stopped deliberately; every connection failure after this point is a consequence")
	}
}

// mongoNamespaceKind describes a namespace worth naming: MongoDB's internal
// collections are where a capture's more surprising traffic lives.
func mongoNamespaceKind(ns string) string {
	switch {
	case strings.HasPrefix(ns, "local.oplog.rs"):
		return "the oplog — the replication log every secondary tails"
	case strings.HasPrefix(ns, "local.system.replset"):
		return "the stored replica-set configuration"
	case strings.HasPrefix(ns, "local."):
		return "a local (non-replicated) collection"
	case ns == "config.shards":
		return "the list of shards in the cluster"
	case ns == "config.chunks":
		return "the chunk map — which shard owns which range"
	case ns == "config.collections":
		return "the sharded collections and their shard keys"
	case ns == "config.databases":
		return "the databases and their primary shards"
	case ns == "config.mongos":
		return "the routers the cluster knows about"
	case ns == "config.settings":
		return "cluster settings, including whether the balancer is enabled"
	case ns == "config.locks", ns == "config.lockpings":
		return "the distributed lock the balancer takes"
	case ns == "config.transactions":
		return "retryable-write and transaction bookkeeping"
	case strings.HasPrefix(ns, "config."):
		return "the sharded cluster's own metadata"
	case strings.HasPrefix(ns, "admin.system.version"):
		return "the cluster's feature-compatibility and shard identity"
	case strings.HasSuffix(ns, ".system.profile"):
		return "the profiler's own collection"
	}
	return ""
}
