# Configuration & commands

Everything DBCanvas reads from `.env`, every `make` target, and the things that go wrong
often enough to be worth writing down. For what the product *does*, see the
[feature guides](README.md); for how it is built, [Architecture](ARCHITECTURE.md).

## Commands

| Command | What it does |
| --- | --- |
| `make install` | The first run: `images`, then `versions`, then `compose`. Safe to re-run. |
| `make compose` | Create `.env` if missing, build the app image and start it. Enough on its own once the node images exist. |
| `make images` | Build the systemd base images (OS × the one platform this installation targets), plus the pre-baked Intranet and Ubuntu VNC images. Slow — this is the long part of `make install`. |
| `make versions` | Run the built images and record what each can actually install into `versions.yaml`. This is what fills the designer's version pickers. |
| `make up` / `make down` | Start / stop the app without rebuilding. |
| `make restart` | `down`, then `compose`. |
| `make logs` | Follow the app's logs. |
| `make clean` | Stop the app and remove its locally built images. Deployed stacks are untouched. |
| `make smoke` | Render every page off-browser and fail on a render error. |

Single-image rebuilds, for when only one thing changed: `make intranet-image`,
`make vnc-image`, and one per demo app (`make trafficsim-image`, `make hotelsim-image`,
`make airlinesim-image`, `make carsim-image`, `make marketchaos-image`,
`make stocksim-image`).

> **`make images` rewrites `versions.yaml`.** It discards the enrichment `make versions`
> adds, which is why `make install` runs them in that order. If you run `make images` on its
> own, run `make versions` after it.

## `.env`

`make install` (or `make compose`) creates `.env` from [`.env.example`](../.env.example) on
first run. Everything has a working default — but **change the passwords before exposing
anything beyond localhost.**

**App & networking**

| Variable | Default | Meaning |
| --- | --- | --- |
| `APP_HOST` | `127.0.0.1` | Host interface the app's published port binds to. `127.0.0.1` = this machine only; `0.0.0.0` = all interfaces (e.g. your LAN). |
| `APP_PORT` | `8080` | Port the app listens on (host + container). |
| `CONTAINER_BIND_IP` | `127.0.0.1` | Host interface that **deployed stack nodes** publish their exposed ports on (PXC, ProxySQL, Percona Server, PostgreSQL, MongoDB, Valkey, HAProxy, SeaweedFS, PMM, …). `0.0.0.0` publishes on all interfaces. |
| `DOMAIN` | `example.net` | Domain used to configure deployed stacks (Intranet LDAP base DN, DNS, mail, CA). |
| `DEPLOYMENT_TIMEOUT` | `60` | Minutes a provisioner waits for a dependency (cluster / node / shared service) to become ready before failing the deploy. Raise it for large stacks. |
| `DOCKER_PLATFORM` | `linux/amd64` | The platform this installation targets — exactly one of `linux/amd64` or `linux/arm64`. Drives the app image build, and the systemd base images: `make images` builds only this platform and `make versions` only probes/records images on it. |
| `K3D_PLATFORM` | `linux/amd64` | The platform K3D frames' Kubernetes (k3s) nodes target — exactly one of `linux/amd64` or `linux/arm64`. Independent of `DOCKER_PLATFORM`: forcing a k3s node onto a non-native platform runs its own inner containerd/runc under emulation, which can break pod-sandbox creation (e.g. "seccomp is not supported"). Set to `linux/arm64` to run K3D natively on Apple Silicon. |

