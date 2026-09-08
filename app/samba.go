package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Samba AD DC node (Type=="sambaad") — a singleton Active Directory Domain Controller,
// deployed only on the Ubuntu 24.04 base image (complete Samba packages). Its realm comes
// from DOMAIN (example.net → realm EXAMPLE.NET, workgroup EXAMPLE) and the Administrator
// password from SAMBA_PASSWORD. It provides: LDAP user/group management (samba-tool), a
// downloadable krb5.conf, per-service Kerberos principals (postgres/<fqdn>, mongodb/<fqdn>)
// with exportable keytabs, and optional TLS signed by the Intranet CA. smb.conf carries
// `ldap server require strong auth = no` so plain ldap:// binds work.

const sambaService = "samba-ad-dc"

func sambaPassword() string { return envOr("SAMBA_PASSWORD", "SambaPassword2026") }

// sambaRealm / sambaWorkgroup derive the AD realm + NetBIOS domain from DOMAIN.
func sambaRealm(domain string) string { return strings.ToUpper(domain) }
func sambaWorkgroup(domain string) string {
	d := domain
	if i := strings.IndexByte(d, '.'); i > 0 {
		d = d[:i]
	}
	return strings.ToUpper(d)
}

type sambaConfig struct {
	Image        string `json:"image"`
	OS           string `json:"os"`
	OSVersion    string `json:"osVersion"`
	Arch         string `json:"arch"`
	Hostname     string `json:"hostname"`
	FQDN         string `json:"fqdn"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	Workgroup    string `json:"workgroup"`
	BaseDN       string `json:"baseDN"`
	AdminUser    string `json:"adminUser"`
	BindDN       string `json:"bindDN"`
	GenerateCert bool   `json:"generateCert"`
	UseProxy     bool   `json:"useProxy"`
	TLS          bool   `json:"tls"`
}

type sambaSecrets struct {
	AdminPassword string `json:"adminPassword"`
	BindPassword  string `json:"bindPassword"`
}

// provisionSambaNode brings up the AD DC: install Samba, provision the domain, enable plain
// LDAP binds, write krb5.conf, optionally apply an Intranet-CA cert, start the DC, and
// create an `ldapbind` service account + a sample user/group.
func (a *App) provisionSambaNode(st Stack, n designNode, doc designDoc) {
	domain := envOr("DOMAIN", "example.net")
	host := stackHostnames(doc)[n.ID]
	if host == "" {
		host = sanitizeName(n.Label)
	}
	fqdn := fqdnOf(host, domain)
	os := n.OS
	if os == "" {
		os = "ubuntu"
	}
	osVersion := "24.04" // Samba AD DC is Ubuntu 24.04 only
	image := pxcImage(os, osVersion, n.Arch)
	realm := sambaRealm(domain)
	workgroup := sambaWorkgroup(domain)
	baseDN := domainToDN(domain)

	// Admin password from SAMBA_PASSWORD; bind password reused across redeploys.
	bindPass := genSecret("Bind1!")
	if dep, err := a.store.GetDeployment(st.ID, n.ID); err == nil && len(dep.Secrets) > 0 {
		var s sambaSecrets
		if json.Unmarshal(dep.Secrets, &s) == nil && s.BindPassword != "" {
			bindPass = s.BindPassword
		}
	}
	sec := sambaSecrets{AdminPassword: sambaPassword(), BindPassword: bindPass}
	secJSON, _ := json.Marshal(sec)

	cfg := sambaConfig{
		Image: image, OS: os, OSVersion: osVersion, Arch: archOr(n.Arch),
		Hostname: host, FQDN: fqdn, Domain: domain, Realm: realm, Workgroup: workgroup,
		BaseDN: baseDN, AdminUser: "Administrator", BindDN: "CN=ldapbind,CN=Users," + baseDN,
		GenerateCert: n.GenerateCert, UseProxy: n.UseProxy, TLS: n.GenerateCert,
	}
	cfgJSON, _ := json.Marshal(cfg)
	a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, State: DeployPending, Config: cfgJSON, Secrets: secJSON})

	ctx, endScope := a.deployScope(st.ID, a.nodeEngine(st, n.Type))
	go func() {
		defer endScope()
		pr := a.pxcNewProg(st.ID, n.ID)
		a.store.SetDeploymentState(st.ID, n.ID, DeployProvisioning)

		if ok, _ := a.engCtx(ctx).ImageExists(ctx, image); !ok {
			pr.fail("image %s not found — run `make images` first", image)
			return
		}
		pr.phase("Waiting for Intranet to be ready", 6)
		intranetID, intranetIP, werr := a.waitIntranet(ctx, st.ID, doc, deployTimeout())
		if werr != nil {
			pr.fail("%v", werr)
			return
		}

		pr.phase("Creating container", 12)
		name := containerName(st.ID, n.ID)
		if cid, ok, _ := a.engCtx(ctx).ContainerByName(ctx, name); ok {
			a.engCtx(ctx).ContainerRemove(ctx, cid)
		}
		id, err := a.engCtx(ctx).ContainerCreate(ctx, ContainerSpec{
			Name: name, Image: image, Hostname: host, Privileged: true,
			Network: networkName(st.ID), Aliases: []string{host, fqdn},
			DNS: []string{intranetIP}, DNSSearch: []string{domain},
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
		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployProvisioning, Config: cfgJSON, Secrets: secJSON})

		pr.phase("Waiting for systemd", 16)
		if err := a.engCtx(ctx).WaitSystemd(ctx, id, 90*time.Second); err != nil {
			pr.fail("systemd did not start: %v", err)
			return
		}
		a.trustIntranetCA(ctx, st, id, n.OS, pr.logln)
		if n.UseProxy {
			if err := a.runStep(ctx, id, pkgProxyDebian, []string{"PROXY=http://intranet." + domain + ":3128"}, pr.logln); err != nil {
				pr.logln("configure apt proxy skipped: " + err.Error())
			}
		}

		pr.phase("Installing Samba + Kerberos", 30)
		if err := a.runStep(ctx, id, sambaInstallScript, []string{"REALM=" + realm}, pr.logln); err != nil {
			pr.fail("install samba: %v", err)
			return
		}
		pr.logln("samba + krb5 + ldap tooling installed")

		pr.phase("Provisioning Active Directory domain", 55)
		provEnv := []string{"REALM=" + realm, "WORKGROUP=" + workgroup, "ADMINPASS=" + sec.AdminPassword, "DOMAIN=" + domain, "FQDN=" + fqdn, "INTRANET=" + intranetIP}
		if err := a.runStep(ctx, id, sambaProvisionScript, provEnv, pr.logln); err != nil {
			pr.fail("provision domain: %v", err)
			return
		}
		pr.logln("AD domain " + realm + " provisioned (strong-auth off, krb5.conf written)")

		if n.GenerateCert {
			pr.phase("Issuing TLS certificate (Intranet CA)", 70)
			if err := a.sambaApplyCert(ctx, id, intranetID, fqdn, n.CertTTLValue, n.CertTTLUnit, pr.logln); err != nil {
				pr.fail("%v", err)
				return
			}
		}

		pr.phase("Starting AD DC", 82)
		if err := a.runStep(ctx, id, sambaStartScript, nil, pr.logln); err != nil {
			pr.fail("start samba-ad-dc: %v", err)
			return
		}
		pr.phase("Creating LDAP bind account + sample directory", 92)
		if err := a.runStep(ctx, id, sambaAccountsScript, []string{"BINDPW=" + sec.BindPassword}, pr.logln); err != nil {
			pr.logln("directory bootstrap had issues (continuing): " + err.Error())
		} else {
			pr.logln("ldapbind account + sample user/group created")
		}

		a.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: n.ID, ContainerID: id, State: DeployRunning, Config: cfgJSON, Secrets: secJSON})
		a.reconcileStackDNS(ctx, st.ID)
		pr.phase("Running", 100)
		pr.p.Message = "provisioned"
		pr.save()
		log.Printf("stack %d samba %s: provisioned (realm %s)", st.ID, n.Label, realm)
	}()
}

// sambaApplyCert stages the Intranet CA and signs a server cert for the DC FQDN into
// /var/lib/samba/private/tls/{cert,key,ca}.pem — Samba's default TLS paths, so LDAPS on
// :636 is served with an Intranet-CA-signed cert. Mirrors pgApplyCert.
func (a *App) sambaApplyCert(ctx context.Context, containerID, intranetID, fqdn string, ttlValue int, ttlUnit string, logln func(string)) error {
	if err := a.waitIntranetCAReady(ctx, intranetID, 120*time.Second); err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	caCrt, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.crt")
	if err != nil {
		return fmt.Errorf("read CA cert: %w", err)
	}
	caKey, err := a.readIntranetFile(ctx, intranetID, "/etc/pki/dbcanvas/ca.key")
	if err != nil {
		return fmt.Errorf("read CA key: %w", err)
	}
	if err := a.engCtx(ctx).PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{
		"dbca-ca.crt": {0o644, 0, caCrt},
		"dbca-ca.key": {0o644, 0, caKey},
	})); err != nil {
		return fmt.Errorf("stage CA: %w", err)
	}
	if ttlValue <= 0 {
		ttlValue, ttlUnit = 365, "days"
	}
	switch ttlUnit {
	case "minutes", "hours", "days":
	default:
		ttlUnit = "days"
	}
	env := []string{"FQDN=" + fqdn, "VALUE=" + strconv.Itoa(ttlValue), "UNIT=" + ttlUnit}
	if err := a.runStep(ctx, containerID, sambaCertScript, env, logln); err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}
	logln("TLS cert for " + fqdn + " written to Samba (Intranet CA)")
	return nil
}

// ------------------------------------------------------------------ scripts

const sambaInstallScript = `set -e
export DEBIAN_FRONTEND=noninteractive
echo "krb5-config krb5-config/default_realm string $REALM" | debconf-set-selections
echo "samba-common samba-common/dhcp boolean false" | debconf-set-selections
apt-get update -qq
# NB: no --no-install-recommends — the AD provisioning templates (samba-ad-provision)
# arrive as a recommended package and samba-tool domain provision needs them.
apt-get install -y -qq \
  samba smbclient krb5-user winbind libnss-winbind libpam-winbind ldb-tools ldap-utils dnsutils openssl >/dev/null
systemctl disable --now smbd nmbd winbind >/dev/null 2>&1 || true`

// sambaProvisionScript provisions the domain (idempotent — skipped if already provisioned),
// enables plain-LDAP binds, and writes an explicit krb5.conf that DB nodes can use directly.
const sambaProvisionScript = `set -e
if [ ! -f /var/lib/samba/private/sam.ldb ]; then
  rm -f /etc/samba/smb.conf
  if [ -n "$INTRANET" ]; then
    samba-tool domain provision --use-rfc2307 --realm="$REALM" --domain="$WORKGROUP" --server-role=dc --dns-backend=SAMBA_INTERNAL --adminpass="$ADMINPASS" --option="dns forwarder = $INTRANET" >/tmp/prov.log 2>&1 || { echo "provision failed:"; tail -15 /tmp/prov.log; exit 1; }
  else
    samba-tool domain provision --use-rfc2307 --realm="$REALM" --domain="$WORKGROUP" --server-role=dc --dns-backend=SAMBA_INTERNAL --adminpass="$ADMINPASS" >/tmp/prov.log 2>&1 || { echo "provision failed:"; tail -15 /tmp/prov.log; exit 1; }
  fi
fi
# Allow unencrypted LDAP binds (plain ldap://).
grep -q "strong auth" /etc/samba/smb.conf || sed -i "/^\[global\]/a ldap server require strong auth = no" /etc/samba/smb.conf
# Explicit krb5.conf (KDC + admin_server pinned to this DC) — download-ready for DB nodes.
cat > /etc/krb5.conf <<EOF
[libdefaults]
	default_realm = $REALM
	dns_lookup_realm = false
	dns_lookup_kdc = true
	rdns = false

[realms]
	$REALM = {
		kdc = $FQDN
		admin_server = $FQDN
		default_domain = $DOMAIN
	}

[domain_realm]
	.$DOMAIN = $REALM
	$DOMAIN = $REALM
EOF`

// sambaCertScript signs an Intranet-CA cert for $FQDN into Samba's default TLS dir. Samba
// auto-serves LDAPS from these paths, so no smb.conf change is needed.
const sambaCertScript = `set -e
case "$UNIT" in
  minutes) SECS=$((VALUE*60));;
  hours)   SECS=$((VALUE*3600));;
  *)       SECS=$((VALUE*86400));;
