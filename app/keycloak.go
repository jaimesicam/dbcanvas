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
	SSL       bool   `json:"ssl"` // serves HTTPS with an Intranet-CA cert (required for MongoDB OIDC)
}

// keycloakSecrets holds the bootstrap admin password.
type keycloakSecrets struct {
	AdminPassword string `json:"adminPassword"`
}

// keycloakIssuer returns the OIDC issuer base URL a MongoDB node uses to reach this
// Keycloak in-network. The realm is appended by the caller. MongoDB OIDC requires an
// HTTPS issuer for non-local hosts, so an SSL Keycloak issues https://<fqdn>:8443.
func keycloakIssuer(host string, ssl bool) string {
	if ssl {
		return fmt.Sprintf("https://%s:%d", host, keycloakHTTPSPort)
	}
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

	cfg := keycloakConfig{Image: keycloakImage, Hostname: host, FQDN: fqdn, Alias: host, AdminUser: "admin", SSL: n.GenerateCert}
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
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// When SSL is requested, sign a server cert for the Keycloak FQDN with the
		// Intranet CA — MongoDB OIDC requires an HTTPS issuer.
		var tlsCert, tlsKey []byte
		if n.GenerateCert {
			pr.phase("Issuing TLS certificate", 45)
			if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
				pr.fail("%v", err)
				return
			}
			caCrt, cerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
			caKey, kerr := a.readContainerFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
			if cerr != nil || kerr != nil || len(caCrt) == 0 || len(caKey) == 0 {
				pr.fail("read Intranet CA: %v %v", cerr, kerr)
				return
			}
			c, k, serr := signTLSCert(caCrt, caKey, fqdn, []string{fqdn, host, "keycloak"}, certTTL(n.CertTTLValue, n.CertTTLUnit))
			if serr != nil {
				pr.fail("sign certificate: %v", serr)
				return
			}
			tlsCert, tlsKey = c, k
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
		cmd := []string{"start-dev", fmt.Sprintf("--https-port=%d", keycloakHTTPSPort)}
		if n.GenerateCert {
			cmd = []string{
				"start-dev",
				"--http-enabled=true", fmt.Sprintf("--http-port=%d", keycloakHTTPPort),
				fmt.Sprintf("--https-port=%d", keycloakHTTPSPort),
				"--https-certificate-file=/opt/keycloak/conf/tls.crt",
				"--https-certificate-key-file=/opt/keycloak/conf/tls.key",
				fmt.Sprintf("--hostname=https://%s:%d", fqdn, keycloakHTTPSPort),
			}
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: keycloakImage, Hostname: host,
			Cmd: cmd,
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
		// Stage the cert into the created (not-yet-started) container before launch.
		if n.GenerateCert {
			if err := a.docker.CopyFile(ctx, id, "/opt/keycloak/conf", "tls.crt", 0o644, tlsCert); err != nil {
				pr.fail("copy tls cert: %v", err)
				return
			}
			if err := a.docker.CopyFile(ctx, id, "/opt/keycloak/conf", "tls.key", 0o644, tlsKey); err != nil {
				pr.fail("copy tls key: %v", err)
				return
			}
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
// in-network host + whether it serves SSL (for building the OIDC issuer) + the
// container id + admin password (for kcadm). ok=false if it never comes up.
func (a *App) waitKeycloak(ctx context.Context, stackID int64, nodeID string, timeout time.Duration) (host string, ssl bool, containerID, adminPW string, ok bool) {
	if nodeID == "" {
		return "", false, "", "", false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := a.store.GetDeployment(stackID, nodeID)
		if err == nil {
			if dep.State == DeployError {
				return "", false, "", "", false
			}
			if dep.State == DeployRunning {
				var cfg keycloakConfig
				var sec keycloakSecrets
				json.Unmarshal(dep.Secrets, &sec)
				if json.Unmarshal(dep.Config, &cfg) == nil && cfg.Hostname != "" {
					return cfg.Hostname, cfg.SSL, dep.ContainerID, sec.AdminPassword, true
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", false, "", "", false
}
