# Database Explorer

The **Database Explorer** is a browser-based database client built into DBCanvas. It
browses the schemas, collections and keys of the databases *this installation
deployed*, runs SQL, MongoDB queries and Valkey commands against them, and shows the
results as tables, documents or charts.

Open it from the sidebar (**Database Explorer**) or at `#database-explorer`.

The thing that makes it different from a database client you would install is that you
never tell it anything. It already knows: DBCanvas provisioned every node on your
canvas, so it holds the address, the port, the account and the password, and it reaches
them over the stack's own Docker network. There is no connection dialog, nothing to
copy out of a node panel, and no port has to be published to your host.

> [!IMPORTANT]
> This is a lab tool pointed at lab databases. It will happily let you `DROP` a table,
> because that is what a lab is for. Do not point DBCanvas at anything you care about.

## What it can reach

Everything running in a stack you may access — your own stacks, or every stack if you
are an administrator.

| Family | Covered |
| --- | --- |
| **MySQL** | Percona Server, Percona XtraDB Cluster, MySQL Community, MariaDB, Group Replication members, and the proxies in front of them (HAProxy's write and read ports, ProxySQL's client port, MySQL Router's 6446/6447) |
| **PostgreSQL** | Percona Distribution for PostgreSQL, Patroni, repmgr, Spock, and HAProxy in front of a cluster |
| **MongoDB** | a standalone, a replica set as a whole, each member individually, and a sharded cluster through `mongos` |
| **Valkey** | a standalone, and a cluster seeded from its first shard |
| **ClickHouse** | PMM Server's Query Analytics store today; a provisioned ClickHouse node when DBCanvas grows one |
| **All-in-One** | each database instance inside the node, separately |
| **Kubernetes operators** | the databases a Percona, CloudNativePG or Crunchy operator deployed inside a K3D frame — see [Kubernetes clusters](#kubernetes-clusters) |

A *family* is a wire protocol and a catalogue, not a product: Percona Server, PXC,
MariaDB and MySQL Community share one adapter because they share one of each.

## Connection discovery

When the page opens it reads your stacks and their deployments and builds the tree
from what is actually running:

```
Stack
├─ MySQL / PXC
│   ├─ haproxy-01 · write port     ★   the endpoint a client should use
│   ├─ haproxy-01 · read port
│   ├─ pxc-cluster · pxc-01
│   ├─ pxc-cluster · pxc-02
│   └─ pxc-cluster · pxc-03
├─ PostgreSQL
│   ├─ patroni-01 …
├─ MongoDB
│   ├─ rs-01 (replica set)         ★
│   └─ rs-01 · mongo-01
├─ Valkey
└─ PMM Server
    ├─ pmm-01 · PostgreSQL — PMM Internal        (read only)
    └─ pmm-01 · ClickHouse — PMM Query Analytics (read only)
```

★ marks the **logical endpoint** — HAProxy's write port rather than one PXC member,
the replica set rather than one of its members, `mongos` rather than a shard. That is
what an application would connect to, so it sorts first. The individual members are
listed too, because connecting to one on purpose is most of what a lab is for.

The tree expands lazily: a connection fetches its databases when you open it, a
database its schemas or objects, a table its columns. Nothing is loaded recursively,
so a stack with fifty databases costs one request to draw.

### Authorization

The same rule as everywhere else in DBCanvas: you see your own stacks, an
administrator sees all of them. That is enforced on **every** request and not once at
page load — a connection is named by an opaque id, and the server re-reads the stack,
re-checks ownership and re-derives the endpoint from the deployments each time one
arrives. An id that names somebody else's stack does not resolve, and the refusal is
the same sentence as for one that does not exist.

A node that is stopped, destroyed or renamed between two clicks stops resolving on the
next one, rather than being served from a cache.

## SQL

For MySQL, PostgreSQL and ClickHouse you get a SQL editor and a result grid.

| | |
| --- | --- |
| **Run** | `Ctrl`/`Cmd` + `Enter` |
| **Run the selection, or the statement the cursor is in** | `Ctrl`/`Cmd` + `Shift` + `Enter` |
| **Cancel** | the button, while a query is in flight |
| **Format** | whitespace only — keywords start lines, nothing else changes |
| **Explain** | asks the database for a plan |

Several statements can be submitted together; each produces its own result set, and
the Messages tab lists what each one did. Duration, returned rows, affected rows and
any warnings the server raised (`SHOW WARNINGS` on MySQL) are all reported.

**A destructive statement is never run on a keystroke.** A `DROP`, `TRUNCATE`,
`DELETE`, `ALTER`, `GRANT` or `REVOKE` shows you the statement and asks. This is not a
permission check — a lab database is meant to be written to — it is the difference
between running a `DROP` and running one by accident.

### Explain

**Explain** asks for a structured plan: `EXPLAIN (FORMAT JSON)` on PostgreSQL,
`EXPLAIN FORMAT=JSON` on MySQL, `EXPLAIN` on ClickHouse. The plan is shown as
formatted JSON and carried as a payload alongside it, so a richer plan visualiser can
be added later without changing anything that produces one.

`ANALYZE` is deliberately never added. `EXPLAIN ANALYZE` *executes* the statement, and
somebody asking what a `DELETE` would do should not have it happen.

## Browsing a table

Clicking a table opens a tab with five views:

| Tab | What it shows |
| --- | --- |
| **Overview** | row estimate, size, primary key, engine, owner, foreign keys, constraints, triggers |
| **Data** | the first page of rows (`SELECT * FROM … LIMIT 500`) |
| **Columns** | name, type, nullability, key, default, comment |
| **Indexes** | columns, uniqueness, type, size |
| **DDL** | the definition, syntax-highlighted |

The row count on the Overview is an **estimate** — `reltuples` on PostgreSQL,
`TABLE_ROWS` on MySQL, `total_rows` on ClickHouse — and is labelled `~`. Counting
every row of every table to draw a panel is exactly what this feature does not do. Use
**Count Rows** when you want the real number.

PostgreSQL has no `SHOW CREATE TABLE`, so its DDL is reassembled from the catalogue.
It is a faithful description rather than a byte-for-byte replay of the original
statement. MySQL's and ClickHouse's come from the server itself.

## The result grid

The grid is most of what this feature is, so it is worth saying what it does.

- **Large results do not freeze the browser.** Only the visible rows are in the DOM;
  the rest is two spacer elements. Scrolling a 50,000-row result costs the same as
  scrolling a 50-row one.
- **`NULL` is not the empty string.** `NULL` renders as `∅` in muted italics; an empty
  `VARCHAR` renders as nothing. The distinction survives sorting, filtering, copying
  and both exports — in CSV a `NULL` is an empty field and `""` is a quoted one.
- **Binary is shown as binary**: `BINARY[3] 0x000102`, never as mojibake. The full
  value is available base64-encoded from the cell viewer.
- **Wide integers keep every digit.** A MySQL `BIGINT UNSIGNED` or a ClickHouse
  `UInt64` past 2⁵³ would round through JSON, so it travels as text and is still
  sorted numerically.
- **Numbers are right-aligned**, booleans render as ✓ / ✗, timestamps use one format
  everywhere (UTC, RFC 3339 with milliseconds).
- **The database's own type is in the header**, beside the column name.
- Columns **sort** (third click restores the database's order), **resize**, **reorder**
  by dragging, and **hide**.
- **Search within the result**, **copy a cell or a row**, **export CSV or JSON**.
- A value too long for a cell is cut at 220 characters with the whole thing one click
  away in the value viewer, which pretty-prints JSON.

Right-click a cell for: copy cell, copy row, copy column name, view full value, view
as JSON, sort, filter by this value, and — where the table allows it — edit or delete
the row.

### Result limits

Every result is capped. The default is **500 rows**; the picker offers up to 50,000,
and the server clamps anything larger. The cap applies to **every** path, including
clicking a table — there is no route through this feature that fetches a whole table
because something was clicked.

The cap is applied where each engine can apply it honestly:

| Engine | How |
| --- | --- |
| MySQL, PostgreSQL | the driver stops reading at the ceiling |
| PostgreSQL over exec | `LIMIT n+1` inside the wrapper |
| ClickHouse | `max_result_rows`, `result_overflow_mode='break'` and `max_block_size` — the third matters, or the first block is still 65,536 rows on the wire |
| MongoDB | `limit` on a find, a `$limit` stage appended to a pipeline |
| Valkey | `SCAN`/`HSCAN`/`SSCAN` pages, `LRANGE`/`ZRANGE`/`XRANGE` counts |

One row past the ceiling is always requested, so **truncated** is a fact rather than an
inference from a result that happens to be full.

There is also a **query timeout** (30 s by default, 300 s maximum) and **cancellation**:
pressing Cancel cancels the request context, which makes pgx send a `CancelRequest`,
the MySQL driver drop its connection, the MongoDB driver kill its cursor, and the exec
transports kill the client process inside the container.

## MongoDB

MongoDB is not dressed up as SQL. The editor has the shape the operations have.

**Find** — filter, projection, sort, skip, as JSON:

```json
{ "status": "active" }
```

**Aggregate** — a pipeline:

```json
[
  { "$match": { "status": "active" } },
  { "$group": { "_id": "$country", "count": { "$sum": 1 } } }
]
```

Also **count**, **indexes**, **stats**, and **command** (a raw database command, which
is how `serverStatus`, `currentOp` and the rest are reached).

Inputs are **Extended JSON**, so the types MongoDB actually stores can be written:
`{"_id": {"$oid": "507f1f77bcf86cd799439011"}}` names a document by its id, where a
plain-JSON parser would build a nested object matching nothing.

Results arrive in **both** shapes at once:

- **Documents** — a collapsible viewer, nesting intact, `$oid` still an `$oid`.
- **Raw JSON** — the whole result formatted.
- **Table** — top-level fields flattened into columns, so the grid, the chart builder,
  the filter and the exports all work. A nested object or array shows a compact preview
  whose cell opens the real value. The flattening never replaces the documents.

A collection's "columns" are **inferred from a sample** of 100 documents, and are
labelled as such — including when a field is a string in some documents and an int in
others, which on a schemaless database is often the finding.

## Valkey

A purpose-built key browser.

**Discovery uses `SCAN`, never `KEYS`.** `KEYS` walks the whole keyspace in a single
blocking call and stalls every other client while it does — which is precisely the
failure a lab should teach you to avoid rather than commit on your behalf. Typing
`KEYS` into the console gets a refusal that says so. The browser pages through the
keyspace with a cursor, filters server-side with `MATCH` and `TYPE`, and a bare prefix
you type (`users:`) becomes the glob you meant (`users:*`).

Each key shows its **type**, **TTL** and **element count**. Opening one gets the viewer
its type deserves:

| Type | View |
| --- | --- |
| string | the value, pretty-printed when it is JSON |
| hash | field / value table, read with `HSCAN` |
| list | indexed rows |
| set | members, read with `SSCAN` |
| sorted set | member / score table |
| stream | stream ID / field / value table |

The **console** runs arbitrary commands — `GET foo`, `HGETALL customer:100`, `INFO`,
`SLOWLOG GET 20`, `SCAN 0 MATCH users:*` — and renders the reply structurally rather
than dumping protocol: a hash comes back as a field/value table, `INFO` as a
section/key/value table you can filter. Commands that turn the connection into a
stream (`SUBSCRIBE`, `MONITOR`, `PSYNC`) are refused with a reason, because a
request/response API has no way to show one.

Errors keep the token that names them: `WRONGTYPE`, `NOAUTH`, `CROSSSLOT`, and `MOVED`
— which on a cluster also gets a note saying the key lives on another shard.

## Charts

Any result with a number in it can be charted: **bar**, **horizontal bar**, **line**,
**area**, **pie**, **donut**, **scatter** and **histogram**.

The builder is proposed rather than imposed. When a result arrives, the shape of its
columns picks a starting point — `country | count` suggests a bar chart of count by
country, `date | orders | revenue` suggests a line over the date — and that suggestion
is written into the controls where you can see and change it. A result with nothing
numeric stays a table.

You choose the visualisation, the category or X field, the value or Y field, an
optional series, an aggregation (sum, average, min, max, count), a sort and a Top N.

> **Everything the chart builder does is a client-side view of the result — it never
> changes the query.** Grouping, Top N and sorting all happen in the browser over rows
> the database already returned, and whenever any of them is on, the chart header says
> so: *"1,204 rows grouped into 12 by country"*. A bar labelled 4,812 is never silently
> a sum of rows you thought was a value.

Charts are inline SVG with tooltips, a legend, readable axes with round tick values,
responsive sizing, and SVG export. They use the validated categorical palette shared
with the Stalk Summary charts, picked light or dark from the current theme, and series
identity is always carried by the legend and not by colour alone.

## Editing data

Where it is safe, rows can be inserted, edited and deleted.

**The rule: a row is only editable when it can be named exactly.** A primary key
qualifies. A unique index over non-nullable columns qualifies. Anything else does not,
and the Overview says why rather than offering a button that would have to explain
itself afterwards. A predicate assembled from whatever columns happen to be visible is
how a grid silently rewrites every duplicate row, and it is never generated here.

- Values are **bound as parameters**, never interpolated into SQL.
- A column that is not a column of the table is rejected before any SQL exists.
- **The statement is shown before it runs.** Preview first, apply second.
- A **delete** additionally requires a confirmation.
- `NULL` is a checkbox and not an empty text box, because that is the only way the two
  stay distinguishable in a form.

**MongoDB** edits by `_id`: insert a document, replace one, delete one. An update
replaces rather than patches, because that is what editing a document in a viewer
means and a partial `$set` would quietly leave removed fields behind.

**Valkey** edits per type: set a string, set a hash field, push or replace a list
element, add or remove a set member, add a sorted-set member with a score, set or clear
a TTL, delete a key.

**ClickHouse** does not pretend to have OLTP row editing. It has no row identity to
update by, and the Overview says so; change data with an explicit
`ALTER TABLE … UPDATE` / `DELETE` in the editor.

## Kubernetes clusters

> [!WARNING]
> **This part is new and needs more testing.** It has been exercised end to end against
> **Percona PostgreSQL Operator** clusters — discovery, both transports, querying, data
> generation, a Query Runner run and Expose for tools. The **PXC**, **Percona Server**,
> **PSMDB**, **CloudNativePG** and **Crunchy PGO** paths are written from each
> operator's own Service naming and Secret layout, and are covered by unit tests, but
> have **not** been run against a live cluster of each. Expect rough edges there —
> particularly a Service name or a Secret key that a version spells differently — and
> treat a wrong or missing endpoint as a bug to report rather than a limit of the
> design.

A K3D frame is a real k3s cluster with a real operator in it, and the databases that
operator creates are as much part of the stack as the ones DBCanvas provisions itself.
They appear under **Kubernetes operators**, one entry per tier:

```
Kubernetes operators
├─ k3d-01 · pgBouncer      ★  LoadBalancer 172.20.255.238:5432
├─ k3d-01 · primary           ClusterIP — read through its pod
└─ k3d-01 · replicas          ClusterIP — read through its pod
```

Nothing about them is read off the canvas: the Services, the pods and the credentials
are discovered from the cluster itself, because what a cluster has is the operator's
business and changes while the frame stays the same. The result is cached for 20
seconds, so a page left open does not exec into every cluster on the host every few
seconds.

**Credentials come from the operator's own Secrets**, read rather than assumed —
`<cluster>-pguser-postgres` and `<cluster>-pguser-<cluster>` for the PostgreSQL
operators, `<cluster>-secrets` for PXC, PS and PSMDB, and the generated application
Secret for CloudNativePG. A pooler authenticates as the *application* user, because
pgBouncer only knows the roles the operator registered with it; a direct endpoint uses
the administrative one.

### Reaching them

| Service type | How |
| --- | --- |
| **LoadBalancer** | dialled directly. MetalLB's address comes out of the stack's own Docker subnet, so it is reachable exactly like any other node. |
| **NodePort** | dialled at a k3s node container's address on that subnet. |
| **ClusterIP** | no address outside the cluster at all, so the database's own client runs inside one of its pods through `kubectl exec`. |

ClusterIP is the operator default, so the exec route is the common one rather than a
fallback for odd cases. The connection says which it is using, and a ClusterIP
connection says why in plain words instead of appearing to be broken.

A Service with no selector — which is how the PostgreSQL operators publish a primary,
so that a failover moves it — has its pod found through its Endpoints object. That is
exact and needs no knowledge of any operator's labelling.

### Which tools can use them

| Tool | LoadBalancer / NodePort | ClusterIP only |
| --- | --- | --- |
| **Database Explorer** | dialled | `kubectl exec` |
| **Data Generator** | `kubectl exec` | `kubectl exec` |
| **Query Runner** | dialled | — |
| **Benchmark** | dialled | — |

The Query Runner and the Benchmark open many connections and time them. A process per
statement would measure the process, so they list only the endpoints they can really
dial, and an endpoint without an address says what to do about it rather than failing
obscurely.

The Data Generator offers PostgreSQL clusters only, and that is a limit of how it
reaches a database rather than of what it can generate: its MySQL path pipes a password
through `MYSQL_PWD`, and `kubectl exec` carries no environment from the caller — so a
MySQL cluster would need the password on a command line inside the pod, which is not a
trade worth making quietly. MySQL and MongoDB clusters are still reachable from the
Explorer, the Query Runner and the Benchmark.

### Expose for tools

A ClusterIP database can be given an address, from the connection's own entry in the
tree:

> **Expose for tools** — adds a Service *beside* the operator's own, with the same
> selector and the same port, of a type that has an address. LoadBalancer first; a
> NodePort when MetalLB's pool (eight addresses per cluster) is exhausted.

**It never modifies anything the operator owns.** Patching a resource an operator
manages invites it to patch back — reconciliation is its whole job — so the companion
Service is a separate object named `<service>-dbcanvas` and labelled as DBCanvas's.
**Exposed — remove** deletes it again and puts the cluster back exactly as it was;
nothing without that label is ever deleted, so a Service somebody else made cannot be
removed by this even by naming it.

A Service with no selector cannot be mirrored — there is nothing to copy, and guessing
a selector would pin the mirror to whichever pod is primary right now, which is the
very thing the operator's arrangement exists to avoid. Expose a tier that has one (the
pooler, the replicas), or set the Service type in the frame's settings and redeploy.

## PMM Server

A DBCanvas-provisioned **PMM Server** carries two databases that are extremely useful
and normally invisible:

```
PMM Server
├─ PostgreSQL — PMM Internal          pmm-managed's inventory
└─ ClickHouse — PMM Query Analytics   the QAN store
```

**PMM Internal — Read Only.** Both are labelled that way everywhere they appear, and
both carry the warning:

> These databases are used internally by PMM. DBCanvas exposes them for inspection and
> troubleshooting. Modifying PMM internal data may corrupt or break the PMM
> installation.

### PMM PostgreSQL

`pmm-managed`'s own database: the nodes, services, agents and settings PMM believes
exist, plus `grafana` with its dashboards, users and datasources. Browse the databases,
schemas and tables and run `SELECT`s.

Nothing here depends on a Grafana datasource — DBCanvas connects itself.

### PMM ClickHouse

The Query Analytics store. `pmm.metrics` is the big one: every query digest PMM has
collected, its fingerprint, and 269 columns of per-minute metrics. Browse the
databases, tables, columns and engines, and run `SELECT`s.

### How they are reached

Not over the network, because they do not listen on one. On a running
`percona/pmm-server:3` the listeners are:

```
127.0.0.1:5432   postgres
127.0.0.1:8123   clickhouse (HTTP)
127.0.0.1:9000   clickhouse (native)
```

Loopback only, so nothing outside the container can dial them whether or not a port is
published. DBCanvas therefore uses the same Docker exec integration the rest of the app
uses for in-container work, and runs each database's own client inside the PMM
container — asking both for machine-readable output rather than the decorated tables
they print by default:

- **psql** with `\gdesc` for the real column names and PostgreSQL types (which it gets
  from the server's own describe, *without running the query*), then the rows as
  positional JSON arrays. Positional, so two columns called `x` both survive; JSON, so
  `NULL` stays distinct from `""`, a `bigint` keeps every digit, and a `jsonb` column
  arrives nested rather than flattened into a string.
- **ClickHouse's HTTP interface**, reached with `curl` inside the container, asking for
  `JSONCompact` — which carries `meta` (each column's name and real ClickHouse type)
  and `data` as positional arrays.

  HTTP and not the `clickhouse-client` next to it, for a specific reason: on
  `percona/pmm-server:3` that client cannot execute an `INSERT` at all. It fails with
  `Code: 100. Unknown packet 11 from server` — client and server are far enough apart
  in the native protocol that the client does not recognise a packet the server sends
  while inserting. `SELECT` is unaffected, which is why it stayed invisible while these
  connections were read-only. The HTTP interface has none of that coupling and returns
  its errors in the same envelope. The password reaches `curl` through a config file
  written from the environment, never on a command line, because an argv is readable
  by anyone with a shell in that container.

### Read-only, and where it is enforced

Read-only here is enforced **by the databases**, not by a keyword filter:

| | |
| --- | --- |
| **PostgreSQL** | every statement runs inside `BEGIN READ ONLY`. A write is refused by the server with **SQLSTATE 25006** — including one that got past the application's classifier. |
| **ClickHouse** | every query runs with **`readonly=2`**. Writes are refused by the server with **error 164 (READONLY)**, and ClickHouse refuses to modify the `readonly` setting itself in readonly mode, so a session cannot lift its own restriction. |

Both were verified against a running PMM Server rather than assumed.

`readonly=2` and not `readonly=1`: both refuse every write, but `readonly=1` also
refuses *any* settings change — including the result ceiling this adapter needs — so a
read-only connection would be the one that could not cap its own results. `readonly=2`
permits settings but still not that one.

On top of that, DBCanvas refuses the statement before it is sent, which gives a better
message (`REFUSED: DROP is not allowed on a read-only connection`) and is defence in
depth. It is deliberately **not** the control. It rejects `INSERT`, `UPDATE`, `DELETE`,
`ALTER`, `DROP`, `TRUNCATE`, `CREATE`, `GRANT`, `REVOKE`, `SET` and `CALL`; it reads
through leading comments and CTE preambles (`WITH … DELETE` is a delete); and it
refuses several statements in one submission outright, which is the shape every attempt
to smuggle a write past a classifier takes.

### Writing to them anyway

A lab exists to break things in, and *"what does PMM do when its own inventory is
wrong"* is a question you cannot answer from a read-only connection. So the policy can
be lifted — behind two locks, because the same connection that makes that testable is
the one that breaks a PMM installation by accident.

1. **An administrator turns it on**, in *Settings → Writes to internal databases*. It
   is instance-wide, off by default, and read on every request — turning it back off
   takes effect on the next query, not the next restart.
2. **A query tab is armed**, in the Explorer, with the red banner's *Arm writes…*. A
   tab left open from before the setting changed carries no arm, so pressing Run in it
   cannot write.

Neither alone does anything, and the server checks both on every request: the browser
saying `allowWrites` is a *request*, not a permission.

While a tab is armed the banner is red and says so, results are badged
`ran with writes armed`, and the connection behaves like any other writable database —
`BEGIN READ ONLY` is not opened, ClickHouse runs with `readonly=0`, and the statement
filter steps aside. Row editing becomes available on tables that have a usable row
identity, the same rule as everywhere else.

Disarming is one click. Turning the setting off again is one click. Do both when you
are finished: a PMM Server whose inventory you have edited is a PMM Server whose
dashboards may stop making sense.


### Provisioned ClickHouse

PMM's is not the only ClickHouse the adapter supports. It is an ordinary ClickHouse
adapter with two orthogonal settings — a *transport* (HTTP for one that listens on a
network address, exec for one that does not) and a *policy* (read-only or not). A
standalone ClickHouse node added to the canvas later is supported by setting neither,
with nothing about PMM to unpick first.

## Query history and saved queries

Every query you run is recorded, per user: timestamp, connection, database, statement,
duration, row count and whether it worked. Rerun, copy, delete one, or clear the lot.
The most recent 500 are kept.

A history entry names its connection **by id**, so rerunning it goes through the same
authorization as any other request — an entry that outlives its stack simply stops
resolving. **No password is stored, because there is none to store.**

Saved queries keep a name, an engine, the statement and an optional description.

## Moving between tools

The Database Explorer and the [Query Runner](QUERY_RUNNER.md) do different jobs and
neither replaces the other:

| Query Runner | Database Explorer |
| --- | --- |
| one statement, many times, in parallel | many statements, once each |
| concurrency, threads, time limits | browsing, inspecting, visualising |
| processlist gating — "only while an `ALTER` is running" | schema, DDL, documents, keys, charts, CRUD |
| what contention does | what the data says |

**Open in Query Runner** on a SQL editor hands the statement and its target across.

## Security

- **Credentials stay server-side.** No endpoint in this feature accepts a host, a user
  or a password, and none returns one — there is no password field in any response
  shape. Passwords are never logged, never placed in a URL, and never put in frontend
  state.
- **Every request is re-authorized.** A connection id is a token the server
  re-resolves, not a permission. It is validated for shape, matched against the stack's
  *current* endpoints, and checked against the caller's ownership on each request.
- **Result sizes are capped and queries are timed out**, on every path.
- **Cancellation** cancels the request context, which stops the work at the database.
- **Row editing requires a real row identity**, checked server-side as well as in the
  UI.
- **PMM's databases are read-only at the database**, separately from DBCanvas's own
  permissions — and lifting that needs an administrator setting *and* a per-tab arm,
  neither of which the browser can grant itself.

## TLS

DBCanvas reaches a node by its address on the stack's Docker network, because Docker's
embedded DNS does not resolve the Intranet's `*.<domain>` names — every network-dialling
tool in the app does the same. That has a consequence worth stating: the app connects
to an address, and a certificate issued for `pg-01.example.net` does not match one, so
full verification would fail on the hostname for a certificate that is perfectly valid.

So the posture is **chain verification against the stack's own CA, without the hostname
check** — `verify-ca` in PostgreSQL's own vocabulary. A certificate not issued by this
stack's CA is rejected. Nothing here disables verification, globally or otherwise.

It applies to nodes **deployed with a certificate** (`generateCert`). A node deployed
without one is not listening for TLS at all, so demanding it there would not be
stricter — it would fail to connect; those use opportunistic encryption instead. The CA
is read once per stack from the Intranet, the same file `trustIntranetCA` installs.

## Known limitations

- **PostgreSQL DDL is reconstructed** from the catalogue, not replayed. It describes
  the table faithfully; it is not the statement you originally ran.
- **A replica set is dialled through one member** (`directConnection=true`), because
  Docker's embedded DNS cannot resolve the other members' names. The endpoint is
  presented as the set, which is what it is, but driver-side failover is not what is
  happening behind it.
- **A Valkey cluster is browsed one shard at a time.** The keyspace is per-node, so
  each shard's connection browses its own keys; a `MOVED` from the console says which
  shard owns a key.
- **MongoDB column inference is a sample**, not a schema. It reads 100 documents.
- **ClickHouse row estimates** come from `system.tables` and are zero for a table that
  has never been written to.
- **PMM's databases are read-only until an administrator says otherwise**, and even
  then a tab has to be armed for each session of writing.
- **Only the Percona PostgreSQL Operator path has been verified against a live
  cluster** — see the warning above.
- **A Kubernetes database reached by `kubectl exec` is slower** — roughly 100 ms a
  statement against under 1 ms dialled, because each one is a process. Expose it for
  the tools if that matters.
- **Query cancellation is not offered on an exec-reached Kubernetes endpoint**: there
  is no second connection to cancel from.
- **The `EXPLAIN` view is the plan as text or JSON.** A graphical plan is possible on
  top of the payload that is already carried, and is not built yet.
- **One statement at a time on a read-only connection.** Multi-statement submissions
  are refused there even when every statement is a read.
- **Query cancellation is best-effort on the exec transports**: the client process
  inside the container is killed, which the server notices when the connection drops.

## Dependencies

**None were added.** The charts, the result grid, the document viewer and the Valkey
protocol client are all written for this feature; the database drivers
(`go-sql-driver/mysql`, `jackc/pgx`, `go.mongodb.org/mongo-driver` — MIT, MIT and
Apache-2.0) were already in `go.mod` for the Query Runner, the Benchmark and the Data
Generator. DBCanvas is GPL-3.0 and nothing here changes that.
