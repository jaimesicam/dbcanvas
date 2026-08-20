package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every manifest below was validated with `kubectl apply --dry-run=server` against a live PGO
// 5.8.8 cluster on k3s v1.31, and the install itself was run end to end. These tests lock in the
// facts a server dry-run would NOT catch — the ones that make the difference between a cluster
// that works and one that looks fine and is unusable.

// The chart is an OCI artifact in Crunchy's own registry, so there is no index.yaml: `chart:`
// carries the whole oci:// reference and there is no `repo:` at all. helm-controller looks for
// an index.yaml whenever a repo is set, and Crunchy publishes none.
func TestPGOHelmChartIsAnOCIReference(t *testing.T) {
	m := string(pgoHelmChartManifest("5.8.8", "pgo"))
	for _, want := range []string{
		"kind: HelmChart",
		"  chart: oci://registry.developers.crunchydata.com/crunchydata/pgo",
		"  version: 5.8.8",
		"  targetNamespace: pgo",
		"  createNamespace: true",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("HelmChart missing %q:\n%s", want, m)
		}
	}
	if strings.Contains(m, "repo:") {
		t.Errorf("HelmChart sets repo: for an OCI chart — helm then looks for an index.yaml that does not exist:\n%s", m)
	}
	// A blank version is legitimate (the registry's newest); it must not emit an empty key.
	if strings.Contains(string(pgoHelmChartManifest("", "pgo")), "version:") {
		t.Error("a blank chart version emits an empty `version:`, which helm rejects")
	}
}

// The three labels are what make a DBCanvas-chosen password stick. PGO looks up existing pguser
// Secrets by this selector and re-derives the SCRAM verifier from the password it finds; an
// unlabelled Secret is invisible to that lookup and gets overwritten with a random password.
// Both behaviours were observed on a live 5.8.8 cluster.
func TestPGOUserSecretCarriesTheSelectorLabels(t *testing.T) {
	m := string(pgoUserSecret("hippo", "hippo", "s3cret"))
	for _, want := range []string{
		"  name: hippo-pguser-hippo",
		"    postgres-operator.crunchydata.com/cluster: hippo",
		"    postgres-operator.crunchydata.com/role: pguser",
		"    postgres-operator.crunchydata.com/pguser: hippo",
		"  password: s3cret",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("user secret missing %q:\n%s", want, m)
		}
	}
}

func TestPGOClusterManifest(t *testing.T) {
	m := string(pgoClusterManifest("hippo", "pgo", 2, 5, "17", "ClusterIP", "LoadBalancer", nil, false))
	for _, want := range []string{
		// v1beta1, not v1: PGO 5.x serves only v1beta1 and 6.x serves both, so this is the one
		// spelling that works across every version the picker offers.
		"apiVersion: postgres-operator.crunchydata.com/v1beta1",
		"kind: PostgresCluster",
		"  postgresVersion: 17",
		"    replicas: 2",
		"          storage: 5Gi",
		"  service:\n    type: ClusterIP",
		"      service:\n        type: LoadBalancer",
		// No S3 target: the cluster keeps a pgBackRest PVC repo rather than no repo at all.
		"        volume:",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("cluster manifest missing %q:\n%s", want, m)
		}
	}
	// The application role must NOT be a superuser: PGO's pgBouncer auth_query excludes
	// superusers (AND NOT pg_authid.rolsuper), so a superuser app role reaches the primary
	// directly but gets "no such user" through the pooler — verified live, both ways.
	if strings.Contains(m, "SUPERUSER") {
		t.Errorf("the application role is a superuser, which makes pgBouncer reject it:\n%s", m)
	}
	// Both users, and a database for the application one.
	for _, want := range []string{"  - name: postgres", "  - name: hippo", "    databases:\n    - hippo"} {
		if !strings.Contains(m, want) {
			t.Errorf("cluster manifest missing %q:\n%s", want, m)
		}
	}

	// With an S3 target the PVC repo is replaced, not added to — two repo1 definitions would
	// be rejected, and a leftover volume would silently keep backups inside the cluster.
	s3 := string(pgoClusterManifest("hippo", "pgo", 1, 1, "18", "", "", &crS3{
		Bucket: "b", Region: "r", EndpointURL: "https://sw.example.net:8333", Secret: "hippo-pgbackrest-secrets",
	}, false))
	for _, want := range []string{
		"      configuration:\n      - secret:\n          name: hippo-pgbackrest-secrets",
		"        repo1-s3-uri-style: path",
		"          bucket: b",
		"          endpoint: sw.example.net:8333", // pgBackRest wants host[:port], not a URL
	} {
		if !strings.Contains(s3, want) {
			t.Errorf("S3 cluster manifest missing %q:\n%s", want, s3)
		}
	}
	if strings.Contains(s3, "volumeClaimSpec") {
		t.Errorf("the S3 repo did not replace the PVC repo:\n%s", s3)
	}
	// No expose choice must leave the stanza out rather than emit an empty type.
	if strings.Contains(s3, "  service:") {
		t.Errorf("an unset expose emits an empty service stanza:\n%s", s3)
	}
}

