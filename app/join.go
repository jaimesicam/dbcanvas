package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// join.go — adding a member to a cluster that is already running.
//
// A cluster's provisioner builds the whole cluster: every member's container is created afresh
// and the cluster is formed from scratch. That is right for a first deploy and wrong for any
// other: a deploy of a running cluster with one new member used to recreate the members already
// there, with their data. Deploy now decides per member (handleDeployStack): a cluster none of
// whose members is built is provisioned as before; one with built and new members has only the
// new ones built, and each joins the running cluster the way that cluster takes a new member —
// with a copy of the data from the members already there. Nothing on the built members restarts.
//
// A cluster kind whose join is not written yet is refused — at validation, and by deploy — rather
// than rebuilt.

// joinKinds are the cluster kinds a new member can join while the others keep running.
var joinKinds = map[string]bool{
	"mysql":         true,
	"pxc":           true,
	"psmrs":         true,
	"patroni":       true,
	"repmgr":        true,
	"innodb":        true,
	"mysqlcerepl":   true,
	"mysqlceinnodb": true,
	"mariadbrepl":   true,
	"mariadbgalera": true,
}

// rebuildOnJoin are cluster kinds whose members hold nothing but configuration DBCanvas writes:
// a new member is still added by provisioning the whole cluster again, which loses nothing.
var rebuildOnJoin = map[string]bool{"proxysql": true}

// frameSplit sorts a cluster's members into those already built (a container, running or
// stopped) and those to build (never deployed, or whose provisioning failed).
func frameSplit(f designFrame, doc designDoc, existing map[string]Deployment) (built, fresh []designNode) {
	for _, n := range doc.Nodes {
		if n.FrameID != f.ID || n.Type != f.Type {
			continue
		}
		if d, ok := existing[n.ID]; ok && deployBuilt(d) {
			built = append(built, n)
		} else {
			fresh = append(fresh, n)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Label < fresh[j].Label })
	return built, fresh
}

// joinRefused is why a deploy cannot add fresh members to a running cluster of this kind, or "".
func joinRefused(f designFrame, built, fresh []designNode) string {
	if len(built) == 0 || len(fresh) == 0 || joinKinds[f.Type] || rebuildOnJoin[f.Type] {
		return ""
	}
	names := ""
	for i, n := range fresh {
		if i > 0 {
			names += ", "
		}
		names += n.Label
	}
	return fmt.Sprintf("%s is running and %s cannot join it yet: adding a member to a running cluster of this kind is not supported — remove %s from the canvas, or destroy the stack to build the cluster again", f.Label, names, map[bool]string{true: "them", false: "it"}[len(fresh) > 1])
}

// joinFrame builds fresh members of a running cluster and joins them to it.
func (a *App) joinFrame(st Stack, f designFrame, doc designDoc, fresh []designNode) {
	switch f.Type {
	case "mysql", "mysqlcerepl":
		a.joinMySQLFrame(st, f, doc, fresh)
	case "pxc":
		a.joinPXCFrame(st, f, doc, fresh)
	case "psmrs":
		a.joinMongoRSFrame(st, f, doc, fresh)
	case "patroni":
		a.joinPatroniFrame(st, f, doc, fresh)
	case "repmgr":
		a.joinRepmgrFrame(st, f, doc, fresh)
	case "innodb", "mysqlceinnodb":
		a.joinInnoDBFrame(st, f, doc, fresh)
	case "mariadbrepl":
		a.joinMariaDBFrame(st, f, doc, fresh)
	case "mariadbgalera":
		a.joinMariaDBGaleraFrame(st, f, doc, fresh)
	default:
		for _, n := range fresh {
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError})
			a.pxcNewProg(st.ID, n.ID).fail("%s", joinRefused(f, []designNode{{}}, []designNode{n}))
		}
	}
}

// joinPrimary finds the running cluster's primary among its built members, as the servers say.
func (a *App) joinPrimary(ctx context.Context, st Stack, f designFrame, doc designDoc) (swTopo, swMember, error) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	t := swTopo{Kind: rebuildKinds[f.Type], Frame: f}
	for _, n := range doc.Nodes {
		if n.FrameID != f.ID || n.Type != f.Type {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		t.Members = append(t.Members, swMember{Node: n, Dep: dep, Host: hosts[n.ID], FQDN: fqdnOf(hosts[n.ID], domain)})
	}
	states, pi := a.probeTopo(ctx, st, t)
	if pi < 0 {
		return t, swMember{}, fmt.Errorf("no running member of %s reports itself primary — there is nothing to copy from", f.Label)
	}
	m, _ := t.member(states[pi].NodeID)
	return t, m, nil
}

// joinMember is a fresh member as the copy code (rebuild.go) addresses it.
func (a *App) joinMember(st Stack, doc designDoc, n designNode) swMember {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	return swMember{Node: n, Dep: dep, Host: host, FQDN: fqdnOf(host, domain)}
}

// joinCopy gives a fresh member the primary's data and makes it replicate from it, with the
// same code as Rebuild from primary; its steps go to the member's deploy log.
func (a *App) joinCopy(ctx context.Context, st Stack, t swTopo, primary, target swMember, pr *pxcProg) error {
	t.Members = append(append([]swMember(nil), t.Members...), target)
	p := rebuildPlan{Supported: true, Kind: t.Kind, t: t, target: target, primary: primary, From: primary.Node.Label, FromID: primary.Node.ID}
	if t.Kind == "mysql" || t.Kind == "mariadb" {
		if reason := a.planSQLCopy(ctx, st, &p, true); reason != "" {
			return fmt.Errorf("%s", reason)
		}
	}
	pr.logln(p.Method)
	return a.runRebuild(ctx, st, p, &rebuildJob{logf: pr.logln})
}

