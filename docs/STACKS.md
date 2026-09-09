# Stacks
Design a topology on a canvas and turn it into real running nodes — containers, or VMs in
hybrid mode. Add nodes and cluster **frames**, connect them, give the stack a **TTL**, and
deploy. Every node type gets its own management panel: a web terminal, its credentials and
certificate, users, and on-demand backups.

> [!IMPORTANT]
> Stacks are **disposable lab infrastructure**, for previewing features and testing. They are
> built for speed of setup rather than durability or security, and are meant to be torn down.
> Do not deploy production infrastructure with DBCanvas.

![The stack list — every stack, its lifetime and its state](screenshots/stacks-list.png)

> **New here?** [Getting started](GETTING_STARTED.md) walks the whole path — install, first
> stack, deploy, connect — in order. This page is the reference behind it.

## How the canvas works

Four things live on it, and everything below is a variation on them.

- **Nodes** are the things that run: a database, a proxy, a monitoring server, a desktop.
  **Right-click the canvas** where you want one and pick it from the **Infrastructure
  Library** — the first level of the menu is the categories, and whatever you pick lands at
  the point you clicked. Hover an entry to see what that type actually gets you before you
  add it, including the reason a greyed-out one is unavailable. If you would rather browse the
  whole catalog at once — with a search box that also matches aliases like `redis`, `k8s` or
  `vault`, and a recently-used list — set **Adding nodes** to *Docked library* in Settings and
  the library returns as a column on the left. The canvas menu keeps working either way.
- **Frames** are clusters. A dashed box owns its members, and has its own panel for what is
  true of the whole cluster — its name, version, replication mode — while each member's panel
  sets what is true of just that node. Adding a *PXC Cluster* gives you the frame and its
  nodes together.
- **Links** wire nodes to each other: drag from one node's port to another's. This is how a
  database gets monitored by a PMM server, fronted by a ProxySQL, or backed up to a SeaweedFS
  S3 node. An **association line** has no direction — it records that two things are related,
  and it reads the same whichever end you started the drag from, so a ProxySQL fronting a
  cluster and a simulator driving one are both drawn as plain lines. The one link that *is*
  directional is a **cross-cluster replication link** between two cluster members, which is
  drawn with an arrow from source to replica (or a head at each end when it is bidirectional)
  because there the direction is what you are choosing. The same arrow between **two Kubernetes
  frames** sets up cross-cluster replication between two operator-managed PXC clusters — the one
  link that joins two frames rather than two nodes, because a Kubernetes cluster's identity on the
  canvas *is* its frame.
- **The Properties panel** on the right edits whatever is selected. Every field has a `?`
  explaining what it is for and when you would change it.

**The Intranet node goes on first.** The rest of the library is greyed out (with the reason on
hover) until it is there,
because it provides what everything else assumes exists: DNS for the stack's hostnames, a
certificate authority the other nodes trust, OpenLDAP, mail, and a Squid proxy that caches
package downloads (which is what makes the second node of the same OS much faster than the
first).

Then **Validate** — a dry run that catches missing links, impossible topologies and host-port
clashes without building anything — and **Deploy**, which provisions in dependency order and
shows every step in the deployment console. A node that fails leaves the rest running, so you
can get into it and look.

One kind of Validate error can be fixed without leaving the page. `make images` builds the
operating-system bases and the Intranet on top of them; the rest of the images layered on the
bases — the VNC desktop, the K3D collector, MClusterAdmin, Big Hole — are `make extra-images`,
which `make install` runs but does not insist on, because they fetch from npm, GitHub and
Percona's repositories and fail for reasons that are not about your machine. So a stack can
name an image nobody has built yet, and when it does, an **administrator
gets a Build button under the error**: the same build, on the same Docker daemon DBCanvas is already using, with the
log if it fails. The demo applications are the exception — their build context is a directory of
the DBCanvas source, so they need a checkout and `make <name>-image`.

## What you can put on the canvas


- **PostgreSQL** — standalone, **Patroni** HA clusters, **repmgr** clusters, and **Spock**
  multi-master (active-active) clusters (pgBackRest / Barman cloud backups; pgvector &
  TimescaleDB supported).
- **MySQL (Percona)** — **Percona XtraDB Cluster**, Percona Server, MySQL replication, and
  **InnoDB / Group Replication** clusters.
- **MySQL (Community)** — Oracle's community builds (8.0 / 8.4) from repo.mysql.com:
  standalone, replication, and **InnoDB Cluster / Group Replication** (MySQL Shell +
  MySQL Router).
- **MariaDB** — from mariadb.org (10.6 / 10.11 / 11.4 / 11.8): standalone, replication
  and **Galera** clusters. MariaDB's GTIDs are `domain-server-seq`, so replication is
  wired with `MASTER_USE_GTID = slave_pos` and the cluster's `gtid_domain_id` is derived
  from its name; Galera state transfers use `mariabackup`.
- **MongoDB** — Percona Server for MongoDB: standalone, replica set, and sharded
  (PBM backups; optional Keycloak OIDC auth), plus two tools for them: **MClusterAdmin**, a web
  administration panel, and **Big Hole**, an FTDC viewer (both below).
- **Valkey** — standalone and cluster (LDAP integration, PMM monitoring).
- **Kubernetes** — a **K3D cluster** frame (1–3 k3s nodes, created by k3d on the stack network,
  with MetalLB for LoadBalancer services) that can install any of the four Percona operators —
  **MySQL (PXC)**, **MySQL (Percona Server)**, **MongoDB (PSMDB)** or **PostgreSQL** — into a
  namespace of your choosing.
- **Core** — an **Intranet** node (OpenLDAP, bind DNS, an internal CA, a Squid proxy, and
  Roundcube/Dovecot webmail). One per stack, and the one node that goes on first.
- **Proxies & HA** — **ProxySQL** (standalone or a cluster), **HAProxy**, and
  **Orchestrator** (MySQL topology discovery, failure detection and failover).
- **Monitoring** — **PMM** and **Watchtower**, which rolls a PMM server onto a newer image
  so an upgrade can be demonstrated.
