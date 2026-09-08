package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Car Rental Sim (Type=="carsim"): the "PostgreSQL Car Rental Lab" demo app — ten
// background agents run a 180-location rental workload against a 2000-vehicle
// fleet, reading/writing whichever PostgreSQL-family target it's linked to (by a
// drawn edge, exactly like Airline Sim links to a MySQL-family target): a
// standalone PostgreSQL node ("pg"), a Patroni/repmgr/Spock cluster frame, or any
// of the last three fronted by HAProxy — seven resolvable shapes in total. An
// embedded web server exposes a live dashboard of the result. Runs dbcanvas's own
// first-party dbcanvas-carsim:latest image (built by `make carsim-image`), not a
// systemd OS image — no product installed on top, no PMM monitoring. Its
// dashboard port is published to the host (like PMM's own HTTP/HTTPS ports) on a
// fixed, auto-assigned port that's reused across a redeploy — see the HTTPPort
// field below — so it's reachable directly from the host browser, with no VNC
// desktop needed.

const (
	carSimImage = "dbcanvas-carsim:latest"
	carSimPort  = 8091
)

// carSimConfig is the non-secret profile shown for a deployed Car Rental Sim node.
type carSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // pg | patroni | repmgr | spock | haproxy-patroni | haproxy-repmgr | haproxy-spock
	TargetName string `json:"targetName"` // linked node/frame label, for display
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
}

// carSimTarget resolves the coarse kind ("pg" | "patroni" | "repmgr" | "spock" |
// "haproxy") and target node/frame id a carsim node is linked to via a drawn
// edge. Mirrors airlineSimTarget's undirected-edge walk. "haproxy" is resolved
// further (into its -patroni/-repmgr/-spock variant) once the linked node's own
// backend is known — see waitCarSimTarget.
func carSimTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
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
			case n.Type == "pg":
				return "pg", n.ID, true
			case n.Type == "haproxy":
				return "haproxy", n.ID, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID != other {
				continue
			}
			switch f.Type {
			case "patroni":
				return "patroni", f.ID, true
			case "repmgr":
				return "repmgr", f.ID, true
			case "spock":
				return "spock", f.ID, true
			}
		}
	}
	return "", "", false
}

