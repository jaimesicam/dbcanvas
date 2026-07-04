# Benchmark Tool — Design (for review)

Status: **design draft for review.** Not built yet. Benchmark is a **distinct
feature** from the Query Runner but **reuses its plumbing**: native TCP drivers
(`go-sql-driver/mysql`, `jackc/pgx`), on-demand stack-network join + node-IP dial,
the run registry + live-poll + stop model, owner-scoped canvas targets, and the
frontend page conventions. v1 engines: **MySQL/PXC + PostgreSQL** (same as Query
Runner).

The benchmark loads its **own purpose-built dataset** into a database you choose and
drives it with one of four **workload profiles**, reporting throughput + latency.

---

## 1. Inputs (the run form)

| Field | Meaning |
| --- | --- |
| **Server** | A canvas-provisioned MySQL/PXC or PostgreSQL node (dropdown, owner-scoped; admins see all). Read-only / OLAP profiles can point at a **replica**. |
| **Database** | Where the `bench_*` tables live. Default `dbcanvas_bench`. **Create if missing** toggle (the admin/superuser can `CREATE DATABASE`). |
| **Workload** | **OLTP** · **OLAP** · **Read-Write** · **Read-Only** (see §3). |
| **Scale** | Dataset size factor (see §2). Controls row counts loaded during Prepare. |
| **Threads** | Concurrent workers driving the workload. |
| **Duration (s)** | How long to run the measured phase (or a max-transaction cap). |
| **Warmup (s)** | Optional unmeasured ramp before recording (default 0). |
| **Keep data after run** | If **on**, the `bench_*` tables are left in place (and a later run at the same scale **reuses** them, skipping load). If **off**, they're dropped after the run. |
| **Seed** | Optional — deterministic data + access pattern for repeatable runs. |

Safety: the tool only ever creates/drops tables with the **`bench_` prefix**, so it
can share a database without touching other tables. It never drops the database
itself (even one it created) — only its own tables.

---

## 2. Schema (my design — one dataset, all four workloads)

A compact **star schema** modelling e-commerce sales: two dimensions + an order
header/line fact pair. It gives OLTP point/range access + updates **and** OLAP joins
+ aggregations over a large fact table. IDs are assigned by the loader (no
AUTO_INCREMENT/SERIAL), so the DDL is identical across engines apart from type names.

```
bench_customer   (dimension, ~10k * scale)
  customer_id  BIGINT   PK
  name         VARCHAR(80)
  email        VARCHAR(120)
  city         VARCHAR(60)
  country      VARCHAR(40)
  segment      VARCHAR(20)      -- consumer | corporate | smb
  created_at   TIMESTAMP
  INDEX (country, segment)      INDEX (email)

bench_product    (dimension, ~1k * scale)
  product_id   BIGINT   PK
  name         VARCHAR(120)
  category     VARCHAR(40)
  subcategory  VARCHAR(40)
  price        DECIMAL(10,2)
  cost         DECIMAL(10,2)
  active       SMALLINT
  INDEX (category, subcategory)

bench_order      (fact header, ~100k * scale)
  order_id     BIGINT   PK
  customer_id  BIGINT                     -- → bench_customer
  order_ts     TIMESTAMP
  status       VARCHAR(16)                -- new | paid | shipped | cancelled
  total_amount DECIMAL(12,2)
  item_count   INT
  INDEX (customer_id)   INDEX (order_ts)   INDEX (status)

bench_order_item (fact lines, ~400k * scale)
  item_id      BIGINT   PK
  order_id     BIGINT                     -- → bench_order
  product_id   BIGINT                     -- → bench_product
  quantity     INT
  unit_price   DECIMAL(10,2)
  line_amount  DECIMAL(12,2)
  INDEX (order_id)   INDEX (product_id)
```

Type mapping: `TIMESTAMP` (MySQL) / `TIMESTAMP` (PG); `DECIMAL`/`NUMERIC` are
compatible spellings (DECIMAL works on both). **FK constraints are indexes only, not
enforced** by default (keeps the write path measuring the engine, not constraint
checks) — an "enforce FKs" toggle can come later.

**Scale factor** (SF, default 1), row counts: customers `10,000·SF`, products
`1,000·SF`, orders `100,000·SF`, order_items ≈ `400,000·SF` (1–10 lines/order,
avg ~4). SF 1 ≈ half a million rows total — quick; raise SF for bigger runs.

---

## 3. Workload profiles (statement mixes)

All four hit the same schema; keys are drawn uniformly at random from the loaded id
ranges (a skew option can come later). A **transaction** = the bracketed statement
group, run inside `BEGIN … COMMIT`.

**OLTP** — balanced short transactions (~70% read / 30% write). Primary metric **TPS**.
```
[ 8× point SELECT  (order by id, its items, customer, product by id)
  1× range SELECT  (a customer's recent orders, ORDER BY order_ts LIMIT 20)
  1× UPDATE        (bench_order.status by id)
  1× INSERT+DELETE (add one order+items, delete an old order — keeps row count stable) ]
```

**Read-Write** — write-dominant (~30% read / 70% write). Stresses write path,
replication, locking. Primary metric **TPS**.
```
[ 1× INSERT order + N INSERT items
  2× UPDATE  (order status; a product price)
  1× DELETE  (an old order + its items)
  2× point SELECT ]
```

