package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// PS MongoDB labs — 10 of a planned 30 (see IMPLEMENTATION.md), covering standalone
// (psm), replica set (psmrs) and sharded cluster (psmdb) topologies. Every design
// template embeds a lab-hotelsim node (linked by edge to the Mongo target) + a
// lab-vnc node, exactly like every Valkey lab embeds lab-trafficsim + lab-vnc —
// same rationale: a live workload to look at, and a browser to view it from.
//
// Check Work dials MongoDB directly with the real driver over the stack's Docker
// network — the same joinStackForDial + directConnection mechanism the Data
// Generator's MongoDB backend already uses (datagen_mongo.go's mongoClientFor and
// dbConnFor) — never a shell/mongosh round trip. directConnection is exactly what
// most of these checks want anyway: a specific member's own state (which replica
// set role it holds, its own opcounters, whether a document replicated to it).

// labPSMDesign is a single standalone psm node + Intranet + hotelsim + VNC — for
// labs about core standalone behavior (query planning, TTL/capped collections,
// auth, backup/restore) that need no replication or sharding at all.
var labPSMDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-psm","type":"psm","label":"psm-1","os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"","x":300,"y":40},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":300,"y":220}
  ],
  "frames": [],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psm","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMRSDesign is a 3-member psmrs replica set + Intranet + hotelsim + VNC — for
// labs about replication mechanics, elections, read preference and write concern.
var labPSMRSDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-rs-1","type":"psmrs","label":"rs-1","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"lab-rs-2","type":"psmrs","label":"rs-2","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"lab-rs-3","type":"psmrs","label":"rs-3","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":830,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":560,"y":220}
  ],
  "frames": [
    {"id":"lab-psmrs","type":"psmrs","label":"lab-psmrs","x":560,"y":20,"w":400,"h":138,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "rootPassword":"","pmmNodeId":"","useProxy":false,"enablePBM":false,"seaweedfsNodeId":"",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmrs","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMDBShardedDesign is a "minimum" sharded cluster (mongos + 1 config server +
// 3 shards, each a single member) + Intranet + hotelsim + VNC — for labs about
// shard keys, chunks and query routing. The full "standard" 13-node setup (needed
// for the later config-server-replica-set lab) is deliberately deferred so this
// first batch of sharded labs shares one fast-deploying template.
var labPSMDBShardedDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-mongos","type":"psmdb","label":"mongos","frameId":"lab-psmdb","role":"mongos","exportEnabled":false,"exportHostPort":0,"x":560,"y":20},
    {"id":"lab-cfg1","type":"psmdb","label":"cfg1","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":680,"y":20},
    {"id":"lab-s0r1","type":"psmdb","label":"s0r1","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":110},
    {"id":"lab-s1r1","type":"psmdb","label":"s1r1","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":110},
    {"id":"lab-s2r1","type":"psmdb","label":"s2r1","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":110},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":560,"y":220}
  ],
  "frames": [
    {"id":"lab-psmdb","type":"psmdb","label":"lab-psmdb","x":540,"y":0,"w":300,"h":150,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "psmdbSetup":"minimum","rootPassword":"","pmmNodeId":"","useProxy":false,
     "enablePBM":false,"seaweedfsNodeId":"",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmdb","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// mongoLabFrameFromStack finds this lab stack's psmrs/psmdb frame.
func mongoLabFrameFromStack(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return designDoc{}, designFrame{}, false
	}
	for _, f := range doc.Frames {
		if f.Type == "psmrs" || f.Type == "psmdb" {
			return doc, f, true
		}
	}
	return doc, designFrame{}, false
}

// mongoLabFrameMembers returns ready-to-dial connections for a psmrs/psmdb
// frame's currently-running members, optionally filtered by Role ("mongos" |
// "config" | "shard"; "" = every member). Built on dbConnFor (datagen.go) —
// the same "find the deployment, read its engine + admin secrets" helper the
// Data Generator already uses for every SQL/Mongo node.
func (a *App) mongoLabFrameMembers(st Stack, doc designDoc, frame designFrame, role string) []dbConn {
	var out []dbConn
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != frame.Type {
			continue
		}
		if role != "" && n.Role != role {
			continue
		}
		if c, ok := a.dbConnFor(st, n.ID); ok {
			out = append(out, c)
		}
	}
	return out
}

// mongoLabSingleConn is a convenience for the standalone labs' fixed "lab-psm" node.
func (a *App) mongoLabSingleConn(st Stack) (dbConn, bool) {
	return a.dbConnFor(st, "lab-psm")
}

// mongoReplSetStatus is the minimal shape of `replSetGetStatus` these checks need.
type mongoReplSetStatus struct {
	MyState int32 `bson:"myState"`
	Members []struct {
		Name     string `bson:"name"`
		StateStr string `bson:"stateStr"`
		Self     bool   `bson:"self"`
	} `bson:"members"`
}

// mongoLabReplStatus dials c and runs replSetGetStatus.
func (a *App) mongoLabReplStatus(ctx context.Context, c dbConn) (mongoReplSetStatus, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return mongoReplSetStatus{}, err
	}
	defer closer()
	var out mongoReplSetStatus
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&out)
	return out, err
}

