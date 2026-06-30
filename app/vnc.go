package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Ubuntu VNC node (Type=="vnc"): a desktop "jump box" for troubleshooting. It runs
// the stock ubuntu:24.04 image (pulled at deploy) with an XFCE desktop served over a
// web-based VNC client (TigerVNC + noVNC/websockify), plus percona-release and the
// Percona client tools (MySQL/PSMDB/Valkey/PostgreSQL) and ldap-utils preinstalled.
// A sudo-enabled login user (credentials from the node properties) lets the operator
// install more packages for ad-hoc debugging.
//
// There is no systemd in the base image: the container runs `sleep infinity` as PID 1
// and the desktop stack is installed + launched via exec steps. The launch step is
// idempotent so a redeploy brings the session back up.

const (
	vncImage     = "ubuntu:24.04"
	vncImageRepo = "ubuntu"
	vncImageTag  = "24.04"
	vncWebPort   = 6080 // in-container noVNC (websockify) port, published to the host
	vncRFBPort   = 5901 // Xvnc display :1
	vncGeometry  = "1440x900"
	vncDefedUser = "dbadmin"
)

// vncConfig is the non-secret profile shown for a deployed VNC node.
type vncConfig struct {
	Image    string `json:"image"`
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
	WebPort  int    `json:"webPort"` // published host port → container noVNC 6080 (0 if unpublished)
	VNCUser  string `json:"vncUser"`
	UseProxy bool   `json:"useProxy"`
}

// vncSecrets holds the desktop/VNC password (also the sudo user's password).
type vncSecrets struct {
	VNCPassword string `json:"vncPassword"`
}

// genVNCPassword returns an 8-character password (TigerVNC truncates VNC auth to 8).
func genVNCPassword() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b) // 8 lowercase hex chars
}

// provisionVNC records the deployment then runs an async goroutine that pulls
// ubuntu:24.04, installs the desktop + VNC + Percona clients, and starts the session.
func (a *App) provisionVNC(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	user := strings.TrimSpace(n.VNCUser)
	if user == "" {
		user = vncDefedUser
	}
	// Reuse the password across redeploys.
	pw := strings.TrimSpace(n.VNCPassword)
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s vncSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil && s.VNCPassword != "" {
			pw = s.VNCPassword
		}
	}
	if pw == "" {
		pw = genVNCPassword()
	}
	sec := vncSecrets{VNCPassword: pw}
	secJSON, _ := json.Marshal(sec)

	cfg := vncConfig{Image: vncImage, Hostname: host, FQDN: fqdn, VNCUser: user, UseProxy: n.UseProxy}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	go func() {
		ctx := context.Background()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		pr.phase("Pulling image", 6)
		if ok, _ := a.docker.ImageExists(ctx, vncImage); !ok {
			pr.logln("pulling " + vncImage)
			if err := a.docker.ImagePull(ctx, vncImageRepo, vncImageTag); err != nil {
				pr.fail("pull image: %v", err)
				return
			}
		}

		pr.phase("Waiting for Intranet to be ready", 12)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, 10*time.Minute)
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 18)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: vncImage, Hostname: host,
			Cmd:     []string{"sleep", "infinity"},
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: vncWebPort}},
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
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

		// Record the published host port now so the manager link works even if a later
		// step fails.
		if hp, e := a.docker.ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", vncWebPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.WebPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		if n.UseProxy {
			if err := a.runStep(ctx, id, pkgProxyDebian, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.logln("configure apt proxy skipped: " + err.Error())
			}
		}

		pr.phase("Installing desktop + VNC", 35)
		if err := a.runStep(ctx, id, vncInstallDesktopScript, nil, pr.logln); err != nil {
			pr.fail("install desktop/VNC: %v", err)
			return
		}
		pr.logln("XFCE desktop + TigerVNC + noVNC installed")

		pr.phase("Installing Percona clients", 60)
		if err := a.runStep(ctx, id, vncInstallClientsScript, nil, pr.logln); err != nil {
			// Best-effort: the desktop is still usable and the operator has sudo.
			pr.logln("Percona client install had issues (continuing; install manually with sudo): " + err.Error())
		} else {
			pr.logln("percona-release + clients installed (ps/psmdb/valkey/ppg, ldap-utils)")
		}

		pr.phase("Creating desktop user", 80)
		if err := a.runStep(ctx, id, vncSetupUserScript, []string{"VNCUSER=" + user, "VNCPW=" + pw}, pr.logln); err != nil {
			pr.fail("create desktop user: %v", err)
			return
		}
		pr.logln("user " + user + " created (sudo) + VNC password set")

		pr.phase("Starting desktop session", 92)
		if err := a.runStep(ctx, id, vncStartScript,
			[]string{"VNCUSER=" + user, "GEOMETRY=" + vncGeometry, "WEBPORT=" + strconv.Itoa(vncWebPort), "RFBPORT=" + strconv.Itoa(vncRFBPort)}, pr.logln); err != nil {
			pr.fail("start desktop session: %v", err)
			return
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d vnc %s: provisioned (noVNC host port %d)", st.ID, n.Label, cfg.WebPort)
	}()
}

// vncInstallDesktopScript installs the XFCE desktop, TigerVNC and noVNC/websockify.
const vncInstallDesktopScript = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  xfce4 xfce4-goodies xfce4-terminal dbus-x11 xterm \
  tigervnc-standalone-server tigervnc-common tigervnc-tools \
  novnc websockify python3 \
  wget gnupg2 lsb-release curl ca-certificates sudo net-tools nano vim less procps >/dev/null
