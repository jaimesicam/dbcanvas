package main

import (
	"os"
	"strings"
	"testing"
)

// psmdbTransform runs against the MongoDB operator's real cr.yaml (testdata/cr-psmdb.yaml, 1.22.0).
func TestPSMDBTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb.yaml")
	if err != nil {
		t.Skipf("no PSMDB cr.yaml fixture: %v", err)
	}
	out := psmdbTransform(string(raw), psmdbOptions{
		Name:          "mongo-01",
		Sharding:      true,
		ExposeReplset: "ClusterIP",
		ExposeMongos:  "LoadBalancer",
		PMMHost:       "pmm-01.example.net:8443",
		S3: &crS3{
			Bucket: "backups", Region: "us-east-1",
			EndpointURL:    "http://seaweedfs-01.example.net:8333",
			Secret:         "mongo-01-backup-s3",
			ForcePathStyle: true,
		},
	})

	// 1. Anti-affinity: a 1–3 node cluster cannot place one pod per node — and PSMDB carries the key
	//    in six places (replset, arbiter, hidden, nonvoting, config servers, mongos).
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented || !strings.HasPrefix(body, "antiAffinityTopologyKey:") {
			continue
		}
		if body != `antiAffinityTopologyKey: "none"` {
			t.Errorf("line %d: anti-affinity not neutralised: %q", i+1, body)
		}
	}

	// 2. No CPU/memory request survives — but every PersistentVolumeClaim keeps its storage size,
	//    which the operator requires. PSMDB nests them differently from PXC: a replset's resources
	//    sit at 4 spaces, its arbiter's and mongos's at 6, and the PVC's at 8 and 12.
	pvc, storages := newCRPVC(), 0
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
			storages++
		}
	}
	if storages == 0 {
		t.Error("every PVC's storage request was commented out — the volume size must survive")
	}

	// 3. Sharding and expose. mongos has a type and no `enabled` key; the replica set has both, and
	//    ships disabled.
	if !strings.Contains(out, "  sharding:\n    enabled: true") {
		t.Error("sharding was not enabled")
	}
	if !strings.Contains(out, "    expose:\n      enabled: true\n      type: ClusterIP") {
		t.Error("the replica set's expose block was not set")
	}
	if !strings.Contains(out, "      expose:\n        type: LoadBalancer") {
		t.Error("mongos was not exposed on a LoadBalancer")
	}

	// 4. The cluster's name. `secrets.users` names the users secret explicitly — if it keeps
	//    pointing at "my-cluster-name-secrets" the operator finds no secret and generates its own
	//    random passwords instead of the ones from .env.
	if !strings.Contains(out, "\n  name: mongo-01\n") {
		t.Error("metadata.name was not set to the cluster name")
	}
	if !strings.Contains(out, "    users: mongo-01-secrets") ||
		!strings.Contains(out, "    encryptionKey: mongo-01-mongodb-encryption-key") {
		t.Error("spec.secrets still points at the shipped my-cluster-name-* secrets")
	}

	// 5. PMM: enabled, and pointed at the server *with its port* (the operator hands serverHost to
	//    the sidecars verbatim as PMM_AGENT_SERVER_ADDRESS, which otherwise defaults to :443).
	if !strings.Contains(out, "    serverHost: pmm-01.example.net:8443") {
		t.Error("PMM serverHost was not set")
	}
	if !strings.Contains(out, "  pmm:\n    enabled: true") {
		t.Error("PMM was not enabled")
	}

	// 6. Backups: the shipped storages are all commented out, so ours is inserted.
	for _, want := range []string{
		"    storages:",
		"      seaweedfs:",
		"        main: true",
		"          endpointUrl: http://seaweedfs-01.example.net:8333",
		"          credentialsSecret: mongo-01-backup-s3",
		"          insecureSkipTLSVerify: true",
		"          forcePathStyle: true", // SeaweedFS has no virtual-host bucket addressing
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing backup storage line: %q", want)
		}
	}
}

// A replica set (the default) must leave the routers out of the CR entirely.
func TestPSMDBReplicaSetTopology(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-psmdb.yaml")
	if err != nil {
		t.Skipf("no PSMDB cr.yaml fixture: %v", err)
	}
	out := psmdbTransform(string(raw), psmdbOptions{Name: "mongo-01", Sharding: false, ExposeReplset: "LoadBalancer"})
	if !strings.Contains(out, "  sharding:\n    enabled: false") {
		t.Error("sharding must be off for a plain replica set (9 pods otherwise)")
	}
	if !strings.Contains(out, "    expose:\n      enabled: true\n      type: LoadBalancer") {
		t.Error("the replica set's expose block was not set")
	}
	if strings.Contains(out, "serverHost: pmm") {
		t.Error("PMM was enabled without a PMM node")
	}
}

// The users secret: renamed to what the CR points at, .env passwords, and no PMM 2 relic.
func TestPSMDBSecretsTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/secrets-psmdb.yaml")
	if err != nil {
		t.Skipf("no PSMDB secrets.yaml fixture: %v", err)
	}
	out := psmdbSecretsTransform(string(raw), "mongo-01", map[string]string{
		"MONGODB_USER_ADMIN_PASSWORD":    "s3cret",
		"MONGODB_CLUSTER_ADMIN_PASSWORD": "s3cret",
	})
	if !strings.Contains(out, "  name: mongo-01-secrets") {
		t.Error("the secret was not renamed to what cr.yaml's secrets.users points at")
	}
	if !strings.Contains(out, "  MONGODB_USER_ADMIN_PASSWORD: s3cret") ||
		!strings.Contains(out, "  MONGODB_CLUSTER_ADMIN_PASSWORD: s3cret") {
		t.Error("the .env passwords were not applied")
	}
	// The usernames the operator ships stay put.
	if !strings.Contains(out, "  MONGODB_USER_ADMIN_USER: userAdmin") {
		t.Error("the shipped usernames must survive")
	}
	// A password with no .env counterpart keeps the shipped value.
	if !strings.Contains(out, "  MONGODB_BACKUP_PASSWORD: backup123456") {
		t.Error("an unmapped user must keep the value the operator ships")
	}
	// PMM 2's API key must go: the operator picks the PMM 2 sidecar whenever it is present and a
	// PMM 3 token is not, and would then authenticate against a PMM 3 server with "apikey".
	if strings.Contains(out, "PMM_SERVER_API_KEY: apikey") {
		t.Error("the shipped PMM 2 API key survived — it selects the PMM 2 code path")
	}
}

// yPath is what makes the nested CR safe to rewrite: `expose.type` alone appears under the replica
// set, the config servers and the routers.
func TestYPathTracksNestedKeys(t *testing.T) {
	src := `spec:
  replsets:
  - name: rs0
    expose:
      type: ClusterIP
  sharding:
    mongos:
      expose:
        type: ClusterIP
`
	var paths []string
	y := newYPath()
	for _, ln := range strings.Split(src, "\n") {
		ind, commented, body := crLine(ln)
		if key := y.update(ind, commented, body); key == "type" {
			paths = append(paths, y.String())
		}
	}
	want := []string{"spec.replsets.expose.type", "spec.sharding.mongos.expose.type"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}
