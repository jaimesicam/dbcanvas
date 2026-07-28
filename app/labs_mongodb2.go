package main

import (
	"encoding/json"
)

// Second batch of PS MongoDB labs (20 of the planned 30 — see IMPLEMENTATION.md #167
// for the first 10). Same mechanics throughout: dial via a.dbConnFor/a.mongoClientFor
// (datagen.go/datagen_mongo.go), check-dispatch cases added to labs.go's
// handleCheckLabStep switch, shared helpers reused from labs_mongodb.go
// (mongoLabFrameFromStack, mongoLabFrameMembers, mongoLabReplStatus, etc).

// ------------------------------------------------------------ design templates

// labPSMTLSDesign is labPSMDesign with GenerateCert baked in — TLS material is
// staged on disk at deploy (mongoApplyCert, app/mongodb.go) but never auto-enabled
// (see mongoApplyCert's own doc comment); the TLS lab's whole point is wiring it
// up by hand.
var labPSMTLSDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-psm","type":"psm","label":"psm-1","os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"","generateCert":true,"certTtlValue":365,"certTtlUnit":"days","x":300,"y":40},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":300,"y":220}
  ],
  "frames": [],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psm","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMRSSpareDesign is labPSMRSDesign's 3-member replica set plus one extra,
// unattached standalone psm node — a fresh empty mongod, exactly what rs.addArb()
// needs, with no backend changes required to support it.
var labPSMRSSpareDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-rs-1","type":"psmrs","label":"rs-1","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"lab-rs-2","type":"psmrs","label":"rs-2","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"lab-rs-3","type":"psmrs","label":"rs-3","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":830,"y":66},
    {"id":"lab-spare","type":"psm","label":"spare","os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"","x":700,"y":180},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":560,"y":300}
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

