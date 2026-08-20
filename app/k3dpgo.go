package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// k3dpgo.go — Crunchy Postgres for Kubernetes (PGO, the Postgres Operator from Crunchy Data)
// on a K3D frame.
//
// It is installed the way CloudNativePG is — a k3s HelmChart object, so no helm binary is
// needed — with one difference that shapes this whole file: **Crunchy publishes the installer
// chart as an OCI artifact in their own registry, not on a Helm HTTP repository.** There is no
// index.yaml, so `repo:` is wrong and the chart reference itself is the oci:// URL. k3s'
// helm-controller pulls it anonymously; verified on k3s v1.31 against chart 5.8.8 and 6.0.2.
//
// The other consequence of that registry is the version list. GitHub tags are NOT the set of
// installable versions: v5.8.9 and v6.0.3 are tagged there, but their images answer 404 —
// "This container version is no longer available from the Crunchy Data Developer Program" —
// while everything the chart repository lists pulls without credentials. So the catalogue is
// scraped from the registry's own tags (images/versions.sh § pgo_chart_discover), and the
// chart version doubles as the operator version because Crunchy sets appVersion to match.
//
// Unlike the Percona operators there is no cr.yaml in a release tarball to rewrite: the chart
// installs the operator and nothing else, so the PostgresCluster is generated here and, like
// CloudNativePG's manifests, archived to the first node so what was deployed can be read back.

const (
	// The chart: its catalogue key under `charts:`, and the OCI reference a HelmChart pins.
	pgoChart    = "pgo"
	pgoChartRef = "oci://registry.developers.crunchydata.com/crunchydata/pgo"
	// The operator's Deployment, named by the chart. Waiting for it is not optional: the
	// PostgresCluster CRD arrives with the chart, and applying a cluster before the operator
	// has endpoints leaves a resource nothing reconciles.
	pgoDeployment = "pgo"
	pgoCRD        = "postgresclusters.postgres-operator.crunchydata.com"

	// The custom resource's API group/version.
	//
	// v1beta1 rather than v1 deliberately: PGO 5.x serves only v1beta1, PGO 6.x serves both
	// (checked in the 6.0.2 chart's own CRD, which lists v1 and v1beta1), so v1beta1 is the one
	// spelling that works across every version the picker offers. A v1 CR would fail on all of 5.x.
	pgoAPIVersion = "postgres-operator.crunchydata.com/v1beta1"

	// The labels PGO selects a user's Secret by. They are the whole reason a DBCanvas-chosen
	// password survives: the operator looks up existing pguser Secrets with this selector and
	// re-derives the SCRAM verifier from whatever password it finds, but a Secret WITHOUT these
	// labels is invisible to that lookup and is simply overwritten with a generated password.
	// Verified both ways on a live 5.8.8 cluster — unlabelled secrets came back random,
	// labelled ones kept POSTGRES_PASSWORD.
	pgoLabelCluster = "postgres-operator.crunchydata.com/cluster"
	pgoLabelRole    = "postgres-operator.crunchydata.com/role"
	pgoLabelUser    = "postgres-operator.crunchydata.com/pguser"

	pgoPostgresPort = 5432

	// ---- monitoring ----
	// The exporter is the operator's own: spec.monitoring.pgmonitor.exporter adds a
	// crunchy-postgres-exporter sidecar to every instance pod, pulls the image from the
	// operator's RELATED_IMAGE_PGEXPORTER (so it is always version-matched), creates the
	// monitoring user and its <cluster>-monitoring Secret, and serves plain-HTTP metrics.
	// Nothing about it needs configuring, which is why the frame knob is a single checkbox.
	pgoExporterPort = "exporter" // containerPort 9187, named by the operator
	// The label the operator puts on a pod that carries the exporter. It is the PodMonitor's
	// whole selector — it appears only once the exporter is enabled, so the PodMonitor cannot
	// match a pod that has nothing to scrape.
	pgoLabelExporter = "postgres-operator.crunchydata.com/crunchy-postgres-exporter"

	// pgMonitor's published Grafana dashboards. They are not in the operator repository and
	// PGO 6 dropped the bundled monitoring stack from postgres-operator-examples, so this is
	// where they live now. "development" is that repository's default branch.
	pgoDashboardBase = "https://raw.githubusercontent.com/CrunchyData/pgmonitor/development/grafana/postgres/"
	// The datasource kube-prometheus-stack provisions Grafana with. Verified on a live stack:
	// the chart pins the uid, which is what lets a dashboard be repointed at it (see
	// pgoDashboardJSON).
	grafanaPromDatasource    = "Prometheus"
	grafanaPromDatasourceUID = "prometheus"
	// Where the generated manifests are archived on the first k3s node (see cnpgManifestDir).
	pgoManifestDir = "/root/pgo"
)

