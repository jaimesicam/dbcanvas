# Architecture

How DBCanvas is put together. If you are here to *use* it, start at the
[README](../README.md) and the [feature guides](README.md) — nothing on this page is needed
to run a stack.

## The shape of it

DBCanvas is one Go binary, `dbcanvas`, run by one person (the owner) on their own Mac or Linux
machine. It serves the embedded React SPA **and** the JSON API on one port, keeps its own
metadata in SQLite, and talks to the Docker Engine API to provision stack containers. Docker is
the only engine.

```
                    ┌──────────────────────────── your machine ───┐
browser ──HTTP──>  dbcanvas service (127.0.0.1:8080)              │
dbcanvas ls/up/… ─> ~/.dbcanvas/dbcanvas.sock (control socket)    │
                   ├─ serves embedded React SPA (//go:embed)      │
                   ├─ /api/*  ──> SQLite (~/.dbcanvas/dbcanvas.db)│
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

### The background service

There is no command to start it. A command that needs it (`dbcanvas`, which opens the browser,
`up`, `ls`, …) starts the binary as a detached background service when it is not running
(`app/soloservice.go`), waits for it to answer on the control socket, and returns. One service
runs per data folder: it holds a lock there, and a second one that finds the lock taken exits.

- **Control socket.** The command line talks to the service over
  `~/.dbcanvas/dbcanvas.sock` (`app/ctlsock.go`). The data folder and the socket are the
  owner's alone (0700), and that is the socket's authentication: a request on it acts as the
  owner.
- **Browser port.** It listens on `127.0.0.1` only, port 8080 (`port` in config.yaml); when
  that port is taken a free one is chosen. The browser is signed in with a start link the
  command opens. A browser on another machine comes through the owner's own reverse proxy or
  SSH tunnel, with `publicUrl` set.
- **Idle stop.** It stops by itself once nothing needs it — no stack has a container, no
  deploy is running, nothing is connected (a browser, a shared session's guests, a command
  waiting on a deploy) — and that has lasted 10 minutes (`DBCANVAS_IDLE_GRACE`). After a
  reboot it starts with the next command and catches up, removing what expired while it was
  down. `dbcanvas status` says what keeps it up; `dbcanvas stop` stops it.
- **Binary updates.** When the file it runs from is replaced, it restarts as the new binary
  once no deploy is running and nothing is connected.
- **Embedded files.** Besides the web app, the binary carries the version catalogue
  (`app/catalogembed.go`) and a bundle (`app/bundle.go`): the node images' Dockerfiles, Pager
  Panic's banks, the licence texts and Big Hole. The bundle is unpacked once per version into
  `~/.dbcanvas/assets/<version>`. Its log is `~/.dbcanvas/dbcanvas.log`.

### Why the Docker socket

DBCanvas drives the Docker daemon through its socket (`dockerSocket` in config.yaml) to create
and manage the stack's containers. That is a privileged capability — a process that can drive
the daemon can do anything the daemon can. The browser port listens on `127.0.0.1` only for that
reason.

### Reaching nodes

On a Linux host the host routes to Docker's bridge networks, and DBCanvas dials container
addresses directly. On macOS (Docker Desktop, Rancher Desktop) Docker's networks live inside a
VM, so a connection is tunnelled through an exec in a container on the stack's network — a relay
over the Docker API (`app/nodedial.go`, `app/noderelay.go`). Container IPs need not be routable
from the host.

### Stack networks

Each stack gets its own Docker network, a free /24 found at creation in 10.0.0.0/8 (or
`networkRange`), avoiding every network on that Docker, Docker's default address pools, the
host's own interfaces and routes, and the ranges Kubernetes uses inside a cluster
(`app/subnets.go`). Containers get the lower half, less its top 16 addresses, which are kept for
VM nodes; K3D MetalLB blocks come from the upper half.

### VM nodes

A node can run as a Lima virtual machine instead of a container (`app/lima.go`). A stack with VM
nodes uses an engine router that creates those nodes on Lima and everything else on Docker
(`app/engine_router.go`), so provisioning is the same code either way. A VM reaches the stack's
Docker network through the Intranet: dbcanvas runs the relay's TUN mode in the Intranet and in the
VM and copies packets between them; the Intranet routes the VM's address and answers ARP for it
(`app/vmnet.go`). Nothing on the host changes. See [VM_NODES.md](VM_NODES.md).

## Node images are pre-built, not assembled at deploy

Deployed nodes run systemd inside a container so that a database behaves the way it does on a
real host — services, journald, unit files. Those base images are built ahead of time, one per
OS × platform, and published to the image registry (`imageRegistry`, default
`ghcr.io/jaimesicam`). A deploy pulls a node image that is missing from the Docker host. In a
source checkout, `make images` (`images/build.sh`) builds them locally instead.

`make images` then builds everything that sits on top of the bases. The Intranet, the VNC
desktop, the K3D collector and the Kubeadm node come from `images/service.sh`. Their package
sets never vary, so deploying one is configuration only. The Kubeadm node is built for
`K8S_PLATFORM` rather than `DOCKER_PLATFORM`, because a Kubernetes node has to run natively.
The five demo application images come from `images/apps.sh`. They are Go binaries (Ledger Sim
is a jar) with their frontends embedded: no OS matrix, one tag apiece, and nothing to record in
the version catalogue. `make pull-images` fetches the same set from the registry
(`scripts/registry.sh`) instead of building it.

Those builds fetch from npm, GitHub, Maven Central and Percona's repositories, so they can fail
for reasons that have nothing to do with the machine running them. A failure is reported, and
the rest still build.

That is why you can build most of them from the web interface at the moment a Validate
says one is missing (`app/extraimages.go`). It works because no `images/*.Dockerfile` copies
anything from its build context — every `COPY` is `--from=<stage>` — so the Dockerfile *is* the
context, and the app carries a copy. The build itself runs in a throwaway `docker:cli` container
with the daemon's socket mounted, the same shape as the K3D collector, because the Engine API's
own `/build` endpoint is the classic builder, which sets neither `BUILDPLATFORM` nor
`TARGETARCH`, the two variables a cross-compiling Dockerfile relies on. The Dockerfiles come from
the bundle unpacked into `~/.dbcanvas/assets/<version>`. The demo applications build only in a
source checkout (`make <app>-image`): their context is a directory of this repository's source.

Each node has an **Architecture**: Automatic (the Docker host's) or amd64/arm64. The host's
architecture is read from Docker at start-up and is the default (`platform` in config.yaml
overrides it). K3D and Kubeadm nodes always run native. Each architecture has its own version
catalogue, `versions.yaml` for amd64 and `versions-arm64.yaml` for arm64.

## The image matrix and the version catalogue

Two generated files, written by two commands that never overwrite each other's work:

- `images.yaml` — what `make images` (`images/build.sh`) built: one entry per OS × version
  and architecture. The OS pickers read this.
- `versions.yaml` / `versions-arm64.yaml` — a catalogue, not a lockfile: `make versions`
  (`images/versions.sh`) runs each
  of those images and asks its package manager what it can actually install, then records it. The
  version pickers read this, so every version offered is one that the image in front of you can
  install today. The same file carries Helm chart versions, operator tags, k3s releases and the
  image tags chart-installed operators are pointed at; each section names how it was discovered
  and how to refresh it.

They are separate files because they cost two very different things to produce. Rebuilding an
image is routine; probing every repository on every image takes hours, so `make images` never
touches the catalogue and `make versions` is run deliberately.

The catalogue committed to the repository is copied into `app/catalog` (`make catalog`) and
embedded in the binary. `dbcanvas versions update` downloads a newer one into `~/.dbcanvas`,
which is read in preference to the embedded copy; `dbcanvas versions reset` goes back to it.

## A stack is a design, then a deployment

A **design** is what the canvas holds: nodes, frames (a frame is a cluster — its members are
nodes inside it), edges (links between ports), and per-node settings. It is stored as JSON.

A **deployment** is what exists on the host: one row per node, with the container id, the
resolved configuration, generated secrets and a progress log. Deploying a stack walks the
design, provisions what is missing, and writes a deployment row per node as it goes.

That split is why redeploying is not destructive: the design is edited freely, and a deploy
reconciles the difference. It is also why the panels can show what a node *is* rather than
what it was asked to be — they read the deployment, not the design.

### Host ports

Every TCP port a node publishes on the host (a PMM UI, a database export, a VNC desktop, the
Intranet's webmail) is chosen by DBCanvas, pinned, and recorded (`app/hostports.go`):

- **Chosen on the host.** DBCanvas binds candidate ports at `CONTAINER_BIND_IP` itself, on the
  machine it runs on, and keeps the ones it could.
- **Pinned.** The container is created with that explicit host port, so its URL survives a restart
  (a port Docker picks itself changes on every start).
- **Recorded.** The `host_ports` table says which container owns each port, running or stopped, so
  no other node is given it, even by two deploys at once (one lock serializes the choice). A node
  keeps its ports across stop, start and redeploy; deleting the node, destroying or deleting the
  stack releases them; the table is rebuilt from Docker's containers at start-up.
- **Re-checked.** On every create the ports are tested again. A port DBCanvas chose is replaced if
  something now holds it, and the node records the new one (the app log says why). A port a user
  typed — an export port — is never moved: the deploy fails naming who holds it, another node or
  another program on the host.

## Kubernetes frames

A K3D frame creates a real k3s cluster with `k3d`, on the same Docker network as the rest of
the stack, so pods resolve the Intranet's DNS and can reach PMM and SeaweedFS by name.
DBCanvas then installs an operator into it — the four Percona ones from their release
manifests, CloudNativePG and Crunchy PGO from Helm charts through k3s' own helm-controller,
so no `helm` binary is involved.

LoadBalancer addresses come from MetalLB in L2 mode. Several clusters can share one stack
network, so each gets its own block of 8 addresses from the upper half of the stack's /24
(Docker gives containers the lower half): the block is
chosen from the frame's position among the stack's K3D frames (frames deploy concurrently, so
a shared-state lookup would race), and recorded ranges are stepped around for clusters that
were added later.

## Where state lives

Everything is in the data folder, `~/.dbcanvas` (`DBCANVAS_DATA` overrides it).

| What | Where |
| --- | --- |
| Stacks, designs, deployments, sessions, tokens, notifications | SQLite, `~/.dbcanvas/dbcanvas.db` |
| The key the database's credentials are encrypted with | Its own file, `~/.dbcanvas/dbcanvas-encryption.key` |
| Settings | `~/.dbcanvas/config.yaml`, read when the service starts |
| Node account passwords (`APP_PASSWORD`, `PMM_ADMIN_PASSWORD`, …) | Generated on first run, sealed in the database; `dbcanvas passwords` shows them |
| Generated credentials for a deployed node | The deployment row, shown in the node's panel |
| What a deploy actually applied (K8s manifests) | Archived on the cluster's first node, numbered in apply order |
| Node images and the version catalogue | The Docker image store; the catalogue embedded in the binary, or an updated one in `~/.dbcanvas` |
| The bundled files, the service log, the `k3d` binary | `~/.dbcanvas/assets/<version>`, `~/.dbcanvas/dbcanvas.log`, `~/.dbcanvas/bin/k3d` |

Everything a stack needs is derived from the design plus the settings and node passwords at
deploy time, so a redeploy re-reads them. Passwords are not stored per-node on the canvas.

## Security model

- **One owner.** There are no accounts, roles or sign-ups: every stack belongs to the owner.
  A request is the owner's when it arrives on the control socket (the data folder is
  owner-only), carries the browser session a start link created, or carries an API token
  (`dbcanvas token create`) as a bearer token. A shared session's guests come in with a join
  link and a guest cookie, and reach only that session.
- **Encryption at rest.** The columns that hold a credential are sealed with AES-256-GCM:
  every stack's design (it carries the passwords typed into the canvas), the node account
  passwords, the secrets each deployed node was given, and what shared-session guests typed —
  their name, email and the chat. Each value is bound to the row it belongs to, so it cannot be
  copied into another. Browser sessions, API tokens, share links and guest cookies are stored
  only as SHA-256 hashes. The key is 32 random bytes, generated on first start into its own
  file; the server refuses to start with a key that is not the one the database was sealed
  with. `dbcanvas rotate-key` replaces it.
- **Node credentials** are the same for every node of a kind. A value under `env:` in
  config.yaml takes precedence over the generated one.
- **Exposure.** The browser port listens on `127.0.0.1` only. The ports deployed nodes publish
  bind to `CONTAINER_BIND_IP`, default `127.0.0.1`. Every request on the browser port must be
  addressed to its own name (or `publicUrl`'s) and carry one of the credentials above.
- **The Docker socket** is the real trust boundary — see above.

## The frontend

React + Vite, built to static files that the Go binary embeds with `//go:embed`. There is no
separate web server and no CDN: one binary serves the SPA and the API on one port.

Icons are hand-written inline SVG rather than an icon package, so they inherit `currentColor`
and stay legible at the sizes the sidebar actually uses.

## Testing

- `go test ./...` — unit tests beside the code. Many encode a fact learned from a live system
  (a version ceiling, a label an operator selects by, a repository spelling) so the reason
  survives with the assertion.
- `make smoke` — renders every React page off-browser and fails on a render error, plus a set
  of checks over the designer's own model.
- Every embedded shell script (a `…Script` constant) goes through `shellcheck` at warning
  severity in `go test` (`scriptlint_test.go`; skipped where shellcheck is not installed, never
  in CI). A deliberate exception is marked on its line: `# shellcheck disable=SCxxxx # why`.
- `make test-container` — tests that need Docker (build tag `containertest`): the keyring
  scripts run against real Percona Server binaries in throwaway containers. CI runs them in a
  job of their own.
- `make e2e MATRIX=keyring` — builds `dist/dbcanvas` and deploys the stacks a matrix in
  `scripts/e2e/` describes, runs its checks (SQL on the nodes, API calls) and removes them. It
  runs in an instance of its own (a throwaway `HOME`, `DBCANVAS_INSTANCE=dbcanvas-e2e`) on the
  Docker your shell uses, so your own DBCanvas is not touched; `E2E_KEEP=1` leaves the stacks up.
  A matrix is a bash file calling the helpers `scripts/e2e.sh` describes.
- Version-dependent server features live in one table, `app/mysqlfeatures.go`; validation, the
  deploy and the designer (through a generated `web/src/lib/mysqlFeatures.json`, kept in step
  by `TestMySQLFeaturesJSON`) all read it.
- None of these replaces deploying the thing: anything that talks to a real database, operator
  or package manager is verified on a live stack before it is called done.

## Local development

Docker is required for provisioning stacks. `make dbcanvas` builds the binary for this machine
into `dist/dbcanvas`: the web app (`npm run build`), the relay forwarders, the catalogue and the
bundle embedded, the version stamped from `VERSION`.

Run it against its own data folder and instance name, so it does not share state or container
names with an installed `dbcanvas`:

```sh
DBCANVAS_DATA=$HOME/.dbcanvas-dev DBCANVAS_INSTANCE=dev ./dist/dbcanvas
```

A running service restarts as the new binary when `dist/dbcanvas` is rebuilt, once no deploy is
running and nothing is connected (close the browser tab, or `dbcanvas stop`).

## Adding an endpoint

Routes live in one table, `app/api_routes.go`. A row with its group and a one-sentence
summary is registered, catalogued, published in the OpenAPI document and scope-checked.
`TestEveryRouteIsDocumented` fails if the summary is missing.

## Versioning

The `VERSION` file is stamped into the binary at build time (`main.appVersion`).
`/api/setup/status` and `dbcanvas version` report it, and the **What's new** dialog compares it
with the owner's read state. A plain
`go build` reports `dev`, which sorts newer than every release.

To release: edit `VERSION`, add an entry to `whatsNewNotes` in `app/whatsnew.go` and the
matching prose to `CHANGELOG.md` (`TestWhatsNewMatchesChangelog` checks
they agree), rebuild, and run `make adopt-images`.

## Tech stack

Go (standard library HTTP, `modernc.org/sqlite`, no ORM) · React + Vite + Tailwind ·
Docker Engine API over the unix socket, no SDK · k3d/k3s for Kubernetes frames.
