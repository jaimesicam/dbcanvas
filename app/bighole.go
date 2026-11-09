package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// bighole.go — Big Hole, a MongoDB FTDC viewer, as an install option on the nodes that have FTDC
// to look at: a standalone MongoDB node, every mongod of a replica set or sharded cluster (not
// mongos, which writes no diagnostic.data of its own), and the Linux Client.
//
// Big Hole (github.com/zelmario/Big-hole, MIT) runs entirely in the browser: you drag a
// mongod's `diagnostic.data` folder, or a whole support tarball, onto the page and it decodes
// and charts it. Nothing is uploaded and there is no backend, so "installing" it on a node is
// serving its static files: nginx from the OS packages, on bigHoleNodePort, published to the
// host. The files are built once, at a pinned upstream commit, in DBCanvas's own image
// (app/Dockerfile, stage "bighole") and copied onto the node at deploy — no Node toolchain on
// the node and no image of its own to build. It used to be a node type with its own image;
// IMPLEMENTATION.md 525.
//
// DBCanvas has its own FTDC Summary page, which reads the same files and answers a different
// question: it distils one capture into verdicts and findings, where Big Hole charts every
// metric it can find and lets you go looking. See docs/FTDC_SUMMARY.md.
//
// THE ONE TRAP, and it is upstream's own warning: browsers grant private on-disk storage
// (OPFS) and workers only in a *secure context*, which means HTTPS or localhost. Opened as
// http://<some-host>:<port> the app cannot keep a capture and fails during ingest, which reads
// like a broken decoder rather than a deployment mistake. So the panel links to
// http://localhost:<port> and, when DBCanvas itself is not browsed on localhost, hands over the
// ssh -L line that puts it there.
//
// Like the other tools, a failed install is logged and never fails the deploy.

const (
	// bigHoleRef is the upstream commit the files are built from — the project publishes no
	// tags and its package.json says 0.0.0, so the revision is its version. app/Dockerfile's
	// BIGHOLE_REF must be the same; a test checks.
	bigHoleRef = "896984fe9e9a7f1f59cc4ce29237625f8aaa713f"
	// bigHoleNodePort is the port nginx serves Big Hole on inside a node. Not 80: a node
	// may run something of its own there.
	bigHoleNodePort = 8070
	// bigHoleTarballDefault is where the image keeps the built files (app/Dockerfile).
	bigHoleTarballDefault = "/usr/local/share/dbcanvas/bighole/bighole.tar.gz"
)

// bigHoleTarball is the built app's path: BIGHOLE_TARBALL, else the image's.
func bigHoleTarball() string { return envOr("BIGHOLE_TARBALL", bigHoleTarballDefault) }

// bigHoleOn reports whether a MongoDB member serves Big Hole: the option is on, and the member
// runs a mongod (mongos keeps no diagnostic.data).
func bigHoleOn(on bool, role string) bool { return on && role != "mongos" }

// bigHolePortMap is the published port for a node, keeping the host port a previous deploy
// pinned so a bookmarked URL — and an ssh -L forwarding it — keeps working.
func bigHolePortMap(prevHostPort int) PortMap {
	return PortMap{ContainerPort: bigHoleNodePort, HostPort: prevHostPort, Auto: true}
}

// bigHolePrevPort reads the host port a node's last deployment published Big Hole on.
func (a *App) bigHolePrevPort(stackID int64, nodeID string) int {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil || len(dep.Config) == 0 {
		return 0
	}
	var c struct {
		BigHolePort int `json:"bigHolePort"`
	}
	json.Unmarshal(dep.Config, &c)
	return c.BigHolePort
}

