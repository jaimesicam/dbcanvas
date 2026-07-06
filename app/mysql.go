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
func mysqlServerID(name string) int { return serverIDFor(name) }

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

	// All credentials come from .env (re-read on every deploy).
	sec := mysqlFamilySecrets()
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
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
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

		// The stack-wide barrier: every MySQL-family participant must reach its reset
		// baseline before any replication (intra-cluster attach or cross-cluster
		// channels) is set up. A deferred drain guarantees this frame's members always
		// release the barrier, even on an early failure return below.
		barrier := a.deployBarrierFor(st.ID)
		if barrier != nil {
			defer func() {
				for _, n := range members {
					barrier.arrive(n.ID)
				}
			}()
		}

		// ---- Phase 2 (parallel): baseline every member — start, .env credentials
		// (incl. admin@'%'), GTID reset. No replication is wired yet. ----
		var wg2 sync.WaitGroup
		var mu2 sync.Mutex
		baseFailed := false
		for _, n := range members {
			role := "secondary"
			if n.ID == primary.ID {
				role = "primary"
			}
			wg2.Add(1)
			go func(n designNode, role string) {
				defer wg2.Done()
				pr := progs[n.ID]
				pr.phase("Setting credentials + reset baseline", 60)
				if err := a.mysqlSetupBaseline(ctx, st, frame, n, role, major, sec, pr); err != nil {
					mu2.Lock()
					baseFailed = true
					mu2.Unlock()
					return
				}
				if barrier != nil {
					barrier.arrive(n.ID)
				}
			}(n, role)
		}
		wg2.Wait()
		if baseFailed {
			return
		}

		// ---- Barrier: wait for every MySQL-family node in the stack to be baselined. ----
		if barrier != nil {
			for _, n := range members {
				progs[n.ID].phase("Waiting for all servers to reach baseline", 68)
			}
			barrier.wait(deployTimeout())
		}

		// ---- Phase 3: attach each secondary to the primary (intra-cluster replication). ----
		for _, n := range secondaries {
			pr := progs[n.ID]
			pr.phase("Attaching replica", 72)
			if err := a.mysqlAttachReplica(ctx, st, frame, n, primary.ID, primaryFQDN, sec, pr); err != nil {
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

	// All credentials come from .env (re-read on every deploy).
	sec := mysqlFamilySecrets()
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
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		if err := a.mysqlPrepareNode(ctx, st, frame, n, host, image, intranetIP, domain); err != nil {
			return // recorded its own error
		}
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Configuring Percona Server", 60)
		if err := a.mysqlSetupBaseline(ctx, st, frame, n, "standalone", major, sec, pr); err != nil {
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
	switch major {
	case "8.4":
		return "8.4"
	case "5.7":
		return "5.7"
	}
	return "8.0"
}

// psServerPackage is the OS package that installs the Percona Server daemon for a
// series. 8.0/8.4 share the unsuffixed percona-server-server; the legacy 5.7 series
// keeps a version-suffixed name that also differs between package managers.
func psServerPackage(os, major string) string {
	if major == "5.7" {
		if isDebianOS(os) {
			return "percona-server-server-5.7"
		}
		return "Percona-Server-server-57"
	}
	return "percona-server-server"
}

// psAuthPlugin is the authentication plugin used for accounts DBCanvas sets a password
// on. 8.0+ default to caching_sha2_password; 5.7 predates it, so it uses the classic
// mysql_native_password (also why 5.7 links need no GET_SOURCE_PUBLIC_KEY handshake).
func psAuthPlugin(major string) string {
	if major == "5.7" {
		return "mysql_native_password"
	}
	return "caching_sha2_password"
}

// validatePasswordRelax returns the SET statements that lower the validate_password
// policy so the .env credentials are accepted. 5.7 exposes validate_password as a
// plugin with underscore system variables (validate_password_policy); 8.0+ expose it
// as a component with dotted variables (validate_password.policy).
func validatePasswordRelax(major string) string {
	if major == "5.7" {
		return "SET GLOBAL validate_password_policy=LOW; SET GLOBAL validate_password_length=6;"
	}
	return "SET GLOBAL validate_password.policy=LOW; SET GLOBAL validate_password.length=6;"
}

// persistScope is the SET variable scope that survives a restart: 8.0+ have SET PERSIST
// (which writes mysqld-auto.cnf); 5.7 has only SET GLOBAL (runtime-only).
func persistScope(major string) string {
	if major == "5.7" {
		return "GLOBAL"
	}
	return "PERSIST"
}

// monitorGrants is the privilege list granted to the PMM monitor user on *.*. 8.0+
// include BACKUP_ADMIN (a dynamic privilege introduced in 8.0); 5.7 predates dynamic
// privileges entirely, so granting it there is a syntax error — drop it on 5.7.
func monitorGrants(major string) string {
	if major == "5.7" {
		return "SELECT, PROCESS, REPLICATION CLIENT, RELOAD"
	}
	return "SELECT, PROCESS, REPLICATION CLIENT, RELOAD, BACKUP_ADMIN"
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
	psPkg := psServerPackage(frame.OS, psMajorOf(frame.PSMajor))
	if err := a.runStep(ctx, id, instScript, []string{"PRODUCT=" + psClientProduct(psMajorOf(frame.PSMajor)), "PKG=" + psPkg}, pr.logln); err != nil {
		return pr.fail("install %s: %v", psPkg, err)
	}
	pr.logln(psPkg + " installed")

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

	// Install pmm-client only when monitored by a PMM server (frame carries the
	// association; standalone Percona Server passes a synthetic frame with it set).
	if frame.PMMNodeID != "" {
		if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
			return pr.fail("install pmm-client: %v", err)
		}
		pr.logln("pmm-client installed")
	}
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
	// Percona Server 5.7 defaults the replication metadata to FILE-based repositories,
	// which cannot carry more than the default channel — a named cross-cluster channel
	// (xrepl_*, see replication.go) then fails to CHANGE MASTER with ERROR 3077 ("To have
	// multiple channels, repository cannot be of type TABLE"). 8.0+ are already TABLE by
	// default (and removed these variables), so set them only on 5.7.
	if psMajorOf(frame.PSMajor) == "5.7" {
		fmt.Fprintf(&b, "master_info_repository=TABLE\nrelay_log_info_repository=TABLE\n")
	}
	return b.String()
}

// mysqlSetupBaseline brings ONE member (primary or secondary) to the pre-replication
// baseline: it starts the server, sets the root password, creates the admin@'%'
// superuser plus the app/repl/monitor/cluster users LOCALLY (every node creates its
// own — see the note below), then clears binlog/GTID history. For semi-sync it also
// installs+enables the role's plugin so the threads register before any attach.
//
// Credentials are created on every node (not just the primary) because the following
// RESET purges them from the binlog, so a secondary attaching later via
// AUTO_POSITION from the empty primary would never receive them via replication.
func (a *App) mysqlSetupBaseline(ctx context.Context, st Stack, frame designFrame, n designNode, role, major string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{
		"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS),
		"RESET_CMD=" + mysqlResetCmd(major),
		"VPRELAX=" + validatePasswordRelax(major), "AUTH_PLUGIN=" + psAuthPlugin(major),
		"MON_GRANTS=" + monitorGrants(major),
		"ROOT_PW=" + sec.RootPassword,
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"CC_USER=" + sec.ClusterCheckUser, "CC_PW=" + sec.ClusterCheckPassword,
	}
	if err := a.runStep(ctx, id, mysqlBaselineScript, env, pr.logln); err != nil {
		return pr.fail("configure %s baseline: %v", role, err)
	}
	pr.logln(role + " baseline ready; root/admin passwords set; app/repl/monitor/cluster users created; GTID reset")
	if mysqlReplMode(frame.ReplMode) == "semisync" {
		var plugin, so, enableVar string
		if role == "primary" {
			plugin, so, enableVar = semisyncSource(major)
		} else {
			plugin, so, enableVar = semisyncReplica(major)
		}
		if err := a.runStep(ctx, id, mysqlSemisyncScript, []string{"ROOT_PW=" + sec.RootPassword, "PLUGIN=" + plugin, "SONAME=" + so, "ENABLEVAR=" + enableVar, "SETVAR=" + persistScope(major)}, pr.logln); err != nil {
			return pr.fail("enable semi-sync %s: %v", role, err)
		}
		pr.logln("semi-sync " + role + " enabled")
	}
	return nil
}

// mysqlAttachReplica attaches an already-baselined secondary to the primary and makes
// it super_read_only (persisted). The server is already running and reset by the
// baseline step, so this only wires and starts replication — run after the stack-wide
// barrier releases. GTID frames use auto-positioning; non-GTID frames fall back to
// binary-log file/position captured from the primary now (AUTO_POSITION requires
// gtid_mode=ON, so it can't be used when the frame has GTID off).
func (a *App) mysqlAttachReplica(ctx context.Context, st Stack, frame designFrame, n designNode, primaryID, primaryFQDN string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	env := []string{
		"ROOT_PW=" + sec.RootPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"SOURCE_HOST=" + primaryFQDN,
	}
	method := "GTID auto-position"
	if frame.GTID {
		env = append(env, "AUTO=1")
	} else {
		pdep, err := a.store.GetDeployment(st.ID, primaryID)
		if err != nil || pdep.ContainerID == "" {
			return pr.fail("attach replica: primary not available to read binlog position")
		}
		file, pos, err := a.sourceBinlogPos(ctx, pdep.ContainerID, sec.RootPassword, frameMajor(frame))
		if err != nil {
			return pr.fail("attach replica: read primary binlog position: %v", err)
		}
		env = append(env, "AUTO=0", "LOG_FILE="+file, "LOG_POS="+pos)
		method = fmt.Sprintf("file/position %s:%s", file, pos)
	}
	script := mysqlAttachScript
	if psMajorOf(frame.PSMajor) == "5.7" {
		script = mysqlAttachScript57
	}
	if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
		return pr.fail("attach replica: %v", err)
	}
	pr.logln(fmt.Sprintf("replica attached (%s); super_read_only enabled", method))
	return nil
}