// ---------------------------------------------------------------- Percona Server replication

// joinMySQLFrame adds fresh members to a running Percona Server replication cluster: each is
// built like a first-deploy member (container, packages, my.cnf, credentials baseline), then
// CLONEd from the current primary — or dumped from it where CLONE is not available — and set
// replicating, read-only, with semi-sync if the cluster uses it.
func (a *App) joinMySQLFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	// MySQL Community replication is the same cluster built from other packages.
	prepare := a.mysqlPrepareNode
	if frame.Type == "mysqlcerepl" {
		frame = mysqlceSyntheticFrame(frame)
		prepare = a.mysqlcePrepareNode
	}
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	major := psMajorOf(frame.PSMajor)
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
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
		orchestratedBy = fqdnOf(hosts[frame.OrchestratorNodeID], domain)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := mysqlConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: "secondary", Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mysqlServerID(host),
			PSVersion: frame.PSVersion, ReplMode: mysqlReplMode(frame.ReplMode), GTID: frame.GTID,
			ReadOnly: true, GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, OrchestratedBy: orchestratedBy, Ports: mysqlPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		// One at a time: each copy comes from the primary, and two at once double its load.
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			if prepare(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			pr.phase("Setting credentials", 60)
			if a.mysqlSetupBaseline(ctx, st, frame, n, "secondary", major, sec, pr) != nil {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			t, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Copying the data from "+primary.Node.Label, 70)
			if err := a.joinCopy(ctx, st, t, primary, a.joinMember(st, doc, n), pr); err != nil {
				pr.fail("join %s: %v", frame.Label, err)
				continue
			}
			a.persistConfigKey(st, n.ID, "sourceHost", primary.FQDN)
			if a.mysqlFinishMember(ctx, st, frame, doc, n, sec, intranetID, monitoredBy, false, pr) != nil {
				continue
			}
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a replica of " + primary.Node.Label)
		}
		if frame.OrchestratorNodeID != "" {
			var orch []pxcMember
			for _, n := range doc.Nodes {
				if n.FrameID == frame.ID && n.Type == frame.Type {
					if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && dep.ContainerID != "" {
						orch = append(orch, pxcMember{FQDN: fqdnOf(hosts[n.ID], domain), ContainerID: dep.ContainerID})
					}
				}
			}
			a.registerOrchestrator(ctx, st, frame.OrchestratorNodeID, orch, func(string) {})
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d mysql repl %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// ------------------------------------------------------------------------------------- PXC

// joinPXCFrame adds fresh members to a running PXC cluster: each is built with the whole cluster
// in its wsrep_cluster_address and started, and Galera gives it a full state transfer from a
// Synced member. An encrypted cluster's members carry one shared TLS certificate, which a joiner
// gets from a running member. The running members only have their configuration's cluster
// address updated — on disk, for their next restart; Galera's membership is live already.
func (a *App) joinPXCFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	var gcomm []string
	var running []designNode
	isFresh := map[string]bool{}
	for _, n := range fresh {
		isFresh[n.ID] = true
	}
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "pxc" || n.Role == "arbitrator" {
			continue
		}
		gcomm = append(gcomm, fqdnOf(hosts[n.ID], domain))
		if !isFresh[n.ID] {
			running = append(running, n)
		}
	}
	clusterAddr := strings.Join(gcomm, ",")
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := pxcConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Role: roleOf(n), Hostname: host, FQDN: fqdnOf(host, domain),
			ServerID: pxcServerID(host), PXCVersion: frame.PXCVersion, GTID: frame.GTID, GenerateCert: frame.GenerateCert,
			UseProxy: frame.UseProxy, MonitoredBy: monitoredBy, Ports: pxcPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		failAll := func(f string, args ...any) {
			for _, n := range fresh {
				progs[n.ID].fail(f, args...)
			}
		}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			failAll("%v", err)
			return
		}
		// The running cluster's own TLS material, when it encrypts its traffic.
		var clusterSSL []string
		if frame.EnableVault {
			src := ""
			for _, n := range running {
				if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && dep.State == DeployRunning && dep.ContainerID != "" {
					src = dep.ContainerID
					break
				}
			}
			if src == "" {
				failAll("no running member of %s to take the cluster's TLS certificate from", frame.Label)
				return
			}
			for _, kv := range []string{"CA=ca.pem", "SCERT=server-cert.pem", "SKEY=server-key.pem", "CCERT=client-cert.pem", "CKEY=client-key.pem"} {
				key, file, _ := strings.Cut(kv, "=")
				b, err := a.readContainerFile(withEngine(ctx, a.nodeEngine(st, frame.Type)), src, pxcSSLDir+"/"+file)
				if err != nil {
					failAll("read the cluster's %s: %v", file, err)
					return
				}
				clusterSSL = append(clusterSSL, key+"="+string(b))
			}
		}
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			if a.pxcPrepareNode(ctx, st, frame, n, hosts, domain, image, clusterAddr, intranetIP, sec, clusterSSL) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			if n.Role == "arbitrator" {
				pr.phase("Starting arbitrator (garbd)", 70)
				if a.pxcStartGarbd(ctx, st, n, frame, clusterAddr, pr) != nil {
					continue
				}
			} else {
				pr.phase("Joining "+frame.Label+" (SST)", 65)
				if a.pxcJoin(ctx, st, frame, n, hosts[n.ID], domain, intranetID, sec, pr) != nil {
					continue
				}
			}
			if frame.EnableVault && n.Role != "arbitrator" {
				pr.phase("Verifying keyring (OpenBao)", 90)
				dep, _ := a.store.GetDeployment(st.ID, n.ID)
				mount, _, _ := mysqlVaultMount(frame.PXCMajor, hosts[n.ID])
				if err := a.verifyMySQLVault(ctx, dep.ContainerID, frame.OS, frame.PXCMajor, mount, sec.RootPassword, false, pr); err != nil {
					pr.fail("verify keyring_vault: %v", err)
					continue
				}
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 92)
				pmmUser, pmmPass := "", ""
				if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, p
				}
				a.pxcRegisterPMM(ctx, st, n, frame, monitoredBy, pmmUser, pmmPass, sec, pr)
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label)
		}
		a.galeraAddressOnDisk(ctx, st, running, clusterAddr)
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d pxc %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// galeraAddressOnDisk rewrites wsrep_cluster_address in the running members' configuration, so a
// restart finds the members that joined since. Nothing is restarted: Galera already knows them.
func (a *App) galeraAddressOnDisk(ctx context.Context, st Stack, members []designNode, clusterAddr string) {
	for _, n := range members {
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		c := withEngine(ctx, a.depEngine(st, n.ID))
		a.engCtx(c).Exec(c, dep.ContainerID, []string{"sh", "-c", `for f in $(grep -rls '^wsrep_cluster_address' /etc/my.cnf /etc/my.cnf.d /etc/mysql 2>/dev/null); do
  sed -i "s|^wsrep_cluster_address.*|wsrep_cluster_address=gcomm://$ADDR|" "$f"
done`}, []string{"ADDR=" + clusterAddr})
	}
}