// ------------------------------------------------------------------ frame knobs

// pgoInstances is how many Postgres pods the cluster runs (one primary + replicas).
//
// Two, not CloudNativePG's three: a PGO cluster is not just its Postgres pods. Each instance is
// a 4-container pod, and the cluster also runs a pgBackRest repo host and a pgBouncer — so two
// instances is already 4 pods on a k3d budget, and the default should fit the default budget.
func pgoInstances(f designFrame) int {
	if f.K3DPGOInstances > 0 {
		return clampInt(f.K3DPGOInstances, 1, 5)
	}
	return 2
}

func pgoStorageGB(f designFrame) int {
	if f.K3DPGOStorageGB > 0 {
		return clampInt(f.K3DPGOStorageGB, 1, 512)
	}
	return 1
}

// pgoExporterMaxPG is the newest PostgreSQL major the pgMonitor exporter works on.
//
// PGO 6.0.2 refuses it above this and says so only in its own debug log — "postgres_exporter
// not supported for pg18; use OTel for postgres 18 and later", reason
// ExporterNotSupportedForPostgresVersion. What makes it worth a constant rather than a
// release note is how it fails: the operator still adds the exporter sidecar, so the pod runs
// 5/5 and Prometheus scrapes it as up, but it never installs the `monitor` schema or creates
// the ccp_monitoring role ("ERROR: schema \"monitor\" does not exist"). The exporter then
// authenticates against a role that does not exist, publishes nothing but Go runtime metrics,
// and every pgMonitor dashboard reads "No data" with no error anywhere a user would look.
const pgoExporterMaxPG = 17

// pgoPGVersion is the PostgreSQL major spec.postgresVersion pins. Unlike CloudNativePG's
// imageName this field is REQUIRED and has no operator-side default, so an unset frame gets the
// catalogue's newest rather than an empty value that the CRD would reject.
//
// With monitoring on, "newest" means the newest the exporter actually works on: defaulting to
// a version that silently disables the metrics the checkbox just asked for is the worse
// surprise. An explicitly chosen version is left alone — k3dFrameIssues warns about it
// instead, because overriding what someone typed is worse still.
func pgoPGVersion(f designFrame) string {
	if v := strings.TrimSpace(f.K3DPGOVersion); v != "" {
		return v
	}
	newest := "17"
	if ov, ok := loadChartImageCatalog()[pgoPGImageKey]; ok && ov.Latest != "" {
		newest = ov.Latest
	}
	if f.K3DPGOMonitoring && !pgoExporterSupports(newest) {
		return strconv.Itoa(pgoExporterMaxPG)
	}
	return newest
}

// pgoExporterSupports reports whether the pgMonitor exporter works on a PostgreSQL major.
// An unparseable version is taken as supported: it is not this function's job to reject one.
func pgoExporterSupports(pgMajor string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(pgMajor, ".", 2)[0]))
	if err != nil {
		return true
	}
	return n <= pgoExporterMaxPG
}

// pgoPGImageKey is the `chart_images:` entry listing the PostgreSQL majors a PGO release can
// run. They come from the chart's own relatedImages, which is the only place the supported set
// is stated — the crunchy-postgres repository serves no tag list.
const pgoPGImageKey = "crunchy-postgres"

// pgoUsers are the roles the cluster is created with: the superuser PGO manages anyway, and an
// application user (with a like-named database) named after the cluster. Same shape as the
// Percona PostgreSQL operator's pgUsers, for the same reason — one known password per cluster.
func pgoUsers(cluster string) []string { return []string{"postgres", cluster} }

// ------------------------------------------------------------------ manifests

