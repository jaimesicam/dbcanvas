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
	if err := a.pxcApplyCert(r.Context(), dep.ContainerID, intranetID, fqdn, unit, b.Value, b.Unit, nil); err != nil {
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
