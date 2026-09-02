// help.js — the tooltip text behind the "?" beside a control.
//
// Why a catalog rather than a string at each site: the designer asks the same handful
// of questions on nearly every node. "OS version" appears 17 times, "Monitored by
// (PMM)" 17, "Use Intranet proxy (Squid) for downloads" 19. Written inline they drift —
// three of them end up saying subtly different things about the same switch, and the
// fourth never gets written at all. Written once here, every node form explains a
// shared concept the same way, and improving the wording improves it everywhere.
//
// What belongs in one of these. Not a restatement of the label — the reader can see the
// label. The tooltip answers the questions a label cannot: what the setting is for, what
// happens if it is left alone, and when somebody would want to change it. The
// always-visible `hint` under an input stays the place for the one line needed every
// time ("0 / empty = random unused port"); this is the paragraph behind it.

// product() keeps the version pair readable for the dozen engines that share it.
const major = (p) => `The ${p} release line, which fixes the package repository the node installs from. ` +
  `Locked once deployed — an in-place major upgrade is not something DBCanvas does, so build a new node instead.`
const minor = (p) => `Which ${p} build to install. Leave it on "latest" and the node takes the newest version this ` +
  `installation knows about; pin one to reproduce a customer's exact build, or to stand two nodes side by side on ` +
  `different patch levels. The list comes from \`make versions\`, so it reflects what was actually published when ` +
  `this host last probed the repositories.`

