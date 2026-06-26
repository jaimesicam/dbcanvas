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