**Read-Only** — pure reads, point + range (no writes, no heavy aggregation). Safe on
**replicas**. Primary metric **QPS**.
```
[ 10× point SELECT (order/customer/product by id)
  2×  range SELECT (orders by customer; items by order) ]
```

**OLAP** — one heavy analytical query per iteration, chosen at random, over a random
date window. Joins + aggregations + scans. Primary metric **queries/s + per-query p95**.
```
Q1  Revenue by product category      (order_item ⋈ product, GROUP BY category)
Q2  Monthly sales trend              (GROUP BY date_trunc/month(order_ts), SUM)
Q3  Top customers by spend           (order ⋈ customer, GROUP BY customer, ORDER BY sum DESC LIMIT 50)
Q4  Avg order value by country/segment (order ⋈ customer, GROUP BY country, segment)
Q5  Product performance in a window   (order_item ⋈ product ⋈ order, date filter, GROUP BY product)
```
(MySQL vs PG differ only in date functions — `MONTH()`/`DATE_FORMAT` vs
`date_trunc()` — handled per engine.)

---

## 4. Lifecycle

1. **Prepare** — (create database if requested); `DROP` then `CREATE` the `bench_*`
   tables; bulk-load data at the chosen scale (batched multi-row INSERTs across a few
   loader threads, deterministic from `seed`). **Skipped** when *Keep data* is on and a
   matching dataset already exists (checked via a `bench_meta` marker row storing
   scale + seed).
2. **Warmup** (optional) — drive the workload for *warmup* seconds without recording.
3. **Measure** — `threads` workers loop the profile's transaction until *duration*
   elapses (or a max-tx cap). Record per-transaction + per-statement-type latency,
   counts, errors; expose a live TPS/QPS readout (polled like the Query Runner).
4. **Finish/Cleanup** — compute p50/p95/p99, totals, TPS/QPS. If *Keep data* is off,
   `DROP` the `bench_*` tables (database left intact).

Cancellable at any phase (context cancel + Stop button), like the Query Runner.

---

## 5. Metrics & output

- **Headline:** TPS (OLTP/RW) or QPS (RO/OLAP), total transactions/queries, error count,
  wall-clock, rows loaded, scale.
- **Latency:** p50 / p95 / p99 / max (per transaction, and per statement type).
- **Breakdown table:** per statement type (point-select, range-select, update, insert,
  delete, or Q1–Q5) — count, share, avg/p95 latency, errors.
- **Live:** a small TPS-over-time readout while running; **History** of past runs
  (in-memory this-session for v1, like the Query Runner).

---

## 6. Backend design

Shared refactor: extract the Query Runner's connectivity into a reusable helper —
`a.dialNode(ctx, stackID, node) → (driver, ip, creds)` + network-join — used by both
tools (avoids duplicating the stack-network logic).

New `app/benchmark.go` (handlers, DDL, workload SQL, data gen) + `app/benchmark_run.go`
(prepare/measure/cleanup engine + stats), mirroring `queryrun*.go`.

| Method + path | Purpose |
| --- | --- |
| `GET  /api/benchmark/targets` | Owner-scoped MySQL/PG nodes (shared with queryrun). |
| `GET  /api/benchmark/databases/{stackId}/{nodeId}` | List databases (for the Database picker). |
| `POST /api/benchmark/runs` | Start a benchmark → `{runId}`. Body = the §1 inputs. |
| `GET  /api/benchmark/runs/{id}` | Live status: phase, progress, TPS/QPS, latency, breakdown. |
| `POST /api/benchmark/runs/{id}/stop` | Cancel. |
| `GET  /api/benchmark/history` | Past runs (owner-scoped). |

Guardrails: caps on scale, threads, duration; only `bench_`-prefixed DDL; the
database is created but never dropped; owner-scoped like every other tool.

---

## 7. Frontend

New nav entry **Benchmark** (`#benchmark`) + `pages/Benchmark.jsx` +
`lib/benchmarkApi.js`. A single run form (server, database + create toggle, workload
radio, scale, threads, duration, warmup, keep-data checkbox, seed), a **Run/Stop**
control, a live headline (TPS/QPS + latency) with a small sparkline, the per-statement
breakdown table, and a **History** list. Reuses `Card`/`Button`/`Badge`/`Field`.

---

## 8. Open questions / decisions

1. **Workload taxonomy** — I split the four you named into distinct profiles: **OLTP**
   (balanced), **Read-Write** (write-heavy), **Read-Only** (point/range reads), **OLAP**
   (analytics). Is that the split you want, or is it two axes (OLTP vs OLAP) × (RW vs
   RO)? This shapes the UI.
2. **Scale units** — a scale *factor* (SF·row-count table above) vs letting you set
   explicit row counts per table?
3. **Keep-data / cleanup** — confirm: *Keep data off* drops only the `bench_*` tables
   and **never** the database (even one we created). OK?
4. **Database auto-create** — create the DB when missing (needs `CREATE DATABASE`; for
   Postgres this runs against the `postgres` maintenance DB, outside a txn), or require
   it to pre-exist?
5. **FK enforcement** — indexes-only (default, measures the engine) vs enforced FKs
   (measures constraint cost too)?
6. **Metric emphasis** — report both TPS and QPS always, or the profile-primary one?
7. **Replica targeting** — should Read-Only/OLAP be *allowed* to target a secondary
   (read_only node), with writes rejected client-side?
8. **Relationship to Data Generator** — the benchmark ships its own deterministic
   loader (needs referential structure + exact scale). Keep separate from the Data
   Generator, yes?
