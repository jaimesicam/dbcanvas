package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MySQL replication frame. A primary + one or more secondaries running Percona
// Server (percona-server-server) on the systemd OS images. Replication is GTID
// based with auto-positioning; secondaries run super_read_only so a fronting
// ProxySQL can route reads to them. Mode is async ("normal") or semi-sync.
//
// Keyword note: only the modern, 8.0.23+/8.4-safe terms are used —
// CHANGE REPLICATION SOURCE TO … SOURCE_AUTO_POSITION=1, START REPLICA,
// SHOW REPLICA STATUS (never CHANGE MASTER / START SLAVE / SHOW SLAVE STATUS,
// which 8.4 removed). Semi-sync plugin names differ by series (8.0
// master/slave, 8.4 source/replica) and are selected per major.

var mysqlPorts = []int{3306}

// mysqlConfig is the non-secret profile shown for a deployed MySQL replication node.
type mysqlConfig struct {
	Cluster      string `json:"cluster"`
	Image        string `json:"image"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Role         string `json:"role"` // primary | secondary
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	ServerID     int    `json:"serverId"`
	PSVersion    string `json:"psVersion"`
	ReplMode     string `json:"replMode"` // async | semisync
	GTID         bool   `json:"gtid"`
	ReadOnly     bool   `json:"readOnly"`
	SourceHost   string `json:"sourceHost"` // primary FQDN (secondaries)
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"`
	Ports        []int  `json:"ports"`
	ExportPort   int    `json:"exportPort"`
}

func mysqlUnit(os string) string {
	if isDebianOS(os) {
		return "mysql"
	}
	return "mysqld"
}

// mysqlServerID derives a stable, unique server-id from a mysqlNN node name.
func mysqlServerID(name string) int {
	digits := strings.TrimLeft(strings.TrimPrefix(name, "mysql"), "0")
	if v, err := strconv.Atoi(digits); err == nil && v > 0 {
		return v
	}
	return int(fnv32(name)%100000) + 1
}

func mysqlReplMode(m string) string {
	if m == "semisync" {
		return "semisync"
	}
	return "async"
}

// semisyncEnv returns the per-series plugin SONAME + enable variable for the
// source (primary) and replica (secondary) semi-sync roles.
//
//	8.0 → rpl_semi_sync_master / _slave   (semisync_master.so / semisync_slave.so)
//	8.4 → rpl_semi_sync_source / _replica (semisync_source.so / semisync_replica.so)
func semisyncSource(major string) (plugin, soname, enableVar string) {
	if major == "8.4" {
		return "rpl_semi_sync_source", "semisync_source.so", "rpl_semi_sync_source_enabled"
	}
	return "rpl_semi_sync_master", "semisync_master.so", "rpl_semi_sync_master_enabled"
}
func semisyncReplica(major string) (plugin, soname, enableVar string) {
	if major == "8.4" {
		return "rpl_semi_sync_replica", "semisync_replica.so", "rpl_semi_sync_replica_enabled"
	}
	return "rpl_semi_sync_slave", "semisync_slave.so", "rpl_semi_sync_slave_enabled"
}