var psmdbLabs = []Lab{
	// ---------------------------------------------------------- PSMDB standalone
	{
		ID:          "psmdb-crud-query-planner",
		Title:       "CRUD Fundamentals & the Query Planner",
		Description: "Insert documents, run a query that scans every one of them, and fix it with an index — then go one step further and make the query answerable from the index alone.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Fundamentals & Query Performance",
		TimeLimit:   "2h",
		LectureNotes: `Every query has a plan, whether you asked for one or not

MongoDB's query planner picks an access method for every find/aggregate: either a collection scan (COLLSCAN — read every document, test each against the filter) or an index scan (IXSCAN — jump straight to the documents that match). explain() shows you exactly which one it picked, and why, without changing what the query actually does.

Why an index turns COLLSCAN into IXSCAN

An index is a separate, sorted data structure mapping field values to document locations. Without one, "find every item in category X" has no shortcut — MongoDB must look at every document. Create an index on that field and the same query becomes a direct lookup: explain()'s winningPlan.stage flips from "COLLSCAN" to "IXSCAN" (sometimes nested under an inputStage, depending on the plan shape).

Covered queries: skipping the document fetch entirely

A normal IXSCAN still has to fetch the full document from disk to return whatever fields you asked for (a FETCH stage). But if a compound index already contains every field the query filters on and every field it projects, MongoDB can answer straight from the index — no FETCH at all. executionStats.totalDocsExamined drops to 0 even though nReturned is greater than zero. This is the "covered query" — the cheapest possible read MongoDB can do.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "create-index",
				Title: "Turn a COLLSCAN into an IXSCAN",
				Instructions: "Open a terminal on the psm node. Connect with `mongosh -u admin -p admin_password --authenticationDatabase admin`. In `labdb`, insert a few documents into `items` with a `category` field, e.g. `db.items.insertMany([{category:\"a\",name:\"one\"},{category:\"b\",name:\"two\"},{category:\"a\",name:\"three\"}])`. Run `db.items.find({category:\"a\"}).explain()` and note the `COLLSCAN` stage. Create an index with `db.items.createIndex({category:1})`, then re-run the same explain — it should now show `IXSCAN`. Click Check Work.",
				Hint:  "The stage name lives at `.queryPlanner.winningPlan.stage` (or `.inputStage.stage` if there's a wrapping stage above it).",
			},
			{
				ID:    "covered-query",
				Title: "Make the query fully covered",
				Instructions: "Create a compound index that includes every field the query needs: `db.items.createIndex({category:1,name:1})`. Run `db.items.find({category:\"a\"},{name:1,_id:0}).explain(\"executionStats\")` — with `_id` excluded and both remaining fields in the index, MongoDB never has to fetch the document. Click Check Work.",
				Hint:  "`executionStats.totalDocsExamined` should read exactly 0, while `nReturned` is greater than 0 — that gap is the whole point.",
			},
		},
	},
	{
		ID:          "psmdb-ttl-capped",
		Title:       "TTL Indexes & Capped Collections",
		Description: "Two different ways MongoDB deletes old data for you: a TTL index that expires individual documents on a schedule, and a capped collection that evicts the oldest documents once it hits a fixed size.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Data Modeling",
		TimeLimit:   "2h",
		LectureNotes: `TTL indexes: expiration as a background job, not a query filter

A TTL (time-to-live) index is a normal single-field index on a date field with an extra expireAfterSeconds option. A background thread — the TTL monitor — sweeps roughly once a minute, deleting any document whose indexed date is older than expireAfterSeconds ago. Nothing about your queries changes; documents just quietly stop existing once they age out. This is how session stores, caches, and short-lived event logs are usually built in MongoDB — no cron job, no manual DELETE.

Capped collections: a fixed-size ring buffer

A capped collection is created with a maximum byte size up front. Once it's full, inserting a new document automatically deletes the oldest one to make room — insertion order is preserved and never reordered, which is also what makes capped collections the storage MongoDB itself uses internally for the replication oplog. Unlike a TTL index, there's no notion of "age" here at all — only "does it still fit."`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "ttl-index",
				Title: "Expire a document with a TTL index",
				Instructions: "On the psm node's mongosh: `db.sessions.createIndex({expiresAt:1},{expireAfterSeconds:0})`. Insert a document that's already \"expired\": `db.sessions.insertOne({_id:\"s1\",expiresAt:new Date(Date.now()-60000)})`. Wait about a minute for the TTL monitor to sweep, then click Check Work (click again if it hasn't run yet — the sweep isn't instant).",
				Hint:  "If Check Work says the index is missing, confirm `expireAfterSeconds` is really part of the index options, not a plain field.",
			},
			{
				ID:    "capped-collection",
				Title: "Overfill a capped collection",
				Instructions: "Create a small capped collection: `db.createCollection(\"logs\",{capped:true,size:4096})`. Insert 500 small documents: `for (let i=0;i<500;i++) db.logs.insertOne({i:i,msg:\"x\".repeat(50)})`. Click Check Work.",
				Hint:  "`db.logs.stats().capped` should be `true`, and `db.logs.countDocuments({})` should be far fewer than 500 — the oldest ones were evicted to keep the collection under 4096 bytes.",
			},
		},
	},
	{
		ID:          "psmdb-auth-rbac",
		Title:       "Authentication & Role-Based Access Control",
		Description: "Create a user scoped to exactly one collection and one permission, then prove from the other side that the restriction actually holds — not just that the role looks right on paper.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Security & Access Control",
		TimeLimit:   "2h",
		LectureNotes: `Roles are the unit of permission, users just hold roles

MongoDB's RBAC grants privileges through roles, not directly to users — a user is really just an identity plus a list of roles. Built-in roles like read or readWrite scope by database; for anything narrower (one specific collection, one specific action) you define a custom role with an explicit privileges list naming the exact resource and actions allowed.

The gap between "looks right" and "is enforced"

Reading back usersInfo/rolesInfo after creating a scoped user only proves the role document exists with the fields you intended — it says nothing about whether MongoDB actually enforces it. The only real proof is connecting as that user and trying the operation: an authorized read should succeed, and anything outside the granted scope should fail with an Unauthorized error (code 13), not merely "no matching documents" or some unrelated failure. This lab's second step is deliberately built on that distinction.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "create-scoped-user",
				Title: "Create a role scoped to one collection",
				Instructions: "As admin: `db.getSiblingDB(\"admin\").createRole({role:\"labItemsReader\",privileges:[{resource:{db:\"labdb\",collection:\"items\"},actions:[\"find\"]}],roles:[]})`. Then `db.getSiblingDB(\"admin\").createUser({user:\"labreader\",pwd:\"labreader_pw\",roles:[{role:\"labItemsReader\",db:\"admin\"}]})`. Click Check Work.",
				Hint:  "Both commands run against the `admin` database, authenticated as the cluster admin.",
			},
			{
				ID:    "verify-restriction",
				Title: "Prove the restriction actually holds",
				Instructions: "Nothing to run here — Check Work itself connects as `labreader` and confirms a read on `labdb.items` succeeds while a write to it fails with Unauthorized. If it doesn't pass yet, double check the role's `actions` list only includes `find`, and that the user has no other roles.",
				Hint:  "An `Unauthorized` error (code 13) is the specific proof required — any other kind of failure doesn't count.",
			},
		},
	},
	{
		ID:          "psmdb-backup-restore",
		Title:       "Backup & Restore with mongodump/mongorestore",
		Description: "Take a logical backup of the linked Hotel Sim's own data, destroy it, and bring it back exactly as it was.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Backup & Recovery",
		TimeLimit:   "2h",
		LectureNotes: `mongodump/mongorestore: the logical backup baseline

mongodump reads every document in a database or collection and writes it out as BSON files (plus metadata describing indexes); mongorestore reads those files back and re-inserts everything, recreating indexes too. It's slower and heavier than a filesystem/volume snapshot for a large deployment, but it's portable across MongoDB versions and doesn't require stopping anything — the standard first tool to reach for before layering on something like Percona Backup for MongoDB (covered in a later replica-set lab).

Why this lab targets Hotel Sim's data specifically

The Hotel Sim demo app seeds exactly 100 hotel documents into hotelsim.hotels at startup — always precisely 100, deterministically, every time. That fixed number turns "did the restore actually work" into a simple, unambiguous count check: drop the collection, restore it, and hotelsim.hotels.countDocuments({}) should read exactly 100 again.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "take-backup",
				Title: "Take a backup of hotelsim.hotels",
				Instructions: "On the psm node's terminal: `mongodump --username=admin --password=admin_password --authenticationDatabase=admin --db=hotelsim --collection=hotels -o /tmp/backup`. Click Check Work.",
				Hint:  "Check Work looks for `/tmp/backup/hotelsim/hotels.bson` on disk and confirms it isn't empty.",
			},
			{
				ID:    "restore-after-loss",
				Title: "Destroy it, then restore it",
				Instructions: "In mongosh: `db.getSiblingDB(\"hotelsim\").hotels.drop()`. Confirm it's gone, then restore: `mongorestore --username=admin --password=admin_password --authenticationDatabase=admin /tmp/backup`. Click Check Work.",
				Hint:  "Hotel Sim itself doesn't refill this collection — only your restore does. Expect exactly 100 documents back.",
			},
		},
	},
	// --------------------------------------------------------- PSMDB Replica Set
	{
		ID:          "psmdb-rs-fundamentals",
		Title:       "Replica Set Fundamentals: Initiate, Elect, Observe",
		Description: "Confirm a healthy 3-member replica set has exactly one PRIMARY, then force a new election with a clean step-down and prove a different member took over.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Replication Mechanics",
		TimeLimit:   "2h",
		LectureNotes: `One writable copy, replicated to the rest

A MongoDB replica set is a group of mongod processes holding the same data, with exactly one elected PRIMARY accepting writes at any moment — every other healthy member is a SECONDARY, continuously replaying the PRIMARY's oplog (a capped collection recording every write) to stay current. rs.status() reports every member's current state; rs.conf() shows the configuration (host list, priorities, votes) that state was elected under.

Step-down vs. crash: two very different ways to lose a primary

rs.stepDown() asks the current primary to voluntarily give up its role and triggers a clean election among the remaining members — no data loss, no unavailability window beyond the brief election itself. This is what a planned maintenance operation looks like in practice. A hard crash forces the same election but without the courtesy of a clean handoff — the remaining members simply notice the primary is unreachable and elect a new one once a majority agrees. Both this lab and later ones exercise the clean path first before advanced labs get into what happens when things go wrong mid-flight.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "observe-election",
				Title: "Find the current PRIMARY",
				Instructions: "Open a terminal on any of the three replica set members and run `mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'rs.status().members.map(m=>[m.name,m.stateStr])'`. Identify which member is PRIMARY. Click Check Work.",
				Hint:  "All three members should report healthy — one PRIMARY, two SECONDARY. If a member is still in STARTUP2, give it a little longer.",
			},
			{
				ID:    "force-election",
				Title: "Step down and confirm a new PRIMARY",
				Instructions: "On the current PRIMARY (the one from the previous step), run `rs.stepDown()` from mongosh. Wait a few seconds for the election, then click Check Work.",
				Hint:  "Check Work compares against the PRIMARY it saw in the previous step — a *different* member must hold the role now.",
			},
		},
	},
	{
		ID:          "psmdb-read-preference",
		Title:       "Read Preference Modes & Routing Reads to Secondaries",
		Description: "Point reads at the secondaries instead of the primary, and prove — from the server's own counters, not just \"it didn't error\" — that they actually served the traffic.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Read/Write Semantics",
		TimeLimit:   "2h",
		LectureNotes: `Read preference: choosing who answers a read

By default every read (like every write) goes to the PRIMARY. Read preference lets a client redirect reads elsewhere: secondary (only secondaries, error if none are available), secondaryPreferred (secondaries if available, primary otherwise), and a few others for more specific topologies. It's set per connection or per operation — nothing about the replica set's configuration has to change.

Why this trades consistency for capacity

Secondary reads can lag the primary by however long replication takes to catch up — usually milliseconds, but not guaranteed. In exchange, read load spreads across every member instead of bottlenecking on one, which is exactly why analytics/reporting workloads are a classic secondaryPreferred use case: slightly-stale answers are an acceptable trade for keeping that load off the primary serving live writes.

Proving it happened, not just that it didn't error

A read against a secondary succeeding doesn't by itself prove the secondary served it — the driver could in principle still be talking to the primary if you got the connection string wrong. The database profiler is the real proof: turned on for a database on one specific member, it records every operation that member itself actually executed, by namespace — so a profiled read of labdb.items on a secondary is a direct record of that secondary having done the work, not just of your query returning successfully.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "baseline",
				Title: "Enable profiling on the secondaries",
				Instructions: "Click Check Work now, before running anything — this turns on the database profiler for `labdb` on each secondary, so the next step can prove exactly which member actually served your reads.",
				Hint:  "If this fails, give the replica set a little longer to finish electing a PRIMARY.",
			},
			{
				ID:    "confirm-secondary-served-reads",
				Title: "Read from a secondary and confirm it served you",
				Instructions: "Connect with `mongosh \"mongodb://rs-1,rs-2,rs-3/?replicaSet=lab-psmrs&readPreference=secondary\" -u admin -p admin_password --authenticationDatabase admin` and run a handful of finds, e.g. `for (let i=0;i<20;i++) db.getSiblingDB(\"labdb\").items.find().toArray()`. Click Check Work.",
				Hint:  "Check Work looks in labdb's profiler log (system.profile) on each secondary for a recorded read against labdb.items — that's only there if that secondary actually executed it.",
			},
		},
	},
	{
		ID:          "psmdb-write-concern",
		Title:       "Write Concern & Majority Durability",
		Description: "Write one document with majority acknowledgment, then confirm directly from each secondary — not just the primary — that it's really there.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Read/Write Semantics",
		TimeLimit:   "2h",
		LectureNotes: `Write concern: how sure do you need to be before moving on?

Write concern controls how many replica set members must acknowledge a write before the driver considers it successful. w:1 (the loosest common setting) only waits for the primary — fast, but that write could vanish in a primary failover before it ever replicates. w:"majority" waits until a majority of voting members (2 of 3 here) have applied it, which is the durability guarantee MongoDB's own multi-document transactions rely on internally, and the level most production applications should default to for anything they can't afford to lose.

Majority-committed means actually present on other members, not just "acknowledged"

The only way to trust that a majority write really replicated — as opposed to just trusting the client library's report — is to go ask a secondary directly. This lab does exactly that: insert with w:"majority" from the primary, then connect straight to each secondary (bypassing any read-preference routing) and confirm the document is physically present there.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "majority-write",
				Title: "Insert with majority write concern",
				Instructions: "On the primary's mongosh: `db.getSiblingDB(\"labdb\").wc.insertOne({_id:\"labMarker\",note:\"majority write\"},{writeConcern:{w:\"majority\",wtimeout:5000}})`. Click Check Work.",
				Hint:  "Check Work connects directly to each secondary and looks for `_id:\"labMarker\"` in `labdb.wc` — it doesn't just trust that the insert call returned success.",
			},
		},
	},
	// ------------------------------------------------------------ PSMDB Sharded
	{
		ID:          "psmdb-sharding-fundamentals",
		Title:       "Sharding Fundamentals: Enable, Choose a Key, Watch Chunks Form",
		Description: "Shard your first collection on a chosen key and watch MongoDB split it into multiple chunks as data arrives.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Shard Key & Chunk Management",
		TimeLimit:   "4h",
		LectureNotes: `Sharding splits one logical collection across multiple independent replica sets

A sharded cluster distributes a collection's documents across several shards (each its own replica set) so no single one has to hold — or serve — all of it. Applications still see one logical collection through mongos, the query router; mongos and the config servers (a small replica set holding cluster metadata) are what make this transparent.

Chunks: the unit sharding actually moves around

Once a collection is sharded on a key, its documents are grouped into chunks — contiguous ranges of shard-key values. A brand-new sharded collection starts as a single chunk on one shard; as data accumulates and a chunk grows past a size threshold, MongoDB splits it into two, and the balancer (a background process) migrates chunks between shards to keep the distribution roughly even. Watching config.chunks grow from 1 row to several is watching this mechanism kick in for the first time.`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "shard-a-collection",
				Title: "Enable sharding and shard a collection",
				Instructions: "On the mongos node's terminal: `mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'sh.enableSharding(\"labdb\"); sh.shardCollection(\"labdb.items\",{itemId:1})'`. This lab's dataset is small, so also shrink this one collection's chunk size well below the 128MB default — otherwise nothing you insert next will ever be large enough to trigger a split: `db.adminCommand({configureCollectionBalancing:\"labdb.items\",chunkSize:1})`. Click Check Work.",
				Hint:  "Check Work reads `config.collections` for `labdb.items` and confirms its shard key matches `{itemId:1}`.",
			},
			{
				ID:    "watch-chunks-form",
				Title: "Insert enough data to split into multiple chunks",
				Instructions: "From mongosh on mongos: `for (let i=0;i<20000;i++) db.getSiblingDB(\"labdb\").items.insertOne({itemId:i,payload:\"x\".repeat(200)})`. This takes a little while — once it finishes, click Check Work (and again after a short wait if it hasn't split yet).",
				Hint:  "Check Work counts rows in `config.chunks` for `labdb.items` — it needs to be more than 1.",
			},
		},
	},
	{
		ID:          "psmdb-targeted-vs-scatter",
		Title:       "Targeted vs Scatter-Gather Queries",
		Description: "Run the same style of query two ways — one that names the shard key, one that doesn't — and see mongos route them completely differently.",
		Difficulty:  "Beginner",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Query Routing",
		TimeLimit:   "4h",
		LectureNotes: `mongos routes every query based on whether the shard key is known

If a query's filter includes an equality (or $in) on the shard key, mongos can compute exactly which shard(s) own that value and send the query only there — a targeted query. If the filter says nothing about the shard key, mongos has no way to rule any shard out, so it broadcasts the query to every shard and merges the results — a scatter-gather query.

Why this is the single most consequential design decision in a sharded schema

A scatter-gather query isn't wrong, but it costs roughly N times the work of a targeted one (N = shard count), and that cost only grows as more shards are added. Choosing a shard key that matches your application's actual query patterns — so the hot-path reads stay targeted — is the crux of designing for a sharded cluster; this lab makes the difference directly observable via explain()'s own shard-routing report rather than just taking it on faith.`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "targeted-query",
				Title: "Run a targeted query",
				Instructions: "This step reuses the `labdb.items` collection from the Sharding Fundamentals lab (if you haven't done that lab yet, shard it first the same way). From mongos: `db.getSiblingDB(\"labdb\").items.find({itemId:5}).explain()`. Click Check Work.",
				Hint:  "Check Work independently re-runs the same explain and checks `queryPlanner.winningPlan.shards.length` — for an equality match on the shard key, it should be 1.",
			},
			{
				ID:    "scatter-gather-query",
				Title: "Run a scatter-gather query",
				Instructions: "From mongos: `db.getSiblingDB(\"labdb\").items.find({payload:/^xxx/}).explain()` — a filter that says nothing about `itemId`. Click Check Work.",
				Hint:  "This time `shards.length` should equal the shard count (3) — mongos can't rule any of them out.",
			},
		},
	},
	{
		ID:          "psmdb-hashed-vs-ranged",
		Title:       "Hashed vs Ranged Shard Keys",
		Description: "Shard the same monotonically-increasing values two ways — once ranged, once hashed — and watch one hotspot on a single shard while the other spreads evenly.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Shard Key & Chunk Management",
		TimeLimit:   "4h",
		LectureNotes: `Ranged keys preserve order; hashed keys destroy it on purpose

A ranged shard key ({field:1}) keeps documents in shard-key order, so range queries stay targeted and efficient — but every chunk boundary is a contiguous range of values. A hashed shard key ({field:"hashed"}) instead shards on the hash of the field, scattering even sequential values across the keyspace essentially at random.

The monotonic-key hotspot problem

If application data arrives with an ever-increasing key — an auto-incrementing counter, a timestamp — a ranged shard key sends every single new insert into the *same* chunk (the current highest range), on the *same* shard, no matter how many shards exist. That shard becomes the write bottleneck for the entire cluster. Hashing the same field spreads those same monotonic inserts roughly evenly across every shard, at the cost of destroying range-query locality (a hashed key can't efficiently answer "give me everything between X and Y"). Neither option is universally correct — it depends entirely on whether your workload needs range scans or write-throughput more.`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "create-both",
				Title: "Shard the same key two ways",
				Instructions: "From mongos: `sh.shardCollection(\"labdb.itemsRanged\",{seq:1}); sh.shardCollection(\"labdb.itemsHashed\",{seq:\"hashed\"})`. Then insert the same monotonic sequence into both: `for (let i=0;i<20000;i++){db.getSiblingDB(\"labdb\").itemsRanged.insertOne({seq:i}); db.getSiblingDB(\"labdb\").itemsHashed.insertOne({seq:i});}`. Click Check Work.",
				Hint:  "Check Work confirms both namespaces are sharded — one with a plain ascending key, one with a hashed key.",
			},
			{
				ID:    "compare-distribution",
				Title: "Compare how evenly each one spread",
				Instructions: "No further action needed — Check Work compares each collection's per-shard document distribution.",
				Hint:  "Expect `itemsRanged` heavily concentrated on one shard (the monotonic key always extends the current top chunk) and `itemsHashed` spread much more evenly across all three.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, psmdbLabs...)
}

