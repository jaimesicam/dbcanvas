package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// MongoDB node management — post-deployment certificate re-issuance. Like the
// PostgreSQL/MySQL families, this only overwrites the per-node cert material in place
// (mongoApplyCert never restarts mongod, and cluster TLS is an operator-driven step);
// the operator applies it via the node's TLS docs.

const mongoCertInfoScript = `if [ -f /etc/mongo/certs/server-cert.pem ]; then
  echo "exists: yes"
  openssl x509 -in /etc/mongo/certs/server-cert.pem -noout -subject -issuer -startdate -enddate
else
  echo "exists: no"
fi`

func (a *App) handleMongoCertInfo(w http.ResponseWriter, r *http.Request) {
	_, dep, ok := a.loadRunningPMM(w, r) // generic running-deployment loader
	if !ok {
		return
	}
	out, err := a.execScript(r.Context(), dep.ContainerID, mongoCertInfoScript, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": strings.TrimSpace(out)})
}

// handleMongoCertGenerate re-signs the node's per-node cert (server.pem) from the
// Intranet CA and overwrites it in place, without restarting mongod.
func (a *App) handleMongoCertGenerate(w http.ResponseWriter, r *http.Request) {
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
	var cfg struct {
		Hostname     string `json:"hostname"`
		FQDN         string `json:"fqdn"`
		GenerateCert bool   `json:"generateCert"`
	}
	json.Unmarshal(dep.Config, &cfg)
	intranetID, found := a.intranetContainerFor(st)
	if !found {
		writeErr(w, http.StatusBadRequest, "a running Intranet node (CA) is required to issue the certificate")
		return
	}
	fqdn := cfg.FQDN
	if fqdn == "" {
		fqdn = fqdnOf(cfg.Hostname, envOr("DOMAIN", "example.net"))
	}
	if err := a.mongoApplyCert(r.Context(), dep.ContainerID, intranetID, fqdn, cfg.Hostname, b.Value, b.Unit, nil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reflect that the node now carries an Intranet-signed certificate. Merge into the
	// raw config JSON to preserve every mongoConfig field.
	if !cfg.GenerateCert {
		var m map[string]any
		if json.Unmarshal(dep.Config, &m) == nil {
			m["generateCert"] = true
			cfgJSON, _ := json.Marshal(m)
			a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
		}
	}

	out, _ := a.execScript(r.Context(), dep.ContainerID, mongoCertInfoScript, nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": strings.TrimSpace(out)})
}
