package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// k3d.go — the K3D cluster frame: a throwaway k3s cluster (1–3 nodes) created by k3d, used to run
// the Percona Kubernetes operators the way they are actually run in production.
//
// Where k3d runs. k3d is a Docker API *client*: it tells the daemon to create the k3s containers.
// DBCanvas already holds the daemon socket, so it runs the k3d binary itself — baked into the app
// image, or the host's binary in local dev (where k3d sits next to Docker). validateStack refuses
// the deploy when the binary is missing, rather than failing halfway through.
//
// The cluster is created **on the stack network** (--network dbcanvas-stack-<id>). That one flag is
// what makes the rest work: the k3s nodes get Intranet DNS names like any other node, pods can
// reach the PMM and SeaweedFS nodes by FQDN, and MetalLB can hand out LoadBalancer IPs from the
// stack subnet — reachable from every other container in the stack (e.g. the Ubuntu VNC desktop).
//
// k3s ships kubectl, so every kubectl call is an exec into the first node; nothing else needs a
// Kubernetes client. The operator source is unpacked into /root on that same node, which is where
// bundle.yaml and the (rewritten) cr.yaml are applied from.

const (
	// The k3s node containers k3d creates: k3d-<cluster>-server-0, -agent-0, …
	k3dContainerPrefix = "k3d-"
	// kubectl inside a k3s node reads the admin kubeconfig from here.
	k3dKubeconfig = "/etc/rancher/k3s/k3s.yaml"
	// Where the operator source is unpacked on the first node.
	k3dOperatorDir = "/root"
	// MetalLB is pinned: its manifest and CRDs must agree with the pool we apply below.
	metalLBVersion  = "v0.14.9"
	metalLBManifest = "https://raw.githubusercontent.com/metallb/metallb/" + metalLBVersion + "/config/manifests/metallb-native.yaml"
	// The operator source tarball (the git tag carries deploy/bundle.yaml + deploy/cr.yaml).
	operatorTarballFmt = "https://github.com/percona/%s/archive/refs/tags/v%s.tar.gz"
)

// k3dOperatorRepos maps a product to its GitHub repository — the tag's source tarball is where
// bundle.yaml, secrets.yaml and cr.yaml come from.
var k3dOperatorRepos = map[string]string{
	"pxc":   "percona-xtradb-cluster-operator",
	"ps":    "percona-server-mysql-operator",
	"psmdb": "percona-server-mongodb-operator",
	"pg":    "percona-postgresql-operator",
}

// k3dDeployableOperator is the subset DBCanvas can actually install — all four, now.
var k3dDeployableOperator = map[string]bool{"pxc": true, "ps": true, "psmdb": true, "pg": true}

// k3dExposeTypes are the Kubernetes Service types a cr.yaml `expose` section accepts.
var k3dExposeTypes = map[string]string{
	"clusterip":    "ClusterIP",
	"nodeport":     "NodePort",
	"loadbalancer": "LoadBalancer",
}

// k3dConfig is the non-secret profile stored on every member node of a K3D frame, so any member's
// properties panel can describe the whole cluster.
type k3dConfig struct {
	Cluster      string `json:"cluster"`      // k3d cluster name (= frame label)
	Role         string `json:"role"`         // "server" | "agent"
	Hostname     string `json:"hostname"`     // DBCanvas hostname (also the DNS name)
	FQDN         string `json:"fqdn"`         //
	Nodes        int    `json:"nodes"`        // 1..3
	K3SVersion   string `json:"k3sVersion"`   // the rancher/k3s tag the cluster runs
	CPUs         int    `json:"cpus"`         // total CPUs for the cluster
	MemoryGB     int    `json:"memoryGb"`     // total memory for the cluster
	MetalLBRange string `json:"metallbRange"` // the LoadBalancer address pool
	Operator     string `json:"operator"`     // "" | "pxc" | "ps" | "psmdb" | "pg"
	OperatorVer  string `json:"operatorVer"`  //
	OperatorSrc  string `json:"operatorSrc"`  // /root/<repo>-<ver> on the first node
	Namespace    string `json:"namespace"`    //
	ClusterName  string `json:"crName"`       // the database cluster's name inside cr.yaml
	// PXC / PS: the front end (they are mutually exclusive) and the Service type of each tier.
	// PXC's proxy is haproxy|proxysql; PS's is haproxy|router.
	Proxy       string `json:"proxy"`       //
	Expose      string `json:"expose"`      // the database Service type — kept for the card
	ExposePXC   string `json:"exposePxc"`   // ClusterIP | NodePort | LoadBalancer
	ExposeProxy string `json:"exposeProxy"` // the chosen proxy's Service type
	// PS: group replication, or async replication under Orchestrator.
	ClusterType string `json:"clusterType"`
	ExposeMySQL string `json:"exposeMysql"`
	// PSMDB: a plain replica set, or sharded (config servers + mongos routers).
	Sharding      bool   `json:"sharding"`
	ExposeReplset string `json:"exposeReplset"`
	ExposeMongos  string `json:"exposeMongos"` // sharded clusters only
	// PG: the primary Postgres Service and the pgBouncer pool in front of it.
	ExposePG        string `json:"exposePg"`
	ExposePGBouncer string `json:"exposePgbouncer"`
	MonitoredBy     string `json:"monitoredBy"` // PMM FQDN ("" = none)
	PMMToken        string `json:"pmmToken"`    // "" | "expires <when>" — the service token's lifetime
	BackupRepo      string `json:"backupRepo"`  // SeaweedFS S3 target ("" = none)
	Image           string `json:"image"`       // the k3s image k3d used
}

// ---------------------------------------------------------------- the k3d binary