- **Identity & Secrets** — a **Samba AD DC** (Active Directory, LDAP, Kerberos),
  **Keycloak** (OIDC), and **OpenBao** (secrets manager).
- **Storage & Clients** — **SeaweedFS** (S3 for backups, up to 10 buckets, browsable from
  its panel), an **Ubuntu VNC** desktop, and a **Linux Client** jump box (a bare OS host with
  nothing installed, on any base image the matrix builds — Oracle Linux 8/9/10, Ubuntu
  22.04/24.04 or Debian 12/13: join the stack's DNS/CA trust, then use its terminal to install
  and exercise whatever client tools a task needs — or tick *use this client for core-dump
  analysis* and it becomes one, see below).
- **App Simulators** — link **Traffic Sim** to a Valkey node/cluster, **Hotel Sim** to a PS
  MongoDB standalone/replica-set/sharded node, **Airline Sim** to a standalone Percona Server
  node, a MySQL replication or PXC cluster, or a ProxySQL/HAProxy node fronting one, **Car
  Rental Sim** to a standalone PostgreSQL node, a Patroni/repmgr/Spock cluster, or an HAProxy
  node fronting one, and it drives real, continuous background traffic against it (reads/writes
  for Traffic Sim; a 100-hotel reservation workload exercising CRUD, transactions and change
  streams for Hotel Sim; a 200-route reservation workload against a 2000-aircraft fleet,
  exercising real MySQL transactions and Galera certification-conflict retries under contention,
  for Airline Sim; a 180-location rental workload against a 2000-vehicle fleet, exercising a
  date-range-guarded multi-row UPDATE for booking and a `FOR UPDATE SKIP LOCKED` claim for
  vehicle check-out, for Car Rental Sim; a 200-security fictional stock exchange — 10 workload
  agents, a traffic level × mix control, and an 18-challenge catalog of deliberately-injected
  indexing/query/locking/PXC problems the learner diagnoses and fixes, graded on outcome
  (baseline vs. validated measurements, a hard correctness gate, a 100-point score) rather than
  a checked SQL answer, for MarketChaos), with a live dashboard reachable from the stack's
  Ubuntu VNC desktop.
- **Stock Market Sim** — the one app simulator you *operate* rather than just watch, and the one
  that does not have to be linked to anything on the canvas. Background agents move prices, place
  orders and settle trades continuously, while you create, edit and delete securities, portfolios
  and orders from its own web interface; it generates a printable **report** (print to PDF) and
  per-table **CSV exports**, shows the tables it created in the target database, and can drop them
  again when you're done. The same application runs on **MySQL, PostgreSQL, MongoDB or Valkey** —
  link it to a standalone Percona Server, PostgreSQL, PS MongoDB or Valkey node with a drawn
  association line, *or* switch the node to a **manual connection** and give it a host, port,
  user, password and database — reaching a database that is not part of the stack at all:
  elsewhere on the Docker host (`host.docker.internal`), on your network, or a managed cloud
  instance, with a **Test connection** button that checks it before you deploy. One node drives
  exactly one database, so four nodes side by side give you four independent applications with
  four dashboards, one per engine. It connects to **any database in the stack**: a standalone
  Percona Server, MariaDB, MySQL, PostgreSQL, PS MongoDB or Valkey node; any cluster frame (PXC,
  MySQL/MariaDB/MySQL CE replication, Galera, InnoDB Cluster and Group Replication, Patroni,
  repmgr, Spock, PSMDB replica sets and sharded clusters, Valkey cluster); a **Kubernetes frame
  running any of the six database operators** (PXC, Percona Server for MySQL, PSMDB, Percona
  PostgreSQL, CloudNativePG, Crunchy PGO), whose engine follows the operator the frame runs; or a
  **ProxySQL/HAProxy** node fronting one — always resolving to the cluster's write endpoint, be
  that the primary, the leader, the router or the mongos. A database inside Kubernetes has to be
  reachable from the stack network first: set the tier in front of it — the proxy, the mongos
  routers, the pgBouncer pool, or the database pods themselves — to **LoadBalancer** or
  **NodePort** on the frame, since a ClusterIP address exists only inside the cluster, and the
  designer says so before you deploy if none of them is. The two PostgreSQL operators are reached
  through their **pgBouncer** pool when it has an address, as their own application user, and
  directly on the primary as `postgres` when it does not. A MongoDB **replica set** is the one target whose
  address does not follow a failover — the set advertises in-cluster names, so the sim is pointed
  straight at the member holding the primary role and an election means redeploying the node; a
  sharded cluster has mongos in front of it and does not have this problem. An **All in One** node draws no association lines, so its instance is chosen from a
  picker on this node instead of with a line. Set the load level to **High** and it also grows the dataset:
  bulk price history is written until the app owns a configurable **dataset size** (5 GiB by
  default), so there is something real on the volume to measure a disk, a storage class or a
  backup against — the simulation carries on at its normal rate once the target is met. A
  configurable **working set** (half the dataset by default) is then kept under continuous random
  read, which is what makes cache size measurable: a dataset that is only ever written to is
  served out of a few hundred kilobytes of hot rows, so a 128 MiB buffer pool reports a ~100% hit
  rate and reading its size back tells you nothing. With a working set larger than the cache it
  misses properly — the same 2 GiB dataset that showed 99.98% and 0.01 MiB/s off disk shows 92.8%
  and 469 MiB/s, and raising the pool past the working set takes throughput up 6.6×. **Database
  threads** (4 by default) sets how many workers write that history and read it back, and sizes
  the connection pool with it. Settled orders are swept after a retention window (15 minutes by
  default) so the order book stays bounded — without it the dashboard's own two-second order
  count grows linearly more expensive for the life of the deployment, and cumulative figures
  stay correct across the sweep because what was removed is tallied durably.
  Six **deliberate problems** can also be switched on, each reproducing a condition that is
  hard to cause on purpose and easy to hit by accident: an **idle transaction** held open with
  a read snapshot (up to 24h) so purge cannot advance and the InnoDB history list — or
  PostgreSQL's xmin horizon and the bloat behind it — grows for as long as it sits there;
  **extra tables** (up to 5000, read in rotation) so `table_open_cache` stops holding the
  working set and every query pays to reopen one; **temporary-table queries** shaped to
  build a large intermediate result either in memory or forced to spill to disk;
  **lock contention**, where concurrent writers compete for a handful of rows — queueing on
  *light*, and on *heavy* taking the same two rows in opposite orders so the server has to
  detect and break real deadlocks; **scan queries** against the tick history with a predicate
  no index can serve, so the server reads every row to return a handful; and **write
  pressure** in either of its two distinct shapes — *commits*, many tiny transactions that
  each pay for their own log flush, or *redo*, bulk rewrites that fill the write-ahead log and
  eat checkpoint headroom. Measured on a lab server: history list 6,496 and climbing, 15,896
  `Table_open_cache_overflows` against 1,200 tables and a 400-entry cache, the same rollup
  taking 1,961 ms spilled versus 429 ms in memory, 364 deadlocks a minute on *heavy* against
  none on *light*, 11.0M rows read to return 122,949 (about 90 read per row returned), and one
  second of *commits* costing 142 log syncs for 176 KiB against *redo*'s 18 syncs for 2.2 MiB —
  the same knob, opposite costs. Nothing grows: the contended and committed writes go to rows
  this app owns, and the bulk rewrites overwrite a fixed 256 rows in place.
  Each knob is offered only on an engine that can actually do it. On MySQL, PostgreSQL and MongoDB it takes its own database or
  schema; on Valkey there is no size target and no working set, because its tick history is a
  length-capped stream that writing to does not enlarge and that holds no cold data to read.
  This one is a working application rather than a tuning puzzle: it has CRUD and a report.
