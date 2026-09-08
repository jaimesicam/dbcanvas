package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Airline Sim (Type=="airlinesim"): the "MySQL Airline Reservation Lab" demo app —
// ten background agents run a 200-route reservation workload against a
// 2000-aircraft fleet, reading/writing whichever MySQL-family target it's linked to
// (by a drawn edge, exactly like Hotel Sim links to a MongoDB target): a standalone
// Percona Server node ("ps"), a MySQL replication frame's primary, a PXC cluster
// frame, or either of the last two fronted by HAProxy or ProxySQL — seven resolvable
// shapes in total. An embedded web server exposes a live dashboard of the result.
// Runs dbcanvas's own first-party dbcanvas-airlinesim:latest image (built by
// `make airlinesim-image`), not a systemd OS image — no product installed on top,
// no PMM monitoring. Its dashboard port is published to the host (like PMM's own
// HTTP/HTTPS ports) on a fixed, auto-assigned port that's reused across a redeploy
// — see the HTTPPort field below — so it's reachable directly from the host
// browser, with no VNC desktop needed.

const (
	airlineSimImage = "dbcanvas-airlinesim:latest"
	airlineSimPort  = 8090
)

// airlineSimConfig is the non-secret profile shown for a deployed Airline Sim node.
type airlineSimConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // ps | mysql | pxc | haproxy-pxc | haproxy-mysql | proxysql-pxc | proxysql-mysql
	TargetName string `json:"targetName"` // linked node/frame label, for display
	HTTPPort   int    `json:"httpPort"`   // host port mapped to the container's dashboard port
}

// airlineSimTarget resolves the coarse kind ("ps" | "mysql" | "pxc" | "haproxy" |
// "proxysql") and target node/frame id an airlinesim node is linked to via a drawn
// edge. Mirrors hotelSimTarget's undirected-edge walk. "haproxy" and "proxysql" are
// resolved further (into their -pxc/-mysql variant) once the linked node's own
// backend is known to be running — see waitAirlineSimTarget.
func airlineSimTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
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
			case n.Type == "ps":
				return "ps", n.ID, true
			case n.Type == "haproxy":
				return "haproxy", n.ID, true
			case n.Type == "proxysql" && n.FrameID == "":
				return "proxysql", n.ID, true
			}
		}
		for _, f := range doc.Frames {
			if f.ID != other {
				continue
			}
			switch f.Type {
			case "mysql":
				return "mysql", f.ID, true
			case "pxc":
				return "pxc", f.ID, true
			case "proxysql":
				return "proxysql", f.ID, true
			}
		}
	}
	return "", "", false
}

func frameByID(doc designDoc, id string) designFrame {
	for _, f := range doc.Frames {
		if f.ID == id {
			return f
		}
	}
	return designFrame{}
}

