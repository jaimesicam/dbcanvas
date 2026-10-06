# Support Sim — a vector search support desk, on MongoDB or PostgreSQL

A help desk for **Orbit**, a fictional managed-database company, run on vector search, with
a dashboard that shows why that is useful and teaches how to do it yourself. The same desk runs on
two engines: **MongoDB** with mongot (`$vectorSearch`, `$search`, `$rankFusion`) — DBCanvas's
*Support Sim* node — or **PostgreSQL with pgvector** (`ORDER BY embedding <=> $1`, full-text search,
fusion in SQL) — the *pgvector Support Sim* node. Every page speaks the engine it runs on; the MongoDB
wording is used below, and [PostgreSQL](#postgresql-and-pgvector) lists what differs.

Tickets arrive continuously. Each one is embedded **inside this process** with
`all-MiniLM-L6-v2` (384 dimensions, a pure-Go implementation in `internal/embed`, so no Python,
no cgo and no API key), stored with its vector, and searched with `$vectorSearch`. The desk then acts:

| What it does | How |
| --- | --- |
| **Answers** a ticket on its own | The 5 nearest *resolved* tickets vote on the article that closed them. It sends the answer when the nearest is cosine ≥ 0.70 and ≥ 60% of the vote agrees. A wrong answer is reopened by the "customer". |
| **Routes** it to a team | The 10 nearest resolved tickets vote on their team (k-nearest-neighbour classification). |
| **Merges** a repeat | The same customer's open ticket at cosine ≥ 0.70, found with a `filter: { customer, status, createdAt }` pre-filter. |
| **Raises an incident** | A burst of *new* problems (nothing resolved is ≥ 0.60 close) that resemble each other (≥ 0.40), 4 within 3 minutes. |

Keyword search (`$search`, BM25) and hybrid search (`$rankFusion`) are asked the same questions on
the same tickets, and all of them are **scored against the simulator's ground truth**: every
generated ticket knows which problem it really is. So the dashboard's claims are measured, and they
are honest about where keyword search holds up (comparing tickets with other tickets) and where it
does not (comparing tickets with articles written by someone else).

## The dashboard

- **Live Desk** — the stream, what the desk did with each ticket and why (click one: neighbours,
  votes, thresholds, and every pipeline it ran), the vector-vs-keyword scoreboard, the incident radar.
- **Search Showdown** — one question, three engines, the right answer marked, and a note on which
  lesson that query teaches (including the ones vector search gets wrong). Hybrid is `$rankFusion` or
  `$scoreFusion` (sigmoid / minMaxScaler / none) with a weight slider, and each hybrid hit shows what it
  got from each side (`scoreDetails`). **Tune the blend** sweeps the weight from keyword-only to
  vector-only on 80 unseen tickets, draws accuracy against weight, and can make the live desk use the
  best one.
- **Vector Lab** — seven steps: a sentence as 384 numbers; cosine similarity and its traps
  (negation, shared words); a 2-D PCA map of every ticket; the index definitions with live status;
  a `$vectorSearch` builder with `numCandidates`, filters, `exact`, recall@k measured against
  mongot's exact search, and the same query in mongosh, Python, Node.js and Go; how long after an
  insert a document becomes findable; and the gotchas worth knowing before you build one.
- **Index Workshop** — the choices in an index definition, measured on real indexes over a copy of
  the tickets (`vector_variants`, 2,000 documents): nine variants — cosine / dotProduct / euclidean,
  scalar and binary quantization, BSON binary vectors (float32, int8), and un-normalized vectors with
  cosine and dotProduct — benchmarked for recall against exact search, answer accuracy, latency,
  stored field size and index size; a **lifecycle playground** (create, update, break, drop one index
  while two probe queries run every 0.7 s and the timeline records what mongot reports and what they
  get back); **explain** of `$vectorSearch` (which Lucene query ran, per-segment HNSW visits); and
  **mongot live** from its Prometheus endpoint (latency inside mongot, JVM heap, every index's size,
  documents and change-stream lag).
- **Under the Hood** — the path of a query through mongod and mongot, the indexes, a document.

Every result on every page comes with the exact aggregation pipeline that produced it.

## Running

DBCanvas runs it as the **Support Sim** node, linked to a PS MongoDB replica set or sharded
cluster with **Vector search** on, or to a K3D frame running the PSMDB operator with it on. On
its own:

```sh
make supportsim-image      # from the repository root; downloads the model weights (pinned, checksummed)
docker run -p 8095:8095 -e MONGO_URI='mongodb://user:pw@host:27017/?replicaSet=rs0' dbcanvas-supportsim:latest
```

| Variable | Default | |
| --- | --- | --- |
| `MONGO_URI` | `mongodb://127.0.0.1:27017/?directConnection=true` | needs a user that can create collections and search indexes |
| `MONGO_DB` | `supportsim` | everything lives in this one database |
| `MONGO_TARGET_LABEL` | | shown in the header |
| `PORT` | `8095` | |
| `MODEL_DIR` | `/model` | `model.safetensors` + `vocab.txt` |
| `MONGOT_METRICS` | the server's `mongotHost`, port 9946 | comma-separated `host:9946` list; DBCanvas sets it to every mongot of the cluster |
| `DB_ENGINE` | `mongodb` | `postgres` runs the desk on PostgreSQL + pgvector |
| `POSTGRES_DSN` | | with `DB_ENGINE=postgres`: a libpq URL; the role should be able to `CREATE DATABASE` and `CREATE EXTENSION vector` |

It needs PSMDB 8.3+ with mongot for `$vectorSearch`/`$search`/`$rankFusion`. Against a MongoDB
without mongot it still runs: vector search becomes an in-app brute-force scan and keyword search
becomes the classic `$text` index, and every page says so.

## PostgreSQL and pgvector

`DB_ENGINE=postgres` with `POSTGRES_DSN` runs the desk on PostgreSQL. The app creates its own
`supportsim` database when its role may (otherwise a `supportsim` schema in the database it was given),
then `CREATE EXTENSION IF NOT EXISTS vector`. Tables `kb_articles` and `tickets` hold an
`embedding vector(384)` and a generated `tsvector`; the indexes are `USING hnsw (embedding
vector_cosine_ops)` and `USING gin (tsv)`. Without pgvector the column is `real[]` and the desk scans
in the app, saying so on every page.

| | MongoDB | PostgreSQL |
| --- | --- | --- |
| vector query | `$vectorSearch`, `numCandidates` | `ORDER BY embedding <=> $1 LIMIT k`, `SET LOCAL hnsw.ef_search` |
| filters | declared `filter` fields, pre-filtered | any `WHERE`; `hnsw.iterative_scan = relaxed_order` (pgvector 0.8) so selective filters still fill the LIMIT |
| keyword | `$search` (BM25) | `tsv @@ to_tsquery(…)` ranked by `ts_rank_cd` (the query's words ORed) |
| hybrid | `$rankFusion` / `$scoreFusion` | the same arithmetic in one statement: two CTEs, reciprocal rank or normalized score fusion |
| Lab step 6 | eventually consistent: time from insert to findable | transactional: the writer sees its uncommitted row, other sessions don't, everyone does after COMMIT |
| Workshop step 1 | nine search index definitions | no index; HNSW with `<=>` / `<->` / `<#>`; HNSW m=4; IVFFlat (lists=45); `halfvec`; `binary_quantize` + re-rank; un-normalized vectors with `<#>`. Each `CREATE INDEX` is timed; `hnsw.ef_search` and `ivfflat.probes` are sliders |
| Workshop step 2 | one search index's life | `DROP INDEX` / `CREATE INDEX CONCURRENTLY` (with `pg_stat_progress_create_index`) / `ef_search = 5` under a LIMIT 10 / a 2% filter with iterative scans off, then on / `REINDEX CONCURRENTLY`, probed every 0.7 s on an 8,000-row table |
| Workshop step 3 | mongot's explain | `EXPLAIN (ANALYZE, BUFFERS)`: Index Scan or Seq Scan + Sort, and why |
| Workshop step 4 | mongot's Prometheus metrics | settings, every index's size / scans / cache hit ratio, `pg_stat_statements` |

The desk's own tables are small (hundreds of rows), so the planner usually reads them with a
sequential scan instead of the HNSW index — correct, and explained on the Explain step. The Lab's
query builder sets `enable_seqscan = off` (shown, and called a lab device) so `ef_search` visibly
matters there.

```sh
docker run -p 8095:8095 -e DB_ENGINE=postgres \
  -e POSTGRES_DSN='postgres://postgres:pw@host:5432/postgres' dbcanvas-supportsim:latest
```

## Layout

```
main.go                 config, model load, HTTP server
internal/embed          all-MiniLM-L6-v2 in pure Go (tokenizer, BERT, pooling) — see NOTICE
internal/corpus         teams, 37 KB articles, 39 ticket archetypes and the generator
internal/store          collections, B-tree + search indexes, capability detection
internal/search         pipeline builders, mongosh rendering, mongot / in-app engines
internal/sim            the desk: intake, decisions, lifecycle, metrics, the Lab, hybrid tuning, the Index Workshop;
                        Backend / Searcher / WorkshopBackend interfaces, with mongo_* and pg_* implementations
internal/mongotmetrics  mongot's Prometheus endpoint, parsed and summarized
internal/api            JSON API + Server-Sent Events
web/static              the dashboard (plain JS, no build step)
```

`go test ./...` runs the tokenizer, pipeline and corpus tests; set `SUPPORTSIM_MODEL_DIR` to a
directory holding the model (`sh scripts/fetch-model.sh ./model`) to also run the embedding
reference checks and the corpus accuracy measurement the thresholds were chosen from.
