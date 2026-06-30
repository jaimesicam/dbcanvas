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

// repmgr PostgreSQL cluster frame (Type=="repmgr"). A repmgr cluster is a group of
// PostgreSQL nodes (Percona Distribution for PostgreSQL) on the systemd OS images,
// using **streaming replication managed by repmgr**: one node is the primary, the
// rest are standbys cloned from it, and `repmgrd` on every node provides automatic
// failover. It mirrors the Patroni frame's options (catalog OS/version/arch,
// superuser password, PMM, Squid proxy, Intranet-CA TLS) but uses repmgr instead of
// Patroni/etcd and, for backups, **Barman cloud** (barman-cloud-backup /
// -wal-archive) pushing to a SeaweedFS S3 node instead of pgBackRest. Quorum-style
// guidance: 3–7 nodes. Each node exposes PostgreSQL on 5432 (publishable to the host).

// barmanSeaweedIssues validates the SeaweedFS node backing Barman for a repmgr frame
// (identified by `who`): it must be selected and present in the design. Unlike
// pgBackRest, Barman cloud (boto3) works over plain HTTP, so S3 TLS is **not** required.
func barmanSeaweedIssues(who, seaweedNodeID string, doc designDoc) []issue {
	if seaweedNodeID == "" {
		return []issue{{"error", who + " has Barman backups enabled but no SeaweedFS node selected"}}
	}
	for _, n := range doc.Nodes {
		if n.ID == seaweedNodeID && n.Type == "seaweedfs" {
			return nil
		}
	}
	return []issue{{"error", who + ": the selected Barman SeaweedFS node is not in the design"}}
}

// repmgrConfig is the non-secret profile shown for a deployed repmgr node.
type repmgrConfig struct {
	Cluster      string `json:"cluster"`
	Image        string `json:"image"`
	OS           string `json:"os"`
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	PGMajor      string `json:"pgMajor"`
	PGVersion    string `json:"pgVersion"`
	Role         string `json:"role"`   // primary | standby (initial; repmgr may fail over)
	NodeID       int    `json:"nodeId"` // repmgr node_id
	UseBarman    bool   `json:"useBarman"`
	BackupRepo   string `json:"backupRepo"` // e.g. "Barman → SeaweedFS S3 (bucket/prefix)" when enabled
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"`
	Ports        []int  `json:"ports"`
	ExportPort   int    `json:"exportPort"`
}

// pgHome is the postgres OS user's home directory (where barman-cloud reads AWS
// credentials from ~/.aws). EL packages use /var/lib/pgsql; Debian /var/lib/postgresql.
func pgHome(os string) string {
	if isDebianOS(os) {
		return "/var/lib/postgresql"
	}
	return "/var/lib/pgsql"
}

// repmgrAllPackages returns the PostgreSQL server + repmgr package set for the
// OS/major. repmgr is NOT in the Percona repo, so the repmgr frame installs both
// PostgreSQL and repmgr from the PGDG repo. PGDG PostgreSQL uses the same on-disk
// layout as Percona PostgreSQL (/usr/pgsql-NN/bin, /var/lib/pgsql/NN/data,
// postgresql-NN.service), so the existing pg.go path helpers still apply.
//
//   - EL:     postgresqlNN-server + postgresqlNN-contrib + repmgr_NN   (from pgdg-redhat-repo)
//   - Debian: postgresql-NN + postgresql-NN-repmgr                     (from apt.postgresql.org)
func repmgrAllPackages(os, major string) []string {
	major = ppgMajorOf(major)
	if isDebianOS(os) {
		return []string{"postgresql-" + major, "postgresql-" + major + "-repmgr"}
	}
	return []string{"postgresql" + major + "-server", "postgresql" + major + "-contrib", "repmgr_" + major}
}

// barmanServer is the Barman/cloud server (stanza) name for a cluster.
func barmanServer(label string) string {
	s := sanitizeName(strings.TrimSpace(label))
	if s == "" {
		s = "repmgr"
	}
	return s
}

// barmanS3URL / barmanEndpoint build the destination URL + endpoint for the
// barman-cloud-* commands from the SeaweedFS node's config.
func barmanS3URL(label string, sw seaweedConfig) string {
	bucket := sw.Bucket
	if bucket == "" {
		bucket = "backups"
	}
	return fmt.Sprintf("s3://%s/barman/%s", bucket, barmanServer(label))
}

