---
name: dbcanvas
description: Drive a DBCanvas installation over its HTTP API or with dbcanvas-cli — deploy database stacks (PostgreSQL, MySQL/PXC, MongoDB, Valkey, clustered or standalone), run load against them, and diagnose them from packet captures, logs, FTDC, pt-stalk and core dumps. Use when asked to spin up a database lab, reproduce a database problem on a specific version or topology, generate test data, benchmark a cluster, or read a stack's logs and captures. Triggers on dbcanvas, dbcanvas-cli, "spin up a stack", "deploy a PXC cluster", "database lab", DBCANVAS_TOKEN, /api/stacks.
---

# Driving DBCanvas

DBCanvas builds real database clusters in containers and gives you tools to load,
watch and diagnose them. Everything it does is an HTTP endpoint; the web UI is one
client and you are another.

**Two ways in, one API behind both.** Prefer `dbcanvas-cli` — it resolves stacks by
name, waits for deploys, and maps failures onto exit codes. Fall back to `curl` when
the CLI is not installed or an endpoint has no subcommand.

---

## 1. Before you do anything

### Establish that you can reach it

```sh
dbcanvas whoami      # account, role, server, token scope and expiry
```

Non-zero exit? Read the message. **Exit 3 means not signed in** — you need a token,
and you must ask the user for one or for permission to create one. Do not attempt to
guess credentials.

Without the CLI:

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" "$DBCANVAS_URL/api/me"
```

### Getting a credential — ask, do not improvise

A token is created by `dbcanvas login`, which needs the user's **password**. Never
invent, guess or brute-force one. If `DBCANVAS_TOKEN` is not set and no profile
exists, stop and ask the user to either run `dbcanvas login` themselves or paste a
token they created in **API → Tokens**.

`dbcanvas login` prompts interactively. In an automated context that will hang, so
prefer the environment:

```sh
export DBCANVAS_URL=http://localhost:8080
export DBCANVAS_TOKEN=dbc_...
```

### Know what this installation actually has

Endpoints and installable versions differ per installation. **Read, don't assume:**

```sh
dbcanvas endpoints --long              # every endpoint this server serves
dbcanvas endpoints -q backup           # find one
dbcanvas stack kinds                   # what compose can build here
dbcanvas api GET /api/catalog/ps       # which Percona Server versions exist here
dbcanvas template list                 # which templates exist here
```

Never hardcode a version string you have not seen in a catalogue response.

---

## 2. The five things you will be asked to do

### A. Spin up a database lab

**Use `compose`.** It takes a description and works out the design — versions, the
cluster frame and its members, the monitoring and directory wiring, the layout.

```sh
dbcanvas stack kinds            # what this installation can build, and the options

dbcanvas stack compose repro-1234 --ttl 4h \
  --node 'pxc:3,version=8.4.5,os=el8,monitor' \
  --node 'ps,version=8.0.45,os=el9,ldap,monitor' \
  --node 'pmm,version=3' \
  --dry-run                     # look at the plan first
```

Drop `--dry-run` to build it, and add `--deploy --wait` to provision it.

**Run `--dry-run` first and read the resolved versions.** That column is the answer
to "which build am I actually reproducing on" — `8.4.5` is `8.4.5-5.1` on Oracle Linux
and `8.4.5-5-1` on Ubuntu, and the plan says which one you got.

An Intranet node is added for you. A PMM node is **not**: `monitor` without one is an
error telling you to add `--node pmm`, because a PMM server is a heavy container and
starting one unasked would be a surprise.

**The relationships** are what makes compose worth using over a template — each wires
this node to another one in the spec:

| flag | wires | needs a node of kind |
| --- | --- | --- |
| `monitor` | PMM registration | `pmm` |
| `ldap` | directory authentication | `intranet` (added for you) or `sambaad` |
| `oidc` | Keycloak single sign-on | `keycloak` — the `vnc` desktop it needs is added for you |
| `kerberos` | GSSAPI single sign-on | `sambaad` |
| `vault` | data-at-rest keys | `openbao` |
| `backup` | pgBackRest / Barman / PBM to S3 | `seaweedfs` |
| `orchestrator` | topology discovery, failover | `orchestrator` |

Ask for one with no provider in the spec and the error names the node to add. Ask for
one a kind does not have and the error names the kind. Run `dbcanvas stack kinds` if
you are unsure — it prints the matrix for that installation.

**`to=` is different, and easy to miss.** A relationship above is a *field*. `to=` is
an association **line**, and seven kinds do nothing without one — a ProxySQL with no
backend, a simulator with no database. Compose refuses rather than building those, so
you will see the error; the fix is to add the target to the spec, or name it:

```sh
dbcanvas stack compose chaos --ttl 4h \
  --node 'pxc:3,version=8.4.5,os=el8' \
  --node 'haproxy,os=el9' \
  --node 'marketchaos,to=haproxy-01'
