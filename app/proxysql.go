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

// ProxySQL node. ProxySQL is a high-performance MySQL proxy that sits in front of
// a Percona XtraDB Cluster (PXC) and routes application traffic (read/write split
// or load-balanced) to the cluster's nodes. It runs on a systemd OS image (built
// by `make images`), is wired to a PXC cluster frame via a canvas association
// line, and is configured with `proxysql-admin` against /etc/proxysql-admin.cnf.
// Like the PXC nodes it can be monitored by PMM (pmm-client is always installed)
// and its admin/MySQL ports can be published to the host.

// proxysql admin (6032) and MySQL traffic (6033) ports.
const (
	proxysqlAdminPort = 6032
	proxysqlMySQLPort = 6033
)

// proxysqlConfig is the non-secret profile shown for a deployed ProxySQL node.
type proxysqlConfig struct {
	Image           string `json:"image"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	Major           string `json:"major"`           // "2" | "3"
	ProxySQLVersion string `json:"proxysqlVersion"` // selected minor (display)
	Hostname        string `json:"hostname"`
	FQDN            string `json:"fqdn"`
	Mode            string `json:"mode"`            // PXC: singlewrite|loadbal · MySQL: primary|rwsplit
	BackendKind     string `json:"backendKind"`     // "pxc" | "mysql"
	ProxySQLCluster string `json:"proxysqlCluster"` // ProxySQL cluster frame name (members only)
	Cluster         string `json:"cluster"`         // associated PXC cluster name
	ClusterHost     string `json:"clusterHost"`     // CLUSTER_HOSTNAME (a PXC FQDN)
	MonitoredBy     string `json:"monitoredBy"`     // PMM node FQDN, if any
	AdminPort       int    `json:"adminPort"`       // host port mapped to 6032 (0 = not published)
	MySQLPort       int    `json:"mysqlPort"`       // host port mapped to 6033 (0 = not published)
	Ports           []int  `json:"ports"`
}

// proxysqlSecrets holds ProxySQL's admin-interface credential plus the backend
// (PXC) credentials it was configured with — surfaced read-only in the manager.
type proxysqlSecrets struct {
	AdminUser       string `json:"adminUser"` // ProxySQL admin interface (6032)
	AdminPassword   string `json:"adminPassword"`
	AppUser         string `json:"appUser"` // CLUSTER_APP_USERNAME (app traffic on 6033)
	AppPassword     string `json:"appPassword"`
	MonitorUser     string `json:"monitorUser"` // MONITOR_USERNAME (backend health checks)
	MonitorPassword string `json:"monitorPassword"`
	ClusterUser     string `json:"clusterUser"` // CLUSTER_USERNAME (PXC 'cluster' admin user)
	ClusterPassword string `json:"clusterPassword"`
}

// proxysqlMode normalizes the PXC implementation mode (single-writer or
// load-balanced); mysqlProxyMode normalizes the MySQL-replication mode (all traffic
// to the primary, or read/write split between primary and replicas).
func proxysqlMode(m string) string {
	if m == "loadbal" {
		return "loadbal"
	}
	return "singlewrite"
}
func mysqlProxyMode(m string) string {
	if m == "primary" {
		return "primary"
	}
	return "rwsplit"
}

// proxysqlPackage maps a ProxySQL major series to its Percona package name.
func proxysqlPackage(major string) string {
	if major == "3" {
		return "proxysql3"
	}
	return "proxysql2"
}

// proxysqlMySQLEnv builds the exec env for proxysqlMySQLConfigureScript from the
// resolved backend credentials + the MySQL member FQDNs.
func proxysqlMySQLEnv(sec proxysqlSecrets, members []string, mode string) []string {
	monUser := sec.MonitorUser
	if monUser == "" {
		monUser = "monitor"
	}
	return []string{
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"MON_USER=" + monUser, "MON_PW=" + sec.MonitorPassword,
		"SERVERS=" + strings.Join(members, ","),
		"MODE=" + mysqlProxyMode(mode),
	}
}

// psClientProduct maps a PXC major series to the percona-release product for the
// matching Percona Server client (ps84lts for 8.4, ps80 otherwise).
func psClientProduct(pxcMajor string) string {
	switch pxcMajor {
	case "8.4":
		return "ps84lts"
	case "5.7":
		return "ps57"
	}
	return "ps80"
}

// proxysqlAdminPassword is the ProxySQL admin-interface (6032) password, taken from
// .env (re-read on every deploy). ProxySQL ships with admin/admin; proxysqlStartScript
// rewrites the credential to this value on first start.
func proxysqlAdminPassword() string { return envOr("PROXYSQL_ADMIN_PASSWORD", "admin_password") }

// proxysqlStartEnv is the admin credential env for proxysqlStartScript (which sets
// the 6032 admin password on first start).
func proxysqlStartEnv() []string {
	return []string{"ADMIN_USER=admin", "ADMIN_PW=" + proxysqlAdminPassword()}
}

func proxysqlAlias(label string) string {
	a := sanitizeName(strings.TrimSpace(label))
	if a == "" {
		a = "proxysql"
	}
	return a
}

// proxysqlPlan is the effective configuration for one ProxySQL instance, whether a
// standalone node or a member of a ProxySQL cluster frame. AssocID is the canvas id
// whose association edge points at the backend PXC cluster (the node id for a
// standalone ProxySQL, the frame id for a cluster member).
type proxysqlPlan struct {
	NodeID          string
	Label           string
	AssocID         string
	OS, OSVersion   string
	Arch            string
	Major, Version  string
	Mode            string
	UseProxy        bool
	PMMNodeID       string
	ExportEnabled   bool
	ExportHostPort  int
	ProxySQLCluster string // ProxySQL cluster frame label (members only; "" standalone)
}

// provisionProxySQL records + provisions a standalone ProxySQL node.
func (a *App) provisionProxySQL(st Stack, n designNode, doc designDoc) {
	a.provisionProxySQLInstance(st, doc, proxysqlPlan{
		NodeID: n.ID, Label: n.Label, AssocID: n.ID,
		OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch,
		Major: proxysqlMajorOf(n.ProxySQLMajor), Version: n.ProxySQLVersion,
		Mode: n.Mode, UseProxy: n.UseProxy, PMMNodeID: n.PMMNodeID,
		ExportEnabled: n.ExportEnabled, ExportHostPort: n.ExportHostPort,
	})
}

// provisionProxySQLFrame brings up a ProxySQL cluster frame as one unit: it installs
// every member, joins them into a native ProxySQL cluster (shared config via
// proxysql_servers), and runs `proxysql-admin --enable` on a single primary member —
// the backend config then syncs to the rest. The whole cluster fronts the one PXC
// cluster the frame is associated with.
func (a *App) provisionProxySQLFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "proxysql" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		return
	}

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == frame.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}

	// Record every member pending with its profile (admin creds reused on redeploy).
	for _, n := range members {
		host := hosts[n.ID]
		if host == "" {
			host = proxysqlAlias(n.Label)
		}
		var sec proxysqlSecrets
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			json.Unmarshal(dep.Secrets, &sec)
		}
		sec.AdminUser, sec.AdminPassword = "admin", proxysqlAdminPassword()
		cfg := proxysqlConfig{
			Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Major: proxysqlMajorOf(frame.ProxySQLMajor), ProxySQLVersion: frame.ProxySQLVersion,
			Hostname: host, FQDN: fqdnOf(host, domain), Mode: proxysqlMode(frame.Mode),
			ProxySQLCluster: frame.Label, MonitoredBy: monitoredBy,
			Ports: []int{proxysqlAdminPort, proxysqlMySQLPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		secJSON, _ := json.Marshal(sec)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	go func() {
		ctx := context.Background()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			progs[n.ID] = pr
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			pr.phase("Waiting for Intranet to be ready", 5)
		}
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
		backFrame, backendKind, ok := backendFrameForProxySQL(doc, frame.ID)
		if !ok {
			failAll("no backend cluster is associated — link a PXC or MySQL cluster to this ProxySQL cluster")
			return
		}
		for _, n := range members {
			progs[n.ID].phase("Waiting for backend cluster", 15)
		}
		var clusterHost, clientSeries string
		var mysqlMembers []string
		var pxcSec pxcSecrets
		if backendKind == "mysql" {
			ph, ms, sc, cerr := a.waitMySQLRunning(ctx, st.ID, backFrame, doc, domain, deployTimeout())
			if cerr != nil {
				failAll("%v", cerr)
				return
			}
			clusterHost, mysqlMembers, pxcSec, clientSeries = ph, ms, sc, psMajorOf(backFrame.PSMajor)
		} else {
			ch, sc, cerr := a.waitPXCRunning(ctx, st.ID, backFrame, doc, domain, deployTimeout())
			if cerr != nil {
				failAll("%v", cerr)
				return
			}
			clusterHost, pxcSec, clientSeries = ch, sc, backFrame.PXCMajor
		}

		// ---- Phase 1 (parallel): container + install + start proxysql per member ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.proxysqlPrepareMember(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain, clientSeries, backFrame.Label, clusterHost, pxcSec, progs[n.ID]); err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if failed {
			return // each failed member recorded its own error
		}
		// Record the effective mode + backend kind on each member for display.
		effMode := proxysqlMode(frame.Mode)
		if backendKind == "mysql" {
			effMode = mysqlProxyMode(frame.Mode)
		}
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			var c proxysqlConfig
			json.Unmarshal(dep.Config, &c)
			c.Mode, c.BackendKind = effMode, backendKind
			cj, _ := json.Marshal(c)
			a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cj, Secrets: dep.Secrets})
		}
		a.reconcileStackDNS(ctx, st.ID) // member FQDNs must resolve for clustering

		// ---- Phase 2: join all members into a native ProxySQL cluster ----
		var fqdns []string
		for _, n := range members {
			fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
		}
		clEnv := []string{
			"ADMIN_USER=admin", "ADMIN_PW=" + proxysqlAdminPassword(),
			"CL_USER=cluster", "CL_PW=" + pxcSec.ClusterPassword,
			"SERVERS=" + strings.Join(fqdns, ","),
		}
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			progs[n.ID].phase("Joining ProxySQL cluster", 80)
			if err := a.runStep(ctx, dep.ContainerID, proxysqlClusterScript, clEnv, progs[n.ID].logln); err != nil {
				progs[n.ID].fail("join ProxySQL cluster: %v", err)
				return
			}
		}

		// ---- Phase 3: configure the primary (config syncs to the rest via clustering) ----
		primary := members[0]
		pdep, _ := a.store.GetDeployment(st.ID, primary.ID)
		monUser := pxcSec.MonitorUser
		if monUser == "" {
			monUser = "monitor"
		}
		if backendKind == "mysql" {
			progs[primary.ID].phase("Configuring ProxySQL for MySQL (primary)", 88)
			env := proxysqlMySQLEnv(proxysqlSecrets{AdminUser: "admin", AdminPassword: proxysqlAdminPassword(), AppUser: pxcSec.AppUser, AppPassword: pxcSec.AppPassword, MonitorUser: monUser, MonitorPassword: pxcSec.MonitorPassword}, mysqlMembers, frame.Mode)
			if err := a.runStep(ctx, pdep.ContainerID, proxysqlMySQLConfigureScript, env, progs[primary.ID].logln); err != nil {
				progs[primary.ID].fail("configure ProxySQL for MySQL: %v", err)
				return
			}
			progs[primary.ID].logln("ProxySQL manually configured for MySQL on primary; syncs to the cluster")
		} else {
			cfgEnv := []string{
				"PSQL_ADMIN_USER=admin", "PSQL_ADMIN_PW=" + proxysqlAdminPassword(),
				"CLUSTER_USER=" + pxcSec.ClusterUser, "CLUSTER_PW=" + pxcSec.ClusterPassword,
				"CLUSTER_HOST=" + clusterHost,
				"MON_USER=" + monUser, "MON_PW=" + pxcSec.MonitorPassword,
				"APP_USER=" + pxcSec.AppUser, "APP_PW=" + pxcSec.AppPassword,
				"MODE=" + proxysqlMode(frame.Mode),
			}
			progs[primary.ID].phase("Configuring proxysql-admin (primary)", 88)
			if err := a.runStep(ctx, pdep.ContainerID, proxysqlConfigureScript, cfgEnv, progs[primary.ID].logln); err != nil {
				progs[primary.ID].fail("configure proxysql-admin: %v", err)
				return
			}
			progs[primary.ID].logln("proxysql-admin enabled on primary; config syncs to the cluster")
		}

		// ---- Phase 4: PMM registration + finalize ----
		for _, n := range members {
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			var sec proxysqlSecrets
			json.Unmarshal(dep.Secrets, &sec)
			if frame.PMMNodeID != "" {
				progs[n.ID].phase("Registering with PMM", 95)
				a.proxysqlRegisterPMM(ctx, st, n.ID, frame.OS, doc, frame.PMMNodeID, sec, progs[n.ID].logln)
			}
			progs[n.ID].phase("Running", 100)
			progs[n.ID].p.Message = "provisioned"
			progs[n.ID].save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d proxysql cluster %s: provisioned (%d member(s))", st.ID, frame.Label, len(members))
	}()
}

// proxysqlPrepareMember creates a ProxySQL cluster member's container, installs
// ProxySQL + the mysql client + pmm-client, starts proxysql (so its admin
// interface is up for clustering), and records the container id + published ports.
func (a *App) proxysqlPrepareMember(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, intranetIP, domain, pxcMajor, pxcClusterName, clusterHost string, pxcSec pxcSecrets, pr *pxcProg) error {
	if host == "" {
		host = proxysqlAlias(n.Label)
	}
	pr.phase("Creating container", 30)
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
		spec.PublishMap = []PortMap{
			{ContainerPort: proxysqlMySQLPort, HostPort: n.ExportHostPort},
			{ContainerPort: proxysqlAdminPort, HostPort: 0},
		}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	// Record container id + ports + backend creds.
	var cfg proxysqlConfig
	var sec proxysqlSecrets
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
		json.Unmarshal(dep.Secrets, &sec)
	}
	cfg.Cluster, cfg.ClusterHost = pxcClusterName, clusterHost
	cfg.AdminPort, cfg.MySQLPort = a.readProxySQLPorts(ctx, id, n.ExportEnabled)
	sec.AppUser, sec.AppPassword = pxcSec.AppUser, pxcSec.AppPassword
	sec.MonitorUser, sec.MonitorPassword = pxcSec.MonitorUser, pxcSec.MonitorPassword
	sec.ClusterUser, sec.ClusterPassword = pxcSec.ClusterUser, pxcSec.ClusterPassword
	if sec.MonitorUser == "" {
		sec.MonitorUser = "monitor"
	}
	if sec.ClusterUser == "" {
		sec.ClusterUser = "cluster"
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 40)
	if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, frame.OS, pr.logln)
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

	pr.phase("Installing ProxySQL", 55)
	pkg := proxysqlPackage(proxysqlMajorOf(frame.ProxySQLMajor))
	instScript, clientScript, pmmScript := proxysqlInstallRHEL, proxysqlInstallClientRHEL, pxcInstallPMMClientRHEL
	if debian {
		instScript, clientScript, pmmScript = proxysqlInstallDebian, proxysqlInstallClientDebian, pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{"PKG=" + pkg}, pr.logln); err != nil {
		return pr.fail("install %s: %v", pkg, err)
	}
	if err := a.runStep(ctx, id, clientScript, []string{"PRODUCT=" + psClientProduct(pxcMajor)}, pr.logln); err != nil {
		return pr.fail("install percona-server-client: %v", err)
	}
	// Install pmm-client only when the cluster is monitored by a PMM server.
	if frame.PMMNodeID != "" {
		if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
			return pr.fail("install pmm-client: %v", err)
		}
	}
	pr.logln("packages installed (proxysql, mysql client)")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	pr.phase("Starting ProxySQL", 70)
	if err := a.runStep(ctx, id, proxysqlStartScript, proxysqlStartEnv(), pr.logln); err != nil {
		return pr.fail("start proxysql: %v", err)
	}
	return nil
}

// provisionProxySQLInstance records the deployment and starts an async provisioning
// goroutine for one ProxySQL instance (the backend PXC cluster comes from the
// association on p.AssocID, the creds/settings from p).
func (a *App) provisionProxySQLInstance(st Stack, doc designDoc, p proxysqlPlan) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[p.NodeID]
	if host == "" {
		host = proxysqlAlias(p.Label)
	}
	image := pxcImage(p.OS, p.OSVersion, p.Arch)

	monitoredBy := ""
	if p.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == p.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(stackHostnames(doc)[m.ID], domain)
			}
		}
	}

	// Admin-interface credential comes from .env (re-read on every deploy).
	var sec proxysqlSecrets
	if dep, err := a.store.GetDeployment(st.ID, p.NodeID); err == nil && len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &sec)
	}
	sec.AdminUser, sec.AdminPassword = "admin", proxysqlAdminPassword()

	cfg := proxysqlConfig{
		Image: image, OS: p.OS, Arch: archOr(p.Arch),
		Major: proxysqlMajorOf(p.Major), ProxySQLVersion: p.Version,
		Hostname: host, FQDN: fqdnOf(host, domain), Mode: proxysqlMode(p.Mode),
		ProxySQLCluster: p.ProxySQLCluster,
		MonitoredBy:     monitoredBy, Ports: []int{proxysqlAdminPort, proxysqlMySQLPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: p.NodeID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		prog := &provProgress{Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, p.NodeID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(ph string, pct int) { prog.Phase = ph; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d proxysql %s: %s", st.ID, p.NodeID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, p.NodeID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, p.NodeID, DeployProvisioning)

		// The Intranet is the stack's DNS resolver / CA, so ProxySQL must wait for it.
		setPhase("Waiting for Intranet to be ready", 5)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failNode("%v", werr)
			return
		}

		// Resolve the associated backend cluster (PXC or MySQL replication) and wait
		// for it to be running so its users/credentials exist before we configure.
		frame, backendKind, ok := backendFrameForProxySQL(doc, p.AssocID)
		if !ok {
			failNode("no backend cluster is associated — link a PXC or MySQL cluster to this ProxySQL")
			return
		}
		setPhase("Waiting for backend cluster", 15)
		var clusterHost, clientSeries string
		var mysqlMembers []string
		var pxcSec pxcSecrets
		if backendKind == "mysql" {
			ph, ms, sc, cerr := a.waitMySQLRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				failNode("%v", cerr)
				return
			}
			clusterHost, mysqlMembers, pxcSec, clientSeries = ph, ms, sc, psMajorOf(frame.PSMajor)
		} else {
			ch, sc, cerr := a.waitPXCRunning(ctx, st.ID, frame, doc, domain, deployTimeout())
			if cerr != nil {
				failNode("%v", cerr)
				return
			}
			clusterHost, pxcSec, clientSeries = ch, sc, frame.PXCMajor
		}
		cfg.Cluster, cfg.ClusterHost, cfg.BackendKind = frame.Label, clusterHost, backendKind
		if backendKind == "mysql" {
			cfg.Mode = mysqlProxyMode(p.Mode)
		} else {
			cfg.Mode = proxysqlMode(p.Mode)
		}
		logln(backendKind + " cluster " + frame.Label + " is running (backend " + clusterHost + ")")

		setPhase("Creating container", 25)
		name := containerName(st.ID, p.NodeID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		if p.ExportEnabled {
			spec.PublishMap = []PortMap{
				{ContainerPort: proxysqlMySQLPort, HostPort: p.ExportHostPort},
				{ContainerPort: proxysqlAdminPort, HostPort: 0},
			}
		}
		id, err := a.docker.ContainerCreate(ctx, spec)
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		cfg.AdminPort, cfg.MySQLPort = a.readProxySQLPorts(ctx, id, p.ExportEnabled)
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: p.NodeID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		setPhase("Waiting for systemd", 35)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			failNode("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, p.OS, logln)
		a.ensureDNFIPv4(ctx, id, p.OS, logln)

		debian := isDebianOS(p.OS)

		// Optionally route package egress through the Intranet Squid proxy. Set it on
		// the package manager once so every install below inherits it.
		if p.UseProxy {
			proxyScript := pkgProxyRHEL
			if debian {
				proxyScript = pkgProxyDebian
			}
			if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, logln); err != nil {
				failNode("configure package proxy: %v", err)
				return
			}
			logln("package egress via Intranet proxy")
		}

		setPhase("Installing ProxySQL", 45)
		pkg := proxysqlPackage(cfg.Major)
		instScript := proxysqlInstallRHEL
		clientScript := proxysqlInstallClientRHEL
		pmmScript := pxcInstallPMMClientRHEL
		if debian {
			instScript = proxysqlInstallDebian
			clientScript = proxysqlInstallClientDebian
			pmmScript = pxcInstallPMMClientDebian
		}
		if err := a.runStep(ctx, id, instScript, []string{"PKG=" + pkg}, logln); err != nil {
			failNode("install %s: %v", pkg, err)
			return
		}
		logln(pkg + " installed")

		// ProxySQL talks to the backend with the mysql client, so the Percona Server
		// client must be installed first (series matches the backend).
		setPhase("Installing MySQL client", 58)
		clientEnv := []string{"PRODUCT=" + psClientProduct(clientSeries)}
		if err := a.runStep(ctx, id, clientScript, clientEnv, logln); err != nil {
			failNode("install percona-server-client: %v", err)
			return
		}
		logln("percona-server-client installed")

		// Install pmm-client only when this ProxySQL is monitored by a PMM server.
		if p.PMMNodeID != "" {
			setPhase("Installing PMM client", 66)
			if err := a.runStep(ctx, id, pmmScript, nil, logln); err != nil {
				failNode("install pmm-client: %v", err)
				return
			}
		}
		logln("pmm-client installed")
		a.ensureRsyslog(ctx, id, p.OS, logln)

		// Fill in the backend credentials from the PXC cluster's secrets. ProxySQL's
		// CLUSTER_USERNAME/PASSWORD use the dedicated 'cluster' user (CLUSTER_PASSWORD
		// from .env), not root.
		sec.AppUser, sec.AppPassword = pxcSec.AppUser, pxcSec.AppPassword
		sec.MonitorUser, sec.MonitorPassword = pxcSec.MonitorUser, pxcSec.MonitorPassword
		sec.ClusterUser, sec.ClusterPassword = pxcSec.ClusterUser, pxcSec.ClusterPassword
		if sec.MonitorUser == "" {
			sec.MonitorUser = "monitor"
		}
		if sec.ClusterUser == "" {
			sec.ClusterUser = "cluster"
		}
		secJSON, _ = json.Marshal(sec)

		if backendKind == "mysql" {
			// proxysql-admin only supports PXC; a MySQL replication backend is wired
			// manually over the admin interface (6032), so proxysql must be running
			// first (the proxysql-admin path starts it itself; here we do it).
			setPhase("Configuring ProxySQL (MySQL backend)", 80)
			if err := a.runStep(ctx, id, proxysqlStartScript, proxysqlStartEnv(), logln); err != nil {
				failNode("start proxysql: %v", err)
				return
			}
			if err := a.runStep(ctx, id, proxysqlMySQLConfigureScript, proxysqlMySQLEnv(sec, mysqlMembers, cfg.Mode), logln); err != nil {
				failNode("configure ProxySQL for MySQL: %v", err)
				return
			}
			logln("ProxySQL manually configured for MySQL replication (mode=" + cfg.Mode + ")")
		} else {
			setPhase("Configuring proxysql-admin", 80)
			// Start proxysql first so its 6032 admin password is set to the .env value
			// before proxysql-admin --enable connects with it.
			if err := a.runStep(ctx, id, proxysqlStartScript, proxysqlStartEnv(), logln); err != nil {
				failNode("start proxysql: %v", err)
				return
			}
			cfgEnv := []string{
				"PSQL_ADMIN_USER=" + sec.AdminUser, "PSQL_ADMIN_PW=" + sec.AdminPassword,
				"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
				"CLUSTER_HOST=" + clusterHost,
				"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
				"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
				"MODE=" + cfg.Mode,
			}
			if err := a.runStep(ctx, id, proxysqlConfigureScript, cfgEnv, logln); err != nil {
				failNode("configure proxysql-admin: %v", err)
				return
			}
			logln("proxysql-admin configured and enabled (mode=" + cfg.Mode + ")")
		}

		// Optional PMM monitoring (pmm-client is already installed).
		if p.PMMNodeID != "" {
			setPhase("Registering with PMM", 92)
			a.proxysqlRegisterPMM(ctx, st, p.NodeID, p.OS, doc, p.PMMNodeID, sec, logln) // best-effort
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: p.NodeID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)

		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, p.NodeID, DeployRunning)
		log.Printf("stack %d proxysql %s: provisioned", st.ID, p.NodeID)
	}()
}

// proxysqlMajorOf normalizes a ProxySQL major series (default "2").
func proxysqlMajorOf(major string) string {
	if major == "3" {
		return "3"
	}
	return "2"
}

// readProxySQLPorts reads the published host ports for 6033/6032 (0 when not
// published).
func (a *App) readProxySQLPorts(ctx context.Context, id string, exported bool) (adminPort, mysqlPort int) {
	if !exported {
		return 0, 0
	}
	if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", proxysqlAdminPort)); e == nil {
		if p, e2 := strconv.Atoi(hp); e2 == nil {
			adminPort = p
		}
	}
	if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", proxysqlMySQLPort)); e == nil {
		if p, e2 := strconv.Atoi(hp); e2 == nil {
			mysqlPort = p
		}
	}
	return adminPort, mysqlPort
}

// waitPXCRunning blocks until every regular (data) member of a PXC frame is
// running, then returns the FQDN of the first regular member (a CLUSTER_HOSTNAME
// for proxysql-admin) and that member's secrets (root/app/monitor credentials).
func (a *App) waitPXCRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) (string, pxcSecrets, error) {
	hosts := stackHostnames(doc)
	var regulars []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "pxc" && n.Role != "arbitrator" {
			regulars = append(regulars, n)
		}
	}
	if len(regulars) == 0 {
		return "", pxcSecrets{}, fmt.Errorf("associated PXC cluster %s has no regular (data) node", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var sec pxcSecrets
		for _, n := range regulars {
			dep, err := a.store.GetDeployment(stackID, n.ID)
			if err != nil {
				allRunning = false // not recorded yet — keep waiting
				break
			}
			if dep.State == DeployError {
				return "", pxcSecrets{}, fmt.Errorf("associated PXC cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
		}
		if allRunning {
			return fqdnOf(hosts[regulars[0].ID], domain), sec, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", pxcSecrets{}, fmt.Errorf("associated PXC cluster %s did not become ready within %s", frame.Label, timeout)
}

// proxysqlRegisterPMM registers a ProxySQL instance with the PMM server
// (best-effort), over its admin interface, using the node's own label/OS.
func (a *App) proxysqlRegisterPMM(ctx context.Context, st Stack, nodeID, os string, doc designDoc, pmmNodeID string, sec proxysqlSecrets, logln func(string)) {
	pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, pmmNodeID)
	if !ok {
		logln("PMM registration skipped: PMM node not running")
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, nodeID)
	label := nodeID
	for _, m := range doc.Nodes {
		if m.ID == nodeID {
			label = m.Label
		}
	}
	env := []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"NODE=" + label,
	}
	script := proxysqlPMMRHEL
	if isDebianOS(os) {
		script = proxysqlPMMDebian
	}
	if _, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
		logln("PMM registration skipped: " + err.Error())
	} else {
		logln("registered with PMM at " + pmmFQDN)
	}
}

// ------------------------------------------------------------------ scripts

// pkgProxy{RHEL,Debian} point the package manager at the Intranet Squid proxy so
// all subsequent installs egress through it.
const pkgProxyRHEL = `set -e
grep -q '^proxy=' /etc/dnf/dnf.conf 2>/dev/null || echo "proxy=$PROXY" >> /etc/dnf/dnf.conf`

const pkgProxyDebian = `set -e
echo "Acquire::http::Proxy \"$PROXY\";" > /etc/apt/apt.conf.d/01dbcanvas-proxy`

// proxysql-admin shells out to `which`, which is not present on a minimal OEL
// image — install it alongside ProxySQL (Debian's `which` ships in debianutils,
// already present, but install it to be explicit/equivalent).
const proxysqlInstallRHEL = `set -e
percona-release setup -y proxysql >/dev/null 2>&1
dnf -y -q install "$PKG" which >/dev/null`

const proxysqlInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y proxysql >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq "$PKG" debianutils >/dev/null`

// proxysqlInstallClient{RHEL,Debian} install the Percona Server mysql client
// (percona-server-client) used by proxysql-admin to talk to the PXC cluster.
const proxysqlInstallClientRHEL = `set -e
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install percona-server-client >/dev/null`

const proxysqlInstallClientDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq percona-server-client >/dev/null`

// proxysqlStartScript enables + starts the proxysql service so its admin interface
// (6032) is reachable (needed before configuring native clustering), then rewrites
// the admin credential to the .env password. ProxySQL ships with admin/admin; this
// connects with whichever works (the target password on a redeploy, else the
// admin/admin default on a fresh node) and, if still on the default, sets the
// admin-admin_credentials to $ADMIN_USER:$ADMIN_PW and persists it to disk.
const proxysqlStartScript = `set -e
systemctl enable --now proxysql >/dev/null 2>&1 || systemctl restart proxysql
CONN=""
for i in $(seq 1 20); do
  if mysql -u"$ADMIN_USER" -p"$ADMIN_PW" -h127.0.0.1 -P6032 --protocol=tcp -N -e "SELECT 1" >/dev/null 2>&1; then CONN=new; break; fi
  if mysql -uadmin -padmin -h127.0.0.1 -P6032 --protocol=tcp -N -e "SELECT 1" >/dev/null 2>&1; then CONN=def; break; fi
  sleep 2
done
[ -n "$CONN" ] || { echo "proxysql admin interface (6032) did not come up"; exit 1; }
if [ "$CONN" = def ] && [ "$ADMIN_USER:$ADMIN_PW" != "admin:admin" ]; then
  mysql -uadmin -padmin -h127.0.0.1 -P6032 --protocol=tcp -e "UPDATE global_variables SET variable_value='$ADMIN_USER:$ADMIN_PW' WHERE variable_name='admin-admin_credentials'; LOAD ADMIN VARIABLES TO RUNTIME; SAVE ADMIN VARIABLES TO DISK;"
fi`

// proxysqlClusterScript joins a ProxySQL instance to the native ProxySQL cluster:
// it registers a dedicated cluster sync credential and lists every member in
// proxysql_servers, so the members keep their config (mysql_servers/users/etc.) in
// sync — only the primary then needs `proxysql-admin --enable`. SERVERS is a
// comma-separated list of member FQDNs.
const proxysqlClusterScript = `set -e
A="mysql -u$ADMIN_USER -p$ADMIN_PW -h127.0.0.1 -P6032 --protocol=tcp"
$A -e "UPDATE global_variables SET variable_value='$ADMIN_USER:$ADMIN_PW;$CL_USER:$CL_PW' WHERE variable_name='admin-admin_credentials';
UPDATE global_variables SET variable_value='$CL_USER' WHERE variable_name='admin-cluster_username';
UPDATE global_variables SET variable_value='$CL_PW' WHERE variable_name='admin-cluster_password';
LOAD ADMIN VARIABLES TO RUNTIME; SAVE ADMIN VARIABLES TO DISK;"
SQL="DELETE FROM proxysql_servers;"
for h in $(echo "$SERVERS" | tr ',' ' '); do SQL="$SQL INSERT INTO proxysql_servers (hostname,port,weight,comment) VALUES ('$h',6032,0,'$h');"; done
SQL="$SQL LOAD PROXYSQL SERVERS TO RUNTIME; SAVE PROXYSQL SERVERS TO DISK;"
$A -e "$SQL"`

// proxysqlConfigureScript edits /etc/proxysql-admin.cnf with the cluster + app +
// monitor credentials and the implementation MODE, starts proxysql, and enables
// the configuration (proxysql-admin --enable discovers the PXC topology from
// CLUSTER_HOSTNAME). The MONITOR_USERNAME/PASSWORD are the PXC monitor user, the
// CLUSTER_* are the PXC 'cluster' admin user, and CLUSTER_APP_* are the PXC app user.
const proxysqlConfigureScript = `set -e
CNF=/etc/proxysql-admin.cnf
[ -f "$CNF" ] || { echo "proxysql-admin.cnf not found"; exit 1; }
setk() {
  k="$1"; v="$2"
  if grep -qE "^[[:space:]]*(export[[:space:]]+)?$k=" "$CNF"; then
    sed -i -E "s|^[[:space:]]*(export[[:space:]]+)?$k=.*|export $k=\"$v\"|" "$CNF"
  else
    echo "export $k=\"$v\"" >> "$CNF"
  fi
}
setk PROXYSQL_USERNAME "$PSQL_ADMIN_USER"
setk PROXYSQL_PASSWORD "$PSQL_ADMIN_PW"
setk PROXYSQL_HOSTNAME "localhost"
setk PROXYSQL_PORT "6032"
setk CLUSTER_USERNAME "$CLUSTER_USER"
setk CLUSTER_PASSWORD "$CLUSTER_PW"
setk CLUSTER_HOSTNAME "$CLUSTER_HOST"
setk CLUSTER_PORT "3306"
setk MONITOR_USERNAME "$MON_USER"
setk MONITOR_PASSWORD "$MON_PW"
setk CLUSTER_APP_USERNAME "$APP_USER"
setk CLUSTER_APP_PASSWORD "$APP_PW"
setk MODE "$MODE"
systemctl enable --now proxysql >/dev/null 2>&1 || systemctl restart proxysql
# Wait for the proxysql admin interface, then enable the configuration.
# --use-existing-monitor-password keeps proxysql-admin non-interactive (it would
# otherwise prompt "enter a new password [y/n]?" since the monitor user exists).
ok=0
for i in $(seq 1 20); do
  if proxysql-admin --enable --use-existing-monitor-password >/tmp/psqladm.log 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || { echo "proxysql-admin --enable failed:"; tail -25 /tmp/psqladm.log 2>/dev/null; exit 1; }`

// proxysqlMySQLConfigureScript wires ProxySQL to a MySQL replication backend
// manually over the 6032 admin interface (proxysql-admin is PXC-only). It defines
// the writer(10)/reader(20) replication hostgroups, lists every backend in HG10
// (ProxySQL's monitor moves read_only secondaries to HG20), registers the app user
// (default HG10), points the monitor at the backend monitor user, and adds query
// rules to route plain SELECTs to the readers. SERVERS is comma-separated FQDNs.
const proxysqlMySQLConfigureScript = `set -e
A="mysql -u$ADMIN_USER -p$ADMIN_PW -h127.0.0.1 -P6032 --protocol=tcp"
$A -e "UPDATE global_variables SET variable_value='$MON_USER' WHERE variable_name='mysql-monitor_username';
UPDATE global_variables SET variable_value='$MON_PW' WHERE variable_name='mysql-monitor_password';
LOAD MYSQL VARIABLES TO RUNTIME; SAVE MYSQL VARIABLES TO DISK;"
$A -e "DELETE FROM mysql_replication_hostgroups;
INSERT INTO mysql_replication_hostgroups (writer_hostgroup,reader_hostgroup,comment) VALUES (10,20,'mysqlrepl');"
SQL="DELETE FROM mysql_servers;"
for h in $(echo "$SERVERS" | tr ',' ' '); do SQL="$SQL INSERT INTO mysql_servers (hostgroup_id,hostname,port) VALUES (10,'$h',3306);"; done
$A -e "$SQL LOAD MYSQL SERVERS TO RUNTIME; SAVE MYSQL SERVERS TO DISK;"
$A -e "DELETE FROM mysql_users WHERE username='$APP_USER';
INSERT INTO mysql_users (username,password,default_hostgroup,active) VALUES ('$APP_USER','$APP_PW',10,1);
LOAD MYSQL USERS TO RUNTIME; SAVE MYSQL USERS TO DISK;"
# Mode: "primary" → no read split (everything uses default_hostgroup 10 = primary);
# "rwsplit" → route reads (plain SELECT) to the reader hostgroup 20.
if [ "$MODE" = "primary" ]; then
  $A -e "DELETE FROM mysql_query_rules; LOAD MYSQL QUERY RULES TO RUNTIME; SAVE MYSQL QUERY RULES TO DISK;"
else
  $A -e "DELETE FROM mysql_query_rules;
INSERT INTO mysql_query_rules (rule_id,active,match_digest,destination_hostgroup,apply) VALUES (10,1,'^SELECT.*FOR UPDATE',10,1),(20,1,'^SELECT',20,1);
LOAD MYSQL QUERY RULES TO RUNTIME; SAVE MYSQL QUERY RULES TO DISK;"
fi`

const proxysqlPMMRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove proxysql "$NODE" >/dev/null 2>&1 || true
pmm-admin add proxysql --username="$ADMIN_USER" --password="$ADMIN_PW" --host=127.0.0.1 --port=6032 "$NODE"`

const proxysqlPMMDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove proxysql "$NODE" >/dev/null 2>&1 || true
pmm-admin add proxysql --username="$ADMIN_USER" --password="$ADMIN_PW" --host=127.0.0.1 --port=6032 "$NODE"`