// k3dBinary resolves the k3d executable: $K3D_BIN, else "k3d" on PATH. In the app image the binary
// is baked in; in local development it is the one installed next to Docker on the host.
func k3dBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("K3D_BIN")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("K3D_BIN=%s does not exist", p)
	}
	return exec.LookPath("k3d")
}

// k3dInstalled reports whether the k3d binary is available. validateStack calls this so a design
// with a K3D frame fails with a clear message instead of dying mid-deploy.
func k3dInstalled() bool {
	_, err := k3dBinary()
	return err == nil
}

// runK3D executes k3d against the same Docker daemon DBCanvas uses. k3d wants a HOME for its
// config; it never needs one in our flow, so it gets a scratch dir.
func (a *App) runK3D(ctx context.Context, logln func(string), args ...string) (string, error) {
	bin, err := k3dBinary()
	if err != nil {
		return "", fmt.Errorf("k3d is not installed: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(),
		"DOCKER_HOST=unix://"+envOr("DOCKER_SOCK", "/var/run/docker.sock"),
		"HOME=/tmp",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	if logln != nil {
		for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if ln != "" {
				logln("k3d: " + ln)
			}
		}
	}
	if err != nil {
		return out.String(), fmt.Errorf("k3d %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// k3dClusterName is the k3d cluster name for a frame: its label, sanitized, scoped by the stack.
// The scope is not cosmetic — every stack's first K3D frame is labelled "k3d-00" by default, and
// k3d cluster names are global to the Docker daemon, so without it two stacks would fight over one
// cluster (the second deploy dies with "a cluster with that name already exists").
func k3dClusterName(stackID int64, frame designFrame) string {
	return fmt.Sprintf("%s-s%d", sanitizeName(frame.Label), stackID)
}

// k3dNodeContainer is the container k3d creates for the i-th member (0 = the server).
func k3dNodeContainer(cluster string, i int) string {
	if i == 0 {
		return fmt.Sprintf("%s%s-server-0", k3dContainerPrefix, cluster)
	}
	return fmt.Sprintf("%s%s-agent-%d", k3dContainerPrefix, cluster, i-1)
}

// ---------------------------------------------------------------- validation

// k3dFrameIssues validates a K3D frame: node count, namespace, operator selection, and the
// CPU/memory budget. The budget produces *warnings*, never errors — it is a judgement call about
// the host, and the operator explicitly asked for one.
func (a *App) k3dFrameIssues(ctx context.Context, f designFrame, members int, opCat OperatorCatalog) []issue {
	var out []issue
	name := f.Label
	if members < 1 || members > 3 {
		out = append(out, issue{"error", "K3D cluster " + name + " must have 1–3 nodes"})
	}
	if !k3dInstalled() {
		out = append(out, issue{"error", "K3D cluster " + name + " needs the k3d binary — install k3d on the host (it talks to the same Docker daemon), or rebuild the app image"})
	}
	if _, ok := loadK3SCatalog().resolveK3SVersion(f.K3DK3SVersion); !ok {
		out = append(out, issue{"error", "K3D cluster " + name + " requests an unknown Kubernetes version " + f.K3DK3SVersion + " — pick one from the list, or run `make versions`"})
	}
	if ns := strings.TrimSpace(f.K3DNamespace); ns != "" && !validNamespace(ns) {
		out = append(out, issue{"error", "K3D cluster " + name + " has an invalid namespace " + ns + " — use lowercase letters, digits and '-' (a DNS-1123 label)"})
	}
	if op := strings.TrimSpace(f.K3DOperator); op != "" {
		if !k3dDeployableOperator[op] {
			out = append(out, issue{"error", "K3D cluster " + name + ": unknown operator " + op})
		} else if _, ok := opCat.resolveOperatorVersion(op, f.K3DOperatorVer); !ok {
			out = append(out, issue{"error", "K3D cluster " + name + " requests an unknown " + op + " operator version — pick one from the list, or run `make versions`"})
		}
	}

	// The CPU/memory budget is for the whole cluster, split across its nodes.
	cpus, memGB := k3dCPUs(f), k3dMemoryGB(f)
	if cpus < 4 || memGB < 6 {
		out = append(out, issue{"warning", fmt.Sprintf("K3D cluster %s is allotted %d CPU / %d GiB — a database cluster (3 pods plus a proxy or router) is unlikely to schedule below 4 CPU / 6 GiB", name, cpus, memGB)})
	}
	// An async PS cluster adds 3 Orchestrator pods on top of the database and the proxy.
	if f.K3DOperator == "ps" && psClusterType(f.K3DClusterType) == "async" && (cpus < 8 || memGB < 12) {
		out = append(out, issue{"warning", fmt.Sprintf("K3D cluster %s runs async replication — 9 pods (3 MySQL + 3 Orchestrator + 3 HAProxy) against %d CPU / %d GiB; below 8 CPU / 12 GiB, use group replication instead", name, cpus, memGB)})
	}
	// A sharded MongoDB cluster is 3 replica-set + 3 config-server + 3 mongos pods.
	if f.K3DOperator == "psmdb" && f.K3DSharding && (cpus < 8 || memGB < 12) {
		out = append(out, issue{"warning", fmt.Sprintf("K3D cluster %s is sharded — 9 MongoDB pods (replica set + config servers + mongos) against %d CPU / %d GiB; below 8 CPU / 12 GiB, deploy it as a replica set instead", name, cpus, memGB)})
	}
	if ncpu, memBytes := a.docker.HostResources(ctx); ncpu > 0 && memBytes > 0 {
		hostGB := int(memBytes / (1 << 30))
		if cpus*5 > ncpu*4 || memGB*5 > hostGB*4 { // > 80% of the host
			out = append(out, issue{"warning", fmt.Sprintf("K3D cluster %s is allotted %d CPU / %d GiB of this host's %d CPU / %d GiB — leaving under 20%% for the host and the rest of the stack", name, cpus, memGB, ncpu, hostGB)})
		}
	}
	return out
}

// validNamespace enforces a DNS-1123 label (what Kubernetes accepts as a namespace).
func validNamespace(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return true
}

// Defaults for the frame's knobs, so an older design (or a hand-written one) still deploys.
func k3dCPUs(f designFrame) int {
	if f.K3DCPUs > 0 {
		return f.K3DCPUs
	}
	return 4
}
func k3dMemoryGB(f designFrame) int {
	if f.K3DMemoryGB > 0 {
		return f.K3DMemoryGB
	}
	return 8
}
func k3dNamespace(f designFrame) string {
	if ns := strings.TrimSpace(f.K3DNamespace); ns != "" {
		return ns
	}
	return "default"
}

// k3dExposeOf normalizes one section's Service type, falling back to the frame's legacy
// single-value setting (designs saved before the per-section split) and then to ClusterIP.
func k3dExposeOf(want, fallback string) string {
	if t, ok := k3dExposeTypes[strings.ToLower(strings.TrimSpace(want))]; ok {
		return t
	}
	if t, ok := k3dExposeTypes[strings.ToLower(strings.TrimSpace(fallback))]; ok {
		return t
	}
	return "ClusterIP"
}

// k3dProxy is the front end in front of the database: HAProxy (cr.yaml's own default) or ProxySQL.
// They are mutually exclusive.
func k3dProxy(f designFrame) string {
	if strings.ToLower(strings.TrimSpace(f.K3DProxy)) == "proxysql" {
		return "proxysql"
	}
	return "haproxy"
}

// ---------------------------------------------------------------- provisioning

// provisionK3DFrame brings up a k3d cluster and (optionally) a Percona operator on it.
func (a *App) provisionK3DFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "k3d" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		log.Printf("stack %d k3d %s: no members", st.ID, frame.Label)
		return
	}

	cluster := k3dClusterName(st.ID, frame)
	nodes := len(members)
	cpus, memGB := k3dCPUs(frame), k3dMemoryGB(frame)
	ns := k3dNamespace(frame)
	proxy := k3dProxy(frame)
	exposePXC := k3dExposeOf(frame.K3DExposePXC, frame.K3DExpose)
	exposeProxy := k3dExposeOf(frame.K3DExposeHAProxy, frame.K3DExpose)
	if proxy == "proxysql" {
		exposeProxy = k3dExposeOf(frame.K3DExposeProxySQL, frame.K3DExpose)
	}

	// The Kubernetes the cluster runs. An unknown tag was already flagged by validation; fall back
	// to the catalog's latest rather than letting k3d pick its own (stale) default.
	k3sCat := loadK3SCatalog()
	k3sTag, ok := k3sCat.resolveK3SVersion(frame.K3DK3SVersion)
	if !ok {
		k3sTag = k3sCat.Latest
	}
	k3sImage := k3sCat.k3sImageRef(k3sTag)

	opCat := loadOperatorCatalog()
	operator := strings.TrimSpace(frame.K3DOperator)
	operatorVer := ""
	if operator != "" {
		if v, ok := opCat.resolveOperatorVersion(operator, frame.K3DOperatorVer); ok {
			operatorVer = v
		} else {
			operator = "" // validation already flagged this; do not guess a version
		}
	}

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == frame.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}
	backupRepo := ""
	if frame.SeaweedFSNodeID != "" {
		backupRepo = "SeaweedFS S3" // refined to name the bucket once the node is up (k3dBackupSecret)
	}

	// One Deployment row per member, up front: without these the canvas shows no cards.
	base := k3dConfig{
		Cluster: cluster, Nodes: nodes, CPUs: cpus, MemoryGB: memGB, K3SVersion: k3sTag,
		Operator: operator, OperatorVer: operatorVer, Namespace: ns,
		Proxy: proxy, Expose: exposePXC, ExposePXC: exposePXC, ExposeProxy: exposeProxy,
		Sharding:    frame.K3DSharding,
		MonitoredBy: monitoredBy, BackupRepo: backupRepo, ClusterName: k3dCRName(frame),
	}
	if operator == "psmdb" {
		base.ExposeReplset = k3dExposeOf(frame.K3DExposeReplset, frame.K3DExpose)
		base.Expose = base.ExposeReplset // the card shows the database tier
		if frame.K3DSharding {
			base.ExposeMongos = k3dExposeOf(frame.K3DExposeMongos, frame.K3DExpose)
		}
	}
	if operator == "ps" {
		base.ClusterType = psClusterType(frame.K3DClusterType)
		base.Proxy = psProxy(frame.K3DProxy, base.ClusterType)
		base.ExposeMySQL = k3dExposeOf(frame.K3DExposeMySQL, frame.K3DExpose)
		base.Expose = base.ExposeMySQL
		if base.Proxy == "router" {
			base.ExposeProxy = k3dExposeOf(frame.K3DExposeRouter, frame.K3DExpose)
		}
	}
	if operator == "pg" {
		base.ExposePG = k3dExposeOf(frame.K3DExposePG, frame.K3DExpose)
		base.ExposePGBouncer = k3dExposeOf(frame.K3DExposePGBouncer, frame.K3DExpose)
		base.Expose = base.ExposePG
	}
	if repo, ok := k3dOperatorRepos[operator]; ok && operator != "" {
		base.OperatorSrc = fmt.Sprintf("%s/%s-%s", k3dOperatorDir, repo, operatorVer)
	}
	for i, n := range members {
		cfg := base
		cfg.Role = "agent"
		if i == 0 {
			cfg.Role = "server"
		}
		cfg.Hostname = hosts[n.ID]
		cfg.FQDN = fqdnOf(hosts[n.ID], domain)
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})
	}

	ctx, endScope := a.deployScope(st.ID)
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		pr := progs[members[0].ID] // the server node carries the cluster-wide progress
		failAll := func(format string, args ...any) {
			for _, n := range members {
				progs[n.ID].fail(format, args...)
			}
		}

		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failAll("%v", werr)
			return
		}

		// ---- create the cluster on the stack network ----
		pr.phase("Creating k3d cluster", 15)
		// NOTE: k3d's --servers-memory/--agents-memory are deliberately NOT used. They work by
		// writing a fake /proc/meminfo under $HOME and bind-mounting that *host* path into the k3s
		// container — which breaks the moment k3d runs inside the app container (`make compose`):
		// the file exists in the app container, the daemon looks for it on the host, does not find
		// it, and the mount fails ("not a directory"). CPU and memory are both imposed with
		// ContainerUpdate below instead: a real cgroup limit, identical whether DBCanvas runs on
		// the host or in Docker.
		args := []string{
			"cluster", "create", cluster,
			// The Kubernetes version is always ours, never k3d's default — that one trails the
			// releases (5.8.3 still ships v1.31.5), and an API server too old for an operator's CRDs
			// makes the operator uninstallable: the ps-operator's clusterset CRD has a CEL rule that
			// needs the `format` library, and a 1.31 API server rejects the CRD outright.
			"--image", k3sImage,
			"--network", networkName(st.ID),
			"--servers", "1",
			"--agents", strconv.Itoa(nodes - 1),
			// servicelb (klipper) and MetalLB both hand out external IPs and fight over them;
			// MetalLB is the one we want. Traefik is not needed and just eats resources.
			"--k3s-arg", "--disable=servicelb@server:*",
			"--k3s-arg", "--disable=traefik@server:*",
			// Let the *daemon* pick the host port the API server is published on. Left to itself k3d
			// probes for a free port in its own network namespace — which, when DBCanvas runs in a
			// container, is not the host's: the port can be free in here and taken out there (another
			// cluster's serverlb, say), and the create then dies with "Bind for 127.0.0.1:xxxxx
			// failed: port is already allocated". Port 0 defers the choice to Docker, which knows.
			// Nothing uses this port anyway — kubectl runs inside the server node.
			"--api-port", "0.0.0.0:0",
			"--wait", "--timeout", "10m",
		}
		// A previous run (or a failed one) may have left the cluster behind, and k3d refuses to
		// create over it. Removing it first makes a redeploy idempotent — the same thing every
		// other provisioner does with "remove the container of this name before creating it".
		a.runK3D(ctx, nil, "cluster", "delete", cluster)
		if _, err := a.runK3D(ctx, pr.logln, args...); err != nil {
			failAll("create k3d cluster: %v", err)
			return
		}

		// ---- adopt the containers k3d created, one per member card ----
		pr.phase("Registering nodes", 30)
		// The CPU/memory budget is for the whole cluster, so each node gets an equal share.
		nanoCPUs := int64(float64(cpus) / float64(nodes) * 1e9)
		memPerNode := int64(max(1, memGB/nodes)) << 30
		serverID := ""
		for i, n := range members {
			cname := k3dNodeContainer(cluster, i)
			cid, ok, _ := a.docker.ContainerByName(ctx, cname)
			if !ok {
				failAll("k3d did not create the expected container %s", cname)
				return
			}
			if i == 0 {
				serverID = cid
			}
			// Impose this node's share of the cluster's CPU/memory budget as a cgroup limit.
			if err := a.docker.ContainerUpdate(ctx, cid, nanoCPUs, memPerNode); err != nil {
				progs[n.ID].logln("could not apply the CPU/memory limit: " + err.Error())
			}
			cfg := base
			cfg.Role = "agent"
			if i == 0 {
				cfg.Role = "server"
			}
			cfg.Hostname = hosts[n.ID]
			cfg.FQDN = fqdnOf(hosts[n.ID], domain)
			cfg.Image = k3dNodeImage(ctx, a, cid)
			cfgJSON, _ := json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: cid, State: DeployProvisioning, Config: cfgJSON})
			progs[n.ID].phase("Node up", 40)
		}
		// The k3s nodes are on the stack network, so they get DNS names like every other node.
		a.reconcileStackDNS(ctx, st.ID)

		// ---- pods must resolve the stack's own names (pmm-01.example.net, seaweedfs-01…) ----
		pr.phase("Wiring cluster DNS to the Intranet", 45)
		if err := a.kubectlApply(ctx, serverID, "", corednsCustomConfigMap(domain, intranetIP)); err != nil {
			pr.logln("CoreDNS forward to the Intranet skipped: " + err.Error())
		} else {
			pr.logln("CoreDNS forwards *." + domain + " to the Intranet DNS (" + intranetIP + ")")
		}

		// ---- MetalLB, so LoadBalancer services get an address on the stack network ----
		pr.phase("Installing MetalLB", 55)
		pool, perr := a.metalLBPool(ctx, st.ID)
		if perr != nil {
			pr.logln("MetalLB address pool skipped: " + perr.Error())
		} else if err := a.installMetalLB(ctx, serverID, pool, pr.logln); err != nil {
			pr.logln("MetalLB install failed (LoadBalancer services will stay pending): " + err.Error())
		} else {
			base.MetalLBRange = pool
			pr.logln("MetalLB pool " + pool + " (from the stack subnet)")
		}

		// ---- the operator ----
		switch operator {
		case "pxc":
			if err := a.installPXCOperator(ctx, st, frame, doc, serverID, &base, pr); err != nil {
				failAll("install the PXC operator: %v", err)
				return
			}
		case "psmdb":
			if err := a.installPSMDBOperator(ctx, st, frame, doc, serverID, &base, pr); err != nil {
				failAll("install the MongoDB operator: %v", err)
				return
			}
		case "ps":
			if err := a.installPSOperator(ctx, st, frame, doc, serverID, &base, pr); err != nil {
				failAll("install the MySQL (Percona Server) operator: %v", err)
				return
			}
		case "pg":
			if err := a.installPGOperator(ctx, st, frame, doc, serverID, &base, pr); err != nil {
				failAll("install the PostgreSQL operator: %v", err)
				return
			}
		}

		// ---- done: record the final config on every member ----
		for i, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			cfg := base
			cfg.Role = "agent"
			if i == 0 {
				cfg.Role = "server"
			}
			cfg.Hostname = hosts[n.ID]
			cfg.FQDN = fqdnOf(hosts[n.ID], domain)
			cfg.Image = k3dNodeImage(ctx, a, dep.ContainerID)
			cfgJSON, _ := json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: cfgJSON})
			progs[n.ID].phase("Running", 100)
			progs[n.ID].p.Message = "provisioned"
			progs[n.ID].save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d k3d %s: provisioned (%d node(s), operator %q)", st.ID, frame.Label, nodes, operator)
	}()
}