- **All in One** — one container running **many** database instances side by side, instead of
  one product per node. Add features to it from a menu (Percona Server, PS replication, InnoDB
  Cluster / Group Replication, PXC, PostgreSQL, repmgr, Patroni, Spock, PSMDB standalone /
  replica set / sharded, Valkey standalone / cluster, ProxySQL, HAProxy, Orchestrator) and each
  becomes an independent instance with its own datadir, config, systemd unit and **non-default
  port**. It draws no association lines: every relationship — PMM monitoring, an LDAP directory,
  OpenBao, Keycloak, a SeaweedFS backup target, the instance a proxy fronts — is a drop-down on
  the instance itself. See [All in One](ALL_IN_ONE.md).
- **Operations** — cross-cluster replication links, per-node web terminals, certificate
  management, on-demand backups, and TTL-based auto-teardown.
- **Constrained nodes** — every node can be limited on all four resources it competes for:
  **CPU** and **memory** (container limits), **disk** (`--device-read-bps`/`--device-write-bps`),
  and the **network** — added latency, jitter, packet loss and a bandwidth cap, applied with
  `tc`. The network one is what makes a synchronous cluster interesting. Measured on a live
  three-node Galera cluster: a degraded link (200 ms ±40 ms, 10% loss, 1 Mbit) backs the *writer*
  up — `wsrep_local_send_queue` rises and a bulk insert that took 22 ms unimpaired ran for
  minutes — and severing it (100% loss) evicts the member in **8 seconds**, the majority side
  holding `cluster_size 2 / Primary` while the isolated node drops to `1 / non-Primary` and stops
  accepting writes; clearing the impairment rejoins it in **11 seconds**. Worth knowing: a slow
  *link* stalls the sender, whereas receiver-side flow control needs a slow *node* — they are
  different experiments, and the second one is produced with the CPU/memory limits or plain
  configuration rather than with `tc`. Measured on the same rig, with two PXC members tuned
  (`innodb_flush_log_at_trx_commit=2`, `sync_binlog=0`, 2 GiB pool and redo) and one left at
  stock (`1`, `1`, 128 MiB): driving updates at a tuned member left the stock member's receive
  queue averaging **168 writesets against the tuned member's 1.1** on the identical stream,
  sending **500 flow-control pauses** while the tuned member sent none, and pausing the writer
  **44%** of the time. A bandwidth cap mostly just slows a state transfer down. Shaping is
  scoped to the node's own database and cluster ports (for PXC: 3306, 4444, 4567, 4568), so DNS,
  LDAP and health checks stay clean and the node degrades rather than looking broken — measured
  on a lab node, ping stayed at 0.09 ms while port 4567 went to 124 ms. It is applied *after* the
  cluster forms, because a lossy link fails state transfer, and it is a runtime change, so a
  redeploy re-applies it without recreating anything.

**Authentication.** Point a database at a directory and it is wired at deploy: **LDAP** against
the Intranet OpenLDAP or the Samba AD DC (Percona Server, PostgreSQL, PSMDB), **Kerberos/GSSAPI**
single sign-on against the Samba AD DC (PostgreSQL, PSMDB), and **Keycloak OIDC** (PMM, PostgreSQL
18 via `pg_oidc_validator`, PSMDB via `MONGODB-OIDC`, Percona Server 8.4 via `auth_openid_connect`).
The designer greys out combinations an engine cannot actually run — PostgreSQL cannot do LDAP and
OIDC at once (they compete for the same `pg_hba` line), and MongoDB cannot combine OIDC with
LDAP/Kerberos (each needs its own `mongod.conf` `setParameter` block) — and validation blocks the
deploy rather than letting one silently win. MySQL has no such conflict: it picks an auth plugin
per account, so LDAP and OIDC accounts live side by side on one server.

Turning on Keycloak SSO for a **Percona Server** node moves it to 8.4 (latest minor): Percona
added `auth_openid_connect` in **8.4.11-11**, and the 9.7 series does not carry it yet. The deploy
wires the whole demo, not just the plugin — a realm and a public `mysql` client on the Keycloak
node, sample users `jane` and `john` in an `accounting` group, MySQL accounts bound to those users'
`sub` claims, and an `oidc_demo` schema only the group's role can read. The node's **Keycloak SSO**
tab shows how to log in, and DBCanvas writes a small shell wrapper to `/usr/local/bin/oidc-login`
on the node (not an upstream tool) that does the whole round-trip in one command: ask Keycloak for
an ID token, hand the file to `mysql`. No MySQL password is ever sent, and the
group→role mapping means `SHOW GRANTS` gains `accounting` at connection time (activate it with
`SET ROLE`). The link must be encrypted — a Unix socket, or TCP with `--ssl-mode=REQUIRED`.