export const HELP = {
  // --- identity ------------------------------------------------------------
  label:
    'The name on the canvas — and the node\'s hostname on the stack network, so every other node can reach it at ' +
    'this name. Must be unique in the stack. Pick something you will recognise in a terminal prompt.',
  nodeName:
    'The node\'s hostname on the stack network; other nodes reach it at this name. Assigned from the cluster name ' +
    'and the member number, and not editable, because the provisioners address members by a predictable name.',
  clusterName:
    'Names the group, and prefixes every member\'s hostname. Must be unique across the stack — two clusters sharing ' +
    'a name would give two nodes the same DNS name.',
  clusterReadonly:
    'The cluster this node belongs to. Set by the frame it sits in on the canvas; drag the node out of the frame to ' +
    'change it.',
  role:
    'Which end of replication this member is. Exactly one node is the primary and takes writes; the rest are ' +
    'read-only and follow it. Promoting a different node here demotes the current primary automatically.',

  // --- platform ------------------------------------------------------------
  os:
    'The Linux distribution the node runs, which decides its package manager and which Percona repository the ' +
    'install pulls from. Match it to the platform you are trying to reproduce — the same database version behaves ' +
    'differently on a distro with a different glibc or systemd.',
  osVersion:
    'The distribution release. Locked once the node is deployed: the packages already installed came from this ' +
    'release\'s repository.',
  major,
  minor,
  vmCPUs:
    'A ceiling on how much CPU this node may use, not a reservation — it stays idle until the node asks for it. ' +
    'Leave it at the default unless you are deliberately starving a node to watch what that does.',
  vmMemory:
    'A hard memory limit. The node is killed if it exceeds this, so give a database enough room for its buffer pool ' +
    'plus overhead; a PXC or PostgreSQL node squeezed too tightly will be OOM-killed mid-benchmark rather than ' +
    'slowing down gracefully.',
  proxy:
    'Route this node\'s package downloads through the Intranet node\'s Squid proxy instead of straight out to the ' +
    'internet. Worth turning on when you are deploying many nodes of the same OS: the second and later nodes take ' +
    'their packages from the proxy\'s cache, which is dramatically faster and spares the upstream mirrors.',
  blockDevice:
    'Put the data directory on a real block device on the host instead of the container filesystem. This is how you ' +
    'reproduce an I/O problem — a slow disk, a full filesystem, a particular scheduler — that a shared overlay ' +
    'filesystem would hide. The device is used as-is and is not formatted for you.',

  // --- exposure ------------------------------------------------------------
  exportPort:
    'Publish this node\'s port on the machine running DBCanvas, so a client outside the stack — your local ' +
    'mysql/psql/mongosh, a GUI, an application under test — can connect to it. Leave it off and the node is still ' +
    'fully reachable from every other node in the stack, just not from outside.',
  hostPort:
    'Which port on the host to publish on. Leave it 0 and Docker picks a free one, which is what you want unless ' +
    'something outside DBCanvas is hard-coded to a number — a saved connection, a config file, a script. A fixed ' +
    'port that is already taken fails the deploy, and it is also the one setting that stops the same design being ' +
    'deployed twice on one host.',
  sshTunnel:
    'Published ports are bound to the machine DBCanvas runs on. If that is a remote server, right-click the node ' +
    'and copy the SSH tunnel command to bring these ports to your own machine.',

  // --- security ------------------------------------------------------------
  generateCert:
    'Mint a TLS certificate for this node from the Intranet node\'s certificate authority and configure the ' +
    'database to use it, so traffic in the stack is encrypted and clients can actually verify who they connected ' +
    'to. The CA is trusted by every node in the stack and by the VNC desktop\'s browser, so nothing has to be ' +
    'told to skip verification. Needs an Intranet node.',
  certTTL:
    'How long the issued certificate stays valid. The default is a year; set it to minutes or hours when the point ' +
    'of the exercise is to watch what breaks the moment a certificate expires.',
  ldap:
    'Authenticate database users against the Intranet node\'s OpenLDAP directory instead of local accounts, so one ' +
    'directory identity works across the stack. Needs an Intranet node.',
  oidc:
    'Single sign-on through the Keycloak node: users log in against Keycloak and the database trusts the token it ' +
    'issues, instead of holding its own password for them. Needs a deployed Keycloak node.',
  kerberos:
    'GSSAPI single sign-on against the Samba AD DC node — the client presents a Kerberos ticket rather than a ' +
    'password. Needs a deployed Samba AD DC node and a service principal for this host.',
  vault:
    'Keep the data-at-rest encryption keys in the OpenBao node rather than in a file beside the data. This is the ' +
    'shape a real keyring deployment takes, and it lets you demonstrate what happens to a running database when the ' +
    'key server goes away.',

  // --- monitoring & topology ----------------------------------------------
  pmm:
    'Register this node with a PMM server so it shows up in Percona Monitoring and Management with metrics and ' +
    'Query Analytics. The monitoring user is created on the node for you and the agent is configured at deploy. ' +
    'Add a PMM node to the canvas first; leave it on "none" if you do not need the dashboards.',
  orchestrator:
    'Point an Orchestrator node at this cluster so it discovers the replication topology and can drive failovers ' +
    'from its own web UI. Optional — replication works the same without it.',
  replMode:
    'Asynchronous commits on the primary without waiting for anyone, which is fast and can lose the last few ' +
    'transactions if the primary dies. Semi-synchronous makes the primary wait for at least one replica to ' +
    'acknowledge each commit — slower, and the usual answer when the demo is about durability.',
  gtid:
    'Global Transaction IDs give every transaction a cluster-unique name, which is what lets a replica be ' +
    'repointed at a new primary with AUTO_POSITION instead of a hand-calculated binlog file and offset. Leave it ' +
    'on unless you are specifically demonstrating the old way.',

  // --- storage & backup ----------------------------------------------------
  seaweedfsBackup:
    'Send backups to the SeaweedFS node\'s S3 endpoint, which stands in for S3 without leaving the stack. Add a ' +
    'SeaweedFS node to the canvas to make this available.',
  storageGiB:
    'The size of the volume claimed for each instance. It is a request against the cluster\'s local storage, so ' +
    'oversizing it on a laptop leaves pods Pending rather than failing loudly.',
  instances:
    'How many database instances the operator runs. One is a single point of failure and is fine for a demo; three ' +
    'is the smallest number that survives losing one and still has a quorum.',

  // --- lifecycle -----------------------------------------------------------
  lockedWhenDeployed:
    'Locked because the node is deployed — this value shaped how it was built. Destroy the node (or the stack) and ' +
    'redeploy to change it.',
  deployState:
    'Where this node is in its lifecycle. "running" means the container is up and provisioning finished; "error" ' +
    'means provisioning stopped — open the deployment console for the step that failed.',

  // --- deployed-node panel -------------------------------------------------
  depImage:
    'The exact image this node was built from. Worth quoting in a bug report — "8.0" is a range, this is one build.',
  depContainer:
    'The container name on the Docker host. Use it with docker logs / docker inspect from your own shell; the ' +
    'right-click menu will copy a ready-made docker exec line.',
  depHostname:
    'The node\'s DNS name inside the stack. This is the host every other node connects to it by, and the name to ' +
    'use in a connection string typed on another node or on the VNC desktop.',
  depAddress:
    'Where to reach this node from the machine running DBCanvas. On a remote install, right-click the node and copy ' +
    'the SSH tunnel command first — these ports are bound to the server, not to your laptop.',
  depPassword:
    'Generated at deploy, or taken from .env. Click to reveal, and again to copy. Treat every credential in a ' +
    'DBCanvas stack as disposable: these are lab databases, not somewhere to keep anything real.',
  depUser:
    'The account to connect as. Its password is on this panel; the root/superuser account generally cannot connect ' +
    'over the network, which is why this one exists.',
  // --- Kubernetes (K3D frames) --------------------------------------------
  k8sExpose:
    'How this service is reachable from outside the Kubernetes cluster. ClusterIP keeps it in-cluster, which is ' +
    'the operator default and fine when you only ever connect from a pod or from the k3s node itself. LoadBalancer ' +
    'asks k3s\'s built-in load balancer for an address you can dial from the host, which is what you want to point ' +
    'a local client or a GUI at. NodePort is the middle ground when a LoadBalancer address is not available.',
  k8sOperator:
    'Which operator manages the cluster. This is the whole point of a K3D frame — the databases are created as ' +
    'custom resources and reconciled by the operator, exactly as they would be in a real Kubernetes deployment, ' +
    'rather than being provisioned by DBCanvas.',
  k8sChartVersion:
    'The Helm chart release to install the operator from. Leave it at the default unless you are reproducing ' +
    'behaviour specific to an older operator.',
  k8sNamespace:
    'The Kubernetes namespace the database resources are created in. Handy to change when you want two clusters ' +
    'from the same operator side by side.',
  k8sPoolMode:
    'How aggressively pgBouncer reuses backend connections. Session pooling hands a client one backend for the ' +
    'life of its connection and is always safe. Transaction pooling returns the backend between transactions — far ' +
    'better reuse, at the cost of anything that relies on session state (temporary tables, prepared statements, ' +
    'advisory locks, SET).',
  k8sPgBouncerPods:
    'How many pooler pods to run. More than one gives the pooler itself redundancy; it does not increase how many ' +
    'backends PostgreSQL will accept.',
  k8sTopology:
    'A replica set is three mongod pods and is enough for high availability. Sharding adds three config servers ' +
    'and three mongos routers on top — the shape you need to demonstrate a shard key, chunk balancing, or a ' +
    'scatter-gather query, and roughly twice the resources.',

  // --- resource limits & fault injection ----------------------------------
  cpuLimit:
    'A ceiling on CPU, not a reservation. Blank means unlimited. Set it to make a node CPU-bound on purpose — the ' +
    'quickest way to show what a saturated database server looks like in PMM.',
  memLimit:
    'A hard memory limit. Blank means unlimited. A database that crosses it is OOM-killed rather than slowed, so ' +
    'leave headroom above the buffer pool or shared_buffers you configured.',
  diskBps:
    'Throttle the node\'s disk throughput to this many MB/s. Blank means unlimited. This is how you reproduce a ' +
    'slow-storage incident — checkpoint stalls, replica lag, a backup that never finishes — on a host with a fast SSD.',
  netLatency:
    'Adds delay to every packet leaving this node, applied with tc/netem. The standard way to make a local stack ' +
    'behave like a cross-region one: 80 ms turns a chatty query pattern or a synchronous commit into something you ' +
    'can actually see.',
  netJitter:
    'Randomly varies the added latency by up to this much either way. Real networks are not uniformly slow, and ' +
    'jitter is what breaks naive timeouts and heartbeat tuning.',
  netLoss:
    'Drops this percentage of outgoing packets. A percent or two is enough to expose a cluster that reacts badly ' +
    'to retransmits; higher values will start evicting the node from its cluster, which may be the point.',
  netBandwidth:
    'Caps outgoing bandwidth. Use it to squeeze state transfers, backups and replication catch-up into a link that ' +
    'looks like the customer\'s.',

  // --- benchmark / load generator -----------------------------------------
  benchTarget:
    'Which deployed node this run drives load against. Pick the node whose behaviour you want to measure — for a ' +
    'cluster, that usually means the proxy or the primary rather than a single member.',
  benchEngine:
    'The database dialect to speak. Set automatically when you pick a node from the stack; choose it by hand only ' +
    'when pointing at something outside DBCanvas.',
  benchHost: 'Where to connect. Prefilled from the node you picked — override it to benchmark something outside this stack.',
  benchPort: 'The port to connect on. Prefilled from the node\'s exported port when it has one.',
  benchUser: 'The account the benchmark connects as. It needs enough privileges to create and populate its own tables.',
  benchPassword: 'Password for that account. Prefilled from the deployed node\'s generated credentials where DBCanvas knows them.',
  benchDatabase: 'The schema the benchmark creates its tables in. It is written to, so point it somewhere disposable.',
  benchTLS: 'Whether to require an encrypted connection. Turn it on to measure what TLS actually costs on this workload.',
  benchDriverParams: 'Extra driver options appended to the connection string, for anything the fields above do not cover.',
  benchDSN: 'The assembled connection string, shown so you can check it or paste it into another client. Editing it overrides the fields above.',
  benchDataset:
    'How much data to generate before measuring. The number that matters is whether it fits in memory: a dataset ' +
    'smaller than the buffer pool measures CPU, one comfortably larger measures storage. Fixed at deploy.',
  benchWorkingSet:
    'How much of the dataset the queries actually touch. A small working set over a large dataset is the realistic ' +
    'case, and the one where cache-hit ratios in PMM start meaning something.',
  benchThreads:
    'How many concurrent connections drive load. Raise it past the number of cores to find the point where the ' +
    'server stops going faster and starts queueing — that knee is usually what the benchmark is for.',
  benchIdleTxn:
    'Hold a transaction open without committing. Reproduces the classic production incident: a forgotten ' +
    'transaction pinning the undo log / preventing vacuum, and everything that follows from it.',
  benchExtraTables: 'Create additional tables so the schema is not a single hot object — closer to a real application, and to a real information_schema.',
  benchTempTables: 'Issue queries that force temporary tables, so you can watch them spill to disk and see the cost in the query plan.',
  benchLockContention: 'Deliberately make transactions fight over the same rows. Produces lock waits, deadlocks and the lock graphs that go with them.',
  benchScanQueries: 'How many full-scan queries to issue per minute, for the deliberately unindexed access pattern that makes a slow query log interesting.',
  benchWritePressure: 'How hard to push writes alongside the read workload — the mix that produces replica lag, checkpoint storms and purge lag.',
  benchDisplayName: 'A name for this run in the results list. Worth setting when you are about to compare several configurations.',

  // --- core-dump analysis --------------------------------------------------
  gdbCoreDir:
    'A directory on the Docker host holding the core file. It is bind-mounted read-only into the analyzer. Must sit ' +
    'under this installation\'s GDB_MOUNT_ROOT — that confinement is the only thing standing between a typed path ' +
    'and the Docker daemon.',
  gdbLibDir:
    'A directory on the Docker host holding the crashed server\'s mysqld binary and every library it was linked ' +
    'against (its ldd closure). Mounted read-only as the sysroot, so gdb resolves symbols against the libraries the ' +
    'process actually ran with rather than this container\'s.',
  gdbProduct:
    'Which product crashed. Together with the version below this selects the debug symbols to install, and getting ' +
    'it wrong is why a backtrace comes back as hex addresses.',
  gdbVersion:
    'The exact build that crashed. Debug symbols must match the binary precisely — a neighbouring patch release ' +
    'will load and then lie to you about line numbers.',

  // --- storage node --------------------------------------------------------
  s3AccessKey: 'The S3 access key id clients use against this node. Anything that speaks S3 — pgBackRest, PBM, aws-cli, a browser — authenticates with this pair.',
  s3SecretKey: 'The S3 secret. Left empty it is generated at deploy and shown on the node\'s Access tab afterwards.',
  s3Buckets: 'Buckets to create at deploy. Give each consumer its own — a backup tool that shares a bucket with another will happily list, and sometimes expire, its objects.',
  s3Bucket: 'Which bucket on that node this cluster writes to.',

  // --- desktop -------------------------------------------------------------
  vncUser: 'The Linux account you log into the desktop as. It has passwordless sudo, so you can install whatever else the exercise needs.',
  vncPassword: 'Used both as the desktop login and as the VNC access code. VNC authentication only reads the first 8 characters, so anything after that protects the desktop login only.',

  // --- templates & stack ---------------------------------------------------
  tplStartFrom: 'Seed the canvas with a ready-made topology instead of starting empty. Everything stays editable afterwards — the template is a starting point, not a constraint.',
  tplName: 'What this template is called in the picker. Name it after the topology, not the stack you happened to save it from.',
  tplDescription: 'A line for whoever picks this template later — what it builds and what it is for.',
  tplCategory: 'Groups the template in the picker, alongside the built-ins (MySQL, PostgreSQL, MongoDB, …).',
  tplPick: 'The template to merge into the canvas. Ids are rewritten, singleton nodes you already have are reused rather than duplicated, and colliding hostnames are numbered.',
  stackTTL:
    'When this stack tears itself down. Lab stacks are easy to forget and expensive to leave running; pick the ' +
    'shortest lifetime that covers what you are doing, and extend it later if you need to.',

  // --- misc ----------------------------------------------------------------
  oidcRealm: 'The Keycloak realm holding the OIDC client this node authenticates against.',
  oidcClientId: 'The OIDC client id, which is also the audience the issued token must carry for this node to accept it.',
  oidcClaim: 'Which token claim carries the user\'s groups, so group membership in Keycloak can be mapped to roles here.',
  proxysqlMode:
    'How ProxySQL decides where a query goes. Read/write splitting sends writes to the primary and reads to the ' +
    'replicas, which is the interesting configuration; a single-host mode is the one to pick when you want ' +
    'ProxySQL in the path without it making routing decisions.',
  debugPort:
    'The host port the Delve debugger listens on, so an IDE on your machine can attach to the operator running in ' +
    'the cluster. Point a Go remote-debug configuration at it.',
  alertEmail:
    'Where failure-detection alerts are mailed. A bare name is treated as a mailbox on the stack\'s Intranet ' +
    'domain, so it lands in the Intranet node\'s webmail; clear it to turn alerts off.',
  // --- the last few one-offs ------------------------------------------------
  ldapDirectory:
    'Which directory node authenticates this database\'s users — the Intranet node\'s OpenLDAP, or the Samba AD DC ' +
    'if the stack has one. They behave differently on purpose: OpenLDAP is a plain LDAP bind, Samba is real Active ' +
    'Directory.',
  nodeType:
    'What this node is. Fixed when the node was added to the canvas — delete it and add the right kind to change it.',
  k3sVersion:
    'The k3s build the Kubernetes nodes run. Pin it when an operator you are testing only supports certain ' +
    'Kubernetes versions; otherwise take the default.',
  k8sPxcProxy:
    'The front end in front of the PXC pods. HAProxy load-balances connections and is the operator default; ' +
    'ProxySQL adds query-aware routing and its own admin interface. The cr.yaml runs one or the other, never both.',
  k8sPsReplication:
    'How the MySQL pods replicate. Group Replication is self-managing and needs only the three database pods; ' +
    'async replication is driven by Orchestrator, which adds three more pods but is the topology most existing ' +
    'deployments actually run.',
  k8sPsProxy:
    'The front end for the MySQL pods. MySQL Router understands Group Replication and routes read/write splits ' +
    'from its metadata; HAProxy works with either replication mode.',
  pdpsRepo:
    'The Percona Distribution for MySQL repository the nodes install from, which is what fixes the Percona Server ' +
    'version. Pick the distribution release you are reproducing.',
  psmdbSetup:
    'How big the sharded cluster is. Standard is the production shape — three shards, each a three-node replica ' +
    'set, plus a three-node config replica set — and needs the resources to match. Minimum is the smallest thing ' +
    'that is still genuinely sharded, for when you only need mongos and a shard key to behave correctly.',
  orchestratorVersion:
    'Which Percona Orchestrator build to install. Leave it at the latest unless you are matching a deployment ' +
    'already in the field.',
  simTraders: 'How many trader accounts the generated dataset contains — the cardinality of the dimension most queries join against.',
  simOrders: 'How many orders to generate. This is the big table, and the one that decides whether the dataset fits in memory.',
  simTrades: 'How many executed trades to generate — the fact table the reporting queries aggregate over.',
  simTicks: 'How many price ticks to generate. The time-series table, and the one that makes range scans expensive.',
  // --- deployed-node panel rows --------------------------------------------
  depInternalURL:
    'The address to use from inside the stack — another node\'s shell, or the browser on the VNC desktop, which ' +
    'resolves these names and trusts the Intranet CA. It will not resolve from your own machine.',
  depLinkedTo:
    'The node this one was wired to at deploy: where it sends its data, or which cluster it fronts. Change it by ' +
    'editing the link on the canvas and redeploying.',
  depHost:
    'The node\'s DNS name inside the stack — what every other node connects to it by, and the host to put in a ' +
    'connection string typed on another node.',
  depExportedPort:
    'The port this node is published on, on the machine running DBCanvas. Connect a local client here. If DBCanvas ' +
    'runs on a remote server, right-click the node and copy the SSH tunnel command first.',
  depTLS:
    'Whether the node is serving TLS, and who signed the certificate. "Intranet CA" means every node in the stack ' +
    'and the VNC desktop\'s browser already trust it — no --insecure needed.',
  depConsole:
    'The web console for this service. Open it from the VNC desktop unless the address is a host one, in which ' +
    'case your own browser will do.',
  depAPIToken:
    'A bearer token for this node\'s API. Click to reveal and copy. Disposable, like everything else in a lab stack.',
  depMonitoredBy:
    'The PMM server collecting this node\'s metrics. Open that node\'s panel for the PMM address and login.',
  depBaseImage:
    'The image the node was built from before DBCanvas provisioned it. The database itself was installed on top by ' +
    'the provisioner, so this names the OS, not the database version.',
  depVersion:
    'The version actually installed, read back from the running node rather than from the design — this is the one ' +
    'to quote in a bug report.',
  depDataset:
    'How much data the load generator seeded. Fixed at deploy: reseeding at a different size means deleting the ' +
    'node and redeploying it.',
  depThreads:
    'How many concurrent connections the generator is driving right now. Change it to move the offered load up and ' +
    'down while watching the effect in PMM.',
  depCoreDumps:
    'The host directory holding the core file, mounted read-only inside the analyzer at /coredumps.',
  depLibraries:
    'The host directory holding the crashed server\'s binary and libraries, mounted read-only as the sysroot so gdb ' +
    'resolves symbols against the right build.',
  depDebugging:
    'Where the debugger is listening. Point your IDE\'s Go remote-debug configuration at this address to attach to ' +
    'the operator running in the cluster.',
  depAlertEmail:
    'Where this node mails its alerts. Read them in the Intranet node\'s webmail.',

  // --- designer chrome ------------------------------------------------------
  uiBack: 'Back to the stack list. The canvas saves itself as you work, so nothing is lost.',
  uiPalette: 'The Infrastructure Library — every node type you can put on the canvas, grouped by engine. Click one to add it.',
  uiInsertTemplate:
    'Merge a saved topology into this canvas. Ids are rewritten, a singleton you already have (the Intranet, ' +
    'Keycloak, the desktop) is reused rather than duplicated, and colliding hostnames are numbered — whatever had ' +
    'to change is reported when it lands.',
  uiSaveTemplate:
    'Save this canvas as a reusable template. Passwords, host paths and pinned host ports are deliberately left ' +
    'out, so the template travels to another stack, another user, or another installation.',
  uiValidate:
    'Check the design without deploying anything: missing links, impossible topologies, host ports that clash, ' +
    'paths outside the allowed root. Cheap, and worth doing before a long deploy.',
  uiDeploy:
    'Build the stack. Nodes are provisioned in dependency order and the console shows each step; a node that fails ' +
    'leaves the rest running so you can read its log.',
  uiDestroy:
    'Tear down every container and volume in this stack. The design stays on the canvas, so you can redeploy it — ' +
    'but the data inside the nodes is gone.',
  uiResetView: 'Recentre the canvas at 100%, for when you have panned or zoomed somewhere you cannot find your way back from.',
  uiSaveState: 'The canvas saves itself a moment after every change; this says whether the last one has landed.',
  uiTTL: 'When this stack tears itself down automatically. Set when the stack was created.',
  uiStackStatus: 'Whether this design has been deployed, and whether its containers are still up.',
  uiCounts: 'How many nodes and links are on the canvas — a quick check that a template inserted what you expected.',
  uiMinimap: 'The whole canvas at a glance. Click or drag inside it to jump the view.',
  uiDockPanel: 'Detach this panel into a floating window, or dock it back into the column. The choice is remembered.',
  uiAddMember: 'Add another node to this cluster. Available until the cluster is deployed.',
  uiRemoveMember: 'Remove the last node from this cluster.',
  uiNodeContext: 'Right-click any node for its root console, a file browser, start/stop, and the commands to reach it from your own shell.',
  uiDragToResize: 'Drag to resize this panel.',
}

