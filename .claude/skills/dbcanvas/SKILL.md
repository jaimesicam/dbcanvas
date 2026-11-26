---
name: dbcanvas
description: Drive DBCanvas, the `dbcanvas` program on this machine, to deploy database stacks (PostgreSQL, MySQL/PXC, MongoDB, Valkey, clustered or standalone, with proxies, PMM, directories, Kubernetes operators), run commands and copy files on their nodes, put load on them, and diagnose them from logs, packet captures, pt-stalk, FTDC and core dumps. Use when asked to spin up a database lab, reproduce a database problem on a specific version or topology, generate test data, benchmark a cluster, or read a stack's logs and captures. Covers the `dbcanvas` command (up, exec, cp, down, rm, stack files), and the HTTP API with a token for the design catalogue, the design endpoints and the tools. Triggers on dbcanvas, "spin up a stack", "deploy a PXC cluster", "database lab", stack file, DBCANVAS_TOKEN, /api/stacks, design catalog.
---

# Driving DBCanvas

DBCanvas builds real database clusters as Docker containers and gives you tools to
load, watch and diagnose them. It is one program, `dbcanvas`, run by the person who
owns this machine; its web app and its HTTP API are served by a background service that
any `dbcanvas` command starts when it is needed.

**Two ways in.** Use the `dbcanvas` command for stacks and nodes: it resolves names,
waits for deploys and passes a node command's exit status through. Use the HTTP API,
with a token, for everything the command does not cover: the design catalogue, changing
a design piece by piece, templates, and the load and diagnosis tools.

---

## 1. Before you do anything

### Is it here, and does it work

```sh
dbcanvas version          # the release
dbcanvas status           # whether the service runs, its URL, the Docker architecture
dbcanvas doctor           # Docker, images, catalogues, networks, data folder: how to fix each failure
```

`dbcanvas status` and `dbcanvas version` never start anything. Most other commands start
the service in the background and return at once; it stops by itself when no stack is
deployed and nothing uses it.

Everything lives in the user's data folder, `~/.dbcanvas` (or `DBCANVAS_DATA`), including
`config.yaml`. **Do not change `config.yaml`, `DBCANVAS_DATA` or `DBCANVAS_INSTANCE`** unless
the user asked: another data folder is another DBCanvas, with none of their stacks.

### Know what this installation actually has

```sh
dbcanvas ls                 # their stacks
dbcanvas kinds              # what `up --node` can build, with each kind's options
dbcanvas commands           # every command and flag this build accepts
dbcanvas <command> -h       # one command's flags; runs nothing
```

Never write a version you have not seen: a dry run (below) refuses a version this
installation does not have and lists the ones it does.

### A token, for the HTTP API

The command needs none. The API does: a bearer token, which only the owner can make.
Make a short-lived one for the task, keep it in a variable, and revoke it when done:

```sh
URL=$(dbcanvas status --json | jq -r .url)
DBCANVAS_TOKEN=$(dbcanvas token create assistant --scope write --days 1 | grep -o 'dbc_[A-Za-z0-9_]*' | head -1)
api() { curl -sS -H "Authorization: Bearer $DBCANVAS_TOKEN" -H 'Content-Type: application/json' "$@"; }
api "$URL/api/stacks" | jq -r '.[].name'
# when done:
dbcanvas token ls; dbcanvas token rm <id>
```

- Tell the user you made one, and never print its secret into the transcript, a file or
  a commit. `--days 1` caps the damage if it leaks.
- `--scope read` is enough for looking; `write` for changing stacks; `admin` only for
  instance settings and image builds. A token can be limited to the stacks it creates
  (`--own-stacks`), to labelled stacks (`--label k=v`), to short lifetimes (`--max-ttl 8h`)
  and away from node types (`--deny k3d,kubeadm`).
- The API listens on 127.0.0.1 only. `GET $URL/api/meta/endpoints` lists every endpoint
  with the scope it needs; `docs/API_REFERENCE.md` describes them by feature.

---

## 2. The six things you will be asked to do

### A. Spin up a database lab