// ------------------------------------------------------------ check functions

// mongoLabExplain runs `db.runCommand({explain:{find:...}, verbosity:...})`
// against c and returns the decoded result — the one primitive every
// query-planner-flavored check in this file builds on.
func (a *App) mongoLabExplain(ctx context.Context, c dbConn, dbName string, findCmd bson.D, verbosity string) (bson.M, error) {
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return nil, err
	}
	defer closer()
	var out bson.M
	cmd := bson.D{
		{Key: "explain", Value: findCmd},
		{Key: "verbosity", Value: verbosity},
	}
	err = client.Database(dbName).RunCommand(ctx, cmd).Decode(&out)
	return out, err
}

// planStage extracts the winning plan's stage name, unwrapping one inputStage
// level if present (a FETCH/IXSCAN pair reports the leaf stage there).
func planStage(explain bson.M) string {
	qp, _ := explain["queryPlanner"].(bson.M)
	wp, _ := qp["winningPlan"].(bson.M)
	if s, _ := wp["stage"].(string); s != "" && s != "FETCH" {
		return s
	}
	if inner, ok := wp["inputStage"].(bson.M); ok {
		if s, _ := inner["stage"].(string); s != "" {
			return s
		}
	}
	if s, _ := wp["stage"].(string); s != "" {
		return s
	}
	return ""
}