// ------------------------------------------------------------------ scripts

const mysqlInstallRHEL = `set -e
dnf -y -q module disable mysql >/dev/null 2>&1 || true
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install "$PKG" >/dev/null`

const mysqlInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq "$PKG" >/dev/null`

// mysqlSetRootPW is shared shell that sets root@localhost to $ROOT_PW regardless of
// distro (RHEL expired temp password / Debian auth_socket). The ALTER USER is run
// ALONE: with an expired temp password only ALTER USER is permitted, so prefixing
// it with anything (e.g. SET sql_log_bin=0) fails with ERROR 1820. The replica's
// later RESET clears the GTID this creates, so binlogging it here is harmless.
//
// validate_password (installed by default on Percona Server) rejects a weak
// $ROOT_PW with ERROR 1819. On the expired-temp path we cannot relax the policy
// first (only ALTER USER is permitted while expired), so we set a strong interim
// password, relax the policy, then apply the desired $ROOT_PW. On the Debian
// auth_socket path we're a full (non-expired) root, so we can relax up front.
//
// $VPRELAX (the validate_password relax statements) and $AUTH_PLUGIN vary by series
// (component vs plugin variables; caching_sha2_password vs mysql_native_password for
// 5.7), so both are supplied by the caller — see validatePasswordRelax / psAuthPlugin.
const mysqlSetRootPW = `if mysql -uroot -p"$ROOT_PW" -e "SELECT 1" >/dev/null 2>&1; then
  :