// --------------------------------------------------------------------------- MongoDB replica set

// joinMongoRSFrame adds fresh members to a running replica set: each is built like a first-deploy
// member — with the set's own keyFile and secrets, read from a running member, since a member with
// another keyFile cannot authenticate to the set — then the primary adds it to the set's config,
// and it copies every collection in an initial sync before it serves as a SECONDARY.
func (a *App) joinMongoRSFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	rs := sanitizeName(frame.Label)
	if rs == "" {
		rs = "rs"
	}
	var sec mongoSecrets
	var secJSON []byte
	searchAddr := ""
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != frame.Type {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || !deployBuilt(dep) || len(dep.Secrets) == 0 {
			continue
		}
		if json.Unmarshal(dep.Secrets, &sec) == nil && sec.KeyFile != "" {
			secJSON = dep.Secrets
			var cfg mongoConfig
			json.Unmarshal(dep.Config, &cfg)
			searchAddr = cfg.SearchHost
			break
		}
	}
	if secJSON == nil {
		for _, n := range fresh {
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError})
			a.pxcNewProg(st.ID, n.ID).fail("no built member of %s holds the replica set's keyFile — cannot join it", frame.Label)
		}
		return
	}
	// The admin password is re-read from .env on every deploy, as the first deploy does.
	sec.AdminPassword = envOr("MONGODB_ADMIN_PASSWORD", sec.AdminPassword)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := firstNonEmpty(frame.PSMDBMajor, "8.0")
	mcaAdminPW, mcaROPW := mcaPasswordsFor(doc, frame.ID)
	mcaSecretsFor(&sec, frame.MCACredentials, mcaAdminPW, mcaROPW)
	params := mongoMCASetParams(frame.MCACredentials, false, false)
	if frame.VectorSearch && searchAddr != "" {
		params = mergeSetParams(params, mongoSearchSetParams(searchAddr))
	}
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := mongoConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: "member", ReplSet: rs, Hostname: host, FQDN: fqdnOf(host, domain),
			PSMDBMajor: major, Version: frame.PSMDBVersion,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			EnablePBM: frame.EnablePBM, Ports: []int{mongoPort}, VectorSearch: frame.VectorSearch, SearchHost: searchAddr,
		}
		if frame.EnablePBM {
			cfg.BackupRepo = "PBM → SeaweedFS S3"
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			n.Role = "member"
			if a.mongoPrepareNode(ctx, st, frame, n, hosts[n.ID], image, major, rs, "", intranetID, intranetIP, domain, params, sec, nil, pr) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			t, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Adding to replica set "+rs, 70)
			addr := fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), mongoPort)
			if err := a.mongoAddMember(ctx, st, primary, addr); err != nil {
				pr.fail("add %s to %s: %v", addr, rs, err)
				continue
			}
			pr.logln(primary.Node.Label + " added " + addr + " to " + rs + "; initial sync from the set")
			pr.phase("Initial sync", 80)
			target := a.joinMember(st, doc, n)
			t.Members = append(t.Members, target)
			p := rebuildPlan{t: t, target: target}
			if err := a.waitHealthy(ctx, st, p, &rebuildJob{logf: pr.logln}, 60*time.Minute, func(r *liveRole) bool { return r.Role == "secondary" }); err != nil {
				pr.fail("initial sync: %v", err)
				continue
			}
			if pmmFQDN, pmmUser, pmmPass, ok := a.mongoWaitPMM(st, doc, frame.PMMNodeID, time.Minute); ok {
				a.mongoRegisterPMM(ctx, st, n, frame.OS, pmmFQDN, pmmUser, pmmPass, rs, sec, pr)
			}
			if frame.EnablePBM {
				a.mongoSetupPBMAgent(ctx, st, n, frame.OS, sec, pr)
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + rs)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d psmrs %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// mongoAddMember adds a member to the replica set, on its primary: the next _id, the set's
// config version moved on, majority-committed before it returns.
func (a *App) mongoAddMember(ctx context.Context, st Stack, primary swMember, addr string) error {
	c, ok := a.dbConnFor(st, primary.Node.ID)
	if !ok {
		return fmt.Errorf("%s is not running", primary.Node.Label)
	}
	cctx := withEngine(ctx, a.depEngine(st, primary.Node.ID))
	client, closer, err := a.mongoClientFor(cctx, c)
	if err != nil {
		return err
	}
	defer closer()
	var cur struct {
		Config bson.M `bson:"config"`
	}
	if err := client.Database("admin").RunCommand(cctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&cur); err != nil {
		return err
	}
	cfg := cur.Config
	members, _ := cfg["members"].(bson.A)
	next := int32(0)
	for _, m := range members {
		md, ok := m.(bson.M)
		if d, isD := m.(bson.D); isD {
			md, ok = d.Map(), true
		}
		if !ok {
			return fmt.Errorf("unexpected replica set member %T", m)
		}
		if h, _ := md["host"].(string); strings.EqualFold(h, addr) {
			return nil // already a member: a deploy that ran this far before
		}
		switch id := md["_id"].(type) {
		case int32:
			if id >= next {
				next = id + 1
			}
		case int64:
			if int32(id) >= next {
				next = int32(id) + 1
			}
		}
	}
	cfg["members"] = append(members, bson.M{"_id": next, "host": addr})
	switch v := cfg["version"].(type) {
	case int32:
		cfg["version"] = v + 1
	case int64:
		cfg["version"] = v + 1
	}
	return client.Database("admin").RunCommand(cctx, bson.D{{Key: "replSetReconfig", Value: cfg}}).Err()
}

