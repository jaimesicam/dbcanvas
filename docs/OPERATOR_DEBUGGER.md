# Operator Debugger

The **Operator Debugger** steps through the Kubernetes operator that is managing one of your
clusters: set a breakpoint in `Reconcile`, make it reconcile, and read the actual values it is
working from — the custom resource it fetched, the request that woke it, the decision it is
about to take. It is [Delve](https://github.com/go-delve/delve) driven from inside DBCanvas,
so there is **no IDE to set up, no clone of the operator, and no Go toolchain**.

![The Operator Debugger stopped in Reconcile, with the call stack and locals beside the source](screenshots/operator-debugger.png)

> *Stopped at the top of `Reconcile` on a live PXC cluster. The call stack shows the operator's
> own frame above three of controller-runtime's (which have no source on the node, and say so);
> `Locals` holds the receiver, the context and the request; and `request.NamespacedName` has been
> evaluated on the spot — it answers `{Namespace: "pxc", Name: "k3d-dbg"}`, the cluster being
> reconciled.*

Open it from the sidebar (**Operator Debugger**) or at `#operator-debugger`. The K3D node's
**Operator** tab also has an **Open debugger** button that opens the page on that frame.

## Getting a cluster you can debug

The operator has to be **deployed under the debugger** — it is a deploy-time decision, because
the operator is rebuilt from that release's own source with the optimiser off
(`-gcflags=all=-N -l`) and run under `dlv` in place of the released binary.

1. In **Database Stacks**, add a **K3D cluster** frame and set its operator to
   **Percona Operator for MySQL (PXC)**.
2. Tick **Run the operator under Delve**.
3. Deploy. The build adds a few minutes on the first run and is cached afterwards.

The released image stays on the pod — only its command changes — so the init containers the
operator gives every database pod still resolve. The liveness probe and leader election are
turned off (both would kill a process sitting at a breakpoint), and Delve is started with
`--continue`, so **the cluster is built whether or not anyone ever attaches**.

Nothing else is needed. The debugger talks to Delve over the stack network, and reads the
operator's source from the k3s node where the release tarball was already unpacked — the same
source the binary was built from, which is why line numbers line up with no configuration.

> **Also publish the debugger to the host** is a separate tick on the frame, and only matters
> if you want to attach VS Code or another external editor. It claims a fixed host port
> (`127.0.0.1:40000`), so leave it off when you are debugging from DBCanvas — two clusters
> debugged at once would otherwise collide on it. The K3D node panel carries the `launch.json`
> and the matching `git clone` when it is on.

## Using it

**Set a breakpoint.** Either click a line number in the source, or use a **quick breakpoint** —
named landmarks in this operator, so you do not have to go and find the line:

| Quick breakpoint | Where it stops |
| --- | --- |
| Reconcile | the main loop — every change to the cluster comes through here |
| deploy | creating or updating the PXC and proxy StatefulSets |
| smartUpdate | the rolling restart: which pod goes next, and why |
| reconcileUsers | the system users' passwords and grants |
| Backup Reconcile | a `PerconaXtraDBClusterBackup` being acted on |
| Restore Reconcile | a `PerconaXtraDBClusterRestore` being acted on |

A breakpoint that could not be resolved shows a hollow marker and Delve's reason — usually
*"please use a line with a statement"*, i.e. you clicked a blank line or a comment.

**Make it stop.** Press **Force a reconcile**. That annotates the custom resource, which is what
makes the operator run `Reconcile` now rather than at its next resync — the one thing an IDE
attached to the same port cannot do for you. (The PXC operator also requeues every five seconds
on its own, so a breakpoint in `Reconcile` will be reached either way.)

**Read the state.** When it stops:

- the **call stack** shows the operator's frames and its dependencies'. Frames from the module
  cache — controller-runtime, the standard library — are listed but marked *no source*: that
  code was never unpacked onto the node. Click any frame to follow it.
- **Variables** shows that frame's scope. Values expand on demand, because an operator's cluster
  object is a graph and serialising all of it to look at one field would cost seconds.
- **Evaluate** takes a Go expression in the selected frame — `request.NamespacedName`,
  `cr.Spec.PXC.Size`, `o.Status.State`. Each one is re-evaluated at every stop.

**Step** with Continue · Pause · Step over · Step into · Step out, exactly as in an IDE.

**Maximize any panel** with the arrows in its top-right corner — the source, the variables, the
call stack, the session log, all of them. Three columns is the right default until you need a
60-frame stack, a struct four levels deep, or a line of Go that runs past the middle column;
maximizing covers the workspace with the one panel you are reading, and the same button (or
<kbd>Esc</kbd>) docks it back. Nothing is lost either way: the panels underneath stay live, so a
variables tree you expanded is still expanded when you come back.

## Two things worth knowing

**A stopped operator manages nothing.** While you sit at a breakpoint the operator is halted: no
probe fails, nothing is logged, and the cluster simply stops being reconciled until you continue.
That is the entire point of the tool, and also the way to break a cluster without noticing. So:

- The banner says so whenever the operator is stopped.
- If nobody touches a stopped session for **five minutes**, DBCanvas continues it for you and
  says so in the session log. The window is configurable (**Resume if idle for**), and can be
  turned off if you know you are stepping through something slowly.
- Closing the page while the operator is stopped **clears the breakpoints and resumes it**
  immediately. Your breakpoints are remembered and re-armed the next time you attach, so a page
  reload costs nothing.
- If DBCanvas itself dies mid-session, a watchdog sidecar in the operator pod notices a halted
  process with no debugger attached and resumes it within ten seconds.

**Evaluating an expression can run code.** Delve will happily call a method on the live operator
if the expression asks it to — inside the process managing a real cluster. Expressions that look
like calls are refused until you tick **Allow function calls**.

## Reaching it, and who can

The debugger is a NodePort Service in front of the operator pod; DBCanvas joins the stack network
and dials the k3s node's own address on it, which is the same route the Stock Market Sim takes to
reach a database inside a K3D cluster. Nothing has to be published to the host.

Access is scoped to a stack you own (an admin sees all), like the web terminal and the file
manager. That is the right boundary and worth being explicit about: **a debugger is remote code
execution by design** — it can read any memory in the operator process and, with function calls
allowed, run its code. Anyone who can open this can already open a root shell on the same node.

## Only PXC, for now

The plumbing is operator-agnostic — the DAP client, the session, the source browsing and the path
mapping know nothing about which operator they are pointed at. What is per-operator is the list of
quick breakpoints, the custom resource to annotate, and the leader-election environment variable
to switch off; each also needs to be tried on a live cluster before it is offered. Today that is
the **Percona Operator for MySQL (PXC)**. A frame that asks for a debugger on any other operator
is warned in the designer and deploys normally, without one.

## See also

- [Stacks](STACKS.md#kubernetes-k3d) — the K3D frame, its operators, and the deploy options
- [Architecture](ARCHITECTURE.md) — how DBCanvas drives Docker and Kubernetes
