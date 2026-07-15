package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pmmServerURL builds the URL for `pmm-admin config --server-url`, percent-encoding
// the credentials via net/url. The admin password can contain characters (e.g. '^')
// that are illegal in raw URL userinfo — embedding them unencoded makes pmm-admin
// fail with "invalid userinfo", so the whole PMM registration aborts.
func pmmServerURL(fqdn, user, pass string) string {
	u := url.URL{Scheme: "https", User: url.UserPassword(user, pass), Host: fqdn + ":8443"}
	return u.String()
}

// PMM (Percona Monitoring and Management) v3 node. Unlike the Intranet node
// (built from a systemd OS image by `make images`), PMM ships as the
// percona/pmm-server container, which runs Grafana, VictoriaMetrics, ClickHouse,
// PostgreSQL, QAN and an nginx TLS front-end under supervisord. We pull the
// selected image, publish its HTTP (8080) and HTTPS (8443) ports, set the admin
// password, point Grafana's SMTP at the Intranet mail server, and optionally
// re-issue the nginx certificate from the Intranet CA.

// pmmConfig is the non-secret profile shown for a deployed PMM node.
type pmmConfig struct {
	Image        string   `json:"image"`
	Version      string   `json:"version"`
	Arch         string   `json:"arch"`
	Hostname     string   `json:"hostname"` // unique DNS hostname on the stack
	FQDN         string   `json:"fqdn"`     // hostname.<domain>
	Alias        string   `json:"alias"`    // network alias (== hostname)
	AdminUser    string   `json:"adminUser"`
	HTTPPort     int      `json:"httpPort"`     // host port mapped to container 8080
	HTTPSPort    int      `json:"httpsPort"`    // host port mapped to container 8443
	SMTPHost     string   `json:"smtpHost"`     // Grafana SMTP target (Intranet)
	GenerateCert bool     `json:"generateCert"` // certs signed by the Intranet CA
	Services     []string `json:"services"`
}

