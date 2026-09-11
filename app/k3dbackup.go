package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// k3dbackup.go — the backups and restores of an operator-managed cluster, as a panel.
//
// The cr.yaml editor (k3dcrform.go) covers what the operator is told to do and the Secrets editor
// (k3dobjects.go) the objects it is told it with. This covers the third thing an operator does on
// request rather than continuously: it takes a backup, and it puts one back.
//
// Every one of the four Percona operators models both as a custom resource — a Backup object with
// a cluster and a storage, a Restore object with a cluster and the name of a backup — so the whole
// tab is one table (k3dBackupKinds) and one set of handlers, not four. What differs between them
// is the API group, the two kinds, and whether the field naming the destination is called
// `storageName` (PXC, PS, PSMDB: an entry in spec.backup.storages) or `repoName` (PG: a pgBackRest
// repository). Nothing else about the shape of the request changes.
//
// Three rules shape the file, and they are the reason it is worth having at all rather than
// telling people to go and write the YAML:
//
//  1. EVERY OPERATION IS A MANIFEST, AND THE MANIFEST IS KEPT. Nothing here does anything to a
//     cluster that is not `kubectl apply` of a document this file wrote. That document is archived
//     into the operator's own source tree, at <OperatorSrc>/deploy/backup/, alongside the
//     backup.yaml and restore.yaml samples the release already ships — so what the panel did is
//     readable, re-appliable and diffable against what Percona documents, from the node, with no
//     DBCanvas involved. The archived file carries the kubectl commands in its header.
//
//  2. DELETING A BACKUP OBJECT AND DELETING A BACKUP ARE DIFFERENT ACTS. `kubectl delete
//     pxc-backup` removes the record and leaves every byte in the bucket; it is the finalizer
//     `percona.com/delete-backup` that makes the operator clear the storage too. Both are
//     offered, separately and by name, because a lab is where people find out the difference —
//     preferably not by deleting the one copy of something.
//
//  3. A RESTORE IS NOT UNDOABLE AND THE PANEL SAYS SO. It stops the cluster, replaces its data,
//     and on PXC/PS takes the GTID history with it. The handler refuses nothing — this is a lab,
//     and that is the experiment — but it logs what it did to the stack's deployment log, where
//     the rest of the destructive operations in this app are already recorded.
//
// The bucket underneath all of this is in k3dbucket.go: what the operator wrote, as objects.

// k3dBackupKind is one operator's backup and restore custom resources.
//
// Res is the short resource name kubectl accepts (`pxc-backup`) rather than the kind, because it
// is both shorter and unambiguous across API groups — two operators on one cluster would otherwise
// need a fully-qualified `perconaxtradbclusterbackups.pxc.percona.com`.
type k3dBackupKind struct {
	APIVersion  string // pxc.percona.com/v1
	BackupKind  string // PerconaXtraDBClusterBackup
	BackupRes   string // pxc-backup
	RestoreKind string // PerconaXtraDBClusterRestore
	RestoreRes  string // pxc-restore
	// ClusterField is the spec field naming the database cluster. The four operators managed to
	// pick three different names for it.
	ClusterField string // pxcCluster | clusterName | pgCluster
	// StorageField is the spec field naming where the backup goes: `storageName` for the three
	// that keep a spec.backup.storages map, `repoName` for PG's pgBackRest repositories.
	StorageField string
	// Finalizer reports whether deleting the backup object can be made to delete the data too.
	// PG is the exception: pgBackRest owns its repository's retention and the operator does not
	// reach into it, so a PerconaPGBackup is only ever a record.
	Finalizer bool
	// RestoreByName reports whether a restore can name a backup object (`spec.backupName`). PG
	// restores a *repository* — `repoName` plus pgBackRest options — and has no such field.
	RestoreByName bool
}

