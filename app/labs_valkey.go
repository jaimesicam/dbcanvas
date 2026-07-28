package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Valkey Cluster labs — hands-on scenarios on a real, disposable 3-node
// all-master Valkey Cluster (the same cluster-create pipeline Stack Designer
// uses: valkey-cli --cluster create ... --cluster-replicas 0). Unlike the
// Patroni curriculum, there's no HAProxy in front — a cluster-aware client
// talks to any node and follows MOVED redirects itself, so labs work
// directly against each node's own valkey-cli.

// labValkeyClusterDesign is a 3-node Valkey Cluster + Intranet, the minimum
// Valkey Cluster needs (a 3-member cluster bus can reach quorum for its own
// gossip-based failure detection).
var labValkeyClusterDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-vk-1","type":"valkeycluster","label":"valkey-1","frameId":"lab-valkey-cluster","x":574,"y":66},
    {"id":"lab-vk-2","type":"valkeycluster","label":"valkey-2","frameId":"lab-valkey-cluster","x":702,"y":66},
    {"id":"lab-vk-3","type":"valkeycluster","label":"valkey-3","frameId":"lab-valkey-cluster","x":830,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-trafficsim","type":"trafficsim","label":"trafficsim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-valkey-cluster","type":"valkeycluster","label":"lab-valkey","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [
    {"id":"lab-ts-edge","from":{"node":"lab-valkey-cluster","port":"bottom"},"to":{"node":"lab-trafficsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labValkeyCluster4Design is a 4-node Valkey Cluster — one more than the
// minimum, specifically so the "Growing a Cluster" lab has a real fourth
// shard to remove and re-add rather than needing to shrink below 3 (Valkey
// Cluster's own minimum for gossip-based quorum).
var labValkeyCluster4Design = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-vk-1","type":"valkeycluster","label":"valkey-1","frameId":"lab-valkey-cluster","x":574,"y":66},
    {"id":"lab-vk-2","type":"valkeycluster","label":"valkey-2","frameId":"lab-valkey-cluster","x":702,"y":66},
    {"id":"lab-vk-3","type":"valkeycluster","label":"valkey-3","frameId":"lab-valkey-cluster","x":830,"y":66},
    {"id":"lab-vk-4","type":"valkeycluster","label":"valkey-4","frameId":"lab-valkey-cluster","x":958,"y":66},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-trafficsim","type":"trafficsim","label":"trafficsim-01","x":300,"y":220}
  ],
  "frames": [
    {"id":"lab-valkey-cluster","type":"valkeycluster","label":"lab-valkey","x":560,"y":20,"w":528,"h":138}
  ],
  "edges": [
    {"id":"lab-ts-edge","from":{"node":"lab-valkey-cluster","port":"bottom"},"to":{"node":"lab-trafficsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labValkeyStandaloneDesign is a single standalone Valkey node + Intranet —
// no clustering involved, for labs about core Valkey operation (persistence,
// memory, ACLs, transactions, diagnostics, backup) that apply just as much
// to a lone instance as to any cluster member.
var labValkeyStandaloneDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-valkey","type":"valkey","label":"valkey-1","x":300,"y":40},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-trafficsim","type":"trafficsim","label":"trafficsim-01","x":300,"y":220}
  ],
  "frames": [],
  "edges": [
    {"id":"lab-ts-edge","from":{"node":"lab-valkey","port":"bottom"},"to":{"node":"lab-trafficsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

// labValkeyReplicationDesign is two standalone Valkey nodes + Intranet — a
// blank pair, deliberately with no replication wired up at deploy time (unlike
// every clustered lab, this app's provisioning never links standalone Valkey
// nodes together). The "Standalone Replication" and "Sentinel" labs have the
// learner establish that relationship themselves with REPLICAOF, run entirely
// from each node's own terminal — no backend plumbing needed for either lab.
var labValkeyReplicationDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-valkey-a","type":"valkey","label":"valkey-a","x":300,"y":40},
    {"id":"lab-valkey-b","type":"valkey","label":"valkey-b","x":460,"y":40},
    {"id":"lab-vnc","type":"vnc","label":"Ubuntu VNC","os":"ubuntu","osVersion":"24.04","arch":"amd64","x":40,"y":220},
    {"id":"lab-trafficsim","type":"trafficsim","label":"trafficsim-01","x":300,"y":220}
  ],
  "frames": [],
  "edges": [
    {"id":"lab-ts-edge","from":{"node":"lab-valkey-a","port":"bottom"},"to":{"node":"lab-trafficsim","port":"top"},"type":"directional"}
  ],
  "view": {"x":0,"y":0,"z":1}
}`)

var valkeyClusterLabs = []Lab{
	{
		ID:          "valkey-hash-slots",
		Title:       "Hash Slot Routing & MOVED Redirects",
		Description: "Every key belongs to exactly one of 16384 hash slots, and every slot belongs to exactly one node. Connect to the wrong one and watch Valkey tell you where to go instead of just answering.",
		Difficulty:  "Beginner",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Sharding & Routing",
		TimeLimit:   "2h",
		LectureNotes: `There's no query planner deciding where data lives

In a Patroni or repmgr cluster, every node has the entire dataset — replication copies it everywhere, and any node can theoretically answer any query. A Valkey Cluster is fundamentally different: the keyspace is split into 16384 hash slots, and each slot is owned by exactly one node (in this lab's all-master cluster, roughly 16384/3 slots per node). A key that hashes to a slot your node doesn't own simply isn't there — there's no cross-node forwarding of the data itself.

CLUSTER KEYSLOT: the same hash, every time

"CLUSTER KEYSLOT <key>" runs the exact CRC16-based hash function Valkey Cluster uses internally to decide slot ownership for that key — deterministic, so the same key always maps to the same slot regardless of which node you ask. "CLUSTER NODES" (or "CLUSTER SHARDS") shows which node currently owns which slot ranges.

MOVED: a redirect, not an error about your key

Send a command for a key whose slot your node doesn't own, and instead of failing or silently forwarding it, Valkey replies "MOVED <slot> <ip>:<port>" — telling a smart client exactly which node actually owns that slot. A raw "valkey-cli" (no "-c") just prints the MOVED reply and stops. "valkey-cli -c" is cluster-aware: it follows that redirect automatically and re-issues the command against the right node, transparently. Real application clients (redis-py, Jedis, node-redis in cluster mode) all do this same MOVED-following internally — this lab has you watch the raw mechanics smart clients normally hide.

Hash tags: when you need related keys on the same node

Multi-key operations only work if every key involved maps to the same slot. "{tag}" syntax lets you force that: "user:{42}:profile" and "user:{42}:orders" both hash on just the substring inside the braces ("42"), landing on the same slot even though the full key names differ — the standard pattern for keeping related keys co-located in a sharded cluster.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "route-a-key",
				Title: "Find a key's slot, then write it correctly",
				Instructions: "Open a terminal on any Valkey node.\n\n" +
					"Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT lab:hello` to see which slot the key `lab:hello` hashes to.\n\n" +
					"Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and find which node owns that slot (the ranges after each master's line).\n\n" +
					"If it isn't the node you're on, try writing it directly anyway: `valkey-cli -a valkey_password --no-auth-warning SET lab:hello world` " +
					"— you should see a `MOVED` reply instead of `OK`.\n\n" +
					"Now write it the way a real client would: `valkey-cli -c -a valkey_password --no-auth-warning SET lab:hello world` — cluster mode " +
					"(`-c`) follows the redirect for you. Click Check Work.",
				Hint: "If your node happens to already own that slot, `SET` will just succeed with `OK` on the first try — that's fine, still run the `-c` version so the key definitely ends up set.",
			},
		},
	},
	{
		ID:          "valkey-resharding",
		Title:       "Live Resharding Between Masters",
		Description: "Move a range of hash slots — and the keys in them — from one master to another while the cluster stays up and serving traffic.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Sharding & Routing",
		TimeLimit:   "2h",
		LectureNotes: `Resharding is the defining cluster-mode operation

Nothing about a standalone Valkey node prepares you for this — it only exists because the keyspace is split across nodes at all. Resharding moves a range of hash slots (and every key currently in them) from one master to another, live, without taking the cluster offline or losing data.

What actually happens during a slot's move

For each key in a slot being moved, the source node streams it to the destination and only then removes its own copy — one slot fully migrates before the next one starts. Clients that ask for a key mid-migration get steered correctly the whole time (an "ASK" redirect for a key that's already moved, similar in spirit to "MOVED" but for a slot still mid-transfer) — this lab won't have you observe that directly, but it's why resharding is safe to run against a live cluster rather than something that requires a maintenance window.

valkey-cli --cluster reshard: the operator's tool

"valkey-cli --cluster reshard <host:port> --cluster-from <source-id> --cluster-to <dest-id> --cluster-slots <n> --cluster-yes" moves exactly <n> slots from one specific master to another, non-interactively. You name nodes by their cluster ID (from "CLUSTER NODES"), not by hostname — every node in the cluster already knows every other member's ID from gossip, so the source node doesn't need any out-of-band coordination to carry this out.

Why you'd actually do this

The obvious case is rebalancing after adding a new master to a cluster that's been running for a while (an empty new node owns nothing until you reshard slots onto it). The other real case is deliberately steering load: if key access patterns turn out uneven across your keyspace, moving slots is how you rebalance actual request volume, not just slot count.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "reshard-slots",
				Title: "Move slots from one master to another",
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` to list the three masters and their node IDs " +
					"(the long hex string at the start of each line) and current slot ranges.\n\n" +
					"Pick a source and a destination master, then run:\n\n" +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <source-id> --cluster-to <dest-id> --cluster-slots 1000 --cluster-yes`\n\n" +
					"on the source node.\n\n" +
					"Once it finishes, run `CLUSTER NODES` again and confirm the destination master's slot ranges grew and the source's shrank. Click Check Work.",
				Hint: "`--cluster-slots` takes a plain count, not a range — 1000 is enough to be obviously different from the ~1-slot imbalance a fresh 3-way split naturally has (16384 doesn't divide evenly by 3).",
			},
		},
	},
	{
		ID:          "valkey-manual-failover",
		Title:       "Building Replica Topology & Manual Failover",
		Description: "This cluster started as three masters with no replicas at all. Empty one out via resharding, watch it automatically become a replica, then promote it back with a manual failover.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Replication & Failover",
		TimeLimit:   "2h",
		LectureNotes: `This cluster has zero fault tolerance right now

"valkey-cli --cluster create ... --cluster-replicas 0" (what provisioned this lab's cluster, and Stack Designer's default) makes every node a master with its own slice of the keyspace — and no replicas at all. If any one master were to actually disappear, its slots would become unreachable with nothing to take over: cluster_state flips to "fail" and stays that way until that master comes back or you manually intervene. Production Valkey Cluster deployments almost always run with at least one replica per master (--cluster-replicas 1 or higher) specifically to avoid this.

A master with zero slots automatically becomes a replica

You don't need to run any special "make this a replica" command by hand. Reshard every slot away from a master — leaving it owning nothing — and Valkey Cluster automatically reconfigures it as a replica of another master on its own, the moment it notices it holds no slots. This is exactly how you'd retrofit replication onto an all-master cluster that's already running in production: reshard a node empty, and it becomes available as a replica without ever being taken down.

CLUSTER FAILOVER: a planned handover, not a crash

Once a replica exists, "CLUSTER FAILOVER" — run from the replica itself, not the master — requests a clean, coordinated promotion: the replica confirms it's caught up, the master stops accepting writes for a brief moment, and the replica takes over the master's slots and role. This is a planned switchover — a voluntary handover between two healthy nodes, not a reaction to one disappearing.

Why this lab doesn't simulate a crash

Simulating an actual node failure convincingly needs the failure to last long enough for the survivors' gossip-based failure detector to notice (this cluster's cluster-node-timeout is 5 seconds) — reliably keeping a node down for that long from inside its own container (rather than stopping the container from outside, which a real operator could do but a lab terminal session can't) turns out to be far less clean than it sounds. A manual, voluntary failover exercises the same promotion mechanics without needing a contrived crash simulation.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "build-replica",
				Title: "Empty a master to turn it into a replica",
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and note the three masters' node IDs and slot counts.\n\n" +
					"Pick one to empty out (the \"source\") and one to receive its slots (the \"destination\"). On the source node, run:\n\n" +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <source-id> --cluster-to <dest-id> --cluster-slots <however many the source currently owns> --cluster-yes`\n\n" +
					"— moving ALL of its slots, not just some.\n\n" +
					"Once it owns zero slots, run `CLUSTER NODES` again: it should now show up with a `slave` flag instead of `master`, pointing at the destination — automatically, with nothing else for you to run. Click Check Work.",
				Hint: "If it still shows as `master` with 0 slots and hasn't converted, give it a few seconds — the automatic slave conversion happens on the next cluster cron cycle, not the instant the last slot moves.",
			},
			{
				ID:    "manual-failover",
				Title: "Promote the replica back with CLUSTER FAILOVER",
				Instructions: "Open a terminal on the node that just became a replica (from the previous step) and run `valkey-cli -a valkey_password --no-auth-warning CLUSTER FAILOVER`.\n\n" +
					"Wait a few seconds, then run `CLUSTER NODES` on any node and confirm that same node now shows a `master` flag and owns the " +
					"slots its former master used to hold — and that former master now shows `slave` instead. Click Check Work.",
				Hint: "Run CLUSTER FAILOVER from the replica itself, not the master — it's the replica that requests its own promotion.",
			},
		},
	},
	{
		ID:          "valkey-persistence",
		Title:       "Persistence Trade-offs: RDB vs AOF Durability",
		Description: "Every write's durability guarantee is a config knob, not a fixed property of Valkey — and tightening it has a real, measurable performance cost.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Persistence & Durability",
		TimeLimit:   "2h",
		LectureNotes: `Two independent persistence mechanisms, not one

Valkey can persist to disk two different ways at once. RDB is a point-in-time binary snapshot of the whole dataset (what "save" points and BGSAVE produce) — compact and fast to load, but only as current as the last snapshot. AOF (append-only file, enabled by default in this app's clusters and standalone nodes) logs every write command as it happens — more current, but a larger file that takes longer to replay on startup. Production Valkey almost always runs both together, exactly like this lab's node does.

appendfsync: the actual durability dial

AOF logs writes to a buffer immediately, but "appendfsync" controls when that buffer actually reaches disk: "always" (fsync after every single write — maximum durability, most overhead), "everysec" (the default — fsync once a second, batching writes, at most ~1 second of loss on a crash), or "no" (let the OS decide when to flush — fastest, but a crash can lose much more than a second of writes). This isn't a one-time setup decision; it's live-tunable with "CONFIG SET" and takes effect immediately.

The cost of "always" is real and measurable, not theoretical

Fsyncing after every write means every write now waits on a disk round-trip before it's acknowledged. Valkey's own latency monitor ("CONFIG SET latency-monitor-threshold") tracks this as its own event class — "aof-fsync-always" — separate from a generic "command" latency spike. Seeing that event class populate the moment you flip to "always" is direct evidence of the trade-off, not just something the documentation claims.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "tune-fsync",
				Title: "Switch to maximum durability",
				Instructions: "Open a terminal on the Valkey node.\n\n" +
					"Confirm the default first: `valkey-cli -a valkey_password --no-auth-warning CONFIG GET appendfsync` (should show `everysec`).\n\n" +
					"Now tighten it: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET appendfsync always`.\n\n" +
					"Confirm it took with `CONFIG GET appendfsync` again. Click Check Work.",
				Hint: "CONFIG SET takes effect immediately and persists only in memory — a real deployment would also update the config file so it survives a restart, but that's outside what this lab checks.",
			},
			{
				ID:    "observe-durability-cost",
				Title: "Watch the fsync cost show up in latency monitoring",
				Instructions: "Turn on Valkey's latency monitor: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET latency-monitor-threshold 1` (track anything over 1ms).\n\n" +
					"Generate some writes: `valkey-cli -a valkey_password --no-auth-warning SET durability:test hello`, then a few more with different key names.\n\n" +
					"Run `valkey-cli -a valkey_password --no-auth-warning LATENCY HISTORY aof-fsync-always` — you should see recorded events. Click Check Work.",
				Hint: "If LATENCY HISTORY comes back empty, make sure step one's CONFIG SET appendfsync always actually applied — writes under `everysec` don't fsync per-command, so they won't generate this specific event class.",
			},
		},
	},
	{
		ID:          "valkey-memory-eviction",
		Title:       "Memory Management & Eviction Policies",
		Description: "Give Valkey a hard memory ceiling and a policy for what to throw away once it's hit — then actually hit it and watch real keys get evicted.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Memory & Performance",
		TimeLimit:   "2h",
		LectureNotes: `Unbounded by default

"maxmemory 0" (the default) means Valkey will grow to consume however much memory the host allows — fine for a lab, dangerous in production, where an unbounded cache-like workload can take down the whole box. Setting a real "maxmemory" ceiling is one of the first things a production deployment needs, and it needs a policy alongside it for what happens once that ceiling is hit.

maxmemory-policy: eviction isn't automatic just because a limit exists

With the default "noeviction", hitting the ceiling doesn't free anything — write commands simply start failing with an OOM error, keeping every existing key intact but making the instance read-only for new data until something's cleared out (whether by TTL expiry or you raising the limit). "allkeys-lru" instead evicts the least-recently-used key to make room, which is what most cache-shaped workloads actually want. Other policies narrow which keys are eligible (only ones with a TTL set, random selection, LFU frequency instead of recency) — the right choice depends entirely on whether Valkey is your source of truth or a cache in front of one.

Why the baseline matters more than you'd expect

Valkey itself — its own process, connected-client buffers, replication backlog if any — already consumes several megabytes before a single one of your keys exists. Setting "maxmemory" below that baseline doesn't leave any room for eviction to work with; every write fails immediately regardless of policy. This lab has you set a ceiling with real headroom above that floor, so eviction has actual keys to reclaim from.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "configure-eviction",
				Title: "Set a memory ceiling and an LRU eviction policy",
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CONFIG SET maxmemory 12mb`.\n\n" +
					"Then: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET maxmemory-policy allkeys-lru`.\n\n" +
					"Confirm both with `CONFIG GET maxmemory` and `CONFIG GET maxmemory-policy`. Click Check Work.",
				Hint: "12mb leaves real headroom above Valkey's own baseline memory usage (a few MB just for the process itself) — a smaller ceiling can reject every write outright instead of giving eviction anything to work with.",
			},
			{
				ID:    "trigger-eviction",
				Title: "Fill past the ceiling and watch real eviction happen",
				Instructions: "Write enough data to exceed the ceiling — a shell loop is the fastest way:\n\n" +
					"`for i in $(seq 1 2000); do valkey-cli -a valkey_password --no-auth-warning SET \"evict:test:$i\" \"$(head -c 5000 /dev/urandom | base64)\" >/dev/null; done`\n\n" +
					"Once it finishes, run `valkey-cli -a valkey_password --no-auth-warning INFO stats | grep evicted_keys` — it should be well above 0. Click Check Work.",
				Hint: "If evicted_keys is still 0, confirm maxmemory-policy is actually allkeys-lru (not the default noeviction) — with noeviction, writes past the ceiling just fail instead of evicting anything.",
			},
		},
	},
	{
		ID:          "valkey-acl",
		Title:       "Fine-Grained Access Control with ACLs",
		Description: "This cluster uses one shared password with full access to everything by default. Create a user that can only read a specific key pattern, and prove the restriction actually holds.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Security & Access Control",
		TimeLimit:   "2h",
		LectureNotes: `Beyond a single shared password

"requirepass" (what "-a valkey_password" authenticates against everywhere else) is an all-or-nothing gate: anyone with the password can run any command against any key. Valkey's ACL system is the real access-control layer underneath that — multiple named users, each with their own password and their own precisely scoped permissions, the same system this app's own PMM integration already uses under the hood to create a read-only monitoring user.

Three things an ACL rule restricts independently

"ACL SETUSER <name> on >password <key-pattern> <command-permission>" combines three separate restrictions: whether the user can authenticate at all ("on"/"off"), which keys they can touch ("~app:*" — a glob pattern, "~*" for unrestricted), and which commands they're allowed to run ("+@read" grants the whole read-only command category; "+get" would grant only GET specifically). A user can be read-only on one key pattern and have no access at all outside it — both restrictions apply simultaneously, not as alternatives.

Why this matters operationally

A monitoring tool, a reporting job, or a third-party integration should never hold a credential that can run FLUSHALL or read keys outside its own namespace — even though that's exactly what happens by default if every integration just shares the same admin password. ACLs are how you actually give each consumer of a shared Valkey instance only the access it needs.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "create-restricted-user",
				Title: "Create a read-only user scoped to one key pattern",
				Instructions: "First, put some data in the pattern this user will be allowed to read: `valkey-cli -a valkey_password --no-auth-warning SET app:config hello`.\n\n" +
					"Now create the restricted user: `valkey-cli -a valkey_password --no-auth-warning ACL SETUSER app_readonly on >apppass \\~app:* +@read`.\n\n" +
					"Confirm with `valkey-cli -a valkey_password --no-auth-warning ACL LIST` — you should see a line for app_readonly. Click Check Work.",
				Hint: "The `~` before the key pattern needs escaping (`\\~`) in most shells so it isn't interpreted specially — if ACL LIST doesn't show the pattern you expected, check for that.",
			},
			{
				ID:    "verify-enforcement",
				Title: "Prove the restriction actually holds",
				Instructions: "As the new user, confirm reading inside the pattern works:\n\n" +
					"`valkey-cli --user app_readonly -a apppass --no-auth-warning GET app:config` (should return `hello`, no error).\n\n" +
					"Now confirm writing is denied even inside the pattern:\n\n" +
					"`valkey-cli --user app_readonly -a apppass --no-auth-warning SET app:config bye` (should return a NOPERM error). Click Check Work.",
				Hint: "If SET succeeds instead of erroring, double-check the user was actually created with +@read (read-only), not +@all or +@write.",
			},
		},
	},
	{
		ID:          "valkey-transactions",
		Title:       "Transactions & Atomic Scripting",
		Description: "MULTI/EXEC batches commands; Lua scripting guarantees true atomicity. Use both, and see exactly where the difference matters.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Transactions & Scripting",
		TimeLimit:   "2h",
		LectureNotes: `MULTI/EXEC: queued, then run back-to-back

Commands issued between "MULTI" and "EXEC" don't execute immediately — they're queued client-side (you'll see "QUEUED" for each) and only actually run, one after another with nothing else interleaved, when "EXEC" arrives. This guarantees no OTHER client's command can execute in between yours — but it does not evaluate any logic; every queued command runs unconditionally in order.

Lua scripting: real conditional atomicity

"EVAL" runs an entire Lua script as a single atomic step — including reading a value, making a decision based on it, and writing a result, with a guarantee that no other command can run on the server in between any of those steps. This is strictly more powerful than MULTI/EXEC for "check-then-act" logic: a queued MULTI/EXEC transaction can't branch on a value it reads mid-transaction, but a Lua script can, atomically.

Why this distinction matters for real application code

"Increment a counter, but only if it's still under some limit" can't be expressed correctly with plain INCR (a second client could increment between your GET and your INCR, a classic race condition) and MULTI/EXEC alone doesn't help either, since it can't make the increment conditional on what it read. A Lua script that does the check and the increment in one EVAL call is the actual correct, race-free way to implement that pattern in Valkey.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "run-transaction",
				Title: "Batch multiple writes with MULTI/EXEC",
				Instructions: "Open a terminal and run `valkey-cli -a valkey_password --no-auth-warning` to get an interactive prompt (so MULTI/EXEC share one connection).\n\n" +
					"Inside it, run: `MULTI`, then `SET tx:counter 10`, then `INCR tx:counter`, then `EXEC` — you should see both queued commands' results returned together.\n\n" +
					"Exit with `exit`.\n\n" +
					"Confirm from outside: `valkey-cli -a valkey_password --no-auth-warning GET tx:counter` should show `11`. Click Check Work.",
				Hint: "MULTI/EXEC only works within a single connection — running each command as a separate `valkey-cli ... COMMAND` invocation opens a new connection each time and won't queue anything.",
			},
			{
				ID:    "atomic-lua",
				Title: "Enforce a limit atomically with a Lua script",
				Instructions: "Run this EVAL, which only increments a counter if it's still under 5, atomically:\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning EVAL \"local v = tonumber(redis.call('GET', KEYS[1]) or '0'); if v < 5 then return redis.call('INCR', KEYS[1]) else return -1 end\" 1 tx:limited`\n\n" +
					"Run it six times in a row — the first five should return 1 through 5, and the sixth should return -1 (the limit held). Click Check Work.",
				Hint: "Each EVAL call is one atomic step — run it as six separate invocations (six separate commands), not once; that's what proves the limit check and the increment never race against each other across calls.",
			},
		},
	},
	{
		ID:          "valkey-slowlog",
		Title:       "Diagnosing Performance with Slowlog & Latency Monitoring",
		Description: "A real production incident starts with \"why is Valkey slow right now\" — these are the two built-in tools that actually answer that question.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Observability & Diagnostics",
		TimeLimit:   "2h",
		LectureNotes: `Slowlog: a rolling log of individual slow commands

"CONFIG SET slowlog-log-slower-than <microseconds>" sets the threshold above which a command's execution time gets recorded (0 logs everything, useful for this lab; a real deployment would set something like 10000 — 10ms). "SLOWLOG GET" returns the most recent entries: timestamp, duration, the exact command and arguments, and the client that issued it — everything you need to identify exactly what ran and who ran it.

KEYS *: the classic anti-pattern slowlog exists to catch

"KEYS *" (and pattern variants like "KEYS user:*") scan the entire keyspace synchronously, blocking every other client for however long that scan takes — on a keyspace with millions of keys, that can be seconds of total unavailability. ("SCAN" is the safe, non-blocking, cursor-based alternative for the same use case.) This is one of the single most common real-world causes of a Valkey instance suddenly looking "frozen," and it's exactly the kind of thing this lab has you catch in the slowlog.

Latency monitoring: categorized, not just per-command

"LATENCY HISTORY <event>" and "LATENCY LATEST" track latency by event class — "command" for slow individual commands (overlapping with slowlog, but from a different angle), "fork" for background-save fork time, "aof-fsync-always" for the durability cost of fsyncing on every write, and others. Where slowlog tells you which specific command was slow, latency monitoring tells you which class of internal operation is contributing to overall latency — the two tools answer related but different diagnostic questions.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "catch-slow-command",
				Title: "Catch a KEYS * scan in the slowlog",
				Instructions: "Log everything for this exercise: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET slowlog-log-slower-than 0`.\n\n" +
					"Populate enough keys that a full scan is actually slow (a Lua loop is far faster than one valkey-cli call per key):\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning EVAL \"for i=1,50000 do redis.call('SET', 'bulk:'..i, 'v') end\" 0`\n\n" +
					"Now run the anti-pattern: `valkey-cli -a valkey_password --no-auth-warning KEYS '*' >/dev/null` (redirected, so it doesn't print 50000 lines).\n\n" +
					"Confirm it's in the log: `valkey-cli -a valkey_password --no-auth-warning SLOWLOG GET 5`. Click Check Work.",
				Hint: "Look for an entry whose command is `KEYS` `*` — with only a few thousand keys the scan can finish in under a " +
					"millisecond and never even reach the slowlog threshold, which is why this step uses 50000.",
			},
			{
				ID:    "latency-diagnostics",
				Title: "Confirm it shows up in latency monitoring too",
				Instructions: "Turn on latency tracking: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET latency-monitor-threshold 1`.\n\n" +
					"Run the same `KEYS '*'` scan again.\n\n" +
					"Check `valkey-cli -a valkey_password --no-auth-warning LATENCY HISTORY command` — it should show at least one recorded event. Click Check Work.",
				Hint: "latency-monitor-threshold has to be set before the slow command runs, not after — it only records events that happen while monitoring is active.",
			},
		},
	},
	{
		ID:          "valkey-backup-restore",
		Title:       "Backup & Integrity Verification",
		Description: "Taking a backup is easy to get wrong in a way you don't discover until the moment you actually need it. Take one, then actually verify it's restorable.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Persistence & Durability",
		TimeLimit:   "2h",
		LectureNotes: `BGSAVE: a consistent snapshot without blocking

"BGSAVE" forks a child process that writes the current dataset to "dump.rdb" (the file "CONFIG GET dbfilename" / "CONFIG GET dir" point at) while the main process keeps serving requests normally — the snapshot is a consistent point-in-time view despite writes continuing during the save. "LASTSAVE" returns the Unix timestamp of the most recent successful save, the simplest way to confirm a BGSAVE you just triggered actually completed.

A backup you haven't verified isn't a backup yet

The failure mode that actually burns people isn't "we forgot to take backups" — it's discovering during a real recovery that the backup file was truncated, corrupted, or from the wrong dataset entirely. "valkey-check-rdb" parses an RDB file's structure and checksum without needing a running server to load it into, reporting exactly how many keys it found and whether the file is structurally sound — this is the step that turns "a file that's probably a backup" into "a backup you've confirmed you can actually restore."

Why check the file, not just that BGSAVE returned success

BGSAVE returning "Background saving started" only means the save was scheduled — it says nothing about whether the write to disk actually succeeded (a full disk, a permissions issue, or a crash mid-write could all leave a bad file behind while the command itself appeared to accept). Checking the resulting file directly is the only way to know the backup you're relying on is real.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "take-backup",
				Title: "Take a consistent backup",
				Instructions: "Write some data worth backing up: `valkey-cli -a valkey_password --no-auth-warning SET backup:marker hello`.\n\n" +
					"Trigger a snapshot: `valkey-cli -a valkey_password --no-auth-warning BGSAVE`.\n\n" +
					"Wait a couple seconds, then copy it to a separate backup location (simulating off-node storage):\n\n" +
					"`cp /var/lib/valkey/data/dump.rdb /var/lib/valkey/data/backup-dump.rdb`\n\n" +
					"Click Check Work.",
				Hint: "Check Work confirms LASTSAVE advanced (proof BGSAVE actually completed, not just that you ran the command) and that the copy exists.",
			},
			{
				ID:    "verify-backup",
				Title: "Verify the backup file is actually restorable",
				Instructions: "Run `valkey-check-rdb /var/lib/valkey/data/backup-dump.rdb`.\n\n" +
					"It should report \"RDB looks OK!\" and how many keys it found. Click Check Work.",
				Hint: "If it reports a checksum or structural error, the copy didn't finish cleanly — re-run the cp from the previous step and try again.",
			},
		},
	},
	{
		ID:          "valkey-cluster-resize",
		Title:       "Growing a Cluster: Adding & Removing a Shard Live",
		Description: "Shrink this 4-shard cluster down to 3, then grow it back — the same operations behind resizing a real cluster to match changing load, without any downtime.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Cluster Administration",
		TimeLimit:   "2h",
		LectureNotes: `Resharding moves slots between existing members — this is different

Resharding moves slots between masters that are already part of the cluster. This lab changes cluster membership itself: removing a shard entirely, and later bringing a (or another) node in as a brand new member. A shard can only be removed once it owns zero slots — reshard everything off it first, the same way retrofitting replication onto an all-master cluster starts by emptying a master out.

del-node: a real departure, not just going quiet

"valkey-cli --cluster del-node <any-live-host:port> <node-id>" sends CLUSTER FORGET to every remaining member and a CLUSTER RESET to the departing node itself — it's actually removed from the cluster's membership, not just sitting there empty. The departing node's own process keeps running (this lab doesn't stop it), but it now reports cluster_state:fail, since it's not part of any cluster at all anymore.

