package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
    {"id":"lab-vk-3","type":"valkeycluster","label":"valkey-3","frameId":"lab-valkey-cluster","x":830,"y":66}
  ],
  "frames": [
    {"id":"lab-valkey-cluster","type":"valkeycluster","label":"lab-valkey","x":560,"y":20,"w":400,"h":138}
  ],
  "edges": [],
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
    {"id":"lab-vk-4","type":"valkeycluster","label":"valkey-4","frameId":"lab-valkey-cluster","x":958,"y":66}
  ],
  "frames": [
    {"id":"lab-valkey-cluster","type":"valkeycluster","label":"lab-valkey","x":560,"y":20,"w":528,"h":138}
  ],
  "edges": [],
  "view": {"x":0,"y":0,"z":1}
}`)

// labValkeyStandaloneDesign is a single standalone Valkey node + Intranet —
// no clustering involved, for labs about core Valkey operation (persistence,
// memory, ACLs, transactions, diagnostics, backup) that apply just as much
// to a lone instance as to any cluster member.
var labValkeyStandaloneDesign = json.RawMessage(`{
  "nodes": [
    {"id":"lab-intranet","type":"intranet","label":"Intranet","arch":"amd64","x":40,"y":40},
    {"id":"lab-valkey","type":"valkey","label":"valkey-1","x":300,"y":40}
  ],
  "frames": [],
  "edges": [],
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
    {"id":"lab-valkey-b","type":"valkey","label":"valkey-b","x":460,"y":40}
  ],
  "frames": [],
  "edges": [],
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
				Instructions: "Open a terminal on any Valkey node. Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT lab:hello` " +
					"to see which slot the key `lab:hello` hashes to. Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and " +
					"find which node owns that slot (the ranges after each master's line). If it isn't the node you're on, try writing it " +
					"directly anyway: `valkey-cli -a valkey_password --no-auth-warning SET lab:hello world` — you should see a `MOVED` reply " +
					"instead of `OK`. Now write it the way a real client would: `valkey-cli -c -a valkey_password --no-auth-warning SET " +
					"lab:hello world` — cluster mode (`-c`) follows the redirect for you. Click Check Work.",
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
					"(the long hex string at the start of each line) and current slot ranges. Pick a source and a destination master, then run " +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <source-id> --cluster-to " +
					"<dest-id> --cluster-slots 1000 --cluster-yes` on the source node. Once it finishes, run `CLUSTER NODES` again and confirm " +
					"the destination master's slot ranges grew and the source's shrank. Click Check Work.",
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

Once a replica exists, "CLUSTER FAILOVER" — run from the replica itself, not the master — requests a clean, coordinated promotion: the replica confirms it's caught up, the master stops accepting writes for a brief moment, and the replica takes over the master's slots and role. This is the Valkey Cluster equivalent of the Patroni curriculum's planned switchover — a voluntary handover between two healthy nodes, not a reaction to one disappearing.

Why this lab doesn't simulate a crash

