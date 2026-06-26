package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PS MongoDB Sharded Cluster frame (Type=="psmdb"): a fixed-topology Percona Server
// for MongoDB sharded cluster — 3 shards, each a 3-node replica set (9 mongod), a
// 3-node config-server replica set (3 mongod), and 1 mongos query router (the
// "mongosh" node). Internal auth is via a shared keyFile; an `admin` root user is
// created on the config replica set and used (through mongos) to add the shards.

const (
	mongoPort      = 27017
	mongoCfgRS     = "cfg"
	mongoKeyFile   = "/etc/mongo.keyFile"
	mongoDataDir   = "/var/lib/mongo"
	mongoLogDir    = "/var/log/mongo"
	mongoRunDir    = "/var/run/mongodb"
	mongodConf     = "/etc/mongod.conf"
	mongosConf     = "/etc/mongos.conf"
	mongosUnitPath = "/etc/systemd/system/mongos.service"
)

// mongoConfig is the per-node profile stored on each deployment.
type mongoConfig struct {
	Cluster      string `json:"cluster"`
	Image        string `json:"image"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Role         string `json:"role"`    // "shard" | "config" | "mongos"
	Shard        int    `json:"shard"`   // shard index for role=="shard"
	ReplSet      string `json:"replSet"` // replica-set name (cfg / rs0 / rs1 / ...)
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	PSMDBMajor   string `json:"psmdbMajor"`
	Version      string `json:"version"`
	MongosPort   int    `json:"mongosPort"` // host-published mongos port (mongos node, 0 if unpublished)
	ExportPort   int    `json:"exportPort"` // host-published 27017 for a member/standalone (0 if unpublished)
	ConfigDB     string `json:"configDB"`   // configDB connection string (mongos)
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	MonitoredBy  string `json:"monitoredBy"`
	Ports        []int  `json:"ports"`
}

// mongoSecrets holds the cluster admin credentials and the shared internal-auth
// keyFile (same bytes on every member). KeyFile is never surfaced in the UI.
type mongoSecrets struct {
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`
	KeyFile       string `json:"keyFile"`
	PMMUser       string `json:"pmmUser"`     // MongoDB user PMM uses to scrape metrics
	PMMPassword   string `json:"pmmPassword"` // its password (stable across redeploys)
}

// psmdbRepo maps a major series to its percona-release repository name.
func psmdbRepo(major string) string {
	switch strings.TrimSpace(major) {
	case "6.0", "6":
		return "psmdb-60"
	case "7.0", "7":
		return "psmdb-70"
	default:
		return "psmdb-80"
	}
}

// shardRS returns the replica-set name for shard index i.
func shardRS(i int) string { return fmt.Sprintf("rs%d", i) }

