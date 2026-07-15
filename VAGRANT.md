# Deployment backend: Docker (default) or Vagrant (hybrid)

## Context

DBCanvas provisions every stack node as a **Docker container** (`dbcanvas-systemd:*` images) through a
single `*Docker` client — ~275 call sites, a ~33-method surface (`ContainerCreate`, `Exec`/`ExecAs`,
`CopyFile`, networks, DNS, port publishing, `HijackExec`, …).

We want a per-user **Deployment** setting: `docker` (default) or **`vagrant` (hybrid)**. In hybrid mode a
single stack mixes engines **per node**: node types that map to an OS box run as real **VirtualBox VMs**
(via Vagrant), and every other node keeps running as a **Docker container** in the same stack. Nothing is
rejected — the deploy just routes each node to the engine that supports it. Vagrant 2.4.9 + VirtualBox
7.2.6 are installed on this host.

OS/box matrix (matches the `make images` matrix): **Oracle Linux 8/9/10, Ubuntu 22.04/24.04**. Boxes:
official `oraclelinux/8|9|10` and `ubuntu/jammy64` (22.04); `bento/ubuntu-24.04` for Noble (no official
box — Canonical stopped at 24.04). All box names are in one env-overridable map (`DBCANVAS_BOX_*`).

### Per-node routing (the hybrid rule)
- **Run as a Vagrant VM** (when the setting is `vagrant`): the OS/DB nodes and their cluster frames —
  PostgreSQL (standalone + Patroni/repmgr/Spock), MySQL/PXC/InnoDB, MongoDB, Valkey, ProxySQL, HAProxy.
- **Stay Docker even in hybrid mode**: the image-only infra nodes with no box equivalent — PMM, Keycloak,
  OpenBao, SeaweedFS, VNC, Watchtower, Samba AD, and **K3D** (k3s-in-Docker) — and crucially the
  **Intranet**. Its bind config forwards to Docker's embedded resolver (`127.0.0.11`), which only exists
  inside a container, so it must stay a container; VM nodes reach its DNS/CA over VirtualBox NAT.

The engine is chosen **per node from its type**, not per stack.

### Runtime requirement (important)
Hybrid mode needs the DBCanvas process to reach *both* Docker and VirtualBox, so it must run **on the
host** with `vagrant`/`VBoxManage`/`ssh` on `PATH` **and** access to the Docker daemon — not inside the
distroless container. Pure-Docker mode is unchanged and still runs in-container.

---

## Already implemented and verified (branch `vagrant`)

- **Setting** `deploymentBackend` = `docker` | `vagrant` (`app/settings.go`, `Settings.jsx`,
  `SettingsProvider.jsx`).
- **Engine seam** (`app/engine.go`): an `Engine` interface both `*Docker` and the new `*Vagrant` satisfy.
  The engine rides on the **deploy context** (injected by `deployScope`); `a.docker.X(ctx,…)` was
  rewritten to `a.engCtx(ctx).X(ctx,…)` across ~273 sites — Docker behaviour is byte-identical.
- **Vagrant provider** (`app/vagrant.go`, `app/vagrant_ssh.go`): drives `vagrant`/`VBoxManage`/`ssh` — one
  Vagrantfile per VM, OS→box map, static host-only IPs in VirtualBox's default-allowed `192.168.56.0/21`,
  forwarded ports, sudo-wrapped exec/copy, ssh PTY console. **Real single-VM e2e passes**
  (`DBCANVAS_VAGRANT_E2E=1`): box add → up → static IP → systemd → root/user exec → copy → port → destroy.
- **Per-stack** backend stamping + teardown/terminal routing.

**What must change for hybrid:** today the backend is pinned **per stack** and a stack containing an
unsupported node type is **rejected** (`vagrantUnsupportedTypes`). Hybrid replaces both with per-node
routing and cross-engine connectivity (below).

---

## Remaining work for hybrid

### 1. Per-node engine selection (replace the reject) — ✅ DONE (branch `vagrant`, uncommitted)
Implemented as `a.nodeEngine(st, typ)` / `a.depEngine(st, nodeID)` / `a.stackEngines(st)` (engine.go);
`deployScope(stackID, eng)` now takes the node's engine and every provisioner passes
`a.nodeEngine(st, n.Type/frame.Type)`. The deploy-time reject (`vagrantUnsupportedTypes`) is gone; the
maps became the VM-capable set `vagrantVMNode`/`vagrantVMFrame`. Intranet DNS/CA ops route through
`a.intranetEngine()` (Docker) and read each peer's IP on its own engine (dns.go). UI relabeled
"Vagrant (hybrid)". Routing unit tests rewritten (`TestNodeEngineRouting`). `go build`/`vet`/`test` green.
Original notes:
- Setting value stays `vagrant`, meaning "hybrid". Relabel the UI option **"Vagrant (hybrid)"**.
- Add `a.nodeEngine(st, nodeType) Engine`: returns the Vagrant engine when `st.Backend == vagrant` **and**
  the type is VM-supported; otherwise the Docker engine. `vagrantUnsupportedNode/Frame` (engine.go) become
  the "stays Docker" set — keep the maps, drop the deploy-time rejection in `handleDeployStack`.
