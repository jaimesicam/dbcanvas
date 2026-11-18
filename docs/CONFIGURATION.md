# Configuration

Where DBCanvas keeps its data, what `config.yaml` sets, the node passwords, the settings in the
app, and fixes for common problems. The commands are in [The `dbcanvas` command](CLI.md).

> [!IMPORTANT]
> **DBCanvas is for previewing features, testing and learning — not for production.** The nodes
> are built for quick setup, not hardened.

## The data folder

Everything DBCanvas keeps is in `~/.dbcanvas`, or the folder `DBCANVAS_DATA` names. The folder is
readable by your user only.

| File | |
| --- | --- |
| `config.yaml` | Settings (below). Written on the first run with every setting commented out. |
| `dbcanvas.db` | The database: stacks, designs, tokens, settings, history. |
| `dbcanvas-encryption.key` | The key the database's secrets are encrypted with. See [The encryption key](#the-encryption-key). |
| `dbcanvas.log` | The service's log. |
| `dbcanvas.sock` | The service's control socket. |
| `versions.yaml`, `versions-arm64.yaml` | A version catalogue downloaded by `dbcanvas versions update`, read ahead of the built-in one. |
| `assets/<version>/` | Files the binary carries and unpacks on start: node image Dockerfiles, licence texts, Pager Panic banks, Big Hole. |
| `bin/k3d` | k3d, downloaded and checksum-verified the first time a K3D cluster is deployed. |

**Back up** the whole folder with the service stopped (`dbcanvas stop`). The database cannot be
read without the key, so keep them together, or the key somewhere safer.

**Uninstall:** `dbcanvas rm` every stack (or `dbcanvas down` them), `dbcanvas stop`, then delete
the `dbcanvas` binary and `~/.dbcanvas`.

## `config.yaml`

```yaml
# ~/.dbcanvas/config.yaml
port: 8080
publicUrl: https://lab.example.com
networkRange: 10.20.0.0/16
env:
  SSH_FORWARDING_HOST: 192.0.2.10
  EXPERIMENTAL: "on"
```

The service reads it when it starts: after a change, run `dbcanvas stop`, and the next command
starts it with the new settings. A setting also set as an environment variable (the name in
brackets) uses the environment's value.

