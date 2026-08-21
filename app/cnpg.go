package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CloudNativePG on a K3D frame. Unlike the four Percona operators, which DBCanvas installs
// by unpacking a release tarball and applying deploy/bundle.yaml (see installPXCOperator),
// CloudNativePG is installed with Helm — which the app can do without a helm binary in its
// image, because k3s bundles helm-controller and its HelmChart CRD. Applying a HelmChart
// manifest through the existing kubectlApply is a Helm install.
//
// The same mechanism installs kube-prometheus-stack for the optional monitoring, which is
// how the upstream quickstart does it too.

const (
	// The CloudNativePG chart and the namespace the operator runs in.
	cnpgChartRepo = "https://cloudnative-pg.github.io/charts"
	cnpgChart     = "cloudnative-pg"
	cnpgNamespace = "cnpg-system"
	// The Cluster CR's API group/version.
	cnpgAPIVersion = "postgresql.cnpg.io/v1"
	// Postgres images CNPG publishes. The tag is the major series; the operator tracks the
	// minor. Left empty the chart's own default applies, which is the safest thing for a
	// design that did not pin one.
	cnpgPGImageRepo = "ghcr.io/cloudnative-pg/postgresql"
	// kube-prometheus-stack: Prometheus + Alertmanager + Grafana in one chart.
	promChartRepo = "https://prometheus-community.github.io/helm-charts"
	promChart     = "kube-prometheus-stack"
	promNamespace = "monitoring"
	// CNPG's published alerting rules for the cluster it manages.
	cnpgPrometheusRuleURL = "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/docs/src/samples/monitoring/prometheusrule.yaml"

	// ---- barman-cloud backups, the plugin way ----
	// The in-tree spec.backup.barmanObjectStore is NOT usable here. The 1.30 operator's own
	// admission warning is explicit: "Native support for Barman Cloud backups and recovery is
	// deprecated and will be completely removed in CloudNativePG 1.31.0" — the next minor.
	// Since the chart installs the repo's latest by default, a design using the in-tree field
	// would start failing the moment 1.31 ships. So backups go through the Barman Cloud Plugin
	// (CNPG-I), which is the supported path and runs the same barman-cloud underneath.
	//
	// The plugin needs cert-manager (it serves gRPC over TLS to the operator) and must live in
	// the operator's own namespace.
	certManagerChartRepo = "https://charts.jetstack.io"
	certManagerChart     = "cert-manager"
	certManagerNamespace = "cert-manager"
	// The plugin's release manifest. Pinned: it carries CRDs and a Deployment whose contract
	// with the operator is version-specific, so tracking main would be a moving target.
	barmanPluginVersion     = "v0.14.0"
	barmanPluginManifestFmt = "https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/%s/manifest.yaml"
	// The name the plugin registers with the operator, used in spec.plugins and in a
	// ScheduledBackup's pluginConfiguration.
	barmanPluginName = "barman-cloud.cloudnative-pg.io"
	barmanObjectAPI  = "barmancloud.cnpg.io/v1"

	// The operator's own Deployment. Waiting for its CRDs is NOT enough before creating a
	// Cluster: the mutating webhook is served by this Deployment, and applying a Cluster
	// before it has endpoints fails with
	//   failed calling webhook "mcluster.cnpg.io": no endpoints available for service
	//   "cnpg-webhook-service"
	// It is a race — it passes whenever anything else happened to take a few seconds first.
	cnpgOperatorDeployment = "cloudnative-pg"
	// The phase string CNPG reports when the cluster is up.
	cnpgHealthyPhase = "Cluster in healthy state"
	cnpgPostgresPort = 5432
	// Grafana's Service, named by helm-controller after the HelmChart release.
	grafanaService = promChart + "-grafana"
	grafanaPort    = 80
	// The admin login promStackValues installs the chart with.
	grafanaAdminUser = "admin"

	// CNPG's published Grafana dashboard for the clusters it manages. Applied as a
	// ConfigMap the kube-prometheus-stack Grafana sidecar picks up — see
	// cnpgDashboardConfigMap. Without it the stack installs Grafana with Prometheus
	// wired up but no PostgreSQL dashboard to look at, which is the state a user
	// reasonably reports as "monitoring is not working".
	cnpgGrafanaDashboardURL = "https://raw.githubusercontent.com/cloudnative-pg/grafana-dashboards/main/charts/cluster/grafana-dashboard.json"

	// Where the manifests DBCanvas applies are archived on the first k3s node.
	//
	// The Percona operators leave their whole release tarball in /root (see
	// k3dOperatorDir), so "what was actually deployed" is readable after the fact.
	// CloudNativePG is installed from HelmChart CRDs and manifests generated in this
	// file and piped to `kubectl apply -f -`, so nothing would otherwise touch disk
	// and there would be nothing to review. Every manifest is therefore also written
	// here, numbered in apply order, with a README explaining the sequence.
	cnpgManifestDir = "/root/cnpg"
)

// Defaults for the CNPG knobs, so an older design (or a hand-written one) still deploys.
func cnpgInstances(f designFrame) int {
	if f.K3DCNPGInstances > 0 {
		return clampInt(f.K3DCNPGInstances, 1, 5)
	}
	return 3
}

func cnpgStorageGB(f designFrame) int {
	if f.K3DCNPGStorageGB > 0 {
		return clampInt(f.K3DCNPGStorageGB, 1, 512)
	}
	return 1
}