// destroyK3DClusters deletes every k3d cluster a stack owns. Called from teardownStack *before* it
// sweeps `dbcanvas-<id>-*` containers: k3d's containers (and volumes, and its serverlb) carry
// k3d's own names, so nothing else in the teardown would ever touch them.
func (a *App) destroyK3DClusters(ctx context.Context, stackID int64) {
	st, err := a.store.GetStack(stackID)
	if err != nil {
		return
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return
	}
	for _, f := range doc.Frames {
		if f.Type != "k3d" {
			continue
		}
		cluster := k3dClusterName(stackID, f)
		if _, err := a.runK3D(ctx, nil, "cluster", "delete", cluster); err != nil {
			log.Printf("stack %d: k3d cluster delete %s: %v", stackID, cluster, err)
			continue
		}
		log.Printf("stack %d: k3d cluster %s deleted", stackID, cluster)
	}
}

// k3dNodeImage reports the k3s image a node container runs (for the properties panel).
func k3dNodeImage(ctx context.Context, a *App, containerID string) string {
	if containerID == "" {
		return ""
	}
	out, err := a.docker.Exec(ctx, containerID, []string{"sh", "-c", "echo $K3S_IMAGE"}, nil)
	if err == nil && strings.TrimSpace(out.Stdout) != "" {
		return strings.TrimSpace(out.Stdout)
	}
	return "rancher/k3s"
}

