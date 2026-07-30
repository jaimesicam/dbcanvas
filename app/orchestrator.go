package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Percona Orchestrator node (Type=="orchestrator"). A standalone topology
// visualization / failure-detection service for PXC and MySQL-replication clusters,
// installed from RPM/DEB (percona-orchestrator + percona-orchestrator-client) on a
// systemd OS image — like standalone Percona Server (mysql.go), not a pulled image
// (PMM). PXC and MySQL-replication frames optionally point at it via
// designFrame.OrchestratorNodeID — the same optional 1:N relationship PMMNodeID
// already has (see pmmServerFor) — rather than a canvas association edge.
//
// Runs its own embedded SQLite backend (BackendDB: sqlite; no extra database to
// provision) and serves its web UI on :3000, always published to the host — like
// PMM and the app simulators, not an opt-in toggle (there's no cluster of these to
// avoid host-port clashes between).
//
// The topology user every MySQL-family node already creates unconditionally
// (orchestrator@'%', see mysqlFamilySecrets / pxcBootstrapScript / mysqlBaselineScript)
// is configured here as Orchestrator's single, cluster-wide MySQLTopologyUser/
// Password: every linked cluster shares the same credential (from .env's
// ORCHESTRATOR_PASSWORD), so one Orchestrator config works for all of them.
//
// Package/paths verified against a live install (both RHEL and Debian systemd
// images) rather than assumed:
//   - percona-orchestrator (server + web UI + systemd unit) and
//     percona-orchestrator-client (the `orchestrator-client` CLI) are reachable from
//     any enabled PDPS repo ("Percona Distribution for MySQL" — percona-server
//     based). PDPXC ("...- PXC", the Galera-based distribution) does NOT carry the
//     package at all — confirmed live, despite both being plausible-sounding percona-
//     release repo families — so PDPS is the only one this uses, regardless of
//     which kind of cluster (PXC or plain replication) ends up linked to it.
//   - The RHEL package ships /lib/systemd/system/orchestrator.service; the Debian
//     package does NOT ship a unit at all, so one is written here for Debian/Ubuntu.
//   - Config is auto-loaded from /etc/orchestrator.conf.json (no -config flag
//     needed on the systemd ExecStart line).
//   - The orchestrator-client wrapper script requires `curl` and `which` on PATH;
//     `which` is missing by default on the Oracle Linux systemd image.
//   - `orchestrator-client -c discover -i host:port` talks to http://localhost:3000
//     by default, matching our ListenAddress.

const (
	orchestratorPort = 3000
	orchestratorUnit = "orchestrator"
)

