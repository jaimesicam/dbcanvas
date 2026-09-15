package main

import (
	"os"
	"strings"
	"testing"
)

// pgTransform runs against the PostgreSQL operator's real cr.yaml (testdata/cr-pg.yaml, 3.1.0).
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

// ---------------------------------------------------------------- the 3.1.0 features

// The gate is the whole point of these three options: below 3.1.0 the CRD has no such field, so
// the API server rejects the entire cr.yaml rather than ignoring it.
func TestPGHasClusterFeatures(t *testing.T) {
	for _, tc := range []struct {
		ver  string
		want bool
	}{
		{"3.1.0", true},
		{"3.1.1", true},
		{"3.2.0", true},
		{"3.10.0", true}, // string comparison would put this below "3.2.0"
		{"4.0.0", true},
		{"3.0.0", false},
		{"2.9.0", false},
		{"2.10.0", false},
		{"", false}, // unknown is not "newest" — guessing wrong costs the whole cr.yaml
	} {
		if got := pgHasClusterFeatures(tc.ver); got != tc.want {
			t.Errorf("pgHasClusterFeatures(%q) = %v, want %v", tc.ver, got, tc.want)
		}
	}
}

// With every 3.1.0 knob on, cr.yaml grows the three sections — and the logcollector line it
// already ships is rewritten in place rather than duplicated.
func TestPGTransform310Features(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	on := true
	out := pgTransform(string(raw), pgOptions{
		Name:             "pg-01",
		LogicalReplicas:  2,
		LogicalStorageGB: 5,
		LogicalBootstrap: "pg_basebackup",
		LogCollector:     &on,
		TDE: &pgTDE{
			WALEncryption: true,
			VaultHost:     "https://bao-01.example.net:8200",
			MountPath:     "postgresql-pg-01",
			Secret:        "pg-01-pgtde-vault",
			CAKey:         "ca.crt",
		},
	})

	for _, want := range []string{
		"  logicalReplicas:\n  - name: replica1\n    databases: []\n    bootstrapMethod: pg_basebackup",
		"  - name: replica2\n",
		"    dataVolumeClaimSpec:\n      accessModes:\n      - ReadWriteOnce\n      resources:\n        requests:\n          storage: 5Gi",
		"  extensions:\n    pg_tde:\n      enabled: true\n      walEncryption: true",
		"      vault:\n        host: https://bao-01.example.net:8200\n        mountPath: postgresql-pg-01",
		"        tokenSecret:\n          name: pg-01-pgtde-vault\n          key: token",
		"        caSecret:\n          name: pg-01-pgtde-vault\n          key: ca.crt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}

	// Exactly one active spec.logcollector.enabled — the shipped line was rewritten, not joined
	// by a second one. A duplicate key is a cr.yaml the API server refuses.
	path, enabled := newYPath(), 0
	for _, ln := range strings.Split(out, "\n") {
		ind, commented, body := crLine(ln)
		path.update(ind, commented, body)
		if !commented && body != "" && path.String() == "spec.logcollector.enabled" {
			enabled++
		}
	}
	if enabled != 1 {
		t.Errorf("spec.logcollector.enabled appears %d times, want 1", enabled)
	}
}

// Turning persistent logging off rewrites the line the operator ships enabled.
func TestPGTransformLogCollectorOff(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	off := false
	out := pgTransform(string(raw), pgOptions{Name: "pg-01", LogCollector: &off})
	if !strings.Contains(out, "  logcollector:\n    enabled: false") {
		t.Error("the log collector was not turned off")
	}
}

// A frame with none of the 3.1.0 options set must leave cr.yaml exactly as it was — including the
// logcollector line, which is the one section the operator ships active.
func TestPGTransformLeaves310SectionsAloneWhenUnset(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	out := pgTransform(string(raw), pgOptions{Name: "pg-01"})
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented {
			continue
		}
		if strings.HasPrefix(body, "logicalReplicas:") || strings.HasPrefix(body, "pg_tde:") {
			t.Errorf("line %d: a 3.1.0 section was emitted for a frame that asked for none: %q", i+1, body)
		}
	}
	if !strings.Contains(out, "  logcollector:\n    enabled: true") {
		t.Error("the shipped logcollector setting was not left alone")
	}
}