// NODE_BLURB — what a palette entry actually gets you, for the tooltip on the button
// that adds it. NODE_TYPES already carries a `sub` (a product subtitle); this answers
// the question the subtitle does not: why would I put this on the canvas?
const NODE_BLURB = {
  intranet:
    'The service node every stack needs first: DNS for the stack\'s hostnames, a certificate authority the other ' +
    'nodes trust, OpenLDAP, mail, and a Squid proxy that caches package downloads. One per stack.',
  sambaad:
    'A real Active Directory domain controller — LDAP, Kerberos and DNS — for demonstrating AD-backed database ' +
    'authentication and GSSAPI single sign-on. One per stack.',
  pmm: 'A Percona Monitoring and Management server. Point database nodes at it and they register themselves, with metrics and Query Analytics.',
  watchtower: 'Watches for newer images of the nodes it manages and updates them, for demonstrating an upgrade rollout.',
  keycloak: 'An OIDC identity provider, so databases in the stack can do single sign-on instead of holding their own passwords.',
  openbao: 'A key-management server, so a database can keep its data-at-rest encryption keys somewhere other than beside the data.',
  aio: 'One container running MySQL, PostgreSQL, MongoDB and Valkey at once. The fastest way to get a client connected to something.',
  pxc: 'Percona XtraDB Cluster — synchronous multi-primary replication. The cluster to reach for when the topic is Galera, SST, or split brain.',
  ps: 'A standalone Percona Server for MySQL. The plain single-node MySQL to start from.',
  mysql: 'Percona Server with classic asynchronous replication: one primary, one or more replicas, GTID by default.',
  innodb: 'InnoDB Cluster — Group Replication with MySQL Router in front, MySQL\'s own built-in HA answer.',
  orchestrator: 'Percona Orchestrator: discovers a replication topology, visualises it, and drives failovers.',
  mysqlce: 'Upstream MySQL Community, for comparing behaviour against Percona Server.',
  mysqlcerepl: 'Upstream MySQL Community with asynchronous replication.',
  mysqlceinnodb: 'Upstream MySQL Community as an InnoDB Cluster (Group Replication + Router).',
  mariadb: 'A standalone MariaDB server, for the cases where the difference from MySQL is the point.',
  mariadbrepl: 'MariaDB with asynchronous replication.',
  mariadbgalera: 'MariaDB Galera Cluster — synchronous multi-primary, MariaDB\'s equivalent of PXC.',
  proxysql: 'ProxySQL: query-aware routing, read/write splitting, connection pooling and query rules in front of MySQL.',
  haproxy: 'A TCP load balancer in front of a cluster, health-checking members so traffic only reaches the ones that are ready.',
  psmdb: 'A sharded Percona Server for MongoDB cluster: shards, config servers and mongos routers.',
  psmrs: 'A Percona Server for MongoDB replica set — three mongod members with automatic elections.',
  psm: 'A standalone Percona Server for MongoDB.',
  pg: 'A standalone PostgreSQL server.',
  patroni: 'A Patroni-managed PostgreSQL cluster: leader election, automatic failover, and a REST API to watch it happen.',
  repmgr: 'A PostgreSQL cluster managed by repmgr — the older, more manual HA approach, and still widely deployed.',
  spock: 'A Spock cluster: logical, multi-master PostgreSQL replication.',
  valkey: 'A standalone Valkey (Redis-compatible) server.',
  valkeycluster: 'A Valkey Cluster — sharded key space with replicas, for demonstrating slots, resharding and failover.',
  seaweedfs: 'An S3-compatible object store inside the stack, so backup tools (pgBackRest, PBM, Barman) have somewhere to write without leaving it.',
  k3d: 'A real Kubernetes cluster (k3s) on the canvas, so Percona operators provision the databases as custom resources rather than DBCanvas doing it.',
  vnc: 'A browser-reachable Linux desktop inside the stack, with the database clients and a Firefox that resolves stack DNS and trusts the Intranet CA. The way to open a node\'s web console.',
  linuxclient: 'A plain Linux box on the stack network — a place to run clients and tools from, and the host for core-dump analysis.',
  stocksim: 'A stock-market simulator that drives realistic OLTP load at a database node.',
  marketchaos: 'A market-data load generator with knobs for lock contention, idle transactions and scan queries — the shapes real incidents take.',
  trafficsim: 'A traffic simulator, for a different load profile against the same node.',
  airlinesim: 'An airline-booking workload generator.',
  hotelsim: 'A hotel-booking workload generator.',
  carsim: 'A vehicle-telemetry workload generator.',
}
export const nodeHelp = (type) => NODE_BLURB[type] || ''