// cnpgExposeLoadBalancer reports whether the cluster's primary should get a LoadBalancer
// address. Default off: the other operators' expose settings default to in-cluster too, and an
// address is a finite resource from the MetalLB pool.
func cnpgExposeLoadBalancer(f designFrame) bool {
	return k3dExposeOf(f.K3DCNPGExpose, "clusterip") == "LoadBalancer"
}

func cnpgPoolerInstances(f designFrame) int {
	if f.K3DCNPGPoolerInstances > 0 {
		return clampInt(f.K3DCNPGPoolerInstances, 1, 5)
	}
	return 2
}

// cnpgPoolerMode is PgBouncer's pool mode. CNPG's own default is session; transaction is what
// makes pooling worth having for most workloads, but it is the caller's choice because it
// changes what the application may rely on (session state, prepared statements, advisory locks).
func cnpgPoolerMode(f designFrame) string {
	if strings.TrimSpace(f.K3DCNPGPoolerMode) == "transaction" {
		return "transaction"
	}
	return "session"
}

// cnpgPoolerExposeLoadBalancer reports whether the Pooler's Service should be a LoadBalancer.
// Independent of the primary's: pooling the primary while leaving Postgres itself in-cluster is
// the usual arrangement, and it is the reason the Pooler gets an expose setting of its own.
func cnpgPoolerExposeLoadBalancer(f designFrame) bool {
	return k3dExposeOf(f.K3DCNPGPoolerExpose, "clusterip") == "LoadBalancer"
}

func cnpgPoolerName(cluster string) string { return cluster + "-pooler-rw" }