// genKeyFile returns 756 random bytes, base64-encoded (a valid MongoDB keyFile).
func genKeyFile() string {
	b := make([]byte, 756)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// provisionMongoDBFrame brings up the whole sharded cluster: install + base config
// per member (parallel), initiate the config and shard replica sets, create the
// admin user, then start mongos and add the shards.
func (a *App) provisionMongoDBFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	// Partition members by role.
	var config []designNode
	shards := map[int][]designNode{}
	var mongos *designNode
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "psmdb" {
			continue
		}
		switch n.Role {
		case "config":
			config = append(config, n)
		case "mongos":
			m := n
			mongos = &m
		default: // "shard"
			shards[n.Shard] = append(shards[n.Shard], n)
		}
	}
	byLabel := func(s []designNode) { sort.Slice(s, func(i, j int) bool { return s[i].Label < s[j].Label }) }
	byLabel(config)
	var shardIdx []int
	for i := range shards {
		byLabel(shards[i])
		shardIdx = append(shardIdx, i)
	}
	sort.Ints(shardIdx)
	if len(config) == 0 || mongos == nil || len(shardIdx) == 0 {
		log.Printf("stack %d psmdb %s: incomplete topology (config=%d shards=%d mongos=%v)", st.ID, frame.Label, len(config), len(shardIdx), mongos != nil)
		return
	}

	// All members, in a stable order (config, shards, mongos).
	var members []designNode
	members = append(members, config...)
	for _, i := range shardIdx {
		members = append(members, shards[i]...)
	}
	members = append(members, *mongos)

	// Secrets: reuse the admin password + keyFile + PMM password across redeploys.
	admin := strings.TrimSpace(frame.RootPassword)
	keyFile := ""
	pmmPass := ""
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s mongoSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil {
				if s.AdminPassword != "" {
					admin = s.AdminPassword
				}
				if s.KeyFile != "" {
					keyFile = s.KeyFile
				}
				if s.PMMPassword != "" {
					pmmPass = s.PMMPassword
				}
			}
		}
	}
	if admin == "" {
		admin = genSecret("MongoAdm!")
	}
	if keyFile == "" {
		keyFile = genKeyFile()
	}
	if pmmPass == "" {
		pmmPass = genSecret("MongoPMM!")
	}
	sec := mongoSecrets{AdminUser: "admin", AdminPassword: admin, KeyFile: keyFile, PMMUser: "pmm", PMMPassword: pmmPass}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := frame.PSMDBMajor
	if major == "" {
		major = "8.0"
	}

	// configDB connection string for mongos: cfg/host1:27017,host2:27017,host3:27017
	var cfgHosts []string
	for _, n := range config {
		cfgHosts = append(cfgHosts, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), mongoPort))
	}
	configDB := mongoCfgRS + "/" + strings.Join(cfgHosts, ",")

	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	replSetOf := func(n designNode) string {
		switch n.Role {
		case "config":
			return mongoCfgRS
		case "mongos":
			return ""
		default:
			return shardRS(n.Shard)
		}
	}

	// Record every member as pending with its profile.
	for _, n := range members {
		host := hosts[n.ID]
		cfg := mongoConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: n.Role, Shard: n.Shard, ReplSet: replSetOf(n), Hostname: host, FQDN: fqdnOf(host, domain),
			PSMDBMajor: major, Version: frame.PSMDBVersion, ConfigDB: configDB,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: []int{mongoPort},
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

		// ---- Phase 1 (parallel): container + install + keyFile + config + start mongod ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mongoPrepareNode(ctx, st, frame, n, hosts[n.ID], image, major, replSetOf(n), configDB, intranetIP, domain, sec, progs[n.ID]); err != nil {
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

		// ---- Phase 2: initiate replica sets (config first, then each shard) ----
		if err := a.mongoInitReplicaSet(ctx, st, mongoCfgRS, config, hosts, domain, "configsvr", progs); err != nil {
			return
		}
		// Create the admin user on the config RS primary (replicates within the RS;
		// reachable cluster-wide through mongos).
		if err := a.mongoCreateAdmin(ctx, st, config[0], sec, progs[config[0].ID]); err != nil {
			return
		}
		for _, i := range shardIdx {
			if err := a.mongoInitReplicaSet(ctx, st, shardRS(i), shards[i], hosts, domain, "shardsvr", progs); err != nil {
				return
			}
		}

		// ---- Phase 3: start mongos + add the shards ----
		mpr := progs[mongos.ID]
		mpr.phase("Starting mongos router", 80)
		if err := a.mongoStartMongos(ctx, st, *mongos, configDB, progs[mongos.ID]); err != nil {
			return
		}
		var shardSpecs []string
		for _, i := range shardIdx {
			var hs []string
			for _, n := range shards[i] {
				hs = append(hs, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), mongoPort))
			}
			shardSpecs = append(shardSpecs, shardRS(i)+"/"+strings.Join(hs, ","))
		}
		if err := a.mongoAddShards(ctx, st, *mongos, sec, shardSpecs, progs[mongos.ID]); err != nil {
			return
		}

		// ---- PMM: create the monitoring user + register each node (when a running
		// PMM node is selected). The pmm user goes on the config RS (admin auth) and
		// each shard RS (shards have no admin → created via the localhost exception).
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, frame.PMMNodeID, 12*time.Minute); ok {
			a.mongoEnsurePMMUser(ctx, st, config[0], major, sec, progs[config[0].ID])
			for _, i := range shardIdx {
				a.mongoEnsurePMMUser(ctx, st, shards[i][0], major, sec, progs[shards[i][0].ID])
			}
			for _, n := range members {
				a.mongoRegisterPMM(ctx, st, n, frame.OS, pmmFQDN, pmmUser, pmmPass, frame.Label, sec, progs[n.ID])
			}
		}

		// ---- Phase 4: finalize ----
		for _, n := range members {
			pr := progs[n.ID]
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d psmdb %s: provisioned (%d members, %d shards)", st.ID, frame.Label, len(members), len(shardIdx))
		_ = intranetID
	}()
}

