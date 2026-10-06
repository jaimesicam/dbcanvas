package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pgvector Support Sim (Type=="pgvectorsim"): the Support Sim's help desk (supportsim.go)
// run against PostgreSQL instead of MongoDB. Same first-party image
// (dbcanvas-supportsim:latest), same dashboard port, same distroless health check — the
// sim picks its backend from DB_ENGINE, and with DB_ENGINE=postgres it stores tickets and
// their embeddings in PostgreSQL and searches them with pgvector (an HNSW index, <=>).
// Without pgvector on the target it still runs: embeddings go into a real[] column and
// the search is an in-app brute-force scan, said plainly on every page.
//
// It is linked by a drawn edge to one PostgreSQL target, and the target is resolved to a
// DSN by Stock Market Sim's resolvers (waitStockSimTarget), so every shape they reach
// works here the same way: a standalone pg node, a Patroni / repmgr / Spock frame (a
// multi-host DSN with target_session_attrs=read-write for the first two), an HAProxy node
// in front of one of those, a PgBouncer node in front of any of them, or a K3D frame
// running a PostgreSQL operator. The DSN names the default database those resolvers use;
// the sim creates its own `supportsim` database when the role may, and otherwise works
// in a `supportsim` schema of the database it was given.

// pgVectorSimConfig is the non-secret profile shown for a deployed pgvector Support Sim.
type pgVectorSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // pg | patroni | repmgr | spock | haproxy-<back> | pgbouncer-<back> | k3d-<operator>
	TargetName string `json:"targetName"`
	Endpoint   string `json:"endpoint,omitempty"` // host:port the DSN connects to
	PGVector   bool   `json:"pgVector"`           // the target was designed with pgvector on
	HTTPPort   int    `json:"httpPort"`
}

// pgVectorSimTarget resolves what a pgvectorsim node is linked to. The kind is the node or
// frame type (pg, haproxy, pgbouncer, patroni, repmgr, spock, k3d); whether a router
// fronts PostgreSQL, and which operator a K3D frame runs, is the validator's business.
func pgVectorSimTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
	for _, e := range doc.Edges {
		var other string
		switch startID {
		case e.From.Node:
			other = e.To.Node
		case e.To.Node:
			other = e.From.Node
		default:
			continue
		}
		for _, n := range doc.Nodes {
			if n.ID != other {
				continue
			}
			switch {
			case n.Type == "pg" && n.FrameID == "",
				n.Type == "haproxy" && n.FrameID == "",
				n.Type == "pgbouncer":
				return n.Type, n.ID, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID == other && (f.Type == "patroni" || f.Type == "repmgr" || f.Type == "spock" || f.Type == "k3d") {
				return f.Type, f.ID, true
			}
		}
	}
	return "", "", false
}

// pgVectorSimK3DOperators are the K3D operators whose database the sim can drive.
var pgVectorSimK3DOperators = map[string]bool{"pg": true, "cnpg": true, "pgo": true}

// pgVectorSimHasPGVector reports whether the PostgreSQL behind the target was designed with
// pgvector on, and names that database for the warning when it was not. For a router it is
// the backend's option; for a K3D frame it is the Percona operator's, the only one that
// offers pgvector here.
func pgVectorSimHasPGVector(doc designDoc, kind, targetID string) (has bool, where string) {
	switch kind {
	case "pg":
		n := nodeByID(doc, targetID)
		return n.PGVector, "PostgreSQL node " + n.Label
	case "patroni", "repmgr", "spock":
		f := frameByID(doc, targetID)
		return f.PGVector, "cluster " + f.Label
	case "haproxy":
		if back, _, ok := haproxyBackend(doc, targetID); ok {
			return back.PGVector, "cluster " + back.Label
		}
	case "pgbouncer":
		if bk, f, n, ok := pgBouncerBackend(doc, targetID); ok {
			if bk == "pg" {
				return n.PGVector, "PostgreSQL node " + n.Label
			}
			return f.PGVector, "cluster " + f.Label
		}
	case "k3d":
		f := frameByID(doc, targetID)
		return pgVectorSimK3DOperators[f.K3DOperator] && f.K3DPgVector, "Kubernetes frame " + f.Label
	}
	return false, ""
}

