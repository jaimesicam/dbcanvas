package main

// designcatalog_apps.go — the catalogue's entries for the All-in-One node and the application simulators. See designcatalog.go.

// catalogAIOInstancesDesc documents the shape of an All-in-One node's aioInstances
// array, which is the node's real configuration: every option below the node level
// lives on one of these objects (aioInstance in aio.go).
const catalogAIOInstancesDesc = "The feature instances the container runs: an array of objects, one per instance. " +
	"Keys: " +
	"id (string, required) — stable id unique within the node; proxies and orchestratorRef refer to instances by it. " +
	"kind (string, required) — one of ps, psrepl, innodb, pxc, mysqlce, mysqlcerepl, mysqlceinnodb, mariadb, mariadbrepl, mariadbgalera (MySQL family); pg, patroni, repmgr, spock (PostgreSQL); psmdb, psmrs, psmdbsharded (MongoDB); valkey, valkeycluster; proxysql; haproxy; orchestrator. " +
	"Only one MySQL flavour per node: Percona Server kinds (ps, psrepl, innodb), pxc, MySQL Community kinds (mysqlce*) and MariaDB kinds (mariadb*) cannot be mixed. " +
	"name (string, required) — lowercase letters, digits and dashes, unique within the node; becomes the instance's hostname, directory and systemd unit (the designer proposes <kind>NN, or <kind>-cluster-NN for a cluster kind). " +
	"members (integer) — cluster kinds only, 1 otherwise: psrepl, pxc, mysqlcerepl, mariadbrepl, patroni, repmgr 2–5 (default 3); spock 2–5 (default 2); innodb, mysqlceinnodb 3–9 odd (default 3); mariadbgalera 3–5 odd (default 3); psmrs 3–7 odd (default 3); psmdbsharded exactly 5 (mongos + 1 config server + 3 one-member shards) or 13 (mongos + 3-member config replica set + 3 three-member shards), default 5; valkeycluster 3–7 (default 3); proxysql 1–3 (default 1). Cluster members are named <name>-n1, <name>-n2, …. " +
	"rootPassword (string) — \"\" uses the stack's .env credentials. " +
	"gtid (boolean, default true) — ps only. " +
	"replMode (string) — psrepl: \"async\" (default) or \"semisync\"; innodb: \"groupreplication\" (default; \"innodbcluster\" is refused because MySQL Shell is not installed). " +
	"pgMajor, pgVersion (string) — PostgreSQL kinds: the major per instance (majors co-install) and the minor, \"\" for the newest; spock takes only the majors its patch set supports. " +
	"generateCert (boolean), certTtlValue (integer, default 365), certTtlUnit (\"minutes\" | \"hours\" | \"days\", default \"days\") — a TLS certificate from the Intranet CA; MySQL, PostgreSQL and MongoDB kinds only. " +
	"exportEnabled (boolean), exportHostPort (integer, 0 picks a free port) — publish the instance's client port (and, for orchestrator, haproxy and patroni, its web interface) to the host. " +
	"pmmNodeId (string) — id of a pmm node that monitors the instance; every kind except orchestrator (valkey, valkeycluster, proxysql and haproxy are accepted with a warning: not registered as a PMM service). " +
	"orchestratorRef (string) — \"<orchestrator node id>\" or \"inst:<orchestrator instance id>\" on this node; ps, psrepl, innodb, mysqlce, mysqlcerepl, mysqlceinnodb, mariadb, mariadbrepl, mariadbgalera only. " +
	"backendInstanceId (string, required for proxysql and haproxy) — id of the instance on this node the proxy fronts: a MySQL-family instance for proxysql, MySQL-family or PostgreSQL for haproxy. " +
	"alertEmail (string) — orchestrator: mailbox for failure alerts, a local part on the stack's mail domain or a full address such as dba@example.net; the designer sets \"admin\"; \"\" turns alerts off. " +
	"Accepted by the schema but refused by validation (not implemented on All-in-One): ldapAuth/ldapDirNodeId, enableVault/openbaoNodeId, seaweedfsNodeId/seaweedfsBucket, keycloakNodeId/enableOIDC; usePgBackRest, useBarman, enablePBM, psmdbSetup, useLdap and mode are read by no All-in-One provisioner. " +
	"Ports are assigned by the node (a private slot per instance, never the product default) and are not part of the design."

// catalogSimDashboard is the sentence every simulator summary ends with.
const catalogSimDashboard = " Its dashboard is published on an auto-assigned host port, reported as config.httpPort by GET /api/stacks/{id}/nodes/{nid}."