# noVNC ships vnc.html under /usr/share/novnc; ensure an index points at it.
[ -f /usr/share/novnc/index.html ] || ln -sf /usr/share/novnc/vnc.html /usr/share/novnc/index.html 2>/dev/null || true`

// vncInstallClientsScript installs percona-release and the Percona client tools plus
// ldap-utils. Each client install is best-effort (|| true) so one bad package name in
// a future repo refresh never blocks the others — the operator has sudo to fix it.
const vncInstallClientsScript = `set -e
export DEBIAN_FRONTEND=noninteractive
wget -qO /tmp/percona-release.deb https://repo.percona.com/apt/percona-release_latest.generic_all.deb
dpkg -i /tmp/percona-release.deb >/dev/null 2>&1 || { apt-get update -qq; apt-get -y -qq -f install >/dev/null; }
for r in ps-80 psmdb-80 ppg-17 valkey-91 tools; do percona-release enable "$r" >/dev/null 2>&1 || true; done
apt-get update -qq
apt-get install -y -qq ldap-utils >/dev/null 2>&1 || true
apt-get install -y -qq percona-server-client >/dev/null 2>&1 || true       # Percona Server (MySQL) client
apt-get install -y -qq percona-mongodb-mongosh >/dev/null 2>&1 || true      # PSMDB shell (mongosh)
apt-get install -y -qq percona-postgresql-client-17 >/dev/null 2>&1 || true # Percona PostgreSQL client (psql)
apt-get install -y -qq percona-valkey-tools >/dev/null 2>&1 || apt-get install -y -qq valkey-tools >/dev/null 2>&1 || true  # Valkey client (valkey-cli)
apt-get install -y -qq percona-toolkit >/dev/null 2>&1 || true             # Percona Toolkit (pt-* utilities)
# Report what landed so the deploy log shows which clients are present.
echo "clients present:"
for c in mysql mongosh psql valkey-cli ldapsearch pt-query-digest; do command -v "$c" >/dev/null 2>&1 && echo "  $c: $(command -v $c)" || echo "  $c: MISSING (install with sudo)"; done`

// vncSetupUserScript creates the sudo login user, sets its password + the VNC auth
// password (TigerVNC, 8-char), and writes the XFCE xstartup. Idempotent.
const vncSetupUserScript = `set -e
id "$VNCUSER" >/dev/null 2>&1 || useradd -m -s /bin/bash "$VNCUSER"
echo "$VNCUSER:$VNCPW" | chpasswd
usermod -aG sudo "$VNCUSER"
echo "$VNCUSER ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-dbcanvas-vnc
chmod 0440 /etc/sudoers.d/90-dbcanvas-vnc
HOME_DIR=$(getent passwd "$VNCUSER" | cut -d: -f6)
install -d -o "$VNCUSER" -g "$VNCUSER" -m 700 "$HOME_DIR/.vnc"
VNCPASSWD=$(command -v tigervncpasswd || command -v vncpasswd)
printf '%s\n' "$VNCPW" | "$VNCPASSWD" -f > "$HOME_DIR/.vnc/passwd"
chmod 600 "$HOME_DIR/.vnc/passwd"
cat > "$HOME_DIR/.vnc/xstartup" <<'XS'
#!/bin/sh
unset SESSION_MANAGER
unset DBUS_SESSION_BUS_ADDRESS
exec dbus-launch --exit-with-session startxfce4
XS
chmod 755 "$HOME_DIR/.vnc/xstartup"
chown -R "$VNCUSER":"$VNCUSER" "$HOME_DIR/.vnc"`

// vncStartScript (re)launches Xvnc (display :1) as the user and websockify/noVNC on
// the web port. Idempotent — kills any existing session first. Also persists a small
// start script so the session can be relaunched after a container restart.
const vncStartScript = `set -e
HOME_DIR=$(getent passwd "$VNCUSER" | cut -d: -f6)
VNCSRV=$(command -v tigervncserver || command -v vncserver)
cat > /usr/local/bin/dbcanvas-vnc-start.sh <<SH
#!/bin/bash
set -e
VNCSRV="$VNCSRV"
runuser -l "$VNCUSER" -c "\$VNCSRV -kill :1 >/dev/null 2>&1 || true"
# Stop a previous noVNC by its recorded PID (do NOT pkill -f websockify: the deploy
# step's own command line contains that word and would get killed too).
[ -f /run/dbcanvas-novnc.pid ] && kill "\$(cat /run/dbcanvas-novnc.pid)" 2>/dev/null || true
sleep 1
runuser -l "$VNCUSER" -c "\$VNCSRV :1 -geometry $GEOMETRY -depth 24 -localhost no -SecurityTypes VncAuth -rfbport $RFBPORT"
nohup websockify --web=/usr/share/novnc $WEBPORT localhost:$RFBPORT >/var/log/websockify.log 2>&1 &
echo \$! > /run/dbcanvas-novnc.pid
SH
chmod 755 /usr/local/bin/dbcanvas-vnc-start.sh
/usr/local/bin/dbcanvas-vnc-start.sh
# Verify Xvnc (rfb $RFBPORT) and the noVNC web port are listening. (Checking the
# listening ports is reliable; tigervncserver -list formats the display without a colon.)
for port in $RFBPORT $WEBPORT; do
  OK=0
  for i in $(seq 1 15); do
    (exec 3<>/dev/tcp/127.0.0.1/$port) 2>/dev/null && { exec 3>&-; OK=1; break; }
    sleep 1
  done
  [ "$OK" = 1 ] || { echo "port $port not listening after start"; tail -20 "$HOME_DIR/.vnc/"*.log /var/log/websockify.log 2>/dev/null; exit 1; }
done`
