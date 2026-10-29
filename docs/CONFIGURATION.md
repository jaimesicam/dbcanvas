# Configuration & commands

What DBCanvas reads from `.env`, every `make` target, the settings an administrator changes in
the app, and fixes for common problems.

> [!IMPORTANT]
> **DBCanvas is for previewing features, testing and learning — not for production.** The
> credential defaults are published in `.env.example` and the nodes are not hardened. Change the
> passwords before anyone else can reach the installation.

## Commands

| Command | What it does |
| --- | --- |
| `make install` | First run: `make images`, then the optional images, then `make compose`. Safe to re-run. An optional image that fails is reported and skipped; DBCanvas still starts. Does not run `make versions`. |
| `make compose` | Create `.env` if missing, build the app image and start it. The everyday command once the node images exist. |
| `make up` / `make down` | Start / stop the app without rebuilding. |
| `make restart` | `make down`, then `make compose`. |
| `make build` | Build the app image only. |
| `make env` | Create `.env` from `.env.example` if it is missing. Run by `compose`, `build` and `up`. |
| `make logs` | Follow the app's logs. |
| `make clean` | Stop the app, remove its locally built images and `dist/`. Deployed stacks are untouched. |
| `make images` | Build what every stack needs: the OS base images (Oracle Linux 8/9/10, Ubuntu 22.04/24.04, Debian 12/13) for the one platform in `DOCKER_PLATFORM`, and the **Intranet** image. Slow — the long part of `make install`. If only the Intranet fails, retry with `make intranet-image` or the **Build** button that Validate offers. |
| `make extra-images` | Build every optional image: the Ubuntu VNC desktop, the K3D diagnostics collector, the Big Hole FTDC viewer and the demo applications. Fails if any of them fails (unlike `make install`). |
| `make versions` | Probe every built image for the versions it can install and write `versions.yaml` — what the version pickers offer. Takes hours. **Run `make restart` afterwards**, or every picker is empty. `ONLY=percona` or `ONLY=upstream` re-probes one group and keeps the other. |
| `make adopt-images` | After an upgrade, retag images this host already built with the current release instead of rebuilding them. Nothing is rebuilt or removed. |
| `make rotate-key` | Re-encrypt stored secrets under a new key. See [The encryption key](#the-encryption-key). |
| `make cli` | Cross-compile `dbcanvas-cli` into `dist/` for Linux (amd64, arm64), macOS (amd64, arm64) and Windows (amd64), with `SHA256SUMS`. See [`dbcanvas-cli`](CLI.md). |
| `make cli-test` | Build, vet and test the CLI. |
| `make smoke` | Render every page and fail on a render error; also loads the heavy pages in headless Chrome when Chrome is available (`CHROME=<path>` to point at it). |

Rebuild one image: `make intranet-image`, `make vnc-image`, `make k8scollector-image`,
`make bighole-image`, or one demo application — `make supportsim-image`, `make stocksim-image`,
`make ledgersim-image`.

When Validate reports the Intranet, VNC, K3D collector or Big Hole image missing, an
administrator gets a **Build** button that builds it from the web interface. The demo
applications need a checkout and their `make` target.

## `.env`

`make compose` creates `.env` from [`.env.example`](../.env.example) on first run. Every setting
has a working default — **change the passwords before exposing anything beyond localhost.**
Restart the app (`make restart`) after editing `.env`.

### App and networking

| Variable | Default | Meaning |
| --- | --- | --- |
| `DBCANVAS_INSTANCE` | `dbcanvas-<login>` | This installation's name on the Docker daemon: its compose project, app image and volumes, and the prefix of every container, network and K3D cluster it deploys. Lets several people share one Docker host — each also needs their own `APP_PORT`. `make env` sets it to `dbcanvas-<your login>` for a new `.env`, and to `dbcanvas` for an existing `.env` without it. Lowercase letters, digits and single dashes, starting with a letter, at most 40 characters; no dash-separated part may be only digits, or `s` followed by digits. **Changing it starts the installation with new, empty volumes** (the old ones are kept). |
| `APP_HOST` | `127.0.0.1` | Host interface the app is published on. `0.0.0.0` exposes it on all interfaces. |
| `APP_PORT` | `8080` | Port the app listens on. |
| `PUBLIC_URL` | *(empty)* | The address other people use to reach this installation (e.g. `http://10.0.4.12:8080`). Shared-session links are built on it; empty uses the address your browser used. Shared sessions are off until an administrator enables them in **Settings → Shared sessions**. |
| `CONTAINER_BIND_IP` | `127.0.0.1` | Host interface that deployed nodes publish their exported ports on. `0.0.0.0` publishes on all interfaces. |
| `SSH_FORWARDING_HOST` | *(empty)* | Where this server answers SSH, as `IP[:port]` (port defaults to 22). When set, right-clicking a running node offers **Copy SSH tunnel command**: an `ssh -L` line that forwards every port the node publishes to the same port on your machine. With no `user@`, the login is the name of whoever is signed in to DBCanvas; write `user@IP[:port]` to use one login for everybody. Empty = off. |
| `DOMAIN` | `example.net` | Domain of deployed stacks (Intranet DNS, LDAP base DN, mail, CA). |
| `DEPLOYMENT_TIMEOUT` | `60` | Minutes a deploy waits for a dependency (cluster, node, shared service) to become ready before failing. Raise it for large stacks. |
| `DOCKER_PLATFORM` | `linux/amd64` | `linux/amd64` or `linux/arm64`. The platform of the app image and the OS base images; `make images` builds and `make versions` probes only this platform. |
| `K3D_PLATFORM` | `linux/amd64` | `linux/amd64` or `linux/arm64`. The platform of K3D frames' k3s nodes, independent of `DOCKER_PLATFORM`. Set `linux/arm64` on Apple Silicon — an emulated k3s node can fail to create pods (e.g. "seccomp is not supported"). |
| `GDB_MOUNT_ROOT` | `/srv/coredumps` | The only host directory a Linux Client set up for the [Core Dump Analyzer](CORE_DUMP_ANALYZER.md) may bind-mount from. Mounts are read-only; a path outside it fails validation. |
| `EXPERIMENTAL` | `off` | `on` also offers unfinished features, which are not documented. |
| `EOL` | `off` | `on` also offers end-of-life releases: **CentOS 7** as a Linux Client release (repositories pointed at vault.centos.org, no systemd) and a **PMM2** monitoring node (PMM 2.25.0–2.44.1, not connected to other nodes). |

`EXPERIMENTAL` and `EOL` accept `on`/`off`; `true`, `1` and `yes` also mean on. Turning either
off again only hides those entries — anything already built with them is left alone.

K3D cluster names have the form `<frame>-s<stack>-<instance tag>`, where the tag is
`DBCANVAS_INSTANCE` without its leading `dbcanvas-` (just `dbcanvas` for the installation of
that name).

### Credentials

Passwords for deployed nodes come only from `.env` — they cannot be set per node on the canvas,
except where noted — and a redeploy re-reads them. Variables named after an engine apply only to
that engine.

| Variable | Default | Used for |
| --- | --- | --- |
| `MYSQL_ROOT_PASSWORD` | `root_password` | `root@localhost` on every MySQL-family node (PXC, replication, InnoDB/Group Replication, standalone Percona Server). |
| `MYSQL_ADMIN_PASSWORD` | `admin_password` | The network-reachable `admin@'%'` superuser on every MySQL-family node. |
| `APP_PASSWORD` | `app_password` | The application user on PXC nodes. |
| `REPL_PASSWORD` | `repl_password` | The replication user (MySQL family and PostgreSQL). |
| `MONITOR_PASSWORD` | `monitor_password` | The monitoring user for ProxySQL health checks. |
| `CLUSTER_PASSWORD` | `cluster_password` | The cluster-admin user for ProxySQL's `proxysql-admin`. |
| `CLUSTERCHECK_PASSWORD` | `cluster_password` | `clustercheck@localhost`, behind the PXC health check on port 9200 that HAProxy polls. |
| `ORCHESTRATOR_PASSWORD` | `orchestrator_password` | The `orchestrator@'%'` user that Percona Orchestrator nodes connect as. |
| `POSTGRES_PASSWORD` | `postgres_password` | The `postgres` superuser on every PostgreSQL node. |
| `MONGODB_ADMIN_PASSWORD` | `admin_password` | The admin user on every Percona Server for MongoDB node. |
| `MCLUSTERADMIN_PASSWORD` | `madmin_password` | The `madmin` account for the MClusterAdmin panel. The panel node's own field overrides it. |
| `MCLUSTERADMIN_RO_PASSWORD` | `madmin_ro_password` | The read-only `madmin-ro` account, same. |
| `VALKEY_PASSWORD` | `valkey_password` | The default user on every Valkey node. |
| `PROXYSQL_ADMIN_PASSWORD` | `admin_password` | The ProxySQL `admin` user (port 6032). |
| `PMM_PASSWORD` | `pmm_password` | The `pmm` monitoring user on nodes monitored by a PMM server. |
| `PMM_ADMIN_PASSWORD` | `admin_password` | The PMM web UI `admin` login. A password set on the PMM node overrides it. |
| `GRAFANA_PASSWORD` | `grafana_password` | The `admin` user of the Grafana installed with a CloudNativePG K3D frame. Shown on the k3s node's panel. |
| `EVEREST_PASSWORD` | `everest_password` | OpenEverest's initial `admin` password on a K3D frame. Shown on the k3s node's panel. |
| `KEYCLOAK_PASSWORD` | `keycloak_password` | The Keycloak `admin` console user. |
| `KEYCLOAK_USER_PASSWORD` | `keycloak_user_password` | The sample Keycloak users created for SSO (`alice` and `bob` for PMM; `jane` and `john` for PostgreSQL and Percona Server). |
| `SAMBA_PASSWORD` | `SambaPassword2026` | The Samba AD DC administrator. Must meet Active Directory complexity (three of: uppercase, lowercase, digit, symbol). |
| `VNC_PASSWORD` | `vnc_password` | The Ubuntu VNC desktop login. VNC uses only the first 8 characters. A password set on the node overrides it. |

### Advanced

Set by `docker-compose.yml`, or useful when running DBCanvas on the host:

| Variable | Default | Meaning |
| --- | --- | --- |
| `DB_PATH` | `dbcanvas.db` (container: `/data/dbcanvas.db`) | The SQLite database. Shared-session recordings are kept beside it. |
| `DBCANVAS_ENCRYPTION_KEY_PATH` | beside the database (container: `/keys/dbcanvas-encryption.key`) | The encryption key. |
| `DOCKER_SOCK` | `/var/run/docker.sock` | The Docker socket. |
| `VERSIONS_FILE` | `versions.yaml` in the current or parent directory (container: `/etc/dbcanvas/versions.yaml`) | Path to the version catalog. |
| `IMAGES_FILE` | `images.yaml`, found the same way | Path to the OS image list; falls back to the version catalog. |
| `SPOCK_REF` | `v5.0.10` | The pgEdge Spock git ref built for Spock clusters. |

### Hybrid (Vagrant) backend

Read only when DBCanvas runs on the host with Vagrant available. All optional.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DBCANVAS_VAGRANT_ROOT` | `~/.dbcanvas/vagrant` | Working directory for VMs and network state. |
| `DBCANVAS_VM_CPUS` | `2` | vCPUs for a VM whose node sets none. The node's **CPUs** field wins. |
| `DBCANVAS_VM_MEMORY` | `2048` | Memory (MB) for a VM whose node sets none. The node's **Memory (GiB)** field wins. |
| `DBCANVAS_VM_SUBNET_BASE` | `192.168` | First two octets of the host-only networks. VirtualBox allows only `192.168.56.0/21` unless widened in `/etc/vbox/networks.conf` — change both together. |
| `DBCANVAS_BOX_<OS>_<VER>` | — | Override the Vagrant box for one OS, dots and dashes as underscores, e.g. `DBCANVAS_BOX_UBUNTU_24_04=my/box`. |
| `DBCANVAS_NO_SUDO` | unset | `1` runs `iptables`/`ip`/`sysctl` directly instead of through `sudo -n` (needs `CAP_NET_ADMIN`). |
| `DBCANVAS_HOST_MODE` | auto | `1` forces "running on the host" detection. |

## Instance settings (in the app)

Administrators change these in **Settings**; they apply to everyone.

| Setting | Default | Meaning |
| --- | --- | --- |
| **Node file uploads** | 4 GiB | Largest single file drop onto a node. 1 MiB–1 TiB. |
| **API token lifetime** | 90 days | Longest expiry a non-administrator may give an [API token](API.md). 1–365 days. Longer requests are shortened; existing tokens are not. Only administrators can create a token that never expires. |
| **Writes to internal databases** | off | Whether the [Database Explorer](DATABASE_EXPLORER.md#pmm-server) may write to PMM Server's own databases. |
| **Registration** | approval required | Whether a newly registered account can sign in at once or waits for an administrator. |
| **Shared sessions** | off | Whether stack owners may share a live session with guests through a link, and for how long. |
| **Stack history and alerts** | — | How often deployed stacks are sampled for trend lines, the failover timeline and alerts. Off records none. |

## The encryption key

Passwords and secrets in the database are encrypted with a key generated on first start and kept
in its own file: `/keys/dbcanvas-encryption.key` on the app's key volume (or beside the database
when DBCanvas runs on the host).

- **Back the key up separately from the database.** Without it the database cannot be read.
- DBCanvas will not start with a missing or different key. Restore the original key file.
- `docker compose down -v` deletes both volumes — the database and the key.

To re-encrypt everything under a new key:

```sh
make rotate-key
```

It stops the app, re-encrypts every value in one transaction (nothing changes on error), keeps
the old key as `dbcanvas-encryption.key.old`, and starts the app. Delete the `.old` file once the
new key is backed up.

## Recovering an admin password

Run the reset tool inside the app container. The container is named `<DBCANVAS_INSTANCE>-app-1`:

```sh
docker exec -it dbcanvas-alex-app-1 dbcanvas_reset_password
```

It names the admin account it will change, prompts twice for the new password, and signs that
account out everywhere. With more than one admin, choose one with `-user <name>`.

## Troubleshooting

### A version is missing from a picker

The pickers read two files generated on the host:

- **`images.yaml`**, written by `make images` — the OS images that exist, which the **OS**
  pickers offer. Debian is offered only on the Linux Client; other node types use Oracle Linux
  or Ubuntu.
- **`versions.yaml`**, written by `make versions` — what each OS image can install, which the
  **version** pickers offer. It also holds the PMM, operator, Helm chart and k3s versions.

A release published after the last `make versions` is not in the file yet:

```sh
make versions          # hours
make restart           # required — until then every version picker is empty
```

`make compose` alone does not pick up the new file; `make restart` does. Running stacks and the
database are not affected.

A whole OS or series missing:

```sh
make images
make versions
make restart
```

Still empty? Check:

- **`DOCKER_PLATFORM`** — only that platform's images are built and probed.
- **The series has no packages for that OS** — the list is empty because the repository is
  (for example Percona Server 5.7 or PXC 8.0 on Oracle Linux 10).
- **`versions.yaml` or `images.yaml` is a directory.** If a file was missing when the container
  was created, Docker created a directory in its place. Check with
  `ls -ld versions.yaml images.yaml`, remove the directory, then `make env && make restart`.
- **Running on the host:** point `VERSIONS_FILE` and `IMAGES_FILE` at the repository's files.
  Without a catalog the database version pickers are empty.

A deployed node keeps its version; the new list applies to nodes you add or redeploy.

### The node runs a different minor than I picked

When the repository no longer has the chosen minor (common on MariaDB mirrors), the newest
available build is installed and the node's deploy log says so:

```
note: MariaDB-server has no 10.11.8 build in this repository — installing the newest it has
```

Run `make versions` and `make restart` to bring the pickers in line with the repository.

### A hybrid stack deployed everything as containers

A stack's backend is fixed at its first deploy, and falls back to Docker when Vagrant is not
available. Check that the stack is new, that DBCanvas runs **on the host** (not through
`make compose`), and that `vagrant`, `VBoxManage` and `ssh` are on its `PATH`.

### VM nodes and container nodes cannot reach each other

Routing between VMs and containers needs iptables rules on the host — see
[Deployment backends](STACKS.md#deployment-backends--docker-or-vagrant-hybrid). Without
`sudo -n`, root, or `DBCANVAS_NO_SUDO=1` with `CAP_NET_ADMIN`, both halves come up but stay
isolated: DNS lookups against the Intranet time out and PMM shows the VM nodes as down.
