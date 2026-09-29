package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// k3deverest.go — OpenEverest on a K3D frame.
//
// OpenEverest (formerly Percona Everest) is not an operator in the sense the other six choices
// are. It is a database platform: a web UI and API server, its own operator that translates a
// DatabaseCluster into the right Percona custom resource, and — installed underneath it through
// OLM — the Percona operators themselves. So a frame running it creates no database: the chart
// installs the platform and the chosen operators, and databases are created from Everest's UI
// or API afterwards. That is the product being exercised; a cluster pre-made by DBCanvas would
// be one Everest never saw being asked for.
//
// Installed the way CloudNativePG is — a k3s HelmChart object, so no helm binary — from
// openeverest.github.io/helm-charts. Verified on k3s v1.36.4 against chart 1.16.2: the chart's
// hooks (the one that approves the operators' OLM InstallPlans is what makes the database
// operators appear at all, and the README says --no-hooks is unsupported) run fine under
// helm-controller, and the whole install settles in about two minutes.
//
// What the chart puts on the cluster, for anyone reading `kubectl get pods -A`:
//
//	everest-system      everest-server (UI + API on :8080), everest-operator
//	everest-olm         OLM itself, and the everest-catalog CatalogSource the operators come from
//	everest-monitoring  kube-state-metrics and the VictoriaMetrics operator
//	<db namespace>      one Deployment per chosen Percona operator, installed by OLM
//
// The operator versions are therefore not DBCanvas's to pick: each chart release pins a catalog,
// and OLM installs what that catalog's stable channel carries. They are read back from the
// DatabaseEngine objects the Everest operator keeps, one per operator, and recorded as landed.

const (
	// The chart: its catalogue key under `charts:`, the repository, and the release name.
	// "everest-core" is what the chart's own README installs as, and what everestctl upgrades.
	everestChart     = "openeverest"
	everestChartRepo = "https://openeverest.github.io/helm-charts"
	everestRelease   = "everest-core"
	// The chart supports no other namespace for its own components ("Currently, we do not
	// support specifying a different namespace for Everest" — the chart README).
	everestNamespace = "everest-system"
	// The server's Service and port, named by the chart (server.service.name / .port).
	everestService = "everest"
	everestPort    = 8080
	// Deployments the chart creates in everestNamespace; both are waited for.
	everestServerDeployment   = "everest-server"
	everestOperatorDeployment = "everest-operator"
	// The initial admin login. The chart stores server.initialAdminPassword in the
	// everest-accounts Secret (in plain text, as its README warns).
	everestAdminUser = "admin"
	// The DB namespace the chart's own default creates. The frame's namespace overrides it.
	everestDefaultDBNamespace = "everest"
	// The CRD a DatabaseEngine lives in, and the status the Everest operator gives one whose
	// OLM install has finished.
	everestEngineCRD       = "databaseengines.everest.percona.com"
	everestEngineInstalled = "installed"
	// Where the applied HelmChart is archived on the first k3s node (see cnpgManifestDir).
	everestManifestDir = "/root/openeverest"
	// The UI on the host, the way a PMM node's is: a NodePort Service of DBCanvas's own
	// (everestHostService — the chart's Service takes no nodePort value), published on a
	// host port by k3d's load balancer. The NodePort is fixed because the publish is fixed at
	// `k3d cluster create`, before the Service exists; one cluster per frame, so it never
	// collides with another frame's.
	everestHostService = "everest-host"
	everestNodePort    = 30808
)

// everestOperators is the database operators Everest can install into its DB namespace, in the
// order the UI lists them: the frame key, the chart's dbNamespace value, and the name of the
// DatabaseEngine (= the OLM package) it becomes.
var everestOperators = []struct {
	Key, Value, Engine string
}{
	{"pxc", "pxc", "percona-xtradb-cluster-operator"},
	{"psmdb", "psmdb", "percona-server-mongodb-operator"},
	{"pg", "postgresql", "percona-postgresql-operator"},
}

// k3dNoDatabaseOperator is the operator choices that deploy no database of their own — mirrored
// by K3D_NO_DATABASE in StackDesigner.jsx. Like a frame with no operator, there is nothing on
// one for a sim or a load tool to be pointed at: Everest's databases are made in its UI.
var k3dNoDatabaseOperator = map[string]bool{"everest": true}

// everestBrokenVersions are chart releases whose server cannot be used, with why. They are left
// out of the catalog (images/versions.sh), and a design pinned to one gets this reason instead of
// a bare "unknown version".
//
// 1.15.0: its everest-server answers every UI path with an empty 200 and 404s its own
// /static assets, so the browser gets a blank page — the API still works. Verified by swapping
// each release's server image into one cluster: 1.15.1, 1.15.2, 1.16.1 and 1.16.2 serve the UI.
var everestBrokenVersions = map[string]string{
	"1.15.0": "its server serves a blank page instead of the UI (fixed in 1.15.1)",
}

