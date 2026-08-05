package main

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MySQL Community node types: a standalone server ("mysqlce"), a replication frame
// ("mysqlcerepl"), and an InnoDB Cluster / Group Replication frame
// ("mysqlceinnodb"). These are Oracle's community builds from repo.mysql.com,
// alongside the existing Percona Server ("ps"/"mysql"/"innodb") types.
//
// Percona Server is a fork of MySQL, so the *server* behaves identically for
// everything dbcanvas does: same GTID model (gtid_mode + SOURCE_AUTO_POSITION),
// same 8.0-vs-8.4 keyword split, same validate_password component, same expired
// temp-password bootstrap. All of that logic is therefore reused verbatim from
// mysql.go and innodb.go rather than copied — these provisioners differ only in
// which packages get installed, and pass a frame whose PSMajor carries the
// community major so the shared steps pick the right per-series behaviour.
//
// Contrast mariadb.go, which cannot share those steps: MariaDB diverges in the
// replication vocabulary itself.
//
// Only 8.0 and 8.4 are offered. Oracle publishes 5.7 for el7 only, which is not in
// the image matrix, so a 5.7 picker would list versions that cannot be installed.

// mysqlceMajorOf normalizes a MySQL Community major series.
func mysqlceMajorOf(major string) string {
	if major == "8.0" {
		return "8.0"
	}
	return "8.4"
}

// mysqlceServerPackages lists the packages for a community install. The server and
// client are one package set on both families; the shell and router are optional and
// live in a separate "tools" repository (see mysqlceInstallRHEL).
func mysqlceServerPackages(os string, shell, router bool) []string {
	pkgs := []string{"mysql-community-server"}
	if isDebianOS(os) {
		pkgs = []string{"mysql-community-server", "mysql-community-client"}
	}
	if shell {
		pkgs = append(pkgs, "mysql-shell")
	}
	if router {
		// The RPM is mysql-router-community; Debian names it mysql-router.
		if isDebianOS(os) {
			pkgs = append(pkgs, "mysql-router")
		} else {
			pkgs = append(pkgs, "mysql-router-community")
		}
	}
	return pkgs
}

// mysqlceSyntheticFrame returns a frame that the shared Percona-path steps can
// consume: they read PSMajor to select per-series SQL (keywords, auth plugin,
// validate_password variables, RESET spelling), which is identical between Percona
// Server and MySQL Community at the same major.
func mysqlceSyntheticFrame(f designFrame) designFrame {
	f.PSMajor = mysqlceMajorOf(f.MySQLCEMajor)
	f.PSVersion = f.MySQLCEVersion
	return f
}

// ------------------------------------------------------------------ provisioners

