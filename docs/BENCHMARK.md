# Benchmark

The **Benchmark** page loads a test dataset into a database and drives it with a workload,
reporting throughput and latency. It can also run a CRUD mix against one of your own tables or
collections. Open it from the sidebar (**Benchmark**) or at `#benchmark`.

![An OLTP benchmark mid-run, with live throughput and per-statement latency](screenshots/benchmark.png)

## Run a benchmark

1. Pick a **Server** — any running MySQL-family, PostgreSQL-family or MongoDB node you can
   access (see [Targets](#targets)).
2. Set the **Database**. Tick **create** to create it if it is missing.
3. Pick a **Workload**.
4. Set **Scale**, **Threads**, **Duration (s)** and **Warmup (s)**.
5. Press **Run benchmark**. **Stop** ends the run early and keeps what was measured.

A run goes through four phases: load the dataset (or reuse it), warm up, measure, clean up.

## Workloads

| Workload | What it runs | Headline metric |
| --- | --- | --- |
| **OLTP** | Short mixed transactions, about 70% reads / 30% writes: point reads, a range read, an update, an insert and a delete. | TPS |
| **OLAP** | One analytical query per iteration: revenue by category, monthly revenue, top customers, average order value by country and segment, top products in a recent window. | QPS |
| **Read-Write** | Write-heavy transactions: an order insert, updates, a delete and a couple of reads. | TPS |
| **Read-Only** | Point and range reads only. Safe against a replica. | QPS |
| **CRUD** | Inserts, updates, deletes and selects against an **existing** table or collection, in the proportions you set. | TPS |

## The dataset

The OLTP, OLAP, Read-Write and Read-Only workloads use a generated e-commerce dataset. Everything
the benchmark creates starts with `bench_`.

- **MySQL and PostgreSQL:** a star schema — `bench_customer`, `bench_product`, `bench_order`,
  `bench_order_item`, plus `bench_meta`. Foreign keys are enforced (order → customer, order item →
  order with `ON DELETE CASCADE`, order item → product).
- **MongoDB:** collections `bench_customers`, `bench_products`, `bench_orders` and `bench_meta`.
  Each order embeds its line items and the customer and product fields the analytical queries need,
  so every aggregation runs on one collection. Operations are single-document (no multi-document
  transactions).

**Scale 1** is about 10,000 customers, 1,000 products, 100,000 orders and about 250,000 order items (one to four per order).

## CRUD on your own table

Choosing **CRUD** replaces the dataset options with:

| Field | Meaning |
| --- | --- |
| **Table** / **Collection** | The existing table or collection to run against. Rows or documents are added and removed. |
| **Operation weights** | Relative weights for insert, update, delete and select (default 25 each). |
| **Filter columns** / **Filter fields** | Update, delete and select match on a random subset of these. Default: the primary key (`_id` on MongoDB). A table with no primary key needs at least one picked. |

CRUD never creates the database, never loads data and never drops your table.

## Options

| Option | Meaning | Default | Limit |
| --- | --- | --- | --- |
| **Server** | The node to benchmark. | — | — |
| **Database** | Where the `bench_*` objects (or your CRUD table) live. **create** creates it if missing. | `dbcanvas_bench` | letters, digits, underscore |
| **Workload** | See [Workloads](#workloads). | OLTP | — |
| **Scale** | Dataset size factor (not shown for CRUD). | 1 | 50 |
| **Threads** | Concurrent workers. | 8 | 128 |
| **Duration (s)** | Length of the measured phase. | 30 | 3600 |
| **Warmup (s)** | Unmeasured run before recording starts. | 5 | 600 |
| **Keep data after run** | See below (not shown for CRUD). | off | — |
| **Seed (0 = random)** | Fixes the generated data and the access pattern. | 0 | — |

**Keep data after run**

- **Off:** the dataset is loaded fresh and the `bench_*` objects are dropped afterwards.
- **On:** the `bench_*` objects are left in place. A later run that also has it on, at the same
  scale and seed, reuses them and skips the load.

The database itself is never dropped.

## Results

While a run is active the page shows the headline metric (TPS or QPS), transaction and query
counts, elapsed time, transaction latency p50/p95/p99, and a per-operation table (count, errors,
avg/p95/p99 ms). Finished runs are listed under **History** until DBCanvas restarts.

## Targets

| Engine | Nodes offered |
| --- | --- |
| MySQL family | Percona Server, PXC, InnoDB Cluster / GR, MySQL Community, MariaDB — standalone or clustered |
| PostgreSQL family | PostgreSQL, Patroni, repmgr, Spock |
| MongoDB | PSMDB standalone, PSMDB RS members, and the **mongos** router of a sharded cluster (config servers and shard members are not offered) |

Also offered:

- **All-in-One instances**, listed as `<node> / <instance>` — see [All in One](ALL_IN_ONE.md).
- **Databases deployed by an operator in a K3D frame**, when they have a LoadBalancer or NodePort
  Service. A ClusterIP-only database is not listed; use **Expose for tools** in the
  [Database Explorer](DATABASE_EXPLORER.md#expose-for-tools) to give it an address.

Regular users see nodes in their own stacks; admins see all.

## Notes

- The run connects as the node's admin account: `admin` on MySQL and MongoDB, the superuser on
  PostgreSQL.
- Only `bench_`-prefixed objects are created or dropped, so the benchmark can share a database
  with other tables.
- On a MongoDB replica set, pick the primary for any workload that writes.