**Credentials** — passwords for deployed database & service nodes. These are the single
source of truth (they can't be set per-node on the canvas), and a redeploy re-reads them.
Engine-specific variables (`MYSQL_*`, `POSTGRES_*`, `MONGODB_*`, `VALKEY_*`, `PROXYSQL_*`)
apply to that engine only; the rest are shared where relevant.

| Variable | Default | Applies to |
| --- | --- | --- |
| `MYSQL_ROOT_PASSWORD` | `root_password` | `root@localhost` on every MySQL-family node (PXC, MySQL replication, InnoDB/GR, standalone Percona Server). |
| `MYSQL_ADMIN_PASSWORD` | `admin_password` | The network-reachable superuser `admin@'%'` on every MySQL-family node. |
| `POSTGRES_PASSWORD` | `postgres_password` | The `postgres` superuser on every PostgreSQL node (standalone, Patroni, repmgr, Spock). |
| `MONGODB_ADMIN_PASSWORD` | `admin_password` | The admin user on every PSMDB node (standalone / replica set / sharded). |
| `VALKEY_PASSWORD` | `valkey_password` | The default user (`requirepass` / `masterauth`) on every Valkey node. |
| `PROXYSQL_ADMIN_PASSWORD` | `admin_password` | The ProxySQL `admin` user (port 6032) on every ProxySQL node. |
| `APP_PASSWORD` | `app_password` | The application user created on PXC nodes. |
| `REPL_PASSWORD` | `repl_password` | The replication user (MySQL-family + PostgreSQL replication). |
| `MONITOR_PASSWORD` | `monitor_password` | The monitoring user used by ProxySQL's health checks. |
| `CLUSTER_PASSWORD` | `cluster_password` | The cluster-admin user used by ProxySQL's `proxysql-admin`. |
| `CLUSTERCHECK_PASSWORD` | `cluster_password` | `clustercheck@localhost`, backing the PXC `:9200` health endpoint an HAProxy polls. |
| `PMM_PASSWORD` | `pmm_password` | The least-privilege `pmm` monitoring user, created only on nodes associated with a PMM server. |
| `PMM_ADMIN_PASSWORD` | `admin_password` | The PMM server's Grafana `admin` user (the PMM web UI login). A per-node password set on the canvas overrides it. |
| `GRAFANA_PASSWORD` | `grafana_password` | The `admin` user of the Grafana that kube-prometheus-stack installs alongside a CloudNativePG K3D frame. The address and this login appear on the k3s node's panel after deploy. |
| `KEYCLOAK_PASSWORD` | `keycloak_password` | The Keycloak node's `admin` console user. |
| `KEYCLOAK_USER_PASSWORD` | `keycloak_user_password` | The sample Keycloak users (`alice`, `bob`) created when a node enables Keycloak SSO. Demo identities — don't reuse this password for anything real. |
| `SAMBA_PASSWORD` | `SambaPassword2026` | The Samba AD DC administrator, used to provision the domain and to bind for LDAP/Kerberos management. Must satisfy Active Directory complexity (at least three of: uppercase, lowercase, digit, symbol) or provisioning rejects it. |
| `VNC_PASSWORD` | `vnc_password` | The Ubuntu VNC desktop login and VNC access code. VncAuth uses only the first 8 characters, so this authenticates as `vnc_pass`. A per-node password set on the canvas overrides it. |

The container always listens on all interfaces internally; host-side exposure is controlled
by the compose publish binding, not by `APP_HOST` inside the container.

**Advanced (rarely changed)** — set by `docker-compose.yml` or handy for local dev:
`DB_PATH` (SQLite file, default `dbcanvas.db`; the container uses a `/data` volume),
`DOCKER_SOCK` (Docker socket, default `/var/run/docker.sock`), `VERSIONS_FILE` (path to the
`versions.yaml` catalog), and `SPOCK_REF` (the pgEdge/spock git ref built for Spock clusters,
default `v5.0.10`).

**Hybrid (Vagrant) tuning** — environment variables read only when the vagrant backend is
active. All optional; the defaults work out of the box.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DBCANVAS_VAGRANT_ROOT` | `~/.dbcanvas/vagrant` | Working root — one subdirectory per VM (holding its `Vagrantfile`), plus the network and host-port allocation state. |
| `DBCANVAS_VM_CPUS` | `2` | vCPUs for a VM whose node doesn't set its own. Per-node **CPUs** in the designer wins. |
| `DBCANVAS_VM_MEMORY` | `2048` | Memory (MB) for a VM whose node doesn't set its own. Per-node **Memory (GiB)** wins. |
| `DBCANVAS_VM_SUBNET_BASE` | `192.168` | First two octets of the host-only range stacks draw their `/24`s from. VirtualBox only permits `192.168.56.0/21` unless you widen it in `/etc/vbox/networks.conf` — change both together. |
| `DBCANVAS_BOX_<OS>_<VER>` | — | Override the Vagrant box for one OS (dots/dashes → underscores), e.g. `DBCANVAS_BOX_UBUNTU_24_04=my/box`. |
| `DBCANVAS_NO_SUDO` | unset | Run `iptables`/`ip`/`sysctl` directly instead of via `sudo -n`, for hosts that already grant `CAP_NET_ADMIN`. |
| `DBCANVAS_HOST_MODE` | auto | Force "the app runs on the host" detection (normally inferred from `/.dockerenv`). Only needed for odd environments. |

## Recovering an admin password

The app image ships a reset tool, because its runtime is distroless — no shell, no `sqlite3` —
and the database lives on a volume only that container mounts:

```sh
docker exec -it dbcanvas-app-1 dbcanvas_reset_password
```

It names the admin account it is about to change, prompts for a new password and a
confirmation with the echo off, and signs out that account's existing sessions. With more than
one admin, name one with `-user`.

## Troubleshooting

### A minor version is missing from a node's version list

The version pickers don't guess — they read [`versions.yaml`](../versions.yaml), a catalog built
in two passes:

- **`make images`** builds the `dbcanvas-systemd:*` base images (Oracle Linux 8/9/10, Ubuntu
  22.04/24.04, Debian 12/13) and records the image matrix. The Debian bases are offered on the
  **Linux Client** only — it installs nothing, so no product's package path is exercised there;
  every other node type picks from Oracle Linux and Ubuntu. It then builds the two pre-baked
  service images — `dbcanvas-intranet` and `dbcanvas-vnc` — from those bases; they are not
  selectable instances, so they are not recorded in `versions.yaml`.
- **`make versions`** starts a throwaway container per image and asks that OS's own package
  manager what it can actually install (`dnf search --showduplicates` / `apt-cache madison`),
  writing the result back per image, keyed by major series. It also refreshes the PMM, Percona
  operator and k3s tag lists from the registries.

So a point release published *after* your last run simply isn't in the file yet:

```sh
make versions          # re-probe the repos and rewrite versions.yaml
```

Then **reload the browser tab**. The app re-reads `versions.yaml` on every catalog request and
compose mounts it read-only from the repo, so no rebuild or restart is needed — the pickers
pick it up on the next page load.

If a whole **OS or series** is missing rather than one point release:

```sh
make images            # the image must exist before it can be probed
make versions
```

Still empty? Check these, in order:

- **`DOCKER_PLATFORM`** (in `.env`) selects the *one* platform both targets build and probe —
  `linux/amd64` by default. An `arm64` host that never changed it has no arm64 images to probe,
  and the catalog for that arch stays empty.
- **The series really has no packages for that OS.** `make versions` records an empty list
  rather than inventing one — Percona Server 5.7 on Oracle Linux 10 and PXC 8.0 on Oracle
  Linux 10 are genuinely empty, not a probe that failed.
- **`versions.yaml` got mounted as a directory.** If the file was missing when the container was
  first created, Docker helpfully created an empty *directory* at that path and the catalog will
  stay empty forever. Confirm with `ls -ld versions.yaml`, then `make down && make compose` to
  recreate the container against the real file.
- **Running on the host** (hybrid or dev), make sure `VERSIONS_FILE` points at the repo's
  `versions.yaml`. If no catalog file is found at all, the database pickers come up **empty**
  (only PMM and k3s have built-in fallbacks).

A node that was *already deployed* keeps the version it deployed with; the new list applies to
the next node you add or redeploy.

### A hybrid stack deployed everything as containers

The backend is decided on a stack's **first deploy** and pinned for life, and a vagrant request
falls back to Docker when the engine isn't available. Check, in order: the stack is new (an
existing stack keeps the backend it was first deployed with — make a new one); DBCanvas is
running **on the host**, not via `make compose`; and `vagrant`, `VBoxManage` and `ssh` are all
on the `PATH` of the process that runs it.

### VM nodes and container nodes can't reach each other

Cross-engine routing needs to install iptables rules on the host — see the
[hybrid quick start](#hybrid-vagrant--virtualbox). Without `sudo -n` (or root, or
`DBCANVAS_NO_SUDO=1` with `CAP_NET_ADMIN`), both halves of the stack come up healthy but stay
isolated: DNS lookups against the Intranet time out and PMM shows the VM nodes as down.