// cnpgSecretValue reads and decodes one key out of a Secret. Empty on any problem — these feed
// display fields, and a blank row beats failing a deploy that otherwise worked.
func (a *App) cnpgSecretValue(ctx context.Context, serverID, ns, secret, key string) string {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "secret", secret,
		"-o", "go-template={{index .data \""+key+"\" | base64decode}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// helmChartManifest renders a k3s HelmChart object. targetNamespace is created if missing;
// version empty means "whatever the repo's latest is", which is what the UI's blank version
// offers. values is optional inline chart values (YAML, already indented at column 0).
func helmChartManifest(name, repo, chart, version, targetNamespace, values string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: helm.cattle.io/v1\nkind: HelmChart\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: kube-system\nspec:\n", name)
	fmt.Fprintf(&b, "  chart: %s\n  repo: %s\n", chart, repo)
	if v := strings.TrimSpace(version); v != "" {
		fmt.Fprintf(&b, "  version: %s\n", v)
	}
	fmt.Fprintf(&b, "  targetNamespace: %s\n  createNamespace: true\n", targetNamespace)
	if values = strings.TrimRight(values, "\n"); values != "" {
		b.WriteString("  valuesContent: |\n")
		for _, ln := range strings.Split(values, "\n") {
			b.WriteString("    " + ln + "\n")
		}
	}
	return []byte(b.String())
}

// cnpgArchive writes one file into cnpgManifestDir on the first k3s node. Best-effort:
// failing to archive must never fail a deploy that otherwise worked, so the error is logged
// and swallowed — but the caller is told, so it does not later claim an archive exists.
//
// The directory has to be created first. The k3s image is minimal and has no /root at all,
// so Docker's copy-archive API answers 404 for a path whose parent is missing; the Percona
// operator path hits the same thing and does the same mkdir (see k3dFetchOperator).
func (a *App) cnpgArchive(ctx context.Context, serverID, dir, file string, content []byte, pr *pxcProg) bool {
	if _, err := a.engCtx(ctx).Exec(ctx, serverID, []string{"mkdir", "-p", dir}, nil); err != nil {
		pr.logln("could not create " + dir + ": " + err.Error())
		return false
	}
	if err := a.engCtx(ctx).CopyFile(ctx, serverID, dir, file, 0o644, content); err != nil {
		pr.logln("could not archive " + file + " to " + dir + ": " + err.Error())
		return false
	}
	return true
}

// cnpgArchiver applies manifests and keeps the archive in step with them. It exists so the
// numbering and the README's file list come from the same place — a second counter threaded
// through the installer separately would drift the first time a resource was added.
type cnpgArchiver struct {
	app      *App
	serverID string
	// dir is where the copies land — cnpgManifestDir for CloudNativePG, pgoManifestDir for
	// Crunchy PGO, which installs the same way (a HelmChart plus manifests generated here,
	// none of which would otherwise touch disk).
	dir   string
	pr    *pxcProg
	step  int
	files []string
	// ok stays true only while every archive write has succeeded, so the final log line
	// and the node panel do not point at a directory that was never written.
	ok bool
}

// apply applies a manifest and archives a copy under cnpgManifestDir, numbered so a directory
// listing reads in apply order. Every CNPG resource goes through this rather than kubectlApply
// directly, so the directory is a complete and ordered record of what was deployed — which is
// the whole point of having it.
func (ar *cnpgArchiver) apply(ctx context.Context, ns, name string, manifest []byte) error {
	ar.step++
	file := fmt.Sprintf("%02d-%s.yaml", ar.step, name)
	ar.files = append(ar.files, file)
	if !ar.app.cnpgArchive(ctx, ar.serverID, ar.dir, file, manifest, ar.pr) {
		ar.ok = false
	}
	return ar.app.kubectlApply(ctx, ar.serverID, ns, manifest)
}

// applyLarge is apply for a manifest too big for `kubectl apply`.
//
// kubectl's client-side apply records the entire manifest in the object's
// kubectl.kubernetes.io/last-applied-configuration annotation, and annotations are capped at
// 256KiB in total — so a 253KiB Grafana dashboard is rejected outright with
// "metadata.annotations: Too long". Server-side apply tracks ownership in managedFields
// instead and writes no such annotation, so it has no size ceiling of its own.
func (ar *cnpgArchiver) applyLarge(ctx context.Context, ns, name string, manifest []byte) error {
	ar.step++
	file := fmt.Sprintf("%02d-%s.yaml", ar.step, name)
	ar.files = append(ar.files, file)
	if !ar.app.cnpgArchive(ctx, ar.serverID, ar.dir, file, manifest, ar.pr) {
		ar.ok = false
	}
	return ar.app.kubectlApplyServerSide(ctx, ar.serverID, ns, manifest)
}

// cnpgArchiveReadme explains the directory to whoever opens it. It names the apply order, what
// each file is, and how to re-apply — the questions someone reviewing a deployment actually
// has. Written last so it can state what really happened rather than what was intended.
func cnpgArchiveReadme(cfg *k3dConfig, ns string, files []string, monitoring bool, objectStore string) []byte {
	var b strings.Builder
	b.WriteString("# CloudNativePG deployment — the manifests DBCanvas applied\n\n")
	b.WriteString("Every file here was applied to this cluster with `kubectl apply -f -`, in the\n")
	b.WriteString("order the numeric prefixes give. They are a record, not live state: editing a\n")
	b.WriteString("file changes nothing until you re-apply it.\n\n")

	b.WriteString("## Why this directory exists\n\n")
	b.WriteString("The Percona operators (PXC, PSMDB, PGO) are installed by unpacking their release\n")
	b.WriteString("tarball into /root and applying `deploy/bundle.yaml` from it, so the source is\n")
	b.WriteString("already on disk to read. CloudNativePG is installed differently — the operator,\n")
	b.WriteString("cert-manager and kube-prometheus-stack are k3s `HelmChart` resources, and the rest\n")
	b.WriteString("are manifests DBCanvas generates and pipes to kubectl over stdin. Nothing would\n")
	b.WriteString("otherwise be written to disk, so copies are archived here instead.\n\n")

	fmt.Fprintf(&b, "## What was deployed\n\n")
	fmt.Fprintf(&b, "- Cluster:        %s (namespace `%s`)\n", cfg.ClusterName, ns)
	fmt.Fprintf(&b, "- Instances:      %d\n", cfg.CNPGInstances)
	fmt.Fprintf(&b, "- Storage:        %dGi per instance\n", cfg.CNPGStorageGB)
	if cfg.CNPGPGVersion != "" {
		fmt.Fprintf(&b, "- PostgreSQL:     %s\n", cfg.CNPGPGVersion)
	} else {
		b.WriteString("- PostgreSQL:     the chart's default major\n")
	}
	fmt.Fprintf(&b, "- Operator chart: %s %s (namespace `%s`)\n", cnpgChart, cnpgVerLabel(cfg.OperatorVer), cnpgNamespace)
	fmt.Fprintf(&b, "- Exposure:       %s", cfg.CNPGExpose)
	if cfg.CNPGEndpoint != "" {
		fmt.Fprintf(&b, " — %s", cfg.CNPGEndpoint)
	}
	b.WriteString("\n")
	if cfg.CNPGPooler {
		fmt.Fprintf(&b, "- PgBouncer:      %d pod(s), %s pooling, %s", cfg.CNPGPoolerInstances, cfg.CNPGPoolerMode, cfg.CNPGPoolerExpose)
		if cfg.CNPGPoolerEndpoint != "" {
			fmt.Fprintf(&b, " — %s", cfg.CNPGPoolerEndpoint)
		}
		b.WriteString("\n")
		b.WriteString("                  (a Pooler CR of type `rw`, so the pool follows the primary\n")
		b.WriteString("                  across a failover; the app role's credentials are unchanged)\n")
	} else {
		b.WriteString("- PgBouncer:      not enabled for this cluster\n")
	}
	if objectStore != "" {
		fmt.Fprintf(&b, "- Backups:        barman-cloud plugin %s → ObjectStore `%s`\n", barmanPluginVersion, objectStore)
	} else {
		b.WriteString("- Backups:        none (no SeaweedFS node linked, or the plugin was unavailable)\n")
	}
	if monitoring {
		fmt.Fprintf(&b, "- Monitoring:     kube-prometheus-stack in `%s`", promNamespace)
		if cfg.GrafanaURL != "" && cfg.GrafanaURL != "pending" {
			fmt.Fprintf(&b, " — Grafana at %s", cfg.GrafanaURL)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "                  (sign in as `%s`, password from $GRAFANA_PASSWORD — it is\n", grafanaAdminUser)
		fmt.Fprintf(&b, "                  also on the node's panel in DBCanvas; the PostgreSQL dashboard\n")
		b.WriteString("                  is loaded from the ConfigMap below by Grafana's sidecar)\n")
		fmt.Fprintf(&b, "                  Service: `%s`\n", promNamespace+"/"+grafanaService)
	} else {
		b.WriteString("- Monitoring:     not enabled for this cluster\n")
	}
	b.WriteString("\nCloudNativePG has no PMM integration — it is not a Percona product and ships no\n")
	b.WriteString("pmm-client sidecar — so Prometheus/Grafana above is the whole monitoring story.\n")

	b.WriteString("\n## Files, in apply order\n\n")
	for _, f := range files {
		b.WriteString("- `" + f + "`\n")
	}

	b.WriteString("\n## Reviewing and re-applying\n\n")
	b.WriteString("```sh\n")
	b.WriteString("export KUBECONFIG=" + k3dKubeconfig + "\n")
	b.WriteString("cd " + cnpgManifestDir + "\n\n")
	b.WriteString("cat 0*.yaml                      # read what was applied, in order\n")
	b.WriteString("kubectl apply -f 06-cluster.yaml # re-apply one resource after editing it\n\n")
	if monitoring {
		b.WriteString("# The Grafana dashboard is ~250KB, which does not fit in the annotation a\n")
		b.WriteString("# client-side apply writes (annotations are capped at 256KiB in total), so it\n")
		b.WriteString("# needs a server-side apply — which is how DBCanvas applied it too:\n")
		b.WriteString("kubectl apply --server-side --force-conflicts -f 11-grafana-dashboard.yaml\n\n")
	}
	fmt.Fprintf(&b, "kubectl -n %s get cluster,pods,pvc\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s get objectstore,scheduledbackup,backup\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s get podmonitor,prometheusrule\n", ns)
	fmt.Fprintf(&b, "kubectl -n %s get svc,configmap -l grafana_dashboard=1\n", promNamespace)
	b.WriteString("```\n\n")
	b.WriteString("The three HelmChart files are k3s `helm-controller` resources: applying one runs a\n")
	b.WriteString("Helm install/upgrade. `kubectl -n kube-system get helmchart` shows their state, and\n")
	b.WriteString("`kubectl -n kube-system logs job/helm-install-<name>` shows what a failed one did.\n")
	return []byte(b.String())
}

// cnpgObjectStoreManifest holds the barman-cloud S3 configuration as its own resource, which
// is how the plugin consumes it. spec.configuration has the same shape as the retired in-tree
// spec.backup.barmanObjectStore block, so the fields are familiar.
func cnpgObjectStoreManifest(name, ns string, s3 *crS3) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: ObjectStore\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n",
		barmanObjectAPI, name, ns)
	// destinationPath is a bucket URI. SeaweedFS is S3-compatible and path-style, which
	// barman-cloud handles via the explicit endpointURL. Unlike pgBackRest (see
	// k3dBackupIssues) barman-cloud goes over boto3 and does not require TLS, so a plain-HTTP
	// SeaweedFS node is a usable target.
	fmt.Fprintf(&b, "  configuration:\n    destinationPath: s3://%s/\n", s3.Bucket)
	fmt.Fprintf(&b, "    endpointURL: %s\n", s3.EndpointURL)
	b.WriteString("    s3Credentials:\n")
	fmt.Fprintf(&b, "      accessKeyId:\n        name: %s\n        key: AWS_ACCESS_KEY_ID\n", s3.Secret)
	fmt.Fprintf(&b, "      secretAccessKey:\n        name: %s\n        key: AWS_SECRET_ACCESS_KEY\n", s3.Secret)
	b.WriteString("    wal:\n      compression: gzip\n")
	b.WriteString("  retentionPolicy: \"30d\"\n")
	return []byte(b.String())
}