func barmanEndpoint(sw seaweedConfig) string {
	if sw.InternalEndpoint != "" {
		return sw.InternalEndpoint
	}
	return fmt.Sprintf("http://%s:%d", sw.FQDN, seaweedS3Port)
}

func barmanRepoLabel(label string, sw seaweedConfig) string {
	return fmt.Sprintf("Barman → SeaweedFS S3 (%s)", strings.TrimPrefix(barmanS3URL(label, sw), "s3://"))
}

// barmanArchiveCommand is the postgresql.conf archive_command that ships WAL to the
// SeaweedFS S3 store via barman-cloud-wal-archive.
func barmanArchiveCommand(label string, sw seaweedConfig) string {
	return fmt.Sprintf("barman-cloud-wal-archive --cloud-provider aws-s3 --endpoint-url %s %s %s %%p",
		barmanEndpoint(sw), barmanS3URL(label, sw), barmanServer(label))
}

// --- frame orchestration ---

// provisionRepmgrFrame brings up a repmgr PostgreSQL cluster: it records each member,
// creates every container + installs PostgreSQL + repmgr (+ Barman cloud), initialises
// the primary, clones + registers each standby, starts repmgrd everywhere, optionally
// runs the initial Barman backup, and registers PMM.
func (a *App) provisionRepmgrFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "repmgr" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		return
	}

	// Cluster-wide credentials: superuser (postgres) + a `repmgr` superuser/replication
	// role used for both streaming replication and repmgr metadata. Reused across redeploys.
	var sec pgSecrets
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s pgSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil && s.SuperPassword != "" {
				sec = s
				break
			}
		}
	}
	if sec.SuperUser == "" {
		sec.SuperUser = "postgres"
	}
	if sec.SuperPassword == "" {
		if rp := strings.TrimSpace(frame.RootPassword); rp != "" {
			sec.SuperPassword = rp
		} else {
			sec.SuperPassword = genSecret("PgSuper!")
		}
	}
	if sec.ReplUser == "" {
		sec.ReplUser = "repmgr"
	}
	if sec.ReplPassword == "" {
		sec.ReplPassword = genSecret("PgRepmgr!")
	}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := ppgMajorOf(frame.PGMajor)

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID && n.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}
	backupRepo := ""
	if frame.UseBarman {
		backupRepo = "Barman → SeaweedFS S3"
	}

	// node_id is the 1-based member index (stable while labels are stable). Member 0
	// is the initial primary.
	for i, n := range members {
		host := hosts[n.ID]
		role := "standby"
		if i == 0 {
			role = "primary"
		}
		cfg := repmgrConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS,
			Hostname: host, FQDN: fqdnOf(host, domain),
			PGMajor: major, PGVersion: frame.PGVersion, Role: role, NodeID: i + 1,
			UseBarman: frame.UseBarman, BackupRepo: backupRepo,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: []int{patroniPGPort},
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
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if err != nil {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).fail("%v", err)
			}
			return
		}

		// Barman needs the SeaweedFS node up so its S3 config/secret are readable.
		var swCfg seaweedConfig
		var swSec seaweedSecrets
		if frame.UseBarman {
			for _, n := range members {
				a.pxcNewProg(st.ID, n.ID).phase("Waiting for SeaweedFS (Barman store)", 8)
			}
			c, s, werr := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, 10*time.Minute)
			if werr != nil {
				for _, n := range members {
					a.pxcNewProg(st.ID, n.ID).fail("%v", werr)
				}
				return
			}
			swCfg, swSec = c, s
		}

		// ---- Phase 1 (parallel): container + install + repmgr.conf (+ barman creds) ----
		var wg sync.WaitGroup
		failed := make(map[string]bool)
		var mu sync.Mutex
		for i, n := range members {
			wg.Add(1)
			go func(i int, n designNode) {
				defer wg.Done()
				if err := a.repmgrPrepareNode(ctx, st, frame, n, i+1, image, intranetIP, domain, sec, swCfg, swSec); err != nil {
					mu.Lock()
					failed[n.ID] = true
					mu.Unlock()
				}
			}(i, n)
		}
		wg.Wait()
		if len(failed) > 0 {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// ---- Phase 2: initialise + register the primary (member 0) ----
		primary := members[0]
		ppr := a.pxcNewProg(st.ID, primary.ID)
		ppr.phase("Initialising primary PostgreSQL", 55)
		if err := a.repmgrSetupPrimary(ctx, st, frame, primary, major, sec, swCfg, ppr); err != nil {
			return
		}

		// ---- Phase 3: clone + register each standby (sequential — needs the primary up) ----
		primaryFQDN := fqdnOf(hosts[primary.ID], domain)
		for _, n := range members[1:] {
			spr := a.pxcNewProg(st.ID, n.ID)
			spr.phase("Cloning standby from primary", 60)
			if err := a.repmgrSetupStandby(ctx, st, frame, n, major, primaryFQDN, sec, spr); err != nil {
				return
			}
		}

		// ---- Phase 4: start repmgrd on every node (automatic failover) ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			pr.phase("Starting repmgrd", 82)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, dep.ContainerID, repmgrdStartScript, []string{"BINDIR=" + pgBinDir(frame.OS, major)}, pr.logln); err != nil {
				pr.logln("repmgrd start failed (failover disabled): " + err.Error())
			}
		}

		// ---- Phase 5: initial Barman backup on the primary (best-effort) ----
		if frame.UseBarman {
			pr := a.pxcNewProg(st.ID, primary.ID)
			pr.phase("Taking initial Barman backup", 90)
			dep, _ := a.store.GetDeployment(st.ID, primary.ID)
			env := barmanBackupEnv(frame.Label, swCfg)
			if err := a.runStep(ctx, dep.ContainerID, barmanBackupScript, env, pr.logln); err != nil {
				pr.logln("initial Barman backup failed: " + err.Error())
			} else {
				pr.logln("initial Barman backup taken")
			}
		}

		// ---- Phase 6: PMM + finalize ----
		for _, n := range members {
			pr := a.pxcNewProg(st.ID, n.ID)
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 95)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr) // generic postgres PMM register
			}
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: dep.ContainerID, State: DeployRunning, Config: dep.Config, Secrets: secJSON})
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
		}
		a.reconcileStackDNS(ctx, st.ID)
		_ = intranetID
		log.Printf("stack %d repmgr %s: provisioned (%d node(s), primary %s)", st.ID, frame.Label, len(members), primary.Label)
	}()
}

