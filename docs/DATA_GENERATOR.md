# Data Generator

An embedded feature under **Database Stacks** that generates realistic test data for
tables in databases provisioned by dbcanvas. It targets testing, demos, troubleshooting,
benchmarking, performance validation, and app development.

Engines: **PostgreSQL** (implemented) and **MySQL/PXC** (designed; see roadmap). This
document describes the whole feature; sections tagged _(roadmap)_ are designed but not yet
built. The shipped slice covers the full PostgreSQL workflow end-to-end.

---

## 1. Feature overview

- Reuses the **connection profiles** already known to Database Stacks — the user never
  re-enters host/port/credentials. Every SQL call runs via `docker exec psql` inside the
  node container using the deployment's stored superuser secret, so it works whether or not
  the DB port is published to the host.
- Introspects a chosen table (types, PK/FK, identity/generated, unique, defaults, pgvector
  columns, TimescaleDB hypertables) and builds a **per-column generator template** with a
  smart-inferred default and a combobox of alternatives.
- Generates data with configurable **rows / batch size / workers**, is **foreign-key aware**
  (samples referenced tables), previews rows before inserting, and reports **live progress**.

## 2. User workflow

1. Open **Data Generator** from the left nav (directly below Database Stacks).
2. Pick a **connection** — a running PostgreSQL node (pg / patroni / repmgr) from any stack
   the user owns.
3. Pick a **database**, then browse/filter **schemas + tables** (with estimated row counts).
4. Select a **table** → review the **column template** (type, flags, inferred generator).
5. Adjust generators / options per column; mark columns to skip.
6. Set **total rows, batch size, workers, FK sample size, seed, stop-on-error**.
7. **Preview** sample rows (no writes).
8. **Generate** → live progress (rows, rows/s, elapsed, ETA, errors). Cancel any time.

## 3. Data Generator wizard flow

```
Connection ─▶ Database ─▶ Table ─▶ Column template ─▶ Options ─▶ Preview ─▶ Confirm ─▶ Run ─▶ Progress
   (list)      (list)     (browse)   (per-column)      (rows/…)   (10 rows)  (summary)  (job)   (poll)
```

The confirmation summary (Step 4 card) restates target schema.table, insertable column
count, DB-managed/skipped count, and any FK columns being sampled before the user clicks
Generate.

## 4. PostgreSQL metadata inspection

One JSON-returning query per concern (all via `psql -tAqc`, unmarshalled in Go):

- **Columns**: `attnum`, name, `format_type` (→ `varchar(50)`, `numeric(10,2)`), udt name,
  nullability, default expr, `attidentity` → identity, `attgenerated` → generated column,
  char length, numeric precision/scale, pgvector dimension (parsed from `format_type` for
  `vector`/`halfvec`/`sparsevec`), enum labels (`pg_enum` when `typtype='e'`).
- **Keys**: `pg_index` → primary-key and unique columns.
- **Foreign keys**: `pg_constraint contype='f'` (single-column) → referenced schema/table/col.
- **Hypertable**: `timescaledb_information.dimensions` (Time dimension) → hypertable + time
  column; query error (extension absent) is treated as "not a hypertable".
- **Tables + estimated rows**: `pg_class.reltuples` (internal TimescaleDB/`pg_catalog`
  schemas excluded).

_(roadmap)_ CHECK constraints (`pg_get_constraintdef`), composite FKs, chunk interval /
compression settings, existing time range, index list.

## 5. MySQL/PXC metadata inspection _(roadmap)_

Same shape, sourced from `information_schema` via `docker exec mysql`:
`COLUMNS` (type, nullability, default, `EXTRA` → `auto_increment`/`STORED GENERATED`,
`CHARACTER_MAXIMUM_LENGTH`, `NUMERIC_PRECISION/SCALE`, enum/set members from `COLUMN_TYPE`),
`STATISTICS` (PK/unique), `KEY_COLUMN_USAGE` + `REFERENTIAL_CONSTRAINTS` (FKs),
`TABLES.TABLE_ROWS` (estimate). Writes route to the PXC primary / writer.

## 6. Column generator template design

Per column the UI shows: name, data type, flags (PK / FK→table / identity / generated /
NOT NULL), a **generator combobox** (choices ordered most-relevant first), inline **options**
for the chosen generator, and a **skip** toggle. Request/response shape:

```jsonc
// column (server → UI)
{ "name":"email", "dataType":"text", "udt":"text", "nullable":true,
  "isPrimaryKey":false, "fk":null, "vectorDim":0, "enum":null,
  "generator":"email", "generators":["email","auto","skip","default", "..."] }
```

## 7. Smart generator inference rules

Resolution order (first match wins):

1. **DB-managed** → `Use database default`: generated columns, identity columns, `nextval(`
   defaults (serial/bigserial).
2. **Foreign key** → `Foreign key sampler`.
3. **pgvector column** → `pgvector embedding` (dimension from the type).
4. **Enum type** → `Enum-like value` (uses the actual labels).
5. **Hypertable time column** → `Time-series timestamp`.
6. **Name heuristics** (regex, case-insensitive): `first_name|fname`→first name,
   `last_name|lname`→last name, `email`→email, `user_name|login`→username,
   `phone|mobile|contact_number`→phone, `city`/`country`/`address`/`company`/`job_title`→the
   matching library, `url|website`→URL, `full_name|name`→full name.
7. **Type fallback**: uuid→UUID, bool→boolean, int→random int (bigint→bigint,
   `qty|count`→int), numeric/float→decimal (`price|amount|cost`→money-like,
   `temperature|cpu|latency|usage`→metric), date/time→timestamp/date, json→JSON object,
   inet/cidr/macaddr→IP/MAC, otherwise text (with `status`→enum-word, `device_id`→device id,
   `is_/has_/enabled/active/flag`→boolean).

If nothing infers, a safe **type-aware random** value is used. Values are length-clipped to
`char_max_length` and decimals honor numeric scale.

## 8. Foreign key handling strategy

- FK columns default to **Foreign key sampler**.
- Before a run, each FK column's referenced table is sampled:
  `SELECT quote_nullable(refcol) FROM refschema.reftable WHERE refcol IS NOT NULL
   ORDER BY random() LIMIT <fkSampleSize>`. `quote_nullable` returns ready-to-inject,
  correctly-typed SQL literals; rows draw uniformly from the cached sample.
- If a **non-nullable** FK's referenced table is empty → the job **fails fast** with a clear
  message. Nullable empties → a warning; the column gets `NULL`.
- Supported now: single-column, nullable, and (via ordering by dependency) parent-first
  workflows. _(roadmap)_ composite FKs, self-referencing sampling, periodic sample refresh
  for very large runs, weighted sampling, multi-level dependency ordering.

## 9. Large data generation strategy

- The generate endpoint spawns a job with **N workers** (1–16). A shared, mutex-guarded
  counter hands each worker a **batch** (≤20k); each worker owns its own seeded RNG and FK
  sample view (no shared mutable RNG).
- Each batch is a single **multi-row `INSERT … VALUES (…),(…)`** executed with
  `ON_ERROR_STOP=1`. Progress is tracked with atomic counters.
- **On error**: increment error count; if `stopOnError`, cancel the job; else continue.
- Progress endpoint reports rows generated/inserted, **rows/s**, elapsed, **ETA**, errors,
  status. The job is cancellable.
- _(roadmap)_ `COPY`-stream loading for max throughput, commit-interval / rows-per-transaction
  tuning, prepared inserts, max-error-count, dry-run, generate-SQL-only, pause/resume.

## 10. pgvector support

- Detects `vector`, `halfvec`, `sparsevec` and parses the **dimension** from the type
  (`embedding vector(1536)` → 1536).
- Generates valid literals `'[v1,v2,…]'::vector` (or `::halfvec`) at the detected dimension.
- Options: **dimension**, **min**, **max** (uniform components). _(roadmap)_ normalized /
  clustered / category-based embeddings, sparse vectors, distribution + seed controls.

## 11. TimescaleDB support

- Detects hypertables + the time column via `timescaledb_information`.
- The time column defaults to **Time-series timestamp** (regular interval from a start
  cursor); metric columns default to a **sine-wave + noise** metric generator; `device_id`
  columns to a **device id** generator.
- Options today: metric min/max, interval start/step (internal), device count.
  _(roadmap)_ start/end/interval/jitter, rows-per-device, backfill vs recent, and metric
  patterns (trend, seasonal, spikes, missing, outliers, per-device baseline).

## 12. PostgreSQL & MySQL/PXC type support