// ---------------------------------------------------------------- kubectl

// kubectl runs kubectl inside a k3s node (the k3s image ships it) against the cluster's own admin
// kubeconfig. Nothing outside the cluster needs a Kubernetes client.
func (a *App) kubectl(ctx context.Context, serverID string, args ...string) (string, error) {
	res, err := a.docker.Exec(ctx, serverID, append([]string{"kubectl"}, args...), []string{"KUBECONFIG=" + k3dKubeconfig})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return res.Stdout + res.Stderr, fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return res.Stdout, nil
}

// kubectlApply pipes a manifest to `kubectl apply -f -` (no temp files on the node). ns targets a
// namespace; pass "" for manifests that carry their own (MetalLB, the CoreDNS ConfigMap). The
// custom resource MUST be applied into the operator's namespace — applied without one it lands in
// `default`, where nothing watches it and the cluster is silently never created.
func (a *App) kubectlApply(ctx context.Context, serverID, ns string, manifest []byte) error {
	args := []string{"kubectl", "apply", "-f", "-"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	res, err := a.docker.ExecInput(ctx, serverID, "", args,
		[]string{"KUBECONFIG=" + k3dKubeconfig}, manifest)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("kubectl apply: %s", strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return nil
}

// ---------------------------------------------------------------- CoreDNS + MetalLB

// corednsCustomConfigMap forwards the stack's domain to the Intranet DNS. k3s's CoreDNS imports
// /etc/coredns/custom/*.server, so a ConfigMap is all it takes — the shipped Corefile is untouched.
func corednsCustomConfigMap(domain, intranetIP string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns-custom
  namespace: kube-system
data:
  %s.server: |
    %s:53 {
      errors
      cache 30
      forward . %s
    }
`, domain, domain, intranetIP))
}

// metalLBPool carves a LoadBalancer address range out of the stack's Docker subnet. Docker's IPAM
// hands out addresses from the bottom, so the pool is taken from the very top — the last usable
// addresses below the broadcast.
func (a *App) metalLBPool(ctx context.Context, stackID int64) (string, error) {
	cidr, err := a.docker.NetworkSubnet(ctx, networkName(stackID))
	if err != nil || cidr == "" {
		return "", fmt.Errorf("could not read the stack subnet: %v", err)
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return "", fmt.Errorf("stack subnet %s is not IPv4", cidr)
	}
	// Broadcast = network | ^mask.
	bcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bcast[i] = ip4[i] | ^ipnet.Mask[i]
	}
	last := ipToU32(bcast) - 2   // leave the broadcast and one address free
	first := last - 49           // a 50-address pool
	if first <= ipToU32(ip4)+1 { // a tiny subnet: fall back to whatever is above the network address
		first = ipToU32(ip4) + 2
	}
	return fmt.Sprintf("%s-%s", u32ToIP(first), u32ToIP(last)), nil
}

func ipToU32(ip net.IP) uint32 {
	b := ip.To4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func u32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// installMetalLB applies the pinned MetalLB manifest, waits for its controller, then advertises an
// address pool from the stack subnet (L2 mode). servicelb was disabled at cluster creation, so
// MetalLB owns LoadBalancer services outright.
func (a *App) installMetalLB(ctx context.Context, serverID, pool string, logln func(string)) error {
	manifest, err := httpGetBytes(ctx, metalLBManifest)
	if err != nil {
		return fmt.Errorf("fetch the MetalLB manifest: %w", err)
	}
	if err := a.kubectlApply(ctx, serverID, "", manifest); err != nil {
		return err
	}
	if _, err := a.kubectl(ctx, serverID, "-n", "metallb-system", "wait", "--for=condition=Available",
		"deployment/controller", "--timeout=180s"); err != nil {
		return fmt.Errorf("MetalLB controller did not become ready: %w", err)
	}
	// The webhook needs a moment after Available before it accepts the CRs.
	poolYAML := []byte(fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: dbcanvas
  namespace: metallb-system
spec:
  addresses:
    - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: dbcanvas
  namespace: metallb-system
spec:
  ipAddressPools:
    - dbcanvas
`, pool))
	var lastErr error
	for i := 0; i < 10; i++ {
		if lastErr = a.kubectlApply(ctx, serverID, "", poolYAML); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return lastErr
}

// httpGetBytes fetches a URL (the MetalLB manifest, the operator source). The app does the
// fetching because the k3s image has neither curl nor git.
func httpGetBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// ---------------------------------------------------------------- the PXC operator

// k3dCRName is the database cluster's name inside Kubernetes — and the stem of every resource the
// operator derives from it (<name>-pxc / <name>-rs0, <name>-secrets, …). It is the frame's label, NOT the k3d
// cluster name: that one is suffixed with the stack id because k3d names are global to the Docker
// daemon, but a custom resource lives inside its own Kubernetes cluster and never collides, so the
// suffix would only show up in every resource name for no reason.
func k3dCRName(frame designFrame) string {
	name := "cluster1"
	if l := sanitizeName(frame.Label); l != "" {
		name = l
	}
	if len(name) > 22 { // the operator appends suffixes to build resource names
		name = name[:22]
	}
	return strings.Trim(name, "-")
}

// k3dFetchOperator downloads an operator's source tarball for the selected version and unpacks it
// into /root on the first node — the k3s image has neither git nor curl, so the app does the
// fetching. Returns the tarball, which is also where cr.yaml and secrets.yaml are read from
// (readContainerFile needs bash; k3s is busybox).
func (a *App) k3dFetchOperator(ctx context.Context, serverID, repo string, cfg *k3dConfig, pr *pxcProg) ([]byte, error) {
	pr.phase("Fetching the operator source", 65)
	url := fmt.Sprintf(operatorTarballFmt, repo, cfg.OperatorVer)
	tgz, err := httpGetBytes(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	tarball, err := gunzip(tgz)
	if err != nil {
		return nil, fmt.Errorf("unpack the operator source: %w", err)
	}
	if _, err := a.docker.Exec(ctx, serverID, []string{"mkdir", "-p", k3dOperatorDir}, nil); err != nil {
		return nil, err
	}
	if err := a.docker.PutArchive(ctx, serverID, k3dOperatorDir, tarball); err != nil {
		return nil, fmt.Errorf("copy the operator source to %s: %w", k3dOperatorDir, err)
	}
	pr.logln("operator source in " + cfg.OperatorSrc + " (on the first node)")
	return tarball, nil
}

// k3dApplyBundle creates the namespace and applies deploy/bundle.yaml (CRDs, RBAC and the operator
// itself), then waits for the operator Deployment. Nothing may be applied before it: the custom
// resource's CRD arrives with the bundle.
func (a *App) k3dApplyBundle(ctx context.Context, serverID, deployment string, cfg *k3dConfig, pr *pxcProg) error {
	pr.phase("Installing the operator", 75)
	ns := cfg.Namespace
	if _, err := a.kubectl(ctx, serverID, "create", "namespace", ns); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	if _, err := a.kubectl(ctx, serverID, "apply", "--server-side", "-n", ns, "-f", cfg.OperatorSrc+"/deploy/bundle.yaml"); err != nil {
		return err
	}
	if _, err := a.kubectl(ctx, serverID, "-n", ns, "wait", "--for=condition=Available",
		"deployment/"+deployment, "--timeout=300s"); err != nil {
		return fmt.Errorf("the operator did not become ready: %w", err)
	}
	return nil
}

// k3dPMMToken mints a PMM service token and patches it into the cluster's secret under tokenKey
// (PXC: `pmmservertoken`; PSMDB: `PMM_SERVER_TOKEN`). It returns the value for the CR's
// `pmm.serverHost` — "" when PMM is not usable, which leaves PMM disabled in the CR rather than
// starting sidecars that can only fail.
//
// This must run BEFORE cr.yaml: the operator reads the secret while creating the pods.
//
// The returned host carries the **port**. Both operators hand serverHost to the sidecars verbatim as
// PMM_AGENT_SERVER_ADDRESS, whose default port is 443 — but a DBCanvas PMM node serves HTTPS on
// 8443, so a bare hostname leaves every pmm-client retrying "connection refused".
func (a *App) k3dPMMToken(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID, secret, tokenKey string, cfg *k3dConfig, pr *pxcProg) string {
	if cfg.MonitoredBy == "" {
		return ""
	}
	_, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, frame.PMMNodeID)
	pmmID := a.containerOf(st.ID, frame.PMMNodeID)
	if !ok || pmmID == "" {
		pr.logln("PMM monitoring skipped: the PMM node is not running")
		return ""
	}
	ttl := certTTL(frame.K3DPMMTokenTTLValue, frame.K3DPMMTokenTTLUnit)
	token, err := a.pmmServiceToken(ctx, pmmID, pmmUser, pmmPass, "dbcanvas-"+cfg.ClusterName, ttl)
	if err != nil {
		pr.logln("PMM monitoring skipped: could not create a service token: " + err.Error())
		return ""
	}
	patch := fmt.Sprintf(`{"stringData":{%q:%q}}`, tokenKey, token)
	if _, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "patch", "secret", secret, "--type=merge", "-p", patch); err != nil {
		pr.logln("PMM monitoring skipped: could not patch the service token into " + secret + ": " + err.Error())
		return ""
	}
	cfg.PMMToken = "expires " + time.Now().Add(ttl).UTC().Format(time.RFC3339)
	host := cfg.MonitoredBy + ":8443"
	pr.logln("PMM serverHost " + host + "; service token patched into " + secret + " as " + tokenKey +
		" (" + cfg.PMMToken + ")")
	return host
}

