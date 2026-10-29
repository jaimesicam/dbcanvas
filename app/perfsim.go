package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// Performance Sim (Type=="perfsim"): a dashboard of stressors that make a linked
// MySQL-family server or cluster misbehave on purpose — replication lag from a
// large transaction, a no-op ALTER, FLUSH TABLES WITH READ LOCK on a replica, a
// stopped or delayed applier — with a timeline that shows what each one does to
// the cluster and to an application running against it. Runs DBCanvas's own
// dbcanvas-perfsim:v<release> image (built by `make perfsim-image`).
//
// Everything about the stressors is chosen on the sim's dashboard while it runs;
// the node itself has no options beyond its association line. What DBCanvas
// does here is the one thing the dashboard cannot: find every member of the
// linked cluster, with an account allowed to lock and reconfigure them. Every
// other sim needs one write endpoint; this one also needs each replica, because
// that is where most of its stressors act.
//
// The workload connects as the application user through the cluster's write
// endpoint, exactly as Stock Market Sim does (a ProxySQL in front of the
// cluster only knows that user). The stressors connect to each member directly
// as the network superuser admin@'%'.
const perfSimPort = 8096

var perfSimImage = "dbcanvas-perfsim:" + imageRelease()

// perfSimTargets are the canvas types a Performance Sim node can be linked to:
// MySQL-family standalone servers and cluster frames, and the two routers when
// they front a MySQL-family cluster.
var perfSimTargets = map[string]bool{
	"ps": true, "mariadb": true, "mysqlce": true,
	"pxc": true, "mysql": true, "innodb": true, "mariadbrepl": true, "mariadbgalera": true,
	"mysqlcerepl": true, "mysqlceinnodb": true,
	"haproxy": true, "proxysql": true,
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
	Kind           string          `json:"kind"`
	User           string          `json:"user"`
	Password       string          `json:"password"`
	WriterUser     string          `json:"writerUser"`
	WriterPassword string          `json:"writerPassword"`
	Writer         perfSimEndpoint `json:"writer"`
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
	case "ps", "mariadb", "mysqlce":
		return designFrame{}, false
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
			" needs an association line to a MySQL-family server or cluster, or to an HAProxy or ProxySQL node in front of one."}}
	case !ok:
		return []issue{{Level: "error", Message: fmt.Sprintf("Performance Sim node %s is linked to %s, which it cannot drive yet: it supports Percona Server, MySQL and MariaDB servers and clusters.",
			n.Label, nodeLabel(doc, targetID))}}
	}
	if kind == "haproxy" {
		if _, back, ok := haproxyBackend(doc, targetID); !ok || (back != "pxc" && back != "mysql") {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label +
				" is linked to an HAProxy node that does not front a PXC or MySQL replication cluster."}}
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
// A Galera arbitrator (garbd) holds no data and runs no mysqld, so it is left out.
func perfSimMembers(doc designDoc, kind, targetID string, hosts map[string]string, domain string) []designNode {
	var out []designNode
	if f, ok := perfSimFrame(doc, kind, targetID); ok {
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Type == f.Type && n.Role != "arbitrator" {
				out = append(out, n)
			}
		}
	} else {
		for _, n := range doc.Nodes {
			if n.ID == targetID {
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
		r, err := a.stockSimMySQLTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout(), false)
		if err != nil {
			pr.fail("%v", err)
			return
		}
		members := perfSimMembers(doc, kind, targetID, hosts, domain)
		if len(members) == 0 {
			pr.fail("the linked target has no database members")
			return
		}

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
		target := perfSimTarget{
			Name: r.displayName, Kind: kind,
			User: admin.AdminUser, Password: admin.AdminPassword,
			WriterUser: r.secrets.User, WriterPassword: r.secrets.Password,
			Writer: perfSimEndpoint{Host: r.host, Port: r.port},
		}
		for _, m := range members {
			id := hosts[m.ID]
			if id == "" {
				id = sanitizeName(m.Label)
			}
			target.Members = append(target.Members, perfSimMember{ID: id, Host: fqdnOf(id, domain), Port: pxcMySQLPort, Role: m.Role})
			cfg.Members = append(cfg.Members, id)
			if cfg.MemberNodes == nil {
				cfg.MemberNodes = map[string]string{}
			}
			cfg.MemberNodes[id] = m.ID
		}
		cfg.TargetKind, cfg.TargetName = r.kind, r.displayName
		cfg.Writer = fmt.Sprintf("%s:%d", r.host, r.port)
		pr.logln(fmt.Sprintf("target: %s %s, writes through %s, %d member(s): %v", kind, r.displayName, cfg.Writer, len(cfg.Members), cfg.Members))

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
	}()
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