// installBigHole copies the built app onto a node, serves it with nginx on bigHoleNodePort and
// returns the host port it is published on (0 when the install did not happen). Every failure
// is logged and returned as 0, never as a failed deploy.
func (a *App) installBigHole(ctx context.Context, id, nodeOS string, logln func(string)) int {
	data, err := os.ReadFile(bigHoleTarball())
	if err != nil {
		logln("Big Hole not installed: this DBCanvas build has no Big Hole files (" + err.Error() + ")")
		return 0
	}
	if err := a.engCtx(ctx).CopyFile(ctx, id, "/tmp", "bighole.tar.gz", 0o644, data); err != nil {
		logln("Big Hole not installed: copy the files: " + err.Error())
		return 0
	}
	script := bigHoleInstallRHEL
	if isDebianOS(nodeOS) {
		script = bigHoleInstallDebian
	}
	if err := a.runStep(ctx, id, script+bigHoleServeScript, []string{"PORT=" + strconv.Itoa(bigHoleNodePort)}, logln); err != nil {
		logln("Big Hole not installed: " + err.Error())
		return 0
	}
	hp, err := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", bigHoleNodePort))
	if err != nil {
		logln("Big Hole installed, but its port is not published: " + err.Error())
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(hp))
	logln(fmt.Sprintf("Big Hole (%s) serving on port %d — open http://localhost:%d", bigHoleRef[:7], bigHoleNodePort, p))
	return p
}

// bigHoleInstallRHEL / Debian install nginx; bigHoleServeScript unpacks the app and serves it.
const bigHoleInstallRHEL = `set -e
command -v nginx >/dev/null 2>&1 || dnf -y -q install nginx >/dev/null
`

const bigHoleInstallDebian = `set -e
export DEBIAN_FRONTEND=noninteractive
command -v nginx >/dev/null 2>&1 || { apt-get update -qq >/dev/null; apt-get install -y -qq nginx >/dev/null; }
# The packaged default site listens on 80; Big Hole is on its own port, and nothing here needs it.
rm -f /etc/nginx/sites-enabled/default
`

// bigHoleServeScript is upstream's nginx.conf (docker/nginx.conf) on PORT, with the files from
// /tmp/bighole.tar.gz in /usr/share/big-hole.
const bigHoleServeScript = `rm -rf /usr/share/big-hole && mkdir -p /usr/share/big-hole
tar -xzf /tmp/bighole.tar.gz -C /usr/share/big-hole && rm -f /tmp/bighole.tar.gz
mkdir -p /etc/nginx/conf.d
cat > /etc/nginx/conf.d/big-hole.conf <<CONF
server {
    listen $PORT;
    server_name _;
    root /usr/share/big-hole;
    index index.html;
    gzip on;
    gzip_types application/javascript text/css application/json image/svg+xml;
    gzip_min_length 1024;
    location /assets/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }
    location / {
        try_files \$uri \$uri/ /index.html;
        add_header Cache-Control "no-store";
    }
}
CONF
nginx -t >/dev/null 2>&1 || { nginx -t; exit 1; }
systemctl enable nginx >/dev/null 2>&1 || true
systemctl restart nginx
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" || { echo "nginx is not answering on $PORT"; exit 1; }`

// setDeploymentConfigField sets one key of a deployment's stored config, keeping the rest.
func (a *App) setDeploymentConfigField(stackID int64, nodeID, key string, value any) {
	dep, err := a.store.GetDeployment(stackID, nodeID)
	if err != nil {
		return
	}
	m := map[string]any{}
	if len(dep.Config) > 0 {
		json.Unmarshal(dep.Config, &m)
	}
	m[key] = value
	b, _ := json.Marshal(m)
	dep.Config = b
	a.store.UpsertDeployment(dep)
}

// bigHoleIssues checks a node's or cluster's Big Hole option: the Linux Client on CentOS 7 has
// no systemd to run nginx.
func bigHoleIssues(what, nodeType, os string, on bool) []issue {
	if on && nodeType == "linuxclient" && isEL7OS(os) {
		return []issue{{Level: "error", Message: what + ": Big Hole is served by nginx as a service, and CentOS 7 here has no systemd — choose another OS release or turn Big Hole off"}}
	}
	return nil
}