// provisionMongoRSFrame brings up a single Percona Server for MongoDB replica set
// (Type=="psmrs"): N mongod members with a shared keyFile for internal auth, one
// rs.initiate over all members, and an `admin` (root) user created on the primary.
func (a *App) provisionMongoRSFrame(st Stack, frame designFrame, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)

	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "psmrs" {
			m := n
			m.Role = "member"
			members = append(members, m)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	if len(members) == 0 {
		log.Printf("stack %d psmrs %s: no members", st.ID, frame.Label)
		return
	}

	rs := sanitizeName(frame.Label)
	if rs == "" {
		rs = "rs"
	}

	// Secrets: reuse the admin password + keyFile + PMM password across redeploys.
	admin := strings.TrimSpace(frame.RootPassword)
	keyFile := ""
	pmmPass := ""
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s mongoSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil {
				if s.AdminPassword != "" {
					admin = s.AdminPassword
				}
				if s.KeyFile != "" {
					keyFile = s.KeyFile
				}
				if s.PMMPassword != "" {
					pmmPass = s.PMMPassword
				}
			}
		}
	}
	if admin == "" {
		admin = genSecret("MongoAdm!")
	}
	if keyFile == "" {
		keyFile = genKeyFile()
	}
	if pmmPass == "" {
		pmmPass = genSecret("MongoPMM!")
	}
	sec := mongoSecrets{AdminUser: "admin", AdminPassword: admin, KeyFile: keyFile, PMMUser: "pmm", PMMPassword: pmmPass}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := frame.PSMDBMajor
	if major == "" {
		major = "8.0"
	}
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		for _, n := range doc.Nodes {
			if n.ID == frame.PMMNodeID {
				monitoredBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	for _, n := range members {
		host := hosts[n.ID]
		cfg := mongoConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: "member", ReplSet: rs, Hostname: host, FQDN: fqdnOf(host, domain),
			PSMDBMajor: major, Version: frame.PSMDBVersion,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			Ports: []int{mongoPort},
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
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			failAll("%v", werr)
			return
		}

		// Phase 1 (parallel): container + install + keyFile + config + start mongod.
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mongoPrepareNode(ctx, st, frame, n, hosts[n.ID], image, major, rs, "", intranetIP, domain, sec, progs[n.ID]); err != nil {
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

		// Phase 2: initiate the replica set + create the admin user on the primary.
		if err := a.mongoInitReplicaSet(ctx, st, rs, members, hosts, domain, "", progs); err != nil {
			return
		}
		if err := a.mongoCreateAdmin(ctx, st, members[0], sec, progs[members[0].ID]); err != nil {
			return
		}

		// PMM: create the monitoring user on the primary (replicates to the set) and
		// register each member with --cluster=<replica-set name>.
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, frame.PMMNodeID, 12*time.Minute); ok {
			a.mongoEnsurePMMUser(ctx, st, members[0], major, sec, progs[members[0].ID])
			for _, n := range members {
				a.mongoRegisterPMM(ctx, st, n, frame.OS, pmmFQDN, pmmUser, pmmPass, rs, sec, progs[n.ID])
			}
		}

		for _, n := range members {
			pr := progs[n.ID]
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d psmrs %s: provisioned (%d members, rs=%s)", st.ID, frame.Label, len(members), rs)
	}()
}

// provisionMongoStandalone provisions a standalone Percona Server for MongoDB node
// (Type=="psm"): a single mongod with authorization enabled (no replica set, no
// keyFile) and an `admin` (root) user created via the localhost exception.
func (a *App) provisionMongoStandalone(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}

	// Synthetic frame carrying the node's own image/options (mongoPrepareNode reads
	// frame.OS/UseProxy/PSMDBVersion).
	frame := designFrame{
		Type: "psm", Label: n.Label,
		OS: n.OS, OSVersion: n.OSVersion, Arch: n.Arch,
		PSMDBMajor: n.PSMDBMajor, PSMDBVersion: n.PSMDBVersion,
		UseProxy: n.UseProxy, GenerateCert: n.GenerateCert,
		CertTTLValue: n.CertTTLValue, CertTTLUnit: n.CertTTLUnit, PMMNodeID: n.PMMNodeID,
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
	major := n.PSMDBMajor
	if major == "" {
		major = "8.0"
	}

	admin := strings.TrimSpace(n.RootPassword)
	pmmPass := ""
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s mongoSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil {
			if s.AdminPassword != "" {
				admin = s.AdminPassword
			}
			if s.PMMPassword != "" {
				pmmPass = s.PMMPassword
			}
		}
	}
	if admin == "" {
		admin = genSecret("MongoAdm!")
	}
	if pmmPass == "" {
		pmmPass = genSecret("MongoPMM!")
	}
	sec := mongoSecrets{AdminUser: "admin", AdminPassword: admin, PMMUser: "pmm", PMMPassword: pmmPass} // no keyFile → standalone

	monitoredBy := ""
	if n.PMMNodeID != "" {
		for _, m := range doc.Nodes {
			if m.ID == n.PMMNodeID && m.Type == "pmm" {
				monitoredBy = fqdnOf(hosts[m.ID], domain)
			}
		}
	}
	cfg := mongoConfig{
		Cluster: "", Image: image, OS: n.OS, Arch: archOr(n.Arch), Role: "standalone",
		Hostname: host, FQDN: fqdnOf(host, domain), PSMDBMajor: major, Version: n.PSMDBVersion,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports: []int{mongoPort},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		pr.phase("Waiting for Intranet to be ready", 5)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		nn := n
		nn.Role = "standalone"
		if err := a.mongoPrepareNode(ctx, st, frame, nn, host, image, major, "", "", intranetIP, domain, sec, pr); err != nil {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)
		if err := a.mongoCreateAdmin(ctx, st, n, sec, pr); err != nil {
			return
		}

		// PMM: create the monitoring user + register the standalone (no --cluster).
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, n.PMMNodeID, 12*time.Minute); ok {
			a.mongoEnsurePMMUser(ctx, st, nn, major, sec, pr)
			a.mongoRegisterPMM(ctx, st, nn, frame.OS, pmmFQDN, pmmUser, pmmPass, "", sec, pr)
		}

		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d psm %s: provisioned (standalone)", st.ID, n.Label)
	}()
}

