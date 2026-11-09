# HTTP API

Everything the web UI does goes through this HTTP API, so anything you can click you
can also script.

The **API** page in the app lists every endpoint on your installation, with a `curl`
line to copy, and is where you create tokens. For each feature's endpoints and the
matching `dbcanvas-cli` command, see the [API & CLI reference](API_REFERENCE.md).

## Authenticate

| Credential | Sent as | Used by |
| --- | --- | --- |
| Session cookie | `Cookie: dbcanvas_session=…`, set by `POST /api/auth/login` | the web UI |
| API token | `Authorization: Bearer dbc_…` | scripts, `dbcanvas-cli`, CI |

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" http://localhost:8080/api/stacks
```

A token acts as your account, with your permissions. It stops working when your
account is disabled or rejected.

### As a guest

When an administrator turns on **Guest sign-in** (Settings → Access & security, off by
default), `POST /api/auth/guest` creates a guest account of its own, without a password and with
the user role, and signs it in, and sets the same session cookie as `POST /api/auth/login`. That session
can create a token (`POST /api/tokens`) like any other, which is what
`dbcanvas-cli login --guest` does. `GET /api/setup/status` says whether guest sign-in is on
(`"guestLogin": true`), and `GET /api/me` marks the account (`"guestAccount": true`).

```sh
curl -s -c jar -X POST http://localhost:8080/api/auth/guest
curl -s -b jar -H 'Content-Type: application/json' \
  -d '{"name":"my script","scope":"write","days":7}' http://localhost:8080/api/tokens
