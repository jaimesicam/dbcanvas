package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Watchtower node (Type=="watchtower"; a per-stack singleton). It runs Percona's
// Watchtower image (percona/watchtower, pulled at deploy) with the docker socket
// mounted and its HTTP API enabled, so a PMM node associated with it can trigger
// in-app server upgrades (PMM calls Watchtower's API to pull + recreate the PMM
// container). It is reached in-network by the PMM node at http://<watchtower>:8080
// using a shared API token; nothing is published to the host.
//
// Modeled on the example:
//
//	docker run -d --network <net> \
//	  -e WATCHTOWER_HTTP_API_TOKEN=<token> -e WATCHTOWER_HTTP_API_UPDATE=1 \
//	  -v /var/run/docker.sock:/var/run/docker.sock \
//	  --name watchtower percona/watchtower:latest
//
// and the PMM container then gets PMM_WATCHTOWER_HOST + PMM_WATCHTOWER_TOKEN.

const (
	watchtowerImage   = "percona/watchtower:latest"
	watchtowerAPIPort = 8080
)

// watchtowerConfig is the non-secret profile shown for a deployed Watchtower node.
type watchtowerConfig struct {
	Image    string `json:"image"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	Alias    string `json:"alias"`
	APIPort  int    `json:"apiPort"` // HTTP API port (in-network only; not published)
}

// watchtowerSecrets holds the HTTP API token shared with the PMM node(s).
type watchtowerSecrets struct {
	APIToken string `json:"apiToken"`
}

// genWatchtowerToken returns a 32-hex-character random API token.
func genWatchtowerToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// provisionWatchtower records the deployment then runs an async goroutine that pulls
// the Watchtower image and starts the container with the docker socket mounted and
// the HTTP API enabled.
func (a *App) provisionWatchtower(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the API token across redeploys so an already-associated PMM node keeps
	// working without a redeploy.
	token := ""
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s watchtowerSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil {
			token = s.APIToken
		}
	}
	if token == "" {
		token = genWatchtowerToken()
	}
	sec := watchtowerSecrets{APIToken: token}
	secJSON, _ := json.Marshal(sec)

	cfg := watchtowerConfig{Image: watchtowerImage, Hostname: host, FQDN: fqdn, Alias: host, APIPort: watchtowerAPIPort}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID)
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Pulling image", 10)
		if ok, _ := a.docker.ImageExists(ctx, watchtowerImage); !ok {
			pr.logln("pulling " + watchtowerImage)
			if err := a.docker.ImagePull(ctx, "percona/watchtower", "latest"); err != nil {
				pr.fail("pull image: %v", err)
				return
			}
		}

		pr.phase("Waiting for Intranet to be ready", 30)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 55)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		aliases := []string{host}
		if host != "watchtower" {
			aliases = append(aliases, "watchtower")
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: watchtowerImage, Hostname: host,
			Env: []string{
				"WATCHTOWER_HTTP_API_TOKEN=" + token,
				"WATCHTOWER_HTTP_API_UPDATE=1",
			},
			Binds:   []string{"/var/run/docker.sock:/var/run/docker.sock"},
			Network: networkName(st.ID), Aliases: aliases,
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d watchtower %s: provisioned", st.ID, n.Label)
	}()
}

// waitWatchtower waits (bounded) for a Watchtower node to be running and returns its
// FQDN + API token (used to wire a PMM node's PMM_WATCHTOWER_* env).
func (a *App) waitWatchtower(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (fqdn, token string, ok bool) {
	if nodeID == "" {
		return "", "", false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return "", "", false
			}
			if dep.State == DeployRunning {
				var cfg watchtowerConfig
				var sec watchtowerSecrets
				json.Unmarshal(dep.Config, &cfg)
				json.Unmarshal(dep.Secrets, &sec)
				if sec.APIToken != "" {
					return cfg.FQDN, sec.APIToken, true
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", "", false
}

// watchtowerHostEnv builds the PMM_WATCHTOWER_* env for a PMM container associated
// with the given Watchtower fqdn + token.
func watchtowerHostEnv(fqdn, token string) []string {
	return []string{
		fmt.Sprintf("PMM_WATCHTOWER_HOST=http://%s:%d", fqdn, watchtowerAPIPort),
		"PMM_WATCHTOWER_TOKEN=" + token,
	}
}
