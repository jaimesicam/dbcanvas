package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Directory authentication for standalone database nodes (ps / pg / psm). When a node
// has LdapAuth set, applyDirectoryAuth configures the engine at deploy time to authenticate
// against the chosen directory node (Intranet OpenLDAP or Samba AD DC), and — for pg/psm with
// KerberosAuth — wires GSSAPI single sign-on using a keytab minted on the Samba DC.
//
// Recipes were validated live (see IMPLEMENTATION §103):
//   - PostgreSQL: pg_hba `ldap` (search+bind) + `hostgssenc … gss`; both coexist because a
//     Kerberos client uses gssencmode=require (matches hostgssenc) and a password client falls
//     through to the ldap line. The `postgres` superuser keeps scram. Kerberos needs a
//     postgres/<fqdn> keytab + krb_server_keyfile.
//   - Percona Server: authentication_ldap_simple. Its variables are startup-only and the plugin
//     must be loaded from /etc/my.cnf (the my.cnf.d dir is NOT included), so config is appended
//     there and mysqld restarted. LDAP only (no Kerberos).
//   - PSMDB: security.ldap (simple bind + userToDNMapping) with authenticationMechanisms PLAIN;
//     Kerberos adds cyrus-sasl-gssapi, a mongodb/<fqdn> keytab (KRB5_KTNAME), GSSAPI mechanism
//     and saslHostName=<fqdn> (mongod otherwise builds a short-hostname acceptor principal).

// dirAuthIssues validates a DB node's directory-auth selection against the stack's directory
// nodes (id → "intranet"|"sambaad"). LDAP (a chosen directory) and Kerberos (a Samba AD DC
// node existing in the stack) are independent options.
func dirAuthIssues(n designNode, dirNodes map[string]string) []issue {
	var out []issue
	if n.LdapAuth {
		if _, ok := dirNodes[n.LdapDirNodeID]; !ok {
			out = append(out, issue{"error", nodeKindLabel(n.Type) + " node " + n.Label + " has LDAP integration enabled but no directory is selected — add an Intranet or Samba AD DC node and pick it"})
		}
	}
	if n.KerberosAuth {
		hasSamba := false
		for _, t := range dirNodes {
			if t == "sambaad" {
				hasSamba = true
				break
			}
		}
		if !hasSamba {
			out = append(out, issue{"error", nodeKindLabel(n.Type) + " node " + n.Label + " has Kerberos enabled, which requires a Samba AD DC node — add a Samba AD DC node or turn off Kerberos"})
		}
	}
	return out
}

// sambaNodeID returns the (singleton) Samba AD DC node's id, or "" if the stack has none.
func sambaNodeID(doc designDoc) string {
	for _, n := range doc.Nodes {
		if n.Type == "sambaad" {
			return n.ID
		}
	}
	return ""
}

func nodeKindLabel(t string) string {
	switch t {
	case "ps":
		return "Percona Server"
	case "pg":
		return "PostgreSQL"
	case "psm":
		return "PSMDB"
	case "pmm":
		return "PMM"
	}
	return t
}

// dirInfo describes the resolved directory a DB node authenticates against.
type dirInfo struct {
	Type        string // "intranet" | "sambaad"
	ContainerID string
	FQDN        string // directory host to reach over LDAP
	Domain      string
	Realm       string // Kerberos realm (sambaad only)
	BaseDN      string // user search base
	BindDN      string // service-bind DN
	BindPW      string
	UserAttr    string // "uid" (OpenLDAP) | "sAMAccountName" (AD)
}

// dirAuthInfo is persisted into the DB node's Deployment.Config (key "dirAuth") so the
// manager's Directory-Login tab can render exact login commands. LDAP and Kerberos are
// independent.
type dirAuthInfo struct {
	Enabled  bool   `json:"enabled"` // ldap || kerberos (controls tab visibility)
	Ldap     bool   `json:"ldap"`
	Kerberos bool   `json:"kerberos"`
	DirType  string `json:"dirType"`
	DirFQDN  string `json:"dirFQDN"`
	NodeFQDN string `json:"nodeFQDN"`
	Realm    string `json:"realm"`
	UserAttr string `json:"userAttr"`
}

