package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PXC node management — post-deployment certificate (re)issuance, mirroring the
// PMM certificate frame.

const pxcCertInfoScript = `if [ -f /var/lib/mysql/server-cert.pem ]; then
  echo "exists: yes"
  openssl x509 -in /var/lib/mysql/server-cert.pem -noout -subject -issuer -startdate -enddate
else
  echo "exists: no"
fi`

func (a *App) handlePXCCertInfo(w http.ResponseWriter, r *http.Request) {
	_, dep, ok := a.loadRunningPMM(w, r) // generic running-deployment loader
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, pxcCertInfoScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": strings.TrimSpace(out)})
}

func (a *App) handlePXCCertGenerate(w http.ResponseWriter, r *http.Request) {
	st, dep, ok := a.loadRunningPMM(w, r)
	if !ok {
		return
	}
	var b struct {
		Value int    `json:"value"`
		Unit  string `json:"unit"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var cfg pxcConfig
	json.Unmarshal(dep.Config, &cfg)
	if cfg.Role == "arbitrator" {
		writeErr(w, http.StatusBadRequest, "arbitrator nodes have no MySQL certificate")
		return
	}
	intranetID, found := a.intranetContainerFor(st)
	if !found {
		writeErr(w, http.StatusBadRequest, "a running Intranet node (CA) is required to issue the certificate")
		return
	}
	unit := "mysql"
	if cfg.Bootstrap {
		unit = "mysql@bootstrap"
	}
	fqdn := cfg.FQDN
	if fqdn == "" {
		fqdn = fqdnOf(cfg.Hostname, envOr("DOMAIN", "example.net"))
	}
	if err := a.pxcApplyCert(r.Context(), dep.ContainerID, intranetID, fqdn, unit, cfg.OS, b.Value, b.Unit, nil, true); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reflect that the node now carries an Intranet-signed certificate.
	if !cfg.GenerateCert {
		cfg.GenerateCert = true
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
	}
	out, _ := a.execScript(r.Context(), dep.ContainerID, pxcCertInfoScript, nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": strings.TrimSpace(out)})
}

// handlePXCFrameMonitor turns PMM monitoring on or off for a deployed PXC cluster.
// Body: {"pmmNodeId": "<id>"} to register the cluster's data nodes with that PMM
// server, or "" to deregister them. It applies to every running regular member
// (arbitrators have no MySQL to monitor) and records the change in each member's
// config so the manager reflects it.
func (a *App) handlePXCFrameMonitor(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	fid := r.PathValue("fid")
	var b struct {
		PMMNodeID string `json:"pmmNodeId"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return
	}
	var frame designFrame
	found := false
	for _, f := range doc.Frames {
		if f.ID == fid && f.Type == "pxc" {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "PXC cluster not found")
		return
	}

	monitoredBy, pmmUser, pmmPass := "", "", ""
	if b.PMMNodeID != "" {
		fqdn, user, pass, running := a.pmmServerFor(st, doc, b.PMMNodeID)
		if !running {
			writeErr(w, http.StatusConflict, "the selected PMM node is not running")
			return
		}
		monitoredBy, pmmUser, pmmPass = fqdn, user, pass
	}

	// Every member of a pxc frame runs on the same engine (a VM in a hybrid stack).
	ctx := withEngine(r.Context(), a.nodeEngine(st, frame.Type))
	updated := 0
	for _, n := range doc.Nodes {
		if n.FrameID != fid || n.Type != "pxc" || n.Role == "arbitrator" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		var sec pxcSecrets
		json.Unmarshal(dep.Secrets, &sec)
		if b.PMMNodeID != "" {
			if err := a.pxcPMMExec(ctx, dep.ContainerID, frame.OS, pxcPMMEnv(monitoredBy, pmmUser, pmmPass, sec, n.Label)); err != nil {
				writeErr(w, http.StatusInternalServerError, "register "+n.Label+" with PMM: "+err.Error())
				return
			}
		} else {
			// Best-effort deregister; ignore errors so an unreachable agent doesn't block.
			a.engCtx(ctx).Exec(ctx, dep.ContainerID, []string{"bash", "-c", pxcPMMRemoveScript}, []string{"NODE=" + n.Label})
		}
		var cfg pxcConfig
		json.Unmarshal(dep.Config, &cfg)
		cfg.MonitoredBy = monitoredBy
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
		updated++
	}

	// The frame's pmmNodeId is persisted by the designer's autosave; here we only
	// reconcile the live containers and each member's recorded MonitoredBy.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "monitoredBy": monitoredBy, "updated": updated})
}
