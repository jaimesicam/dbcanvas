# Operator Backups

The **Backups** tab on a Kubernetes frame does the three things a Percona operator's backups
need doing: takes them, puts them back, and shows you what is actually in the bucket underneath.
Every action is a manifest the panel writes, applies and files on the node — so the tab is a way
to *learn* this, not a thing you have to keep using.

It is on any K3D frame running one of the four Percona operators — **MySQL (PXC)**,
**MySQL (Percona Server)**, **MongoDB**, or **PostgreSQL**. Open the cluster's **server** node
and pick **Backups**, next to **cr.yaml** and **Secrets & configs**. The three sit together on
purpose: `cr.yaml` is the standing instruction to the operator, Secrets are the objects it is
told it with, and this is the two things you ask it to do once.

## What each pane is for

| Pane | What it shows |
| --- | --- |
| **Backups** | Every Backup custom resource in the namespace, its state, where it went, and whether deleting it will take the data too. Takes new ones; restores from one. |
| **Restores** | Every Restore object and what became of it. |
| **Bucket** | What is really in the object store — listed, downloaded and deleted through a pod on the cluster. |
| **Manifests** | Every document this tab has applied, as files on the node. |

## Taking a backup

Press **Take a backup**. The panel writes a manifest, applies it, and shows you both the document
and where it filed it:

```yaml
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterBackup
metadata:
#  finalizers:
#    - percona.com/delete-backup
  name: cluster1-backup-20260911-143002
spec:
  pxcCluster: cluster1
  storageName: seaweedfs
```

The name is the cluster, the verb and a UTC timestamp, so the list sorts newest-first by name
alone. **Preview YAML** builds the same document and applies nothing, which is the way to read
what a setting does before it does it.

The state in the table is the operator's own — `Starting`, `Running`, `Succeeded`, `Failed` — and
the table polls every five seconds while you watch. An object the operator has not reached yet
reads `Pending` rather than showing a blank cell.

> [!NOTE]
> **Where does it go?** `storageName` (or `repoName` on PostgreSQL) names the storage in the
> cluster's `cr.yaml`, which DBCanvas pointed at the stack's SeaweedFS node at deploy time. A
> frame deployed without one still takes backups — to the PVC the operator ships — and the
> Bucket pane is simply not offered.

## Deleting a backup, and deleting a backup

These are different acts, and the tab gives them different buttons because the difference is the
part people come to a lab to find out:

- **the object** — `kubectl delete pxc-backup <name>`. The record goes. Every byte stays in the
  bucket, unreferenced and invisible to Kubernetes.
- **the object and its data** — the same delete, but the panel first patches the
  `percona.com/delete-backup` finalizer onto the object, which is what makes the operator clear
  the storage on its way out.

The finalizer is set at the moment of deletion rather than assumed from how the backup was
created, so "delete the data too" means the same thing for a backup taken here, by a schedule, or
by the replication seed. There is also a **Delete the data with the object** switch when taking
one, which arms the finalizer up front — that is how the operators' own `backup.yaml` documents
it, commented out.

Percona's PostgreSQL operator is the exception and the tab says so: pgBackRest owns its
repository's retention and the operator does not reach into it, so a `PerconaPGBackup` is only
ever a record. Expire it from the repository, or delete the objects in the Bucket pane.

## Restoring

**Restore…** on a `Succeeded` backup opens a confirmation that states the cost plainly: the
cluster stops serving, its data is replaced, and on the MySQL operators the GTID history goes
with it. There is no undo. The manifest is one field:

```yaml
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterRestore
metadata:
  name: cluster1-restore-20260911-150411
spec:
  pxcCluster: cluster1
  backupName: cluster1-backup-20260911-143002
```

**PostgreSQL restores a repository, not a backup object.** `PerconaPGRestore` names `repo1` and
hands the rest to pgBackRest, so that pane offers an options box instead of a button per row:
`--set=<label>` picks a specific backup, `--type=time --target=…` does point-in-time. With no
options it restores the latest backup in the repo, which is what pgBackRest does on its own.

## The bucket

A backup object says where it went and stops there. The **Bucket** pane is the other half:

- the binlogs a PITR collector is uploading right now,
- the prefix a failed backup left half-written,
- a pgBackRest repository nothing has expired,
- anything whose backup object was deleted **without** the finalizer.

None of that is a Kubernetes object, so none of it appears anywhere else.

### How it works — the toolbox pod

Every operation in this pane runs `aws` **in a pod on the cluster**, not from DBCanvas. The pod
is `<cluster>-dbcanvas-s3`, it runs an image that carries the AWS CLI, and it takes the cluster's
own backup credentials straight from the Secret the operator already uses:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cluster1-dbcanvas-s3
spec:
  restartPolicy: Never
  containers:
  - name: s3
    image: percona/percona-xtrabackup:8.0
    command: ["sleep", "infinity"]
    env:
    - name: AWS_ENDPOINT_URL
      value: "http://seaweedfs-1.example.net:8333"
    - name: AWS_EC2_METADATA_DISABLED
      value: "true"
    envFrom:
    - secretRef:
        name: cluster1-backup-s3
