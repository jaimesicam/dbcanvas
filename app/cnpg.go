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
func promStackValues(grafanaPassword string) string {
	return fmt.Sprintf(`alertmanager:
  enabled: false
grafana:
  enabled: true
  adminPassword: %q
  service:
    type: LoadBalancer
prometheus:
  prometheusSpec:
    # Watch every namespace, not just the chart's own release, so the CNPG
    # PodMonitor in the database namespace is actually scraped.
    podMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    retention: 7d
`, grafanaPassword)
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

	// ---- monitoring first ----
	// kube-prometheus-stack brings the PodMonitor CRD, and the Cluster below asks the
	// operator to create a PodMonitor. Applied the other way round the operator would log a
	// missing-CRD error and quietly leave the cluster unmonitored.
	monitoring := frame.K3DCNPGMonitoring
	if monitoring {
		pr.phase("Installing Prometheus + Grafana", 70)
		pw := envOr("GRAFANA_PASSWORD", "grafana_password")
		if err := a.kubectlApply(ctx, serverID, "", helmChartManifest(
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
				pr.logln("Grafana at " + cfg.GrafanaURL + " (admin / GRAFANA_PASSWORD)")
			} else {
				cfg.GrafanaURL = "pending"
				pr.logln("Grafana has no LoadBalancer address yet — check the MetalLB pool; the Service is " +
					promNamespace + "/" + grafanaService)
			}
		}
	}

	// ---- the operator ----
	pr.phase("Installing the CloudNativePG operator", 75)
	if err := a.kubectlApply(ctx, serverID, "", helmChartManifest(
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
		if err := a.installBarmanPlugin(ctx, serverID, pr); err != nil {
			// The cluster is still worth deploying without backups, and saying so beats a
			// Cluster that references a plugin the operator has never heard of.
			pr.logln("barman-cloud plugin unavailable, deploying without backups: " + err.Error())
			cfg.BackupRepo = ""
		} else {
			objectStore = cfg.ClusterName + "-store"
			if err := a.kubectlApply(ctx, serverID, ns, cnpgObjectStoreManifest(objectStore, ns, s3)); err != nil {
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
	if err := a.kubectlApply(ctx, serverID, ns, manifest); err != nil {
		return fmt.Errorf("apply the CNPG Cluster: %w", err)
	}
	pr.logln(fmt.Sprintf("Cluster %s applied: %d instance(s), %dGi storage%s",
		cfg.ClusterName, cnpgInstances(frame), cnpgStorageGB(frame), cnpgPGLabel(frame.K3DCNPGVersion)))

	cfg.CNPGInstances = cnpgInstances(frame)
	cfg.CNPGStorageGB = cnpgStorageGB(frame)
	cfg.CNPGPGVersion = strings.TrimSpace(frame.K3DCNPGVersion)

	if objectStore != "" {
		if err := a.kubectlApply(ctx, serverID, ns, cnpgScheduledBackupManifest(cfg.ClusterName, ns)); err != nil {
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
		if err := a.kubectlApply(ctx, serverID, ns, cnpgPrimaryServiceManifest(cfg.ClusterName, ns)); err != nil {
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

	// ---- monitoring resources, now that the cluster exists to be selected ----
	if monitoring {
		if err := a.kubectlApply(ctx, serverID, ns, cnpgPodMonitorManifest(cfg.ClusterName, ns)); err != nil {
			pr.logln("PodMonitor skipped: " + err.Error())
		} else {
			pr.logln("PodMonitor " + cfg.ClusterName + " registered for scraping")
		}
		if rules, err := httpGetBytes(ctx, cnpgPrometheusRuleURL); err != nil {
			pr.logln("CNPG PrometheusRule skipped: " + err.Error())
		} else if err := a.kubectlApply(ctx, serverID, ns, rules); err != nil {
			pr.logln("CNPG PrometheusRule skipped: " + err.Error())
		} else {
			pr.logln("CNPG alerting rules applied")
		}
	}
	return nil
}

// installBarmanPlugin installs cert-manager (the plugin serves gRPC to the operator over TLS
// and gets its certificates from it) and then the plugin itself, which must land in the
// operator's own namespace.
func (a *App) installBarmanPlugin(ctx context.Context, serverID string, pr *pxcProg) error {
	pr.phase("Installing cert-manager", 84)
	if err := a.kubectlApply(ctx, serverID, "", helmChartManifest(
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
	if err := a.kubectlApply(ctx, serverID, "", manifest); err != nil {
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