// The frame's defaults have to produce a cluster that fits the frame's default budget, and the
// PostgreSQL major is REQUIRED by the CRD — unlike CloudNativePG's imageName there is no
// operator-side default to fall back on, so it can never be blank.
func TestPGOFrameDefaults(t *testing.T) {
	if n := pgoInstances(designFrame{}); n != 2 {
		t.Errorf("pgoInstances default = %d, want 2", n)
	}
	if n := pgoInstances(designFrame{K3DPGOInstances: 99}); n != 5 {
		t.Errorf("pgoInstances is unclamped: %d", n)
	}
	if n := pgoStorageGB(designFrame{}); n != 1 {
		t.Errorf("pgoStorageGB default = %d, want 1", n)
	}
	if v := pgoPGVersion(designFrame{}); v == "" {
		t.Error("pgoPGVersion is blank — spec.postgresVersion is required and the CRD rejects an empty value")
	}
	if v := pgoPGVersion(designFrame{K3DPGOVersion: "16"}); v != "16" {
		t.Errorf("pgoPGVersion(16) = %q", v)
	}
}

// A PostgresCluster reports no single phase string, so the card's status line is composed from
// the ready/desired counts — and readiness is "no tier is short".
func TestPGOShort(t *testing.T) {
	for status, want := range map[string]bool{
		"instance1 2/2 ready · pgBouncer 1/1": false,
		"instance1 1/2 ready · pgBouncer 1/1": true,
		"instance1 2/2 ready · pgBouncer 0/1": true,
		"instance1 0/2 ready":                 true,
		"instance1 0/0 ready":                 true, // nothing desired yet: not ready
		// The operator publishes status.instances[] in stages, so a count can be missing.
		// Reading an unparseable count as "fine" made a deploy declare a cluster ready
		// while its pods were still initialising — observed live.
		"instance1 0/<no value> ready":                 true,
		"instance1 2/2 ready · pgBouncer 0/<no value>": true,
		"": false,
	} {
		if got := pgoShort(status); got != want {
			t.Errorf("pgoShort(%q) = %v, want %v", status, got, want)
		}
	}
}

// PGO is installed from a chart, so its version must resolve against the chart catalogue. Going
// through the operator catalogue instead finds no "pgo" product and silently disables the
// operator — the frame deploys a plain Kubernetes cluster and says nothing.
func TestPGOResolvesAgainstTheChartCatalogue(t *testing.T) {
	if k3dChartOperator["pgo"] != pgoChart {
		t.Fatalf("pgo is not registered as a chart-installed operator")
	}
	if !k3dDeployableOperator["pgo"] {
		t.Fatal("pgo is not in the deployable set, so the frame would refuse it")
	}
	cat := loadChartCatalog()
	if _, ok := cat[pgoChart]; !ok {
		t.Skip("versions.yaml has no pgo chart entry — run `make versions`")
	}
	if v, ok := cat.resolveChartVersion(pgoChart, ""); !ok || v == "" {
		t.Errorf("a blank PGO version does not resolve to the catalogue's latest (%q, %v)", v, ok)
	}
	// The GitHub tags are NOT the installable set: v5.8.9 and v6.0.3 are tagged there but
	// their images answer 404. The catalogue comes from the registry, so it must not list them.
	for _, absent := range []string{"5.8.9", "6.0.3"} {
		if _, ok := cat.resolveChartVersion(pgoChart, absent); ok {
			t.Errorf("the catalogue offers PGO %s, whose image is not published to Crunchy's registry", absent)
		}
	}
}