// k3dBackupSecret creates the S3 credentials secret the operators expect for a backup storage
// (both read AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY out of it), pointed at the stack's SeaweedFS
// node. Returns nil when there is no SeaweedFS node, or it is not usable — backups are then simply
// left as cr.yaml ships them.
func (a *App) k3dBackupSecret(ctx context.Context, st Stack, frame designFrame, serverID string, cfg *k3dConfig, pr *pxcProg) *crS3 {
	if frame.SeaweedFSNodeID == "" {
		return nil
	}
	sw, sec, err := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
	if err != nil {
		pr.logln("backups skipped: " + err.Error())
		return nil
	}
	name := cfg.ClusterName + "-backup-s3"
	if _, err := a.kubectl(ctx, serverID, "-n", cfg.Namespace, "create", "secret", "generic", name,
		"--from-literal=AWS_ACCESS_KEY_ID="+seaweedAccessKeyOf(sw, sec),
		"--from-literal=AWS_SECRET_ACCESS_KEY="+sec.SecretKey); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		pr.logln("backup secret skipped: " + err.Error())
		return nil
	}
	pr.logln("backups → " + sw.InternalEndpoint + " (bucket " + sw.Bucket + ")")
	cfg.BackupRepo = "SeaweedFS S3 (" + sw.Bucket + ")"
	return &crS3{
		Bucket:      sw.Bucket,
		Region:      sw.Region,
		EndpointURL: sw.InternalEndpoint,
		Secret:      name,
	}
}