// cnpgClusterManifest builds the Cluster CR. objectStore non-empty attaches the barman-cloud
// plugin as the WAL archiver and backup provider.
//
// Monitoring is deliberately *not* spec.monitoring.enablePodMonitor: the 1.30 operator warns
// that field is deprecated and directs users to manage the PodMonitor themselves, so
// cnpgPodMonitorManifest writes one instead.
func cnpgClusterManifest(name, ns string, instances, storageGB int, pgVersion, objectStore string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: Cluster\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n",
		cnpgAPIVersion, name, ns)
	fmt.Fprintf(&b, "  instances: %d\n", instances)
	if v := strings.TrimSpace(pgVersion); v != "" {
		fmt.Fprintf(&b, "  imageName: %s:%s\n", cnpgPGImageRepo, v)
	}
	fmt.Fprintf(&b, "  storage:\n    size: %dGi\n", storageGB)
	if objectStore != "" {
		// isWALArchiver hands continuous WAL archiving to the plugin; without it the cluster
		// would take base backups with no WAL stream, which is not a restorable backup.
		fmt.Fprintf(&b, "  plugins:\n  - name: %s\n    isWALArchiver: true\n    parameters:\n      barmanObjectName: %s\n",
			barmanPluginName, objectStore)
	}
	return []byte(b.String())
}

// cnpgPoolerManifest renders the Pooler CR: PgBouncer in front of the cluster's primary.
// CloudNativePG wires the pooler to the cluster's own credentials by itself — no authQuery or
// secret of ours — so the app role logs in through PgBouncer with the same password as direct.
// serviceType is written into serviceTemplate only when it is not the ClusterIP the operator
// would create anyway, which keeps the archived manifest free of a no-op stanza.
func cnpgPoolerManifest(cluster, ns string, instances int, poolMode, serviceType string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: Pooler\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n",
		cnpgAPIVersion, cnpgPoolerName(cluster), ns)
	fmt.Fprintf(&b, "  cluster:\n    name: %s\n", cluster)
	fmt.Fprintf(&b, "  instances: %d\n", instances)
	// rw: the pool follows the primary, so it survives a failover the same way the -rw Service
	// does. A read-only pool would be a second Pooler, which the designer does not offer yet.
	fmt.Fprintf(&b, "  type: rw\n")
	fmt.Fprintf(&b, "  pgbouncer:\n    poolMode: %s\n", poolMode)
	if serviceType != "" && serviceType != "ClusterIP" {
		fmt.Fprintf(&b, "  serviceTemplate:\n    spec:\n      type: %s\n", serviceType)
	}
	return []byte(b.String())
}

