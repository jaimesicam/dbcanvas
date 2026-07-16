package main

// Query Runner — HTTP handlers + target resolution. Targets are restricted to the
// caller's canvas-provisioned DB nodes (admins see all); a run points each query at
// a node picked from a dropdown, and the node's network coordinates + credentials
// are resolved server-side (passwords never reach the browser). See queryrun_run.go
// for the execution engine and docs/QUERY_RUNNER.md for usage.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// qrAppContainerID identifies the app's own container for the Docker network API.
// Compose leaves the container hostname at Docker's default (the short container id),
// which the daemon accepts as a container reference.
func qrAppContainerID() string {
	h, _ := os.Hostname()
	return h
}

// appIsContainerized reports whether DBCanvas itself runs inside a container (the
// pure-Docker deployment) versus on the host (required for hybrid Vagrant stacks —
// see VAGRANT.md). It gates the self-join in joinStackForDial: only a containerized
// app can — and must — attach itself to the stack bridge to reach a Docker node's
// IP. On the host there is no self-container to join, and none is needed: the host
// already routes to both the Docker bridge and the VM host-only net (vagrant_net.go).
// DBCANVAS_HOST_MODE=1 forces the host answer (for e2e runs on a host that still has
// a /.dockerenv lying around).
func appIsContainerized() bool {
	switch os.Getenv("DBCANVAS_HOST_MODE") {
	case "1", "true":
		return false
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// joinStackForDial attaches the app's own container to the stack network so it can
// dial a Docker node's bridge IP directly (Docker's embedded DNS doesn't resolve the
// Intranet's *.<domain> names). It is idempotent on the Docker engine and a no-op on
// the Vagrant engine (VM IPs are host-only-routable). On the host it does nothing:
// there is no self-container, and the host reaches both networks unaided. Shared by
// the Query Runner, Benchmark (dialNodeDSN) and the Mongo Data Generator.
func (a *App) joinStackForDial(ctx context.Context, netName string) error {
	if !appIsContainerized() {
		return nil
	}
	return a.engCtx(ctx).NetworkConnect(ctx, netName, qrAppContainerID())
}

// qrTarget is a running MySQL/PXC or PostgreSQL node the runner may point at.
type qrTarget struct {
	StackID   int64  `json:"stackId"`
	StackName string `json:"stackName"`
	NodeID    string `json:"nodeId"`
	Label     string `json:"label"`
	Engine    string `json:"engine"` // mysql | postgres
	Type      string `json:"type"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}

func qrNewID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// listSQLTargets returns the running MySQL/PXC + PostgreSQL nodes the user may target
// (owner-scoped; admins see all). Shared by the Query Runner and Benchmark.
func (a *App) listSQLTargets(u User) []qrTarget {
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	out := []qrTarget{}
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		doc := buildDoc(st)
		for _, n := range doc.Nodes {
			engine := engineForType(n.Type)
			if engine == "" || engine == "mongodb" {
				continue // the Query Runner is SQL-only; MongoDB has its own target list (benchmark)
			}
			if dep, err := a.store.GetDeployment(st.ID, n.ID); err != nil || dep.State != DeployRunning {
				continue
			}
			port := 3306
			if engine == "postgres" {
				port = 5432
			}
			out = append(out, qrTarget{
				StackID: st.ID, StackName: st.Name, NodeID: n.ID, Label: n.Label,
				Engine: engine, Type: n.Type, Port: port,
				Host: fqdnOf(stackHostnames(doc)[n.ID], envOr("DOMAIN", "example.net")),
			})
		}
	}
	return out
}

// resolveNodeCreds validates ownership + that the node is a running supported SQL
// target, returning its engine, container id, label, and network-account credentials
// (MySQL admin@'%', Postgres superuser). Shared by the Query Runner and Benchmark.
func (a *App) resolveNodeCreds(u User, stackID int64, nodeID string) (engine, containerID, label, user, pass string, err error) {
	st, e := a.store.GetStack(stackID)
	if e != nil {
		return "", "", "", "", "", fmt.Errorf("target stack not found")
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		return "", "", "", "", "", fmt.Errorf("not your stack")
	}
	var node designNode
	found := false
	for _, n := range buildDoc(st).Nodes {
		if n.ID == nodeID {
			node, found = n, true
			break
		}
	}
	if !found {
		return "", "", "", "", "", fmt.Errorf("node not found in stack")
	}
	engine = engineForType(node.Type)
	if engine == "" {
		return "", "", "", "", "", fmt.Errorf("node type %q is not a supported target", node.Type)
	}
	dep, e2 := a.store.GetDeployment(stackID, nodeID)
	if e2 != nil || dep.State != DeployRunning || dep.ContainerID == "" {
		return "", "", "", "", "", fmt.Errorf("node is not running")
	}
	label, containerID = node.Label, dep.ContainerID
	if engine == "mongodb" {
		var s mongoSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.AdminUser, s.AdminPassword
		if user == "" {
			user = "admin"
		}
	} else if engine == "mysql" {
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.AdminUser, s.AdminPassword
		if user == "" {
			user = "admin"
		}
	} else {
		var s pgSecrets
		json.Unmarshal(dep.Secrets, &s)
		user, pass = s.Super(), s.SuperPassword
	}
	return engine, containerID, label, user, pass, nil
}

// handleQueryRunTargets lists the running DB nodes the caller may target.
func (a *App) handleQueryRunTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, a.listSQLTargets(u))
}

type qrRunRequest struct {
	Queries []qrQuerySpec `json:"queries"`
}

// handleQueryRunStart validates + resolves every query spec, then launches the run.
func (a *App) handleQueryRunStart(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req qrRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Queries) == 0 {
		writeErr(w, http.StatusBadRequest, "at least one query is required")
		return
	}
	if len(req.Queries) > qrMaxQueries {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("too many queries (max %d)", qrMaxQueries))
		return
	}

	run := &qrRun{id: qrNewID(), ownerID: u.ID, app: a, status: "running"}
	for i := range req.Queries {
		q, err := a.qrBuildQuery(u, req.Queries[i])
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("query %d: %v", i+1, err))
			return
		}
		run.queries = append(run.queries, q)
	}

	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	qrRegister(run)
	run.launch(ctx)
	writeJSON(w, http.StatusOK, map[string]string{"runId": run.id})
}

// qrBuildQuery validates one spec, enforces ownership, and gathers everything needed
// to dial the target — but defers the actual stack-network join + node-IP resolution
// to run time (see qrRun.dial), keeping the disruptive network attach off the request
// path. Credentials use the node's network account (MySQL admin@'%', Postgres
// superuser) — root@localhost / peer-auth accounts can't connect over TCP.
func (a *App) qrBuildQuery(u User, spec qrQuerySpec) (*qrQuery, error) {
	if strings.TrimSpace(spec.SQL) == "" {
		return nil, fmt.Errorf("SQL is required")
	}
	if spec.Threads < 1 {
		spec.Threads = 1
	}
	if spec.Threads > qrMaxThreads {
		spec.Threads = qrMaxThreads
	}
	if spec.Count < 0 {
		spec.Count = 0
	}
	if spec.TimeLimitS < 1 {
		spec.TimeLimitS = 60
	}
	if spec.TimeLimitS > 3600 {
		spec.TimeLimitS = 3600
	}
	st, err := a.store.GetStack(spec.StackID)
	if err != nil {
		return nil, fmt.Errorf("target stack not found")
	}
	if st.OwnerID != u.ID && u.Role != RoleAdmin {
		return nil, fmt.Errorf("not your stack")
	}
	var node designNode
	found := false
	for _, n := range buildDoc(st).Nodes {
		if n.ID == spec.NodeID {
			node, found = n, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("node not found in stack")
	}
	engine := engineForType(node.Type)
	if engine == "" {
		return nil, fmt.Errorf("node type %q is not a supported query target", node.Type)
	}
	dep, err := a.store.GetDeployment(st.ID, spec.NodeID)
	if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
		return nil, fmt.Errorf("node is not running")
	}

	q := &qrQuery{
		spec: spec, label: node.Label, engine: engine, status: "pending",
		token:           qrMarker + "-" + qrNewID(), // unique per query, for gate self-exclusion
		stackID:         st.ID,
		nodeContainerID: dep.ContainerID,
		database:        spec.Database,
	}
	switch engine {
	case "mysql":
		var s pxcSecrets
		json.Unmarshal(dep.Secrets, &s)
		q.driver, q.dbUser, q.dbPass = "mysql", s.AdminUser, s.AdminPassword
		if q.dbUser == "" {
			q.dbUser = "admin"
		}
	default: // postgres
		var s pgSecrets
		json.Unmarshal(dep.Secrets, &s)
		q.driver, q.dbUser, q.dbPass = "pgx", s.Super(), s.SuperPassword
	}
	if spec.Gate.Enabled {
		if spec.Gate.PollMs < 100 {
			q.spec.Gate.PollMs = 1000
		}
		re, err := regexp.Compile(spec.Gate.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid gate pattern: %v", err)
		}
		q.re = re
	}
	return q, nil
}

// handleQueryRunStatus returns a live snapshot of a run (owner-scoped).
func (a *App) handleQueryRunStatus(w http.ResponseWriter, r *http.Request) {
	run, ok := a.qrOwnedRun(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

// handleQueryRunStop cancels a running run.
func (a *App) handleQueryRunStop(w http.ResponseWriter, r *http.Request) {
	run, ok := a.qrOwnedRun(w, r)
	if !ok {
		return
	}
	run.mu.Lock()
	if run.status == "running" {
		run.status = "stopped"
	}
	run.mu.Unlock()
	if run.cancel != nil {
		run.cancel()
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

// handleQueryRunHistory lists finished runs the caller owns (in-memory, this session).
func (a *App) handleQueryRunHistory(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	qrRuns.Lock()
	ids := append([]string(nil), qrRuns.order...)
	runs := make([]*qrRun, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- { // newest first
		if run := qrRuns.m[ids[i]]; run != nil && (run.ownerID == u.ID || u.Role == RoleAdmin) {
			runs = append(runs, run)
		}
	}
	qrRuns.Unlock()
	out := make([]qrRunDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, run.snapshot())
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) qrOwnedRun(w http.ResponseWriter, r *http.Request) (*qrRun, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	run := qrGet(r.PathValue("id"))
	if run == nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	if run.ownerID != u.ID && u.Role != RoleAdmin {
		writeErr(w, http.StatusForbidden, "not your run")
		return nil, false
	}
	return run, true
}
