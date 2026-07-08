package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Patroni PostgreSQL cluster frame. A Patroni cluster is a group of nodes deployed
// on the systemd OS images (built by `make images`), each co-locating three
// services installed at deploy time: PostgreSQL (Percona Distribution for
// PostgreSQL), Patroni (the HA template that runs PostgreSQL and elects a leader),
// and an etcd member (the distributed key-value store Patroni uses as its DCS).
// The etcd members form one cluster across all nodes (quorum needs an odd 3–7),
// Patroni then bootstraps PostgreSQL on whichever node wins the leader lock and
// clones the rest as streaming replicas. Optionally pgBackRest is configured
// against a SeaweedFS node (S3) for WAL archiving + the replica-clone method, and
// per-node TLS certs are signed by the Intranet CA. An HAProxy node linked to the
// frame routes writes to the leader and reads to the replicas via Patroni's REST
// health checks. Each node exposes PostgreSQL (5432), the Patroni REST API (8008),
// and etcd client/peer (2379/2380) on the stack network and can publish 5432 to
// the host.

// The ports a Patroni node uses on the stack network.
//
//	5432 — PostgreSQL client/SQL   8008 — Patroni REST API (health/topology)
//	2379 — etcd client             2380 — etcd peer (member-to-member)
const (
	patroniPGPort   = 5432
	patroniRESTPort = 8008
	etcdClientPort  = 2379
	etcdPeerPort    = 2380
)

var patroniPorts = []int{patroniPGPort, patroniRESTPort, etcdClientPort, etcdPeerPort}

// patroniConfig is the non-secret profile shown for a deployed Patroni node.
type patroniConfig struct {
	Cluster       string   `json:"cluster"`
	Image         string   `json:"image"`
	OS            string   `json:"os"` // os family (oraclelinux | ubuntu | …) — drives paths
	Hostname      string   `json:"hostname"`
	FQDN          string   `json:"fqdn"`
	PGMajor       string   `json:"pgMajor"`   // "13".."17"
	PGVersion     string   `json:"pgVersion"` // selected minor (display)
	Role          string   `json:"role"`      // leader | replica (best-effort, filled post-bootstrap)
	EtcdEndpoints []string `json:"etcdEndpoints"`
	UsePgBackRest bool     `json:"usePgBackRest"`
	BackupRepo    string   `json:"backupRepo"` // e.g. "s3://<bucket>/pgbackrest" when pgBackRest is on
	GenerateCert  bool     `json:"generateCert"`
	UseProxy      bool     `json:"useProxy"`
	MonitoredBy   string   `json:"monitoredBy"` // PMM node FQDN, if any
	Ports         []int    `json:"ports"`
	ExportPort    int      `json:"exportPort"` // published host port for 5432 (0 = none)
}

// pgSecrets holds the cluster-wide PostgreSQL credentials: the superuser
// (postgres) and the replication user Patroni uses to clone/stream replicas.
type pgSecrets struct {
	SuperUser     string `json:"superUser"`
	SuperPassword string `json:"superPassword"`
	ReplUser      string `json:"replUser"`
	ReplPassword  string `json:"replPassword"`
}

// pgFamilySecrets builds the credential set for any PostgreSQL engine (standalone
// Percona PostgreSQL, Patroni, repmgr). The superuser password comes from
// POSTGRES_PASSWORD; the internal replication user reuses the shared REPL_PASSWORD.
// Every value comes from .env (re-read on every deploy) — no node-property or
// stored-secret overrides.
func pgFamilySecrets() pgSecrets {
	return pgSecrets{
		SuperUser: "postgres", SuperPassword: envOr("POSTGRES_PASSWORD", "postgres_password"),
		ReplUser: "replicator", ReplPassword: envOr("REPL_PASSWORD", "repl_password"),
	}
}

// ppgProduct maps a PostgreSQL major series to its percona-release product
// (e.g. "16" → "ppg-16"); pgServerPackages to the OS-specific server + contrib
// package names; pgBinDir/pgDataDir to the OS-specific binary + data directories.
//
// Package naming differs by family: on EL the server is percona-postgresqlNN-server
// (no hyphen between "postgresql" and the major), while Debian follows the PGDG
// percona-postgresql-NN convention.
func ppgProduct(major string) string { return "ppg-" + ppgMajorOf(major) }

func pgServerPackages(os, major string) []string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return []string{"percona-postgresql-" + major}
	}
	return []string{"percona-postgresql" + major + "-server", "percona-postgresql" + major + "-contrib"}
}

func ppgMajorOf(major string) string {
	major = strings.TrimSpace(major)
	if major == "" {
		return "16"
	}
	return major
}

func pgBinDir(os, major string) string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return "/usr/lib/postgresql/" + major + "/bin"
	}
	return "/usr/pgsql-" + major + "/bin"
}

func pgDataDir(os, major string) string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return "/var/lib/postgresql/" + major + "/main"
	}
	return "/var/lib/pgsql/" + major + "/data"
}