// pmmSecrets holds the PMM admin credential (generated or user-supplied).
type pmmSecrets struct {
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`
}

// pmmImage resolves the image reference for a PMM node from its selected
// version, falling back to the catalog's rolling default tag.
func pmmImage(cat PMMCatalog, version string) (repo, tag, ref string) {
	repo = cat.Repository
	if repo == "" {
		repo = "percona/pmm-server"
	}
	tag = version
	if tag == "" {
		tag = cat.DefaultTag
	}
	if tag == "" {
		tag = "3"
	}
	return repo, tag, repo + ":" + tag
}

// pmmAlias is the network alias / hostname for a PMM node.
func pmmAlias(label string) string {
	a := sanitizeName(strings.TrimSpace(label))
	if a == "" {
		a = "pmm"
	}
	return a
}

// provisionPMM records the deployment and starts an async provisioning goroutine
// for a PMM node. doc is the full design, used to locate the Intranet node for
// the SMTP target and (when enabled) certificate signing.
// pmmDataVolume is the stable name of a PMM node's /srv data volume (persists across
// upgrades; removed when the stack is destroyed).
func pmmDataVolume(stackID int64, nodeID string) string {
	return fmt.Sprintf("dbcanvas-pmm-%d-%s", stackID, nodeID)
}

func (a *App) provisionPMM(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	cat := loadPMMCatalog()
	repo, tag, ref := pmmImage(cat, n.Version)
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = pmmAlias(n.Label)
	}
	fqdn := fqdnOf(host, domain)

	// Reuse the admin password across redeploys; otherwise take the user's value
	// or generate one when left empty. Also reuse the published host ports so the PMM
	// server keeps the same URLs across a redeploy AND across a Watchtower upgrade.
	var sec pmmSecrets
	httpPort, httpsPort := 0, 0
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil {
		if len(dep.Secrets) > 0 {
			json.Unmarshal(dep.Secrets, &sec)
		}
		if len(dep.Config) > 0 {
			var old pmmConfig
			if json.Unmarshal(dep.Config, &old) == nil {
				httpPort, httpsPort = old.HTTPPort, old.HTTPSPort
			}
		}
	}
	if sec.AdminPassword == "" {
		pw := strings.TrimSpace(n.AdminPassword)
		if pw == "" {
			pw = envOr("PMM_ADMIN_PASSWORD", "admin_password")
		}
		sec = pmmSecrets{AdminUser: "admin", AdminPassword: pw}
	}
	// Pin fixed host ports (allocate free ones on first deploy). Publishing explicit
	// HostPorts — rather than Docker's ephemeral empty binding — is what makes them
	// survive Watchtower recreating the PMM container during an in-GUI upgrade.
	if httpPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpPort = p
		}
	}
	if httpsPort == 0 {
		if p, e := freeHostPort(); e == nil {
			httpsPort = p
		}
	}

	cfg := pmmConfig{
		// PMM (percona/pmm-server) has no arm64 image yet — always amd64.
		Image: ref, Version: tag, Arch: "amd64",
		Hostname: host, FQDN: fqdn, Alias: host,
		AdminUser: sec.AdminUser, SMTPHost: "intranet." + domain + ":25",
		GenerateCert: n.GenerateCert, HTTPPort: httpPort, HTTPSPort: httpsPort,
		Services: []string{"Grafana", "VictoriaMetrics", "ClickHouse", "PostgreSQL", "QAN", "nginx (TLS)"},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		prog := &provProgress{Percent: 0, Phase: "Starting", Log: []string{}}
		save := func() { b, _ := json.Marshal(prog); a.store.SetDeploymentProgress(st.ID, n.ID, b) }
		logln := func(s string) {
			prog.Log = append(prog.Log, s)
			if len(prog.Log) > 200 {
				prog.Log = prog.Log[len(prog.Log)-200:]
			}
			save()
		}
		setPhase := func(p string, pct int) { prog.Phase = p; prog.Percent = pct; save() }
		failNode := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			log.Printf("stack %d pmm %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		setPhase("Pulling image", 5)
		logln("ensuring " + ref + " for " + platformAMD64 + " (this can take a while)")
		if err := a.engCtx(ctx).EnsureImage(ctx, repo, tag, platformAMD64); err != nil {
			failNode("pull image %s: %v", ref, err)
			return
		}
		logln("image ready: " + ref)

		// The Intranet is the stack's DNS resolver (and SMTP relay / LDAP / CA), so
		// the PMM node must not start until the Intranet is fully up and running.
		setPhase("Waiting for Intranet to be ready", 15)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			failNode("%v", werr)
			return
		}
		logln("Intranet is running (resolver at " + intranetIP + ")")

		// If a Watchtower node is associated, point PMM at it so the UI can trigger
		// server upgrades (PMM_WATCHTOWER_HOST/TOKEN). Best-effort: if the Watchtower
		// node never comes up we still bring PMM online without the integration.
		var wtEnv []string
		if n.WatchtowerNodeID != "" {
			setPhase("Waiting for Watchtower", 18)
			if wfqdn, wtoken, ok := a.waitWatchtower(ctx, st.ID, n.WatchtowerNodeID, deployTimeout()); ok {
				wtEnv = watchtowerHostEnv(wfqdn, wtoken)
				logln("associated with Watchtower at " + wfqdn)
			} else {
				logln("warning: Watchtower node not ready; PMM upgrades via Watchtower disabled")
			}
		}

		setPhase("Creating container", 20)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		// Persist /srv (Grafana DB, VictoriaMetrics, ClickHouse, etc.) in a stable named
		// volume so a PMM server upgrade (in-GUI/Watchtower recreate) keeps all data —
		// otherwise the recreated container starts empty and you get "session closed" on
		// login (the Grafana DB / signing key are gone).
		srvVol := pmmDataVolume(st.ID, n.ID)
		if err := a.engCtx(ctx).VolumeCreate(ctx, srvVol); err != nil {
			logln("warning: could not create PMM data volume: " + err.Error())
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: ref, Hostname: host, Platform: platformAMD64,
			Env:     wtEnv,
			Network: networkName(st.ID), Aliases: []string{host},
			PublishMap: []PortMap{{ContainerPort: 8080, HostPort: httpPort}, {ContainerPort: 8443, HostPort: httpsPort}},
			Binds:      []string{srvVol + ":/srv"},
			DNS:        []string{intranetIP}, DNSSearch: []string{domain},
		})
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.engCtx(ctx).ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}
		// Use the Intranet as the sole resolver so forward + reverse DNS for every
		// stack host resolves through its authoritative zones.
		a.pointResolverAtIntranet(ctx, id, intranetIP, domain)

		// Record the published host ports for the access URLs.
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, "8080/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPPort = p
			}
		}
		if hp, e := a.engCtx(ctx).ContainerPort(ctx, id, "8443/tcp"); e == nil {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.HTTPSPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		logln(fmt.Sprintf("container started (http %d, https %d)", cfg.HTTPPort, cfg.HTTPSPort))

		setPhase("Waiting for PMM server", 35)
		if err := a.waitPMMReady(ctx, id, 180*time.Second); err != nil {
			failNode("PMM did not become ready: %v", err)
			return
		}
		logln("PMM server is ready")

		setPhase("Setting admin password", 60)
		if err := a.runStep(ctx, id, pmmAdminPasswordScript, []string{"PW=" + sec.AdminPassword}, logln); err != nil {
			failNode("set admin password: %v", err)
			return
		}
		logln("admin password set")

		setPhase("Configuring Grafana SMTP", 75)
		if err := a.runStep(ctx, id, pmmSMTPScript, []string{"SMTP_HOST=" + cfg.SMTPHost, "DOMAIN=" + domain}, logln); err != nil {
			failNode("configure SMTP: %v", err)
			return
		}
		logln("Grafana SMTP pointed at " + cfg.SMTPHost)

		if n.GenerateCert {
			setPhase("Issuing certificate from Intranet CA", 88)
			if werr := a.waitIntranetCAReady(ctx, intranetID, 180*time.Second); werr != nil {
				failNode("certificate: %v", werr)
				return
			}
			if _, err := a.pmmGenerateCert(ctx, id, intranetID, domain, host, 365, "days"); err != nil {
				failNode("generate certificate: %v", err)
				return
			}
			logln("nginx certificate signed by Intranet CA")
		}

		// Publish this node (and refresh all others) in the Intranet DNS zones.
		a.reconcileStackDNS(ctx, st.ID)

		a.trustIntranetCA(ctx, st, id, n.OS, logln)

		if n.LdapAuth {
			if err := a.pmmConfigureLDAP(ctx, st, n, doc, id, setPhase, logln); err != nil {
				logln("LDAP authentication skipped: " + err.Error())
			}
		}

		if n.EnableOIDC {
			if err := a.pmmConfigureOIDC(ctx, st, n, doc, id, setPhase, logln); err != nil {
				logln("Keycloak SSO skipped: " + err.Error())
			}
		}

		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d pmm %s: provisioned", st.ID, n.ID)
	}()
}

// runStep runs a single bash script in the container with up to 10 retries,
// logging each failed attempt. Mirrors the Intranet step loop.
func (a *App) runStep(ctx context.Context, id, script string, env []string, logln func(string)) error {
	var lastErr string
	for attempt := 1; attempt <= 10; attempt++ {
		// The stack may have been destroyed mid-deploy: bail immediately instead
		// of retrying against a container that no longer exists.
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", script}, env)
		if err == nil && res.Code == 0 {
			return nil
		}
		if err != nil {
			lastErr = err.Error()
		} else if lastErr = strings.TrimSpace(res.Stderr); lastErr == "" {
			lastErr = strings.TrimSpace(res.Stdout)
		}
		logln(fmt.Sprintf("attempt %d/10 failed: %s", attempt, lastLines(lastErr, 160)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("%s", lastLines(lastErr, 160))
}

// waitPMMReady polls the PMM readiness endpoint inside the container.
func (a *App) waitPMMReady(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	script := `curl -fsS -o /dev/null -w '%{http_code}' http://localhost:8080/v1/server/readyz 2>/dev/null`
	for time.Now().Before(deadline) {
		res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", script}, nil)
		if err == nil && strings.TrimSpace(res.Stdout) == "200" {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("readyz not 200 within %s", timeout)
}

// waitIntranet blocks until the stack's Intranet node is fully provisioned and
// running, returning its container id and IP on the stack network. Other nodes
// depend on the Intranet's services (DNS resolver, SMTP relay, LDAP, CA), so they
// must not start until it is up — this gates the whole provisioning sequence on
// the Intranet reaching the running state. Fails fast if the Intranet errors.
func (a *App) waitIntranet(ctx context.Context, stackID int64, doc designDoc, timeout time.Duration) (string, string, error) {
	var intranetNode string
	for _, n := range doc.Nodes {
		if n.Type == "intranet" {
			intranetNode = n.ID
			break
		}
	}
	if intranetNode == "" {
		return "", "", fmt.Errorf("an Intranet node is required in the stack")
	}
	netName := networkName(stackID)
	// The Intranet's address must be read on the Intranet's own engine, not the
	// calling node's — in a hybrid stack a VM node depends on the Docker Intranet
	// (and reaches its DNS/CA over VirtualBox NAT).
	ie := a.intranetEngine()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", "", err // stack destroyed mid-deploy
		}
		dep, err := a.store.GetDeployment(stackID, intranetNode)
		if err == nil {
			if dep.State == DeployError {
				return "", "", fmt.Errorf("Intranet failed to provision — cannot start dependent nodes")
			}
			if dep.State == DeployRunning && dep.ContainerID != "" {
				if ip, e := ie.ContainerIP(ctx, dep.ContainerID, netName); e == nil && ip != "" {
					return dep.ContainerID, ip, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return "", "", fmt.Errorf("Intranet did not become ready within %s", timeout)
}

// waitIntranetCAReady waits until the Intranet container has its CA material
// (created partway through Intranet provisioning).
func (a *App) waitIntranetCAReady(ctx context.Context, intranetID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, e := a.intranetEngine().Exec(ctx, intranetID, []string{"bash", "-c", `test -f /etc/pki/dbcanvas/ca.crt && test -f /etc/pki/dbcanvas/ca.key && echo ok`}, nil)
		if e == nil && strings.TrimSpace(res.Stdout) == "ok" {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("Intranet CA not ready within %s", timeout)
}

// pmmGenerateCert re-issues the PMM nginx certificate signed by the Intranet CA.
// It ferries the CA cert+key out of the Intranet container, drops them into the
// PMM container's /tmp, then runs an in-container openssl script that archives
// the existing /srv/nginx certs before writing the new ones. Returns the new
// certificate's notAfter line.
func (a *App) pmmGenerateCert(ctx context.Context, pmmID, intranetID, domain, alias string, value int, unit string) (string, error) {
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return "", fmt.Errorf("read Intranet CA cert: %w", err)
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return "", fmt.Errorf("read Intranet CA key: %w", err)
	}
	// Stage the CA material into the PMM container's /tmp owned by the pmm user
	// (UID 1000) that the in-container openssl runs as — so it can read the CA
	// key and later delete the files from sticky /tmp. (Docker's archive extract
	// honours the tar uid/gid even though the daemon itself is root.)
	if err := a.engCtx(ctx).PutArchive(ctx, pmmID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0600, pmmUID, caCrt},
		"dbca-ca.key": {0600, pmmUID, caKey},
	})); err != nil {
		return "", fmt.Errorf("stage CA into PMM: %w", err)
	}
	env := []string{
		fmt.Sprintf("VALUE=%d", value),
		"UNIT=" + unit,
		"DOMAIN=" + domain,
		"ALIAS=" + alias,
	}
	res, err := a.engCtx(ctx).Exec(ctx, pmmID, []string{"bash", "-c", pmmCertScript}, env)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("%s", lastLines(msg, 200))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// readContainerFile returns the bytes of a file inside a container (base64 over
// the exec channel, so binary-safe).
func (a *App) readContainerFile(ctx context.Context, id, path string) ([]byte, error) {
	res, err := a.engCtx(ctx).Exec(ctx, id, []string{"bash", "-c", "base64 -w0 " + path}, nil)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
}

// pmmUID is the unprivileged user the percona/pmm-server image runs as.
const pmmUID = 1000

// fileEntry is a file's mode, owner uid, and content for tarFiles.
type fileEntry struct {
	mode    int64
	uid     int
	content []byte
}

// tarFiles builds an uncompressed tar with the given name→content entries,
// stamping each with the requested owner uid (gid 0 = root group).
func tarFiles(files map[string]fileEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, f := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: f.mode, Uid: f.uid, Gid: 0, ModTime: time.Now(), Size: int64(len(f.content))})
		tw.Write(f.content)
	}
	tw.Close()
	return buf.Bytes()
}

