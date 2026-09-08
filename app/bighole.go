package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
)

// Big Hole node (Type=="bighole") — a third-party MongoDB FTDC viewer
// (github.com/zelmario/Big-hole, MIT) that runs entirely in the browser: you drag a
// mongod's `diagnostic.data` folder, or a whole support tarball, onto the page and
// it decodes and charts it. Nothing is uploaded and there is no backend; the
// container is the built app behind nginx and nothing else.
//
// It is the loosest-coupled node this app has, and deliberately so. It takes no
// association line, no credentials and no configuration, because it talks to
// nothing — not to a database, not to an API, not even to DBCanvas. What it gives a
// stack is the tool, at a URL, without anybody installing Node.
//
// DBCanvas has its own FTDC Summary page, which reads the same files and answers a
// different question: it distils one capture into verdicts and findings, where this
// charts every metric it can find and lets you go looking. Having both is the point
// — see docs/FTDC_SUMMARY.md.
//
// THE ONE TRAP, and it is upstream's own warning: browsers grant private on-disk
// storage (OPFS) and workers only in a *secure context*, which means HTTPS or
// localhost. Opened as http://<some-host>:<port> the app cannot keep a capture and
// fails during ingest, which reads like a broken decoder rather than a deployment
// mistake. So the node's panel links to http://localhost:<port> and, when DBCanvas
// itself is not being browsed on localhost, hands over the ssh -L line that puts it
// there. Nothing in here can enforce that, which is exactly why it is said out loud
// in the UI rather than only in this comment.

const (
	// bigHoleRef is the upstream commit this node runs, and bigHoleImage's tag is
	// its short form. A commit rather than a version because the project has
	// neither tags nor a package.json version — the revision is the only honest
	// answer to "which Big Hole is this". images/service.sh must build that tag
	// from this ref; the two are checked against each other by test.
	bigHoleRef   = "896984fe9e9a7f1f59cc4ce29237625f8aaa713f"
	bigHoleImage = "dbcanvas-bighole:896984f"
	// The port nginx listens on inside the container (images/bighole.Dockerfile).
	bigHolePort = 80
)

// bigHoleConfig is the non-secret profile shown for a deployed Big Hole node. There
// are no secrets at all — hence no bigHoleSecrets to go with it.
type bigHoleConfig struct {
	Image    string `json:"image"`
	Ref      string `json:"ref"` // the upstream commit the image was built from
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	HTTPPort int    `json:"httpPort"` // host port mapped to nginx's 80
}

// provisionBigHole records the deployment then starts the viewer.
func (a *App) provisionBigHole(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the published host port across a redeploy, so a bookmarked URL — and
	// any ssh -L already forwarding it — keeps working.
	httpPort := 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Config) > 0 {
		var old bigHoleConfig
		if json.Unmarshal(dep.Config, &old) == nil {
			httpPort = old.HTTPPort
		}
	}
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}

	cfg := bigHoleConfig{Image: bigHoleImage, Ref: bigHoleRef, Hostname: host, FQDN: fqdn, HTTPPort: httpPort}
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: mustJSON(cfg)})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, bigHoleImage); !ok {
			pr.fail("image %s not found — run `make bighole-image` first", bigHoleImage)
			return
		}

		// The app resolves nothing, so this wait buys it no capability of its own.
		// It is here for the other direction: the node joins the stack's DNS like
		// every other node, so it can be reached by name from the VNC desktop, and
		// reconcileStackDNS needs the Intranet up to record it.
		pr.phase("Waiting for Intranet to be ready", 25)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 60)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: bigHoleImage, Hostname: host,
			Network:    networkName(st.ID),
			Aliases:    []string{host},
			PublishMap: []PortMap{{ContainerPort: bigHolePort, HostPort: httpPort}},
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
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", bigHolePort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}

		a.store.UpsertDeployment(Deployment{
			StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: mustJSON(cfg),
		})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d bighole %s: provisioned (viewer on host port %d — open it as localhost)", st.ID, n.Label, cfg.HTTPPort)
	}()
}