// pgBackRest speaks S3 only over TLS. Crunchy's operator is what Percona's was forked from and
// runs the same pgBackRest, so a plain-HTTP SeaweedFS node must produce the same warning for
// both — otherwise a PGO frame silently backs up to a PVC while the bucket stays empty.
func TestPGOBackupWarningMatchesPercona(t *testing.T) {
	doc := designDoc{Nodes: []designNode{{ID: "sw", Type: "seaweedfs", Label: "sw-01", TLS: false}}}
	for _, op := range []string{"pg", "pgo"} {
		f := designFrame{Label: "k3d-00", K3DOperator: op, SeaweedFSNodeID: "sw"}
		got := k3dBackupIssues(f, doc)
		if len(got) != 1 || got[0].Level != "warning" {
			t.Errorf("%s frame with a plaintext SeaweedFS node: got %v, want one warning", op, got)
		}
	}
	// With TLS on there is nothing to warn about.
	doc.Nodes[0].TLS = true
	if got := k3dBackupIssues(designFrame{K3DOperator: "pgo", SeaweedFSNodeID: "sw"}, doc); len(got) != 0 {
		t.Errorf("a TLS SeaweedFS node still warns: %v", got)
	}
}

// ----------------------------------------------------------------------- monitoring

// The exporter is one field, and it must be absent unless asked for: it adds a container to
// every instance pod, which on a k3d budget is not free.
func TestPGOClusterManifestMonitoring(t *testing.T) {
	off := string(pgoClusterManifest("hippo", "pgo", 2, 1, "17", "", "", nil, false))
	if strings.Contains(off, "monitoring:") {
		t.Errorf("the exporter is configured on a frame that did not ask for it:\n%s", off)
	}
	on := string(pgoClusterManifest("hippo", "pgo", 2, 1, "17", "", "", nil, true))
	if !strings.Contains(on, "  monitoring:\n    pgmonitor:\n      exporter: {}\n") {
		t.Errorf("monitoring does not enable the pgMonitor exporter:\n%s", on)
	}
}

// The three relabelings are the difference between a scrape that works and dashboards that
// show data. Verified live: without them Prometheus collects every ccp_* series and every
// pgMonitor panel reads "No data", because they filter on labels a PodMonitor never sets.
func TestPGOPodMonitorRelabelsForPGMonitor(t *testing.T) {
	m := string(pgoPodMonitorManifest("hippo", "pgo"))
	for _, want := range []string{
		"  name: hippo-exporter",
		// The selector is the operator's own exporter label, which only exists on a pod that
		// actually carries the sidecar.
		"      postgres-operator.crunchydata.com/crunchy-postgres-exporter: \"true\"",
		"  - port: exporter",
		"    - targetLabel: exp_type\n      replacement: pg",
		"    - sourceLabels: [__meta_kubernetes_pod_label_postgres_operator_crunchydata_com_cluster]\n      targetLabel: cluster_name",
		// job must come from the pod, not the PodMonitor, or the dashboards cannot tell the
		// primary from its replicas.
		"    - sourceLabels: [__meta_kubernetes_pod_name]\n      targetLabel: job",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("PodMonitor missing %q:\n%s", want, m)
		}
	}
}