// provisionMySQLFrame brings up a MySQL replication topology: install every node,
// bootstrap the primary (root pw + app/repl/monitor/cluster users), then attach
// each secondary via GTID auto-positioning, and optionally enable semi-sync, PMM
// and per-node TLS.
func (a *App) provisionMySQLFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	major := psMajorOf(frame.PSMajor)

	var primary designNode
	var secondaries []designNode
	havePrimary := false
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "mysql" {
			continue
		}
		if n.Role == "primary" && !havePrimary {
			primary, havePrimary = n, true
		} else {
			secondaries = append(secondaries, n)
		}
	}
	if !havePrimary {
		return
	}
	sort.Slice(secondaries, func(i, j int) bool { return secondaries[i].Label < secondaries[j].Label })
	members := append([]designNode{primary}, secondaries...)

	// Cluster-wide root password (reuse across redeploys, else frame value, else
	// generated). App/repl/monitor/cluster come from the environment.
	root := strings.TrimSpace(frame.RootPassword)
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s pxcSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil && s.RootPassword != "" {
				root = s.RootPassword
				break
			}
		}
	}
	if root == "" {
		root = genSecret("MyRoot!")
	}
	sec := pxcSecrets{
		RootUser: "root", RootPassword: root,
		AppUser: "app", AppPassword: envOr("APP_PASSWORD", "app_password"),
		ReplUser: "repl", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
		MonitorUser: "monitor", MonitorPassword: envOr("MONITOR_PASSWORD", "monitor_password"),
		ClusterUser: "cluster", ClusterPassword: envOr("CLUSTER_PASSWORD", "cluster_password"),
	}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	primaryFQDN := fqdnOf(hosts[primary.ID], domain)
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	for _, n := range members {
		host := hosts[n.ID]
		role := "secondary"
		if n.ID == primary.ID {
			role = "primary"
		}
		cfg := mysqlConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: role, Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mysqlServerID(host),
			PSVersion: frame.PSVersion, ReplMode: mysqlReplMode(frame.ReplMode), GTID: frame.GTID,
			ReadOnly: role == "secondary", GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, Ports: mysqlPorts,
		}
		if role == "secondary" {
			cfg.SourceHost = primaryFQDN
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	go func() {
		ctx := context.Background()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		failAll := func(format string, args ...any) {
			for _, n := range members {
				progs[n.ID].fail(format, args...)
			}
		}
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			failAll("%v", werr)
			return
		}

		// ---- Phase 1 (parallel): container + install + my.cnf per node ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mysqlPrepareNode(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain); err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if failed {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2: bootstrap the primary ----
		pp := progs[primary.ID]
		pp.phase("Configuring primary", 60)
		if err := a.mysqlSetupPrimary(ctx, st, frame, primary, major, sec, pp); err != nil {
			return
		}

		// ---- Phase 3: attach each secondary ----
		for _, n := range secondaries {
			pr := progs[n.ID]
			pr.phase("Attaching replica", 70)
			if err := a.mysqlSetupReplica(ctx, st, frame, n, primaryFQDN, major, sec, pr); err != nil {
				return
			}
		}

		// ---- Phase 4: TLS + PMM + finalize ----
		for _, n := range members {
			pr := progs[n.ID]
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			// Let the unix root user run mysql without typing the password.
			a.docker.CopyFile(ctx, dep.ContainerID, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 90)
				host := hosts[n.ID]
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(host, domain), mysqlUnit(frame.OS), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
					pr.fail("%v", err)
					return
				}
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 95)
				pmmUser, pmmPass := "", ""
				if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, p
				}
				a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label)) // best-effort
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d mysql repl %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
	}()
}

// provisionPerconaServer provisions a standalone Percona Server node — a single
// read/write instance (no replication). It reuses the replication primary path via
// a synthetic frame built from the node's own settings.
func (a *App) provisionPerconaServer(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	frame := designFrame{
		Type: "mysql", Label: n.Label,
		OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch,
		PSMajor: n.PSMajor, PSVersion: n.PSVersion, GTID: n.GTID,
		UseProxy: n.UseProxy, GenerateCert: n.GenerateCert,
		CertTTLValue: n.CertTTLValue, CertTTLUnit: n.CertTTLUnit, PMMNodeID: n.PMMNodeID,
	}
	major := psMajorOf(n.PSMajor)
	image := pxcImage(n.OS, n.OSVersion, n.Arch)

	root := strings.TrimSpace(n.RootPassword)
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s pxcSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil && s.RootPassword != "" {
			root = s.RootPassword
		}
	}
	if root == "" {
		root = genSecret("MyRoot!")
	}
	sec := pxcSecrets{
		RootUser: "root", RootPassword: root,
		AppUser: "app", AppPassword: envOr("APP_PASSWORD", "app_password"),
		ReplUser: "repl", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
		MonitorUser: "monitor", MonitorPassword: envOr("MONITOR_PASSWORD", "monitor_password"),
		ClusterUser: "cluster", ClusterPassword: envOr("CLUSTER_PASSWORD", "cluster_password"),
	}
	monitoredBy := ""
	if n.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == n.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(stackHostnames(doc)[m.ID], domain)
			}
		}
	}
	cfg := mysqlConfig{
		Cluster: "", Image: image, OS: n.OS, Arch: archOr(n.Arch), Role: "standalone",
		Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mysqlServerID(host),
		PSVersion: n.PSVersion, ReplMode: "", GTID: n.GTID, ReadOnly: false,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy, Ports: mysqlPorts,
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		pr.phase("Waiting for Intranet to be ready", 5)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		if err := a.mysqlPrepareNode(ctx, st, frame, n, host, image, intranetIP, domain); err != nil {
			return // recorded its own error
		}
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Configuring Percona Server", 60)
		if err := a.mysqlSetupPrimary(ctx, st, frame, n, major, sec, pr); err != nil {
			return
		}
		// Let the unix root user run mysql without typing the password.
		a.docker.CopyFile(ctx, a.containerOf(st.ID, n.ID), "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
		if n.GenerateCert {
			pr.phase("Issuing certificate", 90)
			if err := a.pxcApplyCert(ctx, a.containerOf(st.ID, n.ID), intranetID, fqdnOf(host, domain), mysqlUnit(n.OS), n.OS, n.CertTTLValue, n.CertTTLUnit, pr.logln); err != nil {
				pr.fail("%v", err)
				return
			}
		}
		if n.PMMNodeID != "" {
			pr.phase("Registering with PMM", 95)
			pmmUser, pmmPass := "", ""
			if _, u, p, ok := a.pmmServerFor(st, doc, n.PMMNodeID); ok {
				pmmUser, pmmPass = u, p
			}
			a.pxcPMMExec(ctx, a.containerOf(st.ID, n.ID), n.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label)) // best-effort
		}
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d percona-server %s: provisioned", st.ID, n.ID)
	}()
}