// repmgrPrepareNode creates the container, installs PostgreSQL + repmgr (+ Barman
// cloud + pmm-client), stages optional barman AWS credentials, and writes repmgr.conf.
func (a *App) repmgrPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, nodeID int, image, intranetIP, domain string, sec pgSecrets, swCfg seaweedConfig, swSec seaweedSecrets) error {
	pr := a.pxcNewProg(st.ID, n.ID)
	host := stackHostnames(buildDoc(st))[n.ID] // resolved below if empty
	if host == "" {
		host = sanitizeName(n.Label)
	}
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

	var cfg repmgrConfig
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

	pr.phase("Installing PostgreSQL + repmgr (PGDG)", 35)
	pkgs := repmgrAllPackages(frame.OS, major)
	instScript := repmgrInstallRHEL
	env := []string{"PKGS=" + strings.Join(pkgs, " ")}
	if debian {
		instScript = repmgrInstallDebian
	} else {
		// EPEL provides dependencies (e.g. libs) for the PGDG packages on Oracle Linux.
		env = append(env, "WITH_EPEL=1", "EPELPKG="+epelPackage(frame.OSVersion))
	}
	if err := a.runStep(ctx, id, instScript, env, pr.logln); err != nil {
		return pr.fail("install PostgreSQL/repmgr: %v", err)
	}
	pr.logln("PostgreSQL " + major + " + repmgr installed (PGDG)")
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	// pmm-client is always installed so monitoring can be enabled later.
	pr.phase("Installing PMM client", 42)
	pmmScript := pxcInstallPMMClientRHEL
	if debian {
		pmmScript = pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, id, pmmScript, nil, pr.logln); err != nil {
		pr.logln("pmm-client install skipped: " + err.Error())
	} else {
		pr.logln("pmm-client installed")
	}

	// Barman cloud utilities + AWS credentials (when enabled) — installed on every
	// node so any node can archive WAL after a failover.
	if frame.UseBarman {
		pr.phase("Installing Barman cloud", 46)
		barmanScript := barmanInstallRHEL
		if debian {
			barmanScript = barmanInstallDebian
		}
		if err := a.runStep(ctx, id, barmanScript, nil, pr.logln); err != nil {
			return pr.fail("install barman-cloud: %v", err)
		}
		ak := swCfg.AccessKey
		if ak == "" {
			ak = swSec.AccessKey
		}
		region := swCfg.Region
		if region == "" {
			region = seaweedRegion
		}
		home := pgHome(frame.OS)
		// The Docker archive endpoint extracts only into an existing directory (a
		// missing path 404s), so ~/.aws must exist before the credentials/config copies.
		if err := a.runStep(ctx, id, `install -d -m 700 "$HOME/.aws"`, []string{"HOME=" + home}, pr.logln); err != nil {
			return pr.fail("create ~/.aws: %v", err)
		}
		if err := a.docker.CopyFile(ctx, id, home+"/.aws", "credentials", 0o600, []byte(barmanAWSCredentials(ak, swSec.SecretKey))); err != nil {
			return pr.fail("write AWS credentials: %v", err)
		}
		if err := a.docker.CopyFile(ctx, id, home+"/.aws", "config", 0o600, []byte(barmanAWSConfig(region))); err != nil {
			return pr.fail("write AWS config: %v", err)
		}
		if err := a.runStep(ctx, id, barmanChownScript, []string{"HOME=" + home}, pr.logln); err != nil {
			return pr.fail("fix AWS credential ownership: %v", err)
		}
		pr.logln("Barman cloud installed (S3 → SeaweedFS)")
	}

	// Write repmgr.conf (per node; node_id + conninfo to itself).
	if err := a.docker.CopyFile(ctx, id, "/etc", "repmgr.conf", 0o644, []byte(repmgrConf(nodeID, host, fqdn, frame, sec))); err != nil {
		return pr.fail("write repmgr.conf: %v", err)
	}
	// .pgpass so repmgr can authenticate to peers without prompting.
	home := pgHome(frame.OS)
	if err := a.docker.CopyFile(ctx, id, home, ".pgpass", 0o600, []byte(fmt.Sprintf("*:*:*:%s:%s\n", sec.ReplUser, sec.ReplPassword))); err != nil {
		return pr.fail("write .pgpass: %v", err)
	}
	if err := a.runStep(ctx, id, repmgrConfChownScript, []string{"HOME=" + home}, pr.logln); err != nil {
		return pr.fail("fix repmgr file ownership: %v", err)
	}
	return nil
}

