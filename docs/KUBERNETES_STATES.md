# Kubernetes States
A live board of everything in a Kubernetes cluster: one card per object, red for what is
broken, lit for what just changed, and a tombstone for what disappeared while you were
looking somewhere else.

Where [Operator Summary](OPERATOR_SUMMARY.md) reads a capture of a cluster **after the
fact**, this watches one **while it happens**. Same subject, opposite tense. It is the page
to have open while a failover runs, while an operator rolls a StatefulSet, while a backup
job goes through — the minutes when the question is not "what is wrong with this object"
but "what is moving".

## Opening it

- **Kubernetes States** in the sidebar, then pick a source.
- **Watch Kubernetes states** on a K3D cluster's own **server** node panel, which opens the
  page on that cluster.

Nothing has to have been deployed specially: watching a cluster is a read of its own API
server through the k3s node's `kubectl`, exactly like every other Kubernetes feature here.

## Three sources

The source picker holds all three, and once a board is drawn it makes no difference which one
it came from — the same cards, the same colours, the same panes:

| Source | What it is |
| --- | --- |
| **Live clusters** | Every running Kubernetes frame across your stacks, sampled on a timer. |
| **Captures** | A [pt-k8s-debug-collector](OPERATOR_SUMMARY.md) `cluster-dump` kept by a cluster's **Diagnostics** tab. Captures outlive the clusters they came from, so a board can be drawn of something that no longer exists. |
| **Uploaded** | A `cluster-dump.tar.gz` from **this machine** — a customer's cluster this installation has never seen reads exactly the same. |

A capture is the same objects in a different encoding (the collector writes Kubernetes Lists
as YAML; a live cluster answers in JSON), so every rule on this page applies to it unchanged:
a pod is red for the same reason, its container rows say the same thing, and its warning
events hang off it the same way.

**What a capture cannot be**, said on the board rather than left to be discovered:

- **It is one instant.** Nothing is changing and nothing has disappeared, because there is no
  next sample. The sample rate control is therefore not offered for one.
- **It is only what the collector could reach.** `pt-k8s-debug-collector` keeps its own
  failures in `errors.txt`, and the board counts them and quotes the first — a board quietly
  showing fewer objects because half the cluster was unreachable is exactly the kind of
  silence this page exists to avoid.
- **A pod's logs are whatever the collector kept** — see [Logs](#logs-per-container) below.

### Everything in the archive, and where it comes out

The layout below is from the collector's own source (`paths.go`, `resources.go`), not from
one example — and two layouts are in the wild, both of which read:

| In the capture | On the board |
| --- | --- |
| `<ns>/<resource>.yaml` — **every** resource the API server served that had items | a card each, by the same rules a live cluster gets. Not a fixed list of kinds: whatever is in the archive is on the board, including operators this app has never heard of |
| `cluster-scope/<resource>.yaml` (newer collectors) or `<resource>.yaml` at the root (older) | the same |
| `<ns>/secrets/<name>.yaml` — one object per file, not a List | a card each (key counts only) |
| `<ns>/events.yaml` | the warnings in a card's State pane |
| `<ns>/<pod>/<container>.log` (newer) or `<ns>/<pod>/logs.txt` (older) | the Logs pane |
| `<ns>/<pod>/summary.txt` — `pt-mysql-summary`, `pt-mongodb-summary`, or pg_gather's output | the Logs pane |
| `<ns>/<pod>/var/lib/mysql/…` — PXC's `mysqld-error.log`, the **`innobackup.backup/move/prepare.log` backup logs**, `grastate.dat`, `gvwstate.dat` | the Logs pane, with the backup logs ranked above the rest |
| `<ns>/<pod>/pg_log/…`, `pgbackrest_log/…`, `pgbackrest-info.log`, `patronictl-list.log` | the Logs pane — PG's server logs, **pgBackRest's own logs**, and `pgbackrest info` / `patronictl list` as they were at capture time |
| `<ns>/<secret-name>` — a TLS secret's certificate, `openssl x509 -noout -text` | **Capture files** |
| `errors.txt` | the warning above the board, and **Capture files** |

**Capture files** is the chip beside the kind chips when the source is a capture: every file
in the archive, what it is, what it belongs to, and its contents. It is there so the answer to
*"is there anything in this capture the board is not showing me"* is no.

### Comparing two captures

