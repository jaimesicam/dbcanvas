# IMPLEMENTATION.md — Post-Scaffold Build Log

This file records **every change made after the initial build from
`SCAFFOLD.md`**. Together they reproduce the project end to end:

1. Build from `SCAFFOLD.md` (§0 naming substitution, §0.5 versioning policy, etc.).
2. Then apply each numbered feature below **in order**.

Naming derived in the scaffold (carry these everywhere): `APP_SLUG=dbcanvas`,
`APP_NAME=DBCanvas`, `APP_GLYPH=D`.

> Same spirit as the scaffold: prefer the simplest implementation that satisfies
> the described behavior, and resolve dependencies/base images to the newest
> stable at generation time unless a step says otherwise.

---

## 1. `make images` — selectable systemd base images

**Goal.** Build a matrix of **systemd-enabled** base images (full-OS containers
running systemd as PID 1) that will later back an "OS + version" picker for
creating container instances. Record every **successful** build in
`versions.yaml` at the repo root; that file is the source of truth for the
picker (combo box) implemented in a later entry.

**Matrix.** Five base images × two Docker platforms:

| OS family | Base images | Platforms |
| --- | --- | --- |
| RHEL (Oracle Linux) | `oraclelinux:8`, `oraclelinux:9`, `oraclelinux:10` | `linux/amd64`, `linux/arm64` |
| Debian (Ubuntu) | `ubuntu:22.04`, `ubuntu:24.04` | `linux/amd64`, `linux/arm64` |

**Failure is tolerated by design.** A build may fail — e.g. the local Docker
cannot emulate a non-native platform, or a package is not yet published for a
given OS. Such builds are **logged and skipped, never recorded**. The whole
matrix always completes and `versions.yaml` is always (re)written with whatever
succeeded.

**Required packages in every image** (install names differ per family):

| Purpose | RHEL/Oracle Linux | Ubuntu |
| --- | --- | --- |
| net-tools (ifconfig/netstat) | `net-tools` | `net-tools` |
| OpenLDAP client (ldapsearch) | `openldap-clients` | `ldap-utils` |
| sysstat (sar/iostat) | `sysstat` | `sysstat` |
| Percona repo manager | `percona-release` | `percona-release` |
| Percona Toolkit (pt-*) | `percona-toolkit` | `percona-toolkit` |

