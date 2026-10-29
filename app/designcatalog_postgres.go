package main

// designcatalog_postgres.go — the catalogue's entries for PostgreSQL, Patroni, repmgr, Spock, PgBouncer and Valkey. See designcatalog.go.

// pgcatSizing are the per-node container sizing and network-shaping fields of every node
// type in this family (standalone or member).
var pgcatSizing = []catalogOption{
	{Field: "cpus", Desc: "CPUs: the container's --cpus limit on Docker (0 = unlimited) or the VirtualBox VM's CPUs on Vagrant."},
	{Field: "memoryGb", Desc: "Memory in GiB: the container's --memory limit on Docker (0 = unlimited) or the VirtualBox VM's memory on Vagrant."},
	{Field: "deviceReadMbps", Desc: "Disk read limit in MB/s (container --device-read-bps); 0 = unlimited. Docker only."},
	{Field: "deviceWriteMbps", Desc: "Disk write limit in MB/s (container --device-write-bps); 0 = unlimited. Docker only."},
	{Field: "devicePath", Desc: "Host block device the disk limits apply to; \"\" auto-detects the disk behind Docker's data root.", When: "deviceReadMbps or deviceWriteMbps is set"},
	{Field: "netLatencyMs", Desc: "Added one-way latency in ms (0–1000) on the node's database and cluster ports, applied with tc after the cluster forms. Docker only; not locked after deploy."},
	{Field: "netJitterMs", Desc: "Latency jitter (± ms), capped at netLatencyMs. Docker only."},
	{Field: "netLossPct", Desc: "Outbound packet loss in percent (0–100). Docker only."},
	{Field: "netRateMbit", Desc: "Outbound bandwidth cap in Mbit/s; 0 = unlimited. Docker only."},
	{Field: "netAllTraffic", Desc: "Shape every packet the node sends, not only its database and cluster ports. Docker only."},
}