else
  TMP=$(grep -i 'temporary password' "$LOGERR" 2>/dev/null | tail -1 | sed 's/.*localhost: //')
  if [ -n "$TMP" ]; then
    if ! mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';" 2>/dev/null; then
      mysql -uroot --connect-expired-password -p"$TMP" -e "ALTER USER 'root'@'localhost' IDENTIFIED BY 'Dbc#Interim7Pw';"
      mysql -uroot -p'Dbc#Interim7Pw' -e "$VPRELAX" 2>/dev/null || true
      mysql -uroot -p'Dbc#Interim7Pw' -e "ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';"
    fi
  else
    mysql -uroot -e "$VPRELAX" 2>/dev/null || true
    mysql -uroot -e "ALTER USER 'root'@'localhost' IDENTIFIED WITH $AUTH_PLUGIN BY '$ROOT_PW';"
  fi
fi`

// mysqlDatadirInit is the datadir-preparation prelude shared by the async-replication
// baseline (mysqlBaselineScript) and the InnoDB/GR baseline (innodbBaseScript). It
// (re)creates the mysql-owned error log (unconditionally, at the top — /var/log is
// root-owned, so the dropped-privilege mysqld running as user=mysql can't recreate a
// deleted log there), defines say_err() to surface the real [ERROR] line, then, when the
// datadir is NOT already initialized, wipes it and initializes it explicitly with a
// minimal, replication-free defaults file so the system tables exist before the first
// start. Leaves mysqld stopped — the caller starts it.
//
// "Initialized" is detected by the presence of the privilege store — mysql.ibd (the
// 8.0/8.4 mysql-schema tablespace) or mysql/user.frm (the 5.7 MyISAM table). Checking the
// mysql/ directory alone is NOT sufficient: the package's first-start auto-init can be
// interrupted (deploy timeout, container restart) and leave that directory present but
// WITHOUT the tables inside it. mysqld then sees a populated datadir, skips its own init,
// and aborts on the missing privilege tables ("Table 'mysql.user' doesn't exist" /
// "Data Dictionary initialization failed") — restart-looping at 100% CPU under the
// package unit's Restart=on-failure. The file-presence check re-initializes that
// half-baked state while still preserving a genuinely initialized datadir on redeploy.
const mysqlDatadirInit = `LOGERR=${LOGERR:-/var/log/mysqld.log}
rm -f "$LOGERR" 2>/dev/null || true
install -m 0640 -o mysql -g mysql /dev/null "$LOGERR" 2>/dev/null || { touch "$LOGERR"; chown mysql:mysql "$LOGERR" 2>/dev/null || true; }
say_err() { echo "$1:"; grep -iE '\[ERROR\]|error' "$LOGERR" /tmp/mysql-init.log 2>/dev/null | grep -viE 'log-error|--log-error' | tail -4; }
install -d -m 0755 -o mysql -g mysql /var/run/mysqld 2>/dev/null || true
if [ ! -f /var/lib/mysql/mysql.ibd ] && [ ! -f /var/lib/mysql/mysql/user.frm ]; then
  # Clear everything incl. dotfiles (mysqld --initialize refuses a non-empty datadir).
  find /var/lib/mysql -mindepth 1 -delete 2>/dev/null || true
  printf '[mysqld]\nuser=mysql\ndatadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\nlog-error=%s\npid-file=/var/run/mysqld/mysqld.pid\n' "$LOGERR" > /tmp/mysql-init.cnf
  mysqld --defaults-file=/tmp/mysql-init.cnf --initialize-insecure >/tmp/mysql-init.log 2>&1 || { say_err "datadir initialize failed"; exit 1; }
  chown -R mysql:mysql /var/lib/mysql