// resolveDirectory waits for the chosen directory node to be running and derives the
// connection parameters for LDAP binds (and Kerberos realm for AD).
func (a *App) resolveDirectory(ctx context.Context, st Stack, doc designDoc, dirNodeID string) (dirInfo, error) {
	var dirType string
	for _, n := range doc.Nodes {
		if n.ID == dirNodeID && (n.Type == "intranet" || n.Type == "sambaad") {
			dirType = n.Type
			break
		}
	}
	if dirType == "" {
		return dirInfo{}, fmt.Errorf("directory node not found in stack")
	}
	// Wait for the directory to finish provisioning (it deploys in parallel).
	deadline := time.Now().Add(5 * time.Minute)
	var dep Deployment
	for {
		d, err := a.store.GetDeployment(st.ID, dirNodeID)
		if err == nil {
			if d.State == DeployError {
				return dirInfo{}, fmt.Errorf("directory node failed to provision")
			}
			if d.State == DeployRunning && d.ContainerID != "" {
				dep = d
				break
			}
		}
		if time.Now().After(deadline) {
			return dirInfo{}, fmt.Errorf("directory node did not become ready within 5m")
		}
		time.Sleep(3 * time.Second)
	}

	info := dirInfo{Type: dirType, ContainerID: dep.ContainerID}
	if dirType == "sambaad" {
		var cfg sambaConfig
		var sec sambaSecrets
		json.Unmarshal(dep.Config, &cfg)
		json.Unmarshal(dep.Secrets, &sec)
		info.FQDN, info.Domain, info.Realm = cfg.FQDN, cfg.Domain, cfg.Realm
		info.BaseDN, info.BindDN, info.BindPW = cfg.BaseDN, cfg.BindDN, sec.BindPassword
		info.UserAttr = "sAMAccountName"
	} else {
		var sec nodeSecrets
		json.Unmarshal(dep.Secrets, &sec)
		info.Domain = sec.Domain
		info.FQDN = fqdnOf("intranet", sec.Domain)
		info.BaseDN = "ou=People," + sec.BaseDN
		info.BindDN, info.BindPW = sec.LDAPAdminDN, sec.LDAPAdminPassword
		info.UserAttr = "uid"
	}
	return info, nil
}