// provisionCarSim records the deployment then brings up the sim container once
// its linked PostgreSQL-family target is running.
func (a *App) provisionCarSim(st Stack, n designNode, doc designDoc) {
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
		var old carSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	coarseKind, targetID, ok := carSimTarget(doc, n.ID)
	cfg := carSimConfig{Image: carSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort}
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

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, carSimImage); !ok {
			pr.fail("image %s not found — run `make carsim-image` first", carSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Waiting for linked PostgreSQL-family target", 20)
		targetHost, targetPort, sec, kind, targetName, werr := a.waitCarSimTarget(ctx, st, hosts, doc, domain, coarseKind, targetID, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}
		cfg.TargetKind, cfg.TargetName = kind, targetName
		pr.logln(fmt.Sprintf("target: %s %s (%s:%d)", cfg.TargetKind, cfg.TargetName, targetHost, targetPort))

		pr.phase("Creating container", 45)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		dsn := (&url.URL{
			Scheme: "postgres", User: url.UserPassword(sec.SuperUser, sec.SuperPassword),
			Host: fmt.Sprintf("%s:%d", targetHost, targetPort), Path: "/postgres",
			RawQuery: "sslmode=prefer&connect_timeout=10",
		}).String()
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: carSimImage, Hostname: host,
			Env: []string{
				"POSTGRES_DSN=" + dsn, "TARGET_KIND=" + cfg.TargetKind,
				"TARGET_LABEL=" + cfg.TargetName, fmt.Sprintf("PORT=%d", carSimPort),
			},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: carSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", carSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for simulation engine", 80)
		if err := a.waitCarSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("carsim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitCarSimTarget resolves a coarse target (from carSimTarget) all the way down
// to a connectable host:port and superuser credentials, blocking until that
// target is actually running. Returns the resolved TargetKind string (one of the
// 7) and a display name for the deployed node's config.
func (a *App) waitCarSimTarget(ctx context.Context, st Stack, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration) (host string, port int, sec pgSecrets, kind, displayName string, err error) {
	switch coarseKind {
	case "pg":
		h, s, werr := a.waitPgNodeRunning(ctx, st.ID, targetID, hosts, domain, timeout)
		if werr != nil {
			return "", 0, pgSecrets{}, "", "", werr
		}
		return h, patroniPGPort, s, "pg", nodeLabel(doc, targetID), nil

	case "patroni":
		frame := frameByID(doc, targetID)
		fqdns, s, werr := a.waitPatroniRunning(ctx, st.ID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pgSecrets{}, "", "", werr
		}
		leaderHost := a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.patroniLeaderContainer(ctx, st, frame, doc))
		return leaderHost, patroniPGPort, s, "patroni", frame.Label, nil

	case "repmgr":
		frame := frameByID(doc, targetID)
		fqdns, s, werr := a.waitRepmgrRunning(ctx, st.ID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pgSecrets{}, "", "", werr
		}
		primaryHost := a.leaderOrFirst(ctx, st, frame, doc, hosts, domain, fqdns, a.repmgrPrimaryContainer(ctx, st, frame, doc))
		return primaryHost, patroniPGPort, s, "repmgr", frame.Label, nil

	case "spock":
		frame := frameByID(doc, targetID)
		fqdns, s, werr := a.waitSpockRunning(ctx, st.ID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pgSecrets{}, "", "", werr
		}
		if len(fqdns) == 0 {
			return "", 0, pgSecrets{}, "", "", fmt.Errorf("associated Spock cluster %s has no running member", frame.Label)
		}
		return fqdns[0], patroniPGPort, s, "spock", frame.Label, nil // symmetric peers — any member is a valid write target

	case "haproxy":
		backFrame, backKind, hok := haproxyBackend(doc, targetID)
		if !hok || (backKind != "patroni" && backKind != "repmgr" && backKind != "spock") {
			return "", 0, pgSecrets{}, "", "", fmt.Errorf("Car Rental Sim's linked HAProxy node must front a Patroni, repmgr, or Spock cluster (found %q)", backKind)
		}
		if !a.waitNodeRunning(st.ID, targetID, timeout) {
			return "", 0, pgSecrets{}, "", "", fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		var s pgSecrets
		var werr error
		switch backKind {
		case "patroni":
			_, s, werr = a.waitPatroniRunning(ctx, st.ID, backFrame, doc, domain, timeout)
		case "repmgr":
			_, s, werr = a.waitRepmgrRunning(ctx, st.ID, backFrame, doc, domain, timeout)
		default:
			_, s, werr = a.waitSpockRunning(ctx, st.ID, backFrame, doc, domain, timeout)
		}
		if werr != nil {
			return "", 0, pgSecrets{}, "", "", werr
		}
		return fqdnOf(hosts[targetID], domain), haproxyWritePort, s, "haproxy-" + backKind, nodeLabel(doc, targetID), nil
	}
	return "", 0, pgSecrets{}, "", "", fmt.Errorf("unresolved Car Rental Sim target")
}

// leaderOrFirst maps a Patroni/repmgr frame's known-current-leader container id
// (from patroniLeaderContainer/repmgrPrimaryContainer, "" if undetermined right
// now — e.g. mid-failover) back to that member's FQDN, falling back to the
// first member in fqdns so a momentary detection gap never blocks provisioning
// entirely; the next write simply lands on whichever member answers, same
// tolerance every other client of these clusters already has.
func (a *App) leaderOrFirst(ctx context.Context, st Stack, frame designFrame, doc designDoc, hosts map[string]string, domain string, fqdns []string, leaderContainerID string) string {
	if leaderContainerID != "" {
		deps, _ := a.store.ListDeployments(st.ID)
		if nodeID := nodeIDForContainer(deps, leaderContainerID); nodeID != "" {
			return fqdnOf(hosts[nodeID], domain)
		}
	}
	if len(fqdns) > 0 {
		return fqdns[0]
	}
	return ""
}

// waitPgNodeRunning blocks until a standalone PostgreSQL node is running and
// returns its FQDN plus its own stored credentials. Mirrors
// waitPSNodeRunning's MySQL-family shape.
func (a *App) waitPgNodeRunning(ctx context.Context, stackID int64, nodeID string, hosts map[string]string, domain string, timeout time.Duration) (string, pgSecrets, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return "", pgSecrets{}, fmt.Errorf("linked PostgreSQL node failed to provision")
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				var sec pgSecrets
				json.Unmarshal(dep.Secrets, &sec)
				return fqdnOf(hosts[nodeID], domain), sec, nil
			}
		}
		if time.Now().After(deadline) {
			return "", pgSecrets{}, fmt.Errorf("linked PostgreSQL node did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", pgSecrets{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitRepmgrRunning waits for every member of a repmgr frame to be running and
// returns their FQDNs plus the cluster credentials — the repmgr analog of
// waitPatroniRunning.
func (a *App) waitRepmgrRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, domain string, timeout time.Duration) ([]string, pgSecrets, error) {
	hosts := stackHostnames(doc)
	var members []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "repmgr" {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		return nil, pgSecrets{}, fmt.Errorf("associated repmgr cluster %s has no nodes", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allRunning := true
		var sec pgSecrets
		var fqdns []string
		for _, n := range members {
			dep, err := a.store.GetDeployment(stackID, n.ID)
			if err != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return nil, pgSecrets{}, fmt.Errorf("associated repmgr cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
			fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
		}
		if allRunning {
			return fqdns, sec, nil
		}
		time.Sleep(3 * time.Second)
	}
	return nil, pgSecrets{}, fmt.Errorf("associated repmgr cluster %s did not become ready within %s", frame.Label, timeout)
}

// waitCarSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching
// waitAirlineSimHealthy, this execs the check inside the container: `carsim
// -healthcheck` exits 0 only if /healthz answers 200.
func (a *App) waitCarSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/carsim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