// -------------------------------------------------------------------------------- Patroni

// joinPatroniFrame adds fresh members to a running Patroni cluster. Every member also runs an
// etcd member, so a joiner first joins etcd — announced with `etcdctl member add` on a running
// member, then started with initial-cluster-state existing — and then starts Patroni, which finds
// the leader in etcd and takes its base backup by itself (pgBackRest when configured).
func (a *App) joinPatroniFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	sec := pgFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	var members []designNode
	isFresh := map[string]bool{}
	for _, n := range fresh {
		isFresh[n.ID] = true
	}
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "patroni" {
			members = append(members, n)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	var etcdEndpoints []string
	for _, n := range members {
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("%s:%d", fqdnOf(hosts[n.ID], domain), etcdClientPort))
	}
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	backupRepo, backupStanza := "", ""
	if frame.UsePgBackRest {
		backupRepo, backupStanza = "pgbackrest → SeaweedFS S3", patroniStanza(frame.Label)
	}
	pgVector := ""
	if frame.PGVector {
		pgVector = pgVectorSource(frame.Type, frame.OS, frame.PGMajor)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := patroniConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Hostname: host, FQDN: fqdnOf(host, domain),
			PGMajor: ppgMajorOf(frame.PGMajor), PGVersion: frame.PGVersion, Role: "replica",
			EtcdEndpoints: etcdEndpoints, UsePgBackRest: frame.UsePgBackRest, BackupRepo: backupRepo, BackupStanza: backupStanza,
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			QuerySource: pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID), PGVector: pgVector, Ports: patroniPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		failAll := func(f string, args ...any) {
			for _, n := range fresh {
				progs[n.ID].fail(f, args...)
			}
		}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			failAll("%v", err)
			return
		}
		var swCfg seaweedConfig
		var swSec seaweedSecrets
		if frame.UsePgBackRest {
			c, s, werr := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
			if werr != nil {
				failAll("%v", werr)
				return
			}
			swCfg, swSec = c, s
		}
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			if a.patroniPrepareNode(ctx, st, frame, n, members, hosts, domain, image, etcdEndpoints, intranetID, intranetIP, sec, swCfg, swSec) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			// Announce the new etcd member to the running ones, from a running member.
			host, fqdn := hosts[n.ID], fqdnOf(hosts[n.ID], domain)
			peer := ""
			for _, m := range members {
				if isFresh[m.ID] {
					continue
				}
				if dep, e := a.store.GetDeployment(st.ID, m.ID); e == nil && dep.State == DeployRunning && dep.ContainerID != "" {
					peer = dep.ContainerID
					break
				}
			}
			if peer == "" {
				pr.fail("no running member of %s to join etcd through", frame.Label)
				continue
			}
			pr.phase("Joining etcd", 60)
			if err := a.runStep(ctx, peer, `set -e
export ETCDCTL_API=3
E=http://127.0.0.1:2379
# This node's container and etcd data are new: a member already registered at its URL — from
# an earlier attempt, started or not — is a member whose data is gone, which etcd will not take
# back. It is removed and announced again.
etcdctl --endpoints=$E member list | awk -F', ' -v u="http://$FQDN:2380" '$4==u {print $1}' | xargs -r -n1 etcdctl --endpoints=$E member remove
etcdctl --endpoints=$E member add "$NAME" --peer-urls="http://$FQDN:2380"`, []string{"NAME=" + host, "FQDN=" + fqdn}, pr.logln); err != nil {
				pr.fail("etcd member add: %v", err)
				continue
			}
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, dep.ContainerID, `sed -i 's/^initial-cluster-state: new/initial-cluster-state: existing/' "$CONF"`, []string{"CONF=" + etcdConfPath(frame.OS)}, pr.logln); err != nil {
				pr.fail("etcd config: %v", err)
				continue
			}
			if err := a.runStep(ctx, dep.ContainerID, patroniEtcdStartScript, nil, pr.logln); err != nil {
				pr.fail("start etcd: %v", err)
				continue
			}
			if err := a.patroniWaitEtcd(ctx, st, []designNode{n}, 5*time.Minute); err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Starting Patroni (base backup from the leader)", 72)
			if err := a.runStep(ctx, dep.ContainerID, patroniStartScript, nil, pr.logln); err != nil {
				pr.fail("start patroni: %v", err)
				continue
			}
			target := a.joinMember(st, doc, n)
			p := rebuildPlan{t: swTopo{Kind: "patroni", Frame: frame, Members: []swMember{target}}, target: target}
			if err := a.waitHealthy(ctx, st, p, &rebuildJob{logf: pr.logln}, 60*time.Minute, func(r *liveRole) bool { return r.Role == "replica" && len(r.Problems) == 0 }); err != nil {
				pr.fail("replica did not come up: %v", err)
				continue
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 94)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr)
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a replica")
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d patroni %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// --------------------------------------------------------------------------------- repmgr