func (a *App) checkMongoStandaloneIndexUsed(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	explain, err := a.mongoLabExplain(ctx, c, "labdb",
		bson.D{{Key: "find", Value: "items"}, {Key: "filter", Value: bson.D{{Key: "category", Value: "a"}}}}, "queryPlanner")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain — has `labdb.items` been created with a document where category is \"a\"?"}
	}
	stage := planStage(explain)
	if stage != "IXSCAN" {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("The query is still using %s — create an index with `db.items.createIndex({category:1})` and try again.", stageOrUnknown(stage))}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the query on category now uses an IXSCAN."}
}

func stageOrUnknown(s string) string {
	if s == "" {
		return "an unrecognized plan"
	}
	return s
}

func (a *App) checkMongoStandaloneCoveredQuery(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	explain, err := a.mongoLabExplain(ctx, c, "labdb", bson.D{
		{Key: "find", Value: "items"},
		{Key: "filter", Value: bson.D{{Key: "category", Value: "a"}}},
		{Key: "projection", Value: bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 0}}},
	}, "executionStats")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain — make sure `labdb.items` still has matching documents."}
	}
	es, _ := explain["executionStats"].(bson.M)
	examined, _ := toInt64(es["totalDocsExamined"])
	returned, _ := toInt64(es["nReturned"])
	if returned == 0 {
		return LabStepResult{Passed: false, Message: "The query returned no documents — insert at least one with category \"a\" first."}
	}
	if examined != 0 {
		return LabStepResult{Passed: false, Message: "The query still examines documents (not fully covered) — create `db.items.createIndex({category:1,name:1})` and query with a projection that excludes `_id`."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: totalDocsExamined is 0 — this query is fully covered by the index."}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func (a *App) checkMongoTTLIndex(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	cur, err := client.Database("labdb").Collection("sessions").Indexes().List(ctx)
	if err != nil {
		return LabStepResult{Passed: false, Message: "The `labdb.sessions` collection doesn't exist yet — create the TTL index first."}
	}
	var idxs []bson.M
	cur.All(ctx, &idxs)
	hasTTL := false
	for _, ix := range idxs {
		if _, ok := ix["expireAfterSeconds"]; ok {
			hasTTL = true
		}
	}
	if !hasTTL {
		return LabStepResult{Passed: false, Message: "No TTL index found on `labdb.sessions` — create one with `expireAfterSeconds`."}
	}
	count, err := client.Database("labdb").Collection("sessions").CountDocuments(ctx, bson.M{"_id": "s1"})
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not count documents in `labdb.sessions`."}
	}
	if count > 0 {
		return LabStepResult{Passed: false, Message: "The document is still there — the TTL monitor sweeps roughly once a minute. Wait a bit and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the expired document is gone — the TTL index did its job."}
}

func (a *App) checkMongoCappedCollection(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	// listCollections' own options document is the reliable source for whether a
	// collection was actually created capped — collStats succeeds even against a
	// namespace that doesn't exist at all (returning a zeroed-out stats doc with
	// no `capped` field whatsoever), which would otherwise misreport "exists but
	// isn't capped" for a collection that was never created.
	specs, err := client.Database("labdb").ListCollectionSpecifications(ctx, bson.M{"name": "logs"})
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not list collections in `labdb`."}
	}
	if len(specs) == 0 {
		return LabStepResult{Passed: false, Message: "The `labdb.logs` collection doesn't exist yet — create it with `db.createCollection(\"logs\",{capped:true,size:4096})`."}
	}
	var opts struct {
		Capped bool `bson:"capped"`
	}
	bson.Unmarshal(specs[0].Options, &opts)
	if !opts.Capped {
		return LabStepResult{Passed: false, Message: "`labdb.logs` exists but isn't capped — recreate it with `db.createCollection(\"logs\",{capped:true,size:4096})`."}
	}
	count, _ := client.Database("labdb").Collection("logs").CountDocuments(ctx, bson.M{})
	if count == 0 {
		return LabStepResult{Passed: false, Message: "Insert documents into `labdb.logs` until it overflows its size limit."}
	}
	if count >= 500 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("`labdb.logs` currently holds %d documents — with a 4096-byte cap that shouldn't be possible unless the collection wasn't actually capped at 4096 bytes.", count)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: `labdb.logs` is capped and holds only %d documents — older ones were evicted to stay under the size limit.", count)}
}