// mongoPrepareNode creates the container, installs Percona Server for MongoDB, writes
// the shared keyFile and the mongod/mongos config, and starts mongod (config/shard
// members; the mongos node is started later in Phase 3).
func (a *App) mongoPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, major, replSet, configDB, intranetIP, domain string, sec mongoSecrets, pr *pxcProg) error {
	if host == "" {
		host = sanitizeName(n.Label)
	}
	pr.phase("Creating container", 12)
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
		spec.PublishMap = []PortMap{{ContainerPort: mongoPort, HostPort: n.ExportHostPort}}
	}
	id, err := a.docker.ContainerCreate(ctx, spec)
	if err != nil {
		return pr.fail("create container: %v", err)
	}
	if err := a.docker.ContainerStart(ctx, id); err != nil {
		return pr.fail("start container: %v", err)
	}
	a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: a.depConfig(st.ID, n.ID), Secrets: a.depSecrets(st.ID, n.ID)})

	// Record the auto-assigned host port for an exported node so the manager can
	// show the connect string (mongos also keeps it under MongosPort).
	if n.ExportEnabled {
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", mongoPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				if dep, e3 := a.store.GetDeployment(st.ID, n.ID); e3 == nil {
					var cfg mongoConfig
					json.Unmarshal(dep.Config, &cfg)
					cfg.ExportPort = p
					if n.Role == "mongos" {
						cfg.MongosPort = p
					}
					cfgJSON, _ := json.Marshal(cfg)
					a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: dep.Secrets})
				}
			}
		}
	}

	pr.phase("Waiting for systemd", 22)
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

	pr.phase("Installing Percona Server for MongoDB", 40)
	pkgs := "percona-server-mongodb-server percona-server-mongodb-tools percona-mongodb-mongosh"
	if n.Role == "mongos" {
		pkgs = "percona-server-mongodb-mongos percona-server-mongodb-tools percona-mongodb-mongosh"
	}
	instScript := mongoInstallRHEL
	if debian {
		instScript = mongoInstallDebian
	}
	if err := a.runStep(ctx, id, instScript, []string{"PSMDB_REPO=" + psmdbRepo(major), "PKGS=" + pkgs, "VER=" + frame.PSMDBVersion}, pr.logln); err != nil {
		return pr.fail("install packages: %v", err)
	}
	pr.logln("installed: " + pkgs)
	a.ensureRsyslog(ctx, id, frame.OS, pr.logln)

	// Always install pmm-client (pmm3-client) — independent of whether monitoring is
	// enabled — so it can be turned on later without a reinstall (same as PXC).
	pmmInstall := pxcInstallPMMClientRHEL
	if debian {
		pmmInstall = pxcInstallPMMClientDebian
	}
	if err := a.runStep(ctx, id, pmmInstall, nil, pr.logln); err != nil {
		return pr.fail("install pmm-client: %v", err)
	}
	pr.logln("pmm-client installed")

	// Shared keyFile (same bytes everywhere) for internal cluster auth. Standalone
	// nodes have no keyFile (sec.KeyFile == "").
	if sec.KeyFile != "" {
		if err := a.docker.CopyFile(ctx, id, "/etc", "mongo.keyFile", 0o400, []byte(sec.KeyFile)); err != nil {
			return pr.fail("write keyFile: %v", err)
		}
	}

	if n.Role == "mongos" {
		// mongos has no datadir/replset; its config + unit are written in Phase 3.
		// Just lay down the shared dirs/keyFile ownership now.
		if err := a.runStep(ctx, id, mongoPrepDirsScript, nil, pr.logln); err != nil {
			return pr.fail("prepare dirs: %v", err)
		}
		return nil
	}

	// Write mongod.conf and start mongod. Sharded members carry a clusterRole; a
	// plain replica-set member ("member") or standalone ("standalone") has none.
	clusterRole := ""
	switch n.Role {
	case "config":
		clusterRole = "configsvr"
	case "shard":
		clusterRole = "shardsvr"
	}
	conf := mongodConfYAML(replSet, clusterRole, sec.KeyFile != "")
	if err := a.docker.CopyFile(ctx, id, "/etc", "mongod.conf", 0o644, []byte(conf)); err != nil {
		return pr.fail("write mongod.conf: %v", err)
	}
	pr.phase("Starting mongod", 55)
	if err := a.runStep(ctx, id, mongoStartMongodScript, nil, pr.logln); err != nil {
		return pr.fail("start mongod: %v", err)
	}
	return nil
}