// joinRepmgrFrame adds fresh standbys to a running repmgr cluster: built like a first-deploy
// member, given SSH to the others (for switchovers), cloned from the current primary with `repmgr
// standby clone`, registered, and running repmgrd. repmgr's node_id must be unique, so a joiner
// takes the next one after the highest already in use rather than its place in the label order.
func (a *App) joinRepmgrFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	sec := pgFamilySecrets()
	sec.ReplUser = "repmgr"
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	major := ppgMajorOf(frame.PGMajor)
	var members []designNode
	maxID := 0
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != "repmgr" {
			continue
		}
		members = append(members, n)
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && deployBuilt(dep) {
			var c repmgrConfig
			if json.Unmarshal(dep.Config, &c) == nil && c.NodeID > maxID {
				maxID = c.NodeID
			}
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	backupRepo, backupEngine := "", repmgrBackupEngine(frame)
	switch backupEngine {
	case "barman":
		backupRepo = "Barman → SeaweedFS S3"
	case "pgbackrest":
		backupRepo = "pgBackRest → SeaweedFS S3"
	}
	pgVector := ""
	if frame.PGVector {
		pgVector = pgVectorSource(frame.Type, frame.OS, major)
	}
	nodeIDs := map[string]int{}
	for i, n := range fresh {
		nodeIDs[n.ID] = maxID + 1 + i
		host := hosts[n.ID]
		idx := 0
		for k, m := range members {
			if m.ID == n.ID {
				idx = k
			}
		}
		cfg := repmgrConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Hostname: host, FQDN: fqdnOf(host, domain),
			PGMajor: major, PGVersion: frame.PGVersion, Role: "standby", NodeID: nodeIDs[n.ID],
			UseBarman: frame.UseBarman, BackupRepo: backupRepo, BackupEngine: backupEngine, BackupStanza: repmgrStanza(frame.Label),
			Service: pgServiceName(frame.OS, major), DataDir: pgDataDir(frame.OS, major),
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy,
			QuerySource: pgQuerySourceFor(frame.Type, frame.PGQuerySource, frame.PMMNodeID), PGVector: pgVector,
			Ports: []int{patroniPGPort}, RepmgrConf: pgRepmgrConfPath(major), RepmgrBin: pgBinDir(frame.OS, major) + "/repmgr",
			Peers: repmgrPeersOf(members, hosts, domain, idx),
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		failAll := func(f string, args ...any) {
			for _, n := range fresh {
				progs[n.ID].fail(f, args...)
			}
		}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			failAll("%v", err)
			return
		}
		var swCfg seaweedConfig
		var swSec seaweedSecrets
		if repmgrUsesBackups(frame) {
			c, s, werr := a.waitSeaweedBucket(ctx, st.ID, frame.SeaweedFSNodeID, frame.SeaweedFSBucket, deployTimeout())
			if werr != nil {
				failAll("%v", werr)
				return
			}
			swCfg, swSec = c, s
		}
		var built []designNode
		for _, n := range fresh {
			if a.repmgrPrepareNode(ctx, st, frame, n, nodeIDs[n.ID], image, intranetIP, domain, sec, swCfg, swSec) == nil {
				built = append(built, n)
			}
		}
		if len(built) == 0 {
			return
		}
		a.reconcileStackDNS(ctx, st.ID)
		// SSH between every member — the joiners and the running ones — for switchovers.
		fqdns := map[string]string{}
		for _, n := range members {
			fqdns[n.ID] = fqdnOf(hosts[n.ID], domain)
		}
		var sshMembers []designNode
		for _, n := range members {
			if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && dep.ContainerID != "" {
				sshMembers = append(sshMembers, n)
			}
		}
		a.repmgrWireSSH(ctx, st, frame, sshMembers, fqdns, major, func(nid string) *pxcProg {
			if p, ok := progs[nid]; ok {
				return p
			}
			return a.sideProg(st.ID, nid, nodeLabel(doc, nid), progs[built[0].ID])
		})
		for _, n := range built {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Cloning standby from "+primary.Node.Label, 60)
			if a.repmgrSetupStandby(ctx, st, frame, n, major, primary.FQDN, sec, pr) != nil {
				continue
			}
			pr.phase("Starting repmgrd", 82)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if err := a.runStep(ctx, dep.ContainerID, repmgrdStartScript, []string{"MAJOR=" + major, "CONF=" + pgRepmgrConfPath(major)}, pr.logln); err != nil {
				pr.logln("repmgrd start failed (failover disabled): " + err.Error())
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 95)
				a.patroniRegisterPMM(ctx, st, n, frame, doc, sec, pr)
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a standby of " + primary.Node.Label)
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d repmgr %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// --------------------------------------------------------------- InnoDB Cluster / Group Replication

// innodbAddInstanceScript adds one instance to a running InnoDB Cluster with MySQL Shell, on its
// primary: configureInstance, then addInstance with clone recovery, as the first deploy does.
const innodbAddInstanceScript = `set -e
ADMIN="$CLUSTER_USER:$CLUSTER_PW@localhost:3306"
sh_run() {
  if timeout "$1" mysqlsh --uri "$ADMIN" --js -e "$2" >/tmp/sh.log 2>&1; then cat /tmp/sh.log; return 0; fi
  echo "MySQL Shell step failed:"; grep -iE 'ERROR|exception|Dba\.|Cluster\.' /tmp/sh.log | tail -4 || true; return 1
}
SHVER=$(mysqlsh --version 2>/dev/null | grep -oE 'Ver [0-9]+\.[0-9]+' | head -1 | cut -d' ' -f2)
case "$SHVER" in
  8.0|5.7|"") CFGOPT="{interactive:false, restart:false}" ;;
  *)          CFGOPT="{restart:false}" ;;
esac
# The joiner's container is new. If an earlier attempt left it in the cluster's metadata but it
# is not ONLINE, that entry is for data that no longer exists: remove it before adding again.
# The metadata names an instance by its report_host — the short name here, not the FQDN.
SHORT=${NEW%%.*}
timeout 120 mysqlsh --uri "$ADMIN" --js -e "var c=dba.getCluster('$CLUSTER'); var topo=c.status().defaultReplicaSet.topology; for (var k in topo) { if ((k == '$NEW:3306' || k == '$SHORT:3306') && topo[k].status != 'ONLINE') { c.removeInstance(k, {force:true}); print('removed the stale ' + k + ' entry'); } }" 2>&1 | grep -v '^WARNING' || true
sh_run 300 "dba.configureInstance('$CLUSTER_USER:$CLUSTER_PW@$NEW:3306', $CFGOPT);"
sh_run 1800 "var c=dba.getCluster('$CLUSTER'); try { c.addInstance('$CLUSTER_USER:$CLUSTER_PW@$NEW:3306', {recoveryMethod:'clone'}); } catch (e) { if (String(e).indexOf('already') < 0) throw e; }"
echo "$NEW added to $CLUSTER"`

// innodbCloneJoinScript joins a raw Group Replication member by clone: the plugin installed,
// the clone threshold at 1 so distributed recovery copies a donor instead of replaying binlogs
// it may no longer have, and GR started. The server restarts on the copy and rejoins by itself.
const innodbCloneJoinScript = `mysql --force <<'SQL'
SET GLOBAL super_read_only=OFF;
INSTALL PLUGIN clone SONAME 'mysql_clone.so';
SET GLOBAL group_replication_clone_threshold=1;
START GROUP_REPLICATION;
SQL
true`

// joinInnoDBFrame adds fresh members to a running InnoDB Cluster (MySQL Shell addInstance with
// clone recovery, on the primary) or raw Group Replication group (clone recovery from a donor).
// Either way the joiner is built as on a first deploy, and the running members only learn its
// address — addInstance tells them; for raw GR their persisted group seeds are updated.
func (a *App) joinInnoDBFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	// MySQL Community InnoDB Cluster is the same cluster built from other packages.
	prepare := a.innodbPrepareNode
	serverID := innodbServerID
	if frame.Type == "mysqlceinnodb" {
		frame = mysqlceSyntheticFrame(frame)
		prepare, serverID = a.mysqlceInnoDBPrepareNode, mysqlServerID
	}
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	mode := innodbReplMode(frame.ReplMode)
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	groupName := ""
	var members, running []designNode
	isFresh := map[string]bool{}
	for _, n := range fresh {
		isFresh[n.ID] = true
	}
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != frame.Type {
			continue
		}
		members = append(members, n)
		if !isFresh[n.ID] {
			running = append(running, n)
			if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil {
				var c innodbConfig
				if json.Unmarshal(dep.Config, &c) == nil && c.GroupName != "" {
					groupName = c.GroupName
				}
			}
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Label < members[j].Label })
	var seeds []string
	for _, n := range members {
		seeds = append(seeds, fqdnOf(hosts[n.ID], domain)+":"+strconv.Itoa(grCommPort))
	}
	seedList := strings.Join(seeds, ",")
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	if groupName == "" {
		for _, n := range fresh {
			a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError})
			a.pxcNewProg(st.ID, n.ID).fail("the running members of %s do not record their group name — cannot join it", frame.Label)
		}
		return
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := innodbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			PDPSRepo: frame.PDPSRepo, ReplMode: mode, Hostname: host, FQDN: fqdnOf(host, domain),
			ServerID: serverID(host), GroupName: groupName,
			Router: frame.MySQLRouter, GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, Ports: []int{mysqlBackPort, grCommPort, routerRWPort, routerROPort},
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		joined := false
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			if prepare(ctx, st, frame, n, hosts[n.ID], image, groupName, seedList, intranetIP, domain, sec, pr) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			t, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			fqdn := fqdnOf(hosts[n.ID], domain)
			dep, _ := a.store.GetDeployment(st.ID, n.ID)
			if mode == "innodbcluster" {
				pr.phase("Adding to InnoDB Cluster (clone from "+primary.Node.Label+")", 65)
				env := []string{"CLUSTER=" + sanitizeName(frame.Label), "CLUSTER_USER=" + sec.ClusterUser, "CLUSTER_PW=" + sec.ClusterPassword, "NEW=" + fqdn}
				if err := a.runStep(ctx, primary.Dep.ContainerID, innodbAddInstanceScript, env, pr.logln); err != nil {
					pr.fail("addInstance: %v", err)
					continue
				}
			} else {
				pr.phase("Joining the group (clone from a donor)", 65)
				// Every possible donor needs the clone plugin.
				for _, m := range t.Members {
					a.sqlAs(ctx, st, m, "mysql", sec.RootPassword, `SET @ro := @@GLOBAL.super_read_only;
SET GLOBAL super_read_only=OFF;
`+installPluginSQL("clone", "mysql_clone.so")+`;
SET GLOBAL super_read_only=@ro`, false)
				}
				if err := a.runStep(ctx, dep.ContainerID, innodbCloneJoinScript, nil, pr.logln); err != nil {
					pr.fail("start group replication: %v", err)
					continue
				}
			}
			target := a.joinMember(st, doc, n)
			t.Members = append(t.Members, target)
			p := rebuildPlan{t: t, target: target}
			if err := a.waitHealthy(ctx, st, p, &rebuildJob{logf: pr.logln}, 30*time.Minute, func(r *liveRole) bool {
				return r.Role == "secondary" && len(r.Problems) == 0
			}); err != nil {
				pr.fail("join %s: %v", frame.Label, err)
				continue
			}
			if mode != "innodbcluster" {
				a.sqlAs(ctx, st, target, "mysql", sec.RootPassword, "SET GLOBAL group_replication_clone_threshold=9223372036854775807", true)
			}
			if frame.MySQLRouter {
				pr.phase("Bootstrapping MySQL Router", 85)
				if a.innodbSetupRouter(ctx, st, frame, n, hosts, domain, mode, sec, pr) != nil {
					continue
				}
				var cfg innodbConfig
				dep, _ = a.store.GetDeployment(st.ID, n.ID)
				json.Unmarshal(dep.Config, &cfg)
				cfg.RWPort, cfg.ROPort = a.readInnoDBRouterPorts(ctx, dep.ContainerID, n.ExportEnabled)
				a.persistConfigKeys(st, n.ID, map[string]any{"rwPort": cfg.RWPort, "roPort": cfg.ROPort})
			}
			if frame.GenerateCert {
				pr.phase("Issuing certificate", 92)
				if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdn, mysqlUnit(frame.OS), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
					pr.fail("%v", err)
					continue
				}
			}
			if frame.PMMNodeID != "" {
				pr.phase("Registering with PMM", 96)
				pmmUser, pmmPass := "", ""
				if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
					pmmUser, pmmPass = u, p
				}
				a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label))
			}
			pr.phase("Running", 100)
			pr.p.Message = "provisioned"
			pr.save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a secondary")
			joined = true
		}
		// Raw GR: the running members' persisted seeds name the new members for their next
		// restart, and their static Router configuration routes to them.
		if joined && mode != "innodbcluster" {
			var any *pxcProg
			for _, n := range fresh {
				any = progs[n.ID]
			}
			for _, m := range running {
				dep, err := a.store.GetDeployment(st.ID, m.ID)
				if err != nil || dep.State != DeployRunning {
					continue
				}
				mm := swMember{Node: m, Dep: dep}
				a.sqlAs(ctx, st, mm, "mysql", sec.RootPassword, fmt.Sprintf("SET PERSIST group_replication_group_seeds='%s'", seedList), true)
				if frame.MySQLRouter {
					a.innodbSetupRouter(ctx, st, frame, m, hosts, domain, mode, sec, a.sideProg(st.ID, m.ID, m.Label, any))
				}
			}
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d innodb %s: %d member(s) joined (%s)", st.ID, frame.Label, len(fresh), mode)
	}()
}