// pgoHelmChartManifest renders the HelmChart that installs PGO. It is helmChartManifest's OCI
// sibling: an OCI chart carries its name in the reference, so `chart:` is the full oci:// URL
// and there is no `repo:` — passing one makes helm look for an index.yaml that does not exist.
func pgoHelmChartManifest(version, targetNamespace string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: helm.cattle.io/v1\nkind: HelmChart\nmetadata:\n")
	b.WriteString("  name: pgo\n  namespace: kube-system\nspec:\n")
	fmt.Fprintf(&b, "  chart: %s\n", pgoChartRef)
	if v := strings.TrimSpace(version); v != "" {
		fmt.Fprintf(&b, "  version: %s\n", v)
	}
	fmt.Fprintf(&b, "  targetNamespace: %s\n  createNamespace: true\n", targetNamespace)
	return []byte(b.String())
}

// pgoUserSecret is the <cluster>-pguser-<user> Secret carrying the .env password, created
// BEFORE the PostgresCluster so the operator adopts it instead of generating one. The labels
// are load-bearing, not decoration — see pgoLabelCluster.
func pgoUserSecret(cluster, user, password string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s-pguser-%s
  labels:
    %s: %s
    %s: pguser
    %s: %s
type: Opaque
stringData:
  user: %s
  password: %s
`, cluster, user, pgoLabelCluster, cluster, pgoLabelRole, pgoLabelUser, user, user, password))
}

// pgoClusterManifest renders the PostgresCluster.
//
// The application user is deliberately NOT given SUPERUSER. PGO's pgBouncer authenticates
// through an auth_query whose function excludes superusers outright
// (`AND NOT pg_catalog.pg_authid.rolsuper`), so a superuser application role makes every
// connection through the pooler fail with "no such user" while direct connections to the
// primary work — which is a confusing way to lose the pooler. Verified on a live 5.8.8 cluster:
// dropping the attribute fixed it immediately, and nothing else changed.
func pgoClusterManifest(name, ns string, instances, storageGB int, pgVersion, exposePG, exposePGBouncer string, s3 *crS3, monitoring bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: PostgresCluster\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n",
		pgoAPIVersion, name, ns)
	fmt.Fprintf(&b, "  postgresVersion: %s\n", pgVersion)
	fmt.Fprintf(&b, "  users:\n  - name: postgres\n  - name: %s\n    databases:\n    - %s\n", name, name)
	if exposePG != "" {
		fmt.Fprintf(&b, "  service:\n    type: %s\n", exposePG)
	}
	fmt.Fprintf(&b, "  instances:\n  - name: instance1\n    replicas: %d\n", instances)
	fmt.Fprintf(&b, "    dataVolumeClaimSpec:\n      accessModes:\n      - ReadWriteOnce\n"+
		"      resources:\n        requests:\n          storage: %dGi\n", storageGB)
	// An empty exporter object is the whole configuration: the operator supplies the image,
	// the monitoring user and its Secret. Anything more is optional tuning.
	if monitoring {
		b.WriteString("  monitoring:\n    pgmonitor:\n      exporter: {}\n")
	}
	fmt.Fprintf(&b, "  proxy:\n    pgBouncer:\n      replicas: 1\n")
	if exposePGBouncer != "" {
		fmt.Fprintf(&b, "      service:\n        type: %s\n", exposePGBouncer)
	}
	b.WriteString("  backups:\n    pgbackrest:\n")
	if s3 != nil {
		b.WriteString(pgoBlock(pgBackRestGlobal(name, s3), 6))
		fmt.Fprintf(&b, "      repos:\n      - name: repo1\n")
		b.WriteString(pgoBlock(pgS3Repo(s3), 8))
	} else {
		fmt.Fprintf(&b, "      repos:\n      - name: repo1\n        volume:\n          volumeClaimSpec:\n"+
			"            accessModes:\n            - ReadWriteOnce\n            resources:\n"+
			"              requests:\n                storage: %dGi\n", storageGB)
	}
	return []byte(b.String())
}

// pgoBlock indents a YAML fragment to a column and returns it as manifest text. The Percona
// PostgreSQL operator's helpers (pgBackRestGlobal, pgS3Repo) emit unindented fragments for
// splicing into a rewritten cr.yaml; this file builds its manifest instead, so it needs them
// as a string rather than as lines.
func pgoBlock(fragment string, indent int) string {
	return strings.Join(crIndent(fragment, indent), "\n") + "\n"
}

// pgoArchiveReadme explains the archive directory: what was applied, in what order, and how to
// re-apply it. Written last so it describes what actually happened.
func pgoArchiveReadme(cfg *k3dConfig, ns string, files []string, backups string) []byte {
	var b strings.Builder
	b.WriteString("# Crunchy Postgres for Kubernetes — what DBCanvas applied\n\n")
	fmt.Fprintf(&b, "Cluster `%s` in namespace `%s`, PGO chart %s (operator %s), PostgreSQL %s.\n\n",
		cfg.ClusterName, ns, cfg.OperatorVer, cfg.OperatorVer, cfg.PGOPGVersion)
	b.WriteString("The operator is installed from Crunchy's OCI Helm chart through k3s' bundled\n" +
		"helm-controller: the HelmChart object below is the whole install. Everything after it\n" +
		"is a manifest generated by DBCanvas, applied in this order:\n\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - %s\n", f)
	}
	fmt.Fprintf(&b, "\nBackups: %s\n", backups)
	if cfg.GrafanaURL != "" {
		fmt.Fprintf(&b, "\nMonitoring: spec.monitoring.pgmonitor.exporter adds a crunchy-postgres-exporter\n"+
			"sidecar to every instance pod; the PodMonitor above relabels its series to the\n"+
			"exp_type/cluster_name/job labels pgMonitor's dashboards select on. Grafana: %s (%s).\n",
			cfg.GrafanaURL, cfg.GrafanaDashboard)
	}
	fmt.Fprintf(&b, "\nThe user Secrets are applied BEFORE the PostgresCluster on purpose. PGO adopts an\n"+
		"existing %s-pguser-<user> Secret and re-derives the SCRAM verifier from its password —\n"+
		"but only when the Secret carries the three postgres-operator.crunchydata.com labels it\n"+
		"selects by. Without them it generates a random password instead.\n", cfg.ClusterName)
	b.WriteString("\nRe-apply everything:\n\n    cd " + pgoManifestDir + "\n    for f in [0-9]*.yaml; do kubectl apply -f \"$f\"; done\n")
	return []byte(b.String())
}

// pgoDashboards are the pgMonitor dashboards DBCanvas loads into Grafana, in the order they
// should appear. PGBackrest is included because it is the one view CloudNativePG's dashboard
// has no equivalent of: the exporter publishes ccp_backrest_* straight from the repo host, so
// a cluster backing up to SeaweedFS can be watched from the same Grafana.
var pgoDashboards = []struct{ File, Key, Title string }{
	{"PG_Details.json", "pgo-pg-details.json", "PostgreSQL Details"},
	{"PGBackrest.json", "pgo-pgbackrest.json", "pgBackRest"},
}

// pgoDashboardJSON repoints a pgMonitor dashboard at the Grafana this stack installs.
//
// Shipped as published, these dashboards render every panel as "No data". Two reasons, both
// baked into the JSON and both invisible until you look at a rendered page:
//
//   - Every *template variable* carries a hardcoded datasource uid — Crunchy's own
//     (PDC1078F23EBDF0E5 at the time of writing) — while the panels correctly reference the
//     dashboard's `${ccp_datasource}` variable. That uid does not exist in any other Grafana,
//     so the variables error, the cluster/node selectors stay empty, and every panel that
//     interpolates them has nothing to query. Repointing them at the datasource variable is
//     what fixes it, and it is uid-agnostic: whatever they ship, the variables now follow the
//     picker at the top of the dashboard.
//   - The saved `current` of each query variable is Crunchy's own test cluster ("iota",
//     "iota_ip16_pg1"). Grafana honours a saved selection over a refresh-on-load, so the
//     dashboard opens pinned to a cluster that does not exist here. Cleared, the refresh
//     resolves them against this stack.
func pgoDashboardJSON(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse the dashboard: %w", err)
	}
	tmpl, _ := doc["templating"].(map[string]any)
	list, _ := tmpl["list"].([]any)
	// The name of the datasource variable is read from the dashboard rather than assumed, so
	// a dashboard that names it something other than ccp_datasource is still handled.
	dsVar := ""
	for _, item := range list {
		if v, ok := item.(map[string]any); ok && v["type"] == "datasource" {
			dsVar, _ = v["name"].(string)
			break
		}
	}
	for _, item := range list {
		v, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if v["type"] == "datasource" {
			v["current"] = map[string]any{
				"selected": true, "text": grafanaPromDatasource, "value": grafanaPromDatasourceUID,
			}
			continue
		}
		if dsVar != "" {
			v["datasource"] = map[string]any{"type": "prometheus", "uid": "${" + dsVar + "}"}
		}
		v["current"] = map[string]any{}
		v["options"] = []any{}
	}
	return json.MarshalIndent(doc, "", " ")
}

// pgoPodMonitorManifest selects the exporter sidecars and gives their series the labels
// pgMonitor's dashboards select on.
//
// The relabelings are the whole point. A PodMonitor labels a series with pod/namespace/
// container and a `job` naming the PodMonitor itself, while every pgMonitor dashboard queries
// exp_type="pg", cluster_name="<cluster>" and a per-instance job — labels their old bundled
// Prometheus synthesised in its own scrape config, which PGO 6 no longer ships. Without these
// three the scrape works perfectly and every dashboard is empty.
func pgoPodMonitorManifest(cluster, ns string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: %s-exporter
  namespace: %s
spec:
  selector:
    matchLabels:
      %s: "true"
  podMetricsEndpoints:
  - port: %s
    relabelings:
    # The dashboards' first filter: pgMonitor tags the PostgreSQL exporter this way to tell it
    # apart from its node and pgBouncer exporters.
    - targetLabel: exp_type
      replacement: pg
    # The cluster selector at the top of every dashboard.
    - sourceLabels: [__meta_kubernetes_pod_label_postgres_operator_crunchydata_com_cluster]
      targetLabel: cluster_name
    # The node selector. It has to be one job per instance pod, not one job for the
    # PodMonitor, or the dashboards cannot separate the primary from its replicas.
    - sourceLabels: [__meta_kubernetes_pod_name]
      targetLabel: job
`, cluster, ns, pgoLabelExporter, pgoExporterPort))
}

