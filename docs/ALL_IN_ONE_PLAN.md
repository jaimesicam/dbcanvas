# All-in-One node — implementation plan

A new Database Stacks node type (`aio`) that provisions **one container running many
database feature instances side by side**, instead of one product per node.

Status — **7 of 11 phases complete**; all sixteen feature kinds deploy.

| ✅ done | ⏳ partial |
| --- | --- |
| 0 ports/layout · 2 MySQL PS · 3 PostgreSQL · 4 MongoDB · 5 Valkey · 7 PXC | 1 node end-to-end · 6 proxies+Orchestrator · 8 cross-cutting · 9 app integration · 10 docs |

Deployable kinds: `ps`, `psrepl`, `innodb` (Group Replication mode), `pxc`, `pg`,
`repmgr`, `patroni`, `spock`, `psmdb`, `psmrs`, `psmdbsharded`, `valkey`,
`valkeycluster`, `proxysql`, `haproxy`, `orchestrator`.

What the four partial phases still owe, in priority order:

1. **Phase 1** — the manager UI is missing its *Credentials*, *Console* and
   *Certificates* tabs, so a deployed node never shows how to connect to its
   instances. Biggest usability gap.
2. **Phase 8** — per-instance TLS is implemented but **unit-tested only, never
   verified live** (internet connection issues mid-session). LDAP, OpenBao,
   Keycloak OIDC and SeaweedFS backups are gated: `aioUnimplementedOptions`
   rejects them by name rather than letting the form promise what nothing reads.
   InnoDB Cluster *mode* is likewise gated (needs MySQL Shell).
3. **Phase 9** — `dbauth`, `dbcerts`, Visual Summary and Labs still resolve
   *node → one connection*. Labs is the largest remaining piece of work.
4. **Phase 6** — everything works; only the PDPS/PDPXC repo interaction is
   unverified, and it is reachable only in a PXC node that also runs Orchestrator.
5. **Phase 10** — screenshots and a standalone `docs/ALL_IN_ONE.md`.

Known limits, deliberate rather than accidental: PostgreSQL kinds are Oracle
Linux only; published host ports are fixed at container-create, so an instance
that newly opts into an export needs a destroy/redeploy; the node is Docker-only
(excluded from the Vagrant backend).

Implemented in `app/aio*.go`, `web/src/pages/AllInOne.jsx` and
`web/src/lib/aioPorts.js`; see IMPLEMENTATION.md sessions 192–207.

---

## 1. What it is

| | Existing node types | All-in-One |
| --- | --- | --- |
| Container count | 1 per node / 1 per cluster member | **1 total** |
| Hostname | 1 | 1 (plus per-instance DNS aliases) |
| Product | exactly 1 | **many, co-tenant** |
| Ports | product defaults (3306, 5432, 27017…) | **never the default — allocated slots** |
| Wiring | canvas association lines | **drop-downs only** (`ports: false`) |
| Service control | vendor systemd unit | **`aioctl` + per-instance units** |

### In scope (feature kinds the node can instantiate)

MySQL family — `ps` (Percona Server standalone), `psrepl` (PS replication group),
`pxc` (Percona XtraDB Cluster), `innodb` (InnoDB Cluster **or** Group Replication —
one kind, `replMode` drop-down, two palette entries).

PostgreSQL family — `pg` (standalone), `patroni`, `repmgr`, `spock`.

MongoDB family — `psmdb` (standalone), `psmrs` (replica set), `psmdbsharded`.

Valkey — `valkey` (standalone), `valkeycluster`.

Proxies — `proxysql` (single **or** cluster, `members` drop-down), `haproxy`.

Topology management — `orchestrator` (Percona Orchestrator).

### Explicitly out of scope (stay separate canvas nodes)

Intranet, PMM, Watchtower, Keycloak, K3D cluster, Ubuntu VNC, OpenBao, SeaweedFS,
Linux Client, Traffic/Hotel/Airline/Car Rental Sim, Unoptimized MySQL Challenge.

An AiO instance **references** those by drop-down (`pmmNodeId`, `keycloakNodeId`,
`openbaoNodeId`, `seaweedfsNodeId`), exactly as `ps`/`psm` nodes already do today
— no new edge kinds, no association lines.