// installPXCOperator unpacks the operator source into /root on the first node, applies the bundle
// into the chosen namespace, rewrites cr.yaml (§ crTransform) and applies it.
func (a *App) installPXCOperator(ctx context.Context, st Stack, frame designFrame, doc designDoc, serverID string, cfg *k3dConfig, pr *pxcProg) error {
	tarball, err := a.k3dFetchOperator(ctx, serverID, k3dOperatorRepos["pxc"], cfg, pr)
	if err != nil {
		return err
	}
	if err := a.k3dApplyBundle(ctx, serverID, "percona-xtradb-cluster-operator", cfg, pr); err != nil {
		return err
	}
	ns := cfg.Namespace

	// ---- secrets.yaml, BEFORE cr.yaml ----
	// The cluster's users (root, monitor, replication, …) come from this secret. The operator reads
	// it while creating the cluster, so it has to exist first — applied afterwards it changes
	// nothing, and the operator will have generated its own random passwords instead.
	pr.phase("Applying secrets.yaml", 82)
	rawSecrets, err := tarFile(tarball, "deploy/secrets.yaml")
	if err != nil {
		return fmt.Errorf("read secrets.yaml from the operator source: %w", err)
	}
	newSecrets := secretsTransform(string(rawSecrets), cfg.ClusterName, k3dSecretsPasswords())
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "secrets.yaml", 0o600, []byte(newSecrets)); err != nil {
		pr.logln("could not write secrets.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newSecrets)); err != nil {
		return fmt.Errorf("apply secrets.yaml: %w", err)
	}
	pr.logln("secrets.yaml applied as " + cfg.ClusterName + "-secrets (passwords from .env)")

	// ---- cr.yaml: rewrite before applying ----
	pr.phase("Applying cr.yaml", 88)
	// Read it out of the tarball, not off the node: the k3s image is busybox — it has no bash,
	// which readContainerFile needs.
	raw, err := tarFile(tarball, "deploy/cr.yaml")
	if err != nil {
		return fmt.Errorf("read cr.yaml from the operator source: %w", err)
	}
	opts := crOptions{
		Name:  cfg.ClusterName,
		Proxy: cfg.Proxy,
		// Each section gets its own Service type; only the chosen proxy's section is enabled, so
		// the other one's expose value is irrelevant (and left as cr.yaml ships it).
		ExposePXC: cfg.ExposePXC,
	}
	if cfg.Proxy == "proxysql" {
		opts.ExposeProxySQL = cfg.ExposeProxy
	} else {
		opts.ExposeHAProxy = cfg.ExposeProxy
	}

	// Backups → the SeaweedFS node's S3 endpoint, with its credentials in a secret.
	if opts.S3 = a.k3dBackupSecret(ctx, st, frame, serverID, cfg, pr); opts.S3 != nil {
		// `forcePathStyle` (SeaweedFS does not do virtual-host bucket addressing) only exists in the
		// PXC operator's S3 schema from 1.20.0 — an older CRD rejects the WHOLE custom resource over
		// the unknown field, so the cluster is never created. The selected version's own cr.yaml is
		// the authority on what its schema accepts. Backups still work without it: xbcloud already
		// addresses path-style when it is given a custom endpoint (verified against 1.19.1).
		opts.S3.ForcePathStyle = strings.Contains(string(raw), "forcePathStyle")
		if !opts.S3.ForcePathStyle {
			pr.logln("operator " + cfg.OperatorVer + " has no forcePathStyle option (added in 1.20.0) — omitted; xbcloud addresses path-style against a custom endpoint anyway")
		}
	}
	// PMM 3's pmm-client sidecars authenticate with a service token, not a password.
	opts.PMMHost = a.k3dPMMToken(ctx, st, frame, doc, serverID, cfg.ClusterName+"-secrets", "pmmservertoken", cfg, pr)

	newCR := crTransform(string(raw), opts)
	// Keep /root in sync with what was actually applied — the source is there to be read.
	if err := a.docker.CopyFile(ctx, serverID, cfg.OperatorSrc+"/deploy", "cr.yaml", 0o644, []byte(newCR)); err != nil {
		pr.logln("could not write the rewritten cr.yaml back to the source tree: " + err.Error())
	}
	if err := a.kubectlApply(ctx, serverID, ns, []byte(newCR)); err != nil {
		return err
	}
	pr.logln(fmt.Sprintf("cr.yaml applied (affinity none, resources commented out, %s front end, pxc %s / %s %s)",
		cfg.Proxy, cfg.ExposePXC, cfg.Proxy, cfg.ExposeProxy))
	return nil
}

