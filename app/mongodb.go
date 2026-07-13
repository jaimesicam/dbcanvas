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
	EnablePBM    bool   `json:"enablePBM"`  // Percona Backup for MongoDB → SeaweedFS S3
	BackupRepo   string `json:"backupRepo"` // e.g. "PBM → SeaweedFS S3 (bucket/prefix)" when enabled
	// Keycloak OIDC (standalone psm node only).
	OIDCEnabled      bool   `json:"oidcEnabled"`
	OIDCIssuer       string `json:"oidcIssuer"`   // https://<keycloak-fqdn>:8443/realms/<realm>
	OIDCClientID     string `json:"oidcClientId"` // == audience
	OIDCRealm        string `json:"oidcRealm"`
	OIDCAuthClaim    string `json:"oidcAuthClaim"` // group claim (when useAuthorizationClaim)
	OIDCUseAuthClaim bool   `json:"oidcUseAuthClaim"`
	OIDCSampleUsers  string `json:"oidcSampleUsers"` // sample Keycloak users created (for the manager)
	// Data-at-rest encryption keyed by an OpenBao node (psm standalone only; see dbvault.go).
	Vault vaultInfo `json:"vault"`
}

// mongoSecrets holds the cluster admin credentials and the shared internal-auth
// keyFile (same bytes on every member). KeyFile is never surfaced in the UI.
type mongoSecrets struct {
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`
	KeyFile       string `json:"keyFile"`
	PMMUser       string `json:"pmmUser"`     // MongoDB user PMM uses to scrape metrics
	PMMPassword   string `json:"pmmPassword"` // its password (stable across redeploys)
	PBMUser       string `json:"pbmUser"`     // MongoDB user Percona Backup for MongoDB uses
	PBMPassword   string `json:"pbmPassword"` // its password (stable across redeploys)
	// Password for the sample Keycloak OIDC users created at deploy (lab convenience).
	OIDCSamplePassword string `json:"oidcSamplePassword"`
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

	// The admin password comes from .env (re-read on every deploy). The internal
	// keyFile + PMM/PBM passwords are non-canvas secrets, still reused across redeploys.
	admin := envOr("MONGODB_ADMIN_PASSWORD", "admin_password")
	keyFile := ""
	pmmPass := ""
	pbmPass := ""
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s mongoSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil {
				if s.KeyFile != "" {
					keyFile = s.KeyFile
				}
				if s.PMMPassword != "" {
					pmmPass = s.PMMPassword
				}
				if s.PBMPassword != "" {
					pbmPass = s.PBMPassword
				}
			}
		}
	}
	if keyFile == "" {
		keyFile = genKeyFile()
	}
	if pmmPass == "" {
		pmmPass = envOr("PMM_PASSWORD", "pmm_password")
	}
	if pbmPass == "" {
		pbmPass = genSecret("MongoPBM!")
	}
	sec := mongoSecrets{AdminUser: "admin", AdminPassword: admin, KeyFile: keyFile, PMMUser: "pmm", PMMPassword: pmmPass, PBMUser: "pbm", PBMPassword: pbmPass}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := frame.PSMDBMajor
	if major == "" {
		major = "8.0"
	}
	pbmRepo := ""
	if frame.EnablePBM {
		pbmRepo = "PBM → SeaweedFS S3"
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
			EnablePBM: frame.EnablePBM, BackupRepo: pbmRepo,
			Ports: []int{mongoPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
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

		// ---- Phase 1 (parallel): container + install + keyFile + config + start mongod ----
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mongoPrepareNode(ctx, st, frame, n, hosts[n.ID], image, major, replSetOf(n), configDB, intranetID, intranetIP, domain, "", sec, nil, progs[n.ID]); err != nil {
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
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, frame.PMMNodeID, deployTimeout()); ok {
			a.mongoEnsurePMMUser(ctx, st, config[0], major, sec, progs[config[0].ID])
			for _, i := range shardIdx {
				a.mongoEnsurePMMUser(ctx, st, shards[i][0], major, sec, progs[shards[i][0].ID])
			}
			for _, n := range members {
				a.mongoRegisterPMM(ctx, st, n, frame.OS, pmmFQDN, pmmUser, pmmPass, frame.Label, sec, progs[n.ID])
			}
		}

		// ---- Percona Backup for MongoDB: configure pbm-agent on every mongod member
		// + register the SeaweedFS S3 store (best-effort; the cluster stays up if it
		// fails). The PBM user goes on the config RS (admin auth) + each shard RS
		// (localhost-exception path). Storage is set once, from a config server.
		if frame.EnablePBM {
			pr := progs[config[0].ID]
			pr.phase("Configuring Percona Backup for MongoDB", 96)
			if swCfg, swSec, err := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, deployTimeout()); err != nil {
				pr.logln("PBM setup skipped: " + err.Error())
			} else {
				a.mongoEnsurePBMUser(ctx, st, config[0], sec, progs[config[0].ID])
				for _, i := range shardIdx {
					a.mongoEnsurePBMUser(ctx, st, shards[i][0], sec, progs[shards[i][0].ID])
				}
				for _, n := range config {
					a.mongoSetupPBMAgent(ctx, st, n, frame.OS, sec, progs[n.ID])
				}
				for _, i := range shardIdx {
					for _, n := range shards[i] {
						a.mongoSetupPBMAgent(ctx, st, n, frame.OS, sec, progs[n.ID])
					}
				}
				a.mongoConfigurePBMStorage(ctx, st, config[0], frame, swCfg, swSec, sec, progs[config[0].ID])
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

	// The admin password comes from .env (re-read on every deploy). The internal
	// keyFile + PMM/PBM passwords are non-canvas secrets, still reused across redeploys.
	admin := envOr("MONGODB_ADMIN_PASSWORD", "admin_password")
	keyFile := ""
	pmmPass := ""
	pbmPass := ""
	for _, n := range members {
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
			var s mongoSecrets
			if json.Unmarshal(dep.Secrets, &s) == nil {
				if s.KeyFile != "" {
					keyFile = s.KeyFile
				}
				if s.PMMPassword != "" {
					pmmPass = s.PMMPassword
				}
				if s.PBMPassword != "" {
					pbmPass = s.PBMPassword
				}
			}
		}
	}
	if keyFile == "" {
		keyFile = genKeyFile()
	}
	if pmmPass == "" {
		pmmPass = envOr("PMM_PASSWORD", "pmm_password")
	}
	if pbmPass == "" {
		pbmPass = genSecret("MongoPBM!")
	}
	sec := mongoSecrets{AdminUser: "admin", AdminPassword: admin, KeyFile: keyFile, PMMUser: "pmm", PMMPassword: pmmPass, PBMUser: "pbm", PBMPassword: pbmPass}
	secJSON, _ := json.Marshal(sec)

	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := frame.PSMDBMajor
	if major == "" {
		major = "8.0"
	}
	pbmRepo := ""
	if frame.EnablePBM {
		pbmRepo = "PBM → SeaweedFS S3"
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
			EnablePBM: frame.EnablePBM, BackupRepo: pbmRepo,
			Ports: []int{mongoPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
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

		// Phase 1 (parallel): container + install + keyFile + config + start mongod.
		var wg sync.WaitGroup
		var mu sync.Mutex
		failed := false
		for _, n := range members {
			wg.Add(1)
			go func(n designNode) {
				defer wg.Done()
				if err := a.mongoPrepareNode(ctx, st, frame, n, hosts[n.ID], image, major, rs, "", intranetID, intranetIP, domain, "", sec, nil, progs[n.ID]); err != nil {
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
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, frame.PMMNodeID, deployTimeout()); ok {
			a.mongoEnsurePMMUser(ctx, st, members[0], major, sec, progs[members[0].ID])
			for _, n := range members {
				a.mongoRegisterPMM(ctx, st, n, frame.OS, pmmFQDN, pmmUser, pmmPass, rs, sec, progs[n.ID])
			}
		}

		// PBM: configure pbm-agent on every member + register the SeaweedFS S3 store
		// (best-effort). The PBM user is created on the primary (replicates to the set).
		if frame.EnablePBM {
			pr := progs[members[0].ID]
			pr.phase("Configuring Percona Backup for MongoDB", 96)
			if swCfg, swSec, err := a.waitSeaweedRunning(ctx, st.ID, frame.SeaweedFSNodeID, deployTimeout()); err != nil {
				pr.logln("PBM setup skipped: " + err.Error())
			} else {
				a.mongoEnsurePBMUser(ctx, st, members[0], sec, progs[members[0].ID])
				for _, n := range members {
					a.mongoSetupPBMAgent(ctx, st, n, frame.OS, sec, progs[n.ID])
				}
				a.mongoConfigurePBMStorage(ctx, st, members[0], frame, swCfg, swSec, sec, progs[members[0].ID])
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

	// The admin password and the Keycloak sample-user password come from .env (re-read on
	// every deploy); the internal PMM password is a non-canvas secret reused across redeploys.
	admin := envOr("MONGODB_ADMIN_PASSWORD", "admin_password")
	pmmPass := ""
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s mongoSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil {
			if s.PMMPassword != "" {
				pmmPass = s.PMMPassword
			}
		}
	}
	if pmmPass == "" {
		pmmPass = envOr("PMM_PASSWORD", "pmm_password")
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

	// Keycloak OIDC: build the issuer from the linked Keycloak node. MongoDB OIDC
	// requires an HTTPS issuer, so an SSL Keycloak gives https://<fqdn>:8443/realms/<realm>
	// (validation guarantees the linked Keycloak has SSL on). Keycloak's --hostname fixes
	// its token issuer to that exact string.
	realm, clientID, authClaim := mongoOIDCDefaults(n)
	oidcIssuer := ""
	sampleUsers := ""
	if n.EnableOIDC {
		kcHost, kcSSL := "", false
		for _, m := range doc.Nodes {
			if m.Type == "keycloak" && (m.ID == n.KeycloakNodeID || n.KeycloakNodeID == "") {
				kcHost = hosts[m.ID]
				kcSSL = m.GenerateCert
				break
			}
		}
		oidcIssuer = keycloakIssuer(fqdnOf(kcHost, domain), kcSSL) + "/realms/" + realm
		sampleUsers = "alice (dbadmins), bob (developers)"
		sec.OIDCSamplePassword = keycloakUserPassword()
	}

	cfg := mongoConfig{
		Cluster: "", Image: image, OS: n.OS, Arch: archOr(n.Arch), Role: "standalone",
		Hostname: host, FQDN: fqdnOf(host, domain), PSMDBMajor: major, Version: n.PSMDBVersion,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, MonitoredBy: monitoredBy,
		Ports:            []int{mongoPort},
		OIDCEnabled:      n.EnableOIDC,
		OIDCIssuer:       oidcIssuer,
		OIDCClientID:     clientID,
		OIDCRealm:        realm,
		OIDCAuthClaim:    authClaim,
		OIDCUseAuthClaim: n.OIDCUseAuthClaim,
		OIDCSampleUsers:  sampleUsers,
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID)
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

		// When OIDC is enabled, wait for Keycloak (with SSL) to be up, derive the HTTPS
		// issuer from its authoritative host, and render the mongod setParameter block.
		setParams := ""
		kcContainerID, kcAdminPW := "", ""
		if n.EnableOIDC {
			pr.phase("Waiting for Keycloak", 8)
			kcHost, kcSSL, kcID, kcPW, ok := a.waitKeycloak(ctx, st.ID, n.KeycloakNodeID, deployTimeout())
			if !ok {
				pr.fail("Keycloak node is not ready — cannot configure MONGODB-OIDC")
				return
			}
			oidcIssuer = keycloakIssuer(kcHost, kcSSL) + "/realms/" + realm
			kcContainerID, kcAdminPW = kcID, kcPW
			setParams = mongoOIDCSetParameter(oidcIssuer, clientID, authClaim, n.OIDCUseAuthClaim)
			// Persist the resolved issuer for the manager.
			cfg.OIDCIssuer = oidcIssuer
			cfgJSON, _ = json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		}

		// Data-at-rest encryption: mint this node's own mount + token on the linked OpenBao
		// node now — mongod writes its master key at the first start, so the token file and
		// the security.vault block have to be in place before mongoPrepareNode starts it.
		var mv *mongoVault
		if n.EnableVault {
			pr.phase("Preparing encryption keyring (OpenBao)", 10)
			v, info, err := a.prepareMongoVault(ctx, st, n, host, pr)
			if err != nil {
				pr.fail("configure encryption at rest: %v", err)
				return
			}
			mv = v
			cfg.Vault = info
			cfgJSON, _ = json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		}

		nn := n
		nn.Role = "standalone"
		if err := a.mongoPrepareNode(ctx, st, frame, nn, host, image, major, "", "", intranetID, intranetIP, domain, setParams, sec, mv, pr); err != nil {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)

		// OIDC: mongod fetches the issuer's JWKS over HTTPS, so trust the Intranet CA
		// (then restart mongod so it picks the new trust up).
		if n.EnableOIDC {
			pr.phase("Trusting Intranet CA", 60)
			id := a.containerOf(st.ID, n.ID)
			if caCrt, e := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt"); e == nil && len(caCrt) > 0 {
				if err := a.docker.CopyFile(ctx, id, "/etc/pki/ca-trust/source/anchors", "dbcanvas-ca.crt", 0o644, caCrt); err == nil {
					if err := a.runStep(ctx, id, mongoCATrustScript, nil, pr.logln); err != nil {
						pr.fail("trust Intranet CA: %v", err)
						return
					}
					pr.logln("Intranet CA trusted; mongod restarted")
				}
			}
		}

		if err := a.mongoCreateAdmin(ctx, st, n, sec, pr); err != nil {
			return
		}

		// Group-enumeration roles for Keycloak OIDC (useAuthorizationClaim=true): a
		// member of the Keycloak "developers"/"dbadmins" group maps to the matching
		// keycloak/<group> role. (When useAuthorizationClaim is off, users are created
		// in $external by name instead — see below.)
		if n.EnableOIDC && n.OIDCUseAuthClaim {
			pr.phase("Creating OIDC roles", 72)
			rdep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, rdep.ContainerID, mongoOIDCRolesScript,
				[]string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "ROLES_JS=" + mongoOIDCRolesJS()}, pr.logln); err != nil {
				pr.logln("create OIDC roles failed: " + err.Error())
			} else {
				pr.logln("OIDC group roles created (keycloak/developers, keycloak/dbadmins)")
			}
		}

		// Programmatically configure Keycloak (realm, OIDC client with device+auth-code
		// flows, group/audience mappers, groups, sample users) via kcadm in the Keycloak
		// container — replacing the manual console steps.
		if n.EnableOIDC && kcContainerID != "" {
			pr.phase("Configuring Keycloak (kcadm)", 76)
			env := []string{
				"KC_ADMIN_PW=" + kcAdminPW,
				"REALM=" + realm,
				"CLIENT_ID=" + clientID,
				"AUTH_CLAIM=" + authClaim,
				"USE_CLAIM=" + strconv.FormatBool(n.OIDCUseAuthClaim),
				"DOMAIN=" + domain,
				"SAMPLE_PW=" + sec.OIDCSamplePassword,
			}
			if err := a.runStep(ctx, kcContainerID, keycloakSetupScript, env, pr.logln); err != nil {
				pr.logln("Keycloak setup had issues (configure manually in the console): " + err.Error())
			} else {
				pr.logln("Keycloak realm/client/groups/users configured (realm " + realm + ")")
			}
		}

		// When authorizing by username ($external), create the matching MongoDB users for
		// the sample Keycloak users.
		if n.EnableOIDC && !n.OIDCUseAuthClaim {
			pr.phase("Creating $external users", 78)
			rdep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, rdep.ContainerID, mongoOIDCExternalUsersScript,
				[]string{"ADMIN_USER=" + sec.AdminUser, "ADMIN_PW=" + sec.AdminPassword, "USERS_JS=" + mongoOIDCExternalUsersJS(domain)}, pr.logln); err != nil {
				pr.logln("create $external users failed: " + err.Error())
			}
		}

		// PMM: create the monitoring user + register the standalone (no --cluster).
		if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, n.PMMNodeID, deployTimeout()); ok {
			a.mongoEnsurePMMUser(ctx, st, nn, major, sec, pr)
			a.mongoRegisterPMM(ctx, st, nn, frame.OS, pmmFQDN, pmmUser, pmmPass, "", sec, pr)
		}

		if n.LdapAuth || n.KerberosAuth {
			if err := a.applyDirectoryAuth(ctx, st, n, doc, a.containerOf(st.ID, n.ID), "psm", "", pr); err != nil {
				pr.logln("directory authentication skipped: " + err.Error())
			}
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
// vault (nil for none) wires data-at-rest encryption: its token file is staged and its
// security.vault block goes into mongod.conf *before* the first start — mongod establishes
// encryption only on an empty dbPath, so it cannot be turned on afterwards.
func (a *App) mongoPrepareNode(ctx context.Context, st Stack, frame designFrame, n designNode, host, image, major, replSet, configDB, intranetID, intranetIP, domain, setParams string, sec mongoSecrets, vault *mongoVault, pr *pxcProg) error {
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

	// Install pmm-client only when monitored by a PMM server (the frame carries the
	// association; a standalone node passes a synthetic frame with it set).
	if frame.PMMNodeID != "" {
		pmmInstall := pxcInstallPMMClientRHEL
		if debian {
			pmmInstall = pxcInstallPMMClientDebian
		}
		if err := a.runStep(ctx, id, pmmInstall, nil, pr.logln); err != nil {
			return pr.fail("install pmm-client: %v", err)
		}
		pr.logln("pmm-client installed")
	}

	// Install Percona Backup for MongoDB on every member of a sharded cluster /
	// replica set (not the standalone psm node) — always, so PBM backups can be
	// turned on later without a reinstall (same pattern as pmm-client).
	if frame.Type == "psmdb" || frame.Type == "psmrs" {
		pbmInstall := pbmInstallRHEL
		if debian {
			pbmInstall = pbmInstallDebian
		}
		if err := a.runStep(ctx, id, pbmInstall, nil, pr.logln); err != nil {
			return pr.fail("install percona-backup-mongodb: %v", err)
		}
		pr.logln("percona-backup-mongodb installed")
	}

	// Shared keyFile (same bytes everywhere) for internal cluster auth. Standalone
	// nodes have no keyFile (sec.KeyFile == "").
	if sec.KeyFile != "" {
		if err := a.docker.CopyFile(ctx, id, "/etc", "mongo.keyFile", 0o400, []byte(sec.KeyFile)); err != nil {
			return pr.fail("write keyFile: %v", err)
		}
	}

	// Per-node TLS material (CA-signed) when requested. Written to every node type
	// (config / shard / mongos / standalone); TLS is not auto-enabled — the manager's
	// TLS tab documents how to turn it on. Best-effort.
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 52)
		a.mongoApplyCert(ctx, id, intranetID, fqdnOf(host, domain), host, frame.CertTTLValue, frame.CertTTLUnit, pr.logln)
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
	// Encryption at rest: the token file must exist before mongod's first start, and mongod
	// refuses to read a token file anyone else can (0600, mongod-owned).
	vaultBlock := ""
	if vault != nil {
		if err := a.runStep(ctx, id, mongoVaultTokenScript, []string{"TOKEN=" + vault.Token}, pr.logln); err != nil {
			return pr.fail("stage vault token: %v", err)
		}
		vaultBlock = vault.Block
	}
	conf := mongodConfYAML(replSet, clusterRole, sec.KeyFile != "", setParams, vaultBlock)
	if err := a.docker.CopyFile(ctx, id, "/etc", "mongod.conf", 0o644, []byte(conf)); err != nil {
		return pr.fail("write mongod.conf: %v", err)
	}
	pr.phase("Starting mongod", 55)
	if err := a.runStep(ctx, id, mongoStartMongodScript, nil, pr.logln); err != nil {
		return pr.fail("start mongod: %v", err)
	}
	return nil
}

// mongoCertDir is where a node's per-node TLS material lives when GenerateCert is on.
const mongoCertDir = "/etc/mongo/certs"

// mongoApplyCert issues a per-node CA-signed certificate for a MongoDB node and writes
// it as a MongoDB-style PEM (certificate followed by its private key — the format
// mongod's certificateKeyFile wants) plus the CA cert, under mongoCertDir, owned by
// mongod. It deliberately does NOT enable TLS in mongod.conf: turning on cluster TLS is
// an all-members-at-once operator step (see the node's TLS docs in the manager), so this
// only makes the signed material available on the node. Best-effort — a failure is
// logged, never fatal (the node runs fine without TLS).
func (a *App) mongoApplyCert(ctx context.Context, containerID, intranetID, fqdn, host string, ttlValue int, ttlUnit string, logln func(string)) error {
	if logln == nil {
		logln = func(string) {}
	}
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		logln("per-node certificate skipped: " + err.Error())
		return err
	}
	caCrt, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		logln("per-node certificate skipped: read CA cert: " + err.Error())
		return err
	}
	caKey, err := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		logln("per-node certificate skipped: read CA key: " + err.Error())
		return err
	}
	if err := a.docker.PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		logln("per-node certificate skipped: stage CA: " + err.Error())
		return err
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{
		"FQDN=" + fqdn, "HOST=" + host, "DIR=" + mongoCertDir,
		"VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit,
	}
	if err := a.runStep(ctx, containerID, mongoCertScript, env, logln); err != nil {
		logln("per-node certificate FAILED: " + err.Error())
		return err
	}
	logln("per-node certificate written to " + mongoCertDir + "/server.pem (TLS not auto-enabled — see the node's TLS tab)")
	return nil
}

// mongoCertScript generates a CA-signed server certificate (CN=$FQDN, SAN the FQDN +
// short host, serverAuth+clientAuth) and writes $DIR/server.pem (cert then key), plus
// the CA at $DIR/ca.crt, owned by mongod. Uses the CA staged at /tmp by mongoApplyCert.
const mongoCertScript = `set -e
command -v openssl >/dev/null 2>&1 || { echo "openssl not installed in this image"; exit 1; }
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
install -d -m 0750 "$DIR"
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/server-key.pem" -out /tmp/mongo.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null 2>&1
cat >/tmp/mongo.ext <<EXT
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=DNS:$FQDN,DNS:$HOST
EXT
openssl x509 -req -in /tmp/mongo.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial \
  -out "$DIR/server-cert.pem" -extfile /tmp/mongo.ext -not_after "$END" >/dev/null 2>&1
cat "$DIR/server-cert.pem" "$DIR/server-key.pem" > "$DIR/server.pem"
cp -f "$CA" "$DIR/ca.crt"
id mongod >/dev/null 2>&1 && chown -R mongod:mongod "$DIR" 2>/dev/null || true
chmod 640 "$DIR/server.pem" "$DIR/server-key.pem"; chmod 644 "$DIR/ca.crt" "$DIR/server-cert.pem"
rm -f /tmp/mongo.csr /tmp/mongo.ext /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/dbca-ca.srl
openssl x509 -in "$DIR/server-cert.pem" -noout -enddate | sed 's/notAfter=//'`

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
		"PMM_FQDN=" + pmmFQDN, "PMM_USER=" + pmmUser, "PMM_PASS=" + pmmPass, "PMM_URL=" + pmmServerURL(pmmFQDN, pmmUser, pmmPass),
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
// only (no keyFile, for a standalone with no internal cluster auth). setParams, when
// non-empty, is an already-rendered "setParameter:" block (e.g. MONGODB-OIDC). vault,
// when non-empty, is the rendered security.vault sub-block (dbvault.go) and must go
// *inside* the security block — mongod reads encryption settings only at first start.
func mongodConfYAML(replSet, clusterRole string, useKeyFile bool, setParams, vault string) string {
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
	if vault != "" {
		b.WriteString(vault)
	}
	if replSet != "" {
		fmt.Fprintf(&b, "replication:\n  replSetName: %s\n", replSet)
	}
	if clusterRole != "" {
		fmt.Fprintf(&b, "sharding:\n  clusterRole: %s\n", clusterRole)
	}
	if setParams != "" {
		b.WriteString(setParams)
	}
	return b.String()
}

// mongoOIDCDefaults fills the OIDC fields of a psm node with the documented defaults.
func mongoOIDCDefaults(n designNode) (realm, clientID, authClaim string) {
	realm = strings.TrimSpace(n.OIDCRealm)
	if realm == "" {
		realm = "mongodb"
	}
	clientID = strings.TrimSpace(n.OIDCClientID)
	if clientID == "" {
		clientID = "mongodb-client"
	}
	authClaim = strings.TrimSpace(n.OIDCAuthClaim)
	if authClaim == "" {
		authClaim = "MyClaim"
	}
	return
}

// mongoOIDCIssues validates a PSMDB node's Keycloak-SSO selection: a linked SSL-enabled
// Keycloak (MONGODB-OIDC requires an HTTPS issuer), and no directory authentication
// alongside it. LDAP and Kerberos share one `# dbcanvas-dirauth` setParameter block
// (authenticationMechanisms PLAIN and/or GSSAPI) and so can be enabled together, but OIDC
// renders a setParameter block of its own (mongoOIDCSetParameter) — mongod.conf cannot carry
// two, so OIDC excludes both.
func mongoOIDCIssues(n designNode, keycloakIDs, keycloakSSL map[string]bool) []issue {
	if !n.EnableOIDC {
		return nil
	}
	if !keycloakIDs[n.KeycloakNodeID] {
		return []issue{{"error", "PSMDB node " + n.Label + " has Keycloak OIDC enabled but is not linked to a Keycloak node — add a Keycloak node and select it"}}
	}
	if !keycloakSSL[n.KeycloakNodeID] {
		return []issue{{"error", "PSMDB node " + n.Label + " uses Keycloak OIDC, which requires an HTTPS issuer — enable \"Use Intranet CA SSL\" on the Keycloak node"}}
	}
	if n.LdapAuth && n.KerberosAuth {
		return []issue{{"error", "PSMDB node " + n.Label + " has LDAP, Kerberos and Keycloak OIDC enabled — MongoDB cannot combine Keycloak OIDC with directory authentication (each needs its own mongod.conf setParameter block); turn off Keycloak SSO, or turn off both LDAP and Kerberos"}}
	}
	if n.LdapAuth {
		return []issue{{"error", "PSMDB node " + n.Label + " has both LDAP and Keycloak OIDC enabled — MongoDB cannot use both (each needs its own mongod.conf setParameter block); turn one off"}}
	}
	if n.KerberosAuth {
		return []issue{{"error", "PSMDB node " + n.Label + " has both Kerberos and Keycloak OIDC enabled — MongoDB cannot use both (each needs its own mongod.conf setParameter block); turn one off"}}
	}
	return nil
}

// mongoOIDCSetParameter renders the mongod.conf setParameter block enabling
// MONGODB-OIDC against the given Keycloak identity provider. The audience equals the
// clientId (per the Keycloak→PSMDB mapping); authNamePrefix is fixed to "keycloak"
// (the keycloak/<group> roles below depend on it). SCRAM mechanisms are kept so the
// admin/PMM/PBM users still authenticate.
func mongoOIDCSetParameter(issuer, clientID, authClaim string, useAuthClaim bool) string {
	m := map[string]any{
		"issuer":                issuer,
		"audience":              clientID,
		"authNamePrefix":        "keycloak",
		"clientId":              clientID,
		"useAuthorizationClaim": useAuthClaim,
		"supportsHumanFlows":    true,
	}
	if useAuthClaim {
		m["authorizationClaim"] = authClaim
	}
	prov, _ := json.Marshal(m)
	var b strings.Builder
	b.WriteString("setParameter:\n")
	b.WriteString("  authenticationMechanisms: SCRAM-SHA-1,SCRAM-SHA-256,MONGODB-OIDC\n")
	fmt.Fprintf(&b, "  oidcIdentityProviders: '[ %s ]'\n", string(prov))
	return b.String()
}

// mongoOIDCRolesJS returns the mongosh script that (idempotently) creates the group
// roles used for Keycloak group enumeration (useAuthorizationClaim=true): the
// keycloak/developers role grants readWriteAnyDatabase and keycloak/dbadmins grants
// root, matching the documented setup.
func mongoOIDCRolesJS() string {
	return `var a=db.getSiblingDB("admin");
try{a.createRole({role:"keycloak/developers",privileges:[],roles:["readWriteAnyDatabase"]})}catch(e){if(!/already exists/i.test(e.message))throw e}
try{a.createRole({role:"keycloak/dbadmins",privileges:[],roles:["root"]})}catch(e){if(!/already exists/i.test(e.message))throw e}`
}

// mongoOIDCExternalUsersJS returns the mongosh script that (idempotently) creates the
// $external MongoDB users matching the sample Keycloak users, for the username path
// (useAuthorizationClaim=false). The username is <authNamePrefix>/<email>.
func mongoOIDCExternalUsersJS(domain string) string {
	return fmt.Sprintf(`var e=db.getSiblingDB("$external");
function mk(u,r){try{e.createUser({user:u,roles:[{role:r,db:"admin"}]})}catch(x){if(!/already exists/i.test(x.message))throw x}}
mk("keycloak/alice@%s","keycloak/dbadmins");
mk("keycloak/bob@%s","keycloak/developers");`, domain, domain)
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

const mongoInstallRHEL = pinInstallRHEL + `set -e
percona-release enable -y "$PSMDB_REPO" >/dev/null 2>&1 || percona-release enable "$PSMDB_REPO" >/dev/null 2>&1 || percona-release setup -y "$PSMDB_REPO" >/dev/null 2>&1
pin_install $PKGS`

const mongoInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
percona-release enable -y "$PSMDB_REPO" >/dev/null 2>&1 || percona-release enable "$PSMDB_REPO" >/dev/null 2>&1 || percona-release setup -y "$PSMDB_REPO" >/dev/null 2>&1
apt-get update -qq >/dev/null
pin_install $PKGS`

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
# Percona MongoDB 6.0/7.0 ship a Type=forking mongod unit; because we run mongod
# with fork:false (foreground), the forking start job never sees the process
# daemonize and systemctl start times out even though mongod is serving. Force
# Type=simple (as PSMDB 8.0 already ships) so systemd tracks the foreground
# process directly. Harmless where the unit is already Type=simple.
mkdir -p /etc/systemd/system/mongod.service.d
cat > /etc/systemd/system/mongod.service.d/10-dbcanvas-nofork.conf <<'DROPIN'
[Service]
Type=simple
PIDFile=
DROPIN
systemctl daemon-reload 2>/dev/null || true
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

// mongoOIDCRolesScript creates the Keycloak group-enumeration roles (ROLES_JS) as the
// admin user. Idempotent (the JS swallows "already exists").
const mongoOIDCRolesScript = `set -e
mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval "$ROLES_JS"`

// mongoOIDCExternalUsersScript creates the sample $external users (USERS_JS) as admin.
const mongoOIDCExternalUsersScript = `set -e
mongosh --quiet --port 27017 -u "$ADMIN_USER" -p "$ADMIN_PW" --authenticationDatabase admin --eval "$USERS_JS"`

// mongoCATrustScript adds the staged Intranet CA to the system trust store (so mongod
// can validate the Keycloak HTTPS issuer's certificate when it fetches the JWKS) and
// restarts mongod so it picks up the new trust, waiting for it to answer a ping again.
const mongoCATrustScript = `set -e
update-ca-trust extract 2>/dev/null || update-ca-trust 2>/dev/null || true
systemctl restart mongod
OK=0
for i in $(seq 1 30); do
  mongosh --quiet --port 27017 --eval 'db.adminCommand({ping:1})' >/dev/null 2>&1 && { OK=1; break; }
  sleep 2
done
[ "$OK" = 1 ] || { echo "mongod did not come back after CA trust:"; tail -20 /var/log/mongo/mongod.log 2>/dev/null; exit 1; }`

// keycloakSetupScript runs inside the Keycloak container and (idempotently) creates the
// realm, the OIDC client (standard + device-auth flows), the group-membership +
// audience protocol mappers, the dbadmins/developers groups, and two sample users
// joined to those groups. Replaces the manual console walkthrough. Env: KC_ADMIN_PW,
// REALM, CLIENT_ID, AUTH_CLAIM, USE_CLAIM, DOMAIN, SAMPLE_PW.
const keycloakSetupScript = `set -e
KC=/opt/keycloak/bin/kcadm.sh
$KC config credentials --server http://localhost:8080 --realm master --user admin --password "$KC_ADMIN_PW" >/dev/null
# Realm
$KC get "realms/$REALM" >/dev/null 2>&1 || $KC create realms -s realm="$REALM" -s enabled=true -s sslRequired=external >/dev/null
# Public OIDC client with standard + device-authorization-grant flows.
CID=$($KC get clients -r "$REALM" -q clientId="$CLIENT_ID" --fields id --format csv --noquotes 2>/dev/null | tail -n1)
if [ -z "$CID" ]; then
  cat > /tmp/dbc-client.json <<JSON
{"clientId":"$CLIENT_ID","protocol":"openid-connect","enabled":true,"publicClient":true,"standardFlowEnabled":true,"directAccessGrantsEnabled":false,"attributes":{"oauth2.device.authorization.grant.enabled":"true"},"redirectUris":["http://localhost:27097/redirect"]}
JSON
  $KC create clients -r "$REALM" -f /tmp/dbc-client.json >/dev/null
  CID=$($KC get clients -r "$REALM" -q clientId="$CLIENT_ID" --fields id --format csv --noquotes | tail -n1)
fi
# Audience mapper (always) + group-membership mapper (when authorizing by group claim).
$KC get "clients/$CID/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q '^"\?mongodb-audience' || \
  $KC create "clients/$CID/protocol-mappers/models" -r "$REALM" -s name=mongodb-audience -s protocol=openid-connect -s protocolMapper=oidc-audience-mapper -s 'config."included.client.audience"='"$CLIENT_ID" -s 'config."access.token.claim"=true' >/dev/null
if [ "$USE_CLAIM" = "true" ]; then
  $KC get "clients/$CID/protocol-mappers/models" -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q 'group-membership-mapper' || \
    $KC create "clients/$CID/protocol-mappers/models" -r "$REALM" -s name=group-membership-mapper -s protocol=openid-connect -s protocolMapper=oidc-group-membership-mapper -s 'config."claim.name"='"$AUTH_CLAIM" -s 'config."full.path"=false' -s 'config."access.token.claim"=true' -s 'config."id.token.claim"=true' >/dev/null
  for g in dbadmins developers; do
    $KC get groups -r "$REALM" --fields name --format csv --noquotes 2>/dev/null | grep -q "\"\?$g\"\?" || $KC create groups -r "$REALM" -s name="$g" >/dev/null
  done
fi
# Sample users joined to their groups.
mkuser() {
  U=$1; FN=$2; LN=$3; GRP=$4
  $KC create users -r "$REALM" -s username="$U" -s enabled=true -s email="$U@$DOMAIN" -s emailVerified=true -s firstName="$FN" -s lastName="$LN" >/dev/null 2>&1 || true
  UID1=$($KC get users -r "$REALM" -q username="$U" --fields id --format csv --noquotes | tail -n1)
  [ -n "$UID1" ] || return 0
  $KC set-password -r "$REALM" --userid "$UID1" --new-password "$SAMPLE_PW" --temporary=false >/dev/null 2>&1 || true
  if [ "$USE_CLAIM" = "true" ]; then
    GID=$($KC get groups -r "$REALM" --fields id,name --format csv --noquotes | grep ",\?$GRP\"\?$" | head -n1 | cut -d, -f1 | tr -d '"')
    [ -n "$GID" ] && $KC update "users/$UID1/groups/$GID" -r "$REALM" -n >/dev/null 2>&1 || true
  fi
}
mkuser alice Alice Admin dbadmins
mkuser bob Bob Developer developers
echo "keycloak realm '$REALM' configured (client $CLIENT_ID, users alice/bob)"`

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
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin remove mongodb "$NODE" >/dev/null 2>&1 || true
CL=""; [ -n "$CLUSTER" ] && CL="--cluster=$CLUSTER"
pmm-admin add mongodb --username="$PMM_DB_USER" --password="$PMM_DB_PW" --host=127.0.0.1 --port=27017 $CL --enable-all-collectors "$NODE"`

const mongoPMMAddDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v pmm-admin >/dev/null 2>&1 || { percona-release setup -y pmm3-client >/dev/null 2>&1; apt-get update -qq >/dev/null; apt-get install -y -qq pmm-client >/dev/null; }
systemctl enable --now pmm-agent >/dev/null 2>&1 || true
pmm-admin config --force --server-insecure-tls --server-url="$PMM_URL" >/dev/null
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
