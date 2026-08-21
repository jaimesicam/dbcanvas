package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Ubuntu VNC node (Type=="vnc"): a desktop "jump box" for troubleshooting. It runs the
// pre-baked desktop image (images/vnc.Dockerfile) — the systemd Ubuntu 24.04 base with
// the whole desktop already installed — so the stack runs as real systemd services that
// survive restarts, and deploying the node is configuration only.
//
// It provides an XFCE desktop over a browser-based VNC client (TigerVNC + noVNC/
// websockify), Firefox, the OpenSSH client, and percona-release with the Percona client
// tools (MySQL/PSMDB/Valkey/PostgreSQL) + percona-toolkit + ldap-utils. The MySQL client is
// 8.4, and percona-server-server is installed alongside it — masked, never started — purely
// because that package is the only one carrying the client-side auth plugins, including
// authentication_openid_connect_client.so, which the desktop needs to sign in to a
// Keycloak-backed Percona Server node (see images/vnc.Dockerfile). A sudo-enabled
// login user (credentials from the node properties) lets the operator install more
// packages for ad-hoc debugging. Per-stack singleton.
//
// Services: the packaged `tigervncserver@:1` unit (driven by /etc/tigervnc/
// vncserver.users + the user's ~/.vnc/config) runs Xvnc on display :1 (rfb 5901), and a
// small `dbcanvas-novnc` unit runs websockify serving noVNC on 6080 (published to the
// host).

const (
	vncWebPort   = 6080 // in-container noVNC (websockify) port, published to the host
	vncRFBPort   = 5901 // Xvnc display :1
	vncGeometry  = "1440x900"
	vncDefedUser = "dbadmin"
	// The one Ubuntu release the desktop image is built for. A second pre-baked
	// desktop per OS version would cost 2.4 GB to say the same thing, so — like the
	// Intranet, which is Oracle Linux 9 whatever the canvas says — the node pins its
	// release and the form no longer asks. Designs saved with 22.04 deploy on this.
	vncOS    = "ubuntu"
	vncOSVer = "24.04"
)

// vncImage is the pre-baked Ubuntu VNC image (images/vnc.Dockerfile): XFCE, TigerVNC +
// noVNC, Firefox and the Percona clients on the systemd Ubuntu 24.04 base. Built by
// `make images` / `make vnc-image`.
func vncImage(arch string) string {
	return "dbcanvas-vnc:" + vncOS + "-" + vncOSVer + "-" + archOr(arch)
}

// vncConfig is the non-secret profile shown for a deployed VNC node.
type vncConfig struct {
	Image     string `json:"image"`
	OS        string `json:"os"`
	OSVersion string `json:"osVersion"`
	Arch      string `json:"arch"`
	Hostname  string `json:"hostname"`
	FQDN      string `json:"fqdn"`
	WebPort   int    `json:"webPort"` // published host port → container noVNC 6080 (0 if unpublished)
	VNCUser   string `json:"vncUser"`
	UseProxy  bool   `json:"useProxy"`
}

// vncSecrets holds the desktop/VNC password (also the sudo user's password).
type vncSecrets struct {
	VNCPassword string `json:"vncPassword"`
}

// vncAuthPassword caps a password at the 8 bytes TigerVNC's VncAuth scheme uses.
// `vncpasswd -f` truncates silently, so we truncate here too and store the result:
// what the panel shows is then exactly what authenticates (the default
// VNC_PASSWORD "vnc_password" logs in as "vnc_pass").
func vncAuthPassword(pw string) string {
	if len(pw) > 8 {
		return pw[:8]
	}
	return pw
}