**Use `dbcanvas up` with compose entries.** Each entry describes a node or a cluster; DBCanvas
works out the design: versions, the cluster frame and its members, the monitoring and
directory wiring, the layout.

```sh
dbcanvas up repro-1234 --ttl 4h \
  --node 'pxc:3,version=8.4.5,os=el8,monitor' \
  --node 'ps,version=8.0.45,os=el9,ldap,monitor' \
  --node pmm \
  --dry-run                     # the plan; nothing is created
dbcanvas up repro-1234 --ttl 4h --node … --wait   # the same entries: create, deploy, wait
```

**Read the dry run.** It lists every node by the name you will use (`+ node pxc-1`), and
what each entry resolved to (`= pxc-cluster-01: pxc 8.4.5-5.1 on oraclelinux 8 (arm64)`).
That line answers "which build am I actually reproducing on": `8.4.5` is `8.4.5-5.1` on
Oracle Linux and `8.4.5-5-1` on Ubuntu. A series (`8.4`) is taken at its newest build when
the stack deploys; read the version on the node afterwards. A version that does not exist
is refused with the list of those that do.

**`--wait` is not optional if anything comes next.** Without it `up` returns as soon as the
deploy starts. With it, `up` returns when every node is running, or exits **3** naming the
node that failed and why. Deploying takes minutes, tens of minutes for an HA cluster or a
Kubernetes operator: say so before you start.

**Always give `--ttl`**: `2h`, `4h`, `8h`, `24h`, `2w` or `30d`. Every stack expires (30 days at
most); pick the shortest that covers the work.

An Intranet node (DNS, CA, LDAP, package proxy) is added for you. A PMM node is **not**:
`monitor` without one is an error telling you to add `--node pmm`.

**The relationships** wire one node to another in the same command:

| option | wires | needs a node of kind |
| --- | --- | --- |
| `monitor` | PMM registration | `pmm` |
| `ldap` | directory authentication | `intranet` (added for you) or `sambaad` |
| `oidc` | Keycloak single sign-on | `keycloak` (the `vnc` desktop it needs is added for you) |
| `kerberos` | GSSAPI single sign-on | `sambaad` |
| `vault` | data-at-rest keys; on `pxc` and `ps-repl` every member, one KV mount each | `openbao` |
| `backup` | pgBackRest / Barman / PBM to S3 | `seaweedfs` |
| `orchestrator` | topology discovery, failover | `orchestrator` |

Ask for one with no provider and the error names the node to add; ask for one a kind does
not have and the error names the kind. `dbcanvas kinds` shows each kind's options and needs.

**`to=` is an association line, not a field.** A ProxySQL or PgBouncer with no backend, a
simulator with no database: compose refuses rather than building those. Add the target,
or name it when there are several:

```sh
dbcanvas up pg --ttl 4h --node 'patroni:3,os=el9' --node 'haproxy,os=el9' --node 'pgvectorsim,to=haproxy-01'
```

- **HAProxy and ProxySQL packages** come from the OS repository unless `source=` says
  otherwise: HAProxy `percona` (2.8) or `pgdg` (2.7 to 3.x, Oracle Linux only); ProxySQL
  `percona` (default), `upstream-admin` or `upstream-native`. `version=` then takes a build or
  a branch of that source. Upstream ProxySQL 3 also fronts PostgreSQL:
  `--node 'patroni:3,os=el9' --node 'proxysql,os=el9,source=upstream-native,version=3.0'`.
- **PgBouncer** is the PostgreSQL pool: `--node 'pgbouncer,os=el9,mode=transaction,cert'` in
  front of one `pg`, Patroni, repmgr, streaming replication or Spock cluster.
- **A Patroni standby cluster** is `standbyOf=`:
  `--node 'patroni:3,os=el9,name=east' --node 'patroni:3,os=el9,name=west,standbyOf=east'`.
  Same major and build on both; the standby has no `backup` or `vault`.