// containerOf returns a node's current container id (or "").
func (a *App) containerOf(stackID int64, nodeID string) string {
	if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
		return dep.ContainerID
	}
	return ""
}

// waitMySQLRunning blocks until every member of a MySQL replication frame is
// running, then returns the primary's FQDN, all member FQDNs, and the shared
// credentials — used by a fronting ProxySQL to configure its backends manually.
func (a *App) waitMySQLRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) (primaryFQDN string, memberFQDNs []string, sec pxcSecrets, err error) {
	hosts := stackHostnames(doc)
	var members []designNode
	var primary designNode
	havePrimary := false
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "mysql" {
			continue
		}
		members = append(members, n)
		if n.Role == "primary" && !havePrimary {
			primary, havePrimary = n, true
		}
	}
	if !havePrimary {
		return "", nil, pxcSecrets{}, fmt.Errorf("MySQL replication %s has no primary", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var s pxcSecrets
		for _, n := range members {
			dep, e := a.store.GetDeployment(stackID, n.ID)
			if e != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return "", nil, pxcSecrets{}, fmt.Errorf("MySQL replication %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &s)
		}
		if allRunning {
			var fqdns []string
			for _, n := range members {
				fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
			}
			return fqdnOf(hosts[primary.ID], domain), fqdns, s, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", nil, pxcSecrets{}, fmt.Errorf("MySQL replication %s did not become ready within %s", frame.Label, timeout)
}

// psMajorOf normalizes a Percona Server major series (default "8.0").
func psMajorOf(major string) string {
	if major == "8.4" {
		return "8.4"
	}
	return "8.0"
}

// mysqlResetCmd is the statement that clears a server's binary logs + GTID state so
// replicas can connect with AUTO_POSITION cleanly. 8.4 renamed RESET MASTER.
func mysqlResetCmd(major string) string {
	if major == "8.4" {
		return "RESET BINARY LOGS AND GTIDS"
	}
	return "RESET MASTER"
}

// mysqlPrepareNode creates a node container, installs percona-server-server (+
// pmm-client), and writes its my.cnf.
func (a *App) mysqlPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, intranetIP, domain string) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	if host == "" {
		host = sanitizeName(n.Label)
	}
	pr.phase("Creating container", 15)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
		a.docker.ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: 3306, HostPort: n.ExportHostPort}}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	var cfg mysqlConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if n.ExportEnabled {
		if hp, e := a.docker.ContainerPort(ctx, id, "3306/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON := []byte("{}")
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		secJSON = dep.Secrets
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 25)
	if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)

	debian := isDebianOS(frame.OS)
	if frame.UseProxy {
		proxyScript := pkgProxyRHEL
		if debian {
			proxyScript = pkgProxyDebian
		}
		if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return pr.fail("configure package proxy: %v", err)
		}
	}

	pr.phase("Installing Percona Server", 40)
	instScript, pmmScript := mysqlInstallRHEL, pxcInstallPMMClientRHEL
	if debian {
		instScript, pmmScript = mysqlInstallDebian, pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{"PRODUCT=" + psClientProduct(psMajorOf(frame.PSMajor))}, pr.logln); err != nil {
		return pr.fail("install percona-server-server: %v", err)
	}
	pr.logln("percona-server-server installed")

	// Install Percona XtraBackup matching the Percona Server series (8.0 → pxb80 /
	// percona-xtrabackup-80, 8.4 → pxb84lts / percona-xtrabackup-84) so the node can
	// take physical backups (e.g. to a SeaweedFS S3 target via xbcloud).
	pr.phase("Installing Percona XtraBackup", 45)
	xbpkg := pxbPackage(frame.PSMajor)
	xbScript := pxcInstallXtrabackupRHEL
	if debian {
		xbScript = pxcInstallXtrabackupDebian
	}
	if err := a.runStep(ctx, id, xbScript, []string{"PRODUCT=" + pxbProduct(frame.PSMajor), "PKG=" + xbpkg}, pr.logln); err != nil {
		return pr.fail("install %s: %v", xbpkg, err)
	}
	pr.logln(xbpkg + " installed")

	if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
		return pr.fail("install pmm-client: %v", err)
	}
	pr.logln("pmm-client installed")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	cnf := mysqlMyCnf(frame, host)
	dir, base := pxcCnfDir(frame.OS)
	if err := a.docker.CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", pxcCnfPath(frame.OS), err)
	}
	if debian {
		if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
			return pr.fail("include my.cnf: %v", err)
		}
	}
	return nil
}