// applyDirectoryAuth configures engine ("pg"|"ps"|"psm") on containerID to authenticate against
// the node's chosen directory, wiring Kerberos (pg/psm) when requested, and records the result
// in the node's Deployment.Config. rootPW is the engine's admin/root password (used to verify a
// restart came back for MySQL).
func (a *App) applyDirectoryAuth(ctx context.Context, st Stack, n designNode, doc designDoc, containerID, engine, rootPW string, pr *pxcProg) error {
	pr.phase("Configuring directory authentication", 96)
	domain := envOr("DOMAIN", "example.net")

	// LDAP directory (optional): the chosen Intranet or Samba node.
	var ldap dirInfo
	if n.LdapAuth {
		d, err := a.resolveDirectory(ctx, st, doc, n.LdapDirNodeID)
		if err != nil {
			return fmt.Errorf("directory: %w", err)
		}
		ldap = d
		domain = d.Domain
	}

	// Kerberos (optional, independent of LDAP; pg/psm only): always via the stack's Samba DC.
	kerberos := n.KerberosAuth && engine != "ps"
	var samba dirInfo
	if kerberos {
		sid := sambaNodeID(doc)
		if sid == "" {
			return fmt.Errorf("Kerberos requires a Samba AD DC node in the stack")
		}
		s, err := a.resolveDirectory(ctx, st, doc, sid)
		if err != nil {
			return fmt.Errorf("kerberos directory: %w", err)
		}
		samba = s
		if !n.LdapAuth {
			domain = s.Domain
		}
	}
	nodeFQDN := fqdnOf(stackHostnames(doc)[n.ID], domain)

	// For Kerberos, mint a service principal + keytab on the Samba DC and stage it (+ krb5.conf)
	// into the DB container.
	keytabPath := ""
	if kerberos {
		svc := "postgres"
		keytabPath = "/var/lib/pgsql/dbcanvas.keytab"
		if engine == "psm" {
			svc, keytabPath = "mongodb", "/etc/mongod.keytab"
		}
		keytab, krb5, err := a.mintKeytab(ctx, samba.ContainerID, svc, nodeFQDN)
		if err != nil {
			return fmt.Errorf("kerberos keytab: %w", err)
		}
		if err := a.docker.PutArchive(ctx, containerID, "/etc", tarFiles(map[string]fileEntry{"dbcanvas.krb5.conf": {0o644, 0, krb5}})); err != nil {
			return fmt.Errorf("stage krb5.conf: %w", err)
		}
		if err := a.docker.PutArchive(ctx, containerID, "/tmp", tarFiles(map[string]fileEntry{"dbcanvas.keytab": {0o600, 0, keytab}})); err != nil {
			return fmt.Errorf("stage keytab: %w", err)
		}
		pr.logln("minted " + svc + "/" + nodeFQDN + " keytab on the Samba DC")
	}

	env := []string{
		"LDAP=" + boolEnv(n.LdapAuth),
		"DIRFQDN=" + ldap.FQDN, "BASE=" + ldap.BaseDN, "ATTR=" + ldap.UserAttr,
		"BINDDN=" + ldap.BindDN, "BINDPW=" + ldap.BindPW,
		"NODEFQDN=" + nodeFQDN, "REALM=" + samba.Realm, "ROOTPW=" + rootPW,
		"KEYTAB=" + keytabPath, "KRB=" + boolEnv(kerberos),
	}
	var script string
	switch engine {
	case "pg":
		script = pgDirAuthScript
	case "ps":
		script = mysqlDirAuthScript
	case "psm":
		script = mongoDirAuthScript
	}
	if err := a.runStep(ctx, containerID, script, env, pr.logln); err != nil {
		return err
	}

	info := dirAuthInfo{Enabled: n.LdapAuth || kerberos, Ldap: n.LdapAuth, Kerberos: kerberos,
		DirType: ldap.Type, DirFQDN: ldap.FQDN, NodeFQDN: nodeFQDN, Realm: samba.Realm, UserAttr: ldap.UserAttr}
	a.persistDirAuth(st, n.ID, info)
	var msg string
	if n.LdapAuth {
		msg = "LDAP against " + ldap.FQDN
	}
	if kerberos {
		if msg != "" {
			msg += " + "
		}
		msg += "Kerberos SSO (realm " + samba.Realm + ")"
	}
	pr.logln(msg + " configured")
	return nil
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// mintKeytab creates service/<fqdn> on the Samba DC (idempotent) and returns its keytab plus the
// DC's krb5.conf.
func (a *App) mintKeytab(ctx context.Context, sambaID, service, fqdn string) (keytab, krb5 []byte, err error) {
	if _, err = a.execScript(ctx, sambaID, sambaPrincipalCreateScript, []string{"SERVICE=" + service, "FQDN=" + fqdn, "PW=" + genSecret("Svc1!")}); err != nil {
		return nil, nil, fmt.Errorf("create principal: %w", err)
	}
	if _, err = a.execScript(ctx, sambaID, sambaKeytabScript, []string{"PRINC=" + service + "/" + fqdn}); err != nil {
		return nil, nil, fmt.Errorf("export keytab: %w", err)
	}
	if keytab, err = a.readContainerFile(ctx, sambaID, "/tmp/svc.keytab"); err != nil {
		return nil, nil, fmt.Errorf("read keytab: %w", err)
	}
	if krb5, err = a.readContainerFile(ctx, sambaID, "/etc/krb5.conf"); err != nil {
		return nil, nil, fmt.Errorf("read krb5.conf: %w", err)
	}
	return keytab, krb5, nil
}

// persistDirAuth merges the dirAuth summary into the DB node's Deployment.Config (leaving the
// engine's own config keys intact) so the manager can render login instructions.
func (a *App) persistDirAuth(st Stack, nodeID string, info dirAuthInfo) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil {
		return
	}
	m := map[string]any{}
	if len(dep.Config) > 0 {
		json.Unmarshal(dep.Config, &m)
	}
	m["dirAuth"] = info
	if b, err := json.Marshal(m); err == nil {
		dep.Config = b
		a.store.UpsertDeployment(dep)
	}
}

