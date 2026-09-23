# DBCanvas — Database Interaction Lab

Design a database topology on a canvas, click **Deploy**, and get real running nodes — wired
together with DNS, TLS, LDAP, replication, monitoring and backups. Then use the tools that
come with it to load those databases, watch them work, and find out why they do what they do.

Built for testing, demos, training, troubleshooting, benchmarking and application
development: spin up a production-shaped cluster in minutes, exercise it, tear it down.

> [!IMPORTANT]
> **DBCanvas is for previewing, testing and learning — not for production.** Everything it
> deploys is disposable lab infrastructure: default credentials from a `.env` file, containers
> that trade durability for speed of setup, and stacks that are meant to be torn down. Do not
> run anything you care about on it, and do not use it to deploy production infrastructure.

![The Database Stacks canvas with a deployed stack](docs/screenshots/stacks-canvas.png)

---

## Install

Requires **Docker**, with access to its daemon socket.

```sh
git clone https://github.com/jaimesicam/dbcanvas.git && cd dbcanvas
make install
```

That builds every image DBCanvas can build, records what each of them can install, and starts
DBCanvas at **http://localhost:8080**. The first run takes a while — it is building
operating-system images, and then the tool and demo images on top of them — and later runs are
just `make compose`. An image that fails to build (they fetch from npm, GitHub and Percona's
repositories) is reported and skipped: DBCanvas still starts, and only the node types that
need it are affected.

Open the URL and create an account: **the first one becomes the administrator.** Anyone who
signs up afterwards waits for an admin to approve them.

Everything has a working default. Before exposing DBCanvas beyond your own machine, change
the passwords in `.env` — see [Configuration](docs/CONFIGURATION.md).

## Your first stack

Go to **Database Stacks** → **New stack**, name it, and pick something under **Start from**.
Eleven templates ship with the app, one per engine family.

![The New stack dialog with the PXC + ProxySQL + PMM template selected](docs/screenshots/getting-started-new-stack.png)

The whole topology lands on the canvas, editable. Press **Validate** to check the design
without building anything, then **Deploy** and watch the node cards turn `running`.

![A deployed stack — Intranet, PMM and a Percona Server node](docs/screenshots/getting-started-deployed.png)

**Building one by hand?** Add the **Intranet** node first — it provides the DNS, certificate
authority, LDAP and package proxy every other node assumes exists, which is why the rest of
the library stays greyed out until it is there.

**Set the stack's lifetime** to the shortest thing that covers what you are doing. Lab stacks
are easy to forget; DBCanvas tears them down for you when the TTL elapses.

→ **[Getting started](docs/GETTING_STARTED.md)** walks the whole thing end to end.

## Operating what you built

**Right-click any running node** for a root console in the browser, a file manager, the
`docker exec` line for your own terminal, start/stop/restart, and — on a server install — an
`ssh -L` line that brings its ports to your machine.

![The right-click menu on a running node](docs/screenshots/getting-started-node-menu.png)

**Click a node** for its panel: what it actually is, where it is on the network, its
credentials and certificate, and per-engine management — replication, users, backups,
diagnostics captures.

**Four ways to connect**, in the order you will want them:

| | |
| --- | --- |
| **The web terminal** | Right-click → *Enter root console*. Root inside the node, client already installed, nothing to set up. |
| **From another node** | Every node resolves every other by name: `mysql -h ps-01.example.net -u root -p`. The **Ubuntu VNC** desktop is this with a browser in it. |
| **From your own machine** | Tick *Export … port to the host* before deploying; the panel then shows which host port it landed on. |
| **Through an SSH tunnel** | For when DBCanvas runs on a server — set `SSH_FORWARDING_HOST` and copy the line from the node's menu. |

**Anything with a `?` next to it explains itself** — every field, every value on a deployed
node, every toolbar button and every entry in the node library. Hover it, or tab to it.

## What you can build

**Databases**, standalone or clustered, on Oracle Linux, Rocky, Alma, Debian or Ubuntu:

- **PostgreSQL** — standalone, Patroni HA, repmgr, Spock multi-master, and CloudNativePG or
  Crunchy PGO on Kubernetes
- **MySQL** — Percona XtraDB Cluster, Percona Server, MySQL Community, asynchronous
  replication, InnoDB Cluster / Group Replication, and MariaDB (standalone, replication,
  Galera)
- **MongoDB** — Percona Server for MongoDB: standalone, replica set, sharded, with an
  optional **MClusterAdmin** web administration panel and a **Big Hole** FTDC viewer beside them
- **Valkey** — standalone and cluster

**The infrastructure around them**: an Intranet node (DNS · mail · OpenLDAP · Squid proxy ·
a certificate authority), PMM, ProxySQL, HAProxy, Orchestrator, SeaweedFS S3, Keycloak,
OpenBao, Samba AD DC, Watchtower, MClusterAdmin, Big Hole, an Ubuntu VNC desktop, and a
Kubernetes cluster frame that runs any of six database operators.