Orchestrator is the one type that exists **both ways**: as today's standalone
`orchestrator` node *and* as an AiO feature instance. A MySQL-family instance's
"Monitored by (Orchestrator)" drop-down therefore lists both, so the reference is
`orchestratorRef` — either a canvas node id or `inst:<instanceId>` for an
Orchestrator living in the same container.

---

## 2. The hard part: co-tenancy

Every existing provisioner assumes it owns the machine. Concretely, in
`app/mysql.go` alone: datadir `/var/lib/mysql`, socket `/var/lib/mysql/mysql.sock`,
config `/etc/my.cnf` (`pxcCnfPath`), unit `mysqld` (`mysqlUnit`), port `3306`
(hardcoded at `mysql.go:553`), pid `/var/run/mysqld/mysqld.pid`. Same shape in
`pg.go`, `mongodb.go`, `valkey.go`, `proxysql.go`, `haproxy.go`.

### 2.1 Per-instance filesystem layout

```
/opt/aio/<inst>/          inst = sanitized instance id, e.g. ps01, pxc-cluster-01-n2
  ├── etc/                my.cnf | postgresql.conf | mongod.conf | valkey.conf | …
  ├── data/               datadir / dbpath / PGDATA
  ├── log/                error log, slow log
  └── tmp/                socket, pid  (also /run/aio/<inst> via RuntimeDirectory=)
/etc/dbcanvas/aio/
  ├── instances.tsv       the registry (see §4)
  └── <inst>.env          per-instance env consumed by the unit
/usr/local/bin/aioctl
```

Ownership stays with the **vendor OS user** (`mysql`, `postgres`, `mongod`,
`valkey`, `proxysql`, `haproxy`) — no new users, so SELinux/AppArmor and the
vendor binaries behave. Only the directories move.

### 2.2 Per-instance systemd units

dbcanvas writes its own unit per instance — `aio-<inst>.service` — rather than
relying on vendor units or `mysqld@.service` templates (whose presence and
semantics differ per distro and per Percona series). Vendor units are
`systemctl disable --now` + `mask`ed at provision time so nothing ever grabs a
default port.

All units are `PartOf=`/`WantedBy=` a synthetic `aio.target`, so
`systemctl start aio.target` boots the whole node and group ordering can be
expressed with `After=`/`Requires=` between members of a cluster.

### 2.3 Package conflicts and the MySQL flavor rule

| Family | Multi-instance from one install? | Notes |
| --- | --- | --- |
| PostgreSQL (`pg`/`patroni`) | ✅ yes | PPG RPMs are per-major (`/usr/pgsql-16`); majors co-install; `initdb` per instance |
| PostgreSQL (`repmgr`/`spock`) | ⚠️ PGDG only | `repmgr_NN` requires PGDG's `postgresqlNN-server`, which `percona-postgresqlNN-server` does not provide (verified by repoquery) — so a **per-major** flavor rule: Percona and PGDG cannot share a major, but can share a node on different majors |
| MongoDB (`psmdb`/`psmrs`/`psmdbsharded`) | ✅ yes | install `-server` **and** `-mongos`; one PSMDB version per node; `mongod --config` per instance |
| Valkey | ✅ yes | one package, N configs; **`cluster-port` must be pinned** (default bus = port+10000 collides) |
| ProxySQL | ✅ yes | `--datadir`/`--admin-*` per instance |
| HAProxy | ✅ yes | `-f` per instance |
| MySQL: `ps` + `psrepl` + `innodb` | ✅ yes | one `percona-server-server` install, N datadirs; `group_replication.so` ships with PS |
| MySQL: several `pxc` clusters | ✅ yes | one `percona-xtradb-cluster-server` install, N datadirs — **all PXC instances on one version** |
| Orchestrator | ✅ yes | no `mysql-server` dependency (embedded SQLite backend); see the repo caveat below |
| **PXC alongside `ps`/`psrepl`/`innodb`** | ❌ **no** | `percona-xtradb-cluster-server` and `percona-server-server` both `Provides: mysql-server` and conflict at the RPM/DEB level |

**Decision: packages only — no binary tarballs.** An AiO node therefore has a
single, *derived* MySQL flavor:

| Instances present | Flavor | Packages installed |
| --- | --- | --- |
| any of `ps`, `psrepl`, `innodb` | **PS** | `percona-server-server` (+ `percona-xtrabackup-*`) |
| one or more `pxc` | **PXC** | `percona-xtradb-cluster-server` (+ Galera, `percona-xtrabackup-*`) |
| both | **invalid** | validation error, see §8 |

