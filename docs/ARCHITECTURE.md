# Architecture

How DBCanvas is put together. If you are here to *use* it, start at the
[README](../README.md) and the [feature guides](README.md) — nothing on this page is needed
to run a stack.

## The shape of it

The control plane is one small Go binary. It serves the embedded React SPA **and** the JSON
API on a single port, keeps its own metadata in SQLite, and talks to the Docker daemon to
provision stack containers alongside itself.

```
                    ┌───────────────────────── your Docker host ──┐
browser ──HTTP──>  DBCanvas (Go binary, :APP_PORT)                │
                   ├─ serves embedded React SPA (//go:embed)      │
                   ├─ /api/*  ──> SQLite (/data, Docker volume)   │
                   └─ Docker Engine API (/var/run/docker.sock)    │
                          │  creates / execs / monitors           │
                          ▼                                       │
                   stack containers: pg · patroni · pxc · psmdb ·  │
                   valkey · intranet · pmm · proxysql · haproxy ·  │
                   seaweedfs · …                                   │
                   └──────────────────────────────────────────────┘
```

There is no agent, no message bus and no scheduler. A deploy is the Go process making Docker
API calls and running commands inside the containers it created; progress is streamed back to
the browser as it goes.

### Why the Docker socket

`/var/run/docker.sock` is mounted into the app so it can create and manage the stack's
containers. That is a privileged capability — a process that can drive the daemon can do
anything the daemon can. Run DBCanvas somewhere you trust, and keep `APP_HOST` bound to
`127.0.0.1` unless you have a reason not to.

## Two engines: Docker and Vagrant

The default backend is Docker: every node is a container. In **hybrid** mode the same binary
runs *on the host* and drives two engines at once — Docker for infrastructure nodes,
Vagrant/VirtualBox for the OS and database nodes — bridging the two networks so every node
still resolves and reaches every other.

```
                    ┌───────────────────────── your host ─────────┐
browser ──HTTP──>  DBCanvas (Go binary on the host, :APP_PORT)    │
                   ├─ Docker Engine API ──> intranet · pmm ·      │
                   │                        keycloak · k3d · …    │
                   │                        (bridge 172.x)        │
                   └─ vagrant / VBoxManage / ssh ──> pg · pxc ·   │
                                            psmdb · valkey · …    │
                                            (host-only 192.168.5x)│
                          host iptables + routes join the two ────┘
```

Both engines sit behind one interface, so a provisioner does not know which it is talking to:
it creates a node, copies files in, runs commands and reads output the same way either side.
The cross-engine routing installs `iptables` rules on the host, which is why hybrid is
Linux-only in practice.

Vagrant needs `vagrant`, `VBoxManage` and `ssh` on the `PATH` of the DBCanvas *process* — so
hybrid means running the binary on the host rather than in its container.

## Node images are pre-built, not assembled at deploy

Deployed nodes run systemd inside a container so that a database behaves the way it does on a
real host — services, journald, unit files. Those base images are built ahead of time by
`make images` (`images/build.sh`), one per OS × platform, and the pre-baked Intranet and
Ubuntu VNC images by `images/service.sh`.

An installation targets **exactly one platform**. `DOCKER_PLATFORM` selects it,
`make images` builds only that, and `make versions` only probes and records it. There is
never more than one architecture of a node image on disk, which is why nothing in the
designer asks you to pick one.

## The version catalogue

`versions.yaml` is a catalogue, not a lockfile: `make versions` (`images/versions.sh`) runs
the built images and asks each package manager what it can actually install, then records it.
The designer's version pickers are populated from that file, so every version offered is one
that the image in front of you can install today.

The same file carries Helm chart versions, operator tags, k3s releases and the image tags
chart-installed operators are pointed at. Each section names how it was discovered and how to
refresh it.

## A stack is a design, then a deployment

A **design** is what the canvas holds: nodes, frames (a frame is a cluster — its members are
nodes inside it), edges (links between ports), and per-node settings. It is stored as JSON.

A **deployment** is what exists on the host: one row per node, with the container id, the
resolved configuration, generated secrets and a progress log. Deploying a stack walks the
design, provisions what is missing, and writes a deployment row per node as it goes.