func (a *App) checkMongoScopedUserCreated(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	var out bson.M
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: "labreader"}}).Decode(&out)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not query usersInfo."}
	}
	users, _ := out["users"].(bson.A)
	if len(users) == 0 {
		return LabStepResult{Passed: false, Message: "No user named `labreader` found — create it with `createUser`."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the `labreader` user exists."}
}

func (a *App) checkMongoRestrictionEnforced(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	scoped := c
	scoped.Super = "labreader"
	scoped.Password = "labreader_pw"
	// mongo.Connect never fails on bad credentials by itself (auth happens
	// lazily on the first real command) — the actual proof has to come from
	// the operations below, not from this call succeeding.
	client, closer, err := a.mongoClientFor(ctx, scoped)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not reach the psm node as `labreader`."}
	}
	defer closer()
	if _, err := client.Database("labdb").Collection("items").CountDocuments(ctx, bson.M{}); err != nil {
		return LabStepResult{Passed: false, Message: "`labreader` could not read `labdb.items` — confirm the user exists with password `labreader_pw` and the role grants `find` on `labdb.items`."}
	}
	_, err = client.Database("labdb").Collection("items").InsertOne(ctx, bson.M{"_id": "should-fail"})
	if err == nil {
		return LabStepResult{Passed: false, Message: "`labreader` was able to write to `labdb.items` — the role should only grant `find`, not write access."}
	}
	if !isMongoUnauthorized(err) {
		return LabStepResult{Passed: false, Message: "The write failed, but not with the expected Unauthorized error — got: " + err.Error()}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labreader can read labdb.items but is rejected with Unauthorized on a write."}
}

func isMongoUnauthorized(err error) bool {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 13
	}
	var we mongo.WriteException
	if errors.As(err, &we) {
		if we.WriteConcernError != nil && we.WriteConcernError.Code == 13 {
			return true
		}
		for _, we2 := range we.WriteErrors {
			if we2.Code == 13 {
				return true
			}
		}
	}
	return false
}

