# Benchmark

The **Benchmark** tool loads a purpose-built dataset into a database you choose and
drives it with one of four **workload profiles**, reporting throughput + latency. It
targets a **canvas-provisioned** MySQL/PXC or PostgreSQL node and reaches it the same
way the Query Runner does (native TCP over the stack's Docker network). Open it from
the sidebar (**Benchmark**) or at `#benchmark`.

## Workload profiles

| Profile | What it runs | Primary metric |
| --- | --- | --- |
| **OLTP** | Balanced short transactions (~70% read / 30% write): point selects, a range scan, an update, an insert + a delete. | TPS |
| **Read-Write** | Write-heavy transactions: insert order+items, updates, a delete, a couple of reads. | TPS |
| **Read-Only** | Point + range selects only (no writes) — safe against a replica. | QPS |
| **OLAP** | One heavy analytical query per iteration (revenue by category, monthly trend, top customers, AOV by country/segment, product performance). | queries/s + p95 |

## The dataset (schema)

A compact e-commerce **star schema**, all tables prefixed `bench_`:

- `bench_customer`, `bench_product` — dimensions.
- `bench_order`, `bench_order_item` — the order header + line fact tables.
- **Enforced foreign keys**: `bench_order.customer_id → bench_customer`,
  `bench_order_item.order_id → bench_order` (ON DELETE CASCADE),
  `bench_order_item.product_id → bench_product`.

**Scale** sets the size: scale 1 ≈ 10k customers, 1k products, 100k orders, ~250k
order items (~½M rows). Raise it for bigger runs.

## Options

| Option | Meaning |
| --- | --- |
| **Server** | The provisioned node to benchmark (owner-scoped; admins see all). |
| **Database** | Where the `bench_*` tables live (default `dbcanvas_bench`). **create** creates it if missing. |
| **Workload** | OLTP / OLAP / Read-Write / Read-Only. |
| **Scale** | Dataset size factor. |
| **Threads** | Concurrent workers driving the workload. |
| **Duration (s)** | Length of the measured phase. |
| **Warmup (s)** | Unmeasured ramp before recording (default 5). |
| **Keep data after run** | On: the `bench_*` tables are left in place after the run, and — when the **next run also has this on** at the same scale+seed — reused (skipping the load). Off: the dataset is (re)loaded fresh for this run and the `bench_*` tables are dropped afterwards. So reuse needs Keep-data enabled on **both** the run that leaves the data and the run that consumes it; a Keep-data-off run always does a clean load + drop. The database itself is never dropped. |
| **Seed** | Deterministic data + access pattern (0 = random). |

## Lifecycle

**Prepare** (create DB if asked, create schema, bulk-load data) → **Warmup** →
**Measure** (threads × duration) → **Cleanup** (drop `bench_*` unless *Keep data*).
Cancel any time with **Stop**.

## Results

Live headline (TPS or QPS by profile), transaction latency p50/p95/p99, and a
per-statement-type breakdown (count, errors, avg/p95/p99). Finished runs appear under
**History** (this server session).

## Notes & limits

- Only `bench_`-prefixed tables are ever created or dropped, so the tool can share a
  database with other tables safely.
- The run uses the node's admin/superuser account (MySQL `admin@'%'`, Postgres
  superuser). Read-Only / OLAP can point at a replica.
- Caps: scale ≤ 50, threads ≤ 128, duration ≤ 3600 s.
