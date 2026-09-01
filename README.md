# DBCanvas — Database Interaction Lab

Design a database topology on a canvas, click **Deploy**, and get real running nodes — wired
together with DNS, TLS, LDAP, replication, monitoring and backups. Then use the tools that
come with it to load those databases, watch them work, and find out why they misbehave.

Built for testing, demos, training, troubleshooting, benchmarking and application
development: spin up a production-shaped cluster in minutes, exercise it, tear it down.

![The Database Stacks canvas with a deployed stack](docs/screenshots/stacks-canvas.png)

---

## Install

Requires **Docker**, with access to its daemon socket.

```sh
git clone https://github.com/jaimesicam/dbcanvas.git && cd dbcanvas
make install
```

That builds the node images, records what each of them can install, and starts DBCanvas at
**http://localhost:8080**. The first run takes a while — it is building operating-system
images — and later runs are just `make compose`.

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
- **MongoDB** — Percona Server for MongoDB: standalone, replica set, sharded
- **Valkey** — standalone and cluster

**The infrastructure around them**: an Intranet node (DNS · mail · OpenLDAP · Squid proxy ·
a certificate authority), PMM, ProxySQL, HAProxy, Orchestrator, SeaweedFS S3, Keycloak,
OpenBao, Samba AD DC, Watchtower, an Ubuntu VNC desktop, and a Kubernetes cluster frame that
runs any of six database operators.

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
| [**Operator Debugger**](docs/OPERATOR_DEBUGGER.md) | Step through the Kubernetes operator itself — breakpoints, stack and variables, no IDE. |
| [**Core Dump Analyzer**](docs/CORE_DUMP_ANALYZER.md) | Read a `mysqld` core dump from another server — threads, stack, arguments. |
| [**All in One**](docs/ALL_IN_ONE.md) | Many database instances in one node, for when you need versions side by side. |
| [**Labs**](docs/LABS.md) | Hands-on scenarios on a disposable stack, graded against the real cluster. |

## Documentation

- **[Getting started](docs/GETTING_STARTED.md)** — install to running cluster, end to end
- [Feature guides](docs/README.md) — the tools above, one page each
- [Stacks](docs/STACKS.md) — every node and cluster type, and everything the canvas does
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

<details>
<summary><b>Every control now explains itself</b> — 345 pieces of written help behind a “?”</summary>

Hover — or tab to — the small **?** beside any field, value or button and DBCanvas says what
it is *for*, what happens if you leave it alone, and when you would change it: across every
node form, every deployed node's panel, the node library and the toolbar. The one-line hints
under the inputs stay where they are; this is the paragraph behind them.
</details>

<details>
<summary><b>Reach a node on a remote install from your own machine</b> — <code>SSH_FORWARDING_HOST</code></summary>

DBCanvas binds every port a deployed node publishes to the server's loopback, which is right —
and leaves the PMM console and the database ports unreachable from the laptop you are actually
sitting at. Set **`SSH_FORWARDING_HOST`** in `.env` to where the server answers SSH and
right-clicking a running node offers **Copy SSH tunnel command**: the exact `ssh -L` line
forwarding every port that node publishes, each to the same port locally, so every address the
UI shows works verbatim through the tunnel.
[Configuration →](docs/CONFIGURATION.md)
</details>

<details>
<summary><b>Deployment templates</b> — save a topology, deploy it again</summary>