// etcdConfPath is the YAML config file the etcd unit reads (Percona's EL etcd unit
// runs `etcd --config-file /etc/etcd/etcd.conf.yaml`).
func etcdConfPath(os string) string {
	if isDebianOS(os) {
		return "/etc/default/etcd"
	}
	return "/etc/etcd/etcd.conf.yaml"
}

// epelPackage maps an Oracle Linux release to its EPEL release package — needed for
// libssh2, a percona-pgbackrest dependency not carried by BaseOS/AppStream.
func epelPackage(osVersion string) string {
	major := osVersion
	if i := strings.IndexAny(osVersion, ".-"); i >= 0 {
		major = osVersion[:i]
	}
	if major == "" {
		major = "9"
	}
	return "oracle-epel-release-el" + major
}

// --- frame orchestration ---

// provisionPatroniFrame brings up an entire Patroni PostgreSQL cluster frame: it
// records each member's deployment, creates every container (in parallel),
// installs PostgreSQL + Patroni + etcd (+ pgBackRest), forms the etcd cluster,
// starts Patroni so one node bootstraps as leader and the rest clone as replicas,
// optionally runs the initial pgBackRest backup, and registers PMM.
func (a *App) provisionPatroniFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "patroni" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		return
	}

	// Cluster-wide credentials come from .env (re-read on every deploy).
	sec := pgFamilySecrets()
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)

	// etcd endpoints (client URLs) for every member — Patroni and etcdctl use these.
	var etcdEndpoints []string
	for _, n := range members {
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), etcdClientPort))
	}

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	// Resolve the pgBackRest SeaweedFS backing store (config + secret) up front; the
	// goroutine waits for it to be running before writing pgbackrest.conf.
	backupRepo := ""
	if frame.UsePgBackRest {
		backupRepo = "pgbackrest → SeaweedFS S3"
	}

	// Record every member as pending with its profile.
	for _, n := range members {
		host := hosts[n.ID]
		cfg := patroniConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS,
			Hostname: host, FQDN: fqdnOf(host, domain),
			PGMajor: ppgMajorOf(frame.PGMajor), PGVersion: frame.PGVersion,
			EtcdEndpoints: etcdEndpoints, UsePgBackRest: frame.UsePgBackRest, BackupRepo: backupRepo,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: patroniPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	go func() {
		ctx := context.Background()
		for _, n := range members {
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			a.pxcNewProg(st.ID, n.ID).phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// pgBackRest needs the SeaweedFS node up so its S3 config/secret are readable.
		var swCfg seaweedConfig
		var swSec seaweedSecrets
		if frame.UsePgBackRest {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).phase("Waiting for SeaweedFS (pgBackRest store)", 8)
			}
			c, s, werr := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, deployTimeout())
			if werr != nil {
				for _, n := range members {
					a.pxcNewProg(st.ID, n.ID).fail("%v", werr)
				}
				return
			}
			swCfg, swSec = c, s
		}

		// ---- Phase 1 (parallel): container + install + etcd/patroni config per node ----
		var wg sync.WaitGroup
		failed := make(map[string]bool)
		var mu sync.Mutex
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.patroniPrepareNode(ctx, st, frame, n, members, hosts, domain, image, etcdEndpoints, intranetID, intranetIP, sec, swCfg, swSec); err != nil {
					mu.Lock()
					failed[n.ID] = true
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if len(failed) > 0 {
			return // each failed node already recorded its error
		}

		// All containers exist — publish DNS so every FQDN resolves for etcd/Patroni.
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2: form the etcd cluster, then wait for quorum ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Starting etcd member", 60)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, dep.ContainerID, patroniEtcdStartScript, nil, pr.logln); err != nil {
				pr.fail("start etcd: %v", err)
				return
			}
		}
		if err := a.patroniWaitEtcd(ctx, st, members, deployTimeout()); err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// ---- Phase 3: start Patroni on all nodes; one wins the leader lock ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Starting Patroni", 72)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, dep.ContainerID, patroniStartScript, nil, pr.logln); err != nil {
				pr.fail("start patroni: %v", err)
				return
			}
		}
		leaderID, err := a.patroniWaitCluster(ctx, st, frame, members, deployTimeout())
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// ---- Phase 4: initial pgBackRest stanza + full backup on the leader ----
		if frame.UsePgBackRest && leaderID != "" {
			pr := a.pxcNewProg(st.ID, leaderID)
			pr.phase("Creating pgBackRest stanza + initial backup", 88)
			ldep, _ := a.store.GetDeployment(st.ID, leaderID)
			env := []string{"STANZA=" + patroniStanza(frame.Label)}
			if err := a.runStep(ctx, ldep.ContainerID, patroniBackupScript, env, pr.logln); err != nil {
				// Non-fatal: the cluster is up; surface the failure but keep running.
				pr.logln("initial pgBackRest backup failed: " + err.Error())
			} else {
				pr.logln("pgBackRest stanza created + initial full backup taken")
			}
		}

		// ---- Phase 5: PMM + finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 94)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr) // best-effort
			}
			// Record the resolved role (leader/replica) for the manager.
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			var cfg patroniConfig
			json.Unmarshal(dep.Config, &cfg)
			if n.ID == leaderID {
				cfg.Role = "leader"
			} else {
				cfg.Role = "replica"
			}
			cfgJSON, _ := json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d patroni %s: provisioned (%d node(s), leader %s)", st.ID, frame.Label, len(members), leaderID)
	}()
}

