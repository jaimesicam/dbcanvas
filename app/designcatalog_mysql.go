package main

// designcatalog_mysql.go — the catalogue's entries for MySQL, Percona Server, PXC, MariaDB, MySQL Community, ProxySQL and HAProxy. See designcatalog.go.

// mysqlCatSizing are the per-node container sizing and network-shaping fields of every
// MySQL-family node, proxy and HAProxy node (standalone or member).
var mysqlCatSizing = []catalogOption{
	{Field: "cpus", Desc: "CPUs: the container's --cpus limit on Docker (0 = unlimited) or the VirtualBox VM's CPUs on Vagrant. Locked once deployed."},
	{Field: "memoryGb", Desc: "Memory in GiB: the container's --memory limit on Docker (0 = unlimited) or the VirtualBox VM's memory on Vagrant. Locked once deployed."},
	{Field: "deviceReadMbps", Desc: "Disk read limit in MB/s (container --device-read-bps); 0 = unlimited. Docker only."},
	{Field: "deviceWriteMbps", Desc: "Disk write limit in MB/s (container --device-write-bps); 0 = unlimited. Docker only."},
	{Field: "devicePath", Desc: "Host block device the disk limits apply to; \"\" auto-detects the disk behind Docker's data root.", When: "deviceReadMbps or deviceWriteMbps is set"},
	{Field: "netLatencyMs", Desc: "Added one-way latency in ms (0–1000) on the node's database and cluster ports, applied with tc after the cluster forms. Docker only; not locked after deploy (a redeploy re-applies it)."},
	{Field: "netJitterMs", Desc: "Latency jitter in ± ms, capped at netLatencyMs. Docker only."},
	{Field: "netLossPct", Desc: "Outbound packet loss in percent (0–100; 100 severs the link while the node stays up). Docker only."},
	{Field: "netRateMbit", Desc: "Outbound bandwidth cap in Mbit/s; 0 = unlimited. Docker only."},
	{Field: "netAllTraffic", Desc: "Shape every packet the node sends, not only its database and cluster ports (DNS, LDAP and health checks are impaired too). Docker only."},
}

// mysqlCatCert are the Intranet-CA TLS fields.
var mysqlCatCert = []catalogOption{
	{Field: "generateCert", Desc: "Issue the server's TLS certificate from the Intranet CA at deploy (per member on a cluster). Locked once deployed."},
	{Field: "certTtlValue", Default: 365, Desc: "Certificate lifetime, in certTtlUnit.", When: "generateCert is on"},
	{Field: "certTtlUnit", Default: "days", Values: []string{"minutes", "hours", "days"}, Desc: "Unit of certTtlValue; any other value is read as days.", When: "generateCert is on"},
}

