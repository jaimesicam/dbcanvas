package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// MarketChaos (Type=="marketchaos"): the "Unoptimized MySQL Challenge" — a fictional
// stock-exchange demo app deliberately deployed with bad indexes, queries, and
// transaction patterns for a learner to diagnose and fix. Reads/writes whichever
// MySQL-family target it's linked to (by a drawn edge, mirroring Airline Sim): a
// standalone Percona Server node ("ps"), a single PXC member node linked directly
// ("pxcnode" — bypasses cluster-wide resolution on purpose, for challenges about an
// app that never load-balances at all), a PXC cluster frame ("pxc"), a MySQL
// replication frame ("mysql"), or either of the last two fronted by HAProxy — five
// resolvable shapes. Unlike Airline Sim, there is no ProxySQL shape (out of scope
// per the product spec) and the "pxc" shape additionally needs the full list of
// running member hosts once the PXC-specific challenge pack lands (stage S2+), not
// just the one primary connection every other sim resolves to.
//
// Runs dbcanvas's own first-party dbcanvas-marketchaos:latest image (built by
// `make marketchaos-image`), not a systemd OS image. Never published to the host:
// it's reached from inside the stack's Ubuntu VNC desktop's own browser, at
// marketchaos.<domain>:8092 on the stack's internal network.

const (
	marketChaosImage = "dbcanvas-marketchaos:latest"
	marketChaosPort  = 8092
)

