package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Performance Sim (Type=="perfsim"): a dashboard of stressors that make a linked
// MySQL-family or PostgreSQL server or cluster misbehave on purpose —
// replication lag from a large transaction, a no-op ALTER or VACUUM FULL, a
// locked, stopped, paused or delayed replica — with a timeline that shows what
// each one does to the cluster and to an application running against it. Runs DBCanvas's own
// dbcanvas-perfsim:v<release> image (built by `make perfsim-image`).
//
// Everything about the stressors is chosen on the sim's dashboard while it runs;
// the node itself has no options beyond its association line. What DBCanvas
// does here is the one thing the dashboard cannot: find every member of the
// linked cluster, with an account allowed to lock and reconfigure them. Every
// other sim needs one write endpoint; this one also needs each replica, because
// that is where most of its stressors act.
//
// The workload connects through the cluster's write endpoint with the account
// Stock Market Sim uses there: the application user on MySQL (a ProxySQL in
// front of the cluster only knows that user), the superuser on PostgreSQL. The
// stressors connect to each member directly as the MySQL network superuser
// admin@'%', or the PostgreSQL superuser.
const perfSimPort = 8096

var perfSimImage = "dbcanvas-perfsim:" + imageRelease()

// perfSimTargets are the canvas types a Performance Sim node can be linked to:
// MySQL-family and PostgreSQL standalone servers and cluster frames (Patroni,
// repmgr, and Spock, multi-master with logical replication), and the routers
// and pools in front of one, a MongoDB server, replica set or sharded cluster, and a
// Valkey server or Valkey Cluster.
var perfSimTargets = map[string]bool{
	"ps": true, "mariadb": true, "mysqlce": true,
	"pxc": true, "mysql": true, "innodb": true, "mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true,
	"pg": true, "patroni": true, "repmgr": true, "spock": true,
	"psm": true, "psmrs": true, "psmdb": true,
	"valkey": true, "valkeycluster": true,
	"haproxy": true, "proxysql": true, "pgbouncer": true,
	"k3d": true, // the database its operator runs; see perfsim_k8s.go
}

// perfSimConfig is the non-secret profile shown for a deployed node.
type perfSimConfig struct {
	Image      string   `json:"image"`
	Hostname   string   `json:"hostname"`
	FQDN       string   `json:"fqdn"`
	HTTPPort   int      `json:"httpPort"`
	TargetKind string   `json:"targetKind"`
	TargetName string   `json:"targetName"`
	Writer     string   `json:"writer"`
	Members    []string `json:"members"`
	// MemberNodes maps each member's id, as the sim names it, to its canvas node:
	// the only members DBCanvas applies network and disk conditions to for this sim.
	MemberNodes map[string]string `json:"memberNodes,omitempty"`
	// Applied is the network and disk conditions DBCanvas has put on members for
	// this sim, kept so they can be cleared after an app restart. See perfsimcond.go.
	Applied map[string]perfSimApplied `json:"applied,omitempty"`
}

// perfSimMember and perfSimTarget are what the image reads from PERFSIM_TARGET
// (perfsim/internal/mysqlx.Config); the field names are that contract.
type perfSimMember struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Role string `json:"role"`
}

type perfSimEndpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type perfSimTarget struct {
	Name           string          `json:"name"`
	Engine         string          `json:"engine"` // mysql | postgres
	Kind           string          `json:"kind"`
	Manager        string          `json:"manager,omitempty"`  // patroni | repmgr | spock: what runs a PostgreSQL cluster
	Database       string          `json:"database,omitempty"` // Spock: the database it replicates
	User           string          `json:"user"`
	Password       string          `json:"password"`
	WriterUser     string          `json:"writerUser"`
	WriterPassword string          `json:"writerPassword"`
	Writer         perfSimEndpoint `json:"writer"`
	URI            string          `json:"uri,omitempty"` // MongoDB: the application's connection string
	// Valkey in Sentinel mode: the name Sentinel monitors the primary under, and
	// every Sentinel, so the workload follows a failover.
	SentinelMaster string          `json:"sentinelMaster,omitempty"`
	Sentinels      []string        `json:"sentinels,omitempty"`
	Members        []perfSimMember `json:"members"`
}

// perfSimLink is the node's linked target, if it is one Performance Sim supports.
func perfSimLink(doc designDoc, nodeID string) (kind, targetID string, ok bool) {
	kind, targetID, ok = stockSimTarget(doc, nodeID)
	if !ok || !perfSimTargets[kind] {
		return kind, targetID, false
	}
	return kind, targetID, true
}

