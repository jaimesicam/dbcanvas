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
| [**Benchmark**](docs/BENCHMARK.md) | OLTP, OLAP, read-write and read-only workloads, with throughput and latency. |
| [**Packet Inspector**](docs/PACKET_INSPECTOR.md) | Capture on a node and decode MySQL, PostgreSQL, MongoDB and Valkey off the wire. |
| [**Log Summary**](docs/LOG_SUMMARY.md) | Several nodes' logs on one timeline, split into the good, the warning and the bad. |
| [**Stalk Summary**](docs/STALK_SUMMARY.md) | Turn a pt-stalk capture into charts, and say which variables to change. |
| [**FTDC Summary**](docs/FTDC_SUMMARY.md) | Read MongoDB's own black box — the diagnostic data every mongod already writes. |
| [**Operator Summary**](docs/OPERATOR_SUMMARY.md) | Read a pt-k8s-debug-collector cluster-dump — what is not running, and what the operator says about it. |
| [**Operator Debugger**](docs/OPERATOR_DEBUGGER.md) | Step through the Kubernetes operator itself — breakpoints, stack and variables, no IDE. |
| [**Core Dump Analyzer**](docs/CORE_DUMP_ANALYZER.md) | Read a `mysqld` core dump from another server — threads, stack, arguments. |
| [**All in One**](docs/ALL_IN_ONE.md) | Many database instances in one node, for when you need versions side by side. |
| [**HTTP API**](docs/API.md) | Every one of the 228 endpoints, with tokens you create and expire yourself. |
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
