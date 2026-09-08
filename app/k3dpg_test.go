package main

import (
	"os"
	"strings"
	"testing"
)

// pgTransform runs against the PostgreSQL operator's real cr.yaml (testdata/cr-pg.yaml, 3.0.0).
func TestPGTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	out := pgTransform(string(raw), pgOptions{
		Name:            "pg-01",
		ExposePostgres:  "ClusterIP",
		ExposePGBouncer: "LoadBalancer",
		PMMHost:         "pmm-01.example.net:8443",
		S3: &crS3{
			Bucket: "backups", Region: "us-east-1",
			EndpointURL: "https://seaweedfs-01.example.net:8333",
			Secret:      "pg-01-pgbackrest-secrets",
		},
	})

	// 1. No CPU/memory request survives — but every volume claim keeps its size. PostgreSQL spells
	//    its claims dataVolumeClaimSpec / volumeClaimSpec, not persistentVolumeClaim.
	pvc, sizes := newCRPVC(), 0
	for i, ln := range strings.Split(out, "\n") {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)
		if commented {
			continue
		}
		if body == "resources:" && !pvc.inside() {
			t.Errorf("line %d: a CPU/memory resources block is still active", i+1)
		}
		if pvc.inside() && strings.HasPrefix(body, "storage:") {
			sizes++
		}
	}
	if sizes == 0 {
		t.Error("a volume claim lost its storage request — the operator requires it")
	}

	// 2. The cluster's name, and the users whose secrets are pre-created with the .env password.
	if !strings.Contains(out, "\n  name: pg-01\n") {
		t.Error("metadata.name was not set")
	}
	if !strings.Contains(out, "  users:\n  - name: postgres\n  - name: pg-01\n    databases:\n    - pg-01") {
		t.Error("the users block was not inserted")
	}

	// 3. Expose: the primary Service and the pgBouncer pool are independent.
	if !strings.Contains(out, "  expose:\n    type: ClusterIP") {
		t.Error("the primary Postgres Service was not exposed")
	}
	if !strings.Contains(out, "    pgBouncer:\n      expose:\n        type: LoadBalancer") {
		t.Error("pgBouncer was not exposed")
	}

	// 4. PMM: enabled, its secret renamed after the cluster, and serverHost carrying the port (the
	//    operator hands it to the sidecar verbatim as PMM_AGENT_SERVER_ADDRESS).
	for _, want := range []string{"    enabled: true", "    secret: pg-01-pmm-secret", "    serverHost: pmm-01.example.net:8443"} {
		if !strings.Contains(out, want) {
			t.Errorf("PMM: missing %q", want)
		}
	}

	// 5. Backups: repo1 is the SeaweedFS bucket, not the shipped PVC, and pgBackRest's credentials
	//    and options (which have no CR field) come from the configuration secret and `global`.
	for _, want := range []string{
		"      - name: repo1",
		"        s3:\n          bucket: backups\n          endpoint: seaweedfs-01.example.net:8333",
		"      configuration:\n      - secret:\n          name: pg-01-pgbackrest-secrets",
		"        repo1-s3-uri-style: path",
		`        repo1-storage-verify-tls: "n"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("backups: missing %q", want)
		}
	}
	if strings.Contains(out, "        volume:\n          volumeClaimSpec:") {
		t.Error("the shipped PVC repo survived alongside the S3 repo")
	}
}

// With no SeaweedFS node the cluster keeps the operator's own PVC repo — pgBackRest still works,
// it just backs up to a volume.
func TestPGWithoutS3KeepsThePVCRepo(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	out := pgTransform(string(raw), pgOptions{Name: "pg-01", ExposePostgres: "LoadBalancer"})
	if !strings.Contains(out, "        volume:\n          volumeClaimSpec:") {
		t.Error("the PVC repo must survive when there is no S3 target")
	}
	// (cr.yaml's own commented examples mention both, so only active lines count.)
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented {
			continue
		}
		if strings.Contains(body, "repo1-s3-uri-style") || strings.Contains(body, "pgbackrest-secrets") {
			t.Errorf("line %d: an S3 option was emitted without an S3 target: %q", i+1, body)
		}
	}
	if strings.Contains(out, "serverHost: pmm") {
		t.Error("PMM was enabled without a PMM node")
	}
}

// The shipped secrets are renamed after the cluster, and PMM 2's key is dropped (its presence alone
// makes the operator run the PMM 2 sidecar).
func TestPGSecretsTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/secrets-pg.yaml")
	if err != nil {
		t.Skipf("no PG secrets.yaml fixture: %v", err)
	}
	out := pgSecretsTransform(string(raw), "pg-01")
	if !strings.Contains(out, "  name: pg-01-pmm-secret") || !strings.Contains(out, "  name: pg-01-extensions-secret") {
		t.Error("the shipped secrets were not renamed after the cluster")
	}
	if strings.Contains(out, "PMM_SERVER_KEY") {
		t.Error("PMM 2's key survived")
	}
}

func TestPGUserSecretCarriesTheEnvPassword(t *testing.T) {
	out := pgUserSecret("pg-01", "postgres", "s3cret")
	for _, want := range []string{"  name: pg-01-pguser-postgres", "  user: postgres", "  password: s3cret"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No verifier: the operator derives the SCRAM verifier from the password we set.
	if strings.Contains(out, "verifier") {
		t.Error("the secret must not carry a verifier — the operator builds it from the password")
	}
}
