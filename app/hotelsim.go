package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Hotel Sim (Type=="hotelsim"): the "MongoDB Hotel Reservation Lab" demo app — ten
// background agents run a 100-hotel reservation chain against the PS MongoDB
// standalone node, replica-set frame, or sharded-cluster frame it's linked to (by a
// drawn edge, exactly like Traffic Sim links to a Valkey node/cluster), while an
// embedded web server exposes a live dashboard of the result. Runs dbcanvas's own
// first-party dbcanvas-hotelsim:latest image (built by `make hotelsim-image`), not
// a systemd OS image — no product installed on top, no PMM monitoring. Its
// dashboard port is published to the host (like PMM's own HTTP/HTTPS ports) on a
// fixed, auto-assigned port that's reused across a redeploy — see the HTTPPort
// field below — so it's reachable directly from the host browser, with no VNC
// desktop needed.

const (
	hotelSimImage = "dbcanvas-hotelsim:latest"
	hotelSimPort  = 8089
)

// hotelSimConfig is the non-secret profile shown for a deployed Hotel Sim node.
type hotelSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // "psm" | "psmrs" | "psmdb"
	TargetName string `json:"targetName"` // linked node/frame label, for display
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
}

// hotelSimTarget resolves the standalone psm node, psmrs replica-set frame, or
// psmdb sharded-cluster frame a hotelsim node is linked to via a drawn edge.
// Mirrors trafficSimTarget's undirected-edge walk, three-way instead of two.
func hotelSimTarget(doc designDoc, startID string) (nodeID string, frame designFrame, kind string, ok bool) {
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
			if f.ID == other && (f.Type == "psmrs" || f.Type == "psmdb") {
				return "", f, f.Type, true
			}
		}
	}
	return "", designFrame{}, "", false
}

// provisionHotelSim records the deployment then brings up the sim container once
// its linked MongoDB target is running.
func (a *App) provisionHotelSim(st Stack, n designNode, doc designDoc) {
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
		var old hotelSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	targetNodeID, targetFrame, kind, ok := hotelSimTarget(doc, n.ID)
	cfg := hotelSimConfig{Image: hotelSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort}
	if !ok {
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployError, Config: mustJSON(cfg)})
		return
	}
	cfg.TargetKind = kind
	if kind == "psm" {
		cfg.TargetName = nodeLabel(doc, targetNodeID)
	} else {
		cfg.TargetName = targetFrame.Label
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, hotelSimImage); !ok {
			pr.fail("image %s not found — run `make hotelsim-image` first", hotelSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Waiting for linked MongoDB target", 20)
		uri, werr := a.waitHotelSimTarget(ctx, st.ID, hosts, targetNodeID, targetFrame, kind, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		pr.logln("target: " + cfg.TargetKind + " " + cfg.TargetName)

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: hotelSimImage, Hostname: host,
			Env:     []string{"MONGO_URI=" + uri, "MONGO_DB=hotelsim", "MONGO_TARGET_LABEL=" + cfg.TargetName, fmt.Sprintf("PORT=%d", hotelSimPort)},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: hotelSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", hotelSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for simulation engine", 80)
		if err := a.waitHotelSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("hotelsim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitHotelSimTarget polls until the linked MongoDB target is ready and returns a
// full mongodb:// connection URI for it: a directConnection URI for a standalone
// psm node, a multi-host replicaSet URI once a majority of a psmrs frame's members
// are running, or the single running mongos router's URI for a psmdb frame.
func (a *App) waitHotelSimTarget(ctx context.Context, stackID int64, hosts map[string]string, targetNodeID string, targetFrame designFrame, kind string, doc designDoc, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		deps, err := a.store.ListDeployments(stackID)
		if err == nil {
			byNode := map[string]Deployment{}
			for _, d := range deps {
				byNode[d.NodeID] = d
			}
			switch kind {
			case "psm":
				if d, ok := byNode[targetNodeID]; ok && d.State == DeployRunning && d.ContainerID != "" {
					user, pass := hotelSimCreds(d)
					return fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
						url.QueryEscape(user), url.QueryEscape(pass), hosts[targetNodeID], mongoPort), nil
				}
			case "psmrs":
				var running []Deployment
				memberCount := 0
				for _, n := range doc.Nodes {
					if n.FrameID != targetFrame.ID || n.Type != "psmrs" {
						continue
					}
					memberCount++
					if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
						running = append(running, d)
					}
				}
				if memberCount > 0 && len(running)*2 > memberCount {
					var addrs []string
					for _, d := range running {
						addrs = append(addrs, hosts[d.NodeID]+":"+strconv.Itoa(mongoPort))
					}
					user, pass := hotelSimCreds(running[0])
					rs := sanitizeName(targetFrame.Label)
					if rs == "" {
						rs = "rs"
					}
					return fmt.Sprintf("mongodb://%s:%s@%s/?authSource=admin&replicaSet=%s",
						url.QueryEscape(user), url.QueryEscape(pass), strings.Join(addrs, ","), url.QueryEscape(rs)), nil
				}
			case "psmdb":
				for _, n := range doc.Nodes {
					if n.FrameID != targetFrame.ID || n.Type != "psmdb" || n.Role != "mongos" {
						continue
					}
					if d, ok := byNode[n.ID]; ok && d.State == DeployRunning && d.ContainerID != "" {
						user, pass := hotelSimCreds(d)
						return fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin",
							url.QueryEscape(user), url.QueryEscape(pass), hosts[n.ID], mongoPort), nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("linked MongoDB target did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// hotelSimCreds reads the admin credentials from a target member's own stored
// secrets (set once at cluster/node creation), falling back to the same .env
// default every other MongoDB provisioner in this codebase uses.
func hotelSimCreds(d Deployment) (user, pass string) {
	var sec mongoSecrets
	json.Unmarshal(d.Secrets, &sec)
	user = sec.AdminUser
	if user == "" {
		user = "admin"
	}
	pass = sec.AdminPassword
	if pass == "" {
		pass = envOr("MONGODB_ADMIN_PASSWORD", "admin_password")
	}
	return user, pass
}

// waitHotelSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching waitTrafficSimHealthy,
// this execs the check inside the container: `hotelsim -healthcheck` exits 0 only
// if /healthz answers 200.
func (a *App) waitHotelSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/hotelsim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
