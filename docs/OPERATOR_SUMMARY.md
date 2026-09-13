# Operator Summary
Read a **pt-k8s-debug-collector** `cluster-dump.tar.gz` and say what is wrong with the
Kubernetes cluster it came from: which workloads are short of replicas, which pods are not
running and why, what the operator's own custom resources say about themselves, and what the
operator has been logging.

Where [Stalk Summary](STALK_SUMMARY.md) is ~90% charts, this is ~90% text — the questions are
different in kind. A pt-stalk capture is a time series and the answer is a shape; a
cluster-dump is a single instant and the answer is a sentence: *this pod is crash-looping,
that custom resource never went ready, the operator has been failing to reconcile for an hour.*

The same archives can be drawn as a board rather than read as a report:
[**Kubernetes States**](KUBERNETES_STATES.md) takes a kept capture or an uploaded
`cluster-dump` and lays every object out as a card, red for what was broken at the moment of
the capture — and can diff two captures against each other.

## Where the archive comes from

Three ways, and the third is the one that matters most:

1. **A K3D cluster in this installation.** Open the cluster's **server** node, go to
   **Diagnostics**, and start a capture. Each finished capture is kept with a timestamp, so a
   cluster has a history rather than only its latest. Download it, or open it here.
2. **A capture already kept here** — the list on this page, newest first. Captures outlive the
   clusters they came from, which is the point.
3. **A file dropped on this page**, from any cluster anywhere. A support engineer's
   cluster-dump from a customer's cluster never touched this installation, and reads exactly
   the same: the collector's output has the same shape wherever it was taken.

## What it reads

The archive's internal layout is not documented by Percona, so this parser was written against
a real capture rather than a specification:

```
cluster-dump/nodes.yaml                          cluster-scoped, a v1 List
cluster-dump/errors.txt                          the collector's own failures
cluster-dump/<namespace>/<resource>.yaml         pods, deployments, statefulsets, events, …
cluster-dump/<namespace>/<plural>.<group>.yaml   operator CRs, e.g.
                                                 perconaxtradbclusters.pxc.percona.com.yaml
cluster-dump/<namespace>/<pod>/logs.txt          one file per pod, all containers
```

The collector writes a file per resource per namespace whether or not anything exists, so most
of a real archive is empty lists — those are skipped rather than reported as a hundred empty
namespaces.

## What it tells you

**Four verdicts**, always answered in the order you would ask them, because a question that is
silently omitted reads as one that was never checked:

- Are the **nodes** healthy?
- Is everything that should be running at **full replicas**?
- What is wrong with the **pods**, in one line?
- What does the **operator** itself say?

Plus a **backups** verdict and a **certificates** one whenever the capture contains them.