// mongoInitReplicaSet runs rs.initiate on the first member of a replica set (via the
// localhost exception, before any user exists) and waits for a PRIMARY.
func (a *App) mongoInitReplicaSet(ctx context.Context, st Stack, rs string, ms []designNode, hosts map[string]string, domain, role string, progs map[string]*pxcProg) error {
	if len(ms) == 0 {
		return nil
	}
	first := ms[0]
	dep, _ := a.store.GetDeployment(st.ID, first.ID)
	pr := progs[first.ID]
	pr.phase("Initiating replica set "+rs, 65)
	var memberJSON []string
	for i, n := range ms {
		memberJSON = append(memberJSON, fmt.Sprintf(`{_id:%d,host:"%s:%d"}`, i, fqdnOf(hosts[n.ID], domain), mongoPort))
	}
	cfg := fmt.Sprintf(`{_id:"%s",configsvr:%v,members:[%s]}`, rs, role == "configsvr", strings.Join(memberJSON, ","))
	if err := a.runStep(ctx, dep.ContainerID, mongoInitRSScript, []string{"RSCFG=" + cfg, "RS=" + rs}, pr.logln); err != nil {
		return pr.fail("initiate replica set %s: %v", rs, err)
	}
	pr.logln("replica set " + rs + " initiated (PRIMARY elected)")
	return nil
}

// mongoCreateAdmin creates the cluster admin (root) user on a replica-set primary via
// the localhost exception.
func (a *App) mongoCreateAdmin(ctx context.Context, st Stack, primary designNode, sec mongoSecrets, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, primary.ID)
	pr.phase("Creating admin user", 70)
	if err := a.runStep(ctx, dep.ContainerID, mongoCreateAdminScript, []string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword}, pr.logln); err != nil {
		return pr.fail("create admin user: %v", err)
	}
	pr.logln("admin user created on config replica set")
	return nil
}

// ------------------------------------------------------------------ PMM

// mongoWaitPMM resolves the selected PMM node, waiting (bounded) for it to finish
// provisioning — the PMM server is heavy and often comes up after the database
// nodes. Returns ok=false when no PMM node is selected or it never becomes ready.
func (a *App) mongoWaitPMM(st Stack, doc designDoc, pmmNodeID string, timeout time.Duration) (fqdn, user, pass string, ok bool) {
	if pmmNodeID == "" {
		return "", "", "", false
	}
	deadline := time.Now().Add(timeout)
	for {
		if f, u, p, k := a.pmmServerFor(st, doc, pmmNodeID); k {
			return f, u, p, true
		}
		if time.Now().After(deadline) {
			return "", "", "", false
		}
		time.Sleep(5 * time.Second)
	}
}

// mongoPMMRoles returns the JS roles array for the PMM monitoring user. Per the PMM3
// docs: pmmMonitor + read@local + clusterMonitor, plus directShardOperations on 8.0+.
func mongoPMMRoles(major string) string {
	roles := `[{db:"admin",role:"pmmMonitor"},{db:"local",role:"read"},{db:"admin",role:"clusterMonitor"}`
	if strings.HasPrefix(strings.TrimSpace(major), "8.") {
		roles += `,{db:"admin",role:"directShardOperations"}`
	}
	return roles + "]"
}