// provisionVNC records the deployment then runs an async goroutine that brings up the
// systemd container, installs the desktop + Firefox + clients, and starts the services.
func (a *App) provisionVNC(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	image := vncImage(n.Arch)

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
		pw = envOr("VNC_PASSWORD", "vnc_password")
	}
	pw = vncAuthPassword(pw)
	sec := vncSecrets{VNCPassword: pw}
	secJSON, _ := json.Marshal(sec)

	cfg := vncConfig{Image: image, OS: vncOS, OSVersion: vncOSVer, Arch: archOr(n.Arch), Hostname: host, FQDN: fqdn, VNCUser: user, UseProxy: n.UseProxy}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, image); !ok {
			pr.fail("image %s not found — run `make vnc-image` first", image)
			return
		}

		pr.phase("Waiting for Intranet to be ready", 8)
		_, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 14)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: vncWebPort}},
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
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

		// Record the published host port now so the manager link works even if a later
		// step fails.
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, fmt.Sprintf("%d/tcp", vncWebPort)); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.WebPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 18)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}

		if n.UseProxy {
			if err := a.runStep(ctx, id, pkgProxyDebian, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.logln("configure apt proxy skipped: " + err.Error())
			}
		}

		pr.phase("Trusting the Intranet CA", 40)
		// Trust the Intranet CA in the system store (CLI tools) + install it into Firefox
		// via enterprise policy (the desktop browser has its own root store), so the node
		// trusts stack TLS endpoints (Keycloak/PMM HTTPS, ...). The policy references the
		// CA file trustIntranetCA stages, so it has to run after it.
		// vncOS, not n.OS: the image is Ubuntu whatever the design says, and the trust
		// store path is chosen from the OS family (Debian's differs from RHEL's).
		a.trustIntranetCA(ctx, st, id, vncOS, pr.logln)
		if err := a.runStep(ctx, id, vncFirefoxCAScript, nil, pr.logln); err != nil {
			pr.logln("Firefox CA trust setup skipped: " + err.Error())
		}

		pr.phase("Creating desktop user", 60)
		if err := a.runStep(ctx, id, vncSetupUserScript, []string{"VNCUSER=" + user, "VNCPW=" + pw, "GEOMETRY=" + vncGeometry}, pr.logln); err != nil {
			pr.fail("create desktop user: %v", err)
			return
		}
		pr.logln("user " + user + " created (sudo) + VNC password set")

		pr.phase("Starting desktop services", 85)
		if err := a.runStep(ctx, id, vncStartServicesScript,
			[]string{"WEBPORT=" + strconv.Itoa(vncWebPort), "RFBPORT=" + strconv.Itoa(vncRFBPort)}, pr.logln); err != nil {
			pr.fail("start desktop services: %v", err)
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

// vncFirefoxCAScript makes Firefox trust the Intranet CA (e.g. for a Keycloak HTTPS
// issuer at https://keycloak.<domain>:8443).
//
// Firefox does NOT read the OS trust store, so `update-ca-certificates` (which
// trustIntranetCA runs, and which curl honours) is not enough on its own. The
// `ImportEnterpriseRoots` policy does not close the gap either: it is implemented
// for Windows and macOS only, so on Linux it is a no-op.
//
// `Certificates.Install` is the policy that works on Linux — Firefox reads the PEM at
// startup and trusts it for websites. (It is tracked separately from NSS trust flags,
// so the cert shows up in the profile's cert9.db with empty flags even though the
// browser trusts it; don't be fooled by `certutil -L`.) The path is the file
// trustIntranetCA stages, so this must run after it.
//
// Note: replacing NSS's libnssckbi.so with p11-kit's trust module — the usual
// "make Firefox use the system store" trick — does nothing here: current Firefox
// builds ship no libnssckbi.so at all.
const vncFirefoxCAScript = `set -e
CA=/usr/local/share/ca-certificates/dbcanvas-ca.crt
install -d /etc/firefox/policies
cat > /etc/firefox/policies/policies.json <<JSON
{ "policies": { "Certificates": { "ImportEnterpriseRoots": true, "Install": ["${CA}"] } } }
JSON
[ -f "$CA" ] || echo "WARN: ${CA} missing; Firefox will not trust the Intranet CA" >&2
echo "firefox policy installs Intranet CA from ${CA}"`

// vncSetupUserScript creates the sudo login user, sets its password + the VNC auth
// password (TigerVNC, 8-char), writes the per-user ~/.vnc/config (key=value: xfce
// session, geometry, no localhost-only, VncAuth) + xstartup, and maps display :1 to the
// user in /etc/tigervnc/vncserver.users for the packaged tigervncserver@ unit. Idempotent.
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
printf 'session=xfce\ngeometry=%s\nlocalhost=no\nsecuritytypes=VncAuth\n' "$GEOMETRY" > "$HOME_DIR/.vnc/config"
cat > "$HOME_DIR/.vnc/xstartup" <<'XS'
#!/bin/sh
unset SESSION_MANAGER
unset DBUS_SESSION_BUS_ADDRESS
exec dbus-launch --exit-with-session startxfce4
XS
chmod 755 "$HOME_DIR/.vnc/xstartup"
chown -R "$VNCUSER":"$VNCUSER" "$HOME_DIR/.vnc"
install -d /etc/tigervnc
echo ":1=$VNCUSER" > /etc/tigervnc/vncserver.users`

// vncStartServicesScript writes the websockify unit and enables both systemd services
// (the packaged tigervncserver@:1 for Xvnc, dbcanvas-novnc for the noVNC web bridge),
// then verifies the rfb + web ports are listening.
const vncStartServicesScript = `set -e
cat > /etc/systemd/system/dbcanvas-novnc.service <<UNIT
[Unit]
Description=DBCanvas noVNC (websockify)
After=tigervncserver@:1.service network-online.target
[Service]
ExecStart=/usr/bin/websockify --web=/usr/share/novnc $WEBPORT localhost:$RFBPORT
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl reset-failed "tigervncserver@:1" dbcanvas-novnc 2>/dev/null || true
systemctl enable --now "tigervncserver@:1"
systemctl enable --now dbcanvas-novnc
for port in $RFBPORT $WEBPORT; do
  OK=0
  for i in $(seq 1 20); do
    (exec 3<>/dev/tcp/127.0.0.1/$port) 2>/dev/null && { exec 3>&-; OK=1; break; }
    sleep 1
  done
  [ "$OK" = 1 ] || { echo "port $port not listening after start"; systemctl --no-pager status "tigervncserver@:1" dbcanvas-novnc 2>&1 | tail -20; exit 1; }
done`