// perfSimFrame is the cluster frame behind a link: the frame itself, or the one
// a router fronts. A standalone server has none.
func perfSimFrame(doc designDoc, kind, targetID string) (designFrame, bool) {
	switch kind {
	case "ps", "mariadb", "mysqlce", "pg", "psm", "valkey", "k3d":
		// A Kubernetes frame's members are pods, not canvas nodes.
		return designFrame{}, false
	case "pgbouncer":
		_, f, _, ok := pgBouncerBackend(doc, targetID)
		return f, ok && f.ID != ""
	case "haproxy":
		f, _, ok := haproxyBackend(doc, targetID)
		return f, ok
	case "proxysql":
		f, _, ok := backendFrameForProxySQL(doc, targetID)
		return f, ok
	}
	f := frameByID(doc, targetID)
	return f, f.ID != ""
}

// perfSimIssues validates a Performance Sim node.
func perfSimIssues(doc designDoc, n designNode) []issue {
	kind, targetID, ok := perfSimLink(doc, n.ID)
	switch {
	case targetID == "":
		return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
			" needs an association line to a MySQL-family, PostgreSQL, MongoDB or Valkey server or cluster, or to an HAProxy, ProxySQL or PgBouncer node in front of one."}}
	case !ok:
		return []issue{{Level: "error", Message: fmt.Sprintf("Performance Sim node %s is linked to %s, which it cannot drive yet: it supports Percona Server, MySQL, MariaDB, PostgreSQL (standalone, Patroni or repmgr), MongoDB (standalone, replica set or sharded cluster) and Valkey (standalone, Valkey Cluster or Sentinel) servers and clusters.",
			n.Label, nodeLabel(doc, targetID))}}
	}
	if kind == "k3d" {
		switch frameByID(doc, targetID).K3DOperator {
		case "pxc", "ps", "psmdb", "pg", "pgo", "cnpg":
		default:
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
				" is linked to Kubernetes frame " + nodeLabel(doc, targetID) + ", which runs none of the database operators it can drive (PXC, Percona Server, PSMDB, Percona PostgreSQL, Crunchy PGO, CloudNativePG)."}}
		}
	}
	if kind == "haproxy" {
		if _, _, ok := haproxyBackend(doc, targetID); !ok {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
				" is linked to an HAProxy node that does not front a PXC, MySQL replication, Patroni, repmgr or Spock cluster."}}
		}
	}
	if kind == "pgbouncer" {
		if back, _, _, ok := pgBouncerBackend(doc, targetID); !ok || back == "spock" {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
				" is linked to a PgBouncer node that does not pool for a PostgreSQL node, or a Patroni or repmgr cluster."}}
		}
	}
	if kind == "proxysql" {
		if _, _, ok := backendFrameForProxySQL(doc, targetID); !ok {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
				" is linked to a ProxySQL node with no PXC or MySQL replication cluster behind it."}}
		}
	}
	return nil
}

