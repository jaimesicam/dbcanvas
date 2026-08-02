package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// aio_mgmt.go — the All-in-One node's management API.
//
// Every operation here shells out to `aioctl` inside the container rather than
// calling systemctl directly. That is deliberate: the CLI an operator uses in the
// node's terminal and the buttons in the manager UI must do exactly the same
// thing, including the cluster start ordering, or the two will drift and one of
// them will be subtly wrong. aioctl is the single implementation; this file is a
// transport for it.

// aioRuntimeInstances reads the planned instance list out of a deployment's
// config. Used by the API, and by the DNS reconciler for per-instance aliases.
func aioRuntimeInstances(d Deployment) []aioInstanceRuntime {
	if len(d.Config) == 0 {
		return nil
	}
	var cfg aioConfig
	if json.Unmarshal(d.Config, &cfg) != nil {
		return nil
	}
	return cfg.Instances
}

// aioLoadNode resolves {id}/{nid} to a deployed All-in-One node, writing the
// error response itself on failure.
func (a *App) aioLoadNode(w http.ResponseWriter, r *http.Request) (Stack, Deployment, aioConfig, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, Deployment{}, aioConfig{}, false
	}
	nid := r.PathValue("nid")
	if nodeTypeOf(st, nid) != "aio" {
		writeErr(w, http.StatusBadRequest, "not an All-in-One node")
		return Stack{}, Deployment{}, aioConfig{}, false
	}
	dep, err := a.store.GetDeployment(st.ID, nid)
	if err != nil || dep.ContainerID == "" {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return Stack{}, Deployment{}, aioConfig{}, false
	}
	var cfg aioConfig
	json.Unmarshal(dep.Config, &cfg)
	a.stampEngine(r, st, nid)
	return st, dep, cfg, true
}

// aioExecCtl runs `aioctl <args...>` in the node's container and returns its
// combined output.
func (a *App) aioExecCtl(ctx context.Context, containerID string, args ...string) (string, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, append([]string{aioCtlPath}, args...), nil)
	if err != nil {
		return "", err
	}
	out := res.Stdout
	if strings.TrimSpace(res.Stderr) != "" {
		out += res.Stderr
	}
	if res.Code != 0 {
		return out, fmt.Errorf("aioctl %s exited %d: %s", strings.Join(args, " "), res.Code, lastLines(strings.TrimSpace(out), 200))
	}
	return out, nil
}

// aioLiveState parses `aioctl list` into inst → systemd state, so the manager UI
// shows what is actually running rather than what was planned.
func aioParseStates(listing string) map[string]string {
	states := map[string]string{}
	for i, line := range strings.Split(listing, "\n") {
		f := strings.Fields(line)
		// Header is "INSTANCE KIND GROUP ROLE STATE PORTS"; rows have >= 5 fields.
		if i == 0 || len(f) < 5 || f[0] == "INSTANCE" {
			continue
		}
		states[f[0]] = f[4]
	}
	return states
}

// handleAIOInstances lists the node's instances with their live systemd state.
func (a *App) handleAIOInstances(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.aioLoadNode(w, r)
	if !ok {
		return
	}
	states := map[string]string{}
	if out, err := a.aioExecCtl(r.Context(), dep.ContainerID, "list"); err == nil {
		states = aioParseStates(out)
	}
	type row struct {
		aioInstanceRuntime
		State string `json:"state"`
	}
	rows := make([]row, 0, len(cfg.Instances))
	for _, m := range cfg.Instances {
		s := states[m.Inst]
		if s == "" {
			s = "unknown"
		}
		rows = append(rows, row{aioInstanceRuntime: m, State: s})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"flavor":    cfg.Flavor,
		"hostname":  cfg.Hostname,
		"fqdn":      cfg.FQDN,
		"instances": rows,
	})
}

// handleAIOInstanceAction is start/stop/restart for one instance, one group, or
// "all" — the same selectors aioctl itself accepts, so the UI inherits its
// cluster ordering (seed first on start, followers first on stop).
func (a *App) handleAIOInstanceAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, dep, cfg, ok := a.aioLoadNode(w, r)
		if !ok {
			return
		}
		sel := r.PathValue("inst")
		if !aioValidSelector(cfg, sel) {
			writeErr(w, http.StatusNotFound, "unknown instance or group: "+sel)
			return
		}
		out, err := a.aioExecCtl(r.Context(), dep.ContainerID, action, sel)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "output": out})
	}
}

// handleAIOInstanceLogs returns the tail of one instance's journal.
func (a *App) handleAIOInstanceLogs(w http.ResponseWriter, r *http.Request) {
	_, dep, cfg, ok := a.aioLoadNode(w, r)
	if !ok {
		return
	}
	inst := r.PathValue("inst")
	if !aioKnownInstance(cfg, inst) {
		writeErr(w, http.StatusNotFound, "unknown instance: "+inst)
		return
	}
	out, err := a.aioExecCtl(r.Context(), dep.ContainerID, "logs", inst, "-n", "200")
	if err != nil && out == "" {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// aioKnownInstance guards the exec against an arbitrary argument from the
// client: only names the deployment itself planned are accepted.
func aioKnownInstance(cfg aioConfig, inst string) bool {
	for _, m := range cfg.Instances {
		if m.Inst == inst {
			return true
		}
	}
	return false
}

// aioValidSelector additionally accepts a group name or "all".
func aioValidSelector(cfg aioConfig, sel string) bool {
	if sel == "all" {
		return true
	}
	if aioKnownInstance(cfg, sel) {
		return true
	}
	for _, m := range cfg.Instances {
		if m.Group != "" && m.Group == sel {
			return true
		}
	}
	return false
}
