package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// PostgreSQL-family node management — post-deployment certificate re-issuance for
// standalone PostgreSQL, repmgr and Spock (cert lives in the data dir) and Patroni
// (cert lives in /etc/patroni). Like the MySQL-family re-issue, this only overwrites
// the cert files in place and leaves the running server untouched — pgApplyCert /
// patroniApplyCert never restart. The operator reloads/restarts PostgreSQL to apply
// the new certificate.

const pgCertInfoScript = `if [ -f "$DIR/server.crt" ]; then
  echo "exists: yes"
  openssl x509 -in "$DIR/server.crt" -noout -subject -issuer -startdate -enddate
else
  echo "exists: no"
fi`

// pgCertDir is where a PostgreSQL-family node's server cert lives: /etc/patroni for a
// Patroni member, the data dir otherwise (standalone PostgreSQL, repmgr, Spock).
func pgCertDir(nodeType, os, major string) string {
	if nodeType == "patroni" {
		return "/etc/patroni"
	}
	return pgDataDir(os, major)
}

// pgNodeType returns the design node type for nid (pg | patroni | repmgr | spock).
func (a *App) pgNodeType(st Stack, nid string) string {
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) == nil {
		for _, n := range doc.Nodes {
			if n.ID == nid {
				return n.Type
			}
		}
	}
	return ""
}

// pgCommonCfg is the subset of every PostgreSQL-family config the cert handlers need.
type pgCommonCfg struct {
	OS           string `json:"os"`
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	PGMajor      string `json:"pgMajor"`
	GenerateCert bool   `json:"generateCert"`
}

func (a *App) handlePGCertInfo(w http.ResponseWriter, r *http.Request) {
	st, dep, ok := a.loadRunningPMM(w, r) // generic running-deployment loader
	if !ok {
		return
	}
	var cfg pgCommonCfg
	json.Unmarshal(dep.Config, &cfg)
	dir := pgCertDir(a.pgNodeType(st, dep.NodeID), cfg.OS, cfg.PGMajor)
	out, err := a.execScript(r.Context(), dep.ContainerID, pgCertInfoScript, []string{"DIR=" + dir})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": strings.TrimSpace(out)})
}

// handlePGCertGenerate re-signs the node's server cert from the Intranet CA and
// overwrites it in place, without restarting PostgreSQL.
func (a *App) handlePGCertGenerate(w http.ResponseWriter, r *http.Request) {
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
	var cfg pgCommonCfg
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

	nodeType := a.pgNodeType(st, dep.NodeID)
	var err error
	if nodeType == "patroni" {
		err = a.patroniApplyCert(r.Context(), dep.ContainerID, intranetID, fqdn, b.Value, b.Unit, nil)
	} else {
		err = a.pgApplyCert(r.Context(), dep.ContainerID, intranetID, fqdn, pgDataDir(cfg.OS, cfg.PGMajor), b.Value, b.Unit, nil)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reflect that the node now carries an Intranet-signed certificate. The concrete
	// config type varies by engine, so merge into the raw JSON to preserve all fields.
	if !cfg.GenerateCert {
		var m map[string]any
		if json.Unmarshal(dep.Config, &m) == nil {
			m["generateCert"] = true
			cfgJSON, _ := json.Marshal(m)
			a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
		}
	}

	dir := pgCertDir(nodeType, cfg.OS, cfg.PGMajor)
	out, _ := a.execScript(r.Context(), dep.ContainerID, pgCertInfoScript, []string{"DIR=" + dir})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "result": strings.TrimSpace(out)})
}