// ------------------------------------------------------------------ status

// pgoStatus renders the cluster's readiness as one line for the node card: the instance set's
// ready/desired plus pgBouncer's. PostgresCluster has no single phase string the way a CNPG
// Cluster does, so this composes one from status.instances[] and status.proxy.
func (a *App) pgoStatus(ctx context.Context, serverID, ns, name string) string {
	// Both counts are guarded, not just readyReplicas: the operator fills status.instances[]
	// in stages, and an entry that has a name but no replicas yet renders "<no value>" —
	// which is not a number, and used to be read as "nothing is short, so the cluster is
	// ready" the moment the CR was created. Rendering 0/0 instead keeps the wait waiting.
	tpl := "{{range .status.instances}}{{.name}} {{if .readyReplicas}}{{.readyReplicas}}{{else}}0{{end}}/{{if .replicas}}{{.replicas}}{{else}}0{{end}} ready{{end}}" +
		"{{with .status.proxy}}{{with .pgBouncer}} · pgBouncer {{if .readyReplicas}}{{.readyReplicas}}{{else}}0{{end}}/{{.replicas}}{{end}}{{end}}"
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "postgrescluster", name, "-o", "go-template="+tpl)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// waitForPGOCluster polls until every instance in the set is ready, returning the status line it
// ended on. It does not fail the deploy on a timeout: the operator keeps working, and a cluster
// still pulling images is better reported as "1/2 ready" on the card than as a failed deploy.
func (a *App) waitForPGOCluster(ctx context.Context, serverID, ns, name string, timeout time.Duration) string {
	deadline, last := time.Now().Add(timeout), ""
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return last
		}
		if s := a.pgoStatus(ctx, serverID, ns, name); s != "" {
			last = s
			// "instance1 2/2 ready · pgBouncer 1/1" — done when no tier is short.
			if !pgoShort(s) {
				return s
			}
		}
		time.Sleep(5 * time.Second)
	}
	return last
}