// k3dBackupKinds is the four Percona operators, and only them. The two community PostgreSQL
// operators DBCanvas can also install (CloudNativePG, Crunchy PGO) are Helm-installed and model
// backups differently enough that sharing this panel would be a lie; they are gated out in the UI
// the same way they are gated out of the cr.yaml editor.
//
// Verified against the backup.yaml / restore.yaml each release ships in deploy/.
var k3dBackupKinds = map[string]k3dBackupKind{
	"pxc": {
		APIVersion: "pxc.percona.com/v1",
		BackupKind: "PerconaXtraDBClusterBackup", BackupRes: "pxc-backup",
		RestoreKind: "PerconaXtraDBClusterRestore", RestoreRes: "pxc-restore",
		ClusterField: "pxcCluster", StorageField: "storageName",
		Finalizer: true, RestoreByName: true,
	},
	"ps": {
		APIVersion: "ps.percona.com/v1",
		BackupKind: "PerconaServerMySQLBackup", BackupRes: "ps-backup",
		RestoreKind: "PerconaServerMySQLRestore", RestoreRes: "ps-restore",
		ClusterField: "clusterName", StorageField: "storageName",
		Finalizer: true, RestoreByName: true,
	},
	"psmdb": {
		APIVersion: "psmdb.percona.com/v1",
		BackupKind: "PerconaServerMongoDBBackup", BackupRes: "psmdb-backup",
		RestoreKind: "PerconaServerMongoDBRestore", RestoreRes: "psmdb-restore",
		ClusterField: "clusterName", StorageField: "storageName",
		Finalizer: true, RestoreByName: true,
	},
	"pg": {
		APIVersion: "pgv2.percona.com/v2",
		BackupKind: "PerconaPGBackup", BackupRes: "pg-backup",
		RestoreKind: "PerconaPGRestore", RestoreRes: "pg-restore",
		ClusterField: "pgCluster", StorageField: "repoName",
		Finalizer: false, RestoreByName: false,
	},
}

// k3dDeleteBackupFinalizer makes the operator clear the object storage when the backup object is
// deleted. cr.yaml's shipped backup.yaml carries it commented out, which is the right default and
// also the reason deleting a backup in the panel usually leaves the data exactly where it was.
const k3dDeleteBackupFinalizer = "percona.com/delete-backup"

// k3dBackupDir is where every manifest this panel applies is archived, inside the operator release
// the cluster was installed from. PXC, PS and PSMDB already ship a deploy/backup/ holding their
// own backup.yaml and restore.yaml samples, so the panel's files land beside the documentation
// they are an instance of; PG ships its samples at deploy/ and the directory is created.
const k3dBackupDir = "deploy/backup"

// k3dBackupObjectName is the naming scheme for everything the panel creates: the cluster, what it
// is, and when — sortable, unique per second, and a legal Kubernetes name. The timestamp is UTC
// because the k3s node, the browser and the person reading the bucket listing are rarely in the
// same zone.
func k3dBackupObjectName(cluster, verb string, at time.Time) string {
	return fmt.Sprintf("%s-%s-%s", cluster, verb, at.UTC().Format("20060102-150405"))
}