- **Encryption at rest on a cluster** is the frame's flag: `--node 'pxc:3,os=el9,vault' --node openbao`.
- **Shaping reproduces an environment, not just a version.** A slow disk or a lossy link is
  what most cluster bugs need:

  ```sh
  --node 'pxc:3,os=el9,netLatencyMs=80,netJitterMs=20,netLossPct=1'   # flow control, then eviction
  --node 'ps,os=el9,deviceReadMbps=20,deviceWriteMbps=10'             # a slow disk
  ```

  Shaping covers the node's database and cluster ports; `netAllTraffic` widens it to every
  packet, which can break the node's own provisioning.
- **An option a kind would ignore is refused**, not accepted (`cpus=` on a Keycloak node).
  The refusal means the knob does not exist there; do not work around it.

**Anything the shorthand does not cover, set by name** in a stack file. Each `nodes:` entry
can be a map with `options` (fields of the node, or of a cluster's frame) and
`memberOptions` (fields on every member), named as the design catalogue names them (below):

```yaml
apiVersion: dbcanvas/v1
kind: Stack
name: repro-1234
ttl: 4h
nodes:
  - {kind: pxc, count: 3, version: "8.4", options: {pxcMajor: "8.4"}, memberOptions: {cpus: 2}}
  - pmm
```

```sh
dbcanvas up -f repro-1234.yaml --dry-run
dbcanvas up -f repro-1234.yaml --wait
```

`up` on a stack that exists is declarative: the design becomes the file's, a node no
longer listed loses its container, new ones are built, running ones keep running as they
are (the plan says so). `dbcanvas export repro-1234 -o repro-1234.yaml` writes the full
design back out, so a stack can be kept and rebuilt.

**Single sign-on is one command:**

```sh
dbcanvas up sso-lab --ttl 8h --node keycloak --node 'ps,version=8.4.11,os=el9,oidc,monitor' --node pmm --wait
```

Compose adds the Intranet, the `vnc-01` desktop Keycloak's console needs, and the
certificate an OIDC issuer requires; do not add `--node vnc` yourself. Spell the version
out: `auth_openid_connect` needs Percona Server 8.4.11-11 or 9.7.2-2, and compose refuses
older builds. To prove it on the node: `oidc-login jane` (seeded users `jane` and `john`,
password `KEYCLOAK_USER_PASSWORD` from `dbcanvas passwords`), then **`SET ROLE accounting;`**
before querying `oidc_demo`: the role is granted but not activated.

**Kubernetes operators are a compose entry**: `k3d` with the operator and any other K3D
setting as `opt.FIELD=VALUE` (by its design catalogue name; `member.FIELD=VALUE` sets a field
on every member of a cluster):

```sh
dbcanvas up k8s-lab --node 'k3d:2,opt.k3dOperator=pxc,opt.k3dOperatorVer=1.20.0' --ttl 8h --wait
dbcanvas exec k8s-lab k3d-1 -- kubectl get pods -A
```

`opt.k3dOperator` is `pxc`, `ps`, `psmdb`, `pg`, `cnpg`, `pgo` or `everest`; `""` is plain
Kubernetes.

**Not built by compose**: Kubeadm, All in One, the Stock Market Sim, a core-dump host.
`dbcanvas kinds` lists them with the reason. Start from a template, or add
them with the design endpoints (§2, *Every option, by name*), then deploy and wait with the
command:

```sh
api "$URL/api/templates" | jq -r '.[] | "\(.id)  \(.name)"'
api -X POST "$URL/api/stacks" -d '{"name":"pxc-op","ttl":"8h","templateId":"builtin:k3d-pxc-operator"}' | jq '.id'
dbcanvas export pxc-op -o pxc-op.yaml && dbcanvas up -f pxc-op.yaml --wait
```

**Never author a whole canvas design by hand.** It is one document of hundreds of
type-discriminated fields cross-referenced by generated ids. Compose it, or start from a
template, then change it one piece at a time.

### Every option, by name: the design catalogue and the design endpoints

`GET /api/stacks/design/catalog` is the designer's properties panels as data. For each node
type (`nodes`) and cluster type (`frames`):

- `options`: `field`, JSON `type`, accepted `values`, the `default`, and `when` it applies.
  A cluster's own settings are the frame entry's; per-member settings are the member node
  entry's.
- `relations`: named references to another node (`monitor`, `backup`, `vault`, `oidc`,
  `ldap`/`kerberos`, `orchestrator`, `repository`, `storage`, `app`).
- `edges`: the lines it draws and what may be at the other end.
- `actions`: **what you can do once it is deployed** (backups and restores, switchover,
  certificates, database users, captures), each with its method, path, body and when it
  applies. `commonActions` apply to every node.

```sh
CAT="$URL/api/stacks/design/catalog"
api "$CAT?type=patroni" | jq '.frame.options[] | {field, type, values, default, when}'
api "$CAT?type=patroni" | jq -r '.node.actions[] | "\(.method) \(.path): \(.description)"'
```

Then change a stack one thing at a time. API paths take **ids**, not names (§3):

```sh
S=$(dbcanvas ls --json | jq -r '.[] | select(.name=="repro-1234") | .id')
api -X POST "$URL/api/stacks/$S/design/nodes"  -d '{"type":"pmm"}'
api -X POST "$URL/api/stacks/$S/design/frames" -d '{"type":"pxc","members":3,"options":{"pxcMajor":"8.4"},"relations":{"monitor":"<pmm id>"}}'
api -X POST "$URL/api/stacks/$S/design/edges"  -d '{"from":"<frame id>","to":"<proxysql id>"}'
api -X PUT  "$URL/api/stacks/$S/design/frames/<frame id>" -d '{"memberOptions":{"cpus":2}}'
api -X PUT  "$URL/api/stacks/$S/design/nodes/<id>" -d '{"relations":{"monitor":null}}'   # null clears
api -X POST "$URL/api/stacks/$S/validate" | jq '.issues'
dbcanvas export repro-1234 -o repro-1234.yaml && dbcanvas up -f repro-1234.yaml --wait
```

- Each call answers with what it changed plus the validator's `issues` and `ok`. Issues do
  not block an edit; read them before deploying.
- A refusal names what is accepted. Fix the request; do not push a field past the check
  with `PUT /api/stacks/{id}`.
- Removing a node or cluster also removes its lines, clears relations to it, and **removes
  its container if deployed**. Ask before removing anything deployed.
- A sharded `psmdb` cluster's shape is `options.psmdbSetup` (`standard` or `minimum`), not
  `members`. Nodes cannot be added or removed while a deploy is running.

### B. Reproduce a problem on a specific version or topology

Usually compose with the version written out, dry run first:

```sh
dbcanvas up bug-4821 --ttl 4h --node 'ps,version=8.0.42,os=el9' --dry-run
```

For a shape compose does not build, compose what it can and add the rest with the design
endpoints. To change the version or topology of a running lab, run `up` again with the new
entries, or `dbcanvas rm` it and build it again.

### C. Put load on it

From the nodes themselves, with the database's own tools:

```sh
dbcanvas exec repro-1234 ps-01 -- mysql -e 'CREATE DATABASE IF NOT EXISTS app'
dbcanvas exec -i repro-1234 ps-01 -- mysql app < ./schema.sql
```

Or with DBCanvas's tools over the API (`docs/API_REFERENCE.md`, *Running load*). They take
the stack id and the node id, start a job and are polled:

```sh
api "$URL/api/benchmark/targets" | jq '.'
RUN=$(api -X POST "$URL/api/benchmark/runs" \
  -d "{\"stackId\":$S,\"nodeId\":\"<node id>\",\"workload\":\"oltp\",\"threads\":8,\"durationS\":60,\"createDb\":true}" | jq -r .runId)
api "$URL/api/benchmark/runs/$RUN" | jq '{status, message, tps, qps}'
```

The Data Generator (`/api/datagen/…/generate`) fills tables with realistic data, and the
Query Runner (`/api/queryrun/runs`) runs SQL on several nodes at once.

### D. Find out what happened

```sh
dbcanvas exec repro-1234 pxc-1 -- journalctl -n 200 --no-pager
dbcanvas exec repro-1234 pxc-1 -- sh -c 'tail -n 200 /var/log/mysqld.log'
```

For several nodes at once, the Log Summary puts their logs on one timeline, classified by
severity. The comparison across members is usually where the answer is:

```sh
B=$(api -X POST "$URL/api/logsummary/collect" \
  -d "{\"stackId\":$S,\"nodeIds\":[\"<id 1>\",\"<id 2>\",\"<id 3>\"],\"lines\":5000}" | jq -r .id)
api "$URL/api/logsummary/bundles/$B" | jq '{summary: .bundle.summary, findings}'
api "$URL/api/logsummary/bundles/$B/events?sev=bad&limit=50" | jq '.'
```

Packet captures (`/api/pktinspect/captures`), pt-stalk (`…/nodes/{nid}/ptstalk`) and MongoDB's
FTDC (`…/nodes/{nid}/ftdc`) work the same way; their paths and bodies are in
`docs/API_REFERENCE.md`. A `mysqld` **core dump** is read in the Core Dump Analyzer in the
browser (`dbcanvas open`), which runs gdb interactively.

### E. Get a shell or move a file

```sh
dbcanvas exec repro-1234 pxc-1 -- mysql -e 'SHOW ENGINE INNODB STATUS\G'
dbcanvas exec repro-1234 pxc-1 -- sh -c 'grep -c wsrep /var/log/mysqld.log'
dbcanvas cp repro-1234 ./my.cnf pxc-1:/etc/my.cnf.d/
dbcanvas cp repro-1234 pxc-1:/var/log/mysqld.log ./
```

- `exec` runs the command without a terminal: stdout and stderr come back exactly as
  written, and **`dbcanvas` exits with the command's own status**, so `&&` and `if` work.
- Arguments are passed as given, not through a shell: for pipes or redirections use
  `-- sh -c '…'`. `-i` passes standard input (up to 16 MiB), `--timeout` (default 10m, at
  most 1h) stops a long command (exit 124), `--user` runs as someone else, `--json` returns
  the result as one object.
- `cp` copies one file. For a directory, `tar` it on the node and copy the archive.
- The accounts DBCanvas created on the nodes are in `dbcanvas passwords`. As root on a node
  the database clients usually need none.
- For an interactive shell, the user opens the node's console in the browser
  (`dbcanvas open`). Do not try to drive one.

### F. Check whether the installation is current

```sh
dbcanvas version
dbcanvas versions                                  # the version catalogue in use, and its date
api "$URL/api/dashboard/updates" | jq '{current, latest, available}'
```

The update check asks GitHub, and only when called. Updating the binary, or refreshing the
catalogue with `dbcanvas versions update`, is the user's call: report what is available, and
do not do it yourself.

---

## 3. Rules that will save you a wrong turn

**The command takes names; API paths take ids.** `dbcanvas exec repro-1234 pxc-1` resolves
both. An API path is sent as written, so `/api/stacks/repro-1234/…` or `…/nodes/pxc-1/…` does
not fail cleanly: the message reads like a problem with the node. Get the ids first:

```sh
S=$(dbcanvas ls --json | jq -r '.[] | select(.name=="repro-1234") | .id')
api "$URL/api/stacks/$S" | jq -r '.design.nodes[] | "\(.id)  \(.label)"'
```

**`--json` for anything you will parse** (`ls`, `status`, `up`, `exec`), so your `jq` reads a
stable format rather than a table.

**Poll, don't sleep blindly.** `up --wait` waits properly. API jobs (benchmark, datagen,
query runs, captures) return an id and report a terminal `status`.