// pgcatExport publishes the PostgreSQL port of a node or member.
var pgcatExport = []catalogOption{
	{Field: "exportEnabled", Desc: "Publish PostgreSQL's port 5432 on the host."},
	{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
}

// pgcatPPGVersion are the image and server version fields of the PostgreSQL types that
// install from the Percona PostgreSQL catalogue.
func pgcatPPGVersion(what string) []catalogOption {
	return []catalogOption{
		{Field: "os", Default: "oraclelinux", Desc: "OS family of the image; one of those GET /api/catalog/ppg lists with " + what + " packages."},
		{Field: "osVersion", Default: "9", Desc: "OS release of the image (e.g. \"9\", \"24.04\"), as GET /api/catalog/ppg lists it for the chosen os."},
		{Field: "pgMajor", Default: "16", Desc: "PostgreSQL major (\"13\" to \"18\"), from GET /api/catalog/ppg for the chosen image. Changing it clears pgVersion."},
		{Field: "pgVersion", Desc: "Exact PostgreSQL minor (e.g. \"16.4\"); \"\" is the newest of the major."},
	}
}

// pgcatCert are the Intranet-CA TLS fields of a PostgreSQL node or cluster.
var pgcatCert = []catalogOption{
	{Field: "generateCert", Desc: "Issue PostgreSQL's server certificate from the Intranet CA and turn on TLS."},
	{Field: "certTtlValue", Default: 365, Desc: "Certificate lifetime, in certTtlUnit.", When: "generateCert is on"},
	{Field: "certTtlUnit", Default: "days", Values: []string{"minutes", "hours", "days"}, Desc: "Unit of certTtlValue.", When: "generateCert is on"},
}

// pgcatHostAuth is the pg_hba method list, shared by the node and the three cluster types.
var pgcatHostAuth = catalogOption{Field: "pgHostAuth",
	Desc: "Remote client authentication, as an ordered comma-separated pg_hba method list from scram-sha-256, md5, trust, cert and reject; \"\" is scram-sha-256. The designer offers one password method, optionally preceded by \"cert\" (a hostssl rule that authenticates a TLS client by its certificate, CN = role name), e.g. \"cert,scram-sha-256\", or \"cert\" alone. cert needs generateCert."}

// pgcatCommon are the extension and download options a PostgreSQL node and every
// PostgreSQL cluster frame offer.
var pgcatCommon = []catalogOption{
	{Field: "pgVector", Desc: "Install pgvector and create the vector extension in the postgres and template1 databases."},
	{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
}

// pgcatPercona are the Percona-package options of the standalone node and the Patroni frame.
var pgcatPercona = []catalogOption{
	{Field: "pgQuerySource", Values: []string{"", "pgstatements", "pgstatmonitor"},
		Desc: "Statement statistics extension PMM Query Analytics reads: \"\" none, \"pgstatements\" (pg_stat_statements preloaded) or \"pgstatmonitor\" (Percona's pg_stat_monitor installed and preloaded); passed to pmm-admin as --query-source.", When: "the monitor relation is set"},
	{Field: "pgAudit", Desc: "Install and preload pgAudit. Only where the Percona PostgreSQL component table offers it for the major and OS."},
	{Field: "pgAuditLog", Desc: "pgaudit.log statement classes, e.g. \"ddl,role\", \"write,ddl\", \"all,-misc\"; \"\" is \"ddl,role\".", When: "pgAudit is on"},
	{Field: "pgSetUser", Desc: "Install and preload set_user (audited privilege escalation). Only where the Percona PostgreSQL component table offers it."},
	{Field: "pgbenchScale", Desc: "Load `pgbench -i -s N` into a database called pgbench at deploy; 0 = none, at most 1000 (about 16 MB per unit)."},
	{Field: "enableVault", Desc: "Encrypt data at rest with pg_tde, keyed to the stack's OpenBao node (the vault relation); tde_heap becomes the default table access method. Only where the component table offers pg_tde for the version and OS."},
	{Field: "pgTdeWal", Desc: "Also encrypt the WAL (pg_tde.wal_encrypt). With pgBackRest, archiving and restores go through pg_tde_archive_decrypt / pg_tde_restore_encrypt.", When: "enableVault is on"},
}

// pgcatQuerySourcePGDG is pgQuerySource on repmgr and Spock, which run PGDG packages: only
// pg_stat_statements, and not tied to PMM.
var pgcatQuerySourcePGDG = catalogOption{Field: "pgQuerySource", Values: []string{"", "pgstatements"},
	Desc: "\"pgstatements\" preloads pg_stat_statements on every member (read by PMM Query Analytics when the monitor relation is set); \"\" none. pg_stat_monitor is not available: these clusters do not run Percona's packages."}

// pgcatBackRest are the pgBackRest backup fields of the standalone node and the Patroni frame.
var pgcatBackRest = []catalogOption{
	{Field: "usePgBackRest", Desc: "WAL archiving and an initial full backup with pgBackRest to the SeaweedFS node of the backup relation, which must have S3 TLS on (pgBackRest's S3 client needs HTTPS)."},
	{Field: "seaweedfsBucket", Desc: "Which bucket of that SeaweedFS node to use; \"\" is its first (default) bucket.", When: "usePgBackRest is on"},
}

// pgcatSimEdges are the simulator lines a PostgreSQL node, cluster or pool can draw.
var pgcatSimEdges = []catalogEdge{
	{To: []string{"stocksim", "ledgersim", "perfsim"}, Type: "directional",
		Desc: "Drive a simulator's workload against this PostgreSQL (from the database to the simulator)."},
}

// pgcatSpockSimEdges are a Spock cluster's: Performance Sim's lag stressors are
// for physical streaming replication, which a Spock cluster does not use.
var pgcatSpockSimEdges = []catalogEdge{
	{To: []string{"stocksim", "ledgersim"}, Type: "directional",
		Desc: "Drive a simulator's workload against this PostgreSQL (from the database to the simulator)."},
}

// pgcatClusterProxyEdges are the proxy lines a PostgreSQL cluster frame can draw.
var pgcatClusterProxyEdges = []catalogEdge{
	{To: []string{"haproxy"}, Type: "directional", Desc: "Front the cluster with an HAProxy node (from the cluster to the HAProxy)."},
	{To: []string{"pgbouncer"}, Type: "directional", Desc: "Pool connections to the cluster with a PgBouncer node (from the cluster to the pool)."},
}

// pgcatNodeActions apply to any deployed PostgreSQL server: standalone or a member of a
// Patroni, repmgr or Spock cluster.
var pgcatNodeActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pg/cert", Desc: "The certificate PostgreSQL serves.", When: "generateCert is on"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/cert", Desc: "Reissue the node's server certificate from the Intranet CA, in place, without restarting PostgreSQL.", Body: "value, unit (minutes|hours|days)", When: "generateCert is on"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pggather", Desc: "State of the node's pg_gather capture."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pggather", Desc: "Run pg_gather against a database in the background.", Body: "database"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pggather/download", Desc: "Download the last pg_gather report (GatherReport.html)."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/activity", Desc: "Sessions, locks and blocking on the node (pg_stat_activity); ?with= adds digests, resources or deadlock."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/kill", Desc: "Cancel a backend's query, or terminate its connection.", Body: "session (pid), mode (\"connection\" terminates; anything else cancels the query)"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/explain", Desc: "EXPLAIN a session's running statement.", Body: "session, statement, db"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/deadlocks", Desc: "The deadlocks the live watch has recorded on the node."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/databases", Desc: "The node's databases, for the Data Generator.", When: "the node takes writes (the primary)"},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/tables", Desc: "A database's tables with their estimated row counts."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/columns", Desc: "A table's columns and the generators inferred for them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/preview", Desc: "Generate sample rows without writing them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/generate", Desc: "Start a generation job; returns its id.", Body: "database, schema, table, rows, batch, threads, seed, stopOnError, fkSampleSize, columns"},
}

// pgcatReplicatedMemberActions apply to a deployed member of a Patroni or repmgr cluster:
// clusters with one primary and streaming standbys.
var pgcatReplicatedMemberActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/switchover", Desc: "Whether the cluster can switch its primary, which member is primary, and each member's state."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/promote", Desc: "Make this member the primary (a switchover); returns the steps taken. The design is updated to match."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/config-drift", Desc: "Every member's pg_settings, and the ones that differ."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Whether the cluster can be restarted one member at a time, and the running or last rolling restart."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Restart the cluster one member at a time in the background, standbys first.", Body: "switchover (hand the primary to a restarted standby and restart the old primary last)"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "The cluster's roles, with the members that have each one and any whose credential differs from the primary's."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "Create, change the password of, or drop a role on the primary, and confirm it reached every member.", Body: "action (create|password|drop), name, password, privilege (readonly|readwrite|admin), database (\"\" = every database)"},
}

// pgcatFrameBackupStatus is the backup summary of a Patroni or repmgr frame.
var pgcatFrameBackupStatus = catalogAction{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/backup-status",
	Desc: "The cluster's backup engine, how many backups its repository holds, and the newest one (name, type, size, when it finished)."}

// pgcatValkeyVersion are the image and server version fields of a Valkey node or cluster.
var pgcatValkeyVersion = []catalogOption{
	{Field: "os", Default: "oraclelinux", Desc: "OS family of the image; one of those GET /api/catalog/valkey lists with Valkey packages."},
	{Field: "osVersion", Default: "9", Desc: "OS release of the image, as GET /api/catalog/valkey lists it for the chosen os."},
	{Field: "valkeyMajor", Default: "9.1", Desc: "Valkey series, from GET /api/catalog/valkey for the chosen image (installed from percona-release's valkey repository). Changing it clears valkeyVersion."},
	{Field: "valkeyVersion", Desc: "Exact Valkey version; \"\" is the newest of the series."},
}

func pgcatOpts(groups ...[]catalogOption) []catalogOption {
	var out []catalogOption
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func pgcatActs(groups ...[]catalogAction) []catalogAction {
	var out []catalogAction
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func pgcatEdges(groups ...[]catalogEdge) []catalogEdge {
	var out []catalogEdge
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

var catalogPostgresFamily = []catalogEntry{
	// ---------------------------------------------------------- standalone PostgreSQL
	{Type: "pg", Label: "PostgreSQL", Summary: "A single Percona Distribution for PostgreSQL server, with no replication.", Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"usePgBackRest: a SeaweedFS node with S3 TLS on, set as the backup relation",
			"ldapAuth and enableOIDC cannot both be on (both need pg_hba's host catch-all)",
			"kerberosAuth: a Samba AD DC node",
			"enableOIDC: a Keycloak node serving HTTPS and a PostgreSQL version that has pg_oidc_validator (18)",
			"enableVault: the OpenBao node, and a version and OS for which pg_tde is available",
		},
		Options: pgcatOpts(
			pgcatSizing,
			pgcatPPGVersion("Percona PostgreSQL"),
			pgcatBackRest,
			pgcatPercona,
			pgcatCommon,
			pgcatCert,
			[]catalogOption{pgcatHostAuth},
			pgcatExport,
			[]catalogOption{
				{Field: "ldapAuth", Desc: "Authenticate remote clients against an LDAP directory (the ldap relation: the Intranet's OpenLDAP or a Samba AD DC). Not with enableOIDC."},
				{Field: "kerberosAuth", Desc: "GSSAPI (Kerberos) single sign-on against the Samba AD DC: a service principal and keytab are minted at deploy. Independent of ldapAuth; set the kerberos relation."},
				{Field: "enableOIDC", Desc: "OAuth login with Keycloak through pg_oidc_validator (the oidc relation). The designer moves pgMajor to \"18\" and clears pgVersion when this is turned on. Not with ldapAuth."},
				{Field: "oidcRealm", Desc: "Keycloak realm holding the OIDC client; \"\" is \"dbcanvas\".", When: "enableOIDC is on"},
			},
		),
		Relations: []string{"monitor", "repository", "backup", "vault", "ldap", "kerberos", "oidc"},
		Edges: pgcatEdges(
			[]catalogEdge{{To: []string{"pgbouncer"}, Type: "directional", Desc: "Pool connections to this server with a PgBouncer node (from the server to the pool)."}},
			pgcatSimEdges,
		),
		Actions: pgcatActs(pgcatNodeActions, []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/backup", Desc: "Take a pgBackRest full backup now.", When: "usePgBackRest is on"},
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pg/backups", Desc: "The pgBackRest backup sets and the earliest point a restore can reach.", When: "usePgBackRest is on"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/restore", Desc: "Stop PostgreSQL, restore it with pgBackRest and start it again.", Body: "mode (latest|time|backup), time (RFC 3339, for time), set (backup label, for backup)", When: "usePgBackRest is on"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/tde/rotate", Desc: "Rotate the pg_tde principal key.", When: "enableVault is on"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/pgbench", Desc: "Run pgbench against the node's pgbench database and return the parsed result.", Body: "clients, seconds, readOnly", When: "pgbenchScale > 0"},
		}),
	},

	// ---------------------------------------------------------- Patroni
	{Type: "patroni", Label: "Patroni", Summary: "A Patroni cluster member: Percona PostgreSQL, Patroni and an etcd member in one node.", Placement: "member",
		FrameTypes: []string{"patroni"},
		Options:    pgcatOpts(pgcatSizing, pgcatExport),
		Actions: pgcatActs(pgcatNodeActions, pgcatReplicatedMemberActions, []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "The failure drills the cluster offers, and the stack's running drill."},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "Start a failure drill: the primary's container is stopped and Patroni should elect a new leader.", Body: "kind (\"kill-primary\")"},
			{Method: "DELETE", Path: "/api/stacks/{id}/drill", Desc: "End the stack's running drill."},
		}),
	},
	{Type: "patroni", Label: "Patroni Cluster", Summary: "A Percona PostgreSQL cluster managed by Patroni with etcd: one leader, streaming replicas, automatic failover.", Placement: "frame",
		MemberType: "patroni", DefaultMembers: 3, MinMembers: 3, MaxMembers: 7,
		Requires: []string{
			"an Intranet node",
			"3 to 7 members (etcd quorum); an odd number is recommended",
			"a cluster name (the frame label) unique among Patroni clusters; it becomes the Patroni scope and pgBackRest stanza",
			"usePgBackRest: a SeaweedFS node with S3 TLS on, set as the backup relation",
			"enableVault: the OpenBao node, and a version and OS for which pg_tde is available",
		},
		Options: pgcatOpts(
			pgcatPPGVersion("Percona PostgreSQL"),
			pgcatBackRest,
			pgcatPercona,
			pgcatCommon,
			pgcatCert,
			[]catalogOption{
				pgcatHostAuth,
				{Field: "disablePgRewind", Desc: "Turn off Patroni's use_pg_rewind (on by default), so a former leader is not rewound automatically. Not shown in the designer."},
				{Field: "enableRoleChangeCallback", Desc: "Install an on_role_change callback script that appends each role change to its own log. Not shown in the designer."},
			},
		),
		Relations: []string{"monitor", "repository", "backup", "vault"},
		Edges:     pgcatEdges(pgcatClusterProxyEdges, pgcatSimEdges),
		Actions: []catalogAction{
			pgcatFrameBackupStatus,
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/patroni/backup", Desc: "Take a pgBackRest full backup on the current leader.", When: "usePgBackRest is on"},
			{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/patroni/backups", Desc: "The cluster's pgBackRest backup sets (read on the leader) and the earliest point a restore can reach.", When: "usePgBackRest is on"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/patroni/restore", Desc: "Restore the whole cluster from pgBackRest: the leader is restored and every replica is rebuilt from it.", Body: "mode (latest|time|backup), time (RFC 3339, for time), set (backup label, for backup)", When: "usePgBackRest is on"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/patroni/tde/rotate", Desc: "Rotate the cluster's pg_tde principal key, on the leader.", When: "enableVault is on"},
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/patroni/pgbench", Desc: "Run pgbench against the leader's pgbench database and return the parsed result.", Body: "clients, seconds, readOnly", When: "pgbenchScale > 0"},
		},
	},

	// ---------------------------------------------------------- repmgr
	{Type: "repmgr", Label: "repmgr", Summary: "A repmgr cluster member: PGDG PostgreSQL with repmgr and repmgrd.", Placement: "member",
		FrameTypes: []string{"repmgr"},
		Options:    pgcatOpts(pgcatSizing, pgcatExport),
		Actions:    pgcatActs(pgcatNodeActions, pgcatReplicatedMemberActions),
	},
	{Type: "repmgr", Label: "repmgr Cluster", Summary: "A PGDG PostgreSQL cluster with streaming replication managed by repmgr; the first member starts as primary and repmgrd handles failover.", Placement: "frame",
		MemberType: "repmgr", DefaultMembers: 3, MinMembers: 3, MaxMembers: 7,
		Requires: []string{
			"an Intranet node",
			"3 to 7 members; an odd number is recommended",
			"a cluster name (the frame label) unique among repmgr clusters",
			"one backup engine at most: usePgBackRest and useBarman cannot both be on",
			"usePgBackRest: a SeaweedFS node with S3 TLS on, set as the backup relation; useBarman: a SeaweedFS node (HTTP or HTTPS) as the backup relation",
		},
		Options: pgcatOpts(
			pgcatPPGVersion("PostgreSQL"),
			[]catalogOption{
				{Field: "useBarman", Desc: "WAL archiving and base backups with barman-cloud to the SeaweedFS node of the backup relation (works over HTTP). Not with usePgBackRest."},
				{Field: "usePgBackRest", Desc: "WAL archiving and base backups with pgBackRest to the SeaweedFS node of the backup relation, which must have S3 TLS on. Not with useBarman."},
				{Field: "seaweedfsBucket", Desc: "Which bucket of that SeaweedFS node to use; \"\" is its first (default) bucket.", When: "useBarman or usePgBackRest is on"},
				pgcatQuerySourcePGDG,
			},
			pgcatCommon,
			pgcatCert,
			[]catalogOption{pgcatHostAuth},
		),
		Relations: []string{"monitor", "repository", "backup"},
		Edges:     pgcatEdges(pgcatClusterProxyEdges, pgcatSimEdges),
		Actions: []catalogAction{
			pgcatFrameBackupStatus,
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/barman/backup", Desc: "Take a base backup now on the current primary, with whichever engine the cluster uses (barman-cloud or pgBackRest).", When: "useBarman or usePgBackRest is on"},
		},
	},

	// ---------------------------------------------------------- Spock
	{Type: "spock", Label: "Spock", Summary: "A Spock cluster member: PostgreSQL built from source with the pgEdge Spock extension, a writable node of the mesh.", Placement: "member",
		FrameTypes: []string{"spock"},
		Options:    pgcatOpts(pgcatSizing, pgcatExport),
		Actions: pgcatActs(pgcatNodeActions, []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/config-drift", Desc: "Every member's pg_settings, and the ones that differ."},
		}),
	},
	{Type: "spock", Label: "Spock Cluster", Summary: "An active-active PostgreSQL cluster: every member is writable and replicates to every other through pgEdge Spock logical replication (full mesh).", Placement: "frame",
		MemberType: "spock", DefaultMembers: 3, MinMembers: 2, MaxMembers: 7,
		Requires: []string{
			"an Intranet node",
			"2 to 7 members",
			"a cluster name (the frame label) unique among Spock clusters",
		},
		Options: pgcatOpts(
			[]catalogOption{
				{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux"}, Desc: "OS family of the image; Oracle Linux only."},
				{Field: "osVersion", Default: "9", Desc: "Oracle Linux release, as GET /api/catalog/spock lists it."},
				{Field: "pgMajor", Default: "16", Desc: "PostgreSQL major, \"15\" or later, from GET /api/catalog/spock for the chosen image. Changing it clears pgVersion."},
				{Field: "pgVersion", Desc: "Exact PostgreSQL minor to build; \"\" is the newest of the major."},
				pgcatQuerySourcePGDG,
			},
			pgcatCommon,
			pgcatCert,
			[]catalogOption{pgcatHostAuth},
		),
		Relations: []string{"monitor", "repository"},
		Edges:     pgcatEdges(pgcatClusterProxyEdges, pgcatSpockSimEdges),
	},

	// ---------------------------------------------------------- PgBouncer
	{Type: "pgbouncer", Label: "PgBouncer", Summary: "A PostgreSQL connection pooler (percona-pgbouncer) in front of one PostgreSQL node or Patroni, repmgr or Spock cluster, on port 6432.", Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"exactly one backend: an association line from a PostgreSQL node or a Patroni, repmgr or Spock cluster",
			"pgbAuthType \"cert\": generateCert on, and no simulator linked to the pool",
		},
		Options: pgcatOpts(
			pgcatSizing,
			[]catalogOption{
				{Field: "os", Default: "oraclelinux", Desc: "OS family of the image; one of those GET /api/catalog/images lists."},
				{Field: "osVersion", Default: "9", Desc: "OS release of the image, as GET /api/catalog/images lists it for the chosen os."},
				{Field: "pgMajor", Desc: "The ppg-NN repository percona-pgbouncer is installed from (e.g. \"17\"); \"\" follows the linked backend's PostgreSQL major."},
				{Field: "pgbPoolMode", Default: "transaction", Values: []string{"transaction", "session", "statement"}, Desc: "pool_mode: a server connection per transaction, per client session, or per statement."},
				{Field: "pgbMaxClientConn", Default: 500, Desc: "max_client_conn; 0 is 500."},
				{Field: "pgbDefaultPoolSize", Default: 20, Desc: "default_pool_size: server connections per user/database pair; 0 is 20."},
				{Field: "pgbMinPoolSize", Desc: "min_pool_size: server connections kept open while idle."},
				{Field: "pgbReservePoolSize", Desc: "reserve_pool_size: extra connections a waiting client may get."},
				{Field: "pgbMaxDbConnections", Desc: "max_db_connections: a cap across all pools of one database; 0 = none."},
				{Field: "pgbServerIdleTimeout", Desc: "server_idle_timeout in seconds; 0 keeps PgBouncer's own default."},
				{Field: "pgbIgnoreStartupParams", Default: "extra_float_digits,options,search_path", Desc: "ignore_startup_parameters, comma-separated; \"\" is extra_float_digits,options,search_path."},
				{Field: "pgbAuthType", Default: "scram-sha-256", Values: []string{"scram-sha-256", "md5", "cert", "trust"}, Desc: "auth_type for clients. \"cert\" takes a client certificate and no password, and needs generateCert."},
				{Field: "pgbAuthQuery", Default: true, Desc: "Look roles up in the backend's pg_shadow (auth_query) instead of only userlist.txt. Has no effect with pgbAuthType \"cert\"."},
				{Field: "pgbRouting", Values: []string{"", "rw", "rw-ro", "mesh"}, Desc: "Shape of the [databases] section: \"rw\" one pool at the writable member, \"rw-ro\" plus a read-only <database>_ro alias on a standby (Patroni, repmgr), \"mesh\" one <database>_<member> alias per member (Spock). \"\" follows the backend: rw for a PostgreSQL node, rw-ro for Patroni and repmgr, mesh for Spock."},
				{Field: "pgbDatabase", Desc: "Database the named pools point at; \"\" is postgres, or spockdemo on a Spock backend. A wildcard pool takes any database."},
				{Field: "pgbFollowPrimary", Default: true, Desc: "A timer re-points the pool at the member that can take writes (Patroni's REST API, repmgr's pg_is_in_recovery(), or the first Spock member that answers) and reloads PgBouncer when it changes. Ignored for a PostgreSQL node backend."},
				{Field: "pgbWatchInterval", Default: 5, Desc: "Seconds between the follow timer's probes; 0 is 5.", When: "pgbFollowPrimary is on and the backend is a cluster"},
				{Field: "pgbServerTLS", Default: "prefer", Values: []string{"prefer", "disable", "require", "verify-full"}, Desc: "server_tls_sslmode: TLS from the pool to the backend."},
				{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
				{Field: "generateCert", Desc: "Issue a certificate from the Intranet CA and terminate client TLS at the pool."},
				{Field: "certTtlValue", Default: 365, Desc: "Certificate lifetime, in certTtlUnit.", When: "generateCert is on"},
				{Field: "certTtlUnit", Default: "days", Values: []string{"minutes", "hours", "days"}, Desc: "Unit of certTtlValue.", When: "generateCert is on"},
				{Field: "exportEnabled", Desc: "Publish PgBouncer's port 6432 on the host."},
				{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
			},
		),
		Relations: []string{"repository"},
		Edges: pgcatEdges(
			[]catalogEdge{{To: []string{"pg", "patroni", "repmgr", "spock"}, Type: "directional", Required: true,
				Desc: "The one PostgreSQL backend the pool fronts: a PostgreSQL node or a Patroni, repmgr or Spock cluster frame (from the backend to the pool)."}},
			[]catalogEdge{{To: []string{"stocksim", "ledgersim", "perfsim"}, Type: "directional",
				Desc: "Drive a simulator's workload through this pool (from the pool to the simulator)."}},
		),
		Actions: []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/activity", Desc: "The pool's clients and pools (SHOW CLIENTS and SHOW POOLS on the admin console)."},
		},
	},

	// ---------------------------------------------------------- Valkey
	{Type: "valkey", Label: "Valkey", Summary: "A standalone Valkey server, installed from percona-release.", Placement: "standalone",
		Requires: []string{"an Intranet node", "useLdap: an Oracle Linux image (percona-valkey-ldap is not published for Ubuntu)"},
		Options: pgcatOpts(
			pgcatSizing,
			pgcatValkeyVersion,
			[]catalogOption{
				{Field: "rootPassword", Desc: "The default user's password (requirepass); \"\" uses the installation's VALKEY_PASSWORD."},
				{Field: "useLdap", Desc: "Load the valkey-ldap module and authenticate against the Intranet's OpenLDAP (users under ou=People). Oracle Linux only."},
				{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
				{Field: "exportEnabled", Desc: "Publish Valkey's port 6379 on the host."},
				{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
			},
		),
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{{To: []string{"stocksim", "perfsim"}, Type: "directional",
			Desc: "Drive a simulator's workload against this Valkey (from the server to the simulator)."}},
	},
	{Type: "valkeycluster", Label: "Valkey Cluster", Summary: "A Valkey Cluster shard member (an all-master shard).", Placement: "member",
		FrameTypes: []string{"valkeycluster"},
		Options: pgcatOpts(pgcatSizing, []catalogOption{
			{Field: "exportEnabled", Desc: "Publish this member's port 6379 on the host."},
			{Field: "exportHostPort", Desc: "Host port to publish it on; 0 picks an unused one.", When: "exportEnabled is on"},
		}),
		Actions: []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/config-drift", Desc: "Every member's CONFIG GET settings, and the ones that differ."},
		},
	},
	{Type: "valkeycluster", Label: "Valkey Cluster", Summary: "A Valkey Cluster of all-master shards, created with valkey-cli --cluster create; installed from percona-release.", Placement: "frame",
		MemberType: "valkeycluster", DefaultMembers: 3, MinMembers: 3, MaxMembers: 7,
		Requires: []string{
			"an Intranet node",
			"3 to 7 members",
			"useLdap: an Oracle Linux image (percona-valkey-ldap is not published for Ubuntu)",
		},
		Options: pgcatOpts(
			pgcatValkeyVersion,
			[]catalogOption{
				{Field: "rootPassword", Desc: "The default user's password, shared by every member; \"\" uses the installation's VALKEY_PASSWORD."},
				{Field: "useLdap", Desc: "Load the valkey-ldap module on every member and authenticate against the Intranet's OpenLDAP. Oracle Linux only."},
				{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
			},
		),
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{{To: []string{"stocksim", "perfsim"}, Type: "directional",
			Desc: "Drive a simulator's workload against this cluster (from the cluster to the simulator)."}},
	},
}