func (a *App) checkMongoBackupTaken(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, c.ContainerID, []string{"test", "-s", "/tmp/backup/hotelsim/hotels.bson"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "No backup file found at /tmp/backup/hotelsim/hotels.bson yet — run mongodump first."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the backup file exists and isn't empty."}
}

func (a *App) checkMongoRestoreRecovered(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabSingleConn(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "The psm node isn't running yet — wait for it to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to the psm node."}
	}
	defer closer()
	count, err := client.Database("hotelsim").Collection("hotels").CountDocuments(ctx, bson.M{})
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not count hotelsim.hotels."}
	}
	if count != 100 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("hotelsim.hotels currently has %d documents, not 100 — drop it and mongorestore from /tmp/backup.", count)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: hotelsim.hotels is back to exactly 100 documents."}
}

// ---- replica set ----

func (a *App) checkMongoRSObserveElection(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Not all three replica set members are running yet — wait for the cluster to finish deploying."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var primaryNodeID string
	primaries, secondaries := 0, 0
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil {
			continue
		}
		switch status.MyState {
		case 1:
			primaries++
			primaryNodeID = nodeIDForContainer(deps, m.ContainerID)
		case 2:
			secondaries++
		}
	}
	if primaries != 1 || secondaries != 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected exactly 1 PRIMARY and 2 SECONDARY, found %d PRIMARY and %d SECONDARY — give the replica set a bit longer to settle.", primaries, secondaries)}
	}
	a.store.SetLabRunLeader(run.ID, primaryNodeID)
	return LabStepResult{Passed: true, Message: "Confirmed: exactly one PRIMARY and two healthy SECONDARY members."}
}