var mysqlCatUseProxy = catalogOption{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."}

var mysqlCatGTID = catalogOption{Field: "gtid", Default: true, Desc: "Enable GTIDs (gtid_mode=ON, enforce_gtid_consistency); replicas then use auto-positioning, otherwise binary-log file/position."}

// mysqlCatXtraBackup are the Percona XtraBackup fields of a standalone Percona Server node
// and the Percona Server replication and PXC frames (mysqlbackup.go). They stay editable on
// a deployed node or cluster: nothing is set up until the first backup.
var mysqlCatXtraBackup = []catalogOption{
	{Field: "useXtraBackup", Desc: "Back up with Percona XtraBackup (full and incremental) to the SeaweedFS node of the backup relation (seaweedfsNodeId). Not allowed together with enableVault."},
	{Field: "xbPitr", Desc: "Also ship binary logs to the bucket, for point-in-time restore. The designer turns it on with useXtraBackup.", When: "useXtraBackup is on"},
	{Field: "seaweedfsBucket", Desc: "Which bucket of the SeaweedFS node backups go to; \"\" is its first (default) bucket.", When: "useXtraBackup is on"},
}

// mysqlCatPSAuth are the directory and Keycloak login fields of Percona Server (standalone
// node, replication frame) and PXC (frame). On a frame every member loads the plugins.
func mysqlCatPSAuth(product string) []catalogOption {
	return []catalogOption{
		{Field: "ldapAuth", Desc: "Load authentication_ldap_simple against the directory of the ldap relation (ldapDirNodeId: an Intranet or Samba AD DC node). Not available on Percona Server 5.7. Locked once deployed."},
		{Field: "enableOIDC", Desc: "Load the auth_openid_connect plugin for token logins issued by the Keycloak node of the oidc relation (keycloakNodeId, which must serve HTTPS). Needs " + product + " 8.4.11-11 / 9.7.2-2 or newer; the designer moves the major to 8.4 (9.7 stays) and the minor to latest when it is turned on. Locked once deployed."},
		{Field: "oidcRealm", Default: "dbcanvas", Desc: "Keycloak realm holding the OIDC client; \"\" means \"dbcanvas\".", When: "enableOIDC is on"},
	}
}

// mysqlCatMemberExport is the per-member port export of a MySQL-family cluster member.
var mysqlCatMemberExport = []catalogOption{
	{Field: "exportEnabled", Desc: "Publish the member's MySQL port (3306) on the Docker host. Locked once deployed."},
	{Field: "exportHostPort", Desc: "Host port for 3306; 0 picks a free one. Must not clash with another node.", When: "exportEnabled is on"},
}

// mysqlCatDBActions apply to every deployed MySQL-family server: standalone or member.
var mysqlCatDBActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/activity", Desc: "Sessions, lock waits (row and metadata), open transactions and DDL in flight. ?with=digests,resources,deadlock adds statement digests, per-session resources and the latest deadlock."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/kill", Desc: "KILL QUERY or KILL a session.", Body: "session, mode (query|connection)"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/explain", Desc: "The plan of a running session's statement (EXPLAIN FOR CONNECTION; SHOW EXPLAIN on MariaDB).", Body: "session, statement, db"},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/deep", Desc: "Turn on the performance_schema stage, wait and transaction instruments for a while; they switch back off by themselves.", Body: "minutes (1–60; 0 turns it off)"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/deadlocks", Desc: "The InnoDB deadlocks the watcher recorded, newest first."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk", Desc: "Whether a pt-stalk capture is running and what it has collected."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk", Desc: "Start a capture of pt-summary, pt-mysql-summary and about 90 s of pt-stalk samples."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk/download", Desc: "Download the current capture as a .tar.gz."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk/archives", Desc: "The kept pt-stalk archives captured from this node."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stalksummary", Desc: "Analyse the node's current pt-stalk capture (pt-stalk Summary)."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/databases", Desc: "The server's databases, for the Data Generator."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/tables", Desc: "A database's tables with their row counts."},
	{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/columns", Desc: "A table's columns and the generators inferred for them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/preview", Desc: "Generate sample rows without writing them."},
	{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/generate", Desc: "Start a generation job; returns its id.", Body: "database, table, rows, batch, threads, seed, stopOnError, columns", When: "the node takes writes (standalone, a primary, or any Galera member)"},
}

// mysqlCatGroupActions apply to a deployed member of any MySQL-family cluster (the
// replication, Group Replication and Galera kinds; app/rebuild.go rebuildKinds).
var mysqlCatGroupActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/config-drift", Desc: "Every member's global variables, and the ones that differ side by side (identity settings left out, role settings marked expected)."},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Whether the cluster can be restarted one member at a time, and the running or last rolling restart with its steps."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/rolling-restart", Desc: "Restart the cluster one member at a time in the background: replicas first, each back and caught up before the next (Galera: each until Synced).", Body: "switchover (hand the primary to a restarted replica and restart the old primary last)"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "The cluster's database accounts, with the members that have each one and any whose credential differs from the primary's."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/db-users", Desc: "Create, rotate the password of, or drop an account on the primary (a Synced member on Galera), and confirm it reached every member.", Body: "action (create|password|drop), name, host (\"%\" by default), password, privilege (readonly|readwrite|admin), database (\"\" = all)"},
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "The failure drills the cluster offers, and the stack's running drill with its checklist."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/drill", Desc: "Start a failure drill; the fault is real. kill-primary on Group Replication (the primary) and Galera (this member); stop-replication on an async replica.", Body: "kind (kill-primary|stop-replication)"},
}

// mysqlCatSwitchActions apply to a deployed member of a cluster with a single primary:
// async/semi-sync replication and Group Replication / InnoDB Cluster.
var mysqlCatSwitchActions = []catalogAction{
	{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/switchover", Desc: "Whether the cluster can switch its primary, who the primary is, and which members could take over, as the servers report it now."},
	{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/promote", Desc: "Make this member the primary, demoting the current one (async: writes frozen and every replica caught up first; Group Replication: group_replication_set_as_primary). Returns the steps taken."},
}

// mysqlCatXBFrameActions are a Percona Server replication or PXC cluster's XtraBackup
// endpoints.
var mysqlCatXBFrameActions = []catalogAction{
	{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/xb/backup", Desc: "Take an XtraBackup backup of the cluster to SeaweedFS.", Body: "type (full|incremental)", When: "useXtraBackup is on"},
	{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/xb/backups", Desc: "The cluster's XtraBackup backups and the window a point-in-time restore can reach.", When: "useXtraBackup is on"},
	{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/xb/restore", Desc: "Restore the cluster from XtraBackup: one backup, the latest point, or a time (with xbPitr).", Body: "mode (backup|latest|time), name (mode backup), time (RFC 3339, mode time)", When: "useXtraBackup is on"},
	{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/xb/delete", Desc: "Delete a backup and every incremental built on it.", Body: "name", When: "useXtraBackup is on"},
	{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/backup-status", Desc: "When the cluster was last backed up: how many backups, the newest one's time, type and size, and whether one is running.", When: "useXtraBackup is on"},
}

// mysqlCatProxyEdge is the association a PXC or Percona Server replication frame draws to
// the proxy in front of it.
var mysqlCatProxyEdge = catalogEdge{To: []string{"proxysql", "haproxy"}, Type: "directional",
	Desc: "Front the cluster with a ProxySQL node or cluster frame, or an HAProxy node. Stored from the cluster frame to the proxy; the frame takes one association line in all (a proxy or a simulator)."}

// mysqlCatReplEdges are the cross-cluster replication links between members of two
// different PXC / Percona Server replication frames (replication.go).
var mysqlCatReplEdges = []catalogEdge{
	{To: []string{"pxc", "mysql"}, Type: "async",
		Desc: "Asynchronous replication channel (xrepl_<source host>) from this member (from end, the source) to a member of another PXC or Percona Server replication cluster (to end, the replica). GTID auto-positioning when both clusters have gtid on, else file/position from the source's current coordinates. Reconciled on every deploy."},
	{To: []string{"pxc", "mysql"}, Type: "bidir",
		Desc: "Replication both ways between this member and a member of another PXC or Percona Server replication cluster: one channel on each side (multi-writer)."},
}

func mysqlCatOpts(groups ...[]catalogOption) []catalogOption {
	var out []catalogOption
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func mysqlCatActs(groups ...[]catalogAction) []catalogAction {
	var out []catalogAction
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// mysqlCatImage is the os/osVersion pair of a catalogue-driven image picker.
func mysqlCatImage(catalog, product string) []catalogOption {
	return []catalogOption{
		{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux", "ubuntu"}, Desc: "OS family of the image; the designer offers those GET " + catalog + " lists with " + product + " packages. Locked once deployed."},
		{Field: "osVersion", Default: "9", Desc: "OS release of the image (e.g. \"9\", \"24.04\"), as GET " + catalog + " lists it for the chosen os. Locked once deployed."},
	}
}

var catalogMySQLFamily = []catalogEntry{
	// ------------------------------------------------------------------ standalone servers
	{Type: "ps", Label: "Percona Server", Summary: "A standalone Percona Server for MySQL instance (no replication).", Placement: "standalone",
		Requires: []string{"an Intranet node"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/ps", "Percona Server"),
			[]catalogOption{
				{Field: "psMajor", Default: "8.0", Desc: "Percona Server series (e.g. \"8.0\", \"8.4\"), from GET /api/catalog/ps for the chosen image. Locked once deployed."},
				{Field: "psVersion", Desc: "Exact Percona Server package version (e.g. \"8.4.11-11\"); \"\" is the newest of the series. Locked once deployed."},
				mysqlCatGTID,
				mysqlCatUseProxy,
			},
			mysqlCatCert,
			[]catalogOption{
				{Field: "exportEnabled", Desc: "Publish the MySQL port (3306) on the Docker host. Locked once deployed."},
				{Field: "exportHostPort", Desc: "Host port for 3306; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatPSAuth("Percona Server"),
			mysqlCatXtraBackup,
			[]catalogOption{
				{Field: "enableVault", Desc: "Data-at-rest encryption keyed to the OpenBao node of the vault relation: component_keyring_vault on 8.4, the keyring_vault plugin on 5.7/8.0, with its own KV mount and token. Not allowed together with useXtraBackup. Locked once deployed."},
			},
			mysqlCatSizing,
		),
		Relations: []string{"monitor", "repository", "ldap", "oidc", "vault", "backup"},
		Edges: []catalogEdge{
			{To: []string{"airlinesim", "marketchaos", "stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against this server (stored from the server to the simulator; one per server)."},
		},
		Actions: mysqlCatActs(mysqlCatDBActions, []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/xb/backup", Desc: "Take an XtraBackup backup of the server to SeaweedFS.", Body: "type (full|incremental)", When: "useXtraBackup is on"},
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/xb/backups", Desc: "The server's XtraBackup backups and the window a point-in-time restore can reach.", When: "useXtraBackup is on"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/xb/restore", Desc: "Restore the server from XtraBackup: one backup, the latest point, or a time (with xbPitr).", Body: "mode (backup|latest|time), name (mode backup), time (RFC 3339, mode time)", When: "useXtraBackup is on"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/xb/delete", Desc: "Delete a backup and every incremental built on it.", Body: "name", When: "useXtraBackup is on"},
		}),
	},
	{Type: "mariadb", Label: "MariaDB", Summary: "A standalone MariaDB server from mariadb.org packages.", Placement: "standalone",
		Requires: []string{"an Intranet node"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mariadb", "MariaDB"),
			[]catalogOption{
				{Field: "mariadbMajor", Default: "11.4", Desc: "MariaDB series (e.g. \"10.6\", \"10.11\", \"11.4\", \"11.8\"), from GET /api/catalog/mariadb for the chosen image; coverage differs per OS. Locked once deployed."},
				{Field: "mariadbVersion", Desc: "Exact MariaDB version (e.g. \"11.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				{Field: "gtid", Default: true, Desc: "Enable GTID-based positioning (MariaDB domain-server-sequence GTIDs)."},
				mysqlCatUseProxy,
			},
			mysqlCatCert,
			[]catalogOption{
				{Field: "exportEnabled", Desc: "Publish the MySQL port (3306) on the Docker host."},
				{Field: "exportHostPort", Desc: "Host port for 3306; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Relations: []string{"monitor"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against this server (stored from the server to the simulator; one per server)."},
		},
		Actions: mysqlCatDBActions,
	},
	{Type: "mysqlce", Label: "MySQL", Summary: "A standalone MySQL Community server from repo.mysql.com packages.", Placement: "standalone",
		Requires: []string{"an Intranet node"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mysqlce", "MySQL Community"),
			[]catalogOption{
				{Field: "mysqlceMajor", Default: "8.4", Desc: "MySQL series (\"8.0\" or \"8.4\"), from GET /api/catalog/mysqlce for the chosen image. Locked once deployed."},
				{Field: "mysqlceVersion", Desc: "Exact MySQL version (e.g. \"8.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				mysqlCatGTID,
				mysqlCatUseProxy,
			},
			mysqlCatCert,
			[]catalogOption{
				{Field: "exportEnabled", Desc: "Publish the MySQL port (3306) on the Docker host."},
				{Field: "exportHostPort", Desc: "Host port for 3306; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Relations: []string{"monitor"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against this server (stored from the server to the simulator; one per server)."},
		},
		Actions: mysqlCatDBActions,
	},

	// ------------------------------------------------------------------ cluster members
	{Type: "pxc", Label: "PXC Node", Summary: "A Percona XtraDB Cluster member: a data node or a garbd arbitrator.", Placement: "member", FrameTypes: []string{"pxc"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "role", Default: "regular", Values: []string{"regular", "arbitrator"}, Desc: "\"regular\" is a data node; \"arbitrator\" runs garbd, which votes for quorum and stores no data. Locked once deployed."},
			},
			mysqlCatMemberExport,
			mysqlCatSizing,
		),
		Edges: append(append([]catalogEdge{}, mysqlCatReplEdges...),
			catalogEdge{To: []string{"marketchaos"}, Type: "directional",
				Desc: "Drive a MarketChaos workload against this one member directly, bypassing any proxy."}),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pxc/cert", Desc: "The member's server and cluster-internal certificates.", When: "role is regular"},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pxc/cert", Desc: "Reissue the member's certificates from the Intranet CA.", Body: "value, unit (minutes|hours|days)", When: "role is regular; a running Intranet node"},
		}),
	},
	{Type: "mysql", Label: "Percona Server", Summary: "A member of a Percona Server async or semi-sync replication cluster: the primary or a read-only secondary.", Placement: "member", FrameTypes: []string{"mysql"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "role", Default: "secondary", Values: []string{"primary", "secondary"}, Desc: "Exactly one member is the primary (read/write); the rest are super_read_only secondaries replicating from it. A new cluster's first member is the primary. Locked once deployed (use promote to move it)."},
			},
			mysqlCatMemberExport,
			mysqlCatSizing,
		),
		Edges: append(append([]catalogEdge{}, mysqlCatReplEdges...),
			catalogEdge{To: []string{"marketchaos"}, Type: "directional",
				Desc: "Drive a MarketChaos workload against this one member directly, bypassing any proxy."}),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, mysqlCatSwitchActions),
	},
	{Type: "innodb", Label: "InnoDB Cluster / GR", Summary: "A Percona Server Group Replication member (InnoDB Cluster or raw Group Replication); the group elects its primary.", Placement: "member", FrameTypes: []string{"innodb"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "exportEnabled", Desc: "Publish the member's MySQL Router ports on the Docker host: read/write 6446 at exportHostPort, read-only 6447 on a free port. Locked once deployed.", When: "the frame's mysqlRouter is on"},
				{Field: "exportHostPort", Desc: "Host port for Router's read/write port 6446; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, mysqlCatSwitchActions),
	},
	{Type: "mariadbrepl", Label: "MariaDB Replication", Summary: "A member of a MariaDB async or semi-sync replication cluster: the primary or a read_only secondary.", Placement: "member", FrameTypes: []string{"mariadbrepl"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "role", Default: "secondary", Values: []string{"primary", "secondary"}, Desc: "Exactly one member is the primary; the rest are read_only secondaries (MariaDB has no super_read_only, so SUPER accounts can still write to one). A new cluster's first member is the primary. Locked once deployed."},
			},
			mysqlCatMemberExport,
			mysqlCatSizing,
		),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, mysqlCatSwitchActions),
	},
	{Type: "mariadbgalera", Label: "MariaDB Galera", Summary: "A MariaDB Galera Cluster member; every member takes writes.", Placement: "member", FrameTypes: []string{"mariadbgalera"},
		Options: mysqlCatOpts(mysqlCatMemberExport, mysqlCatSizing),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions),
	},
	{Type: "mysqlcerepl", Label: "MySQL Replication", Summary: "A member of a MySQL Community async or semi-sync replication cluster: the primary or a read-only secondary.", Placement: "member", FrameTypes: []string{"mysqlcerepl"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "role", Default: "secondary", Values: []string{"primary", "secondary"}, Desc: "Exactly one member is the primary; the rest are read-only secondaries. A new cluster's first member is the primary. Locked once deployed."},
			},
			mysqlCatMemberExport,
			mysqlCatSizing,
		),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, mysqlCatSwitchActions),
	},
	{Type: "mysqlceinnodb", Label: "MySQL InnoDB / GR", Summary: "A MySQL Community Group Replication member (InnoDB Cluster or raw Group Replication); the group elects its primary.", Placement: "member", FrameTypes: []string{"mysqlceinnodb"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "exportEnabled", Desc: "Publish the member's MySQL Router ports on the Docker host: read/write 6446 at exportHostPort, read-only 6447 on a free port. Nothing is published without Router.", When: "the frame's mysqlRouter is on"},
				{Field: "exportHostPort", Desc: "Host port for Router's read/write port 6446; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Actions: mysqlCatActs(mysqlCatDBActions, mysqlCatGroupActions, mysqlCatSwitchActions),
	},

	// ------------------------------------------------------------------ proxies
	{Type: "proxysql", Label: "ProxySQL", Summary: "A ProxySQL node in front of a PXC or Percona Server replication cluster: standalone, or a member of a ProxySQL cluster frame.", Placement: "standalone,member", FrameTypes: []string{"proxysql"},
		Requires: []string{"an Intranet node", "standalone: an association line from a PXC or Percona Server replication cluster frame (directly, or through another ProxySQL node)"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux", "ubuntu"}, Desc: "OS family of the image, from GET /api/catalog/proxysql. Locked once deployed.", When: "standalone (a member takes the frame's)"},
				{Field: "osVersion", Default: "9", Desc: "OS release of the image (e.g. \"9\"), as GET /api/catalog/proxysql lists it. Locked once deployed.", When: "standalone"},
				{Field: "proxysqlMajor", Default: "2", Values: []string{"2", "3"}, Desc: "ProxySQL series (proxysql2 or proxysql3), from GET /api/catalog/proxysql for the chosen image. Locked once deployed.", When: "standalone"},
				{Field: "proxysqlVersion", Desc: "Exact ProxySQL package version (e.g. \"2.7.1-1\"); \"\" is the newest of the series. A backend on 8.4 or 9.x needs 2.6 or later; a PXC backend may need a newer proxysql-admin. Locked once deployed.", When: "standalone"},
				{Field: "mode", Default: "singlewrite", Values: []string{"singlewrite", "loadbal", "rwsplit", "primary"}, Desc: "Routing. With a PXC backend, proxysql-admin's MODE: \"singlewrite\" (one writer) or \"loadbal\" (all members). With a Percona Server replication backend: \"rwsplit\" (writes to the primary, reads to replicas) or \"primary\" (everything to the primary); the designer snaps the value to the backend's first mode. Locked once deployed.", When: "standalone"},
				{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy.", When: "standalone"},
				{Field: "exportEnabled", Desc: "Publish ProxySQL's MySQL port 6033 on the Docker host at exportHostPort, and its admin port 6032 on a free port. Locked once deployed."},
				{Field: "exportHostPort", Desc: "Host port for 6033; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{
			{To: []string{"pxc", "mysql"}, Type: "directional", Required: true,
				Desc: "The backend cluster frame, stored from the frame to this node. A standalone ProxySQL takes one incoming line. A member uses its frame's line instead."},
			{To: []string{"proxysql"}, Type: "directional",
				Desc: "Chain to another standalone ProxySQL node; the backend is found by walking the lines undirected."},
			{To: []string{"airlinesim", "stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload through this proxy (one per proxy)."},
		},
		Actions: []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/activity", Desc: "ProxySQL's client sessions and connection pools."},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/activity/kill", Desc: "KILL CONNECTION on a ProxySQL client session.", Body: "session, mode"},
		},
	},
	{Type: "haproxy", Label: "HAProxy", Summary: "A TCP load balancer fronting one cluster: Patroni, repmgr, Spock, PXC or Percona Server replication. Writes on 5000, reads on 5001, stats on 7000.", Placement: "standalone",
		Requires: []string{"an Intranet node", "exactly one association line from a Patroni, repmgr, Spock, PXC or Percona Server replication cluster frame"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux", "ubuntu"}, Desc: "OS family of the image, from GET /api/catalog/images. Locked once deployed."},
				{Field: "osVersion", Default: "9", Desc: "OS release of the image, as GET /api/catalog/images lists it for the chosen os. Locked once deployed."},
				{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
				{Field: "exportEnabled", Desc: "Publish the write port 5000 on the Docker host at exportHostPort, and the read (5001) and stats (7000) ports on free ports. Locked once deployed."},
				{Field: "exportHostPort", Desc: "Host port for the write port 5000; 0 picks a free one.", When: "exportEnabled is on"},
			},
			mysqlCatSizing,
		),
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{
			{To: []string{"pxc", "mysql", "patroni", "repmgr", "spock"}, Type: "directional", Required: true,
				Desc: "The one backend cluster frame, stored from the frame to this node. PXC: writes to a single writer, reads round-robin (clustercheck on 9200); Patroni: the leader and replicas by the REST API."},
			{To: []string{"airlinesim", "carsim", "marketchaos", "stocksim", "ledgersim", "pgvectorsim"}, Type: "directional",
				Desc: "Drive a simulator's workload through this load balancer (one per node); the simulator must match the backend's engine."},
		},
	},

	// ------------------------------------------------------------------ cluster frames
	{Type: "pxc", Label: "PXC Cluster", Summary: "A Percona XtraDB Cluster (Galera): synchronous multi-writer replication between data nodes, with optional garbd arbitrators.", Placement: "frame",
		MemberType: "pxc", DefaultMembers: 3, MinMembers: 1, MaxMembers: 9,
		Requires: []string{"an Intranet node", "at least one regular (data) member; 3 or more and an odd total are recommended"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/pxc", "PXC"),
			[]catalogOption{
				{Field: "pxcMajor", Default: "8.0", Desc: "PXC series (e.g. \"8.0\", \"8.4\", \"9.7\"), from GET /api/catalog/pxc for the chosen image. Locked once deployed."},
				{Field: "pxcVersion", Desc: "Exact PXC package version (e.g. \"8.0.45-36.1\"); \"\" is the newest of the series. Locked once deployed."},
				mysqlCatUseProxy,
				mysqlCatGTID,
			},
			mysqlCatCert,
			mysqlCatPSAuth("PXC"),
			mysqlCatXtraBackup,
			[]catalogOption{
				{Field: "enableVault", Desc: "Data-at-rest encryption on every data member, keyed to the OpenBao node of the vault relation, each with its own KV mount; also turns on pxc_encrypt_cluster_traffic with one shared Intranet-CA certificate (per-member certificates are skipped). Not allowed together with useXtraBackup. Locked once deployed."},
			},
		),
		Relations: []string{"monitor", "repository", "ldap", "oidc", "vault", "backup"},
		Edges: []catalogEdge{mysqlCatProxyEdge,
			{To: []string{"airlinesim", "marketchaos", "stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster directly (counts as the frame's one association line)."},
		},
		Actions: mysqlCatActs([]catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/pmm", Desc: "Apply the frame's PMM monitoring to the running data members now: install pmm-client and register, or deregister when pmmNodeId is \"\".", Body: "pmmNodeId"},
		}, mysqlCatXBFrameActions),
	},
	{Type: "mysql", Label: "Percona Server Replication", Summary: "Percona Server async or semi-sync replication: one primary and read-only secondaries.", Placement: "frame",
		MemberType: "mysql", DefaultMembers: 3, MinMembers: 2, MaxMembers: 9,
		Requires: []string{"an Intranet node", "exactly one primary member and at least one secondary"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/ps", "Percona Server"),
			[]catalogOption{
				{Field: "psMajor", Default: "8.0", Desc: "Percona Server series (e.g. \"8.0\", \"8.4\"), from GET /api/catalog/ps for the chosen image. Locked once deployed."},
				{Field: "psVersion", Desc: "Exact Percona Server package version (e.g. \"8.4.11-11\"); \"\" is the newest of the series. Locked once deployed."},
				{Field: "replMode", Default: "async", Values: []string{"async", "semisync"}, Desc: "\"async\" replication, or \"semisync\" (the primary waits for one replica to acknowledge each commit). Locked once deployed."},
				mysqlCatUseProxy,
				mysqlCatGTID,
			},
			mysqlCatCert,
			mysqlCatPSAuth("Percona Server"),
			mysqlCatXtraBackup,
			[]catalogOption{
				{Field: "enableVault", Desc: "Data-at-rest encryption on every member, keyed to the OpenBao node of the vault relation, each with its own KV mount and key. Not allowed together with useXtraBackup. Locked once deployed."},
			},
		),
		Relations: []string{"monitor", "orchestrator", "repository", "ldap", "oidc", "vault", "backup"},
		Edges: []catalogEdge{mysqlCatProxyEdge,
			{To: []string{"airlinesim", "marketchaos", "stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster's primary directly (counts as the frame's one association line)."},
		},
		Actions: mysqlCatActs([]catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/orchestrator", Desc: "Seed or refresh topology discovery on the Orchestrator node now; \"\" clears the link (Orchestrator is not asked to forget the cluster).", Body: "orchestratorNodeId"},
		}, mysqlCatXBFrameActions),
	},
	{Type: "innodb", Label: "InnoDB Cluster / GR", Summary: "Percona Server Group Replication, managed as an InnoDB Cluster through MySQL Shell or as raw Group Replication, with optional MySQL Router on every member.", Placement: "frame",
		MemberType: "innodb", DefaultMembers: 3, MinMembers: 1, MaxMembers: 9,
		Requires: []string{"an Intranet node", "at least one member; 3 or more and an odd count are recommended for quorum"},
		Options: mysqlCatOpts(
			[]catalogOption{
				{Field: "replMode", Default: "innodbcluster", Values: []string{"innodbcluster", "groupreplication"}, Desc: "\"innodbcluster\" (MySQL Shell orchestration and Router metadata) or \"groupreplication\" (raw Group Replication). GTIDs are always on. Locked once deployed."},
				{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux", "ubuntu"}, Desc: "OS family of the image, from GET /api/catalog/ps. Locked once deployed."},
				{Field: "osVersion", Default: "9", Desc: "OS release of the image, as GET /api/catalog/ps lists it for the chosen os. Locked once deployed."},
				{Field: "pdpsRepo", Desc: "percona-release repository that sets the Percona Server version (e.g. \"pdps-84-lts\"), one of GET /api/catalog/pdps; the designer picks the first when empty. Locked once deployed."},
				{Field: "mysqlRouter", Default: true, Desc: "Install and run MySQL Router on every member (6446 read/write, 6447 read-only). Locked once deployed."},
				mysqlCatUseProxy,
			},
			mysqlCatCert,
		),
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster (stored from the frame to the simulator)."},
		},
	},
	{Type: "mariadbrepl", Label: "MariaDB Replication", Summary: "MariaDB async or semi-sync replication: one primary and read_only secondaries.", Placement: "frame",
		MemberType: "mariadbrepl", DefaultMembers: 3, MinMembers: 2, MaxMembers: 9,
		Requires: []string{"an Intranet node", "exactly one primary member and at least one secondary"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mariadb", "MariaDB"),
			[]catalogOption{
				{Field: "mariadbMajor", Default: "11.4", Desc: "MariaDB series (e.g. \"10.6\", \"10.11\", \"11.4\", \"11.8\"), from GET /api/catalog/mariadb for the chosen image. Locked once deployed."},
				{Field: "mariadbVersion", Desc: "Exact MariaDB version (e.g. \"11.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				{Field: "replMode", Default: "async", Values: []string{"async", "semisync"}, Desc: "\"async\" replication, or \"semisync\" (the primary waits for one replica to acknowledge each commit)."},
				{Field: "gtid", Default: true, Desc: "Replicate with MariaDB GTIDs (MASTER_USE_GTID); the GTID domain is derived from the cluster name. Off: binary-log file/position."},
				mysqlCatUseProxy,
			},
			mysqlCatCert,
		),
		Relations: []string{"monitor", "orchestrator"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster's primary (stored from the frame to the simulator)."},
		},
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/orchestrator", Desc: "Seed or refresh topology discovery on the Orchestrator node now; \"\" clears the link.", Body: "orchestratorNodeId"},
		},
	},
	{Type: "mariadbgalera", Label: "MariaDB Galera", Summary: "MariaDB Galera Cluster: synchronous multi-writer replication, mariabackup state transfers.", Placement: "frame",
		MemberType: "mariadbgalera", DefaultMembers: 3, MinMembers: 3, MaxMembers: 9,
		Requires: []string{"an Intranet node", "at least 3 members; an odd count is recommended"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mariadb", "MariaDB"),
			[]catalogOption{
				{Field: "mariadbMajor", Default: "11.4", Desc: "MariaDB series (e.g. \"10.11\", \"11.4\", \"11.8\"), from GET /api/catalog/mariadb for the chosen image. Locked once deployed."},
				{Field: "mariadbVersion", Desc: "Exact MariaDB version (e.g. \"11.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				mysqlCatUseProxy,
			},
			mysqlCatCert,
		),
		Relations: []string{"monitor"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster (stored from the frame to the simulator)."},
		},
	},
	{Type: "mysqlcerepl", Label: "MySQL Replication", Summary: "MySQL Community async or semi-sync replication: one primary and read-only secondaries.", Placement: "frame",
		MemberType: "mysqlcerepl", DefaultMembers: 3, MinMembers: 2, MaxMembers: 9,
		Requires: []string{"an Intranet node", "exactly one primary member and at least one secondary"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mysqlce", "MySQL Community"),
			[]catalogOption{
				{Field: "mysqlceMajor", Default: "8.4", Desc: "MySQL series (\"8.0\" or \"8.4\"), from GET /api/catalog/mysqlce for the chosen image. Locked once deployed."},
				{Field: "mysqlceVersion", Desc: "Exact MySQL version (e.g. \"8.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				{Field: "replMode", Default: "async", Values: []string{"async", "semisync"}, Desc: "\"async\" replication, or \"semisync\" (the primary waits for one replica to acknowledge each commit)."},
				mysqlCatGTID,
				mysqlCatUseProxy,
			},
			mysqlCatCert,
		),
		Relations: []string{"monitor", "orchestrator"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster's primary (stored from the frame to the simulator)."},
		},
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/orchestrator", Desc: "Seed or refresh topology discovery on the Orchestrator node now; \"\" clears the link.", Body: "orchestratorNodeId"},
		},
	},
	{Type: "mysqlceinnodb", Label: "MySQL InnoDB / GR", Summary: "MySQL Community Group Replication, as an InnoDB Cluster through MySQL Shell or as raw Group Replication, with optional MySQL Router on every member.", Placement: "frame",
		MemberType: "mysqlceinnodb", DefaultMembers: 3, MinMembers: 3, MaxMembers: 9,
		Requires: []string{"an Intranet node", "at least 3 members; an odd count is recommended for quorum"},
		Options: mysqlCatOpts(
			mysqlCatImage("/api/catalog/mysqlce", "MySQL Community"),
			[]catalogOption{
				{Field: "mysqlceMajor", Default: "8.4", Desc: "MySQL series (\"8.0\" or \"8.4\"), from GET /api/catalog/mysqlce for the chosen image. Locked once deployed."},
				{Field: "mysqlceVersion", Desc: "Exact MySQL version (e.g. \"8.4.5\"); \"\" is the newest of the series. Locked once deployed."},
				{Field: "replMode", Default: "innodbcluster", Values: []string{"innodbcluster", "groupreplication"}, Desc: "\"innodbcluster\" (MySQL Shell orchestration and cluster metadata) or \"groupreplication\" (raw Group Replication). Locked once deployed."},
				{Field: "mysqlRouter", Default: true, Desc: "Install and run MySQL Router on every member (6446 read/write, 6447 read-only)."},
				mysqlCatUseProxy,
			},
			mysqlCatCert,
		),
		Relations: []string{"monitor"},
		Edges: []catalogEdge{
			{To: []string{"stocksim", "ledgersim"}, Type: "directional",
				Desc: "Drive a simulator's workload against the cluster (stored from the frame to the simulator)."},
		},
	},
	{Type: "proxysql", Label: "ProxySQL Cluster", Summary: "Several ProxySQL nodes sharing one configuration, in front of a PXC or Percona Server replication cluster.", Placement: "frame",
		MemberType: "proxysql", DefaultMembers: 3, MinMembers: 1, MaxMembers: 9,
		Requires: []string{"an Intranet node", "an association line from a PXC or Percona Server replication cluster frame"},
		Options: []catalogOption{
			{Field: "os", Default: "oraclelinux", Values: []string{"oraclelinux", "ubuntu"}, Desc: "OS family of the image, from GET /api/catalog/proxysql. Locked once deployed."},
			{Field: "osVersion", Default: "9", Desc: "OS release of the image (e.g. \"9\"), as GET /api/catalog/proxysql lists it. Locked once deployed."},
			{Field: "proxysqlMajor", Default: "2", Values: []string{"2", "3"}, Desc: "ProxySQL series (proxysql2 or proxysql3), from GET /api/catalog/proxysql for the chosen image. Locked once deployed."},
			{Field: "proxysqlVersion", Desc: "Exact ProxySQL package version (e.g. \"2.7.1-1\"); \"\" is the newest of the series. A backend on 8.4 or 9.x needs 2.6 or later. Locked once deployed."},
			{Field: "mode", Default: "singlewrite", Values: []string{"singlewrite", "loadbal", "rwsplit", "primary"}, Desc: "Routing. PXC backend: \"singlewrite\" or \"loadbal\" (proxysql-admin MODE). Percona Server replication backend: \"rwsplit\" or \"primary\". Locked once deployed."},
			{Field: "useProxy", Desc: "Download packages through the Intranet's Squid proxy."},
		},
		Relations: []string{"monitor", "repository"},
		Edges: []catalogEdge{
			{To: []string{"pxc", "mysql"}, Type: "directional", Required: true,
				Desc: "The backend cluster frame, stored from the backend frame to this frame (one incoming line)."},
			{To: []string{"airlinesim"}, Type: "directional",
				Desc: "Drive an Airline Sim workload through the ProxySQL cluster."},
		},
	},
}
