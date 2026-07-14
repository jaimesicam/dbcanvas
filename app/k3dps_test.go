package main

import (
	"os"
	"strings"
	"testing"
)

// activeValues maps every *active* line of a CR to its path — the readable way to assert on a file
// whose keys sit six levels deep and whose neighbours are comments.
func activeValues(src string) map[string]string {
	out := map[string]string{}
	y := newYPath()
	for _, ln := range strings.Split(src, "\n") {
		ind, commented, body := crLine(ln)
		y.update(ind, commented, body)
		if commented || body == "" || !strings.Contains(body, ":") {
			continue
		}
		if v := strings.TrimSpace(strings.SplitN(body, ":", 2)[1]); v != "" {
			out[y.String()] = v
		}
	}
	return out
}

// psTransform runs against the ps-operator's real cr.yaml (testdata/cr-ps.yaml, 1.2.0).
func TestPSTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-ps.yaml")
	if err != nil {
		t.Skipf("no PS cr.yaml fixture: %v", err)
	}
	out := psTransform(string(raw), psOptions{
		Name:        "ps-01",
		ClusterType: "group-replication",
		Proxy:       "haproxy",
		ExposeMySQL: "ClusterIP",
		ExposeProxy: "LoadBalancer",
		PMMHost:     "pmm-01.example.net:8443",
		S3: &crS3{
			Bucket: "backups", Region: "us-east-1",
			EndpointURL: "http://seaweedfs-01.example.net:8333",
			Secret:      "ps-01-backup-s3",
		},
	})

	// 1. Anti-affinity and the CPU/memory requests — but the PVC keeps its size.
	pvc, sawStorage := newCRPVC(), false
	for i, ln := range strings.Split(out, "\n") {
		ind, commented, body := crLine(ln)
		pvc.update(ind, commented, body)
		if commented {
			continue
		}
		if strings.HasPrefix(body, "antiAffinityTopologyKey:") && body != `antiAffinityTopologyKey: "none"` {
			t.Errorf("line %d: anti-affinity not neutralised: %q", i+1, body)
		}
		if body == "resources:" && !pvc.inside() {
			t.Errorf("line %d: a CPU/memory resources block is still active", i+1)
		}
		if pvc.inside() && strings.HasPrefix(body, "storage:") {
			sawStorage = true
		}
	}
	if !sawStorage {
		t.Error("the PVC's storage request was commented out")
	}

	// 2. The cluster's name — and the secrets it names, which are spelled out in the shipped file.
	//    A secretsName the operator cannot find means random passwords instead of .env's.
	if !strings.Contains(out, "\n  name: ps-01\n") {
		t.Error("metadata.name was not set")
	}
	if !strings.Contains(out, "  secretsName: ps-01-secrets") || !strings.Contains(out, "  sslSecretName: ps-01-ssl") {
		t.Error("secretsName/sslSecretName still point at the shipped ps-cluster1-* secrets")
	}

	// 3. Group replication with HAProxy: Orchestrator and MySQL Router both stay off.
	if !strings.Contains(out, "    clusterType: group-replication") {
		t.Error("clusterType was not set")
	}
	v := activeValues(out)
	if v["spec.orchestrator.enabled"] != "false" {
		t.Error("Orchestrator must stay off for group replication")
	}
	if v["spec.proxy.haproxy.enabled"] != "true" || v["spec.proxy.router.enabled"] != "false" {
		t.Errorf("the front end is not exactly HAProxy: haproxy=%s router=%s",
			v["spec.proxy.haproxy.enabled"], v["spec.proxy.router.enabled"])
	}

	// 4. Expose: the primary Service and the front end are independent.
	if v["spec.mysql.exposePrimary.type"] != "ClusterIP" || v["spec.mysql.exposePrimary.enabled"] != "true" {
		t.Error("the primary MySQL Service was not exposed")
	}
	if v["spec.proxy.haproxy.expose.type"] != "LoadBalancer" {
		t.Error("HAProxy was not exposed")
	}

	// 5. PMM and the SeaweedFS backup storage (which replaces the shipped placeholder).
	if v["spec.pmm.enabled"] != "true" || v["spec.pmm.serverHost"] != "pmm-01.example.net:8443" {
		t.Error("PMM was not enabled/pointed at the server")
	}
	for _, want := range []string{"      seaweedfs:", "        verifyTLS: false", "          endpointUrl: http://seaweedfs-01.example.net:8333"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing backup storage line: %q", want)
		}
	}
	// Only *active* lines count — cr.yaml's own commented examples mention the placeholder bucket
	// and a storageName that no longer exists, and neither reaches the API server.
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented {
			continue
		}
		if strings.Contains(body, "S3-BACKUP-BUCKET-NAME-HERE") || strings.Contains(body, "s3-us-west") {
			t.Errorf("line %d: the shipped placeholder storage survived: %q", i+1, body)
		}
		// The ps-operator's S3 schema has no forcePathStyle at all; an unknown field is a
		// strict-decoding error, and the API server rejects the whole custom resource.
		if strings.Contains(body, "forcePathStyle") {
			t.Errorf("line %d: forcePathStyle was emitted; this operator's CRD does not know it", i+1)
		}
	}
}