// The frame knobs: their defaults, their clamps, and the one that is spelled as a negative.
func TestPGFrameKnobs(t *testing.T) {
	if got := pgLogicalReplicas(designFrame{}); got != 0 {
		t.Errorf("logical replicas default = %d, want 0", got)
	}
	if got := pgLogicalReplicas(designFrame{K3DPGLogicalReplicas: 9}); got != 3 {
		t.Errorf("logical replicas clamp = %d, want 3", got)
	}
	if got := pgLogicalStorageGB(designFrame{}); got != 1 {
		t.Errorf("logical storage default = %d, want 1", got)
	}
	if got := pgLogicalBootstrap(designFrame{}); got != "pgbackrest" {
		t.Errorf("bootstrap default = %q, want pgbackrest", got)
	}
	if got := pgLogicalBootstrap(designFrame{K3DPGLogicalBootstrap: "nonsense"}); got != "pgbackrest" {
		t.Errorf("an unknown bootstrap method must fall back to the CRD default, got %q", got)
	}
	// The negative: an untouched frame keeps the operator's own default, which is on.
	if !pgLogCollector(designFrame{}) {
		t.Error("persistent logging must default to on — that is what cr.yaml ships")
	}
	if pgLogCollector(designFrame{K3DPGNoLogCollector: true}) {
		t.Error("persistent logging was not turned off")
	}
}

// The vault Secret carries the token, and the CA only when there is TLS to verify — a caSecret
// key the Secret does not hold stops the instance pods from starting.
func TestPGTDEVaultSecret(t *testing.T) {
	withCA := pgTDEVaultSecret("pg-01-pgtde-vault", "hvs.token", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	for _, want := range []string{
		"  name: pg-01-pgtde-vault",
		"  token: hvs.token",
		"  ca.crt: |\n    -----BEGIN CERTIFICATE-----\n    AAAA\n    -----END CERTIFICATE-----",
	} {
		if !strings.Contains(withCA, want) {
			t.Errorf("missing %q in:\n%s", want, withCA)
		}
	}
	if plain := pgTDEVaultSecret("pg-01-pgtde-vault", "hvs.token", ""); strings.Contains(plain, "ca.crt") {
		t.Error("a plaintext OpenBao node must not get a ca.crt key")
	}
}

// Without WAL encryption the field is left off rather than written as false — and without a CA
// there is no caSecret at all.
func TestPGTDEBlockOmitsWhatWasNotAskedFor(t *testing.T) {
	out := pgTDEBlock(&pgTDE{VaultHost: "http://bao-01.example.net:8200", MountPath: "postgresql-pg-01", Secret: "s"})
	if strings.Contains(out, "walEncryption") {
		t.Error("walEncryption was written for a cluster that did not ask for it")
	}
	if strings.Contains(out, "caSecret") {
		t.Error("caSecret was written with no CA to verify")
	}
}

// `databases` defaults to an EMPTY ARRAY, and an empty array is not the same as an absent key:
// [] is the CRD's own "every non-template database except postgres". It also has to be written
// in flow style — a block sequence with no items is not YAML.
func TestPGLogicalReplicasDatabasesDefaultToAnEmptyArray(t *testing.T) {
	out := pgLogicalReplicasBlock(2, 1, "pgbackrest", nil)
	if strings.Count(out, "  databases: []\n") != 2 {
		t.Errorf("every replica must carry an empty databases array:\n%s", out)
	}
	if strings.Contains(out, "databases:\n") {
		t.Errorf("an empty list must be flow style, not an empty block sequence:\n%s", out)
	}
	// The same when it reaches cr.yaml through the transform.
	raw, err := os.ReadFile("testdata/cr-pg.yaml")
	if err != nil {
		t.Skipf("no PG cr.yaml fixture: %v", err)
	}
	cr := pgTransform(string(raw), pgOptions{Name: "pg-01", LogicalReplicas: 1, LogicalStorageGB: 1, LogicalBootstrap: "pgbackrest"})
	if !strings.Contains(cr, "  - name: replica1\n    databases: []\n") {
		t.Error("cr.yaml must carry databases: [] for a frame that named none")
	}
}

// A named list is emitted as a block sequence, one entry per database.
func TestPGLogicalReplicasDatabasesList(t *testing.T) {
	out := pgLogicalReplicasBlock(1, 1, "pgbackrest", []string{"shop", "analytics"})
	if !strings.Contains(out, "  databases:\n  - shop\n  - analytics\n") {
		t.Errorf("named databases were not emitted as a list:\n%s", out)
	}
	if strings.Contains(out, "databases: []") {
		t.Error("the empty-array form survived alongside a named list")
	}
}

// The frame spells the list as free text, so the parsing is where the mistakes are: blanks,
// stray commas, padding and repeats all have to come out as a clean set — and the default has
// to be the empty list, not a list containing "".
func TestPGLogicalDatabasesParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"shop", []string{"shop"}},
		{" shop , analytics ", []string{"shop", "analytics"}},
		{"shop,,analytics,", []string{"shop", "analytics"}},
		{"shop, shop ,analytics", []string{"shop", "analytics"}}, // a set, not a bag
	} {
		got := pgLogicalDatabases(designFrame{K3DPGLogicalDatabases: tc.in})
		if len(got) != len(tc.want) {
			t.Errorf("pgLogicalDatabases(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("pgLogicalDatabases(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