// pmmServiceTokenScript mints a PMM service token, printing just the token.
//
// PMM 3's pmm-client authenticates with a *service token*, not a password — the operator reads it
// from the cluster secret's `pmmservertoken` key. PMM is Grafana underneath, so the token comes
// from Grafana's API: the service-accounts endpoint (Grafana 11+), falling back to the older
// api-keys endpoint that Percona's own docs still use. Both honour secondsToLive, which is what
// gives the token its expiry. Run inside the PMM container, against its own loopback.
const pmmServiceTokenScript = `set -e
BASE="https://127.0.0.1:8443"
AUTH="-u $PMM_USER:$PMM_PASS"
key_of() { sed -n 's/.*"key":"\([^"]*\)".*/\1/p'; }

# Service accounts (Grafana 11+). An account of this name may already exist (a redeploy), in which
# case it is looked up rather than recreated — only the token is new.
SA=$(curl -sk $AUTH -X POST -H 'Content-Type: application/json' \
  -d "{\"name\":\"$NAME\",\"role\":\"Admin\",\"isDisabled\":false}" "$BASE/graph/api/serviceaccounts" 2>/dev/null || true)
ID=$(printf '%s' "$SA" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' | head -1)
if [ -z "$ID" ]; then
  ID=$(curl -sk $AUTH "$BASE/graph/api/serviceaccounts/search?query=$NAME" 2>/dev/null \
    | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p' | head -1)
fi
if [ -n "$ID" ]; then
  KEY=$(curl -sk $AUTH -X POST -H 'Content-Type: application/json' \
    -d "{\"name\":\"$NAME-$STAMP\",\"secondsToLive\":$TTL}" "$BASE/graph/api/serviceaccounts/$ID/tokens" 2>/dev/null | key_of)
  if [ -n "$KEY" ]; then printf '%s' "$KEY"; exit 0; fi
fi

# Older api-keys endpoint (what the Percona docs show).
KEY=$(curl -sk $AUTH -X POST -H 'Content-Type: application/json' \
  -d "{\"name\":\"$NAME-$STAMP\",\"role\":\"Admin\",\"secondsToLive\":$TTL}" "$BASE/graph/api/auth/keys" 2>/dev/null | key_of)
[ -n "$KEY" ] || { echo "could not create a PMM service token" >&2; exit 1; }
printf '%s' "$KEY"`