// provisionAirlineSim records the deployment then brings up the sim container once
// its linked MySQL-family target is running.
func (a *App) provisionAirlineSim(st Stack, n designNode, doc designDoc) {
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
		var old airlineSimConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	coarseKind, targetID, ok := airlineSimTarget(doc, n.ID)
	cfg := airlineSimConfig{Image: airlineSimImage, Hostname: host, FQDN: fqdn, HTTPPort: httpPort}
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

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, airlineSimImage); !ok {
			pr.fail("image %s not found — run `make airlinesim-image` first", airlineSimImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Waiting for linked MySQL-family target", 20)
		targetHost, targetPort, sec, kind, targetName, werr := a.waitAirlineSimTarget(ctx, st.ID, hosts, doc, domain, coarseKind, targetID, deployTimeout())
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
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/", sec.AppUser, sec.AppPassword, targetHost, targetPort)
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: airlineSimImage, Hostname: host,
			Env: []string{
				"MYSQL_DSN=" + dsn, "MYSQL_DB=airlinesim", "TARGET_KIND=" + cfg.TargetKind,
				"TARGET_LABEL=" + cfg.TargetName, fmt.Sprintf("PORT=%d", airlineSimPort),
			},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: airlineSimPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", airlineSimPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for simulation engine", 80)
		if err := a.waitAirlineSimHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("airlinesim did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitAirlineSimTarget resolves a coarse target (from airlineSimTarget) all the way
// down to a connectable host:port and app credentials, blocking until that target
// is actually running. Returns the resolved TargetKind string (one of the 7) and a
// display name for the deployed node's config.
func (a *App) waitAirlineSimTarget(ctx context.Context, stackID int64, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration) (host string, port int, sec pxcSecrets, kind, displayName string, err error) {
	switch coarseKind {
	case "ps":
		h, s, werr := a.waitPSNodeRunning(ctx, stackID, targetID, hosts, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", werr
		}
		return h, pxcMySQLPort, s, "ps", nodeLabel(doc, targetID), nil

	case "mysql":
		frame := frameByID(doc, targetID)
		primaryFQDN, _, s, werr := a.waitMySQLRunning(ctx, stackID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", werr
		}
		return primaryFQDN, pxcMySQLPort, s, "mysql", frame.Label, nil

	case "pxc":
		frame := frameByID(doc, targetID)
		h, s, werr := a.waitPXCRunning(ctx, stackID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", werr
		}
		return h, pxcMySQLPort, s, "pxc", frame.Label, nil

	case "haproxy":
		backFrame, backKind, hok := haproxyBackend(doc, targetID)
		if !hok || (backKind != "pxc" && backKind != "mysql") {
			return "", 0, pxcSecrets{}, "", "", fmt.Errorf("Airline Sim's linked HAProxy node must front a PXC or MySQL replication cluster (found %q)", backKind)
		}
		if !a.waitNodeRunning(stackID, targetID, timeout) {
			return "", 0, pxcSecrets{}, "", "", fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		var s pxcSecrets
		var werr error
		if backKind == "pxc" {
			_, s, werr = a.waitPXCRunning(ctx, stackID, backFrame, doc, domain, timeout)
		} else {
			_, _, s, werr = a.waitMySQLRunning(ctx, stackID, backFrame, doc, domain, timeout)
		}
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", werr
		}
		return fqdnOf(hosts[targetID], domain), haproxyWritePort, s, "haproxy-" + backKind, nodeLabel(doc, targetID), nil

	case "proxysql":
		h, backKind, s, name, werr := a.waitProxySQLRunning(ctx, stackID, doc, hosts, domain, targetID, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", werr
		}
		return h, proxysqlMySQLPort, s, "proxysql-" + backKind, name, nil
	}
	return "", 0, pxcSecrets{}, "", "", fmt.Errorf("unresolved Airline Sim target")
}

// waitPSNodeRunning blocks until a standalone Percona Server node is running and
// returns its FQDN plus its own stored credentials.
func (a *App) waitPSNodeRunning(ctx context.Context, stackID int64, nodeID string, hosts map[string]string, domain string, timeout time.Duration) (string, pxcSecrets, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return "", pxcSecrets{}, fmt.Errorf("linked Percona Server node failed to provision")
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				var sec pxcSecrets
				json.Unmarshal(dep.Secrets, &sec)
				return fqdnOf(hosts[nodeID], domain), sec, nil
			}
		}
		if time.Now().After(deadline) {
			return "", pxcSecrets{}, fmt.Errorf("linked Percona Server node did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", pxcSecrets{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitProxySQLRunning blocks until at least one member of the linked ProxySQL
// node/frame is running, then returns that member's FQDN, the backend kind
// ("pxc"|"mysql") it was configured for (read from the member's own stored config —
// ProxySQL is a transparent MySQL-protocol proxy, so airlinesim connects the same
// way regardless, but the backend kind still selects the right Profile sizing), the
// app credentials it was configured with, and a display name.
func (a *App) waitProxySQLRunning(ctx context.Context, stackID int64, doc designDoc, hosts map[string]string, domain, targetID string, timeout time.Duration) (host, backendKind string, sec pxcSecrets, displayName string, err error) {
	frame := frameByID(doc, targetID)
	var members []designNode
	if frame.ID != "" {
		for _, n := range doc.Nodes {
			if n.FrameID == frame.ID && n.Type == "proxysql" {
				members = append(members, n)
			}
		}
		displayName = frame.Label
	} else {
		for _, n := range doc.Nodes {
			if n.ID == targetID {
				members = append(members, n)
			}
		}
		displayName = nodeLabel(doc, targetID)
	}
	if len(members) == 0 {
		return "", "", pxcSecrets{}, "", fmt.Errorf("linked ProxySQL %s has no member", displayName)
	}

	deadline := time.Now().Add(timeout)
	for {
		for _, m := range members {
			dep, derr := a.store.GetDeployment(stackID, m.ID)
			if derr != nil {
				continue
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				var pcfg proxysqlConfig
				json.Unmarshal(dep.Config, &pcfg)
				var psec proxysqlSecrets
				json.Unmarshal(dep.Secrets, &psec)
				if pcfg.BackendKind == "" {
					continue // configured but hasn't recorded a backend kind yet — keep waiting
				}
				sec := pxcSecrets{AppUser: psec.AppUser, AppPassword: psec.AppPassword}
				return fqdnOf(hosts[m.ID], domain), pcfg.BackendKind, sec, displayName, nil
			}
		}
		if time.Now().After(deadline) {
			return "", "", pxcSecrets{}, "", fmt.Errorf("linked ProxySQL %s did not become ready within %s", displayName, timeout)
		}
		select {
		case <-ctx.Done():
			return "", "", pxcSecrets{}, "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitAirlineSimHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching waitHotelSimHealthy,
// this execs the check inside the container: `airlinesim -healthcheck` exits 0 only
// if /healthz answers 200.
func (a *App) waitAirlineSimHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/airlinesim", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