// orchestratorConfig is the non-secret profile shown for a deployed Orchestrator node.
type orchestratorConfig struct {
	Image      string `json:"image"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Hostname   string `json:"hostname"`
	FQDN       string `json:"fqdn"`
	Version    string `json:"version"`
	AlertEmail string `json:"alertEmail"`
	ExportPort int    `json:"exportPort"` // published host port for :3000 (0 = none)
}

// orchestratorRepo picks a PDPS percona-release repo to enable so the
// percona-orchestrator package is reachable (confirmed live: PDPXC does not carry
// it at all, so only PDPS is used — see the package comment above). This just
// takes the PDPS catalog's first entry, the same (unsorted) choice the InnoDB/
// Group Replication frame's own picker already defaults to (loadPDPSCatalog,
// StackDesigner.jsx's pdpsRepo default) — every PDPS repo carries the same
// percona-orchestrator package family, just at different pinned versions, and
// `make versions` records the actual installable minors against whichever one
// this resolves to. Falls back to a known-good name when `make versions` has
// never run.
func orchestratorRepo() string {
	if repos := loadPDPSCatalog(); len(repos) > 0 {
		return repos[0]
	}
	return "pdps-84-lts"
}

// provisionOrchestrator records + provisions a standalone Orchestrator node.
func (a *App) provisionOrchestrator(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	image := pxcImage(n.OS, n.OSVersion, n.Arch)
	sec := mysqlFamilySecrets()
	alertEmail := strings.TrimSpace(n.AlertEmail)

	// The web UI is always published to the host — like PMM and the app simulators,
	// not an opt-in toggle (ExportEnabled/ExportHostPort, used by ps/pg/haproxy)
	// since there's no cluster of these to avoid port clashes between. Reuse the
	// previous deploy's host port so the URL survives a redeploy (same reasoning as
	// PMM's httpPort/httpsPort — see pmm.go).
	webPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old orchestratorConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			webPort = old.ExportPort
		}
	}
	if webPort == 0 {
		if p, e := freeHostPort(); e == nil {
			webPort = p
		}
	}

	cfg := orchestratorConfig{
		Image: image, OS: n.OS, Arch: archOr(n.Arch), Hostname: host, FQDN: fqdnOf(host, domain),
		Version: n.OrchestratorVersion, AlertEmail: alertEmail, ExportPort: webPort,
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		pr.phase("Waiting for Intranet to be ready", 5)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 15)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		spec := ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		}
		applyVMSize(&spec, n.CPUs, n.MemoryGB)
		spec.PublishMap = []PortMap{{ContainerPort: orchestratorPort, HostPort: webPort}}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, spec)
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON})

		pr.phase("Waiting for systemd", 25)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		a.ensureDNFIPv4(ctx, id, n.OS, pr.logln)

		pr.phase("Installing Percona Orchestrator", 40)
		repo := orchestratorRepo()
		proxy := ""
		if n.UseProxy {
			proxy = "http://intranet." + domain + ":3128"
		}
		script := orchestratorInstallRHEL
		if isDebianOS(n.OS) {
			script = orchestratorInstallDebian
		}
		env := []string{"REPO=" + repo, "VER=" + n.OrchestratorVersion, "PROXY=" + proxy}
		if err := a.runStep(ctx, id, script, env, pr.logln); err != nil {
			pr.fail("install percona-orchestrator: %v", err)
			return
		}
		pr.logln("percona-orchestrator installed from " + repo)
		a.ensureRsyslog(ctx, id, n.OS, pr.logln)

		pr.phase("Configuring Orchestrator", 60)
		if alertEmail != "" {
			if err := a.engCtx(ctx).CopyFile(ctx, id, "/usr/local/bin", "dbcanvas-orch-alert.sh", 0o755,
				[]byte(orchestratorAlertScript(alertEmail, domain))); err != nil {
				pr.fail("write alert hook: %v", err)
				return
			}
			pr.logln("failure-detection alerts wired to " + alertEmailAddress(alertEmail, domain))
		}
		if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc", "orchestrator.conf.json", 0o644,
			[]byte(orchestratorConfJSON(sec, alertEmail))); err != nil {
			pr.fail("write orchestrator.conf.json: %v", err)
			return
		}
		if isDebianOS(n.OS) {
			// The Debian/Ubuntu package ships no systemd unit at all (verified
			// against a live install) — write the same one the RHEL package
			// carries (/lib/systemd/system/orchestrator.service).
			if err := a.engCtx(ctx).CopyFile(ctx, id, "/etc/systemd/system", "orchestrator.service", 0o644,
				[]byte(orchestratorUnitFile)); err != nil {
				pr.fail("write orchestrator.service: %v", err)
				return
			}
			if err := a.runStep(ctx, id, "systemctl daemon-reload", nil, pr.logln); err != nil {
				pr.fail("daemon-reload: %v", err)
				return
			}
		}

		pr.phase("Starting Orchestrator", 80)
		if err := a.runStep(ctx, id,
			"systemctl reset-failed "+orchestratorUnit+" 2>/dev/null || true; systemctl enable --now "+orchestratorUnit,
			nil, pr.logln); err != nil {
			pr.fail("start orchestrator: %v", err)
			return
		}
		if err := a.waitOrchestratorReady(ctx, id, 60*time.Second); err != nil {
			pr.fail("%v", err)
			return
		}
		pr.logln("Orchestrator web UI ready on :" + fmt.Sprint(orchestratorPort))

		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", orchestratorPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.ExportPort = p
			}
		}
		cfgJSON2, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON2})
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d orchestrator %s: provisioned", st.ID, n.ID)
	}()
}

// waitOrchestratorReady polls Orchestrator's own health endpoint until it answers
// healthy, mirroring waitPMMReady.
func (a *App) waitOrchestratorReady(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	script := fmt.Sprintf(`curl -fsS http://localhost:%d/api/status 2>/dev/null | grep -q '"Healthy":true' && echo ok`, orchestratorPort)
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", script}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) == "ok" {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("Orchestrator did not become healthy within %s", timeout)
}