// repmgrSetupPrimary initialises the primary's data dir, configures postgresql.conf
// + pg_hba (incl. replication + repmgr + optional Barman archiving + TLS), starts
// PostgreSQL, creates the repmgr role + database + superuser password, and runs
// `repmgr primary register`.
func (a *App) repmgrSetupPrimary(ctx context.Context, st Stack, frame designFrame, n designNode, major string, sec pgSecrets, swCfg seaweedConfig, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	debian := isDebianOS(frame.OS)
	dataDir := pgDataDir(frame.OS, major)
	confDir := pgConfDir(frame.OS, major)
	service := pgServiceName(frame.OS, major)
	domain := envOr("DOMAIN", "example.net")
	fqdn := fqdnOf(stackHostnames(buildDoc(st))[n.ID], domain)
	if fqdn == "." || strings.HasPrefix(fqdn, ".") {
		fqdn = fqdnOf(sanitizeName(n.Label), domain)
	}

	initEnv := []string{"MAJOR=" + major, "BINDIR=" + pgBinDir(frame.OS, major), "DATADIR=" + dataDir}
	if debian {
		initEnv = append(initEnv, "DEBIAN=1")
	}
	if err := a.runStep(ctx, id, pgInitScript, initEnv, pr.logln); err != nil {
		return pr.fail("initdb: %v", err)
	}
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 56)
		if err := a.pgApplyCert(ctx, id, a.intranetContainerID(ctx, st), fqdn, dataDir, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	confEnv := []string{"CONFDIR=" + confDir, "DATADIR=" + dataDir, "REPLUSER=" + sec.ReplUser}
	if frame.UseBarman {
		confEnv = append(confEnv, "ARCHIVE_CMD="+barmanArchiveCommand(frame.Label, swCfg))
	}
	if frame.GenerateCert {
		confEnv = append(confEnv, "TLS=1")
	}
	if err := a.runStep(ctx, id, repmgrConfigurePrimaryScript, confEnv, pr.logln); err != nil {
		return pr.fail("configure primary: %v", err)
	}
	if err := a.runStep(ctx, id, pgStartScript, []string{"SERVICE=" + service}, pr.logln); err != nil {
		return pr.fail("start PostgreSQL: %v", err)
	}
	// Set the superuser password, then create the repmgr role + database.
	if err := a.runStep(ctx, id, pgSetPasswordScript, []string{"SUPERPW=" + sec.SuperPassword}, pr.logln); err != nil {
		return pr.fail("set superuser password: %v", err)
	}
	if err := a.runStep(ctx, id, repmgrCreateRoleScript, []string{"REPLUSER=" + sec.ReplUser, "REPLPW=" + sec.ReplPassword}, pr.logln); err != nil {
		return pr.fail("create repmgr role/db: %v", err)
	}
	if err := a.runStep(ctx, id, repmgrPrimaryRegisterScript, []string{"BINDIR=" + pgBinDir(frame.OS, major)}, pr.logln); err != nil {
		return pr.fail("repmgr primary register: %v", err)
	}
	pr.logln("primary registered with repmgr (node " + strconv.Itoa(1) + ")")
	return nil
}