// provisionMySQLCE provisions a standalone MySQL Community node.
func (a *App) provisionMySQLCE(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	major := mysqlceMajorOf(n.MySQLCEMajor)
	frame := designFrame{
		Type: "mysqlcerepl", Label: n.Label,
		OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch,
		MySQLCEMajor: major, MySQLCEVersion: n.MySQLCEVersion,
		PSMajor: major, PSVersion: n.MySQLCEVersion,
		GTID: n.GTID, UseProxy: n.UseProxy, GenerateCert: n.GenerateCert,
		CertTTLValue: n.CertTTLValue, CertTTLUnit: n.CertTTLUnit, PMMNodeID: n.PMMNodeID,
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
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
		Image: image, OS: n.OS, Arch: archOr(n.Arch), Role: "standalone",
		Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mysqlServerID(host),
		PSVersion: n.MySQLCEVersion, GTID: n.GTID,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports: mysqlPorts,
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		pr.phase("Waiting for Intranet to be ready", 5)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		if err := a.mysqlcePrepareNode(ctx, st, frame, n, host, image, intranetIP, domain); err != nil {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Configuring MySQL", 60)
		if err := a.mysqlSetupBaseline(ctx, st, frame, n, "standalone", major, sec, pr); err != nil {
			return
		}
		a.engCtx(ctx).CopyFile(ctx, a.containerOf(st.ID, n.ID), "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
		if n.GenerateCert {
			pr.phase("Issuing certificate", 90)
			if err := a.pxcApplyCert(ctx, a.containerOf(st.ID, n.ID), intranetID, fqdnOf(host, domain), mysqlUnit(n.OS), n.OS, n.CertTTLValue, n.CertTTLUnit, pr.logln, false); err != nil {
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
		log.Printf("stack %d mysql-community %s: provisioned", st.ID, n.ID)
	}()
}

// provisionMySQLCEFrame brings up a MySQL Community replication topology. Same
// sequence as provisionMySQLFrame — install, baseline behind the stack-wide
// barrier, then attach each secondary with SOURCE_AUTO_POSITION.
func (a *App) provisionMySQLCEFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	frame = mysqlceSyntheticFrame(frame)
	major := mysqlceMajorOf(frame.MySQLCEMajor)

	var primary designNode
	var secondaries []designNode
	havePrimary := false
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "mysqlcerepl" {
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
	orchestratedBy := ""
	if frame.OrchestratorNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.OrchestratorNodeID {
				orchestratedBy = fqdnOf(hosts[n.ID], domain)
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
			PSVersion: frame.MySQLCEVersion, ReplMode: mysqlReplMode(frame.ReplMode), GTID: frame.GTID,
			ReadOnly: role == "secondary", GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, OrchestratedBy: orchestratedBy, Ports: mysqlPorts,
		}
		if role == "secondary" {
			cfg.SourceHost = primaryFQDN
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			for _, n := range members {
				progs[n.ID].fail("%v", werr)
			}
			return
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mysqlcePrepareNode(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain); err != nil {
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

		barrier := a.deployBarrierFor(st.ID)
		if barrier != nil {
			defer func() {
				for _, n := range members {
					barrier.arrive(n.ID)
				}
			}()
		}

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
		if barrier != nil {
			for _, n := range members {
				progs[n.ID].phase("Waiting for all servers to reach baseline", 68)
			}
			barrier.wait(deployTimeout())
		}

		for _, n := range secondaries {
			pr := progs[n.ID]
			pr.phase("Attaching replica", 72)
			if err := a.mysqlAttachReplica(ctx, st, frame, n, primary.ID, primaryFQDN, sec, pr); err != nil {
				return
			}
		}

		if frame.OrchestratorNodeID != "" {
			var orchMembers []pxcMember
			for _, n := range members {
				if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && dep.ContainerID != "" {
					orchMembers = append(orchMembers, pxcMember{FQDN: fqdnOf(hosts[n.ID], domain), ContainerID: dep.ContainerID})
				}
			}
			progs[primary.ID].phase("Registering with Orchestrator", 93)
			a.registerOrchestrator(ctx, st, frame.OrchestratorNodeID, orchMembers, progs[primary.ID].logln)
		}

		for _, n := range members {
			pr := progs[n.ID]
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 90)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mysqlUnit(frame.OS), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
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
		log.Printf("stack %d mysql-community repl %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
	}()
}

// provisionMySQLCEInnoDBFrame brings up a MySQL Community InnoDB Cluster (MySQL
// Shell + Router) or a raw Group Replication group, mirroring provisionInnoDBFrame.
// The group formation, Shell orchestration and Router bootstrap are reused from
// innodb.go — they speak SQL, mysqlsh and mysqlrouter, none of which differ between
// the Percona and community builds.
func (a *App) provisionMySQLCEInnoDBFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	frame = mysqlceSyntheticFrame(frame)
	mode := innodbReplMode(frame.ReplMode)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "mysqlceinnodb" {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })

	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	groupName := genUUID()
	var seeds []string
	for _, n := range members {
		seeds = append(seeds, fqdnOf(hosts[n.ID], domain)+":"+strconv.Itoa(grCommPort))
	}
	seedList := strings.Join(seeds, ",")

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	for i, n := range members {
		host := hosts[n.ID]
		// innodbConfig, not mysqlConfig: this is the same shape the Percona InnoDB
		// node records, so the deployed node gets InnoDBManager — cluster topology,
		// group name and the Router's published RW/RO ports — rather than a
		// replication panel that knows nothing about any of them.
		cfg := innodbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			ReplMode: mode, Hostname: host, FQDN: fqdnOf(host, domain),
			ServerID: mysqlServerID(host), GroupName: groupName, Bootstrap: i == 0,
			Router: frame.MySQLRouter, GenerateCert: frame.GenerateCert,
			UseProxy: frame.UseProxy, MonitoredBy: monitoredBy, Ports: mysqlPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range members {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			for _, n := range members {
				progs[n.ID].fail("%v", werr)
			}
			return
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mysqlceInnoDBPrepareNode(ctx, st, frame, n, hosts[n.ID], image, groupName, seedList, intranetIP, domain, sec, progs[n.ID]); err != nil {
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

		if mode == "innodbcluster" {
			if err := a.innodbCreateCluster(ctx, st, frame, members, hosts, domain, sec, progs); err != nil {
				return
			}
		} else {
			if err := a.innodbFormGroup(ctx, st, members, sec, progs); err != nil {
				return
			}
		}

		for _, n := range members {
			pr := progs[n.ID]
			if frame.MySQLRouter {
				pr.phase("Configuring MySQL Router", 85)
				if err := a.innodbSetupRouter(ctx, st, frame, n, hosts, domain, mode, sec, pr); err != nil {
					return
				}
			}
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			// Record the Router's published host ports, as the Percona InnoDB frame
			// does. Nothing else fills them in at deploy time — refreshPublishedPorts
			// only runs on a start/restart action — so without this the manager shows
			// no way to reach the cluster from the host even though the ports exist.
			if frame.MySQLRouter {
				var icfg innodbConfig
				json.Unmarshal(dep.Config, &icfg)
				icfg.RWPort, icfg.ROPort = a.readInnoDBRouterPorts(ctx, dep.ContainerID, n.ExportEnabled)
				if b, e := json.Marshal(icfg); e == nil {
					a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: b, Secrets: dep.Secrets})
					dep.Config = b
				}
			}
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 90)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mysqlUnit(frame.OS), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
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
		log.Printf("stack %d mysql-community innodb %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
	}()
}

// ------------------------------------------------------------------ steps

// mysqlcePrepareNode creates the container, installs MySQL Community and writes
// /etc/my.cnf. The config itself is the Percona one (mysqlMyCnf) — the settings it
// writes are plain MySQL, and the frame's PSMajor has been set to the community
// major so the per-series branches inside it behave correctly.
func (a *App) mysqlcePrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, intranetIP, domain string) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	if host == "" {
		host = sanitizeName(n.Label)
	}
	id, err := a.mysqlceContainer(ctx, st, frame, n, host, image, intranetIP, domain, pr, nil)
	if err != nil {
		return err
	}
	if err := a.mysqlceInstall(ctx, frame, id, false, false, pr); err != nil {
		return err
	}
	cnf := mysqlMyCnf(frame, host)
	dir, base := pxcCnfDir(frame.OS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", pxcCnfPath(frame.OS), err)
	}
	if isDebianOS(frame.OS) {
		if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
			return pr.fail("include my.cnf: %v", err)
		}
	}
	return nil
}

// mysqlceInnoDBPrepareNode is the InnoDB/GR counterpart: it additionally installs
// MySQL Shell (cluster mode) and Router, writes the GR config, and runs the shared
// innodbBaseScript to initialize the datadir and create the recovery users.
func (a *App) mysqlceInnoDBPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, groupName, seedList, intranetIP, domain string, sec pxcSecrets, pr *pxcProg) error {
	if host == "" {
		host = sanitizeName(n.Label)
	}
	mode := innodbReplMode(frame.ReplMode)
	var publish []PortMap
	if n.ExportEnabled && frame.MySQLRouter {
		publish = []PortMap{
			{ContainerPort: routerRWPort, HostPort: n.ExportHostPort},
			{ContainerPort: routerROPort, HostPort: 0},
		}
	}
	id, err := a.mysqlceContainer(ctx, st, frame, n, host, image, intranetIP, domain, pr, publish)
	if err != nil {
		return err
	}
	if err := a.mysqlceInstall(ctx, frame, id, mode == "innodbcluster", frame.MySQLRouter, pr); err != nil {
		return err
	}
	cnf := innodbMyCnf(frame, host, domain, groupName, seedList, mode)
	dir, base := pxcCnfDir(frame.OS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", pxcCnfPath(frame.OS), err)
	}
	if isDebianOS(frame.OS) {
		if err := a.runStep(ctx, id, pxcDebianIncludeCnf, nil, pr.logln); err != nil {
			return pr.fail("include my.cnf: %v", err)
		}
	}
	major := mysqlceMajorOf(frame.MySQLCEMajor)
	pr.phase("Initializing MySQL", 55)
	env := []string{
		"UNIT=" + mysqlUnit(frame.OS), "LOGERR=" + pxcLogError(frame.OS),
		"RESET_CMD=" + mysqlResetCmd(major),
		"ROOT_PW=" + sec.RootPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"VPRELAX=" + validatePasswordRelax(major), "AUTH_PLUGIN=" + psAuthPlugin(major),
	}
	if err := a.runStep(ctx, id, innodbBaseScript, env, pr.logln); err != nil {
		return pr.fail("initialize MySQL: %v", err)
	}
	a.engCtx(ctx).CopyFile(ctx, id, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
	return nil
}

// mysqlceContainer is the container-creation half shared by both prepare paths.
func (a *App) mysqlceContainer(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, intranetIP, domain string, pr *pxcProg, publish []PortMap) (string, error) {
	pr.phase("Creating container", 15)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	spec := ContainerSpec{
		Name: name, Image: image, Hostname: host, Privileged: true,
		Network: networkName(st.ID), Aliases: []string{host},
		DNS: []string{intranetIP}, DNSSearch: []string{domain},
	}
	applyVMSize(&spec, n.limits())
	if publish != nil {
		spec.PublishMap = publish
	} else if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: 3306, HostPort: n.ExportHostPort}}
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return "", pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return "", pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	// Patch the stored config through a map rather than a struct. This helper is
	// shared by the replication and InnoDB paths, which record *different* config
	// shapes (mysqlConfig and innodbConfig); round-tripping through either one
	// silently drops every field the other has — which is how the InnoDB members
	// lost bootstrap, router, groupName and the Router port pair.
	cfgMap := map[string]any{}
	secJSON := []byte("{}")
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		if len(dep.Config) > 0 {
			json.Unmarshal(dep.Config, &cfgMap)
		}
		secJSON = dep.Secrets
	}
	if n.ExportEnabled && publish == nil {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, "3306/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfgMap["exportPort"] = p
			}
		}
	}
	cfgJSON, _ := json.Marshal(cfgMap)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 25)
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
		return "", pr.fail("systemd did not start: %v", err)
	}
	a.trustIntranetCA(ctx, st, id, frame.OS, pr.logln)
	a.ensureDNFIPv4(ctx, id, frame.OS, pr.logln)
	if frame.UseProxy {
		proxyScript := pkgProxyRHEL
		if isDebianOS(frame.OS) {
			proxyScript = pkgProxyDebian
		}
		if err := a.runStep(ctx, id, proxyScript, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
			return "", pr.fail("configure package proxy: %v", err)
		}
	}
	return id, nil
}