// Async replication needs Orchestrator, and MySQL Router cannot serve it — the transform must not
// leave a cluster that the operator refuses to run.
func TestPSAsyncEnablesOrchestratorAndKeepsHAProxy(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-ps.yaml")
	if err != nil {
		t.Skipf("no PS cr.yaml fixture: %v", err)
	}
	out := psTransform(string(raw), psOptions{
		Name: "ps-01", ClusterType: "async", Proxy: "router", // router is not valid for async
		ExposeMySQL: "ClusterIP", ExposeProxy: "LoadBalancer",
	})
	v := activeValues(out)
	if v["spec.mysql.clusterType"] != "async" {
		t.Error("clusterType was not set to async")
	}
	if v["spec.orchestrator.enabled"] != "true" {
		t.Error("async replication needs Orchestrator")
	}
	if v["spec.proxy.haproxy.enabled"] != "true" || v["spec.proxy.router.enabled"] != "false" {
		t.Error("async must fall back to HAProxy — MySQL Router only speaks group replication")
	}
}

// MySQL Router, on a group-replication cluster.
func TestPSRouterFrontEnd(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr-ps.yaml")
	if err != nil {
		t.Skipf("no PS cr.yaml fixture: %v", err)
	}
	out := psTransform(string(raw), psOptions{
		Name: "ps-01", ClusterType: "group-replication", Proxy: "router", ExposeProxy: "LoadBalancer",
	})
	v := activeValues(out)
	if v["spec.proxy.router.enabled"] != "true" || v["spec.proxy.router.expose.type"] != "LoadBalancer" {
		t.Error("MySQL Router was not enabled and exposed")
	}
	if v["spec.proxy.haproxy.enabled"] != "false" {
		t.Error("HAProxy must be disabled when Router is the front end")
	}
}

// The users secret is the same shape as PXC's, so it goes through the same transform.
func TestPSSecretsTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/secrets-ps.yaml")
	if err != nil {
		t.Skipf("no PS secrets.yaml fixture: %v", err)
	}
	out := secretsTransform(string(raw), "ps-01", map[string]string{"root": "s3cret", "monitor": "m0nitor"})
	if !strings.Contains(out, "  name: ps-01-secrets") {
		t.Error("the secret was not renamed to what cr.yaml's secretsName points at")
	}
	if !strings.Contains(out, "  root: s3cret") || !strings.Contains(out, "  monitor: m0nitor") {
		t.Error("the .env passwords were not applied")
	}
	// Users with no .env counterpart keep what the operator ships.
	if !strings.Contains(out, "  orchestrator: orchestrator_password") {
		t.Error("an unmapped user must keep the shipped value")
	}
}