// ------------------------------------------------------------------------- MariaDB

// mariadbFinishMember is a MariaDB member's last steps, as the first deploy has them.
func (a *App) mariadbFinishMember(ctx context.Context, st Stack, frame designFrame, doc designDoc, n designNode, sec pxcSecrets, intranetID, monitoredBy string, pr *pxcProg) error {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	dep, _ := a.store.GetDeployment(st.ID, n.ID)
	a.engCtx(ctx).CopyFile(ctx, dep.ContainerID, "/root", ".my.cnf", 0o600, pxcRootMyCnf(sec))
	if frame.GenerateCert {
		pr.phase("Issuing certificate", 90)
		if err := a.pxcApplyCert(ctx, dep.ContainerID, intranetID, fqdnOf(hosts[n.ID], domain), mariadbUnit(), frame.OS, frame.CertTTLValue, frame.CertTTLUnit, pr.logln, false); err != nil {
			return pr.fail("%v", err)
		}
	}
	if frame.PMMNodeID != "" {
		pr.phase("Registering with PMM", 95)
		pmmUser, pmmPass := "", ""
		if _, u, p, ok := a.pmmServerFor(st, doc, frame.PMMNodeID); ok {
			pmmUser, pmmPass = u, p
		}
		a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label))
	}
	pr.phase("Running", 100)
	pr.p.Message = "provisioned"
	pr.save()
	a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
	return nil
}

