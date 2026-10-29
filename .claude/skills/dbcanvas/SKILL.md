---
name: dbcanvas
description: Drive a DBCanvas installation over its HTTP API or with dbcanvas-cli — deploy database stacks (PostgreSQL, MySQL/PXC, MongoDB, Valkey, clustered or standalone), run load against them, and diagnose them from packet captures, logs, FTDC, pt-stalk and core dumps. Use when asked to spin up a database lab, reproduce a database problem on a specific version or topology, generate test data, benchmark a cluster, or read a stack's logs and captures. Also configures every deploy option, relationship and post-deployment action of a stack through the design catalogue and design endpoints, and manages user accounts. Triggers on dbcanvas, dbcanvas-cli, "spin up a stack", "deploy a PXC cluster", "database lab", DBCANVAS_TOKEN, /api/stacks, design catalog.
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
invent, guess or brute-force one. If `DBCANVAS_TOKEN` is not set, no profile exists
and the user has not given you a password, stop and ask them to either run
`dbcanvas login` themselves or paste a token they created in **API → Tokens**:

```sh
export DBCANVAS_URL=http://localhost:8080
export DBCANVAS_TOKEN=dbc_...
```

**A password the user gave you is yours to use.** The CLI does not need a terminal:
when stdin is not one, every password prompt reads one line from stdin, in the order
the prompts appear. So pipe them in — never put a password in a flag, an environment
variable, a file or your reply:

```sh
printf '%s\n' "$PW" | dbcanvas login --url http://localhost:8080 --user alice
```

Whatever you pipe is still in your transcript, so say once that this suits a lab
instance and that a password used this way is worth rotating on a shared one.

### Setting up an account for somebody, and working as it

The request looks like: *"DBCanvas is at URL, the admin is A with password P — create
user U and use DBCanvas as U."* Every account has a first name, a last name and an
email; ask for any you were not given. U's own password is one the user names, or one
you generate and tell them once (at least 8 characters, no leading or trailing space).
Use one profile per account and revoke the admin's token when you are done.

**With `dbcanvas-cli`:**

```sh
URL=http://localhost:8080
# 1. A short-lived admin token, in its own profile.
printf '%s\n' "$ADMIN_PW" |
  dbcanvas login --url "$URL" --user admin --scope admin --expires 1 --profile admin

# 2. The account. Prompts, in order: the admin's password, then U's temporary
#    password twice. (--invite instead prints a 7-day link and asks only the first.)
printf '%s\n' "$ADMIN_PW" "$TEMP_PW" "$TEMP_PW" |
  dbcanvas --profile admin user create bob --first Bob --last Lee --email bob@example.net

# 3. U's first sign-in. Prompts: the temporary password, then U's own twice — an
#    account with an admin-set password can do nothing else until it is replaced.
printf '%s\n' "$TEMP_PW" "$USER_PW" "$USER_PW" |
  dbcanvas login --url "$URL" --user bob --profile bob

# 4. Work as U, then drop the admin token.
dbcanvas --profile bob whoami
dbcanvas --profile admin logout
```

**With `curl`** (no CLI installed). Account creation and password changes refuse API
tokens, so steps 1–3 run on a password **session**: a cookie in a jar that is deleted at
the end. Bodies are built with `jq -n --arg`, which escapes any character a password
holds, and sent on stdin (`--data @-`) so no password is ever in a command-line
argument. Every response is JSON; an error is `{"error": "…"}` with a 4xx status.

```sh
URL=http://localhost:8080
JAR=$(mktemp); trap 'rm -f "$JAR"' EXIT
post() { curl -sS -b "$JAR" -c "$JAR" -X POST "$URL$1" \
           -H 'Content-Type: application/json' --data @- ; }

# 1. Admin session.
jq -n --arg u admin --arg p "$ADMIN_PW" '{username:$u, password:$p}' | post /api/auth/login

# 2. The account. "password" is the temporary one; leave it out (or "") to get
#    {"url": …, "expiresAt": …} — a 7-day link for U to choose one in a browser.
jq -n --arg p "$TEMP_PW" '{username:"bob", role:"user", password:$p,
  firstName:"Bob", lastName:"Lee", email:"bob@example.net"}' | post /api/users

# 3. Sign the admin out, then U's first sign-in: the login response says
#    "mustChangePassword": true, and until the change only it is allowed.
post /api/auth/logout </dev/null
jq -n --arg u bob --arg p "$TEMP_PW" '{username:$u, password:$p}' | post /api/auth/login
jq -n --arg c "$TEMP_PW" --arg n "$USER_PW" '{currentPassword:$c, newPassword:$n}' |
  post /api/me/password

# 4. A token for U (same session), then sign the session out.
DBCANVAS_TOKEN=$(jq -n '{name:"bob via curl", scope:"write", days:30}' |
  post /api/tokens | jq -r .secret)
post /api/auth/logout </dev/null

# 5. Work as U.
curl -sS -H "Authorization: Bearer $DBCANVAS_TOKEN" "$URL/api/me"
```