// mysqlceInstall writes the repo definitions and installs the chosen packages.
func (a *App) mysqlceInstall(ctx context.Context, frame designFrame, id string, shell, router bool, pr *pxcProg) error {
	pr.phase("Installing MySQL Community", 40)
	major := mysqlceMajorOf(frame.MySQLCEMajor)
	pkgs := strings.Join(mysqlceServerPackages(frame.OS, shell, router), " ")
	script := mysqlceInstallRHEL
	if isDebianOS(frame.OS) {
		script = mysqlceInstallDebian
	}
	env := []string{"MAJOR=" + major, "PKGS=" + pkgs, "VER=" + frame.MySQLCEVersion, "TOOLS=" + mysqlceToolsRepo(major)}
	if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
		return pr.fail("install MySQL Community %s: %v", major, err)
	}
	pr.logln("installed: " + pkgs)
	if frame.PMMNodeID != "" {
		pmmScript := pxcInstallPMMClientRHEL
		if isDebianOS(frame.OS) {
			pmmScript = pxcInstallPMMClientDebian
		}
		if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
			return pr.fail("install pmm-client: %v", err)
		}
		pr.logln("pmm-client installed")
	}
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)
	return nil
}

// mysqlceToolsRepo names the yum repository carrying mysql-shell and
// mysql-router-community for a series. 8.4 has its own tools repo; 8.0's tools live
// in the unversioned mysql-tools-community.
func mysqlceToolsRepo(major string) string {
	if major == "8.4" {
		return "mysql-tools-8.4-community"
	}
	return "mysql-tools-community"
}

