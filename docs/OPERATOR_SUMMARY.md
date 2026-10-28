# Operator Summary

Operator Summary reads a **pt-k8s-debug-collector** `cluster-dump.tar.gz` and reports what is
wrong with the Kubernetes cluster it came from. It shows which workloads are short of
replicas, which pods are not running and why, what the operator's custom resources say,
and what the operator has been logging.

Open it from the sidebar (**Operator Summary**).

To see the same capture as a board of cards, with broken objects in red and a diff between
two captures, use [Kubernetes States](KUBERNETES_STATES.md).

## Get a cluster-dump

- **From a K3D cluster in a stack.** Open the cluster's **server** node, go to its
  **Diagnostics** tab and click **Start capture**. A busy cluster takes a few minutes. Each
  finished capture is kept under the time it was taken, with **Download** and **Operator
  Summary** buttons.
- **From a capture kept here.** **Captures kept here** lists them newest first, with
  **Analyse**, **Download** and **Delete**. Kept captures remain after the cluster is
  deleted.
- **From any cluster.** Drop a `cluster-dump.tar.gz` on the page, or click **Choose a file**.
  The collector's output has the same layout wherever it was taken.

The capture runs pt-k8s-debug-collector in a separate container next to the cluster, with
`-resource` set to the cluster's operator so that the operator's custom resources are
included. The collector image is linux/amd64 only, so on an arm64 host it runs under
emulation and is slower.

## What the report shows

**Verdict.** These questions are always answered, in this order:

- Are the **nodes** healthy?
- Are all **workloads** at full replicas?
- What is wrong with the **pods**?
- What does the **operator** say?

When the capture contains them, **backups** and **certificates** get a verdict too.

Then, card by card:

| Card | Contents |
| --- | --- |
| **Findings** | Everything worth acting on, most severe first |
| **Deployment** | Operator and version, CR version, Kubernetes version, PMM client |
| **Custom resources** | Each component's ready count against its size (e.g. `pxc 1/3`) and any `Error` condition |
| **Backups & restores** | Every backup and restore resource, newest first |
| **Certificates** | TLS certificates, soonest to expire first |
| **Logs** | Kubernetes Events, the operator log and each database's error log, classified as in [Log Summary](LOG_SUMMARY.md) |
| **Operator log** | Error and warning lines, with repeats collapsed into a count |
| **Galera state** | PXC only: each member's `grastate.dat` / `gvwstate.dat` (seqno, `safe_to_bootstrap`), which tell you whether a stopped cluster can start and from which member |
| **Backup logs** | What xtrabackup wrote on each member |
| **Pods needing attention** | Unhealthy pods only, each with the end of its log |
| **Workloads** | Deployments and StatefulSets, those short of replicas first |
| **Storage** | Persistent volume claims, unbound ones first |
| **Images** | Every distinct image. A half-finished upgrade shows up here |
| **Secrets** | Secrets the pods mount and the custom resources name, by name only. The collector does not collect secret values |
| **Database summary** | Each pod's `pt-mysql-summary`, `pt-mongodb-summary` or `pg_gather` output |
| **Schedules, rollouts, budgets and RBAC** | Scheduled jobs, rendered configuration, rollout revisions, disruption budgets and the operator's RBAC |
| **The collector could not read everything** | The collector's own errors |

## Things to know when reading it

- **A backup marked `Running` with failing pods is reported as failing.** The operator keeps
  retrying the job, so the custom resource can stay `Running` indefinitely.
- **A crash-looping pod can look healthy.** If the capture landed between restarts, the pod
  is Running and Ready. It is still reported because of its restart count and its last
  termination reason.
- **Pod logs come from the current container only.** A pod that has just restarted has a
  short log. Previous containers' logs are not collected.
- **A pod's log mixes all of its containers' output.** It is labelled `<pod> (stdout)`. When
  a sidecar writes more than the database, the log may be classified as the sidecar's.
- **MongoDB and PostgreSQL server logs are not in the archive.** Only PXC captures include
  the database's own error log. A note is raised for each database pod whose server log is
  missing.
- **A capture taken while pods are starting has few logs.** The collector cannot read the
  logs of a pod whose containers are still initialising. A warning says how many pods were
  still starting. Take the capture again once the cluster is up.

---

See also: [Kubernetes States](KUBERNETES_STATES.md) · [Operator Debugger](OPERATOR_DEBUGGER.md) · [Log Summary](LOG_SUMMARY.md) · [Stacks](STACKS.md)