**PostgreSQL (implemented):** smallint/integer/bigint, serial/bigserial (DB default),
numeric/decimal/real/double/money, boolean, char/varchar/text, uuid, date/time/timetz/
timestamp/timestamptz, json/jsonb, inet/cidr/macaddr/macaddr8, enums, identity & generated
(DB-managed), pgvector `vector`/`halfvec`/`sparsevec`. _(roadmap)_ interval, bytea, xml,
arrays, range/domain types.

**MySQL/PXC _(roadmap)_:** tinyint…bigint, decimal/float/double, bit, boolean, char/varchar,
text family, date/time/datetime/timestamp/year, json, binary/varbinary/blob family, enum/set,
generated & auto-increment (DB-managed).

DB-managed columns (identity, generated, serial/auto-increment, useful defaults) are omitted
from the INSERT so the database fills them; they can also be explicitly set to
`Use database default` or `Skip`.

## 13. Safety & confirmation behavior

- **Preview** generates 10 rows client-visible with **zero writes**.
- The generate card shows a **summary** (target schema.table, insertable vs skipped/DB-managed
  columns, FK columns to be sampled) before the explicit **Generate** click.
- FK fatal check (empty referenced table for NOT NULL) blocks the run.
- Jobs are **cancellable**; `stopOnError` halts on the first failed batch.
- _(roadmap)_ full pre-run confirmation screen (engine/impact/constraint warnings),
  dry-run, generate-SQL-only, export/import config JSON, richer error log.

## 14. Job progress & error handling

Job snapshot (`GET /api/datagen/jobs/{job}`):

```jsonc
{ "id":"…", "status":"running|done|error|canceled",
  "total":1000000, "inserted":734000, "errors":0,
  "rowsPerSec":48211.5, "elapsedSec":15.2, "etaSec":5.5, "message":"" }
```

Errors are counted per failed batch; the last error message is surfaced. `stopOnError`
cancels the job's context so all workers wind down promptly.

## 15. Example generation configuration structure

Request body for **preview** and **generate** (`POST …/preview`, `POST …/generate`):

```jsonc
{
  "database": "appdb",
  "schema": "public",
  "table": "users",
  "rows": 1000000,
  "batch": 2000,
  "threads": 8,
  "seed": 42,                // 0 = random
  "stopOnError": true,
  "fkSampleSize": 1000,
  "columns": [
    { "name": "id",         "generator": "default" },                       // identity → DB
    { "name": "org_id",     "generator": "fk" },                            // sampled from orgs
    { "name": "first_name", "generator": "firstname" },
    { "name": "email",      "generator": "email",  "options": { "nullPct": 5 } },
    { "name": "age",        "generator": "randint", "options": { "min": 18, "max": 90 } },
    { "name": "status",     "generator": "enum" },                          // uses enum labels
    { "name": "price",      "generator": "decimal", "options": { "min": 0, "max": 9999 } },
    { "name": "embedding",  "generator": "pgvector", "options": { "dim": 1536, "min": -1, "max": 1 } },
    { "name": "created_at", "generator": "timestamp" },
    { "name": "full_label", "skip": true }                                  // generated column
  ]
}
```

## API surface

| Method | Path | Purpose |
|--------|------|---------|
| GET  | `/api/datagen/connections` | running PostgreSQL nodes across the user's stacks |
| GET  | `/api/datagen/stacks/{id}/nodes/{nid}/databases` | list databases |
| GET  | `/api/datagen/stacks/{id}/nodes/{nid}/tables?db=` | list schema.table + est. rows |
| GET  | `/api/datagen/stacks/{id}/nodes/{nid}/columns?db=&schema=&table=` | column template + metadata |
| POST | `/api/datagen/stacks/{id}/nodes/{nid}/preview` | 10 sample rows (no writes) |
| POST | `/api/datagen/stacks/{id}/nodes/{nid}/generate` | start a generation job → `{jobId}` |
| GET  | `/api/datagen/jobs/{job}` | progress snapshot |
| POST | `/api/datagen/jobs/{job}/cancel` | cancel a running job |

## Source map

- `app/datagen.go` — connections, database/table/column introspection, `tableMeta`.
- `app/datagen_gen.go` — generator IDs, inference, per-column value generation.
- `app/datagen_data.go` — realistic-data libraries (names, cities, companies, …).
- `app/datagen_job.go` — config, FK sampling, worker/batch engine, progress, cancel.
- `app/web/src/pages/DataGenerator.jsx` — wizard UI.
- `app/web/src/lib/datagenApi.js` — API wrapper + generator labels.