// patroniPrepareNode creates the node container, points it at the Intranet
// resolver, installs PostgreSQL + Patroni + etcd (+ pgBackRest + pmm-client),
// stages optional TLS certs, and writes the etcd / patroni / pgbackrest configs.
func (a *App) patroniPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, members []designNode, hosts map[string]string, domain, image string, etcdEndpoints []string, intranetID, intranetIP string, sec pgSecrets, swCfg seaweedConfig, swSec seaweedSecrets) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	host := hosts[n.ID]
	fqdn := fqdnOf(host, domain)
	major := ppgMajorOf(frame.PGMajor)

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
		spec.PublishMap = []PortMap{{ContainerPort: patroniPGPort, HostPort: n.ExportHostPort}}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

	// Record container id + export host port.
	var cfg patroniConfig
	if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil {
		json.Unmarshal(dep.Config, &cfg)
	}
	if n.ExportEnabled {
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", patroniPGPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
	}
	secJSON, _ := json.Marshal(sec)
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

	pr.phase("Waiting for systemd", 25)
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
		pr.logln("package egress via Intranet proxy")
	}

	pr.phase("Installing PostgreSQL + Patroni + etcd", 35)
	pkgs := append(pgServerPackages(frame.OS, major), "percona-patroni", "etcd")
	if frame.UsePgBackRest {
		pkgs = append(pkgs, "percona-pgbackrest")
	}
	instScript := patroniInstallRHEL
	if debian {
		instScript = patroniInstallDebian
	}
	env := []string{"PRODUCT=" + ppgProduct(major), "PKGS=" + strings.Join(pkgs, " ")}
	if frame.UsePgBackRest && !debian {
		env = append(env, "WITH_EPEL=1", "EPELPKG="+epelPackage(frame.OSVersion))
	}
	if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
		return pr.fail("install PostgreSQL/Patroni/etcd: %v", err)
	}
	pr.logln("PostgreSQL " + major + " + Patroni + etcd installed")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	// Install pmm-client only when the cluster is monitored by a PMM server.
	if frame.PMMNodeID != "" {
		pr.phase("Installing PMM client", 45)
		pmmScript := pxcInstallPMMClientRHEL
		if debian {
			pmmScript = pxcInstallPMMClientDebian
		}
		if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
			pr.logln("pmm-client install skipped: " + err.Error())
		} else {
			pr.logln("pmm-client installed")
		}
	}

	// Optional per-node TLS cert (signed by the Intranet CA) staged into /etc/patroni
	// before Patroni starts, so PostgreSQL can enable ssl from first boot.
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 50)
		if err := a.patroniApplyCert(ctx, id, intranetID, fqdn, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}

	// Ensure the config directories exist before any CopyFile — Docker's copy API
	// 404s on a missing destination dir, and the packages don't all ship theirs.
	if err := a.runStep(ctx, id, patroniConfigDirsScript, nil, pr.logln); err != nil {
		return pr.fail("prepare config dirs: %v", err)
	}

	// Write the etcd YAML config (every node is a member; initial-cluster lists all
	// peers). State is "existing" on redeploy (datadir already initialised).
	pr.phase("Writing etcd config", 54)
	var initialCluster []string
	for _, m := range members {
		mh := hosts[m.ID]
		initialCluster = append(initialCluster, fmt.Sprintf("%s=http://%s:%d", mh, fqdnOf(mh, domain), etcdPeerPort))
	}
	etcdConf := patroniEtcdConf(host, fqdn, frame.Label, strings.Join(initialCluster, ","))
	ecDir, ecBase := splitPath(etcdConfPath(frame.OS))
	if err := a.docker.CopyFile(ctx, id, ecDir, ecBase, 0o644, []byte(etcdConf)); err != nil {
		return pr.fail("write etcd config: %v", err)
	}

	// Write pgbackrest.conf (S3 → SeaweedFS) when enabled. Create the config + runtime
	// dirs first: the percona-pgbackrest package doesn't ship /etc/pgbackrest, so the
	// CopyFile would 404 on a missing destination directory.
	if frame.UsePgBackRest {
		pr.phase("Writing pgBackRest config", 56)
		if err := a.runStep(ctx, id, patroniPgBackRestDirsScript, nil, pr.logln); err != nil {
			return pr.fail("prepare pgbackrest dirs: %v", err)
		}
		conf := patroniPgBackRestConf(frame.Label, frame.OS, major, swCfg, swSec)
		if err := a.docker.CopyFile(ctx, id, "/etc/pgbackrest", "pgbackrest.conf", 0o644, []byte(conf)); err != nil {
			return pr.fail("write pgbackrest.conf: %v", err)
		}
	}

	// Write the Patroni config to the path the packaged unit reads by default
	// (PATRONI_CONFIG_LOCATION=/etc/patroni/postgresql.yml; ExecStart=patroni $that).
	pr.phase("Writing Patroni config", 58)
	yml := patroniYAML(frame, host, fqdn, etcdEndpoints, sec)
	if err := a.docker.CopyFile(ctx, id, "/etc/patroni", "postgresql.yml", 0o644, []byte(yml)); err != nil {
		return pr.fail("write patroni config: %v", err)
	}
	if err := a.runStep(ctx, id, patroniPrepDirsScript, []string{"DATADIR=" + pgDataDir(frame.OS, major)}, pr.logln); err != nil {
		return pr.fail("prepare data dir: %v", err)
	}
	return nil
}