```

The HAProxy needed no `to=` — one legal backend, so compose used it and said so in the
plan. MarketChaos needed one, because driving the cluster directly and driving it
through the proxy are different tests and it will not guess. `dbcanvas stack kinds`
prints each kind's legal targets in the LINKS TO column.

**Shaping is how you reproduce an environment, not just a version.** A version string
cannot express a slow disk or a lossy link, which is what most cluster bugs actually
need:

```sh
--node 'pxc:3,os=el9,netLatencyMs=80,netJitterMs=20,netLossPct=1'   # flow control, then eviction
--node 'ps,os=el9,deviceReadMbps=20,deviceWriteMbps=10'             # a slow disk
```

`dbcanvas stack validate` prints what each shaped node will get before anything is
built. Shaping covers the node's database and cluster ports; `netAllTraffic` widens it
to every packet, which can make a node fail its own provisioning — use it deliberately.

**An option a kind would ignore is refused, not accepted.** `cpus=` on a Keycloak node
is an error, because nine node types never apply a CPU limit. Do not work around it —
the refusal is telling you the knob does not exist there. `dbcanvas stack kinds` is
generated from the same table, so it is never out of date with what will be accepted.

If `compose` refuses a kind — a Kubernetes frame, an All-in-One, the Stock Market Sim,
a core-dump host — it says why. Fall back to a template for those:

```sh
dbcanvas template list
dbcanvas stack create repro-1234 --template "PXC + ProxySQL + PMM" --ttl 4h --deploy --wait
```

**Single sign-on is one command, and it deploys.** A worked example, because the
prerequisites are the part that trips people up:

```sh
dbcanvas stack compose sso-lab --ttl 8h \
  --node keycloak \
  --node 'ps,version=8.4.11,os=el9,oidc,monitor' \
  --node 'pmm,version=3' \
  --deploy --wait
