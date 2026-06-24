package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// design parsing (the canvas document stored in stacks.design_json)
type designNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	// PMM node fields (ignored by other node types).
	Version       string `json:"version"`       // PMM minor version tag ("" → catalog default)
	AdminPassword string `json:"adminPassword"` // PMM admin password ("" → auto-generated)
	GenerateCert  bool   `json:"generateCert"`  // sign nginx certs from the Intranet CA on deploy
	// PXC node fields — a PXC node belongs to a PXC frame (FrameID) and is either
	// a data member ("regular") or a voting-only "arbitrator" (garbd).
	FrameID        string `json:"frameId"`
	Role           string `json:"role"`           // "regular" | "arbitrator"
	ExportEnabled  bool   `json:"exportEnabled"`  // publish the DB port to the host
	ExportHostPort int    `json:"exportHostPort"` // desired host port (0 = random/unused)
}

// designFrame is a group container on the canvas. Currently only the PXC cluster
// frame, which holds PXC nodes and carries cluster-wide configuration.
type designFrame struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"` // "pxc"
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	// PXC cluster config.
	OS           string `json:"os"`           // os family: "oraclelinux" | "ubuntu"
	OSVersion    string `json:"osVersion"`    // e.g. "9" | "24.04"
	Arch         string `json:"arch"`         // "amd64" | "arm64"
	PXCMajor     string `json:"pxcMajor"`     // "8.0" | "8.4"
	PXCVersion   string `json:"pxcVersion"`   // minor (e.g. 8.0.45-36.1); "" → latest
	RootPassword string `json:"rootPassword"` // "" → auto-generated
	PMMNodeID    string `json:"pmmNodeId"`    // PMM node that monitors this cluster (optional)
	UseProxy     bool   `json:"useProxy"`     // route egress via the Intranet Squid proxy
	GTID         bool   `json:"gtid"`         // enable GTID (default on)
	GenerateCert bool   `json:"generateCert"` // per-node certs signed by the Intranet CA
	CertTTLValue int    `json:"certTtlValue"`
	CertTTLUnit  string `json:"certTtlUnit"`
}

type designDoc struct {
	Nodes  []designNode  `json:"nodes"`
	Frames []designFrame `json:"frames"`
}

