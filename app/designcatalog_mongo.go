package main

// designcatalog_mongo.go — the catalogue's entries for MongoDB (standalone, replica set, sharded) and MClusterAdmin. See designcatalog.go.

// mongoSizingOptions are the per-node container sizing and network-shaping fields every
// MongoDB node (standalone or member) offers.
var mongoSizingOptions = []catalogOption{
	{Field: "cpus", Desc: "CPUs: the container's --cpus limit on Docker (0 = unlimited) or the VirtualBox VM's CPUs on Vagrant."},
	{Field: "memoryGb", Desc: "Memory in GiB: the container's --memory limit on Docker (0 = unlimited) or the VirtualBox VM's memory on Vagrant."},
	{Field: "deviceReadMbps", Desc: "Disk read limit in MB/s (container --device-read-bps); 0 = unlimited. Docker only."},
	{Field: "deviceWriteMbps", Desc: "Disk write limit in MB/s (container --device-write-bps); 0 = unlimited. Docker only."},
	{Field: "devicePath", Desc: "Host block device the disk limits apply to; \"\" auto-detects the disk behind Docker's data root.", When: "deviceReadMbps or deviceWriteMbps is set"},
	{Field: "netLatencyMs", Desc: "Added network latency in ms, applied with tc after the cluster forms. Docker only; not locked after deploy."},
	{Field: "netJitterMs", Desc: "Latency jitter (± ms). Docker only."},
	{Field: "netLossPct", Desc: "Packet loss in percent. Docker only."},
	{Field: "netRateMbit", Desc: "Bandwidth cap in Mbit/s. Docker only."},
}

// mongoVersionOptions are the catalogue-driven OS and server version fields shared by the
// standalone node and both cluster frames.
var mongoVersionOptions = []catalogOption{
	{Field: "os", Default: "oraclelinux", Desc: "OS family of the image; one of those GET /api/catalog/psmdb lists with Percona Server for MongoDB packages."},
	{Field: "osVersion", Default: "9", Desc: "OS release of the image (e.g. \"9\", \"24.04\"), as GET /api/catalog/psmdb lists it for the chosen os."},
	{Field: "psmdbMajor", Default: "8.0", Desc: "Percona Server for MongoDB series (e.g. \"7.0\", \"8.0\", \"8.3\"), from GET /api/catalog/psmdb for the chosen image."},
	{Field: "psmdbVersion", Desc: "Exact Percona Server for MongoDB version (e.g. \"8.0.26-11\"); \"\" is the newest of the series."},
}

// mongoCertOptions are the Intranet-CA TLS fields.
var mongoCertOptions = []catalogOption{
	{Field: "generateCert", Desc: "Issue certificates from the Intranet CA and turn TLS on in mongod (and mongos): net.tls mode preferTLS, allowConnectionsWithoutCertificates."},
	{Field: "certTtlValue", Default: 365, Desc: "Certificate lifetime, in certTtlUnit.", When: "generateCert is on"},
	{Field: "certTtlUnit", Default: "days", Values: []string{"minutes", "hours", "days"}, Desc: "Unit of certTtlValue.", When: "generateCert is on"},
}

// mongoCommonFrameOptions are the cluster-wide fields of both MongoDB frames.
var mongoCommonFrameOptions = []catalogOption{
	{Field: "enablePBM", Desc: "Percona Backup for MongoDB: pbm-agent on every mongod member, with the S3 store on the SeaweedFS node of the backup relation (seaweedfsNodeId). Needs that relation."},
	{Field: "seaweedfsBucket", Desc: "Which bucket of the SeaweedFS node backups go to; \"\" is its first (default) bucket. Backups land under pbm/<cluster name>.", When: "enablePBM is on"},
	{Field: "vectorSearch", Desc: "Run Percona Search for MongoDB (mongot) so $search and $vectorSearch work: one mongot per data-bearing replica set, on its first member. Needs psmdbMajor 8.3 or later and Oracle Linux 8/9 or Ubuntu 22.04/24.04."},
	{Field: "mcaCredentials", Desc: "Create the MClusterAdmin accounts madmin and madmin-ro (SCRAM-SHA-256) at deploy; their passwords come from the stack's MClusterAdmin node."},
	{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
}

// mongoClusterMemberActions apply to a deployed mongod member of a replica set or a
// sharded cluster's shard or config replica set.
var mongoClusterMemberActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/switchover", Desc: "Whether this member's replica set can switch its primary, who the primary is and which members could take over.", When: "not a mongos router"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/promote", Desc: "Make this member the primary of its replica set; returns the steps taken.", When: "not a mongos router"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/config-drift", Desc: "Every member's getParameter settings, and the ones that differ."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Whether the replica set can be restarted one member at a time, and the running or last rolling restart."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Restart the replica set one member at a time in the background, secondaries first.", Body: "switchover (hand the primary to a restarted secondary and restart the old primary last)"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "The replica set's database accounts, with the members that have each one and any whose credential differs from the primary's."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "Create, rotate the password of, or drop an account on the primary, and confirm it reached every member.", Body: "action, name, password, privilege (readonly|readwrite|admin), database"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "The failure drills the replica set offers, and the stack's running drill."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "Start a failure drill: the primary's container is stopped and the set should elect a new one.", Body: "kind (\"kill-primary\")"},
}