```

Four `--node` flags, five containers: compose adds the Intranet, adds the `vnc-01`
desktop Keycloak's port-less admin console needs, and turns on the certificate an OIDC
issuer requires — each reported under `+`. Do not pass `--node vnc` yourself.

Spell the version out. `auth_openid_connect` arrived in Percona Server 8.4.11-11, and
below that the provisioner skips the plugin *without failing* — no error at compose,
validate or deploy, just a node with no SSO. Compose refuses it instead, naming the
minimum.

To prove it works, on the node: `oidc-login jane` (seeded users are `jane` and `john`,
password `KEYCLOAK_USER_PASSWORD`), then **`SET ROLE accounting;`** before querying
`oidc_demo`. The Keycloak group maps to a role that is granted but *not activated*, so
without `SET ROLE` the query fails with *SELECT command denied* and looks like the SSO
is broken when it is not.

**Never author a canvas design from scratch.** It is one document of ~120
type-discriminated fields with cross-references by generated id. Compose, or a
template, or an export you edit.

**Always set a `--ttl`.** `2h`, `4h`, `8h`, `24h`, `2w`, `infinity`. A lab you forget
runs until somebody notices. Only use `infinity` if the user asked for it.

**`--wait` is not optional if anything comes next.** `deploy` returns as soon as
provisioning *starts*; without `--wait` your next command runs against a cluster that
does not exist yet. `--wait` exits **4** if the deploy failed.

Deploying is slow — minutes, sometimes tens of minutes for an HA cluster. Say so
before you start rather than after.

### B. Reproduce a problem on a specific version or topology

Usually this is just compose with the version written out:

```sh
dbcanvas stack compose bug-4821 --node 'ps,version=8.0.42,os=el9' --dry-run
```

If the version is not available the error lists what is, in that series. For a shape
compose does not build, edit an exported design:

```sh
dbcanvas stack export some-lab > design.json
# edit design.json
dbcanvas api PUT /api/stacks/12 --data @design.json
dbcanvas stack validate 12                           # check before building
dbcanvas stack deploy 12 --wait
```

`validate` builds nothing and exits non-zero when the design has problems. Run it
before every deploy you assembled by hand.

### C. Put load on it

```sh
dbcanvas datagen tables repro-1234 pxc-01 --database sbtest
dbcanvas datagen run repro-1234 pxc-01 --database sbtest --table sbtest1 --rows 5000000 --wait
dbcanvas benchmark run repro-1234 pxc-01 --workload oltp --duration 60 --wait
dbcanvas query run --stack repro-1234 --nodes pxc-01,pxc-02 --sql 'SHOW STATUS LIKE "wsrep%"' --json
```

### D. Find out what happened

```sh
dbcanvas logs collect repro-1234 --nodes pxc-01,pxc-02,pxc-03   # one timeline across members
dbcanvas logs events <bundle-id> --severity error
dbcanvas capture start repro-1234 pxc-01 --seconds 30            # decoded packets
dbcanvas stalk start repro-1234 pxc-01                           # pt-stalk
dbcanvas ftdc node mongo-lab psmdb-01                            # MongoDB's black box
```

Collecting logs from **several** nodes at once is the point — the comparison across
members is usually where the answer is.

### E. Get a shell or move a file

```sh
dbcanvas node exec repro-1234 pxc-01 -- mysql -e 'SHOW ENGINE INNODB STATUS\G'
dbcanvas node cp ./my.cnf repro-1234:pxc-01:/etc/my.cnf.d/
dbcanvas node cp repro-1234:pxc-01:/var/log/mysqld.log ./
dbcanvas node cp repro-1234:pxc-01:/etc/my.cnf repro-1234:pxc-02:/etc/my.cnf   # node → node
```

`node console` is interactive and will hang in an automated context — use `node exec`.
Note that `exec` runs through the console, so **the remote exit status is not
reported**; check the output, or have the command print a marker.

---

## 3. Rules that will save you a wrong turn

**Resolve by name — in the subcommands, not in `dbcanvas api`.** Every stack *and*
node argument of a curated command takes a name or an id. A node's name is its
hostname — `pxc-01`, what the compose plan and `node list` show; the id is internal,
and on a stack drawn in the UI it looks like `pxc-mt1kvaak-3`. If two stacks share a
name the CLI refuses and asks for an id rather than guessing — that refusal is
correct, do not work around it by picking one.

`dbcanvas api` is the exception, and it is the one that will catch you: it passes the
path through verbatim, so a URL needs the **id**. A name there does not fail cleanly.
The message depends on which endpoint you hit — `node is not deployed` on one, `this
capture is not available for this node type` on another — and none of them say "no
such node", so it reads as a problem with the node rather than with the name. If a
node command works and the same thing through `api` does not, this is why. `node list`
prints both columns; the id is also one line away:

```sh
NODE=$(dbcanvas --json stack get my-lab \
  | jq -r '.design.nodes[] | select(.label=="pxc-01") | .id')
