# API & CLI reference

Every feature of DBCanvas with its endpoints and the `dbcanvas-cli` command that calls
them. For authentication, scopes and status codes, read [HTTP API](API.md); for CLI
profiles, `--json`, `--wait` and exit codes, read [`dbcanvas-cli`](CLI.md).

The always-current list is the **API** page in the app, or:

```sh
dbcanvas endpoints --long
```

**Conventions on this page**

- `…/nodes/{nid}` is short for `/api/stacks/{id}/nodes/{nid}`, and `…/frames/{fid}`
  for `/api/stacks/{id}/frames/{fid}`. A *frame* is a cluster on the canvas.
- Where the CLI column says `dbcanvas api …`, there is no dedicated subcommand;
  `dbcanvas api METHOD PATH [--data JSON|@file] [--out FILE]` calls any endpoint.
- Global CLI flags such as `--json` go **before** the command:
  `dbcanvas --json query run …`.

## Contents

**Getting in** · [Sign-in & accounts](#sign-in--accounts) · [Your account](#your-account) · [API tokens](#api-tokens) · [Users (admin)](#users-admin) · [Instance settings (admin)](#instance-settings-admin)

**Building** · [Stacks](#stacks) · [Templates](#templates) · [Nodes](#nodes) · [Clusters & backups](#clusters--backups) · [Version catalogues](#version-catalogues) · [Kubernetes frames](#kubernetes-frames) · [All in One](#all-in-one) · [Repository, Ceph and Linux Client nodes](#repository-ceph-and-linux-client-nodes)

**Running load** · [Data Generator](#data-generator) · [Query Runner](#query-runner) · [Database Explorer](#database-explorer) · [Benchmark](#benchmark) · [Stock Market Sim](#stock-market-sim) · [Sample Client Code](#sample-client-code) · [Ledger Sim](#ledger-sim)

**Finding out what happened** · [Activity](#activity) · [Packet Inspector](#packet-inspector) · [Log Summary](#log-summary) · [FTDC Summary](#ftdc-summary) · [Stalk Summary](#stalk-summary) · [Operator Summary](#operator-summary) · [Diagnostic captures](#diagnostic-captures) · [Operator Debugger](#operator-debugger) · [Core Dump Analyzer](#core-dump-analyzer)

**Managing a node** · [Node file manager](#node-file-manager) · [Certificates](#certificates) · [Intranet mail](#intranet-mail) · [Intranet LDAP](#intranet-ldap) · [Intranet DNS](#intranet-dns) · [Samba AD DC](#samba-ad-dc) · [SeaweedFS](#seaweedfs) · [OpenBao](#openbao)

**Watching** · [Dashboard](#dashboard) · [Notifications](#notifications) · [Kanban](#kanban) · [Shared sessions](#shared-sessions) · [API metadata](#api-metadata)

---

# Getting in

## Sign-in & accounts

These endpoints need no session.

| To do this | API | CLI |
| --- | --- | --- |
| Is this installation set up, and who am I | `GET /api/setup/status` | `dbcanvas version` (also prints the server version) |
| Create the first account (it becomes the administrator) | `POST /api/setup` | *(UI only)* |
| Request an account | `POST /api/auth/register` `{"username", "password", "firstName", "lastName", "email", "avatar"}` | *(UI only)* |
| Sign in and get a session cookie | `POST /api/auth/login` | `dbcanvas login` |
| Sign out | `POST /api/auth/logout` | `dbcanvas logout` (also revokes the CLI's token) |
| Check a password reset link · set the new password | `GET` · `POST /api/auth/reset/{token}` | *(UI only)* |

Names and email are required to register, and an email belongs to one account. The
response's `status` is `"pending"` (waiting for an administrator) or `"approved"`
(when open registration is on).

`dbcanvas login` signs in, creates an API token, signs out and keeps only the token —
see [`dbcanvas-cli`](CLI.md#signing-in).

```sh
dbcanvas login --url http://localhost:8080
```

## Your account

**UI:** **Profile** (name, avatar, password, tokens) and **Settings** (preferences).

| To do this | API | CLI |
| --- | --- | --- |
| Who am I | `GET /api/me` | `dbcanvas whoami` |
| Change your name, email and avatar | `PUT /api/me/profile` `{"firstName", "lastName", "email", "avatar"}` | — |
| The avatars to choose from | `GET /api/avatars` | — |
| Read your preferences | `GET /api/me/settings` | `dbcanvas api GET /api/me/settings` |
| Change them | `PUT /api/me/settings` | `dbcanvas api PUT /api/me/settings --data '{"theme":"forest","look":"industrial","maxTabs":20}'` |
| **Change your password** | `POST /api/me/password` | `dbcanvas password [--revoke-tokens]` |
| Your kept Scratchboard boards | `GET /api/me/scratchboards` · `PUT`/`DELETE /api/me/scratchboards/{bid}` | — |
| Release notes, and whether you have read them | `GET /api/whatsnew` · `POST /api/whatsnew/seen` | `dbcanvas api GET /api/whatsnew` |
| Node images beyond the OS bases, and which are present | `GET /api/images` | `dbcanvas api GET /api/images` |
| Build one of them (admin) | `POST /api/images/{id}/build` | `dbcanvas api POST /api/images/bighole/build` |

Preference fields (`PUT` changes only the fields you send; an unknown value falls back
to the default):

| Field | Values |
| --- | --- |
| `theme` | `glass-light` (default), `glass-dark`, `light`, `dark`, `midnight`, `solarized`, `synthwave`, `forest` |
| `look` | `glass` (default), `modern`, `industrial`, `editorial`, `soft` |
| `maxTabs` | 2–40 (default 20) |
| `terminalMode` | `docked`, `undocked` |
| `terminalsOnTop` | `on` unless exactly `off` |
| `tooltips` | `on` unless exactly `off` |
| `deploymentBackend` | `docker`, `vagrant` |
| `nodeLibrary` | `menu`, `docked` |
| `scratchboard` | `off`, `on` (keep chosen boards on the server) |

![The change-password form](screenshots/settings-password.png)

**Changing your password** needs your current password, and an API token cannot do it.
It signs out every other session and keeps yours. Your API tokens keep working unless
you tick **Also revoke my API tokens** (`revokeTokens`, or `--revoke-tokens`).

```sh
$ dbcanvas password
Changing the password for alice on http://localhost:8080.
Current password: ********
New password: ********
New password again: ********
Password changed. Every other session was signed out.
Your API tokens still work. Pass --revoke-tokens if you wanted them gone too.
```

> [!TIP]
> Forgotten your password? An administrator can set a new one or send you a reset
> link from **Manage Users** (see [Users](#users-admin)). If no administrator can sign
> in, see [Configuration](CONFIGURATION.md#recovering-an-admin-password).

## API tokens

**UI:** **API → Tokens**. See [HTTP API](API.md#create-a-token) for scopes and expiry.

![API → Tokens: the create form and the token table](screenshots/api-tokens.png)

| To do this | API | CLI |
| --- | --- | --- |
| List your tokens (active, expired and revoked) | `GET /api/tokens` | `dbcanvas token list` |
| Create one (password sign-in only) | `POST /api/tokens` `{"name", "scope", "days"}` | `dbcanvas token create --name N --scope write --expires 30` |
| Revoke one of yours | `DELETE /api/tokens/{id}` | `dbcanvas token revoke <id>` |
| List every token on the instance (admin) | `GET /api/admin/tokens` | `dbcanvas token list --all` |
| Revoke anyone's (admin) | `DELETE /api/admin/tokens/{id}` | `dbcanvas token revoke <id> --admin` |

The secret is returned once, by the create call. `days: 0` means never expires
(administrators only).

## Users (admin)

**UI:** **Manage Users**.

![Manage Users: the account list](screenshots/manage-users.png)

| To do this | API | CLI |
| --- | --- | --- |
| List accounts, pending first, with last sign-in, activity and session count | `GET /api/users` | `dbcanvas user list` |
| Read one, with its stack and active token counts | `GET /api/users/{id}` | `dbcanvas api GET /api/users/1` |
| The stacks it owns | `GET /api/users/{id}/stacks` | `dbcanvas api GET /api/users/1/stacks` |
| Its API tokens (never the secrets) | `GET /api/users/{id}/tokens` | `dbcanvas api GET /api/users/1/tokens` |
| Revoke all its API tokens | `DELETE /api/users/{id}/tokens` | `dbcanvas api DELETE /api/users/1/tokens` |
| Create an approved account (password sign-in only) | `POST /api/users` | `dbcanvas user create bob --first Bob --last Lee --email bob@example.net [--role admin] [--invite]` |
| Change its username and profile | `PUT /api/users/{id}` | `dbcanvas api PUT /api/users/1 --data '{"username":"pat"}'` |
| Approve · reject · disable | `POST /api/users/{id}/approve` · `/reject` · `/disable` | `dbcanvas api POST /api/users/1/approve` |
| Delete it and its stacks, or hand the stacks over first | `DELETE /api/users/{id}[?transferStacksTo={userId}]` | `dbcanvas api DELETE '/api/users/1?transferStacksTo=3'` |
| Make it an administrator, or a user again | `POST /api/users/{id}/role` `{"role"}` | `dbcanvas api POST /api/users/1/role --data '{"role":"admin"}'` |
| Set its password (password sign-in only) | `POST /api/users/{id}/password` | — |
| Create a one-time reset link (password sign-in only) | `POST /api/users/{id}/reset-link` | — |
| List its sessions | `GET /api/users/{id}/sessions` | `dbcanvas api GET /api/users/1/sessions` |
| Sign it out everywhere, or of one session | `DELETE /api/users/{id}/sessions[/{sid}]` | `dbcanvas api DELETE /api/users/1/sessions` |
| Sign every account out except your own session | `DELETE /api/users/sessions` | `dbcanvas api DELETE /api/users/sessions` |
| Clear sign-in history for one account, or everyone | `DELETE /api/users/{id}/sign-ins` · `DELETE /api/users/sign-ins` | `dbcanvas api DELETE /api/users/sign-ins` |
| Hand a stack to another account | `POST /api/stacks/{id}/owner` `{"userId"}` | `dbcanvas api POST /api/stacks/12/owner --data '{"userId":3}'` |

- **Reject** or **disable** revokes the account's sessions and API tokens together.
  Revoking tokens alone leaves sessions and password untouched; the owner is notified.
- **Delete** removes the account's stacks and their containers. With
  `?transferStacksTo=` they move, still running, to another approved account. A moved
  stack's new owner is notified.
- You cannot change your own role. A role change applies on the account's next
  request without signing it out.
- **Add user** creates an approved account with either a 7-day link to choose a
  password (`--invite`) or a temporary password the owner must change at first
  sign-in.
- **Reset password** either sets a new password (signs the account out everywhere,
  optionally revokes its tokens; temporary by default) or returns a
  `/reset-password/<token>` link that works once, for 24 hours. Neither works on your
  own account.
- **Sessions** lists where an account is signed in. Your current session is marked
  *This browser*. **Sign out everyone** keeps your own session; passwords and tokens
  are untouched. **Clear sign-in history** signs nobody out.

## Instance settings (admin)

**UI:** **Settings** (the instance-wide rows are editable by administrators only).

![The Settings page](screenshots/settings.png)

| To do this | API | CLI |
| --- | --- | --- |
| Read the instance settings | `GET /api/system/settings` | `dbcanvas api GET /api/system/settings` |
| Change them (admin) | `PUT /api/system/settings` | `dbcanvas api PUT /api/system/settings --data '{"maxTokenDays":30}'` |

A `PUT` keeps any field it leaves out, and out-of-range values are clamped, not
refused.

| Field | Settings row | Default |
| --- | --- | --- |
| `maxUploadBytes` | Node file uploads | 4 GiB (1 MiB–1 TiB) |
| `maxTokenDays` | API token lifetime | 90 (1–365) |
| `internalWrites` | Writes to internal databases (PMM's own databases in the Database Explorer) | off |
| `openRegistration` | Registration: approve sign-ups at once and sign them in | off |
| `allowGuestSessions`, `maxGuestMinutes`, `sessionRetentionDays`, `recordingRetentionDays` | Shared sessions | off, 120 (5–120), 90 (`0` keeps them while the stack exists), 30 (1–3650) |
| `liveWatchSeconds` | Stack history and alerts: sampling interval | 30 (15–600; `0` off) |

Read-only fields come from the server's environment: `sshForwarding`
(`SSH_FORWARDING_HOST`), `experimental`, `eol` and `publicUrl`. With open registration
off, a new account waits under **Manage Users**; turning it on approves no one already
pending. `GET /api/setup/status` reports `openRegistration` too.

---

# Building

## Stacks

A stack is a design on the canvas plus the containers it deployed.

![The Database Stacks canvas with a deployed stack](screenshots/stacks-canvas.png)

| To do this | API | CLI |
| --- | --- | --- |
| List your stacks | `GET /api/stacks` | `dbcanvas stack list` |
| **Build one from a short description** | `POST /api/stacks/compose` | `dbcanvas stack compose <name> --node …` |
| What compose can build | `GET /api/stacks/compose/kinds` | `dbcanvas stack kinds` |
| Create one from a template or a design | `POST /api/stacks` `{"name", "ttl", "templateId" or "design"}` | `dbcanvas stack create <name> --template <t> --ttl 4h` (or `--design file.json`) |
| Read one, with every node's state | `GET /api/stacks/{id}` | `dbcanvas stack get <stack>` |
| Replace its name, TTL or design | `PUT /api/stacks/{id}` | `dbcanvas api PUT /api/stacks/1 --data @design.json` |
| Read the design alone | `GET /api/stacks/{id}/design` | `dbcanvas api GET /api/stacks/12/design` |
| **What a design can hold** | `GET /api/stacks/design/catalog[?type=pxc]` | `dbcanvas api GET '/api/stacks/design/catalog?type=pxc'` |
| Add · change · remove a node | `POST /api/stacks/{id}/design/nodes` · `PUT`/`DELETE …/design/nodes/{nid}` | `dbcanvas api POST /api/stacks/12/design/nodes --data '{"type":"pmm"}'` |
| Add · change · remove a cluster | `POST /api/stacks/{id}/design/frames` · `PUT`/`DELETE …/design/frames/{fid}` | `dbcanvas api POST /api/stacks/12/design/frames --data '{"type":"pxc","members":3}'` |
| Draw · remove a line | `POST /api/stacks/{id}/design/edges` · `DELETE …/design/edges/{eid}` | `dbcanvas api POST /api/stacks/12/design/edges --data '{"from":"…","to":"…"}'` |
| Check the design without building | `POST /api/stacks/{id}/validate` | `dbcanvas stack validate <stack>` |
| What a deploy would do now | `GET /api/stacks/{id}/deploy/preview` | `dbcanvas api GET /api/stacks/12/deploy/preview` |
| Deploy | `POST /api/stacks/{id}/deploy` | `dbcanvas stack deploy <stack> [--wait] [--timeout 60m]` |
| Live CPU, memory, I/O and replication role per node | `GET /api/stacks/{id}/live` | `dbcanvas api GET /api/stacks/12/live` |
| Extend its lifetime | `POST /api/stacks/{id}/extend` `{"by": "4h"}` | `dbcanvas api POST /api/stacks/12/extend --data '{"by":"4h"}'` |
| Remove the containers, keep the design | `POST /api/stacks/{id}/destroy` | `dbcanvas stack destroy <stack>` |
| Delete both | `DELETE /api/stacks/{id}` | `dbcanvas stack delete <stack> [--yes]` |
| Export the design as JSON | `GET /api/stacks/{id}` | `dbcanvas stack export <stack> > lab.json` |

![The New stack dialog with a template selected](screenshots/getting-started-new-stack.png)

**Deploy returns at once.** Progress is each node's `state` in `GET /api/stacks/{id}`:
`pending` → `provisioning` → `running`, or `error` (`stopped` when stopped on
purpose). `--wait` polls for you and exits **4** if the deploy failed.

![A deployed stack — Intranet, PMM and a Percona Server node](screenshots/getting-started-deployed.png)

```sh
dbcanvas stack create pxc-lab --template "PXC + ProxySQL + PMM" --ttl 4h --deploy --wait
dbcanvas stack get pxc-lab
dbcanvas stack destroy pxc-lab
```

**Give every scripted stack a TTL**: `2h`, `4h`, `8h` (the CLI default), `24h`, `2w`
or `infinity`. When the TTL runs out, the stack's containers are removed and the stack
is marked `expired`.

### The design catalogue, and editing a design one piece at a time

`GET /api/stacks/design/catalog` describes everything the designer's panels offer:

| Key | Contents |
| --- | --- |
| `nodes`, `frames` | one entry per node type and cluster type: its `placement` (`standalone`, `member` of listed `frameTypes`, or `frame`), whether it is a `singleton`, what it `requires`, and its `options` (field, JSON `type`, accepted `values`, `default`, `description`, and `when` it applies). Clusters add `memberType`, `defaultMembers`, `minMembers`, `maxMembers`. |
| `relations` | references a node holds to another node — `monitor` (PMM), `backup` (SeaweedFS), `vault` (OpenBao), `oidc` (Keycloak), `ldap`, `kerberos`, `orchestrator`, `repository`, `storage` (Ceph), `upgrade` (Watchtower), `app` (All in One) — with the field each is stored in |
| `edges` (per entry) | the lines a type draws and what may be at the other end: a proxy's backend, a simulator's database, replication between clusters (`async` or `bidir`) |
| `actions` (per entry) | endpoints that apply once deployed, with request body and when each applies; `commonActions` apply to every node |

`?type=pxc` narrows it to the `pxc` entries.

The design endpoints change one thing at a time and check it against the catalogue.
An unknown field, a wrong type or value, a relation to the wrong node type, a label
already in use, or a cluster outside its member range is refused with a message
naming what is accepted. Each call returns what it changed plus the validator's
`issues` for the whole design (`"ok": false` on an error); issues do not block the
edit.

```json
POST /api/stacks/12/design/frames
{
  "type": "pxc", "label": "pxc-a", "members": 3,
  "options": {"pxcMajor": "8.4", "generateCert": true},
  "relations": {"monitor": "pmm-mvqf0a1b-1"}
}

POST /api/stacks/12/design/nodes
{"type": "proxysql", "options": {"mode": "singlewrite"}}

POST /api/stacks/12/design/edges
{"from": "frame-mvqf0c2d-1", "to": "proxysql-mvqf0e3f-1", "type": "directional"}

PUT /api/stacks/12/design/nodes/proxysql-mvqf0e3f-1
{"label": "proxy-1", "options": {"cpus": 2}, "relations": {"monitor": null}}
```

- A new node or cluster gets the designer's defaults and is placed to the right of
  the canvas unless `x`/`y` are given. A sharded `psmdb` cluster takes its shape from
  `options.psmdbSetup` (`standard` or `minimum`), not `members`.
- Add a member to a cluster with `POST …/design/nodes` and `frameId`; remove one with
  `DELETE …/design/nodes/{nid}`, down to the cluster's minimum.
- `PUT` changes only what the body names. An option set to `null` is removed; a
  relation set to `null` or `""` is cleared. On a cluster, `memberOptions` applies to
  every member.
- Removing a node or cluster also removes its lines and every relation pointing at it,
  and a deployed node's container.
- Nodes cannot be added or removed while a deploy is running; options can change.

### Composing a stack

`POST /api/stacks/compose` builds a whole stack from a short description: kinds,
counts, versions, OS and relationships. It fills in package builds, cluster ids and
node references for you.

```sh
dbcanvas stack compose lab-1 --ttl 8h \
  --node 'pxc:3,version=8.4.5,os=el8,monitor' \
  --node 'ps,version=8.0.45,os=el9,ldap,monitor' \
  --node 'pmm,version=3' \
  --deploy --wait
```

```json
POST /api/stacks/compose
{
  "name": "lab-1", "ttl": "8h",
  "nodes": [
    {"kind": "pxc", "count": 3, "version": "8.4.5", "os": "el8", "monitor": true},
    {"kind": "ps",  "version": "8.0.45", "os": "el9", "ldap": true, "monitor": true},
    {"kind": "pmm", "version": "3"}
  ]
}
```

Use `--spec file.json` (or `-` for stdin) to send a JSON spec from the CLI.

| You write | It becomes |
| --- | --- |
| `"os": "el8"` | Oracle Linux 8 |
| `"version": "8.4.5"` | series `8.4`, the matching package build for that OS |
| `"version": "8.4"` | the latest build in that series |
| `"version": ""` | the newest series, latest build |
| `"kind": "pxc", "count": 3` | a cluster with 3 members |
| `"monitor": true` | monitoring by the PMM node in the spec |
| *(no Intranet node)* | an Intranet node is added (every stack needs one); reported in `added` |

**Relationships.** Each is a boolean that points the node at another node in the
spec. When the spec has more than one candidate, name it with the matching `…With`
option (`monitorWith`, `ldapWith`, …).

| Option | Wires | Provided by | Available on |
| --- | --- | --- | --- |
| `monitor` | PMM monitoring | `pmm` | every engine, cluster and ProxySQL |
| `ldap` | directory authentication | `intranet`, `sambaad` | `ps`, `pg`, `psm`, `valkey`, `pxc`, `ps-repl`, `valkey-cluster` |
| `oidc` | Keycloak single sign-on | `keycloak` | `ps`, `pg`, `psm`, `pmm`, `pxc`, `ps-repl` |
| `kerberos` | GSSAPI single sign-on | `sambaad` | `pg`, `psm` |
| `vault` | data-at-rest keys in OpenBao | `openbao` | `ps`, `pg` (pg_tde: PostgreSQL 17/18), `psm`, `pxc`, `ps-repl`, `patroni` |
| `backup` | backups to S3 | `seaweedfs` | `ps`, `pg`, `pxc`, `ps-repl`, `patroni`, `repmgr`, `psmrs`, `psmdb` |
| `orchestrator` | topology discovery and failover | `orchestrator` | `ps-repl`, `mariadb-repl`, `mysql-repl` |

```sh
dbcanvas stack compose sso-lab \
  --node keycloak \
  --node 'ps,version=8.4.11,os=el9,oidc,monitor' \
  --node 'pmm,oidc' \
  --node openbao --node 'psm,os=el9,vault,ldap'
```

**Lines (`to=`).** Proxies and simulators find their database through a line on the
canvas. `"to"` names the target; omit it and compose uses the only valid one, or
refuses and lists the candidates.

| Kind | Connects to |
| --- | --- |
| `proxysql`, `proxysql-cluster` | `pxc`, `ps-repl` |
| `haproxy` | one of `patroni`, `repmgr`, `spock`, `pxc`, `ps-repl` |
| `pgbouncer` | one of `pg`, `patroni`, `repmgr`, `spock` |
| `supportsim` | `psm`, `psmrs`, `psmdb`, `k3d` |
| `pgvectorsim` | `pg`, `patroni`, `repmgr`, `spock`, `haproxy`, `pgbouncer`, `k3d` |

`mclusteradmin` and `bighole` take no line. MClusterAdmin gets a ready-made
connection string when the spec has exactly one MongoDB.

```json
{"kind": "haproxy", "os": "el9"}
{"kind": "pgvectorsim", "to": "haproxy-01"}
```

**Shaping the machine.** Per-node options; a kind that would ignore one refuses it.

| Field | Effect |
| --- | --- |
| `netLatencyMs`, `netJitterMs`, `netLossPct`, `netRateMbit` | delay, jitter, loss and a rate cap on the node's database and cluster ports |
| `netAllTraffic` | apply those to all of the node's traffic |
| `deviceReadMbps`, `deviceWriteMbps` | disk throughput limits |
| `cpus`, `memoryGb` | container limits |

**Per-engine choices**: `replMode` (`async`, `semisync`, `innodbcluster`,
`groupreplication`), `mode` (ProxySQL `singlewrite`/`loadbal`; PgBouncer pool mode
`transaction` by default, `session` or `statement`), `mysqlRouter`, `setup` (sharded
PSMDB: `standard` or `minimum`; `count` is refused),
`buckets` and `tls` (SeaweedFS), `alertEmail` (Orchestrator), `certTtl` (`365d`,
`2h`, `30m`). `dbcanvas stack kinds` lists exactly which options each kind takes.

```sh
dbcanvas stack compose pool-lab --ttl 4h \
  --node 'patroni:3,os=el9' \
  --node 'pgbouncer,os=el9,mode=transaction,cert' \
  --node 'pgvectorsim,to=pgbouncer-01'
```

**What compose adds or refuses for you**

- `oidc` turns on the Keycloak node's certificate, and a Keycloak node brings the
  Ubuntu desktop it needs (its admin console is reachable only from inside the stack).
- `oidc` on Percona Server is refused below 8.4.11-11 (9.7.2-2 on the 9.7 series).
- OpenBao runs only on Oracle Linux 9 and the VNC desktop only on Ubuntu 24.04.
- Only one each of `intranet`, `keycloak`, `openbao`, `vnc` and `watchtower` per stack.
- Not composable: Kubernetes frames, All in One, Stock Market Sim and Linux Client
  nodes. Add them with the design endpoints, or start from a template.

**Any other option** by its design-catalogue name: `"options"` sets fields on the node
(or a cluster's frame) and `"memberOptions"` on every member. They are checked like
the design endpoints and override the shorthand.

```json
{"kind": "patroni", "count": 3,
 "options": {"usePgBackRest": true, "pgAudit": true},
 "memberOptions": {"cpus": 2}}
```

**`"dryRun": true`** (`--dry-run`) reports the resolved plan — versions, added nodes
and the validator's `issues` — without creating anything. Errors name what is
available: the builds in a series, the OS aliases, the member range, or the node to add.

Compose does not deploy; `--deploy` in the CLI calls `POST /api/stacks/{id}/deploy`
afterwards.

## Templates

**UI:** **New stack → Start from**, and **Save as template** on the canvas.

| To do this | API | CLI |
| --- | --- | --- |
| List templates (built-in, yours, published) | `GET /api/templates` | `dbcanvas template list` |
| Save a canvas as one | `POST /api/templates` | `dbcanvas api POST /api/templates --data @tpl.json` |
| Read one, with its design | `GET /api/templates/{id}` | `dbcanvas api GET /api/templates/builtin:pxc-proxysql-pmm` |
| Rename or re-describe it | `PUT /api/templates/{id}` | `dbcanvas api PUT /api/templates/3 --data '{"name":"…"}'` |
| Delete one of yours | `DELETE /api/templates/{id}` | `dbcanvas api DELETE /api/templates/3` |
| Publish or withdraw instance-wide (admin) | `POST /api/templates/{id}/share` | `dbcanvas api POST /api/templates/3/share` |
| Download as `.json` | `GET /api/templates/{id}/export` | `dbcanvas template export <name> > pxc.json` |
| Import one | `POST /api/templates/import` | `dbcanvas template import pxc.json` |

Built-in templates have ids like `builtin:pxc-proxysql-pmm`; saved ones are numbers.
Saved templates are stripped of secrets, host paths and pinned host ports.

## Nodes

**UI:** click a node for its panel; right-click for its menu.

![The right-click menu on a running node](screenshots/getting-started-node-menu.png)

| To do this | API | CLI |
| --- | --- | --- |
| A node's panel: type, address, published ports, credentials, certificate | `GET …/nodes/{nid}` | `dbcanvas api GET /api/stacks/1/nodes/<nid>` |
| List a stack's nodes | `GET /api/stacks/{id}` | `dbcanvas node list <stack>` |
| Start · stop · restart | `POST …/nodes/{nid}/start` · `/stop` · `/restart` | `dbcanvas node start\|stop\|restart <stack> <node>` |
| Open a root console | `GET …/nodes/{nid}/term` *(WebSocket)* | `dbcanvas node console <stack> <node> [--user U]` |
| Run one command | *(the same WebSocket)* | `dbcanvas node exec <stack> <node> -- mysql -e 'SHOW STATUS'` |
| List the pods on a Kubernetes server node | `GET …/nodes/{nid}/k8s/pods` | `dbcanvas node pods <stack> <node>` |
| Console inside a pod's container | `GET …/nodes/{nid}/term?namespace=&pod=&container=&shell=` *(WebSocket)* | `dbcanvas node console <stack> <node> --pod P --container C [--namespace N] [--shell S]` |
| The `ssh -L` tunnel line | `GET …/nodes/{nid}/sshforward` | `dbcanvas node tunnel <stack> <node>` |
| Copy files onto a node (drag and drop) | `POST …/nodes/{nid}/upload` *(multipart)* | `dbcanvas node cp ./my.cnf <stack>:<node>:/etc/` |
| Download a MongoDB node's `diagnostic.data` and log | `GET …/nodes/{nid}/mongo/diagnostic` | `dbcanvas api GET …/mongo/diagnostic --out diag.tar.gz` |
| Who is primary, and who could take over | `GET …/nodes/{nid}/switchover` | `dbcanvas api GET …/switchover` |
| Make this member the primary | `POST …/nodes/{nid}/promote` | `dbcanvas api POST …/promote` |

![A deployed node's panel](screenshots/getting-started-node-panel.png)
![Credentials on a node's panel](screenshots/getting-started-credentials.png)

Passwords are generated per deployment and returned by `GET …/nodes/{nid}`, so a
script can connect without anyone pasting one.

![A root console in the browser](screenshots/terminal.png)

On the console WebSocket, binary frames are keystrokes and text frames are
`{"type":"resize"}` messages.

> [!NOTE]
> A node gets a new host port each time it starts. Re-read the node after a restart.

### Switching the primary

**UI:** right-click a member of a replicated cluster → **Replication role**. A replica
offers **Promote to primary**; the primary offers **Demote — hand primary to** with each
replica's lag. Members that cannot take over are greyed, with the reason.

| Cluster | How the switch is made |
| --- | --- |
| Patroni | `patronictl switchover --candidate` |
| repmgr | `repmgr standby switchover --siblings-follow` |
| MongoDB replica set (and each shard or config set) | other secondaries frozen, primary steps down, candidate elected |
| Group Replication / InnoDB Cluster | `group_replication_set_as_primary()` |
| MySQL and MariaDB replication (GTID or not) | writes frozen on the primary, every replica caught up, then the candidate made writable and the others re-pointed |

For MySQL and MariaDB, if a replica does not catch up within a minute, writes are
restored and nothing changes. A switch is refused while any replica is not
replicating. Semi-sync moves with the primary.

HAProxy, ProxySQL, PgBouncer and MySQL Router follow the new primary. The design is
updated, so a redeploy keeps the new roles. The response lists every step, and which
step failed if one did. PXC, Galera and Spock have no primary to move.

### Rebuilding a replica, rolling restarts, comparing configuration

**UI:** right-click a cluster member.

- **Rebuild from primary…** (**Rebuild (full state transfer)…** on PXC and Galera)
  discards the member's data, copies it afresh and puts it back into replication:

  | Cluster | How |
  | --- | --- |
  | MySQL / Percona Server 8.0.17+ replication | CLONE plugin |
  | MariaDB, older MySQL | a consistent `mariadb-dump` / `mysqldump` of the primary |
  | Group Replication / InnoDB Cluster | rejoin with clone recovery |
  | Patroni | `patronictl reinit` |
  | repmgr | `repmgr standby clone`, then `standby register` |
  | MongoDB replica set | full initial sync |
  | PXC, MariaDB Galera | full SST |

  `GET …/nodes/{nid}/rebuild` shows the plan (or why not) and the running or last
  rebuild; `POST` starts it.
- **Rolling restart of the cluster…** restarts members one at a time, replicas first,
  each caught up before the next, optionally handing over the primary first.
  `GET`/`POST …/nodes/{nid}/rolling-restart` `{"switchover": true}`.
- **Compare configuration…** shows the settings that differ across members:
  `GET …/nodes/{nid}/config-drift`.

The **Live details** tab of a database node shows its error log
(`GET …/nodes/{nid}/errorlog?lines=200`, up to 1000) and links to its PMM dashboards
when monitored.

### Database users, failure drills, backups

**UI:** right-click a cluster member.

- **Database users…** lists the cluster's accounts, which members have each, and
  whether passwords match. Create (read, read and write, or admin; one database or
  all), rotate or drop an account on the primary. `GET`/`POST …/nodes/{nid}/db-users`
  `{action, name, host, password, privilege, database}`.
- **Failure drill…** breaks the cluster for real and checks off what happened: *kill
  the primary* (Patroni, MongoDB, Group Replication, any Galera member) or *stop
  replication* on a MySQL/MariaDB replica. `GET`/`POST …/nodes/{nid}/drill`,
  `DELETE /api/stacks/{id}/drill`.

A cluster deployed with backups shows the age of its newest backup on its header (red
for none, amber past a day) and **Back up now**: `GET …/frames/{fid}/backup-status`.

### Before a redeploy

Deploy works per member:

- a cluster with no built members is built whole;
- a cluster whose members are all built (running, or stopped on purpose) is left alone;
- a cluster with built and new members builds **only the new members**, which join the
  running cluster.

| Cluster | How a new member joins |
| --- | --- |
| Percona Server / MySQL replication | CLONE from the primary (a consistent dump where CLONE is unavailable), then replicates read-only |
| MariaDB replication | a consistent `mariadb-dump` of the primary |
| PXC, MariaDB Galera | full state transfer |
| InnoDB Cluster / Group Replication | `addInstance` with clone recovery |
| Patroni | joins etcd; Patroni takes a base backup from the leader |
| repmgr | `repmgr standby clone` and `register` |
| MongoDB replica set | added with `replSetReconfig`, initial sync |

Sharded MongoDB, Spock, Valkey cluster and Kubernetes frames cannot take a new member
yet; validation and deploy refuse it.

`GET /api/stacks/{id}/deploy/preview` lists what a deploy would do: nodes deleted
from the canvas (removed with their volumes), nodes to build, members to join, and
refused clusters with the reason. Settings changed on running nodes are not applied
by a deploy.

### Health, history and alerts

DBCanvas samples every deployed stack every 30 seconds by default (**Settings → Stack
history and alerts**). The canvas's **Health** button shows:

- **Alerts** open now: a database down, replication broken, a replica behind, a
  filesystem filling, connections running out, a transaction open too long, a long
  lock wait, a deadlock. Each opens after two bad samples (database down: one),
  closes after two good ones, and reaches the notification bell. A node stopped or
  restarted by DBCanvas raises nothing.
- **Timeline**: role changes, unplanned failovers, switchovers, rebuilds, restarts and
  lifetime changes, with who asked.
- **Trends**: a day of CPU, memory, lag, QPS, TPS, connections, IOPS and filesystem
  use per node.
- **Rules**: the stack's thresholds.

| To do this | API |
| --- | --- |
| Samples, timeline and open alerts | `GET /api/stacks/{id}/history?minutes=60` (up to a day) |
| Open alerts and rules | `GET /api/stacks/{id}/alerts` |
| Change the rules | `PUT /api/stacks/{id}/alert-rules` |
| Open alerts across all your stacks | `GET /api/alerts/summary` |

Default rules: replica lag 30 s, filesystem 85 %, connections 90 %, transaction open
10 minutes (`trxSec`), lock wait 30 s (`lockWaitSec`); `0` switches a threshold off.

### A console inside a pod

On a **k3s server node**, the `…/term` WebSocket also takes `namespace`, `pod`,
`container` and `shell` (`auto`, `bash`, `sh`, `mysql`, `psql` or `mongosh`) and opens
the console inside that container. Nothing is configured on your machine and no port
is published. Invalid names get an HTTP error before the socket opens.

`GET …/nodes/{nid}/k8s/pods` lists every pod with its containers, their state
(`running`, `waiting`, `terminated`), which are init containers, and which database
clients each can start (`clients`). Read it fresh each time: the operator recreates
pods with new names.

```sh
dbcanvas node pods k8s-lab k3s-01
dbcanvas node console k8s-lab k3s-01 --namespace default --pod cluster1-pxc-0 --container pxc --shell mysql
```

With `shell=mysql`, `psql` or `mongosh` the console opens that client already logged
in as an administrator. The password is read inside the container from what the
operator mounted, and never passes through DBCanvas:

| Client | Credential used | Offered on containers |
| --- | --- | --- |
| `mysql` | `/etc/mysql/mysql-users-secret/root`, else `$MYSQL_ROOT_PASSWORD` | `pxc`, `mysql`, `haproxy`, `pxc-monit`, `proxysql` |
| `psql` | peer authentication as `postgres` | `database`, `postgres` |
| `mongosh` | `/etc/users-secret/MONGODB_DATABASE_ADMIN_{USER,PASSWORD}` | `mongod`, `mongos` |

If the client, credential or server is missing, the console says what was missing and
opens a shell instead.

## Clusters & backups

These act on a cluster (frame) or on one database node.

| To do this | API | CLI |
| --- | --- | --- |
| Add or remove PMM monitoring on every member | `POST …/frames/{fid}/pmm` | `dbcanvas api POST /api/stacks/1/frames/<fid>/pmm` |
| Add or remove Orchestrator on a MySQL replication cluster | `POST …/frames/{fid}/orchestrator` | `dbcanvas api POST …/orchestrator` |
| When the cluster was last backed up | `GET …/frames/{fid}/backup-status` | `dbcanvas api GET …/backup-status` |
| **Patroni (pgBackRest)**: back up · list backups · restore (latest, a time, or a set) | `POST …/frames/{fid}/patroni/backup` · `GET …/patroni/backups` · `POST …/patroni/restore` | `dbcanvas api POST …/patroni/backup` |
| **Standalone PostgreSQL (pgBackRest)**: back up · list · restore | `POST …/nodes/{nid}/pg/backup` · `GET …/pg/backups` · `POST …/pg/restore` | `dbcanvas api POST …/pg/backup` |
| **MongoDB (PBM)**: back up a replica set or sharded cluster | `POST …/frames/{fid}/pbm/backup` | `dbcanvas api POST …/pbm/backup` |
| **repmgr**: back up from the primary (pgBackRest or Barman) | `POST …/frames/{fid}/barman/backup` | `dbcanvas api POST …/barman/backup` |
| **XtraBackup, PS replication or PXC cluster**: back up · list · restore · delete | `POST …/frames/{fid}/xb/backup` · `GET …/xb/backups` · `POST …/xb/restore` · `POST …/xb/delete` | `dbcanvas api POST …/xb/backup --data '{"type":"full"}'` |
| **XtraBackup, standalone Percona Server**: the same four | `…/nodes/{nid}/xb/backup` · `/backups` · `/restore` · `/delete` | `dbcanvas api POST …/xb/backup --data '{"type":"incremental"}'` |
| Run pgbench on a PostgreSQL node · a Patroni leader | `POST …/nodes/{nid}/pg/pgbench` · `POST …/frames/{fid}/patroni/pgbench` | `dbcanvas api POST …/pg/pgbench` |
| Rotate the pg_tde principal key | `POST …/nodes/{nid}/pg/tde/rotate` · `POST …/frames/{fid}/patroni/tde/rotate` | `dbcanvas api POST …/pg/tde/rotate` |

XtraBackup backups go to SeaweedFS. `xb/restore` takes `{"mode": "backup", "name"}`,
`{"mode": "latest"}` or `{"mode": "time", "time"}`; `xb/delete` `{"name"}` also deletes
the incrementals built on it.

![A PMM node's panel](screenshots/pmm-node.png)
![PMM's own web UI, reached from the panel](screenshots/pmm-web.png)

Ticking **Monitored by** on a deployed cluster calls `…/frames/{fid}/pmm`: it creates
the monitoring user on every member and registers each one.

## Version catalogues

What this installation can install. The designer's version pickers use these; a
script that builds a design should read them rather than guess a version.

| Catalogue | API |
| --- | --- |
| Percona XtraDB Cluster | `GET /api/catalog/pxc` |
| Percona Server for MySQL | `GET /api/catalog/ps` |
| Percona Distribution for MySQL | `GET /api/catalog/pdps` |
| MySQL Community | `GET /api/catalog/mysqlce` |
| MariaDB | `GET /api/catalog/mariadb` |
| Percona Server for MongoDB | `GET /api/catalog/psmdb` |
| Percona Distribution for PostgreSQL | `GET /api/catalog/ppg` |
| Spock (pgEdge) | `GET /api/catalog/spock` |
| Valkey | `GET /api/catalog/valkey` |
| ProxySQL · Orchestrator | `GET /api/catalog/proxysql` · `/api/catalog/orchestrator` |
| PMM 3 · PMM 2 | `GET /api/catalog/pmm` · `/api/catalog/pmm2` |
| k3s · Kubernetes operators | `GET /api/catalog/k3s` · `/api/catalog/operators` |
| What a Repository node can mirror | `GET /api/catalog/repository` |
| The OS base images built on this host | `GET /api/catalog/images` |

Engine catalogues list versions per OS.

```sh
dbcanvas api GET /api/catalog/ps
```

## Kubernetes frames

**UI:** the k3s node's panel on a K3D frame. These endpoints are addressed by frame;
the pod list and pod console are under [Nodes](#nodes).

![A K3D cluster frame on the canvas](screenshots/k3d-cluster.png)

| To do this | API | CLI |
| --- | --- | --- |
| An admin kubeconfig | `GET …/frames/{fid}/k3d/kubeconfig` | `dbcanvas api GET …/k3d/kubeconfig` |
| Clusters you can watch live | `GET /api/k3d/states/targets` | `dbcanvas api GET /api/k3d/states/targets` |
| Every object's state at once | `GET …/frames/{fid}/k3d/states` | `dbcanvas api GET …/k3d/states` |
| One container's log (or the previous run's) | `GET …/k3d/states/logs?namespace=&name=&container=&previous=1` | `dbcanvas api GET '…/k3d/states/logs?namespace=pxc&name=cluster1-pxc-0&container=pxc'` |
| One object as YAML | `GET …/k3d/states/manifest?kind=&namespace=&name=` | `dbcanvas api GET '…/k3d/states/manifest?kind=Pod&namespace=pxc&name=cluster1-pxc-0'` |
| States board from a kept cluster-dump | `POST /api/k8sstates/dumps/{did}` | `dbcanvas api POST /api/k8sstates/dumps/2` |
| States board from an uploaded cluster-dump | `POST /api/k8sstates/upload` *(multipart)* | — |
| From an archive: an object's YAML · a pod's files · any file | `GET /api/k8sstates/archive/manifest?dump=&kind=&namespace=&name=` · `/archive/logs?dump=&namespace=&name=&file=` · `/archive/files?dump=&path=` | `dbcanvas api GET '/api/k8sstates/archive/files?dump=2'` |
| RBAC users: list · create · delete | `GET`/`POST …/frames/{fid}/k3d/users` · `POST …/k3d/users/delete` | `dbcanvas api POST …/k3d/users --data '{"username":"dev","role":"view"}'` |
| A kubeconfig for one user | `GET …/k3d/users/{username}/kubeconfig` | `dbcanvas api GET …/k3d/users/dev/kubeconfig` |
| TLS Secrets, parsed and checked | `GET …/frames/{fid}/k3d/certs` | `dbcanvas api GET …/k3d/certs` |
| Cross-cluster replication: read · re-seed the replica | `GET …/frames/{fid}/k3d/replication` · `POST …/k3d/replication/reseed` | `dbcanvas api GET …/k3d/replication` |
| The custom resource: read as a form model · patch | `GET`/`POST …/frames/{fid}/k3d/cr` | `dbcanvas api POST …/k3d/cr --data '{"patch":{"pxc":{"size":5}}}'` |
| Secrets and ConfigMaps: list · read one · write keys | `GET …/k3d/objects?kind=secret` · `GET …/k3d/object?kind=&namespace=&name=` · `POST …/k3d/object` | `dbcanvas api POST …/k3d/object --data '{"kind":"secret","name":"cluster1-secrets","set":{"root":"new"}}'` |
| Volumes: read · grow | `GET`/`POST …/frames/{fid}/k3d/storage` `{"sizeGiB"}` | `dbcanvas api POST …/k3d/storage --data '{"sizeGiB":20}'` |
| PostgreSQL logical replicas: list · add · remove · reseed · bootstrap log | `GET`/`POST …/k3d/logicalreplicas` · `POST …/logicalreplicas/remove` · `/reseed` · `GET …/logicalreplicas/bootstraplog` | `dbcanvas api GET …/k3d/logicalreplicas` |
| PSMDB ClusterSync: status · start · stop · pause/resume/finalize | `GET …/k3d/clustersync` · `POST …/clustersync/start` `{"source": "<frame id>"}` · `/stop` · `/{action}` | `dbcanvas api GET …/k3d/clustersync` |
| Backups and restores: list · take · delete · restore · delete a restore | `GET`/`POST …/k3d/backups` · `POST …/k3d/backups/delete` · `POST …/k3d/restores` · `POST …/k3d/restores/delete` | `dbcanvas api POST …/k3d/backups --data '{}'` |
| Manifests the Backups tab applied | `GET …/k3d/backups/manifests[?name=]` | `dbcanvas api GET '…/k3d/backups/manifests?name=b1.yaml'` |
| Backup bucket: list · delete · download (≤ 64 MiB) | `GET …/k3d/bucket?prefix=` · `POST …/k3d/bucket/delete` · `GET …/k3d/bucket/download?key=` | `dbcanvas api GET '…/k3d/bucket?prefix=cluster1-full'` |
| The bucket toolbox pod: status · start/stop | `GET`/`POST …/k3d/bucket/toolbox` | `dbcanvas api POST …/k3d/bucket/toolbox --data '{"action":"stop"}'` |

```sh
dbcanvas api GET /api/stacks/1/frames/k3d-01/k3d/kubeconfig | jq -r .kubeconfig > kube.yaml
KUBECONFIG=kube.yaml kubectl get pods -A
```

**Replication** between two PXC-operator frames is set up by drawing a line and
deploying ([Stacks](STACKS.md)). **Re-seed replaces the replica's data** with a fresh
backup of the source.

**Custom resource editor (`k3d/cr`).** `GET` returns the operator's CRD as a form model
(fields, types, enums, bounds) with the live `spec`. `POST` takes a JSON merge patch of
`spec` (`null` deletes a key), always dry-runs it on the API server first, and applies
nothing if it is rejected. Add `"dryRun": true` to validate only.

```sh
dbcanvas api POST /api/stacks/1/frames/k3d-00/k3d/cr \
  --data '{"patch":{"pxc":{"size":4}},"dryRun":true}'
```

**Secrets and ConfigMaps editor (`k3d/object`).** A write is a merge patch of `data`
(`set` and `remove`), dry-run first, `dryRun` to stop there. Secret values are plain
text in this API; DBCanvas handles the base64. The list returns key names and sizes
only. A non-text value is returned as `"binary": true` without its value. Errors
return the API server's message, never the value being written.

**Backups tab** — see [Operator Backups](OPERATOR_BACKUPS.md). The same calls work for
every Percona operator: the API finds the cluster and its storage from the frame.
Every write returns the manifest it applied and keeps a copy on the node; `"dryRun":
true` returns the manifest without applying it.

> [!WARNING]
> `restores` stops the cluster and replaces its data; there is no undo.
> `backups/delete` with `"data": true` also deletes the backup files from storage
> (refused for the PostgreSQL operator, which does not support it); without it only
> the record is deleted.

```sh
dbcanvas api POST /api/stacks/1/frames/k3d-01/k3d/backups --data '{}'
dbcanvas api GET  /api/stacks/1/frames/k3d-01/k3d/backups | jq '.backups[] | {name, state, destination}'
```

**Bucket** calls run `aws` in a toolbox pod on the cluster (`<cluster>-dbcanvas-s3`,
started on demand) with the cluster's own backup credentials, and each answer includes
the command it ran. Listing stops at the next `/` and pages with `after`. A larger
download is refused with the command to copy it inside the cluster instead.

## All in One

Many database instances inside one node.

| To do this | API | CLI |
| --- | --- | --- |
| List instances, with engine, version, port and state | `GET …/nodes/{nid}/aio/instances` | `dbcanvas api GET …/aio/instances` |
| Start · stop · restart one | `POST …/aio/instances/{inst}/start` · `/stop` · `/restart` | `dbcanvas api POST …/aio/instances/mysql-8.0/restart` |
| Tail one instance's log | `GET …/aio/instances/{inst}/logs` | `dbcanvas api GET …/aio/instances/mysql-8.0/logs` |

## Repository, Ceph and Linux Client nodes

| To do this | API | CLI |
| --- | --- | --- |
| Add packages, images or operator releases to a Repository node | `POST …/nodes/{nid}/repository/add` | `dbcanvas api POST …/repository/add --data '{…}'` |
| Re-sync a Repository node | `POST …/nodes/{nid}/repository/sync` | `dbcanvas api POST …/repository/sync` |
| A Ceph node's health, capacity and RBD images | `GET …/nodes/{nid}/ceph` | `dbcanvas api GET …/ceph` |
| Set a Linux Client's current kubeconfig context | `PUT …/nodes/{nid}/kubeconfig/context` | `dbcanvas api PUT …/kubeconfig/context --data '{…}'` |
| Linux Client ClusterSync (MongoDB to MongoDB): status · start · stop · document counts | `GET …/nodes/{nid}/pcsm` · `POST …/pcsm/start` `{source, target, include, exclude, fresh}` · `POST …/pcsm/stop` · `GET …/pcsm/compare` | `dbcanvas api GET …/pcsm` |
| Pause, resume, resume-failed or finalize it | `POST …/nodes/{nid}/pcsm/{action}` | `dbcanvas api POST …/pcsm/pause` |

Repository `add` and `sync` return `202`; progress shows on the node.

# Running load

## Data Generator

Fill tables with realistic data. See [Data Generator](DATA_GENERATOR.md).

![The Data Generator](screenshots/data-generator.png)

| To do this | API | CLI |
| --- | --- | --- |
| Nodes it can write to | `GET /api/datagen/connections` | `dbcanvas datagen connections` |
| Databases on a node | `GET /api/datagen/stacks/{id}/nodes/{nid}/databases` | `dbcanvas datagen databases <stack> <node>` |
| Tables, with row counts | `…/tables?database=` | `dbcanvas datagen tables <stack> <node> --database sbtest` |
| A table's columns and suggested generators | `…/columns?database=&table=` | `dbcanvas api GET '…/columns?database=sbtest&table=sbtest1'` |
| Preview rows without writing | `POST …/preview` | `dbcanvas api POST …/preview --data '{…}'` |
| Start a job | `POST …/generate` | `dbcanvas datagen run <stack> <node> --database D --table T --rows N [--wait] [--timeout 2h]` |
| Poll · cancel a job | `GET /api/datagen/jobs/{job}` · `POST …/cancel` | `--wait` polls · `dbcanvas api POST /api/datagen/jobs/<job>/cancel` |

```sh
dbcanvas datagen run pxc-lab pxc-01 --database sbtest --table sbtest1 --rows 5000000 --wait
```

Rows already written stay written when a job is cancelled.

## Query Runner

Run SQL on several nodes in parallel. See [Query Runner](QUERY_RUNNER.md).

![The Query Runner](screenshots/query-runner.png)

| To do this | API | CLI |
| --- | --- | --- |
| Nodes it can run against | `GET /api/queryrun/targets` | `dbcanvas query targets` |
| Start a run | `POST /api/queryrun/runs` | `dbcanvas query run --stack S --nodes a,b --sql '…'` |
| Poll it | `GET /api/queryrun/runs/{id}` | *(the CLI waits by default)* |
| Stop it, killing the statements | `POST /api/queryrun/runs/{id}/stop` | `dbcanvas api POST /api/queryrun/runs/<id>/stop` |
| Your previous runs | `GET /api/queryrun/history` | `dbcanvas query history` |

`query run` flags: `--database`, `--count` (per node; `0` repeats until
`--time-limit`, default 60 s), `--threads`, `--wait` (default true), `--timeout`.
`--sql @file.sql` reads the statement from a file.

```sh
dbcanvas --json query run --stack pxc-lab --nodes pxc-01,pxc-02,pxc-03 \
  --sql 'SELECT @@hostname, @@server_id' | jq '.nodes[]'
```

## Database Explorer

Browse and query the databases in your stacks — MySQL, PostgreSQL, MongoDB, Valkey and
ClickHouse, including PMM Server's own PostgreSQL and Query Analytics store. See
[Database Explorer](DATABASE_EXPLORER.md).

A connection is an opaque id from the first call. No endpoint takes or returns a host,
user or password.

| To do this | API | CLI |
| --- | --- | --- |
| Every database you can reach, by stack and engine | `GET /api/dbexplorer/connections` | `dbcanvas api GET /api/dbexplorer/connections` |
| One connection, with its server version | `GET /api/dbexplorer/connections/{cid}` | *(UI)* |
| Its databases (or Valkey keyspaces) | `GET …/connections/{cid}/databases` | *(UI)* |
| Its schemas | `GET …/connections/{cid}/schemas?database=` | *(UI)* |
| One level of the object tree | `GET …/connections/{cid}/objects?database=&schema=&folder=&cursor=&filter=` | *(UI)* |
| Describe one object: columns, indexes, keys, size, DDL | `GET …/connections/{cid}/object?database=&schema=&name=&kind=` | *(UI)* |
| Its first page of rows, documents or values | `GET …/connections/{cid}/viewdata?database=&name=&kind=&limit=` | *(UI)* |
| Run SQL, a MongoDB query or a Valkey command | `POST /api/dbexplorer/query` | `dbcanvas api POST /api/dbexplorer/query --data '{…}'` |
| Cancel your running query | `POST /api/dbexplorer/cancel` | *(UI)* |
| Insert, edit or delete one row, or preview the statement | `POST /api/dbexplorer/mutate` | *(UI)* |
| Query history · delete one · clear | `GET /api/dbexplorer/history` · `DELETE …/history/{hid}` · `DELETE …/history` | `dbcanvas api GET /api/dbexplorer/history` |
| Saved queries | `GET`/`POST /api/dbexplorer/saved` · `DELETE …/saved/{sid}` | *(UI)* |
| Give a Kubernetes database an address the load tools can reach · remove it | `POST /api/dbexplorer/expose` · `POST /api/dbexplorer/unexpose` | *(UI)* |

```sh
CID=$(dbcanvas api GET /api/dbexplorer/connections |
      jq -r '.stacks[0].groups[0].connections[0].id')
dbcanvas api POST /api/dbexplorer/query --data "$(jq -n --arg c "$CID" \
  '{connectionId:$c, database:"shop", sql:"SELECT country, count(*) FROM orders GROUP BY 1", limit:500}')"
```

- Results are capped at 500 rows by default, 50,000 at most; queries time out after
  30 s by default, 300 s at most.
- Valkey's `cursor` is a `SCAN` cursor you pass back to continue.
- Databases inside a K3D frame are included. `expose` adds a Service beside the
  operator's own and never changes what the operator owns.
- PMM's databases are read-only unless an administrator turns on **Writes to internal
  databases**; the database itself also refuses writes.

## Benchmark

OLTP, OLAP, read-write and read-only workloads. See [Benchmark](BENCHMARK.md).

![Benchmark results](screenshots/benchmark.png)

| To do this | API | CLI |
| --- | --- | --- |
| Nodes, and the workloads each supports | `GET /api/benchmark/targets` | `dbcanvas benchmark targets` |
| Start a run | `POST /api/benchmark/runs` | `dbcanvas benchmark run <stack> <node> --workload oltp --duration 60 --wait` |
| Live throughput and latency | `GET /api/benchmark/runs/{id}` | *(`--wait` polls it)* |
| Stop early, keeping the results | `POST /api/benchmark/runs/{id}/stop` | `dbcanvas api POST /api/benchmark/runs/<id>/stop` |
| Previous runs | `GET /api/benchmark/history` | `dbcanvas benchmark history` |

`benchmark run` flags: `--workload` (`oltp`, `olap`, `rw`, `ro`), `--duration` (s),
`--threads`, `--database` (default `dbcanvas_bench`), `--create-db` (default true),
`--scale`, `--warmup` (s), `--wait`.

## Stock Market Sim

![Three Stock Market Sim nodes driving three Kubernetes operators](screenshots/stocksim-operators.png)

The sim is configured by deploying it and linking it on the canvas. One endpoint
tests a hand-entered connection:

| To do this | API | CLI |
| --- | --- | --- |
| Test a database connection from inside the stack network | `POST …/nodes/{nid}/stocksim/test` | `dbcanvas api POST …/stocksim/test --data '{…}'` |

## Sample Client Code

Generate a client program for a deployment, install what it needs on a Linux Client,
and run it. See [Sample Client Code](SAMPLE_CODE.md).

| To do this | API | CLI |
| --- | --- | --- |
| Every sample, with dependencies and licences | `GET /api/samplecode/catalog` | `dbcanvas api GET /api/samplecode/catalog` |
| Linux Clients that can run one | `GET /api/samplecode/nodes` | `dbcanvas api GET /api/samplecode/nodes` |
| The endpoints in that node's stack | `GET …/nodes/{nid}/samplecode/targets` | `dbcanvas api GET …/samplecode/targets` |
| Render a sample (changes nothing) | `POST …/nodes/{nid}/samplecode/generate` | `dbcanvas api POST …/samplecode/generate --data '{…}'` |
| Save · prepare · run · reset | `POST …/nodes/{nid}/samplecode/runs` | `dbcanvas api POST …/samplecode/runs --data '{…}'` |
| Follow · stop the job | `GET …/samplecode/runs/{jid}` · `POST …/runs/{jid}/stop` | `dbcanvas api GET …/samplecode/runs/<jid>` |

```json
{ "sample": "mysql/java/hikari/crud", "target": "ps1", "tls": "verify", "action": "run" }
```

`sample` is database/language/client/scenario. `target` is an id from
`…/samplecode/targets`. `tls` is `off`, `require` or `verify` (omit it to use the
deployment's setting). `action` (`save`, `prepare`, `run`, `reset`) is for `runs`
only. The job log shows every command and its output; a non-zero exit from the
program is reported as `ran: true` with the exit code.

## Ledger Sim

The JDBC simulator, configured by deploying and linking it. Its JDBC URL, driver,
HikariCP settings and workload are changed on its own dashboard while it runs.

| To do this | API | CLI |
| --- | --- | --- |
| Test a JDBC connection with the chosen driver, from inside the stack network | `POST …/nodes/{nid}/ledgersim/test` | `dbcanvas api POST …/ledgersim/test --data '{…}'` |

The answer includes the driver's version string and any `SQLState`.

---

# Finding out what happened

## Activity

**UI:** right-click a running database or proxy node → **Activity…** (or *see
Activity* on a transaction or lock-wait alert). It refreshes every 2 seconds by
default while open.

| Tab | Shows |
| --- | --- |
| Overview | what to look at first: the oldest blocker, the longest transaction, DDL stuck in a queue |
| Blocking | who blocks whom: row locks, metadata locks, PostgreSQL blockers, a pool with no free connection |
| Transactions | open transactions by age, rows locked and modified; what an idle one last ran |
| Sessions | every session; optionally CPU, I/O and memory per session |
| DDL | DDL in flight with phase, progress and estimate, and what is queued behind it |
| Deadlocks | each recorded deadlock: the transactions, what each held and waited for, the one rolled back |
| Statements | normalised statements by time per second |
| Pools | ProxySQL's per-backend pool; PgBouncer's per database and user |

Supported: MySQL, MariaDB, PostgreSQL, MongoDB, ProxySQL and PgBouncer (tabs vary by
engine). **Kill** ends a statement or connection; **EXPLAIN** shows a running
statement's plan without re-running it. **Deep** (MySQL family) turns on extra
performance_schema instrumentation for 1–60 minutes and turns back off only what it
turned on.

```sh
N=/api/stacks/12/nodes/$NODE_ID      # a node id: see `dbcanvas node list`
dbcanvas api GET "$N/activity?with=digests,resources,deadlock"
dbcanvas api POST "$N/activity/kill" --data '{"session": "4711", "mode": "connection"}'
dbcanvas api POST "$N/activity/explain" --data '{"session": "4711"}'
dbcanvas api POST "$N/activity/deep" --data '{"minutes": 10}'
dbcanvas api GET "$N/deadlocks"
```

## Packet Inspector

`tcpdump` on a database node, decoded packet by packet — MySQL, PostgreSQL, MongoDB
and Valkey. See [Packet Inspector](PACKET_INSPECTOR.md).

![The Packet Inspector](screenshots/packet-inspector.png)

| To do this | API | CLI |
| --- | --- | --- |
| Nodes that can be captured, and on which port | `GET /api/pktinspect/targets` | `dbcanvas capture targets` |
| Start a capture | `POST /api/pktinspect/captures` | `dbcanvas capture start <stack> <node> [--seconds 30] [--label L]` |
| List your captures | `GET /api/pktinspect/captures` | `dbcanvas capture list` |
| A capture's state and counters | `GET /api/pktinspect/captures/{id}` | `dbcanvas capture get <id>` |
| Stop and decode | `POST …/captures/{id}/stop` | `dbcanvas capture stop <id>` |
| A page of decoded packets | `GET …/captures/{id}/packets` | `dbcanvas capture packets <id> --limit 50` |
| One packet in full | `GET …/captures/{id}/packets/{no}` | `dbcanvas api GET /api/pktinspect/captures/<id>/packets/42` |
| The timeline | `GET …/captures/{id}/timeline` | `dbcanvas api GET …/timeline` |
| Download the `.pcap` | `GET …/captures/{id}/download` | `dbcanvas capture download <id> [--out F]` |
| Server-log records from the same window | `GET …/captures/{id}/serverlog` | `dbcanvas api GET …/serverlog` |
| Upload a `.pcap` | `POST /api/pktinspect/upload` *(multipart, field `file`)* | `curl -F file=@x.pcap …` |

![One decoded packet in detail](screenshots/packet-inspector-detail.png)

```sh
ID=$(dbcanvas capture start pxc-lab pxc-01 --seconds 20 | jq -r .id)
sleep 25 && dbcanvas capture download "$ID"
```

## Log Summary

Several nodes' logs on one timeline, classified by severity. See
[Log Summary](LOG_SUMMARY.md).

![The Log Summary](screenshots/log-summary.png)

| To do this | API | CLI |
| --- | --- | --- |
| What can be collected | `GET /api/logsummary/targets` | `dbcanvas logs targets` |
| Collect several nodes' logs as one bundle | `POST /api/logsummary/collect` | `dbcanvas logs collect <stack> [--nodes a,b,c]` (every node with logs if omitted) |
| List your bundles | `GET /api/logsummary/bundles` | `dbcanvas logs list` |
| A bundle's sources, verdict and counts | `GET /api/logsummary/bundles/{id}` | `dbcanvas logs get <id>` |
| A page of events | `GET …/bundles/{id}/events` | `dbcanvas logs events <id> [--severity bad,warn] [--limit N]` |
| The per-source timeline | `GET …/bundles/{id}/timeline` | `dbcanvas api GET …/timeline` |
| What every node was doing at one instant | `GET …/bundles/{id}/at?t=` | `dbcanvas api GET '…/at?t=…'` |
| Download one source's raw log | `GET …/bundles/{id}/sources/{src}/raw` | `dbcanvas api GET …/raw --out node.log` |
| Correct a source's clock offset | `POST …/bundles/{id}/offset` | `dbcanvas api POST …/offset --data '{…}'` |
| Delete a bundle | `DELETE /api/logsummary/bundles/{id}` | `dbcanvas logs delete <id>` |
| Upload logs | `POST /api/logsummary/upload` *(multipart, field `files`)* | `curl -F files=@mysqld.log …` |

```sh
dbcanvas logs collect pxc-lab --nodes pxc-01,pxc-02,pxc-03 | jq '{id, verdict}'
dbcanvas logs events <id> --severity bad --limit 20
```

## FTDC Summary

Charts and findings from MongoDB's `diagnostic.data`. See [FTDC Summary](FTDC_SUMMARY.md).

![FTDC Summary: a decoded capture, with the charts worth looking at called out](screenshots/ftdc-summary.png)

| To do this | API | CLI |
| --- | --- | --- |
| MongoDB nodes that can be read | `GET /api/ftdc/targets` | `dbcanvas ftdc targets` |
| Read a running node's `diagnostic.data` | `POST …/nodes/{nid}/ftdc` | `dbcanvas ftdc node <stack> <node>` |
| Compare two windows | `POST /api/ftdc/compare` | `dbcanvas api POST /api/ftdc/compare --data '{…}'` |
| Upload `diagnostic.data` | `POST /api/ftdc/upload` *(multipart, field `files`)* | `curl -F files=@metrics.2026-09-02 …` |

## Stalk Summary

Charts, verdicts and configuration advice from a `pt-stalk` capture. See
[Stalk Summary](STALK_SUMMARY.md).

![The Stalk Summary](screenshots/stalk-summary.png)

| To do this | API | CLI |
| --- | --- | --- |
| Capture and analyse in one request | `POST …/nodes/{nid}/stalksummary` | `dbcanvas api POST …/stalksummary` |
| Analyse a kept archive | `POST /api/stalksummary/archive/{aid}` | `dbcanvas stalk analyse <aid>` |
| Compare two captures | `POST /api/stalksummary/compare` | `dbcanvas api POST /api/stalksummary/compare --data '{…}'` |
| Upload an archive | `POST /api/stalksummary/upload` *(multipart, field `file`)* | `curl -F file=@stalk.tar.gz …` |

## Operator Summary

Workload health, custom-resource status and operator errors from a
`pt-k8s-debug-collector` cluster-dump, taken here or on any cluster. See
[Operator Summary](OPERATOR_SUMMARY.md).

| To do this | API | CLI |
| --- | --- | --- |
| Capture and analyse in one request | `POST …/frames/{fid}/k3d/opsummary` | `dbcanvas api POST …/k3d/opsummary` |
| Analyse a kept cluster-dump | `POST /api/opsummary/dumps/{did}` | `dbcanvas api POST /api/opsummary/dumps/3` |
| Every kept cluster-dump | `GET /api/opsummary/dumps` | `dbcanvas api GET /api/opsummary/dumps` |
| Upload a cluster-dump | `POST /api/opsummary/upload` *(multipart)* | `curl -F file=@cluster-dump.tar.gz …` |

## Diagnostic captures

`pt-stalk` (MySQL family) and `pg_gather` (PostgreSQL) run on the node;
`pt-k8s-debug-collector` collects a Kubernetes cluster.

| To do this | API | CLI |
| --- | --- | --- |
| Start pt-stalk | `POST …/nodes/{nid}/ptstalk` | `dbcanvas stalk start <stack> <node>` |
| Its status and what it has collected | `GET …/nodes/{nid}/ptstalk` | `dbcanvas stalk status <stack> <node>` |
| Download the current capture | `GET …/nodes/{nid}/ptstalk/download` | `dbcanvas stalk download <stack> <node> [--out F]` |
| Kept archives from this node · from everywhere | `GET …/nodes/{nid}/ptstalk/archives` · `GET /api/ptstalk/archives` | `dbcanvas stalk archives` |
| Download · annotate · delete a kept archive | `GET /api/ptstalk/archives/{aid}/download` · `POST …/{aid}/note` · `DELETE /api/ptstalk/archives/{aid}` | `dbcanvas api GET /api/ptstalk/archives/3/download --out stalk.tar.gz` |
| Start pg_gather · status · report | `POST …/nodes/{nid}/pggather` · `GET …/pggather` · `GET …/pggather/download` | `dbcanvas api POST …/pggather` |
| Start a Kubernetes cluster-dump · status | `POST`/`GET …/frames/{fid}/k3d/collect` | `dbcanvas api POST …/k3d/collect` |
| Kept cluster-dumps for this cluster | `GET …/frames/{fid}/k3d/collect/dumps` | `dbcanvas api GET …/k3d/collect/dumps` |
| Download · delete a kept cluster-dump | `GET /api/k8sdumps/{did}/download` · `DELETE /api/k8sdumps/{did}` | `dbcanvas api GET /api/k8sdumps/3/download --out cluster-dump.tar.gz` |

Kept archives outlive the node they came from, so you can compare before and after a
node is recreated.

## Operator Debugger

Step through a Kubernetes operator under Delve. See
[Operator Debugger](OPERATOR_DEBUGGER.md).

![The Operator Debugger stopped in Reconcile](screenshots/operator-debugger.png)

| To do this | API | CLI |
| --- | --- | --- |
| Frames running their operator under Delve | `GET /api/k3d/debug/targets` | `dbcanvas api GET /api/k3d/debug/targets` |
| The operator's source tree · one file | `GET …/frames/{fid}/k3d/debug/sources` · `GET …/k3d/debug/source?path=` | `dbcanvas api GET …/debug/sources` |
| The live debug session | `GET …/frames/{fid}/k3d/debug/ws` *(WebSocket)* | *(UI only)* |
| Force a reconcile | `POST …/frames/{fid}/k3d/debug/reconcile` | `dbcanvas api POST …/debug/reconcile` |

## Core Dump Analyzer

Read a crashed `mysqld`'s core dump and get a diagnosis. See
[Core Dump Analyzer](CORE_DUMP_ANALYZER.md#using-it).

![The Core Dump Analyzer: a SIGABRT read out of a real core, with its threads and stack](screenshots/core-dump-analyzer.png)

| To do this | API | CLI |
| --- | --- | --- |
| Linux Clients set up as core-dump hosts | `GET /api/gdb/targets` | `dbcanvas api GET /api/gdb/targets` |
| The cores available, and whether symbols and libraries match | `GET …/nodes/{nid}/gdb/cores` | `dbcanvas api GET …/gdb/cores` |
| The live gdb session | `GET …/nodes/{nid}/gdb/ws` *(WebSocket)* | *(UI only)* |

The WebSocket takes one JSON command per message, `{"cmd": …}`: `open`, `backtrace`,
`variables`, `evaluate`, `disassemble`, `source`, `console`, and:

| `cmd` | Returns |
| --- | --- |
| `threads` | every thread's stack at once, identical stacks folded |
| `framevars` | arguments and locals for the frames in `levels` of `thread` |
| `children` | the members of `expr` in `frame` of `thread`, one level at a time |

`source` returns `file` (as requested) and `path` (where it was found).

---

# Managing a node

## Node file manager

Read and write files inside a node.

![Dragging a file onto a node](screenshots/file-drop.png)
![The File Manager browsing a node's filesystem](screenshots/file-manager.png)

| To do this | API | CLI |
| --- | --- | --- |
| Nodes that can be browsed | `GET /api/stacks/{id}/fs/nodes` | `dbcanvas api GET /api/stacks/1/fs/nodes` |
| List a directory | `GET …/nodes/{nid}/fs/list?path=` | `dbcanvas api GET '…/fs/list?path=/etc'` |
| Read a text file · write one | `GET …/fs/read?path=` · `POST …/fs/write` | `dbcanvas api GET '…/fs/read?path=/etc/my.cnf'` |
| Download a file, or a directory as `.tar.gz` | `GET …/nodes/{nid}/fs/download?path=` | `dbcanvas node cp <stack>:<node>:/path ./` |
| Upload into a directory | `POST …/nodes/{nid}/fs/upload` *(multipart)* | `dbcanvas node cp ./file <stack>:<node>:/dir/` |
| mkdir · delete · rename | `POST …/fs/mkdir` · `/fs/delete` · `/fs/rename` | `dbcanvas api POST …/fs/mkdir --data '{"path":"/opt/x"}'` |
| chmod · chown | `POST …/fs/chmod` · `/fs/chown` | `dbcanvas api POST …/fs/chmod --data '{…}'` |
| Users and groups inside the node | `GET …/nodes/{nid}/fs/identities` | `dbcanvas api GET …/fs/identities` |
| **Copy node to node directly** | `POST …/nodes/{nid}/fs/transfer` | `dbcanvas node cp <stack>:a:/f <stack>:b:/f` |

Uploads are streamed and limited by **Settings → Node file uploads**. `fs/transfer`
copies between nodes on the host without passing through your machine.

## Certificates

The Intranet node is the stack's certificate authority; every node trusts it.

| To do this | API | CLI |
| --- | --- | --- |
| The CA certificate | `GET …/nodes/{nid}/cert` (Intranet node) | `dbcanvas api GET …/cert` |
| Reissue the Intranet node's certificate | `POST …/nodes/{nid}/cert` | `dbcanvas api POST …/cert` |
| Client certificates: list · issue · fetch (with key and chain) · delete | `GET`/`POST …/nodes/{nid}/dbcerts` · `GET …/dbcerts/{user}` · `POST …/dbcerts/delete` | `dbcanvas api POST …/dbcerts --data '{"user":"appuser"}'` |
| A database node's server certificate: read · reissue with a lifetime (`{"value": 10, "unit": "minutes"}`; `minutes`, `hours` or `days`) over the installed files, without a restart | `GET`/`POST …/nodes/{nid}/mysql/cert` (every MySQL-family node; `/pxc/cert` also works), `/pg/cert`, `/mongo/cert`, `/pmm/cert` | `dbcanvas api POST …/mysql/cert --data '{"value":10,"unit":"minutes"}'` |

## Intranet mail

| To do this | API | CLI |
| --- | --- | --- |
| List · create mailboxes | `GET`/`POST …/nodes/{nid}/email/users` | `dbcanvas api GET …/email/users` |
| Set a password · delete | `POST …/email/users/password` · `/email/users/delete` | `dbcanvas api POST …/email/users/delete --data '{…}'` |

## Intranet LDAP

The directory LDAP authentication uses.

| To do this | API | CLI |
| --- | --- | --- |
| Users: list · create | `GET`/`POST …/nodes/{nid}/ldap/users` | `dbcanvas api GET …/ldap/users` |
| Users: update · set password · delete | `POST …/ldap/users/update` · `/password` · `/delete` | `dbcanvas api POST …/ldap/users/password --data '{…}'` |
| Groups (with members): list · create | `GET`/`POST …/nodes/{nid}/ldap/groups` | `dbcanvas api GET …/ldap/groups` |
| Groups: set members · delete | `POST …/ldap/groups/members` · `/delete` | `dbcanvas api POST …/ldap/groups/members --data '{…}'` |

## Intranet DNS

Records added by hand beside the automatic ones. A custom record replaces an automatic
one with the same name or address. Every write returns the updated view.

| To do this | API | CLI |
| --- | --- | --- |
| Automatic and custom records, and the reverse zones | `GET …/nodes/{nid}/dns` | `dbcanvas api GET …/dns` |
| Add an A, CNAME or PTR record | `POST …/nodes/{nid}/dns/records` | `dbcanvas api POST …/dns/records --data '{"type":"A","name":"db","value":"10.1.2.3","ptr":true}'` |
| Change · delete one, by id | `POST …/dns/records/update` · `/delete` | `dbcanvas api POST …/dns/records/delete --data '{"id":"…"}'` |

## Samba AD DC

| To do this | API | CLI |
| --- | --- | --- |
| Users: list · create | `GET`/`POST …/nodes/{nid}/samba/users` | `dbcanvas api GET …/samba/users` |
| Users: update · set password · delete | `POST …/samba/users/update` · `/password` · `/delete` | `dbcanvas api POST …/samba/users/password --data '{…}'` |
| Groups: list · create | `GET`/`POST …/nodes/{nid}/samba/groups` | `dbcanvas api GET …/samba/groups` |
| Groups: set members · delete | `POST …/samba/groups/members` · `/delete` | `dbcanvas api POST …/samba/groups/members --data '{…}'` |
| The `krb5.conf` a client needs | `GET …/samba/krb5` | `dbcanvas api GET …/samba/krb5` |
| Nodes a service principal can be created for | `GET …/samba/targets` | `dbcanvas api GET …/samba/targets` |
| Kerberos principals: list · register | `GET`/`POST …/samba/principals` | `dbcanvas api POST …/samba/principals --data '{…}'` |
| Export a keytab | `GET …/samba/keytab` | `dbcanvas api GET …/samba/keytab` |
| Issue the LDAPS certificate | `POST …/samba/cert` | `dbcanvas api POST …/samba/cert` |

## SeaweedFS

![Browsing a SeaweedFS bucket](screenshots/seaweedfs-buckets.png)

| To do this | API | CLI |
| --- | --- | --- |
| Running SeaweedFS nodes and their buckets | `GET /api/stacks/{id}/seaweed/nodes` | `dbcanvas api GET /api/stacks/1/seaweed/nodes` |
| Browse a bucket | `GET …/nodes/{nid}/seaweed/objects` | `dbcanvas api GET …/seaweed/objects` |
| Download one object | `GET …/nodes/{nid}/seaweed/download?bucket=&path=` | `dbcanvas api GET '…/seaweed/download?bucket=b&path=p' --out f` |
| Upload files into a folder | `POST …/nodes/{nid}/seaweed/upload` *(multipart)* | `curl -F` |
| Copy objects to another bucket | `POST …/nodes/{nid}/seaweed/transfer` | `dbcanvas api POST …/seaweed/transfer --data '{…}'` |
| Delete objects (a folder needs `recursive`) | `POST …/nodes/{nid}/seaweed/delete` | `dbcanvas api POST …/seaweed/delete --data '{…}'` |

**UI:** **Files…** on the node's Buckets tab, or **Bucket file manager** on its
right-click menu: two panes, with download, drag-and-drop upload, delete and copy
between buckets. Uploads follow **Settings → Node file uploads**. Objects are ordinary
S3 objects.

## OpenBao

| To do this | API | CLI |
| --- | --- | --- |
| Initialised, sealed, key thresholds | `GET …/nodes/{nid}/openbao/status` | `dbcanvas api GET …/openbao/status` |
| Unseal with the stored keys | `POST …/nodes/{nid}/openbao/unseal` | `dbcanvas api POST …/openbao/unseal` |

---

# Watching

## Dashboard

![The dashboard](screenshots/dashboard.png)

| To do this | API | CLI |
| --- | --- | --- |
| Counters: stacks, nodes, engines, jobs, recent activity | `GET /api/dashboard/summary` | `dbcanvas dashboard` |
| Live CPU, memory, network and disk per node | `GET /api/dashboard/stats` | `dbcanvas dashboard --live` |
| Is a newer DBCanvas available, and what changed | `GET /api/dashboard/updates` | `dbcanvas updates [--full]` |

`stats` is sampled on request and cached for two seconds.

`updates` is the **Check for updates** button; DBCanvas never checks on its own. It
asks the DBCanvas GitHub repository for the latest released version and its release
notes, and returns `current`, `latest`, `available` and `notes` (each release after
yours, newest first, in the same shape as `GET /api/whatsnew`). A development build
returns `"dev": true` and never `available`. If the notes cannot be read, `notesError`
says why and the check still succeeds. If GitHub cannot be reached, the answer is
`502`.

## Notifications

| To do this | API | CLI |
| --- | --- | --- |
| List, with the unread count | `GET /api/notifications` | `dbcanvas notifications` |
| **Live stream** | `GET /api/notifications/stream` *(SSE)* | `curl -N -H "Authorization: Bearer $T" …/stream` |
| Mark all read | `POST /api/notifications/read-all` | `dbcanvas notifications --read-all` |
| Mark one read | `POST /api/notifications/{id}/read` | `dbcanvas api POST /api/notifications/7/read` |

![The notification bell, open](screenshots/notifications.png)

The stream stays open and sends each notification as one JSON event — a deploy
finishing, a token created, a stack expiring — so a script can wait without polling.

## Kanban

Boards of columns of cards ([Kanban](KANBAN.md)). A board belongs to its owner; a
shared board's columns and cards can be edited by any signed-in user. Positions are
indexes: `0` is the top of a column (or leftmost on the board); an index past the end
means the end.

| To do this | API | CLI |
| --- | --- | --- |
| Your boards and shared ones | `GET /api/kanban/boards` | `dbcanvas api GET /api/kanban/boards` |
| Create a board | `POST /api/kanban/boards` `{"name", "shared", "columns": ["To do", …]}` | — |
| A board with its columns and cards (`?since=<rev>` for a cheap poll) | `GET /api/kanban/boards/{id}` | `dbcanvas api GET /api/kanban/boards/3` |
| Rename or share it · delete it *(owner)* | `PUT /api/kanban/boards/{id}` `{"name", "shared"}` · `DELETE …` | — |
| Add · change · delete a column | `POST /api/kanban/boards/{id}/columns` `{"name", "index"}` · `PUT /api/kanban/columns/{cid}` `{"name", "wipLimit"}` or `{"color"}` · `DELETE …` | — |
| Move a column | `POST /api/kanban/columns/{cid}/move` `{"index"}` | — |
| Add a card | `POST /api/kanban/columns/{cid}/cards` `{"title", "description", "labels", "assigneeId", "due", "color", "index"}` | — |
| Change · delete a card | `PUT` · `DELETE /api/kanban/cards/{kid}` | — |
| Move a card | `POST /api/kanban/cards/{kid}/move` `{"columnId", "index"}` | `dbcanvas api POST /api/kanban/cards/12/move --data '{"columnId":4,"index":0}'` |
| Who a card can be assigned to | `GET /api/kanban/people` | — |

## Shared sessions

A stack's owner shares a live session through a link that lasts up to two hours (the
administrator's **Shared sessions** setting may set less). Guests give a name and
email, wait in a lobby until admitted, then follow the host and chat; a guest given
control can do anything the host can in the workspace. An administrator must turn
it on (`allowGuestSessions`). See [Shared sessions](SHARED_SESSIONS.md).

Ended sessions' records (guests' names, emails and addresses, the transcript, guest
actions) are deleted after `sessionRetentionDays` (default 90). Recordings are kept
`recordingRetentionDays` (default 30) unless the host changes the date.

| To do this | API | CLI |
| --- | --- | --- |
| Start a session and get its link *(password sign-in only)* | `POST /api/share/sessions` `{"minutes": 5–120, "hideSecrets": false, "mirror": true}`, or `POST /api/stacks/{id}/share` to file it on a stack | — |
| Mirror everything on or off | `POST /api/share/sessions/{sid}/mirror` `{"mirror"}` | — |
| Your sessions (`?live=1` for running ones) | `GET /api/share/sessions` | `dbcanvas api GET '/api/share/sessions?live=1'` |
| One session: guests and who has control | `GET /api/share/sessions/{sid}` | `dbcanvas api GET /api/share/sessions/3` |
| Admit · deny · remove a guest | `POST …/sessions/{sid}/guests/{gid}/admit` · `/deny` · `/remove` | `dbcanvas api POST /api/share/sessions/3/guests/5/admit` |
| Mute or unmute a guest | `POST …/guests/{gid}/mute` `{"muted"}` | — |
| Give or take back control | `POST /api/share/sessions/{sid}/control` `{"to": 5}` or `{"to": "host"}` | — |
| Let every guest draw, or none · one guest | `POST …/sessions/{sid}/draw` `{"draw"}` · `POST …/guests/{gid}/draw` | — |
| Start recording (video from the host's browser) | `POST /api/share/sessions/{sid}/recordings` `{"mime"}` | — |
| Append a chunk, in order | `POST /api/share/recordings/{rid}/chunks?seq=N` *(raw bytes)* | — |
| Stop a recording | `POST /api/share/recordings/{rid}/finish` `{"durationMs"}` | — |
| Your recordings | `GET /api/share/recordings` | `dbcanvas api GET /api/share/recordings` |
| Download one (`?inline=1` to play) | `GET /api/share/recordings/{rid}/file` | `dbcanvas api GET /api/share/recordings/4/file --out session.webm` |
| Its chat transcript (text; `?format=json`) | `GET /api/share/recordings/{rid}/transcript` | `dbcanvas api GET /api/share/recordings/4/transcript --out chat.txt` |
| Rename it or move its purge date | `PUT /api/share/recordings/{rid}` `{"title", "purgeAt": "2026-12-31"}` | — |
| Purge it now | `DELETE /api/share/recordings/{rid}` | `dbcanvas api DELETE /api/share/recordings/4` |
| New invitation link (retires the old one) *(password sign-in only)* | `POST /api/share/sessions/{sid}/link` | — |
| End it now | `POST /api/share/sessions/{sid}/end` | `dbcanvas api POST /api/share/sessions/3/end` |
| Transcripts filed on a stack | `GET /api/stacks/{id}/share/transcripts` | — |
| Download a session transcript (text; `?format=json`) | `GET /api/share/sessions/{sid}/transcript` | `dbcanvas api GET /api/share/sessions/3/transcript --out session.txt` |
| **Live channel**: presence, follow, cursor, chat, control, drawings | `GET /api/share/sessions/{sid}/ws` *(WebSocket)* | — |
| Shared terminal: open · close · watch | `POST …/sessions/{sid}/terms` · `DELETE …/terms/{tid}` · `GET …/terms/{tid}/ws` | — |
| Guest side *(no sign-in)*: the join page · join · status · leave | `GET /api/join/{token}` · `POST /api/join/{token}` `{"name", "email", "avatar"}` · `GET …/status` · `POST …/leave` | — |
| Join as your DBCanvas account *(no sign-in; does not sign the browser in)* | `POST /api/join/{token}/account` `{"username", "password"}` or `{"useSession": true}` | — |

**Opening a node's web UI in a window.** `POST /api/browse {"url":
"http://localhost:32790/vnc.html"}` finds the node publishing that host port, checks
it is on one of your stacks, and returns a `/_p/<key>/…` address that DBCanvas proxies
to the node — so only DBCanvas's own port needs to be reachable. In a session, a
window the host or driver opens opens for everyone. A watching guest can view proxied
pages but not submit to them; a VNC desktop is shown to them view-only.

**Opening a link in the VNC desktop.** `POST /api/browse/desktop {"url": "…"}` opens
the link as a Firefox tab on the stack's running Ubuntu VNC desktop, using the node's
name on the stack network, and returns `{"desktopLink", "desktop", "opened"}`. `409`
when the stack has no running VNC desktop; a watching guest gets `403`.

**Guests are not accounts.** A guest's requests carry `X-DBCanvas-Guest` (or
`?guest=1` on sockets and downloads) and act as the host with `read` scope while
watching and `write` while in control. A guest never reaches the host's account,
tokens, notifications or administration. Every guest write is recorded, with the
guest's name, in the transcript.

## API metadata

![API → Endpoints: the generated catalogue with a row expanded](screenshots/api-endpoints.png)
![API → Getting started](screenshots/api-getting-started.png)

| To do this | API | CLI |
| --- | --- | --- |
| The endpoint catalogue | `GET /api/meta/endpoints` | `dbcanvas endpoints [--group G] [--scope S] [-q term] [--long]` |
| OpenAPI 3.1 document | `GET /api/meta/openapi.json` | `dbcanvas api GET /api/meta/openapi.json` |
| Download the CLI for a platform | `GET /api/cli/download?os=&arch=` | *(how you get the CLI)* |
| The CLI's own help, from the binary this server ships | `GET /api/cli/help` | `dbcanvas --help` |

> [!NOTE]
> The Operator Debugger and Core Dump Analyzer sessions are interactive WebSockets
> built for the UI; the CLI does not drive them.

---

See also: [HTTP API](API.md) · [`dbcanvas-cli`](CLI.md) · [Feature guides](README.md)