Eleven templates ship with the app, one per engine family: PXC + ProxySQL + PMM, Percona
Server replication + Orchestrator, InnoDB Cluster, Patroni + HAProxy, PostgreSQL + pgBackRest,
a PSMDB replica set with PBM, a sharded PSMDB cluster, Valkey Cluster, the PXC operator on
k3s, and an All-in-One node running four engines at once. Pick one in **New stack** and the
whole design is on the canvas; or **Insert template** to merge one into a stack you are
already building — ids are rewritten, the Intranet you already have is not duplicated, and
colliding hostnames are numbered. **Save as template** turns any canvas into a reusable one,
without its passwords, host paths or pinned host ports, and templates export to a `.json` file
you can hand to someone else or check into git. Admins can publish one instance-wide.
[Templates →](docs/STACKS.md#templates--save-a-topology-deploy-it-again)
</details>

<details>
<summary><b>Read a crashed server's core dump, here</b></summary>

A **Linux Client** node can be deployed as a core-dump analysis host: point it at a host
directory holding a `mysqld` core file and another holding the crashed server's `mysqld` plus
its `ldd` closure, pick the version that crashed, and DBCanvas mounts both read-only and
installs the matching debug symbols. The **Core Dump Analyzer** then **diagnoses** it rather
than just rendering it: what kind of crash it was, which frame is actually the bug (almost
never the top one), how deep the runaway went across the whole stack, and — for a runaway —
the input that set it off, dug out of a frame a thousand deep. Plus the threads, the stack and
each frame's arguments, and a verdict on whether the symbols and libraries actually match the
core. [Core Dump Analyzer →](docs/CORE_DUMP_ANALYZER.md)
</details>

<details>
<summary><b>Debug the Kubernetes operator itself, without an IDE</b> — all four Percona operators</summary>

Deploy a K3D frame running the MySQL (PXC), MySQL (Percona Server), MongoDB or PostgreSQL
operator with *Run the operator under Delve* ticked, then open the **Operator Debugger**: set a
breakpoint, press **Force a reconcile**, and read the call stack, the locals and any Go
expression you type. Each operator brings its own quick breakpoints — PS's `doReconcile`,
MongoDB's `reconcileCluster`, PostgreSQL's second loop in Crunchy's `PostgresCluster` — so you
can stop where that operator actually does its work. No clone, no Go toolchain, no
`launch.json`, no `kubectl port-forward`. [Operator Debugger →](docs/OPERATOR_DEBUGGER.md)

![The Operator Debugger stopped in Reconcile, with the call stack and locals beside the source](docs/screenshots/operator-debugger.png)

> *Stopped at the top of `Reconcile` on a live PXC cluster — the call stack, the operator's own
> locals, and an expression evaluated on the spot.*
</details>

<details>
<summary><b>OpenID Connect sign-in for Percona Server</b>, with Keycloak as the identity provider</summary>

Percona added the `auth_openid_connect` plugin in **Percona Server 8.4.11-11**, and DBCanvas
wires it up for you: add a **Keycloak** node, tick *Keycloak SSO* on a **Percona Server** node,
and deploy. You get a realm and an OIDC client on Keycloak, sample users in a group, MySQL
accounts bound to those users' identities, and a demo schema only that group's role can read.
Users then sign in with a signed ID token from Keycloak — **no MySQL password is ever sent** —
and their Keycloak group grants the matching MySQL role.

![The Percona Server node's Keycloak SSO tab, beside a console signing in with an ID token](docs/screenshots/keycloak-oidc.png)

> *The node's **Keycloak SSO** tab gives you the issuer, client, accounts and the sample users'
> password, then the exact commands. In the console beside it, `jane` trades her Keycloak
> password for an `id_token` and logs in with it — `SHOW GRANTS` shows the `accounting` role
> arriving from her Keycloak group.*

The 8.4 LTS series is the only one that has this so far — 9.7 does not carry the plugin yet, so
the designer keeps the node on 8.4 when you turn SSO on. The **Ubuntu VNC** desktop ships the
8.4 client and its OIDC plugin, so you can sign in from there too.
</details>

<details>
<summary><b>Every Kubernetes operator can drive a Stock Market Sim</b></summary>

Link a **Stock Market Sim** node to a K3D frame and the sim runs a live trading workload
against the cluster that frame's operator built — the fastest way to put real, continuous
activity on an operator and watch it behave. All six are supported: **Percona XtraDB
Cluster**, **Percona Server for MySQL**, **Percona Server for MongoDB**, **Percona PostgreSQL
(PGO)**, **CloudNativePG**, and **Crunchy PGO**. The sim picks up the engine from the operator,
resolves the cluster's front end and its generated credentials by itself, and reports
throughput, portfolios and a per-instrument ticker.

![Three Stock Market Sim nodes driving three Kubernetes operators on one canvas](docs/screenshots/stocksim-operators.png)

> *Three K3D frames, three sims, one canvas. The open one is trading against `k3d-02` through
> that cluster's `k3d-02-rw-lb` Service; the panel on the right is the same frame's k3s node —
> PGO 6.0.2, pgBouncer in front, **Expose · proxy: LoadBalancer**, which is what made it
> reachable.*

The cluster has to be reachable from the stack network first, and that is the one thing the
frame cannot guess: a **ClusterIP** address exists only inside Kubernetes. Set the tier in
front of the database — the proxy, the MySQL Router, the mongos routers, the pgBouncer pool,
or the database pods themselves — to **LoadBalancer** (or NodePort) on the frame. The designer
checks this per operator and warns you before you deploy if every tier is ClusterIP, naming the
ones to change.
</details>

<details>
<summary><b>A file manager, and drag and drop</b></summary>

Drag a file — or a whole folder — from your desktop straight onto a node on the canvas and
DBCanvas asks where to put it: no scp, no bind mount, no shell. Right-click a running node and
choose **File manager** for the whole filesystem — navigate, upload and download, create,
rename, change permissions and ownership, delete, and **edit a file in place**. **Split** opens
a second pane on another node and copies between the two, which is the fastest way to put one
file on every member of a cluster.

![The File Manager browsing a node's filesystem](docs/screenshots/file-manager.png)
</details>