| Key | Default | Meaning |
| --- | --- | --- |
| `instance` (`DBCANVAS_INSTANCE`) | `dbcanvas-<login>` | The prefix of every container, network and K3D cluster this dbcanvas creates, so several people can share one Docker host. Lowercase letters, digits and single dashes, starting with a letter, at most 40 characters. Changing it leaves what the old name created alone. |
| `port` (`APP_PORT`) | `8080` | The browser's port on 127.0.0.1. When it is taken, a free one is used; `dbcanvas status` shows which. |
| `publicUrl` (`PUBLIC_URL`) | *(empty)* | The address a reverse proxy or tunnel of yours gives DBCanvas. Shared-session join links are built on it, and `dbcanvas` prints your own sign-in link on it too; empty uses the address your browser is on. DBCanvas itself listens on 127.0.0.1 only. |
| `domain` (`DOMAIN`) | `example.net` | The DNS domain of every stack's nodes (Intranet DNS, LDAP base DN, mail, CA). |
| `dockerSocket` (`DOCKER_SOCK`) | the current Docker context's | The Docker API socket. |
| `platform` (`DOCKER_PLATFORM`) | the Docker host's | `linux/amd64` or `linux/arm64`: what a node on Automatic runs as. See [Architecture](#architecture). |
| `imageRegistry` (`IMAGE_REGISTRY`) | `ghcr.io/jaimesicam` | Where DBCanvas's own node images are pulled from when this host does not have them. `off` turns the pulls off. |
| `deploymentTimeout` (`DEPLOYMENT_TIMEOUT`) | `60` | How long a deploy waits for a dependency to become ready: minutes (`90`) or a duration (`45m`, `2h`). Raise it for large stacks. |
| `networkRange` (`DBCANVAS_NETWORK_RANGE`) | `10.0.0.0/8` | Where stack networks may be placed, as CIDRs separated by commas. See [Stack networks](#stack-networks). |
| `env` | — | Any other setting, by its variable name (below). |

### Other settings, under `env:`

| Variable | Default | Meaning |
| --- | --- | --- |
| `CONTAINER_BIND_IP` | `127.0.0.1` | The host interface that nodes publish their exported ports on. `0.0.0.0` publishes on all interfaces. |
| `SSH_FORWARDING_HOST` | *(empty)* | Where this machine answers SSH, as `[user@]IP[:port]`. When set, a running node's menu offers **Copy SSH tunnel command**, an `ssh -L` line forwarding every port the node publishes. Without `user@` the login is your OS login. |
| `K8S_PLATFORM` | the Docker host's | The platform of Kubernetes nodes (K3D and Kubeadm). It must be the Docker host's own architecture: under emulation a node cannot create pods. |
| `GDB_MOUNT_ROOT` | `/srv/coredumps` | The only host directory a Linux Client set up for the [Core Dump Analyzer](CORE_DUMP_ANALYZER.md) may bind-mount from, read-only. |
| `EXPERIMENTAL` | `off` | `on` also offers unfinished features, which are not documented. |
| `EOL` | `off` | `on` also offers end-of-life releases: CentOS 7 as a Linux Client release and a PMM 2 node. |
| `SPOCK_REF` | `v5.0.10` | The pgEdge Spock git ref built for Spock clusters. |
| `DBCANVAS_IDLE_GRACE` | `10m` | How long the service stays up with no stack containers and nothing using it. |
| `DBCANVAS_CATALOG_URL` | the project's repository | Where `dbcanvas versions update` downloads catalogues from (a fork or a mirror). |
| the node passwords | generated | See [Node passwords](#node-passwords). |

`EXPERIMENTAL` and `EOL` accept `on`/`off`; `true`, `1` and `yes` also mean on. Turning either
off again only hides those entries.

## Node passwords

The accounts DBCanvas creates on nodes take their passwords from these settings. On the first run
each one gets a random password, kept encrypted in the database and never generated again.
`dbcanvas passwords` lists them, and `dbcanvas passwords NAME` prints one.

To choose one yourself, set it under `env:` in `config.yaml`. It applies to nodes deployed after
the service restarts; nodes deployed before keep the password they were deployed with until they
are rebuilt. `dbcanvas passwords` marks a password set this way.

| Variable | Used for |
| --- | --- |
| `MYSQL_ROOT_PASSWORD` | `root@localhost` on every MySQL-family node. |
| `MYSQL_ADMIN_PASSWORD` | The network-reachable `admin@'%'` superuser on every MySQL-family node. |
| `APP_PASSWORD` | The application user on PXC nodes. |
| `REPL_PASSWORD` | The replication user (MySQL family and PostgreSQL). |
| `MONITOR_PASSWORD` | The monitoring user for ProxySQL health checks. |
| `CLUSTER_PASSWORD` | The cluster-admin user for ProxySQL's `proxysql-admin`. |
| `CLUSTERCHECK_PASSWORD` | `clustercheck@localhost`, behind the PXC health check on port 9200 that HAProxy polls. |
| `ORCHESTRATOR_PASSWORD` | The `orchestrator@'%'` user that Orchestrator nodes connect as. |
| `POSTGRES_PASSWORD` | The `postgres` superuser on every PostgreSQL node. |
| `MONGODB_ADMIN_PASSWORD` | The admin user on every Percona Server for MongoDB node. |
| `MCLUSTERADMIN_PASSWORD` | The `madmin` account for the MClusterAdmin panel. The panel node's own field overrides it. |
| `MCLUSTERADMIN_RO_PASSWORD` | The read-only `madmin-ro` account, same. |
| `VALKEY_PASSWORD` | The default user on every Valkey node. |
| `PROXYSQL_ADMIN_PASSWORD` | The ProxySQL `admin` user (port 6032). |
| `PMM_PASSWORD` | The `pmm` monitoring user on nodes monitored by a PMM server. |
| `PMM_ADMIN_PASSWORD` | The PMM web UI `admin` login. A password set on the PMM node overrides it. |
| `GRAFANA_PASSWORD` | The `admin` user of the Grafana installed with a CloudNativePG K3D frame. |
| `EVEREST_PASSWORD` | OpenEverest's initial `admin` password on a K3D frame. |
| `KEYCLOAK_PASSWORD` | The Keycloak `admin` console user. |
| `KEYCLOAK_USER_PASSWORD` | The sample Keycloak users created for SSO. |
| `SAMBA_PASSWORD` | The Samba AD DC administrator. It must meet Active Directory complexity (three of: uppercase, lowercase, digit, symbol). |
| `VNC_PASSWORD` | The VNC desktop login. VNC uses only the first 8 characters. A password set on the node overrides it. |

## Architecture

DBCanvas asks Docker for its architecture when the service starts. A node on **Automatic** runs as
that architecture, unless `platform` sets another.

- A database node or cluster can choose amd64 or arm64: its **Architecture** field on the canvas,
  `arch=` in a compose entry, `platform:` in a stack file, or `dbcanvas up --platform`. A choice
  other than the Docker host's runs under emulation, which works, more slowly.
- Each architecture has its own version catalogue (`versions.yaml` for amd64,
  `versions-arm64.yaml` for arm64), so the version pickers offer what that architecture
  publishes.
- K3D and Kubeadm nodes always run as the Docker host's architecture.
- PMM 3 runs as arm64 from 3.10.0; PMM 2 and MClusterAdmin are amd64 only.

`dbcanvas status` and `dbcanvas doctor` show the architecture in use.

## Stack networks

Each stack gets its own Docker network, a /24 that DBCanvas finds free when the network is
created. It overlaps none of:

- the networks on that Docker daemon, whoever created them;
- Docker's default address pools (a network inside one takes the whole pool from everyone who lets
  Docker choose);
- this host's interfaces and, on Linux, its routes;
- the pod and service ranges inside K3D and Kubeadm clusters.

The search covers `10.0.0.0/8`, or `networkRange`, starting at a point derived from the
instance name so that users of one host spread out. Containers get addresses from the lower half
of the /24; K3D's MetalLB load-balancer addresses come from the upper half. A network that exists
keeps its subnet.

A network reached only through the default gateway (a company LAN, a VPN) cannot be seen from the
host. If one is inside the search space, set `networkRange` to a range your network does not use.
`dbcanvas network check` shows what is avoided and where the next stack would go, without creating
anything.

## Node images

Nodes run on DBCanvas's own images: the OS bases (Oracle Linux, Ubuntu, Debian), the Intranet,
the VNC desktop, the Kubeadm node, the K3D diagnostics collector and the demo applications. Each is
tagged with the release that uses it. A deploy that needs one this host does not have pulls it from
`imageRegistry` first; **Validate** says which images it will pull, and one the registry cannot
provide is an error naming the registry's reason.

The Intranet, VNC and K3D collector images can also be built on this host: Validate offers a
**Build** button when one is missing. The Dockerfiles are in `~/.dbcanvas/assets/<version>/images`.

## Settings in the app

**Settings** in the browser holds the settings that are kept in the database. Your own preferences
(theme, look, tabs) are there too.

| Setting | Default | Meaning |
| --- | --- | --- |
| **Node file uploads** | 4 GiB | Largest single file dropped onto a node. 1 MiB–1 TiB. |
| **API token lifetime** | 90 days | The longest expiry a token may be given. 1–365 days. Longer requests are shortened; existing tokens are not. |
| **Writes to internal databases** | off | Whether the [Database Explorer](DATABASE_EXPLORER.md#pmm-server) may write to PMM Server's own databases. |
| **Shared sessions** | off | Whether a stack may be shared with guests through a link, and for how long. See [Shared sessions](SHARED_SESSIONS.md). |
| **Stack history and alerts** | — | How often deployed stacks are sampled for trend lines, the failover timeline and alerts. Off records none. |

## The encryption key

Passwords and other secrets in the database are encrypted with a key generated on the first run,
`~/.dbcanvas/dbcanvas-encryption.key`.

- **Back the key up.** Without it the database cannot be read.
- DBCanvas will not start with a missing or different key. Restore the original key file.

`dbcanvas rotate-key` re-encrypts everything under a new key in one transaction (nothing changes on
error). It stops the service first and keeps the old key as `dbcanvas-encryption.key.old`; delete
that once the new key is backed up.

## Troubleshooting

Start with `dbcanvas doctor`: it checks Docker, the images, the catalogues, the data folder, the
networks, k3d and the service, and says how to fix what fails.

### dbcanvas cannot reach Docker

`dbcanvas doctor` names the socket it tried. If it is the wrong one, set `dockerSocket` in
`config.yaml`. On Linux, "permission denied" means your user cannot open the socket: add it to the
`docker` group (then log in again), or use rootless Docker.

### A version is missing from a picker

The version pickers read the version catalogue built into `dbcanvas`. A release published after
it was built is not in it yet:

```sh
dbcanvas versions update
```

A running service uses the new catalogue from its next deploy. If a series is missing for one OS,
the repository may have no packages for it (for example Percona Server 5.7 on Oracle Linux 10).
A deployed node keeps its version; the new list applies to nodes you add or redeploy.

### The node runs a different minor than I picked

When the repository no longer has the chosen minor (common on MariaDB mirrors), the newest
available build is installed and the node's deploy log says so:

```
note: MariaDB-server has no 10.11.8 build in this repository — installing the newest it has
```

`dbcanvas versions update` brings the pickers in line with the repositories.

### A stack network cannot be created

`dbcanvas network check` shows what rules each range out. On a shared host, give this instance
a range of its own with `networkRange`, and check that no LAN or VPN behind the default gateway
uses it.

### The service does not start

`~/.dbcanvas/dbcanvas.log` has the reason. `dbcanvas status` says whether one runs; only one service
runs per data folder.