- Injection point: `deployScope(stackID)` currently injects one per-stack engine. Change so each node's
  provisioner carries **its** engine. Cleanest: have `deployScope` take the node type (or add
  `deployScopeFor(stackID, nodeType)`) and inject `a.nodeEngine(...)`; every provisioner already calls
  `deployScope` at entry, so this is a localized change, not another 273-site sweep. Frame provisioners
  pass their member type.
- `ContainerCreate`'s existing NetworkEnsure and per-node network wiring already run on the injected
  engine, so a Docker node and a VM node in the same stack each get the right network primitive.

### 2. Cross-engine connectivity — host routing ✅ DONE & e2e-verified (`app/vagrant_net.go`)
Docker nodes (on the `dbcanvas-<stackID>` bridge, `172.x`) and VM nodes (on a host-only `192.168.56.x`)
must interconnect: the Intranet serves DNS/LDAP/CA to both; PMM (Docker) monitors DB VMs; DB VMs reach
SeaweedFS/OpenBao (Docker). Because the control-plane runs on the host, the host sits on both networks and
can route between them.

Key realization that shrank this: **provisioning-time** CA/secret distribution is host-mediated (the host
reads the Docker Intranet via the Docker API, then `CopyFile`s bytes into the VM over ssh), so cross-engine
*network* routing is only needed at **runtime** (DNS lookups, PMM scraping DB VMs, DB VMs reaching
SeaweedFS/OpenBao). And post-Part-1, `pointResolverAtIntranet` already writes each VM's resolv.conf pointing
at the Docker Intranet's `172.x` IP (via the vagrant engine). So the whole remaining gap was host-applied
iptables/route plumbing, implemented in `app/vagrant_net.go` as `stackRules()` + `reconcileStackRouting()`.