dbcanvas api GET "/api/stacks/12/nodes/$NODE/ptstalk"
```

**`--json` for anything you are going to parse.** It emits the server's own response
unchanged, so your `jq` depends on the documented API rather than on table
formatting.

**Poll, don't sleep blindly.** Jobs (`datagen`, `benchmark`, `queryrun`) return an id
and are polled. The `--wait` flags do it properly. If you must poll by hand, the job
endpoints report a terminal `status`.

**Read the node back after a restart.** Docker reassigns ephemeral host ports on
every start, so a port you noted before is stale.

**A `read` token cannot POST.** If you get a 403 naming a scope, the token is too
narrow — say so; do not retry.

**Never construct a canvas design from nothing.** Use `stack compose`; failing that,
start from a template or an export.

---

## 4. Scopes: what your token can do

| Scope | Can |
| --- | --- |
| `read` | every `GET`, plus `validate`, `compare`, `preview` and a lab step check — the POSTs that change nothing |
| `write` | everything the owning account can do in the UI |
| `admin` | additionally `/api/users/*`, `/api/admin/*`, `PUT /api/system/settings` |

`dbcanvas whoami` prints the scope. `dbcanvas endpoints --scope read` lists what a
read token could reach.

**Two things no token can ever do**, whatever its scope — both deliberate:

- `POST /api/tokens` — create another token
- `POST /api/me/password` — change the password

Both need a password sign-in. If asked to do either, use `dbcanvas token create` or
`dbcanvas password`, which prompt; in a non-interactive context, explain that the
user must run it themselves.

---

## 5. Things to be careful with

These are destructive or outward-facing. **Confirm with the user before running them**
unless they explicitly asked for that exact action.

| Command | Why |
| --- | --- |
| `dbcanvas stack destroy` | Removes every container in the stack. The design survives. |
| `dbcanvas stack delete` | Removes the design too. Not recoverable. Prompts unless `--yes`. |
| `dbcanvas api DELETE /api/users/{id}` | Deletes an account **and its stacks**. |
| `dbcanvas api POST /api/users/{id}/disable` | Cuts somebody's access, sessions and tokens. |
| `dbcanvas token revoke` | Breaks whatever was holding that token, immediately. |
| `dbcanvas password --revoke-tokens` | Breaks every script of theirs, including your own session. |
| `dbcanvas node stop` / `restart` | Interrupts a running database; ports move on restart. |
| `dbcanvas api POST …/fs/delete`, `…/fs/write`, `…/fs/chmod` | Arbitrary writes inside a node. |
| `dbcanvas api PUT /api/system/settings` | Instance-wide, affects every user. |

**Never print a token secret into a transcript, a commit or a log.** If you create
one, hand it over once and tell the user it cannot be shown again. Prefer
`$DBCANVAS_TOKEN` over pasting a literal.

**Deploying costs real resources.** An HA cluster is several containers with real
memory limits. Do not deploy speculatively on someone's machine.

---

## 6. Reading failures

| Exit | Means | Do |
| --- | --- | --- |
| 0 | success | — |
| 1 | the request failed | read the message; it is the server's own |
| 2 | bad command line | re-read the usage |
| 3 | not signed in, or the token expired or was revoked | ask the user for a token; do not guess |
| 4 | `--wait` gave up — the deploy failed or timed out | `dbcanvas stack get <stack>` for which node is in `error`, then `dbcanvas logs collect` |

HTTP statuses: `400` bad body, `401` bad credential, `403` not permitted (the message
says which scope or which ownership), `404` no such thing, `409` wrong state (node
not running, stack already deployed).

A failed deploy is diagnosed, not retried: find the node in `error`, read its logs,
fix the design.

---

## 7. When there is no subcommand

`dbcanvas api` reaches every endpoint, and there is nothing second-class about it:

```sh
dbcanvas api GET /api/stacks/12
dbcanvas api POST /api/stacks/12/deploy
dbcanvas api PUT  /api/me/settings --data '{"theme":"forest"}'
dbcanvas api POST /api/templates/import --data @pxc.json
dbcanvas api GET  /api/templates/3/export --out pxc.json
```

Find the endpoint first (`dbcanvas endpoints -q <term>`), then call it. The
`--data` value can be inline JSON, `@file` or `@-` for stdin.

The one thing it does not do is resolve names: paths are sent as written, so
`/api/stacks/12/nodes/pxc-01/...` is a lookup for a node whose *id* is `pxc-01` and
comes back `409`. See "Resolve by name" above for getting the id.

Raw `curl` needs the header every time:

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" "$DBCANVAS_URL/api/stacks"
curl -s -X POST -H "Authorization: Bearer $DBCANVAS_TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"lab","ttl":"4h"}' \
  "$DBCANVAS_URL/api/stacks"
```

Four kinds of endpoint are not plain JSON, and the catalogue marks each: `multipart`
uploads (`curl -F file=@x`), `download` responses (`curl -OJ`, or `--out` in the CLI),
the `sse` notification stream (`curl -N`), and `websocket` upgrades (the CLI, or the
UI — a WebSocket cannot be curled).

---

## 8. A worked example, end to end

> *"Reproduce a Galera flow-control stall on PXC 8.0 and get me the logs."*

```sh
# 1. Can I reach it, and what does it have?
dbcanvas whoami
dbcanvas api GET /api/catalog/pxc | jq 'keys'

# 2. Build it, with a lifetime, and wait for it. The latency and loss are the
#    reproduction — a stall is a property of the network, not of 8.0.42.
dbcanvas stack compose galera-stall --ttl 4h \
  --node 'pxc:3,version=8.0.42,os=el9,monitor,netLatencyMs=80,netLossPct=1' \
  --node pmm --dry-run
dbcanvas stack compose galera-stall --ttl 4h \
  --node 'pxc:3,version=8.0.42,os=el9,monitor,netLatencyMs=80,netLossPct=1' \
  --node pmm
dbcanvas stack validate galera-stall
dbcanvas stack deploy galera-stall --wait

# 3. Load it until the symptom appears
dbcanvas datagen run galera-stall pxc-01 --database sbtest --table sbtest1 \
  --rows 20000000 --wait
dbcanvas benchmark run galera-stall pxc-01 --workload oltp --duration 300 --wait

# 4. Capture the evidence, from every member
dbcanvas logs collect galera-stall --nodes pxc-01,pxc-02,pxc-03 --json | tee bundle.json
BUNDLE=$(jq -r .id bundle.json)
dbcanvas logs get "$BUNDLE" --json | jq '.verdict'
dbcanvas logs events "$BUNDLE" --severity error --limit 50 --json > errors.json
dbcanvas stalk start galera-stall pxc-01
sleep 60 && dbcanvas stalk download galera-stall pxc-01 --out ./

# 5. Hand it back, and ask before tearing down
dbcanvas stack get galera-stall
```

Note what that does **not** do: it does not destroy the stack. Leave the evidence
standing and ask; the TTL is the safety net if the user walks away.

---

## 9. Where the detail is

| Question | Read |
| --- | --- |
| Which endpoint, and its CLI equivalent, per feature | [`docs/API_REFERENCE.md`](https://github.com/jaimesicam/dbcanvas/blob/main/docs/API_REFERENCE.md) |
| Tokens, scopes, expiry, status codes, ownership | [`docs/API.md`](https://github.com/jaimesicam/dbcanvas/blob/main/docs/API.md) |
| CLI profiles, `--json`, `--wait`, exit codes, CI notes | [`docs/CLI.md`](https://github.com/jaimesicam/dbcanvas/blob/main/docs/CLI.md) |
| What a node or cluster type actually is | [`docs/STACKS.md`](https://github.com/jaimesicam/dbcanvas/blob/main/docs/STACKS.md) |
| Installing and configuring DBCanvas | [`docs/CONFIGURATION.md`](https://github.com/jaimesicam/dbcanvas/blob/main/docs/CONFIGURATION.md) |
| **This installation's live endpoint list** | `dbcanvas endpoints --long` |

When this file and `dbcanvas endpoints` disagree, `dbcanvas endpoints` is right: it
comes from the running server, and this file is written by hand.
