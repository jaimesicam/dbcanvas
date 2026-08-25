# Core Dump Analyzer

The **Core Dump Analyzer** reads a `mysqld` core dump from a server that is not this one:
its threads, the stack that took the signal, and what each frame was holding — without
installing anything on the machine that crashed.

Open it from the sidebar (**Core Dump Analyzer**) or at `#core-dump`. A Linux Client node
set up for it also has an **Open analyzer** button that opens the page on that node.

## The problem it replaces

You have a core file from production. You cannot debug it there: gdb and a few hundred
megabytes of debug symbols on a production database server is not something anyone will
sign off. So the core and the crashed host's libraries get copied somewhere else and read
with one command:

```
gdb -ex "set solib-search-path /path/to/ldd_files" \
    -ex "set sysroot /path/to/ldd_files" \
    -ex "set pagination 0" -ex "thread apply all bt" -ex "quit" \
    /usr/sbin/mysqld /path/to/corefile/core.pidno
```

That works, and it produces a wall of text. Sixty threads scroll past in one go; a
stack-exhaustion crash is four hundred identical frames with the interesting top and bottom
pushed off screen; and there is nowhere to ask a frame what its arguments were. Worst of
all, **the command cannot tell you when it is lying to you** — point it at the wrong
libraries and it prints a backtrace, point it at the wrong build and it prints a backtrace.

## Getting a node you can analyse on

It is a deploy-time decision, because the two directories are bind-mounted and the debug
symbols are installed while the node is built.

