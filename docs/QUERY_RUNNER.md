# Query Runner

The **Query Runner** runs several SQL queries at once, each on its own database node, with its own
thread count and an optional gate on what is currently running. Use it for locking, contention
and online-DDL experiments — for example, "run SELECTs only while an `ALTER TABLE` is in progress".
It is not a result browser (use the [Database Explorer](DATABASE_EXPLORER.md)) or a benchmark (use
[Benchmark](BENCHMARK.md)).

Open it from the sidebar (**Query Runner**) or at `#queryrun`.

![The Query Runner — parallel SQL across a stack, gated on the processlist](screenshots/query-runner.png)

## Run queries in parallel

1. Fill in the first query card (fields below).
2. Press **Add another query** for each extra query.
3. Press **Run N queries**. All queries start together. **Stop** cancels the whole run.

| Field | Meaning | Default |
| --- | --- | --- |
| **Server** | The node this query runs on. Each card can pick a different one. | — |
| **Database** | Default database or schema. Blank uses the engine default. | blank |
| **Query** | One SQL statement. | — |
| **Count (0=∞)** | Total executions across all threads. `0` runs until the time limit. | 1 |
| **Threads** | Concurrent connections for this query. | 1 |
| **Time limit (s)** | Wall-clock cap. The query stops at Count or Time limit, whichever comes first. | 60 |

## Gate a query on what is running

Tick **Run condition** on a card to make it wait for a pattern on its own server's active
statements (`SHOW PROCESSLIST` on MySQL, `pg_stat_activity` on PostgreSQL).

| Field | Meaning |
| --- | --- |
| **Pattern (regex)** | A [Go RE2](https://github.com/google/re2/wiki/Syntax) pattern, e.g. `ALTER TABLE\s+orders`. RE2 has no backreferences or lookaround. |
| **Condition** | **No match running** — run only while nothing matches. **A match IS running** — run only while something matches. |
| **Check** | **Every iteration** — check before each execution. **Once (gate start)** — wait once, then run the rest ungated. |
| **Poll (ms)** | How often to re-check while waiting (minimum 100, default 1000). |

A query's gate ignores its own statements, but it does see the other queries in the same run.
While the gate is closed the query waits, up to its time limit.

### Example: probe a table during an ALTER

1. **Query 1:** `ALTER TABLE orders ADD COLUMN note TEXT;` — Count `1`.
2. **Query 2:** `SELECT COUNT(*) FROM orders;` — Threads `4`, Count `0`, **Run condition** on,
   Pattern `ALTER TABLE\s+orders`, Condition **A match IS running**, Check **Every iteration**.

Both start together. Query 2 runs only while the `ALTER` is active.

## Results

While a run is active each card shows executed count, errors, latency (avg / p95 / max) and gate
state. Finished runs are listed under **History** until DBCanvas restarts.

## Targets

- Running MySQL-family nodes (Percona Server, PXC, InnoDB Cluster / GR, MySQL Community, MariaDB)
  and PostgreSQL-family nodes (PostgreSQL, Patroni, repmgr, Spock). MongoDB is not supported —
  use the Benchmark for MongoDB load.
- **All-in-One** SQL instances, listed as `<node> / <instance>`.
- **Databases deployed by an operator in a K3D frame** that have a LoadBalancer or NodePort Service.
  A ClusterIP-only database is not listed; use **Expose for tools** in the
  [Database Explorer](DATABASE_EXPLORER.md#expose-for-tools) to give it an address.

Regular users see nodes in their own stacks; admins see all. You never enter an address or
password: DBCanvas resolves them, and passwords are not sent to the browser.

## Limits and notes

- Up to **16 queries** per run, **64 threads** per query, **3600 s** time limit.
- Queries run as the node's admin account (`admin` on MySQL, the superuser on PostgreSQL), so
  reads, writes and DDL are all allowed.
- PostgreSQL connections use `sslmode=prefer`. A server that requires verified TLS may refuse them.