// mongoNodeActions apply to any deployed MongoDB node: standalone, member or router.
var mongoNodeActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/activity", Desc: "Running operations (currentOp) on this node."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/kill", Desc: "Kill a running operation (killOp).", Body: "session, mode"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/mongo/diagnostic", Desc: "Download the node's diagnostic.data (FTDC) with its mongod.log (mongos.log on a router) as a .tar.gz."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ftdc", Desc: "Read and summarise the running node's own diagnostic.data (FTDC Summary)."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/mongo/cert", Desc: "The node's server and cluster certificates.", When: "generateCert is on"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/mongo/cert", Desc: "Reissue the node's certificate from the Intranet CA, in place, without restarting mongod.", Body: "value, unit (minutes|hours|days)", When: "generateCert is on"},
}

// mongoDataGenActions are the Data Generator's endpoints for a node that takes writes.
var mongoDataGenActions = []catalogAction{
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/databases", Desc: "The node's databases, for the Data Generator."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/tables", Desc: "A database's collections with their document counts."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/columns", Desc: "A collection's fields and the generators inferred for them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/preview", Desc: "Generate sample documents without writing them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/generate", Desc: "Start a generation job; returns its id.", Body: "database, table, rows, batch, threads, seed, stopOnError, columns"},
}

// mongoSimEdges are the simulator association lines a MongoDB node or frame can draw.
var mongoSimEdges = []catalogEdge{
	{To: []string{"supportsim", "stocksim", "perfsim"}, Type: "directional",
		Desc: "Drive a simulator's workload against this MongoDB (from the database to the simulator)."},
}

// mongoShardedSimEdges are a sharded cluster's: Performance Sim's lag stressors
// act on one replica set's members, not yet on a sharded cluster's shards.
var mongoShardedSimEdges = []catalogEdge{
	{To: []string{"supportsim", "stocksim"}, Type: "directional",
		Desc: "Drive a simulator's workload against this MongoDB (from the database to the simulator)."},
}