Percona is installed per the official docs
(<https://docs.percona.com/percona-software-repositories/installing.html>):
install the `percona-release` package, run **`percona-release setup pt`** (the
dedicated Percona Toolkit repository), then install `percona-toolkit`.

- RHEL: `yum install -y https://repo.percona.com/yum/percona-release-latest.noarch.rpm`
- Ubuntu: install `percona-release_latest.$(lsb_release -sc)_all.deb` from
  `https://repo.percona.com/apt/`.

> **Why `setup pt`, not `enable tools`:** the generic "tools" repo does **not**
> carry `percona-toolkit` on EL10, so `percona-release enable tools` fails there
> with `Unable to find a match: percona-toolkit`. The toolkit-specific repo
> (`percona-release setup pt`) carries it on every target — EL8/9/10 and Ubuntu —
> so it is used uniformly. (`setup pt` is non-interactive; it disables other
> Percona repos and enables only the Toolkit repo, which is all we need here.)

### Files added

```
images/
├── rhel.Dockerfile     # ARG BASE_IMAGE; systemd + tools for Oracle Linux
├── debian.Dockerfile   # ARG BASE_IMAGE; systemd + tools for Ubuntu
└── build.sh            # matrix driver → writes versions.yaml
versions.yaml           # generated output (see schema below)
```

Both Dockerfiles take `ARG BASE_IMAGE`, set `ENV container=docker`, install the
packages above, trim container-hostile systemd units (without `set -e`, so a
missing unit never fails the build), declare `STOPSIGNAL SIGRTMIN+3`,
`VOLUME ["/sys/fs/cgroup"]`, and set `CMD` to systemd init
(`/usr/sbin/init` on RHEL, `/sbin/init` on Ubuntu).

`images/build.sh` iterates the matrix, running for each (base, platform):

```sh
docker build --platform <platform> --build-arg BASE_IMAGE=<base> \
  -f images/<family>.Dockerfile -t dbcanvas-systemd:<os>-<version>-<arch> images/
```

On success it appends a record; at the end it writes `versions.yaml`. (`docker
build --platform` is used rather than `docker buildx`, since the latter is not
guaranteed present; BuildKit is the default builder in modern Docker.)

Image tag convention: **`dbcanvas-systemd:<os>-<version>-<arch>`**
(e.g. `dbcanvas-systemd:ubuntu-24.04-amd64`).

### Makefile

Added `images` to `.PHONY` and the target:

```make
## images: build systemd base images (OS × platform matrix) → versions.yaml
images:
	bash images/build.sh
```

### `versions.yaml` schema (generated — do not hand-edit)

Top-level keys `generated_at`, `image_prefix`, and `images` (a list; `images: []`
when nothing built). Each list item:

```yaml
- os: ubuntu            # picker group
  version: "24.04"      # quoted (string)
  platform: linux/amd64
  arch: amd64
  tag: dbcanvas-systemd:ubuntu-24.04-amd64
  base: ubuntu:24.04
  built_at: 2026-06-22T00:15:40Z
```

Regenerate any time with `make images`. The file is environment-specific (it
reflects which platforms the local Docker could build), so it is expected to
differ per machine.

### Running the instances (operator note)

These are systemd/PID-1 images; they need cgroup access at run time, e.g.:

```sh
docker run -d --name inst --privileged \
  -v /sys/fs/cgroup:/sys/fs/cgroup:ro \
  dbcanvas-systemd:ubuntu-24.04-amd64
```

(Runtime orchestration/launch from the app is a later IMPLEMENTATION entry.)

### Verification performed

- `make images` completed the full 10-cell matrix with **all 10 recorded**:
  Oracle Linux 8, 9 & 10 and Ubuntu 22.04 & 24.04, each on amd64 + arm64.
  (arm64 builds succeed via the host's binfmt emulation; on a host without it
  those cells would simply be skipped.)
- Spot-checked a built image per family **including OL10**: `netstat`,
  `ldapsearch`, `sar`, `pt-query-digest`, `percona-release` all resolve, and
  `init` → systemd.

> Note: an earlier draft used `percona-release enable tools` and reported OL10 as
> a tolerated failure. That was wrong — `percona-toolkit` **is** published for
> EL10; the fix was switching to `percona-release setup pt` (above).

---

## 2. Database Stack Designer

A node-graph workspace (modeled on the Node Editor) to design, validate, deploy,
and manage stacks of real Docker containers. Nav link **"Database Stacks"** sits
between Dashboard and Interactions. First node type: **Intranet** (per-stack
singleton on OEL9: Squid, DNS, SMTP, IMAP, RoundCube webmail, OpenLDAP, self-
signing CA). Delivered in four phases — **all complete**.

### Architecture decisions
- The Go backend drives Docker via the **Engine API over the mounted unix socket
  using only the stdlib** (`app/docker.go`) — no SDK, no docker CLI, so the app
  stays a static distroless binary. `docker-compose.yml` mounts
  `/var/run/docker.sock` and passes a new `DOMAIN` env (default `example.net`).
- The Intranet is **provisioned at deploy time**: start the OL9 systemd base image
  (`dbcanvas-systemd:oraclelinux-9-<arch>` from `make images` — a hard
  prerequisite), wait for systemd, then run an embedded script via `docker exec`.
- Browser terminals (Phase 4) will use xterm.js over a WebSocket.

### Infrastructure
- `.env.example` / `.env`: add `DOMAIN=example.net`.
- `docker-compose.yml`: add `DOMAIN` env + bind-mount `/var/run/docker.sock`.
- `app/Dockerfile`: `COPY provision ./provision` (the embedded provisioning
  script must live under `app/` to be reachable by `//go:embed`).

### Phase 1 — designer + stack CRUD + TTL
- **`app/store.go`**: tables `stacks(id,name,owner_id,ttl,status,created_at,
  expires_at,design_json)` and `deployments(stack_id,node_id,container_id,state,
  config_json,secrets_json)` + CRUD/list/expired-scan/deployment methods.
- **`app/stacks.go`**: owner-scoped routes `GET/POST /api/stacks`,
  `GET/PUT/DELETE /api/stacks/{id}` (admins see all); TTL `2h|4h|8h|24h|2w|infinity`
  → `expires_at` (NULL for infinity); a **reaper goroutine** (startup + every 60s)
  marks expired stacks and tears down their containers.
- **Frontend**: `src/lib/canvas.js` (shared geometry extracted from
  `NodeEditorFrames.jsx`, which now imports it), `src/lib/stackApi.js`,
  `src/pages/StackDesigner.jsx` (stack list + create modal + design canvas with
  pan/zoom/connect/properties, Intranet singleton, debounced autosave). Nav entry
  added in `src/App.jsx`.

### Phase 2 — Docker client + validate + deploy + lifecycle
- **`app/docker.go`** (stdlib Engine API over the socket): `Ping`, `ImageExists`,
  `NetworkEnsure`/`NetworkRemove`, `ContainerByName`, `ContainerCreate`/`Start`/
  `Stop`/`Restart`/`Remove`, `ContainerState`, `Exec` (multiplexed stdout/stderr
  demux + exit code), `CopyFile` (tar→`PUT /archive`), `WaitSystemd`.
  - **systemd-in-container** requires (verified on a cgroup-v2 host):
    `Privileged=true`, **`CgroupnsMode=host`**, bind `/sys/fs/cgroup:rw`, tmpfs
    `/run` + `/run/lock`. Without the cgroupns/host cgroup mount, `/usr/sbin/init`
    crash-loops with exit 255. Use unversioned API paths; do **not** URL-escape the
    image `repo:tag` (the `:` must stay literal).
- **`app/intranet.go`**: validate (Docker reachable, OL9 image present, Intranet
  singleton, unique labels), deploy (per-stack network `dbcanvas-stack-<id>`,
  container `dbcanvas-<id>-<nodeId>` with alias **`intranet`**, async provisioning
  goroutine, generated creds), redeploy diff (keep `running` nodes, remove nodes
  deleted from canvas), lifecycle start/stop/restart, node-profile GET,
  **destroy** (`POST /api/stacks/{id}/destroy` → `handleDestroyStack`), and
  `teardownStack`. Generated secrets: LDAP admin pw = `LdapAdm!`+8 hex upper (e.g.
  `LdapAdm!AAD1CBFC`); base DN derived from `DOMAIN` (`example.net`→`dc=example,dc=net`).
- **`app/provision/intranet.sh`** (embedded via `//go:embed`, run in-container):
  enables Oracle EPEL + CodeReady, installs squid/bind/postfix/dovecot/
  openldap-servers+clients/httpd/php/roundcubemail/mod_ssl/openssl; creates the CA
  at `/etc/pki/dbcanvas/ca.crt`; initializes slapd (suffix/rootDN/rootPW via
  `cn=config`, loads cosine/inetorgperson/nis schemas, creates the base entry +
  `ou=People`/`ou=Groups`); configures postfix/dovecot virtual mailboxes with an
  admin mailbox; enables+starts all services. Idempotent via a marker file.
  - **OL9 note:** unlike stock RHEL 8/9, Oracle Linux 9 **does** ship
    `openldap-servers` (2.6.x) and `roundcubemail` via Oracle EPEL + CodeReady.
- **Routes** (`app/main.go`): `POST /api/stacks/{id}/validate`, `.../deploy`,
  `.../destroy`, `GET /api/stacks/{id}/nodes/{nid}`,
  `POST .../nodes/{nid}/{start|stop|restart}`.
  `App` gains a `docker` field (`NewDocker(DOCKER_SOCK | /var/run/docker.sock)`).
- **Network isolation + destroy/reset:** each stack's containers join **only**
  their `dbcanvas-stack-<id>` network (stacks can't interfere; aliases like
  `intranet` don't collide across stacks). **Destroy** removes all of a stack's
  containers **and** its network and deletes the `deployments` rows, returning the
  stack to `draft`; this **resets every post-deployment-only property** (generated
  credentials, LDAP/email users, certificates) so a redeploy re-provisions fresh.
- **Frontend** (`StackDesigner.jsx`): Validate/Deploy toolbar buttons + issues
  panel, 3s deployment-state polling (design stays local — poll only refreshes
  deployment state/status), per-node state badges, right-click lifecycle
  (view config / start / stop / restart / delete), a node-profile modal, and the
  **OS field locked once the node is deployed**.

### Phase 3 — Intranet node management
All actions run via `docker exec` into the running container (no LDAP/SMTP client
libraries). Inputs that reach shell scripts are passed via the exec **environment**
(never interpolated) and validated (`^[a-zA-Z0-9._-]+$` for names/uids; passwords
reject `:` and newlines). New backend file **`app/intranet_mgmt.go`** + routes:
- **Email users** (`/email/users` GET/POST, `.../password`, `.../delete`): manages
  Dovecot `passwd-file` (`/etc/dovecot/users`) + Postfix `vmailbox`; usernames are
  normalized to the node's domain.
- **LDAP users** (`/ldap/users` …): create (`ldapadd` inetOrgPerson + `ldappasswd`),
  list (`ldapsearch -LLL`, parsed by a small LDIF parser), update
  `givenName/sn/cn/mail` (`ldapmodify`), set password, delete.
- **LDAP groups** (`/ldap/groups` …): create `posixGroup` (auto next `gidNumber`),
  set members from a comma-separated uid list (`replace: memberUid`), delete.
- **Certificate** (`/cert` GET/POST): generates `/etc/pki/dbcanvas/intranet.crt`
  signed by the node CA with **serverAuth + clientAuth** EKU; validity in
  minutes/hours/days (default **365 days**) via openssl `-not_after` (the underscore
  flag; it overrides `-days` and gives sub-day granularity on OL9's OpenSSL 3.5);
  **archives** any existing cert+key under `/etc/pki/dbcanvas/archive/` first.
- **Credentials**: served from the existing `deployments.secrets_json` (owner-only),
  no extra endpoint.
- **Frontend** `src/pages/IntranetManager.jsx` (rendered in the properties panel
  when a **running** Intranet node is selected; panel widens to `420px`): tabbed
  Overview / Email / LDAP / Certificate / Credentials. Each LDAP user and group row
  has a **copy button** emitting the exact `ldapsearch` command templated with the
  admin DN/password + base DN (`ldap://intranet:389`, `uid=…,ou=People,…` /
  `cn=…,ou=Groups,…`). `intranetApi(id,nid)` added to `lib/stackApi.js`.

### Phase 4 — terminals + dock/detach
- **`app/docker.go`**: `HijackExec` opens an interactive (TTY) exec by dialing the
  socket raw and writing a `POST /exec/{id}/start` with `Connection: Upgrade` /
  `Upgrade: tcp`, then returns the raw bidirectional stream (`ExecConn`; with
  `Tty:true` the stream is **not** multiplexed). `ResizeExec` posts the TTY size.
- **`app/terminal.go`** (`GET /api/stacks/{id}/nodes/{nid}/term`, WebSocket via
  `github.com/coder/websocket` — pure Go, keeps the static binary): authenticates
  + resolves a running node, bridges browser↔`/bin/bash` (`TERM=xterm-256color`).
  Browser→container binary frames = keystrokes; text frames = `{"type":"resize"}`;
  container→browser = raw pty output as binary frames. `InsecureSkipVerify` keeps
  the Vite dev proxy working (same-origin in production).
- **Frontend**: `@xterm/xterm` + `@xterm/addon-fit`. A top-level
  `src/terminal/TerminalProvider.jsx` (mounted in `App.jsx` **above** the page
  switch) holds xterm instances + WebSockets in a ref map and renders a persistent
  **TerminalDock** — so sessions **survive navigation** (leaving/returning to the
  Stacks page doesn't reset them). The dock is multi-tab (one per container),
  minimisable, and **dock/detach**-able: docked = bottom bar with a drag-to-resize
  height handle; detached = floating window (drag header to move, corner handle to
  free-resize). Layout persists in `localStorage`. "Enter root console" is offered
  from the node right-click menu and the Intranet Overview tab.
- **Properties panel** (`StackDesigner.jsx`): now **horizontally resizable when
  docked** (left-edge drag) and **detachable** into a floating, freely-resizable
  window (move + corner handle); layout persists in `localStorage`.
- Bundle note: xterm pushes the JS bundle to ~640 kB (gzip ~175 kB); acceptable
  here (no code-splitting requirement) — Vite prints a size warning only.

### Refinements (post-Phase-4)
- **Stepwise provisioning + retry + progress.** Provisioning was reworked from one
  embedded script into an **ordered list of idempotent steps** (`intranetSteps()`
  in `intranet.go`) run via `bash -c`; each step is **retried up to 10×**. Live
  progress (`percent`, `phase`, rolling `log`, completion `message`) is stored in a
  new **`deployments.progress_json`** column (`store.go` migration + `Progress`
  field + `SetDeploymentProgress`) and surfaced in the API. Each node provisions in
  its **own goroutine**, so one node failing never blocks the others.
- **Webmail port + link.** The Intranet container now **publishes httpd:80 to an
  auto-assigned (guaranteed-unused) host port** (`ContainerSpec.PublishPort` +
  `ContainerPort` in `docker.go`); the port is stored in the node config. The
  webmail step writes a working RoundCube config (sqlite db, `des_key`, IMAP/SMTP
  localhost), initialises the sqlite schema via PHP, relaxes the httpd access rule,
  and starts `php-fpm`. The Email tab shows an **"Open RoundCube webmail"** link to
  `http://<host>:<port>/roundcubemail/`.
  - **Mail auth fix:** stock Dovecot on OL9 ships `ssl=required`,
    `disable_plaintext_auth=yes`, an empty `mail_location`, and a **system-user**
    passdb — so the virtual users in `/etc/dovecot/users` were never consulted and
    RoundCube login failed. The mail step now writes `99-dbcanvas.conf` adding a
    `passwd-file` passdb + static `vmail` userdb, `mail_location =
    maildir:/var/mail/vhosts/%d/%n`, and plaintext IMAP (`ssl=no`,
    `disable_plaintext_auth=no`) so localhost IMAP login works.
  - **Mail send fix:** the EL package ships **RoundCube 1.5**, where SMTP uses
    `smtp_server`/`smtp_port` (not the 1.6 `smtp_host`) — so `smtp_host='localhost:25'`
    was ignored and RoundCube dialled the default port **587** (refused; Postfix
    listens on 25). The config now sets `smtp_server=localhost`, `smtp_port=25`, and
    empty `smtp_user`/`smtp_pass` (no-auth send, permitted from localhost via Postfix
    `mynetworks`), keeping `smtp_host` for 1.6 forward-compat.
- **Deployment console** (`DeploymentConsole` in `StackDesigner.jsx`): a dockable
  (bottom, drag-resize height) / **detachable + free-resize** floating panel that
  auto-opens while a deploy runs, showing per-node **progress bars**, phase, and a
  log tail, plus a completion banner — **"Deployment complete"** or **"completed
  with errors — N of M failed"**. Layout persists in `localStorage`. It can be
  **minimized** to a restore pill (like the terminal dock; minimizing is respected
  by the auto-open) and has **no close button** (it auto-opens on deploy and
  unmounts when you leave the stack),
  and it is rendered through a **`createPortal` to `document.body`** — otherwise the
  page's `.animate-fade-in` wrapper (a lingering `transform`) makes `position:
  fixed` resolve against that div and get clipped by `main`'s `overflow`. The
  detached properties panel, the **right-click context menu**, and the
  profile/new-stack **modals** are portaled for the same reason (otherwise the menu
  appears offset from the cursor and the modals are mis-centered).
- **No JavaScript dialogs.** All `confirm()`/`prompt()` were removed: a reusable
  **`ConfirmButton`** (two-click arm, in `components/ui.jsx`) replaces confirms
  (delete stack, destroy, delete email/LDAP user/group); password changes use
  **inline input editors** (email + LDAP user).
- **Multiple terminals per node.** `openTerminal` now mints a fresh session id per
  call (`stackId:nodeId#n`), so a node can have several concurrent terminal tabs.
- **Autosave loop fixed.** The 3s status poll replaced the `stack` object, which was
  in the autosave effect deps → it saved every tick ("Saving…"↔"Saved" loop). The
  effect now depends only on the design and writes only when it differs from a
  `lastSaved` snapshot.
- **"copy" → icon.** The LDAP/credentials copy controls use an `Icon.Copy` glyph.
- **Node card redesign + architecture.** Nodes are larger (212×104), drop the
  colored top bar, show the **full** (wrapping) service description, use a
  **server** glyph (`Icon.Server`), and display **OS version + architecture**
  (e.g. "Oracle Linux 9 · amd64"). Architecture is now a real node field
  (`arch`, default `amd64`) selectable in the properties panel (amd64/arm64),
  **locked once deployed**, and used for image selection
  (`dbcanvas-systemd:oraclelinux-9-<arch>`) + validation; the backend
  `designNode.Arch`/`nodeConfig.Arch` carry it. While a node provisions, a small
  **progress ring** (upper-right) replaces the old bottom progress bar.

### Verification performed
- `go build`/`vet`/`test` pass; `stacks_test.go` covers the reaper + TTL gate.
- End-to-end via the host binary: create→design→validate→**deploy**; Intranet
  provisioned to `running` in ~55s; inside the container `slapd/squid/named/
  postfix/dovecot/httpd` all `active`; `ldapsearch` with the generated admin
  password returns the base + `ou=People`/`ou=Groups`; the CA exists. Lifecycle
  stop→`exited`, start→`up`; `DELETE` stack removes container **and** network.
- Destroy/reset: deploy→`running` (pw `LdapAdm!069AE512`), **destroy** removed the
  container and the `dbcanvas-stack-1` network and set status `draft`; **redeploy**
  re-provisioned fresh with a **new** password (`LdapAdm!F293EA93`) — confirming the
  post-deployment reset.
- Production path: `docker compose build` succeeds with the embedded script; the
  **containerized distroless app** validates a stack successfully — confirming it
  reaches Docker via the mounted socket (Docker-out-of-Docker).
- Phase 3 management (against a live deployment): created LDAP users (full +
  minimal attrs), listed, **updated** `givenName`/`mail` (confirmed by a direct
  in-container `ldapsearch`), changed a password (confirmed via `ldapwhoami`),
  created a `posixGroup` and assigned members from `"dd, ada"`; added/listed/
  password-changed/deleted email mailboxes (confirmed in `/etc/dovecot/users`);
  generated a **90-minute** cert (notAfter exactly +90m, EKU = serverAuth +
  clientAuth) and a default-365-day renewal that **archived** the prior cert+key.
- Phase 4 terminal: a standalone WebSocket client (cookie-authenticated) connected
  to `/term`, sent a resize + a command over the PTY bridge, and received the
  echoed marker + command output — confirming the hijacked `docker exec` TTY
  round-trips. `go build`/`vet`/`test` and `docker compose build` (with the new
  `coder/websocket` dep) all pass.
- Refinements (live): a deploy progressed through stepwise phases
  (`Creating container → Enable repositories → Install packages → … → Running`,
  3→10→21→…→100%); the deployment payload carried `progress.{percent,phase,log}` +
  `config.webmailPort`; the webmail host port was auto-assigned (unused) and
  `GET /roundcubemail/` returned **HTTP 200** ("DBCanvas Webmail"). `go build`/
  `vet`/`test` and `docker compose build` pass; frontend builds.
- Mail auth: after the Dovecot fix, `doveadm auth login admin@<domain>` and a raw
  IMAP `LOGIN` on :143 both **succeed** (LIST returns INBOX), and a mailbox added
  via the API authenticates too — confirming RoundCube login works (it proxies to
  the same IMAP).
- Mail send: a scripted RoundCube login + compose + **send** succeeded (no error
  banner); Postfix logged `status=sent (delivered to maildir)` and the message
  landed in the admin virtual mailbox.

> Operational note: each stack creates its own user-defined bridge network. On a
> host with many networks this can exhaust Docker's default address pool
> (`all predefined address pools have been fully subnetted`); `docker network
> prune` frees space. Pre-existing similarly-named projects/containers are left
> untouched.

---

## 3. `make versions` — installable version catalog (Percona Server + PMM3)

**Goal.** Enrich `versions.yaml` (produced by `make images`, §1) with the
**installable software versions** each artifact offers, so later UI pickers can
offer real choices:

- For every **built systemd image** (per OS × platform): the **Percona Server**
  releases installable on it, grouped by major series (`"8.0"`, `"8.4"`).
- A trailing **`pmm`** section: the **PMM3** (`percona/pmm-server`) image
  versions selectable for a PMM node (§4).

`make versions` **reads and rewrites** `versions.yaml` in place — it preserves the
image records from `make images` and adds/refreshes the version data. It is the
single source of truth the app reads at runtime.

### Files added

```
images/versions.sh         # probes images + the PMM registry → rewrites versions.yaml
```

### Makefile

Added `versions` to `.PHONY` and the target:

```make
## versions: probe built images for installable Percona Server versions → versions.yaml
versions:
	bash images/versions.sh
```

### Percona Server discovery (per image)

For each image entry parsed out of `versions.yaml`, `versions.sh` spins up a
throwaway container (`docker run --rm <tag> bash -lc <probe>`) and uses the
`percona-release` manager already baked into the image (§1) to enumerate the
`percona-server-server` package versions:

- **RHEL family (Oracle Linux):** `percona-release setup ps80` then
  `dnf -q search percona-server-server --showduplicates`; repeat with
  `percona-release setup ps84lts` for the 8.4 LTS series.
- **Debian family (Ubuntu):** same products, queried with
  `apt-cache madison percona-server-server` after `apt-get update`.

The output is filtered to the exact `percona-server-server` binary package
(dropping `-debuginfo`/source rows), the upstream version string is normalised
(e.g. `8.0.46-37.1.el9.x86_64` → `8.0.46-37.1`; Debian `…-1.noble` → `…-1`),
deduplicated and `sort -V`-ordered, and split into the `8.0` / `8.4` series by a
`^8\.0\.` / `^8\.4\.` match (robust even if both repos end up enabled).

- **EL8 gotcha — the distro `mysql` module masks the package.** On Oracle Linux 8
  the default `mysql` dnf **module** hides Percona's `percona-server-server`
  (search returns only `-debuginfo`, `repoquery` is empty). The probe runs
  `dnf -y module disable mysql` first — a harmless no-op on EL9/EL10, which have
  no such module — after which all ~33 EL8 8.0 builds enumerate. Without it EL8
  reports **zero** versions.
- Each image is recorded with whatever it has; a series with no packages is
  written as an empty list (e.g. EL10 carries only a couple of 8.0 builds).
- **arm64 caveat:** on a host without binfmt the `…-arm64` tags are actually
  amd64 builds, so they enumerate the amd64 repo. The version *strings* are
  arch-independent, so the recorded data is still correct; on a host with real
  emulation each arch is probed natively.

### PMM3 discovery (from the registry)

PMM3 ships as a Docker image, not an OS package, so its installable minor
versions come from the **registry**, not a container. `versions.sh` queries the
Docker Hub tags API for `percona/pmm-server` (paginated; no JSON parser — tag
names and the `next` page URL are grepped out), keeps the full three-part
`3.x.y` releases (`sort -V`), and writes the `pmm` section. `default_tag` is the
rolling `"3"` tag (latest 3.x) used when no specific minor is selected; `latest`
is the newest discovered `3.x.y`.

### `versions.yaml` schema additions (generated — do not hand-edit)

Per-image entries gain a `percona_server` map; a new top-level `pmm` mapping is
appended. A `versions_generated_at` timestamp is added alongside `generated_at`.

```yaml
images:
  - os: oraclelinux
    version: "9"
    # …existing make-images fields (platform, arch, tag, base, built_at)…
    percona_server:
      "8.0":
        - 8.0.30-22.1
        - 8.0.46-37.1
      "8.4":
        - 8.4.0-1.1
        - 8.4.8-8.1
pmm:
  repository: percona/pmm-server
  default_tag: "3"          # rolling latest-3.x; used when no minor is picked
  latest: "3.8.1"
  versions:                 # selectable PMM3 minor versions
    - "3.0.0"
    - "3.8.1"
```

Regenerate any time with `make versions`. Like `versions.yaml` generally, the
contents are environment-/time-specific (registry state, which images built).

### Verification performed

- `make versions` probed all 10 images + discovered **13 PMM3 versions**
  (`3.0.0`…`3.8.1`, latest `3.8.1`); output parses as valid YAML.
- Per-image Percona Server counts as expected: OL8 33×8.0 + 8×8.4 (after the
  `mysql`-module fix; **0** before it), OL9 16+8, OL10 2+3, Ubuntu 22.04 16+5,
  Ubuntu 24.04 9+5.

---

## 4. PMM3 node (Percona Monitoring & Management)

A second Stack Designer node type: a **PMM3 server** (`percona/pmm-server` —
Grafana, VictoriaMetrics, ClickHouse, PostgreSQL, QAN and an nginx TLS
front-end, all under supervisord). Unlike the Intranet node it is **not** built
by `make images`; the selected image is pulled at deploy. The node offers a
**minor-version picker** (from §3's catalog), a **user-set-or-generated admin
password**, and an optional **nginx certificate signed by the Intranet CA**.

### versions.yaml at runtime — mount + catalog

The app reads the §3 catalog at runtime (the build context is `./app`, so
`versions.yaml` is **not** embedded — it is mounted):

- **`docker-compose.yml`**: bind-mount `./versions.yaml:/etc/dbcanvas/versions.yaml:ro`
  and set `VERSIONS_FILE=/etc/dbcanvas/versions.yaml`. Re-run `make versions` on
  the host to refresh what the pickers offer (no rebuild needed; the app reads
  the file per request).
- **`app/versions.go`**: parses **only** the `pmm:` block by hand (the format is
  fixed and we emit it — no YAML dependency added). `versionsFilePath()` tries
  `VERSIONS_FILE`, then `/etc/dbcanvas/versions.yaml`, then `versions.yaml` /
  `../versions.yaml` for local `go run`. `loadPMMCatalog()` never errors — on any
  problem it returns a fallback (`percona/pmm-server`, tag `3`) so a PMM node can
  still deploy. `PMMCatalog.validPMMTag` accepts the default tag, `latest`, or a
  discovered version (guards the Docker pull against arbitrary tags).
- **Route** (`main.go`): `GET /api/catalog/pmm` (auth required) → the catalog
  `{repository, defaultTag, latest, versions[]}`.

### Node model

`designNode` (in `intranet.go`) gains PMM-only fields (ignored by other types),
carried in the saved design JSON: `version` (minor tag; `""` → catalog default),
`adminPassword` (`""` → auto-generated), `generateCert` (sign nginx certs from
the Intranet CA on deploy). Deploy dispatch (`handleDeployStack`) switches on
node type: `intranet` → `provisionIntranet`, `pmm` → `provisionPMM`.

### Provisioning — `app/pmm.go`

`provisionPMM(stack, node, doc)` records the deployment then runs an async,
stepwise goroutine (same progress/percent/log model as the Intranet, §2):

1. **Pull image** (`ImagePull`, new in `docker.go`) if not already present —
   `repo:tag` from the node version / catalog default.
2. **Create + start** the container publishing **two** ports, **8080** (HTTP) and
   **8443** (HTTPS), via `ContainerSpec.PublishPorts` (new). Network = the stack
   network, aliases `[<label>, "pmm"]`, hostname = the sanitised label.
3. **Wait for readiness** — poll `GET http://localhost:8080/v1/server/readyz`
   for `200` inside the container (`waitPMMReady`, up to 180s).
4. **Admin password** — `change-admin-password "$PW"` (PMM ships it at
   `/usr/local/sbin/`). The password is reused across redeploys, else the user's
   value, else `genSecret("PmmAdm!")`; the effective value is stored in the
   deployment **secrets** (`pmmSecrets`).
5. **Grafana SMTP** — rewrite the `[smtp]` section of `/etc/grafana/grafana.ini`
   to relay through the Intranet mail server (`host = intranet.<domain>:25`,
   `enabled = true`, `skip_verify = true`, `startTLS_policy = NoStartTLS`, …,
   matching the requested template), then `supervisorctl restart grafana`. Any
   pre-existing `[smtp]` block is stripped first (awk, up to the next section
   header) so it is never duplicated.
6. **Certificate** (when `generateCert`) — see below.

The published host ports, admin user, image, SMTP host and service list are
stored in the deployment **config** (`pmmConfig`).

- **For the SMTP `host` to resolve**, the Intranet container now also advertises
  the FQDN network alias `intranet.<domain>` (added to its `Aliases`), so peers
  on the stack network reach the mail server at `intranet.<domain>:25` (Docker's
  embedded DNS, no bind dependency).
- **Validation** (`validateStack`): a PMM `version` not in the catalog is a
  warning; `generateCert` **requires an Intranet node** in the stack (its CA) —
  an error otherwise. The PMM image is not required to pre-exist (it is pulled).

### Certificate from the Intranet CA → `/srv/nginx`

PMM's nginx serves `/srv/nginx/{certificate.crt,certificate.key,ca-certs.pem,
certificate.conf,dhparam.pem}`. `pmmGenerateCert(pmm, intranet, domain, alias,
ttlValue, ttlUnit)`:

1. Reads the Intranet CA cert+key (`/etc/pki/dbcanvas/{ca.crt,ca.key}`) out of the
   Intranet container (`readContainerFile` = `base64 -w0` over the exec channel,
   binary-safe).
2. Stages them into the PMM container's `/tmp` via `PutArchive` (new in
   `docker.go`: extract a tar into a dir).
3. Runs an in-container openssl script that **archives** the existing
   `/srv/nginx` cert set to `/srv/nginx/archive/<timestamp>/`, then writes a new
   key + CA-signed cert (SANs: `<alias>`, `<alias>.<domain>`, `pmm`, `localhost`,
   `127.0.0.1`; validity from the TTL via openssl `-not_after`), sets
   `ca-certs.pem` to the signing CA, regenerates `certificate.conf`, keeps the
   existing `dhparam.pem`, fixes ownership, and `supervisorctl restart nginx`.

At deploy time `provisionPMM` first waits for the Intranet CA to exist
(`waitIntranetCA`, since both nodes provision concurrently) and signs with a
365-day default. Post-deploy, the **certificate frame** re-issues on demand:

- **`app/pmm_mgmt.go`** + routes: `GET /api/stacks/{id}/nodes/{nid}/pmm/cert`
  (current cert subject/issuer/dates) and `POST …/pmm/cert` (`{value, unit}` →
  generate with that TTL). The handler finds the stack's running Intranet node
  for the CA (`intranetContainerFor`) and flips `config.generateCert` true.

### `docker.go` additions / fixes

- **`ImagePull`** (POST `/images/create`, drain the progress stream to block
  until present), **`PutArchive`** (PUT `/archive` with a tar), and
  **`ContainerSpec.PublishPorts []int`** (publish several auto-assigned host
  ports; the single `PublishPort` still works for the Intranet).
- **tmpfs scoped to privileged containers.** The systemd images need a tmpfs at
  `/run` + `/run/lock`; this was previously mounted for **every** container.
  PMM runs **unprivileged as UID 1000** and crash-loops (`mkdir /run/postgresql:
  Permission denied`) when `/run` is a root-owned tmpfs — so the tmpfs (and the
  cgroup bind / host cgroupns) are now applied **only when `Privileged`**.
- **`tarFiles` stamps an owner uid.** `PutArchive` extracts as root into PMM's
  **sticky** `/tmp`, but the in-container openssl runs as `pmm` (UID 1000) — so
  the staged CA files are written with `Uid: 1000` (mode `0600`), letting the
  unprivileged user both read the CA key and delete the files afterward.

### Lifecycle — published ports refreshed on start/restart

Containers are created with an **empty HostPort** binding, so Docker assigns a
**new** ephemeral host port every time the container **starts** — a stop/start or
restart therefore changes the published port and would leave the recorded access
links (PMM 8080/8443, Intranet webmail :80) pointing at the old port.
`handleNodeAction` now calls **`refreshPublishedPorts`** after a successful
`start`/`restart` (both node types): it re-inspects the container, reads the live
host ports, and rewrites the stored config so the links stay valid (the 3-s
deployment poll then re-renders them).

### Frontend

- **`StackDesigner.jsx`**: new `pmm` entry in `NODE_TYPES` (label **PMM3**, sub
  **"Percona Monitoring & Management"** — deliberately short so the node card
  doesn't overflow), a **PMM3** toolbar button (non-singleton), and node defaults
  (`version/adminPassword/generateCert`). `PMMOptions` (shown in the properties
  panel when an undeployed PMM node is selected): **version** select populated
  from `GET /api/catalog/pmm` (default option = `latest (<defaultTag>)`),
  **admin password** input (placeholder "auto-generate if empty"), and a
  **Generate nginx certificate from Intranet CA** checkbox; all lock once
  deployed. A running PMM node renders **`PMMManager`** (the properties panel
  widens, as for the Intranet).
- **`PMMManager.jsx`**: tabs **Overview** (image/version/alias/SMTP/cert mode +
  root console + delete), **Access** (HTTP/HTTPS URLs built from the published
  host ports, admin user + password with copy buttons, "Open PMM" link), and
  **Certificate** (current cert info + a generate frame with a TTL value/unit;
  notes that it archives existing `/srv/nginx` certs and needs the Intranet node).
- **`lib/stackApi.js`**: `stackApi.pmmCatalog()` and `pmmApi(id, nid)`
  (`certInfo`/`certGenerate`).

### Verification performed

- `make versions` wrote the `pmm` catalog (13 versions); `GET /api/catalog/pmm`
  returned it (default `3`, latest `3.8.1`).
- End-to-end (host binary, real Docker): deployed an **Intranet + PMM** stack
  (cert generation on). PMM reached `running`; node config exposed the published
  **8080/8443** host ports and the generated admin password
  (`PmmAdm!…`), which authenticated to Grafana (`/api/user` → 200). `grafana.ini`
  carried the exact `[smtp]` block (`host = intranet.example.net:25`, which
  resolved on the stack network). `/srv/nginx/certificate.crt` was issued by
  `DBCanvas CA` (subject `CN=pmm.example.net`) and served on **8443**;
  `ca-certs.pem` was the CA; the prior cert set was archived under
  `/srv/nginx/archive/<ts>/`; `/tmp` CA staging was cleaned up.
- Certificate **frame**: `POST …/pmm/cert` with a 2-hour TTL produced a new
  Intranet-signed cert (notAfter ≈ +2h) and a **second** archive directory.
- **Port refresh:** restart and **stop→start** each re-assigned the host ports
  (e.g. `32821/32822` → `32823/32824` → `32825/32826`); after each, the stored
  config matched `docker port` and the HTTPS link returned 302.
- **PMM `/run` fix:** before scoping the tmpfs, PMM crash-looped
  (`mkdir /run/postgresql: Permission denied`); after, it boots cleanly.
- `go build`/`vet`/`test`, `gofmt`, the web build, and `docker compose config`
  all pass.

---

## 5. Intranet DNS authority + unique hostnames + required-Intranet gating

A set of changes making the Intranet the stack's real DNS server, giving every
node a unique hostname/FQDN, and enforcing the Intranet as a prerequisite.

### Node card / description

- The Intranet node's description is shortened to **"Squid Proxy · DNS · Mail ·
  OpenLDAP · CA"**. The previous 7-segment string overflowed the fixed-height
  card and (with `justify-center`) clipped the colored top accent bar; the node
  card also gained `overflow-hidden` so no description can clip it again.

### Unique hostnames + FQDN (`dns.go`)

- **`stackHostnames(doc)`** assigns every node a stable, DNS-safe, **unique**
  hostname. The Intranet (singleton) is always `intranet`. Other nodes use their
  sanitized label (`hostLabel`: lowercased, `[a-z0-9-]`); when two share a label
  (e.g. two PMMs both "pmm"), each gets a stable suffix from a short FNV hash of
  its node id (`pmm-c170`, `pmm-c629`) — so a single instance stays clean and
  duplicates keep their names across redeploys regardless of canvas order.
- This one hostname is used consistently for the container **hostname**, the
  **network alias**, the **DNS record**, and the displayed **FQDN**
  (`<hostname>.<domain>`, `$DOMAIN` from `.env`). `pmmConfig`/`nodeConfig` carry
  `Hostname` + `FQDN`; the PMM **Overview**, Intranet **Overview**, and the
  node-profile modal display the FQDN.

### Intranet as authoritative DNS (`dns.go`, `bind`)

The Intranet runs `bind`/`named` as the **authoritative server for `$DOMAIN`**
with both a forward zone and a reverse (PTR) zone, plus forwarding for everything
else.

- **`docker.go`** additions: `ContainerIP(network)`, `NetworkSubnet(name)`, and
  `ContainerSpec.{DNS, DNSSearch, IPv4Address}` (→ `HostConfig.Dns/DnsSearch` and
  endpoint `IPAMConfig.IPv4Address`). `Exec` was refactored to `ExecAs(user,…)`
  so root-owned files (e.g. `/etc/resolv.conf`) can be edited inside images that
  run unprivileged.
- **Stable resolver IP:** the Intranet is pinned to a **static address** (host
  `.2` of the stack subnet, `staticIntranetIP`), so it stays a reliable resolver
  across restarts.
- **Ordering:** every non-Intranet node **blocks until the Intranet is fully
  up and running** before it starts its own container — it depends on the
  Intranet's DNS / SMTP / LDAP / CA. `waitIntranet` polls the Intranet deployment
  and only returns its container id + IP once it reaches `running` (failing fast
  if the Intranet errors). The node's image is still pulled beforehand, so the
  slow pull overlaps the Intranet build; only the container start is gated.
- **`reconcileStackDNS(stackID)`** rebuilds the zones from the stack's current
  deployments: it writes `named.conf` (listening on `127.0.0.1` + the Intranet's
  own IP, **never** Docker's `127.0.0.11`, which it forwards external queries to),
  a forward zone (`A` for every node incl. `intranet`), and a reverse zone (PTR;
  the `in-addr.arpa` zone name + owner are derived from the network subnet by
  `reverseZoneInfo`, rounded to /8·/16·/24), then reloads named (`rndc reconfig &&
  rndc reload`, restart fallback). It is a full idempotent rebuild, called after
  each node provisions, after start/restart (IPs change), and after a node is
  removed (so stale records drop).
- **Nodes use the Intranet as resolver.** Non-Intranet containers are created
  with `Dns=[intranetIP]`; additionally their `/etc/resolv.conf` is rewritten
  (as root) to the Intranet as **sole** nameserver (`pointResolverAtIntranet`),
  because Docker's embedded resolver answers reverse PTR for in-network IPs itself
  and won't forward it. Docker regenerates resolv.conf on each start, so
  `restoreNodeResolver` re-applies it after start/restart. External names still
  resolve (the Intranet's bind forwards to `127.0.0.11`).

### Intranet required (UI + validation)

- **Frontend** (`StackDesigner.jsx`): the PMM3 (and any future non-Intranet)
  add-button is disabled until an Intranet node exists (`disabled={!hasIntranet}`,
  with a tooltip); `addNode` also guards against adding a non-Intranet node first.
- **Validation** (`validateStack`): errors if any non-Intranet node exists with no
  Intranet node ("An Intranet node is required — add one before deploying other
  nodes").

### Verification performed

Deployed an Intranet + **two PMM nodes both labelled "pmm"**:
- Unique hostnames/FQDNs: `intranet.example.net`, `pmm-c170.example.net`,
  `pmm-c629.example.net`; the Intranet pinned to `172.20.0.2`.
- From a PMM node (resolv.conf → `172.20.0.2` only): **forward** resolves all
  three hosts (and short names via the search domain); **reverse** `dig -x` /
  `getent hosts <ip>` returns each FQDN incl. `intranet.example.net`; **external**
  (`repo.percona.com`) resolves via the Intranet's forwarder; the Grafana SMTP
  target `intranet.example.net` resolves.
- The Intranet's forward + reverse zone files contain an entry for every host
  including itself; `dig @127.0.0.1` on the Intranet answers both directions.
- **Restart** of a PMM node kept resolv.conf pointed at the Intranet and the zone
  was rebuilt with the node's (possibly new) IP — forward + reverse stayed
  consistent.
- **Gating:** validating a stack of PMM-only nodes errors; the PMM3 button is
  disabled until an Intranet is added.
- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.

---

## 6. Auto-numbered labels, unique-label deploy gate, and canvas minimap

### Per-type auto-numbered labels

Non-Intranet nodes are now created with an auto-numbered label `"<slug>-NN"` — NN
zero-padded from `01` and increasing **per node type** (`pmm-01`, `pmm-02`, …; a
future Percona Server type would give `psmysql-01`, `psmysql-02`, …). The Intranet
singleton keeps its plain label. `nextLabel(type, nodes)` (in `StackDesigner.jsx`)
parses existing `^<slug>-(\d+)$` labels of that type and uses `max+1`; each node
type carries an optional `slug` (defaults to the type key). Because these labels
are unique by construction, they become the node **hostnames / FQDNs** directly in
the Intranet DNS (`stackHostnames` no longer needs its hash-suffix fallback for
the common case — e.g. `pmm-01.example.net`).

### Unique-label deploy gate

Labels are DNS hostnames, so `validateStack` now **errors** (blocking deploy) when
any label is **duplicated** ("Duplicate node label: … — labels must be unique") or
**blank** ("Every node must have a label"). This replaces the earlier soft
warning.

### Minimap

`StackDesigner.jsx` gained a **`Minimap`** in the canvas's bottom-right corner: a
scaled overview of the whole design showing every node (colored by type, the
selected one outlined) and the current **viewport** rectangle. It tracks pan/zoom,
auto-fits the bounds of all nodes plus the viewport, and is **interactive** —
click or drag inside it to recenter the main view on that point (its pointer
handlers `stopPropagation` so they don't trigger a canvas pan).

### Verification performed

- Validation: a stack with two `"pmm"`-labelled nodes errors with the duplicate
  message; relabelled `pmm-01`/`pmm-02` it passes; a blank label errors. Numbered
  labels carry through to hostnames (`pmm-01` → `pmm-01.example.net`).
- `go build`/`vet`, `gofmt`, and the web build pass.

---

## 7. Percona XtraDB Cluster (PXC) frame

A Galera **cluster** modeled as a canvas **frame** holding PXC nodes. PXC nodes
run on the systemd OS images (built by `make images`) with the
percona-xtradb-cluster packages installed at deploy time. Built in phases A–F.

### `make versions` (Phase A)

`images/versions.sh` now sorts **every** series newest-first (`sort -rV`) and, for
each image, also discovers **percona-xtradb-cluster** versions (`pxc80` /
`pxc84lts`) into a `percona_xtradb_cluster:` map (8.0/8.4), mirroring
`percona_server`. RHEL needs `dnf module disable mysql`; Ubuntu PXC packages
carry an epoch (`1:8.0.45-…`) that is stripped. The package version line is
`^percona-xtradb-cluster-[0-9]` (the meta package), which excludes `-garbd`,
`-server`, etc.

### Data model + catalog (Phase B)

- `.env`/`.env.example`/compose add `APP_PASSWORD` / `REPL_PASSWORD` (defaults
  `app_password` / `repl_password`) — the app and replication DB users.
- The canvas design doc gains **`frames[]`**. `designFrame` carries the PXC
  cluster config (OS/version/arch, PXC major/minor, root password, PMM monitor,
  proxy, GTID, cert + TTL); PXC nodes are `designNode`s with `frameId`, `role`
  (`regular`/`arbitrator`), and `exportEnabled`/`exportHostPort`.
- `versions.go` parses the per-image `percona_xtradb_cluster` sections;
  `GET /api/catalog/pxc` returns installable PXC versions per OS/arch. (The YAML
  key-quote bug — `splitYAMLKV` didn't unquote the `"8.0"` key — was fixed.)

### Canvas frame UI (Phase C)

`StackDesigner.jsx` gained frame support: a **"PXC Cluster"** toolbar button
(gated on Intranet) creates a frame with **3 PXC nodes**; the frame title has
**+/-** to add/remove nodes. Cluster names auto-number **`pxc-cluster-NN`** (from
00) and node names **`pxcNN`**, unique across the whole stack. Frame properties
(version/OS/arch from the catalog, root pw, PMM monitor, proxy, GTID, cert+TTL,
quorum guidance) and node properties (regular/arbitrator, host-port export) live
in the side panel. Frames render behind nodes, lay their members out in a row,
and drag as a unit; PXC nodes are excluded from the normal node loop.

### Provisioning (Phase D) — `pxc.go`

`provisionPXCFrame` orchestrates a whole cluster as one unit:
1. Wait for the Intranet to be **running** (DNS/CA/proxy).
2. **In parallel** per node: create the container (systemd image, Intranet
   resolver, regular nodes publish 3306 to the host via `PublishMap` when export
   is on), install `percona-xtradb-cluster` (or `-garbd` for arbitrators) via
   `percona-release`, and write `/etc/my.cnf` (server-id, GTID, wsrep, gcomm of
   all regular FQDNs). DNS is reconciled so every FQDN resolves.
3. **Sequentially**: bootstrap the first regular node (`mysql@bootstrap`), set the
   root password, create the app/repl users; join the rest (`mysql`, xtrabackup
   SST); start `garbd` for arbitrators.
4. Optional per-node **TLS**: certs signed by the Intranet CA into
   `/var/lib/mysql/{ca,server-cert,server-key,client-cert,client-key}.pem`
   (mysql-owned, TTL), `ssl-*` added to my.cnf, mysqld restarted.
5. Optional **PMM** registration (best-effort) and **Intranet Squid proxy** for
   package egress.

- **GTID** (default on): `server-id` (from the `pxcNN` name), `gtid_mode=ON`,
  `enforce_gtid_consistency=ON`, `binlog_format=ROW`, `log_bin=ON`, and
  `log_replica_updates=ON` for 8.4 / 8.0.26+ (`log_slave_updates` for older 8.0).
- **Cluster traffic** runs with `pxc_encrypt_cluster_traffic=OFF` (the stack
  network is isolated; PXC's default ON requires all nodes to share a CA, which
  conflicts with SST mirroring per-node certs into the datadir). The per-node
  certs provide **client (3306) TLS**.
- The **four ports** (3306/4567/4444/4568) are reachable between nodes on the
  stack network; only **3306** is published to the host (export option).

Two general bugs were fixed while getting this working: `CopyFile`/`tarFiles`
now stamp a current tar **ModTime** (bind's `rndc reload` keys off mtime, so
zero-mtime zone files were never re-read — DNS silently went stale), and the DNS
reconcile uses a **monotonic serial** + per-zone `rndc reload`.

### Validation (Phase E)

`validateStack` checks each PXC frame: **≥1 regular node** (error), **duplicate
cluster names** (error), **export host-port conflicts** within the design and
against ports already published by other containers (error; the stack's own
containers are excluded so redeploy doesn't self-flag — via
`Docker.ListPublishedPorts`), and **warnings** for <3 regular nodes and even node
counts (split-brain quorum).

### Management (Phase F)

A running PXC node shows **`PXCManager`** in the properties panel: Overview
(cluster/role/FQDN/server-id/ports/GTID/TLS/monitor + host-access `host:port` +
root console + delete), Credentials (root/app/repl), and a Certificate frame to
re-issue from the Intranet CA with a TTL (`GET`/`POST /pxc/cert`, reusing the
deploy-time `pxcApplyCert`). Arbitrators show only Overview.

### Verification performed

- `make versions` records PXC 8.0/8.4 per image (newest-first); `GET
  /api/catalog/pxc` returns them per OS/arch.
- Manual recipe validation on two OL9 systemd containers nailed the install /
  bootstrap / SST / garbd commands and the `pxc_encrypt_cluster_traffic` issue.
- End-to-end via the app: a **2 data + 1 arbitrator** cluster on OL9 reached
  `wsrep_cluster_size=3`, Primary/Synced; GTID fully on (`log_replica_updates`,
  `server_id=1`); app/repl users present; **replication works** (write on n1 read
  on n2); per-node certs are **Intranet-CA-signed** (`CN=pxc01.example.net`,
  issuer `DBCanvas CA`, in `/var/lib/mysql`, mysql-owned, 365-day TTL) with
  `have_ssl=YES`; **3306 reachable from the host** on the chosen export port
  (connected as `app`); garbd active on the arbitrator (no mysqld).
- Validation: arbitrator-only frame, duplicate cluster name, and duplicate export
  port all error; a 3-regular cluster passes.
- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.

> Note: Ubuntu/Debian PXC paths are wired (provider dir, apt install) but only
> the Oracle Linux path was validated end-to-end; PMM auto-registration and the
> Squid proxy are best-effort (non-fatal) steps.