Tick **compare** and loading another source keeps what is on the board instead of clearing
it. Load Monday's capture, then Tuesday's: what changed between them is **lit**, with its old
value beside it, and what is in the first and not the second is a **tombstone**. It is the
same machinery that watches a live cluster — two captures are two samples a long way apart.

Uploaded archives are held **in memory, for an hour, for the account that uploaded them**.
They are never written to the dumps directory: a kept capture belongs to a stack, which is
what decides who may read it, and an uploaded archive belongs to nobody.

## What is on the board

One column per kind: **Nodes**, **Pods**, **StatefulSets**, **Deployments**, **ReplicaSets**,
**Jobs**, **CronJobs**, **PersistentVolumeClaims**, **PersistentVolumes**, **Services**,
**PodDisruptionBudgets**, then the operator's own **custom resources** — the cluster object,
its backups and its restores. A capture adds everything else the collector found:
ConfigMaps, Secrets, StorageClasses, the RBAC objects, and any custom resource from any
operator, known or not.

**Nothing is dropped, but not everything is in front of you.** A real cluster has 468
ClusterRoles and two StatefulSets, so the noisy kinds start **folded**: their chip shows the
count and one click puts them on the board. *show all* unfolds every one of them. A kind you
unfold stays unfolded while you work.

Each card carries the two or three numbers the object reports about itself:

| Kind | What the card says |
| --- | --- |
| Pod | phase, ready *n*/*m*, restarts, node, IP, and a row per container that is not simply running (`CrashLoopBackOff`, `ImagePullBackOff`, `Error (exit 3)`, `Running, not ready`) |
| StatefulSet / Deployment | ready *n*/*m*, updated *n*/*m* mid-rollout, available |
| Job | active, succeeded, failed, and when it started and finished |
| PersistentVolumeClaim | phase, capacity, storage class, volume |
| Service | type, cluster IP, ports, and a LoadBalancer's external address |
| Node | Ready, any pressure condition that is **True**, cordoned, kubelet version |
| PersistentVolume | phase (**Released** is a claim's outage seen from the other end), capacity, reclaim policy, which claim held it |
| CronJob | the schedule, whether it is **suspended**, when it last ran and last succeeded |
| PodDisruptionBudget | healthy *n*/*m*, and `disruptionsAllowed: 0` — which is why an operator's rolling restart is sitting there doing nothing |
| ConfigMap / Secret | how many keys, and a Secret's type. Never a key name, never a value — [Secrets & configs](STACKS.md) is where those are read |
| Anything with no status (Roles, StorageClasses, …) | an inventory card, uncoloured. A board of amber "no status" cards about objects that never have one is what teaches people to ignore amber |
| Custom resource | whatever its status says — `state: ready`, `postgres.ready 3`, and any condition that is not True |

## The four colours, and the fifth

| Colour | Means | Examples |
| --- | --- | --- |
| **Red** | The object says it is broken | `Failed` pod · a container in `CrashLoopBackOff` or `ImagePullBackOff` · a container that exited non-zero · `Lost` claim · `NotReady` node · a custom resource in `error` · a `Ready` condition that is `False` · a workload with **zero** ready replicas |
| **Amber** | On its way somewhere, or short of what it wants | `Pending` · `ContainerCreating` · `Terminating` · ready 2/3 · restarts > 0 · a LoadBalancer with no address yet · a custom resource `initializing` |
| **Green** | Doing what it says it should | a Running, fully ready pod · 3/3 replicas · a `Bound` claim · `state: ready` |
| **Grey** | Finished, on purpose | a `Succeeded` pod · a complete Job · a workload scaled to 0 |
| **Dashed grey** | **Deleted** — gone from the cluster, kept here | any object that disappeared between two samples |

**Red is narrow on purpose.** It means the object itself says it is broken, never "something
nearby looks odd", and it is never inferred from events — a board where everything is red is
a board nobody reads. A completed backup pod is grey, not red, and a lab cluster is mostly
completed backup pods.

Inside a card the **rows are coloured individually**, and the coloured ones come first. The
card tells you the pod is broken; the row tells you it is the `database` container, waiting
on `CrashLoopBackOff`.

## What just changed

Every sample is compared with the one before it. A property whose value changed is **lit for
about twelve seconds and says what it changed from**:

```
Ready   2/3 (was 3/3)
```

That is a failover, visible without reading anything. A **new** object arrives with a
highlight of its own, so a pod the operator has just created announces itself.

Highlights fade on their own, on a clock of their own — a one-minute sample rate does not
leave a change ringed for a minute.

