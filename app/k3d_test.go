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

// The K3D frame chooses its own Kubernetes version: k3d's default trails the releases, and an API
// server too old for an operator's CRDs makes that operator uninstallable.
func TestResolveK3SVersion(t *testing.T) {
	cat := PMMCatalog{
		Repository: "rancher/k3s",
		Latest:     "v1.36.2-k3s1",
		Versions:   []string{"v1.36.2-k3s1", "v1.33.13-k3s1"},
	}
	for _, tc := range []struct {
		want string
		tag  string
		ok   bool
	}{
		{"", "v1.36.2-k3s1", true},       // the default: latest
		{"latest", "v1.36.2-k3s1", true}, //
		{"v1.33.13-k3s1", "v1.33.13-k3s1", true},
		{"v1.31.5-k3s1", "", false}, // not in the catalog: refused, not silently pulled
		{"garbage", "", false},
	} {
		got, ok := cat.resolveK3SVersion(tc.want)
		if got != tc.tag || ok != tc.ok {
			t.Errorf("resolveK3SVersion(%q) = (%q, %v), want (%q, %v)", tc.want, got, ok, tc.tag, tc.ok)
		}
	}
	if img := cat.k3sImageRef("v1.36.2-k3s1"); img != "rancher/k3s:v1.36.2-k3s1" {
		t.Errorf("k3sImageRef = %q", img)
	}
	// With no versions.yaml at all, the fallback still names a k3s every operator installs on.
	if _, ok := (PMMCatalog{Latest: k3sCatalogFallback.Latest}).resolveK3SVersion(""); !ok {
		t.Error("the fallback must still resolve a version")
	}
}

// A PostgreSQL-operator cluster pointed at a plain-HTTP SeaweedFS node cannot back up to it —
// pgBackRest has no plaintext S3 — and today the only trace is a line in the node's deploy log. The
// designer has to say so, or the bucket just stays empty and nobody knows why.
func TestK3DBackupIssuesWarnsWhenPGCannotReachS3(t *testing.T) {
	doc := func(tls bool) designDoc {
		return designDoc{Nodes: []designNode{
			{ID: "s1", Type: "seaweedfs", Label: "seaweedfs-01", Buckets: []string{"pg"}, TLS: tls},
		}}
	}
	pg := designFrame{Label: "k3d-00", K3DOperator: "pg", SeaweedFSNodeID: "s1"}

	iss := k3dBackupIssues(pg, doc(false))
	if len(iss) != 1 || iss[0].Level != "warning" {
		t.Fatalf("a PG cluster on a plaintext SeaweedFS node must warn, got %v", iss)
	}
	if !strings.Contains(iss[0].Message, "seaweedfs-01") {
		t.Errorf("the warning must name the node: %q", iss[0].Message)
	}
	// It is a warning, not an error: the cluster still deploys, and still backs up — to the
	// operator's PVC repo.
	if len(k3dBackupIssues(pg, doc(true))) != 0 {
		t.Error("S3 TLS on: nothing to warn about")
	}
	// The other operators do plaintext S3 quite happily (xbcloud, PBM).
	pxc := designFrame{Label: "k3d-01", K3DOperator: "pxc", SeaweedFSNodeID: "s1"}
	if len(k3dBackupIssues(pxc, doc(false))) != 0 {
		t.Error("PXC backs up over plain HTTP; it must not warn")
	}
	// No SeaweedFS node selected at all: backups are simply off, which is not a problem.
	if len(k3dBackupIssues(designFrame{Label: "k3d-02", K3DOperator: "pg"}, doc(false))) != 0 {
		t.Error("no backup target selected must not warn")
	}
}