**Load to run against them**: app simulators that drive continuous, realistic traffic — a
hotel booking system, an airline, a car rental fleet, and a stock exchange you operate
yourself.

## What you can do with it

| | |
| --- | --- |
| [**Stacks**](docs/STACKS.md) | Design on a canvas, deploy, and manage every node from its own panel — terminal, files, credentials, certificates, replication. |
| [**Data Generator**](docs/DATA_GENERATOR.md) | Fill tables with realistic data, at the scale it takes to see a problem. |
| [**Query Runner**](docs/QUERY_RUNNER.md) | Run SQL across nodes in parallel, gated on the processlist. |
| [**Database Explorer**](docs/DATABASE_EXPLORER.md) | Browse schemas, collections and keys; run SQL, MongoDB queries and Valkey commands; inspect results as tables, documents or charts — including read-only access to PMM's internal PostgreSQL and ClickHouse data. |
| [**Benchmark**](docs/BENCHMARK.md) | OLTP, OLAP, read-write and read-only workloads, with throughput and latency. |
| [**Sample Client Code**](docs/SAMPLE_CODE.md) | Runnable client code for a deployment on the canvas — pick an endpoint, a language and a driver; DBCanvas installs what it needs and runs it. |
| [**Packet Inspector**](docs/PACKET_INSPECTOR.md) | Capture on a node and decode MySQL, PostgreSQL, MongoDB and Valkey off the wire. |
| [**Log Summary**](docs/LOG_SUMMARY.md) | Several nodes' logs on one timeline, split into the good, the warning and the bad. |
| [**Stalk Summary**](docs/STALK_SUMMARY.md) | Turn a pt-stalk capture into charts, and say which variables to change. |
| [**FTDC Summary**](docs/FTDC_SUMMARY.md) | Read MongoDB's own black box — the diagnostic data every mongod already writes. |
| [**Operator Summary**](docs/OPERATOR_SUMMARY.md) | Read a pt-k8s-debug-collector cluster-dump — what is not running, and what the operator says about it. |
| [**Operator Debugger**](docs/OPERATOR_DEBUGGER.md) | Step through the Kubernetes operator itself — breakpoints, stack and variables, no IDE. |
| [**Core Dump Analyzer**](docs/CORE_DUMP_ANALYZER.md) | Read a `mysqld` core dump from another server — threads, stack, arguments. |
| [**All in One**](docs/ALL_IN_ONE.md) | Many database instances in one node, for when you need versions side by side. |
| [**HTTP API**](docs/API.md) | Every one of the 294 endpoints, with tokens you create and expire yourself. |
| [**`dbcanvas-cli`**](docs/CLI.md) | Sign in once, then compose, deploy and drive stacks from your terminal. |

## Documentation

- **[Getting started](docs/GETTING_STARTED.md)** — install to running cluster, end to end
- [Feature guides](docs/README.md) — the tools above, one page each
- [Stacks](docs/STACKS.md) — every node and cluster type, and everything the canvas does
- [HTTP API](docs/API.md) — tokens, scopes, the endpoint catalogue, OpenAPI
- [API & CLI reference](docs/API_REFERENCE.md) — every feature, its endpoints, and the
  equivalent CLI command
- [`dbcanvas-cli`](docs/CLI.md) — install, sign in, every command
- [`.claude/skills/dbcanvas/`](.claude/skills/dbcanvas/SKILL.md) — an Agent Skill for
  driving DBCanvas: the safe path through the API and the CLI. It loads by itself for
  anyone working in this checkout; to use it from anywhere else, copy it into your own
  skills directory:

  ```sh
  mkdir -p ~/.claude/skills/dbcanvas
  cp .claude/skills/dbcanvas/SKILL.md ~/.claude/skills/dbcanvas/
  ```
- [Configuration & commands](docs/CONFIGURATION.md) — `.env`, every `make` target,
  troubleshooting, recovering an admin password
- [Architecture](docs/ARCHITECTURE.md) — how it is wired, and why

## Requirements

- **Docker**, with access to the daemon socket. DBCanvas drives the daemon to create your
  stacks, which is a privileged capability — run it somewhere you trust.
- Enough resources for what you deploy: a full HA cluster is several containers.
- Linux recommended; also runs on macOS and Windows Docker, including Apple Silicon.
- **k3d** only for Kubernetes frames (the app image ships it), and **Vagrant + VirtualBox**
  only for the hybrid backend, where database nodes are real VMs instead of containers.

---

## What's new

### 0.0.10

<details open>
<summary><b>PgBouncer — connection pooling for the PostgreSQL family</b></summary>

A **PgBouncer** node pools for one backend drawn on the canvas: a standalone PostgreSQL node, or a
**Patroni**, **repmgr** or **Spock** cluster. It sits beside HAProxy rather than replacing it —
HAProxy balances TCP and leaves a hundred clients as a hundred backend processes; PgBouncer
terminates the protocol so they share a few dozen server connections.