// patroniApplyCert stages the Intranet CA into the node and signs a server cert +
// key into /etc/patroni (owned by postgres) with the given TTL.
func (a *App) patroniApplyCert(ctx context.Context, containerID, intranetID, fqdn string, ttlValue int, ttlUnit string, logln func(string)) error {
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.docker.PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{"FQDN=" + fqdn, "VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit}
	if err := a.runStep(ctx, containerID, patroniCertScript, env, logln); err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}
	logln("per-node certificate written to /etc/patroni (postgres-owned)")
	return nil
}

// patroniWaitEtcd polls every member's etcd endpoint health until all report
// healthy (quorum is implied once a majority answer).
func (a *App) patroniWaitEtcd(ctx context.Context, st Stack, members []designNode, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allHealthy := true
		for _, n := range members {
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil || dep.ContainerID == "" {
				allHealthy = false
				break
			}
			res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", patroniEtcdHealthScript}, nil)
			if err != nil || res.Code != 0 {
				allHealthy = false
				break
			}
		}
		if allHealthy {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("etcd cluster did not become healthy within %s", timeout)
}

// patroniWaitCluster polls Patroni until a leader is elected and every member is
// running. It returns the design node id of the leader.
func (a *App) patroniWaitCluster(ctx context.Context, st Stack, frame designFrame, members []designNode, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leaderID := ""
		running := 0
		for _, n := range members {
			dep, err := a.store.GetDeployment(st.ID, n.ID)
			if err != nil || dep.ContainerID == "" {
				continue
			}
			// /leader returns 200 only on the current leader/primary; /health returns
			// 200 when the node's PostgreSQL is up (leader or streaming replica).
			if res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil); err == nil {
				switch strings.TrimSpace(res.Stdout) {
				case "leader":
					leaderID = n.ID
					running++
				case "running":
					running++
				}
			}
		}
		if leaderID != "" && running == len(members) {
			return leaderID, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("Patroni cluster %s did not elect a leader / all replicas within %s", frame.Label, timeout)
}

// waitPatroniRunning waits for every member of a Patroni frame to be running and
// returns their FQDNs (for HAProxy backends) plus the cluster credentials.
func (a *App) waitPatroniRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) ([]string, pgSecrets, error) {
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "patroni" {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return nil, pgSecrets{}, fmt.Errorf("associated Patroni cluster %s has no nodes", frame.Label)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var sec pgSecrets
		var fqdns []string
		for _, n := range members {
			dep, err := a.store.GetDeployment(stackID, n.ID)
			if err != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return nil, pgSecrets{}, fmt.Errorf("associated Patroni cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
			fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
		}
		if allRunning {
			return fqdns, sec, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, pgSecrets{}, fmt.Errorf("associated Patroni cluster %s did not become ready within %s", frame.Label, timeout)
}

// waitSeaweedRunning waits for a SeaweedFS node to be running and returns its
// config + secret (used to write pgbackrest.conf for the S3 repo).
func (a *App) waitSeaweedRunning(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (seaweedConfig, seaweedSecrets, error) {
	if nodeID == "" {
		return seaweedConfig{}, seaweedSecrets{}, fmt.Errorf("no SeaweedFS node selected for pgBackRest")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return seaweedConfig{}, seaweedSecrets{}, fmt.Errorf("the pgBackRest SeaweedFS node failed to provision")
			}
			if dep.State == DeployRunning {
				var cfg seaweedConfig
				var sec seaweedSecrets
				json.Unmarshal(dep.Config, &cfg)
				json.Unmarshal(dep.Secrets, &sec)
				return cfg, sec, nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return seaweedConfig{}, seaweedSecrets{}, fmt.Errorf("the pgBackRest SeaweedFS node did not become ready within %s", timeout)
}

// patroniRegisterPMM registers a node's PostgreSQL with the PMM server
// (best-effort), using the superuser over the local connection.
func (a *App) patroniRegisterPMM(ctx context.Context, st Stack, n designNode, frame designFrame, doc designDoc, sec pgSecrets, pr *pxcProg) {
	pmmFQDN, pmmUser, pmmPass, ok := a.pmmServerFor(st, doc, frame.PMMNodeID)
	if !ok {
		pr.logln("PMM registration skipped: PMM node not running")
		return
	}
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	script := patroniPMMRHEL
	if isDebianOS(frame.OS) {
		script = patroniPMMDebian
	}
	env := []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
		// PMM connects as the dedicated 'pmm' role (created on the primary via local
		// peer auth); PMM_PW is that role's password.
		"PMM_PW=" + envOr("PMM_PASSWORD", "pmm_password"),
		"NODE=" + n.Label,
	}
	if _, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("registered with PMM at " + pmmFQDN)
	}
}

// patroniLeaderContainer returns the container id of the current leader of a
// Patroni frame (by querying each member's REST /leader), or "" if none.
func (a *App) patroniLeaderContainer(ctx context.Context, st Stack, frame designFrame, doc designDoc) string {
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "patroni" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		if res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", patroniRoleScript}, nil); err == nil {
			if strings.TrimSpace(res.Stdout) == "leader" {
				return dep.ContainerID
			}
		}
	}
	return ""
}

// handlePatroniBackup runs an on-demand pgBackRest full backup on the current
// leader of a Patroni frame (owner-scoped).
func (a *App) handlePatroniBackup(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	fid := r.PathValue("fid")
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	var frame designFrame
	found := false
	for _, f := range doc.Frames {
		if f.ID == fid && f.Type == "patroni" {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Patroni cluster not found")
		return
	}
	if !frame.UsePgBackRest {
		writeErr(w, http.StatusBadRequest, "pgBackRest is not enabled for this cluster")
		return
	}
	ctx := r.Context()
	leaderID := a.patroniLeaderContainer(ctx, st, frame, doc)
	if leaderID == "" {
		writeErr(w, http.StatusConflict, "no running leader found for this cluster")
		return
	}
	env := []string{"STANZA=" + patroniStanza(frame.Label)}
	if res, err := a.docker.Exec(ctx, leaderID, []string{"bash", "-c", patroniBackupNowScript}, env); err != nil {
		writeErr(w, http.StatusInternalServerError, "pgBackRest backup failed: "+err.Error())
		return
	} else if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "pgBackRest backup failed: "+lastLines(res.Stderr+res.Stdout, 200))
		return
	}
	a.notifyStack(st.ID, "backup.done", "success", "Backup completed", frame.Label+": pgBackRest backup finished.", "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// patroniStanza is the pgBackRest stanza name for a cluster (sanitized label).
func patroniStanza(label string) string {
	s := sanitizeName(strings.TrimSpace(label))
	if s == "" {
		s = "patroni"
	}
	return s
}

// splitPath splits an absolute file path into (dir, base) for CopyFile.
func splitPath(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	return p[:i], p[i+1:]
}

// --------------------------------------------------------------- config files

// patroniEtcdConf renders the etcd YAML config (the EL etcd unit runs
// `etcd --config-file <this>`). initial-cluster-state starts "new"; the start
// script flips it to "existing" on redeploy (when a member dir already exists).
func patroniEtcdConf(name, fqdn, token, initialCluster string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "data-dir: /var/lib/etcd\n")
	fmt.Fprintf(&b, "listen-peer-urls: http://0.0.0.0:%d\n", etcdPeerPort)
	fmt.Fprintf(&b, "listen-client-urls: http://0.0.0.0:%d\n", etcdClientPort)
	fmt.Fprintf(&b, "initial-advertise-peer-urls: http://%s:%d\n", fqdn, etcdPeerPort)
	fmt.Fprintf(&b, "advertise-client-urls: http://%s:%d\n", fqdn, etcdClientPort)
	fmt.Fprintf(&b, "initial-cluster: \"%s\"\n", initialCluster)
	fmt.Fprintf(&b, "initial-cluster-token: %s\n", sanitizeName(token))
	fmt.Fprintf(&b, "initial-cluster-state: new\n")
	fmt.Fprintf(&b, "enable-v2: true\n")
	return b.String()
}

// patroniYAML renders /etc/patroni/patroni.yml for one member. PostgreSQL
// parameters live under bootstrap.dcs (cluster-wide, applied at init); per-node
// connection settings + auth live under postgresql.
func patroniYAML(frame designFrame, host, fqdn string, etcdEndpoints []string, sec pgSecrets) string {
	major := ppgMajorOf(frame.PGMajor)
	stanza := patroniStanza(frame.Label)
	var b strings.Builder
	fmt.Fprintf(&b, "scope: %s\n", sanitizeName(frame.Label))
	fmt.Fprintf(&b, "namespace: /dbcanvas/\n")
	fmt.Fprintf(&b, "name: %s\n\n", host)

	fmt.Fprintf(&b, "restapi:\n")
	fmt.Fprintf(&b, "  listen: 0.0.0.0:%d\n", patroniRESTPort)
	fmt.Fprintf(&b, "  connect_address: %s:%d\n\n", fqdn, patroniRESTPort)

	fmt.Fprintf(&b, "etcd3:\n")
	fmt.Fprintf(&b, "  hosts:\n")
	for _, e := range etcdEndpoints {
		fmt.Fprintf(&b, "  - %s\n", e)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "bootstrap:\n")
	fmt.Fprintf(&b, "  dcs:\n")
	fmt.Fprintf(&b, "    ttl: 30\n")
	fmt.Fprintf(&b, "    loop_wait: 10\n")
	fmt.Fprintf(&b, "    retry_timeout: 10\n")
	fmt.Fprintf(&b, "    maximum_lag_on_failover: 1048576\n")
	fmt.Fprintf(&b, "    postgresql:\n")
	fmt.Fprintf(&b, "      use_pg_rewind: true\n")
	fmt.Fprintf(&b, "      use_slots: true\n")
	fmt.Fprintf(&b, "      parameters:\n")
	fmt.Fprintf(&b, "        max_connections: 200\n")
	fmt.Fprintf(&b, "        hot_standby: \"on\"\n")
	fmt.Fprintf(&b, "        wal_level: replica\n")
	if frame.UsePgBackRest {
		fmt.Fprintf(&b, "        archive_mode: \"on\"\n")
		fmt.Fprintf(&b, "        archive_command: \"pgbackrest --stanza=%s archive-push %%p\"\n", stanza)
	}
	fmt.Fprintf(&b, "  initdb:\n")
	fmt.Fprintf(&b, "  - encoding: UTF8\n")
	fmt.Fprintf(&b, "  - data-checksums\n")
	fmt.Fprintf(&b, "  pg_hba:\n")
	fmt.Fprintf(&b, "  - local all all trust\n")
	fmt.Fprintf(&b, "  - host all all 127.0.0.1/32 trust\n")
	fmt.Fprintf(&b, "  - host all all 0.0.0.0/0 scram-sha-256\n")
	fmt.Fprintf(&b, "  - host replication %s 0.0.0.0/0 scram-sha-256\n\n", sec.ReplUser)

	fmt.Fprintf(&b, "postgresql:\n")
	fmt.Fprintf(&b, "  listen: 0.0.0.0:%d\n", patroniPGPort)
	fmt.Fprintf(&b, "  connect_address: %s:%d\n", fqdn, patroniPGPort)
	fmt.Fprintf(&b, "  data_dir: %s\n", pgDataDir(frame.OS, major))
	fmt.Fprintf(&b, "  bin_dir: %s\n", pgBinDir(frame.OS, major))
	fmt.Fprintf(&b, "  pgpass: /tmp/pgpass\n")
	fmt.Fprintf(&b, "  authentication:\n")
	fmt.Fprintf(&b, "    superuser:\n")
	fmt.Fprintf(&b, "      username: %s\n", sec.SuperUser)
	fmt.Fprintf(&b, "      password: \"%s\"\n", yamlEscape(sec.SuperPassword))
	fmt.Fprintf(&b, "    replication:\n")
	fmt.Fprintf(&b, "      username: %s\n", sec.ReplUser)
	fmt.Fprintf(&b, "      password: \"%s\"\n", yamlEscape(sec.ReplPassword))
	if frame.GenerateCert {
		fmt.Fprintf(&b, "  parameters:\n")
		fmt.Fprintf(&b, "    ssl: \"on\"\n")
		fmt.Fprintf(&b, "    ssl_cert_file: /etc/patroni/server.crt\n")
		fmt.Fprintf(&b, "    ssl_key_file: /etc/patroni/server.key\n")
		fmt.Fprintf(&b, "    ssl_ca_file: /etc/patroni/ca.crt\n")
	}
	if frame.UsePgBackRest {
		fmt.Fprintf(&b, "  create_replica_methods:\n")
		fmt.Fprintf(&b, "  - pgbackrest\n")
		fmt.Fprintf(&b, "  - basebackup\n")
		fmt.Fprintf(&b, "  pgbackrest:\n")
		fmt.Fprintf(&b, "    command: pgbackrest --stanza=%s --delta restore\n", stanza)
		fmt.Fprintf(&b, "    keep_data: true\n")
		fmt.Fprintf(&b, "    no_params: true\n")
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "watchdog:\n")
	fmt.Fprintf(&b, "  mode: off\n\n")
	fmt.Fprintf(&b, "tags:\n")
	fmt.Fprintf(&b, "  nofailover: false\n")
	fmt.Fprintf(&b, "  noloadbalance: false\n")
	fmt.Fprintf(&b, "  clonefrom: false\n")
	fmt.Fprintf(&b, "  nosync: false\n")
	return b.String()
}

// patroniPgBackRestConf renders /etc/pgbackrest/pgbackrest.conf for the S3
// (SeaweedFS) repository plus the cluster stanza. Mirrors the keys surfaced in
// SeaweedFSManager.jsx (repo1-s3-uri-style=path over plain HTTP).
func patroniPgBackRestConf(label, os, major string, sw seaweedConfig, sec seaweedSecrets) string {
	endpoint := sw.InternalEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", sw.FQDN, seaweedS3Port)
	}
	hostPort := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	ak := sw.AccessKey
	if ak == "" {
		ak = sec.AccessKey
	}
	region := sw.Region
	if region == "" {
		region = seaweedRegion
	}
	stanza := patroniStanza(label)
	var b strings.Builder
	fmt.Fprintf(&b, "[global]\n")
	fmt.Fprintf(&b, "repo1-type=s3\n")
	fmt.Fprintf(&b, "repo1-s3-endpoint=%s\n", hostPort)
	fmt.Fprintf(&b, "repo1-s3-uri-style=path\n")
	fmt.Fprintf(&b, "repo1-s3-bucket=%s\n", sw.Bucket)
	fmt.Fprintf(&b, "repo1-s3-region=%s\n", region)
	fmt.Fprintf(&b, "repo1-s3-key=%s\n", ak)
	fmt.Fprintf(&b, "repo1-s3-key-secret=%s\n", sec.SecretKey)
	fmt.Fprintf(&b, "repo1-s3-verify-tls=n\n")
	fmt.Fprintf(&b, "repo1-path=/pgbackrest\n")
	fmt.Fprintf(&b, "start-fast=y\n")
	fmt.Fprintf(&b, "log-level-console=info\n")
	fmt.Fprintf(&b, "log-level-file=detail\n\n")
	fmt.Fprintf(&b, "[%s]\n", stanza)
	fmt.Fprintf(&b, "pg1-path=%s\n", pgDataDir(os, major))
	fmt.Fprintf(&b, "pg1-port=%d\n", patroniPGPort)
	return b.String()
}