// pgoShort reports whether any "<ready>/<desired>" pair in a status line is short of its target.
func pgoShort(status string) bool {
	for _, f := range strings.Fields(status) {
		ready, desired, ok := strings.Cut(f, "/")
		if !ok {
			continue
		}
		r, err1 := strconv.Atoi(ready)
		d, err2 := strconv.Atoi(desired)
		if err1 != nil || err2 != nil {
			// A count that is not a number is a count the operator has not published yet.
			// Skipping it reads as "this tier is fine", which is the wrong way to be wrong:
			// the deploy then stops waiting and reports a cluster that is still starting.
			return true
		}
		if d == 0 || r < d {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ the installer

func (a *App) installPGOOperator(ctx context.Context, st Stack, frame designFrame, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	ns := cfg.Namespace
	cluster := cfg.ClusterName
	ar := &cnpgArchiver{app: a, serverID: serverID, dir: pgoManifestDir, pr: pr, ok: true}

	// ---- monitoring first ----
	// kube-prometheus-stack brings the PodMonitor CRD, and it has to exist before the
	// PodMonitor below is applied. Installing it first also means Grafana is starting while
	// the database cluster builds, so both are ready at about the same time.
	monitoring := frame.K3DPGOMonitoring
	if monitoring {
		pr.phase("Installing Prometheus + Grafana", 66)
		if err := ar.apply(ctx, "", "kube-prometheus-stack", helmChartManifest(
			promChart, promChartRepo, promChart, frame.K3DPGOPromVersion, promNamespace,
			promStackValues(grafanaAdminPassword()))); err != nil {
			return fmt.Errorf("apply the kube-prometheus-stack HelmChart: %w", err)
		}
		pr.logln("kube-prometheus-stack installing into " + promNamespace + " (Grafana admin password from GRAFANA_PASSWORD)")
		if err := a.waitForCRD(ctx, serverID, "podmonitors.monitoring.coreos.com", deployTimeout()); err != nil {
			// Not fatal: the cluster is still worth having. But nothing below may assume the
			// CRD, so record that monitoring did not land.
			pr.logln("Prometheus CRDs did not appear, continuing without monitoring: " + err.Error())
			monitoring = false
		} else {
			cfg.MonitoredBy = "Prometheus/Grafana (" + promNamespace + ")"
			cfg.GrafanaUser = grafanaAdminUser
			cfg.GrafanaService = promNamespace + "/" + grafanaService
			pr.logln("PodMonitor CRD established")
		}
	}

	// ---- the operator ----
	pr.phase("Installing Crunchy PGO", 70)
	if err := ar.apply(ctx, "kube-system", "pgo-helmchart",
		pgoHelmChartManifest(cfg.OperatorVer, ns)); err != nil {
		return fmt.Errorf("apply the PGO HelmChart: %w", err)
	}
	pr.logln("HelmChart pgo " + cfg.OperatorVer + " → namespace " + ns + " (" + pgoChartRef + ")")
	// helm-controller runs the install as a Job, so the CRD and the Deployment appear some time
	// after the HelmChart is accepted. Both waits are needed, and in this order.
	if err := a.waitForCRD(ctx, serverID, pgoCRD, 5*time.Minute); err != nil {
		return fmt.Errorf("the PostgresCluster CRD did not appear: %w", err)
	}
	if err := a.waitForDeployment(ctx, serverID, ns, pgoDeployment, 5*time.Minute); err != nil {
		return fmt.Errorf("the operator did not become ready: %w", err)
	}
	pr.logln("operator ready (deployment/" + pgoDeployment + " in " + ns + ")")

	// ---- the user secrets, BEFORE the cluster ----
	pr.phase("Creating user secrets", 80)
	pw := envOr("POSTGRES_PASSWORD", "postgres_password")
	for _, user := range pgoUsers(cluster) {
		if err := ar.apply(ctx, ns, "pguser-"+user, pgoUserSecret(cluster, user, pw)); err != nil {
			return fmt.Errorf("create the %s user secret: %w", user, err)
		}
	}
	cfg.PGOAppUser, cfg.PGOAppDB = cluster, cluster
	cfg.PGOAppSecret = cluster + "-pguser-" + cluster
	pr.logln("user secrets created for " + strings.Join(pgoUsers(cluster), ", ") + " (password from .env)")

	// ---- backups ----
	// pgBackRest has no plaintext S3 at all, so a SeaweedFS node with TLS off cannot be a repo.
	// The cluster then keeps a PVC repo rather than failing every backup — the same call the
	// Percona PostgreSQL operator makes, because it is the same pgBackRest.
	var s3 *crS3
	backups := "a PVC repo inside the cluster (pgBackRest repo1)"
	cfg.BackupRepo = "PVC (pgBackRest)"
	if frame.SeaweedFSNodeID != "" {
		sw, sec, serr := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
		switch {
		case serr != nil:
			pr.logln("backups skipped: " + serr.Error())
		case !sw.TLS:
			pr.logln("backups → the PVC repo: pgBackRest speaks S3 only over TLS, and " +
				sw.InternalEndpoint + " is plaintext — turn TLS on for the SeaweedFS node to back up to it")
		default:
			secret := cluster + "-pgbackrest-secrets"
			conf := fmt.Sprintf("[global]\nrepo1-s3-key=%s\nrepo1-s3-key-secret=%s\n",
				seaweedAccessKeyOf(sw, sec), sec.SecretKey)
			if _, err := a.kubectl(ctx, serverID, "-n", ns, "create", "secret", "generic", secret,
				"--from-literal=s3.conf="+conf); err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create the pgBackRest secret: %w", err)
			}
			s3 = &crS3{Bucket: sw.Bucket, Region: sw.Region, EndpointURL: sw.InternalEndpoint, Secret: secret}
			backups = "SeaweedFS S3, bucket " + sw.Bucket + " (pgBackRest repo1)"
			cfg.BackupRepo = "SeaweedFS S3 (" + sw.Bucket + ")"
			pr.logln("backups → " + sw.InternalEndpoint + " (bucket " + sw.Bucket + ", pgBackRest repo1)")
		}
	}

	// ---- the cluster ----
	pr.phase("Creating the PostgresCluster", 88)
	manifest := pgoClusterManifest(cluster, ns, cfg.PGOInstances, cfg.PGOStorageGB, cfg.PGOPGVersion,
		cfg.ExposePG, cfg.ExposePGBouncer, s3, monitoring)
	if err := ar.apply(ctx, ns, "postgrescluster", manifest); err != nil {
		return fmt.Errorf("apply the PostgresCluster: %w", err)
	}
	pr.logln(fmt.Sprintf("PostgresCluster %s: %d instance(s) × %dGi, PostgreSQL %s, pgBouncer (postgres %s / pgBouncer %s)",
		cluster, cfg.PGOInstances, cfg.PGOStorageGB, cfg.PGOPGVersion, cfg.ExposePG, cfg.ExposePGBouncer))

	pr.phase("Waiting for PostgreSQL", 94)
	cfg.PGOStatus = a.waitForPGOCluster(ctx, serverID, ns, cluster, deployTimeout())
	status := cfg.PGOStatus
	if status == "" {
		status = "not reported yet — the operator is still reconciling"
	}
	pr.logln("cluster status: " + status)

	// ---- how to reach it ----
	// pgBouncer is the front end, so its address is the one worth showing when it has one;
	// otherwise the primary's. Both speak TLS only — PGO's Postgres rejects plaintext.
	cfg.PGOEndpoint = fmt.Sprintf("%s-primary.%s.svc:%d", cluster, ns, pgoPostgresPort)
	if cfg.ExposePGBouncer == "LoadBalancer" {
		if ip := a.waitForLoadBalancerIP(ctx, serverID, ns, cluster+"-pgbouncer", 3*time.Minute); ip != "" {
			cfg.PGOEndpoint = fmt.Sprintf("%s:%d", ip, pgoPostgresPort)
		}
	} else if cfg.ExposePG == "LoadBalancer" {
		if ip := a.waitForLoadBalancerIP(ctx, serverID, ns, cluster+"-ha", 3*time.Minute); ip != "" {
			cfg.PGOEndpoint = fmt.Sprintf("%s:%d", ip, pgoPostgresPort)
		}
	}
	pr.logln("primary reachable at " + cfg.PGOEndpoint + " as " + cfg.PGOAppUser +
		" (sslmode=require — PGO's Postgres and pgBouncer both refuse plaintext)")

	// ---- monitoring resources, now that there are exporter pods to select ----
	if monitoring {
		if err := ar.apply(ctx, ns, "podmonitor", pgoPodMonitorManifest(cluster, ns)); err != nil {
			pr.logln("PodMonitor skipped: " + err.Error())
		} else {
			pr.logln("PodMonitor " + cluster + "-exporter registered for scraping (port " +
				pgoExporterPort + ", relabelled to exp_type/cluster_name/job)")
		}
		loaded := []string{}
		for _, d := range pgoDashboards {
			// Every step is non-fatal on its own: a cluster whose dashboard did not download
			// is still a working cluster, and saying which one failed beats failing the deploy.
			raw, err := httpGetBytes(ctx, pgoDashboardBase+d.File)
			if err != nil {
				pr.logln("Grafana dashboard " + d.Title + " skipped: " + err.Error())
				continue
			}
			fixed, err := pgoDashboardJSON(raw)
			if err != nil {
				pr.logln("Grafana dashboard " + d.Title + " skipped: " + err.Error())
				continue
			}
			name := "grafana-" + strings.TrimSuffix(d.Key, ".json")
			if err := ar.applyLarge(ctx, promNamespace, name,
				cnpgDashboardConfigMap("pgo-"+name, d.Key, fixed)); err != nil {
				pr.logln("Grafana dashboard " + d.Title + " skipped: " + err.Error())
				continue
			}
			loaded = append(loaded, d.Title)
		}
		if len(loaded) > 0 {
			cfg.GrafanaDashboard = strings.Join(loaded, ", ")
			pr.logln("Grafana dashboards loaded: " + cfg.GrafanaDashboard +
				" (the sidecar picks the ConfigMaps up within a minute of Grafana starting)")
		}
		// Left until here because MetalLB assigns the address while everything above is
		// happening, so by now it usually needs no waiting at all.
		if addr := a.waitForLoadBalancerIP(ctx, serverID, promNamespace, grafanaService, deployTimeout()); addr != "" {
			cfg.GrafanaURL = "http://" + addr
			if grafanaPort != 80 {
				cfg.GrafanaURL = fmt.Sprintf("http://%s:%d", addr, grafanaPort)
			}
			pr.logln("Grafana at " + cfg.GrafanaURL + " — sign in as " + grafanaAdminUser +
				"; the password is on this node's panel")
		} else {
			cfg.GrafanaURL = "pending"
			pr.logln("Grafana has no LoadBalancer address yet — check the MetalLB pool; the Service is " +
				cfg.GrafanaService)
		}
	}

	// ---- the archive ----
	if ar.ok && a.cnpgArchive(ctx, serverID, ar.dir, "README.md",
		pgoArchiveReadme(cfg, ns, ar.files, backups), pr) {
		cfg.ManifestDir = pgoManifestDir
		pr.logln("manifests archived to " + pgoManifestDir + " on the first k3s node (see its README)")
	} else {
		pr.logln("manifests could not be archived to " + pgoManifestDir + " — the deployment itself is unaffected")
	}
	return nil
}

// pgoExporterIssue flags a frame that asks for monitoring on a PostgreSQL the exporter does
// not support. Only an explicitly chosen version reaches here — an unset one is defaulted to
// a supported major by pgoPGVersion — so the wording is about a choice, not a default.
func pgoExporterIssue(f designFrame, name string) (issue, bool) {
	if f.K3DOperator != "pgo" || !f.K3DPGOMonitoring {
		return issue{}, false
	}
	v := pgoPGVersion(f)
	if pgoExporterSupports(v) {
		return issue{}, false
	}
	max := strconv.Itoa(pgoExporterMaxPG)
	return issue{"warning", "K3D cluster " + name + " monitors PostgreSQL " + v +
		", which Crunchy's pgMonitor exporter does not support (" + max + " is the newest it works on). " +
		"The exporter sidecar still runs and Prometheus still scrapes it, but the operator never creates its " +
		"monitoring role, so every dashboard stays empty. Pick " + max + " or turn monitoring off"}, true
}
