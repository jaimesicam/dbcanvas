package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The array is replaced wholesale on every change — a JSON merge patch cannot address one element
// of a list — so the single most dangerous thing this file can do is silently drop a field it does
// not model. A replica somebody added by hand with an affinity and a probe must survive a rewrite.
func TestK3DLRSpecKeepsUnmodelledFields(t *testing.T) {
	in := `{"name":"analytics","databases":["shop"],"bootstrapMethod":"pg_basebackup",
	        "dataVolumeClaimSpec":{"accessModes":["ReadWriteOnce"]},
	        "expose":{"type":"LoadBalancer"},
	        "affinity":{"nodeAffinity":{"x":1}},"tolerations":[{"key":"k"}],"priorityClassName":"high"}`
	var s k3dLRSpec
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for _, k := range []string{"name", "databases", "bootstrapMethod", "dataVolumeClaimSpec", "expose",
		"affinity", "tolerations", "priorityClassName"} {
		if _, ok := got[k]; !ok {
			t.Errorf("a rewrite dropped %q:\n%s", k, out)
		}
	}
}

// databases is always written, and written as an array. [] is the CRD's "all non-template
// databases except postgres" — omitting the key is a different statement.
func TestK3DLRSpecAlwaysEmitsDatabasesArray(t *testing.T) {
	out, err := json.Marshal(k3dLRSpec{Name: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"databases":[]`) {
		t.Errorf("databases must be an empty array, got %s", out)
	}
	// An entry that names none must not grow a null.
	if strings.Contains(string(out), "null") {
		t.Errorf("no field may serialise as null: %s", out)
	}
}

// The name rules are the operator's: 20 characters, a DNS-1123 label, unique, and never the same
// as an instance set's.
func TestK3DLRNameIssue(t *testing.T) {
	cr := &k3dLRCluster{}
	cr.Spec.LogicalReplicas = []k3dLRSpec{{Name: "analytics"}}
	cr.Spec.Instances = []struct {
		Name string `json:"name"`
	}{{Name: "instance1"}}

	for _, tc := range []struct{ name, want string }{
		{"reporting", ""},
		{"", "needs a name"},
		{strings.Repeat("a", 21), "at most 20"},
		{strings.Repeat("a", 20), ""},
		{"Analytics", "DNS-1123"},
		{"has_underscore", "DNS-1123"},
		{"-leading", "DNS-1123"},
		{"analytics", "already has a logical replica"},
		{"instance1", "spec.instances already has a set"},
	} {
		got := k3dLRNameIssue(tc.name, cr)
		if tc.want == "" && got != "" {
			t.Errorf("%q should be accepted, got %q", tc.name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%q: got %q, want it to mention %q", tc.name, got, tc.want)
		}
	}
}

// The preflight is the doc's requirements list. What matters is which failures BLOCK an add:
// a blocking one leaves a replica broken or bootstrapping forever, a warning has a valid answer.
func TestK3DLRPreflight(t *testing.T) {
	cfg := k3dConfig{OperatorVer: "3.1.0"}
	base := func() *k3dLRCluster {
		cr := &k3dLRCluster{}
		cr.Spec.PostgresVersion = 18
		cr.Status.State = "ready"
		return cr
	}
	find := func(checks []k3dLRCheck, substr string) k3dLRCheck {
		for _, c := range checks {
			if strings.Contains(c.Name, substr) {
				return c
			}
		}
		t.Fatalf("no check mentioning %q", substr)
		return k3dLRCheck{}
	}

	ok := k3dLRPreflight(cfg, base(), k3dLRBackups{Succeeded: 1, Completed: "2026-09-15T07:25:51Z"})
	for _, c := range ok {
		if c.Blocks && !c.OK {
			t.Errorf("a healthy cluster must have no blocking failure, got %q: %s", c.Name, c.Detail)
		}
	}

	// PostgreSQL 16 blocks: the feature needs 17+.
	old := base()
	old.Spec.PostgresVersion = 16
	if c := find(k3dLRPreflight(cfg, old, k3dLRBackups{Succeeded: 1, Completed: "2026-09-15T07:25:51Z"}), "PostgreSQL"); c.OK || !c.Blocks {
		t.Error("PostgreSQL 16 must be a blocking failure")
	}

	// TDE blocks — documented as unsupported, and the failure mode is a replica stuck broken.
	tde := base()
	tde.Spec.Extensions.PGTDE.Enabled = true
	c := find(k3dLRPreflight(cfg, tde, k3dLRBackups{Succeeded: 1, Completed: "2026-09-15T07:25:51Z"}), "encryption")
	if c.OK || !c.Blocks {
		t.Error("transparent data encryption must be a blocking failure")
	}

	// No backup yet is a WARNING, not a blocker: pg_basebackup is a supported answer to it.
	none := base()
	if c := find(k3dLRPreflight(cfg, none, k3dLRBackups{}), "backup"); c.OK || c.Blocks {
		t.Error("a missing backup must warn, not block — pg_basebackup is the answer to it")
	}
	// Backups disabled says so specifically, and still does not block.
	off := base()
	no := false
	off.Spec.Backups.Enabled = &no
	if c := find(k3dLRPreflight(cfg, off, k3dLRBackups{}), "backup"); c.Blocks || !strings.Contains(c.Detail, "pg_basebackup") {
		t.Errorf("backups disabled must point at pg_basebackup without blocking: %+v", c)
	}

	// A cluster that is not ready warns — the operator waits rather than failing.
	notReady := base()
	notReady.Status.State = "initializing"
	if c := find(k3dLRPreflight(cfg, notReady, k3dLRBackups{Succeeded: 1, Completed: "2026-09-15T07:25:51Z"}), "Cluster ready"); c.OK || c.Blocks {
		t.Error("a cluster that is not ready must warn, not block")
	}

	// logicalrepl is reserved; redefining it in spec.users breaks replication, so it blocks.
	clash := base()
	clash.Spec.Users = []struct {
		Name string `json:"name"`
	}{{Name: "logicalrepl"}}
	if c := find(k3dLRPreflight(cfg, clash, k3dLRBackups{Succeeded: 1, Completed: "2026-09-15T07:25:51Z"}), "logicalrepl"); c.OK || !c.Blocks {
		t.Error("redefining logicalrepl must be a blocking failure")
	}
}

// The add form's defaults are the CRD's: pgbackrest, and a dataVolumeClaimSpec always written
// because the CRD requires one and has no default for it.
func TestK3DLRSpecFromDefaults(t *testing.T) {
	s, err := k3dLRSpecFrom(k3dLRAddRequest{Name: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	if s.BootstrapMethod != "pgbackrest" {
		t.Errorf("bootstrapMethod default = %q", s.BootstrapMethod)
	}
	if len(s.DataVolume) == 0 || !strings.Contains(string(s.DataVolume), "1Gi") {
		t.Errorf("dataVolumeClaimSpec is required by the CRD: %s", s.DataVolume)
	}
	// ClusterIP is the operator's own default, so no expose block is written for it.
	if len(s.Expose) != 0 {
		t.Errorf("ClusterIP must not write an expose block: %s", s.Expose)
	}
	if lb, _ := k3dLRSpecFrom(k3dLRAddRequest{Name: "a", Expose: "LoadBalancer"}); !strings.Contains(string(lb.Expose), "LoadBalancer") {
		t.Error("LoadBalancer must write an expose block")
	}
	if _, err := k3dLRSpecFrom(k3dLRAddRequest{Name: "a", BootstrapMethod: "rsync"}); err == nil {
		t.Error("an unknown bootstrap method must be refused")
	}
	if _, err := k3dLRSpecFrom(k3dLRAddRequest{Name: "a", Expose: "Ingress"}); err == nil {
		t.Error("an unknown expose type must be refused")
	}
	// Databases are a set, and blanks never become an entry.
	dup, _ := k3dLRSpecFrom(k3dLRAddRequest{Name: "a", Databases: []string{" shop ", "shop", "", "analytics"}})
	if len(dup.Databases) != 2 || dup.Databases[0] != "shop" || dup.Databases[1] != "analytics" {
		t.Errorf("databases = %v", dup.Databases)
	}
}

// The objects the operator builds are named by concatenation, and the panel has to predict them
// to find the Service and the bootstrap Job.
func TestK3DLRName(t *testing.T) {
	if got := k3dLRName("cluster1", "analytics"); got != "cluster1-lr-analytics" {
		t.Errorf("got %q", got)
	}
}

// The backup check must put the newest backup's own timestamp on screen: "a backup exists" is not
// the requirement, "a backup taken after the databases were created" is, and only the user can
// judge that. The live failure this guards against is pg_createsubscriber stopping with
// `database "duh" does not exist` because the restore predated the database.
func TestK3DLRPreflightNamesTheBackupClock(t *testing.T) {
	cr := &k3dLRCluster{}
	cr.Spec.PostgresVersion = 18
	cr.Status.State = "ready"
	checks := k3dLRPreflight(k3dConfig{OperatorVer: "3.1.0"}, cr,
		k3dLRBackups{Succeeded: 2, Latest: "k3d-01-backup-5h6d", Completed: "2026-09-15T07:25:51Z"})
	for _, c := range checks {
		if !strings.Contains(c.Name, "backup") {
			continue
		}
		if !strings.Contains(c.Detail, "2026-09-15T07:25:51Z") {
			t.Errorf("the backup check must name when the newest backup finished: %q", c.Detail)
		}
		if !strings.Contains(c.Detail, "does not exist") {
			t.Errorf("it must name the failure it prevents: %q", c.Detail)
		}
		return
	}
	t.Error("no backup check")
}
