# Getting started

From an empty machine to a running cluster you can connect to. Already installed? Skip to
[Your first stack](#your-first-stack).

> [!IMPORTANT]
> **DBCanvas is for previewing features, testing and learning — not for production.** It
> deploys disposable lab infrastructure. Nothing is hardened. Do not deploy production infrastructure with it or keep data you care about on it.

- [Install](#install)
- [Open it](#open-it)
- [Where things are](#where-things-are)
- [Your first stack](#your-first-stack)
- [Building one by hand](#building-one-by-hand)
- [Operating a running node](#operating-a-running-node)
- [Connecting to your databases](#connecting-to-your-databases)
- [Putting load on it](#putting-load-on-it)
- [Watching it work](#watching-it-work)
- [Saving a topology for next time](#saving-a-topology-for-next-time)
- [Cleaning up](#cleaning-up)
- [When something goes wrong](#when-something-goes-wrong)

---

## Install

You need **Docker** (Docker Engine on Linux; Docker Desktop, Rancher Desktop or OrbStack on
macOS) and a user that can use it. DBCanvas uses the Docker socket to create your stacks, which
is a privileged capability: run it on a machine you trust.

Download `dbcanvas` for your system from the
[releases page](https://github.com/jaimesicam/dbcanvas/releases) (`dbcanvas-linux-amd64`,
`dbcanvas-linux-arm64`, `dbcanvas-darwin-arm64`, `dbcanvas-darwin-amd64`), put it on your `PATH`
and make it executable:

```sh
install -m 0755 dbcanvas-linux-amd64 ~/.local/bin/dbcanvas
dbcanvas doctor
```

On macOS, the binary is not notarized by Apple. One downloaded in a browser is quarantined, and
macOS refuses to open it ("cannot be opened because Apple cannot check it for malicious
software"). Clear the flag once after installing it, or download it with `curl`, which does not
set it:

```sh
xattr -d com.apple.quarantine ~/.local/bin/dbcanvas
# or
curl -fLo dbcanvas https://github.com/jaimesicam/dbcanvas/releases/latest/download/dbcanvas-darwin-arm64
```

Every file on the release is signed with Sigstore, and `dbcanvas-checksums.txt` lists their
SHA-256 sums: see [Verifying a download](#verifying-a-download).

To build it from a checkout instead (Go and Node.js needed): `make dbcanvas`, which writes
`dist/dbcanvas`.

`dbcanvas doctor` checks Docker, the architecture, the images, the version catalogues, the
data folder and Lima, and says how to fix anything that fails. Node images that are not on the
host yet are pulled from `ghcr.io/jaimesicam` when a stack first needs them, so the first deploy
of each node type takes longer.

Nodes can also run as virtual machines with their own kernel, when [Lima](https://lima-vm.io) is
installed; without it every node runs in a container. See
[Nodes that run as virtual machines](VM_NODES.md#install-lima).

### Verifying a download

Each release lists the SHA-256 of every binary in `dbcanvas-checksums.txt`, and signs every file
with Sigstore: the signature says the file was built by this repository's release workflow from
the release's tag, and there is no key to trust or lose. Download the binary's `.sigstore.json`
beside it and check both:

```sh
grep ' dbcanvas-darwin-arm64$' dbcanvas-checksums.txt | shasum -a 256 -c -
cosign verify-blob dbcanvas-darwin-arm64 \
  --bundle dbcanvas-darwin-arm64.sigstore.json \
  --certificate-identity-regexp '^https://github.com/jaimesicam/dbcanvas/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Open it

```sh
dbcanvas
```

This starts DBCanvas in the background (if it is not running), opens your browser on it signed
in, and returns the prompt. There is no account to create: whoever runs `dbcanvas` owns it. The
web app listens on `http://127.0.0.1:8080` (or a free port when 8080 is taken), and refuses any
request without a credential, so other users of the machine cannot use it.

- `dbcanvas open --print` prints the sign-in link instead, for another browser or a browser at
  the end of an SSH tunnel. It works once, for 3 minutes.
- On a server with no browser (no display, or over SSH), `dbcanvas` prints the
  `ssh -N -L PORT:127.0.0.1:PORT` tunnel to make from your computer and the link to open there.
  See [Signing in](CLI.md#signing-in-dbcanvas-and-dbcanvas-open).
- `dbcanvas status` shows whether it runs and on which port; `dbcanvas stop` stops it. Stacks
  keep running, and any command that needs it starts it again.
- It stops by itself when no stack is deployed and nothing has used it for 10 minutes.

Settings live in `~/.dbcanvas/config.yaml`; every one has a working default. See
[Configuration](CONFIGURATION.md) and [The `dbcanvas` command](CLI.md).

## Where things are

Every page opens from the **Menu** button on the taskbar, or by double-clicking its desktop
icon. (Prefer a sidebar and tabs? Open **Appearance**, the sun icon at the top right, and choose
**Layout → Classic**.)

| Section | Pages |
| --- | --- |
| **Workspace** | **Dashboard** (what is running), **Database Stacks** (design, deploy and operate stacks — the main page), **Kanban** |
| **Data and queries** | **Data Generator**, **Database Explorer**, **Query Runner**, **Benchmark**, **Sample Client Code** |
| **Diagnose** | **Packet Inspector**, **Operator Debugger**, **Core Dump Analyzer**, **Stalk Summary**, **Log Summary**, **FTDC Summary**, **Operator Summary**, **Kubernetes States** |
| **System** | **Profile**, **API**, **Settings** |

- **Choosing a page that is already open switches to it.** Right-click an entry to open another
  copy — two stacks side by side, say.
- **Open pages keep their state** (scroll position, filters, running work) while you look
  elsewhere. Up to 20 can be open at once; change this in **Settings → Tabs** (2–40). At the
  limit a new page does not open, and nothing is closed for you.
- **Hover a `?`** next to any field, panel value, toolbar button or library entry for what it is
  for and when to change it. Turn these off in **Settings → Tooltips → Hidden**.
- **Settings has four tabs:** **Appearance** (theme, look, tooltips), **Workspace** (terminals,
  deployment, the node library, tabs, Scratchboard), **Access & security** (shared sessions,
  token lifetime) and **System** (uploads, writes to internal databases, stack history and
  alerts). A link can open one: `#settings/access`.

## Your first stack

The fastest route is a template.

1. Go to **Database Stacks** → **New stack**.
2. Enter a **Name** and pick a template under **Start from**.
3. Pick a **Lifetime** — the stack is torn down when it elapses. Choose the shortest that covers
   your work. The longest is **30 days**; every stack expires.
4. Press **Create**. The topology appears on the canvas, ready to edit.

![The New stack dialog, with the PXC + ProxySQL + PMM template selected](screenshots/getting-started-new-stack.png)

> *Picking a template shows what it builds and how many nodes it takes.*

5. Press **Validate** to check the design without building anything (missing links, impossible
   topologies, clashing host ports).
6. Press **Deploy**. Nodes come up in dependency order and the deployment console shows each
   step.
7. Wait for the node cards to show **running**.

![A deployed three-node stack: Intranet, Percona Server and VNC, all running](screenshots/getting-started-deployed.png)

> *`running` means the container is up and provisioning finished. A node that failed shows
> `error`, and the deployment console shows the failed step.*

A first deploy downloads packages, so it takes minutes. Later deploys are faster, especially
with **Use Intranet proxy** left on, which caches packages.

## Building one by hand

**Add the Intranet node first.** Other node types stay greyed out until it is on the canvas,
because it provides DNS, the certificate authority, OpenLDAP, mail and a package proxy for the
whole stack.

1. **Add nodes:** right-click the canvas and pick from the **Infrastructure Library** menu; the
   node lands where you clicked. Hover an entry to see what it provides, or why it is greyed
   out. (For a searchable column instead: **Settings → Adding nodes → Docked library**.)
2. **Connect them:** drag from one node's port to another — a database to a PMM server, a
   cluster to the ProxySQL in front of it, a backup tool to a SeaweedFS S3 node.
3. **Set options** in the **Properties** panel: OS and version, port export, TLS certificate,
   monitoring.
4. **Validate**, then **Deploy**.

Clusters live inside **frames** — a dashed box around the members. The frame's panel sets
cluster-wide options (name, version, replication mode); each member's panel sets that node's
options.

## Operating a running node

**Right-click a node** for its actions:

![The right-click menu on a running node](screenshots/getting-started-node-menu.png)

| Action | What it does |
| --- | --- |
| **View config / profile** | Image, version, credentials, certificate and the provisioning steps that ran. |
| **Enter root console** | A root shell inside the node, in the browser. |
| **File manager** | Browse files — upload, download, edit, change permissions. |
| **Copy docker exec command** | The command for a shell from your own terminal. |
| **Copy limactl shell command** | On a node that runs as a VM, in place of the docker exec command: a root shell on the VM from your own terminal. See [VM nodes](VM_NODES.md). |
| **Copy SSH tunnel command** | Only when `SSH_FORWARDING_HOST` is set under `env:` in `config.yaml` — see [Connecting](#connecting-to-your-databases). |
| **Stop** / **Restart** / **Start** | Lifecycle. Data survives a stop. |
| **Delete node** | Removes the node and its container, and its data. |

**Click a node** to open its panel:

![A deployed Percona Server node's panel](screenshots/getting-started-node-panel.png)

> *The **Overview** tab: what the node is, its network address, what monitors it, and the image
> and container it runs in.*

The tabs depend on the node type — for example **Overview**, **Credentials**, **Certificate**
and **Diagnostics** on a database node, LDAP users and mail on the Intranet, a bucket browser on
SeaweedFS. On database nodes, **Diagnostics** captures a `pg_gather` report or a `pt-stalk`
archive for [Stalk Summary](STALK_SUMMARY.md), and **Certificate** re-issues the server
certificate with a lifetime you choose (see [Stacks](STACKS.md#work-with-deployed-nodes)).

## Connecting to your databases

**1. Web terminal.** Right-click → **Enter root console**. The database client is already
installed. **Settings → Terminal** chooses whether consoles open docked or in their own window.

**2. From another node.** Nodes resolve each other by name, so from any shell in the stack
`mysql -h ps-01.example.net -u root -p` works. The **VNC** node is a desktop with a
browser that resolves the stack's names and trusts its CA — use it for consoles not published
to the host, such as PMM or Keycloak.

**3. From your machine.** Tick **Export DB port to the host** before deploying. Leave **Host
port** at `0` to get a free port; the node's panel shows which. Connect to
`127.0.0.1:<port>`.

Credentials are on the node's **Credentials** tab:

![The Credentials tab, with passwords masked until revealed](screenshots/getting-started-credentials.png)

> *Click a password to reveal it. Passwords are generated on the first run and are the same on every node; `dbcanvas passwords` lists them.*

**4. Through an SSH tunnel, when DBCanvas runs on another machine.** Published ports bind to the
server's loopback by default (`CONTAINER_BIND_IP`), so your laptop cannot reach them directly.
Set `SSH_FORWARDING_HOST` under `env:` in `~/.dbcanvas/config.yaml` and right-click a running node → **Copy SSH tunnel
command**. It forwards every port the node publishes to the same port locally, so every address
DBCanvas shows works through the tunnel. The SSH login is your login on that machine (`alex@10.0.0.7`)
unless the setting names a fixed `user@`.

## Putting load on it

| Tool | What it does |
| --- | --- |
| [**Data Generator**](DATA_GENERATOR.md) | Fills tables with realistic data at scale. Start here. |
| [**Database Explorer**](DATABASE_EXPLORER.md) | Browse and query any database in your stacks. |
| [**Query Runner**](QUERY_RUNNER.md) | Runs SQL across nodes in parallel, optionally gated on the processlist. |
| [**Benchmark**](BENCHMARK.md) | OLTP, OLAP, read-write and read-only workloads with throughput and latency. |
| **Demo applications** | Canvas nodes that drive continuous traffic — Stock Market Sim, Ledger Sim, Support Sim and pgvector Support Sim — and Performance Sim, whose stressors make a database misbehave on purpose. Connect one to a database and it runs on its own. |

## Watching it work

| Tool | What it shows |
| --- | --- |
| **PMM** | Add a PMM node and set **Monitored by (PMM)** on your databases; they register at deploy. Open PMM from the VNC desktop, or from your browser if you exported its port. |
| **Dashboard** | Containers, CPU, memory and recent activity across your stacks. |
| [**Log Summary**](LOG_SUMMARY.md) | Several nodes' logs on one timeline, sorted by severity. |
| [**Packet Inspector**](PACKET_INSPECTOR.md) | Captures traffic on a node and decodes MySQL, PostgreSQL, MongoDB or Valkey. |
| [**Stalk Summary**](STALK_SUMMARY.md) / [**FTDC Summary**](FTDC_SUMMARY.md) | Charts and findings from a pt-stalk archive or MongoDB diagnostic data. |
| [**Kubernetes States**](KUBERNETES_STATES.md) | A live board of every object in a Kubernetes cluster — red when broken, highlighted when it changes. |

## Saving a topology for next time

- **Save as template** (designer toolbar) saves the canvas as a reusable template. Passwords,
  host paths and fixed host ports are not saved, so a template can be shared as a `.json` file.
- **Insert template** adds a saved topology to the current canvas. Nodes you can only have one
  of (Intranet, Keycloak, VNC desktop) are reused, clashing hostnames are renumbered, and the
  changes are reported.

Details: [Templates](STACKS.md#templates--save-a-topology-deploy-it-again).

## Cleaning up

![The stack list: each stack's state, nodes, engines, CPU, memory and lifetime](screenshots/getting-started-stacks.png)

- **Destroy** (designer toolbar) removes every container and volume but keeps the design, so you
  can deploy again. Data is lost.
- **Delete** (bin icon in the stack list) removes the stack and its containers. Tick several rows
  to delete them together.
- **Lifetime** — a stack is torn down automatically when it expires.

If the Dashboard shows more running than you expect, look there first.

## When something goes wrong

| Problem | What to do |
| --- | --- |
| **A deploy failed** | Open the deployment console and read the step that failed. The rest of the stack keeps running so you can investigate. |
| **A node shows `error`** | Right-click → **Enter root console** and check the service (`systemctl status`, then its log). **View config / profile** shows how far provisioning got. |
| **"Add an Intranet node first"** | Add the Intranet node; the rest of the library becomes available. |
| **A port is already taken** | A fixed **Host port** clashes with something else. Set it back to `0`. |
| **A version is missing from a picker** | Run `dbcanvas versions update`. See [Troubleshooting](CONFIGURATION.md#troubleshooting). |
| **A published port is unreachable** | Check **Export … to the host** is ticked. If DBCanvas runs on another machine, use an [SSH tunnel](#connecting-to-your-databases). |

---

## Next

- [Stacks](STACKS.md) — every node and cluster type, and everything the canvas can do
- [Feature guides](README.md) — one page per tool
- [Configuration](CONFIGURATION.md) — `config.yaml`, node passwords, troubleshooting
- [The `dbcanvas` command](CLI.md) — stack files, tokens and every command
- [Architecture](ARCHITECTURE.md) — how it is built