// pmmServiceToken mints a service token on the PMM node and returns it.
func (a *App) pmmServiceToken(ctx context.Context, pmmContainerID, user, pass, name string, ttl time.Duration) (string, error) {
	out, err := a.execScript(ctx, pmmContainerID, pmmServiceTokenScript, []string{
		"PMM_USER=" + user,
		"PMM_PASS=" + pass,
		"NAME=" + name,
		"STAMP=" + strconv.FormatInt(time.Now().Unix(), 10),
		"TTL=" + strconv.Itoa(int(ttl.Seconds())),
	})
	token := strings.TrimSpace(out)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("PMM returned an empty service token")
	}
	return token, nil
}

// seaweedAccessKeyOf mirrors the other backup consumers: the config's access key, else the secret's.
func seaweedAccessKeyOf(sw seaweedConfig, sec seaweedSecrets) string {
	if sw.AccessKey != "" {
		return sw.AccessKey
	}
	return sec.AccessKey
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 256<<20))
}

// tarFile reads the file with the given suffix out of a tarball — e.g. "deploy/cr.yaml", which a
// GitHub source tarball nests under "<repo>-<version>/". Matching on the suffix means the pax
// header entry GitHub prepends, and the top directory's exact name, are both irrelevant.
//
// cr.yaml is taken from here rather than read back off the node: the k3s image is busybox, and
// readContainerFile needs bash.
func tarFile(tarball []byte, suffix string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in the operator source", suffix)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if name := strings.TrimPrefix(h.Name, "./"); name == suffix || strings.HasSuffix(name, "/"+suffix) {
			return io.ReadAll(io.LimitReader(tr, 8<<20))
		}
	}
}
