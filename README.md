# DBCanvas — Database Interaction Lab

Design a database topology on a canvas, click **Deploy**, and get real running nodes — wired
together with DNS, TLS, LDAP, replication, monitoring and backups. Then use the tools that
come with it to load those databases, watch them work, and find out why they misbehave.

Built for testing, demos, training, troubleshooting, benchmarking and application
development: spin up a production-shaped cluster in minutes, exercise it, tear it down.

![The Database Stacks canvas with a deployed stack](docs/screenshots/stacks-canvas.png)

## Quickstart

Requires **Docker**, with access to its daemon socket.

```sh
git clone <this repo> && cd dbcanvas
make install
```

`make install` builds the node images, records what each of them can install, then starts
DBCanvas at **http://localhost:8080**. The first run takes a while — it is building operating
system images — and later runs are just `make compose`.

Open the URL and create the admin account. In a new stack, add an **Intranet** node first: it
provides the DNS, mail, LDAP and certificate authority every other node uses. Then add a
database and press **Deploy**.

Everything has a working default. Before exposing DBCanvas beyond your own machine, change
the passwords in `.env` — see [Configuration](docs/CONFIGURATION.md).

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
| [**All in One**](docs/ALL_IN_ONE.md) | Many database instances in one node, for when you need versions side by side. |
| [**Labs**](docs/LABS.md) | Hands-on scenarios on a disposable stack, graded against the real cluster. |

## Documentation

- [Feature guides](docs/README.md) — the list above, in detail
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