// k3dBackupNameRe is what the panel will accept as the name of an object it is about to act on.
// Every name it offers came from a listing it did itself, but the request is still a request.
var k3dBackupNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?$`)

// k3dBackupRow is one backup or restore object, flattened to what a table needs.
//
// The status fields are read positionally out of whatever the operator wrote rather than typed per
// operator: all four keep `.status.state`, three keep `.status.destination`, and the one that
// reports its failure in `.status.comments` instead of `.status.error` is a difference worth
// absorbing here rather than in four almost-identical structs.
type k3dBackupRow struct {
	Name        string `json:"name"`
	State       string `json:"state"`                 // Succeeded | Failed | Running | Starting | …
	Storage     string `json:"storage,omitempty"`     // storageName / repoName, as the spec asked for
	Destination string `json:"destination,omitempty"` // s3://bucket/key — where it actually went
	Backup      string `json:"backup,omitempty"`      // restores only: the backup object restored
	Created     string `json:"created,omitempty"`     // metadata.creationTimestamp
	Completed   string `json:"completed,omitempty"`   //
	Error       string `json:"error,omitempty"`       // status.error / status.comments
	Type        string `json:"type,omitempty"`        // logical | physical (PSMDB), full (PG)
	// Retained reports that this object carries the delete-backup finalizer, i.e. deleting it
	// will also clear the storage. It is the single most consequential bit on the row.
	Retained bool `json:"retained,omitempty"`
}

// k3dRawBackup is the part of a backup or restore object this file reads. Everything is a pointer
// or a string with an omitempty twin because the four operators populate slightly different
// subsets, and an absent field must render as absent rather than as empty.
type k3dRawBackup struct {
	Metadata struct {
		Name              string   `json:"name"`
		CreationTimestamp string   `json:"creationTimestamp"`
		Finalizers        []string `json:"finalizers"`
	} `json:"metadata"`
	Spec struct {
		StorageName string `json:"storageName"`
		RepoName    string `json:"repoName"`
		BackupName  string `json:"backupName"`
		Type        string `json:"type"`
	} `json:"spec"`
	Status struct {
		State       string `json:"state"`
		Destination string `json:"destination"`
		Completed   string `json:"completed"`
		Error       string `json:"error"`
		Comments    string `json:"comments"`
		Type        string `json:"type"`
		// BackupType is PG's name for the same thing: a PerconaPGBackup reports `full`,
		// `incr` or `diff` in status.backupType and has no status.type at all.
		BackupType string      `json:"backupType"`
		Repo       k3dRepoName `json:"repo"`
	} `json:"status"`
}

// k3dRepoName is the name of a backup repository as it appears in a status, whichever of the two
// shapes an operator wrote it in.
//
// The PG operator writes status.repo as the whole pgBackRest repository — `{"name":"repo1","s3":
// {...},"schedules":{...}}` — not as its name. Reading it as a string made ONE such object fail to
// unmarshal, and because the list is decoded in a single pass, one object taking an object where a
// string was expected emptied the entire backups table with "the cluster did not answer with a
// list of objects". Anything that is not an object is taken as the name itself, so an operator
// that does write a bare string keeps working.
type k3dRepoName string

func (n *k3dRepoName) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*n = k3dRepoName(s)
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		// A repo in a shape neither branch understands is not worth failing the whole listing
		// over: the storage column is one cell, and the rest of the row is still true.
		return nil
	}
	*n = k3dRepoName(obj.Name)
	return nil
}

// k3dBackupRowFrom flattens one object. isRestore picks which of the two shapes to read the
// cross-reference from.
func k3dBackupRowFrom(raw k3dRawBackup, isRestore bool) k3dBackupRow {
	row := k3dBackupRow{
		Name:        raw.Metadata.Name,
		State:       raw.Status.State,
		Destination: raw.Status.Destination,
		Created:     raw.Metadata.CreationTimestamp,
		Completed:   raw.Status.Completed,
		Error:       orDefault(raw.Status.Error, raw.Status.Comments),
		Type:        orDefault(raw.Spec.Type, orDefault(raw.Status.Type, raw.Status.BackupType)),
	}
	// The spec is what was asked for and the status what happened; for the destination the status
	// is the only truth, but for the storage the spec is, because PG reports its repo in the
	// status and the other three do not report one at all.
	row.Storage = orDefault(orDefault(raw.Spec.StorageName, raw.Spec.RepoName), string(raw.Status.Repo))
	if isRestore {
		row.Backup = raw.Spec.BackupName
	}
	for _, f := range raw.Metadata.Finalizers {
		if f == k3dDeleteBackupFinalizer {
			row.Retained = true
		}
	}
	// An object the operator has not looked at yet has no state at all, and a blank cell in a
	// table reads as a bug rather than as a fact about the cluster.
	if row.State == "" {
		row.State = "Pending"
	}
	return row
}

