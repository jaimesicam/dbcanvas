# All in One

An **All in One** node is a single container that runs many database instances side by side — for
example a standalone, a replication set, a cluster and a proxy — without spending a container, IP
address and hostname on each. It is meant for labs and demos: a container running many daemons is
slow to deploy and uses a lot of memory.

## Build an All-in-One node

1. In **Database Stacks**, add an **All in One** node and choose its **OS** and **OS version**.
2. Press **Add feature** and pick a kind (see the table below). Repeat for each instance.
3. For each instance set its **Name** (lowercase letters, digits and dashes — it becomes the
   instance's hostname), member count, and any options.
4. Under **Versions**, pick the major and minor for each product family in use. A blank minor means
   the newest available. PostgreSQL is the exception: each PostgreSQL instance has its own
   **PostgreSQL major** and **Minor version**, because majors install side by side.
5. Deploy.

The form shows each instance's assigned ports and warns when the node runs more than 20 daemons or
its estimated memory exceeds the node's limit or the host.

## Instance kinds

| Family | Kinds (as listed in **Add feature**) | Members |
| --- | --- | --- |
| MySQL | Percona Server · PS Replication · InnoDB Cluster / GR · PXC Cluster | Replication 2–5; GR 3–9, odd; PXC 2–5 |
| MySQL Community | MySQL Community · MySQL Replication · MySQL InnoDB / GR | Replication 2–5; GR 3–9, odd |
| MariaDB | MariaDB · MariaDB Replication · MariaDB Galera | Replication 2–5; Galera 3–5, odd |
| PostgreSQL | PostgreSQL · Patroni Cluster · repmgr Cluster · Spock Cluster | 2–5 each |
| MongoDB | PSMDB · PSMDB Replica Set · PSMDB Sharded | Replica set 3–7, odd; Sharded 5 or 13 |
| Valkey | Valkey · Valkey Cluster | Cluster 3–7 |
| Proxies | ProxySQL · HAProxy | ProxySQL 1–3 |
| Topology | Orchestrator | — |

Notes on specific kinds:

- **Replication** kinds offer **Asynchronous** or **Semi-synchronous** mode.
- **InnoDB Cluster / GR** runs Group Replication. InnoDB Cluster mode needs MySQL Shell, which
  All in One does not install, so it is refused.
- **Spock Cluster** offers only the PostgreSQL majors Spock supports.
- **PSMDB Sharded** has two fixed topologies, the same as the dedicated frame: **5** = mongos + one
  config server + three single-member shards; **13** = mongos + a 3-member config replica set +
  three 3-member shards.
- **ProxySQL** and **HAProxy** front another instance on the same node, chosen in **Fronts**.
- **Orchestrator** can monitor this node's MySQL-family instances other than PXC (set
  **Monitored by (Orchestrator)** on each), or a canvas Orchestrator node can. Its **Alert email** defaults to `admin`, the mailbox the
  Intranet always provides; a bare name is delivered inside the stack's domain, and an empty field
  turns alerts off.

A clustered instance becomes one member per daemon: `repl01` with three members is `repl01-n1`,
`repl01-n2` and `repl01-n3`.

## Packaging rules

These are checked before deploy, naming the instances involved.

- **One MySQL flavor per node.** Percona Server, PXC, MariaDB and MySQL Community packages conflict
  with each other, so a node uses only one. Once you add one, the form disables the others. Several
  instances of the same flavor are fine and share its version.
- **One PostgreSQL source per major.** Percona PostgreSQL (`PostgreSQL`, `Patroni Cluster`), PGDG
  (`repmgr Cluster`) and Spock's own build cannot share a major. Put them on different majors.

## Ports

No instance uses its product's default port. Each member gets a 10-port slot in its family's range,
assigned in order: the first MySQL member gets 13000, the next 13010, and so on.

| Family | Base | Offsets in a slot |
| --- | --- | --- |
| MySQL | 13000 | +0 client · +1 X protocol · +2 Galera/GR · +3 IST · +4 SST · +5 clustercheck |
| PostgreSQL | 15000 | +0 postgres · +1 Patroni REST · +2 etcd client · +3 etcd peer |
| ProxySQL | 16000 | +0 MySQL interface · +1 admin · +2 cluster interface |
| MongoDB | 17000 | +0 mongod / mongos |
| HAProxy | 18000 | +0 write · +1 read · +2 stats |
| Valkey | 19000 | +0 client · +1 cluster bus |
| Orchestrator | 20000 | +0 web UI / API |

Adding an instance never moves an existing one's ports, and a redeploy keeps the same map.