// orchestratorDiscover seeds/refreshes Orchestrator's topology discovery for a set
// of cluster members, by running `orchestrator-client -c discover` against each
// one's MySQL port from inside the Orchestrator node itself. Idempotent — safe to
// call repeatedly (a redeploy, or the live re-link management endpoint). Best-effort:
// failures are logged, never fatal, matching pxcRegisterPMM's contract.
func (a *App) orchestratorDiscover(ctx context.Context, orchestratorContainerID string, members []pxcMember, logln func(string)) {
	if logln == nil {
		logln = func(string) {}
	}
	ok := 0
	for _, m := range members {
		target := fmt.Sprintf("%s:%d", m.FQDN, pxcMySQLPort)
		if _, err := a.engCtx(ctx).Exec(ctx, orchestratorContainerID, []string{"orchestrator-client", "-c", "discover", "-i", target}, nil); err != nil {
			logln("orchestrator discover " + target + " failed: " + err.Error())
			continue
		}
		ok++
	}
	logln(fmt.Sprintf("discovery seeded for %d/%d member(s)", ok, len(members)))
}

// registerFrameWithOrchestrator is the Orchestrator analog of pxcRegisterPMM: called
// from the end of a PXC or MySQL-replication frame's own provisioning (Phase 3),
// when the frame has an OrchestratorNodeID linked. Resolves the Orchestrator node's
// container, gets the frame's already-running members via the given wait function
// (waitPXCMembers / waitMySQLReplMembers — both return []pxcMember), and seeds
// discovery. Best-effort — logged via pr, never fails the frame's deploy.
func (a *App) registerOrchestrator(ctx context.Context, st Stack, orchestratorNodeID string, members []pxcMember, logln func(string)) {
	if orchestratorNodeID == "" || len(members) == 0 {
		return
	}
	dep, err := a.store.GetDeployment(st.ID, orchestratorNodeID)
	if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
		logln("Orchestrator registration skipped: the Orchestrator node is not running")
		return
	}
	a.orchestratorDiscover(ctx, dep.ContainerID, members, logln)
}

// ---------------------------------------------------------------- management