No `jq`? Build the JSON with `python3 -c 'import json,sys; …'` — never by pasting a
password into a JSON string by hand, where a quote or backslash in it breaks the body.
Keep the token out of your reply; it is a password for the account.

### Managing accounts (admin)

Everything **Manage Users** does is an endpoint under `/api/users` (admin only; list
them with `dbcanvas endpoints -q users`). The ones you reach for most:

```sh
dbcanvas user list                                          # pending first
dbcanvas api GET    /api/users/7                            # one account: stacks, active tokens, sessions
dbcanvas api POST   /api/users/7/approve                    # or /reject, /disable
dbcanvas api POST   /api/users/7/role -d '{"role":"admin"}'
dbcanvas api GET    /api/users/7/stacks
dbcanvas api GET    /api/users/7/tokens                     # never the secrets
dbcanvas api DELETE /api/users/7/tokens                     # revoke them all; sessions untouched
dbcanvas api DELETE /api/users/7/sessions                   # sign it out everywhere
dbcanvas api POST   /api/stacks/12/owner -d '{"userId":3}'  # hand one stack over
dbcanvas api DELETE '/api/users/7?transferStacksTo=3'       # delete, keeping its stacks
```

**Deleting an account deletes its stacks and their containers** unless
`transferStacksTo` names an approved account to give them to. Ask before either.

Sign-ups wait for approval unless an administrator has turned on open registration
(`openRegistration` in `GET /api/system/settings`; `PUT` it to change). It is an
instance-wide decision about who may use the installation: do not turn it on to get
yourself an account — ask for one (§1).

### Know what this installation actually has

Endpoints and installable versions differ per installation. **Read, don't assume:**

```sh
dbcanvas endpoints --long              # every endpoint this server serves
dbcanvas endpoints -q backup           # find one
dbcanvas stack kinds                   # what compose can build here
dbcanvas api GET /api/stacks/design/catalog   # every node/cluster type, option, relation and action
dbcanvas api GET /api/catalog/ps       # which Percona Server versions exist here
dbcanvas template list                 # which templates exist here
```

Never hardcode a version string you have not seen in a catalogue response.

---

## 2. The six things you will be asked to do

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
| `vault` | data-at-rest keys — on `pxc` and `ps-repl` the whole cluster, one KV mount per member | `openbao` |
| `backup` | pgBackRest / Barman / PBM to S3 | `seaweedfs` |
| `orchestrator` | topology discovery, failover | `orchestrator` |

Ask for one with no provider in the spec and the error names the node to add. Ask for
one a kind does not have and the error names the kind. Run `dbcanvas stack kinds` if
you are unsure — it prints the matrix for that installation.

**`to=` is different, and easy to miss.** A relationship above is a *field*. `to=` is
an association **line**, and six kinds do nothing without one — a ProxySQL or a
PgBouncer with no backend, a simulator with no database. Compose refuses rather than building those, so
you will see the error; the fix is to add the target to the spec, or name it:

```sh
dbcanvas stack compose pg --ttl 4h \
  --node 'patroni:3,os=el9' \
  --node 'haproxy,os=el9' \
  --node 'pgvectorsim,to=haproxy-01'
```

The HAProxy needed no `to=` — one legal backend, so compose used it and said so in the
plan. The pgvector Support Sim needed one, because using the cluster directly and using
it through the proxy are different tests and it will not guess. `dbcanvas stack kinds`
prints each kind's legal targets in the LINKS TO column.

**PgBouncer is the PostgreSQL pool**, as HAProxy is the TCP balancer: one `pgbouncer`
node in front of one `pg` node or one Patroni, repmgr or Spock cluster. `mode=` is the
pool mode (`transaction` by default, `session`, `statement`), `cert` terminates TLS at
the pool, and compose turns on following the primary across a failover and
`auth_query`. A pgvector Support Sim can use the cluster through it:

```sh
dbcanvas stack compose pool-lab --ttl 4h \
  --node 'patroni:3,os=el9' \
  --node 'pgbouncer,os=el9,mode=transaction,cert' \
  --node 'pgvectorsim,to=pgbouncer-01'
```