**Read a node's ports back after a restart.** Docker assigns new host ports on every start.

**A `read` token cannot change anything.** A 403 naming a scope means the token is too
narrow: say so, do not retry.

**Look up post-deployment actions; don't guess paths.** They are the catalogue's `actions`.

---

## 4. Things to be careful with

Destructive or outward-facing. **Confirm with the user first** unless they asked for that
exact action.

| Command | Why |
| --- | --- |
| `dbcanvas rm` | Removes the stack and its design. Not recoverable. |
| `dbcanvas down` | Removes every container in the stack; the design survives. |
| `up` on an existing stack, or a design `DELETE` | Removes the containers of nodes no longer in the design. |
| `dbcanvas exec … -- systemctl stop/restart …`, `kill`, `rm` | Interrupts or changes a running database. |
| `dbcanvas stop` | Stops the service for everyone using it (their browser tabs, shared sessions). Stacks keep running. |
| `dbcanvas token rm` | Breaks whatever holds that token, at once. |
| `dbcanvas rotate-key` | Re-keys their database; they must back up the new key. |
| `dbcanvas versions update` / `reset` | Changes which versions every later deploy offers. |
| `PUT /api/system/settings` | Instance-wide. |
| `dbcanvas open --print` | Prints a one-time sign-in link: a credential for 3 minutes. Hand it to the user, nowhere else. |