// yamlEscape escapes a value for a double-quoted YAML scalar.
func yamlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// ------------------------------------------------------------------ scripts

const patroniInstallRHEL = `set -e
dnf -y -q install which >/dev/null 2>&1 || true
# percona-pgbackrest needs libssh2, which lives in EPEL on Oracle Linux.
if [ -n "$WITH_EPEL" ]; then
  dnf -y -q install "$EPELPKG" >/dev/null 2>&1 || dnf -y -q install epel-release >/dev/null 2>&1 || true
fi
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
dnf -y -q install $PKGS >/dev/null`

const patroniInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release setup -y "$PRODUCT" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq $PKGS >/dev/null`

// patroniConfigDirsScript ensures the etcd + Patroni config directories exist
// before their files are copied in (Docker's copy API 404s on a missing dir).
const patroniConfigDirsScript = `set -e
mkdir -p /etc/etcd /etc/patroni`

// patroniPrepDirsScript ensures the PostgreSQL data dir parent exists and is owned
// by postgres (Patroni initdb writes into DATADIR; the parent must be writable).
const patroniPrepDirsScript = `set -e
mkdir -p "$DATADIR"
chown -R postgres:postgres "$(dirname "$DATADIR")" 2>/dev/null || chown -R postgres:postgres "$DATADIR"
chmod 700 "$DATADIR" 2>/dev/null || true`

// patroniPgBackRestDirsScript creates the pgBackRest config + log/spool dirs. The
// percona-pgbackrest package ships none of these, so /etc/pgbackrest must exist
// before the config is copied in; the runtime dirs are postgres-owned (pgBackRest
// runs as the postgres user).
const patroniPgBackRestDirsScript = `set -e
mkdir -p /etc/pgbackrest /var/log/pgbackrest /var/lib/pgbackrest /var/spool/pgbackrest
chown -R postgres:postgres /var/log/pgbackrest /var/lib/pgbackrest /var/spool/pgbackrest`

// patroniEtcdStartScript prepares the data dir and starts etcd **non-blocking** on
// every node. etcd is a Type=notify unit that does not signal "ready" until the
// cluster reaches quorum, so a plain (blocking) `systemctl start` on the first node
// would hang waiting for peers that the sequential caller has not started yet — a
// deadlock. `--no-block` returns immediately on each node; the caller then polls
// every node's health (patroniWaitEtcd) until quorum forms.
//
// A (re)deploy always recreates the container, so /var/lib/etcd is normally empty;
// any leftover member/ here is a stale partial bootstrap from an aborted attempt in
// this same container, which would force etcd into the wrong join path. Clear it and
// always bootstrap fresh ("new"), matching the recreate-on-redeploy model.
const patroniEtcdStartScript = `set -e
systemctl stop etcd 2>/dev/null || true
rm -rf /var/lib/etcd/member
mkdir -p /var/lib/etcd
chown -R etcd:etcd /var/lib/etcd 2>/dev/null || true
systemctl enable etcd >/dev/null 2>&1 || true
systemctl reset-failed etcd 2>/dev/null || true
systemctl --no-block restart etcd`

// patroniEtcdHealthScript checks the local etcd endpoint is healthy (exit 0).
const patroniEtcdHealthScript = `ETCDCTL_API=3 etcdctl --endpoints=http://127.0.0.1:2379 endpoint health >/dev/null 2>&1`