```

Guests need the **API** feature in their security group to create or use a token (see
[Security groups](#security-groups)); by default they do not have it, so an administrator
ticks *API* for Guests first.

Every guest shares the account, its stacks and its tokens. A guest token is write scope
at most; it cannot change the account's password or profile; and it stops working, and
is revoked, the moment an administrator turns guest sign-in off.

## Create a token

1. Open **API → Tokens** (or run `dbcanvas-cli login` or `dbcanvas-cli token create`).
2. Give it a name, a scope and an expiry.
3. Copy the secret. It is shown **once**: DBCanvas keeps only a hash of it, and the
   list shows only its first characters (`dbc_` plus eight).

A token is `dbc_` followed by 43 characters, so secret scanners can find a leaked one.

### Scopes

| Scope | Can call |
| --- | --- |
| `read` | every `GET`, plus `POST`s that change nothing (validate, compare, preview, a lab step check) |
| `write` | everything your account can do |
| `admin` | also the administrator endpoints: `/api/users/*`, `/api/admin/*`, `PUT /api/system/settings`, `POST /api/images/{id}/build`, `POST /api/stacks/{id}/owner`. Only an administrator can create one. |

A token with too narrow a scope gets `403` naming the scope it needs. A token's scope
is also capped by its owner's current role: when an administrator is demoted, their
`admin` tokens act as `write` tokens.

### Expiry

The UI offers 7, 30, 60 or 90 days, and **Never** for administrators. Through the API
you can send any number of `days`. Every dated token is capped by the instance-wide
**API token lifetime** in **Settings** (default 90 days, 1–365); a longer request is
shortened, not refused.

An expired or revoked token stays in your list, marked as such, for 30 days and is
then deleted.

### Limit a token to some stacks

A token can also be limited to the stacks it creates itself, and/or to stacks carrying given
labels. Pass `"limit": {"ownStacks": true, "labels": {"tool": "ci"}}` to `POST /api/tokens`, or use
`dbcanvas-cli token create --own-stacks --limit-label tool=ci`.

- **What it can call:** a limited token may call any route of a stack within its limit, list
  stacks (filtered to its limit), create and compose stacks, and read the catalogues and
  `/api/me`. Everything else answers `403`, whatever its scope.
- **Labels on new stacks:** a stack it creates gets its labels, and it cannot set labels that
  would drop them.

### Revoke a token

Revoking is immediate. You can revoke your own tokens; an administrator can revoke
anyone's under **API → All tokens**, and the owner is notified. Disabling or rejecting
an account revokes its tokens and its sessions together.

## What a token can never do

These endpoints refuse a token, whatever its scope, and need a password sign-in:

| Endpoint | Why |
| --- | --- |
| `POST /api/tokens` | create a token (`dbcanvas-cli token create` and `dbcanvas-cli login` ask for your password) |
| `POST /api/me/password` | change your password (`dbcanvas-cli password` asks for the current one) |
| `POST /api/users`, `POST /api/users/{id}/password`, `POST /api/users/{id}/reset-link` | create an account, or set or reset someone else's password |
| `POST /api/share/sessions`, `POST /api/stacks/{id}/share`, `POST /api/share/sessions/{sid}/link` | start a shared session or issue its link |

Changing your password needs your **current** password. It signs out every other
session and keeps yours. API tokens keep working unless you send `revokeTokens`
(`--revoke-tokens` in the CLI, or **Also revoke my API tokens** on the **Profile**
page).

An account given a temporary password gets `403` on everything else until it sets
its own.

## Find an endpoint

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" \
  http://localhost:8080/api/meta/endpoints | jq '.groups[].name'
```

| Endpoint | Returns |
| --- | --- |
| `GET /api/meta/endpoints` | every endpoint, grouped by feature, with a one-line summary, path parameters, the scope it needs and how it is called |
| `GET /api/meta/openapi.json` | the same, as an OpenAPI 3.1 document for generated clients, Postman or Bruno. Request and response bodies are declared as free-form JSON. |
| `GET /api/stacks/design/catalog` | every node and cluster type a design can hold, its options, relations and lines — see [the design catalogue](API_REFERENCE.md#the-design-catalogue-and-editing-a-design-one-piece-at-a-time) |

`dbcanvas-cli endpoints` prints the catalogue in a terminal.

## Endpoints that are not plain JSON

Most endpoints take and return JSON. The catalogue marks the others:

| Kind | Examples | How to call it |
| --- | --- | --- |
| Upload | `…/fs/upload`, `/api/pktinspect/upload`, `/api/ftdc/upload` | `multipart/form-data`: `curl -F file=@capture.pcap` |
| Download | `…/download`, `…/export`, `…/raw` | an attachment: `curl -OJ` |
| Raw bytes | `POST /api/share/recordings/{rid}/chunks` | the request body is the bytes |
| Event stream | `GET /api/notifications/stream` | `text/event-stream`; each event's data is one notification as JSON |
| WebSocket | `…/term`, `…/gdb/ws`, `…/k3d/debug/ws`, `/api/share/sessions/{sid}/ws` | send the `Authorization` header on the handshake; there is no `?token=` parameter. Browsers use the session cookie. |

## Status codes

| Code | Means |
| --- | --- |
| `400` | a bad body or parameter; `error` says which |
| `401` | not signed in, or the token is unknown, expired or revoked |
| `403` | not permitted: not your stack, not an administrator, a token scope too narrow, or an endpoint that refuses tokens |
| `404` | no such stack, node, capture or token — or no such endpoint (a mistyped path, or the right path with the wrong method) |
| `409` | wrong state for the request, for example the node is not running, or a stack name you already use |
| `410` | the stack was deleted: `deleted` says when, by whom and why (`user`, `admin`, `account`, `guest-idle`); kept 30 days |
| `502` | an outside service could not be reached (the update check) |

Errors are always `{"error": "…"}`.

## Who can reach what

A stack belongs to the account that created it. You can reach your own stacks; an
administrator can reach any. A token sees exactly what its owner sees in the UI.

These need no sign-in: `GET /api/setup/status`, `POST /api/setup`,
`POST /api/auth/register`, `POST /api/auth/login`, `POST /api/auth/guest`, `POST /api/auth/logout`,
`GET`/`POST /api/auth/reset/{token}`, and the `/api/join/{token}` endpoints a shared
session's guests use.

## Security groups

Every account is in one of three groups: **Administrators** (role `admin`), **Regular
Users** (role `user`) and **Guests** (every guest account). An administrator decides
which features — the pages of the app — each group may use, under Settings → Security
groups, with `PUT /api/security/groups`, or with `dbcanvas-cli group set`.

The server enforces it on every request. A route that belongs to a feature the caller's
group does not have answers `403` with `your security group (Guests) does not have access
to Kanban`, whether the request came from the browser, a token or `dbcanvas-cli`. A few
routes belong to no feature and are always allowed: who you are (`/api/me`,
`/api/setup/status`), your preferences, notifications and signing out.

**API** is the feature that lets a group use API tokens at all. Without it, creating a token
(and so `dbcanvas-cli login`) is refused, and so is every request made with a token the account
already holds.

| Group | Default |
| --- | --- |
| Administrators | everything |
| Regular Users | everything but Manage Users |
| Guests | Dashboard and Database Stacks |

Some cells are fixed. Administrators always keep **Settings** and **Manage Users**, so nobody
can lock every administrator out. No other group can be given **Manage Users**, because
managing accounts includes making an administrator. Most tools work on a deployed stack, so
they need **Database Stacks**: granting one grants Database Stacks, and removing Database
Stacks removes them.

```sh
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/api/security/groups
curl -s -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"allowed":{"guest":{"kanban":true,"api":true}}}' http://localhost:8080/api/security/groups
```

Cells you leave out keep their value. `GET /api/setup/status` lists the features the caller
may use (`features`) and their group (`securityGroup`).

## Stacks for programs

**Names.** An account's stack names are unique, compared without regard to case. Creating,
renaming or transferring onto a name in use answers `409`, naming the existing stack's id. Every
route takes the stack's id, which never changes.

**Labels.** Labels are `key=value` tags a program can find its stacks by.

- **Set:** on `POST /api/stacks` or a compose spec as `"labels": {"tool": "ci", "run": "12"}`, or
  replace them with `PUT /api/stacks/{id}/labels`.
- **Select:** `GET /api/stacks?label=tool=ci&label=run` keeps the stacks carrying every term (`run`
  alone means any value).
- **Rules:** keys are lowercase letters, digits and `. _ / -` (up to 63 characters); values are up
  to 128 characters. A stack carries at most 32 labels.
- **CLI:** `stack compose|create --label k=v`, `stack list --label k=v`, `stack label <stack>
  k=v k-`, and `stack destroy|delete --label k=v` for every match.
- **Web:** the stack list shows each stack's labels under its name. The pencil beside them edits
  them, one `key=value` a line.

**Events.** `GET /api/stacks/events` is a server-sent event stream of your stacks' lifecycle:
`created`, `deploying`, `destroyed`, `deleted` (with `reason` and `by`), `expired`, `extended` and
`labels`. Each event carries the stack's id, name, owner and labels. `?label=` narrows it, and an
administrator's `?all=1` covers every stack. `dbcanvas-cli stack events` follows it. A stack that
was deleted answers `410` with how it went, instead of `404`.

## Limits and checks (administrators)

**Limits per security group.** `GET`/`PUT /api/security/policies` (`dbcanvas-cli group policy`)
sets, per group:

- `maxTtl`: the longest lifetime a stack is created or extended with
- `maxStacks`: stacks per account
- `maxContainers`: deployed nodes per account
- `deniedNodeTypes`: node and cluster types the group may not deploy (refused at validation)

Empty or 0 is no limit. See [Security model](SECURITY.md).

**Doctor.** `GET /api/system/doctor` (`dbcanvas-cli doctor`) checks:

- Docker
- the node images for this release
- the version catalogue's age
- passwords still at their `.env.example` value
- the encryption key
- exposure and sign-in settings
- the CLI and Big Hole files

Each check is `ok`, `warn` or `fail`, and every one that is not `ok` says what to do.

**The CLI download.** `GET /api/cli/download` sends `X-Checksum-Sha256` (and `Digest`) and
`X-DBCanvas-Version`. `GET /api/cli/checksums` lists every shipped binary's SHA-256 in `sha256sum`
format.

## Example: deploy a stack and wait for it

```sh
API=http://localhost:8080
AUTH="Authorization: Bearer $DBCANVAS_TOKEN"

# Create a stack from a built-in template, with a 4-hour lifetime
ID=$(curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"api-demo","ttl":"4h","templateId":"builtin:pxc-proxysql-pmm"}' \
  $API/api/stacks | jq -r .id)

curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/validate | jq .
curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/deploy

# Deploy returns at once; poll the node states until none is pending or provisioning
while curl -s -H "$AUTH" $API/api/stacks/$ID |
      jq -e '[.deployments[].state] | any(. == "pending" or . == "provisioning")' >/dev/null; do
  sleep 5
done
curl -s -H "$AUTH" $API/api/stacks/$ID | jq '.deployments[] | {nodeId, state}'

curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/destroy
```

`dbcanvas-cli stack create api-demo --template "PXC + ProxySQL + PMM" --ttl 4h --deploy --wait`
does the same in one line — see [`dbcanvas-cli`](CLI.md).