// pmmAdminPasswordScript sets the Grafana/PMM admin password.
const pmmAdminPasswordScript = `set -e
change-admin-password "$PW"`

// pmmSMTPScript rewrites the [smtp] section of Grafana's config to relay through
// the Intranet mail server, then restarts Grafana. Any pre-existing [smtp]
// block is stripped first so the section is never duplicated.
const pmmSMTPScript = `set -e
INI=/etc/grafana/grafana.ini
[ -f "$INI" ] || { echo "grafana.ini not found"; exit 1; }
# Drop an existing [smtp] section (up to the next [section] header).
awk '
  /^[[:space:]]*\[smtp\]/ { skip=1; next }
  /^[[:space:]]*\[/       { skip=0 }
  { if (!skip) print }
' "$INI" > "$INI.tmp"
cat >> "$INI.tmp" <<EOF

[smtp]
enabled = true
host = $SMTP_HOST
user =
# If the password contains # or ; you have to wrap it with triple quotes. Ex """#password;"""
password =
cert_file =
key_file =
skip_verify = true
from_address = admin@grafana.localhost
from_name = Grafana
ehlo_identity =
startTLS_policy = NoStartTLS
EOF
mv "$INI.tmp" "$INI"
supervisorctl restart grafana >/dev/null 2>&1 || true`