With two PostgreSQL targets in the spec, the PgBouncer needs `to=` as well.

**Encryption at rest on a cluster is the frame's flag.** `--node 'pxc:3,os=el9,vault'`
with `--node openbao` keys every member, each with its own KV mount, and turns on
encrypted cluster traffic, which PXC requires once a keyring is configured. The deploy
checks every member for a loaded keyring. `ps-repl` keys every member the same way.

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

**Anything the shorthand does not cover, set by name.** Every compose entry also takes
`options` (fields of the node, or of a cluster's frame) and `memberOptions` (fields on
every member), named as the design catalogue names them — see "Every option, by name"
below. In the CLI that means `--spec spec.json` (or `--spec -`) or the `curl` form; they are checked
the same way, and an unknown field is refused.

If `compose` refuses a kind — a Kubernetes frame, an All-in-One, the Stock Market Sim,
a core-dump host — it says why. Compose the rest, then add those with the design
endpoints (below), or start from a template:

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

Spell the version out. `auth_openid_connect` arrived in Percona Server 8.4.11-11 and
9.7.2-2, and below those the provisioner skips the plugin *without failing* — no error at compose,
validate or deploy, just a node with no SSO. Compose refuses it instead, naming the
minimum.

To prove it works, on the node: `oidc-login jane` (seeded users are `jane` and `john`,
password `KEYCLOAK_USER_PASSWORD`), then **`SET ROLE accounting;`** before querying
`oidc_demo`. The Keycloak group maps to a role that is granted but *not activated*, so
without `SET ROLE` the query fails with *SELECT command denied* and looks like the SSO
is broken when it is not.

**Never author a whole canvas design by hand.** It is one document of ~335
type-discriminated fields with cross-references by generated id. Compose it, or start
from a template, then change it one piece at a time with the design endpoints, which
check every field against the catalogue.

### Every option, by name: the design catalogue and the design endpoints

`GET /api/stacks/design/catalog` is the designer's properties panels as data. For each
node type (`nodes`) and cluster type (`frames`) it lists:

- `options` — `field`, JSON `type`, accepted `values`, the `default` a new one gets,
  and `when` it applies (`"usePgBackRest is on"`, `"k3dOperator is pxc or ps"`). A
  cluster's own settings are the frame entry's options; per-member settings are the
  member node entry's (`memberType`).
- `relations` — named references to another node: `monitor` (PMM), `backup`
  (SeaweedFS), `vault` (OpenBao), `oidc` (Keycloak), `ldap`/`kerberos` (a directory),
  `orchestrator`, `repository`, `storage` (Ceph), `upgrade` (Watchtower), `app` (an
  All-in-One). Top-level `relations` says which field each is stored in.
- `edges` — the lines it draws (a proxy's backend, a simulator's database, replication
  between clusters) and what may be at the other end. Provisioners walk these.
- `actions` — **what you can do once it is deployed**: backups and restores,
  switchover, certificates, database users, captures, with each one's body and when it
  applies. `commonActions` (start, stop, rebuild, logs, shell, files) apply to every node.

```sh
CAT='/api/stacks/design/catalog'
dbcanvas api GET "$CAT?type=patroni" | jq '.frame.options[] | {field, type, values, default, when}'
dbcanvas api GET "$CAT?type=patroni" | jq '.node.actions[] | "\(.method) \(.path) — \(.description)"'
dbcanvas api GET "$CAT" | jq -r '.frames[].type'      # every cluster type
```

Then change a stack's design one thing at a time. Ids, not labels, in paths (§3);
`GET /api/stacks/{id}/design` shows both.

```sh
S=12
dbcanvas api POST /api/stacks/$S/design/nodes  -d '{"type":"pmm"}'
dbcanvas api POST /api/stacks/$S/design/frames -d '{"type":"pxc","members":3,
  "options":{"pxcMajor":"8.4","generateCert":true},"relations":{"monitor":"<pmm id>"}}'
dbcanvas api POST /api/stacks/$S/design/nodes  -d '{"frameId":"<frame id>"}'        # one more member
dbcanvas api POST /api/stacks/$S/design/nodes  -d '{"type":"proxysql"}'
dbcanvas api POST /api/stacks/$S/design/edges  -d '{"from":"<frame id>","to":"<proxysql id>"}'
dbcanvas api PUT  /api/stacks/$S/design/frames/<frame id> -d '{"memberOptions":{"cpus":2}}'
dbcanvas api PUT  /api/stacks/$S/design/nodes/<id> -d '{"relations":{"monitor":null}}'  # null clears
dbcanvas api DELETE /api/stacks/$S/design/nodes/<id>
dbcanvas stack validate $S && dbcanvas stack deploy $S --wait
```

