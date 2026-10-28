# Data Generator

The **Data Generator** fills a table or collection in one of your deployed databases with realistic
test data. It reads the table's structure, picks a generator for each column, lets you adjust
them, previews the result, and then inserts as many rows as you ask for.

Engines: **PostgreSQL**, **MySQL** (including PXC, MariaDB and MySQL Community) and **MongoDB**.
Open it from the sidebar (**Data Generator**) or at `#data-generator`.

![The Data Generator with a table chosen and its column template ready](screenshots/data-generator.png)

## Generate data

1. **Connection** — pick a running database node. Connections come from your stacks; you never
   enter a host or password.
2. **Database & table** — pick a database, then a table (or collection). Use the filter box to
   narrow a long list; each entry shows its estimated row count.
3. **Column template** (or **Field template** on MongoDB) — review the generator chosen for each
   column, change any of them, set options, or tick **skip**.
4. **Generate** — set the options below, press **Preview** to see sample rows (nothing is
   written), then **Generate**.

Progress shows rows inserted, rows/s, elapsed time, ETA and error count. **Cancel** stops the job.

Before you generate, a summary line shows the target table, how many columns will be inserted, how
many are skipped or filled by the database, and how many foreign-key columns will be sampled.

## Generate options

| Option | Meaning | Default |
| --- | --- | --- |
| **Total rows** | Rows (or documents) to insert. | 1000 |
| **Batch size** | Rows per INSERT statement (max 20,000). | 1000 |
| **Workers** | Parallel insert workers (1–16). | 4 |
| **FK sample** | How many values to read from each referenced table (SQL only). | 500 |
| **Seed (0=random)** | Fixes the generated values so a run can be repeated. | 0 |
| **On error: stop** | Stop the job at the first failed batch. Off: count the error and continue. | on |

## Column template (SQL)

Each row shows the column name, type, flags (**PK**, **FK→table**, **identity**, **generated**,
**NOT NULL**), a **Generator** list ordered most-relevant first, its options, and **skip**.

**How a default generator is picked** (first match wins):

1. Identity, generated and serial/auto-increment columns → **Use database default** (left out of the
   INSERT so the database fills them).
2. Foreign key → **Foreign key sampler**.
3. pgvector column → **pgvector embedding**, at the column's dimension.
4. Enum column → **Enum-like value**, using the real labels.
5. TimescaleDB hypertable time column → **Time-series timestamp**.
6. Column name: first name, last name, email, username, phone, city, country, company, job title,
   URL, address and full name are recognised from common spellings (`fname`, `surname`, `mobile`,
   `website`, …).
7. Column type: UUID, boolean, integer (bigint for 64-bit), decimal (metric-like names such as
   `cpu` or `latency` get **TimescaleDB metric**), date/timestamp, JSON, IP/MAC address, otherwise
   **Random text**.

Text is trimmed to the column's length and decimals respect its scale.

**Options by generator**

| Generator | Options |
| --- | --- |
| Any, on a nullable column | **null%** — share of NULLs |
| Random integer, Random decimal, TimescaleDB metric | **min**, **max** |
| Sequential integer | **start** |
| pgvector embedding | **dim** |
| Device/sensor ID | **devices** |
| Constant value | the value |

## Foreign keys

A foreign-key column samples real values from the table it references (up to **FK sample** values)
and picks among them. If a NOT NULL foreign key references an empty table, the job stops before
inserting. A nullable one gets NULL and a warning. Only single-column foreign keys are recognised —
fill parent tables first.

## Field template (MongoDB)

DBCanvas samples up to 200 documents to infer each field's BSON type, including embedded documents
and arrays. Unlike the SQL template, it is fully editable: **Add field**, rename, change the BSON
type, or remove a field — so you can build documents for an empty collection.

- BSON types: ObjectId, String, Int32, Int64, Double, Decimal128, Boolean, Date, Timestamp, Binary,
  Regex, JavaScript, Embedded document, Array, MinKey, MaxKey.
