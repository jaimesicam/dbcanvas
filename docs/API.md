# HTTP API

Everything the web UI does goes through this HTTP API, so anything you can click you
can also script.

The **API** page in the app lists every endpoint, with a `curl` line to copy, and is
where you create and revoke tokens. For each feature's endpoints, see the
[API reference](API_REFERENCE.md).

The examples use `http://127.0.0.1:8080`, the default address. If 8080 is taken, a
free port is chosen; `port:` in `~/.dbcanvas/config.yaml` sets it.

## Authenticate

The port listens on `127.0.0.1` only, and every request needs a credential:

| Credential | Sent as | Used by |
| --- | --- | --- |
| Session cookie | `Cookie: dbcanvas_session=…`, set by the one-time start link (`dbcanvas open`) | the web UI |
| API token | `Authorization: Bearer dbc_…` | scripts, CI, another user of this computer |
| Guest cookie | set by `POST /api/join/{token}` | a shared session's guests |
| Join link | `/join/{token}` or `/api/join/{token}/…` in the path, while the session is live | a guest arriving at a shared session |

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" http://127.0.0.1:8080/api/stacks
```

Without a credential, only the start link, the licence texts and the web app's static
files answer; every other request gets `401`. The port also answers only requests
addressed to it by a loopback name (`127.0.0.1`, `localhost`, `[::1]`) or to the
`publicUrl` in `config.yaml`, and refuses a cross-site write or WebSocket whose
`Origin` is anywhere else.

**Signing a browser in.** `dbcanvas` (or `dbcanvas open`) opens the browser signed in.
`dbcanvas open --print` prints the link instead. A start link, `/start/{token}`, works
once, for a minute, and sets the session cookie.

**The control socket.** The `dbcanvas` command reaches the service through a control
socket that only the owner can open, so it needs no token. A few endpoints answer on
the control socket only and refuse the browser port: `POST /api/solo/start-link`,
`GET /api/solo/status`, `POST /api/solo/stop` and `GET /api/solo/passwords`
(`dbcanvas open`, `status`, `stop` and `passwords`).

## Create a token

```sh
dbcanvas token create ci-runner --scope write --days 30
```

Or open **API → Tokens** and give it a name, a scope and an expiry.

The secret is shown **once**: DBCanvas keeps only a hash of it, and the list shows only
its first characters (`dbc_` plus eight). A token is `dbc_` followed by 43 characters,
so secret scanners can find a leaked one.

`dbcanvas token ls` lists your tokens; `dbcanvas token rm ID` revokes one.

### Scopes

| Scope | Can call |
| --- | --- |
| `read` | every `GET`, plus `POST`s that change nothing (validate, compare, preview, a lab step check) |
| `write` | everything the web UI can do, except the endpoints `admin` adds |
| `admin` | also the owner-only endpoints: `PUT /api/system/settings`, `POST /api/images/{id}/build`, `GET /api/system/doctor` |

`dbcanvas token create` uses `write` unless `--scope` says otherwise. A token with too
narrow a scope gets `403` naming the scope it needs.

### Expiry

`dbcanvas token create` sets 30 days unless `--days` says otherwise; `--days 0` never
expires. The API page offers 7, 30, 60 or 90 days, and **Never**. Every dated token is
capped by the **API token lifetime** in **Settings** (`maxTokenDays`, default 90 days,
1–365); a longer request is shortened, not refused.

An expired or revoked token stays in the list, marked as such, for 30 days and is then
deleted.

### Limit a token

A token can be narrowed further:

| Limit | `dbcanvas token create` | `POST /api/tokens` `"limit"` |
| --- | --- | --- |
| Only the stacks it creates itself | `--own-stacks` | `"ownStacks": true` |
| Only stacks carrying these labels | `--label tool=ci` (repeatable) | `"labels": {"tool": "ci"}` |
| The longest a stack it creates may live | `--max-ttl 8h` | `"maxTtl": "8h"` |
| Node types it may not deploy | `--deny pmm,k3d` | `"deny": ["pmm", "k3d"]` |

- **What a stack-limited token can call:** any route of a stack within its limit, the
  stack list (filtered to its limit), creating and composing stacks, and the read-only
  discovery routes: the version catalogues, the design catalogue, templates, `/api/me`
  and `/api/meta`. Everything else answers `403`, whatever its scope.
- **Labels on new stacks:** a stack it creates gets its labels, and it cannot set
  labels that would drop them.
- **`maxTtl`:** a stack it creates cannot be given a longer lifetime, and cannot be
  extended.

### Revoke a token

Revoking is immediate: `dbcanvas token rm ID`, the API page, or
`DELETE /api/tokens/{id}`. A revoked token stays listed as revoked.

## What a token can never do

These endpoints refuse a token, whatever its scope. They need the owner's browser
session or the control socket:

| Endpoint | What it does |
| --- | --- |
| `POST /api/tokens` | create a token (`dbcanvas token create`) |
| `POST /api/share/sessions`, `POST /api/stacks/{id}/share`, `POST /api/share/sessions/{sid}/link` | start a shared session or issue its link |

## Find an endpoint

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" \
  http://127.0.0.1:8080/api/meta/endpoints | jq '.groups[].name'
```