// ------------------------------------------------------------------ scripts

// mysqlceInstallRHEL writes the community repo (plus the tools repo for Shell and
// Router) and installs.
//
// The signing key is RPM-GPG-KEY-mysql-2025, NOT the widely-cited -2023 file: they
// are the same key (B7B3B788A8D3785C) but the -2023 copy expired 2025-10-22, so
// metadata downloads fine and the install then fails the signature check.
//
// skip_if_unavailable keeps one missing series from aborting the whole transaction —
// Oracle publishes no 8.0 repo for EL10, for instance.
const mysqlceInstallRHEL = pinInstallRHEL + `set -e
# EL ships a default mysql module that masks Oracle's own packages.
dnf -y -q module disable mysql >/dev/null 2>&1 || true
cat >/etc/yum.repos.d/dbcanvas-mysqlce.repo <<EOF
[dbcanvas-mysqlce]
name=MySQL $MAJOR Community
baseurl=https://repo.mysql.com/yum/mysql-$MAJOR-community/el/\$releasever/\$basearch/
gpgkey=https://repo.mysql.com/RPM-GPG-KEY-mysql-2025
gpgcheck=1
skip_if_unavailable=1
module_hotfixes=1

[dbcanvas-mysqlce-tools]
name=MySQL Tools ($TOOLS)
baseurl=https://repo.mysql.com/yum/$TOOLS/el/\$releasever/\$basearch/
gpgkey=https://repo.mysql.com/RPM-GPG-KEY-mysql-2025
gpgcheck=1
skip_if_unavailable=1
module_hotfixes=1
EOF
for p in $PKGS; do pin_install "$p"; done`