// Point-in-time recovery is the binlog collector (`backup.pitr`), and the thing that can go
// wrong silently is its STORAGE: the general backup rule repoints every `storageName:` in the
// section at the backup storage, and applying that to pitr sends the binary logs to the backup
// bucket while the panel says otherwise. Nothing notices until a point-in-time restore is
// attempted and finds no binlogs.
func TestCRTransformPITR(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	backups := &crS3{Bucket: "backups", Region: "us-east-1", Secret: "k3d-01-backup-s3",
		EndpointURL: "http://seaweedfs-01.example.net:8333", ForcePathStyle: true}
	binlogs := &crS3{Bucket: "binlogs", Region: "us-east-1", Secret: "k3d-01-backup-s3",
		EndpointURL: "http://seaweedfs-01.example.net:8333", ForcePathStyle: true}

	// pitrKeys reads the three keys inside `backup.pitr` out of the result, so the assertions
	// are about that block and not about a string that happens to appear in the file.
	pitrKeys := func(out string) map[string]string {
		got := map[string]string{}
		section, at := "", -1
		for _, ln := range strings.Split(out, "\n") {
			ind, commented, body := crLine(ln)
			if commented || body == "" {
				continue
			}
			if ind == 2 && strings.HasSuffix(body, ":") {
				section = strings.TrimSuffix(body, ":")
			}
			if at >= 0 && ind <= at {
				at = -1
			}
			if section == "backup" && body == "pitr:" {
				at = ind
				continue
			}
			if at >= 0 && ind > at {
				if k, v, ok := strings.Cut(body, ":"); ok {
					got[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		}
		return got
	}

	t.Run("its own bucket", func(t *testing.T) {
		out := crTransform(string(raw), crOptions{
			Name: "k3d-01", Proxy: "haproxy", ExposePXC: "ClusterIP", S3: backups,
			PITR: &crPITR{Enabled: true, Storage: crBinlogStorageName, Seconds: 30, Binlog: binlogs},
		})
		got := pitrKeys(out)
		if got["enabled"] != "true" {
			t.Errorf("pitr.enabled = %q, want true", got["enabled"])
		}
		if got["storageName"] != crBinlogStorageName {
			t.Errorf("pitr.storageName = %q, want %q — the binlogs must not go to the backup storage", got["storageName"], crBinlogStorageName)
		}
		if got["timeBetweenUploads"] != "30" {
			t.Errorf("pitr.timeBetweenUploads = %q, want 30", got["timeBetweenUploads"])
		}
		// Both storages have to exist, or the operator refuses the whole custom resource over a
		// storageName that names nothing.
		for _, want := range []string{"      " + crStorageName + ":", "      " + crBinlogStorageName + ":"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing storage entry %q", strings.TrimSpace(want))
			}
		}
		if !strings.Contains(out, "bucket: binlogs") || !strings.Contains(out, "bucket: backups") {
			t.Error("each storage must carry its own bucket")
		}
		// And the scheduled backup still points at the backup storage.
		for _, ln := range strings.Split(out, "\n") {
			ind, commented, body := crLine(ln)
			if !commented && ind == 8 && strings.HasPrefix(body, "storageName:") &&
				strings.TrimSpace(strings.TrimPrefix(body, "storageName:")) != crStorageName {
				t.Errorf("the schedule's storage was repointed at %q", body)
			}
		}
	})

	t.Run("sharing the backup bucket", func(t *testing.T) {
		out := crTransform(string(raw), crOptions{
			Name: "k3d-01", Proxy: "haproxy", ExposePXC: "ClusterIP", S3: backups,
			PITR: &crPITR{Enabled: true, Storage: crStorageName},
		})
		got := pitrKeys(out)
		if got["storageName"] != crStorageName {
			t.Errorf("pitr.storageName = %q, want %q", got["storageName"], crStorageName)
		}
		// No second storage was asked for, so none may appear.
		if strings.Contains(out, crBinlogStorageName+":") {
			t.Error("a binlog storage was written that nothing points at")
		}
		// The shipped default is 60 and was not overridden, so it must survive untouched.
		if got["timeBetweenUploads"] != "60" {
			t.Errorf("timeBetweenUploads = %q, want cr.yaml's own 60", got["timeBetweenUploads"])
		}
	})

	t.Run("deferred on a replica", func(t *testing.T) {
		// What k3dPITROptions produces for the replica end of a replication link: the collector
		// is configured but off, so the seed restore replaces the GTID history with nothing
		// uploading across it.
		out := crTransform(string(raw), crOptions{
			Name: "cluster2", Proxy: "haproxy", ExposePXC: "ClusterIP", S3: backups,
			PITR: &crPITR{Enabled: false, Storage: crBinlogStorageName, Binlog: binlogs},
		})
		if got := pitrKeys(out); got["enabled"] != "false" {
			t.Errorf("pitr.enabled = %q, want false on a replica", got["enabled"])
		}
		// The storage is still written, so turning it on later is a one-field patch.
		if !strings.Contains(out, "      "+crBinlogStorageName+":") {
			t.Error("the binlog storage must be in place even while the collector is off")
		}
	})

	t.Run("not asked for", func(t *testing.T) {
		out := crTransform(string(raw), crOptions{
			Name: "k3d-01", Proxy: "haproxy", ExposePXC: "ClusterIP", S3: backups,
		})
		got := pitrKeys(out)
		if got["enabled"] != "false" {
			t.Errorf("pitr.enabled = %q, want cr.yaml's own false", got["enabled"])
		}
		// The shipped placeholder does not exist once the storages block is replaced, and the
		// operator validates the name even with the collector disabled.
		if got["storageName"] != crStorageName {
			t.Errorf("pitr.storageName = %q, want it repointed to %q", got["storageName"], crStorageName)
		}
		if strings.Contains(out, "STORAGE-NAME-HERE") {
			t.Error("a storage name that does not exist was left in the custom resource")
		}
	})
}

// A K3D frame is the one part of a stack whose containers this app does not name — k3d
// does, and the dashboard has to recognise them anyway or a stack's k3s nodes go
// unmonitored (which is exactly what happened: ListManaged matched only "dbcanvas-",
// so every k3s node and load balancer was dropped before stats were sampled).
//
// The names below are real, off a host running two K3D frames in stack 2.
func TestK3DStackIDFromContainer(t *testing.T) {
	ours := map[string]int64{
		"k3d-k3d-00-s2-server-0": 2,
		"k3d-k3d-00-s2-serverlb": 2,
		"k3d-k3d-01-s2-server-0": 2,
		"k3d-k3d-01-s2-serverlb": 2,
		"k3d-k3d-00-s17-agent-0": 17,
		"k3d-k3d-00-s17-agent-3": 17,
	}
	for name, want := range ours {
		got, ok := k3dStackIDFromContainer(name)
		if !ok || got != want {
			t.Errorf("%s → (%d, %v), want (%d, true)", name, got, ok, want)
		}
	}

	// Not ours, and none of it may reach a DBCanvas dashboard: somebody's own k3d
	// cluster (no stack scope), this app's own containers, the app itself, and the
	// container names the host's Kubernetes leaves lying around.
	for _, name := range []string{
		"k3d-mycluster-server-0",
		"k3d-dev-agent-0",
		"k3d-k3d-00-sx-server-0",
		"k3d-k3d-00-s0-server-0",
		"dbcanvas-2-intranet-mtw8qtzg-1",
		"dbcanvas-app-1",
		"k8s_POD_coredns-54996dc9b4-22ld2_kube-system_27f4e82e_0",
		"k3d-",
		"",
	} {
		if id, ok := k3dStackIDFromContainer(name); ok {
			t.Errorf("%s was claimed as stack %d; it is not a DBCanvas k3d container", name, id)
		}
	}

	// And the dashboard's own parser has to agree, for both schemes — a k3s node that
	// arrives with stack 0 is dropped by the ownership filter just the same.
	if got := stackIDFromName("k3d-k3d-00-s2-server-0"); got != 2 {
		t.Errorf("stackIDFromName(k3d node) = %d, want 2", got)
	}
	if got := stackIDFromName("dbcanvas-2-intranet-mtw8qtzg-1"); got != 2 {
		t.Errorf("stackIDFromName(dbcanvas node) = %d, want 2", got)
	}
}
