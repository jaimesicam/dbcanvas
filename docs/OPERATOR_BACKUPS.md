# Operator Backups

The **Backups** tab on a K3D cluster takes backups through the Percona operator, restores them, and
shows what is actually in the object store. Every action applies a manifest you can read, and each
one is saved as a file on the node so you can apply it yourself with `kubectl`.

It is available on K3D frames running one of the four Percona operators: **MySQL (PXC)**,
**MySQL (Percona Server)**, **MongoDB** and **PostgreSQL**. Open the cluster's **server** node and
choose **Backups** (next to **cr.yaml** and **Secrets & configs**). CloudNativePG and Crunchy PGO
are not supported.

| Pane | What it shows |
| --- | --- |
| **Backups** | Every backup object in the namespace: state, storage, destination, and whether deleting it also deletes the data. Take and restore from here. |
| **Restores** | Every restore object and its outcome. |
| **Bucket** | The objects in the backup store. Shown only when the cluster has an object store. |
| **Manifests** | Every manifest this tab has applied. |

The Backups and Restores lists refresh every five seconds.

## Take a backup

1. Optionally change the **Name** (default `<cluster>-backup-<timestamp>`).
2. Optionally turn on **Delete the data with the object** (see [Delete a backup](#delete-a-backup)).
3. Press **Take a backup**. **Preview YAML** shows the manifest without applying it.

Example manifest (PXC):

```yaml
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterBackup
metadata:
  name: cluster1-backup-20260911-143002
spec:
  pxcCluster: cluster1
  storageName: seaweedfs
```

The state column shows the operator's own state (`Starting`, `Running`, `Succeeded`, `Failed`), or
`Pending` before the operator has picked the object up.

> [!NOTE]
> Backups go to the storage named in the cluster's `cr.yaml`, which DBCanvas points at the stack's
> SeaweedFS node when there is one. A cluster without one backs up to the operator's default PVC
> storage, and the **Bucket** pane is not shown.

## Delete a backup

Deleting the backup **object** and deleting its **data** are separate actions:

| Button | Effect |
| --- | --- |
| **the object** | Deletes the Kubernetes object. The backup files stay in the bucket. |
| **the object and its data** | Adds the `percona.com/delete-backup` finalizer first, so the operator also deletes the files. |

Turning on **Delete the data with the object** when taking a backup adds the finalizer up front.

**PostgreSQL:** pgBackRest manages its own retention, so deleting a backup object never deletes
data and only one delete button is shown. Expire backups in pgBackRest, or delete files in the
**Bucket** pane.

## Restore

> [!WARNING]
> A restore replaces the cluster's data and cannot be undone. The cluster stops serving while it
> runs, and on the MySQL operators the GTID history is replaced too. Take a backup first if the
> current data matters.

**MySQL and MongoDB:** press **Restore…** on a `Succeeded` backup, then **Restore <cluster> from
this** (or **Preview YAML**).

```yaml
apiVersion: pxc.percona.com/v1
kind: PerconaXtraDBClusterRestore
metadata:
  name: cluster1-restore-20260911-150411
spec:
  pxcCluster: cluster1
  backupName: cluster1-backup-20260911-143002
```

**PostgreSQL:** a restore names a pgBackRest repository, not a backup. Use the box under the list:
leave it empty to restore the latest backup, or add pgBackRest options such as
`--set=<label>` (a specific backup) or `--type=time --target=…` (point-in-time). Then press
**Restore <cluster> from <repo>**.

## Browse the bucket

The **Bucket** pane lists what is in the object store, including things no Kubernetes object
points to: binlogs being uploaded for PITR, half-written prefixes from failed backups, pgBackRest
files, and data from backup objects deleted without the finalizer.

1. Press **List the bucket**. Folders are grouped at each `/`; click one to open it, use the
   breadcrumb to go back, and **Load more** to page.
2. **Download** saves one object, up to **64 MiB**. For larger files the pane shows a command to
   copy it inside the cluster instead.
3. **Delete** removes one object, or everything under a prefix. A prefix delete has a **dry run**
   button; use it first. Either way the pane reports how many objects matched.

Each action shows the exact `kubectl exec … aws s3api …` command it ran.

**The toolbox pod.** Bucket operations run the AWS CLI in a pod on the cluster named
`<cluster>-dbcanvas-s3`, using the cluster's own backup credentials Secret, so it works with any S3
endpoint the operator uses. It starts on first use and keeps no state. On PXC and Percona Server it
uses the cluster's backup image; on MongoDB and PostgreSQL the first start pulls
`percona/percona-xtrabackup:8.0`. When the store uses HTTPS, the toolbox verifies its certificate
against the Intranet CA and the store's own certificate. **Stop toolbox** removes the pod; the
bucket is not touched.

## Manifests on the node

Every manifest the tab applies is saved in the operator's source directory on the server node:

```
<operator source>/deploy/backup/
```

For example `/root/percona-xtradb-cluster-operator-1.20.0/deploy/backup/`, next to the release's
own `backup.yaml` and `restore.yaml`. Each file starts with the commands to apply it again and
watch the result:

```yaml
#   kubectl apply -n default -f deploy/backup/cluster1-backup-20260911-143002.yaml
#   kubectl -n default get pxc-backup -w
```

The **Manifests** pane lists and shows these files.

## Operator differences

| Operator | Backup / Restore kinds | Cluster field | Storage field | Delete data with object? | Restore from |
| --- | --- | --- | --- | --- | --- |
| **MySQL (PXC)** | `PerconaXtraDBClusterBackup` / `PerconaXtraDBClusterRestore` | `pxcCluster` | `storageName` | yes, via finalizer | a backup |
| **MySQL (Percona Server)** | `PerconaServerMySQLBackup` / `PerconaServerMySQLRestore` | `clusterName` | `storageName` | yes, via finalizer | a backup |
| **MongoDB** | `PerconaServerMongoDBBackup` / `PerconaServerMongoDBRestore` | `clusterName` | `storageName` | yes, via finalizer | a backup |
| **PostgreSQL** | `PerconaPGBackup` / `PerconaPGRestore` | `pgCluster` | `repoName` | no — pgBackRest manages retention | a repository |

## API

Every action has an endpoint under the frame — see
[API & CLI reference](API_REFERENCE.md#kubernetes-frames).

```sh
# take a backup, then watch its state
dbcanvas api POST /api/stacks/1/frames/k3d-01/k3d/backups --data '{}'
dbcanvas api GET  /api/stacks/1/frames/k3d-01/k3d/backups | jq '.backups[] | {name, state, destination}'

# list the bucket under a prefix
dbcanvas api GET '/api/stacks/1/frames/k3d-01/k3d/bucket?prefix=cluster1-2026-09-11'
```