fi`

// mysqlBaselineScript brings a member (primary or secondary) to the pre-replication
// baseline: initialize the datadir (if empty/half-initialized — see mysqlDatadirInit),
// start the server, set the root password, create the admin@'%' superuser and the
// app/repl/monitor/cluster users LOCALLY, then clear binlog/GTID history so the node
// starts from an empty, shared baseline. Run on EVERY member — because the RESET purges
// the user-creation from the binlog, a secondary can't inherit these users via
// replication, so each node creates its own (see mysqlSetupBaseline).
const mysqlBaselineScript = `set -e
` + mysqlDatadirInit + `
systemctl is-active --quiet "$UNIT" || { systemctl reset-failed "$UNIT" 2>/dev/null || true; systemctl start "$UNIT" || { say_err "mysqld failed to start"; exit 1; }; }
` + mysqlSetRootPW + `
# Relax validate_password so the .env passwords are accepted (tolerated if the
# component isn't installed).
mysql -uroot -p"$ROOT_PW" -e "$VPRELAX" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" <<SQL
SET GLOBAL super_read_only=OFF;
SET GLOBAL read_only=OFF;
` + mysqlAdminUserSQL + `
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT $MON_GRANTS ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
ALTER USER '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
GRANT PROCESS ON *.* TO '$CC_USER'@'localhost';
FLUSH PRIVILEGES;
SQL
# Clear GTID/binlog history now that every local user exists, so the node starts
# replication from an empty, shared GTID baseline.
mysql -uroot -p"$ROOT_PW" -e "$RESET_CMD" 2>/dev/null || true
echo "gtid_executed after reset: $(mysql -uroot -p"$ROOT_PW" -N -e "SELECT @@global.gtid_executed" 2>/dev/null | tr '\n' ' ')"`

// mysqlSemisyncScript installs + enables a semi-sync plugin (source or replica)
// and persists the enable variable. Idempotent (ignores already-installed).
const mysqlSemisyncScript = `set -e
mysql -uroot -p"$ROOT_PW" -e "INSTALL PLUGIN $PLUGIN SONAME '$SONAME';" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "SET $SETVAR $ENABLEVAR=1;"`

// mysqlAttachScript attaches an already-baselined, already-running secondary to the
// primary, waits for the threads to run, then makes it super_read_only (persisted).
// With $AUTO=1 (frame has GTID on) it uses GTID auto-positioning; otherwise it starts
// from the primary's binlog file/position in $LOG_FILE/$LOG_POS (AUTO_POSITION=1 is
// rejected when gtid_mode=OFF). No server start / root set / RESET — the baseline step
// already did those and left an empty baseline. GET_SOURCE_PUBLIC_KEY=1 is required for
// the repl user's caching_sha2_password auth over a non-TLS link.
const mysqlAttachScript = `set -e
if [ "$AUTO" = 1 ]; then
  POS="SOURCE_AUTO_POSITION=1"