// mysqlMyCnf renders /etc/my.cnf for a MySQL replication node. read_only is NOT
// set here (it is applied with SET PERSIST on secondaries after replication is
// configured, so the bootstrap writes are not blocked).
func mysqlMyCnf(frame designFrame, host string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n[mysqld]\n")
	fmt.Fprintf(&b, "server-id=%d\n", mysqlServerID(host))
	fmt.Fprintf(&b, "datadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n")
	fmt.Fprintf(&b, "log-error=%s\npid-file=/var/run/mysqld/mysqld.pid\n", pxcLogError(frame.OS))
	fmt.Fprintf(&b, "bind-address=0.0.0.0\n")
	fmt.Fprintf(&b, "slow_query_log=ON\nslow_query_log_file=/var/lib/mysql/slow.log\nlong_query_time=2\n")
	if frame.GTID {
		fmt.Fprintf(&b, "gtid_mode=ON\nenforce_gtid_consistency=ON\n")
	}
	fmt.Fprintf(&b, "log_bin=binlog\n%s\nbinlog_format=ROW\n", logUpdatesOption(frame.PSMajor, frame.PSVersion))
	return b.String()
}

// mysqlSetupPrimary starts the primary, sets the root password, creates the
// app/repl/monitor/cluster users, and (for semi-sync) enables the source plugin.
func (a *App) mysqlSetupPrimary(ctx context.Context, st Stack, frame designFrame, n designNode, major string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{
		"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS),
		"RESET_CMD=" + mysqlResetCmd(major),
		"ROOT_PW=" + sec.RootPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
	}
	if err := a.runStep(ctx, id, mysqlPrimaryScript, env, pr.logln); err != nil {
		return pr.fail("configure primary: %v", err)
	}
	pr.logln("primary ready; root password set; app/repl/monitor/cluster users created")
	if mysqlReplMode(frame.ReplMode) == "semisync" {
		plugin, so, enableVar := semisyncSource(major)
		if err := a.runStep(ctx, id, mysqlSemisyncScript, []string{"ROOT_PW=" + sec.RootPassword, "PLUGIN=" + plugin, "SONAME=" + so, "ENABLEVAR=" + enableVar}, pr.logln); err != nil {
			return pr.fail("enable semi-sync source: %v", err)
		}
		pr.logln("semi-sync source enabled")
	}
	return nil
}

