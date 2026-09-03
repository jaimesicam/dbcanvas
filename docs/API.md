# HTTP API

Everything DBCanvas does, it does over HTTP. The web UI is a client of this API and
nothing more — there is no second, privileged path — so anything you can do on the
canvas you can do from a script, and anything a script can do you can watch happen
in the browser.

The **API** page in the app is the live version of this document: it lists every
endpoint on *your* installation, with a `curl` line you can copy, and it is where you
create tokens. This page explains the parts that are worth reading once.

**Looking for a specific feature's endpoint?** [API & CLI reference](API_REFERENCE.md)
walks every feature with its endpoints, the `dbcanvas-cli` command that calls each
one, and a screenshot of what you would have clicked instead.

---

## Authenticating

Two credentials work, and they resolve to the same account with the same permissions.

| Credential | Sent as | Who uses it |
| --- | --- | --- |
| **Session cookie** | `Cookie: dbcanvas_session=…`, set by `POST /api/auth/login` | the web UI |
| **API token** | `Authorization: Bearer dbc_…` | scripts, `dbcanvas-cli`, CI |

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" http://localhost:8080/api/stacks
```

A token acts as your account. It is not a service account, it has no permissions of
its own, and it stops working the moment your account does.

## Tokens

Create one under **API → Tokens**, or with `dbcanvas login`.

A token looks like `dbc_` followed by 43 characters. **The secret is shown once**, at
creation, and never again — DBCanvas stores only a SHA-256 hash of it, so there is
nothing to recover and nothing for a copy of the database to leak. What the UI keeps
is the first eight characters, which is enough to tell your own tokens apart and far
too little to guess the rest. The `dbc_` prefix is there so a secret scanner, a
pre-commit hook or a panicked search through your shell history can find one that
escaped.

### Scopes

| Scope | Can call |
| --- | --- |
| `read` | every `GET`, plus the few `POST`s that compute something without changing anything — `validate`, `compare`, `preview`, a lab step check |
| `write` | everything the account can do in the UI |
| `admin` | additionally the admin-only endpoints (`/api/users/*`, `/api/admin/*`, `PUT /api/system/settings`). Only an administrator can create one. |

Scopes are checked against the endpoint, not guessed: the route table records each
endpoint's method and whether it mutates, so a `read` token calling `POST
/api/stacks/12/deploy` gets a `403` that names the scope it would need.

A token's scope is also **capped by your account's role on every request**. If an
administrator is demoted, the `admin`-scope tokens they hold quietly become `write`
tokens; nobody has to remember to go and revoke them.

### Expiry

Pick 7, 30, 60 or 90 days, a custom number of days, or — as an administrator —
**never**. An instance-wide ceiling (**Settings → API tokens**, default 90 days)
applies to everyone who is not an administrator; asking for longer gets you the
ceiling rather than an error.

Expiry is enforced when the token is used, not by a background job, and an expired
token **stays in your list** marked `expired`. That is deliberate: "why did my script
start getting 401s on Tuesday" is a question the row answers and its absence does not.
Rows are deleted 30 days after they expire or are revoked.

### Two rules worth knowing

**Creating a token requires your password.** `POST /api/tokens` refuses bearer
authentication outright, whatever the token's scope. So a token that leaks cannot mint
a longer-lived replacement for itself — an attacker who has it still needs your
password to get anything more durable. This is also why `dbcanvas login` asks for a
password rather than accepting a token.

**Losing access loses the API too.** Disabling or rejecting an account revokes its
tokens in the same operation that revokes its browser sessions. Without that,
"disable" would close somebody's browser and leave their scripts running.

### The two things a token can never do

`POST /api/tokens` and `POST /api/me/password` both refuse bearer authentication
outright, whatever the token's scope, and for the same reason: they are the two
endpoints that would turn a leaked token into a permanent one, or into a full account
takeover. Both need a password sign-in — `dbcanvas token create` and
`dbcanvas password` prompt for one and use it directly.

Changing a password requires the **current** password even though the caller is
already signed in: a session somebody else got hold of should not be able to lock the
owner out. It signs out every *other* session and keeps the caller's. API tokens
survive unless `revokeTokens` is set — a scheduled rotation should not break a CI job,
while a password changed because it leaked should be able to take the tokens with it.

Revoking is immediate. You can revoke your own tokens; an administrator can revoke
anyone's from **API → All tokens**, and the owner is notified when they do.

## The endpoint catalogue

```sh
curl -s -H "Authorization: Bearer $DBCANVAS_TOKEN" \
  http://localhost:8080/api/meta/endpoints | jq '.groups[].name'
```

`GET /api/meta/endpoints` returns every endpoint grouped by feature, each with its
group, one-sentence summary, path parameters, the scope it needs, and how it
communicates. It is generated from the same table that installs the handlers, so it
describes what is actually being served — and a test fails the build if a new route
is added without a summary.

`GET /api/meta/openapi.json` is the same surface as an **OpenAPI 3.1** document, for
generated clients, Postman, Bruno or anything else that reads one. Paths, methods,
parameters, tags and security are all described; request and response bodies are
declared as free-form JSON rather than 219 hand-written schemas, because a second
description of the app would be a second thing to get out of date.

## Things that are not plain JSON

Most endpoints take and return JSON. Four kinds do not, and the catalogue marks each
one so you know before you call it:

| Kind | Endpoints | How to call it |
| --- | --- | --- |
| **Uploads** | `POST …/upload`, `POST /api/ftdc/upload`, `POST /api/logsummary/upload`, … | `multipart/form-data`; `curl -F file=@capture.pcap` |
| **Downloads** | `…/download`, `…/export`, `…/raw` | responds with an attachment; `curl -OJ` |
| **Event stream** | `GET /api/notifications/stream` | `text/event-stream`; each event's data is one JSON notification |
| **WebSocket** | `…/term`, `…/gdb/ws`, `…/k3d/debug/ws` | upgrades the connection. Set the `Authorization` header on the handshake — there is deliberately no `?token=` parameter, because a query string ends up in access logs and browser history. Browsers use the session cookie instead. |

## Status codes

| Code | Means |
| --- | --- |
| `400` | the body or a parameter is wrong; the `error` field says which |
| `401` | not signed in, or the token is unknown, expired or revoked |
| `403` | signed in but not permitted — not your stack, not an administrator, or the token's scope is too narrow (the message says which scope is needed) |
| `404` | no such stack, node, capture or token |
| `409` | the state is wrong for the request — the node is not running, the stack is already deployed |

Errors are always `{"error": "…"}`, and the message is written to be read by a person.

## Ownership

Nearly every endpoint is scoped to a stack, and a stack belongs to the account that
created it. You can reach your own stacks; an administrator can reach any. This is
the same check the UI is subject to, applied in the same place, so there is no way for
a token to see more than its owner could by clicking.

## Worked example: deploy a stack and wait for it

```sh
API=http://localhost:8080
AUTH="Authorization: Bearer $DBCANVAS_TOKEN"

# Start from a shipped template
TPL=$(curl -s -H "$AUTH" $API/api/templates | jq -r '.[] | select(.name|test("PXC")) | .id' | head -1)
DESIGN=$(curl -s -H "$AUTH" $API/api/templates/$TPL | jq '.design')

# Create it, with a lifetime, and deploy
ID=$(curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"name\":\"api-demo\",\"ttl\":\"4h\",\"design\":$DESIGN}" \
  $API/api/stacks | jq -r .id)

curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/validate | jq .
curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/deploy

# Deploy returns immediately; the node states are the progress
until [ "$(curl -s -H "$AUTH" $API/api/stacks/$ID |
           jq '[.deployments[] | select(.state != "running")] | length')" = 0 ]; do
  sleep 5
done

curl -s -H "$AUTH" -X POST $API/api/stacks/$ID/destroy
```

`dbcanvas stack deploy api-demo --wait` is the same thing in one line — see
[`dbcanvas-cli`](CLI.md).

---

## Adding an endpoint

For anyone working on DBCanvas: routes live in one table, `app/api_routes.go`. Add a
row with its group and a one-sentence summary, and it is registered, catalogued, in
the OpenAPI document and scope-checked. Leave the summary out and
`TestEveryRouteIsDocumented` fails, which is the point — the reference cannot fall
behind the code, because the code does not build without it.