// ---------------------------------------------------------------- engine scripts

// pgDirAuthScript wires pg_hba `ldap` (search+bind) and, when KRB=1, `hostgssenc … gss`
// (krb_server_keyfile + staged krb5.conf). The postgres superuser keeps scram.
const pgDirAuthScript = `set -e
DATA=$(dirname "$(find /var/lib/pgsql /var/lib/postgresql -name pg_hba.conf 2>/dev/null | head -1)")
[ -n "$DATA" ] || { echo "pg_hba.conf not found"; exit 1; }
HBA="$DATA/pg_hba.conf"; CONF="$DATA/postgresql.conf"
sed -i "/# dbcanvas-dirauth/d" "$HBA" "$CONF"
LDAPLINE=""; SU=""; GSS=""
[ "$LDAP" = "1" ] && LDAPLINE="host all all 0.0.0.0/0 ldap ldapserver=$DIRFQDN ldapbasedn=\"$BASE\" ldapbinddn=\"$BINDDN\" ldapbindpasswd=\"$BINDPW\" ldapsearchattribute=$ATTR # dbcanvas-dirauth"
{ [ "$LDAP" = "1" ] || [ "$KRB" = "1" ]; } && SU="host all postgres 0.0.0.0/0 scram-sha-256 # dbcanvas-dirauth"
if [ "$KRB" = "1" ]; then
  ` + krb5ClientInstall + `
  mv -f /tmp/dbcanvas.keytab "$KEYTAB"; chown postgres "$KEYTAB" 2>/dev/null || true; chmod 600 "$KEYTAB"
  mv -f /etc/dbcanvas.krb5.conf /etc/krb5.conf
  echo "krb_server_keyfile = '$KEYTAB' # dbcanvas-dirauth" >> "$CONF"
  GSS="hostgssenc all all 0.0.0.0/0 gss include_realm=0 # dbcanvas-dirauth"
fi
SU="$SU" GSS="$GSS" LDAPLINE="$LDAPLINE" python3 - "$HBA" <<'PY'
import os,re,sys
hba=sys.argv[1]
add=[l for l in (os.environ["SU"],os.environ["GSS"],os.environ["LDAPLINE"]) if l]
lines=open(hba).read().splitlines(); out=[]; done=False
for ln in lines:
    if not done and re.match(r"host\s+all\s+all\s+0\.0\.0\.0/0\s+scram-sha-256\s*$", ln):
        out+=add; done=True
    out.append(ln)
if not done: out=add+out
open(hba,"w").write("\n".join(out)+"\n")
PY
su - postgres -c "psql -tAc 'SELECT pg_reload_conf()'" >/dev/null
echo "pg_hba.conf updated + reloaded"`

// krb5ClientInstall installs the Kerberos client tools (kinit/klist): krb5-user on Debian/
// Ubuntu, krb5-workstation on Oracle Linux / RHEL.
const krb5ClientInstall = `if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq krb5-user >/dev/null 2>&1 || true
  else
    (microdnf install -y krb5-workstation >/dev/null 2>&1 || dnf install -y krb5-workstation >/dev/null 2>&1) || true
  fi`