// mysqlSetupReplica starts a secondary, sets its root password, attaches it to the
// primary via GTID auto-positioning, enables semi-sync (if requested), and makes
// it super_read_only (persisted).
func (a *App) mysqlSetupReplica(ctx context.Context, st Stack, frame designFrame, n designNode, primaryFQDN, major string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	if mysqlReplMode(frame.ReplMode) == "semisync" {
		plugin, so, enableVar := semisyncReplica(major)
		// Enable the replica plugin before START REPLICA so the IO thread registers.
		if err := a.runStep(ctx, id, mysqlReplicaSemisyncPreScript, []string{"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS), "ROOT_PW=" + sec.RootPassword, "PLUGIN=" + plugin, "SONAME=" + so, "ENABLEVAR=" + enableVar}, pr.logln); err != nil {
			return pr.fail("enable semi-sync replica: %v", err)
		}
	}
	env := []string{
		"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS),
		"RESET_CMD=" + mysqlResetCmd(major),
		"ROOT_PW=" + sec.RootPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"SOURCE_HOST=" + primaryFQDN,
	}
	if err := a.runStep(ctx, id, mysqlReplicaScript, env, pr.logln); err != nil {
		return pr.fail("attach replica: %v", err)
	}
	pr.logln("replica attached (GTID auto-position); super_read_only enabled")
	return nil
}

// ------------------------------------------------------------------ scripts

const mysqlInstallRHEL = `set -e
dnf -y -q module disable mysql >/dev/null 2>&1 || true
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install percona-server-server >/dev/null`

const mysqlInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq percona-server-server >/dev/null`

// mysqlSetRootPW is shared shell that sets root@localhost to $ROOT_PW regardless of
// distro (RHEL expired temp password / Debian auth_socket). The ALTER USER is run
// ALONE: with an expired temp password only ALTER USER is permitted, so prefixing
// it with anything (e.g. SET sql_log_bin=0) fails with ERROR 1820. The replica's
// later RESET clears the GTID this creates, so binlogging it here is harmless.
const mysqlSetRootPW = `if mysql -uroot -p"$ROOT_PW" -e "SELECT 1" >/dev/null 2>&1; then
  :
else
  TMP=$(grep -i 'temporary password' "$LOGERR" 2>/dev/null | tail -1 | sed 's/.*localhost: //')
  if [ -n "$TMP" ]; then
    mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';"
  else
    mysql -uroot -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH caching_sha2_password BY '$ROOT_PW';"
  fi
fi`

// mysqlPrimaryScript starts the primary and creates the users (which replicate to
// the secondaries via GTID).
const mysqlPrimaryScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
rm -f "$LOGERR" 2>/dev/null || true
systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl start "$UNIT"
` + mysqlSetRootPW + `
# Clear GTID/binlog history (incl. the root-password change above) so the cluster's
# replicated transactions start from a clean, shared baseline.
mysql -uroot -p"$ROOT_PW" -e "$RESET_CMD" 2>/dev/null || true
# Relax validate_password so the .env app/repl/monitor/cluster passwords are
# accepted (tolerated if the component isn't installed). Replicas receive the
# already-hashed CREATE USER form, so they don't re-validate.
mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" <<SQL
SET GLOBAL super_read_only=OFF;
SET GLOBAL read_only=OFF;
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
FLUSH PRIVILEGES;
SQL
echo "primary gtid_executed: $(mysql -uroot -p"$ROOT_PW" -N -e "SELECT @@global.gtid_executed" 2>/dev/null | tr '\n' ' ')"`

// mysqlSemisyncScript installs + enables a semi-sync plugin (source or replica)
// and persists the enable variable. Idempotent (ignores already-installed).
const mysqlSemisyncScript = `set -e
mysql -uroot -p"$ROOT_PW" -e "INSTALL PLUGIN $PLUGIN SONAME '$SONAME';" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "SET PERSIST $ENABLEVAR=1;"`

// mysqlReplicaSemisyncPreScript starts the secondary, sets root, and enables the
// replica semi-sync plugin before replication starts.
const mysqlReplicaSemisyncPreScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
rm -f "$LOGERR" 2>/dev/null || true
systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl start "$UNIT"
` + mysqlSetRootPW + `
mysql -uroot -p"$ROOT_PW" -e "INSTALL PLUGIN $PLUGIN SONAME '$SONAME';" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "SET PERSIST $ENABLEVAR=1;"`

// mysqlReplicaScript starts the secondary (if not already up), sets root, attaches
// to the primary with GTID auto-positioning, waits for the threads to run, then
// makes it super_read_only (persisted across restarts). GET_SOURCE_PUBLIC_KEY=1 is
// required for the repl user's caching_sha2_password auth over a non-TLS link.
const mysqlReplicaScript = `set -e
LOGERR=${LOGERR:-/var/log/mysqld.log}
systemctl is-active --quiet "$UNIT" || { systemctl reset-failed "$UNIT" 2>/dev/null || true; rm -f "$LOGERR" 2>/dev/null || true; systemctl start "$UNIT"; }
` + mysqlSetRootPW + `
mysql -uroot -p"$ROOT_PW" -e "STOP REPLICA;" 2>/dev/null || true
# Clear this replica's own GTID history (init + the local root-password change) so
# AUTO_POSITION fetches the full history from the primary with no errant GTIDs.
mysql -uroot -p"$ROOT_PW" -e "$RESET_CMD" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST='$SOURCE_HOST', SOURCE_PORT=3306, SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1;"
mysql -uroot -p"$ROOT_PW" -e "START REPLICA;"
OK=0
for i in $(seq 1 30); do
  S=$(mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS\G" 2>/dev/null)
  if echo "$S" | grep -q "Replica_IO_Running: Yes" && echo "$S" | grep -q "Replica_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "replica threads not running:"; mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS\G" 2>/dev/null | grep -iE 'Running|Last_(IO|SQL)_Error' | head -8; exit 1; }
mysql -uroot -p"$ROOT_PW" -e "SET PERSIST read_only=ON; SET PERSIST super_read_only=ON; SET GLOBAL super_read_only=ON;"`