Simulating an actual node failure convincingly needs the failure to last long enough for the survivors' gossip-based failure detector to notice (this cluster's cluster-node-timeout is 5 seconds) — reliably keeping a node down for that long from inside its own container (rather than stopping the container from outside, which a real operator could do but a lab terminal session can't) turns out to be far less clean than it sounds. A manual, voluntary failover exercises the same promotion mechanics without needing a contrived crash simulation.`,
		DesignTemplate: labValkeyClusterDesign,
		Steps: []LabStep{
			{
				ID:    "build-replica",
				Title: "Empty a master to turn it into a replica",
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and note the three masters' node IDs and slot " +
					"counts. Pick one to empty out (the \"source\") and one to receive its slots (the \"destination\"). On the source node, run " +
					"`valkey-cli --cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <source-id> --cluster-to " +
					"<dest-id> --cluster-slots <however many the source currently owns> --cluster-yes` — moving ALL of its slots, not just " +
					"some. Once it owns zero slots, run `CLUSTER NODES` again: it should now show up with a `slave` flag instead of `master`, " +
					"pointing at the destination — automatically, with nothing else for you to run. Click Check Work.",
				Hint: "If it still shows as `master` with 0 slots and hasn't converted, give it a few seconds — the automatic slave conversion happens on the next cluster cron cycle, not the instant the last slot moves.",
			},
			{
				ID:    "manual-failover",
				Title: "Promote the replica back with CLUSTER FAILOVER",
				Instructions: "Open a terminal on the node that just became a replica (from the previous step) and run " +
					"`valkey-cli -a valkey_password --no-auth-warning CLUSTER FAILOVER`. Wait a few seconds, then run `CLUSTER NODES` on any " +
					"node and confirm that same node now shows a `master` flag and owns the slots its former master used to hold — and that " +
					"former master now shows `slave` instead. Click Check Work.",
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
				Instructions: "Open a terminal on the Valkey node. Confirm the default first: " +
					"`valkey-cli -a valkey_password --no-auth-warning CONFIG GET appendfsync` (should show `everysec`). Now tighten it: " +
					"`valkey-cli -a valkey_password --no-auth-warning CONFIG SET appendfsync always`. Confirm it took with `CONFIG GET " +
					"appendfsync` again. Click Check Work.",
				Hint: "CONFIG SET takes effect immediately and persists only in memory — a real deployment would also update the config file so it survives a restart, but that's outside what this lab checks.",
			},
			{
				ID:    "observe-durability-cost",
				Title: "Watch the fsync cost show up in latency monitoring",
				Instructions: "Turn on Valkey's latency monitor: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET " +
					"latency-monitor-threshold 1` (track anything over 1ms). Generate some writes: `valkey-cli -a valkey_password " +
					"--no-auth-warning SET durability:test hello`, then a few more with different key names. Run `valkey-cli -a " +
					"valkey_password --no-auth-warning LATENCY HISTORY aof-fsync-always` — you should see recorded events. Click Check Work.",
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
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CONFIG SET maxmemory 12mb` then `valkey-cli -a " +
					"valkey_password --no-auth-warning CONFIG SET maxmemory-policy allkeys-lru`. Confirm both with `CONFIG GET maxmemory` " +
					"and `CONFIG GET maxmemory-policy`. Click Check Work.",
				Hint: "12mb leaves real headroom above Valkey's own baseline memory usage (a few MB just for the process itself) — a smaller ceiling can reject every write outright instead of giving eviction anything to work with.",
			},
			{
				ID:    "trigger-eviction",
				Title: "Fill past the ceiling and watch real eviction happen",
				Instructions: "Write enough data to exceed the ceiling — a shell loop is the fastest way: `for i in $(seq 1 2000); do " +
					"valkey-cli -a valkey_password --no-auth-warning SET \"evict:test:$i\" \"$(head -c 5000 /dev/urandom | base64)\" " +
					">/dev/null; done`. Once it finishes, run `valkey-cli -a valkey_password --no-auth-warning INFO stats | grep " +
					"evicted_keys` — it should be well above 0. Click Check Work.",
				Hint: "If evicted_keys is still 0, confirm maxmemory-policy is actually allkeys-lru (not the default noeviction) — with noeviction, writes past the ceiling just fail instead of evicting anything.",
			},
		},
	},
	{
		ID:          "valkey-acl",
		Title:       "Fine-Grained Access Control with ACLs",
		Description: "Every other lab in this curriculum uses one shared password with full access to everything. Create a user that can only read a specific key pattern, and prove the restriction actually holds.",
		Difficulty:  "Intermediate",
		Database:    "Valkey",
		Technology:  "Valkey",
		Category:    "Security & Access Control",
		TimeLimit:   "2h",
		LectureNotes: `Beyond a single shared password

"requirepass" (what every other lab's "-a valkey_password" authenticates against) is an all-or-nothing gate: anyone with the password can run any command against any key. Valkey's ACL system is the real access-control layer underneath that — multiple named users, each with their own password and their own precisely scoped permissions, the same system this app's own PMM integration already uses under the hood to create a read-only monitoring user.

Three things an ACL rule restricts independently

"ACL SETUSER <name> on >password <key-pattern> <command-permission>" combines three separate restrictions: whether the user can authenticate at all ("on"/"off"), which keys they can touch ("~app:*" — a glob pattern, "~*" for unrestricted), and which commands they're allowed to run ("+@read" grants the whole read-only command category; "+get" would grant only GET specifically). A user can be read-only on one key pattern and have no access at all outside it — both restrictions apply simultaneously, not as alternatives.

Why this matters operationally

A monitoring tool, a reporting job, or a third-party integration should never hold a credential that can run FLUSHALL or read keys outside its own namespace — even though that's exactly what happens by default if every integration just shares the same admin password. ACLs are how you actually give each consumer of a shared Valkey instance only the access it needs.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "create-restricted-user",
				Title: "Create a read-only user scoped to one key pattern",
				Instructions: "First, put some data in the pattern this user will be allowed to read: `valkey-cli -a valkey_password " +
					"--no-auth-warning SET app:config hello`. Now create the restricted user: `valkey-cli -a valkey_password " +
					"--no-auth-warning ACL SETUSER app_readonly on >apppass \\~app:* +@read`. Confirm with `valkey-cli -a valkey_password " +
					"--no-auth-warning ACL LIST` — you should see a line for app_readonly. Click Check Work.",
				Hint: "The `~` before the key pattern needs escaping (`\\~`) in most shells so it isn't interpreted specially — if ACL LIST doesn't show the pattern you expected, check for that.",
			},
			{
				ID:    "verify-enforcement",
				Title: "Prove the restriction actually holds",
				Instructions: "As the new user, confirm reading inside the pattern works: `valkey-cli --user app_readonly -a apppass " +
					"--no-auth-warning GET app:config` (should return `hello`, no error). Now confirm writing is denied even inside the " +
					"pattern: `valkey-cli --user app_readonly -a apppass --no-auth-warning SET app:config bye` (should return a NOPERM " +
					"error). Click Check Work.",
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
				Instructions: "Open a terminal and run: `valkey-cli -a valkey_password --no-auth-warning` to get an interactive prompt (so " +
					"MULTI/EXEC share one connection). Inside it, run: `MULTI`, then `SET tx:counter 10`, then `INCR tx:counter`, then " +
					"`EXEC` — you should see both queued commands' results returned together. Exit with `exit`. Confirm from outside: " +
					"`valkey-cli -a valkey_password --no-auth-warning GET tx:counter` should show `11`. Click Check Work.",
				Hint: "MULTI/EXEC only works within a single connection — running each command as a separate `valkey-cli ... COMMAND` invocation opens a new connection each time and won't queue anything.",
			},
			{
				ID:    "atomic-lua",
				Title: "Enforce a limit atomically with a Lua script",
				Instructions: "Run this EVAL, which only increments a counter if it's still under 5, atomically: " +
					"`valkey-cli -a valkey_password --no-auth-warning EVAL \"local v = tonumber(redis.call('GET', KEYS[1]) or '0'); if v < 5 " +
					"then return redis.call('INCR', KEYS[1]) else return -1 end\" 1 tx:limited`. Run it six times in a row — the first " +
					"five should return 1 through 5, and the sixth should return -1 (the limit held). Click Check Work.",
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

"LATENCY HISTORY <event>" and "LATENCY LATEST" track latency by event class — "command" for slow individual commands (overlapping with slowlog, but from a different angle), "fork" for background-save fork time, "aof-fsync-always" for the durability cost the persistence lab covers, and others. Where slowlog tells you which specific command was slow, latency monitoring tells you which class of internal operation is contributing to overall latency — the two tools answer related but different diagnostic questions.`,
		DesignTemplate: labValkeyStandaloneDesign,
		Steps: []LabStep{
			{
				ID:    "catch-slow-command",
				Title: "Catch a KEYS * scan in the slowlog",
				Instructions: "Log everything for this exercise: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET " +
					"slowlog-log-slower-than 0`. Populate enough keys that a full scan is actually slow (a Lua loop is far faster than " +
					"one valkey-cli call per key): `valkey-cli -a valkey_password --no-auth-warning EVAL \"for i=1,50000 do " +
					"redis.call('SET', 'bulk:'..i, 'v') end\" 0`. Now run the anti-pattern: `valkey-cli -a valkey_password " +
					"--no-auth-warning KEYS '*' >/dev/null` (redirected, so it doesn't print 50000 lines). Confirm it's in the log: " +
					"`valkey-cli -a valkey_password --no-auth-warning SLOWLOG GET 5`. Click Check Work.",
				Hint: "Look for an entry whose command is `KEYS` `*` — with only a few thousand keys the scan can finish in under a " +
					"millisecond and never even reach the slowlog threshold, which is why this step uses 50000.",
			},
			{
				ID:    "latency-diagnostics",
				Title: "Confirm it shows up in latency monitoring too",
				Instructions: "Turn on latency tracking: `valkey-cli -a valkey_password --no-auth-warning CONFIG SET " +
					"latency-monitor-threshold 1`. Run the same `KEYS '*'` scan again. Check `valkey-cli -a valkey_password " +
					"--no-auth-warning LATENCY HISTORY command` — it should show at least one recorded event. Click Check Work.",
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
				Instructions: "Write some data worth backing up: `valkey-cli -a valkey_password --no-auth-warning SET backup:marker " +
					"hello`. Trigger a snapshot: `valkey-cli -a valkey_password --no-auth-warning BGSAVE`. Wait a couple seconds, then copy " +
					"it to a separate backup location (simulating off-node storage): `cp /data/dump.rdb /data/backup-dump.rdb`. Click Check " +
					"Work.",
				Hint: "Check Work confirms LASTSAVE advanced (proof BGSAVE actually completed, not just that you ran the command) and that the copy exists.",
			},
			{
				ID:    "verify-backup",
				Title: "Verify the backup file is actually restorable",
				Instructions: "Run `valkey-check-rdb /data/backup-dump.rdb`. It should report \"RDB looks OK!\" and how many keys it found. " +
					"Click Check Work.",
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

The earlier resharding lab moves slots between masters that are already part of the cluster. This lab changes cluster membership itself: removing a shard entirely, and later bringing a (or another) node in as a brand new member. A shard can only be removed once it owns zero slots — reshard everything off it first, exactly like the "Building Replica Topology" lab's first step.

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
				Instructions: "Run `valkey-cli -a valkey_password --no-auth-warning CLUSTER NODES` and note all four node IDs and slot " +
					"ranges. Pick one to remove. Reshard all of its slots onto another master: `valkey-cli --cluster reshard 127.0.0.1:6379 " +
					"-a valkey_password --no-auth-warning --cluster-from <its-id> --cluster-to <another-id> --cluster-slots <however many it " +
					"owns> --cluster-yes`. Once it owns zero slots, remove it entirely: `valkey-cli --cluster del-node 127.0.0.1:6379 " +
					"<its-id> -a valkey_password --no-auth-warning`. Confirm with `CLUSTER NODES` that only three members remain and all " +
					"16384 slots are still covered between them. Click Check Work.",
				Hint: "del-node only accepts a target that currently owns zero slots — if it refuses, double-check the reshard actually moved everything (check its slot range is empty in CLUSTER NODES first).",
			},
			{
				ID:    "grow-cluster",
				Title: "Bring it back and give it work",
				Instructions: "Re-add it right away: `valkey-cli --cluster add-node <removed-node-ip>:6379 <any-remaining-node-ip>:6379 " +
					"-a valkey_password --no-auth-warning`. Confirm on the readded node itself (not one of the other three — see the hint) " +
					"with `CLUSTER NODES` that it now sees all four members. It rejoined with zero slots — give it some: `valkey-cli " +
					"--cluster reshard 127.0.0.1:6379 -a valkey_password --no-auth-warning --cluster-from <a-busy-node-id> --cluster-to " +
					"<its-id> --cluster-slots 2000 --cluster-yes` (this works immediately, even before the rest of the cluster has fully " +
					"caught up — see the hint). Click Check Work.",
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
				Instructions: "Open a terminal on any Valkey node. Run `valkey-cli -c -a valkey_password --no-auth-warning MSET " +
					"plain:a 1 plain:b 2` — with two ordinary, unrelated key names, these almost certainly hash to different slots, so you " +
					"should see a `CROSSSLOT` error (even with `-c` — there's no single node to redirect to). Click Check Work.",
				Hint: "If it succeeds instead of erroring, you got unlucky and both keys landed on the same slot by chance — try a different pair of key names.",
			},
			{
				ID:    "fix-with-hash-tags",
				Title: "Fix it with a hash tag",
				Instructions: "Run `valkey-cli -c -a valkey_password --no-auth-warning MSET \"tagged:{demo}:a\" 1 \"tagged:{demo}:b\" 2` " +
					"— both keys share the same `{demo}` hash tag, so they hash to the same slot and the command succeeds. Confirm: " +
					"`valkey-cli -a valkey_password --no-auth-warning CLUSTER KEYSLOT \"tagged:{demo}:a\"` and `CLUSTER KEYSLOT " +
					"\"tagged:{demo}:b\"` should print the same slot number. Click Check Work.",
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

Unlike every clustered lab in this curriculum, this app never wires standalone Valkey nodes together automatically — the two nodes in this lab start out completely independent, each with its own empty dataset. "REPLICAOF <host> <port>" (run on the node that should become the replica) is the single command that establishes the relationship.

What actually happens on REPLICAOF

The replica connects to the named primary, requests a full sync, receives a snapshot of the primary's entire current dataset, loads it, and then stays connected, applying every subsequent write the primary makes in real time. "INFO replication" on either side shows the live state — role, connection status, and (on the primary) how many replicas are currently connected.

Replicas are read-only by default, and that's enforced, not just documented

Once a node is a replica, direct writes to it fail with a READONLY error (unless you explicitly turn that off, which defeats the point) — the primary is the sole source of truth for anything the replica serves. This matters because it's the thing that makes a replica safe to point read traffic at without worrying about it silently diverging from the primary's data.

The foundation, not the whole HA story

Plain replication alone doesn't include any automatic failover — if the primary disappears, the replica just keeps being a read-only replica of a primary that's gone, forever, until something (a human, or Sentinel, covered in the next lab) tells it to stop following and become a primary itself.`,
		DesignTemplate: labValkeyReplicationDesign,
		Steps: []LabStep{
			{
				ID:    "establish-replication",
				Title: "Make valkey-b a replica of valkey-a",
				Instructions: "On valkey-a, write some data: `valkey-cli -a valkey_password --no-auth-warning SET repl:marker hello`. On " +
					"valkey-b, run `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379`. Wait a few seconds for the " +
					"initial sync, then confirm with `valkey-cli -a valkey_password --no-auth-warning INFO replication` on valkey-b that " +
					"`role:slave` and `master_link_status:up`. Confirm the data arrived: `valkey-cli -a valkey_password --no-auth-warning " +
					"GET repl:marker` on valkey-b should return `hello`. Click Check Work.",
				Hint: "master_link_status can briefly show `down` right after REPLICAOF while the initial full sync is still in progress — give it a few more seconds and check again.",
			},
			{
				ID:    "verify-read-only-replica",
				Title: "Confirm the replica rejects direct writes",
				Instructions: "On valkey-b, try to write directly: `valkey-cli -a valkey_password --no-auth-warning SET repl:direct " +
					"nope`. You should get a `READONLY` error — the replica refuses writes that don't come from its primary's replication " +
					"stream. Click Check Work.",
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

Once a primary is objectively down, Sentinel picks a replica (preferring the most caught-up one), sends it a command to stop following and become an independent primary, and reconfigures every other known replica to follow the newly promoted node instead. All of this happens automatically, without you running a single manual command — contrast this with the Valkey Cluster curriculum's manual CLUSTER FAILOVER, which only ever acts on your explicit request.

TILT mode: Sentinel's own safety brake

Sentinel constantly compares wall-clock time against its own internal event loop timing, and if it ever sees a gap far larger than expected — the kind of jump caused by a heavily loaded host, a paused process, or (as in this lab) a primary vanishing abruptly enough to disrupt Sentinel's own timing assumptions — it enters "TILT" mode: a roughly 30-second window where Sentinel deliberately stops making any failover decisions at all, on the theory that its own recent observations might not be trustworthy. This is a deliberate conservatism, not a bug: an automated system that fails over confidently on bad information is more dangerous than one that briefly refuses to act. It's also why this lab's failover takes on the order of 30-40 seconds rather than the sub-second reaction time down-after-milliseconds alone would suggest.

A client's job: ask Sentinel who's primary right now, not hardcode it

Because the primary's identity can change after any failover, real client code doesn't hardcode "connect to valkey-a" — it asks a Sentinel ("SENTINEL GET-MASTER-ADDR-BY-NAME mymaster") for the current primary's address first, every time it needs to (re)connect. This lab has you observe the failover directly rather than build that client logic, but it's the reason Sentinel exists as a separate discovery service instead of just being "replication with a script that restarts things."`,
		DesignTemplate: labValkeyReplicationDesign,
		Steps: []LabStep{
			{
				ID:    "setup-replication-and-sentinel",
				Title: "Wire up replication, then start Sentinel to watch it",
				Instructions: "On valkey-b, run `valkey-cli -a valkey_password --no-auth-warning REPLICAOF valkey-a 6379` and wait a few " +
					"seconds for it to sync. On valkey-b, write a Sentinel config: `cat > /tmp/sentinel.conf <<EOF\nport 26379\nsentinel " +
					"resolve-hostnames yes\nsentinel monitor mymaster valkey-a 6379 1\nsentinel down-after-milliseconds mymaster 500\n" +
					"sentinel failover-timeout mymaster 10000\nsentinel auth-pass mymaster valkey_password\nEOF`. Start it in the " +
					"background: `setsid valkey-sentinel /tmp/sentinel.conf > /tmp/sentinel.log 2>&1 < /dev/null &`. Confirm it's watching: " +
					"`valkey-cli -p 26379 SENTINEL MASTERS` should show `mymaster` with `flags` of `master` (healthy). Click Check Work.",
				Hint: "If SENTINEL MASTERS refuses to connect, the sentinel process didn't start — check `cat /tmp/sentinel.log` for why (a common cause is a typo in the config heredoc).",
			},
			{
				ID:    "crash-and-failover",
				Title: "Crash the primary and watch Sentinel promote the replica",
				Instructions: "On valkey-a, run `valkey-cli -a valkey_password --no-auth-warning SHUTDOWN NOSAVE`. This trips Sentinel's " +
					"own safety brake (\"TILT\" mode — see the hint), so the full failover takes about 30-40 seconds, not the 500ms " +
					"down-after-milliseconds alone would suggest. Wait, then on valkey-b run `valkey-cli -p 26379 SENTINEL MASTERS` again " +
					"— the `ip` field should now show valkey-b's own address, not valkey-a's, meaning Sentinel already promoted it. Confirm " +
					"directly: `valkey-cli -a valkey_password --no-auth-warning INFO replication` on valkey-b should now show `role:master`. " +
					"Click Check Work.",
				Hint: "This container restarts automatically within a few seconds of SHUTDOWN — an abrupt enough event that Sentinel " +
					"itself detects a suspicious time jump and enters TILT mode, deliberately pausing all failover decisions for about 30 " +
					"seconds as a safety measure against acting on bad information. That's why this step takes noticeably longer than " +
					"down-after-milliseconds (500ms) alone implies — the delay is Sentinel being cautious, not failing.",
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
		[]string{"sh", "-c", "test -f /data/backup-dump.rdb && echo yes || echo no"}, nil)
	if err != nil || strings.TrimSpace(res.Stdout) != "yes" {
		return LabStepResult{Passed: false, Message: "backup-dump.rdb doesn't exist yet — copy /data/dump.rdb to /data/backup-dump.rdb, then check again."}
	}
	return LabStepResult{Passed: true, Message: "Confirmed: BGSAVE completed and the backup file was copied to /data/backup-dump.rdb."}
}

// checkValkeyBackupVerified passes once valkey-check-rdb confirms the backup
// file is structurally sound — the step that turns "a file that's probably
// a backup" into a verified, restorable one.
func (a *App) checkValkeyBackupVerified(ctx context.Context, st Stack) LabStepResult {
	dep, ok := a.singleValkeyNode(st)
	if !ok {
		return LabStepResult{Passed: false, Message: "No Valkey node is running yet — wait for it to finish deploying."}
	}
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"valkey-check-rdb", "/data/backup-dump.rdb"}, nil)
	if err != nil {
		return LabStepResult{Passed: false, Message: "Could not run valkey-check-rdb — make sure /data/backup-dump.rdb exists first."}
	}
	if res.Code != 0 || !strings.Contains(res.Stdout, "RDB looks OK") {
		return LabStepResult{Passed: false, Message: "valkey-check-rdb didn't report a clean file — re-copy /data/dump.rdb to /data/backup-dump.rdb and try again."}
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