// mysqlDirAuthScript loads authentication_ldap_simple via /etc/my.cnf (the .d dir is not
// included) and restarts mysqld.
const mysqlDirAuthScript = `set -e
CNF=/etc/my.cnf
sed -i "/# dbcanvas-dirauth-begin/,/# dbcanvas-dirauth-end/d" "$CNF"
cat >> "$CNF" <<EOF

# dbcanvas-dirauth-begin
[mysqld]
plugin-load-add=authentication_ldap_simple.so
authentication_ldap_simple_server_host=$DIRFQDN
authentication_ldap_simple_bind_base_dn=$BASE
authentication_ldap_simple_user_search_attr=$ATTR
authentication_ldap_simple_bind_root_dn=$BINDDN
authentication_ldap_simple_bind_root_pwd=$BINDPW
# dbcanvas-dirauth-end
EOF
systemctl restart mysqld
for i in $(seq 1 30); do mysql -uroot -p"$ROOTPW" -e "SELECT 1" >/dev/null 2>&1 && break; sleep 2; done
mysql -uroot -p"$ROOTPW" -e "SELECT @@authentication_ldap_simple_server_host" 2>/dev/null | tail -1
echo "authentication_ldap_simple loaded"`

// mongoDirAuthScript merges a security.ldap block + PLAIN mechanism into mongod.conf, and when
// KRB=1 installs cyrus-sasl-gssapi, wires the keytab (KRB5_KTNAME) + saslHostName + GSSAPI.
const mongoDirAuthScript = `set -e
KRB="$KRB" LDAP="$LDAP" DIRFQDN="$DIRFQDN" BASE="$BASE" ATTR="$ATTR" BINDDN="$BINDDN" BINDPW="$BINDPW" NODEFQDN="$NODEFQDN" python3 - <<'PY'
import os,re
conf="/etc/mongod.conf"; t=open(conf).read()
t=re.sub(r"\n# dbcanvas-dirauth.*?# dbcanvas-dirauth-end\n","\n",t,flags=re.S)
q=os.environ
if q["LDAP"]=="1":
    ldap=(
    'security:\n  authorization: enabled\n  ldap:\n'
    f'    servers: "{q["DIRFQDN"]}"\n'
    '    transportSecurity: none\n'
    '    bind:\n'
    f'      queryUser: "{q["BINDDN"]}"\n'
    f'      queryPassword: "{q["BINDPW"]}"\n'
    '    userToDNMapping: ' + "'" + f'[{{match: "(.+)", ldapQuery: "{q["BASE"]}??sub?({q["ATTR"]}={{0}})"}}]' + "'"
    )
    if "security:\n  authorization: enabled" in t:
        t=t.replace("security:\n  authorization: enabled", ldap, 1)
    else:
        t=t.rstrip()+"\n"+ldap+"\n"
mechs="SCRAM-SHA-256,SCRAM-SHA-1"
if q["LDAP"]=="1": mechs+=",PLAIN"
extra=""
if q["KRB"]=="1":
    mechs+=",GSSAPI"; extra=f'\n  saslHostName: {q["NODEFQDN"]}'
t=t.rstrip()+"\n# dbcanvas-dirauth\nsetParameter:\n  authenticationMechanisms: "+mechs+extra+"\n# dbcanvas-dirauth-end\n"
open(conf,"w").write(t)
PY
if [ "$KRB" = "1" ]; then
  (microdnf install -y cyrus-sasl-gssapi krb5-libs >/dev/null 2>&1 || dnf install -y cyrus-sasl-gssapi krb5-libs >/dev/null 2>&1) || true
  ` + krb5ClientInstall + `
  mv -f /tmp/dbcanvas.keytab "$KEYTAB"; chown mongod "$KEYTAB" 2>/dev/null || true; chmod 600 "$KEYTAB"
  mv -f /etc/dbcanvas.krb5.conf /etc/krb5.conf
  mkdir -p /etc/systemd/system/mongod.service.d
  printf "[Service]\nEnvironment=KRB5_KTNAME=%s\n" "$KEYTAB" > /etc/systemd/system/mongod.service.d/dbcanvas-krb5.conf
  systemctl daemon-reload
fi
systemctl restart mongod
for i in $(seq 1 30); do systemctl is-active --quiet mongod && break; sleep 2; done
echo "mongod directory auth configured"`