// cnpgPodMonitorManifest is the PodMonitor the deprecated enablePodMonitor would have
// generated: it selects the cluster's pods by the label CNPG puts on them and scrapes their
// "metrics" port.
func cnpgPodMonitorManifest(cluster, ns string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    matchLabels:
      cnpg.io/cluster: %s
  podMetricsEndpoints:
  - port: metrics
`, cluster, ns, cluster))
}

// cnpgScheduledBackupManifest schedules a nightly base backup through the plugin. The
// schedule is a six-field cron (CNPG includes seconds), which is why it is not the
// five-field form.
func cnpgScheduledBackupManifest(cluster, ns string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: %s
kind: ScheduledBackup
metadata:
  name: %s-nightly
  namespace: %s
spec:
  schedule: "0 0 0 * * *"
  backupOwnerReference: self
  method: plugin
  pluginConfiguration:
    name: %s
  cluster:
    name: %s
`, cnpgAPIVersion, cluster, ns, barmanPluginName, cluster))
}

// promStackValues keeps kube-prometheus-stack small enough for a lab cluster: no
// Alertmanager, and Grafana on a LoadBalancer so MetalLB gives it an address on the stack
// network (the same treatment the other exposed services get).
// grafanaAdminPassword is the password promStackValues installs Grafana's admin with. It is the
// single source for it: installCNPGOperator sets it on the chart and provisionK3DFrame records
// the same value in the frame's k3dSecrets, and the two must not be able to disagree.
func grafanaAdminPassword() string { return envOr("GRAFANA_PASSWORD", "grafana_password") }

func promStackValues(grafanaPassword string) string {
	return fmt.Sprintf(`alertmanager:
  enabled: false
grafana:
  enabled: true
  adminPassword: %q
  service:
    type: LoadBalancer
  sidecar:
    dashboards:
      # The sidecar is what turns a labelled ConfigMap into a Grafana dashboard.
      # It is on by default, but its search namespace is not: left unset it looks
      # only in its own, so being explicit is what lets the CNPG dashboard be
      # applied from anywhere. See cnpgDashboardConfigMap.
      enabled: true
      label: grafana_dashboard
      labelValue: "1"
      searchNamespace: ALL
      folder: /tmp/dashboards
prometheus:
  prometheusSpec:
    # Watch every namespace, not just the chart's own release, so the CNPG
    # PodMonitor in the database namespace is actually scraped.
    podMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    retention: 7d
`, grafanaPassword)
}

// cnpgDashboardConfigMap wraps a published Grafana dashboard JSON in the ConfigMap shape
// the kube-prometheus-stack Grafana sidecar watches for: any ConfigMap labelled
// grafana_dashboard=1 has its data keys loaded as dashboards.
//
// The JSON is embedded as a block scalar rather than a quoted string so no escaping is needed;
// every line is indented four spaces under the key, and a blank line inside a block scalar is
// legal, so the JSON survives verbatim.
func cnpgDashboardConfigMap(name, key string, dashboard []byte) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: %s\n", name, promNamespace)
	b.WriteString("  labels:\n    grafana_dashboard: \"1\"\ndata:\n")
	fmt.Fprintf(&b, "  %s: |-\n", key)
	for _, ln := range strings.Split(strings.TrimRight(string(dashboard), "\n"), "\n") {
		b.WriteString("    " + ln + "\n")
	}
	return []byte(b.String())
}

