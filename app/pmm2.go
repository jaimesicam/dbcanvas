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

// pmm2.go — a PMM 2 server, offered only with EOL=on (eol.go).
//
// PMM 2 reached end of life with 2.44.1 in 2025, and PMM 3 is a different product in the ways
// that matter here: different ports (80/443 rather than 8080/8443), a different client, and an API
// that moved. So this is its own node type rather than a version of the PMM node, and it is
// deliberately small. It deploys a PMM 2 server at the version you pick, sets its admin password,
// publishes its console, and stops there: no Watchtower, no Intranet certificate, no Grafana SMTP,
// no LDAP or Keycloak, and nothing on the canvas can be pointed at it for monitoring. Every
// association picker in the designer and every provisioner here looks for Type "pmm", so a
// "pmm2" node is invisible to all of them by construction.
//
// Because PMM 2 will never change again, its versions are a constant list rather than something
// `make versions` probes — there is nothing new to discover.

// pmm2Repository is PMM 2's image. amd64 only: percona/pmm-server never had an arm64 build of 2.x.
const pmm2Repository = "percona/pmm-server"

// pmm2Versions is every PMM 2 release this node offers, newest first — each is an exact
// percona/pmm-server tag. 2.44.1 was the last PMM 2 release.
//
// The list starts at 2.25.0 because that is as far back as it was checked. Both ends were
// deployed through the same steps provisionPMM2 runs: /v1/readyz answers on port 80, and the admin
// password is set — by change-admin-password where the image has it, and by grafana-cli where it
// does not (2.25.0 predates the helper). Older 2.x images exist on Docker Hub and may well work;
// they are not offered because nobody has watched one come up.
var pmm2Versions = []string{
	"2.44.1", "2.44.0",
	"2.43.2", "2.43.1", "2.43.0",
	"2.42.0",
	"2.41.2", "2.41.1", "2.41.0",
	"2.40.1", "2.40.0",
	"2.39.0",
	"2.38.1", "2.38.0",
	"2.37.1", "2.37.0",
	"2.36.0", "2.35.0", "2.34.0", "2.33.0", "2.32.0", "2.31.0", "2.30.0",
	"2.29.1", "2.29.0",
	"2.28.0", "2.27.0", "2.26.0", "2.25.0",
}

// pmm2DefaultVersion is what an unset version deploys: the last release.
const pmm2DefaultVersion = "2.44.1"

// pmm2Version resolves a node's version to a tag this node offers, or "" when it is not one.
func pmm2Version(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return pmm2DefaultVersion
	}
	for _, known := range pmm2Versions {
		if v == known {
			return v
		}
	}
	return ""
}

// pmm2Config is the non-secret profile shown for a deployed PMM 2 node.
type pmm2Config struct {
	Image     string `json:"image"`
	Version   string `json:"version"`
	Hostname  string `json:"hostname"`
	FQDN      string `json:"fqdn"`
	AdminUser string `json:"adminUser"`
	HTTPPort  int    `json:"httpPort"`  // host port mapped to container 80
	HTTPSPort int    `json:"httpsPort"` // host port mapped to container 443
}

// pmm2DataVolume is a PMM 2 node's /srv, kept across a redeploy like PMM 3's (pmmDataVolume) and
// removed with the stack.
func pmm2DataVolume(stackID int64, nodeID string) string {
	return fmt.Sprintf("dbcanvas-pmm2-%d-%s", stackID, nodeID)
}

// pmm2AdminPasswordScript sets the Grafana admin password — which is the PMM login. PMM 2 grew a
// change-admin-password helper partway through its life; before it, the documented way was
// grafana-cli against PMM's own Grafana data directory. Both are tried, newest first.
const pmm2AdminPasswordScript = `set -e
if command -v change-admin-password >/dev/null 2>&1; then
  change-admin-password "$PW" >/dev/null
else
  grafana-cli --homepath /usr/share/grafana --configOverrides cfg:default.paths.data=/srv/grafana \
    admin reset-admin-password "$PW" >/dev/null
fi
curl -fsS -o /dev/null -u "admin:$PW" http://localhost/v1/version`