// patroniStartScript enables + starts the Patroni unit (which reads
// /etc/patroni/postgresql.yml). The leader initdb's PostgreSQL; replicas clone
// (pgbackrest then basebackup).
const patroniStartScript = `set -e
systemctl enable patroni >/dev/null 2>&1 || true
systemctl reset-failed patroni 2>/dev/null || true
systemctl restart patroni
sleep 2
systemctl is-active --quiet patroni || { echo "patroni failed to start:"; journalctl -u patroni --no-pager 2>/dev/null | tail -20; exit 1; }`

// patroniRoleScript prints "leader" when this node is the Patroni leader/primary,
// "running" when its PostgreSQL is up as a replica, else nothing. Uses the REST
// API (200 on /leader = leader; 200 on /health = running).
const patroniRoleScript = `if curl -fsS -o /dev/null http://127.0.0.1:8008/leader 2>/dev/null; then echo leader;
elif curl -fsS -o /dev/null http://127.0.0.1:8008/health 2>/dev/null; then echo running; fi`

// patroniBackupScript creates the pgBackRest stanza then takes an initial full
// backup, both as the postgres user. Idempotent (stanza-create is a no-op if it
// already exists).
const patroniBackupScript = `set -e
runuser -u postgres -- pgbackrest --stanza="$STANZA" stanza-create
runuser -u postgres -- pgbackrest --stanza="$STANZA" --type=full backup`