// mysqlceInstallDebian is the apt counterpart. Note the component names differ from
// the yum repo paths: mysql-8.0 but mysql-8.4-lts (yum: mysql-8.4-community).
const mysqlceInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq curl gnupg ca-certificates >/dev/null 2>&1 || true
install -d /etc/apt/keyrings
curl -fsSL https://repo.mysql.com/RPM-GPG-KEY-mysql-2025 | gpg --dearmor -o /etc/apt/keyrings/dbcanvas-mysql.gpg
CODE=$(. /etc/os-release; echo "$VERSION_CODENAME")
case "$MAJOR" in
  8.4) COMP=mysql-8.4-lts ;;
  *)   COMP=mysql-$MAJOR ;;
esac
{
  echo "deb [signed-by=/etc/apt/keyrings/dbcanvas-mysql.gpg] https://repo.mysql.com/apt/ubuntu $CODE $COMP"
  echo "deb [signed-by=/etc/apt/keyrings/dbcanvas-mysql.gpg] https://repo.mysql.com/apt/ubuntu $CODE mysql-tools"
} >/etc/apt/sources.list.d/dbcanvas-mysqlce.list
apt-get update -qq >/dev/null
# The server package prompts for a root password via debconf on Debian; preseed it
# empty so the install stays non-interactive (the baseline sets the real password).
echo "mysql-community-server mysql-community-server/root-pass password" | debconf-set-selections
echo "mysql-community-server mysql-community-server/re-root-pass password" | debconf-set-selections
for p in $PKGS; do pin_install "$p"; done`
