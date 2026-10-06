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

// Support Sim (Type=="supportsim"): the MongoDB Vector Search Support Desk — a help
// desk for a fictional cloud-database company. Every ticket is embedded in-process
// (all-MiniLM-L6-v2, pure Go, no API call) and searched with $vectorSearch, and the
// desk acts on what it finds: answers from similar resolved tickets, routes to a
// team, merges a customer's repeat ticket, raises an incident when a burst of new
// problems appears. A keyword engine ($search) answers the same questions alongside,
// and both are scored against the simulator's ground truth, so the dashboard's case
// for vectors is measured rather than claimed. A Vector Lab tab teaches the pieces.
//
// It is linked by a drawn edge to a PS MongoDB replica set or sharded cluster with
// Vector search on (mongosearch.go), or to a K3D frame running the PSMDB operator
// with Vector search on (k3dsearch.go). Linked to MongoDB without mongot — a
// standalone node, or a cluster with the option off — it still runs, answering
// searches with an in-app brute-force scan, and says plainly on every page that it
// is doing so and how to turn the real thing on.
//
// Same shape as Hotel Sim: a first-party image (dbcanvas-supportsim:latest, built by
// `make supportsim-image`), dashboard published to the host on a port kept across
// redeploys.

const (
	supportSimImage = "dbcanvas-supportsim:latest"
	supportSimPort  = 8095
)

// supportSimConfig is the non-secret profile shown for a deployed Support Sim node.
type supportSimConfig struct {
	Image        string `json:"image"`
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	TargetKind   string `json:"targetKind"` // psm | psmrs | psmdb | k3d
	TargetName   string `json:"targetName"`
	VectorSearch bool   `json:"vectorSearch"` // the target was designed with mongot
	HTTPPort     int    `json:"httpPort"`
}

// supportSimTarget resolves what a supportsim node is linked to: a standalone psm
// node, a psmrs / psmdb frame, or a K3D frame (whose operator is checked by the
// caller). An undirected-edge walk like hotelSimTarget's.
func supportSimTarget(doc designDoc, startID string) (nodeID string, frame designFrame, kind string, ok bool) {
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
			if n.ID == other && n.Type == "psm" {
				return n.ID, designFrame{}, "psm", true
			}
		}
		for _, f := range doc.Frames {
			if f.ID == other && (f.Type == "psmrs" || f.Type == "psmdb" || f.Type == "k3d") {
				return "", f, f.Type, true
			}
		}
	}
	return "", designFrame{}, "", false
}

// supportSimHasSearch reports whether the linked target was designed with mongot.
func supportSimHasSearch(frame designFrame, kind string) bool {
	switch kind {
	case "psmrs", "psmdb":
		return frame.VectorSearch
	case "k3d":
		return frame.K3DOperator == "psmdb" && frame.K3DVectorSearch
	}
	return false
}

// supportSimIssues validates a Support Sim node.
func supportSimIssues(doc designDoc, n designNode) []issue {
	_, frame, kind, ok := supportSimTarget(doc, n.ID)
	if !ok {
		return []issue{{Level: "error", Message: "Support Sim node " + n.Label + " must be linked to a PS MongoDB replica set or sharded cluster (with Vector search on), or to a K3D frame running the MongoDB operator — draw an association line from one to it"}}
	}
	var out []issue
	if kind == "k3d" {
		if frame.K3DOperator != "psmdb" {
			return []issue{{Level: "error", Message: "Support Sim node " + n.Label + " is linked to Kubernetes frame " + frame.Label +
				", which runs " + orDefault(k3dOperatorLabel(frame.K3DOperator), "no operator") + " — it needs the Percona Operator for MongoDB"}}
		}
		tiers := k3dExposedTiers(frame)
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
			fix := "set " + joinOr(internal) + " to LoadBalancer or NodePort on the frame"
			if frame.K3DVectorSearch && !frame.K3DSharding {
				// LoadBalancer is the one to pick here, and it is safe: DBCanvas has the operator
				// expose the members as NodePort and adds a LoadBalancer per member beside it,
				// which keeps operator 1.23.x out of its search status bug (psmdbSearchExposeMode).
				fix = "set Expose · replica set to LoadBalancer on the frame (with Vector search on, DBCanvas exposes it in a way the operator's search status bug does not affect)"
			}
			out = append(out, issue{Level: "warning", Message: fmt.Sprintf(
				"Support Sim node %s is linked to Kubernetes frame %s, where %s %s exposed only as ClusterIP — an address that exists inside Kubernetes, while the sim runs outside it, so its deploy would fail. %s",
				n.Label, frame.Label, strings.Join(internal, " and "), tierVerb(internal), strings.ToUpper(fix[:1])+fix[1:])})
		}
	}
	if !supportSimHasSearch(frame, kind) {
		where := "PS MongoDB node " + nodeLabel(doc, supportSimNodeID(doc, n.ID))
		fix := "use a replica set (one member is enough) with Vector search on"
		switch kind {
		case "psmrs", "psmdb":
			where, fix = "cluster "+frame.Label, "tick Vector search on the frame (PSMDB 8.3+)"
		case "k3d":
			where, fix = "Kubernetes frame "+frame.Label, "tick Vector search on the frame (operator 1.23.0+)"
		}
		out = append(out, issue{Level: "warning", Message: "Support Sim node " + n.Label + ": " + where +
			" has no vector search (mongot), so the desk will run on an in-app brute-force scan instead of $vectorSearch — " + fix})
	}
	return out
}