// cnpgPrimaryServiceManifest exposes the cluster's *primary* on a LoadBalancer address, which
// is the only way to reach Postgres from outside the cluster — CNPG's own -rw/-ro/-r services
// are all ClusterIP. The selector is the pair CNPG puts on the pod that currently holds the
// primary role, so the address follows a failover rather than pinning to one instance.
func cnpgPrimaryServiceManifest(cluster, ns string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    cnpg.io/cluster: %s
spec:
  type: LoadBalancer
  selector:
    cnpg.io/cluster: %s
    cnpg.io/instanceRole: primary
  ports:
  - name: postgres
    port: %d
    targetPort: %d
`, cnpgPrimaryServiceName(cluster), ns, cluster, cluster, cnpgPostgresPort, cnpgPostgresPort))
}

func cnpgPrimaryServiceName(cluster string) string { return cluster + "-rw-lb" }

// waitForLoadBalancerIP polls a Service for the address its LoadBalancer implementation
// assigned. An empty return is not an error: MetalLB may have no free address, or the pool may
// never have been installed, and a cluster that is otherwise fine should not fail to deploy
// over an unassigned address — the caller records "pending" and says so.
func (a *App) waitForLoadBalancerIP(ctx context.Context, serverID, ns, svc string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		// Some implementations report a hostname rather than an IP; take whichever is set.
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "svc", svc, "-o",
			"jsonpath={.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}")
		if err == nil {
			if addr := strings.TrimSpace(out); addr != "" {
				return addr
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForCNPGCluster blocks until the Cluster reports itself healthy, returning the last status
// seen either way so the caller can record it. A cluster that is still converging is not a
// failure — the pods may simply still be pulling Postgres — so the timeout is not fatal.
func (a *App) waitForCNPGCluster(ctx context.Context, serverID, ns, name string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	last := "pending"
	for {
		last = a.cnpgStatus(ctx, serverID, ns, name)
		if strings.HasPrefix(last, cnpgHealthyPhase) {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForCRD blocks until a CRD is established, so a CR that depends on it is not applied
// into a cluster that would reject it. Helm-controller runs the install as a Job, so the
// CRDs appear a little after the HelmChart is accepted.
func (a *App) waitForCRD(ctx context.Context, serverID, crd string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := a.kubectl(ctx, serverID, "get", "crd", crd, "-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}")
		if err == nil && strings.Contains(out, "True") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for CRD %s", crd)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// installCNPGOperator installs CloudNativePG with Helm, optionally installs
// kube-prometheus-stack alongside it, then applies the Cluster CR (plus a ScheduledBackup
// when a SeaweedFS backup target is configured).
func (a *App) installCNPGOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	ns := cfg.Namespace

	// Everything applied below is also archived to cnpgManifestDir, numbered in apply
	// order. See cnpgArchiver.
	ar := &cnpgArchiver{app: a, serverID: serverID, dir: cnpgManifestDir, pr: pr, ok: true}
	apply := func(name, applyNS string, manifest []byte) error {
		return ar.apply(ctx, applyNS, name, manifest)
	}

	// ---- monitoring first ----
	// kube-prometheus-stack brings the PodMonitor CRD, and the Cluster below asks the
	// operator to create a PodMonitor. Applied the other way round the operator would log a
	// missing-CRD error and quietly leave the cluster unmonitored.
	monitoring := frame.K3DCNPGMonitoring
	if monitoring {
		pr.phase("Installing Prometheus + Grafana", 70)
		pw := grafanaAdminPassword()
		if err := apply("kube-prometheus-stack", "", helmChartManifest(
			promChart, promChartRepo, promChart, frame.K3DCNPGPromVersion, promNamespace, promStackValues(pw))); err != nil {
			return fmt.Errorf("apply the kube-prometheus-stack HelmChart: %w", err)
		}
		pr.logln("kube-prometheus-stack installing into " + promNamespace + " (Grafana admin password from GRAFANA_PASSWORD)")
		if err := a.waitForCRD(ctx, serverID, "podmonitors.monitoring.coreos.com", deployTimeout()); err != nil {
			// Not fatal: the database cluster is still worth having. But the PodMonitor must
			// then not be applied, so record that monitoring did not land.
			pr.logln("Prometheus CRDs did not appear, continuing without monitoring: " + err.Error())
			monitoring = false
		} else {
			cfg.MonitoredBy = "Prometheus/Grafana (" + promNamespace + ")"
			cfg.GrafanaUser = grafanaAdminUser
			cfg.GrafanaService = promNamespace + "/" + grafanaService
			pr.logln("PodMonitor CRD established")
			// Grafana is a LoadBalancer, so MetalLB gives it an address on the stack network.
			// The address is what makes the dashboards reachable at all, so it is worth
			// waiting for — but an unassigned address (no pool, or an exhausted one) must not
			// fail the deploy.
			if addr := a.waitForLoadBalancerIP(ctx, serverID, promNamespace, grafanaService, deployTimeout()); addr != "" {
				cfg.GrafanaURL = fmt.Sprintf("http://%s", addr)
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
	}

	// ---- the operator ----
	pr.phase("Installing the CloudNativePG operator", 75)
	if err := apply("cloudnative-pg-operator", "", helmChartManifest(
		cnpgChart, cnpgChartRepo, cnpgChart, cfg.OperatorVer, cnpgNamespace, "")); err != nil {
		return fmt.Errorf("apply the CloudNativePG HelmChart: %w", err)
	}
	pr.logln("CloudNativePG chart " + cnpgVerLabel(cfg.OperatorVer) + " installing into " + cnpgNamespace)
	if err := a.waitForCRD(ctx, serverID, "clusters.postgresql.cnpg.io", deployTimeout()); err != nil {
		return fmt.Errorf("CloudNativePG CRDs never became established: %w", err)
	}
	// The CRD is not enough. The operator serves the mutating webhook every Cluster create is
	// admitted through, so without waiting for its Deployment the apply below races it and
	// fails with "no endpoints available for service cnpg-webhook-service".
	if err := a.waitForDeployment(ctx, serverID, cnpgNamespace, cnpgOperatorDeployment, deployTimeout()); err != nil {
		return fmt.Errorf("CloudNativePG operator never became ready: %w", err)
	}
	pr.logln("Cluster CRD established and the operator's admission webhook is serving")

	// ---- the namespace the database cluster goes into ----
	if ns != "" && ns != "default" {
		if _, err := a.kubectl(ctx, serverID, "create", "namespace", ns); err != nil &&
			!strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
	}

	// ---- backups: the S3 credentials secret, before anything that references it ----
	pr.phase("Configuring backups", 82)
	s3 := a.k3dBackupSecret(ctx, st, frame, serverID, cfg, pr)
	objectStore := ""
	if s3 != nil {
		if err := a.installBarmanPlugin(ctx, ar, pr); err != nil {
			// The cluster is still worth deploying without backups, and saying so beats a
			// Cluster that references a plugin the operator has never heard of.
			pr.logln("barman-cloud plugin unavailable, deploying without backups: " + err.Error())
			cfg.BackupRepo = ""
		} else {
			objectStore = cfg.ClusterName + "-store"
			if err := apply("objectstore", ns, cnpgObjectStoreManifest(objectStore, ns, s3)); err != nil {
				pr.logln("ObjectStore skipped, deploying without backups: " + err.Error())
				objectStore, cfg.BackupRepo = "", ""
			} else {
				pr.logln("ObjectStore " + objectStore + " → " + s3.EndpointURL + " (bucket " + s3.Bucket + ")")
			}
		}
	}

	// ---- the Cluster CR ----
	pr.phase("Applying the Cluster CR", 88)
	manifest := cnpgClusterManifest(cfg.ClusterName, ns,
		cnpgInstances(frame), cnpgStorageGB(frame), frame.K3DCNPGVersion, objectStore)
	if err := apply("cluster", ns, manifest); err != nil {
		return fmt.Errorf("apply the CNPG Cluster: %w", err)
	}
	pr.logln(fmt.Sprintf("Cluster %s applied: %d instance(s), %dGi storage%s",
		cfg.ClusterName, cnpgInstances(frame), cnpgStorageGB(frame), cnpgPGLabel(frame.K3DCNPGVersion)))

	cfg.CNPGInstances = cnpgInstances(frame)
	cfg.CNPGStorageGB = cnpgStorageGB(frame)
	cfg.CNPGPGVersion = strings.TrimSpace(frame.K3DCNPGVersion)

	if objectStore != "" {
		if err := apply("scheduledbackup", ns, cnpgScheduledBackupManifest(cfg.ClusterName, ns)); err != nil {
			pr.logln("nightly ScheduledBackup skipped: " + err.Error())
		} else {
			pr.logln("nightly ScheduledBackup via barman-cloud (WAL archiving continuous)")
		}
	}

	// ---- wait for the cluster, then record how to reach it ----
	pr.phase("Waiting for the Postgres cluster", 92)
	cfg.CNPGStatus = a.waitForCNPGCluster(ctx, serverID, ns, cfg.ClusterName, deployTimeout())
	pr.logln("Cluster status: " + cfg.CNPGStatus)

	// CNPG generates the application role's password itself and puts it, with the database
	// name and host, in <cluster>-app. Only the non-secret parts are recorded here — the
	// password stays in the Secret, which the panel points at.
	cfg.CNPGAppSecret = cfg.ClusterName + "-app"
	cfg.CNPGAppUser = a.cnpgSecretValue(ctx, serverID, ns, cfg.CNPGAppSecret, "username")
	cfg.CNPGAppDB = a.cnpgSecretValue(ctx, serverID, ns, cfg.CNPGAppSecret, "dbname")

	// All three services CNPG creates (-rw, -ro, -r) are ClusterIP, so without this the
	// cluster is unreachable from outside Kubernetes.
	if cnpgExposeLoadBalancer(frame) {
		pr.phase("Exposing Postgres", 95)
		cfg.CNPGExpose = "LoadBalancer"
		if err := apply("primary-service", ns, cnpgPrimaryServiceManifest(cfg.ClusterName, ns)); err != nil {
			pr.logln("LoadBalancer Service skipped: " + err.Error())
			cfg.CNPGExpose = "ClusterIP"
		} else if addr := a.waitForLoadBalancerIP(ctx, serverID, ns, cnpgPrimaryServiceName(cfg.ClusterName), deployTimeout()); addr != "" {
			cfg.CNPGEndpoint = fmt.Sprintf("%s:%d", addr, cnpgPostgresPort)
			pr.logln("Postgres primary at " + cfg.CNPGEndpoint + " (follows failover — the Service selects the primary role)")
		} else {
			cfg.CNPGEndpoint = "pending"
			pr.logln("Postgres LoadBalancer has no address yet — check the MetalLB pool")
		}
	} else {
		cfg.CNPGExpose = "ClusterIP"
		cfg.CNPGEndpoint = fmt.Sprintf("%s-rw.%s.svc:%d", cfg.ClusterName, ns, cnpgPostgresPort)
	}

	// ---- PgBouncer, once the cluster it points at exists ----
	// A Pooler applied before the Cluster is admitted fine but sits idle until the cluster is
	// there, so it goes after the wait — and its own Service is independent of the primary's,
	// which is the point: the pool can be the only thing reachable from outside.
	if frame.K3DCNPGPooler {
		pr.phase("Deploying PgBouncer", 94)
		instances, mode := cnpgPoolerInstances(frame), cnpgPoolerMode(frame)
		svcType := "ClusterIP"
		if cnpgPoolerExposeLoadBalancer(frame) {
			svcType = "LoadBalancer"
		}
		pooler := cnpgPoolerName(cfg.ClusterName)
		if err := apply("pooler", ns, cnpgPoolerManifest(cfg.ClusterName, ns, instances, mode, svcType)); err != nil {
			// The cluster is fully usable without a pool, so this is a skip, not a failure.
			pr.logln("PgBouncer skipped: " + err.Error())
		} else {
			cfg.CNPGPooler = true
			cfg.CNPGPoolerInstances, cfg.CNPGPoolerMode, cfg.CNPGPoolerExpose = instances, mode, svcType
			pr.logln(fmt.Sprintf("Pooler %s applied: %d PgBouncer pod(s), %s pooling", pooler, instances, mode))
			if err := a.waitForDeployment(ctx, serverID, ns, pooler, deployTimeout()); err != nil {
				pr.logln("PgBouncer is not ready yet: " + err.Error())
			}
			if svcType == "LoadBalancer" {
				if addr := a.waitForLoadBalancerIP(ctx, serverID, ns, pooler, deployTimeout()); addr != "" {
					cfg.CNPGPoolerEndpoint = fmt.Sprintf("%s:%d", addr, cnpgPostgresPort)
					pr.logln("PgBouncer at " + cfg.CNPGPoolerEndpoint + " — same app role and password as a direct connection")
				} else {
					cfg.CNPGPoolerEndpoint = "pending"
					pr.logln("PgBouncer LoadBalancer has no address yet — check the MetalLB pool")
				}
			} else {
				cfg.CNPGPoolerEndpoint = fmt.Sprintf("%s.%s.svc:%d", pooler, ns, cnpgPostgresPort)
			}
		}
	}

	// ---- monitoring resources, now that the cluster exists to be selected ----
	if monitoring {
		if err := apply("podmonitor", ns, cnpgPodMonitorManifest(cfg.ClusterName, ns)); err != nil {
			pr.logln("PodMonitor skipped: " + err.Error())
		} else {
			pr.logln("PodMonitor " + cfg.ClusterName + " registered for scraping")
		}
		if rules, err := httpGetBytes(ctx, cnpgPrometheusRuleURL); err != nil {
			pr.logln("CNPG PrometheusRule skipped: " + err.Error())
		} else if err := apply("prometheusrule", ns, rules); err != nil {
			pr.logln("CNPG PrometheusRule skipped: " + err.Error())
		} else {
			pr.logln("CNPG alerting rules applied")
		}
		// The dashboard. Without it the stack gives you Grafana with Prometheus wired up
		// and nothing to look at — metrics are being collected, but no PostgreSQL view
		// exists to read them in. Non-fatal: a cluster with unviewed metrics still beats
		// a failed deploy.
		if dash, err := httpGetBytes(ctx, cnpgGrafanaDashboardURL); err != nil {
			pr.logln("Grafana PostgreSQL dashboard skipped: " + err.Error())
		} else if err := ar.applyLarge(ctx, promNamespace, "grafana-dashboard", cnpgDashboardConfigMap("cnpg-grafana-dashboard", "cloudnative-pg.json", dash)); err != nil {
			pr.logln("Grafana PostgreSQL dashboard skipped: " + err.Error())
		} else {
			cfg.GrafanaDashboard = "CloudNativePG"
			pr.logln("Grafana dashboard \"CloudNativePG\" loaded (sidecar picks up the ConfigMap; " +
				"it appears within a minute or so of Grafana starting)")
		}
	}

	// Written last, so it describes what actually happened rather than what was intended.
	if ar.ok && a.cnpgArchive(ctx, serverID, ar.dir, "README.md",
		cnpgArchiveReadme(cfg, ns, ar.files, monitoring, objectStore), pr) {
		cfg.ManifestDir = cnpgManifestDir
		pr.logln("manifests archived to " + cnpgManifestDir + " on the first k3s node (see its README)")
	} else {
		pr.logln("manifests could not be archived to " + cnpgManifestDir + " — the deployment itself is unaffected")
	}
	return nil
}

// installBarmanPlugin installs cert-manager (the plugin serves gRPC to the operator over TLS
// and gets its certificates from it) and then the plugin itself, which must land in the
// operator's own namespace.
func (a *App) installBarmanPlugin(ctx context.Context, ar *cnpgArchiver, pr *pxcProg) error {
	serverID := ar.serverID
	pr.phase("Installing cert-manager", 84)
	if err := ar.apply(ctx, "", "cert-manager", helmChartManifest(
		certManagerChart, certManagerChartRepo, certManagerChart, "", certManagerNamespace,
		"crds:\n  enabled: true\n")); err != nil {
		return fmt.Errorf("apply the cert-manager HelmChart: %w", err)
	}
	if err := a.waitForCRD(ctx, serverID, "certificates.cert-manager.io", deployTimeout()); err != nil {
		return fmt.Errorf("cert-manager CRDs never became established: %w", err)
	}
	// The plugin's Certificate resources are admitted by cert-manager's webhook, so the
	// webhook has to be serving before the manifest is applied — CRDs alone are not enough.
	if err := a.waitForDeployment(ctx, serverID, certManagerNamespace, "cert-manager-webhook", deployTimeout()); err != nil {
		return fmt.Errorf("cert-manager webhook never became ready: %w", err)
	}
	pr.logln("cert-manager ready")

	pr.phase("Installing the barman-cloud plugin", 86)
	url := fmt.Sprintf(barmanPluginManifestFmt, barmanPluginVersion)
	manifest, err := httpGetBytes(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch the barman-cloud plugin manifest: %w", err)
	}
	// The manifest carries its own namespace (cnpg-system) on every object.
	if err := ar.apply(ctx, "", "barman-cloud-plugin", manifest); err != nil {
		return fmt.Errorf("apply the barman-cloud plugin: %w", err)
	}
	if err := a.waitForCRD(ctx, serverID, "objectstores.barmancloud.cnpg.io", deployTimeout()); err != nil {
		return fmt.Errorf("barman-cloud ObjectStore CRD never became established: %w", err)
	}
	pr.logln("barman-cloud plugin " + barmanPluginVersion + " installed into " + cnpgNamespace)
	return nil
}

// waitForDeployment blocks until a Deployment reports at least one ready replica.
func (a *App) waitForDeployment(ctx context.Context, serverID, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "deploy", name, "-o", "jsonpath={.status.readyReplicas}")
		if err == nil {
			if n, cerr := strconv.Atoi(strings.TrimSpace(out)); cerr == nil && n > 0 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for deployment %s/%s", ns, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// cnpgVerLabel and cnpgPGLabel keep the log lines readable when a version is unpinned.
func cnpgVerLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(latest)"
	}
	return v
}

func cnpgPGLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return ", PostgreSQL " + strings.TrimSpace(v)
}

// cnpgStatus reports the Cluster's phase and ready instance count for the manager panel.
// Errors are folded into a human string rather than returned: the panel wants something to
// display, and "not reachable" is a legitimate thing to display.
func (a *App) cnpgStatus(ctx context.Context, serverID, ns, name string) string {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "cluster.postgresql.cnpg.io", name,
		"-o", "jsonpath={.status.phase}|{.status.readyInstances}|{.spec.instances}")
	if err != nil {
		return "unavailable"
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) < 3 || parts[0] == "" {
		return "pending"
	}
	ready, _ := strconv.Atoi(parts[1])
	want, _ := strconv.Atoi(parts[2])
	return fmt.Sprintf("%s (%d/%d ready)", parts[0], ready, want)
}