// joinMariaDBFrame adds fresh replicas to a running MariaDB replication cluster: built and
// baselined like a first-deploy replica, then a consistent mariadb-dump of the current primary
// loaded into it, and replication started from the dump's position (by GTID where the cluster's
// replicas position by GTID).
func (a *App) joinMariaDBFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	gtidDomain := mariadbGTIDDomain(frame.Label)
	monitoredBy, orchestratedBy := "", ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	if frame.OrchestratorNodeID != "" {
		orchestratedBy = fqdnOf(hosts[frame.OrchestratorNodeID], domain)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := mariadbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: "secondary", Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mariadbServerID(host),
			MariaDBMajor: mariadbMajorOf(frame.MariaDBMajor), MariaDBVersion: frame.MariaDBVersion,
			ReplMode: mariadbReplMode(frame.ReplMode), GTID: frame.GTID, GTIDDomainID: gtidDomain,
			ReadOnly: true, GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy,
			MonitoredBy: monitoredBy, OrchestratedBy: orchestratedBy, Ports: mariadbPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}
	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			host := hosts[n.ID]
			cnf := mariadbReplCnf(frame.OS, host, mariadbServerID(host), gtidDomain, frame.GTID)
			if a.mariadbPrepareNode(ctx, st, frame, n, host, image, intranetIP, domain, cnf, false) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			pr.phase("Setting credentials", 60)
			if a.mariadbSetupBaseline(ctx, st, frame, n, "secondary", sec, pr) != nil {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			t, primary, err := a.joinPrimary(pctx, st, frame, doc)
			cancel()
			if err != nil {
				pr.fail("%v", err)
				continue
			}
			pr.phase("Copying the data from "+primary.Node.Label, 70)
			if err := a.joinCopy(ctx, st, t, primary, a.joinMember(st, doc, n), pr); err != nil {
				pr.fail("join %s: %v", frame.Label, err)
				continue
			}
			a.persistConfigKey(st, n.ID, "sourceHost", primary.FQDN)
			if a.mariadbFinishMember(ctx, st, frame, doc, n, sec, intranetID, monitoredBy, pr) != nil {
				continue
			}
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label + " as a replica of " + primary.Node.Label)
		}
		if frame.OrchestratorNodeID != "" {
			var orch []pxcMember
			for _, n := range doc.Nodes {
				if n.FrameID == frame.ID && n.Type == frame.Type {
					if dep, e := a.store.GetDeployment(st.ID, n.ID); e == nil && dep.ContainerID != "" {
						orch = append(orch, pxcMember{FQDN: fqdnOf(hosts[n.ID], domain), ContainerID: dep.ContainerID})
					}
				}
			}
			a.registerOrchestrator(ctx, st, frame.OrchestratorNodeID, orch, func(string) {})
		}
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d mariadb repl %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}