// handleFrameOrchestrator turns Orchestrator monitoring on or off for an already
// deployed PXC or MySQL-replication cluster — the Orchestrator analog of
// handlePXCFrameMonitor (app/pxc_mgmt.go), covering both frame types with one
// handler/route since the logic (resolve members, seed discovery, persist the
// link) is otherwise identical. Body: {"orchestratorNodeId": "<id>"} to seed
// discovery against that Orchestrator node, or "" to just clear the link (nothing
// is un-discovered on the Orchestrator side — that mirrors upstream Orchestrator's
// own model, where forgetting an instance is a separate, explicit action).
func (a *App) handleFrameOrchestrator(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	fid := r.PathValue("fid")
	var b struct {
		OrchestratorNodeID string `json:"orchestratorNodeId"`
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
		if f.ID == fid && (f.Type == "pxc" || f.Type == "mysql") {
			frame, found = f, true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "PXC or MySQL replication cluster not found")
		return
	}

	orchestratorNodeID := strings.TrimSpace(b.OrchestratorNodeID)
	orchestratedBy := ""
	var orchestratorDep Deployment
	if orchestratorNodeID != "" {
		dep, err := a.store.GetDeployment(st.ID, orchestratorNodeID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			writeErr(w, http.StatusConflict, "the selected Orchestrator node is not running")
			return
		}
		orchestratorDep = dep
		hosts := stackHostnames(doc)
		domain := envOr("DOMAIN", "example.net")
		for _, n := range doc.Nodes {
			if n.ID == orchestratorNodeID {
				orchestratedBy = fqdnOf(hosts[n.ID], domain)
			}
		}
	}

	// Every member of a pxc/mysql frame runs on the same engine (a VM in a hybrid stack).
	ctx := withEngine(r.Context(), a.nodeEngine(st, frame.Type))
	hosts := stackHostnames(doc)
	domain := envOr("DOMAIN", "example.net")
	var members []pxcMember
	updated := 0
	for _, n := range doc.Nodes {
		if n.FrameID != fid || n.Type != frame.Type || n.Role == "arbitrator" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.ContainerID == "" || dep.State != DeployRunning {
			continue
		}
		members = append(members, pxcMember{FQDN: fqdnOf(hosts[n.ID], domain), ContainerID: dep.ContainerID})

		switch frame.Type {
		case "pxc":
			var cfg pxcConfig
			json.Unmarshal(dep.Config, &cfg)
			cfg.OrchestratedBy = orchestratedBy
			cfgJSON, _ := json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
		case "mysql":
			var cfg mysqlConfig
			json.Unmarshal(dep.Config, &cfg)
			cfg.OrchestratedBy = orchestratedBy
			cfgJSON, _ := json.Marshal(cfg)
			a.store.UpsertDeployment(Deployment{StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID, State: dep.State, Config: cfgJSON, Secrets: dep.Secrets})
		}
		updated++
	}

	if orchestratorNodeID != "" {
		a.orchestratorDiscover(ctx, orchestratorDep.ContainerID, members, nil)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "orchestratedBy": orchestratedBy, "updated": updated})
}

// alertEmailAddress qualifies a bare local-part with the stack's domain (a full
// address, i.e. containing '@', is used verbatim).
func alertEmailAddress(alertEmail, domain string) string {
	if strings.Contains(alertEmail, "@") {
		return alertEmail
	}
	return alertEmail + "@" + domain
}

// orchestratorConfJSON renders /etc/orchestrator.conf.json: the embedded SQLite
// backend (no extra database to provision), the shared cluster-wide topology user
// every MySQL-family node already creates, and — when alertEmail is set — the
// failure-detection hook wired to the alert script staged alongside it.
func orchestratorConfJSON(sec pxcSecrets, alertEmail string) string {
	hooks := "[]"
	if alertEmail != "" {
		hooks = `["bash /usr/local/bin/dbcanvas-orch-alert.sh '{failureType}' '{failureCluster}' '{failedHost}' '{failedPort}'"]`
	}
	return fmt.Sprintf(`{
  "ListenAddress": ":%d",
  "BackendDB": "sqlite",
  "SQLite3DataFile": "/usr/local/orchestrator/orchestrator.sqlite3",
  "MySQLTopologyUser": %q,
  "MySQLTopologyPassword": %q,
  "MySQLTopologySSLSkipVerify": true,
  "DefaultInstancePort": %d,
  "DiscoverByShowSlaveHosts": true,
  "InstancePollSeconds": 5,
  "AuthenticationMethod": "",
  "OnFailureDetectionProcesses": %s
}
`, orchestratorPort, sec.OrchestratorUser, sec.OrchestratorPassword, pxcMySQLPort, hooks)
}

// orchestratorUnitFile is the systemd unit for the Debian/Ubuntu package, which
// (verified against a live install) ships no unit of its own — this mirrors the one
// the RHEL package does carry (/lib/systemd/system/orchestrator.service).
const orchestratorUnitFile = `[Unit]
Description=orchestrator: MySQL replication management and visualization
Documentation=https://github.com/openark/orchestrator
After=network.target

[Service]
Type=simple
WorkingDirectory=/usr/local/orchestrator
ExecStart=/usr/local/orchestrator/orchestrator http
ExecReload=/bin/kill -HUP $MAINPID
LimitNOFILE=16384
Restart=on-failure

[Install]
WantedBy=multi-user.target
`