// perfSimMembers lists the data members of the linked target, in label order.
// A Galera arbitrator (garbd) holds no data and runs no mysqld, so it is left out;
// so are a sharded MongoDB cluster's mongos and config servers, which hold none
// of the sim's data: its members are the shards' mongods.
func perfSimMembers(doc designDoc, kind, targetID string, hosts map[string]string, domain string) []designNode {
	var out []designNode
	if f, ok := perfSimFrame(doc, kind, targetID); ok {
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Type == f.Type && n.Role != "arbitrator" && n.Role != "mongos" && n.Role != "config" {
				out = append(out, n)
			}
		}
	} else {
		standalone := targetID
		if kind == "pgbouncer" {
			if _, _, n, ok := pgBouncerBackend(doc, targetID); ok {
				standalone = n.ID
			}
		}
		for _, n := range doc.Nodes {
			if n.ID == standalone {
				out = append(out, n)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// provisionPerfSim records the deployment, then brings up the sim container.
func (a *App) provisionPerfSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	cfg := perfSimConfig{Image: perfSimImage, Hostname: host, FQDN: fqdnOf(host, domain)}

	// Keep the published port across a redeploy so a bookmarked dashboard still works.
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old perfSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			cfg.HTTPPort = old.HTTPPort
		}
	}
	if cfg.HTTPPort == 0 {
		if p, e := freeHostPort(); e == nil {
			cfg.HTTPPort = p
		}
	}

	kind, targetID, ok := perfSimLink(doc, n.ID)
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
		return
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, perfSimImage); !ok {
			pr.fail("image %s not found — run `make perfsim-image` first", perfSimImage)
			return
		}
		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, err := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if err != nil {
			pr.fail("%v", err)
			return
		}

		pr.phase("Waiting for the linked database", 20)
		engine := stockSimEngineForTarget(doc, kind, targetID)
		if kind == "k3d" {
			k, err := a.perfSimK8sTarget(ctx, st, doc, frameByID(doc, targetID), engine, deployTimeout(), pr)
			if err != nil {
				pr.fail("%v", err)
				return
			}
			target := perfSimTarget{Name: k.name, Engine: engine, Kind: "k3d-" + k.operator,
				User: k.user, Password: k.pass, WriterUser: k.user, WriterPassword: k.pass,
				Writer: k.writer, URI: k.uri, Members: k.members, Database: k.database}
			if engine == "postgres" {
				target.Manager = "operator"
			}
			for _, m := range k.members {
				cfg.Members = append(cfg.Members, m.ID)
			}
			cfg.TargetKind, cfg.TargetName = target.Kind, k.name
			cfg.Writer = fmt.Sprintf("%s:%d", k.writer.Host, k.writer.Port)
			pr.logln(fmt.Sprintf("target: %s, writes through %s, %d pod(s): %v", k.name, cfg.Writer, len(cfg.Members), cfg.Members))
			a.startPerfSim(ctx, st, n, host, domain, intranetIP, cfg, target, pr)
			return
		}
		var r stockSimResolved
		switch engine {
		case "postgres":
			r, err = a.stockSimPostgresTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout(), false)
		case "mongodb":
			r, err = a.stockSimMongoTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout())
		case "valkey":
			r, err = a.stockSimValkeyTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout())
		default:
			r, err = a.stockSimMySQLTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout(), false)
		}
		if err != nil {
			pr.fail("%v", err)
			return
		}
		members := perfSimMembers(doc, kind, targetID, hosts, domain)
		if len(members) == 0 {
			pr.fail("the linked target has no database members")
			return
		}

		target := perfSimTarget{
			Name: r.displayName, Engine: engine, Kind: kind,
			WriterUser: r.secrets.User, WriterPassword: r.secrets.Password,
			Writer: perfSimEndpoint{Host: r.host, Port: r.port},
		}
		memberPort := pxcMySQLPort
		if f, ok := perfSimFrame(doc, kind, targetID); ok && (f.Type == "patroni" || f.Type == "repmgr" || f.Type == "spock") {
			target.Manager = f.Type
			if f.Type == "spock" {
				target.Database = spockDefaultDB
			}
		}
		switch engine {
		case "postgres":
			// The resolver already connects as the superuser, the one role a
			// PostgreSQL node provisions; the stressors need it too.
			target.User, target.Password = r.secrets.User, r.secrets.Password
			memberPort = patroniPGPort
		case "mongodb":
			// The admin user, as the resolver gives it; the workload uses the
			// replica set connection string it built.
			target.User, target.Password = r.secrets.User, r.secrets.Password
			memberPort = mongoPort
			for _, kv := range r.env {
				if uri, ok := strings.CutPrefix(kv, "MONGO_URI="); ok {
					target.URI = uri
				}
			}
			if kind == "psmdb" {
				// The sim reads each shard member directly, as the admin user; a
				// shard has none of its own unless PMM made one.
				f, _ := perfSimFrame(doc, kind, targetID)
				a.perfSimShardAccounts(ctx, st, members, f.PSMDBMajor, r.secrets.User, r.secrets.Password, pr)
			}
		case "valkey":
			// The "default" user's password; every member has the same one.
			target.Password = r.secrets.Password
			memberPort = valkeyPort
			if f, ok := perfSimFrame(doc, kind, targetID); ok && f.ValkeyMode == "sentinel" {
				target.SentinelMaster = valkeySentinelName(f)
				for _, m := range members {
					target.Sentinels = append(target.Sentinels, fmt.Sprintf("%s:%d", fqdnOf(hosts[m.ID], domain), valkeySentinelPort))
				}
			}
		default:
			// admin@'%' is the same on every member of a cluster; read it from the
			// first member that has deployed, falling back to the installation's own.
			admin := mysqlFamilySecrets()
			for _, m := range members {
				if dep, err := a.store.GetDeployment(st.ID, m.ID); err == nil {
					var s pxcSecrets
					if json.Unmarshal(dep.Secrets, &s) == nil && s.AdminUser != "" && s.AdminPassword != "" {
						admin = s
						break
					}
				}
			}
			target.User, target.Password = admin.AdminUser, admin.AdminPassword
		}
		for _, m := range members {
			id := hosts[m.ID]
			if id == "" {
				id = sanitizeName(m.Label)
			}
			target.Members = append(target.Members, perfSimMember{ID: id, Host: fqdnOf(id, domain), Port: memberPort, Role: m.Role})
			cfg.Members = append(cfg.Members, id)
			if cfg.MemberNodes == nil {
				cfg.MemberNodes = map[string]string{}
			}
			cfg.MemberNodes[id] = m.ID
		}
		cfg.TargetKind, cfg.TargetName = r.kind, r.displayName
		cfg.Writer = fmt.Sprintf("%s:%d", r.host, r.port)
		if engine == "mongodb" {
			cfg.Writer = "the replica set's primary"
			if kind == "psmdb" {
				cfg.Writer = "mongos (" + r.host + ")"
			}
			if kind == "psm" {
				cfg.Writer = fmt.Sprintf("%s:%d", r.host, r.port)
			}
		}
		if engine == "valkey" && kind == "valkeycluster" {
			cfg.Writer = "each key's primary (cluster client)"
			if target.SentinelMaster != "" {
				cfg.Writer = "the primary Sentinel names for " + target.SentinelMaster
			}
		}
		pr.logln(fmt.Sprintf("target: %s %s, writes through %s, %d member(s): %v", kind, r.displayName, cfg.Writer, len(cfg.Members), cfg.Members))

		a.startPerfSim(ctx, st, n, host, domain, intranetIP, cfg, target, pr)
	}()
}