1. Put the core file and the libraries somewhere under **`GDB_MOUNT_ROOT`** on the Docker
   host (see [Where the files go](#where-the-files-go) below).
2. In **Database Stacks**, add a **Linux Client** node and set its **OS to the one the
   crashed server ran**.
3. Tick **Use this client for core-dump analysis**, give it the two directories, and pick
   the product (Percona Server or PXC), the major series and the exact minor version.
4. Deploy. Installing gdb, elfutils and the symbol packages adds a couple of minutes.

The node is a Linux Client in every other respect — its terminal is still a plain shell if
you would rather run gdb by hand.

### Where the files go

Two directories, both mounted **read-only**. An 800 MB core is read where it lies; nothing
is copied into the node.

| On the host | In the node | What goes in it |
| --- | --- | --- |
| the core-dump directory | `/coredumps` | one or more core files |
| the library directory | `/sysroot` | the crashed host's `mysqld`, **plus** everything `ldd $(which mysqld)` printed |

The library directory is the one people get wrong. Collect it on the crashed host with
something like:

```
mkdir -p /tmp/ldd_files
cp /usr/sbin/mysqld /tmp/ldd_files/
ldd /usr/sbin/mysqld | awk '{for (i=1;i<=NF;i++) if ($i ~ /^\//) print $i}' \
  | sort -u | xargs -I{} cp {} /tmp/ldd_files/
```

…then copy that directory to the DBCanvas host. gdb is pointed at it twice over — `sysroot`
for a copied tree, `solib-search-path` for a flat dump, plus every subdirectory of it that
holds a shared object — so either layout works.

**Putting `mysqld` in there matters.** When it is present, that copy is the binary gdb
reads: it is the one that produced the core, byte for byte, and no version guess can be
wrong about it. Without it the analyzer falls back to the package's own binary, and then the
version you picked has to be exactly right.

### Why it still asks for a version

Because a released `mysqld` carries no debug information at all. The copied binary supplies
the code; the packages supply the names, the arguments and the line numbers.

How gdb pairs them is worth knowing, because it is not the way the documentation leads with.
Percona's `mysqld` has **no build-id note** — `readelf -n` shows only `.gnu.build.attributes`
— so the usual `/usr/lib/debug/.build-id/` lookup never applies to it. What it has is a
`.gnu_debuglink` naming `mysqld-8.0.16-7.1.el8.x86_64.debug`, and gdb resolves a debuglink
**relative to the directory the binary is in**. The package puts its debug file where gdb
would look for `/usr/sbin/mysqld`; the binary being read is at `/sysroot/mysqld`. DBCanvas
links the two together at deploy so the mounted copy keeps its symbols.

With no build-id there is also nothing to compare builds *by*, so the deploy compares the
mounted `mysqld` against the package's own byte for byte and says so if they differ.

That is why the node's OS has to match too: an el8 build and an el9 build of one version are
different binaries with different symbols.

The package sets are not guessable, and one of them looks redundant and is not:

| | RHEL family | Debian / Ubuntu |
| --- | --- | --- |
| Percona Server | `percona-server-server`, `percona-server-server-debuginfo`, **`percona-server-debuginfo`** | `percona-server-server`, `percona-server-dbg` |
| PXC | `percona-xtradb-cluster-server`, `percona-xtradb-cluster-server-debuginfo`, **`percona-xtradb-cluster-debuginfo`** | `percona-xtradb-cluster-server`, `percona-xtradb-cluster-dbg` |

`percona-server-debuginfo` ships three files and no symbols of its own: it is the
DWZ-compressed *common* file that `percona-server-server-debuginfo`'s symbols refer into.
Install only one of the pair and gdb has half a symbol table, while `rpm -q` reports that
debuginfo is installed. On Debian, note that `percona-xtradb-cluster-server-debug` is a
debug **build** of the server, not symbols for the release build, and is never what you want.

A fourth package, **`*-debugsource`**, is what turns a stack into an explanation. Debug symbols
give a frame a file and a line; without the source, `item_sum.cc:4115` is a coordinate for a file
you do not have. debugsource ships that code — and installs it to exactly the path the debug
information records, so gdb and the analyzer's source pane find it with no configuration at all.
It is why the page can show you the line that crashed rather than its number.

Debian has no equivalent: its `-dbg` package carries symbols only. A Debian client still resolves
frames to file and line, it just cannot show the line.

The server package is installed but **never started**.

## Using it

**Pick a core.** The left column lists what is in `/coredumps` with its size, the executable
the kernel recorded, and the signal — and then the part the command line has nowhere to put:

- **`different build`** — the binary's build-id is not the core's. Whatever backtrace follows
  is a different program's.
- **`N objects missing`** — that many of the objects the process had mapped are not in the
  library directory. `eu-unstrip` reads the core's own build-id notes to work this out, so it
  is a fact rather than a guess.

**Read the verdict.** The banner is not a summary of the stack, it is a diagnosis — what went
wrong, why, and the facts it was derived from. It reads the *whole* stack, not the window the
pane shows, which is what lets it say things a backtrace cannot:

- **What kind of crash this is.** A null pointer, a copy that ran off the end, runaway recursion
  that exhausted the stack, an assertion the server chose to die on, glibc's own heap-corruption
  check catching a double free or a buffer overrun, an allocation failure. These need completely
  different responses and look identical in the top few frames — the last two both put `abort()`
  on the stack, and reporting a double free as "an assertion failed" sends you looking for a check
  in the server's code that was never written.
- **Which frame is actually the bug.** Never the top one on a MySQL core. The server catches its
  own fatal signals, so frame 0 is always `__pthread_kill_implementation` inside
  `my_write_core` ← `handle_fatal_signal`. gdb marks the real boundary with
  `<signal handler called>`, and the frame below it is where the program actually was. The page
  opens on that frame, not on frame 0.
- **What was in its hands.** A null `this` is the root cause of a whole class of crashes and it
  sits in the argument list, where a pane of function names never shows it. `this = 0x30` is
  reported as what it is: a null object plus a 48-byte field offset, i.e. the same bug read a few
  instructions later.
- **How big the problem is.** "This cycle repeats 212 times, 1,060 of the stack's 1,085 frames" is
  a fact about the crash. "It repeats 39 times" — which is all a 200-frame pane can see — is a
  fact about the pane.
- **What SQL was running.** Read directly out of the crashing thread's own `THD`, not guessed from
  a nearby argument — so it applies to any crash, not only the ones a "trigger" search is built
  for. On a real crash caused by installing a plugin under `innodb_force_recovery`, the verdict
  showed `install plugin audit_log soname 'audit_log.so'` next to the double free it caused.
- **What set it off.** For a runaway, it goes and finds the input: the string argument in the
  frame just below the recursion. On the crash this tool was built against that is a 2,748-byte
  `+(+(+(...` query at frame #1068, which is the single most useful thing in the core and is
  1,068 frames from where anybody would look.

Every conclusion is listed next to the evidence it rests on, and every piece of that evidence is
something you can go and check in the panes below.

**Follow the stack.** The signalled thread is selected and first; every other thread is
labelled by what it was doing rather than by its LWP, so sixty of them are scannable. Frames
in the C runtime are dimmed. A repeating cycle — direct or mutual, up to eight frames long —
is folded into one row with a **×N** badge counting its runs in the *whole* stack, which is
the only way a stack-exhaustion core is readable at all.

**Read the code.** The pane under the stack shows the source of the selected frame, with the
crashing line highlighted — the reason `*-debugsource` is installed. A frame with no `file:line`
(a library, or code built without debug information) says which of the two it is.

**Read the frame.** Selecting a frame shows its arguments and locals. A frame inside a
library has none, and neither does any frame when the executable has no separate debug
symbols — the panel says which of those it is rather than showing an empty list.

**Evaluate** takes a C expression in the selected frame — `node->type`, `*state`,
`cr->name` — re-read whenever you change frames.

**The gdb console** is the escape hatch for anything the panels do not cover:
`info sharedlibrary`, `info registers`, `x/16xb $rsp`, `thread apply all bt`.

**Maximize any panel** with the arrows in its top-right corner, and dock it back with the
same button or <kbd>Esc</kbd>.

## Two things worth knowing

**A core file is a dead process.** Nothing here can run the program's code: there is no
continue, no breakpoints, and an expression that would call a function has nothing to call it
on. That is why this page has no equivalent of the Operator Debugger's *allow function calls*
tick — the risk does not exist.

**gdb is a programmable debugger, though.** `shell` is a root shell on the node, `python` and
`guile` are interpreters, `source` loads a script, and `file` would repoint the session at
something the mount confinement never approved. Those are refused in the console until you
tick **Allow shell commands**, and the session log records it when you do.

## Reaching it, and who can

Access is scoped to a stack you own (an admin sees all), the same gate the web terminal and
the file manager use. That is the right boundary and worth being explicit about: gdb runs
*in* the node, so anyone who can open this can already open a root shell on the same
container.

The bind mounts get their own boundary, because they are the one place in DBCanvas where a
path somebody typed reaches the Docker daemon — and the daemon applies no confinement of its
own. `GDB_MOUNT_ROOT` (`.env`, default `/srv/coredumps`) is that confinement: a path outside
it is refused when the design is validated, before anything is deployed, and both mounts are
always read-only.

Docker also **creates a missing bind source as an empty directory** rather than failing, so a
typo would otherwise produce a node that comes up perfectly and an analyzer that reports an
empty directory, with nothing anywhere to say the path was wrong. The deploy therefore probes
both paths in a throwaway container first and fails with the path in the message.

## What the verdict recognizes

Every one of these was found by reproducing a real, previously-reported crash — not invented —
and checking what the verdict said against what actually caused it:

| Class | What it means | Verified against |
| --- | --- | --- |
| `stack-exhaustion` | A function recursed until the thread ran out of stack. | A boolean-mode FTS query nesting `+(` ~275 deep (PS-5712) — 1,085 real frames, a 5-frame cycle repeating 212 times, the 2,748-byte query recovered from frame #1,068. Also (Percona XtraDB Cluster's first core on this page): `ALTER USER CURRENT_USER() IDENTIFIED BY '...'` with the `audit_log` plugin loaded (PXC-3848) — reporting that PXC forbids `CURRENT_USER()` for a `USER` operation re-enters the same check through its own audit-logging path, a genuine 10-frame cycle 1,022 frames deep. |
| `null-deref` | A method was called on a null (or near-null, i.e. null-plus-field-offset) receiver or argument. | A `temptable::Table` read before it existed, reached through nested `MaterializeIterator`s — `this = 0x30`, one call below a `this = 0x0`. Also: `SELECT ... FROM JSON_TABLE(...)` where the JSON document argument is itself a string concatenated with a correlated subquery (PS-9314) — `QEP_shared_owner::set_idx` called with `this = 0x0`, one frame below `JOIN::get_best_combination`, where the optimizer's join-plan array was never sized for whatever `JSON_TABLE` plus the subquery actually produced. |
| `heap-corruption` | glibc's malloc/free caught its own bookkeeping already wrong — a double free, a free of an unmanaged pointer, or an overrun. Deliberately **not** reported as "an assertion" — no check in the server's own code fired. | Installing the audit_log plugin under `innodb_force_recovery=1` (PS-8797): a partial-init cleanup path frees `audit_log_exclude_accounts`, and normal deinit frees it again — `my_free()` twice on `audit_log.cc:889`, found through `abort → __libc_message → malloc_printerr → _int_free`. |
| `bad-pointer` | A memory fault that is not a stack overflow and not an obvious null — often a stale reference into memory that was freed or moved out from under it. | `SELECT * FROM information_schema.APPLICABLE_ROLES` with `tmp_table_size=51200` (PS-8647): a valid-looking `this` reading a field that is no longer backed by real memory — TempTable's storage was converted to on-disk representation out from under a lingering reference. Also: `GROUP_CONCAT(...) GROUP BY ... WITH ROLLUP` over a `TEXT` column (PS-8328) — the verdict lands in `String::append` at `sql_string.cc:451` with a garbage `m_ptr`/`m_length`, one frame below `dump_leaf_key` (`item_sum.cc`), which is exactly where the upstream window-function framebuffer regression this bug tracked back to hands ROLLUP a corrupted row. A malformed nested `audit_log_filter_set_filter()` definition (PS-10345) landed the same way inside a *vendored* third-party library, not Percona's own code — `rapidjson::GenericValue::DataString` reading a garbage `data` pointer, one frame below `GetString` — confirming the culprit search works on `extra/rapidjson` the same as on `sql/` or `storage/innobase/`. A stale `Item_cache` walked while checking column privileges on a correlated subquery inside a stored procedure (PS-10990, also filed as PXC-4794 and MySQL#115885) is a genuine use-after-free rather than an invented one — chased live for several minutes of concurrent stored-procedure calls and DDL and not reproduced (the original reports needed real production data and called it "not consistently reproducible" even then), so this one is verified by unit test against the bug's own real, published backtrace. Two more, both `INSERT` into a `ROCKSDB`-engine table with a unique key and a `TTL` comment (PS-8273, PS-9666), land in `myrocks::rdb_should_hide_ttl_rec` inside `ha_rocksdb.so` — the first crashes this tool has read from a plugin rather than `mysqld` itself, and the deploy does not install debug symbols for a storage engine nobody told it about, so the culprit resolves by name only, from the plugin's own export table. Rather than silently show a culprit with no source line and let a reader wonder whether the search failed, the verdict says why: no debug symbols for this plugin, not a gap in the search. A fifth, `SET GLOBAL binlog_transaction_dependency_tracking = ...` under concurrent write load (PS-9719), is a heap-use-after-free the bug's own ASAN report already names — but the fault lands eight frames deep inside libstdc++'s own `std::unordered_map`/`std::equal_to` template, compiled straight into `mysqld` for `Writeset_trx_dependency_tracker`'s own key type, with no shared-object `From` for the system-library filter to catch — the same gap `gdbFirstOwnFrame` had for `std::basic_string` on PS-9668's core, just in `gdbFaultFrame` this time. The real culprit, `Writeset_trx_dependency_tracker::get_dependency`, is nine frames below the top. |
| `assertion` | The server's own check failed and it chose to abort — the condition is read from the assertion helper's own arguments (`ut_dbg_assertion_failed(expr, file, line)` or glibc's `__assert_fail`), not guessed. | Confirmed against the published stack and source for `Assertion failure: log0pfs.cc:263:m_position == m_rows_n + 1` (PS-8877, `performance_schema.innodb_redo_log_files` racing a redo-log checkpoint) — the trigger is timing-sensitive and was not reproduced live in the time available, so this row is verified by unit test against the real backtrace and source rather than a live core. Reproduced live: `MATCH(...) AGAINST('...\0...')` against an `ngram`-parsed FULLTEXT index (PS-7958) — `ut_a(arg3)` at `eval0eval.cc:130`, condition read as `arg3` from the assertion helper's own 4-character argument; concurrent `OPTIMIZE TABLE`/`UPDATE` against an FTS-indexed table under `innodb_optimize_fulltext_only=1` (PS-7538) — `ib_vector_size(optim->words) > 0` at `fts0opt.cc:1646`; and `SELECT * FROM information_schema.GLOBAL_TEMPORARY_TABLES` racing a concurrent `ALTER TABLE ADD/DROP COLUMN` on a partitioned table (PS-9159) — `table2 == nullptr` at `dict0dict.cc:1229`. Also: `ALTER TABLE ... ADD COLUMN` on a `ROCKSDB`-engine table under `rocksdb_write_disable_wal=ON` (PS-7883) — MyRocks's own `rdb_handle_io_error` chose to abort deliberately when the commit path tried a sync write with the WAL disabled, culprit resolved (and its missing source line explained) inside `ha_rocksdb.so` the same way PS-8273's and PS-9666's were. |
| `exception` | An uncaught C++ exception reached `std::terminate`, which calls `abort()` — not a check the server wrote, and checked *before* `assertion` for the same reason `heap-corruption` is: `abort()` is on this stack too. The exception type comes from the specific `std::__throw_*` helper on the stack when there is one; when `__cxa_throw` never appears at all, the wording changes — nothing was thrown, `std::terminate()` was called directly. | Enabling the `audit_log_filter` component and running `LOCK TABLES FOR BACKUP` (what `xtrabackup` does to start a hot backup) (PS-9668) — `std::logic_error` ("basic_string: construction from null"), culprit `LogRecordFormatter::apply` at `new.cc:204`; and pointing `audit_log_filter.file` at a directory that does not exist (PS-9828) — `std::filesystem::filesystem_error`, culprit `FileHandle::get_not_rotated_file_path` at `file_handle.cc:109`. Both also confirmed a second bug: the culprit search had no rule for a standard-library template (`basic_string`'s own constructor) compiled straight into `mysqld`, and reported the STL doing what it was told instead of the Percona code that told it. A third case, `authentication_policy=INVALID_VALUE` aborting startup before component deinit runs (PS-11273), is the *other* shape: no throw anywhere on the stack, because a still-joinable `std::thread` calls `std::terminate()` directly from its own destructor — the server's error log says so verbatim ("terminate called without an active exception"), and the verdict now says so too instead of claiming something was thrown. |

Not every reported crash is reproducible this way. PS-8291 (`ALTER ... ALGORITHM=INSTANT`
add/drop cycling hitting `dd_column_is_dropped(old_col)` at `dict0dd.cc:1685`) turned out, on
inspection of its own JIRA thread, to assert only on a **debug build** — `ut_ad`, not `ut_a`, so
the check compiles out entirely on the release packages this tool installs. Repeating its exact
DDL sequence against a real node produced no crash and no error, which is the expected (if
unhelpful) result, not a tool failure: there is nothing a release-build core can show for a check
that never ran. PS-8303 (`dict0mem.h:2498:pos < n_def`, also an instant add/drop dictionary
assertion) is the same family and the same result — its own report ran a `-debug` build too, and a
release node cycling the same `ADD`/`DROP COLUMN ... ALGORITHM=INSTANT` sequence, including a
restart to force a full dictionary reload, produced nothing. PS-8428 (`ALTER TABLE ... ADD
FULLTEXT` under `innodb_encrypt_online_alter_logs=ON`) is a different kind of environmental gap:
its root cause, per its own JIRA thread, is a behavior difference in `EVP_CIPHER_CTX_buf_noconst()`
specific to binaries built against OpenSSL 3.0.x — and every Percona Server package this tool has
installed, across every version tried, links `libssl.so.1.1`/`libcrypto.so.1.1`, not
`libssl.so.3`. Run against a real node (including with 50,000 rows, to force the actual
file-based, multi-threaded encrypted merge-sort the bug lives in, not just an in-memory one), it
did not crash — consistent with never running against the OpenSSL build the bug depends on.
PS-9117 (`SET @@SESSION.innodb_interpreter='init'`) is the cleanest case yet — the variable itself
does not exist on a release build (`ERROR 1193: Unknown system variable`), exactly as its own
report says: "only debug build is affected." PS-9083 (slow query log crash with
`log_slow_verbosity` including `query_info` and `long_query_time=0`) and PS-10210
(`rocksdb_debug_cardinality_multiplier=0`) are different: both plausibly still affect a release
build — nothing in either report says otherwise — but neither reproduced despite a real attempt
(PS-9083: single and concurrent connect/query/disconnect cycles, a heavier concurrent DML
workload, and explicit `mysqladmin ping` after a query, none of it over several hundred cycles;
PS-10210: `ANALYZE TABLE`, index range scans, and a restart on a `ROCKSDB` table, which did once
produce a visibly corrupted cardinality value — `-9223372036854775808`, i.e. `INT64_MIN` — without
an actual crash). PS-10210's own report carries no backtrace at all to fall back on verifying by
unit test, unlike every other unreproduced bug on this page; it is recorded here as attempted and
inconclusive, not resolved either way.

Three more join the debug-only family, one of them proven rather than assumed. PS-7856 (`assert()`
in `Field_long::val_int()` when a partitioned table is updated with binary logging disabled) —
several UPDATE shapes against partitioned, auto-incrementing tables produced nothing, and rather
than leave it at "did not reproduce," `nm -D mysqld | grep __assert_fail` on the installed release
binary settles it directly: **zero** references. Every plain `assert()` in the SQL layer is
compiled out of a release build entirely — this check cannot fire no matter what SQL is sent,
because the instruction that would fire it does not exist in the binary. PS-10227
(`rocksdb_table_stats_skip_system_cf` aborting on restart via a `safe_mutex` check in
`my_mutex_lock`) says so directly in its own report — `Server Version: 8.0.43-34-debug` — without
needing to be run at all; `safe_mutex` is the same MySQL debug-build-only mutex instrumentation
behind PS-8291's and PS-8303's family. PS-11143 (Thread Pool clashing with Performance Schema
instrumentation) is a different kind of gap again: `thread_pool.so` is not in Percona's public
RHEL/Debian repositories at all, under any name, for either the 8.0 or 8.4 series — confirmed with
`dnf provides '*/thread_pool.so'` returning no matches — so there is no way to install the
component this bug needs, not a question of the right SQL or the right build flavor.

## Percona XtraDB Cluster

The analyzer reads a PXC core exactly the way it reads a plain Percona Server one — pick `pxc`
as the product, the same `sysroot`/debug-symbol machinery applies — but a PXC bug often needs
Galera's cluster mechanics to fire at all, not just the right SQL, so getting a core is a
different exercise. Two things learned building the first PXC-targeted reproductions:

**A cluster of one still runs the applier.** PXC-3848 (`ALTER USER CURRENT_USER() IDENTIFIED BY
'...'` crashing the connected node — the `stack-exhaustion` example above) needs no second node:
Galera's own machinery is present the moment a node bootstraps, whether or not anything ever
joins it. Bugs that fire in code every node runs regardless of cluster size — as opposed to code
that only runs during certification, conflict resolution, or state transfer between members — are
reachable on a single bootstrapped `pxc` node, which is far cheaper to stand up than a real
cluster.

**Not every real fault produces a core.** PXC-4341 (`PREPARE` a `CREATE TABLE`, `FLUSH TABLES`,
`EXECUTE` — same statement, same connection, twice) reproduced cleanly on a 3-node cluster,
right down to the exact error text from its own report (`WSREP has not yet prepared node for
application use`) — but the node that lost the resulting consistency vote self-isolated
(`wsrep_local_state_comment` → `Inconsistent`, `wsrep_cluster_size` → `0`) rather than aborting
its `mysqld` process. No crash means no core, which means nothing for this tool to read: Galera
detected the problem and evicted the node instead of letting it fall over, which is precisely the
distinction between a bug that needs *this* tool and one that only shows up as a cluster-topology
symptom — a disconnected member, an unexpected SST — with nothing to attach a debugger to. PXC-2500
(a two-node `ALTER USER ... REPLACE` reproduction, published against an early 8.0 build) is a
plainer case of the same non-finding: run against 8.0.26-16.1, both the exact statement from the
bug's own report and a same-shape variant completed cleanly on every member, with no eviction and
no crash — consistent with an old, low-numbered bug already fixed long before this version, the
same shape as PS-8291 and PS-8504's already-fixed findings above.

### The eviction pattern, confirmed six times over

Handed a much larger PXC catalog split explicitly into "crashes" and "evictions," the eviction
half confirmed the finding above is not a one-off: **six** different triggers — a `GRANT` for a
user that does not exist (PXC-4284), `SET PASSWORD` containing an unescaped `'` (PXC-4965), a
`CHECK CONSTRAINT` whose own creation violates it (PXC-4336), an `ALTER TABLE` on a table that
does not exist, but only when run *inside a stored procedure* (PXC-4683, matching its own report's
oddly specific precondition exactly), a `CREATE USER` that satisfies `authentication_policy` on
the node that ran it but not on the node applying it — evicting a *different* node than the one
that issued the statement, with no error at all on the initiator (PXC-4709) — and PXC-4341 from
the previous session, all reproduced cleanly and every one of them ended the same way: the losing
node self-isolates (`Inconsistent`, cluster size 0), the rest of the cluster stays a healthy
majority, and no `mysqld` process ever aborts. This is Galera doing exactly what it is designed to
do — a consistency vote deciding who is right, and the loser leaving rather than corrupting
itself — and it is worth stating plainly: **this shape of PXC bug is out of scope for a core-dump
analyzer by construction**, not by a gap in this one. The right tool for a disconnected member or
an unexpected SST is the cluster's own status variables and error log, which this project has no
plans to duplicate.

A handful of attempted eviction triggers did *not* reproduce at the versions tried — `ALTER
DEFINER VIEW` with insufficient privilege (PXC-4268, report was 8.0.32, tried at 8.0.34 —
plausibly already fixed), `CREATE FUNCTION`/`CREATE TRIGGER` without `SUPER` (PXC-4362, PXC-4765),
a backup-lock-plus-`FLUSH TABLES WITH READ LOCK` deadlock (PXC-4799), and concurrent `CREATE USER
IF NOT EXISTS` across three nodes (PXC-4012, which likely needs tighter same-instant timing than
three sequential `docker exec` calls can deliver) — recorded honestly as attempted-not-reproduced
rather than pursued further, since the mechanism this category exercises was already confirmed six
times over and a seventh or eighth confirmation would not teach the tool anything new.

### The crash half of the same catalog: mostly the same non-reproducibility reasons as before

Of seventeen more candidates from the "crashes" list, one reproduced with nothing new to fix
(PXC-3848, above); four are confirmed debug-build-only the same way earlier sessions' PS bugs
were — PXC-667 needs `SET DEBUG_SYNC`, which does not exist as a variable on a release build
(`ERROR 1193: Unknown system variable`); PXC-4033 and PXC-4403 both assert via a plain C++
`assert()`, and `nm -D mysqld | grep __assert_fail` again returns nothing; two more (PXC-2500,
already covered; PXC-4849, an event-scheduler-plus-`read_only` SST failure reproduced step for
step including a real full SST, but the event loaded cleanly) are already-fixed at the versions
tried. Four have no information in their own JIRA record at all to build a reproduction from
(PXC-4278, PXC-4340, PXC-5099, PXC-5209) and two more are effectively the same (PXC-3608's FK
repro steps are referenced but not included; PXC-3184 is, on inspection, not a crash at all —
`mysqld` exits cleanly and on purpose when SST's prerequisites are missing, so there was never
going to be a core regardless of how carefully it was run). The rest — PXC-3442, PXC-3936,
PXC-4211, PXC-4217, PXC-4348 — got real, specific attempts (2-node and 3-node clusters, the exact
config and SQL from each report, concurrent conflicting load where the report called for it) and
did not reproduce at the versions tried; unlike the eviction half, there is no single unifying
mechanism to point to here, just five bugs that needed a more specific timing window or an older
build than the ones tried.

Three things generalize across all of them, not just the crash they were found on:

- **What SQL was running** — read straight out of the crashing thread's own `THD.m_query_string`,
  not guessed from a nearby argument. It found the exact query on two unrelated crash classes on
  the first try (the FTS query, and the plugin install command).
- **Where the program actually was** — every one of these crashes is caught by the server's own
  `handle_fatal_signal`, so frame 0 is always the handler. The verdict follows gdb's
  `<signal handler called>` marker to the real fault frame instead.
- **The source, highlighted** — `*-debugsource` installs to exactly the path the debug
  information records, so the panel shows the crashing line itself, not just its coordinates —
  and this held even for a dynamically-loaded plugin's own `.cc` file (`audit_log.cc`), which
  `ldd` would never have found on its own.

## Only Percona MySQL, for now

The plumbing — the GDB/MI client, the session, the collapsing, the page — knows nothing about
which program crashed. What is per-product is the debug-symbol package set and the version
catalog, and today that is **Percona Server for MySQL** and **Percona XtraDB Cluster**.

## See also

- [Stacks](STACKS.md) — the Linux Client node and its options
- [Operator Debugger](OPERATOR_DEBUGGER.md) — the same shape, for a live Kubernetes operator