// orchestratorAlertScriptTpl emails a failure-detection alert straight to the
// Intranet mail server: no local MTA is installed on this node — Postfix's default
// mynetworks_style=subnet already trusts the whole stack network, the same way
// PMM's Grafana SMTP integration relays through intranet:25 with no auth (see
// pmmSMTPScript). The recipient/relay are baked in literally at install time (not
// threaded through Orchestrator's own process environment, whose inheritance by a
// hook subprocess was not verified live) — changing AlertEmail means redeploying
// this node.
//
// Every SMTP response is read and discarded before the next command is sent
// (including after DATA's terminating "." and after QUIT) — skipping those reads
// leaves server responses unread in the kernel receive buffer, so closing the
// socket sends a TCP RST instead of a clean FIN and the message can be lost
// mid-delivery. Verified against a live raw-SMTP handshake test.
const orchestratorAlertScriptTpl = `#!/bin/bash
TO='__TO__'
FROM='orchestrator@__DOMAIN__'
RELAY='intranet.__DOMAIN__'
SUBJECT="Orchestrator alert: $1 on $2"
BODY="Failure type: $1
Cluster: $2
Failed host: $3:$4
Detected at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

exec 3<>"/dev/tcp/$RELAY/25" 2>/dev/null || exit 0
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'EHLO orchestrator.__DOMAIN__\r\n' >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'MAIL FROM:<%s>\r\n' "$FROM" >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'RCPT TO:<%s>\r\n' "$TO" >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'DATA\r\n' >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n.\r\n' "$FROM" "$TO" "$SUBJECT" "$BODY" >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
printf 'QUIT\r\n' >&3
IFS= read -r -u3 -t 5 _ 2>/dev/null
exec 3<&- 3>&- 2>/dev/null
`

func orchestratorAlertScript(alertEmail, domain string) string {
	to := alertEmailAddress(alertEmail, domain)
	r := strings.NewReplacer("__TO__", to, "__DOMAIN__", domain)
	return r.Replace(orchestratorAlertScriptTpl)
}

// ------------------------------------------------------------------ scripts

// orchestratorInstall{RHEL,Debian} enable a pdps/pdpxc percona-release repo and
// install percona-orchestrator + percona-orchestrator-client, plus curl/which —
// which the orchestrator-client wrapper script requires on PATH and which is not
// present by default on every systemd image (verified live: missing on Oracle
// Linux 9). No version pinning beyond percona-release's own repo choice: unlike
// PXC/PS, percona-orchestrator has no per-major-series split, so $VER (when set)
// only needs to match its own release numbering.
const orchestratorInstallRHEL = pinInstallRHEL + `set -e
if [ -n "$PROXY" ]; then grep -q '^proxy=' /etc/dnf/dnf.conf 2>/dev/null || echo "proxy=$PROXY" >> /etc/dnf/dnf.conf; fi
percona-release enable -y "$REPO" >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1
dnf -y -q install which curl >/dev/null 2>&1 || true
pin_install percona-orchestrator percona-orchestrator-client`

const orchestratorInstallDebian = pinInstallDebian + `set -e
export DEBIAN_FRONTEND=noninteractive
if [ -n "$PROXY" ]; then echo "Acquire::http::Proxy \"$PROXY\";" > /etc/apt/apt.conf.d/01dbcanvas-proxy; fi
percona-release enable -y "$REPO" >/dev/null 2>&1 || percona-release setup -y "$REPO" >/dev/null 2>&1
apt-get update -qq >/dev/null
apt-get install -y -qq which curl >/dev/null 2>&1 || true
pin_install percona-orchestrator percona-orchestrator-client`
