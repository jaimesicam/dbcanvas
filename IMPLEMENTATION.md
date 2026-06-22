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