// repmgrSetupStandby clones the standby from the primary, starts PostgreSQL, and runs
// `repmgr standby register`. TLS (if enabled) is re-issued for this node after clone.
func (a *App) repmgrSetupStandby(ctx context.Context, st Stack, frame designFrame, n designNode, major, primaryFQDN string, sec pgSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	id := dep.ContainerID
	dataDir := pgDataDir(frame.OS, major)
	service := pgServiceName(frame.OS, major)
	domain := envOr("DOMAIN", "example.net")
	fqdn := fqdnOf(stackHostnames(buildDoc(st))[n.ID], domain)
	if fqdn == "." || strings.HasPrefix(fqdn, ".") {
		fqdn = fqdnOf(sanitizeName(n.Label), domain)
	}

	env := []string{
		"BINDIR=" + pgBinDir(frame.OS, major), "DATADIR=" + dataDir,
		"PRIMARY=" + primaryFQDN, "REPLUSER=" + sec.ReplUser,
	}
	if err := a.runStep(ctx, id, repmgrStandbyCloneScript, env, pr.logln); err != nil {
		return pr.fail("repmgr standby clone: %v", err)
	}
	if frame.GenerateCert {
		if err := a.pgApplyCert(ctx, id, a.intranetContainerID(ctx, st), fqdn, dataDir, frame.CertTTLValue, frame.CertTTLUnit, pr.logln); err != nil {
			return pr.fail("%v", err)
		}
	}
	pr.phase("Starting standby PostgreSQL", 70)
	if err := a.runStep(ctx, id, pgStartScript, []string{"SERVICE=" + service}, pr.logln); err != nil {
		return pr.fail("start PostgreSQL: %v", err)
	}
	if err := a.runStep(ctx, id, repmgrStandbyRegisterScript, []string{"BINDIR=" + pgBinDir(frame.OS, major)}, pr.logln); err != nil {
		return pr.fail("repmgr standby register: %v", err)
	}
	pr.logln("standby cloned + registered with repmgr")
	return nil
}

// intranetContainerID returns the running Intranet node's container id (for CA reads).
func (a *App) intranetContainerID(ctx context.Context, st Stack) string {
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	for _, n := range doc.Nodes {
		if n.Type == "intranet" {
			if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil {
				return dep.ContainerID
			}
		}
	}
	return ""
}

// buildDoc parses the stack design (helper for hostname lookups).
func buildDoc(st Stack) designDoc {
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	return doc
}

// repmgrPrimaryContainer returns the container id of the current primary (the member
// not in recovery), or "" if none — used for on-demand backups (post-failover safe).
func (a *App) repmgrPrimaryContainer(ctx context.Context, st Stack, frame designFrame, doc designDoc) string {
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "repmgr" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		if res, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", repmgrIsPrimaryScript}, nil); err == nil {
			if strings.TrimSpace(res.Stdout) == "primary" {
				return dep.ContainerID
			}
		}
	}
	return ""
}