**Data-at-rest encryption (OpenBao).** Add an **OpenBao** node (a Vault-compatible secrets
manager, one per stack) and tick *Encrypt with OpenBao* on a Percona Server or PSMDB node. At
deploy the node is initialized and unsealed for you — its **5 unseal keys and root token** appear
in the node's properties, since OpenBao prints them exactly once — and the database is wired to it
as its keyring: `component_keyring_vault` on Percona Server 8.4, the `keyring_vault` **plugin** on
5.7/8.0 (the component does not exist before 8.4), and `security.vault` on PSMDB. Each database
gets its own KV mount and a token scoped to it, and verifies OpenBao with the Intranet CA every
node already trusts. OpenBao seals itself on every restart, so its panel shows the live seal state
and can replay the stored keys with one click.

**Kubernetes with the Percona operators.** Add a **K3D Cluster** frame and pick a Percona operator —
all four are supported: **PXC**, **MySQL (Percona Server)**, **MongoDB** and **PostgreSQL**. DBCanvas runs
k3d against the same Docker daemon it already uses, creating the k3s nodes **on the stack network**
— so pods resolve the Intranet DNS, reach PMM and SeaweedFS by name, and **MetalLB** hands out
LoadBalancer addresses from the stack subnet that every other container can reach. Several K3D
frames can share one stack: each cluster gets its own block of 8 addresses from the top of the
subnet, so two clusters on the same network never advertise the same address. You choose the
cluster size, its **Kubernetes version** (any k3s release `make versions` discovered; the newest by
default — k3d's own default trails the releases far enough to break some operators' CRDs), its
CPU/memory budget (a total, split across the nodes — DBCanvas warns if it is too small to schedule the
cluster, or too large for your host), the namespace, the shape of the cluster
(PXC: **HAProxy or ProxySQL** in front; Percona Server: **group replication or async** replication
under Orchestrator, behind HAProxy or **MySQL Router**; MongoDB: a **replica set or a sharded
cluster** with mongos routers; PostgreSQL: a Patroni HA cluster behind **pgBouncer**), and how each tier is exposed
(ClusterIP / NodePort / LoadBalancer — the database can stay in-cluster while the proxy, router or
pooler takes a LoadBalancer address). The operator's source is unpacked into
`/root` on the first node, and its `cr.yaml` is rewritten before it is applied — anti-affinity set to
`none` and every section's CPU/memory requests commented out, because the shipped file assumes a real
multi-node cluster and would otherwise never schedule.

