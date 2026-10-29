package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A Performance Sim's other two ways to its database (perfsim.go is the linked
// one): an instance on an All in One node, and a server named by address,
// which may be outside the stack. Neither has members that are canvas nodes,
// so network and disk conditions are unavailable, and the target says why.

// perfSimMode is the node's connection mode: "linked", "aio" or "manual".
func perfSimMode(n designNode) string {
	switch n.PSMode {
	case "aio", "manual":
		return n.PSMode
	}
	return "linked"
}

// perfSimManualEngine is the manual connection's engine, MySQL by default.
func perfSimManualEngine(n designNode) string {
	switch n.PSEngine {
	case "postgres", "mongodb", "valkey":
		return n.PSEngine
	}
	return "mysql"
}

// perfSimDefaultPort is an engine's usual port.
func perfSimDefaultPort(engine string) int {
	switch engine {
	case "postgres":
		return patroniPGPort
	case "mongodb":
		return mongoPort
	case "valkey":
		return valkeyPort
	}
	return pxcMySQLPort
}

// perfSimTLS is the manual connection's TLS mode.
func perfSimTLS(n designNode) string {
	switch n.PSTLS {
	case "disable", "require":
		return n.PSTLS
	}
	return "prefer"
}

// perfSimModeIssues validates the aio and manual modes.
func perfSimModeIssues(doc designDoc, n designNode) []issue {
	switch perfSimMode(n) {
	case "manual":
		if strings.TrimSpace(n.PSHost) == "" {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label + " connects manually but has no host: set psHost (host.docker.internal reaches the Docker host)."}}
		}
		if n.PSPort < 0 || n.PSPort > 65535 {
			return []issue{{Level: "error", Message: fmt.Sprintf("Performance Sim node %s has port %d, which is not a TCP port.", n.Label, n.PSPort)}}
		}
	case "aio":
		in, ok := simAIODeclared(doc, n.PSAIONode, n.PSAIOInstance)
		if !ok {
			return []issue{{Level: "error", Message: "Performance Sim node " + n.Label + " uses an All in One instance, but psAIONode and psAIOInstance do not name one declared on an All in One node in this stack."}}
		}
		switch aioEngineForKind(in.Kind) {
		case "mysql", "postgres", "mongodb":
		default:
			return []issue{{Level: "error", Message: fmt.Sprintf("Performance Sim node %s uses All in One instance %s, a %s instance, which it cannot drive: choose a MySQL-family, PostgreSQL or MongoDB instance.", n.Label, in.Name, in.Kind)}}
		}
	}
	return nil
}

// perfSimManualTarget is the target for a server named by address: one
// member, the server itself.
func perfSimManualTarget(n designNode) perfSimTarget {
	engine := perfSimManualEngine(n)
	host := strings.TrimSpace(n.PSHost)
	port := n.PSPort
	if port == 0 {
		port = perfSimDefaultPort(engine)
	}
	id := strings.Split(host, ".")[0]
	if id == "" {
		id = "server"
	}
	t := perfSimTarget{
		Name: fmt.Sprintf("%s:%d", host, port), Engine: engine, Kind: "manual",
		User: n.PSUser, Password: n.PSPassword, WriterUser: n.PSUser, WriterPassword: n.PSPassword,
		Writer:  perfSimEndpoint{Host: host, Port: port},
		Members: []perfSimMember{{ID: id, Host: host, Port: port}},
		TLS:     perfSimTLS(n),
		CondWhy: "This server is reached by address, not as a node DBCanvas runs, so DBCanvas cannot shape its network or disk.",
	}
	if engine == "postgres" {
		t.Database = strings.TrimSpace(n.PSDatabase)
	}
	if engine == "mongodb" {
		t.URI = perfSimMongoURI(n.PSUser, n.PSPassword, host, port, t.TLS)
	}
	return t
}

// perfSimMongoURI reaches one MongoDB server directly. directConnection keeps
// the driver from trading the address it was given for the names the replica
// set reports, which need not resolve from the stack; the engine follows a
// new primary itself (perfsim/internal/mongo followPrimary).
func perfSimMongoURI(user, pass, host string, port int, tls string) string {
	u := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
		url.QueryEscape(user), url.QueryEscape(pass), host, port)
	if tls == "require" {
		u += "&tls=true"
	}
	return u
}

