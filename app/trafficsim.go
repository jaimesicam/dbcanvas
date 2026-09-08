package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Traffic Sim (Type=="trafficsim"): the "Valkey Traffic Lab" demo app — background
// agents simulate a small fictional city's traffic and continuously read/write the
// Valkey node or Valkey Cluster it's linked to (by a drawn edge, exactly like
// HAProxy links to a backend cluster frame), while a small embedded web server
// exposes a live map of the result. Runs dbcanvas's own first-party
// dbcanvas-trafficsim:latest image (built by `make trafficsim-image`), not a
// systemd OS image and not a third-party pulled one — no product installed on top,
// no PMM monitoring. Its dashboard port is published to the host (like PMM's own
// HTTP/HTTPS ports) on a fixed, auto-assigned port that's reused across a redeploy
// — see the HTTPPort field below — so it's reachable directly from the host
// browser, with no VNC desktop needed.

const (
	trafficSimImage = "dbcanvas-trafficsim:latest"
	trafficSimPort  = 8088
)

// trafficSimConfig is the non-secret profile shown for a deployed Traffic Sim node.
type trafficSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // "valkey" | "valkeycluster"
	TargetName string `json:"targetName"` // linked node/frame label, for display
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
}

// trafficSimTarget resolves the Valkey node or Valkey Cluster frame a trafficsim
// node is linked to via a drawn edge. Mirrors haproxyClusterFrames's undirected-edge
// walk (intranet.go) — but unlike every other edge resolver in this file, the other
// endpoint can be a plain standalone node, not only a frame.
func trafficSimTarget(doc designDoc, startID string) (nodeID string, frame designFrame, isCluster bool, ok bool) {
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
			if n.ID == other && n.Type == "valkey" {
				return n.ID, designFrame{}, false, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID == other && f.Type == "valkeycluster" {
				return "", f, true, true
			}
		}
	}
	return "", designFrame{}, false, false
}

// provisionTrafficSim records the deployment then brings up the sim container once
// its linked Valkey target is running.
func (a *App) provisionTrafficSim(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the previously published host port across a redeploy so the dashboard
	// URL shown in node properties stays stable — mirrors provisionPMM's own
	// reused-host-port pattern.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old trafficSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	targetNodeID, targetFrame, isCluster, ok := trafficSimTarget(doc, n.ID)
	cfg := trafficSimConfig{Image: trafficSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort}
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
		return
	}
	if isCluster {
		cfg.TargetKind, cfg.TargetName = "valkeycluster", targetFrame.Label
	} else {
		cfg.TargetKind, cfg.TargetName = "valkey", nodeLabel(doc, targetNodeID)
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, trafficSimImage); !ok {
			pr.fail("image %s not found — run `make trafficsim-image` first", trafficSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Waiting for linked Valkey target", 20)
		addrs, pw, werr := a.waitTrafficSimTarget(ctx, st.ID, hosts, targetNodeID, targetFrame, isCluster, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		pr.logln("target: " + strings.Join(addrs, ","))

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: trafficSimImage, Hostname: host,
			Env:     []string{"VALKEY_ADDRS=" + strings.Join(addrs, ","), "VALKEY_PASSWORD=" + pw, fmt.Sprintf("PORT=%d", trafficSimPort)},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: trafficSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", trafficSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for simulation engine", 80)
		if err := a.waitTrafficSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("trafficsim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitTrafficSimTarget polls until the linked Valkey node/cluster is running and
// returns its connection info: one "host:6379" for a standalone node, or every
// running member's "host:6379" (go-redis's ClusterClient auto-discovers the full
// topology from any subset of seed addresses) for a Valkey Cluster frame.
func (a *App) waitTrafficSimTarget(ctx context.Context, stackID int64, hosts map[string]string, targetNodeID string, targetFrame designFrame, isCluster bool, timeout time.Duration) ([]string, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		deps, err := a.store.ListDeployments(stackID)
		if err == nil {
			byNode := map[string]Deployment{}
			for _, d := range deps {
				byNode[d.NodeID] = d
			}
			if isCluster {
				st, serr := a.store.GetStack(stackID)
				if serr == nil {
					var doc designDoc
					if json.Unmarshal(st.Design, &doc) == nil {
						running, rerr := a.valkeyRunningMembers(st, doc)
						if rerr == nil && len(running) >= 3 {
							var addrs []string
							for _, d := range running {
								addrs = append(addrs, hosts[d.NodeID]+":"+strconv.Itoa(valkeyPort))
							}
							return addrs, valkeyPasswordFor(running[0]), nil
						}
					}
				}
			} else if d, ok := byNode[targetNodeID]; ok && d.State == DeployRunning && d.ContainerID != "" {
				return []string{hosts[targetNodeID] + ":" + strconv.Itoa(valkeyPort)}, valkeyPasswordFor(d), nil
			}
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("linked Valkey target did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitTrafficSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl), so — matching the pattern every
// other pulled-image node type uses for its own readiness check (see
// waitPMMReady) — this execs a check *inside* the container rather than dialing it
// from the dbcanvas server (whose own network may not even reach the stack's
// per-stack bridge network). With no shell available, the check is the binary
// itself: `trafficsim -healthcheck` exits 0 only if /healthz answers 200.
func (a *App) waitTrafficSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/trafficsim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