// labPSMRSPBMDesign is labPSMRSDesign with Percona Backup for MongoDB wired to a
// SeaweedFS node — mirrors the Patroni pgBackRest lab's SeaweedFS design shape
// (app/labs.go's labPatroniBackupDesign). PBM's storage config is set up
// automatically at deploy time (mongoConfigurePBMStorage) since EnablePBM is on.
var labPSMRSPBMDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-rs-1","type":"psmrs","label":"rs-1","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":574,"y":66},
    {"id":"lab-rs-2","type":"psmrs","label":"rs-2","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":702,"y":66},
    {"id":"lab-rs-3","type":"psmrs","label":"rs-3","frameId":"lab-psmrs","exportEnabled":false,"exportHostPort":0,"x":830,"y":66},
    {"id":"lab-seaweed","type":"seaweedfs","label":"seaweed","arch":"amd64","bucket":"lab-backups","tls":false,"x":700,"y":220},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":560,"y":300}
  ],
  "frames": [
    {"id":"lab-psmrs","type":"psmrs","label":"lab-psmrs","x":560,"y":20,"w":400,"h":138,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "rootPassword":"","pmmNodeId":"","useProxy":false,
     "enablePBM":true,"seaweedfsNodeId":"lab-seaweed","seaweedfsBucket":"lab-backups",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmrs","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMDBStandardShardedDesign is the "standard" 13-node sharded setup (1 mongos
// + 3-member config RS + 3 shards x 3-member replica sets) — matches exactly what
// StackDesigner.jsx's psmdbMembers("standard") produces (app/web/src/pages/
// StackDesigner.jsx:1456-1480). Needed only by the Config Server Replica Set lab;
// every other sharded lab in this batch reuses the lighter "minimum" template.
var labPSMDBStandardShardedDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-mongos","type":"psmdb","label":"mongos","frameId":"lab-psmdb","role":"mongos","exportEnabled":false,"exportHostPort":0,"x":560,"y":20},
    {"id":"lab-cfg1","type":"psmdb","label":"cfg1","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":680,"y":20},
    {"id":"lab-cfg2","type":"psmdb","label":"cfg2","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":800,"y":20},
    {"id":"lab-cfg3","type":"psmdb","label":"cfg3","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":920,"y":20},
    {"id":"lab-s0r1","type":"psmdb","label":"s0r1","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":110},
    {"id":"lab-s0r2","type":"psmdb","label":"s0r2","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":190},
    {"id":"lab-s0r3","type":"psmdb","label":"s0r3","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":270},
    {"id":"lab-s1r1","type":"psmdb","label":"s1r1","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":110},
    {"id":"lab-s1r2","type":"psmdb","label":"s1r2","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":190},
    {"id":"lab-s1r3","type":"psmdb","label":"s1r3","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":270},
    {"id":"lab-s2r1","type":"psmdb","label":"s2r1","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":110},
    {"id":"lab-s2r2","type":"psmdb","label":"s2r2","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":190},
    {"id":"lab-s2r3","type":"psmdb","label":"s2r3","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":270},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":920,"y":110}
  ],
  "frames": [
    {"id":"lab-psmdb","type":"psmdb","label":"lab-psmdb","x":540,"y":0,"w":420,"h":320,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "psmdbSetup":"standard","rootPassword":"","pmmNodeId":"","useProxy":false,
     "enablePBM":false,"seaweedfsNodeId":"",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmdb","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMDBSpareShardDesign is the "minimum" sharded cluster plus a spare,
// unattached 3-member psmrs frame — a fully-initialized standalone replica set
// the learner adds live via sh.addShard, then removes.
var labPSMDBSpareShardDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-mongos","type":"psmdb","label":"mongos","frameId":"lab-psmdb","role":"mongos","exportEnabled":false,"exportHostPort":0,"x":560,"y":20},
    {"id":"lab-cfg1","type":"psmdb","label":"cfg1","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":680,"y":20},
    {"id":"lab-s0r1","type":"psmdb","label":"s0r1","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":110},
    {"id":"lab-s1r1","type":"psmdb","label":"s1r1","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":110},
    {"id":"lab-s2r1","type":"psmdb","label":"s2r1","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":110},
    {"id":"lab-spare-1","type":"psmrs","label":"spare-1","frameId":"lab-spare-rs","exportEnabled":false,"exportHostPort":0,"x":560,"y":260},
    {"id":"lab-spare-2","type":"psmrs","label":"spare-2","frameId":"lab-spare-rs","exportEnabled":false,"exportHostPort":0,"x":680,"y":260},
    {"id":"lab-spare-3","type":"psmrs","label":"spare-3","frameId":"lab-spare-rs","exportEnabled":false,"exportHostPort":0,"x":800,"y":260},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":920,"y":110}
  ],
  "frames": [
    {"id":"lab-psmdb","type":"psmdb","label":"lab-psmdb","x":540,"y":0,"w":300,"h":150,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "psmdbSetup":"minimum","rootPassword":"","pmmNodeId":"","useProxy":false,
     "enablePBM":false,"seaweedfsNodeId":"",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"},
    {"id":"lab-spare-rs","type":"psmrs","label":"lab-spare-rs","x":540,"y":240,"w":400,"h":138,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "rootPassword":"","pmmNodeId":"","useProxy":false,"enablePBM":false,"seaweedfsNodeId":"",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmdb","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labPSMDBPBMDesign is the "minimum" sharded cluster with Percona Backup for
// MongoDB wired to a SeaweedFS node.
var labPSMDBPBMDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-mongos","type":"psmdb","label":"mongos","frameId":"lab-psmdb","role":"mongos","exportEnabled":false,"exportHostPort":0,"x":560,"y":20},
    {"id":"lab-cfg1","type":"psmdb","label":"cfg1","frameId":"lab-psmdb","role":"config","exportEnabled":false,"exportHostPort":0,"x":680,"y":20},
    {"id":"lab-s0r1","type":"psmdb","label":"s0r1","frameId":"lab-psmdb","role":"shard","shard":0,"exportEnabled":false,"exportHostPort":0,"x":560,"y":110},
    {"id":"lab-s1r1","type":"psmdb","label":"s1r1","frameId":"lab-psmdb","role":"shard","shard":1,"exportEnabled":false,"exportHostPort":0,"x":680,"y":110},
    {"id":"lab-s2r1","type":"psmdb","label":"s2r1","frameId":"lab-psmdb","role":"shard","shard":2,"exportEnabled":false,"exportHostPort":0,"x":800,"y":110},
    {"id":"lab-seaweed","type":"seaweedfs","label":"seaweed","arch":"amd64","bucket":"lab-backups","tls":false,"x":700,"y":220},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-hotelsim","type":"hotelsim","label":"hotelsim-01","x":920,"y":110}
  ],
  "frames": [
    {"id":"lab-psmdb","type":"psmdb","label":"lab-psmdb","x":540,"y":0,"w":300,"h":150,
     "os":"oraclelinux","osVersion":"9","arch":"amd64","psmdbMajor":"8.0","psmdbVersion":"",
     "psmdbSetup":"minimum","rootPassword":"","pmmNodeId":"","useProxy":false,
     "enablePBM":true,"seaweedfsNodeId":"lab-seaweed","seaweedfsBucket":"lab-backups",
     "generateCert":false,"certTtlValue":365,"certTtlUnit":"days"}
  ],
  "edges": [
    {"id":"lab-hs-edge","from":{"node":"lab-psmdb","port":"bottom"},"to":{"node":"lab-hotelsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)
var psmdbLabsBatch2 = []Lab{
	// ---------------------------------------------------------- PSMDB standalone
	{
		ID:          "psmdb-indexing-strategies",
		Title:       "Indexing Strategies: Compound, Multikey & Partial Indexes",
		Description: "Index an array field and watch MongoDB automatically build a multikey index, then build a partial index that only covers the documents that actually need it.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Fundamentals & Query Performance",
		TimeLimit:   "2h",
		LectureNotes: `Multikey indexes: one index entry per array element

When you index a field that holds an array, MongoDB doesn't just index "the array" as one value — it creates one index entry per element, automatically. The index is flagged multikey (explain() reports isMultiKey:true on the scan), and a query matching any single element finds the document. No special syntax is needed to opt in; MongoDB detects the array shape at index-build time.

Partial indexes: paying only for the documents that need it

A partial index only includes documents matching a filter expression (partialFilterExpression) you specify at creation time — documents outside that filter are simply absent from the index, keeping it smaller and cheaper to maintain than indexing the whole collection. This is the right tool when a query pattern only ever targets a subset of documents (e.g. "active" orders, or documents that have a particular optional field at all) — a sparse index is the older, narrower-purpose ancestor of this idea; a partial index can express any filter, not just "field exists."`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "multikey-index",
				Title: "Index an array field",
				Instructions: "On the psm node: `mongosh -u admin -p admin_password --authenticationDatabase admin`.\n\n" +
					"`db.getSiblingDB(\"labdb\").products.insertMany([{name:\"a\",tags:[\"x\",\"y\"]},{name:\"b\",tags:[\"y\",\"z\"]}])`\n\n" +
					"`db.getSiblingDB(\"labdb\").products.createIndex({tags:1})`\n\n" +
					"Run `db.getSiblingDB(\"labdb\").products.find({tags:\"y\"}).explain()` — the scan stage should report `isMultiKey: true`. Click Check Work.",
				Hint: "Check Work re-runs the same explain and looks for `isMultiKey:true` anywhere in the winning plan.",
			},
			{
				ID:    "partial-index",
				Title: "Build a partial index",
				Instructions: "Insert a mix of documents, some with `inStock:true` and some without it at all: `db.getSiblingDB(\"labdb\").products.insertMany([{name:\"c\",inStock:true},{name:\"d\"}])`.\n\n" +
					"Create a partial index: `db.getSiblingDB(\"labdb\").products.createIndex({name:1},{partialFilterExpression:{inStock:true}})`. Click Check Work.",
				Hint: "Check Work reads the index list and confirms a `partialFilterExpression` is present and matches `{inStock:true}`.",
			},
		},
	},
	{
		ID:          "psmdb-schema-validation",
		Title:       "Schema Validation with $jsonSchema",
		Description: "MongoDB is schema-flexible by default — add a $jsonSchema validator to a collection and watch it start rejecting documents that don't conform.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Data Modeling",
		TimeLimit:   "2h",
		LectureNotes: `Flexible by default, strict by choice

Every MongoDB collection accepts any valid BSON document by default — there's no built-in notion of "columns" to violate. Schema validation lets you opt in to structure exactly where you need it: attach a $jsonSchema validator (a JSON Schema-flavored document describing required fields, types, and constraints) to a collection, and every insert/update is checked against it before being accepted.

Validation is enforced server-side, on every writer

Because the validator lives on the collection itself (not in application code), it's enforced no matter which client, script, or accidental typo tries to write — the same guarantee a relational NOT NULL/CHECK constraint gives you, without giving up the schema-per-document flexibility everywhere else in the database.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "create-validator",
				Title: "Attach a $jsonSchema validator",
				Instructions: "On the psm node:\n\n" +
					"`db.getSiblingDB(\"labdb\").createCollection(\"orders\",{validator:{$jsonSchema:{bsonType:\"object\",required:[\"orderId\",\"amount\"],properties:{amount:{bsonType:\"number\",minimum:0}}}}})`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads the collection's own options and confirms a `$jsonSchema` validator is present.",
			},
			{
				ID:           "enforce-validation",
				Title:        "Prove it rejects a bad document",
				Instructions: "Nothing to run here — Check Work itself attempts to insert a document missing `orderId` into `labdb.orders` and expects MongoDB to reject it.",
				Hint:         "If this fails, double check `required` really lists `orderId` and that the collection wasn't recreated without the validator afterward.",
			},
		},
	},
	{
		ID:          "psmdb-tls",
		Title:       "Securing Connections with TLS",
		Description: "A per-node certificate is staged automatically, but TLS itself is never auto-enabled — wire it up by hand, then require it outright.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Security & Access Control",
		TimeLimit:   "2h",
		LectureNotes: `Certificate material staged, TLS left off on purpose

This node was deployed with a signed certificate already written to /etc/mongo/certs (server.pem, the combined cert+key mongod expects, plus a copy of the Intranet's CA) — but turning cluster TLS on is treated as a deliberate, all-at-once operator decision, never something a deploy flips silently. Making it live means editing mongod.conf's net.tls block yourself and restarting mongod once — after that, MongoDB lets you escalate or de-escalate between allowTLS / preferTLS / requireTLS live, no further restart needed.

allowTLS → preferTLS → requireTLS: one step at a time, by design

allowTLS accepts both TLS and plaintext connections at once — the correct starting mode for a live system, since it lets you migrate clients over one at a time before anything breaks. requireTLS is the end state: plaintext connections are refused outright. MongoDB won't let you jump straight from allowTLS to requireTLS via setParameter — it's an illegal state transition — you pass through preferTLS (TLS preferred, plaintext still tolerated) first. That enforced middle step is deliberate: it's one more forcing function against a live system going straight from "anything works" to "only TLS works" in a single command.`,
		DesignTemplate: labPSMTLSDesign,
		Steps: []LabStep{
			{
				ID:    "enable-tls",
				Title: "Turn on TLS",
				Instructions: "Open a terminal on the psm node as root.\n\n" +
					"Edit `/etc/mongod.conf` and add a `tls:` block *nested inside the existing* `net:` block (alongside `port` and `bindIpAll`, not a second `net:` key):\n\n" +
					"```\n  tls:\n    mode: allowTLS\n    certificateKeyFile: /etc/mongo/certs/server.pem\n    CAFile: /etc/mongo/certs/ca.crt\n    allowConnectionsWithoutCertificates: true\n```\n\n" +
					"(the last line matters — without it mongod expects every client to present its own certificate too, not just verify the server's).\n\n" +
					"Run `systemctl restart mongod`.\n\n" +
					"Confirm it's listening with TLS — note `--host psm-1`: the certificate only covers this node's real hostname, not 127.0.0.1, so connecting without `--host` fails on hostname verification even though TLS itself is working:\n\n" +
					"`mongosh --host psm-1 --tls --tlsCAFile=/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt -u admin -p admin_password --authenticationDatabase admin --eval 'db.adminCommand({ping:1})'`\n\n" +
					"Click Check Work.",
				Hint: "If mongod fails to restart, double-check the YAML indentation — `tls:` must line up under `net:`, one level deeper than `port:`.",
			},
			{
				ID:    "require-tls",
				Title: "Require TLS outright",
				Instructions: "Escalate live, no restart needed — but one step at a time: MongoDB rejects jumping straight from allowTLS to requireTLS.\n\n" +
					"First: `mongosh --host psm-1 --tls --tlsCAFile=/etc/pki/ca-trust/source/anchors/dbcanvas-ca.crt -u admin -p admin_password --authenticationDatabase admin --eval 'db.adminCommand({setParameter:1,tlsMode:\"preferTLS\"})'`\n\n" +
					"Then the same command with `tlsMode:\"requireTLS\"`. Click Check Work.",
				Hint: "Check Work confirms a non-TLS connection is now refused, and a TLS connection still works.",
			},
		},
	},
	{
		ID:          "psmdb-profiler-currentop-killop",
		Title:       "Diagnosing Slow Operations: Profiler, currentOp & killOp",
		Description: "Catch a slow query with the database profiler, then find and terminate a runaway operation before it ties up a connection forever.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Observability & Diagnostics",
		TimeLimit:   "2h",
		LectureNotes: `The profiler: a permanent record of what ran and how long it took

Enable profiling (db.setProfilingLevel(1, {slowms: N})) and MongoDB writes one document per operation slower than N milliseconds into system.profile — a capped collection you can query like any other. Set slowms to 0 and it logs everything, useful for a short diagnostic window; the default 100ms is meant for continuous production use, catching only genuinely slow outliers.

currentOp and killOp: acting on what's running right now, not what already finished

The profiler tells you what already happened. db.currentOp() shows what's running at this exact instant — every active operation, its duration so far, and the command that started it. Once you've identified a stuck or runaway operation by its opid, db.killOp(opid) terminates it — the standard escape hatch for a query that's going to run forever (an infinite-loop expression, a full collection scan nobody meant to run) without restarting the whole server.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "catch-with-profiler",
				Title: "Catch a query with the profiler",
				Instructions: "On the psm node: `db.getSiblingDB(\"labdb\").setProfilingLevel(1,{slowms:0})`.\n\n" +
					"Run any query: `db.getSiblingDB(\"labdb\").products.find().toArray()`. Click Check Work.",
				Hint: "Check Work looks in `labdb.system.profile` for at least one recorded operation.",
			},
			{
				ID:    "find-and-kill-hung-op",
				Title: "Find and kill a runaway operation",
				Instructions: "The $function below only ever runs once there's a document to evaluate it against, so seed one first:\n\n" +
					"`mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'db.getSiblingDB(\"labdb\").hang.insertOne({x:1})'`\n\n" +
					"Then start a deliberately infinite operation in the background:\n\n" +
					"`setsid mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'db.getSiblingDB(\"labdb\").hang.aggregate([{$match:{$expr:{$function:{body:\"function(){while(true){}}\",args:[],lang:\\\"js\\\"}}}}])' > /tmp/hang.log 2>&1 < /dev/null &`\n\n" +
					"Find it with `db.currentOp({\"command.aggregate\":\"hang\"})`, note its `opid`, then run `db.killOp(<opid>)`. Click Check Work.",
				Hint: "Check Work runs currentOp itself before and after — it expects to see the hung op, then confirms it's gone.",
			},
		},
	},
	{
		ID:          "psmdb-aggregation-pipeline",
		Title:       "The Aggregation Pipeline: From $match to $merge",
		Description: "Build a multi-stage aggregation pipeline that filters, groups, and finally writes its own results into a summary collection.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Fundamentals & Query Performance",
		TimeLimit:   "2h",
		LectureNotes: `A pipeline, not a single query

Aggregation is a sequence of stages, each one transforming the documents that flow out of the stage before it — $match filters (ideally first, so later stages see less data), $group collapses documents by a key while computing accumulators (sums, counts, averages), $sort and $limit shape the final order. Unlike find(), there's no ceiling on what a pipeline can express — most of what a SQL GROUP BY/JOIN/window function can do has an aggregation-stage equivalent.

$merge: a pipeline that persists its own output

Most pipelines just return a cursor of results. $merge as the final stage instead writes each output document into a target collection — inserting new ones and optionally updating existing ones by a key you choose. This turns an aggregation into a materialized, queryable summary that doesn't need to be recomputed on every read — exactly the shape a reporting/rollup table takes.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "build-pipeline",
				Title: "Group sales by category",
				Instructions: "Seed a fixed dataset:\n\n" +
					"`db.getSiblingDB(\"labdb\").sales.insertMany([{category:\"a\",amount:10},{category:\"a\",amount:15},{category:\"b\",amount:20},{category:\"b\",amount:5},{category:\"c\",amount:7}])`\n\n" +
					"Run `db.getSiblingDB(\"labdb\").sales.aggregate([{$group:{_id:\"$category\",total:{$sum:\"$amount\"}}}])` and confirm category \"a\" totals 25, \"b\" totals 25, \"c\" totals 7. Click Check Work.",
				Hint: "Check Work re-runs the exact same $group pipeline itself and compares the totals against this fixed dataset.",
			},
			{
				ID:    "merge-into-collection",
				Title: "Persist the summary with $merge",
				Instructions: "Add a $merge stage to write the grouped totals into a summary collection:\n\n" +
					"`db.getSiblingDB(\"labdb\").sales.aggregate([{$group:{_id:\"$category\",total:{$sum:\"$amount\"}}},{$merge:{into:\"salesSummary\"}}])`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads `labdb.salesSummary` directly and confirms it holds the same three totals — no pipeline re-run needed this time, since $merge's whole point is that the result now just sits there.",
			},
		},
	},
	{
		ID:          "psmdb-gridfs",
		Title:       "GridFS for Large File Storage",
		Description: "Store a file too large for a single document by splitting it into chunks with GridFS, then retrieve it back byte-for-byte.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB",
		Category:    "Data Modeling",
		TimeLimit:   "2h",
		LectureNotes: `Why files need their own convention

A single BSON document is capped at 16MB — fine for almost any application record, but not for a video, a large PDF, or a dataset export. GridFS is MongoDB's own convention (not a special server feature) for storing arbitrarily large files anyway: it splits a file into 255KB chunks written to a fs.chunks collection, with one fs.files document holding the file's metadata (filename, length, upload date) and enough information to reassemble the chunks in order on read.

mongofiles: the GridFS CLI

mongofiles (part of the same database-tools package as mongodump/mongorestore) is the simplest way to move a file in and out of GridFS without writing driver code: put uploads a local file, get downloads one back out, list shows what's stored. Application code that needs streaming access instead uses the driver's own GridFS bucket API, which does the same chunking under the hood.`,
		DesignTemplate: labPSMDesign,
		Steps: []LabStep{
			{
				ID:    "upload-file",
				Title: "Upload a file into GridFS",
				Instructions: "On the psm node's terminal:\n\n" +
					"`echo \"gridfs-test-content\" > /tmp/upload-me.txt && cd /tmp && mongofiles --db=labdb -u admin -p admin_password --authenticationDatabase=admin put upload-me.txt`\n\n" +
					"Click Check Work.",
				Hint: "Check Work looks in `labdb.fs.files` for a document named `upload-me.txt`.",
			},
			{
				ID:    "download-file",
				Title: "Download it back and confirm it matches",
				Instructions: "`mkdir -p /tmp/download && cd /tmp/download && mongofiles --db=labdb -u admin -p admin_password --authenticationDatabase=admin get upload-me.txt`\n\n" +
					"Then: `cmp /tmp/upload-me.txt /tmp/download/upload-me.txt`. Click Check Work.",
				Hint: "Check Work execs `cmp` between the original and the downloaded copy inside the container and expects them to be byte-identical.",
			},
		},
	},
	// --------------------------------------------------------- PSMDB Replica Set
	{
		ID:          "psmdb-election-priorities",
		Title:       "Manual Stepdown & Election Priorities",
		Description: "Configure one member to always win elections by giving it a higher priority, then prove that's exactly who takes over.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Failover & Elections",
		TimeLimit:   "2h",
		LectureNotes: `Priority: a thumb on the scale, not a guarantee

Every replica set member has a priority (default 1) that biases elections — a higher-priority member that's eligible to vote and reasonably caught up will be preferred over lower-priority peers, and MongoDB will even trigger an election to hand leadership to it if it's not already primary. Priority 0 goes further: that member can never become primary at all, no matter how caught up it is (the same mechanism behind hidden/delayed/reporting-only members generally).

Why this matters operationally

Priority is how you steer where the primary role lives — keeping it on the members in your primary datacenter, off an analytics-only replica, or off a member with weaker hardware — without having to manually step down and hope the "right" member wins the resulting election by chance.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "set-priorities",
				Title: "Give one member a higher priority",
				Instructions: "On any member:\n\n" +
					"`cfg = rs.conf(); cfg.members.forEach(m => m.priority = 1); cfg.members[0].priority = 2; rs.reconfig(cfg)`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads the live replica set config and confirms exactly one member has a strictly higher priority than the rest.",
			},
			{
				ID:    "verify-priority-wins",
				Title: "Step down and confirm the favored member wins",
				Instructions: "Find the current PRIMARY and run `rs.stepDown(15)` on it — 15, not 60: that number is how long *this* member refuses to seek re-election afterward, and a full 60-second freeze just makes you wait needlessly (if the current PRIMARY is already the favored member, step it down anyway; it should win the re-election right back).\n\n" +
					"The immediate election may hand leadership to whichever member happens to be fastest, not the favored one — MongoDB's priority takeover then calls a second election within about 30 seconds to correct that.\n\n" +
					"Wait roughly 30 seconds, then click Check Work.",
				Hint: "Check Work compares the new PRIMARY against the specific member you gave the higher priority to in the previous step — it must match exactly, not just be \"a different one.\" If it's still the wrong member, give it a little longer for the priority takeover to fire.",
			},
		},
	},
	{
		ID:          "psmdb-change-streams",
		Title:       "Change Streams: Reacting to Data in Real Time",
		Description: "Watch a collection for changes and react to inserts, updates, and deletes as they happen — no polling required.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Change Streams & Transactions",
		TimeLimit:   "2h",
		LectureNotes: `Change streams: the oplog, without exposing the oplog

Change streams give applications a subscribe-to-writes API without ever reading the internal replication oplog directly — collection.watch() returns a cursor that emits one document per write, tagged with an operationType (insert/update/delete/replace/...), the affected document's key, and (depending on options) the changed fields. Under the hood it's built on the oplog, but the interface is stable and public in a way the oplog's own format never was.

Resumability is the whole point

Every change event carries a resume token; a client that disconnects can reopen the stream from exactly that token and pick up without missing or duplicating anything, as long as the oplog hasn't rolled past that point yet. This is what makes change streams usable for real integrations (search-index sync, cache invalidation, cross-system replication) rather than just a live-tail curiosity — they're built to survive being one of many things production systems depend on.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "watch-and-record",
				Title: "Watch labdb.events and log what you see",
				Instructions: "Start a background watcher:\n\n" +
					"`setsid mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'const cur = db.getSiblingDB(\"labdb\").events.watch(); while (!cur.isClosed()) { if (cur.hasNext()) { const ch = cur.next(); db.getSiblingDB(\"labdb\").changeLog.insertOne({op: ch.operationType, at: new Date()}); } }' > /tmp/changestream.log 2>&1 < /dev/null &`\n\n" +
					"Then generate some activity:\n\n" +
					"`db.getSiblingDB(\"labdb\").events.insertOne({_id:1,v:1})`\n\n" +
					"`db.getSiblingDB(\"labdb\").events.updateOne({_id:1},{$set:{v:2}})`\n\n" +
					"`db.getSiblingDB(\"labdb\").events.deleteOne({_id:1})`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads `labdb.changeLog` and looks for insert, update, and delete all represented — give the watcher a couple seconds to catch up if it's not there yet.",
			},
		},
	},
	{
		ID:          "psmdb-transactions",
		Title:       "Multi-Document ACID Transactions",
		Description: "Move money between two documents atomically, then prove an aborted transaction leaves no trace at all.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Change Streams & Transactions",
		TimeLimit:   "2h",
		LectureNotes: `Beyond single-document atomicity

A single document's fields always update atomically in MongoDB — that's been true since long before transactions existed. What transactions add is atomicity *across* documents and collections: a session.startTransaction()/commitTransaction() block groups several writes so that either all of them become visible together, or (on abort) none of them do — the classic "debit one account, credit another" problem, which is unsafe to model as two independent writes no matter how carefully you order them.

Requires a replica set (or sharded cluster) — never standalone

Multi-document transactions need the oplog and the majority-commit machinery a replica set provides; a standalone mongod has neither, so transactions simply aren't available there. This is the single biggest behavioral difference standalone deployments have from replica sets and sharded clusters, and it's why any dbcanvas app that spans topologies (like Hotel Sim) has to fall back to a different, compensating-write strategy specifically for standalone.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "run-transaction",
				Title: "Transfer funds atomically",
				Instructions: "Seed two accounts:\n\n`db.getSiblingDB(\"labdb\").accounts.insertMany([{_id:\"A\",balance:500},{_id:\"B\",balance:500}])`\n\n" +
					"Run a transaction moving 100 from A to B:\n```js\nconst s = db.getMongo().startSession();\ns.startTransaction();\nconst db2 = s.getDatabase(\"labdb\");\ndb2.accounts.updateOne({_id:\"A\"},{$inc:{balance:-100}});\ndb2.accounts.updateOne({_id:\"B\"},{$inc:{balance:100}});\ns.commitTransaction();\n```\nClick Check Work.",
				Hint: "Check Work confirms A is 400, B is 600, and the total is still exactly 1000.",
			},
			{
				ID:           "abort-and-verify-atomicity",
				Title:        "Abort a transaction and confirm nothing leaked through",
				Instructions: "Run another transaction, but abort it instead of committing:\n```js\nconst s = db.getMongo().startSession();\ns.startTransaction();\nconst db2 = s.getDatabase(\"labdb\");\ndb2.accounts.updateOne({_id:\"A\"},{$inc:{balance:-100}});\ns.abortTransaction();\n```\nClick Check Work.",
				Hint:         "Check Work confirms A and B are unchanged from the previous step (400/600) — a partial, uncommitted write must be completely invisible.",
			},
		},
	},
	{
		ID:          "psmdb-arbiter-quorum",
		Title:       "Adding an Arbiter & Understanding Voting Quorum",
		Description: "Add a vote-only member with no data of its own, then watch it single-handedly keep the cluster past quorum when a real member goes down.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Cluster Topology",
		TimeLimit:   "2h",
		LectureNotes: `An arbiter votes but never holds data

rs.addArb() adds a member that participates in elections — it can vote for a primary and helps the set reach majority — but never receives any data and can never become primary itself. It's the lightest possible way to change a replica set's vote count without adding another full, data-bearing (and therefore storage- and resource-consuming) member.

Why 3 data-bearing members plus 1 arbiter beats 3 alone

With 3 voting members, losing 1 still leaves 2 of 3 — a majority, fine. But with only 2 data-bearing members and no arbiter, losing 1 leaves exactly 1 of 2, which is *not* a majority, and the survivor can't become primary. Adding a 4th vote (the arbiter) restores a comfortable majority margin without the cost of a 4th full data copy — exactly the trade this lab has you observe directly: stop one of two data-bearing secondaries and watch the arbiter's vote is what keeps the primary standing.`,
		DesignTemplate: labPSMRSSpareDesign,
		Steps: []LabStep{
			{
				ID:    "add-arbiter",
				Title: "Add the spare node as an arbiter",
				Instructions: "On the current PRIMARY: growing from 3 to 4 voting members changes what \"majority\" means by default, and MongoDB refuses the reconfig until you acknowledge that explicitly:\n\n" +
					"`db.adminCommand({setDefaultRWConcern:1,defaultWriteConcern:{w:\"majority\"}})`\n\n" +
					"Then: `rs.addArb(\"spare:27017\")`. Click Check Work.",
				Hint: "Check Work reads the live replica set config and looks for a 4th member with `arbiterOnly:true`. If rs.addArb itself fails, the error message names the setDefaultRWConcern command you need to run first.",
			},
			{
				ID:    "survive-secondary-loss",
				Title: "Lose a secondary, keep the primary",
				Instructions: "Pick one of the two data-bearing secondaries (not the arbiter) and stop mongod on it: `systemctl stop mongod`.\n\n" +
					"Click Check Work.\n\n" +
					"Afterward, `systemctl start mongod` to bring it back.",
				Hint: "Check Work confirms the PRIMARY is still PRIMARY (not stepped down) even with one data-bearing member gone — the arbiter's vote is what keeps 3 of 4 a majority.",
			},
		},
	},
	{
		ID:          "psmdb-hidden-delayed",
		Title:       "Hidden & Delayed Members for Analytics and Recovery",
		Description: "Turn one member into an invisible, time-delayed replica — safe for analytics load, and a rewind button for operator mistakes.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Cluster Topology",
		TimeLimit:   "2h",
		LectureNotes: `Hidden: present in the replica set, invisible to clients

A hidden member (hidden:true, which requires priority:0) still receives every write, but is left out of the host list a driver's replica-set discovery (the hello command) reports — clients using the connection string's seed list never get routed there, even with a broad read preference. It's a way to run a member for internal purposes only (a dedicated backup source, an analytics replica reached by a separate, explicit connection) without it ever accidentally absorbing production read traffic.

Delayed: a replica that lags on purpose

secondaryDelaySecs holds a member's oplog application back by a fixed number of seconds — the data was written to the primary, but this member only applies it after the delay elapses. Combined with hidden:true, this gives you a rolling window of "the cluster's state N minutes ago" you can query directly — a working, always-current answer to "what did this look like right before someone ran that bad update," without waiting for a much slower point-in-time restore from backup.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "make-hidden-delayed",
				Title: "Reconfigure one member hidden and delayed",
				Instructions: "On the PRIMARY:\n\n" +
					"`cfg = rs.conf(); cfg.members[2].hidden = true; cfg.members[2].priority = 0; cfg.members[2].secondaryDelaySecs = 60; rs.reconfig(cfg)`\n\n" +
					"(adjust the index if member 2 isn't rs-3). Click Check Work.",
				Hint: "hidden:true requires priority:0 in the same reconfig, or MongoDB rejects it outright — Check Work looks for all three fields together on one member.",
			},
			{
				ID:           "verify-delay-and-invisibility",
				Title:        "Confirm it's both delayed and invisible",
				Instructions: "Nothing to run — Check Work inserts its own marker document on the primary and immediately confirms it is (a) not yet present on the delayed member, and (b) that member's host doesn't appear in a `hello` topology check.",
				Hint:         "If this fails, confirm `secondaryDelaySecs` really landed on the intended member — a typo'd array index reconfigures the wrong one.",
			},
		},
	},
	{
		ID:          "psmdb-pbm-full-pitr",
		Title:       "Percona Backup for MongoDB: Full & Point-in-Time Restore",
		Description: "Take a full cluster-consistent backup with PBM, then enable point-in-time recovery so you're never limited to only the moment the backup was taken.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Backup & Recovery",
		TimeLimit:   "4h",
		LectureNotes: `PBM: cluster-consistent backups without stopping anything

Percona Backup for MongoDB coordinates a backup across every replica set member (and every shard, on a sharded cluster) so the result is a single consistent snapshot as of one moment — not N independent per-node dumps that could each reflect a slightly different point in time. Storage is already configured for this node (S3-compatible, pointed at the linked SeaweedFS node) — pbm-agent runs on every member, and the pbm CLI talks to whichever agent is reachable to drive backup/restore/status.

PITR: replaying the oplog past the last full backup

Point-in-time recovery layers continuous oplog slice uploads on top of periodic full backups (pbm config --set pitr.enabled=true). A PITR-enabled restore isn't limited to "whichever backup happened to run most recently" — it replays the oplog from the nearest prior full backup up to any timestamp you choose, which is what actually lets you undo "we ran the wrong update five minutes ago" without losing everything written since the last nightly backup.`,
		DesignTemplate: labPSMRSPBMDesign,
		Steps: []LabStep{
			{
				ID:    "full-backup",
				Title: "Take a full backup",
				Instructions: "On any replica set member's terminal — note `export $(cat ...)`, not `source`, since the env file has no `export` keyword of its own and a plain `source` wouldn't be visible to the `pbm` subprocess:\n\n" +
					"`export $(cat /etc/sysconfig/pbm-agent) && pbm backup`\n\n" +
					"Wait for it to finish, then run `pbm list`. Click Check Work.",
				Hint: "Check Work runs `pbm list -o json` itself and looks for at least one backup with status `done`.",
			},
			{
				ID:    "enable-pitr",
				Title: "Enable point-in-time recovery",
				Instructions: "`export $(cat /etc/sysconfig/pbm-agent) && pbm config --set pitr.enabled=true`\n\n" +
					"Wait about a minute for the first oplog slice to upload, then run `pbm status`. Click Check Work.",
				Hint: "Check Work runs `pbm config` and confirms `pitr.enabled` is true.",
			},
		},
	},
	{
		ID:          "psmdb-rollback",
		Title:       "Rollback: When a Former Primary Rejoins",
		Description: "Force a real rollback: freeze replication, write data that never makes it out, crash the primary, and watch that data get surgically removed when it rejoins.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Replica Set",
		Category:    "Failover & Elections",
		TimeLimit:   "2h",
		LectureNotes: `Why rollback exists at all

A write is only truly safe once it's replicated to a majority — MongoDB's own durability guarantee is built around that, not around "the primary accepted it." A write acknowledged with w:1 (not majority) can, in a narrow window, exist only on the primary that took it. If that primary then becomes unreachable before replicating and a different member gets elected in its place, the old primary's un-replicated writes are no longer part of the replica set's history at all once it rejoins — and MongoDB removes them, rather than silently diverging forever.

Manufacturing a deliberately narrow window, made observable

Real rollback windows are normally sub-second and invisible. This lab manufactures one on purpose: stop both secondaries first, so the primary has nowhere to replicate to; write to the primary in that gap (acknowledged locally, replicated nowhere); then stop the primary too before restarting the others. The two survivors elect a new primary between themselves, never having seen that write — and when the old primary restarts, it detects its oplog has diverged from the new primary's and rolls its extra write back, writing it out to a rollback file on disk rather than just silently discarding it, in case you need to recover that data by hand later.`,
		DesignTemplate: labPSMRSDesign,
		Steps: []LabStep{
			{
				ID:    "cause-divergence",
				Title: "Stop the secondaries, write to the primary, then crash it",
				Instructions: "This build doesn't have test-only failpoints enabled, so the window is created with timing instead: identify the current PRIMARY (call it P) and the two secondaries.\n\n" +
					"First give yourself room to work by raising the election timeout on P:\n\n" +
					"`mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'cfg=rs.conf(); cfg.settings.electionTimeoutMillis=60000; rs.reconfig(cfg)'`\n\n" +
					"Then stop mongod on BOTH secondaries — `systemctl stop mongod` on each — and *confirm* each one is actually down before continuing (`systemctl is-active mongod` should print `inactive`; `systemctl stop` returns before the process has necessarily finished exiting, so don't skip this check).\n\n" +
					"Only once both read inactive, on P:\n\n" +
					"`db.getSiblingDB(\"labdb\").rollbacktest.insertOne({_id:\"willroll\",note:\"never replicated\"})`\n\n" +
					"— with both secondaries confirmed down, this is acknowledged locally but has nowhere to replicate to.\n\n" +
					"Then on P: `systemctl stop mongod`.\n\n" +
					"Finally restart both former secondaries (`systemctl start mongod` on each) and wait — with P gone, they hold a real election between themselves (2 of 3 is still a majority). Click Check Work once one of them becomes PRIMARY.",
				Hint: "Check Work confirms a NEW primary has been elected among the two members that were secondaries — proof P is down and out of the picture. The election can take up to a minute. If the final rollback step doesn't take effect later, the most likely cause is writing to P before both secondaries had actually finished stopping.",
			},
			{
				ID:    "rejoin-and-rollback",
				Title: "Restart the old primary and watch it roll back",
				Instructions: "On P: `systemctl start mongod`.\n\n" +
					"Give it a little time to rejoin as a secondary and detect the divergence. Click Check Work.",
				Hint: "Check Work confirms the `willroll` document is gone from `labdb.rollbacktest` on every member — it existed only on P, and P rolled it back on rejoining a replica set whose history had already moved on without it.",
			},
		},
	},
	// ------------------------------------------------------------ PSMDB Sharded
	{
		ID:          "psmdb-balancer",
		Title:       "The Balancer: Chunk Migration in Action",
		Description: "Confirm the balancer is running, then force enough imbalance that it actually has to move a chunk — and prove it in the cluster's own audit log.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Shard Key & Chunk Management",
		TimeLimit:   "4h",
		LectureNotes: `The balancer: automatic, but not invisible

A background process on the cluster (coordinated through the config servers, not any one shard) periodically checks whether chunks are evenly distributed and, if not, migrates them between shards — moveChunk, run without any operator involvement by default. It's on unless explicitly disabled (sh.stopBalancer()), and its current mode is queryable at any time via the balancerStatus admin command.

config.changelog: the cluster's own audit trail

Every meaningful cluster-metadata event — a chunk split, a migration starting and committing, a shard being added or removed — is recorded in config.changelog on the config servers. It's the authoritative way to confirm a migration genuinely happened, rather than inferring it indirectly from where data ended up (which could just as easily reflect where it was initially placed).`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "confirm-balancer-on",
				Title: "Confirm the balancer is enabled",
				Instructions: "From mongos:\n\n" +
					"`mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'db.adminCommand({balancerStatus:1})'`\n\n" +
					"Click Check Work.",
				Hint: "Check Work runs the same command itself and expects `mode` to not be `\"off\"`.",
			},
			{
				ID:    "force-a-migration",
				Title: "Force enough imbalance to trigger a migration",
				Instructions: "From mongos:\n\n" +
					"`sh.enableSharding(\"labdb\"); sh.shardCollection(\"labdb.balanced\",{k:1}); db.adminCommand({configureCollectionBalancing:\"labdb.balanced\",chunkSize:1})`\n\n" +
					"Then insert a spread of keys — a short `for` loop inserting a few thousand small documents with sequential `k` values.\n\n" +
					"Give the balancer a minute, then click Check Work.",
				Hint: "Check Work reads `config.changelog` for `moveChunk` entries against `labdb.balanced` — that's the cluster's own record that a real migration happened, not just a guess from where the data landed.",
			},
		},
	},
	{
		ID:          "psmdb-config-server-rs",
		Title:       "Config Server Replica Set: the Cluster's Source of Truth",
		Description: "The config servers are a completely ordinary replica set underneath — connect to one directly, then take down its primary and watch the whole cluster keep working.",
		Difficulty:  "Intermediate",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Cluster Topology",
		TimeLimit:   "4h",
		LectureNotes: `The metadata layer is a replica set, not a special construct

Every shard key, chunk boundary, and shard membership fact a sharded cluster relies on lives in a handful of collections under the config database — and that database is hosted by an ordinary 3-member replica set, running the exact same election/replication machinery as any other replica set. mongos routers read this metadata (and cache it) constantly; every one of them is a client of this replica set, nothing more.

Losing the config RS's primary doesn't stop the cluster

Because it's a real replica set with real automatic failover, losing one config server member (even the primary) just triggers an ordinary election among the remaining two — sharded reads and writes through mongos continue uninterrupted, the same as any ordinary replica-set failover. This is deliberately not a single point of failure, even though it's easy to assume "the metadata store" must be one.`,
		DesignTemplate: labPSMDBStandardShardedDesign,
		Steps: []LabStep{
			{
				ID:    "observe-config-rs",
				Title: "Connect directly to a config server member",
				Instructions: "Open a terminal on any `cfg` node (e.g. `lab-cfg1`) and run:\n\n" +
					"`mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'rs.status().members.map(m=>[m.name,m.stateStr])'`\n\n" +
					"It looks like any other replica set. Click Check Work.",
				Hint: "Check Work connects to all three config members directly and expects exactly 1 PRIMARY and 2 SECONDARY among them.",
			},
			{
				ID:    "config-rs-survives-primary-loss",
				Title: "Take down the config RS primary",
				Instructions: "Identify which `cfg` node is PRIMARY (from the previous step) and stop mongod on it: `systemctl stop mongod`.\n\n" +
					"From mongos, confirm the cluster still works:\n\n" +
					"`mongosh -u admin -p admin_password --authenticationDatabase admin --eval 'db.adminCommand({listDatabases:1})'`\n\n" +
					"Click Check Work.",
				Hint: "Check Work confirms a new PRIMARY was elected among the two remaining config members, and that mongos can still answer a basic admin command.",
			},
		},
	},
	{
		ID:          "psmdb-zone-sharding",
		Title:       "Zone Sharding for Geo-Partitioned Data",
		Description: "Pin each region's data to its own shard by tagging shards into zones — a query for one region should only ever touch that region's shard.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Zones & Data Placement",
		TimeLimit:   "4h",
		LectureNotes: `Zones: telling the balancer where data is allowed to live

A zone is a named tag you attach to one or more shards (sh.addShardToZone), paired with one or more shard-key ranges you assign to that zone (sh.updateZoneKeyRange). Once both exist, the balancer treats the assignment as a hard constraint — it migrates chunks so that every key range ends up on a shard tagged for its zone, not just wherever happens to be least loaded.

The real-world use case: data residency and locality

Zone sharding is how a single sharded cluster keeps EU customer data on shards physically in the EU, or keeps a specific region's data on the shard nearest that region's application servers — without operating three entirely separate database clusters. The shard key has to include the zoning dimension (here, region) as its leading component, since zone ranges are defined in terms of shard-key values.`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "tag-zones",
				Title: "Tag each shard into its own zone",
				Instructions: "From mongos:\n\n" +
					"`sh.addShardToZone(\"rs0\",\"north\"); sh.addShardToZone(\"rs1\",\"central\"); sh.addShardToZone(\"rs2\",\"south\")`\n\n" +
					"Shard a collection on the zoning field:\n\n" +
					"`sh.enableSharding(\"labdb\"); sh.shardCollection(\"labdb.zoned\",{region:1,seq:1})`\n\n" +
					"Assign each zone's key range:\n\n" +
					"`sh.updateZoneKeyRange(\"labdb.zoned\",{region:\"north\",seq:MinKey},{region:\"north\",seq:MaxKey},\"north\"); sh.updateZoneKeyRange(\"labdb.zoned\",{region:\"central\",seq:MinKey},{region:\"central\",seq:MaxKey},\"central\"); sh.updateZoneKeyRange(\"labdb.zoned\",{region:\"south\",seq:MinKey},{region:\"south\",seq:MaxKey},\"south\")`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads `config.tags` and confirms all three zone/shard/range assignments exist.",
			},
			{
				ID:    "verify-region-pinned",
				Title: "Confirm each region's data lives on its assigned shard",
				Instructions: "Insert some data into each region: a short loop inserting a handful of `{region:\"north\",seq:i}`, `{region:\"central\",seq:i}`, `{region:\"south\",seq:i}` documents.\n\n" +
					"Give the balancer a minute to move any misplaced chunks, then click Check Work.",
				Hint: "Check Work runs `explain(\"executionStats\")` on a query filtered to one region and confirms every document actually returned came from the shard that region is zoned to — mongos may still list a neighboring shard in the routing plan (an empty boundary chunk right at the zone edge), which is harmless as long as that shard returns zero documents.",
			},
		},
	},
	{
		ID:          "psmdb-add-remove-shard",
		Title:       "Adding & Removing a Shard Live",
		Description: "Attach an already-running replica set to the cluster as a brand new shard, then gracefully drain and remove it — both without any downtime.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Cluster Topology",
		TimeLimit:   "4h",
		LectureNotes: `Adding a shard: introducing an existing replica set, once it's actually eligible

sh.addShard() takes a replica set's connection string and registers it as a new shard — but "any" replica set only in the loose sense. A shard has to be a full peer of the config servers and every other shard in exactly the ways that matter for cluster membership: the same internal cluster-auth key (every member authenticates every other member across the whole deployment with one shared secret, not per-replica-set secrets), and mongod started with sharding.clusterRole: shardsvr so it identifies itself as shard-eligible at all. A replica set stood up as an ordinary, freestanding deployment — exactly like this lab's spare one — needs both retrofitted before addShard will accept it; provisioning a shard's replica set with the target cluster's key and role from the start is what avoids this step in practice. Once added, the balancer starts considering it a valid migration target immediately, growing the cluster's total capacity without any downtime for existing traffic.

Removing a shard: draining first, gone second

sh.removeShard() doesn't instantly delete a shard — it first switches the shard into "draining," during which the balancer migrates every chunk it owns off to the remaining shards. Only once draining reports complete (state: "completed") does the shard actually leave the cluster's shard list. Removing a shard that still owns data is exactly the operation this drain protects against — no chunk (and no data) can ever go missing mid-removal.`,
		DesignTemplate: labPSMDBSpareShardDesign,
		Steps: []LabStep{
			{
				ID:    "add-shard",
				Title: "Add the spare replica set as a shard",
				Instructions: "The spare replica set was provisioned as an ordinary standalone one, not a shard — three real prerequisites first.\n\n" +
					"(1) Every shard in a cluster must share the same internal cluster-auth key; copy the main cluster's onto all three spare members and restart mongod on each (run from the dbcanvas host, not inside a node):\n\n" +
					"`docker cp <a-main-shard-container>:/etc/mongo.keyFile /tmp/shared.keyFile`\n\n" +
					"then for each spare member:\n\n" +
					"`docker cp /tmp/shared.keyFile <container>:/etc/mongo.keyFile && docker exec -u root <container> chown mongod:mongod /etc/mongo.keyFile && systemctl restart mongod`\n\n" +
					"(2) Each spare member's `/etc/mongod.conf` needs `sharding:\\n  clusterRole: shardsvr` appended, then `systemctl restart mongod` again.\n\n" +
					"(3) From mongos, add it using the members' full FQDNs (short hostnames won't match what the replica set itself reports):\n\n" +
					"`sh.addShard(\"lab-spare-rs/spare-1.example.net:27017,spare-2.example.net:27017,spare-3.example.net:27017\")`\n\n" +
					"Click Check Work.",
				Hint: "Check Work reads `config.shards` and confirms a 4th shard now exists. If addShard itself errors, the message tells you exactly which prerequisite is still missing.",
			},
			{
				ID:    "remove-shard",
				Title: "Drain and remove it",
				Instructions: "From mongos:\n\n" +
					"`db.adminCommand({removeShard:\"lab-spare-rs\"})`\n\n" +
					"Repeat the same command every so often — it reports draining progress — until `state` reads `\"completed\"`. Click Check Work.",
				Hint: "Check Work reads `config.shards` and confirms the 4th shard is gone again.",
			},
		},
	},
	{
		ID:          "psmdb-jumbo-chunk",
		Title:       "Diagnosing & Fixing a Jumbo Chunk",
		Description: "Build a shard key too coarse to split, watch a chunk grow oversized and stuck, then fix it by refining the shard key with a differentiator field.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Shard Key & Chunk Management",
		TimeLimit:   "4h",
		LectureNotes: `Why a chunk can get stuck

A chunk covers a range of shard-key values; splitting it means finding a value strictly inside that range to split on. If every document in an oversized chunk shares the exact same shard-key value (a low-cardinality key — a tenant ID with one enormous tenant, a status field with only a few possible values), there's no value to split on at all — the chunk can grow indefinitely and MongoDB marks it jumbo rather than pretending it can shrink it.

refineCollectionShardKey: adding precision without a full reshard

You can't change a sharded collection's key outright, but refineCollectionShardKey lets you extend an existing key with additional trailing fields (as long as a compatible index already exists covering the new, longer key) — turning {tenantId:1} into {tenantId:1,_id:1} gives every document within even the biggest tenant a genuinely unique position, making splits possible again where they weren't before. It's a much lighter operation than a full reshardCollection, and the right first thing to reach for when the fix is "make the key more selective," not "change what the key even is."`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "produce-a-jumbo-chunk",
				Title: "Build a shard key too coarse to split",
				Instructions: "From mongos:\n\n" +
					"`sh.enableSharding(\"labdb\"); sh.shardCollection(\"labdb.jumbo\",{tenantId:1}); db.adminCommand({configureCollectionBalancing:\"labdb.jumbo\",chunkSize:1})`\n\n" +
					"Insert a lot of documents that all share the exact same tenantId — a loop inserting several thousand `{tenantId:\"bigtenant\",payload:\"x\".repeat(200)}` documents.\n\n" +
					"Wait a couple minutes for the auto-splitter to try and fail, then click Check Work.",
				Hint: "Check Work reads `config.chunks` for `labdb.jumbo` and looks for a chunk flagged `jumbo:true`.",
			},
			{
				ID:    "refine-the-shard-key",
				Title: "Refine the shard key to add a differentiator",
				Instructions: "Create a compatible compound index first:\n\n" +
					"`db.getSiblingDB(\"labdb\").jumbo.createIndex({tenantId:1,_id:1})`\n\n" +
					"Then refine: `db.adminCommand({refineCollectionShardKey:\"labdb.jumbo\",key:{tenantId:1,_id:1}})`. Click Check Work.",
				Hint: "Check Work reads `config.collections` for `labdb.jumbo` and confirms the shard key now includes `_id` as well as `tenantId`.",
			},
		},
	},
	{
		ID:          "psmdb-resharding",
		Title:       "Resharding a Live Collection",
		Description: "Realize the original shard key was the wrong choice and switch to a completely different one — live, without dropping and recreating the collection.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Shard Key & Chunk Management",
		TimeLimit:   "4h",
		LectureNotes: `refineCollectionShardKey extends a key; reshardCollection replaces it

refineCollectionShardKey can only ever add trailing fields to the existing key — it can't change the leading field, and it can't fix a key that's wrong for reasons other than low cardinality (wrong access pattern entirely, for instance). reshardCollection is the more powerful — and much heavier — tool: it copies the collection's data into a new, differently-keyed sharded collection behind the scenes, cuts traffic over once caught up, and only then drops the old copy.

Why it's disruptive by nature, not a bug

Because every document has to be re-examined and placed under the new key, reshardCollection's running time scales with the size of the collection being resharded — for a large production collection this is a genuinely heavy, carefully-scheduled operation, not something to reach for casually. This lab's dataset is intentionally tiny so the whole operation completes in seconds, but the mechanism is the same one used on collections orders of magnitude larger.`,
		DesignTemplate: labPSMDBShardedDesign,
		Steps: []LabStep{
			{
				ID:    "reshard-to-a-new-key",
				Title: "Reshard from oldKey to newKey",
				Instructions: "From mongos:\n\n" +
					"`sh.enableSharding(\"labdb\"); sh.shardCollection(\"labdb.reshardme\",{oldKey:1})`\n\n" +
					"Insert a small amount of data:\n\n" +
					"`for (let i=0;i<200;i++) db.getSiblingDB(\"labdb\").reshardme.insertOne({oldKey:i,newKey:200-i})`\n\n" +
					"Then reshard: `db.adminCommand({reshardCollection:\"labdb.reshardme\",key:{newKey:1}})`. This can take a little while even for a small collection. Click Check Work.",
				Hint: "Check Work reads `config.collections` for `labdb.reshardme` and confirms the shard key is now `{newKey:1}`, not `{oldKey:1}`.",
			},
		},
	},
	{
		ID:          "psmdb-pbm-sharded",
		Title:       "Percona Backup for MongoDB on a Sharded Cluster",
		Description: "Take a coordinated, cluster-wide backup that captures every shard and the config servers at one consistent moment.",
		Difficulty:  "Advanced",
		Database:    "PSMDB",
		Technology:  "PSMDB Sharded",
		Category:    "Backup & Recovery",
		TimeLimit:   "4h",
		LectureNotes: `Coordinating across shards is the hard part

Backing up a single replica set just means capturing one consistent point in its own oplog. A sharded cluster is many replica sets (every shard, plus the config servers) that each have their own independent oplog — a useful backup has to capture all of them at effectively the same logical moment, or the result is internally inconsistent (a document's chunk migration recorded as complete on one shard's backup but not yet started on another's). PBM's agents coordinate this cluster-wide, driven from a single pbm backup command issued anywhere in the cluster — you never orchestrate each shard's backup by hand.

Restore is topology-aware too

A PBM restore of a sharded cluster restores every shard and the config servers together, from the same coordinated backup — not a series of independent per-shard restores an operator has to sequence and hope stay consistent with each other.`,
		DesignTemplate: labPSMDBPBMDesign,
		Steps: []LabStep{
			{
				ID:    "cluster-wide-backup",
				Title: "Take a cluster-wide backup",
				Instructions: "pbm-agent only runs on data-bearing members (shards and config servers) — mongos holds no data of its own, so run this from one of the shard terminals (e.g. `s0r1`) instead:\n\n" +
					"`export $(cat /etc/sysconfig/pbm-agent) && pbm backup`\n\n" +
					"Wait for it to finish, then run `pbm list`. Click Check Work.",
				Hint: "Check Work runs `pbm list -o json` from a shard member itself and looks for at least one backup with status `done`.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, psmdbLabsBatch2...)
}