- Each call answers with what it made or changed, plus the validator's `issues` and
  `ok`. Issues do not block an edit (a design is often incomplete half-way); read them
  before deploying.
- A refusal names what is accepted: the type's options, the relation's target types,
  the member range. Fix the request; do not fall back to `PUT /api/stacks/{id}` to get
  a field past the check. A field that is refused does nothing on that type.
- `PUT` changes only what the body names; an option set to `null` goes back to its
  zero value.
- Removing a node or cluster also removes its lines and clears relations to it, and
  **removes its container if deployed**. Ask before removing anything deployed.
- A sharded `psmdb` cluster's shape comes from `options.psmdbSetup` (`standard` or
  `minimum`), not `members`.
- Nodes cannot be added or removed while a deploy is running; option changes can.

**Always set a `--ttl`.** `2h`, `4h`, `8h`, `24h`, `2w`, `infinity`. A lab you forget
runs until somebody notices. Only use `infinity` if the user asked for it.

**`--wait` is not optional if anything comes next.** `deploy` returns as soon as
provisioning *starts*; without `--wait` your next command runs against a cluster that
does not exist yet. `--wait` exits **4** if the deploy failed.

Deploying is slow — minutes, sometimes tens of minutes for an HA cluster. Say so
before you start rather than after.

### A, without the CLI: the same lab over `curl`

Every step above is one endpoint. `$DBCANVAS_TOKEN` is a token with `write` scope (§1
shows how to get one from a password), and the stack belongs to that token's account.

```sh
URL=http://localhost:8080
api() { curl -sS -H "Authorization: Bearer $DBCANVAS_TOKEN" \
          -H 'Content-Type: application/json' "$@"; }

# What this installation has — never guess a kind, an option or a version.
api "$URL/api/meta/endpoints" | jq -r '.groups[].endpoints[] | "\(.method) \(.path)"' |
  grep -E ' /api/stacks(/compose(/kinds)?|/\{id\}(/(validate|deploy|destroy))?)?$'
api "$URL/api/stacks/compose/kinds" | jq -c '.kinds[] | {kind, options, needs}'
api "$URL/api/catalog/ps" | jq -c '.images[] | {os, osVersion, series: (.versions | keys)}'

# The spec is the JSON form of --node: one object per entry, "kind" required,
# every other field optional (count, version, os, monitor, ldap, oidc, vault,
# backup, to, netLatencyMs, …). The CLI's 'ps,version=8.4,os=el9' is:
SPEC='{"name":"repro-1234","ttl":"2h",
       "nodes":[{"kind":"ps","version":"8.4","os":"el9"}]}'

# 1. The plan. Nothing is created; read .resolved and .added (what compose added
#    for you — the Intranet, a Keycloak's desktop…). A full version ("8.4.11")
#    resolves to the exact build; a series ("8.4") shows "version": null and means
#    its newest build at deploy time.
echo "$SPEC" | jq '.dryRun = true' | api -X POST "$URL/api/stacks/compose" --data @- |
  jq '{ok, added, resolved: [.resolved[] | select(.os != "")
       | {kind, major, version, os: (.os + " " + .osVersion)}]}'

# 2. Create it. The response carries the new stack and the validator's issues;
#    "ok": false means an issue of level "error" — read it before deploying.
STACK=$(echo "$SPEC" | api -X POST "$URL/api/stacks/compose" --data @- |
  tee /dev/stderr | jq -r 'if .ok then .stack.id else empty end')
[ -n "$STACK" ] || { echo "compose refused or validation failed — see above"; exit 1; }

# 3. Deploy, then wait the way `--wait` does: poll until every node is "running"
#    or "error". A single Percona Server takes a few minutes; a cluster, longer.
api -X POST "$URL/api/stacks/$STACK/deploy" >/dev/null
for i in $(seq 1 120); do
  STATES=$(api "$URL/api/stacks/$STACK" | jq -r '.deployments[] | "\(.nodeId) \(.state)"')
  echo "$STATES"; echo ---
  [ -n "$STATES" ] && ! echo "$STATES" | grep -vqE ' (running|error)$' && break
  sleep 10
done
echo "$STATES" | grep -q ' error$' && echo "a node failed — read its log before retrying"

# 4. Node ids are opaque; the labels you will be asked about live in the design.
api "$URL/api/stacks/$STACK" |
  jq '[.design.nodes[] | {id, label, type}] as $n
      | .deployments[] | . as $d | ($n[] | select(.id == $d.nodeId)) + {state}'
```