func (a *App) checkMongoRSForceElection(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no PRIMARY has been recorded yet."}
	}
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil || status.MyState != 1 {
			continue
		}
		nodeID := nodeIDForContainer(deps, m.ContainerID)
		if nodeID == "" {
			continue
		}
		if nodeID == run.InitialLeaderNode {
			return LabStepResult{Passed: false, Message: nodeLabel(doc, nodeID) + " is still PRIMARY — run `rs.stepDown()` on it."}
		}
		return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, nodeID) + " is now PRIMARY — a different member from before."}
	}
	return LabStepResult{Passed: false, Message: "No PRIMARY is currently reachable — wait for the election to finish and check again."}
}

// checkMongoReadPrefBaseline enables the profiler on labdb on every reachable
// secondary. hotelsim's own analytics agent already issues a steady stream of
// secondaryPreferred reads against its own database as part of its normal
// simulated workload — comparing serverStatus().opcounters.query before/after
// would trivially "pass" from that background traffic alone regardless of
// anything the learner does. Scoping to labdb (a database only this lab's own
// reads ever touch) and to the profiler (which records exactly which ops ran
// on *this specific node*, unlike a global counter) is what makes the next
// step's proof actually about the learner's own connection.
func (a *App) checkMongoReadPrefBaseline(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Not all three replica set members are running yet."}
	}
	enabled := 0
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil || status.MyState != 2 {
			continue
		}
		client, closer, err := a.mongoClientFor(ctx, m)
		if err != nil {
			continue
		}
		err = client.Database("labdb").RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err()
		closer()
		if err == nil {
			enabled++
		}
	}
	if enabled < 2 {
		return LabStepResult{Passed: false, Message: "Could not enable profiling on the secondaries yet — wait for the replica set to settle."}
	}
	a.store.SetLabRunBackupCount(run.ID, 1)
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Profiling enabled on %d secondaries. Now connect with readPreference=secondary and run some finds against labdb.items, then check the next step.", enabled)}
}

func (a *App) checkMongoReadPrefConfirmed(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialBackupCount == 0 {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — profiling hasn't been enabled yet."}
	}
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	total := int64(0)
	for _, m := range members {
		status, err := a.mongoLabReplStatus(ctx, m)
		if err != nil || status.MyState != 2 {
			continue
		}
		client, closer, err := a.mongoClientFor(ctx, m)
		if err != nil {
			continue
		}
		n, cErr := client.Database("labdb").Collection("system.profile").CountDocuments(ctx,
			bson.M{"ns": "labdb.items", "op": bson.M{"$in": []string{"query", "getmore"}}})
		closer()
		if cErr == nil {
			total += n
		}
	}
	if total <= 0 {
		return LabStepResult{Passed: false, Message: "No profiled reads on labdb.items found on any secondary yet — connect with readPreference=secondary and run some finds."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: %d profiled read(s) of labdb.items on a secondary — your reads were actually served there.", total)}
}

func (a *App) checkMongoMajorityWriteReplicated(ctx context.Context, st Stack) LabStepResult {
	doc, frame, ok := mongoLabFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No replica set found in this lab's stack."}
	}
	members := a.mongoLabFrameMembers(st, doc, frame, "")
	if len(members) < 3 {
		return LabStepResult{Passed: false, Message: "Not all three replica set members are running yet."}
	}
	present := 0
	for _, m := range members {
		client, closer, err := a.mongoClientFor(ctx, m)
		if err != nil {
			continue
		}
		count, cErr := client.Database("labdb").Collection("wc").CountDocuments(ctx, bson.M{"_id": "labMarker"})
		closer()
		if cErr == nil && count > 0 {
			present++
		}
	}
	if present < 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("The marker document is present on only %d member(s) so far — insert it with writeConcern {w:\"majority\"} from the primary.", present)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: the marker document is present on %d members — the majority write actually replicated.", present)}
}

// ---- sharded cluster ----

func (a *App) mongoLabMongos(st Stack) (dbConn, bool) {
	return a.dbConnFor(st, "lab-mongos")
}

func (a *App) checkMongoShardCollectionCreated(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet — wait for the cluster to finish deploying."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	var out bson.M
	err = client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": "labdb.items"}).Decode(&out)
	if err != nil {
		return LabStepResult{Passed: false, Message: "labdb.items isn't sharded yet — run sh.enableSharding + sh.shardCollection."}
	}
	key, _ := out["key"].(bson.M)
	if _, ok := key["itemId"]; !ok {
		return LabStepResult{Passed: false, Message: "labdb.items is sharded, but not on {itemId:1} as expected."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: labdb.items is sharded on {itemId:1}."}
}

func (a *App) checkMongoChunksFormed(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	n, ok := chunkCount(ctx, client, "labdb.items")
	if !ok {
		return LabStepResult{Passed: false, Message: "labdb.items isn't sharded yet — complete the previous step first."}
	}
	if n <= 1 {
		return LabStepResult{Passed: false, Message: "Still only 1 chunk — insert more spread-out itemId values and wait for the balancer/auto-split to catch up, then check again."}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: labdb.items has split into %d chunks.", n)}
}

// chunkCount looks up a namespace's collection UUID in config.collections, then
// counts config.chunks rows for that UUID — modern MongoDB keys chunks by UUID,
// not by namespace string directly.
func chunkCount(ctx context.Context, client *mongo.Client, ns string) (int64, bool) {
	var collDoc bson.M
	if err := client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": ns}).Decode(&collDoc); err != nil {
		return 0, false
	}
	uuid, ok := collDoc["uuid"]
	if !ok {
		return 0, false
	}
	n, err := client.Database("config").Collection("chunks").CountDocuments(ctx, bson.M{"uuid": uuid})
	if err != nil {
		return 0, false
	}
	return n, true
}

func (a *App) checkMongoTargetedQuery(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	explain, err := a.mongoLabExplain(ctx, c, "labdb",
		bson.D{{Key: "find", Value: "items"}, {Key: "filter", Value: bson.D{{Key: "itemId", Value: 5}}}}, "queryPlanner")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain — has labdb.items been sharded and populated?"}
	}
	n := shardCountFromExplain(explain)
	if n != 1 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected exactly 1 shard touched for an equality match on the shard key, saw %d.", n)}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the equality query on itemId was routed to exactly 1 shard."}
}

func (a *App) checkMongoScatterGatherQuery(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	explain, err := a.mongoLabExplain(ctx, c, "labdb",
		bson.D{{Key: "find", Value: "items"}, {Key: "filter", Value: bson.D{{Key: "payload", Value: bson.D{{Key: "$regex", Value: "^xxx"}}}}}}, "queryPlanner")
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run explain."}
	}
	n := shardCountFromExplain(explain)
	if n < 2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Expected the query to broadcast to every shard (3), saw %d — query on a field other than itemId.", n)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: querying on a non-shard-key field broadcast to %d shards.", n)}
}