func mongoOpts(groups ...[]catalogOption) []catalogOption {
	var out []catalogOption
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func mongoActs(groups ...[]catalogAction) []catalogAction {
	var out []catalogAction
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

var catalogMongoFamily = []catalogEntry{
	{Type: "psm", Label: "PSMDB", Summary: "Percona Server for MongoDB, standalone (no replication).", Placement: "standalone",
		Requires: []string{"an Intranet node"},
		Options: mongoOpts(mongoVersionOptions, mongoSizingOptions, []catalogOption{
			{Field: "mcaCredentials", Desc: "Create the MClusterAdmin accounts madmin and madmin-ro (SCRAM-SHA-256) at deploy; their passwords come from the stack's MClusterAdmin node."},
			{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
		}, mongoCertOptions, []catalogOption{
			{Field: "exportEnabled", Desc: "Publish mongod's port 27017 on the host."},
			{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
			{Field: "enableOIDC", Desc: "MONGODB-OIDC authentication against the Keycloak node of the oidc relation. Cannot be combined with ldapAuth or kerberosAuth."},
			{Field: "oidcRealm", Default: "mongodb", Desc: "Keycloak realm holding the OIDC client.", When: "enableOIDC is on"},
			{Field: "oidcClientId", Default: "mongodb-client", Desc: "OIDC client id, also the token audience.", When: "enableOIDC is on"},
			{Field: "oidcUseAuthClaim", Default: true, Desc: "Authorize by a group claim (creates the keycloak/developers and keycloak/dbadmins roles); off authorizes by user name in $external.", When: "enableOIDC is on"},
			{Field: "oidcAuthClaim", Default: "MyClaim", Desc: "Token claim carrying the user's groups.", When: "enableOIDC and oidcUseAuthClaim are on"},
			{Field: "ldapAuth", Desc: "LDAP authentication against the directory of the ldap relation (Intranet OpenLDAP or Samba AD DC). Cannot be combined with enableOIDC."},
			{Field: "kerberosAuth", Desc: "GSSAPI (Kerberos) sign-in against a Samba AD DC (kerberos relation). Cannot be combined with enableOIDC."},
			{Field: "enableVault", Desc: "Data-at-rest encryption with mongod's security.vault, keyed in the OpenBao node of the vault relation. Set at first start; cannot be turned on later without re-creating the data."},
		}),
		Relations: []string{"monitor", "repository", "oidc", "ldap", "kerberos", "vault"},
		Edges:     mongoSimEdges,
		Actions:   mongoActs(mongoNodeActions, mongoDataGenActions),
	},
	{Type: "psmrs", Label: "PSMDB RS", Summary: "A member of a Percona Server for MongoDB replica set; the set elects its own primary.", Placement: "member",
		FrameTypes: []string{"psmrs"},
		Options: mongoOpts(mongoSizingOptions, []catalogOption{
			{Field: "exportEnabled", Desc: "Publish this member's port 27017 on the host."},
			{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
		}),
		Actions: mongoActs(mongoClusterMemberActions, mongoNodeActions, mongoDataGenActions),
	},
	{Type: "psmdb", Label: "PS MongoDB", Summary: "A member of a Percona Server for MongoDB sharded cluster: a shard member, a config server or the mongos router.", Placement: "member",
		FrameTypes: []string{"psmdb"},
		Options: mongoOpts([]catalogOption{
			{Field: "role", Values: []string{"shard", "config", "mongos"}, Desc: "The member's part in the cluster. The frame's psmdbSetup fixes the layout: standard = 1 mongos, 3 config servers, 3 shards of 3 (13 members); minimum = 1 mongos, 1 config server, 3 shards of 1 (5 members). Labels are mongos, cfgN and sNrM."},
			{Field: "shard", Desc: "Shard index (0, 1 or 2) of a shard member.", When: "role is \"shard\""},
		}, mongoSizingOptions, []catalogOption{
			{Field: "exportEnabled", Desc: "Publish the mongos router's port 27017 on the host; clients connect through mongos.", When: "role is \"mongos\""},
			{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "role is \"mongos\" and exportEnabled is on"},
		}),
		Actions: mongoActs(mongoClusterMemberActions, mongoNodeActions, []catalogAction{
			{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/databases", Desc: "The cluster's databases, for the Data Generator.", When: "role is \"mongos\""},
			{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/tables", Desc: "A database's collections with their document counts.", When: "role is \"mongos\""},
			{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/columns", Desc: "A collection's fields and the generators inferred for them.", When: "role is \"mongos\""},
			{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/preview", Desc: "Generate sample documents without writing them.", When: "role is \"mongos\""},
			{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/generate", Desc: "Start a generation job through mongos; returns its id.", Body: "database, table, rows, batch, threads, seed, stopOnError, columns", When: "role is \"mongos\""},
		}),
	},
	{Type: "psmrs", Label: "PSMDB Replica Set", Summary: "A Percona Server for MongoDB replica set; the frame's label is the replica-set name.", Placement: "frame",
		MemberType: "psmrs", DefaultMembers: 3, MinMembers: 1, MaxMembers: 9,
		Requires:  []string{"an Intranet node", "a SeaweedFS node when enablePBM is on"},
		Options:   mongoOpts(mongoVersionOptions, mongoCommonFrameOptions, mongoCertOptions),
		Relations: []string{"monitor", "repository", "backup"},
		Edges:     mongoShardedSimEdges,
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/pbm/backup", Desc: "Run an on-demand PBM backup.", When: "enablePBM is on"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/backup-status", Desc: "The newest PBM backup, how many there are, and whether one is running.", When: "enablePBM is on"},
		},
	},
	{Type: "psmdb", Label: "PSMDB Sharded", Summary: "A Percona Server for MongoDB sharded cluster of fixed topology: one mongos router, a config-server replica set and three shard replica sets.", Placement: "frame",
		MemberType: "psmdb", DefaultMembers: 13, MinMembers: 5, MaxMembers: 13,
		Requires: []string{"an Intranet node", "a SeaweedFS node when enablePBM is on"},
		Options: mongoOpts([]catalogOption{
			{Field: "psmdbSetup", Default: "standard", Values: []string{"standard", "minimum"}, Desc: "Topology: standard = 3 shards × 3-member replica sets + 3 config servers + 1 mongos (13 members); minimum = 3 single-member shards + 1 config server + 1 mongos (5 members). The members must match it; members cannot be added or removed otherwise."},
		}, mongoVersionOptions, mongoCommonFrameOptions, mongoCertOptions),
		Relations: []string{"monitor", "repository", "backup"},
		Edges:     mongoSimEdges,
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/pbm/backup", Desc: "Run an on-demand PBM backup of the whole cluster.", When: "enablePBM is on"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/backup-status", Desc: "The newest PBM backup, how many there are, and whether one is running.", When: "enablePBM is on"},
		},
	},
	{Type: "mclusteradmin", Label: "MClusterAdmin", Summary: "MClusterAdmin, a MongoDB administration panel with a web UI published on the host. It connects to a URI entered in its own UI, so it draws no association line.", Placement: "standalone",
		Requires: []string{"an Intranet node"},
		Options: []catalogOption{
			{Field: "mcaAdminPassword", Desc: "Password of madmin (full administration) on every MongoDB in the stack with mcaCredentials on; \"\" uses the installation default."},
			{Field: "mcaReadonlyPassword", Desc: "Password of madmin-ro (read-only); \"\" uses the installation default."},
			{Field: "viewOnly", Desc: "Start the panel with --view-only, every write disabled."},
		},
	},
}