var catalogApps = []catalogEntry{
	{Type: "aio", Label: "All in One",
		Summary:   "One container running many database instances side by side (MySQL family, PostgreSQL, MongoDB, Valkey, ProxySQL, HAProxy, Orchestrator), each on its own port slot, directory and systemd unit, controlled with aioctl.",
		Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"at least one entry in aioInstances (otherwise it deploys as a bare host, with a warning)",
			"one MySQL flavour per node; one PostgreSQL package source per major",
		},
		Options: []catalogOption{
			{Field: "aioInstances", Default: []any{}, Desc: catalogAIOInstancesDesc},
			{Field: "os", Values: []string{"oraclelinux", "ubuntu"}, Default: "oraclelinux", Desc: "Operating system family of the container image."},
			{Field: "osVersion", Default: "9", Desc: "OS release of the image, e.g. \"9\" for Oracle Linux or \"24.04\" for Ubuntu; the installation's image catalogue lists what is built."},
			{Field: "useProxy", Default: false, Desc: "Download packages through the Intranet's Squid proxy."},
			{Field: "aioPsMajor", Values: []string{"8.0", "8.4"}, Default: "8.0", Desc: "Percona Server major for every ps, psrepl and innodb instance (one install per container).", When: "a Percona Server instance is declared"},
			{Field: "aioPsVersion", Desc: "Percona Server minor; \"\" is the newest available.", When: "a Percona Server instance is declared"},
			{Field: "aioPxcMajor", Default: "8.0", Desc: "Percona XtraDB Cluster major for every pxc instance.", When: "a pxc instance is declared"},
			{Field: "aioPxcVersion", Desc: "Percona XtraDB Cluster minor; \"\" is the newest available.", When: "a pxc instance is declared"},
			{Field: "aioMariadbMajor", Desc: "MariaDB major for every mariadb, mariadbrepl and mariadbgalera instance; \"\" is the newest the catalogue has.", When: "a MariaDB instance is declared"},
			{Field: "aioMariadbVersion", Desc: "MariaDB minor; \"\" is the newest available.", When: "a MariaDB instance is declared"},
			{Field: "aioMysqlceMajor", Desc: "MySQL Community major for every mysqlce, mysqlcerepl and mysqlceinnodb instance; \"\" is the newest the catalogue has.", When: "a MySQL Community instance is declared"},
			{Field: "aioMysqlceVersion", Desc: "MySQL Community minor; \"\" is the newest available.", When: "a MySQL Community instance is declared"},
			{Field: "aioPsmdbMajor", Default: "8.0", Desc: "Percona Server for MongoDB major for every psmdb, psmrs and psmdbsharded instance.", When: "a MongoDB instance is declared"},
			{Field: "aioPsmdbVersion", Desc: "Percona Server for MongoDB minor; \"\" is the newest available.", When: "a MongoDB instance is declared"},
			{Field: "aioValkeyMajor", Default: "9.1", Desc: "Valkey major for every valkey and valkeycluster instance.", When: "a Valkey instance is declared"},
			{Field: "aioValkeyVersion", Desc: "Valkey minor; \"\" is the newest available.", When: "a Valkey instance is declared"},
			{Field: "aioProxysqlMajor", Default: "2", Desc: "ProxySQL major for every proxysql instance.", When: "a proxysql instance is declared"},
			{Field: "aioProxysqlVersion", Desc: "ProxySQL minor; \"\" is the newest available.", When: "a proxysql instance is declared"},
			{Field: "aioOrchestratorVersion", Desc: "Percona Orchestrator version; \"\" is the newest available.", When: "an orchestrator instance is declared"},
			{Field: "cpus", Desc: "Container CPU limit (--cpus); 0 is unlimited. Not shown in the designer's All-in-One form."},
			{Field: "memoryGb", Desc: "Container memory limit in GiB (--memory); 0 is unlimited. Validation warns when the instances' estimated footprint exceeds it. Not shown in the designer's All-in-One form."},
			{Field: "deviceReadMbps", Desc: "Container disk read limit in MB/s (--device-read-bps); 0 is unlimited. Not shown in the designer's All-in-One form."},
			{Field: "deviceWriteMbps", Desc: "Container disk write limit in MB/s (--device-write-bps); 0 is unlimited. Not shown in the designer's All-in-One form."},
			{Field: "devicePath", Desc: "Host block device the disk limits apply to; \"\" detects it from Docker's data root.", When: "deviceReadMbps or deviceWriteMbps is set"},
		},
		Relations: []string{"repository"},
		Actions: []catalogAction{
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances", Desc: "Every running member inside the node: name, kind, family, group, role, ports, published host port, web interfaces and systemd state."},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/start", Desc: "Start a member, a whole cluster instance (its declared name, started in order), or every instance (\"all\"), through aioctl."},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/stop", Desc: "Stop a member, a whole cluster instance, or \"all\"."},
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/restart", Desc: "Restart a member, a whole cluster instance, or \"all\"."},
			{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/logs", Desc: "The last 200 lines of one member's log; {inst} is a member name such as ps01 or pxc-cluster-01-n2."},
		},
	},

	{Type: "supportsim", Label: "Support Sim",
		Summary:   "MongoDB Vector Search support desk: tickets are embedded in the node (all-MiniLM-L6-v2, 384 dimensions) and answered, routed and merged with $vectorSearch, scored against $search; includes a Search Showdown and a Vector Lab." + catalogSimDashboard,
		Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"an association line to a PS MongoDB replica-set or sharded frame (vectorSearch on), or a K3D frame running the MongoDB operator (k3dOperator \"psmdb\", k3dVectorSearch on); without mongot the desk falls back to an in-app scan",
		},
		Options: []catalogOption{},
		Edges: []catalogEdge{
			{To: []string{"psm", "psmrs", "psmdb", "k3d"}, Type: "directional", Required: true,
				Desc: "The MongoDB the desk runs on. A standalone psm node works but has no mongot, so $vectorSearch is replaced by an in-app scan; a k3d frame must run the psmdb operator."},
		},
	},

	{Type: "pgvectorsim", Label: "pgvector Support Sim",
		Summary:   "The Support Sim's help desk on PostgreSQL: embeddings in a pgvector column searched through an HNSW index with <=>, alongside keyword search. Creates its own supportsim database." + catalogSimDashboard,
		Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"an association line to PostgreSQL: a standalone pg node, a Patroni, repmgr or Spock frame, an HAProxy or PgBouncer node fronting exactly one of those, or a K3D frame running a PostgreSQL operator (pg, cnpg or pgo); without pgVector on the backend it stores real[] and scans in the app",
		},
		Options: []catalogOption{},
		Edges: []catalogEdge{
			{To: []string{"pg", "patroni", "repmgr", "spock", "haproxy", "pgbouncer", "k3d"}, Type: "directional", Required: true,
				Desc: "The PostgreSQL the desk runs on. A router must front one PostgreSQL backend; pgVector on that backend (k3dPgVector with the Percona operator on a k3d frame) enables the HNSW index."},
		},
	},

	{Type: "stocksim", Label: "Stock Market Sim",
		Summary: "A stock-exchange application on MySQL, PostgreSQL, MongoDB or Valkey: agents move prices, place orders and settle trades, with browser CRUD, a printable report (/report), CSV exports and optional lab problems that load the target." + catalogSimDashboard +
			" Before deleting the node, its data can be dropped through the application's own POST /api/control/drop with {\"confirm\": \"<database>\"} on that port.",
		Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"ssMode \"linked\": an association line to a database node, cluster frame, K3D frame with a database operator, or a router fronting exactly one cluster",
			"ssMode \"aio\": ssAIONode and ssAIOInstance naming a MySQL-family, PostgreSQL or MongoDB instance",
			"ssMode \"manual\": ssHost or ssDSN",
		},
		Options: []catalogOption{
			{Field: "ssMode", Values: []string{"linked", "aio", "manual"}, Default: "linked",
				Desc: "Where the database is: linked (the association line), aio (an instance on an All-in-One node, chosen with the app relation and ssAIOInstance), or manual (the connection fields below, which may point outside the stack). \"\" means linked."},
			{Field: "ssAIOInstance", Desc: "Declared instance name on the All-in-One node (e.g. ps01, pxc-cluster-01); a cluster resolves to its write endpoint. MySQL-family, PostgreSQL and MongoDB kinds only.", When: "ssMode is \"aio\""},
			{Field: "ssEngine", Values: []string{"mysql", "postgres", "mongodb", "valkey"}, Default: "mysql", Desc: "Database engine of the manual connection. In linked and aio modes the engine comes from the target.", When: "ssMode is \"manual\""},
			{Field: "ssHost", Desc: "Host name or address, e.g. db.example.com; host.docker.internal reaches the Docker host.", When: "ssMode is \"manual\""},
			{Field: "ssPort", Desc: "Port; 0 uses the engine default (3306, 5432, 27017, 6379).", When: "ssMode is \"manual\""},
			{Field: "ssUser", Desc: "User name.", When: "ssMode is \"manual\""},
			{Field: "ssPassword", Desc: "Password; stored in the deployment's secrets.", When: "ssMode is \"manual\""},
			{Field: "ssDatabase", Default: "stocksim", Desc: "The application's own database, schema or key prefix; \"\" is stocksim. System databases are refused.", When: "ssMode is \"manual\""},
			{Field: "ssTLS", Values: []string{"prefer", "require", "disable"}, Default: "prefer", Desc: "TLS mode of the manual connection.", When: "ssMode is \"manual\""},
			{Field: "ssLabel", Desc: "Display name shown on the dashboard and report.", When: "ssMode is \"manual\""},
			{Field: "ssParams", Desc: "Extra driver parameters appended verbatim, e.g. readTimeout=30s.", When: "ssMode is \"manual\""},
			{Field: "ssDSN", Desc: "Full connection string handed to the driver verbatim; overrides every other connection field.", When: "ssMode is \"manual\""},
			{Field: "ssTargetSize", Desc: "How large the application grows its dataset at the High load level: \"5Gi\", \"512Mi\", a byte count; \"\" is 5Gi, \"0\" or \"off\" disables growth.", When: "the engine is not valkey"},
			{Field: "ssWorkingSet", Default: "", Desc: "How much of the dataset is kept under continuous random read: \"50%\", \"0.5\", \"2G\" or \"off\"; \"\" is 50%.", When: "the engine is not valkey"},
			{Field: "ssThreads", Default: 0, Desc: "Concurrent database workers per heavy agent, and the connection-pool size, 1–64; 0 is 4."},
			{Field: "ssSplitReads", Default: false, Desc: "Send display-only reads to HAProxy's read port (5001); everything else stays on 5000.", When: "ssMode is \"linked\" and the target is an haproxy node"},
			{Field: "ssIdleTxn", Default: "", Desc: "Lab: hold a transaction open with a read snapshot for this long (e.g. 30m, 2h; at most 24h) so purge cannot advance; \"\" is off.", When: "the engine is mysql or postgres"},
			{Field: "ssExtraTables", Default: 0, Desc: "Lab: create this many small tables (or collections) and read them in rotation, 0–5000; 0 is off.", When: "the engine is mysql, postgres or mongodb"},
			{Field: "ssTempTables", Values: []string{"off", "memory", "disk"}, Default: "off", Desc: "Lab: run a rollup that builds a large intermediate result, in memory or forced to spill to disk.", When: "the engine is mysql or postgres"},
			{Field: "ssLockContention", Values: []string{"off", "light", "heavy"}, Default: "off", Desc: "Lab: concurrent writers on a few rows; light makes them queue (row lock waits), heavy takes rows in opposite orders (real deadlocks).", When: "the engine is mysql or postgres"},
			{Field: "ssScanQueries", Default: 0, Desc: "Lab: reads per minute on the tick history with a predicate no index serves, 0–120; 0 is off.", When: "the engine is mysql, postgres or mongodb"},
			{Field: "ssWritePressure", Values: []string{"off", "commits", "redo"}, Default: "off", Desc: "Lab: commits runs many tiny transactions (fsync cost); redo rewrites wide rows in bulk (write-ahead log and checkpoint cost).", When: "the engine is mysql or postgres"},
		},
		Relations: []string{"app"},
		Edges: []catalogEdge{
			{To: []string{
				"ps", "mariadb", "mysqlce", "pg", "psm", "valkey",
				"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb",
				"patroni", "repmgr", "spock", "k3d", "psmrs", "psmdb", "valkeycluster",
				"haproxy", "proxysql", "pgbouncer",
			}, Type: "directional",
				Desc: "The database it drives when ssMode is \"linked\" (required then): a standalone database node, a cluster frame (its write endpoint), a k3d frame running pxc, ps, psmdb, pg, cnpg or pgo, or a router fronting exactly one cluster. Members inside a frame are not targets."},
		},
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stocksim/test", Desc: "Test a manual connection from inside the stack network; works before the node is deployed.",
				Body: "engine, host, port, user, password, database, tls, params, dsn", When: "ssMode is \"manual\""},
		},
	},

	{Type: "ledgersim", Label: "Ledger Sim",
		Summary:   "An order-and-payment ledger driven over JDBC with a HikariCP pool, on MySQL-family or PostgreSQL databases. The driver, URL properties and pool can be changed on its dashboard while it runs; these options set where it starts." + catalogSimDashboard,
		Placement: "standalone",
		Requires: []string{
			"an Intranet node",
			"lsMode \"linked\": an association line to a MySQL-family or PostgreSQL node, cluster frame, K3D frame with a MySQL or PostgreSQL operator, or a router fronting exactly one cluster",
			"lsMode \"manual\": lsHost or lsJdbcUrl",
		},
		Options: []catalogOption{
			{Field: "lsMode", Values: []string{"linked", "manual"}, Default: "linked", Desc: "Where the database is: linked (the association line) or manual (the connection fields, which may point outside the stack). \"\" means linked."},
			{Field: "lsEngine", Values: []string{"mysql", "postgres"}, Default: "mysql", Desc: "Engine of the manual connection; in linked mode it comes from the target.", When: "lsMode is \"manual\""},
			{Field: "lsHost", Desc: "Host name or address, e.g. db.example.net.", When: "lsMode is \"manual\""},
			{Field: "lsPort", Desc: "Port; 0 uses the engine default (3306, 5432).", When: "lsMode is \"manual\""},
			{Field: "lsUser", Desc: "User name.", When: "lsMode is \"manual\""},
			{Field: "lsPassword", Desc: "Password; stored in the deployment's secrets.", When: "lsMode is \"manual\""},
			{Field: "lsLabel", Desc: "Display name for the dashboard header. Not shown in the designer form.", When: "lsMode is \"manual\""},
			{Field: "lsDriver", Values: []string{"mysql-connector-j", "mariadb-connector-j", "pgjdbc"}, Default: "mysql-connector-j",
				Desc: "JDBC driver: mysql-connector-j (GPL-2.0 with the Universal FOSS Exception) or mariadb-connector-j (LGPL-2.1) for MySQL-family targets, pgjdbc (BSD-2-Clause) for PostgreSQL. A driver that cannot speak the engine is replaced, with a warning."},
			{Field: "lsDatabase", Default: "ledgersim", Desc: "The application's database; \"\" is ledgersim. System databases are refused."},
			{Field: "lsTLS", Values: []string{"disable", "prefer", "require"}, Default: "prefer", Desc: "TLS mode, mapped to each driver's own properties."},
			{Field: "lsParams", Desc: "Extra driver properties appended verbatim, e.g. socketTimeout=30000&tcpKeepAlive=true."},
			{Field: "lsJdbcUrl", Desc: "Full JDBC URL handed to the driver verbatim; overrides the other connection fields, in either mode (a linked node can be aimed at one member this way)."},
			{Field: "lsPoolMode", Values: []string{"pooled", "direct"}, Default: "pooled", Desc: "pooled uses HikariCP; direct opens and closes a connection per transaction."},
			{Field: "lsPoolMax", Default: 0, Desc: "HikariCP maximumPoolSize, 1–500; 0 is 10.", When: "lsPoolMode is \"pooled\""},
			{Field: "lsThreads", Default: 0, Desc: "Worker threads, 1–256; 0 is 8."},
			{Field: "lsIsolation", Values: []string{"", "TRANSACTION_READ_UNCOMMITTED", "TRANSACTION_READ_COMMITTED", "TRANSACTION_REPEATABLE_READ", "TRANSACTION_SERIALIZABLE"}, Default: "",
				Desc: "Transaction isolation level; \"\" is the driver default (REPEATABLE READ on MySQL, READ COMMITTED on PostgreSQL)."},
			{Field: "lsRevenueShards", Default: 8, Desc: "Revenue accounts every order credits, 1–8; 1 serialises every transaction on one row."},
			{Field: "lsDeadlockShare", Default: 0, Desc: "Share of transactions (0–1) that take their two row locks in the opposite order, producing real deadlocks."},
			{Field: "lsHotShare", Default: 0, Desc: "Share of traffic (0–1) aimed at a handful of hot accounts. Not shown in the designer form."},
			{Field: "lsCustomers", Default: 0, Desc: "Customer rows seeded, up to 5000000; 0 is 2000. Not shown in the designer form."},
			{Field: "lsStartPaused", Default: false, Desc: "Deploy without starting the workload; start it from the dashboard."},
		},
		Edges: []catalogEdge{
			{To: []string{
				"ps", "mariadb", "mysqlce", "pg",
				"pxc", "mysql", "innodb", "mariadbrepl", "mariadbgalera", "mysqlcerepl", "mysqlceinnodb",
				"patroni", "repmgr", "spock", "k3d",
				"haproxy", "proxysql", "pgbouncer",
			}, Type: "directional",
				Desc: "The database it drives when lsMode is \"linked\" (required then). A k3d frame must run a MySQL or PostgreSQL operator (pxc, ps, pg, cnpg or pgo); MongoDB and Valkey targets are refused."},
		},
		Actions: []catalogAction{
			{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ledgersim/test", Desc: "Test a manual JDBC connection with the chosen driver from inside the stack network; works before the node is deployed.",
				Body: "engine, driver, host, port, user, password, database, tls, params, jdbcUrl", When: "lsMode is \"manual\""},
		},
	},
}
