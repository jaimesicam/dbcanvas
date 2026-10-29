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

// simwait.go — waits the app simulators share: each blocks until the database a simulator
// is linked to is running, and returns how to reach it.

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

// waitProxySQLRunning blocks until at least one member of the linked ProxySQL
// node/frame is running, then returns that member's FQDN, the backend kind
// ("pxc"|"mysql") it was configured for (read from the member's own stored config —
// ProxySQL is a transparent MySQL-protocol proxy, so a simulator connects the same
// way regardless; the kind is returned for callers that size by it), the
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
		failed := 0
		for _, m := range members {
			dep, derr := a.store.GetDeployment(stackID, m.ID)
			if derr != nil {
				continue
			}
			if dep.State == DeployError {
				failed++
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
		// Every member failed: none will come up, so say so now rather than holding the
		// linked node — and the stack's Deploy lock — until the deploy timeout.
		if failed == len(members) {
			return "", "", pxcSecrets{}, "", fmt.Errorf("linked ProxySQL %s failed to provision", displayName)
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

// waitMongoTarget polls until the linked MongoDB target is ready and returns a
// full mongodb:// connection URI for it: a directConnection URI for a standalone
// psm node, a multi-host replicaSet URI once a majority of a psmrs frame's members
// are running, or the single running mongos router's URI for a psmdb frame.
func (a *App) waitMongoTarget(ctx context.Context, stackID int64, hosts map[string]string, targetNodeID string, targetFrame designFrame, kind string, doc designDoc, timeout time.Duration) (string, error) {
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
					user, pass := mongoAdminCreds(d)
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
					user, pass := mongoAdminCreds(running[0])
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
						user, pass := mongoAdminCreds(d)
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

// mongoAdminCreds reads the admin credentials from a target member's own stored
// secrets (set once at cluster/node creation), falling back to the same .env
// default every other MongoDB provisioner in this codebase uses.
func mongoAdminCreds(d Deployment) (user, pass string) {
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

// waitValkeyTarget polls until the linked Valkey node/cluster is running and
// returns its connection info: one "host:6379" for a standalone node, or every
// running member's "host:6379" (go-redis's ClusterClient auto-discovers the full
// topology from any subset of seed addresses) for a Valkey Cluster frame.
func (a *App) waitValkeyTarget(ctx context.Context, stackID int64, hosts map[string]string, targetNodeID string, targetFrame designFrame, isCluster bool, timeout time.Duration) ([]string, string, error) {
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

// nodeIDForContainer is the node whose deployment runs containerID, or "".
func nodeIDForContainer(deps []Deployment, containerID string) string {
	for _, d := range deps {
		if d.ContainerID == containerID {
			return d.NodeID
		}
	}
	return ""
}