// nodeConfig is the non-secret profile shown for a deployed node.
type nodeConfig struct {
	Domain      string   `json:"domain"`
	BaseDN      string   `json:"baseDN"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Alias       string   `json:"alias"`
	Hostname    string   `json:"hostname"`
	FQDN        string   `json:"fqdn"`
	LDAPAdminDN string   `json:"ldapAdminDN"`
	Services    []string `json:"services"`
	WebmailPort int      `json:"webmailPort,omitempty"`
}

// provProgress is the live provisioning status surfaced to the deployment console.
type provProgress struct {
	Percent int      `json:"percent"`
	Phase   string   `json:"phase"`
	Log     []string `json:"log"`
	Message string   `json:"message,omitempty"`
}

// provStep is one idempotent provisioning step (retried up to 10×).
type provStep struct {
	Name   string
	Script string
}

// nodeSecrets holds generated credentials for a deployed node.
type nodeSecrets struct {
	Domain            string `json:"domain"`
	BaseDN            string `json:"baseDN"`
	LDAPAdminDN       string `json:"ldapAdminDN"`
	LDAPAdminPassword string `json:"ldapAdminPassword"`
	MailAdminUser     string `json:"mailAdminUser"`
	MailAdminPassword string `json:"mailAdminPassword"`
}

type issue struct {
	Level   string `json:"level"` // info | warning | error
	Message string `json:"message"`
}

func hasError(issues []issue) bool {
	for _, i := range issues {
		if i.Level == "error" {
			return true
		}
	}
	return false
}

func networkName(stackID int64) string { return fmt.Sprintf("dbcanvas-stack-%d", stackID) }

func containerName(stackID int64, nodeID string) string {
	return fmt.Sprintf("dbcanvas-%d-%s", stackID, sanitizeName(nodeID))
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// domainToDN turns "example.net" into "dc=example,dc=net".
func domainToDN(domain string) string {
	parts := strings.Split(domain, ".")
	for i, p := range parts {
		parts[i] = "dc=" + p
	}
	return strings.Join(parts, ",")
}

// genSecret returns prefix + 8 uppercase hex chars (e.g. LdapAdm!A02FB5C6).
func genSecret(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + strings.ToUpper(hex.EncodeToString(b))
}

// archOr returns the node's chosen arch, falling back to the host arch.
func archOr(a string) string {
	if a == "amd64" || a == "arm64" {
		return a
	}
	return hostArch()
}

func intranetImage(arch string) string {
	return "dbcanvas-systemd:oraclelinux-9-" + archOr(arch)
}

// --- validation ---

func (a *App) validateStack(ctx context.Context, st Stack) []issue {
	var out []issue
	if err := a.docker.Ping(ctx); err != nil {
		return append(out, issue{"error", "Docker is not reachable: " + err.Error()})
	}
	if osEnv := envOr("DOMAIN", ""); osEnv == "" {
		out = append(out, issue{"warning", "DOMAIN is not set; using default example.net"})
	}
	var doc designDoc
	if err := json.Unmarshal(st.Design, &doc); err != nil {
		return append(out, issue{"error", "stack design is invalid"})
	}
	if len(doc.Nodes) == 0 {
		out = append(out, issue{"warning", "Stack has no nodes to deploy"})
	}
	intranet := 0
	others := 0
	labels := map[string]int{}
	seenImg := map[string]bool{}
	pmmCat := loadPMMCatalog()
	for _, n := range doc.Nodes {
		labels[strings.TrimSpace(n.Label)]++
		switch n.Type {
		case "intranet":
			intranet++
			img := intranetImage(n.Arch)
			if !seenImg[img] {
				seenImg[img] = true
				if ok, _ := a.docker.ImageExists(ctx, img); !ok {
					out = append(out, issue{"error", "Missing image " + img + " — run `make images` first"})
				}
			}
		case "pmm":
			others++
			if !pmmCat.validPMMTag(n.Version) {
				out = append(out, issue{"warning", "Unknown PMM version " + n.Version + " for node " + n.Label + " — run `make versions`"})
			}
		default:
			others++
		}
	}
	if intranet > 1 {
		out = append(out, issue{"error", "Only one Intranet node is allowed per stack"})
	}
	// The Intranet provides DNS, mail, LDAP and the CA for the whole stack, so it
	// is required before any other node can be deployed.
	if others > 0 && intranet == 0 {
		out = append(out, issue{"error", "An Intranet node is required — add one before deploying other nodes"})
	}
	// Labels become DNS hostnames, so they must be present and unique — a stack
	// with duplicate (or blank) labels cannot be deployed.
	if labels[""] > 0 {
		out = append(out, issue{"error", "Every node must have a label"})
	}
	for l, c := range labels {
		if c > 1 && l != "" {
			out = append(out, issue{"error", "Duplicate node label: " + l + " — labels must be unique"})
		}
	}
	if len(out) == 0 {
		out = append(out, issue{"info", "All checks passed"})
	}
	return out
}

func (a *App) handleValidateStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	issues := a.validateStack(r.Context(), st)
	writeJSON(w, http.StatusOK, map[string]any{"ok": !hasError(issues), "issues": issues})
}

// --- deploy ---

func (a *App) handleDeployStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	bg := context.Background()
	issues := a.validateStack(bg, st)
	if hasError(issues) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "issues": issues})
		return
	}

	var doc designDoc
	json.Unmarshal(st.Design, &doc)

	if err := a.docker.NetworkEnsure(bg, networkName(st.ID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create network: "+err.Error())
		return
	}

	deps, _ := a.store.ListDeployments(st.ID)
	existing := map[string]Deployment{}
	for _, d := range deps {
		existing[d.NodeID] = d
	}
	inDesign := map[string]bool{}
	for _, n := range doc.Nodes {
		inDesign[n.ID] = true
	}

	// Remove containers for nodes deleted from the canvas.
	removed := false
	for _, d := range deps {
		if !inDesign[d.NodeID] {
			if d.ContainerID != "" {
				a.docker.ContainerRemove(bg, d.ContainerID)
			}
			a.store.DeleteDeployment(st.ID, d.NodeID)
			removed = true
		}
	}
	// Drop removed hosts from the Intranet DNS zones.
	if removed {
		a.reconcileStackDNS(bg, st.ID)
	}

	// Create newly added nodes; keep already-running ones (redeploy).
	for _, n := range doc.Nodes {
		if d, ok := existing[n.ID]; ok && d.State == DeployRunning {
			continue
		}
		switch n.Type {
		case "intranet":
			a.provisionIntranet(st, n)
		case "pmm":
			a.provisionPMM(st, n, doc)
		}
	}

	a.store.SetStackStatus(st.ID, StackDeployed)
	out, _ := a.store.ListDeployments(st.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"deployments": out})
}

// provisionIntranet records the deployment and starts an async provisioning
// goroutine for an Intranet node.
func (a *App) provisionIntranet(st Stack, n designNode) {
	domain := envOr("DOMAIN", "example.net")
	baseDN := domainToDN(domain)
	adminDN := "cn=admin," + baseDN

	// reuse secrets if this node was deployed before (keeps creds stable)
	var sec nodeSecrets
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		json.Unmarshal(dep.Secrets, &sec)
	}
	if sec.LDAPAdminPassword == "" {
		sec = nodeSecrets{
			Domain:            domain,
			BaseDN:            baseDN,
			LDAPAdminDN:       adminDN,
			LDAPAdminPassword: genSecret("LdapAdm!"),
			MailAdminUser:     "admin@" + domain,
			MailAdminPassword: genSecret("MailAdm!"),
		}
	}
	cfg := nodeConfig{
		Domain: domain, BaseDN: baseDN, OS: "oel9", Arch: archOr(n.Arch),
		Alias: "intranet", Hostname: "intranet", FQDN: "intranet." + domain, LDAPAdminDN: adminDN,
		Services: []string{"Squid proxy", "DNS", "SMTP", "IMAP", "Webmail (RoundCube)", "OpenLDAP", "Self-signing CA"},
	}
	cfgJSON, _ := json.Marshal(cfg)
	secJSON, _ := json.Marshal(sec)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	// Each node provisions in its own goroutine, so one failing never blocks the
	// others. Steps are retried up to 10×; progress is published for the console.
	go func() {
		ctx := context.Background()
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
			log.Printf("stack %d node %s: %s", st.ID, n.ID, msg)
			prog.Phase = "failed"
			prog.Message = msg
			save()
			a.store.SetDeploymentState(st.ID, n.ID, DeployError)
		}

		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)
		setPhase("Creating container", 3)

		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.docker.ContainerByName(ctx, name); ok {
			a.docker.ContainerRemove(ctx, cid)
		}
		// Pin the Intranet to a stable address (host .2 of the stack subnet) so it
		// stays a reliable DNS resolver / SMTP relay for the other nodes across
		// restarts. The FQDN alias also lets peers reach it as intranet.<domain>.
		subnet, _ := a.docker.NetworkSubnet(ctx, networkName(st.ID))
		id, err := a.docker.ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: intranetImage(n.Arch), Hostname: "intranet",
			Network: networkName(st.ID), Aliases: []string{"intranet", "intranet." + domain},
			Privileged: true, PublishPort: 80, IPv4Address: staticIntranetIP(subnet),
		})
		if err != nil {
			failNode("create container: %v", err)
			return
		}
		if err := a.docker.ContainerStart(ctx, id); err != nil {
			failNode("start container: %v", err)
			return
		}

		// record the auto-assigned (unused) host port for RoundCube
		if hp, e := a.docker.ContainerPort(ctx, id, "80/tcp"); e == nil && hp != "" {
			if p, e2 := strconv.Atoi(hp); e2 == nil {
				cfg.WebmailPort = p
			}
		}
		cfgJSON, _ = json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})
		logln(fmt.Sprintf("container started (webmail host port %d)", cfg.WebmailPort))

		setPhase("Waiting for systemd", 8)
		if err := a.docker.WaitSystemd(ctx, id, 90*time.Second); err != nil {
			failNode("systemd did not start: %v", err)
			return
		}

		env := []string{
			"DOMAIN=" + sec.Domain,
			"BASE_DN=" + sec.BaseDN,
			"LDAP_ADMIN_DN=" + sec.LDAPAdminDN,
			"LDAP_ADMIN_PW=" + sec.LDAPAdminPassword,
			"MAIL_ADMIN=admin",
			"MAIL_ADMIN_PW=" + sec.MailAdminPassword,
		}
		steps := intranetSteps()
		for i, step := range steps {
			setPhase(step.Name, 10+i*88/len(steps))
			lastErr := ""
			ok := false
			for attempt := 1; attempt <= 10; attempt++ {
				res, err := a.docker.Exec(ctx, id, []string{"bash", "-c", step.Script}, env)
				if err == nil && res.Code == 0 {
					ok = true
					break
				}
				if err != nil {
					lastErr = err.Error()
				} else if lastErr = strings.TrimSpace(res.Stderr); lastErr == "" {
					lastErr = strings.TrimSpace(res.Stdout)
				}
				logln(fmt.Sprintf("%s: attempt %d/10 failed: %s", step.Name, attempt, lastLines(lastErr, 160)))
				time.Sleep(2 * time.Second)
			}
			if !ok {
				failNode("step %q failed after 10 attempts: %s", step.Name, lastLines(lastErr, 160))
				return
			}
			logln(step.Name + ": ok")
		}

		// Configure bind as the authoritative resolver and publish DNS records for
		// every host in the stack (including the Intranet itself).
		setPhase("Publishing DNS records", 98)
		a.reconcileStackDNS(ctx, st.ID)
		logln("DNS zones published")

		setPhase("Running", 100)
		prog.Message = "provisioned"
		save()
		a.store.SetDeploymentState(st.ID, n.ID, DeployRunning)
		log.Printf("stack %d node %s: provisioned", st.ID, n.ID)
	}()
}

// intranetSteps is the ordered, idempotent provisioning sequence. Each step is
// run via `bash -c` inside the container and may be retried.
func intranetSteps() []provStep {
	return []provStep{
		{"Enable repositories", `set -e
dnf -y install oracle-epel-release-el9 >/dev/null 2>&1 || dnf -y install epel-release >/dev/null 2>&1 || true
dnf config-manager --set-enabled ol9_codeready_builder >/dev/null 2>&1 || true`},

		{"Install packages", `set -e
dnf -y install squid bind bind-utils postfix dovecot openldap-servers openldap-clients httpd php php-fpm roundcubemail mod_ssl openssl net-tools >/dev/null`},

		{"Create CA", `set -e
install -d -m 0755 /etc/pki/dbcanvas
if [ ! -f /etc/pki/dbcanvas/ca.crt ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -keyout /etc/pki/dbcanvas/ca.key -out /etc/pki/dbcanvas/ca.crt -subj "/O=DBCanvas/CN=DBCanvas CA" >/dev/null 2>&1
fi
chmod 600 /etc/pki/dbcanvas/ca.key 2>/dev/null || true`},

		{"Configure OpenLDAP", `set -e
chown -R ldap:ldap /var/lib/ldap 2>/dev/null || true
systemctl enable --now slapd
for i in $(seq 1 20); do ldapsearch -Y EXTERNAL -H ldapi:/// -b cn=config -s base >/dev/null 2>&1 && break; sleep 1; done
HASH=$(slappasswd -s "$LDAP_ADMIN_PW")
cat >/tmp/db.ldif <<EOF
dn: olcDatabase={2}mdb,cn=config
changetype: modify
replace: olcSuffix
olcSuffix: $BASE_DN
-
replace: olcRootDN
olcRootDN: $LDAP_ADMIN_DN
-
replace: olcRootPW
olcRootPW: $HASH
EOF
ldapmodify -Y EXTERNAL -H ldapi:/// -f /tmp/db.ldif
for s in cosine inetorgperson nis; do ldapadd -Y EXTERNAL -H ldapi:/// -f "/etc/openldap/schema/$s.ldif" >/dev/null 2>&1 || true; done`},

		{"Seed LDAP directory", `set -e
DC="${BASE_DN%%,*}"; DC="${DC#dc=}"
cat >/tmp/base.ldif <<EOF
dn: $BASE_DN
objectClass: top
objectClass: dcObject
objectClass: organization
o: $DOMAIN
dc: $DC

dn: ou=People,$BASE_DN
objectClass: organizationalUnit
ou: People

dn: ou=Groups,$BASE_DN
objectClass: organizationalUnit
ou: Groups
EOF
ldapadd -x -D "$LDAP_ADMIN_DN" -w "$LDAP_ADMIN_PW" -f /tmp/base.ldif 2>/dev/null || ldapsearch -x -D "$LDAP_ADMIN_DN" -w "$LDAP_ADMIN_PW" -b "$BASE_DN" -s base dn >/dev/null`},

		{"Configure mail", `set -e
getent group vmail >/dev/null || groupadd -g 5000 vmail
id vmail >/dev/null 2>&1 || useradd -g vmail -u 5000 -d /var/mail/vhosts -s /sbin/nologin vmail
install -d -o vmail -g vmail "/var/mail/vhosts/$DOMAIN"
postconf -e "myhostname = intranet.$DOMAIN" "mydomain = $DOMAIN" "myorigin = \$mydomain" "inet_interfaces = all" "inet_protocols = ipv4" "virtual_mailbox_domains = $DOMAIN" "virtual_mailbox_base = /var/mail/vhosts" "virtual_mailbox_maps = hash:/etc/postfix/vmailbox" "virtual_minimum_uid = 5000" "virtual_uid_maps = static:5000" "virtual_gid_maps = static:5000"
touch /etc/postfix/vmailbox
grep -q "^$MAIL_ADMIN@$DOMAIN " /etc/postfix/vmailbox || echo "$MAIL_ADMIN@$DOMAIN $DOMAIN/$MAIL_ADMIN/" >> /etc/postfix/vmailbox
postmap /etc/postfix/vmailbox
install -d /etc/dovecot
[ -f /etc/dovecot/users ] || echo "$MAIL_ADMIN@$DOMAIN:{PLAIN}$MAIL_ADMIN_PW::::::" > /etc/dovecot/users
# Wire dovecot to authenticate the virtual users (passwd-file) over plaintext
# IMAP on localhost, with maildirs matching postfix's virtual_mailbox_base.
cat > /etc/dovecot/conf.d/99-dbcanvas.conf <<'DCONF'
protocols = imap
ssl = no
disable_plaintext_auth = no
auth_mechanisms = plain login
mail_location = maildir:/var/mail/vhosts/%d/%n
first_valid_uid = 5000
passdb {
  driver = passwd-file
  args = scheme=PLAIN username_format=%u /etc/dovecot/users
}
userdb {
  driver = static
  args = uid=vmail gid=vmail home=/var/mail/vhosts/%d/%n
}
DCONF`},

		{"Configure webmail", `set -e
install -d -o apache -g apache /var/lib/roundcubemail
RC=/etc/roundcubemail/config.inc.php
cat > "$RC" <<'RCCFG'
<?php
$config = [];
$config['db_dsnw'] = 'sqlite:////var/lib/roundcubemail/roundcube.db?mode=0646';
$config['imap_host'] = 'localhost';
$config['imap_port'] = 143;
// SMTP: localhost:25 with no auth (delivery permitted via postfix mynetworks).
// smtp_server/smtp_port are the RoundCube 1.5 keys; smtp_host is the 1.6 name.
$config['smtp_server'] = 'localhost';
$config['smtp_port'] = 25;
$config['smtp_host'] = 'localhost:25';
$config['smtp_user'] = '';
$config['smtp_pass'] = '';
$config['des_key'] = 'dbcanvasRoundcube24key!!';
$config['enable_installer'] = false;
$config['support_url'] = '';
$config['product_name'] = 'DBCanvas Webmail';
RCCFG
chown apache:apache "$RC" 2>/dev/null || true
php -r '$f="/var/lib/roundcubemail/roundcube.db"; if(!file_exists($f)){$db=new PDO("sqlite:".$f); $db->exec(file_get_contents("/usr/share/roundcubemail/SQL/sqlite.initial.sql"));}' 2>/dev/null || true
chown -R apache:apache /var/lib/roundcubemail 2>/dev/null || true
CONF=/etc/httpd/conf.d/roundcubemail.conf
[ -f "$CONF" ] && sed -i 's/Require local/Require all granted/g' "$CONF" || true
true`},

		{"Enable services", `set -e
echo "ServerName intranet.$DOMAIN" > /etc/httpd/conf.d/servername.conf
for svc in slapd squid named postfix dovecot php-fpm httpd; do
  systemctl enable "$svc" >/dev/null 2>&1 || true
  systemctl restart "$svc" >/dev/null 2>&1 || true
done`},
	}
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// --- lifecycle + profile ---

func (a *App) handleGetNode(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	dep, err := a.store.GetDeployment(st.ID, r.PathValue("nid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "node is not deployed")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (a *App) handleNodeAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, _, ok := a.loadOwnedStack(w, r)
		if !ok {
			return
		}
		nid := r.PathValue("nid")
		dep, err := a.store.GetDeployment(st.ID, nid)
		if err != nil || dep.ContainerID == "" {
			writeErr(w, http.StatusNotFound, "node is not deployed")
			return
		}
		ctx := r.Context()
		switch action {
		case "start":
			err = a.docker.ContainerStart(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployRunning)
				a.refreshPublishedPorts(ctx, st, nid, dep)
				a.restoreNodeResolver(ctx, st, nid, dep)
				a.reconcileStackDNS(ctx, st.ID)
			}
		case "stop":
			err = a.docker.ContainerStop(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployStopped)
			}
		case "restart":
			err = a.docker.ContainerRestart(ctx, dep.ContainerID)
			if err == nil {
				a.store.SetDeploymentState(st.ID, nid, DeployRunning)
				a.refreshPublishedPorts(ctx, st, nid, dep)
				a.restoreNodeResolver(ctx, st, nid, dep)
				a.reconcileStackDNS(ctx, st.ID)
			}
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		updated, _ := a.store.GetDeployment(st.ID, nid)
		writeJSON(w, http.StatusOK, updated)
	}
}

// refreshPublishedPorts re-reads a node container's auto-assigned host ports and
// persists them into the stored config. Containers are created with an empty
// HostPort binding, so Docker hands out a *new* ephemeral host port every time
// the container starts — meaning a stop/start or restart changes the published
// port and would otherwise leave the recorded access links (Intranet webmail,
// PMM 8080/8443) pointing at the old, now-invalid port. Called after start and
// restart for both node types.
func (a *App) refreshPublishedPorts(ctx context.Context, st Stack, nid string, dep Deployment) {
	if dep.ContainerID == "" {
		return
	}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	typ := ""
	for _, n := range doc.Nodes {
		if n.ID == nid {
			typ = n.Type
			break
		}
	}
	readPort := func(portProto string) (int, bool) {
		hp, err := a.docker.ContainerPort(ctx, dep.ContainerID, portProto)
		if err != nil || hp == "" {
			return 0, false
		}
		v, err := strconv.Atoi(hp)
		return v, err == nil
	}
	save := func(cfg any) {
		b, _ := json.Marshal(cfg)
		a.store.UpsertDeployment(Deployment{
			StackID: dep.StackID, NodeID: dep.NodeID, ContainerID: dep.ContainerID,
			State: DeployRunning, Config: b, Secrets: dep.Secrets,
		})
	}
	switch typ {
	case "intranet":
		var cfg nodeConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("80/tcp"); ok {
			cfg.WebmailPort = p
		}
		save(cfg)
	case "pmm":
		var cfg pmmConfig
		json.Unmarshal(dep.Config, &cfg)
		if p, ok := readPort("8080/tcp"); ok {
			cfg.HTTPPort = p
		}
		if p, ok := readPort("8443/tcp"); ok {
			cfg.HTTPSPort = p
		}
		save(cfg)
	}
}

// handleDestroyStack tears down the deployment (all containers + the per-stack
// network), clears the deployment records, and returns the stack to draft so it
// can be redeployed fresh. The stack design is preserved; post-deployment-only
// node state (generated credentials, LDAP/email users, certificates) is reset
// because the deployment rows and containers are removed.
func (a *App) handleDestroyStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	a.teardownStack(st.ID)
	a.store.SetStackStatus(st.ID, StackDraft)
	writeJSON(w, http.StatusOK, map[string]any{"status": StackDraft, "deployments": []Deployment{}})
}

// teardownStack stops and removes every container deployed for a stack and
// removes its network. Best-effort.
func (a *App) teardownStack(stackID int64) {
	if a.docker == nil {
		return
	}
	ctx := context.Background()
	deps, _ := a.store.ListDeployments(stackID)
	for _, d := range deps {
		if d.ContainerID != "" {
			a.docker.ContainerRemove(ctx, d.ContainerID)
		}
		a.store.DeleteDeployment(stackID, d.NodeID)
	}
	a.docker.NetworkRemove(ctx, networkName(stackID))
}