// marketChaosConfig is the non-secret profile shown for a deployed MarketChaos node.
type marketChaosConfig struct {
	Image      string `json:"image"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	TargetKind string `json:"targetKind"` // ps | pxcnode | pxc | mysql | haproxy-pxc | haproxy-mysql
	TargetName string `json:"targetName"` // linked node/frame label, for display
}

// marketChaosTarget resolves the coarse kind ("ps" | "pxcnode" | "pxc" | "mysql" |
// "haproxy") and target node/frame id a marketchaos node is linked to via a drawn
// edge. Mirrors airlineSimTarget's undirected-edge walk. "haproxy" is resolved
// further (into its -pxc/-mysql variant) once the linked node's own backend is
// known to be running — see waitMarketChaosTarget. "pxcnode" is the one shape no
// other sim resolves: a direct link to a single PXC member node rather than its
// frame, matched via the member's own designNode entry (the frame-level "pxc" case
// below only ever matches an edge drawn to the frame's own boundary/port).
func marketChaosTarget(doc designDoc, startID string) (kind, targetID string, ok bool) {
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
			case n.Type == "pxc" && n.FrameID != "":
				return "pxcnode", n.ID, true
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
			}
		}
	}
	return "", "", false
}

// marketChaosDatasetEnv translates the node's dataset-profile property into the
// DATASET_* env vars main.go reads (see datasetFromEnv in marketchaos/main.go).
// "" defaults to the engine's own "medium" default, so designs saved before this
// property existed still deploy exactly as before. For "custom" only the counts
// the learner actually set (>0) are passed — a zero field falls back to the
// medium preset on the container side, same as leaving it blank.
func marketChaosDatasetEnv(n designNode) []string {
	env := []string{}
	if n.MCDataset != "" {
		env = append(env, "DATASET_PROFILE="+n.MCDataset)
	}
	if n.MCDataset != "custom" {
		return env
	}
	if n.MCTraders > 0 {
		env = append(env, fmt.Sprintf("DATASET_TRADERS=%d", n.MCTraders))
	}
	if n.MCOrders > 0 {
		env = append(env, fmt.Sprintf("DATASET_ORDERS=%d", n.MCOrders))
	}
	if n.MCTrades > 0 {
		env = append(env, fmt.Sprintf("DATASET_TRADES=%d", n.MCTrades))
	}
	if n.MCTicks > 0 {
		env = append(env, fmt.Sprintf("DATASET_TICKS=%d", n.MCTicks))
	}
	return env
}

// provisionMarketChaos records the deployment then brings up the sim container once
// its linked MySQL-family target is running.
func (a *App) provisionMarketChaos(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	coarseKind, targetID, ok := marketChaosTarget(doc, n.ID)
	cfg := marketChaosConfig{Image: marketChaosImage, Hostname: host, FQDN: fqdn}
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

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, marketChaosImage); !ok {
			pr.fail("image %s not found — run `make marketchaos-image` first", marketChaosImage)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Waiting for linked MySQL-family target", 20)
		targetHost, targetPort, sec, kind, targetName, members, werr := a.waitMarketChaosTarget(ctx, st.ID, hosts, doc, domain, coarseKind, targetID, deployTimeout())
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
		env := []string{
			"MYSQL_DSN=" + dsn, "MYSQL_DB=marketchaos", "TARGET_KIND=" + cfg.TargetKind,
			"TARGET_LABEL=" + cfg.TargetName, fmt.Sprintf("PORT=%d", marketChaosPort),
		}
		env = append(env, marketChaosDatasetEnv(n)...)
		// MYSQL_DSN_MEMBERS: only set for a direct PXC cluster-frame link (kind
		// "pxc") — the PXC-specific challenge pack's genuinely simultaneous
		// multi-member connections (see waitPXCAllMembersRunning). Every other
		// target shape only ever gets the one primary DSN above.
		if cfg.TargetKind == "pxc" && len(members) > 0 {
			memberDSNs := make([]string, len(members))
			for i, m := range members {
				memberDSNs[i] = fmt.Sprintf("%s:%s@tcp(%s:%d)/marketchaos", sec.AppUser, sec.AppPassword, m, targetPort)
			}
			env = append(env, "MYSQL_DSN_MEMBERS="+string(mustJSON(memberDSNs)))
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: marketChaosImage, Hostname: host,
			Env:     env,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: mustJSON(cfg)})

		pr.phase("Waiting for simulation engine", 80)
		if err := a.waitMarketChaosHealthy(ctx, id, 60*time.Second); err != nil {
			pr.fail("marketchaos did not become ready: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg)})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
	}()
}

// waitMarketChaosTarget resolves a coarse target (from marketChaosTarget) all the
// way down to a connectable host:port and app credentials, blocking until that
// target is actually running. Returns the resolved TargetKind string (one of the 5)
// and a display name for the deployed node's config.
func (a *App) waitMarketChaosTarget(ctx context.Context, stackID int64, hosts map[string]string, doc designDoc, domain, coarseKind, targetID string, timeout time.Duration) (host string, port int, sec pxcSecrets, kind, displayName string, members []string, err error) {
	switch coarseKind {
	case "ps":
		h, s, werr := a.waitPSNodeRunning(ctx, stackID, targetID, hosts, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", nil, werr
		}
		return h, pxcMySQLPort, s, "ps", nodeLabel(doc, targetID), nil, nil

	case "pxcnode":
		h, s, werr := a.waitPXCNodeRunning(ctx, stackID, targetID, hosts, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", nil, werr
		}
		return h, pxcMySQLPort, s, "pxcnode", nodeLabel(doc, targetID), nil, nil

	case "mysql":
		frame := frameByID(doc, targetID)
		primaryFQDN, _, s, werr := a.waitMySQLRunning(ctx, stackID, frame, doc, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", nil, werr
		}
		return primaryFQDN, pxcMySQLPort, s, "mysql", frame.Label, nil, nil

	case "pxc":
		frame := frameByID(doc, targetID)
		h, s, mem, werr := a.waitPXCAllMembersRunning(ctx, stackID, frame, doc, hosts, domain, timeout)
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", nil, werr
		}
		return h, pxcMySQLPort, s, "pxc", frame.Label, mem, nil

	case "haproxy":
		backFrame, backKind, hok := haproxyBackend(doc, targetID)
		if !hok || (backKind != "pxc" && backKind != "mysql") {
			return "", 0, pxcSecrets{}, "", "", nil, fmt.Errorf("MarketChaos's linked HAProxy node must front a PXC or MySQL replication cluster (found %q)", backKind)
		}
		if !a.waitNodeRunning(stackID, targetID, timeout) {
			return "", 0, pxcSecrets{}, "", "", nil, fmt.Errorf("linked HAProxy node did not become ready within %s", timeout)
		}
		var s pxcSecrets
		var werr error
		if backKind == "pxc" {
			_, s, werr = a.waitPXCRunning(ctx, stackID, backFrame, doc, domain, timeout)
		} else {
			_, _, s, werr = a.waitMySQLRunning(ctx, stackID, backFrame, doc, domain, timeout)
		}
		if werr != nil {
			return "", 0, pxcSecrets{}, "", "", nil, werr
		}
		return fqdnOf(hosts[targetID], domain), haproxyWritePort, s, "haproxy-" + backKind, nodeLabel(doc, targetID), nil, nil
	}
	return "", 0, pxcSecrets{}, "", "", nil, fmt.Errorf("unresolved MarketChaos target")
}

// waitPXCAllMembersRunning is waitPXCRunning's polling shape, extended to
// return every regular (non-arbitrator) member's FQDN once the whole set is
// running — not just the first one. The PXC-specific challenge pack (stage
// S4+) needs genuinely independent connections to more than one member to
// manufacture real cross-node Galera certification conflicts (see the
// written plan's §5.3 design note on why that replaces "multi-writer through
// HAProxy", which this repo's single-writer HAProxy+PXC config can't
// represent). Every other MarketChaos target shape only ever needs the one
// connection waitMarketChaosTarget's other cases already resolve.
func (a *App) waitPXCAllMembersRunning(ctx context.Context, stackID int64, frame designFrame, doc designDoc, hosts map[string]string, domain string, timeout time.Duration) (primary string, sec pxcSecrets, members []string, err error) {
	var regulars []designNode
	for _, n := range doc.Nodes {
		if n.FrameID == frame.ID && n.Type == "pxc" && n.Role != "arbitrator" {
			regulars = append(regulars, n)
		}
	}
	if len(regulars) == 0 {
		return "", pxcSecrets{}, nil, fmt.Errorf("associated PXC cluster %s has no regular (data) node", frame.Label)
	}
	deadline := time.Now().Add(timeout)
	for {
		allRunning := true
		var fqdns []string
		for _, n := range regulars {
			dep, derr := a.store.GetDeployment(stackID, n.ID)
			if derr != nil {
				allRunning = false
				break
			}
			if dep.State == DeployError {
				return "", pxcSecrets{}, nil, fmt.Errorf("associated PXC cluster %s failed to provision", frame.Label)
			}
			if dep.State != DeployRunning {
				allRunning = false
				break
			}
			json.Unmarshal(dep.Secrets, &sec)
			fqdns = append(fqdns, fqdnOf(hosts[n.ID], domain))
		}
		if allRunning {
			return fqdns[0], sec, fqdns, nil
		}
		if time.Now().After(deadline) {
			return "", pxcSecrets{}, nil, fmt.Errorf("associated PXC cluster %s did not become ready within %s", frame.Label, timeout)
		}
		select {
		case <-ctx.Done():
			return "", pxcSecrets{}, nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// waitPXCNodeRunning blocks until a single, directly-linked PXC member node
// (not its frame) is running and returns its FQDN plus its own stored
// credentials — the same generic single-node polling waitPSNodeRunning does,
// with error text that names a PXC member instead of a Percona Server node
// so a failure is clearly attributed to the right link shape.
func (a *App) waitPXCNodeRunning(ctx context.Context, stackID int64, nodeID string, hosts map[string]string, domain string, timeout time.Duration) (string, pxcSecrets, error) {
	deadline := time.Now().Add(timeout)
	for {
		if dep, err := a.store.GetDeployment(stackID, nodeID); err == nil {
			if dep.State == DeployError {
				return "", pxcSecrets{}, fmt.Errorf("linked PXC member node failed to provision")
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				var sec pxcSecrets
				json.Unmarshal(dep.Secrets, &sec)
				return fqdnOf(hosts[nodeID], domain), sec, nil
			}
		}
		if time.Now().After(deadline) {
			return "", pxcSecrets{}, fmt.Errorf("linked PXC member node did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", pxcSecrets{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitMarketChaosHealthy polls until the container's own /healthz answers 200. The
// runtime image is distroless (no shell, no curl) — matching waitAirlineSimHealthy,
// this execs the check inside the container: `marketchaos -healthcheck` exits 0
// only if /healthz answers 200.
func (a *App) waitMarketChaosHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"/marketchaos", "-healthcheck"}, nil)
		if err == nil && res.Code == 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("healthz not ready within %s", timeout)
}
