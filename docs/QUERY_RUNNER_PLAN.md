# Query Runner — Implementation Plan (for review)

Status: **v3 — decisions locked.** Ready to build on your go-ahead. Modeled on
the vibecoded UI screenshot; backend is new (no prior code in this repo).

> Routing: nav route is `#queryrun` (hash-router). Bare-path `/queryrun`
> deep-linking is out of scope (would require moving the app off hash-routing).

---

## 0. Locked decisions

| # | Decision |
| --- | --- |
| Purpose | A **concurrency/orchestration tool**, **not** a benchmark. Distinct feature. |
| Transport | **Option A — native TCP drivers** (`go-sql-driver/mysql`, `jackc/pgx`). Real pooled connections. |
| Targets | **Only canvas-provisioned DB nodes.** No arbitrary host/port. |
| AuthZ | **Admin → all DB nodes; regular users → only their own stacks** (same scope as `ListStacks(u.ID, isAdmin)`). |
| Engines (v1) | **MySQL / PXC** and **PostgreSQL**. |
| COUNT | **Total executions across all THREADS** (shared atomic counter). |
| Regex | **Go RE2** (no backreferences/lookaround). |
| Gate | **Self-excluding** — the processlist watcher ignores the tool's own connections. |
| Docs | Ship a **usage guide** (`docs/QUERY_RUNNER.md`) + README section. |

---

## 1. What this tool is

You define one or more **queries**; each targets a **canvas-provisioned DB node**,
has its own **load parameters** (count / threads / time limit), and its own
**processlist gate**. **All queries start together and run in parallel**, each
watching only its own target's processlist. Past runs land in **History**.

Typical use: "hammer `orders` with SELECTs *only while* an `ALTER TABLE orders`
is running on that server" — concurrency / locking / DDL experiments.

---

## 2. Per-query fields

| Field | Source / behavior |
| --- | --- |
| **SERVER** (was ENGINE/HOST/PORT/USER/PASSWORD) | A **dropdown of the user's running DB nodes** (admin: all). Selecting it resolves engine + in-network host:port + stored admin creds **server-side**. Engine/host/port shown read-only for transparency; **password never sent to the browser**. |
| **DATABASE** | Optional default schema/db (free text, or picker via the existing databases endpoint). |
| **QUERY** | SQL to run (single statement for v1). |
| **COUNT (0=∞)** | Total runs across all threads; `0` = until TIME LIMIT. |
| **THREADS** | Concurrency (each thread = its own pooled connection). |
| **TIME LIMIT (S)** | Wall-clock cap; run ends at whichever of COUNT / TIME LIMIT hits first. |
| **Run condition** ☑ | Enable the processlist gate (off = fire immediately). |
| **PATTERN (REGEX)** | RE2 matched against active statements on the target. |
| **CONDITION** | `No match running` / `A match IS running`. |
| **CHECK** | `Every iteration` / `Once (gate start)`. |
| **POLL (MS)** | Processlist poll interval while the gate is closed. |

Top-level: **Run N queries**, **+ Add another query**, **History**.

> Note vs the screenshot: the manual HOST/PORT/USER/PASSWORD inputs are replaced
> by the SERVER picker because targets are canvas-only and creds must not be
> hand-typed/exposed. If you'd rather keep the literal manual fields (prefilled
> from the picked node, editable), that's a small tweak — say so.

---

## 3. Run condition (processlist gate) semantics

Per gated query, poll the **target's processlist** every `POLL` ms:
- MySQL/PXC: `SHOW FULL PROCESSLIST` → match the `Info` column.
- Postgres: `SELECT pid, query FROM pg_stat_activity WHERE state='active'`.

Apply `PATTERN` (RE2) → *matched?*. **CONDITION** sets when the gate is **open**:
- `No match running` → open when nothing matches (wait while something does).
- `A match IS running` → open when ≥1 matches (wait until one appears).

**CHECK**: `Once (gate start)` polls until open once, then runs the loop ungated;
`Every iteration` re-waits before each iteration. Gate-closed = the query waits
(polling) up to its TIME LIMIT.

**Self-exclusion:** tag the tool's own sessions (a marker comment
`/* dbcanvas-qr:<runId> */` and/or filter by connection id / `pid`) so the gate
never matches the tool's own activity and deadlocks.

---

## 4. Execution model

- On **Run**, all queries launch simultaneously (parallel, independent).
- Per query: a pool of `THREADS` connections; workers loop running `QUERY` until
  a shared atomic counter reaches COUNT (or `0` = unbounded) or TIME LIMIT
  elapses, honoring the gate per CHECK.
- Per-query live stats: executed, errors (+ sample messages), rows affected,
  latency (min/avg/max/p95), gate-wait time, start/end.
- A top-level **Stop** cancels the whole run (`context.CancelFunc`).

