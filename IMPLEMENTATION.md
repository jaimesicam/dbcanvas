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
  - **rsyslog** is installed + enabled on the Intranet (in the install/enable steps)
    and on **every other systemd-image node** (PXC, ProxySQL, Percona Server /
    replication) via a shared best-effort **`ensureRsyslog`** helper
    (`command -v rsyslogd || install; systemctl enable --now rsyslog`) called after
    each node's package install. (PMM uses the `percona/pmm-server` image with its
    own logging and is excluded.)
  - **Squid cache** (a "Configure Squid" step before services start): appends
    `maximum_object_size 150 MB` and `cache_dir ufs /var/spool/squid 4000 16 256`
    to `/etc/squid/squid.conf` (idempotent grep-guarded) and ensures the
    `/var/spool/squid` dir exists. The cache **swap directories are initialized by
    the squid.service's own `ExecStartPre` (`cache_swap.sh`) on start** — we must
    **not** run `squid -z` manually, as it leaves a detached instance + `/run/squid.pid`
    that makes the subsequent `systemctl start` fail with "Squid is already running"
    (`Result: protocol`).
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
  terminal layer — so sessions **survive navigation** (leaving/returning to the
  Stacks page doesn't reset them). The bottom **dock** is multi-tab (one per
  container), minimisable, with a drag-to-resize height handle. **Each tab is
  individually detachable** into its own floating window (the **⧉** button on the
  tab) and **re-attachable** (the **Dock** button on the window); floating windows
  drag by their header and free-resize (CSS `resize`). Detach/attach does **not**
  re-create xterm — each session owns a persistent host `<div>` (with xterm opened
  into it once) that is **re-parented via `appendChild`** between the dock area and
  its floating window, so scrollback and the live socket survive the move. The dock
  height persists in `localStorage`. "Enter root console" is offered from the node
  right-click menu and the Intranet Overview tab.
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

---

## 8. PXC refinements — descriptions, post-deploy PMM toggle, XtraBackup

Three follow-ups to the PXC frame (§7).

### Canvas descriptions (frontend)

The PXC cluster **frame** and its **node cards** now carry a description like the
other node types. In `StackDesigner.jsx`:
- The frame title bar gains a second muted line: **`Percona XtraDB Cluster <ver> ·
  N node(s)`** (`pxcVersionLabel(f)` — the pinned minor `pxcVersion` if set, else
  the `pxcMajor` series), with the cluster name on the first line and the +/-
  buttons unchanged on the right.
- Each PXC node card replaces the bare `regular`/`arbitrator` word with a fuller
  description — **"Galera data node"** for regular members, **"Arbitrator ·
  garbd"** for arbitrators — and adds an **OS + platform** line
  (`pxcOSLabel(f) · <arch>`, e.g. "Oracle Linux 9 · amd64") taken from the frame,
  matching the OS/arch line the Intranet/PMM node cards already show.

### Post-deploy PMM monitoring toggle

A deployed PXC cluster can now be **switched on/off PMM monitoring** without a
redeploy, from the frame's properties panel.

- **Backend** `app/pxc_mgmt.go` + route `POST /api/stacks/{id}/frames/{fid}/pmm`
  (`handlePXCFrameMonitor`): body `{pmmNodeId}` — a PMM node id registers every
  **running regular** member with that PMM server; `""` deregisters them
  (arbitrators have no MySQL, so they are skipped). It records the change in each
  member's `config.MonitoredBy`; the frame's `pmmNodeId` itself is persisted by
  the designer's autosave (the handler does **not** rewrite the design, to avoid
  clobbering a concurrent autosave). Selecting a PMM node that isn't running →
  409.
- **pmm-client installed unconditionally at deploy.** Every **data node** installs
  the PMM client at provision time (`pxcPrepareNode`, ~45%, regular nodes only —
  arbitrators have no MySQL), **regardless of whether monitoring is on**, so it can
  be enabled on-the-fly later without an install: `pxcInstallPMMClient{RHEL,Debian}`
  = **`percona-release setup pmm3-client`** then `dnf install pmm-client` (OEL) /
  `apt-get update && apt-get install pmm-client` (Ubuntu — the repo `update` was
  previously missing). The install fails loudly (no `|| true`) so a broken install
  surfaces; the earlier register scripts used `percona-release enable` and swallowed
  every error with `|| true`, so a failed install was silent and the node never
  joined PMM. The register scripts still re-ensure the client (guarded by
  `command -v pmm-admin`) so they self-heal on clusters provisioned before the
  install became unconditional. Turning monitoring **off only deregisters**
  (`pxcPMMRemoveScript`: `pmm-admin remove mysql` + `pmm-admin unregister --force`)
  — it never uninstalls pmm-client.
- **Real PMM credentials.** Registration previously hard-coded `admin:admin` in
  the `--server-url`. A new **`pmmServerFor(st, doc, pmmNodeId)`** resolves the
  PMM node's FQDN + admin user/password from its deployment **secrets**
  (`pmmSecrets`), and the register scripts now use `https://$PMM_USER:$PMM_PASS@…`
  (`--force` so re-config is idempotent; `pmm-admin remove` before `add` so
  re-registration doesn't duplicate the service). The deploy-time best-effort
  registration (`provisionPXCFrame` Phase 3) feeds the same credentials, falling
  back to `admin/admin` when the PMM node isn't up yet. New deregister script
  `pxcPMMRemoveScript` (`pmm-admin remove mysql` + `pmm-admin unregister --force`).
- **Frontend.** `PXCFrameForm` now takes `stackId` + `running`; when any member is
  running it shows an **Apply PMM monitoring / Disable PMM monitoring** button
  (busy/success/error states) that calls **`frameApi(id, fid).setMonitoring()`**
  (new in `lib/stackApi.js`, `POST …/frames/{fid}/pmm`). The "Monitored by (PMM)"
  select stays editable post-deploy and drives the apply.

### Percona XtraBackup on data nodes

PXC's SST method is `xtrabackup-v2`, so every **regular (data)** node now installs
**Percona XtraBackup** matching the cluster's series, in `pxcPrepareNode` (after
the PXC package, ~40%): `percona-release setup pxb80` → `percona-xtrabackup-80`
for PXC 8.0, `percona-release setup pxb84lts` → `percona-xtrabackup-84` for 8.4
(RHEL uses `dnf install`, Ubuntu `apt install` — `pxcInstallXtrabackup{RHEL,Debian}`,
mapped by `pxbProduct`/`pxbPackage`). Arbitrators (garbd, no datadir/SST) skip it.

### Slow query log

Every data node enables the slow query log: `pxcMyCnf` now writes
`slow_query_log=ON`, `slow_query_log_file=/var/lib/mysql/slow.log` (the
mysql-owned datadir, so mysqld can always create it), and `long_query_time=2` to
the `[mysqld]` section. Arbitrators run garbd only (no mysqld) and have no my.cnf.

### Root login (`/root/.my.cnf`) + monitor user

- **`/root/.my.cnf`.** Every data node gets `/root/.my.cnf` (mode 0600) with a
  `[client]` section (`user=root`, `password=<root pw>`, `socket=…`), so the unix
  root user can run `mysql` without typing the password (`pxcRootMyCnf`). It is
  written **after** the root password is established — after bootstrap on the first
  node, after SST on the joiners — so it doesn't interfere with the bootstrap
  auth_socket path.
- **`monitor`@'%' user.** A monitoring user is created on the bootstrap node (and
  replicated cluster-wide by Galera) with PMM-appropriate grants
  (`SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.*` + `SELECT ON
  performance_schema.*`, `MAX_USER_CONNECTIONS 10`). Its password comes from the new
  **`MONITOR_PASSWORD`** env (default `monitor_password`), added to
  `.env`/`.env.example`/`docker-compose.yml` alongside `APP_PASSWORD`/`REPL_PASSWORD`
  and carried in `pxcSecrets` (`MonitorUser`/`MonitorPassword`). The PXC manager's
  **Credentials** tab now lists the monitor user/password too. (PMM registration
  itself still uses root for now — the monitor user is created and available.)
- **`cluster`@'%' user.** Likewise created at bootstrap with **`ALL PRIVILEGES …
  WITH GRANT OPTION`** (it replaces root as ProxySQL's `CLUSTER_USERNAME`). Password
  from the new **`CLUSTER_PASSWORD`** env (default `cluster_password`); carried in
  `pxcSecrets` (`ClusterUser`/`ClusterPassword`) and consumed by §9's ProxySQL.

### Ubuntu/Debian PXC fixes

The Ubuntu path (previously only wired, not validated) had several distro-specific
bugs, all fixed by making the provisioner OS-aware:

- **Config file was ignored → every node bootstrapped standalone.** DBCanvas wrote
  `/etc/my.cnf`, but on Debian that is read *before* the package's `/etc/mysql`
  includes, whose default **empty `wsrep_cluster_address`** then overrode ours — so
  each node formed its own single-node cluster. Now on Debian the config is written
  to **`/etc/mysql/dbcanvas.cnf`** and a trailing **`!include /etc/mysql/dbcanvas.cnf`**
  is appended to `/etc/mysql/my.cnf` (`pxcDebianIncludeCnf`) so it is read **last**
  and wins (`pxcCnfPath`/`pxcCnfDir`). `pxcMyCnf` also now sets `bind-address=0.0.0.0`
  (Debian's package config defaults to `127.0.0.1`, which would block the published
  host port and cross-node access) and uses an OS-aware **error-log path**
  (`/var/log/mysql/error.log` on Debian — apparmor only permits `/var/log/mysql`;
  `pxcLogError`).
- **Root password was not applied.** The bootstrap script only handled RHEL's
  *temporary password* logged to the error log. Debian/Ubuntu leaves
  `root@localhost` on **auth_socket** (no password), so that path was skipped and
  `mysql -uroot -p…` then failed. `pxcBootstrapScript` now handles all three cases:
  already-set (redeploy), RHEL temp-password (`ALTER USER … IDENTIFIED BY`), and
  Debian auth_socket (connect over the socket as the root OS user and
  `ALTER USER … IDENTIFIED WITH caching_sha2_password BY`). **Note:** the temp
  password is *expired*, which permits only `ALTER USER`, not `SELECT` — so the
  script must **not** probe it with a `SELECT 1` first (an earlier revision did and
  fell through to the passwordless branch → `Access denied … using password: NO` on
  OEL); it runs the `ALTER` directly with `--connect-expired-password`. `LOGERR` is passed in so
  the temp-password grep / failure tails read the right file; the join and cert
  scripts take `LOGERR`/`CNF` too (the cert script appends `ssl-*` to the OS-correct
  config file).
- **pmm-agent not enabled when joining PMM.** `pmm-admin config` talks to the local
  pmm-agent (127.0.0.1:7777), which the RHEL package starts at install but the
  Debian package leaves **disabled** — so registration failed. The register scripts
  now run **`systemctl enable --now pmm-agent`** before (and after) `pmm-admin
  config` on both families.
- **MySQL service wasn't added → register over the socket (not TCP).**
  `pmm-admin add mysql` connected as **root over TCP** (`--host=127.0.0.1`), which
  fails: `root@localhost` doesn't match a TCP connection and caching_sha2 over plain
  TCP needs the server key — so the MySQL service was never added (most visibly on
  Ubuntu). It now adds the service as **root over the unix socket**
  (`--username=$DB_USER --password=$DB_PW --socket=/var/lib/mysql/mysql.sock`),
  which authenticates cleanly (socket = secure transport, so caching_sha2 works
  without TLS). `pxcPMMEnv` passes the root creds from `pxcSecrets`, and the `add`
  no longer pipes to `/dev/null` so a real failure surfaces. (The `monitor` user is
  **not** used here — it is reserved for ProxySQL.) The **query source** is chosen at
  registration time: `slowlog` when `@@global.slow_query_log` is on (the default —
  see Slow query log above), otherwise `perfschema`.
- **Arbitrator config path.** garbd's config is `/etc/sysconfig/garb` on RHEL but
  **`/etc/default/garb`** on Debian; `pxcStartGarbd` now passes the right path
  (`GARBCONF`) and the script writes there.

### Deploy/validate flush the design first

The certificate step was running even when "Generate per-node certificates" was
**unticked**: the designer autosave is debounced (600 ms), so unticking and
clicking **Deploy** quickly deployed the *previously saved* design (cert still on).
`runDeploy`/`runValidate` now call a new **`saveNow()`** that flushes the current
canvas (nodes/edges/frames/view) to the server **before** validating/deploying, so
the deploy acts on exactly what's on screen.

Separately, the cert step failed **silently** (the script sent all openssl/systemctl
output to `/dev/null`, so the deploy log showed `attempt N/10 failed:` with no
message). `pxcCertScript` no longer discards stderr and now checks `command -v
openssl` up front, so a real cause surfaces (e.g. openssl missing from the base
image, or mysqld failing to restart with TLS).

### Frame OS/version cascade

Changing a PXC frame's **OS** left the now-invalid `osVersion`/`arch`/`pxcMajor`
in place (e.g. switching to ubuntu kept `osVersion="9"`), so the catalog `entry`
lookup missed and the **PXC major/minor selects came up empty** until you toggled
the version back and forth. `PXCFrameForm` now has a normalization effect that
**cascade-snaps** each invalid dependent field to the first valid option for the
current catalog (osVersion → arch → major → clears an invalid minor) in one pass,
skipped when the frame is deployed (locked).

### Verification performed

- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.
- **Caveat:** the Ubuntu/Debian PXC path is still **not validated end-to-end on a
  live cluster** — these fixes target the specific distro differences (config
  precedence, auth_socket, pmm-agent, garb path) but a real Ubuntu deploy should be
  run to confirm.

---

## 9. ProxySQL node

A **ProxySQL** node — a MySQL proxy that fronts a PXC cluster and routes
application traffic (read/write split or load-balanced) to its members. It runs on
a systemd OS image (built by `make images`), is wired to a **PXC cluster frame**
via a canvas **association line**, and is configured with `proxysql-admin`. Like
PXC nodes it can be PMM-monitored and can publish its ports to the host.

### `make versions` (ProxySQL discovery)

`images/versions.sh` now also probes **ProxySQL** versions per image and writes a
`proxysql:` map keyed by major series **"2"/"3"** (mirroring `percona_server` /
`percona_xtradb_cluster`). Discovery: a single `percona-release setup proxysql`
repo carries both packages, enumerated separately — RHEL
`yum/dnf search proxysql2|proxysql3 --showduplicates`, Ubuntu
`apt-cache madison proxysql2 / proxysql3`. New probe markers `@@PROXYSQL2@@` /
`@@PROXYSQL3@@`; `emit_series` was generalized to take the two series keys
(so it serves "8.0"/"8.4" and "2"/"3").

### Catalog (versions.go)

`loadPXCCatalog` was generalized into **`loadImageCatalog(section)`** (parses any
per-image major-series map); `loadPXCCatalog`/`loadProxySQLCatalog` call it with
`percona_xtradb_cluster` / `proxysql`. New route **`GET /api/catalog/proxysql`**
(`handleProxySQLCatalog`) → `{images:[{os,osVersion,arch,versions:{"2":[…],"3":[…]}}]}`.

### Data model

- `designNode` gains `osVersion` (shared) and ProxySQL fields: `proxysqlMajor`
  ("2"/"3"), `proxysqlVersion` (minor, "" → latest), `mode`
  (`singlewrite` default | `loadbal`), `pmmNodeId`, plus the existing
  `exportEnabled`/`exportHostPort`.
- `designDoc` now carries **`edges`** (`designEdge{from,to:{node,port},type}`; an
  endpoint's `node` may be a node **or** a frame id). **`pxcFrameForProxySQL(doc,
  nodeID)`** resolves the PXC frame a ProxySQL node is linked to.

### Provisioning — `app/proxysql.go`

`provisionProxySQL` records the deployment then runs an async, stepwise goroutine:
1. Wait for the **Intranet** (resolver/CA), then for the **associated PXC cluster**
   to be running (`waitPXCRunning` — polls the frame's regular members; returns a
   member FQDN as `CLUSTER_HOSTNAME` and that member's `pxcSecrets`).
2. **Create + start** the container (systemd image `dbcanvas-systemd:<os>-<ver>-<arch>`,
   Intranet resolver, publishing **6033** (MySQL) and **6032** (admin) to the host
   when export is on).
3. **Install** `proxysql2`/`proxysql3` + **`which`** (`proxysql-admin` shells out to
   it; absent on a minimal OEL image — Debian's ships in `debianutils`), the
   **Percona Server mysql client** (`percona-server-client` via `ps80`/`ps84lts`
   matching the cluster — `proxysql-admin` needs the `mysql` client to talk to PXC),
   and **pmm-client** (always, so monitoring can be turned on later). When the node's
   **Use Intranet proxy** option (`useProxy`) is on, the package manager's proxy is
   pointed at the Intranet Squid (`pkgProxy{RHEL,Debian}`) once up front so every
   install egresses through it.
4. **Configure `/etc/proxysql-admin.cnf`** and `proxysql-admin --enable`: the keys
   come from the linked cluster — `CLUSTER_USERNAME/PASSWORD` = the PXC **`cluster`**
   admin user (`CLUSTER_PASSWORD` from `.env`, default `cluster_password` —
   created `cluster`@'%' `WITH GRANT OPTION` on the cluster at bootstrap), not root;
   `CLUSTER_HOSTNAME` = a PXC node FQDN, `MONITOR_USERNAME/PASSWORD` = the PXC
   **monitor** user (the user reserved in §8 for exactly this), `CLUSTER_APP_*` =
   PXC **app** user/`APP_PASSWORD`, and **`MODE`** = `singlewrite`|`loadbal`.
   `proxysql-admin --enable` is **interactive** (it prompts "enter a new password
   [y/n]?" because the `monitor` user already exists), so it is run with
   **`--use-existing-monitor-password`**, which keeps it non-interactive.
5. Optional **PMM** registration (`pmm-admin add proxysql … --port 6032`).

`proxysqlConfig`/`proxysqlSecrets` store the profile (image, mode, cluster,
backend host, published host ports, PMM target) and credentials (ProxySQL admin
interface + the backend app/monitor/cluster creds). `refreshPublishedPorts` and
the lifecycle (start/stop/restart) handle the 6033/6032 host ports like the other
nodes. Deploy dispatch adds a `proxysql` case; **install ignores the selected
minor version** and installs the major package (same as PXC).

### Validation

`validateStack`: a ProxySQL node requires its **OS image to exist** (`make images`)
and to be **linked to a PXC cluster** (error otherwise); its export host port joins
the shared port-conflict check.

### Frontend

- **`NODE_TYPES.proxysql`** (`ports: true`, dedicated **`Icon.ProxySQL`** — a
  proxy/router fanning a client out to three cluster backends) + a **ProxySQL**
  toolbar button (gated
  on Intranet). The canvas connection system was extended so a **PXC cluster frame
  exposes its 4 ports** (`PortHandles` on the frame, rendered last so they sit above
  the title bar) and `rectOf`/`hitPort` resolve **frame** endpoints; only
  ports-enabled free nodes and PXC frames are connectable. Association rules live in
  **`tryConnect`/`createFlow`** (every edge is a directed data flow, arrow at the
  destination, captioned **"forwards SQL traffic to"** at its midpoint):
  - **PXC frame → ProxySQL** — orientation is fixed (frame is always the source);
    no prompt.
  - **ProxySQL → ProxySQL** — a **`LinkDirectionModal`** asks which way data flows
    (A→B or B→A); the option whose destination already receives a flow is disabled.
  - **One incoming flow per ProxySQL** — `createFlow` rejects (no arrow) if the
    destination already has any incoming edge (from a PXC frame *or* another
    ProxySQL). So dropping a frame→ProxySQL link onto a ProxySQL that already
    receives one is silently ignored.
  - **No frame↔frame links** (a PXC cluster can't associate with another cluster or
    itself) and **no self-links**.
  A ProxySQL chained behind another ProxySQL still resolves its upstream PXC cluster:
  both **`pxcFrameForProxySQL`** (backend) and the form's linked-cluster banner now
  **walk the association graph** (BFS) rather than only checking direct edges.
- **`ProxySQLForm`** also has a **Use Intranet proxy (Squid)** checkbox (`useProxy`).
- **`ProxySQLForm`** (undeployed): catalog-driven OS/version/arch + ProxySQL
  major/minor (same cascade-normalization as the PXC frame), **mode** select, PMM
  monitor select, host-port export, and a linked-cluster banner (error until an
  association line is drawn).
- **`ProxySQLManager`** (running): Overview / Access (host:port for 6033 app
  traffic + 6032 admin) / Credentials. The generic right-click menu already gives
  **root console, start, stop, restart, delete**.
- `stackApi.proxysqlCatalog()`.

### Verification performed

- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.
- **Caveat — not validated end-to-end on a live deployment.** The ProxySQL install,
  `proxysql-admin.cnf` keys, `proxysql-admin --enable` flags and PMM `add proxysql`
  follow Percona's documented usage but were **not** run against real containers; a
  live deploy (OL first, then Ubuntu) should confirm, and `make versions` should be
  re-run to populate the `proxysql:` catalog.

---

## 10. ProxySQL cluster frame

A **ProxySQL cluster frame** — a canvas frame (like the PXC cluster frame, §7)
holding ProxySQL nodes, all fronting the same PXC cluster. Add/remove members with
the frame's **+/-**; minimum one member. The members have **no exposed endpoints**
— only the **frame** carries the association port.

### Data model + provisioning

- `designFrame` gains `Type=="proxysql"` and ProxySQL fields (`proxysqlMajor`,
  `proxysqlVersion`, `mode`; it reuses `os`/`osVersion`/`arch`, `pmmNodeId`,
  `useProxy`). Members are `designNode`s with `Type=="proxysql"` + `FrameID`, each
  carrying its own `exportEnabled`/`exportHostPort`. The frame's `os`/`osVersion`/
  `arch` drive the **shared image** — members do **not** carry their own (so they're
  validated/provisioned via the frame, never as standalone nodes; standalone
  `provisionProxySQL` is a thin wrapper over `provisionProxySQLInstance`).
- **`provisionProxySQLFrame`** brings the cluster up as one unit: it waits for the
  Intranet + the PXC cluster (resolved from the **frame's** single association via
  `pxcFrameForProxySQL`), then **in parallel** `proxysqlPrepareMember` creates each
  container and installs ProxySQL + mysql client + pmm-client and starts proxysql.
  It then **joins all members into a native ProxySQL cluster** (`proxysqlClusterScript`
  — a dedicated cluster sync credential + every member listed in `proxysql_servers`),
  and runs **`proxysql-admin --enable` on a single primary** member; the backend
  config (mysql_servers/users) then syncs across the cluster. So **only one member
  configures the whole cluster** — members do not each run `proxysql-admin`.
- **Deploy dispatch** (`handleDeployStack`) skips **all frame members** (`FrameID != ""`,
  PXC or ProxySQL) in the per-node loop — they are provisioned by their frame, not
  individually (this also prevents the double-provisioning a member would otherwise
  get). **Validation** skips ProxySQL members in the standalone-node case (so they
  no longer demand their own PXC link or report a `dbcanvas-systemd:--amd64` image)
  and validates the **frame** instead: a ProxySQL cluster needs **≥1 member**, its
  **OS image** to exist, a **PXC-cluster association**, a unique cluster name, and
  its members' export ports join the shared port-conflict check.
  `refreshPublishedPorts` already covers member nodes (type `proxysql`).

### Canvas + association rules

- **"ProxySQL Cluster" toolbar button** (gated on Intranet); members auto-named
  `proxysqlNN`, cluster `proxysql-cluster-NN`. Frame rendering is now type-aware
  (color/description/member-card per `f.type`, via `frameColor`/`frameVersionLabel`);
  the `Database` PXC accent stays purple, ProxySQL frames are amber. `+/-` dispatch
  through `addFrameMember`/`removePXCNode`.
- The association ruleset (`endpointKind`/`tryConnect`/`createFlow`) now has three
  connectable endpoint kinds: **`pxc`** (frame, source only), **`proxysql`**
  (standalone node), **`proxysql-frame`** (cluster frame). Rules:
  - **PXC frame → ProxySQL node or ProxySQL cluster frame** — frame is the source;
    a PXC frame may have **at most one outgoing** link (`createFlow` `singleOutgoing`),
    so once a cluster points at one ProxySQL/ProxySQL-cluster you can't add another.
  - **A ProxySQL cluster frame has at most one incoming** flow and **no outgoing**
    (it can't be a source — only `pxc → proxysql-frame` is accepted, regardless of
    drag direction).
  - ProxySQL **node ↔ node** still prompts for direction; frame↔frame and self
    links remain disallowed.
- **`ProxySQLFrameForm`** (catalog-driven OS/version/major/minor cascade, mode, PMM
  monitor, Intranet-proxy, linked-cluster banner) and **`ProxySQLFrameMemberForm`**
  (per-member host-port export only). A running member shows **`ProxySQLManager`**;
  the generic right-click menu gives **view config, root console, stop, restart,
  delete** after deploy.

### Verification performed

- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.
- **Caveat — not validated on a live deployment.** The native ProxySQL clustering
  setup (`proxysql_servers` + the `admin-cluster_*` sync credential applied over the
  6032 admin interface, then `proxysql-admin --enable` on a single primary) follows
  ProxySQL's documented clustering model but was **not** run against real containers;
  a live deploy should confirm the config actually syncs to the non-primary members.

---

## 11. Percona Server: replication frame + standalone node

A **Percona Server Replication** frame (`Type=="mysql"` internally): a primary +
one or more secondaries running Percona Server (`percona-server-server`) on the
systemd OS images, with GTID-based replication. Default = 1 primary + 2 secondaries
(validation requires exactly one primary and **≥1 secondary**). It has the PXC
frame's options plus a **replication mode** (normal/async or semi-synchronous), and
every member's properties pick its **role** (primary | secondary, with exactly one
primary enforced). *(The feature is labelled "Percona Server Replication"
throughout the UI; the frame/member type key stays `mysql`.)*

A standalone **Percona Server** node (`Type=="ps"`) is also available: a single
read/write Percona Server instance with the **same options minus the replication
mode and role**. `provisionPerconaServer` reuses the replication primary path
(`mysqlPrepareNode` + `mysqlSetupPrimary`) via a synthetic single-node frame built
from the node's settings; it deploys in the per-node loop (dispatch/validation case
`ps`, ports refreshed like `mysql`), exports 3306, and shows in `MySQLManager`
(role rendered as *standalone (read/write)*; replication/source rows hidden).

### Catalog
`percona_server` versions are already discovered by `make versions` (§3), so this
just adds `loadPSCatalog()` (= `loadImageCatalog("percona_server")`) +
`GET /api/catalog/ps` + `stackApi.psCatalog()`.

### Data model
`designFrame` gains `Type=="mysql"` + `psMajor`/`psVersion`/`replMode` (reusing
`os`/`osVersion`/`arch`, `rootPassword`, `pmmNodeId`, `useProxy`, `gtid`,
`generateCert`/`certTtl*`). Members are `designNode`s `Type=="mysql"` + `FrameID` +
`Role` (`primary`|`secondary`).

### Provisioning — `app/mysql.go`
`provisionMySQLFrame`: in parallel, create each container, install
`percona-server-server` + pmm-client, and write `my.cnf` (unique `server-id`, GTID
on, `log_bin`, `binlog_format=ROW`, the version-correct `log_replica_updates`/
`log_slave_updates`). Then **sequentially** bootstrap the primary (set root pw,
create app/repl/monitor/cluster users — which replicate via GTID — `read_only=OFF`),
then attach each secondary.

**Keyword versioning (8.0.23+ / 8.4 safe — the removed forms are never used):**
- `CHANGE REPLICATION SOURCE TO … SOURCE_HOST=…, SOURCE_USER=…,
  SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1` (not `CHANGE MASTER`/`MASTER_HOST`).
- `START REPLICA`, `SHOW REPLICA STATUS` (not `START SLAVE`/`SHOW SLAVE STATUS`).
- `RESET MASTER` (8.0) / `RESET BINARY LOGS AND GTIDS` (8.4) clears each node's
  GTID/binlog history right after the root-password reset: on the primary before it
  creates the users (so the replicated history starts clean), and on each replica
  before `CHANGE REPLICATION SOURCE` (so AUTO_POSITION fetches the full history with
  **no errant GTIDs**). **Note:** the root-password reset is run as a **bare
  `ALTER USER`** — an expired temp password permits *only* `ALTER USER`, so prefixing
  it (e.g. with `SET sql_log_bin=0`) fails with `ERROR 1820`; the subsequent RESET is
  what removes the GTID that the binlogged `ALTER` creates.
- After `START REPLICA` succeeds, the secondary is made `SET PERSIST read_only=ON;
  super_read_only=ON` (so a fronting ProxySQL classifies it as a reader, and the
  setting survives restarts; the replication applier bypasses it).
- **Semi-sync** plugin/variable names branch by series: 8.0 `rpl_semi_sync_master`/
  `_slave` (`semisync_master.so`/`semisync_slave.so`), 8.4 `rpl_semi_sync_source`/
  `_replica` (`semisync_source.so`/`semisync_replica.so`).
- Percona Server ships the **`validate_password`** component with a MEDIUM policy
  that rejects the `.env` passwords (`app_password`, …) with `ERROR 1819`. The
  primary runs `SET GLOBAL validate_password.policy=LOW; …length=6` (tolerated if
  absent) **before** creating the users so they're accepted; replicas receive the
  already-hashed `CREATE USER` form from the binlog, so they don't re-validate. The
  same relax was added defensively to the PXC bootstrap (§7).

Per-node TLS reuses `pxcApplyCert` (unit `mysqld` on RHEL / `mysql` on Debian); PMM
registration + Squid-proxy egress reuse the PXC helpers. Each node also gets
`/root/.my.cnf` (`pxcRootMyCnf`, mode 0600) so the unix root user can run `mysql`
without a password, like the PXC nodes. Deploy dispatch + the frames loop +
`refreshPublishedPorts` + validation all handle `mysql` frames.

### ProxySQL for a MySQL backend (manual, since `proxysql-admin` is PXC-only)
`pxcFrameForProxySQL` was generalized to **`backendFrameForProxySQL`** (returns the
frame **and** its type, `pxc`|`mysql`, walking the association graph). When a
ProxySQL node/cluster is linked to a **MySQL** frame, the provisioner skips
`proxysql-admin` and runs **`proxysqlMySQLConfigureScript`** over the 6032 admin
interface (proxysql is **started first** — `proxysql-admin` starts it itself, but
the manual path must `systemctl enable --now proxysql` + wait for 6032, else the
admin connection gets `ERROR 2003 … 6032 (111)`): defines the writer(10)/reader(20)
`mysql_replication_hostgroups`, lists
every backend in HG10 (ProxySQL's monitor moves `read_only` secondaries to HG20),
registers the app user (default HG10), points `mysql-monitor_*` at the monitor user,
and (for **read/write split** mode) adds query rules routing plain `SELECT`s to the
readers. For a ProxySQL **cluster** backed by MySQL, only the primary member is
configured (it syncs to the rest via native ProxySQL clustering). `waitMySQLRunning`
gates this on the whole topology being up.

**Backend-aware implementation mode.** ProxySQL's "implementation mode" options
depend on the linked backend and the irrelevant set is never shown
(`proxyModeOpts` + a normalization effect on both ProxySQL forms;
`proxysqlConfig.BackendKind` records which applies):
- **PXC backend** → `singlewrite` | `loadbal` (passed to `proxysql-admin`).
- **MySQL backend** → `primary` (all traffic to the primary; no read split) |
  `rwsplit` (writes → primary HG10, reads → replica HG20). The manual configure
  script adds the `SELECT`→reader query rules only for `rwsplit`.

### Frontend
`NODE_TYPES.mysql` (blue, DB-cylinder icon) + **"MySQL Replication"** toolbar
button; `addMySQLCluster` (1 primary + 2 secondaries; members `mysqlNN`). The
type-aware frame render shows each member's **Primary / Secondary · read-only**.
**`MySQLFrameForm`** (PS catalog cascade + replication-mode select + root pw + PMM/
proxy/GTID/cert + a "exactly one primary / ≥1 secondary" guard) and
**`MySQLMemberForm`** (role select that **auto-demotes** the current primary, host
export). A running member shows **`MySQLManager`** (Overview: role, mode, source,
read_only, server-id, GTID, host access; Credentials). The association ruleset now
treats a PXC **or** MySQL frame as a `backend` source (max one outgoing to a
ProxySQL/ProxySQL-cluster); the ProxySQL forms' linked-cluster banner accepts either.

### Verification performed
- `go build`/`vet`/`test`, `gofmt`, and the web build all pass.
- **Caveat — not validated on a live deployment.** The replication SQL (GTID
  auto-position, the RESET-based GTID baseline, semi-sync plugin branching) and
  especially the **manual ProxySQL→MySQL** wiring follow MySQL/ProxySQL docs but
  were **not** run against real containers. A live deploy should confirm
  replication forms, secondaries go `super_read_only`, and ProxySQL routes
  reads/writes correctly (caching_sha2 over a non-TLS ProxySQL→backend link is the
  most likely thing to need a tweak).

---

## 12. InnoDB / Group Replication frame

An **InnoDB / Group Replication** frame: Percona Server nodes (installed from a
**PDPS repository**) forming a single-primary MySQL Group Replication group —
either **InnoDB Cluster** (MySQL-Shell-managed) or raw **Group Replication**. MySQL
Router is installed on **each member** (default on), so the cluster is
self-contained and exposes **no canvas association endpoints** (the router is the
proxy). Default = 3 members.

### `make versions`: PDPS repositories
`images/versions.sh` runs `percona-release | grep -oiE 'pdps[a-z0-9._-]*'` in a
built image and writes a top-level **`pdps:`** list of repo names (e.g. `pdps-80-lts`,
`pdps-84-lts`, `pdps-8x-innovation`). `versions.go` parses it →
**`GET /api/catalog/pdps`** → `stackApi.pdpsCatalog()`. The chosen repo (passed to
`percona-release enable <repo>`) determines the Percona Server major/minor — there
is no separate version picker.

### Data model
`designFrame` `Type=="innodb"` + `pdpsRepo`, `replMode` (`innodbcluster` |
`groupreplication`), `mysqlRouter bool` (default true); reuses `os`/`osVersion`/
`arch` (base image), `rootPassword`, `pmmNodeId`, `useProxy`, `generateCert`/
`certTtl*`. Members: `designNode` `Type=="innodb"` + `FrameID` + export (no role —
GR auto-elects the primary).

### Provisioning — `app/innodb.go`
`provisionInnoDBFrame`: in parallel `innodbPrepareNode` creates each container,
installs `percona-server-server` + `percona-mysql-router` (+ `percona-mysql-shell`
for InnoDB Cluster) from the PDPS repo, writes `my.cnf` (GTID + GR settings for raw
GR mode; base only for InnoDB Cluster — Shell configures GR), starts mysqld, sets
root pw (reusing the `mysql.go` helpers + `validate_password` relax + `/root/.my.cnf`
+ rsyslog), creates the GR **recovery user** (not binlogged), and clears GTID state
(`RESET …`). Then:
- **Group Replication** mode: bootstrap on member 0 (`group_replication_bootstrap_group`
  + `START GROUP_REPLICATION`, wait `ONLINE` via `performance_schema.replication_group_members`),
  create app/monitor/cluster users (replicate via GR), then `START GROUP_REPLICATION`
  on the rest.
- **InnoDB Cluster** mode: MySQL Shell `dba.createCluster()` on member 0 + `addInstance`
  (clone recovery) for the rest, connecting as the `cluster` user.

A unique `group_replication_group_name` UUID is generated per frame (stable across
redeploys). **MySQL Router** (Phase 3) is installed on each member: InnoDB-Cluster
mode → `mysqlrouter --bootstrap` against the cluster metadata; raw GR → a static
`mysqlrouter.conf` routing to the members (RW first-available 6446 / RO round-robin
6447 — **not** primary-aware). Router ports are the host-export target. TLS/PMM/proxy
reuse the PXC/MySQL helpers. Deploy dispatch + frames loop + `refreshPublishedPorts`
+ validation (≥1 member, image, odd/≥3 quorum warnings, unique name) handle `innodb`.

### Frontend
`NODE_TYPES.innodb` (cyan, DB-cylinder icon) + **"InnoDB / Group Replication"**
toolbar button; `addInnoDBCluster` (3 members `innodbNN`); type-aware frame render
**without `PortHandles`** (`endpointKind` returns null and `hitPort` excludes it, so
it can't be linked to a ProxySQL). **`InnoDBFrameForm`** (image OS/version/arch +
**PDPS repo** picker + replication-mode select + root pw + PMM/proxy/cert + **Enable
MySQL Router** default on) and **`InnoDBMemberForm`** (router host-port export). A
running member shows **`InnoDBManager`** (Overview incl. group name / bootstrap /
router, Access showing the router RW/RO host ports, Credentials).

### Verification performed
- `go build`/`vet`/`test`, `gofmt`, `bash -n images/versions.sh`, and the web build pass.
- **Caveat — not validated on a live deployment.** The PDPS `percona-release enable`,
  the `percona-mysql-router`/`percona-mysql-shell` package names, GR bootstrap/join,
  `dba.createCluster`/`addInstance`, and `mysqlrouter --bootstrap` follow the docs but
  were **not** run against real containers; a live deploy (and `make versions` to
  populate `pdps:`) is needed to confirm — the package names and the raw-GR static
  router config are the most likely spots to need a tweak.

## 13. Cross-cluster replication links (async / bidirectional)

A **replication link** is an association line drawn between two **cluster member
nodes** — a **PXC** member or a **Percona Server replication** member — that live in
**different** frames. It sets up MySQL channel-based replication between the two
clusters, configured as the **final phase of a deploy** (so the clusters are already
up and their `repl` users exist) and **reconciled on every redeploy**.

- **async** — directed `source → replica`; the arrow points at the replica, which
  pulls from the source over one channel.
- **bidirectional** — both nodes replicate from each other (a channel on each side);
  multi-writer and conflict-prone (a validation warning says so).

### Canvas / endpoints
Every PXC and Percona Server replication **member card** now exposes 4 hover-revealed
ports (the card is wrapped in a non-clipping `group` div so the ports sit outside its
rounded border; ProxySQL/InnoDB members stay portless). `rectOf`/`hitPort` resolve
member endpoints at the small member geometry. `endpointKind` gains **`replmember`**
for PXC/Percona-Server members — distinct from the frame-level `backend` ports that
still drive the ProxySQL association. `tryConnect` rejects same-frame pairs and (via
the existing one-edge-per-pair guard) a second link between the same two nodes; on a
valid drop it opens **`ReplicationLinkModal`** (Async A→B / Async B→A / Bidirectional).
A replication edge renders green + dashed with an arrowhead at the replica (and a
second arrowhead at the source for bidirectional), captioned "async/bidirectional
replication". Selecting it shows **`ReplicationLinkForm`** — switch async direction or
async↔bidirectional (the "modify" path; options anchored to a sorted node pair so the
active choice doesn't jump), or delete. Changes take effect on the next Deploy.

### Backend — `app/replication.go`
`designEdge.Type` carries `"async"`/`"bidir"` for these links. `replicationLinks(doc)`
expands edges into directed `source→replica` links (bidir → two). `reconcileReplication`
runs in a goroutine kicked off at the end of `handleDeployStack`: it waits for the
involved members to be running, then on each replica runs **`replChannelApply`** —
`CHANGE REPLICATION SOURCE TO … GET_SOURCE_PUBLIC_KEY=1, <pos> FOR CHANNEL
'xrepl_<source host>'; START REPLICA FOR CHANNEL …` (modern 8.0.23+/8.4-safe
keywords; **no RESET**, so each node keeps its own cluster's data). **GTID is not
required:** when **both** clusters have GTID on, `<pos>` is `SOURCE_AUTO_POSITION=1`
(the replica fetches the GTIDs it is missing); otherwise it falls back to binary-log
**file/position** — `reconcileReplication` reads the source's current coordinates
(`sourceBinlogPos`: `SHOW BINARY LOG STATUS` on 8.4 / `SHOW MASTER STATUS` on 8.0) and
sets `SOURCE_LOG_FILE`/`SOURCE_LOG_POS`, so only writes made after deploy replicate
(seed existing data first). To make a PXC node usable as an async source/replica
without GTID, **`pxcMyCnf` now enables `log_bin` (+ `log_replica_updates`)
unconditionally** (previously only under GTID). The shared `repl`/`REPL_PASSWORD` user
created by every cluster's bootstrap is used to auth to the source. Channels removed
from the canvas are torn down by **`replChannelPrune`** (`STOP REPLICA` + `RESET REPLICA
ALL FOR CHANNEL` for any `xrepl_*` channel not in the kept set), so a redeploy
reconciles to match the design. Progress is appended to the replica's deployment log
via `replLogln` (the node stays "running"; replication is annotated, not a separate
node). `validateStack` checks each link connects members in **different** clusters,
warns when GTID is off on a side (file/position — only post-deploy writes replicate),
warns on a **server-id collision** between the endpoints (`memberServerID`) and on
bidirectional multi-writer, and errors on a duplicate pair.

### Decisions
Apply timing **at deploy / reconcile-on-redeploy** (not a separate post-deploy action);
GTID **best-effort auto-position** (divergent pre-existing data is the operator's
concern). Member labels are unique stack-wide so same-type links (PXC↔PXC, PS↔PS) get
distinct server-ids; only a mixed PXC↔PS pair can collide (validation warns).

### Verification performed
- `go build`/`vet`/`test`, `gofmt`, and the web build pass.
- **Caveat — not validated on a live deployment.** The cross-cluster `CHANGE
  REPLICATION SOURCE … FOR CHANNEL` flow (both GTID auto-position and file/position),
  GTID consistency between two independently-bootstrapped clusters, PXC as an async
  source/replica (now that `log_bin` is always on; Galera applies the stream
  cluster-wide), and the `caching_sha2`-over-TCP repl auth are **build-verified only**
  and need a live deploy to confirm.

## 14. InnoDB / GR live-deploy fixes (datadir init + MySQL Shell)

The §12 InnoDB / Group Replication frame was **build-verified only**; the first
live deploys failed in several ways. This section is the result of debugging
against real containers until **both modes deploy green** (single member: member
`ONLINE`, MySQL Router up; InnoDB Cluster `cluster.status()` = OK). All fixes are
in `app/innodb.go`.

### Datadir initialization (`innodbBaseScript`) — both modes
**Symptom.** mysqld aborted on first start with `Table 'mysql.user' doesn't exist`
(also `mysql.plugin` / `mysql.component`) — the datadir was never initialized, so
the node sat in provisioning forever.

**Cause.** In `groupreplication` mode `innodbMyCnf` writes the full GR block
(`plugin_load_add=group_replication.so` + `group_replication_*`) into
`/etc/my.cnf` **before the first start**, and the package's first-start
auto-initialize loads that plugin and aborts, leaving an empty datadir.

**Fix.** `innodbBaseScript` initializes the datadir explicitly before starting the
service, guarded on `[ ! -d /var/lib/mysql/mysql ]` (redeploys keep their data).
Getting this right took three follow-on corrections, each found by reading the
real error:
1. **GR-free init config** — `mysqld --defaults-file=/tmp/mysql-init.cnf
   --initialize-insecure` with a minimal config so init can't load the GR plugin;
   the later normal `systemctl start` reads the full `my.cnf` with system tables
   present. `--initialize-insecure` leaves `root@localhost` password-less, handled
   by the existing `mysqlSetRootPW` else-branch.
2. **Error-log ownership** — the script deletes the package's `/var/log/mysqld.log`,
   but `/var/log` is root-owned so the dropped-privilege (`user=mysql`) `mysqld
   --initialize` can't recreate it (`Could not open file … Permission denied`,
   which cascades to a misleading "data directory unusable"). Recreate it owned by
   mysql first: `install -m 0640 -o mysql -g mysql /dev/null "$LOGERR"`.
