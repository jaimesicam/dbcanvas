package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PMM node management — post-deployment actions driven via docker exec, mirroring
// the Intranet management handlers. Currently exposes the nginx certificate
// (re-issued from the Intranet CA, archiving the prior set).

const pmmCertInfoScript = `if [ -f /srv/nginx/certificate.crt ]; then
  echo "exists: yes"
  openssl x509 -in /srv/nginx/certificate.crt -noout -subject -issuer -startdate -enddate
else
  echo "exists: no"
fi`

// loadRunningPMM resolves the stack + a running PMM node deployment, returning
// the stack (needed to find the sibling Intranet node) and the deployment.
func (a *App) loadRunningPMM(w http.ResponseWriter, r *http.Request) (Stack, Deployment, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return Stack{}, Deployment{}, false
	}
	dep, err := a.store.GetDeployment(st.ID, r.PathValue("nid"))
	if err != nil || dep.ContainerID == "" {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return Stack{}, Deployment{}, false
	}
	if dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "node is not running")
		return Stack{}, Deployment{}, false
	}
	return st, dep, true
}

// intranetContainerFor returns the running Intranet node's container id for a
// stack, used as the CA source when (re)issuing a PMM certificate.
func (a *App) intranetContainerFor(st Stack) (string, bool) {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		return "", false
	}
	for _, n := range doc.Nodes {
		if n.Type != "intranet" {
			continue
		}
		if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && dep.ContainerID != "" {
			return dep.ContainerID, true
		}
	}
	return "", false
}

func (a *App) handlePMMCertInfo(w http.ResponseWriter, r *http.Request) {
	_, dep, ok := a.loadRunningPMM(w, r)
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, pmmCertInfoScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": strings.TrimSpace(out)})
}

func (a *App) handlePMMCertGenerate(w http.ResponseWriter, r *http.Request) {
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
	if b.Value <= 0 {
		b.Value, b.Unit = 365, "days"
	}
	switch b.Unit {
	case "minutes", "hours", "days":
	default:
		b.Unit = "days"
	}

	intranetID, found := a.intranetContainerFor(st)
	if !found {
		writeErr(w, http.StatusBadRequest, "a running Intranet node (CA) is required to issue the certificate")
		return
	}

	var cfg pmmConfig
	json.Unmarshal(dep.Config, &cfg)
	domain := envOr("DOMAIN", "example.net")
	alias := cfg.Alias
	if alias == "" {
		alias = "pmm"
	}

	result, err := a.pmmGenerateCert(r.Context(), dep.ContainerID, intranetID, domain, alias, b.Value, b.Unit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reflect that the node now carries an Intranet-signed certificate.
	if !cfg.GenerateCert {
		cfg.GenerateCert = true
		cfgJSON, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": result})
}