func shardCountFromExplain(explain bson.M) int {
	qp, _ := explain["queryPlanner"].(bson.M)
	wp, _ := qp["winningPlan"].(bson.M)
	shards, _ := wp["shards"].(bson.A)
	return len(shards)
}

func (a *App) checkMongoHashedRangedCreated(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	rangedOK := shardKeyIs(ctx, client, "labdb.itemsRanged", false)
	hashedOK := shardKeyIs(ctx, client, "labdb.itemsHashed", true)
	if !rangedOK || !hashedOK {
		return LabStepResult{Passed: false, Message: "Shard both labdb.itemsRanged ({seq:1}) and labdb.itemsHashed ({seq:\"hashed\"}) first."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: both collections are sharded, one ranged and one hashed on seq."}
}

func shardKeyIs(ctx context.Context, client *mongo.Client, ns string, wantHashed bool) bool {
	var out bson.M
	if err := client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": ns}).Decode(&out); err != nil {
		return false
	}
	key, _ := out["key"].(bson.M)
	v, ok := key["seq"]
	if !ok {
		return false
	}
	s, isStr := v.(string)
	if wantHashed {
		return isStr && s == "hashed"
	}
	return !isStr
}

func (a *App) checkMongoDistributionCompared(ctx context.Context, st Stack) LabStepResult {
	c, ok := a.mongoLabMongos(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "mongos isn't running yet."}
	}
	client, closer, err := a.mongoClientFor(ctx, c)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not connect to mongos."}
	}
	defer closer()
	rangedRatio, rOK := distributionRatio(ctx, client, "labdb.itemsRanged")
	hashedRatio, hOK := distributionRatio(ctx, client, "labdb.itemsHashed")
	if !rOK || !hOK {
		return LabStepResult{Passed: false, Message: "Could not read $shardedDataDistribution for both collections yet — complete the previous step and insert data into both."}
	}
	if rangedRatio <= hashedRatio*2 {
		return LabStepResult{Passed: false, Message: fmt.Sprintf("Ranged distribution (max/min ratio %.1f) isn't meaningfully more skewed than hashed (%.1f) yet — insert more monotonically increasing seq values into both and check again.", rangedRatio, hashedRatio)}
	}
	return LabStepResult{Passed: true, Message: fmt.Sprintf("Confirmed: ranged is far more skewed (ratio %.1f) than hashed (ratio %.1f) — the monotonic key hotspots on one shard while hashing spreads it out.", rangedRatio, hashedRatio)}
}

// distributionRatio returns max(ownedDocs)/max(1,min(ownedDocs)) across shards
// for ns via the $shardedDataDistribution aggregation stage (the same one
// hotelsim's summarizeSharding already uses) — a high ratio means one shard
// dominates, a ratio near 1 means an even spread.
func distributionRatio(ctx context.Context, client *mongo.Client, ns string) (float64, bool) {
	// $shardedDataDistribution only lists shards that actually own at least one
	// document for ns — a collection sitting entirely on one shard comes back
	// with a single-element `shards` array, not one entry per shard with zeros
	// for the rest. Treating "not listed" as "not counted" (rather than as
	// zero) silently collapses the max/min ratio to 1.0 for exactly the
	// maximally-skewed case this check exists to catch, so every known shard
	// not present in the result must be folded in as an explicit 0.
	totalShards, err := client.Database("config").Collection("shards").CountDocuments(ctx, bson.M{})
	if err != nil || totalShards == 0 {
		return 0, false
	}
	cur, err := client.Database("admin").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$shardedDataDistribution", Value: bson.D{}}},
		{{Key: "$match", Value: bson.D{{Key: "ns", Value: ns}}}},
	})
	if err != nil {
		return 0, false
	}
	defer cur.Close(ctx)
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil || len(rows) == 0 {
		return 0, false
	}
	shards, _ := rows[0]["shards"].(bson.A)
	present := int64(0)
	var maxN, minN int64 = 0, 0
	for _, s := range shards {
		sm, ok := s.(bson.M)
		if !ok {
			continue
		}
		n, _ := toInt64(sm["numOwnedDocuments"])
		if n > maxN {
			maxN = n
		}
		present++
	}
	if present == 0 {
		return 0, false
	}
	// Every shard config.shards knows about but $shardedDataDistribution didn't
	// list for ns owns 0 documents — that's the true minimum whenever there are
	// more shards than rows returned.
	if present < totalShards {
		minN = 0
	} else {
		minN = maxN
		for _, s := range shards {
			sm, ok := s.(bson.M)
			if !ok {
				continue
			}
			n, _ := toInt64(sm["numOwnedDocuments"])
			if n < minN {
				minN = n
			}
		}
	}
	if minN < 1 {
		minN = 1
	}
	return float64(maxN) / float64(minN), true
}