Each instance also gets its own hostname (for example `ps01.example.net`, `repl01-n2.example.net`),
all pointing at the same container. Always pass the port explicitly.

**Publish to the host.** Tick it on an instance to publish its client port (set **Host port**, or
`0` for automatic). For Orchestrator, HAProxy and Patroni the web UI, stats page or REST API is
published too, on an automatically chosen port, and shows as a link in the **Ports** tab. Host ports
are fixed when the container is created, so publishing a new one later needs a destroy and redeploy.

## Manage instances

The node's manager panel has four tabs: **Instances** (state, start/stop/restart, logs),
**Connect** (a copyable connection string per instance, from inside the stack and from the host
when published), **Credentials** and **Ports**.

From the node's root console (**Open root console** in the panel, or **Enter root console** on the
canvas node menu) use `aioctl`:

| Command | What it does |
| --- | --- |
| `aioctl list` | Instances, their state and ports |
| `aioctl ports` | The node's full port map |
| `aioctl info <inst>` | Paths, ports and config for one instance |
| `aioctl status [<sel>]` | `systemctl status` (default: all) |
| `aioctl start <sel>` | Start; clusters start seed-first |
| `aioctl stop <sel>` | Stop; clusters stop followers-first |
| `aioctl restart <sel>` | Restart |
| `aioctl logs <inst> [args]` | The instance's journal, e.g. `-f`, `-n 200` |
| `aioctl connect <inst>` | Open the instance's own client with credentials applied |

`<sel>` is an instance name, a cluster name, or `all`. The manager panel uses the same commands.

**Where things are inside the container**

| Path | Contents |
| --- | --- |
| `/opt/aio/<inst>/etc/` | Config (`my.cnf`, `mongod.conf`, `valkey.conf`, `patroni.yml`, `repmgr.conf`, TLS files) |
| `/opt/aio/<inst>/data/` | Data directory |
| `/opt/aio/<inst>/log/` | Logs |
| `/run/aio/<inst>/` | Socket and PID file |
| `/etc/dbcanvas/aio/instances.tsv` | The instance list `aioctl` reads (rewritten on every deploy) |
| `/etc/systemd/system/aio-<inst>.service` | One systemd unit per instance, all grouped under `aio.target` |

The products' own systemd units are masked, so a package upgrade cannot start a server on a
default port.

## Change a deployed node

Add an instance and redeploy: the new one is built in the existing container and the others are left
alone. A deploy that failed part-way retries the unfinished instances.

- **Removing** an instance from the design does not stop or delete it.
- Publishing a new host port needs a destroy and redeploy (see [Ports](#ports)).

## Use the instances with other tools

- **Query Runner**, **Benchmark** and **Data Generator** list each database instance separately as
  `<node> / <instance>`. Valkey, the proxies and Orchestrator are not listed there.
- **PMM:** set **Monitored by (PMM)** per instance. It is offered for MySQL, PostgreSQL, MongoDB,
  Valkey, ProxySQL and HAProxy, not Orchestrator. Clustered instances are registered with
  `--cluster=<instance>` (and `--replication-set` for MongoDB members), so PMM groups them the way
  the design does.
- **TLS:** **Issue a TLS certificate from the Intranet CA** is available for MySQL, PostgreSQL and
  MongoDB instances. TLS is enabled but not required, so plaintext clients still connect.

## Not available on All in One

| Feature | Use instead |
| --- | --- |
| Directory (LDAP) authentication, OpenBao encryption, SeaweedFS backups, Keycloak OIDC | A dedicated node |
| TLS for Valkey and the proxies | A dedicated node |
| InnoDB Cluster mode | Group Replication |
| PostgreSQL instances on Debian or Ubuntu | An Oracle Linux All-in-One node |
| Vagrant VMs | All in One always runs in Docker, also in a Vagrant stack |

These options are rejected by validation rather than ignored.

## Troubleshooting

| Problem | What to check |
| --- | --- |
| An instance will not start | `aioctl logs <inst> -n 100`, then `aioctl info <inst>` for its config and data directory. Error logs are under `/opt/aio/<inst>/log/`. |
| A cluster is up but not clustered | `aioctl start <cluster>` also runs each member's post-start step. Check `aioctl list` shows every member active, then ask the product: `performance_schema.replication_group_members` (GR), `SHOW STATUS LIKE 'wsrep_%'` (PXC/Galera), `repmgr cluster show`, `patronictl list`, `rs.status()`, `cluster info` (Valkey). |
| A client reaches the wrong server | It is probably using a default port. Check `aioctl ports` and pass the port explicitly. |
| A deploy sits on "Installing …" | Packages are downloading. On Oracle Linux, watch `/var/log/dnf.librepo.log` inside the container. |
