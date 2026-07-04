# Query Runner

The **Query Runner** runs one or more SQL queries **concurrently**, each against a
**canvas-provisioned database node**, with per-query load parameters and an optional
**processlist "run condition" gate**. It's built for concurrency, locking, and DDL
experiments — e.g. "hammer a table with SELECTs *only while* an `ALTER TABLE` is
running" — not for browsing result sets and not as a benchmark.

Open it from the sidebar (**Query Runner**) or at `#queryrun`.

## What you can target

- **MySQL / PXC** and **PostgreSQL** nodes that are **running** in your stacks.
- Admins see every DB node; regular users see only nodes in **their own** stacks.
- You pick a node from the **Server** dropdown — DBCanvas resolves its in-network
  address and credentials server-side (the app reaches nodes over the shared Docker
  network, so no host port publishing is needed). Passwords are never shown in the
  browser. MySQL uses the network `admin@'%'` account; PostgreSQL uses the superuser.

## Building a run

Each **query card** has:

| Field | Meaning |
| --- | --- |
| **Server** | The provisioned node to run against (each query can pick a different one). |
| **Database** | Optional default database/schema (blank = the engine default). |
| **Query** | The SQL to execute. A single statement per card. |
| **Count (0=∞)** | How many times to run it — **total across all threads**. `0` runs until the time limit. |
| **Threads** | Concurrency for this query (each thread uses its own connection). |
| **Time limit (s)** | Wall-clock cap; the query stops at whichever of Count / Time limit hits first. |

Use **+ Add another query** to stack more cards, then **Run N queries**. **All
queries start together and run in parallel.** Press **Stop** to cancel the whole run.

## Run condition (processlist gate)

Tick **Run condition** to gate a query on what's currently running on *its own
target*:

- **Pattern (regex)** — a [Go RE2](https://github.com/google/re2/wiki/Syntax)
  pattern matched against the target's active statements (MySQL
  `SHOW PROCESSLIST` / Postgres `pg_stat_activity`). Example: `ALTER TABLE\s+orders`.
  (RE2 has no backreferences or lookaround.)
- **Condition**
  - **No match running** — the query fires only while **nothing** matches the pattern.
  - **A match IS running** — the query fires only while **something** matches.
- **Check**
  - **Every iteration** — re-check the gate before **each** execution.
  - **Once (gate start)** — wait for the gate once, then run the whole loop ungated.
- **Poll (ms)** — how often to re-read the processlist while the gate is closed.

The gate **excludes the runner's own statements** (they carry a `dbcanvas-qr`
marker), so a query never blocks on itself. While the gate is closed the query
waits, up to its time limit.

### Example — exercise a query only during a DDL

1. **Query 1** → the DDL: `ALTER TABLE orders ADD COLUMN note TEXT;`, Count `1`.
2. **Query 2** → the probe: `SELECT COUNT(*) FROM orders;`, Threads `4`, Count `0`,
   Run condition on, Pattern `ALTER TABLE\s+orders`, Condition **A match IS running**,
   Check **Every iteration**.

Both start together; Query 2 only fires while Query 1's `ALTER` is active, then
stops when it completes.

## Live stats & history

While a run is active each card shows **executed**, **errors**, latency
(**avg / p95 / max**), and gate state. Finished runs appear under **History**
(kept for the current server session).

## Limits & notes

- Caps: up to **16 queries** per run, **64 threads** per query, **3600 s** time limit.
- Reads/writes/DDL are all allowed — the run uses the node's admin/superuser
  account, consistent with DBCanvas's trusted-host model. Only nodes you may access
  can be targeted.
- Connections use the node's standard port (3306 / 5432) over the Docker network
  with `sslmode=prefer` (Postgres). A node that *mandates* verified TLS may need
  additional configuration.