That split is why redeploying is not destructive: the design is edited freely, and a deploy
reconciles the difference. It is also why the panels can show what a node *is* rather than
what it was asked to be — they read the deployment, not the design.

## Kubernetes frames

A K3D frame creates a real k3s cluster with `k3d`, on the same Docker network as the rest of
the stack, so pods resolve the Intranet's DNS and can reach PMM and SeaweedFS by name.
DBCanvas then installs an operator into it — the four Percona ones from their release
manifests, CloudNativePG and Crunchy PGO from Helm charts through k3s' own helm-controller,
so no `helm` binary is involved.

LoadBalancer addresses come from MetalLB in L2 mode. Several clusters can share one stack
network, so each gets its own block of 8 addresses from the top of the subnet: the block is
chosen from the frame's position among the stack's K3D frames (frames deploy concurrently, so
a shared-state lookup would race), and recorded ranges are stepped around for clusters that
were added later.

## Where state lives

| What | Where |
| --- | --- |
| Users, sessions, stacks, designs, deployments, notifications | SQLite (`DB_PATH`, a Docker volume at `/data`) |
| Generated credentials for a deployed node | The deployment row, shown in the node's panel |
| What a deploy actually applied (K8s manifests) | Archived on the cluster's first node, numbered in apply order |
| Node images and the version catalogue | The Docker image store and `versions.yaml` |

Everything a stack needs is derived from the design plus `.env` at deploy time, so a redeploy
re-reads both. Passwords are not stored per-node on the canvas.

## Security model

- **Authentication** is username + password with bcrypt hashes and server-side sessions;
  registration is approval-gated, and an admin approves accounts.
- **Locked out?** The app image ships `dbcanvas_reset_password`, because the runtime is
  distroless — no shell, no `sqlite3` — and the database is on a volume only that container
  mounts. See [Configuration](CONFIGURATION.md#recovering-an-admin-password).
- **Stack credentials** come from `.env` and are the same for every node of a kind. They are
  lab credentials with working defaults; change them before exposing anything beyond
  localhost.
- **Exposure** is two separate settings: `APP_HOST` for the app itself and
  `CONTAINER_BIND_IP` for the ports deployed nodes publish. Both default to `127.0.0.1`.
- **The Docker socket** is the real trust boundary — see above.

## The frontend

React + Vite, built to static files that the Go binary embeds with `//go:embed`. There is no
separate web server and no CDN: one binary serves the SPA and the API on one port, which is
also why the whole thing is a single image.

Icons are hand-written inline SVG rather than an icon package, so they inherit `currentColor`
and stay legible at the sizes the sidebar actually uses.

## Testing

- `go test ./...` — unit tests beside the code. Many encode a fact learned from a live system
  (a version ceiling, a label an operator selects by, a repository spelling) so the reason
  survives with the assertion.
- `make smoke` — renders every React page off-browser and fails on a render error, plus a set
  of checks over the designer's own model.
- Neither replaces deploying the thing: anything that talks to a real database, operator or
  package manager is verified on a live stack before it is called done.

## Local development

Two terminals (Docker is still required for provisioning stacks):

```sh
# terminal 1 — Go API + SQLite (needs the Docker socket to provision stacks)
cd app && APP_PORT=8080 DB_PATH=./dbcanvas.db VERSIONS_FILE=../versions.yaml go run .

# terminal 2 — Vite dev server (proxies /api → :8080)
cd app/web && npm install && npm run dev
```

The Go server binds `APP_HOST` (default `127.0.0.1`), so a bare `go run` stays private to your
machine. Load `.env` first (`set -a; . ../.env; set +a`) to get the same passwords and
`DOMAIN` that compose would pass.

This is also the shape hybrid runs in — a host process with `vagrant`/`VBoxManage` on `PATH` —
except that hybrid serves the SPA from the binary (`npm run build`, then `go build`) rather
than from the Vite dev server.

## Tech stack

Go (standard library HTTP, `modernc.org/sqlite`, no ORM) · React + Vite + Tailwind ·
Docker Engine API over the unix socket, no SDK · k3d/k3s for Kubernetes frames ·
Vagrant + VirtualBox for the hybrid backend.
