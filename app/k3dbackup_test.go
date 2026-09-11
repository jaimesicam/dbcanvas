package main

import (
	"strings"
	"testing"
	"time"
)

// The four operators are the point of the kind table: every one of them must be able to describe
// a backup and a restore, or the panel silently offers a tab that cannot do anything.
func TestBackupKindsCoverEveryPerconaOperator(t *testing.T) {
	for _, op := range []string{"pxc", "ps", "psmdb", "pg"} {
		k, ok := k3dBackupKinds[op]
		if !ok {
			t.Fatalf("%s: no backup kinds", op)
		}
		if k.APIVersion == "" || k.BackupKind == "" || k.BackupRes == "" ||
			k.RestoreKind == "" || k.RestoreRes == "" || k.ClusterField == "" || k.StorageField == "" {
			t.Errorf("%s: incomplete kind table: %+v", op, k)
		}
	}
	// The cr.yaml editor is gated on exactly these four (CR_EDITABLE in K3DManager.jsx). A fifth
	// entry here would mean a tab offered for an operator whose cr.yaml cannot be read.
	if len(k3dBackupKinds) != 4 {
		t.Errorf("k3dBackupKinds has %d operators, want the four Percona ones", len(k3dBackupKinds))
	}
	// Field names verified against the backup.yaml / restore.yaml each release ships. Getting one
	// wrong produces a manifest the API server accepts (unknown fields are pruned) and an object
	// the operator never acts on, which is the worst possible failure mode: a backup that silently
	// never happens.
	for op, want := range map[string][2]string{
		"pxc":   {"pxcCluster", "storageName"},
		"ps":    {"clusterName", "storageName"},
		"psmdb": {"clusterName", "storageName"},
		"pg":    {"pgCluster", "repoName"},
	} {
		k := k3dBackupKinds[op]
		if k.ClusterField != want[0] || k.StorageField != want[1] {
			t.Errorf("%s: fields are %q/%q, want %q/%q", op, k.ClusterField, k.StorageField, want[0], want[1])
		}
	}
}

// PG is the one that cannot delete its data with the object and cannot restore by naming one.
// Both flags drive a refusal in the handlers, so a regression here changes what the panel offers.
func TestPGBackupsDifferFromTheRest(t *testing.T) {
	if k3dBackupKinds["pg"].Finalizer {
		t.Error("PG: the operator does not clear pgBackRest data with the backup object")
	}
	if k3dBackupKinds["pg"].RestoreByName {
		t.Error("PG: a PerconaPGRestore names a repository, not a backup object")
	}
	for _, op := range []string{"pxc", "ps", "psmdb"} {
		if !k3dBackupKinds[op].Finalizer || !k3dBackupKinds[op].RestoreByName {
			t.Errorf("%s: should support the delete-backup finalizer and restore-by-name", op)
		}
	}
}

func TestBackupManifestNamesTheClusterAndStorage(t *testing.T) {
	k := k3dBackupKinds["pxc"]
	got := k3dBackupManifest(k, "cluster1-backup-20260911-120000", "cluster1", "seaweedfs", false)
	for _, want := range []string{
		"apiVersion: pxc.percona.com/v1",
		"kind: PerconaXtraDBClusterBackup",
		"name: cluster1-backup-20260911-120000",
		"pxcCluster: cluster1",
		"storageName: seaweedfs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest is missing %q:\n%s", want, got)
		}
	}
	// Without retain the finalizer must be present but COMMENTED OUT — that is how the release's
	// own backup.yaml ships it, and it is what makes a later delete leave the bucket alone.
	if !strings.Contains(got, "#    - "+k3dDeleteBackupFinalizer) {
		t.Errorf("the finalizer should be commented out by default:\n%s", got)
	}
	if strings.Contains(got, "\n  finalizers:") {
		t.Errorf("an unretained backup must not carry a live finalizer:\n%s", got)
	}
}

func TestBackupManifestRetainArmsTheFinalizer(t *testing.T) {
	got := k3dBackupManifest(k3dBackupKinds["psmdb"], "b1", "cluster1", "seaweedfs", true)
	if !strings.Contains(got, "\n  finalizers:\n    - "+k3dDeleteBackupFinalizer) {
		t.Errorf("retain should arm the finalizer:\n%s", got)
	}
}

// PG has no finalizer at all, so neither the live nor the commented form belongs in its manifest.
func TestPGBackupManifestHasNoFinalizer(t *testing.T) {
	got := k3dBackupManifest(k3dBackupKinds["pg"], "b1", "cluster1", "repo1", true)
	if strings.Contains(got, "finalizers") {
		t.Errorf("PG manifests must not mention finalizers:\n%s", got)
	}
	if !strings.Contains(got, "repoName: repo1") || !strings.Contains(got, "pgCluster: cluster1") {
		t.Errorf("PG manifest is wrong:\n%s", got)
	}
}