// everestReservedNamespaces are the chart's own namespaces. A DB namespace named after one of
// them would put Percona operators in among OLM or the Everest server.
var everestReservedNamespaces = map[string]bool{
	everestNamespace: true, "everest-olm": true, "everest-monitoring": true,
}

// everestChosen is the frame's operator selection, normalized: known keys only, in
// everestOperators order. Nothing chosen means all three, which is also the chart's default —
// a frame or API caller that says nothing gets what `helm install` with no flags would give.
func everestChosen(f designFrame) []string {
	want := map[string]bool{}
	for _, k := range f.K3DEverestOperators {
		want[strings.ToLower(strings.TrimSpace(k))] = true
	}
	var out []string
	for _, op := range everestOperators {
		if len(want) == 0 || want[op.Key] {
			out = append(out, op.Key)
		}
	}
	if len(out) == 0 { // only unknown keys: validation says so; deploy the default
		for _, op := range everestOperators {
			out = append(out, op.Key)
		}
	}
	return out
}

// everestUnknownOperators lists what the frame asked for that Everest cannot install.
func everestUnknownOperators(f designFrame) []string {
	known := map[string]bool{}
	for _, op := range everestOperators {
		known[op.Key] = true
	}
	var out []string
	for _, k := range f.K3DEverestOperators {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !known[k] {
			out = append(out, k)
		}
	}
	return out
}

// everestDBNamespace is the namespace the operators and the databases go into.
func everestDBNamespace(f designFrame) string {
	if ns := strings.TrimSpace(f.K3DNamespace); ns != "" && ns != "default" {
		return ns
	}
	return everestDefaultDBNamespace
}

func everestAdminPassword() string { return envOr("EVEREST_PASSWORD", "everest_password") }

// everestValues is the chart values DBCanvas sets; everything else is the chart's default.
//
//   - The server is a LoadBalancer so MetalLB gives the UI an address on the stack network,
//     reachable from any other node (the VNC desktop, say) — the chart's ClusterIP would leave
//     port-forward as the only way in.
//   - Telemetry is off: a lab cluster is not a user worth counting.
//   - The DB namespace and its operators are the frame's, spelled the chart's way.
func everestValues(dbNS string, chosen []string, password string) string {
	on := map[string]bool{}
	for _, k := range chosen {
		on[k] = true
	}
	var b strings.Builder
	b.WriteString("telemetry: false\n")
	b.WriteString("server:\n")
	fmt.Fprintf(&b, "  initialAdminPassword: %q\n", password)
	b.WriteString("  service:\n    type: LoadBalancer\n")
	b.WriteString("dbNamespace:\n  enabled: true\n")
	fmt.Fprintf(&b, "  namespaceOverride: %s\n", dbNS)
	b.WriteString("  telemetry: false\n")
	for _, op := range everestOperators {
		fmt.Fprintf(&b, "  %s: %t\n", op.Value, on[op.Key])
	}
	return b.String()
}

// everestPreviousHostPort is the host port this frame's UI was published on last time, so a
// redeploy keeps the same URL — PMM pins its ports for the same reason. 0 on a first deploy.
func (a *App) everestPreviousHostPort(stackID int64, nodeID string) int {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil || len(dep.Config) == 0 {
		return 0
	}
	var old k3dConfig
	if json.Unmarshal(dep.Config, &old) != nil {
		return 0
	}
	return old.EverestHostPort
}