- String fields offer the same name-based generators as SQL (names, email, city, …).
- `_id` is skipped by default so MongoDB assigns it. Untick **skip** to generate ObjectIds instead.
- Options: **null%** on every field; **min**/**max** on numeric types; **start** on Sequential
  integer; **bytes** on Binary.
- Preview shows five documents as Extended JSON.

## Special column types

- **pgvector** — `vector`, `halfvec` and `sparsevec` columns are detected with their dimension.
  Values are uniform between **min** and **max** (default −1 to 1).
- **TimescaleDB** — on a hypertable the time column gets evenly spaced timestamps, metric-like
  columns a sine-wave-plus-noise value, and `device_id`-style columns a device id
  (`device-0001`…).

**Supported types**

| Engine | Types |
| --- | --- |
| PostgreSQL | smallint/integer/bigint, serial/bigserial, numeric/decimal/real/double/money, boolean, char/varchar/text, uuid, date/time/timetz/timestamp/timestamptz, json/jsonb, inet/cidr/macaddr/macaddr8, enums, identity and generated columns, pgvector |
| MySQL | tinyint…bigint, decimal/float/double, bit, boolean, char/varchar, text types, date/time/datetime/timestamp/year, json, binary/varbinary/blob types, enum/set, generated and auto-increment columns |

Not generated: PostgreSQL interval, bytea, xml, arrays, range and domain types.

## Targets

| Engine | Nodes offered |
| --- | --- |
| PostgreSQL | PostgreSQL, Patroni, repmgr, Spock |
| MySQL | Percona Server, PXC, InnoDB Cluster / GR, MySQL Community, MariaDB |
| MongoDB | PSMDB standalone, PSMDB RS members, and the **mongos** router of a sharded cluster |

Also offered:

- **All-in-One** database instances, listed per instance.
- **Databases deployed by an operator in a K3D frame**:
  - PostgreSQL — the primary only, whether or not its Service has an outside address.
  - MongoDB — members and mongos routers with a LoadBalancer or NodePort Service (not config
    servers).
  - MySQL operator clusters are not offered. Use the
    [Database Explorer](DATABASE_EXPLORER.md#kubernetes-clusters), Query Runner or Benchmark for them.

Write to the primary. Any PXC or Galera member accepts writes; on async replication, Group
Replication or a MongoDB replica set, pick the primary (a MongoDB secondary rejects the insert).

## API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/datagen/connections` | Nodes you can generate into |
| GET | `/api/datagen/stacks/{id}/nodes/{nid}/databases` | Databases on a node |
| GET | `/api/datagen/stacks/{id}/nodes/{nid}/tables?db=` | Tables or collections, with estimated rows |
| GET | `/api/datagen/stacks/{id}/nodes/{nid}/columns?db=&schema=&table=` | Column template |
| POST | `/api/datagen/stacks/{id}/nodes/{nid}/preview` | Sample rows (no writes) |
| POST | `/api/datagen/stacks/{id}/nodes/{nid}/generate` | Start a job; returns `{jobId}` |
| GET | `/api/datagen/jobs/{job}` | Job progress |
| POST | `/api/datagen/jobs/{job}/cancel` | Cancel a job |

Preview and generate take the same body:

```jsonc
{
  "database": "appdb", "schema": "public", "table": "users",
  "rows": 1000000, "batch": 2000, "threads": 8,
  "seed": 42,                 // 0 = random
  "stopOnError": true,
  "fkSampleSize": 1000,
  "columns": [
    { "name": "id",         "generator": "default" },
    { "name": "org_id",     "generator": "fk" },
    { "name": "email",      "generator": "email",   "options": { "nullPct": 5 } },
    { "name": "age",        "generator": "randint", "options": { "min": 18, "max": 90 } },
    { "name": "embedding",  "generator": "pgvector", "options": { "dim": 1536, "min": -1, "max": 1 } },
    { "name": "full_label", "skip": true }
  ]
}
```

A job snapshot looks like:

```jsonc
{ "id": "…", "status": "running",   // running | done | error | canceled
  "total": 1000000, "inserted": 734000, "errors": 0,
  "rowsPerSec": 48211.5, "elapsedSec": 15.2, "etaSec": 5.5, "message": "" }
```