// patroniBackupNowScript runs an on-demand full backup (stanza already created).
const patroniBackupNowScript = `set -e
runuser -u postgres -- pgbackrest --stanza="$STANZA" stanza-create >/dev/null 2>&1 || true
runuser -u postgres -- pgbackrest --stanza="$STANZA" --type=full backup`

// patroniCertScript signs a server cert + key from the staged Intranet CA into
// /etc/patroni (postgres-owned) with the given TTL.
const patroniCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
DIR=/etc/patroni
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
mkdir -p "$DIR"
cp -f "$CA" "$DIR/ca.crt"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server.key" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null
openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$DIR/server.crt" -not_after "$END" >/dev/null
chown postgres:postgres "$DIR/ca.crt" "$DIR/server.crt" "$DIR/server.key"
chmod 600 "$DIR/server.key"
chmod 644 "$DIR/ca.crt" "$DIR/server.crt"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/dbca-ca.srl`

// patroniPMM{RHEL,Debian} point an already-installed pmm-client at the PMM server
// and register this node's PostgreSQL (best-effort).
const patroniPMMRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove postgresql "$NODE" >/dev/null 2>&1 || true
# Dedicated PMM monitoring role (per the Percona PMM docs). Created on the primary
# only (a standby is read-only; the role replicates to it), as the postgres OS user
# over the local socket. SUPERUSER as the docs recommend; :'pw' expands on stdin.
if [ "$(runuser -u postgres -- psql -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "f" ]; then
  if runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='pmm'" 2>/dev/null | grep -q 1; then
    printf '%s\n' "ALTER ROLE pmm WITH LOGIN SUPERUSER PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$PMM_PW" >/dev/null 2>&1 || true
  else
    printf '%s\n' "CREATE ROLE pmm WITH LOGIN SUPERUSER PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$PMM_PW" >/dev/null 2>&1 || true
  fi
fi
pmm-admin add postgresql --username=pmm --password="$PMM_PW" --host=127.0.0.1 --port=5432 "$NODE"`

const patroniPMMDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove postgresql "$NODE" >/dev/null 2>&1 || true
# Dedicated PMM monitoring role (per the Percona PMM docs). Created on the primary
# only (a standby is read-only; the role replicates to it), as the postgres OS user
# over the local socket. SUPERUSER as the docs recommend; :'pw' expands on stdin.
if [ "$(runuser -u postgres -- psql -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)" = "f" ]; then
  if runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='pmm'" 2>/dev/null | grep -q 1; then
    printf '%s\n' "ALTER ROLE pmm WITH LOGIN SUPERUSER PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$PMM_PW" >/dev/null 2>&1 || true
  else
    printf '%s\n' "CREATE ROLE pmm WITH LOGIN SUPERUSER PASSWORD :'pw';" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$PMM_PW" >/dev/null 2>&1 || true
  fi
fi
pmm-admin add postgresql --username=pmm --password="$PMM_PW" --host=127.0.0.1 --port=5432 "$NODE"`