func supportSimNodeID(doc designDoc, startID string) string {
	id, _, _, _ := supportSimTarget(doc, startID)
	return id
}

// provisionSupportSim records the deployment, then brings the container up once the
// linked MongoDB answers.
func (a *App) provisionSupportSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old supportSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	targetNodeID, frame, kind, ok := supportSimTarget(doc, n.ID)
	cfg := supportSimConfig{Image: supportSimImage, Hostname: host, FQDN: fqdnOf(host, domain), HTTPPort: httpPort}
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
		return
	}
	cfg.TargetKind = kind
	cfg.VectorSearch = supportSimHasSearch(frame, kind)
	if kind == "psm" {
		cfg.TargetName = nodeLabel(doc, targetNodeID)
	} else {
		cfg.TargetName = frame.Label
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

		pr.phase("Waiting for linked MongoDB target", 20)
		var uri string
		metrics := supportSimMongotMetrics(doc, frame, kind, domain, hosts)
		if kind == "k3d" {
			// The K3D resolver is Stock Market Sim's: it waits for the operator's
			// custom resource to report ready, then picks the mongos or the primary's
			// exposed Service and reads the credentials out of the cluster.
			res, err := a.stockSimK3DTarget(ctx, st, doc, frame.ID, deployTimeout(), pr.logln)
			if err != nil {
				pr.fail("%v", err)
				return
			}
			for _, kv := range res.env {
				if v, ok := strings.CutPrefix(kv, "MONGO_URI="); ok {
					uri = v
				}
			}
			cfg.TargetName = res.displayName
			metrics = a.k3dMongotMetrics(ctx, st, doc, frame.ID)
		} else {
			u, err := a.waitHotelSimTarget(ctx, st.ID, hosts, targetNodeID, frame, kind, doc, deployTimeout())
			if err != nil {
				pr.fail("%v", err)
				return
			}
			uri = u
		}
		if uri == "" {
			pr.fail("the linked MongoDB target resolved to no connection string")
			return
		}
		search := "no vector search: the desk will use an in-app scan"
		if cfg.VectorSearch {
			search = "vector search (mongot) on"
		}
		pr.logln("target: " + kind + " " + cfg.TargetName + " — " + search)

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: supportSimImage, Hostname: host,
			Env: []string{"MONGO_URI=" + uri, "MONGO_DB=supportsim", "MONGO_TARGET_LABEL=" + cfg.TargetName, fmt.Sprintf("PORT=%d", supportSimPort),
				"MONGOT_METRICS=" + metrics},
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
			pr.fail("supportsim did not become ready: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// supportSimMongotMetrics lists every mongot's metrics endpoint (host:9946) for the
// Index Workshop's live panel — the same members mongosearch.go put mongot on: a
// replica set's first member by label, or each shard's. Empty when the target has no
// mongot DBCanvas placed (a K3D cluster's is in-cluster; the sim falls back to asking
// the server for its mongotHost).
func supportSimMongotMetrics(doc designDoc, frame designFrame, kind, domain string, hosts map[string]string) string {
	if !frame.VectorSearch || (kind != "psmrs" && kind != "psmdb") {
		return ""
	}
	byShard := map[int][]designNode{}
	for _, n := range doc.Nodes {
		if n.FrameID != frame.ID || n.Type != kind || n.Role == "config" || n.Role == "mongos" {
			continue
		}
		key := 0
		if kind == "psmdb" {
			key = n.Shard
		}
		byShard[key] = append(byShard[key], n)
	}
	var keys []int
	for k := range byShard {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var eps []string
	for _, k := range keys {
		ms := byShard[k]
		sort.Slice(ms, func(i, j int) bool { return ms[i].Label < ms[j].Label })
		eps = append(eps, fmt.Sprintf("%s:%d", fqdnOf(hosts[ms[0].ID], domain), mongotMetricsPort))
	}
	return strings.Join(eps, ",")
}

// waitSupportSimHealthy execs the image's own health check (distroless: no shell).
func (a *App) waitSupportSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/supportsim", "-healthcheck"}, nil)
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