func TestRestoreManifestByNameAndByRepo(t *testing.T) {
	byName := k3dRestoreManifest(k3dBackupKinds["pxc"], "r1", "cluster1", "b1", "seaweedfs", nil)
	if !strings.Contains(byName, "backupName: b1") || !strings.Contains(byName, "pxcCluster: cluster1") {
		t.Errorf("a PXC restore names the backup object:\n%s", byName)
	}
	// The storage must NOT appear: a PerconaXtraDBClusterRestore has no storageName, and a pruned
	// unknown field would hide the mistake rather than report it.
	if strings.Contains(byName, "storageName") {
		t.Errorf("a restore-by-name must not carry a storage:\n%s", byName)
	}

	byRepo := k3dRestoreManifest(k3dBackupKinds["pg"], "r1", "cluster1", "", "repo1",
		[]string{"--type=immediate", "--set=20260911-120000F"})
	for _, want := range []string{"repoName: repo1", "options:", `"--type=immediate"`, `"--set=20260911-120000F"`} {
		if !strings.Contains(byRepo, want) {
			t.Errorf("a PG restore is missing %q:\n%s", want, byRepo)
		}
	}
	if strings.Contains(byRepo, "backupName") {
		t.Errorf("PG has no backupName field:\n%s", byRepo)
	}
}

func TestRestoreOptionsRefuseAnythingButFlags(t *testing.T) {
	ok, err := k3dRestoreOptions([]string{"--type=time", "  ", "--target=2026-09-11 12:00:00+00"})
	if err != nil {
		t.Fatalf("valid options refused: %v", err)
	}
	if len(ok) != 2 {
		t.Errorf("blank options should be dropped, got %q", ok)
	}
	for _, bad := range []string{
		"; rm -rf /",
		"--set=$(whoami)",
		"--set=`id`",
		`--set="quoted"`,
		"--set=a\nb",
		"set=novalue",
	} {
		if _, err := k3dRestoreOptions([]string{bad}); err == nil {
			t.Errorf("%q should have been refused", bad)
		}
	}
	many := make([]string, 9)
	for i := range many {
		many[i] = "--type=full"
	}
	if _, err := k3dRestoreOptions(many); err == nil {
		t.Error("nine options should have been refused")
	}
}

func TestBackupObjectNameIsSortableAndLegal(t *testing.T) {
	at := time.Date(2026, 9, 11, 12, 34, 56, 0, time.UTC)
	got := k3dBackupObjectName("cluster1", "backup", at)
	if got != "cluster1-backup-20260911-123456" {
		t.Errorf("got %q", got)
	}
	if !k3dBackupNameRe.MatchString(got) {
		t.Errorf("%q is not a legal Kubernetes name", got)
	}
	// Sortable by name means sortable by time, which is what the listing relies on.
	later := k3dBackupObjectName("cluster1", "backup", at.Add(time.Second))
	if !(got < later) {
		t.Errorf("%q should sort before %q", got, later)
	}
	// The timestamp is UTC wherever the node thinks it is.
	east := time.FixedZone("UTC+8", 8*3600)
	if k3dBackupObjectName("cluster1", "backup", at.In(east)) != got {
		t.Error("the name must not depend on the caller's zone")
	}
}

func TestBackupNamesAreChecked(t *testing.T) {
	for _, bad := range []string{"", "-leading", "trailing-", "Upper", "has space", "a/b", "a;b", strings.Repeat("a", 64)} {
		if k3dBackupNameRe.MatchString(bad) {
			t.Errorf("%q should have been refused as a name", bad)
		}
	}
	for _, good := range []string{"b1", "cluster1-backup-20260911-123456", "a.b.c", "a1"} {
		if !k3dBackupNameRe.MatchString(good) {
			t.Errorf("%q should have been accepted as a name", good)
		}
	}
}