## What disappeared

An object that is in one sample and not the next is **not removed**. It stays in place,
greyed and dashed, its name struck through, showing the last state it was ever in — until you
dismiss it with the **✕** on the card, or **Dismiss *n* deleted** in the header.

This is the reason the page exists in the shape it does. The pod that was crash-looping and
then vanished is the one you needed to see, and every other tool shows you the list it is no
longer in. A tombstone also survives the *Only what is wrong or changing* filter: filtering
for trouble must not hide the evidence.

Tombstones are per-session — they live in the browser tab, not in DBCanvas — so switching
clusters or reloading starts a fresh board.

## Panes: state, logs, YAML — and pinning any of them

Click a card and it opens as a **pane** in the rail beside the board. A pane is one object
seen one of three ways:

| Tab | What it shows |
| --- | --- |
| **State** | Every property with its colour and its change note, the object's owner, and the cluster's recent **warning events** about it — `BackOff`, `FailedScheduling`, `Unhealthy`, `ProvisioningFailed`, with their repeat counts |
| **Logs** | One container's log (pods only) |
| **YAML** | The object as `kubectl get -o yaml` prints it — read-only |

Warning events **explain** a red card, they never **cause** one. A healthy pod with a
liveness probe that failed twice an hour ago is still green, and the warnings are still
there to read.

**Pin any pane** — the pin in its header — and it stays in the rail while you click through
everything else. That is what makes this a workbench rather than a viewer: pin the custom
resource's **State**, pin the crashing container's **Logs**, and watch both while you go
through the pods. Pinned panes keep updating and keep highlighting. Switching a pinned pane's
tab moves that pane; it does not unpin it or leave a copy behind. The pin on a *card* pins
its State pane, which is what a card shows.

Drag the divider to make the rail wider — a log wants far more room than a property list —
or **maximize** one pane to fill the page (Esc to come back).

### Logs, per container
<a id="logs-per-container"></a>

A pod is several logs, not one. The pane has a **container picker**, and it opens on the
container worth reading: the one that is unhealthy if there is one, otherwise the first real
container — never an init container that finished half an hour ago. Init containers are in
the list, marked, because a pod stuck in `Init:` is exactly when you want one.

| Control | What it does |
| --- | --- |
| Container | Which log. Ordered worst-first; a broken container is marked. |
| Lines | 100, 200 (default), 1000 or 5000 — this is a tail, capped server-side. |
| follow | Re-read with every sample of the board, so the log and the card are never different ages. On by default. Turn it off the moment you are reading something. |
| previous | `kubectl logs --previous` — **the log of the run that died**. Offered once a container has restarted at all, and it is the only log a `CrashLoopBackOff` has anything useful in: the current one has barely started. |

The pane scrolls itself to the newest line as the tail grows, unless you have scrolled up.
`kubectl`'s own notes about a successful read appear above the log — which container it
defaulted to, or why a previous run's log could not be retrieved — because "no log lines" and
"that log is gone" are different answers.

**From a capture the picker lists files instead**, because that is what the collector kept —
and which files depends on which collector took it. A current one writes **one log per
container** (`<container>.log`); an older one writes a single `logs.txt` for the whole pod.
The pane says which it is looking at rather than assuming, and it asks a capture for no
particular file so the reply can name one: a container name means nothing to an archive, and
asking for one anyway is what used to leave the pane with nothing but "no such file". Ask for
something a capture does not have and it answers with the pod's first file, says so, and
lists the rest — the file list is the one thing the pane must never be denied.

Beside the logs is everything the collector pulled off the container's disk, and this is the
reason a capture is sometimes better reading than the live cluster: on a **PXC** pod,
`mysqld-error.log`, the **`innobackup.*.log` backup logs**, `grastate.dat`, `gvwstate.dat` and
`pt-mysql-summary`'s output; on a **PG** pod, the server's `pg_log/`, **pgBackRest's own
`pgbackrest_log/`**, and the output of `pgbackrest info` and `patronictl list` as they were at
capture time. `kubectl logs` would never have given you any of it.

### YAML

The whole object, as the API server has it. Since Kubernetes 1.21 `kubectl get -o yaml`
leaves `managedFields` out, so it is the object rather than a page of apply bookkeeping.

**Read-only, deliberately.** This page is a monitor. The
[custom resource editor and the Secrets editor](STACKS.md) are where a cluster is changed,
and both dry-run against the API server before they write. `follow` is off by default here,
unlike a log: a manifest that reloads under you takes your scroll position with it.

