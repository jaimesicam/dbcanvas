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

// MariaDB node types: a standalone server ("mariadb"), an async/semi-sync
// replication frame ("mariadbrepl"), and a Galera cluster frame ("mariadbgalera").
// They are the MariaDB counterparts of the Percona Server, MySQL-replication and
// PXC types, and deliberately mirror their shape — same secrets, same progress
// reporting, same PMM/TLS/proxy hooks — so the three MySQL-family flavours behave
// alike in the designer.
//
// What is NOT shared is the replication vocabulary, which is where MariaDB diverges
// most sharply from Percona Server / PXC:
//
//   - GTIDs are "<domain>-<server>-<sequence>" (e.g. 7-1-3), not "<uuid>:<n>".
//   - A replica is pointed at a source with CHANGE MASTER TO … MASTER_USE_GTID =
//     slave_pos. There is no SOURCE_AUTO_POSITION and no gtid_mode/
//     enforce_gtid_consistency pair; the equivalent knobs are gtid_domain_id and
//     gtid_strict_mode.
//   - The classic keywords are the current ones: START SLAVE / SHOW SLAVE STATUS,
//     RESET MASTER. MariaDB never renamed them, so unlike Percona Server 8.4 there
//     is no per-major keyword split.
//   - There is no SET PERSIST. Settings that must survive a restart are written to
//     a config drop-in instead (see mariadbReadOnlyDropIn).
//
// Packages come from mariadb.org, which serves a separate repository per major
// series (like PGDG, unlike percona-release). See mariadbRepoFile.

var mariadbPorts = []int{3306}

// Galera's three fixed ports, matching the PXC node's choices so a stack can mix
// the two without a port surprise: group communication, IST, and SST.
const (
	mariadbGaleraPort = 4567
	mariadbISTPort    = 4568
	mariadbSSTPort    = 4444
)