esac
DAYS=$((SECS/86400)); [ "$DAYS" -lt 1 ] && DAYS=1
CA=/tmp/dbca-ca.crt; CAKEY=/tmp/dbca-ca.key
[ -f "$CA" ] && [ -f "$CAKEY" ] || { echo "CA material missing"; exit 1; }
TLS=/var/lib/samba/private/tls
mkdir -p "$TLS"
cp -f "$CA" "$TLS/ca.pem"
printf 'subjectAltName = DNS:%s\n' "$FQDN" > /tmp/san.cnf
openssl req -newkey rsa:2048 -nodes -keyout "$TLS/key.pem" -out /tmp/s.csr -subj "/O=DBCanvas/CN=$FQDN" >/dev/null 2>&1 || { echo "openssl req failed"; exit 1; }
openssl x509 -req -in /tmp/s.csr -CA "$CA" -CAkey "$CAKEY" -CAcreateserial -out "$TLS/cert.pem" -days "$DAYS" -extfile /tmp/san.cnf >/dev/null 2>&1 || { echo "openssl sign failed"; exit 1; }
chmod 600 "$TLS/key.pem"; chmod 644 "$TLS/cert.pem" "$TLS/ca.pem"
rm -f /tmp/dbca-ca.crt /tmp/dbca-ca.key /tmp/s.csr /tmp/dbca-ca.srl /tmp/san.cnf
echo "TLS material written to $TLS (cert/key/ca.pem)"`

// sambaStartScript enables + starts the AD DC daemon and waits for LDAP to answer.
const sambaStartScript = `set -e
systemctl unmask samba-ad-dc >/dev/null 2>&1 || true
systemctl disable --now smbd nmbd winbind >/dev/null 2>&1 || true
systemctl enable --now samba-ad-dc >/tmp/svc.log 2>&1
OK=0
for i in $(seq 1 30); do
  if ldapsearch -x -H ldap://localhost -b "" -s base defaultNamingContext >/dev/null 2>&1; then OK=1; break; fi
  sleep 1
done
[ "$OK" = 1 ] || { echo "samba-ad-dc not answering LDAP:"; journalctl -u samba-ad-dc --no-pager 2>/dev/null | tail -15; exit 1; }`

// sambaAccountsScript creates only the ldapbind service account (for DB simple binds).
// No sample user/group — the directory starts clean apart from AD's own built-ins.
// Idempotent.
const sambaAccountsScript = `set -e
if ! samba-tool user list | grep -qx ldapbind; then
  samba-tool user create ldapbind "$BINDPW" --description="LDAP bind account for database authentication" >/dev/null
  samba-tool user setexpiry ldapbind --noexpiry >/dev/null 2>&1 || true
fi
echo "directory ready (ldapbind service account)"`