```

That choice buys three things. It works against **any** S3 endpoint the operator was pointed at,
not just DBCanvas's own SeaweedFS node. Every operation is therefore a real `kubectl` command
against a real manifest, and the panel hands you the exact line it ran. And it is how this is
actually done: Percona's backup images ship the AWS CLI next to `xbcloud`, which is why "spawn an
xtrabackup pod and use the CLI" is the standard way to get at a Percona cluster's bucket.

The pod is **started on demand and holds no state**. On a PXC or Percona Server cluster it uses
the cluster's own backup image, which is already on every node, so it costs nothing; on MongoDB
and PostgreSQL — whose backup images carry `pbm` and `pgbackrest` and no S3 client — the first
start pulls `percona/percona-xtrabackup`. **Stop toolbox** removes it; the bucket is untouched.

### Listing, downloading, deleting

Listing folds at the next `/`, so a flat keyspace reads as directories and a pgBackRest
repository is navigable rather than forty thousand rows. Click a folder to descend, the
breadcrumb to come back, **Load more** to page.

```sh
kubectl -n default exec cluster1-dbcanvas-s3 -- \
  aws s3api list-objects-v2 --bucket dbcanvas --delimiter / --prefix cluster1-2026-09-11/ --output json
```

**Download** streams one object to your browser, up to **64 MiB**. The cap is deliberate: the
things worth reading from here — an `xtrabackup_info`, a pgBackRest manifest, a `.md5` — are
kilobytes, and a backup artefact is gigabytes. Ask for a larger one and the panel refuses with
the command that copies it *inside* the cluster instead.

**Delete** removes one object, or everything under a prefix. A prefix delete offers a **dry run**
beside it and you should use it — `aws s3 rm --recursive` over the wrong prefix is the one action
in this tab with no counterpart anywhere else. Either way the panel reports how many objects
matched, so "nothing matched" never reads as "done".

## Everything it applied, on the node

Every manifest this tab applies is filed at:

```
<operator source>/deploy/backup/
```

— which on a PXC cluster is `/root/percona-xtradb-cluster-operator-1.20.0/deploy/backup/`, beside
the `backup.yaml` and `restore.yaml` samples the release already ships. Each file carries the
commands to apply it again in its header:

```yaml
# pxc-backup cluster1-backup-20260911-143002 — take a backup of cluster1
#
# Written by DBCanvas on 2026-09-11T14:30:02Z. Apply it again, or edit it and apply that, with:
#
#   kubectl apply -n default -f deploy/backup/cluster1-backup-20260911-143002.yaml
#
# and watch what the operator makes of it with:
#
#   kubectl -n default get pxc-backup -w
```

The **Manifests** pane lists them and shows any of them, but the point is that they are files on
a node you have a root console to. Nothing in this tab does anything to a cluster that is not
`kubectl apply` of a document you can read.

## The four operators, side by side

The tab is one set of controls over four CRDs. What differs:

| | Backup / Restore kinds | Cluster field | Storage field | Delete the data? | Restore names |
| --- | --- | --- | --- | --- | --- |
| **MySQL (PXC)** | `PerconaXtraDBClusterBackup` / `…Restore` | `pxcCluster` | `storageName` | yes, via finalizer | a backup |
| **MySQL (PS)** | `PerconaServerMySQLBackup` / `…Restore` | `clusterName` | `storageName` | yes, via finalizer | a backup |
| **MongoDB** | `PerconaServerMongoDBBackup` / `…Restore` | `clusterName` | `storageName` | yes, via finalizer | a backup |
| **PostgreSQL** | `PerconaPGBackup` / `PerconaPGRestore` | `pgCluster` | `repoName` | no — pgBackRest owns retention | a repository |

The two community PostgreSQL operators DBCanvas can also install — CloudNativePG and Crunchy PGO
— are Helm-installed and model backups differently enough that they are left out, the same way
they are left out of the cr.yaml editor.

## Two things worth knowing

**A restore is not undoable, and the cluster is down while it runs.** The operator pauses it,
replaces the data, and brings it back. On a lab this is the experiment; it is also the fastest way
to lose the state you were part-way through building. Take a backup of what is there first — it
costs a minute.

**Deleting a backup object usually leaves the data.** This surprises people, and it is the
operators' own default, not DBCanvas's. If your bucket keeps growing while the backup list stays
short, that is why — and the Bucket pane is how you find what is in there and clear it.

## API

Every action has an endpoint under the frame; see
[API & CLI reference](API_REFERENCE.md#kubernetes-frames).

```sh
# take one, and watch for it
dbcanvas api POST /api/stacks/1/frames/k3d-01/k3d/backups --data '{}'
dbcanvas api GET  /api/stacks/1/frames/k3d-01/k3d/backups | jq '.backups[] | {name, state, destination}'

# what is in the bucket
dbcanvas api GET '/api/stacks/1/frames/k3d-01/k3d/bucket?prefix=cluster1-2026-09-11'
```