// joinMariaDBGaleraFrame adds fresh members to a running MariaDB Galera cluster: built with the
// whole cluster in wsrep_cluster_address and started, so a Synced member gives it a state
// transfer; the running members' configuration learns the address for their next restart.
func (a *App) joinMariaDBGaleraFrame(st Stack, frame designFrame, doc designDoc, fresh []designNode) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	sec := mysqlFamilySecrets()
	secJSON, _ := json.Marshal(sec)
	image := pxcImage(frame.OS, frame.OSVersion, frame.Arch)
	isFresh := map[string]bool{}
	for _, n := range fresh {
		isFresh[n.ID] = true
	}
	var memberHosts []string
	var running []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == frame.Type {
			memberHosts = append(memberHosts, hosts[n.ID])
			if !isFresh[n.ID] {
				running = append(running, n)
			}
		}
	}
	sort.Strings(memberHosts)
	clusterAddr := mariadbGaleraClusterAddr(memberHosts, domain)
	monitoredBy := ""
	if frame.PMMNodeID != "" {
		monitoredBy = fqdnOf(hosts[frame.PMMNodeID], domain)
	}
	for _, n := range fresh {
		host := hosts[n.ID]
		cfg := mariadbConfig{
			Cluster: frame.Label, Image: image, OS: frame.OS, Arch: archOr(frame.Arch),
			Role: "member", Hostname: host, FQDN: fqdnOf(host, domain), ServerID: mariadbServerID(host),
			MariaDBMajor: mariadbMajorOf(frame.MariaDBMajor), MariaDBVersion: frame.MariaDBVersion,
			ClusterAddress: clusterAddr, SSTMethod: "mariabackup",
			GenerateCert: frame.GenerateCert, UseProxy: frame.UseProxy, MonitoredBy: monitoredBy, Ports: mariadbPorts,
		}
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})
	}
	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, frame.Type))
	go func() {
		defer endScope()
		progs := map[string]*pxcProg{}
		for _, n := range fresh {
			progs[n.ID] = a.pxcNewProg(st.ID, n.ID)
			a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
			progs[n.ID].phase("Waiting for Intranet to be ready", 5)
		}
		intranetID, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			for _, n := range fresh {
				progs[n.ID].fail("%v", err)
			}
			return
		}
		for _, n := range fresh {
			pr := progs[n.ID]
			markNodeAction(st.ID, n.ID)
			cnf := mariadbGaleraCnf(frame, hosts[n.ID], domain, clusterAddr, sec)
			if a.mariadbPrepareNode(ctx, st, frame, n, hosts[n.ID], image, intranetIP, domain, cnf, true) != nil {
				continue
			}
			a.reconcileStackDNS(ctx, st.ID)
			pr.phase("Joining "+frame.Label+" (SST)", 70)
			if a.mariadbGaleraJoin(ctx, st, frame, n, sec, pr) != nil {
				continue
			}
			if a.mariadbFinishMember(ctx, st, frame, doc, n, sec, intranetID, monitoredBy, pr) != nil {
				continue
			}
			markNodeAction(st.ID, n.ID)
			pr.logln("joined " + frame.Label)
		}
		a.galeraAddressOnDisk(ctx, st, running, strings.TrimPrefix(clusterAddr, "gcomm://"))
		a.reconcileStackDNS(ctx, st.ID)
		log.Printf("stack %d mariadb galera %s: %d member(s) joined", st.ID, frame.Label, len(fresh))
	}()
}