Tearing down is two calls and both are outward-facing — **ask first** (§5):
`POST /api/stacks/$STACK/destroy` removes the containers and keeps the design;
`DELETE /api/stacks/$STACK` deletes the stack once nothing is running.

### B. Reproduce a problem on a specific version or topology

Usually this is just compose with the version written out:

```sh
dbcanvas stack compose bug-4821 --node 'ps,version=8.0.42,os=el9' --dry-run
```

If the version is not available the error lists what is, in that series. For a shape
compose does not build, compose what it can and change the rest with the design
endpoints ("Every option, by name", above):

```sh
dbcanvas api POST /api/stacks/12/design/frames -d '{"type":"k3d","options":{"k3dOperator":"pxc"}}'
dbcanvas stack validate 12                           # check before building
dbcanvas stack deploy 12 --wait
```

Replacing the whole document (`stack export`, edit, `PUT /api/stacks/{id}`) still works,
but nothing checks the fields; prefer the design endpoints.

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
dbcanvas logs events <bundle-id> --severity bad
dbcanvas capture start repro-1234 pxc-01 --seconds 30            # decoded packets
dbcanvas stalk start repro-1234 pxc-01                           # pt-stalk
dbcanvas ftdc node mongo-lab psmdb-01                            # MongoDB's black box
```

Collecting logs from **several** nodes at once is the point — the comparison across
members is usually where the answer is.

A `mysqld` **core dump** is read in the Core Dump Analyzer, which runs gdb over a
WebSocket and so is UI-only; send the user there. From the CLI you can check that a
core-dump host exists and that its symbols match the core:

```sh
dbcanvas api GET /api/gdb/targets
dbcanvas api GET /api/stacks/12/nodes/<node-id>/gdb/cores   # ids, not names — see §3
```

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

### F. Check whether the installation is current

```sh
dbcanvas updates          # this build, the latest on GitHub, and what changed in between
dbcanvas updates --full   # with each release note's text
```

This is the dashboard's **Check for updates** button. The *server* asks GitHub, so
the answer is about the installation you are signed in to, not the CLI binary. It
is the only thing in DBCanvas that contacts GitHub, and it only does so when called.
A `dev` build cannot be compared. If the server has no route out, the command
exits 1 with a 502. Updating is the user's call (`git pull && make compose` on the host). Report
what is available and do not run the update yourself.

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
a template. Change it with the design endpoints, not by editing the JSON.

**Look up post-deployment actions; don't guess paths.** What a deployed node or cluster
can do is its catalogue entry's `actions` (`GET /api/stacks/design/catalog?type=<type>`),
each with its method, path, body and when it applies.

---

## 4. Scopes: what your token can do

| Scope | Can |
| --- | --- |
| `read` | every `GET`, plus `validate`, `compare`, `preview` and a lab step check — the POSTs that change nothing |
| `write` | everything the owning account can do in the UI |
| `admin` | additionally `/api/users/*`, `/api/admin/*`, `PUT /api/system/settings` |

`dbcanvas whoami` prints the scope. `dbcanvas endpoints --scope read` lists what a
read token could reach.

**Things no token can ever do**, whatever its scope — all deliberate:

- `POST /api/tokens` — create another token
- `POST /api/me/password` — change the password
- `POST /api/users` — create an account (and setting another account's password or
  issuing it a reset link)

All need a password sign-in: `dbcanvas token create`, `dbcanvas password` and
`dbcanvas user create` sign in for the one request and sign out, reading the password
from stdin when there is no terminal (§1). With the password from the user, run them;
without it, ask — do not report the 403 a token gets as a dead end. The curl form of
creating an account is in §1, *Setting up an account for somebody*.

---

## 5. Things to be careful with

These are destructive or outward-facing. **Confirm with the user before running them**
unless they explicitly asked for that exact action.

| Command | Why |
| --- | --- |
| `dbcanvas stack destroy` | Removes every container in the stack. The design survives. |
| `dbcanvas stack delete` | Removes the design too. Not recoverable. Prompts unless `--yes`. |
| `dbcanvas api DELETE /api/users/{id}` | Deletes an account **and its stacks**. |
| `dbcanvas user create` | Creates an account somebody can sign in with — an administrator if `--role admin`. |
| `dbcanvas api POST /api/users/{id}/disable` | Cuts somebody's access, sessions and tokens. |
| `dbcanvas api POST /api/users/{id}/role` | Grants or removes somebody's administrator access. |
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
dbcanvas --json logs events "$BUNDLE" --severity bad --limit 50 > errors.json
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