// pgVectorSimIssues validates a pgvector Support Sim node.
func pgVectorSimIssues(doc designDoc, n designNode) []issue {
	kind, targetID, ok := pgVectorSimTarget(doc, n.ID)
	who := "pgvector Support Sim node " + n.Label
	if !ok {
		return []issue{{Level: "error", Message: who + " must be linked to PostgreSQL — a standalone PostgreSQL node, a Patroni, repmgr or Spock cluster frame, an HAProxy or PgBouncer node in front of one, or a Kubernetes frame running a PostgreSQL operator — draw an association line from one to it"}}
	}
	var out []issue
	switch kind {
	case "haproxy":
		back, backKind, ok := haproxyBackend(doc, targetID)
		if !ok {
			return []issue{{Level: "error", Message: who + " is linked to HAProxy node " + nodeLabel(doc, targetID) +
				", which does not front exactly one cluster — link a Patroni, repmgr or Spock frame to the HAProxy node, or this node to the cluster directly"}}
		}
		if stockSimEngineForKind(backKind) != "postgres" {
			return []issue{{Level: "error", Message: who + " is linked to HAProxy node " + nodeLabel(doc, targetID) +
				", which fronts " + back.Label + " — a MySQL-family cluster. This sim needs PostgreSQL"}}
		}
	case "pgbouncer":
		if _, _, _, ok := pgBouncerBackend(doc, targetID); !ok {
			return []issue{{Level: "error", Message: who + " is linked to PgBouncer node " + nodeLabel(doc, targetID) +
				", which does not pool for exactly one PostgreSQL backend — link the node or cluster to it, or this sim to the backend directly"}}
		}
	case "k3d":
		f := frameByID(doc, targetID)
		if !pgVectorSimK3DOperators[f.K3DOperator] {
			return []issue{{Level: "error", Message: who + " is linked to Kubernetes frame " + f.Label +
				", which runs " + orDefault(k3dOperatorLabel(f.K3DOperator), "no operator") + " — it needs a PostgreSQL operator (the Percona Operator for PostgreSQL, for pgvector)"}}
		}
		tiers := k3dExposedTiers(f)
		var internal []string
		routable := false
		for tier, typ := range tiers {
			if typ == "ClusterIP" {
				internal = append(internal, tier)
			} else {
				routable = true
			}
		}
		if !routable && len(internal) > 0 {
			sort.Strings(internal)
			out = append(out, issue{Level: "warning", Message: fmt.Sprintf(
				"%s is linked to Kubernetes frame %s, where %s %s exposed only as ClusterIP — an address that exists inside Kubernetes, while the sim runs outside it, so its deploy would fail. Set %s to LoadBalancer or NodePort on the frame",
				who, f.Label, strings.Join(internal, " and "), tierVerb(internal), joinOr(internal))})
		}
	}
	if has, where := pgVectorSimHasPGVector(doc, kind, targetID); !has {
		out = append(out, issue{Level: "warning", Message: who + ": turn on pgvector on " + where +
			" to use HNSW indexes; without it the sim stores embeddings as real[] and scans in the app"})
	}
	return out
}

// nodeByID finds a design node by id (zero value if absent).
func nodeByID(doc designDoc, id string) designNode {
	for _, n := range doc.Nodes {
		if n.ID == id {
			return n
		}
	}
	return designNode{}
}

// provisionPGVectorSim records the deployment, then brings the container up once the
// linked PostgreSQL answers.
func (a *App) provisionPGVectorSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old pgVectorSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	kind, targetID, ok := pgVectorSimTarget(doc, n.ID)
	cfg := pgVectorSimConfig{Image: supportSimImage, Hostname: host, FQDN: fqdnOf(host, domain), HTTPPort: httpPort}
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
		return
	}
	cfg.TargetKind = kind
	cfg.PGVector, _ = pgVectorSimHasPGVector(doc, kind, targetID)
	if f := frameByID(doc, targetID); f.ID != "" {
		cfg.TargetName = f.Label
	} else {
		cfg.TargetName = nodeLabel(doc, targetID)
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, supportSimImage); !ok {
			pr.fail("image %s not found — run `make supportsim-image` first", supportSimImage)
			return
		}
		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// Stock Market Sim's resolver, which knows every PostgreSQL shape: it blocks until
		// the target is running, then hands back DB_ENGINE=postgres and POSTGRES_DSN.
		pr.phase("Waiting for linked PostgreSQL target", 20)
		res, err := a.waitStockSimTarget(ctx, st, hosts, doc, domain, kind, targetID, deployTimeout(), false, pr.logln)
		if err != nil {
			pr.fail("%v", err)
			return
		}
		if res.engine != "postgres" {
			pr.fail("the linked target resolved to %s, not PostgreSQL", orDefault(engineDisplayLabel(res.engine), "no database"))
			return
		}
		var dsn string
		for _, kv := range res.env {
			if v, ok := strings.CutPrefix(kv, "POSTGRES_DSN="); ok {
				dsn = v
			}
		}
		if dsn == "" {
			pr.fail("the linked PostgreSQL target resolved to no connection string")
			return
		}
		cfg.TargetKind = res.kind
		if res.displayName != "" {
			cfg.TargetName = res.displayName
		}
		if res.host != "" {
			cfg.Endpoint = fmt.Sprintf("%s:%d", res.host, res.port)
		}
		search := "no pgvector: embeddings as real[], in-app scan"
		if cfg.PGVector {
			search = "pgvector on (HNSW)"
		}
		pr.logln("target: " + cfg.TargetKind + " " + cfg.TargetName + " — " + search)

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: supportSimImage, Hostname: host,
			// MONGO_TARGET_LABEL is the sim's display label for its target whatever the
			// engine — the name predates the PostgreSQL backend.
			Env: []string{"DB_ENGINE=postgres", "POSTGRES_DSN=" + dsn, "MONGO_TARGET_LABEL=" + cfg.TargetName,
				fmt.Sprintf("PORT=%d", supportSimPort)},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: supportSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", supportSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for the support desk", 80)
		if err := a.waitSupportSimHealthy(ctx, id, 90*time.Second); err != nil {
			pr.fail("pgvectorsim did not become ready: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}