// parseK3DBackupList flattens a `kubectl get … -o json` list, newest first. Sorting is by name,
// which for everything this panel creates is the same as by time — and for an object created by
// hand outside the panel, alphabetical is at least stable.
func parseK3DBackupList(out []byte, isRestore bool) ([]k3dBackupRow, error) {
	var list struct {
		Items []k3dRawBackup `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		// The reason is part of the message: "did not answer with a list of objects" on its own
		// sends the reader to the cluster, when what actually went wrong is here — a field an
		// operator writes in a shape this file does not read yet.
		return nil, fmt.Errorf("the cluster did not answer with a list of objects this panel can read: %v", err)
	}
	rows := make([]k3dBackupRow, 0, len(list.Items))
	for _, it := range list.Items {
		rows = append(rows, k3dBackupRowFrom(it, isRestore))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name > rows[j].Name })
	return rows, nil
}

// ---------------------------------------------------------------- manifests

// k3dBackupManifest is the document a "take a backup" applies, and the one archived for it.
//
// It is written as text rather than marshalled from a struct on purpose: it is meant to be read,
// edited and re-applied by hand from the node, so it keeps the field order of the sample the
// release ships and carries the commented-out finalizer the sample carries — which is the one
// piece of a backup manifest that changes what deleting it later will do.
func k3dBackupManifest(k k3dBackupKind, name, cluster, storage string, retain bool) string {
	fin := "#  finalizers:\n#    - " + k3dDeleteBackupFinalizer + "\n"
	if retain {
		fin = "  finalizers:\n    - " + k3dDeleteBackupFinalizer + "\n"
	}
	if !k.Finalizer {
		fin = ""
	}
	return fmt.Sprintf(`apiVersion: %s
kind: %s
metadata:
%s  name: %s
spec:
  %s: %s
  %s: %s
`, k.APIVersion, k.BackupKind, fin, name, k.ClusterField, cluster, k.StorageField, storage)
}

// k3dRestoreManifest is the document a restore applies.
//
// Three operators name a backup object and the operator goes and finds where it went. PG does not:
// a PerconaPGRestore names a pgBackRest *repository* and hands the rest to pgBackRest as options,
// so restoring "that backup" there means `--set=<pgBackRest backup label>`, which is a different
// identifier from the PerconaPGBackup object's name. When the caller has one, it goes in; when it
// does not, the manifest restores the latest backup in the repo, which is what pgBackRest does
// with no --set at all.
func k3dRestoreManifest(k k3dBackupKind, name, cluster, backup, storage string, opts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\nspec:\n  %s: %s\n",
		k.APIVersion, k.RestoreKind, name, k.ClusterField, cluster)
	if k.RestoreByName {
		fmt.Fprintf(&b, "  backupName: %s\n", backup)
		return b.String()
	}
	fmt.Fprintf(&b, "  %s: %s\n", k.StorageField, storage)
	if len(opts) > 0 {
		b.WriteString("  options:\n")
		for _, o := range opts {
			fmt.Fprintf(&b, "  - %q\n", o)
		}
	}
	return b.String()
}

// k3dBackupArchive writes one manifest into the operator's source tree and returns the path it
// landed at. The header is the point of the exercise: the file is a complete instruction for doing
// this again without the panel, which is what makes the panel safe to stop using.
//
// A failure to archive does NOT fail the operation. The manifest has already been shown to the
// caller and is about to be applied; refusing to act because a note about it could not be filed
// would be the wrong trade, and the returned error is reported as a warning instead.
func (a *App) k3dBackupArchive(ctx context.Context, serverID, src, ns, file, what, manifest string) (string, error) {
	if src == "" {
		return "", fmt.Errorf("this cluster has no operator source tree on the node")
	}
	dir := src + "/" + k3dBackupDir
	// The directory exists in three of the four release tarballs; PG keeps its samples one level
	// up. mkdir -p rather than a check: the k3s image is busybox and this costs nothing.
	if _, err := a.engCtx(ctx).Exec(ctx, serverID, []string{"mkdir", "-p", dir}, nil); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	header := fmt.Sprintf(`# %s
#
# Written by DBCanvas on %s. Apply it again, or edit it and apply that, with:
#
#   kubectl apply -n %s -f %s/%s
#
# and watch what the operator makes of it with:
#
#   kubectl -n %s get %s -w
#
`, what, time.Now().UTC().Format(time.RFC3339), ns, dir, file, ns, strings.SplitN(what, " ", 2)[0])
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, dir, file, 0o644, []byte(header+manifest)); err != nil {
		return "", fmt.Errorf("write %s/%s: %w", dir, file, err)
	}
	return dir + "/" + file, nil
}

// ---------------------------------------------------------------- request plumbing

// k3dBackupCtx resolves a request to a frame whose operator has backup and restore custom
// resources, and writes its own errors. Every handler in this file starts with it.
func (a *App) k3dBackupCtx(w http.ResponseWriter, r *http.Request) (Deployment, k3dConfig, k3dBackupKind, bool) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return Deployment{}, k3dConfig{}, k3dBackupKind{}, false
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)
	kind, known := k3dBackupKinds[cfg.Operator]
	if !known {
		writeErr(w, http.StatusConflict, "this frame's operator has no backup custom resources DBCanvas knows about")
		return Deployment{}, k3dConfig{}, k3dBackupKind{}, false
	}
	return dep, cfg, kind, true
}

// k3dBackupStorageOf is the storage a new backup should name: what the cluster was actually
// deployed against, falling back to the name cr.yaml was rewritten to use. The fallback matters
// for a cluster deployed before these fields were recorded — it is the same constant that
// k3dcr.go writes into every DBCanvas cr.yaml.
func k3dBackupStorageOf(cfg k3dConfig) string {
	if cfg.BackupStorage != "" {
		return cfg.BackupStorage
	}
	if cfg.Operator == "pg" {
		return pgBackRestRepo
	}
	return crStorageName
}

// handleK3DBackups lists the cluster's backups and its restores in one answer, with the store they
// go to. One request rather than two: the panel shows both tables at once, and a restore is only
// meaningful beside the backup it names.
func (a *App) handleK3DBackups(w http.ResponseWriter, r *http.Request) {
	dep, cfg, kind, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	ns := cfg.Namespace
	resp := map[string]any{
		"operator": cfg.Operator, "namespace": ns, "cluster": cfg.ClusterName,
		"backupKind": kind.BackupKind, "backupResource": kind.BackupRes,
		"restoreKind": kind.RestoreKind, "restoreResource": kind.RestoreRes,
		"storage": k3dBackupStorageOf(cfg), "storageField": kind.StorageField,
		"finalizer": kind.Finalizer, "restoreByName": kind.RestoreByName,
		"repo": cfg.BackupRepo, "bucket": cfg.BackupBucket, "endpoint": cfg.BackupEndpoint,
		"secret": cfg.BackupSecret, "manifestDir": cfg.OperatorSrc + "/" + k3dBackupDir,
	}
	// A missing CRD and an empty list are different answers, and the one that means "this cluster
	// cannot do backups at all" must not render as "no backups yet".
	for _, spec := range []struct {
		key     string
		res     string
		restore bool
	}{{"backups", kind.BackupRes, false}, {"restores", kind.RestoreRes, true}} {
		out, err := a.kubectl(r.Context(), dep.ContainerID, "-n", ns, "get", spec.res, "-o", "json")
		if err != nil {
			resp[spec.key+"Error"] = lastLines(err.Error(), 200)
			continue
		}
		rows, perr := parseK3DBackupList([]byte(out), spec.restore)
		if perr != nil {
			resp[spec.key+"Error"] = perr.Error()
			continue
		}
		resp[spec.key] = rows
	}
	writeJSON(w, http.StatusOK, resp)
}

// k3dBackupCreateRequest is "take a backup now".
type k3dBackupCreateRequest struct {
	// Name is optional; the timestamped default is what the panel sends.
	Name string `json:"name"`
	// Storage overrides where it goes — the storages entry, or the pgBackRest repo. Empty means
	// the one the cluster was deployed with.
	Storage string `json:"storage"`
	// Retain sets the delete-backup finalizer, so deleting this object later also clears the
	// bucket. Off by default, which is how the operators ship it.
	Retain bool `json:"retain"`
	// DryRun builds and archives nothing and applies nothing — it returns the manifest that would
	// be applied, so the panel can show it before anything happens.
	DryRun bool `json:"dryRun"`
}

// handleK3DBackupCreate applies a Backup custom resource and archives the manifest it applied.
func (a *App) handleK3DBackupCreate(w http.ResponseWriter, r *http.Request) {
	dep, cfg, kind, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	var req k3dBackupCreateRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = k3dBackupObjectName(cfg.ClusterName, "backup", time.Now())
	}
	if !k3dBackupNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "the backup name must be a Kubernetes name: lowercase letters, digits, '-' and '.'")
		return
	}
	storage := strings.TrimSpace(req.Storage)
	if storage == "" {
		storage = k3dBackupStorageOf(cfg)
	}
	if !k3dBackupNameRe.MatchString(storage) {
		writeErr(w, http.StatusBadRequest, "the storage name must be a Kubernetes name")
		return
	}
	manifest := k3dBackupManifest(kind, name, cfg.ClusterName, storage, req.Retain)
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "dryRun": true, "name": name, "manifest": manifest,
			"message": "nothing has been applied — this is the manifest that would be",
		})
		return
	}
	file := name + ".yaml"
	path, aerr := a.k3dBackupArchive(r.Context(), dep.ContainerID, cfg.OperatorSrc, cfg.Namespace, file,
		kind.BackupRes+" "+name+" — take a backup of "+cfg.ClusterName, manifest)
	if err := a.kubectlApply(r.Context(), dep.ContainerID, cfg.Namespace, []byte(manifest)); err != nil {
		writeErr(w, http.StatusBadGateway, "apply the backup: "+lastLines(err.Error(), 600))
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf("%s %s/%s created from the panel (%s %s)",
		kind.BackupKind, cfg.Namespace, name, kind.StorageField, storage))
	writeJSON(w, http.StatusOK, k3dBackupResult(name, manifest, path, aerr, kind.BackupRes, cfg.Namespace,
		"the operator has been asked for a backup — watch its state in the table"))
}

// k3dBackupResult is the answer every mutating handler here gives: what it did, the document it
// did it with, where that document was filed, and the one command that shows what happens next.
func k3dBackupResult(name, manifest, path string, aerr error, res, ns, message string) map[string]any {
	out := map[string]any{
		"ok": true, "name": name, "manifest": manifest, "message": message,
		"watch": fmt.Sprintf("kubectl -n %s get %s %s -o yaml -w", ns, res, name),
	}
	if path != "" {
		out["archived"] = path
		out["apply"] = fmt.Sprintf("kubectl apply -n %s -f %s", ns, path)
	}
	if aerr != nil {
		out["warning"] = "the manifest was applied but could not be archived: " + aerr.Error()
	}
	return out
}

// k3dBackupDeleteRequest is "remove this object", with the one question that matters attached.
type k3dBackupDeleteRequest struct {
	Name string `json:"name"`
	// Data deletes what is in the object store as well, by putting the delete-backup finalizer on
	// the object before removing it. Without it the bucket keeps every byte and only the record
	// goes — which is the operators' own default, and a thing worth being able to demonstrate.
	Data bool `json:"data"`
}

// handleK3DBackupDelete deletes a backup object, and optionally what it wrote.
//
// The finalizer is patched on (or off) immediately before the delete rather than assumed from how
// the object was created: a backup taken by a schedule, by the replication seed, or by hand has
// whatever finalizer that path gave it, and "delete the data too" must mean the same thing for all
// of them. Patching it off matters just as much — a backup created with the finalizer would
// otherwise take the bucket contents with it when someone only meant to tidy the list.
func (a *App) handleK3DBackupDelete(w http.ResponseWriter, r *http.Request) {
	dep, cfg, kind, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	var req k3dBackupDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !k3dBackupNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "the backup name must be a Kubernetes name")
		return
	}
	steps := []string{}
	if kind.Finalizer {
		fin := "[]"
		if req.Data {
			fin = fmt.Sprintf("[%q]", k3dDeleteBackupFinalizer)
		}
		patch := fmt.Sprintf(`{"metadata":{"finalizers":%s}}`, fin)
		if _, err := a.kubectlQuiet(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "patch",
			kind.BackupRes, name, "--type", "merge", "-p", patch); err != nil {
			writeErr(w, http.StatusBadGateway, "set the delete-backup finalizer: "+lastLines(err.Error(), 400))
			return
		}
		steps = append(steps, fmt.Sprintf("kubectl -n %s patch %s %s --type merge -p '%s'",
			cfg.Namespace, kind.BackupRes, name, patch))
	} else if req.Data {
		// PG: say so rather than delete the record and let the user believe the repository shrank.
		writeErr(w, http.StatusBadRequest,
			"the PostgreSQL operator does not delete pgBackRest data with the backup object — "+
				"expire it from the repository instead, or delete the objects in the bucket")
		return
	}
	if _, err := a.kubectlQuiet(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "delete",
		kind.BackupRes, name, "--ignore-not-found"); err != nil {
		writeErr(w, http.StatusBadGateway, "delete "+name+": "+lastLines(err.Error(), 400))
		return
	}
	steps = append(steps, fmt.Sprintf("kubectl -n %s delete %s %s", cfg.Namespace, kind.BackupRes, name))
	what := "the object only — the storage keeps what it wrote"
	if req.Data {
		what = "the object AND what it wrote to the storage"
	}
	a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf("%s %s/%s deleted from the panel: %s",
		kind.BackupKind, cfg.Namespace, name, what))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name, "deleted": what, "steps": steps,
		"message": "deleted " + what,
	})
}

// k3dRestoreRequest is "put this backup back".
type k3dRestoreRequest struct {
	// Backup names the backup object to restore (PXC, PS, PSMDB).
	Backup string `json:"backup"`
	// Name is the restore object's own name; the timestamped default is what the panel sends.
	Name string `json:"name"`
	// Storage is PG's repository. Ignored by the three that restore by name.
	Storage string `json:"storage"`
	// Options are pgBackRest options for a PG restore — `--set=<label>` to pick a specific
	// backup, `--type=time --target=…` for point-in-time. Ignored by the other three.
	Options []string `json:"options"`
	DryRun  bool     `json:"dryRun"`
}

// handleK3DRestore applies a Restore custom resource and archives the manifest it applied.
func (a *App) handleK3DRestore(w http.ResponseWriter, r *http.Request) {
	dep, cfg, kind, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	var req k3dRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	backup := strings.TrimSpace(req.Backup)
	if kind.RestoreByName && !k3dBackupNameRe.MatchString(backup) {
		writeErr(w, http.StatusBadRequest, "a restore needs the name of a backup object")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = k3dBackupObjectName(cfg.ClusterName, "restore", time.Now())
	}
	if !k3dBackupNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "the restore name must be a Kubernetes name")
		return
	}
	storage := orDefault(strings.TrimSpace(req.Storage), k3dBackupStorageOf(cfg))
	if !k3dBackupNameRe.MatchString(storage) {
		writeErr(w, http.StatusBadRequest, "the storage name must be a Kubernetes name")
		return
	}
	opts, err := k3dRestoreOptions(req.Options)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	manifest := k3dRestoreManifest(kind, name, cfg.ClusterName, backup, storage, opts)
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "dryRun": true, "name": name, "manifest": manifest,
			"message": "nothing has been applied — this is the manifest that would be",
		})
		return
	}
	file := name + ".yaml"
	src := backup
	if !kind.RestoreByName {
		src = storage + " " + strings.Join(opts, " ")
	}
	path, aerr := a.k3dBackupArchive(r.Context(), dep.ContainerID, cfg.OperatorSrc, cfg.Namespace, file,
		kind.RestoreRes+" "+name+" — restore "+cfg.ClusterName+" from "+strings.TrimSpace(src), manifest)
	if err := a.kubectlApply(r.Context(), dep.ContainerID, cfg.Namespace, []byte(manifest)); err != nil {
		writeErr(w, http.StatusBadGateway, "apply the restore: "+lastLines(err.Error(), 600))
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf(
		"%s %s/%s created from the panel — %s is being restored from %s; it stops serving while this runs",
		kind.RestoreKind, cfg.Namespace, name, cfg.ClusterName, strings.TrimSpace(src)))
	writeJSON(w, http.StatusOK, k3dBackupResult(name, manifest, path, aerr, kind.RestoreRes, cfg.Namespace,
		"the restore has been created — the cluster pauses while the operator runs it"))
}

// k3dRestoreOptionRe is what a pgBackRest option may look like. It is deliberately narrow: these
// strings are written into a YAML document that becomes a container's argv, and the panel only
// ever needs --flag and --flag=value.
var k3dRestoreOptionRe = regexp.MustCompile(`^--[a-z][a-z0-9-]*(=[^\n"'$` + "`" + `\\]{1,200})?$`)

// k3dRestoreOptions validates the pgBackRest options a PG restore may carry.
func k3dRestoreOptions(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, o := range in {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if len(out) >= 8 {
			return nil, fmt.Errorf("at most 8 restore options")
		}
		if !k3dRestoreOptionRe.MatchString(o) {
			return nil, fmt.Errorf("%q is not a pgBackRest option — expected --flag or --flag=value", o)
		}
		out = append(out, o)
	}
	return out, nil
}

// handleK3DRestoreDelete removes a restore object. It only ever removes the record: a restore that
// has run has already replaced the cluster's data, and deleting the object does not put it back.
func (a *App) handleK3DRestoreDelete(w http.ResponseWriter, r *http.Request) {
	dep, cfg, kind, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	var req k3dBackupDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !k3dBackupNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "the restore name must be a Kubernetes name")
		return
	}
	if _, err := a.kubectlQuiet(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "delete",
		kind.RestoreRes, name, "--ignore-not-found"); err != nil {
		writeErr(w, http.StatusBadGateway, "delete "+name+": "+lastLines(err.Error(), 400))
		return
	}
	a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf("%s %s/%s deleted from the panel (the record only)",
		kind.RestoreKind, cfg.Namespace, name))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name,
		"steps":   []string{fmt.Sprintf("kubectl -n %s delete %s %s", cfg.Namespace, kind.RestoreRes, name)},
		"message": "the restore record is gone — the data it restored is not affected",
	})
}

// handleK3DBackupManifests lists what the panel has filed in deploy/backup/, and reads one file.
//
// This is the tab's receipt: everything that was ever applied from here, on the node, in the
// operator's own tree. Without a name it lists; with one it returns that file's contents.
func (a *App) handleK3DBackupManifests(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	if cfg.OperatorSrc == "" {
		writeErr(w, http.StatusConflict, "this cluster has no operator source tree on the node")
		return
	}
	dir := cfg.OperatorSrc + "/" + k3dBackupDir
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		// `ls -1` of a directory that does not exist yet is an empty list, not an error: the tab
		// is openable before anything has been applied from it.
		res, err := a.engCtx(r.Context()).Exec(r.Context(), dep.ContainerID,
			[]string{"sh", "-c", "ls -1 \"$DIR\" 2>/dev/null || true"}, []string{"DIR=" + dir})
		if err != nil {
			writeErr(w, http.StatusBadGateway, "list "+dir+": "+err.Error())
			return
		}
		var files []string
		for _, l := range strings.Split(res.Stdout, "\n") {
			if l = strings.TrimSpace(l); strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml") {
				files = append(files, l)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
		writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "files": files})
		return
	}
	// The name comes from the listing above, but it arrives as a request: anything with a path
	// separator in it is refused rather than cleaned, so a traversal is visible as a refusal.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") || len(name) > 128 {
		writeErr(w, http.StatusBadRequest, "not a file in "+dir)
		return
	}
	res, err := a.engCtx(r.Context()).Exec(r.Context(), dep.ContainerID,
		[]string{"sh", "-c", "cat \"$DIR/$NAME\""}, []string{"DIR=" + dir, "NAME=" + name})
	if err != nil || res.Code != 0 {
		writeErr(w, http.StatusNotFound, "no such manifest: "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "name": name, "content": res.Stdout})
}
