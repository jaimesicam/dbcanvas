package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
)

// Keycloak node (Type=="keycloak"; a per-stack singleton). It runs the upstream
// Keycloak image in dev mode and is used as an OpenID Connect (OIDC) identity
// provider — e.g. a standalone Percona Server for MongoDB node can enable
// MONGODB-OIDC authentication against it.
//
// Modeled on the example:
//
//	docker run -p 8080:8080 -p 8443:8443 \
//	  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
//	  --hostname=keycloak --name=keycloak --network=<net> \
//	  quay.io/keycloak/keycloak:26.5.5 start-dev --https-port=8443
//
// The container hostname/alias is the node's host (sanitized label, normally
// "keycloak"), so in dev mode Keycloak issues tokens with that issuer host — which
// is exactly what a MongoDB node points its oidcIdentityProviders.issuer at
// (http://<host>:8080/realms/<realm>). The admin console is published to the host on
// auto-assigned ports (8080 http / 8443 https).

const (
	keycloakImage     = "quay.io/keycloak/keycloak:26.5.5"
	keycloakImageRepo = "quay.io/keycloak/keycloak"
	keycloakImageTag  = "26.5.5"
	keycloakHTTPPort  = 8080
	keycloakHTTPSPort = 8443
)

// keycloakConfig is the non-secret profile shown for a deployed Keycloak node.
type keycloakConfig struct {
	Image     string `json:"image"`
	Hostname  string `json:"hostname"`
	FQDN      string `json:"fqdn"`
	Alias     string `json:"alias"`
	HTTPPort  int    `json:"httpPort"`  // published host port → container 8080 (0 if unpublished)
	HTTPSPort int    `json:"httpsPort"` // published host port → container 8443 (0 if unpublished)
	AdminUser string `json:"adminUser"`
}

// keycloakSecrets holds the bootstrap admin password.
type keycloakSecrets struct {
	AdminPassword string `json:"adminPassword"`
}

// keycloakIssuer returns the OIDC issuer base URL a MongoDB node uses to reach this
// Keycloak in-network (http://<host>:8080). The realm is appended by the caller.
func keycloakIssuer(host string) string {
	return fmt.Sprintf("http://%s:%d", host, keycloakHTTPPort)
}

// provisionKeycloak records the deployment then runs an async goroutine that pulls
// the Keycloak image and starts it in dev mode with the admin bootstrap credentials.
func (a *App) provisionKeycloak(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the admin password across redeploys (a Keycloak realm/client set up in the
	// console survives a redeploy only if the data does, but keep the password stable).
	adminPW := ""
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s keycloakSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil {
			adminPW = s.AdminPassword
		}
	}
	if adminPW == "" {
		adminPW = genSecret("KcAdmin!")
	}
	sec := keycloakSecrets{AdminPassword: adminPW}
	secJSON, _ := json.Marshal(sec)

	cfg := keycloakConfig{Image: keycloakImage, Hostname: host, FQDN: fqdn, Alias: host, AdminUser: "admin"}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Pulling image", 10)
		if ok, _ := a.docker.ImageExists(ctx, keycloakImage); !ok {
			pr.logln("pulling " + keycloakImage)
			if err := a.docker.ImagePull(ctx, keycloakImageRepo, keycloakImageTag); err != nil {
				pr.fail("pull image: %v", err)
				return
			}
		}

		pr.phase("Waiting for Intranet to be ready", 30)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
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
		if host != "keycloak" {
			aliases = append(aliases, "keycloak")
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: keycloakImage, Hostname: host,
			Cmd: []string{"start-dev", fmt.Sprintf("--https-port=%d", keycloakHTTPSPort)},
			Env: []string{
				"KC_BOOTSTRAP_ADMIN_USERNAME=" + cfg.AdminUser,
				"KC_BOOTSTRAP_ADMIN_PASSWORD=" + adminPW,
			},
			Network: networkName(st.ID), Aliases: aliases,
			PublishMap: []PortMap{{ContainerPort: keycloakHTTPPort}, {ContainerPort: keycloakHTTPSPort}},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			pr.fail("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			pr.fail("start container: %v", err)
			return
		}

		// Record the auto-assigned host ports for the admin console.
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", keycloakHTTPPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", keycloakHTTPSPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPSPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d keycloak %s: provisioned (console http port %d)", st.ID, n.Label, cfg.HTTPPort)
	}()
}

// waitKeycloak waits (bounded) for a Keycloak node to be running and returns its
// in-network host (for building the OIDC issuer). ok=false if it never comes up.
func (a *App) waitKeycloak(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (host string, ok bool) {
	if nodeID == "" {
		return "", false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return "", false
			}
			if dep.State == DeployRunning {
				var cfg keycloakConfig
				if json.Unmarshal(dep.Config, &cfg) == nil && cfg.Hostname != "" {
					return cfg.Hostname, true
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", false
}