// pmmCertScript signs a fresh nginx server certificate from the Intranet CA
// staged at /tmp/dbca-ca.{crt,key}. It archives the current /srv/nginx
// certificate material before replacing it, keeps the existing dhparam.pem, and
// reloads nginx. Prints the new notAfter date.
const pmmCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
DIR=/srv/nginx
CA=/tmp/dbca-ca.crt
CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing in /tmp"; exit 1; }

# Archive the existing certificate set before replacing it.
TS=$(date -u +%Y%m%d%H%M%S)
ARCHIVE="$DIR/archive/$TS"
install -d "$ARCHIVE"
for f in certificate.crt certificate.key ca-certs.pem certificate.conf dhparam.pem; do
  [ -f "$DIR/$f" ] && cp -a "$DIR/$f" "$ARCHIVE/$f" || true
done

# certificate.conf with SANs covering the PMM aliases and localhost.
cat > "$DIR/certificate.conf" <<EOF
[ req ]
distinguished_name = req_distinguished_name
req_extensions     = v3_req
prompt             = no

[ req_distinguished_name ]
O                  = DBCanvas
CN                 = $ALIAS.$DOMAIN

[ v3_req ]
basicConstraints = CA:FALSE
keyUsage         = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth, clientAuth
subjectAltName   = @alt_names