| Endpoint | Returns |
| --- | --- |
| `GET /api/meta/endpoints` | every endpoint, grouped by feature, with a one-line summary, path parameters, the scope it needs and how it is called |
| `GET /api/meta/openapi.json` | the same, as an OpenAPI 3.1 document for generated clients, Postman or Bruno. Request and response bodies are declared as free-form JSON. |
| `GET /api/stacks/design/catalog` | every node and cluster type a design can hold, its options, relations and lines — see [the design catalogue](API_REFERENCE.md#the-design-catalogue-and-editing-a-design-one-piece-at-a-time) |
| `GET /api/cli/help` | the `dbcanvas` command's usage, as `dbcanvas help` prints it |

## Endpoints that are not plain JSON

Most endpoints take and return JSON. The catalogue marks the others:

| Kind | Examples | How to call it |
| --- | --- | --- |
| Upload | `…/fs/upload`, `/api/pktinspect/upload`, `/api/ftdc/upload` | `multipart/form-data`: `curl -F file=@capture.pcap` |
| Download | `…/download`, `…/export`, `…/stackfile`, `…/raw` | an attachment: `curl -OJ` |
| Raw bytes | `POST /api/share/recordings/{rid}/chunks` | the request body is the bytes |
| Event stream | `GET /api/notifications/stream`, `GET /api/stacks/events` | `text/event-stream`; each event's data is one JSON object |
| WebSocket | `…/term`, `…/gdb/ws`, `…/k3d/debug/ws`, `/api/share/sessions/{sid}/ws` | send the `Authorization` header on the handshake; there is no `?token=` parameter. Browsers use the session cookie. |

## Status codes

| Code | Means |
| --- | --- |
| `400` | a bad body or parameter; `error` says which |
| `401` | no credential, or the token is unknown, expired or revoked |
| `403` | not permitted: a token scope or limit too narrow, an endpoint that refuses tokens, a control-socket-only endpoint, or a cross-site request |
| `404` | no such stack, node, capture or token — or no such endpoint (a mistyped path, or the right path with the wrong method) |
| `409` | wrong state for the request, for example the node is not running, or a stack name already in use |
| `410` | the stack was deleted: `deleted` says when and how; kept 30 days |
| `421` | the request was addressed to a host name other than the port's own |
| `502` | an outside service could not be reached (the update check) |

Errors are always `{"error": "…"}`.

## Stacks for programs

**Names.** Stack names are unique, compared without regard to case. Creating or
renaming onto a name in use answers `409`, naming the existing stack's id. Every route
takes the stack's id, which never changes.

**Labels.** Labels are `key=value` tags a program can find its stacks by.

- **Set:** on `POST /api/stacks`, a compose spec or a stack file as
  `"labels": {"tool": "ci", "run": "12"}`, or replace them with
  `PUT /api/stacks/{id}/labels`.
- **Select:** `GET /api/stacks?label=tool=ci&label=run` keeps the stacks carrying every
  term (`run` alone means any value).
- **Rules:** keys are lowercase letters, digits and `. _ / -` (up to 63 characters);
  values are up to 128 characters. A stack carries at most 32 labels.
- **Web:** the stack list shows each stack's labels under its name. The pencil beside
  them edits them, one `key=value` a line.

**Stack files.** `GET /api/stacks/{id}/stackfile` returns a stack as YAML
(`dbcanvas export STACK`); `POST /api/stacks/apply` `{"file": "<YAML or JSON>",
"dryRun": false}` creates the stack or saves the file's design over the one of that
name (`dbcanvas up -f FILE`, which then deploys).

**Events.** `GET /api/stacks/events` is a server-sent event stream of your stacks'
lifecycle: `created`, `deploying`, `destroyed`, `deleted` (with `reason`), `expired`,
`extended` and `labels`. Each event carries the stack's id, name and labels. `?label=`
narrows it. A stack that was deleted answers `410` with how it went, instead of `404`.

## Doctor

`GET /api/system/doctor` (admin scope; `dbcanvas doctor` also checks the data folder,
networks and the service) checks:

- Docker
- the node images for this release
- the version catalogue's age
- the node account passwords
- the encryption key
- the bundled files

Each check is `ok`, `warn` or `fail`, and every one that is not `ok` says what to do.

## Example: deploy a stack and wait for it

```sh
API=http://127.0.0.1:8080
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

With a stack file, `dbcanvas up -f api-demo.yaml --wait` does the same in one line.
