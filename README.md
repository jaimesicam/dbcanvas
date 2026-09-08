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

### 0.0.3

<details open>
<summary><b>MClusterAdmin</b> — a MongoDB administration panel</summary>

A node that runs [MClusterAdmin](https://github.com/PrzemekMalkowski/mclusteradmin), a
third-party web panel for MongoDB: topology and replica-set status, sharding and the balancer,
current operations, slow queries with `explain`, indexes and profiling, users and roles, and
oplog stats. Its UI is published to a host port like PMM's, so it opens straight from your
browser — no VNC desktop needed. Upstream publishes no image, so DBCanvas builds one from source
at a pinned tag.

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