[ alt_names ]
DNS.1 = $ALIAS
DNS.2 = $ALIAS.$DOMAIN
DNS.3 = pmm
DNS.4 = localhost
IP.1  = 127.0.0.1
EOF

END=$(date -u -d "+$SECS seconds" +%Y%m%d%H%M%SZ)
openssl req -newkey rsa:2048 -nodes -keyout "$DIR/certificate.key" \
  -out /tmp/pmm.csr -config "$DIR/certificate.conf" >/dev/null 2>&1
openssl x509 -req -in /tmp/pmm.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial \
  -out "$DIR/certificate.crt" -extensions v3_req -extfile "$DIR/certificate.conf" \
  -not_after "$END" >/dev/null 2>&1
# Publish the signing CA as the chain bundle nginx serves.
cp -f "$CA" "$DIR/ca-certs.pem"
# Keep a dhparam.pem (reuse the archived one, else generate a small fresh set).
[ -f "$DIR/dhparam.pem" ] || openssl dhparam -out "$DIR/dhparam.pem" 2048 >/dev/null 2>&1

# nginx runs as pmm:root; keep ownership and lock down the private key.
chown pmm:root "$DIR/certificate.crt" "$DIR/certificate.key" "$DIR/ca-certs.pem" "$DIR/certificate.conf" 2>/dev/null || true
chmod 600 "$DIR/certificate.key"
chmod 644 "$DIR/certificate.crt" "$DIR/ca-certs.pem" "$DIR/certificate.conf"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/dbca-ca.srl /tmp/pmm.csr
supervisorctl restart nginx >/dev/null 2>&1 || nginx -s reload >/dev/null 2>&1 || true
openssl x509 -in "$DIR/certificate.crt" -noout -enddate`
