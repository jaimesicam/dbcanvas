package main

import (
	"os"
	"strings"
	"testing"
)

// The operator catalog is what stands between a hand-edited design and a git/image fetch of an
// arbitrary tag, so unknown versions must not resolve.
func TestOperatorCatalog(t *testing.T) {
	t.Setenv("VERSIONS_FILE", "../versions.yaml")
	cat := loadOperatorCatalog()

	for _, product := range []string{"pxc", "psmdb", "pg"} {
		ov, ok := cat[product]
		if !ok {
			t.Fatalf("catalog has no %q operator — did `make versions` run?", product)
		}
		if ov.Repository == "" || ov.Latest == "" || len(ov.Versions) == 0 {
			t.Errorf("%s: incomplete entry %+v", product, ov)
		}
		if ov.Versions[0] != ov.Latest {
			t.Errorf("%s: versions are not newest-first (latest %q, first %q)", product, ov.Latest, ov.Versions[0])
		}
	}
	if repo := cat["pxc"].Repository; repo != "percona/percona-xtradb-cluster-operator" {
		t.Errorf("pxc repository = %q", repo)
	}

	// resolveOperatorVersion: empty → latest; known → itself; unknown → refused.
	if v, ok := cat.resolveOperatorVersion("pxc", ""); !ok || v != cat["pxc"].Latest {
		t.Errorf("empty request should resolve to latest, got %q ok=%v", v, ok)
	}
	known := cat["pxc"].Versions[1]
	if v, ok := cat.resolveOperatorVersion("pxc", known); !ok || v != known {
		t.Errorf("a known version should resolve to itself, got %q ok=%v", v, ok)
	}
	if _, ok := cat.resolveOperatorVersion("pxc", "9.9.9"); ok {
		t.Error("an unknown version must be refused — it would otherwise reach a git fetch")
	}
	if _, ok := cat.resolveOperatorVersion("nope", ""); ok {
		t.Error("an unknown product must be refused")
	}
}

// crTransform runs against the operator's real cr.yaml (testdata/cr.yaml).
func TestCRTransform(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{
		Name:  "k3d-01",
		Proxy: "haproxy",
		// The sections are independent: database in-cluster, proxy on a LoadBalancer.
		ExposePXC:     "ClusterIP",
		ExposeHAProxy: "LoadBalancer",
		PMMHost:       "pmm-01.example.net",
		S3: &crS3{
			Bucket: "backups", Region: "us-east-1",
			EndpointURL: "http://seaweedfs-01.example.net:8333",
			Secret:      "k3d-01-backup-s3",
			// testdata/cr.yaml is 1.20.0, which knows the field (see the version test below).
			ForcePathStyle: true,
		},
	})

	// 1. Anti-affinity: a 1-node cluster cannot place one pod per node.
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented || !strings.HasPrefix(body, "antiAffinityTopologyKey:") {
			continue
		}
		if body != `antiAffinityTopologyKey: "none"` {
			t.Errorf("line %d: anti-affinity not neutralised: %q", i+1, body)
		}
	}

	// 2. No section keeps an active resources block (its pods would not be admitted) — but the
	//    PersistentVolumeClaim MUST keep its storage request, or the PVC is invalid.
	sawStorage := false
	for i, ln := range strings.Split(out, "\n") {
		ind, commented, body := crLine(ln)
		if commented {
			continue
		}
		if ind == 4 && body == "resources:" {
			t.Errorf("line %d: a section's resources block is still active", i+1)
		}
		if strings.HasPrefix(body, "storage:") {
			sawStorage = true
		}
	}
	if !sawStorage {
		t.Error("the PVC's storage request was commented out — the volumeSpec resources must survive")
	}

	// 3. Expose is per section, not one blanket value: the database keeps ClusterIP while HAProxy
	//    gets a LoadBalancer address.
	for _, want := range []string{
		"    expose:\n      enabled: true\n      type: ClusterIP",            // pxc
		"    exposePrimary:\n      enabled: true\n      type: LoadBalancer",  // haproxy
		"    exposeReplicas:\n      enabled: true\n      type: LoadBalancer", // haproxy
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing expose block:\n%s", want)
		}
	}

	// 4. metadata.name — the operator names every resource it creates after this, and the manager
	//    advertises it. Left as the shipped "cluster1" it would not match what the UI shows.
	if !strings.Contains(out, "\n  name: k3d-01\n") {
		t.Error("metadata.name was not set to the cluster name")
	}

	// 5. PMM + backups.
	if !strings.Contains(out, "serverHost: pmm-01.example.net") || !strings.Contains(out, "    enabled: true\n    image: percona/pmm-client") {
		t.Error("PMM was not enabled/pointed at the server")
	}
	for _, want := range []string{
		"      seaweedfs:",
		"        type: s3",
		"          endpointUrl: http://seaweedfs-01.example.net:8333",
		"          forcePathStyle: true",
		"          credentialsSecret: k3d-01-backup-s3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing backup storage line: %q", want)
		}
	}
	if strings.Contains(out, "S3-BACKUP-BUCKET-NAME-HERE") {
		t.Error("the shipped placeholder storage survived")
	}
	// The backup pods do not trust the Intranet CA (nothing hands them the stack CA), so verifying
	// SeaweedFS's certificate would fail the backup.
	if !strings.Contains(out, "        verifyTLS: false") {
		t.Error("the SeaweedFS storage must not verify TLS")
	}

	// 6. No dangling storage reference. cr.yaml's shipped schedule names "fs-pvc", which the
	//    storages replacement removes — the operator then rejects the whole CR with
	//    "storage fs-pvc doesn't exist" and never creates the cluster. Every active storageName
	//    must name a storage that exists.
	for i, ln := range strings.Split(out, "\n") {
		_, commented, body := crLine(ln)
		if commented || !strings.HasPrefix(body, "storageName:") {
			continue
		}
		if got := strings.TrimSpace(strings.TrimPrefix(body, "storageName:")); got != crStorageName {
			t.Errorf("line %d: storageName %q does not exist (the only storage is %q)", i+1, got, crStorageName)
		}
	}
}