// handleRepmgrBackup runs an on-demand Barman cloud backup on the current primary
// of a repmgr frame (owner-scoped).
func (a *App) handleRepmgrBackup(w http.ResponseWriter, r *http.Request) {
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
		if f.ID == fid && f.Type == "repmgr" {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "repmgr cluster not found")
		return
	}
	if !frame.UseBarman {
		writeErr(w, http.StatusBadRequest, "Barman backup is not enabled for this cluster")
		return
	}
	ctx := r.Context()
	cid := a.repmgrPrimaryContainer(ctx, st, frame, doc)
	if cid == "" {
		writeErr(w, http.StatusConflict, "no running primary found for this cluster")
		return
	}
	// The SeaweedFS config (bucket/endpoint) is needed to build the backup command.
	swCfg, _, err := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusConflict, "SeaweedFS backup node is not available")
		return
	}
	if res, err := a.docker.Exec(ctx, cid, []string{"bash", "-c", barmanBackupScript}, barmanBackupEnv(frame.Label, swCfg)); err != nil {
		writeErr(w, http.StatusInternalServerError, "Barman backup failed: "+err.Error())
		return
	} else if res.Code != 0 {
		writeErr(w, http.StatusInternalServerError, "Barman backup failed: "+lastLines(res.Stderr+res.Stdout, 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// barmanBackupEnv builds the env for the barman-cloud-backup command.
func barmanBackupEnv(label string, sw seaweedConfig) []string {
	return []string{
		"ENDPOINT=" + barmanEndpoint(sw),
		"S3URL=" + barmanS3URL(label, sw),
		"SERVER=" + barmanServer(label),
	}
}

// --------------------------------------------------------------- config files

// repmgrConf renders /etc/repmgr.conf for one node.
func repmgrConf(nodeID int, host, fqdn string, frame designFrame, sec pgSecrets) string {
	major := ppgMajorOf(frame.PGMajor)
	bindir := pgBinDir(frame.OS, major)
	var b strings.Builder
	fmt.Fprintf(&b, "node_id=%d\n", nodeID)
	fmt.Fprintf(&b, "node_name='%s'\n", host)
	fmt.Fprintf(&b, "conninfo='host=%s user=%s dbname=repmgr password=%s connect_timeout=2'\n", fqdn, sec.ReplUser, sec.ReplPassword)
	fmt.Fprintf(&b, "data_directory='%s'\n", pgDataDir(frame.OS, major))
	fmt.Fprintf(&b, "pg_bindir='%s'\n", bindir)
	fmt.Fprintf(&b, "failover='automatic'\n")
	fmt.Fprintf(&b, "promote_command='%s/repmgr standby promote -f /etc/repmgr.conf --log-to-file'\n", bindir)
	fmt.Fprintf(&b, "follow_command='%s/repmgr standby follow -f /etc/repmgr.conf --log-to-file --upstream-node-id=%%n'\n", bindir)
	fmt.Fprintf(&b, "reconnect_attempts=6\n")
	fmt.Fprintf(&b, "reconnect_interval=10\n")
	fmt.Fprintf(&b, "monitoring_history=yes\n")
	return b.String()
}

func barmanAWSCredentials(ak, sk string) string {
	return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n", ak, sk)
}

// barmanAWSConfig forces path-style S3 addressing (SeaweedFS requires it).
func barmanAWSConfig(region string) string {
	return fmt.Sprintf("[default]\nregion = %s\ns3 =\n    addressing_style = path\n", region)
}

// ------------------------------------------------------------------ scripts

// repmgrInstall{RHEL,Debian} install PostgreSQL + repmgr from the PGDG repo. repmgr
// is not packaged in the Percona repo, so the whole repmgr frame uses PGDG (its
// PostgreSQL layout matches Percona's, so the pg.go path helpers still apply).
const repmgrInstallRHEL = `set -e
dnf -y -q install which >/dev/null 2>&1 || true
if [ -n "$WITH_EPEL" ]; then
  dnf -y -q install "$EPELPKG" >/dev/null 2>&1 || dnf -y -q install epel-release >/dev/null 2>&1 || true
fi
elver=$(rpm -E %rhel)
arch=$(uname -m)
dnf -y -q install "https://download.postgresql.org/pub/repos/yum/reporpms/EL-${elver}-${arch}/pgdg-redhat-repo-latest.noarch.rpm" >/dev/null
# The OS-bundled postgresql module masks the PGDG packages; disable it first.
dnf -qy module disable postgresql >/dev/null 2>&1 || true
dnf -y -q install $PKGS >/dev/null`

const repmgrInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq curl ca-certificates gnupg lsb-release >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq curl ca-certificates gnupg lsb-release >/dev/null; }
install -d /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
codename=$(. /etc/os-release; echo "${VERSION_CODENAME}")
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt ${codename}-pgdg main" > /etc/apt/sources.list.d/pgdg.list
apt-get update -qq >/dev/null
apt-get install -y -qq $PKGS >/dev/null`

// barmanInstall{RHEL,Debian} install the barman-cloud utilities from the PGDG /
// apt.postgresql.org repos (already added for repmgr), plus boto3 for the aws-s3
// provider. Package naming differs by repo: PGDG's EL/yum repo ships the barman-cloud-*
// binaries in **barman-cli** (there is no barman-cli-cloud there), whereas
// apt.postgresql.org splits them into **barman-cli-cloud**. Debian falls back to barman-cli.
//
// boto3 must be importable by the *interpreter barman-cloud-backup actually runs under*,
// not the system python3. On EL9 PGDG builds barman for python3.12 (system python3 is
// 3.9) and there is no python3.12-boto3 RPM — so a `dnf install python3-boto3` lands in
// 3.9 and `barman-cloud-backup` then dies with "No module named 'botocore'". We derive
// the interpreter from the script shebang, pip-install boto3 into it (python3.12-pip is
// in AppStream), and verify against that interpreter. (Installing only boto3 into the
// otherwise-empty 3.12 site avoids the ResolutionImpossible that the full barman[cloud]
// pip route hit against 3.9's dnf-managed barman.)
const barmanInstallRHEL = `set -e
dnf -y -q install barman-cli >/dev/null
BCB="$(command -v barman-cloud-backup)" || { echo "barman-cloud-backup not on PATH after install"; exit 1; }
PYINT="$(head -1 "$BCB" | sed 's|^#!||; s| .*||')"
[ -x "$PYINT" ] || PYINT=/usr/bin/python3
PYVER="$(basename "$PYINT")"
if ! "$PYINT" -c 'import botocore' >/dev/null 2>&1; then
  dnf -y -q install "${PYVER}-pip" >/dev/null 2>&1 || true
  "$PYINT" -m pip install --quiet --upgrade boto3 >/dev/null 2>&1 || dnf -y -q install python3-boto3 >/dev/null 2>&1 || true
fi
"$PYINT" -c 'import boto3, botocore' >/dev/null 2>&1 || { echo "boto3/botocore not importable under $PYINT (barman-cloud needs it for the aws-s3 provider)"; exit 1; }`

const barmanInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq barman-cli-cloud python3-boto3 >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq barman-cli-cloud python3-boto3 >/dev/null 2>&1 || apt-get install -y -qq barman-cli python3-boto3 >/dev/null; }
BCB="$(command -v barman-cloud-backup)" || { echo "barman-cloud-backup not on PATH after install"; exit 1; }
# Verify boto3 against the interpreter barman actually uses (its shebang), not whichever
# python3 happens to be first on PATH.
PYINT="$(head -1 "$BCB" | sed 's|^#!||; s| .*||')"
[ -x "$PYINT" ] || PYINT=/usr/bin/python3
"$PYINT" -c 'import boto3, botocore' >/dev/null 2>&1 || { echo "boto3/botocore not importable under $PYINT (barman-cloud needs it for the aws-s3 provider)"; exit 1; }`

// barmanChownScript fixes ownership of the staged AWS credentials (postgres reads them).
const barmanChownScript = `set -e
chown -R postgres:postgres "$HOME/.aws"
chmod 700 "$HOME/.aws"; chmod 600 "$HOME/.aws/"* 2>/dev/null || true`

// repmgrConfChownScript makes repmgr.conf + .pgpass readable by postgres.
const repmgrConfChownScript = `set -e
chown postgres:postgres /etc/repmgr.conf 2>/dev/null || true
chown postgres:postgres "$HOME/.pgpass" 2>/dev/null || true
chmod 600 "$HOME/.pgpass" 2>/dev/null || true`

// repmgrConfigurePrimaryScript appends the replication/repmgr settings (and optional
// Barman archiving + TLS) to the primary's postgresql.conf and pg_hba.conf.
const repmgrConfigurePrimaryScript = `set -e
CONF="$CONFDIR/postgresql.conf"
HBA="$CONFDIR/pg_hba.conf"
[ -f "$CONF" ] || { echo "postgresql.conf not found at $CONF"; exit 1; }
{
  echo ""
  echo "# --- dbcanvas repmgr ---"
  echo "listen_addresses = '*'"
  echo "port = 5432"
  echo "password_encryption = scram-sha-256"
  echo "wal_level = replica"
  echo "max_wal_senders = 10"
  echo "max_replication_slots = 10"
  echo "wal_keep_size = 512MB"
  echo "hot_standby = on"
  echo "shared_preload_libraries = 'repmgr'"
} >> "$CONF"
if [ -n "$ARCHIVE_CMD" ]; then
  echo "archive_mode = on" >> "$CONF"
  echo "archive_command = '$ARCHIVE_CMD'" >> "$CONF"
fi
if [ -n "$TLS" ]; then
  {
    echo "ssl = on"
    echo "ssl_cert_file = '$DATADIR/server.crt'"
    echo "ssl_key_file = '$DATADIR/server.key'"
    echo "ssl_ca_file = '$DATADIR/ca.crt'"
  } >> "$CONF"
fi
grep -q "dbcanvas-repmgr" "$HBA" 2>/dev/null || {
  {
    echo "# dbcanvas-repmgr"
    echo "local replication $REPLUSER trust"
    echo "host replication $REPLUSER 0.0.0.0/0 scram-sha-256"
    echo "host replication $REPLUSER ::/0 scram-sha-256"
    echo "local repmgr $REPLUSER trust"
    echo "host repmgr $REPLUSER 0.0.0.0/0 scram-sha-256"
    echo "host all all 0.0.0.0/0 scram-sha-256"
  } >> "$HBA"
}
chown -R postgres:postgres "$CONFDIR" 2>/dev/null || true`

// repmgrCreateRoleScript creates the repmgr superuser/replication role + database.
const repmgrCreateRoleScript = `set -e
printf '%s\n' "CREATE ROLE \"$REPLUSER\" WITH LOGIN SUPERUSER REPLICATION PASSWORD :'pw';" \
  | runuser -u postgres -- psql -v ON_ERROR_STOP=0 -v pw="$REPLPW" 2>&1 | grep -viE 'already exists' || true
printf '%s\n' "ALTER ROLE \"$REPLUSER\" WITH PASSWORD :'pw';" \
  | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -v pw="$REPLPW"
runuser -u postgres -- psql -v ON_ERROR_STOP=0 -c "CREATE DATABASE repmgr OWNER \"$REPLUSER\";" 2>&1 | grep -viE 'already exists' || true
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -c "ALTER ROLE \"$REPLUSER\" SET search_path TO repmgr, public;"`

// repmgrPrimaryRegisterScript registers the primary with repmgr (idempotent via -F).
const repmgrPrimaryRegisterScript = `set -e
runuser -u postgres -- "$BINDIR/repmgr" -f /etc/repmgr.conf primary register -F 2>&1 | tail -20`

// repmgrStandbyCloneScript clones the standby from the primary then prepares to start.
const repmgrStandbyCloneScript = `set -e
install -d -m 700 -o postgres -g postgres "$DATADIR"
find "$DATADIR" -mindepth 1 -delete 2>/dev/null || true
runuser -u postgres -- "$BINDIR/repmgr" -h "$PRIMARY" -U "$REPLUSER" -d repmgr -f /etc/repmgr.conf standby clone --fast-checkpoint -F 2>&1 | tail -30`

// repmgrStandbyRegisterScript registers the standby after it is streaming.
const repmgrStandbyRegisterScript = `set -e
runuser -u postgres -- "$BINDIR/repmgr" -f /etc/repmgr.conf standby register -F 2>&1 | tail -20`

// repmgrdStartScript runs repmgrd (automatic failover) via a small systemd unit.
const repmgrdStartScript = `set -e
cat > /etc/systemd/system/repmgrd.service <<UNIT
[Unit]
Description=repmgr daemon
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=postgres
Group=postgres
ExecStart=$BINDIR/repmgrd -f /etc/repmgr.conf --no-pid-file
Restart=on-failure
RestartSec=5
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl reset-failed repmgrd 2>/dev/null || true
systemctl enable --now repmgrd >/dev/null 2>&1 || systemctl restart repmgrd
sleep 2
systemctl is-active --quiet repmgrd || { echo "repmgrd failed to start:"; journalctl -u repmgrd --no-pager 2>/dev/null | tail -15; exit 1; }`

// repmgrIsPrimaryScript prints "primary" when this node's PostgreSQL is not in recovery.
const repmgrIsPrimaryScript = `R=$(runuser -u postgres -- psql -tAc 'SELECT pg_is_in_recovery()' 2>/dev/null)
[ "$R" = "f" ] && echo primary || true`

// barmanBackupScript runs a barman-cloud base backup to the SeaweedFS S3 store as
// the postgres user (AWS creds come from ~postgres/.aws).
const barmanBackupScript = `set -e
runuser -u postgres -- barman-cloud-backup --cloud-provider aws-s3 --endpoint-url "$ENDPOINT" "$S3URL" "$SERVER"`