// mongoPMMUserJS builds the mongosh script that (idempotently) creates the pmmMonitor
// role and the PMM user with the given roles.
func mongoPMMUserJS(user, pass, rolesJS string) string {
	const priv = `[{resource:{db:"",collection:""},actions:["dbHash","find","listIndexes","listCollections","collStats","dbStats","indexStats"]},` +
		`{resource:{db:"",collection:"system.version"},actions:["find"]},` +
		`{resource:{db:"",collection:"system.profile"},actions:["dbStats","collStats","indexStats"]}]`
	return fmt.Sprintf(`var a=db.getSiblingDB("admin");
try{a.createRole({role:"pmmMonitor",privileges:%s,roles:[]})}catch(e){if(!/already exists/i.test(e.message))throw e}
try{a.createUser({user:%q,pwd:%q,roles:%s})}catch(e){if(/already exists/i.test(e.message)){a.updateUser(%q,{pwd:%q,roles:%s})}else throw e}`,
		priv, user, pass, rolesJS, user, pass, rolesJS)
}

// mongoEnsurePMMUser creates/updates the PMM monitoring user on a replica-set primary
// (or standalone). It authenticates as admin when those creds work, otherwise falls
// back to the localhost exception (used by sharded shards, which have no admin user).
func (a *App) mongoEnsurePMMUser(ctx context.Context, st Stack, node designNode, major string, sec mongoSecrets, pr *pxcProg) error {
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return nil
	}
	js := mongoPMMUserJS(sec.PMMUser, sec.PMMPassword, mongoPMMRoles(major))
	env := []string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "PMM_JS=" + js}
	if err := a.runStep(ctx, dep.ContainerID, mongoPMMUserScript, env, pr.logln); err != nil {
		return pr.fail("create PMM user: %v", err)
	}
	pr.logln("PMM monitoring user ready")
	return nil
}

// mongoRegisterPMM points the node's pmm-client at the PMM server and registers its
// mongodb service (with --cluster for replica-set / sharded members). Best-effort:
// failures are logged but do not fail the deployment.
func (a *App) mongoRegisterPMM(ctx context.Context, st Stack, node designNode, os, pmmFQDN, pmmUser, pmmPass, cluster string, sec mongoSecrets, pr *pxcProg) {
	if pmmFQDN == "" {
		return
	}
	dep, err := a.store.GetDeployment(st.ID, node.ID)
	if err != nil || dep.ContainerID == "" {
		return
	}
	if pmmUser == "" {
		pmmUser = "admin"
	}
	if pmmPass == "" {
		pmmPass = "admin"
	}
	script := mongoPMMAddRHEL
	if isDebianOS(os) {
		script = mongoPMMAddDebian
	}
	env := []string{
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass,
		"PMM_DB_USER=" + sec.PMMUser, "PMM_DB_PW=" + sec.PMMPassword,
		"NODE=" + node.Label, "CLUSTER=" + cluster,
	}
	if _, err := a.docker.Exec(ctx, dep.ContainerID, []string{"bash", "-c", script}, env); err != nil {
		pr.logln("PMM registration skipped: " + err.Error())
	} else {
		pr.logln("registered with PMM at " + pmmFQDN)
	}
}

// mongoStartMongos writes mongos.conf + the mongos systemd unit and starts it.
func (a *App) mongoStartMongos(ctx context.Context, st Stack, mongos designNode, configDB string, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, mongos.ID)
	if err := a.docker.CopyFile(ctx, dep.ContainerID, "/etc", "mongos.conf", 0o644, []byte(mongosConfYAML(configDB))); err != nil {
		return pr.fail("write mongos.conf: %v", err)
	}
	if err := a.docker.CopyFile(ctx, dep.ContainerID, "/etc/systemd/system", "mongos.service", 0o644, []byte(mongosUnit)); err != nil {
		return pr.fail("write mongos unit: %v", err)
	}
	if err := a.runStep(ctx, dep.ContainerID, mongoStartMongosScript, nil, pr.logln); err != nil {
		return pr.fail("start mongos: %v", err)
	}
	pr.logln("mongos router running on 27017")
	return nil
}