func (a *App) provisionPMM2(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = pmmAlias(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	tag := pmm2Version(n.Version)
	ref := pmm2Repository + ":" + tag

	// Keep the published ports across a redeploy, so the console URL does not move.
	httpPort, httpsPort := 0, 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old pmm2Config
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort, httpsPort = old.HTTPPort, old.HTTPSPort
		}
	}
	for _, p := range []*int{&httpPort, &httpsPort} {
		if *p == 0 {
			if free, err := freeHostPort(); err == nil {
				*p = free
			}
		}
	}
	sec := pmmSecrets{AdminUser: "admin", AdminPassword: envOr("PMM_ADMIN_PASSWORD", "admin_password")}
	cfg := pmm2Config{Image: ref, Version: tag, Hostname: host, FQDN: fqdn, AdminUser: sec.AdminUser,
		HTTPPort: httpPort, HTTPSPort: httpsPort}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		if tag == "" {
			pr.fail("%q is not a PMM 2 release this node offers — pick one of %s … %s",
				n.Version, pmm2Versions[len(pmm2Versions)-1], pmm2Versions[0])
			return
		}

		pr.phase("Pulling image", 5)
		pr.logln("ensuring " + ref + " for " + platformAMD64 + " (this can take a while)")
		if err := a.engCtx(ctx).EnsureImage(ctx, pmm2Repository, tag, platformAMD64); err != nil {
			pr.fail("pull image %s: %v", ref, err)
			return
		}

		// The Intranet is still the stack's resolver, so the console's name resolves from the
		// other nodes — that is the one thing this node takes from the stack.
		pr.phase("Waiting for Intranet to be ready", 15)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 20)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		vol := pmm2DataVolume(st.ID, n.ID)
		if err := a.engCtx(ctx).VolumeCreate(ctx, vol); err != nil {
			pr.logln("warning: could not create the PMM 2 data volume: " + err.Error())
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: ref, Hostname: host, Platform: platformAMD64,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: 80, HostPort: httpPort}, {ContainerPort: 443, HostPort: httpsPort}},
			Binds:      []string{vol + ":/srv"},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}
		a.pmm2ReadPorts(ctx, id, &cfg)
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		pr.logln(fmt.Sprintf("container started (http %d, https %d)", cfg.HTTPPort, cfg.HTTPSPort))

		pr.phase("Waiting for PMM 2 server", 40)
		if err := a.waitPMM2Ready(ctx, id, 240*time.Second); err != nil {
			pr.fail("PMM 2 did not become ready: %v", err)
			return
		}

		pr.phase("Setting admin password", 75)
		if err := a.runStep(ctx, id, pmm2AdminPasswordScript, []string{"PW=" + sec.AdminPassword}, pr.logln); err != nil {
			pr.fail("set admin password: %v", err)
			return
		}
		pr.logln("admin password set (PMM_ADMIN_PASSWORD)")

		a.reconcileStackDNS(ctx, st.ID)
		pr.logln("PMM " + tag + " is end of life — deployed on its own, not wired to any other node")
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d pmm2 %s: provisioned %s", st.ID, n.ID, ref)
	}()
}

// pmm2ReadPorts records the host ports Docker actually published, which is what the console links
// are built from.
func (a *App) pmm2ReadPorts(ctx context.Context, id string, cfg *pmm2Config) {
	if hp, err := a.engCtx(ctx).ContainerPort(ctx, id, "80/tcp"); err == nil {
		if p, err := strconv.Atoi(hp); err == nil {
			cfg.HTTPPort = p
		}
	}
	if hp, err := a.engCtx(ctx).ContainerPort(ctx, id, "443/tcp"); err == nil {
		if p, err := strconv.Atoi(hp); err == nil {
			cfg.HTTPSPort = p
		}
	}
}

// waitPMM2Ready polls PMM 2's readiness endpoint, which is on port 80 inside the container (PMM 3
// moved it to 8080).
func (a *App) waitPMM2Ready(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	script := `curl -fsS -o /dev/null -w '%{http_code}' http://localhost/v1/readyz 2>/dev/null`
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", script}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) == "200" {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("/v1/readyz not 200 within %s", timeout)
}

// handlePMM2Catalog serves the fixed PMM 2 version list, so the designer's picker and this file
// cannot disagree about what is offered.
func (a *App) handlePMM2Catalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": pmm2Versions, "default": pmm2DefaultVersion})
}