// startPerfSim creates and starts the sim's container for a resolved target,
// and waits for its dashboard.
func (a *App) startPerfSim(ctx context.Context, st Stack, n designNode, host, domain, intranetIP string, cfg perfSimConfig, target perfSimTarget, pr *pxcProg) {
	pr.phase("Creating container", 45)
	name := containerName(st.ID, n.ID)
	if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
		a.engCtx(ctx).ContainerRemove(ctx, cid)
	}
	id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
		Name: name, Image: perfSimImage, Hostname: host,
		Env:     []string{"PERFSIM_TARGET=" + string(mustJSON(target)), "PORT=" + strconv.Itoa(perfSimPort)},
		Network: networkName(st.ID), Aliases: []string{host},
		PublishMap: []PortMap{{ContainerPort: perfSimPort, HostPort: cfg.HTTPPort}},
		DNS:        []string{intranetIP}, DNSSearch: []string{domain},
	})
	if err != nil {
		pr.fail("create container: %v", err)
		return
	}
	if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
		pr.fail("start container: %v", err)
		return
	}
	if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", perfSimPort)); e == nil {
		if p, e2 := strconv.Atoi(hp); e2 == nil {
			cfg.HTTPPort = p
		}
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

	pr.phase("Waiting for the dashboard", 80)
	if err := a.waitPerfSimHealthy(ctx, id, 90*time.Second); err != nil {
		pr.fail("perfsim did not become ready: %v", err)
		return
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
	a.reconcileStackDNS(ctx, st.ID)
	pr.logln(fmt.Sprintf("dashboard on http://localhost:%d — every stressor is chosen there", cfg.HTTPPort))
	pr.phase("Running", 100)
	pr.p.Message = "provisioned"
	pr.save()
}

// perfSimShardAccounts makes sure every shard member of a sharded MongoDB
// cluster accepts the admin user, the account the sim reads it with directly.
// mongoAdminEvalScript creates it through the localhost exception where a shard
// has no user yet; that only works on a shard's primary, so every member is
// tried and a secondary's refusal is expected. From 8.0 a direct connection to
// a shard needs directShardOperations as well.
func (a *App) perfSimShardAccounts(ctx context.Context, st Stack, members []designNode, major, user, pass string, pr *pxcProg) {
	js := "db.adminCommand({ping:1})"
	if strings.HasPrefix(strings.TrimSpace(major), "8.") {
		js = fmt.Sprintf(`db.getSiblingDB("admin").grantRolesToUser(%q,[{role:"directShardOperations",db:"admin"}])`, user)
	}
	env := []string{"ADMIN_USER=" + user, "ADMIN_PW=" + pass, "USER_JS=" + js}
	for _, m := range members {
		dep, err := a.store.GetDeployment(st.ID, m.ID)
		if err != nil || dep.ContainerID == "" {
			continue
		}
		if _, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", mongoAdminEvalScript}, env); err != nil {
			pr.logln(fmt.Sprintf("%s: %v", m.Label, err))
		}
	}
}

// waitPerfSimHealthy waits for the dashboard to answer. The image is
// distroless, so the check is the binary's own -healthcheck flag.
func (a *App) waitPerfSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/perfsim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