**Never print a token secret into a transcript, a commit or a log.**

**Deploying costs real resources.** An HA cluster is several containers with real memory
limits. Do not deploy speculatively on someone's machine.

---

## 5. Reading failures

| Exit | Means | Do |
| --- | --- | --- |
| 0 | success | |
| 1 | the command failed | read the message; it is DBCanvas's own |
| 2 | a usage error | `dbcanvas <command> -h` |
| 3 | `up --wait`: a node failed, or the deploy did not finish in `--timeout` | the failing node and its error are printed; read its log with `exec`, fix the design |
| any | `exec` exits with the command's own status (124: it timed out) | |

HTTP statuses: `400` bad body, `401` no or bad token, `403` not permitted (the message says
which scope or limit), `404` no such thing, `409` wrong state (node not running, a deploy
running), `410` the stack was deleted.

A failed deploy is diagnosed, not retried: find the node, read its log, fix the design.

---

## 6. A worked example, end to end

> *"Reproduce a Galera flow-control stall on PXC 8.0 and get me the logs."*

```sh
# 1. Is it here, and what does it have?
dbcanvas status && dbcanvas kinds | grep -A3 '^pxc '

# 2. Build it, with a lifetime, and wait for it. The latency and loss are the
#    reproduction: a stall is a property of the network, not of 8.0.42.
dbcanvas up galera-stall --ttl 4h \
  --node 'pxc:3,version=8.0.42,os=el9,monitor,netLatencyMs=80,netLossPct=1' --node pmm --dry-run
dbcanvas up galera-stall --ttl 4h \
  --node 'pxc:3,version=8.0.42,os=el9,monitor,netLatencyMs=80,netLossPct=1' --node pmm --wait

# 3. Load it until the symptom appears: a write workload for five minutes (§2 C)
S=$(dbcanvas ls --json | jq -r '.[] | select(.name=="galera-stall") | .id')
P1=$(api "$URL/api/stacks/$S" | jq -r '.design.nodes[] | select(.label=="pxc-1") | .id')
RUN=$(api -X POST "$URL/api/benchmark/runs" \
  -d "{\"stackId\":$S,\"nodeId\":\"$P1\",\"workload\":\"rw\",\"threads\":16,\"durationS\":300,\"createDb\":true}" | jq -r .runId)
api "$URL/api/benchmark/runs/$RUN" | jq '{status, tps, qps}'       # poll until status is final

# 4. Capture the evidence, from every member
for n in pxc-1 pxc-2 pxc-3; do
  dbcanvas exec galera-stall "$n" -- mysql -e "SHOW GLOBAL STATUS LIKE 'wsrep_flow_control%'"
done
#    and the logs on one timeline: §2 D, with the three node ids

# 5. Hand it back, and ask before tearing down
dbcanvas ls
```

It does **not** remove the stack. Leave the evidence standing and ask; the TTL is the
safety net if the user walks away.

---

## 7. Where the detail is

| Question | Read |
| --- | --- |
| Every command, stack files, exit statuses | `docs/CLI.md`, or `dbcanvas help` |
| Which endpoint does what, by feature | `docs/API_REFERENCE.md` |
| Tokens, scopes, limits, status codes | `docs/API.md` |
| What a node or cluster type is | `docs/STACKS.md` |
| Settings, the data folder, troubleshooting | `docs/CONFIGURATION.md` |
| **This build's live command and endpoint lists** | `dbcanvas commands`, `GET /api/meta/endpoints` |

When this file and `dbcanvas commands` or `/api/meta/endpoints` disagree, those are right:
they come from the binary you are running.