// mongoAddShards authenticates to mongos as admin and adds each shard replica set.
func (a *App) mongoAddShards(ctx context.Context, st Stack, mongos designNode, sec mongoSecrets, shardSpecs []string, pr *pxcProg) error {
	dep, _ := a.store.GetDeployment(st.ID, mongos.ID)
	pr.phase("Adding shards", 88)
	env := []string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "SHARDS=" + strings.Join(shardSpecs, " ")}
	if err := a.runStep(ctx, dep.ContainerID, mongoAddShardsScript, env, pr.logln); err != nil {
		return pr.fail("add shards: %v", err)
	}
	pr.logln("shards added: " + strings.Join(shardSpecs, " "))
	return nil
}

// ------------------------------------------------------------------ config

// mongodConfYAML renders mongod.conf. replSet=="" → standalone (no replication
// block); clusterRole=="" → no sharding block; useKeyFile=false → authorization
// only (no keyFile, for a standalone with no internal cluster auth).
func mongodConfYAML(replSet, clusterRole string, useKeyFile bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "storage:\n  dbPath: %s\n", mongoDataDir)
	fmt.Fprintf(&b, "systemLog:\n  destination: file\n  path: %s/mongod.log\n  logAppend: true\n", mongoLogDir)
	fmt.Fprintf(&b, "net:\n  port: %d\n  bindIpAll: true\n", mongoPort)
	fmt.Fprintf(&b, "processManagement:\n  fork: false\n  pidFilePath: %s/mongod.pid\n", mongoRunDir)
	if useKeyFile {
		fmt.Fprintf(&b, "security:\n  keyFile: %s\n  authorization: enabled\n", mongoKeyFile)
	} else {
		fmt.Fprintf(&b, "security:\n  authorization: enabled\n")
	}
	if replSet != "" {
		fmt.Fprintf(&b, "replication:\n  replSetName: %s\n", replSet)
	}
	if clusterRole != "" {
		fmt.Fprintf(&b, "sharding:\n  clusterRole: %s\n", clusterRole)
	}
	return b.String()
}

func mongosConfYAML(configDB string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "systemLog:\n  destination: file\n  path: %s/mongos.log\n  logAppend: true\n", mongoLogDir)
	fmt.Fprintf(&b, "net:\n  port: %d\n  bindIpAll: true\n", mongoPort)
	fmt.Fprintf(&b, "processManagement:\n  fork: false\n  pidFilePath: %s/mongos.pid\n", mongoRunDir)
	fmt.Fprintf(&b, "security:\n  keyFile: %s\n", mongoKeyFile)
	fmt.Fprintf(&b, "sharding:\n  configDB: %s\n", configDB)
	return b.String()
}

// ------------------------------------------------------------------ scripts

const mongoInstallRHEL = `set -e
percona-release enable -y "$PSMDB_REPO" >/dev/null 2>&1 || percona-release enable "$PSMDB_REPO" >/dev/null 2>&1 || percona-release setup -y "$PSMDB_REPO" >/dev/null 2>&1
dnf -y -q install $PKGS >/dev/null`

const mongoInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable -y "$PSMDB_REPO" >/dev/null 2>&1 || percona-release enable "$PSMDB_REPO" >/dev/null 2>&1 || percona-release setup -y "$PSMDB_REPO" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq $PKGS >/dev/null`

// mongoPrepDirsScript ensures the runtime/log dirs exist and own the keyFile (mongos
// node, which has no mongod package post-install to create them).
const mongoPrepDirsScript = `set -e
id mongod >/dev/null 2>&1 || useradd -r -s /sbin/nologin mongod 2>/dev/null || true
install -d -o mongod -g mongod /var/log/mongo /var/run/mongodb 2>/dev/null || true
chown mongod:mongod /etc/mongo.keyFile 2>/dev/null || true`

// mongoStartMongodScript fixes keyFile ownership, ensures dirs, starts mongod and
// waits until it answers a ping (still in STARTUP / pre-initiate is fine).
const mongoStartMongodScript = `set -e
chown mongod:mongod /etc/mongo.keyFile 2>/dev/null || true
install -d -o mongod -g mongod /var/lib/mongo /var/log/mongo /var/run/mongodb 2>/dev/null || true
systemctl reset-failed mongod 2>/dev/null || true
systemctl enable --now mongod >/dev/null 2>&1 || systemctl restart mongod
OK=0
for i in $(seq 1 30); do
  mongosh --quiet --port 27017 --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1 && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "mongod did not become reachable:"; tail -20 /var/log/mongo/mongod.log 2>/dev/null; exit 1; }`

// mongoInitRSScript runs rs.initiate (idempotent: a re-run finds it already
// initiated) and waits for a PRIMARY.
const mongoInitRSScript = `set -e
mongosh --quiet --port 27017 --eval 'try { rs.initiate('"$RSCFG"') } catch (e) { if (!/already initialized/i.test(e.message)) throw e }'
OK=0
for i in $(seq 1 60); do
  S=$(mongosh --quiet --port 27017 --eval 'try{rs.status().myState}catch(e){-1}' 2>/dev/null)
  [ "$S" = "1" ] && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "replica set $RS has no PRIMARY:"; mongosh --quiet --port 27017 --eval 'try{rs.status()}catch(e){print(e)}' 2>/dev/null | head -40; exit 1; }`

// mongoCreateAdminScript creates the root admin user via the localhost exception
// (idempotent: tolerates an already-existing user).
const mongoCreateAdminScript = `set -e
mongosh --quiet --port 27017 --eval 'db.getSiblingDB("admin").createUser({user:"'"$ADMIN_USER"'",pwd:"'"$ADMIN_PW"'",roles:[{role:"root",db:"admin"}]})' 2>&1 | grep -viE 'already exists' || true
mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval 'db.adminCommand({ping:1})' >/dev/null`

// mongoStartMongosScript starts the mongos router and waits until it answers a ping.
const mongoStartMongosScript = `set -e
chown mongod:mongod /etc/mongo.keyFile 2>/dev/null || true
install -d -o mongod -g mongod /var/log/mongo /var/run/mongodb 2>/dev/null || true
systemctl daemon-reload
systemctl reset-failed mongos 2>/dev/null || true
systemctl enable --now mongos >/dev/null 2>&1 || systemctl restart mongos
OK=0
for i in $(seq 1 30); do
  mongosh --quiet --port 27017 --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1 && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "mongos did not become reachable:"; tail -20 /var/log/mongo/mongos.log 2>/dev/null; journalctl -u mongos --no-pager -n 20 2>/dev/null; exit 1; }`

// mongoAddShardsScript adds each shard (idempotent: an already-added shard is
// reported and ignored).
const mongoAddShardsScript = `set -e
for s in $SHARDS; do
  mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval 'sh.addShard("'"$s"'")' 2>&1 | grep -viE 'already a member|already exists' || true
done
mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval 'sh.status()' >/dev/null`

// mongoPMMUserScript creates the PMM role + user, authenticated as the cluster admin.
// When admin auth fails (a sharded shard has no admin user), it first creates the
// admin via the localhost exception — which only permits creating the first user, not
// roles — then authenticates to create the pmmMonitor role + PMM user. PMM_JS carries
// the role/user creation JS (built in Go). Run on a replica-set PRIMARY.
const mongoPMMUserScript = `set -e
if ! mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1; then
  mongosh --quiet --port 27017 --eval 'db.getSiblingDB("admin").createUser({user:"'"$ADMIN_USER"'",pwd:"'"$ADMIN_PW"'",roles:[{role:"root",db:"admin"}]})' 2>&1 | grep -viE 'already exists' || true
fi
mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval "$PMM_JS"`

// mongoPMMAdd{RHEL,Debian} point pmm-client at the PMM server and register this
// node's mongodb service (idempotent: a prior service of the same name is removed
// first). pmm-admin config talks to the local pmm-agent, so it is enabled first.
const mongoPMMAddRHEL = `set -e
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; dnf -y -q install pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="https://$PMM_USER:$PMM_PASS@$PMM_FQDN:8443" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove mongodb "$NODE" >/dev/null 2>&1 || true
CL=""; [ -n "$CLUSTER" ] && CL="--cluster=$CLUSTER"
pmm-admin add mongodb --username="$PMM_DB_USER" --password="$PMM_DB_PW" --host=127.0.0.1 --port=27017 $CL --enable-all-collectors "$NODE"`

const mongoPMMAddDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="https://$PMM_USER:$PMM_PASS@$PMM_FQDN:8443" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove mongodb "$NODE" >/dev/null 2>&1 || true
CL=""; [ -n "$CLUSTER" ] && CL="--cluster=$CLUSTER"
pmm-admin add mongodb --username="$PMM_DB_USER" --password="$PMM_DB_PW" --host=127.0.0.1 --port=27017 $CL --enable-all-collectors "$NODE"`

const mongosUnit = `[Unit]
Description=Percona Server for MongoDB mongos router
After=network-online.target
Wants=network-online.target

[Service]
User=mongod
Group=mongod
ExecStart=/usr/bin/mongos --config /etc/mongos.conf
PIDFile=/var/run/mongodb/mongos.pid
LimitNOFILE=64000
TimeoutStartSec=90
Restart=on-failure

[Install]
WantedBy=multi-user.target
`