// perfSimAIOTarget resolves an All in One instance, every member of a cluster
// instance included, so the lag stressors have replicas to act on. The writer
// is the member taking writes (or mongos).
func (a *App) perfSimAIOTarget(ctx context.Context, st Stack, doc designDoc, n designNode, timeout time.Duration) (perfSimTarget, error) {
	label := nodeLabel(doc, n.PSAIONode)
	deadline := time.Now().Add(timeout)
	for {
		dep, err := a.store.GetDeployment(st.ID, n.PSAIONode)
		switch {
		case err == nil && dep.State == DeployError:
			return perfSimTarget{}, fmt.Errorf("the selected All in One node %s failed to provision", label)
		case err == nil && dep.State == DeployRunning && dep.ContainerID != "":
			writer, ok := simAIOMember(dep, n.PSAIOInstance)
			if !ok {
				return perfSimTarget{}, fmt.Errorf("All in One node %s has no ready instance called %q", label, n.PSAIOInstance)
			}
			engine, port, user, pass := aioInstanceCreds(dep, writer)
			t := perfSimTarget{
				Name: aioTargetLabel(label, writer), Engine: engine, Kind: "aio-" + engine,
				User: user, Password: pass, WriterUser: user, WriterPassword: pass,
				Writer:  perfSimEndpoint{Host: writer.FQDN, Port: port},
				CondWhy: "These members are instances inside one All in One container; DBCanvas shapes a whole container's network and disk, not one instance's.",
			}
			for _, m := range aioTargetableInstances(dep) {
				if (m.Inst != n.PSAIOInstance && m.Group != n.PSAIOInstance) || m.Role == "mongos" || m.Role == "config" {
					continue
				}
				_, mport, _, _ := aioInstanceCreds(dep, m)
				t.Members = append(t.Members, perfSimMember{ID: m.Inst, Host: m.FQDN, Port: mport, Role: m.Role})
			}
			switch engine {
			case "mysql", "postgres":
			case "mongodb":
				t.URI = perfSimMongoURI(user, pass, writer.FQDN, port, "")
			default:
				return perfSimTarget{}, fmt.Errorf("All in One instance %q is a %s instance, which Performance Sim cannot drive", n.PSAIOInstance, writer.Kind)
			}
			if len(t.Members) == 0 {
				t.Members = []perfSimMember{{ID: writer.Inst, Host: writer.FQDN, Port: port}}
			}
			return t, nil
		}
		if time.Now().After(deadline) {
			return perfSimTarget{}, fmt.Errorf("the selected All in One node %s did not become ready within %s", label, timeout)
		}
		select {
		case <-ctx.Done():
			return perfSimTarget{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// handlePerfSimTest checks a manual connection the way the sim will make it:
// from a sim container on the stack's network (Docker's default bridge before
// the first deploy), running the image's own -testconn, which connects as the
// engine does and reports the server it found.
func (a *App) handlePerfSimTest(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var body struct {
		Engine   string `json:"engine"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		Database string `json:"database"`
		TLS      string `json:"tls"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	n := designNode{Label: "test", PSMode: "manual", PSEngine: body.Engine, PSHost: body.Host, PSPort: body.Port,
		PSUser: body.User, PSPassword: body.Password, PSDatabase: body.Database, PSTLS: body.TLS}
	if iss := perfSimModeIssues(designDoc{}, n); len(iss) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": iss[0].Message})
		return
	}
	ctx := r.Context()
	eng := a.engCtx(ctx)
	if ok, _ := eng.ImageExists(ctx, perfSimImage); !ok {
		writeErr(w, http.StatusPreconditionFailed, "image "+perfSimImage+" not found — run `make perfsim-image` first")
		return
	}
	network := networkName(st.ID)
	if _, nerr := eng.NetworkSubnet(ctx, network); nerr != nil {
		network = ""
	}
	name := fmt.Sprintf("%sperfsim-testconn-%d", stackContainerPrefix(st.ID), time.Now().UnixNano())
	id, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name: name, Image: perfSimImage, Network: network,
		Env:        []string{"PERFSIM_TARGET=" + string(mustJSON(perfSimManualTarget(n))), "PORT=" + strconv.Itoa(perfSimPort)},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		NoRestart:  true,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create test container: "+err.Error())
		return
	}
	defer eng.ContainerRemove(context.Background(), id)
	if err := eng.ContainerStart(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "start test container: "+err.Error())
		return
	}
	// The check runs beside the dashboard the container starts with, from the
	// same environment.
	res, err := eng.Exec(ctx, id, []string{"/perfsim", "-testconn"}, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "run connection test: "+err.Error())
		return
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Code == 0, "message": out})
}
