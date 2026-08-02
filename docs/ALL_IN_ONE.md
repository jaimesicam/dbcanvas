# All in One — operator guide

One container running many database instances side by side, instead of one product
per node. This is the reference; the [README section](../README.md#all-in-one) is the
short version, and [ALL_IN_ONE_PLAN.md](ALL_IN_ONE_PLAN.md) is the design and phase
tracker.

---

## What it is for

Every other node type owns its container, so it can take the product's default port,
`/var/lib/<product>`, and the vendor systemd unit. An All-in-One node inverts that: a
single container hosting a whole estate, so a stack can demonstrate a standalone, a
replication pair, a cluster and a proxy without spending a container — and an IP, and a
DNS name — on each.

It is a lab and demo node. A container running thirty daemons is slow to deploy and
memory-hungry; the form shows a running estimate and validation warns before you get
there.

---

## Feature kinds

| Family | Kinds | Notes |
| --- | --- | --- |
| MySQL | `ps` · `psrepl` · `innodb` · `pxc` | `psrepl` is async or semi-sync; `innodb` is Group Replication (InnoDB Cluster mode needs MySQL Shell and is refused) |
| PostgreSQL | `pg` · `repmgr` · `patroni` · `spock` | Oracle Linux only. `pg` may run a different major per instance |
| MongoDB | `psmdb` · `psmrs` · `psmdbsharded` | One install serves all three |
| Valkey | `valkey` · `valkeycluster` | |
| Proxies | `proxysql` · `haproxy` | Front an instance chosen by drop-down, not a drawn line |
| Topology | `orchestrator` | Monitors the node's own MySQL instances, or a canvas Orchestrator node can |

Cluster kinds expand into one *member* per daemon: `repl01` with three members becomes
`repl01-n1`, `repl01-n2`, `repl01-n3`, each an independent server.

**Versions.** One package install serves a whole family, so the major and minor are chosen
once per family at the top of the node's form — Percona Server *or* PXC, PS MongoDB, Valkey,
ProxySQL, Orchestrator. Both lists come from the `make versions` catalog and are filtered by
the node's OS/version/arch, so they only offer what is actually installable; leaving the
minor blank means "the newest available". PostgreSQL is the exception: its packages are
per-major and co-install, so each PostgreSQL instance carries **its own** major and minor,
set on the instance card.

---

## Ports

**Nothing listens on its product's default port.** That is the point of the node type:
with several servers in one container the defaults collide, so each member gets a
private ten-port slot.

| Family | Base | Offsets inside a slot |
| --- | --- | --- |
| MySQL | 13000 | +0 client · +1 mysqlx · +2 galera/GR · +3 IST · +4 SST · +5 clustercheck |
| PostgreSQL | 15000 | +0 postgres · +1 Patroni REST · +2 etcd client · +3 etcd peer |
| ProxySQL | 16000 | +0 mysql iface · +1 admin · +2 cluster iface |
| MongoDB | 17000 | +0 mongod/mongos |
| HAProxy | 18000 | +0 write · +1 read · +2 stats |
| Valkey | 19000 | +0 client · +1 cluster bus |
| Orchestrator | 20000 | +0 web/API |

Allocation is **positional within a family**: the first MySQL member takes 13000, the
next 13010, and so on. Adding an instance therefore never moves an existing one's port,
and a redeploy reproduces the same map.

Each instance also gets its own DNS name — `ps01.example.net`, `repl01-n2.example.net` —
all resolving to the single container. The name identifies the instance; the port is what
actually distinguishes it.

Two traps this avoids, both of which would otherwise fail silently:

- Valkey's cluster bus defaults to `port + 10000`, which at base 19000 would land outside
  the family's range. `cluster-port` is set explicitly and announced.
- Galera's `gmcast.listen_addr`, `ist.recv_addr` and SST receive address all default to
  per-host ports, so the second member could not bind. All three are pinned to the slot.

---

## `aioctl`

Open the node's console (**Connect → Open root console**, or the canvas node menu) and
control instances the way PMM's container gives you `supervisorctl`:

```
aioctl list                 instances, their state and ports
aioctl ports                the node's full port map
aioctl info    <inst>       paths, ports and config for one instance
aioctl status  [<sel>]      systemctl status (default: all)
aioctl start   <sel>        start   — clusters seed-first
aioctl stop    <sel>        stop    — clusters followers-first
aioctl restart <sel>        restart
aioctl logs    <inst> [...] journalctl for one instance (e.g. -f, -n 200)
aioctl connect <inst>       that instance's own CLI, credentials applied
```

`<sel>` is an instance name, a cluster (group) name, or `all`. Ordering is not cosmetic:
a Galera or Group Replication seed must come up before its joiners, and a mongos will not
start until its config replica set exists, so routers go last.

The node's manager panel drives this same script over `docker exec`, so the UI and the
console cannot disagree.

The instance table lives at `/etc/dbcanvas/aio/instances.tsv` and is rewritten on every
deploy. It is TSV read with `awk` rather than JSON read with `jq`, because `jq` is not on
the base images and adding a package so a control script can work would be the wrong trade.

---

## Layout inside the container

```
/opt/aio/<inst>/
  ├── etc/     my.cnf | mongod.conf | valkey.conf | patroni.yml | repmgr.conf | tls/
  ├── data/    datadir / dbpath / PGDATA
  └── log/
/run/aio/<inst>/          socket + pid (systemd RuntimeDirectory)
/etc/dbcanvas/aio/
  ├── instances.tsv       the registry aioctl reads
  ├── <inst>.env          per-instance coordinates
  └── <inst>.poststart    optional hook, run after the unit starts
/etc/systemd/system/aio-<inst>.service      bound to aio.target
```

Directories keep the vendor OS user (`mysql`, `postgres`, `mongod`, …) — only the paths
move, so package scripts and SELinux behave. Vendor units are **masked**, not merely
disabled, so a package upgrade cannot start a server on a default port behind your back.

`aio.target` groups every instance, which is what makes `aioctl stop all` one call.

Some products need work *after* their daemon reports ready. Group Replication members run
with `group_replication_start_on_boot=OFF` — otherwise three members race into a split
group — so systemd reporting `active` would leave the group down. `<inst>.poststart` is
run by `aioctl` after a successful start or restart, and the provisioner stages each GR
member's own idempotent bootstrap/join there.

---

## Two packaging rules

Both are refused before deploy, with the offending instances named, rather than failing
halfway through a `dnf` transaction.

**MySQL — one flavor per node.** `percona-server-server` and
`percona-xtradb-cluster-server` both `Provides: mysql-server`, so a node is either PXC or
Percona Server. Add one and the designer greys out the other with the reason. Several PXC
clusters in one node are fine; they share the single install's version.

**PostgreSQL — one distribution per major.** `repmgr_NN` requires PGDG's
`postgresqlNN-server`, which `percona-postgresqlNN-server` does not provide; Spock
compiles a patched PostgreSQL into the same `/usr/pgsql-NN` prefix. Since these packages
are per-major, so is the rule — a Percona `pg` on 16 alongside a PGDG `repmgr` on 17 is
fine, both on 16 is not.

Everything else shares only a *version*, which costs nothing: one PSMDB install serves a
standalone, a replica set and a sharded cluster simultaneously.

---

## Editing a deployed node

Add a feature to a running node and redeploy: the new instance is built into the existing
container without touching the ones already there. The deploy tracks which instances were
actually built, so a deploy that failed halfway retries the unfinished one rather than
skipping it forever.

Two limits:

- **Removing** an instance leaves it running. Tearing down a datadir stays an explicit
  action, not a side effect of a redeploy.
- An instance that **newly opts into publishing a host port** needs a destroy/redeploy,
  because published ports are fixed when the container is created.

---

## Using the databases

Instances appear individually in the **Query Runner**, **Benchmark** and **Data
Generator**, named `<node> / <instance>`, each on its own port. Non-database instances
(Valkey, the proxies, Orchestrator) are not offered there — reach those on their ports.

The manager's **Connect** tab gives a copyable connection string per instance, both from
inside the stack and from the host where a port is published.

---

## What is not available yet

| | |
| --- | --- |
| LDAP / directory auth, OpenBao encryption, SeaweedFS backups, Keycloak OIDC | Rejected by validation rather than silently ignored — the form does not offer them |
| InnoDB Cluster *mode* | Needs MySQL Shell; use Group Replication |
| PostgreSQL on Debian/Ubuntu | Debian's packaging is cluster-managed (`pg_createcluster`), a different model |
| The Vagrant backend | The node is Docker-only |
| Labs authoring against instances | Labs still target whole nodes |

Per-instance TLS **is** implemented for MySQL, PostgreSQL and MongoDB (Intranet CA,
enabled not required, so plaintext clients keep working) but at time of writing has been
unit-tested only — see IMPLEMENTATION.md session 204.

---

## Troubleshooting

**An instance will not start.** `aioctl logs <inst> -n 100`, then `aioctl info <inst>` for
its config path and datadir. Each instance's error log is under `/opt/aio/<inst>/log/`.

**A cluster is up but not clustered.** `aioctl start <group>` starts the daemons and runs
each member's post-start hook; check `aioctl list` shows every member `active`, then ask
the product: `SELECT * FROM performance_schema.replication_group_members` for GR,
`SHOW STATUS LIKE 'wsrep_%'` for PXC, `repmgr cluster show`, `patronictl list`,
`rs.status()`, `cluster info`.

**A client connects to the wrong server.** Almost always a default port. Nothing here uses
one — check `aioctl ports` and pass the port explicitly.

**A deploy stalls on "Installing …".** That is `dnf` fetching packages, not the node.
Watch `/var/log/dnf.librepo.log` inside the container; these are large downloads and
internet connection issues can make one take a long while.