func TestParseBackupListReadsStateAndFinalizer(t *testing.T) {
	rows, err := parseK3DBackupList([]byte(`{"items":[
      {"metadata":{"name":"b1","creationTimestamp":"2026-09-11T12:00:00Z",
                   "finalizers":["percona.com/delete-backup"]},
       "spec":{"storageName":"seaweedfs"},
       "status":{"state":"Succeeded","destination":"s3://dbcanvas/b1","completed":"2026-09-11T12:04:00Z"}},
      {"metadata":{"name":"b2"},"spec":{"storageName":"seaweedfs"},
       "status":{"state":"Failed","error":"no such bucket"}},
      {"metadata":{"name":"b3"},"spec":{"storageName":"seaweedfs"}}
    ]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	// Newest first: the names sort in reverse, which for panel-created names is newest first.
	if rows[0].Name != "b3" || rows[2].Name != "b1" {
		t.Errorf("rows are not sorted newest-first: %v %v %v", rows[0].Name, rows[1].Name, rows[2].Name)
	}
	b1 := rows[2]
	if !b1.Retained {
		t.Error("b1 carries the delete-backup finalizer and should be reported as retained")
	}
	if b1.Destination != "s3://dbcanvas/b1" || b1.State != "Succeeded" {
		t.Errorf("b1: %+v", b1)
	}
	if rows[1].Error != "no such bucket" {
		t.Errorf("b2 should carry its error: %+v", rows[1])
	}
	// An object the operator has not reached yet reports Pending, never an empty cell.
	if rows[0].State != "Pending" {
		t.Errorf("b3 has no status and should read Pending, got %q", rows[0].State)
	}
	if rows[0].Retained {
		t.Error("b3 has no finalizer")
	}
}

// A restore reports its failure in `.status.comments` on PXC and in `.status.error` elsewhere.
// Both have to land in the same column or the table shows a failed restore with no reason.
func TestParseBackupListReadsRestoreComments(t *testing.T) {
	rows, err := parseK3DBackupList([]byte(`{"items":[
      {"metadata":{"name":"r1"},"spec":{"backupName":"b1"},
       "status":{"state":"Failed","comments":"the cluster is not ready"}}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Backup != "b1" {
		t.Errorf("a restore row should name the backup it restored: %+v", rows[0])
	}
	if rows[0].Error != "the cluster is not ready" {
		t.Errorf("status.comments should be reported as the error: %+v", rows[0])
	}
}

// PG reports its repository in the status and nothing in spec.storageName. The storage column has
// to find it wherever the operator put it.
func TestParseBackupListFindsTheStorageWhereverItIs(t *testing.T) {
	rows, _ := parseK3DBackupList([]byte(`{"items":[
      {"metadata":{"name":"b1"},"spec":{"repoName":"repo1"},"status":{"state":"Succeeded"}},
      {"metadata":{"name":"b2"},"spec":{},"status":{"state":"Succeeded","repo":"repo2"}}]}`), false)
	if rows[1].Storage != "repo1" {
		t.Errorf("spec.repoName should be the storage: %+v", rows[1])
	}
	if rows[0].Storage != "repo2" {
		t.Errorf("status.repo is the fallback: %+v", rows[0])
	}
}

// The PG operator (v2) writes status.repo as the whole pgBackRest repository object, not as a
// name. Read as a string it failed to unmarshal, and because the items decode in one pass a single
// such backup emptied the whole table. This is the shape 3.1.0 actually writes.
func TestParseBackupListReadsPGsRepoObject(t *testing.T) {
	rows, err := parseK3DBackupList([]byte(`{"items":[
      {"metadata":{"name":"k3d-00-backup-jpm9-ng4fd","creationTimestamp":"2026-09-11T17:15:56Z"},
       "spec":{"pgCluster":"k3d-00","repoName":"repo1","method":"pgbackrest"},
       "status":{"state":"Succeeded","backupType":"full","completed":"2026-09-11T17:16:19Z",
                 "destination":"s3://bucket1/pgbackrest/k3d-00/repo1",
                 "repo":{"name":"repo1","s3":{"bucket":"bucket1","region":"us-east-1"},
                         "schedules":{"full":"0 0 * * 6"}}}}]}`), false)
	if err != nil {
		t.Fatalf("a PG backup should parse, got %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Storage != "repo1" {
		t.Errorf("spec.repoName should be the storage: %+v", rows[0])
	}
	// PG names the kind of backup in status.backupType; the other three use spec.type.
	if rows[0].Type != "full" {
		t.Errorf("status.backupType should fill the type column: %+v", rows[0])
	}
	if rows[0].State != "Succeeded" || rows[0].Destination != "s3://bucket1/pgbackrest/k3d-00/repo1" {
		t.Errorf("row: %+v", rows[0])
	}
}

// A repo object with no name, and a repo in a shape neither branch understands, each cost their own
// cell and nothing else — never the listing.
func TestParseBackupListSurvivesAnUnreadableRepo(t *testing.T) {
	rows, err := parseK3DBackupList([]byte(`{"items":[
      {"metadata":{"name":"b1"},"status":{"state":"Succeeded","repo":["repo1"]}},
      {"metadata":{"name":"b2"},"status":{"state":"Succeeded","repo":{"s3":{}}}}]}`), false)
	if err != nil {
		t.Fatalf("an unreadable repo should not fail the listing, got %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, r := range rows {
		if r.Storage != "" {
			t.Errorf("%s should have an empty storage cell, got %q", r.Name, r.Storage)
		}
	}
}

func TestBackupStorageFallsBackToWhatCrYamlWrote(t *testing.T) {
	if got := k3dBackupStorageOf(k3dConfig{BackupStorage: "elsewhere"}); got != "elsewhere" {
		t.Errorf("the recorded storage wins, got %q", got)
	}
	// A cluster deployed before these fields were recorded still has to produce a working
	// manifest, and the constant below is the one k3dcr.go writes into every DBCanvas cr.yaml.
	if got := k3dBackupStorageOf(k3dConfig{Operator: "pxc"}); got != crStorageName {
		t.Errorf("PXC should fall back to %q, got %q", crStorageName, got)
	}
	if got := k3dBackupStorageOf(k3dConfig{Operator: "pg"}); got != pgBackRestRepo {
		t.Errorf("PG should fall back to %q, got %q", pgBackRestRepo, got)
	}
}