The ~60 second reintroduction window — cosmetic on the others, not a real block

Immediately after a CLUSTER FORGET, every OTHER remaining member temporarily refuses to re-learn about that exact node ID via gossip, for about a minute — a safety measure against a node flapping in and out right after being deliberately removed. Running CLUSTER NODES on one of those other three during that window won't show the rejoining node yet. But this doesn't actually block anything: the readded node itself already has the full picture from its own CLUSTER MEET handshake, and resharding onto it by ID works immediately — the ban only delays when the OTHER members' own local view catches up, not whether the node is functionally back. In production this practically never matters anyway (you're normally adding a genuinely new node, not the one you just removed).

add-node: CLUSTER MEET, then you decide what it does

"valkey-cli --cluster add-node <new-host:port> <existing-host:port>" introduces a node to the cluster via gossip — it joins as an empty master, owning nothing, until you explicitly reshard slots onto it (or add it with --cluster-replica to make it a replica instead). Membership and workload are two separate steps; joining the cluster doesn't automatically give a new node any of the keyspace.`,
		DesignTemplate: labValkeyCluster4Design,
		Steps: []LabStep{
			{
				ID:    "shrink-cluster",
				Title: "Empty and remove the fourth shard",
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and note all four node IDs and slot ranges.\n\n" +
					"Pick one to remove. Reshard all of its slots onto another master:\n\n" +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <its-id> --cluster-to <another-id> --cluster-slots <however many it owns> --cluster-yes`\n\n" +
					"Once it owns zero slots, remove it entirely:\n\n" +
					"`valkey-cli --cluster del-node 127.0.0.1:6379 <its-id> -a valkey_password --no-auth-warning`\n\n" +
					"Confirm with `CLUSTER NODES` that only three members remain and all 16384 slots are still covered between them. Click Check Work.",
				Hint: "del-node only accepts a target that currently owns zero slots — if it refuses, double-check the reshard actually moved everything (check its slot range is empty in CLUSTER NODES first).",
			},
			{
				ID:    "grow-cluster",
				Title: "Bring it back and give it work",
				Instructions: "Re-add it right away:\n\n" +
					"`valkey-cli --cluster add-node <removed-node-ip>:6379 <any-remaining-node-ip>:6379 -a valkey_password --no-auth-warning`\n\n" +
					"Confirm on the readded node itself (not one of the other three — see the hint) with `CLUSTER NODES` that it now sees all four members.\n\n" +
					"It rejoined with zero slots — give it some (this works immediately, even before the rest of the cluster has fully caught up — see the hint):\n\n" +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <a-busy-node-id> --cluster-to <its-id> --cluster-slots 2000 --cluster-yes`\n\n" +
					"Click Check Work.",
				Hint: "The three nodes that were already in the cluster temporarily refuse to re-learn this exact node ID via gossip for " +
					"about a minute after removing it (a safety measure), so CLUSTER NODES on THEM may not show it for a while — but the " +
					"readded node itself already knows the full cluster from its own CLUSTER MEET handshake, and resharding onto it by ID " +
					"works right away regardless. No need to wait.",
			},
		},
	},
	{
		ID:          "valkey-cross-slot",
		Title:       "Cross-Slot Operations & Hash Tags",
		Description: "Multi-key commands only work if every key involved lives in the same slot. Hit the error every Cluster-mode client eventually hits, then fix it the standard way.",
		Difficulty:  "Beginner",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Sharding & Routing",
		TimeLimit:   "2h",
		LectureNotes: `Multi-key commands need every key on the same node

MSET, MGET, and transactions (MULTI/EXEC) that touch multiple keys all require every key involved to resolve to the same node — because Valkey Cluster has no cross-node coordination for a single command. There's no query planner that could split "MSET a 1 b 2" across two nodes and merge the result; it's simply not attempted.

CROSSSLOT: a hard stop, not a redirect

If your keys hash to different slots, cluster mode can't fix this the way it fixes a single-key MOVED — there's no one node to redirect the whole command to. Instead you get a flat "CROSSSLOT Keys in request don't hash to the same slot" error, whether or not you're using "-c". This is the sharp edge every real Cluster-mode client integration eventually discovers the first time application code assumes multi-key commands "just work" the way they do against a single Valkey instance.

Hash tags: forcing co-location on purpose

Wrapping part of a key name in braces — "user:{42}:profile" — tells Valkey to hash only the substring inside the braces when deciding the key's slot, ignoring the rest of the key name. "user:{42}:profile" and "user:{42}:orders" then land on the same slot even though they're different keys, because only "42" gets hashed. This is the standard, deliberate pattern for any related keys — an entity's various pieces of data — that your application needs to read, write, or transact on together in one command.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "hit-crossslot-error",
				Title: "Hit a real CROSSSLOT error",
				Instructions: "Open a terminal on any Valkey node.\n\n" +
					"Run `valkey-cli -c -a valkey_password --no-auth-warning MSET plain:a 1 plain:b 2` — with two ordinary, unrelated key names, " +
					"these almost certainly hash to different slots, so you should see a `CROSSSLOT` error (even with `-c` — there's no single node to redirect to). Click Check Work.",
				Hint: "If it succeeds instead of erroring, you got unlucky and both keys landed on the same slot by chance — try a different pair of key names.",
			},
			{
				ID:    "fix-with-hash-tags",
				Title: "Fix it with a hash tag",
				Instructions: "Run `valkey-cli -c -a valkey_password --no-auth-warning MSET \"tagged:{demo}:a\" 1 \"tagged:{demo}:b\" 2` " +
					"— both keys share the same `{demo}` hash tag, so they hash to the same slot and the command succeeds.\n\n" +
					"Confirm: `valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT \"tagged:{demo}:a\"` and `CLUSTER KEYSLOT \"tagged:{demo}:b\"` " +
					"should print the same slot number. Click Check Work.",
				Hint: "The braces have to contain the exact same substring in both keys — `{demo}` and `{Demo}` (different case) would hash to different slots.",
			},
		},
	},
	{
		ID:          "valkey-replication",
		Title:       "Standalone Replication with REPLICAOF",
		Description: "Wire up a plain primary/replica pair by hand — the foundational relationship every other Valkey HA mechanism (Sentinel, and Cluster's own replica-per-shard model) is built on top of.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Replication & Failover",
		TimeLimit:   "2h",
		LectureNotes: `Two independent nodes until you tell one to follow the other

Unlike a Valkey Cluster frame, standalone Valkey nodes are never wired together automatically — the two nodes in this lab start out completely independent, each with its own empty dataset. "REPLICAOF <host> <port>" (run on the node that should become the replica) is the single command that establishes the relationship.

What actually happens on REPLICAOF

The replica connects to the named primary, requests a full sync, receives a snapshot of the primary's entire current dataset, loads it, and then stays connected, applying every subsequent write the primary makes in real time. "INFO replication" on either side shows the live state — role, connection status, and (on the primary) how many replicas are currently connected.

Replicas are read-only by default, and that's enforced, not just documented

Once a node is a replica, direct writes to it fail with a READONLY error (unless you explicitly turn that off, which defeats the point) — the primary is the sole source of truth for anything the replica serves. This matters because it's the thing that makes a replica safe to point read traffic at without worrying about it silently diverging from the primary's data.

The foundation, not the whole HA story

Plain replication alone doesn't include any automatic failover — if the primary disappears, the replica just keeps being a read-only replica of a primary that's gone, forever, until something (a human, or Sentinel) tells it to stop following and become a primary itself.`,
		DesignTemplate: labValkeyReplicationDesign,
		Steps: []LabStep{
			{
				ID:    "establish-replication",
				Title: "Make valkey-b a replica of valkey-a",
				Instructions: "On valkey-a, write some data: `valkey-cli -a valkey_password --no-auth-warning SET repl:marker hello`.\n\n" +
					"On valkey-b, run `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379`.\n\n" +
					"Wait a few seconds for the initial sync, then confirm with `valkey-cli -a valkey_password --no-auth-warning INFO replication` " +
					"on valkey-b that `role:slave` and `master_link_status:up`.\n\n" +
					"Confirm the data arrived: `valkey-cli -a valkey_password --no-auth-warning GET repl:marker` on valkey-b should return `hello`. Click Check Work.",
				Hint: "master_link_status can briefly show `down` right after REPLICAOF while the initial full sync is still in progress — give it a few more seconds and check again.",
			},
			{
				ID:    "verify-read-only-replica",
				Title: "Confirm the replica rejects direct writes",
				Instructions: "On valkey-b, try to write directly: `valkey-cli -a valkey_password --no-auth-warning SET repl:direct nope`.\n\n" +
					"You should get a `READONLY` error — the replica refuses writes that don't come from its primary's replication stream. Click Check Work.",
				Hint: "If the write succeeds instead of erroring, confirm valkey-b actually completed REPLICAOF (check INFO replication shows role:slave) — a plain standalone node accepts writes normally.",
			},
		},
	},
	{
		ID:          "valkey-sentinel",
		Title:       "Automatic Failover with Valkey Sentinel",
		Description: "Plain replication has no opinion about what happens when the primary disappears. Sentinel is the piece that actually watches, decides, and promotes — automatically, with no manual command from you.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Replication & Failover",
		TimeLimit:   "2h",
		LectureNotes: `Sentinel is its own process, monitoring from outside

"valkey-sentinel" isn't a mode of the regular server — it's a separate process (bundled in this lab's image) that connects to a primary, discovers its replicas automatically from the primary's own state, and continuously monitors all of them. A real deployment runs several Sentinel processes (often 3, on separate hosts) so failure decisions are made by quorum rather than trusting any single observer — this lab runs one, enough to see the mechanism work, though production setups reach for more precisely to avoid one Sentinel's view being wrong.

down-after-milliseconds and quorum: how "failed" gets decided

Sentinel doesn't act the instant it can't reach the primary — "down-after-milliseconds" sets how long an unreachable primary must stay unreachable before Sentinel even considers it a candidate for failover ("subjectively down"). With multiple Sentinels, "quorum" additionally requires that many of them to agree before it becomes "objectively down" and failover actually starts — this lab's single Sentinel with quorum 1 skips that cross-checking, which is exactly why production wants more than one.

What actually happens during failover

Once a primary is objectively down, Sentinel picks a replica (preferring the most caught-up one), sends it a command to stop following and become an independent primary, and reconfigures every other known replica to follow the newly promoted node instead. All of this happens automatically, without you running a single manual command — contrast this with Valkey Cluster's manual CLUSTER FAILOVER, which only ever acts on your explicit request.

TILT mode: Sentinel's own safety brake

Sentinel constantly compares wall-clock time against its own internal event loop timing, and if it ever sees a gap far larger than expected — the kind of jump caused by a heavily loaded host, a paused process, or (as in this lab) a primary vanishing abruptly enough to disrupt Sentinel's own timing assumptions — it enters "TILT" mode: a roughly 30-second window where Sentinel deliberately stops making any failover decisions at all, on the theory that its own recent observations might not be trustworthy. This is a deliberate conservatism, not a bug: an automated system that fails over confidently on bad information is more dangerous than one that briefly refuses to act. It's also why this lab's failover takes on the order of 30-40 seconds rather than the sub-second reaction time down-after-milliseconds alone would suggest.

A client's job: ask Sentinel who's primary right now, not hardcode it

Because the primary's identity can change after any failover, real client code doesn't hardcode "connect to valkey-a" — it asks a Sentinel ("SENTINEL GET-MASTER-ADDR-BY-NAME mymaster") for the current primary's address first, every time it needs to (re)connect. This lab has you observe the failover directly rather than build that client logic, but it's the reason Sentinel exists as a separate discovery service instead of just being "replication with a script that restarts things."`,
		DesignTemplate: labValkeyReplicationDesign,
		Steps: []LabStep{
			{
				ID:    "setup-replication-and-sentinel",
				Title: "Wire up replication, then start Sentinel to watch it",
				Instructions: "On valkey-b, run `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379` and wait a few seconds for it to sync.\n\n" +
					"On valkey-b, write a Sentinel config:\n\n" +
					"`cat > /tmp/sentinel.conf <<EOF\nport 26379\nsentinel resolve-hostnames yes\nsentinel monitor mymaster valkey-a 6379 1\nsentinel down-after-milliseconds mymaster 500\nsentinel failover-timeout mymaster 10000\nsentinel auth-pass mymaster valkey_password\nEOF`\n\n" +
					"Start it in the background: `setsid valkey-sentinel /tmp/sentinel.conf > /tmp/sentinel.log 2>&1 < /dev/null &`\n\n" +
					"Confirm it's watching: `valkey-cli -p 26379 SENTINEL MASTERS` should show `mymaster` with `flags` of `master` (healthy). Click Check Work.",
				Hint: "If SENTINEL MASTERS refuses to connect, the sentinel process didn't start — check `cat /tmp/sentinel.log` for why (a common cause is a typo in the config heredoc).",
			},
			{
				ID:    "crash-and-failover",
				Title: "Crash the primary and watch Sentinel promote the replica",
				Instructions: "On valkey-a, run `valkey-cli -a valkey_password --no-auth-warning SHUTDOWN NOSAVE`.\n\n" +
					"This trips Sentinel's own safety brake (\"TILT\" mode — see the hint), so the full failover takes about 30-40 seconds, " +
					"not the 500ms down-after-milliseconds alone would suggest.\n\n" +
					"Wait, then on valkey-b run `valkey-cli -p 26379 SENTINEL MASTERS` again — the `ip` field should now show valkey-b's own " +
					"address, not valkey-a's, meaning Sentinel already promoted it.\n\n" +
					"Confirm directly: `valkey-cli -a valkey_password --no-auth-warning INFO replication` on valkey-b should now show `role:master`. Click Check Work.",
				Hint: "systemd restarts the Valkey service automatically within a second or two of SHUTDOWN — an abrupt enough event that " +
					"Sentinel itself detects a suspicious time jump and enters TILT mode, deliberately pausing all failover decisions for " +
					"about 30 seconds as a safety measure against acting on bad information. That's why this step takes noticeably longer than " +
					"down-after-milliseconds (500ms) alone implies — the delay is Sentinel being cautious, not failing.",
			},
		},
	},
	{
		ID:          "valkey-streams",
		Title:       "Streams & Consumer Groups for Event Processing",
		Description: "A stream is an append-only log, and a consumer group turns it into a work queue — each message goes to exactly one member of the group, with an acknowledgment protocol for recovering work when a consumer dies mid-processing.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Messaging & Streams",
		TimeLimit:   "2h",
		LectureNotes: `A stream is a log, not a queue

"XADD" appends an entry with an auto-generated ID (a millisecond timestamp plus a sequence number, guaranteeing strictly increasing order) and the entry stays in the stream after it's been read — nothing is removed just because someone consumed it. Any number of independent readers can replay the whole history from the beginning with "XRANGE", exactly like reading a log file. That persistence is what separates a Stream from Pub/Sub: a Pub/Sub message that arrives with no subscriber connected is gone forever, but a Stream entry is durable data sitting in the keyspace until you explicitly trim or delete it.

Consumer groups: each entry delivered once per group, not once per reader

Plain "XREAD" lets multiple clients all read the same entries independently — fine for fan-out, useless for splitting work across a pool of workers. "XGROUP CREATE" adds a named cursor that a group of consumers shares: "XREADGROUP GROUP <group> <consumer> ... STREAMS key >" hands out only entries the group hasn't already delivered to *someone*, and every entry goes to exactly one consumer in the group. This is the actual work-queue behavior — the same shape as a message broker's consumer group, built on a data structure you can also just read like a log.

The pending entries list: what "delivered" doesn't mean "done"

The instant "XREADGROUP" hands an entry to a consumer, that entry goes on the group's pending entries list (PEL) — delivered but not yet confirmed processed. "XACK" is the confirmation; only after it does the entry leave the PEL. Until then, "XPENDING" shows exactly which consumer is holding which entry and for how long — the mechanism that makes "did a worker actually finish this" answerable instead of assumed.

XCLAIM: recovering work from a consumer that vanished

If a consumer reads an entry and then crashes before acking, that entry just sits on the PEL forever under its name — nothing automatically reassigns it. "XCLAIM key group new-consumer min-idle-time id" is how another consumer takes over: it transfers ownership of that specific pending entry, but only if it's been idle at least "min-idle-time" milliseconds (a guard against claiming work a consumer is still actively — just slowly — processing). This is the real mechanism a production system uses to recover from a worker dying mid-job, not a special "crash recovery mode" — it's the same XCLAIM you'd use for any kind of rebalancing.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "process-with-group",
				Title: "Create a stream, consume it as a group, and acknowledge",
				Instructions: "Add an entry: `valkey-cli -a valkey_password --no-auth-warning XADD lab:orders '*' order_id 1001 total 42`.\n\n" +
					"Create a consumer group starting from the beginning: `valkey-cli -a valkey_password --no-auth-warning XGROUP CREATE lab:orders lab:processors 0`.\n\n" +
					"Read it as a group member: `valkey-cli -a valkey_password --no-auth-warning XREADGROUP GROUP lab:processors consumer-1 COUNT 1 STREAMS lab:orders '>'` " +
					"— note the entry ID it prints.\n\n" +
					"Confirm it's pending: `valkey-cli -a valkey_password --no-auth-warning XPENDING lab:orders lab:processors` should show 1.\n\n" +
					"Acknowledge it: `valkey-cli -a valkey_password --no-auth-warning XACK lab:orders lab:processors <that-id>`.\n\n" +
					"Confirm XPENDING now shows 0. Click Check Work.",
				Hint: "`'>'` (with quotes, to stop your shell from treating it as a redirect) means \"entries never delivered to this group before\" — it's what makes XREADGROUP a work queue instead of a replay.",
			},
			{
				ID:    "reclaim-stalled-entry",
				Title: "Simulate a crashed consumer and reclaim its work",
				Instructions: "Add a second entry: `valkey-cli -a valkey_password --no-auth-warning XADD lab:orders '*' order_id 1002 total 7`.\n\n" +
					"Read it as consumer-1 but don't acknowledge it — simulating a crash right after pickup:\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning XREADGROUP GROUP lab:processors consumer-1 COUNT 1 STREAMS lab:orders '>'`\n\n" +
					"Wait about 2 seconds so it's genuinely idle, then find it: `valkey-cli -a valkey_password --no-auth-warning XPENDING lab:orders lab:processors - + 10` " +
					"(note the entry ID and idle time).\n\n" +
					"Reclaim it as a different consumer: `valkey-cli -a valkey_password --no-auth-warning XCLAIM lab:orders lab:processors consumer-2 1000 <that-id>`.\n\n" +
					"Run XPENDING again and confirm the entry is now listed under consumer-2, not consumer-1. Click Check Work.",
				Hint: "The `1000` in XCLAIM is min-idle-time in milliseconds — if you claim it before the entry has actually been idle that long, Valkey just returns an empty reply and ownership doesn't change.",
			},
		},
	},
	{
		ID:          "valkey-pubsub",
		Title:       "Pub/Sub & Sharded Pub/Sub in Cluster Mode",
		Description: "Regular PUBLISH in a cluster reaches every subscriber no matter which node they're connected to — at the cost of broadcasting to every node for every message. Sharded Pub/Sub trades that convenience for scoping delivery to just one shard.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Messaging & Streams",
		TimeLimit:   "2h",
		LectureNotes: `Regular PUBLISH doesn't care about slots

Unlike every key-based command, "PUBLISH" and "SUBSCRIBE" have nothing to do with hash slots — a channel name is never hashed to decide ownership. In cluster mode, when any node receives a PUBLISH, it forwards the message to every other node over the cluster bus, and each node delivers it to whichever of its own clients are subscribed. The practical effect: subscribe on any node, publish from any (possibly different) node, and the message still arrives — cluster mode makes Pub/Sub *feel* like a single shared bus even though the keyspace underneath it is sharded.

The cost of that convenience: an O(N) broadcast per message

That forwarding-to-every-node behavior is exactly why it's expensive at scale — a single PUBLISH costs the cluster N-1 extra hops (one to every other master), regardless of whether anyone on those nodes is even subscribed. In a small 3-node cluster that's noise; in a cluster with dozens of shards handling a high message rate, it becomes real, wasted bandwidth and CPU on nodes with zero interested subscribers.

Sharded Pub/Sub: scoping delivery to one shard

"SSUBSCRIBE" and "SPUBLISH" are the shard-scoped equivalents — a shard channel name *is* hashed to a slot exactly like a key, and delivery only happens within that slot's shard (the owning master and its replicas), never broadcast cluster-wide. This is a genuine trade-off, not a strict upgrade: a shard-channel subscriber only sees messages published to nodes in its own shard, so if you need every subscriber cluster-wide to see a message regardless of shard, sharded Pub/Sub is the wrong tool — that's still what regular PUBLISH is for.

Where you connect actually matters now

Because shard channels route by slot, a client has to connect to (or be redirected to) a node that actually owns the channel's slot before SSUBSCRIBE works — try it on the wrong node and you get a MOVED error, the same as any other slot-routed command. Regular SUBSCRIBE has no such restriction; it works identically no matter which node you happen to be connected to.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "cluster-wide-publish",
				Title: "Publish from one node, receive on another",
				Instructions: "On valkey-2, start a subscriber in the background:\n\n" +
					"`setsid valkey-cli -a valkey_password --no-auth-warning SUBSCRIBE lab:announcements > /tmp/pubsub.log 2>&1 < /dev/null &`\n\n" +
					"On valkey-1 — a completely different node — publish: `valkey-cli -a valkey_password --no-auth-warning PUBLISH lab:announcements hello-cluster`.\n\n" +
					"Back on valkey-2, run `cat /tmp/pubsub.log` and confirm `hello-cluster` shows up, even though it was never published from valkey-2 itself. Click Check Work.",
				Hint: "If the log is empty, give it a second — the message has to travel over the cluster bus between nodes, which takes a moment longer than a local Pub/Sub delivery.",
			},
			{
				ID:    "sharded-publish",
				Title: "Confine delivery to one shard with SSUBSCRIBE/SPUBLISH",
				Instructions: "Find which node owns the shard channel: `valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT lab:shardnews` " +
					"gives a slot number; check `CLUSTER NODES` to see which master's range covers it.\n\n" +
					"On that node, start a sharded subscriber:\n\n" +
					"`setsid valkey-cli -a valkey_password --no-auth-warning SSUBSCRIBE lab:shardnews > /tmp/shardpubsub.log 2>&1 < /dev/null &`\n\n" +
					"From the SAME node, publish: `valkey-cli -a valkey_password --no-auth-warning SPUBLISH lab:shardnews hello-shard`.\n\n" +
					"Confirm `cat /tmp/shardpubsub.log` shows `hello-shard`. Click Check Work.",
				Hint: "If you SSUBSCRIBE on the wrong node you'll get a MOVED error immediately instead of subscribing — that's expected, it's the same slot-ownership check every other command gets.",
			},
		},
	},
	{
		ID:          "valkey-keyspace-notifications",
		Title:       "Keyspace Notifications: Building Reactive Applications",
		Description: "Valkey can publish a Pub/Sub event for every write it processes — including the one write your own client never issues directly: a key expiring. This is the mechanism behind cache invalidation and reactive application patterns that don't poll.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Messaging & Streams",
		TimeLimit:   "2h",
		LectureNotes: `Off by default, opt in with a flag string

Keyspace notifications cost a little CPU on every write (Valkey has to check whether anyone might care and publish an event if so), so they're disabled out of the box. "CONFIG SET notify-keyspace-events <flags>" turns specific classes on — "K" for keyspace-prefixed events, "E" for keyevent-prefixed events, and letters like "g" (generic commands), "x" (expired), "$" (string commands) selecting which kinds of operations get published at all. "KEA" turns on everything, which this lab uses for its second step; a real deployment would enable only the specific classes it actually consumes.

Two classes of events, two ways to ask the same question

With "K" enabled, every write to key "foo" publishes to channel "__keyspace@0__:foo" with the event name (e.g. "set") as the message — subscribe to that if you care about *one specific key*. With "E" enabled, every "set" anywhere publishes to channel "__keyevent@0__:set" with the key name as the message — subscribe to that if you care about *one specific kind of event*, regardless of which key it happened to. They're two different ways of slicing the same underlying stream of writes, and you can enable either or both.

The case polling can't cover: expiry

A key expiring isn't a command your client ever issued — it's Valkey's own background process (or a lazy check on next access) removing it. There's no write for your application to observe by normal means, which is exactly why "expired" is its own event class ("x" or "g" depending on exact Valkey version) rather than something layered on top of SET/DEL notifications. Subscribing to "__keyevent@0__:expired" is how a cache invalidation listener finds out a TTL actually elapsed without polling TTL on every key it might care about.

The trade-off: at-most-once delivery, no history

Keyspace notifications are plain Pub/Sub underneath — if no one is subscribed when the event fires, it's gone, and a subscriber that disconnects for a few seconds misses whatever happened in that window. For anything that needs guaranteed, replayable delivery instead of best-effort, that's what Streams are for — the two features solve adjacent but different problems.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "watch-expired-events",
				Title: "React to a key expiring, without polling",
				Instructions: "Turn on expired-key keyevent notifications: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET notify-keyspace-events Ex`.\n\n" +
					"Start a subscriber in the background:\n\n" +
					"`setsid valkey-cli -a valkey_password --no-auth-warning PSUBSCRIBE '__keyevent@0__:expired' > /tmp/notif.log 2>&1 < /dev/null &`\n\n" +
					"Set a key with a short TTL: `valkey-cli -a valkey_password --no-auth-warning SET lab:expiring hello PX 500`.\n\n" +
					"Wait about 2 seconds, then `cat /tmp/notif.log` and confirm `lab:expiring` shows up as the delivered message. Click Check Work.",
				Hint: "The `x` flag specifically means expired-key events — plain `E` alone (without `x` or `g`) won't publish anything when a TTL elapses.",
			},
			{
				ID:    "watch-generic-keyspace-events",
				Title: "React to a specific key, by name, no matter the command",
				Instructions: "Turn on everything: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET notify-keyspace-events KEA`.\n\n" +
					"Start a subscriber scoped to one specific key:\n\n" +
					"`setsid valkey-cli -a valkey_password --no-auth-warning PSUBSCRIBE '__keyspace@0__:lab:tracked' > /tmp/notif2.log 2>&1 < /dev/null &`\n\n" +
					"Write that key: `valkey-cli -a valkey_password --no-auth-warning SET lab:tracked hi`.\n\n" +
					"Confirm `cat /tmp/notif2.log` shows `set` as the delivered message (the event name, not the value). Click Check Work.",
				Hint: "The keyspace-prefixed channel's message payload is the *event name* (`set`, `del`, `expire`...) — if you want the *key name* instead you'd subscribe to the keyevent-prefixed channel like the previous step did.",
			},
		},
	},
	{
		ID:          "valkey-client-side-caching",
		Title:       "Client-Side Caching with RESP3 Tracking",
		Description: "A cache that never goes stale because the server tells you exactly when to throw an entry away — no TTL guesswork, no polling. This is what CLIENT TRACKING and RESP3's push messages are for.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Memory & Performance",
		TimeLimit:   "2h",
		LectureNotes: `The problem: a local cache with no way to know when it's wrong

Caching a GET result in your application's own memory is the single cheapest latency win available — no network round trip at all for a hit. The catch has always been invalidation: without some signal from the server, your local copy can silently go stale the moment another client changes the value, and you either accept staleness or add a TTL short enough to bound the damage (which caps how much benefit the cache can ever provide).

RESP3: a connection that isn't strictly request/response anymore

RESP2 (the protocol every plain valkey-cli connection uses implicitly) is strictly one reply per request. RESP3 adds a genuinely new frame type — the "push" message — that the server can send unprompted, outside the normal request/reply sequence, over a connection that opted in. "valkey-cli -3" connects using RESP3 instead of RESP2; this is the prerequisite everything else here depends on.

CLIENT TRACKING: opting a connection in to invalidation

"CLIENT TRACKING on" tells Valkey to remember which keys *this specific connection* has read since. Every key you GET afterward gets added to that connection's tracking table (visible cluster-wide via "INFO stats"' "tracking_total_keys"). The moment any client — including a completely different connection — modifies one of those keys, Valkey pushes an invalidation message down the tracking connection and removes the key from its tracking table. Your application-side cache, wired to listen for that push, evicts its local copy at exactly the right moment: not before, not (meaningfully) after.

Default mode vs broadcasting mode

What this lab uses is "default" tracking mode: precise, per-key, but Valkey has to remember exactly which keys each tracking connection has actually read, which costs server-side memory proportional to (connections × keys read). "CLIENT TRACKING on BCAST" trades that precision for a fixed, prefix-based cost — the server doesn't track individual keys per client at all, just invalidates any tracking client subscribed to a prefix whenever any key under it changes, whether that specific client ever read it or not. Same underlying push mechanism, different cost/precision trade-off.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "enable-tracking",
				Title: "Cache a key over a RESP3 connection with tracking on",
				Instructions: "Set a baseline value: `valkey-cli -a valkey_password --no-auth-warning SET lab:cached original`.\n\n" +
					"Start a RESP3 connection with tracking on, kept alive in the background:\n\n" +
					"`setsid sh -c \"(printf 'CLIENT TRACKING on\\r\\nGET lab:cached\\r\\n'; sleep 30) | valkey-cli -3 -a valkey_password --no-auth-warning\" > /tmp/track.log 2>&1 < /dev/null &`\n\n" +
					"Confirm it registered: `valkey-cli -a valkey_password --no-auth-warning CLIENT LIST` should show a client with `flags=t` and `resp=3`. Click Check Work.",
				Hint: "The `-3` flag is what requests RESP3 — without it, `CLIENT TRACKING on` still succeeds but has nowhere to deliver an invalidation push, since RESP2 has no out-of-band frame type for the server to use.",
			},
			{
				ID:    "observe-invalidation",
				Title: "Modify the key from elsewhere and watch it get invalidated",
				Instructions: "From a separate connection, change the tracked key: `valkey-cli -a valkey_password --no-auth-warning SET lab:cached updated`.\n\n" +
					"Check `valkey-cli -a valkey_password --no-auth-warning INFO stats | grep tracking_total_keys` — it should drop back to 0, " +
					"meaning the server just invalidated the entry it was tracking on the other connection's behalf. Click Check Work.",
				Hint: "tracking_total_keys dropping to 0 is the server-side proof of invalidation — the tracking connection's own terminal would show the actual RESP3 push frame too, but only when read interactively rather than through a piped background session like this lab's.",
			},
		},
	},
	{
		ID:          "valkey-functions",
		Title:       "Migrating Lua Scripts to Valkey Functions",
		Description: "EVAL scripts live in an ephemeral cache with no name and no metadata. Functions are the same Lua underneath, but loaded as a persisted, named library — with declarative safety flags EVAL never had.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Transactions & Scripting",
		TimeLimit:   "2h",
		LectureNotes: `EVAL's problem: the script cache isn't really state

A Lua script run with "EVAL" is identified purely by its SHA1 hash. That script lives in an internal cache with no name, no way to list "what scripts are loaded right now" in any meaningful form, and — critically — that cache can be silently cleared ("SCRIPT FLUSH", certain replication/failover situations), after which every "EVALSHA" call for it starts failing until re-submitted. Production code has to be defensive about this exact failure mode; it's a well-known EVAL gotcha, not a hypothetical.

FUNCTION LOAD: scripts as a persisted, named library

"FUNCTION LOAD" takes a Lua source string with a "#!lua name=<library>" shebang header and one or more "redis.register_function(...)" calls, and installs it as a durable library with an actual name — visible in "FUNCTION LIST", persisted across restarts, unaffected by SCRIPT FLUSH (a different subsystem entirely). You call a registered function by name with "FCALL <name> <numkeys> [keys...] [args...]" — no hash to manage, no cache-miss fallback logic needed in your client.

Declarative safety: the no-writes flag

"redis.register_function{function_name=..., callback=..., flags={'no-writes'}}" lets a function declare, up front, that it never writes — and Valkey actually enforces that declaration rather than trusting it. "FCALL_RO" is the counterpart: it will only execute a function flagged "no-writes", and rejects anything else outright. EVAL has no equivalent — there's no way to mark a script read-only and have the server refuse to run it if it turns out to write anyway. This is the practical reason to prefer Functions for anything you want a read replica (or a careful reviewer) to trust is actually safe to run.

Hot-reloading: FUNCTION LOAD REPLACE

"FUNCTION LOAD REPLACE" swaps a library's implementation in place — the next FCALL uses the new code immediately, with no restart, no flush of unrelated functions, and existing connections unaffected. That's a genuine operational advantage over hand-rolled EVAL versioning schemes, where "which version of this script is currently live" is a question your application has to answer for itself.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "load-and-call",
				Title: "Load a function library and call it",
				Instructions: "Write the library:\n\n" +
					"`cat > /tmp/lib.lua <<'EOF'\n#!lua name=lablib\nredis.register_function{\n  function_name='safe_get',\n  callback=function(keys, args) return redis.call('GET', keys[1]) end,\n  flags={'no-writes'}\n}\nredis.register_function{\n  function_name='unsafe_set',\n  callback=function(keys, args) return redis.call('SET', keys[1], args[1]) end\n}\nEOF`\n\n" +
					"Load it: `valkey-cli -a valkey_password --no-auth-warning FUNCTION LOAD \"$(cat /tmp/lib.lua)\"`.\n\n" +
					"Call the writer: `valkey-cli -a valkey_password --no-auth-warning FCALL unsafe_set 1 fn:demo hello`.\n\n" +
					"Call the reader: `valkey-cli -a valkey_password --no-auth-warning FCALL safe_get 1 fn:demo` should return `hello`.\n\n" +
					"Confirm `FUNCTION LIST` shows the `lablib` library with both functions. Click Check Work.",
				Hint: "FUNCTION LOAD takes the whole library source as one argument — wrap the `$(cat ...)` in double quotes or your shell will split it on whitespace and Valkey will see a mangled script.",
			},
			{
				ID:    "enforce-readonly-with-fcall_ro",
				Title: "Prove the no-writes flag is actually enforced",
				Instructions: "Try the flagged read-only function through the read-only call path: `valkey-cli -a valkey_password --no-auth-warning FCALL_RO safe_get 1 fn:demo` " +
					"— should succeed and return the current value.\n\n" +
					"Now try the unflagged writer the same way: `valkey-cli -a valkey_password --no-auth-warning FCALL_RO unsafe_set 1 fn:demo nope` " +
					"— this should be rejected with an error about the write flag, not silently execute. Click Check Work.",
				Hint: "FCALL (without _RO) would run unsafe_set just fine — the enforcement is specifically that FCALL_RO refuses anything not explicitly flagged 'no-writes', regardless of what the function actually does.",
			},
		},
	},
	{
		ID:          "valkey-manual-migration",
		Title:       "Manual Slot Migration: MIGRATE, ASKING & the Raw Cluster Protocol",
		Description: "--cluster reshard moves slots with one command. This lab does the exact same thing by hand — the sequence of primitives that tool is automating underneath.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Sharding & Routing",
		TimeLimit:   "2h",
		LectureNotes: `What --cluster reshard does on your behalf

Running --cluster reshard as one command just works. Underneath, that command is running the exact sequence this lab has you type by hand: marking both sides of the move, transferring each key individually, and only then updating global slot ownership. Seeing the raw steps is what makes the automated version make sense, and it's also genuinely useful knowledge — the manual sequence is a real emergency-runbook procedure for moving one specific slot without waiting on the full reshard tool.

IMPORTANT and MIGRATING: two-sided consent before any data moves

Before a single key transfers, the destination master marks the slot "CLUSTER SETSLOT <slot> IMPORTING <source-id>" and the source marks it "CLUSTER SETSLOT <slot> MIGRATING <dest-id>". Neither side's global ownership has changed yet — this is purely local bookkeeping on the two nodes involved, visible in each one's own "CLUSTER NODES" line as a bracketed annotation next to their slot ranges (e.g. "[100->-<dest-id>]" on the source, "[100-<-<source-id>]" on the destination).

MIGRATE: the actual data transfer, key by key

"MIGRATE <dest-host> <dest-port> <key> <dest-db> <timeout> AUTH <password>" atomically copies one key to the destination and removes it from the source — a real, synchronous operation per key (which is why a full reshard of thousands of keys takes visibly longer than the instant slot-ownership flip at the end). Only after every key in the slot has actually migrated does "CLUSTER SETSLOT <slot> NODE <dest-id>", run on every node in the cluster, finalize ownership — turning the temporary MIGRATING/IMPORTING bookkeeping into the real, permanent thing.

ASK vs MOVED: a redirect that only lasts one command

Mid-migration, a client asking the SOURCE for a key that's already moved gets "ASK <slot> <dest>" — not "MOVED". The difference matters: "MOVED" means "the slot lives elsewhere now, permanently, update your routing table" — but ownership hasn't actually changed yet mid-migration, so that would be wrong. "ASK" instead means "just this once, go ask the destination" — and the destination will only serve that one key if the client sends "ASKING" as the immediately preceding command on that same connection. Skip ASKING, or send some other command in between, and the destination redirects right back to the source with a plain MOVED — ASKING's effect applies to exactly the next command and nothing else.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "start-migration",
				Title: "Mark both sides of the move before any data transfers",
				Instructions: "Write a key and follow the redirect: `valkey-cli -c -a valkey_password --no-auth-warning SET '{migrate}demo' before`.\n\n" +
					"Find its slot and current owner: `valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT '{migrate}demo'`, then " +
					"`CLUSTER NODES` to see which master's range covers that slot (the source) and note the node IDs of the source and any other master (your destination).\n\n" +
					"On the destination: `valkey-cli -a valkey_password --no-auth-warning CLUSTER SETSLOT <slot> IMPORTING <source-id>`.\n\n" +
					"On the source: `valkey-cli -a valkey_password --no-auth-warning CLUSTER SETSLOT <slot> MIGRATING <dest-id>`.\n\n" +
					"Confirm with `CLUSTER NODES` on each — the source's own line should show `[<slot>->-<dest-id>]` and the destination's " +
					"should show `[<slot>-<-<source-id>]`. Click Check Work.",
				Hint: "Nothing about global slot ownership has changed yet at this point — both annotations are purely local bookkeeping between these two nodes, which is why a third node's view of the cluster is still unaffected.",
			},
			{
				ID:    "migrate-and-finalize",
				Title: "Transfer the key and finalize ownership everywhere",
				Instructions: "Transfer the key itself (run this on the source):\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning MIGRATE <dest-hostname> 6379 '{migrate}demo' 0 5000 AUTH valkey_password`\n\n" +
					"Finalize ownership on every node in the cluster (source, destination, and the third master):\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning CLUSTER SETSLOT <slot> NODE <dest-id>`\n\n" +
					"Confirm the destination now owns the slot outright — `CLUSTER NODES` shows it in the destination's plain range with no " +
					"bracketed annotation — and `valkey-cli -a valkey_password --no-auth-warning GET '{migrate}demo'` run directly on the " +
					"destination returns `before` with no MOVED redirect. Click Check Work.",
				Hint: "If you forget to run CLUSTER SETSLOT NODE on the *third* master (the one not involved in the migration), that node will keep routing clients to the old source for this slot even though the data has already moved — every node's view has to agree.",
			},
		},
	},
	{
		ID:          "valkey-split-brain",
		Title:       "Split-Brain Resilience: cluster-require-full-coverage & Quorum",
		Description: "By default, one unreachable master takes the entire cluster down for writes — even keys with nothing to do with the missing shard. This lab makes that trade-off concrete, then flips it, and asks what you actually bought by flipping it.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey Cluster",
		Category:    "Cluster Administration",
		TimeLimit:   "2h",
		LectureNotes: `Availability vs consistency, made concrete

"The cluster is up" is the normal state. This lab deliberately breaks that, because the default behavior when a master genuinely disappears is stricter than most people expect: it's not just that keys in the missing shard become unavailable — by default, the *entire cluster* refuses writes, including shards that are perfectly healthy.

cluster-require-full-coverage: all-or-nothing by default

With this setting at its default of "yes", Valkey Cluster considers itself down ("cluster_state:fail" in "CLUSTER INFO") the instant any of the 16384 slots isn't owned by a reachable master — whether that's from a genuine node failure or mid-operation bookkeeping. Once that happens, every node refuses commands with "CLUSTERDOWN", not just ones touching the affected slot range. This is a deliberate, conservative choice: an application that can only see part of its keyspace might be making decisions on an incomplete view of its data, and the default assumes that's worse than an outage.

Turning it off: a real trade-off, not a bugfix

"CONFIG SET cluster-require-full-coverage no" changes the calculus: the cluster now serves whatever slots ARE covered and only fails commands that map to the actually-missing range. This buys back partial availability during a real outage — genuinely valuable if your application can tolerate some keys being temporarily unreachable while others keep working. But it's not free: your application now has to handle "some of my data is available, some isn't" as a normal operating condition, which is strictly harder to reason about than "everything works or nothing does."

Detecting failure: gossip, not a central authority

In a real deployment, there's no coordinator deciding a node is down — every master independently marks another as PFAIL (possibly failed) once it can't be reached within "cluster-node-timeout", then FAIL once enough other masters agree via gossip (the same quorum-by-agreement idea Sentinel uses, but built into Cluster itself rather than a separate process). In this lab's environment, systemd restarts a crashed node's Valkey service automatically within about a second — faster than that detection window, and with its cluster membership file (nodes.conf) intact, so the node simply rejoins before anyone else's gossip-based failure detection would even fire. To still produce a genuine, observable coverage gap, this lab has the surviving nodes explicitly disown the missing one with "CLUSTER FORGET" — the same command an operator runs in real life once they've independently confirmed a node is truly gone for good (a hardware failure, a decommission), rather than waiting on gossip to catch up.

CLUSTER FORGET: an explicit, immediate declaration

"CLUSTER FORGET <node-id>" removes a node from the caller's own view of the cluster right away — no waiting on gossip rounds — and that node's previously-owned slots become instantly unaccounted for from the caller's perspective. It also blacklists that node ID for about a minute, ignoring any gossip messages it sends trying to reintroduce itself, which is exactly why this lab's approach stays stable even though the "crashed" node's container comes back and starts talking to the cluster again almost immediately.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "crash-and-observe-clusterdown",
				Title: "Disown a master and watch the whole cluster refuse writes",
				Instructions: "Pick a master and note its node ID from `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES`.\n\n" +
					"Crash it: on that node, run `valkey-cli -a valkey_password --no-auth-warning SHUTDOWN NOSAVE` (systemd will restart the " +
					"Valkey service and it'll rejoin on its own within a second or two — that's fine, the next step doesn't depend on it staying down).\n\n" +
					"On EACH of the other two nodes, declare it gone: `valkey-cli -a valkey_password --no-auth-warning CLUSTER FORGET <that-node-id>`.\n\n" +
					"On one of those two nodes, run `CLUSTER INFO` — `cluster_state` should now read `fail`.\n\n" +
					"Try any write there, even one that has nothing to do with the missing shard: `valkey-cli -a valkey_password --no-auth-warning SET some:key val` " +
					"— it should be refused with `CLUSTERDOWN`. Click Check Work.",
				Hint: "CLUSTER FORGET has to be run on each surviving node individually — it changes only that node's own view, so a node you skip will still see the \"forgotten\" node as a normal, healthy member.",
			},
			{
				ID:    "disable-full-coverage-requirement",
				Title: "Trade strict consistency for partial availability",
				Instructions: "On the SAME two nodes you ran CLUSTER FORGET on, run `valkey-cli -a valkey_password --no-auth-warning CONFIG SET cluster-require-full-coverage no`.\n\n" +
					"Check `CLUSTER INFO` again on either of them — `cluster_state` should now read `ok` despite still not knowing about the third master.\n\n" +
					"Confirm writes to healthy slots now work: `valkey-cli -a valkey_password --no-auth-warning SET some:key val` should succeed. Click Check Work.",
				Hint: "You have to set this on both of the forgetting nodes individually — CONFIG SET isn't a cluster-wide operation, so a node you skip will still enforce full coverage on its own. The third node (the one you originally crashed) doesn't matter for this check — it rejoined on its own and was never told to forget anyone.",
			},
		},
	},
	{
		ID:          "valkey-replication-internals",
		Title:       "Replication Internals: Partial Resync vs Full Resync",
		Description: "Every replica reconnect is either cheap (resume from where it left off) or expensive (re-transfer the entire dataset) — and which one happens depends on whether the master still remembers what the replica missed. This lab produces both, on purpose, and shows the counters that tell them apart.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Replication & Failover",
		TimeLimit:   "2h",
		LectureNotes: `Every replication link has a replication ID and an offset

Once a replica has fully synced, both sides agree on a replication ID (identifying this specific lineage of the dataset) and a numeric offset (how many bytes of the write stream have been applied). "INFO replication" on the master shows "master_replid" and "master_repl_offset"; the replica caches both when connected. That cached state is the entire basis for what happens on the *next* reconnect.

The replication backlog: a bounded buffer of recent writes

The master keeps a circular buffer — "repl-backlog-size", 1MB by default — of the most recent bytes of the write stream, independent of whether any replica is currently attached. On reconnect, a replica presents its cached replication ID and offset via PSYNC; if the master's backlog still contains everything from that offset forward AND the replication ID matches, it can just replay the missing slice from the buffer. That's a partial resync — "PSYNC CONTINUE" in the protocol, "sync_partial_ok" in "INFO stats" — cheap regardless of how large the full dataset is, because only the *missed writes* get retransmitted.

Why a brief network blip is cheap

A short disconnect — the master losing the TCP connection to a replica that's still configured to follow it and immediately reconnects — is exactly the case a partial resync is built for: the replica still has its cached replication ID and offset from before the blip, and the backlog almost certainly still covers however little was missed. This is the common case in real operation, and it's why replication links surviving routine network hiccups doesn't come with a full-resync performance cliff attached.

Why a real outage doesn't get the shortcut

A replica that's genuinely restarted from scratch — not just reconnected, but lost its own process state entirely — has no cached replication ID or offset to present at all. There's nothing to resume from, so PSYNC falls back to a full resync ("sync_full" incrementing): a complete RDB transfer, then replay of everything since. This is strictly more expensive, and it's the reason "how long was this replica actually down" matters operationally — a network blip and a genuine crash produce the same *end state* (a caught-up replica) through very different amounts of work to get there.`,
		DesignTemplate: labValkeyReplicationDesign,
		Steps: []LabStep{
			{
				ID:    "trigger-partial-resync",
				Title: "Simulate a network blip and confirm a partial resync",
				Instructions: "On valkey-b, connect it: `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379`.\n\n" +
					"Wait for `INFO replication` on valkey-b to show `master_link_status:up`.\n\n" +
					"On valkey-a, simulate the blip from the master's side — this drops the TCP connection without touching valkey-b's own " +
					"REPLICAOF configuration, so it reconnects on its own:\n\n" +
					"`valkey-cli -a valkey_password --no-auth-warning CLIENT KILL TYPE replica`\n\n" +
					"Wait a couple seconds, then check `valkey-cli -a valkey_password --no-auth-warning INFO stats` on valkey-a: `sync_partial_ok` " +
					"should now be at least 1, while `sync_full` should still read 1 (only the very first connection). Click Check Work.",
				Hint: "Don't use `REPLICAOF NO ONE` to simulate the disconnect — that command explicitly discards valkey-b's cached master state, which guarantees a full resync on reconnect instead of the partial one this step is demonstrating.",
			},
			{
				ID:    "trigger-full-resync-after-outage",
				Title: "Crash the replica for real and confirm it costs a full resync",
				Instructions: "On valkey-b, crash it for real: `valkey-cli -a valkey_password --no-auth-warning SHUTDOWN NOSAVE`.\n\n" +
					"Wait a few seconds for systemd to restart the Valkey service automatically (it comes back with no replication configured " +
					"at all — REPLICAOF was never written to its config file).\n\n" +
					"Reconnect it: `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379`.\n\n" +
					"Wait for the link to come back up, then check `INFO stats` on valkey-a again: `sync_full` should now be higher than before " +
					"— a second full resync, not a partial one. Click Check Work.",
				Hint: "A real restart runs a brand-new valkey-server process that only ever reads its static config file — REPLICAOF was set at runtime, never persisted there, so the restarted instance has no cached replication ID or offset to present at all. There's nothing to resume from, so PSYNC has no choice but to fall back to a full transfer.",
			},
		},
	},
	{
		ID:          "valkey-tls",
		Title:       "Securing Valkey with TLS & Mutual Authentication",
		Description: "This node authenticates with a plaintext password over an unencrypted connection by default. This lab turns on TLS at runtime — no restart — and confirms it actually enforces client certificates, not just server identity.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Security & Access Control",
		TimeLimit:   "2h",
		LectureNotes: `TLS support is compiled in, but off by default

This lab's image ships with TLS support already built in — "CONFIG GET tls-port" returns a real (if currently 0) value, proof the capability exists — but no TLS listener is active until it's explicitly configured. That's a deliberate default: TLS needs a certificate and key to mean anything, and there's no sensible one to generate automatically at first boot.

Hot-reloading certificates: no restart required

Unlike most services where enabling TLS means editing a config file and restarting, Valkey's TLS-related settings ("tls-cert-file", "tls-key-file", "tls-ca-cert-file", "tls-port", and others) are all modifiable at runtime with plain "CONFIG SET" — the plaintext port keeps serving existing connections throughout, and the TLS listener comes up the moment the configuration is valid. This matters operationally: rotating a certificate before it expires doesn't require a maintenance window.

tls-auth-clients: TLS with a password vs genuine mutual TLS

By default, "tls-auth-clients" is "yes" — the server requires every connecting client to present a valid certificate signed by the configured CA, verified during the TLS handshake itself, before the connection is even established. This is mutual TLS: both sides prove their identity via certificates, not just the server. A client that connects over TLS but skips presenting its own certificate gets rejected outright — encryption alone doesn't satisfy this requirement, only a trusted client certificate does. (Setting it to "no" would fall back to TLS purely for encryption-in-transit, still layered on top of the regular AUTH password — a weaker but sometimes sufficient posture.)

Why permissions matter here in a way they haven't elsewhere

The private key file has to actually be readable by the user Valkey runs as, or the CONFIG SET simply fails with a permission error and silently leaves TLS disabled — a real, easy-to-hit gotcha the moment you generate a key with restrictive default permissions ("openssl req" defaults to mode 600, owner-only) inside a container where the server process isn't necessarily the same user that ran openssl.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "enable-tls",
				Title: "Generate a certificate and turn on TLS at runtime",
				Instructions: "Install openssl: `apt-get update -qq && apt-get install -y -qq openssl`.\n\n" +
					"Generate a self-signed cert: `mkdir -p /tmp/tls && cd /tmp/tls && openssl req -x509 -newkey rsa:2048 -days 1 -nodes -keyout key.pem -out cert.pem -subj '/CN=valkey-lab'`.\n\n" +
					"Make the key readable: `chmod 644 /tmp/tls/key.pem`.\n\n" +
					"Turn TLS on: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET tls-cert-file /tmp/tls/cert.pem tls-key-file /tmp/tls/key.pem tls-ca-cert-file /tmp/tls/cert.pem tls-port 6380`.\n\n" +
					"Confirm it's listening: `valkey-cli --tls --cert /tmp/tls/cert.pem --key /tmp/tls/key.pem --cacert /tmp/tls/cert.pem -p 6380 -a valkey_password --no-auth-warning PING` " +
					"should return `PONG`. Click Check Work.",
				Hint: "If CONFIG SET fails with a permission error mentioning the key file, that's the `chmod 644` step — `openssl req` writes the key world-unreadable by default, and CONFIG SET fails closed rather than partially applying a broken TLS config.",
			},
			{
				ID:    "verify-mutual-tls-enforced",
				Title: "Confirm the server actually requires a client certificate",
				Instructions: "Try connecting over TLS without presenting a client certificate:\n\n" +
					"`valkey-cli --tls --cacert /tmp/tls/cert.pem -p 6380 -a valkey_password --no-auth-warning PING`\n\n" +
					"— this should fail (an I/O or connection error, not a normal Valkey error reply), because `tls-auth-clients` defaults to `yes`.\n\n" +
					"Confirm the same connection succeeds when you DO present the certificate:\n\n" +
					"`valkey-cli --tls --cert /tmp/tls/cert.pem --key /tmp/tls/key.pem --cacert /tmp/tls/cert.pem -p 6380 -a valkey_password --no-auth-warning PING`\n\n" +
					"should return `PONG`. Click Check Work.",
				Hint: "The no-cert failure happens during the TLS handshake itself, before Valkey's own command processing even runs — that's why it shows up as a connection-level I/O error rather than an AUTH or permission error from the server.",
			},
		},
	},
	{
		ID:          "valkey-client-management",
		Title:       "Client & Resource Management: CLIENT LIST, CLIENT KILL & CLIENT PAUSE",
		Description: "Every connection Valkey has is inspectable and individually terminable, and writes cluster-wide can be held at a synchronization point on command — the operational tools for containing a misbehaving client in production, without restarting anything.",
		Difficulty:  "Advanced",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Client & Resource Management",
		TimeLimit:   "2h",
		LectureNotes: `CLIENT LIST: every connection's live state, at a glance

"CLIENT LIST" prints one line per connected client — address, connection age, idle time, the command it's currently running, memory it's using, and more — everything you'd need to answer "what is this server actually doing right now" without guessing from aggregate stats. "CLIENT LIST TYPE replica" (or "normal", "pubsub", "master") filters to one connection class, useful the moment a server has more than a handful of clients.

CLIENT KILL: surgical, not a restart

"CLIENT KILL ID <id>" (or by address, or by matching other filters) terminates one specific connection immediately, without touching any other client and without restarting the server. This is the real tool for a stuck or misbehaving connection in production — a client holding a long-running blocking command, an old connection that never got cleaned up client-side, a script gone rogue — where the alternative of restarting Valkey to clear it would be enormously more disruptive than it needs to be.

CLIENT PAUSE: buying yourself a synchronization point

"CLIENT PAUSE <ms> WRITE" makes every client's write commands block for up to that many milliseconds (reads keep working with plain "WRITE"; "ALL" pauses everything). This is exactly the kind of primitive administrative tooling needs underneath it — briefly guaranteeing "nothing is writing right now" gives you a clean point to, for example, take a consistent snapshot or coordinate a handoff, without actually stopping the server or refusing connections.

Resource limits exist too, just not covered hands-on here

"maxclients" caps total concurrent connections, and "client-output-buffer-limit" caps how much unsent output a client (particularly a slow Pub/Sub subscriber or replica) can have queued before Valkey disconnects it rather than let memory grow unbounded. Both are worth knowing exist — they're the guardrails that turn "one slow client" into a bounded problem instead of an OOM risk — even though this lab's checks focus on the two commands above.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "find-and-kill-a-client",
				Title: "Find a specific client by its command, then kill it",
				Instructions: "Open a long-lived blocking connection in the background:\n\n" +
					"`setsid valkey-cli -a valkey_password --no-auth-warning BLPOP lab:nokey 30 > /tmp/blpop.log 2>&1 < /dev/null &`\n\n" +
					"Find it: `valkey-cli -a valkey_password --no-auth-warning CLIENT LIST` — look for the line with `cmd=blpop` and note its `id=`.\n\n" +
					"Kill it: `valkey-cli -a valkey_password --no-auth-warning CLIENT KILL ID <that-id>`.\n\n" +
					"Confirm `cat /tmp/blpop.log` shows a connection-closed error (not a normal BLPOP timeout reply), and that `CLIENT LIST` " +
					"no longer shows any `cmd=blpop` client. Click Check Work.",
				Hint: "If you wait the full 30 seconds instead of killing it, BLPOP just times out normally and the log won't show the connection-closed error this check looks for — kill it well before then.",
			},
			{
				ID:    "pause-writes-briefly",
				Title: "Confirm CLIENT PAUSE actually blocks a write for its duration",
				Instructions: "Pause writes for 2 seconds: `valkey-cli -a valkey_password --no-auth-warning CLIENT PAUSE 2000 WRITE`.\n\n" +
					"Immediately time a write against it: `time valkey-cli -a valkey_password --no-auth-warning SET lab:paused val` — the " +
					"real time reported should be close to 2 seconds, not near-instant, proving the write genuinely waited rather than the pause being a no-op. Click Check Work.",
				Hint: "Run the timed SET immediately after CLIENT PAUSE, not a few seconds later — if the 2-second window has already elapsed by the time your write arrives, it'll return instantly and look like the pause did nothing.",
			},
		},
	},
}

func init() {
	labCatalog = append(labCatalog, valkeyClusterLabs...)
}

// valkeyFrameFromStack parses a lab stack's design and returns its (single)
// Valkey Cluster frame — every check below needs this first, mirroring
// patroniFrameFromStack.
func valkeyFrameFromStack(st Stack) (designDoc, designFrame, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return doc, designFrame{}, false
	}
	for _, f := range doc.Frames {
		if f.Type == "valkeycluster" {
			return doc, f, true
		}
	}
	return doc, designFrame{}, false
}

// valkeyRunningMembers returns the running valkeycluster deployments for a
// lab stack, mirroring runningPatroniMembers.
func (a *App) valkeyRunningMembers(st Stack, doc designDoc) ([]Deployment, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var running []Deployment
	for _, n := range doc.Nodes {
		if n.Type != "valkeycluster" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	return running, nil
}

// valkeyPasswordFor reads the shared default-user password from a member's
// own stored secrets (set once at cluster creation, identical on every
// member) rather than assuming the .env default directly.
func valkeyPasswordFor(dep Deployment) string {
	var sec valkeySecrets
	json.Unmarshal(dep.Secrets, &sec)
	if sec.Password == "" {
		return envOr("VALKEY_PASSWORD", "valkey_password")
	}
	return sec.Password
}

// valkeyClusterNode is one line of `CLUSTER NODES` output, parsed.
type valkeyClusterNode struct {
	ID        string
	Addr      string
	Flags     []string
	Master    string   // master's node ID, "" if this node is itself a master
	Ranges    [][2]int // owned slot ranges (inclusive), empty for a replica or an empty master
	SlotCount int
}

func (n valkeyClusterNode) hasFlag(flag string) bool {
	for _, f := range n.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

// rangesContain reports whether this node currently owns slot.
func (n valkeyClusterNode) rangesContain(slot int) bool {
	for _, r := range n.Ranges {
		if slot >= r[0] && slot <= r[1] {
			return true
		}
	}
	return false
}

// parseValkeyClusterNodes parses `CLUSTER NODES` plaintext output. Slot
// migration annotations (tokens starting with "[", e.g.
// "[100-<-abcd1234]") are skipped — only slots a node actually owns count,
// not slots mid-migration in or out.
func parseValkeyClusterNodes(output string) []valkeyClusterNode {
	var nodes []valkeyClusterNode
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		n := valkeyClusterNode{ID: fields[0], Addr: fields[1], Flags: strings.Split(fields[2], ",")}
		if fields[3] != "-" {
			n.Master = fields[3]
		}
		for _, tok := range fields[8:] {
			if strings.HasPrefix(tok, "[") {
				continue
			}
			if dash := strings.IndexByte(tok, '-'); dash > 0 {
				lo, errLo := strconv.Atoi(tok[:dash])
				hi, errHi := strconv.Atoi(tok[dash+1:])
				if errLo == nil && errHi == nil {
					n.Ranges = append(n.Ranges, [2]int{lo, hi})
					n.SlotCount += hi - lo + 1
				}
			} else if v, err := strconv.Atoi(tok); err == nil {
				n.Ranges = append(n.Ranges, [2]int{v, v})
				n.SlotCount++
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// fetchValkeyClusterNodes execs `CLUSTER NODES` on a specific container and
// parses the result. ok is false if the exec failed.
func (a *App) fetchValkeyClusterNodes(ctx context.Context, containerID, password string) ([]valkeyClusterNode, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID,
		[]string{"valkey-cli", "-a", password, "--no-auth-warning", "CLUSTER", "NODES"}, nil)
	if err != nil || res.Code != 0 {
		return nil, false
	}
	return parseValkeyClusterNodes(res.Stdout), true
}

// checkValkeyKeyRouted passes once lab:hello actually exists on whichever
// node CLUSTER KEYSLOT says owns its slot — proof the key really landed on
// the slot-correct node, regardless of whether the learner followed the
// MOVED redirect by hand or let -c do it. Checked by asking each running
// node's own "myself" entry whether it owns the slot, rather than matching
// IP addresses — every node already knows its own ownership.
func (a *App) checkValkeyKeyRouted(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	pw := valkeyPasswordFor(running[0])
	res, err := a.engCtx(ctx).Exec(ctx, running[0].ContainerID,
		[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLUSTER", "KEYSLOT", "lab:hello"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's status — is the cluster still forming?"}
	}
	wantSlot, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not parse CLUSTER KEYSLOT output."}
	}
	for _, d := range running {
		nodes, ok := a.fetchValkeyClusterNodes(ctx, d.ContainerID, pw)
		if !ok {
			continue
		}
		var mine *valkeyClusterNode
		for i := range nodes {
			if nodes[i].hasFlag("myself") {
				mine = &nodes[i]
				break
			}
		}
		if mine == nil || !mine.rangesContain(wantSlot) {
			continue
		}
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "EXISTS", "lab:hello"}, nil)
		if err != nil || res.Code != 0 {
			return LabStepResult{Passed: false, Message: "Could not query the node that owns slot " + strconv.Itoa(wantSlot) + "."}
		}
		if strings.TrimSpace(res.Stdout) != "1" {
			return LabStepResult{Passed: false, Message: "lab:hello isn't set yet — write it with `valkey-cli -c -a valkey_password --no-auth-warning SET lab:hello world`."}
		}
		return LabStepResult{Passed: true, Message: "Confirmed: lab:hello (slot " + strconv.Itoa(wantSlot) + ") is stored on " + nodeLabel(doc, d.NodeID) + ", the node that actually owns that slot."}
	}
	return LabStepResult{Passed: false, Message: "Could not determine which node owns slot " + strconv.Itoa(wantSlot) + " — wait a moment and check again."}
}

// valkeyMasterSlotCounts execs CLUSTER NODES on the first reachable member
// and returns each MASTER's own slot count (replicas excluded — they own
// none of their own). Used by checks that need to compare distribution
// across masters without any stored baseline.
func (a *App) valkeyMasterSlotCounts(ctx context.Context, running []Deployment) (map[string]int, bool) {
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		nodes, ok := a.fetchValkeyClusterNodes(ctx, d.ContainerID, pw)
		if !ok {
			continue
		}
		counts := map[string]int{}
		for _, n := range nodes {
			if n.Master == "" { // masters report Master == "" (the "-" placeholder)
				counts[n.ID] = n.SlotCount
			}
		}
		return counts, true
	}
	return nil, false
}

// checkValkeyReshardOccurred passes once the spread between the busiest and
// quietest master's slot count is well beyond the ~1-slot imbalance a fresh
// 3-way split of 16384 slots naturally has (16384 doesn't divide evenly by
// 3) — proof a real reshard moved a meaningful number of slots, without
// needing any stored baseline to compare against.
func (a *App) checkValkeyReshardOccurred(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	counts, ok := a.valkeyMasterSlotCounts(ctx, running)
	if !ok || len(counts) == 0 {
		return LabStepResult{Passed: false, Message: "Could not read the cluster's slot distribution — is the cluster still forming?"}
	}
	min, max := -1, -1
	for _, c := range counts {
		if min == -1 || c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}
	if max-min < 200 {
		return LabStepResult{Passed: false, Message: "Slot distribution is still roughly even (max " + strconv.Itoa(max) + ", min " + strconv.Itoa(min) + ") — reshard at least 1000 slots from one master to another, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: slot counts across masters now range from " + strconv.Itoa(min) + " to " + strconv.Itoa(max) + " — a real reshard happened."}
}

// checkValkeyReplicaBuilt passes once some member's own CLUSTER NODES entry
// shows itself as a slave — proof resharding all of a master's slots away
// really did trigger Valkey's automatic slave conversion. Remembers which
// node it was via LabRun.InitialLeaderNode (SetLabRunLeader) — reused here
// purely as a free string slot this lab run can stash one node ID in, not
// because this is a "leader" in the Patroni sense — so the next step can
// confirm specifically that node (not just "some" node) got promoted back.
func (a *App) checkValkeyReplicaBuilt(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		nodes, ok := a.fetchValkeyClusterNodes(ctx, d.ContainerID, pw)
		if !ok {
			continue
		}
		for _, n := range nodes {
			if n.hasFlag("myself") && n.hasFlag("slave") {
				a.store.SetLabRunLeader(run.ID, d.NodeID)
				return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, d.NodeID) + " is now a replica — Valkey converted it automatically once it owned zero slots."}
			}
		}
	}
	return LabStepResult{Passed: false, Message: "No member is a replica yet — reshard all of one master's slots onto another, then check again."}
}

// checkValkeyManualFailover passes once the specific node that became a
// replica in the previous step (LabRun.InitialLeaderNode) is now reporting
// itself as master with slots — proof CLUSTER FAILOVER actually promoted
// that node, not merely that some master/replica pair exists.
func (a *App) checkValkeyManualFailover(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no node has been converted to a replica yet."}
	}
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var target Deployment
	for _, d := range deps {
		if d.NodeID == run.InitialLeaderNode {
			target = d
		}
	}
	if target.ContainerID == "" || target.State != DeployRunning {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, run.InitialLeaderNode) + " isn't running — wait for the cluster to settle and check again."}
	}
	pw := valkeyPasswordFor(target)
	nodes, ok := a.fetchValkeyClusterNodes(ctx, target.ContainerID, pw)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read " + nodeLabel(doc, run.InitialLeaderNode) + "'s cluster status."}
	}
	for _, n := range nodes {
		if n.hasFlag("myself") {
			if n.hasFlag("master") && n.SlotCount > 0 {
				return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, run.InitialLeaderNode) + " is now master, holding " + strconv.Itoa(n.SlotCount) + " slots — the manual failover promoted it."}
			}
			return LabStepResult{Passed: false, Message: nodeLabel(doc, run.InitialLeaderNode) + " is still a replica — run `CLUSTER FAILOVER` from it, then check again."}
		}
	}
	return LabStepResult{Passed: false, Message: "Could not confirm " + nodeLabel(doc, run.InitialLeaderNode) + "'s current role — wait a moment and check again."}
}

// valkeyStandaloneRunningMembers returns running standalone "valkey" (not
// "valkeycluster") node deployments for a lab stack — labs 4-6, 9, 10 use
// this plain node type instead of a cluster frame.
func (a *App) valkeyStandaloneRunningMembers(st Stack, doc designDoc) ([]Deployment, error) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return nil, err
	}
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	var running []Deployment
	for _, n := range doc.Nodes {
		if n.Type != "valkey" {
			continue
		}
		if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
			running = append(running, d)
		}
	}
	return running, nil
}

// singleValkeyNode is a convenience for the single-node standalone labs
// (persistence, memory eviction, ACLs, transactions, slowlog, backup) that
// only ever have one "valkey" node to check against.
func (a *App) singleValkeyNode(st Stack) (Deployment, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return Deployment{}, false
	}
	running, err := a.valkeyStandaloneRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return Deployment{}, false
	}
	return running[0], true
}

// valkeyNodeDeployment finds a specific, currently-running deployment by its
// design node ID — used by the replication and Sentinel labs to tell
// valkey-a and valkey-b apart.
func (a *App) valkeyNodeDeployment(st Stack, nodeID string) (Deployment, bool) {
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return Deployment{}, false
	}
	for _, d := range deps {
		if d.NodeID == nodeID && d.State == DeployRunning && d.ContainerID != "" {
			return d, true
		}
	}
	return Deployment{}, false
}

// valkeyConfigGet execs CONFIG GET on a single key and returns its value.
func (a *App) valkeyConfigGet(ctx context.Context, containerID, password, key string) (string, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID,
		[]string{"valkey-cli", "-a", password, "--no-auth-warning", "CONFIG", "GET", key}, nil)
	if err != nil || res.Code != 0 {
		return "", false
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) < 2 {
		return "", false
	}
	return fields[1], true
}

// valkeyInfoInt parses a single "field:value" integer line out of an INFO
// section's plaintext output. Returns -1 if the field isn't present or
// isn't an integer.
func valkeyInfoInt(output, field string) int {
	prefix := field + ":"
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix))); err == nil {
				return v
			}
		}
	}
	return -1
}

// checkValkeyFsyncAlways passes once appendfsync has actually been tightened
// to "always" — the durability dial the persistence lab's first step tunes.
func (a *App) checkValkeyFsyncAlways(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	v, ok := a.valkeyConfigGet(ctx, dep.ContainerID, pw, "appendfsync")
	if !ok || v != "always" {
		return LabStepResult{Passed: false, Message: "appendfsync isn't set to always yet — run CONFIG SET appendfsync always, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: appendfsync is set to always."}
}

// checkValkeyFsyncLatencyRecorded passes once the aof-fsync-always latency
// event class actually has recorded history — direct evidence the fsync
// cost is measurable, not just theoretical.
func (a *App) checkValkeyFsyncLatencyRecorded(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "LATENCY", "HISTORY", "aof-fsync-always"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read latency history — is the node still starting up?"}
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" || out == "(empty array)" {
		return LabStepResult{Passed: false, Message: "No aof-fsync-always events recorded yet — make sure appendfsync is set to always and latency-monitor-threshold is on, then write a few keys and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: aof-fsync-always latency events are being recorded — the durability cost of fsync-per-write is measurable."}
}

// checkValkeyEvictionConfigured passes once a real maxmemory ceiling and the
// allkeys-lru policy are both set.
func (a *App) checkValkeyEvictionConfigured(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	mm, ok := a.valkeyConfigGet(ctx, dep.ContainerID, pw, "maxmemory")
	if !ok || mm == "" || mm == "0" {
		return LabStepResult{Passed: false, Message: "maxmemory isn't set yet — run CONFIG SET maxmemory 12mb, then check again."}
	}
	policy, ok := a.valkeyConfigGet(ctx, dep.ContainerID, pw, "maxmemory-policy")
	if !ok || policy != "allkeys-lru" {
		return LabStepResult{Passed: false, Message: "maxmemory-policy isn't allkeys-lru yet — run CONFIG SET maxmemory-policy allkeys-lru, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: maxmemory is capped and maxmemory-policy is allkeys-lru."}
}

// checkValkeyEvictionOccurred passes once INFO stats reports real evictions
// — proof the ceiling was actually hit and allkeys-lru did its job, not
// just that the config was set.
func (a *App) checkValkeyEvictionOccurred(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "stats"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read INFO stats — is the node still starting up?"}
	}
	n := valkeyInfoInt(res.Stdout, "evicted_keys")
	if n <= 0 {
		return LabStepResult{Passed: false, Message: "evicted_keys is still 0 — fill past the maxmemory ceiling with more writes, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: " + strconv.Itoa(n) + " keys have been evicted under memory pressure."}
}

// checkValkeyACLUserCreated passes once app_readonly exists, scoped to
// ~app:* with +@read — checked via ACL GETUSER rather than parsing ACL
// LIST's more free-form line format.
func (a *App) checkValkeyACLUserCreated(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "ACL", "GETUSER", "app_readonly"}, nil)
	if err != nil || res.Code != 0 || strings.TrimSpace(res.Stdout) == "" {
		return LabStepResult{Passed: false, Message: "The app_readonly user doesn't exist yet — create it with ACL SETUSER, then check again."}
	}
	out := res.Stdout
	if !strings.Contains(out, "~app:*") {
		return LabStepResult{Passed: false, Message: "app_readonly exists but isn't scoped to the app:* key pattern yet."}
	}
	if !strings.Contains(out, "+@read") {
		return LabStepResult{Passed: false, Message: "app_readonly exists but doesn't have read command permissions yet."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: app_readonly exists, scoped to ~app:* with +@read."}
}

// checkValkeyACLEnforced passes once app_readonly can read app:config but is
// actually denied (NOPERM) on a write — proof the restriction is enforced,
// not just configured.
func (a *App) checkValkeyACLEnforced(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	getRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "--user", "app_readonly", "-a", "apppass", "--no-auth-warning", "GET", "app:config"}, nil)
	if err != nil || getRes.Code != 0 || strings.TrimSpace(getRes.Stdout) != "hello" {
		return LabStepResult{Passed: false, Message: "app_readonly couldn't read app:config — confirm the user exists with the apppass password and +@read on ~app:*."}
	}
	setRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "--user", "app_readonly", "-a", "apppass", "--no-auth-warning", "SET", "app:config", "bye"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not test app_readonly's write access."}
	}
	// valkey-cli exits 0 even when the server replies with an error (NOPERM
	// included) — the exit code alone can't distinguish "wrote OK" from
	// "denied", so the reply text itself has to be checked instead.
	out := strings.TrimSpace(setRes.Stdout)
	if out == "OK" {
		return LabStepResult{Passed: false, Message: "app_readonly was able to write app:config — it should only have +@read, not write access."}
	}
	if !strings.Contains(strings.ToUpper(out), "NOPERM") {
		return LabStepResult{Passed: false, Message: "The write was rejected, but not with the expected NOPERM error — check the user's command permissions."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: app_readonly can read app:config but is denied (NOPERM) on SET."}
}

// checkValkeyTransactionRan passes once tx:counter reflects the MULTI/EXEC
// batch (SET 10, then INCR) actually ran as one queued unit.
func (a *App) checkValkeyTransactionRan(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "GET", "tx:counter"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read tx:counter — is the node still starting up?"}
	}
	if strings.TrimSpace(res.Stdout) != "11" {
		return LabStepResult{Passed: false, Message: "tx:counter isn't 11 yet — run the MULTI/EXEC transaction (SET tx:counter 10, INCR tx:counter), then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: tx:counter is 11 — the MULTI/EXEC transaction ran."}
}

// checkValkeyLuaLimitHeld passes once tx:limited stopped at exactly 5 —
// proof the Lua script's check-then-act logic held the limit atomically
// across six separate EVAL calls.
func (a *App) checkValkeyLuaLimitHeld(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "GET", "tx:limited"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read tx:limited — is the node still starting up?"}
	}
	if strings.TrimSpace(res.Stdout) != "5" {
		return LabStepResult{Passed: false, Message: "tx:limited isn't 5 yet — run the EVAL script six times in a row so the limit actually gets hit, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: tx:limited stopped at 5 — the Lua script enforced the limit atomically."}
}

// checkValkeySlowlogCaught passes once a KEYS command actually shows up in
// the slowlog — proof the anti-pattern was really caught, not just run.
func (a *App) checkValkeySlowlogCaught(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "SLOWLOG", "GET", "25"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read the slowlog — is the node still starting up?"}
	}
	// valkey-cli's non-interactive output prints each queued command's name
	// on its own bare line (no quotes, no index numbering), unlike its
	// interactive REPL formatting — match that exact plain form.
	found := false
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == "KEYS" {
			found = true
			break
		}
	}
	if !found {
		return LabStepResult{Passed: false, Message: "No KEYS command found in the slowlog yet — set slowlog-log-slower-than to 0 and run KEYS '*', then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a KEYS command shows up in the slowlog."}
}

// checkValkeyLatencyCommandRecorded passes once the same slow command also
// shows up under latency monitoring's "command" event class.
func (a *App) checkValkeyLatencyCommandRecorded(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "LATENCY", "HISTORY", "command"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read latency history — is the node still starting up?"}
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" || out == "(empty array)" {
		return LabStepResult{Passed: false, Message: "No command latency events recorded yet — set latency-monitor-threshold to 1, run the slow KEYS scan again, then check."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the slow command shows up in latency monitoring too."}
}

// checkValkeyBackupTaken passes once BGSAVE has actually completed (LASTSAVE
// advanced) and the snapshot was copied to a separate backup path.
func (a *App) checkValkeyBackupTaken(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	lastsave, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "LASTSAVE"}, nil)
	if err != nil || lastsave.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read LASTSAVE — is the node still starting up?"}
	}
	if ts, convErr := strconv.Atoi(strings.TrimSpace(lastsave.Stdout)); convErr != nil || ts == 0 {
		return LabStepResult{Passed: false, Message: "No successful save recorded yet — run BGSAVE, wait a couple seconds, then check again."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"sh", "-c", "test -f /var/lib/valkey/data/backup-dump.rdb && echo yes || echo no"}, nil)
	if err != nil || strings.TrimSpace(res.Stdout) != "yes" {
		return LabStepResult{Passed: false, Message: "backup-dump.rdb doesn't exist yet — copy /var/lib/valkey/data/dump.rdb to /var/lib/valkey/data/backup-dump.rdb, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: BGSAVE completed and the backup file was copied to /var/lib/valkey/data/backup-dump.rdb."}
}

// checkValkeyBackupVerified passes once valkey-check-rdb confirms the backup
// file is structurally sound — the step that turns "a file that's probably
// a backup" into a verified, restorable one.
func (a *App) checkValkeyBackupVerified(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-check-rdb", "/var/lib/valkey/data/backup-dump.rdb"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run valkey-check-rdb — make sure /var/lib/valkey/data/backup-dump.rdb exists first."}
	}
	if res.Code != 0 || !strings.Contains(res.Stdout, "RDB looks OK") {
		return LabStepResult{Passed: false, Message: "valkey-check-rdb didn't report a clean file — re-copy /var/lib/valkey/data/dump.rdb to /var/lib/valkey/data/backup-dump.rdb and try again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: valkey-check-rdb verified the backup file is structurally sound."}
}

// checkValkeyClusterShrunk passes once exactly one of the four members has
// been fully removed (CLUSTER RESET leaves it seeing only itself) and the
// remaining three still cover all 16384 slots between them. Remembers the
// removed node's ID via LabRun.InitialLeaderNode (the same free-slot reuse
// as the manual-failover lab) so the next step can confirm that SAME node
// rejoins, not just any node.
func (a *App) checkValkeyClusterShrunk(ctx context.Context, run LabRun, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) < 4 {
		return LabStepResult{Passed: false, Message: "Not all four Valkey nodes are running yet — wait for the cluster to finish deploying."}
	}
	var survivors []Deployment
	var removed *Deployment
	survivorSlots := 0
	for i, d := range running {
		pw := valkeyPasswordFor(d)
		nodes, ok := a.fetchValkeyClusterNodes(ctx, d.ContainerID, pw)
		if !ok {
			return LabStepResult{Passed: false, Message: "Could not read " + nodeLabel(doc, d.NodeID) + "'s cluster status."}
		}
		if len(nodes) <= 1 {
			removed = &running[i]
			continue
		}
		survivors = append(survivors, d)
		for _, n := range nodes {
			if n.Master == "" && n.hasFlag("myself") {
				survivorSlots += n.SlotCount
			}
		}
	}
	if removed == nil || len(survivors) != 3 {
		return LabStepResult{Passed: false, Message: "The cluster still has all four members — reshard one master's slots away entirely, then del-node it, then check again."}
	}
	if survivorSlots != 16384 {
		return LabStepResult{Passed: false, Message: "The remaining three members don't cover all 16384 slots yet (currently " + strconv.Itoa(survivorSlots) + ") — make sure the removed node's slots were fully reshared before removing it."}
	}
	a.store.SetLabRunLeader(run.ID, removed.NodeID)
	return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, removed.NodeID) + " was removed from the cluster, and the remaining three members cover all 16384 slots."}
}

// checkValkeyClusterGrown passes once the specific node removed in the
// previous step (LabRun.InitialLeaderNode) has rejoined the cluster and owns
// a real number of slots again — proof add-node plus a follow-up reshard
// actually worked, not just that some node somewhere owns slots.
func (a *App) checkValkeyClusterGrown(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no node has been removed from the cluster yet."}
	}
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	deps, err := a.store.ListDeployments(st.ID)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Failed to read the lab stack's deployments."}
	}
	var target Deployment
	for _, d := range deps {
		if d.NodeID == run.InitialLeaderNode {
			target = d
		}
	}
	if target.ContainerID == "" || target.State != DeployRunning {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, run.InitialLeaderNode) + " isn't running — wait for it and check again."}
	}
	pw := valkeyPasswordFor(target)
	nodes, ok := a.fetchValkeyClusterNodes(ctx, target.ContainerID, pw)
	if !ok {
		return LabStepResult{Passed: false, Message: "Could not read " + nodeLabel(doc, run.InitialLeaderNode) + "'s cluster status."}
	}
	if len(nodes) <= 1 {
		return LabStepResult{Passed: false, Message: nodeLabel(doc, run.InitialLeaderNode) + " hasn't rejoined the cluster yet — run add-node, then check again."}
	}
	for _, n := range nodes {
		if n.hasFlag("myself") {
			if n.SlotCount == 0 {
				return LabStepResult{Passed: false, Message: nodeLabel(doc, run.InitialLeaderNode) + " has rejoined but owns no slots yet — reshard some slots onto it, then check again."}
			}
			return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, run.InitialLeaderNode) + " rejoined the cluster and now owns " + strconv.Itoa(n.SlotCount) + " slots."}
		}
	}
	return LabStepResult{Passed: false, Message: "Could not confirm " + nodeLabel(doc, run.InitialLeaderNode) + "'s current status — wait a moment and check again."}
}

// checkValkeyCrossSlotErrorSeen passes once INFO errorstats shows at least
// one recorded CROSSSLOT error on any node — direct evidence the learner
// actually hit the error, not just that they believe they did.
func (a *App) checkValkeyCrossSlotErrorSeen(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "errorstats"}, nil)
		if err != nil || res.Code != 0 {
			continue
		}
		if strings.Contains(res.Stdout, "errorstat_CROSSSLOT") {
			return LabStepResult{Passed: true, Message: "Confirmed: a CROSSSLOT error was recorded — a multi-key command hit keys in different slots."}
		}
	}
	return LabStepResult{Passed: false, Message: "No CROSSSLOT error recorded yet on any node — run an MSET across two unrelated keys, then check again."}
}

// checkValkeyHashTagFixed passes once both hash-tagged keys are actually set
// and co-located on the same node — found by trying MGET on each node in
// turn and taking the one that doesn't error (the one that owns the slot).
func (a *App) checkValkeyHashTagFixed(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		res, err := a.engCtx(ctx).Exec(ctx, d.ContainerID,
			[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "MGET", "tagged:{demo}:a", "tagged:{demo}:b"}, nil)
		if err != nil || res.Code != 0 {
			continue
		}
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) == 2 && strings.TrimSpace(lines[0]) == "1" && strings.TrimSpace(lines[1]) == "2" {
			return LabStepResult{Passed: true, Message: "Confirmed: both hash-tagged keys landed on the same node and hold the values from the MSET."}
		}
	}
	return LabStepResult{Passed: false, Message: "tagged:{demo}:a and tagged:{demo}:b aren't both set yet — run the MSET with the {demo} hash tag, then check again."}
}

// checkValkeyReplicationEstablished passes once valkey-b is a connected
// replica of valkey-a (role:slave, master_link_status:up) and has actually
// received repl:marker via replication.
func (a *App) checkValkeyReplicationEstablished(ctx context.Context, st Stack) LabStepResult {
	b, ok := a.valkeyNodeDeployment(st, "lab-valkey-b")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-b isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(b)
	res, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "replication"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read valkey-b's replication status — is it still starting up?"}
	}
	if !strings.Contains(res.Stdout, "role:slave") || !strings.Contains(res.Stdout, "master_link_status:up") {
		return LabStepResult{Passed: false, Message: "valkey-b isn't a connected replica yet — run REPLICAOF valkey-a 6379 on valkey-b, wait a few seconds, then check again."}
	}
	getRes, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "GET", "repl:marker"}, nil)
	if err != nil || getRes.Code != 0 || strings.TrimSpace(getRes.Stdout) != "hello" {
		return LabStepResult{Passed: false, Message: "valkey-b is replicating but repl:marker hasn't arrived yet — write it on valkey-a first, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: valkey-b is a connected replica of valkey-a and has replicated repl:marker."}
}

// checkValkeyReplicaReadOnly passes once a direct write to valkey-b is
// actually rejected with READONLY — proof the replica enforces read-only
// mode rather than just being documented as such.
func (a *App) checkValkeyReplicaReadOnly(ctx context.Context, st Stack) LabStepResult {
	b, ok := a.valkeyNodeDeployment(st, "lab-valkey-b")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-b isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(b)
	res, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "SET", "repl:direct", "nope"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not test valkey-b's write access."}
	}
	// valkey-cli exits 0 even when the server replies with an error
	// (READONLY included) — check the reply text, not the exit code.
	out := strings.TrimSpace(res.Stdout)
	if out == "OK" {
		return LabStepResult{Passed: false, Message: "valkey-b accepted a direct write — confirm it's actually a replica of valkey-a (REPLICAOF valkey-a 6379) first."}
	}
	if !strings.Contains(strings.ToUpper(out), "READONLY") {
		return LabStepResult{Passed: false, Message: "The write was rejected, but not with the expected READONLY error — check valkey-b's replication status."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: valkey-b rejects direct writes with READONLY — it's correctly serving as a read-only replica."}
}

// checkValkeySentinelWatching passes once valkey-b is replicating from
// valkey-a AND a Sentinel process reachable on valkey-b's own network
// namespace reports mymaster as healthy (not s_down/o_down).
func (a *App) checkValkeySentinelWatching(ctx context.Context, st Stack) LabStepResult {
	b, ok := a.valkeyNodeDeployment(st, "lab-valkey-b")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-b isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(b)
	replRes, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "replication"}, nil)
	if err != nil || replRes.Code != 0 || !strings.Contains(replRes.Stdout, "role:slave") {
		return LabStepResult{Passed: false, Message: "valkey-b isn't replicating from valkey-a yet — run REPLICAOF valkey-a 6379 on valkey-b first, then check again."}
	}
	sentRes, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-p", "26379", "SENTINEL", "MASTERS"}, nil)
	if err != nil || sentRes.Code != 0 || !strings.Contains(sentRes.Stdout, "mymaster") {
		return LabStepResult{Passed: false, Message: "Sentinel isn't running or isn't monitoring mymaster yet — start it with the config from the lab instructions, then check again."}
	}
	if strings.Contains(sentRes.Stdout, "s_down") || strings.Contains(sentRes.Stdout, "o_down") {
		return LabStepResult{Passed: false, Message: "Sentinel is running but reports mymaster as down — confirm valkey-a is actually up and reachable."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: Sentinel is running on valkey-b and monitoring mymaster as healthy."}
}

// checkValkeySentinelFailedOver passes once valkey-b reports role:master —
// proof Sentinel detected valkey-a's disappearance and actually promoted
// valkey-b on its own, with no manual command from the learner.
func (a *App) checkValkeySentinelFailedOver(ctx context.Context, st Stack) LabStepResult {
	b, ok := a.valkeyNodeDeployment(st, "lab-valkey-b")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-b isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(b)
	res, err := a.engCtx(ctx).Exec(ctx, b.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "replication"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read valkey-b's replication status — is it still starting up?"}
	}
	if !strings.Contains(res.Stdout, "role:master") {
		return LabStepResult{Passed: false, Message: "valkey-b is still a replica — shut down valkey-a (SHUTDOWN NOSAVE) and give Sentinel a few seconds to promote valkey-b, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: valkey-b is now master — Sentinel detected valkey-a's failure and promoted it automatically."}
}

// valkeyCatLog execs `cat` on a fixed log-file path a lab's instructions had
// the learner redirect a background command's output into — used by every
// check that verifies a Pub/Sub or keyspace-notification delivery the
// learner already triggered from their own terminal, rather than the check
// re-running it. Missing file is not an error (`2>/dev/null` + `|| true`)
// since the learner may not have reached that step yet.
func (a *App) valkeyCatLog(ctx context.Context, containerID, path string) string {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", "cat " + path + " 2>/dev/null || true"}, nil)
	if err != nil {
		return ""
	}
	return res.Stdout
}

// checkValkeyStreamGroupProcessed passes once lab:processors has zero
// pending entries while the stream itself has at least one — proof the
// learner's XREADGROUP + XACK sequence actually delivered and acknowledged
// a real entry, not just that the group exists.
func (a *App) checkValkeyStreamGroupProcessed(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	lenRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "XLEN", "lab:orders"}, nil)
	if err != nil || lenRes.Code != 0 || strings.TrimSpace(lenRes.Stdout) == "0" {
		return LabStepResult{Passed: false, Message: "lab:orders doesn't have any entries yet — run XADD, then check again."}
	}
	pendRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "XPENDING", "lab:orders", "lab:processors"}, nil)
	if err != nil || pendRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "lab:processors doesn't exist yet — create it with XGROUP CREATE, then check again."}
	}
	lines := strings.Split(strings.TrimSpace(pendRes.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "0" {
		return LabStepResult{Passed: false, Message: "lab:processors still has a pending, unacknowledged entry — run XACK with the entry ID XREADGROUP printed, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lab:orders has entries and lab:processors has none pending — the entry was delivered and acknowledged."}
}

// checkValkeyStreamReclaimed passes once XPENDING's extended form shows
// consumer-2 (not consumer-1) owning a pending entry — proof XCLAIM actually
// transferred ownership rather than the learner just believing it worked.
func (a *App) checkValkeyStreamReclaimed(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"valkey-cli", "-a", pw, "--no-auth-warning", "XPENDING", "lab:orders", "lab:processors", "-", "+", "10"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read lab:processors' pending list — is lab:orders' second entry added and read yet?"}
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == "consumer-2" {
			return LabStepResult{Passed: true, Message: "Confirmed: consumer-2 now owns a previously-pending entry — XCLAIM reassigned it."}
		}
	}
	return LabStepResult{Passed: false, Message: "No entry is owned by consumer-2 yet — read the second entry as consumer-1 without acking it, wait a couple seconds, then XCLAIM it as consumer-2."}
}

// checkValkeyPubSubBroadcast passes once any cluster member's /tmp/pubsub.log
// contains the message published from a different node — proof regular
// PUBLISH really did broadcast across the cluster bus.
func (a *App) checkValkeyPubSubBroadcast(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		if strings.Contains(a.valkeyCatLog(ctx, d.ContainerID, "/tmp/pubsub.log"), "hello-cluster") {
			return LabStepResult{Passed: true, Message: "Confirmed: hello-cluster was delivered to a subscriber, even though it was published from a different node."}
		}
	}
	return LabStepResult{Passed: false, Message: "hello-cluster hasn't shown up in /tmp/pubsub.log yet — start the background SUBSCRIBE on one node, then PUBLISH from a different one, then check again."}
}

// checkValkeySPubSubDelivered passes once any cluster member's
// /tmp/shardpubsub.log contains the sharded message — proof SSUBSCRIBE and
// SPUBLISH on the slot-owning node actually delivered.
func (a *App) checkValkeySPubSubDelivered(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		if strings.Contains(a.valkeyCatLog(ctx, d.ContainerID, "/tmp/shardpubsub.log"), "hello-shard") {
			return LabStepResult{Passed: true, Message: "Confirmed: hello-shard was delivered over the sharded Pub/Sub channel."}
		}
	}
	return LabStepResult{Passed: false, Message: "hello-shard hasn't shown up in /tmp/shardpubsub.log yet — make sure you SSUBSCRIBE and SPUBLISH on the node that actually owns lab:shardnews' slot, then check again."}
}

// checkValkeyExpiredEventCaught passes once /tmp/notif.log shows the expired
// key's name — proof the keyevent notification for expiry actually fired
// and was received.
func (a *App) checkValkeyExpiredEventCaught(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	if strings.Contains(a.valkeyCatLog(ctx, dep.ContainerID, "/tmp/notif.log"), "lab:expiring") {
		return LabStepResult{Passed: true, Message: "Confirmed: an expired-key event for lab:expiring was received."}
	}
	return LabStepResult{Passed: false, Message: "lab:expiring hasn't shown up in /tmp/notif.log yet — enable notify-keyspace-events Ex, start the PSUBSCRIBE in the background, then SET the key with a short PX, then check again."}
}

// checkValkeyKeyspaceEventCaught passes once /tmp/notif2.log shows the "set"
// event name — proof the keyspace-prefixed (per-key) notification class
// delivered, distinct from the keyevent class the previous step used.
func (a *App) checkValkeyKeyspaceEventCaught(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	for _, line := range strings.Split(a.valkeyCatLog(ctx, dep.ContainerID, "/tmp/notif2.log"), "\n") {
		if strings.TrimSpace(line) == "set" {
			return LabStepResult{Passed: true, Message: "Confirmed: a set event for lab:tracked was received on the keyspace-prefixed channel."}
		}
	}
	return LabStepResult{Passed: false, Message: "No set event found in /tmp/notif2.log yet — enable notify-keyspace-events KEA, PSUBSCRIBE to __keyspace@0__:lab:tracked in the background, then SET that key, then check again."}
}

// valkeyClientListField extracts one field's value (e.g. "flags", "resp")
// from a single CLIENT LIST line's space-separated key=value tokens.
func valkeyClientListField(line, field string) string {
	prefix := field + "="
	for _, tok := range strings.Fields(line) {
		if strings.HasPrefix(tok, prefix) {
			return strings.TrimPrefix(tok, prefix)
		}
	}
	return ""
}

// checkValkeyTrackingEnabled passes once CLIENT LIST shows a RESP3 client
// with tracking on (flags contains "t", resp=3) and INFO stats confirms it's
// actually tracking at least one key — proof CLIENT TRACKING on plus the GET
// really registered, not just that the connection is open.
func (a *App) checkValkeyTrackingEnabled(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLIENT", "LIST"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read CLIENT LIST — is the node still starting up?"}
	}
	found := false
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(valkeyClientListField(line, "flags"), "t") && valkeyClientListField(line, "resp") == "3" {
			found = true
			break
		}
	}
	if !found {
		return LabStepResult{Passed: false, Message: "No RESP3 client with tracking on found yet — start the background `valkey-cli -3 ... CLIENT TRACKING on` session from the lab instructions, then check again."}
	}
	statsRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "stats"}, nil)
	if err != nil || statsRes.Code != 0 || valkeyInfoInt(statsRes.Stdout, "tracking_total_keys") < 1 {
		return LabStepResult{Passed: false, Message: "A tracking client is connected, but it isn't tracking any keys yet — make sure the GET lab:cached actually ran on that same connection."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a RESP3 client has tracking on and is tracking lab:cached."}
}

// checkValkeyTrackingInvalidated passes once tracking_total_keys has dropped
// back to 0 (the server invalidated the tracked key) while the tracking
// client is still connected and the key holds its new value — proof
// invalidation actually happened, not that the tracking client disconnected.
func (a *App) checkValkeyTrackingInvalidated(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	listRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLIENT", "LIST"}, nil)
	if err != nil || listRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read CLIENT LIST — is the node still starting up?"}
	}
	stillTracking := false
	for _, line := range strings.Split(listRes.Stdout, "\n") {
		if strings.Contains(valkeyClientListField(line, "flags"), "t") {
			stillTracking = true
			break
		}
	}
	if !stillTracking {
		return LabStepResult{Passed: false, Message: "The tracking client from the previous step isn't connected anymore — redo that step's background session, then this one, without a long gap between them."}
	}
	getRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "GET", "lab:cached"}, nil)
	if err != nil || getRes.Code != 0 || strings.TrimSpace(getRes.Stdout) != "updated" {
		return LabStepResult{Passed: false, Message: "lab:cached isn't set to \"updated\" yet — SET it from a separate connection, then check again."}
	}
	statsRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "stats"}, nil)
	if err != nil || statsRes.Code != 0 || valkeyInfoInt(statsRes.Stdout, "tracking_total_keys") != 0 {
		return LabStepResult{Passed: false, Message: "lab:cached was updated, but the tracking table hasn't cleared it yet — wait a moment and check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lab:cached was updated and the server invalidated it from the tracking table."}
}

// checkValkeyFunctionLoaded passes once FUNCTION LIST shows both registered
// functions and fn:demo reflects a real FCALL having run.
func (a *App) checkValkeyFunctionLoaded(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	listRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "FUNCTION", "LIST"}, nil)
	if err != nil || listRes.Code != 0 || !strings.Contains(listRes.Stdout, "safe_get") || !strings.Contains(listRes.Stdout, "unsafe_set") {
		return LabStepResult{Passed: false, Message: "The lablib library isn't loaded with both functions yet — run FUNCTION LOAD with the library from the lab instructions, then check again."}
	}
	getRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "GET", "fn:demo"}, nil)
	if err != nil || getRes.Code != 0 || strings.TrimSpace(getRes.Stdout) != "hello" {
		return LabStepResult{Passed: false, Message: "fn:demo isn't set to \"hello\" yet — call FCALL unsafe_set 1 fn:demo hello, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: lablib is loaded with both functions, and fn:demo reflects a real FCALL."}
}

// checkValkeyFunctionReadonlyEnforced passes once FCALL_RO succeeds on the
// no-writes-flagged function and is actually rejected on the unflagged one —
// checking the reply text, not just exit codes, per the same valkey-cli
// quirk every ACL/READONLY check in this file already accounts for.
func (a *App) checkValkeyFunctionReadonlyEnforced(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	safeRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "FCALL_RO", "safe_get", "1", "fn:demo"}, nil)
	if err != nil || safeRes.Code != 0 || strings.HasPrefix(strings.TrimSpace(safeRes.Stdout), "ERR") {
		return LabStepResult{Passed: false, Message: "FCALL_RO safe_get failed — confirm lablib is loaded (previous step) before retrying this one."}
	}
	unsafeRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "FCALL_RO", "unsafe_set", "1", "fn:demo", "nope"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not test FCALL_RO against unsafe_set."}
	}
	if !strings.Contains(strings.ToUpper(unsafeRes.Stdout), "WRITE") {
		return LabStepResult{Passed: false, Message: "FCALL_RO unsafe_set didn't get rejected for its write flag — confirm unsafe_set was registered WITHOUT the no-writes flag, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: FCALL_RO runs the no-writes-flagged function but rejects the one that can write."}
}

// valkeyRawClusterNodes execs CLUSTER NODES and returns its unparsed text —
// the manual-migration checks need the bracketed [<slot>-<-<id>] /
// [<slot>->-<id>] migration annotations that parseValkeyClusterNodes
// deliberately discards.
func (a *App) valkeyRawClusterNodes(ctx context.Context, containerID, password string) (string, bool) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"valkey-cli", "-a", password, "--no-auth-warning", "CLUSTER", "NODES"}, nil)
	if err != nil || res.Code != 0 {
		return "", false
	}
	return res.Stdout, true
}

// checkValkeyMigrationStarted passes once one running member's own line
// shows the MIGRATING annotation for {migrate}demo's slot and another shows
// the matching IMPORTING annotation — proof both sides of the two-sided
// consent were actually set, not just attempted.
func (a *App) checkValkeyMigrationStarted(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	pw := valkeyPasswordFor(running[0])
	slotRes, err := a.engCtx(ctx).Exec(ctx, running[0].ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLUSTER", "KEYSLOT", "{migrate}demo"}, nil)
	if err != nil || slotRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not compute {migrate}demo's slot — is the cluster still forming?"}
	}
	slot := strings.TrimSpace(slotRes.Stdout)
	migrating, importing := false, false
	for _, d := range running {
		text, ok := a.valkeyRawClusterNodes(ctx, d.ContainerID, valkeyPasswordFor(d))
		if !ok {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(line, "myself") {
				continue
			}
			if strings.Contains(line, "["+slot+"->-") {
				migrating = true
			}
			if strings.Contains(line, "["+slot+"-<-") {
				importing = true
			}
		}
	}
	if !migrating || !importing {
		return LabStepResult{Passed: false, Message: "Slot " + slot + " isn't marked MIGRATING on the source and IMPORTING on the destination yet — run both CLUSTER SETSLOT commands from the lab instructions, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: slot " + slot + " is marked MIGRATING on the source and IMPORTING on the destination."}
}

// checkValkeyMigrationFinalized passes once some member now owns
// {migrate}demo's slot outright (in its plain ranges, no annotation needed
// since parseValkeyClusterNodes already ignores them) and serves the key
// directly with no MOVED redirect.
func (a *App) checkValkeyMigrationFinalized(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	pw := valkeyPasswordFor(running[0])
	slotRes, err := a.engCtx(ctx).Exec(ctx, running[0].ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLUSTER", "KEYSLOT", "{migrate}demo"}, nil)
	if err != nil || slotRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not compute {migrate}demo's slot — is the cluster still forming?"}
	}
	slotNum, convErr := strconv.Atoi(strings.TrimSpace(slotRes.Stdout))
	if convErr != nil {
		return LabStepResult{Passed: false, Message: "Could not parse CLUSTER KEYSLOT output."}
	}
	for _, d := range running {
		nodes, ok := a.fetchValkeyClusterNodes(ctx, d.ContainerID, valkeyPasswordFor(d))
		if !ok {
			continue
		}
		for _, n := range nodes {
			if !n.hasFlag("myself") || !n.rangesContain(slotNum) {
				continue
			}
			getRes, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", valkeyPasswordFor(d), "--no-auth-warning", "GET", "{migrate}demo"}, nil)
			if err != nil || getRes.Code != 0 {
				return LabStepResult{Passed: false, Message: "Could not query the node that now owns slot " + strconv.Itoa(slotNum) + "."}
			}
			out := strings.TrimSpace(getRes.Stdout)
			if out != "before" {
				return LabStepResult{Passed: false, Message: nodeLabel(doc, d.NodeID) + " owns slot " + strconv.Itoa(slotNum) + " now, but doesn't have the key yet — run MIGRATE, then finalize with CLUSTER SETSLOT NODE on every node."}
			}
			return LabStepResult{Passed: true, Message: "Confirmed: " + nodeLabel(doc, d.NodeID) + " owns slot " + strconv.Itoa(slotNum) + " outright and serves {migrate}demo directly."}
		}
	}
	return LabStepResult{Passed: false, Message: "No node owns slot " + strconv.Itoa(slotNum) + " outright yet — finalize with CLUSTER SETSLOT <slot> NODE <dest-id> on every node in the cluster, then check again."}
}

// checkValkeyClusterDownOnCoverageLoss passes once some reachable member
// reports cluster_state:fail and actually refuses a write with CLUSTERDOWN
// — proof a real coverage gap took the whole cluster down for writes, not
// just the missing shard.
func (a *App) checkValkeyClusterDownOnCoverageLoss(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		infoRes, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLUSTER", "INFO"}, nil)
		if err != nil || infoRes.Code != 0 {
			continue // likely the node we just crashed
		}
		if !strings.Contains(infoRes.Stdout, "cluster_state:fail") {
			continue
		}
		setRes, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "SET", "some:key", "val"}, nil)
		if err != nil || !strings.Contains(setRes.Stdout, "CLUSTERDOWN") {
			return LabStepResult{Passed: false, Message: "The cluster reports cluster_state:fail, but a write wasn't refused with CLUSTERDOWN — wait a moment and check again."}
		}
		return LabStepResult{Passed: true, Message: "Confirmed: cluster_state is fail and writes are refused with CLUSTERDOWN, cluster-wide, from the single missing master."}
	}
	return LabStepResult{Passed: false, Message: "No surviving node reports cluster_state:fail yet — crash one master with SHUTDOWN NOSAVE and wait about 10 seconds for the others to detect it, then check again."}
}

// checkValkeyPartialCoverageRestored passes once at least two members (the
// two the learner ran CLUSTER FORGET on) have cluster-require-full-coverage
// set to no with cluster_state reading ok, and a write to a healthy slot
// actually succeeds. It deliberately doesn't require this of every running
// member — the originally-crashed node rejoins on its own within a second or
// two (systemd restarts its Valkey service and its cluster membership file
// is intact), and was never told to forget anyone, so it's expected to still read yes
// and isn't part of what this step is checking.
func (a *App) checkValkeyPartialCoverageRestored(ctx context.Context, st Stack) LabStepResult {
	doc, _, ok := valkeyFrameFromStack(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey Cluster found in this lab's stack."}
	}
	running, err := a.valkeyRunningMembers(st, doc)
	if err != nil || len(running) == 0 {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for the cluster to finish deploying."}
	}
	transitioned := 0
	var writeConfirmed bool
	for _, d := range running {
		pw := valkeyPasswordFor(d)
		v, ok := a.valkeyConfigGet(ctx, d.ContainerID, pw, "cluster-require-full-coverage")
		if !ok || v != "no" {
			continue // unreachable, or the untouched third node — not part of this check
		}
		infoRes, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLUSTER", "INFO"}, nil)
		if err != nil || infoRes.Code != 0 || !strings.Contains(infoRes.Stdout, "cluster_state:ok") {
			continue
		}
		transitioned++
		if !writeConfirmed {
			setRes, err := a.engCtx(ctx).Exec(ctx, d.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "SET", "some:key", "val"}, nil)
			if err == nil && strings.TrimSpace(setRes.Stdout) == "OK" {
				writeConfirmed = true
			}
		}
	}
	if transitioned < 2 {
		return LabStepResult{Passed: false, Message: "Fewer than two nodes have cluster-require-full-coverage off with cluster_state:ok yet — run CONFIG SET cluster-require-full-coverage no on both nodes you ran CLUSTER FORGET on, then check again."}
	}
	if !writeConfirmed {
		return LabStepResult{Passed: false, Message: "cluster_state is ok, but a write to a healthy slot hasn't succeeded yet — try SET some:key val, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: both nodes that forgot the missing master have cluster-require-full-coverage off, report cluster_state:ok, and accept writes to healthy slots."}
}

// checkValkeyPartialResyncOccurred passes once valkey-a's INFO stats shows
// at least one partial resync — proof the CLIENT KILL-simulated blip resumed
// from the backlog at least once. Doesn't require sync_full to still be
// exactly 1: a learner who kills the connection before the initial link is
// actually up will trigger an extra full resync first, which is a mistake
// worth letting them recover from by just trying the blip again once the
// link is up, not a dead end requiring the whole stack to be redeployed.
// Records the current sync_full as this run's baseline (reusing
// LabRun.InitialLeaderNode as a free string slot, the same pattern the
// manual-failover and cluster-resize labs use) so the next step can require
// a genuine further increase regardless of what happened here.
func (a *App) checkValkeyPartialResyncOccurred(ctx context.Context, run LabRun, st Stack) LabStepResult {
	aDep, ok := a.valkeyNodeDeployment(st, "lab-valkey-a")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-a isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(aDep)
	res, err := a.engCtx(ctx).Exec(ctx, aDep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "stats"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read valkey-a's INFO stats — is it still starting up?"}
	}
	syncFull := valkeyInfoInt(res.Stdout, "sync_full")
	syncPartial := valkeyInfoInt(res.Stdout, "sync_partial_ok")
	if syncFull < 1 {
		return LabStepResult{Passed: false, Message: "valkey-b hasn't connected as a replica yet — run REPLICAOF valkey-a 6379 on valkey-b first, then check again."}
	}
	if syncPartial < 1 {
		return LabStepResult{Passed: false, Message: "No partial resync recorded yet — make sure valkey-b's INFO replication shows master_link_status:up first, THEN run CLIENT KILL TYPE replica on valkey-a to simulate the blip, then check again."}
	}
	a.store.SetLabRunLeader(run.ID, strconv.Itoa(syncFull))
	return LabStepResult{Passed: true, Message: "Confirmed: sync_partial_ok is " + strconv.Itoa(syncPartial) + " — the blip resumed from the backlog instead of a full resync."}
}

// checkValkeyFullResyncAfterOutage passes once sync_full has advanced past
// the baseline the previous step recorded — proof the real crash-and-restart
// genuinely cost an additional full resync, contrasting with the previous
// step's partial one, regardless of the exact numbers either step landed on.
func (a *App) checkValkeyFullResyncAfterOutage(ctx context.Context, run LabRun, st Stack) LabStepResult {
	if run.InitialLeaderNode == "" {
		return LabStepResult{Passed: false, Message: "Complete the previous step first — no partial-resync baseline recorded yet."}
	}
	baseline, convErr := strconv.Atoi(run.InitialLeaderNode)
	if convErr != nil {
		return LabStepResult{Passed: false, Message: "Could not read this run's baseline — complete the previous step again."}
	}
	aDep, ok := a.valkeyNodeDeployment(st, "lab-valkey-a")
	if !ok {
		return LabStepResult{Passed: false, Message: "valkey-a isn't running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(aDep)
	res, err := a.engCtx(ctx).Exec(ctx, aDep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "INFO", "stats"}, nil)
	if err != nil || res.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read valkey-a's INFO stats — is it still starting up?"}
	}
	syncFull := valkeyInfoInt(res.Stdout, "sync_full")
	if syncFull <= baseline {
		return LabStepResult{Passed: false, Message: "sync_full is still at its post-blip baseline (" + strconv.Itoa(baseline) + ") — on valkey-b run SHUTDOWN NOSAVE, wait for it to restart, and reconnect with REPLICAOF valkey-a 6379."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: sync_full advanced from " + strconv.Itoa(baseline) + " to " + strconv.Itoa(syncFull) + " — the real outage cost a full resync, unlike the earlier blip."}
}

// checkValkeyTLSEnabled passes once a direct TLS+mTLS PING against the
// certs the learner generated actually returns PONG — proof the runtime
// CONFIG SET genuinely brought the TLS listener up, checked independently
// rather than trusting the learner's own terminal output.
func (a *App) checkValkeyTLSEnabled(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{
		"valkey-cli", "--tls", "--cert", "/tmp/tls/cert.pem", "--key", "/tmp/tls/key.pem", "--cacert", "/tmp/tls/cert.pem",
		"-p", "6380", "-a", pw, "--no-auth-warning", "PING",
	}, nil)
	if err != nil || res.Code != 0 || strings.TrimSpace(res.Stdout) != "PONG" {
		return LabStepResult{Passed: false, Message: "A TLS connection on port 6380 with the generated certificate isn't answering PING yet — generate the cert, chmod the key readable, then CONFIG SET the tls-* settings from the lab instructions."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: Valkey answers PING over a TLS connection on port 6380."}
}

// checkValkeyMutualTLSEnforced passes once a TLS connection presenting no
// client certificate is actually rejected (tls-auth-clients still yes) while
// one presenting a valid certificate succeeds — checked by making both
// connection attempts directly, the same pattern the ACL/READONLY checks use
// to verify enforcement rather than configuration.
func (a *App) checkValkeyMutualTLSEnforced(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	noCertRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{
		"valkey-cli", "--tls", "--cacert", "/tmp/tls/cert.pem", "-p", "6380", "-a", pw, "--no-auth-warning", "PING",
	}, nil)
	rejected := err != nil || noCertRes.Code != 0 || strings.TrimSpace(noCertRes.Stdout) != "PONG"
	if !rejected {
		return LabStepResult{Passed: false, Message: "A TLS connection with no client certificate succeeded — confirm tls-auth-clients is still set to yes (the default; don't disable it for this lab)."}
	}
	withCertRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{
		"valkey-cli", "--tls", "--cert", "/tmp/tls/cert.pem", "--key", "/tmp/tls/key.pem", "--cacert", "/tmp/tls/cert.pem",
		"-p", "6380", "-a", pw, "--no-auth-warning", "PING",
	}, nil)
	if err != nil || withCertRes.Code != 0 || strings.TrimSpace(withCertRes.Stdout) != "PONG" {
		return LabStepResult{Passed: false, Message: "The no-certificate connection was correctly rejected, but the with-certificate connection isn't succeeding — confirm TLS is enabled from the previous step."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: a TLS connection without a client certificate is rejected, and one with a valid certificate succeeds — mutual TLS is enforced."}
}

// checkValkeyClientKilled passes once /tmp/blpop.log shows the
// connection-closed error CLIENT KILL produces (not a normal BLPOP timeout
// reply) and no client with cmd=blpop remains connected.
func (a *App) checkValkeyClientKilled(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	log := a.valkeyCatLog(ctx, dep.ContainerID, "/tmp/blpop.log")
	if !strings.Contains(strings.ToLower(log), "closed") {
		return LabStepResult{Passed: false, Message: "/tmp/blpop.log doesn't show a connection-closed error yet — start the background BLPOP, find its id in CLIENT LIST, then CLIENT KILL ID it well before its 30-second timeout elapses."}
	}
	pw := valkeyPasswordFor(dep)
	listRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLIENT", "LIST"}, nil)
	if err != nil || listRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not read CLIENT LIST — is the node still starting up?"}
	}
	if strings.Contains(listRes.Stdout, "cmd=blpop") {
		return LabStepResult{Passed: false, Message: "A client with cmd=blpop is still connected — confirm you killed the right ID."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the BLPOP client's connection was closed by CLIENT KILL, and it's no longer listed."}
}

// checkValkeyClientPauseHeld passes once a write timed immediately after
// issuing CLIENT PAUSE ... WRITE actually took close to the full pause
// duration — the check performs the timed test itself (server-side state
// from CLIENT PAUSE doesn't persist for a later click to observe) rather
// than trusting the learner's own `time` output.
func (a *App) checkValkeyClientPauseHeld(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	pw := valkeyPasswordFor(dep)
	pauseRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "CLIENT", "PAUSE", "2000", "WRITE"}, nil)
	if err != nil || strings.TrimSpace(pauseRes.Stdout) != "OK" {
		return LabStepResult{Passed: false, Message: "CLIENT PAUSE didn't succeed — is the node still starting up?"}
	}
	start := time.Now()
	setRes, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-cli", "-a", pw, "--no-auth-warning", "SET", "lab:paused", "val"}, nil)
	elapsed := time.Since(start)
	if err != nil || setRes.Code != 0 {
		return LabStepResult{Passed: false, Message: "Could not test the paused write."}
	}
	if elapsed < 1500*time.Millisecond {
		return LabStepResult{Passed: false, Message: "The write returned after only " + elapsed.Round(time.Millisecond).String() + " — CLIENT PAUSE doesn't seem to have actually held it. Check again right after issuing CLIENT PAUSE 2000 WRITE."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: the write was held for " + elapsed.Round(time.Millisecond).String() + " — CLIENT PAUSE genuinely blocked it."}
}