// pgMonitor's published dashboards cannot be shipped as-is: their template variables carry
// Crunchy's own datasource uid, and their saved selections name Crunchy's own test cluster.
// Both were observed on a rendered page — every panel said "No data" while the same queries
// returned rows through the API.
func TestPGODashboardJSONRepointsVariables(t *testing.T) {
	raw := []byte(`{
	  "title": "PostgreSQL Details",
	  "templating": {"list": [
	    {"name": "ccp_datasource", "type": "datasource", "query": "prometheus",
	     "current": {"text": "PROMETHEUS", "value": "PDC1078F23EBDF0E5"}},
	    {"name": "pgcluster", "type": "query", "datasource": {"type": "prometheus", "uid": "PDC1078F23EBDF0E5"},
	     "current": {"text": "iota", "value": "iota"}, "options": [{"text": "iota"}]}
	  ]},
	  "panels": [{"datasource": {"type": "prometheus", "uid": "${ccp_datasource}"}}]
	}`)
	out, err := pgoDashboardJSON(raw)
	if err != nil {
		t.Fatalf("pgoDashboardJSON: %v", err)
	}
	if strings.Contains(string(out), "PDC1078F23EBDF0E5") {
		t.Errorf("the foreign datasource uid survived:\n%s", out)
	}
	if strings.Contains(string(out), "iota") {
		t.Errorf("Crunchy's own cluster is still the saved selection:\n%s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the rewritten dashboard is not valid JSON: %v", err)
	}
	list := doc["templating"].(map[string]any)["list"].([]any)
	ds := list[0].(map[string]any)
	if cur := ds["current"].(map[string]any); cur["value"] != grafanaPromDatasourceUID {
		t.Errorf("the datasource variable was not repointed at this stack's Prometheus: %v", cur)
	}
	q := list[1].(map[string]any)
	// Repointed at the *variable*, not at a uid: that is what makes it work in any Grafana.
	if got := q["datasource"].(map[string]any)["uid"]; got != "${ccp_datasource}" {
		t.Errorf("query variable datasource = %v, want ${ccp_datasource}", got)
	}
	if len(q["current"].(map[string]any)) != 0 {
		t.Errorf("the query variable kept a saved selection: %v", q["current"])
	}
	// The panels are already correct upstream and must not be disturbed.
	if got := doc["panels"].([]any)[0].(map[string]any)["datasource"].(map[string]any)["uid"]; got != "${ccp_datasource}" {
		t.Errorf("panel datasource was rewritten to %v", got)
	}
}

// A dashboard that names its datasource variable something else must still be repointed, and
// a dashboard with no templating at all must not panic.
func TestPGODashboardJSONEdgeCases(t *testing.T) {
	out, err := pgoDashboardJSON([]byte(`{"templating": {"list": [
	  {"name": "ds", "type": "datasource"},
	  {"name": "cluster", "type": "query", "datasource": {"uid": "x"}}]}}`))
	if err != nil {
		t.Fatalf("pgoDashboardJSON: %v", err)
	}
	if !strings.Contains(string(out), `"uid": "${ds}"`) {
		t.Errorf("a differently-named datasource variable was not followed:\n%s", out)
	}
	if _, err := pgoDashboardJSON([]byte(`{"title": "x"}`)); err != nil {
		t.Errorf("a dashboard without templating: %v", err)
	}
	if _, err := pgoDashboardJSON([]byte(`not json`)); err == nil {
		t.Error("invalid JSON was accepted")
	}
}

// The ConfigMap is what the Grafana sidecar turns into a dashboard, so the label is what makes
// it appear at all, and the two dashboards must not collide on one name.
func TestPGODashboardConfigMaps(t *testing.T) {
	seenName, seenKey := map[string]bool{}, map[string]bool{}
	for _, d := range pgoDashboards {
		if seenName[d.Key] || seenKey[d.Title] {
			t.Errorf("duplicate dashboard %q", d.Key)
		}
		seenName[d.Key], seenKey[d.Title] = true, true
		cm := string(cnpgDashboardConfigMap("pgo-grafana-"+strings.TrimSuffix(d.Key, ".json"), d.Key,
			[]byte(`{"title":"x"}`)))
		for _, want := range []string{
			"  namespace: " + promNamespace,
			"    grafana_dashboard: \"1\"",
			"  " + d.Key + ": |-",
		} {
			if !strings.Contains(cm, want) {
				t.Errorf("dashboard ConfigMap for %s missing %q:\n%s", d.File, want, cm)
			}
		}
	}
}