---

## 5. Connectivity (Option A specifics)

- **Reach:** the app container is on the stack's Docker network, so it connects
  to a node at its **in-network host (intranet FQDN or container IP) + standard
  port** (3306 / 5432) — **no host port publishing required**. To verify during
  Phase 1: MySQL grants a network account (provisioning creates `admin`@`%`) and
  Postgres `pg_hba` allows TCP from the app's subnet. Use the **`admin`/network
  account**, not `root`/`postgres` (which may be local-only).
- **Drivers:** add `github.com/go-sql-driver/mysql` and `github.com/jackc/pgx/v5`
  to `go.mod` (a deliberate, noted departure from the current stdlib-only
  backend — justified: hand-rolling MySQL/PG wire protocol is not viable).
- **TLS:** nodes may require/offer TLS; start with `sslmode=prefer` / MySQL
  `tls=preferred` and revisit if a node mandates verified certs.

---

## 6. Backend design

New `app/queryrun.go` (handlers) + `app/queryrun_run.go` (worker loop + gate).

| Method + path | Purpose |
| --- | --- |
| `GET /api/queryrun/targets` | The user's running MySQL/PXC + Postgres nodes (owner-scoped; admin = all). |
| `POST /api/queryrun/runs` | Start a run (array of query specs) → `{runId}`. |
| `GET /api/queryrun/runs/{id}` | Poll live per-query status. |
| `POST /api/queryrun/runs/{id}/stop` | Cancel the run. |
| `GET /api/queryrun/history` | List past runs (owner-scoped). |
| `GET /api/queryrun/history/{id}` | One run's detail. |

- **Target resolution:** reuse the node→stack ownership pattern from
  `loadDGNode`; extend `dbConnFor` to also yield **network coordinates + network
  account creds** for TCP (not just the exec container id).
- **Run registry:** mutex map + `newJobID` + per-run `context.CancelFunc`
  (mirrors `dgJobs`).
- **Gate poller:** one poller per distinct target per run (de-duplicated),
  exposing an "open" signal workers await.
- **Persistence:** a `query_runs` table in SQLite (owner id, specs **with creds
  redacted**, per-query results JSON, timestamps) for History.
- Routes register in `app/main.go` beside `/api/datagen/*`; every handler gates
  on `currentUser` + ownership.

---

## 7. Frontend

- Add `#queryrun` to `NAV` in `App.jsx`.
- New `pages/QueryRunner.jsx` + `lib/queryrunApi.js` (clone `datagenApi`'s
  `request` wrapper).
- State: `queries: [{ id, target, database, sql, count, threads, timeLimit,
  gateOn, pattern, condition, check, pollMs, live }]`; **+ Add another query**
  appends, **Remove** deletes.
- Poll the run ~800 ms (Data Generator pattern) for live stats; render a compact
  per-query readout + a **History** list below.
- Reuse `Card`/`Button`/`Badge`/`Field`/`inputCls` from `components/ui.jsx`.

---

## 8. Security / guardrails

- Owner-scoped runs; admins unrestricted, regular users limited to their stacks.
- Server-enforced caps: max queries per run, max THREADS, TIME LIMIT honored
  server-side regardless of client.
- Passwords resolved server-side, never returned to the browser, **redacted in
  History**.
- No arbitrary targets — only resolvable canvas nodes the caller may access.

---

## 9. Deliverables & phasing

1. **Core parallel runner** — targets endpoint, native-driver pools,
   count/threads/time-limit, live stats, stop. (Verify network reachability +
   accounts here.)
2. **Processlist gate** — pattern/condition/check/poll, self-exclusion, shared
   pollers.
3. **History** — persist + list + detail (owner-scoped, redacted).
4. **Docs** — `docs/QUERY_RUNNER.md` usage guide, README "What's inside"
   section, `IMPLEMENTATION.md` session entry.

Files: **add** `app/queryrun.go`, `app/queryrun_run.go`,
`app/web/src/pages/QueryRunner.jsx`, `app/web/src/lib/queryrunApi.js`,
`docs/QUERY_RUNNER.md`; **change** `app/main.go`, `app/store*.go` (History
table), `go.mod`/`go.sum`, `app/web/src/App.jsx`, `README.md`,
`IMPLEMENTATION.md`.

---

## 10. Remaining verification (not blockers — done during Phase 1)

- Confirm MySQL `admin`@`%` (network) creds + Postgres `pg_hba` allow TCP from
  the app container; pick the right account per engine.
- Confirm the node's in-network address the app should dial (intranet FQDN vs
  container IP) and standard ports.
- Confirm processlist visibility for the chosen account (`PROCESS` privilege on
  MySQL; `pg_stat_activity` shows other backends' `query` — may need
  `pg_monitor`/superuser to see all).
