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

## Create a token

1. Open **API → Tokens** (or run `dbcanvas login` or `dbcanvas token create`).
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

### Revoke a token

Revoking is immediate. You can revoke your own tokens; an administrator can revoke
anyone's under **API → All tokens**, and the owner is notified. Disabling or rejecting
an account revokes its tokens and its sessions together.

## What a token can never do

These endpoints refuse a token, whatever its scope, and need a password sign-in:

| Endpoint | Why |
| --- | --- |
| `POST /api/tokens` | create a token (`dbcanvas token create` and `dbcanvas login` ask for your password) |
| `POST /api/me/password` | change your password (`dbcanvas password` asks for the current one) |
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

`dbcanvas endpoints` prints the catalogue in a terminal.

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
| `404` | no such stack, node, capture or token |
| `409` | wrong state for the request, for example the node is not running |
| `502` | an outside service could not be reached (the update check) |

Errors are always `{"error": "…"}`.

## Who can reach what

A stack belongs to the account that created it. You can reach your own stacks; an
administrator can reach any. A token sees exactly what its owner sees in the UI.

These need no sign-in: `GET /api/setup/status`, `POST /api/setup`,
`POST /api/auth/register`, `POST /api/auth/login`, `POST /api/auth/logout`,
`GET`/`POST /api/auth/reset/{token}`, and the `/api/join/{token}` endpoints a shared
session's guests use.

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

`dbcanvas stack create api-demo --template "PXC + ProxySQL + PMM" --ttl 4h --deploy --wait`
does the same in one line — see [`dbcanvas-cli`](CLI.md).