// MENU_HELP — the node context menu. These are the actions somebody reaches for when a
// node is already running and they want to get *into* it, which is exactly the moment
// the labels are shortest and the consequences least obvious.
export const MENU_HELP = {
  config: 'Everything DBCanvas recorded about this node: the image and version it was built from, its generated credentials, its certificate, and the provisioning steps it ran.',
  rootConsole: 'A root shell inside the node, in the browser. The same thing you would get from docker exec, without leaving the page.',
  pmmConsole: 'A shell as the unprivileged pmm user — the account PMM\'s own tooling expects. Use the root console for anything that needs to write outside PMM\'s directories.',
  fileManager: 'Browse this node\'s filesystem: upload, download, edit a config in place, change ownership and permissions. Usually faster than a shell for fixing one wrong line.',
  copyExec: 'Copies a ready-made `docker exec -it … bash` line, for when you would rather work in your own terminal than the browser.',
  sshTunnel: 'Copies an `ssh -L` line that forwards every port this node publishes to the same port on your own machine \u2014 for when DBCanvas runs on a server and the ports are bound to it, not to you. It logs in as your DBCanvas username, so on a server where that is also your ssh account the line is ready to paste.',
  stop: 'Stops the container. The data survives; start it again from this menu.',
  start: 'Starts a stopped node back up. Published host ports are re-assigned on start, so the panel\'s addresses may change.',
  restart: 'Restarts the container — the quick way to make a config change you just wrote take effect.',
  deleteNode: 'Removes the node from the canvas, and tears down its container if it is deployed. The data goes with it.',
}