// mariadbConfig is the non-secret profile shown for a deployed MariaDB node. One
// struct serves all three types; the fields that do not apply stay zero (a
// standalone has no Cluster, a replication member no ClusterAddress).
type mariadbConfig struct {
	Cluster        string `json:"cluster"`
	Image          string `json:"image"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Role           string `json:"role"` // standalone | primary | secondary | seed | member
	Hostname       string `json:"hostname"`
	FQDN           string `json:"fqdn"`
	ServerID       int    `json:"serverId"`
	MariaDBMajor   string `json:"mariadbMajor"`
	MariaDBVersion string `json:"mariadbVersion"`
	ReplMode       string `json:"replMode"` // async | semisync (replication frame only)
	GTID           bool   `json:"gtid"`
	GTIDDomainID   int    `json:"gtidDomainId"`
	ReadOnly       bool   `json:"readOnly"`
	SourceHost     string `json:"sourceHost"`     // primary FQDN (secondaries)
	ClusterAddress string `json:"clusterAddress"` // gcomm:// (Galera only)
	SSTMethod      string `json:"sstMethod"`      // mariabackup | rsync (Galera only)
	GenerateCert   bool   `json:"generateCert"`
	UseProxy       bool   `json:"useProxy"`
	MonitoredBy    string `json:"monitoredBy"`
	OrchestratedBy string `json:"orchestratedBy"`
	Ports          []int  `json:"ports"`
	ExportPort     int    `json:"exportPort"`
}

// mariadbUnit is the systemd unit name. Unlike Percona Server (mysqld on RHEL,
// mysql on Debian) MariaDB installs the same "mariadb" unit on both families; the
// mysql/mysqld names exist only as aliases.
func mariadbUnit() string { return "mariadb" }

// mariadbMajorOf normalizes a MariaDB major series. The default tracks the newest
// long-term series that every image in the matrix can install.
func mariadbMajorOf(major string) string {
	switch major {
	case "10.6", "10.11", "11.4", "11.8":
		return major
	}
	return "11.4"
}

// mariadbServerID derives a stable, unique server-id from a node name.
func mariadbServerID(name string) int { return serverIDFor(name) }

// mariadbReplMode normalizes the replication frame's mode.
func mariadbReplMode(m string) string {
	if m == "semisync" {
		return "semisync"
	}
	return "async"
}

// mariadbGTIDDomain derives the cluster's gtid_domain_id from its label.
//
// The domain identifies the *write source*, so every member of one topology must
// share it (a replica that is promoted keeps producing GTIDs in the same domain),
// while two independent clusters in one stack must NOT collide — otherwise a later
// cross-cluster replication link would see two different servers minting sequence
// numbers in the same domain and refuse to order them. Deriving it from the label
// gives both properties without another form field, and reproduces across deploys.
// Domain 0 is avoided because it is the server default: a node that somehow missed
// this setting would otherwise look like a legitimate member.
func mariadbGTIDDomain(label string) int { return int(fnv32(label)%64000) + 1 }

// mariadbServerPackages lists the packages that make up a MariaDB install.
// MariaDB-backup supplies mariabackup, which is both the Galera SST method and the
// physical-backup tool, so it is installed everywhere for parity with the Percona
// nodes (which always get XtraBackup).
func mariadbServerPackages(os string, galera bool) []string {
	var pkgs []string
	if isDebianOS(os) {
		pkgs = []string{"mariadb-server", "mariadb-client", "mariadb-backup"}
	} else {
		// The EL packages are capitalised. The lowercase mariadb-server on EL is the
		// distro's own, older AppStream build — a different package that these repos
		// do not provide, which is why the install script disables the mariadb module.
		pkgs = []string{"MariaDB-server", "MariaDB-client", "MariaDB-backup"}
	}
	if galera {
		pkgs = append(pkgs, "galera-4")
	}
	return pkgs
}

// mariadbGaleraProvider is the libgalera path, which differs by packaging family.
func mariadbGaleraProvider(os string) string {
	if isDebianOS(os) {
		return "/usr/lib/galera/libgalera_smm.so"
	}
	return "/usr/lib64/galera-4/libgalera_smm.so"
}

// mariadbCnfDir returns the directory + filename for dbcanvas's config drop-in.
//
// Unlike the Percona nodes, which own /etc/my.cnf outright, MariaDB's packages ship
// a populated conf.d that /etc/my.cnf includes — overwriting my.cnf would drop it.
// A drop-in is used instead, named so it sorts last: files are read in directory
// order, so "zz-" guarantees these settings win over the vendor's server.cnf.
func mariadbCnfDir(os string) (string, string) {
	if isDebianOS(os) {
		return "/etc/mysql/mariadb.conf.d", "zz-dbcanvas.cnf"
	}
	return "/etc/my.cnf.d", "zz-dbcanvas.cnf"
}

func mariadbCnfPath(os string) string {
	dir, base := mariadbCnfDir(os)
	return dir + "/" + base
}

// mariadbLogError is the error-log path written into the config. MariaDB defaults
// to logging to journald/stderr, which leaves nothing for the baseline scripts to
// quote back when a start fails, so it is always set explicitly.
func mariadbLogError(os string) string {
	if isDebianOS(os) {
		return "/var/log/mysql/error.log"
	}
	return "/var/log/mysqld.log"
}

// mariadbOptQuote quotes an option-file value.
//
// Not cosmetic: MySQL/MariaDB option files treat '#' as the start of a comment
// *mid-line*, so an unquoted wsrep_sst_auth=user:pa#ss silently becomes
// "user:pa" and SST then fails with a bare "Access denied", nowhere near the
// cause. dbcanvas passwords come from .env and routinely contain '#'.
func mariadbOptQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// mariadbBaseCnf renders the settings shared by every MariaDB type.
func mariadbBaseCnf(b *strings.Builder, os, host string, serverID int) {
	fmt.Fprintf(b, "[client]\nsocket=/var/lib/mysql/mysql.sock\n\n[mysqld]\n")
	fmt.Fprintf(b, "server-id=%d\n", serverID)
	fmt.Fprintf(b, "datadir=/var/lib/mysql\nsocket=/var/lib/mysql/mysql.sock\n")
	fmt.Fprintf(b, "log-error=%s\n", mariadbLogError(os))
	// Debian's packaged config binds to 127.0.0.1, which would block both the
	// published host port and any cross-node client.
	fmt.Fprintf(b, "bind-address=0.0.0.0\n")
	fmt.Fprintf(b, "slow_query_log=ON\nslow_query_log_file=/var/lib/mysql/slow.log\nlong_query_time=2\n")
	_ = host
}

// mariadbReplCnf renders the config for a standalone or replication-frame node.
func mariadbReplCnf(os, host string, serverID, gtidDomain int, gtid bool) string {
	var b strings.Builder
	mariadbBaseCnf(&b, os, host, serverID)
	fmt.Fprintf(&b, "log_bin=binlog\nbinlog_format=ROW\n")
	// log_slave_updates lets a replica act as a source in turn (chained replication,
	// and the promotion case), which is the behaviour the Percona nodes also assume.
	fmt.Fprintf(&b, "log_slave_updates=ON\n")
	if gtid {
		// gtid_strict_mode makes the server refuse out-of-order GTIDs rather than
		// silently diverge — the closest MariaDB equivalent of MySQL's
		// enforce_gtid_consistency, and worth having on in a teaching stack.
		fmt.Fprintf(&b, "gtid_domain_id=%d\ngtid_strict_mode=ON\n", gtidDomain)
	}
	return b.String()
}

// mariadbGaleraCnf renders the config for a Galera member.
func mariadbGaleraCnf(frame designFrame, host, domain, clusterAddr string, sec pxcSecrets) string {
	var b strings.Builder
	mariadbBaseCnf(&b, frame.OS, host, mariadbServerID(host))
	fqdn := fqdnOf(host, domain)
	fmt.Fprintf(&b, "binlog_format=ROW\ndefault_storage_engine=InnoDB\n")
	// Galera requires these two: it cannot certify a transaction that took an
	// auto-increment gap lock, and it needs full row images to apply remotely.
	fmt.Fprintf(&b, "innodb_autoinc_lock_mode=2\n")
	fmt.Fprintf(&b, "wsrep_on=ON\nwsrep_provider=%s\n", mariadbGaleraProvider(frame.OS))
	fmt.Fprintf(&b, "wsrep_cluster_name=%s\n", mariadbOptQuote(sanitizeName(frame.Label)))
	fmt.Fprintf(&b, "wsrep_cluster_address=%s\n", mariadbOptQuote(clusterAddr))
	fmt.Fprintf(&b, "wsrep_node_name=%s\n", mariadbOptQuote(host))
	fmt.Fprintf(&b, "wsrep_node_address=%s\n", mariadbOptQuote(fqdn))
	fmt.Fprintf(&b, "wsrep_sst_method=mariabackup\n")
	// Quoted — the password may contain '#'. See mariadbOptQuote.
	fmt.Fprintf(&b, "wsrep_sst_auth=%s\n", mariadbOptQuote(sec.ClusterUser+":"+sec.ClusterPassword))
	fmt.Fprintf(&b, "wsrep_slave_threads=4\n")
	// log_slave_updates is required for a Galera node that also serves as an async
	// replication source, and harmless otherwise.
	fmt.Fprintf(&b, "log_bin=binlog\nlog_slave_updates=ON\n")
	return b.String()
}

// mariadbGaleraClusterAddr builds the gcomm:// address listing every member.
func mariadbGaleraClusterAddr(hosts []string, domain string) string {
	var addrs []string
	for _, h := range hosts {
		addrs = append(addrs, fqdnOf(h, domain))
	}
	return "gcomm://" + strings.Join(addrs, ",")
}

// ------------------------------------------------------------------ provisioners

// provisionMariaDB provisions a standalone MariaDB node — a single read/write
// server with no replication. Like provisionPerconaServer it reuses the frame path
// by synthesising a frame from the node's own settings.
func (a *App) provisionMariaDB(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	frame := designFrame{
		Type: "mariadbrepl", Label: n.Label,
		OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch,
		MariaDBMajor: n.MariaDBMajor, MariaDBVersion: n.MariaDBVersion, GTID: n.GTID,
		UseProxy: n.UseProxy, GenerateCert: n.GenerateCert,
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
	cfg := mariadbConfig{
		Image: image, OS: n.OS, Arch: archOr(n.Arch), Role: "standalone",
		Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mariadbServerID(host),
		MariaDBMajor: mariadbMajorOf(n.MariaDBMajor), MariaDBVersion: n.MariaDBVersion,
		GTID: n.GTID, GTIDDomainID: mariadbGTIDDomain(n.Label),
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports: mariadbPorts,
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
		cnf := mariadbReplCnf(n.OS, host, mariadbServerID(host), mariadbGTIDDomain(n.Label), n.GTID)
		if err := a.mariadbPrepareNode(ctx, st, frame, n, host, image, intranetIP, domain, cnf, false); err != nil {
			return // recorded its own error
		}
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Configuring MariaDB", 60)
		if err := a.mariadbSetupBaseline(ctx, st, frame, n, "standalone", sec, pr); err != nil {
			return
		}
		a.engCtx(ctx).CopyFile(ctx, a.containerOf(st.ID, n.ID), "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
		if n.GenerateCert {
			pr.phase("Issuing certificate", 90)
			if err := a.pxcApplyCert(ctx, a.containerOf(st.ID, n.ID), intranetID, fqdnOf(host, domain), mariadbUnit(), n.OS, n.CertTTLValue, n.CertTTLUnit, pr.logln, false); err != nil {
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
		log.Printf("stack %d mariadb %s: provisioned", st.ID, n.ID)
	}()
}

// provisionMariaDBFrame brings up a MariaDB replication topology: install every
// member, baseline them all in parallel, then attach each secondary to the primary
// with MASTER_USE_GTID. The shape follows provisionMySQLFrame exactly, including
// the stack-wide baseline barrier, so a ProxySQL or Orchestrator fronting either
// flavour sees the same sequence.
func (a *App) provisionMariaDBFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var primary designNode
	var secondaries []designNode
	havePrimary := false
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "mariadbrepl" {
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
	gtidDomain := mariadbGTIDDomain(frame.Label)

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
		cfg := mariadbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: role, Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mariadbServerID(host),
			MariaDBMajor: mariadbMajorOf(frame.MariaDBMajor), MariaDBVersion: frame.MariaDBVersion,
			ReplMode: mariadbReplMode(frame.ReplMode), GTID: frame.GTID, GTIDDomainID: gtidDomain,
			ReadOnly: role == "secondary", GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, OrchestratedBy: orchestratedBy, Ports: mariadbPorts,
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

		// ---- Phase 1 (parallel): container + install + config per member ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				host := hosts[n.ID]
				cnf := mariadbReplCnf(frame.OS, host, mariadbServerID(host), gtidDomain, frame.GTID)
				if err := a.mariadbPrepareNode(ctx, st, frame, n, host, image, intranetIP, domain, cnf, false); err != nil {
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

		// ---- Phase 2 (parallel): baseline every member (users + GTID reset) ----
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
				if err := a.mariadbSetupBaseline(ctx, st, frame, n, role, sec, pr); err != nil {
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

		// ---- Phase 3: attach each secondary ----
		for _, n := range secondaries {
			pr := progs[n.ID]
			pr.phase("Attaching replica", 72)
			if err := a.mariadbAttachReplica(ctx, st, frame, n, primaryFQDN, sec, pr); err != nil {
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

		// ---- Phase 4: TLS + PMM + finalize ----
		for _, n := range members {
			pr := progs[n.ID]
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 90)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mariadbUnit(), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
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
		log.Printf("stack %d mariadb repl %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
	}()
}

// provisionMariaDBGaleraFrame brings up a MariaDB Galera cluster: install every
// member, bootstrap the seed, then start the joiners one at a time so each SSTs
// from a settled donor.
//
// Joiners are deliberately sequential. Galera can service only one state transfer
// per donor at a time; starting three at once puts two into a retry loop that is
// slow at best and, on a small container, wedges the donor in Donor/Desynced.
func (a *App) provisionMariaDBGaleraFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "mariadbgalera" {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return
	}
	// Stable order, seed first: the node marked "primary" seeds, else the first by
	// label. Positional so a redeploy bootstraps the same member.
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	seedIdx := 0
	for i, n := range members {
		if n.Role == "primary" || n.Role == "seed" {
			seedIdx = i
			break
		}
	}
	seed := members[seedIdx]
	joiners := make([]designNode, 0, len(members)-1)
	for i, n := range members {
		if i != seedIdx {
			joiners = append(joiners, n)
		}
	}

	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	var memberHosts []string
	for _, n := range members {
		memberHosts = append(memberHosts, hosts[n.ID])
	}
	clusterAddr := mariadbGaleraClusterAddr(memberHosts, domain)

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
		role := "member"
		if n.ID == seed.ID {
			role = "seed"
		}
		cfg := mariadbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: role, Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mariadbServerID(host),
			MariaDBMajor: mariadbMajorOf(frame.MariaDBMajor), MariaDBVersion: frame.MariaDBVersion,
			ClusterAddress: clusterAddr, SSTMethod: "mariabackup",
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, Ports: mariadbPorts,
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

		// ---- Phase 1 (parallel): container + install + config ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				cnf := mariadbGaleraCnf(frame, hosts[n.ID], domain, clusterAddr, sec)
				if err := a.mariadbPrepareNode(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain, cnf, true); err != nil {
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

		// ---- Phase 2: bootstrap the seed ----
		pr := progs[seed.ID]
		pr.phase("Bootstrapping Galera cluster", 55)
		if err := a.mariadbGaleraBootstrap(ctx, st, frame, seed, sec, pr); err != nil {
			for _, n := range joiners {
				progs[n.ID].fail("seed %s did not bootstrap", seed.Label)
			}
			return
		}

		// ---- Phase 3: joiners, one at a time (see the function comment) ----
		for _, n := range joiners {
			jp := progs[n.ID]
			jp.phase("Joining Galera cluster (SST)", 70)
			if err := a.mariadbGaleraJoin(ctx, st, frame, n, sec, jp); err != nil {
				return
			}
		}

		// ---- Phase 4: TLS + PMM + finalize ----
		for _, n := range members {
			p := progs[n.ID]
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
			if frame.GenerateCert {
				p.phase("Issuing certificate", 90)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mariadbUnit(), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, p.logln, false); err != nil {
					p.fail("%v", err)
					return
				}
			}
			if frame.PMMNodeID != "" {
				p.phase("Registering with PMM", 95)
				pmmUser, pmmPass := "", ""
				if _, u, pw, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, pw
				}
				a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label)) // best-effort
			}
			p.phase("Running", 100)
			p.p.Message = "provisioned"
			p.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d mariadb galera %s: provisioned (%d node(s))", st.ID, frame.Label, len(members))
	}()
}

// ------------------------------------------------------------------ steps

// mariadbPrepareNode creates the container, installs MariaDB (+ galera-4 and
// pmm-client where wanted) and writes the config drop-in. The rendered config is
// passed in because the three types differ only in that file.
func (a *App) mariadbPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, intranetIP, domain, cnf string, galera bool) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	if host == "" {
		host = sanitizeName(n.Label)
	}
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
	applyVMSize(&spec, n.CPUs, n.MemoryGB)
	if n.ExportEnabled {
		spec.PublishMap = []PortMap{{ContainerPort: 3306, HostPort: n.ExportHostPort}}
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	var cfg mariadbConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if n.ExportEnabled {
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, "3306/tcp"); e == nil {
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
	if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
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

	pr.phase("Installing MariaDB", 40)
	major := mariadbMajorOf(frame.MariaDBMajor)
	pkgs := strings.Join(mariadbServerPackages(frame.OS, galera), " ")
	instScript := mariadbInstallRHEL
	if debian {
		instScript = mariadbInstallDebian
	}
	env := []string{"MAJOR=" + major, "PKGS=" + pkgs, "VER=" + frame.MariaDBVersion}
	if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
		return pr.fail("install MariaDB %s: %v", major, err)
	}
	pr.logln("MariaDB " + major + " installed (" + pkgs + ")")

	if frame.PMMNodeID != "" {
		pmmScript := pxcInstallPMMClientRHEL
		if debian {
			pmmScript = pxcInstallPMMClientDebian
		}
		if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
			return pr.fail("install pmm-client: %v", err)
		}
		pr.logln("pmm-client installed")
	}
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	dir, base := mariadbCnfDir(frame.OS)
	if err := a.engCtx(ctx).CopyFile(ctx, id, dir, base, 0o644, []byte(cnf)); err != nil {
		return pr.fail("write %s: %v", mariadbCnfPath(frame.OS), err)
	}
	return nil
}

// mariadbSetupBaseline brings one server to the pre-replication baseline: init the
// datadir if needed, start, set the root password, create the shared users, then
// clear binlog + GTID state so every member starts from the same empty position.
//
// As in the Percona path the users are created on EVERY member rather than
// replicated, because the RESET below purges them from the binlog.
func (a *App) mariadbSetupBaseline(ctx context.Context, st Stack, frame designFrame, n designNode, role string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	env := []string{
		"UNIT=" + mariadbUnit(), "LOGERR=" + mariadbLogError(frame.OS),
		"ROOT_PW=" + sec.RootPassword,
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"CC_USER=" + sec.ClusterCheckUser, "CC_PW=" + sec.ClusterCheckPassword,
		"ORCH_USER=" + sec.OrchestratorUser, "ORCH_PW=" + sec.OrchestratorPassword,
	}
	if err := a.runStep(ctx, dep.ContainerID, mariadbBaselineScript, env, pr.logln); err != nil {
		return pr.fail("configure %s baseline: %v", role, err)
	}
	pr.logln(role + " baseline ready; users created; GTID reset")
	if mariadbReplMode(frame.ReplMode) == "semisync" {
		// MariaDB 10.3+ has semi-sync built in — no INSTALL PLUGIN, just the
		// enable variable, and the names differ from MySQL's plugin variables.
		v := "rpl_semi_sync_master_enabled"
		if role != "primary" && role != "standalone" {
			v = "rpl_semi_sync_slave_enabled"
		}
		if err := a.runStep(ctx, dep.ContainerID, mariadbSemisyncScript, []string{"ROOT_PW=" + sec.RootPassword, "ENABLEVAR=" + v, "CNF=" + mariadbCnfPath(frame.OS), "UNIT=" + mariadbUnit()}, pr.logln); err != nil {
			return pr.fail("enable semi-sync %s: %v", role, err)
		}
		pr.logln("semi-sync " + role + " enabled")
	}
	return nil
}

// mariadbAttachReplica attaches an already-baselined secondary to the primary and
// makes it read-only. GTID frames use MASTER_USE_GTID=slave_pos (MariaDB's
// auto-positioning); non-GTID frames fall back to a file/position read from the
// primary.
func (a *App) mariadbAttachReplica(ctx context.Context, st Stack, frame designFrame, n designNode, primaryFQDN string, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	env := []string{
		"ROOT_PW=" + sec.RootPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"SOURCE_HOST=" + primaryFQDN,
		"CNFDIR=" + func() string { d, _ := mariadbCnfDir(frame.OS); return d }(),
	}
	method := "GTID (slave_pos)"
	if frame.GTID {
		env = append(env, "AUTO=1")
	} else {
		env = append(env, "AUTO=0")
		method = "file/position"
	}
	if err := a.runStep(ctx, dep.ContainerID, mariadbAttachScript, env, pr.logln); err != nil {
		return pr.fail("attach replica: %v", err)
	}
	pr.logln("replica attached (" + method + "); read_only enabled")
	return nil
}

// mariadbGaleraBootstrap initializes the datadir, starts the seed with
// galera_new_cluster, and creates the users — including the SST account the
// joiners' mariabackup will authenticate as.
func (a *App) mariadbGaleraBootstrap(ctx context.Context, st Stack, frame designFrame, n designNode, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	env := []string{
		"UNIT=" + mariadbUnit(), "LOGERR=" + mariadbLogError(frame.OS),
		"ROOT_PW=" + sec.RootPassword,
		"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword,
		"APP_USER=" + sec.AppUser, "APP_PW=" + sec.AppPassword,
		"REPL_USER=" + sec.ReplUser, "REPL_PW=" + sec.ReplPassword,
		"MON_USER=" + sec.MonitorUser, "MON_PW=" + sec.MonitorPassword,
		"CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword,
		"CC_USER=" + sec.ClusterCheckUser, "CC_PW=" + sec.ClusterCheckPassword,
		"ORCH_USER=" + sec.OrchestratorUser, "ORCH_PW=" + sec.OrchestratorPassword,
	}
	if err := a.runStep(ctx, dep.ContainerID, mariadbGaleraBootstrapScript, env, pr.logln); err != nil {
		return pr.fail("bootstrap Galera: %v", err)
	}
	pr.logln("Galera cluster bootstrapped; SST account " + sec.ClusterUser + " ready")
	return nil
}

// mariadbGaleraJoin starts a joiner and waits for it to reach Synced.
func (a *App) mariadbGaleraJoin(ctx context.Context, st Stack, frame designFrame, n designNode, sec pxcSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	env := []string{
		"UNIT=" + mariadbUnit(), "LOGERR=" + mariadbLogError(frame.OS),
		"ROOT_PW=" + sec.RootPassword,
	}
	if err := a.runStep(ctx, dep.ContainerID, mariadbGaleraJoinScript, env, pr.logln); err != nil {
		return pr.fail("join Galera: %v", err)
	}
	pr.logln("joined the cluster (Synced)")
	return nil
}

// ------------------------------------------------------------------ scripts

// mariadbInstallRHEL writes the per-major mariadb.org repo and installs.
//
// $VER pins the exact build when the form chose a minor; the packages share one
// version so a single pin applies to all of them. gpgcheck stays on — the key is
// fetched over HTTPS from the same mirror.
const mariadbInstallRHEL = pinInstallRHEL + `set -e
# The distro's own mariadb module would mask the upstream packages of the same name.
dnf -y -q module disable mariadb mysql >/dev/null 2>&1 || true
cat >/etc/yum.repos.d/dbcanvas-mariadb.repo <<EOF
[dbcanvas-mariadb]
name=MariaDB $MAJOR
baseurl=https://mirror.mariadb.org/yum/$MAJOR/rhel/\$releasever/\$basearch
gpgkey=https://mirror.mariadb.org/yum/RPM-GPG-KEY-MariaDB
gpgcheck=1
module_hotfixes=1
EOF
for p in $PKGS; do pin_install "$p"; done`

const mariadbInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq curl gnupg ca-certificates >/dev/null 2>&1 || true
install -d /etc/apt/keyrings
curl -fsSL https://mariadb.org/mariadb_release_signing_key.pgp -o /etc/apt/keyrings/dbcanvas-mariadb.pgp
CODE=$(. /etc/os-release; echo "$VERSION_CODENAME")
echo "deb [signed-by=/etc/apt/keyrings/dbcanvas-mariadb.pgp] https://mirror.mariadb.org/repo/$MAJOR/ubuntu $CODE main" \
  >/etc/apt/sources.list.d/dbcanvas-mariadb.list
apt-get update -qq >/dev/null
for p in $PKGS; do pin_install "$p"; done`

// mariadbDatadirInit prepares the datadir before the first start.
//
// MariaDB does NOT auto-initialize an existing-but-empty datadir: mariadbd aborts
// with "Table 'mysql.db' doesn't exist", and under Galera that surfaces as a FATAL
// "View callback failed" which looks like a clustering problem rather than a
// missing install step. mysql/global_priv.frm is MariaDB's privilege store (the
// equivalent of MySQL's mysql.ibd) and is the marker for "already initialized" —
// checking for the mysql/ directory alone is not enough, because an interrupted
// first start leaves the directory present but empty.
const mariadbDatadirInit = `say_err() {
  echo "$1"; [ -f "$LOGERR" ] && grep -iE '\[ERROR\]' "$LOGERR" | tail -5
}
mkdir -p "$(dirname "$LOGERR")"
: >"$LOGERR" 2>/dev/null || true
chown mysql:mysql "$LOGERR" 2>/dev/null || true
if [ ! -f /var/lib/mysql/mysql/global_priv.frm ]; then
  rm -rf /var/lib/mysql/* 2>/dev/null || true
  mariadb-install-db --user=mysql --datadir=/var/lib/mysql --skip-test-db >/dev/null 2>&1 \
    || mysql_install_db --user=mysql --datadir=/var/lib/mysql >/dev/null 2>&1 \
    || { say_err "datadir initialization failed"; exit 1; }
  chown -R mysql:mysql /var/lib/mysql
fi`

// mdbRootClient defines mdb_root(), the client invocation the SQL below runs
// through.
//
// On a FIRST deploy root@localhost is unix_socket-authenticated, so `mariadb
// -uroot` connects with no password. The first baseline then gives root a password,
// which switches it to password auth — so on a REDEPLOY the same command is
// rejected with ERROR 1045 and the whole baseline dies before it starts. Probing
// once and reusing the result keeps the step re-runnable either way.
//
// Note also that while root is still on unix_socket, a WRONG -p is ignored rather
// than refused, so a mis-parameterised script looks like it succeeded. That is why
// the SQL runs as one session whose failure is fatal, not a best-effort sequence.
// The working mode is remembered after the first success. Without that, the
// readiness and wsrep polls below would each retry the losing form first and log
// an "Access denied" warning per iteration — up to 150 of them while a slow SST
// runs. Caching only on SUCCESS matters too: early in the baseline the server is
// not up yet and both forms fail, and latching a guess there would pin the wrong
// one for the rest of the run.
const mdbRootClient = `mdb_root() {
  case "${MDB_AUTH:-}" in
    socket) mariadb -uroot "$@"; return ;;
    pw)     mariadb -uroot -p"$ROOT_PW" "$@"; return ;;
  esac
  if mariadb -uroot -e "SELECT 1" >/dev/null 2>&1; then
    MDB_AUTH=socket; mariadb -uroot "$@"
  elif mariadb -uroot -p"$ROOT_PW" -e "SELECT 1" >/dev/null 2>&1; then
    MDB_AUTH=pw; mariadb -uroot -p"$ROOT_PW" "$@"
  else
    # Server not up (or credentials wrong): run the password form so the caller
    # sees the real error rather than a swallowed one.
    mariadb -uroot -p"$ROOT_PW" "$@"
  fi
}`

// mariadbRootSQL is the shared user set. MariaDB needs no expired-temp-password
// dance and ships no validate_password component, so the password is set with a
// plain ALTER USER.
const mariadbRootSQL = mdbRootClient + `
mdb_root <<SQL
ALTER USER 'root'@'localhost' IDENTIFIED BY '$ROOT_PW';
-- Drop the anonymous accounts mariadb-install-db leaves behind: they are more
-- host-specific than our '%' grants, so any connection made over localhost matches
-- them first and fails with a misleading "Access denied".
DELETE FROM mysql.global_priv WHERE User='';
FLUSH PRIVILEGES;
CREATE USER IF NOT EXISTS '$ADMIN_USER'@'%' IDENTIFIED BY '$ADMIN_PW';
ALTER USER '$ADMIN_USER'@'%' IDENTIFIED BY '$ADMIN_PW';
GRANT ALL PRIVILEGES ON *.* TO '$ADMIN_USER'@'%' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$APP_USER'@'%' IDENTIFIED BY '$APP_PW';
GRANT ALL PRIVILEGES ON *.* TO '$APP_USER'@'%';
CREATE USER IF NOT EXISTS '$REPL_USER'@'%' IDENTIFIED BY '$REPL_PW';
GRANT REPLICATION SLAVE ON *.* TO '$REPL_USER'@'%';
CREATE USER IF NOT EXISTS '$MON_USER'@'%' IDENTIFIED BY '$MON_PW' WITH MAX_USER_CONNECTIONS 10;
ALTER USER '$MON_USER'@'%' IDENTIFIED BY '$MON_PW';
GRANT SELECT, PROCESS, RELOAD, BINLOG MONITOR, SLAVE MONITOR ON *.* TO '$MON_USER'@'%';
GRANT SELECT ON performance_schema.* TO '$MON_USER'@'%';
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'localhost' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'localhost' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'localhost' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
ALTER USER '$CLUSTER_USER'@'%' IDENTIFIED BY '$CLUSTER_PW';
GRANT ALL PRIVILEGES ON *.* TO '$CLUSTER_USER'@'%' WITH GRANT OPTION;
CREATE USER IF NOT EXISTS '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
ALTER USER '$CC_USER'@'localhost' IDENTIFIED BY '$CC_PW';
GRANT PROCESS ON *.* TO '$CC_USER'@'localhost';
CREATE USER IF NOT EXISTS '$ORCH_USER'@'%' IDENTIFIED BY '$ORCH_PW';
ALTER USER '$ORCH_USER'@'%' IDENTIFIED BY '$ORCH_PW';
GRANT SUPER, PROCESS, REPLICATION SLAVE, RELOAD, BINLOG MONITOR, SLAVE MONITOR, REPLICATION MASTER ADMIN ON *.* TO '$ORCH_USER'@'%';
FLUSH PRIVILEGES;
SQL`

// mariadbBaselineScript: init, start, users, then clear GTID/binlog state.
//
// STOP SLAVE runs before the reset because SET GLOBAL gtid_slave_pos fails with
// ERROR 1198 while a slave thread is running — which only happens on a REDEPLOY of
// an already-replicating member, so it is exactly the case a first deploy will not
// catch.
const mariadbBaselineScript = `set -e
` + mariadbDatadirInit + `
` + mdbRootClient + `
systemctl is-active --quiet "$UNIT" || { systemctl reset-failed "$UNIT" 2>/dev/null || true; systemctl start "$UNIT" || { say_err "mariadbd failed to start"; exit 1; }; }
# Readiness must accept either auth mode — on a redeploy root already has a password.
for i in $(seq 1 30); do mdb_root -e "SELECT 1" >/dev/null 2>&1 && break; sleep 2; done
` + mariadbRootSQL + `
mariadb -uroot -p"$ROOT_PW" -e "STOP SLAVE;" 2>/dev/null || true
mariadb -uroot -p"$ROOT_PW" -e "RESET MASTER; SET GLOBAL gtid_slave_pos='';"
echo "gtid_current_pos after reset: '$(mariadb -uroot -p"$ROOT_PW" -N -e "SELECT @@gtid_current_pos" 2>/dev/null)'"`

// mariadbSemisyncScript enables semi-sync. MariaDB 10.3+ builds it into the server
// (no INSTALL PLUGIN) and has no SET PERSIST, so the setting is also written to the
// config drop-in to survive a restart.
const mariadbSemisyncScript = `set -e
mariadb -uroot -p"$ROOT_PW" -e "SET GLOBAL $ENABLEVAR=ON;"
grep -q "^$ENABLEVAR" "$CNF" || printf '%s=ON\n' "$ENABLEVAR" >>"$CNF"`

// mariadbAttachScript points a replica at its source and waits for both threads.
//
// MASTER_USE_GTID=slave_pos is MariaDB's auto-positioning; there is no
// SOURCE_AUTO_POSITION and no GET_SOURCE_PUBLIC_KEY handshake (MariaDB's default
// auth is mysql_native_password, not caching_sha2_password).
//
// read_only is persisted by writing a drop-in rather than SET PERSIST, which
// MariaDB does not have.
//
// Note the limit of that protection: MariaDB has no super_read_only (the variable
// does not exist), so read_only cannot stop an account holding SUPER — which every
// ALL PRIVILEGES account here does. A MariaDB secondary therefore accepts writes
// from admin/app where a MySQL or Percona one refuses them, and will silently
// diverge until a conflicting row arrives. Verified live; surfaced in the form.
const mariadbAttachScript = `set -e
mariadb -uroot -p"$ROOT_PW" -e "STOP SLAVE;" 2>/dev/null || true
if [ "$AUTO" = 1 ]; then
  POS="MASTER_USE_GTID = slave_pos"
else
  POS="MASTER_USE_GTID = no"
fi
mariadb -uroot -p"$ROOT_PW" -e "CHANGE MASTER TO MASTER_HOST='$SOURCE_HOST', MASTER_PORT=3306, MASTER_USER='$REPL_USER', MASTER_PASSWORD='$REPL_PW', $POS;"
mariadb -uroot -p"$ROOT_PW" -e "START SLAVE;"
OK=0
for i in $(seq 1 30); do
  S=$(mariadb -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS\G" 2>/dev/null)
  if echo "$S" | grep -q "Slave_IO_Running: Yes" && echo "$S" | grep -q "Slave_SQL_Running: Yes"; then OK=1; break; fi
  sleep 2
done
[ "$OK" = 1 ] || {
  S=$(mariadb -uroot -p"$ROOT_PW" -e "SHOW SLAVE STATUS\\G" 2>/dev/null)
  echo "replica threads not running:"
  echo "$S" | grep -iE 'Slave_(IO|SQL)_Running:|Using_Gtid:' | head -4
  # The reason, last: runStep keeps only the final 160 characters of the output, so
  # anything printed after this is what the user actually sees. Empty error fields
  # are dropped — reporting "Last_SQL_Error:" with nothing after it reads as healthy
  # and hides the populated Last_IO_Error above it.
  echo "$S" | grep -iE 'Last_(IO|SQL)_Error:' | grep -vE ':[[:space:]]*$' | head -2
  exit 1
}
mariadb -uroot -p"$ROOT_PW" -e "SET GLOBAL read_only=ON;"
# The [mysqld] header is required: MariaDB refuses an option file whose first line
# is a bare option ("Found option without preceding group") — and because this
# directory is read by the CLIENT too, a malformed drop-in breaks every later
# mariadb invocation on the node, not just the server's read_only setting.
printf '[mysqld]\nread_only=ON\n' >"$CNFDIR/zz-dbcanvas-readonly.cnf"`

// mariadbGaleraBootstrapScript initializes the datadir and starts the seed with
// galera_new_cluster (equivalently `systemctl start mariadb@bootstrap`), which is
// the only way to create a primary component from nothing.
//
// The SST account must exist on the donor BEFORE any joiner starts: mariabackup
// authenticates as it, and a joiner that arrives first fails with a bare "Access
// denied" that the joiner's own log attributes to a state-transfer error.
const mariadbGaleraBootstrapScript = `set -e
` + mariadbDatadirInit + `
` + mdbRootClient + `
wsrep_stat() { mdb_root -N -e "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='$1'" 2>/dev/null; }
if systemctl is-active --quiet "$UNIT"; then
  # Already running: only usable if this node is in a primary component, otherwise
  # stop it so the bootstrap below can create one. A lone member that was merely
  # restarted cannot re-form one by itself.
  [ "$(wsrep_stat WSREP_CLUSTER_STATUS)" = "Primary" ] || systemctl stop "$UNIT"
fi
if ! systemctl is-active --quiet "$UNIT"; then
  systemctl reset-failed "$UNIT" 2>/dev/null || true
  galera_new_cluster || { say_err "galera_new_cluster failed"; exit 1; }
fi
OK=0
for i in $(seq 1 45); do
  [ "$(wsrep_stat WSREP_LOCAL_STATE_COMMENT)" = "Synced" ] && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { say_err "seed did not reach Synced"; exit 1; }
` + mariadbRootSQL + `
# mariabackup authenticates as this account on the donor. BINLOG MONITOR is the
# 10.5+ name for what older releases called REPLICATION CLIENT.
mdb_root -e "GRANT RELOAD, PROCESS, LOCK TABLES, BINLOG MONITOR, REPLICA MONITOR ON *.* TO '$CLUSTER_USER'@'localhost';" 2>/dev/null || true
echo "wsrep_cluster_size: $(wsrep_stat WSREP_CLUSTER_SIZE)"`

// mariadbGaleraJoinScript starts a joiner and waits for SST to finish.
//
// The datadir is NOT pre-initialized: a joiner receives the donor's entire datadir
// via SST, and mariabackup wipes the target first. Initializing here would only add
// a system-table set that is immediately deleted.
//
// The wait is generous because SST copies the whole dataset; the unit reports
// "activating" for its duration, so unit state alone is not a readiness signal.
const mariadbGaleraJoinScript = `set -e
mkdir -p "$(dirname "$LOGERR")"; : >"$LOGERR" 2>/dev/null || true
chown mysql:mysql "$LOGERR" 2>/dev/null || true
say_err() { echo "$1"; [ -f "$LOGERR" ] && grep -iE '\[ERROR\]|WSREP_SST: \[ERROR\]' "$LOGERR" | tail -8; }
` + mdbRootClient + `
wsrep_stat() { mdb_root -N -e "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='$1'" 2>/dev/null; }
systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl restart --no-block "$UNIT" 2>/dev/null || systemctl start --no-block "$UNIT" || true
OK=0
for i in $(seq 1 150); do
  [ "$(wsrep_stat WSREP_LOCAL_STATE_COMMENT)" = "Synced" ] && { OK=1; break; }
  if [ "$(systemctl is-active "$UNIT" 2>/dev/null)" = "failed" ]; then say_err "mariadb failed while joining"; exit 1; fi
  sleep 2
done
[ "$OK" = 1 ] || { say_err "joiner did not reach Synced"; exit 1; }
echo "wsrep_cluster_size: $(wsrep_stat WSREP_CLUSTER_SIZE)"`