From a capture the YAML comes out of the archive rather than off the cluster — the object as
it was at the moment of the capture, which is usually the thing you actually want to read.

## The controls

| Control | What it does |
| --- | --- |
| Source | A live cluster, a kept capture or an uploaded archive. Switching clears the board — tombstones and highlights belong to what they came from — unless **compare** is ticked. |
| Upload a cluster-dump | Read a `cluster-dump.tar.gz` from this machine. |
| compare | Keep the board when the source changes, so a second capture arrives as a diff of the first. |
| Sample rate | Live clusters only: 2s, 5s (default), 15s, 1m, or paused. Each sample is three `kubectl get`s inside the k3s node, so the fast end is for watching a failover and the slow end is for leaving open beside something else. |
| Sample now / Reload | One sample, whatever the rate — including while paused. Reads the archive again for a capture. |
| Filter box | Matches a name, a kind, a summary **or a property value**, so `crashloop` finds every pod in one. |
| Namespace | One namespace, or all of them. |
| Only what is wrong or changing | Red, amber and tombstones. |
| Kind chips | Fold a kind away or bring it back; the count is on the chip. The noisy ones (RBAC, ConfigMaps, ReplicaSets, …) start folded. **show all** unfolds everything. |
| Capture files | Captures only: browse every file in the archive. |
| Dismiss *n* deleted | Clear every tombstone at once. |
| Reset view | Recentre the board at 100%. |
| Fill the window | The page takes the whole window, sidebar and tabs included. Esc comes back. |

The board pans by dragging the background and zooms with the wheel, like the
[Stack Designer](STACKS.md) canvas.

Sampling is **focus-gated**: it stops when the tab is not visible and not focused, and takes
a fresh sample when you come back. A page left open behind another one costs nothing.

## What it does not do

- **It does not write.** Every call is a read; nothing here scales, deletes, restarts or
  patches anything. The [cluster's own panel](STACKS.md) is where a cluster is changed.
- **It does not watch, it samples.** Something created and deleted between two samples is
  never seen. At 2s that window is small, and the trade is deliberate: three plain
  `kubectl get`s recover from a cluster restart on their own, where a long-lived watch is a
  connection to lose and re-establish.
- **It does not keep a log.** A log pane is a tail — the newest lines, capped, read again on
  each sample. It is not an archive and it does not scroll back past what `--tail` asked for;
  the console on the cluster's node is where the whole thing lives. Warning events are capped
  the same way: five per object, newest first.

## API

| To do this | API |
| --- | --- |
| List the clusters you can watch | `GET /api/k3d/states/targets` |
| Take one sample of a cluster | `GET /api/stacks/{id}/frames/{fid}/k3d/states` |
| Tail one container's log | `GET …/k3d/states/logs?namespace=&name=&container=&tail=&previous=1` |
| Read one object as YAML | `GET …/k3d/states/manifest?kind=&namespace=&name=` |
| Build the board from a kept capture | `POST /api/k8sstates/dumps/{did}` |
| Build it from an uploaded archive | `POST /api/k8sstates/upload` (multipart `file`) |
| Read an object's YAML out of an archive | `GET /api/k8sstates/archive/manifest?dump=&upload=&kind=&namespace=&name=` |
| Read a pod's kept files out of an archive | `GET /api/k8sstates/archive/logs?dump=&upload=&namespace=&name=&file=` |
| List every file in an archive, or read one | `GET /api/k8sstates/archive/files?dump=&upload=&path=` |

A sample is `{objects, namespaces, kinds, clusterNamespace, operator, capturedAt, warnings}`.
Each object is `{uid, kind, namespace, name, tone, summary, owner, createdAt, props[], events[], containers[]}`
(`containers` on pods only — name, init, tone and restart count, which is what the log
picker is built from),
and each property is `{key, value, tone}` — the same four tones the board paints. `warnings`
is what the sample could not read (a cluster with no operator has no custom resources to
list), reported rather than hidden, because a board missing a column should say why.

A log reply is `{text, note, error, truncated, tail, container, readAt}`. `note` is what
`kubectl` said on **stderr** about a successful read; `error` is a read that did not happen
(asking for `--previous` on a container that never restarted, say) — neither is an HTTP
error, because neither is a failure of the request. Every path parameter is checked against
the shape of a Kubernetes name before it reaches `kubectl`: a "name" of `--all-containers`
is a flag, not a name.
