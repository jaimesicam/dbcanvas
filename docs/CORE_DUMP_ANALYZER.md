# Core Dump Analyzer

The **Core Dump Analyzer** reads a `mysqld` core dump from another machine — Percona Server for
MySQL or Percona XtraDB Cluster — and shows its threads, the crashing stack, each frame's values and
source, and what the server was doing at the time. Nothing is installed on the machine that
crashed; the analysis runs in gdb on a Linux Client in DBCanvas.

Open it from **Menu → Diagnose → Core Dump Analyzer**, at `#core-dump`, or with **Open analyzer** in
the panel of a Linux Client set up for it.

## Set up an analysis node

The node is configured at deploy time: the directories are mounted and debug symbols are installed
while it is built.

1. Copy the core file and the crashed host's libraries to the Docker host, under
   **`GDB_MOUNT_ROOT`** (see [Where the files go](#where-the-files-go)).
2. In **Database Stacks**, add a **Linux Client** and set its OS to the one the crashed server ran.
   An el8 and an el9 build of the same version are different binaries with different symbols.
3. Tick **Use this client for core-dump analysis** and fill in:

   | Field | Value |
   | --- | --- |
   | **Core dump directory (on the Docker host)** | Directory holding one or more core files |
   | **Library directory (on the Docker host)** | Directory holding the crashed `mysqld` and its libraries |
   | **Product** | **Percona Server** or **PXC** |
   | **Major**, **Version** | The exact version that crashed |

4. Deploy. Installing gdb and the debug packages adds a couple of minutes. The server package is
   installed for its symbols but never started.

The node's terminal remains a normal shell if you want to run gdb yourself.

### Where the files go

Both directories are mounted **read-only**; the core is read in place, not copied.

| On the Docker host | In the node | Contents |
| --- | --- | --- |
| Core dump directory | `/coredumps` | One or more core files |
| Library directory | `/sysroot` | The crashed host's `mysqld` plus every library `ldd` lists for it |

Collect the library directory on the crashed host, then copy it to the Docker host:

```
mkdir -p /tmp/ldd_files
cp /usr/sbin/mysqld /tmp/ldd_files/
ldd /usr/sbin/mysqld | awk '{for (i=1;i<=NF;i++) if ($i ~ /^\//) print $i}' \
  | sort -u | xargs -I{} cp {} /tmp/ldd_files/
```

A flat directory or a copied directory tree both work.

**Include `mysqld`.** That exact binary is then used, so the result does not depend on the version
you picked. Without it, the analyzer uses the package's binary, and the version must match exactly.
The version is still needed in either case: a release `mysqld` has no debug information, and the
debug packages for that version supply function names, arguments and line numbers. At deploy,
DBCanvas compares the mounted `mysqld` with the package's and reports a difference.

**Source code:** on Oracle Linux the debug packages include the source, so the page shows the
crashing line. Debian and Ubuntu debug packages carry symbols only: frames resolve to file and line,
but the source pane cannot show the code.

### Limits on the mounted paths

- `GDB_MOUNT_ROOT` (in `.env`, default `/srv/coredumps`) confines both paths. A path outside it is
  refused when the design is validated.
- Before deploying, DBCanvas checks that both paths exist and fails with the path in the message
  if one does not.
- Access follows stack ownership (administrators see all stacks), as for the terminal and file
  manager.

## Using it

1. **Pick a core.** The left column lists each core in `/coredumps` with its size, executable and
   signal. Two warnings mean the backtrace cannot be trusted:
   - **different build** — the binary is not the one that produced the core.
   - **N objects missing** — that many libraries the process had loaded are not in the library
     directory.
2. **Read the verdict** at the top (see [Read the verdict](#read-the-verdict)).
3. **Follow the stack** in the **Stack** tab, then open **Server state** for what the server was
   doing.

Opening a core starts a gdb session on the node; it stays open for three minutes after you leave
the page, so a reload does not reload the core.

### Read the verdict

The banner reads the whole stack and states what went wrong, why, and the evidence it used:

| Class | Meaning |
| --- | --- |
| `stack-exhaustion` | A function recursed until the thread ran out of stack |
| `null-deref` | A method was called on a null or near-null object (for example `this = 0x30`: null plus a 48-byte field offset) |
| `heap-corruption` | glibc's allocator detected its own data already broken — a double free, an invalid free or an overrun. Not reported as an assertion |
| `bad-pointer` | A memory fault that is neither a stack overflow nor a null — often a stale pointer to freed or moved memory |
| `assertion` | The server's own check failed and it aborted; the failed condition, file and line are shown |
| `exception` | An uncaught C++ exception (or a direct `std::terminate()`) ended the process |
| `signal` | The process was killed by a signal that matches none of the above |

The verdict also shows, where available:

- **The culprit frame.** MySQL catches its own fatal signals, so frame 0 is always the signal
  handler. The page opens on the real frame below `<signal handler called>`.
- **The faulting address**, from the core, explained (`0x0` is a null object; an address outside
  the valid range was never a pointer) and matched to the variable that held it.
- **The SQL being run**, read from the crashing thread.
- **The source line** being executed, with the frame's values for the names on it. Values are
  clickable.
- **The size of a recursion** across the whole stack (for example "repeats 212 times, 1,060 of
  1,085 frames"), and the input argument just below it.

If no signal was recorded, the core was probably written by `gcore` rather than a crash, and the
verdict says so.

### Follow the stack

- The signalled thread is first and selected. Other threads are labelled by what they were doing.
- C-runtime frames are dimmed. A repeating cycle of up to eight frames is folded into one row with a
  **×N** count.
- **fold library** (on by default) collapses runs of system-library frames into one expandable band.
- **filter frames** shows only frames whose function matches what you type (folding is off while
  filtering).
- **jump**: **culprit** (the frame the verdict blamed), **fault** (the frame the faulting address
  came from), and **▲ own** / **▼ own** (previous / next frame in the program's own code).
- **This thread** / **All threads**: **All threads** is `thread apply all bt`, with identical stacks
  folded into one row (for example `20×`) and each thread's real depth. Click a thread number to
  open its stack. **Copy** copies every stack as text.
- **full** adds each frame's arguments and locals under the frame, like `bt full`, for the top 40
  frames.
- When **Server state** has been read, a thread waiting on an InnoDB row lock shows **Blocked.**
  with the holding thread, its query and a link to its stack; the holder shows **Blocking N
  threads**. Metadata-lock waits are not linked this way.

### Read the frame and its source

- **Source:** the whole file of the selected frame, scrolled to and highlighting the crashing line.
  A frame without `file:line` says whether it is a library or code without debug information.
- **Frame:** arguments and locals. Click a struct to see its fields, or a pointer to follow it, as
  deep as you like. **+** sends a value's expression to **Evaluate**. A pointer that cannot be
  followed shows gdb's message (`Cannot access memory at address 0x0` is often the diagnosis).
  Pretty-printers are off, so a `std::string` shows its readable text first, then its internals.
- **Evaluate:** a C expression in the selected frame (`node->type`, `*state`), re-read when you
  change frames.
- **gdb console:** any other command — `info sharedlibrary`, `info registers`, `x/16xb $rsp`.
  `shell`, `python`, `guile`, `source`, `file` and similar commands are refused until you tick
  **Allow shell commands**; the session log records it.
- **Maximize** any panel with the arrows in its corner; **Esc** or the same button docks it again.

Opening a C++ value can take tens of seconds the first time, while gdb loads that type's debug
information; it is fast afterwards. A command is given up after five minutes.

A core is a dead process: there is no continue, no breakpoints, and no function calls.

### Run it in your own terminal

Once a core is open, **Run this in your own terminal** (under the verdict) holds the gdb command
line for this core, ready to copy. For example:

```sh
docker exec -it dbcanvas-12-linuxclient-01 gdb -nx \
  -ex "set auto-load off" \
  -ex "set startup-with-shell off" \
  -ex "set pagination off" \
  -ex "set print pretty on" \
  -ex "set backtrace past-main on" \
  -ex "set backtrace past-entry on" \
  -ex "set sysroot /sysroot" \
  -ex "set solib-search-path /sysroot:/sysroot/lib64" \
  -ex "set debug-file-directory /usr/lib/debug" \
  -ex "directory /usr/src/debug/percona-server-8.0.30-22.1.el9.x86_64" \
  -ex "thread apply all bt" \
  /sysroot/mysqld /coredumps/core.1234
```

## See what the server was doing (Server state)

The **Server state** tab reads the server's own structures from the core. The first read takes
tens of seconds; the result is cached, and leaving the tab does not cancel it. Each panel offers
the gdb commands that produce it, with **Copy**.

- **Verdict band:** one line describing the server's condition — a PXC node outside the Primary
  component (red), a lock pile-up naming the transaction everything was queued behind (amber), a
  fault (pointing at the stack verdict), or a healthy snapshot — with supporting counts underneath.
- **Headline tiles:** server version and start time (the crash time is not reliably in a core),
  session count (running vs. idle), binlog file and position, InnoDB history-list length, and redo
  written since the last checkpoint (a large gap means a long crash recovery).
- **Lock waits:** each waiting InnoDB transaction → the transaction blocking it, with each one's
  SQL; below it the full transaction list (id, state, rows modified, thread, operation).
- **Sessions:** every connection, with thread id, account (`user@host`), command, time on that
  command, database, state and query. Idle sessions are dimmed; killed sessions and sessions with an
  error are marked. Expand a row for the last error (errno, SQLSTATE, message), query id,
  transaction state, start time and full query. A running session links to its stack.
- **Error log:** the contents of `performance_schema.error_log`'s buffer, oldest first, coloured by
  priority.
- **Galera / PXC cluster** (PXC cores only): `wsrep_ready`, cluster status (Primary or not), member
  count, node name and address, provider, `gcomm://` address and SST method. Red when the node was
  already outside the Primary component.

Not in a core: Galera's gcache (`galera.cache`, a file mapping the kernel leaves out of core
dumps) and the write-set history. The tab notes this. Use the cluster's status variables and error
log for those.

A PXC node that a cluster evicts after an inconsistency vote leaves the cluster without crashing,
so it produces no core to analyse.

## Supported products

Percona Server for MySQL and Percona XtraDB Cluster.

## See also

- [Stacks](STACKS.md) — the Linux Client node and its options
- [Operator Debugger](OPERATOR_DEBUGGER.md) — a similar page for a live Kubernetes operator