Link a **SeaweedFS** node and the cluster backs up to it over S3; link a **PMM** node and DBCanvas
mints a **service token** on the PMM server (you choose how long it lives — 365 days by default) and
patches it into the cluster's secret, so the pmm-client sidecars register themselves and the whole
cluster shows up in PMM. The cluster's users come from your `.env`, like every other database
DBCanvas deploys, so the root password is the one you already know. (PostgreSQL is the one exception
worth knowing: pgBackRest speaks S3 only over TLS, so its backups need a SeaweedFS node with **TLS
on** — the designer warns you when it isn't, because without it the cluster silently keeps the
operator's own PVC backup repo and the bucket stays empty.)

**Step through the operator itself.** A frame running any of the **four Percona operators** can
be deployed with the operator running under **Delve**: tick *Run the operator under Delve*, and
DBCanvas rebuilds the operator from that release's own source with the optimiser off and runs it
under `dlv` in place of the released binary. Then open the
[**Operator Debugger**](OPERATOR_DEBUGGER.md) — breakpoints, call stack, variables and
expressions, plus a button that forces a reconcile so a breakpoint in `Reconcile` is actually
reached. No IDE, no clone of the operator, no Go toolchain, and no `kubectl port-forward` to keep
alive. (The two community PostgreSQL operators come from a Helm chart rather than a release
tarball, so there is no source to compile or to show, and the option is not offered for them.)

The pod keeps the released image (only its command changes), the probes and leader election are
turned off so you can sit on a breakpoint, and Delve starts with `--continue` so the cluster
still deploys whether or not you ever attach. It costs a few minutes of build time on the first
deploy.

**Read a core dump from somewhere else.** A **Linux Client** node can be deployed as a core-dump
analysis host: give it a host directory holding a `mysqld` core file and another holding the
crashed server's `mysqld` plus everything `ldd` listed for it, pick the Percona Server or PXC
version that crashed, and DBCanvas bind-mounts both read-only and installs the matching debug
symbols. Then open the [**Core Dump Analyzer**](CORE_DUMP_ANALYZER.md) — threads, the stack that
took the signal, and each frame's arguments and locals, with recursion collapsed and a verdict on
whether the symbols and libraries actually match the core. An 800 MB core is read where it lies,
not copied. The two directories are confined to `GDB_MOUNT_ROOT` (`.env`).

Ticking *Also publish the debugger to the host* additionally exposes Delve on
`127.0.0.1:40000` for an external editor; the server node's **Operator** tab then hands you the
matching `git clone`, a ready `launch.json` (with the `substitutePath` that makes source line
up), and the annotation that forces a reconcile. Leave it off when you debug from DBCanvas: the
port is fixed, so two debugged clusters would collide on it.

Stop the debug session whenever you like — DBCanvas clears the breakpoints and resumes the
operator when you close the page, resumes a stopped session nobody has touched for five minutes,
and a watchdog sidecar covers even the case where DBCanvas itself dies, within ten seconds. That
matters more than it sounds: a breakpoint that outlives its session fires on the next reconcile
with nobody attached, and the operator freezes with no probe failing and nothing in its log, so
the cluster quietly stops being reconciled; the next attach then shows the breakpoint as
*unverified*, which reads as a broken debugger rather than as leftovers in the way.

![A K3D cluster node's panel beside a console listing the pods the operator built](screenshots/k3d-cluster.png)

> *A one-node K3D cluster on k3s v1.36.3 running the **Percona Operator for MySQL (PXC)
> 1.20.0**, with a console open on the same node: `kubectl get pods -A` shows the three
> `k3d-00-pxc` members and three `k3d-00-haproxy` pods the operator built, alongside MetalLB
> and the operator itself. The panel is the cluster at a glance — the operator and its
> namespace, HAProxy in front, the database kept ClusterIP while the proxy takes a MetalLB
> address — and its tabs carry `kubectl`, a copyable kubeconfig and per-namespace Kubernetes
> users.*

**Kubeconfig and RBAC users, for testing access control.** The K3D server node's panel has a
**Kubeconfig** tab (a copyable admin kubeconfig, pointed at k3d's own load balancer so it works
from any other node in the stack — e.g. paste it into the **Linux Client** node's terminal) and a
**Users** tab: create a genuine Kubernetes `User` — a real X.509 client certificate, signed by the
cluster's own CA — bound to a built-in ClusterRole (`view`/`edit`/`admin` scoped to one namespace,
or `cluster-admin` cluster-wide), then copy that user's own kubeconfig and confirm exactly what it
can and can't do.

**Replicating one Kubernetes cluster into another.** Draw a link between two K3D frames that both
run the **PXC operator** and pick a direction: on the next Deploy the second cluster becomes a
replica of the first. There is nothing else to set up — start from the *Kubernetes — PXC operator,
two clusters replicating* template, or add the link to two clusters you already have.

What DBCanvas does with it is Percona's own two procedures, [*Restore to a new
cluster*](https://docs.percona.com/percona-operator-for-xtradb-cluster/latest/backups-restore-to-new-cluster.html)
and [*Set up cross-site
replication*](https://docs.percona.com/percona-operator-for-xtradb-cluster/latest/replication.html),
run in the order they have to happen in:

1. The **source**'s `cr.yaml` declares the channel (`replicationChannels`, `isSource: true`), and
   its database pods each get their own LoadBalancer address — a replica in another cluster cannot
   reach a ClusterIP, so this overrides the frame's *Database Service* setting and says so in the
   node's log.
2. Both clusters deploy **at the same time**; the replica waits for nothing.
3. Once both are ready, a **backup of the source** is taken to its SeaweedFS bucket.
4. That backup is **restored onto the replica** — pointed at the *source's* bucket, which the
   replica can open because both clusters' S3 credentials come from the same store. The cluster
   pauses while this runs and comes back carrying the source's data and its GTID history.
5. **Only then** is the replica's own channel attached, with the source's pod addresses as its
   sources. Attaching it earlier would point the replica at binary logs the source has already
   purged, and replication would stop with error 1236 instead of starting.

Both clusters must use the **same SeaweedFS node** (each with its own bucket is fine, and is what
the template does) — that is the one thing validation refuses to deploy without, along with the
address arithmetic: a source spends one MetalLB address per database pod on top of its proxy tier,
out of the eight a cluster's block holds.

The server node's **Replication** tab is where it is afterwards: which end this cluster is, the
channel, what it reads from or is reachable at, the backup it was seeded from, and — on the replica
— whether the channel is actually running, with the IO/SQL thread state and the last error when it
is not. **Re-seed** is there too, and it is the only thing that restores again: pressing Deploy
reconciles the channel and never touches the data, because a seed replaces it. Take the link off
the canvas and the next Deploy stops the channel and leaves the data where it is.

> **Replicating is one-way.** The operator holds a cluster that carries an inbound channel
> read-only, so bidirectional is not offered between Kubernetes clusters — write to the source.

**S3 backups (SeaweedFS).** One SeaweedFS node can create **up to 10 buckets**, and every database
that backs up to it — standalone PostgreSQL, Patroni, repmgr, the MongoDB clusters, and all four K3D
operators — **picks which bucket it uses**, so a stack's backups don't have to share one. Once the
node is running, its panel **browses the buckets**: pick one, list what actually landed in it, and
click into the folders backups nest under (`pbm/<cluster>/…`, `pgbackrest/<cluster>/repo1/…`). It is
read-only — a way to confirm a backup exists without exec-ing into anything.

> *Browsing `pxc-backups` inside the backup the PXC operator just wrote — the xtrabackup files with
> their sizes and times. The breadcrumb walks back out; the selector switches buckets.*

**Deployed versions.** Once a node is running, DBCanvas records the version it *actually* deployed
with — `PS 8.4.10-10`, `PSMDB 8.0.26-11`, `PMM 3.3.1` — not just the series that was requested
(`8.0`, or "latest"). It is in the node's properties, and on the canvas it is in the card's
tooltip: a card itself carries an icon, a name and a status, and nothing else. Three lines of
prose in a 212px box wrapped, clipped, and turned a diagram into a wall of half-sentences — so
hovering a card is what tells you what it is, what it was built from, and what state it is in.

Every deployed node gets a **management panel** — runtime profile, endpoints, credentials,
certificates, backups, and one-click consoles:

**MongoDB downloads.** Right-click any MongoDB node — standalone, replica-set member, shard
member or mongos — for **Download diagnostic.data with mongod.log** (mongos.log on a router), an
entry the rest of the app makes you go looking for. One `.tar.gz` holding one
`<hostname>_log_diagnostic_data` directory with both in it: FTDC is what the server was doing, the
log is what happened to it, and nobody reading them wants one without the other. Naming the
directory after the node is what lets three members' bundles unpack side by side instead of over
each other. It streams, so a multi-gigabyte log costs nothing in memory on the way past. DBCanvas
works the paths out itself, including the one that catches people: a mongos keeps its FTDC beside
its log rather than in a dbPath it does not have — and a node with no diagnostic.data yet still
gives you the log. The archive is what [FTDC Summary](FTDC_SUMMARY.md), a **Big Hole** node or
Percona Support all want.

**Web terminals.** Drop into a root (or service) shell on any node, right in the browser —
sessions survive navigation and can be docked or floated (**Settings** picks which they open as):

![A deployed PXC node's panel — what it is, where it is, and how to open a console](screenshots/node-panel.png)

> *Every deployed node has a panel: what it actually is (version, image, ports, container),
> its generated credentials, its certificate, and a root console one click away.*

![A live per-node web terminal, querying the cluster it is running on](screenshots/terminal.png)

**Getting files onto a node.** Drag a file — or a whole folder — from your desktop onto a node
on the canvas. DBCanvas asks where to put it and copies it in; there is no scp, no bind mount
and no shell involved:

![Dropping a file on a node — DBCanvas asks which directory to copy it into](screenshots/file-drop.png)

> *A dropped file offers the destinations worth having on that node (`/`, `/home`, `/root`,
> `/tmp`), and the drop names the node so a mis-aimed drag is obvious before it happens.*

**The file manager.** Right-click a running node and choose **File manager** for a full browser
over its filesystem: navigate, upload and download, create files and folders, rename, change
permissions and ownership, delete — and **edit a file in place**, which is usually what you
actually want when a config is one line wrong.

![The File Manager browsing a node's filesystem](screenshots/file-manager.png)

> *Every running node in the stack is in the picker at the top left, so you can move between
> them without closing the window. **Split** opens a second pane on another node and copies
> between the two — the fastest way to put the same file on every member of a cluster.*

**Finding out what a setting does.** Every field on a node's panel — designing it and
after it is deployed — carries a **?** next to its label. Hover it, or tab to it, for what
the setting is for and when you would change it, rather than a restatement of the label.
The same applies to the values on a deployed node (what to *do* with that port, that
container name, that password), to every button in the toolbar, and to every entry in the
Infrastructure Library, which explains what each node type gets you before you add it.

**Reaching a node from your own machine.** On a server install, `CONTAINER_BIND_IP` keeps
every port a node publishes on the server's loopback — which is the right default and also
means your browser and your local `mysql` client cannot get to any of them. Set
`SSH_FORWARDING_HOST` in `.env` to the address the server answers SSH on (`10.0.0.7`, or
`10.0.0.7:2222`) and right-clicking a running node offers **Copy SSH tunnel command**: the exact
`ssh -L` line forwarding every port that node currently publishes, each to the same port
locally, so every address the panel shows works verbatim through the tunnel.

```
ssh -L 8443:127.0.0.1:8443 -L 8080:127.0.0.1:8080 jaime@10.0.0.7 -p 22
```

**The login is filled in for you.** With no `user@` in the value, the command uses the name of
whoever is signed in to DBCanvas — `jaime` above, because that is who asked for it. On a server
install that is usually the same person who has the ssh account, so the line is ready to paste.
Two people asking the same node each get their own account in it. Write `user@10.0.0.7` instead
to pin one login for everybody regardless of who is signed in.

The ports are read from the engine when you click, not from the design — host ports are
re-assigned every time a container restarts. Leave `SSH_FORWARDING_HOST` empty (the default)
and the menu item is absent; on a laptop install the ports are already local.

**Monitoring with PMM.** Add a PMM node and point databases at it; DB nodes register
themselves, so Percona Monitoring & Management comes up already watching the stack:

![A PMM node's panel — the server, its components, and two ways into it](screenshots/pmm-node.png)

> *PMM is one node: Grafana, VictoriaMetrics, ClickHouse, PostgreSQL, QAN and nginx in a single
> container. The panel names them, and gives a root console alongside a `pmm-admin` one.*

![Percona Monitoring & Management, already watching the services that registered with it](screenshots/pmm-web.png)

**MClusterAdmin — a MongoDB administration panel.** A node that runs
[MClusterAdmin](https://github.com/PrzemekMalkowski/mclusteradmin), a third-party web panel for
MongoDB: topology and replica-set status, sharding and the balancer, current operations, slow
queries with explain, indexes and profiling, users and roles, and oplog stats. Its web UI is
published to a host port like PMM's, so it opens straight from your browser — no VNC desktop
needed. Upstream publishes no image, so DBCanvas builds one from source at a pinned tag
(`make mclusteradmin-image`).

**You enter the connection URI in the panel itself**, which is why this node draws no association
lines: it is configured entirely through its own UI — no environment, no config file — so there is
nothing for a line to carry, and every other line on this canvas is walked by a provisioner. What
DBCanvas can do is the credentials half, below; the host is then the only thing left to type.

The accounts it connects as are a choice on the MongoDB, not on the panel: every MongoDB node and
cluster has an **Add MClusterAdmin credentials** tick. Ticking it creates both accounts when the
database deploys —

| Account | Roles | For |
| --- | --- | --- |
| `madmin` | `clusterMonitor`, `clusterManager`, `hostManager`, `dbAdminAnyDatabase`, `readAnyDatabase`, `userAdminAnyDatabase`, `read` on `local` | Every panel feature. |
| `madmin-ro` | `clusterMonitor`, `readAnyDatabase`, `read` on `local` | The monitoring dashboards. |

Those are upstream's own least-privilege sets, role for role — each is there because some panel
feature does not work without it, and both deliberately exclude `root` and `clusterAdmin`, so the
panel cannot drop a database. They go wherever the cluster keeps users: the standalone itself, the
replica-set primary, and — for a sharded cluster — the config replica set **and the primary of
every shard**. That last part matters: `mongos` authenticates against the config replica set, so
one account there is enough for anything going through the router, but the panel also connects
*directly* to each shard for its per-shard views (replica-set status, oplog, current ops,
cross-shard user sync) and prompts for credentials when it does. A shard is its own replica set
with its own users, so an account that exists only on the config servers produces "could not log
in to rs0" while everything through `mongos` works.

**Both accounts are created with SCRAM-SHA-256, and the tick also names SCRAM-SHA-256 in the
server's `authenticationMechanisms`.** That second half is what makes the first work: an account
with SCRAM-SHA-256 credentials on a server that does not advertise the mechanism is refused with
an error naming it, which reads exactly like a wrong password. (A node already running LDAP,
Kerberos or Keycloak OIDC lists both SCRAM mechanisms in its own `setParameter` block, so the tick
leaves that one alone — `mongod.conf` can carry only one, and a duplicate would stop mongod
starting.)

**The passwords live on the panel node**, which is where you log in from, so a stack with three
MongoDBs does not end up with three passwords for one account: `madmin_password` and
`madmin_ro_password` by default (`MCLUSTERADMIN_PASSWORD` / `MCLUSTERADMIN_RO_PASSWORD`),
overridable per stack on the node. The node's panel shows both, masked and copyable, next to the
URI shape to paste them into.

The panel node's **read-only panel** switch (`--view-only`) disables every write the UI can offer.
Sign in as `madmin-ro` to match it and both halves agree: the panel not offering the button, and
MongoDB refusing it if something else did.

**Big Hole — MongoDB FTDC in the browser.** A node that runs
[Big Hole](https://github.com/zelmario/Big-hole), a third-party viewer for MongoDB's
`diagnostic.data`: drag a folder — or a whole support tarball, however it is nested — onto the
page and it decodes and charts it, every metric it can find, with the replica set laid out so an
election is somewhere to land rather than something to hunt for. Upstream publishes no image, so
DBCanvas builds one from source at a pinned commit (`make bighole-image`).

It is the loosest-coupled node here, deliberately: no association line, no credentials, no
configuration, because the app talks to nothing at all — not to a database, not to an API, not
even to DBCanvas. There is no backend, and nothing you open in it leaves your browser.

> **Open it on `localhost`.** Browsers grant a page the private on-disk storage this needs only
> over HTTPS or localhost, so reaching it as `http://<host>:<port>` leaves it unable to keep a
> capture — and it fails partway through reading one, which looks like a broken decoder rather
> than a deployment mistake. The node's panel links to localhost and, when you are browsing
> DBCanvas from somewhere else, hands you the `ssh -L` line that puts you there.

For a capture from a node in this stack, take `diagnostic.data` from that node's **File Manager**
first. DBCanvas's own [FTDC Summary](FTDC_SUMMARY.md) reads the same files and answers a different
question — verdicts, findings and the handful of charts that carry them, rather than everything
plotted — so the two are worth having side by side.

**Ubuntu VNC desktop.** An optional XFCE desktop jump-box (Firefox + Percona clients)
reachable over a browser-based VNC client — handy for GUI database tools inside the stack network.
Its MySQL client is **8.4**, with the OpenID Connect client plugin, so a Keycloak user can sign in
to a Percona Server node from the desktop the same way they would from the node itself:

![The Ubuntu VNC desktop, querying a cluster node by name with the pre-installed client](screenshots/vnc-desktop.png)

> *The desktop is on the stack network, so `pxc01.example.net` resolves and the clients that
> ship in the image reach it without any setup.*

![The SeaweedFS node's Buckets tab — a read-only browser over what the databases wrote](screenshots/seaweedfs-buckets.png)

> *Inside `mongo-backups/pbm/psmrs-00`, the snapshot a MongoDB replica set in the same stack
> just wrote: `.pbm.init`, the timestamped snapshot directory and its `.pbm.json` metadata. The
> replica set's frame has **Enable PBM** ticked with this node picked as its target, which is
> all it takes. Each engine writes to its own prefix — PBM under `pbm/<cluster>`, pgBackRest
> under `pgbackrest/<cluster>`, xtrabackup and the Percona operators at the top level.*

**Diagnostics captures.** From a running node's panel, capture a diagnostic bundle and
download it: **pg_gather** (a single `GatherReport.html`) on PostgreSQL nodes,
**pt-stalk** + `pt-summary` + `pt-mysql-summary` (a tarball) on MySQL/PXC nodes, or
**pt-k8s-debug-collector** (a `cluster-dump.tar.gz`) on a **K3D** cluster's server node.
Feed a pt-stalk archive straight into **Stalk Summary** to chart it, or a cluster-dump into
**Operator Summary** to read it.

The Kubernetes capture is of the whole cluster rather than one node — every namespace's
resources, every pod's log, and the operator's own custom resources. It runs
`pt-k8s-debug-collector` in a throwaway container *beside* the cluster rather than on a k3s
node, because the collector reaches the cluster over a kubeconfig and its most useful output
is the per-pod database summary it makes by port-forwarding into a database pod and running
`pt-mysql-summary` / `pt-mongodb-summary` / `pg_gather` — which need real database clients
that a k3s node does not have. The container image is built by `make k8scollector-image`, and
is **linux/amd64 only**: Percona's apt repo publishes `percona-toolkit`, the package carrying
the collector, for that architecture alone. Each finished capture is kept on disk with a
timestamp, so a cluster has a history to compare across rather than only its latest.

## Templates — save a topology, deploy it again

A **template** is a canvas design detached from any one stack: the nodes, the clusters, the
links and every option set on them, reusable as the starting point for the next stack.

**The built-in defaults.** Eleven ship with the app, covering the engine families:

| Category | Template |
| --- | --- |
| Getting started | Starter — Percona Server (one node plus a desktop to reach it from) |
| MySQL | PXC + ProxySQL + PMM · Percona Server replication + Orchestrator · InnoDB Cluster |
| PostgreSQL | Patroni + HAProxy · PostgreSQL + pgBackRest |
| MongoDB | PSMDB replica set + PBM · PSMDB sharded cluster |
| Valkey | Valkey Cluster |
| Kubernetes | Percona Operator for MySQL (PXC) on k3s |
| All in One | Four engines in one container |

None of them pins a minor version — they take whatever this installation's `make versions`
found — so they deploy on any host regardless of which builds it probed.

**Two ways to apply one.** From the stack list, **New stack** offers a *Start from* picker
that seeds the whole design. From inside the designer, **Insert template** merges one into
what is already on the canvas — which means resolving the collisions a merge creates:

- Every node and cluster gets a fresh id, and every reference to it follows — the PMM node a
  cluster is monitored by, the SeaweedFS node a backup targets, the frame a member belongs to.
- A node the stack may only have one of (the Intranet, Keycloak, the VNC desktop) is **not
  duplicated** — the template's copy is dropped and anything pointing at it is redirected to
  the one already there.
- Labels become DNS hostnames and must be unique, so a colliding one is numbered: a second
  `pxc-1` arrives as `pxc-1-2`.
- The block is placed clear of existing nodes rather than on top of them.

Whatever it had to change is reported when the insert lands, so a rename never surfaces later
as a deploy error nobody can trace.

**Saving one.** **Save as template** in the designer's toolbar captures the open canvas.
Three classes of field are deliberately *not* saved, because a template is meant to be reused,
exported and shared:

- **Passwords and generated secrets** — root and admin passwords, the VNC password, SeaweedFS
  S3 keys, a Stock Market Sim connection string. Every one of them already falls back to
  `.env`, so the template picks up this installation's values each time it is used.
- **Host paths** — the core-dump and library directories a Linux Client bind-mounts, and any
  pinned block device. They name something about one machine.
- **Fixed host ports** — reset to auto-assign, so instantiating one template twice on a host
  does not collide on the second deploy.

**Sharing and portability.** A template you save is yours alone. An **admin** can publish one
instance-wide, and it then appears in everyone's picker alongside the built-ins. Any template —
including a built-in — can be **exported** to a `.json` file and **imported** into another
DBCanvas installation, so a topology can be checked into git or handed to a colleague. The
design is sanitized again on import, since a file from elsewhere is not to be trusted.

Built-in templates can be applied and exported but never renamed, edited or deleted.

## Deployment backends — Docker or Vagrant (hybrid)

Each user picks a **Deployment** backend in **Settings**; it applies to the *next* deploy of
each stack:

| Backend | What it provisions |
| --- | --- |
| **Docker** (default) | Every node is a Docker container on the local daemon. |
| **Vagrant (hybrid)** | OS/database nodes become real **VirtualBox VMs**; everything else stays a Docker container **in the same stack**. |

**Vagrant is hybrid-only by design — there is no all-VM mode.** Only the node types that are
really *a machine running a database* are worth the cost of a VM; the rest are upstream images
or depend on Docker itself:

| Runs as a **VirtualBox VM** | Stays a **Docker container** |
| --- | --- |
| Percona Server · PostgreSQL · PSMDB (standalone) | **Intranet** — its bind config forwards to Docker's embedded resolver (`127.0.0.11`), which only exists inside a container |
| PXC · MySQL replication · InnoDB/GR · PSMDB replica set & sharded · Patroni · repmgr · Spock · Valkey cluster · ProxySQL cluster | **K3D** — k3s-in-Docker by definition |
| Valkey · ProxySQL · HAProxy | Image-only infra: PMM, Keycloak, OpenBao, SeaweedFS, Samba AD, Ubuntu VNC, Watchtower |

Nothing is rejected: the deploy routes each node to the engine that supports it, and DBCanvas
joins the two networks on the host (iptables + routes) so a VM database still resolves the
Intranet's DNS, trusts its CA, gets scraped by PMM, and reaches SeaweedFS by name.

Each of these node types carries its own **CPUs** and **Memory (GiB)** in its properties, on
either backend: on Vagrant they size the VirtualBox VM (blank → the `DBCANVAS_VM_CPUS`/
`DBCANVAS_VM_MEMORY` defaults below), and on Docker they become the container's `--cpus` and
`--memory` limits (blank → unlimited, the daemon default).

Two things to know before you switch:

- **The backend is pinned per stack on its first deploy** and never changes for that stack's
  life — redeploys, management and teardown all stay on the engine the stack was built with.
  To try the other backend, create a **new stack**.
- **The app must run on the host for hybrid** (next section). If you select *Vagrant (hybrid)*
  while DBCanvas is running in its container — or on a host without `vagrant`/`VBoxManage` —
  the deploy silently falls back to Docker.

## Around the stacks

### Dashboard
Scope-aware overview: an **admin** sees everything, a regular user sees only their own
stacks. Counters (stacks, nodes, containers, by engine/type, users) plus **live OS stats**
(CPU, memory, and per-node network/disk rates as ranked bar charts). The live sampling is
**focus-gated** — it polls only while the dashboard tab is visible and focused, so there's
no background CPU/disk cost when you're not looking.

![The live Dashboard](screenshots/dashboard.png)

### Notifications
A live bell (Server-Sent Events) that surfaces what happens across your stacks: node
deployment failures, data-generation completed/failed, stacks destroyed or **expiring soon**
(TTL), backups completed, high resource usage, and (for admins) new accounts awaiting
approval.

### Settings
Per-user preferences, stored on the **account** rather than the browser, so they follow you to
another machine: whether a node console opens **docked** (a tab in the bottom terminal dock, the
default) or **undocked** (its own floating window), your **deployment backend**
([Docker or Vagrant hybrid](#deployment-backends--docker-or-vagrant-hybrid)), how many **tabs**
the main window will hold open at once (20 by default, 2–40 — a memory budget, since every open
tab is a live page), and the two halves
of the appearance — your **theme** (light, dark, midnight, solarized, synthwave, forest) and your
**look** (modern, industrial, editorial, soft).

A theme is the colour; a look is everything else about the feel — the typeface, the size it is all
set at, corner radius, border weight, elevation, and the chrome on cards, buttons and tables. They
are chosen independently, so every look works in every theme:

| Look | What it is |
| --- | --- |
| **Modern** | The default. A neutral sans, rounded cards, soft shadows, comfortable rows. |
| **Industrial** | An instrument panel. Mono upper-case headings, buttons and inputs, square corners, flat surfaces, card title bars, zebra-striped tables, and the smallest and tightest rows — the most data per screen. |
| **Editorial** | A printed report. A book serif throughout with sans controls, hairline rules, a double rule under each card title, generous whitespace, set largest. |
| **Soft** | A rounded humanist face, with buttons, inputs and badges as full pills, borders faded back and elevation doing the layering. |

Either can be switched from the sun icon in the top bar as well as from Settings; each option
there is previewed in the look it offers. Every typeface is a system face — a lab with no route
out to a font CDN gets the same looks as one with.

### Manage Users (admin)
Registration is approval-gated: admins approve, reject, disable, re-approve, and delete
accounts.

**Locked out?** The image ships a password-reset tool, because the runtime is distroless —
no shell, no `sqlite3` — and the database lives on a volume only that container mounts:

```bash
docker exec -it dbcanvas-app-1 dbcanvas_reset_password
```

It prompts for a new password and a confirmation (echo off), names the admin it is about to
change, and signs out that account's existing sessions. With more than one admin, name one
with `-user`.

---

See also: [Configuration & commands](CONFIGURATION.md) · [Architecture](ARCHITECTURE.md)