3. **Empty datadir** — `mysqld --initialize` refuses a non-empty datadir, and
   `rm -rf /var/lib/mysql/*` misses dotfiles; use `find /var/lib/mysql -mindepth 1
   -delete`. Also `install -d -o mysql /var/run/mysqld` for the pid file.

A `say_err` helper greps the real `[ERROR]` line and prints it **last**, because
`runStep` truncates captured output to the final 160 chars (otherwise all that
shows is mysqld's `Shutdown complete`).

### InnoDB Cluster mode (`innodbShellClusterScript`) — MySQL Shell
Three distinct problems, in order of discovery:

1. **`configureInstance` hang.** Run without `interactive:false`, MySQL Shell
   prompts `perform changes? [y/n]` to set
   `binlog_transaction_dependency_tracking=WRITESET` and blocks forever on the
   no-TTY exec. **`{interactive:false}` makes it auto-apply** the fix (verified:
   exits 0, variable becomes `WRITESET`). Required on every `configureInstance`.
2. **`createCluster` SEGFAULT.** MySQL Shell **8.0.46 segfaults** in
   `createCluster`'s "adopt existing replication group" path — i.e. when a prior
   failed attempt left Group Replication running with stale/invalid metadata. On a
   clean, configured instance `createCluster` succeeds. **Fix:** before creating,
   force a clean slate (`SET GLOBAL super_read_only=OFF; STOP GROUP_REPLICATION;
   DROP SCHEMA IF EXISTS mysql_innodb_cluster_metadata; RESET REPLICA ALL FOR
   CHANNEL 'group_replication_recovery'`, all error-tolerant) so `createCluster`
   takes the working "new group" path. A `getCluster` probe first reuses an
   existing cluster on redeploy (so a healthy cluster is never torn down).
3. **Invisible errors + hangs.** All Shell calls go through `sh_run TIMEOUT JS`,
   which bounds each call with `timeout` (the deploy goroutine uses
   `context.Background()`, so an unbounded `mysqlsh` hangs forever) and, on
   failure, greps the real `ERROR`/`Dba.`/`Cluster.` line and prints it last to
   beat the 160-char truncation. `addInstance` (multi-member, clone recovery) is
   wrapped in a `try/catch` that ignores "already a member".

### Multi-member fixes (3-node), found by live test
Single-member worked but 3-node deploys failed, one bug per mode:

1. **InnoDB Cluster — cluster admin user missing on joiners.**
   `Dba.configureInstance: Access denied for user 'cluster'@'…' (1045)`. The
   `cluster` admin account was created only on the primary (in
   `innodbShellClusterScript`), but `configureInstance`/`addInstance` connect to
   each joiner **as `cluster@joiner` before it is cloned**, so the account must
   already exist there. **Fix:** create the cluster admin user on **every** member
   in `innodbBaseScript` (`SET sql_log_bin=0; CREATE USER … GRANT ALL … WITH GRANT
   OPTION`), passing `CLUSTER_USER`/`CLUSTER_PW` to that step.
2. **Group Replication — recovery auth over non-TLS.** A joiner's distributed
   recovery connects to the donor as `repl` (caching_sha2_password) and fails with
   `Authentication requires secure connection` (`MY-002061`): without TLS,
   caching_sha2 needs the server's public key. **Fix:** add
   `group_replication_recovery_get_public_key=ON` to the GR `my.cnf` block
   (`innodbMyCnf`, `groupreplication` only — InnoDB Cluster mode uses Shell-managed
   SSL recovery and isn't affected).

### Squid proxy reliability (`intranet.go`, "Configure Squid")
Package installs through the Intranet Squid proxy (`useProxy`) failed with "All
mirrors were tried" — Squid tried IPv6/AAAA first in an IPv4-only environment.
Added `dns_v4_first on` to `/etc/squid/squid.conf` (idempotent). Single-member
installs through the proxy then succeed; concurrent 3-node installs can still
strain one proxy, so the four-way live test below used direct egress
(`useProxy:false`) to isolate cluster behavior.

### Frontend
The member sub-label is now **"Cluster member"** for InnoDB Cluster nodes and
stays **"GR member"** for raw Group Replication (`StackDesigner.jsx`, keyed on
`frame.replMode`).

### Verification performed (live, all four combinations green)
Driven through the running app (`POST /api/stacks/{id}/deploy`) against real
OracleLinux-9 systemd containers:
- **1-node innodbcluster** → `cluster.status()` `OK`, member `ONLINE`/`R/W`, Router up.
- **1-node groupreplication** → member `ONLINE`/`PRIMARY`, Router up.
- **3-node innodbcluster** → status `OK`: `innodb01` PRIMARY + `innodb02`/`innodb03`
  SECONDARY, all `ONLINE` (clone recovery).
- **3-node groupreplication** → `innodb01` PRIMARY + two SECONDARY, all `ONLINE`
  (incremental recovery).

## 15. PS MongoDB Sharded Cluster frame

A **PS MongoDB Sharded Cluster** frame: a Percona Server for MongoDB sharded
cluster, always **1 `mongos` router** (the "mongosh" node — a query router with the
`mongosh` shell) + **3 shards** + a **config-server replica set**, in one of two
**setups** chosen in the frame form before deploy (locked after):
- **standard** — 3 shards × **3-node** replica set (9 `mongod`) + a **3-node**
  config-server replica set (CSRS) + mongos = **13 nodes** (HA).
- **minimum** — 3 **single-node** shards + **1** config server + mongos =
  **5 nodes** (smallest working sharded cluster).

Either way the member set is fixed (no add/remove). Node properties mirror the PXC
frame **minus any replication configuration** (the sharded layout is not
user-editable). Internal auth uses a shared **keyFile** (the same random bytes on
every member); apps connect through the `mongos` router.

### `make versions`: PS MongoDB catalog
`images/versions.sh` probes each built image with `percona-release setup
psmdb-60|70|80` then `repoquery`/`madison percona-server-mongodb-server`, fenced with
`@@PSMDB60@@`/`@@PSMDB70@@`/`@@PSMDB80@@` and filtered to `^6\.0\.`/`^7\.0\.`/`^8\.0\.`.
The writer's generalized `emit_series` (variadic key/list pairs) emits a
**`percona_server_mongodb:`** per-image major-series map (`6.0`/`7.0`/`8.0` → minor
lists). `versions.go` `loadPSMDBCatalog()` reuses the generic `loadImageCatalog`
→ **`GET /api/catalog/psmdb`** → `stackApi.psmdbCatalog()`. `psmdbRepo(major)`
(in `mongodb.go`) maps `6.0→psmdb-60`, `7.0→psmdb-70`, else `psmdb-80`.

### Data model
`designFrame` `Type=="psmdb"` + `psmdbMajor`/`psmdbVersion` + `psmdbSetup`
(`"standard"|"minimum"`); reuses `os`/`osVersion`/`arch`, `rootPassword` (the MongoDB
**admin** password), `pmmNodeId`, `useProxy`, `generateCert`/`certTtl*`. **No**
gtid/replMode. Members: `designNode` `Type=="psmdb"` + `FrameID` + `Role`
(`"shard"|"config"|"mongos"`) + `Shard int` (shard index for shard members) + export
(only meaningful on the `mongos` node).

### Provisioning — `app/mongodb.go`
The provisioner is **count-agnostic** — it builds each replica set from whatever
members are present, so the standard and minimum setups share one code path (a
1-node config/shard RS is just `rs.initiate` with a single member).
`provisionMongoDBFrame` partitions members by role, reuses the admin password +
keyFile across redeploys (or generates them), and records each member's profile
(`mongoConfig`) + `mongoSecrets` (`adminUser`/`adminPassword`/`keyFile` — keyFile
never surfaced). A goroutine then:
- **Phase 1 (parallel `mongoPrepareNode`):** create the container (the `mongos` node
  publishes 27017 when export is on); install `percona-release setup psmdb-NN` +
  `percona-server-mongodb-server`/`-tools` (shard/config) or
  `percona-server-mongodb-mongos` + `percona-mongodb-mongosh` (mongos); write the
  shared `/etc/mongo.keyFile` (0400, owned `mongod`); write `mongod.conf`
  (`replSetName`, `sharding.clusterRole=configsvr|shardsvr`, `bindIpAll`, keyFile) and
  start `mongod` (config + shard nodes; the `mongos` node only preps dirs).
- **Phase 2:** `rs.initiate` the config RS (`cfg`) and each shard RS (`rs0/rs1/rs2`),
  waiting for a PRIMARY.
- **Phase 3:** create the cluster **admin** user (root role) via the localhost
  exception on the config-RS primary.
- **Phase 4:** write `mongos.conf` (`sharding.configDB=cfg/host1,2,3`, keyFile), start
  `mongos` via a custom **`mongos.service`** systemd unit (PSMDB ships only
  `mongod.service`), then `sh.addShard("rsN/host1:27017,host2,host3")` for each shard.
- **Phase 5:** TLS (Intranet CA) / PMM register / finalize. Deploy dispatch
  (`intranet.go` frame switch + per-node `case`) and validation handle `psmdb`.

### Validation
`validateStack` `case psmdb`: image exists; member set intact **per setup**
(standard → 3-node CSRS + 3 shards × 3-node RS; minimum → 1 config server + 3
single-node shards; both → exactly 1 mongos); unique cluster name; `mongos`
host-port export feeds the shared `exportReq` conflict check.

### Frontend
`NODE_TYPES.psmdb` (green, DB-cylinder icon) + **"PS MongoDB Sharded Cluster"**
toolbar button; **`addMongoDBCluster(setup)`** (default `standard`) builds the members
via **`psmdbMembers(fid, setup)`** (`mongos` + `cfgN` config RS + `sNrM` shard RS, with
RS size 3/config 3 for standard or RS size 1/config 1 for minimum). The frame-form
**Setup** select calls **`rebuildMongoCluster(frameId, setup)`** (pre-deploy only) to
swap the whole member set. A custom **`layoutPSMDBFrame`** (grouped grid — `mongos` +
config RS on the top row, each shard a column; columns/rows sized to the member count)
replaces the single-row `layoutFrame` via `relayoutFrame`. Every add/remove path is
gated for `psmdb`: no frame +/- buttons, no "Delete node" in the context menu /
member form, Delete-key/`deleteNode` no-op on members (the **frame** is still
deletable = delete the whole cluster). Member sub-labels read "mongos router" /
"config server" / "shard N member". **`MongoDBFrameForm`** (Setup select + catalog
OS/version/arch + PS MongoDB major/minor, admin password, PMM/proxy/cert — **no
replication options**) and **`MongoDBMemberForm`** (read-only role; 27017 host-export
only on the `mongos` node). A running member shows **`MongoDBManager`** (Overview incl.
role/RS/shard/configDB, Access showing the `mongosh` connect string through the router,
Credentials admin user/password).

### Verification performed (live)
- `make versions` populates `percona_server_mongodb:` (OL8/9 `6.0`/`7.0`/`8.0`
  per-image minor lists; OL10 empty — no EL10 packages yet); `go build`/`gofmt`,
  `bash -n images/versions.sh`, and the web build pass.
- **Live deploy — standard (13 nodes)** on OracleLinux-9 systemd containers
  (`useProxy:false`): all `running`; `sh.status()` shows **3 shards** added; the config
  RS + each shard RS report **1 PRIMARY + 2 SECONDARY**; authenticated admin via
  `mongos` works.
- **Live deploy — minimum (5 nodes):** all `running`; 3 single-node shards
  (`rs0`/`rs1`/`rs2`) registered; an authenticated write+read through `mongos` succeeds.
- The exact designs `addMongoDBCluster()` produces for both setups pass `validate`
  with no issues.

## 16. PS MongoDB replica set (PSM RS) + standalone (PSM)

Two more Percona Server for MongoDB shapes that reuse the `mongodb.go` building
blocks:
- **PSM RS frame** (`Type=="psmrs"`): a single MongoDB **replica set** — N `mongod`
  members (default 3, **resizable 1–9** via the frame +/− buttons) with a shared
  keyFile for internal auth, one `rs.initiate` over all members, and an `admin`
  (root) user on the elected primary. No sharding, config servers or mongos.
- **PSM standalone node** (`Type=="psm"`): a single `mongod` with
  `security.authorization: enabled` (no replica set, **no keyFile**), an `admin`
  user created via the localhost exception. A free node (like the standalone
  Percona Server `ps`).

Node properties for both mirror the PXC frame: catalog OS/version/arch + PS MongoDB
major/minor, admin password, PMM, Intranet proxy, TLS cert, host-port export.

### Backend — `app/mongodb.go`
`mongodConfYAML(replSet, clusterRole, useKeyFile)` was generalized: it omits the
`replication` block when `replSet==""` (standalone), omits `sharding` when
`clusterRole==""`, and emits `authorization` only (no `keyFile`) when
`useKeyFile==false`. `mongoPrepareNode` now derives the cluster role from `n.Role`
(`config`→configsvr, `shard`→shardsvr, else none), writes the keyFile only when
`sec.KeyFile!=""`, publishes 27017 for **any** exported node (not just mongos), and
records the auto-assigned host port into `mongoConfig.ExportPort` (mongos also keeps
`MongosPort`). Two new provisioners:
- **`provisionMongoRSFrame`** — parallel prepare (role `member`, replSet =
  `sanitizeName(frame.Label)`, keyFile), `rs.initiate` all members, create admin on
  member 0, finalize.
- **`provisionMongoStandalone`** — a synthetic frame from the node; prepare (role
  `standalone`, no replSet, no keyFile, authorization on), create admin via the
  localhost exception, finalize.

### Data model / dispatch / validation / ports — `app/intranet.go`
`designNode` gains `psmdbMajor`/`psmdbVersion` (for the `psm` node). Deploy dispatch
adds `psm` (node) + `psmrs` (frame, member gate). `validateStack`: `psm` joins the
`ps` node case (image + export conflict); a new `psmrs` block checks 1–9 members,
unique name, image, and an odd-count warning. `refreshPublishedPorts` adds a
`psmdb`/`psmrs`/`psm` case reading `27017/tcp` into `ExportPort` (+ `MongosPort` for
mongos).

### Frontend — `app/web/src/pages/StackDesigner.jsx`
`NODE_TYPES.psmrs` (frame member) + `NODE_TYPES.psm` (free node, with
osOptions/defaults) + `FRAME_COLORS.psmrs`; `frameVersionLabel` psmrs branch;
`nodeOSLabel` includes `psm`. **`addMongoRSCluster`** builds a 3-member frame;
`addFrameMember`/`removePXCNode` resize it within 1–9 (`newPSMRSMember`). Toolbar
buttons **"PSM Replica Set"** and **"PSM"**. A shared **`useMongoCatalog`** hook +
**`MongoCatalogFields`** component drive the OS/version/arch + PS MongoDB
major/minor selects for both **`PSMRSFrameForm`** (admin pw, PMM/proxy/cert, quorum
guidance) and **`PSMStandaloneForm`** (same + host export); **`PSMRSMemberForm`** is
the per-member export form. Running nodes show the (generalized) **`MongoDBManager`**
— `roleText` handles `member`/`standalone`, and the Access tab shows a direct
`mongosh` connect string (host port when exported, else in-cluster) for non-sharded
roles.

### Verification performed (live)
- `go build`/`gofmt`/`go vet` and the web build pass.
- **Live deploy** (one stack, `useProxy:false`): a 3-node PSM replica set + a PSM
  standalone (export on). All `running`; the replica set `rs.status()` shows
  **`psmrs01` PRIMARY + `psmrs02`/`psmrs03` SECONDARY** with an authenticated
  write+read; the standalone reports **`NoReplicationEnabled`** (genuinely
  standalone), enforces auth, and accepts an authenticated write+read; its 27017 is
  published to the host.
- The exact designs the frontend builds for both pass `validate` with no issues.

## 17. PMM3 monitoring for the MongoDB node types

The MongoDB shapes (sharded `psmdb`, replica set `psmrs`, standalone `psm`) now join
**PMM3** the same way the SQL node types do, following the official guide
(`.../install-pmm-client/connect-database/mongodb.html`).

- **pmm-client is installed on every mongo node unconditionally** at deploy
  (`mongoPrepareNode` runs the shared `pxcInstallPMMClient{RHEL,Debian}` after the
  PSMDB packages), so monitoring can be turned on later without a reinstall — even
  the `mongos` node gets it.
- **Registration** (only when a PMM node is selected) happens in each provisioner's
  finalize, gated on **`mongoWaitPMM`** (bounded wait — the PMM server is heavy and
  usually comes up after the DB nodes):
  - **`mongoEnsurePMMUser`** creates the `pmmMonitor` role + `pmm` user per the docs
    (`pmmMonitor` + `read@local` + `clusterMonitor`, plus `directShardOperations` on
    8.0). It authenticates as the cluster admin; on a sharded **shard** (no admin
    user) it first creates the admin via the localhost exception — which only permits
    creating the *first user*, not roles — then authenticates to create the role+user.
    The user is created on each replica-set **primary** and replicates to the set.
  - **`mongoRegisterPMM`** runs `pmm-admin config --force --server-insecure-tls
    --server-url=https://<user>:<pass>@<pmm-fqdn>:8443` then `pmm-admin add mongodb
    --username=pmm --password=… --host=127.0.0.1 --port=27017 [--cluster=<rs/cluster>]
    --enable-all-collectors <node>` on every mongod, plus the `mongos`. The
    `--cluster` name is the replica-set name (`psmrs`) or the sharded-cluster label
    (`psmdb`); standalone nodes omit it.
- **Topology specifics:** for the sharded cluster the `pmm` user goes on the config
  RS (admin auth) and on each shard RS (localhost-exception path); `mongos`
  authenticates the cluster-wide user via the config servers. The `pmm` user/password
  live in `mongoSecrets` (`pmmUser`/`pmmPassword`), stable across redeploys.

### Verification performed (live)
- `go build`/`gofmt`/`go vet` and the web build pass.
- **Live deploy** with an Intranet + PMM node + a 3-node PSM replica set + a PSM
  standalone, all `Monitored by` the PMM node: each mongo node installs pmm-client;
  `pmm-admin list` on the nodes shows their `mongodb` + `mongodb_exporter` services
  `Running`, and the PMM server inventory lists the MongoDB services.

## 18. Dock the Deployment console under Properties (right column)

The Deployment console (`DeploymentConsole` in `app/web/src/pages/StackDesigner.jsx`)
previously docked as a **full-width bar pinned to the viewport bottom**
(`position: fixed; left:0; right:0; bottom:0`), overlapping the canvas and the
Properties panel. Docked is the **default** layout (`loadDeployLayout` →
`docked: true`), so this was the normal experience.

Now, when docked, the console sits **at the bottom of the rightmost column, under
the Properties panel**, sharing that column's width.

- **`StackProperties`** hosts the console. It takes three new props —
  `deployOpen` (`deployPanel === 'open'`), `deployments`, and `onDeployMinimize`
  (`() => setDeployPanel('min')`) — passed from the page (the old standalone
  `{deployPanel === 'open' && <DeploymentConsole/>}` render in the page body was
  removed; the minimized-button portal stays).
  - The docked branch became a **flex column** (`relative flex shrink-0 flex-col
    gap-4`, fixed `width`): the Properties card is `min-h-0 flex-1 overflow-auto`
    (so it scrolls and yields space), and the console renders as the in-flow child
    below it (`<DeploymentConsole … inline columnWidth={width} />`).
  - The detached branch (Properties floating) still portals the Properties window,
    and additionally renders the docked console as a fallback pinned to the
    right-column bottom (`<DeploymentConsole … columnWidth={width} />`, no `inline`).
- **`DeploymentConsole`** gained `inline` + `columnWidth` props and three layout
  modes (was: detached-float vs. fixed full-width bottom):
  - **detached** (`!layout.docked`) → fixed floating panel, portal to `<body>`
    (unchanged).
  - **docked + `inline`** → in-flow flex child (`height: layout.height`,
    `shrink-0 overflow-hidden rounded-xl`); **returned directly, not portaled**, so
    the Properties column positions it.
  - **docked + not `inline`** (Properties detached) → fixed `right:0; bottom:0;
    width: columnWidth; height: layout.height`, portaled.
  - Return rule: `inline && !detached ? node : createPortal(node, document.body)`.
  - The top **height resize handle** (`kind: 'height'`, `d.y0 - e.clientY`) is
    unchanged and works for the in-column panel (dragging up grows it; Properties
    shrinks via `flex-1`).

The Dock/Detach (`Icon.Frame`) and Minimize (`—`) buttons and all per-node
progress rendering are unchanged.

### Verification performed
- `npm run build` (Vite) passes.

## 19. Rename MongoDB + InnoDB entities to PSMDB / InnoDB Cluster

Display-only rename of four creatable entities in
`app/web/src/pages/StackDesigner.jsx` to standardize the abbreviation (PSM →
**PSMDB**, "Percona Server for MongoDB"). **No internal type slugs changed** —
`innodb`, `psmdb`, `psmrs`, `psm` node/frame `type`s, hostnames, and persisted
designs are untouched, so this is purely cosmetic.

| Old name | New name |
| --- | --- |
| `InnoDB / Group Replication` | `InnoDB Cluster / GR` |
| `PS MongoDB Sharded Cluster` | `PSMDB Sharded Cluster` |
| `PSM Replica Set` / `PS MongoDB Replica Set` | `PSMDB RS` |
| `PSM` / `PS MongoDB (standalone)` | `PSMDB` / `PSMDB (standalone)` |

Touched, per entity:
- **Toolbar buttons** (the "+ …" add buttons) for all four.
- **`NODE_TYPES` short labels** (shown on node cards and the read-only "Type"
  field — `def.label`, display-only since `nextLabel` derives hostnames from
  `def.slug`, not the label): `innodb` `'InnoDB / GR' → 'InnoDB Cluster / GR'`,
  `psmrs` `'PSM RS' → 'PSMDB RS'`, `psm` `'PSM' → 'PSMDB'`. The `psmdb` member
  label stays `'PS MongoDB'` (the sharded-cluster *frame* is what's renamed, and
  it avoids colliding with the standalone `PSMDB`).
- **Property-panel / frame-form headers**: `InnoDBFrameForm`, `MongoDBFrameForm`,
  `PSMRSFrameForm`, `PSMStandaloneForm`.
- **Code comments** referencing the entity names, for consistency.

Product-name references in sub-text and field labels (e.g. "PS MongoDB member",
"PS MongoDB major") were intentionally left — those name the upstream product,
not the renamed entity.

### Verification performed
- `npm run build` (Vite) passes.

## 20. SeaweedFS node (S3 object storage / backup target)

A **SeaweedFS** node (`Type=="seaweedfs"`): an **S3-compatible object store** used
as a backup target for the database nodes (xtrabackup/xbcloud, Percona Backup for
MongoDB, pgBackRest). Like the PMM node it runs a **ready-made image**
(`chrislusf/seaweedfs`, pulled at deploy — **not** a `make images` systemd image)
and runs unprivileged. It is a free node gated on the Intranet (so the DB nodes can
resolve its FQDN through the Intranet DNS). Properties: **AWS_ACCESS_KEY_ID**
(default `seaweedfs`), **AWS_SECRET_ACCESS_KEY** (generated if left empty), and a
required **bucket name**; **AWS_DEFAULT_REGION** is fixed at `us-east-1` (SeaweedFS
ignores the region but S3 clients require one). After deploy the node panel shows
the **endpoint URL** and copy-paste backup snippets.

### Data model — `app/intranet.go`
`designNode` gains SeaweedFS fields (ignored by other types): `accessKey`,
`secretKey`, `bucket`. Deploy dispatch adds `case "seaweedfs"` (per-node loop —
free node, `FrameID==""`). `validateStack` adds a `seaweedfs` case that requires a
valid bucket name (`validBucketName`: 3–63 chars, lowercase letters/digits/dots/
hyphens, start/end alphanumeric, no `..`/`.-`/`-.`); it does **not** check an image
(the image is pulled, like PMM). `refreshPublishedPorts` adds a `seaweedfs` case
reading `8080/tcp` (the published web-UI port) into `seaweedConfig.WebPort`.

### Provisioning — `app/seaweedfs.go`
`provisionSeaweedFS(st, n, doc)` records the deployment then runs an async, stepwise
goroutine (same progress/percent/log model as PMM):
1. **Pull** `chrislusf/seaweedfs:latest` (`seaweedDefaultTag`) if absent.
2. **Wait for the Intranet** (`waitIntranet`) — the DB nodes resolve seaweedfs's
   FQDN through it; the container is created with `DNS=[intranetIP]`.
3. **Create** the container with `Cmd` =
   `["server", "-dir=/data", "-s3", "-s3.config=/etc/seaweedfs/s3.json"]`
   (all-in-one master + volume + filer + S3 gateway: S3 on **8333**, volume web UI
   on **8080**, filer 8888, master 9333). Only the **8080 web interface** is
   published to the host (`PublishPorts: [seaweedWebPort]`); the S3 API stays on its
   **8333** default and is reached **in-network** by the database nodes (it is not
   host-published). Then — **before start** — `PutArchive` the S3 identities config
   into `/etc/seaweedfs/s3.json` (a single identity with the access/secret key and
   `Admin` actions). A new **`ContainerSpec.Cmd`** field (in `docker.go`) carries the
   command; `seaweedTar` includes an explicit parent-dir entry so `/etc/seaweedfs` is
   created on extract.
4. **Start**, record the published host port for the web UI (`WebPort`, from
   `8080/tcp`).
5. **Create the bucket** via `weed shell` (`s3.bucket.create` + verify with
   `s3.bucket.list | grep`), run through **`runShStep`** (a `/bin/sh` variant of
   PMM's `runStep` — the alpine image has no bash) with 10 retries, which also
   serves as the readiness gate (weed shell only connects once master+filer are up).
6. **reconcileStackDNS** so the node gets an A record.

`seaweedConfig` (image/hostname/fqdn/alias/accessKey/bucket/region/`WebPort`/
`InternalEndpoint` = `http://<fqdn>:8333`) is the non-secret profile; `seaweedSecrets`
holds the secret key (reused across redeploys, else the user's value, else a 40-char
`genS3Secret`). Both are served by the existing `GET /api/stacks/{id}/nodes/{nid}`
(`handleGetNode`) — **no new routes**.

> **Port design (per request):** the host-published port is the SeaweedFS **web
> interface** (`http://<host>:<WebPort>/ui/index.html`, the volume-server status UI),
> **not** S3. The **S3 endpoint stays on `:8333`** and is used only in-network by the
> database nodes (`http://<fqdn>:8333`), which is what all the backup snippets target.
> `seaweedS3Port=8333` / `seaweedWebPort=8080` are constants in `seaweedfs.go`.

### Terminal — bash→sh fallback (`app/terminal.go`)
The root-console exec was hard-coded to `/bin/bash`; the alpine SeaweedFS image has
no bash. **First attempt** ran `sh -c 'exec bash 2>/dev/null || exec sh'` — which was
wrong: on a missing bash a failed `exec` makes the shell **exit (127)** before the
`|| exec sh` runs, so the terminal opened **blank/dead**. Even when sh did run,
busybox `sh` prints **no prompt** unless interactive. The exec now runs
`sh -c 'if command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi'`
— detect-then-exec (never `exec` a missing binary) with **`-i`** to force an
interactive shell that prints a prompt. OL9 still gets `bash -i`; alpine gets
`sh -i` (prompt `/data # `).

### Frontend
- **`NODE_TYPES.seaweedfs`** (teal, new **`Icon.Bucket`** glyph, `ports:false`,
  `osOptions:[{id:'seaweedfs',label:'chrislusf/seaweedfs'}]`, defaults
  `{accessKey:'seaweedfs', secretKey:'', bucket:''}`) + a **SeaweedFS** toolbar
  button (gated on Intranet). `nodeOSLabel`'s default branch already renders the
  image label; `wide` (panel widen) includes `seaweedfs`.
- **`SeaweedFSForm`** (undeployed): label, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  (placeholder "auto-generate if empty"), bucket (with live name validation), an
  AWS_DEFAULT_REGION=us-east-1 note; all credential/bucket fields lock once deployed.
- **`SeaweedFSManager.jsx`** (running): tabs **Overview** (FQDN/image/alias/bucket/
  region + an **Open web interface** link to `http://<host>:<WebPort>/ui/index.html`
  + delete), **Access** (the in-network **S3 endpoint** `internalEndpoint`
  (`:8333`, used by the DB nodes) + the **web interface** `http://<host>:<WebPort>`
  (`:8080`), access/secret key, region, bucket — each with a copy button), and
  **Backups** — copy-paste snippets built from the config/secrets for
  **xtrabackup → `xbcloud put`**, a **`my.cnf [xbcloud]`** section, **`xbcloud get`**
  (restore), **Percona Backup for MongoDB** (`pbm config --file`), and **pgBackRest**
  config. All use the in-stack endpoint and **path-style** addressing
  (`s3-bucket-lookup=path` / `forcePathStyle:true` / `repo1-s3-uri-style=path`) over
  plain HTTP, as SeaweedFS requires.

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt`, and the web build (`npm run build`) all pass.
- **Live (validated).** A first live deploy crash-looped: `weed`'s S3 server
  aborted on startup with `fail to read /etc/seaweedfs/s3.json: permission denied`
  — the recent `chrislusf/seaweedfs` image runs `weed` as a **non-root** user, so a
  root-owned `0600` config is unreadable to it. **Fix:** `seaweedTar` writes the
  config **world-readable (`0644`)** (the container is the trust boundary). After
  the fix, reproduced the exact provisioner steps against the real image (stage
  `s3.json` 0644 before start → `weed server -s3` stays up, no crash loop; the
  `weed shell s3.bucket.create`/`s3.bucket.list` script created the bucket and
  exited 0). An authenticated S3 round-trip on the published port: **PUT 200**,
  **GET 200** (payload echoed), and **wrong-secret → 403 SignatureDoesNotMatch** —
  confirming path-style addressing, the credentials, and that auth is enforced.

### Fix — config file permission (`0644`)
`seaweedTar` now stamps the staged `/etc/seaweedfs/s3.json` mode `0644` (was
`0600`). The image's non-root `weed` process must be able to read it; the
container is the security boundary, so a world-readable S3 config inside it is
fine.

### SeaweedFS S3 TLS (optional, Intranet-CA-signed)
The S3 endpoint can be served over **HTTPS**. The node gains a **`TLS`** field
(`designNode.TLS`); when set, `provisionSeaweedFS` appends
`-s3.cert.file=/etc/seaweedfs/tls/s3.crt -s3.key.file=/etc/seaweedfs/tls/s3.key` to
the `weed server` command and the `InternalEndpoint` scheme becomes `https://`. When
**`GenerateCert`** is also set the certificate is **signed by the Intranet CA** (so a
client that trusts it verifies the server); otherwise it is **self-signed**.

The SeaweedFS image ships **no `openssl`** (and `weed` runs non-root), so unlike the
systemd nodes (which shell out to in-container openssl) the certificate is signed
**in Go** — new **`app/certs.go`**: `signTLSCert(caCertPEM, caKeyPEM, cn, dnsNames,
ttl)` generates an RSA-2048 key and either signs against the parsed Intranet CA
(`parseCA` handles PKCS#8 or PKCS#1) or self-signs. The cert+key are staged before
start via **`seaweedTLSTar`** (world-readable `0644`, explicit parent-dir entries,
like `seaweedTar`). Reuses the Intranet-CA plumbing (`waitIntranetCAReady`,
`readContainerFile` for `/etc/pki/dbcanvas/ca.{crt,key}`) and the
`CertTTLValue`/`CertTTLUnit` fields (via `certTTL`).

`seaweedConfig` gains `TLS` + `GenerateCert`; the Manager's Overview shows the S3-TLS
mode and the Access note reflects HTTPS (and whether verification applies). The backup
snippets read `internalEndpoint`, so they switch to `https://` automatically.

**Verification:** `go test` covers `signTLSCert` (CA-signed cert chains to the CA with
the right SANs + cert/key pair; self-signed path; ~365-day default TTL). Live smoke
test: ran `chrislusf/seaweedfs` with `-s3.cert.file/-s3.key.file` + a staged cert —
the S3 API answers over **HTTPS** (GET → 403 auth-required), presents the cert with
the expected CN + DNS SAN, and **rejects plain HTTP** on the TLS port (400).

### Percona XtraBackup on Percona Server (standalone + replication)
SeaweedFS is a backup target, so the **Percona Server** node types now ship the
matching backup tool. PXC data nodes already installed Percona XtraBackup (§8 — for
SST); **`mysqlPrepareNode`** (used by both the standalone **Percona Server** `ps`
node and every **Percona Server Replication** `mysql` member) now installs it too,
right after `percona-server-server` (~45%), reusing the PXC helpers: `pxbProduct`/
`pxbPackage` map the **`PSMajor`** series to the percona-release product + package
(`8.0 → pxb80 / percona-xtrabackup-80`, `8.4 → pxb84lts / percona-xtrabackup-84`),
installed via `pxcInstallXtrabackup{RHEL,Debian}`. So an `xbcloud put` to the
SeaweedFS endpoint works out of the box on these nodes. (PXC already had it; the
MongoDB/PSMDB types use Percona Backup for MongoDB instead.)

### Follow-ups — publish the web UI (not S3) + fix the blank terminal
- **Publish 8080 (web UI), keep S3 on 8333.** Per request, the host-published port is
  now the **web interface** (volume-server status UI at `/ui/index.html` on container
  8080), while the **S3 API stays on 8333** for in-network use by the database nodes.
  `seaweedfs.go` reverted the `Cmd` to default ports (no `-s3.port`/`-volume.port`),
  publishes `seaweedWebPort` (8080), and records `WebPort`; `refreshPublishedPorts`
  reads `8080/tcp`; the manager's Access/Overview show the web link + the `:8333` S3
  endpoint. **Live-verified:** `curl http://host:<WebPort>/ui/index.html` → **HTTP
  200** ("SeaweedFS … Volume Server"); S3 still answers on 8333 in-container (403
  ListBuckets without auth = alive).
- **Blank terminal fixed** (see *Terminal* above) — detect-then-exec with `-i`.
  **Live-verified** against the alpine SeaweedFS container under a controlling PTY:
  the prompt `/data # ` renders immediately and after each command (`whoami` → root,
  arithmetic evaluated), where the old `exec bash || exec sh` exited 127 and showed
  nothing.

## 21. Patroni PostgreSQL cluster frame + HAProxy node + PPG catalog

A **Patroni PostgreSQL cluster** frame (`Type=="patroni"`) plus an **HAProxy** node
(`Type=="haproxy"`) bring PostgreSQL HA to the designer. Each Patroni member
co-locates three services installed at deploy on the **systemd OS images** (`make
images`): **PostgreSQL** (Percona Distribution for PostgreSQL), **Patroni** (the HA
template that runs PostgreSQL and elects a leader), and an **etcd** member (the DCS
Patroni stores cluster state in). The etcd members form one cluster across all nodes
(quorum → **3–7 nodes, odd recommended**); Patroni bootstraps PostgreSQL on the node
that wins the leader lock and clones the rest as streaming replicas. Options mirror
the PXC frame (catalog OS/version/arch, superuser password, PMM monitor, Squid
proxy, Intranet-CA TLS) **minus GTID**, **plus** an optional **pgBackRest → SeaweedFS
S3** backup/clone. An **HAProxy** node linked to the frame by a canvas association
line routes **writes → the current leader (:5000)** and **reads → replicas (:5001)**
via Patroni's REST health checks, with a **stats page (:7000)**.

### Part A — PPG version catalog (`images/versions.sh`, `app/versions.go`, `app/main.go`)
`rhel_probe`/`debian_probe` gain PostgreSQL probing: for majors **13–17**,
`percona-release setup ppg-NN` then enumerate **`percona-postgresql-NN`** (fenced
`@@PPG13@@`…`@@PPG17@@`, filtered `^NN\.`). The writer adds `emit_series
percona_postgresql "13" … "17"` so each image entry carries a `percona_postgresql:`
major-series map. `versions.go` adds `loadPPGCatalog() = loadImageCatalog("percona_postgresql")`
+ `handlePPGCatalog`, **and** a generic `loadImagesCatalog() = loadImageCatalog("")`
(every built image, **no** version map — for nodes that only need the OS matrix) +
`handleImagesCatalog`. Routes: **`GET /api/catalog/ppg`**, **`GET /api/catalog/images`**;
`stackApi.ppgCatalog()` / `imagesCatalog()`.

### Part B/C — Data model + dispatch (`app/intranet.go`)
`designFrame` gains patroni fields (reusing `OS`/`OSVersion`/`Arch`, `RootPassword`
= superuser pw, `PMMNodeID`, `UseProxy`, `GenerateCert`/`CertTTL`): **`PGMajor`**,
**`PGVersion`**, **`UsePgBackRest`**, **`SeaweedFSNodeID`**. Patroni members are
`Type=="patroni"` + `FrameID` + `ExportEnabled`/`ExportHostPort` (publish 5432); an
HAProxy node is a free `Type=="haproxy"` reusing `OS`/`OSVersion`/`Arch`,
`ExportEnabled`, `PMMNodeID`, `UseProxy`. Deploy dispatch adds `case "haproxy"`
(free-node loop) and `case "patroni"` → `memberType="patroni"` + `provisionPatroniFrame`.
**`patroniFrameForHAProxy(doc, haproxyNodeID)`** is a near-clone of
`backendFrameForProxySQL` — an undirected BFS over the edges to the nearest
`Type=="patroni"` frame.

### Part D — Patroni provisioning (`app/patroni.go`)
`provisionPatroniFrame(st, f, doc)` (modeled on `provisionPXCFrame`): credentials
**`pgSecrets`** (`postgres` superuser + `replicator`, reused across redeploys else
generated), then an async goroutine —
1. `waitIntranet`; when pgBackRest is on, `waitSeaweedRunning` (the S3 config/secret
   must be readable before writing `pgbackrest.conf`).
2. **Parallel `patroniPrepareNode`**: create the container (systemd image,
   `DNS=[intranetIP]`, publish 5432 when export on), install `percona-postgresql-NN`
   + `-contrib` + `percona-patroni` + `etcd` (+ `percona-pgbackrest`) + `pmm-client`,
   stage optional TLS into `/etc/patroni`, then write the **etcd** EnvironmentFile
   (`/etc/etcd/etcd.conf`; every node a member, `initial-cluster` = all peers),
   **`/etc/pgbackrest/pgbackrest.conf`** (S3 → SeaweedFS, `repo1-s3-uri-style=path`,
   stanza = sanitized cluster name) when enabled, and **`/etc/patroni/patroni.yml`**
   (scope = cluster, `etcd3.hosts` = all `:2379`, `restapi` `:8008`, bootstrap
   `initdb`/`pg_hba` scram, superuser+replication auth; when pgBackRest:
   `create_replica_methods:[pgbackrest, basebackup]` + `archive_command`).
3. **Start etcd** on all nodes (idempotent `new`/`existing` state) → `patroniWaitEtcd`
   (each `etcdctl endpoint health`).
4. **Start Patroni** (systemd drop-in pins `ExecStart=patroni /etc/patroni/patroni.yml`)
   → `patroniWaitCluster` polls each node's REST (`/leader` 200 = leader, `/health`
   200 = running) until one leader + all members are up; returns the leader's node id.
5. When pgBackRest: on the leader, `pgbackrest stanza-create` + initial **full backup**
   (`runuser -u postgres`).
6. PMM (`pmm-admin add postgresql`, best-effort) + record each node's role
   (leader/replica) → running.
Helpers: `patroniPrepareNode`, `patroniApplyCert` (CA staged like `pxcApplyCert`, into
`/etc/patroni`, postgres-owned), `patroniWaitEtcd`, `patroniWaitCluster`,
**`waitPatroniRunning`** (member FQDNs + creds, for HAProxy), `waitSeaweedRunning`,
`patroniRegisterPMM`, `patroniLeaderContainer`, config builders (`patroniEtcdConf`,
`patroniYAML`, `patroniPgBackRestConf`). Ports: PG **5432**, REST **8008**, etcd
client **2379** / peer **2380**.

### Part E — HAProxy provisioning (`app/haproxy.go`)
`provisionHAProxy(st, n, doc)` (modeled on `provisionProxySQLInstance`):
`waitIntranet` → `patroniFrameForHAProxy` (fail if unlinked) → `waitPatroniRunning`
(member FQDNs) → create container (publish **5000/5001/7000** when export on) →
install `haproxy` (distro pkg; Squid proxy when `UseProxy`) + `pmm-client` → write
**`/etc/haproxy/haproxy.cfg`**: a **write** front-end (`bind :5000`, `option httpchk
GET /primary` against each member `:8008` — only the leader returns 200), a **read**
front-end (`:5001`, round-robin, `GET /replica`), and a **stats** page (`:7000`) →
`haproxy -c` validate + start → optional PMM (`pmm-admin add haproxy`). `haproxyConfig`
records the linked cluster, member FQDNs, and published ports.

### Part F/G — Validation + lifecycle ports (`app/intranet.go`)
`validateStack`: a **patroni** node case isn't needed (members fall through to
`default`); a **patroni-frame** block enforces **3 ≤ members ≤ 7** (error), odd-count
(warning), unique cluster name, member 5432 export joins the shared `exportReq`
conflict check, and `UsePgBackRest` ⇒ `SeaweedFSNodeID` set **and** referencing a
`seaweedfs` node in the design. A **haproxy** node case checks the image exists, that
it links to a patroni frame (`patroniFrameForHAProxy`, error otherwise), and joins
the export conflict check. `refreshPublishedPorts` adds `patroni` (5432 →
`ExportPort`) and `haproxy` (5000/5001/7000 → `WritePort`/`ReadPort`/`StatsPort`).

### Part H — Routes (`app/main.go`)
`GET /api/catalog/ppg`, `GET /api/catalog/images`, and
**`POST /api/stacks/{id}/frames/{fid}/patroni/backup`** → `handlePatroniBackup`
(owner-scoped; finds the running leader via `patroniLeaderContainer` and runs an
on-demand `pgbackrest --type=full backup`).

### Part I — Frontend (`StackDesigner.jsx` + managers)
- **`NODE_TYPES.patroni`** (PG blue `#336791`, `Database` icon — member render only)
  and **`NODE_TYPES.haproxy`** (`ports:true`, reuses the `ProxySQL` icon, green,
  imagesCatalog OS). **`FRAME_COLORS.patroni`**; `frameVersionLabel` patroni branch;
  `nodeOSLabel`/`wide` include `haproxy`/`patroni`.
- **Toolbar**: **"Patroni Cluster"** → `addPatroniCluster` (3 members `patroniNN`,
  cluster `patroni-cluster-NN`) and **"HAProxy"** → `addNode('haproxy')`.
- **Member +/−**: `addFrameMember`/`removePXCNode` patroni branch — **min 3 / max 7**.
- **Association framework** (extended): `endpointKind` → patroni frame `'patroni'`,
  haproxy node `'haproxy'`; `hitPort` includes patroni frames; `tryConnect` adds
  **patroni (source) ↔ haproxy** → `createFlow(patroniFrame→haproxy, {singleOutgoing})`
  (HAProxy single-incoming via the dest guard). The patroni frame renders the shared
  `PortHandles`; patroni members don't (no replication links).
- **`PatroniFrameForm`** (`usePPGCatalog` cascade like the Mongo forms): OS/version/
  arch + PG major/minor, superuser password, **"Use pgBackRest (SeaweedFS S3)"** →
  a SeaweedFS-node `<select>`, PMM/proxy/cert, 3–7/odd quorum guidance. **`PatroniMemberForm`**:
  5432 host export. **`HAProxyForm`**: linked-cluster banner (BFS to the patroni frame,
  error until linked), imagesCatalog OS/version/arch, PMM/proxy, 5000/5001/7000 export.
- **`PatroniManager.jsx`** (running member): **Overview** (cluster/role/FQDN/PG
  version/etcd/pgBackRest/host 5432 + delete), **Credentials** (superuser +
  replication + psql URI), and a **Backup** tab (when pgBackRest) with a **Backup now**
  button → `patroniApi(id, fid).backup()`. **`HAProxyManager.jsx`**: **Overview**
  (linked cluster, write/read/stats host ports + a stats-page link) and **Access**
  (psql URIs/commands for the write + read ports). `stackApi` adds `ppgCatalog`,
  `imagesCatalog`, and `patroniApi(id, fid).backup()`.

### Verification performed
- `bash -n images/versions.sh`; `go build`/`go vet`/`go test`, `gofmt -l` (clean);
  web build (`npm run build`); app image rebuilt (`docker compose build`) and
  restarted — the new `/api/catalog/ppg` + `/api/catalog/images` routes respond.
- **Live-validated against the real Oracle Linux 9 image** (throwaway containers off
  `dbcanvas-systemd:oraclelinux-9-amd64`), which **corrected several wrong initial
  assumptions** (see below): the full **5-package install completes**
  (`percona-postgresql16-server` + `-contrib` + `percona-patroni` + `etcd` +
  `percona-pgbackrest`, with EPEL for `libssh2`), the binaries land
  (`/usr/bin/patroni`, `/usr/bin/etcd`, `/usr/pgsql-16/bin/postgres`, `pgbackrest`),
  and **`patroni --validate-config` accepts the generated config** (only DNS
  resolution of the etcd FQDNs fails in isolation — a schema pass).
- **Full multi-node deploy (3-node etcd quorum + leader election + HAProxy routing +
  pgBackRest round-trip): pending** — it runs under an authenticated UI session.
  Checklist: `patronictl list` 1 Leader + 2 streaming replicas; `etcdctl endpoint
  health` on all 3; HAProxy `:5000` → writable leader / `:5001` → read-only replica,
  follows a `switchover`; pgBackRest stanza + backup in the SeaweedFS bucket; **Backup
  now**; PMM shows the postgresql/haproxy services.

### Live-test corrections (applied)
Probing the OL9 image revealed the placeholder package/path assumptions were wrong;
the code now uses the **verified** names:
- **PostgreSQL packages (EL):** the server is **`percona-postgresqlNN-server`** (no
  hyphen) + `percona-postgresqlNN-contrib` — not `percona-postgresql-NN`. The
  version-probe package is **`percona-postgresqlNN`**, whose NVR carries an **epoch**
  (`percona-postgresql16-1:16.14-…`), so `versions.sh` strips the leading `N:`
  (`sed 's/^[0-9]+://'`) before the `^NN\.` filter. `pgServerPackages(os, major)` is
  OS-aware (Debian keeps the PGDG `percona-postgresql-NN`).
- **etcd uses a YAML config**, not an `ETCD_*` EnvironmentFile: the EL unit runs
  `etcd --config-file /etc/etcd/etcd.conf.yaml`. `patroniEtcdConf` emits YAML
  (`name:`/`initial-cluster:`/`initial-cluster-state:`…); the start script flips
  `initial-cluster-state` to `existing` on redeploy via `sed`.
- **Patroni config path:** the packaged unit reads
  `PATRONI_CONFIG_LOCATION=/etc/patroni/postgresql.yml` and runs as `User=postgres`,
  so DBCanvas writes **`/etc/patroni/postgresql.yml`** (no systemd drop-in needed).
- **pgBackRest needs `libssh2`**, carried only by **EPEL** on OL — the install enables
  `oracle-epel-release-el<major>` (`WITH_EPEL`/`EPELPKG`) before installing when
  pgBackRest is on.
- `runuser -u postgres` (not `sudo`, which the image lacks) runs the `pgbackrest`
  stanza-create + backup; `curl`/`openssl`/`python3` confirmed present.
- **Config-dir 404s:** Docker's copy API returns `(404)` when the destination
  directory is missing, and neither `percona-pgbackrest` (`/etc/pgbackrest`) nor
  `percona-patroni` (`/etc/patroni`) reliably ships its config dir. A
  `patroniConfigDirsScript` (`mkdir -p /etc/etcd /etc/patroni`) runs before the
  config writes, and `patroniPgBackRestDirsScript` (now including `/etc/pgbackrest`)
  runs **before** the `pgbackrest.conf` CopyFile. (Surfaced live as
  `write pgbackrest.conf: docker copy archive: (404)`.)
- **HAProxy startup resilience:** `default-server … init-addr last,libc,none` so
  `haproxy -c`/start succeeds even if a backend FQDN is momentarily unresolvable
  (the server starts disabled and is enabled once DNS resolves) instead of failing
  the whole config.
- **etcd multi-node bootstrap deadlock (critical):** etcd's unit is `Type=notify`
  and does **not** signal ready until the cluster reaches quorum. The first version
  started etcd per node with a *blocking* `systemctl restart` + an `is-active` gate,
  so node 1 hung forever waiting for peers that the sequential caller hadn't started
  yet (live symptom: node 1 etcd stuck `activating`, pre-voting, peers
  `…:2380 connection refused`). Fixed: `systemctl --no-block restart etcd` on every
  node (returns immediately, no `is-active` gate), and `patroniWaitEtcd` then polls
  all nodes' `etcdctl endpoint health` until quorum forms. Also: the member-dir
  heuristic that flipped `initial-cluster-state` to `existing` was wrong (etcd creates
  `member/` the instant it starts, so a stale partial bootstrap forced the join path);
  the start script now **`rm -rf /var/lib/etcd/member` and always bootstraps `new`**,
  matching the recreate-container-on-redeploy model. **Validated:** a real 3-node
  systemd-container test forms quorum (all members `started`, `endpoint health`
  healthy on all 3).

### Live-test — single-node runtime validated
Ran the full runtime in a **privileged systemd OL9 container** (same launch flags the
app uses: `--privileged --cgroupns=host -v /sys/fs/cgroup:rw --tmpfs /run`,
`/usr/sbin/init`): installed the packages, wrote the **exact** etcd YAML + Patroni
config the Go builders emit (single member on `127.0.0.1`), and started the services.
**Result:** `etcd` came up healthy (`etcdctl endpoint health`); **Patroni bootstrapped
PostgreSQL and became `Leader / running`** (`patronictl list`), and the REST role probe
returned `/leader → 200` (confirming `patroniRoleScript`). HAProxy's generated config
**passed `haproxy -c`** (with `init-addr` it validates even without DNS); `pgbackrest`
installs (EPEL) and runs as postgres with `/etc/pgbackrest` present. **Still pending:**
the multi-node election + replica streaming + HAProxy failover routing + the pgBackRest
S3 round-trip to a live SeaweedFS — these need the 3-node UI deploy.

## 22. Standalone PostgreSQL node (PG) + optional pgBackRest → SeaweedFS S3

A standalone **PostgreSQL** node (`Type=="pg"`): a single read/write PostgreSQL
instance (Percona Distribution for PostgreSQL) installed at deploy time on a
systemd OS image (`make images`). Its properties **mirror the standalone Percona
Server node** (§11 `ps`) — catalog OS/version/arch, PostgreSQL major/minor,
superuser password, PMM monitor, Intranet Squid proxy, Intranet-CA TLS, host-port
export — **plus** an optional **pgBackRest → SeaweedFS S3** backup, the same option
the Patroni cluster frame (§21) carries. Unlike the Patroni frame there is **no
Patroni/etcd and no replication**: PostgreSQL is bootstrapped directly from the
packaged systemd unit. It is a free node gated on the Intranet (DNS/CA/proxy), and
publishes PostgreSQL on **5432** to the host when export is on.

### Data model + dispatch + validation + ports — `app/intranet.go`
`designNode` gains PG-only fields (ignored by other types): `PGMajor`, `PGVersion`,
`UsePgBackRest`, `SeaweedFSNodeID` (it reuses `OS`/`OSVersion`/`Arch`, `RootPassword`
= the postgres superuser password, `PMMNodeID`, `UseProxy`, `GenerateCert`/`CertTTL*`,
`ExportEnabled`/`ExportHostPort`). Deploy dispatch adds `case "pg"` (per-node loop —
free node). `validateStack` adds a `pg` case: image exists (`make images`), 5432
export joins the shared host-port conflict check, and `UsePgBackRest` ⇒ a
`SeaweedFSNodeID` set **and** referencing a `seaweedfs` node in the design (else an
error — mirroring the patroni-frame rule). `refreshPublishedPorts` adds a `pg` case
reading `5432/tcp` into `pgConfig.ExportPort`.

### Provisioning — `app/pg.go`
`provisionPG(st, n, doc)` records the deployment (`pgConfig` non-secret profile +
`pgSecrets` reused from §21 — only the `postgres` superuser is used; the replication
fields stay empty), then runs an async, stepwise goroutine (same progress/percent/log
model as the other nodes):
1. `waitIntranet`; when pgBackRest is on, `waitSeaweedRunning` (§21) so the S3
   config/secret are readable before writing `pgbackrest.conf`.
2. **Create + start** the container (systemd image, `DNS=[intranetIP]`, publish 5432
   via `PublishMap` when export on), point its resolver at the Intranet.
3. **Install** the PostgreSQL packages (`pgServerPackages` — EL
   `percona-postgresqlNN-server` + `-contrib`, Debian `percona-postgresql-NN`) via
   `percona-release setup ppg-NN` (reusing `patroniInstallRHEL`/`Debian`), plus
   `percona-pgbackrest` (with EPEL for libssh2 on EL) when enabled, and **pmm-client**
   (always, so monitoring can be turned on later). `UseProxy` routes egress through
   the Intranet Squid first.
4. **Initialise the data dir** (`pgInitScript`, guarded on `PG_VERSION`): EL runs
   `initdb` directly as postgres into the packaged unit's data dir
   (`/var/lib/pgsql/NN/data`); Debian registers a cluster with `pg_createcluster NN main`.
5. When pgBackRest: create the config/runtime dirs (`patroniPgBackRestDirsScript`)
   and write `/etc/pgbackrest/pgbackrest.conf` (`patroniPgBackRestConf` — S3 →
   SeaweedFS, `repo1-s3-uri-style=path`, stanza = sanitized label) **before** start,
   so `archive-push` works once the stanza exists.
6. Optional **TLS** (`pgApplyCert`): the Intranet CA is staged and a server cert+key
   signed into the data dir (postgres-owned, TTL via openssl `-not_after`), referenced
   by `ssl_cert_file`/`ssl_key_file`/`ssl_ca_file`.
7. **Configure** (`pgConfigureScript`, OS-aware config dir via `pgConfDir` — the data
   dir on EL, `/etc/postgresql/NN/main` on Debian): append `listen_addresses='*'`,
   `port=5432`, `password_encryption=scram-sha-256`, the `host all all 0.0.0.0/0
   scram-sha-256` HBA line, and (when enabled) WAL archiving + TLS — appended last so
   they win.
8. **Start** the service (`pgStartScript`; `pgServiceName` = `postgresql-NN` on EL /
   `postgresql@NN-main` on Debian), reconcile DNS, then **set the superuser password**
   (`pgSetPasswordScript`: `runuser -u postgres psql … ALTER USER postgres PASSWORD
   :'pw'` — peer auth over the local socket, the password quoted safely via a psql
   variable).
9. When pgBackRest: `pgbackrest stanza-create` + initial **full backup** (reusing
   `patroniBackupScript`; non-fatal). PMM registration (best-effort, reusing
   `patroniPMM{RHEL,Debian}` with the superuser) when a PMM node is selected. Record
   `running`.

`handlePGBackup` (route **`POST /api/stacks/{id}/nodes/{nid}/pg/backup`**, owner-scoped)
runs an on-demand `pgbackrest --type=full backup` (`patroniBackupNowScript`) in the
node's container when pgBackRest is enabled and the node is running.

### Frontend — `app/web/src/pages/StackDesigner.jsx` + `PGManager.jsx`
- **`NODE_TYPES.pg`** (PG blue `#336791`, `Database` icon, `ports:false`,
  `osOptions:[{id:'oraclelinux'}]`, defaults incl. `pgMajor:'16'`,
  `usePgBackRest:false`, `seaweedfsNodeId:''`, cert/export defaults) + a **PostgreSQL**
  toolbar button (gated on Intranet, placed before "Patroni Cluster"). `nodeOSLabel`
  and the manager-panel `wide` list include `pg`.
- **`PostgreSQLForm`** (undeployed): the `usePPGCatalog` cascade (OS/version/arch + PG
  major/minor, reused from the Patroni form), superuser password, a **Use pgBackRest
  (SeaweedFS S3)** checkbox → a SeaweedFS-node `<select>`, PMM monitor, Intranet proxy,
  Intranet-CA cert + TTL, and 5432 host export. All lock once deployed.
- A running PG node renders **`PGManager.jsx`** (panel widens): **Overview**
  (FQDN/PG version/image/role/pgBackRest/TLS/monitored-by/host port + delete),
  **Credentials** (superuser + psql URIs for the published host port and the in-stack
  FQDN), and a **Backup** tab (when pgBackRest) with a **Backup now** button →
  `pgApi(id, nid).backup()` (new in `lib/stackApi.js`).

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt -l` (clean), and the web build (`npm run build`)
  all pass.
- **Caveat — not validated on a live deployment.** The standalone PostgreSQL bootstrap
  reuses the §21 PPG packages/paths and pgBackRest plumbing (which were live-probed on
  Oracle Linux 9), but the non-Patroni path here (`initdb` into the packaged unit's
  data dir, the `postgresql-NN` service name, the configure/start/set-password scripts,
  and the pgBackRest stanza+backup on a plain server) is **build-verified only**; the
  Debian `pg_createcluster` path especially needs a live deploy to confirm.

### Live-deploy fix — superuser password (psql variable on stdin, not `-c`)
The first live deploy looped on `set superuser password: ERROR: syntax error at or
near ":" … ALTER USER postgres PASSWORD :'pw'`. `pgSetPasswordScript` quoted the
password with a psql variable (`:'pw'`) but ran it via **`psql -c`** — and psql only
expands `:'var'` for **stdin/file** input, never for a `-c` command string (a `-c`
string must be fully server-parseable, with no psql-specific features). So the literal
`:'pw'` reached the server. **Fix:** feed the SQL on **stdin** instead —
`printf '%s\n' "ALTER USER postgres PASSWORD :'pw';" | runuser -u postgres -- psql -v
ON_ERROR_STOP=1 -v pw="$SUPERPW"` — keeping the safe `:'pw'` quoting (handles arbitrary
passwords) while letting psql actually interpolate it.

### SeaweedFS node — pgBackRest backup documentation (`SeaweedFSManager.jsx`)
The SeaweedFS node's **Backups** tab previously showed only the `pgbackrest.conf`
`[global]` block. It now carries a full **pgBackRest → SeaweedFS S3** how-to (a bordered
section under the xtrabackup/PBM snippets), in three copyable steps templated with the
node's live endpoint/credentials/bucket/region:
1. **`pgbackrest.conf`** — the `[global]` S3 repo block **plus** a `[<stanza>]` block
   (with `pg1-path`/`pg1-port`) and a note on the per-OS data-dir paths.
2. **`postgresql.conf`** — `archive_mode`/`archive_command=pgbackrest … archive-push`,
   `wal_level`, `max_wal_senders`.
3. **Commands** — `stanza-create`, `check`, `--type=full|incr backup`, `info`, and a
   `--delta restore`, all as the postgres user.

A note points out that DBCanvas's own **PostgreSQL** (§22) and **Patroni** (§21) nodes
do all of this automatically when their *Use pgBackRest* option targets the node; the
snippets are for a manual/external client.

## 23. Percona Backup for MongoDB (PBM) for PSMDB Sharded Cluster + PSMDB RS

The **PSMDB Sharded Cluster** (`Type=="psmdb"`) and **PSMDB RS** (`Type=="psmrs"`)
frames now install **Percona Backup for MongoDB** on every member and can back the
cluster up to a **SeaweedFS S3** node. `percona-backup-mongodb` is installed on all
members **unconditionally** (like pmm-client, so backups can be turned on later
without a reinstall); a frame option enables `pbm-agent` on every mongod member and
registers the S3 store on a chosen SeaweedFS node.

### Install (always) — `app/mongodb.go`
`mongoPrepareNode` installs PBM right after pmm-client on every member of a
`psmdb`/`psmrs` frame (not the standalone `psm` node): `pbmInstall{RHEL,Debian}` =
**`percona-release enable pbm`** then `dnf install percona-backup-mongodb` (OEL) /
`apt-get install percona-backup-mongodb` (Ubuntu). The package ships the `pbm` CLI +
the `pbm-agent` unit; the unit is left unconfigured/stopped until backup is enabled.

### Data model — `app/intranet.go` + `app/mongodb.go`
`designFrame` gains **`EnablePBM bool`** (`enablePBM`) and reuses **`SeaweedFSNodeID`**
(shared with the Patroni fields). `mongoConfig` gains `EnablePBM` + `BackupRepo`;
`mongoSecrets` gains `PBMUser`/`PBMPassword` (user `pbm`, password `MongoPBM!…`,
stable across redeploys, seeded like the PMM password).

### Provisioning — `app/pbm.go`
When `EnablePBM` is set, after the cluster is up + PMM is registered, a **best-effort**
PBM phase runs (the cluster stays `running` even if PBM setup fails — failures are
logged, the node is **not** marked errored):
1. `waitSeaweedRunning` (§21) for the selected SeaweedFS node's S3 config/secret.
2. **`mongoEnsurePBMUser`** creates the documented PBM user + `pbmAnyAction` role
   (`readWrite`/`backup`/`clusterMonitor`/`restore`/`pbmAnyAction` on `admin`),
   reusing `mongoPMMUserScript`'s auth-or-localhost-exception flow. Sharded: on the
   **config-RS primary** + **each shard-RS primary** (replicates within each set);
   RS: on the **RS primary**.
3. **`mongoSetupPBMAgent`** on every **mongod** member (config + shard members; all
   RS members — **never mongos**): write the `pbm-agent` EnvironmentFile
   (`/etc/sysconfig/pbm-agent` on EL, `/etc/default/pbm-agent` on Debian) with
   `PBM_MONGODB_URI=mongodb://pbm:<pw>@localhost:27017/?authSource=admin` (credentials
   percent-encoded via `pbmMongoURI`), then `systemctl enable --now pbm-agent`.
4. **`mongoConfigurePBMStorage`** runs once from a coordinating member (a config server
   for sharded, the RS primary for a replica set): write `pbmStorageYAML` and run
   `pbm config --file` — S3 → SeaweedFS (`type: s3`, `forcePathStyle: true`, a
   per-cluster `prefix: pbm/<cluster>`, `insecureSkipTLSVerify` when the S3 endpoint is
   HTTPS). `pbmConfigScript` waits for the agents to connect (`pbm status`) before
   applying.

`handleMongoPBMBackup` (route **`POST /api/stacks/{id}/frames/{fid}/pbm/backup`**,
owner-scoped) runs an on-demand `pbm backup`, coordinated from a running config server
(sharded) or any running member (RS).

### Validation — `app/intranet.go` (`pbmFrameIssues`)
Both the `psmdb` and `psmrs` validation blocks call `pbmFrameIssues`: when `EnablePBM`
is set, the frame must reference a `seaweedfs` node present in the design (mirrors the
Patroni pgBackRest rule).

### Frontend — `StackDesigner.jsx` + `MongoDBManager.jsx`
- A shared **`PBMOptions`** component (an "Enable backups with Percona Backup for
  MongoDB" checkbox + a SeaweedFS-node `<select>` when enabled) is rendered in both
  **`MongoDBFrameForm`** and **`PSMRSFrameForm`** (after the PMM picker). Frame defaults
  add `enablePBM:false`/`seaweedfsNodeId:''`.
- **`MongoDBManager`** takes `frameId` and gains a **Backup** tab (shown only when
  `cfg.enablePBM`): a **Backup now** button → `mongoApi(id, fid).pbmBackup()` (new in
  `lib/stackApi.js`), the PBM user/password, and a note pointing to `pbm list` /
  `pbm restore` from a root console. The Overview gains a "Backups (PBM)" row.

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt -l` (clean), and the web build (`npm run build`)
  all pass.
- **Caveat — not validated on a live deployment.** The PBM install (`percona-release
  enable pbm` + `percona-backup-mongodb`), the `pbm-agent` EnvironmentFile + service,
  the PBM user/role, and `pbm config`/`pbm backup` against a live SeaweedFS S3 follow
  the PBM docs but are **build-verified only**; a live deploy (sharded + RS) should
  confirm — the sharded-cluster coordination (CLI against the config-server RS) and the
  SeaweedFS path-style S3 round-trip are the most likely spots to need a tweak.

## 24. pgBackRest requires an S3-TLS SeaweedFS node (validation)

pgBackRest's S3 client only speaks **HTTPS**, so it cannot use a plain-HTTP SeaweedFS
endpoint. Validation now enforces this for **both** pgBackRest consumers — the Patroni
cluster frame (§21) and the standalone PostgreSQL node (§22).

- **`app/pg.go`**: new **`pgBackRestSeaweedIssues(who, seaweedNodeID, doc)`** returns
  the SeaweedFS-backing issues for a pgBackRest user — an error when no node is
  selected, when the selected node isn't in the design, **or when the selected
  SeaweedFS node does not have S3 TLS enabled** (`designNode.TLS`, the §20 option). The
  message: *"… pgBackRest requires the SeaweedFS node <label> to have S3 TLS enabled
  (pgBackRest's S3 client needs HTTPS)"*.
- **`app/intranet.go`**: the standalone-`pg` case and the patroni-frame block both call
  the helper (replacing their inline "no node / not in design" checks; the now-unused
  `seaweedIDs` map was removed). `repo1-s3-verify-tls=n` in `patroniPgBackRestConf`
  still stands, so a **self-signed** (TLS on, cert off) SeaweedFS works — verification
  is skipped, but the transport is the required HTTPS.
- **Frontend** (`StackDesigner.jsx`): the SeaweedFS selector in `PostgreSQLForm` and
  `PatroniFrameForm` notes "The node must have S3 TLS enabled (pgBackRest needs HTTPS)"
  and annotates non-TLS nodes in the dropdown with "— needs S3 TLS".

(PBM/MongoDB §23 is unaffected — PBM supports plain-HTTP S3, so its SeaweedFS node has
no TLS requirement.)

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt -l` (clean), and the web build all pass.

## 25. repmgr PostgreSQL cluster frame + Barman (cloud) → SeaweedFS S3

A **repmgr cluster** frame (`Type=="repmgr"`): a group of PostgreSQL nodes (Percona
Distribution for PostgreSQL) on the systemd OS images using **streaming replication
managed by repmgr** — one node bootstraps as primary, the rest are cloned as standbys,
and **`repmgrd`** on every node provides automatic failover. Its options mirror the
Patroni frame (§21) — catalog OS/version/arch, PG major/minor, superuser password,
PMM, Squid proxy, Intranet-CA TLS, 5432 host export — but it uses **repmgr** instead of
Patroni/etcd and, for backups, **Barman cloud** (`barman-cloud-backup` /
`-wal-archive`) pushing to a **SeaweedFS S3** node instead of pgBackRest. **3–7 nodes**
(min 3, max 7). No canvas association/HAProxy (apps connect to the primary; failover is
handled by repmgrd).

### Data model — `app/intranet.go`
`designFrame` gains **`UseBarman bool`** (reusing `SeaweedFSNodeID`, plus the Patroni
PG fields `PGMajor`/`PGVersion` and `OS`/`OSVersion`/`Arch`/`RootPassword`/`PMMNodeID`/
`UseProxy`/`GenerateCert`/`CertTTL`). Members are `Type=="repmgr"` + `FrameID` +
`ExportEnabled`/`ExportHostPort`. Deploy dispatch + the frames loop add a `repmgr`
case; `refreshPublishedPorts` reads `5432/tcp` into `repmgrConfig.ExportPort`.

### Provisioning — `app/repmgr.go`
`provisionRepmgrFrame` (modeled on `provisionPatroniFrame`): credentials `pgSecrets`
(`postgres` superuser + a `repmgr` SUPERUSER/REPLICATION role used for both streaming
replication and repmgr metadata), then an async goroutine —
1. `waitIntranet`; when Barman is on, `waitSeaweedRunning` for the S3 config/secret.
2. **Parallel `repmgrPrepareNode`**: create the container (systemd image,
   `DNS=[intranetIP]`, publish 5432 when export on), install `percona-postgresqlNN-*` +
   the repmgr package (`repmgrPackages`: EL `percona-repmgrNN`, Debian
   `postgresql-NN-repmgr`) + pmm-client; when Barman is on, **install barman-cloud**
   (`barman-cli-cloud` + `python3-boto3` from the PGDG / apt.postgresql.org repos — see
   §26(d); originally pip, which couldn't resolve on EL9) and stage
   `~postgres/.aws/{credentials,config}` (the config forces
   **path-style** S3 addressing, which SeaweedFS requires). Write `/etc/repmgr.conf`
   (node_id, conninfo, `failover=automatic`, promote/follow commands) + `~/.pgpass`.
3. **Primary** (`repmgrSetupPrimary`, member 0): `initdb` (reuses `pgInitScript`),
   optional TLS, append replication/repmgr settings to postgresql.conf + pg_hba
   (`wal_level=replica`, `max_wal_senders`, `shared_preload_libraries='repmgr'`, and the
   Barman `archive_command` when enabled), start PostgreSQL, set the superuser password,
   create the `repmgr` role + `repmgr` database, `repmgr primary register`.
4. **Standbys** (sequential `repmgrSetupStandby`): `repmgr standby clone --fast-checkpoint
   -F` from the primary, optional per-node TLS, start PostgreSQL, `repmgr standby register`.
5. **repmgrd** on every node via a small `repmgrd.service` unit (PGDG ships no clean unit).
6. When Barman: initial `barman-cloud-backup` on the primary (best-effort). PMM register
   (reusing the §21 `patroniRegisterPMM`). Record `running`.

`handleRepmgrBackup` (route **`POST /api/stacks/{id}/frames/{fid}/barman/backup`**)
runs an on-demand `barman-cloud-backup` on the **current primary** (found via
`pg_is_in_recovery()`, so it's correct after a failover).

### Validation — `app/intranet.go` + `app/repmgr.go`
A `repmgr`-frame block enforces **3 ≤ members ≤ 7** (error), odd-count (warning), unique
cluster name, the 5432 export joins the shared host-port conflict check, and
`UseBarman` ⇒ a SeaweedFS node present in the design (`barmanSeaweedIssues`). **Unlike
pgBackRest, Barman does *not* require S3 TLS** — barman-cloud/boto3 work over plain HTTP.

### Frontend — `StackDesigner.jsx` + `RepmgrManager.jsx`
- **`NODE_TYPES.repmgr`** (cyan `#0e7490`, `Database` icon) + `FRAME_COLORS.repmgr`;
  `frameVersionLabel`/member sub-label/`nodeOSLabel`/`wide`/minimap include `repmgr`.
  The frame renders **without `PortHandles`** (no association, like InnoDB).
- **Toolbar** "repmgr Cluster" → `addRepmgrCluster` (3 members `repmgrNN`, cluster
  `repmgr-cluster-NN`); frame **+/−** resizes 3–7 (`addFrameMember`/`removePXCNode`).
- **`RepmgrFrameForm`** (PPG catalog cascade, superuser pw, **Use Barman** checkbox →
  SeaweedFS `<select>`, PMM/proxy/cert, 3–7 guidance) + **`RepmgrMemberForm`** (5432
  export). A running member shows **`RepmgrManager.jsx`** (Overview incl. role/node_id/
  Barman, Credentials, and a Backup tab with **Backup now** → `repmgrApi(id, fid).backup()`).
- **SeaweedFS Backups tab** (`SeaweedFSManager.jsx`) gained a **"Barman (cloud) →
  SeaweedFS S3"** section (3 copyable steps: `~postgres/.aws` credentials+config,
  postgresql.conf `archive_command`, and backup/list/restore commands), templated with
  the node's live endpoint/credentials/bucket/region — alongside the existing
  xtrabackup/PBM/pgBackRest snippets.

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt -l` (clean), and the web build all pass.
- **Caveat — not validated on a live deployment.** The repmgr package names
  (`percona-repmgrNN` / `postgresql-NN-repmgr`), the `repmgr primary/standby
  register`/`clone` flow, the `repmgrd.service` unit, the barman-cloud pip install, and
  the barman-cloud S3 round-trip to SeaweedFS follow the repmgr/Barman docs but are
  **build-verified only**; a live deploy should confirm — the repmgr package/service
  names and the barman-cloud S3 path-style addressing are the most likely spots to need
  a tweak. **(The repmgr package source was corrected in §26 — see below.)**

## 26. Watchtower node (PMM upgrades) + toolbar colors + repmgr PGDG fix

Three changes in one session:

### (a) Toolbar buttons match node/frame colors — `StackDesigner.jsx`
A `typeColor(t)` helper (`FRAME_COLORS[t] || NODE_TYPES[t]?.color`) + `addBtnStyle(t)`
returns inline `{ backgroundColor, borderColor, color:'#fff' }`. Every "add" button in
the toolbar (Intranet, PMM3, PXC/ProxySQL/Percona Server/InnoDB/PSMDB/Patroni/repmgr
clusters, standalone PG/PS/PSMDB, HAProxy, SeaweedFS, Watchtower) is tinted with its
type's canvas color; the shared `disabled:opacity-50` still fades disabled buttons.

### (b) Watchtower node + PMM association
A **Watchtower** node (`Type=="watchtower"`, per-stack **singleton**) running
`percona/watchtower:latest` (pulled at deploy) with the **docker socket mounted** and its
**HTTP API enabled** (`WATCHTOWER_HTTP_API_TOKEN=<generated>` + `WATCHTOWER_HTTP_API_UPDATE=1`).
A PMM node can be **associated** with it so PMM drives in-app server upgrades.

- **`app/docker.go`** — `ContainerSpec` gains **`Binds []string`** (extra `src:dst[:mode]`
  bind mounts), merged into the HostConfig `Binds` in `ContainerCreate` (after the
  privileged cgroup binds). Used for the docker socket.
- **`app/watchtower.go`** (new) — `provisionWatchtower` (pull image → `waitIntranet` →
  create with `Env=[token, update]`, `Binds=["/var/run/docker.sock:/var/run/docker.sock"]`,
  network alias `watchtower`, `DNS=[intranetIP]` → start → `reconcileStackDNS`). The API
  **token is reused across redeploys** (read back from `Secrets`). `watchtowerConfig`
  (image/hostname/fqdn/alias/apiPort 8080) + `watchtowerSecrets` (apiToken).
  `waitWatchtower` (bounded) returns the running Watchtower's FQDN + token;
  `watchtowerHostEnv` builds `PMM_WATCHTOWER_HOST=http://<fqdn>:8080` + `PMM_WATCHTOWER_TOKEN`.
- **`app/pmm.go`** — when `n.WatchtowerNodeID != ""`, `provisionPMM` waits (best-effort)
  for the Watchtower and sets the two `PMM_WATCHTOWER_*` env vars on the PMM container
  (`ContainerSpec.Env`); if the Watchtower never comes up PMM still starts without it.
- **`app/intranet.go`** — `designNode` gains **`WatchtowerNodeID`**; deploy dispatch +
  `validateDesign` add a `watchtower` case (singleton check; a PMM whose `WatchtowerNodeID`
  references a missing/non-watchtower node is an error). No published ports.
- **`StackDesigner.jsx`** — `NODE_TYPES.watchtower` (slate `#475569`, `Server` icon,
  singleton, `percona/watchtower` image); toolbar button (disabled once one exists);
  **`PMMOptions`** gains a **Watchtower `<select>`** (`watchtowerNodeId`); property-panel
  dispatch renders **`WatchtowerForm`** (pre-deploy) / **`WatchtowerManager`** (running —
  shows image/host/API URL/token).

### (c) repmgr installs from PGDG, not Percona — `app/repmgr.go`
`percona-repmgrNN` does **not** exist in the Percona repo (`Unable to find a match`).
repmgr is shipped by **PGDG**, so the whole repmgr frame now installs PostgreSQL **and**
repmgr from PGDG (its on-disk layout — `/usr/pgsql-NN/bin`, `/var/lib/pgsql/NN/data`,
`postgresql-NN.service` — is identical to Percona's, so the `pg.go` path helpers still
apply). `repmgrAllPackages` returns EL `postgresqlNN-server` + `postgresqlNN-contrib` +
**`repmgr_NN`** (underscore) / Debian `postgresql-NN` + `postgresql-NN-repmgr`. New
`repmgrInstallRHEL` installs `pgdg-redhat-repo-latest.noarch.rpm` (EL/arch detected via
`rpm -E %rhel` + `uname -m`), `dnf module disable postgresql`, then the packages (EPEL
on for deps); `repmgrInstallDebian` adds `apt.postgresql.org` (`<codename>-pgdg`) with its
signing key, then installs. `frameVersionLabel` for repmgr now reads "PostgreSQL … ·
repmgr (PGDG)" (dropped "Percona"). The §25 `percona-repmgrNN` caveat is resolved here.

### (d) Barman installs from PGDG packages, not pip — `app/repmgr.go`
The pip install (`barman[cloud]` / `barman boto3`) failed on a live repmgr+Barman deploy
with **`ResolutionImpossible`** — pip's resolver can't satisfy barman's dependency set
against EL9's dnf-managed system Python. Since the repmgr frame already adds the PGDG
(EL) / apt.postgresql.org (Debian) repos, `barmanInstallRHEL/Debian` now install the
distro-packaged **`barman-cli-cloud`** (provides the `barman-cloud-*` binaries) plus
**`python3-boto3`** (the aws-s3 provider; `barman-cli-cloud` only *Recommends* it, so it's
explicit). Both scripts assert `barman-cloud-backup` is on PATH **and** `import boto3`
works. The SeaweedFS "Barman (cloud)" doc snippet (`SeaweedFSManager.jsx`) now shows the
`dnf/apt install barman-cli-cloud python3-boto3` commands instead of `pip3 install`.

### Verification performed
- `go build`/`go vet`/`go test`, `gofmt -l` (clean), and the web build all pass.
- **Caveat — not validated on a live deployment.** The Watchtower HTTP-API ⇄ PMM upgrade
  flow and the PGDG repmgr/Barman install (repo rpm URL, `repmgr_NN`, `barman-cli-cloud` +
  `python3-boto3`, module-disable) are **build-verified only**; a live deploy should confirm.

## 27. Fix Roundcube/dovecot crash under Rosetta (Apple Silicon) — `app/intranet.go`

On macOS/Apple Silicon with Rancher Desktop, an **amd64** Intranet runs under Rosetta and
`php-fpm` (Roundcube) + `dovecot` crashed at start:

```
dovecot[…]: rosetta error: mmap_anonymous_rw mmap failed, size=1000
php-fpm.service: Main process exited, code=dumped, status=11/SEGV
```

### Root cause
The §-existing "Relax sandboxing for emulation" step cleared only
`MemoryDenyWriteExecute=no` + `SystemCallFilter=`. But on **EL9 the units that actually
crash don't set either directive** — so the step was a **no-op** for them. Inspected
in-container (`oraclelinux-9-amd64`): `php-fpm.service` ships `PrivateTmp=true`;
`dovecot.service` ships `PrivateTmp=true`, **`ProtectSystem=full`**, **`PrivateDevices=true`**.
`PrivateDevices`/`ProtectSystem` set up a private mount namespace, a stripped `/dev`, an
`~@raw-io` seccomp filter and RO `/usr` — confinement the Rosetta translator can't work
under (it can't obtain the anonymous RW code-cache mapping it later flips to RX).

### Fix
The "Relax sandboxing for emulation" drop-in (`/etc/systemd/system/<svc>.service.d/
10-dbcanvas-emulation.conf`, written for php-fpm/dovecot/httpd/postfix/named/slapd/squid/
rsyslog when `$EMULATED`) now **fully un-confines** the daemons: adds
`PrivateDevices=no`, `PrivateTmp=no`, `ProtectSystem=no`, `ProtectHome=no`,
`ProtectKernelTunables/Modules=no`, `ProtectControlGroups=no`, `RestrictNamespaces=no`,
`RestrictRealtime=no`, `SystemCallArchitectures=` (allow non-native syscall ABI),
`RestrictAddressFamilies=` and `LockPersonality=no`, on top of the original
`MemoryDenyWriteExecute=no` + `SystemCallFilter=` (kept as belt-and-suspenders for other
unit/OS versions). Emulation detection (`HostArch` arm + node `Arch==amd64`) is unchanged.
These are localhost-only dev services, so the hardening loss is moot.

### Verification performed
- `go build`/`go vet`/`gofmt -l` clean.
- Drop-in applied in a live `oraclelinux-9-amd64` container: `systemd-analyze verify
  php-fpm.service dovecot.service` reports no directive errors, and `cat-config` confirms
  the overrides follow (and thus win over) the units' `PrivateDevices/ProtectSystem/
  PrivateTmp`.
- **Caveat — not validated on Apple Silicon.** This dev host is x86_64, so the actual
  Rosetta round-trip could not be reproduced; the user should confirm php-fpm/dovecot now
  start on macOS/Rancher with an amd64 Intranet. (Native **arm64** Intranet — the default
  when the dbcanvas server itself runs on Apple Silicon — avoids Rosetta entirely.)

## 28. Barman installs from `barman-cli` on EL (not `barman-cli-cloud`) — `app/repmgr.go`

§26(d) installed `barman-cli-cloud` on both EL and Debian, but a live repmgr+Barman deploy
failed on EL: `Unable to find a match: barman-cli-cloud`. The package name differs by
repo — **PGDG's EL/yum repo ships the `barman-cloud-*` binaries inside `barman-cli`**
(there is no `barman-cli-cloud` there); only apt.postgresql.org splits them into
`barman-cli-cloud`. Fixes:
- `barmanInstallRHEL` now installs **`barman-cli`** (+ `python3-boto3`).
- `barmanInstallDebian` keeps `barman-cli-cloud` but **falls back to `barman-cli`** if it's
  unavailable; both still verify `barman-cloud-backup` is on PATH and `import boto3` works.
- The SeaweedFS "Barman (cloud)" doc snippet (`SeaweedFSManager.jsx`) EL line now reads
  `dnf install barman-cli python3-boto3`.

`go build`/`go vet`/`gofmt -l` + web build all pass.

## 29. Fix repmgr+Barman "write AWS credentials: docker copy archive: (404)" — `app/repmgr.go`

First live repmgr+Barman+SeaweedFS deploy failed at "write AWS credentials" with a Docker
`(404)`. Root cause: the Docker `PUT /containers/{id}/archive?path=<dir>` endpoint extracts
only into an **existing** directory (a missing path 404s), but the Barman step copied
`credentials`/`config` into `~postgres/.aws` which was never created — `barmanChownScript`
(which touches `.aws`) only runs *after* the copies. Reproduced locally: copy to a missing
dir → 404, to an existing dir → 200.

Fix: before the two `CopyFile` calls, run `install -d -m 700 "$HOME/.aws"` (HOME=pgHome).
Mirrors the same fix Patroni already uses for `/etc/pgbackrest` (§24). `go build`/`vet`/
`gofmt -l` clean.

## 30. Revert §27 — Rosetta crash is the translator itself, not systemd confinement

A post-§27 deploy still crashed php-fpm/dovecot on Apple Silicon, with audit records
proving the fault is in Rosetta, not the sandbox:

```
comm="php"     exe="/mnt/lima-rosetta/rosetta" sig=11
comm="php-fpm" exe="/mnt/lima-rosetta/rosetta" sig=11
comm="dovecot" exe="/mnt/lima-rosetta/rosetta" sig=5
```

Even the bare `php` CLI (the Roundcube DB-init `php -r` step) segfaults inside
`/mnt/lima-rosetta/rosetta` — so un-confining the systemd units (§27) cannot help, and
just weakened the daemons for no benefit. **§27's expansion is reverted**; the
"Relax sandboxing for emulation" step is back to its original two-directive form
(MemoryDenyWriteExecute / SystemCallFilter).

Confirmed the dead-ends:
- **No mod_php on EL9** — RHEL/Oracle Linux 8+ dropped the Apache PHP module; the only
  PHP SAPI is php-fpm (httpd proxies `.php` to `/run/php-fpm/www.sock`). Verified against
  the reference `db-canvas/oel9-systemd` intranet container, which uses exactly that
  `<IfModule !mod_php.c>` FPM-proxy wiring. That container only "works" because it runs
  **native x86_64** (no Rosetta) on the dev host.
- Roundcube-without-php-fpm wouldn't help anyway: mod_php would load the same Zend engine
  that crashes, and dovecot (needed for IMAP) crashes independently.

Real fixes are environment-level: run the Intranet as **native arm64** (no Rosetta — the
default when the dbcanvas server runs on Apple Silicon), or switch Rancher Desktop's VM
emulation from **Rosetta (VZ) to QEMU**. `go build`/`vet`/`gofmt -l` clean.

## 31. Roundcube via `php -S` (no httpd/php-fpm) — restore Rosetta-working webmail

§30 concluded webmail couldn't work under Rosetta, but the user pointed at the older
`db-canvas/oel9-systemd` intranet container, which *did* work on macOS/Rancher. Inspecting
it revealed the technique: a custom `dbpg-roundcube.service` serving Roundcube with **PHP's
built-in web server** instead of httpd + php-fpm:

```
ExecStart=/usr/bin/php -d error_reporting=0 -S 0.0.0.0:80 -t /usr/share/roundcubemail
Restart=always
```

The current repo had regressed to httpd→php-fpm (Alias `/roundcubemail`). Under Rosetta the
`mmap_anonymous_rw mmap failed` crash is **transient** at process start; a single `php -S`
process with **`Restart=always`** keeps relaunching until a start lands, whereas php-fpm's
master/worker model under httpd dies and its unit gives up. The fix ports the working
approach back:

- **`intranet.go` "Configure webmail"** — initialize the sqlite schema in a **retry loop**
  (the `php` CLI can SIGSEGV mid-init under Rosetta) and fail the step if the db never
  appears; then write **`/etc/systemd/system/dbcanvas-roundcube.service`** (`php -S` on
  container port 80, `Restart=always`, `RestartSec=2`). Dropped the httpd
  `roundcubemail.conf` `Require all granted` tweak.
- **"Enable services"** — start `dbcanvas-roundcube` instead of `php-fpm`/`httpd` (both
  still installed via the roundcubemail RPM, just not started).
- **"Relax sandboxing for emulation"** — now adds **`Restart=always`/`RestartSec=2`** (plus
  the harmless MDW/SCF clears) to dovecot/postfix/named/slapd/squid/rsyslog so they too
  ride out transient Rosetta start failures. Still gated on `$EMULATED`.
- **`IntranetManager.jsx`** — webmail link is now `http://host:port/` (php -S serves
  Roundcube at the root, not under httpd's `/roundcubemail` alias).

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build clean.
- Functionally verified in a live EL9 amd64 container: with httpd/php-fpm stopped, the
  `dbcanvas-roundcube` (`php -S`) unit is active and `GET /` → 200 serving the "DBCanvas
  Webmail" login page, `?_task=login` → 200, and a skin CSS asset → 200. (Native x86 here;
  the Restart=always behavior under actual Rosetta still wants a macOS confirm, but this is
  a faithful port of the user's previously-working config.)

## 32. Roundcube php -S as apache on 8080 (not root:80); drop squid dns_v4_first

Follow-ups to §31:

### Webmail no longer runs as root — `app/intranet.go`
The `dbcanvas-roundcube.service` (php -S) previously ran as **root** on port 80 (matching
the old db-canvas unit). Now it runs as the unprivileged **apache** user, which can't bind
<1024, so it binds **8080**. The container's published port changed 80→8080 accordingly:
`ContainerCreate(PublishPort: 8080)`, the post-start `ContainerPort(id, "8080/tcp")`, and
`refreshPublishedPorts`'s intranet `readPort("8080/tcp")`. dbcanvas still publishes that to
an auto host port, so the recorded `WebmailPort` / frontend `http://host:port/` link are
unchanged. Verified live: the unit runs as `apache`, `GET /:8080` → 200, and a login hit
sets `roundcube_sessid` (sessions writable — apache is in group apache and
`/var/lib/php/session` is group-writable; `/var/lib/roundcubemail` is apache-owned).

### Removed Squid `dns_v4_first` — `app/intranet.go`
The "Configure Squid" step no longer appends `dns_v4_first on` to squid.conf. (The two
comments that referenced it — in `dnfIPv4Script` and the "Configure named" filter-aaaa
step — were updated; dnf `ip_resolve=4` and bind's filter-aaaa remain.)

`go build`/`vet`/`gofmt -l` clean.

## 33. Fix Barman "No module named 'botocore'" — boto3 must target barman's python

A live repmgr+Barman backup failed: `Barman cloud backup exception: No module named
'botocore'`. Root cause (reproduced): on EL9 PGDG builds barman for **python3.12**
(`barman-cloud-backup` shebang `#!/usr/bin/python3.12`, `Requires: python3.12`), but the
system `python3` is **3.9**. §28/§32's `dnf install python3-boto3` lands boto3 in 3.9, and
there is **no `python3.12-boto3` RPM** — so barman-cloud (3.12) can't import botocore. The
old install check `python3 -c 'import boto3'` (3.9) passed → false confidence.

Fix (`app/repmgr.go` `barmanInstallRHEL`): derive the interpreter from the
`barman-cloud-backup` shebang, install `<pyver>-pip` (python3.12-pip is in AppStream) and
`<interp> -m pip install boto3` into *that* interpreter (dnf python3-boto3 fallback), then
verify `import boto3, botocore` **under that interpreter**. Installing only boto3 into the
otherwise-empty 3.12 site avoids the ResolutionImpossible the full `barman[cloud]` pip route
hit against 3.9. `barmanInstallDebian` now also verifies against the shebang interpreter.
The SeaweedFS doc snippet's EL line updated to
`dnf install barman-cli python3.12-pip && python3.12 -m pip install boto3`.

Verified live in an EL9 container: pip installs boto3 1.43.x into python3.12 and
`barman-cloud-backup --help` imports cleanly. `go build`/`vet`/`gofmt -l` + web build pass.

## 34. Webmail deploy no longer fails on the sqlite pre-init under Rosetta

The §31 "Configure webmail" step pre-created the Roundcube sqlite db with a one-shot
`php -r '... new PDO(sqlite) ...'` in a retry loop, failing the step (`exit 1`) if the db
never appeared. On macOS/Rosetta that **one-shot php CLI SIGSEGVs every time** (transient
mmap failure, but a single short-lived process never gets a good run), so all 10 attempts
failed and the whole Intranet deploy aborted at "Configure webmail". (The logged error was
the xtrace echo of the php line — note the normalized `2> /dev/null` — i.e. the step hit
the fatal `exit 1`.)

Fix (`app/intranet.go`): **drop the `php -r` pre-init and the fatal check.** Roundcube
creates the sqlite db + schema itself on first request (verified: wiping the db and hitting
`php -S` once regenerates the full 176 KB schema, apache-owned — matching the old db-canvas
container whose db was likewise created at runtime by `php -S`). The step now just writes
the config and makes `/var/lib/roundcubemail` apache-writable; the long-running
`dbcanvas-roundcube.service` (php -S, `Restart=always`) creates the db on first hit and
rides out any transient Rosetta crash until a request lands. `go build`/`vet`/`gofmt -l`
clean.

## 35. The real Rosetta webmail fix: disable php opcache/JIT (not the user/db)

§31–§34 chased symptoms; comparing the old working `db-canvas` intranet against the current
one found the actual difference: **php-opcache**. The old image had **no `php-opcache`
package** (no `/etc/php.d/10-opcache.ini`, no Zend OPcache module). The current image ships
it with `opcache.enable_cli => On` and `opcache.jit => tracing`.

OPcache and its JIT allocate **executable memory via mmap** — exactly the operation Rosetta
can't satisfy (`mmap_anonymous_rw mmap failed`). So with opcache on, `php -S` (CLI SAPI,
enable_cli=On) starts fine but **SIGSEGVs the instant it executes Roundcube code on a
request** — which is precisely the audit trail (server "started", then `Accepted` →
`status=11/SEGV` every request, `Restart=always` looping to the limit). It is **not** about
the apache user (root would crash too — cf. the stray `uid=0 php sig=5`), so the §32
unprivileged setup is kept.

Fix (`app/intranet.go`): the `dbcanvas-roundcube.service` `ExecStart` now passes
`-d opcache.enable=0 -d opcache.enable_cli=0`, disabling opcache/JIT for the webmail server
(matching the old image's behavior without removing the package). Verified on x86: the flags
turn opcache off and `php -S` still serves the Roundcube login (`GET /` → 200, "DBCanvas
Webmail"). Keeps the apache user + 8080 + runtime db auto-create from §32/§34.
`go build`/`vet`/`gofmt -l` clean. (Still needs a macOS/Rosetta confirm, but this is the
concrete config delta from the setup that worked there.)

## 36. Rosetta dovecot fix: mmap_disable = yes (compared against working image)

After the §35 opcache fix the webmail UI worked on macOS but dovecot crashed under Rosetta
with SIGTRAP (`comm="dovecot" exe="/mnt/lima-rosetta/rosetta" sig=5`). `diff`ing
`dovecot -n` against the old working `db-canvas` intranet showed the relevant delta:
the old image set **`mmap_disable = yes`**, the current one didn't.

Dovecot mmaps its index/cache files by default; under Rosetta that mmap fails (same family
as the opcache/php-fpm crashes) and dovecot dies. Fix (`app/intranet.go`): add
`mmap_disable = yes` to the `/etc/dovecot/conf.d/99-dbcanvas.conf` it writes (forces plain
read/write I/O for indexes). Matches the working image; harmless on native hosts (minor
index I/O cost). Verified the config parses (`dovecot -n` OK) and dovecot starts.
`go build`/`vet`/`gofmt -l` clean.

(The other `dovecot -n` differences — mail_location path, first_valid_uid 5000 vs 1000,
PLAIN vs SHA512-CRYPT passdb, imap-only vs imap+lmtp, ssl — are intentional dbcanvas config
choices, not the crash cause.)

## 37. The actual Rosetta dovecot fix: default_vsz_limit = 1G

§36's `mmap_disable` didn't fix dovecot — the crash was still `rosetta error:
mmap_anonymous_rw mmap failed, size=1000` (Rosetta's *own* translation mmap, not dovecot's
index mmap). The real delta from the working image: dovecot caps each process's address
space (`default_vsz_limit`) at **256 M** by default, but the Rosetta translator needs a much
larger virtual mapping for its runtime/code cache — under 256 M even a 4 KB mmap fails and
dovecot dies (SIGTRAP). The old working `db-canvas` image set **`default_vsz_limit = 1 G`**.

Fix (`app/intranet.go`): add `default_vsz_limit = 1G` to the dovecot `99-dbcanvas.conf`
(kept `mmap_disable = yes` too — both were in the working image). Verified `doveconf` reports
`1 G`, config parses, dovecot starts. `go build`/`gofmt -l` clean.

This is the same class of Rosetta limitation seen throughout (§31/§35/§36): anything that
mmaps fails — php-fpm/httpd (→ php -S), php opcache/JIT (→ disabled), and dovecot under a
tight VSZ cap (→ raised). The common thread is giving the translator room / avoiding the
mmaps it can't satisfy.

## 38. Keycloak node (singleton) + PSMDB MONGODB-OIDC authentication

Adds a Keycloak OIDC identity provider node and an option on the standalone PSMDB (`psm`)
node to authenticate via MONGODB-OIDC against it.

### Keycloak node — `app/keycloak.go` (new) + `app/intranet.go`
A per-stack **singleton** `keycloak` node runs `quay.io/keycloak/keycloak:26.5.5` in dev
mode (`Cmd: start-dev --https-port=8443`, env `KC_BOOTSTRAP_ADMIN_USERNAME/PASSWORD`). Image
is pulled at deploy; the admin console is published to the host on auto-assigned ports
(8080 http / 8443 https), recorded in `keycloakConfig` (HTTPPort/HTTPSPort). Network alias
`keycloak` + container hostname = node host, so in dev mode Keycloak's token issuer matches
`http://<host>:8080/realms/<realm>` — which is what a MongoDB node points at. The bootstrap
admin password is generated + reused across redeploys (`keycloakSecrets`). `waitKeycloak`
gates dependents; `keycloakIssuer(host)` builds the issuer base. Wired into `intranet.go`:
designNode OIDC fields, deploy dispatch (`case "keycloak"`), `validateDesign` (singleton
count + "only one Keycloak per stack"), and `refreshPublishedPorts` (re-reads 8080/8443).

### PSMDB OIDC — `app/mongodb.go` + `app/intranet.go`
The `psm` node gains `EnableOIDC` + `KeycloakNodeID` + `OIDCRealm`/`OIDCClientID`/
`OIDCAuthClaim`/`OIDCUseAuthClaim` (defaults mongodb / mongodb-client / MyClaim / true).
`mongodConfYAML` gained a `setParams` arg; `mongoOIDCSetParameter` renders the
`setParameter:` block — `authenticationMechanisms: SCRAM-SHA-1,SCRAM-SHA-256,MONGODB-OIDC`
plus a single `oidcIdentityProviders` entry (issuer, audience==clientId, authNamePrefix
`keycloak`, clientId, useAuthorizationClaim, supportsHumanFlows, and authorizationClaim when
the group claim is used). `provisionMongoStandalone` resolves the Keycloak host, waits for
it, writes the block, and — when `useAuthorizationClaim` — creates the group-enumeration
roles `keycloak/developers` (readWriteAnyDatabase) + `keycloak/dbadmins` (root) via
`mongoOIDCRolesScript`. validateDesign errors if OIDC is enabled without a linked Keycloak
node. (Sharded/replica-set frames pass `setParams=""` — OIDC is standalone-only for now.)

### Frontend — `StackDesigner.jsx` + `MongoDBManager.jsx`
`NODE_TYPES.keycloak` (singleton, indigo `#4f46e5`, `Users` icon) + toolbar button +
`KeycloakForm`/`KeycloakManager` (manager shows console URL/ports + bootstrap admin creds).
`PSMStandaloneForm` gains a "Keycloak OIDC authentication" section (Keycloak `<select>`,
realm, client id, authorize-by-group toggle → authorization claim). `MongoDBManager`
overview shows the OIDC issuer/client when enabled, and the access tab shows the
`mongosh --authenticationMechanism MONGODB-OIDC --oidcFlows device-auth/auth-code` hints.

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build all pass.
- Rendered mongod OIDC config verified by a throwaway Go test (issuer/audience/authClaim
  present, authorizationClaim omitted when useAuthClaim=false) and confirmed valid YAML
  with the `oidcIdentityProviders` JSON re-parsing.
- Keycloak image pulled and **booted with the exact Cmd/env** — "Keycloak 26.5.5 … started
  … Listening on: http://0.0.0.0:8080", port 8080 reachable.
- **Caveat — not validated as a full live OIDC login.** The realm/client/groups/users are
  set up in the Keycloak console (per the documented steps); the in-network issuer vs.
  host-mongosh issuer resolution depends on the operator's setup as noted.

## 39. Ubuntu VNC node — web desktop jump box with Percona clients

A new **`vnc`** node (non-singleton): an XFCE desktop served over a browser-based VNC
client, with the Percona DB clients preinstalled, for ad-hoc troubleshooting.

### Backend — `app/vnc.go` (new) + `app/intranet.go`
`provisionVNC` pulls **ubuntu:24.04** (no systemd → runs `sleep infinity` as PID 1) and
installs/configures via exec steps:
- **Desktop + web VNC** (`vncInstallDesktopScript`): `xfce4`/`xfce4-goodies`/`dbus-x11`,
  `tigervnc-standalone-server` + **`tigervnc-tools`** (the latter provides
  `tigervncpasswd`, required by the tigervncserver wrapper), `novnc` + `websockify`.
- **Percona clients** (`vncInstallClientsScript`, best-effort): `percona-release` deb, then
  `percona-release enable ps-80 psmdb-80 ppg-17 valkey-91` and install
  `percona-server-client` (mysql), `percona-mongodb-mongosh` (mongosh),
  `percona-postgresql-client-17` (psql), `percona-valkey-tools` (valkey-cli; falls back to
  `valkey-tools`), and `ldap-utils` (ldapsearch). Each `|| true` so a future repo hiccup
  never blocks the desktop; the step logs which clients landed.
- **Sudo user** (`vncSetupUserScript`): creates the login user (default `dbadmin`) with the
  node-property password, adds it to `sudo` with a NOPASSWD sudoers drop-in, writes the
  8-char TigerVNC auth via `tigervncpasswd -f`, and an XFCE `~/.vnc/xstartup`
  (`dbus-launch --exit-with-session startxfce4`).
- **Launch** (`vncStartScript`, idempotent): writes `/usr/local/bin/dbcanvas-vnc-start.sh`
  and runs it — `tigervncserver :1` (VncAuth, rfbport 5901) + `websockify --web=/usr/share/
  novnc 6080 localhost:5901`; verifies Xvnc :1 and the web port are listening. Container
  port 6080 is published to an auto host port (`vncConfig.WebPort`); the manager links to
  `http://<host>:<port>/vnc.html`.

DNS points at the Intranet (so it resolves the stack's DB nodes by FQDN); apt optionally
routes through the Intranet Squid proxy (`UseProxy`). `intranet.go`: designNode `VNCUser`/
`VNCPassword`, deploy dispatch + validate `case "vnc"`, and `refreshPublishedPorts` re-reads
the 6080 host port on restart.

### Frontend — `StackDesigner.jsx`
`NODE_TYPES.vnc` (orange `#dd4814`, `Monitor` icon, non-singleton) + toolbar button +
`VNCForm` (desktop user, password, proxy) / `VNCManager` (web-desktop link, host:port,
user, VNC password).

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **End-to-end in a live ubuntu:24.04 container**: desktop+VNC install, `tigervncpasswd -f`
  password, `tigervncserver :1` started XFCE (xfce4-session/panel running), websockify up,
  and from the **host** `GET /vnc.html` → 200 / `GET /` → 200 through the published port.
- **All Percona clients install and run**: `mysql` (Percona Server 8.0.46), `mongosh`
  (2.8.3), `psql` (Percona PostgreSQL 17.10), `valkey-cli` (9.1.0 via percona-valkey-tools),
  `ldapsearch` present.
- Caveat: no auto-restart of the session on a bare `docker restart` (no systemd) — a
  redeploy relaunches it via the idempotent start step.

## 40. Ubuntu VNC fixes: start-step false failure, singleton, percona-toolkit

First live deploy of the §39 VNC node failed at "start desktop session" (10 attempts).
Two bugs + two requested changes:

### (a) Start step verified wrong — `app/vnc.go`
`vncStartScript` verified the session with `tigervncserver -list | grep ':1'`, but `-list`
prints the display as `1` (no colon), so the check always failed and the step exited
non-zero (the logged "error" was just the tigervncserver success banner on stderr). The VNC
session was actually up. Now verifies by checking the **listening ports** (5901 + the noVNC
web port) via `/dev/tcp`.

### (b) `pkill -f websockify` killed the deploy step itself — `app/vnc.go`
The launch helper ran `pkill -f 'websockify'` to stop a prior instance, but the deploy
step's own command line contains the word "websockify" (it writes the helper via a
heredoc), so pkill SIGTERM'd its own shell (exit 143) and also killed the just-started
websockify. Replaced with a **PID file** (`/run/dbcanvas-novnc.pid`) + `nohup` — no
broad pattern match. (Also confirmed TigerVNC 1.13 kills Xvnc if `xstartup` exits within
3s; the real `exec dbus-launch --exit-with-session startxfce4` stays alive, so this is
fine — only a stub xstartup would trip it.)

### (c) Singleton — `StackDesigner.jsx` + `app/intranet.go`
`NODE_TYPES.vnc.singleton = true`; toolbar button disabled once one exists; validateDesign
counts `vnc` and errors "Only one Ubuntu VNC node is allowed per stack".

### (d) percona-toolkit — `app/vnc.go`
`vncInstallClientsScript` now also `percona-release enable tools` and installs
`percona-toolkit` (pt-* utilities); the clients report includes `pt-query-digest`.

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **Full real-flow test in a live ubuntu:24.04 container** (xfce4 + real
  `dbus-launch startxfce4` xstartup + the corrected start helper): start step **exit 0**,
  websockify survives (PID file), `xfce4-session` running, and from the **host**
  `GET /vnc.html` → 200.
- Percona clients incl. `pt-query-digest` (percona-toolkit) install + resolve.

## 41. Ubuntu VNC rebased on the systemd image + Firefox + openssh-client

Reworked the §39/§40 VNC node to run on the **same systemd Ubuntu image as the database
nodes** (`dbcanvas-systemd:ubuntu-<ver>-<arch>` via `pxcImage`) instead of stock
ubuntu:24.04, so the desktop runs as real systemd services (survives restarts), and added
Firefox + the OpenSSH client.

### Backend — `app/vnc.go` (rewritten) + `app/intranet.go`
- Container is now **privileged with systemd as PID 1** (no `sleep infinity`); waits for
  systemd, then installs/configures via exec steps. Node carries `os`/`osVersion`/`arch`
  (defaults ubuntu/24.04/amd64); `validateDesign` checks the image exists (`make images`).
- **Services as systemd units** (the §40 sleep-infinity + nohup launcher is gone):
  the packaged **`tigervncserver@:1`** unit (driven by `/etc/tigervnc/vncserver.users`
  =`:1=<user>` and the user's **`~/.vnc/config`** — `session=xfce`, geometry, `localhost=no`,
  `securitytypes=VncAuth`) runs Xvnc on 5901; a small **`dbcanvas-novnc`** unit runs
  websockify serving noVNC on 6080 (published). Both `enable --now`; the step verifies the
  rfb + web ports listen. (Key gotcha found + handled: `/etc/tigervnc/vncserver-config-*`
  is Perl-eval'd; only the per-user `~/.vnc/config` is key=value — so the options live
  there.)
- **Firefox** from **Mozilla's APT repo** (Ubuntu's `firefox` is a snap that won't run in a
  container) — signing key + pinned source; best-effort (never fails the deploy).
- **openssh-client** added to the desktop install; **percona-toolkit** + the Percona
  clients + ldap-utils unchanged from §39/§40.

### Frontend — `StackDesigner.jsx`
`NODE_TYPES.vnc` defaults `os/osVersion/arch`; `VNCForm` gains Ubuntu version (24.04/22.04)
+ arch (amd64/arm64) selects; `nodeOSLabel` shows "Ubuntu <ver>"; description mentions
Firefox/SSH/toolkit.

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **End-to-end in a live `dbcanvas-systemd:ubuntu-24.04-amd64` container**: systemd boots;
  desktop+vnc+openssh-client install; **Firefox 152 from Mozilla repo** installs and
  `firefox --version` runs; `tigervncserver@:1` + `dbcanvas-novnc` both **active** (5901 +
  6080 listening); xfce4-session running; from the **host** `GET /vnc.html` → 200; Percona
  clients incl. `pt-query-digest` install.

## 42. Keycloak HTTPS (Intranet CA) + programmatic OIDC setup for PSMDB

The §38 PSMDB↔Keycloak OIDC didn't actually work: MongoDB OIDC **requires an HTTPS
issuer** (`Need to specify https: when accessing non-local URL 'http://keycloak:8080/...'`),
and the realm/client/users were left as manual console steps. This makes it work end to end.

### Keycloak HTTPS — `app/keycloak.go` + `app/intranet.go` + frontend
- Keycloak node gains an **Intranet CA SSL** option (reuses `GenerateCert`/CertTTL; default
  **on**). When set, `provisionKeycloak` signs a server cert for the Keycloak FQDN with the
  Intranet CA (`signTLSCert`), stages it into `/opt/keycloak/conf/tls.{crt,key}` on the
  created (not-yet-started) container, and runs `start-dev --http-enabled --https-port=8443
  --https-certificate-file/key --hostname=https://<fqdn>:8443`. The token issuer becomes
  `https://<fqdn>:8443/realms/<realm>`. `keycloakIssuer(host, ssl)` + `keycloakConfig.SSL`;
  `waitKeycloak` now also returns ssl + the container id + admin password (for kcadm).
- **Validation**: a PSMDB node with OIDC enabled now requires the linked Keycloak to have
  SSL on (else a clear error) — you can't deploy MongoDB OIDC against an HTTP Keycloak.

### PSMDB OIDC — `app/mongodb.go`
- Issuer is now `https://<keycloak-fqdn>:8443/realms/<realm>` (FQDN via stack DNS).
- mongod **trusts the Intranet CA** (`mongoCATrustScript`: stage `ca.crt` into the anchors,
  `update-ca-trust`, restart mongod) so it can fetch the issuer's JWKS over HTTPS.
- **Programmatic Keycloak setup** (`keycloakSetupScript`, run via kcadm *inside the Keycloak
  container*): creates the realm, the public OIDC client (standard flow + **OAuth2 device
  grant**, redirect `http://localhost:27097/redirect`), the **audience** mapper + (for the
  group path) the **group-membership** mapper (claim = the configured authorizationClaim,
  `full.path=false`), the `dbadmins`/`developers` groups, and two **sample users**
  (`dbauser01`→dbadmins, `devuser01`→developers) with a generated password. Idempotent.
- For the username path (`useAuthorizationClaim=false`) it also creates the matching
  `$external` MongoDB users (`keycloak/<user>@<domain>`). Sample username list + password
  are surfaced in the MongoDB manager (overview + credentials tab).

### Ubuntu VNC trusts the Intranet CA — `app/vnc.go`
After the desktop install, the Intranet `ca.crt` is added to the system trust
(`update-ca-certificates`) and Firefox **enterprise roots** are enabled
(`/etc/firefox/policies/policies.json` → `ImportEnterpriseRoots`), so the desktop browser
trusts the Keycloak HTTPS endpoint for the device-/auth-code login.

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **Live**: Keycloak 26.5.5 boots `start-dev` with the CA-signed cert; realm issuer is
  `https://keycloak.example.net:8443/realms/mongodb`, the cert validates against the CA, and
  the realm exposes a `device_authorization_endpoint`.
- **Live**: `keycloakSetupScript` run **twice** (idempotent) creates the realm, the client
  (device-grant + standard flow + public + redirect URI), both protocol mappers
  (`MyClaim`, `full.path=false`), the two groups, and the two sample users joined to their
  groups.
- Caveat: the interactive mongosh device-auth/auth-code token round-trip wasn't exercised
  here (needs a browser), but every required piece (HTTPS issuer, CA trust, audience +
  group claim, roles, sample users) is in place and individually verified. Connect with a
  localhost-allowed host, e.g. `mongosh mongodb://127.0.0.1 --authenticationMechanism
  MONGODB-OIDC --oidcFlows device-auth` (the earlier ALLOWED_HOSTS error was from using the
  FQDN; mongosh restricts OIDC to localhost/allow-listed hosts by default).

## 43. Fix OIDC issuer mismatch (use the Keycloak FQDN, not the bare alias)

After §42, mongosh failed with `discovered metadata issuer does not match the expected
issuer`: mongod was configured with `https://keycloak:8443/realms/mongodb` (bare alias) but
Keycloak's `--hostname=https://<fqdn>:8443` makes its discovered issuer
`https://keycloak.example.net:8443/...`. Root cause: `provisionMongoStandalone` rebuilt the
issuer in its goroutine from `waitKeycloak`, which returned the Keycloak **bare hostname**.
Fix: `waitKeycloak` now returns the Keycloak **FQDN** (`keycloakConfig.FQDN`), so the
configured issuer exactly matches Keycloak's discovered issuer.

### Verification performed (full live end-to-end)
Keycloak (HTTPS, CA-signed, `--hostname=FQDN`) + the kcadm realm setup + a real
percona-server-mongodb node with the OIDC config + the Intranet CA trusted, on a shared
network:
- discovered issuer == configured issuer (`https://keycloak.example.net:8443/realms/mongodb`);
- the issuer metadata fetch validates over TLS via the **system trust** (HTTP 200, no `-k`);
- `mongosh --oidcFlows device-auth` now **passes the issuer check** and prints the
  device-code verification prompt (previously it errored in <2s);
- a token minted for the sample user `dbauser01` carries `aud:[mongodb-client,…]`,
  `MyClaim:[dbadmins]`, and `iss:https://keycloak.example.net:8443/realms/mongodb` — exactly
  what mongod needs to authenticate `keycloak/dbauser01` and grant the `keycloak/dbadmins`
  role. (Only the interactive browser approval step remains, inherent to device/auth-code.)

## 44. mongosh OIDC from a remote host needs --oidcTrustedEndpoint

OIDC worked from the PSMDB server (`127.0.0.1`) but failed from the Ubuntu VNC desktop with
`Host 'psm-01.example.net:27017' is not valid for OIDC authentication with ALLOWED_HOSTS of
'*.mongodb.net,…,localhost,127.0.0.1,…'`. This is a **mongosh client-side safety check**:
by default it only performs OIDC against localhost / a few Atlas domains. Connecting to the
node's FQDN/hostname from another machine is rejected unless you mark it trusted with
**`--oidcTrustedEndpoint`** (confirmed via `mongosh --help`). Not a server/deploy issue.

The MongoDB manager's OIDC connect hints (`MongoDBManager.jsx`) now show the correct
commands: from another host (e.g. the VNC desktop) `mongosh --host <fqdn> --authenticationMechanism
MONGODB-OIDC --oidcFlows auth-code --oidcTrustedEndpoint` (auth-code opens Firefox; device-auth
prints a code), plus the localhost form that needs no flag. Web build passes.

## 45. Valkey standalone node (valkey/valkey-bundle) + LDAP + pmm-client

First half of the Valkey work (cluster frame + palette redesign to follow). A standalone
**`valkey`** node — the Valkey analogue of the standalone Percona Server node — runs the
upstream **`valkey/valkey-bundle`** image (Debian 13; bundles valkey-server + the
json/search/bloom/**ldap** modules), pulled at deploy. (The pulled image obsoletes the
original "add valkey to make versions" ask — there's no repo to probe.)

### Backend — `app/valkey.go` (new) + `app/intranet.go`
`provisionValkeyStandalone` pulls the image, stages a `valkey.conf` into the created
(not-yet-started) container, and starts `valkey-server /etc/dbcanvas-valkey.conf`:
- **Credentials**: a default-user password (`requirepass`/`masterauth`) — from the node's
  RootPassword or auto-generated — shown in the manager.
- **LDAP (optional)**: when enabled, the conf `loadmodule`s libvalkey_ldap.so first (so the
  `ldap.*` directives parse) and points it at the Intranet OpenLDAP
  (`ldap.servers ldap://intranet.<domain>:389`, `auth_mode bind`, `bind_dn_prefix uid=`,
  `bind_dn_suffix ,ou=People,<baseDN>`). The bundle entrypoint auto-loads the other modules
  and skips re-loading ldap.
- **pmm-client**: installed via percona-release + `percona-release setup pmm3-client`
  (works on Debian 13/trixie) and registered with an associated PMM server.
- Reuses RootPassword/PMMNodeID/ExportEnabled+HostPort; adds designNode `UseLDAP`. Wired
  into validateDesign (host-port conflict check) + deploy dispatch (`case "valkey"`).

### Frontend — `StackDesigner.jsx`
`NODE_TYPES.valkey` (purple, Database icon, image `valkey/valkey-bundle`) + toolbar button
+ `ValkeyForm` (password, Enable-LDAP toggle, PMM, export) / `ValkeyManager` (host, LDAP,
export port, password, valkey-cli connect strings).

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **Live** (valkey/valkey-bundle): the exact flow — create → copy conf to /etc → start —
  runs; `valkey-cli -a <pw> PING` → PONG; all four modules load (no double-load); the
  `ldap.*` directives apply from the conf; and pmm-client installs on the Debian-13 image.
  (Full LDAP *bind* against a live slapd, and PMM dashboards, want a real stack to confirm.)

### Still to do (noted)
- Valkey **cluster** frame (3 default, 3–7) — like PXC.
- **Node palette redesign** (vertical, categorized, dockable left / undock + stretch).

## 46. Valkey Cluster frame (3–7 all-master shards)

The cluster half of the Valkey work (palette redesign still pending). A **`valkeycluster`**
frame is the Valkey analogue of the PXC frame: 3 members by default, resizable 3–7 via the
frame +/-, each running valkey/valkey-bundle with `cluster-enabled`, formed into an
all-master cluster with `valkey-cli --cluster create ... --cluster-replicas 0`.

### Backend — `app/valkey.go` + `app/intranet.go`
`provisionValkeyClusterFrame`: shared default-user password (requirepass/masterauth, reused
across redeploys) + optional LDAP, set on the frame. Phase 1 (parallel) creates/configures/
starts every member (`valkeyStartMember`, cluster conf via `valkeyConfFile(..., cluster=true)`);
phase 2 forms the cluster from the first member (`valkeyClusterCreateScript`, idempotent —
skips if already `cluster_state:ok`, polls for ok after create since gossip needs a few
seconds); phase 3 installs pmm-client per member + registers with PMM. designFrame gains
`UseLDAP`; wired into the deploy frame loop (memberType/provision), the redeploy gate, and
validateDesign (3 ≤ members ≤ 7, unique cluster name, host-port export conflicts).

### Frontend — `StackDesigner.jsx`
`FRAME_COLORS.valkeycluster` + `frameVersionLabel`; `addValkeyCluster` (3 `valkeyNN`
members, `valkey-cluster-NN`), `addFrameMember`/`removePXCNode` enforce 3–7; toolbar
"Valkey Cluster" button; member cards show "Valkey shard" + `valkey/valkey-bundle` and have
no association ports; `ValkeyClusterFrameForm` (password, LDAP toggle, PMM, 3–7 guidance) +
`ValkeyClusterMemberForm` (label, host-port export); running members reuse `ValkeyManager`.

### Verification performed
- `go build`/`vet`/`gofmt -l` + web build pass.
- **Live** (3× valkey/valkey-bundle on a network): members start, `valkey-cli --cluster
  create --cluster-replicas 0 --cluster-yes` forms the cluster, `cluster_state` reaches
  **ok** after ~4s of gossip (hence the poll), and a cross-node `SET`/`GET` routes correctly.

### Still to do
- **Node palette redesign** (vertical, categorized, dockable left / undock + stretch).

## 47. Node palette redesign — categorized vertical dock (undock/float/resize)

The horizontal toolbar of ~20 "Add" buttons was replaced with a **categorized vertical
palette** (`StackDesigner.jsx`). Groups: Core (Intranet/PMM3/Watchtower/Keycloak), MySQL
(PXC/ProxySQL/ProxySQL Cluster/Percona Server/PS Replication/InnoDB-GR), MongoDB (Sharded/
Replica Set/Standalone), PostgreSQL (PostgreSQL/Patroni/repmgr), Valkey (Cluster/standalone),
Storage & Tools (HAProxy/SeaweedFS/Ubuntu VNC). Each button keeps its node/frame color tint
and the "Add an Intranet node first" gating.

- **Docked (default)**: a 200px panel to the left of the canvas (flex sibling), scrollable,
  with an **Undock** button in its header.
- **Floating**: an absolutely-positioned panel over the canvas — **draggable** by its header
  (via the shared pointer-drag handler, `dragRef.kind==='palette'` → `palettePos`) and
  **resizable** (native CSS `resize: both`), with a **Dock** button to re-pin it left. Its
  pointer events are stopped from reaching the canvas pan handler.

The top toolbar now carries only the stack actions (Validate/Deploy/Destroy + status) and a
short hint / a "Palette" re-dock button. Web build passes. (Drag/resize are interactive and
want a browser to feel out, but the structure builds and mirrors the existing pointer-drag
machinery.)

## 48. Valkey cluster-member manager title + LDAP connect instructions

Two fixes reported against §45/§46:

1. **Wrong title for cluster members** — a running Valkey *cluster* member reused
   `ValkeyManager`, whose header read "Valkey (standalone)". `ValkeyManager` is now
   role-aware (`cfg.role === 'cluster'` → "Valkey (cluster member)"), and its connect
   examples use `valkey-cli -c` (cluster mode) for members.

2. **No LDAP connect instructions** — added an "LDAP login" panel to `ValkeyManager`
   (shown when `cfg.useLdap`). It documents the verified valkey-ldap flow, established live
   against an OpenLDAP server: an LDAP user can only `AUTH` once a **matching passwordless
   Valkey ACL user exists** (the module verifies the password via an LDAP bind). So the
   panel shows, as the default user, `... ACL SETUSER alice on ~* +@all`, then connecting as
   the LDAP user `valkey-cli --user alice -a <ldap-password>` (binds `uid=alice,ou=People,
   <baseDN>`). Verified end to end: with the ACL user present, `AUTH`/`--user` with the LDAP
   password succeeds (`ACL WHOAMI` → alice) and a wrong password returns WRONGPASS.

Web build passes. (LDAP users live in the Intranet OpenLDAP under ou=People.)

## 49. Stable PMM ports across Watchtower upgrades + Valkey pmm-agent (no systemd)

Two fixes:

### (a) PMM keeps its published ports across an in-GUI (Watchtower) upgrade — `pmm.go` + `docker.go`
PMM was published with Docker's *empty-HostPort* ephemeral binding, so when Watchtower
recreates the PMM container during an in-GUI server upgrade it re-assigns **new** host
ports (the access URLs changed). Now PMM publishes **fixed** host ports: `pmm.go` reuses
the previously-assigned ports from the stored config across redeploys, and allocates free
ones on first deploy via a new `freeHostPort()` helper (bind `:0`, release, reuse the
number); the container is created with explicit `PortBindings` (`PublishMap` HostPort),
which Watchtower preserves on recreate. So the PMM URLs stay stable across both dbcanvas
redeploys and Watchtower upgrades.

### (b) Valkey pmm-agent runs without systemd — `valkey.go`
The valkey/valkey-bundle image has no systemd, so the old `pmm-admin config` path (which
relies on `pmm-agent.service`) never started the agent — the node never joined PMM. Now,
after installing pmm-client, the Valkey PMM step runs `pmm-agent setup` (writes
`/usr/local/percona/pmm/config/pmm-agent.yaml` + registers the node with the server) and
then launches **`/usr/sbin/pmm-agent --config-file=…` in the background** (`setsid`), so it
joins and reports node metrics. Applies to both the standalone node and every cluster
member. (Verified: pmm-client 3.8.1 installs on the bundle's Debian 13; `pmm-agent setup`
flags + the binary path `/usr/sbin/pmm-agent` + config path are correct; setup registers
against a reachable server. No systemd → the agent doesn't auto-restart on a bare container
restart; a redeploy relaunches it. Full join needs a live PMM server to confirm.)

`go build`/`vet`/`gofmt -l` clean.

## 50. PMM /srv volume + root/pmm consoles + port label; Valkey PMM add + PMM_PASSWORD

A batch of PMM + Valkey monitoring fixes.

### .env / compose — `PMM_PASSWORD`
Added `PMM_PASSWORD=pmm_password` to `.env` and `PMM_PASSWORD: ${PMM_PASSWORD:-pmm_password}`
to the app service in docker-compose.yml. It's the password for the read-only **`pmm`**
monitoring user created in Valkey.

### PMM data survives an upgrade — `docker.go` + `pmm.go`
PMM had no persistent storage, so an in-GUI/Watchtower upgrade (container recreate) started
fresh — losing the Grafana DB + signing key, which is why login gave **"session closed"**
after upgrade. Now PMM mounts a **stable named volume** (`dbcanvas-pmm-<stack>-<node>`) at
**`/srv`** (new `docker.VolumeCreate`; bind `vol:/srv`). Named volumes survive container
recreate (dbcanvas redeploy *and* Watchtower), so all PMM data persists. (Combined with the
§49 fixed host ports, the URLs + data both stay put across upgrades.)

### Root vs PMM console — `docker.go` + `terminal.go` + frontend
The PMM container runs as the unprivileged `pmm` user, so "Open root console" actually gave
a *pmm* shell. The terminal exec now takes an optional `?user=` (→ exec `User`); the PMM
manager shows **two** buttons — **Root console** (`user=0`) and **PMM console** (default) —
with a note. `HijackExec` gained a `user` param; `openTerminal({…, user})` appends it.

### PMM port label — `PMMManager.jsx`
Fixed the wrong "HTTP · 8443→container 8080" → **"HTTP · 8080"** (HTTPS row already correct).

### Valkey added to PMM monitoring — `valkey.go`
Standalone + every cluster member now: install pmm-client → run pmm-agent in the background
(§49) → **create the read-only `pmm` ACL user** (`ACL SETUSER pmm on >$PMM_PASSWORD ~*
+@read +info +config|get +slowlog +latency`, per the Percona valkey-redis doc) → **`pmm-admin
add valkey <node> 127.0.0.1:6379 --username=pmm --password=$PMM_PASSWORD [--cluster=<frame>]`**.
Unified into one `valkeySetupPMM` helper used by both paths (fixes the cluster members not
running pmm-agent / not being added). Verified the ACL live (pmm user: INFO ok, writes
denied). PMM_PASSWORD comes from the env (default pmm_password).

`go build`/`vet`/`gofmt -l` + web build pass. (Full PMM join/dashboards need a live stack.)

## 51. PMM context-menu consoles + reconcile stale container id after Watchtower upgrade

Follow-ups to §50:

### Right-click "Enter root console" on a PMM node — `StackDesigner.jsx`
The §50 root/pmm split was only on the property panel; the canvas **right-click menu** still
had a single "Enter root console" that execs as the default (pmm) user. The node context
menu now special-cases PMM: **Enter root console** (`user=0`) + **Enter PMM console**
(default). Other node types keep the single root console (their exec default is already root).

### Console/cert broken after a Watchtower PMM upgrade — `intranet_mgmt.go` + `pmm_mgmt.go`
Watchtower upgrades by **deleting the old PMM container and creating a new one** (same name,
**new id**). dbcanvas had the *old* id persisted, so exec-based features failed with
`docker exec create: No such container: <old-id> (404)` — the console wouldn't open and the
Certificate tab errored. Added `reconcileContainerID`: on each management call it re-resolves
the container **by name** (which Watchtower preserves) via `ContainerByName` (exact `^/name$`
filter, `all=true`) and persists the refreshed id if it drifted. Wired into both
`loadRunningNode` (terminal, email, LDAP, …) and `loadRunningPMM` (cert), so the console and
cert tab work again after an upgrade with no redeploy. Verified live that a delete+recreate
under the same name resolves to the new id.

`go build`/`vet`/`gofmt -l` + web build pass.

## 52. Remove the PMM /srv volume on stack destroy

The §50 PMM `/srv` data volume is a *named* volume, so `ContainerRemove` (which only drops
anonymous volumes) left it behind when a stack was destroyed — leaking one volume per PMM
node across deploy/destroy cycles. `teardownStack` now also calls `docker.VolumeRemove`
(new best-effort `DELETE /volumes/<name>?force=true`) for each deployment, using the shared
`pmmDataVolume(stackID, nodeID)` name. The name is namespaced (`dbcanvas-pmm-…`) so it's a
no-op for non-PMM nodes. Verified the volume lifecycle: it survives container removal,
removes cleanly once the container is gone, and removing a missing volume is harmless.
`go build`/`vet`/`gofmt -l` clean.

## 53. Data Generator feature (PostgreSQL) + nav cleanup

Removed the four demo pages (Interactions/Controls, Node Editor, Data Table, Kanban) and
their nav entries; added a new **Data Generator** nav entry directly below Database Stacks.

New feature: generate realistic test data for tables in databases provisioned by Database
Stacks. This session ships the full **PostgreSQL** slice (pg / patroni / repmgr nodes);
MySQL/PXC and advanced options are designed in `docs/DATA_GENERATOR.md`.

Architecture — all SQL runs via `docker exec psql` inside the node container using the
deployment's stored superuser secret (works whether or not 5432 is published; no DB driver
on the app network). Introspection queries return JSON (`json_agg`) unmarshalled in Go.

- `app/datagen.go` — `pgConnFor`/`pgQueryJSON`/`pgExec`; connections list (running pg-family
  nodes across the user's stacks); databases/tables introspection; `tableMeta` (columns via
  `pg_attribute`/`format_type` → type, nullability, default, identity/generated, char len,
  numeric precision/scale, **pgvector dimension**, **enum labels**; PK/unique via `pg_index`;
  single-column **FKs** via `pg_constraint`; **TimescaleDB** hypertable + time column, with
  the extension-absent error treated as "not a hypertable").
- `app/datagen_gen.go` — generator IDs, `generatorChoices`, `inferGenerator` (DB-managed →
  default; FK → sampler; vector → embedding; enum → labels; hypertable time col; name
  regexes; type fallback), and `value()` emitting a SQL literal per row (length-clipped,
  scale-aware; vector `'[…]'::vector`; FK picks from the sampled pool).
- `app/datagen_data.go` — realistic-data libraries + `mustRe`.
- `app/datagen_job.go` — request config, **FK pre-sampling** (`quote_nullable` → ready
  literals; fatal if a NOT NULL FK's parent is empty), preview (10 rows, no writes), and the
  generation engine: N workers (1–16) pull mutex-guarded batches, each builds one multi-row
  `INSERT … VALUES` run with `ON_ERROR_STOP=1`; atomic progress; `stopOnError` cancels;
  progress + cancel endpoints. Per-worker seeded RNG.
- Routes in `main.go` under `/api/datagen/…`.
- Frontend `app/web/src/pages/DataGenerator.jsx` + `lib/datagenApi.js`: connection → db →
  table wizard, per-column generator template with comboboxes + inline options + skip, run
  options (rows/batch/workers/FK sample/seed/stop-on-error), preview table, and live job
  progress (rows/s, elapsed, ETA, errors) with cancel.

Verified live against `pgvector/pgvector:pg16` with a schema exercising identity, generated
column, `varchar(50)`, numeric(10,2), enum, `vector(3)`, and a FK: the column/FK/tables
queries return correct JSON (identity/generated/vector-dim/enum/char-len all detected); FK
sampling via `quote_nullable` yields insertable literals; a generated multi-row INSERT with
vector/enum/NULL/bool/FK succeeds and the DB auto-fills identity + generated columns. The
hypertable query errors cleanly when TimescaleDB is absent (ignored). `go build`/`vet`/
`gofmt -l` and `npm run build` clean. Design: `docs/DATA_GENERATOR.md`.

**Fix — connections stuck on "Loading…":** the connections handler built its result with
`var out []dgConnection`; when empty, a nil slice marshals to JSON `null`, and the page
(`conns === null` ⇒ "Loading connections…") never advanced. Now `out := []dgConnection{}`
so an empty result serializes to `[]` → the page shows "No running PostgreSQL nodes". The
page also normalizes the response (`Array.isArray(d) ? d : []`) and, on a failed request
(e.g. the Go backend not yet rebuilt/restarted so `/api/datagen/*` 404s), sets `conns=[]`
and shows an actionable error instead of spinning forever.

**Fix — connections always empty (found via live server):** the connections handler ran
`buildDoc` over stacks from `ListStacks`, but `ListStacks` doesn't select `design_json`
(only `GetStack` does), so every stack had an empty design ⇒ zero nodes ⇒ `[]`. Now the
handler reloads each stack via `a.store.GetStack(s.ID)` before scanning nodes.

**Fix — psql peer authentication (found via live server):** `pgQueryJSON`/`pgExec` ran
`docker exec psql -U postgres`, which runs as root; the pg image uses `--auth-local=peer`,
so `psql: FATAL: Peer authentication failed for user "postgres"`. Switched both to
`docker.ExecAs(ctx, id, "postgres", …)` (matching the `runuser -u postgres` pattern used
elsewhere) so psql runs as the postgres OS user and authenticates over the local socket
without a password. Verified end-to-end against a live deployed stack: connections →
databases → columns (correct inference incl. identity/FK) → generate 200 rows, 0 errors,
valid FK values.

**Fix — UNIQUE-column collisions + arg-length limit (found generating into a table with a
UNIQUE email):** two problems surfaced at scale.
1. *Duplicate keys.* Generators didn't guarantee uniqueness, and each worker's row index
   restarted at 0 (overlapping across workers), so a UNIQUE column (e.g. `email`) hit
   `duplicate key value violates unique constraint` — and since a batch is one multi-row
   INSERT, one dup failed the whole batch. Now `take()` hands out **globally unique row-index
   ranges**, and `colGen.value` embeds a per-job nonce + that index into UNIQUE/PK string
   values (`uniquify`: before `@` for emails, else before the closing quote).
2. *`exec /usr/bin/psql: argument list too long`.* Batches were passed via `psql -c`
   (argv), so wide rows × large batch exceeded the OS `execve` limit. Added
   `docker.ExecInput` (attaches stdin, half-closes, demuxes output) and switched `pgExec` to
   `psql -f -`, piping the SQL over **stdin**. Verified live: 200,000 rows into
   `sample.public.sample_customers`, batch 2000 × 6 workers, **0 errors, 200k distinct
   emails, ~89k rows/s**.

## 54. Data Generator — MySQL/PXC engine

Added the MySQL/PXC engine alongside PostgreSQL. The generator library, inference rules, FK
sampler, uniqueness enforcement, worker/batch engine, progress, and the whole frontend
wizard are shared; only the SQL dialect + client differ.

- Engine dispatch: `pgConn` → `dbConn{Engine,…}`; `engineForType` maps node types
  (`pg`/`patroni`/`repmgr`→postgres, `pxc`/`ps`/`mysql`/`innodb`→mysql). `dbConnFor` loads
  `pgSecrets` or `pxcSecrets` (RootUser/RootPassword) accordingly. `pgQueryJSON`/`pgExec`
  became `queryJSON`/`execSQL` that branch by engine; MySQL uses the `mysql` client
  authenticating as root via `MYSQL_PWD` (no password on argv), reading SQL from stdin.
- Introspection: `tableMeta` dispatches to `pgTableMeta` (pg_catalog) or the new
  `myTableMeta` (`app/datagen_mysql.go`, information_schema → JSON). Handles auto_increment
  (→ isIdentity), generated columns, `COLUMN_KEY` PK/unique, single-column FKs, and parses
  enum/set members from `COLUMN_TYPE`. Connections + databases + tables handlers now branch
  by engine (MySQL: `information_schema.schemata`/`.tables`; a schema *is* a database).
- Dialect: `qIdent(engine,…)` backticks vs double-quotes; FK sampling uses `QUOTE()`+`RAND()`
  (MySQL) vs `quote_nullable()`+`random()` (pg); JSON literals skip the `::jsonb` cast on
  MySQL. Type-safe numeric ranges (`intMax`/`decMax`) so values never overflow the column
  type (e.g. MySQL `tinyint`), and `tinyint(1)` infers as boolean.
- Frontend copy generalized (PostgreSQL & MySQL/PXC); each connection chip shows its engine.

Verified the MySQL SQL path live against `percona/percona-server:8.0` with a table exercising
`auto_increment`, a `STORED` generated column, `varchar`, `tinyint`, `tinyint(1)`, `enum`,
`set`, `decimal(12,2)`, `text`, `timestamp`, and a FK: databases/tables/columns introspection
return correct JSON (booleans as JSON true/false → bool fields; enum/set members parsed);
FK sampling via `QUOTE()` yields insertable literals; a generated multi-row INSERT (backtick
idents, `tinyint(1)`→bool, enum/set, decimal, FK) succeeds and MySQL auto-fills the
auto_increment id + generated column. `go build`/`vet`/`gofmt -l` and `npm run build` clean.
(The shared app orchestration — `ExecInput` stdin, workers, uniqueness — is already
end-to-end-verified on PostgreSQL; full app-path confirmation for MySQL wants a deployed
PXC/PS/InnoDB node.) Design: `docs/DATA_GENERATOR.md`.

## 55. Notifications (bell) — phase 1 of the dashboard/notifications plan

Replaced the decorative top-right bell with a live notification center backed by a persisted
event store and a Server-Sent-Events stream.

- Store: new `notifications` table (`user_id, scope, type, severity, title, body, stack_id,
  node_id, job_id, read_at, created_at`) + `CreateNotification`/`ListNotifications`/
  `CountUnread`/`MarkNotificationRead`/`MarkAllRead`. Scope: a user sees rows where
  `user_id` = them; an admin sees all rows.
- `app/notifications.go`: an in-memory SSE hub (`notifBus`) with per-subscriber user/admin
  filtering; `a.notify` (persist + publish) and `a.notifyStack` (resolve owner via GetStack);
  handlers `GET /api/notifications`, `GET /api/notifications/stream` (SSE, 25s heartbeat),
  `POST /api/notifications/{id}/read`, `POST /api/notifications/read-all`.
- Emit hooks (central choke points): `pxcProg.fail` → per-node deploy failure (covers every
  provisioner); `handleDeployStack` → "Deployment started"; `teardownStack` → "Stack
  destroyed" (also fires on TTL reap); `runGenJob` defer → data-gen completed / failed /
  canceled (with row count / error reason).
- Frontend: `lib/notifApi.js` + `NotificationBell` in `App.jsx` — unread badge, dropdown
  with severity dots + relative time, click-through routing (datagen→Data Generator,
  stack→Database Stacks), mark-all-read, and a live `EventSource` subscription (browser
  auto-reconnect).

Verified live against the running app + deployed stack: a data-gen run produced a
`datagen.done` (success) notification delivered both via `GET /api/notifications` and pushed
over the SSE stream; an empty-parent FK run produced a `datagen.error` notification with the
reason; mark-all-read cleared the unread count. `go build`/`vet`/`gofmt -l` + `npm run build`
clean. Still to come per the plan: dashboard summary counters (admin vs. user) and
focus-gated live OS stats, then extended event types (TTL/backups/watchtower/thresholds).

## 56. Dashboard (summary + focus-gated live stats) + extended events — phases 2–4

Replaced the mock Dashboard with real, scope-aware data and added the remaining event types.

- `app/dashboard.go`:
  - `GET /api/dashboard/summary` — cheap, store-derived: stack counts (deployed/draft/
    expired), node counts by state, running DB nodes by engine, nodes by type, in-memory
    data-gen job counts, recent activity (notifications), and (admin only) user total +
    pending-approval count. Admin sees all stacks; a user sees only their own.
  - `GET /api/dashboard/stats` — **focus-gated** live OS stats. `sampleStats` calls Docker
    `/containers/{id}/stats?stream=false` concurrently (worker pool of 6) for managed
    running containers, cached ≤2s. Because it only runs when a client hits the endpoint
    (and the client only polls while the dashboard tab is visible/focused), there is **zero
    background sampling** when nobody is watching. Returns aggregate CPU%/mem + top-N by CPU,
    filtered to the user's own containers (admin: all **DB-tracked** stacks — orphaned
    containers whose stack no longer exists in the DB are excluded), mapped via the
    `dbcanvas-<stackID>-` name prefix.
- `app/docker.go`: `ListManaged` (managed stack containers) + `ContainerStats` (CPU% from
  cpu/precpu deltas × online CPUs; mem = usage − reclaimable cache; net rx/tx; block-IO
  read/write from `blkio_stats.io_service_bytes_recursive`).
- Frontend `Dashboard.jsx` rewrite + `lib/dashApi.js`: scope badge, live indicator, counters
  (stacks/nodes/containers/CPU/memory/users-or-jobs), five ranked horizontal **bar charts**
  (`TopBars`, HTML/CSS — crisp font, animated, color-accented): Top containers by CPU, and
  per-node Top network in / out and Top disk in / out (bytes/s derived by diffing consecutive
  samples client-side), plus by-engine / by-type breakdowns and a real activity feed. The `useFocusGatedInterval` hook polls only while
  `document.visibilityState==='visible' && document.hasFocus()`, and stops on blur/hide and
  on unmount (leaving the page). (The stats endpoint returns the full per-node list; the
  client ranks each table.)
- Extended events (phase 4): admin "New account awaiting approval" on register; owner "Stack
  expiring soon" ~15 min before TTL reap (reaper `warnExpiringStacks`, warn-once via
  `sync.Map`); "Backup completed" on pg / patroni / repmgr / PBM on-demand backups;
  "High resource usage" alerts from the stats sampler (CPU or mem ≥90%, 10-min per-container
  cooldown, emitted to the owner).

Verified live: summary returned correct counts for the deployed stack (19 nodes running,
byEngine mysql 10 / postgres 7, byType breakdown, users 1/pending 0) with the activity feed;
stats returned a real Docker sample (23 managed containers, meaningful non-zero CPU%,
21 GB/725 GB memory, top-by-CPU). `go build`/`vet`/`gofmt -l` + `npm run build` clean.

---

## 57. repmgr: boot-persistent via the packaged unit + single config location — `app/repmgr.go`

A live repmgr cluster showed **automatic failover was silently off**: `repmgrd` was
"enabled" but **inactive (dead)**. Root cause: our hand-rolled `repmgrd.service` was
`Type=simple`, but **repmgrd forks to daemonize** — so ExecStart exited `0/SUCCESS`
immediately and systemd marked the unit dead (`repmgr daemon status` showed the standbys as
`repmgrd: not running`). Compounding it, the config lived in **two places**: our
`/etc/repmgr.conf` (the one actually used) and the PGDG default `/etc/repmgr/<major>/repmgr.conf`
(shipped by the package, unused) — confusing to operate.

Fix — stop reinventing the unit; use the PGDG-packaged one, which is `Type=forking` with a
pidfile and already reads `/etc/repmgr/<major>/repmgr.conf`:

- **Single config location.** New helpers `pgRepmgrConfDir(major)` / `pgRepmgrConfPath(major)`
  → `/etc/repmgr/<major>` and `/etc/repmgr/<major>/repmgr.conf`. `repmgrPrepareNode` now
  `install -d`s that dir and writes the config **there** (not `/etc/repmgr.conf`).
  `repmgrConf()`'s `promote_command`/`follow_command`, the primary-register, standby
  clone/register scripts, and the chown all take the path via a `CONF=` env — no `/etc/repmgr.conf`
  reference remains. (The §25 config comment "renders /etc/repmgr.conf" is obsolete.)
- **Boot-persistent daemon via the packaged unit.** `repmgrdStartScript` was rewritten: it
  removes any stale hand-rolled `/etc/systemd/system/repmgrd.service`, and enables+starts the
  **packaged** unit — on EL `repmgr-<major>.service`, on Debian `repmgrd.service` (first
  flipping `/etc/default/repmgrd` `REPMGRD_ENABLED=yes` + `REPMGRD_CONF=$CONF`). It picks
  whichever unit exists (`systemctl list-unit-files`), `systemctl enable --now`s it, and
  verifies `is-active`. Phase 4 now passes `MAJOR`/`CONF` instead of `BINDIR`.
- **PostgreSQL** was already boot-enabled (`pgStartScript` does `systemctl enable`); this makes
  repmgr match, so both survive a container/host restart.

### Verification performed
- Diagnosed on a **live** 3-node OL9 cluster (stack 119): `postgresql-16` enabled+active but
  `repmgrd` enabled+**inactive**; journal showed repmgrd starting, connecting, then
  `Deactivated successfully` (exit 0). Manually pointing config at `/etc/repmgr/16/repmgr.conf`
  and `systemctl enable --now repmgr-16` brought the daemon up on all three nodes
  (`repmgr daemon status`: all `running`, standbys `Upstream last seen: 0 second(s) ago`).
- **Fresh redeploy from the new code** (rebuilt image, destroy→deploy stack 119): all nodes
  reached `running`; `/etc/repmgr.conf` **absent**; `/etc/repmgr/16/repmgr.conf` present with
  `-f /etc/repmgr/16/repmgr.conf` in promote/follow; `postgresql-16` **and** `repmgr-16` both
  `enabled`+`active`; the old `repmgrd.service` gone; `repmgr daemon status` showed all three
  nodes running repmgrd with live upstream monitoring.
- `go build`/`vet`/`gofmt -l` clean.

## 58. Terminal right-click context menu: Maximize / Minimize / Close — `app/web/src/terminal/TerminalProvider.jsx`

Both terminal surfaces (docked dock tabs and detached floating windows) now expose a
right-click context menu with the three classic window controls. Mapped onto the existing
session model (no new persisted state beyond a `max` flag on `float`):

- **Maximize** — floats the session filling the viewport (`100vw`/`100vh`, square corners,
  move/resize disabled) via `detachTerminal` then `setFloat(id, { max: true })`. **Offered
  only for docked tabs** — once the off-screen drag bug (below) was fixed there was no reason
  to maximize an already-floating window, so the context-menu item is gated on `!s.floating`
  and the floating titlebar's `⛶`/`❐` button was removed. A maximized window is returned to
  normal via **Dock** (or Close).
- **Minimize** — docks the session (clearing `max`, `attachTerminal`) and collapses the dock
  (`setOpen(false)`). Works from either surface.
- **Close** — `closeTerminal(id)`, styled as the danger item.

Implementation notes: `openMenu(id)` sets `menu = { x, y, id }` and `preventDefault`s the
native menu; the menu renders as a fixed positioned list clamped to the viewport, over a
full-screen transparent backdrop that closes it on any click / re-right-click. Floating window
geometry is computed from `f.max` (`geo` object), and the titlebar drag handler is a no-op
while maximized. Existing `floatSlot` ResizeObserver + the per-render `fit()` effect re-fit
xterm when the window grows/shrinks, so no extra fit wiring was needed.

Follow-up — **title bar can no longer be dragged off-screen.** The `fmove` drag applied
the pointer delta with no bounds, so dragging a floating window up past `y=0` hid the title
bar (the only place with the Dock/Maximize/Close controls) with no way to recover it. The
handler now clamps: `y ∈ [0, innerHeight-28]` (top edge always in view) and
`x ∈ [KEEP-w, innerWidth-KEEP]` with `KEEP=64` (at least 64px reachable on either side). The
window width for the clamp is read from the live DOM element at pointer-down
(`parentElement.offsetWidth`) rather than `float.w`, since the window uses native
`resize: both` which changes actual size without updating state.

### Verification performed
- `vite build` clean (48 modules transformed, no errors).

## 59. README screenshots + isolated headless-capture tooling — `README.md`, `docs/screenshots/`, `app/web/scripts/` (gitignored)

The README now showcases the product with real, in-action screenshots woven into the feature
sections: the Database Stacks canvas (deployed 7-node stack), a node management panel, PMM
monitoring the stack, a live per-node web terminal, the Ubuntu VNC desktop, the Data Generator
mid-run (FK-aware), and the live Dashboard. The seven PNGs live in `docs/screenshots/` and are
tracked; a small `docs/screenshots/README.md` indexes them.

**Capture tooling — deliberately isolated and NOT shipped.** `app/web/scripts/screenshots.mjs`
drives a running instance with Playwright's bundled Chromium: it authenticates via the API (the
request client shares the browser context's cookie jar, so pages load authenticated), forces the
theme (`localStorage` init script), and captures at 1440×900 @2x dark. Static mode shots the
hash routes (`#dashboard`/`#stack-designer`/`#data-generator`); with `SHOTS_STACK=<name>` it also
opens the stack canvas, left-clicks a node for its inspector, drives the Data Generator flow
(connection → db → table → Generate) **before** opening a terminal (the terminal dock persists
across navigation and would otherwise cover the page), opens a per-node root console and runs a
`psql \dt`, then logs into the PMM (Grafana) and noVNC UIs on their published ports. Config via
`SHOTS_*` env (base URL, user/pass, theme, viewport, output dir); output path is anchored to the
script's own location via `import.meta.url`.

Crucially the tool lives in **its own package** (`app/web/scripts/package.json`, Playwright as its
only dependency) and the whole `/app/web/scripts/` directory is **gitignored**. This keeps
Playwright out of the app's `package.json`/lock, so the multi-stage Docker build's `npm ci`
(Dockerfile stage 1) never installs it and it never reaches dbcanvas users — the runtime image is
still just the single static Go binary on distroless. Per an explicit privacy request, no tracked
file references the tooling: the `make screenshots` target, README command-table row, and how-to
prose were all removed; only the generated images (and their captions) are committed.

### Verification performed
- Deployed a demo stack (Intranet + PMM + standalone PostgreSQL + 3-node PXC + Ubuntu VNC,
  intranet proxy off) via the API, seeded an FK-rich `shop` schema, and captured all seven shots
  end-to-end; each was visually inspected (authenticated app, correct theme, live data).
- Confirmed `app/web/package.json`/`package-lock.json` carry zero Playwright references and are
  unchanged from HEAD, so `make compose` is unaffected.

## 60. Safer generated passwords + Intranet-proxy default off — `app/intranet.go`, `app/web/src/pages/StackDesigner.jsx`

**Generated passwords no longer contain `!`.** `genSecret(prefix)` (which returns `prefix` + 8
uppercase hex chars and backs all 14 credential prefixes — `PgSuper!`, `PmmAdm!`, `MyRoot!`,
`PxcRoot!`, `LdapAdm!`, `MailAdm!`, `KcAdmin!`, `Valkey!`, `MongoAdm!`, etc.) now
`strings.ReplaceAll(prefix, "!", "^(")`. The `!` separator triggered shell **history expansion**;
`$` would be **variable-interpolated**; `^` and `(` are neither, so passwords stay safe to paste
into terminal / psql / mysql contexts. The change is central (one function), so every current and
future call site is covered. Consumption is already interpolation-safe — secrets are passed as
literal env values to `runStep` (bash does not re-expand a variable's value), psql uses `:'pw'`
quoting, and `.my.cnf`/config files are written as raw bytes via `CopyFile` — so `^`/`(` don't
break provisioning. `genVNCPassword` (8 lowercase hex) and the SeaweedFS secret key (hex) never
used specials and are unchanged. Existing deployed stacks keep their stored passwords (genSecret
only runs when minting a *new* secret), so there's no migration or breakage.

**Intranet proxy defaults to off.** All 14 node-creation templates in `StackDesigner.jsx` flipped
`useProxy: true → false`, so the "route package egress via the Intranet Squid proxy" checkbox is
unchecked by default on every node type. There is no fallback default that would re-enable it, and
saved designs keep their existing value; only newly added nodes default off.

### Verification performed
- `go build ./...`, `go vet`, `gofmt -l` clean; ran `genSecret` standalone to confirm output
  contains no `!` (e.g. `PgSuper^(A02FB5C6`).
- `vite build` clean; confirmed no `useProxy: true` remains in `StackDesigner.jsx` and no
  `useProxy ?? true` / `|| true` fallback exists.

## 61. Skip pmm-client install on unmonitored nodes — all provisioners

Previously every node/cluster **always** installed `pmm-client` during provisioning (comments:
"so monitoring can be enabled later without a reinstall"), even when no PMM server was
associated. Now the install is gated on the same `PMMNodeID != ""` condition already used to
decide whether to register with PMM: **no monitoring → pmm-client is never installed.**

The upfront `pmm-client` install step in each provisioner is wrapped in the node/cluster's PMM
gate:

- **Standalone / single nodes** (`n.PMMNodeID`): `pg.go` (PostgreSQL), `haproxy.go`,
  `proxysql.go` `provisionProxySQLInstance` (via `p.PMMNodeID`).
- **Cluster frames** (`frame.PMMNodeID`): `pxc.go`, `innodb.go`, `patroni.go`, `repmgr.go`,
  `proxysql.go` `proxysqlPrepareMember`.
- **Shared prepare helpers** gated on `frame.PMMNodeID` — `mysqlPrepareNode` (covers the MySQL
  replication frame *and* standalone Percona Server, which passes a synthetic frame carrying
  `PMMNodeID`) and `mongoPrepareNode` (RS / sharded / standalone, standalone likewise passes a
  synthetic frame).
- **Valkey** already gated its pmm-client install behind `n.PMMNodeID` / `frame.PMMNodeID` via
  `valkeySetupPMM`, so it needed no change.

Monitored nodes are unaffected — the gate is true, so the install runs exactly as before, then
registration proceeds. Enabling monitoring later (redeploy with a PMM node associated) re-runs
provisioning, which installs pmm-client then; the `*PMMAdd` register scripts also keep their
`command -v pmm-admin || install` on-demand fallback. Obsolete "always installed" comments were
replaced with the new intent.

### Verification performed
- `go build ./...`, `go vet`, `gofmt -l` clean.
- Statically confirmed all 11 `runStep(..., pmmScript/pmmInstall, ...)` install invocations are
  now preceded by a `PMMNodeID != ""` gate (script-checked, one per provisioner + valkey via its
  gated helper).
- Not runtime-verified (would require an image rebuild + deploy); the change is a conditional
  wrap on the exact variable already governing PMM registration.

## 62. PMM uses a dedicated least-privilege monitoring account per engine — `.env.example`, `app/pxc.go`, `app/patroni.go`, `app/pg.go`, `app/mongodb.go`

Per the Percona PMM docs (connect-database), the `pmm-admin add <engine>` step should connect
as a **dedicated, least-privilege monitoring user**, not root/superuser. Previously MySQL-family
nodes registered as **root** and PostgreSQL-family nodes as the **postgres superuser**. Now every
monitored node uses a dedicated **`pmm`** account whose password defaults to **`PMM_PASSWORD`**
(new in `.env.example`, default `pmm_password`; already wired through compose).

- **`.env.example`** — added `PMM_PASSWORD=pmm_password` with a comment; clarified that
  `MONITOR_PASSWORD` is ProxySQL's health-check user, not PMM's.
- **MySQL family** (`pxcPMM{RHEL,Debian}`, shared by PXC / MySQL replication / InnoDB-GR /
  standalone Percona Server via `pxcPMMExec`): the register script now creates
  `'pmm'@'%'` via root — `CREATE USER … IDENTIFIED BY '$PMM_PW' WITH MAX_USER_CONNECTIONS 10` +
  `GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.*` (+ `SELECT` on
  `performance_schema`), then `pmm-admin add mysql --username=pmm --password=$PMM_PW`. On PXC the
  DDL replicates cluster-wide; `IF NOT EXISTS` + `ALTER USER` keep it idempotent on every node.
  `pxcPMMEnv` now also passes `PMM_PW` (root creds stay, to create the account).
- **PostgreSQL family** (`patroniPMM{RHEL,Debian}`, shared by pg standalone / Patroni / repmgr):
  the register script creates a `pmm` role **on the primary only** (guarded by
  `pg_is_in_recovery()`; the role replicates to standbys), `WITH LOGIN SUPERUSER PASSWORD :'pw'`
  as the docs recommend, via the proven `runuser -u postgres -- psql` + **stdin** pattern (psql
  only expands `:'pw'` on stdin, never `-c`; Patroni's pg_hba is `local all all trust`). Then
  `pmm-admin add postgresql --username=pmm`. `DB_USER/DB_PW` were dropped from the PG env builders
  (`patroniRegisterPMM`, `pgRegisterPMM`) since peer auth needs no superuser password; `PMM_PW`
  added. `pg_hba` already allows the pmm role's `host … 127.0.0.1` connection.
- **MongoDB** — already created a dedicated `pmm` user with an appropriate role (custom
  `pmmMonitor` + `read@local` + `clusterMonitor`, plus `directShardOperations` on 8.x, matching
  the docs); only its password default changed from `genSecret("MongoPMM!")` to
  `envOr("PMM_PASSWORD", "pmm_password")` (3 sites: RS frame, sharded frame, standalone).
- **Valkey** already used a dedicated read-only `pmm` ACL user with `PMM_PASSWORD` — unchanged.
- **ProxySQL / HAProxy** monitor a proxy/LB (ProxySQL admin interface, HAProxy stats), not a
  database, so the DB-account recommendation doesn't apply; left as-is.

Combined with §61 (skip pmm-client when unmonitored), the `pmm` account is created only on nodes
associated with a PMM server. Existing deployments keep working: for Mongo the password is reused
across redeploys if already stored; MySQL/PG create/refresh the `pmm` account on (re)deploy.

### Verification performed
- `go build ./...`, `go vet`, `gofmt -l` clean.
- `bash -n` syntax-checked the rewritten MySQL and PostgreSQL register scripts (heredoc + guarded
  role creation) — both OK.
- Not runtime-verified (needs an image rebuild + PXC/PG/Mongo deploy with a PMM node).

## 63. Dashboard: "Top containers · By memory" panel — `app/web/src/pages/Dashboard.jsx`

Added a memory ranking next to the CPU one. The first dashboard row is now three equal columns —
**Top containers · By CPU**, **Top containers · By memory**, **By engine** — instead of a
double-width CPU card + engine card. The new card reuses the existing `TopBars` component and the
`bars(stats?.nodes, 'memUsed', fmtBytes)` helper (per-node `memUsed` already ships in each
`ContainerStat`), sorted desc, top 5, formatted as bytes to match the Memory stat tile; accent
`var(--color-accent)` to distinguish it from CPU's primary.

### Verification performed
- `vite build` clean.

## 64. Fix: PMM registration broke when the PMM password had URL-unsafe chars — `app/pmm.go` + all PMM register scripts

Symptom: MySQL/PXC, MongoDB, ProxySQL, HAProxy and standalone/Patroni/repmgr PostgreSQL all
failed to connect to PMM (`pmm-admin status`: "pmm-agent is running, but not set up"). Valkey was
unaffected.

Root cause: every register script ran `pmm-admin config --server-url="https://$PMM_USER:$PMM_PASS@$PMM_FQDN:8443"`,
embedding the PMM admin password **unencoded** in the URL. After §60, generated passwords contain
`^` (e.g. `PmmAdm^(…`), and `^` is illegal in URL userinfo, so `pmm-admin` aborted with
`net/url: invalid userinfo` — and `set -e` killed the whole register before `pmm-admin add`. Valkey
survived because it uses `pmm-agent setup` with **separate** `--server-username/--server-password`
flags (no URL). `pmm-admin config` only accepts `--server-url`, so the URL must be encoded.

Fix: build the server URL in Go with proper percent-encoding and pass it as `PMM_URL`:

- New helper `pmmServerURL(fqdn, user, pass)` in `app/pmm.go` uses `net/url` (`url.UserPassword` +
  `url.URL.String()`), so `^`→`%5E`, `(`→`%28`, and any other special char is encoded.
- The six PMM register env builders (`pxcPMMEnv`, `patroniRegisterPMM`, `pgRegisterPMM`,
  `mongoRegisterPMM`, `proxysqlRegisterPMM`, HAProxy's) now also pass
  `PMM_URL=` + `pmmServerURL(...)`.
- All 10 `pmm-admin config` lines (pxc/patroni/mongodb/proxysql/haproxy, RHEL+Debian) now use
  `--server-url="$PMM_URL"` instead of the hand-built URL. Valkey's `pmm-agent setup` path is
  untouched (already correct).

This is a latent bug independent of §60 — any password with `@`, `/`, `#`, `^`, … (including a
user-set `PMM_PASSWORD`) would have broken the raw URL. Encoding fixes all cases.

### Verification performed
- Reproduced on a live stack (122: PXC + ProxySQL + MongoDB + PMM): `pmm-admin config` with the
  raw URL failed `invalid userinfo`; with the `net/url`-encoded URL it returned "Registered".
- Ran the corrected full registration on each node: MySQL (`Connected: true`, mysqld_exporter +
  slowlog agent **Running**), MongoDB and ProxySQL services added with exporters. `go build`,
  `go vet`, `gofmt` clean; Go helper output matches the verified-working encoded URL.

## 65. Fix: cross-cluster replication broke on colliding server-ids — `app/pxc.go`, `app/mysql.go`, `app/innodb.go`

Symptom: an async replication link from a MySQL replication cluster's primary (`mysql01`) to a
PXC node (`pxc01`) never started — the replica log repeated
`MY-013117 … source and replica have equal MySQL server ids`.

Root cause: `mysqlServerID`, `pxcServerID` and `innodbServerID` each derived the server-id from
only the **trailing number** of the node name (stripping the engine prefix), so `mysql01`→1 and
`pxc01`→1 (and 2↔2, 3↔3). Distinct clusters therefore reused the same server-ids, and MySQL
refuses replication between two servers that share one. (validateStack already *warned* "rename
one so the ids differ", but nothing enforced it — poor UX.)

Fix: a shared `serverIDFor(host)` hashes the **full**, stack-unique hostname (labels are unique
across a stack) into a stable server-id in `1..~0xFFFFFFF`, so `mysql01` and `pxc01` no longer
collide. All three per-engine functions now delegate to it. Collision probability is negligible
for a stack's handful of nodes, and the existing validateStack warning remains as a safety net.

### Verification performed
- Reproduced on live stack 123: `pxc01` and `mysql01` both had `server_id=1`; the replica I/O
  thread died with MY-013117.
- `serverIDFor` gives distinct ids (mysql01=221100480, pxc01=83638004, …).
- Hotfixed the running `pxc01` (`SET GLOBAL server_id`) + re-ran the app's exact channel setup:
  `Replica_IO_Running: Yes`, `Replica_SQL_Running: Yes`, `Seconds_Behind_Source: 0`, GTID set
  retrieved from the source, no errors. `go build`/`vet`/`gofmt` clean.

## 66. Root password from ROOT_PASSWORD env (fixes cross-cluster replication) — `.env.example`, `docker-compose.yml`, `app/pxc.go`, `app/mysql.go`, `app/innodb.go`

Symptom: cross-cluster replication (e.g. a bidirectional link between two PXC clusters) would
not sync. On the affected stack, `root@localhost` could not be authenticated with the node's
*stored* password on the MySQL/PXC nodes, so the replication reconcile — which runs
`mysql -uroot -p"$ROOT_PW" …` on each replica to configure/start channels — could not reliably
manage the channels, and bidirectional sync never came up.

Root cause / change: MySQL-family root passwords were auto-generated per cluster
(`genSecret("PxcRoot!")` / `genSecret("MyRoot!")`), which produced random, per-cluster values
(and, post-§60, ones containing `^(`). Now every MySQL-family node defaults its root password to
a single, deterministic, known value from **`ROOT_PASSWORD`** (default `root_password`), matching
the existing `APP_PASSWORD`/`REPL_PASSWORD`/… convention:

- **`.env.example`** + **`docker-compose.yml`**: add `ROOT_PASSWORD` (default `root_password`).
- **PXC** (`pxc.go`), **MySQL replication** (`mysql.go` frame), **standalone Percona Server**
  (`mysql.go`), **InnoDB/GR** (`innodb.go`): the root-password fallback changed from
  `genSecret(...)` to `envOr("ROOT_PASSWORD", "root_password")`. Precedence is unchanged:
  stored secret (redeploy) → explicit canvas value (`frame.RootPassword`) → `ROOT_PASSWORD`.

### Verification performed
- Reproduced on a live multi-cluster stack: PXC nodes rejected root auth with the stored
  password, and a `pxc03 ↔ pxc02` bidir link was not syncing.
- Rebuilt the image and deployed a fresh two-cluster PXC bidir stack:
  - root auth works on both nodes with `root_password` (`SELECT @@server_id` → 137889959 /
    137442225 — also distinct, per §65);
  - both bidir channels (`xrepl_pxca1`, `xrepl_pxcb1`) show `Replica_IO_Running: Yes` +
    `Replica_SQL_Running: Yes`, no errors;
  - a row inserted on pxca1 appears on pxcb1 and a row inserted on pxcb1 appears on pxca1 —
    both nodes show both rows.
- `go build`/`vet`/`gofmt` clean. Existing stacks keep their stored passwords; the fix applies to
  new deploys (a broken existing stack must be redeployed to pick up a known root password).

## 67. Fix: weak `ROOT_PASSWORD` rejected by `validate_password` on first root set — `app/mysql.go`, `app/pxc.go`

Symptom (OL9 stack, deploy retried 10×): attempt 1 failed with
`ERROR 1819 (HY000) … Your password does not satisfy the current policy requirements`, then
attempts 2–10 all failed with `ERROR 1045 (28000): Access denied for user 'root'@'localhost'
(using password: NO)`. No MySQL/PXC node ever got a usable root password, so bootstrap and the
whole cross-cluster mesh could not come up.

Root cause: §66 made the default root password the weak `root_password` (all-lowercase, no digit
or special char). On RHEL/OL, Percona Server ships the **`validate_password` component at
`MEDIUM`/length 8** (confirmed live: `SELECT COMPONENT_URN FROM mysql.component` →
`component_validate_password`; `@@validate_password.policy=MEDIUM`, `.length=8`). The very first
root set happens from the **expired temporary password**, where *only* `ALTER USER` is permitted —
so we cannot relax the policy first, and `ALTER USER … IDENTIFIED BY 'root_password'` is rejected
(→ 1819). The scripts begin with `rm -f "$LOGERR"`, so on each retry the temporary-password log
line is deleted and the datadir (already initialized) issues no new one; `TMP` comes up empty and
control falls into the else/`mysql -uroot` (no-password) branch — which on RHEL is *not*
auth_socket — producing the "using password: NO" cascade for attempts 2–10.

Fix (both shared scripts, `mysqlSetRootPW` in `mysql.go` and the inline `pxcBootstrapScript` in
`pxc.go`):
- **Expired-temp path**: try `ALTER USER … BY '$ROOT_PW'` first; if the policy rejects it, set a
  strong **interim** password `Dbc#Interim7Pw` (satisfies any default policy; also clears the
  password-expired flag), then — now a full, non-expired root — `SET GLOBAL
  validate_password.policy=LOW; …length=6`, then `ALTER USER … BY '$ROOT_PW'`.
- **Debian auth_socket path**: we are already a full (non-expired) root over the local socket, so
  relax `validate_password` *before* setting the password (handles the case where Debian also
  ships the component).

Also reviewed (per the report) the "complex" canvas — stack 127 `StackBest`: 5 Percona Server
replication clusters (mysql01–15) + 2 PXC clusters (pxc01–06) wired into one cross-cluster mesh
of async + bidirectional links (incl. a multi-source node, `mysql07`, replicating from both
`mysql01` and `pxc01`, and relay chains like `mysql07 → mysql01 → mysql04`). Confirmed the design
handles it: `serverIDFor` (§65) yields **21/21 unique server-ids** across all nodes;
`log_replica_updates=ON` is set unconditionally on every MySQL/PXC/InnoDB node so relay chaining
forwards writes; GTID + unique ids protect the bidir cycles from loops; per-source named channels
(`xrepl_<host>`) give each multi-source replica an independent channel; and `validateStack` warns
on any endpoint server-id collision.

### Verification performed
- Reproduced live on `dbcanvas-127-mysql-mr46380r-3` (`mysql01`): root could not log in with
  `root_password` and the temp-password log line was gone — the exact stuck state.
- Confirmed the policy is the cause (`validate_password` = `MEDIUM`/8 via a `--skip-grant-tables`
  boot), and that the fix sequence works: relaxing to `LOW`/6 then
  `ALTER USER 'root'@'localhost' IDENTIFIED BY 'root_password'` succeeds and
  `mysql -uroot -proot_password` logs in (recovered `mysql01` to a working state via an
  `--init-file` one-shot).
- `go build ./...` (from `app/`) clean. The remaining broken nodes need a redeploy with the
  rebuilt binary to pick up the fix (fresh datadir → temp password → interim-password path).

## 68. Rename `ROOT_PASSWORD` → `MYSQL_ROOT_PASSWORD` — `.env.example`, `.env`, `docker-compose.yml`, `app/mysql.go`, `app/pxc.go`, `app/innodb.go`

The root-password env var (§66) is renamed to `MYSQL_ROOT_PASSWORD` to make it self-describing
and consistent with the MySQL ecosystem's conventional name. Pure rename, behavior unchanged:

- **`.env.example`** / **`.env`**: `ROOT_PASSWORD=root_password` → `MYSQL_ROOT_PASSWORD=root_password`.
- **`docker-compose.yml`**: `MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-root_password}`.
- **`app/mysql.go`** (frame + standalone Percona Server), **`app/pxc.go`**, **`app/innodb.go`**:
  `envOr("ROOT_PASSWORD", "root_password")` → `envOr("MYSQL_ROOT_PASSWORD", "root_password")`.

Precedence is unchanged: stored secret (redeploy) → explicit canvas value (`frame.RootPassword`)
→ `MYSQL_ROOT_PASSWORD` (default `root_password`). `go build ./...` (from `app/`) clean.

## 69. Fix: cross-cluster GTID replication from an SST-joined PXC source fails 1236 — `app/replication.go`

Symptom (stack 128 `StackTest`, a mesh of 5 PS-replication + 2 PXC clusters): the async links
`pxc03 → mysql07` and `pxc03 → mysql10` never came up, while `pxc01 → mysql01`, `mysql04 ↔ mysql01`
and `pxc04 ↔ pxc01` did. The two failing replicas ended up with **no channel at all**.

Root cause: a GTID-auto channel (`SOURCE_AUTO_POSITION=1`) makes the replica ask the source for
*every* GTID it is missing, starting from the beginning of history. The replica has its own local
GTIDs (its bootstrap/user-creation transactions under its own server UUID), so auto-position asks
the source for the source's *entire* executed set. That works from **pxc01** (the cluster's
*bootstrap* node — it generated every transaction and still has all binlogs, `gtid_purged` empty),
but **fails from pxc03** (and pxc02): those nodes joined via **SST/xtrabackup**, so they never had
the cluster's early binlogs — `pxc03` has `gtid_purged=cded9700…:1-13`. The replica's I/O thread
dies with `ERROR 1236 … the source purged required binary logs … missing transactions are
cded9700…:1-13`. Then, because the failed `replChannelApply` leaves the channel out of the prune
step's `KEEP` list, the half-configured channel is **removed** — hence zero channels.

Fix (`reconcileReplication` + `replChannelApply`): on a **freshly-created** auto channel, seed the
replica's GTID state with the source's current `gtid_executed` so auto-position replicates only
changes made *after* the link — the same "from now on" semantics the file/position path already
documents. Concretely:

- New helper `sourceGTIDExecuted` reads the source's `@@global.gtid_executed` with `mysql -N --raw`
  (`--raw` disables batch-mode escaping — a multi-UUID set is otherwise printed with a literal
  `\n` between UUIDs) and strips whitespace to a single-line set. Passed as `SRC_GTID` to the apply
  step (only on the `auto` path; best-effort — an empty read just falls back to plain auto-position).
- `replChannelApply`, when `AUTO=1` **and the channel does not yet exist**, computes
  `GTID_SUBTRACT($SRC_GTID, @@global.gtid_executed)` and, if non-empty,
  `SET GLOBAL gtid_purged='+<missing>'` before `CHANGE REPLICATION SOURCE`. The channel-exists guard
  is essential: seeding must happen **once at creation**, never on a later reconcile (which would
  wrongly mark transactions committed on the source since as already-applied and skip them).

### Verification performed (live on stack 128)
- Reproduced the exact `1236` on `mysql07←pxc03`; confirmed `pxc01 gtid_purged` empty vs
  `pxc03 gtid_purged=cded9700…:1-13` (SST-joined) — the reason `pxc01` worked and `pxc03` didn't.
- Applied the seeding fix by hand on `mysql07` and `mysql10`: both channels reach
  `Replica_IO_Running: Yes` + `Replica_SQL_Running: Yes`, 0 lag; a table+rows created on `pxc03`
  *after* the link replicate to both (`finalcheck` → `9`). Repaired the live stack's two broken
  links this way and cleaned up the test DBs.
- Note the pre-existing caveat still applies (documented for the file/pos path): "from now on"
  replication does not back-fill schema/data created *before* the link — seed data first if the
  clusters aren't empty. On a fresh deploy the channel is created right after bootstrap (empty
  DBs), so all subsequent schema+data replicate cleanly.
- `go build ./...`, `go vet ./...`, `gofmt -l` all clean. The existing stack was hand-repaired; a
  redeploy with the rebuilt binary applies the fix automatically to new channels.

## 70. All DB/ProxySQL credentials from `.env` + a stack-wide reset barrier before replication — `.env`, `.env.example`, `docker-compose.yml`, `app/pxc.go`, `app/mysql.go`, `app/innodb.go`, `app/patroni.go`, `app/repmgr.go`, `app/pg.go`, `app/valkey.go`, `app/mongodb.go`, `app/proxysql.go`, `app/replication.go`, `app/intranet.go`, `app/main.go`, `app/web/src/pages/StackDesigner.jsx`

Two coupled changes: (1) make **every** database and ProxySQL credential come exclusively from
`.env` (no per-node canvas passwords; a redeploy re-reads `.env`), and (2) fix cross-cluster
replication reliability by resetting **every** MySQL-family server to a clean, empty GTID baseline
*before* any replication link (intra-cluster attach or cross-cluster channel) is set up.

### 70.1 Credentials read exclusively from `.env`

New variables in **`.env`** and **`.env.example`** (defaults shown):

| Var | Default | Applies to |
| --- | --- | --- |
| `MYSQL_ADMIN_PASSWORD` | `admin_password` | new `admin`@`%` remote superuser on every MySQL-family node |
| `POSTGRES_PASSWORD` | `postgres_password` | PostgreSQL superuser (standalone PG, Patroni, repmgr) |
| `VALKEY_PASSWORD` | `valkey_password` | Valkey default-user password (standalone + cluster) |
| `PROXYSQL_ADMIN_PASSWORD` | `admin_password` | ProxySQL 6032 admin password (standalone + cluster) |
| `MONGODB_ADMIN_PASSWORD` | `admin_password` | MongoDB admin user (standalone, replica set, sharded) |

Naming convention (documented in `.env.example`): a `<ENGINE>_*` variable is exclusive to that
engine family; the rest (`APP`/`REPL`/`MONITOR`/`CLUSTER`/`PMM`) are shared where relevant.

- **`admin`@`%` superuser** (new): `root@localhost` cannot connect over TCP, so every MySQL-family
  engine now also creates a network-reachable full-privilege `admin`@`%` from `MYSQL_ADMIN_PASSWORD`.
  `pxcSecrets` gained `AdminUser`/`AdminPassword`; the shared SQL fragment `mysqlAdminUserSQL` and the
  helper `mysqlFamilySecrets()` (in `app/pxc.go`) build the account + secret set for PXC, MySQL
  replication, InnoDB/GR, and standalone Percona Server. Root stays on `MYSQL_ROOT_PASSWORD`.
- **PostgreSQL**: new `pgFamilySecrets()` (`app/patroni.go`) → superuser from `POSTGRES_PASSWORD`,
  internal replication role from the shared `REPL_PASSWORD`. Used by `provisionPG`, Patroni, and
  repmgr (repmgr overrides the repl role name to `repmgr`).
- **Valkey**: `VALKEY_PASSWORD` for standalone + cluster.
- **MongoDB**: admin from `MONGODB_ADMIN_PASSWORD` (the internal keyFile/PMM/PBM secrets stay
  reused across redeploys, since they are non-canvas).
- **ProxySQL**: admin from `PROXYSQL_ADMIN_PASSWORD`. ProxySQL ships with `admin/admin`, so
  `proxysqlStartScript` now connects with whichever works (the target password on a redeploy, else
  the `admin/admin` default) and rewrites `admin-admin_credentials` to the `.env` value (persisted to
  disk). Every downstream step (native-cluster join, `proxysql-admin.cnf`, MySQL-backend wiring, PMM
  registration) threads the real password through.

The **canvas password inputs were removed** (`StackDesigner.jsx`, 12 `<Field>` blocks across
PXC/MySQL/InnoDB/PS/PG/Valkey/Mongo frames+nodes). The old precedence "stored secret → canvas
`RootPassword` → env" is gone; the value is now simply the `.env` var on every deploy. Non-password
identities that must stay stable (InnoDB GR group name, Mongo keyFile) are still reused from stored
config.

### 70.2 Stack-wide reset barrier before replication

Previously each cluster frame provisioned independently and cross-cluster channels were bolted on at
the end, so links were configured against clusters that had each accumulated their *own* GTID history
at different times — fragile, and the reason bidirectional/mesh links intermittently failed to
configure. New strategy: **bring every MySQL-family server up, create its `.env` credentials, reset
its binlog/GTID — and only once ALL of them have reached that baseline, set up replication.**

- **`deployBarrier`** (`app/replication.go`): a per-stack rendezvous stored on `App.barriers`
  (`sync.Map`, added in `app/main.go`), seeded in `handleDeployStack` (`app/intranet.go`) with every
  `pxc` + `mysql` member being provisioned this pass (frames already fully running are skipped, so it
  never deadlocks). `arrive(id)` is idempotent; `wait(timeout)` releases when all arrive or the
  timeout elapses (a stuck node can't hang the deploy).
- **MySQL replication** (`app/mysql.go`): split into `mysqlSetupBaseline` (start → root + `admin` +
  app/repl/monitor/cluster users created **locally on every node** → `RESET`) run in parallel for all
  members, then each member `arrive`s the barrier, the frame `wait`s, and only then
  `mysqlAttachReplica` wires each secondary via `AUTO_POSITION=1`. Creating users locally is
  required because the `RESET` purges them from the binlog, so a secondary attaching from the empty
  primary can't inherit them via replication. Scripts: `mysqlBaselineScript` (users **then** reset)
  and `mysqlAttachScript` (attach only — no start/root/reset); the old
  `mysqlPrimaryScript`/`mysqlReplicaScript`/`mysqlReplicaSemisyncPreScript` were removed.
- **PXC** (`app/pxc.go`): the bootstrap now creates `admin`@`%` and runs `$RESET_CMD` right after
  user creation (before joiners SST, so joiners inherit the empty baseline). Each member `arrive`s the
  barrier once the cluster is formed. Galera formation is intra-cluster "replication" that can't be
  deferred, so only PXC's *cross-cluster* links wait on the barrier.
- **`reconcileReplication`** (`app/replication.go`) waits on the barrier before configuring any
  cross-cluster channel, then (as before) waits for the involved nodes to be `Running`. With every
  cluster at an empty GTID baseline, `AUTO_POSITION` has nothing to back-fill and channels attach
  cleanly (the §69 `gtid_purged` seeding remains as defensive cover for dirty/redeploy cases).
- **InnoDB/GR** manages its own GTID/group formation and does not participate in cross-cluster
  replication, so it is **not** in the barrier — it only picks up the `.env` credential + `admin`
  account changes. ProxySQL has no binlog/RESET; it stays post-barrier (it already waits for its
  backend).

### Verification performed
- `go build ./...`, `go vet ./...`, `gofmt -l` (from `app/`) all clean; `npm run build` (from
  `app/web`) succeeds. Full end-to-end deploy of a cross-replication mesh not yet re-run on live
  Docker — the orchestration/scripts are in place and compile clean; a redeploy exercises them.

---

## 71. Configurable `DEPLOYMENT_TIMEOUT` for dependency-readiness waits — `.env`, `.env.example`, `docker-compose.yml`, `app/main.go` + all provisioners

**Problem.** Large stacks that spin up many containers hit failures like
`associated PXC cluster pxc-cluster-02 did not become ready within 15m0s`. The
per-dependency wait ceilings were hard-coded (5–20m) and too short when dozens of
containers are provisioning concurrently.

**Change.** A single knob governs how long a provisioner waits for a dependency
(an associated cluster, node, or shared service) to become ready before failing
the deploy:
- `deployTimeout()` (`app/main.go`, next to `envOr`) reads `DEPLOYMENT_TIMEOUT`
  (interpreted as **minutes**, positive integer), defaulting to **60**.
- Every dependency-readiness wait now passes `deployTimeout()` instead of a fixed
  duration — `waitIntranet`, `waitSeaweedRunning`, `waitKeycloak`, `waitWatchtower`,
  `mongoWaitPMM`, `waitPXCRunning`, `waitMySQLRunning`, `waitPatroniRunning`,
  `patroniWaitCluster`, `patroniWaitEtcd`, `waitNodeRunning`, and the replication
  reset barriers (`b.wait` / `barrier.wait`). 41 call sites across `app/*.go`.
  (The non-deploy `time.Minute` uses — TTL sweeps, HTTP header timeouts, cert
  validity, dashboard throttles — are untouched.)
- `DEPLOYMENT_TIMEOUT=60` added to `.env` + `.env.example` (documented) and
  forwarded to the app container in `docker-compose.yml` (`${DEPLOYMENT_TIMEOUT:-60}`).

### Verification performed
- `go build ./...` clean from `app/` (removed the now-unused `time` import in
  `app/valkey.go`); `docker compose config` validates.

---

## 72. Fix: intra-cluster MySQL replication forced GTID auto-position on non-GTID frames — `app/mysql.go`

**Problem.** A MySQL-replication frame with **GTID off** failed to attach its
secondaries: `my.cnf` leaves `gtid_mode=OFF` (only set when `frame.GTID`), yet
`mysqlAttachScript` always issued `CHANGE REPLICATION SOURCE … SOURCE_AUTO_POSITION=1`,
which MySQL rejects unless `GTID_MODE=ON` — "trying to set up GTID replication when
GTID is disabled."

**Change (per-link positioning).** Attach now mirrors the cross-cluster path
(`app/replication.go`, which already picks per link via `srcFrame.GTID && dstFrame.GTID`):
- `mysqlAttachScript` branches on `$AUTO` — `SOURCE_AUTO_POSITION=1` when `1`, else
  `SOURCE_LOG_FILE=…, SOURCE_LOG_POS=…`.
- `mysqlAttachReplica` sets `AUTO=1` when `frame.GTID`; otherwise it reads the
  primary's current binlog coordinates (reusing `sourceBinlogPos` +
  `frameMajor`) and passes `LOG_FILE`/`LOG_POS`. Needed the primary's node id, so
  the signature gained `primaryID` (call site passes `primary.ID`).
- So each replica uses GTID only when **both** endpoints have GTID; otherwise binary
  log file/position. In a mixed chain `a(non)→b(gtid)→c(gtid)→d(non)`, `d←c` and
  `b←a` use file/position while `c←b` uses GTID — the cross-cluster path already did
  this; this fix brings the intra-cluster path in line.

### Verification performed
- `go build ./...` + `go vet ./...` clean. Runtime not exercised (needs a deployed
  non-GTID MySQL replication frame); the fix reuses the proven cross-cluster
  binlog-position helpers.

---

## 73. Query Runner (Phase 1) — parallel query orchestration with processlist gating — `app/queryrun.go`, `app/queryrun_run.go`, `app/web/src/pages/QueryRunner.jsx`, `app/web/src/lib/queryrunApi.js`, `app/main.go`, `app/web/src/App.jsx`, `go.mod`, `docs/QUERY_RUNNER.md`

**Feature.** A `#queryrun` page (nav: "Query Runner") that runs one or more SQL
queries **concurrently**, each against a **canvas-provisioned** MySQL/PXC or
PostgreSQL node, with per-query load params (count / threads / time limit) and an
optional **processlist "run condition" gate**. Distinct from a benchmark. Design
recorded in `docs/QUERY_RUNNER_PLAN.md`; usage in `docs/QUERY_RUNNER.md`.

**Backend.**
- **Native TCP drivers** (new deps): `github.com/go-sql-driver/mysql`,
  `github.com/jackc/pgx/v5` (stdlib driver `pgx`) — a deliberate departure from the
  otherwise stdlib-only backend (hand-rolling the wire protocols isn't viable).
- `app/queryrun.go` — targets endpoint + handlers. **Targets are canvas-only,
  owner-scoped** (admins see all); `qrResolveConn` maps a node to its in-network
  host:port (over the shared Docker network — no host ports needed) and network
  account (MySQL `admin@'%'`, Postgres superuser). **Passwords never reach the
  browser.**
- `app/queryrun_run.go` — run registry + engine. Each query opens a pooled
  `database/sql` connection sized to its thread count; a shared atomic counter
  makes **Count total across all threads**; a `context` deadline enforces the time
  limit; latency stats (min/avg/max, reservoir-sampled p95). The **gate** polls the
  target's processlist (`information_schema.PROCESSLIST` / `pg_stat_activity`),
  matches a **Go RE2** pattern, and opens per `no_match`/`match` × `every`/`once`.
  **Self-exclusion:** every statement (load + polls) carries a `dbcanvas-qr` marker
  the gate ignores, so a query never blocks on itself.
- Routes: `GET /api/queryrun/targets`, `POST /api/queryrun/runs`,
  `GET /api/queryrun/runs/{id}`, `POST /api/queryrun/runs/{id}/stop`,
  `GET /api/queryrun/history`. Caps: 16 queries/run, 64 threads/query, 3600 s.

**Frontend.** `pages/QueryRunner.jsx` + `lib/queryrunApi.js`: query cards with a
**Server dropdown** (not manual host/port/creds), count/threads/time-limit, the gate
controls (pattern/condition/check/poll), **+ Add another query**, **Run/Stop**, live
per-query stats (executed/errors/latency/gate state), and a session **History** list.

**Deferred to later phases:** persistent SQLite History (currently in-memory,
this-session), mandated-TLS targets, richer result inspection.

### Connectivity (resolved during live testing) — `app/docker.go`, `app/queryrun.go`, `app/queryrun_run.go`, `app/intranet.go`
The app container runs only on `dbcanvas_default`, not on stack networks, and Docker's
embedded DNS doesn't know the Intranet's `*.<domain>` names — so the first attempt
failed with `lookup ps-01.example.net … no such host`. Fix:
- `Docker.NetworkConnect`/`NetworkDisconnect` added. At **run time** the run **joins the
  target's `dbcanvas-stack-<id>` network** (idempotent) and dials the node's **container
  IP** (via `ContainerIP`) on the standard port — no host ports, no DNS.
- The network-join is done in the **async run goroutine, not the HTTP handler**: attaching
  the app to a new network briefly resets its in-flight connections, so doing it in the
  handler made the *first* run after boot return an empty reply. `qrBuildQuery` now only
  validates + gathers creds/container-id synchronously; `qrRun.dial` does the join + IP +
  DSN at run start.
- Stack destroy (`handleDestroyStack`) now `NetworkDisconnect`s the app before
  `NetworkRemove`, so a lingering Query Runner attachment can't block network cleanup.

### Verification performed
- `go build`/`go vet`/`gofmt`/`npm run build` clean. **Exercised live on :8090** against a
  running MySQL (`ps-01`) + PostgreSQL (`pg-01`) stack: both engines connect and run;
  `count` totals across threads; latency stats populate; the processlist gate works for
  `no_match`, `match`, and **cross-query** (Query 2 fired 1000× only while Query 1's
  `pg_sleep(3)` was active, then stopped) — confirming per-query self-exclusion. MySQL
  uses `admin@'%'` (has `PROCESS`); Postgres uses the superuser (sees all of
  `pg_stat_activity`).

---

## 74. Benchmark tool — OLTP/OLAP/read-write/read-only workloads — `app/benchmark.go`, `app/benchmark_run.go`, `app/web/src/pages/Benchmark.jsx`, `app/web/src/lib/benchmarkApi.js`, `app/main.go`, `app/web/src/App.jsx`, `docs/BENCHMARK.md`

**Feature.** A `#benchmark` page (nav: "Benchmark") that loads a purpose-built dataset
into a chosen database and drives it with one of four **workload profiles** — **OLTP**,
**OLAP**, **read-write**, **read-only** — against a canvas-provisioned MySQL/PXC or
PostgreSQL node, reporting throughput (TPS/QPS) + latency (p50/p95/p99, per-statement
breakdown). Design in `docs/BENCHMARK_PLAN.md`; usage in `docs/BENCHMARK.md`. Distinct
from the Query Runner but shares its connectivity.

**Schema (my design, `bench_*`).** An e-commerce star schema: `bench_customer` +
`bench_product` dimensions, `bench_order` + `bench_order_item` header/line facts, with
**enforced real foreign keys** (order→customer, item→order ON DELETE CASCADE,
item→product). Loader assigns ids (no AUTO_INCREMENT/SERIAL) so DDL is portable; a
`bench_meta` marker stores scale+seed for reuse. Scale 1 ≈ ½M rows.

**Options (per the request).** Server (owner-scoped picker), **Database** to create the
tables in (+ **create if missing** — Postgres uses the `postgres` maintenance DB since
CREATE DATABASE can't run in a txn), workload, scale, threads, duration, warmup,
**Keep data after run** (off drops only the `bench_*` tables — never the database; on
reuses the dataset on the next same-scale run), and seed.

**Engine.** Reuses the shared `a.dialNodeDSN` + `a.resolveNodeCreds` (factored out of
the Query Runner) for the network-join + native-driver connection. Lifecycle: prepare
(create db/schema/load) → warmup (unrecorded) → measure (threads × duration, shared
`database/sql` pool) → cleanup. Workers run per-profile transaction units (OLTP/RW in
`BEGIN…COMMIT`, RO/OLAP autocommit); per-statement-type + per-transaction latency via a
reservoir-sampled `latAcc`. Deterministic bulk load via batched multi-row INSERTs
(parents before children for the FK). Caps: scale ≤ 50, threads ≤ 128, duration ≤ 3600s.

**Shared refactor.** Extracted `dialNodeDSN`, `listSQLTargets`, `resolveNodeCreds` in
`queryrun*.go`; the Query Runner now calls them too.

### Verification performed
- `go build`/`go vet`/`gofmt`/`npm run build` clean; binary re-embeds the new `dist`.
- **End-to-end DB-execution path verified against live nodes** (deployed a fresh
  `bench-e2e` stack: standalone Percona Server 8.0 + PostgreSQL 16). All four workloads
  ran on **both** engines (8 runs, scale 1 ≈ 361k rows, 4 threads × 10s):
  - MySQL — OLTP 354 TPS, RW 355 TPS, RO ~993 TPS / 11.9k QPS, OLAP 19 q/s.
  - Postgres — OLTP 1305 TPS, RW 1542 TPS, RO ~208 TPS / 2.5k QPS, OLAP 125 q/s.
  - Confirmed: FK schema + bulk load; all five OLAP queries (q1–q5) per engine;
    per-statement + txn p50/p95/p99 latency; **data reuse** (Keep-data run skips the
    load — `rowsLoaded=0`); and **cleanup** (Keep-data off drops only the `bench_*`
    tables, `dbcanvas_bench` DB preserved — verified 0 bench tables remaining on both).
- Reuse gate: `prepare` reuses an existing dataset only when the **current** run also
  has Keep-data on (`cfg.KeepData && datasetMatches`); a Keep-data-off run always does a
  clean reload + drop-after (correct "clean run" semantics). Minor: `docs/BENCHMARK.md`
  describes the producer side of Keep-data but not that the consumer must also enable it
  to skip the load.
- Small per-run error counts (≈ thread count) are in-flight statements cancelled when
  the measured window closes — a metric artifact, not a workload failure.

## 75. Percona Server 5.7 (legacy) as a deployable series for "Percona Server" + "PS Replication" — `images/versions.sh`, `versions.yaml`, `app/pxc.go`, `app/proxysql.go`, `app/mysql.go`, `app/replication.go`

**Goal.** Add the legacy **Percona Server 5.7** series to version discovery and let both
the standalone **Percona Server** node (`ps`) and the **PS Replication** frame (`mysql`)
deploy it — alongside the existing 8.0 / 8.4 series.

**Discovery (`make versions`).** `images/versions.sh` now probes the `ps57` repo in both
OS-family probes, emitting a new `@@PS57@@` section that is folded into each image's
`percona_server:` map as a `"5.7"` series (order `8.0`, `8.4`, `5.7`). The package name
diverges from 8.0/8.4's unsuffixed `percona-server-server`: on EL it is
`Percona-Server-server-57` (queried case-insensitively via `elsearch`), on Debian
`percona-server-server-5.7`. Empty series (no packages for that OS) are recorded `[]`, so
the picker simply omits 5.7 there. `versions.yaml` regenerated for the current image
matrix — 5.7 is installable on **OL8** (5.7.30-33.1 … 5.7.44-48.1), **OL9** and
**Ubuntu 22.04** (5.7.41-44 … 5.7.44-48), and **absent** on OL10 / Ubuntu 24.04 (`[]`).

**Frontend.** No change needed — the PS-major `<select>` in both the PS Replication frame
form and the standalone Percona Server form is fully catalog-driven
(`Object.keys(entry.versions).filter(len>0)`), so `5.7` appears wherever the catalog
offers it. Default stays `8.0` (list head), so 5.7 is strictly opt-in.

**Backend — series-safe provisioning.** 5.7 predates almost all of the modern MySQL
vocabulary the provisioners use, so each series-specific helper/script gained a 5.7 branch:
- `psServerPackage(os,major)` (new) — the daemon package (`Percona-Server-server-57` /
  `percona-server-server-5.7`), threaded into `mysqlInstall{RHEL,Debian}` via `$PKG`.
- `psClientProduct` → `ps57`; `pxbProduct`/`pxbPackage` → `pxb-24` /
  `percona-xtrabackup-24` (5.7 pairs with the legacy **XtraBackup 2.4** series);
  `logUpdatesOption` → `log_slave_updates` (no `log_replica_updates` in 5.7); `psMajorOf`
  learns `5.7`.
- `psAuthPlugin` (new) → `mysql_native_password` (no `caching_sha2_password`, hence no
  `GET_SOURCE_PUBLIC_KEY` handshake); `validatePasswordRelax` (new) → plugin-style
  `validate_password_policy`/`_length` (vs the 8.0+ component `validate_password.policy`);
  `persistScope` (new) → `SET GLOBAL` (5.7 has no `SET PERSIST`). `mysqlSetRootPW` /
  `mysqlBaselineScript` / `mysqlSemisyncScript` now take these as env (`$VPRELAX`,
  `$AUTH_PLUGIN`, `$SETVAR`) so a single script body serves every series.
- Replication uses the legacy grammar on 5.7: intra-cluster attach picks
  `mysqlAttachScript57` (`CHANGE MASTER TO … MASTER_AUTO_POSITION` / `START SLAVE` /
  `SHOW SLAVE STATUS` / `Slave_IO|SQL_Running`, `SET GLOBAL read_only`), and cross-cluster
  channels pick `replChannelApply57` / `replChannelPrune57` (same grammar `… FOR CHANNEL`)
  selected per replica-node series via `memberReplMajor(doc,n)`. `sourceBinlogPos` already
  maps 5.7 → `SHOW MASTER STATUS` (non-8.4 branch). GTID `gtid_purged` seeding is
  best-effort on 5.7 (its `+` incremental form is rejected when `gtid_executed` is
  non-empty; plain auto-position is the fallback).

### Verification performed
- `go build` / `go vet` / `go test` clean; `bash -n images/versions.sh` clean.
- **End-to-end against live Percona Server 5.7.44-48** — two OL8 systemd containers
  provisioned exactly as the app does (privileged + host cgroup + `/run` tmpfs):
  - Install via `ps57` → `Percona-Server-server-57` on both.
  - Baseline (both): temp-password path → `validate_password_policy` relax →
    `mysql_native_password` root → user creation → `RESET MASTER`; `gtid_executed` empty,
    root plugin confirmed `mysql_native_password`.
  - GTID replica attach (`mysqlAttachScript57`, `MASTER_AUTO_POSITION=1`) → both slave
    threads `Yes`, `Auto_Position: 1`; a row written on the primary (`demo.t = 42`)
    replicated to the replica, which enforced `super_read_only = 1`.
  - Semi-sync (`INSTALL PLUGIN semisync_master.so` + `SET GLOBAL
    rpl_semi_sync_master_enabled=1`) → enabled `1`.
  - XtraBackup 2.4 (`pxb-24` → `percona-xtrabackup-24`) installs and recognizes the live
    5.7 server's arguments.
- Discovery data confirmed by probing every built image directly (amd64 == arm64) before
  writing `versions.yaml`; `make versions` remains the authoritative regenerator.

---

## 76. Fix: MySQL async baseline aborts on a half-initialized datadir — `app/mysql.go`

**Symptom.** Provisioning a **PS Replication** member (seen on Percona Server 5.7) failed at
the baseline step with mysqld aborting on:

```
[ERROR] Fatal error: Can't open and lock privilege tables: Table 'mysql.user' doesn't exist
mysqld: Table 'mysql.plugin' doesn't exist
```

The datadir already held an InnoDB tablespace, doublewrite buffer and SSL certs, but the
system tables (`mysql.user`, `mysql.plugin`, `mysql.gtid_executed`) were missing.

**Cause.** `mysqlBaselineScript` started the server with a bare `systemctl start "$UNIT"`,
trusting the package's first-start auto-init to populate the datadir. When that auto-init is
interrupted (deploy timeout, container restart) it can leave `/var/lib/mysql` **non-empty but
incomplete**. On the next start mysqld sees a populated datadir, skips initialization, and
aborts because the privilege tables were never created. The InnoDB Cluster/GR path
(`innodbBaseScript`) already guarded against this; the async-replication path did not.

**Fix.** Mirror the `innodbBaseScript` datadir guard into `mysqlBaselineScript`: when the
system-table directory `/var/lib/mysql/mysql` is absent, wipe the datadir
(`find /var/lib/mysql -mindepth 1 -delete` — `mysqld --initialize` refuses a non-empty dir)
and initialize it explicitly with `mysqld --initialize-insecure` using a minimal,
replication-free defaults file, then `chown -R mysql:mysql`. Guarded on the presence of the
system-table dir, so redeploys keep their data. Added a shared `say_err()` helper (same as
innodb) to surface the real `[ERROR]` line on init/start failure, and wired it into the
existing `systemctl start` branch. Works across 5.7 / 8.0 / 8.4 (all support
`--initialize-insecure`; the empty-password root left by the insecure init is then set via the
existing `mysqlSetRootPW` else-branch). `go build ./...` clean.

---

## 77. Fix (real): the datadir guard from §76 still crash-looped mysqld — `app/mysql.go`, `app/innodb.go`

**Symptom (reported).** After §76, MySQL nodes still failed: mysqld pegged a core at 100% CPU
and kept restarting instead of coming up.

**Root cause.** §76's guard, `[ ! -d /var/lib/mysql/mysql ]`, keys on the **mysql/ directory**,
which is *not* a reliable "initialized" signal. An interrupted first-start auto-init leaves the
`mysql/` directory present but **without the privilege tables inside it** — on 8.0/8.4 the
privilege store is the single tablespace `/var/lib/mysql/mysql.ibd` (not files under `mysql/`),
and on 5.7 it is `mysql/user.frm` + `user.MYD/MYI`. So the directory can exist while the tables
do not. In that state the guard is *false* → re-init is **skipped** → `systemctl start` launches
mysqld against a half-baked datadir → it aborts ("`Table 'mysql.user' doesn't exist`" /
"`Data Dictionary initialization failed`"). Under the package unit's `Restart=on-failure` this
crash-loops, burning CPU. Reproduced directly in an OL9 systemd container for both 8.0
(`mysql.ibd` removed, `mysql/` dir kept) and 5.7 (`user.*` removed, `mysql/` dir kept): the
old guard chose SKIP-and-crash in both.

Secondary bug in the same script: `mysqlBaselineScript` recreated the mysql-owned error log
only *inside* the guard block, then did `rm -f "$LOGERR"` in the start branch. On a redeploy
(guard skipped) that deleted the log with no recreation; since `/var/log` is root-owned and
mysqld drops to `user=mysql`, it then couldn't recreate the log → "Permission denied" abort.

**Fix.** Extracted the datadir prelude into a shared `mysqlDatadirInit` constant used by *both*
`mysqlBaselineScript` and `innodbBaseScript` (they were meant to mirror each other and shared
the same latent bug). Changes vs §76:
- **Robust "initialized" check:** `[ ! -f /var/lib/mysql/mysql.ibd ] && [ ! -f /var/lib/mysql/mysql/user.frm ]`
  — re-initialize unless the actual privilege store is present (8.0/8.4 `mysql.ibd` *or* 5.7
  `mysql/user.frm`). Catches empty **and** half-initialized datadirs; still preserves a genuinely
  initialized datadir on redeploy.
- **Error log recreated unconditionally at the top** (mysql-owned), and the destructive
  `rm -f "$LOGERR"` removed from the start branch — matching the proven `innodbBaseScript`
  ordering.

**Verification (end-to-end, OL9 systemd containers, provisioned as the app does — privileged +
host cgroup + `/run` tmpfs, app-rendered `/etc/my.cnf` with `gtid_mode=ON`/`log_bin`):**
- 5.7: clean init → active; half-init (`user.*` removed, `mysql/` kept) → re-init → active;
  redeploy on intact datadir → "preserving", a pre-created DB survived.
- 8.0: clean init → active; half-init (`mysql.ibd` removed, `mysql/` kept) → re-init → active
  (old guard would SKIP-and-crash); redeploy on intact datadir → "preserving", pre-created DB
  survived.
- `go build ./...` + `go vet ./...` clean. Rebuilt the app image and redeployed a MySQL 5.7
  async-replication stack (intranet + primary + secondary) through the running app at
  `http://localhost:8090` — see §77b for the full green result.

### 77b. Follow-on 5.7 bug surfaced by the green deploy: `BACKUP_ADMIN` in the monitor GRANT — `app/mysql.go`, `app/pxc.go`

With the datadir fix in place, mysqld came up and the baseline reached user-creation (proving the
crash-loop was gone), then failed on `GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD,
BACKUP_ADMIN ON *.* TO 'monitor'@'%'` — `BACKUP_ADMIN` is an 8.0 *dynamic* privilege that does not
exist in 5.7 (syntax error, retried 10×). Added `monitorGrants(major)`: 5.7 →
`SELECT, PROCESS, REPLICATION CLIENT, RELOAD` (no BACKUP_ADMIN), 8.0+ keep BACKUP_ADMIN. Threaded
into `mysqlBaselineScript` as `$MON_GRANTS` (env value with spaces, like the existing `$VPRELAX`/
`$RESET_CMD`). Covers both the MySQL-replication frame and the standalone `ps` node (same
`mysqlSetupBaseline`). PXC/InnoDB are 8.0/8.4-only, so unaffected.

**Green end-to-end (live, `http://localhost:8090`).** Redeployed stack 137 (OL9, PS **5.7.44-48**,
async + GTID): both members reached **running (100%)**. Verified on the live cluster — monitor
grant is now `SELECT, RELOAD, PROCESS, REPLICATION CLIENT` (no BACKUP_ADMIN); a row written on the
primary (`repltest.t = 42`) replicated to the secondary; `Slave_IO_Running`/`Slave_SQL_Running` =
Yes with `Auto_Position: 1`; secondary `@@super_read_only = 1`.

## 78. Intranet Squid: collapsed_forwarding + package-repo refresh_pattern rules — `app/intranet.go`

Added to the "Configure Squid" provisioning step, inserted **before the stock `refresh_pattern`
block** in `/etc/squid/squid.conf` (more specific rules must precede Squid's catch-all):
`collapsed_forwarding on` (coalesce concurrent misses for the same object into one upstream
fetch) plus repo-aware caching — `*.rpm` / `*.deb|udeb|ddeb` bodies held long (10080/90%/43200),
`/repodata/` and `/dists/` metadata held short (0/20%/1440 and 0/20%/60), and a `.` catch-all.
Implemented idempotently: guarded on a `^collapsed_forwarding on$` marker, the block is written
to a temp file and spliced in with `awk` ahead of the first `refresh_pattern` line (temp-file
approach avoids awk/sed escaping of the `\.`/`$` patterns). Re-runs are no-ops.

---

## 79. Fix: chained/bidirectional cross-cluster replication left a downstream replica stuck on error 1236 — `app/replication.go` (+ `app/replication_test.go`); plus PS 5.7 named-channel repository fix — `app/mysql.go`

**Reported symptom.** In an *intranet* stack with three PS Replication frames wired
`psrepl-00 (mysql01, 5.7) ↔ psrepl-01 (mysql04, 8.0) → psrepl-02 (mysql07, 8.0)` — i.e. a
**bidirectional** link mysql01 ↔ mysql04 and an **async** link mysql04 → mysql07 — **mysql07
was not replicating from mysql04**. The `xrepl_mysql04` channel existed on mysql07 but its IO
thread was down:

```
Last_IO_Error: Got fatal error 1236 from source when reading data from binary log:
'Cannot replicate because the source purged required binary logs. …
 The GTID set sent by the replica is 'e183d0f3-…:1-4, …'
```

**Root cause (the 1236).** `reconcileReplication` (the final deploy phase) built its per-replica
channel specs in **one up-front pass**, reading each source's `@@global.gtid_executed` *before
applying any channel*, then applied all channels in `map`-iteration (random) order. In a chained
topology the source of one link is itself the **replica** of another:

- Setting up mysql04 ← mysql01 seeds **mysql04's** `gtid_purged` with mysql01's GTIDs
  (`e1d30209:1-2`) via the existing `SET GLOBAL gtid_purged='+…'` seed — mysql04 marks them
  *applied-without-data* (`Retrieved_Gtid_Set` empty; not in its binlog, so **unserveable
  downstream**).
- mysql07's seed snapshot of mysql04 was taken **earlier**, so it contained only mysql04's own
  GTIDs (`e2512547:1-4`), **not** `e1d30209:1-2`.
- mysql07 therefore lacked `e1d30209:1-2`, requested them from mysql04 via `AUTO_POSITION`, and
  mysql04 could not supply them (they live only in its `gtid_purged`) → **fatal 1236**, IO
  thread stops.

**Fix.** Configure cross-cluster channels in **replication-dependency order** and read each
source's position **at apply time** (not in an up-front snapshot):

- New `replicaApplyOrder(links, replicas)` — a topological sort over the *source→replica* edges
  that run **between two replica nodes** (a plain cluster primary that is not itself a replica
  imposes no constraint). Bidirectional links are cycles; they are broken by emitting the
  least-depended-upon remaining node (smallest in-degree, ties by id) — within a cycle the seeds
  are mutually consistent so any break is safe.
- `chanSpec` no longer carries the pre-read `srcGTID`/`logFile`/`logPos`; it carries the source's
  identity (`srcNodeID`, `srcRootPW`, `srcMajor`). In the apply loop (now ordered) the source's
  `gtid_executed` (auto) or binlog file/pos (file/position) is read **immediately before**
  applying the channel. Because mysql04's own `xrepl_mysql01` channel is applied first, mysql04's
  `gtid_executed` already includes `e1d30209:1-2` when mysql07 is seeded from it — so mysql07's
  `gtid_purged` inherits those GTIDs transitively and never requests them → no 1236. All other
  members are still visited afterwards (unordered) so stale-channel pruning is unchanged.

`replication_test.go` adds `TestReplicaApplyOrderChain` (asserts mysql04 precedes mysql07 for the
mysql01 ↔ mysql04 → mysql07 topology) and `TestReplicaApplyOrderPlainPrimary` (a source that is
not itself a replica imposes no ordering).

**Second, separate bug found while deploying (PS 5.7 named channels).** mysql01 is Percona Server
**5.7**, and its cross-cluster channel (from the bidirectional link) failed to be created at all:

```
ERROR 3077 (HY000): To have multiple channels, repository cannot be of type FILE;
Please check the repository configuration and convert them to TABLE.
```

5.7 defaults `master_info_repository`/`relay_log_info_repository` to **FILE**, which cannot carry
a *named* (multi-source) channel — only the anonymous default channel. Fix: `mysqlMyCnf` now emits
`master_info_repository=TABLE` + `relay_log_info_repository=TABLE` **for 5.7 only** (8.0+ default
to TABLE and removed these variables). Verified live on the deployed mysql01: after the repos were
TABLE the named channel created cleanly (no 3077) and its **IO thread ran**.

**Known limitation (not fixed — inherent).** Even past 3077, mysql01 (5.7) cannot apply mysql04's
(8.0) transactions: an 8.0 `CREATE USER … caching_sha2_password` DDL carries the 8.0-only
collation id 255 (`utf8mb4_0900_ai_ci`), which 5.7 has no charset for →
`Last_SQL_Error: Character set '#255' is not a compiled character set`. Replicating 8.0 → 5.7 is
fundamentally unsound; a 5.7 ↔ 8.0 **bidirectional** link is a topology mistake, not a DBCanvas
bug. The async 8.0 ← 8.0 path (mysql07 ← mysql04) — the reported issue — is fully fixed.

**Verification.** `go build ./...`, `go vet ./...`, `go test ./... -run TestReplicaApplyOrder`
all clean. Rebuilt the app image and did a full clean redeploy of the 18-node intranet stack
(stack 138): mysql07's `xrepl_mysql04` came up **`Replica_IO_Running: Yes` / `SQL_Running: Yes`**,
and mysql07's `gtid_executed` now contains mysql01's `…:1-2` (seeded transitively via mysql04).
All 8.0 cross-cluster channels healthy: mysql04←mysql01, mysql07←mysql04, pxc01←mysql09,
pxc05←pxc02 (each IO+SQL running).

---

## 80. Intranet CA: issue X.509 client certificates for MySQL/PostgreSQL/MongoDB users — `app/dbcerts.go`, `app/main.go`, `app/web/src/lib/stackApi.js`, `app/web/src/pages/IntranetManager.jsx`

**Goal.** From the Intranet node's property panel, generate a CA-signed X.509 client
certificate for a database user (prompting **username** + **expiration**), copy the key +
certificate, and read ready-to-use instructions for MySQL, MongoDB and PostgreSQL —
covering both **server configuration** and **client invocation**. Regenerating for an
existing username **overwrites** the previous cert.

**Backend (`app/dbcerts.go`).** The Intranet already holds the stack CA at
`/etc/pki/dbcanvas/ca.{crt,key}`. New Intranet-scoped endpoints run `openssl` in the
container (the systemd image ships openssl) to issue a client cert per username —
subject `/O=DBCanvas/CN=<username>`, EKU `clientAuth,serverAuth`, signed by the CA —
stored under `/etc/pki/dbcanvas/dbcerts/<username>.{crt,key}` and read back for the
operator to copy:

- `GET  …/nodes/{nid}/dbcerts` — list issued certs (`username`, `notAfter`, `subject`).
- `POST …/nodes/{nid}/dbcerts` — `{username, value, unit}` → issue/overwrite; returns
  the PEM `cert`, `key`, `caCert` plus `subject` (RFC2253) and `notAfter`.
- `GET  …/nodes/{nid}/dbcerts/{user}` — re-fetch an existing cert's material.
- `POST …/nodes/{nid}/dbcerts/delete` — `{username}` → remove cert + key.

Username is validated by `validCertUser` (reuses `validName`: letters/digits/`._-`, ≤64;
rejects `.`/`..` and dot-only names and — via `validName` — `/`), so it is always a safe
CN and non-traversing basename. Inputs reach the scripts only through the exec
environment (`CN=…`, `VALUE`/`UNIT`), never string-interpolated. Files are read back with
the existing `readContainerFile` (base64). Expiration reuses the established
minutes/hours/days → `-not_after` convention.

**Frontend.** A new **DB Certs** tab in `IntranetManager.jsx`: a generate form (username +
value/unit, with a "Regenerate (overwrites)" affordance when the name already exists), a
list of issued certs (open to view / copy, delete with confirm), and, for the
selected/generated cert, copyable **CA cert / certificate / private key** blocks plus a
per-engine (MySQL · PostgreSQL · MongoDB) **Server configuration** + **Client invocation**
guide with the username/subject substituted (e.g. MySQL `REQUIRE SUBJECT` + `--ssl-cert`,
PostgreSQL `clientcert=verify-full` + `psql sslcert=…`, MongoDB `$external` X.509 user +
`mongosh --tlsCertificateKeyFile`). Added the four `dbCert*` methods to `intranetApi`.

**Verification.** `go build`/`go vet` clean; `npm run build` clean. Against the live
Intranet node of stack 138: issued a cert for `appuser` — `openssl verify -CAfile ca.crt`
returns **OK** (issuer `CN=DBCanvas CA`), EKU is `clientAuth,serverAuth`, subject
`CN=appuser,O=DBCanvas`; regenerating updated the expiry with the list still showing a
single entry; GET returned the key; delete removed it; and invalid usernames (`bad/name`,
`..`) were rejected with 400.

---

## 81. Real-time teardown of removed nodes + freeze the node set during deploy — `app/intranet.go`, `app/stacks.go`, `app/web/src/pages/StackDesigner.jsx`

**Goal.** (1) When a **deployed** node is deleted from the canvas, remove its container
**and volumes** immediately (in real time), instead of deferring cleanup to the next
deploy. (2) While a deployment is running, **you cannot add or remove nodes** on the
canvas.

**Real-time teardown (backend).** Extracted a shared
`removeNodeResources(ctx, stackID, dep)` — `ContainerRemove` (already `force=true&v=true`,
so anonymous volumes, e.g. each systemd node's `/sys/fs/cgroup` volume, go with the
container) + `VolumeRemove(pmmDataVolume(...))` (the only named volume; namespaced, so a
no-op for other types) + `DeleteDeployment`. `teardownStack` and the deploy-time
"remove nodes deleted from the canvas" loop now both call it — the latter previously
**forgot the PMM volume**, so a PMM node removed at deploy time leaked its `/srv` volume;
now fixed. `handleUpdateStack` calls new `cleanupRemovedNodes(stackID, design)`: it diffs
the just-saved design's node ids against the live deployments and, for any deployment no
longer on the canvas, tears down its container + volumes and drops it from the Intranet
DNS — in a background goroutine so the autosave stays snappy (the designer's 3 s
deployment poll reflects the removal). The canvas already debounce-autosaves on delete, so
this fires within ~1 s of deleting a node. A `node.removed` notification is emitted.

**Freeze node set during deploy (backend guard).** `handleUpdateStack` now rejects a
design update with **409** when the node-set changed (`sameNodeSet` compares node-id sets;
option/position edits keep the same set and are allowed) **and** a deploy is in progress
(`deployInProgress`: any deployment `pending`/`provisioning`). This is the authoritative
enforcement even if the UI is bypassed.

**Frontend lock.** A `deploying` flag (`busy==='deploy'` or any node
`pending`/`provisioning`) gates the canvas: the node palette (all add buttons) is disabled,
the frame member **+/−** controls are disabled, and `deleteNode`/`deleteFrame`/
`addFrameMember`/`removePXCNode`/`removePXCNodeById` early-return — so keyboard-delete, the
node/frame context menus, and the property-panel Delete buttons all no-op while deploying
(this also prevents a local/server divergence that a rejected 409 autosave would cause).
A palette banner explains the lock. Once every node finishes provisioning the flag clears
and editing resumes.

**Verification.** `go build`/`vet`/`test` and `npm run build` clean. On a live `intranet +
PMM` stack: while provisioning, removing or adding a node via the API returned **409**
while a position-only change returned **200**; after both nodes were `running`, deleting the
PMM node from the canvas removed its container **and** its `dbcanvas-pmm-*` volume **and**
its deployment record in real time (~1 s), leaving the Intranet node running.

---

## 82. Per-node TLS certificates + usage docs for MongoDB (PSMDB sharded / replica set / standalone) — `app/mongodb.go`, `app/web/src/pages/MongoDBManager.jsx`

**Goal.** When **Generate per-node certificates** is enabled on a PSMDB Sharded cluster,
PSMDB Replica Set, or PSMDB Standalone node, actually issue a CA-signed cert on each node
and document, in the node's property panel, how to use it for MongoDB TLS — **server
configuration** and **client**.

**Gap found.** The designer already exposed the "Generate per-node certificates from
Intranet CA" toggle for the psmdb/psmrs frames and the psm node, but the backend never
acted on it for MongoDB (unlike PXC/MySQL, which wire certs into `my.cnf` via
`pxcApplyCert`). Enabling it was a no-op.

**Backend (`mongodb.go`).** New `mongoApplyCert` + `mongoCertScript`: reads the Intranet
CA (`/etc/pki/dbcanvas/ca.{crt,key}`), stages it, and runs `openssl` in the node to issue
a server cert (`CN=<fqdn>`, SAN the FQDN + short host, EKU serverAuth+clientAuth) signed by
the CA, writing `/etc/mongo/certs/server.pem` (**cert then key**, the format mongod's
`certificateKeyFile` wants) + `ca.crt`, owned by `mongod`. Called from `mongoPrepareNode`
(the shared per-node setup) for **every** role — config, shard, mongos, standalone — so it
covers all three node types; `mongoPrepareNode` gained an `intranetID` parameter (call
sites updated; the psmrs provisioner now keeps the previously-discarded `intranetID`). It
**does not auto-enable** mongod TLS — cluster-wide TLS is an all-members-at-once operator
step, so the material is issued and the manager documents how to turn it on. Best-effort:
a cert failure is logged, never fatal.

**Frontend (`MongoDBManager.jsx`).** New **TLS** tab (shown only when `generateCert`),
covering all three node types: the on-node cert paths (`server.pem`, `ca.crt`), a
copyable **server configuration** block for the right file (`/etc/mongod.conf`, or
`/etc/mongos.conf` on a mongos) — the `net.tls` block with `requireTLS`,
`certificateKeyFile`, `CAFile`, and `allowConnectionsWithoutCertificates: true` (needed so
password auth works over TLS without a client cert; a comment notes dropping it to require
X.509) — plus **client** invocations: in-cluster `mongosh --tls --tlsCAFile …`, a
from-the-host variant when the port is published, and an optional X.509 client-cert flow.
For clusters the intro notes enabling on every member + rolling out via `preferTLS` first.
The overview TLS row now reads "cert issued (see TLS tab)".

**Verification.** `go build`/`vet`/`test` and `npm run build` clean. Deployed a live
`intranet + PSMDB Standalone` with per-node certs on: `/etc/mongo/certs/server.pem`
(cert+key) and `ca.crt` were written `mongod`-owned; `openssl verify -CAfile ca.crt`
returned **OK** (issuer `DBCanvas CA`), subject `CN=mongo01.example.net`, SAN + EKU as
expected. Applying the documented server config flipped mongod to `requireTLS` (plaintext
now rejected), and the documented client command
(`mongosh --tls --tlsCAFile … -u admin -p`) connected → `{ ok: 1 }` — which is what caught
the missing `allowConnectionsWithoutCertificates`, now in the documented config.
