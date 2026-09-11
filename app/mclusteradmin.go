package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
)

// MClusterAdmin node (Type=="mclusteradmin") — a third-party MongoDB
// administration panel (github.com/PrzemekMalkowski/mclusteradmin, MIT) that a
// stack can run beside its MongoDB: topology and replica-set status, sharding and
// the balancer, current ops, slow queries with explain, index and profile
// management, users and roles, oplog stats.
//
// Runs upstream's own published image, pulled at deploy the way PMM's and
// Watchtower's are rather than built here, and its web UI port is published to the
// host like PMM's and the simulators' dashboards, so it is reached straight from the
// host browser with no VNC desktop in the way.
//
// Three things about this node are unlike the simulators it otherwise resembles:
//
//   - It draws NO connectors and takes no association line, because there is
//     nothing for one to carry. The panel is configured entirely through its own
//     UI — it reads no environment and no config file (see its main.go: flags
//     only) — so the connection URI is typed into it, by hand, by whoever opens
//     it. DBCanvas cannot put it there, and an association line that only hinted
//     at which database you probably meant was a line that wired nothing: every
//     other line on this canvas is walked by a provisioner.
//
//   - It authenticates to MongoDB as an ordinary user, and that half IS wired: a
//     MongoDB node or cluster creates the two accounts on request (the
//     MCACredentials design field → mongoEnsureMCAUsers in mongodb.go), with the
//     passwords this node carries. So the node's panel shows the credentials, and
//     the host is the one thing left to type.
//
//   - Its image is amd64 and only amd64, because that is the one upstream builds.
//     So this node is pinned to linux/amd64 whatever platform the installation
//     targets — the same treatment PMM and Watchtower get, and the same
//     consequence: on arm64 it needs Rosetta or qemu to run at all.
//
// No TLS option, deliberately. The panel can serve HTTPS (--tls) with a cert we
// could sign from the Intranet CA — but the way this node is actually reached is
// the published host port, and the host browser does not trust that CA, so the
// only thing HTTPS would reliably add here is a click-through warning. The Intranet
// CA is for what happens inside the stack network; see keycloak.go, which serves
// HTTPS for exactly that reason and publishes no host port at all.

const (
	// mcaVersion is the upstream release this node runs, and it is the tag of
	// upstream's published image — the one place the version is written, because the
	// image is `scratch` with one static binary in it and there is no shell for
	// nodeversion.go to ask. Bumping the panel is this line and nothing else.
	//
	// Pinned to a version tag rather than :latest for the reason every other image
	// here is: a stack redeployed next month should be the stack you deployed. As of
	// 0.3.7 the two are the same digest anyway.
	mcaVersion   = "0.3.7"
	mcaImageRepo = "ghcr.io/przemekmalkowski/mclusteradmin"
	mcaImage     = mcaImageRepo + ":" + mcaVersion
	mcaPort      = 8787
)

// mcaConfig is the non-secret profile shown for a deployed MClusterAdmin node.
type mcaConfig struct {
	Image    string `json:"image"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	HTTPPort int    `json:"httpPort"` // host port mapped to the panel's 8787
	ViewOnly bool   `json:"viewOnly"` // started with --view-only (every write disabled)
}

// mcaSecrets holds the panel's two accounts. They carry passwords, so they live in
// Secrets rather than Config — the node's panel reveals them on request, the way
// every other credential in this app is treated.
//
// Recorded whether or not any MongoDB in the stack was asked to create them: they
// are what somebody types into the panel, and the node that owns the passwords is
// the one place to look them up.
type mcaSecrets struct {
	AdminUser        string `json:"adminUser"`
	AdminPassword    string `json:"adminPassword"`
	ReadOnlyUser     string `json:"readonlyUser"`
	ReadOnlyPassword string `json:"readonlyPassword"`
}

// mcaPasswords resolves this panel node's two account passwords, falling back to the
// documented defaults. Same fallback mcaPasswordsFor uses on the database side, so
// the two ends agree even when the fields are left empty.
func mcaPasswords(n designNode) (admin, readOnly string) {
	admin, readOnly = mcaDefaultPasswords()
	if n.MCAAdminPassword != "" {
		admin = n.MCAAdminPassword
	}
	if n.MCAReadOnlyPassword != "" {
		readOnly = n.MCAReadOnlyPassword
	}
	return admin, readOnly
}

// provisionMClusterAdmin records the deployment then starts the panel. Nothing is
// waited for beyond the Intranet: the panel has no database to be ready, because
// which one it opens is a URI somebody types into it.
func (a *App) provisionMClusterAdmin(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	hosts := stackHostnames(doc)
	host := hosts[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the published host port across a redeploy so the URL somebody
	// bookmarked keeps working — the same reason PMM and the simulators do.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old mcaConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	cfg := mcaConfig{
		Image: mcaImage, Version: mcaVersion, Hostname: host, FQDN: fqdn,
		HTTPPort: httpPort, ViewOnly: n.ViewOnly,
	}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		// Pinned to amd64, like PMM's and Watchtower's images and for the same
		// reason: upstream publishes one architecture, not a manifest list. Asking
		// for the platform this installation targets would fail the pull outright on
		// an arm64 one, where this at least runs under Rosetta/qemu — and where it
		// cannot, says so honestly rather than mis-resolving the manifest.
		pr.phase("Pulling image", 10)
		pr.logln("ensuring " + mcaImage + " for " + platformAMD64)
		if err := a.engCtx(ctx).EnsureImage(ctx, mcaImageRepo, mcaVersion, platformAMD64); err != nil {
			pr.fail("pull image: %v", err)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 25)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		// The accounts a MongoDB in this stack creates for the panel when asked
		// (MCACredentials). Recorded here whether or not any did: this node owns the
		// passwords, so this is where somebody reads them.
		adminPW, roPW := mcaPasswords(n)
		sec := mcaSecrets{
			AdminUser: mcaAdminUser, AdminPassword: adminPW,
			ReadOnlyUser: mcaReadOnlyUser, ReadOnlyPassword: roPW,
		}

		pr.phase("Creating container", 70)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		cmd := []string{"--tcp_port", strconv.Itoa(mcaPort)}
		if n.ViewOnly {
			cmd = append(cmd, "--view-only")
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: mcaImage, Hostname: host, Platform: platformAMD64,
			Cmd:        cmd,
			Network:    networkName(st.ID),
			Aliases:    []string{host},
			PublishMap: []PortMap{{ContainerPort: mcaPort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", mcaPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}

		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning,
			Config: mustJSON(cfg), Secrets: mustJSON(sec),
		})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d mclusteradmin %s: provisioned (panel on host port %d)", st.ID, n.Label, cfg.HTTPPort)
	}()
}