The flavor is never chosen in the form — it follows from the instances, and the
form surfaces it: once the first MySQL-family instance is added, the "Add feature"
menu **disables and annotates** the incompatible entries ("PXC can't share a
container with Percona Server — `percona-xtradb-cluster-server` conflicts with
`percona-server-server`"), and a node-level warning banner appears if a design
loaded from a saved stack somehow contains both. Deploy is blocked by a hard
validation error regardless, so a stale saved design can't reach `dnf`.

**Two distinct kinds of constraint — don't conflate them.**

*A package conflict* (MySQL only) removes capability: because
`percona-xtradb-cluster-server` and `percona-server-server` cannot both be
installed, some feature combinations are genuinely impossible in one node, so
this needs the menu-disable + validation-error treatment above.

*A shared version* (everything else) removes no capability at all. It only means
one install serves every instance of that family.

**Version is shared per family.** Several PXC clusters in one node are fully
supported (`pxc-cluster-01`, `pxc-cluster-02`, …, each with its own members,
datadirs and port slots) but they all run the **same PXC major+minor**, because
there is one install. Same for PS, PSMDB, Valkey, ProxySQL, HAProxy and
Orchestrator — a single node-level version picker each, no validation rule
needed.

MongoDB is the clearest example of why this is cheap: the packages are
**unversioned** (`percona-server-mongodb-server`, `-mongos`, `-tools`,
`percona-mongodb-mongosh` — `mongodb.go:846-848`) and the major is chosen purely
by repo (`psmdb-60`/`psmdb-70`/`psmdb-80` — `mongodb.go:83-93`), with binaries at
`/usr/bin/mongod`. So PSMDB 7.0 and 8.0 cannot co-exist — but one install of
`-server` + `-mongos` yields **every** MongoDB topology simultaneously
(standalone, replica sets, sharded clusters, any number), since they are all just
`mongod`/`mongos` processes with different config files. Zero topology cost.
Today's provisioner installs `-server` *or* `-mongos` because each node is
single-purpose; the AiO node installs both.

PostgreSQL is the sole family with no version constraint either — PPG packages
are per-major and co-install (`/usr/pgsql-16`, `/usr/pgsql-17`), so a
per-instance `pgMajor` is genuinely supported there.

Side benefit for MongoDB: `mongodb.go:1332` documents that PSMDB 6.0/7.0 ship a
`Type=forking` mongod unit which needs a workaround against the code's
`fork:false`. AiO writes its own units, so that quirk does not apply here.

**Orchestrator repo caveat.** `percona-orchestrator` is carried **only by the PDPS
repo family, never PDPXC** — confirmed live, see the package note at
`app/orchestrator.go:36-41`. In a PXC-flavored AiO node the provisioner must
therefore enable PDPS *for the orchestrator install only* and scope the PXC
install away from it (`dnf --disablerepo='pdps*' install
percona-xtradb-cluster-server`, or install Orchestrator first and enable PDPXC
after). Orchestrator itself has no `mysql-server` dependency — it uses the
embedded SQLite backend (`orchestrator.go:414`) — so the package coexists fine;
it is only the *repo* interaction that needs care. **Flag for live verification
in Phase 6** rather than assumed.

### 2.4 Cluster members inside one container

A "cluster" instance (PXC, Patroni, PSMDB RS, Valkey Cluster…) becomes N
*instances* in the registry, all on the same IP, distinguished only by port.
Product-specific consequences to handle:

- **Galera** — `wsrep_node_address=<ip>:<gcomm port>`, `wsrep_provider_options=
  "gmcast.listen_addr=tcp://<ip>:<gcomm>;ist.recv_addr=<ip>:<ist>"`, SST port per
  member. SST via `xtrabackup-v2` needs a distinct `sst.port`.
- **Group Replication** — `group_replication_local_address=<ip>:<gr port>`,
  `group_replication_group_seeds` listing all members' GR ports.
- **Patroni** — per-member `restapi.listen`, `postgresql.listen`,
  `postgresql.connect_address`; each member also runs an **etcd** member
  (client + peer ports from the slot).
- **PSMDB RS / sharded** — members are `host:port`; `rs.initiate` config uses the
  same FQDN with different ports. Sharded adds config-server RS + `mongos`.
- **Valkey Cluster** — `port` + explicit `cluster-port` (do not let it default to
  `port+10000`); `cluster-announce-ip/port/bus-port` set explicitly.
- **repmgr / Spock** — `node_id`, `conninfo` with explicit port.
- **ProxySQL cluster** — distinct admin + mysql-ifaces per member; `proxysql_servers`
  table populated with `host:adminport`.
- **Orchestrator** — `ListenAddress` moved off `:3000` in the per-instance
  `orchestrator.conf.json`; `SQLite3DataFile` moved to
  `/opt/aio/<inst>/data/orchestrator.sqlite3`. The RHEL package auto-loads
  `/etc/orchestrator.conf.json` with no flag (`orchestrator.go:44`), so the AiO
  unit must pass `-config /opt/aio/<inst>/etc/orchestrator.conf.json` explicitly.
  `orchestrator-client` defaults to `http://localhost:3000`, so each instance's
  env file sets `ORCHESTRATOR_API` to its own port — otherwise `aioctl connect`
  and every discover call hit the wrong instance.

### 2.5 Per-instance DNS aliases (recommended)

`dnsRecord` in `app/dns.go` is already `{host, ip}` and the zone is rebuilt on
every reconcile. Emitting one extra A record per instance (`ps01.<domain>`,
`pxc-cluster-01-n2.<domain>`, all → the AiO container IP) makes replication
configs, PMM registration and `aioctl connect` read naturally, and costs ~15
lines in `reconcileStackDNS`. Docker network aliases (`ContainerSpec.Aliases`,
already a `[]string`) get the same list so in-container resolution works before
bind reloads.

---

## 3. Port allocation

Requirement: **no instance uses its product's default port.**

Deterministic **10-port slot** per instance, allocated at design time (in the
browser, so the form can show the ports before deploy) and frozen into the
deployment `Config` on first deploy so redeploys are stable.

```
slotBase = familyBase + slotIndex*10        slotIndex = 0,1,2,… per family per node
```

| Family | familyBase | Slot offsets |
| --- | --- | --- |
| MySQL (ps/psrepl/pxc/innodb) | 13000 | +0 client · +1 mysqlx · +2 galera gcomm / GR local · +3 IST · +4 SST · +5 clustercheck · +6 admin |
| ProxySQL | 16000 | +0 mysql iface · +1 admin · +2 cluster iface · +3 sqlite web |
| PostgreSQL (pg/patroni/repmgr/spock) | 15000 | +0 postgres · +1 Patroni REST · +2 etcd client · +3 etcd peer |
| MongoDB (psmdb/psmrs/sharded) | 17000 | +0 mongod/mongos · +1 (config-srv role) |
| HAProxy | 18000 | +0 write · +1 read · +2 stats |
| Valkey | 19000 | +0 client · +1 cluster bus (explicit `cluster-port`) |
| Orchestrator | 20000 | +0 web/API (`ListenAddress`, off the default `:3000`) |

Ceiling: 100 instances per family per node — far past what fits in a container.
A `portsFor(kind, slotIndex)` helper lives in one place (`app/aio_ports.go`) and
is mirrored 1:1 in JS (`web/src/lib/aioPorts.js`) so form and backend agree; a Go
unit test asserts no two slots overlap across all families.

Host publishing stays opt-in per instance (`exportEnabled` / `exportHostPort`),
reusing the existing duplicate-host-port validation in `validateStack`.

---

## 4. `aioctl` — the service control script

Modelled on the PMM container's `supervisorctl` ergonomics, but over systemd.

**Registry** — `/etc/dbcanvas/aio/instances.tsv`, plain TSV so the script needs no
`jq` (which isn't installed on the base images):

```
# inst    kind    group             role      unit              port  extra_ports        datadir              client
ps01      ps      -                 -         aio-ps01          13000 13001              /opt/aio/ps01/data   mysql
pxc1-n1   pxc     pxc-cluster-01    bootstrap aio-pxc1-n1       13010 13011,13012,13013  /opt/aio/pxc1-n1/data mysql
pxc1-n2   pxc     pxc-cluster-01    member    aio-pxc1-n2       13020 …
pg01      pg      -                 -         aio-pg01          15000 -                  /opt/aio/pg01/data   psql
```

**Commands**

| Command | Behaviour |
| --- | --- |
| `aioctl list` | table: instance, kind, group, systemd state, ports, version, uptime |
| `aioctl status [inst\|group]` | `systemctl status` for the matching units |
| `aioctl start\|stop\|restart <inst>` | one instance |
| `aioctl start\|stop\|restart group <group>` | whole cluster, **in dependency order** (bootstrap/primary first on start, last on stop) |
| `aioctl start\|stop\|restart all` | `systemctl {start,stop} aio.target` |
| `aioctl logs <inst> [-f] [-n N]` | `journalctl -u` + tail of the product error log |
| `aioctl ports` | full port map for the node |
| `aioctl connect <inst>` | exec the right client (`mysql`/`psql`/`mongosh`/`valkey-cli`/`proxysql-admin`) with socket/port/creds prefilled |
| `aioctl info <inst>` | paths, config file, creds hint, PMM/LDAP/Vault wiring |

Bash, ~250 lines, no dependencies beyond coreutils + systemd. Ships as a Go
string constant in `app/aio_ctl.go` and is written with `CopyFile` at provision
time (same mechanism as every other script in the codebase).

**Same operations from the app** — the AiO manager UI calls
`POST /api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/{start|stop|restart}`,
which execs `aioctl` in the container. UI and CLI cannot drift because there is
one implementation.

---

## 5. Data model

### Go — `app/intranet.go` (`designNode`)

```go
// All-in-One node fields (Type=="aio"). One container hosting N feature
// instances; see app/aio.go. Reuses OS/OSVersion/Arch/CPUs/MemoryGB/UseProxy.
AIOInstances []aioInstance `json:"aioInstances"`
// Shared per-family versions (one install per family per container).
AIOPSMajor    string `json:"aioPsMajor"`    // Percona Server 8.0|8.4
AIOPSVersion  string `json:"aioPsVersion"`
AIOPXCMajor   string `json:"aioPxcMajor"`
AIOPXCVersion string `json:"aioPxcVersion"`
AIOPSMDBMajor  string `json:"aioPsmdbMajor"`
AIOPSMDBVersion string `json:"aioPsmdbVersion"`
AIOValkeyMajor string `json:"aioValkeyMajor"`
// …proxysql, haproxy
```

`aioInstance` is a **flat union struct**, matching the style `designFrame`
already uses (see `intranet.go:160-290`) — type-safe in Go, and directly
convertible to the synthetic `designFrame`/`designNode` that existing
provisioners already accept:

```go
type aioInstance struct {
    ID      string `json:"id"`      // stable uuid
    Kind    string `json:"kind"`    // ps|psrepl|pxc|innodb|pg|patroni|repmgr|spock|
                                    // psmdb|psmrs|psmdbsharded|valkey|valkeycluster|
                                    // proxysql|haproxy|orchestrator
    Name    string `json:"name"`    // ps01, pxc-cluster-01 — unique within the node
    Members int    `json:"members"` // cluster kinds; 1 for standalone
    Slot    int    `json:"slot"`    // first slot index (ports derive from it)

    // shared knobs
    RootPassword string `json:"rootPassword"`
    GenerateCert bool   `json:"generateCert"`
    CertTTLValue int    `json:"certTtlValue"`
    CertTTLUnit  string `json:"certTtlUnit"`
    ExportEnabled  bool `json:"exportEnabled"`
    ExportHostPort int  `json:"exportHostPort"`

    // drop-down wiring (replaces association lines)
    PMMNodeID          string `json:"pmmNodeId"`
    OrchestratorRef    string `json:"orchestratorRef"` // "<nodeId>" | "inst:<instanceId>"
    OpenBaoNodeID      string `json:"openbaoNodeId"`
    KeycloakNodeID     string `json:"keycloakNodeId"`
    SeaweedFSNodeID    string `json:"seaweedfsNodeId"`
    SeaweedFSBucket    string `json:"seaweedfsBucket"`
    LdapAuth           bool   `json:"ldapAuth"`
    LdapSourceNodeID   string `json:"ldapSourceNodeId"` // Intranet or Samba AD node
    BackendInstanceID  string `json:"backendInstanceId"`// proxysql/haproxy → AiO instance it fronts

    // per-kind knobs (mirroring designFrame's union)
    GTID bool `json:"gtid"` ; ReplMode string `json:"replMode"` // async|semisync | innodbcluster|groupreplication
    MySQLRouter bool `json:"mysqlRouter"` ; PDPSRepo string `json:"pdpsRepo"`
    PGMajor string `json:"pgMajor"` ; PGVersion string `json:"pgVersion"`
    UsePgBackRest bool `json:"usePgBackRest"` ; EnableBarman bool `json:"enableBarman"`
    PSMDBSetup string `json:"psmdbSetup"` ; Shards int `json:"shards"` ; EnablePBM bool `json:"enablePBM"`
    EnableOIDC bool `json:"enableOIDC"` ; OIDCRealm string `json:"oidcRealm"` /* … */
    EnableVault bool `json:"enableVault"`
    Mode string `json:"mode"` // proxysql singlewrite|loadbal
    UseLDAP bool `json:"useLdap"` // valkey
    OrchestratorVersion string `json:"orchestratorVersion"` ; AlertEmail string `json:"alertEmail"`
}
```

Nothing is added to `designFrame` and no new frame type is introduced — the AiO
node is a single `designNode`, so canvas layout, drag, deploy-freeze, teardown
and the removed-node cleanup path all work unchanged.

### Deployment `Config`

```go
type aioConfig struct {
    Image, OS, Arch, Hostname, FQDN string
    Instances []aioInstanceRuntime  // resolved: unit, ports, datadir, fqdn alias, version, state
}
```

Written on first deploy so ports/paths are stable and the manager UI can render
without exec'ing into the container.

---

## 6. Refactor needed in existing code

The reuse win is real but requires one focused refactor. Introduce:

```go
// app/aio_layout.go
type instLayout struct {
    Inst     string // "" for a classic one-product node
    Unit     string // "mysqld" | "aio-ps01"
    ConfPath string
    DataDir  string
    LogErr   string
    RunDir   string
    Sock     string
    Ports    instPorts
}
func defaultLayout(product, os string) instLayout // returns exactly today's values
```

Then thread `instLayout` through the **shared** helpers that both classic and AiO
paths use — `mysqlMyCnf`, `mysqlSetupBaseline`, `pxcApplyCert`, `pxcCnfPath`,
`mysqlUnit`, `pxcLogError`, and their pg/mongo/valkey/proxy equivalents. Existing
call sites pass `defaultLayout(...)`, so behaviour is byte-identical and every
current node type is untouched in effect.

**AiO orchestration lives in new files**, not inside the existing provisioners:

```
app/aio.go            node provisioner: container, base packages, aioctl, registry
app/aio_ports.go      slot allocator + tests
app/aio_layout.go     instLayout + defaultLayout
app/aio_ctl.go        the aioctl script constant + unit templates
app/aio_mysql.go      ps / psrepl / innodb / pxc instances
app/aio_pg.go         pg / patroni / repmgr / spock instances
app/aio_mongo.go      psmdb / psmrs / psmdbsharded instances
app/aio_valkey.go     valkey / valkeycluster instances
app/aio_proxy.go      proxysql / haproxy / orchestrator instances
app/aio_mgmt.go       HTTP handlers (list/start/stop/restart/logs/ports)
app/aio_test.go       port allocation, registry rendering, validation rules
```

This keeps 12k lines of working, live-tested provisioners off the critical path
while still sharing the shell-script constants and config renderers.

---

## 7. Frontend

`StackDesigner.jsx` is already 7.7k lines — the AiO form goes in new files:

```
web/src/pages/AllInOneForm.jsx      the node form (imported by StackDesigner)
web/src/pages/AllInOneManager.jsx   the deployed-node manager tab
web/src/lib/aioPorts.js             mirror of app/aio_ports.go
web/src/lib/aioKinds.js             kind catalog: label, family, fields, defaults, caps
```

### Catalog entry (`NODE_TYPES` in StackDesigner.jsx)

```js
aio: {
  label: 'All in One',
  slug: 'aio',
  sub: 'Every database feature in one container',
  color: '#7c3aed',
  icon: 'Server',
  singleton: false,
  ports: false,              // ← no association lines, by design
  osOptions: [{ id: 'oraclelinux', label: 'Oracle Linux' }, { id: 'ubuntu', label: 'Ubuntu' }],
  defaults: { os: 'oraclelinux', osVersion: '9', arch: 'amd64', useProxy: false,
              cpus: 0, memoryGb: 0, aioInstances: [] },
}
```

Palette: a new top group **`All in One`** placed after `Core`, single entry.

### Form structure

1. **Node header** — OS / version / arch, CPUs, memory, `useProxy`, cert defaults.
2. **Shared versions** — PS *or* PXC (never both), PSMDB, Valkey, ProxySQL,
   HAProxy, Orchestrator major/minor pickers, each rendered **only when that
   family has ≥1 instance**, each labelled "applies to all N *kind* instances in
   this node" so the one-install-per-family rule is visible where it bites.
3. **Add feature ▾** — a drop-down listing the 17 entries, grouped by family.
   Picking one appends an instance card auto-named `ps01` / `pxc-cluster-01`
   (reusing `nextMemberName` / `nextNamedCluster`). Entries incompatible with
   what's already in the node are **shown but disabled, with the reason in the
   row** — so adding PXC to a node that has a Percona Server instance is a
   greyed-out `PXC Cluster — conflicts with percona-server-server (ps01)`, not a
   silent omission or a failure at deploy time.
4. **Instance cards** — collapsible, colour-tinted by family, each showing:
   - name (text) · members (number, cluster kinds only, with a cap)
   - kind-specific checkboxes/selects/textboxes from `aioKinds.js`
   - **wiring drop-downs**: PMM, Orchestrator, LDAP source, OpenBao, Keycloak,
     SeaweedFS + bucket, "fronts instance" (proxies)
   - a **read-only computed port table** for the instance
   - remove button
5. **Node summary bar** — the derived MySQL flavor (PS / PXC / none), instance
   count, daemon count, allocated port ranges, an estimated-RAM figure, and any
   validation warnings live.

### Manager (deployed node)

Tabs, following the existing `*Manager.jsx` pattern:
- **Instances** — the `aioctl list` table with per-row Start/Stop/Restart/Logs and
  a group-level control; polls the mgmt endpoint.
- **Ports** — full map + copyable connection strings.
- **Credentials** — per-instance, via the existing `SecretInline` component.
- **Console** — existing web terminal, opening with a `aioctl list` banner.
- **Certificates** — reuses `PGCertTab` / `MongoCertReissue` per instance.

---

## 8. Validation (`validateStack`)

New AiO rules, all as `issue{}` entries:

| Severity | Rule |
| --- | --- |
| error | instance names not unique within the node; name not DNS-safe |
| **error** | **`pxc` together with any of `ps` / `psrepl` / `innodb` in the same node** — `percona-xtradb-cluster-server` conflicts with `percona-server-server`. Message names the offending instances on both sides and states the packaging reason. |
| error | member counts out of range (PXC 2–5, GR 3–9 odd, PSMDB RS 3–7 odd, Patroni 2–5, Valkey Cluster ≥3, sharded shards 1–3) |
| error | proxy `backendInstanceId` empty or pointing at an incompatible kind |
| error | `orchestratorRef` points at a missing node / missing local instance / non-Orchestrator target |
| error | drop-down references a node id that isn't on the canvas / is the wrong type |
| error | duplicate requested host ports (extend the existing `exportReq` check) |
| warning | more than one Orchestrator instance in a node (supported, but one discovers every cluster already) |
| warning | estimated memory exceeds the host's (`HostResources`) or the node's `memoryGb` |
| warning | >20 instances (deploy time, not correctness) |
| warning | Vagrant backend selected (an AiO node is a Docker-only type — add `aio` to neither `vagrantVMNode` nor `vagrantVMFrame`; Phase 2 at the earliest) |

---

## 9. Deploy wiring

`handleDeployStack` (`app/intranet.go:1457`) gains one case:

```go
case "aio":
    a.provisionAIO(st, n, doc)
```

`provisionAIO` follows the established shape (`pxcNewProg` progress, `deployScope`,
one goroutine) and runs phases:

1. wait for Intranet → create container (all instance FQDNs as network aliases)
2. `WaitSystemd`, trust CA, DNF/apt IPv4, optional Squid proxy
3. install the union of required packages, once per family
4. write `aioctl`, `aio.target`, the registry, `/etc/dbcanvas/aio/*.env`
5. **per family, in dependency order**: init datadirs → write configs → write units
   → `systemctl enable --now` → product bootstrap (root pw, users, `rs.initiate`,
   `bootstrap-pxc`, `patronictl`, `CREATE CLUSTER`…)
6. per-instance extras: certs, PMM client registration, LDAP, Vault, OIDC, PBM/pgBackRest
7. `reconcileStackDNS` (with instance aliases) → `DeployRunning`

Teardown/`removeNodeResources` needs no change — one container, one volume set.

---

## 10. Phasing

Each phase ends with a live deploy on a real stack (per the project's
verify-live rule) and an `IMPLEMENTATION.md` session entry.

| Phase | Deliverable |
| --- | --- |
| **0** | ✅ **done** — `instLayout` + `defaultLayout`, existing node types unchanged, `aio_ports.go` + tests |
| **1** | ⏳ **partial** — palette, form, container, `aioctl`, registry, `aio.target`, mgmt endpoints, validation, DNS aliases ✅. The **manager UI is incomplete**: of the five tabs specified above only *Instances* and a partial *Ports* (no copyable connection strings) were built. *Credentials*, *Console* and *Certificates* are missing, so a deployed node never shows how to connect to its instances — every classic manager does. |
| **2** | ✅ **done** — `ps`, `psrepl`, `innodb` in **Group Replication** mode. InnoDB Cluster mode stays gated by `aioUnsupportedModes` (needs MySQL Shell). Flavor-conflict validation + disabled-menu-entry UI landed here, and is now verified from both the PS and PXC sides. |
| **3** | ✅ **done** — `pg`, `repmgr` (PGDG, repmgrd armed), `patroni` (co-located etcd; Patroni owns postgres), `spock` (source build shared per major, full mesh). Oracle Linux only. |
| **4** | ✅ **done** — `psmdb`, `psmrs`, `psmdbsharded` (one install serves all three; flatter sharded topology than the classic frame) |
| **5** | ✅ **done** — `valkey`, `valkeycluster` (`cluster-port` pinned in-slot, verified live) |
| **6** | ⏳ **partial** — `orchestrator`, `proxysql` and `haproxy` ✅, with `backendInstanceId` wiring verified live against a running cluster. The PDPS/PDPXC repo interaction is still unverified — only reachable once PXC lands. |
| **7** | ✅ **done** — N clusters, one shared version, Galera pinned to slot ports, bootstrap via a generated start wrapper (no `mysql@bootstrap` unit) |
| **8** | ⏳ **partial** — Orchestrator ✅, PMM ✅, per-instance TLS ✅ for MySQL/PostgreSQL/MongoDB (**unit-tested only — not yet verified live**, see session 204). LDAP, OpenBao, Keycloak OIDC and SeaweedFS backups remain gated: `aioUnimplementedOptions` rejects them by name so the form cannot promise what no provisioner reads. |
| **9** | ⏳ **partial** — Query Runner, Benchmark and Data Generator ✅ via a composite `<nodeId>#<inst>` target id. `dbauth`, `dbcerts`, Visual Summary and Labs still resolve *node → one connection*. |
| **10** | ⏳ **partial** — README's *All in One* section ✅. Screenshots and a standalone `docs/ALL_IN_ONE.md` outstanding. |

Phases 2–7 are independent as *development* work and can be reordered or
parallelised — note only that phases 2 and 7 produce mutually exclusive node
flavors at *runtime*, so the phase-2 conflict validation must be in place before
phase 7 ships. Phase 9 is the widest blast radius outside AiO itself — worth
treating as its own mini-project.

---

## 11. Open decisions

**D1 — PXC coexistence. RESOLVED: packages only, no binary tarballs.** An AiO
node is either PS-flavored or PXC-flavored; mixing is a validation error plus a
disabled, annotated menu entry in the form. Several PXC clusters per node are
supported, all sharing one version. See §2.3.

**D2 — instance option storage.** Recommended: the flat `aioInstance` union struct
above (consistent with `designFrame`, type-safe, no schema migration since
`design` is a JSON blob). Alternative: `Options map[string]any` — less code, but
loses Go type-checking and every provisioner does its own casting.

**D3 — how far to push Phase 9.** Minimum viable: Query Runner + Data Generator +
Benchmark see AiO instances as targets. Everything else (Labs authoring against
AiO instances) can stay unsupported with a clear message.

**D4 — Vagrant backend.** Recommended: `aio` is Docker-only for now. A VM running
30 daemons is plausible but doubles the test matrix.