// TOOL_HELP — the tools that act on a deployed stack rather than design it: the query
// runner, the benchmark, the data generator, the packet inspector and the log summary.
export const TOOL_HELP = {
  // Query Runner
  qrServer: 'Which deployed node to run against. For a cluster, running against the proxy and against a member directly are different experiments.',
  qrDatabase: 'The schema the query runs in — the equivalent of a USE / \\connect before it.',
  qrQuery: 'The statement to run. It is sent as written, so anything you can type in a client works here, including DDL and writes.',
  qrCount: 'How many times to run it. 0 runs until the time limit, which is how you turn one query into a sustained workload.',
  qrThreads: 'How many connections run it at once. Raising this is what turns a fast query into lock contention.',
  qrTimeLimit: 'Stop after this long no matter what — the safety net when the count is unbounded or the query is slower than expected.',
  qrPattern: 'A regular expression matched against the result, for turning a query into a pass/fail check.',
  qrCondition: 'What has to be true of the result for this run to count as a success — the comparison the check below is made with.',
  qrCheck: 'Which part of the result to test: a returned value, the row count, or how long it took.',
  qrPoll: 'How often to re-run while watching. Short intervals make a change visible sooner and add load of their own.',

  // Benchmark
  bmServer: 'The node to benchmark. Pick the one whose numbers you actually want — for a cluster, that is usually the proxy or the primary.',
  bmDatabase: 'The schema the benchmark creates and populates its tables in. It is written to, so use a disposable one.',
  bmScale: 'How much data to generate, in units of roughly half a million rows. The number that matters is whether the result fits in the buffer pool: below that you are measuring CPU, above it storage.',
  bmThreads: 'Concurrent client connections. Sweep this upward across runs to find where throughput stops rising and latency starts — the knee is the interesting result.',
  bmDuration: 'How long to measure for. Short runs are dominated by warm-up effects; a minute or more is where the numbers settle.',
  bmWarmup: 'Run this long before the clock starts, so caches are warm and the measured window is steady state rather than a cold start.',
  bmSeed: 'Fixes the random sequence so two runs generate and access the same data. Set it when you are comparing configurations rather than exploring.',

  // Data Generator
  dgTotalRows: 'How many rows to create. The generator streams them, so this is bounded by disk rather than memory.',
  dgBatchSize: 'Rows per INSERT. Bigger batches are faster and produce larger transactions; small ones are how you generate a lot of commits on purpose.',
  dgWorkers: 'How many connections insert in parallel. More workers fill faster and put more pressure on the same indexes.',
  dgFKSample: 'How many parent rows to draw foreign keys from. A small sample concentrates children on a few parents — skewed data, which is what real data looks like.',
  dgSeed: 'Fixes the random sequence, so the same settings produce the same data twice. 0 picks a new one each run.',
  dgOnError: 'Stop at the first failed batch instead of continuing. Leave it off to fill what can be filled; turn it on when a failure means the run is meaningless.',

  // Packet Inspector
  piCaptureFile: 'A capture to decode. DBCanvas parses the protocol itself, so you get statements and replies rather than hex.',
  piServerLog: 'The server log for the same window, so what the server thought was happening lines up with what crossed the wire.',
  piProtocol: 'Which wire protocol to decode as. Get this wrong and the frames will not parse.',
  piServerPort: 'The port the server side of the conversation is on. It is how the decoder tells requests from replies.',
  piNode: 'Capture live from this deployed node instead of uploading a file.',
  piDuration: 'How long to capture for. Start short — a busy node produces a great deal of traffic.',
  piMaxPackets: 'Stop after this many packets, whichever limit is reached first.',
  piSnaplen: 'How many bytes of each packet to keep. Truncating saves space but will cut long statements off mid-query.',
  piBPF: 'An extra libpcap filter, ANDed with the port filter — the way to narrow a capture to one client or one direction.',
  piResolution: 'How finely the timeline is bucketed. Coarser hides short spikes; finer makes a long capture hard to read.',
  piDirection: 'Show only what the client sent, only what the server replied, or both.',
  piIssues: 'Filter to the frames the decoder flagged — errors, retransmits, protocol anomalies. Usually the fastest way in.',

  // Log Summary
  lsLinesPerNode: 'How much of each node\'s log to read back. Raise it when the interesting event is older than the window.',
  lsLogFiles: 'Which logs to include. More files is more context and a slower summary.',
  lsKind: 'Which analyzer to apply. It decides what counts as a finding, so pick the one matching the engine and the topology.',

  // Kubernetes RBAC
  k8sUsername: 'The name of the Kubernetes user this kubeconfig authenticates as. It appears in audit logs and in RBAC bindings.',
  k8sRole: 'What this user may do in the cluster. Grant the narrowest role that lets them do the job — this is the demonstration of RBAC, so making everyone cluster-admin defeats it.',

  // Diagnostics
  pgGatherDatabase: 'Which database pg_gather collects from. It reads catalogs and statistics only, and produces a single HTML report.',
}