// With no options, cr.yaml must still be de-fanged (affinity + resources) but nothing else touched.
func TestCRTransformMinimal(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{Name: "k3d-01", Proxy: "haproxy", ExposePXC: "ClusterIP", ExposeHAProxy: "ClusterIP"})
	if strings.Contains(out, "serverHost: pmm-01") {
		t.Error("PMM must stay as shipped when no PMM node is linked")
	}
	if !strings.Contains(out, "S3-BACKUP-BUCKET-NAME-HERE") {
		t.Error("the shipped storages must be left alone when no SeaweedFS node is linked")
	}
	if !strings.Contains(out, "      type: ClusterIP") {
		t.Error("expose type was not applied")
	}
}

// forcePathStyle was added to the operator's S3 schema in 1.20.0. Emitting it against an older CRD
// is not a warning — the API server rejects the ENTIRE custom resource with a strict-decoding error
// ("unknown field spec.backup.storages.seaweedfs.s3.forcePathStyle"), so the cluster is never
// created. It must be emitted only when the selected version's own cr.yaml knows the field.
func TestCRForcePathStyleFollowsTheOperatorVersion(t *testing.T) {
	s3 := func(force bool) *crS3 {
		return &crS3{Bucket: "backups", Region: "us-east-1", Secret: "s", ForcePathStyle: force,
			EndpointURL: "http://seaweedfs-01.example.net:8333"}
	}
	for _, tc := range []struct {
		fixture string
		want    bool // does that version's cr.yaml know the field?
	}{
		{"testdata/cr.yaml", true},         // 1.20.0
		{"testdata/cr-1.19.1.yaml", false}, // 1.19.1 and older
	} {
		raw, err := os.ReadFile(tc.fixture)
		if err != nil {
			t.Skipf("no fixture %s: %v", tc.fixture, err)
		}
		// This is exactly how installPXCOperator decides.
		supported := strings.Contains(string(raw), "forcePathStyle")
		if supported != tc.want {
			t.Fatalf("%s: forcePathStyle support detected as %v, want %v", tc.fixture, supported, tc.want)
		}
		out := crTransform(string(raw), crOptions{Name: "k3d-01", Proxy: "haproxy", ExposePXC: "ClusterIP", S3: s3(supported)})
		if got := strings.Contains(out, "forcePathStyle: true"); got != tc.want {
			t.Errorf("%s: emitted forcePathStyle=%v, want %v", tc.fixture, got, tc.want)
		}
		// Either way the storage itself must be there, and nothing may dangle.
		if !strings.Contains(out, "      seaweedfs:") {
			t.Errorf("%s: the SeaweedFS storage is missing", tc.fixture)
		}
	}
}