else
  POS="SOURCE_LOG_FILE='$LOG_FILE', SOURCE_LOG_POS=$LOG_POS"
fi
mysql -uroot -p"$ROOT_PW" -e "STOP REPLICA;" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "CHANGE REPLICATION SOURCE TO SOURCE_HOST='$SOURCE_HOST', SOURCE_PORT=3306, SOURCE_USER='$REPL_USER', SOURCE_PASSWORD='$REPL_PW', $POS, GET_SOURCE_PUBLIC_KEY=1;"
mysql -uroot -p"$ROOT_PW" -e "START REPLICA;"
OK=0
for i in $(seq 1 30); do
  S=$(mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS\G" 2>/dev/null)
  if echo "$S" | grep -q "Replica_IO_Running: Yes" && echo "$S" | grep -q "Replica_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "replica threads not running:"; mysql -uroot -p"$ROOT_PW" -e "SHOW REPLICA STATUS\G" 2>/dev/null | grep -iE 'Running|Last_(IO|SQL)_Error' | head -8; exit 1; }
mysql -uroot -p"$ROOT_PW" -e "SET PERSIST read_only=ON; SET PERSIST super_read_only=ON; SET GLOBAL super_read_only=ON;"`

// mysqlAttachScript57 is the Percona Server 5.7 counterpart of mysqlAttachScript.
// 5.7 predates the modern replication vocabulary: it uses CHANGE MASTER TO with
// MASTER_* options (MASTER_AUTO_POSITION / MASTER_LOG_FILE+POS), START/STOP SLAVE,
// SHOW SLAVE STATUS (Slave_IO_Running / Slave_SQL_Running), needs no
// GET_SOURCE_PUBLIC_KEY (repl user authenticates with mysql_native_password), and has
// no SET PERSIST — read_only is set with SET GLOBAL (runtime-only, re-applied on deploy).
const mysqlAttachScript57 = `set -e
if [ "$AUTO" = 1 ]; then
  POS="MASTER_AUTO_POSITION=1"
else
  POS="MASTER_LOG_FILE='$LOG_FILE', MASTER_LOG_POS=$LOG_POS"
fi
mysql -uroot -p"$ROOT_PW" -e "STOP SLAVE;" 2>/dev/null || true
mysql -uroot -p"$ROOT_PW" -e "CHANGE MASTER TO MASTER_HOST='$SOURCE_HOST', MASTER_PORT=3306, MASTER_USER='$REPL_USER', MASTER_PASSWORD='$REPL_PW', $POS;"
mysql -uroot -p"$ROOT_PW" -e "START SLAVE;"
OK=0
for i in $(seq 1 30); do
  S=$(mysql -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS\G" 2>/dev/null)
  if echo "$S" | grep -q "Slave_IO_Running: Yes" && echo "$S" | grep -q "Slave_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || { echo "replica threads not running:"; mysql -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS\G" 2>/dev/null | grep -iE 'Running|Last_(IO|SQL)_Error' | head -8; exit 1; }
mysql -uroot -p"$ROOT_PW" -e "SET GLOBAL read_only=ON; SET GLOBAL super_read_only=ON;"`