// The last handful, on tools whose fields are specific enough not to share.
export const MORE_HELP = {
  bmTable: 'The existing table (or collection) the CRUD workload runs against. It inserts and deletes rows there, so point it at something you can afford to churn.',
  piFromPacket: 'Start the view at this packet number. The pair of these is how you narrow a long capture to the exchange you care about.',
  piToPacket: 'End the view at this packet number.',
  piFromSeconds: 'Start the view this many seconds into the capture — the same window, expressed in time rather than packet count.',
  piToSeconds: 'End the view this many seconds into the capture.',
  piConnection: 'Show one client connection at a time. A busy capture interleaves dozens; picking one turns it back into a conversation you can read.',
  piSearch: 'Filter the frames by their decoded text — a statement fragment, an error code, a peer address.',
  lsSearch: 'Filter the findings by message, error code, peer, or the plain-English explanation attached to them.',
}

// DEP_HELP — the label/value rows on a deployed node's management tabs. The value is
// on screen; what is missing is what to do with it, so that is what these say.
export const DEP_HELP = {
  Container: 'The container name on the Docker host — the handle for `docker logs`, `docker inspect` and `docker exec` from your own shell. The node\'s right-click menu will copy a ready-made exec line.',
  FQDN: 'The node\'s fully-qualified name on the stack network. Use this in a connection string typed on another node or on the VNC desktop; it will not resolve from your own machine.',
  'FQDN (DC / KDC)': 'The domain controller\'s name — also the KDC clients ask for Kerberos tickets from.',
  'Network alias': 'The short name this node also answers to inside the stack, for connection strings that predate the full hostname.',
  Image: 'The exact image this node was built from. Quote it in a bug report — a major version is a range, this is one build.',
  Version: 'The version actually installed, read back from the running node rather than from the design.',
  Arch: 'The CPU architecture the node is running on. Worth checking when behaviour differs from a colleague\'s otherwise-identical stack.',
  'OS / arch': 'The operating system and CPU architecture inside the container.',
  OS: 'The operating system inside the container — which decides the package manager and the repository the database came from.',
  Status: 'What the service itself reports, as opposed to whether the container is up. A running container with an unhealthy service is the interesting case.',
  Role: 'This member\'s part in the cluster: which one takes writes, and which follow.',
  'Role (initial)': 'The role this member was given at deploy. The live role can differ after a failover — check the cluster\'s own status for the current one.',
  Cluster: 'The cluster this node belongs to.',
  Replication: 'How this node replicates, and from where.',
  Source: 'The node this one replicates from.',
  'Source (primary)': 'The primary this replica follows. After a failover this is the node it was repointed at.',
  GTID: 'Whether global transaction IDs are on. With GTID a replica can be repointed with AUTO_POSITION instead of a hand-calculated binlog file and offset.',
  'server-id': 'The unique numeric id MySQL replication uses to tell servers apart. Two nodes sharing one breaks replication in confusing ways.',
  read_only: 'Whether this node refuses writes. A replica should be read-only; a primary should not.',
  TLS: 'Whether the node is serving TLS and who signed the certificate. "Intranet CA" means every node in the stack, and the VNC desktop\'s browser, already trust it.',
  'TLS certificate': 'The certificate this node presents. Check the subject and expiry here before blaming a client for refusing to connect.',
  Subject: 'The certificate\'s subject — the name a verifying client expects to match the host it dialled.',
  'S3 TLS': 'Whether the S3 endpoint is served over HTTPS. Backup tools have to be told which, so this is the setting to check when one cannot connect.',
  Ports: 'The ports this node listens on, and which of them are published to the host.',
  'Exported port': 'The port this node is published on, on the machine running DBCanvas. Point a local client here — and if DBCanvas runs on a remote server, copy the SSH tunnel command from the node\'s right-click menu first.',
  'Host port (5432)': 'Where PostgreSQL is published on the host. `psql -h 127.0.0.1 -p <this>` from the machine running DBCanvas.',
  'Stats port (7000)': 'HAProxy\'s statistics page, showing every backend and its health-check state — the first place to look when traffic is not reaching a member.',
  Endpoint: 'The address clients use for this service.',
  'PgBouncer endpoint': 'Connect here rather than to PostgreSQL directly when you want the pooler in the path.',
  'Monitored by': 'The PMM server collecting this node\'s metrics. Open that node\'s panel for the PMM address and login.',
  'PMM service token': 'The credential the agent on this node authenticates to PMM with. Disposable, like everything else in a lab stack.',
  Grafana: 'The Grafana bundled with this deployment.',
  'Grafana dashboard': 'Open this for the dashboards. From your own browser only if the address is a host one — otherwise use the VNC desktop.',
  'Grafana user': 'The Grafana login. Its password is the row below.',
  'Grafana password': 'Click to reveal and copy. A lab credential — do not reuse it anywhere real.',
  'Grafana service': 'The Kubernetes Service in front of Grafana, and how it is exposed.',
  'Grafana SMTP': 'Where Grafana sends alert mail — the Intranet node\'s mail server, so alerts land in its webmail.',
  Backups: 'The backup tool configured for this node and where it writes.',
  'Backups (PBM)': 'Percona Backup for MongoDB, and the S3 bucket it targets. Run and restore backups from this node\'s Backups tab.',
  'Barman backups': 'Barman\'s backup store for this cluster.',
  pgBackRest: 'The pgBackRest repository this cluster backs up to, and clones new replicas from.',
  Encryption: 'Whether data at rest is encrypted, and where the key lives.',
  'Encryption at rest': 'Whether the data files are encrypted, and which keyring holds the key. If it is the OpenBao node, that node going away takes the database with it — which is usually the point of the exercise.',
  'Seal state': 'Whether the key server is sealed. A sealed vault hands out no keys, so anything depending on it stops.',
  VAULT_ADDR: 'Export this in a shell to point the bao/vault CLI at this node.',
  VAULT_CACERT: 'Export this alongside VAULT_ADDR so the CLI trusts the Intranet CA instead of refusing the connection.',
  LDAP: 'Whether this node authenticates against a directory, and which one.',
  'Base DN': 'The directory subtree users are looked up under — the base of every LDAP search this node makes.',
  Domain: 'The Active Directory / DNS domain this node belongs to.',
  Workgroup: 'The NetBIOS short name of the domain, which older clients ask for.',
  'Sample users': 'Demo accounts created at deploy so there is something to log in as. Their password comes from .env.',
  'Group name': 'The directory group whose membership is mapped to a role here.',
  Realm: 'The Keycloak realm this node authenticates against.',
  'Client ID': 'The OIDC client id, and the audience the token must carry for this node to accept it.',
  Issuer: 'The OIDC issuer URL this node validates tokens against. It must match the token exactly, down to the scheme and port.',
  Authorization: 'How the token is turned into privileges here — which claim is read, and what it maps to.',
  'Keycloak SSO': 'Whether single sign-on is wired up on this node, and against which Keycloak.',
  'App role / database': 'The application account and the database it owns — the credentials to hand an application under test, rather than the superuser.',
  Database: 'The database this row refers to.',
  'Database cluster': 'The operator-managed cluster these settings belong to.',
  'Password in Secret': 'The Kubernetes Secret holding the password. `kubectl get secret <name> -o jsonpath=...` reads it; the node\'s Credentials tab shows it directly.',
  Instances: 'How many database instances the operator is running. Fewer than three means losing one loses quorum.',
  Nodes: 'The members of this cluster.',
  'Backend members': 'The servers this proxy forwards to, and their health.',
  Backend: 'What this proxy sits in front of.',
  'Backend (CLUSTER_HOSTNAME)': 'The cluster proxysql-admin was told to manage. It is how ProxySQL discovers members as they come and go.',
  'Routes to cluster': 'The cluster this proxy sends traffic to.',
  Mode: 'How this proxy decides where a query goes.',
  ProxySQL: 'The ProxySQL fronting this cluster.',
  'ProxySQL cluster': 'The ProxySQL cluster this member belongs to — they share configuration between themselves.',
  'MySQL Router': 'The Router fronting this cluster, and the read/write ports it offers.',
  'PXC cluster': 'The PXC cluster this node is a member of.',
  'PS MongoDB': 'The Percona Server for MongoDB deployment this belongs to.',
  PostgreSQL: 'The PostgreSQL this row refers to.',
  PgBouncer: 'The connection pooler in front of PostgreSQL, and how it is exposed.',
  Spock: 'The Spock logical-replication setup on this node.',
  'Spock node': 'This node\'s identity in the Spock mesh — how the other members address it.',
  'Mesh peers': 'The other members this node replicates with. Spock is multi-master, so every pair is a two-way relationship.',
  'repmgr node_id': 'This node\'s numeric id in the repmgr cluster. repmgr commands take it, and it must be unique.',
  'Bootstrap node': 'The member that started the cluster. It matters only at first boot — afterwards every member is equal.',
  'etcd endpoints': 'The distributed configuration store Patroni keeps its leader lock in. If these are unreachable there is no leader, whatever the databases are doing.',
  configDB: 'The config-server replica set a mongos router reads cluster metadata from.',
  Topology: 'The shape of this deployment — replica set or sharded, and how many of each part.',
  Kubernetes: 'The Kubernetes cluster these resources live in.',
  Namespace: 'The namespace these resources were created in — `kubectl -n <this>` for anything you run by hand.',
  Operator: 'The operator reconciling this database. It, not DBCanvas, is what actually creates and heals the pods.',
  Manifests: 'Where the custom resources were written on the node. Edit them there and re-apply to change the cluster the way you would in production.',
  'LoadBalancer pool': 'The address range k3s hands out to LoadBalancer services in this cluster.',
  'Expose · Postgres': 'How PostgreSQL is reachable from outside the cluster.',
  Config: 'Where this node\'s configuration file lives. Edit it with the file manager, then restart the node from its right-click menu.',
  Debugger: 'Where the Delve debugger is listening. Point your IDE\'s Go remote-debug configuration here to attach to the operator.',
  'Disk limit': 'The I/O ceiling this node was given, if any. A node that looks inexplicably slow is worth checking here first.',
  Budget: 'The resource budget this deployment was planned against.',
  Region: 'The region label on the object store, which S3 clients must be configured to match.',
  'PDPS repo': 'The Percona Distribution repository the packages came from, which is what fixed the version.',
}

// FTDC_HELP — the header of a parsed FTDC (MongoDB full-time diagnostic data) file.
// Half of these are properties of the *capture* rather than of the server, which is
// exactly the distinction that trips people up when two files will not compare.
export const FTDC_HELP = {
  Comparing: 'The captures being overlaid. Charts show one line per capture, so this is who is who.',
  Window: 'The time span covered. Two captures only compare meaningfully where their windows overlap.',
  Charts: 'How many metrics were plotted from this file.',
  Host: 'The server the diagnostic data came from.',
  Version: 'The MongoDB build that wrote the file. Metric names move between releases, so a missing chart is often just this.',
  Role: 'The member\'s role at capture time — primary and secondary have very different-looking metrics.',
  'Replica set': 'The replica set the member belonged to.',
  Span: 'How long the capture covers. A short span exaggerates spikes; a long one can average an incident away entirely.',
  Samples: 'How many data points the file holds. Sparse samples mean the chart is interpolating between distant readings.',
  Metrics: 'How many distinct metrics were recovered. A low count usually means a truncated or partly corrupt file.',
}