A pooler has no health checks, so the node **follows the primary itself**: a timer asks the cluster
who takes writes — Patroni's REST API, `pg_is_in_recovery()` on repmgr — and reloads the pool, so a
failover never drops a client. Pick the **pool mode**, the **read/write routing** (a named `_ro`
pool on a standby, or one pool per Spock member) and, once the node has a certificate,
**certificate authentication**. PostgreSQL nodes and frames gained an ordered `pg_hba` method list
to match, so one server can take a certificate and a password at the same time.

The Car Rental Sim, the Stock Market Sim and the Ledger Sim can all drive a database through the
pool, and each is warned about the one thing transaction pooling breaks: prepared statements
cached per connection. `dbcanvas stack compose` builds it as `pgbouncer`.

[Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>Data-at-rest encryption for PXC clusters and Percona Server replication</b></summary>

The standalone Percona Server node could keep its keyring in **OpenBao**; a cluster could not,
which is backwards — a three-node cluster is where a real keyring deployment is interesting. Tick
**Encrypt with OpenBao** on a **PXC** frame or a **Percona Server replication** frame and every
member is wired to it: `component_keyring_vault` on 8.4, the `keyring_vault` plugin on 8.0, and a
KV mount of its own for each server, because Percona is explicit that a `secret_mount_point` must
serve exactly one instance. That works inside a cluster because keys are never what travels
between members — write-sets and binlog events carry rows, and a PXC joiner's SST re-encrypts the
donor's tablespace keys under a master key it generates for itself.

An encrypted PXC cluster **encrypts its cluster traffic** as well, because PXC does not leave that
optional: with a keyring configured its SST script refuses an unencrypted channel outright, so a
keyed cluster without it bootstraps one member and never adds a second. It gets one certificate
from the Intranet CA — identical on every member, which is what PXC requires — staged before the
first member starts.

The keyring is **staged before each member's first start** rather than added afterwards, which is
the difference between configuring a cluster and restarting members out of it one at a time. Two
bugs fell out of doing it that way: the old path appended `early-plugin-load` to `/etc/my.cnf`,
a file nothing reads on Ubuntu, and OpenBao only published its own DNS record at the *end* of its
provisioning — so a stack of nothing but an Intranet and an OpenBao node could never finish, and
a database waiting for OpenBao deadlocked against it. The deploy also proves the result rather
than asserting it: every member is checked for a loaded keyring, and the writable one creates and
drops a real encrypted table, which is the operation that stores a master key in OpenBao.

[Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>Sample Client Code in C#</b></summary>

C# joins Python, Node.js, Go, Java and the shell: **MySqlConnector** for the MySQL family,
**Npgsql** for PostgreSQL, the **MongoDB C# Driver** and **StackExchange.Redis** for Valkey — every
scenario and every TLS posture the other languages have, mutual TLS included.

The .NET SDK comes from each Linux release's own archive where it has one — .NET 10 on Oracle
Linux 8, 9 and 10 and Ubuntu 24.04, .NET 8 on Ubuntu 22.04 — and on Debian, which packages none,
from Microsoft's SDK archive with its checksum pinned and no repository added. The generated
project targets .NET 8 and rolls forward, so the same project runs on either. It was run on all
seven releases before it shipped: every client, every scenario.

[Sample Client Code →](docs/SAMPLE_CODE.md)
</details>

<details>
<summary><b>The Core Dump Analyzer shows every thread, opens values, and names the line that faulted</b></summary>

**All threads** is `thread apply all bt` with the two things that command cannot do: identical
stacks fold together — twenty idle workers in the same wait become one row saying `20×` — and each
carries its real depth. **full** lists every frame's arguments and locals under it.

**Values open**: a struct into its fields, a pointer into what it points at, each with the
expression to paste into the console. The **source pane** works on real builds, whose recorded
paths run through directories that only existed on the build machine. And the **verdict** reads
the address the process touched from the core itself — null, a null plus a field offset, or never a
pointer — matches it to the faulting frame's variables, and shows that line of source with their
values.

[Core Dump Analyzer →](docs/CORE_DUMP_ANALYZER.md)
</details>

<details>
<summary><b>Check for updates, when you ask and not otherwise</b></summary>

The dashboard has a **Check for updates** button, and it is the only thing in DBCanvas that
contacts GitHub — nothing checks at startup or on a timer, so an installation nobody clicks it on
never makes a request off the machine. When there is a newer version it opens the release notes for
every version between this one and the latest, so skipping a release does not hide what was in it.
`dbcanvas updates` is the same check from the command line.

[API reference →](docs/API_REFERENCE.md)
</details>

<details>
<summary><b>Exported templates no longer carry the MongoDB Cluster Admin passwords</b></summary>

Exporting a template scrubs every secret from the design, and two were missing from the list: the
admin and read-only passwords of an **MClusterAdmin** node, so a template exported from a stack with
one carried a live MongoDB password with it. Both are scrubbed now. **A template exported before
this release may still hold them** — if you shared one, change those passwords on the stack it came
from.
</details>

### 0.0.9

<details open>
<summary><b>Upgrading to 0.0.9 — rebuild the Stock Market Sim and the Oracle Linux 10 image</b></summary>

Two of this release's fixes live **inside images** rather than in DBCanvas itself, so `git pull`
alone does not deliver them:

```sh
make stocksim-image   # the Stock Market Sim: primary-following and the App health panel
make images           # the Oracle Linux 10 base: the EL10 MySQL install fix
```

The Stock Market Sim's new behaviour is compiled into that app, so a node deployed from the old
image keeps the old behaviour. The EL10 repair is a line in the Dockerfile — the distro's
`perl-DBD-MySQL` was dragging the distro's MySQL libraries into the image, which is what stopped
Percona Server and PXC installing there. Rebuilding only `oraclelinux-10` is enough if you would
rather not rebuild everything.

Everything else takes effect on restart. **Nodes already deployed are not changed by any of
this** — redeploy the ones you want the new behaviour on.
[Getting started →](docs/GETTING_STARTED.md)
</details>

<details>
<summary><b>Data-at-rest encryption for the PXC and MongoDB operators</b></summary>

The PostgreSQL operator could encrypt at rest and the other two could not, which was an odd place
for the line to fall. Both now offer it from the same **OpenBao** node on the canvas, with their
own KV v2 mount and a token scoped to it — never OpenBao's root token, which is what both
operators' own examples use.

PXC gets a Secret holding `keyring_vault.conf`; MongoDB gets the two halves the operator keys off
separately, and a sharded cluster's replica set and config servers get **separate keys**, because
they are separate WiredTiger deployments and one shared key path has whichever starts second
overwrite the first's.

The keyring file's format follows the **server** version, not the operator: PXC 8.4 reads it as
the component's JSON and 8.0 as the plugin's `key=value`, and the wrong one crash-loops every pod
before the cluster forms. [Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>A repmgr cluster you can actually switch over, and a tab that tells you how</b></summary>

`repmgr standby switchover` is the one repmgr operation that is not a database operation — it has
to stop PostgreSQL on the *other* machine — so it shells out to SSH, and a cluster without it
stopped at *"unable to connect via SSH to host …, user"*.

Every repmgr cluster now gets its own keypair at deploy, with the public half in every member's
`authorized_keys` **including its own**, so switchover works in whichever direction you choose.
`repmgr.conf` gets the matching `ssh_options` and the service commands that make it stop
PostgreSQL with `systemctl` rather than the `pg_ctl` systemd would undo.

Underneath was a second fault: the base images trim the systemd unit whose only job is removing
`/run/nologin`, so PAM refused every non-root login for the life of the container.

Each member also gets a **repmgr tab** — `cluster show`, `node check`, the switchover dry run
before the real one, promote/follow/rejoin, and repmgrd control — with the config path, the binary
and the peer list already filled in. [Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>A repmgr cluster picks its backup engine</b></summary>

repmgr could only back up with barman-cloud, which was an odd place for the choice to be made:
every other PostgreSQL kind here uses pgBackRest, so the one cluster type whose subject is
controlled failover was also the one whose backup tool differed from everything you would compare
it against.

The frame now offers either, and the trade is one line — **pgBackRest** is what the standalone and
Patroni clusters use, but its S3 client only speaks HTTPS, so the SeaweedFS node needs S3 TLS on;
**barman-cloud** works against a plain-HTTP store. Both at once is refused, because PostgreSQL has
a single `archive_command` and one would silently win.

Either survives a switchover: the standbys inherit the archive command when they clone, so an
incremental taken from the new primary references the full backup the old one took.
[Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>Percona Server installs on Oracle Linux 10</b></summary>

It did not, and for two unrelated reasons with one symptom.

The **base image was carrying the distro's MySQL**: `percona-toolkit` needs `perl(DBD::mysql)`, the
EL10 Percona Toolkit repo does not build it, so dnf took the distro's build — which links
`libmysqlclient` and drags `mysql8.4-libs` in behind it, deadlocking every Percona Server and PXC
8.4 or 9.7 install.

Separately, **Percona's own 8.0 el10 build** still carries unversioned `Obsoletes` on
`mariadb-server` and friends, which on EL10 resolve to the renamed `mariadb11.8` packages and
collide over `/var/lib/mysql`.

The first is fixed in the image and needs `make images`; the second at install time, for EL10
only. PXC does not need the second — there is no PXC 8.0 el10 build to hit it.
[Stacks →](docs/STACKS.md)
</details>

<details>
<summary><b>The Stock Market Sim follows a cluster's primary, and says when it is stuck</b></summary>

Pointed straight at a repmgr or Patroni cluster, the sim was given the member that happened to be
primary at deploy. After a switchover it reconnected to that same host — now a read-only standby —
and every write failed while every read kept working, so the dashboard drew a live-looking market
that had not written a row in an hour.

Its DSN now names **every member** and asks for the one accepting writes, and the store drops its
pooled connections if it ever finds itself on a standby.

The dashboard gained an **App health** panel for the same reason the failure went unnoticed:
writes/s and reads/s, an error count with **when it last happened** and a button to clear it, and
a `stalled` flag that turns red within ten seconds whatever the cause. The simulation's writes are
counted apart from backfill's bulk history, which otherwise buries them.
[Stacks →](docs/STACKS.md)
</details>

### 0.0.8

<details open>
<summary><b>Every member of an operator's replica set, not just the one in front</b></summary>

A MongoDB cluster an operator deployed **without a router** was invisible to all four database
tools, however it was exposed. Its members are published one Service per pod — `k3d-03-rs0-0`,
`-1`, `-2` — and the name-matching that recognises an operator's Services had no pattern for that,
so a replica set with three LoadBalancer addresses on the stack's own subnet contributed nothing,
while a sharded cluster beside it was visible through its `mongos`.

A Service is now recognised **by the pod it selects** rather than by the shape of its name, which
holds for any operator and any replica set name.

**Every member is offered, not only the one that can take writes.** A secondary answers reads on
its own address exactly as the primary does — every MongoDB path here dials with
`directConnection=true` — and a write sent to the wrong one is refused by the server in as many
words. Which member is primary is deliberately not recorded: it changes on failover, and a cached
answer would be a confident wrong one. [Database Explorer →](docs/DATABASE_EXPLORER.md)
</details>

<details>
<summary><b>The Data Generator and the Benchmark reach operator MongoDB</b></summary>

Both tools could see an operator's MongoDB and neither could use it.

The **Data Generator** required the exec route and so offered PostgreSQL only — a limit that came
from how its SQL engines run a client inside the pod, and which never applied to its MongoDB
backend, which dials with the driver over the stack network like every other load tool here.

The **Benchmark** built its MongoDB connection from a container id, which a Service does not have,
so every run died in preparation with *"could not resolve node address"*.

Both now take the address off the endpoint. The **Query Runner**, which is SQL-only and says so,
no longer lists MongoDB endpoints it would refuse at Run. [Data Generator →](docs/DATA_GENERATOR.md)
</details>

<details>
<summary><b>Sample Client Code runs on every supported Linux release</b></summary>

Twenty-three samples across seven base images is a hundred and sixty-one programs, and a third of
them did not compile or connect.

Almost all of it came from one thing: **the environment plan knew the distribution but not the
release**, so Oracle Linux 8 and 10 were handed the same package list, and every check asked
whether a binary existed rather than whether it was new enough. EL8's module streams hid Percona's
own clients behind modular filtering and pinned Python at 3.6 and Node at 10. Ubuntu 22.04's
default JDK is 11 while the generated pom compiles at 17. The drivers' own `go.mod` files require
Go 1.24, which is newer than Debian 12 or Ubuntu 22.04 ship.

The plan now reads the release, and **every check asks the question the build will ask** — whether
`javac` can target 17, whether `node` can parse the syntax the drivers use. Where a distribution
cannot answer at all, the runtime comes from the project that publishes it, pinned to a version and
a SHA-256 per architecture, **with no third-party repository added to the node**.
[Sample Client Code →](docs/SAMPLE_CODE.md)
</details>

### 0.0.7

<details open>
<summary><b>Database Explorer — a database client for the stack you already deployed</b></summary>

A browser-based client for the databases on your canvas, and the point of it is that you never tell
it anything. DBCanvas provisioned them, so it already holds the address, the port, the account and
the password, and it reaches them over the stack's own Docker network — **no port has to be
published to your host, and there is no connection dialog**.

**Five engines, five adapters.** MySQL (Percona Server, PXC, MySQL Community, MariaDB, and the
HAProxy / ProxySQL / MySQL Router endpoints in front of them), PostgreSQL (Patroni, repmgr, Spock),
MongoDB, Valkey and ClickHouse. The tree offers the endpoint a client should actually use —
HAProxy's write port rather than one PXC member, `mongos` rather than a shard, the replica set
rather than a member — with the members listed underneath, because connecting to one on purpose is
most of what a lab is for.

**MongoDB is not dressed up as SQL.** A find has a filter, a projection and a sort; an aggregation
is a pipeline; inputs are Extended JSON so `{"_id": {"$oid": …}}` means what it says. Results travel
twice — as documents with their nesting intact, and as a flattened table so the grid, the chart and
the exports work — and the flattening never replaces the documents.

**Valkey browses with `SCAN`, never `KEYS`,** with a viewer per data type (hash, list, set, sorted
set, stream) and a console that renders a reply structurally instead of dumping protocol. Typing
`KEYS` gets a refusal that explains itself.

**A result grid built for looking at data.** `NULL` is visibly not the empty string — a distinction
that survives sorting, filtering and both exports. Binary is a length and a hex head, not mojibake.
A `BIGINT UNSIGNED` past 2⁵³ keeps every digit. Fifty thousand rows scroll like fifty. Every result
is capped, every query has a timeout, and Cancel stops the work at the database.

**And a chart from any result with a number in it** — bar, line, area, pie, scatter, histogram —
which says so in its header when the numbers on it are the browser's grouping rather than the
query's. [Database Explorer →](docs/DATABASE_EXPLORER.md)
</details>

<details>
<summary><b>PMM Server's own PostgreSQL and Query Analytics, read-only</b></summary>

A PMM Server carries two databases that are extremely useful and normally invisible: `pmm-managed`'s
inventory — every node, service and agent PMM believes exists — and the ClickHouse behind Query
Analytics. Both listen on `127.0.0.1` inside the container, so DBCanvas reaches them by running
their own clients in there and asking for machine-readable output rather than the decorated tables
they print by default.

**Read-only at the database, not by a keyword filter.** PostgreSQL runs every statement inside
`BEGIN READ ONLY` and refuses a write with SQLSTATE `25006`; ClickHouse runs with `readonly=2`,
refuses one with error `164`, and refuses to modify that setting on itself. DBCanvas also declines
the statement before sending it, which gives a better message and is defence in depth — it is
deliberately not the control.

**Writable when a scenario needs it.** An administrator can allow writes to the internal databases
for the installation, and even then a query tab must be armed for it, with a red banner while it is.
A tab left open from before the setting changed cannot write into PMM by pressing Run.
[PMM Server →](docs/DATABASE_EXPLORER.md#pmm-server)
</details>

<details>
<summary><b>The four database tools can target Kubernetes operator clusters</b></summary>

The databases a Percona, CloudNativePG or Crunchy operator deployed inside a K3D frame are now
targets for the **Database Explorer**, the **Data Generator**, the **Query Runner** and the
**Benchmark**. Nothing is read off the canvas: the Services, the pods and the credentials come from
the cluster itself and the operator's own Secrets.

A **LoadBalancer** Service is dialled directly — MetalLB's address comes out of the stack's own
Docker subnet — and a **ClusterIP** one, which is the operator default and has no address outside
the cluster at all, is read by running the database's client inside its pod. The Query Runner and
the Benchmark open many connections and time them, so for them there is **Expose for tools**: a
Service added *beside* the operator's own, never over it, and removed again as easily.

> [!NOTE]
> This one is new and wants more use before it is trusted. It has been exercised against Percona
> PostgreSQL Operator clusters; the PXC, PS, PSMDB, CloudNativePG and Crunchy paths are written from
> each operator's own conventions but have not been run against a live cluster of each.
> [Kubernetes clusters →](docs/DATABASE_EXPLORER.md#kubernetes-clusters)
</details>

### 0.0.5

<details open>
<summary><b>Kubernetes States — a cluster as a board that keeps what died</b></summary>

A new page beside **Operator Summary**, and the opposite tense: that one reads a capture after the
fact, this one watches a cluster now. One column per kind, one card per object, and three things a
table cannot do.

**It colours what is wrong.** A card is red because the object says it is broken — a `Failed` pod, a
container in `CrashLoopBackOff`, a `NotReady` node, a workload with zero ready replicas — and the
property row that caused the colour is red inside it. A completed backup pod is *finished*, not
failed, because a board full of red is a board nobody looks at.

**It lights what moved.** A value that changed since the last sample stays lit for a few seconds and
says what it changed from: `Ready 2/3 (was 3/3)` is a failover, visible without reading anything.

**It keeps what died.** An object missing from a sample is not removed — it stays where it was,
greyed and dashed, until you dismiss it, because the pod that vanished while you were looking at
another card is the one you needed to see.

Beside the board is a rail of panes: an object's **State**, a container's **Logs** (opening on
whichever container is unhealthy, and able to read `--previous` — the only log a `CrashLoopBackOff`
has anything in), and its **YAML**. Any of them can be **pinned**, so the custom resource's state
and the crashing container's log stay on screen while you click through everything else. Reach it
from the menu, or from **Watch Kubernetes states** on a Kubernetes server node.
[Kubernetes States →](docs/KUBERNETES_STATES.md)
</details>

<details>
<summary><b>The same board reads a pt-k8s-debug-collector cluster-dump</b></summary>

Point it at a capture kept by **Diagnostics**, or **upload a cluster-dump** from your machine — a
customer's cluster this installation has never seen reads exactly the same, because the collector's
YAML and `kubectl`'s JSON are the same objects in two encodings. Every rule above applies unchanged:
the same pods come out red for the same reasons, with the same warning events hanging off them.

Every object in the archive becomes a card, including the kinds a live sample leaves alone, so a
capture of an operator DBCanvas has never heard of still comes out complete — the noisy ones start
folded with their count on a chip rather than dropped. A pod offers **what the collector kept for
it**, which is a great deal more than `kubectl logs` would have given you: the logs,
`pt-mysql-summary`'s output, and on a PXC pod the **`innobackup.*.log` backup logs** or on a PG pod
**pgBackRest's own**. **Capture files** lists every file in the archive — certificates and all — so
the answer to *"is there anything in here the board is not showing me"* is no.

And it says what a capture cannot be. A cluster-dump is **one instant**: nothing is changing and
nothing has disappeared, and the board says so rather than implying otherwise. It reports what the
collector itself failed to collect, from the archive's own `errors.txt`. Tick **compare** before
loading a second capture and the whole thing becomes a diff of the first — what changed is lit, what
is gone is a tombstone. Uploads are held **in memory, for an hour, for the account that uploaded
them**, and never written to disk.
[Kubernetes States →](docs/KUBERNETES_STATES.md)
</details>

<details>
<summary><b><code>make install</code> no longer re-probes every repository it just deleted</b></summary>

A first run took people upwards of three hours, and most of it was spent rebuilding a catalog it had
thrown away seconds earlier. `make images` wrote `versions.yaml`, so every image rebuild discarded
everything `make versions` had probed — and `make install` had to run the probe again to get back
what it had just deleted.

The two files are now separate: **`images.yaml` is what was built**, **`versions.yaml` is what is
installable on it**, and neither overwrites the other. `make install` no longer runs the probe at
all, so the catalog committed to this repo survives and the version pickers are populated the moment
DBCanvas comes up. Run `make versions` deliberately — when you want minors released since the last
probe, or have built an OS image that was not there before. An installation that predates the split
keeps working: its image entries are still read out of `versions.yaml` until the next `make images`.
[Getting started →](docs/GETTING_STARTED.md)
</details>

### 0.0.4

<details>
<summary><b>The Stock Market Sim can split its reads to HAProxy's read port</b></summary>

Every simulator resolved an **HAProxy** target to one endpoint — the write port — so a Patroni or
repmgr cluster behind HAProxy took the whole query load on its primary and the replicas sat idle.
HAProxy's read port was configured, published and documented, and nothing ever connected to it.

Tick **Send reads to HAProxy's read port** on a Stock Market Sim linked to an HAProxy node and the
queries that only display — the dashboard, the lists, the report — go to `:5001`, which round-robins
the replicas, while writes keep `:5000`. A read whose answer decides a write stays on the primary
whatever the setting: the row a PUT is about to change, the portfolio an order is placed against.
Otherwise replication lag turns into a write that fails for a reason nothing on screen explains.
For every HAProxy-fronted cluster, PostgreSQL and MySQL alike.
[Simulators →](docs/STACKS.md)
</details>

<details>
<summary><b>A file manager for SeaweedFS buckets</b></summary>

The SeaweedFS node's **Buckets** tab could show you that a backup landed, and nothing else. Now
**Files…** on that tab — or **Bucket file manager** on the node's right-click menu — opens a
two-pane file manager over the buckets themselves.

**Download** an object to your machine. **Upload** files into a folder, by drag-and-drop or from
the button, under the same size ceiling as a node file drop. **Delete** what you no longer want —
it asks first, and takes a folder's contents with it only when the confirmation says so, because a
folder here is a whole backup. Open the second pane and **copy objects from one bucket into
another**, on the same node or on another SeaweedFS node in the stack — streamed container to
container, so nothing lands on the DBCanvas host on the way. What you write is an ordinary S3
object: a database node's `aws s3 ls` sees it with the key, size and ETag you would expect.
[SeaweedFS →](docs/STACKS.md)
</details>

### 0.0.3

<details>
<summary><b>Edit cr.yaml as a form, generated from the operator's own CRD</b></summary>

A Kubernetes server node's panel has a **cr.yaml** tab: the live custom resource as a form built
from the CustomResourceDefinition *that cluster is running*. Nothing is hand-written, so it offers
what this operator version accepts rather than a fixed list that goes stale a release later — and
the whole resource is reachable, with the parts no form can usefully draw (affinity, tolerations,
sidecars) kept as JSON boxes rather than dropped.

Search across every section at once — nobody browses to `backup.pitr.timeBetweenUploads` — and edit
with controls the schema chooses: switches, pickers, bounded numbers, repeatable entries for backup
schedules and storages. **Nothing is sent until you say so.** The footer counts the pending changes,
**Review patch** shows the exact merge patch, **Check** validates it against the API server without
changing anything, and only **Apply** writes. For the four Percona operators.
[Kubernetes frames →](docs/STACKS.md)
</details>

<details>
<summary><b>Point-in-time recovery</b>, two object stores, and kubectl on a Linux Client</summary>

A PXC-operator cluster can run the operator's **binlog collector**, with a **bucket of its own** for
the binary logs — two clusters uploading into one bucket interleave two streams that neither can
replay afterwards. On the **replica end of a replication link it starts switched off**, whatever the
frame says: the seed restore replaces the replica's data and its whole GTID history, so DBCanvas
turns the collector on only once replication is actually *running*, and switches it off again before
any re-seed.

A replication pair may now use **one SeaweedFS node each** — the shape two sites really have. The
restore is handed the source's endpoint by the backup itself, and the source store's credentials are
copied into the replica's cluster under a name of their own.

And a **Linux Client** can be deployed with **kubectl and Helm** already on it, on PATH with
completion and the `k` alias. kubectl is matched to the k3s release of a Kubernetes frame on the
same canvas, because it is only supported one minor version either side of the API server — the one
thing "install the latest" gets wrong, and gets more wrong the longer a stack lives.
[Kubernetes frames →](docs/STACKS.md)
</details>

<details>
<summary><b>Replicate one Kubernetes cluster into another</b></summary>

Draw a link between two **Kubernetes frames that both run the PXC operator**, pick a direction,
and press **Deploy**: the second cluster becomes a replica of the first. It is the one link on the
canvas that joins two *frames* rather than two nodes, because a Kubernetes cluster's identity here
is its frame.

What runs is Percona's own
[*Restore to a new cluster*](https://docs.percona.com/percona-operator-for-xtradb-cluster/latest/backups-restore-to-new-cluster.html)
and [*cross-site replication*](https://docs.percona.com/percona-operator-for-xtradb-cluster/latest/replication.html),
in the order they have to happen in. The source's `cr.yaml` declares the channel and its database
pods each take a LoadBalancer address — a replica in another cluster cannot reach a ClusterIP.
Both clusters build **at the same time**; the replica waits for nothing. Once both are ready a
backup of the source is taken, restored onto the replica, and **only then** is the replica's own
channel attached: attach it any earlier and it asks the source for binary logs it has already
purged, and replication stops with error 1236 instead of starting.

The server node's **Replication** tab shows which end a cluster is, what it reads from, the backup
it was seeded from, and whether the channel is actually running. Pressing Deploy again reconciles
the channel and never re-seeds — a seed replaces the replica's data, so that is a button of its
own. [Kubernetes frames →](docs/STACKS.md)
</details>

<details>
<summary><b>MClusterAdmin</b> — a MongoDB administration panel</summary>

A node that runs [MClusterAdmin](https://github.com/PrzemekMalkowski/mclusteradmin), a
third-party web panel for MongoDB: topology and replica-set status, sharding and the balancer,
current operations, slow queries with `explain`, indexes and profiling, users and roles, and
oplog stats. Its UI is published to a host port like PMM's, so it opens straight from your
browser — no VNC desktop needed. It runs upstream's own published image, pulled at deploy the way
PMM's is (amd64 only, which is what upstream builds).

Every MongoDB node and cluster has an **Add MClusterAdmin credentials** tick, which creates the
two accounts the panel expects when the database deploys: `madmin` for everything it does, and
`madmin-ro` for the monitoring dashboards. Both carry upstream's own least-privilege role sets,
and both deliberately exclude `root` and `clusterAdmin` — the panel cannot drop a database.
[MongoDB nodes →](docs/STACKS.md)
</details>

<details>
<summary><b>Big Hole</b> — MongoDB FTDC in the browser</summary>

A node that runs [Big Hole](https://github.com/zelmario/Big-hole), a third-party viewer for
MongoDB's `diagnostic.data`: drag a folder — or a whole support tarball, however it is nested —
onto the page and it decodes and charts it, every metric it can find, with the replica set laid
out so an election is somewhere to land rather than something to hunt for.

It is the loosest-coupled node in DBCanvas, deliberately: no association line, no credentials, no
configuration, because the app talks to nothing at all — no database, no API, not even DBCanvas.
There is no backend, and nothing you open in it leaves your browser. Open it on **localhost**:
browsers grant a page the on-disk storage this needs only over HTTPS or localhost, and the node's
panel hands you the `ssh -L` line when you are browsing DBCanvas from somewhere else.
[MongoDB nodes →](docs/STACKS.md)
</details>

<details>
<summary><b>Fixes across the app</b></summary>

- A MongoDB node's log and its `diagnostic.data` are **one download**, arriving together under a
  directory named after the node instead of as two files that collide in a downloads folder.
- The **Core Dump Analyzer** shows the whole source file rather than a 29-line window around the
  crashing line, and hands back the `gdb` command line that reproduces the session in your own
  terminal — with this node's paths and library search path already in it.
- A long right-click menu item **wraps** instead of being cut off mid-word.
- Three sidebar icons that were drawing the wrong thing were redrawn: Database Stacks no longer
  looks like the Dashboard, Operator Summary is no longer a beetle, and the Core Dump Analyzer is
  no longer a gem.
- The **Intranet image** is built by `make images`, where it belongs — it is the DNS and CA every
  other node is built against, not an optional extra.
- The **API** page teaches the CLI: download it, put it on your PATH, sign in, and read every
  command it has, without leaving the page to find out how.
</details>