// cr.yaml ships HAProxy enabled and ProxySQL disabled. They are mutually exclusive front ends, so
// picking one must flip BOTH — leaving both enabled makes the operator run two proxies (and
// choosing ProxySQL without disabling HAProxy would simply not take effect).
func TestCRProxyChoice(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	// enabledOf reports the `enabled:` value of a top-level spec section.
	enabledOf := func(out, section string) string {
		cur, want := "", ""
		for _, ln := range strings.Split(out, "\n") {
			ind, commented, body := crLine(ln)
			if commented {
				continue
			}
			if ind == 2 && strings.HasSuffix(body, ":") {
				cur = strings.TrimSuffix(body, ":")
				continue
			}
			if cur == section && ind == 4 && strings.HasPrefix(body, "enabled:") && want == "" {
				want = strings.TrimSpace(strings.TrimPrefix(body, "enabled:"))
			}
		}
		return want
	}

	for _, tc := range []struct{ proxy, haproxy, proxysql string }{
		{"haproxy", "true", "false"},
		{"proxysql", "false", "true"},
		{"", "true", "false"}, // unset → cr.yaml's own default (HAProxy)
	} {
		out := crTransform(string(raw), crOptions{Name: "k3d-01", Proxy: tc.proxy, ExposePXC: "ClusterIP"})
		if got := enabledOf(out, "haproxy"); got != tc.haproxy {
			t.Errorf("proxy=%q: haproxy.enabled = %s, want %s", tc.proxy, got, tc.haproxy)
		}
		if got := enabledOf(out, "proxysql"); got != tc.proxysql {
			t.Errorf("proxy=%q: proxysql.enabled = %s, want %s", tc.proxy, got, tc.proxysql)
		}
	}
}

// secrets.yaml must be named after the cluster (cr.yaml's secretsName defaults to
// "<cluster>-secrets"; a mismatch and the operator silently generates its own random passwords),
// and the passwords come from .env like every other database DBCanvas deploys.
func TestSecretsTransform(t *testing.T) {
	src := `apiVersion: v1
kind: Secret
metadata:
  name: cluster1-secrets
type: Opaque
stringData:
  root: root_password
  xtrabackup: backup_password
  monitor: monitory
  proxyadmin: admin_password
#  pmmserverkey: my_pmm_server_key
  operator: operatoradmin
  replication: repl_password
`
	out := secretsTransform(src, "k3d-01", map[string]string{
		"root":        "R00t!",
		"monitor":     "M0n!",
		"replication": "Repl!",
		"operator":    "Op!",
		"proxyadmin":  "Prox!",
	})
	for _, want := range []string{
		"  name: k3d-01-secrets", // what cr.yaml's secretsName defaults to
		"  root: R00t!",
		"  monitor: M0n!",
		"  replication: Repl!",
		"  operator: Op!",
		"  proxyadmin: Prox!",
		"  xtrabackup: backup_password", // no .env counterpart: left as shipped
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cluster1-secrets") {
		t.Error("the shipped secret name survived — the operator would ignore this secret")
	}
	if !strings.Contains(out, "#  pmmserverkey: my_pmm_server_key") {
		t.Error("commented keys must be left alone")
	}
}

func TestValidNamespace(t *testing.T) {
	for _, ok := range []string{"pxc", "my-ns", "a", "ns1"} {
		if !validNamespace(ok) {
			t.Errorf("%q should be a valid namespace", ok)
		}
	}
	for _, bad := range []string{"", "-x", "x-", "UPPER", "has_underscore", "a.b", strings.Repeat("x", 64)} {
		if validNamespace(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// Every stack's first K3D frame is labelled "k3d-00" by default, and k3d cluster names are global
// to the Docker daemon — so the name must be scoped by stack, or the second stack's deploy fails
// with "a cluster with that name already exists".
func TestK3DClusterNameIsScopedByStack(t *testing.T) {
	f := designFrame{Label: "k3d-00"}
	a, b := k3dClusterName(7, f), k3dClusterName(8, f)
	if a == b {
		t.Fatalf("two stacks with the same frame label share a cluster name (%q)", a)
	}
	if a != "k3d-00-s7" {
		t.Errorf("cluster name = %q, want k3d-00-s7", a)
	}
}

func TestK3DNodeContainer(t *testing.T) {
	if got := k3dNodeContainer("k3d-01", 0); got != "k3d-k3d-01-server-0" {
		t.Errorf("first member should be the server, got %q", got)
	}
	if got := k3dNodeContainer("k3d-01", 2); got != "k3d-k3d-01-agent-1" {
		t.Errorf("third member should be agent-1, got %q", got)
	}
}