The e2e spike (`TestHybridConnectivityE2E`, one alpine Docker node + one Ubuntu VM node on one stack net)
surfaced **three** host-level blockers — bidirectional ping/TCP both ways now pass and teardown removes
every rule. All rules are subnet-scoped, tagged `dbcanvas-stack-<id>`, idempotent (`-C` before `-I`), removed
at teardown by turning the `-S` output's `-A` lines into `-D`, and run via `sudo -n` unless already root
(`DBCANVAS_NO_SUDO=1` to skip). `net.ipv4.ip_forward` is ensured =1. The three rule sets:
1. **`raw`/`PREROUTING` ACCEPT** (the subtle one — **Docker 29+**). Docker installs
   `-d <containerIP>/32 ! -i <bridge> -j DROP` at **raw** priority — *before* conntrack and the FORWARD
   chain — so a packet from any non-bridge interface (our VM's host-only NIC) to a container IP is silently
   dropped and never reaches `DOCKER-USER`. A subnet-scoped ACCEPT prepended ahead of that DROP
   short-circuits the raw table for cross-engine traffic. **Without this, the other two rule sets never see
   the packet** — this was the whole reason the first e2e failed with `DOCKER-USER` counters at 0.
2. **`filter`/`DOCKER-USER` ACCEPT** both ways — Docker's default FORWARD policy is DROP and `DOCKER-USER`
   is consulted first, so an ACCEPT here wins.
3. **`nat`/`POSTROUTING` RETURN** both ways — exempts cross-engine traffic from Docker's
   `-s <dockerCIDR> ! -o br… MASQUERADE`, which would otherwise SNAT a reply to the host's host-only address
   (peers would see the host IP, not each other's — breaking DNS ACLs, replication auth, PMM scraping).
- **VM route** (`reconcileStackRouting`, called from `reconcileStackDNS` so it fires on the same triggers):
  for each running VM node, `ip route replace <dockerCIDR> via <hostOnlyGateway (.1)>`. Docker→VM needs no
  per-container route — it flows via the bridge gateway (the host) + forwarding. No-op for docker-only stacks.
- **DNS** is unchanged from Part 1: `dns.go`'s reconcile reads each node's IP on its own engine and the VM
  resolv.conf already targets the Docker Intranet; the routing above just opens the path.
- Tests: unit `TestStackRules`/`TestHostOnlyGateway`/`TestValidCIDR`/`TestRoutingNoopWithoutHybrid`; real
  e2e `TestHybridConnectivityE2E` (gated by `DBCANVAS_VAGRANT_E2E=1`; ~48s; supports `DBCANVAS_E2E_KEEP=1`
  to leave the topology up for live debugging).
- Intranet's engine — **DECIDED: keep it Docker** (bind forwards to `127.0.0.11`, container-only). VM nodes
  reach the Docker Intranet's DNS/CA over the routed host path above.
- Follow-up worth noting: DNS-name resolution across engines (not just IP ping/TCP) is covered by the
  existing reconcile but was not separately asserted in the spike; the mgmt-panel work (Part 3) exercises it.

### 3. Management panels ✅ DONE (exec-based); network-dial paths deferred
Post-deploy panels resolved to Docker via an unstamped `r.Context()`. Fixed by stamping the node's engine
onto the request context in place — `App.stampEngine(r, st, nid)` does `*r = *r.WithContext(withEngine(...,
depEngine))` — so the many handlers that pass `r.Context()` straight through need **no** change.
- Stamped in **all three** loaders: `loadRunningNode` (dbcerts, intranet, openbao, seaweedfs, samba,
  terminal), `loadRunningDBNode` (diag captures), and the mis-named generic `loadRunningPMM` (pg/mongo/pxc
  cert + user + monitor handlers). The name is historical — it's the generic running-node loader.
- Handlers that bypass the loaders (resolve a deployment directly) were stamped individually:
  `handleNodeAction` (start/stop/restart — a VM's lifecycle is Vagrant, not Docker), `handlePGBackup`,
  `handleMongoPBMBackup`, `handlePXCFrameMonitor`.
- Helpers that hard-coded Docker/`context.Background()` were threaded with `ctx`: `diag.go` `fileExists`,
  `captureStatusFor`, `serveContainerFile`, and `startCapture` (its goroutine outlives the request, so it
  takes the engine explicitly and carries it on a background context).
- **Data Generator** (exec-based, postgres/mysql): the engine now travels on `dbConn.eng` (set by
  `dbConnFor` via `nodeEngine`), and `queryJSON`/`execSQL` exec through `c.engine()` — robust for the
  background generation job whose ctx isn't request-scoped.
- The CA-read vs node-write split from Part 1 (`readIntranetFile`/`intranetEngine` force Docker for the
  Intranet, `engCtx(ctx)` uses the node engine) means the `*ApplyCert` handlers Just Work once the ctx
  carries the VM engine: cert bytes are read from the Docker Intranet, applied on the VM.
- Verified: `TestStampEngineOnRequest`; a repo-wide detector (handler does `GetDeployment` + exec without
  resolving an engine) reports zero hits. `go build/vet/test` green.

**Deferred — network-dial paths (Query Runner, Benchmark, Data Generator over the MongoDB driver).** These
don't exec into the node; they dial its IP over TCP from the DBCanvas *app container* (`dialNodeDSN` /
`datagen_mongo.go`: `NetworkConnect(qrAppContainerID())` + `ContainerIP`). That model assumes the app is a
Docker container joined to the stack bridge — but the hybrid runtime requirement runs the app **on the
host**, which already sits on both networks and would dial the node directly (Docker container IP or VM
host-only IP) with no `NetworkConnect`. Making these hybrid-aware is a self-contained host-mode-networking
change, tracked separately from the management-panel work.

## Files
- Change: `app/engine.go` (`nodeEngine`, drop reject), `app/deployrun.go` (per-node engine injection),
  `app/intranet.go` `handleDeployStack` (remove the unsupported-type rejection; keep stamping),
  new `app/vagrant_net.go` or additions for the host routing rules, the `*_mgmt.go`/`diag.go` loaders,
  `Settings.jsx` (relabel).
- Reuse: `dns.go` reconcile, `ContainerIP` (engine-agnostic), the existing `Engine`/`engCtx` seam.

## Verification
- Unit: `go build/vet/test`; extend `nodeEngine` routing tests (supported→Vagrant, infra→Docker).
- Connectivity spike: one Docker node + one VM node in a stack → bidirectional ping + DNS both ways.
- Hybrid e2e: Intranet + a Percona Server VM + a Docker SeaweedFS/PMM → deploy, resolve names across
  engines, open a VM web terminal, tear the stack down cleanly (VMs destroyed, containers removed, host
  routing rules dropped). Run the app on the host (`vagrant`+`VBoxManage`+`ssh`+Docker); build the SPA
  first (`npm run build`).