// everestPickHostPort keeps want when nothing else has published it, else picks a free port.
// Called after the frame's old cluster is deleted, so a redeploy does not collide with its own
// predecessor's load balancer.
func (a *App) everestPickHostPort(ctx context.Context, want int) (int, error) {
	used, _ := a.engCtx(ctx).ListPublishedPorts(ctx)
	if want > 0 {
		if _, taken := used[want]; !taken {
			return want, nil
		}
	}
	for i := 0; i < 20; i++ {
		p, err := freeHostPort()
		if err != nil {
			return 0, err
		}
		if _, taken := used[p]; !taken {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free host port for the Everest UI")
}

// everestCreateArgs publishes the UI's NodePort on the host through k3d's load balancer,
// bound to CONTAINER_BIND_IP like every port DBCanvas publishes.
func everestCreateArgs(hostPort int) []string {
	return []string{"--port", fmt.Sprintf("%s:%d:%d/tcp@loadbalancer",
		envOr("CONTAINER_BIND_IP", "127.0.0.1"), hostPort, everestNodePort)}
}

// everestHostServiceManifest is the NodePort Service the host port lands on. Beside the
// chart's Service rather than a patch of it, so a helm upgrade never reverts it.
func everestHostServiceManifest() []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: dbcanvas
spec:
  type: NodePort
  selector:
    app.kubernetes.io/name: everest-server
    app.kubernetes.io/component: everest-server
  ports:
    - name: http
      protocol: TCP
      port: %d
      targetPort: %d
      nodePort: %d
`, everestHostService, everestNamespace, everestPort, everestPort, everestNodePort))
}

// everestFrameIssues is the part of k3dFrameIssues only an OpenEverest frame has.
func everestFrameIssues(f designFrame) []issue {
	if f.K3DOperator != "everest" {
		return nil
	}
	var out []issue
	name := f.Label
	if bad := everestUnknownOperators(f); len(bad) > 0 {
		out = append(out, issue{Level: "error", Message: "K3D cluster " + name + " asks OpenEverest for operator(s) " +
			strings.Join(bad, ", ") + " — it installs pxc, psmdb and pg"})
	}
	if ns := everestDBNamespace(f); everestReservedNamespaces[ns] {
		out = append(out, issue{Level: "error", Message: "K3D cluster " + name + ": " + ns +
			" is one of OpenEverest's own namespaces — pick another for the databases (the chart's default is everest)"})
	}
	// Everest has its own answers to both of these, configured in its UI (a monitoring
	// endpoint, a backup storage) — so a PMM or SeaweedFS node on the frame does nothing, and
	// silence would read as monitoring or backups that are wired up.
	if f.PMMNodeID != "" {
		out = append(out, issue{Level: "warning", Message: "K3D cluster " + name + " has a PMM node selected, which DBCanvas does not wire into OpenEverest — " +
			"add it in Everest's UI as a monitoring endpoint instead (Settings → Monitoring endpoints)"})
	}
	if f.SeaweedFSNodeID != "" {
		out = append(out, issue{Level: "warning", Message: "K3D cluster " + name + " has a SeaweedFS backup store selected, which DBCanvas does not wire into OpenEverest — " +
			"add it in Everest's UI as a backup storage instead (Settings → Backup storages)"})
	}
	// The platform alone is ~10 pods before any database; each operator adds one.
	if cpus, memGB := k3dCPUs(f), k3dMemoryGB(f); cpus < 6 || memGB < 10 {
		out = append(out, issue{Level: "warning", Message: fmt.Sprintf("K3D cluster %s runs OpenEverest — about ten pods (server, operator, OLM, monitoring) before any database — against %d CPU / %d GiB; below 6 CPU / 10 GiB a 3-node database created from it is unlikely to schedule", name, cpus, memGB)})
	}
	return out
}

// installEverest installs OpenEverest from its Helm chart, waits for the server and for every
// chosen operator to finish installing, and records how to sign in.
func (a *App) installEverest(ctx context.Context, frame designFrame, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	dbNS := cfg.Namespace
	chosen := everestChosen(frame)
	password := everestAdminPassword()
	ar := &cnpgArchiver{app: a, serverID: serverID, dir: everestManifestDir, pr: pr, ok: true}

	pr.phase("Installing OpenEverest", 70)
	if err := ar.apply(ctx, "", "openeverest", helmChartManifest(
		everestRelease, everestChartRepo, everestChart, cfg.OperatorVer, everestNamespace,
		everestValues(dbNS, chosen, password))); err != nil {
		return fmt.Errorf("apply the OpenEverest HelmChart: %w", err)
	}
	pr.logln("OpenEverest chart " + cnpgVerLabel(cfg.OperatorVer) + " installing into " + everestNamespace +
		" (databases and operators in " + dbNS + ": " + strings.Join(chosen, ", ") + ")")

	// The chart's install is a helm-controller Job, and its failure is the useful error: a
	// Deployment that never appears says nothing about why.
	if err := a.waitHelmInstall(ctx, serverID, everestRelease, deployTimeout()); err != nil {
		return err
	}
	pr.logln("Helm release " + everestRelease + " installed")
	for _, d := range []string{everestOperatorDeployment, everestServerDeployment} {
		if err := a.waitForDeployment(ctx, serverID, everestNamespace, d, deployTimeout()); err != nil {
			return fmt.Errorf("OpenEverest never became ready: %w", err)
		}
	}
	cfg.EverestUser = everestAdminUser
	cfg.EverestService = everestNamespace + "/" + everestService
	pr.logln("Everest server and operator are running")

	// ---- the database operators, through OLM ----
	pr.phase("Waiting for the database operators", 85)
	if err := a.waitForCRD(ctx, serverID, everestEngineCRD, deployTimeout()); err != nil {
		pr.logln("DatabaseEngine CRD never appeared, so the operators' state is unknown: " + err.Error())
	} else {
		cfg.EverestEngines = a.waitEverestEngines(ctx, serverID, dbNS, chosen, deployTimeout(), pr)
	}

	// ---- the host port, like a PMM node's ----
	pr.phase("Exposing the Everest UI", 95)
	if cfg.EverestHostPort > 0 {
		if err := ar.apply(ctx, everestNamespace, "host-service", everestHostServiceManifest()); err != nil {
			pr.logln("the Everest UI is NOT published on the host: " + err.Error())
			cfg.EverestHostPort = 0
		} else {
			// Read back what Docker actually bound rather than trusting the request.
			lb := k3dContainerPrefix + cfg.Cluster + "-serverlb"
			if id, ok, _ := a.engCtx(ctx).ContainerByName(ctx, lb); ok {
				if hp, err := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", everestNodePort)); err == nil {
					if p, err := strconv.Atoi(hp); err == nil {
						cfg.EverestHostPort = p
					}
				}
			}
			pr.logln(fmt.Sprintf("the Everest UI is published on host port %d (open it from this node's panel)", cfg.EverestHostPort))
		}
	}
	if addr := a.waitForLoadBalancerIP(ctx, serverID, everestNamespace, everestService, deployTimeout()); addr != "" {
		cfg.EverestURL = fmt.Sprintf("http://%s:%d", addr, everestPort)
		pr.logln("OpenEverest at " + cfg.EverestURL + " — sign in as " + everestAdminUser + "; the password is on this node's panel")
	} else {
		cfg.EverestURL = "pending"
		pr.logln("the Everest Service has no LoadBalancer address yet — check the MetalLB pool; the Service is " + cfg.EverestService)
	}

	if ar.ok && a.cnpgArchive(ctx, serverID, ar.dir, "README.md", everestArchiveReadme(cfg, dbNS, chosen, ar.files), pr) {
		cfg.ManifestDir = everestManifestDir
		pr.logln("the HelmChart is archived to " + everestManifestDir + " on the first k3s node (see its README)")
	}
	return nil
}

// waitHelmInstall waits for helm-controller's install Job for a HelmChart, and on failure
// returns the tail of its log — which is where a bad value or a failed hook actually says so.
func (a *App) waitHelmInstall(ctx context.Context, serverID, release string, timeout time.Duration) error {
	job := "helm-install-" + release
	deadline := time.Now().Add(timeout)
	for {
		out, err := a.kubectl(ctx, serverID, "-n", "kube-system", "get", "job", job,
			"-o", "jsonpath={.status.succeeded}|{.status.failed}")
		if err == nil {
			parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
			if parts[0] != "" && parts[0] != "0" {
				return nil
			}
			// helm-controller retries a failed install by restarting the pod, so a failure
			// count is not final — but three of them is a chart that is not going to install.
			failed := 0
			if len(parts) == 2 {
				failed, _ = strconv.Atoi(parts[1])
			}
			if failed >= 3 {
				tail, _ := a.kubectl(ctx, serverID, "-n", "kube-system", "logs", "job/"+job, "--tail=15")
				return fmt.Errorf("the Helm install of %s failed:\n%s", release, strings.TrimSpace(tail))
			}
		}
		if time.Now().After(deadline) {
			tail, _ := a.kubectl(ctx, serverID, "-n", "kube-system", "logs", "job/"+job, "--tail=15")
			return fmt.Errorf("timed out waiting for the Helm install of %s:\n%s", release, strings.TrimSpace(tail))
		}
		if !k3dSleep(ctx, 5*time.Second) {
			return ctx.Err()
		}
	}
}

// waitEverestEngines waits until every chosen operator's DatabaseEngine reports installed, and
// returns one line per operator saying what landed ("pxc 1.20.0", or its state if it did not).
// Not fatal on timeout: Everest itself is up, and an operator still installing finishes on its
// own — the panel shows how far it got.
func (a *App) waitEverestEngines(ctx context.Context, serverID, ns string, chosen []string, timeout time.Duration, pr *pxcProg) []string {
	engineOf := map[string]string{}
	for _, op := range everestOperators {
		engineOf[op.Key] = op.Engine
	}
	deadline := time.Now().Add(timeout)
	reported := map[string]bool{}
	for {
		states := a.everestEngineStates(ctx, serverID, ns)
		var lines []string
		done := true
		for _, k := range chosen {
			st := states[engineOf[k]]
			if st.Status == everestEngineInstalled {
				lines = append(lines, strings.TrimSpace(k+" "+st.Version))
				if !reported[k] {
					pr.logln(k3dOperatorLabel(k) + " " + st.Version + " installed in " + ns)
					reported[k] = true
				}
				continue
			}
			done = false
			lines = append(lines, k+" "+orDefault(st.Status, "pending"))
		}
		if done || time.Now().After(deadline) || ctx.Err() != nil {
			if !done {
				pr.logln("not every operator has finished installing (" + strings.Join(lines, ", ") + ") — DBCanvas has stopped waiting; OLM has not")
			}
			return lines
		}
		if !k3dSleep(ctx, 10*time.Second) {
			return lines
		}
	}
}

type everestEngineState struct{ Status, Version string }

// everestEngineStates reads every DatabaseEngine in the DB namespace, by name.
func (a *App) everestEngineStates(ctx context.Context, serverID, ns string) map[string]everestEngineState {
	out, err := a.kubectl(ctx, serverID, "-n", ns, "get", "databaseengines",
		"-o", `go-template={{range .items}}{{.metadata.name}}|{{.status.status}}|{{.status.operatorVersion}}{{"\n"}}{{end}}`)
	res := map[string]everestEngineState{}
	if err != nil {
		return res
	}
	for _, ln := range strings.Split(out, "\n") {
		p := strings.SplitN(strings.TrimSpace(ln), "|", 3)
		if len(p) != 3 || p[0] == "" {
			continue
		}
		clean := func(s string) string {
			if s == "<no value>" {
				return ""
			}
			return s
		}
		res[p[0]] = everestEngineState{Status: clean(p[1]), Version: clean(p[2])}
	}
	return res
}

// everestArchiveReadme explains the archive directory, as cnpgArchiveReadme does for CNPG's.
func everestArchiveReadme(cfg *k3dConfig, dbNS string, chosen, files []string) []byte {
	var b strings.Builder
	b.WriteString("# OpenEverest — what DBCanvas applied\n\n")
	b.WriteString("OpenEverest was installed from its Helm chart through k3s' helm-controller, so\n")
	b.WriteString("the only thing DBCanvas applied is the HelmChart resource below. Everything else\n")
	b.WriteString("on the cluster was created by the chart, its hooks, and OLM.\n\n")
	b.WriteString("## What was deployed\n\n")
	fmt.Fprintf(&b, "- Chart:          %s %s (release `%s`, namespace `%s`)\n", everestChart, cnpgVerLabel(cfg.OperatorVer), everestRelease, everestNamespace)
	fmt.Fprintf(&b, "- DB namespace:   `%s`\n", dbNS)
	fmt.Fprintf(&b, "- Operators:      %s\n", strings.Join(chosen, ", "))
	for _, e := range cfg.EverestEngines {
		fmt.Fprintf(&b, "                  %s\n", e)
	}
	fmt.Fprintf(&b, "- UI / API:       %s (Service `%s`)\n", orDefault(cfg.EverestURL, "—"), cfg.EverestService)
	fmt.Fprintf(&b, "                  sign in as `%s`; the password is from $EVEREST_PASSWORD\n", everestAdminUser)
	b.WriteString("\n## Files\n\n")
	for _, f := range files {
		b.WriteString("- `" + f + "`\n")
	}
	b.WriteString("\n## Looking around\n\n```sh\n")
	b.WriteString("export KUBECONFIG=" + k3dKubeconfig + "\n")
	b.WriteString("kubectl -n kube-system get helmchart " + everestRelease + "\n")
	b.WriteString("kubectl -n kube-system logs job/helm-install-" + everestRelease + "\n")
	fmt.Fprintf(&b, "kubectl -n %s get databaseengines        # one per operator, with its version\n", dbNS)
	fmt.Fprintf(&b, "kubectl -n %s get subscription,csv,installplan\n", dbNS)
	fmt.Fprintf(&b, "kubectl -n %s get databaseclusters       # what the UI has created\n", dbNS)
	b.WriteString("kubectl -n " + everestNamespace + " get secret everest-accounts -o jsonpath='{.data.users\\.yaml}' | base64 -d\n")
	b.WriteString("```\n\n")
	b.WriteString("To add another operator to the DB namespace later, edit the HelmChart's\n")
	b.WriteString("valuesContent (dbNamespace.pxc / psmdb / postgresql) and re-apply it: that is a\n")
	b.WriteString("`helm upgrade`. Removing an operator is not supported by the chart.\n")
	return []byte(b.String())
}