It also reports the **deployment** itself — which operator, at which version (read from the tag
on the operator's own Deployment image, the only place the archive states it), the CR version,
the Kubernetes version and the PMM client in use — along with every distinct **image** the
deployment runs, the **secrets** it depends on *by name*, its **backups and restores**, its
**TLS certificates** with days-to-expiry, and its **persistent volume claims**.

Then the evidence: findings most-severe-first, the custom resources with their per-component
ready-of-size (`pxc 1/3`, `haproxy 2/2`) and any `Error` condition, the operator's error and
warning lines with repeats collapsed to a count, the unhealthy pods with the tail of each
pod's own log, and the workloads with the short ones first.

### Secrets are names, never values

pt-k8s-debug-collector deliberately does not collect Secret objects, and this does not invent a
way to. What it reports is the **reference graph**: which secrets the pods mount, and which ones
the custom resources name. That is the useful half — a secret named by a CR and mounted by no
pod is a classic operator failure, and nothing else in the archive would mention it — while the
values would only be a liability in an archive that gets emailed around.

### The logs go through Log Summary, not a second parser

A cluster-dump is, in large part, a log bundle wearing a different hat: every pod's log, the
operator's log, each database's own error log, and the namespace's Kubernetes Events. DBCanvas
already has a classifier for all of those — `logsummary_k8sevents.go`, `logsummary_pxcop.go`
(and its psmdb/pg/ps siblings), `logsummary_galera.go`, and the four database readers.

So Operator Summary does not carry its own log parsers. It turns the archive into `[]lsInput`
and calls `lsBuild`, which is a pure function — bytes in, classified events and findings out.
The only rewriting needed is `events.yaml`: the classifier recognises the JSON form and the
collector writes YAML. Everything else is sniffed by content, which is what lets a file called
`logs.txt` be recognised as a PXC operator's zap log and `mysqld-error.log` as a Galera member's,
without either being told.

The payoff is not just less code. On a real PXC capture this immediately produced a finding no
amount of Kubernetes-level parsing would have reached:

> **A member was shut down while it had no primary component — on Kubernetes that is the
> liveness probe, not an operator.** `k3d-00-pxc-0` left the primary component and then received
> a shutdown signal within a minute… A PXC pod's liveness probe asks wsrep whether the member is
> Primary; a member on the wrong side of a partition is not, so the probe fails and kubelet kills
> the container.

Every rule Log Summary learns from now on applies to a cluster-dump for free.

### What the logs actually are, and what is missing

A pod's `logs.txt` is `kubectl logs <pod> --namespace <ns> --all-containers` — **every
container's stdout concatenated**, with nothing marking where one ends and the next begins
(`--all-containers` does not prefix lines). That is not the same as the database's own log, and
two things follow from it.

**The file is classified as a whole, by dominant vocabulary.** A pod whose sidecars are noisier
than its database gets filed under the sidecar: a PSMDB member came out as the PBM agent, and of
three identical PostgreSQL instances two sniffed as Patroni and one as neither. The source is
therefore named `<pod> (stdout)` rather than `<pod>/logs.txt` — the latter reads like the mongod
log, and it is a mixture that merely contains it.

**For MongoDB and PostgreSQL the server's own log is not in the archive at all.** PSMDB routes
mongod's output to `/data/db/logs/mongod.log` when the logcollector sidecar is enabled, so its
stdout contributes almost nothing. The only reason a PXC capture has a real server log is that
the collector separately copies `/var/lib/mysql/*.log` off the pod filesystem — there is no
equivalent for the other engines. A **note** is raised per database pod whose server log is
absent, because left implicit it reads as "the logs were fine".

### A capture taken while the cluster is still starting

This is the case that looks most like a bug in Operator Summary and is not one.

`kubectl logs --all-containers` fails for the **whole pod** if *any* container in it is still
`PodInitializing`. So a capture taken during a rollout comes back with almost no logs — and a
mysqld that has not started has written no error log to collect either. Two captures of the same
cluster minutes apart: 176 KB with three pod logs and no `mysqld-error.log`, and 379 KB with
everything.

Operator Summary counts the collector's own failures in `errors.txt` and raises a **warning**
saying how many pods were still starting, so the answer to "why are my logs not here" is on the
page rather than in the archive.

### Where a summary beats reading the custom resource

A backup custom resource that reports `Running` while the pods doing the work are in `Error` is
the case reading the CR alone gets wrong. The operator retries the job, so the resource sits in
a running state indefinitely while every attempt dies. Operator Summary cross-references the two
and says so. This is not hypothetical: it was found on a live cluster, where a PXC backup sat at
`Running` for ten minutes while its `xb-` pods failed with `xbcloud: Probe failed` against an
unreachable S3 endpoint.

Two behaviours worth knowing, both found by parsing real captures:

- **A crash-looping pod caught between restarts is Running and Ready**, and looks perfectly
  healthy — a capture is one instant and lands wherever it lands. Its restart count and
  whatever killed the container last outrank the phase, so it is still reported.
- **A pod's `logs.txt` is the current container instance's log.** A pod captured just after a
  restart has only the first line or two of the new run; the collector keeps no `--previous`
  log, so the tail of a freshly-restarted container is short by nature rather than truncated.

## Also read from the archive

- **Galera state** — `grastate.dat` and `gvwstate.dat` per member: the UUID, the last committed
  seqno, and `safe_to_bootstrap`. These decide whether a stopped PXC cluster can come back and
  from which member, and nothing else in the archive answers it.
- **The database's own summary** — the per-pod `summary.txt` the collector produces by
  port-forwarding in and running `pt-mysql-summary` / `pt-mongodb-summary` / `pg_gather`.
  Everything else in the archive is Kubernetes' opinion of the database; this is the database's.
- **Backup logs** — the `innobackup.*.log` files, which are where a backup actually says why it
  failed.
- **Scheduled jobs, rendered configuration, rollout revisions, disruption budgets and the
  operator's RBAC** — each a small table, each answering a question that has no other home.

## The capture itself

`pt-k8s-debug-collector` runs in a **throwaway container beside the cluster**, not on a k3s
node. It reaches the cluster over a kubeconfig so it does not need to be there, rancher/k3s is
a minimal busybox image with no package manager, and the collector's most useful output is the
per-pod database summary it makes by port-forwarding into a database pod and running
`pt-mysql-summary` / `pt-mongodb-summary` / `pg_gather` — which need real database clients.

The image is built by `make k8scollector-image` and is **linux/amd64 only**: Percona's apt repo
publishes `percona-toolkit`, the package carrying the collector, for that architecture alone.
On an arm64 host the capture runs under emulation, which is slow but fine for a diagnostic.

---

See also: [Stalk Summary](STALK_SUMMARY.md) · [Operator Debugger](OPERATOR_DEBUGGER.md) · [Stacks](STACKS.md)
