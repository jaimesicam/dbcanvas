# Operator Debugger

The **Operator Debugger** lets you step through the Kubernetes operator that manages one of your
K3D clusters: set a breakpoint in `Reconcile`, make it reconcile, and read the values it is working
with. It drives [Delve](https://github.com/go-delve/delve) for you, so there is no IDE, source
checkout or Go toolchain to set up.

Open it from the sidebar (**Operator Debugger**) or at `#operator-debugger`. The K3D server node's
panel also has an **Open debugger** button that opens the page on that cluster.

![The Operator Debugger stopped in Reconcile, with the call stack and locals beside the source](screenshots/operator-debugger.png)

## Supported operators

The four Percona operators: **Percona Operator for MySQL (PXC)**, **Percona Operator for MySQL
(Percona Server)**, **Percona Operator for MongoDB (PSMDB)** and **Percona Operator for PostgreSQL
(PGO)**.

CloudNativePG, Crunchy Postgres for Kubernetes and OpenEverest are not supported. The two
community PostgreSQL operators are installed from Helm charts and cannot be debugged. A frame that
asks for a debugger on one of them gets a designer warning and deploys without one.

## Deploy a cluster you can debug

Debugging is chosen at deploy time and cannot be turned on afterwards.

1. In **Database Stacks**, add a **K3D cluster** frame and pick one of the four Percona operators.
2. Tick **Run the operator under Delve**.
3. Deploy. The operator is compiled from its release source with optimisations off, which adds a
   few minutes the first time.

The pod keeps the released image; only its command changes. Probes and leader election are turned
off so a paused operator is not restarted, and Delve starts with `--continue`, so the cluster is
built normally whether or not anyone attaches.

**Also publish the debugger to the host, for an external IDE** is only needed for VS Code or
another editor. It publishes Delve on `127.0.0.1:<Debugger port (host)>` (default 40000), and the
server node's panel then shows the matching `launch.json` and `git clone`. The port is fixed, so two
clusters debugged at once need different ports. The Operator Debugger page does not need it.

## Debug a reconcile

1. Pick the cluster at the top of the page.
2. **Set a breakpoint.** Click a line number in the source, or use a **quick breakpoint** (below).
   An unresolved breakpoint shows a hollow marker and Delve's reason — usually that the line has
   no statement.
3. **Press Force a reconcile.** This annotates the custom resource, so the operator runs
   `Reconcile` now instead of at its next resync.
4. **Read the state** when it stops (see below).
5. **Step** with **Continue**, **Pause**, **Step over**, **Step into** and **Step out**.

**Quick breakpoints**

| Operator | Quick breakpoints |
| --- | --- |
| **MySQL (PXC)** | Reconcile · deploy · smartUpdate · reconcileUsers · Backup Reconcile · Restore Reconcile |
| **MySQL (Percona Server)** | Reconcile · doReconcile · reconcileDatabase · reconcileReplication · smartUpdate · reconcileUsers · Backup Reconcile · Restore Reconcile |
| **MongoDB** | Reconcile · reconcileReplsets · reconcileCluster · smartUpdate · reconcileUsers · Backup Reconcile · Restore Reconcile |
| **PostgreSQL** | Reconcile · PostgresCluster Reconcile · reconcileBackups · Backup Reconcile · Restore Reconcile |

Each shows a one-line hint in the panel. On MySQL (Percona Server), **doReconcile** is where a reconcile pass
actually happens. On PostgreSQL, **PostgresCluster Reconcile** is the loop that turns the
`PostgresCluster` written by `Reconcile` into instances, Patroni and pgBackRest.

## Read the state

| Panel | What it does |
| --- | --- |
| **Call stack** | The operator's frames and its dependencies'. Frames without source on the node are marked *no source*. Click a frame to select it. |
| **Variables** | The selected frame's scope. Values expand on demand. A shortened value has **show all** to read it in full. |
| **Edit a value** | Hover a variable, click the pencil, type a Go literal and press Enter. The new value is written into the running operator and recorded in the session log. Compiler-generated names (such as `~r0`) cannot be edited. |
| **Evaluate** | A Go expression in the selected frame, e.g. `request.NamespacedName`. Each expression is re-evaluated at every stop. |
| **Operator source** | Browse the operator's files. Line numbers match the running binary. |
| **Breakpoints** | Every breakpoint and whether it is armed. **Clear all** removes them. |

Maximize any panel with the arrows in its top-right corner; the same button or <kbd>Esc</kbd>
docks it again. Panels underneath keep their state.

## Safety

**A stopped operator manages nothing.** While it sits at a breakpoint the cluster is not
reconciled at all. To keep that from lasting:

- A banner shows whenever the operator is stopped.
- **Resume if idle for** (2 minutes, **5 minutes** by default, 15 minutes, or never) continues a
  stopped session nobody is touching, and notes it in the session log.
- Closing the page clears the breakpoints and resumes the operator. Your breakpoints are re-armed
  the next time you attach.
- If DBCanvas itself stops mid-session, a watchdog in the operator pod resumes a halted operator
  with no debugger attached, within about ten seconds.

**Expressions can run code.** An expression that calls a method runs it inside the live operator.
Such expressions are refused until you tick **Allow function calls**.

**Who can use it.** You can debug clusters in stacks you own; admins can debug any. A debugger can
read the operator's memory and run its code, so treat access like a root shell on the node.

## See also

- [Stacks](STACKS.md#what-you-can-put-on-the-canvas) — the K3D frame and its options
- [Operator Backups](OPERATOR_BACKUPS.md) and [Kubernetes States](KUBERNETES_STATES.md) — other
  views of the same clusters
